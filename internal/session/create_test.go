package session_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/session"
)

// members are the members of every session object.
var members = []string{
	"archetype", "cache_creation_input_tokens", "changes", "counts", "credential_type", "duration_ms", "ended_at",
	"error_message", "forks", "id", "metadata", "model", "node_id", "run_id", "sandboxes", "started_at", "status",
	"token_summary", "user_id", "workspace_slug",
}

// v4 is a version 4 UUID in lower case.
var v4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// sessionID is the n-th session id of a test, in lower case.
func sessionID(n int) string { return fmt.Sprintf("%08x-abcd-4abc-8abc-%012x", n, n) }

func TestCreate_OpensAnActiveSession(t *testing.T) {
	w := newWorld(t)
	callers := []struct {
		name, credential, userID, credentialType string
	}{
		{"alice with an API key", w.aliceKey, w.alice, "api_key"},
		{"alice with a PAT holding sessions:write", w.alicePAT, w.alice, "pat"},
		{"the admin token", w.Token, "", "admin_token"},
	}
	n := 0
	for _, caller := range callers {
		bodies := []struct {
			name   string
			body   map[string]any
			parent any
		}{
			{"only workspace_slug", map[string]any{"workspace_slug": demo}, nil},
			{"every member", map[string]any{
				"workspace_slug": demo, "id": strings.ToUpper(sessionID(n + 1)), "run_id": "20260102_030405_0a1b2c",
				"node_id": "n1", "archetype": "coder", "model": "m1", "metadata": map[string]any{"a": []any{1.0}},
			}, nil},
			{"a fork of demo", map[string]any{"workspace_slug": fork1}, demo},
		}
		for _, b := range bodies {
			n++
			t.Run(caller.name+"/"+b.name, func(t *testing.T) {
				before := w.count(t, "sessions")
				s := w.open(t, caller.credential, b.body)

				keys := make([]string, 0, len(s))
				for k := range s {
					keys = append(keys, k)
				}
				slices.Sort(keys)
				if !slices.Equal(keys, members) {
					t.Errorf("members = %v, want %v", keys, members)
				}
				want := map[string]any{
					"workspace_slug": b.body["workspace_slug"], "status": "active", "started_at": w.clock.stamp(),
					"credential_type": caller.credentialType, "ended_at": nil, "duration_ms": nil, "error_message": nil,
					"cache_creation_input_tokens": 0.0, "forks": []any{}, "changes": []any{}, "sandboxes": []any{},
					"token_summary": map[string]any{
						"total_input_tokens": 0.0, "total_output_tokens": 0.0, "total_cache_read_tokens": 0.0, "models_used": []any{},
					},
					"counts": map[string]any{
						"forks": 0.0, "changes": 0.0, "sandboxes": 0.0, "versions": 0.0, "pushes": 0.0, "commits": 0.0,
						"claims": 0.0, "lands": 0.0,
					},
					"user_id": nil, "run_id": b.body["run_id"], "node_id": b.body["node_id"], "archetype": b.body["archetype"],
					"model": b.body["model"], "metadata": b.body["metadata"],
				}
				if caller.userID != "" {
					want["user_id"] = caller.userID
				}
				for k, v := range want {
					if !reflect.DeepEqual(s[k], v) {
						t.Errorf("%s = %#v, want %#v", k, s[k], v)
					}
				}
				id, _ := s["id"].(string)
				if sent, ok := b.body["id"].(string); ok {
					if id != strings.ToLower(sent) {
						t.Errorf("id = %q, want %q", id, strings.ToLower(sent))
					}
				} else if !v4.MatchString(id) {
					t.Errorf("id = %q, want a version 4 UUID in lower case", id)
				}

				var parent sql.NullString
				err := w.Store.Main().QueryRowContext(context.Background(), `SELECT parent_slug FROM sessions WHERE id = ?`, id).Scan(&parent)
				if err != nil {
					t.Fatal(err)
				}
				if got := map[bool]any{true: parent.String, false: nil}[parent.Valid]; got != b.parent {
					t.Errorf("stored parent slug = %v, want %v", got, b.parent)
				}
				if got := w.count(t, "sessions"); got != before+1 {
					t.Errorf("sessions = %d, want %d", got, before+1)
				}
			})
		}
	}
}

