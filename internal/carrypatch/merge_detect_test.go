package carrypatch_test

import (
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

func TestMergeDetection_Trigger(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	a := f.addPatch(t, "patch/a")
	f.mustSync(t)
	// The hub moves patch/a onto the upstream base, which then holds it;
	// neither upstream nor a refresh moved anything.
	gitrepotest.Git(t, f.copy, "update-ref", "refs/heads/patch/a", f.base)
	r1 := f.mustSync(t)
	wantMerged(t, r1)
	if got := f.stored(t, a.ID).Status; got != carrypatch.StatusActive {
		t.Errorf("after a sync that saw no movement patch/a is %s, want active", got)
	}
	up := f.moveUpstream(t, "unrelated", map[string]string{"other.txt": "other\n"})
	wantMerged(t, f.mustSync(t), "patch/a")

	// Origin as the authority: the refresh fast-forwards patch/b onto a
	// commit upstream holds, while upstream stays where it is.
	f.setSettings(t, originAuthority("replace"))
	hubTip := f.commitOn(t, up, "B", map[string]string{"b.txt": "b\n"})
	taken := f.commitOn(t, hubTip, "upstream takes B", map[string]string{"taken.txt": "taken\n"})
	f.upstreamTo(t, taken)
	f.mustSync(t)
	f.putPatch(t, "patch/b", hubTip)
	f.originBranch(t, "patch/b", taken)
	r3 := f.mustSync(t)
	if r3.body["upstream_advanced"] != false {
		t.Fatalf("upstream_advanced = %v, want false", r3.body["upstream_advanced"])
	}
	if got := r3.syncedOf(t, "patch/b").Action; got != "fast_forwarded" {
		t.Errorf("patch/b's refresh action = %s, want fast_forwarded", got)
	}
	wantMerged(t, r3, "patch/b")
}

// mergeCase is a row of the merge test table: the patch it adds, as
// built on the upstream base, and where upstream then goes.
type mergeCase struct {
	setting  string
	name     string
	build    func(t *testing.T, f *syncFixture, base, file string) (tip, upstream string)
	edit     []func(p *carrypatch.Patch)
	wantDone bool
}

func TestMergeDetection_Tests(t *testing.T) {
	t.Parallel()
	// The rows run one after another in one workspace, each on what the
	// previous one left upstream, so that each sync's window is the row's
	// own upstream commits; a row's patch that stays unmerged is disabled
	// afterwards, which leaves it out of the later rows' detection.
	ancestor := func(t *testing.T, f *syncFixture, base, file string) (string, string) {
		tip := f.commitOn(t, base, "patch", map[string]string{file: "patch\n"})
		return tip, f.commitOn(t, tip, "upstream after the patch", map[string]string{"up.txt": file})
	}
	equal := func(t *testing.T, f *syncFixture, base, file string) (string, string) {
		tip := f.commitOn(t, base, "patch", map[string]string{file: "patch\n"})
		return tip, tip
	}
	// twoCommits is a patch of two commits, of which upstream takes the
	// first or both as commits of its own.
	twoCommits := func(both bool) func(t *testing.T, f *syncFixture, base, file string) (string, string) {
		return func(t *testing.T, f *syncFixture, base, file string) (string, string) {
			one, two := map[string]string{file: "one\n"}, map[string]string{"2-" + file: "two\n"}
			tip := f.commitOn(t, f.commitOn(t, base, "patch one", one), "patch two", two)
			up := f.commitOn(t, base, "upstream takes one", one)
			if both {
				return tip, f.commitOn(t, up, "upstream takes two", two)
			}
			return tip, f.commitOn(t, up, "upstream goes on", map[string]string{"up.txt": file})
		}
	}
	atBase := func(t *testing.T, f *syncFixture, base, file string) (string, string) {
		return base, f.commitOn(t, base, "upstream goes on", map[string]string{"up.txt": file})
	}
	subject := func(s string) func(t *testing.T, f *syncFixture, base, file string) (string, string) {
		return func(t *testing.T, f *syncFixture, base, file string) (string, string) {
			tip := f.commitOn(t, base, "patch", map[string]string{file: "patch\n"})
			return tip, f.commitOn(t, base, s, map[string]string{file: "upstream's own take\n"})
		}
	}
	pr := func(url string) []func(p *carrypatch.Patch) {
		return []func(p *carrypatch.Patch){func(p *carrypatch.Patch) { p.UpstreamPRURL = new(url) }}
	}
	pr42 := pr("https://forge.example/o/r/pull/42")
	cases := []mergeCase{
		{"both", "tip an ancestor of the base", ancestor, nil, true},
		{"ancestry_only", "tip equals the base", equal, nil, true},
		{"content_based", "tip an ancestor of the base", ancestor, nil, false},
		{"both", "both commits taken under new ids", twoCommits(true), nil, true},
		{"content_based", "both commits taken under new ids", twoCommits(true), nil, true},
		{"ancestry_only", "both commits taken under new ids", twoCommits(true), nil, false},
		{"both", "one of two commits not taken", twoCommits(false), nil, false},
		{"content_based", "no commit beyond the base", atBase, nil, false},
		{"both", "pull request number in a subject", subject("Add dialer (#42)"), pr42, true},
		{"content_based", "pull request number in a subject", subject("Add dialer (#42)"), pr42, true},
		{"ancestry_only", "pull request number in a subject", subject("Add dialer (#42)"), pr42, false},
		{"both", "another number in a subject", subject("Add dialer (#421)"), pr42, false},
		{"both", "the number not at the subject's end", subject("(#42) add dialer"), pr42, false},
		{"both", "no pull request URL", subject("Add dialer (#42)"), nil, false},
		{"both", "conflict, tip an ancestor of the base", ancestor, []func(p *carrypatch.Patch){withStatus(carrypatch.StatusConflict)}, true},
	}
	f := startSyncFixture(t)
	f.mustSync(t)
	for i, c := range cases {
		label := c.setting + ", " + c.name
		f.setSettings(t, map[string]any{"squash_merge_detection": c.setting})
		base := f.upstream.Head("main")
		branch, file := fmt.Sprintf("patch/%d", i), fmt.Sprintf("p%d.txt", i)
		tip, up := c.build(t, f, base, file)
		p := f.putPatch(t, branch, tip, c.edit...)
		f.upstreamTo(t, up)
		r := f.mustSync(t)
		if got := slices.Contains(r.merged(t), branch); got != c.wantDone {
			t.Errorf("%s: %s in patches_merged %q = %v, want %v", label, branch, r.merged(t), got, c.wantDone)
		}
		if got := f.stored(t, p.ID).Status; (got == carrypatch.StatusMergedUpstream) != c.wantDone {
			t.Errorf("%s: %s is %s, want merged_upstream %v", label, branch, got, c.wantDone)
		}
		if !c.wantDone {
			f.setStatus(t, p.ID, carrypatch.StatusDisabled)
		}
	}
}

func TestMergeDetection_PRNumberWindow(t *testing.T) {
	t.Parallel()
	// The rows run one after another in one workspace; a row's patch that
	// stays unmerged is disabled afterwards. Every patch names pull request
	// 7 and holds a change upstream does not have.
	f := startSyncFixture(t)
	f.mustSync(t)
	n := 0
	addPatch := func() carrypatch.Patch {
		n++
		return f.addPatchOn(t, fmt.Sprintf("patch/%d", n), f.upstream.Head("main"),
			func(p *carrypatch.Patch) { p.UpstreamPRURL = new("https://forge.example/o/r/pull/7") })
	}
	check := func(label string, p carrypatch.Patch, r syncAnswer, want bool) {
		t.Helper()
		if got := slices.Contains(r.merged(t), p.BranchName); got != want {
			t.Errorf("%s: %s in patches_merged %q = %v, want %v", label, p.BranchName, r.merged(t), got, want)
		}
		if !want {
			f.setStatus(t, p.ID, carrypatch.StatusDisabled)
		}
	}
	firstSync := func(label string, newest int, want bool) {
		t.Helper()
		p := addPatch()
		f.setColumn(t, "upstream_head_sha", nil)
		f.upstreamTo(t, f.upstreamChain(t, label, f.upstream.Head("main"), newest, 0))
		check(label, p, f.mustSync(t), want)
	}
	firstSync("first sync, the 50th newest", 50, true)
	firstSync("first sync, the 51st newest", 51, false)

	label := "later sync, older than the stored head"
	f.upstreamTo(t, f.upstreamChain(t, label, f.upstream.Head("main"), 2, 0))
	f.mustSync(t)
	p := addPatch()
	f.upstreamTo(t, f.upstreamChain(t, label+" then", f.upstream.Head("main"), 3, -1))
	check(label, p, f.mustSync(t), false)

	label = "later sync, 60 commits after the stored head"
	p = addPatch()
	f.upstreamTo(t, f.upstreamChain(t, label, f.upstream.Head("main"), 60, 0))
	check(label, p, f.mustSync(t), true)

	forced := func(label string, newest int, want bool) {
		t.Helper()
		p := addPatch()
		f.upstreamTo(t, f.upstreamChain(t, label, "", newest+10, 10))
		r := f.mustSync(t)
		if r.body["force_push_detected"] != true {
			t.Fatalf("%s: force_push_detected = %v, want true", label, r.body["force_push_detected"])
		}
		check(label, p, r, want)
	}
	forced("force-push, the 3rd newest", 3, true)
	forced("force-push, the 51st newest", 51, false)
}

func TestMergeDetection_MarksPatchMerged(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	aTip := f.commitOn(t, f.base, "A", map[string]string{"a.txt": "a\n"})
	cTip := f.commitOn(t, aTip, "C", map[string]string{"c.txt": "c\n"})
	f.upstreamTo(t, f.commitOn(t, cTip, "upstream after A and C", map[string]string{"up.txt": "up\n"}))
	a := f.putPatch(t, "patch/a", aTip, withStatus(carrypatch.StatusConflict, "x.go"))
	b := f.addPatch(t, "patch/b")
	c := f.putPatch(t, "patch/c", cTip)

	r := f.mustSync(t)
	wantMerged(t, r, "patch/a", "patch/c")
	if got := f.stored(t, a.ID); got.Status != carrypatch.StatusMergedUpstream || len(got.ConflictFiles) != 0 {
		t.Errorf("patch/a = %s with conflict files %q, want merged_upstream with none", got.Status, got.ConflictFiles)
	}
	if got := f.stored(t, b.ID).Status; got != carrypatch.StatusActive {
		t.Errorf("patch/b = %s, want active", got)
	}
	ev := f.patchEvents(t, carrypatch.EventPatchUpdate)
	if len(ev) != 2 {
		t.Fatalf("parley.patch.update events = %+v, want two", ev)
	}
	for i, want := range []struct {
		p    carrypatch.Patch
		from string
	}{{a, carrypatch.StatusConflict}, {c, carrypatch.StatusActive}} {
		e := ev[i]
		if e.ResourceType != "patch" || e.ResourceID != want.p.ID || e.ActorType != "api_key" || str(e.ActorID) != f.owner {
			t.Errorf("event %d = %+v, want one of the patch %s by the owner %s", i, e, want.p.ID, f.owner)
		}
		meta := map[string]any{"branch_name": want.p.BranchName, "changed": []any{"status"}, "from": want.from,
			"to": carrypatch.StatusMergedUpstream, "reason": "merged_upstream"}
		if !reflect.DeepEqual(e.Metadata, meta) {
			t.Errorf("event %d's metadata = %v, want %v", i, e.Metadata, meta)
		}
	}
}

func TestMergeDetection_NotExamined(t *testing.T) {
	t.Parallel()
	f := startSyncFixture(t)
	dTip := f.commitOn(t, f.base, "D", map[string]string{"d.txt": "d\n"})
	f.upstreamTo(t, f.commitOn(t, dTip, "Fix the dialer (#9)", map[string]string{"up.txt": "up\n"}))
	d := f.putPatch(t, "patch/d", dTip, withStatus(carrypatch.StatusDisabled))
	m := f.addPatch(t, "patch/m", withStatus(carrypatch.StatusMergedUpstream))
	x := livePatch(f.slug, "patch/x", 3)
	x.UpstreamPRURL = new("https://forge.example/o/r/pull/9")
	f.put(t, x)

	r := f.mustSync(t)
	wantMerged(t, r)
	for _, want := range []struct {
		p      carrypatch.Patch
		status string
	}{{d, carrypatch.StatusDisabled}, {m, carrypatch.StatusMergedUpstream}, {x, carrypatch.StatusActive}} {
		if got := f.stored(t, want.p.ID).Status; got != want.status {
			t.Errorf("%s = %s, want %s", want.p.BranchName, got, want.status)
		}
	}
	if ev := f.patchEvents(t, carrypatch.EventPatchUpdate); len(ev) != 0 {
		t.Errorf("parley.patch.update events = %+v, want none", ev)
	}
}
