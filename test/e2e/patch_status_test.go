package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/cli"
)

// An org member with read access reads the summary of a carry-patch
// workspace from the shell after a rebuild, while origin and upstream are
// down: the hub answers from what it holds.
func TestPatchStatus_AMemberReadsTheSummaryWithTheRemotesDown(t *testing.T) {
	parallel(t)
	h, up, origin, _ := startForkOfUpstream(t, false, map[string]string{"README": "upstream\n"},
		map[string]any{"auto_rebuild_after_push": false}, []forkPatch{
			{"patch/one", "patch one", map[string]string{"one.txt": "one\n"}},
			{"patch/two", "patch two", map[string]string{"two.txt": "two\n"}},
		})
	if status, raw := h.Call(t, http.MethodPost, "/api/v1/workspaces/acme-fork/patches", h.key, []map[string]any{
		{"branch_name": "patch/one"}, {"branch_name": "patch/two"},
	}); status != http.StatusCreated {
		t.Fatalf("register the patches = %d %s, want 201", status, raw)
	}
	if r := h.run(t, "rebuild", "submit", "acme-fork", "--wait", "--poll-interval", "1s"); r.code != 0 || r.json(t)["status"] != "completed" {
		t.Fatalf("parley rebuild submit exited %d with stdout %q, stderr %q; want a completed job", r.code, r.stdout, r.stderr)
	}
	acme := h.CreateOrg(t, "acme")
	h.AddMember(t, acme, h.user)
	member := h.CreateUser(t, "mem")
	h.AddMember(t, acme, member)
	memberKey := h.IssueKey(t, member)
	if status, body := h.Call(t, http.MethodPatch, "/api/v1/workspaces/acme-fork", h.key, map[string]any{"org_id": "acme"}); status != http.StatusOK {
		t.Fatalf("move acme-fork to acme = %d %s, want 200", status, body)
	}
	origin.Close()
	up.Close()

	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), []string{"--endpoint-url", h.URL(""), "--api-key", memberKey, "workspace", "patch-status", "acme-fork"}, &stdout, &stderr)
	var doc struct {
		Summary map[string]any `json:"summary"`
		Last    struct {
			ID, Status string
		} `json:"last_rebuild"`
		LastCompleted struct {
			ID, Status string
		} `json:"last_completed_rebuild"`
		IntegrationHead string           `json:"integration_head_sha"`
		Patches         []map[string]any `json:"patches"`
	}
	if code != 0 || json.Unmarshal(stdout.Bytes(), &doc) != nil {
		t.Fatalf("parley workspace patch-status as the member exited %d with stdout %q, stderr %q; want 0 and the summary", code, stdout.String(), stderr.String())
	}
	if doc.Summary["total_patches"] != 2.0 || doc.Summary["active"] != 2.0 {
		t.Errorf("the summary = %v, want total_patches 2 and active 2", doc.Summary)
	}
	if doc.Last.Status != "completed" || doc.Last.ID == "" || doc.Last != doc.LastCompleted {
		t.Errorf("last_rebuild = %+v and last_completed_rebuild = %+v, want the same completed job", doc.Last, doc.LastCompleted)
	}
	if len(doc.Patches) != 2 {
		t.Errorf("the summary lists %d patches, want 2", len(doc.Patches))
	}
	for _, p := range doc.Patches {
		if p["last_rebuild_result"] != "success" {
			t.Errorf("the patch %v has last_rebuild_result %v, want success", p["branch_name"], p["last_rebuild_result"])
		}
	}
	refs, err := newGitClient(t, h.URL(""), h.key).git("", "ls-remote", h.URL("/git/acme/acme-fork.git"), "refs/heads/deploy")
	if err != nil {
		t.Fatalf("git ls-remote of acme-fork: %v\n%s", err, refs)
	}
	if head, _, _ := strings.Cut(refs, "\t"); doc.IntegrationHead == "" || head != doc.IntegrationHead {
		t.Errorf("integration_head_sha = %q, want the head of deploy a git client reads from the hub, %q", doc.IntegrationHead, head)
	}
}
