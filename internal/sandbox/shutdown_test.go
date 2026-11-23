package sandbox_test

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hubtest"

	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// The background work is stopped while each of its passes is in the middle
// of a checkpoint, the longest step before a runtime call that ends a
// sandbox: the idle stop of one sandbox, the expiry of another and the
// recording of a third's exit. Every command in a sandbox waits for its
// context to end.
func TestShutdown_EndsTheBackgroundWorkWithoutTouchingTheRuntimeOrARecord(t *testing.T) {
	t.Parallel()
	h := startCheckpointHub(t)
	// The hub's own loops are waiting for the clock, which does not move, so
	// that what follows is the work this test starts and stops.
	hubtest.Eventually(t, "the hub's loops to wait a minute", func() bool { return h.timers.created(time.Minute) == 2 })
	h.hooks.set(func(ctx context.Context, op, _ string) error {
		if op != runtime.OpExec {
			return nil
		}
		<-ctx.Done()
		return runtime.NewError(runtime.ErrUnavailable, op, ctx.Err().Error())
	})
	ids := map[string]string{
		"provisioning": h.seedRecord(t, "demo--t1", sandbox.StateProvisioning, nil),
		"stopped":      h.seedRecord(t, "demo--t1", sandbox.StateStopped, nil),
		"completed":    h.seedRecord(t, "demo--t1", sandbox.StateCompleted, func(r *sandbox.Record) { r.ExitCode = new(0) }),
	}
	ids["ready, idle"], _ = h.workSandbox(t, func(r *sandbox.Record) {
		r.IdleTimeout = 30 * time.Minute
		r.LastActivityAt = recordStart.Add(-time.Hour)
	})
	ids["ready, expired"], _ = h.workSandbox(t, func(r *sandbox.Record) { r.ExpiresAt = recordStart })
	ids["ready, main process ended"], _ = h.workSandbox(t, nil)
	h.rt.EndMain("parley", ids["ready, main process ended"], 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var work sync.WaitGroup
	// The lifecycle loop's first iteration deletes the expired sandbox. The
	// idle pass and the reconciliation pass stand for the next tick of their
	// loops, which would wake the hub's own loops too.
	work.Go(func() { h.svc.RunLifecycle(ctx) })
	work.Go(func() { h.svc.RunReconcile(ctx) })
	work.Go(func() { _ = h.svc.IdlePass(ctx) })
	work.Go(func() { _ = h.svc.Reconcile(ctx) })
	busy := []string{ids["ready, idle"], ids["ready, expired"], ids["ready, main process ended"]}
	hubtest.Eventually(t, "a checkpoint of each pass to run a command", func() bool {
		for _, id := range busy {
			if len(h.hooks.runs(id)) == 0 {
				return false
			}
		}
		return true
	})
	before := map[string]sandbox.Record{}
	for _, id := range ids {
		before[id] = storedRecord(t, h.Hub, id)
	}
	calls, commands := len(h.rt.Calls()), map[string]int{}
	for _, id := range busy {
		commands[id] = len(h.hooks.runs(id))
	}

	stopped := time.Now()
	cancel()
	done := make(chan struct{})
	go func() { work.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the background work did not return within 15 s of its context's end")
	}

	for _, c := range h.rt.Calls()[calls:] {
		switch c.Op {
		case runtime.OpStopSandbox, runtime.OpDeleteSandbox, runtime.OpStartSandbox, runtime.OpCreateSandbox, runtime.OpRunMain,
			runtime.OpExec, runtime.OpExecStream, runtime.OpExecInteractive, runtime.OpUpload, runtime.OpMakeDir:
			t.Errorf("the runtime received %s %s after the background work was told to stop", c.Op, c.Target)
		default:
			if strings.Contains(c.Op, "provider") {
				t.Errorf("the runtime received %s %s after the background work was told to stop", c.Op, c.Target)
			}
		}
	}
	for _, id := range busy {
		if n := len(h.hooks.runs(id)) - commands[id]; n != 0 {
			t.Errorf("%d more commands were run in %s after the background work was told to stop", n, id)
		}
	}
	for what, id := range ids {
		if rec := storedRecord(t, h.Hub, id); !reflect.DeepEqual(rec, before[id]) {
			t.Errorf("the %s sandbox changed:\n got %+v\nwant %+v", what, rec, before[id])
		}
	}
	t.Logf("the background work returned %v after its context ended", time.Since(stopped))
}
