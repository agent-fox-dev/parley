package audit

import (
	"context"
	"maps"
	"slices"

	"github.com/agent-fox-dev/parley/internal/metrics"
)

// recordKinds are the kinds of record parley_audit_records counts, each
// exported, at 0 when none is stored.
var recordKinds = []string{
	"hub_event", "run_event", "outcome", "tool_call", "tool_error", "trace", "postmortem", "session", "usage",
}

// RecordCounter is a store of audit or session records as the gauges step
// counts them: *event.Component, *telemetry.Component and
// *session.Component satisfy it.
type RecordCounter interface {
	// CountRecords returns the number of stored records of each kind the
	// store keeps, by record kind.
	CountRecords(ctx context.Context) (map[string]int64, error)
}

// ActiveSessions counts the active sessions; *session.Component satisfies
// it.
type ActiveSessions interface {
	// ActiveCounts returns the number of active sessions of each workspace
	// that has any.
	ActiveCounts(ctx context.Context) (map[string]int64, error)
}

// GaugeSteps returns the step gauges, which sets parley_audit_records of
// every record kind to the number the counters report, and replaces the
// whole of parley_agent_sessions_active with one series per workspace that
// has an active session, so that a workspace whose sessions all ended has
// none. A counter's error or a report rec refuses is the step's.
func GaugeSteps(counters []RecordCounter, active ActiveSessions, rec metrics.Recorder) map[string]Step {
	return map[string]Step{
		"gauges": func(ctx context.Context) error {
			counts := make(map[string]int64, len(recordKinds))
			for _, c := range counters {
				some, err := c.CountRecords(ctx)
				if err != nil {
					return err
				}
				maps.Copy(counts, some)
			}
			for _, kind := range recordKinds {
				if err := rec.Set(metrics.AuditRecords, map[string]string{"kind": kind}, float64(counts[kind])); err != nil {
					return err
				}
			}
			byWorkspace, err := active.ActiveCounts(ctx)
			if err != nil {
				return err
			}
			samples := make([]metrics.Sample, 0, len(byWorkspace))
			for _, slug := range slices.Sorted(maps.Keys(byWorkspace)) {
				samples = append(samples, metrics.Sample{
					Labels: map[string]string{"workspace": slug}, Value: float64(byWorkspace[slug]),
				})
			}
			return rec.SetFamily(metrics.AgentSessionsActive, samples)
		},
	}
}
