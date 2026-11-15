// Package scm serves the source control API: the reads of the refs,
// commits, trees, files, diffs, comparisons, blame and content search of a
// workspace's repository, each answer computed from commits named by their
// full ids, and the writes to it.
//
// Every write route is built by one wrapper (writeRoute), which needs, for a
// personal access token, git:write and write access to the workspace, and
// refuses a workspace, or a fork's parent, that cannot take a write before
// the route's handler runs. Every ref a write moves goes through one step
// (updateRef): the push rule engine decides it and the ref writer writes
// it by compare-and-swap and emits its events, as for a git push, so this
// package writes no ref of its own.
//
// The read routes are GET routes under /workspaces/:slug/scm. Each needs read
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
//
// Every successful read carries an entity tag, derived from the commit ids
// it resolved, its query and its representation, and answers 304 to an
// If-None-Match that holds it, before the costly part of the read; one
// whose revisions were all written as full commit ids is marked immutable.
//
// Every read is bounded: it has the read timeout from the moment its route
// begins handling it, and every git command it runs ends with it. A read
// that has not begun its answer by then is answered 503 scm_timeout; one
// whose answer has begun is cut off.
package scm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The results a request reports to the request recorder: ResultOK for an
// answer whose status is below 400 and ResultError for any other, except
// that a write reports ResultRefMoved for a 409 ref_moved and ResultRefused
// for a 403 of a push rule.
const (
	ResultOK       = "ok"
	ResultError    = "error"
	ResultRefMoved = "ref_moved"
	ResultRefused  = "refused"
)

// The ops the read routes report, one per kind of read.
const (
	opRefs    = "refs"
	opCommits = "commits"
	opTree    = "tree"
	opFile    = "file"
	opDiff    = "diff"
	opCompare = "compare"
	opBlame   = "blame"
	opSearch  = "search"
)

// DefaultReadTimeout is how long a read may run when Deps sets no read
// timeout.
const DefaultReadTimeout = 30 * time.Second

// errReadTimeout is the cause of the end of a read's context at its
// deadline, which tells it from a client that went away.
var errReadTimeout = errors.New("the read timed out")

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

// Deps are what the routes need. Workspaces and Open are required;
// every other member has a default.
type Deps struct {
	// Workspaces reads the record of the workspace slug; found is false
	// when there is none.
	Workspaces func(ctx context.Context, slug string) (w workspace.Workspace, found bool, err error)
	// ParentState is the check a read of the workspace slug makes of its
	// parent, nil for a workspace that is no fork, and otherwise the
	// refusal the read answers; nil checks nothing.
	ParentState func(ctx context.Context, slug string) error
	// WriteParentState is the check a write to the fork slug makes of its
	// parent, nil when the write may go on and otherwise the refusal the
	// write answers, such as 409 parent_archived; nil checks nothing.
	WriteParentState func(ctx context.Context, slug string) error
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
	// Recorder is told of every request a read or write route handles;
	// nil tells no one.
	Recorder Recorder
	// Rules decide every ref update a write asks for; nil is a push rule
	// engine with no lookups.
	Rules Rules
	// RefWriter writes every ref update the rules allow, and its events;
	// nil is a ref writer with no store, no stages and no event log.
	RefWriter RefWriter
	// MaxFileSize is the size in bytes of the largest file the file route
	// serves; zero or less is DefaultMaxFileSize.
	MaxFileSize int64
	// ReadTimeout is how long a read may run, from the moment its route
	// begins handling it; zero or less is DefaultReadTimeout.
	ReadTimeout time.Duration
	// Now is the clock the commits a write makes take their times from;
	// nil is time.Now.
	Now func() time.Time
	// Logger is told of what fails after a write has moved its ref and so
	// cannot change its answer; nil discards it.
	Logger *slog.Logger
}

// Component serves the read and write routes. Its zero value is ready to
// be registered; it serves once Bind has given it its dependencies, before
// the first request.
type Component struct {
	deps Deps

	mu sync.RWMutex
	// linker is the registered change linker, nil for none.
	linker ChangeLinker
	// sessions is the registered session linker, nil for none.
	sessions SessionLinker
	// replayed, when not nil, runs once a rebase has replayed its
	// commits and before it moves the branch: a test's way to move the
	// branch meanwhile.
	replayed func()
}

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
	if d.Rules == nil {
		d.Rules = &pushrule.Engine{}
	}
	if d.RefWriter == nil {
		d.RefWriter = &pushrule.Writer{}
	}
	if d.MaxFileSize <= 0 {
		d.MaxFileSize = DefaultMaxFileSize
	}
	if d.ReadTimeout <= 0 {
		d.ReadTimeout = DefaultReadTimeout
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	c.deps = d
}

