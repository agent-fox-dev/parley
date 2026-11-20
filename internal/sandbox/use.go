package sandbox

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/server"
)

// errorTypeNotReady answers a request to a route that uses a sandbox whose
// state the route does not allow, with 409. Every such state, deleted
// included, gets this one type.
const errorTypeNotReady = "sandbox_not_ready"

// The names of the routes that use a sandbox, by which their entries are
// known.
const (
	useExec     = "exec"
	useUpload   = "upload"
	useDownload = "download"
	useLogs     = "logs"
	useAttach   = "attach"
	useTools    = "tools"
)

// execBodyLimit is the body limit of exec: 4 MiB of stdin fits in it as
// base64.
const execBodyLimit = 8 << 20

// useRequest is what the gate hands an entry's validation and operation,
// so that neither looks up the caller, the workspace or the sandbox again.
type useRequest struct {
	// caller is who sent the request.
	caller identity.Principal
	// slug is the workspace the request names.
	slug string
	// rec is the sandbox as the gate read it.
	rec Record
	// exec is the body of an exec, as its validation read it.
	exec execRequest
	// files is the query of a file transfer, as its validation read it.
	files filesRequest
	// logs is the query of a log read, as its validation read it.
	logs logsRequest
	// attach is the query of an attach, as its validation read it.
	attach attachRequest
	// set is the caller's tool set, on a route that serves the sandbox's
	// tools.
	set string
	// act is the request's use of the sandbox, which the operation starts
	// and fills in and the gate ends.
	act *activity
}

// useEntry is one route that uses a sandbox, under
// /workspaces/:slug/sandboxes/:id: what the gate in front of its operation
// decides, and the operation.
type useEntry struct {
	// name names the entry.
	name string
	// method and segment are the route's method and the last segment of
	// its path.
	method, segment string
	// kind decides the PAT scope and the workspace access the route needs.
	kind Kind
	// covers applies the no-widening rule: the caller must cover the
	// sandbox's hub credential, since the route acts in the sandbox.
	covers bool
	// tools marks a route that serves the sandbox's tools: a sandbox
	// without a tool server is answered 404 tools_unavailable, and the
	// caller's tool set is decided for the request. Such a route is of the
	// read kind and does not apply the no-widening rule, which decides the
	// set instead.
	tools bool
	// states are the states in which the route serves the sandbox; nil
	// for every state.
	states []string
	// maxBody is the route's body limit, 0 for the server's; streamBody
	// leaves the body to the operation, which then enforces its own limit
	// and media types.
	maxBody    int64
	streamBody bool
	// validate reads and checks the request, after the no-widening rule and
	// before the state check, and may leave in u what it read for the
	// operation; nil for a route with nothing to validate.
	validate func(r *http.Request, u *useRequest) error
	// operate serves the request once every check passed.
	operate func(w http.ResponseWriter, r *http.Request, u useRequest) error
}

// useTable is the table of the routes that use a sandbox.
func (s *Service) useTable() []*useEntry {
	exec := newExecutor(s)
	return []*useEntry{
		{
			name: useExec, method: http.MethodPost, segment: "exec", kind: KindWrite, covers: true,
			states: []string{StateReady}, maxBody: execBodyLimit, validate: exec.validate, operate: exec.serve,
		},
		{
			name: useUpload, method: http.MethodPut, segment: "files", kind: KindWrite, covers: true,
			states: []string{StateReady}, streamBody: true, validate: validateUpload, operate: s.upload,
		},
		{
			name: useDownload, method: http.MethodGet, segment: "files", kind: KindRead,
			states: []string{StateReady, StateCompleted, StateFailed}, validate: validateDownload, operate: s.download,
		},
		{
			name: useLogs, method: http.MethodGet, segment: "logs", kind: KindRead,
			validate: validateLogs, operate: s.serveLogs,
		},
		{
			name: useAttach, method: http.MethodGet, segment: "attach", kind: KindWrite, covers: true,
			states: []string{StateReady}, validate: validateAttach, operate: s.attach,
		},
		{
			name: useTools, method: http.MethodGet, segment: "tools", kind: KindRead, tools: true,
			states: []string{StateReady}, operate: s.listTools,
		},
	}
}

