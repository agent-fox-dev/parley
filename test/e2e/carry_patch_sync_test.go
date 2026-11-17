package e2e

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// startSyncedFork starts acme-fork from startForkOfUpstream with settings
// and the patch branches patch/a and patch/b on origin, registers both as
// patches from the shell, then syncs it once and waits until the rebuild
// that first sync queued has completed. It returns the hub, the upstream,
// the origin, the hub's repository of the workspace and the patches' ids by
// branch.
func startSyncedFork(t *testing.T, settings map[string]any) (*workspaceHub, *gitrepotest.Origin, *gitrepotest.Origin, string, map[string]string) {
	t.Helper()
	h, up, origin, copyDir := startForkOfUpstream(t, false, map[string]string{"README": "upstream\n"}, settings, []forkPatch{
		{"patch/a", "patch a", map[string]string{"a.txt": "a\n"}},
		{"patch/b", "patch b", map[string]string{"b.txt": "b\n"}},
	})
	ids := map[string]string{}
	for _, branch := range []string{"patch/a", "patch/b"} {
		ids[branch], _ = h.document(t, "patch", "add", "acme-fork", "--branch", branch)["id"].(string)
	}
	h.syncFork(t)
	return h, up, origin, copyDir, ids
}

// syncFork sends POST /api/v1/workspaces/acme-fork/sync, fails the test
// unless it completes, waits until the rebuild it queued, when it queued
// one, has completed, and returns the answer.
func (h *workspaceHub) syncFork(t *testing.T) map[string]any {
	t.Helper()
	status, answer := h.forkCall(t, http.MethodPost, "/sync", nil)
	if status != http.StatusOK {
		t.Fatalf("sync acme-fork = %d %v, want 200", status, answer)
	}
	if id, _ := answer["rebuild_job_id"].(string); id != "" {
		h.waitForkRebuild(t, id, "completed")
	}
	return answer
}

// A developer syncs from the shell after upstream merged one of the two
// patches: the sync finds it merged and queues a rebuild, which the command
// waits for, and the integration branch is rebuilt on the new upstream
// with the other patch alone.
func TestCarryPatchSync_MergedPatchIsRebuiltAway(t *testing.T) {
	parallel(t)
	h, up, origin, copyDir, ids := startSyncedFork(t, nil)
	gitrepotest.Git(t, origin.Dir(), "push", "--quiet", up.Dir(), "refs/heads/patch/a:refs/heads/main")
	upstreamHead := up.Branch("main", gitrepotest.Commit(t, up.Dir(), "u2", map[string]string{"u2.txt": "u2\n"}, up.Head("main")))

	r := h.run(t, "workspace", "sync", "acme-fork", "--wait", "--poll-interval", "1s")
	if r.code != 0 {
		t.Fatalf("parley workspace sync --wait exited %d with stdout %q, stderr %q; want 0", r.code, r.stdout, r.stderr)
	}
	doc := r.json(t)
	rebuild, _ := doc["rebuild"].(map[string]any)
	if doc["upstream_advanced"] != true || !slices.Equal(asStrings(doc["patches_merged"]), []string{"patch/a"}) ||
		doc["rebuild_triggered"] != true || rebuild["status"] != "completed" {
		t.Errorf("the command printed upstream_advanced %v, patches_merged %v, rebuild_triggered %v and a rebuild %v; want true, [patch/a], true and completed",
			doc["upstream_advanced"], doc["patches_merged"], doc["rebuild_triggered"], rebuild["status"])
	}

	if _, w := h.forkCall(t, http.MethodGet, "", nil); w["upstream_head_sha"] != upstreamHead {
		t.Errorf("upstream_head_sha = %v, want upstream's main %s", w["upstream_head_sha"], upstreamHead)
	}
	if _, err := gitrepotest.Run(copyDir, "", "merge-base", "--is-ancestor", upstreamHead, "refs/heads/deploy"); err != nil {
		t.Errorf("upstream's main %s is not an ancestor of deploy: %v", upstreamHead, err)
	}
	if got := string(gitrepotest.Blob(t, copyDir, "refs/heads/deploy:b.txt")); got != "b\n" {
		t.Errorf("b.txt on deploy = %q, want patch/b's %q", got, "b\n")
	}
	if list := h.forkPatches(t); len(list.Items) != 1 || list.Items[0].BranchName != "patch/b" {
		t.Errorf("the patch list = %+v, want patch/b alone", list.Items)
	}

	var merged, synced int
	for _, e := range h.patchEvents(t, "acme-fork", carrypatch.EventPatchUpdate, syncEventType) {
		var m struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(e.Metadata, &m)
		switch {
		case e.Type == carrypatch.EventPatchUpdate && e.ResourceID == ids["patch/a"] && m.Reason == "merged_upstream":
			merged++
		case e.Type == syncEventType:
			synced++
		}
	}
	if merged != 1 || synced != 2 {
		t.Errorf("the event log holds %d parley.patch.update of patch/a for merged_upstream and %d parley.workspace.sync; want 1 and 2", merged, synced)
	}
}

