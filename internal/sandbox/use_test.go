package sandbox_test

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// unknownID is a well-formed sandbox id no sandbox has.
const unknownID = "sbx_00000000000000000000000000000000"

func TestUseRoutes_AnswerDisabledWhileSandboxesAreOff(t *testing.T) {
	t.Parallel()
	h := startProbeHub(t, false)
	key := h.IssueKey(t, h.owner)
	for _, u := range useRoutes {
		for _, slug := range []string{"ws", "nope"} {
			t.Run(u.String()+" on "+slug, func(t *testing.T) {
				a := h.call(t, u.method, u.path(slug, unknownID), key, u.body())
				if a.status != http.StatusNotFound || a.err.ErrorType != "sandboxes_disabled" {
					t.Errorf("%s = %d %s, want 404 sandboxes_disabled", u, a.status, a.body)
				}
			})
		}
		t.Run(u.String()+" without a credential", func(t *testing.T) {
			a := h.call(t, u.method, u.path("ws", unknownID), "", u.body())
			if a.status != http.StatusUnauthorized || a.err.ErrorType != "unauthenticated" {
				t.Errorf("%s = %d %s, want 401 unauthenticated", u, a.status, a.body)
			}
		})
	}
}

// gateRow is a caller of a route that uses a sandbox, on a workspace, and
// the answer it gets.
type gateRow struct {
	name, cred, slug string
	status           int
	errorType        string
}

func TestUseRoutes_RefuseACallerWithoutTheScopeOrTheAccessBeforeTheRuntime(t *testing.T) {
	t.Parallel()
	h := startUseHub(t, nil)
	id := h.seed(t, "ws", sandbox.StateReady, nil)
	writeOnly, readOnly, noScope := h.pat(t, h.owner, "sandboxes:write"), h.pat(t, h.owner, "sandboxes:read"), h.pat(t, h.owner, "git:read")
	memberKey, outsiderKey := h.IssueKey(t, h.member), h.IssueKey(t, h.outsider)
	// Serial subtests: each counts the operation's calls around its request.
	for _, u := range useRoutes {
		rows := []gateRow{
			{"a PAT without the scope on an unknown workspace", noScope, "nope", 403, "insufficient_scope"},
			{"the API key of a user who is no member", outsiderKey, "ws", 404, "not_found"},
		}
		if u.write {
			rows = append(rows,
				gateRow{"a PAT holding only sandboxes:read", readOnly, "ws", 403, "insufficient_scope"},
				gateRow{"the API key of a read-only member", memberKey, "ws", 403, "forbidden"},
			)
		} else {
			rows = append(rows,
				gateRow{"a PAT holding no sandbox scope", noScope, "ws", 403, "insufficient_scope"},
				// The scope check lets R:write satisfy R:read on every route.
				gateRow{"a PAT holding sandboxes:write and not sandboxes:read", writeOnly, "ws", 200, ""},
				gateRow{"the API key of a read-only member", memberKey, "ws", 200, ""},
			)
		}
		for _, row := range rows {
			t.Run(u.String()+" with "+row.name, func(t *testing.T) {
				h.useCall(t, u, row.slug, id, row.cred, u.body(), row.status, row.errorType)
			})
		}
	}
}

func TestUseRoutes_AnswerASandboxTheWorkspaceDoesNotHaveNotFound(t *testing.T) {
	t.Parallel()
	h := startUseHub(t, nil)
	h.seed(t, "ws", sandbox.StateReady, nil)
	other := h.seed(t, "ws2", sandbox.StateReady, nil)
	// Serial subtests: each counts the operation's calls around its request.
	for _, u := range useRoutes {
		for _, id := range []struct{ name, id string }{
			{"an unknown id", unknownID},
			{"a malformed id", "nope"},
			{"the id of another workspace's sandbox", other},
		} {
			t.Run(u.String()+" with "+id.name, func(t *testing.T) {
				h.useCall(t, u, "ws", id.id, h.key, u.body(), 404, "sandbox_not_found")
			})
		}
	}
}

