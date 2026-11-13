package identity_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

func TestScopes_RegisterExactlyTheThirtyTwoNamesInTheirOrder(t *testing.T) {
	t.Parallel()
	want := []string{
		"users:read", "users:write", "orgs:read", "keys:read", "keys:manage", "tokens:read", "tokens:write", "tokens:manage",
		"workspaces:read", "workspaces:write", "git:read", "git:write", "changes:read", "changes:write", "changes:approve",
		"checks:write", "patches:read", "patches:write", "rebuilds:read", "rebuilds:write", "secrets:read", "secrets:write",
		"vars:read", "vars:write", "forge:read", "forge:write", "sandboxes:read", "sandboxes:write", "audit:read",
		"audit:write", "sessions:read", "sessions:write",
	}
	got := identity.RegisteredScopes()
	if len(got) != 32 || !slices.Equal(got, want) {
		t.Fatalf("RegisteredScopes() = %v, want the 32 scopes %v", got, want)
	}
	got[0] = "changed"
	if identity.RegisteredScopes()[0] != "users:read" {
		t.Errorf("changing the returned list changed the registry")
	}
	for _, s := range want {
		if !identity.IsRegisteredScope(s) {
			t.Errorf("IsRegisteredScope(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "git", "git:", ":read", "git:admin", "GIT:READ", "checks:read", "orgs:write", "keys:write",
		"tokens:approve", " git:read"} {
		if identity.IsRegisteredScope(s) {
			t.Errorf("IsRegisteredScope(%q) = true, want false", s)
		}
	}
}

func TestSatisfies_FollowsTheImplicationRules(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		held   []string
		needed string
		want   bool
	}{
		{[]string{"git:read"}, "git:read", true},
		{[]string{"git:write"}, "git:read", true},
		{[]string{"git:read"}, "git:write", false},
		{[]string{"users:write"}, "users:read", true},
		{[]string{"keys:manage"}, "keys:read", true},
		{[]string{"keys:read"}, "keys:manage", false},
		{[]string{"tokens:manage"}, "tokens:write", true},
		{[]string{"tokens:manage"}, "tokens:read", true},
		{[]string{"tokens:write"}, "tokens:read", true},
		{[]string{"tokens:write"}, "tokens:manage", false},
		{[]string{"tokens:read"}, "tokens:write", false},
		{[]string{"changes:write"}, "changes:approve", false},
		{[]string{"changes:approve"}, "changes:read", false},
		{[]string{"changes:approve"}, "changes:write", false},
		{[]string{"changes:approve"}, "changes:approve", true},
		{[]string{"checks:write"}, "checks:write", true},
		{[]string{"changes:write"}, "checks:write", false},
		{[]string{"git:write"}, "changes:read", false},
		{[]string{}, "git:read", false},
	} {
		if got := identity.Satisfies(c.held, c.needed); got != c.want {
			t.Errorf("Satisfies(%v, %q) = %v, want %v", c.held, c.needed, got, c.want)
		}
	}
}

// TestScopeCheck_HoldsOnlyAPATToTheDeclaredScopeBeforeAnyLaterCheck runs the
// check in the server's pipeline, with a stub authenticator reporting the
// caller and, after the scope position, an access check that answers 404
// for the id "missing", as a lookup of a resource that does not exist would.
// A caller that is the zero Principal is reported without one, as an
// authenticator other than the identity service's would.
func TestScopeCheck_HoldsOnlyAPATToTheDeclaredScopeBeforeAnyLaterCheck(t *testing.T) {
	t.Parallel()
	pat := func(perms ...string) identity.Principal {
		return identity.Principal{Type: identity.TypePAT, CredentialID: "t1", User: &identity.User{ID: "u1", Role: identity.RoleUser}, Permissions: perms}
	}
	key := func(role string) identity.Principal {
		return identity.Principal{Type: identity.TypeAPIKey, CredentialID: "k1", User: &identity.User{ID: "u1", Role: role}}
	}
	type call struct {
		name   string
		caller identity.Principal
		scope  string // "" for a route that declares none
		id     string
	}
	passes := []call{
		{"a PAT holding a scope that implies the needed one", pat("git:write"), "git:read", "present"},
		{"a PAT on a route that declares no scope", pat("git:read"), "", "present"},
	}
	for _, caller := range []struct {
		name string
		p    identity.Principal
	}{
		{"the admin token", identity.Principal{Type: identity.TypeAdmin, CredentialID: "admin"}},
		{"an API key of a user", key(identity.RoleUser)},
		{"an API key of an admin", key(identity.RoleAdmin)},
	} {
		for _, scope := range []string{"git:write", "changes:approve", "checks:write", "tokens:manage"} {
			passes = append(passes, call{caller.name + " on " + scope, caller.p, scope, "present"})
		}
	}
	refusals := []call{
		{"a PAT without the scope, the resource present", pat("git:read"), "git:write", "present"},
		{"a PAT without the scope, the resource missing", pat("git:read"), "git:write", "missing"},
	}
	failures := []call{
		{"a caller without a principal on a route that declares a scope", identity.Principal{}, "git:read", "present"},
	}
	run := func(t *testing.T, c call) (r response, handled, laterChecks int64) {
		var handlerCalls, checkCalls atomic.Int64
		srv := server.New(server.Options{Logger: slog.New(slog.DiscardHandler), RateLimitDisabled: true})
		srv.SetAuthenticator(func(*http.Request) (server.Caller, error) {
			p := c.caller
			if p.Type == "" {
				return server.Caller{CredentialID: "c1"}, nil
			}
			return server.Caller{CredentialID: p.CredentialID, Admin: p.Type == identity.TypeAdmin, Detail: &p}, nil
		})
		srv.SetScopeCheck(identity.ScopeCheck)
		srv.AddAccessCheck(func(r *http.Request) error {
			checkCalls.Add(1)
			if server.Param(r, "id") == "missing" {
				return server.NotFound("thing not found")
			}
			return nil
		})
		srv.Register(server.Route{Method: http.MethodGet, Pattern: "/things/:id", Scope: c.scope,
			Handler: func(w http.ResponseWriter, _ *http.Request) error {
				handlerCalls.Add(1)
				server.WriteJSON(w, http.StatusOK, map[string]any{})
				return nil
			}})
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/things/"+c.id, nil))
		r = response{status: rec.Code, body: rec.Body.Bytes()}
		_ = json.Unmarshal(r.body, &r.json)
		return r, handlerCalls.Load(), checkCalls.Load()
	}
	for _, c := range passes {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if r, handled, _ := run(t, c); r.status != http.StatusOK || handled != 1 {
				t.Errorf("answer = %d %s with %d handler calls, want 200 from the handler", r.status, r.body, handled)
			}
		})
	}
	for _, c := range refusals {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r, handled, later := run(t, c)
			msg, _ := r.errorField("message").(string)
			if r.status != http.StatusForbidden || r.errorField("error_type") != "insufficient_scope" ||
				r.errorField("code") != float64(http.StatusForbidden) || !strings.Contains(msg, c.scope) {
				t.Errorf("answer = %d %s, want 403 insufficient_scope with code 403 naming %s", r.status, r.body, c.scope)
			}
			if handled != 0 || later != 0 {
				t.Errorf("the handler ran %d times and the later check %d times, want 0 and 0", handled, later)
			}
		})
	}
	for _, c := range failures {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r, handled, later := run(t, c)
			if r.status != http.StatusInternalServerError || r.errorField("error_type") != "internal" {
				t.Errorf("answer = %d %s, want 500 internal", r.status, r.body)
			}
			if handled != 0 || later != 0 {
				t.Errorf("the handler ran %d times and the later check %d times, want 0 and 0", handled, later)
			}
		})
	}
}
