package carrypatch_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// noScratchLeft fails the test when the repository dir holds a scratch
// working tree, or a record of one.
func noScratchLeft(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, gitrepo.ScratchParent))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%s holds %q, want no entry", gitrepo.ScratchParent, names)
	}
	if got := gitrepotest.Git(t, dir, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
		t.Errorf("git worktree list = %q, want the repository alone", got)
	}
}

func TestRebuild_ScratchTreeIsAlwaysRemoved(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts carrypatch.RebuildOptions
		// second stores the second patch; prepare readies the attempt.
		second       func(t *testing.T, f *conflictFixture)
		prepare      func(t *testing.T, f *conflictFixture)
		kind, reason string
	}{
		{"completed", continueRebase, func(t *testing.T, f *conflictFixture) { f.addPatch(t, "p2") }, nil, attemptCompleted, ""},
		{"fail_fast conflict", carrypatch.RebuildOptions{FailMode: new("fail_fast")},
			func(t *testing.T, f *conflictFixture) { f.conflictingPatch(t, "p2", "a.txt") }, nil, attemptPermanent, "conflict"},
		{"git_error in the second patch", continueRebase, func(t *testing.T, f *conflictFixture) { f.addPatch(t, "p2") },
			func(t *testing.T, f *conflictFixture) {
				p2 := refIn(f.copy, "refs/heads/p2")
				f.hooks.beforePatch(func(i int) {
					if i == 1 {
						if err := os.Remove(filepath.Join(f.copy, "objects", p2[:2], p2[2:])); err != nil {
							t.Errorf("remove the commit of p2: %v", err)
						}
					}
				})
			}, attemptRetryable, "git_error"},
		{"context cancelled before the second patch", continueRebase, func(t *testing.T, f *conflictFixture) { f.addPatch(t, "p2") },
			func(t *testing.T, f *conflictFixture) {
				f.hooks.beforePatch(func(i int) {
					if i == 1 {
						f.runner.cancelAttempt()
					}
				})
			}, attemptRetryable, "shutdown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startConflictFixture(t, nil)
			f.addPatch(t, "p1")
			tc.second(t, f)
			f.moveOn(t)
			// The scratch working tree exists while the attempt builds.
			during := -1
			if tc.prepare != nil {
				tc.prepare(t, f)
			} else {
				f.hooks.beforePatch(func(i int) {
					if i == 1 {
						entries, _ := os.ReadDir(filepath.Join(f.copy, gitrepo.ScratchParent))
						during = len(entries)
					}
				})
			}

			out, _ := f.attemptWith(t, tc.opts, nil)
			if out.Kind != tc.kind || out.Reason != tc.reason {
				t.Errorf("the attempt = %+v, want %s with reason %q", out, tc.kind, tc.reason)
			}
			if tc.prepare == nil && during != 1 {
				t.Errorf("%s held %d entries before the second patch, want the attempt's scratch working tree", gitrepo.ScratchParent, during)
			}
			noScratchLeft(t, f.copy)
		})
	}
}

func TestRebuild_DiscardsLeftoverScratchTree(t *testing.T) {
	t.Parallel()
	f := startRebuildFixture(t)
	f.addPatch(t, "p1")
	ctx := context.Background()
	repo, err := (gitrepo.Git{}).Open(ctx, f.copy)
	if err != nil {
		t.Fatal(err)
	}
	// An earlier job stopped half-way through the replay of a commit.
	ours := commitFiles(t, f.copy, f.base, "ours", map[string]string{"x.txt": "ours\n"})
	theirs := commitFiles(t, f.copy, f.base, "theirs", map[string]string{"x.txt": "theirs\n"})
	left, err := repo.AddScratch(ctx, "an-earlier-job", ours)
	if err != nil {
		t.Fatal(err)
	}
	hub := gitrepo.Signature{Name: "Hub", Email: "hub@example.org", Time: time.Unix(1760000000, 0)}
	if picked, err := left.Pick(ctx, theirs, hub); err != nil || picked.Outcome != gitrepo.PickConflicted {
		t.Fatalf("Pick(theirs) in the leftover = %+v, %v; want conflicted", picked, err)
	}

	out, id := f.attempt(t, nil)
	if out.Kind != attemptCompleted {
		t.Fatalf("the attempt = %+v, want completed", out)
	}
	if r := f.patchResults(t, id); len(r) != 1 || r[0]["status"] != "success" {
		t.Errorf("patch_results = %v, want p1 applied", r)
	}
	if _, err := os.Stat(left.Dir()); !os.IsNotExist(err) {
		t.Errorf("the leftover scratch working tree is still there: %v", err)
	}
	noScratchLeft(t, f.copy)
}

