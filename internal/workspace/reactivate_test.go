package workspace_test

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// reactivateEvent is the event a reactivate writes.
const reactivateEvent = "parley.workspace.reactivate"

func TestReactivate_PutsAnArchivedWorkspaceBackToPending(t *testing.T) {
	t.Parallel()
	steps := &teardownLog{}
	w := newWorld(t, recordSteps(steps))
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusArchived})
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

			a := w.operate(t, "reactivate", c.cred, "demo")

			rec, _ := w.workspace(t, "demo")
			if c.fault != "" {
				if a.outcome(t) != "500 internal" {
					t.Errorf("answer = %d %s, want 500 internal", a.status, a.body)
				}
				if n := len(w.eventsOf(t, reactivateEvent)); rec.Status != workspace.StatusArchived || n != events || len(w.notifications()) != notified {
					t.Errorf("status %s, %d reactivate events and %d clone notifications after a failed reactivate, want archived and none",
						rec.Status, n-events, len(w.notifications())-notified)
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
			w.checkEvent(t, reactivateEvent, events, "demo", c.actorType, c.actorID, map[string]any{})
			if got := w.notifications()[notified:]; !slices.Equal(got, []string{"demo"}) {
				t.Errorf("clone notifications %v, want [demo]", got)
			}
			if got := steps.recorded(); len(got) == 0 || got[len(got)-1] != "notify demo copy=false pending" {
				t.Errorf("the clone workers were told %v, want while the store held pending: after the commit", got)
			}
		})
	}
}

func TestReactivate_CloneWorkClonesOriginAfreshWhenTheCopyIsNoLongerValid(t *testing.T) {
	t.Parallel()
	s := newSyncWorld(t, withCloneWorkers(), withSafetyPusher(&stubBranchesPusher{answer: taking(0)}))
	for _, c := range []struct {
		name string
		// damage breaks the copy after the archive.
		damage func(t *testing.T)
	}{
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
			if a := s.operate(t, "archive", s.uKey, syncSlug); a.status != http.StatusOK {
				t.Fatalf("archive = %d %s, want 200", a.status, a.body)
			}
			c.damage(t)

			if a := s.operate(t, "reactivate", s.uKey, syncSlug); a.status != http.StatusOK {
				t.Fatalf("reactivate = %d %s, want 200", a.status, a.body)
			}
			rec := s.cloned(t, syncSlug)
			if main := ref(t, s.copy, "refs/heads/main"); main != originMain || deref(rec.HeadSHA) != originMain {
				t.Errorf("the copy's main %s and head_sha %s after the fresh clone, want origin's main %s", main, deref(rec.HeadSHA), originMain)
			}
			if localOnly := ref(t, s.copy, "refs/heads/local-only"); localOnly != "" {
				t.Errorf("refs/heads/local-only is %s in the fresh copy, want it absent", localOnly)
			}
		})
	}
}
