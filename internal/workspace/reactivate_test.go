package workspace_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// reactivateEvent is the event a reactivate writes.
const reactivateEvent = "parley.workspace.reactivate"

// reactivate sends POST /workspaces/<slug>/reactivate, with no body, with
// cred as the bearer credential.
func (w *world) reactivate(t *testing.T, cred, slug string) answer {
	t.Helper()
	status, b := w.Call(t, http.MethodPost, "/api/v1/workspaces/"+slug+"/reactivate", cred, nil)
	return answer{status, b}
}

// statusAtNotify records, for every clone notification, the status the
// store holds for the workspace at that moment, before the world's own
// notification runs.
type statusAtNotify struct {
	mu   sync.Mutex
	seen []workspace.Status
}

func (s *statusAtNotify) option() option {
	return func(c *hubtest.Config, w *world) {
		withCompose(func(comp *hub.Composition) {
			comp.AdjustWorkspace(func(d *workspace.Deps) {
				next := d.Notify
				d.Notify = func(slug string) {
					rec, _, _ := workspace.BySlug(context.Background(), w.Store.Main(), slug)
					s.mu.Lock()
					s.seen = append(s.seen, rec.Status)
					s.mu.Unlock()
					next(slug)
				}
			})
		})(c, w)
	}
}

func (s *statusAtNotify) recorded() []workspace.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.seen)
}

func TestReactivate_PutsAnArchivedWorkspaceBackToPending(t *testing.T) {
	t.Parallel()
	seen := &statusAtNotify{}
	w := newWorld(t, seen.option())
	url := "https://origin.test/r.git"
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusArchived, GitURL: &url})
	for _, c := range []struct {
		name      string
		cred      string
		fault     string
		actorType string
		actorID   string
	}{
		{name: "the owner", cred: w.uKey, actorType: event.ActorAPIKey, actorID: w.u},
		{name: "the admin token", cred: w.Token, actorType: event.ActorAdminToken},
		{name: "the event cannot be written", cred: w.uKey, fault: faultEvent},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := w.Store.Main().Exec(`UPDATE workspaces SET status = ?, clone_error = 'boom' WHERE slug = 'demo'`,
				string(workspace.StatusArchived)); err != nil {
				t.Fatal(err)
			}
			w.fail(c.fault)
			defer w.fail("")
			events, notified := len(w.eventsOf(t, reactivateEvent)), len(w.notifications())

			a := w.reactivate(t, c.cred, "demo")

			rec, _ := w.workspace(t, "demo")
			evs := w.eventsOf(t, reactivateEvent)
			if c.fault != "" {
				if a.status != http.StatusInternalServerError || a.errorType(t) != "internal" {
					t.Errorf("answer = %d %s, want 500 internal", a.status, a.body)
				}
				if rec.Status != workspace.StatusArchived || len(evs) != events || len(w.notifications()) != notified {
					t.Errorf("status %s, %d reactivate events and %d clone notifications after a failed reactivate, want archived and none",
						rec.Status, len(evs)-events, len(w.notifications())-notified)
				}
				return
			}
			obj := a.decode(t, http.StatusOK)
			if obj["status"] != "pending" || obj["clone_error"] != nil || obj["slug"] != "demo" {
				t.Errorf("answer status %v clone_error %v slug %v, want pending, null, demo", obj["status"], obj["clone_error"], obj["slug"])
			}
			if rec.Status != workspace.StatusPending || rec.CloneError != nil {
				t.Errorf("stored status %s clone_error %v, want pending and null", rec.Status, deref(rec.CloneError))
			}
			if len(evs) != events+1 {
				t.Fatalf("%d reactivate events written, want 1", len(evs)-events)
			}
			ev := evs[events]
			if deref(ev.Workspace) != "demo" || ev.ActorType != c.actorType || deref(ev.ActorID) != c.actorID || string(ev.Metadata) != "{}" {
				t.Errorf("reactivate event of workspace %q by %s %q with %s, want workspace demo by %s %q with {}",
					deref(ev.Workspace), ev.ActorType, deref(ev.ActorID), ev.Metadata, c.actorType, c.actorID)
			}
			if got := w.notifications()[notified:]; !slices.Equal(got, []string{"demo"}) {
				t.Errorf("clone notifications %v, want [demo]", got)
			}
			if got := seen.recorded(); len(got) == 0 || got[len(got)-1] != workspace.StatusPending {
				t.Errorf("the clone workers were told while the store held %v, want pending: after the commit", got)
			}
		})
	}
}

