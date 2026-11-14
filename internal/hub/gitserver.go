package hub

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/server"
)

// gitServerHook is the name of the start hook that binds the git server.
const gitServerHook = "gitserver"

// composeGitServer adds the git server: it mounts the three git routes
// under /git on the server's router, outside the REST API, so that they
// pass none of its checks, and binds the component in a start hook, before
// the hub serves its first request, to the identity service, which checks
// the Basic password as it checks a bearer credential, and to the server's
// own rate limiter, so that a credential has one window across git and the
// REST API. The server, and with it the limiter, is built after the start
// hooks, so the limiter reaches the component through a server hook: the
// component holds a serverLimiter, which the hook points at the limiter of
// the server that serves.
//
// No git service is installed: a request that passes the checks is answered
// 500 internal error.
//
// The component exists from the composition on (GitServer); tests change
// its dependencies with AdjustGitServer.
func composeGitServer(c *Composition) {
	comp := &gitserver.Component{}
	c.gitServer = comp
	comp.Mount(c.Handle)
	limiter := &serverLimiter{}
	c.addServerHook(func(srv *server.Server) { limiter.l = srv.RateLimiter() })
	c.AddStartHook(gitServerHook, func(_ context.Context, rt *Runtime) error {
		d := gitserver.Deps{Credentials: c.identity, Limiter: limiter, Logger: rt.Logger}
		for _, adjust := range c.gitServerAdjusters {
			adjust(&d)
		}
		comp.Bind(d)
		return nil
	})
}

// serverLimiter is the rate limiter of the HTTP server the hub serves with,
// which exists only once the server is built. install sets l before the
// server serves its first request.
type serverLimiter struct{ l *server.Limiter }

func (s *serverLimiter) Allow(key string) (bool, int) { return s.l.Allow(key) }

// GitServer returns the hub's git server, which serves the three git routes
// once its start hook has run.
func (c *Composition) GitServer() *gitserver.Component { return c.gitServer }

// AdjustGitServer adds fn to the functions that change the git server's
// dependencies after the start hook has built them and before it binds
// them, in the order they were added. Tests replace a collaborator with it,
// such as the service that runs after the checks.
func (c *Composition) AdjustGitServer(fn func(*gitserver.Deps)) {
	c.gitServerAdjusters = append(c.gitServerAdjusters, fn)
}
