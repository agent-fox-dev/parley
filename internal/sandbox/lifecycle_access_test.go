package sandbox_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
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
