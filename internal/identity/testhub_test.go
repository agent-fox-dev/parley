package identity_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/store"
)

// testHub is the request pipeline of internal/server with the identity
// service, the scope check, the admin check and the service's routes
// installed, as the hub composes them, over a store in t.TempDir(), served
// in-process. It also registers two non-public test routes: GET
// /api/v1/test/whoami answers the caller's Principal, and GET
// /api/v1/test/me, declared as needing a user, answers 200. Both count
// their calls.
type testHub struct {
	t          *testing.T
	st         *store.Store
	dataDir    string
	srv        *server.Server
	svc        *identity.Service
	adminToken string // "" when the hub is not bootstrapped
	provider   *stubProvider

	mu    sync.Mutex
	clock time.Time

	whoamiCalls, meCalls atomic.Int64
}

// hubConfig is what the options of newTestHub set.
type hubConfig struct {
	bootstrap   bool
	adminEmail  string
	providers   []config.Provider
	externalURL string
	client      identity.ProviderClient // nil: the stub provider
}

type hubOption func(*hubConfig)

// notBootstrapped leaves the hub without an admin token.
func notBootstrapped() hubOption { return func(c *hubConfig) { c.bootstrap = false } }

// withAdminEmail bootstraps the hub with email as its admin_email.
func withAdminEmail(email string) hubOption { return func(c *hubConfig) { c.adminEmail = email } }

// withProviders configures the identity providers. An endpoint a provider
// leaves empty is its public one, as when the configuration file does not
// give it.
func withProviders(ps ...config.Provider) hubOption {
	return func(c *hubConfig) {
		for _, p := range ps {
			c.providers = append(c.providers, p.WithPublicEndpoints())
		}
	}
}

// withOAuth configures the providers names with their endpoints on the fake
// provider f and reads identities with the hub's own provider client, which
// abandons a call to f after timeout (0 is the client's default).
func withOAuth(f *fakeProvider, timeout time.Duration, names ...string) hubOption {
	return func(c *hubConfig) {
		for _, name := range names {
			c.providers = append(c.providers, f.provider(name))
		}
		c.client = identity.NewOAuthClient(c.providers, nil, timeout)
	}
}

// withExternalURL sets [server].external_url.
func withExternalURL(u string) hubOption { return func(c *hubConfig) { c.externalURL = u } }

// newTestHub builds a test hub, bootstrapped with an admin token unless an
// option says otherwise. Its clock stands still until advance moves it.
func newTestHub(t *testing.T, opts ...hubOption) *testHub {
	t.Helper()
	cfg := hubConfig{bootstrap: true}
	for _, o := range opts {
		o(&cfg)
	}
	ctx := context.Background()
	dir := t.TempDir()
	h := &testHub{t: t, dataDir: filepath.Join(dir, "data"), clock: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		provider: &stubProvider{}}
	st, err := store.Open(ctx, store.Options{
		DatabasePath:  filepath.Join(h.dataDir, "parley.db"),
		WorkspaceRoot: filepath.Join(dir, "workspaces"),
	}, []store.Step{identity.AdminSettingsStep, identity.SchemaStep})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	h.st = st
	if cfg.bootstrap {
		err := identity.IssueAdminToken(ctx, st, cfg.adminEmail, func(tok string) error { h.adminToken = tok; return nil })
		if err != nil {
			t.Fatalf("bootstrap the admin token: %v", err)
		}
	}
	h.svc = &identity.Service{}
	client := cfg.client
	if client == nil {
		client = h.provider
	}
	h.svc.Bind(identity.Deps{Store: st, Now: h.now, Providers: cfg.providers, ExternalURL: cfg.externalURL, Client: client})
	h.srv = server.New(server.Options{Logger: slog.New(slog.DiscardHandler), RateLimitDisabled: true, Now: h.now})
	h.srv.SetAuthenticator(h.svc.Authenticate)
	h.srv.SetScopeCheck(identity.ScopeCheck)
	h.srv.AddAccessCheck(h.svc.AdminCheck)
	for _, r := range h.svc.Routes() {
		h.srv.Register(r)
	}
	h.srv.Register(server.Route{Method: http.MethodGet, Pattern: "/test/whoami", Handler: func(w http.ResponseWriter, r *http.Request) error {
		h.whoamiCalls.Add(1)
		p, ok := identity.PrincipalOf(r.Context())
		if !ok {
			return server.Internal("no principal")
		}
		server.WriteJSON(w, http.StatusOK, p)
		return nil
	}})
	h.srv.Register(server.Route{Method: http.MethodGet, Pattern: "/test/me", Marks: map[string]string{identity.MarkNeedsUser: "true"},
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			h.meCalls.Add(1)
			server.WriteJSON(w, http.StatusOK, map[string]any{})
			return nil
		}})
	return h
}

