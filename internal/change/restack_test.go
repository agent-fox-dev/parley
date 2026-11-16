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
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hubtest"
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
			s := w.stacked(t, p+"a", p+"b")
			a, b := s[0], s[1]
			// b is pushed onto the next head of a, and flagged, before a
			// gets that head as a version.
			next := commit(t, w.demo, p+"a", a.HeadSHA, "A2\n", map[string]string{p + "a2.txt": "a2\n"})
			pushed := commit(t, w.demo, p+"b", next, "B\n", map[string]string{p + "b.txt": "b\n"})
			w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: p + "b", Head: pushed, Kind: change.KindPush, Caller: w.ownerCaller()})
			if reason != "" {
				w.write(t, func(ctx context.Context, tx *sql.Tx) error {
					return w.changes.SetNeedsRestack(ctx, tx, "demo", b.Number, true, &reason)
				})
			}
			seq := w.seq(t)
			w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: p + "a", Head: next, Kind: change.KindPush, Caller: w.ownerCaller()})
			w.runPending(t)
			ch := w.read(t, b.Number)
			if vs := w.versions(t, "demo", b.Number); ch.Version != 2 || vs[1]["kind"] != change.KindPush || w.head(t, p+"b") != pushed {
				t.Errorf("demo#%d is at version %d of kind %v, its branch at %s; want 2 of kind push at %s", b.Number, ch.Version, vs[len(vs)-1]["kind"], w.head(t, p+"b"), pushed)
			}
			var touched []string
			for _, e := range w.changeEventsOf(t, seq, b.Number) {
				if e.Type == change.EventVersion || e.Type == change.EventRestack || e.Type == change.EventConflict {
					touched = append(touched, e.Type)
				}
			}
			if len(touched) != 0 || slices.Contains(w.pending(t), b.Number) || ch.NeedsRestack {
				t.Errorf("events %v, pending %v, needs_restack %v for %q; want none, none, false", touched, w.pending(t), ch.NeedsRestack, deref(ch.RestackReason))
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
			below := w.amend(t, p+"a", map[string]string{p + "a2.txt": "a2\n"})
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
				// The author's own push onto the head below lets the kept
				// restack run again, which finds the change on it, and its
				// mergeability is computed.
				pushed := commit(t, w.demo, p+"b", below, "B again\n", map[string]string{p + "b.txt": "b\n"})
				w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: p + "b", Head: pushed, Kind: change.KindPush, Caller: w.ownerCaller()})
				w.runPending(t)
				if ch := w.read(t, b.Number); ch.Version != 2 || ch.Mergeable != change.MergeableClean || len(w.pending(t)) != 0 {
					t.Errorf("after the push demo#%d is at version %d, mergeable %s, pending %v; want 2, clean, none", b.Number, ch.Version, ch.Mergeable, w.pending(t))
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

func TestRestack_LeavesAChangeAPushPutOnTheNewHeadBelowWhereItIs(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.holdRestacks(t)
	for i, c := range []struct {
		name string
		// recorded is whether the push's version is recorded before the
		// swap, rather than after the restack.
		recorded bool
	}{
		{"its version is recorded before the swap", true},
		{"its version is not recorded yet", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := fmt.Sprintf("q%d", i)
			b := w.stacked(t, p+"a", p+"b")[1]
			below := w.amend(t, p+"a", map[string]string{p + "a2.txt": "a2\n"})
			// The author pushes b rebased onto the head below while the
			// restack runs.
			pushed := commit(t, w.demo, p+"b", below, "B\n", map[string]string{p + "b.txt": "b\n"})
			change.SetRestackSeams(w.changes, change.RestackSeams{
				BeforeSwap: func(_ context.Context, _ string, number int64, attempt int) {
					if c.recorded && number == b.Number && attempt == 1 {
						w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: p + "b", Head: pushed, Kind: change.KindPush, Caller: w.ownerCaller()})
					}
				},
			})
			defer change.SetRestackSeams(w.changes, change.RestackSeams{})
			seq := w.seq(t)
			w.runPending(t)
			kinds := []any{}
			for _, v := range w.versions(t, "demo", b.Number) {
				kinds = append(kinds, v["kind"])
			}
			want := []any{change.KindRef}
			if c.recorded {
				want = append(want, change.KindPush)
			}
			events, _ := w.restackEvents(t, seq)
			if !reflect.DeepEqual(kinds, want) || w.head(t, p+"b") != pushed || len(events) != 0 || len(w.pending(t)) != 0 {
				t.Errorf("demo#%d has versions of kinds %v, its branch at %s, restack events %v, pending %v; want %v, %s, none, none",
					b.Number, kinds, w.head(t, p+"b"), events, w.pending(t), want, pushed)
			}
		})
	}
}

