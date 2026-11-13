package hub

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// composeIdentity adds the identity records to the transactional store,
// installs the identity service as the pipeline's authenticator, so that
// every non-public route under /api/v1 is authenticated, and registers the
// service's routes: GET /auth/providers and POST /auth/callback. The
// service gets its store, clock and configuration in a start hook, which
// runs before the hub serves its first request. It is bound without a
// provider client yet, so the hub answers every login 502 upstream_error.
func composeIdentity(c *Composition) {
	svc := &identity.Service{}
	c.AddSchemaStep(identity.SchemaStep)
	c.AddStartHook("identity", func(_ context.Context, rt *Runtime) error {
		svc.Bind(identity.Deps{
			Store:       rt.Store,
			Now:         rt.Now,
			Providers:   rt.Config.OAuth.Providers,
			ExternalURL: rt.Config.Server.ExternalURL,
		})
		return nil
	})
	c.SetAuthenticator(svc.Authenticate)
	for _, r := range svc.Routes() {
		c.AddRoute(r)
	}
}
