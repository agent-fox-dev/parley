package event

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/scrub"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/store"
)

// clockStart is where a fake clock starts unless the test sets another time.
var clockStart = time.Date(2026, 11, 14, 9, 30, 15, 0, time.UTC)

// fakeClock is a Clock whose time moves only when the test moves it. Its
// timers fire when Advance or Set reaches them.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	// changed is closed and replaced whenever a timer is added, so that
	// WaitTimers wakes up without polling.
	changed chan struct{}
}

// fakeTimer is a timer of a fakeClock.
type fakeTimer struct {
	clock *fakeClock
	at    time.Time
	c     chan time.Time
}

// newFakeClock returns a fake clock set to now.
func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{now: now, changed: make(chan struct{})}
}

// Now returns the clock's time.
func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// NewTimer returns a timer that fires once the clock reaches now + d; a
// timer of d <= 0 fires at once.
func (f *fakeClock) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{clock: f, at: f.now.Add(d), c: make(chan time.Time, 1)}
	if d <= 0 {
		t.c <- f.now
		return t
	}
	f.timers = append(f.timers, t)
	close(f.changed)
	f.changed = make(chan struct{})
	return t
}

// C is the channel the timer fires on.
func (t *fakeTimer) C() <-chan time.Time { return t.c }

// Stop takes the timer off its clock; it reports whether the timer was still
// pending.
func (t *fakeTimer) Stop() bool {
	f := t.clock
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, p := range f.timers {
		if p == t {
			f.timers = append(f.timers[:i], f.timers[i+1:]...)
			return true
		}
	}
	return false
}

// Set moves the clock to now and fires every timer that is then due.
func (f *fakeClock) Set(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = now
	pending := f.timers[:0]
	for _, t := range f.timers {
		if t.at.After(now) {
			pending = append(pending, t)
			continue
		}
		t.c <- now
	}
	f.timers = pending
}

// Advance moves the clock forward by d and fires every timer that is then
// due.
func (f *fakeClock) Advance(d time.Duration) { f.Set(f.Now().Add(d)) }

// WaitTimers waits until at least n timers are pending on the clock, so that
// a test advances it only once the code under test waits on it. It fails the
// test after 5 s.
func (f *fakeClock) WaitTimers(t testing.TB, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		f.mu.Lock()
		pending, changed := len(f.timers), f.changed
		f.mu.Unlock()
		if pending >= n {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("%d timers pending after 5 s, want at least %d", pending, n)
		}
	}
}

