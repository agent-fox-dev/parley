package session_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/session"
)

// erinFork is a task fork of demo that erin created, so that erin may open
// a session on it.
const erinFork = "demo--t3"

// unknownID is a UUID that names no session.
const unknownID = "0badc0de-0000-4000-8000-000000000000"

// wantSession fails the test unless GET /sessions/:id answered 200 with the
// session id and every member of the session object.
func wantSession(t *testing.T, status int, raw []byte, id string) {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("GET /sessions/:id = %d %s, want 200", status, raw)
	}
	s := decode(t, raw)
	if s["id"] != id {
		t.Errorf("id = %v, want %s", s["id"], id)
	}
	for _, m := range members {
		if _, ok := s[m]; !ok {
			t.Errorf("member %s is missing from %s", m, raw)
		}
	}
}

// readRoute is a route that reads a session under the session's read rule:
// the session itself, or its usage records.
type readRoute struct{ name, suffix string }

// readRoutes are the routes the read rule holds for.
var readRoutes = []readRoute{{"session", ""}, {"usage", "/usage"}}

// read sends GET of the read route r for the session id as credential.
func (w *world) read(t *testing.T, r readRoute, credential, id string) (int, []byte) {
	t.Helper()
	return w.Call(t, http.MethodGet, "/api/v1/sessions/"+id+r.suffix, credential, nil)
}

// wantReadable fails the test unless a GET of the read route r answered
// 200 for the session id.
func wantReadable(t *testing.T, r readRoute, status int, raw []byte, id string) {
	t.Helper()
	if r.suffix == "" {
		wantSession(t, status, raw, id)
		return
	}
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d %s, want 200", r.name, status, raw)
	}
	if got := decode(t, raw)["session_id"]; got != id {
		t.Errorf("session_id = %v, want %s", got, id)
	}
}

func TestRead_AnswersTheSessionToEveryCallerWhoMayReadIt(t *testing.T) {
	w := newWorld(t)
	w.AddWorkspace(t, access.Record{
		Slug: erinFork, OwnerID: w.erin, OrgID: w.acme, Kind: access.KindFork, ParentSlug: demo, Status: access.StatusReady,
	})
	x := w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo})["id"].(string)
	y := w.open(t, w.erinKey, map[string]any{"workspace_slug": erinFork})["id"].(string)
	cases := []struct {
		name, credential, id string
	}{
		{"alice, its creator, reads X", w.aliceKey, x},
		{"alice's PAT with only sessions:write reads X", w.alicePAT, x},
		{"bob, a member of demo's org, reads X", w.bobKey, x},
		{"the admin token reads X", w.Token, x},
		{"alice, the owner of the fork's parent, reads Y", w.aliceKey, y},
		{"bob, a member of the parent's org, reads Y", w.bobKey, y},
		{"erin, its creator, reads Y", w.erinKey, y},
	}
	for _, archived := range []bool{false, true} {
		if archived {
			w.setStatus(t, demo, access.StatusArchived)
		}
		for _, c := range cases {
			if archived && c.id != x {
				continue
			}
			name := c.name
			if archived {
				name += " of an archived workspace"
			}
			t.Run(name, func(t *testing.T) {
				status, raw := w.get(t, c.credential, c.id)
				wantSession(t, status, raw, c.id)
			})
		}
	}
}

func TestRead_KeepsASessionOfARemovedWorkspaceReadable(t *testing.T) {
	w := newWorld(t)
	y := w.open(t, w.aliceKey, map[string]any{"workspace_slug": fork1})["id"].(string)
	z := w.open(t, w.daveKey, map[string]any{"workspace_slug": other})["id"].(string)
	w.removeWorkspace(t, fork1)
	w.removeWorkspace(t, other)
	cases := []struct {
		name, credential, id, workspace string
		status                          int
	}{
		{"alice, its creator, reads Y of the removed fork", w.aliceKey, y, fork1, http.StatusOK},
		{"bob, a member of the fork parent's org, reads Y", w.bobKey, y, fork1, http.StatusOK},
		{"the admin token reads Y", w.Token, y, fork1, http.StatusOK},
		{"carol, with no access, does not read Y", w.carolKey, y, fork1, http.StatusNotFound},
		{"dave, its creator, reads Z of the removed repository", w.daveKey, z, other, http.StatusOK},
		{"the admin token reads Z", w.Token, z, other, http.StatusOK},
		{"alice, with no access to other, does not read Z", w.aliceKey, z, other, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, raw := w.get(t, c.credential, c.id)
			if c.status == http.StatusNotFound {
				if errorType, message := errorOf(t, raw); status != http.StatusNotFound || errorType != "not_found" || message != "session not found" {
					t.Fatalf("GET /sessions/:id = %d %s, want 404 not_found with message %q", status, raw, "session not found")
				}
				return
			}
			wantSession(t, status, raw, c.id)
			if got := decode(t, raw)["workspace_slug"]; got != c.workspace {
				t.Errorf("workspace_slug = %v, want %s", got, c.workspace)
			}
		})
	}
}

