package fork_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestDeleteFork_RemovesTheForkAndItsOwnBranches(t *testing.T) {
	t.Parallel()
	w, _ := newRemovalWorld(t)
	callers := []struct {
		name    string
		creator func() string
		cred    func() string
	}{
		{"creator", func() string { return w.mia }, func() string { return w.miaKey }},
		{"parent owner", func() string { return w.dev }, func() string { return w.devKey }},
		{"admin token", func() string { return w.dev }, func() string { return w.Token }},
	}
	for _, status := range []workspace.Status{workspace.StatusReady, workspace.StatusArchived, workspace.StatusFailed} {
		for _, c := range callers {
			t.Run(string(status)+"/"+c.name, func(t *testing.T) {
				name := "t" + string(status[0]) + string(c.name[0])
				slug := parentSlug + "--" + name
				w.seedHeld(t, slug, func(f *fork.Fork) { f.OwnerID = c.creator() })
				w.setParent(t, status, workspace.MarkerNone)
				heads := gitrepotest.Refs(t, w.parent, "refs/heads/")
				before := w.countForks(t)

				if r := w.call(t, http.MethodDelete, "/workspaces/"+slug, c.cred(), nil); r.status != http.StatusNoContent || len(r.body) != 0 {
					t.Fatalf("DELETE = %d %q, want 204 with an empty body", r.status, r.body)
				}
				wantError(t, w.get(t, w.devKey, "/workspaces/"+slug), http.StatusNotFound, "not_found")
				if slugs, _, _ := page(t, w.get(t, w.devKey, forksPath)); slices.Contains(slugs, slug) {
					t.Errorf("forks of demo = %v, want them without %s", slugs, slug)
				}
				if _, err := os.Stat(w.workspaces.RepoPath(slug)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("repository of the fork: %v, want it gone with its own branches", err)
				}
				if got := gitrepotest.Refs(t, w.parent, "refs/heads/"); !reflect.DeepEqual(got, heads) {
					t.Errorf("demo's branches = %v, want them unchanged from %v", got, heads)
				}
				if got := w.countForks(t); got != before-1 {
					t.Errorf("CountTaskForks = %d, want %d", got, before-1)
				}
				if status != workspace.StatusReady {
					return
				}
				created(t, w.post(t, w.devKey, forksPath, map[string]any{"name": name}))
				if _, ok := w.refs(t, slug).Branch("task"); ok {
					t.Errorf("the fork created again under %s has the removed fork's branch task", slug)
				}
			})
		}
	}
}

func TestRemove_TakesWhatTheForkOwnsInOneStep(t *testing.T) {
	t.Parallel()
	w, r := newRemovalWorld(t)
	actor := identity.Principal{Type: identity.TypeAPIKey, User: &identity.User{ID: w.dev}}
	for cause, reason := range map[string]string{
		"deleted": "workspace deleted", "expired": "fork expired", "parent_deleted": "workspace deleted",
	} {
		t.Run(cause, func(t *testing.T) {
			h := w.seedHeld(t, "demo--"+cause[:3])
			r.take()
			if err := w.forks.RemoveWithCause(context.Background(), h.slug, cause, actor); err != nil {
				t.Fatalf("remove: %v", err)
			}
			wantError(t, w.get(t, h.pat, "/user"), http.StatusUnauthorized, "unauthenticated")
			if n, err := w.secrets.Count(context.Background(), secret.ScopeWorkspace, h.slug); err != nil || n != 0 {
				t.Errorf("secrets of the fork = %d, %v, want none", n, err)
			}
			removed, closed, _ := r.take()
			if !reflect.DeepEqual(removed, []string{h.slug}) || !reflect.DeepEqual(closed, [][2]string{{h.slug, reason}}) {
				t.Errorf("remover asked %v, session closer %v; want [%s] and [[%s %s]]", removed, closed, h.slug, h.slug, reason)
			}
			if _, found, err := fork.BySlug(context.Background(), w.Store.Main(), h.slug); err != nil || found {
				t.Errorf("fork record: found %v, %v; want it gone", found, err)
			}
		})
	}
}

