package sandbox_test

import (
	"context"
	"database/sql"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hubtest"

	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// stateSeries are the series of parley_sandboxes for the counts of the five
// states a sandbox shows while it is stored, by label key.
func stateSeries(provisioning, ready, stopped, completed, failed float64) map[string]float64 {
	return map[string]float64{
		"state=provisioning": provisioning, "state=ready": ready, "state=stopped": stopped,
		"state=completed": completed, "state=failed": failed,
	}
}

func TestMetrics_CreatesAndStates(t *testing.T) {
	t.Parallel()
	h := startProvisionHub(t, agentProfile)
	h.world(t)
	if got := h.rec.values(sandbox.SandboxesMetric); !maps.Equal(got, stateSeries(0, 0, 0, 0, 0)) {
		t.Errorf("parley_sandboxes at start = %v; want the five states at 0", got)
	}

	// Records stored in other states count from the next report on.
	for _, state := range []string{sandbox.StateStopped, sandbox.StateDeleted} {
		ended := recordStart.Add(-time.Hour)
		rec := sandbox.Record{
			ID: sandbox.NewID(), Workspace: "demo", Profile: "agent", State: state, ExpiresAt: recordStart.Add(time.Hour),
			LastActivityAt: ended, CreatedBy: sandbox.Creator{ActorType: "api_key", ActorID: h.dana, Username: "dana"},
			CreatedAt: ended, UpdatedAt: ended, EndedAt: &ended, UserID: h.dana, FamilyRoot: "demo",
		}
		h.insert(t, func(ctx context.Context, tx *sql.Tx) error { return sandbox.Insert(ctx, tx, rec) })
	}
	provision := func(want string) {
		t.Helper()
		id := sandboxObject(t, h.post(t, "demo--t1", map[string]any{}))["id"].(string)
		if rec := h.settled(t, id); rec.State != want {
			t.Fatalf("sandbox = %s %q; want %s", rec.State, rec.StateReason, want)
		}
	}
	provision(sandbox.StateReady)
	provision(sandbox.StateReady)
	h.rt.FailOperation(runtime.OpCreateSandbox, runtime.ErrInvalid, "no room")
	provision(sandbox.StateFailed)
	h.rt.FailOperation(runtime.OpCreateSandbox, nil, "")

	// A provisioning is counted once its ending transaction has committed,
	// so the record can show the state before the count is made.
	hubtest.Eventually(t, "the three provisionings to be counted", func() bool {
		var n float64
		for _, v := range h.rec.values(sandbox.CreatesMetric) {
			n += v
		}
		return n >= 3
	})
	if got, want := h.rec.values(sandbox.CreatesMetric), map[string]float64{"result=ready": 2, "result=failed": 1}; !maps.Equal(got, want) {
		t.Errorf("parley_sandbox_creates_total = %v; want %v", got, want)
	}
	if got := h.rec.values(sandbox.SandboxesMetric); !maps.Equal(got, stateSeries(0, 2, 1, 0, 1)) {
		t.Errorf("parley_sandboxes = %v; want %v, and no series for deleted", got, stateSeries(0, 2, 1, 0, 1))
	}
}

func TestMetrics_CountsToolCallsByToolAndOutcome(t *testing.T) {
	t.Parallel()
	h := startToolsHub(t, "")
	id := h.tooled(t, nil)
	if got := h.toolCallSamples(t); len(got) != 0 {
		t.Fatalf("before any call the counter has the samples %v; want none", got)
	}
	decoded(t, h.tool(t, id, h.key, "read_file", nil), http.StatusOK)
	decoded(t, h.tool(t, id, h.key, "read_file", nil), http.StatusOK)
	h.tools.SetCall(printing(`{"ok":false,"error":"not_found","detail":"no such file"}`, "", 0))
	decoded(t, h.tool(t, id, h.key, "read_file", nil), http.StatusOK)
	toolResultOf(t, h.mcpCall(t, id, h.memberKey, "search_files", nil))
	h.tools.SetCall(printing("", "boom", 1))
	if a := h.tool(t, id, h.key, "read_file", nil); a.status != http.StatusBadGateway {
		t.Fatalf("the call of a tool server that exits 1 = %d %s; want 502", a.status, a.body)
	}
	_, waiting, _ := h.held()
	answered := h.sending(context.Background(), http.MethodPost, toolPath(id, "read_file"), h.key, map[string]any{"timeout": "10s"}, nil)
	hubtest.Eventually(t, "the call to run", func() bool { return waiting.Load() == 1 })
	h.clock.Advance(10 * time.Second)
	receive(t, answered)

	want := []string{
		`tool="read_file",outcome="failed" 1`, `tool="read_file",outcome="ok" 2`, `tool="read_file",outcome="timeout" 1`,
		`tool="read_file",outcome="tool_error" 1`, `tool="search_files",outcome="tool_error" 1`,
	}
	if got := h.toolCallSamples(t); !slices.Equal(got, want) {
		t.Errorf("parley_sandbox_tool_calls_total has the samples\n%v\nwant\n%v", got, want)
	}
	_, body := h.Call(t, http.MethodGet, "/metrics", h.Token, nil)
	for _, line := range []string{
		`parley_sandbox_tool_call_duration_seconds_count{tool="read_file"} 5`,
		`parley_sandbox_tool_call_duration_seconds_count{tool="search_files"} 1`,
		`parley_sandbox_tool_call_duration_seconds_sum{tool="read_file"} 10`,
	} {
		if !strings.Contains(string(body), line+"\n") {
			t.Errorf("the hub's metrics lack the line %s", line)
		}
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "parley_sandbox_exec_total{") && !strings.HasSuffix(line, " 0") {
			t.Errorf("the tool calls were counted as execs: %s", line)
		}
	}
}
