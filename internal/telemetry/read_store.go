package telemetry

import (
	"context"
	"fmt"
	"strings"
	"time"
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
