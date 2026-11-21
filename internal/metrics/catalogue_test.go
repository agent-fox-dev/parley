package metrics_test

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/metrics"
)

// label is a documented label: values lists a closed label's set, nil marks
// an open label.
type label struct {
	name   string
	values []string
}

func closed(name string, values ...string) label { return label{name: name, values: values} }
func open(name string) label                     { return label{name: name} }

var (
	httpBuckets        = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	attemptBuckets     = []float64{1, 2, 3, 5, 8, 13}
	landBuckets        = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
	sandboxBuckets     = []float64{60, 300, 600, 1200, 1800, 3600, 7200, 14400, 28800, 86400}
	retentionStepNames = []string{"stale_sessions", "run_records", "max_runs", "traces", "postmortems", "sessions",
		"event_log", "webhook_deliveries", "hub_events", "orphans", "gauges"}
)

// documented is the catalogue as the operator documentation states it, in
// its order.
var documented = []struct {
	name    string
	typ     metrics.Type
	labels  []label
	buckets []float64
}{
	{"parley_http_requests_total", metrics.Counter, []label{open("method"), open("path"), open("status")}, nil},
	{"parley_http_request_duration_seconds", metrics.Histogram, []label{open("method"), open("path")}, httpBuckets},
	{"parley_git_requests_total", metrics.Counter, []label{
		closed("service", "info-refs", "upload-pack", "receive-pack"), closed("result", "ok", "refused", "error")}, nil},
	{"parley_scm_requests_total", metrics.Counter, []label{
		closed("op", "refs", "commits", "tree", "file", "diff", "compare", "blame", "search", "outline",
			"code_index", "commit", "ref_create", "ref_move", "ref_delete", "rebase"),
		closed("result", "ok", "ref_moved", "refused", "error")}, nil},
	{"parley_changes_open", metrics.Gauge, []label{open("workspace")}, nil},
	{"parley_lands_total", metrics.Counter, []label{closed("result", "landed", "conflict", "blocked", "failed")}, nil},
	{"parley_land_attempts", metrics.Histogram, nil, attemptBuckets},
	{"parley_land_duration_seconds", metrics.Histogram, nil, landBuckets},
	{"parley_forks_active", metrics.Gauge, nil, nil},
	{"parley_events_seq", metrics.Gauge, nil, nil},
	{"parley_sse_connections", metrics.Gauge, nil, nil},
	{"parley_webhook_deliveries_total", metrics.Counter, []label{closed("result", "ok", "retry", "failed")}, nil},
	{"parley_webhook_queue_depth", metrics.Gauge, nil, nil},
	{"parley_audit_events_total", metrics.Counter, []label{closed("source", "hub", "agent"), open("type")}, nil},
	{"parley_agent_sessions_active", metrics.Gauge, []label{open("workspace")}, nil},
	{"parley_agent_tokens_total", metrics.Counter, []label{
		open("workspace"), open("model"), closed("direction", "input", "output", "cache_read")}, nil},
	{"parley_audit_records", metrics.Gauge, []label{closed("kind", "hub_event", "run_event", "outcome", "tool_call",
		"tool_error", "trace", "postmortem", "session", "usage")}, nil},
	{"parley_jobqueue_depth", metrics.Gauge, []label{closed("type", "rebuild"),
		closed("status", "queued", "running", "failed", "completed", "dead_letter", "cancelled")}, nil},
	{"parley_retention_errors_total", metrics.Counter, []label{closed("step", retentionStepNames...)}, nil},
	{"parley_retention_last_run_timestamp_seconds", metrics.Gauge, nil, nil},
	{"parley_sandboxes", metrics.Gauge, []label{closed("state", "provisioning", "ready", "stopped", "completed", "failed")}, nil},
	{"parley_sandbox_creates_total", metrics.Counter, []label{closed("result", "ready", "failed")}, nil},
	{"parley_sandbox_seconds", metrics.Histogram, nil, sandboxBuckets},
	{"parley_sandbox_exec_total", metrics.Counter, []label{closed("result", "ok", "nonzero", "timeout", "error")}, nil},
	{"parley_sandbox_tool_calls_total", metrics.Counter, []label{
		open("tool"), closed("outcome", "ok", "tool_error", "timeout", "failed")}, nil},
	{"parley_sandbox_tool_call_duration_seconds", metrics.Histogram, []label{open("tool")},
		[]float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300, 600}},
	{"parley_sandbox_denials_total", metrics.Counter, nil, nil},
	{"parley_sandbox_checkpoints_total", metrics.Counter, []label{closed("result", "ok", "empty", "failed")}, nil},
	{"parley_sandbox_runtime_up", metrics.Gauge, nil, nil},
	{"parley_code_index_builds_total", metrics.Counter, []label{closed("outcome", "ok", "partial", "failed")}, nil},
	{"parley_code_index_build_duration_seconds", metrics.Histogram, nil, []float64{0.1, 0.5, 1, 5, 10, 30, 60, 120, 300, 600}},
	{"parley_code_index_bytes", metrics.Gauge, nil, nil},
}

// zeroSeries is what a fresh registry exports for a documented metric: one
// zero series per combination of its closed values, open labels empty.
func zeroSeries(labels []label, typ metrics.Type, buckets []float64) map[string]float64 {
	combos := [][]string{{}}
	for _, l := range labels {
		values := l.values
		if values == nil {
			values = []string{""}
		}
		var next [][]string
		for _, c := range combos {
			for _, v := range values {
				next = append(next, append(slices.Clone(c), l.name+"="+v))
			}
		}
		combos = next
	}
	out := map[string]float64{}
	for _, c := range combos {
		key := strings.Join(c, ",")
		if typ != metrics.Histogram {
			out[key] = 0
			continue
		}
		maps.Copy(out, histogram(key, buckets, make([]float64, len(buckets)), 0, 0))
	}
	return out
}

func TestNew_ExportsEveryCatalogueMetricAtZero(t *testing.T) {
	families := metrics.New(nil).Gather(context.Background())
	if len(families) != len(documented) {
		t.Fatalf("a fresh registry exports %d families, want %d", len(families), len(documented))
	}
	for i, m := range documented {
		t.Run(m.name, func(t *testing.T) {
			f := families[i]
			if f.Name != m.name || f.Type != m.typ || f.Help == "" {
				t.Fatalf("family %d is %q %s (help %q), want %q %s with a help text", i, f.Name, f.Type, f.Help, m.name, m.typ)
			}
			if !slices.Equal(f.Buckets, m.buckets) {
				t.Errorf("buckets = %v, want %v", f.Buckets, m.buckets)
			}
			if got, want := samples(f), zeroSeries(m.labels, m.typ, m.buckets); !maps.Equal(got, want) {
				t.Errorf("samples = %v, want %v", got, want)
			}
		})
	}
}
