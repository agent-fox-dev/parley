package carrypatch

import (
	"maps"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
)

// RouteKind tells a carry-patch route that reads the patch list from one
// that writes it.
type RouteKind int

// The kinds of route. A read route needs patches:read and read access to
// the workspace, and serves in every state of the workspace; a write route
// needs patches:write and write access, and a workspace that is ready.
const (
	ReadRoute RouteKind = iota
	WriteRoute
)

// The scopes of the carry-patch routes.
const (
	scopeRead  = "patches:read"
	scopeWrite = "patches:write"
)

// routeKindKey is the key under which a route declared with Route leaves
// its kind on the request, for Guard.
type routeKindKey struct{}

// Route declares the carry-patch route rt, whose pattern names its
// workspace with :slug, as one of kind: it sets the scope a PAT needs and
// the workspace need of kind, replacing those rt gives, so that the
// pipeline runs authentication, the scope check and workspace access before
// the handler; and it leaves kind on the request, where Guard reads it. The
// hub registers the result with its router, as for every route.
func Route(kind RouteKind, rt server.Route) server.Route {
	scope, need := scopeRead, access.NeedRead
	if kind == WriteRoute {
		scope, need = scopeWrite, access.NeedWrite
	}
	rt.Scope = scope
	// A copy, so that the caller's map is not changed.
	rt.Marks = maps.Clone(rt.Marks)
	if rt.Marks == nil {
		rt.Marks = make(map[string]string, 1)
	}
	rt.Marks[access.MarkNeed] = string(need)
	next := rt.Handler
	rt.Handler = func(w http.ResponseWriter, r *http.Request) error {
		server.Attach(r, routeKindKey{}, kind)
		return next(w, r)
	}
	return rt
}
