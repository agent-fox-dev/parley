package land_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/land"
)

func TestPool_RunsAtMostWorkersAttemptsAtOnce(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// workers is [land].workers, 0 for the default; submitted is how
		// many attempts are handed to the pool at once.
		workers, submitted, want int
	}{
		{"two workers", 2, 5, 2},
		{"the default", 0, 9, 8},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t, func(d *land.Deps) {
				if c.workers > 0 {
					d.Workers = c.workers
				}
			})
			var mu sync.Mutex
			inside, most, seen := 0, 0, map[string]bool{}
			entered, release := make(chan struct{}, c.submitted), make(chan struct{})
			land.SetSeams(w.land, land.Seams{BeforeSwap: func(_ context.Context, id string, _ int) {
				mu.Lock()
				inside++
				most = max(most, inside)
				if !seen[id] {
					seen[id] = true
					entered <- struct{}{}
				}
				mu.Unlock()
				<-release
				mu.Lock()
				inside--
				mu.Unlock()
			}})
			var changes []change.Change
			for i := range c.submitted {
				changes = append(changes, w.open(t, "f"+strconv.Itoa(i)))
			}
			var attempts []land.Attempt
			for _, ch := range changes {
				attempts = append(attempts, w.submit(t, ch, time.Now().UTC()))
			}

			for range c.want {
				select {
				case <-entered:
				case <-time.After(30 * time.Second):
					t.Fatalf("fewer than %d attempts started within 30 s", c.want)
				}
			}
			for _, a := range attempts {
				if status, got := w.get(t, "/workspaces/api/lands/"+a.ID, w.ownerKey); status != 200 || got["status"] != land.StatusRunning {
					t.Errorf("GET attempt %s = %d, status %v; want 200 running", a.ID, status, got["status"])
				}
			}
			close(release)
			for _, a := range attempts {
				if got := w.ended(t, a.ID); got.Status != land.StatusLanded {
					t.Errorf("attempt %s ended %s (%v), want landed", a.ID, got.Status, got.Error)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if most != c.want {
				t.Errorf("%d attempts ran at once, want %d", most, c.want)
			}
		})
	}
}

