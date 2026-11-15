package change

import (
	"context"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// ReadVersions is the read of a change's versions, which the tests use
// before the routes that serve them.
var ReadVersions = readVersions

// PatchID is the patch id the component stores for a version from base to
// head in repo.
func PatchID(ctx context.Context, repo *gitrepo.Repo, base, head string) (string, error) {
	changes, err := repo.DiffChanges(ctx, base, head)
	if err != nil {
		return "", err
	}
	files, err := keyed(ctx, repo, base, head, changes)
	return patchID(files), err
}

// MergeableSeams replace what a mergeability check runs with: the trial
// rebase, its deadline, the worker's clock and a gate every check passes
// as it starts; nil and zero keep the hub's.
type MergeableSeams struct {
	Trial    func(ctx context.Context, repo *gitrepo.Repo, base, head, onto string) (*gitrepo.ConflictReport, error)
	Deadline time.Duration
	Clock    interface {
		Now() time.Time
		After(d time.Duration) <-chan time.Time
	}
	Gate func(ctx context.Context, workspace string, number int64)
}

// SetMergeableSeams makes s the seams of c's mergeability checks.
func SetMergeableSeams(c *Component, s MergeableSeams) {
	w := &c.merge
	w.mu.Lock()
	defer w.mu.Unlock()
	w.trial, w.deadline, w.clock, w.gate = s.Trial, s.Deadline, s.Clock, s.Gate
	w.signal()
}

// MergeableIdle reports whether no check of the change is waiting or
// running.
func MergeableIdle(c *Component, workspace string, number int64) bool {
	w := &c.merge
	w.mu.Lock()
	defer w.mu.Unlock()
	k := mergeKey{workspace, number}
	return !w.queued[k] && !w.running[k]
}

// MergeableQuiet reports whether no check of any change is waiting or
// running.
func MergeableQuiet(c *Component) bool {
	w := &c.merge
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.queued) == 0 && len(w.running) == 0
}

// RestackSeams replace what a restack runs with: the replay of a change's
// commits after its base onto the commit it sits on, and a hook called
// between the replay and the swap of the source branch with the attempt's
// number, from 1; nil keeps the hub's.
type RestackSeams struct {
	Rebase     func(ctx context.Context, repo *gitrepo.Repo, base, head, onto string, at time.Time) (string, *gitrepo.ConflictReport, error)
	BeforeSwap func(ctx context.Context, workspace string, number int64, attempt int)
}

// SetRestackSeams makes s the seams of c's restacks.
func SetRestackSeams(c *Component, s RestackSeams) {
	w := &c.restacks
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rebase, w.beforeSwap = s.Rebase, s.BeforeSwap
}

// HoldRestacks keeps c's restack worker from running any pending restack
// from its return on, so that a test runs them with RestackPass.
func HoldRestacks(c *Component) {
	w := &c.restacks
	for {
		w.mu.Lock()
		w.held = true
		running := w.running
		w.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// RestackPass runs, in the caller's goroutine, the pending restacks of c
// until none is left that has not failed, and returns how many ran.
func RestackPass(ctx context.Context, c *Component) int { return c.restackPass(ctx, false) }

// RestacksQuiet reports whether c's restack worker has nothing to run and
// is not running, or is held.
func RestacksQuiet(c *Component) bool {
	w := &c.restacks
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.held || (!w.signaled && !w.running)
}
