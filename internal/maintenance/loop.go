// Package maintenance runs the hub's hourly maintenance loop: one pass at
// start and one every hour, each running the registered tasks one after the
// other in a fixed order. It is the guaranteed backstop for retention and
// cleanup; a feature may also act on a finer timer of its own.
package maintenance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/scrub"
)

// Interval is the time between passes. It is not configurable.
const Interval = time.Hour

// Order is the fixed order of the known tasks in a pass; tasks registered
// under other names run after them, in registration order.
var Order = []string{
	"retention",
	"secret_reencryption",
	"claim_cleanup",
	"forge_delivery_pruning",
	"token_binding_sweep",
	"patch_purge",
	"fork_expiry",
	"sandbox_record_cleanup",
}

// Func is a maintenance task. It should return when ctx is cancelled.
type Func func(ctx context.Context) error

// Registry holds the registered tasks.
type Registry struct {
	mu    sync.Mutex
	tasks map[string]Func
	order []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{tasks: map[string]Func{}} }

// Register adds a task under a name. An empty name and a name registered
// before are refused with an error naming it, and the first task is kept.
func (r *Registry) Register(name string, fn Func) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if name == "" {
		return errors.New("a maintenance task has an empty name \"\"")
	}
	if _, dup := r.tasks[name]; dup {
		return fmt.Errorf("maintenance task %s is registered twice", name)
	}
	r.tasks[name] = fn
	r.order = append(r.order, name)
	return nil
}

type namedTask struct {
	name string
	fn   Func
}

// pass returns the tasks of one pass in their order.
func (r *Registry) pass() []namedTask {
	r.mu.Lock()
	defer r.mu.Unlock()
	known := map[string]bool{}
	var out []namedTask
	for _, name := range Order {
		known[name] = true
		if fn, ok := r.tasks[name]; ok {
			out = append(out, namedTask{name, fn})
		}
	}
	for _, name := range r.order {
		if !known[name] {
			out = append(out, namedTask{name, r.tasks[name]})
		}
	}
	return out
}

// Loop runs passes over a registry.
type Loop struct {
	reg      *Registry
	logger   *slog.Logger
	scrubber *scrub.Scrubber
}

// New builds a loop that logs failures to logger. Error texts pass through
// a scrubber of their own, so that a URL's user information or a hub
// credential never reaches the log even through a logger that does not
// scrub.
func New(reg *Registry, logger *slog.Logger) *Loop {
	return &Loop{reg: reg, logger: logger, scrubber: scrub.New()}
}

// Run runs one pass at once and one at each tick, until ctx is cancelled.
// A tick that arrives while a pass runs is dropped. When ctx is cancelled,
// the running task's context is done, no further task or pass starts, and
// Run returns once the running task has returned. The hub passes a ticker of
// Interval; tests pass a channel.
func (l *Loop) Run(ctx context.Context, ticks <-chan time.Time) {
	done := make(chan struct{}, 1)
	var wg sync.WaitGroup
	busy := false
	start := func() {
		busy = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.pass(ctx)
			done <- struct{}{}
		}()
	}
	start()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-done:
			busy = false
		case <-ticks:
			if !busy {
				start()
			}
		}
	}
}

// pass runs the registered tasks one after the other; a failing task is
// logged and the pass goes on.
func (l *Loop) pass(ctx context.Context) {
	for _, t := range l.reg.pass() {
		if ctx.Err() != nil {
			return
		}
		err := runTask(ctx, t.fn)
		if err != nil && ctx.Err() == nil {
			l.logger.Error("maintenance task failed", "task", t.name, "error", l.scrubber.Scrub(err.Error()))
		}
	}
}

// runTask runs a task and turns a panic into an error.
func runTask(ctx context.Context, fn Func) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return fn(ctx)
}