func TestCreate_RefusesAnInvalidBody(t *testing.T) {
	w := newWorld(t)
	cases := []struct {
		name   string
		body   map[string]any
		member string
	}{
		{"no workspace_slug", map[string]any{"model": "m1"}, "workspace_slug"},
		{"an empty workspace_slug", map[string]any{"workspace_slug": ""}, "workspace_slug"},
		{"a workspace_slug that is a number", map[string]any{"workspace_slug": 7}, "workspace_slug"},
		{"an id that is not a UUID", map[string]any{"workspace_slug": demo, "id": "not-a-uuid"}, "id"},
		{"an id without hyphens", map[string]any{"workspace_slug": demo, "id": "0123456789abcdef0123456789abcdef"}, "id"},
		{"a run_id that is a date", map[string]any{"workspace_slug": demo, "run_id": "2026-01-02"}, "run_id"},
		{"a run_id in upper case", map[string]any{"workspace_slug": demo, "run_id": "20260102_030405_ABCDEF"}, "run_id"},
		{"a node_id of 256 characters", map[string]any{"workspace_slug": demo, "node_id": strings.Repeat("x", 256)}, "node_id"},
		{"a model that is a number", map[string]any{"workspace_slug": demo, "model": 5}, "model"},
		{"an archetype of 256 characters", map[string]any{"workspace_slug": demo, "archetype": strings.Repeat("x", 256)}, "archetype"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, raw := w.post(t, w.aliceKey, c.body)
			errorType, message := errorOf(t, raw)
			if status != http.StatusBadRequest || errorType != "invalid_request" {
				t.Fatalf("POST /sessions = %d %s, want 400 invalid_request", status, raw)
			}
			if !strings.Contains(message, c.member) {
				t.Errorf("message %q does not name %s", message, c.member)
			}
			if n := w.count(t, "sessions"); n != 0 {
				t.Errorf("sessions = %d, want 0", n)
			}
		})
	}
	t.Run("access is decided before the members after workspace_slug", func(t *testing.T) {
		bad := map[string]any{"workspace_slug": demo, "id": "not-a-uuid"}
		if status, raw := w.post(t, w.carolKey, bad); status != http.StatusNotFound {
			t.Errorf("carol: POST /sessions = %d %s, want 404", status, raw)
		}
		if status, raw := w.post(t, w.bobKey, bad); status != http.StatusForbidden {
			t.Errorf("bob: POST /sessions = %d %s, want 403", status, raw)
		}
	})
	t.Run("a label of 255 characters", func(t *testing.T) {
		label := strings.Repeat("é", 255)
		s := w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo, "node_id": label, "archetype": label, "model": label})
		if s["node_id"] != label {
			t.Errorf("node_id = %v, want the 255 characters sent", s["node_id"])
		}
	})
}

