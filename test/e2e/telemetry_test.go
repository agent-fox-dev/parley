package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/telemetry"
)

// telemetryRun is the run of the telemetry flows.
const telemetryRun = "20260102_030405_a1b2c3"

// telemetryHub is a hub holding the ready workspace demo and its task fork
// demo--task1, owned by alice, who has an API key.
type telemetryHub struct {
	*hubtest.Hub
	comp            *hub.Composition
	alice, aliceKey string
}

// startTelemetryHub starts the hub and stores its user and workspaces.
func startTelemetryHub(t *testing.T) *telemetryHub {
	t.Helper()
	h := &telemetryHub{}
	h.Hub = hubtest.StartWith(t, hubtest.Config{Compose: func(c *hub.Composition) { h.comp = c }})
	acme := h.CreateOrg(t, "acme")
	h.alice = h.CreateUser(t, "alice")
	h.aliceKey = h.IssueKey(t, h.alice)
	h.AddWorkspace(t, access.Record{Slug: "demo", OwnerID: h.alice, OrgID: acme, Kind: access.KindRepository, Status: access.StatusReady})
	h.AddWorkspace(t, access.Record{Slug: "demo--task1", OwnerID: h.alice, OrgID: acme, Kind: access.KindFork, ParentSlug: "demo", Status: access.StatusReady})
	return h
}

// openSession opens a session of alice with body and returns its id.
func (h *telemetryHub) openSession(t *testing.T, body map[string]any) string {
	t.Helper()
	status, raw := h.Call(t, http.MethodPost, "/api/v1/sessions", h.aliceKey, body)
	if status != http.StatusCreated {
		t.Fatalf("POST /sessions %v = %d %s, want 201", body, status, raw)
	}
	var s struct {
		ID string `json:"id"`
	}
	decodeInto(t, raw, &s)
	return s.ID
}

// TestTelemetry runs the flows of agent telemetry on one hub, one after the
// other: each keeps to its own workspace.
func TestTelemetry(t *testing.T) {
	parallel(t)
	h := startTelemetryHub(t)
	t.Run("an agent pushes run telemetry and safely sends it twice", func(t *testing.T) { agentPushesTelemetryTwice(t, h) })
	t.Run("the hub records what was run in a sandbox of a run", func(t *testing.T) { hubRecordsSandboxUse(t, h) })
}

