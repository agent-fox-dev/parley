package change_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
)

// Contents of a.txt: base is the world's; each other one changes its first
// line in its own way, so that any two of them conflict.
const (
	lineOneX = "x1\nl2\nl3\n"
	lineOneY = "y1\nl2\nl3\n"
	lineOneZ = "z1\nl2\nl3\n"
	lineOneM = "m1\nl2\nl3\n"
)

// conflictOf is the decoded conflict report of ch.
func conflictOf(t *testing.T, ch change.Change) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(ch.Conflict, &out); err != nil {
		t.Fatalf("conflict %s: %v", ch.Conflict, err)
	}
	return out
}

func TestMergeability_RebasesTheCurrentHeadOntoWhatTheChangeSitsOn(t *testing.T) {
	t.Parallel()
	// stack opens demo#1 from b1 (line one x) and demo#2 from b2 on top of
	// it (line one y), depending on demo#1, and moves main's line one.
	stack := func(t *testing.T, w *world) {
		b1 := commit(t, w.demo, "b1", w.base, "B1", map[string]string{"a.txt": lineOneX})
		w.openBranch(t, "b1", nil)
		commit(t, w.demo, "b2", b1, "B2", map[string]string{"a.txt": lineOneY})
		w.openBranch(t, "b2", nil)
		w.write(t, func(ctx context.Context, tx *sql.Tx) error {
			return w.changes.SetDependsOn(ctx, tx, "demo", 2, new(int64(1)))
		})
		commit(t, w.demo, "main", "main", "M", map[string]string{"a.txt": lineOneM})
	}
	main := func(t *testing.T, w *world) string { return gitrepotest.Refs(t, w.demo, "refs/heads/")["main"] }
	cases := []struct {
		name string
		// setup opens the changes and moves what they sit on, and returns
		// the change to check and the commit it must be rebased onto.
		setup func(t *testing.T, w *world) (int64, string)
		want  string
	}{
		{"own lines, target moved elsewhere", func(t *testing.T, w *world) (int64, string) {
			commit(t, w.demo, "b", w.base, "B", map[string]string{"a.txt": "l1\nl2\nl3 b\n"})
			w.openBranch(t, "b", nil)
			commit(t, w.demo, "main", "main", "M", map[string]string{"b.txt": "m\n"})
			return 1, main(t, w)
		}, change.MergeableClean},
		{"the target's line", func(t *testing.T, w *world) (int64, string) {
			commit(t, w.demo, "b", w.base, "B", map[string]string{"a.txt": lineOneX})
			w.openBranch(t, "b", nil)
			commit(t, w.demo, "main", "main", "M", map[string]string{"a.txt": lineOneM})
			return 1, main(t, w)
		}, change.MergeableConflict},
		{"a draft, own lines", func(t *testing.T, w *world) (int64, string) {
			commit(t, w.demo, "b", w.base, "B", map[string]string{"a.txt": "l1\nl2\nl3 b\n"})
			w.openBranch(t, "b", func(in *change.OpenInput) { in.Draft = true })
			commit(t, w.demo, "main", "main", "M", map[string]string{"b.txt": "m\n"})
			return 1, main(t, w)
		}, change.MergeableClean},
		{"a draft, the target's line", func(t *testing.T, w *world) (int64, string) {
			commit(t, w.demo, "b", w.base, "B", map[string]string{"a.txt": lineOneX})
			w.openBranch(t, "b", func(in *change.OpenInput) { in.Draft = true })
			commit(t, w.demo, "main", "main", "M", map[string]string{"a.txt": lineOneM})
			return 1, main(t, w)
		}, change.MergeableConflict},
		{"stacked on the head it applies to", func(t *testing.T, w *world) (int64, string) {
			stack(t, w)
			return 2, w.read(t, 1).HeadSHA
		}, change.MergeableClean},
		{"stacked, after the lower change rewrote the line", func(t *testing.T, w *world) (int64, string) {
			stack(t, w)
			z := commit(t, w.demo, "b1", w.base, "B1 again", map[string]string{"a.txt": lineOneZ})
			w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "b1", Head: z, Kind: change.KindPush, Caller: w.ownerCaller()})
			return 2, z
		}, change.MergeableConflict},
		{"stacked on an abandoned change", func(t *testing.T, w *world) (int64, string) {
			stack(t, w)
			w.write(t, func(ctx context.Context, tx *sql.Tx) error {
				return w.changes.Abandon(ctx, tx, "demo", 1, "", change.Caller{})
			})
			return 2, main(t, w)
		}, change.MergeableConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			number, onto := tc.setup(t, w)
			w.trigger(t, number)
			ch := w.settled(t, number)
			if ch.Mergeable != tc.want || (len(ch.Conflict) > 0) != (tc.want == change.MergeableConflict) {
				t.Fatalf("mergeable, conflict = %s, %s; want %s", ch.Mergeable, ch.Conflict, tc.want)
			}
			if tc.want != change.MergeableConflict {
				return
			}
			report := conflictOf(t, ch)
			files, _ := report["files"].([]any)
			if len(files) == 0 || files[0].(map[string]any)["path"] != "a.txt" || report["target_sha"] != onto || report["head_sha"] != ch.HeadSHA {
				t.Errorf("conflict = %s, want a.txt first, the head %s rebased onto %s", ch.Conflict, ch.HeadSHA, onto)
			}
		})
	}
}

