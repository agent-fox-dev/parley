package carrypatch

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/server"
)

// The scopes of the rebuild routes.
const (
	scopeRebuildsRead  = "rebuilds:read"
	scopeRebuildsWrite = "rebuilds:write"
)

// The events of a cancel and of a requeue of a rebuild job, and of a
// rollback of the integration branch to the head before a rebuild.
const (
	EventRebuildCancel   = "parley.rebuild.cancel"
	EventRebuildRequeue  = "parley.rebuild.requeue"
	EventRebuildRollback = "parley.rebuild.rollback"
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
// Idempotency-Key with the first answer; the list and the read of one
// job, read routes that serve in every state of the workspace; and the
// cancel and the requeue of one job, write routes that change it; and the
// rollback of the integration branch to the head before one completed job,
// a write route that creates nothing, so that a repeated Idempotency-Key is
// not answered from the first answer.
func (c *Component) rebuildRoutes() []server.Route {
	return []server.Route{
		rebuildRoute(WriteRoute, server.Route{Method: http.MethodPost, Pattern: "/workspaces/:slug/rebuild", Handler: c.submitRoute, Create: true}),
		rebuildRoute(ReadRoute, server.Route{Method: http.MethodGet, Pattern: "/workspaces/:slug/rebuilds", Handler: c.listRebuildsRoute}),
		rebuildRoute(ReadRoute, server.Route{Method: http.MethodGet, Pattern: "/workspaces/:slug/rebuilds/:id", Handler: c.readRebuildRoute}),
		rebuildRoute(WriteRoute, server.Route{Method: http.MethodDelete, Pattern: "/workspaces/:slug/rebuilds/:id", Handler: c.cancelRebuildRoute}),
		rebuildRoute(WriteRoute, server.Route{Method: http.MethodPost, Pattern: "/workspaces/:slug/rebuilds/:id/requeue", Handler: c.requeueRebuildRoute}),
		rebuildRoute(WriteRoute, server.Route{Method: http.MethodPost, Pattern: "/workspaces/:slug/rebuilds/:id/rollback", Handler: c.rollbackRoute}),
	}
}

// listRebuildsRoute answers GET /workspaces/:slug/rebuilds: a page of the
// workspace's rebuild jobs, in the status the query names when it names
// one, newest first. The query is checked before the guard, so that an
// invalid one is answered as such on any workspace.
func (c *Component) listRebuildsRoute(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	status, err := statusFilter(r.URL.Query())
	if err != nil {
		return err
	}
	var after jobs.ListPosition
	paged, err := page.After(&after)
	if err != nil {
		return err
	}
	ws, err := c.Guard(r)
	if err != nil {
		return err
	}
	if c.d.Jobs == nil {
		return errNotBound
	}
	var from *jobs.ListPosition
	if paged {
		from = &after
	}
	js, err := c.d.Jobs.List(r.Context(), RebuildJobType, ws.Slug, status, from, page.Limit+1)
	if err != nil {
		return err
	}
	items := make([]RebuildRecord, 0, len(js))
	keys := make(map[string]jobs.ListPosition, len(js))
	for _, j := range js {
		items = append(items, RebuildRecordOf(j))
		keys[j.ID] = j.Position()
	}
	return server.WritePage(w, page, items, func(rec RebuildRecord) any { return keys[rec.ID] })
}

// statusFilter reads the query parameter status: absent lists every job,
// one of the six statuses of a job lists those in it; anything else, the
// empty value, a list and a repeated parameter included, is 400
// invalid_request rather than a guess.
func statusFilter(query url.Values) (string, error) {
	values, ok := query["status"]
	if !ok {
		return "", nil
	}
	if len(values) == 1 && slices.Contains(jobs.Statuses, values[0]) {
		return values[0], nil
	}
	return "", server.InvalidRequest(fmt.Sprintf("status must be given once, as one of %q", jobs.Statuses))
}

// readRebuildRoute answers GET /workspaces/:slug/rebuilds/:id with the
// job's record; that of a running job holds the result its attempt stored
// last.
func (c *Component) readRebuildRoute(w http.ResponseWriter, r *http.Request) error {
	ws, err := c.Guard(r)
	if err != nil {
		return err
	}
	j, err := c.rebuildJob(r.Context(), ws.Slug, server.Param(r, "id"))
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, RebuildRecordOf(j))
	return nil
}

