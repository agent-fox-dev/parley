package telemetry_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestIdempotency runs the behaviours of a record sent again under its id
// on one hub, alice posting to demo and other, each subtest with ids of
// its own.
func TestIdempotency(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	var singles []ingestionRoute
	for _, r := range ingestionRoutes {
		if !r.batch {
			singles = append(singles, r)
		}
	}
	// bodyWith is a valid body of the route r carrying the id id, with a
	// node_id and a timestamp of the variant's own.
	bodyWith := func(r ingestionRoute, id string, variant int) map[string]any {
		b := r.valid().(map[string]any)
		b["id"] = id
		b["node_id"] = fmt.Sprintf("n%d", variant)
		b["timestamp"] = fmt.Sprintf("2026-01-02T03:00:%02d.5Z", variant)
		return b
	}
	// countOf is the number of records of table with the id id.
	countOf := func(t *testing.T, table, id string) int {
		t.Helper()
		return w.count(t, `SELECT COUNT(*) FROM `+table+` WHERE id = ?`, id)
	}

	t.Run("a replay answers the stored record and changes nothing, whatever its members", func(t *testing.T) {
		for _, r := range singles {
			t.Run(strings.TrimPrefix(r.path, "/"), func(t *testing.T) {
				id, run := w.freshID(), w.freshRun()
				status, first := w.post(t, w.aliceKey, demo, run, r.path, bodyWith(r, id, 0))
				if status != http.StatusCreated {
					t.Fatalf("POST %s = %d %s, want 201", r.path, status, first)
				}
				stored := w.storedRow(t, r.table, id)
				w.clock.advance(time.Second)
				if status, raw := w.post(t, w.aliceKey, demo, run, r.path, bodyWith(r, id, 0)); status != http.StatusOK || !bytes.Equal(raw, first) {
					t.Errorf("the same body again = %d %s, want 200 %s", status, raw, first)
				}
				w.clock.advance(time.Second)
				if status, raw := w.post(t, w.aliceKey, demo, run, r.path, bodyWith(r, id, 1)); status != http.StatusOK || !bytes.Equal(raw, first) {
					t.Errorf("a changed body again = %d %s, want 200 %s", status, raw, first)
				}
				if n := countOf(t, r.table, id); n != 1 {
					t.Errorf("%s holds %d records %s, want 1", r.table, n, id)
				}
				if got := w.storedRow(t, r.table, id); !reflect.DeepEqual(got, stored) {
					t.Errorf("stored record = %v, want it unchanged: %v", got, stored)
				}
			})
		}
	})

	t.Run("an id stored in another run or workspace is refused as a conflict", func(t *testing.T) {
		const runA, runB = "20260102_030405_a1b2c3", "20260102_030405_ffffff"
		for _, r := range singles {
			t.Run(strings.TrimPrefix(r.path, "/"), func(t *testing.T) {
				id := w.freshID()
				if status, raw := w.post(t, w.aliceKey, demo, runA, r.path, bodyWith(r, id, 0)); status != http.StatusCreated {
					t.Fatalf("POST %s = %d %s, want 201", r.path, status, raw)
				}
				stored := w.storedRow(t, r.table, id)
				for _, at := range []struct{ slug, run string }{{demo, runB}, {other, runA}} {
					status, raw := w.post(t, w.aliceKey, at.slug, at.run, r.path, bodyWith(r, id, 0))
					if typ, _ := errorOf(t, raw); status != http.StatusConflict || typ != "conflict" {
						t.Errorf("POST %s to %s run %s = %d %s, want 409 conflict", r.path, at.slug, at.run, status, raw)
					}
				}
				if n := countOf(t, r.table, id); n != 1 {
					t.Errorf("%s holds %d records %s, want 1", r.table, n, id)
				}
				got := w.storedRow(t, r.table, id)
				if !reflect.DeepEqual(got, stored) || got["workspace"] != demo || got["run_id"] != runA {
					t.Errorf("stored record = %v, want it unchanged in demo run %s: %v", got, runA, stored)
				}
			})
		}
	})

	t.Run("an id matches in any letter case and names one record of each kind", func(t *testing.T) {
		lower, run := w.freshID(), w.freshRun()
		if status, raw := w.post(t, w.aliceKey, demo, run, "/events", map[string]any{"id": lower, "event_type": "x"}); status != http.StatusCreated {
			t.Fatalf("POST /events = %d %s, want 201", status, raw)
		}
		status, raw := w.post(t, w.aliceKey, demo, run, "/events", map[string]any{"id": strings.ToUpper(lower), "event_type": "x"})
		if status != http.StatusOK || decode(t, raw)["id"] != lower {
			t.Errorf("POST /events with the id in upper case = %d %s, want 200 with id %s", status, raw, lower)
		}
		if status, raw := w.post(t, w.aliceKey, demo, run, "/tools/calls", map[string]any{"id": lower, "tool_name": "bash"}); status != http.StatusCreated {
			t.Errorf("POST /tools/calls with the id of a run event = %d %s, want 201", status, raw)
		}
		if events, calls := countOf(t, "run_events", lower), countOf(t, "tool_calls", lower); events != 1 || calls != 1 {
			t.Errorf("records %s: %d run events and %d tool calls, want 1 of each", lower, events, calls)
		}
	})

	t.Run("concurrent posts of one new id store it once and answer it to all", func(t *testing.T) {
		id, run := w.freshID(), w.freshRun()
		url := w.URL("/api/v1/workspaces/" + demo + "/runs/" + run + "/events")
		type answer struct {
			status int
			body   []byte
			err    error
		}
		answers := make([]answer, 8)
		var wg sync.WaitGroup
		for i := range answers {
			wg.Go(func() {
				body, _ := json.Marshal(map[string]any{"id": id, "event_type": "x", "node_id": fmt.Sprintf("n%d", i)})
				req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
				if err != nil {
					answers[i].err = err
					return
				}
				req.Header.Set("Authorization", "Bearer "+w.aliceKey)
				req.Header.Set("Content-Type", "application/json")
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					answers[i].err = err
					return
				}
				defer res.Body.Close()
				answers[i].status = res.StatusCode
				answers[i].body, answers[i].err = io.ReadAll(res.Body)
			})
		}
		wg.Wait()
		var createdBody []byte
		statuses := map[int]int{}
		for _, a := range answers {
			if a.err != nil {
				t.Fatalf("POST /events: %v", a.err)
			}
			statuses[a.status]++
			if a.status == http.StatusCreated {
				createdBody = a.body
			}
		}
		if statuses[http.StatusCreated] != 1 || statuses[http.StatusOK] != 7 {
			t.Fatalf("answers by status = %v, want one 201 and seven 200", statuses)
		}
		for _, a := range answers {
			if !bytes.Equal(a.body, createdBody) {
				t.Errorf("answer %d %s, want the body of the 201: %s", a.status, a.body, createdBody)
			}
		}
		if n := countOf(t, "run_events", id); n != 1 {
			t.Errorf("run_events holds %d records %s, want 1", n, id)
		}
	})

	t.Run("a replay that breaks a member's rule is refused before its id is looked up", func(t *testing.T) {
		id, run := w.freshID(), w.freshRun()
		if status, raw := w.post(t, w.aliceKey, demo, run, "/events", map[string]any{"id": id, "event_type": "x"}); status != http.StatusCreated {
			t.Fatalf("POST /events = %d %s, want 201", status, raw)
		}
		stored := w.storedRow(t, "run_events", id)
		status, raw := w.post(t, w.aliceKey, demo, run, "/events", map[string]any{"id": id, "event_type": "x", "severity": "fatal"})
		if typ, _ := errorOf(t, raw); status != http.StatusBadRequest || typ != "invalid_request" {
			t.Errorf("POST /events with severity fatal = %d %s, want 400 invalid_request", status, raw)
		}
		if got := w.storedRow(t, "run_events", id); !reflect.DeepEqual(got, stored) {
			t.Errorf("stored run event = %v, want it unchanged: %v", got, stored)
		}
	})
}
