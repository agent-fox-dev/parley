package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recorder logs task calls and checks that no two tasks run at once.
type recorder struct {
	mu      sync.Mutex
	calls   []string
	running atomic.Int32
	overlap atomic.Bool
}

func (r *recorder) task(name string, then func(ctx context.Context) error) Func {
	return func(ctx context.Context) error {
		if r.running.Add(1) > 1 {
			r.overlap.Store(true)
		}
		defer r.running.Add(-1)
		r.mu.Lock()
		r.calls = append(r.calls, name)
		r.mu.Unlock()
		if then != nil {
			return then(ctx)
		}
		return nil
	}
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *recorder) count(name string) int {
	n := 0
	for _, c := range r.list() {
		if c == name {
			n++
		}
	}
	return n
}

// lockedBuffer is a bytes.Buffer safe for concurrent use.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func jsonLogger() (*slog.Logger, *lockedBuffer) {
	buf := &lockedBuffer{}
	return slog.New(slog.NewJSONHandler(buf, nil)), buf
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// startLoop runs the loop in a goroutine and returns its tick channel and a
// channel closed when Run returns.
func startLoop(t *testing.T, ctx context.Context, reg *Registry, logger *slog.Logger) (chan time.Time, chan struct{}) {
	t.Helper()
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		New(reg, logger).Run(ctx, ticks)
		close(done)
	}()
	return ticks, done
}

func TestLoop_RunsAPassAtOnceAndOneAtEachTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &recorder{}
	reg := NewRegistry()
	if err := reg.Register("retention", rec.task("retention", nil)); err != nil {
		t.Fatal(err)
	}
	logger, _ := jsonLogger()
	ticks, _ := startLoop(t, ctx, reg, logger)
	eventually(t, "the first pass", func() bool { return rec.count("retention") == 1 })
	ticks <- time.Now()
	eventually(t, "the second pass", func() bool { return rec.count("retention") == 2 })
	ticks <- time.Now()
	eventually(t, "the third pass", func() bool { return rec.count("retention") == 3 })
	if Interval != time.Hour {
		t.Errorf("Interval = %v, want 1 h", Interval)
	}
}

func TestLoop_RunsTasksInTheFixedOrder(t *testing.T) {
	eight := []string{"retention", "secret_reencryption", "claim_cleanup", "forge_delivery_pruning", "token_binding_sweep", "patch_purge", "fork_expiry", "sandbox_record_cleanup"}
	reversed := make([]string, len(eight))
	for i, n := range eight {
		reversed[len(eight)-1-i] = n
	}
	tests := []struct {
		register []string
		want     []string
	}{
		{reversed, eight},
		{[]string{"fork_expiry", "extra_b", "retention", "extra_a"}, []string{"retention", "fork_expiry", "extra_b", "extra_a"}},
		{[]string{"extra_a"}, []string{"extra_a"}},
		{nil, nil},
	}
	for _, tt := range tests {
		ctx, cancel := context.WithCancel(context.Background())
		rec := &recorder{}
		reg := NewRegistry()
		for _, n := range tt.register {
			if err := reg.Register(n, rec.task(n, nil)); err != nil {
				t.Fatal(err)
			}
		}
		logger, _ := jsonLogger()
		startLoop(t, ctx, reg, logger)
		eventually(t, "one pass", func() bool { return len(rec.list()) == len(tt.want) })
		time.Sleep(10 * time.Millisecond)
		if got := rec.list(); strings.Join(got, ",") != strings.Join(tt.want, ",") {
			t.Errorf("registered %v: pass ran %v, want %v", tt.register, got, tt.want)
		}
		if rec.overlap.Load() {
			t.Error("two tasks ran at once")
		}
		cancel()
	}
}

