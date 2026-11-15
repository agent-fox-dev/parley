package scm_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/scm"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// gitArgs records the arguments of every git process a repository runs.
type gitArgs struct {
	mu   sync.Mutex
	args [][]string
}

func (g *gitArgs) trace(args []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.args = append(g.args, args)
}

// reset forgets what was recorded and returns it.
func (g *gitArgs) reset() [][]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	seen := g.args
	g.args = nil
	return seen
}

// revisionRepo is a workspace repository whose refs name commits in every
// way a revision can: c1, c2 and c3 on main, f.txt different in each;
// feature/x with x on top of c2; trunk at c1; the lightweight tag v1 at c1
// and the annotated tag v2 at c3; refs/parley/changes/12/3 at x; a branch
// and a tag both named dup; a branch named refs/tags/v1 at c2; the commit
// gone, holding only-on-gone.txt, that no ref reaches; and two commits whose
// ids share the four characters prefix. id maps each of these names, blob
// (f.txt at c1) and v2tag (the tag object) to its object id.
type revisionRepo struct {
	repo   *gitrepo.Repo
	git    *gitArgs
	id     map[string]string
	prefix string
}

func newRevisionRepo(t *testing.T) *revisionRepo {
	t.Helper()
	dir := t.TempDir()
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", dir)
	const who = "committer T <t@example.invalid> 1700000000 +0000\n"
	commit := func(ref, msg, from, file string) string {
		s := "commit " + ref + "\nmark :" + msg[1:] + "\n" + who + "data 2\n" + msg + "\n"
		if from != "" {
			s += "from " + from + "\n"
		}
		return s + "M 100644 inline " + file + "\ndata 3\n" + msg + "\n\n"
	}
	stream := commit("refs/heads/main", "c1", "", "f.txt") +
		commit("refs/heads/main", "c2", "", "f.txt") +
		commit("refs/heads/main", "c3", "", "f.txt") +
		commit("refs/heads/feature/x", "c4", ":2", "x.txt") +
		commit("refs/heads/gone", "c5", ":1", "only-on-gone.txt") +
		"reset refs/heads/trunk\nfrom :1\n\nreset refs/tags/v1\nfrom :1\n\n" +
		"reset refs/parley/changes/12/3\nfrom :4\n\nreset refs/heads/dup\nfrom :1\n\nreset refs/tags/dup\nfrom :2\n\n" +
		"reset refs/heads/refs/tags/v1\nfrom :2\n\n" +
		"tag v2\nfrom :3\ntagger T <t@example.invalid> 1700000000 +0000\ndata 2\nv2\n"
	if _, err := gitrepotest.Run(dir, stream, "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	w := &revisionRepo{git: &gitArgs{}, id: map[string]string{}, prefix: gitrepotest.SharedPrefix(t, dir)}
	names := []string{"c1", "c2", "c3", "x", "gone", "blob", "v2tag"}
	ids := strings.Fields(gitrepotest.Git(t, dir, "rev-parse", "main~2", "main~1", "main", "feature/x", "gone", "main~2:f.txt", "refs/tags/v2"))
	for i, name := range names {
		w.id[name] = ids[i]
	}
	gitrepotest.Git(t, dir, "update-ref", "-d", "refs/heads/gone")
	repo, err := gitrepo.Git{Trace: w.git.trace}.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	w.repo = repo
	w.git.reset()
	return w
}

// refList is a RefView that answers a fixed list of refs.
type refList []scm.Ref

func (l refList) Refs(context.Context) ([]scm.Ref, error) { return l, nil }

// world is a hub whose scm routes the tests call: the org acme with its
// members owner and member, and outsider, who belongs to no org, each
// with an API key; PATs of the owner holding git:read, git:write alone and
// changes:read alone. The owner's workspace demo is ready, with the task
// fork demo--t1 of it, which the member owns, tracking main and the tags
// v1 and v2. The owner's workspace old is archived, with the task fork
// old--t1 of it made while it was ready. The hub's scm routes report to
// observed and count the repositories they open in opens.
type world struct {
	*hubtest.Hub
	workspaces *workspace.Service
	events     *event.Component
	observed   *observations
	opens      atomic.Int64

	owner, member, org               string
	ownerKey, memberKey, outsiderKey string
	readPAT, writePAT, changesPAT    string
	// repo is the git directory of demo's repository, and id the ids of
	// its commits c1, c2 and c3 and of the tag object v2tag (demoStream).
	repo string
	id   map[string]string
}

// demoStream is demo's history: c1 and c2 on main, each writing f.txt;
// c3 on feature/x, on top of c2, adding x.txt; the branches dev and
// release at c1 and topic at c2; the lightweight tag v1 at c1 and the
// annotated tag v2 at c2; refs/parley/changes/12/3 at c3; and refs/notes/x,
// a namespace the reads do not list, at c1.
const demoStream = "commit refs/heads/main\nmark :1\n" + testCommitter + "data 2\nc1\nM 100644 inline f.txt\ndata 3\nc1\n\n" +
	"commit refs/heads/main\nmark :2\n" + testCommitter + "data 2\nc2\nfrom :1\nM 100644 inline f.txt\ndata 3\nc2\n\n" +
	"commit refs/heads/feature/x\nmark :3\n" + testCommitter + "data 2\nc3\nfrom :2\nM 100644 inline x.txt\ndata 3\nc3\n\n" +
	"reset refs/heads/dev\nfrom :1\n\nreset refs/heads/release\nfrom :1\n\nreset refs/heads/topic\nfrom :2\n\n" +
	"reset refs/tags/v1\nfrom :1\n\ntag v2\nfrom :2\ntagger T <t@example.invalid> 1700000000 +0000\ndata 2\nv2\n" +
	"reset refs/parley/changes/12/3\nfrom :3\n\nreset refs/notes/x\nfrom :1\n\n"

const testCommitter = "committer T <t@example.invalid> 1700000000 +0000\n"

// newWorld starts the world's hub, with adjust changing the scm routes'
// dependencies after the world's own changes.
func newWorld(t *testing.T, adjust ...func(*scm.Deps)) *world {
	t.Helper()
	return newConfiguredWorld(t, "", adjust...)
}

// newConfiguredWorld is newWorld with toml added to the hub's config.toml.
func newConfiguredWorld(t testing.TB, toml string, adjust ...func(*scm.Deps)) *world {
	t.Helper()
	return newComposedWorld(t, toml, nil, adjust...)
}

// newComposedWorld is newConfiguredWorld with compose, when it is not nil,
// run on the hub's composition after the world's own changes, such as to
// add a route or register a collaborator.
func newComposedWorld(t testing.TB, toml string, compose func(*hub.Composition), adjust ...func(*scm.Deps)) *world {
	t.Helper()
	w := &world{observed: &observations{}, id: map[string]string{}}
	w.Hub = hubtest.StartWith(t, hubtest.Config{TOML: toml, Compose: func(c *hub.Composition) {
		w.workspaces, w.events = c.Workspaces(), c.Events()
		c.AdjustSCM(func(d *scm.Deps) {
			open := d.Open
			d.Open = func(ctx context.Context, slug string) (scm.Repository, error) {
				w.opens.Add(1)
				return open(ctx, slug)
			}
			d.Recorder = w.observed
			for _, a := range adjust {
				a(d)
			}
		})
		if compose != nil {
			compose(c)
		}
	}})
	now := time.Now().UTC()
	w.owner, w.member = w.CreateUser(t, "owner"), w.CreateUser(t, "member")
	w.ownerKey, w.memberKey, w.outsiderKey = w.IssueKey(t, w.owner), w.IssueKey(t, w.member), w.IssueKey(t, w.CreateUser(t, "outsider"))
	pat := func(perm string) string {
		return w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: []string{perm}, CreatedAt: now})
	}
	w.readPAT, w.writePAT, w.changesPAT = pat("git:read"), pat("git:write"), pat("changes:read")
	acme := w.CreateOrg(t, "acme")
	w.org = acme
	w.AddMember(t, acme, w.owner)
	w.AddMember(t, acme, w.member)

	w.repo = w.addWorkspace(t, "demo", acme, demoStream)
	ids := strings.Fields(gitrepotest.Git(t, w.repo, "rev-parse", "main~1", "main", "feature/x", "refs/tags/v2"))
	for i, name := range []string{"c1", "c2", "c3", "v2tag"} {
		w.id[name] = ids[i]
	}
	w.addFork(t, "demo", acme, []string{"main"}, []string{"v1", "v2"})
	w.addWorkspace(t, "old", acme, "commit refs/heads/main\n"+testCommitter+"data 2\nc1\nM 100644 inline f.txt\ndata 3\nc1\n\n")
	w.addFork(t, "old", acme, []string{"main"}, nil)
	w.set(t, "old", "status", string(workspace.StatusArchived))
	return w
}

