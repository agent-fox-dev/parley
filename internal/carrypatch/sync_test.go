package carrypatch_test

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The upstream fetch runs after origin's and before the sync creates or
// moves any branch, and brings every upstream branch but no upstream tag;
// a missing integration branch is created at the upstream base.
func TestSync_FetchesUpstreamBeforeMoves(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	gitrepotest.Git(t, f.copy, "update-ref", "-d", "refs/heads/deploy")
	next := f.upstream.Commit("next", "N1")
	f.upstream.Tag("up-only", "main")
	feat2 := f.originAhead(t, "feat", "origin")

	var mu sync.Mutex
	var atFetch map[string]string
	var fetchErr error
	f.fetches.before(func() {
		heads, err := headsIn(f.copy)
		mu.Lock()
		defer mu.Unlock()
		atFetch, fetchErr = heads, err
	})
	before := f.heads(t)
	f.mustSync(t)

	mu.Lock()
	defer mu.Unlock()
	if fetchErr != nil {
		t.Fatalf("read the branches at the upstream fetch: %v", fetchErr)
	}
	if atFetch == nil || !maps.Equal(atFetch, before) {
		t.Errorf("branches at the upstream fetch = %v, want those before the sync %v", atFetch, before)
	}
	for branch, want := range map[string]string{"main": f.upstream.Head("main"), "next": next} {
		if got := refIn(f.copy, "refs/remotes/upstream/"+branch); got != want {
			t.Errorf("refs/remotes/upstream/%s = %q, want upstream's %s", branch, got, want)
		}
	}
	if got := refIn(f.copy, "refs/tags/up-only"); got != "" {
		t.Errorf("the upstream tag up-only is in the hub at %s, want no tag from upstream", got)
	}
	if got := refIn(f.copy, "refs/heads/feat"); got != feat2 {
		t.Errorf("feat = %q, want origin's %s", got, feat2)
	}
	if got := refIn(f.copy, "refs/heads/deploy"); got != f.upstream.Head("main") {
		t.Errorf("the integration branch deploy = %q, want it created at the upstream base %s", got, f.upstream.Head("main"))
	}
}

// The upstream credential is read again for every sync, so a changed
// secret counts at the next sync.
func TestSync_UpstreamCredentialsResolvedPerSync(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	const tokenOne, tokenTwo = "first-upstream-token-0f3a", "second-upstream-token-9c1e"
	f.upstream.Accept("x-token-auth", tokenTwo)
	f.secrets.set(carrypatch.SecretUpstreamGitPAT, tokenOne)
	if r := f.sync(t, ""); r.status != http.StatusBadGateway {
		t.Fatalf("sync with the refused token = %d %s, want 502", r.status, r.raw)
	}
	f.secrets.set(carrypatch.SecretUpstreamGitPAT, tokenTwo)
	if r := f.sync(t, ""); r.status != http.StatusOK {
		t.Fatalf("sync with the changed token = %d %s, want 200", r.status, r.raw)
	}
}

