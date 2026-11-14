package access_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/store"
)

// openStore opens the transactional store in dir, on disk, with the
// workspace record steps, and closes it when the test ends. Opening the same
// dir again reopens the same store.
func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{
		DatabasePath:  filepath.Join(dir, "parley.db"),
		WorkspaceRoot: filepath.Join(dir, "workspaces"),
	}, []store.Step{access.SchemaStep, access.CreatingTokenStep})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// repo is a repository record.
func repo(slug, owner, org, status string) access.Record {
	return access.Record{Slug: slug, OwnerID: owner, OrgID: org, Kind: access.KindRepository, Status: status}
}

// fork is a ready fork record of parent.
func fork(slug, owner, org, parent string) access.Record {
	return access.Record{Slug: slug, OwnerID: owner, OrgID: org, Kind: access.KindFork, ParentSlug: parent, Status: access.StatusReady}
}

// createdBy is rec with the creating token tokenID, as fork creation
// records it.
func createdBy(rec access.Record, tokenID string) access.Record {
	rec.CreatingTokenID = tokenID
	return rec
}

// boundPAT is the caller identity's authenticator reports for userID's PAT
// tokenID bound to the workspace slug.
func boundPAT(userID, tokenID, slug string) server.Caller {
	return server.Caller{CredentialID: tokenID, UserID: userID, Detail: &identity.Principal{
		Type: identity.TypePAT, CredentialID: tokenID, User: &identity.User{ID: userID, Role: identity.RoleUser},
		Permissions: []string{"git:read"}, Workspace: &slug}}
}

// insert inserts rec in a transaction of its own.
func insert(st *store.Store, rec access.Record) error {
	return st.Tx(context.Background(), func(tx *sql.Tx) error {
		return access.InsertRecord(context.Background(), tx, rec)
	})
}

// seed inserts recs in order and fails the test at the first refusal.
func seed(t *testing.T, st *store.Store, recs ...access.Record) {
	t.Helper()
	for _, rec := range recs {
		if err := insert(st, rec); err != nil {
			t.Fatalf("insert %+v: %v", rec, err)
		}
	}
}

// memberships is a membership source stub: the orgs each user is a member
// of, of which it answers only those in active, as the real source answers
// only active orgs.
type memberships struct {
	of     map[string][]string
	active []string
}

