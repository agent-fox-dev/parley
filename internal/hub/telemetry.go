package hub

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/telemetry"
)

// telemetryHook is the name of the start hook that binds the telemetry
// component.
const telemetryHook = "telemetry"

// composeTelemetry adds the ingestion of agent telemetry: the schema of the
// telemetry records in the analytical store, the ingestion routes, and the
// component, bound in a start hook to the analytical store, the hub's
// clock and log, and the write limit [audit].write_timeout, whose key it
// registers, and the session component, whose run ids the recorder of
// sandbox use reads. The routes rely on the pipeline's scope check and
// workspace access check, which attach the access decision they read.
//
// It registers the recorder of sandbox use with the sandbox component as
// an operation observer: each exec, attach, upload and download that ended
// in a sandbox whose session belongs to a run becomes a run event. The
// recorder logs its own failures and the observer reports none, so the
// operation's outcome stays the operation's. It is composed after the
// session part, whose component it reads.
//
// It registers no workspace remover: telemetry outlives its workspace.
//
// The component exists from the composition on (Telemetry), so that
// another part, such as the metrics of the audit records, registers its run
// event observers with Telemetry().OnRunEvent while it composes; tests
// change its dependencies with AdjustTelemetry.
func composeTelemetry(c *Composition) {
	comp := &telemetry.Component{}
	c.telemetry = comp
	c.RegisterKey(telemetry.ConfigKey())
	for _, step := range telemetry.SchemaSteps {
		c.AddSchemaStep(step)
	}
	for _, r := range comp.Routes() {
		c.AddRoute(r)
	}
	c.sandbox.RegisterObserver(func(ctx context.Context, op sandbox.Operation) error {
		if rec, ok := sandboxUseOf(op); ok {
			comp.RecordSandboxUse(ctx, rec)
		}
		return nil
	})
	c.AddStartHook(telemetryHook, func(_ context.Context, rt *Runtime) error {
		d := telemetry.Deps{
			DB:           telemetry.SQL(rt.Store.Analytics()),
			Now:          rt.Now,
			Logger:       rt.Logger,
			WriteTimeout: telemetry.WriteTimeoutFrom(rt.Config),
			Sessions:     c.sessions,
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

// sandboxUseOf is the operation op of the sandbox component as the
// recorder of sandbox use takes it: what the operation was and who did it.
// The result of an exec is not part of it. ok is false for an operation of
// a kind the recorder does not record.
func sandboxUseOf(op sandbox.Operation) (rec telemetry.SandboxOperation, ok bool) {
	u := telemetry.SandboxUse{
		Workspace: op.Workspace, SandboxID: op.SandboxID, ActorType: op.ActorType, ActorID: op.ActorID, StartedAt: op.StartedAt,
	}
	if op.SessionID != nil {
		u.SessionID = *op.SessionID
	}
	switch op.Kind {
	case sandbox.OperationExec:
		return telemetry.SandboxExec{SandboxUse: u, Command: op.Command, ExitCode: op.ExitCode, TimedOut: op.TimedOut, DurationMS: op.DurationMS}, true
	case sandbox.OperationAttach:
		return telemetry.SandboxAttach{SandboxUse: u, DurationMS: op.DurationMS}, true
	case sandbox.OperationUpload:
		return telemetry.SandboxUpload{SandboxUse: u, Path: op.Path, Bytes: op.Bytes}, true
	case sandbox.OperationDownload:
		return telemetry.SandboxDownload{SandboxUse: u, Path: op.Path, Bytes: op.Bytes}, true
	}
	return nil, false
}
