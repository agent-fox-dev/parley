package change_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
)

// changeEvents are the events of type typ of the change number of demo
// written after seq.
func (w *world) changeEvents(t *testing.T, seq int64, typ string, number int64) []event.Record {
	t.Helper()
	var out []event.Record
	for _, e := range w.eventsAfter(t, seq) {
		if e.Type == typ && e.ResourceType == "change" && e.ResourceID == strconv.FormatInt(number, 10) &&
			e.Workspace != nil && *e.Workspace == "demo" {
			out = append(out, e)
		}
	}
	return out
}

// announced is the head the change number of demo was last announced
// landable at, "" for none.
func (w *world) announced(t *testing.T, number int64) string {
	t.Helper()
	var head sql.NullString
	if err := w.Store.Main().QueryRow(`SELECT ready_announced_head FROM changes WHERE workspace_slug = 'demo' AND number = ?`,
		number).Scan(&head); err != nil {
		t.Fatal(err)
	}
	return head.String
}

// fixedLanding is a landing evaluator that answers its status for every
// change without a cause, and blocks one with a cause for it.
type fixedLanding string

func (s fixedLanding) EvaluateLanding(_ context.Context, _ *sql.Tx, _ change.Change, _ change.Version, cause *string) (change.Landing, error) {
	if cause != nil {
		return change.Landing{Status: change.LandingBlocked, Reason: cause}, nil
	}
	return change.Landing{Status: string(s)}, nil
}

// wantSystemEvents fails the test unless every event of events was written
// by the hub.
func wantSystemEvents(t *testing.T, events []event.Record) {
	t.Helper()
	for _, e := range events {
		if e.ActorType != change.ActorSystem || e.ActorID != nil || e.ResourceType != "change" {
			t.Errorf("%s by %s %v on %s, want the system on a change", e.Type, e.ActorType, e.ActorID, e.ResourceType)
		}
	}
}

func TestMergeability_AnnouncesAChangeThatStartsToConflict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// setup opens demo#1, which changes line one of a.txt, and makes it
		// conflict with main.
		setup func(t *testing.T, w *world)
	}{
		{"clean, then main changes the line", func(t *testing.T, w *world) {
			commit(t, w.demo, "b", w.base, "B", map[string]string{"a.txt": lineOneX})
			w.openBranch(t, "b", nil)
			if ch := w.settled(t, 1); ch.Mergeable != change.MergeableClean {
				t.Fatalf("mergeable = %s before main moved, want clean", ch.Mergeable)
			}
			w.push(t, "main", "main", map[string]string{"a.txt": lineOneM})
		}},
		{"conflicting at its first check", func(t *testing.T, w *world) {
			commit(t, w.demo, "main", "main", "M", map[string]string{"a.txt": lineOneM})
			commit(t, w.demo, "b", w.base, "B", map[string]string{"a.txt": lineOneX})
			w.openBranch(t, "b", nil)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			seq := w.seq(t)
			tc.setup(t, w)
			ch := w.settled(t, 1)
			main := gitrepotest.Refs(t, w.demo, "refs/heads/")["main"]
			if ch.Mergeable != change.MergeableConflict {
				t.Fatalf("mergeable = %s, want conflict", ch.Mergeable)
			}
			report := conflictOf(t, ch)
			files, _ := report["files"].([]any)
			if len(files) != 1 || files[0].(map[string]any)["path"] != "a.txt" || report["target_sha"] != main || report["head_sha"] != ch.HeadSHA {
				t.Errorf("conflict = %s, want a.txt alone, the head %s rebased onto main at %s", ch.Conflict, ch.HeadSHA, main)
			}
			events := w.changeEvents(t, seq, change.EventConflict, 1)
			if len(events) != 1 {
				t.Fatalf("%d conflict events, want 1", len(events))
			}
			wantSystemEvents(t, events)
			want := map[string]any{"number": 1.0, "ref": "demo#1", "version": float64(ch.Version), "head_sha": ch.HeadSHA,
				"files": []any{"a.txt"}, "needs_restack": false}
			if got := metadata(t, events[0]); !reflect.DeepEqual(got, want) {
				t.Errorf("metadata = %v, want %v", got, want)
			}
		})
	}
}

