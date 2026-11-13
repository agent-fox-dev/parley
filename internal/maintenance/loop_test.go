package maintenance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/maintenance"
)

// fixedOrder is the order in which a pass runs the tasks it knows by name.
var fixedOrder = []string{
	"retention", "secret_reencryption", "claim_cleanup", "forge_delivery_pruning",
	"token_binding_sweep", "patch_purge", "fork_expiry", "sandbox_record_cleanup",
}

func TestLoop_RunsAPassAtOnceAndOneAtEachTick(t *testing.T) {
	t.Parallel()
	reg := maintenance.NewRegistry()
	var count atomic.Int32
	mustRegister(t, reg, "retention", func(context.Context) error {
		count.Add(1)
		return nil
	})
	ticks := make(chan time.Time)
	startLoop(t, maintenance.New(reg, discardLogger()), ticks)

	eventually(t, "the first pass runs without a tick", func() bool { return count.Load() == 1 })
	for want := int32(2); want <= 3; want++ {
		sendTick(t, ticks)
		eventually(t, "a tick starts one pass", func() bool { return count.Load() == want })
	}
	if maintenance.Interval != time.Hour {
		t.Errorf("Interval = %v, want 1h", maintenance.Interval)
	}
}

func TestLoop_PassRunsTheTasksOneAtATimeInTheFixedOrderThenTheOthers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		registered []string
		want       []string
	}{
		{"the eight names reversed", reversed(fixedOrder), fixedOrder},
		{"known and other names mixed", []string{"fork_expiry", "extra_b", "retention", "extra_a"},
			[]string{"retention", "fork_expiry", "extra_b", "extra_a"}},
		{"another name alone", []string{"extra_a"}, []string{"extra_a"}},
		{"no task", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := maintenance.NewRegistry()
			var log callLog
			var running atomic.Int32
			for _, name := range tc.registered {
				mustRegister(t, reg, name, func(context.Context) error {
					if n := running.Add(1); n != 1 {
						t.Errorf("%s runs while %d other tasks run", name, n-1)
					}
					defer running.Add(-1)
					time.Sleep(time.Millisecond)
					log.add(name)
					return nil
				})
			}

			maintenance.New(reg, discardLogger()).Pass(context.Background())

			if got := log.calls(); !slices.Equal(got, tc.want) {
				t.Errorf("calls = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoop_LogsAFailingTaskAndGoesOn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		fail      func() error
		wantError string
	}{
		{"error", func() error { return errors.New("db gone https://u:p@h/x") }, "db gone https://h/x"},
		{"panic", func() error { panic("kaboom") }, "kaboom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := maintenance.NewRegistry()
			var log callLog
			for _, name := range []string{"retention", "claim_cleanup", "fork_expiry"} {
				mustRegister(t, reg, name, func(context.Context) error {
					log.add(name)
					if name == "claim_cleanup" {
						return tc.fail()
					}
					return nil
				})
			}
			var out syncBuffer
			ticks := make(chan time.Time)
			startLoop(t, maintenance.New(reg, slog.New(slog.NewJSONHandler(&out, nil))), ticks)

			eventually(t, "the first pass ends", func() bool { return len(log.calls()) == 3 })
			if got := log.calls(); !slices.Equal(got, []string{"retention", "claim_cleanup", "fork_expiry"}) {
				t.Errorf("calls = %v, want [retention claim_cleanup fork_expiry]", got)
			}
			errs := out.recordsAt(t, "ERROR")
			if len(errs) != 1 {
				t.Fatalf("%d ERROR lines, want 1: %q", len(errs), out.String())
			}
			rec := errs[0]
			if rec["task"] != "claim_cleanup" {
				t.Errorf("task = %v, want claim_cleanup", rec["task"])
			}
			text, _ := rec["error"].(string)
			if !strings.Contains(text, tc.wantError) {
				t.Errorf("error = %q, want it to contain %q", text, tc.wantError)
			}
			if strings.Contains(text, "u:p") {
				t.Errorf("error = %q holds the URL's user information", text)
			}

			sendTick(t, ticks)
			eventually(t, "the next pass calls the failing task again", func() bool {
				return countOf(log.calls(), "claim_cleanup") == 2
			})
		})
	}
}

func TestLoop_DropsATickThatArrivesWhileAPassRuns(t *testing.T) {
	t.Parallel()
	reg := maintenance.NewRegistry()
	var calls, running, highest atomic.Int32
	release := make(chan struct{})
	mustRegister(t, reg, "retention", func(ctx context.Context) error {
		calls.Add(1)
		n := running.Add(1)
		defer running.Add(-1)
		for {
			h := highest.Load()
			if n <= h || highest.CompareAndSwap(h, n) {
				break
			}
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	ticks := make(chan time.Time)
	startLoop(t, maintenance.New(reg, discardLogger()), ticks)

	eventually(t, "the first pass starts", func() bool { return calls.Load() == 1 })
	sendTick(t, ticks)
	sendTick(t, ticks)
	close(release)
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Errorf("calls after ticks during the pass = %d, want 1", n)
	}
	sendTick(t, ticks)
	eventually(t, "a tick after the pass starts the next", func() bool { return calls.Load() == 2 })
	if h := highest.Load(); h != 1 {
		t.Errorf("highest number of concurrent calls = %d, want 1", h)
	}
}

func TestRegistry_RefusesAnEmptyOrRepeatedNameAndKeepsTheFirstTask(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		first bool // whether a first task is registered under name
		fn2   bool // whether the refused registration has a function
	}{
		{"retention", true, true},
		{"extra", true, true},
		{"", false, true},
		{"no_function", false, false},
	} {
		t.Run("name "+tc.name, func(t *testing.T) {
			t.Parallel()
			reg := maintenance.NewRegistry()
			var first, second atomic.Int32
			if tc.first {
				mustRegister(t, reg, tc.name, func(context.Context) error {
					first.Add(1)
					return nil
				})
			}
			var fn2 maintenance.Task
			if tc.fn2 {
				fn2 = func(context.Context) error {
					second.Add(1)
					return nil
				}
			}

			err := reg.Register(tc.name, fn2)
			if err == nil {
				t.Fatalf("Register(%q) = nil, want an error", tc.name)
			}
			if !strings.Contains(err.Error(), tc.name) {
				t.Errorf("error %q does not name %q", err, tc.name)
			}
			maintenance.New(reg, discardLogger()).Pass(context.Background())
			if second.Load() != 0 {
				t.Error("the refused task was called")
			}
			if tc.first && first.Load() != 1 {
				t.Errorf("the first task was called %d times, want 1", first.Load())
			}
		})
	}
}

func TestLoop_StopsAtTheRunningTaskWhenCancelled(t *testing.T) {
	t.Parallel()
	reg := maintenance.NewRegistry()
	var log callLog
	started := make(chan struct{})
	mustRegister(t, reg, "retention", func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		log.add("retention")
		return ctx.Err()
	})
	mustRegister(t, reg, "fork_expiry", func(context.Context) error {
		log.add("fork_expiry")
		return nil
	})
	var out syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	returned := make(chan struct{})
	go func() {
		maintenance.New(reg, slog.New(slog.NewJSONHandler(&out, nil))).Run(ctx, ticks)
		close(returned)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("retention did not start within 5 s")
	}
	cancel()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("Run did not return within 1 s of the cancellation")
	}
	if got := log.calls(); !slices.Equal(got, []string{"retention"}) {
		t.Errorf("calls = %v, want [retention]: the running task sees its context done, no later task runs", got)
	}
	if errs := out.recordsAt(t, "ERROR"); len(errs) != 0 {
		t.Errorf("ERROR lines for a task ended by the cancellation: %q", out.String())
	}
	select {
	case ticks <- time.Now():
		t.Error("a tick after the cancellation was received")
	case <-time.After(20 * time.Millisecond):
	}
}

