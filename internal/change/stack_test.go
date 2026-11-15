package change_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hubtest"
)

func TestLinkVersion_LinksAChangeToTheNearestChangeBelowIt(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	cases := []struct {
		name string
		// setup opens the changes, each branch named with the prefix p,
		// and returns the change to look at and the one it sits on.
		setup func(t *testing.T, p string) (number, lower int64)
		// equivalent is the patch_equivalent_to of its last version.
		equivalent any
	}{
		{"on top of another", func(t *testing.T, p string) (int64, int64) {
			s := w.stacked(t, p+"a", p+"b")
			return s[1].Number, s[0].Number
		}, nil},
		{"on top of two, the nearer one", func(t *testing.T, p string) (int64, int64) {
			s := w.stacked(t, p+"a", p+"b", p+"c")
			return s[2].Number, s[1].Number
		}, nil},
		{"on a draft", func(t *testing.T, p string) (int64, int64) {
			a := commit(t, w.demo, p+"a", w.base, "A\n", map[string]string{p + "a.txt": "a\n"})
			lower := w.openBranch(t, p+"a", func(in *change.OpenInput) { in.Draft = true })
			commit(t, w.demo, p+"b", a, "B\n", map[string]string{p + "b.txt": "b\n"})
			return w.openBranch(t, p+"b", nil).Number, lower.Number
		}, nil},
		{"from a fork, on a change of its parent", func(t *testing.T, p string) (int64, int64) {
			lower := w.stacked(t, p+"a")[0]
			fork := w.addFork(t, "demo--t1", "main", nil, nil)
			commit(t, fork, p+"b", lower.HeadSHA, "B\n", map[string]string{p + "b.txt": "b\n"})
			return w.openBranch(t, p+"b", func(in *change.OpenInput) { in.SourceWorkspace = "demo--t1" }).Number, lower.Number
		}, nil},
		{"at a later version", func(t *testing.T, p string) (int64, int64) {
			lower := w.stacked(t, p+"a")[0]
			own := map[string]string{p + "b.txt": "b\n"}
			commit(t, w.demo, p+"b", w.base, "B\n", own)
			number := w.openBranch(t, p+"b", nil).Number
			head := commit(t, w.demo, p+"b", lower.HeadSHA, "B\n", own)
			w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: p + "b", Head: head, Kind: change.KindPush, Caller: w.ownerCaller()})
			return number, lower.Number
		}, 1.0},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seq := w.seq(t)
			number, lower := c.setup(t, fmt.Sprintf("s%d", i))
			ch, below := w.read(t, number), w.read(t, lower)
			if ch.DependsOn == nil || *ch.DependsOn != lower || deref(ch.BaseSHA) != below.HeadSHA {
				t.Errorf("depends_on, base_sha = %v, %s; want %d, %s", ch.DependsOn, deref(ch.BaseSHA), lower, below.HeadSHA)
			}
			vs := w.versions(t, "demo", number)
			if v := vs[len(vs)-1]; v["base_sha"] != below.HeadSHA || v["patch_equivalent_to"] != c.equivalent {
				t.Errorf("the version's base_sha, patch_equivalent_to = %v, %v; want %s, %v", v["base_sha"], v["patch_equivalent_to"], below.HeadSHA, c.equivalent)
			}
			if m := w.versionEvent(t, seq, number); m["base_sha"] != below.HeadSHA || m["patch_equivalent_to"] != c.equivalent {
				t.Errorf("parley.change.version has base_sha, patch_equivalent_to = %v, %v; want %s, %v", m["base_sha"], m["patch_equivalent_to"], below.HeadSHA, c.equivalent)
			}
		})
	}
}

