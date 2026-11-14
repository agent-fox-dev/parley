package gitserver_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The paths of demo's ref discovery for fetch and of a push to a repository
// that does not exist.
const (
	demoRefs     = "/git/acme/demo.git/info/refs?service=git-upload-pack"
	nosuchPush   = "/git/acme/nosuch.git/git-receive-pack"
	fixedRequest = "5f0c6a52-3a8e-4d43-9c55-0d8b1f0e2a41"
)

// world is a hub the git server tests ask, on a clock that stands still
// unless a test moves it: the org acme; its member owner, who owns the
// ready workspace demo, and its member member; stranger, who belongs to no
// org; each with an API key, and owner with a PAT that holds git:read. The
// owner also owns solo, whose organization no longer exists, and the
// stranger owns other in the org beta. demo's
// repository is at its place on disk, a bare repository whose default branch
// main has two commits. The git server's service is a recorder, so that a
// test sees what reached it.
type world struct {
	*hubtest.Hub
	workspaces *workspace.Service
	service    *recorder

	owner, member, stranger          string
	ownerKey, memberKey, strangerKey string
	ownerPAT                         string
	acme                             string
	// repo is the git directory of demo's repository.
	repo string

	// mu guards now, the hub's time.
	mu  sync.Mutex
	now time.Time
}

// newWorld starts a hub with what cfg adds to its configuration and builds
// the world in it; the world sets cfg.Now and adds to cfg.Compose. Each of
// the world's admin calls starts a new rate window, so that a world on a
// hub with a low request limit can still be built.
func newWorld(t *testing.T, cfg hubtest.Config) *world {
	t.Helper()
	w := &world{now: time.Now().UTC().Truncate(time.Second), service: &recorder{}}
	compose := cfg.Compose
	cfg.Now, cfg.Compose = w.clock, func(c *hub.Composition) {
		w.workspaces = c.Workspaces()
		c.AdjustGitServer(func(d *gitserver.Deps) { d.Service = w.service })
		if compose != nil {
			compose(c)
		}
	}
	w.Hub = hubtest.StartWith(t, cfg)
	w.owner, w.member, w.stranger = w.CreateUser(t, "owner"), w.CreateUser(t, "member"), w.CreateUser(t, "stranger")
	w.ownerKey, w.memberKey, w.strangerKey = w.IssueKey(t, w.owner), w.IssueKey(t, w.member), w.IssueKey(t, w.stranger)
	w.ownerPAT = w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: []string{"git:read"}, CreatedAt: w.clock()})
	w.acme = w.CreateOrg(t, "acme")
	w.advance(time.Minute)
	for _, user := range []string{w.owner, w.member} {
		w.AddMember(t, w.acme, user)
		w.advance(time.Minute)
	}
	beta := w.CreateOrg(t, "beta")
	w.advance(time.Minute)
	w.AddWorkspace(t, access.Record{Slug: "solo", OwnerID: w.owner, OrgID: "org-deleted", Kind: access.KindRepository, Status: access.StatusReady})
	w.AddWorkspace(t, access.Record{Slug: "other", OwnerID: w.stranger, OrgID: beta, Kind: access.KindRepository, Status: access.StatusReady})
	w.repo = w.workspaces.RepoPath("demo")
	gitrepotest.Git(t, w.repo, "init", "--quiet", "--bare", "--initial-branch=main", w.repo)
	stream := "commit refs/heads/main\nmark :1\ncommitter P <p@example.invalid> now\ndata 5\nfirst\nM 100644 inline README\ndata 2\n1\n" +
		"commit refs/heads/main\nmark :2\ncommitter P <p@example.invalid> now\ndata 6\nsecond\nfrom :1\nM 100644 inline README\ndata 2\n2\n"
	if _, err := gitrepotest.Run(w.repo, stream, "fast-import", "--quiet", "--date-format=now"); err != nil {
		t.Fatal(err)
	}
	head := gitrepotest.Refs(t, w.repo, "refs/heads/")["main"]
	rec := workspace.Workspace{
		Slug: "demo", Kind: access.KindRepository, Mode: workspace.ModeStandard,
		GitURL: new("https://origin.test/demo.git"), DefaultBranch: new("main"), OwnerID: w.owner, OrgID: w.acme,
		Status: workspace.StatusReady, DisplayName: "demo", HeadSHA: &head,
		CreatedAt: w.clock(), UpdatedAt: w.clock(), InstanceID: workspace.NewInstanceID(),
	}
	if err := w.Store.Tx(context.Background(), func(tx *sql.Tx) error { return workspace.Insert(context.Background(), tx, rec) }); err != nil {
		t.Fatal(err)
	}
	return w
}

// clock is the hub's time.
func (w *world) clock() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.now
}

// advance moves the hub's time on by d.
func (w *world) advance(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.now = w.now.Add(d)
}