// A sync whose upstream cannot be fetched is refused as a fetch failure
// that names upstream, with nothing created, moved, written, emitted or
// queued, a missing integration branch included.
func TestSync_UpstreamFetchFailure(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	gitrepotest.Git(t, f.copy, "update-ref", "-d", "refs/heads/deploy")
	f.setSettings(t, map[string]any{"patch_branch_source": "origin"})
	f.originAhead(t, "feat", "origin")
	p := f.addPatch(t, "patch/a")
	f.originAhead(t, "patch/a", "origin")
	f.setColumn(t, "upstream_url", strings.Replace(f.upstream.URL(), "https://", "https://bot:s3cret@", 1))
	const refused = "refused-upstream-token-7d2b"

	// The rows run in order on one workspace; the last closes the upstream.
	for _, tc := range []struct {
		name    string
		arrange func()
	}{
		{"upstream refuses the credential", func() {
			f.upstream.Accept("x-token-auth", "accepted-upstream-token-4e8a")
			f.secrets.set(carrypatch.SecretUpstreamGitPAT, refused)
		}},
		{"the secret store fails when the upstream credentials are read", func() {
			f.secrets.failUpstream(errors.New("the secret store is unavailable"))
		}},
		{"upstream unreachable", func() {
			f.secrets.failUpstream(nil)
			f.upstream.Close()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.arrange()
			before, ws := f.heads(t), f.record(t)
			r := f.sync(t, "")
			if r.status != http.StatusBadGateway || r.Error.ErrorType != "upstream_error" {
				t.Fatalf("sync = %d %s, want 502 upstream_error", r.status, r.raw)
			}
			msg := r.Error.Message
			if !strings.Contains(msg, "upstream") || strings.Contains(msg, "s3cret") || strings.Contains(msg, "bot:") || strings.Contains(msg, refused) {
				t.Errorf("message = %q, want one that names upstream and holds no credential", msg)
			}
			after := f.record(t)
			if str(after.SyncError) != msg {
				t.Errorf("sync_error = %q, want the message %q", str(after.SyncError), msg)
			}
			if !equalTime(after.LastSyncAt, ws.LastSyncAt) || str(after.UpstreamHeadSHA) != str(ws.UpstreamHeadSHA) {
				t.Errorf("last_sync_at, upstream_head_sha = %v, %s, want %v, %s unchanged",
					after.LastSyncAt, str(after.UpstreamHeadSHA), ws.LastSyncAt, str(ws.UpstreamHeadSHA))
			}
			if got := f.heads(t); !maps.Equal(got, before) {
				t.Errorf("branches = %v, want %v unchanged", got, before)
			}
			if got := refIn(f.copy, "refs/heads/deploy"); got != "" {
				t.Errorf("the integration branch deploy = %s, want it still missing", got)
			}
			if got := f.stored(t, p.ID).OriginSyncedAt; got != nil {
				t.Errorf("origin_synced_at of patch/a = %v, want it unwritten", got)
			}
			for _, typ := range []string{"parley.patch.%", "parley.workspace.sync", "parley.rebuild.%"} {
				if n := f.eventCount(t, typ); n != 0 {
					t.Errorf("%d events of type %s, want none", n, typ)
				}
			}
			if js := f.rebuildJobs(t); len(js) != 0 {
				t.Errorf("rebuild jobs = %v, want none", js)
			}
		})
	}
}

// Two syncs of one workspace fetch upstream into its copy one after the
// other, never at once, where both would lock the same refs and one would
// fail.
func TestSync_UpstreamFetchesTakeTurns(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	// The first fetch waits for the second to start, for a second at most.
	second := make(chan struct{})
	var calls atomic.Int32
	f.fetches.before(func() {
		if calls.Add(1) == 1 {
			select {
			case <-second:
			case <-time.After(time.Second):
			}
			return
		}
		close(second)
	})
	waits := []func() syncAnswer{f.startSync(t), f.startSync(t)}
	for _, wait := range waits {
		if r := wait(); r.status != http.StatusOK {
			t.Errorf("sync = %d %s, want 200", r.status, r.raw)
		}
	}
	if n := f.fetches.count(); n != 2 {
		t.Errorf("upstream fetches = %d, want 2", n)
	}
	if n := f.fetches.mostAtOnce(); n != 1 {
		t.Errorf("%d upstream fetches ran at once, want 1", n)
	}
}

// equalTime is whether a and b are both nil or the same time.
func equalTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// A failed origin fetch names origin and never reaches upstream.
func TestSync_OriginFailureNamesOrigin(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	f.origin.Close()
	r := f.sync(t, "")
	if r.status != http.StatusBadGateway || r.Error.ErrorType != "upstream_error" {
		t.Fatalf("sync = %d %s, want 502 upstream_error", r.status, r.raw)
	}
	if !strings.Contains(r.Error.Message, "origin") {
		t.Errorf("message = %q, want one that names origin", r.Error.Message)
	}
	if got := str(f.record(t).SyncError); !strings.Contains(got, "origin") {
		t.Errorf("sync_error = %q, want one that names origin", got)
	}
	if n := f.fetches.count(); n != 0 {
		t.Errorf("upstream fetches = %d, want none", n)
	}
}

