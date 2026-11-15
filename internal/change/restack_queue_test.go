package change_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestOnLanded_PendsARestackOfEachDirectDependantOntoTheLandedCommit(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.holdRestacks(t)
	for _, c := range []struct {
		name  string
		names []string
	}{
		{"a change with dependants", []string{"da", "db", "dc"}},
		{"a change without dependants", []string{"na"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := w.stacked(t, c.names...)
			before := w.pending(t)
			landed := w.land(t, s[0].Number, map[string]string{c.names[0] + ".txt": c.names[0] + "\n"})
			if err := w.changes.OnLanded(context.Background(), "demo", s[0].Number, landed); err != nil {
				t.Fatalf("OnLanded = %v", err)
			}
			if len(s) == 1 {
				if got := w.pending(t); !slices.Equal(got, before) {
					t.Errorf("pending restacks of %v, want %v", got, before)
				}
				return
			}
			b, above := w.read(t, s[1].Number), w.read(t, s[2].Number)
			if b.DependsOn != nil || above.DependsOn == nil || *above.DependsOn != b.Number {
				t.Errorf("demo#%d depends on %v and demo#%d on %v; want none and %d", b.Number, b.DependsOn, above.Number, above.DependsOn, b.Number)
			}
			var trigger, onto string
			if err := w.Store.Main().QueryRow(`SELECT trigger, onto FROM change_restacks WHERE workspace_slug = 'demo' AND number = ?`, b.Number).Scan(&trigger, &onto); err != nil {
				t.Fatal(err)
			}
			if got, want := w.pending(t), append(before, b.Number); !slices.Equal(got, want) || trigger != change.TriggerLand || onto != landed {
				t.Errorf("pending restacks of %v, demo#%d's for %q onto %s; want %v, for land onto %s", got, b.Number, trigger, onto, want, landed)
			}
		})
	}
}

func TestRunRestacks_RunsTheRestacksPendingWhenTheHubStarts(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	// The replay of the change above fa fails in the hub's run before the
	// stop; that of the change above a is never run before it.
	f := w.stacked(t, "fa", "fb")
	change.SetRestackSeams(w.changes, change.RestackSeams{Rebase: failFor(f[1].HeadSHA, nil)})
	fa := w.amend(t, "fa", map[string]string{"fa2.txt": "fa2\n"})
	w.holdRestacks(t)
	s := w.stacked(t, "a", "b")
	a := w.amend(t, "a", map[string]string{"a2.txt": "a2\n"})
	if got := w.pending(t); !slices.Equal(got, []int64{f[1].Number, s[1].Number}) {
		t.Fatalf("pending restacks of %v before the stop, want %v", got, []int64{f[1].Number, s[1].Number})
	}

	w.Restart(t)
	for deadline := time.Now().Add(20 * time.Second); len(w.pending(t)) > 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("pending restacks of %v after the start, want none", w.pending(t))
		}
	}
	w.quiet(t)
	for _, c := range []struct {
		name  string
		above int64
		onto  string
	}{
		{"failed before the stop", f[1].Number, fa},
		{"never run before the stop", s[1].Number, a},
	} {
		vs := w.versions(t, "demo", c.above)
		if v := vs[len(vs)-1]; len(vs) != 2 || v["kind"] != change.KindRestack || v["base_sha"] != c.onto {
			t.Errorf("%s: demo#%d's last version of %d is %v %v; want version 2 of kind restack on %s", c.name, c.above, len(vs), v["kind"], v["base_sha"], c.onto)
		}
	}
}

func TestRunRestacks_DropsTheRestackOfAChangeNoLongerDraftOrOpen(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.holdRestacks(t)
	for i, c := range []struct {
		name  string
		close func(t *testing.T, ch change.Change)
	}{
		{"abandoned", func(t *testing.T, ch change.Change) {
			w.write(t, func(ctx context.Context, tx *sql.Tx) error {
				return w.changes.SetAbandoned(ctx, tx, "demo", ch.Number, "")
			})
		}},
		{"landed", func(t *testing.T, ch change.Change) {
			w.land(t, ch.Number, map[string]string{ch.Source.Branch + ".txt": ch.Source.Branch + "\n"})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := fmt.Sprintf("x%d", i)
			b := w.stacked(t, p+"a", p+"b")[1]
			w.amend(t, p+"a", map[string]string{p + "a2.txt": "a2\n"})
			c.close(t, b)
			before, seq := w.read(t, b.Number), w.seq(t)
			w.runPending(t)
			after := w.read(t, b.Number)
			if events := w.changeEventsOf(t, seq, b.Number); after.Version != before.Version || w.head(t, p+"b") != b.HeadSHA ||
				len(w.pending(t)) != 0 || len(events) != 0 {
				t.Errorf("demo#%d at version %d, branch at %s, pending restacks of %v, %d events; want version %d, %s, none, none",
					b.Number, after.Version, w.head(t, p+"b"), w.pending(t), len(events), before.Version, b.HeadSHA)
			}
		})
	}
}

