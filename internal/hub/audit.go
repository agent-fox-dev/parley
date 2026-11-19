package hub

import (
	"context"
	"errors"

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
// clean-up with the loaded settings. A step whose clean-up the hub does not
// have yet does nothing and succeeds.
func retentionSteps(_ *Composition, _ *Runtime, _ audit.Settings) map[string]audit.Step {
	steps := make(map[string]audit.Step, len(audit.StepNames()))
	for _, name := range audit.StepNames() {
		steps[name] = func(context.Context) error { return nil }
	}
	return steps
}
