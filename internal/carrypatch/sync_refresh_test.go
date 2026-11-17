package carrypatch_test

import (
	"errors"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// originAuthority is the settings document of a workspace whose patch
// branches follow origin, with policy as its patch_divergence_policy.
func originAuthority(policy string) map[string]any {
	return map[string]any{"patch_branch_source": "origin", "patch_divergence_policy": policy}
}

// Under origin as the authority, every active, conflict or disabled patch
// gets the outcome of the first rule that fits its two tips, reported in
// position order, and its branch moves to origin's tip only for created,
// fast_forwarded and replaced.
func TestRefresh_Outcomes(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	f.setSettings(t, originAuthority("report"))
	commit := func(name, parent string) string {
		return gitrepotest.Commit(t, f.copy, name, map[string]string{name + ".txt": name + "\n"}, parent)
	}
	equal, f1, d, a1 := commit("equal", f.base), commit("ff1", f.base), commit("div", f.base), commit("ahead1", f.base)
	divHub := commit("div-hub", d)
	rows := []struct {
		name, branch, status string
		// hub and origin are the branch's tips before the sync, "" for
		// none.
		hub, origin   string
		action, state string
	}{
		{"origin lacks the branch", "patch/missing", carrypatch.StatusActive, commit("missing", f.base), "", "none", "missing_on_origin"},
		{"hub lacks the branch", "patch/created", carrypatch.StatusConflict, "", commit("created", f.base), "created", "in_sync"},
		{"equal tips", "patch/equal", carrypatch.StatusDisabled, equal, equal, "none", "in_sync"},
		{"hub tip is an ancestor of origin's", "patch/ff", carrypatch.StatusActive, f1, commit("ff2", f1), "fast_forwarded", "in_sync"},
		{"each side has a commit the other lacks, policy report", "patch/div", carrypatch.StatusConflict, divHub, commit("div-origin", d), "none", "diverged"},
		{"hub tip ahead of origin's, policy report", "patch/ahead", carrypatch.StatusDisabled, commit("ahead2", a1), a1, "none", "diverged"},
	}
	for i, row := range rows {
		p := livePatch(f.slug, row.branch, i+1)
		p.Status = row.status
		if row.status == carrypatch.StatusConflict {
			p.ConflictFiles = []string{"conflict.txt"}
		}
		if row.hub != "" {
			gitrepotest.Git(t, f.copy, "update-ref", "refs/heads/"+row.branch, row.hub)
		}
		if row.origin != "" {
			f.originBranch(t, row.branch, row.origin)
		}
		f.put(t, p)
	}

	r := f.mustSync(t)
	entries := r.synced(t)
	var names []string
	for _, e := range entries {
		names = append(names, e.BranchName)
	}
	var want []string
	for _, row := range rows {
		want = append(want, row.branch)
	}
	if !slices.Equal(names, want) {
		t.Fatalf("patches_synced names %v, want %v in position order", names, want)
	}
	for i, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			e := entries[i]
			if e.Action != row.action || e.State != row.state {
				t.Errorf("entry = %s/%s, want %s/%s", e.Action, e.State, row.action, row.state)
			}
			after := row.hub
			if row.action != "none" {
				after = row.origin
			}
			if got := refIn(f.copy, "refs/heads/"+row.branch); got != after {
				t.Errorf("%s = %q, want %q", row.branch, got, after)
			}
			if got, want := str(e.LocalSHA), orNil(after); got != want {
				t.Errorf("local_sha = %s, want %s", got, want)
			}
			if got, want := str(e.OriginSHA), orNil(row.origin); got != want {
				t.Errorf("origin_sha = %s, want %s", got, want)
			}
			if e.ReplacedSHA != nil {
				t.Errorf("replaced_sha = %s, want null", *e.ReplacedSHA)
			}
		})
	}

	// The same divergence under policy replace, in the next sync.
	t.Run("each side has a commit the other lacks, policy replace", func(t *testing.T) {
		f.setSettings(t, originAuthority("replace"))
		row := rows[4]
		e := f.mustSync(t).syncedOf(t, row.branch)
		if e.Action != "replaced" || e.State != "in_sync" {
			t.Errorf("entry = %s/%s, want replaced/in_sync", e.Action, e.State)
		}
		if got := refIn(f.copy, "refs/heads/"+row.branch); got != row.origin {
			t.Errorf("%s = %s, want origin's %s", row.branch, got, row.origin)
		}
	})
}