func (m memberships) ActiveOrgIDs(_ context.Context, userID string) ([]string, error) {
	var ids []string
	for _, id := range m.of[userID] {
		if slices.Contains(m.active, id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// errRead is the failure of the failing sources. It names a file of the
// hub, which no answer may show.
var errRead = errors.New("disk I/O error at /data/parley.db")

// failingRecords reads through RecordReader, except the record of a slug in
// fail, and the list of owned-or-in records or of forks when failOwned or
// failForks is set, whose reads fail with errRead.
type failingRecords struct {
	access.RecordReader
	fail                 []string
	failOwned, failForks bool
}

func (f failingRecords) Record(ctx context.Context, slug string) (access.Record, bool, error) {
	if slices.Contains(f.fail, slug) {
		return access.Record{}, false, errRead
	}
	return f.RecordReader.Record(ctx, slug)
}

func (f failingRecords) OwnedOrIn(ctx context.Context, ownerID string, orgIDs []string) ([]access.Record, error) {
	if f.failOwned {
		return nil, errRead
	}
	return f.RecordReader.OwnedOrIn(ctx, ownerID, orgIDs)
}

func (f failingRecords) ForksOf(ctx context.Context, parents []string) ([]access.Record, error) {
	if f.failForks {
		return nil, errRead
	}
	return f.RecordReader.ForksOf(ctx, parents)
}

// failingMemberships is a membership source whose every read fails with
// errRead.
var failingMemberships = access.MembershipsFunc(func(context.Context, string) ([]string, error) {
	return nil, errRead
})

// newService is a service bound to records and m.
func newService(records access.RecordReader, m access.Memberships) *access.Service {
	svc := &access.Service{}
	svc.Bind(access.Deps{Records: records, Memberships: m})
	return svc
}

// probe is a server with svc's access check in the access position and the
// probe routes GET /workspaces/:slug/r, /w and /o, which need read, write
// and owner. A probe route answers 200 with the decision it reads, and
// counts its calls. The authenticator takes the caller from the header
// X-Caller: "admin" is the admin token, another value a user id, and no
// header a caller with neither.
type probe struct {
	srv   *server.Server
	calls atomic.Int64
}

func newProbe(svc *access.Service) *probe {
	p := &probe{srv: server.New(server.Options{Logger: slog.New(slog.DiscardHandler), RateLimitDisabled: true})}
	p.srv.SetAuthenticator(func(r *http.Request) (server.Caller, error) {
		switch id := r.Header.Get("X-Caller"); id {
		case "":
			return server.Caller{CredentialID: "anonymous"}, nil
		case "admin":
			return server.Caller{CredentialID: id, Admin: true}, nil
		default:
			return server.Caller{CredentialID: id, UserID: id}, nil
		}
	})
	p.srv.AddAccessCheck(svc.Check)
	p.route("/workspaces/:slug/r", "read")
	p.route("/workspaces/:slug/w", "write")
	p.route("/workspaces/:slug/o", "owner")
	return p
}

// route registers a probe route GET pattern that declares need.
func (p *probe) route(pattern, need string) {
	p.routeFor(http.MethodGet, pattern, need)
}

// routeFor registers a probe route method pattern that declares need.
func (p *probe) routeFor(method, pattern, need string) {
	p.srv.Register(server.Route{
		Method: method, Pattern: pattern, Marks: map[string]string{access.MarkNeed: need},
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			p.calls.Add(1)
			d, ok := access.DecisionOf(r)
			if !ok {
				return errors.New("the handler found no decision")
			}
			server.WriteJSON(w, http.StatusOK, map[string]any{"level": d.Level, "record": d.Record})
			return nil
		},
	})
}

// answer is what the probe answered.
type answer struct {
	status      int
	contentType string
	body        []byte
	allow       string
}

// get calls GET path, under /api/v1, as caller ("" for none).
func (p *probe) get(caller, path string) answer {
	return p.call(http.MethodGet, caller, path)
}

// call calls method path, under /api/v1, as caller ("" for none), without
// a body.
func (p *probe) call(method, caller, path string) answer {
	r := httptest.NewRequest(method, "/api/v1"+path, nil)
	if caller != "" {
		r.Header.Set("X-Caller", caller)
	}
	rec := httptest.NewRecorder()
	p.srv.Handler().ServeHTTP(rec, r)
	return answer{rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes(), ""}
}

// envelope is the error envelope's error object, or the probe's answer.
type envelope struct {
	Error struct {
		Code      int    `json:"code"`
		Message   string `json:"message"`
		ErrorType string `json:"error_type"`
	} `json:"error"`
	Level   access.Level  `json:"level"`
	Record  access.Record `json:"record"`
	Binding *binding      `json:"binding"`
	Covered []string      `json:"covered"`
}

// binding is the binding a guard probe found, as it answers it.
type binding struct {
	BoundWorkspace string          `json:"bound_workspace"`
	Relation       access.Relation `json:"relation"`
}

func decode(t *testing.T, a answer) envelope {
	t.Helper()
	var e envelope
	if err := json.Unmarshal(a.body, &e); err != nil {
		t.Fatalf("body %s is not JSON: %v", a.body, err)
	}
	return e
}

// readLog records, in order, the reads of the sources it wraps: "record"
// for the record of one slug, "orgs" for a user's active orgs.
type readLog struct {
	reads []string
}

// records wraps r so that its record reads are logged.
func (l *readLog) records(r access.RecordReader) access.RecordReader {
	return loggedRecords{RecordReader: r, log: l}
}

// memberships wraps m so that its reads are logged.
func (l *readLog) memberships(m access.Memberships) access.Memberships {
	return access.MembershipsFunc(func(ctx context.Context, userID string) ([]string, error) {
		l.reads = append(l.reads, "orgs")
		return m.ActiveOrgIDs(ctx, userID)
	})
}

type loggedRecords struct {
	access.RecordReader
	log *readLog
}

func (r loggedRecords) Record(ctx context.Context, slug string) (access.Record, bool, error) {
	r.log.reads = append(r.log.reads, "record")
	return r.RecordReader.Record(ctx, slug)
}

// recordingEmitter is an event emitter that remembers, in order, each event
// and the transaction it was emitted in, nil for one emitted now. When seen
// is set it is called with the transaction and the event first, so a test
// can look at what the transaction holds at that moment. When err is set it
// returns err after remembering the event, and when panics is set it panics
// instead, as a broken event log would.
type recordingEmitter struct {
	mu      sync.Mutex
	emitted []emitted
	seen    func(tx *sql.Tx, ev access.Event)
	err     error
	panics  bool
}

type emitted struct {
	event access.Event
	tx    *sql.Tx
}

var _ access.EventEmitter = (*recordingEmitter)(nil)

func (e *recordingEmitter) Emit(_ context.Context, tx *sql.Tx, ev access.Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.emitted = append(e.emitted, emitted{ev, tx})
	if e.seen != nil {
		e.seen(tx, ev)
	}
	if e.panics {
		panic("the event log is broken")
	}
	return e.err
}

func (e *recordingEmitter) EmitNow(_ context.Context, ev access.Event) error {
	return e.Emit(context.Background(), nil, ev)
}

// all returns what was emitted so far.
func (e *recordingEmitter) all() []emitted {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.emitted)
}

// guardHub is the hub's REST pipeline built from internal/server: identity's
// authenticator, the service's binding guard, identity's scope check, then
// identity's admin check, the service's subset rule and the workspace access
// check, over a store in a temporary directory with the identity and
// workspace record steps. Users and credentials are stored with identity's
// own functions; the active orgs of a user come from orgs. The guard's
// refusals go to refusals and the service's events to events. Probe routes
// answer 200 with the binding they find, with its covered set, and count
// their calls.
type guardHub struct {
	t        *testing.T
	st       *store.Store
	ids      *identity.Service
	svc      *access.Service
	srv      *server.Server
	orgs     map[string][]string
	refusals *refusalLog
	events   *recordingEmitter
	calls    *atomic.Int64
}

func newGuardHub(t *testing.T) *guardHub {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{
		DatabasePath:  filepath.Join(dir, "parley.db"),
		WorkspaceRoot: filepath.Join(dir, "workspaces"),
	}, []store.Step{identity.SchemaStep, identity.AdminSettingsStep, access.SchemaStep, access.CreatingTokenStep})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	h := &guardHub{t: t, st: st, ids: &identity.Service{}, svc: &access.Service{},
		orgs: map[string][]string{}, refusals: &refusalLog{}, events: &recordingEmitter{}, calls: &atomic.Int64{}}
	h.ids.Bind(identity.Deps{Store: st})
	h.bind(h.refusals)
	h.srv = h.pipeline(true)
	return h
}

