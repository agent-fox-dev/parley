package land

import (
	"context"
	"errors"
	"sync"
	"time"
)

// The pool of a hub that names no limits.
const (
	// DefaultWorkers is how many attempts are computed at once.
	DefaultWorkers = 8
	// DefaultTimeout is how long an attempt may take.
	DefaultTimeout = 5 * time.Minute
)

// errTimedOut is the cause of the end of an attempt's context when its
// deadline passed, which tells it from the stop of the hub.
var errTimedOut = errors.New("the land attempt passed its deadline")

// pool runs the attempts handed to it, each in a goroutine of its own and
// at most Deps.Workers at once; there is no queue, a further attempt waits
// for a free worker in no promised order and stays running meanwhile.
type pool struct {
	mu sync.Mutex
	// ctx is the context of every run, nil until RunPool runs; pending are
	// the jobs handed over before it ran.
	ctx     context.Context
	pending []*job
	// stopped is set once RunPool's context ended: nothing starts after.
	stopped bool
	// jobs are the attempts the pool holds, by id, until their run
	// returned.
	jobs  map[string]*job
	slots chan struct{}
	runs  sync.WaitGroup
}

// job is one attempt the pool holds: the attempt as it was handed over,
// when its deadline counts from, whether a stop left it running and it
// runs again (Recover), and done, closed once the attempt is final or the
// pool has let go of it.
type job struct {
	a     Attempt
	from  time.Time
	again bool
	done  chan struct{}
	once  sync.Once

	mu sync.Mutex
	// swapping is set while the engine moves the target: the deadline
	// leaves the attempt's end to the engine then.
	swapping bool
}

// release closes done, once.
func (j *job) release() { j.once.Do(func() { close(j.done) }) }

// closed is the channel ended gives for an attempt the pool does not hold.
var closed = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

// RunPool runs the attempts handed to the land pool until ctx ends. Each
// has a deadline of Deps.Timeout from its creation, or from its re-run at
// a start, waiting for a worker included; past it the attempt's work is
// stopped, a running git command killed with it, and the attempt ends
// failed timeout. When ctx ends the pool stops every run without ending
// its attempt, which stays running for the next start to run it again, and
// returns once every run has returned.
func (c *Component) RunPool(ctx context.Context) {
	p := &c.pool
	p.mu.Lock()
	p.ctx = ctx
	for _, j := range p.pending {
		c.start(j)
	}
	p.pending = nil
	p.mu.Unlock()
	<-ctx.Done()
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	p.runs.Wait()
}

// submit hands the running attempt a to the pool, its deadline counted
// from from. It never waits and never fails: an attempt the pool cannot
// run now waits for a worker, and one handed over while the hub stops
// stays running for the next start. An attempt the pool holds already is
// not taken twice.
func (c *Component) submit(a Attempt, from time.Time) { c.hand(a, from, false) }

// hand is submit, of an attempt a stop left running for again.
func (c *Component) hand(a Attempt, from time.Time, again bool) {
	p := &c.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, held := p.jobs[a.ID]; held {
		return
	}
	j := &job{a: a, from: from, again: again, done: make(chan struct{})}
	if p.stopped {
		j.release()
		return
	}
	p.jobs[a.ID] = j
	switch {
	case c.deps.Paused:
	case p.ctx == nil:
		p.pending = append(p.pending, j)
	default:
		c.start(j)
	}
}

// ended returns a channel that is closed once the attempt id is final or
// the pool has let go of it, so that a waiter reads its outcome from the
// store then. It is closed already for an attempt the pool does not hold.
func (c *Component) ended(id string) <-chan struct{} {
	p := &c.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if j, held := p.jobs[id]; held {
		return j.done
	}
	return closed
}

// start runs the job j in a goroutine of its own; p.mu is held.
func (c *Component) start(j *job) {
	p := &c.pool
	p.runs.Add(1)
	go func() {
		defer p.runs.Done()
		defer func() {
			p.mu.Lock()
			delete(p.jobs, j.a.ID)
			p.mu.Unlock()
			j.release()
		}()
		c.attend(j)
	}()
}

// attend runs the attempt of j under its deadline once a worker is free,
// and ends it failed timeout when the deadline passes first, whether the
// attempt waits for a worker or runs. The worker stays taken until the
// engine has returned, so that no more replays than workers run at once.
func (c *Component) attend(j *job) {
	p := &c.pool
	ctx, cancel := context.WithTimeoutCause(p.ctx, c.deps.Timeout-c.now().Sub(j.from), errTimedOut)
	defer cancel()
	select {
	case p.slots <- struct{}{}:
	case <-ctx.Done():
		c.expire(ctx, j)
		return
	}
	defer func() { <-p.slots }()
	ran := make(chan error, 1)
	go func() {
		if j.again {
			ran <- c.rerun(ctx, j.a)
			return
		}
		ran <- c.run(ctx, j.a)
	}()
	var err error
	select {
	case err = <-ran:
	case <-ctx.Done():
		c.expire(ctx, j)
		err = <-ran
	}
	if err != nil && ctx.Err() == nil {
		c.deps.Logger.Error("land attempt", "workspace", j.a.Workspace, "land_id", j.a.ID, "error", err)
	}
}

// expire ends the attempt of j failed timeout once its context ended by
// its deadline, unless its engine is moving the target, which then ends
// the attempt itself; a context ended by the stop of the hub leaves it
// running.
func (c *Component) expire(ctx context.Context, j *job) {
	if !errors.Is(context.Cause(ctx), errTimedOut) {
		return
	}
	j.mu.Lock()
	swapping := j.swapping
	j.mu.Unlock()
	if swapping {
		return
	}
	if err := c.end(ctx, j.a); err != nil {
		c.deps.Logger.Error("land attempt: end past its deadline", "workspace", j.a.Workspace, "land_id", j.a.ID, "error", err)
	}
	j.release()
}

// enterSwap is asked by the engine immediately before it moves the target
// of the attempt id: an attempt whose context ended moves nothing, and
// from then on the deadline no longer ends it, since only the engine knows
// whether the target moved.
func (c *Component) enterSwap(ctx context.Context, id string) error {
	p := &c.pool
	p.mu.Lock()
	j := p.jobs[id]
	p.mu.Unlock()
	if j != nil {
		j.mu.Lock()
		defer j.mu.Unlock()
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if j != nil {
		j.swapping = true
	}
	return nil
}

// leaveSwap is told by the engine that its move of the target of the
// attempt id moved nothing, so that the deadline ends it again.
func (c *Component) leaveSwap(id string) {
	p := &c.pool
	p.mu.Lock()
	j := p.jobs[id]
	p.mu.Unlock()
	if j != nil {
		j.mu.Lock()
		j.swapping = false
		j.mu.Unlock()
	}
}
