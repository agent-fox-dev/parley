package change_test

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

func TestRestack_RebasesADependantOntoTheNewHeadBelowAsAVersionOfTheHub(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.hold(t)
	w.holdRestacks(t)
	a := w.stacked(t, "a")[0]
	commit(t, w.demo, "b", a.HeadSHA, "B\n", map[string]string{"b.txt": "b\n"})
	b := w.openBranch(t, "b", func(in *change.OpenInput) { in.SessionID = new(sessionOne) })
	seq := w.seq(t)
	head := w.amend(t, "a", map[string]string{"a2.txt": "a2\n"})
	w.runPending(t)

	vs := w.versions(t, "demo", b.Number)
	v := vs[len(vs)-1]
	created, _ := v["created_by"].(map[string]any)
	got := map[string]any{"number": v["number"], "kind": v["kind"], "actor": created["actor_type"], "base_sha": v["base_sha"],
		"session_id": v["session_id"], "patch_equivalent_to": v["patch_equivalent_to"]}
	want := map[string]any{"number": 2.0, "kind": change.KindRestack, "actor": change.ActorSystem, "base_sha": head,
		"session_id": sessionOne, "patch_equivalent_to": 1.0}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the restack version is %v, want %v", got, want)
	}
	newHead, _ := v["head_sha"].(string)
	if _, err := gitrepotest.Run(w.demo, "", "merge-base", "--is-ancestor", head, newHead); err != nil || w.head(t, "b") != newHead {
		t.Errorf("b is at %s, the version at %s, on %s: %v; want the version, built on it", w.head(t, "b"), newHead, head, err)
	}
	if ch := w.read(t, b.Number); ch.NeedsRestack || len(w.pending(t)) != 0 {
		t.Errorf("needs_restack %v, pending %v; want false, none", ch.NeedsRestack, w.pending(t))
	}

	var types []string
	var meta []map[string]any
	for _, e := range w.eventsAfter(t, seq) {
		if e.ResourceType == "change" && e.ResourceID == strconv.FormatInt(b.Number, 10) {
			if e.ActorType != change.ActorSystem {
				t.Errorf("%s by %s, want by the hub", e.Type, e.ActorType)
			}
			types, meta = append(types, e.Type), append(meta, metadata(t, e))
		}
	}
	if want := []string{change.EventVersion, change.EventRestack}; !slices.Equal(types, want) {
		t.Fatalf("events of b %v, want %v", types, want)
	}
	if meta[0]["kind"] != change.KindRestack || meta[1]["onto_sha"] != head || meta[1]["trigger"] != "version" {
		t.Errorf("the events say kind %v, onto %v, trigger %v; want restack, %s, version", meta[0]["kind"], meta[1]["onto_sha"], meta[1]["trigger"], head)
	}
	for _, m := range meta {
		if m["version"] != 2.0 || m["head_sha"] != newHead || m["ref"] != b.Ref {
			t.Errorf("an event names version %v, head %v, ref %v; want 2, %s, %s", m["version"], m["head_sha"], m["ref"], newHead, b.Ref)
		}
	}
}

func TestRestack_ContinuesUpTheStackBottomFirst(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.holdRestacks(t)
	s := w.stacked(t, "a", "b", "c", "d")
	seq := w.seq(t)
	w.amend(t, "a", map[string]string{"a2.txt": "a2\n"})
	w.runPending(t)
	for i, ch := range s[1:] {
		vs := w.versions(t, "demo", ch.Number)
		if v, below := vs[len(vs)-1], w.read(t, s[i].Number).HeadSHA; v["kind"] != change.KindRestack || v["base_sha"] != below {
			t.Errorf("demo#%d's last version is %v on %v, want a restack on %s", ch.Number, v["kind"], v["base_sha"], below)
		}
	}
	want := []string{strconv.FormatInt(s[1].Number, 10), strconv.FormatInt(s[2].Number, 10), strconv.FormatInt(s[3].Number, 10)}
	if got, _ := w.restackEvents(t, seq); !slices.Equal(got, want) {
		t.Errorf("restack events of %v, want %v", got, want)
	}
}