func TestCreate_RefusesACallerWithoutWriteAccess(t *testing.T) {
	w := newWorld(t, func(d *session.Deps) { d.Coverage = boundCoverage{} })
	readOnly := w.pat(t, w.alice, "", "sessions:read")
	boundElsewhere := w.pat(t, w.alice, other, "sessions:write")
	cases := []struct {
		name, credential, slug string
		status                 int
		errorType              string
	}{
		{"a PAT with only sessions:read", readOnly, demo, http.StatusForbidden, "insufficient_scope"},
		{"a PAT with only sessions:read on an unknown slug", readOnly, "nope", http.StatusForbidden, "insufficient_scope"},
		{"carol on demo", w.carolKey, demo, http.StatusNotFound, "not_found"},
		{"alice on an unknown slug", w.aliceKey, "nope", http.StatusNotFound, "not_found"},
		{"the admin token on an unknown slug", w.Token, "nope", http.StatusNotFound, "not_found"},
		{"a token bound elsewhere naming demo", boundElsewhere, demo, http.StatusNotFound, "not_found"},
		{"bob, a member of demo's org", w.bobKey, demo, http.StatusForbidden, "forbidden"},
	}
	var notFound [][]byte
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, raw := w.post(t, c.credential, map[string]any{"workspace_slug": c.slug})
			errorType, message := errorOf(t, raw)
			if status != c.status || errorType != c.errorType {
				t.Fatalf("POST /sessions = %d %s, want %d %s", status, raw, c.status, c.errorType)
			}
			if status == http.StatusNotFound {
				if message != "workspace not found" {
					t.Errorf("message = %q, want %q", message, "workspace not found")
				}
				notFound = append(notFound, raw)
			}
		})
	}
	for _, raw := range notFound[1:] {
		if string(raw) != string(notFound[0]) {
			t.Errorf("404 bodies differ: %s and %s", notFound[0], raw)
		}
	}
	if n := w.count(t, "sessions"); n != 0 {
		t.Errorf("sessions = %d, want 0", n)
	}
	t.Run("a token bound to demo", func(t *testing.T) {
		w.open(t, w.pat(t, w.alice, demo, "sessions:write"), map[string]any{"workspace_slug": demo})
	})
}

func TestCreate_RefusesAnArchivedWorkspace(t *testing.T) {
	w := newWorld(t)
	w.setStatus(t, demo, access.StatusArchived)
	status, raw := w.post(t, w.aliceKey, map[string]any{"workspace_slug": demo})
	if errorType, _ := errorOf(t, raw); status != http.StatusConflict || errorType != "workspace_archived" {
		t.Errorf("alice: POST /sessions = %d %s, want 409 workspace_archived", status, raw)
	}
	status, raw = w.post(t, w.bobKey, map[string]any{"workspace_slug": demo})
	if errorType, _ := errorOf(t, raw); status != http.StatusForbidden || errorType != "forbidden" {
		t.Errorf("bob: POST /sessions = %d %s, want 403 forbidden", status, raw)
	}
	if n := w.count(t, "sessions"); n != 0 {
		t.Errorf("sessions = %d, want 0", n)
	}
	for _, st := range []string{access.StatusPending, access.StatusCloning, access.StatusFailed, access.StatusReady} {
		t.Run(st, func(t *testing.T) {
			w.setStatus(t, demo, st)
			w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo})
		})
	}
}

func TestCreate_ReplaysAStoredSession(t *testing.T) {
	w := newWorld(t)
	x, y := sessionID(1), sessionID(2)
	first := map[string]map[string]any{
		x: w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo, "id": x, "model": "m1"}),
		y: w.open(t, w.Token, map[string]any{"workspace_slug": demo, "id": y, "model": "m1"}),
	}
	w.clock.Advance(time.Hour)
	replays := []struct {
		name, credential, id string
	}{
		{"alice with a PAT and the id in upper case", w.alicePAT, strings.ToUpper(x)},
		{"the admin token", w.Token, y},
	}
	for _, archived := range []bool{false, true} {
		if archived {
			w.setStatus(t, demo, access.StatusArchived)
		}
		for _, r := range replays {
			t.Run(fmt.Sprintf("%s/archived=%t", r.name, archived), func(t *testing.T) {
				status, raw := w.post(t, r.credential, map[string]any{
					"workspace_slug": demo, "id": r.id, "model": "m2", "metadata": map[string]any{"z": 1},
				})
				if status != http.StatusOK {
					t.Fatalf("POST /sessions = %d %s, want 200", status, raw)
				}
				got, want := decode(t, raw), first[strings.ToLower(r.id)]
				if !reflect.DeepEqual(got, want) {
					t.Errorf("replay = %v, want the first answer %v", got, want)
				}
				if got["model"] != "m1" {
					t.Errorf("model = %v, want m1", got["model"])
				}
			})
		}
	}
	if n := w.count(t, "sessions"); n != 2 {
		t.Errorf("sessions = %d, want 2", n)
	}
}

