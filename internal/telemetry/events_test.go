package telemetry_test

import (
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// generatedID is the form of an id the hub generates: a version 4 UUID in
// lower case.
var generatedID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestRunEvents runs the behaviours of POST …/events on one hub, alice
// posting to demo, each subtest on runs of its own.
func TestRunEvents(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	// post sends body to /events of a new run of demo as alice.
	post := func(t *testing.T, body any) (int, []byte) {
		t.Helper()
		return w.post(t, w.aliceKey, demo, w.freshRun(), "/events", body)
	}
	// created posts body and returns the run event the hub answered 201 with.
	created := func(t *testing.T, body any) map[string]any {
		t.Helper()
		status, raw := post(t, body)
		if status != http.StatusCreated {
			t.Fatalf("POST /events %v = %d %s, want 201", body, status, raw)
		}
		return decode(t, raw)
	}
	runEventsOf := func(t *testing.T, run string) int {
		t.Helper()
		return w.count(t, `SELECT COUNT(*) FROM run_events WHERE workspace = ? AND run_id = ?`, demo, run)
	}

	t.Run("a new run event is stored and answered with every member", func(t *testing.T) {
		run := w.freshRun()
		sent := map[string]any{
			"id": "0a1b2c3d-0000-4000-8000-000000000001", "event_type": "run.start", "severity": "warning",
			"node_id": "n1", "session_id": "0a1b2c3d-0000-4000-8000-0000000000aa",
			"timestamp": "2026-01-02T03:00:00Z", "payload": map[string]any{"a": map[string]any{"b": 1.0}},
		}
		status, raw := w.post(t, w.aliceKey, demo, run, "/events", sent)
		if status != http.StatusCreated {
			t.Fatalf("POST /events = %d %s, want 201", status, raw)
		}
		want := map[string]any{"workspace": demo, "run_id": run, "ingested_at": "2026-01-02T03:04:05Z"}
		for k, v := range sent {
			want[k] = v
		}
		if got := decode(t, raw); !reflect.DeepEqual(got, want) {
			t.Errorf("answer = %v, want %v", got, want)
		}

		status, raw = w.post(t, w.aliceKey, demo, run, "/events", map[string]any{"event_type": "run.start"})
		if status != http.StatusCreated {
			t.Fatalf("POST /events = %d %s, want 201", status, raw)
		}
		got := decode(t, raw)
		for _, member := range []string{"node_id", "session_id", "payload"} {
			if v, ok := got[member]; !ok || v != nil {
				t.Errorf("%s = %v (present %v), want null", member, v, ok)
			}
		}
		if n := runEventsOf(t, run); n != 2 {
			t.Errorf("run events of the run = %d, want 2", n)
		}
	})

	t.Run("the hub generates an absent id and takes its time for an absent timestamp", func(t *testing.T) {
		a := created(t, map[string]any{"event_type": "x"})
		b := created(t, map[string]any{"event_type": "x"})
		id, _ := a["id"].(string)
		if !generatedID.MatchString(id) {
			t.Errorf("id = %q, want a version 4 UUID in lower case", id)
		}
		if a["id"] == b["id"] {
			t.Errorf("two requests got the same id %v", a["id"])
		}
		if a["timestamp"] != "2026-01-02T03:04:05Z" || a["timestamp"] != a["ingested_at"] {
			t.Errorf("timestamp = %v, ingested_at = %v, want both 2026-01-02T03:04:05Z", a["timestamp"], a["ingested_at"])
		}
	})

	t.Run("an absent severity follows the event type and a sent one is kept", func(t *testing.T) {
		cases := []struct{ eventType, want string }{
			{"session.fail", "error"},
			{"run.limit_reached", "warning"},
			{"git.conflict", "warning"},
			{"harvest.empty", "warning"},
			{"review.parse_failure", "warning"},
			{"run.start", "info"},
			{"session.failed", "info"},
			{"sandbox.exec", "info"},
		}
		for _, c := range cases {
			if got := created(t, map[string]any{"event_type": c.eventType})["severity"]; got != c.want {
				t.Errorf("severity of %s = %v, want %s", c.eventType, got, c.want)
			}
		}
		if got := created(t, map[string]any{"event_type": "session.fail", "severity": "critical"})["severity"]; got != "critical" {
			t.Errorf("sent severity critical answered as %v", got)
		}
	})

	t.Run("ids are answered in lower case and timestamps in UTC", func(t *testing.T) {
		got := created(t, map[string]any{
			"id": "AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE", "session_id": "Bbbbbbbb-CCCC-4ddd-8EEE-ffffffffffff",
			"event_type": "x", "timestamp": "2026-01-02T05:04:05.1234567+02:00",
		})
		want := map[string]string{
			"id":         "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
			"session_id": "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff",
			"timestamp":  "2026-01-02T03:04:05.123456Z",
		}
		for member, v := range want {
			if got[member] != v {
				t.Errorf("%s = %v, want %s", member, got[member], v)
			}
		}
	})

	t.Run("a body that breaks a member's rule is refused naming the member", func(t *testing.T) {
		cases := []struct {
			body   any
			member string
		}{
			{map[string]any{}, "event_type"},
			{map[string]any{"event_type": ""}, "event_type"},
			{map[string]any{"event_type": 7}, "event_type"},
			{map[string]any{"event_type": "x", "severity": "fatal"}, "severity"},
			{map[string]any{"event_type": "x", "id": "not-a-uuid"}, "id"},
			{map[string]any{"event_type": "x", "id": "0123456789abcdef0123456789abcdef"}, "id"},
			{map[string]any{"event_type": "x", "session_id": "abc"}, "session_id"},
			{map[string]any{"event_type": "x", "timestamp": "2026-01-02 03:04:05"}, "timestamp"},
			{map[string]any{"event_type": "x", "timestamp": 1767323045}, "timestamp"},
			{map[string]any{"event_type": "x", "timestamp": "9999-12-31T23:59:59-01:00"}, "timestamp"},
			{map[string]any{"event_type": "x", "timestamp": "2026-01-02T03:04:05,5Z"}, "timestamp"},
			{map[string]any{"event_type": "x", "node_id": strings.Repeat("n", 256)}, "node_id"},
			{map[string]any{"event_type": "x", "payload": []int{1}}, "payload"},
			{map[string]any{"event_type": "x", "payload": "text"}, "payload"},
			{"[1]", ""},
			{"null", ""},
		}
		before := w.countAllRecords(t)
		for _, c := range cases {
			status, raw := post(t, c.body)
			typ, msg := errorOf(t, raw)
			if status != http.StatusBadRequest || typ != "invalid_request" || !strings.Contains(msg, c.member) {
				t.Errorf("POST /events %v = %d %s, want 400 invalid_request naming %q", c.body, status, raw, c.member)
			}
		}
		if n := w.countAllRecords(t); n != before {
			t.Errorf("records = %d after refused requests, want %d", n, before)
		}
	})

	t.Run("a null member is absent, an unknown one ignored, and a node_id of 255 characters accepted", func(t *testing.T) {
		got := created(t, map[string]any{
			"event_type": "session.fail", "id": nil, "severity": nil, "timestamp": nil,
			"node_id": nil, "payload": nil, "colour": "blue",
		})
		if id, _ := got["id"].(string); !generatedID.MatchString(id) {
			t.Errorf("id = %v, want a generated UUID", got["id"])
		}
		if got["severity"] != "error" || got["payload"] != nil || got["timestamp"] != got["ingested_at"] {
			t.Errorf("answer = %v, want severity error, payload null and the hub's timestamp", got)
		}
		if _, ok := got["colour"]; ok {
			t.Errorf("answer = %v holds the unknown member colour", got)
		}
		if status, raw := post(t, map[string]any{"event_type": nil}); status != http.StatusBadRequest {
			t.Errorf("a null event_type = %d %s, want 400", status, raw)
		}
		if status, raw := post(t, map[string]any{"event_type": "x", "node_id": strings.Repeat("é", 255)}); status != http.StatusCreated {
			t.Errorf("a node_id of 255 characters = %d %s, want 201", status, raw)
		}
	})

	t.Run("a store failure answers internal and stores nothing", func(t *testing.T) {
		before := w.countAllRecords(t)
		w.failWrites.Store(true)
		status, raw := post(t, map[string]any{"event_type": "x"})
		w.failWrites.Store(false)
		if typ, _ := errorOf(t, raw); status != http.StatusInternalServerError || typ != "internal" {
			t.Errorf("POST /events on a failing store = %d %s, want 500 internal", status, raw)
		}
		if n := w.countAllRecords(t); n != before {
			t.Errorf("records = %d after a failed write, want %d", n, before)
		}
	})
}
