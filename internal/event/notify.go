package event

import (
	"context"
	"database/sql"
	"maps"
	"slices"
	"sync"
	"time"
)

// The store has no hook that runs after a commit, so the component learns of
// committed events by reading the counter row (ADR 05). These intervals are
// real time, not the component's Clock: they pace reads of the store, not a
// deadline any consumer sees.
const (
	// pollInterval is how often the counter row is read while anything
	// waits for a commit.
	pollInterval = 100 * time.Millisecond
	// wakeInterval is the longest a waiter waits without being woken, so
	// that a wake-up the component missed delays an event by at most that
	// and never loses it.
	wakeInterval = time.Second
)

// Counters are the numbers the component reports for the metrics of the
// hub.
type Counters struct {
	// LastSeq is the last seq written.
	LastSeq int64
	// OpenStreams is the number of event streams open now.
	OpenStreams int
	// WrittenByType is the number of events committed since the start, by
	// type; an emit whose transaction rolled back is not counted.
	WrittenByType map[string]int64
}

// notifier follows the commits of events: it counts them by type, hands them
// to the OnCommit callbacks and wakes the waiters.
type notifier struct {
	// catching orders the catch-ups, so that seen only rises and pending
	// holds the records in seq order.
	catching sync.Mutex

	mu sync.Mutex
	// seen is the last seq observed committed; it starts at last_seq when
	// the component is bound, which is where "since the start" counts from.
	seen   int64
	byType map[string]int64
	places int
	// callbacks are those of OnCommit; pending are the committed records
	// they have not been given yet.
	callbacks []func(Record)
	pending   []Record
	// wake is closed at the next observed commit, or once it is
	// wakeInterval old; nil while nobody waits.
	wake      chan struct{}
	wakeSince time.Time
	// kick wakes Run to read the counter row at once.
	kick chan struct{}
}

// closedWake is the channel waitAfter returns when the commit is already
// observed.
var closedWake = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

// start sets where the counts begin.
func (n *notifier) start(lastSeq int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.seen = lastSeq
	n.byType = map[string]int64{}
}

// kicks returns the kick channel, made on first use so that the zero
// component works.
func (n *notifier) kicks() chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.kicksLocked()
}

// kicksLocked is kicks with n.mu held.
func (n *notifier) kicksLocked() chan struct{} {
	if n.kick == nil {
		n.kick = make(chan struct{}, 1)
	}
	return n.kick
}

// poke asks Run to read the counter row now; kicks that arrive while one is
// pending are one.
func (n *notifier) poke() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pokeLocked()
}

// pokeLocked is poke with n.mu held.
func (n *notifier) pokeLocked() {
	select {
	case n.kicksLocked() <- struct{}{}:
	default:
	}
}

// busy reports whether anything waits for a commit: a waiter, a callback
// or a record a callback has not been given.
func (n *notifier) busy() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.wake != nil || len(n.callbacks) > 0 || len(n.pending) > 0
}

// wakeLocked wakes every waiter. n.mu is held.
func (n *notifier) wakeLocked() {
	if n.wake != nil {
		close(n.wake)
		n.wake = nil
	}
}

// OnCommit registers fn to be called with every event committed from now on,
// once each and in seq order, from one goroutine of the component, shortly
// after its transaction commits. fn must return quickly: the next event waits
// for it. The calls run while the hub runs the component's Run.
func (c *Component) OnCommit(fn func(Record)) {
	if fn == nil {
		return
	}
	n := &c.notify
	n.mu.Lock()
	n.callbacks = append(n.callbacks, fn)
	n.mu.Unlock()
	n.poke()
}

// Counters returns the component's counters. It first reads the commits it
// has not observed yet, so the answer is exact at the time of the call; when
// the store cannot be read, the counts are those last observed.
func (c *Component) Counters() Counters {
	if c.deps.Store != nil {
		_ = c.catchUp(context.Background())
	}
	n := &c.notify
	n.mu.Lock()
	defer n.mu.Unlock()
	byType := maps.Clone(n.byType)
	if byType == nil {
		byType = map[string]int64{}
	}
	return Counters{LastSeq: n.seen, OpenStreams: n.places, WrittenByType: byType}
}