func TestMergeability_AnnouncesAConflictAgainOnlyAfterTheChangeWasClean(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	commit(t, w.demo, "b", w.base, "B", map[string]string{"a.txt": lineOneX})
	w.openBranch(t, "b", nil)
	w.settled(t, 1)
	seq := w.seq(t)
	conflicts := func() int { return len(w.changeEvents(t, seq, change.EventConflict, 1)) }
	main := func() string { return gitrepotest.Refs(t, w.demo, "refs/heads/")["main"] }

	w.push(t, "main", "main", map[string]string{"a.txt": lineOneM})
	w.settled(t, 1)
	w.push(t, "b", "b", map[string]string{"a.txt": lineOneZ})
	w.settled(t, 1)
	w.push(t, "main", "main", map[string]string{"m.txt": "m\n"})
	ch := w.settled(t, 1)
	report := conflictOf(t, ch)
	if ch.Mergeable != change.MergeableConflict || ch.Version != 2 || report["head_sha"] != ch.HeadSHA || report["target_sha"] != main() {
		t.Errorf("version %d is %s with %s, want version 2 conflicting with a report of the latest check", ch.Version, ch.Mergeable, ch.Conflict)
	}
	if n := conflicts(); n != 1 {
		t.Errorf("%d conflict events while the change kept conflicting, want 1", n)
	}

	rebased := commit(t, w.demo, "b", main(), "B rebased", map[string]string{"a.txt": lineOneX})
	w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "b", Head: rebased, Kind: change.KindPush, Caller: w.ownerCaller()})
	if _, out, _ := w.get(t, "/workspaces/demo/changes/1", w.ownerKey); out["mergeable"] != "clean" || out["conflict"] != nil {
		t.Errorf("after a version that applies GET = %v, want clean and no conflict", out)
	}
	w.push(t, "main", "main", map[string]string{"a.txt": lineOneY})
	if ch := w.settled(t, 1); ch.Mergeable != change.MergeableConflict || conflicts() != 2 {
		t.Errorf("after main conflicts again the change is %s with %d conflict events, want conflict and 2", ch.Mergeable, conflicts())
	}
}

func TestMergeability_AnnouncesALandableChangeOncePerHead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		run  func(t *testing.T, w *world)
		want int
	}{
		{"opened open", func(t *testing.T, w *world) {
			w.branch(t, "b", "B")
			w.openBranch(t, "b", nil)
			w.settled(t, 1)
		}, 1},
		{"a clean draft moved to open", func(t *testing.T, w *world) {
			w.branch(t, "b", "B")
			w.openBranch(t, "b", func(in *change.OpenInput) { in.Draft = true })
			w.settled(t, 1)
			seq := w.seq(t)
			w.patch(t, 1, w.ownerKey, map[string]any{"state": change.StateOpen})
			var types []string
			for _, e := range w.eventsAfter(t, seq) {
				types = append(types, e.Type)
			}
			if want := []string{change.EventOpen, change.EventReady}; !reflect.DeepEqual(types, want) {
				t.Errorf("marking the draft ready wrote %v, want %v", types, want)
			}
		}, 1},
		{"conflicting, then clean at a new version", func(t *testing.T, w *world) {
			commit(t, w.demo, "b", w.base, "B", map[string]string{"a.txt": lineOneX})
			w.openBranch(t, "b", nil)
			w.settled(t, 1)
			w.push(t, "main", "main", map[string]string{"a.txt": lineOneM})
			w.settled(t, 1)
			rebased := commit(t, w.demo, "b", "main", "B rebased", map[string]string{"a.txt": lineOneX})
			w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "b", Head: rebased, Kind: change.KindPush, Caller: w.ownerCaller()})
			w.settled(t, 1)
		}, 2},
		{"clean, then clean again at a new version", func(t *testing.T, w *world) {
			w.branch(t, "b", "B")
			w.openBranch(t, "b", nil)
			w.settled(t, 1)
			w.push(t, "b", "b", map[string]string{"b2.txt": "2\n"})
			w.settled(t, 1)
		}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			seq := w.seq(t)
			tc.run(t, w)
			ch := w.read(t, 1)
			events := w.changeEvents(t, seq, change.EventReady, 1)
			if len(events) != tc.want {
				t.Fatalf("%d ready events, want %d", len(events), tc.want)
			}
			wantSystemEvents(t, events)
			want := map[string]any{"number": 1.0, "ref": "demo#1", "version": float64(ch.Version), "head_sha": ch.HeadSHA}
			if got := metadata(t, events[len(events)-1]); !reflect.DeepEqual(got, want) {
				t.Errorf("metadata of the last ready event = %v, want %v", got, want)
			}
		})
	}
}

