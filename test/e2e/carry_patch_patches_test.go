package e2e

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// A developer registers a branch the hub's copy holds only as origin's
// tracking ref from the shell, an agent registers two more in one batch,
// and the developer reads the order and one patch back.
func TestCarryPatch_RegisterAndReadPatches(t *testing.T) {
	parallel(t)
	origin := gitrepotest.NewOrigin(t)
	origin.Commit("main", "o1")
	for _, branch := range []string{"fix-dial", "add-metrics", "vendor-tls"} {
		origin.Branch(branch, "main")
		origin.Commit(branch, branch)
	}
	up := gitrepotest.NewOrigin(t)
	up.Commit("main", "u1")
	h := startCarryPatchHub(t, origin, up)
	status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces", h.key, map[string]any{
		"slug": "variant", "git_url": origin.URL(), "workspace_mode": "carry_patch", "upstream_url": up.URL(),
	})
	if status != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", status, body)
	}
	h.waitStatus(t, "variant", workspace.StatusReady)
	copyDir := filepath.Join(h.root, "variant.git")
	gitrepotest.Git(t, copyDir, "update-ref", "-d", "refs/heads/fix-dial")

	added := h.run(t, "patch", "add", "variant", "--branch", "fix-dial", "--description", "dial timeout")
	if p := added.json(t); added.code != 0 || p["branch_name"] != "fix-dial" || p["position"] != 1.0 || p["status"] != "active" {
		t.Fatalf("patch add exited %d with %v (stderr %q), want 0 and fix-dial at position 1, active", added.code, p, added.stderr)
	}
	if got := gitrepotest.Git(t, copyDir, "rev-parse", "refs/heads/fix-dial"); got != origin.Head("fix-dial") {
		t.Errorf("refs/heads/fix-dial = %s, want origin's %s", got, origin.Head("fix-dial"))
	}

	agent := h.IssuePAT(t, identity.PATSpec{UserID: h.user, Permissions: []string{"patches:read", "patches:write"}})
	status, body = h.Call(t, http.MethodPost, "/api/v1/workspaces/variant/patches", agent, []map[string]any{
		{"branch_name": "add-metrics"}, {"branch_name": "vendor-tls", "position": 1},
	})
	var batch patchItems
	if err := json.Unmarshal(body, &batch); status != http.StatusCreated || err != nil || len(batch.Items) != 2 ||
		batch.Items[0].Position != 3 || batch.Items[1].Position != 1 {
		t.Fatalf("batch = %d %s, want 201 with add-metrics at 3 and vendor-tls at 1", status, body)
	}

	listed := h.run(t, "patch", "list", "variant")
	var list patchItems
	if err := json.Unmarshal([]byte(listed.stdout), &list); listed.code != 0 || err != nil {
		t.Fatalf("patch list exited %d with %q (stderr %q), want 0 and a page", listed.code, listed.stdout, listed.stderr)
	}
	var order []string
	for i, p := range list.Items {
		order = append(order, p.BranchName)
		if p.Position != i+1 {
			t.Errorf("listed %s at position %d, want %d", p.BranchName, p.Position, i+1)
		}
	}
	if len(order) != 3 || order[0] != "vendor-tls" || order[1] != "fix-dial" || order[2] != "add-metrics" {
		t.Fatalf("patch list = %v, want vendor-tls, fix-dial, add-metrics", order)
	}

	shown := h.run(t, "patch", "show", "variant", list.Items[1].ID)
	if p := shown.json(t); shown.code != 0 || p["id"] != list.Items[1].ID || p["branch_name"] != "fix-dial" {
		t.Errorf("patch show exited %d with %v, want 0 and the patch %s of fix-dial", shown.code, p, list.Items[1].ID)
	}

	events := h.events(t, "parley.patch.create", "variant")
	var first struct {
		BranchResolution string `json:"branch_resolution"`
	}
	if len(events) != 3 || json.Unmarshal([]byte(events[0]), &first) != nil || first.BranchResolution != "origin_tracking" {
		t.Errorf("parley.patch.create events of variant = %v, want 3, the first with branch_resolution origin_tracking", events)
	}
}

// patchItems is a page of patches, or a batch's answer, as the hub serves it.
type patchItems struct {
	Items []struct {
		ID         string `json:"id"`
		BranchName string `json:"branch_name"`
		Position   int    `json:"position"`
	} `json:"items"`
}
