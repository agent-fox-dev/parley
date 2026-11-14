package sandbox_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

func TestSandboxRoutes_AnswerEveryCallerDisabledWhileSandboxesAreOff(t *testing.T) {
	t.Parallel()
	h := startProbeHub(t, false)
	creds := []struct{ name, cred string }{
		{"the admin token", h.Token},
		{"an API key", h.IssueKey(t, h.owner)},
		{"a PAT with sandboxes:read", h.pat(t, h.owner, "sandboxes:read")},
		{"a PAT without a sandbox scope", h.pat(t, h.owner, "git:read")},
	}
	// Serial subtests: each counts the handler's calls around its request.
	for _, path := range []string{"/sandbox-profiles", "/sandboxes", "/workspaces/ws/sandboxes/probe-read", "/workspaces/nope/sandboxes/probe-read"} {
		for _, c := range creds {
			t.Run(path+" with "+c.name, func(t *testing.T) {
				before := h.calls.Load()
				a := h.call(t, http.MethodGet, path, c.cred, nil)
				if a.status != http.StatusNotFound || a.err.ErrorType != "sandboxes_disabled" {
					t.Errorf("GET %s = %d %s, want 404 sandboxes_disabled", path, a.status, a.body)
				}
				if n := h.calls.Load() - before; n != 0 {
					t.Errorf("the handler ran %d times, want never", n)
				}
			})
		}
	}
}

func TestSandboxRoutes_LeaveTheChecksBeforeTheFeatureCheckToDecideWhileSandboxesAreOff(t *testing.T) {
	t.Parallel()
	h := startProbeHub(t, false)
	overLimit := []byte(`{"a":"` + strings.Repeat("x", probeBodyLimit-7) + `"}`)
	if len(overLimit) != probeBodyLimit+1 {
		t.Fatalf("the body is %d bytes, want %d", len(overLimit), probeBodyLimit+1)
	}
	// Serial subtests: the handler's calls are counted once they have all run.
	for _, c := range []struct {
		name, method, path, cred string
		body                     any
		status                   int
		errorType                string
	}{
		{"without a credential", http.MethodGet, "/sandbox-profiles", "", nil, 401, "unauthenticated"},
		{"on a path no route has", http.MethodGet, "/sandbox-profilez", h.Token, nil, 404, "not_found"},
		{"with a method the route does not have", http.MethodDelete, "/sandbox-profiles", h.Token, nil, 405, "method_not_allowed"},
		{"with a body over the limit", http.MethodPost, "/workspaces/ws/sandboxes/probe-write", h.Token, overLimit, 413, "payload_too_large"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := h.call(t, c.method, c.path, c.cred, c.body)
			if a.status != c.status || a.err.ErrorType != c.errorType {
				t.Errorf("%s %s = %d %s, want %d %s", c.method, c.path, a.status, a.body, c.status, c.errorType)
			}
		})
	}
	if n := h.calls.Load(); n != 0 {
		t.Errorf("the handler ran %d times, want never", n)
	}
}

// accessRow is a caller of a workspace sandbox route and the answer it
// gets: a status of 200 means the handler ran.
type accessRow struct {
	name, cred, slug string
	status           int
	errorType        string
}

// checkAccess sends method to the probe route of each row on its slug and
// checks the answer and that the handler ran only for the 200 rows.
func checkAccess(t *testing.T, h *probeHub, method, route string, rows []accessRow) {
	t.Helper()
	// Serial subtests: each counts the handler's calls around its request.
	for _, c := range rows {
		t.Run(c.name, func(t *testing.T) {
			var body any
			if method == http.MethodPost {
				body = map[string]any{}
			}
			path := "/workspaces/" + c.slug + "/sandboxes/" + route
			before := h.calls.Load()
			a := h.call(t, method, path, c.cred, body)
			if a.status != c.status || c.errorType != "" && a.err.ErrorType != c.errorType {
				t.Errorf("%s %s = %d %s, want %d %s", method, path, a.status, a.body, c.status, c.errorType)
			}
			want := int64(0)
			if c.status == http.StatusOK {
				want = 1
			}
			if n := h.calls.Load() - before; n != want {
				t.Errorf("the handler ran %d times, want %d", n, want)
			}
		})
	}
}

