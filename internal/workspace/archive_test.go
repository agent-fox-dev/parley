package workspace_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// archiveEvent is the event an archive writes.
const archiveEvent = "parley.workspace.archive"

// archive sends POST /workspaces/<slug>/archive, with no body, with cred as
// the bearer credential.
func (w *world) archive(t *testing.T, cred, slug string) answer {
	t.Helper()
	status, b := w.Call(t, http.MethodPost, "/api/v1/workspaces/"+slug+"/archive", cred, nil)
	return answer{status, b}
}

// setMarker stores m as the maintenance marker of the workspace slug, as an
// operation running on it holds it.
func (w *world) setMarker(t *testing.T, slug string, m workspace.Marker) {
	t.Helper()
	if _, err := w.Store.Main().Exec(`UPDATE workspaces SET maintenance = ? WHERE slug = ?`, string(m), slug); err != nil {
		t.Fatal(err)
	}
}

// withSafetyPusher makes p the hub's safety-copy pusher.
func withSafetyPusher(p workspace.BranchesPusher) option {
	return withCompose(func(c *hub.Composition) {
		c.AdjustWorkspace(func(d *workspace.Deps) { d.SafetyPusher = p })
	})
}

// withTeardown has fn register collaborators on the hub's teardown set.
func withTeardown(fn func(*workspace.Teardown)) option {
	return withCompose(func(c *hub.Composition) { fn(c.Workspaces().Teardown()) })
}

// teardownLog records the calls of stub teardown collaborators, one line
// each, in the order they were made.
type teardownLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *teardownLog) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *teardownLog) recorded() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lines)
}

// stubTeardown is every collaborator of the teardown set at once. It
// records each call in log under its name and then fails with fail, when
// set. As a session closer it first writes the slugs it was given as a row
// of the table session_marks through the transaction, when mark is set; as
// an archive observer it records the status w's store holds for the
// workspace.
type stubTeardown struct {
	name string
	log  *teardownLog
	fail error
	mark bool
	w    *world
}