func TestRead_HidesWhetherAnUnreadableSessionExists(t *testing.T) {
	w := newWorld(t, func(d *session.Deps) { d.Coverage = boundCoverage{} })
	x := w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo})["id"].(string)
	boundElsewhere := w.pat(t, w.alice, other, "sessions:read")
	cases := []struct {
		name, credential, id string
	}{
		{"carol on X", w.carolKey, x},
		{"carol on an unknown id", w.carolKey, unknownID},
		{"alice, its creator, with a token bound elsewhere on X", boundElsewhere, x},
		{"alice with a token bound elsewhere on an unknown id", boundElsewhere, unknownID},
	}
	var bodies [][]byte
	for _, r := range readRoutes {
		for _, c := range cases {
			t.Run(r.name+"/"+c.name, func(t *testing.T) {
				status, raw := w.read(t, r, c.credential, c.id)
				errorType, message := errorOf(t, raw)
				if status != http.StatusNotFound || errorType != "not_found" || message != "session not found" {
					t.Fatalf("GET %s = %d %s, want 404 not_found with message %q", r.name, status, raw, "session not found")
				}
				bodies = append(bodies, raw)
			})
		}
	}
	for i := 1; i < len(bodies); i++ {
		if string(bodies[i]) != string(bodies[0]) {
			t.Errorf("404 bodies differ: %s and %s", bodies[0], bodies[i])
		}
	}
	boundToDemo := w.pat(t, w.alice, demo, "sessions:read")
	for _, r := range readRoutes {
		t.Run(r.name+"/alice with a token bound to demo reads X", func(t *testing.T) {
			status, raw := w.read(t, r, boundToDemo, x)
			wantReadable(t, r, status, raw, x)
		})
	}
}

func TestRead_RefusesAMalformedIdAndAMissingScope(t *testing.T) {
	w := newWorld(t)
	x := w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo})["id"].(string)
	workspacesOnly := w.pat(t, w.alice, "", "workspaces:read")
	cases := []struct {
		name, credential, id string
		status               int
		errorType            string
	}{
		{"an id that is not a UUID", w.aliceKey, "abc", http.StatusBadRequest, "invalid_request"},
		{"a 36-character id holding a g", w.aliceKey, "g" + x[1:], http.StatusBadRequest, "invalid_request"},
		{"a PAT with only workspaces:read on X", workspacesOnly, x, http.StatusForbidden, "insufficient_scope"},
		{"a PAT with only workspaces:read on an unknown id", workspacesOnly, unknownID, http.StatusForbidden, "insufficient_scope"},
		{"the id of X in upper case", w.aliceKey, strings.ToUpper(x), http.StatusOK, ""},
		{"a PAT with only sessions:read on X", w.pat(t, w.alice, "", "sessions:read"), x, http.StatusOK, ""},
	}
	for _, r := range readRoutes {
		for _, c := range cases {
			t.Run(r.name+"/"+c.name, func(t *testing.T) {
				status, raw := w.read(t, r, c.credential, c.id)
				if c.status == http.StatusOK {
					wantReadable(t, r, status, raw, x)
					return
				}
				if errorType, _ := errorOf(t, raw); status != c.status || errorType != c.errorType {
					t.Errorf("GET %s = %d %s, want %d %s", r.name, status, raw, c.status, c.errorType)
				}
			})
		}
	}
}
