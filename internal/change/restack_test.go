package change_test

import (
	"context"
	"database/sql"
	"fmt"
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