// agentPushesTelemetryTwice is an agent with a PAT holding audit:write that
// posts a run event, a batch of five trace records of which one is invalid
// and the run's postmortem to demo, loses the answers and sends all three
// again. An observer registered on the hub's telemetry component, as the
// metrics of the audit records are, is told the run event once.
func agentPushesTelemetryTwice(t *testing.T, h *telemetryHub) {
	var mu sync.Mutex
	var told []string
	h.comp.Telemetry().OnRunEvent(func(eventType string) {
		mu.Lock()
		defer mu.Unlock()
		told = append(told, eventType)
	})
	pat := h.IssuePAT(t, identity.PATSpec{UserID: h.alice, Permissions: []string{"audit:write"}})
	runPath := "/api/v1/workspaces/demo/runs/" + telemetryRun
	post := func(path string, body any) (int, []byte) {
		t.Helper()
		return h.Call(t, http.MethodPost, runPath+path, pat, body)
	}
	event := map[string]any{"id": "0a1b2c3d-0000-4000-8000-000000000001", "event_type": "run.start"}
	var traces []any
	for i, typ := range []string{"session.init", "assistant.message", "user.message", "tool.use", "session.result"} {
		traces = append(traces, map[string]any{"id": fmt.Sprintf("0a1b2c3d-0000-4000-8000-00000000010%d", i), "event_type": typ, "sequence": i})
	}
	postmortem := map[string]any{
		"run_status": "stalled", "started_at": "2026-01-02T03:00:00Z", "completed_at": "2026-01-02T04:00:00Z",
		"task_summary": map[string]any{"total": 4}, "cost_summary": map[string]any{},
	}
	type batchAnswer struct {
		Accepted   int `json:"accepted"`
		Duplicates int `json:"duplicates"`
		Errors     []struct {
			Index int `json:"index"`
		} `json:"errors"`
	}

	evStatus, ev := post("/events", event)
	if evStatus != http.StatusCreated {
		t.Fatalf("run event = %d %s, want 201", evStatus, ev)
	}
	bStatus, b := post("/traces/batch", traces)
	var first batchAnswer
	decodeInto(t, b, &first)
	if bStatus != http.StatusOK || first.Accepted != 4 || len(first.Errors) != 1 || first.Errors[0].Index != 2 {
		t.Fatalf("trace batch = %d %s, want 200 with accepted 4 and one error at index 2", bStatus, b)
	}
	pmStatus, pm := post("/postmortem", postmortem)
	if pmStatus != http.StatusCreated {
		t.Fatalf("postmortem = %d %s, want 201", pmStatus, pm)
	}

	if status, again := post("/events", event); status != http.StatusOK || !bytes.Equal(again, ev) {
		t.Errorf("run event again = %d %s, want 200 %s", status, again, ev)
	}
	status, b2 := post("/traces/batch", traces)
	var second batchAnswer
	decodeInto(t, b2, &second)
	if status != http.StatusOK || second.Accepted != 0 || second.Duplicates != 4 || len(second.Errors) != 1 || second.Errors[0].Index != 2 {
		t.Errorf("trace batch again = %d %s, want 200 with accepted 0, duplicates 4 and one error at index 2", status, b2)
	}
	if status, again := post("/postmortem", postmortem); status != http.StatusOK || !bytes.Equal(again, pm) {
		t.Errorf("postmortem again = %d %s, want 200 %s", status, again, pm)
	}

	for table, want := range map[string]int{"run_events": 1, "traces": 4, "postmortems": 1} {
		var n int
		err := h.Store.Analytics().QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM `+table+` WHERE workspace = ? AND run_id = ?`, "demo", telemetryRun).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("%s of demo and the run = %d, want %d", table, n, want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(told, []string{"run.start"}) {
		t.Errorf("the run event observer was told %v, want [run.start]", told)
	}
}

// hubRecordsSandboxUse is the hub recording an exec in the sandbox of a
// session of a run as a run event of demo--task1, and nothing for one of a
// session without a run, without a hub event.
func hubRecordsSandboxUse(t *testing.T, h *telemetryHub) {
	withRun := h.openSession(t, map[string]any{"workspace_slug": "demo--task1", "run_id": telemetryRun})
	withoutRun := h.openSession(t, map[string]any{"workspace_slug": "demo--task1"})
	hubEvents := func() int {
		var n int
		if err := h.Store.Main().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM hub_events`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := hubEvents()
	started := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	exit := 0
	execIn := func(session string) telemetry.SandboxExec {
		return telemetry.SandboxExec{
			SandboxUse: telemetry.SandboxUse{
				Workspace: "demo--task1", SandboxID: "sbx1", SessionID: session, ActorType: event.ActorAPIKey, ActorID: h.alice, StartedAt: started,
			},
			Command: []string{"go", "test"}, ExitCode: &exit, DurationMS: 900,
		}
	}

	h.comp.Telemetry().RecordSandboxUse(context.Background(), execIn(withRun))

	rows, err := h.Store.Analytics().QueryContext(context.Background(),
		`SELECT event_type, session_id, timestamp, payload FROM run_events WHERE workspace = ? AND run_id = ?`, "demo--task1", telemetryRun)
	if err != nil {
		t.Fatal(err)
	}
	type runEvent struct{ eventType, session, timestamp, payload string }
	var got []runEvent
	for rows.Next() {
		var e runEvent
		if err := rows.Scan(&e.eventType, &e.session, &e.timestamp, &e.payload); err != nil {
			t.Fatal(err)
		}
		got = append(got, e)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("run events of demo--task1 and the run = %v, want one", got)
	}
	if e := got[0]; e.eventType != "sandbox.exec" || e.session != withRun || e.timestamp != "2026-01-02T03:00:00.000000Z" {
		t.Errorf("run event = %+v, want sandbox.exec of session %s at the start of the exec", e, withRun)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(got[0].payload), &payload); err != nil {
		t.Fatalf("payload %s: %v", got[0].payload, err)
	}
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"actor_id", "actor_type", "command", "duration_ms", "exit_code", "sandbox_id", "timed_out"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("payload members = %v, want %v", keys, want)
	}

	h.comp.Telemetry().RecordSandboxUse(context.Background(), execIn(withoutRun))

	var all int
	if err := h.Store.Analytics().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM run_events WHERE workspace = ?`, "demo--task1").Scan(&all); err != nil {
		t.Fatal(err)
	}
	if all != 1 {
		t.Errorf("run events of demo--task1 = %d after an exec of a session without a run, want 1", all)
	}
	if after := hubEvents(); after != before {
		t.Errorf("hub events = %d after the two reports, want %d as before", after, before)
	}
}