func TestDeleteFork_LeavesTheForkWholeWhenAStepFails(t *testing.T) {
	t.Parallel()
	w, r := newRemovalWorld(t)
	for _, c := range []struct {
		name        string
		fail        func(*remover)
		wantRemoved bool
	}{
		{"sandbox deletion fails", func(r *remover) { r.failSandbox = true }, false},
		{"remover of owned data fails", func(r *remover) { r.failRemove = true }, true},
		{"event write fails", func(r *remover) { r.fault.Store("event") }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := w.seedHeld(t, "demo--"+c.name[:2])
			r.take()
			r.set(c.fail)
			t.Cleanup(func() {
				r.set(func(r *remover) { r.failSandbox, r.failRemove = false, false; r.fault.Store("") })
			})
			wantError(t, w.call(t, http.MethodDelete, "/workspaces/"+h.slug, w.devKey, nil), http.StatusInternalServerError, "internal")
			r.set(func(r *remover) { r.fault.Store("") })
			w.whole(t, h)
			removed, _, sandboxes := r.take()
			if (len(removed) > 0) != c.wantRemoved || len(sandboxes) != 1 {
				t.Errorf("remover of owned data asked %v (want a call: %v), sandboxes %v (want one call)", removed, c.wantRemoved, sandboxes)
			}
			if evs := w.eventsFor(t, fork.EventDelete, h.slug); len(evs) != 0 {
				t.Errorf("delete events = %d, want none", len(evs))
			}
		})
	}
}

func TestDeleteFork_DeletesTheSandboxesFirst(t *testing.T) {
	t.Parallel()
	w, r := newRemovalWorld(t)
	for name, remove := range map[string]func(t *testing.T, slug string){
		"DELETE": func(t *testing.T, slug string) {
			if a := w.call(t, http.MethodDelete, "/workspaces/"+slug, w.devKey, nil); a.status != http.StatusNoContent {
				t.Fatalf("DELETE = %d %s, want 204", a.status, a.body)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := w.seedHeld(t, "demo--sb")
			r.take()
			remove(t, h.slug)
			if _, _, got := r.take(); !reflect.DeepEqual(got, []sandboxDelete{{h.slug, "workspace_deleted", false, true}}) {
				t.Errorf("sandbox deletes = %+v, want one of %s, workspace_deleted, no checkpoint, with the record there", got, h.slug)
			}
		})
	}
}

func TestDeleteFork_WritesOneDeleteEvent(t *testing.T) {
	t.Parallel()
	w, _ := newRemovalWorld(t)
	session := uuid.New().String()
	for name, sessionID := range map[string]*string{"no session": nil, "in a session": &session} {
		t.Run(name, func(t *testing.T) {
			h := w.seedHeld(t, "demo--"+name[:2], func(f *fork.Fork) { f.SessionID = sessionID })
			_, b1 := w.pat(t, h.slug, nil, "workspaces:read")
			_, b2 := w.pat(t, h.slug, nil, "workspaces:read")
			if a := w.call(t, http.MethodDelete, "/workspaces/"+h.slug, w.devKey, nil); a.status != http.StatusNoContent {
				t.Fatalf("DELETE = %d %s, want 204", a.status, a.body)
			}
			evs := w.eventsFor(t, fork.EventDelete, h.slug)
			if len(evs) != 1 {
				t.Fatalf("delete events = %d, want 1", len(evs))
			}
			ev := evs[0]
			if ev.ResourceID != h.slug || ev.ActorType != identity.TypeAPIKey || ev.ActorID == nil || *ev.ActorID != w.dev {
				t.Errorf("event resource %s, actor %s %v; want %s by dev's key", ev.ResourceID, ev.ActorType, ev.ActorID, h.slug)
			}
			if !reflect.DeepEqual(ev.SessionID, sessionID) {
				t.Errorf("session_id = %v, want %v", ev.SessionID, sessionID)
			}
			var meta map[string]any
			if err := json.Unmarshal(ev.Metadata, &meta); err != nil {
				t.Fatal(err)
			}
			var want []any
			for _, id := range slices.Sorted(slices.Values([]string{h.patID, b1, b2})) {
				want = append(want, id)
			}
			wantMembers(t, meta, map[string]any{"kind": "fork", "parent_slug": parentSlug, "revoked_tokens": want})
			wantAbsent(t, meta, "deleted_forks")
		})
	}
	if evs := w.eventsOf(t, "parley.fork.expire"); len(evs) != 0 {
		t.Errorf("parley.fork.expire events = %d, want none", len(evs))
	}
}

