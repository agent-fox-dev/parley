package change_test

import (
	"context"
	"database/sql"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// abandonReasons are the reasons of the parley.change.abandon events after
// seq, in order.
func (w *world) abandonReasons(t *testing.T, seq int64) []any {
	t.Helper()
	var reasons []any
	for _, e := range w.eventsAfter(t, seq) {
		if e.Type == change.EventAbandon {
			reasons = append(reasons, metadata(t, e)["reason"])
		}
	}
	return reasons
}

func TestHubPart_ConnectsTheChangeComponentToTheOtherFeatures(t *testing.T) {
	t.Parallel()
	var skew atomic.Int64
	w := newWorldWith(t, hubtest.Config{TOML: zeroDebounce, Now: func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }})
	ctx := context.Background()

	t.Run("a branch a sync moved gets a version of the hub", func(t *testing.T) {
		before := w.branch(t, "synced", "Synced\n\nWhy.\n")
		w.openBranch(t, "synced", nil)
		after := commit(t, w.demo, "synced", before, "Fetched\n", map[string]string{"f.txt": "f\n"})
		w.workspaces.NotifyBranchMove(ctx, workspace.BranchMove{Workspace: "demo", Branch: "synced", Before: before, After: after,
			Cause: workspace.MoveCauseSync})
		v := w.latest(t, 1, 2, after, change.KindSync)
		if by, _ := v["created_by"].(map[string]any); by["actor_type"] != change.ActorSystem || by["actor_id"] != nil {
			t.Errorf("created_by = %v, want the system actor", by)
		}
	})

	m1 := w.addFork(t, "demo--m1", "main", nil, nil)
	commit(t, m1, "task", w.base, "Task\n\nWhy.\n", map[string]string{"t.txt": "t\n"})
	t.Run("a change from a fork", func(t *testing.T) {
		if out := w.mustPost(t, map[string]any{"source": map[string]any{"workspace_slug": "demo--m1", "branch": "task"}}); out["number"] != 2.0 {
			t.Fatalf("the fork's change = %v, want number 2", out["number"])
		}
		var active sql.NullInt64
		if err := w.Store.Main().QueryRow(`SELECT last_activity_at FROM workspaces WHERE slug = 'demo--m1'`).Scan(&active); err != nil || !active.Valid {
			t.Errorf("the fork's last activity = %v, %v; want it recorded", active, err)
		}
		if refs := clientGit(t, "", "ls-remote", w.remote("demo--m1"), "refs/parley/*"); !strings.Contains(refs+"\n", "\trefs/parley/changes/2/1\n") {
			t.Errorf("the fork advertises %q, want its change's version ref refs/parley/changes/2/1", refs)
		}
		pat := w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: []string{"audit:read"}, Workspace: "demo--m1", CreatedAt: time.Now().UTC()})
		status, page := w.send(t, http.MethodGet, "/events/log", pat, nil)
		var seen []string
		items, _ := page["items"].([]any)
		for _, it := range items {
			e, _ := it.(map[string]any)
			if e["resource_type"] == "change" {
				seen = append(seen, e["type"].(string)+" "+e["workspace"].(string)+"#"+e["resource_id"].(string))
			}
		}
		if want := []string{change.EventVersion + " demo#2", change.EventOpen + " demo#2", change.EventReady + " demo#2"}; status != http.StatusOK || !reflect.DeepEqual(seen, want) {
			t.Errorf("the fork-bound token reads %d %v, want the events of the fork's change only, %v", status, seen, want)
		}
	})

	t.Run("deleting a fork abandons its changes", func(t *testing.T) {
		m2 := w.addFork(t, "demo--m2", "main", nil, nil)
		commit(t, m2, "task", w.base, "Task\n\nWhy.\n", map[string]string{"t2.txt": "t\n"})
		w.mustPost(t, map[string]any{"source": map[string]any{"workspace_slug": "demo--m2", "branch": "task"}})
		seq := w.seq(t)
		if status, out := w.send(t, http.MethodDelete, "/workspaces/demo--m2", w.Token, nil); status != http.StatusNoContent {
			t.Fatalf("DELETE demo--m2 = %d %v, want 204", status, out)
		}
		if ch := w.read(t, 3); ch.State != change.StateAbandoned {
			t.Errorf("the deleted fork's change = %s, want abandoned", ch.State)
		}
		if got, want := w.abandonReasons(t, seq), []any{change.ReasonSourceWorkspaceDeleted}; !reflect.DeepEqual(got, want) {
			t.Errorf("abandon reasons = %v, want %v", got, want)
		}
	})

	t.Run("deleting a workspace deletes its changes", func(t *testing.T) {
		other := w.addWorkspace(t, "other")
		commit(t, other, "o", "main", "O\n\nWhy.\n", map[string]string{"o.txt": "o\n"})
		w.openBranch(t, "o", func(in *change.OpenInput) { in.Workspace = "other" })
		w.set(t, "other", "status", workspace.StatusArchived)
		if status, out := w.send(t, http.MethodDelete, "/workspaces/other", w.Token, nil); status != http.StatusNoContent {
			t.Fatalf("DELETE other = %d %v, want 204", status, out)
		}
		var others, demos int
		if err := w.Store.Main().QueryRow(`SELECT COUNT(*) FILTER (WHERE workspace_slug = 'other'), COUNT(*) FILTER (WHERE workspace_slug = 'demo') FROM changes`).
			Scan(&others, &demos); err != nil || others != 0 || demos != 3 {
			t.Errorf("changes of other, demo = %d, %d (%v); want 0, 3", others, demos, err)
		}
	})

	t.Run("a fork whose work is done is removed a day after its last activity", func(t *testing.T) {
		if res := w.forks.RunPass(ctx); slices.Contains(res.Removed, "demo--m1") {
			t.Fatalf("a pass removed demo--m1 while its change is open: %+v", res)
		}
		w.write(t, func(ctx context.Context, tx *sql.Tx) error { return w.changes.SetAbandoned(ctx, tx, "demo", 2, "done") })
		skew.Store(int64(25 * time.Hour))
		if res := w.forks.RunPass(ctx); !slices.Contains(res.Removed, "demo--m1") {
			t.Errorf("the pass a day later = %+v, want demo--m1 removed", res)
		}
	})

	t.Run("a target branch a sync moved has its changes checked again", func(t *testing.T) {
		before := commit(t, w.demo, "upstream", w.base, "Upstream\n", map[string]string{"u.txt": "u\n"})
		w.branch(t, "onto-upstream", "Onto upstream\n\nWhy.\n")
		n := w.openBranch(t, "onto-upstream", func(in *change.OpenInput) { in.TargetBranch = new("upstream") }).Number
		w.settled(t, n)
		g := newGate()
		change.SetMergeableSeams(w.changes, change.MergeableSeams{Gate: g.pass})
		t.Cleanup(g.release)
		after := commit(t, w.demo, "upstream", before, "Fetched\n", map[string]string{"f.txt": "f\n"})
		w.workspaces.NotifyBranchMove(ctx, workspace.BranchMove{Workspace: "demo", Branch: "upstream", Before: before, After: after,
			Cause: workspace.MoveCauseSync})
		if ch := w.read(t, n); ch.Mergeable != change.MergeableUnknown {
			t.Fatalf("after the sync the change is %s, want unknown until its check", ch.Mergeable)
		}
		g.release()
		if ch := w.settled(t, n); ch.Mergeable != change.MergeableClean {
			t.Errorf("after its check the change is %s, want clean", ch.Mergeable)
		}
	})
}
