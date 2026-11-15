package change

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/agent-fox-dev/parley/internal/store"
)

// EventReady is the event of a change that became landable: open, its
// landing ready and its mergeable clean.
const EventReady = "parley.change.ready"

// AnnouncedStep gives the change record the head it was announced landable
// at, NULL while it has not been since it last was not landable. It
// applies after MergeableStep.
var AnnouncedStep = store.Step{
	Name:   "change_ready_announced",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `ALTER TABLE changes ADD COLUMN ready_announced_head TEXT`)
		return err
	},
}

// AnnounceLandable is the announcement step of the change number of the
// workspace, run in tx, the transaction that stored what it judges: a
// mergeability result, a move between draft and open, a retarget, a
// restore, or the landing a policy evaluated. It is the one writer of
// parley.change.ready, so that the two inputs of landable, mergeable and
// landing, which have two owners, give one event per head and landable
// period.
//
// While mergeable is unknown it does nothing. A change that is open, ready
// to land and clean is announced, with an event by the hub, unless it was
// announced at its head already; any other is forgotten (ForgetLandable),
// so that it is announced again once it is landable. The event and the
// head it remembers are written in tx, and go with it when it is rolled
// back.
func (c *Component) AnnounceLandable(ctx context.Context, tx *sql.Tx, workspace string, number int64) error {
	ch, err := readChange(ctx, tx, workspace, number)
	if err != nil || ch.Mergeable == MergeableUnknown {
		return err
	}
	if ch.State != StateOpen || ch.Landing.Status != LandingReady || ch.Mergeable != MergeableClean {
		return c.ForgetLandable(ctx, tx, workspace, number)
	}
	var announced sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT ready_announced_head FROM changes WHERE workspace_slug = ? AND number = ?`,
		workspace, number).Scan(&announced); err != nil {
		return fmt.Errorf("read the announcement of %s: %w", ch.Ref, err)
	}
	if announced.Valid && announced.String == ch.HeadSHA {
		return nil
	}
	if err := c.emit(ctx, tx, Caller{}, ch, EventReady, map[string]any{"version": ch.Version, "head_sha": ch.HeadSHA}); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE changes SET ready_announced_head = ? WHERE workspace_slug = ? AND number = ?`,
		ch.HeadSHA, workspace, number); err != nil {
		return fmt.Errorf("store the announcement of %s: %w", ch.Ref, err)
	}
	return nil
}

// ForgetLandable forgets, in tx, the head the change number of the
// workspace was announced landable at, as a move that makes it not
// landable must, a retarget or an abandon among them: the next time it is
// landable it is announced again, at the same head too. A change that does
// not exist is the change-not-found error.
func (c *Component) ForgetLandable(ctx context.Context, tx *sql.Tx, workspace string, number int64) error {
	res, err := tx.ExecContext(ctx, `UPDATE changes SET ready_announced_head = NULL WHERE workspace_slug = ? AND number = ?`,
		workspace, number)
	if err != nil {
		return fmt.Errorf("forget the announcement of %s: %w", Ref(workspace, number), err)
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		err = errChangeNotFound(workspace, number)
	}
	return err
}
