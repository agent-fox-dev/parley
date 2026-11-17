package hubtest

import (
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/jobs"
)

// JobClock is a clock for a hub's job queue that the test controls, given
// to the queue with (*hub.Composition).AdjustJobs: its time is that of
// base, the clock the test gives the hub, plus what Advance has added, and
// its timers fire only when Advance reaches them, so that the queue passes
// on its interval or ends a stop's grace only when the test makes it. A
// hub that should see the time Advance adds is given Now as its clock.
type JobClock struct {
	base   func() time.Time
	mu     sync.Mutex
	offset time.Duration
	timers map[*jobTimer]bool
	// armed holds, for each duration, a channel closed once a timer of it
	// has been asked for.
	armed map[time.Duration]chan struct{}
}

// NewJobClock returns a job clock on base (time.Now when nil).
func NewJobClock(base func() time.Time) *JobClock {
	if base == nil {
		base = time.Now
	}
	return &JobClock{base: base, timers: map[*jobTimer]bool{}, armed: map[time.Duration]chan struct{}{}}
}

// Now is the clock's time.
func (c *JobClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.base().Add(c.offset)
}

// NewTimer returns a timer that fires once Advance has brought the clock to
// d from now.
func (c *JobClock) NewTimer(d time.Duration) jobs.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &jobTimer{clock: c, at: c.base().Add(c.offset + d), c: make(chan time.Time, 1)}
	c.timers[t] = true
	armed := c.armedLocked(d)
	select {
	case <-armed:
	default:
		close(armed)
	}
	return t
}

// Advance moves the clock on by d and fires the timers that are then due.
func (c *JobClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
	now := c.base().Add(c.offset)
	for t := range c.timers {
		if !t.at.After(now) {
			delete(c.timers, t)
			t.c <- now
		}
	}
}

// Armed is a channel closed once a timer of d has been asked for: the
// queue asks for one of jobs.DefaultInterval after each pass of its
// dispatcher, and one of jobs.ShutdownGrace when a stop waits for its
// attempts.
func (c *JobClock) Armed(d time.Duration) <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.armedLocked(d)
}

func (c *JobClock) armedLocked(d time.Duration) chan struct{} {
	ch, ok := c.armed[d]
	if !ok {
		ch = make(chan struct{})
		c.armed[d] = ch
	}
	return ch
}

type jobTimer struct {
	clock *JobClock
	at    time.Time
	c     chan time.Time
}

func (t *jobTimer) C() <-chan time.Time { return t.c }

func (t *jobTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	pending := t.clock.timers[t]
	delete(t.clock.timers, t)
	return pending
}

// WaitClosed waits up to 10 s for ch to be closed, and fails the test then.
func WaitClosed(t testing.TB, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("waited 10 s for %s", what)
	}
}
