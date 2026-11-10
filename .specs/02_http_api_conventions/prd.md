---
spec_id: "02"
spec_name: "http_api_conventions"
title: "Apply one set of conventions to every REST route"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "e370818e1296bc753c2052931932f7c62c6476a2f08feeecfd709c8de178261c"
schema_version: 3
source: "docs/prd/03_platform.md"
---

# Apply one set of conventions to every REST route

## Intent

Every route of the Parley REST API answers by the same rules: one error envelope with a machine-readable `error_type`, one order of checks, one shape for lists, one meaning of `Idempotency-Key`, one timestamp format, one set of body rules, one set of response headers and one rate limit. The rules are built once, as shared middleware and helpers in the server, so that a client learns them once and every later feature obeys them without restating them.

## Goals

- Every error response of the REST API is the JSON envelope `{"error":{"code","message","error_type"}}`, with `code` equal to the HTTP status and `error_type` taken from one catalogue.
- Every request passes the same ordered checks, and the first failing check decides the answer; later packages plug authentication, the binding guard, the scope check and the access check into fixed positions of that order.
- An unknown path is answered `404 not_found` and a wrong method `405 method_not_allowed` with an `Allow` header, both before authentication.
- Every list endpoint answers `{"items":[…],"next_cursor":…}` with a `limit` of 1–200 (default 50) and an opaque cursor, through one helper.
- Every `POST` that creates a resource can be retried safely with an `Idempotency-Key` for 24 hours.
- Timestamps are written as RFC 3339 in UTC with whole seconds, and durations are read as a number with a unit suffix.
- A request body over the limit is `413`, a non-JSON body is `415`, and a body that is not a JSON object is `400`.
- Every response carries `X-Request-ID` and the three security headers; API responses carry `Cache-Control: no-store`.
- Requests are limited per credential to a configurable number per minute, and a request over the limit is `429 rate_limited` with `Retry-After`.

## Non-goals

- The server process, its listener, `/healthz`, `/readyz` and `/version` as endpoints: package 01 hub_skeleton. This package only adds its middleware around them.
- Reading `[server].max_body_size`, `[limits].requests_per_minute` and `[logging].log_health_probes` from the configuration file, and parsing the size string (`1MB`): package 03 server_lifecycle_and_configuration. This package takes the three values as server options with the PRD's defaults.
- Authentication: credential types, the `401` answers, the `403` for a blocked user, and which routes are public: package 04 authentication_and_login. This package fixes only where authentication runs and what a route without an authenticator answers.
- The PAT scope registry and the scope check's logic: package 06 api_keys_and_access_tokens. Only the position of the scope check is fixed here.
- The workspace access rule and the admin check: packages 07 workspace_access_rule and 05 users_and_organizations. Only their position is fixed here.
- The binding guard and its allowlist: package 08 workspace_bound_tokens. Only its position is fixed here.
- The sandbox feature check (SBX-FR-010): package 44 sandbox_runtime_and_profiles. This package provides the position it runs in.
- Applying the rate limit to the git server and exempting the webhook ingress: packages 17 git_smart_http and 43 forge_webhook_ingress. This package provides the limiter they call.
- Error types that other sections define (`workspace_busy`, `workspace_not_ready`, `fork_has_no_origin`, `unknown_session`, `ref_moved`, `land_conflict`, `policy_blocked`, `cursor_expired` and the rest): the package that owns each. This package provides the way to raise them.
- The order of items in any particular list, and any list with bounds other than 1–200 (the tree listing of package 20 scm_read_api): the package that owns the list.
- Body limits and content types of git routes, the webhook ingress and routes that carry file content: packages 17, 43, 20 and 21.
- Compare-and-swap of refs and the `409 workspace_busy` answer (the rest of PLAT-FR-019): packages 18 git_push_rules and 10 workspace_create_and_clone.
- The CLI's handling of the envelope and of `429`: package 09 cli_foundation.
- The durable storage of idempotency records: package 03 server_lifecycle_and_configuration, which owns the transactional store and installs a store-backed implementation of the record interface this package defines (see Design Decisions, 9). This package delivers the behaviour over that interface and an in-memory implementation.

## Background

The repository holds no code yet; every file named here is new. Package 01 hub_skeleton delivers the `parleyd` process, its HTTP server in `internal/server/` and the endpoints `/healthz`, `/readyz` and `/version`. This package adds, in the same directory, the conventions every later REST route relies on (source PRD `docs/prd/03_platform.md`, PLAT-FR-010 to PLAT-FR-019 and PLAT-FR-025, with §3.7 and §3.10 where they concern them).

