---
spec_id: "01"
spec_name: "service_foundation"
title: "Serve the hub's request contract on two platform targets"
status: "active"
created_at: "2026-10-27T17:39:36Z"
updated_at: "2026-10-27T19:44:58Z"
intent_hash: "3bcb6e59151fefb7855c7a834fa7224be0a4c7ff6b75e2a002d5cde0101a4786"
schema_version: 2
source: "docs/prd/01-port-af-hub-to-typescript-on-cloudflare.md"
---

# Serve the hub's request contract on two platform targets

## Intent

Every route Parley will ever serve shares one envelope: where it lives, how
it reports an error, how it reads a timestamp, how it pages, and how an
operator learns the service is alive. Getting that envelope right once, in
one place, is what keeps a hundred later routes consistent with `af-hub`
instead of consistent with whoever wrote them. The same is true of the
platform underneath: a service that can only run on Cloudflare is a service
whose portability claim is an intention, so the floor this package lays down
is proved by running the same checks on a second target that needs no
Cloudflare account at all.

## Goals

- A deployment answers `GET /healthz`, `GET /readyz`, `GET /version` and
  `GET /metrics` exactly as `docs/prd/api.md` § Server Endpoints specifies.
- Every error any route of the service will ever return uses one envelope,
  produced in one place, with `error_type` present exactly where `api.md`
  § Error types lists one.
- The request conventions of `api.md` § Conventions that this package's own
  routes exercise — the mount point, the body-size limit and RFC 3339
  timestamps normalised to UTC — are provided once and are what later routes
  use.
- Every platform interface this package defines has one shared contract
  suite that passes against its Cloudflare implementation and against a
  second implementation that runs on one Linux or macOS host with no
  Cloudflare account.
- `make check` runs on a laptop, offline, and `make check-release` runs the
  conformance harness against a deployment, skipping with a named reason when
  a binary, a network or a deployment is missing.

## Non-goals

- **Every route under `/api/v1`.** This package serves the root routes and
  the envelope the others will use; it serves no workspace, identity, secret,
  git, forge, audit or session route. A request to a path under the mount
  point that matches no route is a `404` in the envelope, which is the
  correct answer until the package that owns that path is built.
- **Authentication.** No credential is parsed and no caller is identified.
  The root routes this package serves need none, and the credential kinds,
  the scopes and the access model are package `02_identity_and_credentials`
  and package `03_workspaces`.
- **The platform interfaces this package does not use.** It defines and
  implements the relational store, the clock, the configuration source and
  the metrics sink, because its own behaviour needs them. The blob store, the
  queue, the per-workspace lock and actor, the scheduled job runner, the event
  bus, the secret store, the repository store and the history engine are
  defined by the packages that first need them, against the pattern this one
  sets.
- **Cursor pagination.** `api.md` § Conventions defines it, but no route in
  this package pages, so building it here would mean shipping a component
  with no caller in production code. It belongs to the first package that has
  a paged route — `11_sessions_and_cost` — which builds it against the
  contract `api.md` fixes, for `12_audit_and_events` and the rest to reuse.
- **Conformance cases for routes that do not exist yet.** This package builds
  the harness and wires it into `make check-release`; each later package adds
  its own cases to it.
- **The remaining scopes of the port**, each its own package, in order:
  `02_identity_and_credentials`, `03_workspaces`, `04_secrets_and_variables`,
  `05_repository_store`, `06_history_engine`, `07_git_server`,
  `08_sync_and_branch_push`, `09_carry_patch`, `10_merges`,
  `11_sessions_and_cost`, `12_audit_and_events`, `13_forge_api`,
  `14_forge_webhooks`, `15_web_ui`.

## Background

The repository holds documentation and nothing else: `git ls-files` lists
`README.md`, `LICENSE`, `AGENTS.md`, `.specs/steering.md` and four documents
under `docs/prd/`. There is no source file, no `Makefile`, no
`package.json`, no `tsconfig.json` and no test runner. This package therefore
creates the project as well as the behaviour, and the commands it establishes
are the commands every later package is checked with.

