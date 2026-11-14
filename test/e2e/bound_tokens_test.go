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
