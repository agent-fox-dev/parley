package sandbox_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// The stop route's behaviours share one hub: each row has a sandbox of its
// own. The rows that fail the runtime change it for the whole hub, so no
// row runs in parallel.
func TestStopRoute(t *testing.T) {
	t.Parallel()
	h := startCheckpointHub(t)
	path := func(id string) string { return "/workspaces/demo--t1/sandboxes/" + id + "/stop" }
	// The request comes a minute after the sandbox was made, so that the
	// stop's times are the request's.
	h.clock.Advance(time.Minute)
	type stopped struct {
		ID          string                   `json:"id"`
		State       string                   `json:"state"`
		StateReason *string                  `json:"state_reason"`
		UpdatedAt   time.Time                `json:"updated_at"`
		Checkpoint  *sandbox.CheckpointState `json:"checkpoint"`
	}

	t.Run("stops a ready sandbox, keeps its working directory and revokes its hub credential", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			checkpoint bool
			admin      bool
		}{
			{name: "with a checkpoint, by dana", checkpoint: true},
			{name: "without a checkpoint, by dana"},
			{name: "without a checkpoint, by the admin token", admin: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				id, dir := h.workSandbox(t, func(r *sandbox.Record) { r.Snapshot.Checkpoint = tc.checkpoint })
				if tc.checkpoint {
					h.cloneWork(t, dir)
				} else if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				writeWork(t, dir, map[string]string{"notes.txt": "unsaved\n"})
				cred := h.hubCredential(t, id)
				caller := h.key
				if tc.admin {
					caller = h.Token
				}
				calls := len(h.rt.Calls())

				a := h.call(t, http.MethodPost, path(id), caller, nil)
				var got stopped
				if a.status != http.StatusOK || json.Unmarshal(a.body, &got) != nil || got.ID != id ||
					got.State != sandbox.StateStopped || got.StateReason == nil || *got.StateReason != "api" {
					t.Fatalf("POST stop = %d %s; want 200 with the sandbox stopped for api", a.status, a.body)
				}
				ops := h.sandboxCalls(id, calls)
				if n := len(ops); n == 0 || ops[n-1] != runtime.OpStopSandbox || slices.Contains(ops, runtime.OpDeleteSandbox) ||
					strings.Count(strings.Join(ops, " "), runtime.OpStopSandbox) != 1 {
					t.Errorf("the runtime calls of the sandbox = %v; want them to end in its one stop, with no delete", ops)
				}
				if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
					t.Errorf("the working directory lost its file: %v", err)
				}
				if c := h.credentialAnswer(t, cred); c.status != http.StatusUnauthorized || c.err.ErrorType != "unauthenticated" {
					t.Errorf("a request with the hub credential = %d %s; want 401 unauthenticated", c.status, c.body)
				}
				evs := h.sandboxEvents(t, sandbox.EventStop, id)
				wantType, wantID := "api_key", &h.dana
				if tc.admin {
					wantType, wantID = "admin_token", nil
				}
				if len(evs) != 1 || evs[0].metadata["reason"] != "api" || len(evs[0].metadata) != 1 || evs[0].actorType != wantType ||
					(wantID == nil) != (evs[0].actorID == nil) || (wantID != nil && *evs[0].actorID != *wantID) {
					t.Errorf("the stop events = %+v; want one {reason: api} by %s", evs, wantType)
				}
				cps := h.sandboxEvents(t, sandbox.EventCheckpoint, id)
				if !tc.checkpoint {
					if got.Checkpoint != nil || len(cps) != 0 {
						t.Errorf("checkpoint = %+v with %d events; want null and none", got.Checkpoint, len(cps))
					}
					return
				}
				if cp := got.Checkpoint; cp == nil || !cp.OK || !cp.At.Equal(h.clock.Now()) || len(cp.Refs) != 1 || !cp.Refs[0].Dirty {
					t.Errorf("checkpoint = %+v; want ok at %v with the dirty ref of t1", cp, h.clock.Now())
				}
				if len(cps) != 1 || ops[0] != runtime.OpExec {
					t.Errorf("%d checkpoint events, runtime calls %v; want one checkpoint, its commands before the stop", len(cps), ops)
				}
			})
		}
	})

	t.Run("answers a sandbox that is not ready as it is", func(t *testing.T) {
		for _, state := range []string{sandbox.StateProvisioning, sandbox.StateStopped, sandbox.StateCompleted, sandbox.StateFailed, sandbox.StateDeleted} {
			t.Run(state, func(t *testing.T) {
				id := h.seedRecord(t, "demo--t1", state, func(r *sandbox.Record) {
					r.HubAccess = &sandbox.HubAccess{Scopes: allHubScopes, ExpiresAt: r.ExpiresAt}
					r.Snapshot.Checkpoint = true
				})
				h.hubCredential(t, id)
				before := storedRecord(t, h.Hub, id)
				calls := len(h.rt.Calls())

				a := h.call(t, http.MethodPost, path(id), h.key, nil)
				var got stopped
				if a.status != http.StatusOK || json.Unmarshal(a.body, &got) != nil || got.State != state || !got.UpdatedAt.Equal(before.UpdatedAt) {
					t.Fatalf("POST stop = %d %s; want 200 with the sandbox %s as it was, updated at %v", a.status, a.body, state, before.UpdatedAt)
				}
				if ops := h.sandboxCalls(id, calls); len(ops) != 0 {
					t.Errorf("the runtime was called for the sandbox: %v", ops)
				}
				if n := h.count(t, `SELECT count(*) FROM hub_events WHERE resource_id = ?`, id); n != 0 {
					t.Errorf("%d events about the sandbox; want none", n)
				}
				if pat, err := identity.PATByID(context.Background(), h.Store.Main(), before.HubTokenID); err != nil || pat.RevokedAt != nil {
					t.Errorf("the hub credential: %+v, %v; want it live", pat, err)
				}
			})
		}
	})

	t.Run("leaves the sandbox ready when the runtime fails the stop", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			// set makes the runtime fail, with the hub credential cred in
			// the runtime's text, and returns what ends that.
			set func(cred string) func()
		}{
			{name: "the runtime cannot be reached", set: func(string) func() {
				h.rt.SetUnreachable(true)
				return func() { h.rt.SetUnreachable(false) }
			}},
			{name: "the runtime refuses the stop", set: func(cred string) func() {
				h.rt.FailOperation(runtime.OpStopSandbox, runtime.ErrInvalid, "refused for "+cred)
				return func() { h.rt.FailOperation(runtime.OpStopSandbox, nil, "") }
			}},
			{name: "the runtime does not support a stop", set: func(string) func() {
				h.rt.FailOperation(runtime.OpStopSandbox, runtime.ErrUnsupported, "stop is not supported")
				return func() { h.rt.FailOperation(runtime.OpStopSandbox, nil, "") }
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				id, _ := h.workSandbox(t, nil)
				cred := h.hubCredential(t, id)
				t.Cleanup(tc.set(cred))

				a := h.call(t, http.MethodPost, path(id), h.key, nil)
				if a.status != http.StatusBadGateway || a.err.ErrorType != "sandbox_runtime_error" || strings.Contains(string(a.body), cred) {
					t.Fatalf("POST stop = %d %s; want 502 sandbox_runtime_error without the hub credential", a.status, a.body)
				}
				if rec := storedRecord(t, h.Hub, id); rec.State != sandbox.StateReady || rec.StateReason != "" {
					t.Errorf("the sandbox is %s (%q); want it ready", rec.State, rec.StateReason)
				}
				if evs := h.sandboxEvents(t, sandbox.EventStop, id); len(evs) != 0 {
					t.Errorf("the stop events = %+v; want none", evs)
				}
				if c := h.credentialAnswer(t, cred); c.status != http.StatusOK {
					t.Errorf("a request with the hub credential = %d %s; want 200", c.status, c.body)
				}
			})
		}
	})
}
