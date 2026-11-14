package access

import (
	"net/http"
	"slices"
)

// ContributorRoute is a route a PAT bound to a task fork reaches on the
// fork's parent: its method and its pattern relative to /workspaces/:slug,
// with the parameter names a route must be registered with for the binding
// guard to recognise it.
type ContributorRoute struct {
	Method, Pattern string
}

// contributorRoutes are the routes on which an agent working in a task fork
// opens, follows and lands its own changes in the parent and reads what
// others do there. Each route confines what it does to the fork's changes
// itself; the guard only lets the request through, with the fork in the
// Binding.
var contributorRoutes = []ContributorRoute{
	{http.MethodPost, "/changes"},
	{http.MethodGet, "/changes"},
	{http.MethodGet, "/changes/:number"},
	{http.MethodGet, "/changes/by-commit/:sha"},
	{http.MethodPatch, "/changes/:number"},
	{http.MethodPost, "/changes/:number/abandon"},
	{http.MethodPost, "/changes/:number/restore"},
	{http.MethodPost, "/changes/:number/restack"},
	{http.MethodGet, "/changes/:number/versions"},
	{http.MethodGet, "/changes/:number/versions/:version"},
	{http.MethodGet, "/changes/:number/diff"},
	{http.MethodGet, "/changes/:number/stack"},
	{http.MethodPost, "/changes/:number/land"},
	{http.MethodGet, "/changes/:number/lands"},
	{http.MethodGet, "/lands/:id"},
	{http.MethodGet, "/changes/:number/conflict"},
	{http.MethodGet, "/changes/:number/conflict/file"},
	{http.MethodGet, "/changes/:number/approvals"},
	{http.MethodGet, "/changes/:number/checks"},
	{http.MethodGet, "/policy"},
	{http.MethodPost, "/policy/evaluate"},
	{http.MethodGet, "/activity"},
	{http.MethodGet, "/changes/:number/overlaps"},
	{http.MethodGet, "/claims"},
	{http.MethodGet, "/claims/:id"},
}

// ContributorRoutes returns a copy of the fixed table of contributor
// routes.
func ContributorRoutes() []ContributorRoute {
	return slices.Clone(contributorRoutes)
}

// isContributorRoute reports whether method and pattern, relative to
// /workspaces/:slug, are in the contributor table.
func isContributorRoute(method, pattern string) bool {
	return slices.Contains(contributorRoutes, ContributorRoute{method, pattern})
}