func TestLoop_AFailingTaskIsLoggedAndTheRestRun(t *testing.T) {
	failures := []struct {
		name string
		fail func(context.Context) error
		want string
	}{
		{"error", func(context.Context) error { return errors.New("db gone https://u:p@h/x") }, "db gone https://h/x"},
		{"panic", func(context.Context) error { panic("kaboom") }, "kaboom"},
	}
	for _, f := range failures {
		t.Run(f.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rec := &recorder{}
			reg := NewRegistry()
			_ = reg.Register("retention", rec.task("retention", nil))
			_ = reg.Register("claim_cleanup", rec.task("claim_cleanup", f.fail))
			_ = reg.Register("fork_expiry", rec.task("fork_expiry", nil))
			logger, buf := jsonLogger()
			ticks, _ := startLoop(t, ctx, reg, logger)
			eventually(t, "the pass", func() bool { return rec.count("fork_expiry") == 1 })
			if got := rec.list(); strings.Join(got, ",") != "retention,claim_cleanup,fork_expiry" {
				t.Errorf("pass = %v", got)
			}
			var rec1 map[string]any
			for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
				var r map[string]any
				if json.Unmarshal([]byte(l), &r) == nil && r["level"] == "ERROR" {
					rec1 = r
				}
			}
			if rec1 == nil || rec1["task"] != "claim_cleanup" {
				t.Fatalf("no ERROR line for claim_cleanup in %s", buf.String())
			}
			errText, _ := rec1["error"].(string)
			if !strings.Contains(errText, f.want) || strings.Contains(errText, "u:p") {
				t.Errorf("error = %q", errText)
			}
			ticks <- time.Now()
			eventually(t, "the next pass", func() bool { return rec.count("claim_cleanup") == 2 })
		})
	}
}

func TestLoop_NeverRunsTwoPassesAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls, running, highest atomic.Int32
	release := make(chan struct{})
	reg := NewRegistry()
	_ = reg.Register("retention", func(ctx context.Context) error {
		calls.Add(1)
		n := running.Add(1)
		for {
			h := highest.Load()
			if n <= h || highest.CompareAndSwap(h, n) {
				break
			}
		}
		defer running.Add(-1)
		if calls.Load() == 1 {
			<-release
		}
		return nil
	})
	logger, _ := jsonLogger()
	ticks := make(chan time.Time, 2)
	go New(reg, logger).Run(ctx, ticks)
	eventually(t, "pass 1", func() bool { return calls.Load() == 1 })
	ticks <- time.Now()
	ticks <- time.Now()
	time.Sleep(20 * time.Millisecond)
	close(release)
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("calls = %d after ticks during a pass, want 1", calls.Load())
	}
	ticks <- time.Now()
	eventually(t, "pass 2", func() bool { return calls.Load() == 2 })
	if highest.Load() != 1 {
		t.Errorf("%d passes ran at once", highest.Load())
	}
}

func TestRegistry_RefusesAnEmptyOrRepeatedName(t *testing.T) {
	for _, name := range []string{"retention", "extra", ""} {
		reg := NewRegistry()
		firstCalled, secondCalled := false, false
		if name != "" {
			if err := reg.Register(name, func(context.Context) error { firstCalled = true; return nil }); err != nil {
				t.Fatal(err)
			}
		}
		err := reg.Register(name, func(context.Context) error { secondCalled = true; return nil })
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("second registration of %q: %v", name, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		logger, _ := jsonLogger()
		_, done := startLoop(t, ctx, reg, logger)
		time.Sleep(20 * time.Millisecond)
		cancel()
		<-done
		if secondCalled || (name != "" && !firstCalled) {
			t.Errorf("%q: first called %v, second called %v", name, firstCalled, secondCalled)
		}
	}
}

func TestLoop_CancellationStopsThePass(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := &recorder{}
	started := make(chan struct{})
	reg := NewRegistry()
	_ = reg.Register("retention", rec.task("retention", func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}))
	_ = reg.Register("fork_expiry", rec.task("fork_expiry", nil))
	logger, buf := jsonLogger()
	ticks, done := startLoop(t, ctx, reg, logger)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("the first task did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return within 1 s")
	}
	select {
	case ticks <- time.Now():
		t.Error("a tick after the cancellation was received")
	case <-time.After(20 * time.Millisecond):
	}
	if rec.count("fork_expiry") != 0 {
		t.Error("a task after the cancelled one ran")
	}
	if strings.Contains(buf.String(), "ERROR") {
		t.Errorf("a cancelled task was logged as failed: %s", buf.String())
	}
}
