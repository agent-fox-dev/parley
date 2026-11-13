package identity_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/store"
)

// testHub is the request pipeline of internal/server with the identity
// service and its routes installed, over a store in t.TempDir(), served
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

// seedOrg stores an active org without an owner.
func (h *testHub) seedOrg(name, slug string) {
	h.t.Helper()
	now := h.now().UnixNano()
	h.exec(`INSERT INTO orgs (id, name, slug, status, created_at, updated_at) VALUES (?, ?, ?, 'active', ?, ?)`,
		rand.Text(), name, slug, now, now)
}