func TestCreate_RefusesAnIdOfAnotherCreatorOrWorkspace(t *testing.T) {
	w := newWorld(t)
	x, y := sessionID(1), sessionID(2)
	first := w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo, "id": x, "model": "m1"})
	w.open(t, w.Token, map[string]any{"workspace_slug": demo, "id": y})
	cases := []struct {
		name, credential, id, slug string
	}{
		{"dave for other", w.daveKey, x, other},
		{"alice for a fork of demo", w.aliceKey, x, fork1},
		{"the admin token for demo", w.Token, x, demo},
		{"alice for the admin token's session", w.aliceKey, y, demo},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, raw := w.post(t, c.credential, map[string]any{"workspace_slug": c.slug, "id": c.id, "model": "m2"})
			if errorType, _ := errorOf(t, raw); status != http.StatusConflict || errorType != "conflict" {
				t.Errorf("POST /sessions = %d %s, want 409 conflict", status, raw)
			}
		})
	}
	status, raw := w.post(t, w.aliceKey, map[string]any{"workspace_slug": demo, "id": x})
	if status != http.StatusOK || !reflect.DeepEqual(decode(t, raw), first) {
		t.Errorf("replay = %d %s, want 200 with %v", status, raw, first)
	}
	if n := w.count(t, "sessions"); n != 2 {
		t.Errorf("sessions = %d, want 2", n)
	}
}

func TestCreate_ConcurrentCreatesStoreOneSession(t *testing.T) {
	w := newWorld(t)
	const n = 16
	body := map[string]any{"workspace_slug": demo, "id": sessionID(1)}
	statuses, bodies, errs := make([]int, n), make([][]byte, n), make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { statuses[i], bodies[i], errs[i] = w.rawPost(w.aliceKey, body) })
	}
	wg.Wait()
	created, replayed := 0, 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("POST /sessions: %v", errs[i])
		}
		switch statuses[i] {
		case http.StatusCreated:
			created++
		case http.StatusOK:
			replayed++
		default:
			t.Errorf("POST /sessions = %d %s, want 201 or 200", statuses[i], bodies[i])
		}
		if s := decode(t, bodies[i]); s["started_at"] != decode(t, bodies[0])["started_at"] || s["id"] != sessionID(1) {
			t.Errorf("answer %s differs from %s", bodies[i], bodies[0])
		}
	}
	if created != 1 || replayed != n-1 {
		t.Errorf("answers: %d created, %d replayed; want 1 and %d", created, replayed, n-1)
	}
	if got := w.count(t, "sessions"); got != 1 {
		t.Errorf("sessions = %d, want 1", got)
	}
}

func TestCreate_ConfinesABoundTokenToTheWorkspacesItCovers(t *testing.T) {
	w := newWorld(t)
	boundToDemo := w.pat(t, w.alice, demo, "sessions:write")
	cases := []struct {
		name, credential, slug string
		status                 int
	}{
		{"a token bound elsewhere naming demo", w.pat(t, w.alice, other, "sessions:write"), demo, http.StatusNotFound},
		{"a token bound to demo naming a fork it did not create", boundToDemo, fork1, http.StatusNotFound},
		{"a token bound to demo naming demo", boundToDemo, demo, http.StatusCreated},
		{"a token bound to a fork naming the fork", w.pat(t, w.alice, fork2, "sessions:write"), fork2, http.StatusCreated},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if status, raw := w.post(t, c.credential, map[string]any{"workspace_slug": c.slug}); status != c.status {
				t.Errorf("POST /sessions = %d %s, want %d", status, raw, c.status)
			}
		})
	}
}
