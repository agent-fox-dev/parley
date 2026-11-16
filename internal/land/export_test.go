package land

import (
	"context"
	"database/sql"
	"sync/atomic"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/store"
)

// ErrFinal is the error FinishAttempt returns for an attempt that is not
// running.
var ErrFinal = errFinal

// InsertAttempt stores a in a transaction of its own, as the creation of
// an attempt does.
func InsertAttempt(ctx context.Context, s *store.Store, a Attempt) error {
	return s.Tx(ctx, func(tx *sql.Tx) error { return insertAttempt(ctx, tx, a) })
}

// FinishAttempt finishes a in a transaction of its own.
func FinishAttempt(ctx context.Context, s *store.Store, a Attempt) error {
	return s.Tx(ctx, func(tx *sql.Tx) error { return finishAttempt(ctx, tx, a) })
}

// ReadAttempt reads the attempt id of the workspace from the store.
func ReadAttempt(ctx context.Context, s *store.Store, workspace, id string) (Attempt, bool, error) {
	return readAttempt(ctx, s.Main(), workspace, id)
}

// RunAttempt runs the running attempt id of the workspace to its end, in
// the caller's goroutine.
func RunAttempt(ctx context.Context, c *Component, workspace, id string) error {
	a, found, err := readAttempt(ctx, c.deps.Store.Main(), workspace, id)
	if err != nil {
		return err
	}
	if !found {
		return errFinal
	}
	return c.run(ctx, a)
}

// Seams are the points a test steers the engine at: BeforeSwap runs
// between the replay and each compare-and-swap of the target, with the
// number of the try; Fault is asked at each point of the atomic step
// (before-journal, before-swap, after-swap, in-bookkeeping,
// after-bookkeeping), and an error stops the engine there as a stop of
// the process would, writing nothing more. SourceChecked runs once a
// landed change's source branch was found needed by no other change, before
// it is deleted. RevertBranched runs once a revert's checks passed and its
// branch was created, before the reverting change is opened.
type Seams struct {
	BeforeSwap     func(ctx context.Context, id string, try int)
	Fault          func(point string) error
	SourceChecked  func()
	RevertBranched func()
}

// SetSeams sets the seams of c, before it runs an attempt.
func SetSeams(c *Component, s Seams) {
	c.seams = seams{beforeSwap: s.BeforeSwap, fault: s.Fault, sourceChecked: s.SourceChecked, revertBranched: s.RevertBranched}
}

// Journalled counts the entries of the land journal in s.
func Journalled(ctx context.Context, s *store.Store) (n int, err error) {
	err = s.Main().QueryRowContext(ctx, `SELECT COUNT(*) FROM land_journal`).Scan(&n)
	return n, err
}

// CompleteConflict completes, as a land of the workspace's one change
// whose version went from base to head does, the report of its replay
// that stopped on onto, and returns it.
func CompleteConflict(ctx context.Context, c *Component, workspace, onto, head, base string, report *gitrepo.ConflictReport) *gitrepo.ConflictReport {
	return c.conflict(ctx, Attempt{Workspace: workspace}, CoveredChange{Head: head, Base: base}, onto, report)
}

// Submit hands the running attempt a to the pool of c, its deadline
// counted from its creation.
func Submit(c *Component, a Attempt) { c.submit(a, a.CreatedAt) }

// Ended is closed once the pool of c is done with the attempt id.
func Ended(c *Component, id string) <-chan struct{} { return c.ended(id) }

// CountReplays makes the conflict reads of c count the trial replays they
// run, and returns what reads the count.
func CountReplays(c *Component) func() int64 {
	var n atomic.Int64
	c.setTrial(func(ctx context.Context, repo *gitrepo.Repo, base, head, onto string) (*gitrepo.ConflictReport, error) {
		n.Add(1)
		return change.TrialRebase(ctx, repo, base, head, onto)
	})
	return n.Load
}

// RegisteredGate is the policy gate registered on c, nil for none.
func (c *Component) RegisteredGate() Gate {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.gate
}