func TestReadRoute_NeedsTheReadScopeOfAPATAndReadAccessToTheWorkspace(t *testing.T) {
	t.Parallel()
	h := startProbeHub(t, true)
	ownerKey, gitPAT := h.IssueKey(t, h.owner), h.pat(t, h.owner, "git:read")
	checkAccess(t, h, http.MethodGet, "probe-read", []accessRow{
		{"the owner's API key", ownerKey, "ws", 200, ""},
		{"a member's API key", h.IssueKey(t, h.member), "ws", 200, ""},
		{"the admin token", h.Token, "ws", 200, ""},
		{"the owner's PAT with sandboxes:read", h.pat(t, h.owner, "sandboxes:read"), "ws", 200, ""},
		{"the owner's PAT without sandboxes:read", gitPAT, "ws", 403, "insufficient_scope"},
		{"a PAT without sandboxes:read on a slug that does not exist", gitPAT, "nope", 403, "insufficient_scope"},
		{"an outsider's API key", h.IssueKey(t, h.outsider), "ws", 404, "not_found"},
		{"the owner's API key on a slug that does not exist", ownerKey, "nope", 404, "not_found"},
		{"no credential", "", "ws", 401, "unauthenticated"},
	})
}

func TestWriteRoute_NeedsTheWriteScopeOfAPATAndWriteAccessToTheWorkspace(t *testing.T) {
	t.Parallel()
	h := startProbeHub(t, true)
	checkAccess(t, h, http.MethodPost, "probe-write", []accessRow{
		{"the owner's API key", h.IssueKey(t, h.owner), "ws", 200, ""},
		{"the admin token", h.Token, "ws", 200, ""},
		{"the owner's PAT with sandboxes:write", h.pat(t, h.owner, "sandboxes:write"), "ws", 200, ""},
		{"the owner's PAT with only sandboxes:read", h.pat(t, h.owner, "sandboxes:read"), "ws", 403, "insufficient_scope"},
		{"a member's API key", h.IssueKey(t, h.member), "ws", 403, "forbidden"},
		{"a member's PAT with sandboxes:write", h.pat(t, h.member, "sandboxes:write"), "ws", 403, "forbidden"},
		{"a member's PAT with only sandboxes:read", h.pat(t, h.member, "sandboxes:read"), "ws", 403, "insufficient_scope"},
		{"an outsider's API key", h.IssueKey(t, h.outsider), "ws", 404, "not_found"},
	})
}

func TestRouteNeedingAUser_RefusesTheAdminTokenAfterWorkspaceAccessAndBeforeTheHandler(t *testing.T) {
	t.Parallel()
	h := startProbeHub(t, true)
	const path = "/workspaces/ws/sandboxes/probe-create"

	a := h.call(t, http.MethodPost, path, h.Token, map[string]any{})
	if a.status != http.StatusForbidden || a.err.ErrorType != "forbidden" || a.err.Message != "the admin token cannot create a sandbox" {
		t.Errorf("the admin token's POST = %d %s, want 403 forbidden 'the admin token cannot create a sandbox'", a.status, a.body)
	}
	if n := h.calls.Load(); n != 0 {
		t.Errorf("the handler ran %d times for the admin token, want never", n)
	}
	if a := h.call(t, http.MethodPost, "/workspaces/nope/sandboxes/probe-create", h.Token, map[string]any{}); a.status != http.StatusNotFound || a.err.ErrorType != "not_found" {
		t.Errorf("the admin token's POST on a slug that does not exist = %d %s, want 404 not_found", a.status, a.body)
	}
	if a := h.call(t, http.MethodPost, path, h.IssueKey(t, h.owner), map[string]any{}); a.status != http.StatusBadRequest {
		t.Errorf("the owner's POST = %d %s, want the handler's 400", a.status, a.body)
	}
	if n := h.calls.Load(); n != 1 {
		t.Errorf("the handler ran %d times, want once, for the owner", n)
	}
}

