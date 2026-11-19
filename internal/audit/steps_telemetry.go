package audit

import (
	"context"
	"time"
)

// TelemetryStore is the store of run telemetry as the telemetry steps reach
// it; *telemetry.Component satisfies it. Each operation runs in its own
// transaction and returns the number of records it deleted.
type TelemetryStore interface {
	// DeleteRunRecordsBefore deletes the run events, session outcomes,
	// tool calls and tool errors whose timestamp is before cutoff.
	DeleteRunRecordsBefore(ctx context.Context, cutoff time.Time) (int64, error)
	// DeleteTracesBefore deletes the trace records whose timestamp is
	// before cutoff.
	DeleteTracesBefore(ctx context.Context, cutoff time.Time) (int64, error)
	// DeletePostmortemsBefore deletes the postmortems ingested before
	// cutoff.
	DeletePostmortemsBefore(ctx context.Context, cutoff time.Time) (int64, error)
	// KeepNewestRuns deletes the records of every workspace's oldest runs
	// but the newest n, postmortems excepted.
	KeepNewestRuns(ctx context.Context, n int) (int64, error)
}

// TelemetrySteps returns the steps run_records, max_runs, traces and
// postmortems over the store with the limits of settings: the first, third
// and fourth delete the records older than [audit].max_age,
// [audit].trace_max_age and [audit].postmortem_max_age, the second keeps
// the records of the newest [audit].max_runs runs of each workspace. Each
// step reads clock once; the store's error is the step's.
func TelemetrySteps(s TelemetryStore, settings Settings, clock func() time.Time) map[string]Step {
	before := func(del func(context.Context, time.Time) (int64, error), age time.Duration) Step {
		return func(ctx context.Context) error {
			_, err := del(ctx, clock().Add(-age))
			return err
		}
	}
	return map[string]Step{
		"run_records": before(s.DeleteRunRecordsBefore, settings.MaxAge),
		"max_runs": func(ctx context.Context) error {
			_, err := s.KeepNewestRuns(ctx, settings.MaxRuns)
			return err
		},
		"traces":      before(s.DeleteTracesBefore, settings.TraceMaxAge),
		"postmortems": before(s.DeletePostmortemsBefore, settings.PostmortemMaxAge),
	}
}
