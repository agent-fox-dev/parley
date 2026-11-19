package hub

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/telemetry"
)

// telemetryHook is the name of the start hook that binds the telemetry
// component.
const telemetryHook = "telemetry"

// composeTelemetry adds the ingestion of agent telemetry: the schema of the
// telemetry records in the analytical store, the ingestion routes, and the
// component, bound in a start hook to the analytical store and the hub's
// clock and log. The routes rely on the pipeline's scope check and
// workspace access check, which attach the access decision they read.
//
// It registers no workspace remover: telemetry outlives its workspace.
//
// The component exists from the composition on (Telemetry); tests change
// its dependencies with AdjustTelemetry.
func composeTelemetry(c *Composition) {
	comp := &telemetry.Component{}
	c.telemetry = comp
	for _, step := range telemetry.SchemaSteps {
		c.AddSchemaStep(step)
	}
	for _, r := range comp.Routes() {
		c.AddRoute(r)
	}
	c.AddStartHook(telemetryHook, func(_ context.Context, rt *Runtime) error {
		d := telemetry.Deps{
			DB:     telemetry.SQL(rt.Store.Analytics()),
			Now:    rt.Now,
			Logger: rt.Logger,
		}
		for _, adjust := range c.telemetryAdjusters {
			adjust(&d)
		}
		comp.Bind(d)
		return nil
	})
}

// Telemetry returns the hub's telemetry component, which serves once its
// start hook has run.
func (c *Composition) Telemetry() *telemetry.Component { return c.telemetry }

// AdjustTelemetry adds fn to the functions that change the telemetry
// component's dependencies after the start hook has built them and before
// it binds them, in the order they were added.
func (c *Composition) AdjustTelemetry(fn func(*telemetry.Deps)) {
	c.telemetryAdjusters = append(c.telemetryAdjusters, fn)
}