// job is the stored job id.
func (f *rebuildFixture) job(t *testing.T, id string) jobs.Job {
	t.Helper()
	j, err := f.queue.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("read the job %s: %v", id, err)
	}
	return j
}

func TestRebuild_CompletionRecordsCountsAndEmitsComplete(t *testing.T) {
	t.Parallel()
	f := startConflictFixture(t, nil)
	f.addPatch(t, "p1")
	f.addPatch(t, "p2", withStatus(carrypatch.StatusMergedUpstream))
	f.addPatch(t, "p3", withStatus(carrypatch.StatusDisabled))
	f.conflictingPatch(t, "p4", "a.txt")
	f.moveOn(t)
	session := "0b6e1c1e-2f6a-4d35-9a57-3f1f1e2d4c5b"
	opts := continueRebase
	opts.SessionID = &session

	// An earlier attempt failed, while the workspace was being cloned.
	f.setState(t, f.slug, workspace.StatusCloning, workspace.MarkerNone)
	out, id := f.attemptWith(t, opts, nil)
	if out.Kind != attemptRetryable {
		t.Fatalf("the first attempt = %+v, want retryable", out)
	}
	f.setState(t, f.slug, workspace.StatusReady, workspace.MarkerNone)
	j := f.job(t, id)
	if j.Status != jobs.StatusFailed || j.Error == nil || j.NextAttemptAt == nil {
		t.Fatalf("the job after the first attempt is %+v, want failed with an error and a next attempt", j)
	}
	f.advance(j.NextAttemptAt.Sub(f.clock()))

	// What a reader outside the settlement's transaction sees while it is
	// open, once the settlement hook has run.
	var seenStatus string
	seenEvents := -1
	out = f.rerun(t, id, nil, func(jobs.Job) {
		db := f.Store.Main()
		if err := db.QueryRow(`SELECT status FROM jobs WHERE id = ?`, id).Scan(&seenStatus); err != nil {
			t.Errorf("read the job during its settlement: %v", err)
		}
		if err := db.QueryRow(`SELECT count(*) FROM hub_events WHERE type = ? AND resource_id = ?`,
			carrypatch.EventRebuildComplete, id).Scan(&seenEvents); err != nil {
			t.Errorf("count the events during the settlement: %v", err)
		}
	})
	if out.Kind != attemptCompleted {
		t.Fatalf("the second attempt = %+v, want completed", out)
	}
	if seenStatus != jobs.StatusRunning || seenEvents != 0 {
		t.Errorf("a reader during the settlement saw status %q and %d complete events, want running and none", seenStatus, seenEvents)
	}

	j = f.job(t, id)
	if j.Status != jobs.StatusCompleted || j.CompletedAt == nil || !j.CompletedAt.Equal(f.clock()) {
		t.Errorf("the job has status %s, completed_at %v; want completed at %v", j.Status, j.CompletedAt, f.clock())
	}
	if j.Error != nil || j.Detail != nil {
		t.Errorf("the job has error %v and conflict %s, want both null", j.Error, j.Detail)
	}
	res := f.result(t, id)
	counts := map[string]float64{"patches_applied": 1, "patches_skipped": 2, "patches_conflicted": 1, "patches_removed": 1}
	for k, want := range counts {
		if res[k] != want {
			t.Errorf("%s = %v, want %v", k, res[k], want)
		}
	}

	events := f.rebuildEvents(t, f.slug, carrypatch.EventRebuildComplete)
	if len(events) != 1 {
		t.Fatalf("%d parley.rebuild.complete events, want one", len(events))
	}
	e := events[0]
	if e.ResourceType != "rebuild" || e.ResourceID != id || e.ActorType != event.ActorSystem || e.ActorID != nil {
		t.Errorf("the event is about %s %s by %s %v, want rebuild %s by system", e.ResourceType, e.ResourceID, e.ActorType, e.ActorID, id)
	}
	if e.SessionID == nil || *e.SessionID != session {
		t.Errorf("the event's session_id = %v, want %s", e.SessionID, session)
	}
	want := map[string]any{}
	for _, k := range []string{"upstream_head_sha", "integration_head_sha", "previous_integration_head_sha",
		"patches_applied", "patches_skipped", "patches_conflicted", "patches_removed"} {
		want[k] = res[k]
	}
	if want["integration_head_sha"] == nil || want["upstream_head_sha"] == nil {
		t.Errorf("the job holds no heads: %v", res)
	}
	if !reflect.DeepEqual(e.Metadata, want) {
		t.Errorf("the event's metadata = %v, want the job's fields %v", e.Metadata, want)
	}
}

