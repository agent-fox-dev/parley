package sandbox_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/workspace"
)

// strs returns n strings "a0", "a1", ….
func strs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "a" + strconv.Itoa(i)
	}
	return out
}

// entries returns a map of n entries "K0": "v", "K1": "v", ….
func entries(n int) map[string]string {
	out := make(map[string]string, n)
	for i := range n {
		out["K"+strconv.Itoa(i)] = "v"
	}
	return out
}

// wantRefused fails the test unless a is status with error type typ and
// the hub's footprint is still before.
func (h *createHub) wantRefused(t *testing.T, a answer, status int, typ string, before footprint) {
	t.Helper()
	if a.status != status || a.err.ErrorType != typ {
		t.Errorf("create = %d %s; want %d %s", a.status, a.body, status, typ)
	}
	if after := h.footprint(t); after != before {
		t.Errorf("a refused create left %+v; want %+v", after, before)
	}
}

func TestCreate_RefusesInvalidBody(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile+plainProfile)
	h.world(t)
	none := h.seedIn(t, "demo--t1", nil)
	failed := h.seedIn(t, "demo--t1", json.RawMessage(`{"ok":false}`))
	saved := h.seedIn(t, "demo--t1", json.RawMessage(`{"ok":true}`))
	elsewhere := h.seedIn(t, "demo", json.RawMessage(`{"ok":true}`))
	refused := []struct {
		name string
		slug string
		body any
	}{
		{"a list", "", "[]"},
		{"a string", "", `"x"`},
		{"an unknown profile", "", map[string]any{"profile": "nope"}},
		{"a profile that is no string", "", map[string]any{"profile": 7}},
		{"a ttl under a minute", "", map[string]any{"ttl": "59s"}},
		{"a ttl over the profile's", "", map[string]any{"ttl": "4h1m"}},
		{"a ttl that is no duration", "", map[string]any{"ttl": "soon"}},
		{"a ttl that is a number", "", map[string]any{"ttl": 60}},
		{"an empty command", "", map[string]any{"command": []string{}}},
		{"a command that is a string", "", map[string]any{"command": "make"}},
		{"a command with an empty argument", "", map[string]any{"command": []string{"make", ""}}},
		{"a command of 257 arguments", "", map[string]any{"command": strs(257)}},
		{"an env of 65 entries", "", map[string]any{"env": entries(65)}},
		{"an env entry over 8 KiB", "", map[string]any{"env": map[string]string{"A": strings.Repeat("x", 8192)}}},
		{"an env value that is no string", "", map[string]any{"env": map[string]any{"A": 1}}},
		{"an env key starting with a digit", "", map[string]any{"env": map[string]string{"1A": "x"}}},
		{"an env key with a hyphen", "", map[string]any{"env": map[string]string{"A-B": "x"}}},
		{"an env key starting PARLEY_", "", map[string]any{"env": map[string]string{"PARLEY_X": "x"}}},
		{"an env key starting OPENSHELL_", "", map[string]any{"env": map[string]string{"OPENSHELL_X": "x"}}},
		{"an env key the profile exposes", "", map[string]any{"profile": "plain", "env": map[string]string{"NPM_TOKEN": "x"}}},
		{"17 labels", "", map[string]any{"labels": entries(17)}},
		{"an empty label key", "", map[string]any{"labels": map[string]string{"": "v"}}},
		{"a label key starting with a hyphen", "", map[string]any{"labels": map[string]string{"-a": "v"}}},
		{"a label key ending with a hyphen", "", map[string]any{"labels": map[string]string{"a-": "v"}}},
		{"a label key starting parley.", "", map[string]any{"labels": map[string]string{"parley.x": "v"}}},
		{"a label key starting openshell.", "", map[string]any{"labels": map[string]string{"openshell.x": "v"}}},
		{"a label key of 64 characters", "", map[string]any{"labels": map[string]string{strings.Repeat("a", 64): "v"}}},
		{"a label value of 257 bytes", "", map[string]any{"labels": map[string]string{"a": strings.Repeat("x", 257)}}},
		{"a label value with a control character", "", map[string]any{"labels": map[string]string{"a": "x\u0001"}}},
		{"a purpose of 201 characters", "", map[string]any{"purpose": strings.Repeat("x", 201)}},
		{"a purpose that is no string", "", map[string]any{"purpose": 3}},
		{"checkout true", "", map[string]any{"checkout": true}},
		{"checkout a string", "", map[string]any{"checkout": "main"}},
		{"a checkout ref that is no string", "", map[string]any{"checkout": map[string]any{"ref": 5}}},
		{"a checkout ref that names no commit", "", map[string]any{"checkout": map[string]any{"ref": "nope"}}},
		{"an invalid checkout branch", "", map[string]any{"checkout": map[string]any{"branch": "a..b"}}},
		{"a checkout branch on a repository", "demo", map[string]any{"checkout": map[string]any{"branch": "work"}}},
		{"restore_from of no sandbox", "", map[string]any{"restore_from": "sbx_" + strings.Repeat("0", 32)}},
		{"restore_from of another workspace", "", map[string]any{"restore_from": elsewhere}},
		{"restore_from without a checkpoint", "", map[string]any{"restore_from": none}},
		{"restore_from whose checkpoint failed", "", map[string]any{"restore_from": failed}},
		{"restore_from without a checkout", "", map[string]any{"restore_from": saved, "checkout": false}},
		{"restore_from without hub access", "", map[string]any{"profile": "plain", "restore_from": saved}},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			slug := tc.slug
			if slug == "" {
				slug = "demo--t1"
			}
			before := h.footprint(t)
			h.wantRefused(t, h.post(t, slug, tc.body), http.StatusBadRequest, "invalid_request", before)
		})
	}

	accepted := []struct {
		name string
		body map[string]any
	}{
		{"a ttl of a minute", map[string]any{"ttl": "1m"}},
		{"the profile's ttl", map[string]any{"ttl": "4h"}},
		{"a command of 256 arguments", map[string]any{"command": strs(256)}},
		{"an env of 64 entries", map[string]any{"env": entries(64)}},
		{"an env entry of 8 KiB", map[string]any{"env": map[string]string{"A": strings.Repeat("x", 8191)}}},
		{"16 labels", map[string]any{"labels": entries(16)}},
		{"a label key of 63 characters", map[string]any{"labels": map[string]string{strings.Repeat("a", 63): "v"}}},
		{"a label value of 256 bytes", map[string]any{"labels": map[string]string{"a": strings.Repeat("x", 256)}}},
		{"a purpose of 200 characters", map[string]any{"purpose": strings.Repeat("x", 200)}},
		{"checkout false", map[string]any{"checkout": false}},
		{"an empty checkout", map[string]any{"checkout": map[string]any{}}},
		{"a checkout ref and branch", map[string]any{"checkout": map[string]any{"ref": "main", "branch": "work"}}},
		{"restore_from of a saved checkpoint", map[string]any{"restore_from": saved}},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			sandboxObject(t, h.post(t, "demo--t1", tc.body))
		})
	}
}