`docs/prd/api.md` § Conventions and § Server Endpoints are the normative text
for the behaviour here, and `docs/prd/01-port-af-hub-to-typescript-on-cloudflare.md`
is the PRD this package is one scope of. Three of its decisions bear directly
on this package: **D8**, `GET /version` reports `runtime_version` in place of
the hub's `go_version`; **D9**, `GET /metrics` is served in full with a
metric reporting `0` where the port has no source for it yet; and **D11**,
the `Makefile` keeps the target names `AGENTS.md` mandates while `make doc`
and `docs/generated/` are dropped.

`api.md` fixes two details that are easy to get subtly wrong and expensive to
change later. The `path` label on the HTTP metrics is the **route template**,
not the requested URI, so that its cardinality stays bounded. And `/readyz`
is not a copy of `/healthz`: it answers `503` with the error envelope when
the database is unreachable, which is what makes it useful to a load
balancer.

## Requirements

### Route surface

- The service serves `GET /healthz`, `GET /readyz`, `GET /version` and
  `GET /metrics` at the server root, outside the mount point.
- The mount point is configurable and defaults to `/api/v1`. A request to a
  path under the mount point that matches no route is answered `404` in the
  error envelope.
- A request to a path that is neither under the mount point nor one of the
  four root routes is answered `404` in the error envelope.
- A request to a root route with a method other than `GET` is answered `405`
  in the error envelope.

### The error envelope

- Every error response carries `{"error": {"code", "message"}}` as JSON,
  where `code` repeats the HTTP status and `message` describes the error for
  a person.
- An `error_type` field is present exactly where `api.md` § Error types lists
  one for that condition, and is absent otherwise.
- An unhandled failure anywhere in the service is answered `500` with
  `message` `internal server error`, and the underlying error reaches the
  logs and never the response body.

### Request conventions

- A request body larger than the configured maximum is answered `413` before
  any route handler sees it. The maximum is configurable and defaults to
  1 MiB (1,048,576 bytes).
- A timestamp the service renders is RFC 3339 in UTC. A timestamp a client
  sends may carry any offset and is normalised to UTC before use; a value
  that is not RFC 3339 is answered `400` naming the field.

### Liveness, readiness and version

- `GET /healthz` answers `200` with `{"status": "ok"}` whenever the service
  is running, without reading any store.
- `GET /readyz` answers `200` with `{"status": "ready"}` when the relational
  store answers, and `503` with the error envelope when it does not.
- `GET /version` answers `200`, cached publicly, with `build_time`, `commit`,
  `mount_point` and `runtime_version` — the JavaScript runtime's version,
  under a truthful key in place of the hub's `go_version` (D8). `build_time`
  and `commit` come from the build; a build that supplies neither reports the
  empty string rather than failing.

### Metrics

- `GET /metrics` answers `200` with `Content-Type: text/plain; version=0.0.4`
  in the Prometheus text format.
- Every metric named in `api.md` § GET /metrics is present with the type and
  labels that document gives it. A metric the port has no source for yet
  reports `0`, as `api.md` already specifies for `afhub_jobqueue_depth`.
- `afhub_http_requests_total` counts every request the service answers,
  labelled with the method, the **route template** and the status, and
  `afhub_http_request_duration_seconds` observes its latency in the buckets
  `api.md` lists. A request to an unmatched path is counted under a single
  fixed template rather than its URI, so no caller can grow the label set.

### Running on two targets

- Every platform facility this package uses — the relational store, the
  clock, the configuration source and the metrics sink — is reached through
  an interface, and two implementations of each exist: one on Cloudflare and
  one that needs no Cloudflare account.
- Each interface has one shared contract suite, and that suite passes against
  both of its implementations. A case that passes against one and fails
  against the other is a defect in the interface, not a tolerance.
