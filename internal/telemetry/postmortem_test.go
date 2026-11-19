package telemetry_test

import (
	"bytes"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestPostmortem runs the behaviours of the postmortem route on one hub,
// alice posting to demo and other, each subtest on runs of its own.
func TestPostmortem(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	lowerUUID := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	members := []string{
		"id", "workspace", "run_id", "timestamp", "ingested_at", "schema_version", "run_status", "started_at",
		"completed_at", "task_summary", "cost_summary", "blocked_tasks", "session_history",
	}
	// with is a valid postmortem body with the members of changes set, and
	// those of changes that are nil removed. The body is sent as JSON, whose
	// objects have their keys in order.
	with := func(changes map[string]any) map[string]any {
		b := validPostmortem()
		for k, v := range changes {
			if v == nil {
				delete(b, k)
			} else {
				b[k] = v
			}
		}
		return b
	}
	postmortem := func(t *testing.T, slug, run string, body any) (int, []byte) {
		t.Helper()
		return w.post(t, w.aliceKey, slug, run, "/postmortem", body)
	}
	countAll := func(t *testing.T) int {
		t.Helper()
		return w.count(t, `SELECT COUNT(*) FROM postmortems`)
	}
	countOf := func(t *testing.T, slug, run string) int {
		t.Helper()
		return w.count(t, `SELECT COUNT(*) FROM postmortems WHERE workspace = ? AND run_id = ?`, slug, run)
	}

	t.Run("a first postmortem is stored with the hub's id and time and its defaults", func(t *testing.T) {
		for _, status := range []string{"stalled", "block_limit", "cost_limit", "session_limit"} {
			run := w.freshRun()
			code, raw := postmortem(t, demo, run, with(map[string]any{"run_status": status}))
			if code != http.StatusCreated {
				t.Fatalf("POST /postmortem with run_status %s = %d %s, want 201", status, code, raw)
			}
			got := decode(t, raw)
			for _, m := range members {
				if _, ok := got[m]; !ok {
					t.Errorf("answer %v lacks the member %s", got, m)
				}
			}
			if len(got) != len(members) {
				t.Errorf("answer %v has %d members, want %d", got, len(got), len(members))
			}
			summary, _ := got["task_summary"].(map[string]any)
			blocked, _ := got["blocked_tasks"].([]any)
			history, _ := got["session_history"].([]any)
			cost, _ := got["cost_summary"].(map[string]any)
			if got["schema_version"] != 1.0 || got["run_status"] != status || got["workspace"] != demo || got["run_id"] != run ||
				got["started_at"] != "2026-01-02T02:00:00Z" || got["completed_at"] != "2026-01-02T02:30:00Z" ||
				summary["done"] != 3.0 || summary["total"] != 4.0 || cost == nil || len(cost) != 0 ||
				blocked == nil || len(blocked) != 0 || history == nil || len(history) != 0 {
				t.Errorf("answer = %v, want schema_version 1, run_status %s, started_at in UTC, the summaries as sent and empty lists", got, status)
			}
			if got["timestamp"] != "2026-01-02T03:04:05Z" || got["ingested_at"] != "2026-01-02T03:04:05Z" {
				t.Errorf("timestamp %v and ingested_at %v, want both the hub's time 2026-01-02T03:04:05Z", got["timestamp"], got["ingested_at"])
			}
			if id, _ := got["id"].(string); !lowerUUID.MatchString(id) {
				t.Errorf("id = %v, want a lower-case UUID", got["id"])
			}
		}
		run := w.freshRun()
		code, raw := postmortem(t, demo, run, with(map[string]any{
			"schema_version": 1, "blocked_tasks": []any{map[string]any{"id": "t1"}}, "session_history": []any{1},
			"id": "ignored", "timestamp": "2020-01-01T00:00:00Z",
		}))
		if code != http.StatusCreated {
			t.Fatalf("POST /postmortem with every member = %d %s, want 201", code, raw)
		}
		got := decode(t, raw)
		blocked, _ := got["blocked_tasks"].([]any)
		first, _ := blocked[0].(map[string]any)
		history, _ := got["session_history"].([]any)
		if len(blocked) != 1 || first["id"] != "t1" || len(history) != 1 || history[0] != 1.0 {
			t.Errorf("answer = %v, want blocked_tasks [{id: t1}] and session_history [1]", got)
		}
		if id, _ := got["id"].(string); !lowerUUID.MatchString(id) || got["timestamp"] != "2026-01-02T03:04:05Z" {
			t.Errorf("id %v and timestamp %v, want a generated UUID and the hub's time", got["id"], got["timestamp"])
		}
		row := w.storedRow(t, "postmortems", got["id"].(string))
		if row["task_summary"] != `{"done":3,"total":4}` || row["blocked_tasks"] != `[{"id":"t1"}]` {
			t.Errorf("stored task_summary %v and blocked_tasks %v, want them as JSON text", row["task_summary"], row["blocked_tasks"])
		}
	})

	t.Run("a later postmortem of the run answers the stored one and changes nothing", func(t *testing.T) {
		run := w.freshRun()
		code, first := postmortem(t, demo, run, validPostmortem())
		if code != http.StatusCreated {
			t.Fatalf("POST /postmortem = %d %s, want 201", code, first)
		}
		w.clock.advance(time.Second)
		code, raw := postmortem(t, demo, run, with(map[string]any{"run_status": "cost_limit", "task_summary": map[string]any{"total": 9}}))
		if code != http.StatusOK || !bytes.Equal(raw, first) {
			t.Errorf("a second postmortem = %d %s, want 200 %s", code, raw, first)
		}
		if got := decode(t, raw)["run_status"]; got != "stalled" {
			t.Errorf("run_status = %v, want the stored stalled", got)
		}
		if n := countOf(t, demo, run); n != 1 {
			t.Errorf("postmortems of demo run %s = %d, want 1", run, n)
		}
	})

	t.Run("the same run id in two workspaces has two postmortems", func(t *testing.T) {
		run := w.freshRun()
		codeA, rawA := postmortem(t, demo, run, validPostmortem())
		codeB, rawB := postmortem(t, other, run, validPostmortem())
		if codeA != http.StatusCreated || codeB != http.StatusCreated {
			t.Fatalf("POST /postmortem on demo = %d %s and on other = %d %s, want 201 twice", codeA, rawA, codeB, rawB)
		}
		a, b := decode(t, rawA), decode(t, rawB)
		if a["id"] == b["id"] || b["workspace"] != other {
			t.Errorf("postmortems %v and %v, want different ids and the second in other", a, b)
		}
	})

	t.Run("a schema_version other than 1 is unknown and stores nothing", func(t *testing.T) {
		before := countAll(t)
		for _, v := range []any{0, 2, "1", 1.5} {
			code, raw := postmortem(t, demo, w.freshRun(), with(map[string]any{"schema_version": v}))
			if typ, _ := errorOf(t, raw); code != http.StatusBadRequest || typ != "unknown_schema_version" {
				t.Errorf("schema_version %#v = %d %s, want 400 unknown_schema_version", v, code, raw)
			}
		}
		if n := countAll(t); n != before {
			t.Errorf("postmortems = %d after refused requests, want %d", n, before)
		}
		body := validPostmortem()
		body["schema_version"] = nil
		code, raw := postmortem(t, demo, w.freshRun(), body)
		if code != http.StatusCreated || decode(t, raw)["schema_version"] != 1.0 {
			t.Errorf("schema_version null = %d %s, want 201 with schema_version 1", code, raw)
		}
	})

	t.Run("a member that breaks its rule is named and stores nothing, also after a stored postmortem", func(t *testing.T) {
		cases := []struct {
			name    string
			changes map[string]any
			member  string
		}{
			{"run_status removed", map[string]any{"run_status": nil}, "run_status"},
			{"run_status crashed", map[string]any{"run_status": "crashed"}, "run_status"},
			{"started_at removed", map[string]any{"started_at": nil}, "started_at"},
			{"completed_at removed", map[string]any{"completed_at": nil}, "completed_at"},
			{"completed_at now", map[string]any{"completed_at": "now"}, "completed_at"},
			{"task_summary removed", map[string]any{"task_summary": nil}, "task_summary"},
			{"task_summary an array", map[string]any{"task_summary": []any{}}, "task_summary"},
			{"task_summary without total", map[string]any{"task_summary": map[string]any{}}, "task_summary"},
			{"task_summary with a null total", map[string]any{"task_summary": map[string]any{"total": nil}}, "task_summary"},
			{"cost_summary removed", map[string]any{"cost_summary": nil}, "cost_summary"},
			{"cost_summary a string", map[string]any{"cost_summary": "free"}, "cost_summary"},
			{"blocked_tasks an object", map[string]any{"blocked_tasks": map[string]any{}}, "blocked_tasks"},
			{"session_history a string", map[string]any{"session_history": "none"}, "session_history"},
		}
		before := countAll(t)
		for _, c := range cases {
			code, raw := postmortem(t, demo, w.freshRun(), with(c.changes))
			if typ, msg := errorOf(t, raw); code != http.StatusBadRequest || typ != "invalid_request" || !strings.Contains(msg, c.member) {
				t.Errorf("%s = %d %s, want 400 invalid_request naming %s", c.name, code, raw, c.member)
			}
		}
		if n := countAll(t); n != before {
			t.Errorf("postmortems = %d after refused requests, want %d", n, before)
		}
		run := w.freshRun()
		if code, raw := postmortem(t, demo, run, validPostmortem()); code != http.StatusCreated {
			t.Fatalf("POST /postmortem = %d %s, want 201", code, raw)
		}
		code, raw := postmortem(t, demo, run, with(map[string]any{"run_status": nil}))
		if typ, _ := errorOf(t, raw); code != http.StatusBadRequest || typ != "invalid_request" {
			t.Errorf("an invalid postmortem of a run that has one = %d %s, want 400 invalid_request", code, raw)
		}
	})

	t.Run("concurrent postmortems of one run store one and answer it to all", func(t *testing.T) {
		run := w.freshRun()
		bodies := make([]any, 8)
		for i := range bodies {
			bodies[i] = with(map[string]any{"cost_summary": map[string]any{"usd": i}})
		}
		createdOnce(t, w.postAll(t, w.aliceKey, demo, run, "/postmortem", bodies))
		if n := countOf(t, demo, run); n != 1 {
			t.Errorf("postmortems of demo run %s = %d, want 1", run, n)
		}
	})
}