func TestDeleteFork_RefusesAndLeavesTheForkWhole(t *testing.T) {
	t.Parallel()
	w, r := newRemovalWorld(t)
	stranger := w.IssueKey(t, w.CreateUser(t, "stranger"))
	readOnly, _ := w.pat(t, "", nil, "workspaces:read")
	h := w.seedHeld(t, "demo--t1")
	for _, c := range []struct {
		name       string
		cred       func() string
		status     workspace.Status
		marker     workspace.Marker
		wantStatus int
		wantType   string
	}{
		{"org member", func() string { return w.miaKey }, workspace.StatusReady, workspace.MarkerNone, http.StatusForbidden, "forbidden"},
		{"PAT without workspaces:write", func() string { return readOnly }, workspace.StatusReady, workspace.MarkerNone, http.StatusForbidden, "insufficient_scope"},
		{"caller without read access", func() string { return stranger }, workspace.StatusReady, workspace.MarkerNone, http.StatusNotFound, "not_found"},
		{"parent pending", func() string { return w.devKey }, workspace.StatusPending, workspace.MarkerNone, http.StatusConflict, "workspace_busy"},
		{"parent cloning", func() string { return w.devKey }, workspace.StatusCloning, workspace.MarkerNone, http.StatusConflict, "workspace_busy"},
		{"parent under maintenance", func() string { return w.devKey }, workspace.StatusArchived, workspace.MarkerDelete, http.StatusConflict, "workspace_busy"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w.setParent(t, c.status, c.marker)
			r.take()
			wantError(t, w.call(t, http.MethodDelete, "/workspaces/"+h.slug, c.cred(), nil), c.wantStatus, c.wantType)
			if removed, _, sandboxes := r.take(); len(removed)+len(sandboxes) != 0 {
				t.Errorf("collaborators asked: removed %v, sandboxes %v; want nothing", removed, sandboxes)
			}
		})
	}
	w.whole(t, h)
	if evs := w.eventsFor(t, fork.EventDelete, h.slug); len(evs) != 0 {
		t.Errorf("delete events = %d, want none", len(evs))
	}
}

func TestDeleteFork_RemovesAForkAtMostOnce(t *testing.T) {
	t.Parallel()
	w, r := newRemovalWorld(t)
	h := w.seedHeld(t, "demo--t1")
	var mu sync.Mutex
	var statuses []int
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			a := w.call(t, http.MethodDelete, "/workspaces/"+h.slug, w.devKey, nil)
			mu.Lock()
			defer mu.Unlock()
			statuses = append(statuses, a.status)
		})
	}
	wg.Wait()
	if slices.Sort(statuses); !reflect.DeepEqual(statuses, []int{http.StatusNoContent, http.StatusNotFound}) {
		t.Errorf("statuses = %v, want one 204 and one 404", statuses)
	}
	if evs := w.eventsFor(t, fork.EventDelete, h.slug); len(evs) != 1 {
		t.Errorf("delete events = %d, want 1", len(evs))
	}
	if removed, _, _ := r.take(); len(removed) != 1 {
		t.Errorf("remover of owned data asked %v, want once", removed)
	}
	wantError(t, w.call(t, http.MethodDelete, "/workspaces/"+h.slug, w.devKey, nil), http.StatusNotFound, "not_found")
}

