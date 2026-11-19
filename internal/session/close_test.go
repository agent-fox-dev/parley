package session_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/session"
)

// forceClosedType is the type of the event a forced close writes.
const forceClosedType = "parley.session.force_closed"

// closeActive runs CloseActive for the workspace slug with reason at the
// time at in a store transaction of its own, which commits unless the
// close fails, without the test's helpers, for a goroutine.
func (w *world) closeActive(slug, reason string, at time.Time) (int, error) {
	ctx := context.Background()
	var n int
	err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = w.sessions.CloseActive(ctx, tx, slug, reason, at)
		return err
	})
	return n, err
}

// forceClose closes the active sessions of the workspace slug with reason
// at the clock's time, fails the test unless the close commits, and
// returns the number closed.
func (w *world) forceClose(t *testing.T, slug, reason string) int {
	t.Helper()
	n, err := w.closeActive(slug, reason, w.clock.Now())
	if err != nil {
		t.Fatalf("CloseActive(%s, %q) = %d, %v; want no error", slug, reason, n, err)
	}
	return n
}

// forceClosed is the force_closed events of the hub's event log, oldest
// first; with ids given, only those of the sessions ids.
func (w *world) forceClosed(t *testing.T, ids ...string) []event.Record {
	t.Helper()
	recs, _, err := w.events.ReadAfter(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("read the event log: %v", err)
	}
	var out []event.Record
	for _, r := range recs {
		if r.Type == forceClosedType && (len(ids) == 0 || slices.Contains(ids, r.ResourceID)) {
			out = append(out, r)
		}
	}
	return out
}

// session reads the session id as the admin token, failing the test unless
// it is answered 200.
func (w *world) session(t *testing.T, id string) map[string]any {
	t.Helper()
	status, raw := w.get(t, w.Token, id)
	if status != http.StatusOK {
		t.Fatalf("GET /sessions/%s = %d %s, want 200", id, status, raw)
	}
	return decode(t, raw)
}

// wantActive fails the test unless every session ids is still active and
// not ended.
func (w *world) wantActive(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if s := w.session(t, id); s["status"] != "active" || s["ended_at"] != nil || s["error_message"] != nil {
			t.Errorf("session %s: status %v, ended_at %v, error_message %v; want active, null, null",
				id, s["status"], s["ended_at"], s["error_message"])
		}
	}
}

// wantTerminated fails the test unless every session ids is terminated at
// ended with reason as its error message and no duration.
func (w *world) wantTerminated(t *testing.T, ended, reason string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		s := w.session(t, id)
		if s["status"] != "terminated" || s["ended_at"] != ended || s["error_message"] != reason || s["duration_ms"] != nil {
			t.Errorf("session %s: status %v, ended_at %v, error_message %v, duration_ms %v; want terminated, %s, %q, null",
				id, s["status"], s["ended_at"], s["error_message"], s["duration_ms"], ended, reason)
		}
	}
}

// openOn opens a session on each workspace given, in order, as its owner
// (dave for other, alice for the rest), and returns their ids.
func (w *world) openOn(t *testing.T, workspaces ...string) []string {
	t.Helper()
	ids := make([]string, len(workspaces))
	for i, ws := range workspaces {
		owner := w.aliceKey
		if ws == other {
			owner = w.daveKey
		}
		ids[i] = w.open(t, owner, map[string]any{"workspace_slug": ws})["id"].(string)
	}
	return ids
}

