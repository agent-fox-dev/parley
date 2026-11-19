package event

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// outOfLog restricts a delete of hub events to those that have left the
// event log, below oldest_seq as the delete's own transaction reads it, and
// below the highest stored seq: every read of the log is a prefix without
// gaps, so an event still in it stays stored whatever its age, and the
// newest event always stays (ADR 05).
const outOfLog = `seq < (SELECT oldest_seq FROM hub_event_log_bound WHERE only = 1)
AND seq < (SELECT max(seq) FROM hub_events)`

// DeleteBefore deletes, in one transaction, the stored hub events whose
// time is before cutoff and that have left the event log, but never the one
// with the highest seq, and returns the number it deleted. last_seq and
// oldest_seq do not move: deleting events never lowers the counter, and the
// retention run moves the log's start with Expire before it deletes.
func (c *Component) DeleteBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	at := timefmt.Format(ceilSecond(cutoff))
	n, err := c.deleteEvents(ctx, `DELETE FROM hub_events WHERE time < ? AND `+outOfLog, at)
	if err != nil {
		return 0, fmt.Errorf("delete the hub events before %s: %w", at, err)
	}
	return n, nil
}

// WorkspaceSlugs returns every workspace slug a stored hub event carries;
// an event of no workspace carries none.
func (c *Component) WorkspaceSlugs(ctx context.Context) ([]string, error) {
	if c.deps.Store == nil {
		return nil, errNotBound
	}
	rows, err := c.deps.Store.Main().QueryContext(ctx,
		`SELECT DISTINCT workspace FROM hub_events WHERE workspace IS NOT NULL ORDER BY workspace`)
	if err != nil {
		return nil, fmt.Errorf("read the workspaces of the hub events: %w", err)
	}
	defer rows.Close()
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("read the workspaces of the hub events: %w", err)
		}
		slugs = append(slugs, slug)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the workspaces of the hub events: %w", err)
	}
	return slugs, nil
}

// DeleteWorkspaceRecordsBefore deletes, in one transaction, the stored hub
// events of the workspaces slugs whose time is before cutoff and that have
// left the event log, but never the one with the highest seq, and returns
// the number it deleted. An event of no workspace is never deleted by it.
func (c *Component) DeleteWorkspaceRecordsBefore(ctx context.Context, slugs []string, cutoff time.Time) (int64, error) {
	list, err := json.Marshal(slugs)
	if err != nil {
		return 0, err
	}
	at := timefmt.Format(ceilSecond(cutoff))
	n, err := c.deleteEvents(ctx, `DELETE FROM hub_events
WHERE workspace IN (SELECT value FROM json_each(?)) AND time < ? AND `+outOfLog, string(list), at)
	if err != nil {
		return 0, fmt.Errorf("delete the hub events of %d workspaces before %s: %w", len(slugs), at, err)
	}
	return n, nil
}

// deleteEvents runs the delete query with args in a transaction of its own
// and returns the number of events it deleted.
func (c *Component) deleteEvents(ctx context.Context, query string, args ...any) (int64, error) {
	if c.deps.Store == nil {
		return 0, errNotBound
	}
	var n int64
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// CountRecords returns the number of stored hub events under the record
// kind hub_event.
func (c *Component) CountRecords(ctx context.Context) (map[string]int64, error) {
	if c.deps.Store == nil {
		return nil, errNotBound
	}
	var n int64
	if err := c.deps.Store.Main().QueryRowContext(ctx, `SELECT count(*) FROM hub_events`).Scan(&n); err != nil {
		return nil, fmt.Errorf("count the hub events: %w", err)
	}
	return map[string]int64{"hub_event": n}, nil
}