The source PRD states these conventions once and tells every other section to refer to them and not restate them. They therefore have to exist before any feature route: the fifty-odd packages after this one register routes on what this package builds and raise errors through it.

This package registers no REST route of its own. Its behaviour is observed on routes that tests register, and, in a running `parleyd`, on unknown paths and on the endpoints of package 01.

## Requirements

### Surfaces and route matching (PLAT-FR-010, PLAT-FR-013 step 1, §3.10)

The REST API is mounted at `/api/v1`; the mount point is fixed. A later package registers a route by method and path pattern relative to that mount, with `:param` path parameters, and says whether the route is public, whether it creates a resource, and any limits of its own. Trailing slashes are not normalised: `/api/v1/things/` does not reach a route registered as `/things`, and is not redirected to it.

A request whose path matches no route is answered `404 not_found`. A request whose path matches a route under another method only is answered `405 method_not_allowed` with an `Allow` header listing the methods registered for that path. Both answers are given before authentication: a caller with no credential, a bad credential or a bound token gets the same `404` and `405` as any other caller, and the request is not counted against the rate limit.

### Error envelope and catalogue (PLAT-FR-011, PLAT-FR-012)

Every error response of the REST API is JSON with content type `application/json; charset=utf-8`:

```json
{"error":{"code":409,"message":"workspace is busy","error_type":"workspace_busy"}}
```

`code` equals the HTTP status. `error_type` is always present and, with the status, is the machine contract. `message` is for people. An error may carry extra members inside `error`: a named member (for example `oldest_seq`) or a `details` object.

The generic types and their statuses are: `invalid_request` 400, `unauthenticated` 401, `forbidden` 403, `not_found` 404, `method_not_allowed` 405, `conflict` 409, `gone` 410, `payload_too_large` 413, `unsupported_media_type` 415, `rate_limited` 429, `internal` 500, `upstream_error` 502, `unavailable` 503. The specific types of §3 are `insufficient_scope` 403, `token_bound` 403, `path_not_allowed` 403 and `workspace_archived` 409. Another package raises a specific type of its own by giving its name and a status.

A failure that is not one of these errors (a handler or check that returns some other error, or panics) is answered `500 internal`. The body's `message` is the fixed text `internal error`; the cause is written to the server log with the request id and never to the response.

### Order of checks (PLAT-FR-013, PLAT-FR-019)

Every request is checked in this order, and the first failing check decides the answer; no later check runs:

1. the path exists and the method is allowed on it (`404`, `405`);
2. authentication (supplied by package 04);
3. rate limit (`429`);
4. body size and content type (`413`, `415`);
5. the route's feature check, where a route declares one (the sandbox feature check of package 44), then the binding guard (package 08);
6. PAT scope (package 06);
7. workspace access or admin check (packages 07 and 05);
8. request validation, 9. state preconditions and 10. the action, which are the route's handler.

