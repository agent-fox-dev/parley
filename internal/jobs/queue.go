package jobs

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/store"
)

// DefaultInterval is how often the dispatcher passes when Deps sets no
// interval, so that a queued job starts within seconds even without a wake.
const DefaultInterval = time.Second

// Clock is the queue's time: the time a job is stamped with, and the timers
// the dispatcher runs on, so that a test controls both.
type Clock interface {
	// Now is the current time.
	Now() time.Time
	// NewTimer returns a timer that fires once, d from now.
	NewTimer(d time.Duration) Timer
}

// Timer is a timer of a Clock.
type Timer interface {
	// C is the channel the timer sends the time on when it fires.
	C() <-chan time.Time
	// Stop prevents the timer from firing; it reports whether the timer was
	// still pending.
	Stop() bool
}

// SystemClock is the Clock of a running hub: now for the time (time.Now when
// nil) and the runtime's timers.
func SystemClock(now func() time.Time) Clock {
	if now == nil {
		now = time.Now
	}
	return systemClock{now: now}
}

type systemClock struct{ now func() time.Time }

func (c systemClock) Now() time.Time { return c.now() }

func (c systemClock) NewTimer(d time.Duration) Timer { return systemTimer{time.NewTimer(d)} }

type systemTimer struct{ t *time.Timer }

func (t systemTimer) C() <-chan time.Time { return t.t.C }

func (t systemTimer) Stop() bool { return t.t.Stop() }

// Deps are what a queue runs on.
type Deps struct {
	// Store holds the jobs.
	Store *store.Store
	// Clock is the queue's time; SystemClock(time.Now) when nil.
	Clock Clock
	// Logger receives what the queue cannot return; nothing is logged when
	// nil.
	Logger *slog.Logger
	// Interval is the time between the dispatcher's passes; DefaultInterval
	// when 0.
	Interval time.Duration
	// NoWake leaves the dispatcher to its interval alone, so that a test
	// decides when a pass runs.
	NoWake bool
}

// Queue is the job queue. Its zero value takes registrations; Bind gives
// it what it runs on. Its methods are safe for concurrent use.
type Queue struct {
	mu sync.Mutex
	d  Deps
	// wake holds one token: an enqueue leaves it for the dispatcher, which
	// passes at once instead of at its next interval.
	wake chan struct{}
}

// ErrNotFound is the error of an operation on a job that does not exist.
var ErrNotFound = errors.New("job not found")

// errNotBound is the error of an operation that needs the store before
// Bind.
var errNotBound = errors.New("the job queue is not bound")

// New returns a queue bound to d.
func New(d Deps) *Queue {
	q := &Queue{}
	q.Bind(d)
	return q
}

// Bind gives the queue what it runs on, filling in the defaults of Deps.
func (q *Queue) Bind(d Deps) {
	if d.Clock == nil {
		d.Clock = SystemClock(time.Now)
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Interval <= 0 {
		d.Interval = DefaultInterval
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.d = d
	if q.wake == nil {
		q.wake = make(chan struct{}, 1)
	}
}

// deps returns what the queue runs on, or errNotBound before Bind.
func (q *Queue) deps() (Deps, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.d.Store == nil {
		return Deps{}, errNotBound
	}
	return q.d, nil
}

// poke wakes the dispatcher for a pass, unless the queue runs without
// wakes. A pass claims inside a write transaction of the store, so one
// woken while the poking transaction is still open waits for it to end and
// then sees what it wrote.
func (q *Queue) poke() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.d.NoWake || q.wake == nil {
		return
	}
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Get returns the job id as committed; a job that does not exist is
// ErrNotFound.
func (q *Queue) Get(ctx context.Context, id string) (Job, error) {
	d, err := q.deps()
	if err != nil {
		return Job{}, err
	}
	return readJob(ctx, d.Store.Main(), id)
}

// List returns up to limit jobs of typ for the workspace slug, in status
// when it is not "", newest first: created_at as a record shows it, in
// whole seconds, descending, then id descending. after, when not nil, is
// the Position of the last job of the previous page.
func (q *Queue) List(ctx context.Context, typ, slug, status string, after *ListPosition, limit int) ([]Job, error) {
	d, err := q.deps()
	if err != nil {
		return nil, err
	}
	return listJobs(ctx, d.Store.Main(), typ, slug, status, after, limit)
}