func TestRestack_LeavesADependantAlreadyOnTheNewHeadBelowAsItIs(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.holdRestacks(t)
	for i, reason := range []string{"", "conflict", "auto_restack_off"} {
		t.Run(fmt.Sprintf("flagged %q", reason), func(t *testing.T) {
			p := fmt.Sprintf("u%d", i)
			b := w.stacked(t, p+"a", p+"b")[1]
			if reason != "" {
				w.write(t, func(ctx context.Context, tx *sql.Tx) error {
					return w.changes.SetNeedsRestack(ctx, tx, "demo", b.Number, true, &reason)
				})
			}
			head := w.amend(t, p+"a", map[string]string{p + "a2.txt": "a2\n"})
			pushed := commit(t, w.demo, p+"b", head, "B\n", map[string]string{p + "b.txt": "b\n"})
			w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: p + "b", Head: pushed, Kind: change.KindPush, Caller: w.ownerCaller()})
			seq := w.seq(t)
			w.runPending(t)
			ch := w.read(t, b.Number)
			if vs := w.versions(t, "demo", b.Number); ch.Version != 2 || vs[1]["kind"] != change.KindPush || w.head(t, p+"b") != pushed {
				t.Errorf("demo#%d is at version %d of kind %v, its branch at %s; want 2 of kind push at %s", b.Number, ch.Version, vs[len(vs)-1]["kind"], w.head(t, p+"b"), pushed)
			}
			if got, _ := w.restackEvents(t, seq); len(got) != 0 || slices.Contains(w.pending(t), b.Number) || ch.NeedsRestack {
				t.Errorf("restack events %v, pending %v, needs_restack %v; want none, none, false", got, w.pending(t), ch.NeedsRestack)
			}
		})
	}
}

func TestRestack_StartsAgainWhenTheSourceBranchMovesBeforeTheSwap(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.holdRestacks(t)
	for i, c := range []struct {
		name string
		// moves is how many times the branch moves before a swap, -1 for
		// every time.
		moves, rebases int
		restacked      bool
	}{
		{"moved once", 1, 2, true},
		{"moved before every swap", -1, 5, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := fmt.Sprintf("m%d", i)
			b := w.stacked(t, p+"a", p+"b")[1]
			rebases, moved := 0, 0
			change.SetRestackSeams(w.changes, change.RestackSeams{
				Rebase: func(ctx context.Context, repo *gitrepo.Repo, base, head, onto string, at time.Time) (string, *gitrepo.ConflictReport, error) {
					rebases++
					return repo.RebaseFrom(ctx, base, head, onto, at)
				},
				BeforeSwap: func(_ context.Context, _ string, number int64, _ int) {
					if number == b.Number && (c.moves < 0 || moved < c.moves) {
						moved++
						commit(t, w.demo, p+"b", w.head(t, p+"b"), "between"+strconv.Itoa(moved)+"\n", map[string]string{p + "x.txt": strconv.Itoa(moved)})
					}
				},
			})
			defer change.SetRestackSeams(w.changes, change.RestackSeams{})
			seq := w.seq(t)
			w.amend(t, p+"a", map[string]string{p + "a2.txt": "a2\n"})
			w.runPending(t)
			vs := w.versions(t, "demo", b.Number)
			v := vs[len(vs)-1]
			events, _ := w.restackEvents(t, seq)
			if rebases != c.rebases {
				t.Errorf("the rebase ran %d times, want %d", rebases, c.rebases)
			}
			if !c.restacked {
				if len(vs) != 1 || len(events) != 0 || !slices.Contains(w.pending(t), b.Number) || w.read(t, b.Number).NeedsRestack {
					t.Errorf("%d versions, restack events %v, pending %v; want 1, none, still pending", len(vs), events, w.pending(t))
				}
				return
			}
			base, head := v["base_sha"].(string), v["head_sha"].(string)
			if got := w.subjects(t, base, head); v["kind"] != change.KindRestack || !slices.Equal(got, []string{p + "b", "between1"}) || w.head(t, p+"b") != head {
				t.Errorf("the last version is %v with the commits %q, its branch at %s; want a restack of %q at %s", v["kind"], got, w.head(t, p+"b"), []string{p + "b", "between1"}, head)
			}
		})
	}
}

