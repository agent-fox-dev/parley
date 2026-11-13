package hub

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// backgroundWork is work the hub runs from the start to the stop.
type backgroundWork struct {
	name  string
	grace time.Duration
	fn    func(context.Context, *Runtime)
}

// AddBackgroundWork adds work that runs while the hub serves. The hub calls
// fn in a goroutine of its own after the start hooks and the bind, with a
// context that is cancelled when the stop begins, and fn returns once it
// has ended its work. The stop waits for it up to grace, all pieces of work
// at the same time; a grace above DefaultStopLimit counts as
// DefaultStopLimit, and one below zero as zero. Work that has not returned
// when its grace has passed is abandoned: the stores are closed under it.
// Work without a name or a function, or with a name already registered,
// stops the start.
func (c *Composition) AddBackgroundWork(name string, grace time.Duration, fn func(context.Context, *Runtime)) {
	switch {
	case name == "":
		c.errs = append(c.errs, errors.New("a piece of background work has no name"))
		return
	case fn == nil:
		c.errs = append(c.errs, fmt.Errorf("background work %q has no function", name))
		return
	}
	for _, w := range c.works {
		if w.name == name {
			c.errs = append(c.errs, fmt.Errorf("background work %q is registered twice", name))
			return
		}
	}
	c.works = append(c.works, backgroundWork{name: name, grace: min(max(grace, 0), DefaultStopLimit), fn: fn})
}

// ShuttingDown returns a channel that the hub closes, once, when the stop
// begins: when it handles SIGTERM or SIGINT, or when Run's context is
// cancelled, before it waits for anything. A handler that holds a long-lived
// response open watches it and ends the response, so that the stop does not
// wait out the request grace for it.
func (c *Composition) ShuttingDown() <-chan struct{} { return c.shutdown }

// wait is something the stop waits for: done is closed when it has ended,
// and the stop waits for it up to grace.
type wait struct {
	name  string
	done  <-chan struct{}
	grace time.Duration
}

// startBackgroundWork starts each piece of work in a goroutine of its own
// with ctx and rt, and returns what the stop waits for.
func startBackgroundWork(ctx context.Context, works []backgroundWork, rt *Runtime) []wait {
	waits := make([]wait, 0, len(works))
	for _, w := range works {
		done := make(chan struct{})
		go func() {
			defer close(done)
			w.fn(ctx, rt)
		}()
		waits = append(waits, wait{w.name, done, w.grace})
	}
	return waits
}

// stop ends serving: it cancels the context of the server, the maintenance
// loop and the background work with cancel, which makes the server stop
// accepting connections, closes the shutdown notification, and then waits
// for everything in waits at the same time, each up to its grace and none
// beyond the stop limit. What it stopped waiting for is logged and
// abandoned; the caller closes the stores next.
func (r *run) stop(cancel context.CancelFunc, notify chan<- struct{}, waits []wait) {
	cancel()
	close(notify)

	limit := time.NewTimer(r.stopLimit())
	defer limit.Stop()
	quit := make(chan struct{})
	defer close(quit)
	var wg sync.WaitGroup
	for _, w := range waits {
		wg.Go(func() {
			grace := time.NewTimer(w.grace)
			defer grace.Stop()
			select {
			case <-w.done:
			case <-grace.C:
			case <-quit:
			}
		})
	}
	all := make(chan struct{})
	go func() {
		wg.Wait()
		close(all)
	}()
	select {
	case <-all:
	case <-limit.C:
	}

	for _, w := range waits {
		select {
		case <-w.done:
		default:
			r.logger.Warn("abandoned at the stop", "work", w.name)
		}
	}
}

// stopLimit is Options.StopLimit, or DefaultStopLimit when it is zero.
func (r *run) stopLimit() time.Duration {
	if r.opts.StopLimit > 0 {
		return r.opts.StopLimit
	}
	return DefaultStopLimit
}

// loopGrace is Options.LoopGrace, or DefaultLoopGrace when it is zero.
func (r *run) loopGrace() time.Duration {
	if r.opts.LoopGrace > 0 {
		return r.opts.LoopGrace
	}
	return DefaultLoopGrace
}
