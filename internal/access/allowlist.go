package access

import (
	"net/http"
	"slices"
)

// allowedRoute is a route that names no workspace and that a PAT bound to a
// workspace still reaches: its method and its pattern relative to /api/v1,
// with the parameter names a route must be registered with for the binding
// guard to recognise it.
type allowedRoute struct {
	method, pattern string
}

// allowlist are the cross-workspace routes an agent needs inside its
// workspace: the workspace list, the event stream and log, the audit log,
// the sandbox profiles and the session routes. Each confines what it
// returns to the binding's covered set itself (CoveredSet); the guard only
// lets the request through, with the Binding.
var allowlist = []allowedRoute{
	{http.MethodGet, "/workspaces"},
	{http.MethodGet, "/events"},
	{http.MethodGet, "/events/log"},
	{http.MethodGet, "/audit"},
	{http.MethodGet, "/sandbox-profiles"},
	{http.MethodPost, "/sessions"},
	{http.MethodGet, "/sessions"},
	{http.MethodGet, "/sessions/:id"},
	{http.MethodPost, "/sessions/:id/complete"},
	{http.MethodPost, "/sessions/:id/usage"},
	{http.MethodGet, "/sessions/:id/usage"},
}

// isAllowlisted reports whether method and pattern, relative to /api/v1,
// are in the allowlist.
func isAllowlisted(method, pattern string) bool {
	return slices.Contains(allowlist, allowedRoute{method, pattern})
}
