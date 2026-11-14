package gitserver_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
// test sees what reached it, unless the world serves git itself
// (newGitWorld).
type world struct {
	*hubtest.Hub
	workspaces *workspace.Service
	git        *gitserver.Component
	service    *recorder
	runs       countingRunner

	owner, member, stranger          string
	ownerKey, memberKey, strangerKey string
	ownerPAT                         string
	acme                             string
	// repo is the git directory of demo's repository, and head the commit
	// its main is at.
	repo, head string
	// work is the head of the fork's own branch work (newForkWorld).
	work string

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
	return startWorld(t, cfg, &recorder{})
}

// newGitWorld is newWorld with the hub's own git exchange in place of the
// recorder, so that stock git can be served; w.runs counts its runs of
// git.
func newGitWorld(t *testing.T, cfg hubtest.Config) *world {
	t.Helper()
	return startWorld(t, cfg, nil)
}

// startWorld is newWorld with service, when not nil, in place of the
// hub's git exchange.
func startWorld(t *testing.T, cfg hubtest.Config, service *recorder) *world {
	t.Helper()
	w := &world{now: time.Now().UTC().Truncate(time.Second), service: service}
	compose := cfg.Compose
	cfg.Now, cfg.Compose = w.clock, func(c *hub.Composition) {
		w.workspaces, w.git = c.Workspaces(), c.GitServer()
		c.AdjustGitServer(func(d *gitserver.Deps) {
			if service != nil {
				d.Service = service
			}
			w.runs.Runner, d.Runner = d.Runner, &w.runs
		})
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
	w.head = head
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
	w.addForkTracking(t, []string{"main"}, nil)
}

// addForkTracking is addFork with the tracking branches branches and the
// tracking tags tags recorded on the fork.
func (w *world) addForkTracking(t *testing.T, branches, tags []string) {
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
		TrackingBranches: branches, TrackingTags: tags,
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
// exchange; label is the service the request counter counts it under.
type gitRequest struct {
	name, method, suffix string
	push                 bool
	label                string
}

// gitRequests are the four requests, the three routes among them.
var gitRequests = []gitRequest{
	{"fetch refs", http.MethodGet, "/info/refs?service=git-upload-pack", false, gitserver.LabelInfoRefs},
	{"fetch", http.MethodPost, "/git-upload-pack", false, gitserver.LabelUploadPack},
	{"push refs", http.MethodGet, "/info/refs?service=git-receive-pack", true, gitserver.LabelInfoRefs},
	{"push", http.MethodPost, "/git-receive-pack", true, gitserver.LabelReceivePack},
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
	for _, k := range slices.Sorted(maps.Keys(a.header)) {
		if k != "Date" {
			b.WriteString(k + ": " + strings.Join(a.header[k], ",") + "\n")
		}
	}
	return b.String() + a.body
}

// call sends method path to the hub with the Authorization header auth (""
// for none), a fixed X-Request-ID, which the hub echoes, so that two answers
// can be compared byte for byte, and no body.
func (w *world) call(t *testing.T, method, path, auth string) answer {
	t.Helper()
	return w.send(t, method, path, auth, nil, "")
}

// send is call with the headers header and the body body.
func (w *world) send(t *testing.T, method, path, auth string, header http.Header, body string) answer {
	t.Helper()
	req, err := http.NewRequest(method, w.URL(path), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
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
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("%s %s: read the body: %v", method, path, err)
	}
	return answer{res.StatusCode, res.Header, string(got)}
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

// countingRunner counts the runs of git of the runner it wraps, upload-pack
// and receive-pack alike.
type countingRunner struct {
	gitserver.Runner
	n atomic.Int64
}

func (c *countingRunner) UploadPack(ctx context.Context, req gitrepo.ServeRequest) error {
	c.n.Add(1)
	return c.Runner.UploadPack(ctx, req)
}

func (c *countingRunner) ReceivePack(ctx context.Context, req gitrepo.ServeRequest) error {
	c.n.Add(1)
	return c.Runner.ReceivePack(ctx, req)
}

// flushPkt stands for a flush packet among the pkt-lines of a body.
const flushPkt = "<flush>"

// pktlines are the pkt-lines body starts with, each payload as it is and a
// flush packet as flushPkt, up to the first bytes that are not a pkt-line,
// such as a pack sent without the side band.
func pktlines(body string) []string {
	var pkts []string
	for len(body) >= 4 {
		n, err := strconv.ParseUint(body[:4], 16, 16)
		switch {
		case err != nil || n == 1 || n == 2 || n == 3 || int(n) > len(body):
			return pkts
		case n == 0:
			pkts = append(pkts, flushPkt)
			body = body[4:]
		default:
			pkts = append(pkts, body[4:n])
			body = body[n:]
		}
	}
	return pkts
}

// pkt is s as one pkt-line.
func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// fetchRequest is a version 0 upload-pack request that wants want, after
// the flush packet sends the lines haves, and says done.
func fetchRequest(want string, haves ...string) string {
	r := pkt("want "+want+"\n") + "0000"
	for _, h := range haves {
		r += pkt("have " + h + "\n")
	}
	return r + pkt("done\n")
}

// remote is the URL of the repository segment pair repo, such as
// "acme/demo.git", on the hub, with password in its user information, as a
// git client is given it.
func (w *world) remote(password, repo string) string {
	return remoteAt(w.URL("/"), password, repo)
}

// remoteAt is remote on the server at base, such as a proxy in front of
// the hub.
func remoteAt(base, password, repo string) string {
	u, err := url.Parse(base)
	if err != nil {
		panic(err)
	}
	u.Path, u.User = "/git/"+repo, url.UserPassword("x", password)
	return u.String()
}

// history is what extendHistory built in demo's repository.
type history struct {
	// main are main's four commits, oldest first, and dates their commit
	// dates, a day apart.
	main  [4]string
	dates [4]time.Time
	// small and big are the blob ids of a.txt (10 bytes) and big.bin
	// (200 kB), and bigSum the SHA-256 of big.bin's content.
	small, big string
	bigSum     [sha256.Size]byte
	// change is the commit refs/parley/changes/1/1 points at, which is on
	// no branch, and dangling a commit no ref reaches.
	change, dangling string
}

// extendHistory replaces demo's history with one that holds every kind of
// ref a hub serves: main with four commits a day apart, the first adding
// a.txt and big.bin and the others changing README; the tag v0 on the
// first; dev on the second with a commit of its own; the tag v1 on the
// second, so that main~1 is no ref's tip; a note on the fourth in refs/notes/commits; the hub-owned
// refs/parley/changes/1/1 on a commit off the third that no branch holds
// and refs/parley/sandboxes/sb1/main at main; and a commit no ref reaches.
// It moves w.head to main's new head. The refs are written in one run of
// git fast-import.
func (w *world) extendHistory(t *testing.T) history {
	t.Helper()
	var h history
	big := bytes.Repeat([]byte("parley big.bin\n"), 200_000/15+1)[:200_000]
	h.bigSum = sha256.Sum256(big)
	var s strings.Builder
	commit := func(ref string, mark int, at time.Time, from, files string) {
		fmt.Fprintf(&s, "commit %s\nmark :%d\ncommitter P <p@example.invalid> %d +0000\ndata 2\n%02d\n", ref, mark, at.Unix(), mark)
		if from != "" {
			fmt.Fprintf(&s, "from %s\n", from)
		}
		s.WriteString(files + "\n")
	}
	file := func(path, content string) string {
		return fmt.Sprintf("M 100644 inline %s\ndata %d\n%s\n", path, len(content), content)
	}
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range h.dates {
		h.dates[i] = day.AddDate(0, 0, i)
	}
	s.WriteString("reset refs/heads/main\n")
	commit("refs/heads/main", 1, h.dates[0], "", file("a.txt", "0123456789")+file("big.bin", string(big)))
	for i := 2; i <= 4; i++ {
		commit("refs/heads/main", i, h.dates[i-1], fmt.Sprintf(":%d", i-1), file("README", fmt.Sprintf("%d\n", i)))
	}
	commit("refs/heads/dev", 5, h.dates[1], ":2", file("README", "dev\n"))
	commit("refs/parley/changes/1/1", 6, h.dates[2], ":3", file("README", "change\n"))
	commit("refs/notes/commits", 7, h.dates[3], "", "N inline :4\ndata 5\nnote\n")
	s.WriteString("reset refs/tags/v0\nfrom :1\n\nreset refs/tags/v1\nfrom :2\n\nreset refs/parley/sandboxes/sb1/main\nfrom :4\n\n")
	if _, err := gitrepotest.Run(w.repo, s.String(), "fast-import", "--quiet", "--force", "--date-format=raw"); err != nil {
		t.Fatal(err)
	}
	ids := strings.Fields(gitrepotest.Git(t, w.repo, "rev-parse", "main~3", "main~2", "main~1", "main", "main:a.txt", "main:big.bin", "refs/parley/changes/1/1"))
	copy(h.main[:], ids)
	h.small, h.big, h.change = ids[4], ids[5], ids[6]
	h.dangling = gitrepotest.Git(t, w.repo, "commit-tree", "-m", "dangling", h.main[3]+"^{tree}")
	w.head = h.main[3]
	return h
}

// clientGit runs stock git with args in dir as a client of the hub, with
// none of the host's configuration (an empty home, no system
// configuration) and no terminal prompt, env added to its environment, and
// returns what it wrote to its standard output and error.
func clientGit(t *testing.T, dir string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// newForkWorld is newGitWorld with the task fork demo--t1 of demo, which
// tracks main, dev and v1, and view, when not nil, in place of the hub's
// fork view, naming the same tracking refs and change 7 as the fork's. demo
// holds, besides main, the branch dev and the tag v1 on main's first commit
// and the version refs refs/parley/changes/7/1 and 7/2, on main's two
// commits, and 8/1, on dev. The fork's repository holds its own branch work,
// on a commit of the fork's own, at w.work.
func newForkWorld(t *testing.T, view *fakeForkView) *world {
	t.Helper()
	w := newGitWorld(t, hubtest.Config{Compose: func(c *hub.Composition) {
		if view != nil {
			view.set(gitserver.ForkRefs{Tracking: []string{"refs/heads/main", "refs/heads/dev", "refs/tags/v1"}, Changes: []int64{7}}, nil)
			c.AdjustGitServer(func(d *gitserver.Deps) { d.ForkView = view })
		}
	}})
	first := gitrepotest.Git(t, w.repo, "rev-parse", "main~1")
	for ref, id := range map[string]string{
		"refs/heads/dev": first, "refs/tags/v1": first,
		"refs/parley/changes/7/1": first, "refs/parley/changes/7/2": w.head, "refs/parley/changes/8/1": first,
	} {
		gitrepotest.Git(t, w.repo, "update-ref", ref, id)
	}
	w.addForkTracking(t, []string{"main", "dev"}, []string{"v1"})
	fork := w.workspaces.RepoPath("demo--t1")
	w.work = gitrepotest.Git(t, fork, "commit-tree", "-p", w.head, "-m", "work", w.head+"^{tree}")
	gitrepotest.Git(t, fork, "update-ref", "refs/heads/work", w.work)
	return w
}

// commitOn adds a commit to the branch of demo's repository and returns its
// id.
func (w *world) commitOn(t *testing.T, branch string) string {
	t.Helper()
	id := gitrepotest.Git(t, w.repo, "commit-tree", "-p", "refs/heads/"+branch, "-m", "next", "refs/heads/"+branch+"^{tree}")
	gitrepotest.Git(t, w.repo, "update-ref", "refs/heads/"+branch, id)
	return id
}

// fakeForkView is the git server's fork view in the tests: it answers what
// set gave it and counts the questions.
type fakeForkView struct {
	mu    sync.Mutex
	refs  gitserver.ForkRefs
	err   error
	asked int
}

func (f *fakeForkView) ForkRefs(context.Context, string) (gitserver.ForkRefs, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked++
	return f.refs, f.err
}

// set makes the view answer refs and err.
func (f *fakeForkView) set(refs gitserver.ForkRefs, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs, f.err = refs, err
}

// questions is how often the view was asked.
func (f *fakeForkView) questions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked
}

// lsRemote maps every ref stock git lists for remote, run with gitArgs
// before ls-remote, to its id, and each symbolic ref, such as HEAD, as
// "symref HEAD", to the ref it names.
func lsRemote(t *testing.T, remote string, gitArgs ...string) map[string]string {
	t.Helper()
	out := runGit(t, t.TempDir(), append(slices.Clone(gitArgs), "ls-remote", "--symref", remote)...)
	refs := map[string]string{}
	for line := range strings.Lines(out) {
		if id, name, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok {
			if target, symref := strings.CutPrefix(id, "ref: "); symref {
				refs["symref "+name] = target
			} else {
				refs[name] = id
			}
		}
	}
	return refs
}

// runGit is clientGit without an environment of its own, failing the test
// when git fails, and returns git's output.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := clientGit(t, dir, nil, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}