func TestDeleteFork_RecordsADiscardThatFails(t *testing.T) {
	t.Parallel()
	w, r := newRemovalWorld(t)
	h := w.seedHeld(t, "demo--t1")
	r.set(func(r *remover) { r.failDiscard = true })
	if a := w.call(t, http.MethodDelete, "/workspaces/"+h.slug, w.devKey, nil); a.status != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s, want 204", a.status, a.body)
	}
	var parent string
	if err := w.Store.Main().QueryRowContext(context.Background(),
		`SELECT parent_slug FROM fork_discards WHERE slug = ?`, h.slug).Scan(&parent); err != nil || parent != parentSlug {
		t.Errorf("pending discard of %s: parent %q, %v; want one of %s", h.slug, parent, err, parentSlug)
	}
}

func TestRemoveForksOf_RemovesEveryForkInTheParentsTransaction(t *testing.T) {
	t.Parallel()
	w, r := newRemovalWorld(t)
	actor := identity.Principal{Type: identity.TypeAPIKey, User: &identity.User{ID: w.dev}}
	var hs []held
	for _, slug := range []string{"demo--b", "demo--a", "demo--c"} {
		hs = append(hs, w.seedHeld(t, slug))
	}
	slices.SortFunc(hs, func(a, b held) int { return strings.Compare(a.slug, b.slug) })
	want := []string{"demo--a", "demo--b", "demo--c"}

	t.Run("rolled back", func(t *testing.T) {
		r.take()
		w.removeForksOf(t, parentSlug, actor, false, nil)
		for _, h := range hs {
			w.whole(t, h)
		}
		if evs := w.eventsOf(t, fork.EventDelete); len(evs) != 0 {
			t.Errorf("delete events = %d, want none", len(evs))
		}
	})

	t.Run("committed", func(t *testing.T) {
		r.take()
		slugs := w.removeForksOf(t, parentSlug, actor, true, func() {
			for _, h := range hs {
				if got := w.head(t, w.workspaces.RepoPath(h.slug), "task"); got != h.task {
					t.Errorf("before the commit, branch task of %s = %q, want %s", h.slug, got, h.task)
				}
			}
		})
		if !reflect.DeepEqual(slugs, want) {
			t.Errorf("removed forks = %v, want %v", slugs, want)
		}
		if n, err := fork.CountOf(context.Background(), w.Store.Main(), parentSlug); err != nil || n != 0 {
			t.Errorf("forks of demo = %d, %v, want none", n, err)
		}
		for _, h := range hs {
			wantError(t, w.get(t, h.pat, "/user"), http.StatusUnauthorized, "unauthenticated")
			if _, err := os.Stat(w.workspaces.RepoPath(h.slug)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("repository of %s: %v, want it gone with its own branches", h.slug, err)
			}
		}
		removed, closed, sandboxes := r.take()
		wantClosed := [][2]string{{want[0], "workspace deleted"}, {want[1], "workspace deleted"}, {want[2], "workspace deleted"}}
		if !reflect.DeepEqual(removed, want) || !reflect.DeepEqual(closed, wantClosed) || len(sandboxes) != 0 {
			t.Errorf("remover asked %v, session closer %v, sandboxes %v; want %v, %v and no sandbox call", removed, closed, sandboxes, want, wantClosed)
		}
		evs := w.eventsOf(t, fork.EventDelete)
		if len(evs) != len(hs) {
			t.Fatalf("delete events = %d, want %d", len(evs), len(hs))
		}
		for _, h := range hs {
			i := slices.IndexFunc(evs, func(e event.Record) bool { return e.Workspace != nil && *e.Workspace == h.slug })
			if i < 0 {
				t.Errorf("no delete event of %s", h.slug)
				continue
			}
			ev := evs[i]
			if ev.ActorType != identity.TypeAPIKey || ev.ActorID == nil || *ev.ActorID != w.dev {
				t.Errorf("actor of %s's event = %s %v, want dev's key", h.slug, ev.ActorType, ev.ActorID)
			}
			var meta map[string]any
			if err := json.Unmarshal(ev.Metadata, &meta); err != nil {
				t.Fatal(err)
			}
			wantMembers(t, meta, map[string]any{"kind": "fork", "parent_slug": parentSlug, "revoked_tokens": []any{h.patID}})
			wantAbsent(t, meta, "deleted_forks")
		}
		if evs := w.eventsOf(t, fork.EventExpire); len(evs) != 0 {
			t.Errorf("parley.fork.expire events = %d, want none", len(evs))
		}
	})

	t.Run("parent without forks", func(t *testing.T) {
		before := len(w.eventsOf(t, fork.EventDelete))
		if slugs := w.removeForksOf(t, parentSlug, actor, true, nil); slugs == nil || len(slugs) != 0 {
			t.Errorf("removed forks = %#v, want an empty list", slugs)
		}
		if got := len(w.eventsOf(t, fork.EventDelete)); got != before {
			t.Errorf("delete events = %d, want %d", got, before)
		}
	})
}

