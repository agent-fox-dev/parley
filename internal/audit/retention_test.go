package audit_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/audit"
	"github.com/agent-fox-dev/parley/internal/metrics"
)

// stepOrder is the documented order of the retention steps.
var stepOrder = []string{
	"stale_sessions", "run_records", "max_runs", "traces", "postmortems", "sessions",
	"event_log", "webhook_deliveries", "hub_events", "orphans", "gauges",
}

// runAt is the time the retention tests' clock starts at.
var runAt = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

// retentionRig is a retention run whose steps write their names to a log,
// reporting to a registry and logging to a buffer, on a settable clock.
type retentionRig struct {
	run   *audit.Retention
	reg   *metrics.Registry
	clock *clock
	logs  bytes.Buffer

	mu    sync.Mutex
	steps []string
}

// newRetentionRig builds the rig; behave, when it names a step, is what
// that step does once it has written its name.
func newRetentionRig(t *testing.T, behave map[string]audit.Step) *retentionRig {
	t.Helper()
	rig := &retentionRig{reg: metrics.New(nil), clock: &clock{now: runAt}}
	steps := map[string]audit.Step{}
	for _, name := range stepOrder {
		steps[name] = func(ctx context.Context) error {
			rig.mu.Lock()
			rig.steps = append(rig.steps, name)
			rig.mu.Unlock()
			if fn := behave[name]; fn != nil {
				return fn(ctx)
			}
			return nil
		}
	}
	run, err := audit.NewRetention(steps, rig.reg, slog.New(slog.NewJSONHandler(&rig.logs, nil)), rig.clock.Now)
	if err != nil {
		t.Fatalf("NewRetention: %v", err)
	}
	rig.run = run
	return rig
}

// ran is the names of the steps that ran, in order, and empties the log.
func (rig *retentionRig) ran() []string {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	out := rig.steps
	rig.steps = nil
	return out
}

// value is the value of the series of metric name with the label values.
func (rig *retentionRig) value(t *testing.T, name string, labelValues ...string) float64 {
	t.Helper()
	for _, f := range rig.reg.Gather(t.Context()) {
		if f.Name != name {
			continue
		}
		for _, s := range f.Series {
			if slices.Equal(s.LabelValues, labelValues) {
				return s.Value
			}
		}
	}
	t.Fatalf("no series %s%v", name, labelValues)
	return 0
}

// lastRun is the last-run timestamp gauge.
func (rig *retentionRig) lastRun(t *testing.T) float64 {
	t.Helper()
	return rig.value(t, metrics.RetentionLastRunTimestamp)
}

// errorsOf is the retention error counter of step.
func (rig *retentionRig) errorsOf(t *testing.T, step string) float64 {
	t.Helper()
	return rig.value(t, metrics.RetentionErrorsTotal, step)
}

func TestRetention_RunsTheElevenStepsOnceEachInOrder(t *testing.T) {
	t.Parallel()
	rig := newRetentionRig(t, nil)
	if err := rig.run.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := rig.ran(); !slices.Equal(got, stepOrder) {
		t.Errorf("steps ran = %v, want %v", got, stepOrder)
	}
	if got := audit.StepNames(); !slices.Equal(got, stepOrder) {
		t.Errorf("StepNames = %v, want %v", got, stepOrder)
	}
}

func TestRetention_SetsTheLastRunTimeWhenEveryStepSucceeded(t *testing.T) {
	t.Parallel()
	rig := newRetentionRig(t, nil)
	if got := rig.lastRun(t); got != 0 {
		t.Fatalf("last run before any run = %v, want 0", got)
	}
	for _, at := range []time.Time{runAt, runAt.Add(time.Hour)} {
		rig.clock.set(at)
		if err := rig.run.Run(t.Context()); err != nil {
			t.Fatalf("Run at %s: %v", at, err)
		}
		if got := rig.lastRun(t); got != float64(at.Unix()) {
			t.Errorf("last run after the run at %s = %v, want %d", at, got, at.Unix())
		}
	}
	for _, step := range stepOrder {
		if got := rig.errorsOf(t, step); got != 0 {
			t.Errorf("errors of %s = %v, want 0", step, got)
		}
	}
}

func TestRetention_CountsAndLogsAFailedStepAndRunsTheOthers(t *testing.T) {
	t.Parallel()
	fail := func(context.Context) error { return errors.New("the store is gone") }
	for _, tc := range []struct {
		name   string
		failed map[string]audit.Step
	}{
		{"the first step fails", map[string]audit.Step{"stale_sessions": fail}},
		{"a middle step fails", map[string]audit.Step{"sessions": fail}},
		{"the last step fails", map[string]audit.Step{"gauges": fail}},
		{"two steps fail", map[string]audit.Step{"run_records": fail, "hub_events": fail}},
		{"a step panics", map[string]audit.Step{"event_log": func(context.Context) error { panic("boom") }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			failing := true
			behave := map[string]audit.Step{}
			for name, fn := range tc.failed {
				behave[name] = func(ctx context.Context) error {
					if failing {
						return fn(ctx)
					}
					return nil
				}
			}
			rig := newRetentionRig(t, behave)
			failing = false
			rig.clock.set(runAt.Add(-time.Hour))
			if err := rig.run.Run(t.Context()); err != nil {
				t.Fatalf("the first run failed: %v", err)
			}
			rig.ran()
			rig.logs.Reset()
			before := rig.lastRun(t)

			failing = true
			rig.clock.set(runAt)
			err := rig.run.Run(t.Context())
			if err == nil {
				t.Fatalf("Run succeeded with failing steps")
			}
			if got := rig.ran(); !slices.Equal(got, stepOrder) {
				t.Errorf("steps ran = %v, want all eleven in order", got)
			}
			lines := strings.Split(strings.TrimSpace(rig.logs.String()), "\n")
			if len(lines) != len(tc.failed) {
				t.Errorf("log has %d lines, want one per failed step: %q", len(lines), rig.logs.String())
			}
			for _, step := range stepOrder {
				_, failed := tc.failed[step]
				want := 0.0
				if failed {
					want = 1
					if !strings.Contains(err.Error(), step) {
						t.Errorf("error %q does not name %s", err, step)
					}
					if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, `"step":"`+step+`"`) }) {
						t.Errorf("no log line names %s: %q", step, rig.logs.String())
					}
				}
				if got := rig.errorsOf(t, step); got != want {
					t.Errorf("errors of %s = %v, want %v", step, got, want)
				}
			}
			if got := rig.lastRun(t); got != before {
				t.Errorf("last run = %v, want it unchanged at %v", got, before)
			}
		})
	}
}

func TestRetention_StopsAtCancellationWithoutCountingAnything(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rig := newRetentionRig(t, map[string]audit.Step{"max_runs": func(ctx context.Context) error {
		cancel()
		return ctx.Err()
	}})
	err := rig.run.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want the context's error", err)
	}
	if got := rig.ran(); !slices.Equal(got, stepOrder[:3]) {
		t.Errorf("steps ran = %v, want %v", got, stepOrder[:3])
	}
	for _, step := range stepOrder {
		if got := rig.errorsOf(t, step); got != 0 {
			t.Errorf("errors of %s = %v, want 0", step, got)
		}
	}
	if got := rig.lastRun(t); got != 0 {
		t.Errorf("last run = %v, want it unchanged at 0", got)
	}
}