func TestRestack_LeavesAChangeNoLongerOnAnotherWhereItIs(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.holdRestacks(t)
	for i, c := range []struct {
		name string
		// unlink takes the change b, with a restack pending onto the change
		// below, off it.
		unlink func(t *testing.T, p string, b change.Change)
	}{
		{"by PATCH", func(t *testing.T, _ string, b change.Change) {
			if status, got := w.patch(t, b.Number, w.ownerKey, map[string]any{"depends_on": nil}); status != http.StatusOK {
				t.Fatalf("PATCH depends_on null = %d %v, want 200", status, got)
			}
		}},
		{"by a push onto its target", func(t *testing.T, p string, _ change.Change) {
			head := commit(t, w.demo, p+"b", w.head(t, "main"), "B on main\n", map[string]string{p + "b.txt": "b\n"})
			w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: p + "b", Head: head, Kind: change.KindPush, Caller: w.ownerCaller()})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := fmt.Sprintf("n%d", i)
			b := w.stacked(t, p+"a", p+"b")[1]
			w.amend(t, p+"a", map[string]string{p + "a2.txt": "a2\n"})
			c.unlink(t, p, b)
			before := w.read(t, b.Number)
			// The target moves on, so that a restack onto it would move b.
			commit(t, w.demo, "main", w.head(t, "main"), "main moved\n", map[string]string{p + "m.txt": "m\n"})
			seq := w.seq(t)
			w.runPending(t)
			ch := w.read(t, b.Number)
			events, _ := w.restackEvents(t, seq)
			if ch.DependsOn != nil || ch.Version != before.Version || w.head(t, p+"b") != before.HeadSHA || len(events) != 0 ||
				len(w.pending(t)) != 0 || ch.Mergeable == change.MergeableUnknown {
				t.Errorf("demo#%d depends on %v, at version %d, its branch at %s, restack events %v, pending %v, mergeable %s; want none, %d, %s, none, none, computed",
					b.Number, ch.DependsOn, ch.Version, w.head(t, p+"b"), events, w.pending(t), ch.Mergeable, before.Version, before.HeadSHA)
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
		// off turns the workspace's automatic restacks off.
		off bool
	}{
		{"the middle of three", 1, false},
		{"the bottom of two", 0, false},
		{"the middle of three, with automatic restacks off", 1, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.off {
				w.set(t, "demo", "settings", `{"changes.auto_restack":false}`)
				defer w.set(t, "demo", "settings", `{}`)
			}
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
			if c.off {
				events, _ := w.restackEvents(t, seq)
				if !reflect.DeepEqual(ch.DependsOn, link) || !ch.NeedsRestack || deref(ch.RestackReason) != change.RestackAutoOff ||
					ch.Version != 1 || w.head(t, dep.Source.Branch) != dep.HeadSHA || len(w.pending(t)) != 0 || len(events) != 0 {
					t.Errorf("demo#%d depends on %v, needs_restack %v for %q, version %d, branch at %s, pending %v, restack events %v; want on %v, true for auto_restack_off, 1, %s, none, none",
						dep.Number, ch.DependsOn, ch.NeedsRestack, deref(ch.RestackReason), ch.Version, w.head(t, dep.Source.Branch), w.pending(t), events, link, dep.HeadSHA)
				}
				return
			}
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

	// A push onto the head below clears the conflict, so the next one, found
	// before a check computed the change's mergeability again, is announced.
	w.hold(t)
	fixed := commit(t, w.demo, "b", again, "B again\n", map[string]string{"f.txt": "b\n"})
	w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "b", Head: fixed, Kind: change.KindPush, Caller: w.ownerCaller()})
	w.amend(t, "a", map[string]string{"f.txt": "a4\n"})
	w.runPending(t)
	events = w.changeEvents(t, seq, change.EventConflict, b.Number)
	if len(events) != 2 || metadata(t, events[1])["needs_restack"] != true || metadata(t, events[1])["version"] != 2.0 {
		t.Errorf("%d parley.change.conflict events, want 2, the second of version 2 with needs_restack true", len(events))
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
		// kind is the kind of the conflicted file, missing the sides of it
		// that lack it.
		kind    string
		missing []string
	}{
		{"both sides edit the same line", "f.txt", "f.txt", "f.txt", gitrepo.ConflictContent, nil},
		{"both sides add the same path", "f.txt", "n.txt", "n.txt", gitrepo.ConflictAddAdd, []string{"base"}},
		{"the change below deletes a file the change edits", "f.txt", "f.txt", "z.txt", gitrepo.ConflictDeleteModify, []string{"ours"}},
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
			if f["path"] != p+c.own || f["kind"] != c.kind || !reflect.DeepEqual(f["landed_by"], []any{}) {
				t.Errorf("the first file is %v, want %s of kind %s landed by none", f, p+c.own, c.kind)
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

func TestRestack_StartsAgainWhenTheChangeMovesDuringAConflictingReplay(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// move changes demo#b while its replay runs; onto is the new head
		// of the change below it.
		move func(t *testing.T, w *world, b change.Change, onto string)
		// conflict is whether the restack that starts again conflicts too.
		conflict bool
	}{
		{"the author pushes b rebased by hand", func(t *testing.T, w *world, b change.Change, onto string) {
			fixed := commit(t, w.demo, "b", onto, "B\n", map[string]string{"f.txt": "b\n"})
			w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "b", Head: fixed, Kind: change.KindPush, Caller: w.ownerCaller()})
		}, false},
		{"b is measured from another base", func(t *testing.T, w *world, b change.Change, _ string) {
			w.write(t, func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `UPDATE changes SET base_sha = ? WHERE workspace_slug = 'demo' AND number = ?`, w.base, b.Number)
				return err
			})
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.hold(t)
			w.holdRestacks(t)
			a := commit(t, w.demo, "a", w.base, "A\n", map[string]string{"f.txt": "a\n"})
			w.openBranch(t, "a", nil)
			commit(t, w.demo, "b", a, "B\n", map[string]string{"f.txt": "b\n"})
			b := w.openBranch(t, "b", nil)
			onto := w.amend(t, "a", map[string]string{"f.txt": "a2\n"})
			var moved atomic.Bool
			change.SetRestackSeams(w.changes, change.RestackSeams{
				Rebase: func(ctx context.Context, repo *gitrepo.Repo, base, head, to string, at time.Time) (string, *gitrepo.ConflictReport, error) {
					if head == b.HeadSHA && !moved.Swap(true) {
						c.move(t, w, b, onto)
					}
					return repo.RebaseFrom(ctx, base, head, to, at)
				},
			})
			seq := w.seq(t)
			w.runPending(t)
			got := w.read(t, b.Number)
			events := w.changeEvents(t, seq, change.EventConflict, b.Number)
			if (got.Mergeable == change.MergeableConflict) != c.conflict || got.NeedsRestack != c.conflict || len(events) != map[bool]int{false: 0, true: 1}[c.conflict] || len(w.pending(t)) != 0 {
				t.Errorf("mergeable %s, needs_restack %v, %d conflict events, pending %v; want a conflict %v, no pending restack",
					got.Mergeable, got.NeedsRestack, len(events), w.pending(t), c.conflict)
			}
			// A stored report is one of the change as it is now.
			if report := asJSON(t, got.Conflict); c.conflict && (report["base_sha"] != deref(got.BaseSHA) || report["head_sha"] != got.HeadSHA) {
				t.Errorf("the report is on the base %v of the head %v, want %s of %s", report["base_sha"], report["head_sha"], deref(got.BaseSHA), got.HeadSHA)
			}
		})
	}
}

