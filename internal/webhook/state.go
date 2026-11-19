package webhook

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// The types of the events the hub writes when it changes a subscription's
// state on its own.
const (
	EventFailing  = "parley.webhook.failing"
	EventDisabled = "parley.webhook.disabled"
)

// The retry schedule of a delivery whose attempt failed.
const (
	// firstRetryWait is the wait after the first failed attempt; each further
	// failure doubles it, so that a receiver that is briefly down is tried
	// again soon and one that stays down is not hammered.
	firstRetryWait = 10 * time.Second
	// maxRetryWait caps the wait, so that a receiver that comes back is
	// found within the hour.
	maxRetryWait = time.Hour
	// retryWindow is how long after its first attempt an event is tried.
	retryWindow = 24 * time.Hour
	// disableAfter is how long a subscription may go without a success
	// before it is disabled, so that a receiver gone for good is not tried
	// for ever.
	disableAfter = 72 * time.Hour
	// disableCheckInterval is how often the subscriptions are checked for
	// one to disable, so that one is disabled within a minute of its time.
	disableCheckInterval = time.Minute
)

// retryWait is the wait after the failed attempt number n of a delivery,
// counted from 1.
func retryWait(n int) time.Duration {
	wait := firstRetryWait
	for i := 1; i < n && wait < maxRetryWait; i++ {
		wait *= 2
	}
	return min(wait, maxRetryWait)
}

// nextAttempt is when the next attempt of a delivery is due after its
// failed attempt number n, made at at, whose first attempt was made at
// first: after the wait, and never later than the end of the event's retry
// window while it is still ahead.
func nextAttempt(first, at time.Time, n int) time.Time {
	next := at.Add(retryWait(n))
	if end := first.Add(retryWindow); end.After(at) && end.Before(next) {
		return end
	}
	return next
}

// recordFailure sets what a failed attempt at at changes on the subscription
// id: last_status failed, last_delivery_at, and failing_since when it has
// none, so that it tells since when the receiver has been failing. The state
// does not change.
func recordFailure(ctx context.Context, q querier, id string, at time.Time) error {
	_, err := q.ExecContext(ctx, `UPDATE webhook_subscriptions
		SET last_status = 'failed', last_delivery_at = ?, failing_since = coalesce(failing_since, ?)
		WHERE id = ?`, at.UnixNano(), at.UnixNano(), id)
	if err != nil {
		return fmt.Errorf("record the failure of event webhook %s: %w", id, err)
	}
	return nil
}

// giveUp records the failed attempt at at of the delivery d of sub, made at
// the end of its event's retry window or later, in one transaction: the
// delivery ends failed with no next attempt, the position moves past its
// event, the subscription gets what any failure sets (recordFailure) and,
// when it was active, becomes failing, which writes parley.webhook.failing.
// The loop goes on with the next event at once, which gets a window of its
// own.
func (c *Component) giveUp(ctx context.Context, sub Subscription, d delivery, at time.Time, out outcome) (next, time.Time, error) {
	var recorded bool
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		if recorded, err = recordAttempt(ctx, tx, d, at, out, deliveryFailed, nil); err != nil || !recorded {
			return err
		}
		if err := recordFailure(ctx, tx, sub.ID, at); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE webhook_subscriptions SET position = max(position, ?) WHERE id = ?`, d.Seq, sub.ID); err != nil {
			return fmt.Errorf("move the position of event webhook %s: %w", sub.ID, err)
		}
		since, turned, err := markFailing(ctx, tx, sub.ID)
		if err != nil || !turned {
			return err
		}
		_, err = c.emitSystem(ctx, tx, EventFailing, sub, map[string]any{
			"webhook_id": sub.ID, "host": hostOf(sub.URL), "seq": d.Seq, "failing_since": timefmt.Time(since),
			"response_code": nullIfZero(int64(out.responseCode)), "error": nullIfEmpty(out.failure),
		})
		return err
	})
	if err != nil {
		return idle, time.Time{}, err
	}
	if !recorded {
		// Something ended the delivery while the attempt was in flight.
		return again, time.Time{}, nil
	}
	c.report(resultFailed)
	return again, time.Time{}, nil
}

// markFailing sets the subscription id failing when it is active, and
// returns its failing_since; turned is false when it was not active, so that
// its parley.webhook.failing is written once per turn into failing.
func markFailing(ctx context.Context, tx *sql.Tx, id string) (time.Time, bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE webhook_subscriptions SET state = ? WHERE id = ? AND state = ?`,
		StateFailing, id, StateActive)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("mark event webhook %s failing: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return time.Time{}, false, err
	}
	var since int64
	if err := tx.QueryRowContext(ctx, `SELECT failing_since FROM webhook_subscriptions WHERE id = ?`, id).Scan(&since); err != nil {
		return time.Time{}, false, fmt.Errorf("read the failing_since of event webhook %s: %w", id, err)
	}
	return time.Unix(0, since).UTC(), true, nil
}

