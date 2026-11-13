package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain silences slog.Default(), which a server built without
// Options.Logger writes its access log to, so a failing test's output is not
// buried under access-log lines. Tests that read the log pass a logBuffer.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

// errorMembers parses body as the REST error envelope and returns the members
// of its error object. It fails the test when body is not a JSON object whose
// only member is an object named error.
func errorMembers(t *testing.T, body []byte) map[string]any {
	t.Helper()
	doc := decodeObject(t, body)
	if len(doc) != 1 {
		t.Fatalf("body %s has %d top-level members, want only error", body, len(doc))
	}
	members, ok := doc["error"].(map[string]any)
	if !ok {
		t.Fatalf("body %s has no error object", body)
	}
	return members
}

// assertError fails the test unless rec holds the error envelope with status
// and errorType.
func assertError(t *testing.T, rec *httptest.ResponseRecorder, status int, errorType string) {
	t.Helper()
	if rec.Code != status {
		t.Errorf("status = %d, want %d (body %s)", rec.Code, status, rec.Body.Bytes())
		return
	}
	if ct := rec.Header().Get("Content-Type"); ct != jsonContentType {
		t.Errorf("Content-Type = %q, want %q", ct, jsonContentType)
	}
	members := errorMembers(t, rec.Body.Bytes())
	if members["error_type"] != errorType {
		t.Errorf("error_type = %v, want %q", members["error_type"], errorType)
	}
	if members["code"] != float64(status) {
		t.Errorf("code = %v, want %d", members["code"], status)
	}
}

// newRequest builds a request for an in-process call. Its RemoteAddr is
// httptest's default, 192.0.2.1:1234; a test that needs another client
// address sets it on the result.
func newRequest(method, target string, header http.Header, body string) *http.Request {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rd)
	for k, vs := range header {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	return r
}

// do serves r through h in-process and returns what h answered.
func do(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// callLog records, in order, the checks and handlers that ran. It is safe
// for concurrent requests.
type callLog struct {
	mu    sync.Mutex
	names []string
}

func (l *callLog) add(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.names = append(l.names, name)
}

// calls returns the names recorded so far, in order.
func (l *callLog) calls() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.names)
}

// check returns a Check that records name and answers err.
func (l *callLog) check(name string, err error) Check {
	return func(*http.Request) error {
		l.add(name)
		return err
	}
}

// authenticator returns an Authenticator that records "authentication" and
// answers caller and err.
func (l *callLog) authenticator(caller Caller, err error) Authenticator {
	return func(*http.Request) (Caller, error) {
		l.add("authentication")
		return caller, err
	}
}

// handler returns a HandlerFunc that records name and answers status with
// the body {"handler": name}.
func (l *callLog) handler(name string, status int) HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) error {
		l.add(name)
		WriteJSON(w, status, map[string]string{"handler": name})
		return nil
	}
}

// testCaller is the caller a passing authenticator reports.
var testCaller = Caller{CredentialID: "k1", UserID: "u1"}

// testClock is a clock the test moves by hand; its Now is safe for
// concurrent requests.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

// Now is the clock's current time, for Options.Now.
func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// logBuffer collects what a server logs, as JSON lines at debug level and
// above. It is safe for concurrent requests.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// logger returns a logger that writes to b, for Options.Logger.
func (b *logBuffer) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// lines returns the lines logged so far, in order.
func (b *logBuffer) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var lines []string
	for line := range strings.Lines(b.buf.String()) {
		lines = append(lines, strings.TrimSuffix(line, "\n"))
	}
	return lines
}

// decodeLine parses one logged line as a JSON object.
func decodeLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(line), &doc); err != nil {
		t.Fatalf("log line %q is not a JSON object: %v", line, err)
	}
	return doc
}

// assertSecurityHeaders fails the test unless the answer carries the three
// security headers every response of the server has.
func assertSecurityHeaders(t *testing.T, what string, h http.Header) {
	t.Helper()
	for name, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s: %s = %q, want %q", what, name, got, want)
		}
	}
}
