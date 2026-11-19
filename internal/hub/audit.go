package hub

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/audit"
)

// auditHook is the name of the start hook that binds the audit query.
const auditHook = "audit"

// composeAudit adds the audit query, GET /audit, bound in a start hook to
// the event component, whose visibility rule decides the hub events a
// caller sees, the telemetry component, which reads the run events, the
// access rule, whose readable set confines them, and the workspace records,
// which tell a deleted task fork from one that exists. It is composed after
// the event and telemetry parts, whose components it reads.
func composeAudit(c *Composition) {
	comp := &audit.Component{}
	for _, r := range comp.Routes() {
		c.AddRoute(r)
	}
	c.AddStartHook(auditHook, func(_ context.Context, rt *Runtime) error {
		comp.Bind(audit.Deps{
			Hub:    c.events,
			Agent:  c.telemetry,
			Access: c.access,
			Forks:  access.StoreRecords(rt.Store.Main()),
		})
		return nil
	})
}