func TestLinkVersion_LeavesAChangeWithNothingBelowItUnlinked(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	cases := []struct {
		name string
		// setup opens the changes, each branch named with the prefix p,
		// and returns the change to look at and the one it sat on, 0 for
		// none.
		setup func(t *testing.T, p string) (number, former int64)
	}{
		{"a first change on main", func(t *testing.T, p string) (int64, int64) {
			commit(t, w.demo, p+"a", w.base, "A\n", map[string]string{p + "a.txt": "a\n"})
			return w.openBranch(t, p+"a", nil).Number, 0
		}},
		{"a dependant force-pushed onto main", func(t *testing.T, p string) (int64, int64) {
			s := w.stacked(t, p+"a", p+"b")
			if b := w.read(t, s[1].Number); b.DependsOn == nil || *b.DependsOn != s[0].Number {
				t.Fatalf("depends_on before the push = %v, want %d", b.DependsOn, s[0].Number)
			}
			head := commit(t, w.demo, p+"b", w.base, "B again\n", map[string]string{p + "b.txt": "b2\n"})
			w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: p + "b", Head: head, Kind: change.KindPush, Caller: w.ownerCaller()})
			return s[1].Number, s[0].Number
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			number, former := c.setup(t, fmt.Sprintf("u%d", i))
			ch := w.read(t, number)
			mergeBase := gitrepotest.Git(t, w.demo, "merge-base", ch.HeadSHA, "main")
			if ch.DependsOn != nil || deref(ch.BaseSHA) != mergeBase {
				t.Errorf("depends_on, base_sha = %v, %s; want none, %s", ch.DependsOn, deref(ch.BaseSHA), mergeBase)
			}
			if former != 0 && slices.Contains(w.read(t, former).Dependants, number) {
				t.Errorf("the dependants of %d = %v, want no %d", former, w.read(t, former).Dependants, number)
			}
		})
	}
}

func TestLinkVersion_LinksOnlyToADraftOrOpenChangeOfTheSameTargetBelowTheHead(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	other := w.addWorkspace(t, "other")
	// closed opens a change from the new branch name, closes it with fn and
	// returns its head.
	closed := func(t *testing.T, name string, fn func(ctx context.Context, tx *sql.Tx, n int64) error) string {
		lower := w.stacked(t, name)[0]
		w.write(t, func(ctx context.Context, tx *sql.Tx) error { return fn(ctx, tx, lower.Number) })
		return lower.HeadSHA
	}
	cases := []struct {
		name string
		// setup opens the candidates, each branch named with the prefix
		// p, and returns the head of the new change and the number it
		// must depend on, 0 for none.
		setup func(t *testing.T, p string) (head string, want int64)
	}{
		{"a landed change", func(t *testing.T, p string) (string, int64) {
			below := closed(t, p+"a", func(ctx context.Context, tx *sql.Tx, n int64) error {
				return w.changes.SetLanded(ctx, tx, "demo", n, change.Landed{SHA: w.base, At: time.Now(), By: change.Actor{ActorType: change.ActorSystem}, LandID: "l1"})
			})
			return commit(t, w.demo, p+"new", below, "N\n", map[string]string{p + "n.txt": "n\n"}), 0
		}},
		{"an abandoned change", func(t *testing.T, p string) (string, int64) {
			below := closed(t, p+"a", func(ctx context.Context, tx *sql.Tx, n int64) error {
				return w.changes.SetAbandoned(ctx, tx, "demo", n, "superseded")
			})
			return commit(t, w.demo, p+"new", below, "N\n", map[string]string{p + "n.txt": "n\n"}), 0
		}},
		{"a change into another target branch", func(t *testing.T, p string) (string, int64) {
			below := commit(t, w.demo, p+"a", w.base, "A\n", map[string]string{p + "a.txt": "a\n"})
			w.openBranch(t, p+"a", func(in *change.OpenInput) { in.TargetBranch = new("release") })
			return commit(t, w.demo, p+"new", below, "N\n", map[string]string{p + "n.txt": "n\n"}), 0
		}},
		{"a change of another workspace with the same commits", func(t *testing.T, p string) (string, int64) {
			files := map[string]string{p + "a.txt": "a\n"}
			below := commit(t, other, p+"a", w.base, "A\n", files)
			w.openBranch(t, p+"a", func(in *change.OpenInput) { in.Workspace = "other" })
			if mine := commit(t, w.demo, p+"a", w.base, "A\n", files); mine != below {
				t.Fatalf("demo's commit %s is not other's %s", mine, below)
			}
			return commit(t, w.demo, p+"new", below, "N\n", map[string]string{p + "n.txt": "n\n"}), 0
		}},
		{"a change with the same head", func(t *testing.T, p string) (string, int64) {
			return w.stacked(t, p+"a")[0].HeadSHA, 0
		}},
		{"two changes with the nearest head, the lower number", func(t *testing.T, p string) (string, int64) {
			lower := w.stacked(t, p+"a")[0]
			gitrepotest.Git(t, w.demo, "update-ref", "refs/heads/"+p+"b", lower.HeadSHA)
			w.openBranch(t, p+"b", nil)
			return commit(t, w.demo, p+"new", lower.HeadSHA, "N\n", map[string]string{p + "n.txt": "n\n"}), lower.Number
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := fmt.Sprintf("o%d", i)
			head, want := c.setup(t, p)
			gitrepotest.Git(t, w.demo, "update-ref", "refs/heads/"+p+"new", head)
			got := w.read(t, w.openBranch(t, p+"new", nil).Number).DependsOn
			if (want == 0) != (got == nil) || got != nil && *got != want {
				t.Errorf("depends_on = %v, want %d (0 for none)", got, want)
			}
		})
	}
}

