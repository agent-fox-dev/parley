package acceptance

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

// fourRulePolicy lands what touches docs/ freely, asks the check test for
// src/, one approval of the owner for billing/, and never lands
// deploy/secrets/.
const fourRulePolicy = `{"rules": [
	{"name": "docs", "paths": ["docs/**"], "require": {}},
	{"name": "code", "paths": ["src/**"], "require": {"checks": ["test"]}},
	{"name": "billing", "paths": ["billing/**"], "require": {"approvals": 1, "approvers": ["owner"]}},
	{"name": "secrets", "paths": ["deploy/secrets/**"], "require": {"block": true}}
]}`

// policyHub is demoHub with fourRulePolicy set on demo by its owner.
func policyHub(t *testing.T) (*harness.Hub, *harness.Workspace) {
	t.Helper()
	hub, demo := demoHub(t)
	rev := hub.SetPolicy(demo.Owner.Key, demo.Slug, fourRulePolicy)
	if current := harness.Num(hub.Get(demo.Owner.Key, "/api/v1/workspaces/demo/policy").Body["revision"]); current != rev {
		t.Fatalf("demo's policy is at revision %d, want the revision %d the set answered", current, rev)
	}
	return hub, demo
}

// landingStatus is the landing status of change number of demo.
func landingStatus(t *testing.T, hub *harness.Hub, credential string, number int) string {
	t.Helper()
	return harness.Str(harness.Field(hub.Get(credential, changePath(number, "")).Body, "landing.status"))
}

// requirements are the requirements of a landing or an evaluation.
func requirements(landing map[string]any) []map[string]any {
	raw, _ := landing["requirements"].([]any)
	out := []map[string]any{}
	for _, it := range raw {
		m, _ := it.(map[string]any)
		out = append(out, m)
	}
	return out
}

// ruleNames are the rule of each requirement of a landing or an
// evaluation, in order.
func ruleNames(landing map[string]any) []string {
	names := []string{}
	for _, r := range requirements(landing) {
		names = append(names, harness.Str(r["rule"]))
	}
	return names
}

// counted is how many approvals count for the requirement of rule in the
// landing of change number of demo, -1 when it has no such requirement,
// with the landing's status.
func counted(t *testing.T, hub *harness.Hub, credential string, number int, rule string) (int, string) {
	t.Helper()
	landing, _ := hub.Get(credential, changePath(number, "")).Body["landing"].(map[string]any)
	for _, r := range requirements(landing) {
		if r["rule"] == rule {
			return harness.Num(harness.Field(r, "approvals.counted")), harness.Str(landing["status"])
		}
	}
	return -1, harness.Str(landing["status"])
}

// pushedBy clones demo with credential, commits the files on the new
// branch, and pushes it asking for a change, which the push opens with
// credential. It returns the clone and the change's number.
func pushedBy(t *testing.T, hub *harness.Hub, demo *harness.Workspace, credential, branch string, files map[string]string) (string, int) {
	t.Helper()
	clone := hub.Clone(demo.Remote, credential)
	hub.Git(clone, "", "checkout", "-q", "-b", branch).Must(t)
	hub.CommitFiles(clone, "Change "+branch, files)
	return clone, changeOfPush(t, hub.PushChange(clone, "", branch, "change "+branch).Must(t))
}

func TestPolicy_DocsOnlyChangeLandsWithoutApproval(t *testing.T) {
	t.Parallel()
	hub, demo := policyHub(t)
	key := demo.Owner.Key
	n := openChange(t, hub, key, "guide", map[string]string{"docs/guide.md": "# Guide\n"})

	landing, _ := hub.Get(key, changePath(n, "")).Body["landing"].(map[string]any)
	if landing["status"] != "ready" || !slices.Equal(ruleNames(landing), []string{"docs"}) {
		t.Errorf("the docs-only change opened with the landing %v, want ready under the rule docs alone", landing)
	}
	if approvals := hub.Get(key, changePath(n, "/approvals")).Items(); len(approvals) != 0 {
		t.Errorf("the docs-only change has the approvals %v, want none", approvals)
	}
	if l := hub.Land(key, demo.Slug, n); l.Body["status"] != "landed" {
		t.Errorf("the land answered %s, want the status landed", l.Raw)
	}
}

