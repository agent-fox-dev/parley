package audit_test

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/audit"
	"github.com/agent-fox-dev/parley/internal/metrics"
)

// gathered is every series of the metric name in reg, by its label values
// joined with ",".
func gathered(reg *metrics.Registry, name string) map[string]float64 {
	out := map[string]float64{}
	for _, f := range reg.Gather(context.Background()) {
		if f.Name == name {
			for _, s := range f.Series {
				out[strings.Join(s.LabelValues, ",")] = s.Value
			}
		}
	}
	return out
}

func TestGauges_CountTheStoredRecordsAndTheActiveSessionsOfEachWorkspace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStores(t)
	at := retentionNow.Add(-time.Hour)
	for range 3 {
		s.hubEvent(t, "ws1", at)
	}
	for table, n := range map[string]int{"run_events": 2, "session_outcomes": 1, "tool_errors": 4, "traces": 5} {
		for i := range n {
			s.record(t, table, fmt.Sprintf("%s-%d", table, i), "ws1", "20260101_000000_000001", at)
		}
	}
	s.postmortem(t, "postmortem", "ws1", "20260101_000000_000001", at, at)
	s.session(t, "ws1-a", "ws1", "active", at)
	s.session(t, "ws1-b", "ws1", "active", at)
	s.session(t, "ws2-a", "ws2", "active", at)
	for i := range 6 {
		s.usage(t, fmt.Sprintf("usage-%d", i), "ws1-a", "ws1", at)
	}

	reg := metrics.New(nil)
	step := audit.GaugeSteps([]audit.RecordCounter{s.events, s.telemetry, s.sessions}, s.sessions, reg)["gauges"]
	if err := step(ctx); err != nil {
		t.Fatalf("gauges = %v", err)
	}
	wantRecords := map[string]float64{
		"hub_event": 3, "run_event": 2, "outcome": 1, "tool_call": 0, "tool_error": 4, "trace": 5,
		"postmortem": 1, "session": 3, "usage": 6,
	}
	if got := gathered(reg, metrics.AuditRecords); !maps.Equal(got, wantRecords) {
		t.Errorf("%s = %v, want %v", metrics.AuditRecords, got, wantRecords)
	}
	if got, want := gathered(reg, metrics.AgentSessionsActive), map[string]float64{"ws1": 2, "ws2": 1}; !maps.Equal(got, want) {
		t.Errorf("%s = %v, want %v", metrics.AgentSessionsActive, got, want)
	}

	exec(t, s.st.Main(), `UPDATE sessions SET status = 'completed', ended_at = ? WHERE id = 'ws2-a'`,
		retentionNow.Format(time.RFC3339))
	if err := step(ctx); err != nil {
		t.Fatalf("second gauges = %v", err)
	}
	if got, want := gathered(reg, metrics.AgentSessionsActive), map[string]float64{"ws1": 2}; !maps.Equal(got, want) {
		t.Errorf("%s after ws2's session ended = %v, want %v", metrics.AgentSessionsActive, got, want)
	}
}