// bind binds the hub's service to its store and orgs, with the guard's
// refusals going to refusals and its events to the hub's events.
func (h *guardHub) bind(refusals access.RefusalReporter) {
	h.svc.Bind(access.Deps{
		Store: h.st, Events: h.events,
		Records: access.StoreRecords(h.st.Main()),
		Memberships: access.MembershipsFunc(func(_ context.Context, userID string) ([]string, error) {
			return h.orgs[userID], nil
		}),
		Refusals: refusals,
	})
}

// limit puts a refusal limiter built anew, as at the hub's start, between
// the guard and events, with clock as its clock and logger as its log.
func (h *guardHub) limit(events access.EventEmitter, clock *testClock, logger *slog.Logger) {
	h.bind(access.NewRefusalLimiter(events, clock.now, logger))
}

// testClock is a clock that stands still until the test advances it.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// advance moves the clock on by d.
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// pipeline is a server with the hub's checks installed, the binding guard
// only when guarded, and no route.
func (h *guardHub) pipeline(guarded bool) *server.Server {
	srv := server.New(server.Options{Logger: slog.New(slog.DiscardHandler), RateLimitDisabled: true})
	srv.SetAuthenticator(h.ids.Authenticate)
	if guarded {
		srv.SetBindingGuard(h.svc.Guard)
	}
	srv.SetScopeCheck(identity.ScopeCheck)
	srv.AddAccessCheck(h.ids.AdminCheck)
	srv.AddAccessCheck(h.svc.SubsetCheck)
	srv.AddAccessCheck(h.svc.Check)
	return srv
}

