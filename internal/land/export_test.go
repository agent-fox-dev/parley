package land

import (
	"context"
	"database/sql"

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
// the process would, writing nothing more.
type Seams struct {
	BeforeSwap func(ctx context.Context, id string, try int)
	Fault      func(point string) error
}

// SetSeams sets the seams of c, before it runs an attempt.
func SetSeams(c *Component, s Seams) {
	c.seams = seams{beforeSwap: s.BeforeSwap, fault: s.Fault}
}

// Journalled counts the entries of the land journal in s.
func Journalled(ctx context.Context, s *store.Store) (n int, err error) {
	err = s.Main().QueryRowContext(ctx, `SELECT COUNT(*) FROM land_journal`).Scan(&n)
	return n, err
}