// UseRoutes declares the routes that use a sandbox: POST …/exec, PUT and
// GET …/files, GET …/logs, GET …/attach and GET …/tools under
// /workspaces/:slug/sandboxes/:id. Each is a workspace route of its kind,
// so that the pipeline runs the feature check, the binding guard, the PAT
// scope and the workspace access before its handler, the gate (see gate).
func (s *Service) UseRoutes() []server.Route {
	routes := make([]server.Route, 0, len(s.use))
	for _, e := range s.use {
		routes = append(routes, s.WorkspaceRoute(e.kind, server.Route{
			Method:      e.method,
			Pattern:     "/workspaces/:slug/sandboxes/:id/" + e.segment,
			MaxBodySize: e.maxBody,
			StreamBody:  e.streamBody,
			Handler:     s.gate(e),
		}))
	}
	return routes
}

// gate serves a request to the route of e once the pipeline's checks have
// passed: it finds the sandbox, applies the no-widening rule when e acts in
// the sandbox, refuses a tool route of a sandbox without a tool server and
// decides the caller's tool set, validates the request, checks the sandbox's state, and only
// then runs e's operation, whose runtime failures it answers (see
// answerUse). The order is fixed so that an authorization answer never
// depends on the body, and a body error never on the state. An operation
// that started records the end of its activity and is told to the
// observers once its answer is written (see activity).
func (s *Service) gate(e *useEntry) server.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		d, ok := access.DecisionOf(r)
		if !ok {
			return server.Internal("a route that uses a sandbox found no access decision")
		}
		ctx := r.Context()
		rec, err := s.readSandbox(ctx, d.Record.Slug, server.Param(r, "id"))
		if err != nil {
			return err
		}
		if e.covers {
			if err := RequireCovers(r, rec.HubAccess); err != nil {
				return err
			}
		}
		caller, _ := identity.PrincipalOf(ctx)
		u := useRequest{caller: caller, slug: d.Record.Slug, rec: rec}
		if e.tools {
			if rec.Snapshot.ToolServer == "" {
				return toolsUnavailable()
			}
			u.set = toolSetOf(r, d.Level, rec)
		}
		if e.validate != nil {
			if err := e.validate(r, &u); err != nil {
				return err
			}
		}
		if e.states != nil && !slices.Contains(e.states, rec.State) {
			return notReady(rec.State)
		}
		aw := &answerWriter{ResponseWriter: w}
		u.act = s.newActivity(ctx, aw, u)
		// Deferred, so that an operation that drops its connection is
		// recorded too; it runs once the answer below is written.
		defer u.act.end()
		return u.act.answer(s.answerUse(ctx, rec.ID, e.operate(aw, r, u)))
	}
}

// notReady is the answer to a request the sandbox's state does not allow.
func notReady(state string) *server.Error {
	return server.NewError(errorTypeNotReady, http.StatusConflict, "the sandbox is "+state+", which this operation does not allow")
}

// answerUse returns what answers err, the error of an operation on the
// sandbox id. A *server.Error is the operation's own answer. A runtime
// failure of any kind is the runtime-error answer, since the operation
// answers itself what it can act on (a path the runtime does not find);
// its message is the runtime's text cleared first of the sandbox's own
// secret values, which the answer of every sandbox route (Route) does not
// know, and then of the hub's. Any other error keeps its kind for the pipeline with its
// text cleared the same way, since its text goes to the hub's log.
func (s *Service) answerUse(ctx context.Context, id string, err error) error {
	var answer *server.Error
	if err == nil || errors.As(err, &answer) {
		return err
	}
	// The caller may have left; the values are still needed for the log.
	text := s.scrub(s.ScrubSandbox(context.WithoutCancel(ctx), id, err.Error()))
	var failure *runtime.Error
	if errors.As(err, &failure) || errors.Is(err, runtime.ErrUnavailable) || errors.Is(err, runtime.ErrUnsupported) {
		return RuntimeError(text)
	}
	return &scrubbedError{text: text, err: err}
}
