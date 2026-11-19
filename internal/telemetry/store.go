package telemetry

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// DB is the analytical store as the package's writes reach it: a
// transaction, and a read outside one. SQL adapts the store's *sql.DB; a
// test puts its own in Deps to make a write fail or block.
type DB interface {
	BeginTx(ctx context.Context) (Tx, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Tx is a write transaction of DB; *sql.Tx satisfies it.
type Tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	Commit() error
	Rollback() error
}

// SQL is db as a DB.
func SQL(db *sql.DB) DB { return sqlDB{db} }

type sqlDB struct{ db *sql.DB }

func (s sqlDB) BeginTx(ctx context.Context) (Tx, error) { return s.db.BeginTx(ctx, nil) }

func (s sqlDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, query, args...)
}

// write runs fn in one transaction of the analytical store and commits
// what it did, or rolls all of it back when fn or the commit fails, so that
// a failed write stores nothing.
func (c *Component) write(ctx context.Context, fn func(tx Tx) error) error {
	tx, err := c.deps.DB.BeginTx(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return errors.Join(err, rollback(tx))
	}
	if err := tx.Commit(); err != nil {
		return errors.Join(err, rollback(tx))
	}
	return nil
}

// rollback rolls tx back; a transaction that has already ended is no
// failure.
func rollback(tx Tx) error {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return err
	}
	return nil
}

// insertRunEvent stores the run event e.
func insertRunEvent(ctx context.Context, tx Tx, e runEvent) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO run_events (id, workspace, run_id, timestamp, ingested_at, event_type, severity, node_id, session_id, payload)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.Workspace, e.RunID, e.Timestamp.stored(), timefmt.Format(time.Time(e.IngestedAt)), e.EventType, e.Severity,
		e.NodeID, e.SessionID, nullableJSON(e.Payload))
	return err
}

// nullableJSON is the JSON text raw as a column value: NULL when raw is nil.
func nullableJSON(raw []byte) any {
	if raw == nil {
		return nil
	}
	return string(raw)
}