// testRowsStep creates the table the tests write their state changes to,
// beside the events that report them.
var testRowsStep = store.Step{
	Name:   "test_rows",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE test_rows (name TEXT NOT NULL PRIMARY KEY)`)
		return err
	},
}

// fixture is an event component on a real store in a directory of the test,
// with a fake clock, a real scrubber and the real access rule over the
// store's workspace records and memberships.
type fixture struct {
	t        testing.TB
	dir      string
	store    *store.Store
	clock    *fakeClock
	scrubber *scrub.Scrubber
	access   *access.Service
	events   *Component
	// failMemberships makes every read of the memberships fail, for the
	// access rule and the component alike.
	failMemberships atomic.Bool
	// shutdown is the component's shutdown notification, and shutDown
	// closes it, as the hub does when it begins to stop.
	shutdown chan struct{}
	shutDown func()
	// stop ends the component's Run and waits for it, before the store
	// closes under it.
	stop func()
}

// newFixture opens a store in a fresh directory with the event schema, the
// identity and workspace records and a test table, and binds a component to
// it. The
// store is closed when the test ends.
func newFixture(t testing.TB) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: t.TempDir(), clock: newFakeClock(clockStart), scrubber: scrub.New()}
	f.open()
	return f
}

// open opens the fixture's store, binds a new component to it and runs it,
// as a hub does at every start.
func (f *fixture) open() {
	f.t.Helper()
	st, err := store.Open(context.Background(), store.Options{
		DatabasePath:  filepath.Join(f.dir, "parley.db"),
		WorkspaceRoot: filepath.Join(f.dir, "workspaces"),
	}, []store.Step{identity.AdminSettingsStep, identity.SchemaStep, access.SchemaStep, SchemaStep, testRowsStep})
	if err != nil {
		f.t.Fatalf("open store: %v", err)
	}
	f.t.Cleanup(func() { st.Close() })
	f.store = st
	f.access = &access.Service{}
	f.access.Bind(access.Deps{Records: access.StoreRecords(st.Main()), Memberships: f.memberships()})
	shutdown := make(chan struct{})
	f.shutdown, f.shutDown = shutdown, sync.OnceFunc(func() { close(shutdown) })
	f.events, err = New(Deps{Store: st, Clock: f.clock, Scrubber: f.scrubber, Parents: storeParents{},
		Access: f.access, Memberships: f.memberships(), Shutdown: shutdown})
	if err != nil {
		f.t.Fatalf("bind the component: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.events.Run(ctx)
	}()
	f.stop = sync.OnceFunc(func() {
		cancel()
		<-done
	})
	// Cleanups run last first, so Run has ended before the store closes.
	f.t.Cleanup(f.stop)
}

// reopen closes the store and opens it again on the same file, with a new
// component: a restart of the hub.
func (f *fixture) reopen() {
	f.t.Helper()
	f.stop()
	if err := f.store.Close(); err != nil {
		f.t.Fatalf("close store: %v", err)
	}
	f.open()
}

// errMembershipRead is the failure of the memberships while
// failMemberships is set.
var errMembershipRead = errors.New("disk I/O error reading the memberships")

// memberships is the one membership source the access rule and the
// component ask, as in the hub: the active orgs of a user in the store.
func (f *fixture) memberships() access.Memberships {
	return access.MembershipsFunc(func(ctx context.Context, userID string) ([]string, error) {
		if f.failMemberships.Load() {
			return nil, errMembershipRead
		}
		return identity.ActiveOrgIDs(ctx, f.store.Main(), userID)
	})
}

// storeParents is the fork-parent lookup the hub installs: the parent slug
// of the workspace record when it is a fork.
type storeParents struct{}

func (storeParents) ForkParent(ctx context.Context, q Querier, slug string) (string, error) {
	rec, found, err := access.RecordBySlug(ctx, q, slug)
	if err != nil || !found || rec.Kind != access.KindFork {
		return "", err
	}
	return rec.ParentSlug, nil
}

// emit emits in in a committed transaction of its own and fails the test
// when the emit or the commit fails.
func (f *fixture) emit(in Input) Record {
	f.t.Helper()
	var rec Record
	err := f.store.Tx(context.Background(), func(tx *sql.Tx) error {
		var err error
		rec, err = f.events.Emit(context.Background(), tx, in)
		return err
	})
	if err != nil {
		f.t.Fatalf("emit %s: %v", in.Type, err)
	}
	return rec
}

// readAll returns every event of the log after position after.
func (f *fixture) readAll(after int64) []Record {
	f.t.Helper()
	recs, _, err := f.events.ReadAfter(context.Background(), after, 0)
	if err != nil {
		f.t.Fatalf("ReadAfter(%d): %v", after, err)
	}
	return recs
}

// bounds are the log's bounds.
func (f *fixture) bounds() Bounds {
	f.t.Helper()
	b, err := f.events.Bounds(context.Background())
	if err != nil {
		f.t.Fatalf("Bounds: %v", err)
	}
	return b
}

// lastSeq is last_seq of the log's bounds.
func (f *fixture) lastSeq() int64 {
	f.t.Helper()
	return f.bounds().LastSeq
}

// seqs are the seqs of recs, in their order.
func seqs(recs []Record) []int64 {
	var out []int64
	for _, rec := range recs {
		out = append(out, rec.Seq)
	}
	return out
}

// insertRow writes the test row name in tx.
func insertRow(ctx context.Context, tx *sql.Tx, name string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO test_rows (name) VALUES (?)`, name)
	return err
}

// hasRow reports whether the test row name was committed.
func (f *fixture) hasRow(name string) bool {
	f.t.Helper()
	var n int
	if err := f.store.Main().QueryRow(`SELECT count(*) FROM test_rows WHERE name = ?`, name).Scan(&n); err != nil {
		f.t.Fatalf("read test row: %v", err)
	}
	return n == 1
}

// ptr returns a pointer to v, for the optional fields of an Input.
func ptr[T any](v T) *T { return &v }

// valid is a valid input of type typ: a PAT acting on the workspace api.
func valid(typ string) Input {
	return Input{
		Type:         typ,
		Workspace:    ptr("api"),
		ActorType:    "pat",
		ActorID:      ptr("u_1"),
		ResourceType: "change",
		ResourceID:   "42",
	}
}

// world is a fixture with an admin token, users, an org and workspaces made
// through the identity and access components, and the router of
// internal/server with the real check order and the identity routes:
//
//   - repository api of dev in the org acme, and its task fork api-f1 of mem;
//   - repository web of dev in dev's personal org;
//   - dev, mem and mem2 are members of acme; out has no relation.
type world struct {
	*fixture
	srv   *server.Server
	admin string
	users map[string]string
	keys  map[string]string
	acme  string
}