// endPending ends the pending delivery of the subscription id failed, with
// no next attempt, as a subscription that is disabled sends nothing more; the
// record stays in the delivery log.
func endPending(ctx context.Context, q querier, id string) error {
	_, err := q.ExecContext(ctx, `UPDATE webhook_deliveries SET status = ?, next_attempt_at = NULL
		WHERE subscription_id = ? AND status = ?`, deliveryFailed, id, deliveryPending)
	if err != nil {
		return fmt.Errorf("end the pending delivery of event webhook %s: %w", id, err)
	}
	return nil
}

// switchTo applies a change's active to s inside tx. Switched on, a
// disabled or failing subscription becomes active without failing_since;
// a failing one keeps its position and pending delivery, and reenabled
// reports a disabled one, whose position the caller sets to the seq of the
// change's own event, so that it sends nothing written while it was off.
// Switched off, an active or failing subscription becomes disabled and its
// pending delivery ends failed; failing_since stays until a switch-on. A
// subscription already in the state asked for does not change.
func switchTo(ctx context.Context, tx *sql.Tx, s *Subscription, on bool) (reenabled bool, err error) {
	switch {
	case on && s.State != StateActive:
		reenabled = s.State == StateDisabled
		s.State, s.FailingSince = StateActive, nil
		if _, err := tx.ExecContext(ctx, `UPDATE webhook_subscriptions SET failing_since = NULL WHERE id = ?`, s.ID); err != nil {
			return false, fmt.Errorf("clear the failing_since of event webhook %s: %w", s.ID, err)
		}
	case !on && s.State != StateDisabled:
		s.State = StateDisabled
		if err := endPending(ctx, tx, s.ID); err != nil {
			return false, err
		}
	}
	return reenabled, nil
}

// watchFailing disables, at least once a minute on the component's clock,
// every subscription that has been failing for disableAfter, until ctx
// ends. Each check's next one is due a minute after the time it started, so
// that a clock that moved on meanwhile brings it at once.
func (c *Component) watchFailing(ctx context.Context) {
	for {
		now := c.deps.Clock.Now()
		if err := c.disableStale(ctx, now); err != nil && ctx.Err() == nil {
			c.deps.Logger.Warn("event webhook disabling check", "error", err.Error())
		}
		t := c.deps.Clock.NewTimer(now.Add(disableCheckInterval).Sub(c.deps.Clock.Now()))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C():
		}
	}
}

// disableStale disables every active or failing subscription whose
// failing_since is disableAfter or more before now.
func (c *Component) disableStale(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-disableAfter)
	rows, err := c.deps.Store.Main().QueryContext(ctx, `SELECT id FROM webhook_subscriptions
		WHERE state != ? AND failing_since <= ?`, StateDisabled, cutoff.UnixNano())
	if err != nil {
		return fmt.Errorf("find the event webhooks to disable: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("find the event webhooks to disable: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("find the event webhooks to disable: %w", err)
	}
	for _, id := range ids {
		if err := c.disable(ctx, id, cutoff); err != nil {
			return err
		}
	}
	return nil
}

// disable disables the subscription id, unless a success or a change has
// moved it out of reach since it was found, in one transaction: the state
// disabled, its pending delivery ended failed and parley.webhook.disabled.
// Its loop then finds it disabled and ends.
func (c *Component) disable(ctx context.Context, id string, cutoff time.Time) error {
	var changed bool
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		sub, found, err := subscriptionByID(ctx, tx, id)
		if err != nil || !found || sub.State == StateDisabled || sub.FailingSince == nil || sub.FailingSince.After(cutoff) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE webhook_subscriptions SET state = ? WHERE id = ?`, StateDisabled, id); err != nil {
			return fmt.Errorf("disable event webhook %s: %w", id, err)
		}
		if err := endPending(ctx, tx, id); err != nil {
			return err
		}
		_, err = c.emitSystem(ctx, tx, EventDisabled, sub, map[string]any{
			"webhook_id": id, "host": hostOf(sub.URL), "failing_since": timefmt.Time(*sub.FailingSince),
		})
		changed = err == nil
		return err
	})
	if err != nil {
		return err
	}
	if changed {
		c.wakeLoop(id)
	}
	return nil
}

// emitSystem writes the event typ of the subscription s, caused by the hub
// itself, with metadata, inside tx.
func (c *Component) emitSystem(ctx context.Context, tx *sql.Tx, typ string, s Subscription, metadata map[string]any) (event.Record, error) {
	return c.deps.Events.Emit(ctx, tx, event.Input{
		Type: typ, Workspace: &s.Workspace, ActorType: event.ActorSystem,
		ResourceType: resourceType, ResourceID: s.ID, Metadata: metadata,
	})
}
