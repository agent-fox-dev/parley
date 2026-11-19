package webhook

import "github.com/agent-fox-dev/parley/internal/metrics"

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
