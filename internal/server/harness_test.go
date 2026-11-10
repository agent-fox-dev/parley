package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// apiResponse is a recorded answer of the server's handler.
type apiResponse struct {
	status int
	header http.Header
	body   []byte
}

func (r apiResponse) json(t *testing.T) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(r.body, &doc); err != nil {
		t.Fatalf("body %q is not a JSON object: %v", r.body, err)
	}
	return doc
}

// errorOf returns the members of the error envelope, failing the test when
// the body is not one.
func (r apiResponse) errorOf(t *testing.T) map[string]any {
	t.Helper()
	doc := r.json(t)
	e, ok := doc["error"].(map[string]any)
	if !ok || len(doc) != 1 {
		t.Fatalf("body %s is not the error envelope", r.body)
	}
	return e
}

func (r apiResponse) errorType(t *testing.T) string {
	t.Helper()
	s, _ := r.errorOf(t)["error_type"].(string)
	return s
}

// reqOpt shapes a test request.
type reqOpt func(*http.Request)

func bearer(v string) reqOpt {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+v) }
}

func header(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

func fromAddr(ip string) reqOpt { return func(r *http.Request) { r.RemoteAddr = ip + ":40000" } }

// withBody sets a body; the content type is application/json unless another
// option sets it.
func withBody(b []byte) reqOpt {
	return func(r *http.Request) {
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.ContentLength = int64(len(b))
		if r.Header.Get("Content-Type") == "" {
			r.Header.Set("Content-Type", "application/json")
		}
	}
}

func jsonBody(s string) reqOpt { return withBody([]byte(s)) }

// do sends one request to h and records the answer.
func do(t *testing.T, h http.Handler, method, target string, opts ...reqOpt) apiResponse {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = "192.0.2.1:40000"
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return apiResponse{status: rec.Code, header: rec.Result().Header, body: body}
}

// callLog records the order in which checks and handlers ran.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, name)
}

func (l *callLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

func (l *callLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = nil
}

// recordingCheck is a check that logs its name and returns fail.
func recordingCheck(log *callLog, name string, fail error) Check {
	return func(*http.Request) error {
		log.add(name)
		return fail
	}
}

// bearerAuth accepts every bearer value except "bad", reporting the value as
// the credential id and user "u-<value>"; "root" is the admin token.
func bearerAuth(log *callLog) Authenticator {
	return func(r *http.Request) (*Caller, error) {
		if log != nil {
			log.add("authentication")
		}
		v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || v == "" || v == "bad" {
			return nil, Unauthenticated("missing or invalid credential")
		}
		if v == "root" {
			return &Caller{CredentialID: "admin", Admin: true}, nil
		}
		return &Caller{CredentialID: v, UserID: "u-" + v}, nil
	}
}

// countingHandler answers status with an empty JSON object and counts calls.
type countingHandler struct {
	calls  atomic.Int32
	status int
}

func (h *countingHandler) handle(w http.ResponseWriter, _ *http.Request) error {
	h.calls.Add(1)
	WriteJSON(w, h.status, map[string]any{})
	return nil
}

var errPlain = errors.New("plain failure")

// fakeClock is a clock the test advances.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