// readWorld is the world the tests that only read share, built once for the
// package: a world as newWorld makes it, its rate limit off since every
// test that reads sends with the same keys, with the owner's workspaces hist,
// tree, files, diff, cmp, blame and search added. hist, diff and blame are
// the ids addHistory, addDiff and addBlame return; tree, files, cmp and
// search the git directories of those workspaces, files a tree workspace
// holding fileExtras too; and searchMain the id of search's main. Its tests
// change nothing in it and read neither its observations nor its opens.
type readWorld struct {
	*world
	hist, diff, blame        map[string]string
	tree, files, cmp, search string
	searchMain               string
}

// readOnly is the package's readWorld, and the testing.TB it lives on.
var readOnly struct {
	once    sync.Once
	tb      packageTB
	w       *readWorld
	failure any
}

// readOnlyWorld is the package's readWorld, built by the first test that
// asks for it; a failure to build it fails every test that asks.
func readOnlyWorld(t *testing.T) *readWorld {
	t.Helper()
	readOnly.once.Do(func() {
		defer func() { readOnly.failure = recover() }()
		tb := &readOnly.tb
		w := &readWorld{world: newConfiguredWorld(tb, "[limits]\nrequests_per_minute = 0\n")}
		w.hist, w.diff, w.blame = w.addHistory(tb), w.addDiff(tb), w.addBlame(tb)
		w.tree, w.files, w.cmp = w.addTree(tb, "tree"), w.addTree(tb, "files", fileExtras...), w.addCompare(tb)
		w.search, w.searchMain = w.addSearch(tb)
		readOnly.w = w
	})
	if readOnly.failure != nil {
		t.Fatalf("the shared world: %v", readOnly.failure)
	}
	return readOnly.w
}

