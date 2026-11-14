package workspace_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// archiveEvent is the event an archive writes.
const archiveEvent = "parley.workspace.archive"

func TestArchive_ArchivesAndRecordsTheOutcome(t *testing.T) {
	t.Parallel()
	// seen is the status the store held for the workspace when the safety
	// copy offered its branches.
	var mu sync.Mutex
	var seen workspace.Status
	var s *syncWorld
	p := &stubBranchesPusher{before: func(ctx context.Context) {
		rec, _, _ := workspace.BySlug(ctx, s.Store.Main(), syncSlug)
		mu.Lock()
		seen = rec.Status
		mu.Unlock()
	}}
	s = newSyncWorld(t, withSafetyPusher(p))
	ready, _ := s.workspace(t, syncSlug)
	dialFails := func(context.Context, gitrepo.PushBranchesRequest) ([]gitrepo.BranchPush, error) {
		return nil, errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	}
	for _, c := range []struct {
		name   string
		answer pushAnswer
		fault  string
		forks  int
		// failed archives the workspace after its clone failed, with no
		// copy; it is the last row, since the copy is gone afterwards.
		failed bool
		// pushed tells whether the safety copy offers the branches.
		pushed bool
		// want is the origin_push of the event; "" for an archive that
		// fails inside the hub.
		want string
	}{
		{name: "origin takes every branch", answer: taking(2), pushed: true, want: "ok"},
		{name: "origin takes some branches", answer: taking(1), forks: 2, pushed: true, want: "partial"},
		{name: "origin takes no branch", answer: taking(0), forks: 1, pushed: true, want: "failed"},
		{name: "the credential cannot be read", answer: taking(2), fault: faultSecretRead, want: "failed"},
		{name: "origin cannot be dialled", answer: dialFails, pushed: true, want: "failed"},
		{name: "the event cannot be written", answer: taking(2), fault: faultEvent, pushed: true},
		{name: "failed workspace without a copy", answer: taking(2), failed: true, want: "ok"},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, cloneError, head := workspace.StatusReady, (*string)(nil), ready.HeadSHA
			if c.failed {
				status, cloneError, head = workspace.StatusFailed, new("boom"), nil
				if err := os.RemoveAll(s.copy); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Store.Main().Exec(`UPDATE workspaces SET status = ?, clone_error = ?, head_sha = ? WHERE slug = ?`,
				string(status), cloneError, head, syncSlug); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Store.Main().Exec(`DELETE FROM workspaces WHERE parent_slug = ?`, syncSlug); err != nil {
				t.Fatal(err)
			}
			for i := range c.forks {
				s.put(t, workspace.Workspace{Slug: fmt.Sprintf("%s--f%d", syncSlug, i), Kind: access.KindFork,
					ParentSlug: new(syncSlug), Status: workspace.StatusReady})
			}
			p.reset(c.answer)
			mu.Lock()
			seen = ""
			mu.Unlock()
			s.fail(c.fault)
			defer s.fail("")
			events := len(s.eventsOf(t, archiveEvent))

			a := s.operate(t, "archive", s.uKey, syncSlug)

			rec, _ := s.workspace(t, syncSlug)
			if rec.Marker != workspace.MarkerNone {
				t.Errorf("the marker is %q after the archive, want it released", rec.Marker)
			}
			if heads, _ := p.calls(); (len(heads) == 1) != c.pushed || len(heads) > 1 {
				t.Errorf("%d safety copies offered branches, want offered = %v", len(heads), c.pushed)
			}
			mu.Lock()
			if c.pushed && seen != status {
				t.Errorf("the safety copy ran while the store held status %q, want %q", seen, status)
			}
			mu.Unlock()
			if c.want == "" {
				if a.outcome(t) != "500 internal" {
					t.Errorf("answer = %d %s, want 500 internal", a.status, a.body)
				}
				if n := len(s.eventsOf(t, archiveEvent)); rec.Status != status || n != events {
					t.Errorf("status %s and %d archive events after a failed archive, want %s and none", rec.Status, n-events, status)
				}
				return
			}
			obj := a.decode(t, http.StatusOK)
			if obj["status"] != "archived" || obj["clone_error"] != nil || obj["slug"] != syncSlug {
				t.Errorf("answer status %v clone_error %v slug %v, want archived, null, %s", obj["status"], obj["clone_error"], obj["slug"], syncSlug)
			}
			if rec.Status != workspace.StatusArchived || rec.CloneError != nil {
				t.Errorf("stored status %s clone_error %v, want archived and null", rec.Status, deref(rec.CloneError))
			}
			want := map[string]any{"head_sha": nil, "origin_push": c.want, "forks": float64(c.forks)}
			if head != nil {
				want["head_sha"] = *head
			}
			s.checkEvent(t, archiveEvent, events, syncSlug, event.ActorAPIKey, s.u, want)
		})
	}
}

