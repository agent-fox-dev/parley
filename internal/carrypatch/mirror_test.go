package carrypatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The mirror of the patch branches an accepted request created or moved to
// origin, against a push-taking test origin, with the hub's store, event
// log, settings registry and secrets.

// mirrorHub is a patch hub whose carry-patch dependencies the mirror tests
// bind components of their own with, so that each workspace of a test has
// its own forward limit and rebuild enqueuer.
type mirrorHub struct {
	*patchHub
	deps carrypatch.Deps
}

func startMirrorHub(t *testing.T) *mirrorHub {
	t.Helper()
	h := &mirrorHub{}
	h.patchHub = startPatchHub(t, func(d *carrypatch.Deps) { h.deps = *d })
	return h
}

// mirrorCase is a ready carry-patch workspace of a mirror test, whose patch
// branches the hub mirrors to origin. Its origin takes the GIT_PAT
// forwardToken alone and holds main at m and patch/a at a0. The hub's copy shares origin's objects, as a clone does, and holds
// main, patch/a at a1, a commit on m that does not descend from a0, and
// patch/b at b1, with a0 recorded as origin's head of patch/a and nothing
// recorded for patch/b. The patches of patch/a and patch/b are active. Its
// component is bound to the hub's dependencies with a rebuild enqueuer of
// its own that notes origin's request count at each call.
type mirrorCase struct {
	slug          string
	origin        *gitrepotest.Origin
	copy          string
	m, a0, a1, b1 string
	patches       map[string]carrypatch.Patch
	comp          *carrypatch.Component
	rebuilds      *originRebuilds
}

// caseOptions are what a mirror test changes of its case: whether
// auto_rebuild_after_push is off, the forward limit of its component (0 for
// the default), and whether git_url names origin's directory rather than
// its served URL, which spares a case that does not test the transport the
// TLS and the CGI of every request.
type caseOptions struct {
	noRebuild bool
	limit     time.Duration
	local     bool
}

