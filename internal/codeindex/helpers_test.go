package codeindex_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/codeindex"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// TestMain runs the package's tests at most hubtest.HubParallel at a time:
// each starts a hub, and hubs contend for one lock past that.
func TestMain(m *testing.M) {
	hubtest.LimitParallel(hubtest.HubParallel)
	os.Exit(m.Run())
}

// enabled is the configuration of a hub that keeps code indexes.
const enabled = "[code_index]\n"

// passInterval is the time between two maintenance passes of a world.
const passInterval = 50 * time.Millisecond

// world is a hub with the user owner, who has an API key, and the org acme
// the owner is a member of. Its maintenance loop runs every passInterval and
// counts its passes in passes.
type world struct {
	*hubtest.Hub
	workspaces *workspace.Service
	index      *codeindex.Component
	dataDir    string
	passes     atomic.Int64

	owner, key, org string
}

// newWorld starts a world whose config.toml holds toml, with adjust
// changing the code index component's dependencies.
func newWorld(t *testing.T, toml string, adjust ...func(*codeindex.Deps)) *world {
	t.Helper()
	return newComposedWorld(t, hubtest.Config{TOML: toml}, adjust...)
}

// newComposedWorld is newWorld on cfg, whose Compose runs after the world's
// own and whose MaintenanceInterval, when set, replaces passInterval: a
// test of a notice sets a long one, so that no pass can do the notice's
// work.
func newComposedWorld(t *testing.T, cfg hubtest.Config, adjust ...func(*codeindex.Deps)) *world {
	t.Helper()
	w := &world{}
	compose := cfg.Compose
	if cfg.MaintenanceInterval == 0 {
		cfg.MaintenanceInterval = passInterval
	}
	// The workspaces are made by hand, so nothing is cloned; the rate limit
	// would count the polls of a status.
	cfg.TOML = "[limits]\nrequests_per_minute = 0\n" + cfg.TOML
	cfg.Compose = func(c *hub.Composition) {
		w.workspaces, w.index = c.Workspaces(), c.CodeIndex()
		for _, a := range adjust {
			c.AdjustCodeIndex(a)
		}
		c.AddStartHook("code-index-test", func(_ context.Context, rt *hub.Runtime) error {
			w.dataDir = rt.Config.DataDir()
			return nil
		})
		// Registered last, so a pass it counts has run the code index's task.
		c.AddMaintenanceTask("code-index-test-pass", func(context.Context, *hub.Runtime) error {
			w.passes.Add(1)
			return nil
		})
		if compose != nil {
			compose(c)
		}
	}
	w.Hub = hubtest.StartWith(t, cfg)
	w.owner = w.CreateUser(t, "owner")
	w.key = w.IssueKey(t, w.owner)
	w.org = w.CreateOrg(t, "acme")
	w.AddMember(t, w.org, w.owner)
	return w
}

// pass waits until a maintenance pass that began after the call has ended.
func (w *world) pass(t *testing.T) {
	t.Helper()
	// The pass under way may have run the task before the call; the one
	// after it has not.
	want := w.passes.Load() + 2
	eventually(t, "two more maintenance passes", func() bool { return w.passes.Load() >= want })
}

// eventually waits up to 20 s for cond.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 20 s for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// addWorkspace adds the ready repository workspace slug of the owner in
// acme, whose main is one commit holding files, and returns that commit.
func (w *world) addWorkspace(t *testing.T, slug string, files map[string]string) string {
	t.Helper()
	ctx, now, dir := context.Background(), time.Now().UTC(), w.workspaces.RepoPath(slug)
	gitrepotest.InitBare(t, dir, "main")
	head := gitrepotest.Commit(t, dir, "first", files)
	gitrepotest.SetRef(t, dir, "refs/heads/main", head)
	rec := workspace.Workspace{
		Slug: slug, Kind: access.KindRepository, Mode: workspace.ModeStandard,
		GitURL: new("https://origin.test/" + slug + ".git"), DefaultBranch: new("main"), OwnerID: w.owner, OrgID: w.org,
		Status: workspace.StatusReady, DisplayName: slug, HeadSHA: &head,
		CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(),
	}
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error { return workspace.Insert(ctx, tx, rec) }); err != nil {
		t.Fatal(err)
	}
	return head
}

// head is the head of main of the workspace slug.
func (w *world) head(t *testing.T, slug string) string {
	t.Helper()
	return gitrepotest.Refs(t, w.workspaces.RepoPath(slug), "refs/heads/")["main"]
}

