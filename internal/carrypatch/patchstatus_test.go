package carrypatch_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// patchStatus sends GET /api/v1/workspaces/<slug>/patch-status as the
// owner and decodes the answer's document.
func (h *readHub) patchStatus(t *testing.T, slug string) (int, map[string]any) {
	t.Helper()
	status, body := h.Call(t, http.MethodGet, "/api/v1/workspaces/"+slug+"/patch-status", h.key, nil)
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("GET patch-status of %s = %d %s: %v", slug, status, body, err)
	}
	return status, doc
}

// keysOf are the members of the object v, sorted; nil when v is no object.
func keysOf(v any) []string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return slices.Sorted(maps.Keys(m))
}

// sortedNames are names, sorted.
func sortedNames(names ...string) []string { return slices.Sorted(slices.Values(names)) }

// rebuildSummaryMembers are the members of a rebuild in the patch status.
var rebuildSummaryMembers = sortedNames("id", "status", "created_at", "completed_at")

// summaryOf is the summary of the patch status doc as integers by name;
// a member that is no whole number fails the test.
func summaryOf(t *testing.T, doc map[string]any) map[string]int {
	t.Helper()
	raw, ok := doc["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary = %v, want an object", doc["summary"])
	}
	out := map[string]int{}
	for k, v := range raw {
		n, ok := v.(float64)
		if !ok || n != float64(int(n)) {
			t.Fatalf("summary.%s = %v, want an integer", k, v)
		}
		out[k] = int(n)
	}
	return out
}

// patchResults is the result document of a rebuild whose patch_results
// give each patch id of statuses the status statuses gives it.
func patchResults(statuses map[string]string) string {
	type entry struct {
		PatchID string `json:"patch_id"`
		Status  string `json:"status"`
	}
	var entries []entry
	for _, id := range slices.Sorted(maps.Keys(statuses)) {
		entries = append(entries, entry{id, statuses[id]})
	}
	raw, _ := json.Marshal(map[string]any{"patch_results": entries})
	return string(raw)
}

func TestPatchStatus(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)

	// The subtests that run attempts through the queue run one after the
	// other, before the parallel ones: a pass claims every queued job of
	// the hub, and the job left queued last must stay so.
	t.Run("one document of the stored workspace, the integration head and the latest rebuild", func(t *testing.T) {
		patchStatusDocument(t, h)
	})

	t.Run("every non-deleted patch in order with its status in the newest rebuild that has one", func(t *testing.T) {
		patchStatusPatches(t, h)
	})

	t.Run("the newest rebuild and the newest completed one, of the workspace alone", func(t *testing.T) {
		patchStatusRebuilds(t, h)
	})

	t.Run("no integration head without the branch or a copy", func(t *testing.T) {
		t.Parallel()
		patchStatusNoIntegrationHead(t, h)
	})

	t.Run("eight counts, each present", func(t *testing.T) {
		t.Parallel()
		patchStatusSummary(t, h)
	})

	t.Run("reads the stored state only, with the remotes down", func(t *testing.T) {
		t.Parallel()
		patchStatusReadsStoredStateOnly(t, h)
	})

	t.Run("answers in every status of the workspace", func(t *testing.T) {
		t.Parallel()
		patchStatusInEveryStatus(t, h)
	})
}