func TestArchive_LeavesTheCopyAndTheForksAsTheyAre(t *testing.T) {
	t.Parallel()
	calls := &teardownLog{}
	removers := &stubTeardown{name: "remover", log: calls}
	s := newSyncWorld(t, withTeardown(func(td *workspace.Teardown) {
		td.RegisterRemover(removers.remove)
		td.RegisterRemover(removers.remove)
		td.RegisterSandboxRemover(&stubTeardown{name: "sandboxes", log: calls})
	}))
	main := s.origin.Head("main")
	grow(t, s.copy, "local-only", main, "hub")
	gitrepotest.Git(t, s.copy, "update-ref", "refs/parley/changes/1/v1", main)
	s.origin.Reject("refused by policy")
	s.put(t, workspace.Workspace{Slug: "demo--t1", Kind: access.KindFork, ParentSlug: new(syncSlug), Status: workspace.StatusReady})
	before := gitrepotest.Git(t, s.copy, "for-each-ref")

	if a := s.operate(t, "archive", s.uKey, syncSlug); a.status != http.StatusOK {
		t.Fatalf("archive = %d %s, want 200", a.status, a.body)
	}
	if after := gitrepotest.Git(t, s.copy, "for-each-ref"); after != before {
		t.Errorf("the copy's refs after the archive:\n%s\nwant them as before:\n%s", after, before)
	}
	for _, name := range []string{"refs/heads/local-only", "refs/parley/changes/1/v1"} {
		if !strings.Contains(before, name) {
			t.Errorf("the copy has no %s", name)
		}
	}
	if evs := s.eventsOf(t, archiveEvent); len(evs) != 1 || !strings.Contains(string(evs[0].Metadata), `"origin_push":"partial"`) {
		t.Errorf("archive events %v, want one with origin_push partial", evs)
	}
	if _, found := s.workspace(t, "demo--t1"); !found {
		t.Error("the fork demo--t1 is gone after the archive of its parent")
	}
	if got := calls.recorded(); len(got) != 0 {
		t.Errorf("the archive called removers of owned data or the sandbox remover: %v, want no call", got)
	}
	if obj := s.get(t, s.uKey, syncSlug).decode(t, http.StatusOK); obj["status"] != "archived" {
		t.Errorf("a read after the archive answers status %v, want archived", obj["status"])
	}
}

func TestArchive_HubStartReleasesAMarkerLeftByAStoppedHub(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusReady})
	w.setMarker(t, "demo", workspace.MarkerArchive)

	w.Restart(t)

	if rec, _ := w.workspace(t, "demo"); rec.Marker != workspace.MarkerNone {
		t.Errorf("the marker is %q after the restart, want it released", rec.Marker)
	}
	if a := w.operate(t, "archive", w.uKey, "demo"); a.status != http.StatusOK {
		t.Errorf("archive after the restart = %d %s, want 200", a.status, a.body)
	}
}

