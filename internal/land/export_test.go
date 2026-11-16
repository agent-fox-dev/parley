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
