// Package hubtest runs a real hub in-process for tests. Serve runs hub.Run
// in a goroutine until the test ends, and Start bootstraps a hub with an
// admin token first, then serves it on a loopback port the system picks.
// The tests of every module that need a running hub use it, so that there
// is one way to start one.
package hubtest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/store"
)

// LogBuffer is a buffer a hub may write to while the test reads it.
type LogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *LogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *LogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Served is a hub running in a goroutine.
type Served struct {
	// Log is what the hub wrote to its stderr.
	Log *LogBuffer
	// Listening is the hub's listening line.
	Listening map[string]any

	code   chan int
	cancel context.CancelFunc
}

// Serve runs hub.Run with opts, its stderr captured in Log, and waits up to
// 5 s for its listening line. The hub is stopped when the test ends.
func Serve(t testing.TB, opts hub.Options) *Served {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &Served{Log: &LogBuffer{}, code: make(chan int, 1), cancel: cancel}
	opts.Stderr = s.Log
	go func() { s.code <- hub.Run(ctx, opts) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-s.code:
		case <-time.After(5 * time.Second):
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		for line := range strings.SplitSeq(s.Log.String(), "\n") {
			var rec map[string]any
			if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == "listening" {
				s.Listening = rec
				return s
			}
		}
		select {
		case code := <-s.code:
			t.Fatalf("the hub stopped with %d before listening; stderr = %q", code, s.Log.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no listening line within 5 s; stderr = %q", s.Log.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// URL is the hub's URL for path.
func (s *Served) URL(path string) string {
	addr, _ := s.Listening["addr"].(string)
	return "http://" + addr + path
}

// Stop asks the hub to stop, as a cancelled context does.
func (s *Served) Stop() { s.cancel() }

// ExitCode waits up to within for hub.Run to return and gives its status.
func (s *Served) ExitCode(t testing.TB, within time.Duration) int {
	t.Helper()
	select {
	case code := <-s.code:
		s.code <- code // the cleanup reads it again
		return code
	case <-time.After(within):
		t.Fatalf("hub.Run did not return within %v", within)
		return -1
	}
}

// Hub is a running hub bootstrapped with an admin token, and its store, for
// what no route does: seeding users, issuing an API key the way a login
// does, storing a PAT and a workspace record. Orgs and their members are
// made through the admin routes, with Call.
type Hub struct {
	*Served
	Token string
	Store *store.Store
	// opts are the options the hub is served with, which Restart serves it
	// with again.
	opts hub.Options
}

// Config is what StartWith starts a hub with besides its defaults.
type Config struct {
	// Server is TOML added inside the [server] table, after bind and port,
	// such as an external_url line.
	Server string
	// TOML is added to config.toml after the [server] table, such as a
	// [sandbox] section.
	TOML string
	// Files are written into the config dir, where config.toml is, before
	// the hub is bootstrapped: name to content. A relative path in the
	// configuration is taken from that directory.
	Files map[string]string
	// Compose is run on the hub's composition; nil for nothing.
	Compose func(*hub.Composition)
	// Now is the hub's clock once it is served; nil for time.Now.
	Now func() time.Time
}

// Start bootstraps a hub with an admin token in a fresh data directory,
// then serves it on a free loopback port with compose run on its
// composition (nil for none), until the test ends. The hub reads no process
// environment, so tests that start one may run in parallel.
func Start(t testing.TB, compose func(*hub.Composition)) *Hub {
	t.Helper()
	return StartWith(t, Config{Compose: compose})
}

// StartWith is Start with what cfg adds to config.toml and to the config
// dir.
func StartWith(t testing.TB, cfg Config) *Hub {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{"config.toml": "[server]\nbind = \"127.0.0.1\"\nport = 0\n" + cfg.Server + "\n" + cfg.TOML}
	for name, content := range cfg.Files {
		files[name] = content
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := config.MapEnv(map[string]string{"XDG_CONFIG_HOME": dir, "XDG_DATA_HOME": dir})
	var out, errb LogBuffer
	if code := hub.Run(context.Background(), hub.Options{
		Args: []string{"--admin-email", "boss@example.com"}, Env: env, WorkDir: dir, Stdout: &out, Stderr: &errb,
	}); code != 0 {
		t.Fatalf("bootstrap returned %d, want 0; stderr = %q", code, errb.String())
	}
	tokenFile := filepath.Join(dir, "admin_token")
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(tokenFile); err != nil {
		t.Fatal(err)
	}
	h := &Hub{Token: strings.TrimSpace(string(token))}
	h.opts = hub.Options{Env: env, WorkDir: dir, Now: cfg.Now, Compose: func(c *hub.Composition) {
		c.AddStartHook("test-store", func(_ context.Context, rt *hub.Runtime) error {
			h.Store = rt.Store
			return nil
		})
		if cfg.Compose != nil {
			cfg.Compose(c)
		}
	}}
	h.Served = Serve(t, h.opts)
	return h
}

// Restart stops the hub, waits up to 10 s for it to return with status 0,
// and serves it again with the same data directory, configuration and
// composition on the same port, as a restart of parleyd does. Served, URL
// and Store are those of the new run afterwards.
func (h *Hub) Restart(t testing.TB) {
	t.Helper()
	addr, _ := h.Listening["addr"].(string)
	_, portText, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("the listening address %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("the listening port %q: %v", portText, err)
	}
	h.Stop()
	if code := h.ExitCode(t, 10*time.Second); code != 0 {
		t.Fatalf("the hub stopped with %d, want 0; stderr = %q", code, h.Log.String())
	}
	opts := h.opts
	opts.Port = &port
	h.Served = Serve(t, opts)
}

// CreateUser stores the user username with role user and a personal org,
// and returns the user's id.
func (h *Hub) CreateUser(t testing.TB, username string) string {
	t.Helper()
	ctx := context.Background()
	var u identity.User
	err := h.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		u, _, err = identity.CreateUserAndPersonalOrg(ctx, tx, identity.NewUser{
			Username: username, Email: username + "@example.com", Provider: "github", ProviderID: username, CreatedAt: time.Now(),
		}, identity.RoleUser)
		return err
	})
	if err != nil {
		t.Fatalf("create user %s: %v", username, err)
	}
	return u.ID
}

// IssueKey issues an API key for the user userID, with the function a
// login issues its key with.
func (h *Hub) IssueKey(t testing.TB, userID string) string {
	t.Helper()
	ctx := context.Background()
	var key string
	err := h.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		key, _, err = identity.IssueAPIKey(ctx, tx, userID, 0, time.Now())
		return err
	})
	if err != nil {
		t.Fatalf("issue a key for %s: %v", userID, err)
	}
	return key
}

// IssuePAT stores the PAT spec describes, as minting one does, and returns
// the token. An empty name is "test" and a zero creation time is now.
func (h *Hub) IssuePAT(t testing.TB, spec identity.PATSpec) string {
	t.Helper()
	if spec.Name == "" {
		spec.Name = "test"
	}
	if spec.CreatedAt.IsZero() {
		spec.CreatedAt = time.Now()
	}
	ctx := context.Background()
	var token string
	err := h.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		token, _, err = identity.StorePAT(ctx, tx, spec)
		return err
	})
	if err != nil {
		t.Fatalf("store a PAT for %s: %v", spec.UserID, err)
	}
	return token
}