// orNil is id, or what str gives for nil when id is "".
func orNil(id string) string {
	if id == "" {
		return str(nil)
	}
	return id
}

// A replace saves the hub's tip to the branch's backup ref, overwriting an
// earlier backup, before it moves the branch, and reports the saved tip.
func TestRefresh_ReplaceSavesBackup(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	f.setSettings(t, originAuthority("replace"))
	a := f.addPatch(t, "patch/a")
	aTip := refIn(f.copy, "refs/heads/patch/a")
	f.originBranch(t, a.BranchName, gitrepotest.Commit(t, f.copy, "A2", map[string]string{"a.txt": "2\n"}, aTip))
	f.addPatch(t, "patch/b")
	old := refIn(f.copy, "refs/heads/patch/b")
	older := gitrepotest.Commit(t, f.copy, "older", map[string]string{"b.txt": "older\n"}, f.base)
	gitrepotest.Git(t, f.copy, "update-ref", carrypatch.ReplacedRefPrefix+"patch/b", older)
	theirs := gitrepotest.Commit(t, f.copy, "B on origin", map[string]string{"b.txt": "origin\n"}, f.base)
	f.originBranch(t, "patch/b", theirs)

	r := f.mustSync(t)
	e := r.syncedOf(t, "patch/b")
	if e.Action != "replaced" || str(e.ReplacedSHA) != old {
		t.Errorf("patch/b's entry = %s with replaced_sha %s, want replaced with %s", e.Action, str(e.ReplacedSHA), old)
	}
	if got := refIn(f.copy, carrypatch.ReplacedRefPrefix+"patch/b"); got != old {
		t.Errorf("the backup ref of patch/b = %s, want the hub's tip %s", got, old)
	}
	if got := refIn(f.copy, "refs/heads/patch/b"); got != theirs {
		t.Errorf("patch/b = %s, want origin's %s", got, theirs)
	}
	for _, e := range r.synced(t) {
		if e.BranchName != "patch/b" && e.ReplacedSHA != nil {
			t.Errorf("%s's replaced_sha = %s, want null", e.BranchName, *e.ReplacedSHA)
		}
	}
}

// A protected patch branch is moved by the same rules as any other.
func TestRefresh_MovesProtectedPatchBranches(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	settings := originAuthority("replace")
	settings["protected_branches"] = []string{"patch/a", "patch/b"}
	f.setSettings(t, settings)
	f.addPatch(t, "patch/a")
	aOrigin := gitrepotest.Commit(t, f.copy, "A2", map[string]string{"a.txt": "2\n"}, refIn(f.copy, "refs/heads/patch/a"))
	f.originBranch(t, "patch/a", aOrigin)
	f.addPatch(t, "patch/b")
	bOrigin := gitrepotest.Commit(t, f.copy, "B on origin", map[string]string{"b.txt": "origin\n"}, f.base)
	f.originBranch(t, "patch/b", bOrigin)

	r := f.mustSync(t)
	for branch, want := range map[string]struct{ action, tip string }{
		"patch/a": {"fast_forwarded", aOrigin},
		"patch/b": {"replaced", bOrigin},
	} {
		if got := r.syncedOf(t, branch).Action; got != want.action {
			t.Errorf("%s's action = %s, want %s", branch, got, want.action)
		}
		if got := refIn(f.copy, "refs/heads/"+branch); got != want.tip {
			t.Errorf("%s = %s, want origin's %s", branch, got, want.tip)
		}
	}
}