func TestDependants_AreTheDraftAndOpenChangesLinkedDirectlyAbove(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	s := w.stacked(t, "a", "b")
	a, b := s[0], s[1]
	commit(t, w.demo, "c", a.HeadSHA, "C\n", map[string]string{"c.txt": "c\n"})
	c := w.openBranch(t, "c", nil)
	commit(t, w.demo, "d", b.HeadSHA, "D\n", map[string]string{"d.txt": "d\n"})
	d := w.openBranch(t, "d", nil)
	check := func(t *testing.T, want map[int64][]int64) {
		t.Helper()
		for n, deps := range want {
			if got := w.read(t, n).Dependants; !reflect.DeepEqual(got, deps) {
				t.Errorf("the dependants of %d = %v, want %v", n, got, deps)
			}
		}
	}
	check(t, map[int64][]int64{a.Number: {b.Number, c.Number}, b.Number: {d.Number}, d.Number: {}})
	w.write(t, func(ctx context.Context, tx *sql.Tx) error {
		return w.changes.SetAbandoned(ctx, tx, "demo", c.Number, "superseded")
	})
	check(t, map[int64][]int64{a.Number: {b.Number}})
}

func TestLinkVersion_MeasuresADependantByItsOwnCommits(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	a1 := commit(t, w.demo, "a", w.base, "A1\n", map[string]string{"a.txt": "x1\nl2\nl3\n"})
	a2 := commit(t, w.demo, "a", a1, "A2\n", map[string]string{"a.txt": "x1\nx2\nl3\n"})
	w.openBranch(t, "a", nil)
	commit(t, w.demo, "b", a2, "B\n", map[string]string{"b.txt": "b\n"})
	b := w.read(t, w.openBranch(t, "b", nil).Number)
	if b.Stats.Commits != 1 || b.Stats.FilesChanged != 1 || !reflect.DeepEqual(b.Paths, []string{"b.txt"}) {
		t.Errorf("stats, paths = %+v, %v; want 1 commit, 1 file, [b.txt]", b.Stats, b.Paths)
	}
}