- The second target is deployable: a single artefact a person starts on one
  Linux or macOS host, with no Cloudflare account and no network access to
  Cloudflare, which then answers the four root routes identically to the
  Cloudflare deployment — the same status, the same body, the same headers,
  save for the values that are genuinely per-deployment (`build_time`,
  `commit`, and the measured metric values).

### The conformance harness

- A harness runs a suite of HTTP checks against a deployment named by its
  base URL, and reports each check's outcome.
- The harness obtains `afc` by building it from a pinned commit of
  `github.com/agent-fox-dev/hub`.
- A check whose binary, network or deployment is unavailable is **skipped**
  with a message naming what was missing, and a skip is never a failure.
  `make check` therefore passes on a laptop with no network and no
  deployment.

### End-to-end flows

1. **An operator brings the service up and probes it.** The operator deploys
   to Cloudflare, then calls `GET /healthz`, `GET /readyz` and `GET /version`
   in turn; the service answers from the running Worker, reading the real
   relational store for the readiness probe, and the operator sees `ok`,
   `ready` and the build information.
2. **An operator runs the same service without Cloudflare.** The operator
   starts the second target on one host, with its own relational store and no
   Cloudflare account, and the same three probes answer with the same
   statuses and the same body shapes.
3. **A scraper reads the metrics after traffic.** An API client calls a path
   under the mount point that matches no route and receives `404` in the
   error envelope; a scraper then reads `GET /metrics` and finds that request
   counted in `afhub_http_requests_total` under a bounded route-template
   label, with its latency observed.

## Design Decisions

1. **The test commands are `make test` and `make lint`, created by this
   package.** The repository has no build at all, so there is no existing
   command to adopt. D11 of the source PRD fixes the target names; this
   package writes them, and every later package's `test_commands` names the
   same two.
2. **Cursor pagination is deferred, not built here.** It was in the first
   draft of this package, on the argument that building it once stops four
   packages inventing a cursor each. Laying out the tasks showed the flaw: no
   route in this package pages, so the facility would have no caller in
   production code, and the integration task exists precisely to fail work in
   that state. The contract is fixed in `api.md` either way, so the first
   package with a paged route builds it and the rest reuse it.
3. **This package defines only the interfaces its own behaviour needs.**
   Defining all ten of the source PRD's platform interfaces up front would
   mean writing abstractions with no implementation to shape them. The
   relational store, the clock, the configuration source and the metrics sink
   are needed here; the rest are defined by the package that first needs one,
   against this package's pattern.
4. **A request to an unmatched path is counted under one fixed route-template
   label.** `api.md` requires the `path` label to be the route template to
   bound its cardinality, and an unmatched path has no template. Using the
   URI would let any caller grow the label set without limit, which is the
   problem the rule exists to prevent.

## Dependencies

- `docs/prd/api.md` § Conventions and § Server Endpoints are normative for
  every behaviour here, and `docs/prd/cli.md` for nothing in this package:
  `afc` has no command that calls a root route except `afc login`, which
  reaches identity routes this package does not serve.
- `.specs/steering.md`, whose quality-gate rule this package implements in
  TypeScript, and whose Go-specific `make doc` and `docs/generated/` rules
  it drops under D11.
- **Cloudflare Workers** for the first target. No other Cloudflare product is
  used by this package, so Artifacts — open beta, and the port's largest
  external risk — is not yet on the critical path.
- **The real `git` binary and the Go toolchain** are test dependencies of the
  harness only, and every check that needs one skips when it is absent.
- No spec package exists before this one, so it depends on none.

## Verified External API

Nothing could be verified against installed code: the repository has no
package manifest and no dependencies, so no library's source was read.

The source PRD's `## Verified External API` records what was read from
Cloudflare's documentation for the port as a whole; none of it is on this
package's path. The libraries this package will use — a Worker HTTP router,
a test runner, a linter, a formatter and a Prometheus text encoder — have not
been chosen, and no signature has been read. Each must be verified against
its installed types as it is adopted.
