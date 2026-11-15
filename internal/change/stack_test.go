package change_test

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
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