// Under the hub as the authority, the sync compares and moves no patch
// branch and reports none.
func TestRefresh_NotRunWhenHubIsAuthority(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	f.setSettings(t, map[string]any{"patch_branch_source": "hub"})
	f.addPatch(t, "patch/a")
	f.originBranch(t, "patch/a", gitrepotest.Commit(t, f.copy, "A2", map[string]string{"a.txt": "2\n"}, refIn(f.copy, "refs/heads/patch/a")))
	f.addPatch(t, "patch/b")
	f.originBranch(t, "patch/b", gitrepotest.Commit(t, f.copy, "B on origin", map[string]string{"b.txt": "origin\n"}, f.base))
	before := gitrepotest.Refs(t, f.copy, "refs/heads/patch/")

	r := f.mustSync(t)
	for _, member := range []string{"patches_synced", "patches_diverged"} {
		if list, ok := r.body[member].([]any); !ok || len(list) != 0 {
			t.Errorf("%s = %v, want []", member, r.body[member])
		}
	}
	if after := gitrepotest.Refs(t, f.copy, "refs/heads/patch/"); !maps.Equal(after, before) {
		t.Errorf("patch branches = %v, want %v unchanged", after, before)
	}
	if n := f.eventCount(t, "parley.patch.sync"); n != 0 {
		t.Errorf("parley.patch.sync events = %d, want none", n)
	}
}

// A merged or deleted patch is left out of the refresh.
func TestRefresh_SkipsMergedAndDeleted(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	f.setSettings(t, originAuthority("replace"))
	f.addPatch(t, "patch/a")
	f.originBranch(t, "patch/a", refIn(f.copy, "refs/heads/patch/a"))
	f.addPatch(t, "patch/m", withStatus(carrypatch.StatusMergedUpstream))
	before := refIn(f.copy, "refs/heads/patch/m")
	f.originBranch(t, "patch/m", gitrepotest.Commit(t, f.copy, "M2", map[string]string{"m.txt": "2\n"}, before))
	gone := gitrepotest.Commit(t, f.copy, "G1", map[string]string{"gone.txt": "1\n"}, f.base)
	gitrepotest.Git(t, f.copy, "update-ref", "refs/heads/patch/gone", gone)
	f.put(t, deletedPatch(f.slug, "patch/gone", f.clock()))
	f.originBranch(t, "patch/gone", gitrepotest.Commit(t, f.copy, "G2", map[string]string{"gone.txt": "2\n"}, gone))

	r := f.mustSync(t)
	var names []string
	for _, e := range r.synced(t) {
		names = append(names, e.BranchName)
	}
	if !slices.Equal(names, []string{"patch/a"}) {
		t.Errorf("patches_synced names %v, want [patch/a]", names)
	}
	if got := refIn(f.copy, "refs/heads/patch/m"); got != before {
		t.Errorf("patch/m = %s, want %s unchanged", got, before)
	}
}

// Every patch branch the refresh moves or creates is reported once to the
// sync's branch-move observers, and an observer's error does not fail the
// sync.
func TestRefresh_NotifiesBranchMoveObservers(t *testing.T) {
	t.Parallel()
	obs := &moveRecorder{err: errors.New("the observer fails")}
	f := startSyncFixtureWith(t, syncOptions{compose: func(c *hub.Composition) {
		c.Workspaces().RegisterBranchMoveObserver(obs)
	}})
	f.setSettings(t, originAuthority("replace"))
	f.addPatch(t, "patch/a")
	old := refIn(f.copy, "refs/heads/patch/a")
	theirs := gitrepotest.Commit(t, f.copy, "A2", map[string]string{"a.txt": "2\n"}, old)
	f.originBranch(t, "patch/a", theirs)
	f.addPatch(t, "patch/b")
	f.originBranch(t, "patch/b", refIn(f.copy, "refs/heads/patch/b"))
	f.put(t, livePatch(f.slug, "patch/c", 3))
	created := gitrepotest.Commit(t, f.copy, "C1", map[string]string{"c.txt": "1\n"}, f.base)
	f.originBranch(t, "patch/c", created)

	f.mustSync(t)
	want := []workspace.BranchMove{
		{Workspace: f.slug, Branch: "patch/a", Before: old, After: theirs, Cause: workspace.MoveCauseSync},
		{Workspace: f.slug, Branch: "patch/c", Before: "", After: created, Cause: workspace.MoveCauseSync},
	}
	if got := obs.movesUnder("patch/"); !slices.Equal(got, want) {
		t.Errorf("the observer's moves of patch branches = %+v, want %+v", got, want)
	}
}

