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
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// deleteEvent is the event a delete writes for each workspace it removes.
const deleteEvent = "parley.workspace.delete"

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
// records through the transaction and returns their slugs, with a discard
// function that records its call; asked to delete one fork, it removes
// nothing and answers forkErr. As the sandbox remover it runs during, when
// set, after it has recorded the call.
type deleteStub struct {
	w      *world
	log    teardownLog
	tokens map[string][]string

	mu      sync.Mutex
	fail    string
	forkErr error
	during  func()
	txs     map[*sql.Tx]bool
	visible map[int]bool
}

// set makes the call whose line is fail fail, the deletes of a fork
// answer forkErr and the sandbox remover run during.
func (s *deleteStub) set(fail string, forkErr error, during func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail, s.forkErr, s.during = fail, forkErr, during
}

// since are the lines of the log after the first n.
func (s *deleteStub) since(n int) []string { return s.log.recorded()[n:] }

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
	err := s.call(ctx, nil, "", "", fmt.Sprintf("sandboxes %v", slugs))
	s.mu.Lock()
	during := s.during
	s.mu.Unlock()
	if during != nil {
		during()
	}
	return err
}

func (s *deleteStub) DeleteFork(ctx context.Context, slug string, caller identity.Principal) error {
	if err := s.call(ctx, nil, "", "", "delete fork "+slug+" by "+principalName(caller)); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.forkErr
}

func (s *deleteStub) RemoveForksOf(ctx context.Context, tx *sql.Tx, parent string, actor identity.Principal) ([]string, func(), error) {
	// The forks in descending order, which the delete sorts.
	rows, err := tx.QueryContext(ctx, `SELECT slug FROM workspaces WHERE parent_slug = ? ORDER BY slug DESC`, parent)
	if err != nil {
		return nil, nil, err
	}
	var forks []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, nil, err
		}
		forks = append(forks, slug)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE parent_slug = ?`, parent); err != nil {
		return nil, nil, err
	}
	discard := func() { s.log.add("discard committed=%t", s.committed()) }
	return forks, discard, s.call(ctx, tx, "", "", "forks of "+parent+" by "+principalName(actor))
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

// use makes stub every collaborator of a delete, with the removers secrets
// and changes, and the fork remover when forkRemover is set, in place of
// the collaborators registered so far.
func (w *world) use(stub *deleteStub, forkRemover bool) {
	td := w.svc.Teardown()
	td.Clear()
	td.RegisterTokenRevoker(stub)
	td.RegisterRemover(stub.remover("secrets"))
	td.RegisterRemover(stub.remover("changes"))
	td.RegisterSessionCloser(stub)
	td.RegisterSandboxRemover(stub)
	if forkRemover {
		td.RegisterForkRemover(stub)
	}
}

// deleteWorld starts a world with opts, with an empty teardown set and the
// table owned. When stub is set, each removal of a copy is recorded in its
// log with whether the delete had committed.
func deleteWorld(t *testing.T, stub *deleteStub, opts ...option) *world {
	t.Helper()
	opts = append(opts, withTeardown(func(td *workspace.Teardown) { td.Clear() }))
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
	if _, err := w.Store.Main().Exec(`CREATE TABLE owned (owner TEXT NOT NULL, slug TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	return w
}