// commit commits to branch of the workspace slug through the source control
// API, as the owner: every file of put written and every path of remove
// deleted. It returns the new commit.
func (w *world) commit(t *testing.T, slug, branch string, put map[string]string, remove ...string) string {
	t.Helper()
	ops := []map[string]any{}
	for path, content := range put {
		ops = append(ops, map[string]any{"op": "put", "path": path, "content_base64": base64.StdEncoding.EncodeToString([]byte(content))})
	}
	for _, path := range remove {
		ops = append(ops, map[string]any{"op": "delete", "path": path})
	}
	body := map[string]any{"branch": branch, "message": "change", "operations": ops}
	if at := gitrepotest.Refs(t, w.workspaces.RepoPath(slug), "refs/heads/")[branch]; at != "" {
		body["expected_head_sha"] = at
	} else {
		// A new branch, on the workspace's main.
		body["base_ref"] = "main"
	}
	status, raw := w.Call(t, http.MethodPost, "/api/v1/workspaces/"+slug+"/scm/commits", w.key, body)
	var answer struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(raw, &answer); status != http.StatusCreated || err != nil || answer.Commit.SHA == "" {
		t.Fatalf("POST scm/commits on %s of %s = %d %s, want 201 with the commit", branch, slug, status, raw)
	}
	return answer.Commit.SHA
}

// jobs are the code index jobs of the workspace slug that have not ended,
// as their statuses.
func (w *world) jobs(t *testing.T, slug string) []string {
	t.Helper()
	rows, err := w.Store.Main().Query(
		`SELECT status FROM jobs WHERE type = 'code_index' AND workspace_slug = ? AND status IN ('queued', 'running', 'failed') ORDER BY created_at, id`, slug)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	statuses := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, s)
	}
	return statuses
}

// builds is the hub's count of ended builds by outcome, from GET /metrics;
// an outcome the hub does not export counts 0.
func (w *world) builds(t *testing.T) map[string]float64 {
	t.Helper()
	counts := map[string]float64{"ok": 0, "partial": 0, "failed": 0}
	for line := range strings.SplitSeq(w.metricsText(t), "\n") {
		rest, ok := strings.CutPrefix(line, `parley_code_index_builds_total{outcome="`)
		if !ok {
			continue
		}
		outcome, value, _ := strings.Cut(rest, `"} `)
		var n float64
		if err := json.Unmarshal([]byte(strings.TrimSpace(value)), &n); err != nil {
			t.Fatalf("the metrics line %q has no number: %v", line, err)
		}
		counts[outcome] = n
	}
	return counts
}

// metricsText is the body of GET /metrics.
func (w *world) metricsText(t *testing.T) string {
	t.Helper()
	res, err := http.Get(w.URL("/metrics"))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics = %d (%v)", res.StatusCode, err)
	}
	return string(body)
}

// entries are the names of what the hub's code index directory holds for
// the workspace slug, none when it holds nothing of it; with slug "" what
// the directory itself holds.
func (w *world) entries(t *testing.T, slug string) []string {
	t.Helper()
	dir, err := os.ReadDir(filepath.Join(w.dataDir, codeindex.DirName, slug))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	names := []string{}
	for _, e := range dir {
		names = append(names, e.Name())
	}
	return names
}

