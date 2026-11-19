package webhook

import (
	"context"
	"database/sql"
	"fmt"
)

// resumeLoops starts the loop of every active and failing subscription, as
// the store holds it when the component starts: each goes on from its
// stored position, and with its pending delivery first, under its id and
// with its attempts. A pending delivery with no next attempt, or one already
// due, had its attempt in flight when the hub last stopped, which recorded
// nothing of it, and is attempted again at once; any other waits for its
// time. A disabled subscription gets no loop. A store that cannot be read
// is tried again after errorWait until ctx ends.
func (c *Component) resumeLoops(ctx context.Context) {
	for {
		ids, err := idsOf(ctx, c.deps.Store.Main(),
			`SELECT id FROM webhook_subscriptions WHERE state IN (?, ?) ORDER BY created_at, id`, StateActive, StateFailing)
		if err == nil {
			for _, id := range ids {
				c.startLoop(id)
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		c.deps.Logger.Warn("cannot resume the event webhook delivery loops", "error", err.Error())
		t := c.deps.Clock.NewTimer(errorWait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C():
		}
	}
}

// RemoveWorkspace deletes the event webhook subscriptions of the workspace
// slug in tx, the removal transaction of a deleted workspace, with their
// delivery records and their sealed signing secrets, and writes no event:
// the workspace's own deletion is what the log records. It has the shape of
// a workspace.Remover (ADR 07); a task fork has no subscriptions and loses
// nothing.
//
// The loops of the removed subscriptions end once tx has committed: each
// reads its subscription again before every attempt and ends when it is
// gone, so a rollback leaves them running. So that they notice at once
// rather than at the next event, a transaction of the component's own,
// which the store runs only after tx has ended, finds which of them are gone
// and wakes their loops (the pattern of ADR 09).
func (c *Component) RemoveWorkspace(ctx context.Context, tx *sql.Tx, slug string) error {
	ids, err := idsOf(ctx, tx, `SELECT id FROM webhook_subscriptions WHERE workspace = ?`, slug)
	if err != nil {
		return fmt.Errorf("find the event webhooks of %s: %w", slug, err)
	}
	if len(ids) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM webhook_deliveries
		WHERE subscription_id IN (SELECT id FROM webhook_subscriptions WHERE workspace = ?)`, slug); err != nil {
		return fmt.Errorf("delete the event webhook deliveries of %s: %w", slug, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM webhook_subscriptions WHERE workspace = ?`, slug); err != nil {
		return fmt.Errorf("delete the event webhooks of %s: %w", slug, err)
	}
	go c.wakeRemoved(context.WithoutCancel(ctx), ids)
	return nil
}

// wakeRemoved wakes the loops of those of ids that are gone, once the
// transaction that removed them has ended.
func (c *Component) wakeRemoved(ctx context.Context, ids []string) {
	var gone []string
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		gone = gone[:0]
		for _, id := range ids {
			_, found, err := subscriptionByID(ctx, tx, id)
			if err != nil {
				return err
			}
			if !found {
				gone = append(gone, id)
			}
		}
		return nil
	})
	if err != nil {
		// Each loop still ends when the next event wakes it.
		c.deps.Logger.Warn("cannot wake the loops of removed event webhooks", "webhook_ids", ids, "error", err.Error())
		return
	}
	for _, id := range gone {
		c.wakeLoop(id)
	}
}

// idsOf is the first column of the rows query returns, as strings.
func idsOf(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