func TestRebuild_EveryFailedAttemptEmitsFail(t *testing.T) {
	t.Parallel()
	upstreamGone := func(t *testing.T, f *conflictFixture) {
		if err := os.RemoveAll(f.upstream.Dir()); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name string
		// prepare readies the attempt; before runs at its start.
		prepare    func(t *testing.T, f *conflictFixture)
		opts       carrypatch.RebuildOptions
		before     func(t *testing.T, f *conflictFixture, j jobs.Job)
		status     string
		willRetry  bool
		retryCount int
		reason     string
		conflict   bool
	}{
		{name: "upstream unreachable, retries left", prepare: upstreamGone, opts: continueRebase,
			status: jobs.StatusFailed, willRetry: true, retryCount: 1, reason: "upstream_fetch_failed"},
		{name: "upstream unreachable, no retry left", prepare: upstreamGone, opts: continueRebase,
			before: func(t *testing.T, f *conflictFixture, j jobs.Job) {
				if err := f.Store.Tx(context.Background(), func(tx *sql.Tx) error {
					_, err := tx.Exec(`UPDATE jobs SET retry_count = ? WHERE id = ?`, jobs.MaxRetries, j.ID)
					return err
				}); err != nil {
					t.Errorf("give the job %s its last retry: %v", j.ID, err)
				}
			},
			status: jobs.StatusDeadLetter, retryCount: jobs.MaxRetries, reason: "upstream_fetch_failed"},
		{name: "fail_fast conflict", prepare: func(t *testing.T, f *conflictFixture) {
			f.conflictingPatch(t, "p1", "a.txt")
			f.moveOn(t)
		}, opts: carrypatch.RebuildOptions{FailMode: new("fail_fast")},
			status: jobs.StatusDeadLetter, reason: "conflict", conflict: true},
		{name: "workspace archived", prepare: func(t *testing.T, f *conflictFixture) {
			f.setState(t, f.slug, workspace.StatusArchived, workspace.MarkerNone)
		}, opts: continueRebase, status: jobs.StatusDeadLetter, reason: "workspace_archived"},
		{name: "the attempt panics", prepare: func(t *testing.T, f *conflictFixture) {
			f.addPatch(t, "p1")
			f.hooks.beforePatch(func(int) { panic("the attempt broke") })
		}, opts: continueRebase, status: jobs.StatusFailed, willRetry: true, retryCount: 1, reason: "git_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startConflictFixture(t, nil)
			tc.prepare(t, f)
			var before func(ctx context.Context, j jobs.Job)
			if tc.before != nil {
				before = func(_ context.Context, j jobs.Job) { tc.before(t, f, j) }
			}

			_, id := f.attemptWith(t, tc.opts, before)
			j := f.job(t, id)
			if j.Status != tc.status {
				t.Errorf("the job has status %s, want %s", j.Status, tc.status)
			}
			if j.Error == nil || *j.Error == "" {
				t.Errorf("the job's error = %v, want the failure's text", j.Error)
			}
			if tc.conflict != (j.Detail != nil) {
				t.Errorf("the job's conflict = %s, want a report: %v", j.Detail, tc.conflict)
			}
			events := f.rebuildEvents(t, f.slug, carrypatch.EventRebuildFail)
			if len(events) != 1 {
				t.Fatalf("%d parley.rebuild.fail events, want one", len(events))
			}
			e := events[0]
			if e.ResourceType != "rebuild" || e.ResourceID != id || e.ActorType != event.ActorSystem || e.ActorID != nil || e.SessionID != nil {
				t.Errorf("the event is about %s %s by %s %v in session %v, want rebuild %s by system, no session",
					e.ResourceType, e.ResourceID, e.ActorType, e.ActorID, e.SessionID, id)
			}
			var conflict any
			if j.Detail != nil {
				if err := json.Unmarshal(j.Detail, &conflict); err != nil {
					t.Fatal(err)
				}
			}
			want := map[string]any{"reason": tc.reason, "will_retry": tc.willRetry, "retry_count": float64(tc.retryCount), "conflict": conflict}
			if !reflect.DeepEqual(e.Metadata, want) {
				t.Errorf("the event's metadata = %v, want %v", e.Metadata, want)
			}
		})
	}
}

