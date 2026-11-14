package fork_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// parentSlug is the repository workspace every world holds, the parent the
// fork tests fork.
const parentSlug = "demo"

// world is a hub the fork tests ask, on a clock that stands still unless a
// test moves it: the developer dev, who owns the ready repository workspace
// demo in the org acme, and mia, a member of acme who can only read it, each
// with an API key, and demo's repository at its place on disk, a bare
// repository whose default branch main has two commits, with the branch
// release one commit off main's first and the annotated tag v1 on main's
// first.
type world struct {
	*hubtest.Hub
	forks      *fork.Component
	workspaces *workspace.Service

	// dev and mia are the users' ids, devKey and miaKey their API keys,
	// acme the org's id.
	dev, mia       string
	devKey, miaKey string
	acme           string
	// parent is the git directory of demo's repository.
	parent string

	// mu guards now, the hub's time.
	mu  sync.Mutex
	now time.Time
}

// newWorld starts a hub with a clock in the middle of a second, the hub's
// own composition and compose run after it (nil for nothing), and builds
// the world in it.
func newWorld(t *testing.T, compose func(*hub.Composition)) *world {
	t.Helper()
	w := &world{now: time.Now().UTC().Truncate(time.Second).Add(time.Second / 2)}
	w.Hub = hubtest.StartWith(t, hubtest.Config{Now: w.clock, Compose: func(c *hub.Composition) {
		w.forks, w.workspaces = c.Forks(), c.Workspaces()
		if compose != nil {
			compose(c)
		}
	}})
	w.dev, w.mia = w.CreateUser(t, "dev"), w.CreateUser(t, "mia")
	w.devKey, w.miaKey = w.IssueKey(t, w.dev), w.IssueKey(t, w.mia)
	w.acme = w.CreateOrg(t, "acme")
	w.AddMember(t, w.acme, w.dev)
	w.AddMember(t, w.acme, w.mia)
	w.parent = w.workspaces.RepoPath(parentSlug)
	gitrepotest.Git(t, w.parent, "init", "--quiet", "--bare", "--initial-branch=main", w.parent)
	stream := "commit refs/heads/main\nmark :1\ncommitter P <p@example.invalid> now\ndata 5\nfirst\nM 100644 inline README\ndata 2\n1\n" +
		"commit refs/heads/main\nmark :2\ncommitter P <p@example.invalid> now\ndata 6\nsecond\nfrom :1\nM 100644 inline README\ndata 2\n2\n" +
		"commit refs/heads/release\nmark :3\ncommitter P <p@example.invalid> now\ndata 7\nrelease\nfrom :1\nM 100644 inline README\ndata 2\n3\n" +
		"tag v1\nfrom :1\ntagger P <p@example.invalid> now\ndata 2\nv1\n"
	if _, err := gitrepotest.Run(w.parent, stream, "fast-import", "--quiet", "--date-format=now"); err != nil {
		t.Fatal(err)
	}
	head := w.head(t, w.parent, "main")
	w.insert(t, func(tx *sql.Tx) error {
		return workspace.Insert(context.Background(), tx, workspace.Workspace{
			Slug: parentSlug, Kind: access.KindRepository, Mode: workspace.ModeStandard,
			GitURL: new("https://origin.test/demo.git"), DefaultBranch: new("main"), OwnerID: w.dev, OrgID: w.acme,
			Status: workspace.StatusReady, DisplayName: parentSlug, HeadSHA: &head,
			CreatedAt: w.clock(), UpdatedAt: w.clock(), InstanceID: workspace.NewInstanceID(),
		})
	})
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

// insert runs fn in a transaction of the hub's store and fails the test on
// its error.
func (w *world) insert(t *testing.T, fn func(*sql.Tx) error) {
	t.Helper()
	if err := w.Store.Tx(context.Background(), fn); err != nil {
		t.Fatal(err)
	}
}

// seedFork stores a ready fork of demo named slug, as a create would, with
// the fork's repository at its place: owned by dev in acme, taken from main
// at main's head, tracking every branch and tag demo has now, created at the
// hub's time and expiring a week later. edit, when given, changes the record
// before it is stored. It returns the stored record.
func (w *world) seedFork(t *testing.T, slug string, edit ...func(*fork.Fork)) fork.Fork {
	t.Helper()
	now := w.clock()
	f := fork.Fork{
		Workspace: workspace.Workspace{
			Slug: slug, Kind: access.KindFork, Mode: workspace.ModeStandard, DefaultBranch: new("main"),
			OwnerID: w.dev, OrgID: w.acme, Status: workspace.StatusReady, DisplayName: slug,
			ParentSlug: new(parentSlug), ForkPoint: &workspace.ForkPoint{Ref: "main", SHA: w.head(t, w.parent, "main")},
			ExpiresAt: new(now.Add(7 * 24 * time.Hour)), AutoChange: new(false),
			CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(),
		},
		TrackingBranches: slices.Sorted(maps.Keys(gitrepotest.Refs(t, w.parent, "refs/heads/"))),
		TrackingTags:     slices.Sorted(maps.Keys(gitrepotest.Refs(t, w.parent, "refs/tags/"))),
	}
	for _, fn := range edit {
		fn(&f)
	}
	if err := (gitrepo.Git{}).CreateFork(context.Background(), w.parent, w.workspaces.RepoPath(slug)); err != nil {
		t.Fatal(err)
	}
	w.insert(t, func(tx *sql.Tx) error { return fork.Insert(context.Background(), tx, f) })
	return f
}

// git runs git on the repository of the workspace slug and returns its
// trimmed output.
func (w *world) git(t *testing.T, slug string, args ...string) string {
	t.Helper()
	return gitrepotest.Git(t, w.workspaces.RepoPath(slug), args...)
}

// head is the commit branch points at in the repository dir, "" when it has
// no such branch.
func (w *world) head(t *testing.T, dir, branch string) string {
	t.Helper()
	return gitrepotest.Refs(t, dir, "refs/heads/")[branch]
}

// commit adds a commit on top of from (a commit id or a ref) to branch of
// the repository of the workspace slug, creating or moving the branch, and
// returns its id. The commit's objects are written into that repository.
func (w *world) commit(t *testing.T, slug, branch, from string) string {
	t.Helper()
	dir := w.workspaces.RepoPath(slug)
	msg := fmt.Sprintf("on %s %d", branch, time.Now().UnixNano())
	stream := fmt.Sprintf("commit refs/heads/%s\ncommitter T <t@example.invalid> now\ndata %d\n%s\nfrom %s\nM 100644 inline README\ndata %d\n%s\n",
		branch, len(msg), msg, from, len(msg), msg)
	if _, err := gitrepotest.Run(dir, stream, "fast-import", "--quiet", "--force", "--date-format=now"); err != nil {
		t.Fatal(err)
	}
	return w.head(t, dir, branch)
}

// refs is the ref view of the fork slug, failing the test on an error.
func (w *world) refs(t *testing.T, slug string) fork.RefView {
	t.Helper()
	v, err := w.forks.Refs(context.Background(), slug)
	if err != nil {
		t.Fatalf("Refs(%s): %v", slug, err)
	}
	return v
}

// answer is the status and the body of a hub's answer.
type answer struct {
	status int
	body   []byte
}

// object decodes a, which must have status want, as a JSON object.
func (a answer) object(t *testing.T, want int) map[string]any {
	t.Helper()
	var m map[string]any
	if a.status != want || json.Unmarshal(a.body, &m) != nil {
		t.Fatalf("answer = %d %s, want %d with a JSON object", a.status, a.body, want)
	}
	return m
}

// call sends method path, under /api/v1, to the hub with cred as the bearer
// credential and body (nil for none; see hubtest.Served.Call).
func (w *world) call(t *testing.T, method, path, cred string, body any) answer {
	t.Helper()
	status, b := w.Call(t, method, "/api/v1"+path, cred, body)
	return answer{status, b}
}

// get asks GET path with cred.
func (w *world) get(t *testing.T, cred, path string) answer {
	t.Helper()
	return w.call(t, http.MethodGet, path, cred, nil)
}

// post sends POST path with cred and body.
func (w *world) post(t *testing.T, cred, path string, body any) answer {
	t.Helper()
	return w.call(t, http.MethodPost, path, cred, body)
}

// patch sends PATCH path with cred and body.
func (w *world) patch(t *testing.T, cred, path string, body any) answer {
	t.Helper()
	return w.call(t, http.MethodPatch, path, cred, body)
}