// An upstream fetch after which no upstream base resolves fails the sync
// as a failed upstream fetch does, without creating a missing integration
// branch.
func TestSync_NoUpstreamBase(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	gitrepotest.Git(t, f.copy, "update-ref", "-d", "refs/heads/deploy")
	f.originAhead(t, "feat", "origin")
	// Upstream keeps a branch, so it can be served, but neither a default
	// branch nor one named like the workspace's default branch main.
	f.upstream.Branch("other", "main")
	gitrepotest.Git(t, f.upstream.Dir(), "update-ref", "-d", "refs/heads/main")
	f.upstream.Publish()

	before, ws := f.heads(t), f.record(t)
	r := f.sync(t, "")
	if r.status != http.StatusBadGateway || r.Error.ErrorType != "upstream_error" {
		t.Fatalf("sync = %d %s, want 502 upstream_error", r.status, r.raw)
	}
	if !strings.Contains(r.Error.Message, "upstream base cannot be resolved") {
		t.Errorf("message = %q, want it to say the upstream base cannot be resolved", r.Error.Message)
	}
	after := f.record(t)
	if !strings.Contains(str(after.SyncError), "upstream base cannot be resolved") {
		t.Errorf("sync_error = %q, want it to say the upstream base cannot be resolved", str(after.SyncError))
	}
	if got := f.heads(t); !maps.Equal(got, before) {
		t.Errorf("branches = %v, want %v unchanged", got, before)
	}
	if got := refIn(f.copy, "refs/heads/deploy"); got != "" {
		t.Errorf("the integration branch deploy = %s, want it still missing", got)
	}
	if !equalTime(after.LastSyncAt, ws.LastSyncAt) {
		t.Errorf("last_sync_at = %v, want %v unchanged", after.LastSyncAt, ws.LastSyncAt)
	}
}

// A sync the route refuses contacts neither origin nor upstream.
func TestSync_RefusedRequestDoesNotFetchUpstream(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	// The rows run in order on one workspace.
	for _, tc := range []struct {
		name      string
		arrange   func(t *testing.T)
		status    int
		errorType string
	}{
		{"sync disabled", func(t *testing.T) { f.setSettings(t, map[string]any{"sync_enabled": false}) },
			http.StatusBadRequest, "sync_disabled"},
		{"archived", func(t *testing.T) {
			f.setSettings(t, map[string]any{"sync_enabled": true})
			f.setState(t, f.slug, workspace.StatusArchived, workspace.MarkerNone)
		}, http.StatusConflict, "workspace_archived"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.arrange(t)
			originRequests := f.origin.Requests()
			r := f.sync(t, "")
			if r.status != tc.status || r.Error.ErrorType != tc.errorType {
				t.Fatalf("sync = %d %s, want %d %s", r.status, r.raw, tc.status, tc.errorType)
			}
			if n := f.fetches.count(); n != 0 {
				t.Errorf("upstream fetches = %d, want none", n)
			}
			if n := f.origin.Requests() - originRequests; n != 0 {
				t.Errorf("origin got %d requests, want none", n)
			}
		})
	}
}

// Each sync reports whether the upstream base moved since the stored head,
// and whether it moved by a force-push, and completes either way.
func TestSync_UpstreamMovement(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	// The rows are consecutive syncs of one workspace.
	for _, tc := range []struct {
		name            string
		arrange         func(t *testing.T)
		advanced, force bool
	}{
		{"first sync, nothing stored", func(*testing.T) {}, true, false},
		{"no upstream change", func(*testing.T) {}, false, false},
		{"upstream gains a commit", func(t *testing.T) {
			f.moveUpstream(t, "U2", map[string]string{"u2.txt": "2\n"})
		}, true, false},
		{"upstream main is rewritten", func(t *testing.T) {
			f.upstream.Branch("main", gitrepotest.Commit(t, f.upstream.Dir(), "rewrite", map[string]string{"rewrite.txt": "r\n"}))
		}, true, true},
		{"the stored head is not in the hub", func(t *testing.T) {
			f.setColumn(t, "upstream_head_sha", strings.Repeat("ab", 20))
			f.moveUpstream(t, "U3", map[string]string{"u3.txt": "3\n"})
		}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.arrange(t)
			r := f.mustSync(t)
			if r.body["upstream_advanced"] != tc.advanced || r.body["force_push_detected"] != tc.force {
				t.Errorf("upstream_advanced, force_push_detected = %v, %v, want %v, %v",
					r.body["upstream_advanced"], r.body["force_push_detected"], tc.advanced, tc.force)
			}
		})
	}
}

