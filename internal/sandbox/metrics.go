package sandbox

import (
	"context"
	"fmt"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// Recorder is what the sandbox packages report their metrics to: Add, Set
// and Observe with a metric name, a label map and a value, each returning an
// error, the method set of the hub's metrics registry, so that the registry
// is passed as it is. It is runtime.Recorder under the feature's name, so
// that one value serves the runtime client, which reports
// parley_sandbox_runtime_up on every change of its reachability flag, and
// the rest of the feature.
type Recorder = runtime.Recorder

// NopRecorder records nothing and returns no error. It is the recorder of a
// hub that has none.
type NopRecorder = runtime.NopRecorder

// The metrics of the stored sandboxes.
const (
	// CreatesMetric counts the provisionings of created sandboxes that
	// ended, under the label result: ready or failed.
	CreatesMetric = "parley_sandbox_creates_total"
	// SandboxesMetric is the number of stored sandboxes in each state, under
	// the label state.
	SandboxesMetric = "parley_sandboxes"
)

// reportedStates are the states SandboxesMetric has a series for: every
// state but deleted, which a sandbox keeps only until it is purged.
var reportedStates = []string{StateProvisioning, StateReady, StateStopped, StateCompleted, StateFailed}

// ReportStates sets SandboxesMetric for each reported state, 0 included,
// from the counts of the sandboxes stored in q, the store or the caller's
// transaction. An insert and an applied transition report them in their
// own transaction; a caller that removes a record reports them in its
// transaction after Remove.
func (s *Service) ReportStates(ctx context.Context, q access.Querier) error {
	rows, err := q.QueryContext(ctx, `SELECT state, count(*) FROM sandboxes GROUP BY state`)
	if err != nil {
		return fmt.Errorf("count the sandboxes by state: %w", err)
	}
	defer func() { _ = rows.Close() }()
	counts := map[string]float64{}
	for rows.Next() {
		var (
			state string
			n     float64
		)
		if err := rows.Scan(&state, &n); err != nil {
			return fmt.Errorf("count the sandboxes by state: %w", err)
		}
		counts[state] = n
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("count the sandboxes by state: %w", err)
	}
	for _, state := range reportedStates {
		_ = s.deps.Recorder.Set(SandboxesMetric, map[string]string{"state": state}, counts[state])
	}
	return nil
}

// reportStates is ReportStates for a change that has been written: a
// failure to count leaves the metric as it was and the change as it is.
func (s *Service) reportStates(ctx context.Context, q access.Querier) {
	_ = s.ReportStates(ctx, q)
}

// countCreate adds one ended provisioning with result to CreatesMetric.
func (s *Service) countCreate(result string) {
	_ = s.deps.Recorder.Add(CreatesMetric, map[string]string{"result": result}, 1)
}
