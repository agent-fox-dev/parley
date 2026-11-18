package forge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// ErrNumber is the refusal of an issue or pull request number that is not
// a positive decimal integer of at most 2,147,483,647.
var ErrNumber = errors.New("number must be a positive decimal integer")

// maxNumber is the largest issue or pull request number: the forges keep
// them in 32 bits.
const maxNumber = 1<<31 - 1

// ParseNumber parses an issue or pull request number: decimal digits only,
// without a sign or a leading zero, from 1 to 2,147,483,647. Anything else
// is ErrNumber.
func ParseNumber(s string) (int64, error) {
	if s == "" || s[0] == '0' || len(s) > len(strconv.Itoa(maxNumber)) {
		return 0, ErrNumber
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, ErrNumber
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n > maxNumber {
		return 0, ErrNumber
	}
	return n, nil
}

// PathNumber parses the route's :number path parameter with ParseNumber;
// a refusal is the 400 invalid_request the route answers.
func PathNumber(r *http.Request) (int64, error) {
	n, err := ParseNumber(server.Param(r, "number"))
	if err != nil {
		return 0, server.InvalidRequest(err.Error())
	}
	return n, nil
}

// Route is a forge route, which Component.Route builds into a server route
// that runs the checks every forge route shares, in a fixed order, before
// its own steps:
//
//  1. the PAT scope Scope and the access Need on the workspace :slug, which
//     the pipeline decides before the handler;
//  2. for a task fork, Need on the fork's parent, by the level of the user
//     behind the credential and without the token's binding: the parent's
//     record, settings and secrets are then the ones used, so 403 forbidden
//     without it;
//  3. the query parameter target: absent or origin, or upstream on a
//     carry-patch workspace with an upstream_url;
//  4. Params, the route's own path and query parameters;
//  5. the resolution of the target's forge: forge_api_url_invalid,
//     forge_unknown, or a credential that cannot be read;
//  6. Check, a route's own refusal that needs the resolution;
//  7. Serve, which calls the forge.
//
// A failed forge call that Check or Serve returns is answered as the hub's
// error for it (MapError), its message scrubbed of the call's token. A
// request refused by a step sends nothing to the forge. No step looks at
// the workspace's status: a read serves a workspace in every state.
type Route struct {
	Method  string
	Pattern string
	// Scope is ScopeRead for a read and ScopeWrite for a write.
	Scope string
	// Need is the access the caller needs on the workspace and on a
	// fork's parent.
	Need access.Need
	// Params checks the route's path and query parameters, and may keep
	// what it parsed in call.Params; nil for none.
	Params func(r *http.Request, call *Call) error
	// Check refuses a request once the forge is resolved, before Serve;
	// nil for none.
	Check func(r *http.Request, call *Call) error
	// Serve calls the forge and answers.
	Serve func(w http.ResponseWriter, r *http.Request, call *Call) error
}

// Call is what the steps of a forge route work on.
type Call struct {
	// Slug is the slug the request named, and Requested its workspace.
	Slug      string
	Requested workspace.Workspace
	// Workspace is the workspace whose remote, settings and secrets are
	// used: the requested one, or a task fork's parent.
	Workspace workspace.Workspace
	Target    Target
	// Settings are Workspace's forge settings.
	Settings Settings
	// Resolution is the target's forge, set before Check and Serve.
	Resolution Resolution
	// Client sends the forge requests.
	Client *Client
	// Params is what the route's Params kept.
	Params any
}

// Get sends GET <the API URL><path> to the target's forge with the
// target's credential.
func (c *Call) Get(ctx context.Context, path string) (*Response, error) {
	return c.Client.Do(ctx, Request{Method: http.MethodGet, URL: c.Resolution.APIURL + path,
		Kind: c.Resolution.Kind, Credential: c.Resolution.Credential})
}

// Route builds the server route of rt, whose handler runs the shared checks
// and then rt's steps (see Route). It may be called before the component is
// bound.
func (c *Component) Route(rt Route) server.Route {
	return server.Route{
		Method: rt.Method, Pattern: rt.Pattern, Scope: rt.Scope,
		Marks:   map[string]string{access.MarkNeed: string(rt.Need)},
		Handler: func(w http.ResponseWriter, r *http.Request) error { return c.serve(w, r, rt) },
	}
}

func (c *Component) serve(w http.ResponseWriter, r *http.Request, rt Route) error {
	call, err := c.begin(r, rt.Need)
	if err != nil {
		return err
	}
	if call.Target, err = target(r, call.Workspace); err != nil {
		return err
	}
	if rt.Params != nil {
		if err := rt.Params(r, call); err != nil {
			return err
		}
	}
	call.Settings = c.settings(call.Workspace)
	call.Resolution, err = Resolve(r.Context(), c.deps.Secrets, call.Workspace, call.Target, call.Settings)
	if rerr, ok := errors.AsType[*ResolveError](err); ok {
		return server.NewError(rerr.Type, http.StatusUnprocessableEntity, rerr.Message)
	}
	if err != nil {
		return fmt.Errorf("resolve the forge of %s: %w", call.Workspace.Slug, err)
	}
	if rt.Check != nil {
		if err := rt.Check(r, call); err != nil {
			return c.failed(w, r, call, err)
		}
	}
	return c.failed(w, r, call, rt.Serve(w, r, call))
}

// failed answers err, when it is a failed forge call, as the hub's error
// for it (see MapError), with the forge's wait as Retry-After on a rate
// limit, and logs it at debug level; any other error is returned as it is.
// The message is scrubbed of the call's token before it is answered or
// logged.
func (c *Component) failed(w http.ResponseWriter, r *http.Request, call *Call, err error) error {
	e, ok := MapError(err, c.deps.Scrubber, call.Resolution.Credential.Token)
	if !ok {
		return err
	}
	if c.deps.Logger != nil {
		c.deps.Logger.LogAttrs(r.Context(), slog.LevelDebug, "forge call failed",
			slog.String("workspace", call.Slug), slog.String("error_type", e.Type), slog.String("message", e.Message))
	}
	if e.RetryAfter != "" {
		w.Header().Set("Retry-After", e.RetryAfter)
	}
	return e.ServerError()
}

// begin reads the workspace the request names, and for a task fork
// switches to its parent, on which the caller needs need as on the fork.
// The pipeline's access check on the fork does not decide that: a fork's
// owner owns it without regard to what they may do on the parent.
func (c *Component) begin(r *http.Request, need access.Need) (*Call, error) {
	ctx, slug := r.Context(), server.Param(r, "slug")
	call := &Call{Slug: slug, Client: c.client}
	ws, err := c.workspace(ctx, slug)
	if err != nil {
		return nil, err
	}
	call.Requested, call.Workspace = ws, ws
	if ws.Kind != access.KindFork {
		return call, nil
	}
	if ws.ParentSlug == nil {
		return nil, fmt.Errorf("task fork %s has no parent", slug)
	}
	caller, _ := server.CallerOf(ctx)
	// The decision ignores a bound token's binding: the token reaches the
	// fork, and the parent is reached through it.
	d, err := c.deps.Access.Decide(ctx, access.CallerFrom(caller), *ws.ParentSlug, need)
	if err != nil {
		return nil, fmt.Errorf("decide access to the parent of %s: %w", slug, err)
	}
	if d.Outcome != access.Granted {
		return nil, server.Forbidden(fmt.Sprintf("the caller has no %s access to the parent of task fork %q", need, slug))
	}
	if call.Workspace, err = c.workspace(ctx, *ws.ParentSlug); err != nil {
		return nil, err
	}
	return call, nil
}

// workspace reads the workspace slug, which the access check has found;
// one deleted meanwhile is not found.
func (c *Component) workspace(ctx context.Context, slug string) (workspace.Workspace, error) {
	ws, found, err := c.deps.Workspaces(ctx, slug)
	switch {
	case err != nil:
		return workspace.Workspace{}, fmt.Errorf("read workspace %s: %w", slug, err)
	case !found:
		return workspace.Workspace{}, server.NotFound("workspace not found")
	}
	return ws, nil
}

// target reads the query parameter target of r for the workspace ws whose
// remote is used.
func target(r *http.Request, ws workspace.Workspace) (Target, error) {
	values := r.URL.Query()["target"]
	switch {
	case len(values) == 0:
		return TargetOrigin, nil
	case len(values) > 1 || values[0] != string(TargetOrigin) && values[0] != string(TargetUpstream):
		return "", server.InvalidRequest("target must be given at most once, as origin or upstream")
	case values[0] == string(TargetOrigin):
		return TargetOrigin, nil
	case ws.Mode != workspace.ModeCarryPatch || ws.UpstreamURL == nil || *ws.UpstreamURL == "":
		return "", server.NewError("workspace_mode_mismatch", http.StatusBadRequest,
			fmt.Sprintf("target upstream needs a carry-patch workspace with an upstream_url; %q has none", ws.Slug))
	default:
		return TargetUpstream, nil
	}
}
