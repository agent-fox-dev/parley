package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// startForkOfUpstream makes an upstream whose main is the commit made of
// files, and an origin cloned from it whose branch of each entry of patches
// holds one commit on that main, with the entry's message and files; it
// starts a hub that trusts both and creates the carry-patch workspace
// acme-fork from them with settings, waiting until it is ready. It returns
// the hub, the upstream, the origin and the hub's repository of the
// workspace.
func startForkOfUpstream(t *testing.T, smart bool, files map[string]string, settings map[string]any, patches []forkPatch) (*workspaceHub, *gitrepotest.Origin, *gitrepotest.Origin, string) {
	t.Helper()
	return startForkOfUpstreamOn(t, hubtest.Config{}, smart, files, settings, patches)
}

// startForkOfUpstreamOn is startForkOfUpstream on a hub started with what
// cfg sets of its clock, composition and maintenance interval.
func startForkOfUpstreamOn(t *testing.T, cfg hubtest.Config, smart bool, files map[string]string, settings map[string]any, patches []forkPatch) (*workspaceHub, *gitrepotest.Origin, *gitrepotest.Origin, string) {
	t.Helper()
	up := gitrepotest.NewOrigin(t)
	up.Branch("main", gitrepotest.Commit(t, up.Dir(), "u1", files))
	var origin *gitrepotest.Origin
	if smart {
		origin = gitrepotest.SmartOrigin(t)
	} else {
		origin = gitrepotest.NewOrigin(t)
	}
	gitrepotest.Git(t, up.Dir(), "push", "--quiet", origin.Dir(), "refs/heads/main:refs/heads/main")
	origin.Branch("main", "main")
	for _, p := range patches {
		origin.Branch(p.branch, gitrepotest.Commit(t, origin.Dir(), p.msg, p.files, origin.Head("main")))
	}
	h := startCarryPatchHubConfig(t, origin, up, cfg)
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
	upstreamHead := up.Branch("main", gitrepotest.Commit(t, up.Dir(), "u2", map[string]string{"u2.txt": "u2\n"}, up.Head("main")))

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
	upstreamHead := up.Branch("main", gitrepotest.Commit(t, up.Dir(), "u2", map[string]string{"a.txt": "1\n2\n3 upstream\n"}, up.Head("main")))
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
	merge := gitrepotest.Commit(t, copyDir, "merge upstream into feature/a", map[string]string{"a.txt": resolved}, tip, upstreamHead)
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

	// The developer reads what the hub learned from the shell, and takes it
	// back.
	h.forgetTheLearnedResolution(t, copyDir)
}

// forgetTheLearnedResolution lists acme-fork's remembered resolutions from
// the shell, which hold one resolved entry for a.txt, forgets the path, and
// checks that the list, the store and the copy's rr-cache hold none.
func (h *workspaceHub) forgetTheLearnedResolution(t *testing.T, copyDir string) {
	t.Helper()
	listed := h.run(t, "rerere", "list", "acme-fork")
	items, _ := listed.json(t)["items"].([]any)
	entry, _ := firstOf(items).(map[string]any)
	if listed.code != 0 || len(items) != 1 || entry["path"] != "a.txt" || entry["resolved"] != true {
		t.Fatalf("parley rerere list exited %d with %q, want one resolved entry for a.txt", listed.code, listed.stdout)
	}
	forgot := h.run(t, "rerere", "forget", "acme-fork", "a.txt")
	if doc := forgot.json(t); forgot.code != 0 || doc["status"] != "forgotten" || doc["pathspec"] != "a.txt" || len(doc) != 2 {
		t.Errorf("parley rerere forget exited %d with %q, want {status: forgotten, pathspec: a.txt}", forgot.code, forgot.stdout)
	}
	listed = h.run(t, "rerere", "list", "acme-fork")
	if items, ok := listed.json(t)["items"].([]any); listed.code != 0 || !ok || len(items) != 0 {
		t.Errorf("parley rerere list after the forget exited %d with %q, want items []", listed.code, listed.stdout)
	}
	if n, err := carrypatch.CountRerereEntries(context.Background(), h.Store.Main(), "acme-fork"); err != nil || n != 0 {
		t.Errorf("the store holds %d remembered resolutions of acme-fork (%v), want none", n, err)
	}
	if cache, err := os.ReadDir(filepath.Join(copyDir, "rr-cache")); err != nil && !os.IsNotExist(err) || len(cache) != 0 {
		t.Errorf("the copy's rr-cache holds %v (%v), want no entry", cache, err)
	}
}