func TestCloseActive_TerminatesTheActiveSessionsOfExactlyTheWorkspace(t *testing.T) {
	w := newWorld(t)
	// The time of the operation, given in another zone and with a fraction
	// of a second, is stored in UTC with whole seconds.
	at := time.Date(2026, 3, 4, 9, 10, 11, 600_000_000, time.FixedZone("+02:00", 2*3600))
	const ended = "2026-03-04T07:10:11Z"
	for _, reason := range []string{"workspace archived", "workspace deleted", "fork expired"} {
		t.Run(reason, func(t *testing.T) {
			ids := w.openOn(t, demo, demo, fork1, other)
			w.clock.Advance(time.Minute)

			n, err := w.closeActive(demo, reason, at)
			if n != 2 || err != nil {
				t.Fatalf("CloseActive(demo, %q) = %d, %v; want 2, nil", reason, n, err)
			}
			w.wantTerminated(t, ended, reason, ids[0], ids[1])
			w.wantActive(t, ids[2], ids[3])
		})
	}
}

func TestCloseActive_WritesOneEventPerClosedSession(t *testing.T) {
	w := newWorld(t)
	ids := w.openOn(t, demo, demo, fork1)

	n := w.forceClose(t, demo, "workspace deleted")

	recs := w.forceClosed(t)
	if n != 2 || len(recs) != n {
		t.Fatalf("closed %d sessions and wrote %d force_closed events %v, want 2 and 2", n, len(recs), recs)
	}
	var got []string
	for _, r := range recs {
		got = append(got, r.ResourceID)
		var meta map[string]any
		if err := json.Unmarshal(r.Metadata, &meta); err != nil {
			t.Fatalf("event metadata %s: %v", r.Metadata, err)
		}
		if r.Workspace == nil || *r.Workspace != demo || r.ResourceType != "session" || r.Action != "force_closed" ||
			r.SessionID == nil || *r.SessionID != r.ResourceID || r.ActorType != event.ActorSystem || r.ActorID != nil ||
			!reflect.DeepEqual(meta, map[string]any{"reason": "workspace deleted"}) {
			t.Errorf("event %s: workspace %v, resource_type %q, action %q, session_id %v, actor %q/%v, metadata %s; "+
				"want demo, session, force_closed, the resource id, system/null, {reason: workspace deleted}",
				r.ResourceID, r.Workspace, r.ResourceType, r.Action, r.SessionID, r.ActorType, r.ActorID, r.Metadata)
		}
	}
	slices.Sort(got)
	want := slices.Sorted(slices.Values(ids[:2]))
	if !slices.Equal(got, want) {
		t.Errorf("the events' resource ids %v, want the closed sessions %v", got, want)
	}
}

// errEventDown is the failure of failingEmitter.
var errEventDown = errors.New("the event log cannot be written")

// failingEmitter writes events through next until it is armed, and then
// fails the second event it is asked to write.
type failingEmitter struct {
	next  session.Emitter
	armed atomic.Bool
	calls atomic.Int32
}

func (e *failingEmitter) Emit(ctx context.Context, tx *sql.Tx, in event.Input) (event.Record, error) {
	if e.armed.Load() && e.calls.Add(1) == 2 {
		return event.Record{}, errEventDown
	}
	return e.next.Emit(ctx, tx, in)
}

