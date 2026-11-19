package webhook

import (
	"context"
	"fmt"
	"time"
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