// newCase stores the workspace slug of a mirror case with o.
func (h *mirrorHub) newCase(t *testing.T, slug string, o caseOptions) *mirrorCase {
	t.Helper()
	c := &mirrorCase{slug: slug, origin: gitrepotest.SmartOrigin(t, gitrepotest.WithBasicAuth("x-token-auth", forwardToken)),
		copy: h.deps.RepoPath(slug)}
	c.rebuilds = &originRebuilds{origin: c.origin}
	if !o.local {
		h.trust(t, c.origin.CAFile())
	}
	h.origins.set(slug, forwardToken, nil)
	dir := c.origin.Dir()
	c.m = gitrepotest.Commit(t, dir, "main", map[string]string{"README": "main\n"})
	c.a0 = gitrepotest.Commit(t, dir, "patch a", map[string]string{"a.txt": "a0\n"}, c.m)
	gitrepotest.SetRef(t, dir, "refs/heads/main", c.m)
	gitrepotest.SetRef(t, dir, "refs/heads/patch/a", c.a0)
	gitrepotest.InitBare(t, c.copy, "main")
	alternates := filepath.Join(c.copy, "objects", "info", "alternates")
	if err := os.WriteFile(alternates, []byte(filepath.Join(dir, "objects")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.a1 = gitrepotest.Commit(t, c.copy, "patch a rewritten", map[string]string{"a.txt": "a1\n"}, c.m)
	c.b1 = gitrepotest.Commit(t, c.copy, "patch b", map[string]string{"b.txt": "b1\n"}, c.m)
	for ref, id := range map[string]string{
		"refs/heads/main": c.m, "refs/heads/patch/a": c.a1, "refs/heads/patch/b": c.b1,
		"refs/remotes/origin/main": c.m, "refs/remotes/origin/patch/a": c.a0,
	} {
		gitrepotest.SetRef(t, c.copy, ref, id)
	}
	url := c.origin.URL()
	if o.local {
		url = dir
	}
	settings := map[string]json.RawMessage{"patch_branch_source": json.RawMessage(`"hub"`), "push_patches_to_origin": json.RawMessage(`true`),
		"auto_rebuild_after_push": json.RawMessage(fmt.Sprint(!o.noRebuild))}
	h.add(t, slug, workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone, func(w *workspace.Workspace) {
		w.GitURL, w.IntegrationBranch, w.Settings = &url, new("deploy"), settings
	})
	c.patches = map[string]carrypatch.Patch{"patch/a": livePatch(slug, "patch/a", 1), "patch/b": livePatch(slug, "patch/b", 2)}
	h.put(t, c.patches["patch/a"], c.patches["patch/b"])
	deps := h.deps
	deps.ForwardLimit = o.limit
	c.comp = &carrypatch.Component{}
	c.comp.Bind(deps)
	c.comp.RegisterRebuildEnqueuer(c.rebuilds)
	return c
}

// trigger runs the component's trigger for updates, as a push's.
func (c *mirrorCase) trigger(ctx context.Context, updates ...pushrule.Update) {
	c.comp.Trigger().AfterUpdates(ctx, c.slug, updates, carrypatch.SourcePush, nil)
}

// recorded is the origin head the copy records for branch, "" for none.
func (c *mirrorCase) recorded(branch string) string {
	return gitrepotest.Ref(c.copy, "refs/remotes/origin/"+branch)
}

// update is the update of branch from before to after.
func update(branch, before, after string) pushrule.Update {
	return pushrule.Update{Ref: branchRef(branch), Before: before, After: after}
}

// originRebuilds is a recording rebuild enqueuer that notes, at each call,
// how many requests origin had received.
type originRebuilds struct {
	recordingRebuilds
	origin *gitrepotest.Origin
	seenMu sync.Mutex
	seen   []int
}

func (r *originRebuilds) EnqueueRebuild(ctx context.Context, slug string, opts carrypatch.RebuildOptions) (carrypatch.RebuildResult, error) {
	n := r.origin.Requests()
	r.seenMu.Lock()
	r.seen = append(r.seen, n)
	r.seenMu.Unlock()
	return r.recordingRebuilds.EnqueueRebuild(ctx, slug, opts)
}

func TestMirror_PushesTheUpdatedPatchBranchesToOriginAndNothingElse(t *testing.T) {
	t.Parallel()
	h := startMirrorHub(t)
	for i, tc := range []struct {
		name      string
		noRebuild bool
		// request leaves c's copy, origin and patch list as a request and
		// what came after it left them, and returns the request's updates
		// and the commits origin should then hold, by branch.
		request func(t *testing.T, c *mirrorCase) ([]pushrule.Update, map[string]string)
	}{
		{"a forced update of a patch branch and the create of another", false,
			func(t *testing.T, c *mirrorCase) ([]pushrule.Update, map[string]string) {
				return []pushrule.Update{update("patch/a", c.a0, c.a1), update("patch/b", pushrule.ZeroID, c.b1)},
					map[string]string{"patch/a": c.a1, "patch/b": c.b1}
			}},
		{"a patch branch the hub moved on from the request's commit", false,
			func(t *testing.T, c *mirrorCase) ([]pushrule.Update, map[string]string) {
				a2 := gitrepotest.Commit(t, c.copy, "patch a again", map[string]string{"a.txt": "a2\n"}, c.a1)
				gitrepotest.SetRef(t, c.copy, "refs/heads/patch/a", a2)
				return []pushrule.Update{update("patch/a", c.a0, c.a1)}, map[string]string{"patch/a": a2}
			}},
		{"auto_rebuild_after_push off", true,
			func(t *testing.T, c *mirrorCase) ([]pushrule.Update, map[string]string) {
				return []pushrule.Update{update("patch/a", c.a0, c.a1)}, map[string]string{"patch/a": c.a1}
			}},
		{"a request that wrote other refs as well", false,
			func(t *testing.T, c *mirrorCase) ([]pushrule.Update, map[string]string) {
				// Origin holds the integration branch, patch/b and the branch of
				// the deleted patch patch/old; the request deleted patch/b, moved
				// patch/old, created a branch and a tag, and moved patch/c, whose
				// branch the hub deleted since. The hub's integration branch is
				// not origin's.
				for ref, id := range map[string]string{"refs/heads/deploy": c.m, "refs/heads/patch/b": c.a0, "refs/heads/patch/old": c.m} {
					gitrepotest.SetRef(t, c.origin.Dir(), ref, id)
				}
				gitrepotest.Git(t, c.copy, "update-ref", "-d", "refs/heads/patch/b")
				for ref, id := range map[string]string{"refs/heads/deploy": c.b1, "refs/heads/patch/old": c.b1,
					"refs/heads/feature/x": c.b1, "refs/tags/v1": c.m} {
					gitrepotest.SetRef(t, c.copy, ref, id)
				}
				h.put(t, deletedPatch(c.slug, "patch/old", at(10, 0)), livePatch(c.slug, "patch/c", 3))
				return []pushrule.Update{
					update("patch/a", c.a0, c.a1), update("patch/b", c.b1, pushrule.ZeroID), update("patch/old", c.m, c.b1),
					update("feature/x", pushrule.ZeroID, c.b1), update("refs/tags/v1", pushrule.ZeroID, c.m),
					update("patch/c", pushrule.ZeroID, c.b1),
				}, map[string]string{"patch/a": c.a1}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := h.newCase(t, fmt.Sprintf("mirror-%d", i), caseOptions{noRebuild: tc.noRebuild})
			updates, pushed := tc.request(t, c)
			want := gitrepotest.Refs(t, c.origin.Dir(), "")
			for branch, id := range pushed {
				want["refs/heads/"+branch] = id
			}
			requests := c.origin.Requests()

			c.trigger(context.Background(), updates...)
			if got := gitrepotest.Refs(t, c.origin.Dir(), ""); !maps.Equal(got, want) {
				t.Errorf("origin's refs = %v, want %v", got, want)
			}
			for branch, id := range pushed {
				if got := c.recorded(branch); got != id {
					t.Errorf("the recorded origin head of %s = %.7s, want the pushed %.7s", branch, got, id)
				}
			}
			if evs := h.patchEvents(t, c.slug, carrypatch.EventPatchMirrorFailed); len(evs) != 0 {
				t.Errorf("%d mirror_failed events, want none: %v", len(evs), evs)
			}
			c.rebuilds.seenMu.Lock()
			seen := c.rebuilds.seen
			c.rebuilds.seenMu.Unlock()
			switch {
			case tc.noRebuild && len(seen) != 0:
				t.Errorf("the rebuild enqueue was called %d times, want 0", len(seen))
			case !tc.noRebuild && (len(seen) != 1 || seen[0] != requests):
				t.Errorf("the rebuild enqueue was called with origin at %v requests, want once at %d, before the mirror's first", seen, requests)
			}
		})
	}
}

func TestMirror_ReportsEachBranchItCouldNotPushAndGoesOnWithTheNext(t *testing.T) {
	t.Parallel()
	h := startMirrorHub(t)
	dead := gitrepotest.NewOrigin(t)
	dead.Close()
	for i, tc := range []struct {
		name  string
		limit time.Duration
		// fault makes the mirror of patch/a fail.
		fault func(t *testing.T, c *mirrorCase)
		// cancel, when positive, is how long after its start the trigger's
		// context ends.
		cancel time.Duration
		// failed are the branches with an event, in order, each with the
		// error want; bPushed is set where patch/b reaches origin.
		failed  []string
		want    string
		bPushed bool
	}{
		{name: "origin's branch moved from the recorded head",
			fault:  func(t *testing.T, c *mirrorCase) { gitrepotest.SetRef(t, c.origin.Dir(), "refs/heads/patch/a", c.m) },
			failed: []string{"patch/a"}, want: "origin moved", bPushed: true},
		{name: "a hook of origin refuses the branch with the credential in its text",
			fault:  func(t *testing.T, c *mirrorCase) { c.origin.RejectRef("refs/heads/patch/a", "frozen "+forwardToken) },
			failed: []string{"patch/a"}, want: "frozen [redacted]", bPushed: true},
		{name: "the origin credential cannot be read",
			fault:  func(t *testing.T, c *mirrorCase) { h.origins.set(c.slug, "", errors.New("secret store sealed")) },
			failed: []string{"patch/a", "patch/b"}, want: "failed to resolve origin credentials"},
		// The limit leaves patch/b's push, which runs within it too, room
		// on a loaded machine.
		{name: "origin holds the push past the limit", limit: time.Second,
			fault:  func(t *testing.T, c *mirrorCase) { c.origin.StallRef("refs/heads/patch/a") },
			failed: []string{"patch/a"}, want: "timed out", bPushed: true},
		{name: "nothing listens at origin's address",
			fault: func(t *testing.T, c *mirrorCase) {
				if _, err := h.Store.Main().Exec(`UPDATE workspaces SET git_url = ? WHERE slug = ?`, dead.URL(), c.slug); err != nil {
					t.Fatal(err)
				}
			},
			failed: []string{"patch/a", "patch/b"}, want: "origin unreachable"},
		{name: "the trigger's context ends while origin holds the push", cancel: 300 * time.Millisecond,
			fault: func(t *testing.T, c *mirrorCase) { c.origin.StallRef("refs/heads/patch/a") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Origin's directory serves every row but the one whose origin
			// does not listen: the transport is tested above.
			c := h.newCase(t, fmt.Sprintf("fail-%d", i), caseOptions{limit: tc.limit, local: true})
			tc.fault(t, c)
			ctx := context.Background()
			if tc.cancel > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.cancel)
				defer cancel()
			}
			start := time.Now()
			c.trigger(ctx, update("patch/a", c.a0, c.a1), update("patch/b", pushrule.ZeroID, c.b1))
			if took, bound := time.Since(start), tc.limit+tc.cancel+5*time.Second; took > bound {
				t.Errorf("the trigger returned after %v, want within %v", took, bound)
			}
			c.origin.Reject("")

			evs := h.patchEvents(t, c.slug, carrypatch.EventPatchMirrorFailed)
			if len(evs) != len(tc.failed) {
				t.Fatalf("%d mirror_failed events %v, want one for each of %v", len(evs), evs, tc.failed)
			}
			for i, ev := range evs {
				branch := tc.failed[i]
				if ev.ResourceType != "patch" || ev.ResourceID != c.patches[branch].ID || ev.ActorType != event.ActorSystem || ev.ActorID != nil {
					t.Errorf("event %d is of %s %s by %s %v, want of the patch %s by the system", i, ev.ResourceType, ev.ResourceID,
						ev.ActorType, str(ev.ActorID), c.patches[branch].ID)
				}
				if ev.Metadata["branch_name"] != branch || ev.Metadata["error"] != tc.want {
					t.Errorf("event %d has metadata %v, want branch_name %s and error %q", i, ev.Metadata, branch, tc.want)
				}
			}
			if got := c.recorded("patch/a"); got != c.a0 {
				t.Errorf("the recorded origin head of patch/a = %.7s, want it left at %.7s", got, c.a0)
			}
			if got := gitrepotest.Ref(c.copy, "refs/heads/patch/a"); got != c.a1 {
				t.Errorf("the hub's patch/a = %.7s, want it left at %.7s", got, c.a1)
			}
			want := ""
			if tc.bPushed {
				want = c.b1
			}
			if got := c.origin.Head("patch/b"); got != want {
				t.Errorf("origin's patch/b = %.7s, want %.7s", got, want)
			}
		})
	}
}