// An agent with a workspaces:write token syncs a workspace whose patch
// branches follow origin: one is fast-forwarded, the other replaced with a
// backup of the hub's tip. Under policy report a developer's sync then
// leaves a rewritten branch alone and the command fails on the divergence.
func TestCarryPatchSync_OriginRefresh(t *testing.T) {
	parallel(t)
	h, _, origin, copyDir, ids := startSyncedFork(t, map[string]any{"patch_branch_source": "origin"})
	before := len(h.patchEvents(t, "acme-fork", carrypatch.EventPatchSync))
	old := gitrepotest.Git(t, copyDir, "rev-parse", "refs/heads/patch/b")
	origin.Branch("patch/a", gitrepotest.Commit(t, origin.Dir(), "patch a, more", map[string]string{"a.txt": "a, more\n"}, origin.Head("patch/a")))
	origin.Branch("patch/b", gitrepotest.Commit(t, origin.Dir(), "patch b, rewritten", map[string]string{"b.txt": "rewritten\n"}, origin.Head("main")))

	agent := h.IssuePAT(t, identity.PATSpec{UserID: h.user, Permissions: []string{"workspaces:write", "patches:read"}})
	status, raw := h.Call(t, http.MethodPost, "/api/v1/workspaces/acme-fork/sync", agent, nil)
	var answer map[string]any
	if err := json.Unmarshal(raw, &answer); err != nil || status != http.StatusOK {
		t.Fatalf("the agent's sync = %d %s, want 200", status, raw)
	}
	if id, _ := answer["rebuild_job_id"].(string); id != "" {
		h.waitForkRebuild(t, id, "completed")
	}
	actions := map[string]any{}
	entries, _ := answer["patches_synced"].([]any)
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		actions[str(entry, "branch_name")] = entry["action"]
	}
	if actions["patch/a"] != "fast_forwarded" || actions["patch/b"] != "replaced" || len(actions) != 2 {
		t.Errorf("patches_synced actions = %v, want patch/a fast_forwarded and patch/b replaced", actions)
	}
	for _, branch := range []string{"patch/a", "patch/b"} {
		if got := gitrepotest.Git(t, copyDir, "rev-parse", "refs/heads/"+branch); got != origin.Head(branch) {
			t.Errorf("the hub's %s = %s, want origin's %s", branch, got, origin.Head(branch))
		}
	}
	if got := gitrepotest.Git(t, copyDir, "rev-parse", carrypatch.ReplacedRefPrefix+"patch/b"); got != old {
		t.Errorf("%spatch/b = %s, want the hub's former tip %s", carrypatch.ReplacedRefPrefix, got, old)
	}
	_, raw = h.Call(t, http.MethodGet, "/api/v1/workspaces/acme-fork/patches/"+ids["patch/b"], agent, nil)
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	if p["origin_sync_state"] != "in_sync" || p["replaced_sha"] != old {
		t.Errorf("GET patch/b = origin_sync_state %v, replaced_sha %v; want in_sync and %s", p["origin_sync_state"], p["replaced_sha"], old)
	}
	if n := len(h.patchEvents(t, "acme-fork", carrypatch.EventPatchSync)) - before; n != 2 {
		t.Errorf("%d parley.patch.sync events from the sync, want 2", n)
	}
	var replaces []string
	for _, e := range h.patchEvents(t, "acme-fork", carrypatch.EventPatchReplace) {
		var m struct {
			Trigger string `json:"trigger"`
		}
		_ = json.Unmarshal(e.Metadata, &m)
		replaces = append(replaces, m.Trigger)
	}
	if !slices.Equal(replaces, []string{"sync"}) {
		t.Errorf("parley.patch.replace events with the triggers %q, want one with trigger sync", replaces)
	}

	if status, w := h.forkCall(t, http.MethodPatch, "", map[string]any{"settings": map[string]any{"patch_divergence_policy": "report"}}); status != http.StatusOK {
		t.Fatalf("set patch_divergence_policy report = %d %v, want 200", status, w)
	}
	origin.Branch("patch/b", gitrepotest.Commit(t, origin.Dir(), "patch b, rewritten again", map[string]string{"b.txt": "again\n"}, origin.Head("main")))
	kept := gitrepotest.Git(t, copyDir, "rev-parse", "refs/heads/patch/b")

	r := h.run(t, "workspace", "sync", "acme-fork", "--fail-on-diverged")
	if r.code != 3 {
		t.Fatalf("parley workspace sync --fail-on-diverged exited %d with stdout %q, stderr %q; want 3", r.code, r.stdout, r.stderr)
	}
	if got := asStrings(r.json(t)["patches_diverged"]); !slices.Equal(got, []string{"patch/b"}) {
		t.Errorf("the command printed patches_diverged %q, want [patch/b]", got)
	}
	if !strings.Contains(r.stderr, "patch/b") {
		t.Errorf("stderr = %q, want it to name patch/b", r.stderr)
	}
	if got := gitrepotest.Git(t, copyDir, "rev-parse", "refs/heads/patch/b"); got != kept {
		t.Errorf("the hub's patch/b = %s, want %s unchanged", got, kept)
	}
}
