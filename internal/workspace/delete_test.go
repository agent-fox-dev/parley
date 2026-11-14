package workspace_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// deleteEvent is the event a delete writes for each workspace it removes.
const deleteEvent = "parley.workspace.delete"

// deleteWorkspace sends DELETE /workspaces/<slug> with cred as the bearer
// credential.
func (w *world) deleteWorkspace(t *testing.T, cred, slug string) answer {
	t.Helper()
	status, b := w.Call(t, http.MethodDelete, "/api/v1/workspaces/"+slug, cred, nil)
	return answer{status, b}
}

// owners are the collaborators of a delete whose data the stubs keep, as
// one row of the table owned for each owner and workspace.
var owners = []string{"changes", "secrets", "sessions", "tokens"}

// deleteStub is every collaborator of a delete at once. Each call is
// recorded in log as one line, with the transaction it was given and how
// many workspace records another connection of the store sees at that
// moment. The token revoker, the removers and the session closer each
// delete through the transaction their owner's row of the table owned for
// the slug; the call whose line is fail fails after that. As a token
// revoker it returns tokens[slug]. As a fork remover it deletes the forks'
// records through the transaction when dropForks is set and returns forks,
// with a discard function that records its call; asked to delete one fork,
// it removes nothing and answers forkErr.
type deleteStub struct {
	w         *world
	log       teardownLog
	tokens    map[string][]string
	forks     []string
	dropForks bool
	forkErr   error

	mu      sync.Mutex
	fail    string
	txs     map[*sql.Tx]bool
	visible map[int]bool
}

func (s *deleteStub) failing(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = line
}

func (s *deleteStub) call(ctx context.Context, tx *sql.Tx, owner, slug, line string) error {
	var n int
	if err := s.w.Store.Main().QueryRowContext(ctx, `SELECT count(*) FROM workspaces`).Scan(&n); err != nil {
		return err
	}
	s.mu.Lock()
	if s.txs == nil {
		s.txs, s.visible = map[*sql.Tx]bool{}, map[int]bool{}
	}
	if tx != nil {
		s.txs[tx] = true
	}
	s.visible[n] = true
	fail := s.fail
	s.mu.Unlock()
	s.log.add("%s", line)
	if owner != "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM owned WHERE owner = ? AND slug = ?`, owner, slug); err != nil {
			return err
		}
	}
	if line == fail {
		return errors.New("stub failure: " + line)
	}
	return nil
}

// seen are the transactions the calls were given and the numbers of
// workspace records another connection saw during them.
func (s *deleteStub) seen() (txs int, visible []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for n := range s.visible {
		visible = append(visible, n)
	}
	slices.Sort(visible)
	return len(s.txs), visible
}

// committed tells whether the delete of demo has committed.
func (s *deleteStub) committed() bool {
	_, found, _ := workspace.BySlug(context.Background(), s.w.Store.Main(), "demo")
	return !found
}

func (s *deleteStub) RevokeBoundTo(ctx context.Context, tx *sql.Tx, slugs []string) ([]string, error) {
	if err := s.call(ctx, tx, "tokens", slugs[0], "revoke "+strings.Join(slugs, " ")); err != nil {
		return nil, err
	}
	return s.tokens[slugs[0]], nil
}

func (s *deleteStub) remover(name string) workspace.Remover {
	return func(ctx context.Context, tx *sql.Tx, slug string) error {
		return s.call(ctx, tx, name, slug, name+" remove "+slug)
	}
}

func (s *deleteStub) CloseSessions(ctx context.Context, tx *sql.Tx, slugs []string, reason string) error {
	return s.call(ctx, tx, "sessions", slugs[0], fmt.Sprintf("close %v %s", slugs, reason))
}

func (s *deleteStub) RemoveSandboxes(ctx context.Context, slugs []string) error {
	return s.call(ctx, nil, "", "", fmt.Sprintf("sandboxes %v", slugs))
}

func (s *deleteStub) DeleteFork(ctx context.Context, slug string, caller identity.Principal) error {
	if err := s.call(ctx, nil, "", "", "delete fork "+slug+" by "+principalName(caller)); err != nil {
		return err
	}
	return s.forkErr
}