// newWorld builds a world on a new fixture.
func newWorld(t testing.TB) *world {
	t.Helper()
	ctx := context.Background()
	w := &world{fixture: newFixture(t), users: map[string]string{}, keys: map[string]string{}}
	if err := identity.IssueAdminToken(ctx, w.store, "", func(tok string) error { w.admin = tok; return nil }); err != nil {
		t.Fatalf("issue the admin token: %v", err)
	}
	ids := &identity.Service{}
	ids.Bind(identity.Deps{Store: w.store, Now: w.clock.Now})
	w.srv = server.New(server.Options{Logger: slog.New(slog.DiscardHandler), RateLimitDisabled: true, Now: w.clock.Now})
	w.srv.SetAuthenticator(ids.Authenticate)
	w.srv.SetScopeCheck(identity.ScopeCheck)
	w.srv.AddAccessCheck(ids.AdminCheck)
	w.srv.AddAccessCheck(w.access.Check)
	for _, r := range append(ids.Routes(), w.events.Routes()...) {
		w.srv.Register(r)
	}
	personal := map[string]string{}
	for _, name := range []string{"dev", "mem", "mem2", "out"} {
		err := w.store.Tx(ctx, func(tx *sql.Tx) error {
			u, org, err := identity.CreateUserAndPersonalOrg(ctx, tx, identity.NewUser{Username: name,
				Email: name + "@example.com", Provider: "github", ProviderID: name, CreatedAt: clockStart}, identity.RoleUser)
			w.users[name], personal[name] = u.ID, org.ID
			return err
		})
		if err != nil {
			t.Fatalf("create user %s: %v", name, err)
		}
	}
	w.acme = w.createOrg("acme")
	for _, name := range []string{"dev", "mem", "mem2"} {
		w.addMember(w.acme, name)
	}
	w.addWorkspace(access.Record{Slug: "api", OwnerID: w.users["dev"], OrgID: w.acme, Kind: access.KindRepository})
	w.addWorkspace(access.Record{Slug: "api-f1", OwnerID: w.users["mem"], OrgID: w.acme, Kind: access.KindFork, ParentSlug: "api"})
	w.addWorkspace(access.Record{Slug: "web", OwnerID: w.users["dev"], OrgID: personal["dev"], Kind: access.KindRepository})
	return w
}

// user is the caller the user name is to the visibility decision.
func (w *world) user(name string) Caller {
	return Caller{UserID: w.users[name], CredentialID: "key-" + name}
}

// key is an API key of the user name, issued on first use, as a login
// issues one.
func (w *world) key(name string) string {
	w.t.Helper()
	if k, ok := w.keys[name]; ok {
		return k
	}
	ctx := context.Background()
	err := w.store.Tx(ctx, func(tx *sql.Tx) error {
		k, _, err := identity.IssueAPIKey(ctx, tx, w.users[name], 0, w.clock.Now())
		w.keys[name] = k
		return err
	})
	if err != nil {
		w.t.Fatalf("issue an API key of %s: %v", name, err)
	}
	return w.keys[name]
}

// call sends method path to the router in-process with credential as the
// bearer credential and body as JSON (nil for none), and returns the status
// and the body of the answer.
func (w *world) call(method, path, credential string, body any) (int, []byte) {
	w.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			w.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, "/api/v1"+path, rd)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	rec := httptest.NewRecorder()
	w.srv.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// adminCall sends method path as the admin token and fails the test unless the
// router answers want.
func (w *world) adminCall(method, path string, body any, want int) []byte {
	w.t.Helper()
	status, out := w.call(method, path, w.admin, body)
	if status != want {
		w.t.Fatalf("%s %s = %d %s, want %d", method, path, status, out, want)
	}
	return out
}

// createOrg creates the active org slug through the admin route and returns
// its id.
func (w *world) createOrg(slug string) string {
	w.t.Helper()
	var org struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.adminCall(http.MethodPost, "/orgs", map[string]string{"name": slug, "slug": slug}, http.StatusCreated), &org); err != nil || org.ID == "" {
		w.t.Fatalf("create org %s: no id (%v)", slug, err)
	}
	return org.ID
}

// addMember makes the user name a member of the org orgID through the admin
// route.
func (w *world) addMember(orgID, name string) {
	w.t.Helper()
	w.adminCall(http.MethodPut, "/orgs/"+orgID+"/members/"+w.users[name], nil, http.StatusNoContent)
}

// removeMember ends the user name's membership of the org orgID through the
// admin route.
func (w *world) removeMember(orgID, name string) {
	w.t.Helper()
	w.adminCall(http.MethodDelete, "/orgs/"+orgID+"/members/"+w.users[name], nil, http.StatusNoContent)
}

// blockOrg blocks the org orgID through the admin route.
func (w *world) blockOrg(orgID string) {
	w.t.Helper()
	w.adminCall(http.MethodPost, "/orgs/"+orgID+"/block", nil, http.StatusOK)
}

// addWorkspace stores the ready workspace record rec through the access
// component, as registering or forking a workspace does.
func (w *world) addWorkspace(rec access.Record) {
	w.t.Helper()
	rec.Status = access.StatusReady
	ctx := context.Background()
	if err := w.store.Tx(ctx, func(tx *sql.Tx) error { return access.InsertRecord(ctx, tx, rec) }); err != nil {
		w.t.Fatalf("store the workspace record %s: %v", rec.Slug, err)
	}
}

// deleteWorkspace deletes the record of the workspace slug, as deleting the
// workspace does.
func (w *world) deleteWorkspace(slug string) {
	w.t.Helper()
	if _, err := w.store.Main().Exec(`DELETE FROM workspaces WHERE slug = ?`, slug); err != nil {
		w.t.Fatalf("delete the workspace record %s: %v", slug, err)
	}
}
