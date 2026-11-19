package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
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

func TestTelemetry_TheHubRecordsWhatWasRunInASandboxOfARun(t *testing.T) {
	parallel(t)
	h := startTelemetryHub(t)
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
	if err := h.Store.Analytics().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM run_events`).Scan(&all); err != nil {
		t.Fatal(err)
	}
	if all != 1 {
		t.Errorf("run events = %d after an exec of a session without a run, want 1", all)
	}
	if after := hubEvents(); after != before {
		t.Errorf("hub events = %d after the two reports, want %d as before", after, before)
	}
}
