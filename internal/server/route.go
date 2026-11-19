package server

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
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
	// :name segments for path parameters, such as /workspaces/:slug, and
	// optionally a last segment *name, a catch-all parameter that takes one
	// or more remaining segments, such as /refs/*name for refs/heads/main.
	Pattern string
	// EmptyCatchAll lets the pattern's catch-all parameter take an empty
	// remainder too: the path that ends in a slash where the parameter
	// begins matches, with the parameter "", so that the handler answers
	// it, as it answers a remainder it refuses. A remainder that is not
	// empty still takes segments that are not empty.
	EmptyCatchAll bool
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
	// StreamBody leaves the request body to the handler: the body stage
	// reads none of it and checks neither its size nor its content type, so
	// that the handler can stream a body larger than any limit known when
	// the route is declared and enforce its own limit and media types.
	// MaxBodySize and ContentType are then not read.
	StreamBody bool
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
// is invalid or already registered, since both are programming errors. A
// route that differs from a registered one only in its parameter names is
// already registered.
func (s *Server) Register(rt Route) {
	segments, err := parsePattern(rt.Pattern)
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

	s.routesMu.Lock()
	defer s.routesMu.Unlock()
	for _, other := range s.routes {
		if other.route.Method == rt.Method && specificity(other.segments, segments) == 0 {
			panic(fmt.Sprintf("server.Register: %s %s is already registered", rt.Method, rt.Pattern))
		}
	}
	s.routes = append(s.routes, apiRoute{route: rt, segments: segments, muxPattern: muxPattern(rt.Method, segments)})
}

// apiRoute is a registered REST API route, its pattern split into segments,
// a parameter as :name, and its pattern in the syntax of http.ServeMux
// (MatchedPattern).
type apiRoute struct {
	route      Route
	segments   []string
	muxPattern string
}

// matches reports whether the route's pattern matches the unescaped path
// segments: every literal segment equal, every parameter a segment that is
// not empty, and a catch-all parameter one or more segments that are not
// empty, or, for a route with EmptyCatchAll, one empty segment.
func (a *apiRoute) matches(path []string) bool {
	n := len(a.segments)
	if catchAll := n > 0 && isCatchAll(a.segments[n-1]); catchAll && len(path) < n || !catchAll && len(path) != n {
		return false
	}
	emptyRest := a.route.EmptyCatchAll && len(path) == n && path[n-1] == ""
	for i, p := range path {
		seg := a.segments[min(i, n-1)]
		if isCatchAll(seg) && emptyRest {
			continue
		}
		if isParam(seg) || isCatchAll(seg) {
			if p == "" {
				return false
			}
		} else if seg != p {
			return false
		}
	}
	return true
}

// specificity compares two patterns: it is positive when a is more
// specific than b, negative when b is, and 0 when they have the same
// segments up to their parameter names. The first segment where they differ
// in kind decides, a literal winning over a parameter and a parameter over
// a catch-all, so that /changes/by-commit/:sha and /changes/:number/stack
// can both be registered and /changes/by-commit/stack goes to the first, and
// /refs/heads/:branch takes /refs/heads/main from /refs/*name. When one
// pattern is the beginning of the other, the longer is the more specific.
func specificity(a, b []string) int {
	for i := range min(len(a), len(b)) {
		if c := segmentRank(a[i]) - segmentRank(b[i]); c != 0 {
			return c
		}
		if !isParam(a[i]) && !isCatchAll(a[i]) && a[i] != b[i] {
			return strings.Compare(a[i], b[i])
		}
	}
	return len(a) - len(b)
}

// segmentRank orders the kinds of pattern segment from the least specific:
// a catch-all, a parameter, a literal.
func segmentRank(seg string) int {
	switch {
	case isCatchAll(seg):
		return 0
	case isParam(seg):
		return 1
	}
	return 2
}

func isParam(seg string) bool { return strings.HasPrefix(seg, ":") }

