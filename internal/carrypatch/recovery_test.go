package carrypatch_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestDiscardRebuildLeftovers_EmptiesTheWorkAreaAndKeepsTheRefs(t *testing.T) {
	t.Parallel()
	f := startRebuildFixture(t)
	f.addPatch(t, "p1")
	f.addPatch(t, "p2")

	// The attempt stops as a stop of the hub stops it, before the second
	// patch, and its scratch working tree and the repository's record of it
	// are kept where the attempt's own clean-up does not find them, as a
	// killed process leaves them.
	held := t.TempDir()
	parked := []string{gitrepo.ScratchParent, "worktrees"}
	f.hooks.beforePatch(func(i int) {
		if i != 1 {
			return
		}
		for _, name := range parked {
			if err := os.Rename(filepath.Join(f.copy, name), filepath.Join(held, name)); err != nil {
				t.Errorf("keep %s from the attempt's clean-up: %v", name, err)
			}
		}
		f.runner.cancelAttempt()
	})
	out, id := f.attempt(t, nil)
	f.hooks.beforePatch(nil)
	if out.Kind != attemptRetryable || out.Reason != "shutdown" {
		t.Fatalf("the interrupted attempt = %+v, want retryable with reason shutdown", out)
	}
	for _, name := range parked {
		if err := os.Rename(filepath.Join(held, name), filepath.Join(f.copy, name)); err != nil {
			t.Fatalf("put %s back: %v", name, err)
		}
	}
	if got := gitrepotest.Git(t, f.copy, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 2 {
		t.Fatalf("git worktree list before the start = %q, want the repository and the interrupted attempt's scratch working tree", got)
	}
	before := clientRefs(t, f.copy)

	if err := f.comp.DiscardRebuildLeftovers(context.Background()); err != nil {
		t.Fatalf("DiscardRebuildLeftovers = %v, want nil", err)
	}
	noScratchLeft(t, f.copy)
	if after := clientRefs(t, f.copy); after != before {
		t.Errorf("the refs after the start =\n%s\nwant them as before:\n%s", after, before)
	}

	// A rebuild queued now joins the interrupted job, which runs again from
	// the first patch once it is due and completes.
	res, err := f.comp.EnqueueRebuild(context.Background(), f.slug,
		carrypatch.RebuildOptions{ActorType: event.ActorSystem, SubmittedBy: event.ActorSystem})
	if err != nil || res.ExistingJobID != id {
		t.Fatalf("queue a rebuild after the start = %+v, %v; want the interrupted job %s", res, err, id)
	}
	j := f.job(t, id)
	if j.NextAttemptAt == nil {
		t.Fatalf("the interrupted job is %+v, want one with a next attempt", j)
	}
	f.advance(j.NextAttemptAt.Sub(f.clock()))
	if out := f.rerun(t, id, nil, nil); out.Kind != attemptCompleted {
		t.Fatalf("the attempt after the start = %+v, want completed", out)
	}
	results := f.patchResults(t, id)
	if len(results) != 2 || results[0]["status"] != "success" || results[1]["status"] != "success" {
		t.Errorf("patch_results = %v, want p1 and p2 applied", results)
	}
	if head := refIn(f.copy, "refs/heads/deploy"); head == f.base {
		t.Errorf("deploy is still at the upstream base %s after the completed rebuild", head)
	}
	noScratchLeft(t, f.copy)
}

func TestDiscardRebuildLeftovers_LogsAWorkspaceItCannotCleanAndGoesOn(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// spoil makes the leftovers of the workspace whose copy is dir
		// impossible to discard.
		spoil func(t *testing.T, dir string)
	}{
		{"the work area cannot be removed", func(t *testing.T, dir string) {
			parent := filepath.Join(dir, gitrepo.ScratchParent)
			if err := os.Chmod(parent, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
		}},
		{"the copy is gone", func(t *testing.T, dir string) {
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := startRecoveryHub(t)
			jobA, dirA := h.interrupted(t, "a")
			_, dirB := h.interrupted(t, "b")
			tc.spoil(t, dirA)

			if err := h.comp.DiscardRebuildLeftovers(context.Background()); err != nil {
				t.Fatalf("DiscardRebuildLeftovers = %v, want nil", err)
			}
			noScratchLeft(t, dirB)
			if !loggedAbout(h.Log.String(), "a") {
				t.Errorf("no log line names the workspace a; log = %q", h.Log.String())
			}

			// The hub's start runs the same discard: what b holds again is gone
			// once a hub on this data listens.
			leaveScratch(t, dirB, "left-again")
			h.Restart(t)
			if status, body := h.Call(t, http.MethodGet, "/readyz", "", nil); status != http.StatusOK {
				t.Errorf("GET /readyz after the restart = %d %s, want 200", status, body)
			}
			noScratchLeft(t, dirB)
			if got := h.statusAtStart(jobA); got != jobs.StatusQueued {
				t.Errorf("the job of a after the start hooks has status %q, want queued", got)
			}
		})
	}
}

// recoveryHub is a hub whose job queue passes only when its clock is
// advanced, which the test never does, and completes every rebuild job it
// runs at once; it reads the status of every job once its start hooks have
// run, before its job workers start.
type recoveryHub struct {
	*addHub
	comp *carrypatch.Component

	mu      sync.Mutex
	atStart map[string]string
}

func startRecoveryHub(t *testing.T) *recoveryHub {
	t.Helper()
	h := &recoveryHub{addHub: &addHub{readHub: &readHub{}, now: time.Now().UTC().Truncate(time.Second)}}
	clock := hubtest.NewJobClock(h.clock)
	h.Hub = hubtest.StartWith(t, hubtest.Config{
		Now: h.clock,
		Compose: func(c *hub.Composition) {
			h.comp = c.CarryPatch()
			c.AdjustJobs(func(d *jobs.Deps) { d.Clock, d.NoWake = clock, true })
			c.AdjustCarryPatch(func(d *carrypatch.Deps) { h.repoPath = d.RepoPath })
			c.Jobs().Register(carrypatch.RebuildJobType, &attemptRunner{runs: map[string]*attemptRun{}})
			c.AddStartHook("test-job-status", func(ctx context.Context, rt *hub.Runtime) error {
				rows, err := rt.Store.Main().QueryContext(ctx, `SELECT id, status FROM jobs`)
				if err != nil {
					return err
				}
				defer func() { _ = rows.Close() }()
				h.mu.Lock()
				defer h.mu.Unlock()
				h.atStart = map[string]string{}
				for rows.Next() {
					var id, status string
					if err := rows.Scan(&id, &status); err != nil {
						return err
					}
					h.atStart[id] = status
				}
				return rows.Err()
			})
		},
	})
	hubtest.WaitClosed(t, clock.Armed(jobs.DefaultInterval), "the job queue's first pass")
	h.owner = h.CreateUser(t, "owner")
	h.key = h.IssueKey(t, h.owner)
	h.org = h.CreateOrg(t, "acme")
	h.AddMember(t, h.org, h.owner)
	return h
}

// interrupted stores the ready carry-patch workspace slug with a copy, and
// leaves it as a rebuild a stop interrupted leaves it: its job running and
// its scratch working tree in the copy. It returns the job's id and the
// copy's directory.
func (h *recoveryHub) interrupted(t *testing.T, slug string) (string, string) {
	t.Helper()
	ctx := context.Background()
	dir := h.carry(t, slug, func(w *workspace.Workspace) { w.IntegrationBranch = new("deploy") })
	res, err := h.comp.EnqueueRebuild(ctx, slug, carrypatch.RebuildOptions{ActorType: event.ActorSystem, SubmittedBy: event.ActorSystem})
	if err != nil || !res.Queued {
		t.Fatalf("queue a rebuild of %s = %+v, %v; want a queued job", slug, res, err)
	}
	if _, err := h.Store.Main().ExecContext(ctx, `UPDATE jobs SET status = ? WHERE id = ?`, jobs.StatusRunning, res.JobID); err != nil {
		t.Fatal(err)
	}
	leaveScratch(t, dir, res.JobID)
	return res.JobID, dir
}

// leaveScratch makes the scratch working tree name in the repository dir,
// at its branch p1, and leaves it there.
func leaveScratch(t *testing.T, dir, name string) {
	t.Helper()
	ctx := context.Background()
	repo, err := (gitrepo.Git{}).Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AddScratch(ctx, name, refIn(dir, "refs/heads/p1")); err != nil {
		t.Fatalf("make the scratch working tree %s in %s: %v", name, dir, err)
	}
}

// statusAtStart is the status the job id had once the start hooks of the
// hub's last start had run.
func (h *recoveryHub) statusAtStart(id string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.atStart[id]
}

// clientRefs lists the refs of the repository dir a client reads or the
// hub keeps for one, under refs/heads/, refs/remotes/ and
// refs/parley/replaced/, each with its id.
func clientRefs(t *testing.T, dir string) string {
	t.Helper()
	return gitrepotest.Git(t, dir, "for-each-ref", "--format=%(refname) %(objectname)",
		"refs/heads/", "refs/remotes/", "refs/parley/replaced/")
}

// loggedAbout reports whether a line of the hub's JSON log names the
// workspace slug.
func loggedAbout(log, slug string) bool {
	for line := range strings.Lines(log) {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["workspace"] == slug {
			return true
		}
	}
	return false
}
