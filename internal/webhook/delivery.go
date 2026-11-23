package webhook

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/store"
)

// The statuses of a delivery.
const (
	deliveryPending   = "pending"
	deliverySucceeded = "succeeded"
	deliveryFailed    = "failed"
)

// delivery is the stored record of one event sent to one subscription, as
// the loop works with it: the event's seq and type, the attempts made so
// far, the time of the first one, nil before it, and when the next one is
// due, nil for at once.
type delivery struct {
	ID             string
	Seq            int64
	Type           string
	Attempts       int
	FirstAttemptAt *time.Time
	NextAttemptAt  *time.Time
}

// subscriptionByID reads the subscription id, whatever its workspace; found
// is false when it is gone.
func subscriptionByID(ctx context.Context, q querier, id string) (Subscription, bool, error) {
	s, err := scanSubscription(q.QueryRowContext(ctx,
		`SELECT `+subscriptionColumns+` FROM webhook_subscriptions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{}, false, nil
	}
	if err != nil {
		return Subscription{}, false, fmt.Errorf("read event webhook %s: %w", id, err)
	}
	return s, true, nil
}

// pendingDelivery reads the pending delivery of the subscription id; found
// is false when it has none. A subscription has at most one: the loop
// records the next only once the last has ended.
func pendingDelivery(ctx context.Context, q querier, id string) (delivery, bool, error) {
	var d delivery
	var first, next sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT id, seq, type, attempts, first_attempt_at, next_attempt_at
		FROM webhook_deliveries WHERE subscription_id = ? AND status = 'pending'`, id).
		Scan(&d.ID, &d.Seq, &d.Type, &d.Attempts, &first, &next)
	if errors.Is(err, sql.ErrNoRows) {
		return delivery{}, false, nil
	}
	if err != nil {
		return delivery{}, false, fmt.Errorf("read the pending delivery of event webhook %s: %w", id, err)
	}
	d.FirstAttemptAt, d.NextAttemptAt = store.TimeOf(first), store.TimeOf(next)
	return d, true, nil
}

// movePosition moves the position of the subscription id from from to to,
// unless it is no longer from or the subscription is disabled: then
// something changed the subscription since the loop read it, and moved
// reports false so that the loop reads it again.
func movePosition(ctx context.Context, q querier, id string, from, to int64) (bool, error) {
	res, err := q.ExecContext(ctx, `UPDATE webhook_subscriptions SET position = ?
		WHERE id = ? AND position = ? AND state != 'disabled'`, to, id, from)
	if err != nil {
		return false, fmt.Errorf("move the position of event webhook %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("move the position of event webhook %s: %w", id, err)
	}
	return n == 1, nil
}

// insertDelivery records d as the pending delivery of the subscription id,
// created at now, with no attempt made yet.
func insertDelivery(ctx context.Context, q querier, id string, d delivery, now time.Time) error {
	_, err := q.ExecContext(ctx, `INSERT INTO webhook_deliveries (id, subscription_id, seq, type, status, attempts, created_at)
		VALUES (?, ?, ?, ?, ?, 0, ?)`, d.ID, id, d.Seq, d.Type, deliveryPending, now.UnixNano())
	if err != nil {
		return fmt.Errorf("record delivery %s of event webhook %s: %w", d.ID, id, err)
	}
	return nil
}

// recordAttempt records an attempt of the pending delivery d made at at
// with outcome out: one more attempt, the code, error and duration, at as
// the last attempt and as the first when it was. status is the delivery's
// status after it, and next when the next attempt is due, nil for none.
// recorded is false when d is no longer pending: something ended it while
// the attempt was in flight, and its outcome changes nothing.
func recordAttempt(ctx context.Context, q querier, d delivery, at time.Time, out outcome, status string, next *time.Time) (bool, error) {
	res, err := q.ExecContext(ctx, `UPDATE webhook_deliveries
		SET status = ?, attempts = attempts + 1, response_code = ?, error = ?, duration_ms = ?,
			first_attempt_at = coalesce(first_attempt_at, ?), last_attempt_at = ?, next_attempt_at = ?
		WHERE id = ? AND status = 'pending'`,
		status, nullIfZero(int64(out.responseCode)), nullIfEmpty(out.failure), out.durationMS,
		at.UnixNano(), at.UnixNano(), nanosOrNil(next), d.ID)
	if err != nil {
		return false, fmt.Errorf("record an attempt of delivery %s: %w", d.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record an attempt of delivery %s: %w", d.ID, err)
	}
	return n == 1, nil
}

// recordSuccess sets what a successful attempt at at changes on the
// subscription id: the position past the event seq, last_status succeeded,
// last_delivery_at, no failing_since, and active again when it was failing,
// since the receiver is back. That return writes no event.
func recordSuccess(ctx context.Context, q querier, id string, seq int64, at time.Time) error {
	_, err := q.ExecContext(ctx, `UPDATE webhook_subscriptions
		SET position = max(position, ?), last_status = 'succeeded', last_delivery_at = ?, failing_since = NULL,
			state = CASE WHEN state = 'failing' THEN 'active' ELSE state END
		WHERE id = ?`, seq, at.UnixNano(), id)
	if err != nil {
		return fmt.Errorf("record the success of event webhook %s: %w", id, err)
	}
	return nil
}

// dropDelivery marks the pending delivery d failed, with no next attempt,
// and moves the position of the subscription id to past, unless it is
// already there. dropped is false when d was no longer pending.
func dropDelivery(ctx context.Context, q querier, id string, d delivery, past int64) (bool, error) {
	res, err := q.ExecContext(ctx, `UPDATE webhook_deliveries SET status = ?, next_attempt_at = NULL
		WHERE id = ? AND status = 'pending'`, deliveryFailed, d.ID)
	if err != nil {
		return false, fmt.Errorf("end delivery %s: %w", d.ID, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	if _, err := q.ExecContext(ctx, `UPDATE webhook_subscriptions SET position = max(position, ?) WHERE id = ?`, past, id); err != nil {
		return false, fmt.Errorf("move the position of event webhook %s: %w", id, err)
	}
	return true, nil
}

func nullIfZero(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
