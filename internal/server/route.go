package server

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"path"
	"slices"
	"strings"
)

// apiMount is the fixed path every REST API route is registered under.
const apiMount = "/api/v1"

// HandlerFunc handles a request to a REST API route. It writes the response
// itself when it succeeds; an error it returns ends the request and is
// answered as the error envelope.
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

// Route is a REST API route and its declarations. The checks of the pipeline
// and the handler read the declarations of the route they serve through
// MatchedRoute.
type Route struct {
	// Method is the HTTP method, such as GET or POST. A GET route also
	// answers HEAD, without a body.
	Method string
	// Pattern is the path relative to /api/v1, starting with a slash, with
	// :name segments for path parameters, such as /workspaces/:slug.
	Pattern string
	// Handler serves the request once every check has passed.
	Handler HandlerFunc

	// Public routes need no credential: the pipeline skips authentication,
	// the feature check, the binding guard, the scope check and the access
	// checks for them.
	Public bool
	// Create marks a POST that creates a resource and so accepts an
	// Idempotency-Key.
	Create bool
	// ShowOnce names the members of a create route's response, by path such
	// as token or token.token, that are shown once and never stored.
	ShowOnce []string
	// MaxBodySize is the route's own body limit in bytes; 0 means the
	// server's.
	MaxBodySize int64
	// ContentType is the route's own request media type; empty means
	// application/json.
	ContentType string
	// CacheControl is the route's own Cache-Control value; empty means
	// no-store.
	CacheControl string
	// Feature is the route's own feature check, run after the body checks;
	// nil means none.
	Feature Check
	// Scope is the one scope a personal access token needs for the route. The
	// server does not interpret it; the installed scope check does.
	Scope string
	// Marks are named declarations whose meaning the installed checks
	// define, such as the access a workspace route needs.
	Marks map[string]string
}

// Matched is the route a request matched: the method and the pattern as
// registered, the route's declarations and the values of its path
// parameters.
type Matched struct {
	Method  string
	Pattern string
	Route   Route
	Params  map[string]string
}

type matchedKey struct{}

// MatchedRoute returns the REST API route r matched. It reports false for a
// request that did not come through a registered route.
func MatchedRoute(r *http.Request) (Matched, bool) {
	m, ok := r.Context().Value(matchedKey{}).(Matched)
	return m, ok
}

// Param returns the value of the path parameter name of the route r
// matched, or "" when the route has no such parameter.
func Param(r *http.Request, name string) string {
	m, _ := MatchedRoute(r)
	return m.Params[name]
}

// Register adds rt to the REST API under /api/v1. It panics when the route
// is invalid or already registered, since both are programming errors.
func (s *Server) Register(rt Route) {
	names, muxPath, err := parsePattern(rt.Pattern)
	if err != nil {
		panic(fmt.Sprintf("server.Register: %s %s: %v", rt.Method, rt.Pattern, err))
	}
	if !validMethod(rt.Method) {
		panic(fmt.Sprintf("server.Register: %q is not an HTTP method", rt.Method))
	}
	if rt.Handler == nil {
		panic(fmt.Sprintf("server.Register: %s %s has no handler", rt.Method, rt.Pattern))
	}
	// The caller keeps its slice and map; later changes to them must not
	// change the registered route.
	rt.ShowOnce = slices.Clone(rt.ShowOnce)
	rt.Marks = maps.Clone(rt.Marks)

	key := rt.Method + " " + muxPath
	s.routesMu.Lock()
	defer s.routesMu.Unlock()
	if _, dup := s.routes[key]; dup {
		panic(fmt.Sprintf("server.Register: %s %s is already registered", rt.Method, rt.Pattern))
	}
	s.routes[key] = struct{}{}
	s.mux.Handle(key, s.serveRoute(rt, names))
}

// parsePattern checks a route pattern and returns its parameter names and
// the full path in the router's syntax, with :name turned into {name}.
func parsePattern(p string) (names []string, muxPath string, err error) {
	if !strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return nil, "", fmt.Errorf("pattern must start with a slash and not end with one")
	}
	segments := strings.Split(p[1:], "/")
	for i, seg := range segments {
		switch {
		case seg == "" || seg == "." || seg == "..":
			return nil, "", fmt.Errorf("empty or relative segment %q", seg)
		case strings.ContainsAny(seg, "{}"):
			return nil, "", fmt.Errorf("segment %q holds a brace; use :name for a parameter", seg)
		case strings.HasPrefix(seg, ":"):
			name := seg[1:]
			if !validParamName(name) {
				return nil, "", fmt.Errorf("parameter %q is not a valid name", seg)
			}
			if slices.Contains(names, name) {
				return nil, "", fmt.Errorf("parameter %q appears twice", seg)
			}
			names = append(names, name)
			segments[i] = "{" + name + "}"
		case strings.Contains(seg, ":"):
			return nil, "", fmt.Errorf("segment %q holds a colon after its start", seg)
		}
	}
	return names, apiMount + "/" + strings.Join(segments, "/"), nil
}

