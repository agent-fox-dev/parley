package e2e

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// commitIn writes into the repository dir a commit with the message msg
// and the parents given, the first parent first, whose tree is the first
// parent's with the files, at the top level, holding the content given,
// and returns its id.
func commitIn(t *testing.T, dir, msg string, files map[string]string, parents ...string) string {
	t.Helper()
	entries := map[string]string{}
	if len(parents) > 0 {
		for line := range strings.Lines(gitrepotest.Git(t, dir, "ls-tree", parents[0])) {
			meta, name, _ := strings.Cut(strings.TrimSuffix(line, "\n"), "\t")
			entries[name] = meta
		}
	}
	for name, content := range files {
		blob, err := gitrepotest.Run(dir, content, "hash-object", "-w", "--stdin")
		if err != nil {
			t.Fatal(err)
		}
		entries[name] = "100644 blob " + blob
	}
	var tree strings.Builder
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		fmt.Fprintf(&tree, "%s\t%s\n", entries[name], name)
	}
	treeID, err := gitrepotest.Run(dir, tree.String(), "mktree")
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"commit-tree", treeID, "-m", msg}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	return gitrepotest.Git(t, dir, args...)
}

// startForkOfUpstream makes an upstream whose main is the commit made of
// files, and an origin cloned from it whose branch of each entry of patches
// holds one commit on that main, with the entry's message and files; it
// starts a hub that trusts both and creates the carry-patch workspace
// acme-fork from them with settings, waiting until it is ready. It returns
// the hub, the upstream, the origin and the hub's repository of the
// workspace.
func startForkOfUpstream(t *testing.T, smart bool, files map[string]string, settings map[string]any, patches []forkPatch) (*workspaceHub, *gitrepotest.Origin, *gitrepotest.Origin, string) {
	t.Helper()
	up := gitrepotest.NewOrigin(t)
	up.Branch("main", commitIn(t, up.Dir(), "u1", files))
	var origin *gitrepotest.Origin
	if smart {
		origin = gitrepotest.SmartOrigin(t)
	} else {
		origin = gitrepotest.NewOrigin(t)
	}
	gitrepotest.Git(t, up.Dir(), "push", "--quiet", origin.Dir(), "refs/heads/main:refs/heads/main")
	origin.Branch("main", "main")
	for _, p := range patches {
		origin.Branch(p.branch, commitIn(t, origin.Dir(), p.msg, p.files, origin.Head("main")))
	}
	h := startCarryPatchHub(t, origin, up)
	body := map[string]any{"slug": "acme-fork", "git_url": origin.URL(), "workspace_mode": "carry_patch", "upstream_url": up.URL()}
	if settings != nil {
		body["settings"] = settings
	}
	if status, raw := h.Call(t, http.MethodPost, "/api/v1/workspaces", h.key, body); status != http.StatusCreated {
		t.Fatalf("create acme-fork = %d %s, want 201", status, raw)
	}
	h.waitStatus(t, "acme-fork", workspace.StatusReady)
	return h, up, origin, filepath.Join(h.root, "acme-fork.git")
}

// forkPatch is a patch branch startForkOfUpstream puts on origin.
type forkPatch struct {
	branch, msg string
	files       map[string]string
}

// forkCall sends method /api/v1/workspaces/acme-fork+path with the
// developer's key and body, and decodes the answer's JSON object.
func (h *workspaceHub) forkCall(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	status, raw := h.Call(t, method, "/api/v1/workspaces/acme-fork"+path, h.key, body)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	return status, doc
}