func TestMergeability_AnnouncesNoLandableChangeAgainForTheSameHead(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	seq := w.seq(t)
	w.branch(t, "b", "B")
	w.openBranch(t, "b", nil)
	w.settled(t, 1)
	for i := range 5 {
		w.push(t, "main", "main", map[string]string{"m.txt": strconv.Itoa(i)})
		if ch := w.settled(t, 1); ch.Mergeable != change.MergeableClean {
			t.Fatalf("after move %d mergeable = %s, want clean", i, ch.Mergeable)
		}
	}
	for range 2 {
		w.write(t, func(ctx context.Context, tx *sql.Tx) error { return w.changes.AnnounceLandable(ctx, tx, "demo", 1) })
	}
	if n := len(w.changeEvents(t, seq, change.EventReady, 1)); n != 1 {
		t.Errorf("%d ready events, want 1", n)
	}
}

func TestAnnounceLandable_AnnouncesAgainOnlyAfterTheChangeWasNotLandable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// run opens demo#1, and moves it, with seq the log's before.
		run  func(t *testing.T, w *world, seq int64)
		want int
	}{
		{"announced, moved to draft and back to open", func(t *testing.T, w *world, _ int64) {
			w.branch(t, "b", "B")
			w.openBranch(t, "b", nil)
			w.settled(t, 1)
			w.patch(t, 1, w.ownerKey, map[string]any{"state": change.StateDraft})
			w.patch(t, 1, w.ownerKey, map[string]any{"state": change.StateOpen})
		}, 2},
		{"announced, retargeted to release where it is clean", func(t *testing.T, w *world, _ int64) {
			w.branch(t, "b", "B")
			w.openBranch(t, "b", nil)
			w.settled(t, 1)
			if status, out := w.patch(t, 1, w.ownerKey, map[string]any{"target_branch": "release"}); status != http.StatusOK {
				t.Fatalf("retarget = %d %v", status, out)
			}
			w.settled(t, 1)
		}, 2},
		{"a draft found clean", func(t *testing.T, w *world, _ int64) {
			w.branch(t, "b", "B")
			w.openBranch(t, "b", func(in *change.OpenInput) { in.Draft = true })
			w.settled(t, 1)
		}, 0},
		{"open and clean while the policy keeps it pending, then ready", func(t *testing.T, w *world, seq int64) {
			w.changes.RegisterLandingEvaluator(fixedLanding(change.LandingPending))
			w.branch(t, "b", "B")
			w.openBranch(t, "b", nil)
			if ch := w.settled(t, 1); ch.Mergeable != change.MergeableClean || len(w.changeEvents(t, seq, change.EventReady, 1)) != 0 {
				t.Fatalf("a pending change found %s was announced", ch.Mergeable)
			}
			w.write(t, func(ctx context.Context, tx *sql.Tx) error {
				if err := w.changes.SetLanding(ctx, tx, "demo", 1, change.Landing{Status: change.LandingReady}); err != nil {
					return err
				}
				return w.changes.AnnounceLandable(ctx, tx, "demo", 1)
			})
		}, 1},
		{"conflicting", func(t *testing.T, w *world, _ int64) {
			commit(t, w.demo, "main", "main", "M", map[string]string{"a.txt": lineOneM})
			commit(t, w.demo, "b", w.base, "B", map[string]string{"a.txt": lineOneX})
			w.openBranch(t, "b", nil)
			w.settled(t, 1)
		}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			seq := w.seq(t)
			tc.run(t, w, seq)
			if n := len(w.changeEvents(t, seq, change.EventReady, 1)); n != tc.want {
				t.Errorf("%d ready events, want %d", n, tc.want)
			}
		})
	}
}

// failingEvents is an event writer that fails the next event of the type
// fail once, and writes every other one.
type failingEvents struct {
	change.EventWriter

	mu   sync.Mutex
	fail string
}

func (f *failingEvents) Emit(ctx context.Context, tx *sql.Tx, in event.Input) (event.Record, error) {
	f.mu.Lock()
	failed := in.Type == f.fail
	if failed {
		f.fail = ""
	}
	f.mu.Unlock()
	if failed {
		return event.Record{}, errors.New("the event log failed")
	}
	return f.EventWriter.Emit(ctx, tx, in)
}

func (f *failingEvents) failNext(typ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = typ
}