// Each refresh outcome is stored on its patch: the state, origin's tip
// (null when origin lacks the branch) and the time of the comparison, cut
// to the second.
func TestRefresh_StoresOriginState(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	f.setSettings(t, originAuthority("report"))
	a := f.addPatch(t, "patch/a")
	f.originBranch(t, "patch/a", refIn(f.copy, "refs/heads/patch/a"))
	b := f.addPatch(t, "patch/b")
	f.originBranch(t, "patch/b", gitrepotest.Commit(t, f.copy, "B on origin", map[string]string{"b.txt": "origin\n"}, f.base))
	c := f.addPatch(t, "patch/c")
	at := f.clock().UTC().Truncate(time.Second)

	f.mustSync(t)
	for _, want := range []struct {
		p          carrypatch.Patch
		state, tip string
	}{
		{a, "in_sync", f.origin.Head("patch/a")},
		{b, "diverged", f.origin.Head("patch/b")},
		{c, "missing_on_origin", ""},
	} {
		got := f.stored(t, want.p.ID)
		if str(got.OriginSyncState) != want.state || str(got.OriginSHA) != orNil(want.tip) || !equalTime(got.OriginSyncedAt, &at) {
			t.Errorf("%s's origin fields = %s, %s, %v; want %s, %s, %v", want.p.BranchName,
				str(got.OriginSyncState), str(got.OriginSHA), got.OriginSyncedAt, want.state, orNil(want.tip), at)
		}
	}
}

// The three origin fields are null after a sync on every patch under the
// hub as the authority, and on every merged_upstream patch under origin.
func TestRefresh_OriginFieldsNull(t *testing.T) {
	t.Parallel()
	setUp := func(t *testing.T) (*syncFixture, carrypatch.Patch, carrypatch.Patch) {
		f := startSyncFixture(t)
		f.setSettings(t, originAuthority("replace"))
		a, b := f.addPatch(t, "patch/a"), f.addPatch(t, "patch/b")
		for _, p := range []carrypatch.Patch{a, b} {
			f.originBranch(t, p.BranchName, refIn(f.copy, "refs/heads/"+p.BranchName))
		}
		f.mustSync(t)
		for _, p := range []carrypatch.Patch{a, b} {
			if got := f.stored(t, p.ID); got.OriginSyncState == nil || got.OriginSHA == nil || got.OriginSyncedAt == nil {
				t.Fatalf("after the first sync %s's origin fields = %+v, want them set", p.BranchName, got)
			}
		}
		return f, a, b
	}
	wantNull := func(t *testing.T, f *syncFixture, p carrypatch.Patch) {
		t.Helper()
		if got := f.stored(t, p.ID); got.OriginSyncState != nil || got.OriginSHA != nil || got.OriginSyncedAt != nil {
			t.Errorf("%s's origin fields = %s, %s, %v; want all null", p.BranchName, str(got.OriginSyncState), str(got.OriginSHA), got.OriginSyncedAt)
		}
	}

	t.Run("hub is the authority", func(t *testing.T) {
		t.Parallel()
		f, a, b := setUp(t)
		before := f.eventCount(t, "parley.patch.%")
		f.setSettings(t, map[string]any{"patch_branch_source": "hub"})
		f.mustSync(t)
		wantNull(t, f, a)
		wantNull(t, f, b)
		if n := f.eventCount(t, "parley.patch.%"); n != before {
			t.Errorf("parley.patch.* events = %d, want %d: clearing emits none", n, before)
		}
	})
	t.Run("a merged_upstream patch under origin", func(t *testing.T) {
		t.Parallel()
		f, a, b := setUp(t)
		if _, err := f.Store.Main().Exec(`UPDATE patches SET status = ? WHERE id = ?`, carrypatch.StatusMergedUpstream, a.ID); err != nil {
			t.Fatal(err)
		}
		f.mustSync(t)
		wantNull(t, f, a)
		if got := f.stored(t, a.ID).Status; got != carrypatch.StatusMergedUpstream {
			t.Errorf("patch/a's status = %s, want merged_upstream", got)
		}
		if got := str(f.stored(t, b.ID).OriginSyncState); got != "in_sync" {
			t.Errorf("patch/b's origin_sync_state = %s, want in_sync", got)
		}
	})
	t.Run("a patch upstream merged in this sync", func(t *testing.T) {
		t.Parallel()
		f, a, b := setUp(t)
		f.upstreamTo(t, refIn(f.copy, "refs/heads/patch/a"))
		wantMerged(t, f.mustSync(t), "patch/a")
		wantNull(t, f, a)
		if got := f.stored(t, a.ID).Status; got != carrypatch.StatusMergedUpstream {
			t.Errorf("patch/a's status = %s, want merged_upstream", got)
		}
		if got := str(f.stored(t, b.ID).OriginSyncState); got != "in_sync" {
			t.Errorf("patch/b's origin_sync_state = %s, want in_sync", got)
		}
	})
}

