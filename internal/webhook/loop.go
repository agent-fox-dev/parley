package webhook

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"uuid"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/secret"
)

// readBatch is how many events a loop reads from the log at a time.
const readBatch = 100

// The waits of a loop that cannot go on.
const (
	// secretWait is how long a loop waits before it tries again to open a
	// signing secret that no configured key opens: within a minute, and no
	// attempt meanwhile, since an unsigned delivery must never leave the hub.
	secretWait = time.Minute
	// errorWait is how long a loop waits after a read or write of the store
	// failed, so that a store in trouble is not hammered.
	errorWait = 10 * time.Second
)

// loops are the delivery loops of the subscriptions, one per active or
// failing subscription, each in a goroutine of its own, so that a receiver
// that hangs holds up its subscription alone. They run under the context
// Run is given; a loop asked for before Run starts waits for it.
type loops struct {
	// register registers the wake-up with the event component once, when
	// the first loop is asked for, so that a hub without subscriptions keeps
	// the event component from following commits for nobody.
	register sync.Once

	mu      sync.Mutex
	ctx     context.Context
	stopped bool
	running map[string]*loop
	wg      sync.WaitGroup
}

// loop is the delivery loop of one subscription. wake holds at most one
// wake-up: a loop busy with an attempt is woken once when it is done,
// however many events were committed meanwhile.
//
// unopened is the sealed signing secret that last failed to open, and
// retryAt the time it is tried again: the loop's own goroutine alone uses
// them, so that the wake-ups before then, one per event committed anywhere
// in the hub, neither try it nor log the failure again. A new secret, from a
// rotation, is tried at once.
type loop struct {
	id      string
	wake    chan struct{}
	started bool

	unopened []byte
	retryAt  time.Time
}

// waitsForSecret reports whether l waits until retryAt to try the sealed
// signing secret sealed again, which it failed to open less than a
// secretWait ago.
func (l *loop) waitsForSecret(sealed []byte, now time.Time) bool {
	return l.unopened != nil && bytes.Equal(l.unopened, sealed) && now.Before(l.retryAt)
}

// startLoop makes sure the subscription id has a running loop, and wakes it
// when it has one.
func (c *Component) startLoop(id string) {
	c.loops.register.Do(func() {
		if c.deps.Events != nil {
			c.deps.Events.OnCommit(c.wakeAll)
		}
	})
	ls := &c.loops
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if l, ok := ls.running[id]; ok {
		wakeUp(l)
		return
	}
	if ls.stopped {
		return
	}
	if ls.running == nil {
		ls.running = map[string]*loop{}
	}
	l := &loop{id: id, wake: make(chan struct{}, 1)}
	ls.running[id] = l
	if ls.ctx != nil {
		c.spawnLocked(l)
	}
}

// wakeLoop wakes the loop of the subscription id, if it has one, so that it
// reads its subscription again: after a change, or when it is gone.
func (c *Component) wakeLoop(id string) {
	ls := &c.loops
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if l, ok := ls.running[id]; ok {
		wakeUp(l)
	}
}

// wakeAll wakes every loop. It is the OnCommit callback, which runs on the
// event component's goroutine and must return at once (ADR 05): it never
// blocks, and a loop that already has a wake-up is left as it is.
func (c *Component) wakeAll(event.Record) {
	ls := &c.loops
	ls.mu.Lock()
	defer ls.mu.Unlock()
	for _, l := range ls.running {
		wakeUp(l)
	}
}