func TestRestack_RebasesAChangeOntoTheChangeItsCallerLinkedItTo(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.holdRestacks(t)
	for i, c := range []struct {
		name string
		// link opens the change of the branch p+"b", built on main, and
		// links it to the change a.
		link func(t *testing.T, p string, a change.Change) change.Change
	}{
		{"by PATCH", func(t *testing.T, p string, a change.Change) change.Change {
			w.branch(t, p+"b", "B\n\nWhy.\n")
			b := w.openBranch(t, p+"b", nil)
			if status, got := w.patch(t, b.Number, w.ownerKey, map[string]any{"depends_on": a.Number}); status != http.StatusOK {
				t.Fatalf("PATCH depends_on %d = %d %v, want 200", a.Number, status, got)
			}
			return b
		}},
		{"at open", func(t *testing.T, p string, a change.Change) change.Change {
			w.branch(t, p+"b", "B\n\nWhy.\n")
			return w.openBranch(t, p+"b", func(in *change.OpenInput) { in.DependsOn = new(a.Number) })
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := fmt.Sprintf("x%d", i)
			a := w.stacked(t, p+"a")[0]
			seq := w.seq(t)
			b := c.link(t, p, a)
			if !slices.Contains(w.pending(t), b.Number) {
				t.Fatalf("pending restacks of %v, want one of demo#%d", w.pending(t), b.Number)
			}
			w.runPending(t)

			vs := w.versions(t, "demo", b.Number)
			v := vs[len(vs)-1]
			created, _ := v["created_by"].(map[string]any)
			head, _ := v["head_sha"].(string)
			_, err := gitrepotest.Run(w.demo, "", "merge-base", "--is-ancestor", a.HeadSHA, head)
			if v["kind"] != change.KindRestack || created["actor_type"] != change.ActorSystem || v["base_sha"] != a.HeadSHA || err != nil {
				t.Errorf("the last version is %v by %v on %v at %s (on %s: %v); want a restack by the hub on %s", v["kind"], created["actor_type"], v["base_sha"], head, a.HeadSHA, err, a.HeadSHA)
			}
			numbers, meta := w.restackEvents(t, seq)
			if !slices.Equal(numbers, []string{strconv.FormatInt(b.Number, 10)}) || meta[0]["trigger"] != change.TriggerManual {
				t.Errorf("restack events of %v with %v, want one of demo#%d for manual", numbers, meta, b.Number)
			}
		})
	}
}

