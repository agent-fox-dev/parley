package sandbox_test

import (
	"context"
	"database/sql"
	"maps"
	"testing"
	"time"

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

	if got, want := h.rec.values(sandbox.CreatesMetric), map[string]float64{"result=ready": 2, "result=failed": 1}; !maps.Equal(got, want) {
		t.Errorf("parley_sandbox_creates_total = %v; want %v", got, want)
	}
	if got := h.rec.values(sandbox.SandboxesMetric); !maps.Equal(got, stateSeries(0, 2, 1, 0, 1)) {
		t.Errorf("parley_sandboxes = %v; want %v, and no series for deleted", got, stateSeries(0, 2, 1, 0, 1))
	}
}