// startLoop runs loop with ticks until the test ends, and waits for Run to
// return then.
func startLoop(t *testing.T, loop *maintenance.Loop, ticks <-chan time.Time) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan struct{})
	go func() {
		loop.Run(ctx, ticks)
		close(returned)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-returned:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return within 5 s of the test's end")
		}
	})
}

// sendTick sends a tick and fails the test when the loop does not take it
// within a second.
func sendTick(t *testing.T, ticks chan<- time.Time) {
	t.Helper()
	select {
	case ticks <- time.Now():
	case <-time.After(time.Second):
		t.Fatal("the loop did not take a tick within 1 s")
	}
}

func mustRegister(t *testing.T, reg *maintenance.Registry, name string, fn maintenance.Task) {
	t.Helper()
	if err := reg.Register(name, fn); err != nil {
		t.Fatalf("Register(%q): %v", name, err)
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func reversed(s []string) []string {
	r := slices.Clone(s)
	slices.Reverse(r)
	return r
}

func countOf(s []string, v string) int {
	n := 0
	for _, x := range s {
		if x == v {
			n++
		}
	}
	return n
}

// callLog records calls in order, from any goroutine.
type callLog struct {
	mu    sync.Mutex
	names []string
}

func (l *callLog) add(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.names = append(l.names, name)
}

func (l *callLog) calls() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.names)
}

// syncBuffer collects log lines written from any goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// recordsAt decodes the lines written so far and returns those at level.
func (b *syncBuffer) recordsAt(t *testing.T, level string) []map[string]any {
	t.Helper()
	var recs []map[string]any
	for line := range strings.SplitSeq(b.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %q is not a JSON object: %v", line, err)
		}
		if rec["level"] == level {
			recs = append(recs, rec)
		}
	}
	return recs
}

// eventually polls cond every few milliseconds for up to 5 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 5 s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
