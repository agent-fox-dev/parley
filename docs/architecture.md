# Parley Architecture

How Parley is put together: what the pieces are, what each is for, and why
the boundaries fall where they do. It is for people changing the service.

## Contents

- [Shape](#shape)
- [The layout](#the-layout)
- [Two platform targets](#two-platform-targets)
- [The web interface](#the-web-interface)

## Shape

Parley is a TypeScript service that serves `af-hub`'s HTTP contract. It runs
as a Cloudflare Worker, and the same service code runs on a second target
that needs no Cloudflare account — which is what keeps the portability claim
a measured one rather than an intention.

The entry point builds an application object and hands it each request. The
application is built from configuration and a list of routes, so a test
constructs one directly rather than starting a server.

```
request → entry point (src/targets/<target>/…)
            → application (src/http/router.ts)
                 → authentication (src/auth/middleware.ts)
                      → public route → straight through
                      → no credential accepted → 401
                 → scope check (src/auth/scopes.ts) → 403 for a token without it
                 → server routes (src/routes/server.ts)
                 → unmatched  → 404 in the error envelope
                 → wrong method on a server route → 405
                 → unhandled failure → 500, detail to the log only
```

## The layout

| Path                   | What it holds                                                                                                                                |
| ---------------------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| `src/targets/`         | One entry point per target. Each assembles a platform, applies the schema migrations and builds the application once, then dispatches to it. |
| `src/auth/`            | Who is calling: the credential formats, the scope registry, and the middleware that identifies a caller or refuses the request.              |
| `src/store/`           | The schema migrations both targets run, and the reads and writes over the relational store.                                                  |
| `src/http/router.ts`   | Builds the application: the mount point, the route table, and the answers for an unmatched path, a wrong method and an unhandled failure.    |
| `src/http/errors.ts`   | The error envelope every error of the service is answered in, and the error type carrying a status and an optional machine-readable class.   |
| `src/routes/server.ts` | The four routes served at the server root: liveness, readiness, version and metrics.                                                         |

`src/testutil/` holds what many suites need — a migrated store on a
temporary file, seeded users, keys and tokens — so that no test file builds
its own.

A module's tests sit beside it as `*.test.ts` and are named for the
behaviour they check. Unit and integration suites are hermetic and run in
`make check`; a `*.smoke.test.ts` needs a running deployment and runs in
`make check-release`.

## Serving git

Parley is the git server, not a proxy in front of one. The advertisement
is built from the repository store's ref list, a fetch is a pack the store
serves for the wants and haves a client sent, and a push is parsed into
ref commands, filtered, and handed to the store. ADR 10 records why: push
control has to decide each ref before any of it is relayed, and only one
of the two targets has a remote to proxy to at all.

That puts three things in `src/git/`: the protocol itself (`pktline.ts`,
`transport.ts` for the client side, `routes.ts` and `fetch.ts` for the
server side), the decisions (`access.ts`, `push_control.ts`,
`protected_refs.ts`), and what follows a push (`after_push.ts`). The
decisions are deciders over one ref, composed in the route, so a push
carrying a refused ref and an accepted one gets both answers.

## Two platform targets

Every facility of the host platform is reached through a named interface, so
that it can be provided by Cloudflare or by something else without the
service changing. The application object takes its configuration and its
route table as arguments rather than reading globals, which is what lets one
test suite exercise both wirings.

Four facilities are defined so far — the relational store, the clock, the
configuration source and the metrics sink — each with one contract suite run
against every implementation a deployment can be wired with. Which
implementations those are, and why two of the four have one shared
implementation rather than two, is
[ADR 1](adr/01-define-the-platform-interfaces-and-their-implementations.md).

| Interface            | On Cloudflare                                                                                                                       | Off Cloudflare            |
| -------------------- | ----------------------------------------------------------------------------------------------------------------------------------- | ------------------------- |
| relational store     | D1                                                                                                                                  | SQLite on the host's disk |
| workspace lock       | a lease row in the relational store, shared by both targets ([ADR 5](adr/05-hold-the-workspace-lock-as-a-lease-in-the-store.md))    |                           |
| secret store         | sealed values in the relational store, shared by both targets ([ADR 4](adr/04-seal-secrets-with-aes-gcm-under-a-deployment-key.md)) |                           |
| repository store     | Cloudflare Artifacts                                                                                                                | a bare repository on disk |
| configuration source | the Worker's bindings                                                                                                               | the process environment   |
| clock                | the host's own clock, shared by both targets                                                                                        |                           |
| metrics sink         | an in-memory registry, shared by both targets                                                                                       |                           |

## The web interface

`web/` holds the pages a person reads in a browser: the landing page, the
dashboard of their workspaces, and one workspace's state. They are server
components rendered to a string by `web/render.tsx`, with no bundler, no
client framework and the one script that applies a chosen theme before the
first paint ([ADR
13](adr/13-render-the-web-interface-on-the-server-with-no-build-of-its-own.md)).
`src/routes/web.ts` serves them and `src/routes/web_auth.ts` signs a person
in; both are part of the same application, so both targets serve the
interface.

Two boundaries hold it in place. A page reads nothing from the store: it
asks the service's own API, as the person signed in, through
`web/api_client.ts` — which is why the dashboard shows exactly what that
person's own key would be shown, and why a workspace they may not read is
one answer the interface does not have to decide for itself. And a browser
session (`web/session.ts`) is a credential kind of its own: signing in
issues no API key and revokes none, so opening the dashboard cannot
interrupt an agent that is running.

```
browser → /dashboard (src/routes/web.ts)
            → the session names the person (web/session.ts)
            → the API, as that person (web/api_client.ts)
                 → the same routes an API key reaches
            → the page, rendered to a string (web/render.tsx)
```

The interface only reads; everything that changes anything stays with the
API and `afc`.