// TestMain runs the package's tests, then stops the shared world.
func TestMain(m *testing.M) {
	code := m.Run()
	readOnly.tb.cleanup()
	os.Exit(code)
}

// packageTB is the testing.TB of what lasts as long as the package's tests:
// its cleanups run when TestMain calls cleanup, and a failure panics, which
// readOnlyWorld turns into the failure of the test that asked. hubtest and
// the world's helpers call only the methods it defines.
type packageTB struct {
	testing.TB
	mu       sync.Mutex
	cleanups []func()
}

func (*packageTB) Helper() {}

func (p *packageTB) Cleanup(f func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cleanups = append(p.cleanups, f)
}

func (p *packageTB) TempDir() string {
	dir, err := os.MkdirTemp("", "scm-test-")
	if err != nil {
		p.Fatal(err)
	}
	p.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func (*packageTB) Fatal(args ...any) { panic(fmt.Sprint(args...)) }

func (*packageTB) Fatalf(format string, args ...any) { panic(fmt.Sprintf(format, args...)) }

// cleanup runs the cleanups, the last registered first, as a test does.
func (p *packageTB) cleanup() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range slices.Backward(p.cleanups) {
		f()
	}
	p.cleanups = nil
}

// addWorkspace adds the ready workspace slug of the owner in org, its
// repository made by stream, and returns the repository's git directory.
func (w *world) addWorkspace(t testing.TB, slug, org, stream string) string {
	t.Helper()
	ctx, now, dir := context.Background(), time.Now().UTC(), w.workspaces.RepoPath(slug)
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", dir)
	if _, err := gitrepotest.Run(dir, stream, "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	head := gitrepotest.Refs(t, dir, "refs/heads/")["main"]
	rec := workspace.Workspace{
		Slug: slug, Kind: access.KindRepository, Mode: workspace.ModeStandard,
		GitURL: new("https://origin.test/" + slug + ".git"), DefaultBranch: new("main"), OwnerID: w.owner, OrgID: org,
		Status: workspace.StatusReady, DisplayName: slug, HeadSHA: &head,
		CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(),
	}
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error { return workspace.Insert(ctx, tx, rec) }); err != nil {
		t.Fatal(err)
	}
	return dir
}