func TestCreate_NoDefaultProfileListsNames(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, "[[sandbox.profiles]]\nname = \"agent\"\nttl = \"4h\"\n[[sandbox.profiles]]\nname = \"gpu-large\"\nttl = \"4h\"\n")
	h.repository(t, "demo", workspace.StatusReady)
	before := h.footprint(t)
	a := h.post(t, "demo", "{}")
	h.wantRefused(t, a, http.StatusBadRequest, "invalid_request", before)
	for _, name := range []string{"agent", "gpu-large"} {
		if !strings.Contains(a.err.Message, name) {
			t.Errorf("message %q does not name the profile %s", a.err.Message, name)
		}
	}
}

func TestCreate_RefusesUnknownSession(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile)
	h.world(t)
	const session = "6f1c2f0e-0000-4000-8000-000000000001"
	body := map[string]any{"session_id": session}
	for _, tc := range []struct {
		name  string
		setup func()
	}{
		{"a linker that refuses", func() { h.linker.setRefuse(true) }},
		{"no linker", func() { h.svc.RegisterSessionLinker(nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup()
			before := h.footprint(t)
			h.wantRefused(t, h.post(t, "demo--t1", body), http.StatusBadRequest, "unknown_session", before)
		})
	}

	h.linker.setRefuse(false)
	h.svc.RegisterSessionLinker(h.linker)
	s := sandboxObject(t, h.post(t, "demo--t1", body))
	if s["session_id"] != session {
		t.Errorf("session_id = %v; want %s", s["session_id"], session)
	}
	check, record := h.linker.last()
	if want := session + " " + h.dana + " demo--t1"; check != want {
		t.Errorf("the linker checked %q; want %q", check, want)
	}
	if want := session + " demo--t1 " + s["id"].(string); record != want {
		t.Errorf("the linker recorded %q; want %q", record, want)
	}
}

func TestCreate_RefusesByWorkspaceState(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile)
	h.repository(t, "old", workspace.StatusArchived)
	h.fork(t, "old--t1", "old", recordStart.Add(24*time.Hour), nil)
	h.repository(t, "new", workspace.StatusCloning)
	h.repository(t, "bad", workspace.StatusFailed)
	for _, tc := range []struct{ slug, want string }{
		{"old", "workspace_archived"},
		{"old--t1", "parent_archived"},
		// The state guard of the workspaces answers a clone that has not
		// finished as busy.
		{"new", "workspace_busy"},
		{"bad", "workspace_not_ready"},
	} {
		t.Run(tc.slug, func(t *testing.T) {
			before := h.footprint(t)
			h.wantRefused(t, h.post(t, tc.slug, map[string]any{}), http.StatusConflict, tc.want, before)
		})
	}
}
