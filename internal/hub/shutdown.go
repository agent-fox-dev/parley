package hub

import (
	"context"
	"time"
)

// The time limits of a shutdown. Requests get DefaultRequestGrace, the
// maintenance loop DefaultLoopGrace and each piece of background work its
// own grace, all measured from the signal and all cut to DefaultTotalGrace.
const (
	DefaultTotalGrace = 30 * time.Second
	DefaultLoopGrace  = 15 * time.Second
	// MaxWorkGrace is the longest grace background work can register.
	MaxWorkGrace = 30 * time.Second
)

// Work is background work: it runs from the start until its context is
// cancelled, and gets Grace to return once the hub is told to stop.
type Work struct {
	Name  string
	Grace time.Duration
	Run   func(ctx context.Context, rt *Runtime)
}

// AddWork registers background work. The hub starts it after the start
// hooks; a grace above 30 seconds counts as 30 seconds.
func (c *Composition) AddWork(name string, grace time.Duration, fn func(ctx context.Context, rt *Runtime)) {
	c.work = append(c.work, Work{Name: name, Grace: min(grace, MaxWorkGrace), Run: fn})
}

// ShuttingDown is closed once, when the hub is told to stop (SIGTERM or
// SIGINT), before any waiting begins. Handlers of long-lived responses end
// their response on it.
func (rt *Runtime) ShuttingDown() <-chan struct{} { return rt.shutdown }

// running is a piece of background work, or the maintenance loop, that has
// been started.
type running struct {
	name  string
	grace time.Duration
	done  chan struct{}
}

// startWork starts every registered piece of background work.
func (r *hubRun) startWork(ctx context.Context) []running {
	var out []running
	for _, w := range r.comp.work {
		run := running{name: w.Name, grace: w.Grace, done: make(chan struct{})}
		go func(w Work) {
			defer close(run.done)
			w.Run(ctx, r.rt)
		}(w)
		out = append(out, run)
	}
	return out
}

func (r *hubRun) totalGrace() time.Duration {
	if r.opts.TotalGrace > 0 {
		return r.opts.TotalGrace
	}
	return DefaultTotalGrace
}

func (r *hubRun) loopGrace() time.Duration {
	if r.opts.LoopGrace > 0 {
		return r.opts.LoopGrace
	}
	return DefaultLoopGrace
}

func (r *hubRun) requestGrace() time.Duration {
	g := DefaultRequestGrace
	if r.opts.RequestGrace > 0 {
		g = r.opts.RequestGrace
	}
	return min(g, r.totalGrace())
}

// waitStopped waits for each started piece until its grace, counted from
// since and cut to the total grace, has passed; what has not returned by
// then is abandoned.
func (r *hubRun) waitStopped(since time.Time, pieces []running) {
	total := r.totalGrace()
	for _, p := range pieces {
		deadline := since.Add(min(p.grace, total))
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-p.done:
		case <-timer.C:
			r.logger.Warn("background work did not stop within its grace", "work", p.name)
		}
		timer.Stop()
	}
}
