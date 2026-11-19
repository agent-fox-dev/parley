package session

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// msgStale is the error message of a session the retention run timed out.
const msgStale = "session exceeded maximum active age"

// storedCutoff is cutoff in the stored form of a timestamp, rounded up to
// the second: started_at holds whole seconds, so a session started before
// cutoff is exactly one whose stored time sorts before the rounded one, and
// a session started at cutoff is kept.
func storedCutoff(cutoff time.Time) string {
	t := cutoff.UTC()
	if r := t.Truncate(time.Second); r.Before(t) {
		t = r.Add(time.Second)
	}
	return timefmt.Format(t)
}

// TimeOutActiveBefore ends every session that is active and was started
// before cutoff as timeout, ended at now in UTC whole seconds, with the
// error message "session exceeded maximum active age" and the milliseconds
// from its start to its end as its duration, in one transaction. It writes
// no hub event: none reports a timeout. The end is the conditional one a
// complete makes, so a session completed meanwhile keeps its outcome. It
// returns the number of sessions it ended.
func (c *Component) TimeOutActiveBefore(ctx context.Context, cutoff, now time.Time) (int64, error) {
	ended := now.UTC().Truncate(time.Second)
	var n int64
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		stale, err := staleSessions(ctx, tx, storedCutoff(cutoff))
		if err != nil {
			return err
		}
		message := msgStale
		for id, started := range stale {
			d := ended.Sub(started).Milliseconds()
			changed, err := endActive(ctx, tx, id, ending{Status: StatusTimeout, EndedAt: ended, ErrorMessage: &message, DurationMS: &d})
			if err != nil {
				return err
			}
			if changed {
				n++
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("time out the sessions active since before %s: %w", timefmt.Format(cutoff), err)
	}
	return n, nil
}

// staleSessions reads, through tx, the start of every active session
// started before the stored time cutoff, by id.
func staleSessions(ctx context.Context, tx *sql.Tx, cutoff string) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	err := eachRow(ctx, tx, func(scan func(...any) error) error {
		var id, started string
		if err := scan(&id, &started); err != nil {
			return err
		}
		t, err := time.Parse(time.RFC3339, started)
		if err != nil {
			return fmt.Errorf("session %s: started_at: %w", id, err)
		}
		out[id] = t
		return nil
	}, `SELECT id, started_at FROM sessions WHERE status = 'active' AND started_at < ?`, cutoff)
	return out, err
}

// DeleteTerminalBefore deletes every session that has ended (completed,
// failed, timeout or terminated) and was started before cutoff, with its
// usage records and its links, in one transaction; an active session is
// never deleted, whatever its age. It returns the number of sessions it
// deleted.
func (c *Component) DeleteTerminalBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	const ended = `SELECT id FROM sessions WHERE status <> 'active' AND started_at < ?`
	at := storedCutoff(cutoff)
	var n int64
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		// The usage and the links are deleted here rather than through the
		// cascade, so that the step does not rest on the connection's
		// foreign_keys pragma.
		for _, table := range []string{"session_usage", "session_links"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE session_id IN (`+ended+`)`, at); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id IN (`+ended+`)`, at)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("delete the ended sessions started before %s: %w", timefmt.Format(cutoff), err)
	}
	return n, nil
}
