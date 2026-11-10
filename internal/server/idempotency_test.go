package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// createServer has create routes on a test clock, behind bearerAuth: each
// handler counts its calls and answers 201 with a fresh id and a secret.
type createServer struct {
	*Server
	clock *fakeClock
	store *MemoryIdempotencyStore
	calls atomic.Int32
	// respond, when set, decides the handler's answer for call n (1-based).
	respond func(n int32, w http.ResponseWriter) error
	delay   time.Duration
}

func newCreateServer() *createServer {
	c := &createServer{clock: newFakeClock(), store: NewMemoryIdempotencyStore()}
	opts := DefaultOptions()
	opts.Now = c.clock.Now
	opts.IdempotencyStore = c.store
	logger, _ := debugLogger()
	opts.Logger = logger
	c.Server = New(opts)
	c.SetAuthenticator(bearerAuth(nil))
	create := func(body func(n int32) any) HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) error {
			n := c.calls.Add(1)
			time.Sleep(c.delay)
			if c.respond != nil {
				if err := c.respond(n, w); err != nil {
					return err
				}
				return nil
			}
			WriteJSON(w, 201, body(n))
			return nil
		}
	}
	flat := func(n int32) any {
		return map[string]any{"id": fmt.Sprintf("id-%d", n), "token": fmt.Sprintf("secret-%d", n)}
	}
	nested := func(n int32) any {
		return map[string]any{"workspace": map[string]any{"slug": "w"}, "token": map[string]any{"token_id": "t", "token": fmt.Sprintf("secret-%d", n)}}
	}
	for _, p := range []string{"/things", "/others", "/owners/:owner/things"} {
		c.Register(Route{Method: "POST", Pattern: p, Create: true, ShowOnce: []string{"token"}, Handler: create(flat)})
	}
	c.Register(Route{Method: "POST", Pattern: "/nested", Create: true, ShowOnce: []string{"token.token"}, Handler: create(nested)})
	c.Register(Route{Method: "POST", Pattern: "/absent", Create: true, ShowOnce: []string{"secret"}, Handler: create(flat)})
	c.Register(Route{Method: "POST", Pattern: "/things/:id/block", Handler: func(w http.ResponseWriter, r *http.Request) error {
		c.calls.Add(1)
		WriteJSON(w, 200, map[string]any{"blocked": true})
		return nil
	}})
	return c
}

func (c *createServer) post(t *testing.T, path, key, body string, opts ...reqOpt) apiResponse {
	t.Helper()
	all := []reqOpt{bearer("a"), jsonBody(body)}
	if key != "\x00none" {
		all = append(all, header("Idempotency-Key", key))
	}
	return do(t, c.Handler(), "POST", "/api/v1"+path, append(all, opts...)...)
}

func TestIdempotency_ASuccessfulCreateIsRecordedWithoutShowOnceMembers(t *testing.T) {
	c := newCreateServer()
	r := c.post(t, "/things", "k1", `{"name":"a"}`)
	if r.status != 201 || r.json(t)["token"] != "secret-1" {
		t.Fatalf("first create = %d %s, want 201 with the token", r.status, r.body)
	}
	rec, err := c.store.Find(context.Background(), "k1", "user:u-a", "POST /api/v1/things")
	if err != nil || rec == nil {
		t.Fatalf("no record for k1: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(rec.Response, &stored); err != nil {
		t.Fatal(err)
	}
	if _, has := stored["token"]; has || strings.Contains(string(rec.Response), "secret-1") {
		t.Errorf("stored body %s keeps the show-once member", rec.Response)
	}
}

func TestIdempotency_AReplayWithinADayAnswersTheRecordedBody(t *testing.T) {
	c := newCreateServer()
	first := c.post(t, "/things", "k1", `{"name":"a"}`)
	c.clock.advance(23*time.Hour + 59*time.Minute)
	r := c.post(t, "/things", "k1", `{"name":"a"}`)
	if r.status != 200 {
		t.Fatalf("replay = %d, want 200", r.status)
	}
	if r.json(t)["id"] != first.json(t)["id"] {
		t.Errorf("replay id = %v, want %v", r.json(t)["id"], first.json(t)["id"])
	}
	if c.calls.Load() != 1 {
		t.Errorf("handler ran %d times, want 1", c.calls.Load())
	}
}

func TestIdempotency_AReplayOmitsShowOnceMembers(t *testing.T) {
	tests := []struct {
		path    string
		absent  func(map[string]any) bool
		secrets string
	}{
		{"/things", func(d map[string]any) bool { _, ok := d["token"]; return !ok }, "secret-1"},
		{"/nested", func(d map[string]any) bool {
			tok, _ := d["token"].(map[string]any)
			_, ok := tok["token"]
			return !ok && tok["token_id"] == "t"
		}, "secret-1"},
		{"/absent", func(d map[string]any) bool { _, ok := d["secret"]; return !ok }, "\x00"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			c := newCreateServer()
			first := c.post(t, tt.path, "k", "{}").json(t)
			r := c.post(t, tt.path, "k", "{}")
			if r.status != 200 {
				t.Fatalf("replay = %d", r.status)
			}
			doc := r.json(t)
			if !tt.absent(doc) {
				t.Errorf("replay %s keeps the declared member", r.body)
			}
			if strings.Contains(string(r.body), tt.secrets) {
				t.Errorf("replay %s carries the secret", r.body)
			}
			for k, v := range first {
				if k == "token" && tt.path != "/absent" {
					continue
				}
				if fmt.Sprint(doc[k]) != fmt.Sprint(v) {
					t.Errorf("member %s = %v, want %v", k, doc[k], v)
				}
			}
		})
	}
}

