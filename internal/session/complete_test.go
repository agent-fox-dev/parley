package session_test

import (
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/session"
)

// complete sends POST /sessions/:id/complete with body as credential.
func (w *world) complete(t *testing.T, credential, id string, body any) (int, []byte) {
	t.Helper()
	return w.Call(t, http.MethodPost, "/api/v1/sessions/"+id+"/complete", credential, body)
}

// changeRoute is a route that changes a session, with a body it accepts
// from the session's creator while the session is active.
type changeRoute struct {
	name, suffix string
	body         func() any
}

// changeRoutes are the routes the creator rule and the not-found rule hold
// for.
var changeRoutes = []changeRoute{
	{"complete", "/complete", func() any { return map[string]any{"status": "completed"} }},
}

// wantUnchanged fails the test unless the session id is still active, not
// ended, and has no usage record.
func (w *world) wantUnchanged(t *testing.T, id string) {
	t.Helper()
	status, raw := w.get(t, w.Token, id)
	if status != http.StatusOK {
		t.Fatalf("GET /sessions/%s = %d %s, want 200", id, status, raw)
	}
	if s := decode(t, raw); s["status"] != "active" || s["ended_at"] != nil {
		t.Errorf("session after the refusal: status %v, ended_at %v; want active and null", s["status"], s["ended_at"])
	}
	if n := w.count(t, "session_usage"); n != 0 {
		t.Errorf("usage records = %d, want 0", n)
	}
}

func TestComplete_EndsAnActiveSession(t *testing.T) {
	w := newWorld(t)
	cases := []struct {
		name     string
		body     map[string]any
		duration float64
		errorMsg any
		cache    float64
	}{
		{"status completed only", map[string]any{"status": "completed"}, 90000, nil, 0},
		{"status failed with every member", map[string]any{
			"status": "failed", "error_message": "boom", "duration_ms": 1234, "cache_creation_input_tokens": 7,
		}, 1234, "boom", 7},
		{"status timeout with duration_ms 0", map[string]any{"status": "timeout", "duration_ms": 0}, 0, nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo})
			id := s["id"].(string)
			started, err := time.Parse(time.RFC3339, s["started_at"].(string))
			if err != nil {
				t.Fatal(err)
			}
			w.clock.Advance(90 * time.Second)

			status, raw := w.complete(t, w.aliceKey, id, c.body)
			if status != http.StatusOK {
				t.Fatalf("POST complete = %d %s, want 200", status, raw)
			}
			got := decode(t, raw)
			want := map[string]any{
				"status": c.body["status"], "ended_at": started.Add(90 * time.Second).Format(time.RFC3339),
				"duration_ms": c.duration, "error_message": c.errorMsg, "cache_creation_input_tokens": c.cache,
				"started_at": s["started_at"], "workspace_slug": demo,
			}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("%s = %v, want %v", k, got[k], v)
				}
			}
			getStatus, getRaw := w.get(t, w.aliceKey, id)
			if getStatus != http.StatusOK || !reflect.DeepEqual(decode(t, getRaw), got) {
				t.Errorf("GET /sessions/:id = %d %s, want 200 %s", getStatus, getRaw, raw)
			}
		})
	}
}

func TestComplete_AnswersAnEndedSessionUnchanged(t *testing.T) {
	w := newWorld(t)
	for _, stored := range []string{"completed", "failed", "timeout", "terminated"} {
		t.Run(stored, func(t *testing.T) {
			id := w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo})["id"].(string)
			w.clock.Advance(time.Minute)
			if stored == session.StatusTerminated {
				w.terminate(t, id)
			} else if status, raw := w.complete(t, w.aliceKey, id, map[string]any{"status": stored}); status != http.StatusOK {
				t.Fatalf("POST complete = %d %s, want 200", status, raw)
			}
			_, before := w.get(t, w.aliceKey, id)
			w.clock.Advance(time.Minute)

			status, raw := w.complete(t, w.aliceKey, id, map[string]any{"status": "failed", "error_message": "late", "duration_ms": 5})
			if status != http.StatusOK || !reflect.DeepEqual(decode(t, raw), decode(t, before)) {
				t.Errorf("POST complete = %d %s, want 200 %s", status, raw, before)
			}
		})
	}
}

func TestComplete_ConcurrentCompletesApplyOne(t *testing.T) {
	w := newWorld(t)
	id := w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo})["id"].(string)
	w.clock.Advance(time.Second)
	const n = 8
	sent := make([]map[string]any, n)
	statuses, bodies, errs := make([]int, n), make([][]byte, n), make([]error, n)
	for i := range n {
		status := "completed"
		if i%2 == 1 {
			status = "failed"
		}
		sent[i] = map[string]any{"status": status, "error_message": fmt.Sprintf("attempt %d", i)}
	}
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			statuses[i], bodies[i], errs[i] = w.rawPostTo("/api/v1/sessions/"+id+"/complete", w.aliceKey, sent[i])
		})
	}
	wg.Wait()

	_, raw := w.get(t, w.aliceKey, id)
	s := decode(t, raw)
	if s["status"] != "completed" && s["status"] != "failed" {
		t.Fatalf("stored status = %v, want completed or failed", s["status"])
	}
	applied := 0
	for _, b := range sent {
		if b["status"] == s["status"] && b["error_message"] == s["error_message"] {
			applied++
		}
	}
	if applied != 1 {
		t.Errorf("stored status %v with error_message %v matches %d of the completes sent, want 1", s["status"], s["error_message"], applied)
	}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("POST complete: %v", errs[i])
		}
		if statuses[i] != http.StatusOK || !reflect.DeepEqual(decode(t, bodies[i]), s) {
			t.Errorf("POST complete %v = %d %s, want 200 %s", sent[i], statuses[i], bodies[i], raw)
		}
	}
}

