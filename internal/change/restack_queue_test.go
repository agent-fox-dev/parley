package change_test

import (
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/change"
)

func TestRecordVersion_PendsARestackOfEachDirectDependant(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.holdRestacks(t)
	// The store refuses the records in the last row, so it comes last.
	for _, c := range []struct {
		name   string
		refuse bool
	}{
		{"the store keeps the records", false},
		{"the store refuses them", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := map[bool]string{false: "k", true: "r"}[c.refuse]
			s := w.stacked(t, p+"a", p+"b")
			commit(t, w.demo, p+"c", s[0].HeadSHA, "C\n", map[string]string{p + "c.txt": "c\n"})
			a, b, cc := s[0], s[1], w.openBranch(t, p+"c", nil)
			heads, before := []string{w.head(t, p+"b"), w.head(t, p+"c")}, w.pending(t)
			if c.refuse {
				if _, err := w.Store.Main().Exec(`CREATE TRIGGER refuse_restacks BEFORE INSERT ON change_restacks
BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
					t.Fatal(err)
				}
			}
			head := commit(t, w.demo, p+"a", w.base, "A2\n", map[string]string{p + "a.txt": "a2\n"})
			v, err := w.record(t, change.VersionInput{Workspace: "demo", Branch: p + "a", Head: head, Kind: change.KindPush, Caller: w.ownerCaller()})
			if c.refuse {
				if err == nil || v != nil || w.read(t, a.Number).Version != 1 || !slices.Equal(w.pending(t), before) {
					t.Errorf("RecordVersion = %v, %v, with version %d and pending %v; want an error, version 1 and pending %v",
						v, err, w.read(t, a.Number).Version, w.pending(t), before)
				}
				return
			}
			if err != nil || v == nil {
				t.Fatalf("RecordVersion = %v, %v; want a version", v, err)
			}
			if got, want := w.pending(t), append(before, b.Number, cc.Number); !slices.Equal(got, want) {
				t.Errorf("pending restacks of %v, want %v", got, want)
			}
			for i, n := range []int64{b.Number, cc.Number} {
				if ch := w.read(t, n); ch.Version != 1 || w.head(t, ch.Source.Branch) != heads[i] {
					t.Errorf("demo#%d is at version %d with its branch at %s, want version 1 at %s", n, ch.Version, w.head(t, ch.Source.Branch), heads[i])
				}
			}
		})
	}
}

func TestPendRestack_KeepsOneRestackPerChangeOntoTheNewestHeadBelow(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.holdRestacks(t)
	s := w.stacked(t, "a", "b")
	w.amend(t, "a", map[string]string{"a2.txt": "a2\n"})
	head := w.amend(t, "a", map[string]string{"a3.txt": "a3\n"})
	if got := w.pending(t); !slices.Equal(got, []int64{s[1].Number}) {
		t.Fatalf("pending restacks of %v, want one of %d", got, s[1].Number)
	}
	w.runPending(t)
	vs := w.versions(t, "demo", s[1].Number)
	if v := vs[len(vs)-1]; len(vs) != 2 || v["base_sha"] != head {
		t.Errorf("demo#%d has %d versions, the last on %v; want 2, on %s", s[1].Number, len(vs), v["base_sha"], head)
	}
}
