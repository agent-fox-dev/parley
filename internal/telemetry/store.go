package telemetry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// kind is how the records of one kind are stored, each kind in a table of
// its own keyed by the id in lower case, so that one id may name records of
// two kinds: its name, for messages, the insert of a new record and the
// read of the stored one by its id.
type kind[R record] struct {
	name   string
	insert func(ctx context.Context, tx Tx, rec R) error
	get    func(ctx context.Context, tx Tx, id string) (rec R, found bool, err error)
}

// The kinds of single records.
var (
	runEvents  = kind[runEvent]{name: "run event", insert: insertRunEvent, get: getRunEvent}
	outcomes   = kind[outcome]{name: "session outcome", insert: insertOutcome, get: getOutcome}
	toolCalls  = kind[toolCall]{name: "tool call", insert: insertToolCall, get: getToolCall}
	toolErrors = kind[toolError]{name: "tool error", insert: insertToolError, get: getToolError}
	traces     = kind[trace]{name: "trace record", insert: insertTrace, get: getTrace}
)

// storeOnce stores rec in tx unless its id names a stored record of the
// kind k, and returns the record the id names: rec when it is new, which
// created reports, and otherwise the stored record, whatever rec holds. The
// read and the insert share tx, and a transaction of the analytical store
// takes its write lock when it begins, so that of concurrent requests with
// one new id exactly one inserts it and the others read what it stored.
func storeOnce[R record](ctx context.Context, tx Tx, k kind[R], rec R) (stored R, created bool, err error) {
	stored, found, err := k.get(ctx, tx, rec.common().ID)
	if err != nil || found {
		return stored, false, err
	}
	if err := k.insert(ctx, tx, rec); err != nil {
		return stored, false, err
	}
	return rec, true, nil
}

// getRecord reads the record id of table: its head into h and the columns
// named by columns into dest, in their order. found is false when table
// holds no record id.
func getRecord(ctx context.Context, tx Tx, table, columns, id string, h *head, dest ...any) (found bool, err error) {
	var ts, ingested string
	err = tx.QueryRowContext(ctx, `SELECT id, workspace, run_id, timestamp, ingested_at, `+columns+` FROM `+table+` WHERE id = ?`, id).
		Scan(append([]any{&h.ID, &h.Workspace, &h.RunID, &ts, &ingested}, dest...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	t, err := time.Parse(storedLayout, ts)
	if err != nil {
		return false, fmt.Errorf("%s %s: timestamp %q: %w", table, id, ts, err)
	}
	at, err := time.Parse(time.RFC3339, ingested)
	if err != nil {
		return false, fmt.Errorf("%s %s: ingested_at %q: %w", table, id, ingested, err)
	}
	h.Timestamp, h.IngestedAt = clientTime(t), timefmt.Time(at)
	return true, nil
}

// getRunEvent reads the run event id.
func getRunEvent(ctx context.Context, tx Tx, id string) (e runEvent, found bool, err error) {
	found, err = getRecord(ctx, tx, "run_events", "event_type, severity, node_id, session_id, payload", id, &e.head,
		&e.EventType, &e.Severity, &e.NodeID, &e.SessionID, jsonColumn{&e.Payload})
	return e, found, err
}

// getOutcome reads the session outcome id.
func getOutcome(ctx context.Context, tx Tx, id string) (o outcome, found bool, err error) {
	found, err = getRecord(ctx, tx, "session_outcomes", "session_id, status, node_id, duration_ms, token_usage", id, &o.head,
		&o.SessionID, &o.Status, &o.NodeID, &o.DurationMS, jsonColumn{&o.TokenUsage})
	return o, found, err
}

// getToolCall reads the tool call id.
func getToolCall(ctx context.Context, tx Tx, id string) (tc toolCall, found bool, err error) {
	found, err = getRecord(ctx, tx, "tool_calls", "tool_name, node_id, session_id, duration_ms, input, output", id, &tc.head,
		&tc.ToolName, &tc.NodeID, &tc.SessionID, &tc.DurationMS, jsonColumn{&tc.Input}, jsonColumn{&tc.Output})
	return tc, found, err
}

// getToolError reads the tool error id.
func getToolError(ctx context.Context, tx Tx, id string) (te toolError, found bool, err error) {
	found, err = getRecord(ctx, tx, "tool_errors", "tool_name, error_msg, error_code, node_id, session_id", id, &te.head,
		&te.ToolName, &te.ErrorMsg, &te.ErrorCode, &te.NodeID, &te.SessionID)
	return te, found, err
}

// getTrace reads the trace record id.
func getTrace(ctx context.Context, tx Tx, id string) (tr trace, found bool, err error) {
	found, err = getRecord(ctx, tx, "traces", "event_type, node_id, session_id, sequence, data", id, &tr.head,
		&tr.EventType, &tr.NodeID, &tr.SessionID, &tr.Sequence, jsonColumn{&tr.Data})
	return tr, found, err
}

// jsonColumn scans a column of JSON text into the raw JSON it holds, nil
// for NULL, so that a stored member is answered as it was sent.
type jsonColumn struct{ dst *json.RawMessage }

// Scan implements sql.Scanner.
func (j jsonColumn) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*j.dst = nil
	case string:
		*j.dst = json.RawMessage(v)
	case []byte:
		*j.dst = append(json.RawMessage(nil), v...)
	default:
		return fmt.Errorf("a JSON column holds a %T", src)
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