func TestComplete_RefusesAMalformedRequest(t *testing.T) {
	w := newWorld(t)
	x := w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo})["id"].(string)
	completeX := "/api/v1/sessions/" + x + "/complete"
	cases := []struct {
		name, path string
		body       any
	}{
		{"path id abc on complete", "/api/v1/sessions/abc/complete", map[string]any{"status": "completed"}},
		{"an empty body", completeX, map[string]any{}},
		{"status terminated", completeX, map[string]any{"status": "terminated"}},
		{"status active", completeX, map[string]any{"status": "active"}},
		{"status done", completeX, map[string]any{"status": "done"}},
		{"status 4", completeX, map[string]any{"status": 4}},
		{"duration_ms -1", completeX, map[string]any{"status": "completed", "duration_ms": -1}},
		{"duration_ms 1.5", completeX, map[string]any{"status": "completed", "duration_ms": 1.5}},
		{"duration_ms '3'", completeX, map[string]any{"status": "completed", "duration_ms": "3"}},
		{"cache_creation_input_tokens -1", completeX, map[string]any{"status": "completed", "cache_creation_input_tokens": -1}},
		{"cache_creation_input_tokens 1.5", completeX, map[string]any{"status": "completed", "cache_creation_input_tokens": 1.5}},
		{"error_message 4", completeX, map[string]any{"status": "failed", "error_message": 4}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, raw := w.Call(t, http.MethodPost, c.path, w.aliceKey, c.body)
			if errorType, _ := errorOf(t, raw); status != http.StatusBadRequest || errorType != "invalid_request" {
				t.Errorf("POST %s %v = %d %s, want 400 invalid_request", c.path, c.body, status, raw)
			}
			w.wantUnchanged(t, x)
		})
	}
}

func TestComplete_AcceptsOnlyTheCreatorAndTheAdminToken(t *testing.T) {
	w := newWorld(t)
	aliceReadOnly := w.pat(t, w.alice, "", "sessions:read")
	cases := []struct {
		name, opener, sender string
		status               int
		errorType            string
	}{
		{"bob, a member who reads demo, on alice's session", w.aliceKey, w.bobKey, http.StatusForbidden, "forbidden"},
		{"alice's PAT with only sessions:read on her session", w.aliceKey, aliceReadOnly, http.StatusForbidden, "insufficient_scope"},
		{"alice's API key on the session she opened with her PAT", w.alicePAT, w.aliceKey, http.StatusOK, ""},
		{"the admin token on alice's session", w.aliceKey, w.Token, http.StatusOK, ""},
		{"alice, the owner of demo, on a session the admin token opened", w.Token, w.aliceKey, http.StatusForbidden, "forbidden"},
		{"the admin token on its own session", w.Token, w.Token, http.StatusOK, ""},
	}
	for _, r := range changeRoutes {
		for _, c := range cases {
			t.Run(r.name+"/"+c.name, func(t *testing.T) {
				id := w.open(t, c.opener, map[string]any{"workspace_slug": demo})["id"].(string)
				status, raw := w.Call(t, http.MethodPost, "/api/v1/sessions/"+id+r.suffix, c.sender, r.body())
				if c.status == http.StatusOK {
					if status < 200 || status > 299 {
						t.Errorf("POST %s = %d %s, want 2xx", r.suffix, status, raw)
					}
					return
				}
				if errorType, _ := errorOf(t, raw); status != c.status || errorType != c.errorType {
					t.Errorf("POST %s = %d %s, want %d %s", r.suffix, status, raw, c.status, c.errorType)
				}
				w.wantUnchanged(t, id)
			})
		}
	}
}

func TestComplete_HidesWhetherAnUnreadableSessionExists(t *testing.T) {
	w := newWorld(t, func(d *session.Deps) { d.Coverage = boundCoverage{} })
	x := w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo})["id"].(string)
	boundElsewhere := w.pat(t, w.alice, other, "sessions:write")
	cases := []struct {
		name, credential, id string
	}{
		{"carol on X", w.carolKey, x},
		{"alice on an unknown id", w.aliceKey, unknownID},
		{"alice, its creator, with a token bound elsewhere on X", boundElsewhere, x},
	}
	for _, r := range changeRoutes {
		var bodies [][]byte
		for _, c := range cases {
			t.Run(r.name+"/"+c.name, func(t *testing.T) {
				status, raw := w.Call(t, http.MethodPost, "/api/v1/sessions/"+c.id+r.suffix, c.credential, r.body())
				errorType, message := errorOf(t, raw)
				if status != http.StatusNotFound || errorType != "not_found" || message != "session not found" {
					t.Errorf("POST %s = %d %s, want 404 not_found with message %q", r.suffix, status, raw, "session not found")
				}
				bodies = append(bodies, raw)
				w.wantUnchanged(t, x)
			})
		}
		if len(bodies) >= 2 && string(bodies[0]) != string(bodies[1]) {
			t.Errorf("%s: the 404 body for carol on X, %s, differs from the one for an unknown id, %s", r.name, bodies[0], bodies[1])
		}
	}
}