func TestRestack_ReplaysExactlyTheChangesOwnCommits(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.holdRestacks(t)
	a := w.stacked(t, "a")[0]
	from := a.HeadSHA
	for i := range 3 {
		from = commit(t, w.demo, "b", from, "b"+strconv.Itoa(i)+"\n", map[string]string{"b.txt": strconv.Itoa(i) + "\n"})
	}
	b := w.openBranch(t, "b", nil)
	unrelated := commit(t, w.demo, "a", w.base, "A2\n", map[string]string{"z.txt": "z\n"})
	w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "a", Head: unrelated, Kind: change.KindPush, Caller: w.ownerCaller()})
	w.runPending(t)

	vs := w.versions(t, "demo", b.Number)
	v := vs[len(vs)-1]
	base, head := v["base_sha"].(string), v["head_sha"].(string)
	if got, want := w.subjects(t, base, head), []string{"b0", "b1", "b2"}; base != unrelated || !slices.Equal(got, want) {
		t.Fatalf("the restack version is on %s with %q, want on %s with %q", base, got, unrelated, want)
	}
	patchIDs := func(base, head string) []string {
		var out []string
		for _, c := range strings.Fields(gitrepotest.Git(t, w.demo, "rev-list", "--reverse", base+".."+head)) {
			id, err := gitrepotest.Run(w.demo, gitrepotest.Git(t, w.demo, "show", c)+"\n", "patch-id", "--stable")
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, strings.Fields(id)[0])
		}
		return out
	}
	if got, want := patchIDs(base, head), patchIDs(a.HeadSHA, b.HeadSHA); !slices.Equal(got, want) {
		t.Errorf("the replayed commits have the patch ids %q, want %q", got, want)
	}
	if _, err := gitrepotest.Run(w.demo, "", "merge-base", "--is-ancestor", a.HeadSHA, head); err == nil {
		t.Errorf("the restacked head %s still has the old head of a %s", head, a.HeadSHA)
	}
}

func TestRestack_RebasesTheDependantsOfALandedChangeOntoTheLandedCommit(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.holdRestacks(t)
	s := w.stacked(t, "la", "lb", "lc")
	landed := w.land(t, s[0].Number, map[string]string{"la.txt": "la\n"})
	seq := w.seq(t)
	if err := w.changes.OnLanded(context.Background(), "demo", s[0].Number, landed); err != nil {
		t.Fatalf("OnLanded = %v", err)
	}
	w.runPending(t)

	b, c := w.read(t, s[1].Number), w.read(t, s[2].Number)
	vs := w.versions(t, "demo", b.Number)
	v := vs[len(vs)-1]
	if got := w.subjects(t, landed, b.HeadSHA); b.DependsOn != nil || v["kind"] != change.KindRestack || v["base_sha"] != landed || !slices.Equal(got, []string{"lb"}) {
		t.Errorf("demo#%d depends on %v, its last version is %v on %v with %q; want none, a restack on %s with [lb]", b.Number, b.DependsOn, v["kind"], v["base_sha"], got, landed)
	}
	numbers, meta := w.restackEvents(t, seq)
	if want := []string{strconv.FormatInt(b.Number, 10), strconv.FormatInt(c.Number, 10)}; !slices.Equal(numbers, want) ||
		meta[0]["onto_sha"] != landed || meta[0]["trigger"] != change.TriggerLand {
		t.Fatalf("restack events of %v, the first %v; want %v, the first onto %s for land", numbers, meta, want, landed)
	}
	vs = w.versions(t, "demo", c.Number)
	v = vs[len(vs)-1]
	head, _ := v["head_sha"].(string)
	_, err := gitrepotest.Run(w.demo, "", "merge-base", "--is-ancestor", landed, head)
	if c.DependsOn == nil || *c.DependsOn != b.Number || v["kind"] != change.KindRestack || v["base_sha"] != b.HeadSHA || err != nil {
		t.Errorf("demo#%d depends on %v, its last version is %v on %v at %s (on the landed commit: %v); want on %d, a restack on %s",
			c.Number, c.DependsOn, v["kind"], v["base_sha"], head, err, b.Number, b.HeadSHA)
	}
}