func isCatchAll(seg string) bool { return strings.HasPrefix(seg, "*") }

// match returns the REST API route that serves method on the path below
// /api/v1, given as its unescaped segments: of the routes whose pattern
// matches the path and that are registered for method (or for GET when
// method is HEAD), the most specific, and of two with the same pattern the
// one registered for method itself. When none serves it, match returns the
// methods the routes matching the path are registered for, sorted.
func (s *Server) match(method string, path []string) (*apiRoute, []string) {
	s.routesMu.RLock()
	defer s.routesMu.RUnlock()
	var best *apiRoute
	var allow []string
	for i := range s.routes {
		a := &s.routes[i]
		if !a.matches(path) {
			continue
		}
		m := a.route.Method
		if m != method && (method != http.MethodHead || m != http.MethodGet) {
			if !slices.Contains(allow, m) {
				allow = append(allow, m)
			}
			continue
		}
		if best == nil {
			best = a
			continue
		}
		c := specificity(a.segments, best.segments)
		if c > 0 || c == 0 && m == method {
			best = a
		}
	}
	slices.Sort(allow)
	return best, allow
}

// parsePattern checks a route pattern and returns its segments.
func parsePattern(p string) (segments []string, err error) {
	if !strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return nil, fmt.Errorf("pattern must start with a slash and not end with one")
	}
	segments = strings.Split(p[1:], "/")
	var names []string
	for i, seg := range segments {
		switch {
		case seg == "" || seg == "." || seg == "..":
			return nil, fmt.Errorf("empty or relative segment %q", seg)
		case strings.ContainsAny(seg, "{}"):
			return nil, fmt.Errorf("segment %q holds a brace; use :name for a parameter", seg)
		case isCatchAll(seg) && i != len(segments)-1:
			return nil, fmt.Errorf("catch-all parameter %q is not the last segment", seg)
		case isParam(seg) || isCatchAll(seg):
			name := seg[1:]
			if !validParamName(name) {
				return nil, fmt.Errorf("parameter %q is not a valid name", seg)
			}
			if slices.Contains(names, name) {
				return nil, fmt.Errorf("parameter %q appears twice", seg)
			}
			names = append(names, name)
		case strings.Contains(seg, ":"):
			return nil, fmt.Errorf("segment %q holds a colon after its start", seg)
		}
	}
	return segments, nil
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

// serveRoute records the route a matched, its parameter values taken from
// the unescaped path segments (a catch-all's the remaining segments joined
// by slashes), and an empty holder for the checks' attachments in the
// request's context and runs the pipeline.
func (s *Server) serveRoute(w http.ResponseWriter, r *http.Request, a *apiRoute, path []string) {
	params := make(map[string]string)
	for i, seg := range a.segments {
		switch {
		case isParam(seg):
			params[seg[1:]] = path[i]
		case isCatchAll(seg):
			params[seg[1:]] = strings.Join(path[i:], "/")
		}
	}
	m := Matched{Method: a.route.Method, Pattern: a.route.Pattern, Route: a.route, Params: params}
	notePattern(r, a.muxPattern)
	ctx := context.WithValue(r.Context(), matchedKey{}, m)
	ctx = context.WithValue(ctx, attachedKey{}, &attachments{})
	s.runPipeline(w, r.WithContext(ctx), a.route)
}

// apiPath returns the unescaped segments of the escaped path p below
// /api/v1, and false when p is not below it. A segment that does not
// unescape is kept as it is.
func apiPath(p string) ([]string, bool) {
	rest, ok := strings.CutPrefix(p, apiMount+"/")
	if !ok {
		return nil, false
	}
	segs := strings.Split(rest, "/")
	for i, seg := range segs {
		if u, err := url.PathUnescape(seg); err == nil {
			segs[i] = u
		}
	}
	return segs, true
}