// gate holds every check that passes it until it is released, and records
// the changes that passed.
type gate struct {
	mu     sync.Mutex
	open   chan struct{}
	passed []int64
}

func newGate() *gate { return &gate{open: make(chan struct{})} }

func (g *gate) pass(ctx context.Context, _ string, number int64) {
	g.mu.Lock()
	g.passed = append(g.passed, number)
	g.mu.Unlock()
	select {
	case <-g.open:
	case <-ctx.Done():
	}
}

func (g *gate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.open:
	default:
		close(g.open)
	}
}

func (g *gate) seen() []int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.passed)
}

// waitPassed waits until the changes that passed g are at least n.
func (g *gate) waitPassed(t *testing.T, n int) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); len(g.seen()) < n; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d checks passed the gate, want %d", len(g.seen()), n)
		}
	}
}

// push commits files on top of the branch from in a clone of demo and
// pushes the commit to branch, or with files nil deletes branch.
func (w *world) push(t *testing.T, branch, from string, files map[string]string) {
	t.Helper()
	clone := w.clone(t, "demo")
	if files == nil {
		clientGit(t, clone, "push", "--quiet", "origin", "--delete", branch)
		return
	}
	clientGit(t, clone, "checkout", "--quiet", "-B", branch, "origin/"+from)
	for path, content := range files {
		if err := os.WriteFile(filepath.Join(clone, path), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	clientGit(t, clone, "add", "-A")
	clientGit(t, clone, "commit", "--quiet", "-m", "pushed")
	clientGit(t, clone, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
}

func TestMergeability_MarksEveryAffectedChangeUnknownUntilItsCheck(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// before runs before the gate closes; trigger fires the trigger.
		before, trigger func(t *testing.T, w *world)
		affected        []int64
	}{
		{name: "a push to a change's source branch", trigger: func(t *testing.T, w *world) {
			w.push(t, "b3", "b3", map[string]string{"three2.txt": "3\n"})
		}, affected: []int64{3}},
		{name: "a push to the target of demo#1, #2 and #3", trigger: func(t *testing.T, w *world) {
			w.push(t, "main", "main", map[string]string{"m.txt": "m\n"})
		}, affected: []int64{1, 2, 3}},
		{name: "a new version of the change demo#2 depends on", trigger: func(t *testing.T, w *world) {
			w.push(t, "b1", "b1", map[string]string{"one2.txt": "1\n"})
		}, affected: []int64{1, 2}},
		{name: "a retarget", trigger: func(t *testing.T, w *world) {
			if status, out := w.patch(t, 4, w.ownerKey, map[string]any{"target_branch": "main"}); status != http.StatusOK {
				t.Fatalf("retarget = %d %v", status, out)
			}
		}, affected: []int64{4}},
		{name: "a restore", before: func(t *testing.T, w *world) {
			if status, out := w.send(t, http.MethodPost, "/workspaces/demo/changes/3/abandon", w.ownerKey, nil); status != http.StatusOK {
				t.Fatalf("abandon = %d %v", status, out)
			}
		}, trigger: func(t *testing.T, w *world) {
			if status, out := w.send(t, http.MethodPost, "/workspaces/demo/changes/3/restore", w.ownerKey, nil); status != http.StatusOK {
				t.Fatalf("restore = %d %v", status, out)
			}
		}, affected: []int64{3}},
		{name: "a push to a branch no change targets", trigger: func(t *testing.T, w *world) {
			w.push(t, "elsewhere", "main", map[string]string{"e.txt": "e\n"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			b1 := commit(t, w.demo, "b1", w.base, "B1", map[string]string{"one.txt": "1\n"})
			w.openBranch(t, "b1", nil)
			commit(t, w.demo, "b2", b1, "B2", map[string]string{"two.txt": "2\n"})
			w.openBranch(t, "b2", nil)
			w.write(t, func(ctx context.Context, tx *sql.Tx) error {
				return w.changes.SetDependsOn(ctx, tx, "demo", 2, new(int64(1)))
			})
			w.branch(t, "b3", "B3")
			w.openBranch(t, "b3", nil)
			w.branch(t, "b4", "B4")
			w.openBranch(t, "b4", func(in *change.OpenInput) { in.TargetBranch = new("release") })
			w.trigger(t, 2)
			for n := int64(1); n <= 4; n++ {
				w.settled(t, n)
			}
			if tc.before != nil {
				tc.before(t, w)
			}
			g := newGate()
			change.SetMergeableSeams(w.changes, change.MergeableSeams{Gate: g.pass})
			defer g.release()

			tc.trigger(t, w)
			for n := int64(1); n <= 4; n++ {
				ch, affected := w.read(t, n), slices.Contains(tc.affected, n)
				if affected != (ch.Mergeable == change.MergeableUnknown) || affected == change.MergeableIdle(w.changes, "demo", n) {
					t.Errorf("after the trigger demo#%d is %s %s, its check idle: %t; affected: %v", n, ch.State, ch.Mergeable,
						change.MergeableIdle(w.changes, "demo", n), tc.affected)
				}
			}
			g.release()
			for _, n := range tc.affected {
				if ch := w.settled(t, n); ch.Mergeable != change.MergeableClean {
					t.Errorf("demo#%d is %s after its check, want clean", n, ch.Mergeable)
				}
			}
			if got := slices.Sorted(slices.Values(g.seen())); !slices.Equal(slices.Compact(got), tc.affected) {
				t.Errorf("checked %v, want %v", got, tc.affected)
			}
		})
	}
}

func TestMergeability_ComputesOnlyTheLatestPairOfABurst(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	commit(t, w.demo, "b", w.base, "B", map[string]string{"a.txt": lineOneX})
	w.openBranch(t, "b", nil)
	w.settled(t, 1)

	var trials atomic.Int32
	held := newGate()
	change.SetMergeableSeams(w.changes, change.MergeableSeams{
		Trial: func(ctx context.Context, repo *gitrepo.Repo, onto, head string) (*gitrepo.ConflictReport, error) {
			if trials.Add(1) == 1 {
				held.pass(ctx, "demo", 1)
			}
			return repo.TrialRebase(ctx, onto, head)
		},
	})
	w.trigger(t, 1)
	held.waitPassed(t, 1)
	var last string
	for i := range 10 {
		files := map[string]string{"m.txt": fmt.Sprint(i)}
		if i == 9 {
			files = map[string]string{"a.txt": lineOneM}
		}
		last = commit(t, w.demo, "main", "main", fmt.Sprint("M", i), files)
		w.write(t, func(ctx context.Context, tx *sql.Tx) error {
			return w.changes.ScheduleTargetMergeable(ctx, tx, "demo", "main")
		})
		if got := w.read(t, 1).Mergeable; got != change.MergeableUnknown {
			t.Fatalf("after move %d mergeable = %s, want unknown", i, got)
		}
	}
	held.release()
	ch := w.settled(t, 1)
	if ch.Mergeable != change.MergeableConflict || conflictOf(t, ch)["target_sha"] != last {
		t.Errorf("mergeable, conflict = %s, %s; want conflict onto %s", ch.Mergeable, ch.Conflict, last)
	}
	if n := trials.Load(); n > 2 {
		t.Errorf("%d trial rebases ran, want at most 2", n)
	}
}

// fakeClock is a clock that moves only when the test advances it.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, fakeWaiter{c.now.Add(d), ch})
	return ch
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.waiters = slices.DeleteFunc(c.waiters, func(w fakeWaiter) bool {
		if c.now.Before(w.at) {
			return false
		}
		w.ch <- c.now
		return true
	})
}

// starts records, by change number, the time on clock each check started.
type starts struct {
	clock *fakeClock
	mu    sync.Mutex
	at    map[int64][]time.Time
}

func (s *starts) pass(_ context.Context, _ string, number int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.at[number] = append(s.at[number], s.clock.Now())
}

// wait waits until n checks of the change number started and returns
// their start times.
func (s *starts) wait(t *testing.T, number int64, n int) []time.Time {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		s.mu.Lock()
		got := slices.Clone(s.at[number])
		s.mu.Unlock()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("demo#%d had %d checks, want %d", number, len(got), n)
		}
	}
}

func TestMergeability_SpacesTwoChecksOfAChangeByTheDebounce(t *testing.T) {
	t.Parallel()
	adjusted := func(debounce time.Duration) hubtest.Config {
		return hubtest.Config{Compose: func(c *hub.Composition) {
			c.AdjustChange(func(d *change.Deps) { d.MergeableDebounce = debounce })
		}}
	}
	for _, tc := range []struct {
		name     string
		cfg      hubtest.Config
		debounce time.Duration
	}{
		{"200ms", adjusted(200 * time.Millisecond), 200 * time.Millisecond},
		{"0s", adjusted(0), 0},
		{"1m from the configuration file", hubtest.Config{TOML: "[changes]\nmergeable_debounce = \"1m\"\n"}, time.Minute},
	} {
		debounce := tc.debounce
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorldWith(t, tc.cfg)
			w.branch(t, "a", "A")
			w.openBranch(t, "a", nil)
			w.branch(t, "b", "B")
			w.openBranch(t, "b", nil)
			w.settled(t, 1)
			w.settled(t, 2)
			t0 := time.Now().Add(time.Hour)
			clock := &fakeClock{now: t0}
			s := &starts{clock: clock, at: map[int64][]time.Time{}}
			change.SetMergeableSeams(w.changes, change.MergeableSeams{Clock: clock, Gate: s.pass})

			w.trigger(t, 1)
			s.wait(t, 1, 1)
			clock.advance(10 * time.Millisecond)
			w.trigger(t, 1)
			clock.advance(10 * time.Millisecond)
			w.trigger(t, 2)
			if b := s.wait(t, 2, 1); b[0].Sub(t0) >= 100*time.Millisecond {
				t.Errorf("demo#2 started %v after t0, want within 100ms", b[0].Sub(t0))
			}
			if debounce > 0 {
				// Just short of the interval, which for a minute is well past
				// the default's.
				clock.advance(debounce - 40*time.Millisecond)
				w.idle(t, 2)
				if a := s.wait(t, 1, 1); len(a) != 1 {
					t.Fatalf("demo#1 started again at %v, inside its interval", a[1].Sub(t0))
				}
				clock.advance(50 * time.Millisecond)
			}
			a := s.wait(t, 1, 2)
			gap := a[1].Sub(a[0])
			if (debounce > 0 && gap < debounce) || (debounce == 0 && gap >= 50*time.Millisecond) {
				t.Errorf("two checks of demo#1 started %v apart with a debounce of %v", gap, debounce)
			}
		})
	}
}

