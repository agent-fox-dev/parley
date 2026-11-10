package server

import (
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRegister_RunsTheMatchedHandlerWithItsPathParameters(t *testing.T) {
	s := New(Options{})
	s.SetAuthenticator(bearerAuth(nil))
	var sawID string
	var listRan atomic.Bool
	s.Register(Route{Method: "GET", Pattern: "/things/:id", Handler: func(w http.ResponseWriter, r *http.Request) error {
		sawID = Param(r, "id")
		WriteJSON(w, 200, map[string]string{"id": sawID})
		return nil
	}})
	s.Register(Route{Method: "GET", Pattern: "/things", Handler: func(w http.ResponseWriter, r *http.Request) error {
		listRan.Store(true)
		WriteJSON(w, 200, map[string]any{"items": []any{}})
		return nil
	}})
	h := s.Handler()

	if r := do(t, h, "GET", "/api/v1/things/42", bearer("a")); r.status != 200 || sawID != "42" {
		t.Fatalf("GET /api/v1/things/42 = %d with id %q, want 200 and 42", r.status, sawID)
	}
	if r := do(t, h, "GET", "/api/v1/things", bearer("a")); r.status != 200 || !listRan.Load() {
		t.Fatalf("GET /api/v1/things = %d, list ran %v", r.status, listRan.Load())
	}
}

// countingAuth counts calls and accepts everything.
func countingAuth(n *atomic.Int32) Authenticator {
	return func(*http.Request) (*Caller, error) {
		n.Add(1)
		return &Caller{CredentialID: "c", UserID: "u"}, nil
	}
}

func TestRouting_UnknownPathIsNotFoundBeforeAuthentication(t *testing.T) {
	var authCalls atomic.Int32
	handler := &countingHandler{status: 200}
	s := New(Options{})
	s.SetAuthenticator(countingAuth(&authCalls))
	s.Register(Route{Method: "GET", Pattern: "/things", Handler: handler.handle})
	h := s.Handler()

	for _, path := range []string{"/api/v1/nope", "/nope", "/api/v1/things/", "/api/v2/things", "/api/v1", "/api/v1//things"} {
		for _, opts := range [][]reqOpt{nil, {bearer("bogus")}} {
			r := do(t, h, "GET", path, opts...)
			if r.status != http.StatusNotFound {
				t.Fatalf("GET %s = %d, want 404 (never a redirect)", path, r.status)
			}
			e := r.errorOf(t)
			if e["error_type"] != "not_found" || e["code"] != float64(404) {
				t.Errorf("GET %s error = %v, want not_found 404", path, e)
			}
		}
	}
	if n := authCalls.Load(); n != 0 {
		t.Errorf("authenticator called %d times, want 0", n)
	}
	if n := handler.calls.Load(); n != 0 {
		t.Errorf("handler ran %d times, want 0", n)
	}
}

func TestRouting_WrongMethodIsMethodNotAllowedWithAllow(t *testing.T) {
	var authCalls atomic.Int32
	handler := &countingHandler{status: 200}
	s := New(Options{})
	s.SetAuthenticator(countingAuth(&authCalls))
	s.Register(Route{Method: "GET", Pattern: "/things", Handler: handler.handle})
	s.Register(Route{Method: "POST", Pattern: "/things", Handler: handler.handle})

	r := do(t, s.Handler(), "DELETE", "/api/v1/things")
	if r.status != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", r.status)
	}
	if et := r.errorType(t); et != "method_not_allowed" {
		t.Errorf("error_type = %q", et)
	}
	allow := strings.Split(r.header.Get("Allow"), ", ")
	sort.Strings(allow)
	if strings.Join(allow, ",") != "GET,POST" {
		t.Errorf("Allow = %q, want GET and POST", r.header.Get("Allow"))
	}
	if authCalls.Load() != 0 || handler.calls.Load() != 0 {
		t.Errorf("authenticator %d and handler %d calls, want 0", authCalls.Load(), handler.calls.Load())
	}
}

func TestRouting_WrongMethodOnAnotherSurfaceIsMethodNotAllowed(t *testing.T) {
	r := do(t, New(Options{}).Handler(), "POST", "/healthz")
	if r.status != http.StatusMethodNotAllowed || r.errorType(t) != "method_not_allowed" {
		t.Fatalf("POST /healthz = %d %s, want 405 method_not_allowed", r.status, r.body)
	}
	if !strings.Contains(r.header.Get("Allow"), "GET") {
		t.Errorf("Allow = %q, want GET", r.header.Get("Allow"))
	}
}