func TestDelete_RemovesTheForksOfAnArchivedParent(t *testing.T) {
	t.Parallel()
	w, _ := newRemovalWorld(t)
	const parent = "demo2"
	w.addParent(t, parent, func(ws *workspace.Workspace) { ws.Status = workspace.StatusArchived })
	want := []string{parent + "--a", parent + "--b"}
	var hs []held
	for _, slug := range want {
		hs = append(hs, w.seedHeld(t, slug, func(f *fork.Fork) { f.ParentSlug = new(parent) }))
	}
	if a := w.call(t, http.MethodDelete, "/workspaces/"+parent, w.devKey, nil); a.status != http.StatusNoContent {
		t.Fatalf("DELETE %s = %d %s, want 204", parent, a.status, a.body)
	}
	if n, err := fork.CountOf(context.Background(), w.Store.Main(), parent); err != nil || n != 0 {
		t.Errorf("forks of %s = %d, %v, want none", parent, n, err)
	}
	for _, h := range hs {
		if evs := w.eventsFor(t, fork.EventDelete, h.slug); len(evs) != 1 {
			t.Errorf("delete events of %s = %d, want 1", h.slug, len(evs))
		}
		if _, err := os.Stat(w.workspaces.RepoPath(h.slug)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("repository of %s: %v, want it gone", h.slug, err)
		}
	}
	evs := w.eventsFor(t, fork.EventDelete, parent)
	if len(evs) != 1 {
		t.Fatalf("delete events of %s = %d, want 1", parent, len(evs))
	}
	var meta struct {
		DeletedForks []string `json:"deleted_forks"`
	}
	if err := json.Unmarshal(evs[0].Metadata, &meta); err != nil || !reflect.DeepEqual(meta.DeletedForks, want) {
		t.Errorf("deleted_forks = %v (%v), want %v", meta.DeletedForks, err, want)
	}
}

// removeForksOf runs RemoveForksOf for the forks of parent by actor in a
// transaction of the hub's store, with during run inside it after the step
// (nil for nothing), and returns the removed slugs. When commit is set the
// transaction commits and the step's discard runs after it; otherwise the
// transaction rolls back and the discard never runs.
func (w *world) removeForksOf(t *testing.T, parent string, actor identity.Principal, commit bool, during func()) []string {
	t.Helper()
	rollback := errors.New("roll back")
	var slugs []string
	var discard func()
	err := w.Store.Tx(context.Background(), func(tx *sql.Tx) error {
		var err error
		if slugs, discard, err = w.forks.RemoveForksOf(context.Background(), tx, parent, actor); err != nil {
			return err
		}
		if during != nil {
			during()
		}
		if !commit {
			return rollback
		}
		return nil
	})
	switch {
	case !commit && !errors.Is(err, rollback), commit && err != nil:
		t.Fatalf("RemoveForksOf in a transaction: %v", err)
	case commit:
		discard()
	}
	return slugs
}

// countForks is CountTaskForks, failing the test on its error.
func (w *world) countForks(t *testing.T) int {
	t.Helper()
	n, err := w.forks.CountTaskForks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}