func TestRestack_OnlyFlagsTheChangesBehindWhileAutomaticRestacksAreOff(t *testing.T) {
	t.Parallel()
	w := newWorldWith(t, hubtest.Config{TOML: zeroDebounce + "max_stack_depth = 2\n"})
	w.set(t, "demo", "settings", `{"changes.auto_restack":false}`)
	w.holdRestacks(t)
	land := func(t *testing.T, a change.Change, files map[string]string) {
		landed := w.land(t, a.Number, files)
		if err := w.changes.OnLanded(context.Background(), "demo", a.Number, landed); err != nil {
			t.Fatalf("OnLanded = %v", err)
		}
	}
	for i, c := range []struct {
		name string
		// move builds the changes, each branch named with the prefix p,
		// moves what is below them and returns the changes that must be
		// flagged, as they were before, and those that must not.
		move func(t *testing.T, p string) (behind, on []change.Change)
	}{
		{"the change below gets a new version", func(t *testing.T, p string) (behind, on []change.Change) {
			s := w.stacked(t, p+"a", p+"b")
			commit(t, w.demo, p+"c", s[0].HeadSHA, "C\n", map[string]string{p + "c.txt": "c\n"})
			cc := w.openBranch(t, p+"c", nil)
			head := commit(t, w.demo, p+"a", s[0].HeadSHA, "A2\n", map[string]string{p + "a2.txt": "a2\n"})
			w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: p + "a", Head: head, Kind: change.KindPush, Caller: w.ownerCaller()})
			pushed := commit(t, w.demo, p+"c", head, "C\n", map[string]string{p + "c.txt": "c\n"})
			w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: p + "c", Head: pushed, Kind: change.KindPush, Caller: w.ownerCaller()})
			return []change.Change{s[1]}, []change.Change{cc}
		}},
		{"the change below lands", func(t *testing.T, p string) (behind, on []change.Change) {
			s := w.stacked(t, p+"a", p+"b")
			land(t, s[0], map[string]string{p + "a.txt": p + "a\n"})
			if b := w.read(t, s[1].Number); b.DependsOn != nil {
				t.Errorf("demo#%d depends on %d, want none", b.Number, *b.DependsOn)
			}
			return []change.Change{s[1]}, nil
		}},
		{"the caller links the change to one it is not built on", func(t *testing.T, p string) (behind, on []change.Change) {
			a := w.stacked(t, p+"a")[0]
			w.branch(t, p+"b", "B\n\nWhy.\n")
			b := w.openBranch(t, p+"b", nil)
			status, got := w.patch(t, b.Number, w.ownerKey, map[string]any{"depends_on": a.Number})
			if status != http.StatusOK || got["needs_restack"] != true || got["restack_reason"] != change.RestackAutoOff {
				t.Errorf("PATCH depends_on %d = %d %v, want 200 with needs_restack for auto_restack_off", a.Number, status, got)
			}
			return []change.Change{b}, nil
		}},
		{"a change too deep in its stack comes back within the limit", func(t *testing.T, p string) (behind, on []change.Change) {
			s := w.stacked(t, p+"a", p+"b", p+"c")
			w.amend(t, p+"b", map[string]string{p + "b2.txt": "b2\n"})
			b := w.read(t, s[1].Number)
			land(t, s[0], map[string]string{p + "a.txt": p + "a\n"})
			return []change.Change{b, s[2]}, nil
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			seq := w.seq(t)
			behind, on := c.move(t, fmt.Sprintf("o%d", i))
			for _, before := range behind {
				ch := w.read(t, before.Number)
				if !ch.NeedsRestack || deref(ch.RestackReason) != change.RestackAutoOff || ch.Version != before.Version || w.head(t, ch.Source.Branch) != before.HeadSHA {
					t.Errorf("demo#%d: needs_restack %v for %q, version %d, branch at %s; want true for auto_restack_off, %d, %s",
						ch.Number, ch.NeedsRestack, deref(ch.RestackReason), ch.Version, w.head(t, ch.Source.Branch), before.Version, before.HeadSHA)
				}
			}
			for _, o := range on {
				if ch := w.read(t, o.Number); ch.NeedsRestack {
					t.Errorf("demo#%d on the new head below: needs_restack for %q, want false", ch.Number, deref(ch.RestackReason))
				}
			}
			if events, _ := w.restackEvents(t, seq); len(w.pending(t)) != 0 || len(events) != 0 {
				t.Errorf("pending restacks %v, restack events %v; want none", w.pending(t), events)
			}
		})
	}
}

