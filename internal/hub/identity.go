package hub

import (
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// composeIdentity registers the identity records and installs the
// authenticator in the authentication position of the request pipeline, so
// every route under /api/v1 that is not public is authenticated.
func composeIdentity(c *Composition) {
	for _, s := range identity.Steps {
		c.AddStep(s)
	}
	c.OnServer(func(rt *Runtime, srv *server.Server) error {
		srv.SetAuthenticator(identity.NewAuthenticator(rt.Store, rt.Now))
		return nil
	})
}
