package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestStore_KeepsCommittedJobsAcrossAReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	st := openStore(t, dir)
	clock := newFakeClock(clockStart)
	q := New(Deps{Store: st, Clock: clock, NoWake: true})

	var enqueued []Job
	for i, slug := range []string{"a", "b", "c"} {
		clock.Advance(1500 * time.Millisecond) // created_at keeps its nanoseconds
		err := st.Tx(ctx, func(tx *sql.Tx) error {
			j, err := q.Enqueue(ctx, tx, "probe", slug, json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)))
			enqueued = append(enqueued, j)
			return err
		})
		if err != nil {
			t.Fatalf("enqueue for %s: %v", slug, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	q = New(Deps{Store: openStore(t, dir), Clock: newFakeClock(clockStart), NoWake: true})
	for _, want := range enqueued {
		got, err := q.Get(ctx, want.ID)
		if err != nil {
			t.Fatalf("Get %s after reopen: %v", want.ID, err)
		}
		if got.ID != want.ID || got.Type != want.Type || got.Workspace != want.Workspace || got.Status != want.Status ||
			string(got.Payload) != string(want.Payload) || !got.CreatedAt.Equal(want.CreatedAt) {
			t.Errorf("after reopen = %+v, want %+v", got, want)
		}
	}
}

// errUndo is what a test's transaction returns to roll back.
var errUndo = errors.New("undo")

func TestRemoveWorkspace_DeletesTheWorkspacesJobsInTheCallersTransaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := newTestQueue(t)
	var removed []Job
	for _, s := range []struct{ typ, status string }{
		{"probe", StatusCompleted}, {"probe", StatusDeadLetter}, {"probe", StatusFailed}, {"rebuild", StatusQueued},
	} {
		removed = append(removed, q.get(t, q.seed(t, s.typ, "w", s.status).ID))
	}
	other := q.get(t, q.seed(t, "probe", "other", StatusQueued).ID)
	remove := func(undo bool) error {
		return q.st.Tx(ctx, func(tx *sql.Tx) error {
			if err := q.RemoveWorkspace(ctx, tx, "w"); err != nil {
				return err
			}
			if undo {
				return errUndo
			}
			return nil
		})
	}

	if err := remove(true); !errors.Is(err, errUndo) {
		t.Fatalf("rolled-back removal: %v, want the rollback's error", err)
	}
	for _, j := range removed {
		if got := q.get(t, j.ID); !reflect.DeepEqual(got, j) {
			t.Errorf("after a rolled-back removal job = %+v, want %+v", got, j)
		}
	}

	if err := remove(false); err != nil {
		t.Fatalf("removal: %v", err)
	}
	for _, typ := range []string{"probe", "rebuild"} {
		if jobs, err := q.List(ctx, typ, "w", "", nil, 10); err != nil || len(jobs) != 0 {
			t.Errorf("List %s of the removed workspace = %v, %v; want none", typ, jobs, err)
		}
	}
	for _, j := range removed {
		if _, err := q.Get(ctx, j.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get %s of the removed workspace: %v, want ErrNotFound", j.ID, err)
		}
	}
	if got := q.get(t, other.ID); !reflect.DeepEqual(got, other) {
		t.Errorf("job of another workspace = %+v, want %+v", got, other)
	}
	q.enqueue(t, "probe", "w", json.RawMessage(`{}`))
}

func TestRemoveWorkspace_CancelsTheAttemptsOfItsJobsOnceTheRemovalCommits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := newTestQueue(t)
	h := newScriptedHandler(false)
	cancelled := make(chan string, 2)
	h.run = func(ctx context.Context, j Job) error {
		<-ctx.Done()
		cancelled <- j.ID
		return errors.New("interrupted")
	}
	var hooks atomic.Int32
	h.settled = func(context.Context, *sql.Tx, Job, bool) error {
		hooks.Add(1)
		return nil
	}
	q.Register("probe", h)
	a := q.enqueue(t, "probe", "w", json.RawMessage(`{}`))
	b := q.enqueue(t, "probe", "other", json.RawMessage(`{}`))
	q.pass(t)
	h.Enter(t)
	h.Enter(t)
	aDone := q.doneOf(a.ID)
	remove := func(undo bool) error {
		err := q.st.Tx(ctx, func(tx *sql.Tx) error {
			if err := q.RemoveWorkspace(ctx, tx, "w"); err != nil {
				return err
			}
			if undo {
				return errUndo
			}
			return nil
		})
		// What the removal set off has run.
		q.removals.Wait()
		return err
	}

	if err := remove(true); !errors.Is(err, errUndo) {
		t.Fatalf("rolled-back removal: %v, want the rollback's error", err)
	}
	select {
	case id := <-cancelled:
		t.Fatalf("a rolled-back removal cancelled the attempt of job %s", id)
	default:
	}
	if got := q.status(t, a.ID); got != StatusRunning || returned(aDone) {
		t.Fatalf("after a rolled-back removal job a is %s, attempt ended %v; want running", got, returned(aDone))
	}

	if err := remove(false); err != nil {
		t.Fatalf("removal: %v", err)
	}
	select {
	case id := <-cancelled:
		if id != a.ID {
			t.Fatalf("the removal of w cancelled the attempt of job %s, want %s", id, a.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the attempt of the removed job was not cancelled within 5 s")
	}
	waitClosed(t, aDone, "the end of the removed job's attempt")
	if _, err := q.Get(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get of the removed job after its attempt: %v, want ErrNotFound", err)
	}
	if n := q.count(t, "w"); n != 0 {
		t.Errorf("jobs of the removed workspace = %d, want 0", n)
	}
	if n := hooks.Load(); n != 0 {
		t.Errorf("settlement hook calls = %d, want 0", n)
	}
	if err := q.Pass(ctx); err != nil {
		t.Errorf("Pass after the removal: %v", err)
	}
	select {
	case id := <-cancelled:
		t.Errorf("the removal of w cancelled the attempt of job %s too", id)
	default:
	}
	if got := q.status(t, b.ID); got != StatusRunning {
		t.Errorf("job of another workspace is %s, want running", got)
	}
}
