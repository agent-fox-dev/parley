package sandbox_test

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hubtest"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// The idle and expiry passes and the loop that runs them share one hub,
// on a clock that stays at t0 until the loop's rows advance it; each row
// has a sandbox of its own. The rows that fail the runtime change it for
// the whole hub, so no row runs in parallel. The reconciliation, which
// runs the passes too, waits longer than the rows advance the clock.
func TestLifecycleTimers(t *testing.T) {
	t.Parallel()
	h := startCheckpointHubWith(t, "reconcile_interval = \"10m\"\n")
	ctx := context.Background()
	t0 := h.clock.Now()
	// demo--gone expires at t0 only once its row needs it, so that the
	// fork sweep does not remove it first.
	h.fork(t, "demo--gone", "demo", t0.Add(7*24*time.Hour), nil)
	h.fork(t, "demo--late", "demo", t0.Add(time.Second), nil)
	// The loop leaves to the minute's pass a sandbox expired at or before
	// its last expiry pass, but takes at once one it has not passed over
	// yet. A sandbox expired at t0, and a transition that wakes the loop,
	// make it pass at t0 now, so that it takes none of the rows' sandboxes
	// expired at t0 away from the passes the rows run themselves.
	primer := h.seedRecord(t, "demo", sandbox.StateReady, func(r *sandbox.Record) { r.ExpiresAt = t0 })
	if _, err := h.svc.Transition(ctx, nil, primer, sandbox.StateReady, sandbox.StateStopped, "", nil); err != nil {
		t.Fatalf("stop the sandbox that wakes the loop: %v", err)
	}
	hubtest.Eventually(t, "the loop's expiry pass at t0", func() bool { return storedRecord(t, h.Hub, primer).State == sandbox.StateDeleted })
	// on moves a sandbox to the workspace slug, outside any session.
	on := func(slug string) func(*sandbox.Record) {
		return func(r *sandbox.Record) { r.Workspace, r.SessionID = slug, nil }
	}
	// dirty gives the sandbox id a clone of demo with an uncommitted file.
	dirty := func(t *testing.T, id string) {
		dir := h.workDir(id)
		h.cloneWork(t, dir)
		writeWork(t, dir, map[string]string{"notes.txt": "unsaved\n"})
	}
	// before reports whether the event typ about the sandbox id comes
	// before the event next.
	before := func(t *testing.T, id, typ, next string) bool {
		types := h.eventTypes(t, id)
		i, j := slices.Index(types, typ), slices.Index(types, next)
		return i >= 0 && j > i
	}
	// bySystem reports whether evs is one event by the system for reason.
	bySystem := func(evs []hubEvent, reason string) bool {
		return len(evs) == 1 && evs[0].actorType == "system" && evs[0].actorID == nil && evs[0].metadata["reason"] == reason
	}
	refsIn := func(t *testing.T, slug, id string) map[string]string {
		return gitrepotest.Refs(t, h.workspaces.RepoPath(slug), sandbox.CheckpointRefPrefix+id+"/")
	}

	t.Run("the idle pass stops a ready sandbox idle for longer than its idle timeout", func(t *testing.T) {
		rows := []struct {
			name  string
			idle  time.Duration
			since time.Duration
			state string
			stop  bool
		}{
			{"idle a second longer than its timeout", 30 * time.Minute, 1801 * time.Second, sandbox.StateReady, true},
			{"idle exactly its timeout", 30 * time.Minute, 1800 * time.Second, sandbox.StateReady, false},
			{"without an idle timeout", 0, 100000 * time.Second, sandbox.StateReady, false},
			{"provisioning", 30 * time.Minute, 4000 * time.Second, sandbox.StateProvisioning, false},
			{"completed", 30 * time.Minute, 4000 * time.Second, sandbox.StateCompleted, false},
		}
		ids, creds := make([]string, len(rows)), make([]string, len(rows))
		for i, row := range rows {
			ids[i], creds[i] = h.runtimeSandbox(t, row.state, func(r *sandbox.Record) {
				r.IdleTimeout, r.LastActivityAt, r.Snapshot.Checkpoint = row.idle, t0.Add(-row.since), row.stop
			})
			if row.stop {
				dirty(t, ids[i])
			}
		}
		if err := h.svc.IdlePass(ctx); err != nil {
			t.Fatalf("IdlePass: %v", err)
		}
		for i, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				id := ids[i]
				rec := storedRecord(t, h.Hub, id)
				stops := h.sandboxEvents(t, sandbox.EventStop, id)
				if !row.stop {
					if rec.State != row.state || len(stops) != 0 {
						t.Errorf("the sandbox is %s with %d stop events; want it %s as it was", rec.State, len(stops), row.state)
					}
					return
				}
				if rec.State != sandbox.StateStopped || rec.StateReason != sandbox.StopIdle {
					t.Errorf("the sandbox is %s (%q); want it stopped for idle", rec.State, rec.StateReason)
				}
				if !bySystem(stops, "idle") {
					t.Errorf("the stop events = %+v; want one {reason: idle} by the system", stops)
				}
				if !before(t, id, sandbox.EventCheckpoint, sandbox.EventStop) {
					t.Errorf("the events = %v; want a checkpoint before the stop", h.eventTypes(t, id))
				}
				if c := h.credentialAnswer(t, creds[i]); c.status != http.StatusUnauthorized {
					t.Errorf("a request with the hub credential = %d %s; want 401", c.status, c.body)
				}
			})
		}
	})

	// later expires a second after t0 and outlives the expiry passes at
	// t0, until the loop's timer reaches it.
	later, _ := h.runtimeSandbox(t, sandbox.StateReady, func(r *sandbox.Record) {
		r.ExpiresAt, r.Snapshot.Checkpoint = t0.Add(time.Second), false
	})

	t.Run("the expiry pass deletes every live sandbox at its expires_at", func(t *testing.T) {
		states := []string{sandbox.StateProvisioning, sandbox.StateReady, sandbox.StateStopped, sandbox.StateCompleted, sandbox.StateFailed}
		ids, creds := make([]string, len(states)), make([]string, len(states))
		for i, state := range states {
			ids[i], creds[i] = h.runtimeSandbox(t, state, func(r *sandbox.Record) { r.ExpiresAt = t0 })
			if state == sandbox.StateReady {
				dirty(t, ids[i])
			}
		}
		if err := h.svc.ExpiryPass(ctx); err != nil {
			t.Fatalf("ExpiryPass: %v", err)
		}
		for i, state := range states {
			t.Run(state, func(t *testing.T) {
				id := ids[i]
				rec := storedRecord(t, h.Hub, id)
				if rec.State != sandbox.StateDeleted || rec.StateReason != sandbox.DeleteExpired || rec.EndedAt == nil || !rec.EndedAt.Equal(t0) {
					t.Errorf("the sandbox is %s (%q), ended at %v; want it deleted for expired at %v", rec.State, rec.StateReason, rec.EndedAt, t0)
				}
				if evs := h.sandboxEvents(t, sandbox.EventDelete, id); !bySystem(evs, "expired") {
					t.Errorf("the delete events = %+v; want one {reason: expired} by the system", evs)
				}
				if left := h.runtimeLeft(t, id); len(left) != 0 {
					t.Errorf("the runtime still holds %v", left)
				}
				if c := h.credentialAnswer(t, creds[i]); c.status != http.StatusUnauthorized {
					t.Errorf("a request with the hub credential = %d %s; want 401", c.status, c.body)
				}
				if state != sandbox.StateReady {
					return
				}
				if cp := h.storedCheckpoint(t, id); cp == nil || !cp.OK || len(cp.Refs) != 1 || !cp.Refs[0].Dirty || len(h.checkpointRefs(t, id)) != 1 {
					t.Errorf("the checkpoint = %+v with refs %v; want the dirty ref of t1", cp, h.checkpointRefs(t, id))
				}
				if !before(t, id, sandbox.EventCheckpoint, sandbox.EventDelete) {
					t.Errorf("the events = %v; want a checkpoint before the deletion", h.eventTypes(t, id))
				}
			})
		}
		if rec := storedRecord(t, h.Hub, later); rec.State != sandbox.StateReady || len(h.sandboxEvents(t, sandbox.EventDelete, later)) != 0 {
			t.Errorf("the sandbox expiring a second later is %s; want it ready and untouched", rec.State)
		}
	})

	t.Run("the expiry pass deletes the sandbox of an expired fork without a checkpoint", func(t *testing.T) {
		gone, _ := h.runtimeSandbox(t, sandbox.StateReady, func(r *sandbox.Record) { on("demo--gone")(r); r.ExpiresAt = t0 })
		dirty(t, gone)
		live, _ := h.runtimeSandbox(t, sandbox.StateReady, func(r *sandbox.Record) { on("demo--late")(r); r.ExpiresAt = t0 })
		dirty(t, live)
		h.exec(t, `UPDATE workspaces SET expires_at = ? WHERE slug = 'demo--gone'`, t0.UnixNano())
		if err := h.svc.ExpiryPass(ctx); err != nil {
			t.Fatalf("ExpiryPass: %v", err)
		}
		for _, tc := range []struct {
			name, id, slug, reason string
			checkpoint             bool
		}{
			{"the fork expired too", gone, "demo--gone", sandbox.DeleteWorkspaceDeleted, false},
			{"the fork expires a second later", live, "demo--late", sandbox.DeleteExpired, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if rec := storedRecord(t, h.Hub, tc.id); rec.State != sandbox.StateDeleted || rec.StateReason != tc.reason {
					t.Errorf("the sandbox is %s (%q); want it deleted for %s", rec.State, rec.StateReason, tc.reason)
				}
				if evs := h.sandboxEvents(t, sandbox.EventDelete, tc.id); !bySystem(evs, tc.reason) {
					t.Errorf("the delete events = %+v; want one {reason: %s} by the system", evs, tc.reason)
				}
				cps, refs := h.sandboxEvents(t, sandbox.EventCheckpoint, tc.id), refsIn(t, tc.slug, tc.id)
				if tc.checkpoint != (len(cps) == 1) || tc.checkpoint != (len(refs) == 1) {
					t.Errorf("%d checkpoint events, refs %v; want a checkpoint: %t", len(cps), refs, tc.checkpoint)
				}
			})
		}
	})

	t.Run("the expiry pass records the deletion when the runtime fails and leaves its remains to reconciliation", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			// set makes the runtime fail and returns what ends that.
			set         func() func()
			unreachable bool
		}{
			{name: "the runtime cannot be reached", unreachable: true, set: func() func() {
				h.rt.SetUnreachable(true)
				return func() { h.rt.SetUnreachable(false) }
			}},
			{name: "the runtime fails the delete", set: func() func() {
				h.rt.FailOperation(runtime.OpDeleteSandbox, runtime.ErrInvalid, "the sandbox is busy")
				return func() { h.rt.FailOperation(runtime.OpDeleteSandbox, nil, "") }
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				id, cred := h.runtimeSandbox(t, sandbox.StateReady, func(r *sandbox.Record) { r.ExpiresAt = t0 })
				end := tc.set()
				err := h.svc.ExpiryPass(ctx)
				end()
				if err != nil {
					t.Fatalf("ExpiryPass: %v", err)
				}
				if rec := storedRecord(t, h.Hub, id); rec.State != sandbox.StateDeleted || rec.StateReason != sandbox.DeleteExpired {
					t.Errorf("the sandbox is %s (%q); want it deleted for expired", rec.State, rec.StateReason)
				}
				if n := len(h.sandboxEvents(t, sandbox.EventDelete, id)); n != 1 {
					t.Errorf("%d delete events; want 1", n)
				}
				if c := h.credentialAnswer(t, cred); c.status != http.StatusUnauthorized {
					t.Errorf("a request with the hub credential = %d %s; want 401", c.status, c.body)
				}
				if cp := h.storedCheckpoint(t, id); tc.unreachable && (cp == nil || cp.OK || cp.Error == nil) {
					t.Errorf("the stored checkpoint = %+v; want the failed attempt with its reason", cp)
				}
				if left := h.runtimeLeft(t, id); !slices.Contains(left, id) {
					t.Errorf("the runtime holds %v; want the sandbox it could not delete still there", left)
				}
			})
		}
	})

	t.Run("the loop's timer runs the expiry pass at the earliest expires_at", func(t *testing.T) {
		hubtest.Eventually(t, "the loop to arm its timer a second ahead", func() bool { return h.timers.live(time.Second) > 0 })
		ticks := h.timers.created(sandbox.LifecycleInterval)
		h.clock.Advance(time.Second)
		hubtest.Eventually(t, "the sandbox to expire", func() bool { return storedRecord(t, h.Hub, later).State == sandbox.StateDeleted })
		if evs := h.sandboxEvents(t, sandbox.EventDelete, later); !bySystem(evs, "expired") {
			t.Errorf("the delete events = %+v; want one {reason: expired} by the system", evs)
		}
		if n := h.timers.created(sandbox.LifecycleInterval); n != ticks {
			t.Errorf("the loop ticked %d times; want the timer alone to run the pass", n-ticks)
		}

		// The loop arms its timer for the next expires_at, that of the idle
		// rows, before the rows below make one.
		hubtest.Eventually(t, "the loop to arm its timer for the next expires_at", func() bool {
			return h.timers.created(t0.Add(time.Hour).Sub(h.clock.Now())) > 0
		})
		// A new ttl moves the timer to the new expires_at; a refused one
		// does not move it to the sandbox's expires_at, which is the
		// earliest.
		id := h.seedRecord(t, "demo", sandbox.StateReady, func(r *sandbox.Record) {
			r.Snapshot.TTL, r.ExpiresAt = 4*time.Hour, t0.Add(30*time.Minute)
		})
		untilStored := t0.Add(30 * time.Minute).Sub(h.clock.Now())
		armed := h.timers.created(2 * time.Minute)
		path := "/workspaces/demo/sandboxes/" + id
		if a := h.call(t, http.MethodPatch, path, h.key, map[string]any{"ttl": "59s"}); a.status != http.StatusBadRequest {
			t.Fatalf("PATCH of a ttl under a minute = %d %s; want 400", a.status, a.body)
		}
		if a := h.call(t, http.MethodPatch, path, h.key, map[string]any{"ttl": "2m"}); a.status != http.StatusOK {
			t.Fatalf("PATCH = %d %s; want 200", a.status, a.body)
		}
		hubtest.Eventually(t, "the loop to arm its timer for the new expires_at", func() bool { return h.timers.created(2*time.Minute) > armed })
		if n := h.timers.created(untilStored); n != 0 {
			t.Errorf("the loop armed %d timers for the expires_at a refused change left; want none", n)
		}
	})

	t.Run("the loop runs the idle pass every minute and tries a failed stop again", func(t *testing.T) {
		id, _ := h.runtimeSandbox(t, sandbox.StateReady, func(r *sandbox.Record) {
			r.IdleTimeout, r.LastActivityAt, r.Snapshot.Checkpoint = 30*time.Minute, t0.Add(-4000*time.Second), false
		})
		stops := func() int {
			return len(slices.DeleteFunc(h.sandboxCalls(id, 0), func(op string) bool { return op != runtime.OpStopSandbox }))
		}
		h.rt.FailOperation(runtime.OpStopSandbox, runtime.ErrInvalid, "the runtime refuses the stop")
		for pass := 1; pass <= 3; pass++ {
			h.clock.Advance(sandbox.LifecycleInterval)
			hubtest.Eventually(t, "the minute's idle pass", func() bool { return stops() == pass })
			if rec := storedRecord(t, h.Hub, id); rec.State != sandbox.StateReady {
				t.Fatalf("after the failed stop of pass %d the sandbox is %s; want it ready", pass, rec.State)
			}
		}
		h.rt.FailOperation(runtime.OpStopSandbox, nil, "")
		h.clock.Advance(sandbox.LifecycleInterval)
		hubtest.Eventually(t, "the next pass to stop the sandbox", func() bool { return storedRecord(t, h.Hub, id).State == sandbox.StateStopped })
		if rec := storedRecord(t, h.Hub, id); rec.StateReason != sandbox.StopIdle || stops() != 4 {
			t.Errorf("the sandbox is stopped for %q after %d stop calls; want idle after 4", rec.StateReason, stops())
		}
	})
}