// waitForkRebuild reads the rebuild job id of acme-fork until its status is
// status, and fails the test when it is not within a minute.
func (h *workspaceHub) waitForkRebuild(t *testing.T, id, status string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		code, job := h.forkCall(t, http.MethodGet, "/rebuilds/"+id, nil)
		if code == http.StatusOK && job["status"] == status {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("the rebuild job %s is %d %v after a minute, want status %s", id, code, job, status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A developer rebuilds a carry-patch workspace from the shell: the patch
// merged upstream is removed, the other two are replayed on the new
// upstream, and the integration branch reaches origin.
func TestRebuild_PublishesAndPushes(t *testing.T) {
	parallel(t)
	h, up, origin, copyDir := startForkOfUpstream(t, true, map[string]string{"README": "upstream\n"}, nil, []forkPatch{
		{"patch/one", "patch one", map[string]string{"one.txt": "one\n"}},
		{"patch/two", "patch two", map[string]string{"two.txt": "two\n"}},
		{"patch/three", "patch three", map[string]string{"three.txt": "three\n"}},
	})
	upstreamHead := up.Branch("main", commitIn(t, up.Dir(), "u2", map[string]string{"u2.txt": "u2\n"}, up.Head("main")))

	status, raw := h.Call(t, http.MethodPost, "/api/v1/workspaces/acme-fork/patches", h.key, []map[string]any{
		{"branch_name": "patch/one"}, {"branch_name": "patch/two"}, {"branch_name": "patch/three"},
	})
	var added patchItems
	if err := json.Unmarshal(raw, &added); status != http.StatusCreated || err != nil || len(added.Items) != 3 {
		t.Fatalf("register the patches = %d %s, want 201 with three patches", status, raw)
	}
	merged := added.Items[1].ID
	if status, p := h.forkCall(t, http.MethodPatch, "/patches/"+merged, map[string]any{"status": "merged_upstream"}); status != http.StatusOK {
		t.Fatalf("mark patch/two merged upstream = %d %v, want 200", status, p)
	}
	if status, w := h.forkCall(t, http.MethodPatch, "", map[string]any{"settings": map[string]any{"rebuild_push_integration_branch": true}}); status != http.StatusOK {
		t.Fatalf("turn the push of the integration branch on = %d %v, want 200", status, w)
	}

	r := h.run(t, "rebuild", "submit", "acme-fork", "--wait", "--poll-interval", "1s")
	if r.code != 0 {
		t.Fatalf("parley rebuild submit exited %d with stdout %q, stderr %q; want 0", r.code, r.stdout, r.stderr)
	}
	job := r.json(t)
	id, _ := job["id"].(string)
	want := map[string]any{"status": "completed", "patches_applied": 2.0, "patches_skipped": 1.0, "patches_removed": 1.0, "integration_branch_pushed": true}
	for k, v := range want {
		if job[k] != v {
			t.Errorf("the job's %s = %v, want %v", k, job[k], v)
		}
	}

	head, _ := job["integration_head_sha"].(string)
	hubDeploy := gitrepotest.Git(t, copyDir, "rev-parse", "refs/heads/deploy")
	if head == "" || hubDeploy != head || origin.Head("deploy") != head {
		t.Errorf("deploy is %s in the hub and %s on origin, want both the job's integration_head_sha %q", hubDeploy, origin.Head("deploy"), head)
	}
	if job["upstream_head_sha"] != upstreamHead {
		t.Errorf("the job's upstream_head_sha = %v, want the upstream's main %s", job["upstream_head_sha"], upstreamHead)
	}
	if got := gitrepotest.Git(t, copyDir, "log", "--reverse", "--format=%s", upstreamHead+"..refs/heads/deploy"); got != "patch one\npatch three" {
		t.Errorf("the commits of deploy over the upstream head are %q, want those of patches one and three", got)
	}

	status, raw = h.Call(t, http.MethodGet, "/api/v1/workspaces/acme-fork/patches", h.key, nil)
	var list patchItems
	if err := json.Unmarshal(raw, &list); status != http.StatusOK || err != nil || len(list.Items) != 2 ||
		list.Items[0].Position != 1 || list.Items[1].Position != 2 {
		t.Errorf("GET the patches = %d %s, want two patches at positions 1 and 2", status, raw)
	}

	var completes, deletes, followUps int
	for _, e := range h.patchEvents(t, "acme-fork", carrypatch.EventRebuildComplete, carrypatch.EventPatchDelete, carrypatch.EventRebuildFollowUp) {
		switch {
		case e.Type == carrypatch.EventRebuildComplete && e.ResourceID == id:
			completes++
		case e.Type == carrypatch.EventPatchDelete && e.ResourceID == merged:
			deletes++
		case e.Type == carrypatch.EventRebuildFollowUp:
			followUps++
		}
	}
	if completes != 1 || deletes != 1 || followUps != 0 {
		t.Errorf("the event log holds %d parley.rebuild.complete for the job, %d parley.patch.delete for patch/two and %d parley.rebuild.followup; want 1, 1 and 0",
			completes, deletes, followUps)
	}
}

// An agent's rebuild stops at a conflict with upstream; a developer merges
// upstream into the patch branch, and the requeued job learns the
// resolution from that merge and completes.
func TestRebuild_ConflictThenLearnedResolution(t *testing.T) {
	parallel(t)
	h, up, _, copyDir := startForkOfUpstream(t, false, map[string]string{"a.txt": "1\n2\n3\n"},
		map[string]any{"auto_rebuild_after_push": false}, []forkPatch{
			{"feature/a", "feature a", map[string]string{"a.txt": "1\n2\n3 feature\n"}},
		})
	status, p := h.forkCall(t, http.MethodPost, "/patches", map[string]any{"branch_name": "feature/a"})
	pid, _ := p["id"].(string)
	if status != http.StatusCreated || pid == "" {
		t.Fatalf("register feature/a = %d %v, want 201 with a patch", status, p)
	}
	upstreamHead := up.Branch("main", commitIn(t, up.Dir(), "u2", map[string]string{"a.txt": "1\n2\n3 upstream\n"}, up.Head("main")))
	before := gitrepotest.Git(t, copyDir, "rev-parse", "refs/heads/deploy")

	status, job := h.forkCall(t, http.MethodPost, "/rebuild", nil)
	id, _ := job["id"].(string)
	if status != http.StatusAccepted || id == "" {
		t.Fatalf("POST rebuild = %d %v, want 202 with a job", status, job)
	}
	failed := h.waitForkRebuild(t, id, "dead_letter")
	conflict, _ := failed["conflict"].(map[string]any)
	files, _ := conflict["files"].([]any)
	if len(files) == 0 {
		t.Fatalf("the dead-lettered job = %v, want a conflict report with files", failed)
	}
	if file, _ := files[0].(map[string]any); file["path"] != "a.txt" || file["kind"] != "content" {
		t.Errorf("the conflict's first file = %v, want a.txt of kind content", files[0])
	}
	if _, p := h.forkCall(t, http.MethodGet, "/patches/"+pid, nil); p["status"] != "conflict" {
		t.Errorf("the patch after the conflict = %v, want status conflict", p)
	}
	if got := gitrepotest.Git(t, copyDir, "rev-parse", "refs/heads/deploy"); got != before {
		t.Errorf("deploy after the conflict = %s, want %s as before", got, before)
	}

	// The developer's merge of upstream into feature/a, as a push would
	// leave it in the hub's repository.
	const resolved = "1\n2\n3 feature and upstream\n"
	tip := gitrepotest.Git(t, copyDir, "rev-parse", "refs/heads/feature/a")
	merge := commitIn(t, copyDir, "merge upstream into feature/a", map[string]string{"a.txt": resolved}, tip, upstreamHead)
	gitrepotest.Git(t, copyDir, "update-ref", "refs/heads/feature/a", merge, tip)
	if status, job := h.forkCall(t, http.MethodPost, "/rebuilds/"+id+"/requeue", nil); status != http.StatusOK {
		t.Fatalf("requeue = %d %v, want 200", status, job)
	}

	done := h.waitForkRebuild(t, id, "completed")
	if done["patches_applied"] != 1.0 {
		t.Errorf("the completed job = %v, want patches_applied 1", done)
	}
	if _, p := h.forkCall(t, http.MethodGet, "/patches/"+pid, nil); p["status"] != "active" || fmt.Sprint(p["conflict_files"]) != "[]" {
		t.Errorf("the patch after the requeued rebuild = %v, want active with conflict_files []", p)
	}
	if got := string(gitrepotest.Blob(t, copyDir, "refs/heads/deploy:a.txt")); got != resolved {
		t.Errorf("a.txt on deploy = %q, want the developer's resolution %q", got, resolved)
	}

	var got []string
	for _, e := range h.patchEvents(t, "acme-fork", carrypatch.EventRebuildFail, carrypatch.EventRebuildComplete) {
		if e.ResourceID != id {
			continue
		}
		var m struct {
			Reason    string `json:"reason"`
			WillRetry bool   `json:"will_retry"`
		}
		if err := json.Unmarshal(e.Metadata, &m); err != nil {
			t.Fatalf("the metadata of %s = %s: %v", e.Type, e.Metadata, err)
		}
		if e.Type == carrypatch.EventRebuildFail {
			got = append(got, fmt.Sprintf("%s %s %v", e.Type, m.Reason, m.WillRetry))
		} else {
			got = append(got, e.Type)
		}
	}
	if want := []string{carrypatch.EventRebuildFail + " conflict false", carrypatch.EventRebuildComplete}; !slices.Equal(got, want) {
		t.Errorf("the rebuild events of the job are %q, want %q", got, want)
	}
}