func TestMergeability_StoresNothingWhenItsEventIsNotWritten(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, event string
		// setup opens demo#1 and arms the failure, and returns after the check
		// whose event fails was triggered.
		setup func(t *testing.T, w *world, f *failingEvents)
		want  string
	}{
		{"the conflict event", change.EventConflict, func(t *testing.T, w *world, f *failingEvents) {
			commit(t, w.demo, "b", w.base, "B", map[string]string{"a.txt": lineOneX})
			w.openBranch(t, "b", nil)
			w.settled(t, 1)
			f.failNext(change.EventConflict)
			w.push(t, "main", "main", map[string]string{"a.txt": lineOneM})
		}, change.MergeableConflict},
		{"the ready event", change.EventReady, func(t *testing.T, w *world, f *failingEvents) {
			f.failNext(change.EventReady)
			w.branch(t, "b", "B")
			w.openBranch(t, "b", nil)
		}, change.MergeableClean},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &failingEvents{}
			w := newWorldWith(t, hubtest.Config{Compose: func(c *hub.Composition) {
				c.AdjustChange(func(d *change.Deps) { f.EventWriter, d.Events = d.Events, f })
			}})
			seq := w.seq(t)
			tc.setup(t, w, f)
			w.idle(t, 1)
			if ch := w.read(t, 1); ch.Mergeable != change.MergeableUnknown || len(ch.Conflict) > 0 {
				t.Errorf("mergeable, conflict = %s, %s after the event failed; want unknown and none", ch.Mergeable, ch.Conflict)
			}
			if n := len(w.changeEvents(t, seq, tc.event, 1)); n != 0 {
				t.Errorf("%d %s events after the event failed, want 0", n, tc.event)
			}
			if n := failures(t, w, 1); n != 1 {
				t.Errorf("%d log entries of the failure, want 1:\n%s", n, w.Log.String())
			}
			w.trigger(t, 1)
			if ch := w.settled(t, 1); ch.Mergeable != tc.want || len(w.changeEvents(t, seq, tc.event, 1)) != 1 {
				t.Errorf("at the next trigger the change is %s with %d %s events, want %s and 1",
					ch.Mergeable, len(w.changeEvents(t, seq, tc.event, 1)), tc.event, tc.want)
			}
		})
	}
}

func TestAnnounceLandable_DecidesInTheCallersTransaction(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.changes.RegisterLandingEvaluator(fixedLanding(change.LandingPending))
	w.branch(t, "b", "B")
	w.openBranch(t, "b", nil)
	head := w.settled(t, 1).HeadSHA
	seq := w.seq(t)
	ready := func() int { return len(w.changeEvents(t, seq, change.EventReady, 1)) }
	// step sets the landing of demo#1 to status, unless "", and runs the
	// announcement step in one transaction, rolled back when undo is set.
	step := func(status string, undo bool) {
		t.Helper()
		ctx := context.Background()
		err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
			if status != "" {
				if err := w.changes.SetLanding(ctx, tx, "demo", 1, change.Landing{Status: status}); err != nil {
					return err
				}
			}
			if err := w.changes.AnnounceLandable(ctx, tx, "demo", 1); err != nil {
				return err
			}
			if undo {
				return errRollback
			}
			return nil
		})
		if err != nil && (!undo || !errors.Is(err, errRollback)) {
			t.Fatal(err)
		}
	}

	step(change.LandingReady, true)
	if n, got := ready(), w.announced(t, 1); n != 0 || got != "" {
		t.Errorf("a rolled-back announcement left %d events and the announced head %q", n, got)
	}
	step(change.LandingReady, false)
	events := w.changeEvents(t, seq, change.EventReady, 1)
	if len(events) != 1 || metadata(t, events[0])["head_sha"] != head || w.announced(t, 1) != head {
		t.Fatalf("after a committed announcement: %d events, the announced head %q; want 1 for %s", len(events), w.announced(t, 1), head)
	}
	step("", false)
	if n := ready(); n != 1 {
		t.Errorf("announcing again wrote %d events in all, want 1", n)
	}
	step(change.LandingPending, false)
	if got := w.announced(t, 1); got != "" {
		t.Errorf("a pending change keeps the announced head %q", got)
	}
	step(change.LandingReady, false)
	if n := ready(); n != 2 {
		t.Errorf("%d ready events after the change was pending and ready again, want 2", n)
	}
	w.write(t, func(ctx context.Context, tx *sql.Tx) error {
		if err := w.changes.SetMergeable(ctx, tx, "demo", 1, change.MergeableUnknown, nil); err != nil {
			return err
		}
		if err := w.changes.SetLanding(ctx, tx, "demo", 1, change.Landing{Status: change.LandingPending}); err != nil {
			return err
		}
		return w.changes.AnnounceLandable(ctx, tx, "demo", 1)
	})
	if n, got := ready(), w.announced(t, 1); n != 2 || got != head {
		t.Errorf("while mergeable is unknown: %d events and the announced head %q, want 2 and %s", n, got, head)
	}
}
