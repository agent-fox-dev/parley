package forge

import (
	"context"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// ErrorTypeCredentialMissing is the refusal of a write to a target for
// which the workspace stores no credential.
const ErrorTypeCredentialMissing = "forge_credential_missing"

// writeRoute is a forge write route, which writeRoute builds into a server
// route on the shared checks of Route, with the scope forge:write and the
// need write. Its checks run in this order, each refusal sending nothing to
// the forge:
//
//  1. the PAT scope and write access, the fork's parent's included, and the
//     query parameter target, as for every forge route;
//  2. before, the route's path parameters and the body rules that do not
//     depend on the forge;
//  3. the requested workspace is archived (409 workspace_archived), or the
//     task fork's parent is (409 parent_archived);
//  4. the resolution of the target's forge (forge_api_url_invalid,
//     forge_unknown);
//  5. after, the route's rules that depend on the forge's kind;
//  6. the target has no stored credential (422 forge_credential_missing):
//     the hub never writes to a forge anonymously.
//
// Then serve makes the write.
type writeRoute struct {
	method, pattern string
	// before checks the path parameters and the body, and keeps what it
	// read in call.Params; nil for none.
	before func(r *http.Request, call *Call) error
	// after refuses a request by a rule of the target's forge kind; nil
	// for none.
	after func(r *http.Request, call *Call) error
	// serve makes the write on the forge and returns the answer. Its
	// context is not ended by the caller leaving, so that a write is
	// carried out to its end.
	serve func(ctx context.Context, call *Call) (written, error)
}

// written is a write route's answer: its status, and its body, written in
// JSON, or none when it is nil. The frame writes it, so that what a write
// did can be recorded before the caller is answered.
type written struct {
	status int
	body   any
}

// writeRoutes are the forge write routes.
func (c *Component) writeRoutes() []server.Route {
	return []server.Route{
		c.writeRoute(writeRoute{method: http.MethodPost, pattern: "/workspaces/:slug/issues",
			before: readIssueCreate, serve: createIssue}),
	}
}

// writeRoute builds the server route of wr (see writeRoute).
func (c *Component) writeRoute(wr writeRoute) server.Route {
	return c.Route(Route{
		Method: wr.method, Pattern: wr.pattern, Scope: ScopeWrite, Need: access.NeedWrite,
		Params: func(r *http.Request, call *Call) error {
			if wr.before != nil {
				if err := wr.before(r, call); err != nil {
					return err
				}
			}
			return archived(call)
		},
		Check: func(r *http.Request, call *Call) error {
			if wr.after != nil {
				if err := wr.after(r, call); err != nil {
					return err
				}
			}
			if call.Resolution.Credential.Kind == CredentialNone {
				return server.NewError(ErrorTypeCredentialMissing, http.StatusUnprocessableEntity,
					fmt.Sprintf("workspace %q stores no forge credential for the target %s; a write needs one",
						call.Workspace.Slug, call.Resolution.Target))
			}
			return nil
		},
		Serve: func(w http.ResponseWriter, r *http.Request, call *Call) error {
			out, err := wr.serve(context.WithoutCancel(r.Context()), call)
			if err != nil {
				return err
			}
			if out.body == nil {
				w.WriteHeader(out.status)
				return nil
			}
			server.WriteJSON(w, out.status, out.body)
			return nil
		},
	})
}

// archived refuses a write to an archived workspace, or through a task
// fork to its archived parent.
func archived(call *Call) error {
	switch {
	case call.Requested.Status == workspace.StatusArchived:
		return server.WorkspaceArchived(fmt.Sprintf("workspace %q is archived", call.Requested.Slug))
	case call.Workspace.Slug != call.Requested.Slug && call.Workspace.Status == workspace.StatusArchived:
		return server.NewError("parent_archived", http.StatusConflict,
			fmt.Sprintf("workspace %q is read-only: its parent workspace %q is archived", call.Requested.Slug, call.Workspace.Slug))
	}
	return nil
}

// send sends method <the API URL><path> to the target's forge with the
// target's credential, and body in JSON unless it is nil.
func (c *Call) send(ctx context.Context, method, path string, body any) (*Response, error) {
	return c.Client.Do(ctx, Request{Method: method, URL: c.Resolution.APIURL + path,
		Kind: c.Resolution.Kind, Credential: c.Resolution.Credential, Body: body})
}

// sendJSON sends a write as send does and reads the forge's answer into v.
func (c *Call) sendJSON(ctx context.Context, method, path string, body, v any) error {
	resp, err := c.send(ctx, method, path, body)
	if err != nil {
		return err
	}
	return Decode(resp, v)
}

// writer is a forge's API for the writes, as api is for the reads.
type writer interface {
	// createIssue creates an issue with the fields of in that are set.
	createIssue(ctx context.Context, call *Call, in issueInput) (Issue, error)
}

func writerOf(kind ForgeKind) writer {
	if kind == KindGitLab {
		return gitlab{}
	}
	return github{}
}
