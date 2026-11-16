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

// Routes are the carry-patch routes, each declared with Route, which the
// hub registers with its router: the patch list and a single patch, read
// routes that serve in every state of the workspace, and the add, a write
// route that creates, so that the server answers a repeated
// Idempotency-Key with the first answer, and the write routes that
// maintain the list.
func (c *Component) Routes() []server.Route {
	return []server.Route{
		Route(ReadRoute, server.Route{Method: http.MethodGet, Pattern: "/workspaces/:slug/patches", Handler: c.listRoute}),
		Route(ReadRoute, server.Route{Method: http.MethodGet, Pattern: "/workspaces/:slug/patches/:id", Handler: c.getRoute}),
		Route(WriteRoute, server.Route{Method: http.MethodPost, Pattern: "/workspaces/:slug/patches", Handler: c.addRoute, Create: true}),
		Route(WriteRoute, server.Route{Method: http.MethodPatch, Pattern: "/workspaces/:slug/patches/:id", Handler: c.updateRoute}),
		Route(WriteRoute, server.Route{Method: http.MethodDelete, Pattern: "/workspaces/:slug/patches/:id", Handler: c.deleteRoute}),
		Route(WriteRoute, server.Route{Method: http.MethodPost, Pattern: "/workspaces/:slug/patches/:id/restore", Handler: c.restoreRoute}),
		// The router prefers a literal segment to a parameter, so reorder is
		// never taken for the id of a patch.
		Route(WriteRoute, server.Route{Method: http.MethodPost, Pattern: "/workspaces/:slug/patches/reorder", Handler: c.reorderRoute}),
	}
}
