package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// startBoundTokensHub starts a hub with cfg, as hubtest.StartWith does, with
// one more route, which stands in for the routes of the packages that serve
// a workspace: GET /api/v1/workspaces/:slug/probe needs read on the
// workspace and the scope git:read, and answers 200 with the slug.
func startBoundTokensHub(t *testing.T, cfg hubtest.Config) *hubtest.Hub {
	t.Helper()
	compose := cfg.Compose
	cfg.Compose = func(c *hub.Composition) {
		c.AddRoute(server.Route{
			Method: http.MethodGet, Pattern: "/workspaces/:slug/probe", Scope: "git:read",
			Marks: map[string]string{access.MarkNeed: string(access.NeedRead)},
			Handler: func(w http.ResponseWriter, r *http.Request) error {
				server.WriteJSON(w, http.StatusOK, map[string]string{"slug": server.Param(r, "slug")})
				return nil
			},
		})
		if compose != nil {
			compose(c)
		}
	}
	return hubtest.StartWith(t, cfg)
}

// errorType is the error_type of an error envelope, "" for any other body.
func errorType(body []byte) string {
	var e struct {
		Error struct {
			ErrorType string `json:"error_type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	return e.Error.ErrorType
}

// logRecord is the first line of the hub's log with the message msg whose
// members hold every member of want, or nil.
func logRecord(h *hubtest.Hub, msg string, want map[string]any) map[string]any {
	for line := range strings.SplitSeq(h.Log.String(), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil || rec["msg"] != msg {
			continue
		}
		match := true
		for k, v := range want {
			match = match && rec[k] == v
		}
		if match {
			return rec
		}
	}
	return nil
}

// tokenEvent is the part of a hub event record about a token that the
// flows read.
type tokenEvent struct {
	Workspace *string        `json:"workspace"`
	Metadata  map[string]any `json:"metadata"`
}

// tokenEvents are the events of type typ about the token tokenID in the
// hub's event log, oldest first, read as the admin token.
func tokenEvents(t *testing.T, h *hubtest.Hub, typ, tokenID string) []tokenEvent {
	t.Helper()
	status, body := h.Call(t, http.MethodGet, "/api/v1/events/log?after=0", h.Token, nil)
	var page struct {
		Items []struct {
			tokenEvent
			Type       string `json:"type"`
			ResourceID string `json:"resource_id"`
		} `json:"items"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &page) != nil {
		t.Fatalf("GET /api/v1/events/log?after=0 = %d %s, want 200 with a page", status, body)
	}
	var events []tokenEvent
	for _, it := range page.Items {
		if it.Type == typ && it.ResourceID == tokenID {
			events = append(events, it.tokenEvent)
		}
	}
	return events
}

// aliceWithTwoWorkspaces adds the user alice, who owns the workspaces w1
// and w5, to h and returns her id.
func aliceWithTwoWorkspaces(t *testing.T, h *hubtest.Hub) string {
	t.Helper()
	alice := h.CreateUser(t, "alice")
	org := h.CreateOrg(t, "acme")
	for _, slug := range []string{"w1", "w5"} {
		h.AddWorkspace(t, access.Record{Slug: slug, OwnerID: alice, OrgID: org, Kind: access.KindRepository, Status: access.StatusReady})
	}
	return alice
}

func TestBoundTokens_ADeveloperMintsABoundTokenFromTheCLIThatConfinesTheAgent(t *testing.T) {
	h := startBoundTokensHub(t, hubtest.Config{})
	clientEnv(t, t.TempDir())
	parley := &identityHub{h}
	key := h.IssueKey(t, aliceWithTwoWorkspaces(t, h))

	created := parley.must(t, key, "tokens", "create", "--name", "agent", "--permissions", "git:read", "--workspace", "w1")
	tok := created.json(t)
	agent, _ := tok["token"].(string)
	id, _ := tok["token_id"].(string)
	if tok["workspace"] != "w1" || agent == "" || id == "" {
		t.Fatalf("tokens create --workspace w1 printed %s, want a token bound to w1 with its value", created.stdout)
	}

	if status, body := h.Call(t, http.MethodGet, "/api/v1/workspaces/w1/probe", agent, nil); status != http.StatusOK {
		t.Errorf("GET w1's probe with the bound token = %d %s, want 200", status, body)
	}
	if status, body := h.Call(t, http.MethodGet, "/api/v1/workspaces/w5/probe", agent, nil); status != http.StatusNotFound || errorType(body) != "not_found" {
		t.Errorf("GET w5's probe with the bound token = %d %s, want 404 not_found", status, body)
	}
	if status, body := h.Call(t, http.MethodGet, "/api/v1/user", agent, nil); status != http.StatusForbidden || errorType(body) != "token_bound" {
		t.Errorf("GET /api/v1/user with the bound token = %d %s, want 403 token_bound", status, body)
	}

	listed := parley.must(t, key, "tokens", "list", "--workspace", "w1")
	items, _ := listed.json(t)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("tokens list --workspace w1 printed %s, want one item", listed.stdout)
	}
	item, _ := items[0].(map[string]any)
	if _, shown := item["token"]; item["token_id"] != id || shown {
		t.Errorf("tokens list --workspace w1 printed %v, want the token %s without its value", item, id)
	}
	if n := len(tokenEvents(t, h, "parley.token.bind", id)); n != 1 {
		t.Errorf("the event log holds %d parley.token.bind events for the token, want 1", n)
	}
}

