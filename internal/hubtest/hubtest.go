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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
// what no route does: seeding users and issuing an API key the way a login
// does.
type Hub struct {
	*Served
	Token string
	Store *store.Store
}

// Start bootstraps a hub with an admin token in a fresh data directory,
// then serves it on a free loopback port with compose run on its
// composition (nil for none), until the test ends. The hub reads no process
// environment, so tests that start one may run in parallel.
func Start(t testing.TB, compose func(*hub.Composition)) *Hub {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[server]\nbind = \"127.0.0.1\"\nport = 0\n"), 0o644); err != nil {
		t.Fatal(err)
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
	h.Served = Serve(t, hub.Options{Env: env, WorkDir: dir, Compose: func(c *hub.Composition) {
		c.AddStartHook("test-store", func(_ context.Context, rt *hub.Runtime) error {
			h.Store = rt.Store
			return nil
		})
		if compose != nil {
			compose(c)
		}
	}})
	return h
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