// now is the hub's clock.
func (h *testHub) now() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clock
}

// advance moves the hub's clock by d.
func (h *testHub) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clock = h.clock.Add(d)
}

// response is what the hub answered.
type response struct {
	status int
	header http.Header
	body   []byte
	json   map[string]any // the body as a JSON object, nil when it is not one
}

// errorField returns the member name of the error envelope's error object.
func (r response) errorField(name string) any {
	e, _ := r.json["error"].(map[string]any)
	return e[name]
}

// do sends a request to the hub in-process. body, when not nil, is sent as
// JSON (a string or []byte as it is).
func (h *testHub) do(method, path string, body any, header http.Header) response {
	h.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = bytes.NewReader([]byte(b))
	case []byte:
		rd = bytes.NewReader(b)
	default:
		data, err := json.Marshal(b)
		if err != nil {
			h.t.Fatalf("encode body: %v", err)
		}
		rd = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, rd)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	r := response{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
	_ = json.Unmarshal(r.body, &r.json)
	return r
}

// get sends GET path with authorization as the Authorization header; ""
// sends none.
func (h *testHub) get(path, authorization string) response {
	h.t.Helper()
	header := http.Header{}
	if authorization != "" {
		header.Set("Authorization", authorization)
	}
	return h.do(http.MethodGet, path, nil, header)
}

// tx runs fn in a transaction of the hub's store.
func (h *testHub) tx(fn func(*sql.Tx) error) {
	h.t.Helper()
	if err := h.st.Tx(context.Background(), fn); err != nil {
		h.t.Fatalf("store transaction: %v", err)
	}
}

// exec runs a statement against the hub's store, for the states the tests
// set directly.
func (h *testHub) exec(query string, args ...any) {
	h.t.Helper()
	h.tx(func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), query, args...)
		return err
	})
}