// Routes are the read and write routes, for the server's router. The
// commit route's body limit is DefaultMaxCommitSize until LimitCommitBody
// sets another. The three POST routes take an Idempotency-Key, whose replay
// answers 200 with the first answer and moves nothing; PATCH and DELETE
// need none, since expected_sha makes a retry safe.
func (c *Component) Routes() []server.Route {
	commit := c.writeRoute(http.MethodPost, commitsPattern, opCommit, c.createCommit)
	commit.MaxBodySize = DefaultMaxCommitSize
	createRef := c.writeRoute(http.MethodPost, "/refs", opRefCreate, c.createRef)
	rebase := c.writeRoute(http.MethodPost, "/rebase", opRebase, c.rebase)
	for _, r := range []*server.Route{&commit, &createRef, &rebase} {
		r.Create = true
	}
	return []server.Route{
		commit,
		createRef,
		c.writeRoute(http.MethodPatch, "/refs/*name", opRefMove, c.moveRef),
		c.writeRoute(http.MethodDelete, "/refs/*name", opRefDelete, c.deleteRef),
		rebase,
		c.route("/refs", opRefs, c.listRefs),
		c.route("/refs/*name", opRefs, c.getRef),
		c.route("/commits", opCommits, c.listCommits),
		c.route("/commits/:sha", opCommits, c.getCommit),
		c.route("/tree", opTree, c.listTree),
		c.route("/file", opFile, c.getFile),
		c.route("/diff", opDiff, c.getDiff),
		c.route("/compare", opCompare, c.getCompare),
		c.route("/blame", opBlame, c.getBlame),
		c.route("/search", opSearch, c.search),
	}
}

// route is the read route at pattern below /workspaces/:slug/scm, whose
// requests h answers and are reported under op. The shared checks answer
// 401, 403 insufficient_scope and 404 not_found before h runs.
func (c *Component) route(pattern, op string, h server.HandlerFunc) server.Route {
	return server.Route{Method: http.MethodGet, Pattern: "/workspaces/:slug/scm" + pattern, Scope: scopeRead,
		Marks: map[string]string{access.MarkNeed: string(access.NeedRead)}, Handler: c.observe(op, h)}
}

// observe is h bounded by the read timeout and reporting each request it
// answers to the recorder once, with the status it wrote or, when it
// returned an error before writing, the status the error is answered
// with. An answer h did not write itself, an error's, carries none of the
// caching headers h set before it failed.
//
// h's request carries a context that ends at the read timeout, which ends
// every git command h runs with it. Once that deadline has passed, nothing
// h writes reaches the client: a read that had not begun its answer is
// answered 503 scm_timeout, with none of the headers h set, and one whose
// answer had begun and did not finish it is cut off, so that the client
// cannot take a part of a body for all of it.
func (c *Component) observe(op string, h server.HandlerFunc) server.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		ctx, cancel := context.WithTimeoutCause(r.Context(), c.deps.ReadTimeout, errReadTimeout)
		defer cancel()
		sw := &statusWriter{ResponseWriter: w, ctx: ctx}
		before := w.Header().Clone()
		restore := uncached(w.Header())
		defer func() {
			if sw.status == 0 {
				restore()
			}
		}()
		err := h(sw, r.WithContext(ctx))
		timedOut := context.Cause(ctx) == errReadTimeout
		if timedOut && sw.status == 0 {
			if sw.refused {
				replaceHeader(w.Header(), before)
			}
			err = server.NewError("scm_timeout", http.StatusServiceUnavailable,
				fmt.Sprintf("the read did not finish within the read timeout of %s", c.deps.ReadTimeout))
		}
		cut := timedOut && sw.status != 0 && (err != nil || sw.refused)
		status := sw.status
		if status == 0 {
			status = errorStatus(err)
		}
		result := ResultOK
		if status >= http.StatusBadRequest || cut {
			result = ResultError
		}
		c.deps.Recorder.Observe(op, result)
		if cut {
			// net/http drops the connection, so that the answer does not end
			// as if it were whole.
			panic(http.ErrAbortHandler)
		}
		return err
	}
}

// replaceHeader makes h hold what was holds, and nothing else.
func replaceHeader(h, was http.Header) {
	for name := range h {
		delete(h, name)
	}
	for name, values := range was {
		h[name] = values
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

// statusWriter remembers the status of the answer written through it,
// and passes nothing on once the read's context has reached its deadline.
type statusWriter struct {
	http.ResponseWriter
	ctx    context.Context
	status int
	// refused is set once a write came after the deadline.
	refused bool
}

// late reports whether the read's deadline has passed, and remembers that
// a write was refused for it.
func (s *statusWriter) late() bool {
	if context.Cause(s.ctx) != errReadTimeout {
		return false
	}
	s.refused = true
	return true
}

func (s *statusWriter) WriteHeader(status int) {
	if s.late() {
		return
	}
	if s.status == 0 {
		s.status = status
	}
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.late() {
		return 0, context.Cause(s.ctx)
	}
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
