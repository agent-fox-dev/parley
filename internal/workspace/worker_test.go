package workspace_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestWorkers_TakeUpPendingAndCloningWorkInCreationOrderAtStart(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	history(o)
	w := newWorld(t, withGitOrigin(o))
	at := func(s int) time.Time { return time.Date(2026, 1, 2, 10, 0, s, 0, time.UTC) }
	w.insertRecord(t, "ws-c", workspace.StatusCloning, at(2), o.URL())
	w.insertRecord(t, "ws-b", workspace.StatusPending, at(1), o.URL())
	w.insertRecord(t, "ws-a", workspace.StatusPending, at(1), o.URL())
	w.insertRecord(t, "ws-r", workspace.StatusReady, at(0), o.URL())
	w.insertRecord(t, "ws-f", workspace.StatusFailed, at(0), o.URL())
	leftover := filepath.Join(filepath.Dir(w.svc.RepoPath("ws-a")), ".clone-12345")
	if err := os.MkdirAll(filepath.Join(leftover, "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	runs := &runLog{}
	wk := w.startWorkers(t, 1, runs.around(w.svc.Clone))
	// Work recorded once Start has returned runs after all it queued.
	wk.Enqueue("ws-z")
	eventually(t, "four runs", func() bool { _, finished, _ := runs.snapshot(); return len(finished) == 4 })
	if started, _, _ := runs.snapshot(); !slices.Equal(started, []string{"ws-a", "ws-b", "ws-c", "ws-z"}) {
		t.Errorf("clone work ran in the order %v, want ws-a, ws-b, ws-c, then the work recorded after the start", started)
	}
	for _, c := range []struct {
		slug string
		want workspace.Status
	}{{"ws-a", workspace.StatusReady}, {"ws-b", workspace.StatusReady}, {"ws-c", workspace.StatusReady}, {"ws-r", workspace.StatusReady}, {"ws-f", workspace.StatusFailed}} {
		rec, _ := w.workspace(t, c.slug)
		if rec.Status != c.want {
			t.Errorf("%s: status %s, want %s", c.slug, rec.Status, c.want)
		}
		if (c.slug == "ws-r" || c.slug == "ws-f") && (rec.HeadSHA != nil || !rec.UpdatedAt.Equal(at(0))) {
			t.Errorf("%s: head_sha %v, updated_at %v; the workers touched it", c.slug, rec.HeadSHA, rec.UpdatedAt)
		}
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Errorf("the interrupted clone %s is still there (%v)", leftover, err)
	}
}

// commitOnMain adds a commit to main directly in the copy dir and returns
// it.
func commitOnMain(t *testing.T, dir string) string {
	t.Helper()
	id := gitIn(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com",
		"commit-tree", "main^{tree}", "-p", "main", "-m", "made in the copy")
	gitIn(t, dir, "update-ref", "refs/heads/main", id)
	return id
}

func TestClone_KeepsAValidCopyOfTheWorkspaceWithoutAskingOrigin(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	history(o)
	w := newWorld(t, withGitOrigin(o))
	cases := []struct {
		slug string
		// forget removes default_branch from the record before the work.
		forget bool
		head   string
	}{{slug: "kept"}, {slug: "kept-without-branch", forget: true}}
	for i, c := range cases {
		w.createFrom(t, c.slug, o.URL(), nil)
		w.clone(t, c.slug)
		cases[i].head = commitOnMain(t, w.svc.RepoPath(c.slug))
		w.setPending(t, c.slug, c.forget)
	}
	asked := o.Requests()
	o.Close()
	for _, c := range cases {
		rec := w.clone(t, c.slug)
		if rec.Status != workspace.StatusReady || rec.HeadSHA == nil || *rec.HeadSHA != c.head ||
			rec.DefaultBranch == nil || *rec.DefaultBranch != "main" {
			t.Errorf("%s: status %s, head_sha %v, default_branch %v; want ready, the copy's head %s, main",
				c.slug, rec.Status, rec.HeadSHA, rec.DefaultBranch, c.head)
		}
	}
	if o.Requests() != asked {
		t.Errorf("origin got %d requests, want none", o.Requests()-asked)
	}
}

func TestClone_DiscardsWhatIsNotAValidCopyOfTheWorkspaceAndClonesFresh(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	mainHead, devHead, v1 := history(o)
	w := newWorld(t, withGitOrigin(o))
	// cloned makes a valid copy of slug with the marker branch stale.
	cloned := func(slug string) string {
		w.createFrom(t, slug, o.URL(), nil)
		w.clone(t, slug)
		dir := w.svc.RepoPath(slug)
		gitIn(t, dir, "update-ref", "refs/heads/stale", mainHead)
		w.setPending(t, slug, false)
		return dir
	}
	for i, c := range []struct {
		name string
		// place puts what lies at the copy's place of slug before the work.
		place func(slug string)
	}{
		{"a plain file", func(slug string) {
			w.createFrom(t, slug, o.URL(), nil)
			if err := os.WriteFile(w.svc.RepoPath(slug), []byte("not a repository"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"an empty directory", func(slug string) {
			w.createFrom(t, slug, o.URL(), nil)
			if err := os.Mkdir(w.svc.RepoPath(slug), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"a bare repository without commits", func(slug string) {
			w.createFrom(t, slug, o.URL(), nil)
			if out, err := exec.Command("git", "init", "--quiet", "--bare", w.svc.RepoPath(slug)).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v: %s", err, out)
			}
		}},
		{"a copy whose origin URL was changed", func(slug string) {
			gitIn(t, cloned(slug), "config", "--local", "remote.origin.url", "https://example.com/other.git")
		}},
		{"the copy of an earlier workspace of the same slug", func(slug string) {
			cloned(slug)
			w.deleteRecord(t, slug)
			w.createFrom(t, slug, o.URL(), nil)
		}},
		{"a copy without an instance id", func(slug string) {
			gitIn(t, cloned(slug), "config", "--local", "--unset", "parley.workspace")
		}},
	} {
		slug := "found-" + strconv.Itoa(i)
		c.place(slug)
		asked := o.Requests()
		rec := w.clone(t, slug)
		if rec.Status != workspace.StatusReady || rec.HeadSHA == nil || *rec.HeadSHA != mainHead {
			t.Errorf("%s: status %s, head_sha %v; want ready at origin's main %s", c.name, rec.Status, rec.HeadSHA, mainHead)
			continue
		}
		want := fmt.Sprintf("refs/heads/dev=%s\nrefs/heads/main=%s\nrefs/tags/v1=%s", devHead, mainHead, v1)
		if got := gitIn(t, w.svc.RepoPath(slug), "for-each-ref", "--format=%(refname)=%(objectname)", "refs/heads", "refs/tags"); got != want {
			t.Errorf("%s: the copy holds\n%s\nwant origin's refs only\n%s", c.name, got, want)
		}
		if o.Requests() == asked {
			t.Errorf("%s: origin got no request, want a fresh clone", c.name)
		}
	}
}

func TestWorkers_RunAtMostTheirNumberOfClonesFirstRecordedFirst(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	history(o)
	w := newWorld(t, withGitOrigin(o))
	runs := &runLog{hold: make(chan struct{})}
	wk := w.startWorkers(t, 2, runs.around(w.svc.Clone))
	slugs := []string{"ws-1", "ws-2", "ws-3", "ws-4", "ws-5"}
	for _, slug := range slugs {
		w.createFrom(t, slug, o.URL(), nil)
		wk.Enqueue(slug)
	}
	startedN := func(n int) func() bool {
		return func() bool { started, _, _ := runs.snapshot(); return len(started) == n }
	}
	eventually(t, "two clones running", startedN(2))
	time.Sleep(50 * time.Millisecond)
	if started, _, _ := runs.snapshot(); len(started) != 2 || !slices.Equal(slices.Sorted(slices.Values(started)), []string{"ws-1", "ws-2"}) {
		t.Fatalf("running: %v, want ws-1 and ws-2 only", started)
	}
	for _, slug := range slugs[2:] {
		if rec, _ := w.workspace(t, slug); rec.Status != workspace.StatusPending {
			t.Errorf("%s: status %s while two clones run, want pending", slug, rec.Status)
		}
	}
	for n, next := range slugs[2:] {
		runs.hold <- struct{}{}
		eventually(t, next+" started", startedN(3+n))
		if started, _, _ := runs.snapshot(); started[2+n] != next {
			t.Errorf("after %d releases the clones started are %v, want %s next", n+1, started, next)
		}
	}
	close(runs.hold)
	eventually(t, "five runs", func() bool { _, finished, _ := runs.snapshot(); return len(finished) == 5 })
	if _, _, most := runs.snapshot(); most != 2 {
		t.Errorf("at most %d clones ran at once, want 2", most)
	}
	for _, slug := range slugs {
		if rec, _ := w.workspace(t, slug); rec.Status != workspace.StatusReady {
			t.Errorf("%s: status %s, want ready", slug, rec.Status)
		}
	}
}

// gitChildren are the git processes the test process started that still
// run.
func gitChildren(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("pgrep", "-l", "-P", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return nil // pgrep finds no child
	}
	var gits []string
	for line := range strings.Lines(string(out)) {
		if strings.Contains(line, "git") {
			gits = append(gits, strings.TrimSpace(line))
		}
	}
	return gits
}

// Not parallel: it counts the git processes of the whole test process.
func TestWorkers_StopInterruptsACloneAndLeavesItForTheNextStart(t *testing.T) {
	o := gitrepotest.NewOrigin(t)
	history(o)
	w := newWorld(t, withGitOrigin(o))
	w.createFrom(t, "demo", o.URL(), nil)
	o.Block()
	wk, err := workspace.NewWorkers(1, w.svc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := wk.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	wk.Enqueue("demo")
	eventually(t, "the clone asks origin", func() bool { return o.Requests() > 0 })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := wk.Stop(ctx); err != nil || time.Since(start) >= 5*time.Second {
		t.Fatalf("Stop = %v after %v, want nil within 5 s", err, time.Since(start))
	}
	if gits := gitChildren(t); len(gits) != 0 {
		t.Errorf("git processes left after the stop: %v", gits)
	}
	if rec, _ := w.workspace(t, "demo"); rec.Status != workspace.StatusCloning && rec.Status != workspace.StatusPending {
		t.Errorf("status %s after the stop, want cloning or pending", rec.Status)
	}
	if left := w.leftovers(t, "demo"); len(left) != 0 {
		t.Errorf("the repository root holds %v after the stop, want nothing of the clone", left)
	}
	o.Release()
	w.startWorkers(t, 1, nil)
	eventually(t, "the next workers finish the clone", func() bool {
		rec, _ := w.workspace(t, "demo")
		return rec.Status == workspace.StatusReady
	})
}

func TestNewWorkers_RefusesFewerThanOneWorker(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		size    int
		refused bool
	}{{0, true}, {-1, true}, {1, false}, {4, false}} {
		wk, err := workspace.NewWorkers(c.size, &workspace.Service{}, nil)
		switch {
		case c.refused && (err == nil || !strings.Contains(err.Error(), "[workspace].workers") || wk != nil):
			t.Errorf("size %d: %v, %v; want no workers and an error naming [workspace].workers", c.size, wk, err)
		case !c.refused && (err != nil || wk == nil):
			t.Errorf("size %d: %v, %v; want workers", c.size, wk, err)
		}
	}
}

func TestWorkers_CloneWhatTheHubCreatesAndWhatWasPendingAtItsStart(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	history(o)
	w := newWorld(t, withGitOrigin(o), withCloneWorkers())
	ready := func(slug string) func() bool {
		return func() bool { rec, _ := w.workspace(t, slug); return rec.Status == workspace.StatusReady }
	}
	w.createFrom(t, "created", o.URL(), nil)
	eventually(t, "the created workspace is ready", ready("created"))
	w.insertRecord(t, "waiting", workspace.StatusPending, w.clock(), o.URL())
	w.Restart(t)
	eventually(t, "the workspace pending at the start is ready", ready("waiting"))
	if _, err := (gitrepo.Git{}).Open(context.Background(), w.svc.RepoPath("waiting")); err != nil {
		t.Errorf("no copy of the workspace pending at the start: %v", err)
	}
}