func TestSandboxRoute_AnswersARuntimeErrorTheHandlerReturnsWithTheRuntimeErrorAnswer(t *testing.T) {
	t.Parallel()
	h := startProbeHub(t, true)
	const userinfoPass = "pw-SECRET"
	// Serial subtests: each sets the failure of the runtime they share.
	for _, c := range []struct {
		name string
		fail func()
	}{
		{"an unreachable runtime", func() { h.rt.SetUnreachable(true) }},
		{"an operation the runtime does not have", func() { h.rt.FailOperation(runtime.OpGetSandbox, runtime.ErrUnsupported, "") }},
		{"a failure whose text holds the gateway token and a URL with user information", func() {
			h.rt.FailOperation(runtime.OpGetSandbox, runtime.ErrUnavailable,
				"token "+gatewayToken+" refused by https://parley:"+userinfoPass+"@gw.test:8443/")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h.rt.SetUnreachable(false)
			h.rt.FailOperation(runtime.OpGetSandbox, nil, "")
			c.fail()
			a := h.call(t, http.MethodGet, "/workspaces/ws/sandboxes/probe-runtime", h.Token, nil)
			if a.status != http.StatusBadGateway || a.err.ErrorType != "sandbox_runtime_error" || a.err.Code != http.StatusBadGateway {
				t.Errorf("GET = %d %s, want 502 sandbox_runtime_error with code 502", a.status, a.body)
			}
			for _, secret := range []string{gatewayToken, userinfoPass} {
				if bytes.Contains(a.body, []byte(secret)) {
					t.Errorf("the answer %s holds %q", a.body, secret)
				}
			}
			res, err := http.Get(h.URL("/healthz"))
			if err != nil {
				t.Fatalf("GET /healthz after the runtime error: %v", err)
			}
			_ = res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Errorf("GET /healthz after the runtime error = %d, want 200", res.StatusCode)
			}
		})
	}
}

// patWith is a PAT caller holding permissions, limited to paths (nil for no
// limit).
func patWith(paths []string, permissions ...string) identity.Principal {
	return identity.Principal{Type: identity.TypePAT, Permissions: permissions, Paths: paths}
}

func TestCovers_HoldsAPATToEveryHubScopeAndToPathsNoWiderThanItsOwn(t *testing.T) {
	t.Parallel()
	hubScopes := []string{"git:read", "git:write", "changes:write"}
	// hub is a hub credential of hubScopes limited to paths (none for no
	// limit).
	hub := func(paths ...string) *sandbox.HubAccess { return &sandbox.HubAccess{Scopes: hubScopes, Paths: paths} }
	for _, c := range []struct {
		name string
		pat  identity.Principal
		ha   *sandbox.HubAccess
		want bool
	}{
		{"every hub scope, neither limited to paths", patWith(nil, hubScopes...), hub(), true},
		{"git:write missing", patWith(nil, "git:read", "changes:write"), hub(), false},
		{"every hub scope and others", patWith(nil, "git:read", "git:write", "changes:write", "sandboxes:write", "secrets:read"), hub(), true},
		{"git:read implied by git:write", patWith(nil, "git:write", "changes:write"), hub(), true},
		{"a PAT glob covering the hub glob", patWith([]string{"src/**"}, hubScopes...), hub("src/api/*.go"), true},
		{"a PAT glob equal to the hub glob", patWith([]string{"src/**"}, hubScopes...), hub("src/**"), true},
		{"a PAT glob narrower than the hub glob", patWith([]string{"src/*"}, hubScopes...), hub("src/**"), false},
		{"one hub glob no PAT glob covers", patWith([]string{"src/**"}, hubScopes...), hub("src/a", "docs/b"), false},
		{"a PAT limited to paths and a hub credential without a limit", patWith([]string{"src/**"}, hubScopes...), hub(), false},
		{"a PAT without a limit and a hub credential limited to paths", patWith(nil, hubScopes...), hub("docs/**"), true},
		{"no hub scopes and no paths, a PAT without scopes", patWith(nil), &sandbox.HubAccess{Scopes: []string{}}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := sandbox.Covers(c.pat, c.ha); got != c.want {
				t.Errorf("Covers(%+v, %+v) = %v, want %v", c.pat, c.ha, got, c.want)
			}
		})
	}
}