func TestRestack_LeavesAChangeTooDeepInItsStackWhereItIs(t *testing.T) {
	t.Parallel()
	w := newWorldWith(t, hubtest.Config{TOML: zeroDebounce + "max_stack_depth = 2\n"})
	w.holdRestacks(t)
	for i, c := range []struct {
		name string
		// move builds the stack, each branch named with the prefix p, moves
		// a change below the change too deep and returns the change that
		// must be restacked and the one too deep, as it was before.
		move func(t *testing.T, p string) (moved, deep change.Change)
	}{
		{"the change below gets a new version", func(t *testing.T, p string) (moved, deep change.Change) {
			s := w.stacked(t, p+"a", p+"b", p+"c")
			w.amend(t, p+"a", map[string]string{p + "a2.txt": "a2\n"})
			return s[1], s[2]
		}},
		{"the change was flagged for a conflict since", func(t *testing.T, p string) (moved, deep change.Change) {
			s := w.stacked(t, p+"a", p+"b", p+"c")
			w.write(t, func(ctx context.Context, tx *sql.Tx) error {
				return w.changes.SetNeedsRestack(ctx, tx, "demo", s[2].Number, true, new(change.RestackConflict))
			})
			w.amend(t, p+"a", map[string]string{p + "a2.txt": "a2\n"})
			return s[1], s[2]
		}},
		{"the change became too deep after its restack was recorded", func(t *testing.T, p string) (moved, deep change.Change) {
			x := w.stacked(t, p+"x")[0]
			s := w.stacked(t, p+"a", p+"b")
			w.amend(t, p+"a", map[string]string{p + "a2.txt": "a2\n"})
			if status, got := w.patch(t, s[0].Number, w.ownerKey, map[string]any{"depends_on": x.Number}); status != http.StatusOK {
				t.Fatalf("PATCH depends_on %d = %d %v, want 200", x.Number, status, got)
			}
			return s[0], s[1]
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			moved, deep := c.move(t, fmt.Sprintf("t%d", i))
			w.runPending(t)
			vs := w.versions(t, "demo", moved.Number)
			if v := vs[len(vs)-1]; v["kind"] != change.KindRestack {
				t.Errorf("demo#%d's last version is %v, want a restack", moved.Number, v["kind"])
			}
			ch := w.read(t, deep.Number)
			if ch.Version != deep.Version || w.head(t, ch.Source.Branch) != deep.HeadSHA || slices.Contains(w.pending(t), deep.Number) ||
				!ch.NeedsRestack || deref(ch.RestackReason) != change.RestackTooDeep {
				t.Errorf("demo#%d too deep: version %d, branch at %s, pending %v, needs_restack %v for %q; want %d, %s, not pending, true for stack_too_deep",
					ch.Number, ch.Version, w.head(t, ch.Source.Branch), w.pending(t), ch.NeedsRestack, deref(ch.RestackReason), deep.Version, deep.HeadSHA)
			}
		})
	}
}