func TestPool_EndsAnAttemptPastItsDeadlineAsTimeout(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// timeout is [land].timeout, 0 for the default, and age how long
		// before their submission the attempts were created.
		timeout, age time.Duration
		// second hands a second attempt to the pool, which waits for its
		// one worker; hang has the replay run a merge whose command never
		// ends, in place of parking the attempt before the swap.
		second, hang bool
	}{
		{name: "parked before the swap, a second attempt waiting for the worker", timeout: time.Second, second: true},
		{name: "under the default timeout, created five minutes ago", age: 5*time.Minute - time.Second},
		{name: "in a git command of the replay", timeout: time.Second, hang: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			pidFile, attributes := filepath.Join(dir, "pid"), filepath.Join(dir, "attributes")
			if err := os.WriteFile(attributes, []byte("* merge=hang\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var w *world
			w = newWorld(t, func(d *land.Deps) {
				d.Workers = 1
				if c.timeout > 0 {
					d.Timeout = c.timeout
				}
				if c.hang {
					// Every content merge of the land's replay runs the
					// merge driver hang, which writes its process id and
					// sleeps.
					git := gitrepo.Git{Env: []string{"GIT_CONFIG_COUNT=2",
						"GIT_CONFIG_KEY_0=merge.hang.driver", "GIT_CONFIG_VALUE_0=echo $$ > " + pidFile + "; exec sleep 60",
						"GIT_CONFIG_KEY_1=core.attributesFile", "GIT_CONFIG_VALUE_1=" + attributes}}
					d.Repos = func(ctx context.Context, slug string) (*gitrepo.Repo, error) {
						return git.Open(ctx, w.workspaces.RepoPath(slug))
					}
				}
			})
			var mu sync.Mutex
			entered := 0
			land.SetSeams(w.land, land.Seams{BeforeSwap: func(context.Context, string, int) {
				mu.Lock()
				entered++
				mu.Unlock()
				time.Sleep(2 * time.Second)
			}})
			var changes []change.Change
			main := w.base
			if c.hang {
				// The change and main both edit a.txt, so its replay
				// merges the file's content.
				commit(t, w.api, "feature", w.base, "x1", map[string]string{"a.txt": "x1\nl2\nl3\n"})
				changes = append(changes, w.openBranch(t, "feature", w.caller()))
				main = commit(t, w.api, "main", w.base, "y3", map[string]string{"a.txt": "l1\nl2\ny3\n"})
			} else {
				changes = append(changes, w.open(t, "first"))
				if c.second {
					changes = append(changes, w.open(t, "second"))
				}
			}
			created := time.Now().UTC().Add(-c.age)
			start := time.Now()
			var attempts []land.Attempt
			for _, ch := range changes {
				attempts = append(attempts, w.submit(t, ch, created))
			}

			for _, a := range attempts {
				got := w.ended(t, a.ID)
				if took := time.Since(start); took >= 2*time.Second {
					t.Errorf("attempt %s ended after %v, want less than 2 s", a.ID, took)
				}
				if got.Status != land.StatusFailed || got.Error == nil || *got.Error != land.ErrorTimeout ||
					got.FinishedAt == nil || got.LandedSHA != nil {
					t.Errorf("attempt %s = %s (%v), finished %v, landed %v; want failed timeout, nothing landed",
						a.ID, got.Status, got.Error, got.FinishedAt, got.LandedSHA)
				}
			}
			if c.hang {
				raw, err := os.ReadFile(pidFile)
				if err != nil {
					t.Fatalf("the replay never ran the merge driver: %v", err)
				}
				pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
				if err != nil {
					t.Fatal(err)
				}
				for deadline := time.Now().Add(10 * time.Second); !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH); {
					if time.Now().After(deadline) {
						t.Fatalf("the merge driver's process %d outlived the attempt's deadline by 10 s", pid)
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			mu.Lock()
			if !c.hang && entered != 1 {
				t.Errorf("%d attempts reached the swap, want the one with the worker", entered)
			}
			mu.Unlock()

			// The restart waits for the parked run to return, which finds
			// its deadline passed before the swap.
			w.Restart(t)
			if got := head(t, w.api, "main"); got != main {
				t.Errorf("main is at %s, want it unmoved at %s", got, main)
			}
			for i, a := range attempts {
				if got := w.attempt(t, a.ID); got.Status != land.StatusFailed || got.Error == nil || *got.Error != land.ErrorTimeout {
					t.Errorf("attempt %s after the restart = %s (%v), want failed timeout", a.ID, got.Status, got.Error)
				}
				if got := w.readChange(t, changes[i].Number).State; got != change.StateOpen {
					t.Errorf("change %d is %s, want open", changes[i].Number, got)
				}
			}
			if n, err := land.Journalled(context.Background(), w.Store); err != nil || n != 0 {
				t.Errorf("land journal holds %d entries (%v), want none", n, err)
			}
		})
	}
}

// slowRefWriter is the hub's ref writer taking delay more after each move
// of a ref, as a move whose bookkeeping is slow.
type slowRefWriter struct {
	land.RefWriter
	delay time.Duration
}

func (s slowRefWriter) Apply(ctx context.Context, req pushrule.ApplyRequest) (pushrule.Result, error) {
	res, err := s.RefWriter.Apply(ctx, req)
	time.Sleep(s.delay)
	return res, err
}

func TestPool_RecordsALandWhoseDeadlinePassedWhileTheTargetMoved(t *testing.T) {
	t.Parallel()
	w := newWorld(t, func(d *land.Deps) {
		d.Timeout = time.Second
		d.RefWriter = slowRefWriter{d.RefWriter, 2 * time.Second}
	})
	ch := w.open(t, "feature")

	got := w.ended(t, w.submit(t, ch, time.Now().UTC()).ID)

	if got.Status != land.StatusLanded || got.LandedSHA == nil || head(t, w.api, "main") != *got.LandedSHA {
		t.Fatalf("attempt = %s (%v), landed %v, main at %s; want landed with main at its landed head",
			got.Status, got.Error, got.LandedSHA, head(t, w.api, "main"))
	}
	if state := w.readChange(t, ch.Number).State; state != change.StateLanded {
		t.Errorf("the change is %s, want landed", state)
	}
}

func TestPool_LeavesARunningAttemptRunningAtTheStop(t *testing.T) {
	t.Parallel()
	// The pool of the second start runs nothing, so that what the stop
	// left is what the test reads.
	paused := false
	w := newWorld(t, func(d *land.Deps) { d.Paused = paused })
	ch := w.open(t, "feature")
	parked := make(chan struct{})
	land.SetSeams(w.land, land.Seams{BeforeSwap: func(ctx context.Context, _ string, _ int) {
		close(parked)
		<-ctx.Done()
	}})
	a := w.submit(t, ch, time.Now().UTC())
	select {
	case <-parked:
	case <-time.After(30 * time.Second):
		t.Fatal("the attempt did not reach the swap within 30 s")
	}

	paused = true
	w.Restart(t)

	if got := w.attempt(t, a.ID); got.Status != land.StatusRunning || got.Error != nil || got.FinishedAt != nil {
		t.Errorf("attempt after the stop = %s (%v), finished %v; want running", got.Status, got.Error, got.FinishedAt)
	}
	if got := head(t, w.api, "main"); got != w.base {
		t.Errorf("main is at %s, want it unmoved at %s", got, w.base)
	}
	if got := w.readChange(t, ch.Number).State; got != change.StateOpen {
		t.Errorf("the change is %s, want open", got)
	}
}
