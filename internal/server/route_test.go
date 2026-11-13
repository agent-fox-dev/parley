package server

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestRegister_RunsTheMatchedHandlerWithItsPathParameters(t *testing.T) {
	t.Parallel()
	var log callLog
	s := New(Options{})
	s.SetAuthenticator(log.authenticator(testCaller, nil))
	var param string
	var matched Matched
	s.Register(Route{Method: http.MethodGet, Pattern: "/things", Handler: log.handler("list", http.StatusOK)})
	s.Register(Route{
		Method:  http.MethodGet,
		Pattern: "/things/:id",
		Scope:   "things:read",
		Marks:   map[string]string{"access": "read"},
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			param = Param(r, "id")
			matched, _ = MatchedRoute(r)
			WriteJSON(w, http.StatusOK, map[string]string{"id": param})
			return nil
		},
	})
	h := s.Handler()

	rec := do(h, newRequest(http.MethodGet, "/api/v1/things/42", nil, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/things/42: status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.Bytes())
	}
	if got, want := decodeObject(t, rec.Body.Bytes()), map[string]any{"id": "42"}; !reflect.DeepEqual(got, want) {
		t.Errorf("body = %v, want %v", got, want)
	}
	if param != "42" {
		t.Errorf("Param(id) = %q, want %q", param, "42")
	}
	if matched.Method != http.MethodGet || matched.Pattern != "/things/:id" {
		t.Errorf("matched route = %s %s, want GET /things/:id", matched.Method, matched.Pattern)
	}
	if want := map[string]string{"id": "42"}; !reflect.DeepEqual(matched.Params, want) {
		t.Errorf("matched params = %v, want %v", matched.Params, want)
	}
	if matched.Route.Scope != "things:read" || matched.Route.Marks["access"] != "read" {
		t.Errorf("matched declarations = scope %q, marks %v, want things:read and access=read", matched.Route.Scope, matched.Route.Marks)
	}

	rec = do(h, newRequest(http.MethodGet, "/api/v1/things", nil, ""))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /api/v1/things: status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got, want := decodeObject(t, rec.Body.Bytes()), map[string]any{"handler": "list"}; !reflect.DeepEqual(got, want) {
		t.Errorf("GET /api/v1/things: body = %v, want %v", got, want)
	}
}

func TestRouting_AnswersNotFoundForAPathNoRouteMatches(t *testing.T) {
	t.Parallel()
	var log callLog
	s := New(Options{})
	s.SetAuthenticator(log.authenticator(testCaller, nil))
	s.Register(Route{Method: http.MethodGet, Pattern: "/things", Handler: log.handler("things", http.StatusOK)})
	s.Handle("GET /files/", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { log.add("files") }))
	h := s.Handler()

	paths := []string{
		"/api/v1/nope",
		"/nope",
		"/api/v1/things/",
		"/api/v2/things",
		"/api/v1",
		"//api/v1/things",
		"/api/v1//things",
		"/api/v1/./things",
		"/api/v1/nope/../things",
		"/files",
	}
	for _, path := range paths {
		for _, cv := range credentialVariants {
			t.Run(path+"/"+cv.name, func(t *testing.T) {
				rec := do(h, newRequest(http.MethodGet, path, cv.header, ""))

				assertError(t, rec, http.StatusNotFound, "not_found")
				if loc := rec.Header().Get("Location"); loc != "" {
					t.Errorf("Location = %q, want no redirect", loc)
				}
				if calls := log.calls(); len(calls) != 0 {
					t.Errorf("ran %v, want no authenticator call and no handler", calls)
				}
			})
		}
	}
}

func TestRouting_AnswersMethodNotAllowedWithTheRegisteredMethods(t *testing.T) {
	t.Parallel()
	var log callLog
	s := New(Options{})
	s.SetAuthenticator(log.authenticator(testCaller, nil))
	for _, rt := range []struct{ method, pattern string }{
		{http.MethodGet, "/things"},
		{http.MethodPost, "/things"},
		{http.MethodGet, "/things/:id"},
		{http.MethodGet, "/blobs/:id"},
		{http.MethodHead, "/blobs/:id"},
	} {
		s.Register(Route{Method: rt.method, Pattern: rt.pattern, Handler: log.handler(rt.method+" "+rt.pattern, http.StatusOK)})
	}
	h := s.Handler()

	tests := []struct {
		method, path string
		wantAllow    []string
	}{
		{http.MethodDelete, "/api/v1/things", []string{"GET", "POST"}},
		{http.MethodPut, "/api/v1/things/7", []string{"GET"}},
		{http.MethodPost, "/api/v1/blobs/7", []string{"GET", "HEAD"}},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := do(h, newRequest(tt.method, tt.path, nil, ""))

			assertError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
			allow := strings.Split(rec.Header().Get("Allow"), ", ")
			slices.Sort(allow)
			if !slices.Equal(allow, tt.wantAllow) {
				t.Errorf("Allow = %q, want the methods %v joined by \", \"", rec.Header().Get("Allow"), tt.wantAllow)
			}
			if calls := log.calls(); len(calls) != 0 {
				t.Errorf("ran %v, want no authenticator call and no handler", calls)
			}
		})
	}
}

func TestRegister_PanicsOnAnInvalidOrDuplicateRoute(t *testing.T) {
	t.Parallel()
	ok := func(http.ResponseWriter, *http.Request) error { return nil }
	tests := []struct {
		name  string
		route Route
	}{
		{"pattern without a leading slash", Route{Method: "GET", Pattern: "things", Handler: ok}},
		{"pattern with a trailing slash", Route{Method: "GET", Pattern: "/things/", Handler: ok}},
		{"pattern with an empty segment", Route{Method: "GET", Pattern: "/things//x", Handler: ok}},
		{"pattern with a router wildcard", Route{Method: "GET", Pattern: "/things/{id}", Handler: ok}},
		{"parameter without a name", Route{Method: "GET", Pattern: "/things/:", Handler: ok}},
		{"parameter named twice", Route{Method: "GET", Pattern: "/things/:id/parts/:id", Handler: ok}},
		{"lower-case method", Route{Method: "get", Pattern: "/things", Handler: ok}},
		{"no method", Route{Pattern: "/things", Handler: ok}},
		{"no handler", Route{Method: "GET", Pattern: "/things"}},
		{"route already registered", Route{Method: "GET", Pattern: "/taken", Handler: ok}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := New(Options{})
			s.Register(Route{Method: "GET", Pattern: "/taken", Handler: ok})
			defer func() {
				if recover() == nil {
					t.Errorf("Register(%s %q) did not panic", tt.route.Method, tt.route.Pattern)
				}
			}()
			s.Register(tt.route)
		})
	}
}
