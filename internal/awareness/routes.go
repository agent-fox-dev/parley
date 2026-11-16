package awareness

import (
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
)

// Routes are the awareness routes, for the server's router: the overlaps
// of a change, the activity of a list of path globs, the claims' list and
// read, and the creation, which takes an Idempotency-Key, renewal and
// release of a claim. Every member of the workspace's organization may
// write claims; who may renew and release one, the routes decide.
//
// The patterns are those of the contributor table of internal/access,
// parameter names included, so that a token bound to a fork reaches them
// on the fork's parent. Such a token reads the overlaps of every change of
// the parent, not only of the fork's: the overlaps name the same changes
// the activity of the parent shows it.
func (c *Component) Routes() []server.Route {
	need := map[string]string{access.MarkNeed: string(access.NeedRead)}
	return []server.Route{
		{
			Method: http.MethodGet, Pattern: "/workspaces/:slug/changes/:number/overlaps", Scope: "changes:read",
			Marks: need, Handler: c.overlapsRoute,
		},
		{
			Method: http.MethodGet, Pattern: "/workspaces/:slug/activity", Scope: "changes:read",
			Marks: need, Handler: c.activityRoute,
		},
		{
			Method: http.MethodPost, Pattern: "/workspaces/:slug/claims", Scope: "changes:write",
			Marks: need, Create: true, Handler: c.createClaimRoute,
		},
		{
			Method: http.MethodGet, Pattern: "/workspaces/:slug/claims", Scope: "changes:read",
			Marks: need, Handler: c.listClaimsRoute,
		},
		{
			Method: http.MethodGet, Pattern: "/workspaces/:slug/claims/:id", Scope: "changes:read",
			Marks: need, Handler: c.readClaimRoute,
		},
		{
			Method: http.MethodPatch, Pattern: "/workspaces/:slug/claims/:id", Scope: "changes:write",
			Marks: need, Handler: c.renewClaimRoute,
		},
		{
			Method: http.MethodDelete, Pattern: "/workspaces/:slug/claims/:id", Scope: "changes:write",
			Marks: need, Handler: c.releaseClaimRoute,
		},
	}
}