func TestRestack_RebasesAChangeBackWithinTheStackDepthLimit(t *testing.T) {
	t.Parallel()
	w := newWorldWith(t, hubtest.Config{TOML: zeroDebounce + "max_stack_depth = 2\n"})
	w.holdRestacks(t)
	s := w.stacked(t, "a", "b", "c")
	w.amend(t, "a", map[string]string{"a2.txt": "a2\n"})
	w.runPending(t)
	if c := w.read(t, s[2].Number); c.Version != 1 {
		t.Fatalf("demo#%d too deep is at version %d, want 1", c.Number, c.Version)
	}
	landed := w.land(t, s[0].Number, map[string]string{"a2.txt": "a2\n"})
	if err := w.changes.OnLanded(context.Background(), "demo", s[0].Number, landed); err != nil {
		t.Fatalf("OnLanded = %v", err)
	}
	w.runPending(t)

	b, c := w.read(t, s[1].Number), w.read(t, s[2].Number)
	vs := w.versions(t, "demo", c.Number)
	v := vs[len(vs)-1]
	if b.DependsOn != nil || c.NeedsRestack || c.RestackReason != nil || v["kind"] != change.KindRestack || v["base_sha"] != b.HeadSHA {
		t.Errorf("demo#%d depends on %v; demo#%d needs_restack %v for %q, its last version %v on %v; want none; false for none, a restack on %s",
			b.Number, b.DependsOn, c.Number, c.NeedsRestack, deref(c.RestackReason), v["kind"], v["base_sha"], b.HeadSHA)
	}
}
