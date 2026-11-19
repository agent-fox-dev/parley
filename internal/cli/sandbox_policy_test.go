package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// proposalAnswer is a proposal as the hub answers an approve or a reject.
const proposalAnswer = `{"id": "sbp_1", "sandbox_id": "sbx_1", "state": "approved", "scope": "workspace", "workspace_rule_id": "sbr_1"}`

func TestSandboxPolicy_SendsTheRequestAndPrintsTheAnswer(t *testing.T) {
	t.Parallel()
	wantWrites(t, []sentWrite{
		{args: []string{"sandbox", "proposals", "demo", "sbx_1"}, method: http.MethodGet, path: "demo/sandboxes/sbx_1/proposals"},
		{
			args:   []string{"sandbox", "proposals", "demo", "sbx_1", "--state", "approved"},
			method: http.MethodGet, path: "demo/sandboxes/sbx_1/proposals", query: url.Values{"state": {"approved"}},
		},
		{args: []string{"sandbox", "proposals", "a/b", "sbx_1"}, method: http.MethodGet, path: "a%2Fb/sandboxes/sbx_1/proposals"},
		{args: []string{"sandbox", "rules", "demo"}, method: http.MethodGet, path: "demo/sandbox-rules"},
		{
			args:   []string{"sandbox", "rules", "demo", "--limit", "10", "--cursor", "abc"},
			method: http.MethodGet, path: "demo/sandbox-rules", query: url.Values{"limit": {"10"}, "cursor": {"abc"}},
		},
		{args: []string{"sandbox", "rules", "a/b"}, method: http.MethodGet, path: "a%2Fb/sandbox-rules"},
		{
			args:   []string{"sandbox", "approve", "demo", "sbx_1", "sbp_1"},
			method: http.MethodPost, path: "demo/sandboxes/sbx_1/proposals/sbp_1/approve", body: `{}`,
			status: http.StatusOK, answer: proposalAnswer,
		},
		{
			args:   []string{"sandbox", "approve", "demo", "sbx_1", "sbp_1", "--scope", "workspace", "--acknowledge-findings", "--comment", "npm registry"},
			method: http.MethodPost, path: "demo/sandboxes/sbx_1/proposals/sbp_1/approve",
			body:   `{"comment":"npm registry","acknowledge_findings":true,"scope":"workspace"}`,
			status: http.StatusOK, answer: proposalAnswer,
		},
		{
			args:   []string{"sandbox", "approve", "demo", "sbx_1", "sbp_1", "--scope", "sandbox"},
			method: http.MethodPost, path: "demo/sandboxes/sbx_1/proposals/sbp_1/approve", body: `{"scope":"sandbox"}`,
			status: http.StatusOK, answer: proposalAnswer,
		},
		{
			args:   []string{"sandbox", "reject", "demo", "sbx_1", "sbp_1"},
			method: http.MethodPost, path: "demo/sandboxes/sbx_1/proposals/sbp_1/reject", body: `{}`,
			status: http.StatusOK, answer: proposalAnswer,
		},
		{
			args:   []string{"sandbox", "reject", "demo", "sbx_1", "sbp_1", "--comment", "no"},
			method: http.MethodPost, path: "demo/sandboxes/sbx_1/proposals/sbp_1/reject", body: `{"comment":"no"}`,
			status: http.StatusOK, answer: proposalAnswer,
		},
		{
			args:   []string{"sandbox", "rule-delete", "demo", "sbr_1"},
			method: http.MethodDelete, path: "demo/sandbox-rules/sbr_1",
			status: http.StatusNoContent, printed: `{"id":"sbr_1","status":"deleted"}`,
		},
	})
}

func TestSandboxPolicy_PrintsTheHubsRefusalOnStderr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args            []string
		method, rawPath string
		status          int
		errorType       string
	}{
		{
			[]string{"sandbox", "approve", "demo", "sbx_1", "sbp_1"}, http.MethodPost, "/api/v1/workspaces/demo/sandboxes/sbx_1/proposals/sbp_1/approve",
			http.StatusConflict, "findings_not_acknowledged",
		},
		{
			[]string{"sandbox", "approve", "demo", "sbx_1", "sbp_1", "--scope", "workspace"}, http.MethodPost, "/api/v1/workspaces/demo/sandboxes/sbx_1/proposals/sbp_1/approve",
			http.StatusTooManyRequests, "sandbox_rule_limit_reached",
		},
		{
			[]string{"sandbox", "reject", "demo", "sbx_1", "sbp_1"}, http.MethodPost, "/api/v1/workspaces/demo/sandboxes/sbx_1/proposals/sbp_1/reject",
			http.StatusConflict, "proposal_decided",
		},
		{
			[]string{"sandbox", "rule-delete", "demo", "sbr_1"}, http.MethodDelete, "/api/v1/workspaces/demo/sandbox-rules/sbr_1",
			http.StatusNotFound, "sandbox_rule_not_found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.errorType, func(t *testing.T) {
			t.Parallel()
			refusal := fmt.Sprintf(`{"error":{"code":%d,"message":"refused","error_type":%q}}`, tt.status, tt.errorType)
			hub := newStubHub(t).answer(tt.method, tt.rawPath, tt.status, refusal)

			code, stdout, stderr, sent := runOnHub(t, hub, tt.args...)

			if code != 1 || stdout != "" {
				t.Errorf("exit status %d, stdout %q; want 1 and nothing", code, stdout)
			}
			if stderr != indented(t, refusal) {
				t.Errorf("stderr = %q, want the hub's envelope unchanged", stderr)
			}
			if _, _, got := clientEnvelope(t, stderr); got != tt.errorType {
				t.Errorf("error_type = %q, want %q", got, tt.errorType)
			}
			wantOnlyRequest(t, sent, tt.method, tt.rawPath)
		})
	}
}

func TestSandboxPolicy_UsageErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args []string
		// says is what the message must name.
		says string
	}{
		{[]string{"sandbox", "proposals", "demo"}, "<id>"},
		{[]string{"sandbox", "proposals", "demo", "sbx_1", "extra"}, `"extra"`},
		{[]string{"sandbox", "approve", "demo", "sbx_1"}, "<proposal-id>"},
		{[]string{"sandbox", "approve", "demo", "sbx_1", "sbp_1", "--scope", "org"}, `"org"`},
		{[]string{"sandbox", "approve", "demo", "sbx_1", "sbp_1", "--scope", ""}, `""`},
		{[]string{"sandbox", "reject", "demo"}, "<id>"},
		{[]string{"sandbox", "reject", "demo", "sbx_1", "sbp_1", "extra"}, `"extra"`},
		{[]string{"sandbox", "rules"}, "<workspace-slug>"},
		{[]string{"sandbox", "rules", "demo", "extra"}, `"extra"`},
		{[]string{"sandbox", "rule-delete", "demo"}, "<rule-id>"},
		{[]string{"sandbox", "rule-delete", "demo", "sbr_1", "extra"}, `"extra"`},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t)

			code, stdout, stderr, sent := runOnHub(t, hub, tt.args...)

			if message := wantClientFailure(t, code, stdout, stderr, "usage_error"); !strings.Contains(message, tt.says) {
				t.Errorf("message = %q, want it to name %s", message, tt.says)
			}
			if len(sent) != 0 {
				t.Errorf("the hub got %d requests, want none", len(sent))
			}
		})
	}
}
