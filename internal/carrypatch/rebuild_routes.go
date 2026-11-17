package carrypatch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"

	"github.com/agent-fox-dev/parley/internal/server"
)

// The scopes of the rebuild routes.
const (
	scopeRebuildsRead  = "rebuilds:read"
	scopeRebuildsWrite = "rebuilds:write"
)

// rebuildRoute declares the rebuild route rt as a carry-patch route of
// kind, so that the pipeline checks workspace access and the guard knows
// the kind, with the scope of the rebuilds in place of that of the
// patches.
func rebuildRoute(kind RouteKind, rt server.Route) server.Route {
	rt = Route(kind, rt)
	rt.Scope = scopeRebuildsRead
	if kind == WriteRoute {
		rt.Scope = scopeRebuildsWrite
	}
	return rt
}

// rebuildRoutes are the routes of the rebuild jobs: the submit, a write
// route that creates, so that the server answers a repeated
// Idempotency-Key with the first answer.
func (c *Component) rebuildRoutes() []server.Route {
	return []server.Route{
		rebuildRoute(WriteRoute, server.Route{Method: http.MethodPost, Pattern: "/workspaces/:slug/rebuild", Handler: c.submitRoute, Create: true}),
	}
}

// submitRoute answers POST /workspaces/:slug/rebuild: it queues a rebuild
// of the workspace and answers 202 with the job's record. The body is
// optional and validated before the guard; a workspace with no patch in
// active or conflict is 400 no_active_patches, and one with a rebuild
// queued, running or failed is 409 concurrent_rebuild, naming that job.
// The caller submits the rebuild: a user as its id, the admin token, which
// has none, as its actor type.
func (c *Component) submitRoute(w http.ResponseWriter, r *http.Request) error {
	opts, err := submitOptionsOf(r)
	if err != nil {
		return err
	}
	ws, err := c.Guard(r)
	if err != nil {
		return err
	}
	by, err := actorOf(r)
	if err != nil {
		return err
	}
	opts.ActorType, opts.ActorID, opts.SubmittedBy = by.typ, by.id, by.typ
	if by.id != nil {
		opts.SubmittedBy = *by.id
	}
	job, existing, err := c.enqueueRebuild(r.Context(), ws, opts, true)
	if err != nil {
		return err
	}
	if existing != "" {
		return errConcurrentRebuild(ws.Slug, existing)
	}
	server.WriteJSON(w, http.StatusAccepted, RebuildRecordOf(job))
	return nil
}

// errConcurrentRebuild is 409 concurrent_rebuild for the pending rebuild
// job id of slug.
func errConcurrentRebuild(slug, id string) error {
	return server.NewError("concurrent_rebuild", http.StatusConflict,
		fmt.Sprintf("workspace %q has the rebuild job %s queued, running or failed", slug, id))
}

// submitOptionsOf reads the optional body of a submit: no body, one of
// white space only and {} are alike. strategy and fail_mode are a string
// from their lists or null; an empty string or null leaves the setting to
// decide. Members the body does not define are ignored.
func submitOptionsOf(r *http.Request) (RebuildOptions, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return RebuildOptions{}, server.InvalidRequest("request body could not be read")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return RebuildOptions{}, nil
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var members map[string]json.RawMessage
	if err := server.DecodeJSON(r, &members); err != nil {
		return RebuildOptions{}, err
	}
	var opts RebuildOptions
	if opts.Strategy, err = enumMember(members, "strategy", strategyRebase, strategyMerge); err != nil {
		return RebuildOptions{}, err
	}
	if opts.FailMode, err = enumMember(members, "fail_mode", failModeFailFast, failModeContinue); err != nil {
		return RebuildOptions{}, err
	}
	return opts, nil
}

// enumMember is the string member name, nil when it is absent, null or
// empty; any other value outside values is 400 invalid_request.
func enumMember(members map[string]json.RawMessage, name string, values ...string) (*string, error) {
	v, err := stringMember(members, name)
	if err != nil || v == nil || *v == "" {
		return nil, err
	}
	if !slices.Contains(values, *v) {
		return nil, server.InvalidRequest(fmt.Sprintf("%s must be one of %q, not %q", name, values, *v))
	}
	return v, nil
}