func TestMergeability_LeavesUnknownWhileTheTargetBranchIsGone(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.branch(t, "b4", "B4")
	w.openBranch(t, "b4", func(in *change.OpenInput) { in.TargetBranch = new("release") })
	w.settled(t, 1)
	seq := w.seq(t)

	w.push(t, "release", "", nil)
	w.idle(t, 1)
	_, ch, _ := w.get(t, "/workspaces/demo/changes/1", w.ownerKey)
	landing, _ := ch["landing"].(map[string]any)
	if ch["state"] != "open" || ch["mergeable"] != "unknown" || ch["conflict"] != nil ||
		landing["status"] != "blocked" || landing["reason"] != "target_not_found" {
		t.Errorf("GET = %v, want open, unknown, no conflict, blocked by target_not_found", ch)
	}
	for _, e := range w.eventsAfter(t, seq) {
		if e.Type == "parley.change.conflict" {
			t.Errorf("event %s written", e.Type)
		}
	}
}

// failures are the log entries of failed mergeability checks of demo's
// change number.
func failures(t *testing.T, w *world, number int64) int {
	t.Helper()
	n := 0
	for line := range strings.Lines(w.Log.String()) {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) == nil && e["msg"] == "the mergeability check failed" &&
			e["workspace"] == "demo" && e["number"] == float64(number) {
			n++
		}
	}
	return n
}