func TestCloseActive_LeavesNothingWhenItsWorkIsRolledBackOrAnEventFails(t *testing.T) {
	events := &failingEmitter{}
	w := newWorld(t, func(d *session.Deps) {
		events.next = d.Events
		d.Events = events
	})
	ids := w.openOn(t, demo, demo)
	ctx := context.Background()
	errRollBack := errors.New("roll the caller's transaction back")
	for _, c := range []struct {
		name string
		// run runs fn in the store, rolling back what CloseActive wrote.
		run func(fn func(tx *sql.Tx) error) error
	}{
		{"the transaction is rolled back", func(fn func(tx *sql.Tx) error) error {
			err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
				if err := fn(tx); err != nil {
					return err
				}
				return errRollBack
			})
			if errors.Is(err, errRollBack) {
				return nil
			}
			return err
		}},
		{"a savepoint is rolled back", func(fn func(tx *sql.Tx) error) error {
			return w.Store.Tx(ctx, func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(ctx, `SAVEPOINT close_sessions`); err != nil {
					return err
				}
				if err := fn(tx); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `ROLLBACK TO close_sessions`); err != nil {
					return err
				}
				_, err := tx.ExecContext(ctx, `RELEASE close_sessions`)
				return err
			})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var n int
			err := c.run(func(tx *sql.Tx) error {
				var err error
				n, err = w.sessions.CloseActive(ctx, tx, demo, "workspace archived", w.clock.Now())
				return err
			})
			if n != 2 || err != nil {
				t.Fatalf("CloseActive before the rollback = %d, %v; want 2, nil", n, err)
			}
			w.wantActive(t, ids...)
			if recs := w.forceClosed(t); len(recs) != 0 {
				t.Errorf("force_closed events after the rollback: %v, want none", recs)
			}
		})
	}
	t.Run("the second event cannot be written", func(t *testing.T) {
		events.armed.Store(true)
		_, err := w.closeActive(demo, "workspace archived", w.clock.Now())
		if !errors.Is(err, errEventDown) {
			t.Fatalf("CloseActive with a failing event log = %v, want the event failure %v", err, errEventDown)
		}
		w.wantActive(t, ids...)
		if recs := w.forceClosed(t); len(recs) != 0 {
			t.Errorf("force_closed events after the failure: %v, want none", recs)
		}
	})
}

func TestCloseActive_LeavesEndedSessionsAsTheyAre(t *testing.T) {
	w := newWorld(t)
	var ended []string
	for _, status := range []string{"completed", "failed", "timeout"} {
		id := w.openOn(t, demo)[0]
		w.clock.Advance(time.Second)
		if code, raw := w.complete(t, w.aliceKey, id, map[string]any{"status": status, "error_message": "by " + status}); code != http.StatusOK {
			t.Fatalf("POST complete %s = %d %s, want 200", status, code, raw)
		}
		ended = append(ended, id)
	}
	snapshot := func() []map[string]any {
		var out []map[string]any
		for _, id := range ended {
			out = append(out, w.session(t, id))
		}
		return out
	}
	before := snapshot()
	w.clock.Advance(time.Minute)

	if n := w.forceClose(t, demo, "workspace deleted"); n != 0 {
		t.Errorf("CloseActive of a workspace with only ended sessions = %d, want 0", n)
	}
	if after := snapshot(); !reflect.DeepEqual(after, before) {
		t.Errorf("the ended sessions after CloseActive:\n%v\nwant\n%v", after, before)
	}

	terminated := w.openOn(t, demo)[0]
	if n := w.forceClose(t, demo, "fork expired"); n != 1 {
		t.Fatalf("CloseActive of the active session = %d, want 1", n)
	}
	closedAt := w.clock.stamp()
	w.clock.Advance(time.Minute)
	if n := w.forceClose(t, demo, "workspace deleted"); n != 0 {
		t.Errorf("CloseActive a second time = %d, want 0", n)
	}
	w.wantTerminated(t, closedAt, "fork expired", terminated)
	if n := w.forceClose(t, "nowhere", "workspace deleted"); n != 0 {
		t.Errorf("CloseActive of a slug without sessions = %d, want 0", n)
	}
	if recs := w.forceClosed(t); len(recs) != 1 || recs[0].ResourceID != terminated {
		t.Errorf("force_closed events %v, want the one of the session closed once", recs)
	}
}

func TestCloseActive_RefusesAnUnknownReasonAndAnEmptySlug(t *testing.T) {
	w := newWorld(t)
	id := w.openOn(t, demo)[0]
	for _, c := range []struct{ name, slug, reason string }{
		{"reason empty", demo, ""},
		{"reason archived", demo, "archived"},
		{"reason timeout", demo, "timeout"},
		{"reason in another case", demo, "Workspace Deleted"},
		{"slug empty", "", "workspace deleted"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			var n int
			var closeErr error
			err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
				n, closeErr = w.sessions.CloseActive(ctx, tx, c.slug, c.reason, w.clock.Now())
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if n != 0 || closeErr == nil {
				t.Errorf("CloseActive(%q, %q) = %d, %v; want 0 and an error", c.slug, c.reason, n, closeErr)
			}
			w.wantActive(t, id)
			if recs := w.forceClosed(t); len(recs) != 0 {
				t.Errorf("force_closed events: %v, want none", recs)
			}
		})
	}
}

