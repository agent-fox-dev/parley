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
	Trial    func(ctx context.Context, repo *gitrepo.Repo, onto, head string) (*gitrepo.ConflictReport, error)
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
