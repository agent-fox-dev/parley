# Errata: 17_git_smart_http

Where the implementation of package 17 (`.specs/17_git_smart_http`)
differs from what the package says, or decides what it leaves open.

## The git server is composed in `internal/hub`, not in `cmd/parleyd`

The package wires the git server's collaborators "in `cmd/parleyd`". Every
part of the hub is composed in a file of its own in `internal/hub`
([ADR 01](../adr/01-compose-the-hub-in-one-package.md)), and `cmd/parleyd`
only calls `hub.Run`; so is this one. `internal/hub/gitserver.go` adds the
part `composeGitServer`, after `composeForks` in `parts`: it mounts the three
routes with `Composition.Handle` and binds the `gitserver.Component` in the
start hook `gitserver`. `Composition.GitServer()` returns the component, and
`Composition.AdjustGitServer` changes its dependencies before they are
bound, the test seam the other parts have.

## The rate limit is the server's own limiter, reached through a server hook

The package builds the git handler "from options" that include the limiter
of `internal/server`. The hub builds its HTTP server, and with it the
limiter, after the start hooks, where the component is bound; so the
component's `Limiter` is a `serverLimiter` that a server hook
(`Composition.addServerHook`, run by `install`) points at
`srv.RateLimiter()` before the first request. The key a request is counted
against comes from `server.LimitKey(r, credentialID)`, the function the
REST pipeline counts with, so that a credential has one window across git
and the REST API and a request that failed authentication counts against
the same address key.

## The checks run against a real hub

The checks of the package ask for "a hub handler from `internal/gitserver`
served through `net/http/httptest` over a real store". The tests of
`internal/gitserver` start a real hub with `hubtest.StartWith`, which serves
the real server in-process on a loopback port over a real store, and
replace the service that runs after the checks with a recorder through
`AdjustGitServer`; the git routes are thereby also reached through the
server's dispatcher, as in production.