func patchStatusDocument(t *testing.T, h *rebuildHub) {
	syncedAt := time.Date(2026, 11, 16, 9, 30, 0, 0, time.UTC)
	h.add(t, "demo", workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone, func(w *workspace.Workspace) {
		w.UpstreamURL = new("https://upstream.test/demo.git")
		w.UpstreamHeadSHA = new(strings.Repeat("a", 40))
		w.LastSyncAt = &syncedAt
		w.SyncError = new("upstream fetch failed")
	})
	dir := h.repoPath("demo")
	gitrepotest.InitBare(t, dir, "")
	head := rootsAt(t, dir, map[string]string{"refs/heads/deploy": "deploy", "refs/heads/main": "main"})["refs/heads/deploy"]
	job := h.settledRebuild(t, "demo", rebuiltFrom("", head), nil)

	status, doc := h.patchStatus(t, "demo")
	if status != http.StatusOK {
		t.Fatalf("GET patch-status = %d %v, want 200", status, doc)
	}
	want := sortedNames("workspace_slug", "workspace_mode", "status", "git_url", "upstream_url", "integration_branch",
		"clone_error", "sync_error", "upstream_head_sha", "last_sync_at", "integration_head_sha", "last_rebuild",
		"last_completed_rebuild", "patches", "summary")
	if got := keysOf(any(doc)); !slices.Equal(got, want) {
		t.Errorf("the members of the patch status = %v, want exactly %v", got, want)
	}
	for name, value := range map[string]any{
		"workspace_slug": "demo", "workspace_mode": "carry_patch", "status": "ready", "integration_branch": "deploy",
		"git_url": "https://origin.test/demo.git", "upstream_url": "https://upstream.test/demo.git",
		"upstream_head_sha": strings.Repeat("a", 40), "last_sync_at": "2026-11-16T09:30:00Z",
		"clone_error": nil, "sync_error": "upstream fetch failed",
	} {
		if doc[name] != value {
			t.Errorf("%s = %v, want %v", name, doc[name], value)
		}
	}
	if got := doc["integration_head_sha"]; got != head || len(head) != 40 || got != refIn(dir, "refs/heads/deploy") {
		t.Errorf("integration_head_sha = %v, want the 40-character head of refs/heads/deploy, %s", got, head)
	}
	for _, name := range []string{"last_rebuild", "last_completed_rebuild"} {
		if r, _ := doc[name].(map[string]any); r["id"] != job || r["status"] != jobs.StatusCompleted {
			t.Errorf("%s = %v, want the completed rebuild %s", name, doc[name], job)
		}
	}
}

func patchStatusPatches(t *testing.T, h *rebuildHub) {
	syncedAt := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	h.add(t, "listed", workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone)
	p1, p2, p3 := livePatch("listed", "p1", 1), livePatch("listed", "p2", 2), livePatch("listed", "p3", 3)
	p1.OriginSyncState, p1.OriginSHA, p1.OriginSyncedAt = new("in_sync"), new(strings.Repeat("b", 40)), &syncedAt
	p2.Status, p2.ConflictFiles = carrypatch.StatusConflict, []string{"a.go"}
	p3.Status = carrypatch.StatusDisabled
	h.put(t, p1, p2, p3, deletedPatch("listed", "gone", at(10, 0)))
	h.settledRebuild(t, "listed", patchResults(map[string]string{p1.ID: "success", p2.ID: "success", p3.ID: "skipped"}), nil)
	h.advance(time.Minute)
	h.settledRebuild(t, "listed", patchResults(map[string]string{p1.ID: "success", p2.ID: "conflict"}), jobs.Permanent(errors.New("conflict")))
	h.put(t, livePatch("listed", "p4", 4))

	status, doc := h.patchStatus(t, "listed")
	patches, _ := doc["patches"].([]any)
	if status != http.StatusOK || len(patches) != 4 {
		t.Fatalf("GET patch-status = %d with %d patches (%v), want 200 with 4", status, len(patches), doc["patches"])
	}
	members := sortedNames("id", "branch_name", "position", "status", "conflict_files", "last_rebuild_result",
		"origin_sync_state", "origin_sha", "origin_synced_at")
	want := []map[string]any{
		{"id": p1.ID, "branch_name": "p1", "position": 1.0, "status": "active", "conflict_files": []any{}, "last_rebuild_result": "success",
			"origin_sync_state": "in_sync", "origin_sha": strings.Repeat("b", 40), "origin_synced_at": "2026-11-16T08:00:00Z"},
		{"id": p2.ID, "branch_name": "p2", "position": 2.0, "status": "conflict", "conflict_files": []any{"a.go"}, "last_rebuild_result": "conflict",
			"origin_sync_state": nil, "origin_sha": nil, "origin_synced_at": nil},
		{"branch_name": "p3", "position": 3.0, "status": "disabled", "last_rebuild_result": "skipped"},
		{"branch_name": "p4", "position": 4.0, "status": "active", "last_rebuild_result": nil},
	}
	for i, v := range patches {
		p, _ := v.(map[string]any)
		if got := keysOf(v); !slices.Equal(got, members) {
			t.Errorf("the members of patch %d = %v, want exactly %v", i, got, members)
		}
		for name, value := range want[i] {
			if !reflect.DeepEqual(p[name], value) {
				t.Errorf("patch %d %s = %#v, want %#v", i, name, p[name], value)
			}
		}
	}

	h.add(t, "many", workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone)
	var many []carrypatch.Patch
	for i := range 250 {
		many = append(many, livePatch("many", fmt.Sprintf("p%03d", i+1), i+1))
	}
	h.put(t, many...)
	if _, doc := h.patchStatus(t, "many"); len(doc["patches"].([]any)) != 250 {
		t.Errorf("GET patch-status of 250 patches holds %d, want all 250", len(doc["patches"].([]any)))
	}
}