func wakeUp(l *loop) {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// spawnLocked starts the goroutine of l under the loops' context. ls.mu is
// held.
func (c *Component) spawnLocked(l *loop) {
	l.started = true
	c.loops.wg.Add(1)
	go c.run(c.loops.ctx, l)
}

// Run starts the loop of every active and failing subscription the store
// holds, then runs the delivery loops and the check that disables a
// subscription failing for too long until ctx ends, and waits for them: each
// returns as soon as ctx ends, abandoning an attempt in flight, whose
// request ends with ctx and of which nothing is recorded, so that its
// delivery stays pending for the next start. The hub runs it as background
// work.
func (c *Component) Run(ctx context.Context) {
	ls := &c.loops
	ls.mu.Lock()
	ls.ctx = ctx
	for _, l := range ls.running {
		if !l.started {
			c.spawnLocked(l)
		}
	}
	ls.wg.Add(1)
	ls.mu.Unlock()
	go func() {
		defer ls.wg.Done()
		c.watchFailing(ctx)
	}()
	c.resumeLoops(ctx)
	<-ctx.Done()
	ls.mu.Lock()
	ls.stopped = true
	ls.mu.Unlock()
	ls.wg.Wait()
}

// finish removes l from the running loops, unless it was woken since its
// last step: a wake-up that arrived after the step read the subscription
// may come from a change that wants the loop to go on, so l steps again.
func (c *Component) finish(l *loop) bool {
	ls := &c.loops
	ls.mu.Lock()
	defer ls.mu.Unlock()
	select {
	case <-l.wake:
		return false
	default:
	}
	delete(ls.running, l.id)
	return true
}

// next is what a loop does after a step.
type next int

const (
	// again steps once more at once.
	again next = iota
	// idle waits for a wake-up, or for the time the step names.
	idle
	// done ends the loop: its subscription is gone or disabled.
	done
)

// run is the loop of l: it steps until its subscription is gone or
// disabled, or ctx ends, and between steps waits for a wake-up or for the
// time the step asked for, on the component's clock.
func (c *Component) run(ctx context.Context, l *loop) {
	defer c.loops.wg.Done()
	for {
		what, until, err := c.step(ctx, l)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.deps.Logger.Warn("event webhook delivery loop", "webhook_id", l.id, "error", err.Error())
			what, until = idle, c.deps.Clock.Now().Add(errorWait)
		}
		switch what {
		case again:
			continue
		case done:
			if c.finish(l) {
				return
			}
			continue
		}
		if !c.sleep(ctx, l, until) {
			return
		}
	}
}

// sleep waits for a wake-up of l and, when until is set, for the clock to
// reach it; it reports false when ctx ended first.
func (c *Component) sleep(ctx context.Context, l *loop, until time.Time) bool {
	var fire <-chan time.Time
	if !until.IsZero() {
		t := c.deps.Clock.NewTimer(until.Sub(c.deps.Clock.Now()))
		defer t.Stop()
		fire = t.C()
	}
	select {
	case <-ctx.Done():
		return false
	case <-l.wake:
	case <-fire:
	}
	return true
}

// step does the next piece of work of the subscription of l: it reads the
// subscription from the store, so that a change applies from here on, then
// goes on with its pending delivery or, without one, with the events after
// its position.
//
// The clock is read before the subscription, and the pending delivery's
// next attempt is due by that reading: an attempt is made only with the
// subscription as it was stored once the attempt was due, so a change
// committed before then always applies to it. Read after the subscription,
// the clock could pass the due time while a step held a subscription read
// before the change, and that step would send to the old URL.
func (c *Component) step(ctx context.Context, l *loop) (next, time.Time, error) {
	now := c.deps.Clock.Now()
	db := c.deps.Store.Main()
	id := l.id
	sub, found, err := subscriptionByID(ctx, db, id)
	if err != nil {
		return idle, time.Time{}, err
	}
	if !found || sub.State == StateDisabled {
		return done, time.Time{}, nil
	}
	d, pending, err := pendingDelivery(ctx, db, id)
	if err != nil {
		return idle, time.Time{}, err
	}
	if pending {
		return c.resume(ctx, l, sub, d, now)
	}
	return c.examine(ctx, l, sub)
}

