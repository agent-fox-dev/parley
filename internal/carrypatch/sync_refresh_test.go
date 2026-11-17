package carrypatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
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
// position order by an entry of exactly six members, and stored on the
// patch with the time of the comparison, cut to the second; its branch
// moves to origin's tip only for created, fast_forwarded and replaced.
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
		{"neither side has the branch", "patch/none", carrypatch.StatusActive, "", "", "none", "missing_on_origin"},
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		p := livePatch(f.slug, row.branch, i+1)
		p.Status = row.status
		if row.status == carrypatch.StatusConflict {
			p.ConflictFiles = []string{"conflict.txt"}
		}
		if row.hub != "" {
			gitrepotest.SetRef(t, f.copy, "refs/heads/"+row.branch, row.hub)
		}
		if row.origin != "" {
			f.originBranch(t, row.branch, row.origin)
		}
		f.put(t, p)
		ids[i] = p.ID
	}
	at := f.clock().UTC().Truncate(time.Second)

	r := f.mustSync(t)
	var want []string
	for _, row := range rows {
		want = append(want, row.branch)
	}
	if names := r.syncedNames(t); !slices.Equal(names, want) {
		t.Fatalf("patches_synced names %v, want %v in position order", names, want)
	}
	keys := []string{"action", "branch_name", "local_sha", "origin_sha", "replaced_sha", "state"}
	for _, e := range answerList[map[string]json.RawMessage](t, r, "patches_synced") {
		if got := slices.Sorted(maps.Keys(e)); !slices.Equal(got, keys) {
			t.Errorf("entry %s has the members %v, want exactly %v", e["branch_name"], got, keys)
		}
	}
	entries := answerList[syncedEntry](t, r, "patches_synced")
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
			for _, id := range []*string{e.LocalSHA, e.OriginSHA} {
				if id != nil && len(*id) != 40 {
					t.Errorf("the id %s is not in full", *id)
				}
			}
			if e.ReplacedSHA != nil {
				t.Errorf("replaced_sha = %s, want null", *e.ReplacedSHA)
			}
			got := f.stored(t, ids[i])
			if str(got.OriginSyncState) != row.state || str(got.OriginSHA) != orNil(row.origin) || !equalTime(got.OriginSyncedAt, &at) {
				t.Errorf("origin fields = %s, %s, %v; want %s, %s, %v", str(got.OriginSyncState), str(got.OriginSHA),
					got.OriginSyncedAt, row.state, orNil(row.origin), at)
			}
		})
	}

	// The same divergence under policy replace, in the next sync.
	t.Run("each side has a commit the other lacks, policy replace", func(t *testing.T) {
		f.setSettings(t, originAuthority("replace"))
		row := rows[4]
		e := f.mustSync(t).syncedOf(t, row.branch)
		if e.Action != "replaced" || e.State != "in_sync" || str(e.ReplacedSHA) != row.hub {
			t.Errorf("entry = %s/%s with replaced_sha %s, want replaced/in_sync with %s", e.Action, e.State, str(e.ReplacedSHA), row.hub)
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
// earlier backup, before it moves the branch, reports the saved tip and
// emits one parley.patch.replace with the replaced tip, origin's tip and
// the trigger sync. A protected patch branch is moved by the same rules as
// any other.
func TestRefresh_ReplaceSavesBackup(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	settings := originAuthority("replace")
	settings["protected_branches"] = []string{"patch/a", "patch/b"}
	f.setSettings(t, settings)
	f.addPatch(t, "patch/a")
	aOrigin := f.originAhead(t, "patch/a", "origin")
	b := f.addPatch(t, "patch/b")
	old := refIn(f.copy, "refs/heads/patch/b")
	older := f.commitOn(t, f.base, "older", map[string]string{"b.txt": "older\n"})
	gitrepotest.SetRef(t, f.copy, carrypatch.ReplacedRefPrefix+"patch/b", older)
	theirs := f.commitOn(t, f.base, "B on origin", map[string]string{"b.txt": "origin\n"})
	f.originBranch(t, "patch/b", theirs)

	r := f.mustSync(t)
	for branch, want := range map[string]struct{ action, tip string }{
		"patch/a": {"fast_forwarded", aOrigin},
		"patch/b": {"replaced", theirs},
	} {
		e := r.syncedOf(t, branch)
		if e.Action != want.action {
			t.Errorf("%s's action = %s, want %s", branch, e.Action, want.action)
		}
		if got := refIn(f.copy, "refs/heads/"+branch); got != want.tip {
			t.Errorf("%s = %s, want origin's %s", branch, got, want.tip)
		}
		if branch == "patch/b" && str(e.ReplacedSHA) != old {
			t.Errorf("patch/b's replaced_sha = %s, want the hub's tip %s", str(e.ReplacedSHA), old)
		} else if branch != "patch/b" && e.ReplacedSHA != nil {
			t.Errorf("%s's replaced_sha = %s, want null", branch, *e.ReplacedSHA)
		}
	}
	if got := refIn(f.copy, carrypatch.ReplacedRefPrefix+"patch/b"); got != old {
		t.Errorf("the backup ref of patch/b = %s, want the hub's tip %s", got, old)
	}
	ev := f.patchEvents(t, carrypatch.EventPatchReplace)
	if len(ev) != 1 {
		t.Fatalf("parley.patch.replace events = %+v, want one", ev)
	}
	want := map[string]any{"branch_name": "patch/b", "replaced_sha": old, "origin_sha": theirs, "trigger": "sync"}
	if e := ev[0]; e.ResourceType != "patch" || e.ResourceID != b.ID || str(e.ActorID) != f.owner || !reflect.DeepEqual(e.Metadata, want) {
		t.Errorf("event = %s %s by %s %+v; want patch %s by %s %+v", e.ResourceType, e.ResourceID, str(e.ActorID), e.Metadata, b.ID, f.owner, want)
	}
}

// Under the hub as the authority, the sync compares and moves no patch
// branch and reports none.
func TestRefresh_NotRunWhenHubIsAuthority(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	f.setSettings(t, map[string]any{"patch_branch_source": "hub"})
	f.addPatch(t, "patch/a")
	f.originAhead(t, "patch/a", "origin")
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
	f.originAhead(t, "patch/m", "origin")
	gone := gitrepotest.Commit(t, f.copy, "G1", map[string]string{"gone.txt": "1\n"}, f.base)
	gitrepotest.SetRef(t, f.copy, "refs/heads/patch/gone", gone)
	f.put(t, deletedPatch(f.slug, "patch/gone", f.clock()))
	f.originAhead(t, "patch/gone", "origin")

	if names := f.mustSync(t).syncedNames(t); !slices.Equal(names, []string{"patch/a"}) {
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
	theirs := f.originAhead(t, "patch/a", "origin")
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

// The three origin fields are null after a sync on every patch under the
// hub as the authority, and on every merged_upstream patch under origin,
// one already merged or one upstream merged in this sync. The subtests are
// consecutive syncs of one workspace.
func TestRefresh_OriginFieldsNull(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	f.setSettings(t, originAuthority("replace"))
	a, b, c, d := f.addPatch(t, "patch/a"), f.addPatch(t, "patch/b"), f.addPatch(t, "patch/c"), f.addPatch(t, "patch/d")
	for _, p := range []carrypatch.Patch{a, b, c, d} {
		f.originBranch(t, p.BranchName, refIn(f.copy, "refs/heads/"+p.BranchName))
	}
	f.mustSync(t)
	for _, p := range []carrypatch.Patch{a, b, c, d} {
		if got := f.stored(t, p.ID); got.OriginSyncState == nil || got.OriginSHA == nil || got.OriginSyncedAt == nil {
			t.Fatalf("after the first sync %s's origin fields = %+v, want them set", p.BranchName, got)
		}
	}
	wantNull := func(t *testing.T, p carrypatch.Patch) {
		t.Helper()
		if got := f.stored(t, p.ID); got.OriginSyncState != nil || got.OriginSHA != nil || got.OriginSyncedAt != nil {
			t.Errorf("%s's origin fields = %s, %s, %v; want all null", p.BranchName, str(got.OriginSyncState), str(got.OriginSHA), got.OriginSyncedAt)
		}
	}
	wantMergedInSync := func(t *testing.T, merged carrypatch.Patch, others ...carrypatch.Patch) {
		t.Helper()
		wantNull(t, merged)
		if got := f.stored(t, merged.ID).Status; got != carrypatch.StatusMergedUpstream {
			t.Errorf("%s's status = %s, want merged_upstream", merged.BranchName, got)
		}
		for _, p := range others {
			if got := str(f.stored(t, p.ID).OriginSyncState); got != "in_sync" {
				t.Errorf("%s's origin_sync_state = %s, want in_sync", p.BranchName, got)
			}
		}
	}

	t.Run("a merged_upstream patch under origin", func(t *testing.T) {
		f.setStatus(t, a.ID, carrypatch.StatusMergedUpstream)
		f.mustSync(t)
		wantMergedInSync(t, a, b, c, d)
	})
	t.Run("a patch upstream merged in this sync", func(t *testing.T) {
		f.upstreamTo(t, refIn(f.copy, "refs/heads/patch/b"))
		wantMerged(t, f.mustSync(t), "patch/b")
		wantMergedInSync(t, b, c, d)
	})
	t.Run("hub is the authority", func(t *testing.T) {
		before := f.eventCount(t, "parley.patch.%")
		f.setSettings(t, map[string]any{"patch_branch_source": "hub"})
		f.mustSync(t)
		for _, p := range []carrypatch.Patch{a, b, c, d} {
			wantNull(t, p)
		}
		if n := f.eventCount(t, "parley.patch.%"); n != before {
			t.Errorf("parley.patch.* events = %d, want %d: clearing emits none", n, before)
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

	f.originAhead(t, "patch/a", "origin")
	f.originBranch(t, "patch/b", f.commitOn(t, f.base, "B on origin", map[string]string{"b.txt": "origin\n"}))
	gitrepotest.Git(t, f.origin.Dir(), "update-ref", "-d", "refs/heads/patch/c")

	r := f.mustSync(t)
	ev := f.patchEvents(t, carrypatch.EventPatchSync)[setup:]
	if names, want := eventBranches(ev), []string{"patch/a", "patch/b", "patch/c"}; !slices.Equal(names, want) {
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
	f.originAhead(t, "patch/a", "origin")
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

// A refresh that loses a swap to another writer stops the sync at that
// branch with 500, without a retry, a later patch or merge detection; it
// keeps what earlier patches' outcomes did, writes no completion, still
// queues the rebuild of a branch it moved, and the next sync completes
// the rest. The subtests are consecutive steps of one workspace.
func TestRefresh_LostSwap(t *testing.T) {
	t.Parallel()
	mover := &refMover{}
	f := startSyncFixtureWith(t, syncOptions{deps: func(d *carrypatch.Deps) { d.BeforeRefSwap = mover.hook }})
	f.setSettings(t, originAuthority("replace"))
	a, b, c := f.addPatch(t, "patch/a"), f.addPatch(t, "patch/b"), f.addPatch(t, "patch/c")
	patches := []carrypatch.Patch{a, b, c}
	for _, p := range patches {
		f.originBranch(t, p.BranchName, refIn(f.copy, "refs/heads/"+p.BranchName))
	}
	f.mustSync(t)
	f.dropRebuilds(t)
	f.advance(time.Hour)
	// interfere makes another writer move branch to a commit of its own
	// just before the refresh's swap of it.
	interfere := func(t *testing.T, branch, label string) string {
		x := f.commitOn(t, f.base, "another writer "+label, map[string]string{"x.txt": label + "\n"})
		mover.arm(f.copy, branch, x)
		return x
	}
	wantStopped := func(t *testing.T, r syncAnswer, branch string) {
		t.Helper()
		if r.status != http.StatusInternalServerError || r.Error.ErrorType != "internal" || !strings.Contains(r.Error.Message, branch) {
			t.Fatalf("sync = %d %s, want 500 internal naming %s", r.status, r.raw, branch)
		}
	}

	// Origin is ahead on patch/a and patch/c, patch/b diverged from origin
	// at B, and upstream merged patch/c by fast-forward.
	f.originAhead(t, "patch/a", "origin")
	f.originAhead(t, "patch/c", "origin")
	bTip, cTip := refIn(f.copy, "refs/heads/patch/b"), refIn(f.copy, "refs/heads/patch/c")
	f.originBranch(t, "patch/b", f.commitOn(t, f.base, "patch/b on origin", map[string]string{"patch-b.txt": "origin\n"}))
	f.upstreamTo(t, cTip)
	x := interfere(t, "patch/b", "X")
	w0, bBefore := f.record(t), f.stored(t, b.ID)
	syncEvents, completions := len(f.patchEvents(t, carrypatch.EventPatchSync)), f.eventCount(t, "parley.workspace.sync")

	r := f.sync(t, "")
	wantStopped(t, r, "patch/b")

	t.Run("stops the sync at the branch without retrying", func(t *testing.T) {
		if got := refIn(f.copy, "refs/heads/patch/b"); got != x {
			t.Errorf("patch/b = %s, want %s where the other writer put it", got, x)
		}
		if got := refIn(f.copy, "refs/heads/patch/c"); got != cTip {
			t.Errorf("patch/c = %s, want %s unrefreshed", got, cTip)
		}
		for _, p := range patches {
			if got := f.stored(t, p.ID).Status; got != carrypatch.StatusActive {
				t.Errorf("%s's status = %s, want active: no merge detection", p.BranchName, got)
			}
		}
		n, err := mover.swaps("patch/b")
		if err != nil {
			t.Fatalf("the other writer could not move patch/b: %v", err)
		}
		if later, _ := mover.swaps("patch/c"); n != 1 || later != 0 {
			t.Errorf("swaps of patch/b, patch/c = %d, %d; want 1, 0", n, later)
		}
	})
	t.Run("keeps the earlier outcomes and the backup", func(t *testing.T) {
		if got, want := refIn(f.copy, "refs/heads/patch/a"), f.origin.Head("patch/a"); got != want {
			t.Errorf("patch/a = %s, want origin's %s", got, want)
		}
		if got := str(f.stored(t, a.ID).OriginSyncState); got != "in_sync" {
			t.Errorf("patch/a's origin_sync_state = %s, want in_sync", got)
		}
		if names := eventBranches(f.patchEvents(t, carrypatch.EventPatchSync)[syncEvents:]); !slices.Equal(names, []string{"patch/a"}) {
			t.Errorf("parley.patch.sync events name %v, want [patch/a]", names)
		}
		if got := refIn(f.copy, carrypatch.ReplacedRefPrefix+"patch/b"); got != bTip {
			t.Errorf("the backup ref of patch/b = %s, want the tip %s saved before the lost swap", got, bTip)
		}
		if got := f.stored(t, b.ID).OriginSyncedAt; !equalTime(got, bBefore.OriginSyncedAt) {
			t.Errorf("patch/b's origin_synced_at = %v, want %v unchanged", got, bBefore.OriginSyncedAt)
		}
	})
	t.Run("writes no completion", func(t *testing.T) {
		w1 := f.record(t)
		if str(w1.UpstreamHeadSHA) != str(w0.UpstreamHeadSHA) || !equalTime(w1.LastSyncAt, w0.LastSyncAt) || str(w1.SyncError) != str(w0.SyncError) {
			t.Errorf("upstream_head_sha, last_sync_at, sync_error = %s, %v, %s; want %s, %v, %s unchanged",
				str(w1.UpstreamHeadSHA), w1.LastSyncAt, str(w1.SyncError), str(w0.UpstreamHeadSHA), w0.LastSyncAt, str(w0.SyncError))
		}
		if n := f.eventCount(t, "parley.workspace.sync"); n != completions {
			t.Errorf("parley.workspace.sync events = %d, want %d", n, completions)
		}
	})
	t.Run("the next sync completes the rest", func(t *testing.T) {
		r := f.mustSync(t)
		if r.body["upstream_advanced"] != true {
			t.Errorf("upstream_advanced = %v, want true", r.body["upstream_advanced"])
		}
		if names := r.syncedNames(t); !slices.Equal(names, []string{"patch/a", "patch/b", "patch/c"}) {
			t.Errorf("patches_synced names %v, want [patch/a patch/b patch/c]", names)
		}
		if got := r.syncedOf(t, "patch/a").Action; got != "none" {
			t.Errorf("patch/a's action = %s, want none", got)
		}
		if got, want := refIn(f.copy, "refs/heads/patch/c"), f.origin.Head("patch/c"); got != want {
			t.Errorf("patch/c = %s, want origin's %s", got, want)
		}
		if got, want := r.body["upstream_head_sha"], f.upstream.Head("main"); got != want || r.body["last_sync_at"] == nil {
			t.Errorf("upstream_head_sha, last_sync_at = %v, %v; want %s and a time", got, r.body["last_sync_at"], want)
		}
	})

	t.Run("a lost backup swap keeps the backup another writer saved", func(t *testing.T) {
		f.originBranch(t, "patch/b", f.commitOn(t, f.base, "patch/b on origin again", map[string]string{"patch-b.txt": "again\n"}))
		// Another writer replaces patch/b between the comparison and the
		// swap, as a reset to origin does, saving the tip it replaced.
		saved := f.commitOn(t, f.base, "pushed meanwhile", map[string]string{"x.txt": "pushed\n"})
		moved := f.commitOn(t, f.base, "reset meanwhile", map[string]string{"x.txt": "reset\n"})
		mover.armBackup(f.copy, "patch/b", moved, saved)
		wantStopped(t, f.sync(t, ""), "patch/b")
		if got := refIn(f.copy, carrypatch.ReplacedRefPrefix+"patch/b"); got != saved {
			t.Errorf("the backup ref of patch/b = %s, want %s, which the other writer saved", got, saved)
		}
		if got := refIn(f.copy, "refs/heads/patch/b"); got != moved {
			t.Errorf("patch/b = %s, want %s where the other writer put it", got, moved)
		}
	})

	// The rows are consecutive failed syncs, each from a workspace with no
	// rebuild job.
	t.Run("still queues the rebuild", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			auto bool
			// arrange readies the sync and returns the branch whose swap
			// is lost.
			arrange func(t *testing.T) string
			queued  bool
		}{
			{"an earlier branch moved", true, func(t *testing.T) string {
				f.originAhead(t, "patch/a", "r1")
				f.originAhead(t, "patch/b", "r1")
				interfere(t, "patch/b", "r1")
				return "patch/b"
			}, true},
			{"the first patch's swap is lost after upstream advanced", true, func(t *testing.T) string {
				f.moveUpstream(t, "U2", map[string]string{"u2.txt": "2\n"})
				f.originAhead(t, "patch/a", "r2")
				interfere(t, "patch/a", "r2")
				return "patch/a"
			}, false},
			{"an earlier branch moved with auto_rebuild_after_sync off", false, func(t *testing.T) string {
				f.originAhead(t, "patch/a", "r3")
				interfere(t, "patch/b", "r3")
				return "patch/b"
			}, false},
			{"merge detection fails after the refresh moved a branch", true, func(t *testing.T) string {
				// patch/e's branch moves onto a commit upstream holds, while
				// upstream stays where the last completed sync found it.
				e := f.commitOn(t, f.upstream.Head("main"), "patch/e", map[string]string{"e.txt": "e\n"})
				taken := f.commitOn(t, e, "upstream takes patch/e", map[string]string{"taken.txt": "taken\n"})
				f.upstreamTo(t, taken)
				f.mustSync(t)
				f.dropRebuilds(t)
				f.putPatch(t, "patch/e", e)
				f.originBranch(t, "patch/e", taken)
				if _, err := f.Store.Main().Exec(`CREATE TRIGGER refuse_merged BEFORE UPDATE OF status ON patches
WHEN NEW.status = 'merged_upstream' BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _, _ = f.Store.Main().Exec(`DROP TRIGGER refuse_merged`) })
				return "patch/e"
			}, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f.dropRebuilds(t)
				f.setSettings(t, map[string]any{"auto_rebuild_after_sync": tc.auto})
				stopsAt := tc.arrange(t)
				wantStopped(t, f.sync(t, ""), stopsAt)
				js := f.rebuildJobs(t)
				if !tc.queued {
					if len(js) != 0 {
						t.Errorf("rebuild jobs = %v, want none", js)
					}
					return
				}
				if len(js) != 1 {
					t.Fatalf("rebuild jobs = %v, want one", js)
				}
				if job := f.rebuildRecord(t, js[0].ID); job["status"] != "queued" || job["submitted_by"] != f.owner {
					t.Errorf("the rebuild is %v, submitted by %v; want queued by %s", job["status"], job["submitted_by"], f.owner)
				}
			})
		}
	})
}

// holdingObserver is a branch-move observer that holds the sync telling it
// of the first move of branch until release is closed, after closing
// reached.
type holdingObserver struct {
	branch           string
	reached, release chan struct{}
	once             sync.Once
}

func (o *holdingObserver) BranchMoved(_ context.Context, m workspace.BranchMove) error {
	if m.Branch == o.branch {
		o.once.Do(func() {
			close(o.reached)
			<-o.release
		})
	}
	return nil
}

// A sync that fetched origin before a later sync fetched a newer tip of a
// patch branch, and moved the hub's branch to it, leaves the branch at that
// newer tip: it never moves a branch back to the older tip it fetched, and
// replaces and saves nothing.
func TestRefresh_EarlierFetchNeverMovesBack(t *testing.T) {
	t.Parallel()
	hold := &holdingObserver{branch: "feat", reached: make(chan struct{}), release: make(chan struct{})}
	f := startSyncFixtureWith(t, syncOptions{compose: func(c *hub.Composition) {
		c.Workspaces().RegisterBranchMoveObserver(hold)
	}})
	f.setSettings(t, originAuthority("replace"))
	a := f.addPatch(t, "patch/a")
	f.originAhead(t, "patch/a", "o1")
	// The earlier sync is held in its per-branch pass, after its origin
	// fetch, by its move of feat.
	f.originAhead(t, "feat", "f2")
	earlier := f.startSync(t)
	defer close(hold.release)
	select {
	case <-hold.reached:
	case <-time.After(20 * time.Second):
		t.Fatal("the earlier sync did not move feat within 20 s")
	}
	o2 := f.originAhead(t, "patch/a", "o2")
	if got := f.mustSync(t).syncedOf(t, "patch/a").Action; got != "fast_forwarded" {
		t.Fatalf("the later sync's action on patch/a = %s, want fast_forwarded", got)
	}
	replaces := f.eventCount(t, carrypatch.EventPatchReplace)
	hold.release <- struct{}{}

	r := earlier()
	if r.status != http.StatusOK {
		t.Fatalf("the earlier sync = %d %s, want 200", r.status, r.raw)
	}
	if e := r.syncedOf(t, "patch/a"); e.Action != "none" || e.State != "in_sync" || str(e.OriginSHA) != o2 {
		t.Errorf("the earlier sync's patch/a = %s/%s with origin_sha %s, want none/in_sync at %s", e.Action, e.State, str(e.OriginSHA), o2)
	}
	if got := refIn(f.copy, "refs/heads/patch/a"); got != o2 {
		t.Errorf("patch/a = %s, want the later sync's %s", got, o2)
	}
	if got := refIn(f.copy, carrypatch.ReplacedRefPrefix+"patch/a"); got != "" {
		t.Errorf("the backup ref of patch/a = %s, want none", got)
	}
	if n := f.eventCount(t, carrypatch.EventPatchReplace); n != replaces {
		t.Errorf("parley.patch.replace events = %d, want %d", n, replaces)
	}
	if got := f.stored(t, a.ID); str(got.OriginSyncState) != "in_sync" || str(got.OriginSHA) != o2 {
		t.Errorf("patch/a's origin fields = %s, %s; want in_sync, %s", str(got.OriginSyncState), str(got.OriginSHA), o2)
	}
}
