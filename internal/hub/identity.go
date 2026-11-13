package hub

import (
	"context"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// composeIdentity adds the identity records to the transactional store,
// installs the identity service as the pipeline's authenticator, so that
// every non-public route under /api/v1 is authenticated, adds its admin
// check to the access checks, and registers the service's routes: GET
// /auth/providers, POST /auth/callback and the admin route POST /users. The
// service gets its store, clock and configuration in a start hook, which
// runs before the hub serves its first request, with the client that reads
// a user's identity from the configured providers at login.
func composeIdentity(c *Composition) {
	svc := &identity.Service{}
	c.AddSchemaStep(identity.SchemaStep)
	c.AddStartHook("identity", func(_ context.Context, rt *Runtime) error {
		svc.Bind(identity.Deps{
			Store:       rt.Store,
			Now:         rt.Now,
			Providers:   rt.Config.OAuth.Providers,
			ExternalURL: rt.Config.Server.ExternalURL,
			Client:      identity.NewOAuthClient(rt.Config.OAuth.Providers, http.DefaultClient, identity.DefaultProviderTimeout),
		})
		return nil
	})
	c.SetAuthenticator(svc.Authenticate)
	c.AddAccessCheck(svc.AdminCheck)
	for _, r := range svc.Routes() {
		c.AddRoute(r)
	}
}