// examine reads the events after the position of sub and moves the position
// past those sub does not receive, without a record of them. At the first
// one it receives it records a pending delivery, with the position just
// before the event until the delivery ends, and makes the first attempt at
// once.
//
// A position from which events have left the log cannot be read on: it
// moves to just before the oldest event the log holds, and the events
// skipped count as one failed delivery. The subscription's state does not
// change, since the hub cannot tell which of them it would have received.
func (c *Component) examine(ctx context.Context, l *loop, sub Subscription) (next, time.Time, error) {
	recs, examined, err := c.deps.Events.ReadAfter(ctx, sub.Position, readBatch)
	var expired *event.PositionExpiredError
	if errors.As(err, &expired) {
		moved, err := movePosition(ctx, c.deps.Store.Main(), sub.ID, sub.Position, expired.OldestSeq-1)
		if err != nil {
			return idle, time.Time{}, err
		}
		if moved {
			c.report(resultFailed)
		}
		return again, time.Time{}, nil
	}
	if err != nil {
		return idle, time.Time{}, err
	}
	for _, rec := range recs {
		ok, err := c.receives(ctx, sub, rec)
		if err != nil {
			return idle, time.Time{}, err
		}
		if !ok {
			continue
		}
		d := delivery{ID: uuid.NewV4().String(), Seq: rec.Seq, Type: rec.Type}
		var opened bool
		err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
			// A subscription has one pending delivery at a time: the next
			// event waits until the last has ended.
			if _, pending, err := pendingDelivery(ctx, tx, sub.ID); err != nil || pending {
				return err
			}
			// The position moves past the events skipped before this one, not
			// past this one: that waits for the delivery to end.
			var err error
			if opened, err = movePosition(ctx, tx, sub.ID, sub.Position, rec.Seq-1); err != nil || !opened {
				return err
			}
			return insertDelivery(ctx, tx, sub.ID, d, c.deps.Clock.Now().UTC().Truncate(time.Second))
		})
		if err != nil || !opened {
			return again, time.Time{}, err
		}
		return c.attempt(ctx, l, sub, d, rec)
	}
	if examined > sub.Position {
		if _, err := movePosition(ctx, c.deps.Store.Main(), sub.ID, sub.Position, examined); err != nil {
			return idle, time.Time{}, err
		}
	}
	if len(recs) == readBatch {
		return again, time.Time{}, nil
	}
	return idle, time.Time{}, nil
}

// resume goes on with the pending delivery d of sub: it waits until its
// next attempt is due by now, the time read before sub, then reads its
// event again and makes the attempt. An event that has left the log
// meanwhile cannot be sent: the delivery ends failed, counted, the position
// moves past it, or to just before the oldest event when that is further,
// and the subscription's state does not change. While its secret waits to
// be tried again, it waits too.
func (c *Component) resume(ctx context.Context, l *loop, sub Subscription, d delivery, now time.Time) (next, time.Time, error) {
	if d.NextAttemptAt != nil && d.NextAttemptAt.After(now) {
		return idle, *d.NextAttemptAt, nil
	}
	if l.waitsForSecret(sub.Sealed, now) {
		return idle, l.retryAt, nil
	}
	recs, _, err := c.deps.Events.ReadAfter(ctx, d.Seq-1, 1)
	var expired *event.PositionExpiredError
	gone := errors.As(err, &expired)
	if err != nil && !gone {
		return idle, time.Time{}, err
	}
	if !gone && (len(recs) == 0 || recs[0].Seq != d.Seq) {
		gone = true
	}
	if !gone {
		return c.attempt(ctx, l, sub, d, recs[0])
	}
	past := d.Seq
	if expired != nil {
		past = max(past, expired.OldestSeq-1)
	}
	var dropped bool
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		dropped, err = dropDelivery(ctx, tx, sub.ID, d, past)
		return err
	})
	if err != nil {
		return idle, time.Time{}, err
	}
	if dropped {
		c.report(resultFailed)
	}
	return again, time.Time{}, nil
}

