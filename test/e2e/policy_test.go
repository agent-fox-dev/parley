package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/hub"
)

// examplePolicy lets docs land freely, asks one approval of the owner and
// two checks for billing, and blocks deploy/secrets.
const examplePolicy = `{
  "default": {"approvals": 0},
  "rules": [
    {"name": "docs", "paths": ["docs/**", "**/*.md"], "exclude": ["docs/internal/**"], "require": {}},
    {"name": "billing", "paths": ["billing/**"], "targets": ["main", "release/*"],
     "require": {"approvals": 1, "approvers": ["owner"], "checks": ["test", "billing-e2e"], "fresh_checks": true}},
    {"name": "secrets", "paths": ["deploy/secrets/**"], "require": {"block": true}}
  ]
}`

// startPolicyHub serves a fork hub whose mergeability checks are not
// spaced, and writes examplePolicy to policy.json in a directory of the
// test's, whose path it returns.
func startPolicyHub(t *testing.T) (*lifecycleHub, string) {
	t.Helper()
	h := &lifecycleHub{forkHub: startForkHubWith(t, nil, func(c *hub.Composition) {
		c.AdjustChange(func(d *change.Deps) { d.MergeableDebounce = 0 })
	})}
	file := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(file, []byte(examplePolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	return h, file
}

// policyUpdates are the metadata of the parley.policy.update events of demo
// in the hub's log, oldest first.
func (h *lifecycleHub) policyUpdates(t *testing.T) []map[string]any {
	t.Helper()
	rows, err := h.Store.Main().Query(`SELECT metadata FROM hub_events
		WHERE type = 'parley.policy.update' AND workspace = 'demo' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var updates []map[string]any
	for rows.Next() {
		var meta string
		if err := rows.Scan(&meta); err != nil {
			t.Fatal(err)
		}
		updates = append(updates, decoded(t, []byte(meta)))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return updates
}

// A developer sets the policy of demo from a file beside the code; the open
// changes of demo follow it at once, and the revision reads back as the set
// command printed it.
func TestPolicy_ADeveloperSetsAPolicyAndTheOpenChangesFollowIt(t *testing.T) {
	h, file := startPolicyHub(t)
	owner := cloneAs(t, h.forkHub, h.key, "demo")
	top := strings.TrimSpace(owner("rev-parse", "--show-toplevel"))
	want := map[string]string{}
	for _, path := range []string{"docs/a.md", "billing/a.go", "deploy/secrets/k"} {
		branch := strings.ReplaceAll(path, "/", "-")
		owner("checkout", "--quiet", "-b", branch, "origin/main")
		if err := os.MkdirAll(filepath.Join(top, filepath.Dir(path)), 0o700); err != nil {
			t.Fatal(err)
		}
		commitFile(t, owner, path, "x\n", "Add "+path)
		owner("push", "--quiet", "origin", branch)
		want[h.open(t, h.key, "demo", branch, map[string]any{"intent": "why"})] = path
	}

	set := h.runAs(t, h.key, "policy", "set", "demo", "--file", file)
	if set.code != 0 || set.stderr != "" {
		t.Fatalf("policy set exited %d with stderr %q, want 0 and nothing", set.code, set.stderr)
	}
	stored := set.json(t)
	if stored["revision"] != float64(1) {
		t.Errorf("policy set printed %s, want revision 1", set.stdout)
	}
	if updates := h.policyUpdates(t); len(updates) != 1 || updates[0]["revision"] != float64(1) || updates[0]["rules"] != float64(3) {
		t.Errorf("the policy updates of demo are %v, want one with revision 1 and 3 rules", updates)
	}

	statuses := map[string]string{"docs/a.md": "ready", "billing/a.go": "pending", "deploy/secrets/k": "blocked"}
	for number, path := range want {
		c := h.read(t, h.key, number)
		if got := str(c, "landing", "status"); got != statuses[path] {
			t.Errorf("demo#%s, touching %s, has landing %v, want status %s", number, path, c["landing"], statuses[path])
		}
	}

	get := h.runAs(t, h.key, "policy", "get", "demo", "--revision", "1")
	if get.code != 0 || !reflect.DeepEqual(get.json(t), stored) {
		t.Errorf("policy get --revision 1 exited %d and printed %s, want 0 and what policy set printed, %s", get.code, get.stdout, set.stdout)
	}
}

// An agent with a token bound to its task fork asks what two paths would
// need under the parent's policy before any change exists, and cannot
// replace that policy.
func TestPolicy_AnAgentAsksWhatPathsWouldNeedAndCannotSetThePolicy(t *testing.T) {
	h, file := startPolicyHub(t)
	var doc map[string]any
	if err := json.Unmarshal([]byte(examplePolicy), &doc); err != nil {
		t.Fatal(err)
	}
	if status, body := h.Call(t, http.MethodPut, "/api/v1/workspaces/demo/policy", h.key, doc); status != http.StatusOK {
		t.Fatalf("PUT demo's policy = %d %s, want 200", status, body)
	}
	status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/forks", h.memberKey, map[string]any{
		"name": "t1", "purpose": "work on billing", "auto_change": false,
		"token": map[string]any{"permissions": []string{"changes:read", "workspaces:write"}},
	})
	if status != http.StatusCreated {
		t.Fatalf("create demo--t1 = %d %s, want 201", status, body)
	}
	agent := str(decoded(t, body), "token", "token")

	ev := h.runAs(t, agent, "policy", "evaluate", "demo", "--paths", "billing/a.go,docs/x.md")
	if ev.code != 0 || ev.stderr != "" {
		t.Fatalf("policy evaluate exited %d with stderr %q, want 0 and nothing", ev.code, ev.stderr)
	}
	answer := ev.json(t)
	requirements, _ := answer["requirements"].([]any)
	var rules []string
	for _, r := range requirements {
		rules = append(rules, str(r.(map[string]any), "rule"))
	}
	if answer["policy_revision"] != float64(1) || answer["status"] != "pending" || !reflect.DeepEqual(rules, []string{"docs", "billing"}) {
		t.Fatalf("policy evaluate printed %s, want revision 1, status pending and the requirements docs and billing", ev.stdout)
	}
	billing := requirements[1].(map[string]any)
	approvals, _ := billing["approvals"].(map[string]any)
	checks, _ := json.Marshal(billing["checks"])
	if approvals["counted"] != float64(0) || string(checks) != `[{"name":"test","status":"pending"},{"name":"billing-e2e","status":"pending"}]` {
		t.Errorf("the billing requirement is %v, want no approval counted and both checks pending", billing)
	}

	set := h.runAs(t, agent, "policy", "set", "demo", "--file", file)
	if set.code != 1 || set.stdout != "" || set.errorType(t) != "token_bound" {
		t.Errorf("policy set with the fork's token exited %d, printed %q and %s; want 1, nothing and token_bound", set.code, set.stdout, set.stderr)
	}
	if status, body := h.Call(t, http.MethodGet, "/api/v1/workspaces/demo/policy", h.key, nil); status != http.StatusOK || decoded(t, body)["revision"] != float64(1) {
		t.Errorf("GET demo's policy = %d %s, want 200 at revision 1", status, body)
	}
	if updates := h.policyUpdates(t); len(updates) != 1 {
		t.Errorf("the policy updates of demo are %v, want the one of the owner's PUT", updates)
	}
}