// CreateOrg creates the active org slug, named slug, through the admin
// route and returns its id.
func (h *Hub) CreateOrg(t testing.TB, slug string) string {
	t.Helper()
	status, body := h.Call(t, http.MethodPost, "/api/v1/orgs", h.Token, map[string]string{"name": slug, "slug": slug})
	var org struct {
		ID string `json:"id"`
	}
	if status != http.StatusCreated || json.Unmarshal(body, &org) != nil || org.ID == "" {
		t.Fatalf("create org %s = %d %s, want 201 with an id", slug, status, body)
	}
	return org.ID
}

// AddMember makes the user userID a member of the org orgID through the
// admin route.
func (h *Hub) AddMember(t testing.TB, orgID, userID string) {
	t.Helper()
	if status, body := h.Call(t, http.MethodPut, "/api/v1/orgs/"+orgID+"/members/"+userID, h.Token, nil); status != http.StatusNoContent {
		t.Fatalf("add member %s to %s = %d %s, want 204", userID, orgID, status, body)
	}
}

// AddWorkspace stores the workspace record rec, the record the access rule
// decides from, as registering or forking a workspace does.
func (h *Hub) AddWorkspace(t testing.TB, rec access.Record) {
	t.Helper()
	ctx := context.Background()
	if err := h.Store.Tx(ctx, func(tx *sql.Tx) error { return access.InsertRecord(ctx, tx, rec) }); err != nil {
		t.Fatalf("store the workspace record %+v: %v", rec, err)
	}
}

// Call sends method path to the hub with credential as the bearer
// credential ("" for none) and body: nil for none, a []byte or a string as
// it is, anything else as JSON; a body is sent as application/json. It
// returns the status and the body of the answer, and fails the test when the
// request cannot be sent or the answer read.
func (s *Served) Call(t testing.TB, method, path, credential string, body any) (int, []byte) {
	t.Helper()
	var data io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		data = bytes.NewReader(b)
	case string:
		data = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		data = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, s.URL(path), data)
	if err != nil {
		t.Fatal(err)
	}
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("%s %s: read the body: %v", method, path, err)
	}
	return res.StatusCode, b
}
