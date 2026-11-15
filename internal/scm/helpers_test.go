package scm_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	w := &world{observed: &observations{}, id: map[string]string{}}
	w.Hub = hubtest.StartWith(t, hubtest.Config{Compose: func(c *hub.Composition) {
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

// addWorkspace adds the ready workspace slug of the owner in org, its
// repository made by stream, and returns the repository's git directory.
func (w *world) addWorkspace(t *testing.T, slug, org, stream string) string {
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
func (w *world) addFork(t *testing.T, parent, org string, branches, tags []string) {
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
func (w *world) addHistory(t *testing.T) map[string]string {
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

// addTree adds the ready workspace tree of the owner and returns its git
// directory. Its main holds README.md (treeReadme), the executable run.sh,
// the symlink link to README.md, docs/a.md, docs/api/b.md and the submodule
// entry vendor/sub; its branch big holds 1,500 files at its root, f0000.txt
// to f1499.txt.
func (w *world) addTree(t *testing.T) string {
	t.Helper()
	data := func(s string) string { return "data " + strconv.Itoa(len(s)) + "\n" + s + "\n" }
	var big strings.Builder
	big.WriteString("blob\nmark :9\n" + data("x") + "commit refs/heads/big\n" + testCommitter + data("big"))
	for i := range 1500 {
		fmt.Fprintf(&big, "M 100644 :9 f%04d.txt\n", i)
	}
	return w.addWorkspace(t, "tree", w.org, "commit refs/heads/main\n"+testCommitter+data("tree")+
		"M 100644 inline README.md\n"+data(treeReadme)+"M 100755 inline run.sh\n"+data("#!/bin/sh\necho run\n")+
		"M 120000 inline link\n"+data("README.md")+"M 100644 inline docs/a.md\n"+data("a\n")+
		"M 100644 inline docs/api/b.md\n"+data("b\n")+"M 160000 "+strings.Repeat("5", 40)+" vendor/sub\n\n"+
		big.String()+"\n")
}

// set sets column of the workspace slug to value in the store, such as its
// status or its maintenance marker.
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
// turn.
func (w *world) get(t *testing.T, path, credential string, header ...string) reply {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, w.URL("/api/v1"+path), nil)
	if err != nil {
		t.Fatal(err)
	}
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Add(header[i], header[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer res.Body.Close()
	r := reply{status: res.StatusCode, header: res.Header}
	if r.body, err = io.ReadAll(res.Body); err != nil {
		t.Fatalf("GET %s: read the body: %v", path, err)
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
