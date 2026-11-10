package hub

import (
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// composeIdentity registers the identity records, installs the
// authenticator in the authentication position of the request pipeline, so
// every route under /api/v1 that is not public is authenticated, and mounts
// the public login routes over the configured providers.
func composeIdentity(c *Composition) {
	for _, s := range identity.Steps {
		c.AddStep(s)
	}
	c.OnServer(func(rt *Runtime, srv *server.Server) error {
		srv.SetAuthenticator(identity.NewAuthenticator(rt.Store, rt.Now))
		routes := &identity.Routes{Providers: identity.ProvidersFrom(rt.Config.OAuth.Providers)}
		routes.Register(srv)
		return nil
	})
}