func TestArchive_EndsTheSessionsAndTellsTheObserversOfTheWorkspaceAndItsForks(t *testing.T) {
	t.Parallel()
	closed := []string{"first close [demo demo--a demo--b] workspace archived"}
	observed := []string{"first observe demo [demo--a demo--b] archived", "second observe demo [demo--a demo--b] archived"}
	for _, c := range []struct {
		name string
		// closer registers the stub first as the session closer, observers
		// first and second as archive observers; first fails with fail, or
		// panics when panics is set.
		closer, observers bool
		fail              error
		panics            bool
		// marks is the number of rows the closer's write leaves, calls the
		// stubs' calls, logged what the log says of first's failure.
		marks  int
		calls  []string
		logged string
	}{
		{name: "the closer ends them in the archive's transaction", closer: true, marks: 1, calls: closed},
		{name: "the closer fails and its writes are undone", closer: true, fail: errors.New("sessions store down"),
			calls: closed, logged: "sessions store down"},
		{name: "the first observer fails", observers: true, fail: errors.New("sandboxes unreachable"),
			calls: observed, logged: "sandboxes unreachable"},
		{name: "the first observer panics", observers: true, fail: errors.New("sandboxes unreachable"), panics: true,
			calls: observed, logged: "the first panicked"},
		{name: "an empty collaborator set"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var log hubtest.LogBuffer
			calls := &teardownLog{}
			first := &stubTeardown{name: "first", log: calls, mark: true}
			first.set(c.fail, c.panics)
			second := &stubTeardown{name: "second", log: calls}
			w := archiveWithForks(t, []string{"demo--b", "demo--a"}, withLog(&log), withTeardown(func(td *workspace.Teardown) {
				td.Clear()
				if c.closer {
					td.RegisterSessionCloser(first)
				}
				if c.observers {
					td.RegisterArchiveObserver(first)
					td.RegisterArchiveObserver(second)
				}
			}))
			first.w, second.w = w, w
			if _, err := w.Store.Main().Exec(`CREATE TABLE session_marks (slugs TEXT NOT NULL)`); err != nil {
				t.Fatal(err)
			}

			a := w.operate(t, "archive", w.uKey, "demo")

			if obj := a.decode(t, http.StatusOK); obj["status"] != "archived" {
				t.Errorf("answer status %v, want archived", obj["status"])
			}
			if got := calls.recorded(); !slices.Equal(got, c.calls) {
				t.Errorf("the collaborators' calls %v, want %v", got, c.calls)
			}
			var marks int
			if err := w.Store.Main().QueryRow(`SELECT count(*) FROM session_marks`).Scan(&marks); err != nil {
				t.Fatal(err)
			}
			rec, _ := w.workspace(t, "demo")
			if n := len(w.eventsOf(t, archiveEvent)); marks != c.marks || rec.Status != workspace.StatusArchived || n != 1 {
				t.Errorf("%d closer rows, status %s and %d archive events stored, want %d, archived and 1", marks, rec.Status, n, c.marks)
			}
			if c.logged != "" && (!strings.Contains(log.String(), "slug=demo") || !strings.Contains(log.String(), c.logged)) {
				t.Errorf("the log does not report first's failure with the slug:\n%s", log.String())
			}
		})
	}
}

// unreadableObserver is an archive observer that leaves the archived
// workspace's record one the hub cannot read.
type unreadableObserver struct{ w *world }

func (o unreadableObserver) WorkspaceArchived(ctx context.Context, slug string, _ []string) error {
	_, err := o.w.Store.Main().ExecContext(ctx, `UPDATE workspaces SET created_at = 'not a time' WHERE slug = ?`, slug)
	return err
}

func TestArchive_AnswersTheCommittedArchiveWhenTheRecordCannotBeReadAgain(t *testing.T) {
	t.Parallel()
	var log hubtest.LogBuffer
	observer := &unreadableObserver{}
	w := newWorld(t, withLog(&log), withTeardown(func(td *workspace.Teardown) {
		td.RegisterArchiveObserver(observer)
	}))
	observer.w = w
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusFailed, CloneError: new("boom")})

	a := w.operate(t, "archive", w.uKey, "demo")

	if obj := a.decode(t, http.StatusOK); obj["status"] != "archived" || obj["clone_error"] != nil || obj["slug"] != "demo" {
		t.Errorf("answer slug %v status %v clone_error %v, want demo, archived and null", obj["slug"], obj["status"], obj["clone_error"])
	}
	var status, marker string
	if err := w.Store.Main().QueryRow(`SELECT status, maintenance FROM workspaces WHERE slug = 'demo'`).Scan(&status, &marker); err != nil {
		t.Fatal(err)
	}
	if status != string(workspace.StatusArchived) || marker != "" {
		t.Errorf("stored status %q marker %q, want archived and none", status, marker)
	}
	if !strings.Contains(log.String(), "slug=demo") || !strings.Contains(log.String(), "not a time") {
		t.Errorf("the log does not report the failed read with the slug:\n%s", log.String())
	}
}