func TestUseRoutes_RefuseACredentialThatDoesNotCoverTheSandboxesHubCredential(t *testing.T) {
	t.Parallel()
	h := startUseHub(t, nil)
	ha := &sandbox.HubAccess{Scopes: []string{"git:write"}}
	ready, stopped := h.seed(t, "ws", sandbox.StateReady, ha), h.seed(t, "ws", sandbox.StateStopped, ha)
	narrow := h.pat(t, h.owner, "sandboxes:write", "sandboxes:read")
	// Serial subtests: each counts the operation's calls around its request.
	for _, u := range useRoutes {
		if !u.write {
			t.Run(u.String()+" with the narrower PAT", func(t *testing.T) {
				h.useCall(t, u, "ws", ready, narrow, u.body(), 200, "")
			})
			continue
		}
		for _, sb := range []struct{ state, id string }{{"ready", ready}, {"stopped", stopped}} {
			t.Run(u.String()+" with the narrower PAT on a "+sb.state+" sandbox", func(t *testing.T) {
				h.useCall(t, u, "ws", sb.id, narrow, u.body(), 403, "insufficient_scope")
			})
		}
		t.Run(u.String()+" with the narrower PAT and an empty body", func(t *testing.T) {
			h.useCall(t, u, "ws", ready, narrow, nil, 403, "insufficient_scope")
		})
		t.Run(u.String()+" with the creator's API key", func(t *testing.T) {
			h.useCall(t, u, "ws", ready, h.key, u.body(), 200, "")
		})
	}
}

func TestUseRoutes_RefuseASandboxInAStateTheRouteDoesNotAllow(t *testing.T) {
	t.Parallel()
	h := startUseHub(t, nil)
	allowed := map[string][]string{
		"exec":     {sandbox.StateReady},
		"upload":   {sandbox.StateReady},
		"attach":   {sandbox.StateReady},
		"download": {sandbox.StateReady, sandbox.StateCompleted, sandbox.StateFailed},
		"logs":     {sandbox.StateProvisioning, sandbox.StateReady, sandbox.StateStopped, sandbox.StateCompleted, sandbox.StateFailed, sandbox.StateDeleted},
	}
	for _, u := range toolRoutes() {
		allowed[u.name] = []string{sandbox.StateReady}
	}
	// Serial subtests: each counts the operation's calls around its request.
	for _, state := range []string{sandbox.StateProvisioning, sandbox.StateReady, sandbox.StateStopped, sandbox.StateCompleted, sandbox.StateFailed, sandbox.StateDeleted} {
		id := h.seed(t, "ws", state, nil)
		for _, u := range useRoutes {
			t.Run(u.String()+" in "+state, func(t *testing.T) {
				status, errorType := 409, "sandbox_not_ready"
				for _, s := range allowed[u.name] {
					if s == state {
						status, errorType = 200, ""
					}
				}
				h.useCall(t, u, "ws", id, h.key, u.body(), status, errorType)
			})
		}
	}
}

func TestUseRoutes_AnswerARuntimeFailureScrubbedOfTheSandboxesSecrets(t *testing.T) {
	t.Parallel()
	const secretValue = "tok_SECRETVALUE"
	var failure atomic.Pointer[error]
	h := startUseHub(t, func() error { return *failure.Load() })
	id := h.seed(t, "ws", sandbox.StateReady, nil)
	h.svc.SetValues(id, sandbox.Values{Exposed: []string{secretValue}})
	// Serial subtests: each sets the failure the operations return.
	for _, kind := range []struct {
		name string
		err  error
	}{{"unavailable", runtime.ErrUnavailable}, {"unsupported", runtime.ErrUnsupported}} {
		for _, u := range useRoutes {
			if u.name == "logs" {
				continue
			}
			t.Run(u.String()+" failing "+kind.name, func(t *testing.T) {
				err := error(runtime.NewError(kind.err, "Probe", "boom "+secretValue+" "+strings.Repeat("x", 2000)))
				failure.Store(&err)
				a := h.call(t, u.method, u.path("ws", id), h.key, u.body())
				if a.status != http.StatusBadGateway || a.err.ErrorType != "sandbox_runtime_error" {
					t.Fatalf("%s = %d %.300s, want 502 sandbox_runtime_error", u, a.status, a.body)
				}
				msg := a.err.Message
				if !strings.Contains(msg, "boom") || !strings.Contains(msg, "[redacted]") || strings.Contains(msg, secretValue) {
					t.Errorf("message = %.200q, want boom and [redacted] and not the secret", msg)
				}
				if len(msg) > 1024 {
					t.Errorf("message is %d bytes, want at most 1024", len(msg))
				}
			})
		}
	}
	t.Run("a failure that is not the runtime's", func(t *testing.T) {
		err := errors.New("broken " + secretValue)
		failure.Store(&err)
		a := h.call(t, http.MethodPost, sandboxPath("ws", id)+"/exec", h.key, useRoutes[0].body())
		if a.status != http.StatusInternalServerError || strings.Contains(string(a.body), secretValue) {
			t.Errorf("exec = %d %s, want 500 without the secret", a.status, a.body)
		}
	})
	if log := h.Log.String(); strings.Contains(log, secretValue) {
		t.Errorf("the hub's log holds the secret value:\n%s", log)
	}
}