// addFork adds the ready task fork parent--t1 of parent, which the member
// owns, tracking branches and tags, its repository borrowing the parent's
// objects.
func (w *world) addFork(t testing.TB, parent, org string, branches, tags []string) {
	t.Helper()
	ctx, now, slug := context.Background(), time.Now().UTC(), parent+"--t1"
	head := gitrepotest.Refs(t, w.workspaces.RepoPath(parent), "refs/heads/")["main"]
	f := fork.Fork{
		Workspace: workspace.Workspace{
			Slug: slug, Kind: access.KindFork, Mode: workspace.ModeStandard, DefaultBranch: new("main"),
			OwnerID: w.member, OrgID: org, Status: workspace.StatusReady, DisplayName: slug,
			ParentSlug: new(parent), ForkPoint: &workspace.ForkPoint{Ref: "main", SHA: head},
			ExpiresAt: new(now.Add(7 * 24 * time.Hour)), AutoChange: new(false),
			CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(),
		},
		TrackingBranches: branches, TrackingTags: tags,
	}
	if err := (gitrepo.Git{}).CreateFork(ctx, w.workspaces.RepoPath(parent), w.workspaces.RepoPath(slug)); err != nil {
		t.Fatal(err)
	}
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error { return fork.Insert(ctx, tx, f) }); err != nil {
		t.Fatal(err)
	}
}

// The committer times of the history workspace's commits, as the reads
// answer them.
const (
	histT1 = "2026-01-01T00:00:00Z"
	histT2 = "2026-01-02T03:04:05Z"
	histT3 = "2026-01-03T00:00:00Z"
	histT4 = "2026-01-04T00:00:00Z"
)

// histMessage is the message of the history workspace's commit c2.
const histMessage = "Second\n\nBody one.\nBody two.\n"

// addHistory adds the ready workspace hist of the owner and returns the ids
// of its commits. Its main is c1 <- c2 <- m: c1, by Alice at histT1, adds
// old.md and docs/a.md; c2, by Alice at histT2 with histMessage, changes
// docs/a.md (two lines in, one out) and adds docs/b.md (three lines); m, by
// Bob at histT4, merges side, whose commit s1, by Bob at histT3 on top of
// c2, adds side.txt. The branch docs has r, by Bob <bob@example.org> at
// histT3 on top of c2, which renames old.md to new.md.
func (w *world) addHistory(t testing.TB) map[string]string {
	t.Helper()
	data := func(s string) string { return "data " + strconv.Itoa(len(s)) + "\n" + s + "\n" }
	who := func(name, email, at string) string {
		when, err := time.Parse(time.RFC3339, at)
		if err != nil {
			t.Fatal(err)
		}
		sig := name + " <" + email + "> " + strconv.FormatInt(when.Unix(), 10) + " +0000\n"
		return "author " + sig + "committer " + sig
	}
	alice, bob := func(at string) string { return who("Alice", "alice@example.com", at) }, func(at string) string { return who("Bob", "bob@example.org", at) }
	dir := w.addWorkspace(t, "hist", w.org, "commit refs/heads/main\nmark :1\n"+alice(histT1)+data("c1")+
		"M 100644 inline old.md\n"+data("one\n")+"M 100644 inline docs/a.md\n"+data("a\n")+
		"\ncommit refs/heads/main\nmark :2\n"+alice(histT2)+data(histMessage)+
		"M 100644 inline docs/a.md\n"+data("a2\nmore\n")+"M 100644 inline docs/b.md\n"+data("x\ny\nz\n")+
		"\ncommit refs/heads/side\nmark :3\n"+bob(histT3)+data("s1")+"from :2\nM 100644 inline side.txt\n"+data("side\n")+
		"\ncommit refs/heads/main\nmark :4\n"+bob(histT4)+data("m")+"from :2\nmerge :3\nM 100644 inline side.txt\n"+data("side\n")+
		"\ncommit refs/heads/docs\n"+bob(histT3)+data("rename")+"from :2\nD old.md\nM 100644 inline new.md\n"+data("one\n")+"\n")
	ids := map[string]string{}
	for i, id := range strings.Fields(gitrepotest.Git(t, dir, "rev-parse", "main~2", "main~1", "side", "main", "docs")) {
		ids[[]string{"c1", "c2", "s1", "m", "r"}[i]] = id
	}
	return ids
}

