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
	q := &testQueue{Queue: New(Deps{Store: st, Clock: clock, NoWake: true}), st: st, clock: clock}
	// Before the store closes: end the attempts a test left blocked.
	t.Cleanup(func() { q.cancelAll(t) })
	return q
}

// cancelAll cancels every attempt the queue runs and waits until each has
// ended.
func (q *Queue) cancelAll(t testing.TB) {
	t.Helper()
	q.mu.Lock()
	for _, w := range q.running {
		w.cancel()
	}
	q.mu.Unlock()
	q.settleAll(t)
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

// scriptedHandler is a Handler that records every attempt it is given. A
// blocking handler holds each attempt until the test releases it with the
// error the attempt returns; one that does not block returns nil at once.
type scriptedHandler struct {
	block bool
	// entered receives every job an attempt is given, as it begins.
	entered chan Job

	mu              sync.Mutex
	calls           []string
	jobs            []Job
	active, maxSeen int
	release         map[string]chan error
}

// newScriptedHandler returns a handler that blocks every attempt when block
// is set.
func newScriptedHandler(block bool) *scriptedHandler {
	return &scriptedHandler{block: block, entered: make(chan Job, 256), release: map[string]chan error{}}
}

// releaseOf is the channel the attempt of id waits on.
func (h *scriptedHandler) releaseOf(id string) chan error {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.release[id]
	if !ok {
		c = make(chan error, 1)
		h.release[id] = c
	}
	return c
}

// Run records the attempt and, when the handler blocks, waits for its
// release or for ctx.
func (h *scriptedHandler) Run(ctx context.Context, j Job) error {
	h.mu.Lock()
	h.calls = append(h.calls, j.ID)
	h.jobs = append(h.jobs, j)
	h.active++
	h.maxSeen = max(h.maxSeen, h.active)
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.active--
		h.mu.Unlock()
	}()
	h.entered <- j
	if !h.block {
		return nil
	}
	select {
	case err := <-h.releaseOf(j.ID):
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Settled accepts every settlement.
func (h *scriptedHandler) Settled(context.Context, *sql.Tx, Job, bool) error { return nil }

// Calls are the ids of the jobs the attempts were given, in call order.
func (h *scriptedHandler) Calls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

// Jobs are the jobs the attempts were given, in call order.
func (h *scriptedHandler) Jobs() []Job {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Job(nil), h.jobs...)
}

// MaxActive is the most attempts that were running at once.
func (h *scriptedHandler) MaxActive() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.maxSeen
}

// Enter waits for the next attempt to begin and returns its job; it fails
// the test after 5 s.
func (h *scriptedHandler) Enter(t testing.TB) Job {
	t.Helper()
	select {
	case j := <-h.entered:
		return j
	case <-time.After(5 * time.Second):
		t.Fatalf("no attempt began within 5 s")
		return Job{}
	}
}

// Idle reports that no attempt began that the test has not taken with
// Enter.
func (h *scriptedHandler) Idle(t testing.TB) {
	t.Helper()
	select {
	case j := <-h.entered:
		t.Fatalf("an attempt of job %s began, want none", j.ID)
	default:
	}
}

// pass runs one dispatcher pass and fails the test on its error.
func (q *testQueue) pass(t testing.TB) {
	t.Helper()
	if err := q.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
}

// doneOf is the channel that is closed once the running attempt of id has
// ended and its outcome is settled; nil when the queue runs no attempt of
// id.
func (q *Queue) doneOf(id string) <-chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	if w, ok := q.running[id]; ok {
		return w.done
	}
	return nil
}

// finish releases the blocked attempt of id with err and waits until the
// queue has settled it; it fails the test after 5 s.
func (q *testQueue) finish(t testing.TB, h *scriptedHandler, id string, err error) {
	t.Helper()
	done := q.doneOf(id)
	if done == nil {
		t.Fatalf("the queue runs no attempt of job %s", id)
	}
	h.releaseOf(id) <- err
	waitClosed(t, done, "the settlement of job "+id)
}

// settleAll waits until every attempt the queue runs now has ended and is
// settled; it fails the test after 5 s.
func (q *Queue) settleAll(t testing.TB) {
	t.Helper()
	q.mu.Lock()
	var dones []chan struct{}
	for _, w := range q.running {
		dones = append(dones, w.done)
	}
	q.mu.Unlock()
	for _, d := range dones {
		waitClosed(t, d, "a settlement")
	}
}

// waitClosed waits for c to be closed; it fails the test after 5 s.
func waitClosed(t testing.TB, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not happen within 5 s", what)
	}
}

// status is the status of the job id as committed.
func (q *testQueue) status(t testing.TB, id string) string {
	t.Helper()
	j, err := q.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	return j.Status
}

// fail leaves the job id failed after its attempt number retries, with
// errText and its next attempt at next, as a failed attempt's settlement
// does.
func (q *testQueue) fail(t testing.TB, id string, retries int, errText string, next time.Time) {
	t.Helper()
	err := q.st.Tx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(),
			`UPDATE jobs SET status = ?, retry_count = ?, error = ?, next_attempt_at = ? WHERE id = ?`,
			StatusFailed, retries, errText, next.UnixNano(), id)
		return err
	})
	if err != nil {
		t.Fatalf("fail job %s: %v", id, err)
	}
}
