package audit

import (
	"context"
	"time"
)

// EventStore is the hub event log and store as the event steps reach it;
// *event.Component satisfies it.
type EventStore interface {
	// Retention is how long an event stays in the event log,
	// [events].retention.
	Retention() time.Duration
	// Expire makes the events older than cutoff leave the event log,
	// keeping the newest, and deletes none.
	Expire(ctx context.Context, cutoff time.Time) (int64, error)
	// DeleteBefore deletes the stored events older than cutoff but the
	// newest, in one transaction, and returns the number it deleted.
	DeleteBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

// EventSteps returns the steps event_log, which moves the start of the
// event log to the first event not older than the store's retention, and
// hub_events, which deletes the stored events older than
// settings.MaxAge. Neither removes the newest event. The events that
// leave the log stay stored, so that the audit query still returns them
// until hub_events deletes them. Each step reads clock once; the store's
// error is the step's.
func EventSteps(e EventStore, settings Settings, clock func() time.Time) map[string]Step {
	return map[string]Step{
		"event_log": func(ctx context.Context) error {
			_, err := e.Expire(ctx, clock().Add(-e.Retention()))
			return err
		},
		"hub_events": func(ctx context.Context) error {
			_, err := e.DeleteBefore(ctx, clock().Add(-settings.MaxAge))
			return err
		},
	}
}

// DeliveryAge is how long an event webhook delivery record that is no
// longer pending is kept after its last attempt.
const DeliveryAge = 7 * day

// DeliveryPruner is the store of event webhook deliveries as the
// webhook_deliveries step reaches it. The webhook feature registers its
// store from the hub, on the hub's composition (RegisterDeliveryPruner), so
// that this package does not depend on it.
type DeliveryPruner interface {
	// PruneDeliveries deletes the delivery records that are not pending
	// and whose last attempt is before before, and returns the number it
	// deleted.
	PruneDeliveries(ctx context.Context, before time.Time) (int64, error)
}

// DeliverySteps returns the step webhook_deliveries, which asks p to prune
// the delivery records whose last attempt is more than DeliveryAge before
// the clock's time; p's error is the step's. With no pruner the step does
// nothing and succeeds.
func DeliverySteps(p DeliveryPruner, clock func() time.Time) map[string]Step {
	return map[string]Step{
		"webhook_deliveries": func(ctx context.Context) error {
			if p == nil {
				return nil
			}
			_, err := p.PruneDeliveries(ctx, clock().Add(-DeliveryAge))
			return err
		},
	}
}
