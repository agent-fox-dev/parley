package scm

import (
	"errors"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// scopeWrite is the scope a personal access token needs for every write.
const scopeWrite = "git:write"

// The errors of a request that reached a write route without what the
// server's shared checks attach to it, which only a route registered
// outside writeRoute can cause; the pipeline answers them 500 internal.
var (
	errNoPrincipal = errors.New("scm: a write route was reached without an authenticated caller")
	errNoDecision  = errors.New("scm: a write route was reached without an access decision")
)

// write is one request to a write route that the write gate let through:
// who sent it, what the access check granted and the workspace it writes,
// each read once, so that no write handler looks any of them up again.
type write struct {
	// principal is the caller and its credential: the admin token, an API
	// key or a PAT with its scopes, paths and binding.
	principal identity.Principal
	// decision is the access check's grant: the caller's level on the
	// workspace, owner or admin, and the access rule's record of it.
	decision access.Decision
	// binding is the binding of a PAT bound to a workspace, when bound
	// is true.
	binding access.Binding
	bound   bool
	// ws is the workspace's record, read after the access check and
	// found writable.
	ws workspace.Workspace
}

// writeHandler answers a request to a write route, which the write gate
// has let through as wr.
type writeHandler func(w http.ResponseWriter, r *http.Request, wr *write) error

// writeRoute is the write route method pattern below /workspaces/:slug/scm,
// whose requests h answers and are reported under op. It is the one way a
// write route is built, so that every write passes the same gate in the
// conventions' order: the shared checks answer 401, then 403
// insufficient_scope to a PAT without git:write (the same whether or not
// the workspace exists), then 404 not_found to a caller with no access and
// 403 forbidden to one with read access only, before the body is looked
// at; then the gate refuses a workspace that cannot take a write (admit),
// before h runs.
func (c *Component) writeRoute(method, pattern, op string, h writeHandler) server.Route {
	return server.Route{Method: method, Pattern: "/workspaces/:slug/scm" + pattern, Scope: scopeWrite,
		Marks: map[string]string{access.MarkNeed: string(access.NeedWrite)}, Handler: c.gate(op, h)}
}

// gate is h behind the state part of the write gate, reporting each
// request h answers to the recorder once, under op, with the status it
// wrote or, when it returned an error before writing, the status the error
// is answered with. A request the gate refuses reaches no handler and is
// not reported, as one the shared checks refuse is not.
func (c *Component) gate(op string, h writeHandler) server.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		wr, err := c.admit(r)
		if err != nil {
			return err
		}
		sw := &statusWriter{ResponseWriter: w, ctx: r.Context()}
		err = h(sw, r, wr)
		status := sw.status
		if status == 0 {
			status = errorStatus(err)
		}
		result := ResultOK
		if status >= http.StatusBadRequest {
			result = ResultError
		}
		c.deps.Recorder.Observe(op, result)
		return err
	}
}

// admit is the write of the request r, once the shared checks have granted
// the caller write access, or the refusal of a workspace that cannot take a
// write: its own state first (workspace.RequireReady: 409
// workspace_archived, workspace_busy while it is pending, cloning or a
// reclone, archive or delete runs on it, workspace_not_ready once its clone
// failed), then, for a fork, its parent's (WriteParentState: 409
// parent_archived, workspace_busy or workspace_not_ready). Nothing is read
// from the repository before it answers.
func (c *Component) admit(r *http.Request) (*write, error) {
	ctx, slug := r.Context(), server.Param(r, "slug")
	principal, ok := identity.PrincipalOf(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	decision, ok := access.DecisionOf(r)
	if !ok {
		return nil, errNoDecision
	}
	ws, found, err := c.deps.Workspaces(ctx, slug)
	if err != nil {
		return nil, err
	}
	if !found {
		// Deleted since the access check: the answer that check gives for a
		// slug no workspace has.
		return nil, server.NotFound(workspace.ErrNotFound.Error())
	}
	if err := workspace.RequireReady(ws); err != nil {
		return nil, err
	}
	if ws.Kind == access.KindFork && c.deps.WriteParentState != nil {
		if err := c.deps.WriteParentState(ctx, slug); err != nil {
			return nil, err
		}
	}
	wr := &write{principal: principal, decision: decision, ws: ws}
	wr.binding, wr.bound = access.BindingOf(r)
	return wr, nil
}