// seedUser stores an active user with role user named username, with the
// e-mail <username>@x and a github identity.
func (h *testHub) seedUser(username string) identity.User {
	h.t.Helper()
	now := h.now()
	u := identity.User{
		ID: rand.Text(), Username: username, Email: username + "@x", Role: identity.RoleUser,
		Status: identity.StatusActive, Provider: "github", ProviderID: rand.Text(), CreatedAt: now, UpdatedAt: now,
	}
	h.exec(`INSERT INTO users (id, username, email, full_name, role, status, provider, provider_id, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, u.ID, u.Username, u.Email, u.FullName, u.Role, u.Status, u.Provider, u.ProviderID,
		now.UnixNano(), now.UnixNano())
	return u
}

// seedKey issues an API key for userID with a lifetime of days and returns
// it with its record.
func (h *testHub) seedKey(userID string, days int) (string, identity.APIKey) {
	h.t.Helper()
	var key string
	var rec identity.APIKey
	h.tx(func(tx *sql.Tx) (err error) {
		key, rec, err = identity.IssueAPIKey(context.Background(), tx, userID, days, h.now())
		return err
	})
	return key, rec
}

// seedPAT stores a PAT and returns it with its record; an empty name or
// creation time is filled in.
func (h *testHub) seedPAT(spec identity.PATSpec) (string, identity.PAT) {
	h.t.Helper()
	if spec.Name == "" {
		spec.Name = "ci"
	}
	if spec.CreatedAt.IsZero() {
		spec.CreatedAt = h.now()
	}
	var tok string
	var rec identity.PAT
	h.tx(func(tx *sql.Tx) (err error) {
		tok, rec, err = identity.StorePAT(context.Background(), tx, spec)
		return err
	})
	return tok, rec
}

// seedCredential gives userID a live credential of kind, "API key" or
// "PAT", that never expires, and returns it with its id.
func (h *testHub) seedCredential(kind, userID string) (credential, id string) {
	h.t.Helper()
	if kind == "PAT" {
		tok, rec := h.seedPAT(identity.PATSpec{UserID: userID, Permissions: []string{"git:read"}})
		return tok, rec.TokenID
	}
	key, rec := h.seedKey(userID, 0)
	return key, rec.KeyID
}

// storedRevokedAt reads the revoked_at of the API key or PAT with the id
// credentialID; nil when it is not revoked.
func (h *testHub) storedRevokedAt(kind, credentialID string) *time.Time {
	h.t.Helper()
	ctx, db := context.Background(), h.st.Main()
	if kind == "PAT" {
		rec, err := identity.PATByID(ctx, db, credentialID)
		if err != nil {
			h.t.Fatalf("read PAT %s: %v", credentialID, err)
		}
		return rec.RevokedAt
	}
	rec, err := identity.APIKeyByID(ctx, db, credentialID)
	if err != nil {
		h.t.Fatalf("read API key %s: %v", credentialID, err)
	}
	return rec.RevokedAt
}

// revoke marks the API key or PAT with the id credentialID revoked now.
func (h *testHub) revoke(credentialID string) {
	h.exec(`UPDATE api_keys SET revoked_at = ? WHERE key_id = ?`, h.now().UnixNano(), credentialID)
	h.exec(`UPDATE pats SET revoked_at = ? WHERE token_id = ?`, h.now().UnixNano(), credentialID)
}

// expireAt sets the expiry of the API key or PAT with the id credentialID.
func (h *testHub) expireAt(credentialID string, at time.Time) {
	h.exec(`UPDATE api_keys SET expires_at = ? WHERE key_id = ?`, at.UnixNano(), credentialID)
	h.exec(`UPDATE pats SET expires_at = ? WHERE token_id = ?`, at.UnixNano(), credentialID)
}

// setStatus sets the status of the user userID.
func (h *testHub) setStatus(userID, status string) {
	h.exec(`UPDATE users SET status = ? WHERE id = ?`, status, userID)
}

// stubProvider is the test hub's provider client: it answers every code
// with the identity, or the error, that set gave it, and counts its calls.
type stubProvider struct {
	mu    sync.Mutex
	id    identity.Identity
	err   error
	calls int
}

// set makes the provider answer id, or err when it is not nil.
func (p *stubProvider) set(id identity.Identity, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.id, p.err = id, err
}

// Identity implements identity.ProviderClient.
func (p *stubProvider) Identity(context.Context, string, string, string) (identity.Identity, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.id, p.err
}

// callback posts body to POST /api/v1/auth/callback.
func (h *testHub) callback(body any) response {
	h.t.Helper()
	return h.do(http.MethodPost, "/api/v1/auth/callback", body, nil)
}

// count runs a SELECT count(*) query against the hub's store.
func (h *testHub) count(query string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.st.Main().QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		h.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// seedOrg stores an active org without an owner and returns its id, a
// UUID like the ids the hub draws.
func (h *testHub) seedOrg(name, slug string) string {
	h.t.Helper()
	id, now := uuid.NewV4().String(), h.now().UnixNano()
	h.exec(`INSERT INTO orgs (id, name, slug, status, created_at, updated_at) VALUES (?, ?, ?, 'active', ?, ?)`,
		id, name, slug, now, now)
	return id
}

// loginBody is a callback body the provider stub accepts.
var loginBody = map[string]any{"provider": "github", "code": "c", "redirect_uri": "http://127.0.0.1:5000/callback"}

// newLoginHub is a test hub with github configured whose provider answers
// the identity id.
func newLoginHub(t *testing.T, id identity.Identity, opts ...hubOption) *testHub {
	t.Helper()
	opts = append(opts, withProviders(config.Provider{Name: "github", ClientID: "gh-id", ClientSecret: "gh-secret"}))
	h := newTestHub(t, opts...)
	h.provider.set(id, nil)
	return h
}

// records counts the users, orgs, memberships, API keys and revoked API
// keys in the hub's store, to show that a refused login changed nothing.
func (h *testHub) records() []int {
	h.t.Helper()
	var out []int
	for _, q := range []string{`users`, `orgs`, `memberships`, `api_keys`, `api_keys WHERE revoked_at IS NOT NULL`} {
		out = append(out, h.count(`SELECT count(*) FROM `+q))
	}
	return out
}

// newOAuthHub is a test hub whose real provider client reads identities from
// a fake provider configured as the providers names (github when none); its
// user URL answers alice, with the github id 123 and the e-mail a@x.
func newOAuthHub(t *testing.T, timeout time.Duration, names []string, opts ...hubOption) (*testHub, *fakeProvider) {
	t.Helper()
	f := newFakeProvider(t)
	f.answer(fakeUserPath, http.StatusOK, map[string]any{"id": 123, "login": "alice", "email": "a@x"})
	if len(names) == 0 {
		names = []string{"github"}
	}
	return newTestHub(t, append(opts, withOAuth(f, timeout, names...))...), f
}

// newIdentity is a github identity no user has.
func newIdentity(username, email string) identity.Identity {
	return identity.Identity{Provider: "github", ProviderID: "123", Username: username, FullName: "Alice A", Email: email}
}

// orgOf returns the name, slug, status and member ids of the org owned by
// userID.
func (h *testHub) orgOf(userID string) (name, slug, status string, members []string) {
	h.t.Helper()
	ctx, db := context.Background(), h.st.Main()
	var id string
	err := db.QueryRowContext(ctx, `SELECT id, name, slug, status FROM orgs WHERE owner_id = ?`, userID).Scan(&id, &name, &slug, &status)
	if err != nil {
		h.t.Fatalf("read the org of %s: %v", userID, err)
	}
	rows, err := db.QueryContext(ctx, `SELECT user_id FROM memberships WHERE org_id = ?`, id)
	if err != nil {
		h.t.Fatalf("read the members of %s: %v", id, err)
	}
	defer rows.Close()
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			h.t.Fatalf("read a member of %s: %v", id, err)
		}
		members = append(members, m)
	}
	return name, slug, status, members
}

// seedAlice stores alice as an earlier login left her: the github identity
// 123, the e-mail a@x, the role admin and the full name Custom, with her
// personal org and no key.
func (h *testHub) seedAlice() identity.User {
	h.t.Helper()
	var u identity.User
	h.tx(func(tx *sql.Tx) (err error) {
		u, _, err = identity.CreateUserAndPersonalOrg(context.Background(), tx, identity.NewUser{
			Username: "alice", Email: "a@x", FullName: "Custom", Provider: "github", ProviderID: "123", CreatedAt: h.now(),
		}, identity.RoleAdmin)
		return err
	})
	return u
}

// storedUser reads the user userID from the hub's store.
func (h *testHub) storedUser(userID string) identity.User {
	h.t.Helper()
	u, err := identity.UserByID(context.Background(), h.st.Main(), userID)
	if err != nil {
		h.t.Fatalf("read user %s: %v", userID, err)
	}
	return u
}

// aliceHub is a bootstrapped test hub where the user alice (role user,
// status active) holds a live API key and a live PAT with permissions
// [git:read], paths [docs/**] and no workspace.
type aliceHub struct {
	*testHub
	alice    identity.User
	key, pat string
	keyRec   identity.APIKey
	patRec   identity.PAT
}

func newAliceHub(t *testing.T) aliceHub {
	t.Helper()
	h := aliceHub{testHub: newTestHub(t)}
	h.alice = h.seedUser("alice")
	h.key, h.keyRec = h.seedKey(h.alice.ID, 90)
	h.pat, h.patRec = h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{"git:read"}, Paths: []string{"docs/**"}})
	return h
}

// credentialKinds are the credentials that have a state of their own.
var credentialKinds = []string{"API key", "PAT"}

// checkRefusal reports when r is not the error status, error_type and
// message given.
func checkRefusal(t *testing.T, what string, r response, status int, errorType, message string) {
	t.Helper()
	if r.status != status || r.errorField("error_type") != errorType || r.errorField("message") != message {
		t.Errorf("%s: answer = %d %s, want %d %s %q", what, r.status, r.body, status, errorType, message)
	}
}

// everyScope is every scope a route of the identity service declares, so
// that a PAT holding them passes the scope check of every identity route.
var everyScope = []string{"users:read", "users:write", "orgs:read", "tokens:read", "tokens:manage"}

// omitted marks a member that tokenBody leaves out.
type omitted struct{}

// tokenBody is a valid body of POST /user/tokens, the name ci with the
// permissions [git:read], with the members of set put in; a member set to
// omitted{} is left out.
func tokenBody(set map[string]any) map[string]any {
	body := map[string]any{"name": "ci", "permissions": []string{"git:read"}}
	for k, v := range set {
		if _, gone := v.(omitted); gone {
			delete(body, k)
		} else {
			body[k] = v
		}
	}
	return body
}

// createToken sends body to POST /api/v1/user/tokens with authorization as
// the Authorization header.
func (h *testHub) createToken(authorization string, body any) response {
	h.t.Helper()
	return h.call(http.MethodPost, "/api/v1/user/tokens", authorization, body)
}

// tokenCount counts the PATs of the user userID in the hub's store, revoked
// and expired ones included.
func (h *testHub) tokenCount(userID string) int {
	h.t.Helper()
	return h.count(`SELECT count(*) FROM pats WHERE user_id = ?`, userID)
}

// storedPAT reads the record of the PAT tokenID from the hub's store.
func (h *testHub) storedPAT(tokenID string) identity.PAT {
	h.t.Helper()
	rec, err := identity.PATByID(context.Background(), h.st.Main(), tokenID)
	if err != nil {
		h.t.Fatalf("read PAT %s: %v", tokenID, err)
	}
	return rec
}

// teamHub is a bootstrapped test hub with two users, each created with a
// personal org through identity.CreateUserAndPersonalOrg: root, role admin,
// e-mail root@example.com, and alice, role user, e-mail alice@example.com,
// both active and signing in with github. Its callers are the values of an
// Authorization header: admin is the admin token, ka and ku are live API
// keys of root and alice, and pa and pu live PATs of root and alice holding
// everyScope. None of them expires.
type teamHub struct {
	*testHub
	root, alice           identity.User
	admin, ka, ku, pa, pu string
}

// newTeamHub builds a team hub; opts are newTestHub's.
func newTeamHub(t *testing.T, opts ...hubOption) teamHub {
	t.Helper()
	h := teamHub{testHub: newTestHub(t, opts...)}
	h.root = h.createUser("root", identity.RoleAdmin)
	h.alice = h.createUser("alice", identity.RoleUser)
	bearer := func(credential string) string { return "Bearer " + credential }
	h.admin = bearer(h.adminToken)
	h.ka, _ = h.seedKey(h.root.ID, 0)
	h.ku, _ = h.seedKey(h.alice.ID, 0)
	h.pa, _ = h.seedPAT(identity.PATSpec{UserID: h.root.ID, Permissions: everyScope})
	h.pu, _ = h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: everyScope})
	h.ka, h.ku, h.pa, h.pu = bearer(h.ka), bearer(h.ku), bearer(h.pa), bearer(h.pu)
	return h
}

// createUser creates the active user username with the role role and the
// user's personal org, as the hub does: the e-mail is
// <username>@example.com and the github id is the username.
func (h *testHub) createUser(username, role string) identity.User {
	h.t.Helper()
	var u identity.User
	h.tx(func(tx *sql.Tx) (err error) {
		u, _, err = identity.CreateUserAndPersonalOrg(context.Background(), tx, identity.NewUser{
			Username: username, Email: username + "@example.com", Provider: "github", ProviderID: username,
			CreatedAt: h.now(),
		}, role)
		return err
	})
	return u
}

// call sends a request to the hub with authorization as the Authorization
// header ("" sends none) and body, when not nil, as JSON (see do).
func (h *testHub) call(method, path, authorization string, body any) response {
	h.t.Helper()
	header := http.Header{}
	if authorization != "" {
		header.Set("Authorization", authorization)
	}
	return h.do(method, path, body, header)
}

// snapshot reads every row of the users, orgs, memberships, API keys and
// PATs from the hub's store. Two snapshots are equal exactly when the
// store held the same records, so a test compares one taken before a
// request with one taken after it to show that the request changed nothing.
func (h *testHub) snapshot() string {
	h.t.Helper()
	ctx, db := context.Background(), h.st.Main()
	var b strings.Builder
	for _, table := range []string{"users", "orgs", "memberships", "api_keys", "pats"} {
		rows, err := db.QueryContext(ctx, `SELECT * FROM `+table+` ORDER BY rowid`)
		if err != nil {
			h.t.Fatalf("read %s: %v", table, err)
		}
		cols, err := rows.Columns()
		if err != nil {
			h.t.Fatalf("read the columns of %s: %v", table, err)
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				h.t.Fatalf("read a row of %s: %v", table, err)
			}
			fmt.Fprintln(&b, append([]any{table}, vals...)...)
		}
		if err := rows.Err(); err != nil {
			h.t.Fatalf("read %s: %v", table, err)
		}
		rows.Close()
	}
	return b.String()
}

// orgBySlug reads the org with the slug slug from the hub's store.
func (h *testHub) orgBySlug(slug string) identity.Org {
	h.t.Helper()
	o, err := identity.OrgBySlug(context.Background(), h.st.Main(), slug)
	if err != nil {
		h.t.Fatalf("read the org %s: %v", slug, err)
	}
	return o
}

// membershipsOf reads the memberships of the org orgID from the hub's
// store.
func (h *testHub) membershipsOf(orgID string) []identity.Membership {
	h.t.Helper()
	ms, err := identity.Memberships(context.Background(), h.st.Main(), orgID)
	if err != nil {
		h.t.Fatalf("read the memberships of %s: %v", orgID, err)
	}
	return ms
}

// setRole sets the role of the user userID.
func (h *testHub) setRole(userID, role string) {
	h.exec(`UPDATE users SET role = ? WHERE id = ?`, role, userID)
}

// setOrgStatus sets the status of the org orgID.
func (h *testHub) setOrgStatus(orgID, status string) {
	h.exec(`UPDATE orgs SET status = ? WHERE id = ?`, status, orgID)
}

// addMember makes the user userID a member of the org orgID, from now.
func (h *testHub) addMember(orgID, userID string) {
	h.exec(`INSERT INTO memberships (org_id, user_id, created_at) VALUES (?, ?, ?)`, orgID, userID, h.now().UnixNano())
}