func TestRunRestacks_KeepsARestackThatFailsAndGoesOn(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.holdRestacks(t)
	a := commit(t, w.demo, "a", w.base, "A\n", map[string]string{"a.txt": "a\n"})
	w.openBranch(t, "a", nil)
	commit(t, w.demo, "b", a, "B\n", map[string]string{"b.txt": "b\n"})
	b := w.openBranch(t, "b", nil)
	commit(t, w.demo, "c", a, "C\n", map[string]string{"c.txt": "c\n"})
	cc := w.openBranch(t, "c", nil)
	// The error names a credential, which the log must not show.
	var fail atomic.Bool
	fail.Store(true)
	change.SetRestackSeams(w.changes, change.RestackSeams{
		Rebase: failFor(b.HeadSHA, func() bool { return fail.Load() }, w.ownerKey),
	})
	seq := w.seq(t)
	w.amend(t, "a", map[string]string{"a2.txt": "a2\n"})
	w.runPending(t)

	ch := w.read(t, b.Number)
	var touched []string
	for _, e := range w.changeEventsOf(t, seq, b.Number) {
		if e.Type == change.EventVersion || e.Type == change.EventRestack || e.Type == change.EventConflict {
			touched = append(touched, e.Type)
		}
	}
	if ch.Version != 1 || w.head(t, "b") != b.HeadSHA || ch.NeedsRestack || len(touched) != 0 || !slices.Equal(w.pending(t), []int64{b.Number}) {
		t.Errorf("demo#%d at version %d, branch at %s, needs_restack %v, events %v, pending restacks of %v; want version 1 at %s, false, none, one of demo#%d",
			b.Number, ch.Version, w.head(t, "b"), ch.NeedsRestack, touched, w.pending(t), b.HeadSHA, b.Number)
	}
	if vs := w.versions(t, "demo", cc.Number); vs[len(vs)-1]["kind"] != change.KindRestack {
		t.Errorf("demo#%d's last version is %v, want a restack", cc.Number, vs[len(vs)-1]["kind"])
	}

	// The hub goes on: a restack recorded later for another change runs, and
	// so does a newer one for the change whose restack failed.
	e := w.stacked(t, "d", "e")[1]
	w.amend(t, "d", map[string]string{"d2.txt": "d2\n"})
	w.runPending(t)
	fail.Store(false)
	head := w.amend(t, "a", map[string]string{"a3.txt": "a3\n"})
	w.runPending(t)
	for _, n := range []int64{e.Number, b.Number} {
		if vs := w.versions(t, "demo", n); len(vs) != 2 || vs[1]["kind"] != change.KindRestack {
			t.Errorf("demo#%d has %d versions, the last %v; want 2, the last a restack", n, len(vs), vs[len(vs)-1]["kind"])
		}
	}
	if vs := w.versions(t, "demo", b.Number); vs[len(vs)-1]["base_sha"] != head || len(w.pending(t)) != 0 {
		t.Errorf("demo#%d's last version is on %v with pending restacks of %v; want on %s, none", b.Number, vs[len(vs)-1]["base_sha"], w.pending(t), head)
	}
	if status, _, _ := w.get(t, "/workspaces/demo/changes/"+strconv.FormatInt(b.Number, 10), w.ownerKey); status != http.StatusOK {
		t.Errorf("GET demo#%d = %d after the failure, want 200", b.Number, status)
	}
	var failures []map[string]any
	for line := range strings.Lines(w.Log.String()) {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) == nil && e["msg"] == "the restack failed" {
			failures = append(failures, e)
		}
	}
	if len(failures) != 1 || failures[0]["level"] != "ERROR" || failures[0]["workspace"] != "demo" || failures[0]["number"] != float64(b.Number) ||
		!strings.Contains(fmt.Sprint(failures[0]["error"]), "exit status 128") || strings.Contains(w.Log.String(), w.ownerKey) {
		t.Errorf("log entries of the failure %v; want one error of demo#%d with the error's text, without the credential", failures, b.Number)
	}
}