func TestCloseActive_RacesWithACompleteAndOneWins(t *testing.T) {
	w := newWorld(t)
	const reason = "workspace deleted"
	for range 20 {
		id := w.openOn(t, demo)[0]
		var (
			wg           sync.WaitGroup
			status       int
			answer       []byte
			postErr      error
			n            int
			closeErr     error
			completePath = "/api/v1/sessions/" + id + "/complete"
		)
		wg.Go(func() {
			status, answer, postErr = w.rawPostTo(completePath, w.aliceKey, map[string]any{"status": "completed"})
		})
		wg.Go(func() { n, closeErr = w.closeActive(demo, reason, w.clock.Now()) })
		wg.Wait()
		if postErr != nil || closeErr != nil {
			t.Fatalf("complete: %v; CloseActive: %v", postErr, closeErr)
		}
		if status != http.StatusOK {
			t.Fatalf("POST complete = %d %s, want 200", status, answer)
		}
		s := w.session(t, id)
		if got := decode(t, answer)["status"]; got != s["status"] {
			t.Errorf("the complete answered status %v, stored %v", got, s["status"])
		}
		recs := w.forceClosed(t, id)
		switch s["status"] {
		case "terminated":
			if s["error_message"] != reason || n != 1 || len(recs) != 1 {
				t.Errorf("terminated: error_message %v, CloseActive = %d, events %d; want %q, 1, 1", s["error_message"], n, len(recs), reason)
			}
		case "completed":
			if n != 0 || len(recs) != 0 {
				t.Errorf("completed: CloseActive = %d, events %d; want 0, 0", n, len(recs))
			}
		default:
			t.Errorf("status after the race %v, want completed or terminated", s["status"])
		}
	}
}

func TestSessionCloser_EndsTheSessionsOfARemovedForkAndOfAnArchivedWorkspace(t *testing.T) {
	w := newWorld(t)
	ids := w.openOn(t, demo, fork1, fork2, other)
	removedAt := w.clock.stamp()
	ctx := context.Background()

	// A fork's removal ends the fork's sessions with the reason it gives.
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := w.workspaces.RemoveOwned(ctx, tx, fork1, "fork expired")
		return err
	}); err != nil {
		t.Fatalf("RemoveOwned(%s) = %v", fork1, err)
	}
	w.wantTerminated(t, removedAt, "fork expired", ids[1])
	w.wantActive(t, ids[0], ids[2], ids[3])

	// An archive ends the sessions of the workspace and of its forks.
	w.clock.Advance(time.Minute)
	archivedAt := w.clock.stamp()
	if status, raw := w.Call(t, http.MethodPost, "/api/v1/workspaces/"+demo+"/archive", w.aliceKey, nil); status != http.StatusOK {
		t.Fatalf("POST archive = %d %s, want 200", status, raw)
	}
	w.wantTerminated(t, archivedAt, "workspace archived", ids[0], ids[2])
	w.wantTerminated(t, removedAt, "fork expired", ids[1])
	w.wantActive(t, ids[3])

	reasons := map[string]string{}
	for _, r := range w.forceClosed(t) {
		var meta struct{ Reason string }
		if err := json.Unmarshal(r.Metadata, &meta); err != nil {
			t.Fatal(err)
		}
		reasons[r.ResourceID] = meta.Reason
	}
	want := map[string]string{ids[0]: "workspace archived", ids[1]: "fork expired", ids[2]: "workspace archived"}
	if !reflect.DeepEqual(reasons, want) {
		t.Errorf("force_closed events by session %v, want %v", reasons, want)
	}
}
