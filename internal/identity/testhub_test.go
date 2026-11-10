package identity

import (
	"context"
	"database/sql"
	"encoding/json"
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

	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/store"
)

// testClock is a clock tests set and advance.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// testHub is the request pipeline of internal/server with this package's
// authenticator installed, over a store in t.TempDir(), with two test
// routes: GET /test/whoami answers the caller, GET /test/me needs a user.
// Packages that build on identity extend this helper.
type testHub struct {
	t           *testing.T
	st          *store.Store
	dir         string
	clock       *testClock
	srv         *server.Server
	meCalls     atomic.Int32
	whoamiCalls atomic.Int32
}

func newTestHub(t *testing.T) *testHub {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{
		DatabasePath:  filepath.Join(dir, "data", "parley.db"),
		WorkspaceRoot: filepath.Join(dir, "data", "workspaces"),
	}, append([]store.Step{AdminSettingsStep}, Steps...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := &testHub{t: t, st: st, dir: dir, clock: &testClock{now: t0}}
	opts := server.DefaultOptions()
	opts.RequestsPerMinute = 0
	opts.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	opts.Now = h.clock.Now
	h.srv = server.New(opts)
	h.srv.SetAuthenticator(NewAuthenticator(st, h.clock.Now))
	h.srv.Register(server.Route{Method: "GET", Pattern: "/test/whoami", Handler: func(w http.ResponseWriter, r *http.Request) error {
		h.whoamiCalls.Add(1)
		server.WriteJSON(w, 200, PrincipalFrom(r.Context()))
		return nil
	}})
	h.srv.Register(server.Route{Method: "GET", Pattern: "/test/me", Marks: map[string]string{MarkNeedsUser: "true"},
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			h.meCalls.Add(1)
			server.WriteJSON(w, 200, map[string]any{})
			return nil
		}})
	return h
}

func (h *testHub) tx(fn func(tx *sql.Tx) error) {
	h.t.Helper()
	if err := h.st.Tx(context.Background(), fn); err != nil {
		h.t.Fatal(err)
	}
}

// exec runs one statement on the store, for arranging a credential's or a
// user's state.
func (h *testHub) exec(stmt string, args ...any) {
	h.t.Helper()
	h.tx(func(tx *sql.Tx) error { _, err := tx.Exec(stmt, args...); return err })
}

// bootstrapAdmin stores an admin token's hash as package 03's bootstrap does
// and returns the token.
func (h *testHub) bootstrapAdmin() string {
	h.t.Helper()
	tok, err := GenerateAdminToken()
	if err != nil {
		h.t.Fatal(err)
	}
	h.tx(func(tx *sql.Tx) error { return SetAdmin(context.Background(), tx, "", HashToken(tok)) })
	return tok
}

// seedUser stores an active user with role user.
func (h *testHub) seedUser(username string) User {
	h.t.Helper()
	var u User
	h.tx(func(tx *sql.Tx) error {
		var err error
		u, err = CreateUser(context.Background(), tx, User{Username: username, Email: username + "@x", Role: "user", Status: "active"}, h.clock.Now())
		return err
	})
	return u
}

// issueKey issues an API key of 90 days for the user.
func (h *testHub) issueKey(userID string) (key, keyID string) {
	h.t.Helper()
	h.tx(func(tx *sql.Tx) error {
		var err error
		key, keyID, _, err = IssueAPIKey(context.Background(), tx, userID, 90, h.clock.Now())
		return err
	})
	return key, keyID
}

// storePAT stores a PAT for the user.
func (h *testHub) storePAT(userID string, spec PATSpec) (token, tokenID string) {
	h.t.Helper()
	h.tx(func(tx *sql.Tx) error {
		var err error
		token, tokenID, err = StorePAT(context.Background(), tx, userID, spec, h.clock.Now())
		return err
	})
	return token, tokenID
}

type hubResponse struct {
	status int
	header http.Header
	body   string
	doc    map[string]any
}

func (r hubResponse) errorMember(name string) any {
	e, _ := r.doc["error"].(map[string]any)
	return e[name]
}

// do sends a request through the server's handler; auth is the whole
// Authorization value, or "" for none.
func (h *testHub) do(method, path, auth, body string) hubResponse {
	h.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	out := hubResponse{status: rec.Code, header: rec.Header(), body: rec.Body.String()}
	_ = json.Unmarshal(rec.Body.Bytes(), &out.doc)
	return out
}

func (h *testHub) get(path, auth string) hubResponse { return h.do("GET", path, auth, "") }

func bearer(cred string) string { return "Bearer " + cred }