// treeReadme is the content of README.md on the tree workspace's main.
const treeReadme = "# Tree\n\nA fixture.\n"

// addTree adds the ready workspace slug of the owner and returns its git
// directory. Its main holds README.md (treeReadme), the executable run.sh,
// the symlink link to README.md, docs/a.md, docs/api/b.md, the submodule
// entry vendor/sub and the files of files, given as path and content in
// turn; its branch big holds 1,500 files at its root, f0000.txt to
// f1499.txt.
func (w *world) addTree(t testing.TB, slug string, files ...string) string {
	t.Helper()
	data := func(s string) string { return "data " + strconv.Itoa(len(s)) + "\n" + s + "\n" }
	var more strings.Builder
	for i := 0; i+1 < len(files); i += 2 {
		more.WriteString("M 100644 inline " + files[i] + "\n" + data(files[i+1]))
	}
	var big strings.Builder
	big.WriteString("blob\nmark :9\n" + data("x") + "commit refs/heads/big\n" + testCommitter + data("big"))
	for i := range 1500 {
		fmt.Fprintf(&big, "M 100644 :9 f%04d.txt\n", i)
	}
	return w.addWorkspace(t, slug, w.org, "commit refs/heads/main\n"+testCommitter+data("tree")+
		"M 100644 inline README.md\n"+data(treeReadme)+"M 100755 inline run.sh\n"+data("#!/bin/sh\necho run\n")+
		"M 120000 inline link\n"+data("README.md")+"M 100644 inline docs/a.md\n"+data("a\n")+
		"M 100644 inline docs/api/b.md\n"+data("b\n")+"M 160000 "+strings.Repeat("5", 40)+" vendor/sub\n"+more.String()+"\n"+
		big.String()+"\n")
}