func TestRestack_RebasesTheReparentedDependantsOfAnAbandonedChangeWithoutItsCommits(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.holdRestacks(t)
	for i, c := range []struct {
		name string
		// abandoned is the index of the abandoned change, whose dependant
		// is the one after it, the top of the stack.
		abandoned int
	}{
		{"the middle of three", 1},
		{"the bottom of two", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := fmt.Sprintf("r%d", i)
			names := []string{p + "a", p + "b", p + "c"}[:c.abandoned+2]
			s := w.stacked(t, names...)
			gone, dep := s[c.abandoned], s[c.abandoned+1]
			seq := w.seq(t)
			if status, out := w.abandon(t, gone.Number, w.ownerKey, map[string]any{"dependants": "reparent"}); status != http.StatusOK {
				t.Fatalf("abandon = %d %v, want 200", status, out)
			}
			w.runPending(t)

			var link *int64
			onto := w.head(t, "main")
			if c.abandoned > 0 {
				link, onto = &s[c.abandoned-1].Number, s[c.abandoned-1].HeadSHA
			}
			ch := w.read(t, dep.Number)
			vs := w.versions(t, "demo", dep.Number)
			v := vs[len(vs)-1]
			want := slices.Delete(slices.Clone(names), c.abandoned, c.abandoned+1)
			if got := w.subjects(t, w.base, ch.HeadSHA); !reflect.DeepEqual(ch.DependsOn, link) || v["kind"] != change.KindRestack || v["base_sha"] != onto || !slices.Equal(got, want) {
				t.Errorf("demo#%d depends on %v, its last version is %v on %v with %q; want on %v, a restack on %s with %q",
					dep.Number, ch.DependsOn, v["kind"], v["base_sha"], got, link, onto, want)
			}
			numbers, meta := w.restackEvents(t, seq)
			if i := slices.Index(numbers, strconv.FormatInt(dep.Number, 10)); i < 0 || meta[i]["trigger"] != change.TriggerAbandon {
				t.Errorf("restack events of %v with %v, want one of demo#%d for abandon", numbers, meta, dep.Number)
			}
		})
	}
}

