package telemetry

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// unfiredContext is a context whose deadline has a time but which is not
// cancelled when it passes: the state of a context whose deadline timer has
// not run yet on a busy machine.
type unfiredContext struct {
	context.Context
	deadline time.Time
}

func (c unfiredContext) Deadline() (time.Time, bool) { return c.deadline, true }

// lockedDB is a store whose write lock another writer holds: beginning a
// transaction waits until the context's deadline has passed and then fails
// busy, as SQLite does once a busy timeout cut to the deadline runs out.
type lockedDB struct{}

func (lockedDB) BeginTx(ctx context.Context) (Tx, error) {
	deadline, _ := ctx.Deadline()
	time.Sleep(time.Until(deadline) + time.Millisecond)
	return nil, errors.New("database is locked (5) (SQLITE_BUSY)")
}

func (lockedDB) QueryRowContext(context.Context, string, ...any) *sql.Row { return nil }

func (lockedDB) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, errors.New("no reads")
}

// TestWrite_ALockWaitThatEndsAtTheLimitIsATimeoutBeforeTheContextIsCancelled
// checks that a write failing once the limit has passed is a write the limit
// ended, even while the limit's context is not yet cancelled.
func TestWrite_ALockWaitThatEndsAtTheLimitIsATimeoutBeforeTheContextIsCancelled(t *testing.T) {
	t.Parallel()
	var c Component
	c.Bind(Deps{DB: lockedDB{}, WriteTimeout: time.Minute})
	ctx := unfiredContext{Context: context.Background(), deadline: time.Now().Add(20 * time.Millisecond)}

	err := c.write(ctx, func(context.Context, Tx) error { return nil })
	if !errors.Is(err, errWriteTimeout) {
		t.Errorf("write = %v, want errWriteTimeout", err)
	}
}
