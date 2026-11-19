package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/metrics"
)

// Step is one retention step: a clean-up in its own unit of work, which
// returns the error that made it fail.
type Step func(context.Context) error

// stepNames are the retention steps in the order a run takes them. Sessions
// that stayed active too long are timed out first, so that the session step
// of the same run may delete them once they are old enough; the record
// gauges are counted last, from what the other steps left.
var stepNames = []string{
	"stale_sessions",
	"run_records",
	"max_runs",
	"traces",
	"postmortems",
	"sessions",
	"event_log",
	"webhook_deliveries",
	"hub_events",
	"orphans",
	"gauges",
}

// StepNames returns the names of the retention steps in the order a run
// takes them.
func StepNames() []string { return slices.Clone(stepNames) }

// namedStep is a step with its name.
type namedStep struct {
	name string
	fn   Step
}

// Retention is the retention run: the steps in their fixed order, the
// recorder that counts their failures and the time of the last run that had
// none, the logger of the failures and the clock.
type Retention struct {
	steps  []namedStep
	rec    metrics.Recorder
	logger *slog.Logger
	now    func() time.Time
}

// NewRetention returns the retention run over steps, which binds every name
// of StepNames to its function. It refuses a name StepNames does not hold
// and a name with no function, so that no step is left out by mistake. A nil
// recorder records nothing, a nil logger discards the log and a nil clock is
// time.Now.
func NewRetention(steps map[string]Step, rec metrics.Recorder, logger *slog.Logger, now func() time.Time) (*Retention, error) {
	var errs []error
	for name := range steps {
		if !slices.Contains(stepNames, name) {
			errs = append(errs, fmt.Errorf("retention step %q is not one of %s", name, strings.Join(stepNames, ", ")))
		}
	}
	r := &Retention{rec: rec, logger: logger, now: now}
	for _, name := range stepNames {
		fn := steps[name]
		if fn == nil {
			errs = append(errs, fmt.Errorf("retention step %q has no function", name))
		}
		r.steps = append(r.steps, namedStep{name, fn})
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if r.rec == nil {
		r.rec = metrics.Discard
	}
	if r.logger == nil {
		r.logger = slog.New(slog.DiscardHandler)
	}
	if r.now == nil {
		r.now = time.Now
	}
	return r, nil
}

// Run runs the steps once each, one after the other. A step that fails or
// panics is logged with its name and counted in
// parley_retention_errors_total, and the next step runs all the same: the
// steps are independent clean-ups. When every step succeeded, Run sets
// parley_retention_last_run_timestamp_seconds to the clock's time; when one
// failed, it leaves the timestamp and returns an error naming the failed
// steps.
//
// Once ctx is cancelled, as at the hub's stop, no further step starts and
// Run returns the context's error; a step that ends with that error is not
// counted as failed, nor are the steps that did not start.
func (r *Retention) Run(ctx context.Context) error {
	var failed []string
	for _, s := range r.steps {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := runStep(ctx, s.fn)
		if err == nil {
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
			return ctxErr
		}
		r.logger.Error("retention step failed", "step", s.name, "error", err.Error())
		// The step's name is in the metric's closed set, so the report is
		// never refused.
		_ = r.rec.Add(metrics.RetentionErrorsTotal, map[string]string{"step": s.name}, 1)
		failed = append(failed, s.name)
	}
	if len(failed) > 0 {
		return fmt.Errorf("retention steps failed: %s", strings.Join(failed, ", "))
	}
	_ = r.rec.Set(metrics.RetentionLastRunTimestamp, nil, float64(r.now().Unix()))
	return nil
}

// runStep calls fn and turns a panic into its error, so that a step that
// panics fails like one that returns an error.
func runStep(ctx context.Context, fn Step) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return fn(ctx)
}
