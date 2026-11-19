package webhook

import (
	"context"
	"fmt"

	"github.com/agent-fox-dev/parley/internal/metrics"
)

// The results a delivery attempt is counted under in
// parley_webhook_deliveries_total.
const (
	// resultOK is an attempt that succeeded.
	resultOK = "ok"
	// resultRetry is an attempt that failed and leaves its delivery pending
	// for another one.
	resultRetry = "retry"
	// resultFailed is a delivery that ended without success: given up, or
	// its event left the log before it could be sent.
	resultFailed = "failed"
)

// report counts one delivery attempt with result. The labels are the
// catalogue's closed set, so the recorder has no reason to refuse it, and a
// count that is lost changes no delivery.
func (c *Component) report(result string) {
	if err := c.deps.Recorder.Add(metrics.WebhookDeliveriesTotal, map[string]string{"result": result}, 1); err != nil {
		c.deps.Logger.Warn("cannot count an event webhook delivery", "result", result, "error", err.Error())
	}
}

// QueueDepth is the number of events written and not yet delivered or given
// up, summed over the active and failing subscriptions: for each, the events
// after its position that it receives, as the pending of a read of one
// subscription counts them. A disabled subscription receives nothing and
// counts nothing. The hub reports it as parley_webhook_queue_depth.
func (c *Component) QueueDepth(ctx context.Context) (int, error) {
	ids, err := idsOf(ctx, c.deps.Store.Main(),
		`SELECT id FROM webhook_subscriptions WHERE state IN (?, ?)`, StateActive, StateFailing)
	if err != nil {
		return 0, fmt.Errorf("read the event webhooks to count their queue: %w", err)
	}
	total := 0
	for _, id := range ids {
		sub, found, err := subscriptionByID(ctx, c.deps.Store.Main(), id)
		if err != nil {
			return 0, err
		}
		// One deleted or switched off since the read queues nothing.
		if !found {
			continue
		}
		n, err := c.pending(ctx, sub)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}