func patchStatusRebuilds(t *testing.T, h *rebuildHub) {
	// The history with a job left queued is last, so that no later
	// pass claims it.
	for _, tc := range []struct {
		slug           string
		history        []string
		last, lastDone int // indexes into history; -1 for null
	}{
		{"onedone", []string{jobs.StatusCompleted}, 0, 0},
		{"thendead", []string{jobs.StatusCompleted, jobs.StatusDeadLetter}, 1, 0},
		{"thenfailed", []string{jobs.StatusCompleted, jobs.StatusCompleted, jobs.StatusFailed}, 2, 1},
		{"thenqueued", []string{jobs.StatusCancelled, jobs.StatusQueued}, 1, -1},
		{"nojobs", nil, -1, -1},
	} {
		h.carryWith(t, tc.slug, nil)
		var ids []string
		for _, status := range tc.history {
			h.advance(time.Minute)
			id := h.queueRebuild(t, tc.slug)
			if status != jobs.StatusQueued {
				h.drive(t, id, status)
			}
			ids = append(ids, id)
		}
		status, doc := h.patchStatus(t, tc.slug)
		if status != http.StatusOK {
			t.Errorf("GET patch-status of %s = %d %v, want 200", tc.slug, status, doc)
			continue
		}
		for name, i := range map[string]int{"last_rebuild": tc.last, "last_completed_rebuild": tc.lastDone} {
			if i < 0 {
				if doc[name] != nil {
					t.Errorf("%s: %s = %v, want null", tc.slug, name, doc[name])
				}
				continue
			}
			r, _ := doc[name].(map[string]any)
			if got := keysOf(doc[name]); !slices.Equal(got, rebuildSummaryMembers) {
				t.Errorf("%s: the members of %s = %v, want exactly %v", tc.slug, name, got, rebuildSummaryMembers)
			}
			j := h.job(t, ids[i])
			wantDone := any(nil)
			if j.CompletedAt != nil {
				wantDone = j.CompletedAt.UTC().Format(time.RFC3339)
			}
			if r["id"] != j.ID || r["status"] != tc.history[i] || r["created_at"] != j.CreatedAt.UTC().Format(time.RFC3339) || r["completed_at"] != wantDone {
				t.Errorf("%s: %s = %v, want the %s job %s created at %s, completed at %v", tc.slug, name, r, tc.history[i], j.ID, j.CreatedAt, wantDone)
			}
			if (tc.history[i] == jobs.StatusQueued || tc.history[i] == jobs.StatusFailed) && r["completed_at"] != nil {
				t.Errorf("%s: %s.completed_at = %v for a %s job, want null", tc.slug, name, r["completed_at"], tc.history[i])
			}
		}
	}
}