// unguarded is the same hub served by a server of its own without the
// binding guard.
func (h *guardHub) unguarded() *guardHub {
	c := *h
	c.srv = h.pipeline(false)
	return &c
}

// identityRoutes registers identity's routes: the identity, credential and
// admin routes.
func (h *guardHub) identityRoutes() {
	for _, r := range h.ids.Routes() {
		h.srv.Register(r)
	}
}

// tokenRoutes registers the service's bound-token routes.
func (h *guardHub) tokenRoutes() {
	for _, r := range h.svc.TokenRoutes() {
		h.srv.Register(r)
	}
}

// pats counts the PATs in the store that match the SQL condition where
// ("1" for all), with args.
func (h *guardHub) pats(where string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.st.Main().QueryRow("SELECT count(*) FROM pats WHERE "+where, args...).Scan(&n); err != nil {
		h.t.Fatalf("count PATs: %v", err)
	}
	return n
}

// revokedAt returns the revoked_at of every PAT in the store, by token_id,
// as stored.
func (h *guardHub) revokedAt() map[string]sql.NullInt64 {
	h.t.Helper()
	rows, err := h.st.Main().Query("SELECT token_id, revoked_at FROM pats")
	if err != nil {
		h.t.Fatalf("read the PATs: %v", err)
	}
	defer rows.Close()
	got := map[string]sql.NullInt64{}
	for rows.Next() {
		var id string
		var at sql.NullInt64
		if err := rows.Scan(&id, &at); err != nil {
			h.t.Fatalf("read a PAT: %v", err)
		}
		got[id] = at
	}
	if err := rows.Err(); err != nil {
		h.t.Fatalf("read the PATs: %v", err)
	}
	return got
}

// admin issues the hub's admin token and returns it.
func (h *guardHub) admin() string {
	h.t.Helper()
	var token string
	err := identity.IssueAdminToken(context.Background(), h.st, "boss@example.com", func(v string) error {
		token = v
		return nil
	})
	if err != nil {
		h.t.Fatalf("issue the admin token: %v", err)
	}
	return token
}

// key issues an API key of userID and returns it.
func (h *guardHub) key(userID string) string {
	h.t.Helper()
	var key string
	err := h.st.Tx(context.Background(), func(tx *sql.Tx) (err error) {
		key, _, err = identity.IssueAPIKey(context.Background(), tx, userID, 0, time.Now())
		return err
	})
	if err != nil {
		h.t.Fatalf("issue an API key of %s: %v", userID, err)
	}
	return key
}

// user creates the user name with the role user and returns its id and
// the id of its personal org.
func (h *guardHub) user(name string) (id, org string) {
	h.t.Helper()
	err := h.st.Tx(context.Background(), func(tx *sql.Tx) error {
		u, o, err := identity.CreateUserAndPersonalOrg(context.Background(), tx, identity.NewUser{
			Username: name, Email: name + "@example.com", Provider: "github", ProviderID: name, CreatedAt: time.Now(),
		}, identity.RoleUser)
		id, org = u.ID, o.ID
		return err
	})
	if err != nil {
		h.t.Fatalf("create user %s: %v", name, err)
	}
	return id, org
}

