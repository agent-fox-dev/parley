package e2e

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/secret"
)

// policyProfile is the profile agent of the network policy flows: it
// accepts proposals, as a profile does unless it says otherwise, and gives
// a sandbox a hub credential bound to its workspace and the credential
// model.
const policyProfile = "[[sandbox.profiles]]\nname = \"agent\"\ndefault = true\nttl = \"4h\"\nhub_access = \"fork\"\ncredentials = [\"model\"]\n"

// policyEvent is an event of the hub's log as the network policy flows read
// it.
type policyEvent struct {
	Type         string  `json:"type"`
	Workspace    *string `json:"workspace"`
	ResourceType string  `json:"resource_type"`
	ResourceID   string  `json:"resource_id"`
	SessionID    *string `json:"session_id"`
}

// policyEvents returns the events of the given types in the hub's log,
// oldest first, as the admin token reads them.
func policyEvents(t *testing.T, h *sandboxCreateHub, types ...string) []policyEvent {
	t.Helper()
	path := "/api/v1/events/log?" + url.Values{"after": {"0"}, "limit": {"200"}, "types": {strings.Join(types, ",")}}.Encode()
	status, body := h.Call(t, http.MethodGet, path, h.Token, nil)
	var page struct {
		Items []policyEvent `json:"items"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &page) != nil {
		t.Fatalf("GET %s = %d %s, want 200 with a page", path, status, body)
	}
	return page.Items
}

// policySandbox creates a sandbox on slug with parley sandbox create, waits
// until it is ready and returns it as the hub serves it.
func policySandbox(t *testing.T, h *sandboxCreateHub, slug string) map[string]any {
	t.Helper()
	created := h.parley(t, "sandbox", "create", slug)
	if created.code != 0 {
		t.Fatalf("parley sandbox create %s = %d %s %s; want 0", slug, created.code, created.stdout, created.stderr)
	}
	id, _ := created.json(t)["id"].(string)
	s := h.object(t, http.MethodGet, "/api/v1/workspaces/"+slug+"/sandboxes/"+id+"?wait_for=ready&wait=30", nil, http.StatusOK)
	if s["state"] != sandbox.StateReady {
		t.Fatalf("the sandbox %s on %s = %v; want it ready", id, slug, s)
	}
	return s
}

// createdPolicy is the policy the runtime's create call for the sandbox id
// carried.
func createdPolicy(t *testing.T, h *sandboxCreateHub, id string) runtime.Policy {
	t.Helper()
	spec, held := h.fake.Spec("parley", id)
	if !held {
		t.Fatalf("the runtime holds no sandbox %s", id)
	}
	return spec.Policy
}

// ruleNames are the names of rules, in order.
func ruleNames(rules []runtime.Rule) []string {
	names := []string{}
	for _, r := range rules {
		names = append(names, r.Name)
	}
	return names
}

// sameRule reports whether the rules a and b are equal as JSON values.
func sameRule(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	ca, errA := sandbox.CanonicalRule(a)
	cb, errB := sandbox.CanonicalRule(b)
	if errA != nil || errB != nil {
		t.Fatalf("rules %s and %s: %v, %v", a, b, errA, errB)
	}
	return ca == cb
}

// An agent's sandbox on the fork demo--a is denied a destination and
// proposes a rule for it; the owner of demo approves it for the workspace,
// so that a sandbox created later on another fork of demo has it from the
// start. The owner then deletes the rule: the sandbox that has it keeps it,
// and a sandbox created after the delete does not get it.
func TestSandboxPolicy_AnApprovedRuleReachesLaterSandboxesUntilItIsDeleted(t *testing.T) {
	parallel(t)
	h := startSandboxCreateHubWith(t, sandboxHubConfig{profiles: policyProfile, compose: []func(*hub.Composition){func(c *hub.Composition) {
		c.SetSandboxPolicyCollector(sandbox.PolicyCollectorOptions{Interval: 20 * time.Millisecond, RetryDelay: 20 * time.Millisecond})
	}}})
	h.putSecret(t, secret.ScopeOrg, h.org, "MODEL_KEY", "model-SECRET")
	for _, name := range []string{"a", "b"} {
		h.object(t, http.MethodPost, "/api/v1/workspaces/demo/forks", map[string]any{"name": name}, http.StatusCreated)
	}
	s1, _ := policySandbox(t, h, "demo--a")["id"].(string)
	rule := json.RawMessage(`{"endpoints":[{"host":"registry.example.org","port":443}],"binaries":[{"path":"/usr/bin/npm"}]}`)
	var ruleID, s2 string

	if !t.Run("a developer approves the agent's proposal for the workspace", func(t *testing.T) {
		h.fake.EmitPolicyLog("parley", s1, runtime.LogEntry{Action: "deny", Program: "npm", Host: "registry.example.org", Port: 443})
		eventually(t, 10*time.Second, "the denial's event", func() bool {
			return len(policyEvents(t, h, sandbox.EventDeny)) == 1
		})
		h.fake.AddProposal("parley", runtime.Proposal{
			Sandbox: s1, Rule: runtime.Rule{Name: "npm", Body: rule}, Summary: "let npm reach registry.example.org:443",
			Findings: []runtime.Finding{{Severity: "high", Message: "a public package registry"}},
		})
		var items []any
		eventually(t, 10*time.Second, "parley sandbox proposals to print one proposal", func() bool {
			listed := h.parley(t, "sandbox", "proposals", "demo--a", s1)
			if listed.code != 0 {
				t.Fatalf("parley sandbox proposals demo--a %s = %d %s %s; want 0", s1, listed.code, listed.stdout, listed.stderr)
			}
			items, _ = listed.json(t)["items"].([]any)
			return len(items) == 1
		})
		proposal, _ := items[0].(map[string]any)
		proposalID, _ := proposal["id"].(string)
		if findings, _ := proposal["findings"].([]any); len(findings) != 1 || findings[0].(map[string]any)["severity"] != "high" {
			t.Errorf("the listed proposal = %v; want it with the runtime's high finding", proposal)
		}

		approved := h.parley(t, "sandbox", "approve", "demo--a", s1, proposalID, "--scope", "workspace", "--acknowledge-findings")

		if approved.code != 0 {
			t.Fatalf("parley sandbox approve = %d %s %s; want 0", approved.code, approved.stdout, approved.stderr)
		}
		decided := approved.json(t)
		ruleID, _ = decided["workspace_rule_id"].(string)
		if decided["state"] != "approved" || decided["scope"] != "workspace" || ruleID == "" {
			t.Fatalf("the approved proposal = %v; want state approved, scope workspace and a workspace_rule_id", decided)
		}
		if got := h.parley(t, "sandbox", "get", "demo--a", s1).json(t); got["policy_revision"] != 1.0 {
			t.Errorf("the first sandbox's policy_revision = %v; want 1", got["policy_revision"])
		}
		listed := h.parley(t, "sandbox", "rules", "demo")
		rules, _ := listed.json(t)["items"].([]any)
		if len(rules) != 1 {
			t.Fatalf("parley sandbox rules demo = %d %s; want one rule", listed.code, listed.stdout)
		}
		saved, _ := rules[0].(map[string]any)
		from, _ := saved["from_proposal"].(map[string]any)
		if saved["id"] != ruleID || from["workspace"] != "demo--a" {
			t.Errorf("the rule of demo = %v; want %s from a proposal on demo--a", saved, ruleID)
		}
		events := policyEvents(t, h, sandbox.EventDeny, sandbox.EventProposal, sandbox.EventProposalDecided, sandbox.EventRuleAdd)
		var types []string
		for _, e := range events {
			types = append(types, e.Type)
		}
		if want := []string{sandbox.EventDeny, sandbox.EventProposal, sandbox.EventProposalDecided, sandbox.EventRuleAdd}; !reflect.DeepEqual(types, want) {
			t.Errorf("the hub's log holds %v; want %v", types, want)
		} else if ws := events[3].Workspace; ws == nil || *ws != "demo" {
			t.Errorf("the rule_add event's workspace = %v; want demo", ws)
		}

		second := policySandbox(t, h, "demo--b")
		s2, _ = second["id"].(string)

		if got := second["workspace_rules"]; !reflect.DeepEqual(got, []any{ruleID}) {
			t.Errorf("the second sandbox's workspace_rules = %v; want [%s]", got, ruleID)
		}
		policy := createdPolicy(t, h, s2)
		if got := ruleNames(policy.Protected); !reflect.DeepEqual(got, []string{"parley-hub", "credential-model"}) {
			t.Errorf("the second sandbox's protected rules = %v; want the hub rule and the credential rule", got)
		}
		if len(policy.Added) != 1 || policy.Added[0].Name != ruleID || !sameRule(t, policy.Added[0].Body, rule) {
			t.Errorf("the second sandbox's added rules = %v; want the rule %s", policy.Added, ruleID)
		}
	}) {
		return
	}

	t.Run("a developer deletes the workspace rule and earlier sandboxes keep it", func(t *testing.T) {
		if listed := h.parley(t, "sandbox", "rules", "demo"); listed.code != 0 {
			t.Fatalf("parley sandbox rules demo = %d %s; want 0", listed.code, listed.stderr)
		}

		deleted := h.parley(t, "sandbox", "rule-delete", "demo", ruleID)

		if deleted.code != 0 || !reflect.DeepEqual(deleted.json(t), map[string]any{"id": ruleID, "status": "deleted"}) {
			t.Fatalf("parley sandbox rule-delete demo %s = %d %s %s; want 0 and the rule deleted", ruleID, deleted.code, deleted.stdout, deleted.stderr)
		}
		if listed := h.parley(t, "sandbox", "rules", "demo"); !reflect.DeepEqual(listed.json(t)["items"], []any{}) {
			t.Errorf("parley sandbox rules demo after the delete = %s; want no items", listed.stdout)
		}
		removed := policyEvents(t, h, sandbox.EventRuleRemove)
		if len(removed) != 1 || removed[0].ResourceType != "sandbox_rule" || removed[0].ResourceID != ruleID || removed[0].SessionID != nil {
			t.Errorf("the rule_remove events = %+v; want one of the sandbox_rule %s with no session", removed, ruleID)
		}
		third := policySandbox(t, h, "demo")
		if got := third["workspace_rules"]; !reflect.DeepEqual(got, []any{}) {
			t.Errorf("the sandbox created after the delete has workspace_rules %v; want []", got)
		}
		if added := createdPolicy(t, h, third["id"].(string)).Added; len(added) != 0 {
			t.Errorf("the runtime created the sandbox after the delete with the added rules %v; want none", added)
		}
		earlier := h.parley(t, "sandbox", "get", "demo--b", s2).json(t)
		if got := earlier["workspace_rules"]; !reflect.DeepEqual(got, []any{ruleID}) {
			t.Errorf("the sandbox created before the delete has workspace_rules %v; want [%s]", got, ruleID)
		}

		again := h.parley(t, "sandbox", "rule-delete", "demo", ruleID)

		if again.code != 1 || again.stdout != "" || again.errorType(t) != "sandbox_rule_not_found" {
			t.Errorf("a second parley sandbox rule-delete = %d %q %s; want 1, nothing on stdout and sandbox_rule_not_found", again.code, again.stdout, again.stderr)
		}
	})
}