// record is the code index record of the workspace slug, the zero record
// when it has none.
func (w *world) record(t *testing.T, slug string) codeindex.Record {
	t.Helper()
	r, _, err := w.index.Record(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// queue queues a build of the workspace slug, as a move of its head does.
func (w *world) queue(t *testing.T, slug string) {
	t.Helper()
	if err := w.index.Queue(context.Background(), slug); err != nil {
		t.Fatalf("queue a build of %s: %v", slug, err)
	}
}

// indexed queues a build of the workspace slug and waits until its index is
// at the head of its main; it returns the record.
func (w *world) indexed(t *testing.T, slug string) codeindex.Record {
	t.Helper()
	w.queue(t, slug)
	return w.reaches(t, slug, w.head(t, slug))
}

// reaches waits until the index of the workspace slug holds the commit sha
// and none of its builds is pending, and returns its record.
func (w *world) reaches(t *testing.T, slug, sha string) codeindex.Record {
	t.Helper()
	eventually(t, "the index of "+slug+" to hold "+sha, func() bool {
		return w.record(t, slug).IndexedSHA == sha && len(w.jobs(t, slug)) == 0
	})
	return w.record(t, slug)
}

// tree lists what the tree of the commit sha of the workspace slug holds:
// each regular file with its content, each symbolic link as "-> target",
// and each directory that holds nothing as "/".
func (w *world) tree(t *testing.T, slug, sha string) map[string]string {
	t.Helper()
	root := w.index.TreeDir(slug, sha)
	found := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || path == root {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		switch {
		case d.Type()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			found[rel] = "-> " + target
			return err
		case d.IsDir():
			if entries, err := os.ReadDir(path); err != nil || len(entries) == 0 {
				found[rel] = "/"
				return err
			}
		default:
			content, err := os.ReadFile(path)
			found[rel] = string(content)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the tree of %s at %s: %v", slug, sha, err)
	}
	return found
}

// seam stands between the component and its indexes: it can hold the
// builds of a tree until they are let go, and make them fail or report a
// partial index.
type seam struct {
	mu sync.Mutex
	// held are the trees whose builds wait, by a part of their path, with
	// what lets them go.
	held map[string]chan struct{}
	// fail, when not nil, is the error of the build of a tree, nil to let
	// it build.
	fail func(tree string) error
	// partial makes every index report itself partial.
	partial bool
	// began receives the tree of every build that began, and ended the
	// tree of every held build whose context ended; opened are the bounds
	// every index was made with.
	began, ended chan string
	opened       []codeindex.Bounds
	// searchErr, when not nil, is what every search of an index answers;
	// searches counts the searches that reached an index.
	searchErr error
	searches  int
}

func newSeam() *seam {
	return &seam{held: map[string]chan struct{}{}, began: make(chan string, 64), ended: make(chan string, 64)}
}

// waitEnded waits for a held build of a tree whose path holds part to see
// its context end.
func (s *seam) waitEnded(t *testing.T, part string) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case tree := <-s.ended:
			if strings.Contains(tree, part) {
				return
			}
		case <-deadline:
			t.Fatalf("waited 20 s for the held build of %s to be ended", part)
		}
	}
}

// hold makes the builds of the trees whose path holds part wait from now
// on, and returns what lets them go.
func (s *seam) hold(part string) (release func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The builds that began before are not the ones held: forgotten, so
	// that waitBegan waits for one that began under this hold.
	for drained := false; !drained; {
		select {
		case <-s.began:
		default:
			drained = true
		}
	}
	ch := make(chan struct{})
	s.held[part] = ch
	return sync.OnceFunc(func() { close(ch) })
}

// failWith sets what the builds fail with.
func (s *seam) failWith(fail func(tree string) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = fail
}

// adjust puts the seam before the component's indexes.
func (s *seam) adjust(d *codeindex.Deps) {
	open := d.Open
	if open == nil {
		open = codeindex.OpenSearchIndex
	}
	d.Open = func(tree, store, scratch string, b codeindex.Bounds) (codeindex.Index, error) {
		s.mu.Lock()
		s.opened = append(s.opened, b)
		s.mu.Unlock()
		idx, err := open(tree, store, scratch, b)
		if err != nil {
			return nil, err
		}
		return seamIndex{Index: idx, seam: s, tree: tree}, nil
	}
}

// waitBegan waits for a build of a tree whose path holds part to begin.
func (s *seam) waitBegan(t *testing.T, part string) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case tree := <-s.began:
			if strings.Contains(tree, part) {
				return
			}
		case <-deadline:
			t.Fatalf("waited 20 s for a build of %s to begin", part)
		}
	}
}

type seamIndex struct {
	codeindex.Index
	seam *seam
	tree string
}

func (i seamIndex) Build(ctx context.Context) error {
	s := i.seam
	s.mu.Lock()
	var wait chan struct{}
	for part, ch := range s.held {
		if strings.Contains(i.tree, part) {
			wait = ch
		}
	}
	s.mu.Unlock()
	select {
	case s.began <- i.tree:
	default:
	}
	if wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			select {
			case s.ended <- i.tree:
			default:
			}
			return ctx.Err()
		}
	}
	// What a build fails with is asked once it is let go, so that a test
	// can decide it while the build is held.
	s.mu.Lock()
	fail := s.fail
	s.mu.Unlock()
	if fail != nil {
		if err := fail(i.tree); err != nil {
			return err
		}
	}
	return i.Index.Build(ctx)
}

func (i seamIndex) Search(ctx context.Context, query string, maxFiles int) (codeindex.SearchResult, error) {
	i.seam.mu.Lock()
	i.seam.searches++
	err := i.seam.searchErr
	i.seam.mu.Unlock()
	if err != nil {
		return codeindex.SearchResult{}, err
	}
	return i.Index.Search(ctx, query, maxFiles)
}

func (i seamIndex) State(ctx context.Context) (int, bool, error) {
	files, partial, err := i.Index.State(ctx)
	i.seam.mu.Lock()
	defer i.seam.mu.Unlock()
	return files, partial || i.seam.partial, err
}