// A completed sync records the upstream base as upstream_head_sha when it
// moved, beside last_sync_at, and keeps it when it did not.
func TestSync_WritesUpstreamHead(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	t1 := f.clock()
	r1 := f.mustSync(t)
	head, _ := r1.body["upstream_head_sha"].(string)
	if head != f.upstream.Head("main") || len(head) != 40 {
		t.Errorf("first sync's upstream_head_sha = %q, want upstream's main %s", head, f.upstream.Head("main"))
	}
	if got := r1.body["last_sync_at"]; got != timefmt.Format(t1) {
		t.Errorf("first sync's last_sync_at = %v, want %s", got, timefmt.Format(t1))
	}

	f.advance(time.Minute)
	t2 := f.clock()
	r2 := f.mustSync(t)
	if r2.body["upstream_advanced"] != false {
		t.Errorf("second sync's upstream_advanced = %v, want false", r2.body["upstream_advanced"])
	}
	if got := r2.body["upstream_head_sha"]; got != head {
		t.Errorf("second sync's upstream_head_sha = %v, want %s kept", got, head)
	}
	if got := r2.body["last_sync_at"]; got != timefmt.Format(t2) {
		t.Errorf("second sync's last_sync_at = %v, want %s", got, timefmt.Format(t2))
	}
	if got := str(f.record(t).UpstreamHeadSHA); got != head {
		t.Errorf("stored upstream_head_sha = %s, want %s", got, head)
	}
}

// The per-branch pass leaves the branches of live patches alone, whether
// origin is ahead of or diverged from them, with or without a reset to
// origin, while it still syncs every other branch.
func TestSync_PatchBranchesLeftOutOfBranchPass(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	f.setSettings(t, map[string]any{"patch_branch_source": "hub"})
	a := f.addPatch(t, "patch/a")
	f.addPatch(t, "patch/b", withStatus(carrypatch.StatusDisabled))
	f.originAhead(t, "patch/a", "origin")
	f.originBranch(t, "patch/b", f.commitOn(t, f.base, "B on origin", map[string]string{"b.txt": "origin\n"}))
	f.originBranch(t, "deploy", f.commitOn(t, f.base, "D on origin", map[string]string{"d.txt": "origin\n"}))
	f.originAhead(t, "feat", "origin")

	// The rows are consecutive syncs of one workspace.
	for _, query := range []string{"reset_to_origin=false", "reset_to_origin=true"} {
		t.Run(query, func(t *testing.T) {
			before := f.heads(t)
			r := f.sync(t, query)
			if r.status != http.StatusOK {
				t.Fatalf("sync = %d %s, want 200", r.status, r.raw)
			}
			results := r.branches(t)
			if _, ok := results["feat"]; !ok {
				t.Errorf("branches = %v, want an entry for feat", results)
			}
			for _, name := range []string{a.BranchName, "patch/b", "deploy"} {
				if res, ok := results[name]; ok {
					t.Errorf("branches has %s with %s, want it left out", name, res)
				}
			}
			after := f.heads(t)
			for _, name := range []string{a.BranchName, "patch/b"} {
				if after[name] != before[name] {
					t.Errorf("%s = %s, want %s unchanged", name, after[name], before[name])
				}
			}
		})
	}
}

// The branch of a deleted patch is an ordinary branch of the per-branch
// pass.
func TestSync_DeletedPatchBranchIsOrdinary(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	gone := gitrepotest.Commit(t, f.copy, "G1", map[string]string{"gone.txt": "1\n"}, f.base)
	gitrepotest.SetRef(t, f.copy, "refs/heads/patch/gone", gone)
	f.put(t, deletedPatch(f.slug, "patch/gone", f.clock()))
	theirs := f.originAhead(t, "patch/gone", "origin")

	r := f.mustSync(t)
	if got := r.branches(t)["patch/gone"]; got != string(workspace.BranchFastForward) {
		t.Errorf("patch/gone's result = %q, want %s", got, workspace.BranchFastForward)
	}
	if got := refIn(f.copy, "refs/heads/patch/gone"); got != theirs {
		t.Errorf("patch/gone = %s, want origin's %s", got, theirs)
	}
}