// wantSyncEvent checks that the event e reports the refresh outcome entry
// of the patch p, done by the owner: its metadata is the entry without
// replaced_sha.
func wantSyncEvent(t *testing.T, f *syncFixture, e patchEvent, p carrypatch.Patch, entry syncedEntry) {
	t.Helper()
	ptr := func(s *string) any {
		if s == nil {
			return nil
		}
		return *s
	}
	want := map[string]any{"branch_name": entry.BranchName, "action": entry.Action, "state": entry.State,
		"local_sha": ptr(entry.LocalSHA), "origin_sha": ptr(entry.OriginSHA)}
	if e.ResourceType != "patch" || e.ResourceID != p.ID || str(e.ActorID) != f.owner || !reflect.DeepEqual(e.Metadata, want) {
		t.Errorf("event = %s %s by %s %+v; want patch %s by %s %+v",
			e.ResourceType, e.ResourceID, str(e.ActorID), e.Metadata, p.ID, f.owner, want)
	}
}

// A refresh emits one parley.patch.sync per patch whose branch it created
// or moved or whose state changed, in position order, and none for a patch
// it left as it was.
func TestRefresh_EmitsPatchSyncEvents(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	f.setSettings(t, originAuthority("replace"))
	ps := map[string]carrypatch.Patch{}
	for _, branch := range []string{"patch/a", "patch/b", "patch/c", "patch/d"} {
		ps[branch] = f.addPatch(t, branch)
		f.originBranch(t, branch, refIn(f.copy, "refs/heads/"+branch))
	}
	f.mustSync(t)
	setup := len(f.patchEvents(t, carrypatch.EventPatchSync))

	f.originBranch(t, "patch/a", gitrepotest.Commit(t, f.copy, "A2", map[string]string{"a.txt": "2\n"}, refIn(f.copy, "refs/heads/patch/a")))
	f.originBranch(t, "patch/b", gitrepotest.Commit(t, f.copy, "B on origin", map[string]string{"b.txt": "origin\n"}, f.base))
	gitrepotest.Git(t, f.origin.Dir(), "update-ref", "-d", "refs/heads/patch/c")

	r := f.mustSync(t)
	ev := f.patchEvents(t, carrypatch.EventPatchSync)[setup:]
	var names []string
	for _, e := range ev {
		name, _ := e.Metadata["branch_name"].(string)
		names = append(names, name)
	}
	if want := []string{"patch/a", "patch/b", "patch/c"}; !slices.Equal(names, want) {
		t.Fatalf("parley.patch.sync events name %v, want %v", names, want)
	}
	for _, e := range ev {
		branch := e.Metadata["branch_name"].(string)
		wantSyncEvent(t, f, e, ps[branch], r.syncedOf(t, branch))
	}
	if ev[2].Metadata["state"] != "missing_on_origin" || ev[2].Metadata["action"] != "none" {
		t.Errorf("patch/c's event = %+v, want action none, state missing_on_origin", ev[2].Metadata)
	}

	f.mustSync(t)
	if n := len(f.patchEvents(t, carrypatch.EventPatchSync)); n != setup+len(ev) {
		t.Errorf("parley.patch.sync events after a sync that changed nothing = %d, want %d", n, setup+len(ev))
	}
}