func TestIdempotency_AnotherBodyUnderTheSameKeyIsAConflict(t *testing.T) {
	c := newCreateServer()
	c.post(t, "/things", "k", `{"name":"a"}`)
	r := c.post(t, "/things", "k", `{"name":"b"}`)
	if r.status != 409 || r.errorType(t) != "conflict" {
		t.Fatalf("other body = %d %s, want 409 conflict", r.status, r.body)
	}
	if c.calls.Load() != 1 {
		t.Errorf("handler ran %d times, want 1", c.calls.Load())
	}
	if r := c.post(t, "/things", "k", `{"name":"a"}`); r.status != 200 {
		t.Errorf("original body again = %d, want 200", r.status)
	}
}

func TestIdempotency_AKeyMustBeOneTo255Characters(t *testing.T) {
	tests := []struct {
		key    string
		status int
	}{
		{"", 400}, {strings.Repeat("a", 256), 400}, {"a", 201}, {strings.Repeat("a", 255), 201}, {"\x00none", 201},
	}
	for _, tt := range tests {
		c := newCreateServer()
		r := c.post(t, "/things", tt.key, "{}")
		if r.status != tt.status {
			t.Errorf("key of %d characters = %d, want %d", len(tt.key), r.status, tt.status)
			continue
		}
		if tt.status == 400 {
			if r.errorType(t) != "invalid_request" || c.calls.Load() != 0 {
				t.Errorf("key of %d characters: %s, handler calls %d", len(tt.key), r.errorType(t), c.calls.Load())
			}
			if rec, _ := c.store.Find(context.Background(), tt.key, "user:u-a", "POST /api/v1/things"); rec != nil {
				t.Errorf("a refused key was recorded")
			}
		}
		if tt.key == "\x00none" {
			if r := c.post(t, "/things", "\x00none", "{}"); r.status != 201 || c.calls.Load() != 2 {
				t.Errorf("without a key the request was replayed: %d", r.status)
			}
		}
	}
}

func TestIdempotency_AFailedRequestIsNotRecorded(t *testing.T) {
	failures := []struct {
		name   string
		fail   func(w http.ResponseWriter) error
		status int
	}{
		{"400 invalid_request", func(http.ResponseWriter) error { return InvalidRequest("bad") }, 400},
		{"500 through a plain error", func(http.ResponseWriter) error { return errors.New("broken") }, 500},
		{"409 conflict", func(http.ResponseWriter) error { return Conflict("taken") }, 409},
	}
	for _, f := range failures {
		t.Run(f.name, func(t *testing.T) {
			c := newCreateServer()
			c.respond = func(n int32, w http.ResponseWriter) error {
				if n == 1 {
					return f.fail(w)
				}
				WriteJSON(w, 201, map[string]any{"id": fmt.Sprintf("id-%d", n)})
				return nil
			}
			if r := c.post(t, "/things", "k", "{}"); r.status != f.status {
				t.Fatalf("first = %d, want %d", r.status, f.status)
			}
			if r := c.post(t, "/things", "k", "{}"); r.status != 201 {
				t.Fatalf("second = %d, want 201", r.status)
			}
			if c.calls.Load() != 2 {
				t.Errorf("handler ran %d times, want 2", c.calls.Load())
			}
			if r := c.post(t, "/things", "k", "{}"); r.status != 200 {
				t.Errorf("third = %d, want a 200 replay", r.status)
			}
		})
	}
}