// A sync queues a rebuild, in the workspace's strategy and fail mode and
// submitted by the caller, when auto_rebuild_after_sync is on and upstream
// advanced, the refresh moved the branch of an active or conflict patch,
// or a patch was found merged; in every other case it queues none.
func TestSync_RebuildTrigger(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	f.setSettings(t, map[string]any{"rebuild_strategy": "merge", "rebuild_fail_mode": "continue",
		"patch_branch_source": "origin", "patch_divergence_policy": "replace"})
	f.addPatch(t, "patch/a")
	f.addPatch(t, "patch/c", withStatus(carrypatch.StatusConflict, "c.txt"))
	f.addPatch(t, "patch/d", withStatus(carrypatch.StatusDisabled))
	f.mustSync(t)

	// The rows are consecutive syncs of one workspace, each from a
	// workspace with no rebuild job.
	for _, tc := range []struct {
		name    string
		auto    bool
		arrange func(t *testing.T)
		// moved is the patch branch the refresh fast-forwards, merged the
		// patch the sync finds merged, "" for none.
		moved, merged string
		triggered     bool
	}{
		{"upstream advanced", true, func(t *testing.T) {
			f.moveUpstream(t, "U2", map[string]string{"u2.txt": "2\n"})
		}, "", "", true},
		{"origin moved the branch of an active patch", true, func(t *testing.T) {
			f.originAhead(t, "patch/a", "origin")
		}, "patch/a", "", true},
		{"origin moved the branch of a conflict patch", true, func(t *testing.T) {
			f.originAhead(t, "patch/c", "origin")
		}, "patch/c", "", true},
		{"origin moved only the branch of a disabled patch", true, func(t *testing.T) {
			f.originAhead(t, "patch/d", "origin")
		}, "patch/d", "", false},
		{"nothing changed", true, func(*testing.T) {}, "", "", false},
		{"upstream advanced with auto_rebuild_after_sync off", false, func(t *testing.T) {
			f.moveUpstream(t, "U3", map[string]string{"u3.txt": "3\n"})
		}, "", "", false},
		{"upstream advanced and a patch merged with auto_rebuild_after_sync off", false, func(t *testing.T) {
			m := f.addPatchOn(t, "patch/m", f.upstream.Head("main"))
			f.upstreamTo(t, refIn(f.copy, "refs/heads/"+m.BranchName))
		}, "", "patch/m", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.dropRebuilds(t)
			f.setSettings(t, map[string]any{"auto_rebuild_after_sync": tc.auto})
			tc.arrange(t)
			r := f.mustSync(t)
			if tc.moved != "" {
				if got := r.syncedOf(t, tc.moved).Action; got != "fast_forwarded" {
					t.Fatalf("%s's refresh action = %s, want fast_forwarded", tc.moved, got)
				}
			}
			if tc.merged != "" {
				wantMerged(t, r, tc.merged)
			}
			if !tc.triggered {
				wantNoRebuild(t, r)
				if js := f.rebuildJobs(t); len(js) != 0 {
					t.Errorf("rebuild jobs = %v, want none", js)
				}
				return
			}
			job := f.wantRebuildQueued(t, r)
			if job["submitted_by"] != f.owner || job["strategy"] != "merge" || job["fail_mode"] != "continue" {
				t.Errorf("the rebuild is submitted by %v with %v and %v, want %s with merge and continue",
					job["submitted_by"], job["strategy"], job["fail_mode"], f.owner)
			}
		})
	}
}

// A sync that would queue a rebuild while the workspace has one queued,
// running or failed queues none and still completes.
func TestSync_RebuildSkippedOnDuplicate(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	id, _ := f.wantRebuildQueued(t, f.mustSync(t))["id"].(string)

	// The rows are consecutive syncs of one workspace, each after upstream
	// advanced, with the first sync's rebuild in the row's status.
	for _, status := range []string{"queued", "running", "failed"} {
		t.Run(status, func(t *testing.T) {
			if _, err := f.Store.Main().Exec(`UPDATE jobs SET status = ? WHERE id = ?`, status, id); err != nil {
				t.Fatal(err)
			}
			f.moveUpstream(t, status, map[string]string{status + ".txt": status + "\n"})
			f.advance(time.Minute)
			now := f.clock()
			r := f.mustSync(t)
			wantNoRebuild(t, r)
			if js := f.rebuildJobs(t); len(js) != 1 || js[0].ID != id || js[0].Status != status {
				t.Errorf("rebuild jobs = %v, want the %s job %s alone", js, status, id)
			}
			if got := r.body["upstream_head_sha"]; got != f.upstream.Head("main") {
				t.Errorf("upstream_head_sha = %v, want upstream's main %s", got, f.upstream.Head("main"))
			}
			if got := r.body["last_sync_at"]; got != timefmt.Format(now) {
				t.Errorf("last_sync_at = %v, want %s", got, timefmt.Format(now))
			}
		})
	}
}

