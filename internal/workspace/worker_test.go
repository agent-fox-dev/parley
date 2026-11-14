package workspace_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestWorkers_TakeUpPendingAndCloningWorkInCreationOrderAtStart(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	o.History()
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

func TestWorkers_RunAtMostTheirNumberOfClonesFirstRecordedFirst(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	o.History()
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
	o.History()
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
	if err := wk.Launch(context.Background()); err != nil {
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

func TestWorkers_FailAWorkspaceWhoseOutcomeCouldNotBeRecordedOnceTheStoreAnswers(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	o.History()
	w := newWorld(t, withGitOrigin(o))
	w.createFrom(t, "demo", o.URL(), nil)
	// The store refuses every write of an outcome, ready or failed, until the
	// trigger is dropped.
	if _, err := w.Store.Main().Exec(`CREATE TRIGGER refuse_outcome BEFORE UPDATE OF status ON workspaces
WHEN NEW.status IN ('ready', 'failed') BEGIN SELECT RAISE(ABORT, 'the store refuses the write'); END`); err != nil {
		t.Fatal(err)
	}
	runs := &runLog{}
	wk := w.startWorkers(t, 1, runs.around(w.svc.Clone))
	wk.Enqueue("demo")
	eventually(t, "the clone work returns", func() bool { _, finished, _ := runs.snapshot(); return len(finished) == 1 })
	if rec, _ := w.workspace(t, "demo"); rec.Status != workspace.StatusCloning {
		t.Fatalf("status %s while the store refuses the outcome, want cloning", rec.Status)
	}
	asked := o.Requests()
	if _, err := w.Store.Main().Exec(`DROP TRIGGER refuse_outcome`); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the workspace is failed once the store answers", func() bool {
		rec, _ := w.workspace(t, "demo")
		return rec.Status == workspace.StatusFailed
	})
	rec, _ := w.workspace(t, "demo")
	if rec.CloneError == nil || rec.HeadSHA != nil {
		t.Errorf("clone_error %v, head_sha %v; want the write error and no head", rec.CloneError, rec.HeadSHA)
	} else if !strings.Contains(*rec.CloneError, "the store refuses the write") {
		t.Errorf("clone_error %q, want the write error the store answered", *rec.CloneError)
	}
	if left := w.leftovers(t, "demo"); len(left) != 0 {
		t.Errorf("the repository root holds %v for the failed workspace, want nothing", left)
	}
	if _, finished, _ := runs.snapshot(); len(finished) != 1 || o.Requests() != asked {
		t.Errorf("%d runs and %d more requests to origin, want the one run and no further attempt", len(finished), o.Requests()-asked)
	}
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
	o.History()
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

func TestWorkers_CloneNothingWhenALaterPartStopsTheHubsStart(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	o.History()
	var configPath string
	w := newWorld(t, withGitOrigin(o), withCompose(func(c *hub.Composition) {
		c.AddStartHook("config-path", func(_ context.Context, rt *hub.Runtime) error {
			configPath = rt.Config.Path
			return nil
		})
	}))
	w.insertRecord(t, "waiting", workspace.StatusPending, w.clock(), o.URL())
	w.Stop()
	if code := w.ExitCode(t, 10*time.Second); code != 0 {
		t.Fatalf("the hub stopped with %d, want 0", code)
	}
	dir := filepath.Dir(configPath)
	asked := o.Requests()
	var log hubtest.LogBuffer
	code := hub.Run(context.Background(), hub.Options{
		Env: config.MapEnv(map[string]string{
			"XDG_CONFIG_HOME": dir, "XDG_DATA_HOME": dir, secret.EnvCurrentKey: testKey, "GIT_SSL_CAINFO": o.CAFile(),
		}),
		WorkDir: dir, Stderr: &log,
		Compose: func(c *hub.Composition) {
			c.AddStartHook("refuses", func(context.Context, *hub.Runtime) error {
				// Long enough for a clone already running to reach origin.
				for deadline := time.Now().Add(500 * time.Millisecond); o.Requests() == asked && time.Now().Before(deadline); {
					time.Sleep(5 * time.Millisecond)
				}
				return errors.New("a later part refuses to start")
			})
		},
	})
	if code == 0 || !strings.Contains(log.String(), "a later part refuses to start") {
		t.Fatalf("the start returned %d, want it stopped by the refusing part; log:\n%s", code, log.String())
	}
	time.Sleep(100 * time.Millisecond)
	if o.Requests() != asked {
		t.Errorf("origin got %d requests from a hub whose start failed, want none", o.Requests()-asked)
	}
	w.Restart(t)
	eventually(t, "the next start clones the waiting workspace", func() bool {
		rec, _ := w.workspace(t, "waiting")
		return rec.Status == workspace.StatusReady
	})
}

func TestService_EnqueueHandsAPendingWorkspaceToTheCloneWorkers(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	o.History()
	w := newWorld(t, withGitOrigin(o), withCloneWorkers())
	w.insertRecord(t, "back", workspace.StatusPending, w.clock(), o.URL())
	w.svc.Enqueue("back")
	eventually(t, "the workspace put back to pending is ready", func() bool {
		rec, _ := w.workspace(t, "back")
		return rec.Status == workspace.StatusReady
	})
}