// attempt sends rec to sub as an attempt of the delivery d of l, signed
// with the secret as it is stored now, and records the outcome. A secret no
// key opens sends nothing, is logged once, and is tried again a secretWait
// later. An attempt whose ctx ended is abandoned: its outcome is not
// recorded and the delivery stays pending.
func (c *Component) attempt(ctx context.Context, l *loop, sub Subscription, d delivery, rec event.Record) (next, time.Time, error) {
	now := c.deps.Clock.Now()
	if l.waitsForSecret(sub.Sealed, now) {
		return idle, l.retryAt, nil
	}
	plain, err := secret.OpenValue(c.deps.Keys(), binding(sub.ID), sub.Sealed)
	if err != nil {
		l.unopened, l.retryAt = sub.Sealed, now.Add(secretWait)
		c.deps.Logger.Warn("cannot open the signing secret of an event webhook; no attempt is made",
			"webhook_id", sub.ID, "error", err.Error())
		return idle, l.retryAt, nil
	}
	l.unopened = nil
	c.learn(plain)
	body, err := rec.MarshalJSON()
	if err != nil {
		return idle, time.Time{}, err
	}
	at := c.deps.Clock.Now().UTC().Truncate(time.Second)
	out := c.sender.send(ctx, attempt{
		url: sub.URL, body: body, eventType: rec.Type, seq: rec.Seq, delivery: d.ID, secret: plain,
	})
	if ctx.Err() != nil {
		return idle, time.Time{}, ctx.Err()
	}
	if out.success {
		return c.succeeded(ctx, sub, d, at, out)
	}
	return c.failed(ctx, sub, d, at, c.deps.Clock.Now().UTC(), out)
}

// succeeded records the successful attempt at at of the delivery d of sub,
// in one transaction: the delivery succeeded, and on the subscription the
// position past the event, last_status succeeded, last_delivery_at and no
// failing_since.
func (c *Component) succeeded(ctx context.Context, sub Subscription, d delivery, at time.Time, out outcome) (next, time.Time, error) {
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		recorded, err := recordAttempt(ctx, tx, d, at, out, deliverySucceeded, nil)
		if err != nil || !recorded {
			return err
		}
		return recordSuccess(ctx, tx, sub.ID, d.Seq, at)
	})
	if err != nil {
		return idle, time.Time{}, err
	}
	c.report(resultOK)
	return again, time.Time{}, nil
}

// failed records the failed attempt at at of the delivery d of sub, which
// ended at ended. One made at the end of its event's retry window or later
// gives the event up (giveUp). Any other is recorded in one transaction: the
// delivery stays pending with its next attempt scheduled the wait after
// ended (nextAttempt), not after at, which is cut to the whole second, so
// that the receiver never sees the retry sooner than the wait after the
// attempt it failed, and the subscription gets last_status failed,
// last_delivery_at and, when it has none, failing_since. The loop then waits
// for that time and sends nothing else for sub meanwhile, so that no later
// event overtakes the pending one; a wake-up before it changes nothing, since
// resume waits again.
func (c *Component) failed(ctx context.Context, sub Subscription, d delivery, at, ended time.Time, out outcome) (next, time.Time, error) {
	first := at
	if d.FirstAttemptAt != nil {
		first = *d.FirstAttemptAt
	}
	if !at.Before(first.Add(retryWindow)) {
		return c.giveUp(ctx, sub, d, at, out)
	}
	due := nextAttempt(first, ended, d.Attempts+1)
	var recorded bool
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		if recorded, err = recordAttempt(ctx, tx, d, at, out, deliveryPending, &due); err != nil || !recorded {
			return err
		}
		return recordFailure(ctx, tx, sub.ID, at)
	})
	if err != nil {
		return idle, time.Time{}, err
	}
	if !recorded {
		// Something ended the delivery while the attempt was in flight: no
		// retry follows, and the attempt made still counts once, as a
		// successful one does.
		c.report(resultFailed)
		return again, time.Time{}, nil
	}
	c.report(resultRetry)
	return idle, due, nil
}
