package session

import (
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
)

// Routes are the session routes, for the server's router. The patterns of
// the /sessions routes are those of the binding guard's allowlist in
// internal/access, so that a bound token reaches them; each confines such
// a token to the workspaces its binding covers itself. The cost report is
// a workspace route: the pipeline's access check decides read access to
// its workspace, in any status, and the binding guard confines a bound
// token.
func (c *Component) Routes() []server.Route {
	needRead := map[string]string{access.MarkNeed: string(access.NeedRead)}
	return []server.Route{
		{Method: http.MethodPost, Pattern: "/sessions", Scope: "sessions:write", Create: true, Handler: c.createRoute},
		{Method: http.MethodGet, Pattern: "/sessions", Scope: "sessions:read", Handler: c.listRoute},
		{Method: http.MethodGet, Pattern: "/sessions/:id", Scope: "sessions:read", Handler: c.readRoute},
		{Method: http.MethodPost, Pattern: "/sessions/:id/complete", Scope: "sessions:write", Handler: c.completeRoute},
		{Method: http.MethodPost, Pattern: "/sessions/:id/usage", Scope: "sessions:write", Create: true, Handler: c.reportRoute},
		{Method: http.MethodGet, Pattern: "/sessions/:id/usage", Scope: "sessions:read", Handler: c.usageRoute},
		{Method: http.MethodGet, Pattern: "/workspaces/:slug/cost", Scope: "sessions:read", Marks: needRead, Handler: c.costRoute},
	}
}
