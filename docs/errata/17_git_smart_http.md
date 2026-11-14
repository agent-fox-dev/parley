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

## A workspace "without an organization" is one whose organization is gone

The package answers `404 repository not found` for "a workspace without an
organization". Every workspace record carries an `org_id` (the column is
`NOT NULL` and `access.InsertRecord` refuses an empty one), but it carries
no foreign key, so a workspace keeps the id of an organization that has
since been deleted. That is the workspace without an organization: the git
server answers it, and a record whose `org_id` is empty, with the same
`404` as an unknown slug, and its test stores a workspace whose `org_id`
names no organization.

## The two `service` values are checked at the exchange, not at its content type

The check of the package asserts that ref discovery with
`service=git-upload-pack&service=git-receive-pack` answers with the
`Content-Type` of an upload-pack advertisement. Which service serves a
request is decided by the checks, and the advertisement is written by the
service that runs after them; the test of the checks asserts that the
request reaches that service as an upload-pack request with need read. The
content type is the one the upload-pack advertisement always carries, written
by the service of ref discovery, not by the checks.
