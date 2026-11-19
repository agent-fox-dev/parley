package sandbox_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/runtimetest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestLifecycleRoutes_TheAdminTokenStopsAndDeletesASandbox(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile)
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error {
		return workspace.Insert(ctx, tx, h.record("demo", access.KindRepository, workspace.StatusReady))
	})
	id := h.seedRecord(t, "demo", sandbox.StateReady, nil)
	spec := runtime.SandboxSpec{Name: id, WorkDir: sandbox.WorkDir, Labels: map[string]string{sandbox.LabelSandbox: id}}
	if _, err := h.rt.Client("parley", nil).CreateSandbox(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	path := "/workspaces/demo/sandboxes/" + id

	a := h.call(t, http.MethodPost, path+"/stop", h.Token, nil)
	var got struct {
		State string `json:"state"`
	}
	if a.status != http.StatusOK || json.Unmarshal(a.body, &got) != nil || got.State != sandbox.StateStopped {
		t.Fatalf("POST stop with the admin token = %d %s; want 200 with the sandbox stopped", a.status, a.body)
	}
	if a := h.call(t, http.MethodDelete, path, h.Token, nil); a.status != http.StatusNoContent {
		t.Fatalf("DELETE with the admin token = %d %s; want 204", a.status, a.body)
	}
	for _, typ := range []string{sandbox.EventStop, sandbox.EventDelete} {
		if evs := h.sandboxEvents(t, typ, id); len(evs) != 1 || evs[0].actorType != "admin_token" || evs[0].actorID != nil {
			t.Errorf("the %s events = %+v; want one by the admin token", typ, evs)
		}
	}
	if state := storedRecord(t, h.Hub, id).State; state != sandbox.StateDeleted {
		t.Errorf("the sandbox is %s; want it deleted", state)
	}
}

func TestLifecycleRoutes_RefuseACallerWhoMayNotChangeTheSandbox(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile)
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error {
		for _, slug := range []string{"demo", "other"} {
			if err := workspace.Insert(ctx, tx, h.record(slug, access.KindRepository, workspace.StatusReady)); err != nil {
				return err
			}
		}
		return nil
	})
	member := h.CreateUser(t, "member")
	h.AddMember(t, h.org, member)
	ready := h.seedRecord(t, "demo", sandbox.StateReady, nil)
	stopped := h.seedRecord(t, "demo", sandbox.StateStopped, nil)
	foreign := h.seedRecord(t, "other", sandbox.StateReady, nil)
	removed := h.seedRecord(t, "demo", sandbox.StateDeleted, nil)
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error { return sandbox.Remove(ctx, tx, removed) })
	off := startProbeHub(t, false)
	unknown := "sbx_" + strings.Repeat("0", 32)
	readPAT := h.pat(t, h.dana, "sandboxes:read")
	memberKey := h.IssueKey(t, member)
	outsiderKey := h.IssueKey(t, h.CreateUser(t, "outsider"))

	routes := []struct {
		name, method, suffix string
		// target is the sandbox the route would change: the start's is
		// stopped, the others' ready.
		target string
		body   any
	}{
		{"checkpoint", http.MethodPost, "/checkpoint", ready, nil},
		{"stop", http.MethodPost, "/stop", ready, nil},
		{"start", http.MethodPost, "/start", stopped, nil},
		{"delete", http.MethodDelete, "", ready, nil},
		{"extend", http.MethodPatch, "", ready, map[string]any{"ttl": "1h"}},
	}
	// state is what a refused request must leave as it was: the records,
	// the hub events and the runtime calls, but the log collector's.
	state := func(hub *probeHub) (map[string]sandbox.Record, int, int) {
		records := map[string]sandbox.Record{}
		if hub == h.probeHub {
			for _, id := range []string{ready, stopped, foreign} {
				records[id] = storedRecord(t, h.Hub, id)
			}
		}
		calls := 0
		if hub.rt != nil {
			calls = len(slices.DeleteFunc(hub.rt.Calls(), func(c runtimetest.Call) bool { return c.Op == runtime.OpLogs }))
		}
		var events int
		if err := hub.Store.Main().QueryRow(`SELECT count(*) FROM hub_events`).Scan(&events); err != nil {
			t.Fatal(err)
		}
		return records, events, calls
	}
	for _, route := range routes {
		for _, tc := range []struct {
			name      string
			hub       *probeHub
			slug, id  string
			cred      string
			status    int
			errorType string
		}{
			{"sandboxes disabled", off, "ws", unknown, off.IssueKey(t, off.owner), http.StatusNotFound, "sandboxes_disabled"},
			{"a PAT with sandboxes:read only", h.probeHub, "demo", route.target, readPAT, http.StatusForbidden, "insufficient_scope"},
			{"a member with read access", h.probeHub, "demo", route.target, memberKey, http.StatusForbidden, "forbidden"},
			{"a user outside the organization", h.probeHub, "demo", route.target, outsiderKey, http.StatusNotFound, "not_found"},
			{"an unknown id", h.probeHub, "demo", unknown, h.key, http.StatusNotFound, "sandbox_not_found"},
			{"the id of another workspace's sandbox", h.probeHub, "demo", foreign, h.key, http.StatusNotFound, "sandbox_not_found"},
			{"the id of a removed record", h.probeHub, "demo", removed, h.key, http.StatusNotFound, "sandbox_not_found"},
		} {
			t.Run(route.name+"/"+tc.name, func(t *testing.T) {
				records, events, calls := state(tc.hub)
				a := tc.hub.call(t, route.method, "/workspaces/"+tc.slug+"/sandboxes/"+tc.id+route.suffix, tc.cred, route.body)
				if a.status != tc.status || a.err.ErrorType != tc.errorType {
					t.Fatalf("%s = %d %s; want %d %s", route.method, a.status, a.body, tc.status, tc.errorType)
				}
				afterRecords, afterEvents, afterCalls := state(tc.hub)
				if !reflect.DeepEqual(afterRecords, records) {
					t.Errorf("the records became %+v; want %+v", afterRecords, records)
				}
				if afterEvents != events || afterCalls != calls {
					t.Errorf("%d events and %d runtime calls were added; want none", afterEvents-events, afterCalls-calls)
				}
			})
		}
	}
}
