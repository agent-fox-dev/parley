package sandbox_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/secret"
)

// errRollback ends a test's transaction, which the store then rolls back.
var errRollback = errors.New("roll back")

// postFork sends POST /workspaces/demo/forks with cred and body.
func (h *createHub) postFork(t *testing.T, cred string, body map[string]any) answer {
	t.Helper()
	return h.call(t, http.MethodPost, "/workspaces/demo/forks", cred, body)
}

// timeMember parses the RFC 3339 time of the member name of o.
func timeMember(t *testing.T, o map[string]any, name string) time.Time {
	t.Helper()
	s, _ := o[name].(string)
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("%s %v: %v", name, o[name], err)
	}
	return at
}

func TestForkWithSandbox_CreatesBoth(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile)
	h.world(t)
	const session = "8d2f3c4b-1a2b-4c3d-9e8f-0a1b2c3d4e5f"
	for _, tc := range []struct {
		name    string
		body    map[string]any
		purpose string
		session any
		// atForkExpiry is set when the sandbox's ttl reaches past the
		// fork's expiry, to which it is cut.
		atForkExpiry bool
	}{
		{"f1", map[string]any{"purpose": "fix bug", "session_id": session, "sandbox": map[string]any{}}, "fix bug", session, false},
		{"f2", map[string]any{"purpose": strings.Repeat("x", 300), "sandbox": map[string]any{}}, strings.Repeat("x", 200), nil, false},
		{"f3", map[string]any{"purpose": "p", "sandbox": map[string]any{"purpose": "own", "command": []string{"make"}}}, "own", nil, false},
		{"f4", map[string]any{"ttl": "30m", "sandbox": map[string]any{}}, "", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := h.tokenIDs(t)
			tc.body["name"] = tc.name
			o := decoded(t, h.postFork(t, h.key, tc.body), http.StatusCreated)
			s, _ := o["sandbox"].(map[string]any)
			ws, _ := o["workspace"].(map[string]any)
			slug := "demo--" + tc.name
			if s["workspace"] != slug || s["state"] != sandbox.StateProvisioning || s["purpose"] != tc.purpose || s["session_id"] != tc.session {
				t.Errorf("sandbox = workspace %v, state %v, purpose %v, session %v; want %s, provisioning, %q, %v",
					s["workspace"], s["state"], s["purpose"], s["session_id"], slug, tc.purpose, tc.session)
			}
			expires, forkExpires := timeMember(t, s, "expires_at"), timeMember(t, ws, "expires_at")
			if expires.After(forkExpires) || tc.atForkExpiry && !expires.Equal(forkExpires) {
				t.Errorf("sandbox expires_at %v, fork's %v; want no later (equal: %v)", expires, forkExpires, tc.atForkExpiry)
			}
			id, _ := s["id"].(string)
			if got := decoded(t, h.call(t, http.MethodGet, "/workspaces/"+slug+"/sandboxes/"+id, h.key, nil), http.StatusOK); got["id"] != id {
				t.Errorf("get %s = %v; want that sandbox", id, got)
			}
			ha, _ := s["hub_access"].(map[string]any)
			if ha == nil || !reflect.DeepEqual(ha["scopes"], jsonList(allHubScopes)) {
				t.Errorf("hub_access = %v; want the six scopes", s["hub_access"])
			}
			tokens := h.newTokens(t, before)
			if len(tokens) != 1 || tokens[0].Workspace != slug || tokens[0].Name != "sandbox:"+id {
				t.Errorf("the create added tokens %+v; want the hub credential of %s bound to %s", tokens, id, slug)
			}
		})
	}
}

