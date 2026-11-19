package sandbox_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The collaborators through which a workspace's archive and deletion reach
// its sandboxes share one hub whose fake runtime runs the real git in host
// directories; each part works on sandboxes, and mostly workspaces, of its
// own. The parts change the runtime for the whole hub, so none runs in
// parallel.
func TestTeardown(t *testing.T) {
	t.Parallel()
	h := startCheckpointHub(t)
	ctx := context.Background()
	purged := &idLog{}
	h.svc.RegisterPurgeHook(func(_ context.Context, _ *sql.Tx, id string) error {
		purged.add(id)
		return nil
	})
	// faulty is the sandbox whose deleting transaction fails.
	var faultMu sync.Mutex
	faulty := ""
	h.svc.RegisterDeleteHook(func(_ context.Context, _ *sql.Tx, id string) error {
		faultMu.Lock()
		defer faultMu.Unlock()
		if id == faulty {
			return errors.New("injected write fault")
		}
		return nil
	})
	on := func(slug string) func(*sandbox.Record) {
		return func(r *sandbox.Record) { r.Workspace, r.SessionID = slug, nil }
	}
	bySystem := func(evs []hubEvent, reason string) bool {
		return len(evs) == 1 && evs[0].actorType == "system" && evs[0].actorID == nil && evs[0].metadata["reason"] == reason
	}
	unchanged := func(t *testing.T, before sandbox.Record) {
		t.Helper()
		after := storedRecord(t, h.Hub, before.ID)
		if after.State != before.State || !after.UpdatedAt.Equal(before.UpdatedAt) {
			t.Errorf("the sandbox %s on %s is %s, updated at %v; want it %s as it was", before.ID, before.Workspace, after.State, after.UpdatedAt, before.State)
		}
	}
	records := func(t *testing.T, slug string) []string {
		t.Helper()
		var ids []string
		rows, err := h.Store.Main().Query(`SELECT id FROM sandboxes WHERE workspace = ? ORDER BY id`, slug)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		return ids
	}
	hasLive := func(t *testing.T, slug string) bool {
		t.Helper()
		live, err := h.svc.HasLiveSandbox(ctx, slug)
		if err != nil {
			t.Fatalf("HasLiveSandbox(%s): %v", slug, err)
		}
		return live
	}

	t.Run("the archive observer stops the ready sandboxes of the workspace and its forks", func(t *testing.T) {
		a, _ := h.runtimeSandbox(t, sandbox.StateReady, on("demo"))
		b, _ := h.runtimeSandbox(t, sandbox.StateReady, nil)
		c := storedRecord(t, h.Hub, h.seedRecord(t, "demo--t1", sandbox.StateStopped, nil))
		d := storedRecord(t, h.Hub, h.seedRecord(t, "other", sandbox.StateReady, nil))
		if err := h.svc.WorkspaceArchived(ctx, "demo", []string{"demo--t1"}); err != nil {
			t.Fatalf("WorkspaceArchived: %v", err)
		}
		for _, id := range []string{a, b} {
			rec := storedRecord(t, h.Hub, id)
			if rec.State != sandbox.StateStopped || rec.StateReason != sandbox.StopArchived || rec.Checkpoint == nil {
				t.Errorf("the sandbox %s on %s is %s (%q), checkpoint %s; want it stopped for archived with a checkpoint", id, rec.Workspace, rec.State, rec.StateReason, rec.Checkpoint)
			}
			if evs := h.sandboxEvents(t, sandbox.EventStop, id); !bySystem(evs, "archived") {
				t.Errorf("the stop events of %s = %+v; want one {reason: archived} by the system", id, evs)
			}
			types := h.eventTypes(t, id)
			if i, j := slices.Index(types, sandbox.EventCheckpoint), slices.Index(types, sandbox.EventStop); i < 0 || j < i {
				t.Errorf("the events of %s = %v; want a checkpoint before the stop", id, types)
			}
		}
		unchanged(t, c)
		unchanged(t, d)
	})

	t.Run("the archive observer stops the others when the runtime fails the stop of one and names it", func(t *testing.T) {
		// The runtime holds no sandbox for a, so it refuses its stop.
		a := h.seedRecord(t, "demo", sandbox.StateReady, nil)
		b, _ := h.runtimeSandbox(t, sandbox.StateReady, nil)
		err := h.svc.WorkspaceArchived(ctx, "demo", []string{"demo--t1"})
		if err == nil || !strings.Contains(err.Error(), a) {
			t.Fatalf("WorkspaceArchived = %v; want an error naming %s", err, a)
		}
		if rec := storedRecord(t, h.Hub, a); rec.State != sandbox.StateReady || len(h.sandboxEvents(t, sandbox.EventStop, a)) != 0 {
			t.Errorf("the sandbox whose stop failed is %s; want it ready without a stop event", rec.State)
		}
		if rec := storedRecord(t, h.Hub, b); rec.State != sandbox.StateStopped || rec.StateReason != sandbox.StopArchived {
			t.Errorf("the other sandbox is %s (%q); want it stopped for archived", rec.State, rec.StateReason)
		}
	})

	t.Run("the sandbox remover deletes every sandbox of the slugs without a checkpoint", func(t *testing.T) {
		a, _ := h.runtimeSandbox(t, sandbox.StateReady, on("demo--t2"))
		b := h.seedRecord(t, "lost", sandbox.StateProvisioning, nil)
		c := h.seedRecord(t, "lost", sandbox.StateStopped, nil)
		d := storedRecord(t, h.Hub, h.seedRecord(t, "other", sandbox.StateReady, nil))
		if !hasLive(t, "demo--t2") || !hasLive(t, "lost") {
			t.Fatalf("HasLiveSandbox = false before the removal; want true for a ready and a provisioning sandbox")
		}
		if err := h.svc.RemoveSandboxes(ctx, []string{"demo--t2", "lost"}); err != nil {
			t.Fatalf("RemoveSandboxes: %v", err)
		}
		for _, id := range []string{a, b, c} {
			rec := storedRecord(t, h.Hub, id)
			if rec.State != sandbox.StateDeleted || rec.StateReason != sandbox.DeleteWorkspaceDeleted {
				t.Errorf("the sandbox %s is %s (%q); want it deleted for workspace_deleted", id, rec.State, rec.StateReason)
			}
			if evs := h.sandboxEvents(t, sandbox.EventDelete, id); !bySystem(evs, "workspace_deleted") {
				t.Errorf("the delete events of %s = %+v; want one {reason: workspace_deleted} by the system", id, evs)
			}
			if evs := h.sandboxEvents(t, sandbox.EventCheckpoint, id); len(evs) != 0 {
				t.Errorf("the sandbox %s got %d checkpoint events; want none", id, len(evs))
			}
		}
		if left := h.runtimeLeft(t, a); len(left) != 0 {
			t.Errorf("the runtime still holds %v", left)
		}
		unchanged(t, d)
		if hasLive(t, "lost") {
			t.Errorf("HasLiveSandbox(lost) = true after the removal; want false")
		}
	})

	t.Run("the query reports whether a slug has a provisioning or ready sandbox", func(t *testing.T) {
		rows := []struct {
			state string
			live  bool
		}{
			{sandbox.StateProvisioning, true},
			{sandbox.StateReady, true},
			{sandbox.StateStopped, false},
			{sandbox.StateCompleted, false},
			{sandbox.StateFailed, false},
			{sandbox.StateDeleted, false},
			{"", false},
		}
		for _, row := range rows {
			slug := "query-" + row.state
			if row.state != "" {
				h.seedRecord(t, slug, row.state, nil)
			}
			if got := hasLive(t, slug); got != row.live {
				t.Errorf("HasLiveSandbox with a sandbox %q = %v; want %v", row.state, got, row.live)
			}
		}
	})

	t.Run("the sandbox remover records the deletion when the runtime cannot be reached", func(t *testing.T) {
		id := h.seedRecord(t, "unreachable", sandbox.StateReady, nil)
		h.rt.SetUnreachable(true)
		err := h.svc.RemoveSandboxes(ctx, []string{"unreachable"})
		h.rt.SetUnreachable(false)
		if err != nil {
			t.Fatalf("RemoveSandboxes = %v; want nil", err)
		}
		if rec := storedRecord(t, h.Hub, id); rec.State != sandbox.StateDeleted || rec.StateReason != sandbox.DeleteWorkspaceDeleted {
			t.Errorf("the sandbox is %s (%q); want it deleted for workspace_deleted", rec.State, rec.StateReason)
		}
	})

	t.Run("the sandbox remover fails when a deletion cannot be written and deletes the others", func(t *testing.T) {
		bad := h.seedRecord(t, "fault", sandbox.StateStopped, nil)
		good := h.seedRecord(t, "fault", sandbox.StateStopped, nil)
		faultMu.Lock()
		faulty = bad
		faultMu.Unlock()
		err := h.svc.RemoveSandboxes(ctx, []string{"fault"})
		if err == nil || !strings.Contains(err.Error(), bad) {
			t.Errorf("RemoveSandboxes = %v; want an error naming %s", err, bad)
		}
		if rec := storedRecord(t, h.Hub, bad); rec.State != sandbox.StateStopped {
			t.Errorf("the sandbox whose deletion failed is %s; want it stopped as it was", rec.State)
		}
		if rec := storedRecord(t, h.Hub, good); rec.State != sandbox.StateDeleted {
			t.Errorf("the other sandbox is %s; want it deleted", rec.State)
		}
	})

	t.Run("the remover of owned data removes a fork's records and checkpoint refs in the transaction", func(t *testing.T) {
		dir := h.workspaces.RepoPath("demo--t2")
		withCred := h.seedRecord(t, "demo--t2", sandbox.StateDeleted, func(r *sandbox.Record) {
			r.HubAccess = &sandbox.HubAccess{Scopes: allHubScopes, ExpiresAt: r.ExpiresAt}
		})
		cred := h.hubCredential(t, withCred)
		h.seedRecord(t, "demo--t2", sandbox.StateDeleted, nil)
		gitrepotest.SetRef(t, dir, sandbox.CheckpointRefPrefix+withCred+"/t2", h.base)
		ids := records(t, "demo--t2")
		before := purged.snapshot()
		h.insert(t, func(ctx context.Context, tx *sql.Tx) error { return h.svc.RemoveWorkspaceRecords(ctx, tx, "demo--t2") })
		if left := records(t, "demo--t2"); len(left) != 0 {
			t.Errorf("the fork still has the sandbox records %v", left)
		}
		if refs := gitrepotest.Refs(t, dir, sandbox.CheckpointRefPrefix); len(refs) != 0 {
			t.Errorf("the fork still has the checkpoint refs %v", refs)
		}
		after := purged.snapshot()
		for _, id := range ids {
			if n := after[id] - before[id]; n != 1 {
				t.Errorf("the purge hooks ran %d times for %s; want once", n, id)
			}
		}
		// The token was the sandbox's; the remover leaves tokens to the
		// workspace's token revoker.
		if a := h.call(t, http.MethodGet, "/workspaces/demo--t2/claims", cred, nil); a.status == http.StatusUnauthorized {
			t.Errorf("a request with the hub credential = %d %s; want it still authenticated", a.status, a.body)
		}
	})

	t.Run("the remover of owned data leaves the records when the transaction rolls back", func(t *testing.T) {
		h.seedRecord(t, "demo--t1", sandbox.StateDeleted, nil)
		ids := records(t, "demo--t1")
		err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
			if err := h.svc.RemoveWorkspaceRecords(ctx, tx, "demo--t1"); err != nil {
				return err
			}
			return errRollback
		})
		if !errors.Is(err, errRollback) {
			t.Fatalf("the transaction = %v; want it rolled back by the test", err)
		}
		if left := records(t, "demo--t1"); !slices.Equal(left, ids) {
			t.Errorf("the records after the rollback = %v; want %v", left, ids)
		}
	})

	t.Run("a workspace's archive and deletion and a fork's deletion reach their sandboxes", func(t *testing.T) {
		h.repository(t, "gone", workspace.StatusReady)
		h.fork(t, "gone--t1", "gone", recordStart.Add(7*24*time.Hour), nil)
		h.fork(t, "gone--t2", "gone", recordStart.Add(7*24*time.Hour), nil)
		ws, _ := h.runtimeSandbox(t, sandbox.StateReady, on("gone"))
		f1, _ := h.runtimeSandbox(t, sandbox.StateReady, on("gone--t1"))
		f2, _ := h.runtimeSandbox(t, sandbox.StateReady, on("gone--t2"))

		if a := h.call(t, http.MethodDelete, "/workspaces/gone--t2", h.Token, nil); a.status != http.StatusNoContent {
			t.Fatalf("DELETE the fork = %d %s; want 204", a.status, a.body)
		}
		if evs := h.sandboxEvents(t, sandbox.EventDelete, f2); !bySystem(evs, "workspace_deleted") {
			t.Errorf("the delete events of the fork's sandbox = %+v; want one {reason: workspace_deleted} by the system", evs)
		}
		if left := records(t, "gone--t2"); len(left) != 0 {
			t.Errorf("the deleted fork still has the sandbox records %v", left)
		}

		if a := h.call(t, http.MethodPost, "/workspaces/gone/archive", h.Token, nil); a.status != http.StatusOK {
			t.Fatalf("POST archive = %d %s; want 200", a.status, a.body)
		}
		for _, id := range []string{ws, f1} {
			if rec := storedRecord(t, h.Hub, id); rec.State != sandbox.StateStopped || rec.StateReason != sandbox.StopArchived {
				t.Errorf("the sandbox %s after the archive is %s (%q); want it stopped for archived", id, rec.State, rec.StateReason)
			}
		}

		if a := h.call(t, http.MethodDelete, "/workspaces/gone", h.Token, nil); a.status != http.StatusNoContent {
			t.Fatalf("DELETE the workspace = %d %s; want 204", a.status, a.body)
		}
		for _, id := range []string{ws, f1} {
			if evs := h.sandboxEvents(t, sandbox.EventDelete, id); !bySystem(evs, "workspace_deleted") {
				t.Errorf("the delete events of %s = %+v; want one {reason: workspace_deleted} by the system", id, evs)
			}
		}
		for _, slug := range []string{"gone", "gone--t1"} {
			if left := records(t, slug); len(left) != 0 {
				t.Errorf("the deleted workspace %s still has the sandbox records %v", slug, left)
			}
		}
	})
}