func (s *deleteStub) RemoveForksOf(ctx context.Context, tx *sql.Tx, parent string, actor identity.Principal) ([]string, func(), error) {
	if s.dropForks {
		if _, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE parent_slug = ?`, parent); err != nil {
			return nil, nil, err
		}
	}
	discard := func() { s.log.add("discard committed=%t", s.committed()) }
	return slices.Clone(s.forks), discard, s.call(ctx, tx, "", "", "forks of "+parent+" by "+principalName(actor))
}

// withCaller is lines with caller, the name principalName gives, for "%s".
func withCaller(lines []string, caller string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = strings.ReplaceAll(line, "%s", caller)
	}
	return out
}

// principalName is how a fork remover's stub records the caller.
func principalName(p identity.Principal) string {
	if p.User == nil {
		return p.Type
	}
	return p.Type + " " + p.User.ID
}

// registerAll registers stub as every collaborator of a delete, with the
// removers secrets and changes, and as the fork remover when forks is set.
func registerAll(stub *deleteStub, forks bool) func(*workspace.Teardown) {
	return func(td *workspace.Teardown) {
		td.RegisterTokenRevoker(stub)
		td.RegisterRemover(stub.remover("secrets"))
		td.RegisterRemover(stub.remover("changes"))
		td.RegisterSessionCloser(stub)
		td.RegisterSandboxRemover(stub)
		if forks {
			td.RegisterForkRemover(stub)
		}
	}
}

// deleteWorld starts a world with opts whose teardown set holds what
// register registers and nothing else, with the archived repository
// workspace demo with a setting and a copy, its forks, and in the table
// owned a row for each owner and workspace. When stub is set, each removal
// of a copy is recorded in its log with whether the delete had committed.
func deleteWorld(t *testing.T, stub *deleteStub, forks []string, register func(*workspace.Teardown), opts ...option) *world {
	t.Helper()
	opts = append(opts, withTeardown(func(td *workspace.Teardown) {
		td.Clear()
		if register != nil {
			register(td)
		}
	}))
	if stub != nil {
		opts = append(opts, withCompose(func(c *hub.Composition) {
			c.AdjustWorkspace(func(d *workspace.Deps) {
				next := d.RemoveCopy
				if next == nil {
					next = gitrepo.Remove
				}
				d.RemoveCopy = func(dir string) error {
					stub.log.add("remove copy %s committed=%t", strings.TrimSuffix(filepath.Base(dir), ".git"), stub.committed())
					return next(dir)
				}
			})
		}))
	}
	w := newWorld(t, opts...)
	if stub != nil {
		stub.w = w
	}
	url := "https://origin.test/r.git"
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusArchived, GitURL: &url,
		Settings: map[string]json.RawMessage{"note": json.RawMessage(`"kept"`)}})
	for _, f := range forks {
		w.put(t, workspace.Workspace{Slug: f, Kind: access.KindFork, ParentSlug: new("demo"), Status: workspace.StatusReady})
	}
	if _, err := w.Store.Main().Exec(`CREATE TABLE owned (owner TEXT NOT NULL, slug TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, slug := range append([]string{"demo"}, forks...) {
		for _, o := range owners {
			if _, err := w.Store.Main().Exec(`INSERT INTO owned (owner, slug) VALUES (?, ?)`, o, slug); err != nil {
				t.Fatal(err)
			}
		}
	}
	makeCopy(t, w.svc.RepoPath("demo"))
	return w
}

// makeCopy puts repository data at dir.
func makeCopy(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ownedRows are the rows of the table owned, as "owner slug", sorted.
func (w *world) ownedRows(t *testing.T) []string {
	t.Helper()
	rows, err := w.Store.Main().Query(`SELECT owner || ' ' || slug FROM owned ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	return got
}

// removal is the log of the removal of slug's owned data by this package.
func removal(slug string) []string {
	return []string{"revoke " + slug, "secrets remove " + slug, "changes remove " + slug,
		"close [" + slug + "] workspace deleted"}
}

// rowsOf are the rows of the table owned of the slugs, as ownedRows gives
// them.
func rowsOf(slugs ...string) []string {
	var rows []string
	for _, o := range owners {
		for _, s := range slugs {
			rows = append(rows, o+" "+s)
		}
	}
	slices.Sort(rows)
	return rows
}

func TestDelete_RemovesTheWorkspaceItsForksAndWhatTheyOwn(t *testing.T) {
	t.Parallel()
	sandboxes := "sandboxes [demo demo--a demo--b]"
	oneByOne := slices.Concat([]string{sandboxes}, removal("demo--a"), removal("demo--b"), removal("demo"),
		[]string{"remove copy demo--a committed=true", "remove copy demo--b committed=true", "remove copy demo committed=true"})
	for _, c := range []struct {
		name  string
		admin bool
		forks bool
		// want is the log of the calls, with the caller's name for
		// "%s"; left are the rows of owned the delete leaves.
		want []string
		left []string
	}{
		{name: "the owner, with the forks removed one by one", want: oneByOne},
		{name: "the admin token", admin: true, want: oneByOne},
		{name: "the owner, with a fork remover", forks: true,
			want: slices.Concat([]string{sandboxes, "forks of demo by %s"}, removal("demo"),
				[]string{"remove copy demo committed=true", "discard committed=true"}),
			left: rowsOf("demo--a", "demo--b")},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			stub := &deleteStub{forks: []string{"demo--a", "demo--b"}, dropForks: true}
			w := deleteWorld(t, stub, []string{"demo--b", "demo--a"}, registerAll(stub, c.forks))
			cred, caller := w.uKey, identity.TypeAPIKey+" "+w.u
			if c.admin {
				cred = w.Token
			}

			a := w.deleteWorkspace(t, cred, "demo")

			if a.status != http.StatusNoContent || len(a.body) != 0 {
				t.Fatalf("delete = %d %q, want 204 with no body", a.status, a.body)
			}
			if n := w.workspaces(t); n != 0 {
				t.Errorf("%d workspace records after the delete, want none", n)
			}
			if exists(w.svc.RepoPath("demo")) {
				t.Error("the copy of demo is still on disk after the delete")
			}
			want := withCaller(c.want, caller)
			if got := stub.log.recorded(); !slices.Equal(got, want) {
				t.Errorf("the delete's calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			if txs, visible := stub.seen(); txs != 1 || !slices.Equal(visible, []int{3}) {
				t.Errorf("the calls got %d transactions and another connection saw %v workspace records, want 1 and [3]", txs, visible)
			}
			if got := w.ownedRows(t); !slices.Equal(got, c.left) {
				t.Errorf("owned rows after the delete %v, want %v", got, c.left)
			}
			if obj := w.create(t, w.uKey, valid("demo")).decode(t, http.StatusCreated); obj["status"] != "pending" {
				t.Errorf("a create with the deleted slug answers status %v, want pending", obj["status"])
			}
		})
	}
}

func TestDelete_RefusesWithoutTouchingTheWorkspace(t *testing.T) {
	t.Parallel()
	stub := &deleteStub{}
	w := deleteWorld(t, stub, nil, registerAll(stub, true))
	url := "https://origin.test/r.git"
	for _, rec := range []workspace.Workspace{
		{Slug: "ready", Status: workspace.StatusReady},
		{Slug: "failed", Status: workspace.StatusFailed},
		{Slug: "pending", Status: workspace.StatusPending},
		{Slug: "cloning", Status: workspace.StatusCloning},
		{Slug: "archived-busy", Status: workspace.StatusArchived, Marker: workspace.MarkerArchive},
	} {
		rec.GitURL = &url
		w.put(t, rec)
		makeCopy(t, w.svc.RepoPath(rec.Slug))
	}
	for _, c := range []struct{ slug, errorType string }{
		{"ready", "workspace_not_archived"},
		{"failed", "workspace_not_archived"},
		{"pending", "workspace_not_archived"},
		{"cloning", "workspace_not_archived"},
		{"archived-busy", "workspace_busy"},
	} {
		before, _ := w.workspace(t, c.slug)
		if a := w.deleteWorkspace(t, w.uKey, c.slug); a.status != http.StatusConflict || a.errorType(t) != c.errorType {
			t.Errorf("delete of %s = %d %s, want 409 %s", c.slug, a.status, a.body, c.errorType)
		}
		if after, _ := w.workspace(t, c.slug); !reflect.DeepEqual(after, before) {
			t.Errorf("delete of %s changed the record from %+v to %+v", c.slug, before, after)
		}
		if !exists(w.svc.RepoPath(c.slug)) {
			t.Errorf("delete of %s removed its copy", c.slug)
		}
	}
	if got := stub.log.recorded(); len(got) != 0 {
		t.Errorf("refused deletes called collaborators: %v, want no call", got)
	}
	if evs := w.eventsOf(t, deleteEvent); len(evs) != 0 {
		t.Errorf("refused deletes wrote %d delete events, want none", len(evs))
	}
}

func TestDelete_HandsAForkToTheForkRemover(t *testing.T) {
	t.Parallel()
	busy := server.NewError("workspace_busy", http.StatusConflict, `workspace "demo--a" is busy; try again later`)
	for _, c := range []struct {
		name    string
		remover bool
		forkErr error
		// want is the answer, message its error's message; calls are the
		// collaborators' calls, with the caller's name for "%s".
		want, message string
		calls         []string
	}{
		{name: "the fork remover deletes it", remover: true, want: "204", calls: []string{"delete fork demo--a by %s"}},
		{name: "the fork remover refuses", remover: true, forkErr: busy, want: "409 workspace_busy", message: busy.Message(),
			calls: []string{"delete fork demo--a by %s"}},
		{name: "no fork remover is registered", want: "500 internal"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			stub := &deleteStub{forkErr: c.forkErr}
			w := deleteWorld(t, stub, []string{"demo--a"}, registerAll(stub, c.remover))
			parent, _ := w.workspace(t, "demo")

			a := w.deleteWorkspace(t, w.uKey, "demo--a")

			if got := a.outcome(t); got != c.want || (a.status == http.StatusNoContent && len(a.body) != 0) {
				t.Errorf("delete of the fork = %d %q, want %s", a.status, a.body, c.want)
			}
			if c.message != "" {
				if _, message := a.refusal(t); message != c.message {
					t.Errorf("delete of the fork answers the message %q, want the fork remover's %q", message, c.message)
				}
			}
			if got, want := stub.log.recorded(), withCaller(c.calls, identity.TypeAPIKey+" "+w.u); !slices.Equal(got, want) {
				t.Errorf("the delete's calls %v, want %v", got, want)
			}
			if after, _ := w.workspace(t, "demo"); !reflect.DeepEqual(after, parent) {
				t.Errorf("the delete of the fork changed its parent from %+v to %+v", parent, after)
			}
			if _, found := w.workspace(t, "demo--a"); !found {
				t.Error("the fork's record is gone, which only the fork remover removes")
			}
			if got, want := w.ownedRows(t), rowsOf("demo", "demo--a"); !slices.Equal(got, want) {
				t.Errorf("owned rows %v, want all of them: %v", got, want)
			}
			if evs := w.eventsOf(t, deleteEvent); len(evs) != 0 {
				t.Errorf("%d delete events stored, want none", len(evs))
			}
		})
	}
}

func TestDelete_AFailingStepChangesNothing(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// fail is the call that fails, with the caller's name for "%s";
		// trigger is SQL that makes a write of the store fail.
		fail    string
		trigger string
		forks   bool
	}{
		{name: "the sandbox remover", fail: "sandboxes [demo demo--a demo--b]"},
		{name: "the first remover on the first fork", fail: "secrets remove demo--a"},
		{name: "the last remover on the parent", fail: "changes remove demo"},
		{name: "the token revoker", fail: "revoke demo--b"},
		{name: "the fork remover", fail: "forks of demo by %s", forks: true},
		{name: "the session closer", fail: "close [demo] workspace deleted"},
		{name: "the removal of the parent's record", trigger: `CREATE TRIGGER fault BEFORE DELETE ON workspaces
			WHEN old.slug = 'demo' BEGIN SELECT RAISE(ABORT, 'the record is kept'); END`},
		{name: "the write of the parent's event", trigger: `CREATE TRIGGER fault BEFORE INSERT ON hub_events
			WHEN new.workspace = 'demo' AND new.type = '` + deleteEvent + `' BEGIN SELECT RAISE(ABORT, 'no event'); END`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			stub := &deleteStub{forks: []string{"demo--a", "demo--b"}, dropForks: true}
			w := deleteWorld(t, stub, []string{"demo--a", "demo--b"}, registerAll(stub, c.forks))
			stub.failing(withCaller([]string{c.fail}, identity.TypeAPIKey+" "+w.u)[0])
			if c.trigger != "" {
				if _, err := w.Store.Main().Exec(c.trigger); err != nil {
					t.Fatal(err)
				}
			}

			a := w.deleteWorkspace(t, w.uKey, "demo")

			if a.status != http.StatusInternalServerError || a.errorType(t) != "internal" {
				t.Errorf("delete = %d %s, want 500 internal", a.status, a.body)
			}
			rec, _ := w.workspace(t, "demo")
			if n := w.workspaces(t); n != 3 || rec.Status != workspace.StatusArchived || rec.Marker != workspace.MarkerNone {
				t.Errorf("%d records, demo %s with marker %q, want 3, archived and none", n, rec.Status, rec.Marker)
			}
			if got, want := w.ownedRows(t), rowsOf("demo", "demo--a", "demo--b"); !slices.Equal(got, want) {
				t.Errorf("owned rows %v, want all of them back: %v", got, want)
			}
			if evs := w.eventsOf(t, deleteEvent); len(evs) != 0 {
				t.Errorf("%d delete events stored, want none", len(evs))
			}
			if !exists(w.svc.RepoPath("demo")) {
				t.Error("the copy of demo is gone after a failed delete")
			}
			got := stub.log.recorded()
			for _, line := range got {
				if strings.HasPrefix(line, "remove copy") || strings.HasPrefix(line, "discard") {
					t.Errorf("a failed delete discarded repository data: %v", got)
				}
			}
			if strings.HasPrefix(c.fail, "sandboxes") && len(got) != 1 {
				t.Errorf("calls after the sandbox remover failed: %v, want none", got)
			}

			stub.failing("")
			if _, err := w.Store.Main().Exec(`DROP TRIGGER IF EXISTS fault`); err != nil {
				t.Fatal(err)
			}
			if a := w.deleteWorkspace(t, w.uKey, "demo"); a.status != http.StatusNoContent {
				t.Errorf("the repeated delete = %d %s, want 204", a.status, a.body)
			}
		})
	}
}

func TestDelete_WritesOneEventPerRemovedWorkspace(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		forks []string
		// remover registers a fork remover that returns forks and writes
		// no event.
		remover bool
		tokens  map[string][]string
		// want are the events' workspaces and metadata, in the log's order.
		want []string
		meta []map[string]any
	}{
		{name: "the forks removed one by one", forks: []string{"demo--b", "demo--a"},
			tokens: map[string][]string{"demo--a": {"t9", "t2"}, "demo--b": {}, "demo": {"t5"}},
			want:   []string{"demo--a", "demo--b", "demo"},
			meta: []map[string]any{
				{"kind": "fork", "parent_slug": "demo", "revoked_tokens": []any{"t2", "t9"}},
				{"kind": "fork", "parent_slug": "demo", "revoked_tokens": []any{}},
				{"kind": "repository", "parent_slug": nil, "revoked_tokens": []any{"t5"}, "deleted_forks": []any{"demo--a", "demo--b"}},
			}},
		{name: "no forks", want: []string{"demo"},
			meta: []map[string]any{{"kind": "repository", "parent_slug": nil, "revoked_tokens": []any{}, "deleted_forks": []any{}}}},
		{name: "a fork remover", forks: []string{"demo--b", "demo--a"}, remover: true, want: []string{"demo"},
			meta: []map[string]any{{"kind": "repository", "parent_slug": nil, "revoked_tokens": []any{}, "deleted_forks": []any{"demo--a", "demo--b"}}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			stub := &deleteStub{tokens: c.tokens, forks: []string{"demo--b", "demo--a"}, dropForks: true}
			w := deleteWorld(t, stub, c.forks, registerAll(stub, c.remover))

			if a := w.deleteWorkspace(t, w.uKey, "demo"); a.status != http.StatusNoContent {
				t.Fatalf("delete = %d %s, want 204", a.status, a.body)
			}
			evs := w.eventsOf(t, deleteEvent)
			if len(evs) != len(c.want) {
				t.Fatalf("%d delete events, want %d", len(evs), len(c.want))
			}
			for i, ev := range evs {
				var meta map[string]any
				if err := json.Unmarshal(ev.Metadata, &meta); err != nil {
					t.Fatal(err)
				}
				if deref(ev.Workspace) != c.want[i] || ev.ActorType != event.ActorAPIKey || deref(ev.ActorID) != w.u ||
					!reflect.DeepEqual(meta, c.meta[i]) {
					t.Errorf("event %d: workspace %q by %s %q with %v, want %q by %s %q with %v", i, deref(ev.Workspace),
						ev.ActorType, deref(ev.ActorID), meta, c.want[i], event.ActorAPIKey, w.u, c.meta[i])
				}
				// A fork's event stays visible to its parent's readers.
				var parent sql.NullString
				if err := w.Store.Main().QueryRow(`SELECT parent_slug FROM hub_events WHERE seq = ?`, ev.Seq).Scan(&parent); err != nil {
					t.Fatal(err)
				}
				if want := c.meta[i]["parent_slug"] != nil; parent.Valid != want {
					t.Errorf("event %d stores parent %q, want one = %v", i, parent.String, want)
				}
			}
		})
	}
}

func TestDelete_KeepsTheEarlierEventsOfTheDeletedWorkspaces(t *testing.T) {
	t.Parallel()
	w := deleteWorld(t, nil, []string{"demo--a"}, nil)
	ctx := context.Background()
	err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		for _, slug := range []string{"demo", "demo", "demo", "demo--a"} {
			if _, err := w.events.Emit(ctx, tx, event.Input{Type: "parley.workspace.note", Workspace: &slug,
				ActorType: event.ActorSystem, ResourceType: "workspace", ResourceID: slug}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _, err := w.events.ReadAfter(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	if a := w.deleteWorkspace(t, w.uKey, "demo"); a.status != http.StatusNoContent {
		t.Fatalf("delete = %d %s, want 204", a.status, a.body)
	}

	after, _, err := w.events.ReadAfter(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+2 || !reflect.DeepEqual(after[:len(before)], before) {
		t.Fatalf("the log after the delete holds %d events, want the %d before it unchanged and 2 more", len(after), len(before))
	}
	for i, slug := range []string{"demo--a", "demo"} {
		if ev := after[len(before)+i]; ev.Type != deleteEvent || deref(ev.Workspace) != slug {
			t.Errorf("event %d after the delete is %s of %q, want %s of %q", i, ev.Type, deref(ev.Workspace), deleteEvent, slug)
		}
	}
}

func TestDelete_AnswersDoneWhenTheCopyCannotBeRemoved(t *testing.T) {
	t.Parallel()
	var log hubtest.LogBuffer
	w := deleteWorld(t, nil, nil, nil, withLog(&log),
		withRemoveCopy(func(string) error { return errors.New("device busy") }))

	if a := w.deleteWorkspace(t, w.uKey, "demo"); a.status != http.StatusNoContent || len(a.body) != 0 {
		t.Fatalf("delete = %d %q, want 204 with no body", a.status, a.body)
	}
	if _, found := w.workspace(t, "demo"); found {
		t.Error("the record of demo is still stored")
	}
	if evs := w.eventsOf(t, deleteEvent); len(evs) != 1 {
		t.Errorf("%d delete events, want 1", len(evs))
	}
	var line string
	for l := range strings.SplitSeq(log.String(), "\n") {
		if strings.Contains(l, "device busy") {
			line = l
		}
	}
	if !strings.Contains(line, "slug=demo") || !strings.Contains(line, w.svc.RepoPath("demo")) {
		t.Errorf("no log line names the failure with the slug and the path:\n%s", log.String())
	}
}

func TestDelete_WithNoCollaboratorsChangesOnlyTheWorkspaceRecords(t *testing.T) {
	t.Parallel()
	w := archiveWithForks(t, []string{"demo--a"}, withTeardown(func(td *workspace.Teardown) { td.Clear() }))

	if a := w.archive(t, w.uKey, "demo"); a.status != http.StatusOK {
		t.Fatalf("archive = %d %s, want 200", a.status, a.body)
	}
	if a := w.deleteWorkspace(t, w.uKey, "demo"); a.status != http.StatusNoContent {
		t.Fatalf("delete = %d %s, want 204", a.status, a.body)
	}
	if n := w.workspaces(t); n != 0 {
		t.Errorf("%d workspace records after the delete, want none", n)
	}
	evs := w.eventsOf(t, deleteEvent)
	if len(evs) != 2 {
		t.Fatalf("%d delete events, want 2", len(evs))
	}
	for _, ev := range evs {
		if !strings.Contains(string(ev.Metadata), `"revoked_tokens":[]`) {
			t.Errorf("delete event of %q has metadata %s, want revoked_tokens []", deref(ev.Workspace), ev.Metadata)
		}
	}
}