// takePlace takes one of limit stream places and reports whether one was
// free; a stream that got one gives it back with givePlace when it ends.
func (c *Component) takePlace(limit int) bool {
	n := &c.notify
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.places >= limit {
		return false
	}
	n.places++
	return true
}

// givePlace gives back a place takePlace took.
func (c *Component) givePlace() {
	n := &c.notify
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.places > 0 {
		n.places--
	}
}

// waitAfter returns a channel that is closed once the component has observed
// a commit of a seq higher than pos, at once when it already has, and in any
// case within a second. A stream or long-poll that has examined the log up to
// pos waits on it, then reads the log by position again: a wake-up without a
// new event costs one read, and a missed one delays an event by at most a
// second. Only a running Run closes it.
func (c *Component) waitAfter(pos int64) <-chan struct{} {
	n := &c.notify
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.seen > pos {
		return closedWake
	}
	if n.wake == nil {
		n.wake = make(chan struct{})
		n.wakeSince = time.Now()
		n.pokeLocked()
	}
	return n.wake
}

// Run follows the commits of events until ctx ends: while anything waits for
// one, it reads the counter row every 100 ms and at every emit, counts the
// newly committed events, calls the OnCommit callbacks and wakes the
// waiters. While nothing waits, it reads nothing. The hub runs it as
// background work, so the stop waits for it before the store closes. When
// it returns it wakes every waiter one last time.
func (c *Component) Run(ctx context.Context) {
	n := &c.notify
	kick := n.kicks()
	var tick <-chan time.Time
	for {
		if tick == nil && n.busy() {
			tick = time.After(pollInterval)
		}
		select {
		case <-ctx.Done():
			n.mu.Lock()
			n.wakeLocked()
			n.mu.Unlock()
			return
		case <-kick:
		case <-tick:
			tick = nil
		}
		if !n.busy() {
			continue
		}
		var failed bool
		if c.deps.Store != nil {
			failed = c.catchUp(ctx) != nil
		}
		n.mu.Lock()
		// A waiter woken after a failed read reads the log itself and meets
		// the failure there.
		if failed || (n.wake != nil && time.Since(n.wakeSince) >= wakeInterval-pollInterval) {
			n.wakeLocked()
		}
		recs, callbacks := n.pending, slices.Clone(n.callbacks)
		n.pending = nil
		n.mu.Unlock()
		for _, rec := range recs {
			for _, fn := range callbacks {
				fn(rec)
			}
		}
	}
}

// catchUp reads the events committed since the last seq it observed, in one
// snapshot of the store: it counts them by type, queues them for the
// callbacks when there are any, and wakes the waiters when there was a new
// one.
func (c *Component) catchUp(ctx context.Context) error {
	n := &c.notify
	n.catching.Lock()
	defer n.catching.Unlock()
	n.mu.Lock()
	from, wantRecords := n.seen, len(n.callbacks) > 0
	n.mu.Unlock()

	var last int64
	var recs []Record
	counts := map[string]int64{}
	err := c.snapshot(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT last_seq FROM hub_event_seq`).Scan(&last); err != nil {
			return err
		}
		if last <= from {
			return nil
		}
		if wantRecords {
			var err error
			if recs, err = queryRecords(ctx, tx, from, last, 0); err != nil {
				return err
			}
			for _, rec := range recs {
				counts[rec.Type]++
			}
			return nil
		}
		return countByType(ctx, tx, from, last, counts)
	})
	if err != nil || last <= from {
		return err
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	n.seen = last
	if n.byType == nil {
		n.byType = map[string]int64{}
	}
	for typ, k := range counts {
		n.byType[typ] += k
	}
	if len(recs) > 0 {
		n.pending = append(n.pending, recs...)
		n.pokeLocked()
	}
	n.wakeLocked()
	return nil
}

// countByType adds the number of stored events with after < seq <= upTo to
// counts, by type.
func countByType(ctx context.Context, tx *sql.Tx, after, upTo int64, counts map[string]int64) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT type, count(*) FROM hub_events WHERE seq > ? AND seq <= ? GROUP BY type`, after, upTo)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var typ string
		var k int64
		if err := rows.Scan(&typ, &k); err != nil {
			return err
		}
		counts[typ] += k
	}
	return rows.Err()
}
