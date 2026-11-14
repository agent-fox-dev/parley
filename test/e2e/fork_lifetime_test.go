package e2e

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// The tests of this file set environment variables, so they must not run in
// parallel.

// loggedEvent is an event of the hub's event log as a reader gets it.
type loggedEvent struct {
	Type      string          `json:"type"`
	Workspace *string         `json:"workspace"`
	ActorType string          `json:"actor_type"`
	Metadata  json.RawMessage `json:"metadata"`
}

// eventsOf are the events of the workspace slug that the developer reads in
// the event log, oldest first, by type.
func (h *forkHub) eventsOf(t *testing.T, slug string) map[string][]loggedEvent {
	t.Helper()
	status, body := h.Call(t, http.MethodGet, "/api/v1/events/log?after=0", h.key, nil)
	var page struct {
		Items []loggedEvent `json:"items"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &page) != nil {
		t.Fatalf("GET /api/v1/events/log?after=0 = %d %s, want 200 with a page", status, body)
	}
	byType := map[string][]loggedEvent{}
	for _, e := range page.Items {
		if e.Workspace != nil && *e.Workspace == slug {
			byType[e.Type] = append(byType[e.Type], e)
		}
	}
	return byType
}

// sweep has the fork sweep run one pass and waits until it has ended: the
// second tick is taken only once the pass of the first has ended.
func (h *forkHub) sweep(t *testing.T) {
	t.Helper()
	for range 2 {
		select {
		case h.ticks <- time.Now():
		case <-time.After(10 * time.Second):
			t.Fatal("the fork sweep took no tick within 10 s")
		}
	}
}

// expiresAt is the expires_at of the workspace object w.
func expiresAt(t *testing.T, w map[string]any) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, str(w, "expires_at"))
	if err != nil {
		t.Fatalf("expires_at of %v: %v", w, err)
	}
	return at
}

func TestForkLifetime_AnAgentExtendsItsForkAndDeletesItFromTheShell(t *testing.T) {
	h := startForkHub(t)
	parentDir := h.repo("demo")
	parentRefs := gitrepotest.Refs(t, parentDir, "")
	status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/forks", h.key, map[string]any{
		"name":  "t1",
		"token": map[string]any{"permissions": []string{"git:read", "git:write", "workspaces:read", "workspaces:write"}},
	})
	if status != http.StatusCreated {
		t.Fatalf("create demo--t1 = %d %s, want 201", status, body)
	}
	created := decoded(t, body)
	slug, token, tokenID := str(created, "workspace", "slug"), str(created, "token", "token"), str(created, "token", "token_id")
	gitrepotest.PushCommit(t, parentDir, h.repo(slug), "work", "work\n")

	ran := time.Now()
	r := h.runAs(t, token, "fork", "extend", slug, "--ttl", "2d")
	if r.code != 0 {
		t.Fatalf("fork extend exited %d, stderr %q; want 0", r.code, r.stderr)
	}
	if got, want := expiresAt(t, r.json(t)), ran.Add(48*time.Hour); got.Sub(want).Abs() > 5*time.Second {
		t.Errorf("printed expires_at = %s, want two days after the command ran, %s", got, want)
	}

	// A bound token is refused the route 403 token_bound while it is live.
	if status, body := h.Call(t, http.MethodGet, "/api/v1/user", token, nil); status != http.StatusForbidden {
		t.Fatalf("GET /api/v1/user with the fork's bound token before the delete = %d %s, want 403", status, body)
	}
	r = h.runAs(t, token, "fork", "delete", slug)
	if r.code != 0 || r.stdout != "{\n  \"status\": \"deleted\",\n  \"slug\": \"demo--t1\"\n}\n" {
		t.Fatalf("fork delete exited %d with stdout %q, stderr %q; want 0 and the confirmation of demo--t1", r.code, r.stdout, r.stderr)
	}
	if status, body := h.Call(t, http.MethodGet, "/api/v1/workspaces/"+slug, h.key, nil); status != http.StatusNotFound {
		t.Errorf("GET %s as its owner = %d %s, want 404", slug, status, body)
	}
	for _, path := range []string{"/api/v1/workspaces/" + slug, "/api/v1/user"} {
		if status, body := h.Call(t, http.MethodGet, path, token, nil); status != http.StatusUnauthorized {
			t.Errorf("GET %s with the fork's bound token = %d %s, want 401", path, status, body)
		}
	}

	events := h.eventsOf(t, slug)
	if n := len(events["parley.workspace.update"]); n != 1 {
		t.Errorf("update events of %s = %d, want 1", slug, n)
	}
	deletes := events["parley.workspace.delete"]
	if len(deletes) != 1 {
		t.Fatalf("delete events of %s = %d, want 1", slug, len(deletes))
	}
	var meta struct {
		RevokedTokens []string `json:"revoked_tokens"`
	}
	if err := json.Unmarshal(deletes[0].Metadata, &meta); err != nil || !slices.Equal(meta.RevokedTokens, []string{tokenID}) {
		t.Errorf("the delete event's metadata = %s, want revoked_tokens [%s]", deletes[0].Metadata, tokenID)
	}
	if got := gitrepotest.Refs(t, parentDir, ""); !maps.Equal(got, parentRefs) {
		t.Errorf("demo's refs = %v, want them unchanged: %v", got, parentRefs)
	}
}

func TestForkLifetime_TheSweepRemovesAForkWhoseLifetimeHasEnded(t *testing.T) {
	clock := &fakeNow{now: time.Now().Truncate(time.Second)}
	h := startForkHubAt(t, clock.Now)
	parentRefs := gitrepotest.Refs(t, h.repo("demo"), "")
	status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/forks", h.key, map[string]any{"name": "t2", "ttl": "5m"})
	if status != http.StatusCreated {
		t.Fatalf("create demo--t2 = %d %s, want 201", status, body)
	}
	w, _ := decoded(t, body)["workspace"].(map[string]any)
	createdAt, err := time.Parse(time.RFC3339, str(w, "created_at"))
	if err != nil || expiresAt(t, w).Sub(createdAt) != 5*time.Minute {
		t.Errorf("created_at %q, expires_at %q; want expires_at five minutes after created_at", str(w, "created_at"), str(w, "expires_at"))
	}

	clock.advance(6 * time.Minute)
	h.sweep(t)

	if status, body := h.Call(t, http.MethodGet, "/api/v1/workspaces/demo--t2", h.key, nil); status != http.StatusNotFound {
		t.Errorf("GET demo--t2 after the sweep = %d %s, want 404", status, body)
	}
	events := h.eventsOf(t, "demo--t2")
	expiries := events["parley.fork.expire"]
	if len(expiries) != 1 {
		t.Fatalf("expire events of demo--t2 = %d, want 1", len(expiries))
	}
	var meta struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(expiries[0].Metadata, &meta); err != nil || meta.Reason != "ttl" || expiries[0].ActorType != "system" {
		t.Errorf("the expire event has actor %q and metadata %s, want system and reason ttl", expiries[0].ActorType, expiries[0].Metadata)
	}
	if n := len(events["parley.workspace.delete"]); n != 0 {
		t.Errorf("delete events of demo--t2 = %d, want none", n)
	}
	if status, body := h.Call(t, http.MethodGet, "/api/v1/workspaces/demo", h.key, nil); status != http.StatusOK {
		t.Errorf("GET demo = %d %s, want 200", status, body)
	}
	if got := gitrepotest.Refs(t, h.repo("demo"), ""); !maps.Equal(got, parentRefs) {
		t.Errorf("demo's refs = %v, want them unchanged: %v", got, parentRefs)
	}
}