func patchStatusNoIntegrationHead(t *testing.T, h *rebuildHub) {
	h.add(t, "nodeploy", workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone)
	dir := h.repoPath("nodeploy")
	gitrepotest.InitBare(t, dir, "")
	rootsAt(t, dir, map[string]string{"refs/heads/deploy": "deploy", "refs/heads/main": "main"})
	gitrepotest.Git(t, dir, "update-ref", "-d", "refs/heads/deploy")
	h.add(t, "nocopypending", workspace.ModeCarryPatch, workspace.StatusPending, workspace.MarkerNone)
	h.add(t, "nocopyfailed", workspace.ModeCarryPatch, workspace.StatusFailed, workspace.MarkerNone)
	for slug, stored := range map[string]string{"nodeploy": "ready", "nocopypending": "pending", "nocopyfailed": "failed"} {
		status, doc := h.patchStatus(t, slug)
		if status != http.StatusOK || doc["integration_head_sha"] != nil || doc["status"] != stored {
			t.Errorf("GET patch-status of %s = %d integration_head_sha %v status %v, want 200, null and %s",
				slug, status, doc["integration_head_sha"], doc["status"], stored)
		}
	}
}

func patchStatusSummary(t *testing.T, h *rebuildHub) {
	h.add(t, "countnone", workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone)
	h.add(t, "counted", workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone)
	state := func(p carrypatch.Patch, s string) carrypatch.Patch { p.OriginSyncState = &s; return p }
	withStatus := func(p carrypatch.Patch, s string) carrypatch.Patch { p.Status = s; return p }
	h.put(t,
		state(livePatch("counted", "a1", 1), "diverged"),
		state(livePatch("counted", "a2", 2), "missing_on_origin"),
		state(withStatus(livePatch("counted", "c1", 3), carrypatch.StatusConflict), "missing_on_origin"),
		withStatus(livePatch("counted", "d1", 4), carrypatch.StatusDisabled),
		withStatus(livePatch("counted", "m1", 5), carrypatch.StatusMergedUpstream),
		state(deletedPatch("counted", "x1", at(10, 0)), "diverged"),
		state(deletedPatch("counted", "x2", at(11, 0)), "missing_on_origin"),
	)
	when := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	entries := []rerereEntry{
		{rerereID("1"), "a.go", when, true}, {rerereID("2"), "b.go", when, true}, {rerereID("3"), "c.go", when, false},
	}
	dir := h.repoPath("counted")
	gitrepotest.InitBare(t, dir, "")
	h.recordAt(t, "counted", entries...)
	rrCacheOf(t, dir, entries...)

	zero := map[string]int{"total_patches": 0, "active": 0, "conflict": 0, "disabled": 0, "merged_upstream": 0,
		"rerere_resolutions": 0, "patches_diverged": 0, "patches_missing_on_origin": 0}
	for slug, want := range map[string]map[string]int{
		"countnone": zero,
		"counted": {"total_patches": 5, "active": 2, "conflict": 1, "disabled": 1, "merged_upstream": 1,
			"rerere_resolutions": 3, "patches_diverged": 1, "patches_missing_on_origin": 2},
	} {
		status, doc := h.patchStatus(t, slug)
		if got := summaryOf(t, doc); status != http.StatusOK || !maps.Equal(got, want) {
			t.Errorf("GET patch-status of %s = %d with summary %v, want 200 with exactly %v", slug, status, got, want)
		}
		if listed := len(h.get(t, "/workspaces/"+slug+"/rerere").Items); summaryOf(t, doc)["rerere_resolutions"] != listed {
			t.Errorf("rerere_resolutions of %s = %d, want the %d entries the list holds", slug, summaryOf(t, doc)["rerere_resolutions"], listed)
		}
	}
}

