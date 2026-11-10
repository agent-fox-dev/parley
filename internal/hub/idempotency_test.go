package hub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/server"
)

// testClock is a clock the test advances.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestIdempotency_ARecordedCreateIsReplayedAfterARestart(t *testing.T) {
	dir := hubDir(t, loopbackConfig)
	clock := &testClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	var created atomic.Int32
	routes := func(calls *atomic.Int32) Option {
		return withCompose(func(c *Composition) {
			c.OnServer(func(rt *Runtime, srv *server.Server) error {
				srv.SetAuthenticator(func(*http.Request) (*server.Caller, error) {
					return &server.Caller{CredentialID: "k1", UserID: "u1"}, nil
				})
				srv.Register(server.Route{Method: "POST", Pattern: "/things", Create: true, ShowOnce: []string{"token"},
					Handler: func(w http.ResponseWriter, r *http.Request) error {
						calls.Add(1)
						n := created.Add(1)
						server.WriteJSON(w, 201, map[string]string{"id": fmt.Sprintf("id-%d", n), "token": fmt.Sprintf("secret-%d", n)})
						return nil
					}})
				return nil
			})
		})
	}
	withClock := func(o *Options) { o.Now = clock.Now }
	post := func(h *runningHub) (int, map[string]any) {
		req, _ := http.NewRequest("POST", h.url("/api/v1/things"), strings.NewReader(`{"name":"a"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "k")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var doc map[string]any
		_ = json.Unmarshal(b, &doc)
		return resp.StatusCode, doc
	}

	var first atomic.Int32
	h1 := startHub(t, dir, nil, routes(&first), withClock)
	code, r1 := post(h1)
	if code != 201 || r1["token"] == nil {
		t.Fatalf("first create = %d %v", code, r1)
	}
	if c := h1.stop(); c != 0 {
		t.Fatalf("first hub exited %d", c)
	}

	var second atomic.Int32
	h2 := startHub(t, dir, nil, routes(&second), withClock)
	code, r2 := post(h2)
	if _, hasToken := r2["token"]; code != 200 || r2["id"] != r1["id"] || hasToken {
		t.Errorf("replay after restart = %d %v, want 200 with id %v and no token", code, r2, r1["id"])
	}
	if second.Load() != 0 {
		t.Errorf("the handler ran %d times after the restart", second.Load())
	}
	clock.advance(24 * time.Hour)
	if code, _ := post(h2); code != 201 || second.Load() != 1 {
		t.Errorf("after 24 hours = %d with %d handler calls, want 201 and 1", code, second.Load())
	}
}
