package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// forkPatchList is GET /api/v1/workspaces/acme-fork/patches as the test
// reads it.
type forkPatchList struct {
	Items []struct {
		BranchName    string   `json:"branch_name"`
		Position      int      `json:"position"`
		Status        string   `json:"status"`
		ConflictFiles []string `json:"conflict_files"`
	} `json:"items"`
}

// forkPatches reads the patch list of acme-fork.
func (h *workspaceHub) forkPatches(t *testing.T) forkPatchList {
	t.Helper()
	status, raw := h.Call(t, http.MethodGet, "/api/v1/workspaces/acme-fork/patches", h.key, nil)
	var list forkPatchList
	if status != http.StatusOK || json.Unmarshal(raw, &list) != nil {
		t.Fatalf("GET the patches = %d %s, want 200 with a list", status, raw)
	}
	return list
}

// forkRebuild registers branch as a patch of acme-fork, submits a rebuild,
// waits until it is completed and returns the job.
func (h *workspaceHub) forkRebuild(t *testing.T, branch string) map[string]any {
	t.Helper()
	if status, p := h.forkCall(t, http.MethodPost, "/patches", map[string]any{"branch_name": branch}); status != http.StatusCreated {
		t.Fatalf("register %s = %d %v, want 201", branch, status, p)
	}
	status, job := h.forkCall(t, http.MethodPost, "/rebuild", nil)
	id, _ := job["id"].(string)
	if status != http.StatusAccepted || id == "" {
		t.Fatalf("POST rebuild = %d %v, want 202 with a job", status, job)
	}
	return h.waitForkRebuild(t, id, "completed")
}

// An operator rolls the integration branch back from the shell to the head
// it had before the rebuild that added a second patch; the patches and the
// job stay as they were, and the event log records the rollback.
func TestRebuildRollback_FromTheShell(t *testing.T) {
	parallel(t)
	h, _, _, copyDir := startForkOfUpstream(t, false, map[string]string{"README": "upstream\n"}, nil, []forkPatch{
		{"patch/a", "patch a", map[string]string{"a.txt": "a\n"}},
		{"patch/b", "patch b", map[string]string{"b.txt": "b\n"}},
	})
	first := h.forkRebuild(t, "patch/a")
	second := h.forkRebuild(t, "patch/b")
	h1, _ := first["integration_head_sha"].(string)
	h2, _ := second["integration_head_sha"].(string)
	id, _ := second["id"].(string)
	if h1 == "" || h2 == "" || h1 == h2 || second["previous_integration_head_sha"] != h1 {
		t.Fatalf("the rebuilds published %q then %q with previous head %v, want two heads, the second's previous the first", h1, h2, second["previous_integration_head_sha"])
	}

	got := h.document(t, "rebuild", "rollback", "acme-fork", id)

	if want := map[string]any{"rolled_back_to": h1, "previous_head_sha": h2}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("parley rebuild rollback printed %v, want %v", got, want)
	}
	if deploy := gitrepotest.Git(t, copyDir, "rev-parse", "refs/heads/deploy"); deploy != h1 {
		t.Errorf("deploy in the hub's repository = %s, want the first rebuild's head %s", deploy, h1)
	}
	list := h.forkPatches(t)
	var patches []string
	for _, p := range list.Items {
		patches = append(patches, fmt.Sprintf("%s %d %s", p.BranchName, p.Position, p.Status))
	}
	if want := []string{"patch/a 1 active", "patch/b 2 active"}; !slices.Equal(patches, want) {
		t.Errorf("the patches after the rollback are %q, want %q", patches, want)
	}
	if status, job := h.forkCall(t, http.MethodGet, "/rebuilds/"+id, nil); status != http.StatusOK || job["status"] != "completed" || job["integration_head_sha"] != h2 {
		t.Errorf("the second job after the rollback = %d %v, want completed with integration_head_sha %s", status, job, h2)
	}
	events := h.patchEvents(t, "acme-fork", carrypatch.EventRebuildRollback)
	if len(events) != 1 || events[0].ResourceID != id {
		t.Fatalf("the event log holds %+v, want one parley.rebuild.rollback of the second job %s", events, id)
	}
	var m struct {
		RolledBackTo    string `json:"rolled_back_to"`
		PreviousHeadSHA string `json:"previous_head_sha"`
	}
	if err := json.Unmarshal(events[0].Metadata, &m); err != nil || m.RolledBackTo != h1 || m.PreviousHeadSHA != h2 {
		t.Errorf("the rollback event's metadata = %s, want rolled_back_to %s and previous_head_sha %s", events[0].Metadata, h1, h2)
	}
}