func TestRestack_ReportsAConflictOnTheChangeAndMovesNothing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	release := w.hold(t)
	w.holdRestacks(t)
	a := commit(t, w.demo, "a", w.base, "A\n", map[string]string{"f.txt": "a\n"})
	w.openBranch(t, "a", nil)
	commit(t, w.demo, "b", a, "B\n", map[string]string{"f.txt": "b\n"})
	b := w.openBranch(t, "b", nil)
	commit(t, w.demo, "c", b.HeadSHA, "C\n", map[string]string{"c.txt": "c\n"})
	c := w.openBranch(t, "c", nil)
	seq := w.seq(t)
	w.amend(t, "a", map[string]string{"f.txt": "a2\n"})
	// What a replay could leave behind: a ref, a worktree, a lock file.
	leftovers := func() string {
		out := gitrepotest.Git(t, w.demo, "for-each-ref") + gitrepotest.Git(t, w.demo, "worktree", "list")
		err := filepath.WalkDir(w.demo, func(path string, d fs.DirEntry, err error) error {
			if err == nil && strings.HasSuffix(path, ".lock") {
				out += "\n" + path
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := leftovers()
	w.runPending(t)

	got := w.read(t, b.Number)
	if !got.NeedsRestack || deref(got.RestackReason) != change.RestackConflict || got.Mergeable != change.MergeableConflict ||
		got.Conflict == nil || got.Version != 1 || w.head(t, "b") != b.HeadSHA {
		t.Errorf("demo#%d: needs_restack %v for %q, mergeable %s, conflict %s, version %d, branch at %s; want true for conflict, conflict, a report, 1, %s",
			b.Number, got.NeedsRestack, deref(got.RestackReason), got.Mergeable, got.Conflict, got.Version, w.head(t, "b"), b.HeadSHA)
	}
	if above := w.read(t, c.Number); above.Version != 1 || w.head(t, "c") != c.HeadSHA || above.NeedsRestack || above.DependsOn == nil || *above.DependsOn != b.Number {
		t.Errorf("demo#%d above: version %d, branch at %s, needs_restack %v, depends_on %v; want 1, %s, false, %d",
			c.Number, above.Version, w.head(t, "c"), above.NeedsRestack, above.DependsOn, c.HeadSHA, b.Number)
	}
	if p := w.pending(t); len(p) != 0 {
		t.Errorf("pending restacks %v, want none", p)
	}
	if after := leftovers(); after != before {
		t.Errorf("the repository after the restack:\n%s\nbefore:\n%s", after, before)
	}
	events := w.changeEvents(t, seq, change.EventConflict, b.Number)
	if len(events) != 1 {
		t.Fatalf("%d parley.change.conflict events of demo#%d, want 1", len(events), b.Number)
	}
	m := metadata(t, events[0])
	if events[0].ActorType != change.ActorSystem || !reflect.DeepEqual(m["files"], []any{"f.txt"}) || m["needs_restack"] != true || m["version"] != 1.0 {
		t.Errorf("parley.change.conflict by %s with %v, want by the hub with files [f.txt], needs_restack true, version 1", events[0].ActorType, m)
	}

	// The check the new head below scheduled, run after the restack, keeps
	// the report of the change's own commits.
	release()
	w.quiet(t)
	if report := asJSON(t, w.read(t, b.Number).Conflict); report["commit_sha"] != b.HeadSHA || report["base_sha"] != a {
		t.Errorf("after the mergeability check the report is %v, want the commit %s on the base %s", report, b.HeadSHA, a)
	}

	// A second conflict replaces the report and announces nothing.
	again := w.amend(t, "a", map[string]string{"f.txt": "a3\n"})
	w.runPending(t)
	report := asJSON(t, w.read(t, b.Number).Conflict)
	if n := len(w.changeEvents(t, seq, change.EventConflict, b.Number)); n != 1 || report["target_sha"] != again {
		t.Errorf("%d parley.change.conflict events, the report onto %v; want 1, onto %s", n, report["target_sha"], again)
	}
}

func TestRestack_ReportsTheConflictOfTheChangesOwnCommits(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.hold(t)
	w.holdRestacks(t)
	for i, c := range []struct {
		name string
		// lower is the file the change below adds, own the file the
		// change's second commit sets on top of it, amended the file the
		// change below is amended to add instead.
		lower, own, amended string
		// missing are the sides of the conflicted file that lack it.
		missing []string
	}{
		{"both sides edit the same line", "f.txt", "f.txt", "f.txt", nil},
		{"both sides add the same path", "f.txt", "n.txt", "n.txt", []string{"base"}},
		{"the change below deletes a file the change edits", "f.txt", "f.txt", "z.txt", []string{"ours"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := fmt.Sprintf("k%d", i)
			a := commit(t, w.demo, p+"a", w.base, "A\n", map[string]string{p + c.lower: "a\n"})
			w.openBranch(t, p+"a", nil)
			first := commit(t, w.demo, p+"b", a, "B1\n", map[string]string{p + "b.txt": "b\n"})
			second := commit(t, w.demo, p+"b", first, "B2\n", map[string]string{p + c.own: "b\n"})
			b := w.openBranch(t, p+"b", nil)
			onto := w.amend(t, p+"a", map[string]string{p + c.amended: "a2\n"})
			w.runPending(t)

			rep := asJSON(t, w.read(t, b.Number).Conflict)
			keys := slices.Sorted(maps.Keys(rep))
			if want := []string{"base_sha", "change", "commit_sha", "files", "head_sha", "landed_changes", "target_sha"}; !slices.Equal(keys, want) {
				t.Fatalf("the report has %q, want %q", keys, want)
			}
			if rep["target_sha"] != onto || rep["head_sha"] != b.HeadSHA || rep["base_sha"] != a || rep["commit_sha"] != second ||
				!reflect.DeepEqual(rep["landed_changes"], []any{}) || rep["change"] != nil {
				t.Errorf("the report is %v; want onto %s, head %s, base %s, commit %s, no landed changes, no change", rep, onto, b.HeadSHA, a, second)
			}
			files, _ := rep["files"].([]any)
			if len(files) == 0 {
				t.Fatalf("the report names no file: %v", rep)
			}
			f, _ := files[0].(map[string]any)
			if f["path"] != p+c.own || !reflect.DeepEqual(f["landed_by"], []any{}) {
				t.Errorf("the first file is %v, want %s landed by none", f, p+c.own)
			}
			shas := []any{rep["target_sha"], rep["head_sha"], rep["base_sha"], rep["commit_sha"]}
			for _, side := range []string{"base", "ours", "theirs"} {
				if slices.Contains(c.missing, side) != (f[side] == nil) {
					t.Errorf("the %s side is %v, want it null: %v", side, f[side], slices.Contains(c.missing, side))
				}
				if s, ok := f[side].(map[string]any); ok {
					shas = append(shas, s["sha"])
				}
			}
			for _, sha := range shas {
				if s, _ := sha.(string); len(s) != 40 || strings.Trim(s, "0123456789abcdef") != "" {
					t.Errorf("%v is not a commit or blob id", sha)
				}
			}
		})
	}
}

func TestRestack_StartsAgainWhenTheChangeGetsAVersionDuringAConflictingReplay(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.hold(t)
	w.holdRestacks(t)
	a := commit(t, w.demo, "a", w.base, "A\n", map[string]string{"f.txt": "a\n"})
	w.openBranch(t, "a", nil)
	commit(t, w.demo, "b", a, "B\n", map[string]string{"f.txt": "b\n"})
	b := w.openBranch(t, "b", nil)
	onto := w.amend(t, "a", map[string]string{"f.txt": "a2\n"})
	change.SetRestackSeams(w.changes, change.RestackSeams{
		Rebase: func(ctx context.Context, repo *gitrepo.Repo, base, head, to string, at time.Time) (string, *gitrepo.ConflictReport, error) {
			if head == b.HeadSHA {
				// The author pushes b rebased by hand while the replay runs.
				fixed := commit(t, w.demo, "b", onto, "B\n", map[string]string{"f.txt": "b\n"})
				w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "b", Head: fixed, Kind: change.KindPush, Caller: w.ownerCaller()})
			}
			return repo.RebaseFrom(ctx, base, head, to, at)
		},
	})
	seq := w.seq(t)
	w.runPending(t)
	got := w.read(t, b.Number)
	if events := w.changeEvents(t, seq, change.EventConflict, b.Number); got.NeedsRestack || got.Mergeable == change.MergeableConflict || len(events) != 0 || len(w.pending(t)) != 0 {
		t.Errorf("needs_restack %v, mergeable %s, %d conflict events, pending %v; want false, not conflict, none, none",
			got.NeedsRestack, got.Mergeable, len(events), w.pending(t))
	}
}
