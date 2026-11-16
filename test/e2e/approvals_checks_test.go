package e2e

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// reviewPolicy asks one approval of the owner and the check test of every
// change of demo.
var reviewPolicy = map[string]any{
	"default": map[string]any{"approvals": 1, "approvers": []string{"owner"}, "checks": []string{"test"}},
}

// setReviewPolicy sets reviewPolicy on demo as the developer.
func (h *lifecycleHub) setReviewPolicy(t *testing.T) {
	t.Helper()
	if status, body := h.Call(t, http.MethodPut, "/api/v1/workspaces/demo/policy", h.key, reviewPolicy); status != http.StatusOK {
		t.Fatalf("PUT demo's policy = %d %s, want 200", status, body)
	}
}

// requirement is the one requirement of the landing of the change number
// of demo, with the landing's status.
func (h *lifecycleHub) requirement(t *testing.T, number string) (status string, counted any, checks string) {
	t.Helper()
	landing, _ := h.read(t, h.key, number)["landing"].(map[string]any)
	reqs, _ := landing["requirements"].([]any)
	if len(reqs) != 1 {
		t.Fatalf("the landing of demo#%s is %v, want one requirement", number, landing)
	}
	req := reqs[0].(map[string]any)
	approvals, _ := req["approvals"].(map[string]any)
	raw, _ := json.Marshal(req["checks"])
	status, _ = landing["status"].(string)
	return status, approvals["counted"], string(raw)
}

// items are the items of the list GET path answers the developer.
func (h *lifecycleHub) items(t *testing.T, path string) []map[string]any {
	t.Helper()
	list, _ := h.get(t, path)["items"].([]any)
	out := []map[string]any{}
	for _, it := range list {
		out = append(out, it.(map[string]any))
	}
	return out
}

// A review agent reports the check the policy asks for with a token that
// can only report checks, the agent cannot certify its own change with the
// token bound to its fork, and the owner's approval from the shell makes
// the change ready; both commands that list the evidence show it.
func TestApprovalsChecks_ACheckReportAndTheOwnersApprovalMakeAChangeReady(t *testing.T) {
	parallel(t)
	h := startLifecycleHub(t)
	h.setReviewPolicy(t)
	agent := cloneAs(t, h.forkHub, h.agent, "demo--t1")
	agent("checkout", "--quiet", "-b", "feat")
	commitFile(t, agent, "b.txt", "b\n", "Add b")
	agent("push", "--quiet", "origin", "feat")
	n := h.open(t, h.agent, "demo--t1", "feat", map[string]any{"intent": "why"})
	if status, _, _ := h.requirement(t, n); status != "pending" {
		t.Fatalf("demo#%s opened with landing %s, want pending", n, status)
	}
	ci := h.IssuePAT(t, identity.PATSpec{UserID: h.user, Permissions: []string{"checks:write"}, CreatedAt: time.Now()})

	check := h.runAs(t, ci, "change", "check", "demo", n, "test", "--status", "passed")
	if check.code != 0 || check.json(t)["status"] != "passed" {
		t.Fatalf("change check exited %d with %s %s, want 0 and the passed check", check.code, check.stdout, check.stderr)
	}
	if status, _, _ := h.requirement(t, n); status != "pending" {
		t.Errorf("after the check report the landing is %s, want pending until an approval counts", status)
	}
	status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/changes/"+n+"/approvals", h.agent, map[string]any{"verdict": "approve"})
	if status != http.StatusForbidden || (writeAnswer{json: decoded(t, body)}).errorType() != "token_bound" {
		t.Errorf("the fork token's verdict = %d %s, want 403 token_bound", status, body)
	}

	approve := h.runAs(t, h.key, "change", "approve", "demo", n)
	if approve.code != 0 || !reflect.DeepEqual(approve.json(t)["counts_for"], []any{"default"}) {
		t.Fatalf("change approve exited %d with %s %s, want 0 and counts_for [default]", approve.code, approve.stdout, approve.stderr)
	}
	if status, counted, named := h.requirement(t, n); status != "ready" || counted != float64(1) || named != `[{"name":"test","status":"passed"}]` {
		t.Errorf("the landing is %s with %v counted and checks %s, want ready, 1 and test passed", status, counted, named)
	}

	approvals := h.runAs(t, h.key, "change", "approvals", "demo", n)
	items, _ := approvals.json(t)["items"].([]any)
	if approvals.code != 0 || len(items) != 1 {
		t.Fatalf("change approvals exited %d with %s, want 0 and the owner's verdict alone", approvals.code, approvals.stdout)
	}
	verdict := items[0].(map[string]any)
	if str(verdict, "by", "actor_id") != h.user || !reflect.DeepEqual(verdict["counts_for"], []any{"default"}) {
		t.Errorf("change approvals listed %v, want the owner's verdict counting for default", verdict)
	}
	checks := h.runAs(t, h.key, "change", "checks", "demo", n)
	results, _ := checks.json(t)["items"].([]any)
	if checks.code != 0 || len(results) != 1 || results[0].(map[string]any)["name"] != "test" || results[0].(map[string]any)["status"] != "passed" {
		t.Errorf("change checks exited %d with %s, want 0 and test passed", checks.code, checks.stdout)
	}
	if got := typesOf(h.changeEvents(t, n, 0, "parley.change.check", "parley.change.approve", "parley.change.reject")); !reflect.DeepEqual(got, []string{"parley.change.check", "parley.change.approve"}) {
		t.Errorf("the evidence events of demo#%s = %v, want the check and then the approval", n, got)
	}
}