func validParamName(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		letter := c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
		if !letter && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func validMethod(m string) bool {
	if m == "" {
		return false
	}
	for _, c := range m {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

// serveRoute returns the router's handler for rt: it records the matched
// route in the request's context and runs the pipeline.
func (s *Server) serveRoute(rt Route, names []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		params := make(map[string]string, len(names))
		for _, name := range names {
			params[name] = r.PathValue(name)
		}
		m := Matched{Method: rt.Method, Pattern: rt.Pattern, Route: rt, Params: params}
		r = r.WithContext(context.WithValue(r.Context(), matchedKey{}, m))
		s.runPipeline(w, r, rt)
	})
}

// dispatch is the entry of every request. The router alone would redirect a
// path that is not clean or lacks a trailing slash and answer an unknown
// path or a wrong method in plain text; dispatch answers those with the error
// envelope instead, before any check runs, and hands everything else to the
// router.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	p := r.URL.EscapedPath()
	if p == "" || p[0] != '/' || cleanPath(p) != p {
		WriteError(w, NotFound("not found"))
		return
	}
	h, pattern := s.mux.Handler(r)
	if pattern != "" && !redirects(p, pattern) {
		s.mux.ServeHTTP(w, r)
		return
	}
	if pattern == "" {
		// The router's own answer tells a wrong method (405 with Allow) from
		// an unknown path; it is cheap and has no side effects.
		var probe headerRecorder
		h.ServeHTTP(&probe, r)
		if probe.status == http.StatusMethodNotAllowed {
			w.Header().Set("Allow", s.allowed(r, probe.Header().Get("Allow")))
			WriteError(w, MethodNotAllowed("method not allowed"))
			return
		}
	}
	WriteError(w, NotFound("not found"))
}

// allowed removes HEAD from the router's Allow list when only a GET route
// brings it, since the router adds HEAD for every GET pattern by itself.
func (s *Server) allowed(r *http.Request, allow string) string {
	methods := strings.Split(allow, ", ")
	if !slices.Contains(methods, http.MethodHead) {
		return allow
	}
	head := r.Clone(r.Context())
	head.Method = http.MethodHead
	if _, pattern := s.mux.Handler(head); strings.HasPrefix(pattern, http.MethodHead+" ") {
		return allow
	}
	methods = slices.DeleteFunc(methods, func(m string) bool { return m == http.MethodHead })
	return strings.Join(methods, ", ")
}

// redirects reports whether the router answers the clean path p, matched to
// pattern, with a redirect to p plus a trailing slash rather than with the
// pattern's handler. That happens only when the pattern's path ends in a
// slash (or /{$}) that p lacks, and p has one segment fewer than the pattern:
// a real match of such a pattern has at least as many segments as it has.
func redirects(p, pattern string) bool {
	if strings.HasSuffix(p, "/") {
		return false
	}
	pp := pattern[strings.Index(pattern, "/"):]
	if strings.HasSuffix(pp, "/{$}") {
		return true
	}
	return strings.HasSuffix(pp, "/") && strings.Count(pp, "/") == strings.Count(p, "/")+1
}

// cleanPath is the router's own canonical form of a path: a path that
// differs from it would be redirected.
func cleanPath(p string) string {
	np := path.Clean(p)
	if strings.HasSuffix(p, "/") && np != "/" {
		np += "/"
	}
	return np
}

// headerRecorder captures the status and headers a handler writes and drops
// its body.
type headerRecorder struct {
	header http.Header
	status int
}

func (h *headerRecorder) Header() http.Header {
	if h.header == nil {
		h.header = make(http.Header)
	}
	return h.header
}

func (h *headerRecorder) WriteHeader(status int) {
	if h.status == 0 {
		h.status = status
	}
}

func (h *headerRecorder) Write(b []byte) (int, error) {
	h.WriteHeader(http.StatusOK)
	return len(b), nil
}