func TestDependsOn_LinksTheChangeToTheOneTheCallerNames(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	cases := []struct {
		name string
		// send opens the changes, each branch named with the prefix p,
		// sends the request that names the lower change and returns its
		// answer and the lower change.
		send func(t *testing.T, p string) (int, map[string]any, change.Change)
		// want is the status of the answer; built says whether the change
		// is built on the lower change's head.
		want  int
		built bool
	}{
		{"at open, the later of two changes with the same head", func(t *testing.T, p string) (int, map[string]any, change.Change) {
			a := w.stacked(t, p+"a")[0]
			gitrepotest.Git(t, w.demo, "update-ref", "refs/heads/"+p+"a2", a.HeadSHA)
			lower := w.openBranch(t, p+"a2", nil)
			commit(t, w.demo, p+"b", a.HeadSHA, "B\n\nWhy.\n", map[string]string{p + "b.txt": "b\n"})
			status, got := w.post(t, "demo", w.ownerKey, map[string]any{"source": map[string]any{"branch": p + "b"}, "depends_on": lower.Number})
			return status, got, lower
		}, http.StatusCreated, true},
		{"by PATCH, built on the lower change, after its link was cleared", func(t *testing.T, p string) (int, map[string]any, change.Change) {
			s := w.stacked(t, p+"a", p+"b")
			if status, got := w.patch(t, s[1].Number, w.ownerKey, `{"depends_on":null}`); status != http.StatusOK {
				t.Fatalf("PATCH depends_on null = %d %v", status, got)
			}
			status, got := w.patch(t, s[1].Number, w.ownerKey, map[string]any{"depends_on": s[0].Number})
			return status, got, s[0]
		}, http.StatusOK, true},
		{"by PATCH, built on main only", func(t *testing.T, p string) (int, map[string]any, change.Change) {
			lower := w.stacked(t, p+"a")[0]
			w.branch(t, p+"b", "B\n\nWhy.\n")
			status, got := w.patch(t, w.openBranch(t, p+"b", nil).Number, w.ownerKey, map[string]any{"depends_on": lower.Number})
			return status, got, lower
		}, http.StatusOK, false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, got, lower := c.send(t, fmt.Sprintf("e%d", i))
			if status != c.want || got["depends_on"] != float64(lower.Number) {
				t.Fatalf("answer = %d %v, want %d with depends_on %d", status, got, c.want, lower.Number)
			}
			if c.built && got["base_sha"] != lower.HeadSHA {
				t.Errorf("base_sha = %v, want the lower change's head %s", got["base_sha"], lower.HeadSHA)
			}
			if number, _ := got["number"].(float64); !slices.Contains(w.read(t, lower.Number).Dependants, int64(number)) {
				t.Errorf("the dependants of %d = %v, want %v among them", lower.Number, w.read(t, lower.Number).Dependants, number)
			}
		})
	}
}

func TestDependsOn_RefusesALinkToAChangeTheChangeCannotSitOn(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	s := w.stacked(t, "a", "b", "c")
	_, abandoned, landed := closedChanges(t, w)
	w.branch(t, "r", "R\n\nWhy.\n")
	release := w.openBranch(t, "r", func(in *change.OpenInput) { in.TargetBranch = new("release") }).Number
	w.branch(t, "new", "New\n\nWhy.\n")
	a := s[0].Number
	cases := []struct {
		name      string
		dependsOn int64
		// title also renames the change; atOpen also sends the value at
		// open.
		title, atOpen bool
	}{
		{"an unknown change", 999, false, true},
		{"the change itself, with a title", a, true, false},
		{"a change directly above it", s[1].Number, false, false},
		{"a change above it through another", s[2].Number, false, false},
		{"a change into another target branch", release, false, true},
		{"a landed change", landed, false, true},
		{"an abandoned change", abandoned, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := map[string]any{"depends_on": c.dependsOn}
			if c.title {
				body["title"] = "renamed"
			}
			before, _ := w.change(t, a)
			at := w.updatedAt(t, a)
			if status, got := w.patch(t, a, w.ownerKey, body); status != http.StatusBadRequest || errorType(got) != "invalid_dependency" {
				t.Errorf("PATCH %v = %d %v, want 400 invalid_dependency", body, status, got)
			}
			if after, _ := w.change(t, a); !reflect.DeepEqual(after, before) || w.updatedAt(t, a) != at {
				t.Errorf("a refused PATCH left %v, had %v", after, before)
			}
			if c.atOpen {
				w.refused(t, "demo", w.ownerKey, map[string]any{"source": map[string]any{"branch": "new"}, "depends_on": c.dependsOn}, http.StatusBadRequest, "invalid_dependency")
			}
		})
	}
}

func TestSetDependsOn_ClearingTheLinkMeasuresTheChangeFromItsTarget(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	s := w.stacked(t, "a", "b")
	status, got := w.patch(t, s[1].Number, w.ownerKey, `{"depends_on":null}`)
	mergeBase := gitrepotest.Git(t, w.demo, "merge-base", s[1].HeadSHA, "main")
	stats, _ := got["stats"].(map[string]any)
	if status != http.StatusOK || got["depends_on"] != nil || got["base_sha"] != mergeBase {
		t.Errorf("PATCH depends_on null = %d with depends_on %v, base_sha %v; want 200, none, %s", status, got["depends_on"], got["base_sha"], mergeBase)
	}
	if stats["commits"] != 2.0 || !reflect.DeepEqual(got["paths"], []any{"a.txt", "b.txt"}) {
		t.Errorf("stats, paths = %v, %v; want the 2 commits of a and b, [a.txt b.txt]", stats, got["paths"])
	}
	if deps := w.read(t, s[0].Number).Dependants; len(deps) != 0 {
		t.Errorf("the dependants of %d = %v, want none", s[0].Number, deps)
	}
}

