package event

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/scrub"
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

// testRowsStep creates the table the tests write their state changes to,
// beside the events that report them.
var testRowsStep = store.Step{
	Name:   "test_rows",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE test_rows (name TEXT NOT NULL PRIMARY KEY)`)
		return err
	},
}

// fixture is an event component on a real store in a directory of the test,
// with a fake clock and a real scrubber.
type fixture struct {
	t        testing.TB
	dir      string
	store    *store.Store
	clock    *fakeClock
	scrubber *scrub.Scrubber
	events   *Component
	// stop ends the component's Run and waits for it, before the store
	// closes under it.
	stop func()
}

// newFixture opens a store in a fresh directory with the event schema, the
// workspace records and a test table, and binds a component to it. The
// store is closed when the test ends.
func newFixture(t testing.TB) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: t.TempDir(), clock: newFakeClock(clockStart), scrubber: scrub.New()}
	f.open()
	return f
}

// open opens the fixture's store, binds a new component to it and runs it,
// as a hub does at every start.
func (f *fixture) open() {
	f.t.Helper()
	st, err := store.Open(context.Background(), store.Options{
		DatabasePath:  filepath.Join(f.dir, "parley.db"),
		WorkspaceRoot: filepath.Join(f.dir, "workspaces"),
	}, []store.Step{access.SchemaStep, SchemaStep, testRowsStep})
	if err != nil {
		f.t.Fatalf("open store: %v", err)
	}
	f.t.Cleanup(func() { st.Close() })
	f.store = st
	f.events, err = New(Deps{Store: st, Clock: f.clock, Scrubber: f.scrubber, Parents: storeParents{}})
	if err != nil {
		f.t.Fatalf("bind the component: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.events.Run(ctx)
	}()
	f.stop = sync.OnceFunc(func() {
		cancel()
		<-done
	})
	// Cleanups run last first, so Run has ended before the store closes.
	f.t.Cleanup(f.stop)
}

// reopen closes the store and opens it again on the same file, with a new
// component: a restart of the hub.
func (f *fixture) reopen() {
	f.t.Helper()
	f.stop()
	if err := f.store.Close(); err != nil {
		f.t.Fatalf("close store: %v", err)
	}
	f.open()
}

// storeParents is the fork-parent lookup the hub installs: the parent slug
// of the workspace record when it is a fork.
type storeParents struct{}

func (storeParents) ForkParent(ctx context.Context, q Querier, slug string) (string, error) {
	rec, found, err := access.RecordBySlug(ctx, q, slug)
	if err != nil || !found || rec.Kind != access.KindFork {
		return "", err
	}
	return rec.ParentSlug, nil
}

// emit emits in in a committed transaction of its own and fails the test
// when the emit or the commit fails.
func (f *fixture) emit(in Input) Record {
	f.t.Helper()
	var rec Record
	err := f.store.Tx(context.Background(), func(tx *sql.Tx) error {
		var err error
		rec, err = f.events.Emit(context.Background(), tx, in)
		return err
	})
	if err != nil {
		f.t.Fatalf("emit %s: %v", in.Type, err)
	}
	return rec
}

// readAll returns every event of the log after position after.
func (f *fixture) readAll(after int64) []Record {
	f.t.Helper()
	recs, _, err := f.events.ReadAfter(context.Background(), after, 0)
	if err != nil {
		f.t.Fatalf("ReadAfter(%d): %v", after, err)
	}
	return recs
}

// bounds are the log's bounds.
func (f *fixture) bounds() Bounds {
	f.t.Helper()
	b, err := f.events.Bounds(context.Background())
	if err != nil {
		f.t.Fatalf("Bounds: %v", err)
	}
	return b
}

// lastSeq is last_seq of the log's bounds.
func (f *fixture) lastSeq() int64 {
	f.t.Helper()
	return f.bounds().LastSeq
}

// seqs are the seqs of recs, in their order.
func seqs(recs []Record) []int64 {
	var out []int64
	for _, rec := range recs {
		out = append(out, rec.Seq)
	}
	return out
}

// insertRow writes the test row name in tx.
func insertRow(ctx context.Context, tx *sql.Tx, name string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO test_rows (name) VALUES (?)`, name)
	return err
}

// hasRow reports whether the test row name was committed.
func (f *fixture) hasRow(name string) bool {
	f.t.Helper()
	var n int
	if err := f.store.Main().QueryRow(`SELECT count(*) FROM test_rows WHERE name = ?`, name).Scan(&n); err != nil {
		f.t.Fatalf("read test row: %v", err)
	}
	return n == 1
}

// ptr returns a pointer to v, for the optional fields of an Input.
func ptr[T any](v T) *T { return &v }

// valid is a valid input of type typ: a PAT acting on the workspace api.
func valid(typ string) Input {
	return Input{
		Type:         typ,
		Workspace:    ptr("api"),
		ActorType:    "pat",
		ActorID:      ptr("u_1"),
		ResourceType: "change",
		ResourceID:   "42",
	}
}
