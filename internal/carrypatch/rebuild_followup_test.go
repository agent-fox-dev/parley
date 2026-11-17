package carrypatch_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// followUpSettings is an edit of the workspace that stores the rebuild
// options rebase and fail_fast and the two settings that decide which
// stale inputs queue a follow-up rebuild.
func followUpSettings(afterPush, afterSync bool) func(w *workspace.Workspace) {
	return func(w *workspace.Workspace) {
		w.Settings = map[string]json.RawMessage{
			"rebuild_strategy":        json.RawMessage(`"rebase"`),
			"rebuild_fail_mode":       json.RawMessage(`"fail_fast"`),
			"auto_rebuild_after_push": json.RawMessage(boolJSON(afterPush)),
			"auto_rebuild_after_sync": json.RawMessage(boolJSON(afterSync)),
		}
	}
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// setRefNow points ref of the hub's copy at id, or deletes it when id is
// "", as another writer would while an attempt runs. It fails no test
// fatally, so that a hook may call it.
func (f *rebuildFixture) setRefNow(t *testing.T, ref, id string) {
	args := []string{"update-ref", ref, id}
	if id == "" {
		args = []string{"update-ref", "-d", ref}
	}
	if _, err := gitrepotest.Run(f.copy, "", args...); err != nil {
		t.Errorf("git %v: %v", args, err)
	}
}

// softDeleteNow deletes the patch id as a caller's removal would while an
// attempt runs. It fails no test fatally, so that a hook may call it.
func (f *rebuildFixture) softDeleteNow(t *testing.T, id string) {
	ctx := context.Background()
	now := f.clock().UnixNano()
	err := f.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE patches SET status = ?, position = NULL, deleted_at = ?, updated_at = ?
WHERE workspace_slug = ? AND id = ?`, carrypatch.StatusDeleted, now, now, f.slug, id)
		return err
	})
	if err != nil {
		t.Errorf("delete the patch %s: %v", id, err)
	}
}

// rebuildJobs are the rebuild jobs of the fixture's workspace in status,
// every one when status is "", newest first.
func (f *rebuildFixture) rebuildJobs(t *testing.T, status string) []jobs.Job {
	t.Helper()
	js, err := f.queue.List(context.Background(), carrypatch.RebuildJobType, f.slug, status, nil, 200)
	if err != nil {
		t.Fatalf("list the rebuild jobs of %s: %v", f.slug, err)
	}
	return js
}

// payloadOf is the payload of the job j as a JSON object.
func payloadOf(t *testing.T, j jobs.Job) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(j.Payload, &m); err != nil {
		t.Fatalf("the payload of the job %s is not a JSON object: %v", j.ID, err)
	}
	return m
}

// checkFollowUp checks that the ended job id queued exactly one follow-up
// rebuild, still queued, whose payload holds want, and wrote one
// parley.rebuild.followup about id with the stale inputs given, and
// returns the follow-up job.
func (f *rebuildFixture) checkFollowUp(t *testing.T, id string, want map[string]any, stalePatches []any, upstreamStale bool) jobs.Job {
	t.Helper()
	queued := f.rebuildJobs(t, jobs.StatusQueued)
	if len(queued) != 1 || queued[0].ID == id {
		t.Fatalf("the queued rebuild jobs are %v, want one follow-up of %s", queued, id)
	}
	next := queued[0]
	payload := payloadOf(t, next)
	for k, v := range want {
		if payload[k] != v {
			t.Errorf("the follow-up's %s = %v, want %v", k, payload[k], v)
		}
	}
	events := f.rebuildEvents(t, f.slug, carrypatch.EventRebuildFollowUp)
	if len(events) != 1 {
		t.Fatalf("%d parley.rebuild.followup events, want one", len(events))
	}
	e := events[0]
	if e.ResourceType != "rebuild" || e.ResourceID != id || e.ActorType != event.ActorSystem || e.ActorID != nil {
		t.Errorf("the event is about %s %s by %s %v, want rebuild %s by system", e.ResourceType, e.ResourceID, e.ActorType, e.ActorID, id)
	}
	meta := map[string]any{"follow_up_job_id": next.ID, "stale_patches": stalePatches, "upstream_stale": upstreamStale}
	if !reflect.DeepEqual(e.Metadata, meta) {
		t.Errorf("the event's metadata = %v, want %v", e.Metadata, meta)
	}
	return next
}

func TestRebuild_FollowUpAfterPublish(t *testing.T) {
	t.Parallel()
	// What a hook changes before the publish.
	const (
		movePatch = 1 << iota
		moveUpstream
		deleteBranch
		deletePatch
	)
	cases := []struct {
		name          string
		change        int
		afterPush     bool
		afterSync     bool
		followUp      bool
		stalePatches  []any
		upstreamStale bool
	}{
		{"the branch of p1 moved", movePatch, true, true, true, []any{"feature/a"}, false},
		{"the upstream base moved", moveUpstream, true, true, true, []any{}, true},
		{"both moved", movePatch | moveUpstream, true, true, true, []any{"feature/a"}, true},
		{"the branch of p1 moved, auto_rebuild_after_push off", movePatch, false, true, false, nil, false},
		{"the upstream base moved, auto_rebuild_after_sync off", moveUpstream, true, false, false, nil, false},
		{"both moved, auto_rebuild_after_push off", movePatch | moveUpstream, false, true, true, []any{}, true},
		{"nothing moved", 0, true, true, false, nil, false},
		{"the branch of p1 was deleted", deleteBranch, true, true, false, nil, false},
		{"p1 was deleted and its branch moved", deletePatch | movePatch, true, true, false, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startRebuildFixture(t, followUpSettings(tc.afterPush, tc.afterSync))
			p1 := f.addPatch(t, "feature/a")
			moved := commitFiles(t, f.copy, f.base, "feature/a again", map[string]string{"feature-a.txt": "again\n"})
			upstream := commitFiles(t, f.copy, f.base, "U2", map[string]string{"u2.txt": "u2\n"})
			f.hooks.beforePublish(func() {
				if tc.change&deletePatch != 0 {
					f.softDeleteNow(t, p1.ID)
				}
				if tc.change&movePatch != 0 {
					f.setRefNow(t, "refs/heads/feature/a", moved)
				}
				if tc.change&deleteBranch != 0 {
					f.setRefNow(t, "refs/heads/feature/a", "")
				}
				if tc.change&moveUpstream != 0 {
					f.setRefNow(t, "refs/remotes/upstream/main", upstream)
				}
			})
			session := "4d1f7a2e-3c5b-4e8a-9f0d-6b2c1a7e5d3f"
			opts := carrypatch.RebuildOptions{Strategy: new("merge"), FailMode: new("continue"), SessionID: &session}

			out, id := f.attemptWith(t, opts, nil)
			if out.Kind != attemptCompleted {
				t.Fatalf("the attempt = %+v, want completed", out)
			}
			if j := f.job(t, id); j.Status != jobs.StatusCompleted || j.Error != nil {
				t.Errorf("the ended job has status %s and error %v, want completed with none", j.Status, j.Error)
			}
			if !tc.followUp {
				if queued := f.rebuildJobs(t, jobs.StatusQueued); len(queued) != 0 {
					t.Errorf("the queued rebuild jobs are %v, want none", queued)
				}
				if events := f.rebuildEvents(t, f.slug, carrypatch.EventRebuildFollowUp); len(events) != 0 {
					t.Errorf("parley.rebuild.followup events %v, want none", events)
				}
				return
			}
			f.checkFollowUp(t, id, map[string]any{
				"strategy": "merge", "fail_mode": "continue", "integration_branch": "deploy",
				"submitted_by": "system:followup", "session_id": session,
			}, tc.stalePatches, tc.upstreamStale)
			if e := f.rebuildEvents(t, f.slug, carrypatch.EventRebuildFollowUp)[0]; e.SessionID == nil || *e.SessionID != session {
				t.Errorf("the event's session_id = %v, want %s", e.SessionID, session)
			}
		})
	}
}

func TestRebuild_FollowUpAfterFailFastConflict(t *testing.T) {
	t.Parallel()
	f := startConflictFixture(t, nil)
	f.conflictingPatch(t, "feature/a", "a.txt")
	f.moveOn(t)
	applies := commitFiles(t, f.copy, f.cut, "feature/a without a.txt", map[string]string{"feature-a.txt": "feature/a\n"})
	f.hooks.beforePatch(func(i int) {
		if i == 0 {
			f.setRefNow(t, "refs/heads/feature/a", applies)
		}
	})

	out, id := f.attemptWith(t, carrypatch.RebuildOptions{FailMode: new("fail_fast")}, nil)
	if out.Kind != attemptPermanent || out.Reason != "conflict" {
		t.Fatalf("the attempt = %+v, want permanent with reason conflict", out)
	}
	if j := f.job(t, id); j.Status != jobs.StatusDeadLetter || j.Detail == nil {
		t.Errorf("the ended job has status %s and conflict %s, want dead_letter with its conflict", j.Status, j.Detail)
	}
	f.checkFollowUp(t, id, map[string]any{"submitted_by": "system:followup", "fail_mode": "fail_fast"}, []any{"feature/a"}, false)
}

func TestRebuild_NoFollowUpAfterRetryableFailure(t *testing.T) {
	t.Parallel()
	f := startRebuildFixture(t)
	f.addPatch(t, "feature/a")
	tip := refIn(f.copy, "refs/heads/feature/a")
	moved := commitFiles(t, f.copy, f.base, "feature/a again", map[string]string{"feature-a.txt": "again\n"})
	f.hooks.beforePatch(func(i int) {
		if i != 0 {
			return
		}
		f.setRefNow(t, "refs/heads/feature/a", moved)
		if err := os.Remove(filepath.Join(f.copy, "objects", tip[:2], tip[2:])); err != nil {
			t.Errorf("remove the recorded tip of feature/a: %v", err)
		}
	})

	out, id := f.attempt(t, nil)
	if out.Kind != attemptRetryable || out.Reason != "git_error" {
		t.Fatalf("the attempt = %+v, want retryable with reason git_error", out)
	}
	if j := f.job(t, id); j.Status != jobs.StatusFailed {
		t.Errorf("the job has status %s, want failed", j.Status)
	}
	if all := f.rebuildJobs(t, ""); len(all) != 1 || all[0].ID != id {
		t.Errorf("the rebuild jobs of the workspace are %v, want the one waiting to retry", all)
	}
	if events := f.rebuildEvents(t, f.slug, carrypatch.EventRebuildFollowUp); len(events) != 0 {
		t.Errorf("parley.rebuild.followup events %v, want none", events)
	}
}

func TestRebuild_DuplicateFollowUpIsSkippedSilently(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// conflicting makes the patch conflict with upstream; the run is
		// fail_fast.
		conflicting bool
		kind        string
		status      string
	}{
		{"after a publish", false, attemptCompleted, jobs.StatusCompleted},
		{"after a fail_fast conflict", true, attemptPermanent, jobs.StatusDeadLetter},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startConflictFixture(t, nil)
			if tc.conflicting {
				f.conflictingPatch(t, "feature/a", "a.txt")
			} else {
				f.addPatch(t, "feature/a")
			}
			f.moveOn(t)
			moved := commitFiles(t, f.copy, f.cut, "feature/a again", map[string]string{"feature-a.txt": "again\n"})
			f.hooks.beforePatch(func(i int) {
				if i == 0 {
					f.setRefNow(t, "refs/heads/feature/a", moved)
				}
			})
			stub := &recordingRebuilds{}
			stub.answer(f.slug, carrypatch.RebuildResult{ExistingJobID: "0b8f3c1d-7e2a-4b6c-9d5e-1f4a2c3b7e9d"}, nil)
			f.comp.RegisterRebuildEnqueuer(stub)

			out, id := f.attemptWith(t, carrypatch.RebuildOptions{FailMode: new("fail_fast")}, nil)
			if out.Kind != tc.kind {
				t.Fatalf("the attempt = %+v, want %s", out, tc.kind)
			}
			calls := stub.callsOf(f.slug)
			if len(calls) != 1 || calls[0].opts.FollowUpOf == nil || *calls[0].opts.FollowUpOf != id {
				t.Fatalf("the enqueuer had the calls %+v, want one follow-up of %s", calls, id)
			}
			j := f.job(t, id)
			if j.Status != tc.status {
				t.Errorf("the job has status %s, want %s", j.Status, tc.status)
			}
			switch {
			case tc.status == jobs.StatusCompleted && j.Error != nil:
				t.Errorf("the job's error = %q, want null", *j.Error)
			case tc.status == jobs.StatusDeadLetter && (j.Error == nil || *j.Error != out.Text):
				t.Errorf("the job's error = %v, want the conflict's text %q alone", j.Error, out.Text)
			}
			if events := f.rebuildEvents(t, f.slug, carrypatch.EventRebuildFollowUp); len(events) != 0 {
				t.Errorf("parley.rebuild.followup events %v, want none", events)
			}
		})
	}
}