// firstOf is the first element of items, nil when there is none.
func firstOf(items []any) any {
	if len(items) == 0 {
		return nil
	}
	return items[0]
}

// A rebuild is running when the operator stops the hub, and does not end
// within the job queue's grace, since upstream holds its fetch; the hub
// started again on the same data discards what the stop left in the copy,
// runs the job from the start, and publishes the integration branch once.
func TestRebuild_RunsAgainFromTheStartAfterAStopCutItOff(t *testing.T) {
	parallel(t)
	clock := &fakeNow{now: time.Now().UTC()}
	jobsClock := hubtest.NewJobClock(clock.Now)
	h, up, _, copyDir := startForkOfUpstreamOn(t, hubtest.Config{
		Now: jobsClock.Now,
		Compose: func(c *hub.Composition) {
			c.AdjustJobs(func(d *jobs.Deps) { d.Clock = jobsClock })
		},
	}, false, map[string]string{"README": "upstream\n"}, map[string]any{"auto_rebuild_after_push": false}, []forkPatch{
		{"patch/one", "patch one", map[string]string{"one.txt": "one\n"}},
		{"patch/two", "patch two", map[string]string{"two.txt": "two\n"}},
	})
	if status, raw := h.Call(t, http.MethodPost, "/api/v1/workspaces/acme-fork/patches", h.key, []map[string]any{
		{"branch_name": "patch/one"}, {"branch_name": "patch/two"},
	}); status != http.StatusCreated {
		t.Fatalf("register the patches = %d %s, want 201", status, raw)
	}
	before := gitrepotest.Git(t, copyDir, "rev-parse", "refs/heads/deploy")

	up.Block()
	status, job := h.forkCall(t, http.MethodPost, "/rebuild", nil)
	id, _ := job["id"].(string)
	if status != http.StatusAccepted || id == "" {
		t.Fatalf("POST rebuild = %d %v, want 202 with a job", status, job)
	}
	h.waitForkRebuild(t, id, "running")
	deadline := time.Now().Add(10 * time.Second)
	for up.Requests() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the rebuild did not reach upstream within 10 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// What a stop that killed the hub mid-build leaves: a scratch working
	// tree and its registration in the copy.
	gitrepotest.Git(t, copyDir, "worktree", "add", "--quiet", "--detach", filepath.Join(copyDir, "parley-rebuild", "left-by-the-stop"), before)

	// The stop waits for the attempt for the queue's grace, on its clock.
	go func() {
		select {
		case <-jobsClock.Armed(jobs.ShutdownGrace):
			jobsClock.Advance(jobs.ShutdownGrace)
		case <-time.After(10 * time.Second):
		}
	}()
	h.Restart(t)
	if _, j := h.forkCall(t, http.MethodGet, "/rebuilds/"+id, nil); j["status"] == "completed" || j["status"] == "dead_letter" || j["status"] == "cancelled" {
		t.Errorf("the job after the stop = %v, want it not final while upstream holds the fetch", j)
	}
	if got := gitrepotest.Git(t, copyDir, "rev-parse", "refs/heads/deploy"); got != before {
		t.Errorf("deploy after the stop = %s, want %s as before", got, before)
	}
	up.Release()

	done := h.waitForkRebuild(t, id, "completed")
	results, _ := done["patch_results"].([]any)
	if len(results) != 2 {
		t.Fatalf("the completed job = %v, want two patch results", done)
	}
	for _, r := range results {
		if res, _ := r.(map[string]any); res["status"] != "success" {
			t.Errorf("a patch result of the job = %v, want status success", r)
		}
	}
	if head := gitrepotest.Git(t, copyDir, "rev-parse", "refs/heads/deploy"); head == before || head != done["integration_head_sha"] {
		t.Errorf("deploy = %s, want the job's integration_head_sha %v, moved from %s", head, done["integration_head_sha"], before)
	}
	if got := gitrepotest.Git(t, copyDir, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
		t.Errorf("git worktree list of the copy = %q, want the repository alone", got)
	}
	if left, err := os.ReadDir(filepath.Join(copyDir, "parley-rebuild")); err != nil && !os.IsNotExist(err) || len(left) != 0 {
		t.Errorf("the copy's rebuild work area holds %v (%v), want nothing", left, err)
	}
	published := 0
	for _, e := range h.patchEvents(t, "acme-fork", carrypatch.EventRebuildComplete) {
		if e.ResourceID == id {
			published++
		}
	}
	if published != 1 {
		t.Errorf("the job completed %d times in the event log, want once", published)
	}
}
