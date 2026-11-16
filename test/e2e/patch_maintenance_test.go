package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// startPatchHub starts a carry-patch hub whose origin holds a branch for each
// of branches, creates the carry-patch workspace carry through the REST API,
// waits until it is ready, and registers each branch as a patch from the
// shell, in order. It returns the hub, origin and the ids of the patches.
func startPatchHub(t *testing.T, branches ...string) (*workspaceHub, *gitrepotest.Origin, []string) {
	t.Helper()
	origin := gitrepotest.NewOrigin(t)
	origin.Commit("main", "o1")
	for _, branch := range branches {
		origin.Branch(branch, "main")
		origin.Commit(branch, branch)
	}
	up := gitrepotest.NewOrigin(t)
	up.Commit("main", "u1")
	h := startCarryPatchHub(t, origin, up)
	status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces", h.key, map[string]any{
		"slug": "carry", "git_url": origin.URL(), "workspace_mode": "carry_patch", "upstream_url": up.URL(),
	})
	if status != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", status, body)
	}
	h.waitStatus(t, "carry", workspace.StatusReady)
	var ids []string
	for _, branch := range branches {
		id, _ := h.document(t, "patch", "add", "carry", "--branch", branch)["id"].(string)
		ids = append(ids, id)
	}
	return h, origin, ids
}

// document runs parley against the hub with the developer's key, fails the
// test unless it exits 0 with nothing on stderr and exactly one JSON object
// on stdout, and returns that object.
func (h *workspaceHub) document(t *testing.T, args ...string) map[string]any {
	t.Helper()
	r := h.run(t, args...)
	dec := json.NewDecoder(strings.NewReader(r.stdout))
	var doc map[string]any
	if err := dec.Decode(&doc); r.code != 0 || r.stderr != "" || err != nil || dec.Decode(new(any)) != io.EOF {
		t.Fatalf("parley %v exited %d with stdout %q, stderr %q; want 0, one JSON object and nothing", args, r.code, r.stdout, r.stderr)
	}
	return doc
}

// patchEvent is a patch event of the hub's event log, as GET
// /api/v1/events/log serves it.
type patchEvent struct {
	Type       string          `json:"type"`
	Workspace  *string         `json:"workspace"`
	ActorType  string          `json:"actor_type"`
	ActorID    *string         `json:"actor_id"`
	ResourceID string          `json:"resource_id"`
	Metadata   json.RawMessage `json:"metadata"`
}

