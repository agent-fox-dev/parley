// Package maintenance runs the hub's maintenance tasks: one pass at once and
// one at each tick of an hourly timer, the tasks of a pass one after the
// other in a fixed order, a failing task logged without stopping the others.
//
// The packages that own a cleanup register it under its name; this package
// decides only when and in which order the registered tasks run.
package maintenance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/agent-fox-dev/parley/internal/scrub"
)

// Interval is the time between two passes. It is fixed: no configuration key
// changes it.
const Interval = time.Hour

// order is the order in which a pass runs the tasks registered under these
// names. Tasks registered under any other name run after them, in the order
// they were registered, so that a cleanup this list does not know still has
// a place.
var order = []string{
	"retention",
	"secret_reencryption",
	"claim_cleanup",
	"forge_delivery_pruning",
	"token_binding_sweep",
	"patch_purge",
	"fork_expiry",
	"sandbox_record_cleanup",
}

// Task is one maintenance task. Its context is cancelled when the hub stops;
// a task that then ends with an error is not counted as failed.
type Task func(context.Context) error

type entry struct {
	name string
	fn   Task
}

// Registry holds the registered tasks. It is filled from one goroutine before
// a Loop is built from it.
type Registry struct {
	entries []entry
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

// Register adds the task fn under name. It refuses an empty name, a nil
// function and a name registered before, with an error that names it; the
// task registered first is kept.
func (r *Registry) Register(name string, fn Task) error {
	switch {
	case name == "":
		return errors.New("a maintenance task has no name")
	case fn == nil:
		return fmt.Errorf("maintenance task %q has no function", name)
	}
	for _, e := range r.entries {
		if e.name == name {
			return fmt.Errorf("maintenance task %q is registered twice", name)
		}
	}
	r.entries = append(r.entries, entry{name, fn})
	return nil
}

// ordered returns the registered tasks in the order a pass runs them.
func (r *Registry) ordered() []entry {
	tasks := make([]entry, 0, len(r.entries))
	for _, name := range order {
		if i := slices.IndexFunc(r.entries, func(e entry) bool { return e.name == name }); i >= 0 {
			tasks = append(tasks, r.entries[i])
		}
	}
	for _, e := range r.entries {
		if !slices.Contains(order, e.name) {
			tasks = append(tasks, e)
		}
	}
	return tasks
}

// Loop runs the passes over the tasks of a registry.
type Loop struct {
	tasks    []entry
	logger   *slog.Logger
	scrubber *scrub.Scrubber
}

// New returns a loop over the tasks registered in reg so far, which logs a
// failing task to logger. The error text of a failure is scrubbed before it
// is logged, also when the logger does not scrub.
func New(reg *Registry, logger *slog.Logger) *Loop {
	return &Loop{tasks: reg.ordered(), logger: logger, scrubber: scrub.New()}
}

// Run runs one pass at once and then one at each tick received from ticks,
// until ctx is cancelled. A tick that arrives while a pass runs is received
// and dropped, so the sender never waits and the pass that is running is not
// followed by another when it ends. Once ctx is cancelled, the running task's
// context is done, no further task or pass starts, and Run returns when the
// running task has returned. The hub passes the channel of a ticker with
// Interval; tests pass a channel of their own.
func (l *Loop) Run(ctx context.Context, ticks <-chan time.Time) {
	done := l.start(ctx)
	for {
		select {
		case <-ctx.Done():
			if done != nil {
				<-done
			}
			return
		case <-done:
			done = nil
		case <-ticks:
			if done == nil && ctx.Err() == nil {
				done = l.start(ctx)
			}
		}
	}
}

// start runs a pass in a goroutine of its own and returns a channel that is
// closed when it has ended.
func (l *Loop) start(ctx context.Context) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Pass(ctx)
	}()
	return done
}

// Pass runs the registered tasks once, one after the other, in the fixed
// order of the known names and then the others in registration order. A task
// that returns an error or panics is logged at error level with its name in
// task and the scrubbed error in error, and the next task runs. Once ctx is
// cancelled no further task starts.
func (l *Loop) Pass(ctx context.Context) {
	for _, t := range l.tasks {
		if ctx.Err() != nil {
			return
		}
		panicked, err := call(ctx, t.fn)
		if err == nil || (ctx.Err() != nil && !panicked) {
			continue
		}
		l.logger.Error("maintenance task failed", "task", t.name, "error", l.scrubber.Scrub(err.Error()))
	}
}

// call runs fn and turns a panic into an error, so that a task that panics
// fails like one that returns an error.
func call(ctx context.Context, fn Task) (panicked bool, err error) {
	defer func() {
		if p := recover(); p != nil {
			panicked, err = true, fmt.Errorf("panic: %v", p)
		}
	}()
	return false, fn(ctx)
}
