package fork_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/server"
)

func TestCreate_PassesSandboxThrough(t *testing.T) {
	t.Parallel()
	s := &sandboxer{obj: json.RawMessage(`{"id":"sbx_1","state":"provisioning"}`)}
	w := newWorld(t, collaborate(&linker{}, s))
	sid := uuid.New().String()
	const member = `{"profile": "default", "command": ["make", "test"], "extra": 1}`
	body, ws := created(t, w.post(t, w.devKey, forksPath,
		`{"name": "s1", "purpose": "p", "session_id": "`+sid+`", "sandbox": `+member+`}`))
	wantMembers(t, body, map[string]any{"sandbox": map[string]any{"id": "sbx_1", "state": "provisioning"}})
	wantMembers(t, ws, map[string]any{"status": "ready"})
	calls := s.called()
	if len(calls) != 1 {
		t.Fatalf("sandbox component called %d times, want once", len(calls))
	}
	c := calls[0]
	switch {
	case c.req.Caller.User == nil || c.req.Caller.User.ID != w.dev:
		t.Errorf("caller = %+v, want dev", c.req.Caller)
	case c.req.Fork.Slug != "demo--s1" || string(c.req.Sandbox) != member:
		t.Errorf("fork %s with sandbox %s, want demo--s1 with %s", c.req.Fork.Slug, c.req.Sandbox, member)
	case c.req.SessionID == nil || *c.req.SessionID != sid || c.req.Purpose == nil || *c.req.Purpose != "p":
		t.Errorf("defaults: session_id %v, purpose %v, want %s and p", c.req.SessionID, c.req.Purpose, sid)
	case !c.visible || !c.repo:
		t.Errorf("in the transaction: fork record visible %v, repository exists %v, want both", c.visible, c.repo)
	}
}

func TestCreate_SandboxRefusalCreatesNoFork(t *testing.T) {
	t.Parallel()
	s := &sandboxer{}
	w := newWorld(t, collaborate(nil, s))
	for _, c := range []struct {
		name, typ string
		status    int
		extras    map[string]any
	}{
		{"credential-missing", "credential_missing", http.StatusUnprocessableEntity, nil},
		{"capacity", "sandbox_capacity", http.StatusTooManyRequests, map[string]any{"limit": "sandboxes_per_user", "maximum": 2.0, "current": 2.0}},
		{"scope", "insufficient_scope", http.StatusForbidden, nil},
		{"invalid", "invalid_request", http.StatusBadRequest, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := server.NewError(c.typ, c.status, "refused by the sandbox component")
			for k, v := range c.extras {
				err = err.With(k, v)
			}
			s.set(func(s *sandboxer) { s.err = err })
			before := w.footprint(t)
			r := w.post(t, w.devKey, forksPath, map[string]any{"name": c.name,
				"token": map[string]any{"permissions": []string{"git:read"}}, "sandbox": map[string]any{"profile": "default"}})
			wantError(t, r, c.status, c.typ)
			e, _ := r.object(t, c.status)["error"].(map[string]any)
			wantMembers(t, e, c.extras)
			w.unchanged(t, before)
			created(t, w.post(t, w.devKey, forksPath, map[string]any{"name": c.name}))
		})
	}
}

func TestCreate_SandboxWithoutComponent(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	before := w.footprint(t)
	wantError(t, w.post(t, w.devKey, forksPath, map[string]any{"sandbox": map[string]any{"profile": "default"}}),
		http.StatusNotFound, "sandboxes_disabled")
	w.unchanged(t, before)
}
