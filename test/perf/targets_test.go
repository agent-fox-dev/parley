package perf_test

import (
	"os"
	"testing"

	"github.com/agent-fox-dev/parley/test/perf"
)

// TestPerformanceTargets measures the performance targets against the hub
// that PARLEY_PERF_URL names, and is skipped without it. It fails only when
// the harness cannot do its job; a missed target, a skipped scenario and a
// scenario that ended in error are what the report is for.
func TestPerformanceTargets(t *testing.T) {
	cfg, skip, err := perf.ConfigFromEnv(os.LookupEnv)
	if skip != "" {
		t.Skip(skip)
	}
	if err != nil {
		t.Fatal(err)
	}
	rep, err := perf.Run(t.Context(), cfg)
	if rep.Results != nil {
		t.Log("\n" + rep.Table())
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("report written to %s", cfg.ReportPath)
}