// pat stores a PAT of userID with perms, bound to workspace ("" for none),
// and returns its value and its token_id.
func (h *guardHub) pat(userID, workspace string, perms ...string) (value, tokenID string) {
	h.t.Helper()
	return h.storePAT(identity.PATSpec{UserID: userID, Permissions: perms, Workspace: workspace})
}

// storePAT stores the PAT spec describes, named agent unless it names one
// and created now unless it states when, and returns its value and its
// token_id.
func (h *guardHub) storePAT(spec identity.PATSpec) (value, tokenID string) {
	h.t.Helper()
	if spec.Name == "" {
		spec.Name = "agent"
	}
	if spec.CreatedAt.IsZero() {
		spec.CreatedAt = time.Now()
	}
	err := h.st.Tx(context.Background(), func(tx *sql.Tx) error {
		v, rec, err := identity.StorePAT(context.Background(), tx, spec)
		value, tokenID = v, rec.TokenID
		return err
	})
	if err != nil {
		h.t.Fatalf("store a PAT of %s: %v", spec.UserID, err)
	}
	return value, tokenID
}

// probe registers a probe route method pattern that declares need ("" for
// none, a route that names no workspace) and scope. It answers the binding
// it finds and the covered set of that binding, as a route that confines
// its result reads it.
func (h *guardHub) probe(method, pattern string, need access.Need, scope string) {
	var marks map[string]string
	if need != "" {
		marks = map[string]string{access.MarkNeed: string(need)}
	}
	h.srv.Register(server.Route{
		Method: method, Pattern: pattern, Scope: scope, Marks: marks,
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			h.calls.Add(1)
			var found *binding
			var covered []string
			if b, ok := access.BindingOf(r); ok {
				found = &binding{BoundWorkspace: b.Workspace, Relation: b.Relation}
				var err error
				if covered, err = h.svc.CoveredSet(r.Context(), access.Caller{TokenID: b.TokenID, Workspace: b.Workspace}); err != nil {
					return err
				}
			}
			server.WriteJSON(w, http.StatusOK, map[string]any{"binding": found, "covered": covered})
			return nil
		},
	})
}

// do sends method path, under /api/v1, with credential as the bearer
// credential and no body.
func (h *guardHub) do(method, path, credential string) answer {
	return h.send(method, path, credential, "", nil)
}

// post sends POST path, under /api/v1, with credential as the bearer
// credential and body as its JSON body.
func (h *guardHub) post(path, credential, body string) answer {
	return h.send(http.MethodPost, path, credential, body, nil)
}

// send sends method path, under /api/v1, with credential as the bearer
// credential, body as its JSON body ("" for none) and the headers header.
func (h *guardHub) send(method, path, credential, body string, header http.Header) answer {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/api/v1"+path, nil)
	} else {
		r = httptest.NewRequest(method, "/api/v1"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	for name, values := range header {
		r.Header[name] = values
	}
	r.Header.Set("Authorization", "Bearer "+credential)
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, r)
	return answer{rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes(), rec.Header().Get("Allow")}
}

// refusalLog is a refusal reporter that remembers, in order, each refusal.
type refusalLog struct {
	mu  sync.Mutex
	got []access.Refusal
}

func (l *refusalLog) Refused(_ context.Context, f access.Refusal) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.got = append(l.got, f)
}

// all returns the refusals so far.
func (l *refusalLog) all() []access.Refusal {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.got)
}

// bindEvent is the parley.token.bind event of the token tok, minted by
// userID with a credential of actorType.
func bindEvent(tok identity.PAT, userID, actorType string) access.Event {
	var expires any
	if tok.ExpiresAt != nil {
		expires = timefmt.Format(*tok.ExpiresAt)
	}
	return access.Event{Type: "parley.token.bind", Workspace: tok.Workspace, ActorType: actorType, ActorID: userID,
		ResourceType: "token", ResourceID: tok.TokenID, Metadata: map[string]any{
			"token_id": tok.TokenID, "workspace": tok.Workspace, "permissions": tok.Permissions,
			"paths": tok.Paths, "expires_at": expires}}
}
