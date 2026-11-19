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

// insertOutcome stores the session outcome o.
func insertOutcome(ctx context.Context, tx Tx, o outcome) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO session_outcomes (id, workspace, run_id, timestamp, ingested_at, session_id, status, node_id, duration_ms, token_usage)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.ID, o.Workspace, o.RunID, o.Timestamp.stored(), timefmt.Format(time.Time(o.IngestedAt)), o.SessionID, o.Status,
		o.NodeID, o.DurationMS, nullableJSON(o.TokenUsage))
	return err
}

// insertToolCall stores the tool call tc.
func insertToolCall(ctx context.Context, tx Tx, tc toolCall) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO tool_calls (id, workspace, run_id, timestamp, ingested_at, tool_name, node_id, session_id, duration_ms, input, output)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tc.ID, tc.Workspace, tc.RunID, tc.Timestamp.stored(), timefmt.Format(time.Time(tc.IngestedAt)), tc.ToolName,
		tc.NodeID, tc.SessionID, tc.DurationMS, nullableJSON(tc.Input), nullableJSON(tc.Output))
	return err
}

// insertToolError stores the tool error te.
func insertToolError(ctx context.Context, tx Tx, te toolError) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO tool_errors (id, workspace, run_id, timestamp, ingested_at, tool_name, error_msg, error_code, node_id, session_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		te.ID, te.Workspace, te.RunID, te.Timestamp.stored(), timefmt.Format(time.Time(te.IngestedAt)), te.ToolName,
		te.ErrorMsg, te.ErrorCode, te.NodeID, te.SessionID)
	return err
}

// insertTrace stores the trace record tr.
func insertTrace(ctx context.Context, tx Tx, tr trace) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO traces (id, workspace, run_id, timestamp, ingested_at, event_type, node_id, session_id, sequence, data)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tr.ID, tr.Workspace, tr.RunID, tr.Timestamp.stored(), timefmt.Format(time.Time(tr.IngestedAt)), tr.EventType,
		tr.NodeID, tr.SessionID, tr.Sequence, nullableJSON(tr.Data))
	return err
}

// nullableJSON is the JSON text raw as a column value: NULL when raw is nil.
func nullableJSON(raw []byte) any {
	if raw == nil {
		return nil
	}
	return string(raw)
}
