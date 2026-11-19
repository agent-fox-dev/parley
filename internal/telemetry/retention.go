package telemetry

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// recordTables are the tables of the records [audit].max_age limits: the
// run events, session outcomes, tool calls and tool errors.
var recordTables = []string{runEvents.table, outcomes.table, toolCalls.table, toolErrors.table}

// runTables are the tables of the records that make a run, which
// KeepNewestRuns counts and trims: the record tables and the traces, never
// the postmortems.
var runTables = append(slices.Clone(recordTables), traces.table)

// postmortemsTable is the table of the postmortems.
const postmortemsTable = "postmortems"

// ceil is t in UTC rounded up to a multiple of unit, the precision of the
// column it is compared with: a record older than t is then exactly one
// whose stored time sorts before the rounded one, and a record at t is
// kept.
func ceil(t time.Time, unit time.Duration) time.Time {
	t = t.UTC()
	if r := t.Truncate(unit); r.Before(t) {
		return r.Add(unit)
	}
	return t
}

// DeleteRunRecordsBefore deletes, in one transaction, the run events,
// session outcomes, tool calls and tool errors whose timestamp is before
// cutoff, and returns the number it deleted. Traces and postmortems have
// limits of their own.
func (c *Component) DeleteRunRecordsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	return c.deleteBefore(ctx, recordTables, "timestamp", ceil(cutoff, time.Microsecond).Format(storedLayout))
}

// DeleteTracesBefore deletes, in one transaction, the trace records whose
// timestamp is before cutoff, and returns the number it deleted.
func (c *Component) DeleteTracesBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	return c.deleteBefore(ctx, []string{traces.table}, "timestamp", ceil(cutoff, time.Microsecond).Format(storedLayout))
}

// DeletePostmortemsBefore deletes, in one transaction, the postmortems
// ingested before cutoff, whatever their own timestamp, and returns the
// number it deleted.
func (c *Component) DeletePostmortemsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	return c.deleteBefore(ctx, []string{postmortemsTable}, "ingested_at", timefmt.Format(ceil(cutoff, time.Second)))
}

// deleteBefore deletes from each of tables, in one transaction, the
// records whose column sorts before the stored time cutoff.
func (c *Component) deleteBefore(ctx context.Context, tables []string, column, cutoff string) (int64, error) {
	var n int64
	err := c.writeTx(ctx, func(ctx context.Context, tx Tx) error {
		for _, table := range tables {
			res, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+column+` < ?`, cutoff)
			if err != nil {
				return fmt.Errorf("delete from %s: %w", table, err)
			}
			d, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("delete from %s: %w", table, err)
			}
			n += d
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("delete the records with %s before %s: %w", column, cutoff, err)
	}
	return n, nil
}

// excessRuns selects, as one JSON array of [workspace, run_id] pairs, the
// runs of every workspace beyond the newest ? runs. A run is a workspace's
// run id with at least one record of runTables; its age is the timestamp of
// its earliest record, and of two runs as old the lower run id is the
// older.
const excessRuns = `
WITH firsts AS (
	SELECT workspace, run_id, MIN(timestamp) AS first FROM (
		SELECT workspace, run_id, timestamp FROM run_events
		UNION ALL SELECT workspace, run_id, timestamp FROM session_outcomes
		UNION ALL SELECT workspace, run_id, timestamp FROM tool_calls
		UNION ALL SELECT workspace, run_id, timestamp FROM tool_errors
		UNION ALL SELECT workspace, run_id, timestamp FROM traces
	) GROUP BY workspace, run_id
), ranked AS (
	SELECT workspace, run_id, ROW_NUMBER() OVER (PARTITION BY workspace ORDER BY first DESC, run_id DESC) AS place
	FROM firsts
)
SELECT json_group_array(json_array(workspace, run_id)) FROM ranked WHERE place > ?`

// KeepNewestRuns deletes, in one transaction, the records of runTables of
// every workspace's runs but its newest n, and returns the number it
// deleted. The runs to trim are chosen once, before any delete, so that a
// delete does not change which runs are the oldest. Postmortems stay: a
// run with only a postmortem is not counted, and the postmortem of a
// trimmed run is kept.
func (c *Component) KeepNewestRuns(ctx context.Context, n int) (int64, error) {
	var deleted int64
	err := c.writeTx(ctx, func(ctx context.Context, tx Tx) error {
		var runs string
		if err := tx.QueryRowContext(ctx, excessRuns, n).Scan(&runs); err != nil {
			return fmt.Errorf("choose the runs to trim: %w", err)
		}
		if runs == "[]" {
			return nil
		}
		for _, table := range runTables {
			res, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE (workspace, run_id) IN (
	SELECT value ->> 0, value ->> 1 FROM json_each(?))`, runs)
			if err != nil {
				return fmt.Errorf("delete from %s: %w", table, err)
			}
			d, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("delete from %s: %w", table, err)
			}
			deleted += d
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("keep the newest %d runs of each workspace: %w", n, err)
	}
	return deleted, nil
}