// rebuildJob is the rebuild job id of the workspace slug, the lookup of
// every route of one job, made after the guard so that the workspace's
// mode and state are answered first. An id that is not a UUID, that names
// no job, a job of another workspace or a job of another type is the one
// 404, so that an answer does not tell which jobs other workspaces hold.
func (c *Component) rebuildJob(ctx context.Context, slug, id string) (jobs.Job, error) {
	if c.d.Jobs == nil {
		return jobs.Job{}, errNotBound
	}
	if _, err := uuid.Parse(id); err != nil || len(id) != 36 {
		return jobs.Job{}, errRebuildNotFound()
	}
	j, err := c.d.Jobs.Get(ctx, id)
	if errors.Is(err, jobs.ErrNotFound) {
		return jobs.Job{}, errRebuildNotFound()
	}
	if err != nil {
		return jobs.Job{}, fmt.Errorf("read the rebuild job %s: %w", id, err)
	}
	if j.Workspace != slug || j.Type != RebuildJobType {
		return jobs.Job{}, errRebuildNotFound()
	}
	return j, nil
}

// errRebuildNotFound is the 404 of a rebuild job the workspace does not
// have.
func errRebuildNotFound() error { return server.NotFound("rebuild job not found") }

// cancelRebuildRoute answers DELETE /workspaces/:slug/rebuilds/:id: it
// cancels the queued job and answers its record; a job in any other status
// is 409 job_not_cancellable.
func (c *Component) cancelRebuildRoute(w http.ResponseWriter, r *http.Request) error {
	if c.d.Jobs == nil {
		return errNotBound
	}
	return c.changeRebuild(w, r, EventRebuildCancel, c.d.Jobs.Cancel)
}

// requeueRebuildRoute answers POST /workspaces/:slug/rebuilds/:id/requeue,
// whatever its body: it queues the dead-lettered job again, from a retry
// count of 0, and answers its record. A job in any other status is 409
// job_not_requeueable, whatever the workspace's other jobs are; a
// dead-lettered one while another rebuild of the workspace is queued,
// running or failed is 409 concurrent_rebuild, naming that job.
func (c *Component) requeueRebuildRoute(w http.ResponseWriter, r *http.Request) error {
	if c.d.Jobs == nil {
		return errNotBound
	}
	return c.changeRebuild(w, r, EventRebuildRequeue, c.d.Jobs.Requeue)
}

// changeRebuild answers a route that changes one rebuild job: after the
// guard and the lookup, change and the event typ, with the caller as its
// actor and no metadata, are written in one store transaction, and the
// answer is the record of the job as change left it.
func (c *Component) changeRebuild(w http.ResponseWriter, r *http.Request, typ string,
	change func(ctx context.Context, tx *sql.Tx, id string) (jobs.Job, error)) error {
	if c.d.Store == nil || c.d.Events == nil {
		return errNotBound
	}
	ws, err := c.Guard(r)
	if err != nil {
		return err
	}
	j, err := c.rebuildJob(r.Context(), ws.Slug, server.Param(r, "id"))
	if err != nil {
		return err
	}
	by, err := actorOf(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	var changed jobs.Job
	err = c.d.Store.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		if changed, err = change(ctx, tx, j.ID); err != nil {
			return err
		}
		_, err = c.d.Events.Emit(ctx, tx, event.Input{
			Type: typ, Workspace: &ws.Slug, ActorType: by.typ, ActorID: by.id, ResourceType: rebuildResource, ResourceID: j.ID,
		})
		return err
	})
	if dup, ok := errors.AsType[*jobs.DuplicateError](err); ok {
		return errConcurrentRebuild(ws.Slug, dup.JobID)
	}
	switch {
	case errors.Is(err, jobs.ErrNotCancellable):
		return server.NewError("job_not_cancellable", http.StatusConflict, fmt.Sprintf("rebuild job %s is not queued; only a queued job can be cancelled", j.ID))
	case errors.Is(err, jobs.ErrNotRequeueable):
		return server.NewError("job_not_requeueable", http.StatusConflict, fmt.Sprintf("rebuild job %s is not dead_letter; only a dead_letter job can be requeued", j.ID))
	case errors.Is(err, jobs.ErrNotFound):
		// Removed with its workspace since the lookup.
		return errRebuildNotFound()
	case err != nil:
		return err
	}
	server.WriteJSON(w, http.StatusOK, RebuildRecordOf(changed))
	return nil
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
