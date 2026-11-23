package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/store"
)

// position is a record's place in the order of a per-run list: its
// timestamp in the form of storedLayout, then its id. Both are unique
// together, so the order is total and a page continues after the last
// record of the previous one.
type position struct {
	Timestamp string `json:"t"`
	ID        string `json:"id"`
}

// positionOf is the position of the record whose head is h.
func positionOf(h head) position {
	return position{Timestamp: h.Timestamp.stored(), ID: h.ID}
}

// match is an exact-match filter: the column and the value it must hold.
type match struct {
	column, value string
}

// listQuery is a read of the records of one kind of one run of one
// workspace: those that hold every value of matches, whose timestamp is at
// or after since and before until, when given, after the position after,
// when given, in the order (timestamp, id), descending when desc, at most
// limit of them.
type listQuery struct {
	workspace, runID string
	matches          []match
	since, until     *time.Time
	desc             bool
	after            *position
	limit            int
}

// sql is the query q of the columns named by columns of table and its
// arguments. Every query is bound to the workspace and the run. Timestamps
// are compared in the form they are stored in, whose text sorts as the
// instants do.
func (q listQuery) sql(table, columns string) (string, []any) {
	conds := []string{"workspace = ?", "run_id = ?"}
	args := []any{q.workspace, q.runID}
	for _, m := range q.matches {
		conds, args = append(conds, m.column+" = ?"), append(args, m.value)
	}
	if q.since != nil {
		conds, args = append(conds, "timestamp >= ?"), append(args, q.since.UTC().Format(storedLayout))
	}
	if q.until != nil {
		conds, args = append(conds, "timestamp < ?"), append(args, q.until.UTC().Format(storedLayout))
	}
	cmp, dir := ">", "ASC"
	if q.desc {
		cmp, dir = "<", "DESC"
	}
	if q.after != nil {
		conds = append(conds, "(timestamp, id) "+cmp+" (?, ?)")
		args = append(args, q.after.Timestamp, q.after.ID)
	}
	args = append(args, q.limit)
	return selectRecords(table, columns) + ` WHERE ` + strings.Join(conds, " AND ") +
		` ORDER BY timestamp ` + dir + `, id ` + dir + ` LIMIT ?`, args
}

// list reads the records of the kind k that q asks for, from db.
func list[R record](ctx context.Context, db DB, k kind[R], q listQuery) ([]R, error) {
	query, args := q.sql(k.table, k.columns)
	return readRows(ctx, db, k, query, args...)
}

// readRows reads the records of the kind k that query, a selectRecords of
// its table and columns, selects with args, from db.
func readRows[R record](ctx context.Context, db DB, k kind[R], query string, args ...any) ([]R, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", k.table, err)
	}
	defer rows.Close()
	var recs []R
	for rows.Next() {
		var rec R
		h, dest := k.dest(&rec)
		if err := scanRecord(rows.Scan, k.table, h, dest...); err != nil {
			return nil, fmt.Errorf("read %s: %w", k.table, err)
		}
		recs = append(recs, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", k.table, err)
	}
	return recs, nil
}

// Scope is the set of workspaces a read across runs covers: every
// workspace, deleted ones included, when All is set; otherwise the
// workspaces Workspaces names and the task forks <parent>--<name> of the
// parents ForkParents names, which covers forks that no longer exist,
// except the slugs Except names. An empty Scope covers nothing.
type Scope struct {
	All         bool
	Workspaces  []string
	ForkParents []string
	Except      []string
}

// empty reports whether s covers no workspace.
func (s Scope) empty() bool {
	return !s.All && len(s.Workspaces) == 0 && len(s.ForkParents) == 0
}

// sql is the condition on the workspace column that s sets and its
// arguments.
func (s Scope) sql() (string, []any) {
	if s.All {
		return "1", nil
	}
	var ors []string
	var args []any
	if len(s.Workspaces) > 0 {
		ors, args = append(ors, "workspace IN ("+marks(len(s.Workspaces))+")"), append(args, store.Args(s.Workspaces)...)
	}
	if len(s.ForkParents) > 0 {
		// A fork's slug is its parent's, "--" and a name, and no
		// repository's slug holds "--".
		fork := "instr(workspace, '--') > 0 AND substr(workspace, 1, instr(workspace, '--') - 1) IN (" +
			marks(len(s.ForkParents)) + ")"
		args = append(args, store.Args(s.ForkParents)...)
		if len(s.Except) > 0 {
			fork += " AND workspace NOT IN (" + marks(len(s.Except)) + ")"
			args = append(args, store.Args(s.Except)...)
		}
		ors = append(ors, "("+fork+")")
	}
	return "(" + strings.Join(ors, " OR ") + ")", args
}