// ci is a second credential of the owner that can only report checks:
// only the owner or the admin token may report one.
func TestPolicy_CheckRequiredIsPendingUntilPassed(t *testing.T) {
	t.Parallel()
	hub, demo := policyHub(t)
	key := demo.Owner.Key
	ci := hub.PAT(demo.Owner, []string{"checks:write"})
	n := openChange(t, hub, key, "code", map[string]string{"src/a.txt": "a\n"})
	before := mainHead(t, hub, key)

	landing, _ := hub.Get(key, changePath(n, "")).Body["landing"].(map[string]any)
	reqs := requirements(landing)
	if landing["status"] != "pending" || len(reqs) != 1 || reqs[0]["rule"] != "code" ||
		!slices.Equal(members(reqs[0], "checks", "name"), []string{"test"}) ||
		!slices.Equal(members(reqs[0], "checks", "status"), []string{"pending"}) {
		t.Errorf("the change under src/ opened with the landing %v, want pending on the rule code with test pending", landing)
	}
	if r := hub.Call(http.MethodPost, changePath(n, "/land"), key, nil); r.Status != http.StatusConflict || r.ErrorType() != "policy_blocked" {
		t.Errorf("the land before any check answered %d %s, want 409 policy_blocked", r.Status, r.Raw)
	}
	if head := mainHead(t, hub, key); head != before {
		t.Errorf("main moved from %s to %s on a refused land", before, head)
	}

	hub.Call(http.MethodPut, changePath(n, "/checks/test"), ci, map[string]any{"status": "failed"}).Want(t, http.StatusOK)
	if status := landingStatus(t, hub, key, n); status != "pending" {
		t.Errorf("after test failed the landing is %s, want pending", status)
	}
	hub.CLI(ci, "change", "check", demo.Slug, strconv.Itoa(n), "test", "--status", "passed").Must(t)
	if status := landingStatus(t, hub, key, n); status != "ready" {
		t.Errorf("after test passed the landing is %s, want ready", status)
	}
	hub.Land(key, demo.Slug, n)
}

// Approvals count by credential: the token that pushed a version cannot
// approve it, while its owner's API key can.
func TestPolicy_ApprovalByPushingCredentialDoesNotCount(t *testing.T) {
	t.Parallel()
	hub, demo := policyHub(t)
	key := demo.Owner.Key
	p := hub.PAT(demo.Owner, []string{"git:write", "changes:write", "changes:approve"})
	_, n := pushedBy(t, hub, demo, p, "rates", map[string]string{"billing/rates.txt": "rate 1\n"})

	a1 := hub.Call(http.MethodPost, changePath(n, "/approvals"), p, map[string]any{"verdict": "approve"}).Want(t, http.StatusCreated)
	if forRules := members(a1.Body, "counts_for", ""); forRules == nil || len(forRules) != 0 {
		t.Errorf("the pushing token's approval counts for %s, want nothing", a1.Raw)
	}
	if c, status := counted(t, hub, key, n, "billing"); c != 0 || status != "pending" {
		t.Errorf("after the pushing token's approval billing counts %d and the landing is %s, want 0 and pending", c, status)
	}

	a2 := hub.CLI(key, "change", "approve", demo.Slug, strconv.Itoa(n)).Must(t)
	if forRules := members(a2.JSON(), "counts_for", ""); !slices.Equal(forRules, []string{"billing"}) {
		t.Errorf("the API key's approval counts for %s, want [billing]", a2.Stdout)
	}
	if c, status := counted(t, hub, key, n, "billing"); c != 1 || status != "ready" {
		t.Errorf("after the API key's approval billing counts %d and the landing is %s, want 1 and ready", c, status)
	}
}

func TestPolicy_BlockedPathCannotLandEvenWithOverride(t *testing.T) {
	t.Parallel()
	hub, demo := policyHub(t)
	key := demo.Owner.Key
	n := openChange(t, hub, key, "secret", map[string]string{"deploy/secrets/key.txt": "key\n"})
	before := mainHead(t, hub, key)

	if status := landingStatus(t, hub, key, n); status != "blocked" {
		t.Errorf("the change under deploy/secrets/ opened with the landing %s, want blocked", status)
	}
	urgent := map[string]any{"override": map[string]any{"reason": "urgent"}}
	for _, tc := range []struct {
		name, credential string
		body             any
	}{
		{"owner", key, nil},
		{"owner-override", key, urgent},
		{"admin-override", hub.AdminToken, urgent},
	} {
		r := hub.Call(http.MethodPost, changePath(n, "/land"), tc.credential, tc.body)
		if r.Status != http.StatusConflict || r.ErrorType() != "policy_blocked" || harness.Field(r.Body, "error.details.status") != "blocked" {
			t.Errorf("%s: the land answered %d %s, want 409 policy_blocked with the attempt blocked", tc.name, r.Status, r.Raw)
		}
		if head := mainHead(t, hub, key); head != before {
			t.Errorf("%s: main moved from %s to %s", tc.name, before, head)
		}
	}

	// The override itself is accepted: it passes an unmet check.
	code := openChange(t, hub, key, "code", map[string]string{"src/a.txt": "a\n"})
	if r := hub.Call(http.MethodPost, changePath(code, "/land"), key, map[string]any{"override": map[string]any{"reason": "x"}}); r.Status != http.StatusOK {
		t.Errorf("the owner's override of an unmet check answered %d %s, want 200", r.Status, r.Raw)
	}
}