func TestMirror_MirrorsNothingUnlessTheHubIsTheAuthorityAndTheSettingIsOn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		settings map[string]any
	}{
		{"hub, push_patches_to_origin false", map[string]any{"patch_branch_source": "hub", "push_patches_to_origin": false}},
		{"hub, push_patches_to_origin missing", map[string]any{"patch_branch_source": "hub"}},
		{"origin, push_patches_to_origin false", map[string]any{"patch_branch_source": "origin", "push_patches_to_origin": false}},
		{"origin, push_patches_to_origin true", map[string]any{"patch_branch_source": "origin", "push_patches_to_origin": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newTrigger(t, tc.settings)
			f.after(t, "demo", []pushrule.Update{updateOf("patch/a")}, carrypatch.SourcePush, nil)
			f.mirrors.wantMirrors(t)
			if n := f.events.count.Load(); n != 0 {
				t.Errorf("the trigger wrote %d events, want none", n)
			}
		})
	}
}

func TestMirror_RunsTheMirrorsOfOneWorkspaceOneAtATime(t *testing.T) {
	t.Parallel()
	h := startMirrorHub(t)
	c := h.newCase(t, "demo", caseOptions{noRebuild: true, local: true})
	// Two heads for each of the two requests of a round, the pairs taken in
	// turn, so that every round moves the branch away from where the last
	// one left it.
	var heads [4]string
	for i := range heads {
		heads[i] = gitrepotest.Commit(t, c.copy, fmt.Sprintf("x%d", i), map[string]string{"a.txt": fmt.Sprintf("x%d\n", i)}, c.m)
	}
	tr := c.comp.Trigger()
	mirror := tr.Mirror
	var mu sync.Mutex
	running, most := 0, 0
	tr.Mirror = func(ctx context.Context, w workspace.Workspace, patches []carrypatch.Patch) {
		mu.Lock()
		running++
		most = max(most, running)
		mu.Unlock()
		defer func() {
			mu.Lock()
			running--
			mu.Unlock()
		}()
		mirror(ctx, w, patches)
	}
	before := c.a1
	for round := range 20 {
		x1, x2 := heads[2*(round%2)], heads[2*(round%2)+1]
		var done sync.WaitGroup
		for _, x := range []string{x1, x2} {
			gitrepotest.SetRef(t, c.copy, "refs/heads/patch/a", x)
			u := update("patch/a", before, x)
			before = x
			done.Go(func() {
				tr.AfterUpdates(context.Background(), c.slug, []pushrule.Update{u}, carrypatch.SourcePush, nil)
			})
		}
		done.Wait()
		if got := c.origin.Head("patch/a"); got != x2 {
			t.Fatalf("round %d: origin's patch/a = %.7s, want the hub's last head %.7s", round, got, x2)
		}
	}
	if evs := h.patchEvents(t, c.slug, carrypatch.EventPatchMirrorFailed); len(evs) != 0 {
		t.Errorf("%d mirror_failed events, want none: %v", len(evs), evs)
	}
	if most != 1 {
		t.Errorf("%d mirrors of the workspace pushed at once, want 1", most)
	}
}
