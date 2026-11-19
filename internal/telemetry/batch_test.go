package telemetry_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// batchAnswer is the answer to a batch.
type batchAnswer struct {
	Accepted   int         `json:"accepted"`
	Duplicates int         `json:"duplicates"`
	Errors     []itemError `json:"errors"`
}

// itemError is an errors entry of a batch answer.
type itemError struct {
	Index   int     `json:"index"`
	ID      *string `json:"id"`
	Message string  `json:"message"`
}

// indexes are the indexes of errs, in their order.
func indexes(errs []itemError) []int {
	var ix []int
	for _, e := range errs {
		ix = append(ix, e.Index)
	}
	return ix
}

// TestBatches runs the behaviours of POST …/events/batch and
// …/traces/batch on one hub, alice posting to demo, each subtest on runs of
// its own. The subtests run in order, so that a store fault one of them
// switches on meets its own batch only.
func TestBatches(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	routes := []struct {
		path, table string
		item        func() map[string]any
	}{
		{"/events/batch", "run_events", func() map[string]any { return map[string]any{"event_type": "e"} }},
		{"/traces/batch", "traces", func() map[string]any { return map[string]any{"event_type": "tool.use"} }},
	}
	// inRun is the number of records of table in the run run of demo.
	inRun := func(t *testing.T, table, run string) int {
		t.Helper()
		return w.count(t, `SELECT COUNT(*) FROM `+table+` WHERE workspace = ? AND run_id = ?`, demo, run)
	}
	// answered checks that the batch was answered 200 with an errors array
	// and returns the answer.
	answered := func(t *testing.T, path string, status int, raw []byte) batchAnswer {
		t.Helper()
		if status != http.StatusOK {
			t.Fatalf("POST %s = %d %s, want 200", path, status, raw)
		}
		if errs, ok := decode(t, raw)["errors"].([]any); !ok || errs == nil {
			t.Fatalf("POST %s = %s, want an errors array", path, raw)
		}
		var a batchAnswer
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		return a
	}
	// items are n valid items of the route r with fresh ids, as a body.
	items := func(r int, n int) []any {
		body := make([]any, n)
		for i := range body {
			item := routes[r].item()
			item["id"] = w.freshID()
			body[i] = item
		}
		return body
	}

	t.Run("valid items with new ids are all stored with the defaults of a single record", func(t *testing.T) {
		for r, rt := range routes {
			for _, n := range []int{1, 3, 1000} {
				run := w.freshRun()
				body := make([]any, n)
				sent := map[string]bool{}
				withoutID := 0
				for i := range body {
					item := rt.item()
					switch {
					case i == 0 && r == 0:
						item["event_type"] = "session.fail"
						withoutID++
					case i%2 == 0:
						withoutID++
					default:
						id := w.freshID()
						item["id"] = strings.ToUpper(id)
						sent[id] = true
					}
					body[i] = item
				}
				status, raw := w.post(t, w.aliceKey, demo, run, rt.path, body)
				a := answered(t, rt.path, status, raw)
				if a.Accepted != n || a.Duplicates != 0 || len(a.Errors) != 0 {
					t.Errorf("POST %s of %d items = %s, want {accepted: %d, duplicates: 0, errors: []}", rt.path, n, raw, n)
				}
				if got := inRun(t, rt.table, run); got != n {
					t.Errorf("%s of the run = %d, want %d", rt.table, got, n)
				}
				rows, err := w.Store.Analytics().QueryContext(context.Background(),
					`SELECT id FROM `+rt.table+` WHERE workspace = ? AND run_id = ?`, demo, run)
				if err != nil {
					t.Fatal(err)
				}
				generated := 0
				for rows.Next() {
					var id string
					if err := rows.Scan(&id); err != nil {
						t.Fatal(err)
					}
					if sent[id] {
						continue
					}
					generated++
					if !generatedID.MatchString(id) {
						t.Errorf("%s: stored id %q was neither sent nor a version 4 UUID in lower case", rt.path, id)
					}
				}
				if err := rows.Close(); err != nil {
					t.Fatal(err)
				}
				if generated != withoutID {
					t.Errorf("%s of %d items: %d generated ids, want %d", rt.path, n, generated, withoutID)
				}
				if r == 0 {
					severity := w.count(t, `SELECT COUNT(*) FROM run_events WHERE workspace = ? AND run_id = ? AND event_type = 'session.fail' AND severity = 'error'`, demo, run)
					if severity != 1 {
						t.Errorf("session.fail events of severity error = %d, want 1", severity)
					}
				}
			}
		}
	})

	t.Run("invalid items are reported by index and id beside the valid ones, which are stored", func(t *testing.T) {
		cases := []struct {
			route  int
			bad    map[string]any
			member string
		}{
			{0, map[string]any{"severity": "x", "event_type": "e"}, "severity"},
			{1, map[string]any{"event_type": "user.message"}, "event_type"},
		}
		for _, c := range cases {
			rt := routes[c.route]
			run, u2 := w.freshRun(), w.freshID()
			c.bad["id"] = strings.ToUpper(u2)
			badID := rt.item()
			badID["id"] = "bad"
			otherRun := rt.item()
			otherRun["run_id"] = "20260102_030405_ffffff"
			body := []any{rt.item(), c.bad, "text", badID, otherRun, rt.item()}
			status, raw := w.post(t, w.aliceKey, demo, run, rt.path, body)
			a := answered(t, rt.path, status, raw)
			if a.Accepted != 2 || a.Duplicates != 0 || !slices.Equal(indexes(a.Errors), []int{1, 2, 3, 4}) {
				t.Fatalf("POST %s = %s, want accepted 2, duplicates 0 and errors at 1, 2, 3, 4", rt.path, raw)
			}
			if e := a.Errors[0]; e.ID == nil || *e.ID != u2 || !strings.Contains(e.Message, c.member) {
				t.Errorf("%s: errors[0] = %+v, want id %s and a message naming %s", rt.path, e, u2, c.member)
			}
			if e := a.Errors[1]; e.ID != nil {
				t.Errorf("%s: errors[1] of an item that is no object = %+v, want id null", rt.path, e)
			}
			if e := a.Errors[2]; e.ID != nil || !strings.Contains(e.Message, "id") {
				t.Errorf("%s: errors[2] of an invalid id = %+v, want id null and a message naming id", rt.path, e)
			}
			if e := a.Errors[3]; !strings.Contains(e.Message, "run_id") {
				t.Errorf("%s: errors[3] of another run_id = %+v, want a message naming run_id", rt.path, e)
			}
			if got := inRun(t, rt.table, run); got != 2 {
				t.Errorf("%s of the run = %d, want 2", rt.table, got)
			}
		}
	})

	t.Run("an id stored in the run or repeated in the batch is a duplicate and changes nothing", func(t *testing.T) {
		run := w.freshRun()
		u1, u2, u3 := w.freshID(), w.freshID(), w.freshID()
		if status, raw := w.post(t, w.aliceKey, demo, run, "/events", map[string]any{"id": u1, "event_type": "e", "node_id": "first"}); status != http.StatusCreated {
			t.Fatalf("POST /events = %d %s, want 201", status, raw)
		}
		batch := []any{
			map[string]any{"id": u1, "event_type": "e", "node_id": "second"},
			map[string]any{"id": u2, "event_type": "e", "node_id": "a"},
			map[string]any{"id": u2, "event_type": "e", "node_id": "b"},
			map[string]any{"id": u3, "event_type": "e"},
		}
		status, raw := w.post(t, w.aliceKey, demo, run, "/events/batch", batch)
		if a := answered(t, "/events/batch", status, raw); a.Accepted != 2 || a.Duplicates != 2 || len(a.Errors) != 0 {
			t.Errorf("POST /events/batch = %s, want {accepted: 2, duplicates: 2, errors: []}", raw)
		}
		if got := w.storedRow(t, "run_events", u1)["node_id"]; got != "first" {
			t.Errorf("node_id of %s = %v, want first", u1, got)
		}
		if got := w.storedRow(t, "run_events", u2)["node_id"]; got != "a" {
			t.Errorf("node_id of %s = %v, want a", u2, got)
		}
		status, raw = w.post(t, w.aliceKey, demo, run, "/events/batch", batch)
		if a := answered(t, "/events/batch", status, raw); a.Accepted != 0 || a.Duplicates != 4 || len(a.Errors) != 0 {
			t.Errorf("the same batch again = %s, want {accepted: 0, duplicates: 4, errors: []}", raw)
		}
		if got := inRun(t, "run_events", run); got != 3 {
			t.Errorf("run events of the run = %d, want 3", got)
		}
	})

	t.Run("an id stored in another workspace or run is reported and changes nothing", func(t *testing.T) {
		runA, runB := w.freshRun(), w.freshRun()
		u1, u2, u3 := w.freshID(), w.freshID(), w.freshID()
		for _, s := range []struct{ slug, run, id string }{{demo, runB, u1}, {other, runA, u2}} {
			if status, raw := w.post(t, w.aliceKey, s.slug, s.run, "/traces", map[string]any{"id": s.id, "event_type": "tool.use"}); status != http.StatusCreated {
				t.Fatalf("POST /traces to %s = %d %s, want 201", s.slug, status, raw)
			}
		}
		batch := []any{
			map[string]any{"id": u1, "event_type": "tool.use"},
			map[string]any{"id": u2, "event_type": "tool.use"},
			map[string]any{"id": u3, "event_type": "tool.use"},
		}
		status, raw := w.post(t, w.aliceKey, demo, runA, "/traces/batch", batch)
		a := answered(t, "/traces/batch", status, raw)
		if a.Accepted != 1 || a.Duplicates != 0 || !slices.Equal(indexes(a.Errors), []int{0, 1}) {
			t.Fatalf("POST /traces/batch = %s, want accepted 1, duplicates 0 and errors at 0, 1", raw)
		}
		for i, id := range []string{u1, u2} {
			if e := a.Errors[i]; e.ID == nil || *e.ID != id {
				t.Errorf("errors[%d] = %+v, want id %s", i, e, id)
			}
		}
		if got := w.storedRow(t, "traces", u1); got["workspace"] != demo || got["run_id"] != runB {
			t.Errorf("trace %s = %v, want it unchanged in demo run %s", u1, got, runB)
		}
		if got := w.storedRow(t, "traces", u2); got["workspace"] != other || got["run_id"] != runA {
			t.Errorf("trace %s = %v, want it unchanged in other run %s", u2, got, runA)
		}
	})

	t.Run("a body that is not a non-empty JSON array is refused", func(t *testing.T) {
		bodies := []string{`[]`, `{"event_type": "e"}`, `"text"`, `7`, `null`, `not json`}
		for _, rt := range routes {
			before := w.count(t, `SELECT COUNT(*) FROM `+rt.table)
			for _, body := range bodies {
				status, raw := w.post(t, w.aliceKey, demo, w.freshRun(), rt.path, body)
				if typ, _ := errorOf(t, raw); status != http.StatusBadRequest || typ != "invalid_request" {
					t.Errorf("POST %s %s = %d %s, want 400 invalid_request", rt.path, body, status, raw)
				}
			}
			if n := w.count(t, `SELECT COUNT(*) FROM `+rt.table); n != before {
				t.Errorf("%s = %d after refused batches, want %d", rt.table, n, before)
			}
		}
	})

	t.Run("more than 1000 items are refused whole, and 1000 accepted", func(t *testing.T) {
		for r, rt := range routes {
			before := w.count(t, `SELECT COUNT(*) FROM `+rt.table)
			status, raw := w.post(t, w.aliceKey, demo, w.freshRun(), rt.path, items(r, 1001))
			if typ, _ := errorOf(t, raw); status != http.StatusRequestEntityTooLarge || typ != "payload_too_large" {
				t.Errorf("POST %s of 1001 items = %d %s, want 413 payload_too_large", rt.path, status, raw)
			}
			if n := w.count(t, `SELECT COUNT(*) FROM `+rt.table); n != before {
				t.Errorf("%s = %d after a refused batch, want %d", rt.table, n, before)
			}
			status, raw = w.post(t, w.aliceKey, demo, w.freshRun(), rt.path, items(r, 1000))
			if a := answered(t, rt.path, status, raw); a.Accepted != 1000 {
				t.Errorf("POST %s of 1000 items = %s, want accepted 1000", rt.path, raw)
			}
		}
	})

	t.Run("a batch of invalid items only is answered with one error each and stores nothing", func(t *testing.T) {
		run := w.freshRun()
		status, raw := w.post(t, w.aliceKey, demo, run, "/events/batch", []any{map[string]any{}, map[string]any{"event_type": ""}, 5})
		a := answered(t, "/events/batch", status, raw)
		if a.Accepted != 0 || a.Duplicates != 0 || !slices.Equal(indexes(a.Errors), []int{0, 1, 2}) {
			t.Errorf("POST /events/batch = %s, want accepted 0, duplicates 0 and errors at 0, 1, 2", raw)
		}
		if got := inRun(t, "run_events", run); got != 0 {
			t.Errorf("run events of the run = %d, want 0", got)
		}
	})

	t.Run("a store failure part way stores none of the batch, and the batch sent again is stored", func(t *testing.T) {
		run, body := w.freshRun(), items(0, 5)
		w.failThirdInsert.Store(true)
		status, raw := w.post(t, w.aliceKey, demo, run, "/events/batch", body)
		if typ, _ := errorOf(t, raw); status != http.StatusInternalServerError || typ != "internal" {
			t.Errorf("POST /events/batch with a failing third insert = %d %s, want 500 internal", status, raw)
		}
		if w.failThirdInsert.Load() {
			w.failThirdInsert.Store(false)
			t.Fatal("the batch made no third insert, so the fault never struck")
		}
		if got := inRun(t, "run_events", run); got != 0 {
			t.Errorf("run events of the run = %d after the failure, want 0", got)
		}
		status, raw = w.post(t, w.aliceKey, demo, run, "/events/batch", body)
		if a := answered(t, "/events/batch", status, raw); a.Accepted != 5 || a.Duplicates != 0 || len(a.Errors) != 0 {
			t.Errorf("the same batch again = %s, want {accepted: 5, duplicates: 0, errors: []}", raw)
		}
	})
}