// RunEventPlace is the place of a run event in the order newest first:
// its timestamp, then its id, both descending. Inclusive makes a read
// after it include a run event at exactly this place.
type RunEventPlace struct {
	Timestamp time.Time
	ID        string
	Inclusive bool
}

// RunEventQuery is a read of the run events of every run of the
// workspaces Scope covers, newest first: those whose timestamp is at or
// after Since and before Until, when given, that come after Before in the
// order, when given, and that match every filter given, at most Limit of
// them.
type RunEventQuery struct {
	Scope        Scope
	Before       *RunEventPlace
	Since, Until *time.Time
	// Types are the entries a run event's event_type must match one of: an
	// exact type, or a prefix ending in * that matches every type beginning
	// with the text before the *; nil for any type.
	Types []string
	// The other filters each give the value the run event's field of the
	// same name must equal; nil for any. A field the run event does not
	// have (null) equals no value. SessionID is compared as it is, so it is
	// given in lower case, as ids are stored.
	Workspace, SessionID, RunID, NodeID, Severity *string
	Limit                                         int
}

// matches are the filters of q that compare a column with a value.
func (q RunEventQuery) matches() []match {
	var ms []match
	add := func(column string, value *string) {
		if value != nil {
			ms = append(ms, match{column: column, value: *value})
		}
	}
	add("workspace", q.Workspace)
	add("session_id", q.SessionID)
	add("run_id", q.RunID)
	add("node_id", q.NodeID)
	add("severity", q.Severity)
	return ms
}

// RunEventRecord is a stored run event as a read across runs returns it.
// Payload is the JSON object sent, nil when none was.
type RunEventRecord struct {
	ID        string
	Workspace string
	RunID     string
	Timestamp time.Time
	EventType string
	Severity  string
	NodeID    *string
	SessionID *string
	Payload   json.RawMessage
}

// NewestRunEvents reads the run events q asks for, ordered by timestamp,
// then id, both descending. A scope that covers nothing reads nothing.
func (c *Component) NewestRunEvents(ctx context.Context, q RunEventQuery) ([]RunEventRecord, error) {
	if q.Scope.empty() || q.Limit <= 0 {
		return nil, nil
	}
	scope, args := q.Scope.sql()
	conds := []string{scope}
	for _, m := range q.matches() {
		conds, args = append(conds, m.column+" = ?"), append(args, m.value)
	}
	if cond, targs := typesCondition("event_type", q.Types); cond != "" {
		conds, args = append(conds, cond), append(args, targs...)
	}
	if q.Since != nil {
		conds, args = append(conds, "timestamp >= ?"), append(args, q.Since.UTC().Format(storedLayout))
	}
	if q.Until != nil {
		conds, args = append(conds, "timestamp < ?"), append(args, q.Until.UTC().Format(storedLayout))
	}
	if b := q.Before; b != nil {
		cmp := "<"
		if b.Inclusive {
			cmp = "<="
		}
		conds, args = append(conds, "(timestamp, id) "+cmp+" (?, ?)"), append(args, b.Timestamp.UTC().Format(storedLayout), b.ID)
	}
	query := selectRecords(runEvents.table, runEvents.columns) + ` WHERE ` + strings.Join(conds, " AND ") +
		` ORDER BY timestamp DESC, id DESC LIMIT ?`
	recs, err := readRows(ctx, c.deps.DB, runEvents, query, append(args, q.Limit)...)
	if err != nil {
		return nil, err
	}
	out := make([]RunEventRecord, len(recs))
	for i, e := range recs {
		out[i] = RunEventRecord{
			ID: e.ID, Workspace: e.Workspace, RunID: e.RunID, Timestamp: time.Time(e.Timestamp), EventType: e.EventType,
			Severity: e.Severity, NodeID: e.NodeID, SessionID: e.SessionID, Payload: e.Payload,
		}
	}
	return out, nil
}

// typesCondition is the condition that column matches one of the entries
// of types, and its arguments; "" when every value matches. A prefix is
// compared as text, not with LIKE, whose _ and % a type may hold.
func typesCondition(column string, types []string) (string, []any) {
	var ors []string
	var args []any
	for _, entry := range types {
		prefix, ok := strings.CutSuffix(entry, "*")
		switch {
		case !ok:
			ors, args = append(ors, column+" = ?"), append(args, entry)
		case prefix == "":
			return "", nil
		default:
			ors, args = append(ors, "substr("+column+", 1, length(?)) = ?"), append(args, prefix, prefix)
		}
	}
	if len(ors) == 0 {
		return "", nil
	}
	return "(" + strings.Join(ors, " OR ") + ")", args
}

// marks are n comma-separated placeholders.
func marks(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