func TestLinkVersion_KeepsAnExplicitLinkForTheFirstVersionOnly(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.set(t, "demo", "settings", `{"changes.auto_restack":false}`)
	a, x := w.stacked(t, "a")[0], w.stacked(t, "x")[0]
	head := commit(t, w.demo, "b", x.HeadSHA, "B\n", map[string]string{"b.txt": "b\n"})
	b := w.openBranch(t, "b", func(in *change.OpenInput) { in.DependsOn = new(a.Number) }).Number
	if got := w.read(t, b).DependsOn; got == nil || *got != a.Number {
		t.Fatalf("depends_on after the open = %v, want %d", got, a.Number)
	}
	head = commit(t, w.demo, "b", head, "B2\n", map[string]string{"b.txt": "b2\n"})
	w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "b", Head: head, Kind: change.KindPush, Caller: w.ownerCaller()})
	if got := w.read(t, b).DependsOn; got == nil || *got != x.Number {
		t.Errorf("depends_on after the next version = %v, want %d", got, x.Number)
	}
}

func TestLinkVersion_FlagsTheChangesPastTheStackDepthLimit(t *testing.T) {
	t.Parallel()
	limited := newWorldWith(t, hubtest.Config{TOML: zeroDebounce + "max_stack_depth = 3\n"})
	unconfigured := newWorld(t)
	chain := func(p string, n int) []string {
		var names []string
		for i := range n {
			names = append(names, fmt.Sprintf("%s%02d", p, i))
		}
		return names
	}
	cases := []struct {
		name string
		w    *world
		// open opens the changes, each branch named with the prefix p, and
		// returns them bottom first.
		open func(t *testing.T, w *world, p string) []change.Change
		// unflagged is how many changes at the bottom are within the limit.
		unflagged int
	}{
		{"a limit of 3, five opened bottom first", limited, func(t *testing.T, w *world, p string) []change.Change {
			return w.stacked(t, chain(p, 5)...)
		}, 3},
		{"no limit configured, twenty-one opened bottom first", unconfigured, func(t *testing.T, w *world, p string) []change.Change {
			return w.stacked(t, chain(p, 21)...)
		}, 20},
		{"a limit of 3, a fourth attached by PATCH", limited, func(t *testing.T, w *world, p string) []change.Change {
			s := w.stacked(t, chain(p, 3)...)
			w.branch(t, p+"x", "X\n\nWhy.\n")
			x := w.openBranch(t, p+"x", nil)
			if status, got := w.patch(t, x.Number, w.ownerKey, map[string]any{"depends_on": s[2].Number}); status != http.StatusOK {
				t.Fatalf("PATCH depends_on %d = %d %v, want 200", s[2].Number, status, got)
			}
			return append(s, x)
		}, 3},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := c.open(t, c.w, fmt.Sprintf("d%d", i))
			for j, opened := range s {
				ch := c.w.read(t, opened.Number)
				var want *string
				if j >= c.unflagged {
					want = new("stack_too_deep")
				}
				if ch.NeedsRestack != (want != nil) || deref(ch.RestackReason) != deref(want) {
					t.Errorf("change %d at depth %d: needs_restack, restack_reason = %v, %v; want %v, %v", ch.Number, j+1, ch.NeedsRestack, deref(ch.RestackReason), want != nil, deref(want))
				}
				var below *int64
				if j > 0 {
					below = &s[j-1].Number
				}
				if (ch.DependsOn == nil) != (below == nil) || (below != nil && *ch.DependsOn != *below) {
					t.Errorf("change %d at depth %d: depends_on = %v, want %v", ch.Number, j+1, ch.DependsOn, below)
				}
				if vs := c.w.versions(t, "demo", ch.Number); len(vs) == 0 || vs[0]["number"] != 1.0 {
					t.Errorf("change %d at depth %d: versions = %v, want version 1 recorded", ch.Number, j+1, vs)
				}
			}
		})
	}
}