// addDiff adds the ready workspace diff of the owner and returns the ids of
// its commits mb, main, feature and orphan. Its main is mb <- main, which
// adds only-main.txt. mb holds a.txt (ten lines), edit.txt, gone.txt, the
// binary img.bin and the regular file kind. feature, one commit on top of
// mb, adds new.txt (three lines), deletes gone.txt, replaces the line three
// of edit.txt with two lines, moves a.txt to moved/b.txt changing its last
// line, changes img.bin, turns kind into a symlink and adds docs/guide.md,
// docs/api/x.txt, notes.md and src/readme.md. orphan is a root commit of
// its own adding o1.txt and o2.txt.
func (w *world) addDiff(t testing.TB) map[string]string {
	t.Helper()
	data := func(s string) string { return "data " + strconv.Itoa(len(s)) + "\n" + s + "\n" }
	file := func(path, content string) string { return "M 100644 inline " + path + "\n" + data(content) }
	var alpha strings.Builder
	for i := 1; i <= 9; i++ {
		fmt.Fprintf(&alpha, "alpha line %d\n", i)
	}
	dir := w.addWorkspace(t, "diff", w.org, "commit refs/heads/main\nmark :1\n"+testCommitter+data("mb")+
		file("a.txt", alpha.String()+"alpha line 10\n")+file("edit.txt", "one\ntwo\nthree\nfour\nfive\n")+
		file("gone.txt", "gone\n")+file("img.bin", "\x00\x01\x02")+file("kind", "kind\n")+
		"\ncommit refs/heads/main\n"+testCommitter+data("main")+"from :1\n"+file("only-main.txt", "main\n")+
		"\ncommit refs/heads/feature\n"+testCommitter+data("feature")+"from :1\n"+
		file("new.txt", "n1\nn2\nn3\n")+"D gone.txt\n"+file("edit.txt", "one\ntwo\n3a\n3b\nfour\nfive\n")+
		"D a.txt\n"+file("moved/b.txt", alpha.String()+"alpha line ten\n")+file("img.bin", "\x00\x03")+
		"M 120000 inline kind\n"+data("edit.txt")+file("docs/guide.md", "guide\n")+file("docs/api/x.txt", "x\n")+
		file("notes.md", "notes\n")+file("src/readme.md", "readme\n")+
		"\ncommit refs/heads/orphan\n"+testCommitter+data("orphan")+file("o1.txt", "o1\n")+file("o2.txt", "o2\n")+"\n")
	ids := map[string]string{}
	for i, id := range strings.Fields(gitrepotest.Git(t, dir, "rev-parse", "main~1", "main", "feature", "orphan")) {
		ids[[]string{"mb", "main", "feature", "orphan"}[i]] = id
	}
	return ids
}

// addCompare adds the ready workspace cmp of the owner, its repository made
// from gitrepotest.CompareStream, and returns its git directory.
func (w *world) addCompare(t testing.TB) string {
	t.Helper()
	return w.addWorkspace(t, "cmp", w.org, gitrepotest.CompareStream())
}

// blameEdge is a text file of exactly the 2 MiB a blame takes, in lines of
// 64 bytes.
var blameEdge = strings.Repeat(strings.Repeat("e", 63)+"\n", 32768)

// addBlame adds the ready workspace blame of the owner, its main the four
// commits of gitrepotest.BlameStream and on top of them a fifth adding the
// binary file bin.dat, edge.txt (blameEdge), big.txt (blameEdge and one more
// byte), docs/a.md and the submodule entry sub, and returns the ids of c1 to
// c4 and of main.
func (w *world) addBlame(t testing.TB) map[string]string {
	t.Helper()
	data := func(s string) string { return "data " + strconv.Itoa(len(s)) + "\n" + s + "\n" }
	file := func(path, content string) string { return "M 100644 inline " + path + "\n" + data(content) }
	dir := w.addWorkspace(t, "blame", w.org, gitrepotest.BlameStream()+"commit refs/heads/main\n"+testCommitter+data("more")+
		file("bin.dat", "bin\x00ary\n")+file("edge.txt", blameEdge)+file("big.txt", blameEdge+"b")+file("docs/a.md", "a\n")+
		"M 160000 "+strings.Repeat("5", 40)+" sub\n\n")
	ids := map[string]string{}
	for i, id := range strings.Fields(gitrepotest.Git(t, dir, "rev-parse", "main~4", "main~3", "main~2", "main~1", "main")) {
		ids[[]string{"c1", "c2", "c3", "c4", "main"}[i]] = id
	}
	return ids
}

// addSearch adds the ready workspace search of the owner, its repository
// made from gitrepotest.SearchStream, and returns its git directory and the
// id of its main.
func (w *world) addSearch(t testing.TB) (dir, main string) {
	t.Helper()
	dir = w.addWorkspace(t, "search", w.org, gitrepotest.SearchStream())
	return dir, strings.TrimSpace(gitrepotest.Git(t, dir, "rev-parse", "main"))
}