func TestRebuild_FailureTextIsScrubbed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		secrets map[string]string
		// text is what the fetch fails with; hidden must not reach the job
		// or the event.
		text   string
		hidden []string
	}{
		{"the upstream URL with user information",
			map[string]string{carrypatch.SecretUpstreamGitPAT: "ghp_SECRETVALUE"},
			"fatal: unable to access https://x-token-auth:ghp_SECRETVALUE@up.example/r.git",
			[]string{"ghp_SECRETVALUE", "x-token-auth:"}},
		{"the origin's secret",
			map[string]string{carrypatch.SecretUpstreamGitPAT: "ghp_SECRETVALUE", workspace.SecretGitPAT: "ghp_ORIGINVALUE"},
			"fatal: unable to access https://up.example/r.git: the server answered ghp_ORIGINVALUE",
			[]string{"ghp_SECRETVALUE", "ghp_ORIGINVALUE"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var failing atomic.Bool
			f := startRebuildFixtureWith(t, func(d *carrypatch.Deps) {
				g := d.Git
				d.FetchRemote = func(ctx context.Context, dir, url, name string, cred gitrepo.Credential, deadline time.Duration) (map[string]string, error) {
					if failing.Load() {
						return nil, errors.New(tc.text)
					}
					return g.FetchRemote(ctx, dir, url, name, cred, deadline)
				}
			})
			for name, value := range tc.secrets {
				f.secrets.set(name, value)
			}
			failing.Store(true)

			out, id := f.attempt(t, nil)
			if out.Reason != "upstream_fetch_failed" {
				t.Fatalf("the attempt = %+v, want a failed upstream fetch", out)
			}
			j := f.job(t, id)
			if j.Error == nil || !strings.Contains(*j.Error, "up.example") {
				t.Errorf("the job's error = %v, want the failure naming up.example", j.Error)
			}
			for _, v := range tc.hidden {
				if j.Error != nil && strings.Contains(*j.Error, v) {
					t.Errorf("the job's error %q holds %q", *j.Error, v)
				}
			}
			events := f.rebuildEvents(t, f.slug, carrypatch.EventRebuildFail)
			if len(events) != 1 {
				t.Fatalf("%d parley.rebuild.fail events, want one", len(events))
			}
			raw, err := json.Marshal(events[0])
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range tc.hidden {
				if strings.Contains(string(raw), v) {
					t.Errorf("the parley.rebuild.fail event %s holds %q", raw, v)
				}
			}
		})
	}
}
