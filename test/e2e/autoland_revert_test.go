package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/land"
)

// commitOnBranch commits content to file on the new branch of demo, from
// main, through the API as the developer, and returns the commit.
func (h *lifecycleHub) commitOnBranch(t *testing.T, branch, file, content, message string) string {
	t.Helper()
	a := scmWrite(t, h.workspaceHub, http.MethodPost, "/api/v1/workspaces/demo/scm/commits", h.key, map[string]any{
		"branch": branch, "base_ref": "main", "message": message,
		"operations": []map[string]any{{"op": "put", "path": file, "content": content}},
	})
	if a.status != http.StatusCreated {
		t.Fatalf("the commit on %s = %d %s, want 201", branch, a.status, a.body)
	}
	return str(a.json, "commit", "sha")
}

// fileOnMain is the content of file on demo's main, read through the API.
func (h *lifecycleHub) fileOnMain(t *testing.T, file string) string {
	t.Helper()
	r := scmGet(t, h.workspaceHub, "/api/v1/workspaces/demo/scm/file?ref=main&path="+file, h.key)
	if r.status != http.StatusOK {
		t.Fatalf("GET %s on main = %d %s, want 200", file, r.status, r.body)
	}
	return string(r.body)
}

// loggedActor is an event of the hub's log with its actor, as GET
// /events/log answers it.
type loggedActor struct {
	Type       string         `json:"type"`
	ResourceID string         `json:"resource_id"`
	ActorType  string         `json:"actor_type"`
	ActorID    *string        `json:"actor_id"`
	Metadata   map[string]any `json:"metadata"`
}

// logged are the events of type typ in the hub's log, oldest first.
func (h *lifecycleHub) logged(t *testing.T, typ string) []loggedActor {
	t.Helper()
	status, body := h.Call(t, http.MethodGet, "/api/v1/events/log?after=0&limit=200&types="+typ, h.Token, nil)
	var page struct {
		Items []loggedActor `json:"items"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &page) != nil {
		t.Fatalf("GET events/log = %d %s, want 200 with a page", status, body)
	}
	return page.Items
}

// An agent opens a change with auto_land in a workspace whose policy asks
// for the check test; the change waits, pending, and lands by itself once
// a check runner reports test passed: one attempt, triggered by auto_land,
// whose land event names the system as its actor.
func TestAutoLand_AChangeLandsByItselfOnceItsRequiredCheckPasses(t *testing.T) {
	parallel(t)
	h := startLifecycleHub(t)
	policy := map[string]any{"default": map[string]any{"approvals": 0, "checks": []string{"test"}}}
	if status, body := h.Call(t, http.MethodPut, "/api/v1/workspaces/demo/policy", h.key, policy); status != http.StatusOK {
		t.Fatalf("PUT demo's policy = %d %s, want 200", status, body)
	}
	h.commitOnBranch(t, "feat", "b.txt", "b\n", "Add b")
	n := h.open(t, h.key, "demo", "feat", map[string]any{"intent": "why", "auto_land": true})
	if c := h.until(t, n, "computed", computed); str(c, "landing", "status") != "pending" || c["state"] != "open" {
		t.Fatalf("demo#%s opened %v with landing %v, want open and pending", n, c["state"], c["landing"])
	}

	status, body := h.Call(t, http.MethodPut, "/api/v1/workspaces/demo/changes/"+n+"/checks/test", h.key, map[string]any{"status": "passed"})
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("PUT checks/test = %d %s, want it recorded", status, body)
	}
	h.within(t, 10*time.Second, n, "landed", func(c map[string]any) bool { return c["state"] == "landed" })

	attempts, _ := h.get(t, "/workspaces/demo/changes/"+n+"/lands")["items"].([]any)
	if len(attempts) != 1 {
		t.Fatalf("demo#%s has the land attempts %v, want one", n, attempts)
	}
	a := attempts[0].(map[string]any)
	if a["trigger"] != land.TriggerAutoLand || a["status"] != land.StatusLanded {
		t.Errorf("the attempt is %v, want trigger auto_land and status landed", a)
	}
	if head := refHead(t, h.workspaceHub, "demo", "main", h.key); a["landed_sha"] != head || h.fileOnMain(t, "b.txt") != "b\n" {
		t.Errorf("main is at %s, want the attempt's landed commit %v holding b.txt", head, a["landed_sha"])
	}
	var system []loggedActor
	for _, e := range h.logged(t, land.EventLand) {
		if e.ResourceID == n && e.ActorType == "system" && e.ActorID == nil {
			system = append(system, e)
		}
	}
	if len(system) != 1 {
		t.Errorf("the log holds %v, want one %s of demo#%s with actor system", h.logged(t, land.EventLand), land.EventLand, n)
	}
}

// A developer lands a change that edits a.txt, reverts it from the shell,
// sees the branch revert/<n> on the hub and lands the reverting change
// from the shell: main's a.txt is back to what it was, the original names
// its reverter and the log holds the revert.
func TestRevert_ADeveloperRevertsALandedChangeFromTheShellAndLandsTheRevert(t *testing.T) {
	parallel(t)
	h := startLifecycleHub(t)
	before := h.fileOnMain(t, "a.txt")
	h.commitOnBranch(t, "edit", "a.txt", "l1\nchanged\nl3\n", "Change a")
	n := h.open(t, h.key, "demo", "edit", map[string]any{"intent": "why"})
	h.until(t, n, "computed", computed)
	if status, a := h.land(t, h.key, n, ""); status != http.StatusOK || a["status"] != land.StatusLanded {
		t.Fatalf("the land of demo#%s = %d %v, want 200 landed", n, status, a)
	}
	if got := h.fileOnMain(t, "a.txt"); got == before {
		t.Fatalf("a.txt on main is %q after the land, want the change's edit", got)
	}

	r := h.runAs(t, h.key, "change", "revert", "demo", n)
	if r.code != 0 {
		t.Fatalf("change revert exited %d: %s", r.code, r.stderr)
	}
	reverting := r.json(t)
	if fmt.Sprint(reverting["reverts"]) != n {
		t.Fatalf("change revert printed %s, want a change with reverts %s", r.stdout, n)
	}
	m := fmt.Sprint(reverting["number"])
	g := newGitClient(t, h.URL(""), h.key)
	if out, err := g.git("", "ls-remote", h.URL("/git/acme/demo.git"), "refs/heads/revert/"+n); err != nil || !strings.Contains(out, "refs/heads/revert/"+n) {
		t.Errorf("git ls-remote of demo = %v %q, want refs/heads/revert/%s", err, out, n)
	}

	h.until(t, m, "computed", computed)
	l := h.runAs(t, h.key, "change", "land", "demo", m, "--wait")
	if l.code != 0 || l.json(t)["status"] != land.StatusLanded {
		t.Fatalf("change land of demo#%s exited %d with %s %s, want 0 and a landed attempt", m, l.code, l.stdout, l.stderr)
	}
	if got := h.fileOnMain(t, "a.txt"); got != before {
		t.Errorf("a.txt on main is %q after the revert landed, want its content before demo#%s, %q", got, n, before)
	}
	if c := h.read(t, h.key, n); fmt.Sprint(c["reverted_by"]) != m {
		t.Errorf("demo#%s has reverted_by %v, want %s", n, c["reverted_by"], m)
	}
	if evs := h.changeEvents(t, n, 0, "parley.change.revert"); len(evs) != 1 || fmt.Sprint(evs[0].meta["reverted_by"]) != m {
		t.Errorf("the revert events of demo#%s = %v, want one naming %s as the reverter", n, evs, m)
	}
}