// pat stores a PAT of user holding perms, bound to the workspace bound
// unless it is "", and returns the token and its id.
func (w *world) pat(t *testing.T, user, bound string, perms ...string) (token, id string) {
	t.Helper()
	return w.IssuePATID(t, identity.PATSpec{UserID: user, Permissions: perms, Workspace: bound, CreatedAt: w.clock()})
}

// addFork adds the task fork demo--t1 of demo, which the member owns: ready,
// in acme, tracking main, with its own repository made from demo's.
func (w *world) addFork(t *testing.T) {
	t.Helper()
	ctx, now, head := context.Background(), w.clock(), gitrepotest.Refs(t, w.repo, "refs/heads/")["main"]
	f := fork.Fork{
		Workspace: workspace.Workspace{
			Slug: "demo--t1", Kind: access.KindFork, Mode: workspace.ModeStandard, DefaultBranch: new("main"),
			OwnerID: w.member, OrgID: w.acme, Status: workspace.StatusReady, DisplayName: "demo--t1",
			ParentSlug: new("demo"), ForkPoint: &workspace.ForkPoint{Ref: "main", SHA: head},
			ExpiresAt: new(now.Add(7 * 24 * time.Hour)), AutoChange: new(false),
			CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(),
		},
		TrackingBranches: []string{"main"},
	}
	if err := (gitrepo.Git{}).CreateFork(ctx, w.repo, w.workspaces.RepoPath(f.Slug)); err != nil {
		t.Fatal(err)
	}
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error { return fork.Insert(ctx, tx, f) }); err != nil {
		t.Fatal(err)
	}
}

// setStatus sets the status of the workspace slug in the store.
func (w *world) setStatus(t *testing.T, slug, status string) {
	t.Helper()
	w.set(t, slug, "status", status)
}

// setMarker sets the maintenance marker of the workspace slug in the store,
// as a running reclone, archive or delete holds it; "" releases it.
func (w *world) setMarker(t *testing.T, slug string, m workspace.Marker) {
	t.Helper()
	w.set(t, slug, "maintenance", string(m))
}

// set sets column of the workspace slug to value in the store.
func (w *world) set(t *testing.T, slug, column, value string) {
	t.Helper()
	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE workspaces SET `+column+` = ? WHERE slug = ?`, value, slug)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// gitRequest is one of the four requests git sends: ref discovery for a
// fetch, the fetch exchange, ref discovery for a push and the push
// exchange.
type gitRequest struct {
	name, method, suffix string
	push                 bool
}

// gitRequests are the four requests, the three routes among them.
var gitRequests = []gitRequest{
	{"fetch refs", http.MethodGet, "/info/refs?service=git-upload-pack", false},
	{"fetch", http.MethodPost, "/git-upload-pack", false},
	{"push refs", http.MethodGet, "/info/refs?service=git-receive-pack", true},
	{"push", http.MethodPost, "/git-receive-pack", true},
}

// to is the path of the request for the repository segment pair repo, such
// as "acme/demo.git".
func (g gitRequest) to(repo string) string { return "/git/" + repo + g.suffix }

// answer is a response of the hub, its body read.
type answer struct {
	status int
	header http.Header
	body   string
}

// dump is the answer as the bytes that identify it: the status, every
// header but Date, sorted, and the body.
func (a answer) dump() string {
	var b strings.Builder
	b.WriteString(http.StatusText(a.status) + "\n")
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range a.header {
			if k != "Date" && !yield(k) {
				return
			}
		}
	}) {
		b.WriteString(k + ": " + strings.Join(a.header[k], ",") + "\n")
	}
	return b.String() + a.body
}

// call sends method path to the hub with the Authorization header auth (""
// for none), a fixed X-Request-ID, which the hub echoes, so that two answers
// can be compared byte for byte, and no body.
func (w *world) call(t *testing.T, method, path, auth string) answer {
	t.Helper()
	req, err := http.NewRequest(method, w.URL(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("X-Request-ID", fixedRequest)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("%s %s: read the body: %v", method, path, err)
	}
	return answer{res.StatusCode, res.Header, string(body)}
}

// basic is the Authorization header of HTTP Basic with user and password.
func basic(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

// recorder is the git server's service in the tests: it records the
// exchange of every request that passed the checks and answers 200 with
// nothing.
type recorder struct {
	mu        sync.Mutex
	exchanges []gitserver.Exchange
}

func (r *recorder) Serve(_ http.ResponseWriter, _ *http.Request, x gitserver.Exchange) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exchanges = append(r.exchanges, x)
}

// served is the number of requests that reached the service.
func (r *recorder) served() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.exchanges)
}

// last is the exchange of the last request that reached the service.
func (r *recorder) last(t *testing.T) gitserver.Exchange {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.exchanges) == 0 {
		t.Fatal("no request reached the service")
	}
	return r.exchanges[len(r.exchanges)-1]
}
