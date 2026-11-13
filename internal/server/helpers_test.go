package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

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
