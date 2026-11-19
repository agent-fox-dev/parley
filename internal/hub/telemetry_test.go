package hub_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/telemetry"
)

func TestSandboxUseOf_HandsTheRecorderWhatAnOperationWasAndNothingItCarried(t *testing.T) {
	started := time.Date(2026, 1, 2, 3, 0, 0, 250_000_000, time.UTC)
	session, exit := "0a1b2c3d-0000-4000-8000-0000000000a1", 2
	base := sandbox.Operation{
		SandboxID: "sbx1", Workspace: "demo--task1", SessionID: &session, ActorType: "api_key", ActorID: "u1",
		StartedAt: started, DurationMS: 900,
	}
	use := telemetry.SandboxUse{Workspace: "demo--task1", SandboxID: "sbx1", SessionID: session, ActorType: "api_key", ActorID: "u1", StartedAt: started}
	with := func(fn func(op *sandbox.Operation)) sandbox.Operation {
		op := base
		fn(&op)
		return op
	}
	cases := []struct {
		name string
		op   sandbox.Operation
		want telemetry.SandboxOperation
	}{
		{"an exec, without its result", with(func(op *sandbox.Operation) {
			op.Kind, op.Command, op.ExitCode, op.Result = sandbox.OperationExec, []string{"go", "test"}, &exit, sandbox.ExecResultNonzero
		}), telemetry.SandboxExec{SandboxUse: use, Command: []string{"go", "test"}, ExitCode: &exit, DurationMS: 900}},
		{"an exec killed at its timeout", with(func(op *sandbox.Operation) {
			op.Kind, op.Command, op.TimedOut, op.Result = sandbox.OperationExec, []string{"sleep"}, true, sandbox.ExecResultTimeout
		}), telemetry.SandboxExec{SandboxUse: use, Command: []string{"sleep"}, TimedOut: true, DurationMS: 900}},
		{"an attach", with(func(op *sandbox.Operation) { op.Kind = sandbox.OperationAttach }),
			telemetry.SandboxAttach{SandboxUse: use, DurationMS: 900}},
		{"an upload", with(func(op *sandbox.Operation) { op.Kind, op.Path, op.Bytes = sandbox.OperationUpload, "/work/a.txt", 12 }),
			telemetry.SandboxUpload{SandboxUse: use, Path: "/work/a.txt", Bytes: 12}},
		{"a download", with(func(op *sandbox.Operation) { op.Kind, op.Path, op.Bytes = sandbox.OperationDownload, "/work/a.txt", 12 }),
			telemetry.SandboxDownload{SandboxUse: use, Path: "/work/a.txt", Bytes: 12}},
		{"an operation of a sandbox without a session", with(func(op *sandbox.Operation) { op.Kind, op.SessionID = sandbox.OperationAttach, nil }),
			telemetry.SandboxAttach{SandboxUse: func() telemetry.SandboxUse { u := use; u.SessionID = ""; return u }(), DurationMS: 900}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := hub.SandboxUseOf(c.op)
			if !ok || !reflect.DeepEqual(got, c.want) {
				t.Errorf("SandboxUseOf = %#v, %v; want %#v, true", got, ok, c.want)
			}
		})
	}
	t.Run("an operation of an unknown kind", func(t *testing.T) {
		if got, ok := hub.SandboxUseOf(with(func(op *sandbox.Operation) { op.Kind = "logs" })); ok {
			t.Errorf("SandboxUseOf = %#v, true; want nothing", got)
		}
	})
}
