package hub

import (
	"context"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/metrics"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/webhook"
)

// webhookHook is the name of the start hook that binds the event webhook
// component.
const webhookHook = "webhooks"

// webhookDeliveryWork is the name of the background work that runs the
// delivery loops.
const webhookDeliveryWork = "webhook deliveries"

// webhookDeliveryGrace is how long the stop waits for the delivery loops,
// which return as soon as their context ends, abandoning the attempts in
// flight, unless a write of the store is under way.
const webhookDeliveryGrace = time.Second

// composeWebhooks adds the event webhooks: the keys of [webhooks], the
// tables of the subscriptions and their deliveries, the webhook routes, and
// the component, bound in a start hook to the store, the event component,
// which it writes its parley.webhook.* events with, the operator's keys
// composeSecrets loaded, which seal the signing secrets, the secret
// component's answer to whether a value can be stored, the hub's clock,
// log and scrubber, which learns every signing secret, the loaded
// [webhooks] settings and the hub's metrics registry, which counts every
// delivery attempt in parley_webhook_deliveries_total. It runs the delivery
// loops as background work; they are woken through the event component's
// OnCommit, which the component registers when its first loop starts.
//
// It connects the component to the hub's other parts: the delivery records
// are what the retention step webhook_deliveries prunes, the signing
// secrets are converted by the secret re-encryption after a key rotation,
// the queue depth is the source of parley_webhook_queue_depth, and a deleted
// workspace takes its subscriptions with it in its removal transaction.
//
// It is composed after the workspace, event, secret and audit parts, whose
// components and keys it hands on or registers with.
//
// The component exists from the composition on (Webhooks); tests change its
// dependencies with AdjustWebhooks.
func composeWebhooks(c *Composition) {
	comp := &webhook.Component{}
	c.webhooks = comp
	if err := webhook.Register(c.registry); err != nil {
		c.errs = append(c.errs, err)
	}
	c.AddSchemaStep(webhook.SchemaStep)
	for _, r := range comp.Routes() {
		c.AddRoute(r)
	}
	c.RegisterDeliveryPruner(comp)
	c.secrets.RegisterConverter(comp.ConvertSecrets)
	if err := c.metrics.RegisterSource(metrics.WebhookQueueDepth, queueDepth(comp.QueueDepth)); err != nil {
		c.errs = append(c.errs, err)
	}
	// Always: the workspace service names the event webhooks among the
	// owners that register their own remover (ADR 07).
	c.workspaces.Teardown().RegisterRemover(comp.RemoveWorkspace)
	c.AddStartHook(webhookHook, func(_ context.Context, rt *Runtime) error {
		d := webhook.Deps{
			Store:     rt.Store,
			Events:    c.events,
			Keys:      func() secret.Keys { return c.secretKeys },
			Available: c.secrets.Available,
			Clock:     event.SystemClock(rt.Now),
			Logger:    rt.Logger,
			Settings:  webhook.SettingsFrom(rt.Config),
			Recorder:  c.metrics,
			Scrubber:  rt.Scrubber,
		}
		for _, adjust := range c.webhookAdjusters {
			adjust(&d)
		}
		comp.Bind(d)
		return nil
	})
	c.AddBackgroundWork(webhookDeliveryWork, webhookDeliveryGrace, func(ctx context.Context, _ *Runtime) { comp.Run(ctx) })
}

// Webhooks returns the hub's event webhook component, which serves once its
// start hook has run.
func (c *Composition) Webhooks() *webhook.Component { return c.webhooks }

// AdjustWebhooks adds fn to the functions that change the event webhook
// component's dependencies after the start hook has built them and before
// it binds them, in the order they were added.
func (c *Composition) AdjustWebhooks(fn func(*webhook.Deps)) {
	c.webhookAdjusters = append(c.webhookAdjusters, fn)
}

// queueDepth is the source of parley_webhook_queue_depth: the events the
// event webhooks have yet to deliver, as depth counts them.
func queueDepth(depth func(context.Context) (int, error)) metrics.Source {
	return func(ctx context.Context) ([]metrics.Sample, error) {
		n, err := depth(ctx)
		if err != nil {
			return nil, err
		}
		return []metrics.Sample{{Value: float64(n)}}, nil
	}
}