// The verdicts and the check result of a change follow a rebase that keeps
// its content, so it stays ready, and are left behind by a commit that
// changes it, which leaves the change pending; the evidence of the earlier
// versions stays readable.
func TestApprovalsChecks_EvidenceSurvivesAPureRebaseAndNotAContentChange(t *testing.T) {
	parallel(t)
	h := startLifecycleHub(t)
	h.setReviewPolicy(t)
	agentpat := h.IssuePAT(t, identity.PATSpec{UserID: h.user, CreatedAt: time.Now(),
		Permissions: []string{"changes:read", "changes:write", "changes:approve", "git:write", "checks:write"}})
	commits := "/api/v1/workspaces/demo/scm/commits"
	work := scmWrite(t, h.workspaceHub, http.MethodPost, commits, agentpat, map[string]any{
		"branch": "work", "base_ref": "main", "message": "Add work",
		"operations": []map[string]any{{"op": "put", "path": "work.txt", "content": "work\n"}},
	})
	if work.status != http.StatusCreated {
		t.Fatalf("the commit creating work = %d %s, want 201", work.status, work.body)
	}
	n := h.open(t, agentpat, "demo", "work", map[string]any{"intent": "why"})
	approvals := "/workspaces/demo/changes/" + n + "/approvals"
	checks := "/workspaces/demo/changes/" + n + "/checks"
	send := func(credential, method, path string, body map[string]any) {
		t.Helper()
		if status, raw := h.Call(t, method, "/api/v1"+path, credential, body); status/100 != 2 {
			t.Fatalf("%s %s = %d %s, want success", method, path, status, raw)
		}
	}

	send(agentpat, http.MethodPost, approvals, map[string]any{"verdict": "approve"})
	if _, counted, _ := h.requirement(t, n); counted != float64(0) {
		t.Errorf("after the agent approved with the token that created the version, %v counted, want 0", counted)
	}
	send(agentpat, http.MethodPut, checks+"/test", map[string]any{"status": "passed"})
	// The owner's API key replaces the verdict the owner's agent token gave,
	// one verdict per user and version; the member's, which no approver
	// form of the policy names, is the second verdict the rebase carries.
	send(h.key, http.MethodPost, approvals, map[string]any{"verdict": "approve"})
	send(h.memberKey, http.MethodPost, approvals, map[string]any{"verdict": "approve"})
	if status, counted, _ := h.requirement(t, n); status != "ready" || counted != float64(1) {
		t.Fatalf("after the owner approved, the landing is %s with %v counted, want ready with 1", status, counted)
	}

	main := scmWrite(t, h.workspaceHub, http.MethodPost, commits, h.key, map[string]any{
		"branch": "main", "expected_head_sha": refHead(t, h.workspaceHub, "demo", "main", h.key), "message": "Elsewhere",
		"operations": []map[string]any{{"op": "put", "path": "other.txt", "content": "other\n"}},
	})
	if main.status != http.StatusCreated {
		t.Fatalf("the owner's commit on main = %d %s, want 201", main.status, main.body)
	}
	if rb := scmWrite(t, h.workspaceHub, http.MethodPost, "/api/v1/workspaces/demo/scm/rebase", agentpat,
		map[string]any{"branch": "work", "onto": "main"}); rb.status != http.StatusOK || rb.json["status"] != "rebased" {
		t.Fatalf("the rebase of work = %d %s, want 200 rebased", rb.status, rb.body)
	}
	h.until(t, n, "reached version 2", func(c map[string]any) bool { return c["version"] == float64(2) })
	if pe := h.version(t, n, "2")["patch_equivalent_to"]; pe != float64(1) {
		t.Errorf("version 2 is patch-equivalent to %v, want 1", pe)
	}
	carried := h.items(t, approvals+"?version=2")
	if len(carried) != 2 || carried[0]["carried_from"] != float64(1) || carried[1]["carried_from"] != float64(1) {
		t.Errorf("the verdicts on version 2 are %v, want both carried from 1", carried)
	}
	if current := h.items(t, checks); len(current) != 1 || current[0]["name"] != "test" || current[0]["version"] != float64(1) || current[0]["status"] != "passed" {
		t.Errorf("the checks of version 2 are %v, want test passed on version 1", current)
	}
	if status, _, _ := h.requirement(t, n); status != "ready" {
		t.Errorf("after the pure rebase the landing is %s, want ready", status)
	}

	edit := scmWrite(t, h.workspaceHub, http.MethodPost, commits, agentpat, map[string]any{
		"branch": "work", "expected_head_sha": refHead(t, h.workspaceHub, "demo", "work", h.key), "message": "Change work",
		"operations": []map[string]any{{"op": "put", "path": "work.txt", "content": "work, changed\n"}},
	})
	if edit.status != http.StatusCreated {
		t.Fatalf("the content change on work = %d %s, want 201", edit.status, edit.body)
	}
	h.until(t, n, "reached version 3", func(c map[string]any) bool { return c["version"] == float64(3) })
	if v3, current := h.items(t, approvals+"?version=3"), h.items(t, checks); len(v3) != 0 || len(current) != 0 {
		t.Errorf("version 3 has verdicts %v and checks %v, want none of either", v3, current)
	}
	if status, counted, named := h.requirement(t, n); status != "pending" || counted != float64(0) || named != `[{"name":"test","status":"pending"}]` {
		t.Errorf("after the content change the landing is %s with %v counted and checks %s, want pending, 0 and test pending", status, counted, named)
	}
	for _, v := range []string{"1", "2"} {
		if got := h.items(t, approvals+"?version="+v); len(got) != 2 {
			t.Errorf("the verdicts on version %s are %v, want the two still readable", v, got)
		}
		if got := h.items(t, checks+"?version="+v); len(got) != 1 || got[0]["status"] != "passed" {
			t.Errorf("the checks of version %s are %v, want test passed still readable", v, got)
		}
	}
}
