package sandbox_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// The reconciliation's rows share one hub, whose provisioner is held and
// whose clock moves only when a row moves it; each row has sandboxes of its
// own. Every pass looks at every sandbox, so a row leaves the records and
// the runtime agreeing for the rows after it. The rows that make the
// runtime fail change it for the whole hub, so no row runs in parallel, and
// the restart comes last.
func TestReconcile(t *testing.T) {
	t.Parallel()
	h := startCheckpointHub(t)
	ctx := context.Background()
	fake := h.rt.Client("parley", nil)
	pass := func(t *testing.T) {
		t.Helper()
		if err := h.svc.Reconcile(ctx); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	// passes is the number of passes so far: each lists the providers
	// once, and nothing else does while no sandbox is deleted.
	passes := func() int {
		n := 0
		for _, c := range h.rt.Calls() {
			if c.Op == runtime.OpListProviders {
				n++
			}
		}
		return n
	}
	// sandboxTypes are the types of the sandbox events about id, oldest
	// first.
	sandboxTypes := func(t *testing.T, id string) []string {
		return slices.DeleteFunc(h.eventTypes(t, id), func(typ string) bool { return !strings.HasPrefix(typ, "parley.sandbox.") })
	}
	noCheckpoint := func(r *sandbox.Record) { r.Snapshot.Checkpoint = false }
	unchanged := func(t *testing.T, id string, before sandbox.Record) {
		t.Helper()
		if rec := storedRecord(t, h.Hub, id); !reflect.DeepEqual(rec, before) {
			t.Errorf("the record changed:\n got %+v\nwant %+v", rec, before)
		}
	}
	lose := func(t *testing.T, id string) {
		t.Helper()
		if err := fake.DeleteSandbox(ctx, id); err != nil {
			t.Fatalf("remove the sandbox from the runtime: %v", err)
		}
	}

	t.Run("the loop runs a pass every reconcile_interval on the hub's clock after the pass of the start", func(t *testing.T) {
		if n := passes(); n != 1 {
			t.Fatalf("%d passes before the clock moved; want the one of the start", n)
		}
		// The lifecycle loop and the reconciliation each wait on a timer of
		// a minute, and each asks for the next once its pass has begun.
		eventually(t, "both loops to wait a minute", func() bool { return h.timers.created(time.Minute) == 2 })
		for i := 1; i <= 3; i++ {
			armed := h.timers.created(time.Minute)
			h.clock.Advance(time.Minute)
			eventually(t, fmt.Sprintf("pass %d of the loop", i), func() bool {
				return passes() == 1+i && h.timers.created(time.Minute) == armed+2
			})
		}
		if n := passes(); n != 4 {
			t.Errorf("%d passes after 3 minutes; want 3 after the one of the start", n-1)
		}
	})

	t.Run("records the end of a ready sandbox's main process", func(t *testing.T) {
		rows := []struct {
			code          int
			state, reason string
		}{
			{0, sandbox.StateCompleted, ""},
			{1, sandbox.StateFailed, sandbox.ReasonExit},
			{137, sandbox.StateFailed, sandbox.ReasonExit},
		}
		ids, creds := make([]string, len(rows)), make([]string, len(rows))
		for i, row := range rows {
			ids[i], creds[i] = h.runtimeSandbox(t, sandbox.StateReady, nil)
			dir := h.workDir(ids[i])
			h.cloneWork(t, dir)
			writeWork(t, dir, map[string]string{"notes.txt": "unsaved\n"})
			h.rt.EndMain("parley", ids[i], row.code)
		}
		calls := len(h.rt.Calls())
		pass(t)
		now := h.clock.Now()
		for i, row := range rows {
			t.Run(fmt.Sprintf("exit code %d", row.code), func(t *testing.T) {
				id := ids[i]
				rec := storedRecord(t, h.Hub, id)
				if rec.State != row.state || rec.StateReason != row.reason || rec.ExitCode == nil || *rec.ExitCode != row.code ||
					rec.EndedAt == nil || !rec.EndedAt.Equal(now) {
					t.Errorf("the sandbox is %s (%q), exit code %v, ended at %v; want %s (%q), exit code %d, ended at %v",
						rec.State, rec.StateReason, rec.ExitCode, rec.EndedAt, row.state, row.reason, row.code, now)
				}
				if types := sandboxTypes(t, id); !slices.Equal(types, []string{sandbox.EventCheckpoint, sandbox.EventExit}) {
					t.Errorf("the sandbox's events = %v; want a checkpoint, then the exit", types)
				}
				exits := h.sandboxEvents(t, sandbox.EventExit, id)
				if len(exits) != 1 || exits[0].actorType != "system" || exits[0].actorID != nil ||
					!reflect.DeepEqual(exits[0].metadata, map[string]any{"exit_code": float64(row.code)}) {
					t.Errorf("the exit events = %+v; want one {exit_code: %d} by the system", exits, row.code)
				}
				if cp := h.storedCheckpoint(t, id); cp == nil || !cp.OK || len(cp.Refs) != 1 || !cp.Refs[0].Dirty {
					t.Errorf("the checkpoint = %+v; want the dirty ref of t1", cp)
				}
				ops := h.sandboxCalls(id, calls)
				if !slices.Contains(ops, runtime.OpStopSandbox) || slices.Contains(ops, runtime.OpDeleteSandbox) {
					t.Errorf("the runtime was asked for %v; want a stop and no deletion", ops)
				}
				if st, err := fake.GetSandbox(ctx, id); err != nil || !st.WorkDirPresent {
					t.Errorf("the runtime's sandbox = %+v, %v; want it with its working directory", st, err)
				}
				if _, err := os.Stat(filepath.Join(h.workDir(id), "notes.txt")); err != nil {
					t.Errorf("the uncommitted file: %v; want it still in the working directory", err)
				}
				if c := h.credentialAnswer(t, creds[i]); c.status != http.StatusUnauthorized {
					t.Errorf("a request with the hub credential = %d %s; want 401", c.status, c.body)
				}
			})
		}
		before := map[string]int{}
		for _, id := range ids {
			before[id] = len(h.eventTypes(t, id))
		}
		pass(t)
		for _, id := range ids {
			if n := len(h.eventTypes(t, id)); n != before[id] {
				t.Errorf("a second pass wrote %d events about %s; want none", n-before[id], id)
			}
		}
	})

	t.Run("follows the runtime's pause of a ready sandbox without an event and leaves a start's provisioning alone", func(t *testing.T) {
		id, cred := h.runtimeSandbox(t, sandbox.StateReady, nil)
		// started waits for the provisioning of its start, which the hub
		// holds, at a runtime that runs it.
		started, _ := h.runtimeSandbox(t, sandbox.StateProvisioning, func(r *sandbox.Record) {
			r.Marker, r.RuntimeCreated = sandbox.MarkerStart, true
		})
		h.rt.SetState("parley", started, runtime.StateRunning)
		events := func() int { return len(h.eventTypes(t, id)) + len(h.eventTypes(t, started)) }
		before := events()

		h.rt.SetState("parley", id, runtime.StateRestarting)
		pass(t)
		if rec := storedRecord(t, h.Hub, id); rec.State != sandbox.StateProvisioning || !rec.RuntimeProvisioning {
			t.Errorf("while the runtime restarts it the sandbox is %s (runtime's: %t); want provisioning, put there by the runtime",
				rec.State, rec.RuntimeProvisioning)
		}
		if c := h.credentialAnswer(t, cred); c.status != http.StatusOK {
			t.Errorf("a request with the hub credential while the runtime restarts the sandbox = %d %s; want 200", c.status, c.body)
		}

		h.rt.SetState("parley", id, runtime.StateRunning)
		pass(t)
		if rec := storedRecord(t, h.Hub, id); rec.State != sandbox.StateReady || rec.RuntimeProvisioning {
			t.Errorf("once the runtime runs it again the sandbox is %s (runtime's: %t); want ready", rec.State, rec.RuntimeProvisioning)
		}
		if c := h.credentialAnswer(t, cred); c.status != http.StatusOK {
			t.Errorf("a request with the hub credential after the restart = %d %s; want 200", c.status, c.body)
		}
		if n := events() - before; n != 0 {
			t.Errorf("the passes wrote %d events; want none", n)
		}
		if rec := storedRecord(t, h.Hub, started); rec.State != sandbox.StateProvisioning || rec.Marker != sandbox.MarkerStart {
			t.Errorf("the started sandbox is %s with marker %q; want it provisioning for its start", rec.State, rec.Marker)
		}
	})

	t.Run("fails a sandbox the runtime has lost and leaves those that have no compute there", func(t *testing.T) {
		created := func(r *sandbox.Record) { r.Marker, r.RuntimeCreated = sandbox.MarkerCreate, true }
		rows := []struct {
			name, state string
			edit        func(*sandbox.Record)
			fail        bool
		}{
			{"ready", sandbox.StateReady, nil, true},
			{"provisioning, created at the runtime", sandbox.StateProvisioning, created, true},
			{"provisioning, not yet created at the runtime", sandbox.StateProvisioning, func(r *sandbox.Record) { r.Marker = sandbox.MarkerCreate }, false},
			{"stopped", sandbox.StateStopped, nil, false},
			{"completed", sandbox.StateCompleted, nil, false},
			{"failed for its exit", sandbox.StateFailed, func(r *sandbox.Record) { r.StateReason = sandbox.ReasonExit }, false},
		}
		ids, creds := make([]string, len(rows)), make([]string, len(rows))
		before, events := make([]sandbox.Record, len(rows)), make([]int, len(rows))
		for i, row := range rows {
			ids[i], creds[i] = h.runtimeSandbox(t, row.state, row.edit)
			lose(t, ids[i])
			before[i], events[i] = storedRecord(t, h.Hub, ids[i]), len(h.eventTypes(t, ids[i]))
		}
		pass(t)
		now := h.clock.Now()
		for i, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				id := ids[i]
				if !row.fail {
					unchanged(t, id, before[i])
					if n := len(h.eventTypes(t, id)) - events[i]; n != 0 {
						t.Errorf("the pass wrote %d events; want none", n)
					}
					return
				}
				rec := storedRecord(t, h.Hub, id)
				if rec.State != sandbox.StateFailed || rec.StateReason != sandbox.ReasonRuntimeError || rec.EndedAt == nil || !rec.EndedAt.Equal(now) {
					t.Errorf("the sandbox is %s (%q), ended at %v; want failed (runtime_error) at %v", rec.State, rec.StateReason, rec.EndedAt, now)
				}
				fails := h.sandboxEvents(t, sandbox.EventFail, id)
				if len(fails) != 1 || fails[0].actorType != "system" || fails[0].actorID != nil || fails[0].metadata["reason"] != sandbox.ReasonRuntimeError {
					t.Fatalf("the fail events = %+v; want one {reason: runtime_error} by the system", fails)
				}
				if text, _ := fails[0].metadata["error"].(string); text == "" || strings.Contains(text, creds[i]) {
					t.Errorf("the fail event's error = %q; want the reason without the hub credential", fails[0].metadata["error"])
				}
				if c := h.credentialAnswer(t, creds[i]); c.status != http.StatusUnauthorized {
					t.Errorf("a request with the hub credential = %d %s; want 401", c.status, c.body)
				}
			})
		}
	})

	t.Run("deletes what the runtime holds of no live sandbox and nothing else", func(t *testing.T) {
		live, _ := h.runtimeSandbox(t, sandbox.StateReady, nil)
		gone := h.seedRecord(t, "demo--t1", sandbox.StateDeleted, nil)
		unknown := sandbox.NewID()
		for _, id := range []string{gone, unknown} {
			labels := map[string]string{sandbox.LabelSandbox: id}
			if _, err := fake.CreateSandbox(ctx, runtime.SandboxSpec{Name: id, WorkDir: sandbox.WorkDir, Labels: labels}); err != nil {
				t.Fatal(err)
			}
			if err := fake.EnsureProvider(ctx, runtime.Provider{Name: id + "-model", Profile: "anthropic", Value: "v", Labels: labels}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := fake.CreateSandbox(ctx, runtime.SandboxSpec{Name: "another-tool", WorkDir: sandbox.WorkDir}); err != nil {
			t.Fatal(err)
		}
		if err := fake.EnsureProvider(ctx, runtime.Provider{Name: "another-tool-key", Profile: "anthropic", Value: "v"}); err != nil {
			t.Fatal(err)
		}
		liveBefore, goneBefore := storedRecord(t, h.Hub, live), storedRecord(t, h.Hub, gone)
		pass(t)
		for _, id := range []string{gone, unknown} {
			if left := h.runtimeLeft(t, id); len(left) != 0 {
				t.Errorf("the runtime still holds %v of a sandbox without a live record", left)
			}
		}
		if left := h.runtimeLeft(t, live); !slices.Equal(left, []string{live, live + "-model"}) {
			t.Errorf("the runtime holds %v of the live sandbox; want its sandbox and provider", left)
		}
		if _, ok := h.rt.Spec("parley", "another-tool"); !ok {
			t.Error("the pass deleted a sandbox without the hub's label")
		}
		if _, ok := h.rt.Provider("parley", "another-tool-key"); !ok {
			t.Error("the pass deleted a provider without the hub's label")
		}
		unchanged(t, live, liveBefore)
		unchanged(t, gone, goneBefore)
	})

	t.Run("deletes what a deletion left at a runtime that could not delete it, once the runtime can", func(t *testing.T) {
		byRequest, _ := h.runtimeSandbox(t, sandbox.StateReady, noCheckpoint)
		h.rt.SetUnreachable(true)
		err := h.svc.Delete(ctx, storedRecord(t, h.Hub, byRequest), sandbox.DeleteAPI, sandbox.SystemActor, false)
		h.rt.SetUnreachable(false)
		if err != nil {
			t.Fatalf("Delete: %v", err)
		}
		// The runtime fails the deletion before the sandbox exists, so that
		// the lifecycle loop, which may find it expired first, leaves the
		// same remains.
		h.rt.FailOperation(runtime.OpDeleteSandbox, runtime.ErrInvalid, "the sandbox is busy")
		expired, _ := h.runtimeSandbox(t, sandbox.StateReady, func(r *sandbox.Record) {
			r.ExpiresAt, r.Snapshot.Checkpoint = h.clock.Now(), false
		})
		err = h.svc.ExpiryPass(ctx)
		h.rt.FailOperation(runtime.OpDeleteSandbox, nil, "")
		if err != nil {
			t.Fatalf("ExpiryPass: %v", err)
		}
		for _, id := range []string{byRequest, expired} {
			if rec := storedRecord(t, h.Hub, id); rec.State != sandbox.StateDeleted || len(h.runtimeLeft(t, id)) == 0 {
				t.Fatalf("before the pass the sandbox is %s and the runtime holds %v; want it deleted with remains", rec.State, h.runtimeLeft(t, id))
			}
		}
		pass(t)
		for _, id := range []string{byRequest, expired} {
			if left := h.runtimeLeft(t, id); len(left) != 0 {
				t.Errorf("after one pass the runtime still holds %v", left)
			}
		}
	})

	t.Run("changes nothing but by expiry while the runtime cannot be reached, and catches up once it can", func(t *testing.T) {
		exited, _ := h.runtimeSandbox(t, sandbox.StateReady, noCheckpoint)
		h.rt.EndMain("parley", exited, 0)
		lost, _ := h.runtimeSandbox(t, sandbox.StateReady, nil)
		lose(t, lost)
		ids := []string{exited, lost}
		before := []sandbox.Record{storedRecord(t, h.Hub, exited), storedRecord(t, h.Hub, lost)}

		h.rt.SetUnreachable(true)
		if err := h.svc.ReconcileAtStart(ctx); err != nil {
			t.Errorf("the start's pass = %v; want nil", err)
		}
		err := h.svc.Reconcile(ctx)
		h.rt.SetUnreachable(false)
		if err != nil {
			t.Fatalf("Reconcile with the runtime unreachable: %v", err)
		}
		for i, id := range ids {
			unchanged(t, id, before[i])
			for _, typ := range []string{sandbox.EventFail, sandbox.EventExit, sandbox.EventStop} {
				if evs := h.sandboxEvents(t, typ, id); len(evs) != 0 {
					t.Errorf("%s events about %s = %+v; want none", typ, id, evs)
				}
			}
		}

		pass(t)
		if rec := storedRecord(t, h.Hub, exited); rec.State != sandbox.StateCompleted {
			t.Errorf("the exited sandbox is %s once the runtime can be reached; want completed", rec.State)
		}
		if rec := storedRecord(t, h.Hub, lost); rec.State != sandbox.StateFailed || rec.StateReason != sandbox.ReasonRuntimeError {
			t.Errorf("the lost sandbox is %s (%q) once the runtime can be reached; want failed (runtime_error)", rec.State, rec.StateReason)
		}
	})

	t.Run("the pass of a restart does the idle stops and expiries that came while the hub was down", func(t *testing.T) {
		t0 := h.clock.Now()
		expiring, _ := h.runtimeSandbox(t, sandbox.StateReady, func(r *sandbox.Record) {
			r.ExpiresAt, r.Snapshot.Checkpoint = t0.Add(time.Hour), false
		})
		idle, _ := h.runtimeSandbox(t, sandbox.StateReady, func(r *sandbox.Record) {
			r.IdleTimeout, r.LastActivityAt, r.ExpiresAt, r.Snapshot.Checkpoint = 30*time.Minute, t0, t0.Add(4*time.Hour), false
		})
		// The hub is down while the clock passes both instants, so that no
		// loop of the old hub sees them.
		h.Stop()
		h.ExitCode(t, 10*time.Second)
		h.clock.Advance(2 * time.Hour)
		h.Restart(t)
		if rec := storedRecord(t, h.Hub, expiring); rec.State != sandbox.StateDeleted || rec.StateReason != sandbox.DeleteExpired {
			t.Errorf("the expired sandbox is %s (%q) after the start; want deleted (expired)", rec.State, rec.StateReason)
		}
		if rec := storedRecord(t, h.Hub, idle); rec.State != sandbox.StateStopped || rec.StateReason != sandbox.StopIdle {
			t.Errorf("the idle sandbox is %s (%q) after the start; want stopped (idle)", rec.State, rec.StateReason)
		}
	})
}
