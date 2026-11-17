package carrypatch_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/jobs"
)

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
			moved := gitrepotest.Commit(t, f.copy, "feature/a again", map[string]string{"feature-a.txt": "again\n"}, f.base)
			upstream := gitrepotest.Commit(t, f.copy, "U2", map[string]string{"u2.txt": "u2\n"}, f.base)
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
	applies := gitrepotest.Commit(t, f.copy, "feature/a without a.txt", map[string]string{"feature-a.txt": "feature/a\n"}, f.cut)
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
	moved := gitrepotest.Commit(t, f.copy, "feature/a again", map[string]string{"feature-a.txt": "again\n"}, f.base)
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
			moved := gitrepotest.Commit(t, f.copy, "feature/a again", map[string]string{"feature-a.txt": "again\n"}, f.cut)
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

func TestRebuild_UnreadableFollowUpInputsAreLogged(t *testing.T) {
	t.Parallel()
	log := &hubtest.LogBuffer{}
	var copyDir string
	f := startRebuildFixtureWith(t, func(d *carrypatch.Deps) {
		d.Logger = slog.New(slog.NewTextHandler(log, nil))
		// The push, the last step before the check of the inputs, takes the
		// hub's copy away, so that neither a branch tip nor the upstream base
		// can be read.
		d.Push = func(context.Context, gitrepo.PushRequest) (gitrepo.PushResult, error) {
			return gitrepo.PushPushed, os.Rename(copyDir, copyDir+".away")
		}
	}, pushOn)
	copyDir = f.copy
	f.addPatch(t, "p1")

	out, _ := f.attempt(t, nil)
	if out.Kind != attemptCompleted {
		t.Fatalf("the attempt = %+v, want completed", out)
	}
	if err := os.Rename(copyDir+".away", copyDir); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"rebuild could not read a patch branch for the follow-up", "rebuild could not resolve the upstream base for the follow-up"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the component's log does not say %q:\n%s", want, log.String())
		}
	}
	if events := f.rebuildEvents(t, f.slug, carrypatch.EventRebuildFollowUp); len(events) != 0 {
		t.Errorf("parley.rebuild.followup events %v, want none", events)
	}
}