// A sync queues its rebuild also when no patch is active or conflict,
// which the rebuild route would refuse.
func TestSync_RebuildQueuedWithoutActivePatches(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	// The rows are consecutive syncs of one workspace, each after upstream
	// advanced.
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T)
	}{
		{"no patch at all", func(*testing.T) {}},
		{"the only patch is disabled", func(t *testing.T) {
			f.addPatch(t, "patch/d", withStatus(carrypatch.StatusDisabled))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.dropRebuilds(t)
			tc.arrange(t)
			f.moveUpstream(t, tc.name, map[string]string{"up.txt": tc.name + "\n"})
			f.wantRebuildQueued(t, f.mustSync(t))
		})
	}
}

// failingRebuilds is a rebuild enqueuer that, once armed, fails its next
// call with a store error, and passes every other call on.
type failingRebuilds struct {
	carrypatch.RebuildEnqueuer
	armed atomic.Bool
}

func (r *failingRebuilds) EnqueueRebuild(ctx context.Context, slug string, opts carrypatch.RebuildOptions) (carrypatch.RebuildResult, error) {
	if r.armed.CompareAndSwap(true, false) {
		return carrypatch.RebuildResult{}, errors.New("queue a rebuild: database disk image is malformed")
	}
	return r.RebuildEnqueuer.EnqueueRebuild(ctx, slug, opts)
}

// A sync whose rebuild cannot be queued answers 500 and records no
// completion, so that the next sync sees the same upstream movement and
// queues the rebuild.
func TestSync_EnqueueFailure(t *testing.T) {
	t.Parallel()
	rebuilds := &failingRebuilds{}
	f := startSyncFixtureWith(t, syncOptions{compose: func(c *hub.Composition) {
		rebuilds.RebuildEnqueuer = c.CarryPatch()
		c.CarryPatch().RegisterRebuildEnqueuer(rebuilds)
	}})
	f.mustSync(t)
	f.dropRebuilds(t)
	f.moveUpstream(t, "U2", map[string]string{"u2.txt": "2\n"})
	f.advance(time.Minute)
	rebuilds.armed.Store(true)

	w0 := f.record(t)
	r1 := f.sync(t, "")
	if r1.status != http.StatusInternalServerError || r1.Error.ErrorType != "internal" {
		t.Fatalf("sync = %d %s, want 500 internal", r1.status, r1.raw)
	}
	w1 := f.record(t)
	if str(w1.UpstreamHeadSHA) != str(w0.UpstreamHeadSHA) || !equalTime(w1.LastSyncAt, w0.LastSyncAt) {
		t.Errorf("upstream_head_sha, last_sync_at = %s, %v, want %s, %v unchanged",
			str(w1.UpstreamHeadSHA), w1.LastSyncAt, str(w0.UpstreamHeadSHA), w0.LastSyncAt)
	}

	r2 := f.mustSync(t)
	if r2.body["upstream_advanced"] != true {
		t.Errorf("the next sync's upstream_advanced = %v, want true", r2.body["upstream_advanced"])
	}
	f.wantRebuildQueued(t, r2)
}

// syncMembers are the eight members a sync of a carry-patch workspace adds
// to the answer of the general sync.
var syncMembers = []string{"upstream_advanced", "force_push_detected", "origin_fetched", "patches_merged",
	"patches_synced", "patches_diverged", "rebuild_triggered", "rebuild_job_id"}

