package session

import (
	"net/http"

	"github.com/agent-fox-dev/parley/internal/server"
)

// Routes are the session routes, for the server's router. Their patterns
// are those of the binding guard's allowlist in internal/access, so that a
// bound token reaches them; each confines such a token to the workspaces
// its binding covers itself.
func (c *Component) Routes() []server.Route {
	return []server.Route{
		{Method: http.MethodPost, Pattern: "/sessions", Scope: "sessions:write", Create: true, Handler: c.createRoute},
	}
}
