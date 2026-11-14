// Package gitserver serves the hub's workspaces to stock git over smart
// HTTP, on the port of the REST API but outside it: three routes under
// /git, which the hub mounts on the server's router with Component.Mount, so
// that they pass none of the REST API's checks, its body limit, its content
// type rule or its JSON error envelope.
//
// Every git request passes the component's checks in one fixed order before
// the Service runs, and the first check that refuses answers the request
// with an HTTP status and one line of plain text. git shows that line to its
// user, and it asks for a credential only on a 401 that carries the Basic
// challenge, so a refusal never has a JSON body.
package gitserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The patterns of the three git routes on the server's router. Every other
// path or method under /git gets the server's general 404 and 405 answers,
// before any credential is read.
const (
	PatternInfoRefs    = "GET /git/{org}/{repo}/info/refs"
	PatternUploadPack  = "POST /git/{org}/{repo}/git-upload-pack"
	PatternReceivePack = "POST /git/{org}/{repo}/git-receive-pack"
)

// The git services: a fetch is served by git-upload-pack, a push by
// git-receive-pack.
const (
	ServiceUploadPack  = "git-upload-pack"
	ServiceReceivePack = "git-receive-pack"
)

// Route is the git route a request came in on.
type Route int

// The routes: ref discovery, the fetch exchange and the push exchange.
const (
	RouteInfoRefs Route = iota + 1
	RouteUploadPack
	RouteReceivePack
)

// routes are the three routes with their patterns, in the order Mount
// registers them.
var routes = []struct {
	pattern string
	route   Route
}{
	{PatternInfoRefs, RouteInfoRefs},
	{PatternUploadPack, RouteUploadPack},
	{PatternReceivePack, RouteReceivePack},
}

// Credentials checks the credential a request presents, as it checks the
// bearer credential of a REST request: *identity.Service is one. It returns
// the credential's principal, or a *server.Error of status 401 for a
// credential that does not pass and 403 for one whose user is blocked; any
// other error is the store's.
type Credentials interface {
	VerifyCredential(ctx context.Context, credential string) (identity.Principal, error)
}

// Limiter counts one request against a key and reports whether it is
// admitted and, when it is not, the whole seconds until it would be:
// *server.Limiter is one.
type Limiter interface {
	Allow(key string) (ok bool, retryAfter int)
}

// Exchange is what the checks found out about a request that passed them,
// handed to the Service.
type Exchange struct {
	// Route is the route the request came in on.
	Route Route
	// Service is the git service the request asks for: the route's on the
	// two POST routes, and on ref discovery the first value of the query
	// parameter service, as the client sent it.
	Service string
	// Caller is the caller authentication found, with Principal as its
	// Detail, the form the access rule and the binding guard read.
	Caller server.Caller
	// Principal is the credential the caller presented.
	Principal identity.Principal
	// Need is what the request needs of the caller on the workspace, decided
	// by the route alone before anything is looked up.
	Need access.Need
	// Level is the caller's level on the workspace, as the access rule
	// decided it.
	Level access.Level
	// Binding is what the binding guard found a bound PAT reaches; nil for
	// every other credential.
	Binding *access.Binding
	// Workspace is the record of the workspace the request addresses, as the
	// resolution of the repository read it; it is the zero Workspace on a
	// request no check has resolved.
	Workspace workspace.Workspace
}

// Service serves a git request that passed every check. It writes the
// whole answer.
type Service interface {
	Serve(w http.ResponseWriter, r *http.Request, x Exchange)
}

// Deps are what a Component works with. A collaborator that is nil never
// lets a request through that it would have refused.
type Deps struct {
	// Credentials checks the credential of every request; with none, every
	// request is challenged as one without a credential.
	Credentials Credentials
	// Limiter is the rate limit, shared with the REST API; with none, every
	// request is admitted.
	Limiter Limiter
	// Guard is the binding guard on git; with none, every bound PAT is
	// answered as for a repository that does not exist.
	Guard Guard
	// Access is the workspace access rule; with none, a request that reaches
	// the resolution of its repository is answered 500 internal error.
	Access Access
	// Records reads the organization and the workspace record a request
	// names; with none, as with no Access.
	Records Records
	// Service serves a request that passed the checks; with none, such a
	// request is answered 500 internal error.
	Service Service
	// Logger receives the causes of 500 answers; nil discards them.
	Logger *slog.Logger
}

// Component is the git server. The zero Component is ready to be mounted;
// Bind gives it its dependencies before it serves the first request.
type Component struct {
	deps Deps
}

// Bind sets the component's dependencies. The hub calls it once, from a
// start hook, before it serves any request.
func (c *Component) Bind(d Deps) {
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	c.deps = d
}

// Mount registers the three git routes with handle, the server's Handle or
// the hub composition's.
func (c *Component) Mount(handle func(pattern string, h http.Handler)) {
	for _, rt := range routes {
		handle(rt.pattern, c.handler(rt.route))
	}
}

// check is one of the checks a git request passes. It answers the request
// and reports false when the request does not pass, or adds what it found
// to x and reports true.
type check func(c *Component, w http.ResponseWriter, r *http.Request, x *Exchange) bool

// checks are the checks of every git request, in the order the hub answers
// them, so that the first that fails decides the answer: authentication;
// the rate limit, which counts the request against the credential
// authentication found, so that a caller over its limit learns nothing
// about the repository it asked for; the binding guard; the scope of a PAT;
// the resolution of the repository with the access rule; and the
// validation of the request, which a caller who cannot see the repository
// never reaches.
var checks = []check{
	(*Component).authenticate,
	(*Component).rateLimit,
	(*Component).guard,
	(*Component).scope,
	(*Component).resolve,
	(*Component).validate,
}

// handler serves route: it runs the checks in order, stops at the first
// that refuses, and hands a request that passed them all to the Service.
func (c *Component) handler(route Route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x := Exchange{Route: route, Service: serviceOf(route, r)}
		x.Need = needOf(route, x.Service)
		for _, chk := range checks {
			if !chk(c, w, r, &x) {
				return
			}
		}
		if c.deps.Service == nil {
			c.fail(w, r, errors.New("no git service is installed"))
			return
		}
		c.deps.Service.Serve(w, r, x)
	})
}

// serviceOf is the git service a request on route asks for.
func serviceOf(route Route, r *http.Request) string {
	switch route {
	case RouteUploadPack:
		return ServiceUploadPack
	case RouteReceivePack:
		return ServiceReceivePack
	default:
		return r.URL.Query().Get("service")
	}
}

// fail answers a request 500 internal error and logs err, the cause, with
// the request id the answer carries, the route's pattern and the
// repository segment of the path.
func (c *Component) fail(w http.ResponseWriter, r *http.Request, err error) {
	c.deps.Logger.LogAttrs(r.Context(), slog.LevelError, "git request failed",
		slog.String("request_id", server.RequestID(r.Context())),
		slog.String("route", r.Pattern),
		slog.String("repository", r.PathValue("repo")),
		slog.String("error", err.Error()),
	)
	refuse(w, http.StatusInternalServerError, msgInternal)
}