// A completed sync answers the workspace object and branches of the
// general sync with the eight carry-patch members, every list a list also
// when it names nothing and rebuild_job_id null when no rebuild was queued.
func TestSync_AnswerShape(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	f.mustSync(t)
	r := f.mustSync(t)
	if r.body["slug"] != f.slug {
		t.Errorf("slug = %v, want %s", r.body["slug"], f.slug)
	}
	if _, ok := r.body["branches"].([]any); !ok {
		t.Errorf("branches = %v, want a list", r.body["branches"])
	}
	for member, want := range map[string]any{"upstream_advanced": false, "force_push_detected": false,
		"origin_fetched": true, "rebuild_triggered": false} {
		if got := r.body[member]; got != want {
			t.Errorf("%s = %v, want %v", member, got, want)
		}
	}
	raw := r.members(t)
	for member, want := range map[string]string{"patches_merged": "[]", "patches_synced": "[]",
		"patches_diverged": "[]", "rebuild_job_id": "null"} {
		if got, ok := raw[member]; !ok || string(got) != want {
			t.Errorf("%s = %s (present %v), want %s", member, got, ok, want)
		}
	}
}

// The branches of the patches the refresh leaves diverged are listed in
// patches_diverged, in position order, and the sync still completes.
func TestSync_PatchesDiverged(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	f.setSettings(t, originAuthority("report"))
	for _, branch := range []string{"patch/a", "patch/b", "patch/c"} {
		f.addPatch(t, branch)
		f.originBranch(t, branch, refIn(f.copy, "refs/heads/"+branch))
	}
	f.mustSync(t)
	for _, branch := range []string{"patch/b", "patch/c"} {
		f.originBranch(t, branch, f.commitOn(t, f.base, branch+" on origin", map[string]string{"origin.txt": branch + "\n"}))
	}
	f.dropRebuilds(t)

	r := f.mustSync(t)
	if got := answerList[string](t, r, "patches_diverged"); !slices.Equal(got, []string{"patch/b", "patch/c"}) {
		t.Errorf("patches_diverged = %q, want [patch/b patch/c]", got)
	}
	wantNoRebuild(t, r)

	f.setSettings(t, originAuthority("replace"))
	if got := answerList[string](t, f.mustSync(t), "patches_diverged"); len(got) != 0 {
		t.Errorf("patches_diverged under replace = %q, want []", got)
	}
}

// A sync of a standard workspace answers as the general sync does, with
// none of the carry-patch members, and contacts no upstream.
func TestSync_StandardWorkspaceUnchanged(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	f.setColumn(t, "workspace_mode", workspace.ModeStandard)
	r := f.mustSync(t)
	for _, member := range syncMembers {
		if _, ok := r.body[member]; ok {
			t.Errorf("the answer has %s, want none of the carry-patch members", member)
		}
	}
	if n := f.fetches.count(); n != 0 {
		t.Errorf("upstream fetches = %d, want none", n)
	}
	if js := f.rebuildJobs(t); len(js) != 0 {
		t.Errorf("rebuild jobs = %v, want none", js)
	}
}

// A caller that leaves while the sync runs leaves the carry-patch part to
// run to its end, as for a caller that waited.
func TestSync_CompletesAfterDisconnect(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	f.setSettings(t, originAuthority("replace"))
	a := f.addPatch(t, "patch/a")
	theirs := f.originAhead(t, a.BranchName, "origin")
	f.moveUpstream(t, "U2", map[string]string{"u2.txt": "2\n"})
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.fetches.before(func() {
		once.Do(func() { close(started) })
		<-release
	})

	f.syncAndLeave(t, started)
	close(release)
	f.waitSynced(t)

	if got := refIn(f.copy, "refs/heads/patch/a"); got != theirs {
		t.Errorf("patch/a = %s, want origin's %s", got, theirs)
	}
	if got := str(f.stored(t, a.ID).OriginSyncState); got != "in_sync" {
		t.Errorf("origin_sync_state of patch/a = %s, want in_sync", got)
	}
	w := f.record(t)
	if str(w.UpstreamHeadSHA) != f.upstream.Head("main") || w.LastSyncAt == nil {
		t.Errorf("upstream_head_sha, last_sync_at = %s, %v, want upstream's main %s and a time", str(w.UpstreamHeadSHA), w.LastSyncAt, f.upstream.Head("main"))
	}
	if n := f.eventCount(t, carrypatch.EventPatchSync); n != 1 {
		t.Errorf("%d parley.patch.sync events, want 1", n)
	}
	if js := f.rebuildJobs(t); len(js) != 1 || js[0].Status != "queued" {
		t.Errorf("rebuild jobs = %v, want one queued", js)
	}
}
