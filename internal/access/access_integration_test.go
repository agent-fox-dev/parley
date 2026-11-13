package access_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/store"
)

// accessHub is a real hub running in-process on a loopback port the system
// picked, bootstrapped with an admin token, with the probe routes added:
// POST /workspaces/:slug/probe needs write, GET /workspaces/:slug/probe-read
// read and GET /workspaces/:slug/probe-owner owner. A probe answers 200 with
// the decision's level and counts its calls.
type accessHub struct {
	url   string
	token string
	store *store.Store
	calls atomic.Int64
}

// startAccessHub bootstraps a hub in a fresh data directory, then serves it
// until the test ends.
func startAccessHub(t *testing.T) *accessHub {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[server]\nbind = \"127.0.0.1\"\nport = 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := config.MapEnv(map[string]string{"XDG_CONFIG_HOME": dir, "XDG_DATA_HOME": dir})
	var bootErr logBuffer
	if code := hub.Run(context.Background(), hub.Options{
		Args: []string{"--admin-email", "boss@example.com"}, Env: env, WorkDir: dir, Stderr: &bootErr,
	}); code != 0 {
		t.Fatalf("bootstrap returned %d, want 0; stderr = %q", code, bootErr.String())
	}
	tokenFile := filepath.Join(dir, "admin_token")
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(tokenFile); err != nil {
		t.Fatal(err)
	}

	h := &accessHub{token: strings.TrimSpace(string(token))}
	ctx, cancel := context.WithCancel(context.Background())
	stderr := &logBuffer{}
	done := make(chan int, 1)
	go func() {
		done <- hub.Run(ctx, hub.Options{Env: env, WorkDir: dir, Stderr: stderr, Compose: func(c *hub.Composition) {
			c.AddRoute(h.probe(http.MethodPost, "/workspaces/:slug/probe", access.NeedWrite))
			c.AddRoute(h.probe(http.MethodGet, "/workspaces/:slug/probe-read", access.NeedRead))
			c.AddRoute(h.probe(http.MethodGet, "/workspaces/:slug/probe-owner", access.NeedOwner))
			c.AddStartHook("test-store", func(_ context.Context, rt *hub.Runtime) error {
				h.store = rt.Store
				return nil
			})
		}})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	h.url = "http://" + listeningAddr(t, stderr, done) + "/api/v1"
	return h
}

// listeningAddr waits for the hub's listening line on stderr and returns
// its address.
func listeningAddr(t *testing.T, stderr *logBuffer, done <-chan int) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for line := range strings.SplitSeq(stderr.String(), "\n") {
			var rec struct {
				Msg  string `json:"msg"`
				Addr string `json:"addr"`
			}
			if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "listening" {
				return rec.Addr
			}
		}
		select {
		case code := <-done:
			t.Fatalf("the hub stopped with %d before listening; stderr = %q", code, stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no listening line within 5 s; stderr = %q", stderr.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// probe is a probe route method pattern that declares need.
func (h *accessHub) probe(method, pattern string, need access.Need) server.Route {
	return server.Route{
		Method: method, Pattern: pattern, Marks: map[string]string{access.MarkNeed: string(need)},
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			h.calls.Add(1)
			d, _ := access.DecisionOf(r)
			server.WriteJSON(w, http.StatusOK, map[string]any{"level": d.Level})
			return nil
		},
	}
}

// createUser stores the user username with role user and a personal org,
// and returns the user's id.
func (h *accessHub) createUser(t *testing.T, username string) string {
	t.Helper()
	ctx := context.Background()
	var u identity.User
	err := h.store.Tx(ctx, func(tx *sql.Tx) (err error) {
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

// issueKey issues an API key for the user userID.
func (h *accessHub) issueKey(t *testing.T, userID string) string {
	t.Helper()
	ctx := context.Background()
	var key string
	err := h.store.Tx(ctx, func(tx *sql.Tx) (err error) {
		key, _, err = identity.IssueAPIKey(ctx, tx, userID, 0, time.Now())
		return err
	})
	if err != nil {
		t.Fatalf("issue a key for %s: %v", userID, err)
	}
	return key
}

// createOrg creates the active org slug through the admin route and returns
// its id.
func (h *accessHub) createOrg(t *testing.T, slug string) string {
	t.Helper()
	a := h.call(t, http.MethodPost, "/orgs", h.token, map[string]string{"name": slug, "slug": slug})
	var org struct {
		ID string `json:"id"`
	}
	if a.status != http.StatusCreated || json.Unmarshal(a.body, &org) != nil || org.ID == "" {
		t.Fatalf("create org %s = %d %s, want 201 with an id", slug, a.status, a.body)
	}
	return org.ID
}

// addMember makes the user userID a member of the org orgID through the
// admin route.
func (h *accessHub) addMember(t *testing.T, orgID, userID string) {
	t.Helper()
	if a := h.call(t, http.MethodPut, "/orgs/"+orgID+"/members/"+userID, h.token, nil); a.status != http.StatusNoContent {
		t.Fatalf("add %s to %s = %d %s, want 204", userID, orgID, a.status, a.body)
	}
}

// removeMember removes the user userID from the org orgID through the admin
// route.
func (h *accessHub) removeMember(t *testing.T, orgID, userID string) {
	t.Helper()
	if a := h.call(t, http.MethodDelete, "/orgs/"+orgID+"/members/"+userID, h.token, nil); a.status != http.StatusNoContent {
		t.Fatalf("remove %s from %s = %d %s, want 204", userID, orgID, a.status, a.body)
	}
}

// insert stores recs in the hub's store, in order.
func (h *accessHub) insert(t *testing.T, recs ...access.Record) {
	t.Helper()
	seed(t, h.store, recs...)
}

// call sends method path, under /api/v1, with key as the bearer credential
// and body as JSON when it is not nil.
func (h *accessHub) call(t *testing.T, method, path, key string, body any) answer {
	t.Helper()
	var data io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		data = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.url+path, data)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
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
	return answer{res.StatusCode, res.Header.Get("Content-Type"), b}
}

// readable builds the readable set of the user userID with a service bound
// to the hub's store as the hub binds its own.
func (h *accessHub) readable(t *testing.T, userID string) access.ReadableSet {
	t.Helper()
	db := h.store.Main()
	svc := newService(access.StoreRecords(db), access.MembershipsFunc(func(ctx context.Context, id string) ([]string, error) {
		return identity.ActiveOrgIDs(ctx, db, id)
	}))
	set, err := svc.Readable(context.Background(), access.Caller{UserID: userID})
	if err != nil {
		t.Fatalf("Readable(%s): %v", userID, err)
	}
	return set
}

// logBuffer collects the hub's log lines while it runs.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestHub_RunsAMembersReadOfAnOrgWorkspaceThroughTheAccessCheck(t *testing.T) {
	t.Parallel()
	h := startAccessHub(t)
	owner, member := h.createUser(t, "owner"), h.createUser(t, "member")
	org := h.createOrg(t, "team")
	h.addMember(t, org, member)
	h.insert(t, repo("acme", owner, org, access.StatusReady))

	a := h.call(t, http.MethodGet, "/workspaces/acme/probe-read", h.issueKey(t, member), nil)
	if a.status != http.StatusOK || decode(t, a).Level != access.LevelMember || h.calls.Load() != 1 {
		t.Errorf("a member's read of acme = %d %s after %d probe calls, want 200 at level member and one call", a.status, a.body, h.calls.Load())
	}
}