func TestLinkVersion_ClearsANeedsRestackOnceTheChangeSitsOnWhatItMust(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.hold(t)
	w.holdRestacks(t)
	// conflictOnA amends the change below to clash with the change b,
	// whose restack conflicts, and returns the new head below.
	conflictOnA := func(t *testing.T, p string, _ change.Change) string {
		head := w.amend(t, p+"a", map[string]string{p + "f.txt": "a2\n"})
		w.runPending(t)
		return head
	}
	// conflictOnLanded lands the change a below as a commit that clashes
	// with b, whose restack conflicts, and returns the landed commit.
	conflictOnLanded := func(t *testing.T, p string, a change.Change) string {
		landed := w.land(t, a.Number, map[string]string{p + "f.txt": "a2\n"})
		if err := w.changes.OnLanded(context.Background(), "demo", a.Number, landed); err != nil {
			t.Fatalf("OnLanded = %v", err)
		}
		w.runPending(t)
		return landed
	}
	for i, c := range []struct {
		name string
		// flag sets the needs_restack of the change b, which sits on the
		// change a, for reason, and returns the commit the version b gets
		// is made on, from the head below, the landed commit or the base.
		flag    func(t *testing.T, p string, a change.Change) string
		on      func(below, base string) string
		reason  string
		cleared bool
		// linked is whether b still sits on a.
		linked bool
	}{
		{"in conflict, rebased onto the head below", conflictOnA, func(below, _ string) string { return below }, change.RestackConflict, true, true},
		{"with automatic restacks off, rebased onto the head below", func(t *testing.T, p string, a change.Change) string {
			w.write(t, func(ctx context.Context, tx *sql.Tx) error {
				return w.changes.SetNeedsRestack(ctx, tx, "demo", a.Number+1, true, new(change.RestackAutoOff))
			})
			return a.HeadSHA
		}, func(below, _ string) string { return below }, change.RestackAutoOff, true, true},
		{"in conflict on the landed commit, rebased onto it", conflictOnLanded, func(below, _ string) string { return below }, change.RestackConflict, true, false},
		{"in conflict on the landed commit, amended on its old base", conflictOnLanded, func(_, base string) string { return base }, change.RestackConflict, false, false},
		{"in conflict, rebased onto its target", conflictOnA, func(string, string) string { return w.base }, change.RestackConflict, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := fmt.Sprintf("x%d", i)
			s := w.stacked(t, p+"a")
			a := s[0]
			commit(t, w.demo, p+"b", a.HeadSHA, "B\n", map[string]string{p + "f.txt": "b\n"})
			b := w.openBranch(t, p+"b", nil)
			if b.Number != a.Number+1 {
				t.Fatalf("b is demo#%d, want demo#%d", b.Number, a.Number+1)
			}
			below := c.flag(t, p, a)
			if got := w.read(t, b.Number); !got.NeedsRestack || deref(got.RestackReason) != c.reason {
				t.Fatalf("before the version, needs_restack %v for %q; want true for %q", got.NeedsRestack, deref(got.RestackReason), c.reason)
			}
			head := commit(t, w.demo, p+"b", c.on(below, a.HeadSHA), "B again\n", map[string]string{p + "f.txt": "b2\n"})
			w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: p + "b", Head: head, Kind: change.KindPush, Caller: w.ownerCaller()})

			got := w.read(t, b.Number)
			want := c.reason
			if c.cleared {
				want = ""
			}
			if got.NeedsRestack == c.cleared || deref(got.RestackReason) != want {
				t.Errorf("needs_restack %v for %q, want %v for %q", got.NeedsRestack, deref(got.RestackReason), !c.cleared, want)
			}
			if linked := got.DependsOn != nil && *got.DependsOn == a.Number; linked != c.linked || (!c.linked && got.DependsOn != nil) {
				t.Errorf("depends_on %v, want on demo#%d: %v", got.DependsOn, a.Number, c.linked)
			}
			if deps := w.read(t, a.Number).Dependants; !c.linked && len(deps) != 0 {
				t.Errorf("the dependants of demo#%d are %v, want none", a.Number, deps)
			}
		})
	}
}
