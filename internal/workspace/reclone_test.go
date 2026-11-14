package workspace_test

import (
	"context"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// recloneEvent is the event a reclone writes.
const recloneEvent = "parley.workspace.reclone"

// recordSteps records in steps the removals of a copy and the clone
// notifications, in the order the hub makes them, each with whether the
// copy's directory exists at that moment, and a notification with the
// status the store holds for the workspace; the removal still removes.
func recordSteps(steps *teardownLog) option {
	return func(c *hubtest.Config, w *world) {
		withCompose(func(comp *hub.Composition) {
			comp.AdjustWorkspace(func(d *workspace.Deps) {
				next := d.Notify
				d.Notify = func(slug string) {
					rec, _, _ := workspace.BySlug(context.Background(), w.Store.Main(), slug)
					steps.add("notify %s copy=%t %s", slug, exists(w.svc.RepoPath(slug)), rec.Status)
					next(slug)
				}
				d.RemoveCopy = func(dir string) error {
					steps.add("remove copy=%t", exists(dir))
					return gitrepo.Remove(dir)
				}
			})
		})(c, w)
	}
}

// sameRefs fails the test unless the branches and tags of the repository
// at dir are those of origin, with the same ids.
func sameRefs(t *testing.T, dir string, origin *gitrepotest.Origin) {
	t.Helper()
	for _, prefix := range []string{"refs/heads/", "refs/tags/"} {
		if got, want := gitrepotest.Refs(t, dir, prefix), gitrepotest.Refs(t, origin.Dir(), prefix); !maps.Equal(got, want) {
			t.Errorf("the copy's %s are %v, want origin's %v", prefix, got, want)
		}
	}
}

func TestReclone_DiscardsTheCopyAndPutsTheWorkspaceBackToPending(t *testing.T) {
	t.Parallel()
	steps := &teardownLog{}
	p := &stubBranchesPusher{before: func(context.Context) { steps.add("push") }}
	s := newSyncWorld(t, withSafetyPusher(p), recordSteps(steps))
	done := []string{"push", "remove copy=true", "notify demo copy=false pending"}
	for _, c := range []struct {
		name  string
		taken int
		fault string
		// failed reclones the workspace after its clone failed, with no
		// copy.
		failed bool
		// want is the origin_push of the event; "" for a reclone that
		// fails inside the hub.
		want  string
		steps []string
	}{
		{name: "origin takes every branch", taken: 2, want: "ok", steps: done},
		{name: "origin takes some branches", taken: 1, want: "partial", steps: done},
		{name: "origin takes no branch", want: "failed", steps: done},
		{name: "the event cannot be written", taken: 2, fault: faultEvent, steps: []string{"push"}},
		{name: "failed workspace without a copy", taken: 2, failed: true, want: "ok",
			steps: []string{"remove copy=false", "notify demo copy=false pending"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if rec, _ := s.workspace(t, syncSlug); rec.Status != workspace.StatusReady {
				// The row before reclones it: its clone work brings back a
				// ready workspace with a copy.
				if rec = s.clone(t, syncSlug); rec.Status != workspace.StatusReady {
					t.Fatalf("the clone left the workspace %s (%v)", rec.Status, deref(rec.CloneError))
				}
			}
			status, cloneError := workspace.StatusReady, (*string)(nil)
			if c.failed {
				status, cloneError = workspace.StatusFailed, new("boom")
				if err := os.RemoveAll(s.copy); err != nil {
					t.Fatal(err)
				}
			} else {
				gitrepotest.Git(t, s.copy, "update-ref", "refs/remotes/origin/extra", s.origin.Head("main"))
			}
			if _, err := s.Store.Main().Exec(`UPDATE workspaces SET status = ?, clone_error = ?, sync_error = 'x', origin_head_sha = head_sha
				WHERE slug = ?`, string(status), cloneError, syncSlug); err != nil {
				t.Fatal(err)
			}
			before, _ := s.workspace(t, syncSlug)
			var heads map[string]string
			if !c.failed {
				if heads = gitrepotest.Refs(t, s.copy, "refs/remotes/origin/"); len(heads) != 3 {
					t.Fatalf("the copy holds the origin heads %v, want three", heads)
				}
			}
			p.reset(taking(c.taken))
			s.fail(c.fault)
			defer s.fail("")
			events, made := len(s.eventsOf(t, recloneEvent)), len(steps.recorded())

			a := s.operate(t, "reclone", s.uKey, syncSlug)

			rec, _ := s.workspace(t, syncSlug)
			if rec.Marker != workspace.MarkerNone {
				t.Errorf("the marker is %q after the reclone, want it released", rec.Marker)
			}
			if got := steps.recorded()[made:]; !slices.Equal(got, c.steps) {
				t.Errorf("the reclone's steps are %v, want %v", got, c.steps)
			}
			if c.want == "" {
				if a.outcome(t) != "500 internal" {
					t.Errorf("answer = %d %s, want 500 internal", a.status, a.body)
				}
				if n := len(s.eventsOf(t, recloneEvent)); !reflect.DeepEqual(rec, before) || n != events {
					t.Errorf("record %+v and %d reclone events after a failed reclone, want %+v and none", rec, n-events, before)
				}
				if got := gitrepotest.Refs(t, s.copy, "refs/remotes/origin/"); !maps.Equal(got, heads) {
					t.Errorf("the copy's origin heads are %v after a failed reclone, want %v", got, heads)
				}
				return
			}
			obj := a.decode(t, http.StatusOK)
			if obj["status"] != "pending" || obj["sync_error"] != nil || obj["origin_head_sha"] != nil {
				t.Errorf("answer status %v sync_error %v origin_head_sha %v, want pending, null and null",
					obj["status"], obj["sync_error"], obj["origin_head_sha"])
			}
			if rec.Status != workspace.StatusPending || rec.SyncError != nil || rec.OriginHeadSHA != nil {
				t.Errorf("stored status %s sync_error %v origin_head_sha %v, want pending and null",
					rec.Status, deref(rec.SyncError), deref(rec.OriginHeadSHA))
			}
			if rec.InstanceID == before.InstanceID {
				t.Error("the record keeps its instance id after the reclone")
			}
			if deref(rec.HeadSHA) != deref(before.HeadSHA) || deref(rec.DefaultBranch) != deref(before.DefaultBranch) {
				t.Errorf("head_sha %q default_branch %q after the reclone, want %q %q as before",
					deref(rec.HeadSHA), deref(rec.DefaultBranch), deref(before.HeadSHA), deref(before.DefaultBranch))
			}
			if exists(s.copy) {
				t.Error("the copy is still on disk after the reclone")
			}
			s.checkEvent(t, recloneEvent, events, syncSlug, event.ActorAPIKey, s.u, map[string]any{"origin_push": c.want})
		})
	}
}

func TestReclone_TheNewCopyHoldsExactlyWhatOriginHolds(t *testing.T) {
	t.Parallel()
	calls := &teardownLog{}
	removers := &stubTeardown{name: "remover", log: calls}
	s := newSyncWorld(t, withCloneWorkers(), withTeardown(func(td *workspace.Teardown) {
		td.RegisterRemover(removers.remove)
		td.RegisterRemover(removers.remove)
	}))
	main := s.origin.Head("main")
	accepted := grow(t, s.copy, "accepted", main, "hub")
	grow(t, s.copy, "rejected", main, "hub")
	gitrepotest.Git(t, s.copy, "update-ref", "refs/parley/changes/1/v1", main)
	// Origin's update hook refuses the branch rejected and takes the others.
	hook := filepath.Join(s.origin.Dir(), "hooks", "update")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n[ \"$1\" != refs/heads/rejected ]\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if a := s.operate(t, "reclone", s.uKey, syncSlug); a.status != http.StatusOK {
		t.Fatalf("reclone = %d %s, want 200", a.status, a.body)
	}
	rec := s.cloned(t, syncSlug)
	sameRefs(t, s.copy, s.origin)
	for _, name := range []string{"refs/heads/rejected", "refs/parley/changes/1/v1"} {
		if id := ref(t, s.copy, name); id != "" {
			t.Errorf("%s is %s in the new copy, want it absent", name, id)
		}
	}
	if got := ref(t, s.copy, "refs/heads/accepted"); got != accepted {
		t.Errorf("refs/heads/accepted is %q in the new copy, want the hub's former commit %s, which origin took", got, accepted)
	}
	if deref(rec.HeadSHA) != s.origin.Head("main") || deref(rec.OriginHeadSHA) != deref(rec.HeadSHA) {
		t.Errorf("head_sha %s origin_head_sha %s, want origin's main %s for both", deref(rec.HeadSHA), deref(rec.OriginHeadSHA), s.origin.Head("main"))
	}
	if evs := s.eventsOf(t, recloneEvent); len(evs) != 1 || string(evs[0].Metadata) != `{"origin_push":"partial"}` {
		t.Errorf("reclone events %v, want one with origin_push partial", evs)
	}
	if got := calls.recorded(); len(got) != 0 {
		t.Errorf("the reclone called removers of owned data: %v, want no call", got)
	}
}

func TestReclone_ACopyLeftByAStoppedHubIsClonedFresh(t *testing.T) {
	t.Parallel()
	// The removal is a no-op, as for a hub that stopped once the reclone had
	// committed and before it removed the copy.
	s := newSyncWorld(t, withRemoveCopy(func(string) error { return nil }))
	grow(t, s.copy, "local-only", s.origin.Head("main"), "hub")
	s.origin.Reject("refused by policy")

	if a := s.operate(t, "reclone", s.uKey, syncSlug); a.status != http.StatusOK {
		t.Fatalf("reclone = %d %s, want 200", a.status, a.body)
	}
	if rec, _ := s.workspace(t, syncSlug); rec.Status != workspace.StatusPending || ref(t, s.copy, "refs/heads/local-only") == "" {
		t.Fatalf("status %s and local-only %q after the reclone, want pending and the old copy on disk",
			rec.Status, ref(t, s.copy, "refs/heads/local-only"))
	}

	s.startWorkers(t, 1, nil)

	s.cloned(t, syncSlug)
	if id := ref(t, s.copy, "refs/heads/local-only"); id != "" {
		t.Errorf("refs/heads/local-only is %s in the copy, want it absent", id)
	}
	sameRefs(t, s.copy, s.origin)
}

func TestReclone_WaitsOutASyncOrPushAlreadyRunning(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, path, event string
		// hold makes the work stop, once it has passed the state guard, at
		// a point where it still has records to write, until release is
		// closed; held is closed when it has stopped there.
		hold func(s *syncWorld, held, release chan struct{})
	}{
		{"a sync", "/sync", syncEvent, func(s *syncWorld, held, release chan struct{}) {
			s.origin.Commit("main", "ahead on origin")
			s.swaps.set("main", func(call int) {
				if call == 1 {
					close(held)
					<-release
				}
			})
		}},
		{"a push", "/branches/main/push", pushEvent, func(s *syncWorld, held, release chan struct{}) {
			s.pushes.answerWith(func(context.Context) (gitrepo.PushResult, error) {
				close(held)
				<-release
				return gitrepo.PushUpToDate, nil
			})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newSyncWorld(t)
			held, release := make(chan struct{}), make(chan struct{})
			free := sync.OnceFunc(func() { close(release) })
			defer free()
			c.hold(s, held, release)

			work := s.send(http.MethodPost, s.uKey, "/api/v1/workspaces/"+syncSlug+c.path, nil)
			<-held
			reclone := s.send(http.MethodPost, s.uKey, "/api/v1/workspaces/"+syncSlug+"/reclone", nil)
			eventually(t, "the reclone sets its marker", func() bool {
				rec, _ := s.workspace(t, syncSlug)
				return rec.Marker == workspace.MarkerReclone
			})
			select {
			case status := <-reclone:
				t.Fatalf("the reclone answered %d while %s was still running, want it to wait", status, c.name)
			case <-time.After(time.Second):
			}
			if rec, _ := s.workspace(t, syncSlug); rec.Status != workspace.StatusReady || len(s.eventsOf(t, recloneEvent)) != 0 {
				t.Errorf("while %s runs the workspace is %s with %d reclone events, want ready and none",
					c.name, rec.Status, len(s.eventsOf(t, recloneEvent)))
			}
			free()

			if status := <-work; status != http.StatusOK {
				t.Errorf("%s = %d, want 200", c.name, status)
			}
			if status := <-reclone; status != http.StatusOK {
				t.Errorf("the reclone = %d, want 200", status)
			}
			done, recloned := s.eventsOf(t, c.event), s.eventsOf(t, recloneEvent)
			if len(done) == 0 || len(recloned) != 1 || done[len(done)-1].Seq > recloned[0].Seq {
				t.Fatalf("%d %s and %d reclone events, want the work's event before the one reclone event",
					len(done), c.event, len(recloned))
			}
			if rec, _ := s.workspace(t, syncSlug); rec.Status != workspace.StatusPending || rec.OriginHeadSHA != nil {
				t.Errorf("after both the workspace is %s with origin_head_sha %v, want pending and none",
					rec.Status, deref(rec.OriginHeadSHA))
			}
		})
	}
}
