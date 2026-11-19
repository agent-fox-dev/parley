package telemetry_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

// TestIngestionGate runs every check of the ingestion gate over every
// route registered through it, on one hub. The subtests run in order and
// each compares the record count before and after its refused requests.
func TestIngestionGate(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	auditRead := w.pat(t, w.alice, "audit:read")
	sessionsWrite := w.pat(t, w.alice, "sessions:write")

	t.Run("the admin token, the owner's key and the owner's PAT with audit:write store", func(t *testing.T) {
		callers := []struct{ name, credential string }{
			{"admin token", w.Token}, {"owner API key", w.aliceKey}, {"owner PAT", w.alicePAT},
		}
		for _, rt := range ingestionRoutes {
			for _, c := range callers {
				status, raw := w.post(t, c.credential, demo, w.freshRun(), rt.path, rt.valid())
				if status != rt.stored() {
					t.Errorf("%s as %s = %d %s, want %d", rt.path, c.name, status, raw, rt.stored())
				}
			}
		}
	})

	t.Run("a PAT without audit:write is refused whether or not the workspace exists", func(t *testing.T) {
		before := w.countAllRecords(t)
		for _, rt := range ingestionRoutes {
			for _, pat := range []string{auditRead, sessionsWrite} {
				for _, slug := range []string{demo, nowhere} {
					status, raw := w.post(t, pat, slug, someRunID, rt.path, rt.valid())
					if typ, _ := errorOf(t, raw); status != http.StatusForbidden || typ != "insufficient_scope" {
						t.Errorf("%s on %s = %d %s, want 403 insufficient_scope", rt.path, slug, status, raw)
					}
				}
			}
		}
		if n := w.countAllRecords(t); n != before {
			t.Errorf("records = %d after refused requests, want %d", n, before)
		}
	})

	t.Run("an unknown workspace and one the caller cannot reach answer the same not found", func(t *testing.T) {
		before := w.countAllRecords(t)
		for _, rt := range ingestionRoutes {
			answers := map[string][]byte{}
			for name, req := range map[string]struct{ credential, slug string }{
				"admin token on an unknown slug": {w.Token, nowhere},
				"outsider on an unknown slug":    {w.carolKey, nowhere},
				"outsider on demo":               {w.carolKey, demo},
			} {
				status, raw := w.post(t, req.credential, req.slug, someRunID, rt.path, rt.valid())
				typ, msg := errorOf(t, raw)
				if status != http.StatusNotFound || typ != "not_found" || msg != "workspace not found" {
					t.Errorf("%s as %s = %d %s, want 404 not_found 'workspace not found'", rt.path, name, status, raw)
				}
				answers[name] = raw
			}
			if !bytes.Equal(answers["outsider on an unknown slug"], answers["outsider on demo"]) {
				t.Errorf("%s: outsider answers differ: %s and %s", rt.path, answers["outsider on an unknown slug"], answers["outsider on demo"])
			}
		}
		if n := w.countAllRecords(t); n != before {
			t.Errorf("records = %d after refused requests, want %d", n, before)
		}
	})

	t.Run("an org member who is not the owner is forbidden, also on an archived workspace", func(t *testing.T) {
		before := w.countAllRecords(t)
		for _, rt := range ingestionRoutes {
			for _, slug := range []string{demo, old} {
				status, raw := w.post(t, w.bobKey, slug, w.freshRun(), rt.path, rt.valid())
				if typ, _ := errorOf(t, raw); status != http.StatusForbidden || typ != "forbidden" {
					t.Errorf("%s on %s = %d %s, want 403 forbidden", rt.path, slug, status, raw)
				}
			}
		}
		if n := w.countAllRecords(t); n != before {
			t.Errorf("records = %d after refused requests, want %d", n, before)
		}
	})

	t.Run("a malformed run id, or a body run_id other than the path's, is invalid", func(t *testing.T) {
		before := w.countAllRecords(t)
		invalid := func(t *testing.T, status int, raw []byte, what string) {
			t.Helper()
			typ, msg := errorOf(t, raw)
			if status != http.StatusBadRequest || typ != "invalid_request" || !strings.Contains(msg, "run_id") {
				t.Errorf("%s = %d %s, want 400 invalid_request naming run_id", what, status, raw)
			}
		}
		badRuns := []string{
			"run1", "20260102_030405_A1B2C3", "20260102_030405_a1b2c", "2026010_030405_a1b2c3",
			"20260102-030405-a1b2c3", "20260102_030405_a1b2c3x",
		}
		for _, rt := range ingestionRoutes {
			for _, run := range badRuns {
				status, raw := w.post(t, w.aliceKey, demo, run, rt.path, rt.valid())
				invalid(t, status, raw, rt.path+" on run "+run)
			}
			if rt.batch {
				continue
			}
			body := rt.valid().(map[string]any)
			body["run_id"] = "20260102_030405_ffffff"
			status, raw := w.post(t, w.aliceKey, demo, someRunID, rt.path, body)
			invalid(t, status, raw, rt.path+" with another body run_id")
		}
		if n := w.countAllRecords(t); n != before {
			t.Errorf("records = %d after refused requests, want %d", n, before)
		}
		for _, rt := range ingestionRoutes {
			if rt.batch {
				continue
			}
			run := w.freshRun()
			body := rt.valid().(map[string]any)
			body["run_id"] = run
			if status, raw := w.post(t, w.aliceKey, demo, run, rt.path, body); status != http.StatusCreated {
				t.Errorf("%s with the path's body run_id = %d %s, want 201", rt.path, status, raw)
			}
		}
	})

	t.Run("an archived workspace refuses a valid request from a caller with write access", func(t *testing.T) {
		before := w.countAllRecords(t)
		for _, rt := range ingestionRoutes {
			for _, credential := range []string{w.aliceKey, w.Token} {
				status, raw := w.post(t, credential, old, w.freshRun(), rt.path, rt.valid())
				if typ, _ := errorOf(t, raw); status != http.StatusConflict || typ != "workspace_archived" {
					t.Errorf("%s on old = %d %s, want 409 workspace_archived", rt.path, status, raw)
				}
			}
		}
		if n := w.countAllRecords(t); n != before {
			t.Errorf("records = %d after refused requests, want %d", n, before)
		}
	})

	t.Run("every status but archived, and a task fork, accept ingestion under the path's slug", func(t *testing.T) {
		for _, slug := range []string{pending, cloning, demo, failed, demoFork} {
			status, raw := w.post(t, w.aliceKey, slug, w.freshRun(), "/events", map[string]any{"event_type": "run.start"})
			if status != http.StatusCreated {
				t.Errorf("/events on %s = %d %s, want 201", slug, status, raw)
				continue
			}
			if got := decode(t, raw)["workspace"]; got != slug {
				t.Errorf("/events on %s stored under workspace %v", slug, got)
			}
		}
	})

	t.Run("the first failing check answers: scope, access, validation, archived", func(t *testing.T) {
		cases := []struct {
			name             string
			credential, slug string
			run              string
			body             any
			status           int
			errorType        string
		}{
			{"no scope on an unknown slug with a bad run id", auditRead, nowhere, "bad", map[string]any{"event_type": "x"}, http.StatusForbidden, "insufficient_scope"},
			{"an outsider with a bad run id", w.carolKey, demo, "bad", map[string]any{"event_type": "x"}, http.StatusNotFound, "not_found"},
			{"the owner with an invalid body on an archived workspace", w.aliceKey, old, someRunID, map[string]any{}, http.StatusBadRequest, "invalid_request"},
		}
		for _, c := range cases {
			status, raw := w.post(t, c.credential, c.slug, c.run, "/events", c.body)
			if typ, _ := errorOf(t, raw); status != c.status || typ != c.errorType {
				t.Errorf("%s = %d %s, want %d %s", c.name, status, raw, c.status, c.errorType)
			}
		}
	})
}