func TestMergeability_LeavesUnknownWhenTheTrialRebaseFails(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		fail func(ctx context.Context, cancelled *atomic.Bool) error
	}{
		{"an error", func(context.Context, *atomic.Bool) error { return errors.New("git failed") }},
		{"past its deadline", func(ctx context.Context, cancelled *atomic.Bool) error {
			<-ctx.Done()
			cancelled.Store(true)
			return ctx.Err()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.branch(t, "a", "A")
			w.openBranch(t, "a", nil)
			w.branch(t, "b", "B")
			w.openBranch(t, "b", nil)
			w.settled(t, 1)
			w.settled(t, 2)
			var cancelled atomic.Bool
			change.SetMergeableSeams(w.changes, change.MergeableSeams{Deadline: 50 * time.Millisecond,
				Trial: func(ctx context.Context, _ *gitrepo.Repo, _, _ string) (*gitrepo.ConflictReport, error) {
					return nil, tc.fail(ctx, &cancelled)
				}})
			seq := w.seq(t)

			w.trigger(t, 1)
			w.idle(t, 1)
			if ch := w.read(t, 1); ch.Mergeable != change.MergeableUnknown || len(ch.Conflict) > 0 {
				t.Errorf("mergeable, conflict = %s, %s; want unknown and none", ch.Mergeable, ch.Conflict)
			}
			if tc.name == "past its deadline" && !cancelled.Load() {
				t.Error("the trial rebase past its deadline was not cancelled")
			}
			if n := failures(t, w, 1); n != 1 {
				t.Errorf("%d log entries of the failure, want 1:\n%s", n, w.Log.String())
			}
			if events := w.eventsAfter(t, seq); len(events) != 0 {
				t.Errorf("events %v written", events)
			}
			change.SetMergeableSeams(w.changes, change.MergeableSeams{})
			w.trigger(t, 2)
			if ch := w.settled(t, 2); ch.Mergeable != change.MergeableClean {
				t.Errorf("demo#2 is %s, want clean", ch.Mergeable)
			}
			w.trigger(t, 1)
			if ch := w.settled(t, 1); ch.Mergeable != change.MergeableClean {
				t.Errorf("demo#1 is %s at its next trigger, want clean", ch.Mergeable)
			}
		})
	}
}