func TestReactivate_RefusesWithoutTouchingTheWorkspace(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	url := "https://origin.test/r.git"
	for _, rec := range []workspace.Workspace{
		{Slug: "demo", Status: workspace.StatusArchived},
		{Slug: "demo--t1", Kind: access.KindFork, ParentSlug: new("demo"), Status: workspace.StatusArchived},
		{Slug: "ready", Status: workspace.StatusReady},
		{Slug: "failed", Status: workspace.StatusFailed},
		{Slug: "pending", Status: workspace.StatusPending},
		{Slug: "cloning", Status: workspace.StatusCloning},
		{Slug: "archived-busy", Status: workspace.StatusArchived, Marker: workspace.MarkerDelete},
		{Slug: "ready-busy", Status: workspace.StatusReady, Marker: workspace.MarkerReclone},
	} {
		if rec.Kind == "" {
			rec.GitURL = &url
		}
		w.put(t, rec)
	}
	for _, c := range []struct {
		slug      string
		status    int
		errorType string
	}{
		{"demo--t1", http.StatusBadRequest, "not_supported_on_fork"},
		{"ready", http.StatusConflict, "workspace_not_archived"},
		{"failed", http.StatusConflict, "workspace_not_archived"},
		{"pending", http.StatusConflict, "workspace_not_archived"},
		{"cloning", http.StatusConflict, "workspace_not_archived"},
		{"archived-busy", http.StatusConflict, "workspace_busy"},
		{"ready-busy", http.StatusConflict, "workspace_busy"},
	} {
		before, _ := w.workspace(t, c.slug)
		if a := w.reactivate(t, w.uKey, c.slug); a.status != c.status || a.errorType(t) != c.errorType {
			t.Errorf("reactivate of %s = %d %s, want %d %s", c.slug, a.status, a.body, c.status, c.errorType)
		}
		if after, _ := w.workspace(t, c.slug); !reflect.DeepEqual(after, before) {
			t.Errorf("reactivate of %s changed the record from %+v to %+v", c.slug, before, after)
		}
	}
	if evs := w.eventsOf(t, reactivateEvent); len(evs) != 0 {
		t.Errorf("refused reactivates wrote %d reactivate events, want none", len(evs))
	}
	if got := w.notifications(); len(got) != 0 {
		t.Errorf("refused reactivates told the clone workers about %v, want nothing", got)
	}
}

func TestReactivate_CloneWorkBringsTheWorkspaceBackFromItsCopyOrFromOrigin(t *testing.T) {
	t.Parallel()
	refuseAll := &stubBranchesPusher{answer: func(_ context.Context, req gitrepo.PushBranchesRequest) ([]gitrepo.BranchPush, error) {
		var got []gitrepo.BranchPush
		for b, id := range req.Heads {
			got = append(got, gitrepo.BranchPush{Branch: b, Commit: id, Err: gitrepo.ErrNonFastForward})
		}
		return got, nil
	}}
	s := newSyncWorld(t, withCloneWorkers(), withSafetyPusher(refuseAll))
	for _, c := range []struct {
		name string
		// damage breaks the copy after the archive; nil keeps it valid.
		damage func(t *testing.T)
	}{
		// The kept copy comes first: it holds the hub's commits, which a
		// fresh clone of a later row has thrown away.
		{name: "the kept copy is valid"},
		{name: "the copy directory is removed", damage: func(t *testing.T) {
			if err := os.RemoveAll(s.copy); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "the copy's HEAD file is truncated", damage: func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(s.copy, "HEAD"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			originMain := s.origin.Head("main")
			// Commits only the hub holds, on main and on a branch origin
			// does not have, which the refusing safety copy leaves there.
			hubMain := grow(t, s.copy, "main", ref(t, s.copy, "refs/heads/main"), "hub")
			grow(t, s.copy, "local-only", originMain, "hub")
			if _, err := s.Store.Main().Exec(`UPDATE workspaces SET head_sha = ? WHERE slug = ?`, hubMain, syncSlug); err != nil {
				t.Fatal(err)
			}
			if a := s.archive(t, s.uKey, syncSlug); a.status != http.StatusOK {
				t.Fatalf("archive = %d %s, want 200", a.status, a.body)
			}
			if c.damage != nil {
				c.damage(t)
			}
			asked := s.origin.Requests()

			if a := s.reactivate(t, s.uKey, syncSlug); a.status != http.StatusOK {
				t.Fatalf("reactivate = %d %s, want 200", a.status, a.body)
			}
			var rec workspace.Workspace
			eventually(t, "the clone work of the reactivated workspace ends", func() bool {
				rec, _ = s.workspace(t, syncSlug)
				return rec.Status == workspace.StatusReady || rec.Status == workspace.StatusFailed
			})
			if rec.Status != workspace.StatusReady {
				t.Fatalf("the reactivated workspace is %s (%v), want ready", rec.Status, deref(rec.CloneError))
			}
			localOnly := ref(t, s.copy, "refs/heads/local-only")
			if c.damage == nil {
				if deref(rec.HeadSHA) != hubMain || localOnly == "" {
					t.Errorf("head_sha %s and local-only %q after the reactivate, want the head before the archive %s and the branch kept",
						deref(rec.HeadSHA), localOnly, hubMain)
				}
				if n := s.origin.Requests() - asked; n != 0 {
					t.Errorf("origin got %d requests between the reactivate and ready, want none", n)
				}
				return
			}
			if main := ref(t, s.copy, "refs/heads/main"); main != originMain || deref(rec.HeadSHA) != originMain {
				t.Errorf("the copy's main %s and head_sha %s after the fresh clone, want origin's main %s", main, deref(rec.HeadSHA), originMain)
			}
			if localOnly != "" {
				t.Errorf("refs/heads/local-only is %s in the fresh copy, want it absent", localOnly)
			}
		})
	}
}
