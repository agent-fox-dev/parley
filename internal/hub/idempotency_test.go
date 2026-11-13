package hub_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/server"
)

// clock is a time the test advances.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// thingsRoute composes a create route POST /api/v1/things whose token is
// shown once, and an authenticator that reports caller u1. Its handler counts
// its calls in calls.
func thingsRoute(calls *atomic.Int64) func(*hub.Composition) {
	return func(c *hub.Composition) {
		c.SetAuthenticator(func(*http.Request) (server.Caller, error) {
			return server.Caller{CredentialID: "c1", UserID: "u1"}, nil
		})
		c.AddRoute(server.Route{
			Method:   http.MethodPost,
			Pattern:  "/things",
			Create:   true,
			ShowOnce: []string{"token"},
			Handler: func(w http.ResponseWriter, _ *http.Request) error {
				n := calls.Add(1)
				server.WriteJSON(w, http.StatusCreated, map[string]string{
					"id":    fmt.Sprintf("thing-%d-%d", time.Now().UnixNano(), n),
					"token": fmt.Sprintf("secret-%d", n),
				})
				return nil
			},
		})
	}
}

type thing struct {
	status int
	doc    map[string]any
}

func postThing(t *testing.T, r *running, key string) thing {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, r.url("/api/v1/things"), strings.NewReader(`{"name":"x"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/things: %v", err)
	}
	defer res.Body.Close()
	var doc map[string]any
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		t.Fatalf("POST /api/v1/things: body is not JSON: %v", err)
	}
	return thing{status: res.StatusCode, doc: doc}
}

func TestRun_ReplaysAnIdempotentCreateRecordedBeforeARestart(t *testing.T) {
	t.Parallel()
	dir := workDir(t, localConfig)
	clk := &clock{now: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)}

	var before atomic.Int64
	first := startHub(t, hub.Options{WorkDir: dir, Now: clk.Now, Compose: thingsRoute(&before)})
	r1 := postThing(t, first, "k")
	if r1.status != http.StatusCreated || r1.doc["token"] == nil || r1.doc["token"] == "" {
		t.Fatalf("first create = %d %v, want 201 with a token", r1.status, r1.doc)
	}
	if code := first.stop(t); code != 0 {
		t.Fatalf("first hub returned %d after a stop, want 0", code)
	}

	var after atomic.Int64
	second := startHub(t, hub.Options{WorkDir: dir, Now: clk.Now, Compose: thingsRoute(&after)})
	r2 := postThing(t, second, "k")
	if r2.status != http.StatusOK || r2.doc["id"] != r1.doc["id"] {
		t.Errorf("replay after the restart = %d %v, want 200 with id %v", r2.status, r2.doc, r1.doc["id"])
	}
	if _, ok := r2.doc["token"]; ok {
		t.Errorf("replay %v carries the show-once token", r2.doc)
	}
	if n := after.Load(); n != 0 {
		t.Errorf("the handler ran %d times for the replay, want 0", n)
	}

	clk.Advance(24 * time.Hour)
	r3 := postThing(t, second, "k")
	if r3.status != http.StatusCreated || after.Load() != 1 {
		t.Errorf("create 24 h later = %d with %d handler calls, want 201 with 1", r3.status, after.Load())
	}
}