func TestCovers_LetsTheAdminTokenAndAPIKeysAndEveryCallerOfASandboxWithoutAHubCredentialThrough(t *testing.T) {
	t.Parallel()
	admin := identity.Principal{Type: identity.TypeAdmin}
	key := identity.Principal{Type: identity.TypeAPIKey, User: &identity.User{}}
	every := &sandbox.HubAccess{
		Scopes: []string{"git:read", "git:write", "changes:read", "changes:write", "sessions:write", "audit:write"},
		Paths:  []string{"src/**"},
	}
	none := &sandbox.HubAccess{Scopes: []string{}}
	for _, c := range []struct {
		name   string
		caller identity.Principal
		ha     *sandbox.HubAccess
	}{
		{"the admin token, every hub scope limited to paths", admin, every},
		{"the admin token, no hub scopes or paths", admin, none},
		{"an API key, every hub scope limited to paths", key, every},
		{"an API key, no hub scopes or paths", key, none},
		{"the admin token, no hub credential", admin, nil},
		{"an API key, no hub credential", key, nil},
		{"a PAT without scopes limited to paths, no hub credential", patWith([]string{"a/*"}), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if !sandbox.Covers(c.caller, c.ha) {
				t.Errorf("Covers(%+v, %+v) = false, want true", c.caller, c.ha)
			}
		})
	}
}

func TestRouteActingThroughASandbox_RefusesACallerThatDoesNotCoverItsHubCredentialBeforeTheBody(t *testing.T) {
	t.Parallel()
	h := startProbeHub(t, true)
	const path = "/workspaces/ws/sandboxes/probe-exec"
	before := len(h.rt.Calls())

	// The body is one the handler would refuse, so a 403 shows the decision
	// came before it.
	a := h.call(t, http.MethodPost, path, h.pat(t, h.owner, "sandboxes:write"), map[string]any{})
	const msg = `this token does not hold "git:write", which the sandbox's hub credential holds`
	if a.status != http.StatusForbidden || a.err.ErrorType != "insufficient_scope" || a.err.Message != msg {
		t.Errorf("POST with a PAT without git:write = %d %s, want 403 insufficient_scope %q", a.status, a.body, msg)
	}
	if n := h.calls.Load(); n != 0 {
		t.Errorf("the rest of the handler ran %d times, want never", n)
	}
	if calls := h.rt.Calls()[before:]; len(calls) != 0 {
		t.Errorf("the runtime received %v, want no call", ops(calls))
	}

	a = h.call(t, http.MethodPost, path, h.pat(t, h.owner, "sandboxes:write", "git:write"), map[string]any{"command": []string{"true"}})
	if a.status != http.StatusOK {
		t.Errorf("POST with a PAT holding git:write = %d %s, want 200", a.status, a.body)
	}
	if n := h.calls.Load(); n != 1 {
		t.Errorf("the rest of the handler ran %d times, want once", n)
	}
	if calls := h.rt.Calls()[before:]; len(calls) != 1 {
		t.Errorf("the runtime received %v, want the handler's one call", ops(calls))
	}
}
