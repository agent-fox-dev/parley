package change

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// AuthorizeModify judges whether the caller of r may modify the change ch:
// the admin token, a caller who writes the change's workspace, and the
// change's author, whatever credential of theirs they use, so that an org
// member edits the change they opened into a workspace they only read. It
// answers nil, or 403 forbidden for anyone else who reads the workspace.
// The shared checks of the route have answered for the scope and for read
// access, and changeOf for the token's binding; r must carry the access
// decision of a route that declares a need.
func AuthorizeModify(r *http.Request, ch Change) error {
	p, _ := identity.PrincipalOf(r.Context())
	if p.Type == identity.TypeAdmin {
		return nil
	}
	if d, ok := access.DecisionOf(r); ok && (d.Level == access.LevelOwner || d.Level == access.LevelAdmin) {
		return nil
	}
	if p.User != nil && ch.Author.ActorID != nil && *ch.Author.ActorID == p.User.ID {
		return nil
	}
	return server.Forbidden(fmt.Sprintf("%s may be changed only by its author, a writer of %s or the admin token", ch.Ref, ch.WorkspaceSlug))
}

// workspaceUsable is the refusal of a write to a change of the workspace
// slug, read through q, while it cannot take one: 409 workspace_busy while
// it is pending, cloning or under maintenance, workspace_not_ready when its
// clone failed and workspace_archived when it is archived; nil when it is
// ready.
func (c *Component) workspaceUsable(ctx context.Context, q Querier, slug string) error {
	w, found, err := c.deps.Workspaces.Workspace(ctx, q, slug)
	if err != nil {
		return err
	}
	if !found {
		return server.NotFound(fmt.Sprintf("no workspace %q", slug))
	}
	return w.Unusable
}

// operation is what a write route does to its change once every check has
// passed, in the route's transaction, as caller; it applies the route's own
// state rules first.
type operation func(ctx context.Context, tx *sql.Tx, ch Change, caller Caller) error

// modify answers a write route on the change the request names, in one
// store transaction and in this order: changeOf resolves the change (404
// change_not_found), AuthorizeModify applies the caller rule (403
// forbidden), parse decodes and validates the body into the route's
// operation (400), workspaceUsable checks the target workspace's status
// (409) and the operation runs. It answers 200 with the change as it then
// is, as GET returns it.
func (c *Component) modify(w http.ResponseWriter, r *http.Request, parse func(*http.Request) (operation, error)) error {
	ctx := r.Context()
	p, _ := identity.PrincipalOf(ctx)
	caller := CallerOf(p)
	var out Change
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		ch, err := c.changeOf(r, tx)
		if err != nil {
			return err
		}
		if err := AuthorizeModify(r, ch); err != nil {
			return err
		}
		op, err := parse(r)
		if err != nil {
			return err
		}
		if err := c.workspaceUsable(ctx, tx, ch.WorkspaceSlug); err != nil {
			return err
		}
		if err := op(ctx, tx, ch, caller); err != nil {
			return err
		}
		out, err = readChange(ctx, tx, ch.WorkspaceSlug, ch.Number)
		return err
	})
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, out)
	return nil
}
