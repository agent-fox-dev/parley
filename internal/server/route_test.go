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

func TestRouting_PrefersALiteralSegmentOverAParameter(t *testing.T) {
	t.Parallel()
	var log callLog
	s := New(Options{})
	s.SetAuthenticator(log.authenticator(testCaller, nil))
	for _, rt := range []struct{ method, pattern string }{
		{http.MethodGet, "/w/:slug/changes/:number"},
		{http.MethodGet, "/w/:slug/changes/by-commit/:sha"},
		{http.MethodGet, "/w/:slug/changes/:number/stack"},
		{http.MethodPost, "/w/:slug/changes/:number/stack"},
		{http.MethodGet, "/w/:slug/changes/:number/versions/:version"},
	} {
		s.Register(Route{Method: rt.method, Pattern: rt.pattern, Handler: func(w http.ResponseWriter, r *http.Request) error {
			m, _ := MatchedRoute(r)
			WriteJSON(w, http.StatusOK, map[string]any{"route": m.Method + " " + m.Pattern, "params": m.Params})
			return nil
		}})
	}
	h := s.Handler()

	tests := []struct {
		method, path, wantRoute string
		wantParams              map[string]any
	}{
		{http.MethodGet, "/api/v1/w/p1/changes/by-commit", "GET /w/:slug/changes/:number",
			map[string]any{"slug": "p1", "number": "by-commit"}},
		{http.MethodGet, "/api/v1/w/p1/changes/by-commit/a1b2", "GET /w/:slug/changes/by-commit/:sha",
			map[string]any{"slug": "p1", "sha": "a1b2"}},
		{http.MethodGet, "/api/v1/w/p1/changes/by-commit/stack", "GET /w/:slug/changes/by-commit/:sha",
			map[string]any{"slug": "p1", "sha": "stack"}},
		{http.MethodGet, "/api/v1/w/p1/changes/7/stack", "GET /w/:slug/changes/:number/stack",
			map[string]any{"slug": "p1", "number": "7"}},
		{http.MethodHead, "/api/v1/w/p1/changes/by-commit/stack", "", nil},
		{http.MethodPost, "/api/v1/w/p1/changes/by-commit/stack", "POST /w/:slug/changes/:number/stack",
			map[string]any{"slug": "p1", "number": "by-commit"}},
		{http.MethodGet, "/api/v1/w/p1/changes/by-commit/versions/2", "GET /w/:slug/changes/:number/versions/:version",
			map[string]any{"slug": "p1", "number": "by-commit", "version": "2"}},
		{http.MethodGet, "/api/v1/w/a%2Fb/changes/by-commit/c%20d", "GET /w/:slug/changes/by-commit/:sha",
			map[string]any{"slug": "a/b", "sha": "c d"}},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := do(h, newRequest(tt.method, tt.path, nil, ""))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.Bytes())
			}
			if tt.method == http.MethodHead {
				return
			}
			want := map[string]any{"route": tt.wantRoute, "params": tt.wantParams}
			if got := decodeObject(t, rec.Body.Bytes()); !reflect.DeepEqual(got, want) {
				t.Errorf("body = %v, want %v", got, want)
			}
		})
	}
}

func TestRouting_ACatchAllParameterTakesTheRestOfThePath(t *testing.T) {
	t.Parallel()
	var log callLog
	s := New(Options{})
	s.SetAuthenticator(log.authenticator(testCaller, nil))
	for _, pattern := range []string{"/w/:slug/refs", "/w/:slug/refs/*name", "/w/:slug/refs/heads/:branch"} {
		s.Register(Route{Method: http.MethodGet, Pattern: pattern, Handler: func(w http.ResponseWriter, r *http.Request) error {
			m, _ := MatchedRoute(r)
			WriteJSON(w, http.StatusOK, map[string]any{"route": m.Pattern, "params": m.Params})
			return nil
		}})
	}
	h := s.Handler()

	tests := []struct {
		path, wantRoute string
		wantParams      map[string]any
	}{
		{"/api/v1/w/p1/refs", "/w/:slug/refs", map[string]any{"slug": "p1"}},
		{"/api/v1/w/p1/refs/main", "/w/:slug/refs/*name", map[string]any{"slug": "p1", "name": "main"}},
		{"/api/v1/w/p1/refs/feature/x", "/w/:slug/refs/*name", map[string]any{"slug": "p1", "name": "feature/x"}},
		{"/api/v1/w/p1/refs/refs/heads/feature/x", "/w/:slug/refs/*name", map[string]any{"slug": "p1", "name": "refs/heads/feature/x"}},
		{"/api/v1/w/p1/refs/a%2Fb/c%20d", "/w/:slug/refs/*name", map[string]any{"slug": "p1", "name": "a/b/c d"}},
		{"/api/v1/w/p1/refs/heads/main", "/w/:slug/refs/heads/:branch", map[string]any{"slug": "p1", "branch": "main"}},
		{"/api/v1/w/p1/refs/heads/main/x", "/w/:slug/refs/*name", map[string]any{"slug": "p1", "name": "heads/main/x"}},
		{"/api/v1/w/p1/refs/", "", nil},
		{"/api/v1/w/p1/refs/a/", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rec := do(h, newRequest(http.MethodGet, tt.path, nil, ""))
			if tt.wantRoute == "" {
				assertError(t, rec, http.StatusNotFound, "not_found")
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.Bytes())
			}
			want := map[string]any{"route": tt.wantRoute, "params": tt.wantParams}
			if got := decodeObject(t, rec.Body.Bytes()); !reflect.DeepEqual(got, want) {
				t.Errorf("body = %v, want %v", got, want)
			}
		})
	}
}