func TestBoundTokens_AnAgentLeavesItsBindingRepeatedlyAndTheOwnerGetsOneEventAMinute(t *testing.T) {
	clock := &fakeNow{now: time.Now()}
	h := startBoundTokensHub(t, hubtest.Config{Now: clock.Now})
	agent := h.IssuePAT(t, identity.PATSpec{UserID: aliceWithTwoWorkspaces(t, h), Permissions: []string{"git:read"}, Workspace: "w1"})
	var id string
	if err := h.Store.Main().QueryRow("SELECT token_id FROM pats WHERE workspace = 'w1'").Scan(&id); err != nil {
		t.Fatalf("read the bound token's id: %v", err)
	}

	requests := []struct {
		path   string
		status int
		// later is how long after the request before it this one is sent.
		later time.Duration
	}{
		{"/api/v1/workspaces/w5/probe", http.StatusNotFound, 0},
		{"/api/v1/workspaces/nosuch/probe", http.StatusNotFound, 0},
		{"/api/v1/user/tokens", http.StatusForbidden, 0},
		{"/api/v1/workspaces/w5/probe", http.StatusNotFound, 61 * time.Second},
	}
	for _, r := range requests {
		clock.advance(r.later)
		if status, body := h.Call(t, http.MethodGet, r.path, agent, nil); status != r.status {
			t.Errorf("GET %s with the bound token = %d %s, want %d", r.path, status, body, r.status)
		}
	}

	events := tokenEvents(t, h, "parley.token.bound_refused", id)
	if len(events) != 2 {
		t.Fatalf("the event log holds %d parley.token.bound_refused events for the token, want 2: %+v", len(events), events)
	}
	for i, suppressed := range []float64{0, 2} {
		e := events[i]
		if e.Workspace == nil || *e.Workspace != "w1" || e.Metadata["suppressed"] != suppressed {
			t.Errorf("event %d = %+v, want workspace w1 and suppressed %v", i+1, e, suppressed)
		}
	}
	if got := events[0].Metadata["requested_workspace"]; got != "w5" {
		t.Errorf("the first event's requested_workspace = %v, want w5", got)
	}
}

func TestBoundTokens_AMaintenancePassRevokesATokenWhoseWorkspaceIsGone(t *testing.T) {
	h := startBoundTokensHub(t, hubtest.Config{})
	alice := h.CreateUser(t, "alice")
	h.AddWorkspace(t, access.Record{Slug: "w1", OwnerID: alice, OrgID: h.CreateOrg(t, "acme"), Kind: access.KindRepository, Status: access.StatusReady})
	live := h.IssuePAT(t, identity.PATSpec{UserID: alice, Permissions: []string{"git:read"}, Workspace: "w1"})
	// No record of gone was ever stored, as when a removal left its token
	// behind.
	orphan := h.IssuePAT(t, identity.PATSpec{UserID: alice, Permissions: []string{"git:read"}, Workspace: "gone"})
	revoked := func() bool {
		var n int
		if err := h.Store.Main().QueryRow("SELECT count(*) FROM pats WHERE workspace = 'gone' AND revoked_at IS NOT NULL").Scan(&n); err != nil {
			t.Fatalf("read the token bound to gone: %v", err)
		}
		return n == 1
	}
	if status, body := h.Call(t, http.MethodGet, "/api/v1/workspaces/gone/probe", orphan, nil); status != http.StatusNotFound {
		t.Fatalf("GET gone's probe with its live token = %d %s, want 404", status, body)
	}

	// A pass whose sweep cannot write names the task in its failure line,
	// and the hub serves on.
	if _, err := h.Store.Main().Exec(`CREATE TRIGGER fail_update BEFORE UPDATE ON pats
BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatalf("create the failing trigger: %v", err)
	}
	h.Restart(t)
	failed := map[string]any{"level": "ERROR", "task": "token_binding_sweep"}
	waitUntil(10*time.Second, func() bool { return logRecord(h, "maintenance task failed", failed) != nil })
	if logRecord(h, "maintenance task failed", failed) == nil || revoked() {
		t.Fatalf("a pass with a failing store: log %q, want a failure of token_binding_sweep and the token left live", h.Log.String())
	}

	if _, err := h.Store.Main().Exec("DROP TRIGGER fail_update"); err != nil {
		t.Fatalf("drop the failing trigger: %v", err)
	}
	h.Restart(t)
	waitUntil(10*time.Second, revoked)
	if !revoked() {
		t.Fatalf("after a pass the token bound to gone is not revoked; log %q", h.Log.String())
	}
	if logRecord(h, "revoked the tokens bound to workspaces that no longer exist", map[string]any{"level": "INFO", "count": 1.0}) == nil {
		t.Errorf("log %q, want one INFO line with count 1", h.Log.String())
	}
	if status, body := h.Call(t, http.MethodGet, "/api/v1/workspaces/gone/probe", orphan, nil); status != http.StatusUnauthorized || errorType(body) != "unauthenticated" {
		t.Errorf("GET gone's probe with the swept token = %d %s, want 401 unauthenticated", status, body)
	}
	if status, body := h.Call(t, http.MethodGet, "/api/v1/workspaces/w1/probe", live, nil); status != http.StatusOK {
		t.Errorf("GET w1's probe with its token = %d %s, want 200", status, body)
	}
}