// A replaced patch branch is reported by one parley.patch.replace with the
// replaced tip, origin's tip and the trigger sync.
func TestRefresh_EmitsPatchReplaceEvent(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	f.setSettings(t, originAuthority("replace"))
	f.addPatch(t, "patch/a")
	f.originBranch(t, "patch/a", gitrepotest.Commit(t, f.copy, "A2", map[string]string{"a.txt": "2\n"}, refIn(f.copy, "refs/heads/patch/a")))
	b := f.addPatch(t, "patch/b")
	old := refIn(f.copy, "refs/heads/patch/b")
	f.originBranch(t, "patch/b", gitrepotest.Commit(t, f.copy, "B on origin", map[string]string{"b.txt": "origin\n"}, f.base))

	f.mustSync(t)
	ev := f.patchEvents(t, carrypatch.EventPatchReplace)
	if len(ev) != 1 {
		t.Fatalf("parley.patch.replace events = %+v, want one", ev)
	}
	want := map[string]any{"branch_name": "patch/b", "replaced_sha": old, "origin_sha": f.origin.Head("patch/b"), "trigger": "sync"}
	if e := ev[0]; e.ResourceType != "patch" || e.ResourceID != b.ID || str(e.ActorID) != f.owner || !reflect.DeepEqual(e.Metadata, want) {
		t.Errorf("event = %s %s by %s %+v; want patch %s by %s %+v", e.ResourceType, e.ResourceID, str(e.ActorID), e.Metadata, b.ID, f.owner, want)
	}
}

// An outcome whose event cannot be written is not stored either, and its
// branch does not move: the sync stops with 500.
func TestRefresh_OutcomeAndEventAreAtomic(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	f.setSettings(t, originAuthority("replace"))
	a := f.addPatch(t, "patch/a")
	tip := refIn(f.copy, "refs/heads/patch/a")
	f.originBranch(t, "patch/a", tip)
	f.mustSync(t)
	f.advance(time.Hour)
	f.originBranch(t, "patch/a", gitrepotest.Commit(t, f.copy, "A2", map[string]string{"a.txt": "2\n"}, tip))
	if _, err := f.Store.Main().Exec(`CREATE TRIGGER refuse_patch_sync BEFORE INSERT ON hub_events
WHEN NEW.type = 'parley.patch.sync' AND NEW.resource_id = '` + a.ID + `'
BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	before, events := f.stored(t, a.ID), len(f.patchEvents(t, carrypatch.EventPatchSync))

	r := f.sync(t, "")
	if r.status != http.StatusInternalServerError || !strings.Contains(r.Error.Message, "patch/a") {
		t.Fatalf("sync = %d %s, want 500 naming patch/a", r.status, r.raw)
	}
	after := f.stored(t, a.ID)
	if str(after.OriginSyncState) != str(before.OriginSyncState) || str(after.OriginSHA) != str(before.OriginSHA) || !equalTime(after.OriginSyncedAt, before.OriginSyncedAt) {
		t.Errorf("patch/a's origin fields = %s, %s, %v; want %s, %s, %v unchanged", str(after.OriginSyncState), str(after.OriginSHA),
			after.OriginSyncedAt, str(before.OriginSyncState), str(before.OriginSHA), before.OriginSyncedAt)
	}
	if n := len(f.patchEvents(t, carrypatch.EventPatchSync)); n != events {
		t.Errorf("parley.patch.sync events = %d, want %d", n, events)
	}
	if got := refIn(f.copy, "refs/heads/patch/a"); got != tip {
		t.Errorf("patch/a = %s, want %s unmoved", got, tip)
	}
}
