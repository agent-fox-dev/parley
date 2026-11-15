// Package scm serves the read half of the source control API: the refs,
// commits, trees, files, diffs, comparisons, blame and content search of a
// workspace's repository, each answer computed from commits named by their
// full ids.
//
// The routes are GET routes under /workspaces/:slug/scm. Each needs read
// access to the workspace and, for a personal access token, git:read, which
// the server's shared checks decide before a handler runs; each reads only
// a workspace whose repository is intact, and reports every request it
// handles to a request recorder.
//
// A read names commits by revisions, which one resolver turns into commit
// ids: the forms it accepts are a branch name, a tag name, a full ref name
// and a full or abbreviated commit id, and nothing a caller writes is handed
// to git as a revision expression or an option. Refs are read from the
// workspace's ref view (RefView), so that a fork answers with its own view
// of its parent's branches.
package scm

import (
	"context"
	"errors"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The results a read reports to the request recorder.
const (
	ResultOK    = "ok"
	ResultError = "error"
)

// The ops the read routes report, one per kind of read.
const (
	opRefs    = "refs"
	opCommits = "commits"
)

// scopeRead is the scope a personal access token needs for every read;
// git:write implies it.
const scopeRead = "git:read"

// Recorder is told of every request a read route handles, after its
// handler has returned: the route's op, such as refs, and ResultOK when
// the answer's status is below 400, ResultError otherwise. A request the
// server's shared checks refuse reaches no route and is not reported.
type Recorder interface {
	Observe(op, result string)
}

// Deps are what the read routes need. Workspaces and Open are required;
// every other member has a default.
type Deps struct {
	// Workspaces reads the record of the workspace slug; found is false
	// when there is none.
	Workspaces func(ctx context.Context, slug string) (w workspace.Workspace, found bool, err error)
	// ParentState is the check a read of the workspace slug makes of its
	// parent, nil for a workspace that is no fork, and otherwise the
	// refusal the read answers; nil checks nothing.
	ParentState func(ctx context.Context, slug string) error
	// Open opens the repository of the workspace slug.
	Open func(ctx context.Context, slug string) (Repository, error)
	// RefView is the ref view of the workspace w, whose repository repo
	// is; nil makes every workspace's view what its repository holds
	// (RepoRefs).
	RefView func(ctx context.Context, w workspace.Workspace, repo Repository) RefView
	// Protected says which branches of a workspace are protected; nil
	// protects its default branch alone.
	Protected ProtectedLookup
	// Changes names the change of a source branch or of a commit; nil
	// names none.
	Changes ChangeLookup
	// Sessions names the session that introduced a commit; nil names
	// none.
	Sessions SessionLookup
	// Recorder is told of every request a read route handles; nil tells
	// no one.
	Recorder Recorder
}

// Component serves the read routes. Its zero value is ready to be
// registered; it serves once Bind has given it its dependencies, before
// the first request.
type Component struct{ deps Deps }

// Bind gives the component its dependencies, the defaults in place of the
// members d leaves nil.
func (c *Component) Bind(d Deps) {
	if d.RefView == nil {
		d.RefView = func(_ context.Context, _ workspace.Workspace, repo Repository) RefView { return RepoRefs(repo) }
	}
	if d.Protected == nil {
		d.Protected = defaultBranchOnly{}
	}
	if d.Changes == nil {
		d.Changes = noChanges{}
	}
	if d.Sessions == nil {
		d.Sessions = noSessions{}
	}
	if d.Recorder == nil {
		d.Recorder = noRecorder{}
	}
	c.deps = d
}

// Routes are the read routes, for the server's router.
func (c *Component) Routes() []server.Route {
	return []server.Route{
		c.route("/refs", opRefs, c.listRefs),
		c.route("/refs/*name", opRefs, c.getRef),
		c.route("/commits", opCommits, c.listCommits),
		c.route("/commits/:sha", opCommits, c.getCommit),
	}
}

// route is the read route at pattern below /workspaces/:slug/scm, whose
// requests h answers and are reported under op. The shared checks answer
// 401, 403 insufficient_scope and 404 not_found before h runs.
func (c *Component) route(pattern, op string, h server.HandlerFunc) server.Route {
	return server.Route{Method: http.MethodGet, Pattern: "/workspaces/:slug/scm" + pattern, Scope: scopeRead,
		Marks: map[string]string{access.MarkNeed: string(access.NeedRead)}, Handler: c.observe(op, h)}
}

// observe is h reporting each request it answers to the recorder once,
// with the status it wrote or, when it returned an error before writing,
// the status the error is answered with.
func (c *Component) observe(op string, h server.HandlerFunc) server.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		sw := &statusWriter{ResponseWriter: w}
		err := h(sw, r)
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

// errorStatus is the status err is answered with: its own for an API
// error, 500 for any other, and 200 for none.
func errorStatus(err error) int {
	var apiErr *server.Error
	switch {
	case err == nil:
		return http.StatusOK
	case errors.As(err, &apiErr):
		return apiErr.Status()
	}
	return http.StatusInternalServerError
}

// statusWriter remembers the status of the answer written through it.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(status int) {
	if s.status == 0 {
		s.status = status
	}
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the server's writer.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// read is one read of a workspace that may be read: its record, its
// repository, its ref view and the resolver of the read's revisions.
type read struct {
	ws       workspace.Workspace
	repo     Repository
	view     RefView
	resolver *Resolver
	// isProtected is the workspace's protected-branch test, once asked.
	isProtected func(branch string) bool
}

// open begins a read of the route's workspace, once the read's query has
// been checked. A workspace in a state that forbids reading is refused
// before its repository is opened (readable, then the parent's state for a
// fork).
func (c *Component) open(r *http.Request) (*read, error) {
	ctx, slug := r.Context(), server.Param(r, "slug")
	ws, found, err := c.deps.Workspaces(ctx, slug)
	if err != nil {
		return nil, err
	}
	if !found {
		// Deleted since the access check: the answer that check gives for a
		// slug no workspace has.
		return nil, server.NotFound(workspace.ErrNotFound.Error())
	}
	if err := readable(ws); err != nil {
		return nil, err
	}
	if c.deps.ParentState != nil {
		if err := c.deps.ParentState(ctx, slug); err != nil {
			return nil, err
		}
	}
	repo, err := c.deps.Open(ctx, slug)
	if err != nil {
		return nil, err
	}
	view := c.deps.RefView(ctx, ws, repo)
	var defaultBranch string
	if ws.DefaultBranch != nil {
		defaultBranch = *ws.DefaultBranch
	}
	return &read{ws: ws, repo: repo, view: view, resolver: NewResolver(repo, view, defaultBranch)}, nil
}

// readable is nil for a workspace whose repository may be read: ready, or
// archived with no maintenance operation running, since archiving leaves
// the repository as it was; otherwise the workspace precondition's
// refusal, 409 workspace_busy or workspace_not_ready.
func readable(w workspace.Workspace) error {
	if w.Status == workspace.StatusArchived && w.Marker == workspace.MarkerNone {
		return nil
	}
	return workspace.RequireReady(w)
}

type noRecorder struct{}

func (noRecorder) Observe(string, string) {}