func patchStatusReadsStoredStateOnly(t *testing.T, h *rebuildHub) {
	origin, upstream := gitrepotest.NewOrigin(t), gitrepotest.NewOrigin(t)
	stored := upstream.Head("main")
	h.carryWith(t, "offline", nil, carrypatch.StatusActive)
	ctx := context.Background()
	if err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE workspaces SET git_url = ?, upstream_url = ?, upstream_head_sha = ? WHERE slug = ?`,
			origin.URL(), upstream.URL(), stored, "offline")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	dir := h.repoPath("offline")
	gitrepotest.InitBare(t, dir, "")
	rootsAt(t, dir, map[string]string{"refs/heads/deploy": "deploy", "refs/heads/patch/1": "p1"})
	e := rerereEntry{rerereID("d"), "d.go", time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC), true}
	h.recordAt(t, "offline", e)
	rrCacheOf(t, dir, e)
	moved := upstream.Commit("main", "U2")
	origin.Close()
	upstream.Close()

	record := func() workspace.Workspace {
		w, _, err := workspace.BySlug(ctx, h.Store.Main(), "offline")
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	w, state, rows, rrCache := record(), h.previewState(t, "offline"), h.storedEntries(t, "offline"), filesIn(t, filepath.Join(dir, "rr-cache"))
	originRequests, upstreamRequests := origin.Requests(), upstream.Requests()

	status, doc := h.patchStatus(t, "offline")
	if status != http.StatusOK || doc["upstream_head_sha"] != stored || stored == moved {
		t.Errorf("GET patch-status = %d upstream_head_sha %v, want 200 and the stored %s, not upstream's %s", status, doc["upstream_head_sha"], stored, moved)
	}
	if o, u := origin.Requests(), upstream.Requests(); o != originRequests || u != upstreamRequests {
		t.Errorf("the origin and the upstream received %d and %d requests, want %d and %d", o, u, originRequests, upstreamRequests)
	}
	if got := record(); !reflect.DeepEqual(got, w) {
		t.Errorf("the workspace record changed:\n got %+v\nwant %+v", got, w)
	}
	if got := h.previewState(t, "offline"); !reflect.DeepEqual(got, state) {
		t.Errorf("the refs, patches, jobs or event log changed:\n got %+v\nwant %+v", got, state)
	}
	if got := h.storedEntries(t, "offline"); !slices.Equal(got, rows) {
		t.Errorf("the entry rows = %v, want them unchanged, %v", got, rows)
	}
	if got := filesIn(t, filepath.Join(dir, "rr-cache")); !maps.Equal(got, rrCache) {
		t.Errorf("the rr-cache changed: %v, want %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(rrCache)))
	}
}

func patchStatusInEveryStatus(t *testing.T, h *rebuildHub) {
	for _, tc := range []struct {
		slug   string
		status workspace.Status
		marker workspace.Marker
	}{
		{"stpending", workspace.StatusPending, workspace.MarkerNone},
		{"stcloning", workspace.StatusCloning, workspace.MarkerNone},
		{"stready", workspace.StatusReady, workspace.MarkerNone},
		{"stfailed", workspace.StatusFailed, workspace.MarkerNone},
		{"starchived", workspace.StatusArchived, workspace.MarkerNone},
		{"streclone", workspace.StatusReady, workspace.MarkerReclone},
		{"starchiving", workspace.StatusReady, workspace.MarkerArchive},
		{"stdeleting", workspace.StatusReady, workspace.MarkerDelete},
	} {
		h.add(t, tc.slug, workspace.ModeCarryPatch, tc.status, tc.marker)
		if status, doc := h.patchStatus(t, tc.slug); status != http.StatusOK || doc["status"] != string(tc.status) {
			t.Errorf("GET patch-status of a %s workspace (marker %q) = %d status %v, want 200 and %s", tc.status, tc.marker, status, doc["status"], tc.status)
		}
	}
}
