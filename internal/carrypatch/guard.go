package carrypatch

import (
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// Guard is the check every carry-patch route makes after its own request
// validation, so that an invalid request is answered as such on any
// workspace. It loads the record of the workspace the access check granted
// the request, and refuses one that is not in mode carry_patch with 400
// workspace_mode_mismatch, before its state is looked at; a task fork is
// always in mode standard, so every fork is refused. On a write route it
// then applies workspace.RequireReady, whose 409 refuses a workspace that
// is not ready or on which a maintenance operation runs; a read route
// serves from stored state in every state. It returns the record for the
// handler to use.
//
// The route must have been declared with Route, which tells the guard its
// kind; a route that was not, or that declares no access need, is a wiring
// fault, answered 500.
func (c *Component) Guard(r *http.Request) (workspace.Workspace, error) {
	kind, ok := server.Attached(r, routeKindKey{})
	if !ok {
		return workspace.Workspace{}, server.Internal("a carry-patch route was not declared with carrypatch.Route")
	}
	d, ok := access.DecisionOf(r)
	if !ok {
		return workspace.Workspace{}, server.Internal("the carry-patch guard found no access decision")
	}
	if c.d.Store == nil {
		return workspace.Workspace{}, errNotBound
	}
	w, found, err := workspace.BySlug(r.Context(), c.d.Store.Main(), d.Record.Slug)
	if err != nil {
		return workspace.Workspace{}, fmt.Errorf("read the workspace %q: %w", d.Record.Slug, err)
	}
	if !found {
		// Deleted since the access check: the answer for a slug no
		// workspace has.
		return workspace.Workspace{}, server.NotFound("workspace not found")
	}
	if w.Mode != workspace.ModeCarryPatch {
		return workspace.Workspace{}, server.NewError("workspace_mode_mismatch", http.StatusBadRequest,
			fmt.Sprintf("workspace %q is in mode %s; patches need a workspace in mode %s", w.Slug, w.Mode, workspace.ModeCarryPatch))
	}
	if kind == WriteRoute {
		if err := workspace.RequireReady(w); err != nil {
			return workspace.Workspace{}, err
		}
	}
	return w, nil
}
