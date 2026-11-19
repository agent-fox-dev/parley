package hub

import (
	"context"
	"errors"
	"maps"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/audit"
	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/event"
)

// auditHook is the name of the start hook that binds the audit query and
// builds the retention run.
const auditHook = "audit"

// retentionTask is the name of the maintenance task that runs the retention
// run, the first of every maintenance pass.
const retentionTask = "retention"

// composeAudit adds the audit query, GET /audit, bound in a start hook to
// the event component, whose visibility rule decides the hub events a
// caller sees, the telemetry component, which reads the run events, the
// access rule, whose readable set confines them, and the workspace records,
// which tell a deleted task fork from one that exists. It is composed after
// the event and telemetry parts, whose components it reads.
//
// It also registers the retention keys of [audit] and [sessions], checks
// right after the load that [events].retention is not longer than
// [audit].max_age, which stops the start before anything is opened, and
// registers the retention run as the maintenance task retention, built in
// the start hook from the loaded settings with its steps bound by
// retentionSteps, reporting to the hub's metrics registry.
func composeAudit(c *Composition) {
	comp := &audit.Component{}
	for _, r := range comp.Routes() {
		c.AddRoute(r)
	}
	if err := audit.Register(c.registry); err != nil {
		c.errs = append(c.errs, err)
	}
	c.addConfigCheck(func(cfg *config.Config) error {
		// The event component is bound only in its start hook, so the
		// check reads the loaded [events] section itself.
		_, err := audit.NewSettings(cfg, event.SettingsFrom(cfg).Retention)
		return err
	})
	var retention *audit.Retention
	c.AddStartHook(auditHook, func(_ context.Context, rt *Runtime) error {
		comp.Bind(audit.Deps{
			Hub:    c.events,
			Agent:  c.telemetry,
			Access: c.access,
			Forks:  access.StoreRecords(rt.Store.Main()),
		})
		settings, err := audit.NewSettings(rt.Config, c.events.Retention())
		if err != nil {
			return err
		}
		retention, err = audit.NewRetention(retentionSteps(c, rt, settings), c.metrics, rt.Logger, rt.Now)
		return err
	})
	c.AddMaintenanceTask(retentionTask, func(ctx context.Context, _ *Runtime) error {
		if retention == nil {
			return errNoRetention
		}
		return retention.Run(ctx)
	})
}

// errNoRetention is a maintenance pass before the start hook built the
// retention run, which the hub never runs: the loop starts after the hooks.
var errNoRetention = errors.New("the retention run is not built")

// retentionSteps binds every retention step to the function that does its
// clean-up with the loaded settings on the hub's clock: the session steps
// to the session store, the telemetry steps to the telemetry store, the
// event steps to the event component, webhook_deliveries to the delivery
// pruner registered with RegisterDeliveryPruner (none: the step does
// nothing), orphans to the three stores that keep records of a workspace
// after it is gone, asking the workspace records whether it is, and gauges
// to the stores it counts and the hub's metrics registry.
func retentionSteps(c *Composition, rt *Runtime, s audit.Settings) map[string]audit.Step {
	steps := make(map[string]audit.Step, len(audit.StepNames()))
	maps.Copy(steps, audit.SessionSteps(c.sessions, s, rt.Now))
	maps.Copy(steps, audit.TelemetrySteps(c.telemetry, s, rt.Now))
	maps.Copy(steps, audit.EventSteps(c.events, s, rt.Now))
	maps.Copy(steps, audit.DeliverySteps(c.deliveryPruner, rt.Now))
	maps.Copy(steps, audit.OrphanSteps(access.StoreRecords(rt.Store.Main()),
		[]audit.OrphanStore{c.sessions, c.telemetry, c.events}, s, rt.Now))
	maps.Copy(steps, audit.GaugeSteps([]audit.RecordCounter{c.events, c.telemetry, c.sessions}, c.sessions, c.metrics))
	return steps
}

// RegisterDeliveryPruner makes p the store the retention step
// webhook_deliveries prunes. The webhook part registers its delivery store
// with it while the hub is composed; the pruner is kept on the
// composition, not in a package-level registry, so that hubs in one
// process never share it (ADR 03). A second registration is refused and
// stops the start.
func (c *Composition) RegisterDeliveryPruner(p audit.DeliveryPruner) {
	if c.deliveryPruner != nil {
		c.errs = append(c.errs, errSecondPruner)
		return
	}
	c.deliveryPruner = p
}

// errSecondPruner refuses a second delivery pruner: the step prunes one
// delivery store.
var errSecondPruner = errors.New("a delivery pruner is registered already")