// seedDemo stores the archived repository workspace demo with a setting
// and a copy, and its forks, and makes the table owned hold a row for each
// owner and each of them alone.
func (w *world) seedDemo(t *testing.T, forks ...string) {
	t.Helper()
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusArchived,
		Settings: map[string]json.RawMessage{"note": json.RawMessage(`"kept"`)}})
	for _, f := range forks {
		w.put(t, workspace.Workspace{Slug: f, Kind: access.KindFork, ParentSlug: new("demo"), Status: workspace.StatusReady})
	}
	if _, err := w.Store.Main().Exec(`DELETE FROM owned`); err != nil {
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
	forks := []string{"demo--b", "demo--a"}
	sandboxes := "sandboxes [demo demo--a demo--b]"
	copies := []string{"remove copy demo--a committed=true", "remove copy demo--b committed=true", "remove copy demo committed=true"}
	oneByOne := slices.Concat([]string{sandboxes}, removal("demo--a"), removal("demo--b"), removal("demo"), copies)
	oneByOneEvents := []map[string]any{
		{"kind": "fork", "parent_slug": "demo", "revoked_tokens": []any{"t2", "t9"}},
		{"kind": "fork", "parent_slug": "demo", "revoked_tokens": []any{}},
		{"kind": "repository", "parent_slug": nil, "revoked_tokens": []any{"t5"}, "deleted_forks": []any{"demo--a", "demo--b"}},
	}
	for _, c := range []struct {
		name  string
		admin bool
		forks []string
		// collaborators registers the stub as every collaborator, and
		// forkRemover as the fork remover too, which removes the forks
		// and writes no event.
		collaborators, forkRemover bool
		// want is the log of the calls, with the caller's name for "%s";
		// left are the rows of owned the delete leaves; events are the
		// workspaces of the delete events, in order, and meta their
		// metadata.
		want, left, events []string
		meta               []map[string]any
	}{
		{name: "the owner, with the forks removed one by one", forks: forks, collaborators: true, want: oneByOne,
			events: []string{"demo--a", "demo--b", "demo"}, meta: oneByOneEvents},
		{name: "the admin token", admin: true, forks: forks, collaborators: true, want: oneByOne,
			events: []string{"demo--a", "demo--b", "demo"}, meta: oneByOneEvents},
		{name: "the owner, with a fork remover", forks: forks, collaborators: true, forkRemover: true,
			want: slices.Concat([]string{sandboxes, "forks of demo by %s"}, removal("demo"),
				[]string{"remove copy demo committed=true", "discard committed=true"}),
			left: rowsOf("demo--a", "demo--b"), events: []string{"demo"}, meta: []map[string]any{
				{"kind": "repository", "parent_slug": nil, "revoked_tokens": []any{"t5"}, "deleted_forks": []any{"demo--a", "demo--b"}}}},
		{name: "no forks", collaborators: true,
			want:   slices.Concat([]string{"sandboxes [demo]"}, removal("demo"), []string{"remove copy demo committed=true"}),
			events: []string{"demo"}, meta: []map[string]any{
				{"kind": "repository", "parent_slug": nil, "revoked_tokens": []any{"t5"}, "deleted_forks": []any{}}}},
		{name: "no collaborators", forks: forks, want: copies, left: rowsOf("demo", "demo--a", "demo--b"),
			events: []string{"demo--a", "demo--b", "demo"}, meta: []map[string]any{
				{"kind": "fork", "parent_slug": "demo", "revoked_tokens": []any{}},
				{"kind": "fork", "parent_slug": "demo", "revoked_tokens": []any{}},
				{"kind": "repository", "parent_slug": nil, "revoked_tokens": []any{}, "deleted_forks": []any{"demo--a", "demo--b"}}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			stub := &deleteStub{tokens: map[string][]string{"demo--a": {"t9", "t2"}, "demo--b": {}, "demo": {"t5"}}}
			w := deleteWorld(t, stub)
			w.seedDemo(t, c.forks...)
			if c.collaborators {
				w.use(stub, c.forkRemover)
			}
			cred, actorType, actorID := w.uKey, event.ActorAPIKey, w.u
			if c.admin {
				cred, actorType, actorID = w.Token, event.ActorAdminToken, ""
			}
			// Events of the workspaces written before, which the delete keeps.
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

			a := w.operate(t, "delete", cred, "demo")

			if a.status != http.StatusNoContent || len(a.body) != 0 {
				t.Fatalf("delete = %d %q, want 204 with no body", a.status, a.body)
			}
			if n := w.workspaces(t); n != 0 {
				t.Errorf("%d workspace records after the delete, want none", n)
			}
			if exists(w.svc.RepoPath("demo")) {
				t.Error("the copy of demo is still on disk after the delete")
			}
			want := withCaller(c.want, identity.TypeAPIKey+" "+w.u)
			if c.admin {
				want = withCaller(c.want, identity.TypeAdmin)
			}
			if got := stub.log.recorded(); !slices.Equal(got, want) {
				t.Errorf("the delete's calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			txs, visible := stub.seen()
			if wantTxs, wantVisible := 0, []int(nil); c.collaborators {
				wantTxs, wantVisible = 1, []int{1 + len(c.forks)}
				if txs != wantTxs || !slices.Equal(visible, wantVisible) {
					t.Errorf("the calls got %d transactions and another connection saw %v workspace records, want 1 and %v", txs, visible, wantVisible)
				}
			} else if txs != 0 {
				t.Errorf("with no collaborators %d transactions were handed out, want none", txs)
			}
			if got := w.ownedRows(t); !slices.Equal(got, c.left) {
				t.Errorf("owned rows after the delete %v, want %v", got, c.left)
			}
			after, _, err := w.events.ReadAfter(ctx, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before)+len(c.events) || !reflect.DeepEqual(after[:len(before)], before) {
				t.Fatalf("the log after the delete holds %d events, want the %d before it unchanged and %d delete events",
					len(after), len(before), len(c.events))
			}
			for i, ev := range after[len(before):] {
				var meta map[string]any
				if err := json.Unmarshal(ev.Metadata, &meta); err != nil {
					t.Fatal(err)
				}
				if ev.Type != deleteEvent || deref(ev.Workspace) != c.events[i] || ev.ActorType != actorType ||
					deref(ev.ActorID) != actorID || !reflect.DeepEqual(meta, c.meta[i]) {
					t.Errorf("event %d: %s of %q by %s %q with %v, want %s of %q by %s %q with %v", i, ev.Type,
						deref(ev.Workspace), ev.ActorType, deref(ev.ActorID), meta, deleteEvent, c.events[i], actorType, actorID, c.meta[i])
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
			if obj := w.create(t, w.uKey, valid("demo")).decode(t, http.StatusCreated); obj["status"] != "pending" {
				t.Errorf("a create with the deleted slug answers status %v, want pending", obj["status"])
			}
		})
	}
}

func TestDelete_HandsAForkToTheForkRemover(t *testing.T) {
	t.Parallel()
	stub := &deleteStub{}
	w := deleteWorld(t, stub)
	w.seedDemo(t, "demo--a")
	parent, _ := w.workspace(t, "demo")
	busy := server.NewError("workspace_busy", http.StatusConflict, `workspace "demo--a" is busy; try again later`)
	for _, c := range []struct {
		name    string
		remover bool
		forkErr error
		// want is the answer, message its error's message; calls are the
		// collaborators' calls.
		want, message string
		calls         []string
	}{
		{name: "the fork remover deletes it", remover: true, want: "204", calls: []string{"delete fork demo--a by %s"}},
		{name: "the fork remover refuses", remover: true, forkErr: busy, want: "409 workspace_busy", message: busy.Message(),
			calls: []string{"delete fork demo--a by %s"}},
		{name: "no fork remover is registered", want: "500 internal"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w.use(stub, c.remover)
			stub.set("", c.forkErr, nil)
			made := len(stub.log.recorded())

			a := w.operate(t, "delete", w.uKey, "demo--a")

			if got := a.outcome(t); got != c.want || (a.status == http.StatusNoContent && len(a.body) != 0) {
				t.Errorf("delete of the fork = %d %q, want %s", a.status, a.body, c.want)
			}
			if c.message != "" {
				if _, message := a.refusal(t); message != c.message {
					t.Errorf("delete of the fork answers the message %q, want the fork remover's %q", message, c.message)
				}
			}
			if got, want := stub.since(made), withCaller(c.calls, identity.TypeAPIKey+" "+w.u); !slices.Equal(got, want) {
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
	stub := &deleteStub{}
	w := deleteWorld(t, stub)
	for _, c := range []struct {
		name string
		// fail is the call that fails, with the caller's name for "%s";
		// trigger is SQL that makes a write of the store fail; fork makes
		// the fork demo--c while the sandboxes are removed; want is the
		// answer, 500 internal when "".
		fail, trigger, want string
		forkRemover, fork   bool
	}{
		{name: "the sandbox remover", fail: "sandboxes [demo demo--a demo--b]"},
		{name: "the first remover on the first fork", fail: "secrets remove demo--a"},
		{name: "the last remover on the parent", fail: "changes remove demo"},
		{name: "the token revoker", fail: "revoke demo--b"},
		{name: "the fork remover", fail: "forks of demo by %s", forkRemover: true},
		{name: "the session closer", fail: "close [demo] workspace deleted"},
		{name: "the removal of the parent's record", trigger: `CREATE TRIGGER fault BEFORE DELETE ON workspaces
			WHEN old.slug = 'demo' BEGIN SELECT RAISE(ABORT, 'the record is kept'); END`},
		{name: "the write of the parent's event", trigger: `CREATE TRIGGER fault BEFORE INSERT ON hub_events
			WHEN new.workspace = 'demo' AND new.type = '` + deleteEvent + `' BEGIN SELECT RAISE(ABORT, 'no event'); END`},
		{name: "a fork made while the sandboxes are removed", fork: true, want: "409 workspace_busy"},
		{name: "a fork made while the sandboxes are removed, with a fork remover", fork: true, forkRemover: true,
			want: "409 workspace_busy"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w.seedDemo(t, "demo--a", "demo--b")
			w.use(stub, c.forkRemover)
			var during func()
			if c.fork {
				var once sync.Once
				during = func() {
					once.Do(func() {
						w.put(t, workspace.Workspace{Slug: "demo--c", Kind: access.KindFork, ParentSlug: new("demo"),
							Status: workspace.StatusReady})
					})
				}
			}
			stub.set(withCaller([]string{c.fail}, identity.TypeAPIKey+" "+w.u)[0], nil, during)
			if c.trigger != "" {
				if _, err := w.Store.Main().Exec(c.trigger); err != nil {
					t.Fatal(err)
				}
			}
			if c.want == "" {
				c.want = "500 internal"
			}
			made, events := len(stub.log.recorded()), len(w.eventsOf(t, deleteEvent))

			if a := w.operate(t, "delete", w.uKey, "demo"); a.outcome(t) != c.want {
				t.Errorf("delete = %d %s, want %s", a.status, a.body, c.want)
			}

			rec, _ := w.workspace(t, "demo")
			records := 3
			if c.fork {
				records = 4
			}
			if n := w.workspaces(t); n != records || rec.Status != workspace.StatusArchived || rec.Marker != workspace.MarkerNone {
				t.Errorf("%d records, demo %s with marker %q, want %d, archived and none", n, rec.Status, rec.Marker, records)
			}
			if got, want := w.ownedRows(t), rowsOf("demo", "demo--a", "demo--b"); !slices.Equal(got, want) {
				t.Errorf("owned rows %v, want all of them back: %v", got, want)
			}
			if n := len(w.eventsOf(t, deleteEvent)); n != events {
				t.Errorf("%d delete events stored, want none", n-events)
			}
			if !exists(w.svc.RepoPath("demo")) {
				t.Error("the copy of demo is gone after a failed delete")
			}
			got := stub.since(made)
			for _, line := range got {
				if strings.HasPrefix(line, "remove copy") || strings.HasPrefix(line, "discard") {
					t.Errorf("a failed delete discarded repository data: %v", got)
				}
			}
			if strings.HasPrefix(c.fail, "sandboxes") && len(got) != 1 {
				t.Errorf("calls after the sandbox remover failed: %v, want none", got)
			}

			stub.set("", nil, nil)
			if _, err := w.Store.Main().Exec(`DROP TRIGGER IF EXISTS fault`); err != nil {
				t.Fatal(err)
			}
			made = len(stub.log.recorded())
			if a := w.operate(t, "delete", w.uKey, "demo"); a.status != http.StatusNoContent {
				t.Fatalf("the repeated delete = %d %s, want 204", a.status, a.body)
			}
			if got := stub.since(made); c.fork && !slices.Contains(got, "sandboxes [demo demo--a demo--b demo--c]") {
				t.Errorf("the repeated delete's calls %v, want the sandboxes of demo and of its three forks removed", got)
			}
		})
	}
}

func TestDelete_AnswersDoneWhenTheCopyCannotBeRemoved(t *testing.T) {
	t.Parallel()
	var log hubtest.LogBuffer
	w := deleteWorld(t, nil, withLog(&log),
		withRemoveCopy(func(string) error { return errors.New("device busy") }))
	w.seedDemo(t)

	if a := w.operate(t, "delete", w.uKey, "demo"); a.status != http.StatusNoContent || len(a.body) != 0 {
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

func TestDelete_LeavesTheCopyOfAWorkspaceCreatedAgainMeanwhile(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	o.History()
	var w *world
	created, cloned := make(chan int, 1), make(chan error, 1)
	var once sync.Once
	remove := func(dir string) error {
		once.Do(func() {
			// The delete has committed, so the slug is free: a create with it
			// lands here and its clone work makes a copy at the same place.
			created <- <-w.send(http.MethodPost, w.uKey, "/api/v1/workspaces", map[string]any{"slug": "demo", "git_url": o.URL()})
			done := make(chan error, 1)
			go func() { done <- w.svc.Clone(context.Background(), "demo") }()
			select {
			case err := <-done:
				cloned <- err
			case <-time.After(time.Second):
				go func() { cloned <- <-done }()
			}
		})
		return gitrepo.Remove(dir)
	}
	w = deleteWorld(t, nil, withGitOrigin(o), withRemoveCopy(remove))
	w.seedDemo(t)

	if a := w.operate(t, "delete", w.uKey, "demo"); a.status != http.StatusNoContent {
		t.Fatalf("delete = %d %s, want 204", a.status, a.body)
	}
	if status := <-created; status != http.StatusCreated {
		t.Fatalf("the create with the deleted slug = %d, want 201", status)
	}
	if err := <-cloned; err != nil {
		t.Fatalf("the clone work of the new demo: %v", err)
	}
	if rec, _ := w.workspace(t, "demo"); rec.Status != workspace.StatusReady {
		t.Fatalf("the new demo is %s (%v), want ready", rec.Status, deref(rec.CloneError))
	}
	if !exists(w.svc.RepoPath("demo")) {
		t.Fatal("the new demo is ready with no copy on disk: the delete removed it")
	}
	sameRefs(t, w.svc.RepoPath("demo"), o)
}
