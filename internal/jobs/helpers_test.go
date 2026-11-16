package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/store"
)

// clockStart is where a fake clock starts unless the test sets another time.
var clockStart = time.Date(2026, 11, 14, 9, 30, 15, 0, time.UTC)

// fakeClock is a Clock whose time moves only when the test moves it. Its
// timers fire when Advance or Set reaches them.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	// changed is closed and replaced whenever a timer is added, so that
	// WaitTimers wakes up without polling.
	changed chan struct{}
}

// fakeTimer is a timer of a fakeClock.
type fakeTimer struct {
	clock *fakeClock
	at    time.Time
	c     chan time.Time
}

// newFakeClock returns a fake clock set to now.
func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{now: now, changed: make(chan struct{})}
}

// Now returns the clock's time.
func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// NewTimer returns a timer that fires once the clock reaches now + d; a
// timer of d <= 0 fires at once.
func (f *fakeClock) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{clock: f, at: f.now.Add(d), c: make(chan time.Time, 1)}
	if d <= 0 {
		t.c <- f.now
		return t
	}
	f.timers = append(f.timers, t)
	close(f.changed)
	f.changed = make(chan struct{})
	return t
}

// C is the channel the timer fires on.
func (t *fakeTimer) C() <-chan time.Time { return t.c }

// Stop takes the timer off its clock; it reports whether the timer was still
// pending.
func (t *fakeTimer) Stop() bool {
	f := t.clock
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, p := range f.timers {
		if p == t {
			f.timers = append(f.timers[:i], f.timers[i+1:]...)
			return true
		}
	}
	return false
}

// Set moves the clock to now and fires every timer that is then due.
func (f *fakeClock) Set(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = now
	pending := f.timers[:0]
	for _, t := range f.timers {
		if t.at.After(now) {
			pending = append(pending, t)
			continue
		}
		t.c <- now
	}
	f.timers = pending
}

// Advance moves the clock forward by d and fires every timer that is then
// due.
func (f *fakeClock) Advance(d time.Duration) { f.Set(f.Now().Add(d)) }

// WaitTimers waits until at least n timers are pending on the clock, so that
// a test advances it only once the code under test waits on it. It fails the
// test after 5 s.
func (f *fakeClock) WaitTimers(t testing.TB, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		f.mu.Lock()
		pending, changed := len(f.timers), f.changed
		f.mu.Unlock()
		if pending >= n {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("%d timers pending after 5 s, want at least %d", pending, n)
		}
	}
}

// openStore opens the store of dir with the job table, closing it when the
// test ends.
func openStore(t testing.TB, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(),
		store.Options{DatabasePath: filepath.Join(dir, "parley.db"), WorkspaceRoot: filepath.Join(dir, "ws")},
		[]store.Step{SchemaStep})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// testQueue is a queue over a store of its own, on a fake clock, without
// the dispatcher's wake: nothing runs unless the test runs a pass.
type testQueue struct {
	*Queue
	st    *store.Store
	clock *fakeClock
}

// newTestQueue returns a queue over a new store in t.TempDir().
func newTestQueue(t testing.TB) *testQueue {
	t.Helper()
	st := openStore(t, t.TempDir())
	clock := newFakeClock(clockStart)
	return &testQueue{Queue: New(Deps{Store: st, Clock: clock, NoWake: true}), st: st, clock: clock}
}

// enqueue queues a job of typ for slug in a transaction of its own.
func (q *testQueue) enqueue(t testing.TB, typ, slug string, payload json.RawMessage) Job {
	t.Helper()
	var j Job
	err := q.st.Tx(context.Background(), func(tx *sql.Tx) error {
		var err error
		j, err = q.Enqueue(context.Background(), tx, typ, slug, payload)
		return err
	})
	if err != nil {
		t.Fatalf("enqueue %s for %s: %v", typ, slug, err)
	}
	return j
}

// seed stores a job of typ for slug in status, as the queue would have left
// it there, and returns it.
func (q *testQueue) seed(t testing.TB, typ, slug, status string) Job {
	t.Helper()
	j := q.enqueue(t, typ, slug, json.RawMessage(`{}`))
	if status == StatusQueued {
		return j
	}
	err := q.st.Tx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), `UPDATE jobs SET status = ? WHERE id = ?`, status, j.ID)
		return err
	})
	if err != nil {
		t.Fatalf("set job %s to %s: %v", j.ID, status, err)
	}
	j.Status = status
	return j
}

// count is the number of jobs of slug.
func (q *testQueue) count(t testing.TB, slug string) int {
	t.Helper()
	var n int
	err := q.st.Main().QueryRowContext(context.Background(), `SELECT count(*) FROM jobs WHERE workspace_slug = ?`, slug).Scan(&n)
	if err != nil {
		t.Fatalf("count jobs of %s: %v", slug, err)
	}
	return n
}
