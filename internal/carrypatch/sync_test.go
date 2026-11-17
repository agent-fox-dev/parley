package carrypatch_test

import (
	"errors"
	"maps"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The upstream fetch runs after origin's and before the sync creates or
// moves any branch, and brings every upstream branch but no upstream tag.
func TestSync_FetchesUpstreamBeforeMoves(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	next := f.upstream.Commit("next", "N1")
	f.upstream.Tag("up-only", "main")
	feat1 := gitrepotest.Commit(t, f.copy, "F1", map[string]string{"feat.txt": "1\n"}, f.base)
	gitrepotest.Git(t, f.copy, "update-ref", "refs/heads/feat", feat1)
	feat2 := gitrepotest.Commit(t, f.copy, "F2", map[string]string{"feat.txt": "2\n"}, feat1)
	f.originBranch(t, "feat", feat2)

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
// queued.
func TestSync_UpstreamFetchFailure(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	f.setSettings(t, map[string]any{"patch_branch_source": "origin"})
	feat1 := gitrepotest.Commit(t, f.copy, "F1", map[string]string{"feat.txt": "1\n"}, f.base)
	gitrepotest.Git(t, f.copy, "update-ref", "refs/heads/feat", feat1)
	f.originBranch(t, "feat", gitrepotest.Commit(t, f.copy, "F2", map[string]string{"feat.txt": "2\n"}, feat1))
	p := f.addPatch(t, "patch/a")
	f.originBranch(t, "patch/a", gitrepotest.Commit(t, f.copy, "A2", map[string]string{"a.txt": "2\n"}, refIn(f.copy, "refs/heads/patch/a")))
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
// as a failed upstream fetch does.
func TestSync_NoUpstreamBase(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	feat1 := gitrepotest.Commit(t, f.copy, "F1", map[string]string{"feat.txt": "1\n"}, f.base)
	gitrepotest.Git(t, f.copy, "update-ref", "refs/heads/feat", feat1)
	f.originBranch(t, "feat", gitrepotest.Commit(t, f.copy, "F2", map[string]string{"feat.txt": "2\n"}, feat1))
	// Upstream keeps a branch, so it can be served, but neither a default
	// branch nor one named like the workspace's default branch main.
	f.upstream.Branch("other", "main")
	gitrepotest.Git(t, f.upstream.Dir(), "update-ref", "-d", "refs/heads/main")
	gitrepotest.Git(t, f.upstream.Dir(), "update-server-info")

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
	aTip := refIn(f.copy, "refs/heads/patch/a")
	f.originBranch(t, "patch/a", gitrepotest.Commit(t, f.copy, "A2", map[string]string{"a.txt": "2\n"}, aTip))
	f.originBranch(t, "patch/b", gitrepotest.Commit(t, f.copy, "B on origin", map[string]string{"b.txt": "origin\n"}, f.base))
	f.originBranch(t, "deploy", gitrepotest.Commit(t, f.copy, "D on origin", map[string]string{"d.txt": "origin\n"}, f.base))
	feat1 := gitrepotest.Commit(t, f.copy, "F1", map[string]string{"feat.txt": "1\n"}, f.base)
	gitrepotest.Git(t, f.copy, "update-ref", "refs/heads/feat", feat1)
	f.originBranch(t, "feat", gitrepotest.Commit(t, f.copy, "F2", map[string]string{"feat.txt": "2\n"}, feat1))

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
	gitrepotest.Git(t, f.copy, "update-ref", "refs/heads/patch/gone", gone)
	f.put(t, deletedPatch(f.slug, "patch/gone", f.clock()))
	theirs := gitrepotest.Commit(t, f.copy, "G2", map[string]string{"gone.txt": "2\n"}, gone)
	f.originBranch(t, "patch/gone", theirs)

	r := f.mustSync(t)
	if got := r.branches(t)["patch/gone"]; got != string(workspace.BranchFastForward) {
		t.Errorf("patch/gone's result = %q, want %s", got, workspace.BranchFastForward)
	}
	if got := refIn(f.copy, "refs/heads/patch/gone"); got != theirs {
		t.Errorf("patch/gone = %s, want origin's %s", got, theirs)
	}
}
