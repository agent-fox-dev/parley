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
`changes/by-commit/{sha}`, not to `changes/{number}/stack`. A route's last
parameter may take the rest of the path: `{name}` in
`GET /api/v1/workspaces/{slug}/scm/refs/{name}` matches one or more
segments, none of them empty, and its value is their unescaped texts joined
by `/`, so `…/scm/refs/feature/x` has `name` `feature/x`; a route with a
fixed segment or a single-segment parameter in its place serves a path both
match. A request that no route serves is answered as follows:

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
- A git request is counted too, against the same keys, as in
  [Git over HTTP](#git-over-http).
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

Every route that answers a list answers one page of it, `200` with these
two members first:

```json
{"items":[{"id":2},{"id":4}],"next_cursor":"eyJwIjo0LCJmIjoi..."}
```

| Member | Value |
|---|---|
| `items` | an array of the page's items, in the list's order; `[]` when the page is empty, never `null` |
| `next_cursor` | a string when more items follow, `null` on the last page |

A list that names more than its items, such as the commit it was read
from, adds members after `next_cursor`, and its section of this page names
them; every other list has these two alone.

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

## Git over HTTP

Outside `/api/v1`, the hub speaks git smart HTTP on three routes:

| Route | What |
|---|---|
| `GET /git/{org}/{slug}.git/info/refs` | ref discovery, with the query parameter `service` |
| `POST /git/{org}/{slug}.git/git-upload-pack` | the fetch exchange |
| `POST /git/{org}/{slug}.git/git-receive-pack` | the push exchange |

Any other path or method under `/git` gets the general `404 not_found` or
`405 method_not_allowed` envelope of [Routes and matching](#routes-and-matching),
before any credential is read; that includes `HEAD` on ref discovery, which
gets `405` with `Allow: GET`, is not counted and never runs git. The git routes pass none of the REST API's
checks: no JSON body, no body limit, no content-type rule, no error envelope.
A git request passes these checks in this order, and the first that refuses
answers it with an HTTP status and a body of one line of text, sent with
`Content-Type: text/plain; charset=utf-8`:

1. Authentication. git sends the credential as HTTP Basic,
   `Authorization: Basic <base64 of user:password>`, with the scheme in any
   letter case. The password is the credential (an API key, a PAT or the
   admin token, checked as in [Authentication](#authentication)); the user
   name is never read, so any name, an empty one included, works. A missing
   header, another scheme (such as `Bearer`), data that is not base64 or has
   no colon, an empty password, and a credential that is unknown, wrong,
   revoked or expired all get the same answer, whatever the request
   addresses:

   ```
   HTTP/1.1 401 Unauthorized
   WWW-Authenticate: Basic realm="parley"
   Content-Type: text/plain; charset=utf-8

   authentication required
   ```

   The credential of a blocked user gets `403` with the body
   `user is blocked` and no `WWW-Authenticate` header, so git does not ask
   for another credential.
2. The rate limit, as in [Rate limit](#rate-limit): the request is counted
   against its credential, in the same window as the credential's REST
   requests. Over the limit it gets `429` with a `Retry-After` header and the
   body `rate limit exceeded`, before the repository is looked at, so the
   answer is the same for a repository that exists and one that does not. A
   request that authentication refused is counted against its client
   address and still answered by authentication (`401` or `403`).
3. The binding guard, for a PAT bound to a workspace (see
   [Workspace-bound tokens](#workspace-bound-tokens)): on a repository its
   binding does not cover, whether or not it exists, it gets `404` with the
   body `repository not found`, whatever scopes it holds, and the refusal
   is recorded as a `parley.token.bound_refused` event with `surface` `git`.
4. The scope of a PAT. A push (`POST …/git-receive-pack`, and ref discovery
   whose first `service` value is `git-receive-pack`) needs `git:write`;
   every other request needs `git:read`, which `git:write` also satisfies.
   A PAT without it gets `403` with the body `token lacks scope git:read` or
   `token lacks scope git:write`, before the repository is looked at, so the
   answer is the same for a repository that exists and one that does not. An
   API key and the admin token carry no scopes and skip this check; the
   `paths` of a PAT are never read on the fetch side (the push processor
   receives them).
5. The repository and the caller's access to it, by the
   [workspace access rule](#workspace-access): a push needs write access,
   every other request read access. The repository segment must end in
   `.git`, the slug before it must name a workspace the caller can read,
   the workspace's organization must exist, and the org segment must be
   that organization's slug, letter case included. Every request that fails
   one of these gets the same answer, byte for byte but for `Date` and the
   request id, in every status of the workspace:

   ```
   HTTP/1.1 404 Not Found
   Content-Type: text/plain; charset=utf-8

   repository not found
   ```

   An org member who is not the owner gets, on a push to a repository at
   its right URL, in every status of the workspace, `403` with the body
   `org members have read-only access to this workspace; create a task fork to contribute`.
   Memberships and bindings are read on every request, so a removed member
   is refused from the next request on.
6. The request. On ref discovery the first value of the query parameter
   `service` must be `git-upload-pack` or `git-receive-pack`; later values
   are ignored. Otherwise the answer is `400` with the body
   `invalid or missing service parameter`.
7. The workspace's state. A workspace that a reclone, archive or delete is
   working on gets `409` with the body
   `repository not available: workspace is busy`, whatever its status;
   otherwise a workspace whose status is not `ready` gets `409` with the
   body `repository not available: workspace is <status>` (`pending`,
   `cloning`, `failed` or `archived`). A ready task fork is then
   judged by its parent: while the parent is pending, cloning, failed or
   being recloned, archived or deleted, every request gets `409` with the body
   `repository not available: parent workspace is busy`; while the parent is
   archived, a push (`POST …/git-receive-pack` and ref discovery for
   `git-receive-pack`) gets `409` with the body
   `repository is read-only: parent workspace is archived`, and a fetch is
   served.

An error the hub did not expect, such as a failed read of the store or a
workspace whose repository is missing from disk, is answered `500` with the
body `internal error`, nothing of the cause in it, and written to the hub's
log with the request id, the route's pattern and the repository segment.

A fetch that passed every check is served by `git upload-pack` on the
workspace's repository, as stock git expects it; a request body of any
`Content-Type` is taken, with no limit of the REST API: a version 0 body of
any size is streamed to git, and a version 2 body, which the hub reads
whole before git runs, up to 16 MiB, far above any request stock git sends.
The `paths` of a PAT are not read.

- `GET …/info/refs?service=git-upload-pack` answers `200` with
  `Content-Type: application/x-git-upload-pack-advertisement` and
  `Cache-Control: no-cache`. When the `Git-Protocol` header has the entry
  `version=2` (entries are separated by `:`), the body is git's protocol
  version 2 capability advertisement (first pkt-line `version 2`, then
  capabilities such as `ls-refs` and `fetch`, no refs). Otherwise, with no
  header or any other value, it is protocol version 0: the pkt-line
  `# service=git-upload-pack`, a flush packet `0000`, then one pkt-line per
  ref, the first carrying git's capability list.
- `POST …/git-upload-pack` answers `200` with
  `Content-Type: application/x-git-upload-pack-result` and
  `Cache-Control: no-cache`, and its body is git's answer, sent as git
  writes it. A body sent with `Content-Encoding: gzip` is decompressed
  first.

What a fetch can ask for is what stock git offers, the same under both
protocol versions:

- Every ref of the repository is advertised: branches, tags, notes and the
  hub-owned refs under `refs/parley/`, with `HEAD` as a symbolic ref to the
  default branch (`symref=HEAD:refs/heads/<branch>` under version 0, the
  `symref-target` attribute of `ls-refs` under version 2); a task fork's
  `HEAD` names its base branch, the fork's `default_branch`. A version 2
  `ls-refs` with `ref-prefix` arguments lists only the refs that match one
  of them. A default `git clone` takes branches and tags only; a ref under
  `refs/parley/` is fetched by its name.
- Shallow history: `--depth`, `--deepen`, `--shallow-since`,
  `--shallow-exclude` and `--unshallow`.
- Incremental fetch: haves and wants are negotiated and only the objects
  the client lacks are sent. git's progress lines reach the client while
  the fetch runs (`remote: Enumerating objects …`).
- Partial clone with the filters `blob:none`, `blob:limit=<n>` and
  `tree:<depth>`, and the fetch by id of an object the filter left out
  (git does that on its own, at checkout). Every other filter kind
  (`object:type`, `sparse:oid`, `combine`, …) is refused.
- An object by its id, a commit, a tree, a blob or an annotated tag, when a
  ref reaches it. An object no ref reaches is refused under both versions,
  however the want writes its id (git reads the leading 40 or 64
  hexadecimal digits, in either case, and ignores what follows); for a task
  fork, that is a ref of the fork (see
  [errata/17_git_smart_http.md](errata/17_git_smart_http.md)).

Once the answer has begun, an error is reported in band, as one pkt-line
starting with `ERR `, and no pack follows it:

| When | Pkt-line |
|---|---|
| the request body is empty | `ERR empty request` |
| `Content-Encoding` is `gzip` but the body is not valid gzip | `ERR invalid gzip body` |
| git refuses the request | `ERR ` and git's own text, the last line of its error output without `fatal: `; `ERR upload-pack failed` when git wrote none or that line names a place on the hub's disk |
| a filter other than `blob:none`, `blob:limit` and `tree` | git's `ERR filter '<kind>' not supported` |
| a want of an object no ref reaches | `ERR upload-pack: not our ref <id>` |
| a version 2 want whose id git cannot read (fewer than 40 hexadecimal digits) | `ERR upload-pack: protocol error, expected to get oid` |
| a version 2 request body, or the wants of a version 0 one, larger than 16 MiB (after gzip decompression) | `ERR request too large` |
| the request, a task fork's ref copy and the run of git together, takes longer than `[git].timeout` (see [configuration.md](configuration.md)) | `ERR request timed out` |

On a timeout git is killed and the answer ends; the `ERR` pkt-line is
written only when no pack data had been sent yet, and the client's git
fails.

A push that passed every check gets its ref advertisement from
`git receive-pack` on the workspace's repository:

- `GET …/info/refs?service=git-receive-pack` answers `200` with
  `Content-Type: application/x-git-receive-pack-advertisement` and
  `Cache-Control: no-cache`, always under protocol version 0, whatever the
  `Git-Protocol` header says: the pkt-line `# service=git-receive-pack`, a
  flush packet `0000`, then one pkt-line per ref, every ref of the
  repository including those under `refs/parley/`, the first carrying the
  capability list, which holds `report-status`, `delete-refs`,
  `side-band-64k`, `ofs-delta`, `atomic` and `push-options`.
- `POST …/git-receive-pack` is handed, with its body unread and whatever
  its size, to the hub's push processor, which writes the whole answer;
  `git receive-pack` does not run. A push moves all of its refs or none:
  - The body (gzip-decoded when `Content-Encoding` says so) holds the
    commands `<old> <new> <ref>`, the client's capabilities after a NUL on
    the first, a flush packet, then the push options and a flush packet
    when the client asked for `push-options`, then the pack, which a push
    of deletes only leaves out. A body whose command list cannot be read is
    answered `400` with `invalid receive-pack request`; a body holding only
    a flush packet is answered `200` with an empty body and changes
    nothing.
  - The pack is held apart from the repository until the push is accepted.
    A pack git cannot index, or a new value that names no object, is
    reported `unpack invalid pack` with `ng <ref> unpacker error` for every
    ref. A pack larger than `[git].max_push_size` (no limit by default; see
    [configuration.md](configuration.md)) is read no further and reported
    `unpack pack exceeds the maximum push size` with
    `ng <ref> unpacker error` for every ref; a pack of exactly the limit is
    accepted.
  - A push is bounded by `[git].timeout` from its arrival up to its ref
    transaction: when it runs out before, the push's `git` processes are
    killed, nothing changes, and every ref is reported
    `unpack push timed out` with `ng <ref> unpacker error`; once the ref
    transaction has started, the push is finished.
  - Every ref update is decided by the push rules, in the order hub-owned
    refs (`refs/parley/ is maintained by the hub and cannot be pushed to`),
    protected branches (`<branch> is protected and cannot be force-pushed
    or deleted`, `<branch> is protected; push another branch and open a
    change (git push -o parley.change)`; while `protect.owner_push` is
    true, the workspace's owner or an admin may fast-forward one with an
    API key, the admin token, or a PAT neither bound to a workspace nor
    limited to paths), the default branch (`<branch> is the default branch
    and cannot be deleted`), a fork's tracking branches (`<branch> tracks
    the parent workspace and is read-only in this fork; push a new
    branch`), the carry-patch controls and the paths of a path-scoped PAT
    (`path <path> is outside this token's allowed paths`: every path a new
    commit changes, every path of a tree a ref is pointed at, and, for a
    blob a ref is pointed at, which has no path, the ref's name); one that
    cannot be decided is refused with `internal error; try again`. The
    refs then move together by compare-and-swap against the old values
    sent: a ref that moved meanwhile is refused with `ref changed since it
    was read; fetch and try again`, so of two pushes of one ref from one
    value exactly one goes through. In the same store transaction, before
    the refs move, a pushed branch that is the source of a draft or open
    change records the change's next version, of kind `push` (see
    [Changes](#changes)), and a deleted one abandons its change; when that
    fails, such as for a session the hub refuses, every ref is refused with
    `internal error; try again` and nothing moves.
  - The answer is `200` with
    `Content-Type: application/x-git-receive-pack-result` and
    `Cache-Control: no-cache`. With `report-status` (or
    `report-status-v2`) it holds the status report: `unpack ok`, then per
    command, in order, `ok <ref>` for an accepted push, or for a refused
    one `ng <ref> <message>` for each refused ref and
    `ng <ref> not updated: another ref in this push was refused` for the
    others. With `side-band-64k` the report comes inside band 1, and band 2
    carries before it an `error: <ref>: <message>` line per refused ref,
    which git shows as `remote: error: …`. Without `report-status` the
    answer has no report, and without `side-band-64k` no `error:` line.
  - A refused push leaves every ref as it was and none of its objects in
    the repository, also one refused after its objects were moved in
    (a failing stage or store), which has them taken out again.
  - An accepted push writes one `parley.git.push` event per ref update, in
    command order, all in one transaction, so the events of one push are
    adjacent in the event log, before the answer is sent. A refused push,
    one whose pack fails and one without commands write none. The event
    names the addressed workspace (a task fork's own slug for a fork) as
    `workspace`, `resource_type` `ref` and the full ref name as
    `resource_id`, the caller as for the fetch event below (`actor_type`
    `pat`, `api_key` or `admin_token`, the user as `actor_id`, `null` for
    the admin token), and the session the push belongs to as `session_id`,
    `null` for none. Its metadata holds `ref`, `before` (`null` for a
    create), `after` (`null` for a delete), `created`, `deleted`, `forced`
    (neither a create nor a delete, and `before` not an ancestor of
    `after`), `direct` (a fast-forward of a protected branch by its owner
    or the admin token) and `via`, `git` for a push:

    ```json
    {"seq":15,"id":"…","type":"parley.git.push","time":"2026-11-14T10:33:00Z","workspace":"demo","actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","resource_type":"ref","resource_id":"refs/heads/main","action":"push","session_id":null,"metadata":{"ref":"refs/heads/main","before":"3ba7119670b709c3ed9a0fafc7c8d0d3124b74e1","after":"e59b116d14077f2bdc935c1adee921a5b9f2ec67","created":false,"deleted":false,"forced":false,"direct":true,"via":"git"}}
    ```

    A push stays accepted when its events cannot be written: they are
    written when the hub next starts, before it serves, as are those of a
    push whose refs moved just before the hub stopped.

A task fork is served with its own refs and what it shows of its parent's,
on every git route, as of the moment of the request:

- Each tracking branch and tracking tag of the fork (the branches and tags
  the parent had when the fork was created, see
  [`POST /api/v1/workspaces/{slug}/forks`](#post-apiv1workspacesslugforks))
  is advertised under its own name at the value the parent's ref of that
  name has now, and not at all once the parent no longer has it. A branch
  or tag the parent gained after the fork is never advertised.
- The version refs `refs/parley/changes/<number>/<version>` of the changes
  whose source is the fork are advertised under the parent's names, and
  no version ref of another change, whatever state the change is in.
- The fork's own branches are advertised as they are.

When those refs cannot be brought up to date, such as when the parent's
repository is missing from disk, the request is answered `500` with the
body `internal error` before git runs, so a fork is never served with refs
that may be out of date.

Every request on the three routes is counted once, when its answer is
complete, in the counter `parley_git_requests_total` with the labels
`service` and `result`. `service` is the route: `info-refs` for ref
discovery, whichever service it asks for, `upload-pack` or `receive-pack`.
`result` is `refused` for an answer of `401`, `403` or `404`; `error` for
every other refusal (`400`, `409`, `429`, `500`) and for an exchange that
ended with one of the hub's `ERR` pkt-lines, a failed git or a timeout;
`ok` for a served exchange; and, for a push handed to the push processor,
the result the processor reports: `ok` for an accepted push and for one
without commands, `refused` for a push refused by a rule, by the options
stage or because a ref moved meanwhile, `error` for a pack that cannot be
read or is over `[git].max_push_size`, a push that timed out, one refused
with `internal error; try again` and a command list that cannot be read
(`400`). The hub does not export the counter at
`/metrics` yet.

When the workspace's setting `audit_fetches` is `true` (for a task fork,
its parent's: a fork has no settings of its own), each
`POST …/git-upload-pack` that is served and sends a pack (a `packfile`
section under version 2, pack data under version 0) writes one
`parley.git.fetch` event, once the pack is sent and before the answer ends.
Ref discovery, an `ls-refs` request, a fetch with nothing to send, a refused
request and one answered with an `ERR` pkt-line write none. The event names
the addressed workspace (a task fork's own slug for a fork) as `workspace`
and `resource_id`, `resource_type` `workspace`, and the caller: `actor_type`
`pat`, `api_key` or `admin_token`, with the user as `actor_id` for a PAT or
an API key. Its metadata holds `shallow`, `true` when the request has a
`deepen`, `deepen-since` or `deepen-not` line (`--depth`, `--deepen`,
`--shallow-since`, `--shallow-exclude`, `--unshallow`), and `filter`, the
text of the request's filter line or `null`. git sends a size limit in bytes,
so `--filter=blob:limit=1k` is recorded as `blob:limit=1024`:

```json
{"seq":14,"id":"…","type":"parley.git.fetch","time":"2026-11-14T10:32:00Z","workspace":"demo","actor_type":"pat","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","resource_type":"workspace","resource_id":"demo","action":"fetch","session_id":null,"metadata":{"filter":"blob:none","shallow":true}}
```

The fetch is served whatever becomes of its event: an event that cannot be
written is logged with `"event":"parley.git.fetch"`, and the fetch and its
count are unchanged.

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

For a task fork, `head_sha` is computed at every read: the parent's current
head of the fork's base branch (its `default_branch`), read from the
parent's repository, or `null` while the parent has no branch of that name;
the answer is still `200`. `fork_point.sha` stays the head of that branch
when the fork was created.

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
settings of the workspace `{slug}`, and the lifetime, the purpose and
`auto_change` of a task fork. It takes the admin token, an API key or a PAT, declares
the scope `workspaces:write` for a PAT and needs write access to the
workspace (see [Workspace access](#workspace-access)): its owner or the
admin token, and for a task fork also the owner of its parent. It works in
every status but `archived`, and on a task fork only while its parent is
`ready` with no maintenance running.

The body is a JSON object that changes exactly the fields it carries;
members the route does not know are ignored, and a field not given keeps
its value.

| Field | Repository workspace | Task fork |
|---|---|---|
| `display_name` | `null` or a string of at most 128 characters (Unicode code points); `null` or `""` sets it to the slug | the same |
| `description` | `null` or a string of at most 1024 characters; `null` sets it to `""` | the same |
| `org_id` | an org id or slug, or `null` for the personal org of the workspace's owner; the workspace and every fork of it move to that org | `400` `not_supported_on_fork` |
| `settings` | an object or `null`, applied to the stored settings key by key (below) | `400` `not_supported_on_fork` |
| `ttl` | `400` `invalid_request` | a string, a whole number above zero followed by `m`, `h` or `d`, from `5m` to `[forks].max_ttl`, both included (see [the configuration](configuration.md)); `expires_at` becomes the time of the request plus `ttl`, cut to the second, later or earlier than before |
| `purpose` | `400` `invalid_request` | `null` or a string of at most 1024 characters (Unicode code points); `null` clears it |
| `auto_change` | `400` `invalid_request` | `true` or `false`; `null` is refused |
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
`settings`, `ttl`, `purpose`, `auto_change`; an `org_id` naming an org of which
the workspace's owner is not an active member (`403`); and last the state:
an archived workspace is `409`, and for a task fork, whatever the fields,
its parent's state, read in the update's transaction: a parent that is
`pending` or `cloning` or on which a maintenance operation (archive,
reclone, delete) runs is `409` `workspace_busy`, a `failed` parent `409`
`workspace_not_ready`, an `archived` parent `409` `parent_archived`. An
invalid body is therefore `400` also while the parent is archived. A fork
whose parent is archived stays readable:
[`GET /api/v1/workspaces/{slug}`](#get-apiv1workspacesslug) answers it.
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
`parent_slug`, its parent's slug; its `session_id` is the workspace's
`session_id`, that of a fork created in a session and otherwise `null`.
The metadata holds no value of any field, so no setting appears in the
log:

```json
{"seq":13,"id":"…","type":"parley.workspace.update","time":"2026-11-14T10:31:02Z","workspace":"demo","actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","resource_type":"workspace","resource_id":"demo","action":"update","session_id":null,"metadata":{"fields":["display_name","settings"]}}
```

The answer is the workspace object after the update, the object
[`GET /api/v1/workspaces/{slug}`](#get-apiv1workspacesslug) answers, with
`access` the caller's level.

| Status | When |
|---|---|
| `200 OK` | the fields are changed; the body is the workspace object |
| `400` `invalid_request` | the body is not a JSON object; it carries an immutable field (`<field>: cannot be changed`), a fork-only field on a repository workspace (`<field>: applies only to a task fork`) or on a fork that does not accept it (`<field>: cannot be changed`); it carries no field to change (`the body carries no field to change`); or a value is refused: `display_name: must be a string or null`, `display_name: must be at most 128 characters`, the same for `description` with 1024, `org_id: must be a string or null`, `org_id: no organization has the id or slug "<value>"` (also for `""` and an id no org has), `org_id: the workspace's owner has no personal organization`, `settings: must be an object or null`, `unknown setting "<key>"`, or `settings.<key>: <reason>` for a value its rule refuses (`settings.land.strategy: must be one of ["rebase" "squash"]`), `ttl: must be a string` (also for `null`), `ttl: must be a whole number followed by m, h or d`, `ttl: must end in one unit: m, h or d`, `ttl: must be a whole number in decimal digits followed by m, h or d`, `ttl: must be above zero`, `ttl: is too long a duration`, `ttl: must be at least 5m`, `ttl: must be at most <max_ttl>, the hub's [forks].max_ttl` (`30d` by default), `purpose: must be a string`, `purpose: must be at most 1024 characters long`, `auto_change: must be true or false`; the reason never repeats a URL |
| `400` `not_supported_on_fork` | `org_id` or `settings` in an update of a task fork, whatever the value (`<field>: a task fork takes its org and its settings from its parent`) |
| `403` `insufficient_scope` | a PAT without `workspaces:write` (`this route needs the scope workspaces:write`) |
| `403` `forbidden` | a member of the workspace's org who does not own it (`org members have read-only access to this workspace`), in every status; or an `org_id` naming an org of which the workspace's owner is not an active member, whoever the caller is (`the workspace's owner is not an active member of the organization`) |
| `404` `not_found` | the caller cannot read the workspace, or no workspace has the slug (`workspace not found`); also a bound PAT on a workspace outside its binding |
| `409` `workspace_archived` | a valid update of a workspace in status `archived` (`workspace <slug> is archived`) |
| `409` `parent_archived` | a valid update of a task fork whose parent is `archived` (`workspace "<fork slug>" is read-only: its parent workspace "<parent slug>" is archived`) |
| `409` `workspace_busy` | a valid update of a task fork whose parent is `pending` or `cloning` (`workspace "<parent slug>" is busy: its clone has not finished; try again later`) or runs a maintenance operation (`workspace "<parent slug>" is busy: a <archive, reclone or delete> is running; try again later`) |
| `409` `workspace_not_ready` | a valid update of a task fork whose parent is `failed` (`workspace "<parent slug>" is not ready: its clone failed; reclone it`) |
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
is handed to the task-fork feature, which removes forks by its own rules,
described under [Deleting a task fork](#deleting-a-task-fork) below. None
of what follows up to there applies to a fork.

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
and the path, a fork's also kept for a later removal, and the answer stays
`204`. Every event written about the workspace and its forks before the
delete stays in the event log. The slug
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
| `500` `internal` | the sandboxes could not be deleted, or a step of the removal failed: the workspace is still `archived` and nothing has changed |

### Deleting a task fork

A task fork is deleted at once: it needs no archive first, and its parent
may be `ready`, `archived` or `failed`. The fork's creator, the owner of
its parent and the admin token have write access to it. First the fork's
sandboxes are deleted, without a checkpoint; when that fails the answer is
`500 internal` and nothing else has changed. Then, in one store
transaction, the tokens bound to the fork are revoked, the data other
features keep for it (its secrets among them) is removed, its remaining
agent sessions are ended with the reason `workspace deleted`, its record
is removed and one `parley.workspace.delete` event is written. When
anything in that transaction fails, nothing has changed: the fork still
answers `GET` with its `expires_at`, its tokens still authenticate, its
secrets and its own branches are still there. Once it has committed the
fork's repository, with its own branches, is removed from disk; the
parent's branches are never touched. A repository that cannot be removed
is logged and kept for a later removal, and the answer stays `204`. A fork
created later under the same name starts without the removed fork's
branches. Of two deletes of one fork at the same time one answers `204`
and the other `404`, and one event is written.

The event is the one described above with `metadata.kind` `fork`, the
caller as actor, `metadata.parent_slug` the parent's slug, no
`deleted_forks`, and `session_id` the session the fork was created in,
`null` for none; no `parley.fork.expire` event is written.

| Status | When |
|---|---|
| `204 No Content` | the fork and what it owns are gone; the body is empty |
| `403` `insufficient_scope` | a PAT without `workspaces:write` |
| `403` `forbidden` | a member of the parent's org who neither created the fork nor owns the parent |
| `404` `not_found` | the caller cannot read the fork, or no workspace has the slug, also a fork already deleted (`workspace not found`) |
| `409` `workspace_busy` | the parent is `pending` or `cloning` (`workspace "<parent>" is busy: its clone has not finished; try again later`) or a reclone, archive or delete runs on it (`workspace "<parent>" is busy: a <operation> is running; try again later`): the fork is unchanged |
| `500` `internal` | the sandboxes could not be deleted, or a step of the removal failed (`internal error`): the fork is unchanged |

### Expiry of a task fork

A fork whose `expires_at` has passed stays as it was, served by `GET` and
open to a new lifetime with `ttl` in
[`PATCH /api/v1/workspaces/{slug}`](#patch-apiv1workspacesslug), until a
pass of the hub's sweep removes it. The sweep runs one pass right after the
hub starts, one every 60 seconds and one in each hourly maintenance pass
(the task `fork_expiry`), never two at once. A pass removes every fork
whose `expires_at` is at or before its time, the forks of an `archived`
parent included, as a delete does: its sandboxes first, then in one store
transaction its bound tokens, the data other features keep for it, its
sessions (ended with the reason `fork expired`) and its record, then its
repository. A fork whose parent is `pending` or `cloning` or under a
reclone, archive or delete is left for a later pass, and so is a fork whose
removal fails, whole; the failure is logged with the fork's slug. A
repository that could not be removed after a delete or an expiry is
removed by a later pass, unless a fork has been created under the same
slug again.

While [`[forks].expire_on_land`](configuration.md) is `true` (the
default), the same pass then removes early, the same way, every other fork
that is the source of at least one change, whose changes are all `landed`
or `abandoned`, whose every own branch is the source branch of one of
them, whose last activity is at least 24 hours old and which has no
sandbox `provisioning` or `ready`. A fork's last activity is its creation
until a push to it or a change of it being opened, versioned, landed,
abandoned or restored is recorded. A fork past its `expires_at` is
removed once, with the reason `ttl`. A fork whose changes or sandboxes
cannot be read is kept, the failure logged with its slug, until a later
pass.

Each removal writes one `parley.fork.expire` event and no
`parley.workspace.delete` event: actor `system`, `workspace` and
`resource_id` the fork's slug, `resource_type` `workspace`, `session_id`
the session the fork was created in (`null` for none), and `metadata`
with `parent_slug`, `reason` (`ttl` when its `expires_at` had passed,
`landed` for an early removal), `expires_at` the fork's `expires_at`
when it was removed, and `revoked_tokens`, the ids of the tokens bound to
the fork that were revoked, in ascending order. The event is visible to
the readers of the parent's events.

```json
{"seq":21,"id":"…","type":"parley.fork.expire","time":"2026-11-21T11:00:30Z","workspace":"demo--t1","actor_type":"system","actor_id":null,"resource_type":"workspace","resource_id":"demo--t1","action":"expire","session_id":null,"metadata":{"parent_slug":"demo","reason":"ttl","expires_at":"2026-11-21T11:00:00Z","revoked_tokens":["tok1"]}}
```

## `POST /api/v1/workspaces/{slug}/forks`

Creates a task fork of the repository workspace `{slug}`, its parent: a
workspace that is `ready` at once, shares the parent's history without
copying it, and is owned by the caller's user. It takes an API key or a PAT
and declares the scope `workspaces:write` for a PAT and the level `read` on
the parent (see [Workspace access](#workspace-access)), so an org member who
can only read the parent can fork it, and a PAT bound to the parent can too.
It is a create route (see [Idempotency](#idempotency)); a replay leaves out
`token.token`.

The body is a JSON object, or no body at all, which is `{}`; members the
route does not read are ignored, and a member that is `null` counts as
absent, so a client may send back what it read:

| Member | Value |
|---|---|
| `name` | a string of 1 to 32 characters of `a` to `z`, digits and hyphens, starting and ending with a letter or digit, without two hyphens in a row; the fork's slug is `<parent slug>--<name>`. Absent, a name of 8 characters from `a` to `z` and digits, drawn from a cryptographic random source |
| `ref` | a non-empty string, the short name of a branch of the parent, which becomes the fork's base branch and `default_branch`; absent, the parent's `default_branch` |
| `ttl` | a string, a whole number above zero followed by `m`, `h` or `d`, from `5m` to `[forks].max_ttl`; `expires_at` is the creation time plus `ttl`. Absent, plus `[forks].default_ttl` (see [the configuration](configuration.md)) |
| `auto_change` | `true` or `false`; absent, `true` |
| `token` | an object `{name, permissions, paths, expires}` of the token to mint with the fork, bound to it and owned by the caller: `name` a string, absent `fork:<fork slug>`; `permissions` and `paths` lists of strings, judged by the minting component as for [`POST /api/v1/workspaces/{slug}/tokens`](#post-apiv1workspacesslugtokens); `expires` a string, a whole number above zero followed by `m`, `h` or `d`, and the token's `expires_at` is the fork's creation time plus `expires`. Absent `expires`, the token never expires (`expires_at` `null`). A PAT caller must hold `tokens:manage` and is held to the subset rule |
| `purpose` | a string of at most 1024 characters (Unicode code points); absent, `null` |
| `session_id` | a UUID in its hyphenated form, the session the fork is created in, judged by the session-linking rule for the caller and the parent; the fork stores it, and its event carries it, as the rule gives it back, in lower case, and the link of the fork to the session is recorded in the fork's transaction. No session component is part of the hub yet, so no session exists and every `session_id` is refused with `400` `unknown_session` |
| `sandbox` | an object, the sandbox to create with the fork, handed as received, unread, to the sandbox component with the fork's `session_id` and `purpose` as defaults; the component judges it and answers the sandbox object or its refusal, on which no fork is created. No sandbox component is part of the hub yet, so every `sandbox` is refused with `404` `sandboxes_disabled` and no fork is created |

```json
{"name":"task1","ref":"release","ttl":"24h","purpose":"fix loader"}
```

The checks run in this order, the first that fails answering: a PAT without
`workspaces:write` (`403`); the access rule for need `read` (`404` for a
workspace the caller cannot read and for an unknown slug, with the same
bytes); the admin token (`403`), before the body is read; the body, member
by member in the order of the table (`400`); the session (`400`); a parent
that is itself a task fork (`400`); the parent's state (`409`); the base
branch (`400`); the fork limit (`429`); a name a workspace already has
(`409`); the token (`400`, `403`); the sandbox (`404`, or the sandbox
component's refusal). A refused request
records nothing and leaves no repository directory behind.

The fork's repository is an empty bare repository whose alternates file
names the parent's object directory: no object is copied, and every commit
of the parent, also one it receives later, is readable through it. Its
place is the one every workspace copy has, `<[workspace].path>/<slug>.git`;
a directory found there while no workspace has the slug is the leftover of
a create that stopped, and is removed first. The fork is recorded with
`kind` `fork`, `workspace_mode` `standard`, `status` `ready`, the caller as
`owner_id`, the parent's `org_id`, `parent_slug` the parent, `display_name`
the slug, `description` empty, `git_url`, `upstream_url` and
`integration_branch` `null`, `fork_point` the base branch and the parent's
head of it, the names of the parent's branches and tags as they are now as
its tracking names (see [`GET /api/v1/workspaces/{slug}`](#get-apiv1workspacesslug)),
`expires_at`, `auto_change` and `purpose`, and, when a PAT authenticated
the request, that PAT as its creating token, which lets a PAT bound to the
parent reach the forks it created (see
[Workspace-bound tokens](#workspace-bound-tokens)). The parent's state is
judged again in the transaction, the fork, the link of the fork to its
session, its token and a `parley.workspace.fork` event are written in that
one transaction, the sandbox component is asked last in it, and
creates of forks of one parent run one at a time from the name check to the
commit, so two requests for one name answer `201` and `409`. When the
repository cannot be made, or the transaction or its commit fails, the
repository directory is removed, nothing is recorded, and the answer is
`500` `internal` without its cause.

The answer has `workspace`, the fork's workspace object with `access`
`owner` and `head_sha` the parent's head of the base branch; `remote`, the
fork's `hub_url`, `null` when no external URL is configured; and, only when
the body asked for one, `token`, the Token object with `workspace` the
fork's slug and the secret in `token.token`, which no other answer carries;
and, only when the body had a `sandbox`, `sandbox`, the sandbox component's
object as it gave it:

```json
{"workspace":{"slug":"demo--task1","kind":"fork","workspace_mode":"standard","git_url":null,"default_branch":"release","owner_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","org_id":"5e6f7a8b-9c0d-4e1f-8a2b-3c4d5e6f7a8b","org_slug":"acme","status":"ready","display_name":"demo--task1","description":"","head_sha":"9fceb02d0ae598e95dc970b74767f19372d61af8","origin_head_sha":null,"clone_error":null,"last_sync_at":null,"sync_error":null,"upstream_url":null,"integration_branch":null,"upstream_head_sha":null,"parent_slug":"demo","fork_point":{"ref":"release","sha":"9fceb02d0ae598e95dc970b74767f19372d61af8"},"expires_at":"2026-11-15T10:30:15Z","auto_change":true,"purpose":"fix loader","session_id":null,"settings":{"sync_enabled":true,"…":"…"},"hub_url":"https://hub.example/git/acme/demo--task1.git","access":"owner","created_at":"2026-11-14T10:30:15Z","updated_at":"2026-11-14T10:30:15Z"},"remote":"https://hub.example/git/acme/demo--task1.git"}
```

| Status | When |
|---|---|
| `201 Created` | the fork was created; the body is `{workspace, remote}`, and `token` and `sandbox` when asked for |
| `200 OK` | a replay of a recorded `Idempotency-Key` with the same body: the first answer without `token.token`; nothing is created |
| `400` `invalid_request` | the body is not an object (the messages of [Request bodies](#request-bodies)), or a member is refused, the message naming it: `name: must be a string`, `name: must be 1 to 32 characters long`, `name: may hold only the letters a to z, digits and hyphens`, `name: must start with a letter or digit`, `name: must end with a letter or digit`, `name: must not contain two hyphens in a row`; `ref: must be a string`, `ref: must not be empty`; `ttl: must be a string`, `ttl: must end in one unit: m, h or d` and the other duration rules, `ttl: must be at least 5m`, `ttl: must be at most 30d, the hub's [forks].max_ttl`; `auto_change: must be true or false`; `token: must be an object whose name and expires are strings and whose permissions and paths are lists of strings`, `token: expires: must end in one unit: m, h or d` and the other duration rules (`token: expires: must be above zero` for `0h`); `purpose: must be a string`, `purpose: must be at most 1024 characters long`; `session_id: must be a string`, `session_id: must be a UUID`; `sandbox: must be an object`; or the minting component refuses the token, with the messages of `POST /api/v1/user/tokens` |
| `400` `unknown_session` | the body has a `session_id` the session-linking rule refuses, or the hub has no session component (`session_id: no session "<id>"`) |
| `400` `not_supported_on_fork` | the workspace is itself a task fork (`workspace "demo--task1" is a task fork, which cannot be forked; fork its parent instead`) |
| `400` `fork_ref_not_found` | the base branch is not a branch of the parent: a tag, a commit id or a full ref name is not one (`ref: "nope" is not a branch of "demo"`) |
| `403` `insufficient_scope` | a PAT without `workspaces:write`; a PAT without `tokens:manage` whose body has `token` (`token: minting a token needs the scope tokens:manage`); or a token beyond what the calling PAT holds, with the subset rule's messages |
| `403` `forbidden` | the admin token, whatever the body (`a fork needs a user as owner; the admin token cannot create one`) |
| `404` `not_found` | the caller cannot read the workspace, or no workspace has the slug (`workspace not found`) |
| `404` `sandboxes_disabled` | the body has a `sandbox`: no sandbox component is part of the hub (`sandboxes are not enabled on this hub`); no fork is created |
| the sandbox component's | the body has a `sandbox` the sandbox component refuses: its status, `error_type`, message and extra members unchanged (such as `429` `sandbox_capacity` with `limit`, `maximum` and `current`); no fork is created, also when a token was asked for |
| `409` `workspace_busy` | the parent's clone has not finished, or a reclone, archive or delete runs on it |
| `409` `workspace_archived` | the parent is archived |
| `409` `workspace_not_ready` | the parent's clone failed |
| `409` `conflict` | a workspace has the slug the given `name` makes (`name: "demo" has a fork "demo--task1"`) |
| `429` `fork_limit_reached` | the parent has `[forks].max_per_workspace` forks (see [the configuration](configuration.md)), counting only forks that exist; it is counted before the repository is made and again in the transaction, so concurrent creates never pass the limit (`workspace "demo" has 200 forks, the hub's [forks].max_per_workspace`) |
| `500` `internal` | 8 generated names in a row were in use, the session-linking rule could not judge the `session_id`, or making the repository, writing the fork, its session link, its token or its event, or the commit failed; nothing is recorded |

The event has `workspace` and `resource_id` the fork's slug,
`resource_type` `workspace`, `actor_type` `api_key` or `pat` after the
calling credential, `actor_id` the caller's user, `session_id` the fork's,
and in `metadata` the fork's `parent_slug`, `fork_point`, `expires_at`,
`auto_change` and `purpose` (`null` when it has none) and, when a token was
minted, its `token_id`, never its value. The parent's readers see it (see
[`GET /api/v1/events`](#get-apiv1events)).

```json
{"seq":31,"id":"…","type":"parley.workspace.fork","time":"2026-11-14T10:30:15Z","workspace":"demo--task1","actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","resource_type":"workspace","resource_id":"demo--task1","action":"fork","session_id":null,"metadata":{"auto_change":true,"expires_at":"2026-11-15T10:30:15Z","fork_point":{"ref":"release","sha":"9fceb02d0ae598e95dc970b74767f19372d61af8"},"parent_slug":"demo","purpose":"fix loader"}}
```

## `GET /api/v1/workspaces/{slug}/forks`

Lists the task forks of the workspace `{slug}` to a caller who can read it,
in every status of the workspace. It takes the admin token, an API key or a
PAT, and declares the scope `workspaces:read` for a PAT, which a PAT holding
`workspaces:write` also satisfies. It needs read access to the workspace
(see [Workspace access](#workspace-access)).

The list holds the forks of `{slug}` the caller can read, each as its
workspace object with the caller's `access` on it: `owner` for the fork's
owner and the parent's owner, `member` for the other members of the org,
`admin` for the admin token. For a PAT bound to a workspace it holds only
the forks its binding covers (see
[Workspace-bound tokens](#workspace-bound-tokens)): a PAT bound to the
parent lists the forks it created, a PAT bound to a fork lists nothing. A
task fork, and a workspace without forks, list nothing
(`{"items":[],"next_cursor":null}`).

It is a list (see [Lists](#lists)), with `limit` and `cursor` and these
query parameters; with both, a fork is listed when it satisfies both:

| Parameter | Meaning |
|---|---|
| `owner` | `me` keeps the forks whose owner is the caller's user; the admin token, which acts for no user, lists nothing |
| `session_id` | a UUID, in any case: keeps the forks created in that session |

The order is `created_at` descending, then `slug` ascending among the forks
created in the same instant; the cursor carries both, so a walk over the
pages returns each listed fork once.

| Status | When |
|---|---|
| `200 OK` | the body is a page, `items` an array of workspace objects (`[]` when nothing matches) |
| `400` `invalid_request` | `owner` other than exactly `me`, `session_id` that is not a UUID, an empty value included, or either given more than once (`owner must be given once, as me`, `session_id must be given once, as a UUID`); a bad `limit` or `cursor` (see [Lists](#lists)) |
| `403` `insufficient_scope` | a PAT with neither `workspaces:read` nor `workspaces:write` (`this route needs the scope workspaces:read`) |
| `404` `not_found` | the caller cannot read the workspace, or no workspace has the slug (`workspace not found`) |

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

## Source control reads

The routes under `/api/v1/workspaces/{slug}/scm` read a workspace's
repository without a clone. They are `GET` routes; so far they are the two
refs routes, the two commits routes, the tree route, the file route, the
diff route, the compare route, the blame route and the search route below. Every one of them behaves
alike in what follows.

It takes the admin token, an API key or a PAT, and declares the scope
`git:read` for a PAT, which a PAT holding `git:write` also satisfies. It
needs read access to the workspace (see [Workspace access](#workspace-access)),
so an org member reads as the owner does. These checks run before the route
and touch no repository: no credential is `401` `unauthenticated`, a PAT
with neither scope `403` `insufficient_scope` (`this route needs the scope
git:read`), for an unknown slug as well, and a caller who cannot read the
workspace `404` `not_found` (`workspace not found`), with the same bytes as
for an unknown slug.

The route then checks its query parameters (`400` `invalid_request`), then
the state of the workspace, and only then opens the repository:

| State | Answer |
|---|---|
| `ready`; `archived` with no operation running, which reads as it did before the archive | served |
| a reclone, archive or delete running, on any status | `409` `workspace_busy` |
| `pending` or `cloning` | `409` `workspace_busy` |
| `failed` | `409` `workspace_not_ready` |
| a task fork whose parent is in one of the three states above | the parent's answer, `409` `workspace_busy` or `workspace_not_ready`; a fork whose parent is `archived` is served |

Every read is bounded by the hub's read timeout, `[scm].read_timeout`
(30 seconds by default, see [configuration.md](configuration.md)), counted
from the moment the route begins handling the request: when it runs out,
every `git` process of the read is killed with every process it started. A
read that has not begun its answer by then, also one whose git work is
done and whose answer is still being prepared, is answered `503`
`scm_timeout` (`the read did not finish within the read timeout of 30s`),
with none of the read's own headers; a read whose answer has begun is cut
off, the connection dropped before the body ends.

A task fork is read through its ref view, the one the git server
advertises: its own branches, with `tracking` `false`, and the branches and
tags it tracks at its parent's current heads, the branches with `tracking`
`true`, so a read of a fork follows its parent's branch as soon as it
moves. A fork shows no `refs/parley/` ref.

No read emits an event. Each request a route handles, refused by the route
itself or answered, is reported once to the hub's request recorder with the
route's op (`refs` for both refs routes, `commits` for both commits routes,
`tree` for the tree route, `file` for the file route, `diff` for the diff route, `compare` for the compare route, `blame` for the blame route, `search` for the search route) and `ok` for a status
below 400 (a `206` or a `304` included),
`error` otherwise, an answer cut off at the read timeout included; a request the checks before the route refuse is not
reported. The hub has no metric export yet, so the recorder discards what
it is told.

A ref object has exactly these members:

| Member | Value |
|---|---|
| `name` | the full ref name, such as `refs/heads/main` |
| `kind` | `branch` under `refs/heads/`, `tag` under `refs/tags/`, `hub` under `refs/parley/` |
| `sha` | the full id of the commit the ref points at; for an annotated tag, the commit the tag peels to |
| `protected` | `true` for a branch the workspace's effective `protected_branches` protect (a fork's are its parent's) and for every `refs/parley/` ref, else `false` |
| `tracking` | `true` for a task fork's tracking branch, else `false` |
| `change` | `{"workspace_slug": …, "number": …}` for the open or draft change whose source is the branch, named under the workspace it belongs to (a task fork's parent for a fork's branch); `null` when there is none |

Refs in any other namespace, such as `refs/notes/`, have no kind and are
never listed, nor is a ref that reaches no commit.

A revision parameter (`ref` of `…/scm/commits`, `…/scm/tree`,
`…/scm/file`, `…/scm/blame` and `…/scm/search`, `{sha}` of `…/scm/commits/{sha}`, `base` and `head` of
`…/scm/diff` and `…/scm/compare`) is a branch name, a tag name, a full ref name or a
full or abbreviated commit id (4 to 40 hexadecimal characters). A string
that begins with `refs/` is that full ref name only; otherwise the branch or
the tag of that name, where both existing is ambiguous; otherwise a commit
id, which names a commit whether or not a ref reaches it. An annotated tag
names the commit it peels to. A revision that names no one commit, an
expression such as `main~2`, `HEAD` or `@{1}` included, is `404`
`ref_not_found` (such as `revision "main~2" names no commit`, `revision
"dup" is both a branch and a tag`, `revision "4ad6" names more than one
commit`). A `ref` that is absent is the head of the workspace's default
branch. Each revision of a request is resolved once, against one reading of
the workspace's refs, and the whole answer is computed from the commit ids
it resolved to, so a read that runs while a ref moves answers for the old
commit or the new one, never a mix.

Every successful answer (`200` or `206`) carries an `ETag`, a quoted digest
of the request's path (the route, the workspace and any path parameter),
its query string (a list's `cursor` included, so each page has its own
tag), the full ids of the commits its revisions resolved to, and its
representation (raw or JSON on the file route, JSON or patch on the diff
route). The two refs routes resolve no revision; their tag is a digest of
the path, the query and the ref objects of the answer (names, ids and the
other members), so it changes when a listed ref moves, appears or goes.
Two identical requests get the same tag while no ref they name moves.
`Cache-Control` is then:

| Read | `Cache-Control` |
|---|---|
| a route with revision parameters, every one of them written as a full 40-character commit id (an absent `ref` is not); a later page of a list whose `ref` is one | `private, max-age=31536000, immutable` |
| any other: a revision written as a name or an abbreviated id, an absent `ref`, and both refs routes always | `private, no-cache` |

A request whose `If-None-Match` holds the tag the answer would carry, alone
or in a comma-separated list (a `W/` prefix is ignored), is answered `304
Not Modified` with no body and the same `ETag` and `Cache-Control` (and on
the file route `Vary`); `*` is not taken as a match. This is decided once the
revisions are resolved and before the rest of the read, so a current copy
costs no history walk, tree listing, file read, diff, trial rebase, blame
or search; a tag that does not match gets the full answer. An error answer
has no `ETag` and keeps `Cache-Control: no-store`.

A `path` parameter of a read that names a path of the repository is
relative, has no empty, `.` or `..` segment, has no segment `.git` and
holds no NUL byte; any other is `400` `invalid_request` (such as `path must
be a relative path of the repository, such as docs/api.md`, `path
"docs//api" has an empty, . or .. segment`, `path ".git/config" lies under
.git`), checked before the workspace's state and the repository.

A commit object, as every route that answers commits writes it:

| Member | Value |
|---|---|
| `sha` | the commit's full id |
| `parents` | the full ids of its parents, the first parent first; `[]` for a root commit |
| `tree_sha` | the full id of its tree |
| `author`, `committer` | `{"name": …, "email": …, "time": …}`, `time` an RFC 3339 timestamp in UTC, such as `2026-01-02T03:04:05Z` |
| `message` | the whole message, as written, its last newline included when it has one |
| `change` | `{"workspace_slug": …, "number": …, "version": …}` for the change whose version head the commit is or that landed it (a change that landed it first, then the lowest number); `version` is the highest version with the commit as its head or, for a landed commit, the version whose head it was landed from, `null` when there is none; on a task fork's slug, only the parent's changes whose source is the fork; `null` for a commit of no change |
| `session_id` | the agent session that introduced the commit; always `null` until the hub records sessions |
| `stats` | only where the route says: `{"files_changed": …, "insertions": …, "deletions": …}`, the commit against its first parent, or against the empty tree for a root commit; renamed and copied files are found, and a binary file counts as changed with no lines |

A file change, as every route that answers file changes writes it (the
commit route with `files=true`, the diff route, the compare route):

| Member | Value |
|---|---|
| `path` | the file's path, on the new side; for a deleted file, the path it had |
| `old_path` | the path on the old side of a renamed or copied file, else `null` |
| `status` | `added`, `modified`, `deleted`, `renamed`, `copied` or `type_changed` (a path whose kind changed, such as a regular file that became a symbolic link) |
| `insertions`, `deletions` | lines added and removed; `0` and `0` for a binary file |
| `binary` | `true` for a file git treats as binary |
| `old_sha`, `new_sha` | the file's blob id on each side, `null` on the side it is missing from |
| `patch` | only on the diff route with patches on: the file's part of the unified diff, from its `diff --git` line; never for a binary file |

Renamed and copied files are found as git finds them (`-M -C`): a file
moved with small edits is one change `renamed` with both paths, not a
deletion and an addition, and a copy is found when the file it was copied
from changed in the same comparison.

## `GET /api/v1/workspaces/{slug}/scm/refs`

Lists the refs of the workspace whose full names begin with `prefix`, as
ref objects in byte order of name. It is a list (see [Lists](#lists)), with
the standard `limit` and `cursor`, and the query parameter:

| Parameter | Meaning |
|---|---|
| `prefix` | the start of the full names listed, matched as text; default `refs/heads/`; `refs/` lists all three namespaces |

The cursor carries the last name of the page, so a walk over the pages
returns each ref once. A prefix no ref has answers
`{"items":[],"next_cursor":null}`.

| Status | When |
|---|---|
| `200 OK` | a page of ref objects |
| `400` `invalid_request` | a bad `limit` or `cursor` (see [Lists](#lists)) |
| `409` `workspace_busy`, `workspace_not_ready` | the workspace's state (see [Source control reads](#source-control-reads)) |
| `503` `scm_timeout` | the read ran out of the read timeout before its answer began (see [Source control reads](#source-control-reads)) |

```json
{"items":[{"name":"refs/heads/feature/x","kind":"branch","sha":"9fceb02d0ae598e95dc970b74767f19372d61af8","protected":false,"tracking":false,"change":null},{"name":"refs/heads/main","kind":"branch","sha":"4ad62a88646935da6b42d1a9b1462904e8d62e91","protected":true,"tracking":false,"change":null}],"next_cursor":null}
```

## `GET /api/v1/workspaces/{slug}/scm/refs/{name}`

Answers the ref object of one ref. `{name}` takes the rest of the path (see
[Routes and matching](#routes-and-matching)): a full ref name, such as
`refs/tags/v1`, or a branch name, which is any name that does not begin with
`refs/` and is read as `refs/heads/{name}`, so `…/refs/feature/x` and
`…/refs/refs/heads/feature/x` answer the same object.

| Status | When |
|---|---|
| `200 OK` | the ref object |
| `404` `ref_not_found` | no listed ref has the name, or it is outside `refs/heads/`, `refs/tags/` and `refs/parley/` (`ref "refs/heads/nope" does not exist`); a tag name without `refs/tags/` is read as a branch name |
| `409` `workspace_busy`, `workspace_not_ready` | the workspace's state (see [Source control reads](#source-control-reads)) |
| `503` `scm_timeout` | the read ran out of the read timeout before its answer began (see [Source control reads](#source-control-reads)) |

## `GET /api/v1/workspaces/{slug}/scm/commits`

Lists the commits reachable from `ref`, newest first, as commit objects. It
is a list (see [Lists](#lists)), with the standard `limit` and `cursor`,
and names the commit it was read from in `resolved_sha` after
`next_cursor`. Its query parameters:

| Parameter | Meaning |
|---|---|
| `ref` | the revision whose history is listed; default the workspace's default branch |
| `first_parent` | `true` (default) follows only the first parent of each merge, so the commits a merge brought in are not listed; `false` lists them too |
| `path` | only the commits that changed this path, a file or anything below a directory, taken literally; a path no commit changed answers `items: []` |
| `follow` | `true` follows `path`, which must then name one file, across renames; default `false` |
| `since`, `until` | RFC 3339 timestamps: only the commits whose committer time is at or after `since` and at or before `until` |
| `author` | only the commits whose author's name or e-mail contains this text, without regard to case |
| `stats` | `true` adds `stats` to each commit; default `false`, which leaves the member out |

The filters are read together. The cursor carries the commit the first page
resolved and how many commits the pages before held, so every page of one
listing is read from that commit, also when `ref` moves between pages; a
cursor used with other query parameters is refused (see [Lists](#lists)).

| Status | When |
|---|---|
| `200 OK` | a page of commit objects, with `resolved_sha` |
| `400` `invalid_request` | a bad `limit` or `cursor`; an illegal `path`; `first_parent`, `follow` or `stats` other than `true` or `false` (`stats must be true or false`); `since` or `until` not an RFC 3339 timestamp (`since must be an RFC 3339 timestamp, such as 2026-01-02T03:04:05Z`); `follow=true` without `path` (`follow=true needs a path, the one file whose renames it follows`) |
| `404` `ref_not_found` | `ref` names no one commit |
| `409` `workspace_busy`, `workspace_not_ready` | the workspace's state (see [Source control reads](#source-control-reads)) |
| `503` `scm_timeout` | the read ran out of the read timeout before its answer began (see [Source control reads](#source-control-reads)) |

```json
{"items":[{"sha":"4ad62a88646935da6b42d1a9b1462904e8d62e91","parents":["9fceb02d0ae598e95dc970b74767f19372d61af8"],"tree_sha":"e7849aa020dd1d5745b252959a661f3c67634838","author":{"name":"Alice","email":"alice@example.com","time":"2026-01-02T03:04:05Z"},"committer":{"name":"Alice","email":"alice@example.com","time":"2026-01-02T03:04:05Z"},"message":"Second\n\nBody one.\nBody two.\n","change":null,"session_id":null}],"next_cursor":"eyJwIjp7InNoYSI6...","resolved_sha":"4ad62a88646935da6b42d1a9b1462904e8d62e91"}
```

## `GET /api/v1/workspaces/{slug}/scm/commits/{sha}`

Answers the commit object of the commit `{sha}` names, which is any
revision (a branch name with a `/` is written with `%2F`, as
`…/commits/feature%2Fx`), always with `stats`. With `files=true` it also
has `files`, the commit's file changes against its first parent, or
against the empty tree for a root commit, in byte order of path, each a
file change (see [Source control reads](#source-control-reads)) without
`patch`; without it, or with `files=false`, the member is absent.

| Status | When |
|---|---|
| `200 OK` | the commit object, with `stats` and, with `files=true`, `files` |
| `400` `invalid_request` | `files` other than `true` or `false` (`files must be true or false`) |
| `404` `ref_not_found` | `{sha}` names no one commit |
| `409` `workspace_busy`, `workspace_not_ready` | the workspace's state (see [Source control reads](#source-control-reads)) |
| `503` `scm_timeout` | the read ran out of the read timeout before its answer began (see [Source control reads](#source-control-reads)) |

```json
{"sha":"9fceb02d0ae598e95dc970b74767f19372d61af8","parents":[],"tree_sha":"755f3edd3ea43bc4b9134fc17299060a0eae2c91","author":{"name":"Alice","email":"alice@example.com","time":"2026-01-01T00:00:00Z"},"committer":{"name":"Alice","email":"alice@example.com","time":"2026-01-01T00:00:00Z"},"message":"c1","change":null,"session_id":null,"stats":{"files_changed":1,"insertions":1,"deletions":0},"files":[{"path":"old.md","old_path":null,"status":"added","insertions":1,"deletions":0,"binary":false,"old_sha":null,"new_sha":"5626abf0f72e58d7a153368ba57db4c673c0e171"}]}
```

## `GET /api/v1/workspaces/{slug}/scm/tree`

Lists the entries of the directory `path` of the commit `ref` names, in
byte order of path. It is a list (see [Lists](#lists)) whose `limit`
defaults to 1000 and may be 1 to 10000, with `cursor`, and names after
`next_cursor` the commit it was read from in `resolved_sha` and the listed
directory's tree id in `tree_sha`. Its query parameters:

| Parameter | Meaning |
|---|---|
| `ref` | the revision whose tree is listed; default the workspace's default branch |
| `path` | the directory listed (see [Source control reads](#source-control-reads) for the paths taken); default, or empty, the root |
| `recursive` | `true` lists every entry below `path`, directories as well as their contents; default `false`, the directory's own entries |

A tree entry:

| Member | Value |
|---|---|
| `path` | the entry's path from the root, such as `docs/a.md` |
| `name` | the last segment of `path`, such as `a.md` |
| `type` | `blob` for a file, `tree` for a directory, `symlink` for a symbolic link (mode `120000`), `commit` for a submodule entry (mode `160000`) |
| `mode` | git's six-digit octal mode: `100644`, `100755`, `040000`, `120000` or `160000` |
| `sha` | the entry's object id; for a submodule, the commit it records |
| `size` | the file's size in bytes, on entries of type `blob` alone |

The cursor carries the commit the first page resolved and the last path of
the page, so every page of one listing is read from that commit, also when
`ref` moves between pages; a cursor used with other query parameters is
refused (see [Lists](#lists)).

| Status | When |
|---|---|
| `200 OK` | a page of tree entries, with `resolved_sha` and `tree_sha` |
| `400` `invalid_request` | a bad `limit` or `cursor`; `recursive` other than `true` or `false` (`recursive must be true or false`); an illegal `path`; a `path` that names a file, a symlink or a submodule entry (`path "README.md" is not a directory at <sha>`) |
| `404` `ref_not_found` | `ref` names no one commit |
| `404` `path_not_found` | `path` does not exist at the commit (`path "nope" does not exist at <sha>`) |
| `409` `workspace_busy`, `workspace_not_ready` | the workspace's state (see [Source control reads](#source-control-reads)) |
| `503` `scm_timeout` | the read ran out of the read timeout before its answer began (see [Source control reads](#source-control-reads)) |

```json
{"items":[{"path":"README.md","name":"README.md","type":"blob","mode":"100644","sha":"13bcb9b3fd97cada4cd021e2e72c979c8238efa1","size":19},{"path":"docs","name":"docs","type":"tree","mode":"040000","sha":"6da2702274f2abf0f9c6f803b21a74ba377d235f"},{"path":"link","name":"link","type":"symlink","mode":"120000","sha":"42061c01a1c70097d1e4579f29a5adf40abdec95"},{"path":"run.sh","name":"run.sh","type":"blob","mode":"100755","sha":"85ba14df52f8c72688537de6e7555fb402217b1e","size":19},{"path":"vendor","name":"vendor","type":"tree","mode":"040000","sha":"a379d76fcb2ec666646b67b3d22eb53bf9fe5489"}],"next_cursor":null,"resolved_sha":"b6916afb245765e766995f2bf355f74e2aee2453","tree_sha":"1cce13dc891e3208bb9939d7b515da0889e99ccf"}
```

## `GET /api/v1/workspaces/{slug}/scm/file`

Answers the file or symbolic link `path` of the commit `ref` names. Its
query parameters:

| Parameter | Meaning |
|---|---|
| `ref` | the revision whose file is read; default the workspace's default branch |
| `path` | the file read, required (see [Source control reads](#source-control-reads) for the paths taken) |

Without `Accept: application/json` the answer is the file's bytes, a
symbolic link's being its target, with `Content-Type:
application/octet-stream`, `Content-Length` and the headers:

| Header | Value |
|---|---|
| `X-Parley-Blob-Sha` | the file's blob id |
| `X-Parley-Resolved-Sha` | the full id of the commit `ref` resolved to |
| `X-Parley-File-Mode` | git's mode of the file: `100644`, `100755` or `120000` for a symbolic link |

A `Range` header of exactly one byte range, `bytes=a-b`, `bytes=a-` or
`bytes=-n` (the last `n` bytes), is answered `206 Partial Content` with
those bytes, the same headers and `Content-Range: bytes <first>-<last>/<size>`;
an end past the file is cut to its last byte. A range that starts at or
after the end of the file (on an empty file, every range) is `416`
`range_not_satisfiable` with `Content-Range: bytes */<size>`. A header with
several ranges, or one that is not a byte range, is ignored and the whole
file answered `200`.

Both forms of a successful answer carry `Vary: Accept`, and each form has
its own `ETag` (see [Source control reads](#source-control-reads)); a
range answer carries the tag of the whole file's raw form.

When `Accept` names `application/json`, the answer is a JSON object and a
`Range` header is ignored:

| Member | Value |
|---|---|
| `path` | the file's path, as asked |
| `sha` | the file's blob id |
| `mode` | git's mode of the file |
| `size` | the file's size in bytes |
| `encoding` | `utf8` when the bytes are valid UTF-8, else `base64` |
| `content` | the bytes as text for `utf8`, their standard base64 encoding for `base64` |
| `resolved_sha` | the full id of the commit `ref` resolved to |

A file larger than the hub's file size limit, `[scm].max_file_size` (32 MiB,
33,554,432 bytes, by default; see [configuration.md](configuration.md)), is
refused before any of it is read, whole or in part, in both forms; a file of
exactly the limit is served. An error is the JSON error envelope in both
forms.

| Status | When |
|---|---|
| `200 OK` | the file, raw or as JSON |
| `206 Partial Content` | one range of the raw file |
| `400` `invalid_request` | no `path` (`path is required`); an illegal `path`; a `path` that names a directory or a submodule entry (`path "docs" is a directory or a submodule at <sha>, not a file`) |
| `404` `ref_not_found` | `ref` names no one commit |
| `404` `path_not_found` | `path` does not exist at the commit (`path "nope.txt" does not exist at <sha>`) |
| `409` `workspace_busy`, `workspace_not_ready` | the workspace's state (see [Source control reads](#source-control-reads)) |
| `413` `payload_too_large` | the file is over the limit (`file "big.dat" is 33554433 bytes, over the limit of 33554432`), with the member `size`, the file's size in bytes, inside `error` |
| `416` `range_not_satisfiable` | the one range starts at or after the end of the file |
| `503` `scm_timeout` | the read ran out of the read timeout before its answer began (see [Source control reads](#source-control-reads)) |

```json
{"path":"README.md","sha":"13bcb9b3fd97cada4cd021e2e72c979c8238efa1","mode":"100644","size":19,"encoding":"utf8","content":"# Tree\n\nA fixture.\n","resolved_sha":"b6916afb245765e766995f2bf355f74e2aee2453"}
```

```json
{"error":{"code":413,"message":"file \"big.dat\" is 33554433 bytes, over the limit of 33554432","error_type":"payload_too_large","size":33554433}}
```

## `GET /api/v1/workspaces/{slug}/scm/diff`

Answers the file changes between two revisions. Its query parameters:

| Parameter | Meaning |
|---|---|
| `base`, `head` | the two revisions compared, both required |
| `mode` | `merge_base` (the default): the merge base of `base` and `head` against `head`, so a file changed only on `base` since they parted is not in the answer; `direct`: `base` itself against `head` |
| `path` | a glob, repeatable: only the files whose path one of them matches are answered and counted; `*` matches within a segment, `**` across segments, the whole path, case-sensitively; no `path` keeps every file |
| `patches` | `true` (the default) or `false`; with `false` no file has `patch` |
| `context` | the lines of context around each change in the patches, an integer from 0 to 20; default 3 |
| `format` | `json` (the default) or `patch` |

The JSON answer:

| Member | Value |
|---|---|
| `base_sha`, `head_sha` | the full ids `base` and `head` resolved to |
| `merge_base_sha` | the full id of their merge base, in both modes; `null` when they share no ancestor, and then the left side in `merge_base` mode is the empty tree, so every file of `head` is `added` |
| `files` | the file changes (see [Source control reads](#source-control-reads)), in byte order of path |
| `stats` | `{"files_changed": …, "insertions": …, "deletions": …}` over every file the `path` globs keep, also those a cut leaves out |
| `truncated` | `true` when the answer was cut |

The answer lists at most 5,000 files, the first in path order. Patches are
added file by file until the next file's patch would take their total past
8 MiB (8,388,608 bytes); that file and every later one is answered without
`patch`. A cut of either kind sets `truncated`.

With `format=patch` the answer is the unified diff of the same files as
text, `Content-Type: text/x-diff`, each binary file as a binary patch, so
that `git apply` applies it to the left side's tree. It is cut at a file
boundary by the same limits, and a cut answer carries the header
`X-Parley-Truncated: true`.

| Status | When |
|---|---|
| `200 OK` | the diff, as JSON or as text |
| `400` `invalid_request` | no `base` or no `head` (`base and head are required`); `mode` other than `merge_base` or `direct` (`mode must be merge_base or direct`); `format` other than `json` or `patch` (`format must be json or patch`); `context` that is not an integer from 0 to 20 (`context must be an integer from 0 to 20`); `patches` other than `true` or `false` (`patches must be true or false`) |
| `404` `ref_not_found` | `base` or `head` names no one commit |
| `409` `workspace_busy`, `workspace_not_ready` | the workspace's state (see [Source control reads](#source-control-reads)) |
| `503` `scm_timeout` | the read ran out of the read timeout before its answer began (see [Source control reads](#source-control-reads)) |

```json
{"base_sha":"6c638ebb967a4ce0b52b308260b292f0dbdbd489","head_sha":"c7a58b21816fb50c8054f4b8d574c7d205f9a4bb","merge_base_sha":"4a5a3035a79aa72418c2fa14d64c763813e558a6","files":[{"path":"edit.txt","old_path":null,"status":"modified","insertions":2,"deletions":1,"binary":false,"old_sha":"b2f931a67315c95c5daab3aac6de62e534808476","new_sha":"c04cd7487d84c5531d5a28b8763a5982ba7b8501","patch":"diff --git a/edit.txt b/edit.txt\nindex b2f931a..c04cd74 100644\n--- a/edit.txt\n+++ b/edit.txt\n@@ -1,5 +1,6 @@\n one\n two\n-three\n+3a\n+3b\n four\n five\n"},{"path":"img.bin","old_path":null,"status":"modified","insertions":0,"deletions":0,"binary":true,"old_sha":"8352675d67aed6625ece79af41c27fdb4ee2e867","new_sha":"a903574af00b573ad9bdb2bccf8d93ed00c675de"},{"path":"new.txt","old_path":null,"status":"added","insertions":3,"deletions":0,"binary":false,"old_sha":null,"new_sha":"5804e5526e21ff0d1a3d282f07834c45f28612ca","patch":"diff --git a/new.txt b/new.txt\nnew file mode 100644\nindex 0000000..5804e55\n--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1,3 @@\n+n1\n+n2\n+n3\n"}],"stats":{"files_changed":3,"insertions":5,"deletions":1},"truncated":false}
```

## `GET /api/v1/workspaces/{slug}/scm/compare`

Answers how two revisions relate and whether `head` can be rebased onto
`base`, from a trial rebase that moves no ref. Its query parameters:

| Parameter | Meaning |
|---|---|
| `base` | the revision `head` would be rebased onto, required |
| `head` | the revision whose commits would be replayed, required |

The trial replays the commits that `head` reaches and `base` does not,
oldest first, on top of `base`, in the repository's object store alone: no
work tree, no index and no ref is used or changed (it may leave objects no
ref reaches, which git's garbage collection removes). It stops at the first
commit that does not apply. It needs git 2.40 or later on the hub's `PATH`.

The answer:

| Member | Value |
|---|---|
| `base_sha`, `head_sha` | the full ids `base` and `head` resolved to |
| `merge_base_sha` | the full id of their merge base; `null` when they share no ancestor |
| `ahead` | the number of commits `head` reaches and `base` does not |
| `behind` | the number of commits `base` reaches and `head` does not |
| `commits` | the first 200 of the commits `head` reaches and `base` does not, oldest first, each a commit object (see [Source control reads](#source-control-reads)) without `stats` |
| `files` | the first 5,000 file changes, in byte order of path, from the merge base (the empty tree when there is none) to `head`, without `patch` |
| `mergeable` | `clean` when every commit applies, `conflict` when one does not |
| `conflict` | `null` when `clean`; otherwise the conflict report |

A `head` that `base` contains answers `ahead` `0`, `commits` `[]`, `files`
`[]` and `clean`.

The conflict report has every member, whichever is empty:

| Member | Value |
|---|---|
| `target_sha` | the commit the head was rebased onto: `base_sha` |
| `head_sha` | `head_sha` |
| `base_sha` | the merge base: `merge_base_sha` |
| `commit_sha` | the first commit of `head` that did not apply |
| `files` | the conflicted files, in byte order of path: `{"path": …, "kind": …, "base": …, "ours": …, "theirs": …, "landed_by": []}` |
| `landed_changes` | the changes landed on the target that the conflict involves; always `[]` until the hub records which change landed which commit |
| `change` | the change whose head was rebased; always `null` for the same reason |

In a conflicted file, `base`, `ours` and `theirs` are the file on each side
of the three-way merge that failed, `{"sha": …, "mode": …, "path": …}` (the
blob id, git's six-digit octal mode and the path on that side, which a rename
makes differ), or `null` on a side the file does not exist on: `base` the
failed commit's parent, `ours` the target with the commits of `head` before
the failed one replayed on it, `theirs` the failed commit. `kind` is
`content` when both sides changed the same lines, `add_add` when both added
the path, `modify_delete` when the target changed a file the head deleted,
`delete_modify` for the reverse, and `rename`, `mode` (a file on one side, a
symlink or another type on the other), `binary` or `submodule` where git
names the conflict so. `landed_by` lists the landed changes that changed the
file; always `[]` for now.

| Status | When |
|---|---|
| `200 OK` | the comparison, `clean` or `conflict` |
| `400` `invalid_request` | no `base` or no `head` (`base and head are required`) |
| `400` `merge_commits_unsupported` | a merge commit is among the commits of `head` not in `base` (`the commits of head that are not in base include a merge commit, which a rebase cannot replay`), refused before anything is replayed |
| `404` `ref_not_found` | `base` or `head` names no one commit |
| `409` `workspace_busy`, `workspace_not_ready` | the workspace's state (see [Source control reads](#source-control-reads)) |
| `503` `scm_timeout` | the read ran out of the read timeout before its answer began (see [Source control reads](#source-control-reads)) |

```json
{"base_sha":"25f1fc8af806873de70bc5fcbc519d5252016e97","head_sha":"6f061333631277a4de8c1b1608151c23149a7d0d","merge_base_sha":"a0ad1d035cc9cc15942e85b7855fab58d6d91173","ahead":2,"behind":1,"commits":[{"sha":"6df6be0f167d2a962d6b959a9adb8d07e5f8db0f","parents":["a0ad1d035cc9cc15942e85b7855fab58d6d91173"],"tree_sha":"fb9593de39c7271ab6cb836f1c86b5ce9d019732","author":{"name":"T","email":"t@example.invalid","time":"2023-11-14T22:13:20Z"},"committer":{"name":"T","email":"t@example.invalid","time":"2023-11-14T22:13:20Z"},"message":"e1","change":null,"session_id":null},{"sha":"6f061333631277a4de8c1b1608151c23149a7d0d","parents":["6df6be0f167d2a962d6b959a9adb8d07e5f8db0f"],"tree_sha":"401eeb772a93302aaa5152c4fdae59a78fd09088","author":{"name":"T","email":"t@example.invalid","time":"2023-11-14T22:13:20Z"},"committer":{"name":"T","email":"t@example.invalid","time":"2023-11-14T22:13:20Z"},"message":"e2","change":null,"session_id":null}],"files":[{"path":"e.txt","old_path":null,"status":"added","insertions":1,"deletions":0,"binary":false,"old_sha":null,"new_sha":"d905d9da82c97264ab6f4920e20242e088850ce9"},{"path":"f.txt","old_path":null,"status":"modified","insertions":1,"deletions":1,"binary":false,"old_sha":"df967b96a579e45a18b8251732d16804b2e56a55","new_sha":"564b12f45becba5fb2f70e270af067c1f13b3aab"}],"mergeable":"conflict","conflict":{"target_sha":"25f1fc8af806873de70bc5fcbc519d5252016e97","head_sha":"6f061333631277a4de8c1b1608151c23149a7d0d","base_sha":"a0ad1d035cc9cc15942e85b7855fab58d6d91173","commit_sha":"6f061333631277a4de8c1b1608151c23149a7d0d","files":[{"path":"f.txt","kind":"content","base":{"sha":"df967b96a579e45a18b8251732d16804b2e56a55","mode":"100644","path":"f.txt"},"ours":{"sha":"ba2906d0666cf726c7eaadd2cd3db615dedfdf3a","mode":"100644","path":"f.txt"},"theirs":{"sha":"564b12f45becba5fb2f70e270af067c1f13b3aab","mode":"100644","path":"f.txt"},"landed_by":[]}],"landed_changes":[],"change":null}}
```

## `GET /api/v1/workspaces/{slug}/scm/blame`

Answers, for the text file `path` of the commit `ref` names, which commit
last changed each line. Its query parameters:

| Parameter | Meaning |
|---|---|
| `ref` | the revision whose file is blamed; default the workspace's default branch |
| `path` | the file blamed, required (see [Source control reads](#source-control-reads) for the paths taken) |
| `start_line` | the first line answered, an integer, 1-based; default 1 |
| `end_line` | the last line answered, an integer, inclusive; default the file's last line, and one past it is cut to it |

The answer:

| Member | Value |
|---|---|
| `resolved_sha` | the full id of the commit `ref` resolved to |
| `path` | the file's path, as asked |
| `ranges` | the lines `start_line` to `end_line` in ranges, in line order, without overlap and without gaps; `[]` for an empty file |

A range is a run of adjacent lines that one commit last changed at adjacent
lines of the file as that commit wrote it; a range is cut at `start_line`
and `end_line`:

| Member | Value |
|---|---|
| `start_line`, `end_line` | the range's first and last line, 1-based and inclusive |
| `commit` | the commit object (see [Source control reads](#source-control-reads)) of the commit that last changed the lines, without `stats` |
| `original_path` | the file's path in that commit; renames are followed, so lines older than a rename carry the path the file had then |
| `original_start_line` | the line the range's first line was in that commit |

Only a text file of at most 2 MiB (2,097,152 bytes) is blamed; its size is
checked before any of it is read. A file is binary, as git decides, when a
NUL byte is among its first 8,000 bytes.

| Status | When |
|---|---|
| `200 OK` | the ranges |
| `400` `invalid_request` | no `path` (`path is required`); an illegal `path`; a `path` that names a directory or a submodule entry (`path "docs" is a directory or a submodule at <sha>, not a file`); a binary file (`file "bin.dat" is binary at <sha>, and a binary file has no lines to blame`); `start_line` or `end_line` that is not an integer (`start_line must be an integer`); `start_line` below 1 (`start_line must be 1 or more`), past `end_line` (`start_line must not be past end_line`) or past the file's last line (`start_line 7 is past the end of "f.txt", which has 6 lines`) |
| `404` `ref_not_found` | `ref` names no one commit |
| `404` `path_not_found` | `path` does not exist at the commit (`path "nope" does not exist at <sha>`) |
| `409` `workspace_busy`, `workspace_not_ready` | the workspace's state (see [Source control reads](#source-control-reads)) |
| `413` `payload_too_large` | the file is over 2 MiB (`file "big.txt" is 2097153 bytes, over the blame limit of 2097152`), with the member `size`, the file's size in bytes, inside `error` |
| `503` `scm_timeout` | the read ran out of the read timeout before its answer began (see [Source control reads](#source-control-reads)) |

```json
{"resolved_sha":"e94a3ed170581b5a82b33fbae52aa59e5440a85c","path":"f.txt","ranges":[{"start_line":2,"end_line":2,"commit":{"sha":"c879c916954e2527c2c290a3aefd9dbd29c583d5","parents":[],"tree_sha":"25112f361a479cb362affd349fdf98b7d9549372","author":{"name":"T","email":"t@example.invalid","time":"2023-11-14T22:13:20Z"},"committer":{"name":"T","email":"t@example.invalid","time":"2023-11-14T22:13:20Z"},"message":"c1","change":null,"session_id":null},"original_path":"f.txt","original_start_line":2},{"start_line":3,"end_line":3,"commit":{"sha":"5ea6473f65744357e45e476ef873539eaabdf894","parents":["dccb1e2ac5045a0393b8c9ca78770ce76457e489"],"tree_sha":"156a012498a0d513b1aeead4e128793235401724","author":{"name":"T","email":"t@example.invalid","time":"2023-11-14T22:13:20Z"},"committer":{"name":"T","email":"t@example.invalid","time":"2023-11-14T22:13:20Z"},"message":"c3","change":null,"session_id":null},"original_path":"f.txt","original_start_line":3},{"start_line":4,"end_line":4,"commit":{"sha":"dccb1e2ac5045a0393b8c9ca78770ce76457e489","parents":["c879c916954e2527c2c290a3aefd9dbd29c583d5"],"tree_sha":"c2db841be5755f9c5c38e623743dae67506a3265","author":{"name":"T","email":"t@example.invalid","time":"2023-11-14T22:13:20Z"},"committer":{"name":"T","email":"t@example.invalid","time":"2023-11-14T22:13:20Z"},"message":"c2","change":null,"session_id":null},"original_path":"f.txt","original_start_line":4}]}
```

## `GET /api/v1/workspaces/{slug}/scm/search`

Searches the contents of every text file of the commit `ref` names for the
lines that match `q`, and answers one item per matching line, ordered by
path (byte order) and then by line. It is a list (see [Lists](#lists))
whose `limit` defaults to 50 and may be 1 to 200, with `cursor`, and names
after `next_cursor` the commit it searched in `resolved_sha`. Its query
parameters:

| Parameter | Meaning |
|---|---|
| `q` | what to look for, required, 1 to 500 characters; by default a literal string, in which characters special in regular expressions match themselves |
| `ref` | the revision whose files are searched; default the workspace's default branch |
| `regex` | `true` takes `q` as an extended regular expression; default `false`. The escapes `\w`, `\W`, `\s` and `\S` outside a bracket expression mean `[[:alnum:]_]`, `[^[:alnum:]_]`, `[[:space:]]` and `[^[:space:]]` with every C library git is built on |
| `case_sensitive` | `false` matches without regard to case; default `true` |
| `path` | a glob, repeatable: only files whose path one of the globs matches are searched (`*` within a segment, `**` across segments, the whole path, case-sensitive); default every file |

A file git treats as binary (a NUL byte among its first 8,000 bytes) is
never searched, and neither is a submodule entry. A match:

| Member | Value |
|---|---|
| `path` | the file's path from the root |
| `line` | the line's number, 1-based |
| `text` | the line without its line ending (`\n` or `\r\n`), cut to its first 500 characters (characters, not bytes, so no character is split) |

A `q` that holds a newline matches no line. The cursor carries the commit
the first page resolved and the path and line of the last match of the
page, so every page of one search reads that commit, also when `ref` moves
between pages; a cursor used with other query parameters is refused (see
[Lists](#lists)).

| Status | When |
|---|---|
| `200 OK` | a page of matches, with `resolved_sha`; `items` `[]` when nothing matches |
| `400` `invalid_request` | a bad `limit` or `cursor`; no `q`, an empty `q` or one over 500 characters (`q must have 1 to 500 characters`); a `q` holding a NUL character (`q must not contain a NUL character`); `regex` or `case_sensitive` other than `true` or `false` (`regex must be true or false`); with `regex=true`, a `q` git refuses as an extended regular expression (`q "(x" is not a valid extended regular expression`) |
| `404` `ref_not_found` | `ref` names no one commit |
| `409` `workspace_busy`, `workspace_not_ready` | the workspace's state (see [Source control reads](#source-control-reads)) |
| `503` `scm_timeout` | the read ran out of the read timeout before its answer began (see [Source control reads](#source-control-reads)) |

```json
{"items":[{"path":"api/greeting.go","line":3,"text":"func Greeting() string {"},{"path":"docs/a.md","line":1,"text":"# Greeting"}],"next_cursor":null,"resolved_sha":"2ecc9343e95b57b9f37e0e94ee6bae610a47b8bd"}
```

## Source control writes

The routes below change a workspace's repository without a clone: the
commit route, the three refs write routes and the rebase route. Every one
of them behaves alike in what follows.

It takes the admin token, an API key or a PAT, and declares the scope
`git:write` for a PAT. It needs write access to the workspace (see
[Workspace access](#workspace-access)), which the owner and the admin
token have. These checks run before the route and touch no repository: no
credential is `401` `unauthenticated`, a PAT without `git:write` `403`
`insufficient_scope`, for an unknown slug as well, a caller who cannot
read the workspace `404` `not_found` (`workspace not found`), and an org
member, who can only read it, `403` `forbidden`. The route then refuses a
workspace that cannot take a write, before it reads the body:

| State | Answer |
|---|---|
| `archived` | `409` `workspace_archived` |
| a reclone, archive or delete running, on any status; `pending` or `cloning` | `409` `workspace_busy` |
| `failed` | `409` `workspace_not_ready` |
| a task fork whose parent is `archived` | `409` `parent_archived` |
| a task fork whose parent is busy or failed | the parent's answer, `409` `workspace_busy` or `workspace_not_ready` |

Every ref a write moves is decided by the push rules and written by the
hub's ref writer, the same ones a `git push` goes through (see
[Git over HTTP](#git-over-http)), so a write is refused exactly where a push of
the same ref update would be: `403` with the rule's message and its
`error_type` (`hub_owned_ref`, `protected_branch`, `default_branch`,
`tracking_branch`, `integration_branch`, `patch_branch`,
`path_not_allowed`), such as `main is the default branch and cannot be
deleted`, or `500` `internal` (`the push rules could not be evaluated; try
again`, never the failure itself) when a rule could not be evaluated. The
rules are asked once per request, once the commit the ref is to point at
is in the repository and before the ref moves. A `path_not_allowed`
refusal names the first path the token may not change as `error.path`; a
`tracking_branch` refusal names the branch the task fork was taken from,
its `fork_point.ref`, as `error.details.base_ref`, the branch to commit to
instead:

```json
{"error":{"code":403,"message":"path src/b.go is outside this token's allowed paths","error_type":"path_not_allowed","path":"src/b.go"}}
```

```json
{"error":{"code":403,"message":"main tracks the parent workspace and is read-only in this fork; push a new branch","error_type":"tracking_branch","details":{"base_ref":"main"}}}
```

A refused write moves no ref, emits no event, opens no change and is not
recorded for an `Idempotency-Key` replay, so a retry is decided again. In
a task fork the tracking branches are the parent's branches the fork shows
(`tracking: true` in the ref object): a write names them by their current
commit, as a push does, and the push rules refuse it, the protected-branch
rule first where it protects the branch (`main is protected and cannot be
force-pushed or deleted` for a delete of the default branch). The ref is moved by
compare-and-swap against the value the route read: a ref that moved in
between is `409` `ref_moved`, with the commit it is at now in
`error.details.current_sha` (`null` when it no longer exists), and nothing
changes. An accepted write emits one `parley.git.push` event per ref, with
`via` `"api"`, as a push does with `via` `"git"`, and the hub's ref writer
tells the post-push listeners of it as of a push, with `via` `"api"` and
the request's session; a refused write tells them nothing. As for a push,
a branch that is the source of a draft or open change records the change's
next version when the write moves it, in the store transaction of the
move: of kind `commit` for a commit, `ref` for a ref move and `rebase` for
a rebase (see [Changes](#changes)). A failure to record it, such as `400`
`unknown_session` for a session the change component refuses, is the
write's answer, and the branch does not move.

The commit and the rebase route take a `session_id`, the agent session the
write belongs to. It is checked after the body and before the branch is
read: a session the hub does not know for the caller and the workspace is
`400` `unknown_session` (`session_id: no session "…"`), and nothing moves.
No session feature is registered yet, so every `session_id` is refused
that way. An accepted session goes, in lower case, onto the write's
events, the version the write records and, for a commit, the commit
object's `session_id`.

The three `POST` routes (commit, ref creation, rebase) are create routes
(see [Idempotency](#idempotency)): a repeated `Idempotency-Key` with the
same body answers `200` with the first answer and moves nothing, the same
key with another body is `409` `conflict`, and a refused request is not
recorded, so its retry is decided again. `PATCH` and `DELETE` ignore the
header; `expected_sha` makes their retry safe.

A protected branch (one the workspace's effective `protected_branches`
match, a task fork's its parent's) is held as for a push. Nobody, the admin
token included, creates one (`release/1 is protected; push another branch
and open a change (git push -o parley.change)`), moves one to a commit that
does not descend from its head, by a ref move or a rebase, or deletes one
(`main is protected and cannot be force-pushed or deleted`). While
`protect.owner_push` is `true`, the workspace's owner or the admin token
moves one forward, by a commit, a ref move or a rebase whose new head
descends from the old one, with an API key, the admin token, or a PAT
neither bound to a workspace nor limited to paths: a direct push, whose
`parley.git.push` event has `direct` `true`. A direct push opens no
change: the route answers `change` `null`, unless the branch is the source
of a draft or open change, when it names the version the move recorded
(see [Changes](#changes)). Every other
caller, and every caller while `protect.owner_push` is `false`, gets `403`
`protected_branch` (`main is protected; push another branch and open a
change (git push -o parley.change)`).

```json
{"error":{"code":409,"message":"refs/heads/work is not at the expected commit; read it again and retry","error_type":"ref_moved","details":{"current_sha":"d81e7ee540a0ae85fa486bfd9c53f46cfbdf4f89"}}}
```

Writes have no time limit. Each request a write route handles is reported
once to the hub's request recorder with the route's op (`commit`,
`ref_create`, `ref_move`, `ref_delete`, `rebase`) and `ok` for a status below 400, `ref_moved` for
a `409` `ref_moved`, `refused` for a `403` of a push rule, `error`
otherwise; a request the checks before the route refuse is not reported.

A ref name in these routes is a full ref name, or a branch name, which is
any name that does not begin with `refs/` and is read as
`refs/heads/{name}`. It must be a name git takes for a ref
(`git check-ref-format`) below `refs/heads/` or `refs/tags/`; a name below
`refs/parley/` reaches the push rules, which refuse it as the hub's
(`403` `hub_owned_ref`); `HEAD` and every other namespace are `400`
`invalid_request`. A commit id in a body or a query is a full
40-character id.

## `POST /api/v1/workspaces/{slug}/scm/commits`

Makes one commit from a list of file operations, with no working copy,
and moves one branch to it, or creates the branch there. The body:

| Member | Meaning |
|---|---|
| `branch` | required: the branch to commit to, a branch name or a full name below `refs/heads/`, which `git check-ref-format` takes |
| `expected_head_sha` | the full id of the commit the caller saw `branch` at; required when `branch` exists |
| `base_ref` | a revision (a branch name, a tag name, a full ref name, a full or abbreviated commit id) naming the commit a new `branch` is created on; required when `branch` does not exist, ignored when it does |
| `message` | required: the commit message, not empty |
| `author` | `{"name": …, "email": …}`, both required, neither holding `<`, `>` or a line break; without it the author is the caller (the user's full name, else the username, and e-mail), and for the admin token the hub (`parley` `<parley@localhost>`) |
| `operations` | the file operations, at most 1,000, applied in order, each to the tree the one before it left; may be empty or absent |
| `allow_empty` | `true` to make a commit whose tree is its parent's; default `false` |
| `change` | `{"target_branch": …, "title": …, "intent": …, "draft": …}`, a change to open with `branch` as its source; refused on a protected branch (see [Source control writes](#source-control-writes)) |
| `session_id` | the agent session the commit belongs to (see [Source control writes](#source-control-writes)) |

An operation is one of:

| `op` | Members | Effect |
|---|---|---|
| `put` | `path`, exactly one of `content` (UTF-8 text) and `content_base64`, optional `mode` (`100644`, the default, `100755` or `120000`) | writes the file, or replaces it, creating its parent directories; with `120000` the content is a symlink's target |
| `delete` | `path` | removes the file; a directory left empty goes with it |
| `move` | `from`, `to` | renames the file, keeping its content and mode |
| `chmod` | `path`, `mode` (`100644` or `100755`) | changes the file's mode alone |

A path is relative to the root of the repository: not empty, not starting
with `/`, without an empty, `.`, `..` or `.git` segment (at any depth).
A segment is `.git` in any letter case, and also where macOS or Windows
would read it as `.git`: with the code points HFS+ ignores (such as
U+200C or U+FEFF), with trailing dots or spaces, up to a `\` or `:`, or as
the short name `git~1`.

The committer is the hub, `parley` `<parley@localhost>`; author and
committer take the time of the request. The commit's parent is the head of
`branch`, or for a new branch the commit `base_ref` names. The branch is
moved, or created, by compare-and-swap against that parent, through the
push rules, so of several requests with the same `expected_head_sha` one
answers `201` and the others `409` `ref_moved`. A request that is refused
moves no ref and emits no event; objects it wrote stay unreferenced for
git's garbage collection.

The checks run in this order: the body and the shape of every operation,
before the repository is read, so a malformed request answers `400`
whatever the branch's state; a `change` object on a protected branch,
before the parent is resolved or any object is written; the
`session_id`; the parent; the operations against the tree; emptiness; the
push rules; the change component's check of the `change` object; then the
compare-and-swap.

Once the branch has moved, the commit has the consequences of a push of
it. After the `parley.git.push` event of the branch, one
`parley.scm.commit` event names the commit (`resource_type` `commit`, its
id as `resource_id`), with the caller as for the push event and the
request's session; its metadata holds `branch` (the branch's name without
`refs/heads/`), `sha`, `parent_sha` and `files`, the paths the commit
changed against its parent (both paths of a move) in byte order, the first
100 of them, with `truncated` `true` when there were more:

```json
{"seq":16,"id":"…","type":"parley.scm.commit","time":"2026-11-15T11:11:44Z","workspace":"demo","actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","resource_type":"commit","resource_id":"6d2ec0406c25abdbdf0518833eff0667aecbff02","action":"commit","session_id":null,"metadata":{"branch":"work","files":["README.md","api/new.go","api/old.go"],"parent_sha":"8792bc3d8b0e88fe7152443b9175ee4b05c54691","sha":"6d2ec0406c25abdbdf0518833eff0667aecbff02"}}
```

The commit names the version kind `commit`, so a branch that is the
source of a draft or open change gets a version, which `change` names. A
`change` object is checked by the change component after the push rules
and before the branch moves, and its refusal is the answer, unchanged
(such as `403` `insufficient_scope`, `400` `target_not_found` or `409`
`change_exists`), with nothing moved; once the branch has moved the change
is opened, in a task fork in its parent, and `change` names it with
version 1 of kind `commit`, with the request's `session_id` as its
session. A write that moved the branch again between the commit and the
open is recorded as version 2, of kind `ref` by the hub, and `change`
names that version; one that deleted the branch abandons the change
(`source_deleted`). The change component checks it as `POST
/api/v1/workspaces/{slug}/changes` does, without the check of a
path-scoped token's paths, which the push rules made of the commit. In a task fork whose `auto_change` is `true`, a
commit without a `change` object to a branch that is the source of no
draft or open change asks the change component to open one in the parent,
with the fork's branch as source and the fork's base branch
(`fork_point.ref`) as target; the commit is kept whatever the outcome, and
when the opening fails `change` is `null` and `change_error` is
`{"error_type": …, "message": …}`, the change component's refusal, or
`internal` (`the change could not be opened`). A direct push opens no
change.

The body limit of this route is `[scm].max_commit_size`, 33554432 bytes
(32 MiB) by default (see [configuration.md](configuration.md)), in place
of `[server].max_body_size`: a larger body is `413` `payload_too_large`,
before it is parsed.

```json
{"commit":{"sha":"6d2ec0406c25abdbdf0518833eff0667aecbff02","parents":["8792bc3d8b0e88fe7152443b9175ee4b05c54691"],"tree_sha":"474930f4d466d1c07e08b0986e3b5ca1556d21ac","author":{"name":"owner","email":"owner@example.com","time":"2026-11-15T11:11:44Z"},"committer":{"name":"parley","email":"parley@localhost","time":"2026-11-15T11:11:44Z"},"message":"m\n","change":null,"session_id":null,"stats":{"files_changed":1,"insertions":1,"deletions":0}},"branch":{"name":"refs/heads/work","kind":"branch","sha":"6d2ec0406c25abdbdf0518833eff0667aecbff02","protected":false,"tracking":false,"change":null},"change":null,"change_error":null}
```

`commit` is the commit object of `GET …/scm/commits/{sha}`, with `stats`;
its `message` ends in a newline, as every message git writes does.
`branch` is the ref object at the new commit. `change` is
`{"workspace_slug": …, "number": …, "version": …}`, the change the commit
made a version of or opened, `null` when there is none; `change_error` is
`null` unless an `auto_change` opening failed, as above.

| Status | When |
|---|---|
| `201 Created` | the commit, the branch and the change, as above |
| `200 OK` | a replay of a recorded `Idempotency-Key` with the same body: the first answer; nothing is created |
| `400` `invalid_request` | no `branch`, or not a branch name; no `message`, or an empty one; an `author` without `name` or `email`; an `expected_head_sha` that is not a full id; more than 1,000 operations (`a commit takes at most 1000 operations, not 1001`); a malformed operation (`operation 0: op "copy" is none of put, delete, move and chmod`): an unknown or missing `op`, a bad path, a `put` with both or neither of `content` and `content_base64` or invalid base64, a mode the operation does not take, a missing member; a `change` object on a protected branch (`main is protected and cannot be the source of a change; commit to another branch to open one`), for every caller, the owner and the admin token included; a `branch` that does not exist and no `base_ref` |
| `400` `invalid_operation` | an operation that does not apply to the tree the ones before it left: a `delete`, `move` or `chmod` of a missing path or a directory, a `move` onto a file or a directory, a `put` below a file or onto a directory, any operation on or below a submodule entry; `details.index` is the zero-based index of the first such operation |
| `400` `empty_commit` | the operations leave the parent's tree as it is and `allow_empty` is not `true` (`the operations leave the tree as it is; set allow_empty to commit it anyway`) |
| `400` `unknown_session` | the `session_id` is not accepted |
| `400`, `403`, `409` of the change component | the change component refuses the `change` object |
| `403` | a push rule refuses the update, such as `protected_branch` |
| `404` `ref_not_found` | `base_ref` names no commit |
| `409` `ref_moved` | `branch` exists and `expected_head_sha` is absent or another commit, or the branch moved in between; `details.current_sha` is where it is |
| `409` `ref_exists` | `branch` does not exist and another branch's name is in the way of it, one being a directory of the other (`ref refs/heads/feat is in the way of refs/heads/feat/x; delete it to create refs/heads/feat/x`) |
| `409` `conflict` | the `Idempotency-Key` was used with another body |
| `413` `payload_too_large` | the body is larger than `[scm].max_commit_size` |

## `POST /api/v1/workspaces/{slug}/scm/refs`

Creates a branch or a tag at a commit and answers its ref object. The body:

| Member | Meaning |
|---|---|
| `name` | required: the ref name; a bare name creates a branch |
| `sha` | the full id of the commit to create the ref at |
| `ref` | a revision naming that commit, in any of the four forms a read takes (branch name, tag name, full ref name, full or abbreviated commit id) |

Exactly one of `sha` and `ref` is given. A tag created so is lightweight.

```json
{"name":"refs/tags/v1","ref":"main"}
```

| Status | When |
|---|---|
| `201 Created` | the ref object of the new ref |
| `200 OK` | a replay of a recorded `Idempotency-Key` with the same body: the first answer; nothing is created |
| `400` `invalid_request` | no `name` (`name is required`); a name that is no valid ref name (`"a..b" is not a valid ref name`) or lies outside the namespaces (`"refs/notes/n" is outside refs/heads/, refs/tags/ and refs/parley/`); both or neither of `sha` and `ref` (`give exactly one of sha and ref`); a `sha` that is not a full id (`sha must be a full 40-character commit id`) |
| `403` | a push rule refuses the creation, such as `hub_owned_ref` for a name below `refs/parley/` |
| `404` `ref_not_found` | the target names no commit (`revision "nope" names no commit`, `revision "…" names an object that is not a commit`) |
| `409` `ref_exists` | the ref exists (`ref refs/heads/main already exists`), or another ref's name is in the way of the name, one being a directory of the other (`ref refs/heads/feat/x is in the way of refs/heads/feat; delete it to create refs/heads/feat`) |
| `409` `ref_moved` | another write created the ref in between; `details.current_sha` is where it is then, `null` when it is gone again |
| `409` `conflict` | the `Idempotency-Key` was used with another body |

```json
{"name":"refs/tags/v1","kind":"tag","sha":"d81e7ee540a0ae85fa486bfd9c53f46cfbdf4f89","protected":false,"tracking":false,"change":null}
```

## `PATCH /api/v1/workspaces/{slug}/scm/refs/{name}`

Moves a branch or a tag to another commit, when it is at `expected_sha`,
and answers its ref object. `{name}` takes the rest of the path, as in
`GET …/scm/refs/{name}`. The body:

| Member | Meaning |
|---|---|
| `sha` | required: the full id of the commit to move the ref to |
| `expected_sha` | required: the full id of the commit the caller saw the ref at |

The `parley.git.push` event has `forced` `true` when the old commit is not
an ancestor of the new one. A move to the commit the ref is at answers
`200` and emits no event. The move names the version kind `ref`, so a
branch that is the source of a draft or open change gets a version.

| Status | When |
|---|---|
| `200 OK` | the ref object at `sha` |
| `400` `invalid_request` | a bad name; no `sha` or `expected_sha`, or one that is not a full id (`sha and expected_sha must both be full 40-character commit ids`) |
| `403` | a push rule refuses the move, such as `protected_branch` |
| `404` `ref_not_found` | no ref has the name (`ref "refs/heads/nope" does not exist`); `sha` names no commit |
| `409` `ref_moved` | the ref is not at `expected_sha`, or moved in between; `details.current_sha` is where it is |

## `DELETE /api/v1/workspaces/{slug}/scm/refs/{name}`

Deletes a branch or a tag. The optional query parameter `expected_sha` is
the full id of the commit the caller saw the ref at; without it the ref is
deleted at the commit it is at. The ref's commits stay in the repository.

| Status | When |
|---|---|
| `204 No Content` | the ref is deleted |
| `400` `invalid_request` | a bad name; an `expected_sha` that is not a full id (`expected_sha must be a full 40-character commit id`) |
| `403` | a push rule refuses the delete: `default_branch` (`main is the default branch and cannot be deleted`), `protected_branch` (`main is protected and cannot be force-pushed or deleted`), `hub_owned_ref` |
| `404` `ref_not_found` | no ref has the name |
| `409` `ref_moved` | the ref is not at `expected_sha`, or moved in between |
| `409` `ref_in_use` | the branch is the source of a draft or open change (`branch work is the source of change 3 and cannot be deleted`) |

## `POST /api/v1/workspaces/{slug}/scm/rebase`

Replays the commits of a branch that `onto` does not hold, oldest first,
on top of the commit `onto` names, and moves the branch to the result, with
no working copy. The body:

| Member | Meaning |
|---|---|
| `branch` | required: the branch to rebase, a branch name or a full name below `refs/heads/` |
| `onto` | required: a revision (a branch name, a tag name, a full ref name, a full or abbreviated commit id) naming the commit to rebase onto |
| `expected_head_sha` | the full id of the commit the caller saw `branch` at; without it the head the request reads first guards the move |
| `session_id` | the agent session the rebase belongs to (see [Source control writes](#source-control-writes)) |

```json
{"branch":"topic","onto":"main","expected_head_sha":"9cdbe993678a61933dbfc1b29496123ac2997ea0"}
```

Each new commit keeps the author, with the author's time and zone, and the
message of the commit it replays; its committer is the hub, `parley`
`<parley@localhost>`, at the time of the request. A commit that changes
nothing on top of what it is replayed onto is left out; when nothing is
left, the branch moves to the `onto` commit. A merge commit among the
commits to replay is refused, since the rebase keeps no merge. The replay
uses no work tree, index or lock, so a rebase that stops leaves nothing
behind but unreferenced objects for git's garbage collection.

When `onto` is the branch's head or one of its ancestors, the answer is
`up_to_date`: the branch does not move, the push rules are not asked and
no event is emitted. Otherwise the branch is moved by compare-and-swap
against the head the request read first, through the push rules, with the
version kind `rebase`, so a branch that is the source of a draft or open
change gets a version; the `parley.git.push` event has `forced` `true`
when the old head is not an ancestor of the new one.

```json
{"status":"rebased","branch":{"name":"refs/heads/topic","kind":"branch","sha":"6038b95a48a0ef29a63329c161332c1accd8e5e8","protected":false,"tracking":false,"change":null},"old_head_sha":"9cdbe993678a61933dbfc1b29496123ac2997ea0","new_head_sha":"6038b95a48a0ef29a63329c161332c1accd8e5e8","onto_sha":"b68c63f9e524e8f13781aff3a3e31a0c7ab1fe6e","change":null}
```

`status` is `rebased` or `up_to_date` (`new_head_sha` equal to
`old_head_sha`). `branch` is the ref object at `new_head_sha`, and
`onto_sha` the commit `onto` names. `change` is
`{"workspace_slug": …, "number": …, "version": …}`: for `rebased`, the
change and the version the rebase recorded, `null` when the branch is the
source of no draft or open change; for `up_to_date`, the draft or open change whose source
the branch is, with its current version; `null` when there is none.

A commit that does not apply ends the rebase: the branch stays where it
was, no event is emitted, and the answer is `409` `rebase_conflict` with
the conflict report in `error.details`. `target_sha` is the `onto` commit,
`head_sha` the branch's head, `base_sha` their merge base (`null` when they
share none), `commit_sha` the first commit that did not apply, and `files`
the conflicted files in byte order of path, each with its `kind`
(`content`, `add_add`, `modify_delete`, `delete_modify`, `rename`, `mode`,
`binary`, `submodule`) and the file on each side, `base` (the commit's
parent), `ours` (`onto` with the commits replayed before it) and `theirs`
(the commit), `null` on a side that lacks it. `landed_changes`, each
`landed_by` and `change` are empty or `null`: the hub does not say yet
which landed change touched the files.

```json
{"error":{"code":409,"message":"commit 3282e695c40b91d83c5ab6b916b3bb6eb1cafbc8 of refs/heads/clash does not apply on top of b68c63f9e524e8f13781aff3a3e31a0c7ab1fe6e","error_type":"rebase_conflict","details":{"target_sha":"b68c63f9e524e8f13781aff3a3e31a0c7ab1fe6e","head_sha":"2d165dbfc00967ebdead889de8ba17e45a81f98d","base_sha":"a5bc5c5cd7650975def32e01a71d7011f00bfeb6","commit_sha":"3282e695c40b91d83c5ab6b916b3bb6eb1cafbc8","files":[{"path":"a.txt","kind":"content","base":{"sha":"814f4a422927b82f5f8a43f8fab6d3839e3983f2","mode":"100644","path":"a.txt"},"ours":{"sha":"4c1ee586b89c236961f884e55c2745659e6e45f6","mode":"100644","path":"a.txt"},"theirs":{"sha":"08a39ce7008d263b922cbee785e726af46facfac","mode":"100644","path":"a.txt"},"landed_by":[]}],"landed_changes":[],"change":null}}}
```

The checks run in this order: the body, before the repository is read;
the `session_id`; the branch and `expected_head_sha`; `onto`; whether the branch holds
`onto`; the merge commits; the replay; then the push rules and the
compare-and-swap.

| Status | When |
|---|---|
| `200 OK` | `rebased` or `up_to_date`, as above; or a replay of a recorded `Idempotency-Key` with the same body: the first answer, and nothing moves |
| `400` `invalid_request` | no `branch` (`branch is required`), or not a branch name (`branch "refs/tags/v1" is not a valid branch name`); no `onto` (`onto is required`); an `expected_head_sha` that is not a full id (`expected_head_sha must be a full 40-character commit id`) |
| `400` `merge_commits_unsupported` | a commit to replay is a merge commit (`refs/heads/merged has a merge commit that main does not hold; a rebase replays no merge commit`) |
| `400` `unknown_session` | the `session_id` is not accepted |
| `403` | a push rule refuses the move, such as `protected_branch` |
| `404` `ref_not_found` | the branch does not exist (`ref "refs/heads/nope" does not exist`); `onto` names no commit |
| `409` `ref_moved` | the branch is not at `expected_head_sha`, or moved while the commits were replayed; `details.current_sha` is where it is |
| `409` `rebase_conflict` | a commit does not apply; `details` is the conflict report |
| `409` `conflict` | the `Idempotency-Key` was used with another body |

## Changes

A change is a numbered proposal to add the commits of a source branch to a
target branch. It belongs to the workspace that holds its target branch
(the target workspace) and is numbered there from 1; a number is never
given twice, also after its change was abandoned. Its reference is
`<workspace_slug>#<number>`, such as `demo#1`. The source branch is a
branch of the target workspace or of a task fork of it.

Every head the source branch had is a version, numbered from 1. The head
of each version stays fetchable at `refs/parley/changes/<number>/<version>`
in the target workspace's repository; for a fork source the commit is
copied there from the fork.

The change object:

```json
{"workspace_slug":"demo","number":1,"ref":"demo#1","title":"Add x","intent":"Because y","state":"open","target_branch":"main","source":{"workspace_slug":"demo","branch":"feat"},"version":1,"head_sha":"0b86db2bc73da0f067ec404140cfc00060162383","base_sha":"dc54adf9e0607cad9039738e34a4c3061a859f17","depends_on":null,"dependants":[],"author":{"actor_type":"api_key","actor_id":"bce9bb4d-cbcb-47c3-ab7a-d18f4b5ab95a","username":"owner"},"session_id":null,"sessions":[],"mergeable":"unknown","conflict":null,"needs_restack":false,"restack_reason":null,"stats":{"commits":1,"files_changed":1,"insertions":1,"deletions":0},"paths":["feat.txt"],"paths_truncated":false,"landing":{"status":"ready","reason":null,"requirements":[]},"auto_land":false,"labels":["docs"],"landed":null,"reverts":null,"reverted_by":null,"created_at":"2026-11-15T13:12:13Z","updated_at":"2026-11-15T13:12:13Z"}
```

| Member | Meaning |
|---|---|
| `state` | `draft`, `open`, `landed` or `abandoned` |
| `version`, `head_sha`, `base_sha` | the current version, its head and its base: the merge base of the head and the target branch; while the target branch does not exist, the previous version's base; `null` when the head shares no history with the target branch |
| `depends_on`, `dependants` | the change this one depends on, `null` for none, and the numbers of the draft and open changes that depend on this one, lowest first |
| `author` | who opened it: `actor_type` (`admin_token`, `api_key`, `pat` or `system`), `actor_id` and `username`, both `null` for the admin token and the hub |
| `session_id`, `sessions` | the session of the current version, `null` for none, and every session that contributed a version, in the order of its first one |
| `mergeable`, `conflict` | whether the current version's own commits rebase onto what the change sits on: `clean`, `conflict` or `unknown`; the conflict report while it is `conflict`, else `null` (see [Mergeability](#mergeability)) |
| `needs_restack`, `restack_reason` | whether the change must be rebased onto what it depends on, and why |
| `stats` | the `commits`, `files_changed`, `insertions` and `deletions` of `base_sha..head_sha`; with no `base_sha`, of every commit of the head, its files measured from the empty tree |
| `paths`, `paths_truncated` | the files the current version touches, both sides of a rename, in byte order, at most 2,000; `paths_truncated` is `true` when it touches more |
| `landing` | whether it may land: `status` (`ready`, `pending`, `blocked`), the `reason` it is blocked for a structural cause (`draft`, `target_not_found`, `nothing_to_land`, `unrelated_history`, `merge_commits`), else `null`, and the policy's `requirements` |
| `landed` | how it landed, `null` until then: `sha`, `at`, `by` (an author object), `land_id` and `commits`, each `{version_sha, landed_sha}` |
| `reverts`, `reverted_by` | the number of the change it reverts, and of the change that reverts it, `null` for none |

Arrays are never `null`; times are RFC 3339 in UTC with whole seconds.

A version has `number`, `head_sha`, `base_sha`, `kind` (`push`, `commit`,
`ref`, `rebase`, `restack`, `revert`, `sync`), `patch_equivalent_to` (the
latest earlier version whose diff against its own base is the same, file by
file, as the interdiff below compares them, white space included, `null`
for none), `created_by` (an author object), `session_id`, `stats` and
`created_at`. A version recorded for a head that was already on the source
branch, such as version 1 of a change opened through the route below, has
kind `ref`.

Every move of the source branch of a draft or open change to a new head
records the next version in the same store transaction as the move, before
the write is answered: a git push (kind `push`), a commit (`commit`), a ref
move (`ref`) and a rebase (`rebase`) through the [source control
writes](#source-control-writes), each by its caller and in its session;
and a sync from origin (`sync`), by the hub (`created_by.actor_type`
`system`). A direct push to a protected branch records the version too
when that branch is the source of a draft or open change; a landing
records none.
A git push that deletes the source branch abandons the change; the
deletion of its task fork abandons every draft and open change from it,
and the deletion of the target workspace deletes its changes. Each
abandonment writes `parley.change.abandon`, its `reason` `source_deleted`
or `source_workspace_deleted`. The versions and their refs stay.

When `parleyd` starts, before it accepts a request, it compares the source
branch of every draft and open change with the head of the change's
current version, so that a move whose bookkeeping a stop lost is not lost:
a branch at another commit records that commit as the next version, of
kind `ref` by the hub (`created_by.actor_type` `system`), with its
`parley.change.version` event; a missing branch, while the source
workspace still exists, abandons the change with `reason`
`source_deleted` by the hub. A change that is in line with its branch is
left as it is, `updated_at` included. Every draft and open change whose
`mergeable` is `unknown` then gets a mergeability check without a request.

## `POST /api/v1/workspaces/{slug}/changes`

Opens a change in the workspace `{slug}`, the target workspace. It needs,
for a personal access token, `changes:write`, read access to `{slug}` and
write access to the source workspace: an org member who only reads
`{slug}` opens a change from a branch of their own task fork of it. A
token bound to a task fork reaches the fork's parent on this route only to
open a change whose `source.workspace_slug` is that fork; any other bound
token opens only from a workspace its binding covers. The body:

| Member | Meaning |
|---|---|
| `source` | required: `{"workspace_slug"?, "branch"}`; `workspace_slug` defaults to `{slug}` |
| `target_branch` | defaults to the fork's base branch for a fork source, else to the workspace's default branch |
| `title` | one line of at most 200 characters; defaults to the subject of the head commit, cut to 200 characters |
| `intent` | at most 16,000 characters; defaults to the body of the head commit, then to the purpose of the source fork, cut to 16,000 characters; required (not empty or white space) unless `draft` is `true` or the target's setting `changes.require_intent` is `false` |
| `draft` | `true` opens the change as a draft |
| `depends_on` | the number of the change this one depends on, a positive integer |
| `auto_land` | defaults to `false` |
| `labels` | defaults to `[]`; at most 20, each matching `^[a-z0-9][a-z0-9_.:/-]{0,62}$`; a repeated label is kept once |
| `session_id` | defaults to the session recorded on the source fork; a defaulted session the session rule refuses is dropped |

```json
{"source":{"branch":"feat"},"labels":["docs"]}
```

The answer is `201` with the change object, at version 1: the head of the
source branch, of kind `ref`, written at `refs/parley/changes/<number>/1`.
`mergeable` is `unknown` until its first check. A draft has `landing`
`{"status":"blocked","reason":"draft","requirements":[]}`; any other
change the landing evaluation's answer, which until the hub has a landing
policy is `{"status":"ready","reason":null,"requirements":[]}`. Until the
hub has a session feature, every `session_id` is refused and a defaulted
one dropped.

The open writes `parley.change.version` for version 1, with `number`,
`ref`, `version`, `head_sha`, `kind`, `base_sha`, `patch_equivalent_to`
and `draft` in its metadata, then, unless the change is a draft,
`parley.change.open`, with `number`, `ref`, `version`, `head_sha`, `title`,
`target_branch`, `source_workspace`, `source_branch`, `depends_on` and
`was_draft` (`false`). Both have `resource_type` `change`, `resource_id`
the number, the target workspace and the change's session. A refused open
writes no change, version, ref under `refs/parley/changes` or event.

The checks run in this order, and a request that fails several gets the
answer of the first: the token's binding; the scope and read access to
`{slug}`; the body; `session_id`; write access to the source workspace;
the state of the target and the source workspace; the target branch
exists; it is not the integration branch of a carry-patch workspace; the
source workspace is `{slug}` or a fork of it; the source branch exists;
it is not the target branch of the same workspace; it has a commit the
target branch lacks; it shares history with the target branch; none of
the commits the target lacks is a merge commit; no draft or open
change has this source; the title and the intent; for a path-scoped
token, every file the version changes against its base matches one of
the token's globs. Of two opens of one source at the same time, one
answers `201` and the other `409` `change_exists`.

| Status | When |
|---|---|
| `201 Created` | the change, as above; a replay of a recorded `Idempotency-Key` with the same body answers the first answer |
| `400` `invalid_request` | a body member of the wrong type; no `source.branch`; more than 20 labels or a label that does not match; a `depends_on` below 1; the source branch is the target branch of the same workspace; a title that is empty, longer than 200 characters or more than one line, also no `title` while the head commit has no subject; an intent longer than 16,000 characters |
| `400` `intent_required` | the intent is empty or white space, the change is not a draft and the target requires an intent |
| `400` `merge_commits_unsupported` | a merge commit is among the commits the target branch lacks |
| `400` `nothing_to_land` | the source branch has no commit the target branch lacks |
| `400` `source_not_related` | the source workspace does not exist, the caller cannot see it, or it is neither `{slug}` nor a fork of it |
| `400` `source_not_found` | no branch `source.branch` in the source workspace |
| `400` `target_not_found` | no branch `target_branch` in the target workspace |
| `400` `unknown_session` | the `session_id` is not accepted |
| `400` `unrelated_history` | the source branch has no common ancestor with the target branch |
| `403` `forbidden` | the caller reads the source workspace but cannot write it |
| `403` `insufficient_scope` | a personal access token without `changes:write` |
| `403` `integration_branch` | `target_branch` is the integration branch of a carry-patch workspace, which the hub rebuilds |
| `403` `path_not_allowed` | a path-scoped token, and a file of the version matches none of its globs; `error.path` names the first such file in byte order |
| `403` `token_bound` | a token bound to a task fork, on the fork's parent, with another `source.workspace_slug` or none |
| `404` `not_found` | the caller cannot read `{slug}` |
| `409` `change_exists` | another draft or open change has this source; the message names its ref, and `details` is `{"workspace_slug", "number", "ref"}` of that change |
| `409` `workspace_busy` | the target or the source workspace is pending, cloning, or under a reclone, archive or delete |
| `409` `workspace_not_ready` | the target or the source workspace failed its clone |
| `409` `workspace_archived` | the target or the source workspace is archived |
| `409` `conflict` | the `Idempotency-Key` was used with another body |

## Reading a change

`GET /api/v1/workspaces/{slug}/changes/{number}`,
`GET /api/v1/workspaces/{slug}/changes/{number}/versions`,
`GET /api/v1/workspaces/{slug}/changes/{number}/versions/{version}`,
`GET /api/v1/workspaces/{slug}/changes/{number}/diff` (see
[below](#get-apiv1workspacesslugchangesnumberdiff)) and
`GET /api/v1/workspaces/{slug}/changes/by-commit/{sha}` read a change of
the workspace `{slug}`. Each needs, for a personal access token,
`changes:read`, and read access to `{slug}`; a token without the scope is
answered `403` `insufficient_scope` also when `{slug}` names no workspace,
and a caller who cannot read `{slug}` `404` `not_found`, as for a slug
that names none. `by-commit` is a path segment of its own: `{number}` is
never `by-commit`.

A change of `{slug}` that the request cannot name answers `404`
`change_not_found` on the routes under `…/changes/{number}`: a `{number}` that is not a
positive integer, a number `{slug}` does not have, and any number on a task
fork's slug, since a fork holds no changes (its changes belong to its
parent). A token bound to a task fork reaches these five routes on the
fork's parent, and there only the changes whose `source.workspace_slug` is
that fork: every other change answers `404` `change_not_found` as if it did
not exist, and `by-commit` finds only those changes.

`GET …/changes/{number}` answers `200` with the change object. `include`, a
comma list of `versions`, `approvals`, `checks`, `stack` and `overlaps`
(also given more than once), adds a member of that name after the change's
own: `versions` the list of versions, oldest first; each other name what
the hub's feature for it computes, and no member while the hub has none
(none has one yet). Any other name, the empty name included, answers `400`
`invalid_request`.

`GET …/changes/{number}/versions` answers `200` with the versions, oldest
first, in the list envelope `{"items":[…],"next_cursor":…}`, paged by
`limit` and `cursor` as every list. Each item is a version object.

`GET …/changes/{number}/versions/{version}` answers `200` with one version
object, `{version}` a version number or `latest`, the current version,
plus `ref`, the name of its version ref:

```json
{"number":2,"head_sha":"…","base_sha":"…","kind":"push","patch_equivalent_to":null,"created_by":{"actor_type":"api_key","actor_id":"…","username":"owner"},"session_id":null,"stats":{"commits":2,"files_changed":1,"insertions":1,"deletions":0},"created_at":"2026-11-15T13:12:13Z","ref":"refs/parley/changes/1/2"}
```

A `{version}` that is neither a positive integer nor `latest` answers `400`
`invalid_request`; a number the change does not have, `404` `not_found`.

`GET …/changes/by-commit/{sha}` finds the change of `{slug}` that landed
the commit `{sha}`, a full commit id of 40 hexadecimal characters, or has
it as the head of a version. A change that landed it comes first, then the
lowest number. The answer is `200` with
`{"change": <change object>, "version": <int or null>, "landed": <bool>}`:
`version` is the highest version of the change with this head, or, for a
commit the change landed, the version it was landed from, `null` when
there is none; `landed` is `true` only for a commit the change landed. A
commit that is neither, also an inner commit of a version, and any commit
on a task fork's slug, answers `404` `change_not_found`; a `{sha}` that is
not 40 hexadecimal characters `400` `invalid_request`.

## `GET /api/v1/workspaces/{slug}/changes/{number}/diff`

Answers the diff of a version of the change, or, with `against`, the
interdiff of two versions: what moved between them, with what a rebase
brought in left out. It needs what the other reads of a change need (see
[Reading a change](#reading-a-change)), and answers a change it cannot
name as they do. All git work happens in the target workspace's
repository, where every version's head is held under its version ref, a
fork source's too. Its query parameters:

| Parameter | Meaning |
|---|---|
| `version` | a version number or `latest` (the default), the current version |
| `against` | a version number; with it the answer is the interdiff of `version` and `against` |
| `path` | a glob, repeatable, as on [`…/scm/diff`](#get-apiv1workspacesslugscmdiff): only the files whose path one of them matches are answered and counted |
| `patches` | `true` (the default) or `false`; with `false` no file has `patch` |
| `format` | `json` (the default) or `patch` |

Without `against`, the answer is the diff of the version from its
`base_sha` to its `head_sha`, computed as `…/scm/diff` computes it in
`direct` mode, with rename detection, 3 lines of context and the same
limits (5,000 files, 8 MiB of patch text, then `truncated: true`). It has
the members of the `…/scm/diff` answer, `base_sha` and `merge_base_sha`
both the version's `base_sha` (both `null` for a version with no base,
whose files are then diffed from the empty tree, every one `added`), and
two more: `version`, the version's
number, and `against`, `null`.

With `against`, a file is listed when either version touches it against
its own base, and carries `changed`: `true` when its patch (base to head,
compared without the `index` line and the hunks' positions) differs
between the two versions or only one of them touches it, else `false`.
A changed file is described, with its `patch`, by the diff of the head of
`against` to the head of `version`, with rename detection: `added` when
it is only at the head of `version`, `deleted` when it is only at the
head of `against`, else `modified`. When that diff finds a rename that
involves a changed file, the rename's one entry (`renamed`, with
`old_path`) describes both of its paths: it is listed at its new path when
that is a changed file, else at its old one, and the old path has no entry
of its own. A changed file whose content is the same at both heads (the
bases differ) is `modified` with no line changed, `old_sha` and `new_sha`
both its blob at the heads, and an empty `patch`. An unchanged file
has no `patch` and is described by the version's own diff (`against`'s
when only that touches it). `base_sha` is the head of `against`,
`head_sha` the head of `version`, `merge_base_sha` `null`, `version` and
`against` the two numbers, and `stats` count the changed files only.
Two patch-equivalent versions, and a version compared with itself, have no
changed file. The limits of the plain diff apply.

With `format=patch` the answer is the patch text of the files that would
carry a `patch`, `Content-Type: text/x-diff`, as on `…/scm/diff`: it
applies to the tree of `base_sha` (without `against`) or of the head of
`against`, and a cut answer carries `X-Parley-Truncated: true`.

```json
{"base_sha":"dd626b3e…","head_sha":"8adab83c…","merge_base_sha":null,"files":[{"path":"a.txt","old_path":null,"status":"modified","insertions":1,"deletions":1,"binary":false,"old_sha":"…","new_sha":"…","changed":false},{"path":"b.txt","old_path":null,"status":"modified","insertions":1,"deletions":0,"binary":false,"old_sha":"…","new_sha":"…","patch":"diff --git a/b.txt b/b.txt\nindex …\n--- a/b.txt\n+++ b/b.txt\n@@ -2,3 +2,4 @@ b1\n b2\n b3\n b4\n+b5\n","changed":true}],"stats":{"files_changed":1,"insertions":1,"deletions":0},"truncated":false,"version":3,"against":1}
```

| Status | When |
|---|---|
| `200 OK` | the diff or the interdiff, as JSON or as text |
| `400` `invalid_request` | `version` that is neither a positive integer nor `latest` (`version must be a positive integer or latest`); `against` that is not a positive integer (`against must be a positive integer`); `format` other than `json` or `patch` (`format must be json or patch`); `patches` other than `true` or `false` (`patches must be true or false`) |
| `404` `change_not_found` | as for the other reads of a change |
| `404` `not_found` | a `version` or `against` number the change does not have |
| `404` `ref_not_found` | the base or head commit of a named version is no longer in the repository, such as after a reclone; the change and its versions stay readable |

## `GET /api/v1/workspaces/{slug}/changes`

Lists the changes of the workspace `{slug}`. It needs, for a personal
access token, `changes:read`, and read access to `{slug}`, answered as for
the reads of a change above. The answer is `200` with the list envelope
`{"items":[…],"next_cursor":…}`, paged by `limit` (1 to 200, default 50)
and `cursor` as every list: following `next_cursor` until it is `null`
gives every matching change once, whatever the limit. Each item is the
change object without `conflict`, and without `paths` and
`paths_truncated` unless `include=paths`; any other `include` value, the
empty one included, answers `400` `invalid_request`.

Without filters the list holds the changes in state `draft` or `open`,
the most recently updated first: by `updated_at` descending, as the change
object shows it (in whole seconds), then by `number` descending.
`sort=number` orders by `number` ascending instead. The filters combine,
each narrowing the list:

| Parameter | Keeps the changes |
|---|---|
| `state` | in this state: `draft`, `open`, `landed` or `abandoned`; given more than once, in any of them. Without it, `draft` and `open` |
| `target_branch` | with this target branch |
| `source_workspace` | whose source branch is in this workspace |
| `author` | opened by this user id; `me` names the caller's user |
| `session_id` | to which this session contributed a version, current or earlier |
| `label` | that carry this label |
| `path` | that touch, among their stored `paths`, a file this glob matches, in the glob syntax of a token's `paths` (`*` within a path segment, `**` across them) |
| `mergeable` | `clean`, `conflict` or `unknown` |
| `landing_status` | whose `landing.status` is `ready`, `pending` or `blocked` |
| `stack_of` | of the stack of this change, see below |

`stack_of=<number>` lists the stack of that change: the change it reaches
by following `depends_on` down to the one that depends on nothing, then
every change that depends on one already listed, level by level, lower
numbers first within a level. The stack is listed in every state and in
that order, whatever `state` and `sort` say; the other filters still
apply. A `stack_of` that names no change the request may see answers `404`
`change_not_found`.

A `state`, `mergeable` or `landing_status` outside its values, a `sort`
other than `number`, `author=me` from the admin token, which has no user,
and a `stack_of` that is not a positive integer answer `400`
`invalid_request`.

On a task fork's slug the list holds the changes of the fork's parent
whose `source.workspace_slug` is the fork, with the same filters and
order; a fork holds no changes of its own. A token bound to a task fork
lists, on the fork's parent, only the changes whose source is that fork.

## Modifying a change

`PATCH /api/v1/workspaces/{slug}/changes/{number}`,
`POST /api/v1/workspaces/{slug}/changes/{number}/abandon` and
`POST /api/v1/workspaces/{slug}/changes/{number}/restore` modify a change
of the workspace `{slug}`. Each needs, for a personal access token,
`changes:write`, and read access to `{slug}`: a token without the scope is
answered `403` `insufficient_scope` also when `{slug}` names no workspace,
and a caller who cannot read `{slug}` `404` `not_found`, as for a slug
that names none. The change is then found as on the [reads of a
change](#reading-a-change): a number that names no change the request may
reach, the changes outside its fork for a token bound to a task fork
included, answers `404` `change_not_found`.

A change may be modified by the user who opened it (its
`author.actor_id`), with any credential of theirs, also when they only
read `{slug}`; by a caller who writes `{slug}` (its owner); and with the
admin token. Anyone else who reads `{slug}` is answered `403`
`forbidden`, and the change is left as it was.

Then the body is checked (`400` `invalid_request`), and then the state of
`{slug}`: while it is `pending` or `cloning`, or a reclone, archive or
delete of it runs, the answer is `409` `workspace_busy`; when its clone
failed, `409` `workspace_not_ready`; when it is archived, `409`
`workspace_archived`. Each route answers `200` with the change object as
`GET …/changes/{number}` returns it.

- `PATCH` takes a JSON object whose members `title`, `intent`, `labels`,
  `auto_land`, `state` and `target_branch` edit the change; a member the
  body does not name keeps its value, and members it does not know are
  ignored. Every member it names is checked before anything is stored,
  and one that fails refuses the whole body with `400` `invalid_request`:
  `title` a string of 1 to 200 characters (Unicode code points; white
  space only is empty) without a line feed or carriage return, `intent` a
  string of at most 16,000 characters, `labels` an array of strings,
  which replaces the whole list after its duplicates are removed (the
  first kept), of at most 20 labels each matching
  `^[a-z0-9][a-z0-9_.:/-]{0,62}$`, `auto_land` `true` or `false`, `state`
  `"draft"` or `"open"`, and `target_branch` a string; `null` is the
  wrong type for each. A body that names none of them changes nothing,
  `updated_at` included, whatever the change's state. An abandoned change
  takes no edit and a landed one only an edit of `labels` alone: else
  `409` `change_closed`. An edit that would leave the change `open` with
  an empty intent while `{slug}`'s `changes.require_intent` is on is
  answered `400` `intent_required`. A body with `auto_land` asks the
  `auto_land` guard, when one is registered, before anything is stored;
  its refusal is the answer and nothing of the body is applied. A
  `state` of `"open"` on a draft marks it ready: the change becomes
  `open`, its `landing` is evaluated again without the `draft` block, and
  `parley.change.open` is written by the caller with `number`, `ref`,
  `version`, `head_sha`, `title`, `target_branch`, `source_workspace`,
  `source_branch`, `depends_on` and `was_draft` (`true`), followed by
  `parley.change.ready` when the change is then landable (see
  [Mergeability](#mergeability)). A `state` of `"draft"` on an open change
  makes it a draft: `landing` becomes
  `{"status":"blocked","reason":"draft"}`, its versions, labels,
  `auto_land` and `mergeable` stay, its landable announcement is
  forgotten, and no event is written. A `state` the change already has is
  no edit. A `target_branch` other than the change's own retargets a
  draft or open change. The branch is checked as an open checks its
  target, with the same answers: `400` `target_not_found` for a branch
  `{slug}` does not have, `403` `integration_branch` for the integration
  branch of a carry-patch workspace, `400` `invalid_request` for the
  change's own source branch in `{slug}`. A change that depends on
  another, or that a draft or open change depends on, is answered `409`
  `conflict`. The retarget records no version: `target_branch` is set,
  `base_sha` becomes the merge base of `head_sha` and the new target's
  head, `stats`, `paths` and `paths_truncated` are measured against it,
  and the change's versions keep what they were measured against.
  `landing` is evaluated again, so a branch that already has every
  commit of the change, or one from which a merge commit lies before the
  head, is accepted and reported as `{"status":"blocked","reason":
  "nothing_to_land"}` or `"merge_commits"`; a change whose target branch
  was deleted may be retargeted onto one that exists. `mergeable` becomes
  `unknown` and is checked again, the landable announcement is
  forgotten, and `parley.change.version` is written by the caller with
  `number`, `ref`, `version` and `head_sha` (the current ones), `kind`
  `retarget`, the new `base_sha`, `patch_equivalent_to` `null` and
  `draft`. A `target_branch` a draft or open change already has is no
  edit; on a landed or abandoned change it is refused like any other member. The
  members are stored together, the retarget after the other members and
  before a move between draft and open, and `updated_at` advances only
  when a stored value changes; editing `title`, `intent`, `labels` or
  `auto_land` writes no event, and the events of a retarget and of a move
  from draft to open come after them, so they carry the edited `title`.
- `POST …/abandon` takes no body or a JSON object (no body is the same as
  `{}`) with two optional members: `reason`, a string, and `dependants`,
  `"reparent"` or `"abandon"`; another type, `null`, or another value of
  `dependants` is `400` `invalid_request`. An abandoned or landed change
  is answered `409` `change_closed`. A change on which a draft or open
  change depends (its `dependants`) is answered `409` `has_dependants`
  when the body has no `dependants`, with the numbers of the dependants
  in the error's `dependants` member, and nothing changes. The change
  moves to `abandoned`, its waiting mergeability check is dropped and its
  landable announcement forgotten, and `parley.change.abandon` is
  written by the caller with `reason` as given (the empty string when
  absent), `number`, `ref`, `version` and `head_sha`. The source branch
  and the version refs stay. With `"abandon"`, every draft or open change
  above it in its stack, direct or not, is abandoned too in the same
  transaction, with the same `reason` and one `parley.change.abandon`
  each. With `"reparent"`, each direct dependant stays as it is but now
  depends on what the abandoned change depended on (`null` when nothing),
  and its restack is asked for, by the stack feature when the hub has
  one: when that request fails, the answer is `500` and nothing is
  abandoned; without it, the dependant's `mergeable` becomes `unknown`
  and is checked again. `dependants` on a change that has none changes
  nothing.
- `POST …/restore` takes no body (one that is sent is ignored) and brings
  an abandoned change back to `open` under its number. Its refusals come
  in this order, the first that applies answering, and none changes
  anything or creates a branch: a change that is `draft`, `open` or
  `landed` is `409` `change_not_abandoned`; a change whose source
  workspace, a task fork, no longer exists is `409`
  `source_workspace_gone` (its version refs stay); another draft or open
  change with the same source workspace and branch is `409`
  `change_exists`, as an open answers it, the message naming that
  change's ref; a source branch that no longer exists while the head
  commit of the change's last version is gone from the source
  workspace's repository too is `409` `conflict`; and an empty intent
  while `{slug}`'s `changes.require_intent` is on is `400`
  `intent_required`. A source branch that no longer exists while that
  commit does is created at it, with a create-only ref update; a branch
  that appeared in the meantime is taken at its head. The change moves
  to `open` (a change that was a draft when abandoned too), its `landing`
  is evaluated again, `mergeable` becomes `unknown` and is checked again,
  and `parley.change.restore` is written by the caller with `number`,
  `ref`, `version` and `head_sha` of the last version; no
  `parley.change.open` is written. When the source branch is at another
  commit than the last version's head, that commit is then recorded as a
  new version of kind `ref`, created by the caller with their
  credential, with its `parley.change.version`; the answer carries it as
  `version` and `head_sha`. Of two restores of two changes with one
  source, one is answered `200` and the other `409` `change_exists`.

## Mergeability

The hub computes `mergeable` in the background for every `draft` and
`open` change, without being asked: it trial-rebases the commits of the
current version that the onto-commit lacks onto the onto-commit, the head
of the change named by `depends_on` while that change is `draft` or
`open`, otherwise the head of the target branch. The result is `clean`,
or `conflict` with the conflict report in `conflict`: `target_sha` (the
onto-commit), `head_sha`, `base_sha`, `commit_sha` (the first commit that
does not apply), `files` (each `path`, `kind`, `base`, `ours`, `theirs`,
`landed_by`), `landed_changes` and `change`. The trial writes no ref and
no work tree in either repository.

`mergeable` becomes `unknown`, with `conflict` `null`, in the transaction
of what triggers a new check: a new version of the change, a new version
of the change it depends on, a move, creation or deletion of its target
branch by a push, the source control API, a landing or a sync, a
retarget and a restore. It stays `unknown` until the check stores its result; a result
for a head or an onto-commit that is no longer the change's is
discarded and computed again. Two checks of one change start at least
`[changes].mergeable_debounce` apart (default `5s`, see
[configuration.md](configuration.md#keys)). While the target branch does
not exist, `mergeable` stays `unknown` and `landing` reports
`{"status":"blocked","reason":"target_not_found"}`. A trial rebase that
fails or runs past 30 seconds is stopped and leaves `mergeable`
`unknown` until the change's next trigger.

Every change event, these two included, has `resource_type` `change`,
`resource_id` the number, the target workspace, the change's session,
and `number` and `ref` in its metadata. A result writes, by the hub
(`actor_type` `system`), in the transaction that stores it:

- `parley.change.conflict` when the check finds a conflict and the value
  the last check computed was `clean`, or there was none: metadata
  `number`, `ref`, `version`, `head_sha`, `files` (the conflicted paths)
  and `needs_restack`. A further conflict, for the same or a later
  version or target head, replaces the conflict report and writes no
  event; an `unknown` in between does not count. A `clean` result clears
  the report, so the next conflict is announced again.
- `parley.change.ready` when the change is `open`, its `landing.status` is
  `ready` and `mergeable` is `clean` (landable) at a `head_sha` it has not
  been announced at since it last was not landable: metadata `number`,
  `ref`, `version` and `head_sha`, nothing else. A target move that leaves
  it clean announces nothing again; a result or a step that finds it not
  landable (a draft, a landing `pending` or `blocked`, a conflict), an
  abandon or a retarget forgets the announcement, so it is announced
  again the next time it is landable, at the same head too. While
  `mergeable` is `unknown` nothing is announced or forgotten.

When the event cannot be written, or the transaction fails, neither the
result nor the event is stored: `mergeable` stays `unknown`, the failure
is logged as `the mergeability check failed`, and the change's next
trigger computes it again.

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
events of the workspaces its binding covers, and no event of no workspace;
a token bound to a task fork also sees the change events (`resource_type`
`change`) of the fork's parent whose change has the fork as its source.

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
