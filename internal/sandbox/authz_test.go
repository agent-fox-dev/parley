package sandbox_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

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