Steps 2, 5, 6 and 7 are positions that later packages fill; this package runs whatever is installed in them, in this order, and treats an empty position as passing — except authentication. A route that is not public and has no authenticator installed is answered `401 unauthenticated`, so a missing component never grants access. A public route skips authentication and steps 5 to 7; its rate limit and body checks still run. Authentication reports the caller (the credential's id for the rate limit; the user, or the admin token, for idempotency) to the later steps and the handler.

No request fails only because another request is running: two requests in flight at once, on the same route or on different ones, are both served.

### Request bodies (PLAT-FR-017)

A request body larger than the body limit (default 1 MiB, 1 048 576 bytes) is `413 payload_too_large`, whether or not the request declares its length; a body of exactly the limit is accepted. A route may state its own limit. A `POST`, `PUT` or `PATCH` with a body whose `Content-Type` is not `application/json` is `415 unsupported_media_type`; a request without a body is exempt, and a route may state another content type. A body that is both too large and of the wrong type is `413`. Both checks run before the handler and before steps 5 to 7.

A body that is not valid JSON, or is not an object where one is expected (an array, a string, a number, `null`, or nothing at all), is `400 invalid_request`. Unknown members are ignored.

### Lists (PLAT-FR-014)

Every list endpoint answers `{"items":[…],"next_cursor":"<opaque string>"}`. `items` is always an array, `[]` when there is nothing, never null. `next_cursor` is null on the last page, including when the number of items is an exact multiple of `limit`.

`limit` defaults to 50. A value outside 1–200, or one that is not an integer, is `400 invalid_request`, unless the route states other bounds. `cursor` is the `next_cursor` of the previous page and is opaque. A cursor that is malformed, or that was issued for another query (another route, another path parameter, or other filter parameters), is `400 invalid_request`. Following `next_cursor` from the first page to the last returns every item that existed throughout exactly once, in the endpoint's order.

### Idempotency (PLAT-FR-015, §3.7, §3.10)

Every `POST` that creates a resource accepts an optional `Idempotency-Key` header of 1–255 characters. An empty key or one longer than 255 characters is `400 invalid_request`.

The hub records the response of a successful create under the key, the caller (the user, or the admin token) and the route, for 24 hours. A replay with the same key, caller, route and request body within that time creates nothing and answers status `200` with the original body. If the original body carried a secret that is shown once (a token or key value), the replay omits that member. A replay with a different request body is `409 conflict`. A failed request is not recorded, so a retry with the same key runs again. Of several concurrent requests with the same key, exactly one creates and the others answer as replays. After 24 hours the record is dropped and the key runs as new. The 24 hours do not end with the process: §3.7 lists idempotency records as "kept for 24 hours, then dropped" and, on the next line, only the rate-limit counters as "in memory; reset at restart", so a record written before a restart is still replayed after it once the hub runs on its store (package 03). The same key from another caller, or on another route, is unrelated. On a route that does not create a resource the header is ignored (so `POST /auth/callback` does not take it).

### Timestamps and durations (PLAT-FR-016)

Timestamps in responses are RFC 3339 in UTC with whole seconds (`2026-01-02T03:04:05Z`). Durations in requests and configuration are a number with a unit suffix: `30s`, `15m`, `24h`, `30d`. Anything else is refused with an error that names the value.

### Request id, response headers and the access log (PLAT-FR-018, §3.9)

An incoming `X-Request-ID` that is a valid UUID is reused; otherwise the hub generates one. Every response carries `X-Request-ID`, `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY` and `Referrer-Policy: no-referrer` — error answers, `/healthz` and every other surface of the server included.

Responses under `/api/v1` carry `Cache-Control: no-store` unless the route states its own value (as `GET /auth/providers`, source control reads and the event stream do in their packages). `/healthz` and `/readyz` carry `no-cache`; `/version` carries `public, max-age=300`.

Each request produces one access-log line, a JSON line with `method`, `path`, `status`, `duration_ms` and `request_id`. `path` is the path of the request's URL without the query string. Requests to `/healthz` and `/readyz` produce a line only when health-probe logging is on.

### Rate limit (PLAT-FR-025, §3.7, §3.10)

The hub limits requests per credential to a number per minute (default 600; 0 turns the limit off). Requests without a valid credential — requests to public routes, and requests that fail authentication — are counted per client address. A request over the limit is answered `429 rate_limited` with a `Retry-After` header in whole seconds. Requests that a later check refuses count too. `/healthz`, `/readyz`, `/version` and `/metrics` are exempt: they are neither counted nor refused. Counters are in memory and reset at restart.

### End-to-end flows

- A developer sends `GET /api/v1/no-such-thing` to a running `parleyd` with no credential. `parleyd` finds no route, skips every later check and answers `404` with the error envelope (`error_type` `not_found`, `code` 404), a generated `X-Request-ID`, the three security headers and `Cache-Control: no-store`, and writes one access-log line with the same request id.
- An agent sends a create `POST` with an `Idempotency-Key`, loses the answer and sends it again. `parleyd` runs the checks in order, creates once, records the answer, and on the second request answers `200` with the original body and creates nothing. The agent then lists the collection page by page, following `next_cursor` until it is null, and sees the created item exactly once.
- A developer's script sends more requests in a minute than the limit allows. `parleyd` serves the first ones, answers the next `429 rate_limited` with `Retry-After`, keeps answering `/healthz` with `200`, and serves the script again once the minute has passed.

## Design Decisions

1. **Standard library only.** HTTP uses `net/http` and its method-and-pattern router; no web framework and no third-party module is added by this package (project decision 3). The request id is a random (version 4) UUID built from `crypto/rand`.
2. **Where the code lives.** The conventions live in `internal/server/`. Feature packages import `internal/server` to register routes and raise errors; `internal/server` imports no feature package. The timestamp and duration helpers live in the leaf package `internal/server/timefmt`, which imports nothing of the project, so that the configuration loader of package 03 can use them without an import cycle.
3. **Limits arrive as options.** The body limit, the requests-per-minute limit and health-probe logging are options of the server with the defaults 1 048 576 bytes, 600 and off. Package 03 fills them from the configuration file. Why: this package depends on package 01 only.
4. **An empty position passes, except authentication.** With no authenticator installed, a non-public route answers `401 unauthenticated`. Why: `internal` is "never a grant", and a hub built without package 04 must not serve protected routes openly.
5. **Unknown paths outside `/api/v1`.** A path that matches nothing on the server at all (not `/api/v1`, not a surface another package mounted) also gets the `404` envelope. Why: the PRD is silent, and one answer is simpler than two.
6. **Body checks read the body first.** The pipeline reads the whole body, up to one byte past the limit, at step 4, so that `413` is decided before steps 5 to 10 also for a body sent without a declared length. Size is checked before content type. The content type is compared as a media type, ignoring case and parameters such as `charset`. Bytes after the JSON object other than white space make the body invalid (`400`).
7. **What a cursor holds.** A cursor is the URL-safe base64 form of a small JSON document holding the position after the last item and a fingerprint of the query: the route pattern, the values of its path parameters and every query parameter except `cursor` and `limit`. `limit` may change between pages. Cursors are not signed: a cursor grants nothing, because every page runs the full check order again.
8. **What an idempotency record is keyed and compared by.** The route in the key is the method and the concrete request path (so the same key on two workspaces is two records). "The same body" means the same bytes, compared by SHA-256. "Successful" means a 2xx status. The key is validated and looked up after step 7 and before the handler, so a replay is still authenticated and authorised. A route declares its show-once members by path (`token`, `token.token`); they are removed before the record is stored, so the secret is never kept.
9. **Idempotency records live behind a storage interface; the hub keeps them in its store.** The PRD requires more than the life of the process: §3.7 says the records are "kept for 24 hours, then dropped" and reserves "in memory; reset at restart" for the rate-limit counters and the refusal limiter. This package depends only on package 01, which has no store, so it defines the record store interface (find by key, caller and route; write; delete what is 24 hours old or older), every behaviour over it, and an in-memory implementation that is the server's default and the one its tests use. Package 03 server_lifecycle_and_configuration, which owns the transactional store and composes the process in `internal/hub/`, installs a store-backed implementation through a server option, so that a hub started by `parleyd` replays a record after a restart (decided with package 03, which depends on this package for it). A record 24 hours old or older is never replayed and is removed when later records are written, in either implementation. The gate that orders concurrent requests with one key is in memory in both; the hub is one process.
10. **The rate limit is a fixed window.** Each key (credential id, or client address) gets a window of 60 seconds that starts at its first counted request; the window admits the configured number of requests, and `Retry-After` is the time to the window's end rounded up to a whole second, at least 1. The client address is the connection's remote IP address; forwarding headers are not read. Why: the PRD gives only "per minute" and "whole seconds".
11. **A failed authentication is answered `401`, not `429`.** The check order puts authentication before the rate limit, so a request with a bad credential is counted against its client address but still answered by the authentication step; the address counter decides the answer only on public routes. Why: PLAT-FR-013 says the first failing check decides.
12. **Timestamps are truncated, not rounded,** to whole seconds. A duration is a non-negative whole number followed by exactly one of `s`, `m`, `h`, `d` (`d` is 24 hours); fractions, signs, spaces and compound forms (`1h30m`) are refused.
13. **A valid `X-Request-ID`** is the 36-character hyphenated hexadecimal form in either case, of any UUID version, and is echoed as received.
14. **Access log.** This package writes the access-log line, because the request id it must carry is created here. Health-probe lines are written at debug level when the option is on. The `path` member never holds the query string, so a credential sent as a query parameter does not reach the log; package 54 operations_and_packaging relies on this and extends the same file, `internal/server/accesslog.go`.
15. **Route declarations and the matched route are the seam for later packages.** Beside this package's own declarations, a route can declare the one scope a PAT needs (a name this package does not interpret; package 06's scope check reads it) and named marks with a value (package 04's needs-a-user mark, package 05's admin mark, package 07's need `read`, `write` or `owner`, package 08's refuses-bound-tokens mark). Every installed check and the handler can read the matched route: its method, its pattern as registered (`/workspaces/:slug/tokens`), its declarations and its path parameter values. The access position holds a list: packages 05 (admin check) and 07 (workspace access check) each install a check there, the checks run in installation order, and each decides from the route's declarations whether it applies. Why: packages 04 to 08 all assumed such declarations, and one position cannot otherwise serve two access checks.

## Dependencies

- Package 01 hub_skeleton (draft): the `parleyd` process and the HTTP server in `internal/server/` that this middleware wraps, with `/healthz`, `/readyz` and `/version`.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
- **Bound token**: A PAT confined to one workspace (and the forks created with it).
- **Error envelope**: The JSON body of every REST API error, `{"error":{"code","message","error_type"}}`.
- **Public route**: A route that needs no credential.
- **Create route**: A `POST` route that creates a resource and therefore accepts `Idempotency-Key`.
