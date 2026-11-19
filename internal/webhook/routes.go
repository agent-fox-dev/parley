package webhook

import (
	"net/http"

	"github.com/agent-fox-dev/parley/internal/server"
)

// collection is the pattern of a workspace's webhook collection.
const collection = "/workspaces/:slug/webhooks"

// Routes are the webhook routes, for the server's router, each declared
// through route, so that every one refuses a workspace-bound token, needs
// level owner on its workspace and refuses a task fork. The create accepts
// an Idempotency-Key, and a replay answers without the signing secret.
func (c *Component) Routes() []server.Route {
	create := c.route(http.MethodPost, collection, scopeWrite, c.create)
	create.Create, create.ShowOnce = true, []string{"secret"}
	return []server.Route{create}
}