// set sets column of the workspace slug to value in the store, such as its
// status or its maintenance marker.
func (w *world) set(t testing.TB, slug, column, value string) {
	t.Helper()
	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE workspaces SET `+column+` = ? WHERE slug = ?`, value, slug)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// commit writes to dir a commit on top of parent that sets file to content,
// reached by no ref, and returns its id.
func commit(t *testing.T, dir, parent, file, content string) string {
	t.Helper()
	const tmp = "refs/scm-test/new"
	stream := "commit " + tmp + "\n" + testCommitter + "data 4\nnext\nfrom " + parent + "\nM 100644 inline " + file + "\n" +
		"data " + strconv.Itoa(len(content)) + "\n" + content + "\n"
	if _, err := gitrepotest.Run(dir, stream, "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(gitrepotest.Git(t, dir, "rev-parse", tmp))
	gitrepotest.Git(t, dir, "update-ref", "-d", tmp)
	return id
}

// reply is an answer of the hub, its body read and, when it is a JSON
// object, decoded.
type reply struct {
	status int
	header http.Header
	body   []byte
	json   map[string]any
}

// errorType is the error_type of an error envelope, "" for any other body.
func (r reply) errorType() string {
	e, _ := r.json["error"].(map[string]any)
	s, _ := e["error_type"].(string)
	return s
}

// names are the name members of the items of a list.
func (r reply) names() []string { return r.members("name") }

// shas are the sha members of the items of a list.
func (r reply) shas() []string { return r.members("sha") }

// members are the string members key of the items of a list.
func (r reply) members(key string) []string {
	items, _ := r.json["items"].([]any)
	values := []string{}
	for _, it := range items {
		o, _ := it.(map[string]any)
		s, _ := o[key].(string)
		values = append(values, s)
	}
	return values
}

// get sends GET path, below /api/v1, with credential as the bearer
// credential ("" for none) and the headers header names and values in
// turn, a header whose value is "" left out.
func (w *world) get(t *testing.T, path, credential string, header ...string) reply {
	t.Helper()
	return w.send(t, http.MethodGet, path, credential, header...)
}

// send is get with the method method.
func (w *world) send(t *testing.T, method, path, credential string, header ...string) reply {
	t.Helper()
	req, err := http.NewRequest(method, w.URL("/api/v1"+path), nil)
	if err != nil {
		t.Fatal(err)
	}
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	for i := 0; i+1 < len(header); i += 2 {
		if header[i+1] != "" {
			req.Header.Add(header[i], header[i+1])
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	r := reply{status: res.StatusCode, header: res.Header}
	if r.body, err = io.ReadAll(res.Body); err != nil {
		t.Fatalf("%s %s: read the body: %v", method, path, err)
	}
	_ = json.Unmarshal(r.body, &r.json)
	return r
}

// scmPath is the path, below /api/v1, of the read route path, such as
// /refs, of the workspace slug.
func scmPath(slug, path string) string { return "/workspaces/" + slug + "/scm" + path }

// scmRoutes are the read routes: the op each reports, a path below /scm
// that it answers 200 on demo, and one that it answers with an error.
var scmRoutes = []struct{ op, ok, bad string }{
	{"refs", "/refs", "/refs?limit=0"},
	{"refs", "/refs/main", "/refs/nope"},
	{"commits", "/commits", "/commits?stats=yes"},
	{"commits", "/commits/main", "/commits/nope"},
	{"tree", "/tree", "/tree?path=nope"},
	{"file", "/file?path=f.txt", "/file?path=nope.txt"},
	{"diff", "/diff?base=main&head=feature/x", "/diff?base=main"},
	{"compare", "/compare?base=main&head=feature/x", "/compare?base=main"},
	{"blame", "/blame?path=f.txt", "/blame?path=nope.txt"},
	{"search", "/search?q=c1", "/search"},
}

// observations is a request recorder that keeps what it was told, each
// observation as its op and result joined by a space.
type observations struct {
	mu   sync.Mutex
	seen []string
}

func (o *observations) Observe(op, result string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, op+" "+result)
}

// reset forgets what was observed and returns it.
func (o *observations) reset() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	seen := o.seen
	o.seen = nil
	return seen
}