// A developer previews a rebuild from the shell after upstream changed the
// line the second patch changes: the preview names that patch, exits 3
// with --fail-on-conflict and 0 without it, and changes nothing, though the
// upstream is gone.
func TestRebuildPreview_FromTheShell(t *testing.T) {
	parallel(t)
	up := gitrepotest.NewOrigin(t)
	u1 := up.Branch("main", gitrepotest.Commit(t, up.Dir(), "u1", map[string]string{"f.txt": "one\ntwo\n"}))
	origin := gitrepotest.NewOrigin(t)
	gitrepotest.Git(t, up.Dir(), "push", "--quiet", origin.Dir(), "refs/heads/main:refs/heads/main")
	origin.Branch("main", "main")
	origin.Branch("patch/a", gitrepotest.Commit(t, origin.Dir(), "patch a", map[string]string{"a.txt": "a\n"}, u1))
	origin.Branch("patch/b", gitrepotest.Commit(t, origin.Dir(), "patch b", map[string]string{"f.txt": "one by patch b\ntwo\n"}, u1))
	upstreamHead := up.Branch("main", gitrepotest.Commit(t, up.Dir(), "u2", map[string]string{"f.txt": "one upstream\ntwo\n"}, u1))
	h := startCarryPatchHub(t, origin, up)
	body := map[string]any{"slug": "acme-fork", "git_url": origin.URL(), "workspace_mode": "carry_patch", "upstream_url": up.URL()}
	if status, raw := h.Call(t, http.MethodPost, "/api/v1/workspaces", h.key, body); status != http.StatusCreated {
		t.Fatalf("create acme-fork = %d %s, want 201", status, raw)
	}
	h.waitStatus(t, "acme-fork", workspace.StatusReady)
	if status, raw := h.Call(t, http.MethodPost, "/api/v1/workspaces/acme-fork/patches", h.key, []map[string]any{
		{"branch_name": "patch/a"}, {"branch_name": "patch/b"},
	}); status != http.StatusCreated {
		t.Fatalf("register the patches = %d %s, want 201", status, raw)
	}
	copyDir := filepath.Join(h.root, "acme-fork.git")
	refs := gitrepotest.Git(t, copyDir, "for-each-ref")
	up.Close()

	failing := h.run(t, "rebuild", "preview", "acme-fork", "--fail-on-conflict")
	if failing.code != 3 {
		t.Errorf("parley rebuild preview --fail-on-conflict exited %d, want 3 (stderr %q)", failing.code, failing.stderr)
	}
	preview := failing.json(t)
	if preview["upstream_head_sha"] != upstreamHead {
		t.Errorf("upstream_head_sha = %v, want upstream's main as fetched at setup %s", preview["upstream_head_sha"], upstreamHead)
	}
	results, _ := preview["patch_results"].([]any)
	want := []string{"patch/a would_succeed", "patch/b would_conflict"}
	if got := previewStatuses(results); !slices.Equal(got, want) {
		t.Fatalf("the preview's results are %q, want %q", got, want)
	}
	b, _ := results[1].(map[string]any)
	conflict, _ := b["conflict"].(map[string]any)
	if fmt.Sprint(b["conflict_files"]) != "[f.txt]" || conflict == nil || conflict["target_sha"] == preview["upstream_head_sha"] {
		t.Errorf("patch/b's result = %v, want conflict_files [f.txt] and a conflict whose target is the predicted state, not the upstream base", b)
	}
	if failing.errorType(t) != "condition_not_met" || !strings.Contains(failing.stderr, "patch/b") || strings.Contains(failing.stderr, "patch/a") {
		t.Errorf("stderr = %q, want condition_not_met naming patch/b alone", failing.stderr)
	}

	plain := h.run(t, "rebuild", "preview", "acme-fork")
	if plain.code != 0 || plain.stderr != "" {
		t.Errorf("parley rebuild preview exited %d with stderr %q, want 0 and nothing", plain.code, plain.stderr)
	}
	again, _ := plain.json(t)["patch_results"].([]any)
	if got := previewStatuses(again); !slices.Equal(got, want) {
		t.Errorf("the second preview's results are %q, want %q", got, want)
	}

	if got := gitrepotest.Git(t, copyDir, "for-each-ref"); got != refs {
		t.Errorf("the refs after the previews are\n%s\nwant them unchanged\n%s", got, refs)
	}
	for _, p := range h.forkPatches(t).Items {
		if p.Status != "active" || len(p.ConflictFiles) != 0 {
			t.Errorf("%s after the previews is %s with conflict_files %v, want active and none", p.BranchName, p.Status, p.ConflictFiles)
		}
	}
	status, raw := h.Call(t, http.MethodGet, "/api/v1/workspaces/acme-fork/rebuilds", h.key, nil)
	var jobs struct {
		Items []json.RawMessage `json:"items"`
	}
	if status != http.StatusOK || json.Unmarshal(raw, &jobs) != nil || len(jobs.Items) != 0 {
		t.Errorf("GET the rebuilds = %d %s, want 200 and no job", status, raw)
	}
}

// previewStatuses are the branch name and status of each of a preview's
// patch_results.
func previewStatuses(results []any) []string {
	var got []string
	for _, r := range results {
		p, _ := r.(map[string]any)
		got = append(got, fmt.Sprintf("%v %v", p["branch_name"], p["status"]))
	}
	return got
}