func (s *stubTeardown) CloseSessions(ctx context.Context, tx *sql.Tx, slugs []string, reason string) error {
	slugs = slices.Sorted(slices.Values(slugs))
	if s.mark {
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_marks (slugs) VALUES (?)`, strings.Join(slugs, " ")); err != nil {
			return err
		}
	}
	s.log.add("%s close %v %s", s.name, slugs, reason)
	return s.fail
}

func (s *stubTeardown) WorkspaceArchived(ctx context.Context, slug string, forks []string) error {
	var status workspace.Status
	if s.w != nil {
		rec, _, _ := workspace.BySlug(ctx, s.w.Store.Main(), slug)
		status = rec.Status
	}
	s.log.add("%s observe %s %v %s", s.name, slug, forks, status)
	return s.fail
}

func (s *stubTeardown) RemoveSandboxes(_ context.Context, slugs []string) error {
	s.log.add("%s sandboxes %v", s.name, slugs)
	return s.fail
}

func (s *stubTeardown) remove(_ context.Context, _ *sql.Tx, slug string) error {
	s.log.add("%s remove %s", s.name, slug)
	return s.fail
}

func TestArchive_ArchivesAndRecordsTheOutcome(t *testing.T) {
	t.Parallel()
	p := &stubBranchesPusher{}
	s := newSyncWorld(t, withSafetyPusher(p))
	ready, _ := s.workspace(t, syncSlug)

	// seen is the status the store held for the workspace when the safety
	// copy offered its branches.
	var mu sync.Mutex
	var seen workspace.Status
	// taking answers for every branch offered, in the order of the names,
	// that origin took the first n and refused the others.
	look := func(ctx context.Context) {
		rec, _, _ := workspace.BySlug(ctx, s.Store.Main(), syncSlug)
		mu.Lock()
		seen = rec.Status
		mu.Unlock()
	}
	taking := func(n int) func(context.Context, gitrepo.PushBranchesRequest) ([]gitrepo.BranchPush, error) {
		return func(ctx context.Context, req gitrepo.PushBranchesRequest) ([]gitrepo.BranchPush, error) {
			look(ctx)
			var got []gitrepo.BranchPush
			for i, b := range slices.Sorted(maps.Keys(req.Heads)) {
				r := gitrepo.BranchPush{Branch: b, Commit: req.Heads[b], Result: gitrepo.PushUpToDate}
				if i >= n {
					r.Result, r.Err = "", gitrepo.ErrNonFastForward
				}
				got = append(got, r)
			}
			return got, nil
		}
	}
	dialFails := func(ctx context.Context, _ gitrepo.PushBranchesRequest) ([]gitrepo.BranchPush, error) {
		look(ctx)
		return nil, errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	}
	for _, c := range []struct {
		name   string
		answer func(context.Context, gitrepo.PushBranchesRequest) ([]gitrepo.BranchPush, error)
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
			p.mu.Lock()
			p.heads, p.ctxErr, p.answer = nil, nil, c.answer
			p.mu.Unlock()
			mu.Lock()
			seen = ""
			mu.Unlock()
			s.fail(c.fault)
			defer s.fail("")
			events := len(s.eventsOf(t, archiveEvent))

			a := s.archive(t, s.uKey, syncSlug)

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
			evs := s.eventsOf(t, archiveEvent)
			if c.want == "" {
				if a.status != http.StatusInternalServerError || a.errorType(t) != "internal" {
					t.Errorf("answer = %d %s, want 500 internal", a.status, a.body)
				}
				if rec.Status != status || len(evs) != events {
					t.Errorf("status %s and %d archive events after a failed archive, want %s and none", rec.Status, len(evs)-events, status)
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
			if len(evs) != events+1 {
				t.Fatalf("%d archive events written, want 1", len(evs)-events)
			}
			ev := evs[events]
			var meta map[string]any
			if err := json.Unmarshal(ev.Metadata, &meta); err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"head_sha": nil, "origin_push": c.want, "forks": float64(c.forks)}
			if head != nil {
				want["head_sha"] = *head
			}
			if deref(ev.Workspace) != syncSlug || ev.ActorType != event.ActorAPIKey || deref(ev.ActorID) != s.u || !reflect.DeepEqual(meta, want) {
				t.Errorf("archive event of workspace %q by %s %q with %v, want workspace %q by %s %q with %v",
					deref(ev.Workspace), ev.ActorType, deref(ev.ActorID), meta, syncSlug, event.ActorAPIKey, s.u, want)
			}
		})
	}
}

func TestArchive_RefusesWithoutTouchingTheWorkspace(t *testing.T) {
	t.Parallel()
	p := &stubBranchesPusher{answer: func(context.Context, gitrepo.PushBranchesRequest) ([]gitrepo.BranchPush, error) {
		return nil, nil
	}}
	calls := &teardownLog{}
	stub := &stubTeardown{name: "stub", log: calls}
	w := newWorld(t, withSafetyPusher(p), withTeardown(func(td *workspace.Teardown) {
		td.RegisterSessionCloser(stub)
		td.RegisterArchiveObserver(stub)
	}))
	url := "https://origin.test/r.git"
	for _, rec := range []workspace.Workspace{
		{Slug: "demo", Status: workspace.StatusReady},
		{Slug: "demo--t1", Kind: access.KindFork, ParentSlug: new("demo"), Status: workspace.StatusReady},
		{Slug: "archived", Status: workspace.StatusArchived},
		{Slug: "archived-busy", Status: workspace.StatusArchived, Marker: workspace.MarkerDelete},
		{Slug: "ready-busy", Status: workspace.StatusReady, Marker: workspace.MarkerReclone},
		{Slug: "pending", Status: workspace.StatusPending},
		{Slug: "cloning", Status: workspace.StatusCloning},
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
		{"archived", http.StatusConflict, "workspace_archived"},
		{"archived-busy", http.StatusConflict, "workspace_busy"},
		{"ready-busy", http.StatusConflict, "workspace_busy"},
		{"pending", http.StatusConflict, "workspace_busy"},
		{"cloning", http.StatusConflict, "workspace_busy"},
	} {
		before, _ := w.workspace(t, c.slug)
		if a := w.archive(t, w.uKey, c.slug); a.status != c.status || a.errorType(t) != c.errorType {
			t.Errorf("archive of %s = %d %s, want %d %s", c.slug, a.status, a.body, c.status, c.errorType)
		}
		if after, _ := w.workspace(t, c.slug); !reflect.DeepEqual(after, before) {
			t.Errorf("archive of %s changed the record from %+v to %+v", c.slug, before, after)
		}
	}
	if heads, _ := p.calls(); len(heads) != 0 {
		t.Errorf("refused archives offered branches to origin %d times, want never", len(heads))
	}
	if got := calls.recorded(); len(got) != 0 {
		t.Errorf("refused archives called the session closer and the observers: %v, want no call", got)
	}
	if evs := w.eventsOf(t, archiveEvent); len(evs) != 0 {
		t.Errorf("refused archives wrote %d archive events, want none", len(evs))
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

	if a := s.archive(t, s.uKey, syncSlug); a.status != http.StatusOK {
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
	url := "https://origin.test/r.git"
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusReady, GitURL: &url})
	w.setMarker(t, "demo", workspace.MarkerArchive)

	w.Restart(t)

	if rec, _ := w.workspace(t, "demo"); rec.Marker != workspace.MarkerNone {
		t.Errorf("the marker is %q after the restart, want it released", rec.Marker)
	}
	if a := w.archive(t, w.uKey, "demo"); a.status != http.StatusOK {
		t.Errorf("archive after the restart = %d %s, want 200", a.status, a.body)
	}
}

// archiveWithForks starts a world with opts and the ready workspace demo,
// without a copy, so that the safety copy has nothing to offer, and its
// forks.
func archiveWithForks(t *testing.T, forks []string, opts ...option) *world {
	t.Helper()
	w := newWorld(t, opts...)
	url := "https://origin.test/r.git"
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusReady, GitURL: &url})
	for _, f := range forks {
		w.put(t, workspace.Workspace{Slug: f, Kind: access.KindFork, ParentSlug: new("demo"), Status: workspace.StatusReady})
	}
	return w
}

func TestArchive_EndsTheSessionsOfTheWorkspaceAndItsForks(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		closer bool
		fail   error
		// marks is the number of rows the closer's write leaves.
		marks int
	}{
		{name: "the closer ends them in the archive's transaction", closer: true, marks: 1},
		{name: "the closer fails and its writes are undone", closer: true, fail: errors.New("sessions store down")},
		{name: "no closer is registered"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var log hubtest.LogBuffer
			calls := &teardownLog{}
			opts := []option{withLog(&log)}
			if c.closer {
				closer := &stubTeardown{name: "closer", log: calls, fail: c.fail, mark: true}
				opts = append(opts, withTeardown(func(td *workspace.Teardown) { td.RegisterSessionCloser(closer) }))
			}
			w := archiveWithForks(t, []string{"demo--b", "demo--a"}, opts...)
			if _, err := w.Store.Main().Exec(`CREATE TABLE session_marks (slugs TEXT NOT NULL)`); err != nil {
				t.Fatal(err)
			}

			a := w.archive(t, w.uKey, "demo")

			if obj := a.decode(t, http.StatusOK); obj["status"] != "archived" {
				t.Errorf("answer status %v, want archived", obj["status"])
			}
			var want []string
			if c.closer {
				want = []string{"closer close [demo demo--a demo--b] workspace archived"}
			}
			if got := calls.recorded(); !slices.Equal(got, want) {
				t.Errorf("session closer calls %v, want %v", got, want)
			}
			var marks int
			if err := w.Store.Main().QueryRow(`SELECT count(*) FROM session_marks`).Scan(&marks); err != nil {
				t.Fatal(err)
			}
			rec, _ := w.workspace(t, "demo")
			if marks != c.marks || rec.Status != workspace.StatusArchived || len(w.eventsOf(t, archiveEvent)) != 1 {
				t.Errorf("%d closer rows, status %s and %d archive events stored, want %d, archived and 1",
					marks, rec.Status, len(w.eventsOf(t, archiveEvent)), c.marks)
			}
			if c.fail != nil && (!strings.Contains(log.String(), "slug=demo") || !strings.Contains(log.String(), c.fail.Error())) {
				t.Errorf("the log does not report the closer's failure with the slug:\n%s", log.String())
			}
		})
	}
}

func TestArchive_TellsEveryObserverOnceTheArchiveHasCommitted(t *testing.T) {
	t.Parallel()
	var log hubtest.LogBuffer
	calls := &teardownLog{}
	first := &stubTeardown{name: "first", log: calls, fail: errors.New("sandboxes unreachable")}
	second := &stubTeardown{name: "second", log: calls}
	w := archiveWithForks(t, []string{"demo--a"}, withLog(&log), withTeardown(func(td *workspace.Teardown) {
		td.RegisterArchiveObserver(first)
		td.RegisterArchiveObserver(second)
	}))
	first.w, second.w = w, w

	a := w.archive(t, w.uKey, "demo")

	if obj := a.decode(t, http.StatusOK); obj["status"] != "archived" {
		t.Errorf("answer status %v, want archived", obj["status"])
	}
	want := []string{"first observe demo [demo--a] archived", "second observe demo [demo--a] archived"}
	if got := calls.recorded(); !slices.Equal(got, want) {
		t.Errorf("observer calls %v, want %v", got, want)
	}
	if !strings.Contains(log.String(), "slug=demo") || !strings.Contains(log.String(), "sandboxes unreachable") {
		t.Errorf("the log does not report the observer's failure with the slug:\n%s", log.String())
	}
}