// Every version is pushed with the agent's token, so that the owner's API
// key approval counts.
func TestPolicy_ApprovalSurvivesPureRebaseNotContentChange(t *testing.T) {
	t.Parallel()
	hub, demo := policyHub(t)
	key := demo.Owner.Key
	p := hub.PAT(demo.Owner, []string{"git:write", "changes:write"})
	clone, n := pushedBy(t, hub, demo, p, "rates", map[string]string{"billing/rates.txt": "rate 1\n"})
	hub.CLI(key, "change", "approve", demo.Slug, strconv.Itoa(n)).Must(t)
	if c, status := counted(t, hub, key, n, "billing"); c != 1 || status != "ready" {
		t.Fatalf("after the approval billing counts %d and the landing is %s, want 1 and ready", c, status)
	}
	hub.Land(key, demo.Slug, openChange(t, hub, key, "elsewhere", map[string]string{"docs/other.md": "other\n"}))

	push := func(version int, args ...string) {
		t.Helper()
		hub.Git(clone, "", append([]string{"push", "-q"}, args...)...).Must(t)
		harness.Eventually(t, "change "+strconv.Itoa(n)+" to reach version "+strconv.Itoa(version), func() bool {
			return harness.Num(hub.Get(key, changePath(n, "")).Body["version"]) == version
		})
	}
	hub.Git(clone, "", "fetch", "-q", "origin").Must(t)
	hub.Git(clone, "", "rebase", "-q", "origin/main").Must(t)
	push(2, "--force", "origin", "rates")
	if pe := harness.Num(hub.Get(key, changePath(n, "/versions/latest")).Body["patch_equivalent_to"]); pe != 1 {
		t.Errorf("the rebased version is patch-equivalent to %d, want 1", pe)
	}
	if c, status := counted(t, hub, key, n, "billing"); c != 1 || status != "ready" {
		t.Errorf("after the pure rebase billing counts %d and the landing is %s, want 1 and ready", c, status)
	}

	hub.CommitFiles(clone, "Raise the rate", map[string]string{"billing/rates.txt": "rate 2\n"})
	push(3, "origin", "rates")
	if c, status := counted(t, hub, key, n, "billing"); c != 0 || status != "pending" {
		t.Errorf("after the content change billing counts %d and the landing is %s, want 0 and pending", c, status)
	}
}

func TestPolicy_EvaluatePredictsEachOutcome(t *testing.T) {
	t.Parallel()
	hub, demo := policyHub(t)
	key := demo.Owner.Key
	cases := []struct{ path, status, rule string }{
		{"docs/guide.md", "ready", "docs"},
		{"src/a.txt", "pending", "code"},
		{"billing/rates.txt", "pending", "billing"},
		{"deploy/secrets/key.txt", "blocked", "secrets"},
	}
	for _, tc := range cases {
		e := hub.Call(http.MethodPost, "/api/v1/workspaces/demo/policy/evaluate", key, map[string]any{"paths": []string{tc.path}}).
			Want(t, http.StatusOK)
		if e.Body["status"] != tc.status || !slices.Equal(ruleNames(e.Body), []string{tc.rule}) {
			t.Errorf("%s: evaluate answered %s, want %s under the rule %s alone", tc.path, e.Raw, tc.status, tc.rule)
		}
		if cli := hub.CLI(key, "policy", "evaluate", demo.Slug, "--paths", tc.path).Must(t); cli.JSON()["status"] != tc.status {
			t.Errorf("%s: parley policy evaluate printed %s, want the status %s", tc.path, cli.Stdout, tc.status)
		}
	}
	if items := hub.Get(key, changes+"?state=draft&state=open&state=landed&state=abandoned").Items(); len(items) != 0 {
		t.Fatalf("evaluating opened the changes %v, want none", items)
	}

	for _, tc := range cases {
		branch := strings.ReplaceAll(strings.Split(tc.path, ".")[0], "/", "-")
		n := openChange(t, hub, key, branch, map[string]string{tc.path: "content\n"})
		landing, _ := hub.Get(key, changePath(n, "")).Body["landing"].(map[string]any)
		if landing["status"] != tc.status || !slices.Equal(ruleNames(landing), []string{tc.rule}) {
			t.Errorf("%s: the change opened with the landing %v, want %s under the rule %s alone", tc.path, landing, tc.status, tc.rule)
		}
	}
}
