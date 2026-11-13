package hub

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// composeIdentity adds the identity records to the transactional store and
// installs the identity service as the pipeline's authenticator, so that
// every non-public route under /api/v1 is authenticated. The service gets
// its store, clock and configuration in a start hook, which runs before the
// hub serves its first request.
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
}