func TestUseRoutes_AnswerTheFirstCheckARequestFails(t *testing.T) {
	t.Parallel()
	h := startUseHub(t, nil)
	ha := &sandbox.HubAccess{Scopes: []string{"git:write"}}
	ready, stopped := h.seed(t, "ws", sandbox.StateReady, ha), h.seed(t, "ws", sandbox.StateStopped, nil)
	narrow := h.pat(t, h.owner, "sandboxes:write", "sandboxes:read")
	outsiderPAT, outsiderKey := h.pat(t, h.outsider, "git:read"), h.IssueKey(t, h.outsider)
	bound := h.IssuePAT(t, identity.PATSpec{UserID: h.owner, Permissions: []string{"git:read"}, Workspace: "ws2"})
	exec := useRoutes[0]
	// Serial subtests: each counts the operation's calls around its request.
	for _, c := range []struct {
		name, cred, id string
		body           any
		status         int
		errorType      string
	}{
		{"no credential and an empty body", "", ready, nil, 401, "unauthenticated"},
		{"a token bound to another workspace without sandboxes:write", bound, ready, exec.body(), 404, "not_found"},
		{"a PAT without sandboxes:write on a workspace it cannot see", outsiderPAT, ready, exec.body(), 403, "insufficient_scope"},
		{"a user who is no member and an unknown sandbox", outsiderKey, unknownID, exec.body(), 404, "not_found"},
		{"an unknown sandbox and an empty body", h.key, unknownID, nil, 404, "sandbox_not_found"},
		{"a PAT that does not cover the sandbox and an empty body", narrow, ready, nil, 403, "insufficient_scope"},
		{"an empty body on a stopped sandbox", h.key, stopped, nil, 400, "invalid_request"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h.useCall(t, exec, "ws", c.id, c.cred, c.body, c.status, c.errorType)
		})
	}

	noTools := func(rec *sandbox.Record) { rec.Snapshot.ToolServer = "" }
	plain, plainStopped := h.seedWith(t, "ws", sandbox.StateReady, nil, noTools), h.seedWith(t, "ws", sandbox.StateStopped, nil, noTools)
	noScope := h.pat(t, h.owner, "git:read")
	for _, u := range toolRoutes() {
		for _, c := range []struct {
			name, cred, id string
			status         int
			errorType      string
		}{
			{"no credential on a sandbox without tools", "", plain, 401, "unauthenticated"},
			{"a PAT without a sandboxes scope and an unknown sandbox", noScope, unknownID, 403, "insufficient_scope"},
			{"a PAT without a sandboxes scope on a sandbox without tools", noScope, plain, 403, "insufficient_scope"},
			{"a user who is no member on a sandbox without tools", outsiderKey, plain, 404, "not_found"},
			{"a stopped sandbox without tools", h.key, plainStopped, 404, "tools_unavailable"},
			{"a stopped sandbox with tools", h.key, stopped, 409, "sandbox_not_ready"},
		} {
			t.Run(u.String()+" with "+c.name, func(t *testing.T) {
				h.useCall(t, u, "ws", c.id, c.cred, u.body(), c.status, c.errorType)
			})
		}
	}
}