// dispatch is the entry of every request. It answers a path that is not
// clean with the error envelope, serves a REST API route it matches itself
// (the standard router refuses to hold /changes/by-commit/:sha beside
// /changes/:number/stack), and hands every other path to the router. The
// router alone would redirect a path that lacks a trailing slash and answer
// an unknown path or a wrong method in plain text; dispatch answers those
// with the error envelope instead, before any check runs.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	p := r.URL.EscapedPath()
	if p == "" || p[0] != '/' || cleanPath(p) != p {
		WriteError(w, NotFound("not found"))
		return
	}
	if path, ok := apiPath(p); ok {
		a, allow := s.match(r.Method, path)
		if a != nil {
			s.serveRoute(w, r, a, path)
			return
		}
		if len(allow) > 0 {
			w.Header().Set("Allow", strings.Join(allow, ", "))
			WriteError(w, MethodNotAllowed("method not allowed"))
			return
		}
	}
	h, pattern := s.mux.Handler(r)
	if pattern != "" && !redirects(p, pattern) {
		notePattern(r, pattern)
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

type attachedKey struct{}

// attachments are the values the checks of one request attached for the
// checks after them and the handler. A check returns only an error, so it
// cannot hand on a context of its own; the route's handler puts this holder
// in the request's context before the pipeline runs instead (ADR 04).
type attachments struct {
	mu     sync.Mutex
	values map[any]any
}

// Attach leaves value under key on the request r, for the later checks and
// the handler of the same request, which read it with Attached. A check
// uses it to hand on what it found, such as the access it granted. Outside
// a request to a REST route it does nothing. Like a context key, key should
// be of an unexported type of the package that attaches.
func Attach(r *http.Request, key, value any) {
	a, ok := r.Context().Value(attachedKey{}).(*attachments)
	if !ok {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.values == nil {
		a.values = make(map[any]any)
	}
	a.values[key] = value
}

// Attached returns the value a check of the request r attached under key.
// It reports false when none did, and outside a request to a REST route.
func Attached(r *http.Request, key any) (any, bool) {
	a, ok := r.Context().Value(attachedKey{}).(*attachments)
	if !ok {
		return nil, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	v, ok := a.values[key]
	return v, ok
}

type patternKey struct{}

// patternSlot is where dispatch leaves the pattern it matched. The layers
// inside the server serve copies of the request, so a wrapper outside them
// reads it from a slot that its own request carries too.
type patternSlot struct{ pattern string }

// withPatternSlot gives every request an empty pattern slot before next
// serves it.
func withPatternSlot(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), patternKey{}, &patternSlot{})))
	})
}

// notePattern records pattern as the one that matched r.
func notePattern(r *http.Request, pattern string) {
	if slot, ok := r.Context().Value(patternKey{}).(*patternSlot); ok {
		slot.pattern = pattern
	}
}

// MatchedPattern returns the pattern of the route that served r, a request
// the server's handler was given, in the syntax of http.ServeMux, as
// Request.Pattern holds it: the method and a path. A REST API route's path
// is /api/v1 and its pattern, each :name segment written {name} and a
// catch-all *name written {name...}; a surface mounted with Handle has the
// pattern it was mounted with. It is "" when no route matched: an unknown
// path, a wrong method, or a path that is not clean. A wrapper set with
// Options.Wrap reads it once the request has been served.
func MatchedPattern(r *http.Request) string {
	slot, _ := r.Context().Value(patternKey{}).(*patternSlot)
	if slot == nil {
		return ""
	}
	return slot.pattern
}

// muxPattern writes a REST API route's method and pattern segments in the
// syntax of http.ServeMux.
func muxPattern(method string, segments []string) string {
	var b strings.Builder
	b.WriteString(method + " " + apiMount)
	for _, seg := range segments {
		switch {
		case isParam(seg):
			seg = "{" + seg[1:] + "}"
		case isCatchAll(seg):
			seg = "{" + seg[1:] + "...}"
		}
		b.WriteString("/" + seg)
	}
	return b.String()
}