func TestRouting_ACatchAllThatMayBeEmptyTakesTheSlashWhereItBegins(t *testing.T) {
	t.Parallel()
	var log callLog
	s := New(Options{})
	s.SetAuthenticator(log.authenticator(testCaller, nil))
	s.Register(Route{Method: http.MethodDelete, Pattern: "/w/:slug/notes/*spec", EmptyCatchAll: true, Handler: func(w http.ResponseWriter, r *http.Request) error {
		WriteJSON(w, http.StatusOK, map[string]any{"spec": Param(r, "spec")})
		return nil
	}})
	h := s.Handler()

	for path, want := range map[string]any{
		"/api/v1/w/p1/notes/":    "",
		"/api/v1/w/p1/notes/a/b": "a/b",
		"/api/v1/w/p1/notes/a/":  nil,
		"/api/v1/w/p1/notes":     nil,
	} {
		rec := do(h, newRequest(http.MethodDelete, path, nil, ""))
		if want == nil {
			if rec.Code != http.StatusNotFound {
				t.Errorf("DELETE %s = %d %s, want 404", path, rec.Code, rec.Body.Bytes())
			}
			continue
		}
		if rec.Code != http.StatusOK || !reflect.DeepEqual(decodeObject(t, rec.Body.Bytes()), map[string]any{"spec": want}) {
			t.Errorf("DELETE %s = %d %s, want 200 with the parameter %q", path, rec.Code, rec.Body.Bytes(), want)
		}
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
		{"catch-all parameter before the last segment", Route{Method: "GET", Pattern: "/things/*rest/x", Handler: ok}},
		{"catch-all parameter without a name", Route{Method: "GET", Pattern: "/things/*", Handler: ok}},
		{"catch-all parameter named like another", Route{Method: "GET", Pattern: "/things/:id/*id", Handler: ok}},
		{"catch-all route already registered", Route{Method: "GET", Pattern: "/taken/*other", Handler: ok}},
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
			s.Register(Route{Method: "GET", Pattern: "/taken/*rest", Handler: ok})
			defer func() {
				if recover() == nil {
					t.Errorf("Register(%s %q) did not panic", tt.route.Method, tt.route.Pattern)
				}
			}()
			s.Register(tt.route)
		})
	}
}

func TestMatchedPattern_NamesTheRouteThatServedTheRequest(t *testing.T) {
	t.Parallel()
	var got string
	s := New(Options{Wrap: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			got = MatchedPattern(r)
		})
	}})
	ok := func(w http.ResponseWriter, _ *http.Request) error {
		WriteJSON(w, http.StatusOK, map[string]string{})
		return nil
	}
	s.Register(Route{Method: http.MethodGet, Pattern: "/workspaces/:slug/sync", Public: true, Handler: ok})
	s.Register(Route{Method: http.MethodGet, Pattern: "/workspaces/:slug/refs/*name", Public: true, Handler: ok})
	s.Handle("POST /git/{org}/{repo}/git-upload-pack", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h := s.Handler()
	for _, tc := range []struct{ method, target, want string }{
		{http.MethodGet, "/api/v1/workspaces/a/sync", "GET /api/v1/workspaces/{slug}/sync"},
		{http.MethodGet, "/api/v1/workspaces/a/refs/heads/main", "GET /api/v1/workspaces/{slug}/refs/{name...}"},
		{http.MethodPost, "/git/o/r.git/git-upload-pack", "POST /git/{org}/{repo}/git-upload-pack"},
		{http.MethodGet, "/healthz", "GET /healthz"},
		{http.MethodDelete, "/api/v1/workspaces/a/sync", ""},
		{http.MethodPost, "/healthz", ""},
		{http.MethodGet, "/no/such/path", ""},
		{http.MethodGet, "/api/v1/workspaces//sync", ""},
	} {
		got = "unset"
		do(h, newRequest(tc.method, tc.target, nil, ""))
		if got != tc.want {
			t.Errorf("%s %s: MatchedPattern = %q, want %q", tc.method, tc.target, got, tc.want)
		}
	}
}
