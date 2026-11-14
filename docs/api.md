# API reference

The hub, `parleyd`, answers the routes below over HTTP. The REST API is
mounted at `/api/v1`. This page describes the rules every REST route
follows, the three built-in routes and the REST routes registered so far. It
grows as features land.

## Conventions so far

- The three built-in routes (`/healthz`, `/readyz`, `/version`) need no
  credential. They never read the `Authorization` header, so a request with
  or without one, valid or not, gets the same answer.
- Every response body is one JSON object, sent with
  `Content-Type: application/json; charset=utf-8`. Request bodies are JSON
  objects too, unless a route states another content type; see
  [Request bodies](#request-bodies).
- Every response, on every path, carries a request id and the security
  headers; see [Request ids and response headers](#request-ids-and-response-headers).

## Routes and matching

Every REST route lives under the fixed mount `/api/v1`. A route has a method
and a path with named parameters, such as `GET /api/v1/workspaces/{slug}`.
Paths are matched exactly, segment by segment, and a path that does not
match is never redirected. When two routes match a path, the one with a
fixed segment where the other has a parameter, reading from the left, serves
it: `GET /api/v1/workspaces/{slug}/changes/by-commit/stack` goes to
`changes/by-commit/{sha}`, not to `changes/{number}/stack`. A request that
no route serves is answered as follows:

| Request | Answer |
|---|---|
| a path no route matches, under `/api/v1` or anywhere else on the hub | `404` with `error_type` `not_found` |
| a path that differs from a route only by a trailing slash (`/api/v1/things/` for `/api/v1/things`) | `404` `not_found` |
| a path with a doubled slash, `.` or `..` segment (`//api/v1/things`, `/api/v1/./things`) | `404` `not_found` |
| a known path with a method no route of that path has | `405` with `error_type` `method_not_allowed` and an `Allow` header |

Both answers use the error envelope below and come before any credential is
looked at, so they are the same with no credential, a valid one or a bogus
one. `Allow` lists the methods registered for the path, joined by `, `, for
example `Allow: GET, POST`. A route registered for `GET` also answers `HEAD`
with the same status and headers and no body; `HEAD` is listed in `Allow`
only when a route registers it itself.

## Order of checks

A request to a REST route passes these checks in this order. The first one
that fails answers the request, and no later check or the handler runs.

1. Route matching: `404 not_found` or `405 method_not_allowed`, as above.
2. Authentication. A route that is not public needs a credential, as in
   [Authentication](#authentication): `401 unauthenticated`, or `403
   forbidden` for the admin token on a route that needs a user.
3. The rate limit: `429 rate_limited`, as in [Rate limit](#rate-limit).
4. The body size, then the content type: `413` or `415`, as in
   [Request bodies](#request-bodies).
5. The route's own feature check, where it declares one, then the binding
   guard. Every sandbox route has the sandbox feature check: `404`
   `sandboxes_disabled` on a hub without sandboxes (see
   [Sandbox routes](#sandbox-routes)). The binding guard confines a PAT
   bound to a workspace: `404 not_found` or `403 token_bound` (see
   [Workspace-bound tokens](#workspace-bound-tokens)).
6. The scope check: `403 insufficient_scope` for a PAT whose permissions
   do not satisfy the scope the route declares (see [Scopes](#scopes)).
7. The access checks: first the admin check of the admin routes, `403
   forbidden` with the message `this route needs an admin` for every
   caller but the admin token and an API key whose user has the role
   `admin` (see [Admin routes](#admin-routes)); then the workspace access
   check of the workspace routes, `404 not_found` with the message
   `workspace not found` for a caller who cannot read the workspace, or
   `403 forbidden` for an org member on a route that needs more than read
   (see [Workspace access](#workspace-access)).
8. On a create route, the `Idempotency-Key`: `400 invalid_request` for a bad
   key, `409 conflict` for a key used with another body, or a replay, as in
   [Idempotency](#idempotency).
9. The route's handler.

A public route skips steps 2 and 5 to 7; it keeps the rate limit, counted
by client address, and the body checks.

An error the hub did not expect, from a check or a handler, is answered `500`
with `error_type` `internal` and the message `internal error`; this includes
a check or handler that panics. Its details stay out of the response: the hub
writes them to its log with the request id that the response carries in
`X-Request-ID`, and goes on serving other requests. When a handler has
already started its answer before it fails, the answer is left as it is: the
error is written to the log with the request id, not appended to the body.

## Authentication

Every route under `/api/v1` that is not public needs a credential, sent as

```
Authorization: Bearer <credential>
```

The scheme `Bearer` is matched in any letter case and followed by one or
more spaces; white space around the credential is ignored. A credential in a
query parameter, a cookie or any other header is not read.

The hub knows three credentials, each recognised by its exact format:

| Credential | Format | Credential id |
|---|---|---|
| admin token | `parley_admin_` and 64 characters of `[0-9a-f]` | `admin` |
| API key | `parley_<key_id>_<secret>`: 8 and 32 characters of `[0-9A-Za-z]` | the `key_id` |
| personal access token (PAT) | `parley_pat_<token_id>_<secret>`: 8 and 32 characters of `[a-z0-9]` | the `token_id` |

The admin token has no user; an API key and a PAT act for the user they
belong to, and a PAT also carries its permissions, its path limits and the
workspace it is bound to. The hub stores the SHA-256 of each secret, never
the secret, and compares hashes in constant time.

A request that does not pass is answered before any later check runs, with
the first of these that applies, in the order of the table. The state of an
API key or PAT (revoked, expired, its user blocked) is looked at only after
its secret matched, so a wrong secret is answered `invalid credentials`
whatever that state, with the same body as an unknown id. The credential and
its user are read on every request: a revocation, a block or an unblock
holds from the next request on, and blocking a user changes none of the
user's credentials.

| Status | `error_type` | `message` | When |
|---|---|---|---|
| `401` | `unauthenticated` | `authentication required` | no `Authorization` header, another scheme (such as `Basic`), an empty credential, or a credential of none of the three formats |
| `401` | `unauthenticated` | `invalid credentials` | an API key or PAT whose id is not stored or whose secret does not match, an admin-format token that is not the admin token, or any admin-format token on a hub that is not bootstrapped; the answer is the same whichever part was wrong |
| `401` | `unauthenticated` | `credential revoked` | an API key or PAT whose secret matches and that is revoked |
| `401` | `unauthenticated` | `credential expired` | an API key or PAT whose secret matches, that is not revoked, and whose `expires_at` is at or before the current time; one without `expires_at` never expires |
| `403` | `forbidden` | `user is blocked` | a live API key or PAT whose user's status is `blocked` |
| `403` | `forbidden` | `this route needs a user` | the admin token on a route that needs a user; an API key or a PAT passes there |

```json
{"error":{"code":401,"message":"invalid credentials","error_type":"unauthenticated"}}
```

The rate limit counts an authenticated request against its credential id.

## Scopes

A route may declare one scope that a PAT needs to call it; each route's
section names it. The admin token and API keys carry no scopes and are never
held to one, and a route that declares none is open to a PAT whatever its
permissions. These 32 names are the registered scopes, matched exactly, in
case and without trimming; any other string is not a scope:

`users:read`, `users:write`, `orgs:read`, `keys:read`, `keys:manage`,
`tokens:read`, `tokens:write`, `tokens:manage`, `workspaces:read`,
`workspaces:write`, `git:read`, `git:write`, `changes:read`,
`changes:write`, `changes:approve`, `checks:write`, `patches:read`,
`patches:write`, `rebuilds:read`, `rebuilds:write`, `secrets:read`,
`secrets:write`, `vars:read`, `vars:write`, `forge:read`, `forge:write`,
`sandboxes:read`, `sandboxes:write`, `audit:read`, `audit:write`,
`sessions:read`, `sessions:write`.

A PAT passes a route's scope check when it holds the declared scope itself
or a scope that implies it:

| The route needs | Also satisfied by |
|---|---|
| `R:read`, for an area `R` that has `R:write` | `R:write` |
| `keys:read` | `keys:manage` |
| `tokens:read` | `tokens:write`, `tokens:manage` |
| `tokens:write` | `tokens:manage` |

Nothing else implies anything: `changes:approve` and `checks:write` are
satisfied only by themselves and satisfy no other check, so `changes:write`
does not let a token approve a change.

A PAT that fails the check is answered before the access checks, request
validation and the handler run, so the answer is the same whether or not
the resource it addresses exists, and nothing changes:

```json
{"error":{"code":403,"message":"this route needs the scope users:read","error_type":"insufficient_scope"}}
```

## Workspace-bound tokens

A PAT bound to a workspace (its `workspace` is a slug) is confined by the
binding guard, which runs on every route that is not public, after
authentication and the route's feature check and before the scope check
(step 5 of the [order of checks](#order-of-checks)). It runs only for a bound
PAT: the admin token, an API key and a PAT whose `workspace` is `null` get
the same answers as on a hub without it. An unknown path (`404`), a method
the path does not allow (`405`) and a credential that does not authenticate
(`401`: a wrong secret, a revoked or an expired token) are answered before
it, as for any caller. What the guard lets through goes on to the scope
check and the access checks as for an unbound PAT of the same user, so a
binding takes access away and never grants it.

A binding covers the bound workspace and the task forks created with the
token. A token bound to a task fork covers the fork alone, and on the
fork's parent also reaches the contributor routes below.

**Routes that name a workspace** (`/api/v1/workspaces/{slug}` and every
route under it) pass for a covered workspace. Every other slug, whether or
not a workspace has it, is answered with the same bytes as a workspace the
caller cannot read, before the scope check, so a token without the route's
scope gets the same answer:

```json
{"error":{"code":404,"message":"workspace not found","error_type":"not_found"}}
```

On the parent of the fork a token is bound to, the 25 contributor routes
pass; every other route of the parent is answered `403 token_bound` with the
message `a token bound to a task fork reaches only the contributor routes of
its parent`. The contributor routes, under `/api/v1/workspaces/{slug}`: `POST`
and `GET /changes`, `GET` and `PATCH /changes/:number`, `GET
/changes/by-commit/:sha`, `POST /changes/:number/abandon`, `/restore`,
`/restack` and `/land`, `GET /changes/:number/versions`,
`/versions/:version`, `/diff`, `/stack`, `/lands`, `/conflict`,
`/conflict/file`, `/approvals`, `/checks` and `/overlaps`, `GET /lands/:id`,
`GET /policy`, `POST /policy/evaluate`, `GET /activity`, `GET /claims` and
`GET /claims/:id`. The guard only lets them through, with the fork; confining
them to the changes whose source is the fork is each route's own.

**Routes that name no workspace** pass on the allowlist, with the binding,
from which the route confines its result to the covered workspaces (the
event routes do: see [`GET /api/v1/events/log`](#get-apiv1eventslog)):
`GET /api/v1/workspaces`, `GET
/api/v1/events`, `GET /api/v1/events/log`, `GET /api/v1/audit`, `GET
/api/v1/sandbox-profiles`, `POST` and `GET /api/v1/sessions`, `GET
/api/v1/sessions/:id`, `POST /api/v1/sessions/:id/complete`, and `POST` and
`GET /api/v1/sessions/:id/usage`. Every other route that names no workspace,
every identity route (`/user`, `/user/orgs`, `/user/keys…`, `/user/tokens…`),
every admin route, `POST /api/v1/workspaces` and `GET /api/v1/sandboxes`
among them, is answered before the scope check, whatever scopes the token
holds, so a bound token can neither read nor revoke itself nor mint another
token through `/api/v1/user/tokens`:

```json
{"error":{"code":403,"message":"a token bound to a workspace cannot use this route","error_type":"token_bound"}}
```

**Routes closed to bound tokens.** `POST` and `GET
/api/v1/workspaces/{slug}/tokens` refuse every bound PAT, so a bound token
can neither mint another nor list the tokens of its workspace. On a
workspace the binding reaches (the bound workspace, a fork the token
created, and the parent of the fork a token is bound to) they are answered
before the scope check, with or without the route's scope on the token:

```json
{"error":{"code":403,"message":"a token bound to a workspace cannot use this route","error_type":"token_bound"}}
```

On every other workspace the `404` above still comes first, so the answer
tells nothing of which workspaces exist. A route added later is closed the
same way when it is registered with the mark `access.refuses_bound`.

The guard recognises a contributor route and an allowlisted route by its
method and its pattern as registered, with the parameter names given here
(`:slug`, `:number`, `:sha`, `:version`, `:id`). A route added later
reaches a bound token only when it is registered under exactly these names.

**Refusal events.** Each `403` and `404` the guard answers, on the REST API
and on git, is reported as a hub event of type `parley.token.bound_refused`,
at most one per token per minute. The minute starts when the token's event
is emitted; the token's refusals inside it emit nothing and are counted, and
the token's next event carries the count as `suppressed` (`0` when there
was none). Every token is limited on its own, and the count is kept in
memory only, so a restart of the hub starts every token afresh. The answer
to the refused request is the same whether it is reported or counted, and
whether or not the event could be written. A request the guard lets
through, or that another check answers (`401`, `insufficient_scope`, the
access rule's `404`), is neither reported nor counted. The event, as
[`GET /api/v1/events/log`](#get-apiv1eventslog) shows it to the admin token
and to a user with level `owner` or `admin` on the bound workspace:

```json
{"seq":12,"id":"…","type":"parley.token.bound_refused","time":"2026-11-14T12:01:00Z","workspace":"w1","actor_type":"pat","actor_id":"u_1","resource_type":"token","resource_id":"t_1","action":"bound_refused","session_id":null,"metadata":{"bound_workspace":"w1","method":"GET","requested_workspace":"w5","route":"/api/v1/workspaces/:slug/probe","status":404,"suppressed":3,"surface":"api","token_id":"t_1"}}
```

`workspace` is the bound slug; `metadata.route` is the matched route's
pattern without the method (on git, the git route's pattern), `status` the
answered status, `surface` `api` or `git`, and `requested_workspace`, the
slug exactly as the request typed it, is present only when the request
named a workspace.

## Request ids and response headers

Every response the hub sends, on the REST API, on the built-in routes, for an
unknown path and on any other surface, carries these headers:

| Header | Value |
|---|---|
| `X-Request-ID` | the id of the request, see below |
| `X-Content-Type-Options` | `nosniff` |
| `X-Frame-Options` | `DENY` |
| `Referrer-Policy` | `no-referrer` |

A request may send its own `X-Request-ID`. When it is a UUID in the
36-character hyphenated hexadecimal form (`3f2a1bc4-0000-4000-8000-0123456789ab`),
of any version and in either letter case, the hub uses it as the request's id
and returns it exactly as received. Otherwise, or when the header is absent,
the hub generates a random (version 4) UUID in lower case and returns that.
The id is in the hub's log line for the request, so quote it when reporting a
problem.

`Cache-Control` is set once per response:

| Response | `Cache-Control` |
|---|---|
| anything under `/api/v1`, error answers (`401`, `404`, `405`, `413`, `429`, `500`, ...) included | `no-store`, unless the route states its own value in its section of this page |
| `GET /healthz`, `GET /readyz` | `no-cache` |
| `GET /version` | `public, max-age=300` |

A route's own value applies to every answer after the route has been matched,
its errors included; `404` and `405` at route matching are always `no-store`.

## Rate limit

The hub limits how many REST requests a caller may send in a window of 60
seconds: `[limits].requests_per_minute`, 600 by default (see
[configuration.md](configuration.md)); `0` turns the limit off. The window of a caller starts at its first counted request; once
it has ended, the next request starts a new one.

- An authenticated request is counted against its credential, so two
  credentials used from the same machine have a count each. A caller for
  which authentication reported no credential id is counted against its
  client address.
- A request to a public route, and a request that authentication refused,
  is counted against the client address: the IP address of the connection.
  Headers such as `X-Forwarded-For` are not read.
- A request over the limit is answered `429` with `error_type`
  `rate_limited`, the message `rate limit exceeded` and a `Retry-After`
  header: the seconds until the window ends, a whole number from 1 to 60,
  rounded up. It is answered before the body is read and before any later
  check or the handler runs.
- Every request that reaches the limit is counted, whatever its answer: one
  that a later check or its handler refuses counts like one that succeeds.
  A request answered `429` does not lengthen the window.
- A request that authentication refused is still answered with
  authentication's own error (`401`), even when its address is over the
  limit; the address's count decides the answer only on public routes.
- `/healthz`, `/readyz` and `/version`, and any request answered `404` or
  `405` at route matching (`/metrics` included, which the hub does not
  serve yet), are never counted or refused.
- The counts are kept in memory and start from zero when `parleyd` starts.

## Request bodies

The hub reads the whole body of a REST request before any check after the
rate limit runs, so these answers come before the feature check, the binding
guard, the scope check, the access checks and the handler:

| Request | Answer |
|---|---|
| a body over the body limit, with or without a declared `Content-Length`, whatever its `Content-Type` | `413` with `error_type` `payload_too_large` |
| a `POST`, `PUT` or `PATCH` with a body within the limit and a `Content-Type` that is missing or is not `application/json` | `415` with `error_type` `unsupported_media_type` |

- The body limit is `[server].max_body_size`, 1048576 bytes (1 MiB) by
  default (see [configuration.md](configuration.md)); a body of exactly the
  limit is accepted. A route can state a limit of its own, which then applies in place
  of the hub's; its section of this page says so.
- The size is checked before the content type, so an oversized body is `413`
  even when its type is wrong.
- The media type is compared without regard to letter case or parameters:
  `application/json`, `Application/JSON` and
  `application/json; charset=utf-8` are all accepted.
- A request without a body passes both checks whatever its `Content-Type`
  says.
- A route can state another request media type, such as `text/plain`, which
  it then accepts in place of `application/json`; the body limit still
  applies.
- `GET`, `DELETE` and other methods are not checked for a content type, but
  their body is still bounded by the limit.

A route whose body is JSON takes one JSON object. It answers `400` with
`error_type` `invalid_request` when the body is empty, is not valid JSON, is
a JSON value other than an object (an array, a string, a number, `null`), or
has anything but white space after the object. Members the route does not
know are ignored.

## Lists

Every route that answers a list answers one page of it, `200` with exactly
these two members:

```json
{"items":[{"id":2},{"id":4}],"next_cursor":"eyJwIjo0LCJmIjoi..."}
```

| Member | Value |
|---|---|
| `items` | an array of the page's items, in the list's order; `[]` when the page is empty, never `null` |
| `next_cursor` | a string when more items follow, `null` on the last page |

A list request takes two query parameters:

| Parameter | Meaning |
|---|---|
| `limit` | the most items a page holds: an integer from 1 to 200, 50 when absent |
| `cursor` | the `next_cursor` of the previous page; absent for the first page |

To read a whole list, request the first page and then repeat the same request
with `cursor` set to the last answer's `next_cursor` until it is `null`. Every
item that exists throughout the walk comes exactly once and in order, and the
last page has a `null` `next_cursor` also when the item count is a multiple of
the limit. A page continues after the last item of the previous one, so an
item added earlier in the order during the walk neither repeats nor hides
another.

A route whose list has another default or other bounds says so in its
section of this page; they then apply in place of 50 and 1–200.

These answer `400` with `error_type` `invalid_request`, before the list is
read:

- a `limit` that is not an integer (`abc`, `1.5`, an empty value) or is
  outside the bounds (`0`, `-1`, `201`);
- a `cursor` that is malformed (not URL-safe base64, or not a cursor
  document);
- a `cursor` issued for another query: another route, another value of a path
  parameter, or other query parameters apart from `cursor` and `limit`. The
  same cursor with a different `limit` is accepted, so a client may change
  the page size between pages;
- a query string that does not parse (such as `cursor=%%%`, with an
  unescaped `%`).

A cursor is opaque: clients must not build or change one. It carries no
grant; every page passes the full order of checks again.

## Idempotency

A `POST` that creates a resource accepts an `Idempotency-Key` header, so a
client that lost the answer can send the request again without creating a
second resource. A route's section of this page says when it is such a
create route; on every other route the header is ignored, however often it
is sent.

| Request to a create route | Answer |
|---|---|
| no `Idempotency-Key` header | the handler's answer; nothing is recorded |
| an `Idempotency-Key` that is empty or longer than 255 characters | `400` with `error_type` `invalid_request`; the handler does not run |
| a key of 1 to 255 characters not recorded for this caller and request | the handler's answer, unchanged; a `2xx` answer is recorded |
| a recorded key, with the same body as the recorded request | `200` with the recorded body; the handler does not run |
| a recorded key, with another body | `409` with `error_type` `conflict`; the handler does not run and the record stays as it was |

- A record belongs to the key, the caller (the admin token, else the user,
  else, for a credential that belongs to no user, that credential) and the
  request: the method and the request path. The same key sent by another
  caller, by the admin token, to another route or to another path of the same
  route (`/api/v1/owners/a/things` and `/api/v1/owners/b/things`) is a new
  request. A caller with no user and no credential id is never recorded.
- "The same body" means the same bytes: a body with the members in another
  order or with other white space is another body.
- A replay answers `200`, not the original `201` or `202`, with the
  original body and the headers the original answer set, such as its
  `Content-Type` and `Location`. Its `X-Request-ID` and every other header
  the hub stamps on each response are the replay's own.
- Only a `2xx` answer is recorded. After a `400`, `409`, `500` or any other
  failure, from the handler or from a check, the same key runs the handler
  again.
- A record is replayed for 24 hours. A record 24 hours old or older is
  treated as absent, so the request runs the handler and is recorded anew.
- A secret that a create route shows only once (a token, for example) is in
  the first answer only. The route's section names such members; they are
  removed before the answer is recorded, so a replay leaves them out and
  keeps every other member.
- While a request with a key is running, other requests with the same key,
  caller and request path wait for it. When it succeeds they are answered as
  replays; when it fails, one of them runs next. Requests with other keys
  never wait for each other.
- The key is checked after authentication and every other check, so a replay
  is authorised like the original request. A public create route ignores the
  header, since it has no caller to record it for.
- The records are kept in the hub's transactional store, so a request
  recorded before `parleyd` was restarted is replayed after the restart.

## Timestamps and durations

Every timestamp in a response body is a string in RFC 3339, in UTC, with
whole seconds and the suffix `Z`:

```json
{"created_at":"2026-01-02T03:04:05Z"}
```

A fraction of a second is cut off, never rounded, so the instant
`2026-01-02T05:04:05.999+02:00` is written `2026-01-02T03:04:05Z`. A
timestamp never carries an offset other than `Z` or a fraction of a second.

A duration, wherever Parley reads one, is a non-negative whole number
followed by exactly one unit:

| Unit | Means | Example |
|---|---|---|
| `s` | seconds | `30s` |
| `m` | minutes | `15m` (900 seconds) |
| `h` | hours | `24h` (86400 seconds) |
| `d` | days of 24 hours | `30d` (2592000 seconds) |

`0s` is zero. Every other form is refused, with an error that quotes the
value: an empty value, a number without a unit (`30`), a unit without a
number (`s`), any other unit (`30x`, `30w`), an upper-case unit (`30S`), a
sign (`-5m`, `+5m`), a fraction (`1.5h`), a compound (`1h30m`), spaces
(` 30s`, `30 s`), and a value too large to hold (more than about 292 years,
such as `106752d`).

## Errors

Every error of the REST API is answered with one JSON envelope, sent with
`Content-Type: application/json; charset=utf-8`:

```json
{"error":{"code":409,"message":"workspace is busy","error_type":"conflict"}}
```

| Member | Value |
|---|---|
| `code` | a number, always equal to the response's HTTP status |
| `message` | a non-empty message for people; do not branch on it |
| `error_type` | a non-empty, machine-readable type; branch on this and the status |

An error can carry extra members inside `error`, after the three above: a
named member such as `oldest_seq`, or a `details` object. An extra member
never replaces `code`, `message` or `error_type`.

```json
{"error":{"code":410,"message":"the cursor is too old","error_type":"cursor_expired","oldest_seq":17}}
```

The catalogue of error types and the status each is answered with:

| `error_type` | Status |
|---|---|
| `invalid_request` | `400` |
| `unauthenticated` | `401` |
| `forbidden` | `403` |
| `insufficient_scope` | `403` |
| `token_bound` | `403` |
| `path_not_allowed` | `403` |
| `not_found` | `404` |
| `method_not_allowed` | `405` |
| `conflict` | `409` |
| `workspace_archived` | `409` |
| `gone` | `410` |
| `payload_too_large` | `413` |
| `unsupported_media_type` | `415` |
| `rate_limited` | `429` |
| `internal` | `500` |
| `upstream_error` | `502` |
| `unavailable` | `503` |

A feature can answer a type of its own outside this table (for example
`workspace_busy` with `409`); its section of this page names the type and its
status. The envelope's shape is the same for every type.

## `GET /healthz`

Liveness: the process is up and serving HTTP.

| Status | Body |
|---|---|
| `200 OK` | `{"status":"ok"}` |

The answer carries `Cache-Control: no-cache`.

## `GET /readyz`

Readiness: whether the hub can do its work. Each request calls every
registered readiness probe. The hub is ready when every probe succeeds, and
also when no probe is registered. `parleyd` registers one probe, which fails
while either of its stores (the transactional store and the analytical
store) does not answer a query.

| Status | When | Body |
|---|---|---|
| `200 OK` | every probe succeeded, or none is registered | `{"status":"ready"}` |
| `503 Service Unavailable` | at least one probe failed | `{"status":"not ready"}` |

Both answers carry `Cache-Control: no-cache`. The `503` body is this plain
status document, not an error envelope, and it never carries the probe's
error text. `GET /healthz` still answers `200` while
the hub is not ready.

## `GET /version`

The build of the running hub.

| Status | Body |
|---|---|
| `200 OK` | an object with exactly the members below |

The answer carries `Cache-Control: public, max-age=300`.

| Member | Value |
|---|---|
| `version` | the version the binary was built with, or `dev` when none was set |
| `commit` | the commit the binary was built from, or `unknown` when none was set |
| `build_time` | the build time as given at build time, or `unknown` when none was set |
| `go_version` | the version of the Go toolchain that built the binary, for example `go1.27.3` |
| `sandbox_runtime` | only on a hub with sandboxes on (a `[sandbox]` section, see [configuration.md](configuration.md#sandboxes)): an object of two strings, `supported`, the OpenShell releases the hub works with as `>=<lowest> <<first unsupported>`, and `connected`, the release the gateway reported when the hub started. It is read once, at start, and stays the same while the gateway cannot be reached |

Example:

```json
{"version":"1.4.0","commit":"3f2a1bc","build_time":"2026-01-02T03:04:05Z","go_version":"go1.27.3"}
```

With sandboxes on:

```json
{"version":"1.4.0","commit":"3f2a1bc","build_time":"2026-01-02T03:04:05Z","go_version":"go1.27.3","sandbox_runtime":{"supported":">=0.1.0 <0.2.0","connected":"0.1.8"}}
```

How the build values are set is in [architecture.md](architecture.md#build-information).

## `GET /api/v1/auth/providers`

The identity providers the hub is configured with, for a client that starts
a login. The route is public: it needs no credential and does not read the
`Authorization` header.

| Status | Body |
|---|---|
| `200 OK` | a [list](#lists) of the configured providers, ordered by name |

The answer carries `Cache-Control: public, max-age=300`. An item has exactly
these members:

| Member | Value |
|---|---|
| `name` | `github` or `google` |
| `authorize_url` | where the client sends the browser: the provider's authorize endpoint with the hub's `client_id` and the scopes the hub needs |

The authorize endpoint is the provider's `authorize_url` from the
configuration, or its public one (see
[configuration.md](configuration.md#oauth-providers)). Its query parameters
are `client_id` and `scope` (`user:email` for `github`, `openid email
profile` for `google`), and `response_type=code` for `google`. A client
secret is never part of the answer, in the body or a header. With no
provider configured, `items` is `[]`. The hub writes `&` in a JSON string
as `\u0026`, which decodes to `&`:

```json
{"items":[{"name":"github","authorize_url":"https://github.com/login/oauth/authorize?client_id=gh-id\u0026scope=user%3Aemail"},{"name":"google","authorize_url":"https://accounts.google.com/o/oauth2/v2/auth?client_id=gg-id\u0026response_type=code\u0026scope=openid+email+profile"}],"next_cursor":null}
```

## `POST /api/v1/auth/callback`

Signs a user in with an OAuth code from an identity provider and answers
the user with a new API key. The route is public: it needs no credential
and does not read the `Authorization` header. It is not a create route, so
an `Idempotency-Key` header is ignored.

The body is a JSON object:

| Member | Value |
|---|---|
| `provider` | required: a string naming a configured provider, such as `github` |
| `code` | required: the OAuth code the provider gave the client, a non-empty string |
| `redirect_uri` | required: the redirect URI the code was issued for (see below) |
| `expires` | the key's lifetime in days: the JSON integer `0`, `30`, `60` or `90`; `90` when absent or `null`, and a key of `0` days never expires |

The hub checks the body before it calls the provider and answers `400`
`invalid_request` when a required member is missing or empty, `provider`
is not a configured provider, `expires` is another number, a fraction or a
string (`45`, `-1`, `1.5`, `"90"`), or `redirect_uri` is not allowed. A
`redirect_uri` is allowed when it is an absolute URL without user
information and either has the scheme `http` and the host `localhost` or
`127.0.0.1`, with any port and path (the listener of `parley login`), or
has the scheme, host and port of `[server].external_url`, when that is set,
with the scheme's default port filled in (`https://hub.example.com/x` and
`https://hub.example.com:443/x` are the same). `https://localhost/x`,
`http://[::1]:5000/callback`, `http://localhost@evil.example/x` and a
relative URI are refused. An allowed `redirect_uri` is passed to the
provider unchanged.

The hub then exchanges the code at the provider and reads the user's
identity (provider, provider user id, username, full name, verified
e-mail), each call to the provider bounded by 15 seconds, then, in one
transaction, finds the user who signs in with that provider and provider
user id, or creates one at the first login, revokes the user's live API
keys and issues the user a new one. A login the hub refuses, before or
after the provider is called, changes nothing: no user, org, membership or
key is created and no key is revoked.

At a first login the hub creates:

- the user, with `status` `active`, the provider's username and e-mail, and
  the provider's display name as `full_name` (empty when it has none). When
  another user has the username, the first free of `<name>-2` to
  `<name>-10` is used instead. The role is `admin` when the e-mail equals
  the `admin_email` the hub was bootstrapped with, in any letter case, and
  no user with the role `admin` exists, whatever its status; it is `user`
  otherwise.
- the user's personal org: `status` `active`, named after the username and
  owned by the user, with a slug derived from the username: lowercased,
  every character outside `[a-z0-9_-]` replaced with `-`, runs of `-`
  collapsed, `-` and `_` trimmed from both ends; fewer than two characters
  left give `u-` and the first 8 characters of the user id, and a slug that
  starts with a digit gets the prefix `u-`. When an org has that slug, the
  first free of `<slug>-1` to `<slug>-10` is used. `Alice.Smith` gives
  `alice-smith`, `42bob` gives `u-42bob`.
- the user's membership of that org, and the API key.

At a later login of an `active` user the hub refreshes the user's `email`
and `username` from the provider and sets `updated_at` to the login time.
When another user has the new username, the old username is kept and the
login goes on. The user's `id`, `role`, `full_name`, `created_at` and
personal org (its name and slug included) stay as they are.

At every login, a first one included, the hub sets `revoked_at` to the
login time on every API key of the user that is neither revoked nor
expired, then issues the new key, so the user holds exactly one live key.
Those keys are answered `401` `credential revoked` from then on. A key
revoked earlier keeps its `revoked_at`, an expired key is left as it is,
and the user's PATs and the keys of other users are not touched.

| Status | When |
|---|---|
| `200 OK` | the user signed in; the body is `{"user": …, "api_key": …}` |
| `400` `invalid_request` | the body is not one the hub can use (see above); the provider is not called |
| `401` `unauthenticated` | the provider rejected the code: its token URL answered a JSON object with an `error` member and a status below 500. The message is `the identity provider rejected the code`. |
| `403` `forbidden` | the user is `blocked`; the message is `user is blocked`. Nothing changes: the user is not refreshed, no key is revoked and none is issued. |
| `409` `conflict` | a new user cannot be created: the e-mail is another user's, an org already has the username as its name, the username and all of `<name>-2` to `<name>-10` are taken, or the slug and all of `<slug>-1` to `<slug>-10` are taken; or, at a later login, the e-mail the provider now gives is another user's. Nothing is created or changed. |
| `502` `upstream_error` | the provider failed: it could not be reached, did not answer within 15 seconds, answered a status of 500 or above, something that is not a JSON object or no access token, or an identity without a user id or a verified e-mail. The message is `the identity provider failed`. |

The messages are fixed texts: they never hold the code, a client secret, an
access token or anything the provider answered.

`user` is the User object (`id`, `username`, `email`, `full_name`, `role`,
`status`, `provider`, `provider_id`, `created_at`, `updated_at`). `api_key`
has exactly `key`, the key `parley_<key_id>_<secret>`, shown this once;
`key_id`; and `expires_at`, the login time plus `expires` × 24 hours, or
`null` for a key that never expires:

```json
{"user":{"id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","username":"alice","email":"a@x","full_name":"Alice A","role":"user","status":"active","provider":"github","provider_id":"123","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"},"api_key":{"key":"parley_Ab3dE5gH_0123456789abcdefghijABCDEFGHIJKL","key_id":"Ab3dE5gH","expires_at":"2026-04-01T00:00:00Z"}}
```

## `GET /api/v1/user`

Answers the caller's own User object. The route needs a user: an API key or
a PAT acts for its user, and the admin token, which has none, is answered
`403` `forbidden` with the message `this route needs a user`. It declares
the scope `users:read` for a PAT. The user is the one authentication read
from the store for this request.

| Status | When |
|---|---|
| `200 OK` | the body is the caller's User object |
| `403` `forbidden` | the caller is the admin token (`this route needs a user`) |
| `403` `insufficient_scope` | a PAT without `users:read` or a scope that implies it (see [Scopes](#scopes)) |

The User object has exactly `id`, `username`, `email`, `full_name`, `role`,
`status`, `provider`, `provider_id`, `created_at` and `updated_at`;
`created_at` and `updated_at` are RFC 3339 in UTC with whole seconds:

```json
{"id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","username":"alice","email":"alice@example.com","full_name":"","role":"user","status":"active","provider":"github","provider_id":"alice","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}
```

## `PATCH /api/v1/user`

Sets the caller's own `full_name`. The route needs a user, as `GET
/api/v1/user` does, and declares the scope `users:write` for a PAT.

The body is a JSON object with the member `full_name`, required, a string;
the empty string clears the name. Other members are ignored. The hub stores
the string, sets `updated_at` to the time of the change and answers the
updated User object; every other member is unchanged.

| Status | When |
|---|---|
| `200 OK` | the name was stored; the body is the updated User object |
| `400` `invalid_request` | `full_name` is missing (message `full_name is required`), or is not a JSON string, `null` included (message `full_name must be a string`); or the body is not a JSON object. The user is unchanged. |
| `403` `forbidden` | the caller is the admin token (`this route needs a user`) |
| `403` `insufficient_scope` | a PAT without `users:write` or a scope that implies it (see [Scopes](#scopes)) |

```json
{"full_name":"Alice A."}
```

## `GET /api/v1/user/orgs`

Lists the orgs the caller is a member of, a [list](#lists) in the standard
pages. The route needs a user, as `GET /api/v1/user` does, and declares the
scope `orgs:read` for a PAT.

The list holds the orgs with the status `active` of which the caller's user
is a member, ordered by `name` ascending, compared byte by byte. Blocked
orgs and orgs the user is not a member of are left out. A user with no
membership in an active org gets `items` `[]`.

| Status | When |
|---|---|
| `200 OK` | the body is a page: `items` are Org objects (see [`POST /api/v1/orgs`](#post-apiv1orgs)) |
| `400` `invalid_request` | a bad `limit` or `cursor` (see [Lists](#lists)) |
| `403` `forbidden` | the caller is the admin token (`this route needs a user`) |
| `403` `insufficient_scope` | a PAT without `orgs:read` or a scope that implies it (see [Scopes](#scopes)) |

## `GET /api/v1/user/keys`

Lists the caller's API keys, a [list](#lists) in the standard pages. The
route needs a user, as `GET /api/v1/user` does, and declares the scope
`keys:read` for a PAT, which `keys:manage` satisfies.

The list holds every API key of the caller's user in any state: live,
revoked and expired. It is ordered newest first: `created_at` descending,
then `key_id` descending. Each item is a Key object, which has exactly
`key_id`, `user_id`, `expires_at`, `revoked_at` and `created_at`, with
`null` for what is unset; the times are RFC 3339 in UTC with whole seconds.
No Key object carries the key's value or its secret.

```json
{"key_id":"Ab3dE5gH","user_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","expires_at":"2026-04-01T00:00:00Z","revoked_at":null,"created_at":"2026-01-01T00:00:00Z"}
```

| Status | When |
|---|---|
| `200 OK` | the body is a page: `items` are Key objects |
| `400` `invalid_request` | a bad `limit` or `cursor` (see [Lists](#lists)) |
| `403` `forbidden` | the caller is the admin token (`this route needs a user`) |
| `403` `insufficient_scope` | a PAT without `keys:read` or `keys:manage` (`this route needs the scope keys:read`) |

## `POST /api/v1/user/keys/{key_id}/refresh`

Gives one of the caller's live API keys a new secret without a new login.
The key keeps its `key_id`; its new secret is 32 characters from
`[0-9A-Za-z]`, of which only a hash is stored, and its `expires_at` is
`expires_days` × 24 hours after the time of the request, counted from its
whole second, or `null` for a key that never expires. The old secret no
longer authenticates from the next request on. The body is ignored. The
route is not a create route: an `Idempotency-Key` is ignored, and a second
request refreshes again.

Only an API key may refresh. The route needs a user, so the admin token is
refused by authentication, and a PAT, whatever its scopes, is answered
`403` `forbidden` `this route needs an API key` before the key is looked
up. A `key_id` that names no key and one that names another user's key get
the same answer, `404` `not_found` with the message `API key not found`, so
the answer tells nothing of other users' keys; every route under
`/api/v1/user/keys/{key_id}` looks the key up this way. A revoked key, and
one whose `expires_at` has come, cannot be refreshed. A refused request
changes nothing.

The answer has exactly `key`, the new value `parley_<key_id>_<secret>`,
shown this once; `key_id`; and `expires_at`:

```json
{"key":"parley_Ab3dE5gH_0123456789abcdefghijABCDEFGHIJKL","key_id":"Ab3dE5gH","expires_at":"2026-05-01T00:00:00Z"}
```

| Status | When |
|---|---|
| `200 OK` | the key has a new secret; the body is `key`, `key_id` and `expires_at` |
| `403` `forbidden` | the caller is the admin token (`this route needs a user`) or a PAT (`this route needs an API key`) |
| `404` `not_found` | the caller's user has no key with this id (`API key not found`) |
| `409` `conflict` | the key is revoked (`API key is revoked`) or expired (`API key is expired`) |

## `DELETE /api/v1/user/keys/{key_id}`

Revokes one of the caller's API keys: its `revoked_at` is set to the time
of the request and the record is kept, so the key stays in the list and no
longer authenticates. An expired key is revoked too, and so is the key the
request came with. The route needs a user and declares the scope
`keys:manage` for a PAT, checked before the key is looked up. A key that is
already revoked is answered as it is, with its earlier `revoked_at`;
nothing changes. A key not found is answered as for
[`POST /api/v1/user/keys/{key_id}/refresh`](#post-apiv1userkeyskey_idrefresh).

| Status | When |
|---|---|
| `200 OK` | the key is revoked; the body is the Key object with `revoked_at` set |
| `403` `forbidden` | the caller is the admin token (`this route needs a user`) |
| `403` `insufficient_scope` | a PAT without `keys:manage` (`this route needs the scope keys:manage`) |
| `404` `not_found` | the caller's user has no key with this id (`API key not found`) |

## `POST /api/v1/user/tokens`

Mints a personal access token (PAT) owned by the caller's user. The route
needs a user, as `GET /api/v1/user` does, so an API key or a PAT may call
it, and declares the scope `tokens:manage` for a PAT: a PAT without it is
refused before the body is read, also when the body is invalid. It is a
create route (see [Idempotency](#idempotency)).

The body is a JSON object; other members are ignored:

| Member | Required | Value |
|---|---|---|
| `name` | yes | a string of 1 to 255 characters |
| `permissions` | yes | an array of at least one registered scope (see [Scopes](#scopes)), matched exactly; a duplicate is dropped and the order kept |
| `paths` | no | `null`, or an array of 1 to 50 globs, each a string of 1 to 255 characters that does not start with `/`; stored in the order given, duplicates kept. Absent or `null`, the token has no path limits. |
| `expires` | no | the lifetime in days, one of the integers `0`, `30`, `60` and `90`; `0` never expires; absent, `90` |

A token's `paths` limits the files its writes may touch. In a glob, `*`
matches any run of characters without `/`, `**` any run including `/`,
both possibly empty, and every other character itself, case-sensitively;
the glob must match the whole repository path. `docs/**` matches
`docs/a.md` and `docs/api/b.md` but not `docs`; `src/*.go` matches
`src/main.go` but not `src/x/y.go`. A token without `paths` may write any
path.

The token gets a new `token_id` of 8 and a secret of 32 characters from
`[a-z0-9]`; only a hash of the secret is stored. `expires_at` is `expires` ×
24 hours after the time of the request, counted from its whole second, and
from that instant on the token no longer authenticates; it is `null` for
`0`. `workspace` is `null`: a token minted here is not bound to a
workspace.

A PAT caller is held to the subset rule: it can mint a token only within
what it holds itself, so that a token never creates a stronger one. Each
requested permission must be in the caller's own `permissions`, compared
exactly, without the implication rules of [Scopes](#scopes): a PAT with
`git:write` cannot mint one with `git:read`. A caller with `paths` must
send `paths` (absent or `null` is refused), and each requested glob must be
covered by one of the caller's globs: the caller's glob matches every path
the requested one can match, where a `*` of the caller's glob covers no `/`
and no `**` (`src/**` covers `src/api/*.go` and `src/**`; `docs/*` does not
cover `docs/**`). A caller without `paths` may ask for any `paths` or none.
The rule runs after the body is validated, so a body that is invalid and
also asks for more than the caller holds is answered `400`. An API key is
not held to it: it may mint any registered scopes and any valid `paths`.

| Status | When |
|---|---|
| `201 Created` | the token was minted; the body is the Token object and `token` |
| `200 OK` | a replay of a recorded `Idempotency-Key` with the same body: the first answer without `token`; nothing is minted |
| `400` `invalid_request` | a member is missing, of another type or out of range; nothing is minted. The messages: `name must be a string`, `name must be 1 to 255 characters`, `permissions must be an array of strings`, `permissions must hold at least one scope`, `permissions: "git:admin" is not a registered scope`, `paths must be an array of strings`, `paths must hold at least one glob; omit it for no path limit`, `paths holds 51 globs, at most 50 are allowed`, `paths[0] is empty`, `paths[0] is longer than 255 characters`, `paths[0] "/src/**" starts with /; globs are relative to the repository root`, `expires must be one of 0, 30, 60 and 90` |
| `403` `forbidden` | the caller is the admin token (`this route needs a user`) |
| `403` `insufficient_scope` | a PAT without `tokens:manage` (`this route needs the scope tokens:manage`) |
| `403` `insufficient_scope` | a valid body beyond what the calling PAT holds; nothing is minted. The message names the first requested permission, in request order, the PAT does not hold (`permissions: this token does not hold "git:read"`); when it holds them all, the first requested glob none of its globs covers (`paths: "lib/**" is not covered by a glob of this token`), or, for a PAT with `paths` and a body without, `paths is required: this token is limited to paths, so the token it creates must be too` |

```json
{"name":"ci","permissions":["git:write","changes:write"],"paths":["docs/**","src/*.go"],"expires":90}
```

The Token object has exactly `token_id`, `user_id`, `name`, `permissions`,
`paths`, `workspace`, `expires_at`, `revoked_at` and `created_at`, with
`null` for what is unset; the times are RFC 3339 in UTC with whole seconds.
The answer that minted the token adds `token`, the value
`parley_pat_<token_id>_<secret>`, shown this once: no other answer carries
it, a replay included.

```json
{"token_id":"k3x9m2pq","user_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","name":"ci","permissions":["git:write","changes:write"],"paths":["docs/**","src/*.go"],"workspace":null,"expires_at":"2026-04-01T00:00:00Z","revoked_at":null,"created_at":"2026-01-01T00:00:00Z","token":"parley_pat_k3x9m2pq_0123456789abcdefghijklmnopqrstuv"}
```

## `GET /api/v1/user/tokens`

Lists the caller's personal access tokens, a [list](#lists) in the standard
pages. The route needs a user, as `GET /api/v1/user` does, and declares the
scope `tokens:read` for a PAT, which `tokens:write` and `tokens:manage`
satisfy.

The list holds every PAT of the caller's user in any state: live, revoked
and expired, and the tokens bound to a workspace too. It is ordered newest
first: `created_at` descending, then `token_id` descending. Each item is a
Token object (see [`POST /api/v1/user/tokens`](#post-apiv1usertokens)),
without the token's value.

| Status | When |
|---|---|
| `200 OK` | the body is a page: `items` are Token objects |
| `400` `invalid_request` | a bad `limit` or `cursor` (see [Lists](#lists)) |
| `403` `forbidden` | the caller is the admin token (`this route needs a user`) |
| `403` `insufficient_scope` | a PAT without `tokens:read` or a scope that implies it (`this route needs the scope tokens:read`) |

## `GET /api/v1/user/tokens/{token_id}`

Answers one of the caller's PATs, in any state, as a Token object without
the token's value. The route needs a user and declares the scope
`tokens:read` for a PAT, checked before the token is looked up, so a
refused PAT is answered alike for a token that exists and one that does
not.

A `token_id` that names no token and one that names another user's token
get the same answer, `404` `not_found` with the message `token not found`,
so the answer tells nothing of other users' tokens. Every route under
`/api/v1/user/tokens/{token_id}` looks the token up this way.

```json
{"error":{"code":404,"message":"token not found","error_type":"not_found"}}
```

| Status | When |
|---|---|
| `200 OK` | the body is the Token object |
| `403` `forbidden` | the caller is the admin token (`this route needs a user`) |
| `403` `insufficient_scope` | a PAT without `tokens:read` or a scope that implies it (`this route needs the scope tokens:read`) |
| `404` `not_found` | the caller's user has no token with this id (`token not found`) |

## `DELETE /api/v1/user/tokens/{token_id}`

Revokes one of the caller's PATs: its `revoked_at` is set to the time of
the request and the record is kept, so the token stays in the list and no
longer authenticates. An expired token is revoked too, and so is the token
the request came with. The route needs a user and declares the scope
`tokens:manage` for a PAT, checked before the token is looked up. A token
that is already revoked is answered as it is, with its earlier
`revoked_at`; nothing changes. A token not found is answered as for
`GET /api/v1/user/tokens/{token_id}`.

| Status | When |
|---|---|
| `200 OK` | the token is revoked; the body is the Token object with `revoked_at` set |
| `403` `forbidden` | the caller is the admin token (`this route needs a user`) |
| `403` `insufficient_scope` | a PAT without `tokens:manage` (`this route needs the scope tokens:manage`) |
| `404` `not_found` | the caller's user has no token with this id (`token not found`) |

## `PUT`, `PATCH`, `DELETE /api/v1/user/tokens/{token_id}/permissions`

Changes the permissions of one of the caller's PATs without issuing a new
value: `PUT` replaces them with the list given, a duplicate dropped and the
order kept; `PATCH` appends the scopes the token does not have yet, in the
order given, after the ones it has; `DELETE` removes the scopes given and
ignores every string the token does not have, registered or not. From the
next request on, the token is scope-checked against its new permissions.
The routes need a user and declare the scope `tokens:write` for a PAT,
which `tokens:manage` satisfies, checked before the token is looked up.

The body is a JSON object with the member `permissions`, an array of
strings; other members are ignored, `paths` and `workspace` among them. A
change never alters the token's `paths`, `workspace`, `name` or
`expires_at`.

```json
{"permissions":["git:read","changes:write"]}
```

| Method | `permissions` |
|---|---|
| `PUT` | registered scopes; may be empty |
| `PATCH` | at least one registered scope |
| `DELETE` | at least one string |

A change that leaves the token with no scope, a `PUT` with `[]` or a
`DELETE` of every scope it has, stores `permissions` `[]` and revokes the
token in the same transaction: `revoked_at` is the time of the request, and
the token no longer authenticates.

A PAT caller is held to the subset rule (see
[`POST /api/v1/user/tokens`](#post-apiv1usertokens)) for the scopes the
change adds, those of the new list the token does not have now: each must
be in the caller's own `permissions`, compared exactly. A scope the token
keeps, or one `DELETE` removes, is not checked; a PAT can therefore not
widen itself either. When the change adds a scope and the caller is
limited to paths, the token's own `paths` must be covered as well: a token
without `paths`, or with a glob none of the caller's globs covers, is
refused, so that a PAT cannot give a scope to a token that would use it
beyond the caller's paths. An API key is not held to the rule.

The checks run in this order, the first that fails answering: the token is
looked up (`404`, as for
`GET /api/v1/user/tokens/{token_id}`), the body is validated (`400`), the
subset rule (`403`), then the token's state (`409`): a revoked token, and
one whose `expires_at` has come, the instant from which it no longer
authenticates, cannot be changed. A refused request changes nothing.

| Status | When |
|---|---|
| `200 OK` | the permissions are changed; the body is the Token object after the change |
| `400` `invalid_request` | the body has no `permissions` or not an array of strings (`permissions must be an array of strings`), a `PUT` or `PATCH` names a string that is not a registered scope (`permissions: "git:admin" is not a registered scope`), or a `PATCH` or `DELETE` sends `[]` (`permissions must hold at least one scope`) |
| `403` `forbidden` | the caller is the admin token (`this route needs a user`) |
| `403` `insufficient_scope` | a PAT without `tokens:write` or `tokens:manage` (`this route needs the scope tokens:write`) |
| `403` `insufficient_scope` | a `PUT` or `PATCH` by a PAT that adds a scope it does not hold; the message names the first one (`permissions: this token does not hold "git:read"`) |
| `403` `insufficient_scope` | a `PUT` or `PATCH` that adds a scope, by a PAT limited to paths, to a token without `paths` (`paths: this token is limited to paths, so it cannot add scopes to a token that is not`) or with a glob none of the caller's globs covers; the message names the first one (`paths: "src/**" is not covered by a glob of this token`) |
| `404` `not_found` | the caller's user has no token with this id (`token not found`) |
| `409` `conflict` | the token is revoked (`token is revoked`) or expired (`token is expired`) |

## Workspace settings

Every workspace object holds `settings`, a flat object with exactly these
21 keys, each with its effective value: the value stored for the key when
it was set, otherwise its default. Only the keys set are stored.

| Key | Type | Values | Default |
|---|---|---|---|
| `sync_enabled` | bool | | `true` |
| `protected_branches` | list of strings | 0 to 50 distinct branch globs | `[<default_branch>]`, `[]` while `default_branch` is `null` |
| `protect.owner_push` | bool | | `true` |
| `land.strategy` | enum | `rebase`, `squash` | `rebase` |
| `land.by_author` | bool | | `true` |
| `land.delete_source` | bool | | `true` |
| `land.allow_override` | bool | | `true` |
| `changes.require_intent` | bool | | `true` |
| `changes.auto_restack` | bool | | `true` |
| `audit_fetches` | bool | | `false` |
| `forge_api_url` | URL string | `""` or an absolute URL starting with exactly `https://` (lower case), with a host, a port only if from 1 to 65535, and without user information, query, fragment, white space or control characters | `""` |
| `upstream_forge_api_url` | URL string | the same | `""` |
| `patch_branch_source` | enum | `hub`, `origin` | `hub` |
| `patch_divergence_policy` | enum | `replace`, `report` | `replace` |
| `push_patches_to_origin` | bool | | `false` |
| `auto_rebuild_after_push` | bool | | `true` |
| `auto_rebuild_after_sync` | bool | | `true` |
| `rebuild_strategy` | enum | `rebase`, `merge` | `rebase` |
| `rebuild_fail_mode` | enum | `fail_fast`, `continue` | `fail_fast` |
| `rebuild_push_integration_branch` | bool | | `false` |
| `squash_merge_detection` | enum | `ancestry_only`, `content_based`, `both` | `both` |

A bool key is a JSON boolean, an enum or URL key a JSON string and
`protected_branches` a JSON array of strings, in the order given. A URL key
is judged and stored without the white space around it, so
`"  https://ghe.example/api/v3  "` is stored as
`"https://ghe.example/api/v3"` and `"   "` as `""`. The default of
`protected_branches` follows the workspace's `default_branch`; once the key
is set, the empty list included, the stored list is shown as it is.

A task fork has no settings of its own: its `settings` are its parent's
effective settings, read when the fork is answered, so a change of the
parent's settings shows in the fork at once.

A branch glob is 1 to 255 characters that make a valid branch name once
`*` is allowed too. It matches a whole branch name, case-sensitive: `*`
matches any run of characters except `/`, `**` any run including `/`, and
every other character, `?` included, only itself. So `release/*` matches
`release/1.x` but not `release/1/x` or `release`, and `release/**` matches
`release/1/x`.

## `POST /api/v1/workspaces`

Registers a git repository as a workspace owned by the caller's user. It
takes an API key or a PAT and declares the scope `workspaces:write` for a
PAT. It is a create route (see [Idempotency](#idempotency)). It names no
workspace, so a PAT bound to a workspace is refused before the scope check
(see [Workspace-bound tokens](#workspace-bound-tokens)).

The body is a JSON object; members the route does not read are ignored, and
a member that the table allows to be `null` counts as absent when it is:

| Member | Required | Value |
|---|---|---|
| `slug` | yes | a string of 3 to 64 characters of `a` to `z`, digits and hyphens, starting with a letter, ending with a letter or digit, without two hyphens in a row; unique among all workspaces |
| `git_url` | yes | a string `https://<host>/<path>` without user information, or `git@<host>:<path>` |
| `default_branch` | no | a string or `null`; a valid git branch name, stored as given; absent, `null` until the clone takes it from origin |
| `org_id` | no | a string or `null`; an organization id or slug, which the workspace is recorded in; absent, the caller's personal organization |
| `display_name` | no | a string or `null` of at most 128 characters (Unicode code points); absent or empty, the slug |
| `description` | no | a string or `null` of at most 1024 characters (Unicode code points); absent, the empty string |
| `workspace_mode` | no | a string or `null`: `standard` or `carry_patch`; absent, `null` or empty, `standard` |
| `upstream_url` | with `carry_patch` | a string or `null`; required with `carry_patch` and checked by the `git_url` rule; with `standard` only `null` is accepted |
| `integration_branch` | no | a string or `null`; with `carry_patch` a valid branch name, stored as given (`null` when absent); with `standard` only `null` is accepted |
| `settings` | no | an object or `null`: each key one of the [workspace settings](#workspace-settings) with a value its rule accepts, a key with the value `null` keeping its default; only the keys set are stored. Absent or `null`, every key keeps its default |
| `git_pat` | no | a string, not empty; not with `git_username` or `git_password` |
| `git_username`, `git_password` | no | strings, not empty, given together or not at all |

No credential member may be given with a `git_url` of the form
`git@<host>:<path>`. A credential is checked against origin before anything
is stored: the hub lists the refs of `git_url` with it, a token presented
with the user name `x-token-auth`, and gives that listing 10 seconds. The
credential is presented to the scheme, host and port of `git_url` alone, at
this check and at every clone: when origin redirects git to another host,
port or scheme, it is not presented there. A request without a credential
does not contact origin. The credential is
stored as workspace secrets of the new slug, `GIT_PAT` for `git_pat`, or
`GIT_USERNAME` and `GIT_PASSWORD`, in the same transaction as the workspace,
and appears in no answer, event or log line.

```json
{"slug":"demo","git_url":"https://example.com/a.git"}
```

The checks run in this order, the first that fails answering, and a refused
request records nothing: a bound PAT (`403` `token_bound`); a PAT without
`workspaces:write` (`403`); the admin token (`403`), before the body is
read, so it gets the same answer for a valid and an invalid body; the body,
member by member in the order of the table, the first member refused named
in the message (`400`); the organization, the one `org_id` names or the
caller's personal org, which must exist (`400`) and of which the caller must
be an active member (`403`); a slug that a workspace already has, in any
status (`409`); then, for a request with a credential, a hub without a
secret encryption key (`503`), before origin is contacted; and the check of
the credential against origin (`400`). Nothing contacts origin before all
the others pass.

The workspace is recorded with `kind` `repository`, the `workspace_mode`,
`upstream_url` and `integration_branch` given, `status` `pending`, the
caller as `owner_id`, the chosen org as `org_id`, `created_at` equal to
`updated_at`, and a settings document holding the keys `settings` set. A workspace in `pending` is its own clone work.
The workspace, its credential secrets and a `parley.workspace.create`
event are written in one store transaction: a failure of any of them, or of
the commit, leaves none of them and is answered `500` `internal` without
its cause. When two requests race
for one slug, one is answered `201` and the other `409`.

The answer is the workspace object: every field of the workspace, a field
without a value written as `null` and never left out, and `org_slug`, the
slug of its org; `hub_url`, the workspace's git remote on the hub,
`<[server].external_url>/git/<org_slug>/<slug>.git` with one trailing `/`
of the external URL removed, or `null` when no external URL is configured;
`access`, the caller's level (`owner` for the creator); and `settings`,
all 21 setting keys with their effective values (see
[Workspace settings](#workspace-settings)).

```json
{"slug":"demo","kind":"repository","workspace_mode":"standard","git_url":"https://example.com/a.git","default_branch":null,"owner_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","org_id":"5e6f7a8b-9c0d-4e1f-8a2b-3c4d5e6f7a8b","org_slug":"octocat","status":"pending","display_name":"demo","description":"","head_sha":null,"origin_head_sha":null,"clone_error":null,"last_sync_at":null,"sync_error":null,"upstream_url":null,"integration_branch":null,"upstream_head_sha":null,"parent_slug":null,"fork_point":null,"expires_at":null,"auto_change":null,"purpose":null,"session_id":null,"settings":{"sync_enabled":true,"protected_branches":[],"protect.owner_push":true,"land.strategy":"rebase","land.by_author":true,"land.delete_source":true,"land.allow_override":true,"changes.require_intent":true,"changes.auto_restack":true,"audit_fetches":false,"forge_api_url":"","upstream_forge_api_url":"","patch_branch_source":"hub","patch_divergence_policy":"replace","push_patches_to_origin":false,"auto_rebuild_after_push":true,"auto_rebuild_after_sync":true,"rebuild_strategy":"rebase","rebuild_fail_mode":"fail_fast","rebuild_push_integration_branch":false,"squash_merge_detection":"both"},"hub_url":"https://hub.example/git/octocat/demo.git","access":"owner","created_at":"2026-11-14T10:30:15Z","updated_at":"2026-11-14T10:30:15Z"}
```

| Status | When |
|---|---|
| `201 Created` | the workspace was recorded; the body is the workspace object |
| `200 OK` | a replay of a recorded `Idempotency-Key` with the same body: the first answer; nothing is recorded |
| `400` `invalid_request` | the body is refused, the message naming the member: `slug: is required`, `slug: must be a string` (also for `null`), `slug: must start with a letter` and the other slug rules, the same for `git_url` with its rules; `default_branch: must be a string or null` (the same for `org_id`, `display_name`, `description`, `workspace_mode`, `upstream_url` and `integration_branch`), `default_branch: must not be empty` and the other branch-name rules (the same for `integration_branch`), `display_name: must be at most 128 characters`, `description: must be at most 1024 characters`; `workspace_mode: must be "standard" or "carry_patch"`, `upstream_url: is required with workspace_mode carry_patch`, `upstream_url: is accepted only with workspace_mode carry_patch` (the same for `integration_branch`), an `upstream_url:` rule of `git_url`; `settings: must be an object or null`, `unknown setting "<key>"`, `settings.<key>: <reason>` for a value its rule refuses; `git_pat: must be a string` (also for `null`, the same for `git_username` and `git_password`), `git_pat: must not be empty`, `git_pat: must not be given with a git_url of the form git@<host>:<path>`, `git_username: must not be given with git_pat`, `git_password: must not be given with git_pat`, `git_password: is required with git_username`, `git_password: is accepted only with git_username`; or an organization that does not exist (`org_id: no organization has the id or slug "acme"`, or `org_id: the caller has no personal organization`) |
| `403` `token_bound` | a PAT bound to a workspace |
| `403` `insufficient_scope` | a PAT without `workspaces:write` (`this route needs the scope workspaces:write`) |
| `403` `forbidden` | the admin token (`a workspace needs a user as owner; the admin token cannot create one`), or a caller who is not an active member of the org `org_id` names, or of their personal org (`the caller is not an active member of the organization`) |
| `400` `credential_check_failed` | origin refused the credential, could not be reached, or did not answer within 10 seconds (`the credential was refused or origin could not be reached`, whatever the cause; git's own message is logged, scrubbed of the credential, as `workspace credential check refused`) |
| `409` `conflict` | a workspace has the slug, whatever its status (`slug: a workspace with the slug "demo" exists`) |
| `413` `payload_too_large` | the body is over the hub's body limit (see [Request bodies](#request-bodies)) |
| `500` `internal` | writing the workspace, its credential secrets or its event failed; nothing is recorded |
| `503` `secrets_unavailable` | the request carries a credential and the hub has no secret encryption key configured (`secrets cannot be stored: no encryption key configured`); origin is not contacted |

The event has `workspace` and `resource_id` the slug, `resource_type`
`workspace`, `actor_type` `api_key` or `pat` after the calling credential,
`actor_id` the caller's user, and in `metadata` the workspace's `kind`,
`git_url`, `workspace_mode` and `default_branch` (`null` when none was
given):

```json
{"seq":12,"id":"…","type":"parley.workspace.create","time":"2026-11-14T10:30:15Z","workspace":"demo","actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","resource_type":"workspace","resource_id":"demo","action":"create","session_id":null,"metadata":{"default_branch":null,"git_url":"https://example.com/a.git","kind":"repository","workspace_mode":"standard"}}
```

## `GET /api/v1/workspaces`

Lists the workspaces the caller can read, in every status but `archived`,
each as the workspace object `GET /api/v1/workspaces/{slug}` answers, with
`access` the caller's level on that workspace. It takes the admin token, an
API key or a PAT, and declares the scope `workspaces:read` for a PAT, which
a PAT holding `workspaces:write` also satisfies. It names no workspace, so
no access check runs before it; the list itself holds only the workspaces
the caller can read (see [Workspace access](#workspace-access)):

| Caller | Listed, with `access` |
|---|---|
| the admin token | every workspace, `admin` |
| a user | the workspaces they own and the task forks of those, `owner`; the workspaces of their active orgs that they do not own, `member` |
| a PAT bound to a workspace | what its user's list holds of the workspaces its binding covers (see [Workspace-bound tokens](#workspace-bound-tokens)) |

It is a list (see [Lists](#lists)), with `limit` and `cursor` and these
query parameters:

| Parameter | Meaning |
|---|---|
| `include_archived` | `true` lists archived workspaces too; `false` or absent leaves them out |
| `include_forks` | `true` lists task forks too; `false` or absent leaves them out |
| `parent` | lists only the task forks whose `parent_slug` is this slug, whatever `include_forks` says; the list is empty when no workspace has the slug, when the caller cannot read it, or when it is a task fork |

The order is `created_at` descending, then `slug` ascending among the
workspaces created in the same instant; the cursor carries both, so a walk
over the pages returns each listed workspace once.

| Status | When |
|---|---|
| `200 OK` | the body is a page, `items` an array of workspace objects (`[]` when nothing matches) |
| `400` `invalid_request` | `include_archived` or `include_forks` with a value other than exactly `true` or `false`, an empty one included, or given more than once (`include_forks must be given once, as true or false`); a bad `limit` or `cursor` (see [Lists](#lists)) |
| `403` `insufficient_scope` | a PAT with neither `workspaces:read` nor `workspaces:write` (`this route needs the scope workspaces:read`) |

## `GET /api/v1/workspaces/{slug}`

Answers the workspace `{slug}` to a caller who can read it, in every status
(`pending`, `cloning`, `ready`, `failed`, `archived`) and for a repository
workspace and a task fork alike. It takes the admin token, an API key or a
PAT, and declares the scope `workspaces:read` for a PAT, which a PAT holding
`workspaces:write` also satisfies. It needs read access to the workspace
(see [Workspace access](#workspace-access)).

The checks run in this order: a bound PAT on a workspace outside its
binding (`404`, see [Workspace-bound tokens](#workspace-bound-tokens)); a
PAT with neither `workspaces:read` nor `workspaces:write` (`403`), before
the workspace is looked up, so the answer is the same for a slug a
workspace has and for one no workspace has; then the access check, which
answers a caller who cannot read the workspace with the same status and
the same bytes as a slug no workspace has, archived or not.

The answer is the workspace object, the same object `POST
/api/v1/workspaces` answers (see there), with `access` the caller's level on
the workspace: `admin` for the admin token, `owner` for its owner, `member`
for an active member of its org, whatever scopes the caller's PAT holds.

| Status | When |
|---|---|
| `200 OK` | the body is the workspace object |
| `403` `insufficient_scope` | a PAT with neither `workspaces:read` nor `workspaces:write` (`this route needs the scope workspaces:read`) |
| `404` `not_found` | the caller cannot read the workspace, or no workspace has the slug (`workspace not found`); also a bound PAT on a workspace outside its binding |

```json
{"error":{"code":404,"message":"workspace not found","error_type":"not_found"}}
```

## `PATCH /api/v1/workspaces/{slug}`

Changes the display name, the description, the organization and the
settings of the workspace `{slug}`. It takes the admin token, an API key or a PAT, declares
the scope `workspaces:write` for a PAT and needs write access to the
workspace (see [Workspace access](#workspace-access)): its owner or the
admin token. It works in every status but `archived`.

The body is a JSON object that changes exactly the fields it carries;
members the route does not know are ignored, and a field not given keeps
its value.

| Field | Repository workspace | Task fork |
|---|---|---|
| `display_name` | `null` or a string of at most 128 characters (Unicode code points); `null` or `""` sets it to the slug | the same |
| `description` | `null` or a string of at most 1024 characters; `null` sets it to `""` | the same |
| `org_id` | an org id or slug, or `null` for the personal org of the workspace's owner; the workspace and every fork of it move to that org | `400` `not_supported_on_fork` |
| `settings` | an object or `null`, applied to the stored settings key by key (below) | `400` `not_supported_on_fork` |
| `ttl`, `purpose`, `auto_change` | `400` `invalid_request` | `400` `invalid_request` until the fork features accept them |
| `slug`, `kind`, `git_url`, `default_branch`, `owner_id`, `workspace_mode`, `upstream_url`, `integration_branch`, `parent_slug`, `fork_point` | `400` `invalid_request`, whatever the value, the current one included | the same |

In `settings`, a key given with a value its rule accepts is stored with that
value (see [Workspace settings](#workspace-settings)), a key given with
`null` returns to its default, and every key not given keeps its value;
`"settings": null` returns every key to its default. The settings are
applied all or nothing: when one key is refused, nothing is stored.

`org_id` names the org by its id (a value in UUID form) or else by its
slug, exactly; the answer's `org_id` is the org's id either way, with its
`org_slug` and a `hub_url` under the new org. The workspace's owner, not
the caller, must be an active member of that org, also when the admin
token moves it. The workspace moves with every task fork whose
`parent_slug` it is, in the same store transaction: all of them move or
none does. Who can read the workspace and its forks follows: the members
of the new org gain read access and those of the previous org lose it.

```json
{"display_name":"Payments","settings":{"land.strategy":"squash","protected_branches":["main","release/**"]}}
```

The checks run in this order, the first that fails answering: a bound PAT
on a workspace outside its binding (`404`, see
[Workspace-bound tokens](#workspace-bound-tokens)); a PAT without
`workspaces:write` (`403`), before the workspace is looked up; the access
check (`404` for a caller who cannot read the workspace, the same bytes as
for a slug no workspace has, `403` for a member of its org, also on an
archived workspace); then the body: not a JSON object; an immutable field;
a field that does not apply to the workspace's kind; no field to change;
the values in the order `display_name`, `description`, `org_id`,
`settings`; an `org_id` naming an org of which the workspace's owner is
not an active member (`403`); and last the state: an archived workspace is
`409`.
Of several refused setting keys the message names one: an unknown key
before any refused value, the first unknown key in byte order of the keys,
otherwise the refused key that comes first in the table of
[Workspace settings](#workspace-settings), whatever the order of the
members in the body. The workspace is read, changed and written with
`updated_at`, the current time cut to the second, in one store transaction;
an update that changes no value still sets `updated_at`, and a refused
request changes nothing, `updated_at` included.

Every update that is answered `200` writes one `parley.workspace.update`
event in the same store transaction, also one that changes no value; a
refused request writes none, and when the event cannot be written the
update is not made and is answered `500` `internal`. The event has
`workspace` and `resource_id` the slug, `resource_type` `workspace`,
`actor_type` `admin_token` with `actor_id` `null` for the admin token, or
`api_key` or `pat` after the calling credential with `actor_id` the
caller's user, and in `metadata` `fields`, the names of the mutable fields
the body carried in the order `display_name`, `description`, `org_id`,
`settings`, `ttl`, `purpose`, `auto_change`, and for a task fork
`parent_slug`, its parent's slug. The metadata holds no value of any
field, so no setting appears in the log:

```json
{"seq":13,"id":"…","type":"parley.workspace.update","time":"2026-11-14T10:31:02Z","workspace":"demo","actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","resource_type":"workspace","resource_id":"demo","action":"update","session_id":null,"metadata":{"fields":["display_name","settings"]}}
```

The answer is the workspace object after the update, the object
[`GET /api/v1/workspaces/{slug}`](#get-apiv1workspacesslug) answers, with
`access` the caller's level.

| Status | When |
|---|---|
| `200 OK` | the fields are changed; the body is the workspace object |
| `400` `invalid_request` | the body is not a JSON object; it carries an immutable field (`<field>: cannot be changed`), a fork-only field on a repository workspace (`<field>: applies only to a task fork`) or on a fork that does not accept it (`<field>: cannot be changed`); it carries no field to change (`the body carries no field to change`); or a value is refused: `display_name: must be a string or null`, `display_name: must be at most 128 characters`, the same for `description` with 1024, `org_id: must be a string or null`, `org_id: no organization has the id or slug "<value>"` (also for `""` and an id no org has), `org_id: the workspace's owner has no personal organization`, `settings: must be an object or null`, `unknown setting "<key>"`, or `settings.<key>: <reason>` for a value its rule refuses (`settings.land.strategy: must be one of ["rebase" "squash"]`); the reason never repeats a URL |
| `400` `not_supported_on_fork` | `org_id` or `settings` in an update of a task fork, whatever the value (`<field>: a task fork takes its org and its settings from its parent`) |
| `403` `insufficient_scope` | a PAT without `workspaces:write` (`this route needs the scope workspaces:write`) |
| `403` `forbidden` | a member of the workspace's org who does not own it (`org members have read-only access to this workspace`), in every status; or an `org_id` naming an org of which the workspace's owner is not an active member, whoever the caller is (`the workspace's owner is not an active member of the organization`) |
| `404` `not_found` | the caller cannot read the workspace, or no workspace has the slug (`workspace not found`); also a bound PAT on a workspace outside its binding |
| `409` `workspace_archived` | a valid update of a workspace in status `archived` (`workspace <slug> is archived`) |
| `500` `internal` | a write failed, that of a fork moved with the workspace or of the update's event included; nothing is changed |

## `POST /api/v1/workspaces/{slug}/sync`

Brings the repository workspace `{slug}` up to date with its origin. It
takes the admin token, an API key or a PAT, declares the scope
`workspaces:write` for a PAT and needs write access to the workspace (see
[Workspace access](#workspace-access)): its owner or the admin token. The
route reads no body. One query parameter:

| Parameter | Values |
|---|---|
| `reset_to_origin` | absent or `false`: a diverged branch is left as it is; `true`: every diverged branch, and only those, is moved to origin's head. Any other value, an empty one, another spelling (`TRUE`, `1`) or the parameter given twice, is `400` `invalid_request` |

The sync resolves the workspace's origin credential from its secrets at
that moment and fetches every branch and tag of origin with it, with a
deadline of two minutes; the fetch sets the hub's origin heads
(`refs/remotes/origin/*`) to origin's branches, all or none. It then
creates every tag origin has and the hub lacks, at origin's value, and
never moves or deletes a tag the hub has. Last, it compares every branch
of origin, in byte order of the names, with the hub's head of it, and moves
the branch by a compare-and-swap where the result says so. A branch only
the hub has is neither looked at nor reported. Sync does not read
`protected_branches` or `protect.owner_push`: a protected branch moves like
any other.

A workspace whose `workspace_mode` is `carry_patch` leaves some branches
out of the comparison: its `integration_branch`, and the branches a
provider the patch feature registers in the hub names for the workspace.
Such a branch is not compared, not moved, not listed in `branches` and not
in the event, although the fetch still sets its origin head. The provider
is asked once per sync, after the fetch and the tags; if it fails, the sync
stops with `500` `internal` before any branch is compared: no branch moves,
no field is written and no event is emitted. A `standard` workspace leaves
nothing out, whatever its `integration_branch`.

The patch feature may also register a sync extension in the hub, which
runs inside the sync of a `carry_patch` workspace, never of a `standard`
one, at two points: right after the fetch of origin, before any tag or
branch is created or moved, and after the comparison of every branch,
before the completion is recorded. Both run to their end when the caller
disconnects. If the first fails, the sync ends as a failed fetch: no
branch or tag moves, `sync_error` is set and the answer is `502`
`upstream_error`, whose message is the extension's error, scrubbed as below
and without the prefix `origin fetch failed: `; the origin heads stay as
the fetch, which had succeeded, set them. If the second fails, the answer is `500`
`internal` with its error, scrubbed, as message; the completion is not
recorded (no field is written and no event emitted) and the branches moved
before stay moved. When the second succeeds it may set `upstream_head_sha`,
written in the completion's transaction, and add members to the answer.
Without an extension a `carry_patch` workspace syncs as described here.

Sync takes no lock on the workspace and never changes its `status`. A
branch that another writer (a push, a landing, another sync) moves between
the comparison and the swap keeps that writer's head: the sync compares it
again from the new head, against the same origin head, and reports the
result of the last comparison, with `local_sha` the hub's head after it.
A branch whose swap is lost on five comparisons stops the sync with `500`
`internal`, whose message names the branch; nothing is recorded and no
event is written, and branches moved before it stay moved. Two syncs of
one workspace at the same time both complete: the second waits for the
first one's fetch, never answers `409` `workspace_busy` for it, and reports
`up_to_date` for a branch the first one has already moved.

| Result | When | The hub's branch |
|---|---|---|
| `created` | the hub has no such branch | created at origin's head |
| `up_to_date` | both heads are equal | unchanged |
| `fast_forward` | the hub's head is an ancestor of origin's | moved to origin's head |
| `ahead` | origin's head is an ancestor of the hub's | unchanged |
| `diverged` | each side has commits the other lacks, without `reset_to_origin=true` | unchanged |
| `reset` | the same, with `reset_to_origin=true` | moved to origin's head |
| `name_conflict` | the hub has no such branch and cannot create it, because a branch only the hub has is in the way of its name: `feat` for origin's `feat/x`, or `topic/a` for origin's `topic` | not created; the branch in the way is unchanged; `local_sha` is `""` |

A tag of origin that a tag only the hub has is in the way of in the same
way (`v1/rc` beside the hub's `v1`) is not created and not listed in the
event, as a tag the hub has keeps its value; the sync goes on.

The checks run in this order, the first that fails answering: a bound PAT
on a workspace outside its binding (`404`, see
[Workspace-bound tokens](#workspace-bound-tokens)); a PAT without
`workspaces:write` (`403`), before the workspace is looked up; the access
check (`404` for a caller who cannot read the workspace, the same bytes as
for a slug no workspace has, `403` for a member of its org, in every
status); `reset_to_origin`; a task fork; the workspace's state: a reclone,
archive or delete running on it, then its status; and last its effective
setting `sync_enabled` (see [Workspace settings](#workspace-settings)), so
that an archived workspace with sync disabled is refused as archived.
Origin is not fetched before every check has passed. The sync runs to its
end when the caller disconnects, with the same branch moves, fields and
event as when the caller waits.

A sync that cannot fetch origin changes nothing in the hub's repository: no
branch, tag or origin head moves, since the origin heads are set only once
the fetch has succeeded and tags are created only after it. This covers secrets that cannot be
read (nothing is then fetched, never anonymously), origin refusing the
credential or the request, a failing network, and a fetch that has not
ended after two minutes, whose git processes are stopped. The sync writes
only `sync_error`, leaves `last_sync_at`, `head_sha`, `origin_head_sha` and
`updated_at` as they were, writes no event and answers `502`
`upstream_error`. `sync_error` and the answer's `message` are the same
text: `origin fetch failed: ` followed by git's error, with every secret
value the hub knows and the origin credential's secret replaced by
`[redacted]`, the user information of every URL removed, and cut to 1024
bytes. The next sync that completes clears `sync_error`.

```json
{"error":{"code":502,"message":"origin fetch failed: git fetch: exit status 128: fatal: Authentication failed for 'https://example.com/a.git/'","error_type":"upstream_error"}}
```

A completed sync, also one with branches `ahead` or `diverged`, sets in one
store transaction `last_sync_at` to the hub's time cut to the second,
`sync_error` to `null`, `head_sha` to the hub's head of the default branch
and `origin_head_sha` to its origin head, both read from the hub's
repository when the completion is recorded, so that of two overlapping
syncs the one that completes last never records an older origin than the
other already fetched, `null` when there is no such branch; `updated_at` is not changed. In the same
transaction it writes one `parley.workspace.sync` event with `workspace`
and `resource_id` the slug, `resource_type` `workspace`, and as actor the
caller as for [`PATCH /api/v1/workspaces/{slug}`](#patch-apiv1workspacesslug):
`admin_token` with `actor_id` `null`, or `api_key` or `pat` with the
caller's user. Its `metadata` holds `created`, `fast_forward`, `ahead`,
`diverged`, `reset` and `name_conflict`, the names of the branches with that result in byte
order (`[]` when none; an `up_to_date` branch is in no list), and
`updated`, one entry per branch the sync created or moved and per tag it
created, with the full ref name, the head before (`null` for a created ref)
and after, sorted by `ref`:

```json
{"seq":14,"id":"…","type":"parley.workspace.sync","time":"2026-11-14T10:32:00Z","workspace":"demo","actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","resource_type":"workspace","resource_id":"demo","action":"sync","session_id":null,"metadata":{"ahead":[],"created":["new-b"],"diverged":["dev"],"fast_forward":["main"],"name_conflict":[],"reset":[],"updated":[{"ref":"refs/heads/main","before":"9fceb02d0ae598e95dc970b74767f19372d61af8","after":"a3f2b1c4d5e6f708192a3b4c5d6e7f8091a2b3c4"},{"ref":"refs/heads/new-b","before":null,"after":"e83c5163316f89bfbde7d9ab23ca2e25604af290"},{"ref":"refs/tags/v2","before":null,"after":"e83c5163316f89bfbde7d9ab23ca2e25604af290"}]}}
```

The answer is the workspace object, read after the completion was
recorded, so that it carries the new `last_sync_at`, `sync_error`,
`head_sha` and `origin_head_sha`, the object
[`GET /api/v1/workspaces/{slug}`](#get-apiv1workspacesslug) answers, with
`access` the caller's level, and the member `branches`: one entry per
branch of origin that the sync did not leave out, in byte order of the
names, an empty list when there is none. `local_sha` is the hub's head of the branch after the sync (`""` for `name_conflict`),
`origin_sha` origin's head as the fetch found it. The members a sync
extension adds come after `branches`, in byte order of their names; one
whose name the workspace object or `branches` has already is left out.

```json
{"slug":"demo","kind":"repository","status":"ready","access":"owner","…":"…","branches":[{"name":"dev","result":"diverged","local_sha":"9fceb02d0ae598e95dc970b74767f19372d61af8","origin_sha":"e83c5163316f89bfbde7d9ab23ca2e25604af290"},{"name":"main","result":"fast_forward","local_sha":"a3f2b1c4d5e6f708192a3b4c5d6e7f8091a2b3c4","origin_sha":"a3f2b1c4d5e6f708192a3b4c5d6e7f8091a2b3c4"}]}
```

| Status | When |
|---|---|
| `200 OK` | the sync completed, diverged branches included; the body is the workspace object plus `branches`, and the members a sync extension adds |
| `400` `invalid_request` | `reset_to_origin` is neither `true` nor `false`, or given more than once (`reset_to_origin must be given once, as true or false`) |
| `400` `fork_has_no_origin` | the workspace is a task fork (`workspace "<slug>" is a task fork, which has no origin to sync from`) |
| `400` `sync_disabled` | the workspace's setting `sync_enabled` is `false` (`workspace "<slug>" has sync disabled; set sync_enabled to true to sync it`) |
| `403` `insufficient_scope` | a PAT without `workspaces:write` (`this route needs the scope workspaces:write`) |
| `403` `forbidden` | a member of the workspace's org who does not own it (`org members have read-only access to this workspace`) |
| `404` `not_found` | the caller cannot read the workspace, or no workspace has the slug (`workspace not found`); also a bound PAT on a workspace outside its binding |
| `409` `workspace_busy` | the workspace is `pending` or `cloning` (`workspace "<slug>" is busy: its clone has not finished; try again later`), or a reclone, archive or delete runs on it, in any status (`workspace "<slug>" is busy: a reclone is running; try again later`) |
| `409` `workspace_not_ready` | the workspace is `failed` (`workspace "<slug>" is not ready: its clone failed; reclone it`) |
| `409` `workspace_archived` | the workspace is `archived` (`workspace "<slug>" is archived`) |
| `502` `upstream_error` | origin could not be fetched, or the workspace's secrets could not be read to fetch it (`origin fetch failed: <git's error, scrubbed>`), or the first stage of a sync extension failed (`<its error, scrubbed>`); no branch or tag of the hub's repository changed, nor, for a failed fetch, any origin head (after a failed first stage they are those the fetch set), and `sync_error` holds the same text |
| `500` `internal` | the sync could not complete: a branch moved in the hub under each of five comparisons (`the sync of workspace "<slug>" stopped: branch "<name>" moved in the hub during each of its 5 evaluations; sync again`), the excluded-branch provider of a `carry_patch` workspace failed (`the sync of workspace "<slug>" stopped before any branch was compared: the excluded-branch provider failed: <its error, scrubbed>`), the second stage of a sync extension failed (`<its error, scrubbed>`) or added a member that is not JSON, a ref of the hub could not be read or written, or `sync_error`, the completion or its event could not be written; branches moved before stay moved |

## `POST /api/v1/workspaces/{slug}/branches/{name}/push`

Pushes the branch `{name}` of the repository workspace `{slug}` to the same
branch of the workspace's origin. It takes the admin token, an API key or a
PAT, declares the scope `workspaces:write` for a PAT and needs write access
to the workspace (see [Workspace access](#workspace-access)): its owner or
the admin token. `{name}` is percent-decoded once, so `fix%2F42` is the
branch `fix/42` and `a%2525b` the branch `a%25b`; the decoded name must
follow git's branch-name rule, as `default_branch` of
[`POST /api/v1/workspaces`](#post-apiv1workspaces) does.

The body is optional. No body, an empty body and an object without `force`
mean `force` `false`; `{"force": true}` asks for a forced push. Members
other than `force` are ignored. A body that is not a JSON object, or whose
`force` is not `true` or `false` (`null` included), is `400`
`invalid_request`; a body sent without `Content-Type: application/json` is
`415` as for every route (see [Request bodies](#request-bodies)).

```json
{"force": false}
```

The hub reads the head of `refs/heads/{name}` once and pushes that commit,
and only that one ref, to `refs/heads/{name}` on origin in one `git push`,
with the workspace's origin credential resolved from its secrets for this
push; no tag and no other branch is sent, and the credential is in no URL,
argument or configuration file. A branch that moves in the hub while the
push runs changes neither what is pushed nor what is answered; the push
takes no lock. Without `force` origin's branch moves only by a
fast-forward: a branch origin lacks is created, one origin has is moved
only when origin's head is an ancestor of the hub's, and a branch origin
already has at the hub's head is left as it is. A protected branch is
pushed like any other without `force`. A forced push replaces origin's
branch only while it is where the hub last saw it: it is handed to git with
an explicit lease for that one ref, the origin head the hub recorded for
the branch, or, when none is recorded, that origin must not have the
branch, never with a plain force. A forced push that is a fast-forward or
creates the branch is `pushed`; a branch origin already has at the hub's
head is `up_to_date`, whatever the lease. A forced push refused because
origin's branch moved changes neither origin's branch nor the recorded
origin head; a sync brings in what origin has, after which the push may be
tried again.

A push that fails changes neither origin's branch nor the hub's recorded
origin head. The answer says what stopped it: origin refusing the update
(`422`), the credential, the repository, the network or anything else on
origin's side (`502`), or the push's deadline of 2 minutes (`504`), after
which `git` is killed with every process it started. Each is read from the
status git reports for the ref when it reports one, and otherwise from
what the HTTPS transport, or for a `git@` origin the SSH transport, says.
Origin's own text may repeat the credential or the URL it was called with,
so the `message` of every error of this route has every secret value the
hub knows and the value of the origin credential resolved for the push
(the token of `GIT_PAT` or the password of `GIT_PASSWORD`) replaced by
`[redacted]`, the user information of every URL removed, and is cut to
1024 bytes, never inside a character. The branch name of the push event
and `remote_url`, in the answer and in the event, are cleaned the same
way.

```json
{"error":{"code":422,"message":"origin refused the update of branch \"feature\": pre-receive hook declined: denied for [redacted] at https://git.example/r.git","error_type":"origin_rejected"}}
```

Before origin is contacted the hub refuses, after the body and the branch
name and in this order: a workspace that is not `ready` or on which a
reclone, archive or delete runs; a task fork, whatever the state of its
parent; for a `carry_patch` workspace, its `integration_branch` and the
branch of a registered patch that is not deleted, with and without `force`;
a forced push of a branch one of the globs of the effective setting
`protected_branches` matches (by default the default branch); and last a
branch the hub's copy does not have. A request that fails several checks
gets the answer of the first. The integration, patch and protected
refusals do not depend on the branch existing. A refused request changes
no branch and no recorded origin head and writes no event.

The answer of a push origin took names the branch, the commit pushed, the
remote and its URL, the workspace's `git_url` without user information
(a `git@host:path` URL as it is stored) and with any secret value in it
replaced by `[redacted]`, and the result: `pushed` when
origin's branch was created or moved forward, `forced` when a forced push
replaced it with a commit that does not descend from it, `up_to_date` when
it was at the commit already.

```json
{"branch":"feature","sha":"a3f2b1c4d5e6f708192a3b4c5d6e7f8091a2b3c4","remote":"origin","remote_url":"https://example.com/a.git","result":"pushed"}
```

Every push handed to origin, one that passed the checks above and whose
credential was read, writes exactly one `parley.workspace.push` event, also
when it fails and also when the caller disconnected: once the push begins
it runs to its end or to its deadline, and its records are written, whether
or not the caller is still there to read the answer. The one exception is a
hub that cannot write its records: the answer is then `500 internal` and no
event is written. A push origin took
first records the pushed commit as the branch's origin head in the hub's
repository and then, in one store transaction, writes the event and, for
the default branch, the workspace's `origin_head_sha` (`updated_at` and
`status` are not changed). When that transaction fails the origin head is
put back to what it was before the call (removed when there was none),
unless a sync has moved it since, no event is written and the answer is
`500 internal`; origin keeps the
commit, and the next sync or push records it. A push that failed writes
the event alone and leaves the origin head as it was. The event has
`workspace` the slug, `resource_type` `branch`, `resource_id` the branch
name, and as actor the caller as for
[`PATCH /api/v1/workspaces/{slug}`](#patch-apiv1workspacesslug):
`admin_token` with `actor_id` `null`, or `api_key` or `pat` with the
caller's user. Its `metadata` holds `branch`, `sha` (the hub's head that was
offered), `force` (what the request asked), `ok`, `remote_url` (as in the
answer) and, for a push origin took (`ok` `true`), `result`, as in the
answer, or, for one that failed (`ok` `false`), `error_type`, the answer's,
never both:

```json
{"seq":15,"id":"…","type":"parley.workspace.push","time":"2026-11-14T10:35:00Z","workspace":"demo","actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","resource_type":"branch","resource_id":"feature","action":"push","session_id":null,"metadata":{"branch":"feature","sha":"a3f2b1c4d5e6f708192a3b4c5d6e7f8091a2b3c4","force":false,"ok":false,"remote_url":"https://example.com/a.git","error_type":"non_fast_forward"}}
```

| Status | When |
|---|---|
| `200 OK` | origin took the push; `result` is `pushed`, `forced` or `up_to_date` |
| `400` `invalid_request` | the body is not a JSON object (`request body must be a JSON object`, `request body is not valid JSON`), its `force` is not a JSON boolean (`request body member "force" must be true or false`), or the decoded name is not a valid branch name (`branch name "<name>": <the rule it breaks>`) |
| `400` `fork_has_no_origin` | the workspace is a task fork (`workspace "<slug>" is a task fork, which has no origin to push to`) |
| `403` `insufficient_scope` | a PAT without `workspaces:write` (`this route needs the scope workspaces:write`), before the body is looked at |
| `403` `forbidden` | a member of the workspace's org who does not own it (`org members have read-only access to this workspace`), before the body is looked at |
| `403` `integration_branch` | the branch is the `integration_branch` of a `carry_patch` workspace (`branch "<name>" is the integration branch of workspace "<slug>", which only its rebuild pushes to origin`) |
| `403` `patch_branch` | the branch is the branch of a registered patch of a `carry_patch` workspace that is not deleted (`branch "<name>" is a patch branch of workspace "<slug>", which only its patch list pushes to origin`) |
| `403` `protected_branch` | `force` is `true` and a glob of `protected_branches` matches the branch (`branch "<name>" of workspace "<slug>" is protected and cannot be replaced by a forced push`) |
| `404` `not_found` | the caller cannot read the workspace, or no workspace has the slug (`workspace not found`), before the body is looked at; also a bound PAT on a workspace outside its binding |
| `404` `branch_not_found` | the hub's copy has no branch `{name}` (`workspace "<slug>" has no branch "<name>"`) |
| `409` `workspace_busy` | the workspace is `pending` or `cloning` (`workspace "<slug>" is busy: its clone has not finished; try again later`), or a reclone, archive or delete runs on it, in any status (`workspace "<slug>" is busy: a reclone is running; try again later`) |
| `409` `workspace_not_ready` | the workspace is `failed` (`workspace "<slug>" is not ready: its clone failed; reclone it`) |
| `409` `workspace_archived` | the workspace is `archived` (`workspace "<slug>" is archived`) |
| `409` `non_fast_forward` | without `force`, origin's branch is not an ancestor of the hub's head: the two diverged or origin's is ahead (`origin's branch "<name>" is not an ancestor of the hub's head <sha> (the origin head the hub last saw is <sha>); sync the workspace to bring in origin's commits, or push with force to replace origin's branch`); origin's branch and the hub's recorded origin head are unchanged |
| `409` `origin_moved` | `force` is `true` and origin's branch is neither at the hub's head nor where the lease says: it moved since the hub last saw it, it exists although the hub recorded no origin head for it, or it is gone although the hub recorded one (`origin's branch "<name>" changed since the hub last saw it (the origin head the hub last saw is <sha>); sync the workspace to bring in origin's branch, then push again`, or with `the hub has no record of origin's head of it, so origin must not have it` in the parentheses); origin's branch and the hub's recorded origin head are unchanged |
| `422` `origin_rejected` | origin itself refused the update of the branch, by a branch rule or a server-side hook (`origin refused the update of branch "<name>": <origin's reason>`, the reason followed by what origin's hook wrote, such as `pre-receive hook declined: branch is locked`) |
| `502` `origin_unauthenticated` | origin asked for a credential and the workspace has none (`origin at <remote_url> asked for a credential and workspace "<slug>" has no credential; store GIT_PAT, or GIT_USERNAME and GIT_PASSWORD, as its secrets`), or origin did not take the stored one (`origin at <remote_url> rejected the stored credential of workspace "<slug>"`) |
| `502` `origin_forbidden` | origin answered that the credential may not push to the repository, HTTP `403` (`origin at <remote_url> does not let the credential of workspace "<slug>" push to the repository: <what git said>`) |
| `502` `origin_not_found` | origin has no repository at `git_url`, or the credential cannot see it, HTTP `404` (`origin has no repository at <remote_url>, or the credential of workspace "<slug>" cannot see it: <what git said>`) |
| `502` `origin_unreachable` | origin's name did not resolve, the connection failed, or TLS did, an untrusted certificate included; for a `git@` origin also a host key that is not trusted (`origin at <remote_url> cannot be reached: <what git said>`) |
| `502` `upstream_error` | the push failed in any other way, such as HTTP `500` from origin (`the push of branch "<name>" to origin at <remote_url> failed: <what git said>`) |
| `503` `unavailable` | the patch list of a `carry_patch` workspace could not be read (`the patch list of workspace "<slug>" cannot be read; try again later`); nothing is pushed |
| `504` `origin_timeout` | the push had not finished 2 minutes after it began; `git` is killed with every process it started before the answer (`the push of branch "<name>" to origin did not finish within 2m0s`) |
| `500` `internal` | the workspace's secrets could not be read, in which case origin is not contacted and nothing is pushed without a credential and no event is written; the records of a push handed to origin could not be written, in which case no event is written and, when origin took the push, the origin head is put back unless a sync has moved it since; or the hub failed otherwise |

## `POST /api/v1/workspaces/{slug}/archive`

Takes the repository workspace `{slug}` out of service without losing
anything the hub holds. It takes the admin token, an API key or a PAT,
declares the scope `workspaces:write` for a PAT and needs write access to
the workspace (see [Workspace access](#workspace-access)): its owner or the
admin token. The route reads no body; one that passes the content-type and
size rules is ignored.

A workspace in status `ready` or `failed` is archived; the checks run in
the order of the table below, and a refused archive contacts no origin and
changes nothing. Once they have passed, the archive runs to its end even
when the caller disconnects, and while it runs every other request that
needs the workspace's repository answers `409` `workspace_busy`. A sync or
push of the workspace already running when the archive starts is waited
for first. It first
makes the safety copy: every branch of the hub's copy (for a `carry_patch`
workspace every branch but its `integration_branch`) is offered to the
branch of the same name on origin as a fast-forward, in one push with the
workspace's origin credential and a deadline of two minutes, sending no
tag, forcing nothing and deleting nothing. Whatever origin answers, the
archive goes on: a branch origin refused stays in the hub's copy. A
`failed` workspace, or one with no copy, offers nothing. Then, in one
store transaction, `status` becomes `archived`, `clone_error` is cleared,
one `parley.workspace.archive` event is written and every active agent
session of the workspace and of its forks is ended with the reason
`workspace archived`; when that transaction fails nothing has changed and
the answer is `500 internal`. A session that cannot be ended does not fail
the archive: what ending the sessions wrote is undone, the failure is
logged, and the workspace is archived all the same. Once the archive is
stored, the features that act on an archived workspace, such as one that
stops its sandboxes, are told; a failure there is logged and changes
nothing in the answer. The hub's copy, every ref of it, the workspace's
forks and the data they own are left as they are.

The event has `workspace` the slug, `resource_type` `workspace`,
`resource_id` the slug, and as actor the caller as for
[`PATCH /api/v1/workspaces/{slug}`](#patch-apiv1workspacesslug). Its
`metadata` holds `head_sha` (the workspace's `head_sha` when it was
archived, `null` when it has none), `origin_push`, the outcome of the
safety copy, and `forks`, the number of forks of the workspace.
`origin_push` is `ok` when origin took every branch offered or there was
none to offer, `partial` when it took at least one and refused at least
one, and `failed` when it took none, also when the credential could not be
read (no anonymous push is tried), origin refused the credential, could
not be reached or did not answer within the deadline. Neither the answer
nor the event nor the hub's log holds the credential or what origin said.

```json
{"seq":16,"id":"…","type":"parley.workspace.archive","time":"2026-11-14T10:40:00Z","workspace":"demo","actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","resource_type":"workspace","resource_id":"demo","action":"archive","session_id":null,"metadata":{"head_sha":"a3f2b1c4d5e6f708192a3b4c5d6e7f8091a2b3c4","origin_push":"partial","forks":2}}
```

The answer is the workspace object, read after the archive, with `status`
`archived`, `clone_error` `null` and `access` the caller's level, the
object [`GET /api/v1/workspaces/{slug}`](#get-apiv1workspacesslug) answers.

| Status | When |
|---|---|
| `200 OK` | the workspace is archived, whatever `origin_push` says; the body is the workspace object |
| `400` `not_supported_on_fork` | the workspace is a task fork (`workspace "<slug>" is a task fork, which cannot be archived; delete it instead`) |
| `403` `insufficient_scope` | a PAT without `workspaces:write` (`this route needs the scope workspaces:write`) |
| `403` `forbidden` | a member of the workspace's org who does not own it (`org members have read-only access to this workspace`) |
| `404` `not_found` | the caller cannot read the workspace, or no workspace has the slug (`workspace not found`); also a bound PAT on a workspace outside its binding |
| `409` `workspace_busy` | a reclone, archive or delete runs on the workspace, in any status (`workspace "<slug>" is busy: a reclone is running; try again later`), or it is `pending` or `cloning` (`workspace "<slug>" is busy: its clone has not finished; try again later`) |
| `409` `workspace_archived` | the workspace is `archived` already (`workspace "<slug>" is archived`) |
| `500` `internal` | the status change or its event could not be written; the workspace is as it was |

## `POST /api/v1/workspaces/{slug}/reactivate`

Brings the archived repository workspace `{slug}` back into service. It
takes the admin token, an API key or a PAT, declares the scope
`workspaces:write` for a PAT and needs write access to the workspace (see
[Workspace access](#workspace-access)): its owner or the admin token. The
route reads no body; one that passes the content-type and size rules is
ignored.

A workspace in status `archived` is reactivated; the checks run in the
order of the table below, and a refused reactivate changes nothing. In one
store transaction `status` becomes `pending`, `clone_error` is cleared and
one `parley.workspace.reactivate` event is written; when that transaction
fails nothing has changed and the answer is `500 internal`. Once it has
committed, the clone work of the workspace is started. When the copy the
archive kept is still valid, the clone work brings the workspace to
`ready` from it, with its branches and its `head_sha`, without contacting
origin; when the copy is missing or does not open as a repository, the
clone work makes a fresh copy from origin, which holds origin's branches
and nothing that existed only in the hub.

The event has `workspace` the slug, `resource_type` `workspace`,
`resource_id` the slug, as actor the caller as for
[`PATCH /api/v1/workspaces/{slug}`](#patch-apiv1workspacesslug), and an
empty `metadata`.

```json
{"seq":17,"id":"…","type":"parley.workspace.reactivate","time":"2026-11-14T10:45:00Z","workspace":"demo","actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","resource_type":"workspace","resource_id":"demo","action":"reactivate","session_id":null,"metadata":{}}
```

The answer is the workspace object, read after the reactivate, with
`status` `pending`, `clone_error` `null` and `access` the caller's level,
the object [`GET /api/v1/workspaces/{slug}`](#get-apiv1workspacesslug)
answers; read it again to see the clone work reach `ready` or `failed`.

| Status | When |
|---|---|
| `200 OK` | the workspace is back in `pending` and its clone work is started; the body is the workspace object |
| `400` `not_supported_on_fork` | the workspace is a task fork (`workspace "<slug>" is a task fork, which cannot be reactivated on its own`) |
| `403` `insufficient_scope` | a PAT without `workspaces:write` (`this route needs the scope workspaces:write`) |
| `403` `forbidden` | a member of the workspace's org who does not own it (`org members have read-only access to this workspace`) |
| `404` `not_found` | the caller cannot read the workspace, or no workspace has the slug (`workspace not found`); also a bound PAT on a workspace outside its binding |
| `409` `workspace_busy` | a reclone, archive or delete runs on the workspace (`workspace "<slug>" is busy: a delete is running; try again later`) |
| `409` `workspace_not_archived` | the workspace is in any status other than `archived`, `pending` and `cloning` included (`workspace "<slug>" is ready, not archived`) |
| `500` `internal` | the status change or its event could not be written; the workspace is still `archived` |

## `POST /api/v1/workspaces/{slug}/reclone`

Throws away the hub's copy of the repository workspace `{slug}` and clones
its origin again. It takes the admin token, an API key or a PAT, declares
the scope `workspaces:write` for a PAT and needs write access to the
workspace (see [Workspace access](#workspace-access)): its owner or the
admin token. The route reads no body; one that passes the content-type and
size rules is ignored.

A workspace in status `ready` or `failed` without forks is recloned; the
checks run in the order of the table below, and a refused reclone neither
contacts origin nor touches the copy. While the reclone runs, every other
request that needs the workspace's repository is answered `409
workspace_busy`, and a sync or push of the workspace already running when
the reclone starts is waited for before the reclone changes anything.
First the hub makes the same safety copy as
[`POST /api/v1/workspaces/{slug}/archive`](#post-apiv1workspacesslugarchive):
it offers every branch of its copy to origin as a fast-forward, except a
carry-patch workspace's integration branch, and goes on whatever origin
answers. Then, in one store transaction, `status` becomes `pending`,
`sync_error` and `origin_head_sha` are cleared and one
`parley.workspace.reclone` event is written; when that transaction fails
nothing has changed, the copy included, and the answer is `500 internal`.
Once it has committed, the copy is removed from disk, and with it the
origin head of every branch, and the clone work of the workspace is
started. The new copy holds what origin holds and nothing else: a branch
origin did not take, and every ref under `refs/parley/`, are lost; change
records and the other data the workspace owns stay. `head_sha`,
`default_branch`, `last_sync_at` and `clone_error` keep their values until
the clone work changes them. A copy that a hub stopped in the middle of a
reclone left on disk is not taken for the workspace's copy: the clone
work at the next start replaces it with a fresh clone.

The event has `workspace` the slug, `resource_type` `workspace`,
`resource_id` the slug, as actor the caller as for
[`PATCH /api/v1/workspaces/{slug}`](#patch-apiv1workspacesslug), and in
`metadata` `origin_push`, the outcome of the safety copy, with the values
the archive event gives it (`ok`, `partial` or `failed`).

```json
{"seq":18,"id":"…","type":"parley.workspace.reclone","time":"2026-11-14T10:50:00Z","workspace":"demo","actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","resource_type":"workspace","resource_id":"demo","action":"reclone","session_id":null,"metadata":{"origin_push":"ok"}}
```

The answer is the workspace object, read after the reclone, with `status`
`pending`, `sync_error` `null`, `origin_head_sha` `null` and `access` the
caller's level, the object
[`GET /api/v1/workspaces/{slug}`](#get-apiv1workspacesslug) answers; read
it again to see the clone work reach `ready` or `failed`.

| Status | When |
|---|---|
| `200 OK` | the workspace is back in `pending`, its copy is gone and its clone work is started, whatever `origin_push` says; the body is the workspace object |
| `400` `fork_has_no_origin` | the workspace is a task fork (`workspace "<slug>" is a task fork, which has no origin to reclone from`) |
| `403` `insufficient_scope` | a PAT without `workspaces:write` (`this route needs the scope workspaces:write`) |
| `403` `forbidden` | a member of the workspace's org who does not own it (`org members have read-only access to this workspace`) |
| `404` `not_found` | the caller cannot read the workspace, or no workspace has the slug (`workspace not found`); also a bound PAT on a workspace outside its binding |
| `409` `workspace_busy` | a reclone, archive or delete runs on the workspace, in any status (`workspace "<slug>" is busy: a reclone is running; try again later`), or it is `pending` or `cloning` (`workspace "<slug>" is busy: its clone has not finished; try again later`) |
| `409` `workspace_archived` | the workspace is `archived` (`workspace "<slug>" is archived`) |
| `409` `has_forks` | the workspace has at least one fork, which depends on its copy (`workspace "<slug>" has <n> forks, which depend on its copy; delete them first`) |
| `500` `internal` | the status change or its event could not be written; the workspace and its copy are as they were |

## `DELETE /api/v1/workspaces/{slug}`

Deletes the archived repository workspace `{slug}`, its task forks and
everything they own, in one step or not at all. It takes the admin token,
an API key or a PAT, declares the scope `workspaces:write` for a PAT and
needs write access to the workspace (see
[Workspace access](#workspace-access)): its owner or the admin token. The
route reads no body; one that passes the content-type and size rules is
ignored.

For a task fork, once the scope and access checks have passed, the delete
is handed to the task-fork feature, which removes forks by its own rules:
its answer is the route's, `204` with an empty body when it removed the
fork and its error, unchanged, otherwise. None of what follows applies to a
fork. While the hub has no task-fork feature, a fork is answered `500`
`internal` and nothing is removed.

A repository workspace in status `archived` is deleted; the checks run in
the order of the table below, and a refused delete changes nothing. Once
they have passed, the delete runs to its end even when the caller
disconnects, and while it runs every other request that needs the
workspace's repository answers `409` `workspace_busy`. First the
sandboxes of the workspace and of its forks are deleted; when that fails
the answer is `500 internal` and no record, token or other data has
changed, so the delete can be repeated. A fork made after the sandboxes
were deleted makes the delete answer `409` `workspace_busy` with nothing
changed, so it can be repeated. Then, in one store transaction,
the forks are removed in ascending slug order and then the workspace: for
each, the tokens bound to it are revoked, the data other features keep
for it (its secrets among them) is removed, its remaining agent sessions
are ended with the reason `workspace deleted`, its record is removed with
its settings, and one `parley.workspace.delete` event is written, the
workspace's last. When anything in that transaction fails, nothing has
changed, the repository data included, and the answer is `500 internal`.
Once it has committed, the hub's copies of the removed workspaces are
removed from disk; a copy that cannot be removed is logged with the slug
and the path, and the answer stays `204`. Every event written about the
workspace and its forks before the delete stays in the event log. The slug
is free again afterwards:
[`POST /api/v1/workspaces`](#post-apiv1workspaces) takes it as a slug that
was never used.

Each event has `workspace` the removed slug, `resource_type` `workspace`,
`resource_id` the slug, and as actor the caller as for
[`PATCH /api/v1/workspaces/{slug}`](#patch-apiv1workspacesslug). Its
`metadata` holds `kind` (`fork` or `repository`), `parent_slug` (the
fork's parent, `null` for the repository workspace) and `revoked_tokens`,
the ids of the tokens bound to that slug that were revoked, in ascending
order, `[]` when there were none. The repository workspace's event also
holds `deleted_forks`, the slugs of the forks removed with it, in
ascending order, `[]` when it had none. A fork's event is visible to the
readers of its parent's events, as the fork's other events are.

```json
{"seq":19,"id":"…","type":"parley.workspace.delete","time":"2026-11-14T11:00:00Z","workspace":"demo--t1","actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","resource_type":"workspace","resource_id":"demo--t1","action":"delete","session_id":null,"metadata":{"kind":"fork","parent_slug":"demo","revoked_tokens":["tok1"]}}
{"seq":20,"id":"…","type":"parley.workspace.delete","time":"2026-11-14T11:00:00Z","workspace":"demo","actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","resource_type":"workspace","resource_id":"demo","action":"delete","session_id":null,"metadata":{"kind":"repository","parent_slug":null,"revoked_tokens":[],"deleted_forks":["demo--t1"]}}
```

| Status | When |
|---|---|
| `204 No Content` | the workspace, its forks and what they own are gone; the body is empty |
| `403` `insufficient_scope` | a PAT without `workspaces:write` (`this route needs the scope workspaces:write`) |
| `403` `forbidden` | a member of the workspace's org who does not own it (`org members have read-only access to this workspace`) |
| `404` `not_found` | the caller cannot read the workspace, or no workspace has the slug (`workspace not found`); also a bound PAT on a workspace outside its binding |
| `409` `workspace_busy` | a reclone, archive or delete runs on the workspace (`workspace "<slug>" is busy: a reclone is running; try again later`), or a fork of the workspace was made while the delete ran (`workspace "<slug>" got the fork "<fork>" while it was being deleted; try again`): nothing has changed |
| `409` `workspace_not_archived` | the workspace is in any status other than `archived`, `pending` and `cloning` included (`workspace "<slug>" is ready, not archived`) |
| `500` `internal` | the workspace is a task fork and no part of the hub deletes task forks (`workspace "<slug>" is a task fork, and no fork remover deletes it`): the fork is unchanged; or the sandboxes could not be deleted, or a step of the removal failed: the workspace is still `archived` and nothing has changed |

## `POST /api/v1/workspaces/{slug}/tokens`

Mints a personal access token (PAT) owned by the caller's user and bound to
the workspace `{slug}`: its `workspace` is the slug from the moment it
exists, and no route changes it. It takes an API key or a PAT and declares
the scope `tokens:manage` for a PAT and the level `owner` on the workspace
(see [Workspace access](#workspace-access)). It is a create route (see
[Idempotency](#idempotency)); a replay leaves out `token`.

The body is that of [`POST /api/v1/user/tokens`](#post-apiv1usertokens),
validated the same way with the same messages: `name`, `permissions`,
`paths` and `expires`. A PAT caller is held to the same subset rule. The
route is closed to every PAT bound to a workspace, so a bound token cannot
mint another: see [Workspace-bound tokens](#workspace-bound-tokens).

The checks run in this order, the first that fails answering, and a refused
request mints nothing: a bound PAT (`403` `token_bound` on a workspace its
binding reaches, `404` on any other); a PAT without `tokens:manage` (`403`); the subset
rule for a PAT caller, before the workspace is looked up, so the answer is
the same for an unknown slug; the access rule for need `owner` (`404` for a
workspace the caller cannot read and for an unknown slug, with the same
bytes; `403` for an org member, who reads it); the admin token (`403`),
before the body is read; the body (`400`); and then, for a valid body, an
archived workspace (`409`).

The token and a `parley.token.bind` event are written in one store
transaction: a failure of either, or of the commit, leaves neither and is
answered `500` `internal` without its cause.

| Status | When |
|---|---|
| `201 Created` | the token was minted; the body is the Token object, with `workspace` the slug, and `token` |
| `200 OK` | a replay of a recorded `Idempotency-Key` with the same body: the first answer without `token`; nothing is minted |
| `400` `invalid_request` | the body is not valid, with the messages of `POST /api/v1/user/tokens` (`request body must be a JSON object` for a body that is not an object) |
| `403` `token_bound` | a PAT bound to a workspace, on a workspace its binding reaches, whatever scopes it holds (`a token bound to a workspace cannot use this route`) |
| `403` `insufficient_scope` | a PAT without `tokens:manage` (`this route needs the scope tokens:manage`), or a valid body beyond what the calling PAT holds, with the subset rule's messages (`permissions: this token does not hold "git:read"`) |
| `403` `forbidden` | an org member who can read the workspace (`org members have read-only access to this workspace`), or the admin token (`this route needs a user`) |
| `404` `not_found` | the caller cannot read the workspace, or no workspace has the slug (`workspace not found`); also a bound PAT on a workspace outside its binding |
| `409` `workspace_archived` | a valid body for an archived workspace (`workspace is archived`) |
| `500` `internal` | writing the token or its event failed; nothing is minted |

```json
{"name":"agent","permissions":["git:read"],"expires":30}
```

```json
{"token_id":"k3x9m2pq","user_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","name":"agent","permissions":["git:read"],"paths":null,"workspace":"w1","expires_at":"2026-01-31T00:00:00Z","revoked_at":null,"created_at":"2026-01-01T00:00:00Z","token":"parley_pat_k3x9m2pq_0123456789abcdefghijklmnopqrstuv"}
```

The event, as [`GET /api/v1/events/log`](#get-apiv1eventslog) shows it to
the admin token and to a user with level `owner` or `admin` on the
workspace, has `actor_type` `api_key` or `pat` after the calling
credential, `actor_id` the token's owner, and in `metadata` the token's
`token_id`, `workspace`, `permissions`, `paths` and `expires_at` (`null`
when it has none or never expires), never its value:

```json
{"seq":7,"id":"…","type":"parley.token.bind","time":"2026-01-01T00:00:00Z","workspace":"w1","actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","resource_type":"token","resource_id":"k3x9m2pq","action":"bind","session_id":null,"metadata":{"expires_at":"2026-01-31T00:00:00Z","paths":null,"permissions":["git:read"],"token_id":"k3x9m2pq","workspace":"w1"}}
```

## `GET /api/v1/workspaces/{slug}/tokens`

Lists every personal access token bound to the workspace `{slug}`, live,
revoked or expired, so its owner can see everything that can reach it or
once could. It takes an API key, an unbound PAT or the admin token, and
declares the scope `tokens:read` for a PAT, which `tokens:write` and
`tokens:manage` imply, and the level `owner` on the workspace (see
[Workspace access](#workspace-access)); the admin token has it on every
workspace. An archived workspace is listed like any other. Like the mint
route, it is closed to every PAT bound to a workspace (see
[Workspace-bound tokens](#workspace-bound-tokens)).

The answer is a [list](#lists) in the standard envelope, `{"items":[…],"next_cursor":…}`,
with the standard `limit` and `cursor`. Each item is a Token object (see
[`POST /api/v1/user/tokens`](#post-apiv1usertokens)), never with its value,
with `workspace` the slug; tokens bound to another workspace and unbound
tokens are not in it. It is ordered oldest first, by `created_at` in whole
seconds, as it is shown, and then by `token_id`: tokens created in the same
second are in `token_id` order, whatever their order within that second. A
workspace with no
bound token answers `{"items":[],"next_cursor":null}`.

The checks run in this order, the first that fails answering: a bound PAT
(`403` `token_bound` on a workspace its binding reaches, `404` on any
other); a PAT without `tokens:read` (`403`), the same for an unknown slug;
the access rule for need `owner` (`404` for a workspace the caller cannot
read and for an unknown slug, with the same bytes; `403` for an org member,
who reads it); the `limit` and `cursor` (`400`).

| Status | When |
|---|---|
| `200 OK` | a page of the tokens bound to the workspace |
| `400` `invalid_request` | a `limit` or `cursor` that is not valid |
| `403` `token_bound` | a PAT bound to a workspace, on a workspace its binding reaches, whatever scopes it holds (`a token bound to a workspace cannot use this route`) |
| `403` `insufficient_scope` | a PAT without `tokens:read` or a scope that implies it (`this route needs the scope tokens:read`) |
| `403` `forbidden` | an org member who can read the workspace (`org members have read-only access to this workspace`) |
| `404` `not_found` | the caller cannot read the workspace, or no workspace has the slug (`workspace not found`); also a bound PAT on a workspace outside its binding |

```json
{"items":[{"token_id":"k3x9m2pq","user_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","name":"agent","permissions":["git:read"],"paths":null,"workspace":"w1","expires_at":"2026-01-31T00:00:00Z","revoked_at":"2026-01-02T00:00:00Z","created_at":"2026-01-01T00:00:00Z"}],"next_cursor":null}
```

## Admin routes

The user and org administration routes are admin routes. Their caller must
be the admin token or an API key whose user has the role `admin`; every
other caller, a PAT included, also a PAT of an admin, is answered `403`
`forbidden` with the message `this route needs an admin`:

```json
{"error":{"code":403,"message":"this route needs an admin","error_type":"forbidden"}}
```

The check runs in the access position of the [order of checks](#order-of-checks):
after authentication, the rate limit and the body's size and content type,
and before the route validates its body or looks up anything its path
names. So a refused request gets the same `403` whatever its body and
whatever it names, and changes nothing. The role is read from the store on
every request: an API key whose user loses the role `admin` is refused from
its next request on, and one whose user gains it passes.

The admin routes so far: `POST /api/v1/users`, `GET /api/v1/users`,
`GET /api/v1/users/{id}`, `PATCH /api/v1/users/{id}`,
`POST /api/v1/users/{id}/promote`, `POST /api/v1/users/{id}/demote`,
`POST /api/v1/users/{id}/block`, `POST /api/v1/users/{id}/unblock`,
`DELETE /api/v1/users/{id}`, `GET /api/v1/users/{id}/keys`,
`GET /api/v1/users/{id}/tokens`, `DELETE /api/v1/users/{id}/keys/{key_id}`,
`DELETE /api/v1/users/{id}/tokens/{token_id}`, `POST /api/v1/orgs`, `GET /api/v1/orgs`, `PATCH /api/v1/orgs/{id}`,
`PUT /api/v1/orgs/{id}/members/{user_id}`,
`DELETE /api/v1/orgs/{id}/members/{user_id}`,
`POST /api/v1/orgs/{id}/block`, `POST /api/v1/orgs/{id}/unblock` and
`DELETE /api/v1/orgs/{id}`. `GET /api/v1/orgs/{id}` and
`GET /api/v1/orgs/{id}/members` are not: a member reads an org and its
member list too (see [`GET /api/v1/orgs/{id}`](#get-apiv1orgsid)).

## Workspace access

A route that names a workspace by its slug, as in
`/api/v1/workspaces/{slug}/...`, declares what it needs of its caller:
`read`, `write` or `owner`. The workspace access check decides from the
caller's level on the workspace:

| Level | Who | `read` | `write`, `owner` |
|---|---|---|---|
| `admin` | the admin token, on every workspace | granted | granted |
| `owner` | the user who owns the workspace; on a task fork also the owner of its parent | granted | granted |
| `member` | a member of the workspace's org while the org is active | granted | `403 forbidden` |
| none | everyone else | `404 not_found` | `404 not_found` |

A user's level is the same by API key and by PAT, and the role `admin`
gives a user no level. A task fork never opens more than its parent: a user
with no level on the parent has none on the fork, even the user who created
it. The workspace's status does not change the level: a member is refused a
`write` or `owner` route of an archived workspace as of any other, and the
route checks the status after access. A route that needs `read` lets a
member through whatever its method, so that members can contribute.

A caller with no level is answered exactly as for a slug that names no
workspace, with the same status and the same bytes, whatever the route
needs, the workspace's status or its kind:

```json
{"error":{"code":404,"message":"workspace not found","error_type":"not_found"}}
```

The slug's shape is not checked first: a string that cannot be a slug, or
that differs from one only in case, is a workspace that does not exist. (A
path segment `.` or `..` never reaches a route: such a path is not clean
and is answered the generic `404 not_found` with the message `not found`,
as in [Routes and matching](#routes-and-matching); no slug can be either.) So a
caller cannot tell an existing workspace they may not read from a missing
one. A member on a `write` or `owner` route, who can read the workspace, is
answered:

```json
{"error":{"code":403,"message":"org members have read-only access to this workspace","error_type":"forbidden"}}
```

A failure while deciding, such as a failed read of the store, is answered
`500 internal` with the message `internal error`, never as a grant, and
the answer shows neither the failure nor the workspace. In every refusal the
route's handler does not run.

The check runs in the access position of the [order of checks](#order-of-checks),
after the admin check and before the `Idempotency-Key` of a create route, so
a replayed create is decided again. No route declares a need yet; the
workspace routes will.

## Sandbox routes

Sandboxes are optional: a hub offers them only with a `[sandbox]` section
(see [configuration.md](configuration.md#sandboxes)). Every sandbox route
runs the sandbox feature check in step 5 of the [order of checks](#order-of-checks),
after authentication, the rate limit and the body checks and before the
binding guard, the scope check, workspace access and the handler. On a hub
without sandboxes it answers every authenticated caller, the admin token
included, whatever the scopes of a PAT and whether or not the workspace
exists:

```json
{"error":{"code":404,"message":"sandboxes are not enabled on this hub","error_type":"sandboxes_disabled"}}
```

A request without a credential still gets `401 unauthenticated`, a path no
route has `404 not_found`, a method the route does not have `405
method_not_allowed` and a body over the limit `413`, since those steps come
first. On a hub with sandboxes the check passes and the later checks and the
handler decide.

A sandbox route of a workspace, under `/api/v1/workspaces/{slug}/sandboxes`,
is of one of two kinds:

| Kind | Scope of a PAT | Workspace access | Routes |
|---|---|---|---|
| read | `sandboxes:read` | `read` | getting a sandbox, listing a workspace's sandboxes, its logs, downloading a file, listing policy proposals |
| write | `sandboxes:write` | `write` | every other, creating, exec, attach, uploading, stop, start, delete, checkpoints, approving and rejecting a proposal included |

The scope is checked before workspace access, so a PAT without the scope is
answered `403 insufficient_scope` also on a slug that names no workspace,
and an org member's PAT without `sandboxes:write` gets `403
insufficient_scope` on a write route, not `403 forbidden`. The admin token
passes as an owner of every workspace, except on a route that acts for a
user, such as creating a sandbox: there it is answered, after workspace
access and before the route reads its body,

```json
{"error":{"code":403,"message":"the admin token cannot create a sandbox","error_type":"forbidden"}}
```

and on a slug that names no workspace `404 not_found`, since workspace
access is decided first.

When the sandbox runtime cannot be reached, or has no operation for what a
sandbox route asks of it, the route answers `502` with `error_type`
`sandbox_runtime_error`. Its message is the runtime's error with the
gateway's credentials and any user information of a URL removed; do not
branch on it. The hub keeps serving: routes that read only the hub's own
state keep answering.

```json
{"error":{"code":502,"message":"sandbox runtime: get_sandbox: unavailable: connection refused","error_type":"sandbox_runtime_error"}}
```

A route that runs a command in a sandbox, attaches to it or writes a file
into it also needs a credential that covers the sandbox's hub credential
(its `hub_access`), so that a weak token cannot act through a sandbox a
stronger credential created. It is decided once the sandbox is found and
before the body is validated: every caller covers a sandbox whose
`hub_access` is null, the admin token and an API key cover every one, and a
PAT covers it when it holds every scope of `hub_access.scopes` (a `:write`
scope also counting for its `:read`, as in the scope check) and, if the PAT
has `paths`, `hub_access.paths` is not null and each of its globs is covered
by one of the PAT's globs. Any other caller gets `403
insufficient_scope`, and nothing is run, attached or written:

```json
{"error":{"code":403,"message":"this token does not hold \"git:write\", which the sandbox's hub credential holds","error_type":"insufficient_scope"}}
```

The message names the first scope the token lacks, or the first of the
hub credential's globs no glob of the token covers, or says that the token
is limited to paths and the hub credential is not; do not branch on it.

## `GET /api/v1/sandbox-profiles`

Lists the sandbox profiles of the hub's configuration (see
[configuration.md](configuration.md#sandboxes)), so that an agent knows
which profile to ask for. Any authenticated caller may call it: an API key,
the admin token, or a PAT with the scope `sandboxes:read`. It needs access
to no workspace, so a user who owns none gets the same list.

The answer is `200` with every profile, in name order, on one page:
`next_cursor` is always `null`, and the route ignores every query
parameter, `limit` and `cursor` included. It is read from the configuration
alone and asks the sandbox runtime nothing, so it answers the same while the
runtime is unreachable.

```json
{"items":[{"name":"agent","default":true,"description":"an agent","image":"agent:1","cpu":"2","memory":"4Gi","gpu":1,"ttl":"4h","ttl_seconds":14400,"idle_timeout":"30m","policy_summary":["a.test","b.test"],"credentials":["model"],"optional_credentials":["search"],"exposed_secrets":[{"name":"A_TOKEN","optional":true},{"name":"Z_TOKEN","optional":false}],"hub_access":"none","env_keys":["A","B"],"checkpoint":false,"allow_proposals":false}],"next_cursor":null}
```

Each item has exactly these members:

| Member | Value |
|---|---|
| `name` | the profile's name |
| `default` | `true` for the profile a sandbox gets when its creator names none |
| `description` | the profile's description, `""` when it has none |
| `image`, `cpu`, `memory` | as configured, `""` when the profile leaves them to the runtime |
| `gpu` | the number of GPUs, `0` when none |
| `ttl` | the profile's time to live, as a [duration](#timestamps-and-durations) in the largest unit that divides it: `4h`, `30m`, `1d` (a `ttl` of `24h` is written `1d`) |
| `ttl_seconds` | the same time to live in seconds, an integer |
| `idle_timeout` | the profile's idle timeout, the section's when the profile sets none, written like `ttl`; `0s` when the idle stop is off |
| `policy_summary` | the hosts the profile's base policy allows, as the runtime reported them at start, sorted; `[]` for the built-in policy |
| `credentials`, `optional_credentials` | the names of the configured credentials the profile attaches, required and optional; `[]` when none |
| `exposed_secrets` | the names of the secrets the profile exposes, each `{"name": …, "optional": …}`, sorted by name; `[]` when none |
| `hub_access` | `fork` or `none` |
| `env_keys` | the names of the profile's `env` variables, sorted; `[]` when none |
| `checkpoint`, `allow_proposals` | as configured |

The path of a policy file, the values of `env`, and the lists
`expose_secrets` and `optional_expose_secrets` are never shown, and no
member holds a secret's value.

A PAT without `sandboxes:read` gets `403 insufficient_scope`, a request
without a credential `401 unauthenticated`, and on a hub without sandboxes
every authenticated caller gets `404 sandboxes_disabled` (see
[Sandbox routes](#sandbox-routes)).

## `GET /api/v1/sandboxes`

Lists the sandboxes of every workspace on the hub, for the operator. Only
the admin token may call it: every other credential, the API key of a user
with role `admin` and a PAT with `sandboxes:read` and `sandboxes:write`
included, is answered before anything is read with

```json
{"error":{"code":403,"message":"only the admin token may list every sandbox of the hub","error_type":"forbidden"}}
```

A request without a credential gets `401 unauthenticated`, and on a hub
without sandboxes the admin token gets `404 sandboxes_disabled` (see
[Sandbox routes](#sandbox-routes)).

The answer is `200` with a [list](#lists): the sandbox objects as the hub
stores them, newest first by `created_at` and then by `id`, both
descending. It is read from the hub's own records and asks the sandbox
runtime nothing, so it answers the same while the runtime is unreachable.
Until the hub stores sandboxes the list is empty:

```json
{"items":[],"next_cursor":null}
```

| Query parameter | Value |
|---|---|
| `state` | only the sandboxes in this state, one of `provisioning`, `ready`, `stopped`, `completed`, `failed` and `deleted`, written exactly so |
| `user` | only the sandboxes created by the user with this id |
| `limit`, `cursor` | as for every [list](#lists): `limit` from 1 to 200, default 50 |

`state` and `user` together select the sandboxes that match both. Any other
`state`, `limit` or `cursor`, and a cursor of a page with other filters, is
answered `400 invalid_request`. A failure to read the records is `500
internal` with the message `internal error`, and no items.

## User selectors

A route with a user in its path, `/api/v1/users/{id}` or
`/api/v1/orgs/{id}/members/{user_id}`, takes any of the user's three names
there, and its shape alone decides which one it is:

| Value | Looked up as |
|---|---|
| a UUID in its hyphenated form, 36 characters (`0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d`) | the user's `id`, and nothing else |
| any other value with an `@` (`alice@example.com`) | the user's `email`, and nothing else |
| every other value (`alice`) | the user's `username` |

The comparison is exact, letter case included: `ALICE` does not name
`alice`. No second lookup is tried, so a user whose username has the form of
a UUID or holds an `@` is named by the id or the e-mail. A value that names
no user is answered `404` `not_found` with the message `user not found`,
and nothing changes:

```json
{"error":{"code":404,"message":"user not found","error_type":"not_found"}}
```

## `POST /api/v1/users`

Creates a user without OAuth, with the user's personal org. An admin route
(see [Admin routes](#admin-routes)) and a create route, so it accepts an
`Idempotency-Key`.

The body is a JSON object:

| Member | Value |
|---|---|
| `username` | required: a non-empty string |
| `email` | required: a non-empty string |
| `provider` | required: a non-empty string, the identity provider the user will sign in with |
| `provider_id` | required: a non-empty string, the user's id at that provider |
| `full_name` | a string; the empty string when absent |

In one transaction the hub creates:

- the user, with a new UUID as `id`, the role `user` and the status
  `active`. The role is `user` also when the e-mail is the hub's bootstrap
  `admin_email`: only a first login promotes.
- the user's personal org: `status` `active`, named after the username,
  owned by the user, with the slug the first login derives (see
  [`POST /api/v1/auth/callback`](#post-apiv1authcallback)): `Alice.Smith`
  gives `alice-smith`, `42bob` gives `u-42bob`, `-_ab_-` gives `ab` and `a`
  gives `u-` and the first 8 characters of the user id. When an org has
  that slug, the first free of `<slug>-1` to `<slug>-10` is used.
- the user's membership of that org.

The `created_at` and `updated_at` of the user and of the org, and the
`created_at` of the membership, which has no `updated_at`, are the time of
the request.
Usernames, e-mails, provider identities and org names are compared as
stored, letter case included.

| Status | When |
|---|---|
| `201 Created` | the user was created; the body is the User object |
| `400` `invalid_request` | a required member is missing or the empty string (message `<member> is required`), or a member of the table is not a JSON string, `null` included (message `<member> must be a string`); or the body is not a JSON object. Nothing is created. |
| `403` `forbidden` | the caller is not an admin (see [Admin routes](#admin-routes)) |
| `409` `conflict` | the username, the e-mail or the pair of `provider` and `provider_id` is another user's, an org already has the username as its name, or the slug and all of `<slug>-1` to `<slug>-10` are taken. The message says which, in fixed words, such as `the username is taken`. Nothing is created. |

The User object has exactly `id`, `username`, `email`, `full_name`, `role`,
`status`, `provider`, `provider_id`, `created_at` and `updated_at`:

```json
{"id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","username":"carol","email":"carol@example.com","full_name":"Carol C","role":"user","status":"active","provider":"github","provider_id":"1","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}
```

## `GET /api/v1/users`

Lists the users, a [list](#lists) in the standard pages. An admin route
(see [Admin routes](#admin-routes)).

The list holds the users with the status `active`, newest first:
`created_at` descending, then `id` descending. With the query parameter
`include_blocked=true` it holds the blocked users too, in the same order;
any other value, such as `1`, `yes` or `TRUE`, is the same as none. A
`cursor` belongs to its value of `include_blocked`.

| Status | When |
|---|---|
| `200 OK` | the body is a page: `items` are User objects |
| `400` `invalid_request` | a bad `limit` or `cursor` (see [Lists](#lists)) |
| `403` `forbidden` | the caller is not an admin (see [Admin routes](#admin-routes)) |

```json
{"items":[{"id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","username":"alice","email":"alice@example.com","full_name":"","role":"user","status":"active","provider":"github","provider_id":"alice","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}],"next_cursor":null}
```

## `GET /api/v1/users/{id}`

Answers the User object of the user `{id}` names (see
[User selectors](#user-selectors)), active or blocked. An admin route (see
[Admin routes](#admin-routes)).

| Status | When |
|---|---|
| `200 OK` | the body is the User object; a blocked user has the `status` `blocked` |
| `403` `forbidden` | the caller is not an admin (see [Admin routes](#admin-routes)) |
| `404` `not_found` | no user has that id, e-mail or username (`user not found`) |

## `PATCH /api/v1/users/{id}`

Sets the `full_name` of the user `{id}` names (see
[User selectors](#user-selectors)). An admin route (see
[Admin routes](#admin-routes)).

The body follows the rule of [`PATCH /api/v1/user`](#patch-apiv1user): a
JSON object with the member `full_name`, required, a string; the empty
string clears the name, and other members are ignored. The body is checked
before the user is looked up. The hub stores the string, sets `updated_at`
to the time of the change and answers the updated User object.

| Status | When |
|---|---|
| `200 OK` | the name was stored; the body is the updated User object |
| `400` `invalid_request` | `full_name` is missing (`full_name is required`) or is not a JSON string, `null` included (`full_name must be a string`); or the body is not a JSON object. The user is unchanged. |
| `403` `forbidden` | the caller is not an admin (see [Admin routes](#admin-routes)) |
| `404` `not_found` | no user has that id, e-mail or username (`user not found`) |

```json
{"full_name":"Alice B."}
```

## Role and status of a user

`POST /api/v1/users/{id}/promote`, `POST /api/v1/users/{id}/demote`,
`POST /api/v1/users/{id}/block` and `POST /api/v1/users/{id}/unblock`
change the role or the status of the user `{id}` names (see
[User selectors](#user-selectors)). They are admin routes (see
[Admin routes](#admin-routes)) and read no body.

| Route | Sets |
|---|---|
| `POST /api/v1/users/{id}/promote` | the role `admin` |
| `POST /api/v1/users/{id}/demote` | the role `user` |
| `POST /api/v1/users/{id}/block` | the status `blocked` |
| `POST /api/v1/users/{id}/unblock` | the status `active` |

The hub sets `updated_at` to the time of the change and answers the
updated User object; every other member is unchanged. A user who already
has the requested role or status is answered as stored, `updated_at`
included, and nothing changes.

Blocking and unblocking revoke and restore no credential. While a user is
blocked, every API key and PAT of the user is answered `403` `forbidden`
(`user is blocked`, see [Authentication](#authentication)); once the user
is unblocked, the same credentials pass again.

The hub never loses its last active admin to a demotion: demoting the only
user with both the role `admin` and the status `active` is `409` `conflict`,
and the role stays `admin`. Blocked admins do not count. The count and the
demotion run in one write transaction, so of two concurrent demotions of
the last two active admins one is refused; the same holds for a demotion
and a [deletion](#delete-apiv1usersid) running together. Blocking has no
such safeguard, because the admin token can always unblock: blocking the
only active admin answers `200`.

| Status | When |
|---|---|
| `200 OK` | the role or status is set, or was already; the body is the User object |
| `403` `forbidden` | the caller is not an admin (see [Admin routes](#admin-routes)) |
| `404` `not_found` | no user has that id, e-mail or username (`user not found`) |
| `409` `conflict` | demote only: the user is the only active admin (`the user is the only active admin`). The role is unchanged. |

```json
{"error":{"code":409,"message":"the user is the only active admin","error_type":"conflict"}}
```

## `DELETE /api/v1/users/{id}`

Deletes the user `{id}` names (see [User selectors](#user-selectors)),
active or blocked, whatever the role. An admin route (see
[Admin routes](#admin-routes)).

In one transaction the hub deletes the user, every API key and every PAT of
the user, workspace-bound ones included, and every membership of the user,
and clears `owner_id` on the orgs the user owned. Those orgs, the user's
personal org among them, are kept: their `owner_id` is `null` from then on
and their `updated_at` is the time of the delete, and their other members
stay. Afterwards the user is `404` `not_found` by id, e-mail and username
and is gone from `GET /api/v1/users?include_blocked=true`, and a request
with any of the user's former API keys or PATs is answered `401`
`unauthenticated` (`invalid credentials`).

The hub never loses its last active admin to a deletion: deleting the only
user with both the role `admin` and the status `active`, also by that
user's own API key, is `409` `conflict` and deletes nothing. The check and
the delete run in one write transaction, like a demotion's (see
[Role and status of a user](#role-and-status-of-a-user)).

Data that other parts of the hub keep for the user, the user's
[secrets](#secrets) among them, is removed in the same
transaction, before the user (see [ADR 03](adr/03-register-removers-of-owned-data-on-the-identity-service.md)).
When that removal fails, the delete is undone as a whole, and the answer is
`500` `internal`.

| Status | When |
|---|---|
| `204 No Content` | the user was deleted; the body is empty |
| `403` `forbidden` | the caller is not an admin (see [Admin routes](#admin-routes)) |
| `404` `not_found` | no user has that id, e-mail or username (`user not found`). Nothing changes. |
| `409` `conflict` | the user is the only active admin (`the user is the only active admin`). Nothing changes. |
| `500` `internal` | removing data another part of the hub keeps for the user failed (`internal error`). Nothing changes. |

## Keys and tokens of a user

`GET /api/v1/users/{id}/keys`, `GET /api/v1/users/{id}/tokens`,
`DELETE /api/v1/users/{id}/keys/{key_id}` and
`DELETE /api/v1/users/{id}/tokens/{token_id}` let an admin see and revoke
the API keys and PATs of any user, without the user. They are admin routes
(see [Admin routes](#admin-routes)) and declare no scope: every PAT, a PAT
of an admin holding every scope included, is answered `403` `forbidden`
(`this route needs an admin`), as is an API key whose user has the role
`user`, also on that user's own keys and tokens. `{id}` is a user selector
(see [User selectors](#user-selectors)), resolved before anything else the
path names.

Each answers what the route of the caller's own keys or tokens answers, for
the user `{id}` names:

- `GET /api/v1/users/{id}/keys` is a [list](#lists) of every API key of the
  user in any state, newest first, as
  [`GET /api/v1/user/keys`](#get-apiv1userkeys) lists them.
- `GET /api/v1/users/{id}/tokens` is a list of every PAT of the user in any
  state, workspace-bound ones included, newest first, as
  [`GET /api/v1/user/tokens`](#get-apiv1usertokens) lists them, without the
  tokens' values.
- `DELETE /api/v1/users/{id}/keys/{key_id}` and
  `DELETE /api/v1/users/{id}/tokens/{token_id}` revoke the key or token as
  [`DELETE /api/v1/user/keys/{key_id}`](#delete-apiv1userkeyskey_id) and
  [`DELETE /api/v1/user/tokens/{token_id}`](#delete-apiv1usertokenstoken_id)
  do: `revoked_at` is set to the time of the request, an expired one
  included, and the credential no longer authenticates; one already revoked
  is answered as it is, with its earlier `revoked_at`. A `key_id` or
  `token_id` that names nothing and one that names another user's key or
  token get the same `404`.

| Status | When |
|---|---|
| `200 OK` | a list: the body is a page whose `items` are Key or Token objects; a revocation: the body is the Key or Token object with `revoked_at` set |
| `400` `invalid_request` | a list with a bad `limit` or `cursor` (see [Lists](#lists)) |
| `403` `forbidden` | the caller is not an admin (`this route needs an admin`). Nothing changes. |
| `404` `not_found` | no user has that id, e-mail or username (`user not found`); the user has no API key with this id (`API key not found`); the user has no token with this id (`token not found`). Nothing changes. |

## Org selectors

A route with an org in its path, `/api/v1/orgs/{id}`, takes the org's id or
its slug there, and its shape alone decides which one it is:

| Value | Looked up as |
|---|---|
| a UUID in its hyphenated form, 36 characters (`0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d`) | the org's `id`, and nothing else |
| every other value (`acme`) | the org's `slug` |

The comparison is exact, letter case included: `ACME` does not name `acme`,
and an org's `name` never names it. A value that names no org is answered
`404` `not_found` with the message `org not found`, and nothing changes:

```json
{"error":{"code":404,"message":"org not found","error_type":"not_found"}}
```

## `POST /api/v1/orgs`

Creates an org. An admin route (see [Admin routes](#admin-routes)) and a
create route, so it accepts an `Idempotency-Key`.

The body is a JSON object:

| Member | Value |
|---|---|
| `name` | required: a string that is not empty once trimmed of leading and trailing white space; stored trimmed |
| `slug` | required: 1 to 128 characters of `a`-`z`, `0`-`9`, `_` and `-` that start and end with a letter or digit |
| `url` | a string; the empty string when absent |
| `owner_id` | a string, the `id` of an existing user (not a username or an e-mail); no owner when absent |

The org is created with a new UUID as `id`, the status `active` and
`created_at` and `updated_at` the time of the request. It has no member,
also when it has an owner: the owner is informational. Names and slugs are
compared as stored, letter case included, so `acme inc` is free beside
`Acme Inc`.

| Status | When |
|---|---|
| `201 Created` | the org was created; the body is the Org object |
| `400` `invalid_request` | `name` or `slug` is missing (`name is required`, `slug is required`); a member of the table is not a JSON string, `null` included (`<member> must be a string`); the name is white space only (`name must not be empty`); the slug has another form (`slug must be 1 to 128 characters of a-z, 0-9, _ and -, starting and ending with a letter or digit`); `owner_id` is not the id of a user (`owner_id is not the id of a user`); or the body is not a JSON object. Nothing is created. |
| `403` `forbidden` | the caller is not an admin (see [Admin routes](#admin-routes)) |
| `409` `conflict` | another org has the trimmed name (`an org has the name`) or the slug (`an org has the slug`). Nothing is created. |

```json
{"name":"Acme Inc","slug":"acme","url":"https://acme.example"}
```

The Org object has exactly `id`, `name`, `slug`, `url`, `owner_id`,
`status`, `created_at` and `updated_at`; `owner_id` is `null` when the org
has no owner:

```json
{"id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","name":"Acme Inc","slug":"acme","url":"https://acme.example","owner_id":null,"status":"active","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}
```

## `GET /api/v1/orgs`

Lists the orgs, a [list](#lists) in the standard pages. An admin route
(see [Admin routes](#admin-routes)).

The list holds the orgs with the status `active`, ordered by `name`
ascending, compared byte by byte, so `Zed` comes before `alpha`. With the
query parameter `include_blocked=true` it holds the blocked orgs too, in the
same order; any other value, such as `false` or `1`, is the same as none. A
`cursor` belongs to its value of `include_blocked`.

| Status | When |
|---|---|
| `200 OK` | the body is a page: `items` are Org objects |
| `400` `invalid_request` | a bad `limit` or `cursor` (see [Lists](#lists)) |
| `403` `forbidden` | the caller is not an admin (see [Admin routes](#admin-routes)) |

## `GET /api/v1/orgs/{id}`

Answers the Org object of the org `{id}` names (see
[Org selectors](#org-selectors)). Every authenticated caller may send it;
it declares the scope `orgs:read` for a PAT. An admin, the admin token or an
API key whose user has the role `admin`, receives any org, a blocked one
with the status `blocked`. Every other caller, a PAT of an admin included,
receives the org only when the caller's user is a member of it and the org
is `active`; otherwise the answer is the one for an org that does not
exist, so a caller cannot tell an org it may not read from a missing one.

| Status | When |
|---|---|
| `200 OK` | the body is the Org object |
| `403` `insufficient_scope` | a PAT without `orgs:read` or a scope that implies it (see [Scopes](#scopes)) |
| `404` `not_found` | no org has that id or slug, or the caller is not an admin and is not a member of the org or the org is blocked (`org not found`) |

## `PATCH /api/v1/orgs/{id}`

Sets the `name`, the `url` or both of the org `{id}` names (see
[Org selectors](#org-selectors)). An admin route (see
[Admin routes](#admin-routes)).

The body is a JSON object with `name`, a string that is not empty once
trimmed, stored trimmed, and `url`, a string, the empty string included; at
least one of them. A `slug` member is ignored like any other unknown
member: the slug never changes. The body is checked before the org is
looked up. The hub stores the members sent, sets `updated_at` to the time
of the change and answers the updated Org object; every other member is
unchanged. The org's own current name is accepted.

| Status | When |
|---|---|
| `200 OK` | the org was updated; the body is the updated Org object |
| `400` `invalid_request` | neither `name` nor `url` is sent (`name or url is required`), one is not a JSON string, `null` included (`<member> must be a string`), or the name is white space only (`name must not be empty`); or the body is not a JSON object. The org is unchanged. |
| `403` `forbidden` | the caller is not an admin (see [Admin routes](#admin-routes)) |
| `404` `not_found` | no org has that id or slug (`org not found`) |
| `409` `conflict` | another org has the trimmed name (`an org has the name`). The org is unchanged, its `url` included. |

```json
{"name":"Acme Corp","url":"https://acme.example"}
```

## `GET /api/v1/orgs/{id}/members`

Lists the members of the org `{id}` names (see
[Org selectors](#org-selectors)), a [list](#lists) in the standard pages.
Who may read it is decided as for
[`GET /api/v1/orgs/{id}`](#get-apiv1orgsid): an admin, for any org, a
blocked one included, and every other caller, a PAT of an admin included,
only when its user is a member of the org and the org is `active`. Every
other caller gets the answer for an org that does not exist. It declares the
scope `orgs:read` for a PAT. The `limit` and `cursor` are checked before the
org is looked up, so a bad one is `400` for every caller, whatever `{id}`
names, a missing org included, and tells nothing of the org.

The list holds one item per membership, ordered by `username` ascending,
compared byte by byte. An item has exactly `org_id`, `user_id`, `username`,
`email` and `created_at`, the time the membership was created (not the
user's). An org without members gets `items` `[]`.

| Status | When |
|---|---|
| `200 OK` | the body is a page of member items |
| `403` `insufficient_scope` | a PAT without `orgs:read` or a scope that implies it (see [Scopes](#scopes)), checked before anything else |
| `400` `invalid_request` | a bad `limit` or `cursor` (see [Lists](#lists)), checked first after the scope, for an org that does not exist too |
| `404` `not_found` | the `limit` and `cursor` are good, and no org has that id or slug, or the caller is not an admin and is not a member of the org or the org is blocked (`org not found`) |

```json
{"items":[{"org_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","user_id":"1b2c3d4e-5f6a-4b7c-8d9e-0f1a2b3c4d5e","username":"alice","email":"alice@example.com","created_at":"2026-01-01T00:02:00Z"}],"next_cursor":null}
```

## `PUT /api/v1/orgs/{id}/members/{user_id}`

Makes the user `{user_id}` names (see [User selectors](#user-selectors)) a
member of the org `{id}` names (see [Org selectors](#org-selectors)). An
admin route (see [Admin routes](#admin-routes)); it reads no body.

The membership is created with `created_at` the time of the request. A
user who is a member already keeps the membership as it is, its
`created_at` included. Blocked users and blocked orgs are accepted; the org
appears in the user's [`GET /api/v1/user/orgs`](#get-apiv1userorgs) while
it is `active`.

| Status | When |
|---|---|
| `204 No Content` | the user is a member; the body is empty |
| `403` `forbidden` | the caller is not an admin (see [Admin routes](#admin-routes)) |
| `404` `not_found` | no org has that id or slug (`org not found`), or no user has that id, e-mail or username (`user not found`); the org is looked up first |

## `DELETE /api/v1/orgs/{id}/members/{user_id}`

Ends the membership of the user `{user_id}` names (see
[User selectors](#user-selectors)) in the org `{id}` names (see
[Org selectors](#org-selectors)). An admin route (see
[Admin routes](#admin-routes)).

The user and the org are kept, and the org's `owner_id` is left as it is,
also when the user removed is the owner: the owner is informational.

| Status | When |
|---|---|
| `204 No Content` | the membership was deleted; the body is empty |
| `403` `forbidden` | the caller is not an admin (see [Admin routes](#admin-routes)) |
| `404` `not_found` | no org has that id or slug (`org not found`), no user has that id, e-mail or username (`user not found`), or the user is not a member of the org (`membership not found`). Nothing changes. |

```json
{"error":{"code":404,"message":"membership not found","error_type":"not_found"}}
```

## Status of an org

`POST /api/v1/orgs/{id}/block` sets the status `blocked` and
`POST /api/v1/orgs/{id}/unblock` the status `active` on the org `{id}`
names (see [Org selectors](#org-selectors)). Both are admin routes (see
[Admin routes](#admin-routes)) and read no body.

The hub sets `updated_at` to the time of the change and answers the
updated Org object; every other member is unchanged. An org that already
has the requested status is answered as stored, `updated_at` included, and
nothing changes.

The memberships of a blocked org are kept. While it is blocked, the org is
left out of its members' [`GET /api/v1/user/orgs`](#get-apiv1userorgs) and
of [`GET /api/v1/orgs`](#get-apiv1orgs) without `include_blocked=true`, and
only an admin reads it and its member list (see
[`GET /api/v1/orgs/{id}`](#get-apiv1orgsid)). Once it is unblocked, it is
listed in both again, with the same members.

| Status | When |
|---|---|
| `200 OK` | the status is set, or was already; the body is the Org object |
| `403` `forbidden` | the caller is not an admin (see [Admin routes](#admin-routes)) |
| `404` `not_found` | no org has that id or slug (`org not found`) |

## `DELETE /api/v1/orgs/{id}`

Deletes the org `{id}` names (see [Org selectors](#org-selectors)), a
personal org or a blocked one included. An admin route (see
[Admin routes](#admin-routes)).

The org and all its memberships are deleted in one transaction. The users
who were its members or its owner are kept. Afterwards the org is `404`
`not_found` by its id and by its slug, for admins too, it is gone from its
former members' [`GET /api/v1/user/orgs`](#get-apiv1userorgs), and a new
org may take its name and its slug.

Data that other parts of the hub keep for the org, the org's
[secrets](#secrets) among them, is removed in the same
transaction (see [ADR 03](adr/03-register-removers-of-owned-data-on-the-identity-service.md)).
When that removal fails, the delete is undone as a whole, and the answer is
`500` `internal`.

| Status | When |
|---|---|
| `204 No Content` | the org was deleted; the body is empty |
| `403` `forbidden` | the caller is not an admin (see [Admin routes](#admin-routes)) |
| `404` `not_found` | no org has that id or slug (`org not found`). Nothing changes. |
| `500` `internal` | removing data another part of the hub keeps for the org failed (`internal error`). Nothing changes. |

## Secrets

A secret belongs to one of three secret scopes, each with its own routes:

| Scope | Routes | Owner |
|---|---|---|
| user | `/api/v1/user/secrets` | the caller's user |
| org | `/api/v1/orgs/{slug}/secrets` | the org `{slug}` names, by its slug or its id (see [Org selectors](#org-selectors)) |
| workspace | `/api/v1/workspaces/{slug}/secrets` | the workspace `{slug}`, and no other: a task fork's secrets are not its parent's, nor the other way round |

No route answers a secret's value. A route's access to its scope is decided
in the access position of the [order of checks](#order-of-checks), after the
scope of a PAT and the [workspace access](#workspace-access) check, so a PAT
without the route's scope is refused `403` `insufficient_scope` whether or
not the org or the workspace exists. Every secret route of a scope gives the
same answers:

- **User.** An API key or a PAT acts on its user's secrets. The admin token
  has no user and is refused:

  ```json
  {"error":{"code":403,"message":"user-scoped endpoints require a user credential","error_type":"forbidden"}}
  ```

- **Org.** The admin token reaches every org, a blocked one included; a user
  reaches an org with the status `active` of which they are a member. An org
  that does not exist, an org the user is not a member of and a blocked org
  are answered with the same bytes:

  ```json
  {"error":{"code":404,"message":"org not found","error_type":"not_found"}}
  ```

- **Workspace.** The routes need `owner` (see [Workspace access](#workspace-access)):
  the workspace's owner (on a task fork also its parent's owner) and the
  admin token reach them; a member of the workspace's org is answered `403`
  `forbidden` with the message `org members have read-only access to this
  workspace`, the creator of a task fork on the fork's parent included; anyone
  else gets the `404` `workspace not found` of a slug with no workspace. The
  workspace's status plays no part.

A workspace-bound PAT is confined by the binding guard first (see
[Workspace-bound tokens](#workspace-bound-tokens)).

The secrets of a user go with the user's deletion
([`DELETE /api/v1/users/{id}`](#delete-apiv1usersid)) and those of an org with
the org's ([`DELETE /api/v1/orgs/{id}`](#delete-apiv1orgsid)), in the
transaction of the delete and without a `parley.secret.delete` event; a
delete that is refused, such as that of the only active admin, keeps them.

## `GET /api/v1/user/secrets`, `GET /api/v1/orgs/{slug}/secrets`, `GET /api/v1/workspaces/{slug}/secrets`

Lists the secrets of the scope, a [list](#lists) in the standard pages, and
declares the scope `secrets:read` for a PAT, which `secrets:write`
satisfies. Who may call each route is in [Secrets](#secrets).

The list holds the scope's own secrets, ordered by `key` ascending, compared
byte by byte with `A` to `Z` lowered, so `_x`, `A`, `a1`, `b`, `B2`, `c` are
in that order. Each item has exactly `key`, as it was first stored,
`created_at` and `updated_at`, RFC 3339 in UTC with whole seconds; never the
value or the owner. An empty scope gets `items` `[]`. The list reads no
value, so it holds every secret also when the hub has no encryption key or
a value cannot be decrypted with the configured keys.

```json
{"items":[{"key":"GIT_PAT","created_at":"2026-11-14T10:30:15Z","updated_at":"2026-11-14T10:30:15Z"}],"next_cursor":null}
```

| Status | When |
|---|---|
| `200 OK` | the body is a page: `items` are secrets as above |
| `400` `invalid_request` | a bad `limit` (`limit must be an integer from 1 to 200`) or `cursor` (see [Lists](#lists)) |
| `403` `insufficient_scope` | a PAT without `secrets:read` or `secrets:write` (`this route needs the scope secrets:read`) |
| `403` `forbidden` | the admin token on `/user/secrets` (`user-scoped endpoints require a user credential`); a member of the workspace's org on `/workspaces/{slug}/secrets` (`org members have read-only access to this workspace`) |
| `404` `not_found` | an org the caller may not reach or that does not exist (`org not found`); a workspace the caller has no level on or that does not exist (`workspace not found`) |

## `POST /api/v1/user/secrets`, `POST /api/v1/orgs/{slug}/secrets`, `POST /api/v1/workspaces/{slug}/secrets`

Creates secrets in the scope, all of them or none, and declares the scope
`secrets:write` for a PAT. Who may call each route is in
[Secrets](#secrets). It is a create route (see [Idempotency](#idempotency)).

The body is a JSON object with `entries`, an array of at least one object
with `key` and `value`; other members are ignored:

| Member | Required | Value |
|---|---|---|
| `key` | yes | a string of 1 to 255 characters, each an ASCII letter, a digit or `_`, not beginning with a digit; stored as given |
| `value` | no | a string of at most 262144 bytes; absent, the empty string. `null` or another type is refused |

```json
{"entries":[{"key":"GIT_PAT","value":"ghp_…"},{"key":"GIT_USERNAME","value":"octocat"}]}
```

The body is checked in this order, and the first fault is answered:
`entries` is a non-empty array; entry by entry, its key and then its value;
then no two keys are equal ignoring the case of `A` to `Z`. With a valid
body, a hub without a current encryption key answers `503`. Then, in one
transaction: when the scope's secrets and the entries together would be more
than 100, `429`, also when a key already exists; otherwise, when the scope
already holds a key equal to one of the entries ignoring case, `409` naming
the first such key in request order, as the request spells it. Otherwise
every entry is stored, with one creation time, and one event
`parley.secret.create` is written per entry in the same transaction:
`resource_type` `secret`, `resource_id` the key, `metadata` exactly
`{"scope","scope_id","key"}` (`scope_id` the user id, the org id or the
workspace slug), `workspace` the slug on the workspace route and `null`
otherwise, and the caller as the actor. If the events cannot be written,
nothing is stored and the answer is `500`.

The answer holds the entries in request order, as the items of a list:
exactly `key`, `created_at` and `updated_at`, which are equal, the same for
every item, RFC 3339 in UTC with whole seconds; never a value.

```json
{"items":[{"key":"GIT_PAT","created_at":"2026-11-14T10:30:15Z","updated_at":"2026-11-14T10:30:15Z"},{"key":"GIT_USERNAME","created_at":"2026-11-14T10:30:15Z","updated_at":"2026-11-14T10:30:15Z"}]}
```

| Status | When |
|---|---|
| `201 Created` | every entry was stored; the body is `items` as above |
| `200 OK` | a replay of a recorded `Idempotency-Key` with the same body: the first answer; nothing is stored |
| `400` `invalid_request` | the body is refused; nothing is stored. The messages: `entries must be a non-empty array of {key, value} objects`, `entries[0] must be an object`, `entries[0] must have a key that is a string`, `key "" must have 1 to 255 characters`, `key "1A" must not begin with a digit`, `key "A-B" may hold only ASCII letters, digits and _`, `the value of key "K" must be a string`, `the value of key "K" is longer than 262144 bytes`, `key "TOKEN" is given more than once, ignoring case` |
| `403` `insufficient_scope` | a PAT without `secrets:write` (`this route needs the scope secrets:write`) |
| `403` `forbidden` | as for the list (see above) |
| `404` `not_found` | as for the list (see above) |
| `409` `conflict` | the scope holds one of the keys, ignoring case (`key "TOKEN" already exists`); nothing is stored |
| `413` `payload_too_large` | the body is over the hub's body limit (see [Request bodies](#request-bodies)) |
| `429` `secret_limit_reached` | the scope would hold more than 100 secrets (`maximum of 100 entries per scope exceeded`); nothing is stored |
| `503` `secrets_unavailable` | the hub has no current encryption key (`secrets cannot be stored: no encryption key configured`); nothing is stored |

## `PATCH /api/v1/user/secrets/{key}`, `PATCH /api/v1/orgs/{slug}/secrets/{key}`, `PATCH /api/v1/workspaces/{slug}/secrets/{key}`

Replaces the value of the scope's secret whose key equals `{key}` ignoring
the case of `A` to `Z`, and declares the scope `secrets:write` for a PAT.
Who may call each route is in [Secrets](#secrets). `{key}` is not checked
against the key rules: a key no secret can have is not found.

The body is a JSON object with `value`, a string of at most 262144 bytes;
other members are ignored:

```json
{"value":"ghp_…"}
```

The body is checked first, then a hub without a current encryption key
answers `503`, for a key the scope does not hold too; then the secret is
looked up. The new value is sealed under the current key without opening
the old one, so a value the configured keys cannot decrypt is replaced too.
The key keeps the casing it was first stored with and its `created_at`;
`updated_at` is the time of the update. One event `parley.secret.update` is
written in the same transaction, as for a create, with `resource_id` and
`metadata.key` the key as it is stored, whatever casing the path used. If
the event cannot be written, nothing changes and the answer is `500`.

The answer is the secret as an item of the list: exactly `key`, as stored,
`created_at` and `updated_at`; never a value.

```json
{"key":"GIT_PAT","created_at":"2026-11-14T10:30:15Z","updated_at":"2026-11-14T10:31:45Z"}
```

| Status | When |
|---|---|
| `200 OK` | the value was replaced; the body is the item as above |
| `400` `invalid_request` | `value` is absent, `null`, not a string (`the value of key "K" must be a string`) or longer than 262144 bytes (`the value of key "K" is longer than 262144 bytes`), `K` the key as the path gives it; or the body is not a JSON object (see [Request bodies](#request-bodies)). Nothing changes |
| `403` `insufficient_scope` | a PAT without `secrets:write` (`this route needs the scope secrets:write`) |
| `403` `forbidden` | as for the list (see above) |
| `404` `not_found` | as for the list (see above); or the scope holds no such key (`key "K" not found`, `K` as the path gives it). A key held only by another scope, a task fork's parent included, is not found |
| `503` `secrets_unavailable` | the hub has no current encryption key (`secrets cannot be stored: no encryption key configured`); nothing changes |

## `DELETE /api/v1/user/secrets/{key}`, `DELETE /api/v1/orgs/{slug}/secrets/{key}`, `DELETE /api/v1/workspaces/{slug}/secrets/{key}`

Removes the scope's secret whose key equals `{key}` ignoring the case of `A`
to `Z`, and declares the scope `secrets:write` for a PAT. Who may call each
route is in [Secrets](#secrets). The value is never read, so the route works
on a hub without an encryption key and on a value the configured keys
cannot decrypt. One event `parley.secret.delete` is written in the same
transaction, named as for an update. If the event cannot be written, the
secret stays and the answer is `500`. A removed key can be created again.

| Status | When |
|---|---|
| `204 No Content` | the secret was removed; the body is empty |
| `403` `insufficient_scope` | a PAT without `secrets:write` (`this route needs the scope secrets:write`) |
| `403` `forbidden` | as for the list (see above) |
| `404` `not_found` | as for the update (see above) |

## `GET /api/v1/events`

Streams the hub events the caller may see as Server-Sent Events: first the
events after a resume position, then, on the same connection, every event
committed afterwards, in ascending `seq`, with no gap and no repeat between
the two. The route declares the scope `audit:read` for a PAT, which
`audit:write` satisfies; the admin token and API keys are not held to it. It
has no workspace in its path.

| Parameter | Meaning |
|---|---|
| `Last-Event-ID` (header) | the resume position: the stream sends the events with a higher `seq`. A non-negative integer, at most `last_seq`; it wins over `after`, which is then not looked at |
| `after` | the resume position when there is no `Last-Event-ID` header, by the same rules. Without either, the stream starts at `last_seq` when it opens and sends only events committed after that |
| `workspace`, `types`, `session_id` | the filters, as on [`GET /api/v1/events/log`](#get-apiv1eventslog) |

Other query parameters are ignored. The parameters are checked as on
[`GET /api/v1/events/log`](#get-apiv1eventslog), in the same order, and
every refusal is answered before the stream begins, as the JSON error
envelope; no stream is started. The answer is `200` with
`Content-Type: text/event-stream` and `Cache-Control: no-cache`, and its first
frame, sent at once, is a heartbeat. Each frame is three lines and an empty
line:

```text
id: 3
event: heartbeat
data: {"time":"2026-11-14T09:30:15Z","seq":3}

id: 4
event: hub_event
data: {"seq":4,"id":"…","type":"parley.change.open","time":"2026-11-14T09:30:20Z","workspace":"api","actor_type":"pat","actor_id":"u_1","resource_type":"change","resource_id":"42","action":"open","session_id":null,"metadata":{}}

```

| Frame | `id` | `data` |
|---|---|---|
| `hub_event` | the event's `seq` | the hub event record as stored, on one line, the same bytes as an item of the log |
| `heartbeat` | the position up to which the log has been examined for this client | `time`, the hub's time at whole seconds in UTC, and `seq`, the same position |

A heartbeat is sent when the stream opens and then every 30 seconds. Every
visible, matching event at or below a heartbeat's `seq` was sent before it,
and its `seq` rises with `last_seq` also when no event matched the filters or
was visible, so a client that reconnects with the `id` of the last frame it
received, as `Last-Event-ID`, resumes exactly where it stopped. An event
committed while the stream is open is sent within a second of its commit.
Events are decided with the caller's access at the time they are delivered,
by the rules of the log.

The stream ends, and the hub closes the connection, when:

- the client closes the connection;
- the hub begins to stop: every stream ends at once, also one whose client
  is not reading;
- at a heartbeat, the credential no longer authenticates, as a new request
  with it would not (revoked, expired, or its user blocked): no further
  frame is sent, so the stream ends at most 30 seconds after the change;
- 256 events are waiting for a client that reads more slowly than they come
  and another one is due;
- a write to the client has not completed for 60 seconds;
- the stream's position fell below `oldest_seq` minus 1 since its previous
  read of the log, because events after it left the log while it was open:
  no further frame is sent, rather than skip those events, and a reconnect
  with the `id` of the last frame received is answered `410`
  `cursor_expired` with `oldest_seq`.

The hub never drops an event from an open stream: the `hub_event` frames a
client received before the end are a gap-free prefix of the visible,
matching events, and a reconnect with the `id` of the last frame received
sends every later one.

At most `[events].max_sse_connections` streams are open at once (see
[configuration.md](configuration.md#keys)); a held long-poll of
[`GET /api/v1/events/log`](#get-apiv1eventslog) does not count toward the
limit. A stream gives its place back when it ends, for any reason.

| Status | When |
|---|---|
| `200 OK` | the stream; an error after it began ends the connection. When the caller's access cannot be read while an event is decided, the stream ends without a frame for that event |
| `400` `invalid_request` | a malformed filter, as on the log; an `after` or `Last-Event-ID` that is not a non-negative integer (`after must be a non-negative integer`, `Last-Event-ID must be a non-negative integer`) or is above `last_seq` (`after must not be greater than last_seq (10)`, `Last-Event-ID must not be greater than last_seq (10)`); or a query string that does not parse (`the query string is malformed`). No stream is started |
| `403` `insufficient_scope` | a PAT without `audit:read` or `audit:write` (`this route needs the scope audit:read`), before any parameter is looked at |
| `404` `not_found` | a `workspace` the caller may not read, as on the log (`workspace not found`). No stream is started |
| `410` `cursor_expired` | the resume position is below `oldest_seq` minus 1, as on the log (`event cursor expired`, with `oldest_seq`). No stream is started |
| `500` `internal` | the log's bounds or the caller's access could not be read before the stream began (`internal error`) |
| `503` `unavailable` | `[events].max_sse_connections` streams are open (`too many event streams are open`), with the header `Retry-After: 5`. No stream is started |

## `GET /api/v1/events/log`

Answers one page of the event log: the hub events the caller may see, in
ascending `seq`, after a resume position. The route declares the scope
`audit:read` for a PAT, which `audit:write` satisfies; the admin token and API
keys are not held to it. It has no workspace in its path.

| Parameter | Meaning |
|---|---|
| `after` | the resume position: the page holds events with a higher `seq`. A non-negative integer from `oldest_seq` minus 1 to `last_seq` (0 on a hub that has written no event). Without it (and without `cursor`) the page starts at `oldest_seq`, the oldest event in the log |
| `cursor` | another name for `after`; both may be given only with the same value |
| `limit` | the most items the page holds: an integer from 1 to 200, 50 when absent |
| `wait` | how long to hold the request when there is nothing to return: a whole number of seconds from 0 to 60, 0 when absent |
| `workspace` | only events whose `workspace` is this slug. The caller must be able to read the workspace: the admin token may name any slug, also one whose workspace was deleted, and reads the events stored under it; a workspace-bound PAT only a workspace its binding covers, every other `workspace` it sends is `404`; every other caller only a workspace on which it has a level |
| `types` | only events whose `type` matches one of these comma-separated entries: an exact type, or a prefix ending in `*` that matches every type beginning with the text before it (`*` alone matches every type). At most 20 entries, none empty, with `*` only as an entry's last character |
| `session_id` | only events whose `session_id` is this UUID, in any letter case |

The filters apply together; a `types` entry no event has gives an empty
page, not an error. Other query parameters are ignored. The request is
checked in this order, and the first refusal is the answer: the scope of a
PAT, the parameters, the resume position against the bounds of the log,
then the `workspace` filter against the caller's access. A position below
`oldest_seq` minus 1 means at least one event after it has left the log,
and is refused with `410` whether or not the caller could have seen the
events that left:

```json
{"error":{"code":410,"message":"event cursor expired","error_type":"cursor_expired","oldest_seq":6}}
```

```json
{"items":[{"seq":11,"id":"…","type":"parley.change.open","time":"2026-11-14T10:30:15Z","workspace":"api","actor_type":"pat","actor_id":"u_1","resource_type":"change","resource_id":"42","action":"open","session_id":null,"metadata":{}}],"next_cursor":null,"next_after":120,"oldest_seq":11,"last_seq":120}
```

| Member | Value |
|---|---|
| `items` | the visible, matching events after the position, at most `limit`, each the hub event record as stored; `[]` when there is none, never `null` |
| `next_after` | the position to send as `after` for the next page: the `seq` of the last item when the page holds `limit` items, else the position up to which the log was examined, which is then `last_seq` |
| `next_cursor` | `next_after` as a string when the page holds `limit` items, else `null` |
| `oldest_seq`, `last_seq` | the bounds of the event log when the page was read: the lowest `seq` the log holds and the last `seq` written, both 0 while no event was ever written |

Following `next_after` from page to page returns every visible, matching
event exactly once, also when the events in between are ones the caller may
not see or the filters exclude. Items are decided with the caller's access at
the time the page is read: the admin token sees every event; a user sees the
events of a workspace on which, or on whose fork parent, it has a level
(`owner` or `admin` for the types beginning with `parley.secret.`,
`parley.token.` and `parley.webhook.`), and an event of no workspace when its `metadata.scope`
is `user` with the user's id, or `org` with an active org the user is a
member of. A workspace-bound PAT sees, under its user's level, only the
events of the workspaces its binding covers, and no event of no workspace.

**Long-poll.** With `wait` above 0 and nothing to return, the hub holds the
request until a visible, matching event is committed, and then answers with
it, within a second of the commit. An event the caller may not see or the
filters exclude does not end the wait. When `wait` seconds have passed, the
answer is `200` with `items` `[]` and `next_after` moved to the position
examined, `last_seq` at that time. When the hub begins to stop, every held
request is answered at once, `200` with `items` `[]`. A held request whose
client goes away stops waiting.

A position that was valid when the request was checked can fall below
`oldest_seq` minus 1 before the page is read, when events after it leave the
log while the request is held or its page is being collected. The answer is
then `410` `cursor_expired` with `oldest_seq`, as for a position refused at
once, and never a page that skips the events that left. A request without a
position starts at the oldest event the log holds when its page is first
read.

| Status | When |
|---|---|
| `200 OK` | the body is a page of the log |
| `400` `invalid_request` | a `types` with more than 20 entries (`types has more than 20 entries`), an empty value or an empty entry (`types has an empty entry`), or an entry with `*` anywhere but last (`types entry "parley.*.land" has a * that is not its last character`); a `session_id` that is not a UUID (`session_id must be a UUID`); a `limit` that is not an integer from 1 to 200 (`limit must be an integer from 1 to 200`), a `wait` that is not a whole number of seconds from 0 to 60 (`wait must be a whole number of seconds from 0 to 60`), an `after` or `cursor` that is not a non-negative integer (`after must be a non-negative integer`) or is above `last_seq` (`after must not be greater than last_seq (10)`), `after` and `cursor` with different values (`after and cursor name different positions`), or a query string that does not parse (`the query string is malformed`). Answered at once; nothing is held |
| `403` `insufficient_scope` | a PAT without `audit:read` or `audit:write` (`this route needs the scope audit:read`), before any parameter is looked at |
| `404` `not_found` | a `workspace` the caller may not read (`workspace not found`): the same answer for an existing workspace and for a slug with no workspace |
| `410` `cursor_expired` | the resume position is below `oldest_seq` minus 1 (`event cursor expired`), with the member `oldest_seq`, a number; also when it falls below while the request is held or its page is read |
| `500` `internal` | the log or the caller's access could not be read (`internal error`); no item is sent |