func TestIdempotency_ConcurrentRequestsWithOneKeyCreateOnce(t *testing.T) {
	run := func(t *testing.T, c *createServer) []apiResponse {
		var wg sync.WaitGroup
		out := make([]apiResponse, 20)
		for i := range out {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out[i] = c.post(t, "/things", "k", `{"name":"a"}`)
			}()
		}
		wg.Wait()
		return out
	}
	count := func(rs []apiResponse) map[int]int {
		m := map[int]int{}
		for _, r := range rs {
			m[r.status]++
		}
		return m
	}
	ids := func(t *testing.T, rs []apiResponse) map[any]bool {
		m := map[any]bool{}
		for _, r := range rs {
			if r.status < 300 {
				m[r.json(t)["id"]] = true
			}
		}
		return m
	}

	c := newCreateServer()
	c.delay = 20 * time.Millisecond
	rs := run(t, c)
	if c.calls.Load() != 1 {
		t.Errorf("handler ran %d times, want 1", c.calls.Load())
	}
	if got := count(rs); got[201] != 1 || got[200] != 19 {
		t.Errorf("statuses = %v, want one 201 and nineteen 200", got)
	}
	if got := ids(t, rs); len(got) != 1 {
		t.Errorf("ids = %v, want one", got)
	}

	c = newCreateServer()
	c.delay = 20 * time.Millisecond
	c.respond = func(n int32, w http.ResponseWriter) error {
		if n == 1 {
			return errors.New("first attempt fails")
		}
		WriteJSON(w, 201, map[string]any{"id": fmt.Sprintf("id-%d", n)})
		return nil
	}
	rs = run(t, c)
	if got := count(rs); got[500] != 1 || got[201] != 1 || got[200] != 18 {
		t.Errorf("statuses = %v, want one 500, one 201 and eighteen 200", got)
	}
	if c.calls.Load() != 2 {
		t.Errorf("handler ran %d times, want 2", c.calls.Load())
	}
	if got := ids(t, rs); len(got) != 1 {
		t.Errorf("ids = %v, want one", got)
	}
}

func TestIdempotency_AKeyOfAnotherCallerRouteOrDayIsNew(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, c *createServer) apiResponse
	}{
		{"another caller", func(t *testing.T, c *createServer) apiResponse {
			return do(t, c.Handler(), "POST", "/api/v1/things", bearer("b"), jsonBody(`{"name":"a"}`), header("Idempotency-Key", "k"))
		}},
		{"another route", func(t *testing.T, c *createServer) apiResponse { return c.post(t, "/others", "k", `{"name":"x"}`) }},
		{"another path parameter", func(t *testing.T, c *createServer) apiResponse {
			c.post(t, "/owners/a/things", "k2", "{}")
			return c.post(t, "/owners/b/things", "k2", "{}")
		}},
		{"the admin token", func(t *testing.T, c *createServer) apiResponse {
			return do(t, c.Handler(), "POST", "/api/v1/things", bearer("root"), jsonBody(`{"name":"a"}`), header("Idempotency-Key", "k"))
		}},
		{"24 hours later", func(t *testing.T, c *createServer) apiResponse {
			c.clock.advance(24 * time.Hour)
			return c.post(t, "/things", "k", `{"name":"a"}`)
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c := newCreateServer()
			c.post(t, "/things", "k", `{"name":"a"}`)
			before := c.calls.Load()
			r := tt.run(t, c)
			if r.status != 201 {
				t.Fatalf("status = %d %s, want the handler's 201", r.status, r.body)
			}
			if c.calls.Load() <= before {
				t.Error("the handler did not run")
			}
		})
	}

	c := newCreateServer()
	for _, key := range []string{"k", "k", ""} {
		r := c.post(t, "/things/42/block", key, "{}")
		if r.status != 200 {
			t.Errorf("non-create route with key %q = %d, want the handler's 200", key, r.status)
		}
	}
	if c.calls.Load() != 3 {
		t.Errorf("non-create handler ran %d times, want 3", c.calls.Load())
	}
}
