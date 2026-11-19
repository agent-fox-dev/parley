package sandbox_test

import (
	"context"
	"database/sql"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// The delete route's behaviours share one hub: each row has a sandbox of
// its own. The rows that fail the runtime change it for the whole hub, so
// no row runs in parallel.
func TestDeleteRoute(t *testing.T) {
	t.Parallel()
	h := startCheckpointHub(t)
	path := func(id string) string { return "/workspaces/demo--t1/sandboxes/" + id }
	// The request comes a minute after the sandbox was made, so that the
	// deletion's times are the request's.
	h.clock.Advance(time.Minute)
	// The delete hooks record each sandbox they ran for with its state as
	// the hook's transaction reads it.
	var (
		mu     sync.Mutex
		hooked = map[string][]string{}
	)
	h.svc.RegisterDeleteHook(func(ctx context.Context, tx *sql.Tx, id string) error {
		rec, err := sandbox.Get(ctx, tx, id)
		if err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		hooked[id] = append(hooked[id], rec.State)
		return nil
	})
	hookRuns := func(id string) []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(hooked[id])
	}
	// checkpointsSince is the number of checkpoint events about the sandbox
	// id after the first n events about it.
	checkpointsSince := func(t *testing.T, id string, n int) int {
		types := h.eventTypes(t, id)[n:]
		return len(slices.DeleteFunc(types, func(typ string) bool { return typ != sandbox.EventCheckpoint }))
	}

	t.Run("deletes the sandbox and what the runtime holds of it and revokes its hub credential", func(t *testing.T) {
		for _, state := range []string{sandbox.StateProvisioning, sandbox.StateReady, sandbox.StateStopped, sandbox.StateCompleted, sandbox.StateFailed} {
			t.Run(state, func(t *testing.T) {
				id, cred := h.runtimeSandbox(t, state, nil)

				a := h.call(t, http.MethodDelete, path(id), h.key, nil)
				if a.status != http.StatusNoContent || len(a.body) != 0 {
					t.Fatalf("DELETE = %d %q; want 204 with no body", a.status, a.body)
				}
				g := decoded(t, h.call(t, http.MethodGet, path(id), h.key, nil), http.StatusOK)
				if g["state"] != sandbox.StateDeleted || g["state_reason"] != "api" || g["ended_at"] != stamp(h.clock.Now()) {
					t.Errorf("the sandbox reads %v (%v), ended at %v; want deleted for api at %s", g["state"], g["state_reason"], g["ended_at"], stamp(h.clock.Now()))
				}
				evs := h.sandboxEvents(t, sandbox.EventDelete, id)
				if len(evs) != 1 || len(evs[0].metadata) != 1 || evs[0].metadata["reason"] != "api" || evs[0].actorType != "api_key" ||
					evs[0].actorID == nil || *evs[0].actorID != h.dana {
					t.Errorf("the delete events = %+v; want one {reason: api} by dana", evs)
				}
				if left := h.runtimeLeft(t, id); len(left) != 0 {
					t.Errorf("the runtime still holds %v", left)
				}
				if c := h.credentialAnswer(t, cred); c.status != http.StatusUnauthorized || c.err.ErrorType != "unauthenticated" {
					t.Errorf("a request with the hub credential = %d %s; want 401 unauthenticated", c.status, c.body)
				}
				if runs := hookRuns(id); !slices.Equal(runs, []string{sandbox.StateDeleted}) {
					t.Errorf("the delete hooks ran for the sandbox seeing the states %v; want once, in the transaction that deleted it", runs)
				}
				types := h.eventTypes(t, id)
				if state == sandbox.StateReady {
					if !slices.Equal(types, []string{sandbox.EventCheckpoint, sandbox.EventDelete}) {
						t.Errorf("the events about the sandbox are %v; want a checkpoint, then the delete", types)
					}
				} else if !slices.Equal(types, []string{sandbox.EventDelete}) {
					t.Errorf("the events about the sandbox are %v; want the delete alone", types)
				}
			})
		}
	})

	t.Run("takes a checkpoint as the query or the profile asks", func(t *testing.T) {
		for _, tc := range []struct {
			name, state, query string
			profile            bool
			status             int
			taken              bool
		}{
			{name: "a ready sandbox whose profile asks for one", state: sandbox.StateReady, profile: true, status: http.StatusNoContent, taken: true},
			{name: "a ready sandbox whose profile does not", state: sandbox.StateReady, status: http.StatusNoContent},
			{name: "checkpoint=false over the profile", state: sandbox.StateReady, profile: true, query: "?checkpoint=false", status: http.StatusNoContent},
			{name: "checkpoint=true over the profile", state: sandbox.StateReady, query: "?checkpoint=true", status: http.StatusNoContent, taken: true},
			{name: "checkpoint=true for a stopped sandbox", state: sandbox.StateStopped, profile: true, query: "?checkpoint=true", status: http.StatusNoContent},
			{name: "checkpoint=maybe", state: sandbox.StateReady, profile: true, query: "?checkpoint=maybe", status: http.StatusBadRequest},
		} {
			t.Run(tc.name, func(t *testing.T) {
				id, _ := h.runtimeSandbox(t, tc.state, func(r *sandbox.Record) { r.Snapshot.Checkpoint = tc.profile })
				events := len(h.eventTypes(t, id))

				a := h.call(t, http.MethodDelete, path(id)+tc.query, h.key, nil)
				if a.status != tc.status {
					t.Fatalf("DELETE%s = %d %s; want %d", tc.query, a.status, a.body, tc.status)
				}
				want := 0
				if tc.taken {
					want = 1
				}
				if n := checkpointsSince(t, id, events); n != want {
					t.Errorf("the deletion wrote %d checkpoint events; want %d", n, want)
				}
				rec := storedRecord(t, h.Hub, id)
				if tc.status == http.StatusBadRequest {
					if a.err.ErrorType != "invalid_request" || rec.State != tc.state || len(h.runtimeLeft(t, id)) == 0 {
						t.Errorf("DELETE%s = %s, the sandbox is %s with %v at the runtime; want invalid_request and the sandbox kept", tc.query, a.body, rec.State, h.runtimeLeft(t, id))
					}
					return
				}
				if rec.State != sandbox.StateDeleted {
					t.Errorf("the sandbox is %s; want it deleted", rec.State)
				}
			})
		}
	})

	t.Run("answers a sandbox that is already deleted as a deletion", func(t *testing.T) {
		id, _ := h.runtimeSandbox(t, sandbox.StateReady, func(r *sandbox.Record) { r.Snapshot.Checkpoint = false })
		if a := h.call(t, http.MethodDelete, path(id), h.key, nil); a.status != http.StatusNoContent {
			t.Fatalf("the first DELETE = %d %s; want 204", a.status, a.body)
		}
		first := storedRecord(t, h.Hub, id)
		removed := h.seedRecord(t, "demo--t1", sandbox.StateDeleted, nil)
		h.clock.Advance(time.Minute)
		for _, id := range []string{id, removed} {
			calls := len(h.rt.Calls())
			events := len(h.eventTypes(t, id))
			if a := h.call(t, http.MethodDelete, path(id), h.key, nil); a.status != http.StatusNoContent || len(a.body) != 0 {
				t.Errorf("DELETE of the deleted sandbox %s = %d %q; want 204 with no body", id, a.status, a.body)
			}
			if ops := h.sandboxCalls(id, calls); len(ops) != 0 {
				t.Errorf("the runtime was called for the deleted sandbox: %v", ops)
			}
			if types := h.eventTypes(t, id)[events:]; len(types) != 0 {
				t.Errorf("the second deletion wrote the events %v", types)
			}
		}
		if again := storedRecord(t, h.Hub, id); again.EndedAt == nil || !again.EndedAt.Equal(*first.EndedAt) || !again.UpdatedAt.Equal(first.UpdatedAt) {
			t.Errorf("the sandbox ended at %v, updated at %v; want those of the first deletion, %v and %v", again.EndedAt, again.UpdatedAt, first.EndedAt, first.UpdatedAt)
		}
		if n := len(h.sandboxEvents(t, sandbox.EventDelete, id)); n != 1 {
			t.Errorf("%d delete events; want 1", n)
		}
	})

	t.Run("records the deletion when the runtime fails and leaves its remains to reconciliation", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			// set makes the runtime fail and returns what ends that.
			set func() func()
			// unreachable is true for the row whose checkpoint fails too.
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
				id, cred := h.runtimeSandbox(t, sandbox.StateReady, nil)
				end := tc.set()
				a := h.call(t, http.MethodDelete, path(id), h.key, nil)
				end()
				if a.status != http.StatusNoContent {
					t.Fatalf("DELETE = %d %s; want 204", a.status, a.body)
				}
				rec := storedRecord(t, h.Hub, id)
				if rec.State != sandbox.StateDeleted || rec.StateReason != "api" {
					t.Errorf("the sandbox is %s (%q); want it deleted for api", rec.State, rec.StateReason)
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
				// What the runtime still holds is the reconciliation's.
				if left := h.runtimeLeft(t, id); !slices.Contains(left, id) {
					t.Errorf("the runtime holds %v; want the sandbox it could not delete still there", left)
				}
			})
		}
	})
}
