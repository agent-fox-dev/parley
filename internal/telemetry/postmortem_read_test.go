package telemetry_test

import (
	"net/http"
	"reflect"
	"testing"
)

// TestPostmortemRead reads postmortems back on one hub, alice's postmortem
// of runA stored in demo only.
func TestPostmortemRead(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	runA := w.freshRun()
	body := validPostmortem()
	body["task_summary"] = map[string]any{"total": 4}
	body["cost_summary"] = map[string]any{"usd": 1.5}
	body["blocked_tasks"] = []any{}
	body["session_history"] = []any{map[string]any{"node": "n1", "status": "failed"}}
	status, raw := w.post(t, w.alicePAT, demo, runA, "/postmortem", body)
	if status != http.StatusCreated {
		t.Fatalf("POST /postmortem = %d %s, want 201", status, raw)
	}
	stored := decode(t, raw)
	get := func(t *testing.T, credential, slug, run string) (int, []byte) {
		t.Helper()
		return w.Call(t, http.MethodGet, "/api/v1/workspaces/"+slug+"/runs/"+run+"/postmortem", credential, nil)
	}

	t.Run("the run's postmortem is answered with every field as stored", func(t *testing.T) {
		for _, credential := range []string{w.aliceKey, w.bobKey, w.Token} {
			status, raw := get(t, credential, demo, runA)
			if status != http.StatusOK {
				t.Fatalf("GET /postmortem = %d %s, want 200", status, raw)
			}
			got := decode(t, raw)
			if !reflect.DeepEqual(got, stored) {
				t.Errorf("GET /postmortem = %v, want the stored postmortem %v", got, stored)
			}
			summary, _ := got["task_summary"].(map[string]any)
			cost, _ := got["cost_summary"].(map[string]any)
			blocked, isList := got["blocked_tasks"].([]any)
			history, _ := got["session_history"].([]any)
			if summary["total"] != 4.0 || cost["usd"] != 1.5 || !isList || len(blocked) != 0 || len(history) != 1 {
				t.Errorf("GET /postmortem = %v, want the summaries as objects and the lists as arrays, as sent", got)
			}
		}
	})

	t.Run("a run without a postmortem in this workspace is postmortem_not_found", func(t *testing.T) {
		withRecords := w.freshRun()
		w.seed(t, "/events", demo, withRecords, fullRecord("/events", w.freshID(), t1, nil))
		cases := []struct{ name, slug, run string }{
			{"a run with records and no postmortem", demo, withRecords},
			{"a run nothing was stored for", demo, w.freshRun()},
			{"a run whose postmortem is in another workspace", other, runA},
		}
		for _, c := range cases {
			status, raw := get(t, w.aliceKey, c.slug, c.run)
			if typ, _ := errorOf(t, raw); status != http.StatusNotFound || typ != "postmortem_not_found" {
				t.Errorf("%s: GET /postmortem = %d %s, want 404 postmortem_not_found", c.name, status, raw)
			}
		}
	})
}
