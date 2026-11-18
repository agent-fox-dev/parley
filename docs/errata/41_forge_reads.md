# Errata: 41_forge_reads

Where the implementation of package 41 (`.specs/41_forge_reads`) differs from
what the package says.

## The credential kind is resolved from the secrets in internal/forge

The package has the credential resolution use package 10's
`workspace.ResolveOriginCredential` for origin, and package 31's upstream
order for upstream (`carrypatch.ResolveUpstreamCredential`). Both return a
git credential without its kind, and the diagnostic route reports the kind
(`pat`, `basic` or `none`). So `forge.ResolveCredential`
(`internal/forge/resolve.go`) reads the workspace secrets itself, in the same
order: for origin a non-empty `GIT_PAT`, else `GIT_USERNAME` with
`GIT_PASSWORD`; for upstream the `UPSTREAM_` secrets in that order first,
then the origin's. It reads them on every call, and a secret that cannot be
read is an error, never no credential.

## The proxy variables come from the hub's environment

The package names `httpproxy.FromEnvironment`, which reads the process
environment. The hub reads no process environment; `Options.Env` is its only
source ([ADR 06](../adr/06-load-the-secret-keys-before-the-stores-open.md)).
So `forge.NewClient` (`internal/forge/client.go`) reads `HTTPS_PROXY`,
`HTTP_PROXY` and `NO_PROXY` (and their lower-case names) from
`ClientOptions.Env`, which the hub fills with `Options.Env`, into an
`httpproxy.Config` when the client is built.

## The body limit reads one byte past it

A body over 8,388,608 bytes is detected by reading at most one byte past the
limit (`io.LimitReader` of 8,388,609 bytes in `internal/forge/client.go`):
the 8,388,609th byte is read, and nothing after it. A success answer whose
body is longer is `forge_error`. An error answer, a rate limit included,
is judged by its status, and its body is cut at the limit for the message.

## A fork's parent access is the access rule's decision

The package requires the route's need on a task fork's parent "by the level
of the user behind the credential". The route helper
(`internal/forge/route.go`) asks the access rule's `Decide` for the route's
need on the parent, with the caller and no binding applied, and refuses
`403 forbidden` unless it is granted. For the routes of this package, whose
need is read, that decision is the level check the package names.

## The number rule lives in a leaf package

The rule for `:number` and for the CLI's `<number>` argument (digits only,
no sign or leading zero, 1 to 2,147,483,647) lives in
`internal/forge/number`, so that the parley client does not import the
hub's forge component. `forge.ParseNumber` delegates to `number.Parse`.

## internal/server gains list writers and cursors

Forge lists learn the next page from the forge's `Link` header, not from
the items, and the issue thread hands out a cursor of the comments route.
So `internal/server/list.go` gains `WriteList` (a page with a given
`next_cursor`), `(Page).Cursor` (a cursor of the request's own route) and
`CursorFor` (a cursor of another route), beyond the files the package
touches.

## The API URL check at use is the settings rule

`forge.CheckAPIURL` checks a stored `forge_api_url` or
`upstream_forge_api_url` again at use. It calls the rule the settings route
stores the value by, `workspace.ValidateForgeURL`
(`internal/workspace/settings.go`), rather than a copy of it.

## The pull request has 15 members

The check of the pull request read asks for "the 16 members of the pull
request type". The type's contract lists 15 (`number`, `title`, `body`,
`state`, `url`, `draft`, `merged`, `head_sha`, `head_branch`,
`base_branch`, `author`, `created_at`, `updated_at`, `closed_at`,
`merged_at`), and `internal/forge/pulls_test.go` asserts those 15.

## The shared checks are tested through probe routes

The shared checks of a forge route came before any read route, and the
order of the route helper's `Check` hook after the resolution is shown with
a refusal no read route makes. So `internal/forge/route_test.go` and the
failure tests drive the checks through test-only probe routes
(`/workspaces/:slug/forge-probe` and `/workspaces/:slug/forge-probe/:number`),
registered with the component's route helper from
`hubtest.Config.Compose`. The read routes are rows of the same tables.