func TestMergeability_LeavesEveryRefAsItWas(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		main  map[string]string
		trial func(ctx context.Context, repo *gitrepo.Repo, onto, head string) (*gitrepo.ConflictReport, error)
		want  string
	}{
		{"clean", map[string]string{"b.txt": "m\n"}, nil, change.MergeableClean},
		{"conflict", map[string]string{"a.txt": lineOneM}, nil, change.MergeableConflict},
		{"past its deadline", map[string]string{"a.txt": lineOneM}, func(ctx context.Context, repo *gitrepo.Repo, onto, head string) (*gitrepo.ConflictReport, error) {
			// The deadline may end git midway; the check fails at it either way.
			_, err := repo.TrialRebase(ctx, onto, head)
			<-ctx.Done()
			return nil, errors.Join(err, ctx.Err())
		}, change.MergeableUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			fork := w.addFork(t, "demo--t1", "main", nil, nil)
			commit(t, fork, "task", w.base, "T", map[string]string{"a.txt": lineOneX})
			w.openBranch(t, "task", func(in *change.OpenInput) { in.SourceWorkspace = "demo--t1" })
			w.settled(t, 1)
			commit(t, w.demo, "main", "main", "M", tc.main)
			if tc.trial != nil {
				change.SetMergeableSeams(w.changes, change.MergeableSeams{Trial: tc.trial, Deadline: 50 * time.Millisecond})
			}
			state := func() string {
				return gitrepotest.Git(t, w.demo, "for-each-ref") + gitrepotest.Git(t, fork, "for-each-ref") +
					gitrepotest.Git(t, w.demo, "worktree", "list") + gitrepotest.Git(t, fork, "worktree", "list")
			}
			before := state()

			w.trigger(t, 1)
			w.idle(t, 1)
			if got := w.read(t, 1).Mergeable; got != tc.want {
				t.Errorf("mergeable = %s, want %s", got, tc.want)
			}
			if after := state(); after != before {
				t.Errorf("refs and worktrees after the check:\n%s\nbefore:\n%s", after, before)
			}
		})
	}
}
