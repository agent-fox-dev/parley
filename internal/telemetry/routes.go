// Package telemetry stores what agents report about their runs: run
// events, session outcomes, tool calls, tool errors and trace records,
// validated and stored once under their id in the analytical store, and
// the one postmortem of a workspace and run, so that a run can be audited
// later. Run events and trace records are also taken in batches.
//
// Every ingestion route is a POST under
// /workspaces/:slug/runs/:run_id/, built by one gate (ingest) that needs the
// scope audit:write and write access to the workspace, checks the run id of
// the path, validates the body and refuses an archived workspace, in that
// order. Ingestion writes no hub event: telemetry is never part of the
// event log.
package telemetry

import (
	"log/slog"
	"time"

	"github.com/agent-fox-dev/parley/internal/server"
)

// Deps are what the component works with: the analytical store, the clock
// (nil for time.Now), the log (nil discards it) and the time one write may
// take ([audit].write_timeout; zero for DefaultWriteTimeout).
type Deps struct {
	DB           DB
	Now          func() time.Time
	Logger       *slog.Logger
	WriteTimeout time.Duration
}

// Component is the telemetry feature. The hub creates its zero value while
// it composes, registers its schema steps and routes, and binds it in a
// start hook.
type Component struct {
	deps Deps
}

// Bind gives the component its dependencies, filling the defaults of the
// optional ones.
func (c *Component) Bind(d Deps) {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.WriteTimeout <= 0 {
		d.WriteTimeout = DefaultWriteTimeout
	}
	c.deps = d
}

// Routes are the ingestion routes, for the server's router.
func (c *Component) Routes() []server.Route {
	return []server.Route{
		c.ingest("/events", true, single(postRecord(c, runEvents, c.readRunEvent))),
		c.ingest("/sessions/outcomes", true, single(postRecord(c, outcomes, c.readOutcome))),
		c.ingest("/tools/calls", true, single(postRecord(c, toolCalls, c.readToolCall))),
		c.ingest("/tools/errors", true, single(postRecord(c, toolErrors, c.readToolError))),
		c.ingest("/traces", true, single(postRecord(c, traces, c.readTrace))),
		c.ingest("/postmortem", true, single(c.postPostmortem)),
		c.ingest("/events/batch", false, batch(c, runEvents, c.readRunEvent)),
		c.ingest("/traces/batch", false, batch(c, traces, c.readTrace)),
	}
}