func TestForkWithSandbox_RefusalCreatesNeither(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile+needsProfile+credentialTables)
	h.world(t)
	narrow := h.pat(t, h.dana, "workspaces:write", "git:read")
	const refusedSession = "1c9d7e2a-3b4f-4a5d-8e6f-7a8b9c0d1e2f"
	unreadable := fmt.Errorf("the key is gone: %w", secret.ErrUnavailable)
	for _, tc := range []struct {
		name    string
		cred    string
		sandbox map[string]any
		setUp   func() (undo func())
		status  int
		typ     string
		extras  map[string]any
	}{
		{"a PAT without sandboxes:write", narrow, map[string]any{}, nil, http.StatusForbidden, "insufficient_scope", nil},
		{"a ttl under a minute", h.key, map[string]any{"ttl": "1s"}, nil, http.StatusBadRequest, "invalid_request", nil},
		{"an unknown profile", h.key, map[string]any{"profile": "nope"}, nil, http.StatusBadRequest, "invalid_request", nil},
		{"a refused session", h.key, map[string]any{"session_id": refusedSession}, func() func() {
			h.linker.setRefuse(true)
			return func() { h.linker.setRefuse(false) }
		}, http.StatusBadRequest, "unknown_session", nil},
		{"a missing required secret", h.key, map[string]any{"profile": "needs"}, nil, http.StatusUnprocessableEntity, "credential_missing",
			map[string]any{"credentials": nameList("model"), "secrets": nameList(), "scopes_searched": nameList("workspace", "parent", "user")}},
		{"an unreadable secret store", h.key, map[string]any{"profile": "needs"}, func() func() {
			h.resolver.setErr(unreadable)
			return func() { h.resolver.setErr(nil) }
		}, http.StatusServiceUnavailable, "secrets_unavailable", nil},
		{"max_per_user 0", h.key, map[string]any{}, func() func() {
			h.setLimits(only("max_per_user", 0))
			return func() { h.setLimits(limits{100, 100, 100, 100}) }
		}, http.StatusTooManyRequests, "sandbox_capacity", map[string]any{"limit": "max_per_user", "maximum": 0.0, "current": 0.0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setUp != nil {
				defer tc.setUp()()
			}
			before := h.footprint(t)
			a := h.postFork(t, tc.cred, map[string]any{"name": "g1", "sandbox": tc.sandbox})
			h.wantRefused(t, a, tc.status, tc.typ, before)
			e, _ := decoded(t, a, tc.status)["error"].(map[string]any)
			for k, v := range tc.extras {
				if !reflect.DeepEqual(e[k], v) {
					t.Errorf("error member %s = %#v; want %#v", k, e[k], v)
				}
			}
			if got := h.call(t, http.MethodGet, "/workspaces/demo--g1", h.key, nil); got.status != http.StatusNotFound {
				t.Errorf("get demo--g1 = %d %s; want 404", got.status, got.body)
			}
		})
	}
}

// createInFork creates in tx, through the fork create's sandbox component,
// a sandbox with an empty body on the fork f, which tx stores first, for
// dana and with no defaults, and returns its object.
func (h *createHub) createInFork(t *testing.T, ctx context.Context, tx *sql.Tx, f fork.Fork) map[string]any {
	t.Helper()
	if err := fork.Insert(ctx, tx, f); err != nil {
		t.Fatal(err)
	}
	raw, err := h.svc.CreateSandbox(ctx, tx, fork.SandboxRequest{Caller: h.principal(t), Fork: f, Sandbox: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("CreateSandbox = %v", err)
	}
	var o map[string]any
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatalf("the sandbox object %s: %v", raw, err)
	}
	return o
}

func TestComponent_RollbackLeavesNothing(t *testing.T) {
	t.Parallel()
	h := startProvisionHub(t, agentProfile)
	h.world(t)
	ctx := context.Background()
	f := h.forkRecord(t, "demo--r1", "demo", h.dana, recordStart.Add(7*24*time.Hour), nil)
	before := h.footprint(t)
	var id string
	err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
		id, _ = h.createInFork(t, ctx, tx, f)["id"].(string)
		return errRollback
	})
	if !errors.Is(err, errRollback) || id == "" {
		t.Fatalf("the transaction ended with %v, sandbox %q; want a sandbox, then the roll-back", err, id)
	}
	for range 3 {
		h.pass(t, time.Second)
	}
	if after := h.footprint(t); after != before {
		t.Errorf("after the roll-back and three passes the hub holds %+v; want %+v", after, before)
	}
	if _, kept := h.svc.HubCredential(id); !kept {
		t.Errorf("the hub credential of %s was dropped within 3 s; want it kept for 60 s", id)
	}
	h.pass(t, time.Minute)
	if _, kept := h.svc.HubCredential(id); kept {
		t.Errorf("the hub credential of %s is still kept 63 s after its sandbox was rolled back", id)
	}
	if got := h.rec.values(sandbox.SandboxesMetric)["state=provisioning"]; got != 0 {
		t.Errorf("parley_sandboxes{state=provisioning} = %v after the drop; want 0", got)
	}
}

func TestComponent_ProvisionsAfterCommit(t *testing.T) {
	t.Parallel()
	h := startProvisionHub(t, agentProfile)
	h.world(t)
	ctx := context.Background()
	f := h.forkRecord(t, "demo--c1", "demo", h.dana, recordStart.Add(7*24*time.Hour), nil)
	calls := len(h.rt.Calls())
	var id string
	err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
		id, _ = h.createInFork(t, ctx, tx, f)["id"].(string)
		h.pass(t, time.Second)
		h.pass(t, time.Second)
		if n := len(h.rt.Calls()); n != calls {
			t.Errorf("the runtime got %d calls before the commit; want none", n-calls)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	h.pass(t, time.Second)
	s := h.settledObject(t, "demo--c1", id)
	checkout, _ := s["checkout"].(map[string]any)
	if s["state"] != sandbox.StateReady || checkout["branch"] != "c1" {
		t.Errorf("sandbox = %v with checkout %v; want ready on the branch c1", s["state"], s["checkout"])
	}
	if n := len(h.sandboxEvents(t, sandbox.EventReady, id)); n != 1 {
		t.Errorf("%d events %s; want 1", n, sandbox.EventReady)
	}
	if n := len(h.hooks.runs(id)); n == 0 {
		t.Errorf("no command ran in sandbox %s; want the checkout", id)
	}
}