// patchEvents are the events of the workspace slug in GET /api/v1/events/log
// whose type is one of types, oldest first.
func (h *workspaceHub) patchEvents(t *testing.T, slug string, types ...string) []patchEvent {
	t.Helper()
	status, body := h.Call(t, http.MethodGet, "/api/v1/events/log?after=0&limit=200", h.Token, nil)
	var page struct {
		Items []patchEvent `json:"items"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &page) != nil {
		t.Fatalf("GET /api/v1/events/log = %d %s, want 200 with a page", status, body)
	}
	var events []patchEvent
	for _, e := range page.Items {
		if e.Workspace != nil && *e.Workspace == slug && slices.Contains(types, e.Type) {
			events = append(events, e)
		}
	}
	return events
}

// A developer disables, reorders, removes and restores patches from the
// shell, and reads the list back in the order and statuses they left.
func TestPatchMaintenance_FromTheShell(t *testing.T) {
	parallel(t)
	h, _, ids := startPatchHub(t, "patch/a", "patch/b", "patch/c")
	a, b, c := ids[0], ids[1], ids[2]

	if p := h.document(t, "patch", "update", "carry", b, "--status", "disabled"); p["status"] != "disabled" {
		t.Errorf("patch update printed %v, want B disabled", p)
	}
	reordered := h.document(t, "patch", "reorder", "carry", c, a, b)
	items, _ := reordered["items"].([]any)
	var order []any
	for _, item := range items {
		order = append(order, item.(map[string]any)["id"])
	}
	if len(order) != 3 || order[0] != c || order[1] != a || order[2] != b {
		t.Errorf("patch reorder printed the ids %v, want C, A, B", order)
	}
	if p := h.document(t, "patch", "remove", "carry", c); len(p) != 2 || p["status"] != "removed" || p["patch_id"] != c {
		t.Errorf("patch remove printed %v, want {status: removed, patch_id: %s}", p, c)
	}
	if p := h.document(t, "patch", "restore", "carry", c); p["position"] != 3.0 || p["status"] != "active" {
		t.Errorf("patch restore printed %v, want C active at position 3", p)
	}

	type row struct {
		branch   string
		position float64
		status   string
		deleted  any
	}
	want := []row{{"patch/a", 1, "active", nil}, {"patch/b", 2, "disabled", nil}, {"patch/c", 3, "active", nil}}
	for _, args := range [][]string{{"patch", "list", "carry"}, {"patch", "list", "carry", "--include-deleted"}} {
		listed, _ := h.document(t, args...)["items"].([]any)
		var got []row
		for _, item := range listed {
			p := item.(map[string]any)
			position, _ := p["position"].(float64)
			status, _ := p["status"].(string)
			got = append(got, row{p["branch_name"].(string), position, status, p["deleted_at"]})
		}
		if len(got) != len(want) {
			t.Errorf("parley %v listed %v, want %v", args, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("parley %v listed %v, want %v", args, got, want)
				break
			}
		}
	}

	events := h.patchEvents(t, "carry", "parley.patch.update", "parley.patch.reorder", "parley.patch.delete", "parley.patch.restore")
	wantTypes := []string{"parley.patch.update", "parley.patch.reorder", "parley.patch.delete", "parley.patch.restore"}
	if len(events) != len(wantTypes) {
		t.Fatalf("the patch events of carry are %v, want one each of %v in order", events, wantTypes)
	}
	for i, e := range events {
		if e.Type != wantTypes[i] || e.ActorType != identity.TypeAPIKey || e.ActorID == nil || *e.ActorID != h.user {
			t.Errorf("event %d is %s by %s %s, want %s by the developer %s", i, e.Type, e.ActorType, deref(e.ActorID), wantTypes[i], h.user)
		}
	}
	var update struct {
		To string `json:"to"`
	}
	if json.Unmarshal(events[0].Metadata, &update) != nil || update.To != "disabled" {
		t.Errorf("parley.patch.update metadata = %s, want to: disabled", events[0].Metadata)
	}
}

// A developer forces a patch branch that diverged between the hub and
// origin back to origin's tip, finds the hub's tip kept as the backup ref,
// and a second reset finds nothing to do.
func TestPatchMaintenance_ResetToOrigin(t *testing.T) {
	parallel(t)
	h, origin, ids := startPatchHub(t, "patch/a")
	a := ids[0]
	copyDir := filepath.Join(h.root, "carry.git")
	hubTip := advance(t, copyDir, "patch/a", "the hub's own")
	originTip := origin.Commit("patch/a", "origin's own")

	reset := h.document(t, "patch", "reset-to-origin", "carry", a)
	if _, isBool := reset["rebuild_triggered"].(bool); reset["action"] != "replaced" || reset["replaced_sha"] != hubTip || !isBool {
		t.Errorf("patch reset-to-origin printed %v, want action replaced, replaced_sha %s and a boolean rebuild_triggered", reset, hubTip)
	}
	if got := gitrepotest.Git(t, copyDir, "rev-parse", "refs/heads/patch/a"); got != originTip {
		t.Errorf("refs/heads/patch/a = %s, want origin's tip %s", got, originTip)
	}
	if got := gitrepotest.Git(t, copyDir, "rev-parse", "refs/parley/replaced/patch/a"); got != hubTip {
		t.Errorf("refs/parley/replaced/patch/a = %s, want the hub's discarded tip %s", got, hubTip)
	}
	if p := h.document(t, "patch", "show", "carry", a); p["replaced_sha"] != hubTip {
		t.Errorf("patch show printed replaced_sha %v, want %s", p["replaced_sha"], hubTip)
	}

	var got []string
	for _, e := range h.patchEvents(t, "carry", "parley.patch.reset", "parley.patch.replace") {
		var m struct{ Action, Trigger string }
		if e.ResourceID != a || json.Unmarshal(e.Metadata, &m) != nil {
			t.Fatalf("%s of the resource %s with metadata %s, want an event of the patch %s", e.Type, e.ResourceID, e.Metadata, a)
		}
		got = append(got, e.Type+" "+m.Action+m.Trigger)
	}
	if want := []string{"parley.patch.reset replaced", "parley.patch.replace reset_to_origin"}; !slices.Equal(got, want) {
		t.Errorf("the reset events of carry are %q, want %q", got, want)
	}

	if again := h.document(t, "patch", "reset-to-origin", "carry", a); again["action"] != "none" {
		t.Errorf("a second patch reset-to-origin printed %v, want action none", again)
	}
}
