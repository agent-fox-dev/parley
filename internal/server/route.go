package server

import (
	"context"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
)

// APIPrefix is the fixed mount point of the REST API. Route patterns are
// relative to it.
const APIPrefix = "/api/v1"

// HandlerFunc is the handler of a REST route. It answers through w; an error
// it returns, when it has written nothing, is answered as the error envelope
// (an *Error with its own status, any other error as 500 internal).
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

// Check is a check installed in one of the pipeline's positions or declared
// by a route. It reads the request's caller (CallerFrom) and matched route
// (MatchFrom) from the request's context; a non-nil error ends the request
// and is answered as its envelope.
type Check func(r *http.Request) error

// Route is a REST route and its declarations. Method and Pattern are
// required; Pattern is relative to APIPrefix and names path parameters with
// a colon ("/workspaces/:slug/tokens").
type Route struct {
	Method  string
	Pattern string
	Handler HandlerFunc

	// Public routes skip authentication, the binding guard, the scope check
	// and the access checks; the rate limit and the body checks still apply.
	Public bool
	// Create marks a POST that creates a resource: it accepts
	// Idempotency-Key.
	Create bool
	// ShowOnce names the members of a create's response body that are shown
	// once (a token value) and never replayed, as dotted paths
	// ("token", "token.token").
	ShowOnce []string
	// BodyLimit is the route's own request body limit in bytes; 0 means the
	// server's.
	BodyLimit int64
	// ContentType is the request content type the route accepts in place of
	// application/json.
	ContentType string
	// CacheControl is the route's own Cache-Control value in place of
	// no-store.
	CacheControl string
	// Feature is the route's feature check, run after the body checks and
	// before the binding guard.
	Feature Check
	// Scope is the one scope a personal access token needs for the route;
	// the scope check installed by its owner interprets it.
	Scope string
	// Marks are named declarations with a value that installed checks read
	// (an admin mark, the workspace access a route needs, …).
	Marks map[string]string
}

// Match is the route a request matched and its path parameter values.
type Match struct {
	Route  *Route
	Params map[string]string
}

type ctxKey int

const (
	keyMatch ctxKey = iota
	keyCaller
	keyRequestID
	keyBody
)

// MatchFrom returns the route a REST request matched, or nil outside the
// REST API.
func MatchFrom(ctx context.Context) *Match {
	m, _ := ctx.Value(keyMatch).(*Match)
	return m
}

// Param returns the value of the path parameter name of the request's
// matched route, or "" when there is none.
func Param(r *http.Request, name string) string {
	if m := MatchFrom(r.Context()); m != nil {
		return m.Params[name]
	}
	return ""
}

// compiledRoute is a registered route split into its path segments.
type compiledRoute struct {
	route    *Route
	segments []string // literal segments, or ":name"
	literals int
}

// Register adds a REST route under APIPrefix. Routes are registered before
// the server serves.
func (s *Server) Register(rt Route) {
	r := rt
	segs := strings.Split(strings.TrimPrefix(r.Pattern, "/"), "/")
	c := &compiledRoute{route: &r, segments: segs}
	for _, seg := range segs {
		if !strings.HasPrefix(seg, ":") {
			c.literals++
		}
	}
	s.routes = append(s.routes, c)
}

// match finds the route for method and the escaped path relative to
// APIPrefix. When no route has the method, allowed lists the methods
// registered for the path; both are empty for an unknown path.
func (s *Server) match(method, rel string) (*Match, []string) {
	if !strings.HasPrefix(rel, "/") {
		return nil, nil
	}
	segs := strings.Split(rel[1:], "/")
	var best *compiledRoute
	var bestParams map[string]string
	methods := map[string]bool{}
	for _, c := range s.routes {
		params, ok := c.matchSegments(segs)
		if !ok {
			continue
		}
		methods[c.route.Method] = true
		if c.route.Method == method && (best == nil || c.literals > best.literals) {
			best, bestParams = c, params
		}
	}
	if best != nil {
		return &Match{Route: best.route, Params: bestParams}, nil
	}
	allowed := make([]string, 0, len(methods))
	for m := range methods {
		allowed = append(allowed, m)
	}
	sort.Strings(allowed)
	return nil, allowed
}

func (c *compiledRoute) matchSegments(segs []string) (map[string]string, bool) {
	if len(segs) != len(c.segments) {
		return nil, false
	}
	params := map[string]string{}
	for i, want := range c.segments {
		got := segs[i]
		if name, ok := strings.CutPrefix(want, ":"); ok {
			v, err := url.PathUnescape(got)
			if got == "" || err != nil {
				return nil, false
			}
			params[name] = v
			continue
		}
		if got != want {
			return nil, false
		}
	}
	return params, true
}

// isAPIPath reports whether p is under the REST API's mount.
func isAPIPath(p string) bool { return p == APIPrefix || strings.HasPrefix(p, APIPrefix+"/") }

// serveAPI matches a REST request and runs its pipeline; an unknown path is
// 404 and a known path under another method 405, both before any check.
func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	m, allowed := s.match(r.Method, strings.TrimPrefix(r.URL.EscapedPath(), APIPrefix))
	if m == nil {
		writeUnmatched(w, allowed)
		return
	}
	s.runPipeline(w, r.WithContext(context.WithValue(r.Context(), keyMatch, m)), m)
}

func writeUnmatched(w http.ResponseWriter, allowed []string) {
	if len(allowed) > 0 {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		WriteError(w, MethodNotAllowed("method not allowed"))
		return
	}
	WriteError(w, NotFound("not found"))
}

// probeMethods are the methods tried to build the Allow header of another
// surface.
var probeMethods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}

// serveSurface serves a request outside the REST API through the router of
// Handle and HandleFunc, answering unknown paths and wrong methods with the
// envelope instead of the router's own answers, and never redirecting.
func (s *Server) serveSurface(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	clean := path.Clean(p)
	if strings.HasSuffix(p, "/") && clean != "/" {
		clean += "/"
	}
	if p == "" || clean != p {
		WriteError(w, NotFound("not found"))
		return
	}
	if h, pattern := s.mux.Handler(r); pattern != "" {
		h.ServeHTTP(w, r)
		return
	}
	var allowed []string
	for _, m := range probeMethods {
		probe := r.Clone(r.Context())
		probe.Method = m
		if _, pattern := s.mux.Handler(probe); pattern != "" {
			allowed = append(allowed, m)
		}
	}
	writeUnmatched(w, allowed)
}
