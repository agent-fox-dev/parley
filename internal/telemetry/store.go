package telemetry

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// DB is the analytical store as the package reaches it: a transaction for
// a write, and a read outside one, of one row or of a list of records. SQL
// adapts the store's *sql.DB; a test puts its own in Deps to make a write
// fail or block, or to watch the reads.
type DB interface {
	BeginTx(ctx context.Context) (Tx, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Tx is a write transaction of DB; *sql.Tx satisfies it.
type Tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	Commit() error
	Rollback() error
}

// SQL is db as a DB. A transaction it begins under a context with a
// deadline waits for the store's write lock until that deadline at most.
// Beginning one takes the lock, and while another connection holds it
// SQLite waits in its busy handler, up to the connection's busy timeout,
// without looking at the context: so the transaction gets a connection of
// its own whose busy timeout is cut to the time left, for as long as the
// transaction lasts, and the connection goes back to the pool with the
// busy timeout it had.
func SQL(db *sql.DB) DB { return sqlDB{db} }

type sqlDB struct{ db *sql.DB }

func (s sqlDB) BeginTx(ctx context.Context) (Tx, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return s.db.BeginTx(ctx, nil)
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	var was int64
	if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&was); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	// Rounded up, so that a wait the busy timeout ends ends at the deadline
	// or after it, and is then told from other failures as one the limit
	// ended.
	left := int64((time.Until(deadline) + time.Millisecond - 1) / time.Millisecond)
	t := &connTx{conn: conn}
	if left < was {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout = %d`, max(left, 1))); err != nil {
			return nil, errors.Join(err, discard(conn))
		}
		t.restore = fmt.Sprintf(`PRAGMA busy_timeout = %d`, was)
	}
	if t.Tx, err = conn.BeginTx(ctx, nil); err != nil {
		return nil, errors.Join(err, t.release())
	}
	return t, nil
}

// connTx is a transaction of SQL on a connection of its own, which it gives
// back to the pool when it ends, with the busy timeout restore sets, when
// it is not empty.
type connTx struct {
	*sql.Tx
	conn    *sql.Conn
	restore string
}

func (t *connTx) Commit() error   { return errors.Join(t.Tx.Commit(), t.release()) }
func (t *connTx) Rollback() error { return errors.Join(t.Tx.Rollback(), t.release()) }

// release gives the connection back to the pool once; a connection whose
// busy timeout cannot be restored is discarded instead, so that no other
// user of the pool waits for a lock less than it expects.
func (t *connTx) release() error {
	conn := t.conn
	if conn == nil {
		return nil
	}
	t.conn = nil
	if t.restore != "" {
		if _, err := conn.ExecContext(context.Background(), t.restore); err != nil {
			return errors.Join(err, discard(conn))
		}
	}
	return conn.Close()
}

// discard closes conn without giving it back to the pool.
func discard(conn *sql.Conn) error {
	err := conn.Raw(func(any) error { return driver.ErrBadConn })
	if errors.Is(err, driver.ErrBadConn) {
		return nil
	}
	return err
}

func (s sqlDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, query, args...)
}

func (s sqlDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, query, args...)
}

// errWriteTimeout is a write that did not finish within the write limit:
// the caller is told to send it again later.
var errWriteTimeout = errors.New("the write did not finish within [audit].write_timeout")

// write runs fn in one transaction of the analytical store and commits
// what it did, or rolls all of it back when fn or the commit fails, so that
// a failed write stores nothing. The transaction, and every statement fn
// runs with the context it is given, end at the write limit, so that an
// agent never waits on a slow store for its telemetry; a write the limit
// ends fails with errWriteTimeout. That is decided by the limit's context
// and not by the error, since the store's driver reports a statement the
// context ends as interrupted, without the context's error.
func (c *Component) write(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, c.deps.WriteTimeout)
	defer cancel()
	err := c.writeTx(ctx, fn)
	if err != nil && ctx.Err() != nil {
		return errors.Join(errWriteTimeout, err)
	}
	return err
}

// writeTx is write without the limit.
func (c *Component) writeTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error {
	tx, err := c.deps.DB.BeginTx(ctx)
	if err != nil {
		return err
	}
	if err := fn(ctx, tx); err != nil {
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
// two kinds: its name, for messages, its table, the columns it holds after
// the head, the destinations a record of the kind scans those columns
// into, in their order, and the insert of a new record.
type kind[R record] struct {
	name    string
	table   string
	columns string
	dest    func(rec *R) (h *head, dest []any)
	insert  func(ctx context.Context, tx Tx, rec R) error
}

// The kinds of single records.
var (
	runEvents = kind[runEvent]{name: "run event", table: "run_events",
		columns: "event_type, severity, node_id, session_id, payload",
		dest: func(e *runEvent) (*head, []any) {
			return &e.head, []any{&e.EventType, &e.Severity, &e.NodeID, &e.SessionID, jsonColumn{&e.Payload}}
		},
		insert: insertRunEvent}
	outcomes = kind[outcome]{name: "session outcome", table: "session_outcomes",
		columns: "session_id, status, node_id, duration_ms, token_usage",
		dest: func(o *outcome) (*head, []any) {
			return &o.head, []any{&o.SessionID, &o.Status, &o.NodeID, &o.DurationMS, jsonColumn{&o.TokenUsage}}
		},
		insert: insertOutcome}
	toolCalls = kind[toolCall]{name: "tool call", table: "tool_calls",
		columns: "tool_name, node_id, session_id, duration_ms, input, output",
		dest: func(tc *toolCall) (*head, []any) {
			return &tc.head, []any{&tc.ToolName, &tc.NodeID, &tc.SessionID, &tc.DurationMS, jsonColumn{&tc.Input}, jsonColumn{&tc.Output}}
		},
		insert: insertToolCall}
	toolErrors = kind[toolError]{name: "tool error", table: "tool_errors",
		columns: "tool_name, error_msg, error_code, node_id, session_id",
		dest: func(te *toolError) (*head, []any) {
			return &te.head, []any{&te.ToolName, &te.ErrorMsg, &te.ErrorCode, &te.NodeID, &te.SessionID}
		},
		insert: insertToolError}
	traces = kind[trace]{name: "trace record", table: "traces",
		columns: "event_type, node_id, session_id, sequence, data",
		dest: func(tr *trace) (*head, []any) {
			return &tr.head, []any{&tr.EventType, &tr.NodeID, &tr.SessionID, &tr.Sequence, jsonColumn{&tr.Data}}
		},
		insert: insertTrace}
)

// get reads the record id of the kind.
func (k kind[R]) get(ctx context.Context, tx Tx, id string) (rec R, found bool, err error) {
	h, dest := k.dest(&rec)
	found, err = getRecord(ctx, tx, k.table, k.columns, id, h, dest...)
	return rec, found, err
}

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
	return getRecordWhere(ctx, tx, table, columns, "id = ?", []any{id}, h, dest...)
}

// getRecordWhere is getRecord for the record of table that where, with the
// arguments key, selects.
func getRecordWhere(ctx context.Context, tx Tx, table, columns, where string, key []any, h *head, dest ...any) (found bool, err error) {
	err = scanRecord(tx.QueryRowContext(ctx, selectRecords(table, columns)+` WHERE `+where, key...).Scan, table, h, dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// selectRecords is the SELECT of the head and the columns named by columns
// of the records of table, which scanRecord scans.
func selectRecords(table, columns string) string {
	return `SELECT id, workspace, run_id, timestamp, ingested_at, ` + columns + ` FROM ` + table
}

// scanRecord scans a row of selectRecords from table with scan: its head
// into h and the columns after it into dest, in their order.
func scanRecord(scan func(dest ...any) error, table string, h *head, dest ...any) error {
	var ingested string
	if err := scan(append([]any{&h.ID, &h.Workspace, &h.RunID, timeColumn{&h.Timestamp}, &ingested}, dest...)...); err != nil {
		return err
	}
	at, err := time.Parse(time.RFC3339, ingested)
	if err != nil {
		return fmt.Errorf("%s %s: ingested_at %q: %w", table, h.ID, ingested, err)
	}
	h.IngestedAt = timefmt.Time(at)
	return nil
}

// storePostmortemOnce stores p in tx unless the workspace and run of p have
// a postmortem, and returns the postmortem they have: p when it is new,
// which created reports, and otherwise the stored one, whatever p holds.
// Like storeOnce, the read and the insert share tx, whose write lock makes
// one of concurrent first postmortems of a run the one stored.
func storePostmortemOnce(ctx context.Context, tx Tx, p postmortem) (stored postmortem, created bool, err error) {
	stored, found, err := getPostmortem(ctx, tx, p.Workspace, p.RunID)
	if err != nil || found {
		return stored, false, err
	}
	if err := insertPostmortem(ctx, tx, p); err != nil {
		return stored, false, err
	}
	return p, true, nil
}

// getPostmortem reads the postmortem of the run runID of workspace.
func getPostmortem(ctx context.Context, tx Tx, workspace, runID string) (p postmortem, found bool, err error) {
	found, err = getRecordWhere(ctx, tx, "postmortems",
		"schema_version, run_status, started_at, completed_at, task_summary, cost_summary, blocked_tasks, session_history",
		"workspace = ? AND run_id = ?", []any{workspace, runID}, &p.head,
		&p.SchemaVersion, &p.RunStatus, timeColumn{&p.StartedAt}, timeColumn{&p.CompletedAt},
		jsonColumn{&p.TaskSummary}, jsonColumn{&p.CostSummary}, jsonColumn{&p.BlockedTasks}, jsonColumn{&p.SessionHistory})
	return p, found, err
}

// insertPostmortem stores the postmortem p.
func insertPostmortem(ctx context.Context, tx Tx, p postmortem) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO postmortems (id, workspace, run_id, timestamp, ingested_at, schema_version, run_status, started_at, completed_at,
	task_summary, cost_summary, blocked_tasks, session_history)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Workspace, p.RunID, p.Timestamp.stored(), timefmt.Format(time.Time(p.IngestedAt)), p.SchemaVersion, p.RunStatus,
		p.StartedAt.stored(), p.CompletedAt.stored(), string(p.TaskSummary), string(p.CostSummary), string(p.BlockedTasks),
		string(p.SessionHistory))
	return err
}

// timeColumn scans a client timestamp stored in the form of storedLayout.
type timeColumn struct{ dst *clientTime }

// Scan implements sql.Scanner.
func (c timeColumn) Scan(src any) error {
	var s string
	switch v := src.(type) {
	case string:
		s = v
	case []byte:
		s = string(v)
	default:
		return fmt.Errorf("a timestamp column holds a %T", src)
	}
	t, err := time.Parse(storedLayout, s)
	if err != nil {
		return fmt.Errorf("timestamp %q: %w", s, err)
	}
	*c.dst = clientTime(t)
	return nil
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
