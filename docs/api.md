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
them to the changes whose source is the fork is each route's own, and the
four awareness reads do not confine: they show all of the parent's work
(see [Awareness](#awareness)).

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
The id is in the hub's log line for the request, and in every line logged
with the request's context while it is handled, so quote it when reporting
a problem.

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
- `/healthz`, `/readyz` and `/version`, the forge webhook ingress
  ([`POST /hooks/forge/{slug}`](#post-hooksforgeslug)), `GET /metrics`, and
  any request answered `404` or `405` at route matching are never counted
  or refused.
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
  - The body (gzip-decoded when `Content-Encoding` says so) holds, from a
    shallow clone, a `shallow <id>` line for each boundary commit of the
    client's history, then the commands `<old> <new> <ref>`, the client's
    capabilities after a NUL on the first, a flush packet, then the push options and a flush packet
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
  - A push from a shallow clone is accepted when each boundary commit it
    names is one the repository already holds, or one its refs do not
    reach. When the pack itself brings a boundary commit, a ref would end
    on a history whose older commits the repository lacks: the whole push
    is refused, every ref reported `ng <ref> shallow update not allowed`,
    with `error: shallow update not allowed` on the side band. Fetch the
    missing history (`git fetch --unshallow`) and push again.
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
    branch`), the carry-patch controls of a `carry_patch` workspace's
    integration branch and patch branches (see [Pushes to the integration
    branch and patch branches](#pushes-to-the-integration-branch-and-patch-branches),
    which also forward an update to origin before it is written) and the
    paths of a path-scoped PAT
    (`path <path> is outside this token's allowed paths`: every path a new
    commit changes; for an update that adds no commit of its own and moves
    a branch that is the source of a draft or open change, or opens a
    change, every file of the version it makes, the paths that differ
    between the new head and its merge base with the change's target
    branch (for an update that opens a change, the branch the open
    defaults to), both paths of a rename, read from the repository; every
    path of a tree a ref is pointed at; and, for a blob a ref is pointed
    at, which has no path, the ref's name); one that
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
(`400`). The counter is exported at [`GET /metrics`](#get-metrics).

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

## `GET /metrics`

The hub's metrics in the Prometheus text exposition format, version 0.0.4.
It is outside `/api/v1` and needs no credential: an `Authorization` header,
valid or not, changes nothing. Label values include workspace slugs, so it
should not be reachable from untrusted networks.

| Status | When |
|---|---|
| `200 OK` | `GET` or `HEAD`, with `Content-Type: text/plain; version=0.0.4; charset=utf-8` |
| `405` `method_not_allowed` | any other method, with `Allow: GET, HEAD`; nothing is exposed |

`/api/v1/metrics` is `404`. The route is not rate-limited.

Every metric of the table below is exported from the moment the hub
serves, in the order of the table, each with one `# HELP` and one `# TYPE`
line, and at least one sample: every combination of the values of its
closed labels (those listed with their values) is present, at `0` until
something is counted. A metric with an open label (`method`, `path`,
`status`, `workspace`, `model`, `type`) is exported with that label empty
and the value `0` until its first series exists, and without that
placeholder from then on. Samples are sorted by their label values in the
order of the labels; in a label value a backslash, a double quote and a
line feed are written `\\`, `\"` and `\n`; a whole number is written
without an exponent. A
histogram has one `_bucket` sample per bound plus `le="+Inf"`, cumulative,
then `_sum` and `_count`. A metric whose value is read from its owner when
the page is served (the git requests, the open changes, the active forks,
the event counters, the audit events, the job queue) is exported at `0` for
that read when the read fails, and the failure is logged with the metric's
name; a page is never refused for it.

| Metric | Type | Labels | Counts |
|---|---|---|---|
| `parley_http_requests_total` | counter | `method`, `path`, `status` | every HTTP request the hub answered, REST, git, the forge webhook ingress, `/healthz`, `/readyz`, `/version` and `/metrics` itself included. `method` is the request's method in upper case, `OTHER` for any method but `GET`, `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`, `OPTIONS`, `CONNECT` and `TRACE`; `path` is the route that matched, as a template (`/api/v1/workspaces/{slug}/sync`, `/git/{org}/{repo}/info/refs`, `/healthz`), never the request's own path, and `unmatched` when no route matched (an unknown path, a method no route of the path has); `status` the three digits sent, `101` for a request whose connection the hub took over (a sandbox's attach) |
| `parley_http_request_duration_seconds` | histogram | `method`, `path` | the time to answer, by the same `method` and `path`. Buckets `0.005`, `0.01`, `0.025`, `0.05`, `0.1`, `0.25`, `0.5`, `1`, `2.5`, `5`, `10` |
| `parley_git_requests_total` | counter | `service` (`info-refs`, `upload-pack`, `receive-pack`), `result` (`ok`, `refused`, `error`) | git protocol requests, as in [Git over HTTP](#git-over-http) |
| `parley_scm_requests_total` | counter | `op` (`refs`, `commits`, `tree`, `file`, `diff`, `compare`, `blame`, `search`, `outline`, `commit`, `ref_create`, `ref_move`, `ref_delete`, `rebase`), `result` (`ok`, `ref_moved`, `refused`, `error`) | source control API requests, reads and writes |
| `parley_changes_open` | gauge | `workspace` | changes in state `draft` or `open` |
| `parley_lands_total` | counter | `result` (`landed`, `conflict`, `blocked`, `failed`) | land attempts by final status |
| `parley_land_attempts` | histogram | none | compare-and-swap tries per land attempt. Buckets `1`, `2`, `3`, `5`, `8`, `13` |
| `parley_land_duration_seconds` | histogram | none | time from a land request to its final status. Buckets `0.05`, `0.1`, `0.25`, `0.5`, `1`, `2.5`, `5`, `10`, `30`, `60` |
| `parley_forks_active` | gauge | none | task forks that exist |
| `parley_events_seq` | gauge | none | the last `seq` written to the event log |
| `parley_sse_connections` | gauge | none | open event streams |
| `parley_webhook_deliveries_total` | counter | `result` (`ok`, `retry`, `failed`) | event webhook delivery attempts since the hub started: `ok` a successful attempt, ping or redelivery; `retry` a failed attempt of the delivery loop that will be tried again; `failed` an attempt after which the event was given up, a failed ping or redelivery, and an event skipped because it left the event log (see [Event webhooks](#event-webhooks)) |
| `parley_webhook_queue_depth` | gauge | none | events written and not yet delivered or given up, summed over the `active` and `failing` event webhooks: the sum of their `pending` (see [`GET /api/v1/workspaces/{slug}/webhooks/{id}`](#get-apiv1workspacesslugwebhooksid)) |
| `parley_audit_events_total` | counter | `source` (`hub`, `agent`), `type` | hub events written and run events ingested since the hub started, by type |
| `parley_agent_sessions_active` | gauge | `workspace` | sessions in state `active`, set by the retention run; a workspace without one has no series |
| `parley_agent_tokens_total` | counter | `workspace`, `model`, `direction` (`input`, `output`, `cache_read`) | tokens reported in usage records since the hub started |
| `parley_audit_records` | gauge | `kind` (`hub_event`, `run_event`, `outcome`, `tool_call`, `tool_error`, `trace`, `postmortem`, `session`, `usage`) | stored records, counted by the retention run |
| `parley_jobqueue_depth` | gauge | `type` (`rebuild`), `status` (`queued`, `running`, `failed`, `completed`, `dead_letter`, `cancelled`) | durable jobs |
| `parley_retention_errors_total` | counter | `step` (the eleven step names of [the retention run](configuration.md#retention)) | failed retention steps |
| `parley_retention_last_run_timestamp_seconds` | gauge | none | Unix time of the last retention run in which every step succeeded |
| `parley_sandboxes` | gauge | `state` (`provisioning`, `ready`, `stopped`, `completed`, `failed`) | sandboxes by state |
| `parley_sandbox_creates_total` | counter | `result` (`ready`, `failed`) | sandbox creations by outcome |
| `parley_sandbox_seconds` | histogram | none | time from a sandbox's `ready` to its stop, exit or deletion. Buckets `60`, `300`, `600`, `1200`, `1800`, `3600`, `7200`, `14400`, `28800`, `86400` |
| `parley_sandbox_exec_total` | counter | `result` (`ok`, `nonzero`, `timeout`, `error`) | commands run through sandbox exec |
| `parley_sandbox_tool_calls_total` | counter | `tool`, `outcome` (`ok`, `tool_error`, `timeout`, `failed`) | [tool calls](#the-tools-of-a-sandbox) that started in a sandbox, by the tool's name and how the call ended |
| `parley_sandbox_tool_call_duration_seconds` | histogram | `tool` | the time such a call ran. Buckets `0.05`, `0.1`, `0.25`, `0.5`, `1`, `2.5`, `5`, `10`, `30`, `60`, `300`, `600` |
| `parley_sandbox_denials_total` | counter | none | connections and requests the sandbox runtime blocked |
| `parley_sandbox_checkpoints_total` | counter | `result` (`ok`, `empty`, `failed`) | checkpoints attempted |
| `parley_sandbox_runtime_up` | gauge | none | `1` while the sandbox runtime is reachable |

The sandbox metrics stay at `0` while sandboxes are off. Counters start
from zero when `parleyd` starts.

```text
# HELP parley_lands_total Land requests by final result.
# TYPE parley_lands_total counter
parley_lands_total{result="blocked"} 0
parley_lands_total{result="conflict"} 0
parley_lands_total{result="failed"} 0
parley_lands_total{result="landed"} 2
```

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
| `integration_branch` | no | a string or `null`; with `carry_patch` a valid branch name, stored as given (`null` when absent, until the set-up after the first clone stores `deploy`, see [Carry-patch workspaces](#carry-patch-workspaces)); with `standard` only `null` is accepted |
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

The hub registers the carry-patch part of sync as both the provider and the
extension, so on a `carry_patch` workspace:

- The provider names the branch of every patch whose status is `active`,
  `conflict`, `disabled` or `merged_upstream`; the branch of a `deleted`
  patch is an ordinary branch. Patch branches are left out with or without
  `reset_to_origin=true`.
- Right after the origin fetch the hub repeats the workspace's set-up,
  which fetches upstream (`refs/remotes/upstream/*`, no tags) with the
  upstream credentials read at that moment, then resolves the upstream
  base. A failed upstream fetch, or upstream credentials that cannot be
  read, is the `502` `upstream_error` above, with the fetch step's error as
  message (it names upstream); an upstream with neither a default branch
  nor a branch named like the workspace's `default_branch` is the same
  `502` with `upstream base cannot be resolved for workspace "<slug>":
  upstream has neither a default branch nor a branch named like the
  default branch`. Another set-up step that fails is logged and does not
  fail the sync. A missing integration branch is created at the upstream
  base only once the base resolves, so a sync that fails creates none. The
  upstream fetch of a sync waits for one that a rebuild or another sync of
  the workspace is running.
- After the comparison of every branch, with `patch_branch_source`
  `origin`, every `active`, `conflict` or `disabled` patch is refreshed in
  position order against origin's head of its branch as this sync fetched
  it, or, when the hub's head is neither that head nor its ancestor but is
  a newer head a later sync fetched, or its ancestor, against that newer
  head, so that a sync never moves a patch branch back to an older head of
  origin. The first rule that fits gives `action` and `state`: origin lacks the
  branch, `none` and `missing_on_origin`; the hub lacks it, `created` and
  `in_sync`; equal heads, `none` and `in_sync`; the hub's head an ancestor
  of origin's, `fast_forwarded` and `in_sync`; otherwise, under
  `patch_divergence_policy` `report`, `none` and `diverged`, and under
  `replace`, `replaced` and `in_sync`, after the hub's head is saved to
  `refs/parley/replaced/<branch>`, only while that ref holds what it held
  at the comparison (a backup another writer saved meanwhile is kept, and
  the refresh fails as for a lost swap). Every create and move is a
  compare-and-swap that expects the head compared; protected branches move
  too. The outcome is stored on the patch (`origin_sync_state`,
  `origin_sha`, `origin_synced_at`) in one transaction with its events:
  `parley.patch.sync` (metadata `branch_name`, `action`, `state`,
  `local_sha`, `origin_sha`) when the branch moved or the state differs
  from the one stored, and `parley.patch.replace` (metadata
  `branch_name`, `replaced_sha`, `origin_sha`, `trigger` `sync`) for a
  replace. Each move is told to the branch-move observers with cause
  `sync`. With `patch_branch_source` `hub` no patch branch is compared and
  the three origin fields of every patch become `null`; under `origin`
  they are `null` on every `merged_upstream` patch.
- A refresh that fails (a branch or its backup ref moved in the hub
  between the comparison and the swap, a git or a store failure) stops the
  sync at that patch: later patches are not refreshed, nothing below runs,
  and the answer is `500` `internal` naming the branch (`the sync of
  workspace "<slug>" stopped: refresh the patch branch "<branch>" from
  origin: it or its backup ref moved in the hub between the comparison and
  the swap; sync again: …` for a lost swap). The outcomes of the patches
  before it, their events and a backup ref written stay; `upstream_head_sha`,
  `last_sync_at` and `sync_error` are not written. When the refresh, merge
  detection or the clearing of the origin fields fails after the branch of
  an `active` or `conflict` patch was moved or a patch was marked
  `merged_upstream`, and `auto_rebuild_after_sync` is on, a rebuild is
  queued before the `500`, since the next sync does not see those changes
  again.
- When upstream advanced or the refresh created or moved a patch branch,
  merge detection examines the `active` and `conflict` patches whose branch
  the hub has, in position order. `squash_merge_detection` selects the
  tests: by ancestry (`ancestry_only`, `both`), the patch's head is the
  upstream base or its ancestor; by content (`content_based`, `both`),
  `git cherry <base> <head>` lists at least one commit and every one has an
  equivalent upstream; by pull request number (`content_based`, `both`),
  `upstream_pr_url` holds `/pull/<n>`, the digits ending the URL or
  followed by `/`, `?` or `#`, and a new upstream commit's subject ends
  with `(#<n>)`, `<n>` without leading zeros. The new commits are those of the base not reachable
  from the stored `upstream_head_sha`, or the 50 newest of the base on the
  first sync and after a force-push. A merged patch becomes
  `merged_upstream` with `conflict_files` `[]` and its origin fields
  `null`, and `parley.patch.update` is emitted (metadata `branch_name`,
  `changed` `["status"]`, `from`, `to` `merged_upstream`, `reason`
  `merged_upstream`).
- A rebuild is queued, with the workspace's `rebuild_strategy` and
  `rebuild_fail_mode` and the caller as `submitted_by`, when
  `auto_rebuild_after_sync` is on and upstream advanced, the refresh created
  or moved the branch of an `active` or `conflict` patch, or a patch was
  found merged; the check `no_active_patches` of the rebuild route does not
  apply. A rebuild already `queued`, `running` or `failed` skips it; any
  other failure of the enqueue is `500` `internal` (`… queue the rebuild of
  the integration branch: …`) and the completion is not written.
- The completion writes `upstream_head_sha`, the upstream base, when it
  differs from the one stored, beside `last_sync_at`.

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

The carry-patch part adds eight members to the answer of a `carry_patch`
workspace (a `standard` workspace's answer has none of them):

| Member | Value |
|---|---|
| `upstream_advanced` | `true` when the upstream base differs from the stored `upstream_head_sha`, or none was stored |
| `force_push_detected` | `true` when upstream advanced and the stored head is not an ancestor of the new base or is missing from the hub's repository; `false` on the first sync |
| `origin_fetched` | always `true` |
| `patches_merged` | the branches found merged upstream in this sync, in position order |
| `patches_synced` | one object per refreshed patch, in position order: `branch_name`, `action`, `state`, `local_sha` (the hub's head after the refresh, `null` when it has no such branch), `origin_sha` (`null` when origin lacks the branch), `replaced_sha` (the saved head for `replaced`, else `null`); `[]` under `patch_branch_source` `hub` |
| `patches_diverged` | the branches of the entries whose `state` is `diverged`, in position order |
| `rebuild_triggered` | `true` when this sync queued a rebuild |
| `rebuild_job_id` | the queued job's id, else `null` |

```json
{"slug":"variant","…":"…","branches":[],"force_push_detected":false,"origin_fetched":true,"patches_diverged":["patch/b"],"patches_merged":[],"patches_synced":[{"branch_name":"patch/a","action":"fast_forwarded","state":"in_sync","local_sha":"a3f2b1c4d5e6f708192a3b4c5d6e7f8091a2b3c4","origin_sha":"a3f2b1c4d5e6f708192a3b4c5d6e7f8091a2b3c4","replaced_sha":null},{"branch_name":"patch/b","action":"none","state":"diverged","local_sha":"9fceb02d0ae598e95dc970b74767f19372d61af8","origin_sha":"e83c5163316f89bfbde7d9ab23ca2e25604af290","replaced_sha":null}],"rebuild_job_id":"7b1e0000-0000-4000-8000-000000000001","rebuild_triggered":true,"upstream_advanced":false}
```

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
records and the other data the workspace owns stay. When the clone work
leaves the workspace `ready`, in the same store transaction, every draft
and open change whose source branch the new copy does not have is
abandoned with `reason` `source_lost` by the hub (`actor_type` `system`),
with its `parley.change.abandon` event. `head_sha`,
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
for it (its secrets and [variables](#variables) among them) is removed, its remaining agent sessions
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
features keep for it (its secrets and [variables](#variables) among them) is removed, its remaining
agent sessions are ended with the reason `workspace deleted`, its record
is removed and one `parley.workspace.delete` event is written. When
anything in that transaction fails, nothing has changed: the fork still
answers `GET` with its `expires_at`, its tokens still authenticate, its
secrets, its variables and its own branches are still there. Once it has committed the
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
| `session_id` | a UUID in its hyphenated form, the session the fork is created in, judged by the session-linking rule for the caller and the parent; the fork stores it, and its event carries it, as the rule gives it back, in lower case, and the link of the fork to the session is recorded in the fork's transaction. The rule is that of [Sessions](#sessions): the session exists, the caller created it (or is the admin token), and it is on the parent or on a fork of it |
| `sandbox` | an object, the sandbox to create with the fork, handed as received, unread, to the sandbox component with the fork's `session_id` and `purpose` as defaults; the component judges it as the body of [`POST /api/v1/workspaces/{slug}/sandboxes`](#post-apiv1workspacesslugsandboxes) on the new fork (a PAT's `sandboxes:write` first, then the body, the session, the secrets and the quotas; a `purpose` taken from the fork is cut to 200 characters) and answers the sandbox object, stored in the fork's transaction with its hub credential and its `parley.sandbox.create` event, or its refusal, on which no fork is created. The sandbox is provisioned once the fork's transaction has committed, within about a second. On a hub without sandboxes every `sandbox` is refused with `404` `sandboxes_disabled` and no fork is created |

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
| `400` `unknown_session` | the body has a `session_id` the session-linking rule refuses (`session_id: no session "<id>"`) |
| `400` `not_supported_on_fork` | the workspace is itself a task fork (`workspace "demo--task1" is a task fork, which cannot be forked; fork its parent instead`) |
| `400` `fork_ref_not_found` | the base branch is not a branch of the parent: a tag, a commit id or a full ref name is not one (`ref: "nope" is not a branch of "demo"`) |
| `403` `insufficient_scope` | a PAT without `workspaces:write`; a PAT without `tokens:manage` whose body has `token` (`token: minting a token needs the scope tokens:manage`); or a token beyond what the calling PAT holds, with the subset rule's messages |
| `403` `forbidden` | the admin token, whatever the body (`a fork needs a user as owner; the admin token cannot create one`) |
| `404` `not_found` | the caller cannot read the workspace, or no workspace has the slug (`workspace not found`) |
| `404` `sandboxes_disabled` | the body has a `sandbox` and the hub has no `[sandbox]` section, so no sandbox component is part of it (`sandbox: no sandbox component is available on this hub to create one`); no fork is created |
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

## Carry-patch workspaces

A workspace whose `workspace_mode` is `carry_patch` keeps a variant of an
upstream project it does not control: the hub tracks both origin
(`git_url`) and the upstream (`upstream_url`), and keeps an ordered list of
**patches**, branches of the workspace that the hub rebuilds on top of the
upstream into the **integration branch**.

### The set-up after a clone

After every fresh clone of a `carry_patch` workspace (the first clone, a
reclone, a reactivation that cannot keep its copy) and before the workspace
becomes `ready`, the hub sets it up in four steps, each attempted whatever
the earlier ones did:

1. when the record has no `integration_branch`, it stores `deploy`, so the
   workspace object shows `"integration_branch":"deploy"` from then on
   (`updated_at` does not change);
2. it fetches the upstream: every branch of `upstream_url` into
   `refs/remotes/upstream/<branch>` of the hub's copy, removing those the
   upstream no longer has, and no tag; the upstream's default branch is
   remembered in the copy's configuration (`parley.upstreamDefaultBranch`);
3. it turns on remembered resolutions (git's `rerere.enabled`) in the copy;
4. it writes `refs/heads/<integration_branch>` at the **upstream base**,
   the head of the upstream's default branch (else of the upstream branch
   named like the workspace's `default_branch`), or at the workspace's
   `head_sha` (the head of origin's default branch) when the fetch failed
   or no base resolves. A branch of that name the clone brought from origin
   is replaced; `refs/remotes/origin/<name>` stays as origin has it.

A failed step is logged (`workspace post-clone step failed`, with the slug
and the failed steps, scrubbed of credentials) and never keeps the
workspace from `ready`: `clone_error` stays `null`. The set-up writes no
`upstream_head_sha` and emits no event. A copy that clone work keeps
instead of cloning is not set up again, and a `standard` workspace never.

The upstream credential is read from the workspace's
[secrets](#secrets) at every use, the first that applies:
`UPSTREAM_GIT_PAT` when not empty (user name `x-token-auth`, the token as
password); `UPSTREAM_GIT_USERNAME` with `UPSTREAM_GIT_PASSWORD` when both
are not empty; the workspace's origin credential (`GIT_PAT`, else
`GIT_USERNAME` with `GIT_PASSWORD`); none. A rotated secret counts at the
next use. A secret that cannot be read fails the fetch without contacting
the upstream; there is no anonymous attempt. One upstream fetch, the listing
of the upstream's branches included, may take 10 minutes in all; past that
its git process is stopped and the fetch has failed.

### Pushes to the integration branch and patch branches

Two kinds of branch of a `carry_patch` workspace are not ordinary
branches: the integration branch, `refs/heads/<integration_branch>`
(`deploy` while the record holds none), which only the hub writes, and the
**patch branches**, `refs/heads/<branch_name>` of every patch that is not
`deleted`. A client write of a ref that arrives as a [git
push](#git-over-http) or through the source control API is decided by the
fourth push rule, after the hub-owned refs (`refs/parley/`, the backups of
replaced patch tips included), the protected branches, the default branch
and a fork's tracking branches; a [land](#landing) onto a patch branch is
judged by the same table before its target moves (below), and no change
can target the integration branch. A task fork is a `standard` workspace and is never
asked. The settings `patch_branch_source` and `push_patches_to_origin`
are read at every decision, so a change counts at the next push.

| Ref | `patch_branch_source` | `push_patches_to_origin` | Update | Answer |
|---|---|---|---|---|
| the integration branch | any | any | create, update, delete, by any caller, the admin token included | refused, `integration_branch`: `integration branch <name> is maintained by the hub; push a patch branch and rebuild instead` |
| a patch branch | `hub` | any | create, update, forced update, delete | allowed; a deleted branch's patch stays registered |
| a patch branch | `origin` | `false` | create, update, delete | refused, `patch_branch`: `branch is synced from origin; push to <git_url> instead` |
| a patch branch | `origin` | `true` | delete | refused, `patch_branch`: `branch is synced from origin; delete it on origin instead` |
| a patch branch | `origin` | `true` | create, update | allowed once forwarded to origin (below) |
| any other ref: a tag or note named like the integration branch, a deleted patch's branch, any other branch | any | any | any | allowed |

`<git_url>` is the workspace's origin URL without user information; a
`git@host:path` origin is shown as stored. A patch branch listed in
`protected_branches` is refused by the protected-branch rule first; the
owner's direct fast-forward passes that rule and then meets this table.
Over git a refusal fails the whole push; over the source control API it is
`403` with the error type. When the workspace record, its settings or the
patch list cannot be read the update is refused with `internal error; try
again` (`500` `internal` through the API), never allowed.

**Forwarding.** Deciding never contacts origin. Once every ref of a
request has passed every rule, each update to forward is pushed, in
command order, to `refs/heads/<branch>` on origin as a push without force,
with the workspace's origin credential read at that moment, within
120 seconds. Origin takes it: the hub's branch moves and, once the
request is accepted and while the branch is still at that commit,
`refs/remotes/origin/<branch>` of the hub's copy records it. Otherwise the update is refused, `patch_branch`, with
`origin rejected push: <reason>`: origin's own message for an update its
rules or hooks refused, `non-fast-forward`, `authentication failed`,
`permission denied`, `repository not found`, `origin unreachable`,
`timed out` or `push failed`, scrubbed of credentials and URL user
information and cut to 1024 bytes; or with `failed to resolve origin
credentials` when the credential cannot be read, in which case origin is
not contacted. A refused update fails the whole push; a branch of the same
push already forwarded stays on origin and returns through the next sync.

**Landing.** A land onto a patch branch is judged by the same table before
the target moves (see [Landing](#landing)): a refusal ends the attempt
`blocked` with the refusal as the reason of `parley.change.land_fail`; with
forwarding, the landed head is forwarded first, and a forward that fails
ends the attempt `failed` with `error` `origin_rejected`; a decision that
cannot be made ends it `failed` with `error` `internal`. In each case the
target does not move.

**After an accepted write.** After every accepted git push, source
control API write or land that created or moved a patch branch (a delete
counts for nothing, nor does the branch of a deleted patch), the hub,
once the client has its answer and one request of a workspace at a time:

1. queues one rebuild for the request when `auto_rebuild_after_push` is
   `true` (its default), with `submitted_by` `system:push` (a push or an
   API write) or `system:land`, actor `system` and the request's session;
   a rebuild already queued, running or waiting to retry is left as it is,
   silently;
2. mirrors, when `patch_branch_source` is `hub` and
   `push_patches_to_origin` is `true`, whatever `auto_rebuild_after_push`
   says, each patch branch the request created or moved, in the order of
   the updates: the head the branch has when the mirror starts is pushed
   to origin with force-with-lease against `refs/remotes/origin/<branch>`
   (or against origin having no such branch), within 120 seconds, and on
   success recorded there. A branch that fails emits
   `parley.patch.mirror_failed` and the mirror goes on with the next.

Neither step changes the answer the client already has. The hub's stop
ends them; a mirror cut off by it emits nothing. Forwarding, the mirror and
a rebuild's push of the integration branch are the only ways these
branches reach origin.

`parley.patch.mirror_failed` has `resource_type` `patch`, the patch's id as
`resource_id`, actor `system` and `metadata` `branch_name` and `error`: the
reason alone, without `origin rejected push: `, one of the forward's
reasons, `origin moved` for a stale lease, `timed out` or `failed to
resolve origin credentials`.

```json
{"seq":52,"id":"…","type":"parley.patch.mirror_failed","time":"2026-11-17T10:30:00Z","workspace":"variant","actor_type":"system","actor_id":null,"resource_type":"patch","resource_id":"5c0e0000-0000-4000-8000-000000000001","action":"mirror_failed","session_id":null,"metadata":{"branch_name":"patch/tls","error":"origin moved"}}
```

### The patch object

Every patch route answers patches as this object, every member present
(`null` when unset):

| Member | Value |
|---|---|
| `id` | a UUID |
| `workspace_slug` | the workspace's slug |
| `branch_name` | the patch's branch, without `refs/heads/` |
| `position` | its place in the order, `1` to N without gaps among the patches that are not deleted; `null` for a deleted patch |
| `status` | `active`, `conflict`, `disabled`, `merged_upstream` or `deleted` |
| `conflict_files` | an array of paths, `[]` unless `status` is `conflict` |
| `upstream_pr_url`, `description` | strings, or `null` |
| `added_at`, `updated_at` | timestamps (see [Timestamps and durations](#timestamps-and-durations)) |
| `deleted_at` | a timestamp, or `null` |
| `origin_sync_state`, `origin_sha`, `origin_synced_at` | `null` for a new patch |

A branch is the branch of at most one patch of a workspace that is not
deleted. A workspace's patches are removed with it by its delete.

### Common rules

Every route of this section is under `/api/v1/workspaces/{slug}/patches`.
The reads need the scope `patches:read` from a PAT (`patches:write` also
satisfies it) and read access to the workspace; the writes (the add, the
update, the delete, the restore, the reorder and the reset to origin) need
`patches:write` and write access (see [Workspace access](#workspace-access)).
After its own validation of the body, each route refuses a workspace whose
`workspace_mode` is not `carry_patch`, every task fork included, with
`400` `workspace_mode_mismatch`
(`workspace "<slug>" is in mode standard; patches need a workspace in mode carry_patch`),
before it looks at the workspace's status. The reads answer in every
status of the workspace, from the stored list; a write then needs the
workspace to be `ready` with no archive, reclone or delete running
(`409` `workspace_busy`, `workspace_not_ready` or `workspace_archived`, as
for a [sync](#post-apiv1workspacesslugsync)). Only then does a route that
names a patch look it up: an `{id}` that is not a UUID, names no patch or a
patch of another workspace is `404` `not_found` (`patch not found`). A
refused write changes no patch, moves no ref, contacts no remote and emits
no event.

Every write that changes positions (an update with a `position`, a delete,
a restore, a reorder) reads and rewrites the order in one store
transaction, and the store runs one write transaction at a time: two such
requests that arrive together run one after the other, neither fails
because of the other, and the patches that are not deleted hold the
positions `1` to N without a gap or a duplicate afterwards. A patch whose
position a write changes gets a new `updated_at`.

## `POST /api/v1/workspaces/{slug}/patches`

Registers one patch, or a batch of them all or nothing. It accepts an
`Idempotency-Key` (see [Idempotency](#idempotency)). The body is one patch
object or a non-empty array of them:

| Member | Required | Rule |
|---|---|---|
| `branch_name` | yes | a string, a valid branch name, not the workspace's integration branch (its `integration_branch`, `deploy` while that is `null`) |
| `position` | no | a JSON integer of at least 1; absent, `null` or greater than N+1 appends the patch |
| `upstream_pr_url`, `description` | no | strings; absent, `null` or empty is stored as `null` |
| `skip_branch_check` | no | a boolean; `true` registers the branch without looking it up |
| `if_not_exists` | no | a boolean; `true` answers an already registered branch with its patch; refused in a batch |

`null` counts as absent and unknown members are ignored. The body is
validated before anything is looked up; in a batch every element is, in
order, and the message of a refusal starts with `patch[<i>]: `, `<i>` the
zero-based index of the element (the later one of two that name the same
branch).

Then, after the [common rules](#common-rules):

1. **Already registered.** A branch that is the branch of a patch that is
   not deleted is `409` `patch_exists`; with `if_not_exists: true` the
   answer is `200` with that patch, unchanged. In a batch every element is
   checked before any branch is looked up.
2. **Branch resolution**, unless `skip_branch_check` is `true`
   (`skipped`), in array order: `refs/heads/<branch>` of the hub's copy is
   taken as it is (`local`); else the hub creates the branch at
   `refs/remotes/origin/<branch>`, origin's head as of the last fetch
   (`origin_tracking`); else, when the workspace setting
   `patch_branch_source` is `origin`, the hub fetches that one branch from
   origin with the origin credential, within 2 minutes, records it as
   `refs/remotes/origin/<branch>` and creates the branch at its tip
   (`origin_fetch`). A branch found nowhere is `404` `branch_not_found`;
   origin unreachable, refusing, its credential unreadable or the limit
   passed is `502` `origin_fetch_failed`. A branch created by resolution
   stays even when the request then fails.
3. **Storing**, in one transaction with the events: a deleted patch of the
   same branch is removed for good and its backup ref
   `refs/parley/replaced/<branch>` deleted after the commit (a failure to
   delete it is logged, `delete the backup ref of a removed patch`, and
   does not change the answer). With N patches that are not deleted, a
   patch with a `position` of at most N+1 takes it and the patches at and
   after it move down by one; the others are appended. In a batch the
   elements with a `position` are placed first, in array order, each in the
   list as it then stands, then the others are appended in array order.
   Each new patch is `active` with `conflict_files` `[]`, and `added_at`
   equal to `updated_at`.

Concurrent adds on a workspace never fail because of each other: of two
adds of one branch, one creates the patch and the other is answered as
already registered, and the positions stay 1 to N without gaps.

| Status | When |
|---|---|
| `201 Created` | the patch was registered; the body is the patch object. A batch answers `{"items":[…]}`, the patches in request order, each at its final position |
| `200 OK` | `if_not_exists: true` for a registered branch (the existing patch), or a replay of a recorded `Idempotency-Key` (the first answer) |
| `400` `invalid_request` | the body is not a patch object or an array of them (`request body must be a patch object or an array of them`), an empty array (`a batch must hold at least one patch object`), an element that is not an object (`patch[<i>]: a patch must be a JSON object`), `branch_name is required`, `branch_name "<b>" is not a valid branch name: <rule>`, `branch_name "<b>" is the workspace's integration branch`, `position must be an integer of at least 1`, `<member> must be a string`, `<member> must be a boolean`, `patch[<i>]: if_not_exists cannot be true in a batch`, `patch[<i>]: branch_name "<b>" is named by patch[<j>] already` |
| `400` `workspace_mode_mismatch` | the workspace is not in mode `carry_patch` |
| `403` `insufficient_scope` | a PAT without `patches:write` |
| `403` `forbidden` | a caller with read access only |
| `404` `not_found` | the caller cannot read the workspace, or no workspace has the slug |
| `404` `branch_not_found` | the branch is found by none of the lookups that apply (`branch "<b>" not found`) |
| `409` `patch_exists` | the branch is the branch of a patch that is not deleted (`branch "<b>" is a patch of the workspace already`) |
| `409` `conflict` | another branch of the copy is in the way of the branch's name, such as `feat` for `feat/x` (`the branch "feat/x" cannot be created in the hub's copy: refs/heads/feat is in the way of its name`) |
| `409` `workspace_busy`, `workspace_not_ready`, `workspace_archived` | the workspace's status does not allow the write |
| `500` `internal` | storing failed; no patch of the request and no event remain |
| `502` `origin_fetch_failed` | the single-branch fetch from origin failed (`fetch the branch "<b>" from origin: <cause>`, with no credential and URL user information as `***@`) |

Each created patch emits `parley.patch.create` with `resource_type`
`patch`, the patch's id as `resource_id`, the caller as actor, and in
`metadata` `branch_name`, `position` (the final one) and
`branch_resolution` (`local`, `origin_tracking`, `origin_fetch` or
`skipped`):

```json
{"seq":31,"id":"…","type":"parley.patch.create","time":"2026-11-16T10:30:15Z","workspace":"variant","actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","resource_type":"patch","resource_id":"5c0e0000-0000-4000-8000-000000000001","action":"create","session_id":null,"metadata":{"branch_name":"fix-dial","branch_resolution":"origin_tracking","position":1}}
```

## `GET /api/v1/workspaces/{slug}/patches`

Lists the patches that are not deleted in position order. With
`include_deleted=true` the deleted patches follow, newest `deleted_at`
first and by `id` among equal ones. It is a list (see [Lists](#lists)), with
`limit` and `cursor`; the cursor continues across the boundary between the
two groups.

| Status | When |
|---|---|
| `200 OK` | a page, `items` an array of patch objects (`[]` when there are none) |
| `400` `invalid_request` | `include_deleted` other than exactly `true` or `false`, empty or given more than once included (`include_deleted must be given once, as true or false`); a bad `limit` or `cursor` |
| `400` `workspace_mode_mismatch` | the workspace is not in mode `carry_patch` |
| `403` `insufficient_scope` | a PAT with neither `patches:read` nor `patches:write` |
| `404` `not_found` | the caller cannot read the workspace, or no workspace has the slug |

## `GET /api/v1/workspaces/{slug}/patches/{id}`

Answers one patch of the workspace, deleted or not. When the hub's copy
holds the backup ref `refs/parley/replaced/<branch_name>`, the patch object
has one more member, `replaced_sha`, the full commit id the ref points at;
otherwise, and when the copy is not on disk, the member is absent.

| Status | When |
|---|---|
| `200 OK` | the patch object |
| `400` `workspace_mode_mismatch` | the workspace is not in mode `carry_patch` |
| `403` `insufficient_scope` | a PAT with neither `patches:read` nor `patches:write` |
| `404` `not_found` | the caller cannot read the workspace (`workspace not found`); or `{id}` is not a UUID, names no patch, or a patch of another workspace (`patch not found`) |

## `PATCH /api/v1/workspaces/{slug}/patches/{id}`

Changes a patch's status, position, description and upstream pull request
URL. The body is a JSON object; each member is optional:

| Member | Rule |
|---|---|
| `status` | `active`, `disabled`, `merged_upstream` or `conflict` |
| `position` | a JSON integer from 1 to N, the number of patches that are not deleted |
| `description`, `upstream_pr_url` | strings; `""` clears the field to `null` |

A member that is absent or `null` leaves its field as it is; `branch_name`
and unknown members are ignored. A value equal to the stored one is not a
change: when nothing changes, the empty object included, nothing is
written, `updated_at` stays and no event is emitted. A status other than
`conflict` clears `conflict_files` to `[]`; setting `conflict` leaves it as
it is. A new position takes the patch out of the order and puts it back at
that place, the others keeping their relative order. No update queues a
rebuild.

The shape of the body is checked before the [common rules](#common-rules);
the patch lookup, the deleted check and the upper bound of `position` come
after them, so an unknown `status` on a deleted patch is `400` and a
`position` of 99 on one is `409` `patch_deleted`.

| Status | When |
|---|---|
| `200 OK` | the patch object as it now stands (with `replaced_sha` as for the [get](#get-apiv1workspacesslugpatchesid)) |
| `400` `invalid_request` | the body is not a JSON object (`request body must be a JSON object`, `request body is not valid JSON`); a member of the wrong JSON type (`status must be a string`, `description must be a string`, `upstream_pr_url must be a string`, `position must be an integer of at least 1`); `status "<s>" is not one of [active disabled merged_upstream conflict]`, `deleted` included; `position must be between 1 and <N>, the number of patches` |
| `400` `workspace_mode_mismatch` | the workspace is not in mode `carry_patch` |
| `403` `insufficient_scope` | a PAT without `patches:write` |
| `403` `forbidden` | a caller with read access only |
| `404` `not_found` | the caller cannot read the workspace, no workspace has the slug, or `patch not found` |
| `409` `patch_deleted` | the patch is deleted, whatever the body (`the patch of <branch> is deleted; restore it before changing it`) |
| `409` `workspace_busy`, `workspace_not_ready`, `workspace_archived` | the workspace's status does not allow the write |
| `500` `internal` | storing failed; nothing changed and no event remains |

An update that changed something emits `parley.patch.update` with
`resource_type` `patch`, the patch's id as `resource_id`, the caller as
actor, and in `metadata` `branch_name` and `changed`, the names of the
fields that changed in the order `status`, `position`, `description`,
`upstream_pr_url`; a status change adds `from`, `to` and `reason: "api"`:

```json
{"type":"parley.patch.update","resource_type":"patch","resource_id":"5c0e0000-0000-4000-8000-000000000002","metadata":{"branch_name":"patch/b","changed":["status"],"from":"active","to":"disabled","reason":"api"}}
```

## `DELETE /api/v1/workspaces/{slug}/patches/{id}`

Soft-deletes a patch: in one transaction it becomes `deleted`, with
`deleted_at` and `updated_at` set to now, `position` `null` and
`conflict_files` `[]`, and the patches that are not deleted are renumbered
`1` to N-1 in their order. Its row stays, so the get route and the list
with `include_deleted=true` still show it; the patch branch and its backup
ref `refs/parley/replaced/<branch>` are not touched. A request body is
ignored. Deleting a deleted patch answers `204` and changes nothing:
`deleted_at` keeps its value and no event is emitted. No delete queues a
rebuild.

A patch that has stayed `deleted` for more than 7 days (its `deleted_at`
more than 7 days before the pass; one deleted exactly 7 days ago stays) is
removed by the maintenance task `patch_purge`, which runs in the first
maintenance pass after the hub starts and in every hourly pass, for the
workspaces in every status. With the patch it deletes the backup ref
`refs/parley/replaced/<branch>` in the copy, unless a patch of the
workspace that is not deleted uses the same branch; the patch branch
stays. Afterwards the get route answers `404` for the patch and the list
with `include_deleted=true` no longer shows it. A restore that comes first
wins: the patch stays, with its backup ref. A patch whose removal fails is
logged with the workspace's slug and the patch id, stays `deleted` and is
tried again in the next pass; the purge writes no event.

| Status | When |
|---|---|
| `204 No Content` | the patch is deleted; the body is empty |
| `400` `workspace_mode_mismatch` | the workspace is not in mode `carry_patch` |
| `403` `insufficient_scope` | a PAT without `patches:write` |
| `403` `forbidden` | a caller with read access only |
| `404` `not_found` | the caller cannot read the workspace, no workspace has the slug, or `patch not found` |
| `409` `workspace_busy`, `workspace_not_ready`, `workspace_archived` | the workspace's status does not allow the write |
| `500` `internal` | storing failed; the patch and the order are as they were |

A delete that deleted emits `parley.patch.delete`, with the patch as the
resource, the caller as actor and `metadata`
`{"branch_name":"<branch>","reason":"api"}`.

## `POST /api/v1/workspaces/{slug}/patches/{id}/restore`

Brings a deleted patch back, as the last of the order: in one transaction
it becomes `active` at position N+1 (N the patches that are not deleted),
with `deleted_at` `null`, `conflict_files` `[]` and `updated_at` now. It
restores a patch a caller deleted and one a rebuild removed as merged
upstream alike. A request body is ignored.

| Status | When |
|---|---|
| `200 OK` | the restored patch object |
| `400` `workspace_mode_mismatch` | the workspace is not in mode `carry_patch` |
| `403` `insufficient_scope` | a PAT without `patches:write` |
| `403` `forbidden` | a caller with read access only |
| `404` `not_found` | the caller cannot read the workspace, no workspace has the slug, or `patch not found` |
| `409` `patch_not_deleted` | the patch is `active`, `disabled`, `conflict` or `merged_upstream` (`the patch of <branch> is <status>, not deleted`) |
| `409` `patch_exists` | another patch of the workspace that is not deleted has the branch (`branch "<b>" is a patch of the workspace already`) |
| `409` `workspace_busy`, `workspace_not_ready`, `workspace_archived` | the workspace's status does not allow the write |
| `500` `internal` | storing failed; the patch stays deleted |

A restore emits `parley.patch.restore`, with the patch as the resource, the
caller as actor and `metadata` `{"branch_name":"<branch>","position":<n>}`,
the position it took.

## `POST /api/v1/workspaces/{slug}/patches/reorder`

Sets the whole order in one step. The body is
`{"patch_ids":["<id>",…]}`, which must name every patch of the workspace
that is not deleted exactly once. In one transaction the patches take the
positions `1` to N in the order of `patch_ids`; only those whose position
changed get a new `updated_at`. Asking for the current order succeeds and
emits its event too. A workspace with no patch that is not deleted accepts
`{"patch_ids":[]}` and answers `200` with no items and no event.

The shape of the body is checked before the [common rules](#common-rules);
the list is checked against the patches after them, the first failure
answering: an id given twice, an id of no patch of the workspace, the id of
a deleted patch, a number of ids other than N. Ids are compared in their
canonical UUID form. Nothing is changed by a refusal.

| Status | When |
|---|---|
| `200 OK` | `{"items":[…],"next_cursor":null}`, every patch that is not deleted in the new order |
| `400` `invalid_request` | the body is not a JSON object, or `patch_ids` is absent or not an array of strings (`patch_ids must be an array of strings`); `patch_ids names <id> more than once`; `patch_ids names <id>, which is no patch of <slug>`; `patch_ids names <id>, a deleted patch; restore it before ordering it`; `patch_ids names <given> patches; <slug> has <N> that are not deleted, and each must be named once` |
| `400` `workspace_mode_mismatch` | the workspace is not in mode `carry_patch` |
| `403` `insufficient_scope` | a PAT without `patches:write` |
| `403` `forbidden` | a caller with read access only |
| `404` `not_found` | the caller cannot read the workspace, or no workspace has the slug |
| `409` `workspace_busy`, `workspace_not_ready`, `workspace_archived` | the workspace's status does not allow the write |
| `500` `internal` | storing failed; the order is as it was |

A reorder emits one `parley.patch.reorder`, whose `resource_id` is the
first patch of the new order, with the caller as actor and `metadata`
`{"patch_ids":[…]}`, the new order. No reorder queues a rebuild.

## `POST /api/v1/workspaces/{slug}/patches/{id}/reset-to-origin`

Forces the hub's copy of a patch branch to origin's current tip, whatever
the settings `patch_branch_source` and `patch_divergence_policy` say. A
request body is ignored. Only an `active`, `conflict` or `disabled` patch is
reset; for another status origin is not contacted.

The hub fetches that one branch from origin (`git_url`) into
`refs/remotes/origin/<branch>`, with the workspace's origin credential read
at the time of the call, within 2 minutes; then it takes the first action
that applies:

| Situation | What the hub does | `action` |
|---|---|---|
| the hub has no such branch | creates it at origin's tip | `created` |
| the hub's tip equals origin's | writes nothing | `none` |
| the hub's tip is an ancestor of origin's | moves the branch forward | `fast_forwarded` |
| otherwise (diverged, or the hub ahead) | saves the hub's tip to `refs/parley/replaced/<branch>`, overwriting an earlier backup, then moves the branch | `replaced` |

The hub records the outcome and moves the refs in one store transaction,
in which it reads the patch again: a patch deleted or marked
`merged_upstream` while origin was fetched is `409`
`patch_status_invalid`, and nothing is moved. Every move is a
compare-and-swap against the tip the hub compared (against "no such ref"
for `created`); a branch another writer moved, created or deleted in
between is `409` `ref_moved`, and is left as that writer has it. A backup
ref already written stays; nothing else changes.

When the reset succeeded and `patch_branch_source` is `origin`, the patch's
`origin_sync_state` becomes `in_sync`, `origin_sha` origin's tip and
`origin_synced_at` now; with `hub` the three fields are left alone. A reset
does not change `updated_at`. When the branch moved, the patch is `active`
or `conflict` and `auto_rebuild_after_sync` (read at that moment) is
`true`, the hub queues a rebuild of the workspace with the caller as its
submitter; a rebuild already waiting, or an enqueue that fails (logged as
`queue the rebuild after a reset to origin`), leaves the reset in place and
queues nothing.

| Status | When |
|---|---|
| `200 OK` | the patch object with three more members: `action`; `rebuild_triggered`, `true` only when this call queued a rebuild; `rebuild_job_id`, present only then; and `replaced_sha` whenever `refs/parley/replaced/<branch>` exists after the call (also one an earlier reset left) |
| `400` `workspace_mode_mismatch` | the workspace is not in mode `carry_patch` |
| `403` `insufficient_scope` | a PAT without `patches:write` |
| `403` `forbidden` | a caller with read access only |
| `404` `not_found` | the caller cannot read the workspace, no workspace has the slug, or `patch not found` |
| `409` `patch_status_invalid` | the patch is `merged_upstream` or `deleted` (`the patch of <branch> is <status>; only an active, conflict or disabled patch is reset to origin`) |
| `409` `missing_on_origin` | origin has no such branch (`origin has no branch "<b>"`) |
| `409` `ref_moved` | the branch moved in between (`the branch "<b>" moved while it was reset to origin; read it again and retry`); `details.current_sha` is where it is now, `null` when it is gone |
| `409` `conflict` | another ref of the copy is in the way of `refs/remotes/origin/<branch>` or of `refs/heads/<branch>` |
| `409` `workspace_busy`, `workspace_not_ready`, `workspace_archived` | the workspace's status does not allow the write |
| `500` `internal` | recording the outcome failed; the branch, the backup ref and the patch are as they were |
| `502` `origin_fetch_failed` | the workspace has no `git_url`, the origin credential cannot be read, origin cannot be reached or refuses it, or the fetch passed its 2 minutes (`fetch the branch "<b>" from origin: <cause>`, with no credential and URL user information as `***@`) |

```json
{"id":"5c0e0000-0000-4000-8000-000000000001","workspace_slug":"variant","branch_name":"patch/a","position":1,"status":"active","conflict_files":[],"upstream_pr_url":null,"description":null,"added_at":"2026-11-16T10:30:15Z","updated_at":"2026-11-16T10:30:15Z","deleted_at":null,"origin_sync_state":null,"origin_sha":null,"origin_synced_at":null,"replaced_sha":"0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e","action":"replaced","rebuild_triggered":false}
```

A successful reset emits `parley.patch.reset`, whatever the action, with
the patch as the resource, the caller as actor and `metadata`
`branch_name`, `action`, `local_sha` (the hub's tip before the call,
`null` for `created`), `origin_sha` and `replaced_sha` (the tip this call
saved, `null` unless the action is `replaced`). A `replaced` reset also
emits `parley.patch.replace` with `metadata` `branch_name`,
`replaced_sha`, `origin_sha` and `trigger: "reset_to_origin"`. A refused
reset emits nothing and changes no patch field.

## Rebuild jobs

A rebuild recreates the integration branch of a `carry_patch` workspace
from its upstream and its patches. The hub runs it as a **durable job**:
the job is stored before the request that asked for it is answered,
survives a restart of the hub and is retried with growing delays. The
hub runs the jobs itself, in `parleyd`; there is no worker process.

A job is in one of six statuses:

| Status | Meaning | Final |
|---|---|---|
| `queued` | waiting for a worker | no |
| `running` | a worker runs it | no |
| `failed` | the last attempt failed with an error that may pass; the job waits to retry at `next_attempt_at` | no |
| `completed` | succeeded | yes |
| `dead_letter` | failed for good: a permanent error, or its retries are spent; can be requeued | yes, until requeued |
| `cancelled` | cancelled while queued | yes |

`failed` is never final. A workspace has at most one rebuild job that is
`queued`, `running` or `failed`; the one exception is the follow-up a
running rebuild queues for itself, which starts after it. At most 4 jobs
run at once across the hub, and one per workspace; queued jobs start
oldest first, within 5 seconds when a worker is free.

An attempt that fails with an error that may pass is retried: after the
*n*-th such failure the job is `failed` with `retry_count` *n* and
`next_attempt_at` `min(2 s × 2ⁿ, 2 h)` later (4 s, 8 s, 16 s, … 4096 s
after the 11th, 2 h from the 12th on). The 21st such failure, and any
permanent failure, makes the job `dead_letter`, with `retry_count`
unchanged. A job the hub finds `running` when it starts (the hub stopped
during the attempt) returns to `queued` with `retry_count` unchanged and
runs again from the start. At a stop the hub starts no further job and
gives running attempts 30 seconds; one still running then is left
`running` and recovered at the next start. A workspace's jobs are deleted
with the workspace.

### What an attempt does

An attempt recomputes the integration branch as "the upstream base plus
every patch that still applies, in position order", out of sight of every
client, and moves the branch once at the end.

1. **The workspace.** `archived` ends the job for good (reason
   `workspace_archived`), `failed` too (`workspace_not_ready`); `pending` or
   `cloning` fails the attempt for a retry (`workspace_busy`). A workspace
   that was deleted took its jobs with it: the attempt ends with nothing
   written and no event. The same test is made again before every patch
   and inside the publish, right before the branch moves, so an archive, a
   reclone or a delete during a run ends it at its next step without
   moving the branch. A rebuild never blocks them.
2. **Upstream.** `patch_results` starts as `[]`. The hub fetches every
   upstream branch into `refs/remotes/upstream/*` (no tags) with the
   upstream credential (`UPSTREAM_GIT_PAT`, else `UPSTREAM_GIT_USERNAME`
   with `UPSTREAM_GIT_PASSWORD`, else the origin credential) and repeats a
   missing set-up step, except the integration branch, which only the
   publish writes: an attempt that ends before the publish leaves it as it
   was, missing included. The **upstream base** is the head of the
   upstream's default branch, else of the upstream branch named like the
   workspace's `default_branch`; it is stored as `upstream_head_sha`. A
   failed fetch is a retryable `upstream_fetch_failed` (`shutdown` when the
   hub stops during it), a base that does not resolve a retryable
   `upstream_unknown`; no patch is touched.
3. **The patch list.** The non-deleted patches are listed in position order
   and the tip of each patch branch is read once as its `source_sha` (`null`
   for a missing branch); only these commits are used. A push to a patch
   branch during the run is accepted as at any other time and changes
   nothing the run builds.
4. **Each patch**, in a scratch working tree `<slug>.git/parley-rebuild/<job
   id>` that no ref points to (no ref a client can read changes before the
   publish, except `refs/remotes/upstream/*`):
   - `disabled` and `merged_upstream` patches are `skipped` with that
     reason, a patch without a branch with `branch_not_found`;
   - `active` and `conflict` patches are applied. With `rebase`, the commits
     reachable from `source_sha` and not from the upstream base are replayed
     oldest first, merge commits left out, each with its author, author date
     and message and the landing identity (`[land].committer_name`,
     `[land].committer_email`) as committer; a commit whose change is
     already present is dropped. With `merge`, the tip is merged with a merge
     commit (never a fast-forward), message `Merge branch '<branch_name>'`,
     first parent the head under construction, second `source_sha`, the
     landing identity as author and committer. A patch that adds nothing
     (no commit left to replay, or a tip already reachable) is `skipped`
     with `nothing_to_apply`; one that applies is `success` with
     `new_head_sha`.
   - The results so far are stored on the job after every patch, so a read
     of a `running` job shows progress; the four counts stay `0` until the
     job is `completed`.
5. **Conflicts.** When a commit (with `merge`, the tip) does not apply, git
   replays the workspace's remembered resolutions (git rerere, per
   workspace). When paths remain in conflict and the patch has two-parent
   merge commits among the commits the upstream base lacks, the hub redoes
   the newest 50 of them; where one conflicts, the merge commit's content is
   recorded as the resolution, and the commit is tried once more. A path
   still in conflict undoes the patch: the patch becomes `conflict` with
   `conflict_files` (sorted), `parley.patch.update` is emitted when its
   status changed (`{branch_name, changed: ["status"], from, to: "conflict",
   reason: "rebuild_conflict"}`, actor `system`), and its result is
   `conflict` with a [conflict report](#the-rebuild-conflict-report). With
   `fail_fast` the run stops: nothing is published and the job is
   `dead_letter` with the report as `conflict` and the `error` `rebuild of
   <slug>: the patch <branch> does not apply: <n> file(s) in conflict`.
   With `continue` the run goes on from the head before the patch.
6. **Publish.** In one store transaction the hub soft-deletes every patch
   the run skipped as `merged_upstream` that is still `merged_upstream`
   (`parley.patch.delete`, `{branch_name, reason: "merged_upstream"}`), sets
   every patch the run applied that is still `conflict` to `active` with
   `[]` (`parley.patch.update`, `from: "conflict"`, `to: "active"`,
   `reason: "rebuild_clean"`), renumbers the rest 1..N, and moves
   `refs/heads/<integration_branch>` to the new head with one
   compare-and-swap against the head it read (creating the branch when
   absent). The transaction commits only when the swap won; a branch moved
   by another writer meanwhile is a retryable `ref_moved` that changes no
   patch. A run with no applicable patch publishes the upstream base. The
   job gets `integration_head_sha` and `previous_integration_head_sha`
   (`null` when the branch did not exist) right after the commit.
7. **Push.** When `rebuild_push_integration_branch`, read now, is `true`,
   the branch is pushed to origin with the origin credential and
   force-with-lease against `refs/remotes/origin/<integration_branch>` (or
   "origin has no such branch" when the hub has none), within 120 seconds;
   success sets `integration_branch_pushed` and records the pushed head as
   origin's. A failure emits `parley.rebuild.push_failed` and does not fail
   the rebuild. When the setting is `false`, origin is not contacted.
8. **Completion and follow-up.** The counts are stored
   (`patches_removed` is the number of merged patches soft-deleted). After
   a publish, and after a `fail_fast` conflict, the hub compares its inputs
   with the present without fetching: a listed patch, not deleted since,
   whose branch exists at another tip than its `source_sha` is stale (when
   `auto_rebuild_after_push` is `true`), and so is upstream when the base
   resolved now differs from `upstream_head_sha` (when
   `auto_rebuild_after_sync` is `true`). Anything stale queues one
   follow-up rebuild with the job's strategy and fail mode and
   `submitted_by` `system:followup`; a duplicate is skipped silently. A tip
   or a base the check cannot read is logged and is not stale. No check
   follows a retryable failure, since the job runs again.

A git failure that is not a conflict (`git_error`), a git operation that
runs longer than 10 minutes (`timeout`) and a stop of the hub (`shutdown`)
are retryable, record no result for the patch in work, change no patch and
publish nothing. The limit holds each git operation of an attempt on its
own, those of the upstream fetch the attempt repeats included (one that
passes it there fails the attempt as `upstream_fetch_failed`), not the
attempt as a whole. Every attempt removes its scratch working tree, and
discards one left by an earlier attempt before it builds; and each start of
the hub, before any job runs, discards the scratch working trees and their
registrations a stop left in the copy of every carry-patch workspace,
logging a copy it cannot clean or does not find, with the workspace's slug,
and starting all the same. Failure text in
`error` and in events passes through the hub's scrubber, with the
attempt's credential values.

### The patch result

Each entry of `patch_results` has every member:

| Member | Type | Meaning |
|---|---|---|
| `patch_id`, `branch_name`, `position` | | the patch as listed at the start of the run |
| `status` | `success` \| `conflict` \| `skipped` | |
| `skipped_reason` | string or `null` | `disabled`, `merged_upstream`, `branch_not_found` or `nothing_to_apply`; `null` unless `skipped` |
| `source_sha` | string or `null` | the tip read at the listing; `null` for a missing branch |
| `new_head_sha` | string or `null` | the head under construction after the patch; `null` unless `success` |
| `conflict_files` | array of strings | `[]` unless `conflict` |
| `conflict` | object or `null` | the [conflict report](#the-rebuild-conflict-report); `null` unless `conflict` |

### The rebuild conflict report

`patch_id`, then the members of the hub's conflict report: `target_sha` (the
head under construction before the patch: the upstream base for the first
applied patch), `head_sha` (the patch's `source_sha`), `base_sha` (the merge
base of the two), `commit_sha` (with `rebase` the first commit that did not
apply, with `merge` `source_sha`), `files` (one entry per path still in
conflict after remembered resolutions, with `path`, `kind`, `base` (the
file in the base of the three-way merge: with `rebase` the parent of
`commit_sha`, with `merge` `base_sha`), `ours` (the file in `target_sha`),
`theirs` (the file in `commit_sha`) and `landed_by: []`),
`landed_changes: []` and `change: null`.

### The events of an attempt

Each has `resource_type` `rebuild`, the job id as `resource_id`, actor
`system` and the job's session id when it has one, and is written in the
transaction of the state change it reports.

| Event | Metadata |
|---|---|
| `parley.rebuild.complete` | `upstream_head_sha`, `integration_head_sha`, `previous_integration_head_sha`, `patches_applied`, `patches_skipped`, `patches_conflicted`, `patches_removed` |
| `parley.rebuild.fail` | `reason`, `will_retry` (`false` when the job is dead-lettered), `retry_count` (after the attempt), `conflict` (the report for reason `conflict`, else `null`) |
| `parley.rebuild.followup` | `follow_up_job_id`, `stale_patches` (branch names), `upstream_stale` |
| `parley.rebuild.push_failed` | `integration_branch`, `reason` |

The `reason` of `parley.rebuild.fail` is one of `conflict`,
`upstream_fetch_failed`, `upstream_unknown`, `git_error`, `timeout`,
`shutdown`, `ref_moved`, `workspace_busy`, `workspace_not_ready` and
`workspace_archived`. A move of the integration branch by a rebuild
emits no `parley.git.push`; `parley.rebuild.complete` carries both heads.

### The rebuild job record

Every route below that returns a job returns this object, with every
member present (`null` when unset); times are RFC 3339 UTC with whole
seconds.

| Member | Type | Meaning |
|---|---|---|
| `id` | UUID | the job's id |
| `workspace_slug` | string | |
| `status` | enum | one of the six statuses |
| `strategy` | `rebase` \| `merge` | captured when the job was queued |
| `fail_mode` | `fail_fast` \| `continue` | captured when the job was queued |
| `integration_branch` | string | captured when the job was queued |
| `submitted_by` | string | the user id of the caller who queued it (`admin_token` for the admin token), or `system:push`, `system:land`, `system:followup` |
| `retry_count` | integer | the failures that may pass so far |
| `next_attempt_at` | timestamp or `null` | set only while `failed` |
| `error` | string or `null` | the text of the last failure; kept while the job is retried, cleared when it completes or is requeued |
| `conflict` | object or `null` | the detail of the last failure (a conflict report) |
| `created_at`, `updated_at` | timestamp | |
| `completed_at` | timestamp or `null` | set in `completed`, `dead_letter` and `cancelled` |
| `upstream_head_sha`, `integration_head_sha`, `previous_integration_head_sha` | string or `null` | from the attempt's result |
| `patches_applied`, `patches_skipped`, `patches_conflicted`, `patches_removed` | integer | from the attempt's result; `0` without one |
| `integration_branch_pushed` | boolean | from the attempt's result; `false` without one |
| `patch_results` | array | from the attempt's result; `[]` without one |

The members "from the attempt's result" are what the running attempt last
stored: a claim of the job empties the result, so a job that runs again
starts from none.

```json
{"id":"7b1c0e2a-4d5f-4a6b-9c8d-0e1f2a3b4c5d","workspace_slug":"variant","status":"queued","strategy":"rebase","fail_mode":"fail_fast","integration_branch":"deploy","submitted_by":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","retry_count":0,"next_attempt_at":null,"error":null,"conflict":null,"created_at":"2026-11-17T10:30:15Z","updated_at":"2026-11-17T10:30:15Z","completed_at":null,"upstream_head_sha":null,"integration_head_sha":null,"previous_integration_head_sha":null,"patches_applied":0,"patches_skipped":0,"patches_conflicted":0,"patches_removed":0,"integration_branch_pushed":false,"patch_results":[]}
```

### Checks common to the rebuild routes

The three `GET` routes need `rebuilds:read` and read access; the other four
need `rebuilds:write` and write access. The routes check, in this order:
the scope (`403` `insufficient_scope`, whether or not the workspace
exists), access (`404` `not_found` for a workspace the caller cannot read
or an unknown slug, `403` `forbidden` for read access on a write route),
the request (`400` `invalid_request`), the workspace's mode (`400`
`workspace_mode_mismatch` unless it is `carry_patch`, also for a task
fork), on the four write routes and the preview its status (`409`
`workspace_busy` while it is `pending` or `cloning` or a reclone, archive
or delete runs, `409` `workspace_not_ready` when `failed`, `409`
`workspace_archived`), then the job: an id that is not a UUID, names no
job, or names a job of another workspace is `404` `not_found` with the
message `rebuild job not found`. The list and the read of one job answer
in every workspace status.

## `POST /api/v1/workspaces/{slug}/rebuild`

Queues a rebuild. The body is optional: no body, an empty one and `{}`
are alike.

| Member | Type | Meaning |
|---|---|---|
| `strategy` | `rebase` \| `merge` \| `null` | omitted, `null` or `""`: the workspace setting `rebuild_strategy` |
| `fail_mode` | `fail_fast` \| `continue` \| `null` | omitted, `null` or `""`: the workspace setting `rebuild_fail_mode` |

The job captures the strategy, the fail mode and the workspace's
integration branch; later changes of the settings do not change it.
`submitted_by` is the caller's user id. The hub stores the job and
`parley.rebuild.enqueue` in one transaction, before it answers. The route
takes `Idempotency-Key`: a repeat answers `200` with the first answer's
body and queues nothing.

| Status | When |
|---|---|
| `202 Accepted` | the rebuild job record, `status` `queued`, `retry_count` `0` |
| `400` `invalid_request` | `strategy` or `fail_mode` is not a string or `null` (`strategy must be a string`), or not one of its values (`strategy must be one of ["rebase" "merge"], not "squash"`, `fail_mode must be one of ["fail_fast" "continue"], not "stop"`) |
| `400` `workspace_mode_mismatch` | the workspace is not in mode `carry_patch` |
| `400` `no_active_patches` | no patch is `active` or `conflict` (`workspace "<slug>" has no patch in active or conflict to rebuild`) |
| `403` `insufficient_scope`, `forbidden`; `404` `not_found` | see [the common checks](#checks-common-to-the-rebuild-routes) |
| `409` `concurrent_rebuild` | a rebuild job of the workspace is `queued`, `running` or `failed` (`workspace "<slug>" has the rebuild job <id> queued, running or failed`) |
| `409` `workspace_busy`, `workspace_not_ready`, `workspace_archived` | the workspace's status does not allow the write |

`parley.rebuild.enqueue` has the job as its resource (`resource_type`
`rebuild`, `resource_id` the job id), the caller as actor and `metadata`
`submitted_by`, `strategy`, `fail_mode` and `patch_count`, the number of
patches in `active` or `conflict`. Every other source of a rebuild (a
reset to origin, and later a sync, a push to a patch branch, a landing on
one, a follow-up) queues through the same operation and writes the same
event, but skips the `no_active_patches` check and, when a rebuild is
already pending, queues nothing and writes nothing.

## `GET /api/v1/workspaces/{slug}/rebuilds`

The workspace's rebuild jobs as a [list](#lists) of job records, newest
first: `created_at` descending, then `id` descending.

| Query | Meaning |
|---|---|
| `status` | one of the six statuses, given once; any other value, an empty one or a repeat is `400` `invalid_request` (`status must be given once, as one of ["queued" "running" "failed" "completed" "dead_letter" "cancelled"]`) |
| `limit`, `cursor` | as for every list |

## `GET /api/v1/workspaces/{slug}/rebuilds/{id}`

`200` with the job record. A `running` job shows the result its attempt
last stored.

## `DELETE /api/v1/workspaces/{slug}/rebuilds/{id}`

Cancels a `queued` job: it becomes `cancelled` with `completed_at` set, in
one transaction with `parley.rebuild.cancel` (the caller as actor, the job
as resource, no `metadata`). A cancel and a worker's claim exclude each
other: a cancelled job never runs.

| Status | When |
|---|---|
| `200 OK` | the job record, `status` `cancelled` |
| `409` `job_not_cancellable` | the job is not `queued`, also when a worker claimed it a moment earlier (`rebuild job <id> is not queued; only a queued job can be cancelled`) |

## `POST /api/v1/workspaces/{slug}/rebuilds/{id}/requeue`

Moves a `dead_letter` job back to `queued`: `retry_count` `0`, `error`,
`conflict`, `next_attempt_at` and `completed_at` `null`, in one
transaction with `parley.rebuild.requeue` (the caller as actor, the job as
resource, no `metadata`). The job keeps its id, `created_at`, strategy,
fail mode, integration branch and `submitted_by`. A body is ignored.

| Status | When |
|---|---|
| `200 OK` | the job record, `status` `queued` |
| `409` `job_not_requeueable` | the job is not `dead_letter`, whatever the other jobs are (`rebuild job <id> is not dead_letter; only a dead_letter job can be requeued`) |
| `409` `concurrent_rebuild` | another rebuild job of the workspace is `queued`, `running` or `failed` |

## `POST /api/v1/workspaces/{slug}/rebuilds/{id}/rollback`

Moves the workspace's integration branch back to the head it had before
the completed rebuild `{id}`: the job's `previous_integration_head_sha`.
The hub reads the branch's head and moves the branch with one
compare-and-swap against the head it read; a branch that does not exist
is created at the target, the swap expecting it absent. A body is ignored,
and the route takes no `Idempotency-Key`. A branch already at the target
is swapped to it again and answered `200` with both members equal.

Nothing else changes: no patch (its status, position, `conflict_files` or
deletion), no job, no other ref, nothing on origin. The branch is not
pushed, whatever `rebuild_push_integration_branch` says, and no rebuild is
queued. A rebuild job of the workspace that is `queued`, `running` or
`failed` neither delays nor refuses it. The next rebuild that publishes
replaces the rolled-back head; to keep it, turn `auto_rebuild_after_push`
and `auto_rebuild_after_sync` off or disable the patch at fault.

| Status | When |
|---|---|
| `200 OK` | `{"rolled_back_to": "<the job's previous_integration_head_sha>", "previous_head_sha": "<the head it replaced>"}`, both full commit ids; `previous_head_sha` is `null` when the rollback created the branch |
| `400` `workspace_mode_mismatch`; `403` `insufficient_scope`, `forbidden`; `404` `not_found`; `409` `workspace_busy`, `workspace_not_ready`, `workspace_archived` | see [the common checks](#checks-common-to-the-rebuild-routes) |
| `409` `rollback_unavailable` | the job is not `completed` (`rebuild job <id> is <status>; only a completed rebuild can be rolled back`), recorded no previous head, the first rebuild (`rebuild job <id> recorded no previous head of the integration branch`), or its previous head is not a commit of the repository, such as after a reclone (`the previous head <sha> of rebuild job <id> is not a commit of the repository`) |
| `409` `ref_moved` | another writer (a rebuild's publish, another rollback) moved the branch between the read and the swap; `details.current_sha` is the branch's head as read after the miss, `null` when it is gone, and the branch keeps that writer's value (`the branch "<branch>" moved while it was rolled back; read it again and retry`) |
| `409` `conflict` | the branch is missing and another ref's name is in the way of its name |

A successful rollback writes `parley.rebuild.rollback` once the branch has
moved: the job as its resource (`resource_type` `rebuild`, `resource_id`
the job id), the caller as actor, the workspace's slug, and `metadata`
`integration_branch`, `rolled_back_to` and `previous_head_sha` (`null`
when the branch was created). A refused rollback writes nothing. Should
the event fail to commit, the hub swaps the branch back from the target to
the head it read and answers `500` `internal`.

## `GET /api/v1/workspaces/{slug}/rebuild-preview`

Predicts what a rebuild would do with each patch, against the upstream
base as the last upstream fetch left it: the head of the upstream's
default branch under `refs/remotes/upstream/`, else the upstream branch
named like the workspace's `default_branch`. The hub fetches nothing and
contacts neither upstream nor origin, so a preview works from the last
fetched base while upstream is unreachable. Unlike the list and the read
of one job, the route refuses a workspace that is not ready. It takes no
query parameter and no `Idempotency-Key`.

The hub lists the patches that are not deleted, in position order, leaves
out those `disabled` and `merged_upstream`, reads the tip of each
remaining patch branch once, and starts a predicted state at the upstream
base. Each tip is merged onto the state in a trial merge (`git merge-tree
--write-tree`: no working tree, no ref moved, no remembered resolution
read or recorded), whatever `rebuild_strategy` says:

| Situation | `status` | The predicted state |
|---|---|---|
| the patch branch does not exist | `would_skip` | unchanged |
| the tip merges without a conflict, a tip the state already holds included | `would_succeed` | becomes a merge commit of the state and the tip that no ref points to (unchanged for a tip it already holds) |
| the merge has a conflict | `would_conflict` | unchanged: the later patches are tested without this one |

A preview is a prediction: a patch reported `would_conflict` may apply in
a real rebuild, through a remembered resolution or the `rebase` strategy.
It changes no ref, no patch (a `would_conflict` patch keeps its status and
`conflict_files`), no job, no remembered resolution, and writes no event;
it may leave commit objects of the predicted state that no ref reaches,
which git's housekeeping removes. It runs while a rebuild runs.

| Status | When |
|---|---|
| `200 OK` | `{"upstream_head_sha": "<base used>", "patch_results": [{"patch_id", "branch_name", "position", "status", "conflict_files", "conflict"}]}`, one entry per tested patch in position order, `[]` when there is none |
| `400` `workspace_mode_mismatch`; `403` `insufficient_scope`; `404` `not_found`; `409` `workspace_busy`, `workspace_not_ready`, `workspace_archived` | see [the common checks](#checks-common-to-the-rebuild-routes) |
| `409` `upstream_unknown` | neither upstream ref resolves (`workspace "<slug>" has no upstream base: its copy holds no upstream branch to start from; sync it first`) |
| `500` `internal` | a git command failed for another reason than a conflict, its time limit included; no partial result is returned |

`conflict_files` is `[]` unless the status is `would_conflict`; then it
lists the conflicted paths in ascending order. `conflict` is `null` unless
the status is `would_conflict`; then it is [the rebuild conflict
report](#the-rebuild-conflict-report) of a merge: `target_sha` is the
predicted state the patch was tested against (the upstream base for the
first tested patch, otherwise a commit no ref points to), `head_sha` and
`commit_sha` the patch tip, `base_sha` the merge base of the two, and the
files' `base` the file in `base_sha`.

## Remembered resolutions and the patch status

A rebuild records each conflict git's rerere meets in the workspace's
copy, and the resolution it learns from a merge on the patch branch, which
later rebuilds replay (see [Rebuild jobs](#rebuild-jobs)). The hub keeps
one **entry** per recorded conflict: `id`, git's id of the conflict (its
directory in the copy's `rr-cache`, 40 hexadecimal digits), `path`, the
file it was first met at, `resolved`, read from the copy (`true` when git
holds a resolution for it), and `recorded_at`, when the conflict, or later
its resolution, was recorded, in whole seconds UTC. A fresh clone and a
reclone start with no entry; the delete of the workspace removes them.

The three routes check, in this order: the scope (`403`
`insufficient_scope`, whether or not the workspace exists), access (`404`
`not_found` for a workspace the caller cannot read or an unknown slug,
`403` `forbidden` for read access on the `DELETE`), the request (`400`
`invalid_request`), the workspace's mode (`400` `workspace_mode_mismatch`
unless it is `carry_patch`, also for every task fork), and on the `DELETE`
only its status (`409` `workspace_busy` while it is `pending` or `cloning`
or a reclone, archive or delete runs, `409` `workspace_not_ready` when
`failed`, `409` `workspace_archived`). The two `GET` routes answer in
every status.

| Route | Scope | Access |
|---|---|---|
| `GET /api/v1/workspaces/{slug}/rerere` | `workspaces:read` | read |
| `DELETE /api/v1/workspaces/{slug}/rerere/{pathspec}` | `workspaces:write` | write |
| `GET /api/v1/workspaces/{slug}/patch-status` | `workspaces:read` | read |

## `GET /api/v1/workspaces/{slug}/rerere`

The workspace's entries as a [list](#lists), resolved or not, newest
`recorded_at` first, then by `id` ascending. Each item has exactly `id`,
`path`, `resolved` and `recorded_at`. A workspace without an entry, which
includes one whose copy is not on disk yet, answers `items: []`. An entry
whose copy is missing or unreadable is listed with `resolved` `false`. The
route reads the store and the copy's `rr-cache`, runs no git command,
contacts no remote and changes nothing.

| Status | When |
|---|---|
| `200 OK` | a page of entries |
| `400` `invalid_request` | a bad `limit` or `cursor` |
| `400` `workspace_mode_mismatch`; `403` `insufficient_scope`; `404` `not_found` | see the checks above |

## `DELETE /api/v1/workspaces/{slug}/rerere/{pathspec}`

Forgets remembered resolutions. `{pathspec}` is the rest of the path after
`/rerere/`, slashes included, compared after percent-decoding, byte for
byte: when it equals an entry's `id`, that entry alone is forgotten;
otherwise every entry whose `path` equals it. A prefix of a path, a path
in another letter case and a value such as `../x` match nothing. Each
entry is removed by its own id: first git's record of the conflict and its
resolution (`rr-cache/<id>` in the copy, every variant in it), then the
stored entry, so a forget cut short leaves an entry without a resolution,
which the list shows unresolved, never a resolution without an entry. An
entry already gone counts as removed, so two forgets of one entry at once
answer `204` and `204` or `404`.

A forget only changes what later rebuilds replay: a rebuild that meets
the conflict again stops at it, and records it again unresolved. It
changes no rebuild job, no patch and no ref, queues no rebuild and writes
no event.

| Status | When |
|---|---|
| `204 No Content` | entries were forgotten |
| `400` `invalid_request` | the pathspec is empty (`the pathspec is empty; give the id or the path of a remembered resolution`) or starts with `-` (`the pathspec must not start with '-'`), checked before the mode and the status |
| `404` `not_found` | no entry has the pathspec as its id or its path (`resolution not found`); nothing is removed |
| `400` `workspace_mode_mismatch`; `403` `insufficient_scope`, `forbidden`; `404` `not_found`; `409` `workspace_busy`, `workspace_not_ready`, `workspace_archived` | see the checks above |
| `500` `internal` | git did not finish within its time limit (`a git operation did not finish within its time limit`), or another failure |

## `GET /api/v1/workspaces/{slug}/patch-status`

One document that summarises the workspace from what the hub holds: its
record, its patches, its rebuild jobs, its entries and its copy. The hub
fetches nothing, contacts neither origin nor upstream and changes nothing,
so the answer comes while both are unreachable, and `upstream_head_sha` is
the stored value, not upstream's present head. Every member is present,
`null` where unset.

| Member | Meaning |
|---|---|
| `workspace_slug`, `workspace_mode`, `status` | from the workspace record |
| `git_url`, `upstream_url` | the stored values |
| `integration_branch` | the workspace's integration branch, `deploy` when the record names none |
| `clone_error`, `sync_error`, `upstream_head_sha`, `last_sync_at` | from the record, or `null` |
| `integration_head_sha` | the full commit id of `refs/heads/<integration_branch>` in the copy; `null` when the branch, or the copy, does not exist |
| `last_rebuild` | `{id, status, created_at, completed_at}` of the newest rebuild job in any status, by `created_at`; `null` without one |
| `last_completed_rebuild` | the same for the newest `completed` job |
| `patches` | every patch that is not deleted, in position order, each with exactly `id`, `branch_name`, `position`, `status`, `conflict_files` (never `null`), `last_rebuild_result`, `origin_sync_state`, `origin_sha` and `origin_synced_at`; not paged |
| `summary` | `total_patches` (the patches that are not deleted), `active`, `conflict`, `disabled`, `merged_upstream` (those patches by status), `rerere_resolutions` (the entries, resolved or not), `patches_diverged` and `patches_missing_on_origin` (those patches by `origin_sync_state`); `0` when there is nothing to count |

`last_rebuild_result` is the `status` (`success`, `conflict` or `skipped`)
of the patch's entry in the `patch_results` of the newest rebuild job, by
`created_at`, that has one for it, or `null`.

| Status | When |
|---|---|
| `200 OK` | the document |
| `400` `workspace_mode_mismatch`; `403` `insufficient_scope`; `404` `not_found` | see the checks above |

## Source control reads

The routes under `/api/v1/workspaces/{slug}/scm` read a workspace's
repository without a clone. They are `GET` routes; so far they are the two
refs routes, the two commits routes, the tree route, the file route, the
diff route, the compare route, the blame route, the search route and the outline route below. Every one of them behaves
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
`tree` for the tree route, `file` for the file route, `diff` for the diff route, `compare` for the compare route, `blame` for the blame route, `search` for the search route, `outline` for the outline route) and `ok` for a status
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
`…/scm/file`, `…/scm/blame`, `…/scm/search` and `…/scm/outline`, `{sha}` of `…/scm/commits/{sha}`, `base` and `head` of
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
costs no history walk, tree listing, file read, diff, trial rebase, blame,
search or outline; a tag that does not match gets the full answer. An error answer
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
| `session_id` | the agent session that introduced the commit: for the head of a change's version, the session of the first version with that head (the push, API commit or rebase that recorded it); for a commit a land wrote in place of a version's commit, the session the land was asked in; `null` for a write in no session and for any other commit, such as one below a version's head or on a branch that is no change's source |
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
| `landed_changes` | the changes landed on the target between `base_sha` and `target_sha` that changed a conflicted file, as in [a land's conflict report](#landing) |
| `change` | the change whose head was rebased; always `null` in a comparison |

In a conflicted file, `base`, `ours` and `theirs` are the file on each side
of the three-way merge that failed, `{"sha": …, "mode": …, "path": …}` (the
blob id, git's six-digit octal mode and the path on that side, which a rename
makes differ), or `null` on a side the file does not exist on: `base` the
failed commit's parent, `ours` the target with the commits of `head` before
the failed one replayed on it, `theirs` the failed commit. `kind` is one
per file, the first that applies of: `submodule` (a side holds a submodule,
mode `160000`), `binary` (both sides changed a file git treats as binary),
`rename` (the sides renamed the file in ways that clash), `add_add` (both
added the path with different content, or one put a file where the other
put a directory), `modify_delete` (the target changed a file the head
deleted), `delete_modify` (the target deleted a file the head changed),
`mode` (the sides hold the file with different modes, such as a file and a
symlink, or the same content added with two modes, and the content is the
same or merges) and `content` (both changed the same lines, also where one
side alone changed the mode, which merges). A file's `path` is its path in
the target, `ours.path`, so a file both sides renamed is reported at the
target's new path; where the target has no file (`delete_modify`) it is
the path of the other sides. A file git moved aside to `<path>~<commit>`
to record both sides is reported at its own `path`. `landed_by` lists the
numbers of the landed changes in `landed_changes` that changed the file,
`[]` for none.

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


## `GET /api/v1/workspaces/{slug}/scm/outline`

Answers what the source file `path` of the commit `ref` names declares: its
functions, types and other declarations, in line order. It needs no index
and reads one file, so it works at any ref the file route serves, a fork's
own refs included. Its query parameters:

| Parameter | Meaning |
|---|---|
| `ref` | the revision whose file is outlined; default the workspace's default branch, a fork's base branch |
| `path` | the file outlined, required (see [Source control reads](#source-control-reads) for the paths taken) |

The answer:

| Member | Value |
|---|---|
| `path` | the file's path, as asked |
| `resolved_sha` | the full id of the commit `ref` resolved to |
| `language` | the name of the file's language, from its extension (a `.h` file that holds C++ is `C++`): `Go`, `Python`, `JavaScript`, `TypeScript`, `Rust`, `Java`, `Kotlin`, `C#`, `Ruby`, `C`, `C++`, `PHP`, `Scala`, `Lua` or `Shell`; `""` for a file of no known language and for a symbolic link |
| `supported` | whether this hub outlined the file; `false` for a file of no known language, for a symbolic link, for a language this build of the hub has no backend for, and for a file its backend could not take at all (one whose first NUL byte lies between byte 8,000 and byte 8,192 among them) |
| `declarations` | the file's declarations in line order; `[]` when it declares nothing and whenever `supported` is `false` |

A declaration:

| Member | Value |
|---|---|
| `kind` | one of `func`, `method`, `type`, `class`, `interface`, `enum`, `trait`, `const`, `var`, `module`, `macro` |
| `name` | the declared name |
| `container` | the type, class or module the declaration belongs to, such as a method's type; `""` for none |
| `signature` | the declaration's header as the source writes it, without its body |
| `exported` | whether the declaration is visible outside its file or package, by the language's own rule |
| `start_line`, `end_line` | the declaration's first and last line, 1-based and inclusive; `end_line` is `0` when it is not known |

The fifteen languages are outlined by a hub built with cgo, which the hub
image holds. A `parleyd` built without cgo outlines Go alone and answers
every other language with its name and `supported: false`. A file that does
not parse in its language is outlined as far as its syntax tree goes, and a
symbolic link is not followed: its content is the path it points at.

Only a text file of at most 1 MiB (1,048,576 bytes) is outlined; its size
is checked before any of it is read. A file is binary, as for blame, when a
NUL byte is among its first 8,000 bytes. The checks run in this order:
`path`, `ref`, the caching answer, the path at the commit, the size, the
bytes, and then the parse. A read whose parse has not ended at the read
timeout is answered `503 scm_timeout` then, like every read; the hub parses
at most four files at once, a parse left behind by such a read included, and
a read that finds no place among them within its time is answered the same.

The answer's entity tag and `Cache-Control` are those of every read, from
the commit and the request alone. `supported` also depends on how the hub
was built, so an answer kept from a hub built without cgo, one marked
immutable among them, does not change when the hub is replaced by one built
with cgo.

| Status | When |
|---|---|
| `200 OK` | the outline, also when `supported` is `false` |
| `400` `invalid_request` | no `path` (`path is required`); an illegal `path`, with the file route's message for it; a `path` that names a directory or a submodule entry (`path "internal" is a directory or a submodule at <sha>, not a file`) |
| `404` `ref_not_found` | `ref` names no one commit |
| `404` `path_not_found` | `path` does not exist at the commit (`path "nope.go" does not exist at <sha>`) |
| `409` `workspace_busy`, `workspace_not_ready` | the workspace's state (see [Source control reads](#source-control-reads)) |
| `413` `payload_too_large` | the file is over 1 MiB (`file "big.go" is 1048577 bytes, over the outline limit of 1048576`), with the member `size`, the file's size in bytes, inside `error` |
| `422` `binary_file` | the file is binary (`file "logo.png" is binary at <sha>, and a binary file has no declarations to outline`) |
| `503` `scm_timeout` | the read ran out of the read timeout before its answer began (see [Source control reads](#source-control-reads)) |

```json
{"path":"router.go","resolved_sha":"3d0c5a1f0b6e2d5c8a7f4e9b1c2d3e4f5a6b7c8d","language":"Go","supported":true,"declarations":[{"kind":"const","name":"MaxRoutes","container":"","signature":"const MaxRoutes","exported":true,"start_line":4,"end_line":4},{"kind":"type","name":"Router","container":"","signature":"type Router struct","exported":true,"start_line":6,"end_line":8},{"kind":"method","name":"Handle","container":"Router","signature":"func (r *Router) Handle(pattern string)","exported":true,"start_line":10,"end_line":12},{"kind":"func","name":"NewRouter","container":"","signature":"func NewRouter() *Router","exported":true,"start_line":14,"end_line":16},{"kind":"func","name":"match","container":"","signature":"func match(pattern, path string) bool","exported":false,"start_line":18,"end_line":20}]}
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
An accepted session goes, in lower case, onto the write's
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
(the commit), `null` on a side that lacks it. `landed_changes` and each
`landed_by` name the landed changes that touched the files, as in [a
land's conflict report](#landing), and `change` is `null`.

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
{"workspace_slug":"demo","number":1,"ref":"demo#1","title":"Add x","intent":"Because y","state":"open","target_branch":"main","source":{"workspace_slug":"demo","branch":"feat"},"version":1,"head_sha":"0b86db2bc73da0f067ec404140cfc00060162383","base_sha":"dc54adf9e0607cad9039738e34a4c3061a859f17","depends_on":null,"dependants":[],"author":{"actor_type":"api_key","actor_id":"bce9bb4d-cbcb-47c3-ab7a-d18f4b5ab95a","username":"owner"},"session_id":null,"sessions":[],"mergeable":"unknown","conflict":null,"needs_restack":false,"restack_reason":null,"stats":{"commits":1,"files_changed":1,"insertions":1,"deletions":0},"paths":["feat.txt"],"paths_truncated":false,"landing":{"status":"ready","reason":null,"requirements":[{"rule":"default","paths":["feat.txt"],"approvals":{"required":0,"counted":0,"approvers":["owner"]},"checks":[],"block":false,"satisfied":true}]},"auto_land":false,"labels":["docs"],"landed":null,"reverts":null,"reverted_by":null,"created_at":"2026-11-15T13:12:13Z","updated_at":"2026-11-15T13:12:13Z"}
```

| Member | Meaning |
|---|---|
| `state` | `draft`, `open`, `landed` or `abandoned` |
| `version`, `head_sha`, `base_sha` | the current version, its head and its base: the head of the change it depends on; with none, the merge base of the head and the target branch; while the target branch does not exist, the previous version's base; `null` when the head shares no history with the target branch |
| `depends_on`, `dependants` | the change this one depends on, `null` for none, and the numbers of the draft and open changes that depend on this one, lowest first. At open and on every new version the hub sets `depends_on` from the commit graph: to the nearest draft or open change of the same workspace and target branch whose head is a proper ancestor of the new head (the fewest commits between the two heads, the lowest number on a tie), or to `null` when there is none. A `depends_on` the caller names at open is the link of version 1 instead; every later version is linked from the commit graph again. A `PATCH` sets or clears it (see [Modifying a change](#modifying-a-change)) |
| `author` | who opened it: `actor_type` (`admin_token`, `api_key`, `pat` or `system`), `actor_id` and `username`, both `null` for the admin token and the hub |
| `session_id`, `sessions` | the session of the current version, `null` for none, and every session that contributed a version, in the order of its first one |
| `mergeable`, `conflict` | whether the commits of the current version that the onto-commit lacks rebase onto the onto-commit, the commit the change sits on: `clean`, `conflict` or `unknown`; the conflict report while it is `conflict`, else `null` (see [Mergeability](#mergeability)) |
| `needs_restack`, `restack_reason` | whether the change must be rebased onto what it depends on, and why. `stack_too_deep`: the change sits deeper in its stack than `[changes].max_stack_depth` (its depth is the number of changes in its chain of `depends_on` links down to a change with none, itself included); its version and its `depends_on` are recorded all the same, the hub restacks neither it nor the changes above it, and the flag is cleared when a link below it changes so that it is back within the limit. `auto_restack_off`: the workspace's `changes.auto_restack` is `false` and the change is not built on what it must sit on. `conflict`: its restack conflicted. A restack clears the flag for any reason but `stack_too_deep` (see [Stacks and restack](#stacks-and-restack)) |
| `stats` | the `commits`, `files_changed`, `insertions` and `deletions` of `base_sha..head_sha`; with no `base_sha`, of every commit of the head, its files measured from the empty tree |
| `paths`, `paths_truncated` | the files the current version touches, both sides of a rename, in byte order, at most 2,000; `paths_truncated` is `true` when it touches more |
| `landing` | whether it may land: `status` (`ready`, `pending`, `blocked`), the `reason` it is blocked for a structural cause (`draft`, `target_not_found`, `nothing_to_land`, `unrelated_history`, `merge_commits`), else `null`, and the policy's `requirements`, listed for a blocked change too (see [The landing of a change](#the-landing-of-a-change)) |
| `landed` | how it landed, `null` until then: `sha`, `at`, `by` (an author object), `land_id` and `commits`, each `{version_sha, landed_sha}` |
| `reverts`, `reverted_by` | the number of the change it reverts, `null` for none, and the highest number of the `draft`, `open` or `landed` changes that revert it, `null` when there is none: an abandoned reverting change does not count (see [`POST …/revert`](#post-apiv1workspacesslugchangesnumberrevert)) |

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
`system`); a restack (`restack`, see [Stacks and
restack](#stacks-and-restack)) moves it too. A direct push to a protected branch records the version too
when that branch is the source of a draft or open change; a landing
records none.
A git push that deletes the source branch abandons the change; the
deletion of its task fork abandons every draft and open change from it,
and the deletion of the target workspace deletes its changes. A reclone
of the source workspace abandons, once its clone work leaves the workspace
`ready`, every draft and open change whose source branch did not come
back from origin, by the hub. Each
abandonment writes `parley.change.abandon`, its `reason` `source_deleted`,
`source_workspace_deleted` or `source_lost`. The versions and their refs
stay.

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
| `depends_on` | the number of the change this one depends on, a positive integer: a draft or open change of `{slug}` with the same target branch. It is the change's link for version 1 in place of the one the commit graph gives, and `base_sha` is that change's head when the source branch is built on it; when it is not, the change is restacked onto it (see [Stacks and restack](#stacks-and-restack)) |
| `auto_land` | defaults to `false`; `true` has the hub land the change by itself once it is landable (see [Automatic landing](#automatic-landing)). A path-scoped token is held to its paths for it as for the open itself |
| `labels` | defaults to `[]`; at most 20, each matching `^[a-z0-9][a-z0-9_.:/-]{0,62}$`; a repeated label is kept once |
| `session_id` | defaults to the session recorded on the source fork; a defaulted session the session rule refuses is dropped |

```json
{"source":{"branch":"feat"},"labels":["docs"]}
```

The answer is `201` with the change object, at version 1: the head of the
source branch, of kind `ref`, written at `refs/parley/changes/<number>/1`.
`mergeable` is `unknown` until its first check. `landing` is version 1
evaluated against the current revision of the policy of `{slug}` (see
[The landing of a change](#the-landing-of-a-change)): a draft is
`blocked` for `draft`, with its requirements listed. Under the revision-0
document, whose default requires nothing, every other change is `ready`,
its files listed under the `default` requirement. A `session_id` is
judged by the session-linking rule of [Sessions](#sessions).

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
the token's globs; the change `depends_on` names. Of two opens of one source at the same time, one
answers `201` and the other `409` `change_exists`.

| Status | When |
|---|---|
| `201 Created` | the change, as above; a replay of a recorded `Idempotency-Key` with the same body answers the first answer |
| `400` `invalid_request` | a body member of the wrong type; no `source.branch`; more than 20 labels or a label that does not match; a `depends_on` below 1; the source branch is the target branch of the same workspace; a title that is empty, longer than 200 characters or more than one line, also no `title` while the head commit has no subject; an intent longer than 16,000 characters |
| `400` `intent_required` | the intent is empty or white space, the change is not a draft and the target requires an intent |
| `400` `invalid_dependency` | `depends_on` names no change of `{slug}`, a change into another target branch, or a change that is neither draft nor open |
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
[below](#get-apiv1workspacesslugchangesnumberdiff)),
`GET /api/v1/workspaces/{slug}/changes/{number}/stack` (see
[The stack of a change](#the-stack-of-a-change)) and
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
parent). A token bound to a task fork reaches these six routes on the
fork's parent, and there only the changes whose `source.workspace_slug` is
that fork: every other change answers `404` `change_not_found` as if it did
not exist, and `by-commit` finds only those changes.

`GET …/changes/{number}` answers `200` with the change object. `include`, a
comma list of `versions`, `approvals`, `checks`, `stack` and `overlaps`
(also given more than once), adds a member of that name after the change's
own: `versions` the list of versions, oldest first; `stack` the change's
stack, as `GET …/changes/{number}/stack` answers it for the same caller;
`approvals` the verdicts on the current version, newest first, and
`checks` the check results that apply to the current version, by name, as
the lists of [Approvals and checks](#approvals-and-checks) answer them;
and `overlaps` the change's overlap entries, unpaged, in the order of
`GET …/changes/{number}/overlaps` (see [Awareness](#awareness)). Any other name, the empty name included, answers `400`
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
  answered `400` `intent_required`. A body with `auto_land` `true` from a
  path-scoped token is held to the token's paths before anything is
  stored: a file of the change's current version, from its base to its
  head, that matches none of the token's globs is answered `403`
  `path_not_allowed` with that file as `error.path` (`path <path> is
  outside this token's allowed paths`), and nothing of the body is
  applied. `auto_land` `false` is never refused. Turning `auto_land` on
  stores the caller as the user who set it, with the type of the
  credential and its globs, and a change that is landable already gets an
  automatic land attempt (see [Automatic landing](#automatic-landing)). A
  `state` of `"open"` on a draft marks it ready: the change becomes
  `open`, its `landing` is evaluated again without the `draft` block, and
  `parley.change.open` is written by the caller with `number`, `ref`,
  `version`, `head_sha`, `title`, `target_branch`, `source_workspace`,
  `source_branch`, `depends_on` and `was_draft` (`true`), followed by
  `parley.change.ready` when the change is then landable (see
  [Mergeability](#mergeability)). A `state` of `"draft"` on an open change
  makes it a draft: `landing` becomes `blocked` for `draft`, with the
  policy's requirements still listed, its versions, labels,
  `auto_land` and `mergeable` stay, its landable announcement is
  forgotten, and no event is written. A `state` the change already has is
  no edit, before the intent rule as after it: `{"state":"open"}` on an
  open change whose intent is empty, opened before `{slug}` required one,
  is answered `200` with nothing changed, while an edit of another member
  of that change is answered `400` `intent_required`. A `target_branch` other than the change's own retargets a
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
  The member `depends_on`, a change number or `null` (another type is
  `400` `invalid_request`), links a draft or open change to another or
  unlinks it, after the other members are stored; on a landed or
  abandoned change it is ignored, and the link the change already has is
  no edit. A number must name a draft or open change of `{slug}` with
  the same target branch, neither the change itself nor one that already
  depends on it, directly or through others: else `400`
  `invalid_dependency`, and nothing of the body is applied. The change
  then depends on it, and when its head has that change's head as an
  ancestor `base_sha` becomes that head, with `stats`, `paths` and
  `paths_truncated` measured against it; otherwise the base stays and
  the change is restacked onto it (see
  [Stacks and restack](#stacks-and-restack)).
  `null` clears the link, and `base_sha` becomes the merge base of
  `head_sha` and the target branch's head (the base stays while the
  target branch does not exist), with `stats`, `paths` and
  `paths_truncated` measured against it. A new base is also the current
  version's: its `base_sha`, `stats` and `patch_equivalent_to` are
  measured again. Either way `mergeable` becomes `unknown` and is checked
  again, and the `PATCH` writes no event.
- `POST …/abandon` takes no body or a JSON object (no body is the same as
  `{}`) with two optional members: `reason`, a string, and `dependants`,
  `"reparent"` or `"abandon"`; another type, `null`, or another value of
  `dependants` is `400` `invalid_request`. An abandoned or landed change
  is answered `409` `change_closed`. A change on which a draft or open
  change depends (its `dependants`) is answered `409` `has_dependants`
  when the body has no `dependants`, with the numbers of the dependants
  in the error's `dependants` member, and nothing changes. The change
  moves to `abandoned` and its landable announcement is forgotten, its
  waiting mergeability check is dropped once the abandon has committed,
  and `parley.change.abandon` is
  written by the caller with `reason` as given (the empty string when
  absent), `number`, `ref`, `version` and `head_sha`. The source branch
  and the version refs stay. With `"abandon"`, every draft or open change
  above it in its stack, direct or not, is abandoned too in the same
  transaction, with the same `reason` and one `parley.change.abandon`
  each. With `"reparent"`, each direct dependant stays as it is but now
  depends on what the abandoned change depended on (`null` when nothing),
  and gets a pending restack, with the trigger `abandon`, in the same
  transaction (see [Stacks and restack](#stacks-and-restack); with
  `changes.auto_restack` `false` it gets `needs_restack` with
  `restack_reason` `auto_restack_off` instead): it is
  rebased onto the head of the change it now depends on, or onto the head
  of its target branch when it depends on none, without the abandoned
  change's commits. When that record cannot be written, the answer is
  `500` and nothing is abandoned. `dependants` on a change that has none
  changes nothing.
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
  that appeared in the meantime is taken at its head. A branch the
  restore created is a new target too: every draft or open change of the
  source workspace into it has its `mergeable` set to `unknown` and checked
  again, as a push creating it would. The change moves
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
`open`, otherwise the head of the target branch. For a change in a stack
(one with a `depends_on`, a `needs_restack`, or a change below that
landed) the trial replays only the change's own commits, those after its
`base_sha`, as a restack does, so that the report never names a commit of
the change below and its `base_sha` is the change's. The result is `clean`,
or `conflict` with the conflict report in `conflict`: `target_sha` (the
onto-commit), `head_sha`, `base_sha`, `commit_sha` (the first commit that
does not apply), `files` (each `path`, `kind`, `base`, `ours`, `theirs`,
`landed_by`), `landed_changes` and `change`. The trial writes no ref and
no work tree in either repository.

`mergeable` becomes `unknown`, with `conflict` `null`, in the transaction
of what triggers a new check: a new version of the change, a new version
of the change it depends on, a move, creation or deletion of its target
branch by a push, the source control API, a landing or a sync, or by a
restore that creates it as its source branch, a retarget and a restore. It stays `unknown` until the check stores its result, and a
check of a change with a pending restack stores nothing: its head still
holds the old commits of the change below, so it stays `unknown` until
the restack has ended; a result
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
  and `needs_restack`. A restack that conflicts stores its conflict the
  same way, after it has set `needs_restack` (see
  [Stacks and restack](#stacks-and-restack)). A further conflict, for the same or a later
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

### `GET /api/v1/workspaces/{slug}/changes/{number}/conflict`

Answers `200` with the change's conflict as it is now,
`{"mergeable": …, "conflict": …}`: `mergeable` is `clean` or `conflict`,
never `unknown`, and `conflict` is the conflict report described above, or
`null` when `clean`. It needs, for a personal access token,
`changes:read`, and read access to `{slug}`, and refuses as the routes of
[Reading a change](#reading-a-change) do: `403` `insufficient_scope`, `404`
`not_found`, and `404` `change_not_found` for a change the request cannot
name. A token bound to a task fork reaches it on the fork's parent for the
changes whose `source.workspace_slug` is that fork; any other change answers
`404` `change_not_found`, as if it did not exist (unlike the land routes'
`403` `token_bound`).

- A `landed` or `abandoned` change answers what it holds, without a
  replay: `conflict` with the stored report, otherwise `clean`.
- A `draft` or `open` change whose `mergeable` is `conflict` with a report
  whose `target_sha` is the present onto-commit and whose `head_sha` is the
  change's `head_sha` answers that report, without a replay.
- Any other `draft` or `open` change is trial-rebased now, as the
  mergeability check does it: onto the head of the `draft` or `open` change
  named by `depends_on`, otherwise onto the head of the target branch, and
  only its own commits for a change in a stack. A conflict names the landed
  changes it collided with.

The read stores nothing on the change and writes no event: a result it
computes is not the change's `mergeable`, which only the check stores.
While the target branch of a `draft` or `open` change does not exist the
answer is `409` `conflict` (`<ref> has no conflict report: its target branch
<branch> does not exist`); a merge commit among the commits to replay is
`400` `merge_commits_unsupported`.

```json
{"mergeable":"conflict","conflict":{"target_sha":"…","head_sha":"…","base_sha":"…","commit_sha":"…","files":[{"path":"a.txt","kind":"content","base":{"sha":"…","mode":"100644","path":"a.txt"},"ours":{"sha":"…","mode":"100644","path":"a.txt"},"theirs":{"sha":"…","mode":"100644","path":"a.txt"},"landed_by":[]}],"landed_changes":[],"change":null}}
```

### `GET /api/v1/workspaces/{slug}/changes/{number}/conflict/file?path=`

Answers `200` with `Content-Type: text/plain; charset=utf-8` and, as the
whole body, the file `path` (percent-encoded in the query) of the change's
current conflict report merged as text: every line that both sides keep, or
that one side alone changed, appears once, and each region both changed is
written as a line `<<<<<<< target`, the lines of the target as replayed so
far (the entry's `ours`), a line `=======`, the lines of the file in
`commit_sha` (the entry's `theirs`) and a line `>>>>>>> change`. There is
never a `||||||| base` section, whatever `merge.conflictStyle` a git
configuration sets. Only a `content` entry is marked: a file both sides
added with different content, which has no `base`, is `add_add` and
answered `409` `not_text_conflict` (below).

```text
one
<<<<<<< target
TARGET
=======
CHANGE
>>>>>>> change
three
four
```

The report is the one [`GET …/conflict`](#get-apiv1workspacesslugchangesnumberconflict)
answers, taken by the same rule (stored for the present pair, replayed
otherwise), and the route needs the same scope and access, resolves the
change the same way, confines a token bound to a fork the same way and
refuses with the same errors (`409` `conflict` for a `draft` or `open`
change whose target branch does not exist). After the change is resolved:

| Status and `error_type` | When |
|---|---|
| `400` `invalid_request` | the query has no `path`, or an empty one (`path: must name a file of the change's conflict report`) |
| `404` `path_not_found` | `path` is no entry's `path` in the current report, which every path of a `clean` change is (`<ref> has no conflict in "<path>"`) |
| `409` `not_text_conflict` | the entry's `kind` is not `content` (`<ref> conflicts in "<path>" as <kind>, which has no text to mark`) |

## Stacks and restack

A new version of a change records, in the same store transaction, a
pending restack of each `draft` or `open` change directly above it (its
`dependants`), and the push or commit that made the version is answered
without waiting for them; one already built on the new head ends without
a version (see below). When
the version cannot record them, the version is not recorded either. A
change has at most one pending restack: a newer one replaces it, so that
the change is restacked once, onto the newest head below.

When a change lands (see [Landing](#landing)), once the land is recorded
each `draft` or `open` change directly above it, in one store transaction
of its own, gets `depends_on` `null`, keeps the landed commit as the commit it must
sit on, and gets a pending restack onto that commit, with the trigger
`land`; a change without dependants records nothing. The changes further
up keep their `depends_on` and are restacked in turn onto the new head of
the change directly below them. An abandon with `"dependants":
"reparent"` records the restack of each reparented dependant, with the
trigger `abandon` (see [Modifying a change](#modifying-a-change)). A
`depends_on` a caller sets at open or by `PATCH` on a change whose head
does not have the head of the change it names records, in the same
transaction, a pending restack of the change onto that head, with the
trigger `manual`; the hub runs it as any other.

A change deeper in its stack than `[changes].max_stack_depth` gets no
pending restack, and a restack recorded before it sank that deep is
removed without running when its turn comes; since it gets no version,
the changes above it get none either. When a link below it changes so
that it is back within the limit, its `stack_too_deep` is cleared and,
when it does not sit on the head of the change below it (or the commit
it must sit on), it gets a pending restack with the trigger `version`.

While the workspace's setting `changes.auto_restack` is `false`, none of
these records a pending restack and no branch moves: each change that
would have one gets `needs_restack` `true` with `restack_reason`
`auto_restack_off` when it does not sit on the commit it must (a change
already on it is left as it is), and keeps its version and its source
branch. A change below it that lands still leaves it with `depends_on`
`null` and the landed commit as the commit it must sit on.

The hub runs the pending restacks in the background, one at a time, in
the order they were recorded; those pending when `parleyd` stopped run
after it starts again. A restack for a new version below (`version`) or
for a caller's link (`manual`) of a change that, when its turn comes, no
longer depends on a `draft` or `open` change and has no landed commit to
sit on (its `depends_on` was cleared, or a version of its own left it
unlinked) is removed without running: no version, no event and no branch
move, and the change's mergeability is checked. A restack replays the commits of the change after
its `base_sha` up to the head of its source branch, in order, onto the
commit it must sit on: the head of the change it depends on while that
one is `draft` or `open`; else the landed commit of the change below it;
else the head of its target branch. The replay runs in the repository's
object store (no work tree and no ref), each commit with its author and
message and the hub as committer, so that a change below that was
force-pushed to an unrelated head, landed or abandoned does not bring its
old commits along. When the source branch already sits on that commit
(it has the commit, and the commit has the change's `base_sha`), nothing
is recorded and no event is written. Otherwise the
source branch is moved to the result with a compare-and-swap on the head
the restack read; when it moved in between, or the change got a version
or another `base_sha`, the restack starts again from the new head, at most
5 times in one run, after which nothing is changed and the restack stays
pending. A replay that leaves the head as it is, as it does once the
change was pushed onto that commit, records nothing, as for a change that
already sits on it.

The move is recorded as the change's next version, of kind `restack`,
created by the hub (`created_by.actor_type` `system`), with the session of
the version it replaces, `base_sha` the commit it was restacked onto and
`patch_equivalent_to` the earlier version with the same diff, as any
version has it. `needs_restack` is cleared unless its reason is
`stack_too_deep`, and the version restacks the changes above it in turn,
bottom first. The hub writes, by `system`, `parley.change.version` with
`kind` `restack` and then `parley.change.restack` with `number`, `ref`,
`version`, `head_sha` (the dependant's new version and head), `onto_sha`
(the commit it was restacked onto) and `trigger` (`version`, `land`,
`abandon` or `manual`). A restack whose source branch already sat on that commit
clears a
`needs_restack` set for `conflict` or `auto_restack_off`. A change that
leaves a `conflict` this way, by a restack or by a version (below), is no
longer in conflict: the next conflict of a restack writes
`parley.change.conflict` again.

A restack whose replay conflicts moves no branch, records no version and
records no restack of the changes above it, which keep their versions and
their `depends_on`. In one store transaction its pending restack is
removed, the change gets `needs_restack` `true` with `restack_reason`
`conflict`, and then `mergeable` `conflict` with the conflict report, as a
mergeability check stores them (see [Mergeability](#mergeability)): when
the change was not in conflict already, the hub writes
`parley.change.conflict` with `needs_restack` `true`; a later conflict
replaces the report and writes no event. The report has the members
`target_sha` (the commit the change was replayed onto), `head_sha` (the
change's head), `base_sha` (the change's `base_sha`), `commit_sha` (the
first of the change's own commits that did not apply), `files` (each with
`path`, `kind`, `base`, `ours`, `theirs` and `landed_by`),
`landed_changes`, as in [a land's conflict report](#landing), and `change`
`null`. A mergeability check of the
change that was scheduled before the restack stored the conflict keeps
that report. When the change gets a version or the commit it must sit on
moves while the replay runs, the restack starts again instead.

A version recorded for a change whose `needs_restack` is set for
`conflict` or `auto_restack_off` clears `needs_restack` and
`restack_reason` when its head has the commit the change must sit on: the
head of the change it depends on; with none, the landed commit of the
change that was below it, or else the head of its target branch. A
version that leaves a linked change with no `depends_on` clears them too,
together with the link. Any other version leaves them as they are.

A restack that fails for another reason
than a conflict (the `git` command cannot be started or exits with an
error, or the repository cannot be read) changes nothing; it is logged
once, at level `ERROR`, as `the restack failed` with `workspace`,
`number` and `error` (its text scrubbed of credentials), stays pending
until a newer restack of the change replaces it, a version of the change
itself records it again (so that it runs again, and its mergeability is
checked once it ends), or `parleyd` starts again, and the hub goes on with
the next pending restack. A pending
restack whose change is no longer `draft` or `open` when its turn comes
is removed, and nothing else happens: no version, no event and no branch
move.

### Manual restack

`POST /api/v1/workspaces/{slug}/changes/{number}/restack` restacks the
change `{number}` of `{slug}` on demand, as the hub restacks it in the
background, and answers with the outcome in the same call. It is checked
as the routes of [Modifying a change](#modifying-a-change) are, in the
same order: `changes:write` and read access to `{slug}` (`403`
`insufficient_scope`, `404` `not_found`), the change (`404`
`change_not_found`, a change outside the fork of a token bound to a task
fork included), the caller (its author with any credential of theirs, a
caller who writes `{slug}`, or the admin token; anyone else who reads
`{slug}` gets `403` `forbidden`), then the body, then the state of
`{slug}` (`409` `workspace_busy`, `workspace_not_ready`,
`workspace_archived`). The body is absent or a JSON object; its member
`resolutions`, when present, is a list of resolutions (below), and its
member `session_id`, a string, must pass the session-linking rule (`400`
`unknown_session`); both are checked before any replay, and other members
are ignored. A change that is `landed` or `abandoned` is
answered `409` `change_closed`, and a change with `depends_on` `null`
whose target branch does not exist `400` `target_not_found`; neither
changes anything.

Each resolution is an object with a `path` and exactly one of
`content` (the file's text), `content_base64` (its bytes in standard
base64) and `delete` (`true`: no file at the path):

```json
{"resolutions": [
  {"path": "app.txt", "content": "one\nMERGED\nthree\n"},
  {"path": "logo.bin", "content_base64": "AAEC"},
  {"path": "old.txt", "delete": true}
]}
```

The list is refused as a whole with `400` `invalid_request`, before any
replay and with nothing moved, when `resolutions` is not a list (`null`
included), an entry is not an object, a `path` is missing, not a string,
empty, starts with `/` or has an empty, `.` or `..` segment, an entry has
none or more than one of `content`, `content_base64` and `delete`,
`content` or `content_base64` is not a string, `content_base64` is not
standard base64 (padding bits set and line breaks are refused too),
`delete` is not `true`, or two entries name the same path. An empty list
is a restack without resolutions.

The restack replays the change's commits onto the commit it must sit on,
as above. At a commit that does not apply, when every conflicted path of
that commit (each `path` of the report's `files`) has a resolution, the
commit is replayed as git merged it with each conflicted path replaced by
its resolution's bytes, or with no file there for `delete`, the paths
its other sides name gone too (both paths of a file the two sides renamed
leave only the resolution at the target's path), and the
replay goes on; a path that conflicts at several commits gets its
resolution at each. A resolved file has its mode in the change's commit,
else its mode in the target as replayed so far, else `100644`; it takes
the place of whatever is at its path, a directory too, and `delete`
leaves a directory at its path in place. A resolution whose path does not
conflict at a commit changes nothing there, and a commit that a
resolution leaves with no change of its own is left out, as any replayed
commit that changes nothing. A conflicted path without a resolution ends
the restack as a conflict, below. The change's pending restack, if it has one, is taken over and
removed when the restack ends. A change too deep in its stack is restacked
too and keeps `needs_restack` with `restack_reason` `stack_too_deep`. It
answers:

- `200` `{"status": "restacked", "change": <change object>}` when the
  source branch moved: the version is of kind `restack`, created by the
  caller (`created_by`, and the caller's credential as its creating
  credential), in the body's `session_id` or else the session of the
  version it replaces; `needs_restack` is cleared unless its reason is
  `stack_too_deep`; and the hub writes `parley.change.version` and then
  `parley.change.restack` with the trigger `manual`, both with the caller
  as actor.
- `200` `{"status": "up_to_date", "change": <change object>}` when the
  change already sits on that commit, whatever `resolutions` says: no
  version and no event; a
  `needs_restack` set for `conflict` or `auto_restack_off` is cleared and
  the change's mergeability is computed again.
- `409` `rebase_conflict` with the conflict report as the envelope's
  `details` when a commit does not apply and one of its conflicted paths
  has no resolution: the change is left as a
  conflicting background restack leaves it (`needs_restack` for
  `conflict`, `mergeable` `conflict` with the report,
  `parley.change.conflict` unless it was in conflict already), and nothing
  moves.
- `409` `ref_moved` with `details.current_sha`, the source branch's head
  (`null` when it is gone), when the branch moved before each of 5
  compare-and-swaps; nothing is recorded.
- `400` `merge_commits_unsupported` when a merge commit is among the
  change's own commits; nothing moves.

A version guard, when the hub has one, judges the version a manual
restack would record, once per attempt, after the replay and before the
source branch moves, with the caller's credential and paths and the
version's base (the commit sat on) and head; its refusal, such as `403`
`path_not_allowed` with the `path`, is the answer, and nothing changes.
It is not asked for an `up_to_date` answer, a conflict or a restack of
the hub. The hub's guard holds a path-scoped token to its paths: a file
between the base and the replayed head, both paths of a rename, that
matches none of its globs is answered `403` `path_not_allowed` with that
file as `error.path`; the source branch keeps its head and no version is
recorded. A credential without paths passes.

A restack that fails for another reason answers `500` `internal`.

### The stack of a change

`GET /api/v1/workspaces/{slug}/changes/{number}/stack` answers `200` with
the stack the change `{number}` of `{slug}` is part of, checked as every
[read of a change](#reading-a-change) is (`changes:read` and read access
to `{slug}`; `404` `change_not_found` for a change the request cannot
name):

```json
{"target_branch":"main","changes":[{"number":1,"title":"Add low","state":"open","version":2,"head_sha":"…","mergeable":"clean","needs_restack":false,"restack_reason":null,"landing":{"status":"ready"}},{"number":2,"title":"Add mid","state":"open","version":2,"head_sha":"…","mergeable":"clean","needs_restack":false,"restack_reason":null,"landing":{"status":"ready"}}]}
```

`target_branch` is the change's target branch. `changes` lists, bottom
first, the chain of `depends_on` links below the change down to the change
that depends on nothing, the change itself, and every `draft` or `open`
change reachable upward from it through `dependants`, ordered by depth
from the bottom and then by number, so that where a change has several
dependants all are listed; `abandoned` and `landed` changes above it are
left out. Every change of a chain gives the same list; where a change has
several dependants, asking for one of them lists the chain below it and
the changes above it, not the other branches. Each summary has exactly `number`, `title`,
`state`, `version`, `head_sha`, `mergeable`, `needs_restack`,
`restack_reason` and `landing` with its `status` alone. A token bound to a
task fork, calling on the fork's parent, sees only the changes whose
source is that fork. The `stack_of` filter of
[the list](#get-apiv1workspacesslugchanges) walks the stack in every
state instead.

## Landing

Landing adds a change's commits to its target branch. Every request to
land a change, or a stack of changes, is recorded as a land attempt,
which is kept for the life of its workspace: the teardown that deletes a
workspace deletes its attempts. `POST …/changes/{number}/land` creates
an attempt and answers it; three routes read the attempts.

The land attempt object:

```json
{"id":"0f9a7f8e-5c1b-4d6e-9a3b-1c2d3e4f5a6b","workspace_slug":"demo","target_branch":"main","changes":[1,2],"status":"landed","trigger":"manual","strategy":"rebase","attempts":1,"onto_sha":"dc54adf9e0607cad9039738e34a4c3061a859f17","landed_sha":"5e3d0c1a2b4f6e8d0a1c3e5f7a9b1d3f5e7a9c1b","noop":false,"commits":[{"change":1,"version_sha":"0b86db2bc73da0f067ec404140cfc00060162383","landed_sha":"7c1e9a2b3d4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b"},{"change":2,"version_sha":"1f2e3d4c5b6a7980a1b2c3d4e5f6a7b8c9d0e1f2","landed_sha":"5e3d0c1a2b4f6e8d0a1c3e5f7a9b1d3f5e7a9c1b"}],"conflict":null,"blocked":null,"error":null,"override":null,"policy_revision":0,"requested_by":{"actor_type":"api_key","actor_id":"bce9bb4d-cbcb-47c3-ab7a-d18f4b5ab95a","username":"owner"},"session_id":null,"created_at":"2026-11-16T09:00:00Z","finished_at":"2026-11-16T09:00:01Z"}
```

Every member is always present, `null` where the attempt has no value.

| Member | Meaning |
|---|---|
| `id` | the attempt's UUID |
| `workspace_slug`, `target_branch` | the workspace whose branch the attempt lands on, which holds the changes, and the branch |
| `changes` | the numbers of the changes the attempt lands, bottom first. The attempt lands the version each change had when the attempt was created; a version that arrives later is not landed by it |
| `status` | `running`, then one of the final `landed`, `conflict`, `blocked` and `failed`. A final attempt never changes, and reads the same on every later request |
| `trigger` | `manual`, or `auto_land` for the automatic landing of a change |
| `strategy` | `rebase` or `squash` |
| `attempts` | the tries to move the target branch |
| `onto_sha`, `landed_sha` | the target head the commits were landed on and the head the land made it |
| `noop` | `true` for a land whose commits were all on the target already |
| `commits` | each landed commit: the `change` it belongs to, the commit of its version (`version_sha`) and the commit it landed as (`landed_sha`); `[]` for none |
| `conflict` | the conflict report of an attempt that ended `conflict`, in the form [the comparison](#get-apiv1workspacesslugscmcompare) gives it, with its landed changes attributed (below) |
| `blocked` | the change whose landing requirements were not met: `change`, `status` and `requirements` (never `null`) |
| `error` | why an attempt ended `failed`: `contention`, `timeout`, `origin_rejected` or `internal` |
| `override` | `{"reason": …}`, the override the attempt applied |
| `policy_revision` | the revision of the policy that judged the attempt, `null` until it was judged |
| `requested_by` | who asked for the land: `actor_type` (`admin_token`, `api_key` or `pat`), `actor_id` and `username`, both `null` for the admin token |
| `session_id` | the session the land was asked for in |
| `created_at`, `finished_at` | when the attempt was created and when it ended, `null` while it runs |

An attempt with the strategy `rebase` replays each change's own commits,
those of its version that are not in its base, in order onto the head of
the target branch it reads (`onto_sha`). A commit whose parent already is
that head is used as it is, so a version made on the target head lands
with its own commit ids; a commit whose change the target already holds is
left out and has no `commits` entry; every other commit is made anew with
the author, author date and message of the commit it replays and the
landing identity as committer, `Parley <parley@localhost>` unless
`[land].committer_name` and `[land].committer_email` name another
([configuration.md](configuration.md)).
The target then moves from `onto_sha` to `landed_sha` with a
compare-and-swap, which is not a push: the push rules, a protected branch
included, do not apply. When another write moved the target since it was
read, the attempt reads the new head, replays the changes onto it and
tries again; `attempts` counts every try, and `onto_sha` is the head the
try that moved the target was built on. After `[land].max_attempts` lost
tries (10 unless the configuration says otherwise) the attempt ends
`failed` with `error` `contention`, nothing of it in the target and
the change still `open`, so a new land may follow. A target branch that
does not exist when the attempt reads or moves it, or a policy gate,
store or git failure other than a conflict, a policy gate or override
rule that fails or panics, and a change version without a base (one whose
history the target does not share, `base_sha` `null`), ends the attempt
`failed` with `error` `internal`; a land never creates its target branch.
So does a change that is no longer `open` when the target is about to
move, abandoned or set back to `draft` while the attempt ran: the target
does not move and the change keeps its state. A change abandoned in the
moment after that check, while the target moves, is `landed` all the
same, since the target holds it then. The
move is followed by one store transaction
that marks each change `landed` (`landed` = `{sha, at, by, land_id,
commits}`, every landed commit then found by the change's by-commit
lookup), makes the attempt final and writes one `parley.change.land`
event per change: resource `change` `{number}`, the requester as actor
(the hub, `actor_type` `system` with `actor_id` `null`, for an attempt
with `trigger` `auto_land`),
the attempt's `session_id`, and metadata `land_id`, `target_branch`,
`onto_sha`, `landed_sha`, `strategy`, `attempts`, `noop`, `trigger`,
`override`, `policy_revision`, `number` and `ref`. One `parley.git.push`
event follows for the target ref, with `via` `land` and `direct` `false`.
When every commit was left out, the attempt ends `landed` with `noop`
`true`, `landed_sha` and `onto_sha` both the unchanged target head and
`commits` `[]`; the change is `landed` at that head, `parley.change.land`
says `noop` `true`, and no `parley.git.push` is written.

An attempt with the strategy `squash` replays each change's own commits
the same way, then makes of each change one commit on the head it was
replayed onto: its tree is the one the replay ended at, its author the
author of the version's head commit, with its author date and that date's
time zone, its committer the landing identity,
and its message the change's title, a blank line, its intent and a blank
line (both left out for a change without an intent), and the trailers:
`Parley-Change: <slug>#<number>`, then `Parley-Session: <id>` when the
change has a session, then one
`Co-authored-by: Name <email>` per other author of the change's own
commits, in the order they first wrote one. The change's one `commits`
entry has `version_sha` the version's head. A change whose replay leaves
the target's tree as it was gets no commit and no entry.

A stack land covers the change and every change below it that is not
`landed`, bottom first in `changes`; a change below that is already
`landed` is left out and keeps its `landed`. It replays them bottom first,
each onto the result of the one below, and moves the target once, to the
top; with `squash` each change becomes one commit, in order. Every
covered change is then `landed` by the attempt, with `landed.sha` the new
target head and its own `commits`, and gets its `parley.change.land`
event; one `parley.git.push` is written. A conflict, or a gate answer
that is not `ready`, in any covered change ends the whole attempt with
nothing landed: the target stays where it was, every covered change stays
`open`, `conflict.change` or `blocked.change` names the change that
stopped it, and that change alone gets `mergeable` `conflict` and the
`parley.change.land_fail` event. The report's `target_sha` is then the
head the attempt read, and `ours` of each file the side as replayed with
the changes below.

Before it replays anything the attempt asks the policy gate about each
change it covers, once and bottom first, with the workspace, the change,
the version being landed with its head and base commits, and the caller,
so that a change that got a newer version while the attempt waited is
judged at the version the attempt lands; a retry of the compare-and-swap
asks nothing again. The gate is the landing policy of the change's target
workspace. It reads the change and judges the version being landed in one
transaction, so a version pushed meanwhile is never judged in its place.
When the version being landed is still the change's current version, it
evaluates the change again as
[The landing of a change](#the-landing-of-a-change) says: it stores the
change's `landing`, announces the change when it is landable, and answers
that landing. When the change has a newer version by then, it evaluates
the files of the version being landed, from its base to its head, against
the current revision, with that version's approvals and check results and
no structural cause, and stores nothing. A workspace that never
set a policy is at revision 0, whose default requires nothing, so its
changes are `ready` unless they are drafts or a structural cause blocks
them. A change abandoned or landed since the attempt was asked fails the
attempt. `policy_revision` is the revision that answered, the highest when
the answers differ. An answer `pending` or `blocked` on a
request that carries an `override` is put to the override rule, and one
the rule lets pass counts as `ready`; the attempt, and the metadata of
its `parley.change.land` events, show `override` `{"reason": …}` only when
the rule let the answer of at least one change pass, `null` otherwise.
The hub's override rule lets an answer `pending` pass, a change held back
by approvals or checks, and never one `blocked`, by a `block` requirement
or a structural cause. The first change,
bottom first, whose answer is neither `ready` nor let pass ends the
attempt `blocked`, with `blocked` the `change`, its `status` and its
`requirements`, before anything is replayed or moved: the target stays
where it was and the change `open`.

A commit of a change that does not apply on the target head ends the
attempt `conflict`, the target where it was and the change `open`. Its
`conflict` report gives `target_sha`, the head the attempt replayed onto,
`head_sha` and `base_sha` of the version it landed, `commit_sha`, the
first commit that did not apply, the conflicted `files`, and `change`:
`null` for the land of one change, the number of the change whose commit
did not apply in a stack land. `landed_changes` lists, once each and by
number, each change landed between `base_sha` and `target_sha` that
changed a path of a conflicted file, as `{"number": …, "title": …,
"landed_sha": …, "session_id": …}` (its `landed.sha` and its session), and
each file's `landed_by` the numbers, ascending, of those that changed it;
a commit no change landed, such as a direct push, names none. Both are
`[]` when none did, when `base_sha` is `null` or not an ancestor of
`target_sha`, and when the landed history cannot be read; the report is
answered all the same. In the
transaction that ends the attempt the change gets `mergeable` `conflict`
with the same report, as a mergeability check stores it, with
`parley.change.conflict` when it was not in conflict already. A
mergeability check of the change that was scheduled before the land
stored the conflict keeps that report while the change's head and its
target's head are still the ones the land replayed, also for a change in
a stack, whose check rebases it onto the change below; the next trigger
computes its mergeability again.

An attempt that ends `conflict`, `blocked` or `failed` writes one
`parley.change.land_fail` event in the transaction that ends it: resource
`change` `{number}` of the change that stopped it (the one its conflict
report or `blocked` names, else the top change), the requester as actor
(the hub, `system`, for an attempt with `trigger` `auto_land`), the
attempt's `session_id`, and metadata `land_id`, `status`, `reason`,
`number` and `ref`, with `files`, the conflicted paths (the first 100,
and `truncated` `true` when there were more), for a conflict. `reason` is
`conflict` for a conflict, the gate's or the target guard's reason for a
block or `policy` when it gave none, and the `error` of a failure
(`contention`, `timeout`, `origin_rejected`, `internal`). A landed attempt
writes none.

A feature installed on the hub may guard the move of a target branch. Such
a target guard is asked after the changes are replayed and before the
first compare-and-swap, with the workspace, the target branch, its head
before the land and the head the land would make it; a retry on a target
that moved meanwhile asks it again about the head rebuilt on the new one,
before that head is swapped in. A guard that refuses ends the attempt
`blocked`, with `blocked` `{"change": <the top change>, "status":
"blocked", "requirements": []}` and the guard's reason as the `reason` of
`parley.change.land_fail`; a guard that reports a rejection by origin ends
it `failed` with `error` `origin_rejected`; a guard that cannot decide ends
it `failed` with `error` `internal`. In each case the target does not move
and the change stays `open`. The hub installs one, carry-patch push
control, which judges a land onto a patch branch of a `carry_patch`
workspace (see [Pushes to the integration branch and patch
branches](#pushes-to-the-integration-branch-and-patch-branches)); every
other move is allowed.

Once a land is recorded, a land that moved nothing included, the hub does
its follow-up, each step on its own: a step that fails is logged and
changes neither the attempt, nor the answer to the call, nor the steps
after it. The answer to the call waits for the follow-up, which takes no
worker of the pool: a follow-up that takes long delays no other land.

- The source branch of each landed change is deleted, in the change's
  source workspace, when the target workspace's setting
  `land.delete_source` is `true` (its default), the branch is still at the
  head of the version that landed and no other `draft` or `open` change
  has it as its source. A branch that moved since keeps the work that did
  not land and is left as it is, as is the default branch of the source
  workspace. The branch is deleted directly with a compare-and-swap, not
  as a push: no push rule applies and no `parley.git.push` is written; the
  changes whose target is the deleted branch get a mergeability check. A
  change opened from the branch while it is being deleted either keeps it
  or is refused with `source_not_found`.
- Every other `draft` or `open` change whose target is the landed target
  branch gets `mergeable` `unknown` and a mergeability check.
- Each landed change's `draft` or `open` dependants are unlinked from it
  and get a pending restack onto the commit the target held once it
  landed (see [Stacks and restack](#stacks-and-restack)), and a task fork
  that is the source of a landed change records an activity, from which
  its early expiry counts.
- On a `carry_patch` workspace, a land that moved a patch branch queues a
  rebuild with `submitted_by` `system:land` and mirrors the branch to
  origin, as the settings say (see [Pushes to the integration branch and
  patch branches](#pushes-to-the-integration-branch-and-patch-branches)).
  This step runs on its own: the answer to the call does not wait for it.

A land changes only the hub's own repositories and contacts no forge, and
origin only where carry-patch push control forwards or mirrors a patch
branch: publishing any other landed branch is a push to origin of its own.

Attempts run side by side, with no queue and no lock per workspace or
branch: two attempts meet only in the compare-and-swap. At most
`[land].workers` attempts (8 by default) are computed at once; a further
attempt waits for a free worker, reads `running` meanwhile, and none
fails because every worker is busy; waiting attempts start in no
promised order. An attempt that is not final `[land].timeout` (5 minutes
by default) after it was created, or after a start ran it again,
waiting for a worker included, is stopped, every `git` process of its work killed, and ends `failed` with
`error` `timeout`, its change still `open`; it never moves the target
afterwards. An attempt whose target has already begun to move when its
time runs out ends as that move ends: `landed` when the target moved,
`failed` `timeout` when it did not. A stop of the hub stops the attempts it runs without
ending them: they stay `running`, and the next start runs every attempt
still `running` again, while it serves requests, until each is final.
Such an attempt keeps its `id`, `created_at`, `trigger`, `session_id` and
`requested_by`, who is the actor of its events, and lands the versions
it was created with, by its strategy, with its override asked again. It
runs from the preconditions: one that no longer holds, such as a change
that is no longer `open`, ends it `failed` with
`error` `internal`, the change left as it is, and a target branch that is
gone ends it the same way; otherwise it ends `landed`, `conflict` or
`blocked` as a new attempt would.

The move and its record cannot share one transaction, so the hub enters
the move in a journal first. A hub that stops at any point of a land
comes up, before it serves a request, in one of two states: the target at
its old head, the change `open`, no `parley.change.land` event and the
attempt not `landed`; or the target at the new head, the change `landed`
with every landed commit mapped, the attempt `landed` and the land's
events written once, also when other writes moved the target on after
the move. A land the start completes gets its follow-up (above)
then; a land recorded before the stop does not get it again, so a stop
during its follow-up leaves the rest of it undone. A journal entry the
start cannot complete, such as one whose repository cannot be read, is
kept for the next start, and its attempt stays `running` and is not run
again until the entry is completed.

The three reads need the scope `changes:read` and read access to
`{slug}`. Both lists are [lists](#lists) ordered newest first: by
`created_at` at the whole second it shows, then by `id`, both descending.
A token bound to a task fork, calling on the fork's parent, reaches
`GET …/changes/{number}/lands` for a change whose source is the fork and
`GET …/lands/{id}` for an attempt one of whose changes comes from the
fork; another change or attempt is answered `403` `token_bound` (the
change routes answer such a change `404`), an unknown number or id keeps
its `404`, and `GET …/lands` is always `403` `token_bound`.

### `POST /api/v1/workspaces/{slug}/changes/{number}/land`

Lands the change `{number}` of `{slug}` on its target branch, or, with
`stack` `true`, the change and every change below it down its
`depends_on` that is not `landed`, bottom first. It is a create route
(see [Idempotency](#idempotency)). The body is absent or a JSON object;
members not listed are ignored.

| Member | Value |
|---|---|
| `expected_version` | an integer of at least 1, the version the change must be at |
| `stack` | a boolean, `false` when absent |
| `override` | an object whose `reason` is a string that is not blank, kept with the attempt and put to the override rule for a change the policy gate does not answer `ready`: a change whose landing is `pending` passes, one `blocked` never does. Only the owner of `{slug}` with an API key or a PAT not bound to a workspace, and the admin token, may send it, and only while `{slug}`'s setting `land.allow_override` is `true` (check 8) |
| `session_id` | a string, the session the land is asked in |

The query parameter `wait`, a whole number of seconds from 0 to 60 (30
when absent), is how long the call waits for the attempt's end.

The checks, in this order, each refusal creating no attempt and moving
nothing:

1. the scope `changes:write` (`403` `insufficient_scope`, whether or not
   `{slug}` exists) and read access to `{slug}` (`404` `not_found`, with
   the same body as for a workspace that does not exist);
2. the change: a `{number}` that is not a positive integer or names no
   change of `{slug}` is `404` `change_not_found`;
3. the body and `wait`: `400` `invalid_request`, such as
   `expected_version: must be an integer of at least 1`, `stack: must be a
   boolean`, `override: must be an object whose reason is a string that is
   not blank`, `session_id: must be a string` or `wait must be a whole
   number of seconds from 0 to 60`;
4. a token bound to a task fork, calling on the fork's parent, lands only
   changes whose source is the fork, a whole stack included: `403`
   `token_bound` otherwise;
5. who may land: the admin token, the owner of `{slug}`, and, while the
   workspace setting `land.by_author` is `true` (its default), the
   change's author with any credential of theirs, such as an org member
   landing the change they opened from their fork; with `stack`, the
   author of every change the land covers, so that a change below by
   someone else makes it `403`. Anyone else who reads `{slug}` gets `403`
   `forbidden`;
6. the state of `{slug}`: `409` `workspace_archived`, `workspace_busy` or
   `workspace_not_ready`;
7. `session_id` must pass the session-linking rule: `400`
   `unknown_session` otherwise (see [Sessions](#sessions));
8. the request guards a feature installed on the hub registers, each asked
   with the caller, `{slug}`, the changes the land would cover (bottom
   first, each at its current version) and the `override`: the first that
   refuses gives the answer as it gives it, its status, `error_type` and
   members such as `path` (for example `403` `path_not_allowed`), and the
   guards after it are not asked. The hub has two, in this order:
   1. a path-scoped token's paths: every file of the current version of
      each change the land would cover, bottom first, from its base to its
      head, both paths of a rename, must match one of its globs; the first
      that matches none is `403` `path_not_allowed` with `error.path`
      (`path <path> is outside this token's allowed paths`), whatever the
      policy says and also while another attempt runs;
   2. an `override`, whatever the policy status of the changes, is `403`
      `override_not_allowed` for a PAT bound to the workspace or to one of
      its forks (`a token bound to <workspace> may not land with an
      override`), then for a caller that is neither the owner of `{slug}`
      nor the admin token (`only the owner of <slug> or the admin token may
      land with an override`), then, for every caller, while `{slug}`'s
      `land.allow_override` is `false` (`land.allow_override is off in
      <slug>`);
9. the preconditions, read in the transaction that creates the attempt,
   the first that fails being the answer:
   1. every change the land covers is `open`: `409` `change_not_open` for
      a `draft`, `landed` or `abandoned` change;
   2. `expected_version`, when given, is the change's current version:
      `409` `version_mismatch`;
   3. no `running` attempt covers a change the land covers: `409`
      `land_in_progress`, with that attempt in `error.details`. Of several
      lands of one change that arrive at the same moment, exactly one
      creates an attempt and the others get this answer;
   4. without `stack`, every change down the change's `depends_on` is
      `landed`: `409` `depends_on_unlanded`;
   5. the changes the land covers, each at its version, are those the
      checks 4, 5 and 8 judged: a change that got a new version, or a
      stack whose changes changed, since then is `409` `version_mismatch`.

   Only `land_in_progress` carries `details`.

In that transaction the hub creates the land attempt, `running`, with the
link of its session: `trigger` `manual`, `strategy` the workspace's
`land.strategy` at that moment, `attempts` `0`, `requested_by` the
caller, and each change at its current version. The attempt runs on the
hub's pool; nothing is locked while it runs, so the lands of different
changes, also on one target branch, run side by side and meet only in the
compare-and-swap. The call waits up to `wait` seconds for the attempt's
end and answers it in the form of its status once it is final: `200`
with the land attempt when it is `landed`, `409` `land_conflict` when
`conflict`, `409` `policy_blocked` when `blocked` and `503` `land_failed`
when `failed`, each error with the land attempt in `error.details`; or
`202` with the attempt still `running` when the wait runs out, `wait=0`
answering at once. The attempt goes on whatever becomes of the call: a
caller that goes away stops nothing, and `GET …/lands/{id}` gives its
final form later.

### `GET /api/v1/workspaces/{slug}/changes/{number}/lands`

Lists the attempts that cover the change `{number}` of `{slug}`, those of
a stack it is part of included; `items` is `[]` for a change with none.
A `{number}` that is not a positive integer, or that `{slug}` has no change
of, is `404` `change_not_found`.

### `GET /api/v1/workspaces/{slug}/lands`

Lists the attempts whose target is `{slug}`, narrowed by every filter the
query gives:

| Parameter | Keeps |
|---|---|
| `status` | the attempts in that status; repeatable, any of the values given |
| `target_branch` | the attempts on that branch |
| `since` | the attempts created at or after that RFC 3339 timestamp |

A task fork holds no changes, so its attempts are never listed on its
parent. `400` `invalid_request` answers a `status` that is not one of the
five (`status must be one of running, landed, conflict, blocked, failed`),
a `since` that does not parse (`since must be an RFC 3339 timestamp`), a
`limit` outside 1 to 200 and a cursor that is malformed or that a query
with other filters issued.

### `GET /api/v1/workspaces/{slug}/lands/{id}`

Answers `200` with the land attempt `{id}` of `{slug}`. An `{id}` that is
not a UUID, that names no attempt, or that names an attempt of another
workspace is `404` `land_not_found`, with the message `<slug> has no land
attempt "<id>"`.

### Automatic landing

A change with `auto_land` `true` is landed by the hub itself once it is
landable: `open`, its `landing.status` `ready` and its `mergeable`
`clean`. The hub follows the `parley.change.ready` events of its log,
which the change's landable announcement writes once each time the change
becomes landable (see [The landing of a change](#the-landing-of-a-change)),
from a position it stores, so each event starts at most one attempt and
none is lost over a restart. It also starts one when `auto_land` is turned
on for a change that is landable already, and, at start-up, for every open
change with the flag on and no `running` attempt.

When it handles a change it starts nothing, and leaves the flag as it is,
while the change is not `open`, the flag is off, the change is not
landable, an attempt for it is `running`, or its `depends_on` names a
change that is not `landed`. Otherwise it checks the user who set the flag
as of now: the admin token always may; a user must exist, not be blocked,
and own `{slug}` or, while `land.by_author` is `true`, be the change's
author; scopes are not looked at. When the flag was set with a
path-scoped token, every file of the change's current version must match
one of the globs stored with the flag, whether or not the token still
exists. When either check fails, no attempt is created, the flag is
cleared and one `parley.change.land_fail` is written on the change with
actor `system` and metadata `land_id` `null`, `status` `blocked`, `reason`
`not_permitted`, `number` and `ref`. Otherwise the hub starts a land
attempt for that one change, never a stack, with `trigger` `auto_land`, no
`override`, `session_id` `null`, `requested_by` the user who set the flag
(`actor_type` the type of the credential they set it with) and its events
written by `system`, as [`POST …/land`](#post-apiv1workspacesslugchangesnumberland)
creates one from check 9 on.

The outcome decides what follows:

- `landed`, `conflict` or `blocked`: nothing more; the flag stays as it
  is, and the next time the change becomes landable a new attempt starts.
- `failed`: a new attempt starts after 2 s, then, while each retry fails
  too, after 4, 8, 16, 32, 64, 128, 256, 300 and 300 s: at most 10 retries
  after the first failed attempt, each a new attempt. When the tenth retry
  fails as well, the flag is cleared and nothing more is started. The
  count starts again from zero after any attempt that does not end
  `failed`.

A retry that comes due while the change is not `open`, the flag is off or
the change is not landable is dropped and its count starts again from
zero. The count and the due time are stored, so a retry pending when the
hub stopped is made after the next start, at its due time or at once when
that time has passed. Clearing the flag writes no event of its own.

In a stack the change above waits: when the change below lands, the
change above, restacked by the hub when the target moved, or already
sitting on the landed commit, is announced landable again and its own
attempt starts once the lower attempt has ended. An automatic attempt
never lands more than its one change.

### `POST /api/v1/workspaces/{slug}/changes/{number}/revert`

Undoes the landed change `{number}` of `{slug}` as a change of its own.
It is a create route (see [Idempotency](#idempotency)). The body is absent
or a JSON object; members not listed are ignored:

| Member | Value |
|---|---|
| `title` | a string of 1 to 200 characters without a line break; defaults to `Revert "<title of the original>"`, cut to 200 characters |
| `intent` | a string of at most 16,000 characters; defaults to `Reverts change <slug>#<number>: <title of the original>.` |
| `draft` | a boolean; `true` opens the reverting change as a draft |
| `auto_land` | a boolean; `true` sets `auto_land` on the reverting change with the caller as the user who set it |
| `session_id` | a string, the session the reverting change is opened in |

The hub reads the head of the original's target branch and makes on it
one commit per landed commit of the original (`landed.commits`), newest
first, each undoing that commit, with the subject `Revert "<subject of
the undone commit>"`, the body `This reverts commit <full id>.` and the
landing identity as author and committer (`[land].committer_name`,
`[land].committer_email`); an undo that would change nothing is left
out. It creates the branch `revert/<number>` in `{slug}` at the last of
them, or the first free name of `revert/<number>-2`, `revert/<number>-3`
and so on, as an API ref creation: through the push rules and the ref
writer with `via` `api`, so the creation is one `parley.git.push` with
`created` `true`, and a push rule that refuses it is answered as for
[`POST …/scm/refs`](#source-control-writes). It then opens a change from
that branch into the original's target branch with `reverts` the
original's number, a first version of kind `revert`, the caller as author
and the body's members, which gets `parley.change.version` and, unless it
is a draft, `parley.change.open`, as any opened change does, and writes
`parley.change.revert` on the original, by the caller, with metadata
`reverted_by`, `number` and `ref`. The answer is `201` with the new
change as `GET …/changes/{number}` gives it; the original then shows it
as `reverted_by`. The reverting change is an ordinary change: policy
judges its files, and it lands through `POST …/land`. Reverting a
reverting change is allowed and, once landed, restores what the original
landed.

The checks, in this order; each refusal leaves nothing referenced: no
branch, no change, no event (the undo commits of a refusal that comes
after them may remain in the object store, unreferenced, until git's
garbage collection removes them):

1. the scope `changes:write` (`403` `insufficient_scope`) and read access
   to `{slug}` (`404` `not_found`); a token bound to a task fork is `403`
   `token_bound` on the fork's parent, since the revert is not a route it
   reaches there;
2. the change: `404` `change_not_found`;
3. the body: `400` `invalid_request` (`title: must be a string`, `draft:
   must be a boolean`, and the title and intent rules of an open);
4. who may revert: the admin token, the owner of `{slug}` and, while
   `land.by_author` is `true`, the original's author with any credential
   of theirs; any other reader is `403` `forbidden` (`<slug>#<number> may
   be reverted only by a writer of <slug>, the admin token or, while
   land.by_author is on, its author`);
5. `session_id` must pass the session-linking rule: `400`
   `unknown_session`;
6. the state of `{slug}`: `409` `workspace_archived`, `workspace_busy` or
   `workspace_not_ready`;
7. the change must be `landed`: `409` `change_not_landed`;
8. a `draft`, `open` or `landed` change that reverts it already: `409`
   `already_reverted`, with `error.details.reverted_by`; checked again
   when the reverting change is opened, so that of two reverts asked at
   once one is opened and the other answered so, its branch deleted
   again;
9. the original's target branch must still exist: `400`
   `target_not_found`;
10. a path-scoped token: every file of the landed version, from its base
    to its head, must match one of its globs: `403` `path_not_allowed`
    with `error.path`;
11. something to undo: `400` `nothing_to_land` when the original landed
    no commit (`noop`) or every undo would be empty;
12. every undo must apply on the target's head: `409` `revert_conflict`,
    with a conflict report in `error.details.conflict`, its `target_sha`
    the target head, `commit_sha` the landed commit that could not be
    undone, its conflicted `files`, `landed_changes` `[]` and `change`
    `null`.

With `auto_land` `true` a path-scoped token is also held to its paths by
the open, as in [`PATCH`](#modifying-a-change).

## Landing policy

A workspace's landing policy is a document of ordered rules that says what
each part of the repository needs before a change touching it may land.
Every replacement of the document is a new revision of it, numbered from
1 and kept with when and by whom it was written; the teardown that deletes
a workspace deletes its revisions. A workspace that never set a policy is
at revision 0, the never-set document: no rules, and a default that
requires nothing. Revision 0 is never stored. A task fork has no policy
of its own and is always at revision 0, whatever its parent's policy is.

The policy object:

```json
{"revision":2,"default":{"approvals":0,"approvers":["owner"],"checks":[],"fresh_checks":false,"block":false},"rules":[{"name":"all","paths":["**"],"exclude":[],"require":{"approvals":2,"approvers":["owner"],"checks":[],"fresh_checks":false,"block":false}}],"updated_at":"2026-11-16T10:00:03Z","updated_by":{"actor_type":"admin_token","actor_id":null,"username":null}}
```

| Member | Meaning |
|---|---|
| `revision` | the revision's number, `0` for the never-set document |
| `default`, `rules` | the document in normal form: every requirement member present, `exclude` `[]` where it was absent, and `targets` only on a rule that gave it |
| `updated_at` | when the revision was written, `null` at revision 0 |
| `updated_by` | who wrote it: `actor_type` (`api_key`, `pat` or `admin_token`), `actor_id` and `username`, the user's id and username, both `null` for the admin token; `null` at revision 0 |

The revision-0 object is
`{"revision":0,"default":{"approvals":0,"approvers":["owner"],"checks":[],"fresh_checks":false,"block":false},"rules":[],"updated_at":null,"updated_by":null}`.

### `GET /api/v1/workspaces/{slug}/policy`

Needs the scope `changes:read` and `read` access, so an org member, a
token bound to a task fork of `{slug}` (a contributor route) and any
reader of an archived workspace read it as the owner does. Answers `200`
with the policy object of the current revision and the header `ETag:
"<revision>"`, the number in double quotes. The query parameter
`revision` asks for an earlier revision: `200` with that revision as it
was written, and its number as the `ETag`, while it is kept; revision `0`
only while no policy was ever set (on a task fork, always). A revision
that is not kept, `0` after a policy was set, and any other revision of a
task fork, is `404` `not_found` with the message `no such policy
revision`, a number too large for any revision included; a value that is
not a non-negative integer in decimal digits
(empty, `-1`, `+1`, `1.5`, `abc`) is `400` `invalid_request` with the
message `revision must be a non-negative integer`.

### `PUT /api/v1/workspaces/{slug}/policy`

Replaces the workspace's policy with the document in the body. Needs the
scope `workspaces:write` and `owner` access: the workspace's owner or the
admin token. The access refusals come before the body is read: a token
without the scope is `403` `insufficient_scope`, an org member `403`
`forbidden`, a token bound to a task fork of `{slug}` `403` `token_bound`
(the route is not a contributor route), and a caller without access `404`
`not_found`. The route takes a body of up to 1 MiB whatever
`[server].max_body_size` says, so that a document over the format's limit
reaches the validator.

The body is the document, a JSON object:

```json
{"default":{"approvals":0},"rules":[{"name":"docs","paths":["docs/**","**/*.md"],"exclude":["docs/internal/**"],"require":{}},{"name":"billing","paths":["billing/**"],"targets":["main","release/*"],"require":{"approvals":1,"approvers":["owner","user:dana"],"checks":["test","billing-e2e"],"fresh_checks":true}},{"name":"secrets","paths":["deploy/secrets/**"],"require":{"block":true}}]}
```

| Member | Rule |
|---|---|
| the document | at most 65 536 bytes as sent |
| `rules` | a list of at most 200 rules, in the order they are selected; absent is `[]` |
| `default` | a requirement, the one of a file no rule selects; absent takes every member's default |
| `name` | matches `^[a-z0-9][a-z0-9_-]{0,62}$`, is not `default`, and no earlier rule has it |
| `paths` | a non-empty list of path globs, each 1 to 255 characters, not starting with `/` |
| `exclude` | a list of path globs as `paths`; absent is `[]` |
| `targets` | when present, a non-empty list of branch globs, each valid as a `protected_branches` entry; absent matches every branch |
| `require` | a requirement, required on a rule |
| `approvals` | an integer from 0 to 10; absent is `0` |
| `approvers` | when present, a non-empty list of `owner`, `member` or `user:<username>` naming an existing user by username, whatever its status; the value is compared with usernames only, exactly, never with ids or e-mails; absent is `["owner"]` |
| `checks` | a list of check names, each matching `^[a-z0-9][a-z0-9_.-]{0,62}$`; absent is `[]` |
| `fresh_checks`, `block` | booleans; absent is `false` |

The optional header `If-Match` names the revision the caller read, bare
(`If-Match: 2`) or in double quotes as the `ETag` gives it (`If-Match:
"2"`); without it the document replaces whatever revision is stored.

The refusals after access come in this order, and each changes nothing:

| Condition | Answer |
|---|---|
| `{slug}` is a task fork, whatever the body | `400` `not_supported_on_fork`, message `workspace "<slug>" is a task fork, which has no policy of its own; set the policy of its parent` |
| `If-Match` is not a non-negative integer, bare or in double quotes (`abc`, `-1`, `W/"1"`, several values) | `400` `invalid_request`, message `If-Match must be a policy revision, a non-negative integer, bare or in double quotes` |
| the body is not one JSON object | `400` `invalid_request`, message `request body must be one JSON object` |
| the document breaks a rule above | `400` `invalid_policy` naming the first violation (below) |
| the workspace is archived | `409` `workspace_archived`, message `workspace "<slug>" is archived` |
| `If-Match` names another revision than the stored one, a number too large for any revision included | `412` `revision_mismatch`, message `the policy has been replaced since the revision named in If-Match; read it again` |

A body over 65 536 bytes is refused as a document (`invalid_policy` with
the field `document`) even when it is not JSON. The `invalid_policy`
error carries `details` `{rule, index, field}`: `rule` is the rule's name,
`default` for the default requirement, or `null` for the document as a
whole and a rule without a usable name; `index` is the rule's zero-based
position, `null` outside a rule; `field` is the member at fault, prefixed
`require.` inside a rule's requirement and followed by `[i]` for one entry
of a list. For example, a first rule `billing` asking for 11 approvals is

```json
{"error":{"code":400,"message":"invalid policy: rule \"billing\" (index 0), field require.approvals: must be an integer from 0 to 10","error_type":"invalid_policy","details":{"rule":"billing","index":0,"field":"require.approvals"}}}
```

and an unknown user in the default's approvers is `details`
`{"rule":"default","index":null,"field":"approvers[0]"}`.

A workspace in any other status (`pending`, `cloning`, `ready`, `failed`)
takes the replacement. It stores the document's normal form as the stored
revision plus 1, with the caller as `updated_by`, drops every revision
numbered 50 or more below it (the 50 highest are kept), and records one
`parley.policy.update` event with `resource_type` `policy`, `resource_id`
the slug, the caller as actor and `metadata` `{"revision": <new
revision>, "rules": <number of rules>}`. Then it evaluates again, in
ascending number order, the [landing](#the-landing-of-a-change) of every
draft and open change whose target workspace is `{slug}`, those from its
task forks included, against the new revision, with the approvals and
check results the changes have (none of them is changed), stores each and
runs the announcement of [Mergeability](#mergeability) for it, so that a
change that is open and `clean` and becomes `ready` gets one
`parley.change.ready`. Landed and abandoned changes keep their landing.
All of this is one transaction: a failure of any part, the evaluation of
any one change included, stores nothing (no revision, no event, no
landing) and is `500` `internal`. The answer is `200` with the policy
object of the new revision and its `ETag`, as `GET` answers it, once every
change carries its new landing. Replacements of one workspace run one at a time: two sent at
once without `If-Match` get consecutive revisions, and of two with the
same `If-Match` one is stored and the other is `412` `revision_mismatch`.

### `POST /api/v1/workspaces/{slug}/policy/evaluate`

Answers what a list of paths, or an existing change, would need to land,
with no approval counted and no check result, against the stored policy
or a candidate document, and writes nothing: no revision, no event, no
change's `landing`, whether the request is answered or refused. Needs the
scope `changes:read` and `read` access, so an org member and a token
bound to a task fork of `{slug}` (a contributor route) evaluate as the
owner does, and an archived workspace is evaluated like any other. The
access refusals come before the body is read: a token without the scope
is `403` `insufficient_scope`, and a caller without access `404`
`not_found` with the message `workspace not found`, as for a workspace
that does not exist. The route takes a body of up to 1 MiB whatever
`[server].max_body_size` says, so that a candidate over the format's limit
reaches the validator.

The body is a JSON object with exactly one of `paths` and `change`:

| Member | Rule |
|---|---|
| `paths` | a list of 1 to 2 000 paths as sent, each a non-empty string that does not start with `/`; a repeated path counts once |
| `target_branch` | with `paths` only: the branch the paths would land on, a string of 1 to 255 characters that need not name an existing branch; absent is the workspace's default branch |
| `change` | an integer, the number of a change of `{slug}` in any state: its current version's files (those between its `base_sha` and its `head_sha`, as for its [landing](#the-landing-of-a-change)) on its own target branch |
| `policy` | optional, a candidate document, validated as the body of `PUT` and evaluated in place of the stored one |

On a task fork's own slug the stored document is revision 0, as `GET`
answers it. The answer is `200` with the stored revision as
`policy_revision`, also when a candidate was evaluated, and the `status`
and `requirements` of [the landing of a change](#the-landing-of-a-change)
as they would be with no approval and no check result: every `counted` is
`0` and every check `pending`. No structural cause applies, so a draft is
not `blocked` for being a draft. For the paths `billing/a.go`, `docs/a.md`
and `billing/a.go` into `main`, under the document of the `PUT` example
stored as revision 1:

```json
{"policy_revision":1,"status":"pending","requirements":[{"rule":"docs","paths":["docs/a.md"],"approvals":{"required":0,"counted":0,"approvers":["owner"]},"checks":[],"block":false,"satisfied":true},{"rule":"billing","paths":["billing/a.go"],"approvals":{"required":1,"counted":0,"approvers":["owner","user:dana"]},"checks":[{"name":"test","status":"pending"},{"name":"billing-e2e","status":"pending"}],"block":false,"satisfied":false}]}
```

The refusals after access come in this order:

| Condition | Answer |
|---|---|
| the body is not one JSON object | `400` `invalid_request` (`request body must be a JSON object`, `request body is not valid JSON`, `request body is empty; want a JSON object`) |
| both or neither of `paths` and `change` | `400` `invalid_request`, message `give exactly one of paths and change` |
| `change` is not an integer (`"1"`, `1.5`, `null`) | `400` `invalid_request`, message `change must be an integer` |
| `target_branch` with `change` | `400` `invalid_request`, message `target_branch cannot be given with change, whose own target branch is used` |
| `paths` is not a list of 1 to 2 000 entries | `400` `invalid_request`, message `paths must be a list of 1 to 2000 paths` |
| an entry of `paths` is not a non-empty string or starts with `/` | `400` `invalid_request`, message `every path must be a non-empty string that does not start with '/'` |
| `target_branch` is not a string, is empty or is longer than 255 characters | `400` `invalid_request`, message `target_branch must be a branch name of 1 to 255 characters` |
| `policy` breaks a rule of the document | `400` `invalid_policy` with `details` `{rule, index, field}` as for `PUT`; a `policy` that is not an object, or whose JSON text is over 65 536 bytes, is the field `document` |
| `paths` without `target_branch` on a workspace with no default branch yet (a `pending` one) | `400` `invalid_request`, message `the workspace has no default branch yet; give target_branch` |
| `change` names no change of `{slug}`, or the caller is a token bound to a task fork and the change's source is not that fork | `404` `change_not_found`, message `no such change`, the same body in both cases |

### The landing of a change

The `landing` of a draft or open change is its current version evaluated
against the current revision of the policy of its target workspace. A
change into a task fork is evaluated against revision 0. The change's
files are those that differ between its `base_sha` and its `head_sha`,
with no cap. A file renamed or copied counts under both of its paths. A
change stacked on another therefore counts only the files of its own
commits. Each file selects the first rule, in document order, whose
`targets` match the change's `target_branch` (absent `targets` match every
branch), one of whose `paths` matches the file and none of whose `exclude`
does. A file that no rule selects selects `default`. A path glob matches
the whole path: `*` matches any run of characters without `/`, `**` any
run including `/`, both possibly empty, `?` exactly one character other
than `/`, and every other character itself.

Each selected rule, then `default`, gives one requirement:

| Member | Meaning |
|---|---|
| `rule` | the rule's name, or `default` |
| `paths` | the change's files that selected it, in ascending byte order, the first 50 |
| `approvals` | `required` (the rule's `approvals`), `counted` and `approvers` |
| `checks` | each of the rule's checks once, with its `status`: `passed`, `failed` or `pending` (no result) |
| `block` | the rule's `block` |
| `satisfied` | `true` when the rule does not block, enough approvals count, no rejection is outstanding and every check passed |

`counted` and the check statuses come from the verdicts and check
results recorded on the version being evaluated ([Approvals and
checks](#approvals-and-checks)). The `status` is `blocked`, with the `reason`,
for a draft (`draft`). It is also `blocked`, with the `reason`, for a
change with a structural cause (`target_not_found`, `nothing_to_land`,
`unrelated_history`, `merge_commits`), whatever its requirements say.
Otherwise it is `blocked` when a requirement blocks, `pending` when one is
not satisfied, and `ready` otherwise or when there is none. The
requirements are listed in every case.

The landing is evaluated again and stored on each of these:

- a new version;
- a retarget;
- a move between draft and open;
- a restore;
- a mergeability check of a change whose target branch is missing or found
  again;
- a replacement of the policy of its target workspace (`PUT` above);
- a verdict recorded, replaced or withdrawn, and a check result reported
  ([Approvals and checks](#approvals-and-checks));
- verdicts carried to a new version that is patch-equivalent to an
  earlier one;
- the policy gate of a land.

A re-evaluation that runs at the same time as a replacement of the policy
reads the revision in its own transaction, and transactions that write run
one at a time: the landing left is the one of the newer revision.

When the policy re-evaluates a change itself, as the gate and a
replacement do, it stores
the landing and then runs the announcement of [Mergeability](#mergeability)
in the same transaction. `parley.change.ready` is therefore written once
when the change becomes landable, and a failure of either step stores
neither. Every re-evaluation sets the change's `updated_at`. A landed or
abandoned change keeps the landing it had when it closed.

## Approvals and checks

The evidence the [landing](#the-landing-of-a-change) counts about one
version of a change: a user's verdict, `approve` or `reject`, and the named
results of external check runners. Both are recorded against the exact
version they were given on. The routes are under
`/api/v1/workspaces/{slug}/changes/{number}`, where `{slug}` is the
workspace that holds the change's target branch. A `{number}` that is not a
positive integer, or names no change of `{slug}`, answers `404`
`change_not_found` with the message `no such change`. A `version` (query
or body) that is not a positive integer is `400` `invalid_request`
(`version must be a positive integer`), and one the change does not have
is `404` `not_found` (`demo#1 has no version 7`).

The approval object:

```json
{"id":"7192e863-b4a0-4a41-8357-49ca6ca19a64","change":1,"version":2,"verdict":"approve","comment":"","by":{"actor_type":"api_key","actor_id":"53874e1f-8c16-48ae-a95a-51563b197fc7","username":"dev"},"counts_for":["default"],"carried_from":1,"created_at":"2026-11-16T11:13:37Z"}
```

| Member | Meaning |
|---|---|
| `id` | a UUID |
| `change`, `version` | the version the verdict is on |
| `verdict` | `approve` or `reject` |
| `comment` | at most 16 000 characters, `""` when none was given |
| `by` | the credential that gave it: `actor_type` (`api_key` or `pat`), `actor_id` and `username`, the user's |
| `counts_for` | the `rule` of every requirement of the change's stored `landing` toward which the verdict counts, in the order of the requirements, computed when it is read; `[]` for a verdict that is not on the current version |
| `carried_from` | for a copy carried to a patch-equivalent version, the version the verdict was first given on; else `null` |
| `created_at` | when it was given; a carried copy keeps the original's |

**The counting rule.** A verdict counts toward a requirement when its user
is among the requirement's `approvers` (`owner` the workspace's owner,
`member` every member of the workspace's organization, `user:<username>`
the user of exactly that username, compared case-sensitively) and its
credential created no version of the change numbered at or below the
verdict's own. Credentials are compared, not users: a person approving
their agent's push with their own API key counts, an agent approving the
version it pushed does not, and a person who pushed with one token and
approves with another counts. A version the hub created (`created_by`
`system`: a restack, a sync) excludes nobody. A version with no recorded
credential that the hub did not create excludes every credential of its
`created_by` user. The landing counts the `approve` verdicts on the
version evaluated that count, and a `reject` that counts keeps its
requirement unsatisfied until its author replaces or withdraws it or a
version that is not patch-equivalent arrives. A requirement that needs
zero approvals still lists in `counts_for` the verdicts that would count.

**Carry-over.** When a version is recorded with `patch_equivalent_to`
naming an earlier version, every verdict on that version is copied to the
new one in the transaction that records it, with a new `id` and
`carried_from` set, and the change is evaluated again when any was
copied; no event is written for the copies. A version that is not patch-equivalent starts with no
verdicts. Check results are not copied: they are looked up through
`patch_equivalent_to` when read (below).

### `POST /api/v1/workspaces/{slug}/changes/{number}/approvals`

Records the caller's verdict. Needs `read` access and, for a PAT, the
scope `changes:approve`, which no other scope implies (`changes:write`
alone is `403` `insufficient_scope`). Any user who can read the change may
give a verdict, an org member included; whether it counts is the rule
above. The admin token is `403` `forbidden` (`a verdict is given by a
user; the admin token cannot give one`), whatever the body. A token bound
to a task fork of `{slug}` is `403` `token_bound`.

The body is a JSON object with `verdict` (`approve` or `reject`), an
optional `comment` string and an optional `version`, and no other member;
anything else is `400` `invalid_request` (`verdict must be approve or
reject`, `comment must be a string of at most 16000 characters`, `unknown
member "x"; a verdict has verdict, comment and version`). The checks run
in this order: the change exists, the body is valid, the workspace is not
archived (`409` `workspace_archived`), the change is draft or open (`409`
`change_closed`), the version exists. Without `version` the verdict is on
the current version.

A user has at most one verdict per version. The first is answered `201`
with the approval; a later one by the same user on the same version, with
any of the user's credentials, replaces it and is answered `200`: a new
`id`, a new `created_at`, `carried_from` `null`. A verdict on an earlier
version that the current version reaches through `patch_equivalent_to` is
also copied to every version from the named one to the current one,
replacing the user's verdict on each, with `carried_from` the named
version; the answer is the verdict on the
named version. A verdict on any other version is recorded and changes
nothing. In the same transaction the change is evaluated again and one
`parley.change.approve` or `parley.change.reject` event is written:
`resource_type` `change`, `resource_id` the number, the caller as actor,
`session_id` `null`, metadata `{version, approval_id, counts_for, number,
ref}` with the answer's `counts_for`. A failure of any part answers `500`
`internal` and writes nothing.

### `GET /api/v1/workspaces/{slug}/changes/{number}/approvals`

Needs `read` access and, for a PAT, `changes:read`. Answers `200` with the
verdicts of every version, or of the version `version` names, as approval
objects in the list envelope, newest first (`created_at` descending, then
`id` descending), paged by `limit` and `cursor`.

### `DELETE /api/v1/workspaces/{slug}/changes/{number}/approvals/{id}`

Withdraws one verdict. Needs `read` access and, for a PAT,
`changes:approve`; a token bound to a task fork of `{slug}` is `403`
`token_bound`. In order: the change exists, the workspace is not archived
(`409` `workspace_archived`), the change is draft or open (`409`
`change_closed`), `{id}` names a verdict of this change (`404`
`not_found`, also for a string that is not a UUID), and the caller is the
verdict's user (with any credential), the workspace's owner or the admin
token (`403` `forbidden`, `only the verdict's user, the workspace's owner
or the admin token may withdraw it`). It removes exactly that verdict
(copies of it on other versions are verdicts of their own), evaluates the
change again in the same transaction, writes no event of its own and
answers `204` with no body.

The check object:

```json
{"name":"test","change":1,"version":1,"status":"passed","summary":"42 tests","url":"https://ci.example/run/7","details":{"duration_ms":812},"reported_by":{"actor_type":"pat","actor_id":"53874e1f-8c16-48ae-a95a-51563b197fc7","username":"dev"},"updated_at":"2026-11-16T11:13:37Z"}
```

`name` matches `^[a-z0-9][a-z0-9_.-]{0,62}$`; `status` is `pending`,
`passed` or `failed`; `summary` is `""` and `url` and `details` `null`
when the report gave none; `reported_by` is `admin_token` with `null` id
and username for the admin token. There is one result per version and
name.

### `PUT /api/v1/workspaces/{slug}/changes/{number}/checks/{name}`

Reports a check result. Needs `write` access (the owner; a member is `403`
`forbidden`) and, for a PAT, the scope `checks:write`, which no other scope
implies, so a PAT holding only `checks:write` can report and do nothing
else. The admin token may report. A token bound to a task fork of `{slug}`
is `403` `token_bound`.

The body is a JSON object with `status` and the optional `summary` (at
most 2 000 characters), `url` (an absolute `http` or `https` URL of at
most 2 048 characters), `details` (a JSON object whose text as received is
at most 16 384 bytes) and `version`, and no other member; a `{name}` that
does not match the pattern, or a body that breaks a rule, is `400`
`invalid_request`. The order of the other checks is that of the verdict.
The result replaces the version's result of that name whole, so a member
the new report omits is empty; the answer is `200` with the check for a
first report and a replacement alike. In the same transaction the change
is evaluated again and one `parley.change.check` event is written for
every report, the status changed or not, with metadata `{version, name,
status, number, ref}` and `session_id` `null`.

### `GET /api/v1/workspaces/{slug}/changes/{number}/checks`

Needs `read` access and, for a PAT, `changes:read`. Answers `200` with the
results that apply to the current version, or to the version `version`
names, one per name, ordered by name, in the list envelope. The result
that applies to a version for a name is the one reported on that version,
else the one reported on the nearest earlier version reached by following
`patch_equivalent_to`; a result reached that way keeps the `version` it
was reported on. The landing sees, for each check a requirement names,
the status of the result that applies to the version evaluated, or
`pending` when none does; with `fresh_checks` only a result reported on
that version itself applies. A check no runner reports stays `pending`.

A token bound to a task fork reaches the two lists on the fork's parent
only for changes whose source workspace is that fork; every other change
answers `404` `change_not_found`, with the body an unknown number gets.

## Awareness

Awareness shows an agent the other work on the paths it touches: the
`draft` and `open` changes that touch the same files, the claims other
sessions announced, and what landed. It is advisory: nothing here refuses a
push, a commit, an open or a land.

Every route is under `/api/v1/workspaces/{slug}` and answers in every
workspace status. The four reads (`GET /activity`, `GET
/changes/{number}/overlaps`, `GET /claims`, `GET /claims/{id}`) need the
scope `changes:read` from a PAT and read access to the workspace; the three
claim writes (`POST /claims`, `PATCH /claims/{id}`, `DELETE /claims/{id}`)
need `changes:write` and read access, so a member of the workspace's
organization may claim. Through a task fork's slug every route acts on the
fork's **parent**: access is decided on the fork, and the changes, the
change numbers, the claims and the default branch are the parent's; a claim
made through a fork is stored on the parent. A token bound to a fork reaches
the four reads on the parent (they are contributor routes, see
[Workspace-bound tokens](#workspace-bound-tokens)) and is not confined to its
own changes there; the claim writes answer it `403` `token_bound` on the
parent.

### The overlap index

The hub keeps, per workspace and target branch, the complete set of files
each change touches: the files that differ between the `base_sha` and the
`head_sha` of its current version, a renamed or copied file under both of
its paths, with no limit on their number (the change object's own `paths`
stops at 2,000). A background worker follows the event log
(`parley.change.version`, which also carries a retarget,
`parley.change.land`, `parley.change.abandon` and `parley.change.restore`)
and updates the index about a second after the request that caused the
event, which is answered without waiting for it. A version whose files
cannot be listed (its commits are gone) is indexed with no files. Each event
is applied once, also across a restart. Events the event log expired before
the worker read them are skipped: the worker logs it and goes on from the
oldest event the log still holds. A version or restore event read after the
change landed or was abandoned leaves the change out of flight, as its
record says.

Two changes **overlap** when both are `draft` or `open`, have the same
target branch and at least one touched file in common, and are not in one
stack (two changes whose `depends_on` chains reach the same bottom change).
Overlap is on paths, not lines.

When an update of the index makes two changes overlap that did not overlap
before, the hub writes `parley.change.overlap` on each of the two, naming the
other: `resource_type` `change`, `resource_id` the number, the target
workspace, the actor `system`, the session of the change the event is
written on, and metadata `number`, `with` (`number` and `ref` of the other
change), `with_session_id`, `paths` (the files in common, ascending, at most
100, with `truncated: true` when it had to cut them) and `count`. A pair is
announced once while it overlaps; once it stops (no file in common any
more, different targets, or one of the two landed or abandoned) it is
forgotten, and a later overlap is announced again.

An **overlap entry** is

```json
{"change": {"number": 2, "ref": "demo#2", "title": "…", "intent": "…", "state": "open",
            "author": {"actor_type": "api_key", "actor_id": "…", "username": "bob"},
            "session_id": null, "version": 1, "updated_at": "2026-11-16T10:00:00Z",
            "mergeable": "clean", "landing": {"status": "ready"}},
 "paths": ["api/handler.go"], "paths_truncated": false, "count": 1}
```

with `intent` cut to its first 500 characters, `paths` the files in common
in ascending byte order cut to the first 100, `paths_truncated` `true`
exactly when there are more, and `count` the number of files in common.

### `GET /api/v1/workspaces/{slug}/changes/{number}/overlaps`

Answers `200` with a list (`{"items":[…],"next_cursor":…}`, paged by `limit`
and `cursor`) of one overlap entry for every other change that overlaps
this one, ordered by `count` descending, then `number` ascending. A change
that is `landed` or `abandoned` has an empty list. A number that is not a
positive integer, or names no change of the workspace acted on, answers
`404` `change_not_found`. `GET …/changes/{number}?include=overlaps` adds
the same entries, unpaged, as the member `overlaps`.

### `GET /api/v1/workspaces/{slug}/activity`

What touches a list of paths. Query parameters:

| Parameter | Meaning |
|---|---|
| `paths` | required: one or more globs (`*` matches within a path segment, `**` across segments), as a comma list, repeated, or both; at most 50, each 1 to 255 characters and not starting with `/` |
| `target` | the target branch; default the default branch of the workspace acted on (for a fork's slug, the parent's). It is not checked: an unknown branch gives `target_sha` `null` and, normally, empty lists |
| `since` | exactly 40 hexadecimal characters is a commit id: the changes whose landed commit is not reachable from it, or that the repository no longer holds, are reported; anything else must be an RFC 3339 timestamp: the changes landed after it. Default: 24 hours before the request |
| `exclude_session` | leaves out the changes, claims and landed changes of that session |
| `exclude_change` | a positive integer: leaves out that change and the claims linked to it |

It answers `200` with

```json
{"target_branch": "main", "target_sha": "<40 hex>",
 "changes": [<overlap entry>…], "claims": [<claim>…], "landed": [<landed>…]}
```

- `target_sha` is the head of the target branch, `null` when the workspace
  has no such branch;
- `changes`: the `draft` and `open` changes on the target with a touched file
  matching any glob, as overlap entries whose `paths` and `count` are the
  matching files, ordered by `count` descending, then `number` ascending. It
  shows a change whose source is a fork the caller cannot read, and drafts;
- `claims`: the active claims on the target (not past `expires_at`) with a
  glob that can match the same files as a requested glob, newest first. Two
  globs are taken to match the same files when either one, used as a
  pattern, matches the other's text before its first `*`; this errs towards
  reporting;
- `landed`: the changes landed on the target after `since` with a touched
  file matching a glob, newest first, at most 100, each `{number, ref,
  title, landed_sha, landed_at, session_id, paths}`, `paths` the matching
  files cut to 100.

The three lists are never `null`, and neither paged nor cut otherwise. `400`
`invalid_request` for: no `paths` or only empty ones, more than 50 globs, a
glob that is empty, longer than 255 characters or starts with `/`; a `since`
that is neither a commit id nor a timestamp, or a commit id the repository
does not hold; an `exclude_change` that is not a positive integer.

### Claims

A claim is an advisory, expiring statement that a session intends to work on
some paths:

```json
{"id": "<uuid>", "workspace_slug": "demo", "paths": ["api/**"], "target_branch": "main",
 "intent": "rework handlers", "by": {"actor_type": "api_key", "actor_id": "…", "username": "bob"},
 "session_id": null, "change": null, "expires_at": "2026-11-16T11:00:00Z",
 "state": "active", "created_at": "2026-11-16T10:00:00Z"}
```

`state` is `active`, `released`, `expired` or `fulfilled`. A claim past its
`expires_at` reads `expired` at once, in every read, in the activity and in
the limits, before the hub's expiry run (every minute) stores it as
`expired` and writes `parley.claim.expire` (actor `system`, metadata
`paths`). When a change is opened with a `session_id` that has active
claims on the same workspace and target branch, its version 1 sets their
`change` to its number; when that change lands or is abandoned, its claims
still active become `fulfilled`, without an event (a restore does not turn
them back). A claim that is `released`, `expired` or `fulfilled` is removed
7 days after it ended by the `claim_cleanup` maintenance task, and every
claim of a workspace goes with the workspace when it is deleted. Claims
never conflict with each other and have no effect on pushes, commits,
policy or landing.

`POST /api/v1/workspaces/{slug}/claims` takes `{"paths", "intent",
"target_branch"?, "ttl"?, "session_id"?}`: `paths` an array of 1 to 50
globs (as for the activity), `intent` 1 to 2,000 characters,
`target_branch` 1 to 255 characters (default the default branch of the
workspace acted on), `ttl` a duration (a whole number followed by `s`, `m`,
`h` or `d`) of at least `1s` and at most `[awareness].max_claim_ttl`
(default `[awareness].default_claim_ttl`). It accepts an `Idempotency-Key`.
It answers `201` with `{"claim": <claim>, "activity": <the activity answer
for the claim's paths and target, without the claim itself>}` and writes
`parley.claim.create` (`resource_type` `claim`, the caller as actor,
metadata `paths`, `intent`, `target_branch`, `expires_at`). Refusals, which
store nothing and write nothing: `400` `invalid_request` for a member that
is missing, of the wrong type or out of bounds; `400` `unknown_session` for
a `session_id` the session-linking rule of [Sessions](#sessions) refuses;
`429`
`claim_limit_reached` when the workspace the claim would be stored on
already has `[awareness].max_claims_per_workspace` active claims, or the
session already has 50.

`GET /api/v1/workspaces/{slug}/claims` lists the claims stored on the
workspace acted on, newest first (`created_at`, then `id`, descending),
paged by `limit` and `cursor`, filtered by `state` (default `active`; one of
the four, by the state a read reports, else `400` `invalid_request`),
`session_id` and `path` (a glob, matched by the rule of the activity).
`GET …/claims/{id}` answers `200` with the claim.

`PATCH …/claims/{id}` takes `{"ttl"?, "intent"?}`: `ttl` moves `expires_at`
to the time of the request plus `ttl`, `intent` replaces the intent; it
answers `200` with the claim and writes no event. A body with neither
member, or an invalid one, is `400` `invalid_request`; a claim that is not
active is `409` `claim_not_active`.

`DELETE …/claims/{id}` releases an active claim: its state becomes
`released` and `parley.claim.release` is written (the caller as actor,
metadata `paths`). For a claim that is not active it changes nothing. Both
answer `204` with no body.

Only the user who created the claim (with any credential of that user), the
owner of the workspace the claim is stored on and the admin token may
`PATCH` or `DELETE` it; anyone else who can read it gets `403` `forbidden`.
An id that is not a UUID, is unknown, or names a claim of another workspace
answers `404` `claim_not_found` on all three routes of one claim.

## Forge reads

A workspace's repository usually lives on GitHub or GitLab. The hub is that
repository's forge client: the routes below read its issues, comments and
pull requests with the credential the hub stores for the workspace, so a
caller never holds a forge token. They answer GitHub and GitLab in one
shape. They are `GET` routes and behave alike in what follows.

Each takes the admin token, an API key or a PAT, and declares the scope
`forge:read` for a PAT, which a PAT holding `forge:write` also satisfies. It
needs read access to the workspace (see [Workspace access](#workspace-access)),
so an org member reads as the owner does. A PAT with neither scope is
`403` `insufficient_scope`, for an unknown slug as well, and a caller who
cannot read the workspace `404` `not_found`, with the same bytes as for an
unknown slug.

After access, the route checks, in this order, and answers the first
failure; a request refused at any of them sends nothing to the forge:

1. **Task fork.** A task fork has no forge of its own: called with a fork's
   slug, the route acts on the fork's parent, with the parent's record,
   settings and secrets, `target=upstream` included. The caller needs read
   access on the parent, decided by the user behind the credential (a
   token bound to the fork is accepted; its binding is not applied to the
   parent's slug); without it the answer is `403` `forbidden` (`the caller
   has no read access to the parent of task fork "<slug>"`).
2. **`target`**, the remote the route works on. Absent or `origin`: the
   workspace's `git_url`, its origin credential and the setting
   `forge_api_url`. `upstream`: the `upstream_url`, the upstream credential
   and the setting `upstream_forge_api_url` (never `forge_api_url`), only on
   a carry-patch workspace that has an `upstream_url`; elsewhere `400`
   `workspace_mode_mismatch`. `target` given twice or with another value is
   `400` `invalid_request` (`target must be given at most once, as origin
   or upstream`).
3. **Path and query parameters.** A `:number` is digits only, without sign
   or leading zero, from 1 to 2,147,483,647; anything else is `400`
   `invalid_request` (`number must be a positive decimal integer`). Then the
   route's own parameters and the list parameters (below).
4. **Forge configuration.** The API URL setting of the target, when set,
   must be an absolute `https://` URL with a host, without user
   information, query or fragment: else `422` `forge_api_url_invalid`
   (`the setting forge_api_url must …`). A remote URL that names no
   repository is `422` `forge_unknown`, and so is a forge whose kind cannot
   be told, with a message that names the setting to set (`cannot tell
   whether <host> is GitHub or GitLab; set forge_api_url to the forge's API
   URL`, or `upstream_forge_api_url` for upstream). Secrets that cannot be
   read are `500` `internal`; nothing is sent then, and the hub never falls
   back to an anonymous request.
5. **The call.**

Reads work in every workspace state, `pending`, `failed`, archived or busy
included: they never answer `workspace_archived`, `workspace_not_ready` or
`workspace_busy`, and they do not use the hub's copy of the repository.
They record no event, and nothing is cached: every request asks the forge.

**Resolution.** From the remote URL (`https://`, `http://`, `ssh://`,
`git://` or the scp form `user@host:owner/name`) the hub takes the host in
lower case, without a leading `www.`, port or user information, and the path
without one trailing `.git`; it needs at least two segments: `name` is the
last, `owner` all before it (`group/subgroup` is kept). The forge kind is
the one of `github` and `gitlab` the API URL setting contains, compared in
lower case, when it contains exactly one; else the one the host contains,
when exactly one; else it is unknown. The API URL is the setting with one
trailing `/` removed, else `https://api.github.com` for `github.com`,
`https://<host>/api/v3` for another GitHub host and `https://<host>/api/v4`
for GitLab; a GitLab URL that does not end in `/api/v4` gets it appended.

**Credential.** Read anew from the workspace's secrets on every request, so
a rotated secret applies to the next one:

| Kind | Origin | Upstream |
|---|---|---|
| `pat` | `GIT_PAT`, set and not empty | `UPSTREAM_GIT_PAT`, set and not empty |
| `basic` | `GIT_USERNAME` and `GIT_PASSWORD`, both set and not empty; the password is the token | `UPSTREAM_GIT_USERNAME` and `UPSTREAM_GIT_PASSWORD`, the same way |
| `none` | neither | neither, then the origin's credential |

The token is sent as `Authorization: Bearer <token>` to both forges; kind
`none` sends no authentication. The hub reads no forge token or API URL from
its environment (`GITHUB_TOKEN`, `GH_TOKEN`, `GITLAB_TOKEN`,
`GITHUB_API_URL`, `GITLAB_API_URL` are never read).

**Outbound requests.** Every request carries `User-Agent:
parleyd/<version>` and, to GitHub, `Accept: application/vnd.github+json`. It
goes through the proxy `HTTPS_PROXY` or `HTTP_PROXY` names, directly for a
host `NO_PROXY` lists (see [configuration.md](configuration.md)). A redirect
is followed only to the same scheme, host and port, at most 10 times. Each
request has a deadline of 30 seconds, its body read included, and a body is
read up to 8,388,608 bytes: a successful answer with a longer body cannot be
read, while an error answer, a rate limit included, is judged by its status
and its body cut there. A route that needs several forge requests gives
each its own deadline. An answer is rate limited when its status is `429`,
or `403` with `X-RateLimit-Remaining: 0` or a `Retry-After` header; its
stated wait is `Retry-After` in seconds, else the reset time in
`X-RateLimit-Reset` or `RateLimit-Reset` minus now. The request is sent once
more, with a deadline of its own, after the wait, when the wait is at most
the time left of its deadline.

**Forge failures.** First matching row:

| Condition | Status | `error_type` |
|---|---|---|
| a redirect to another scheme, host or port, or an eleventh redirect | `502` | `forge_error` |
| no complete answer within the deadline | `504` | `forge_timeout` |
| rate limited, after the retry rule | `429` | `forge_rate_limited` |
| the forge answers `404` | `404` | `forge_not_found` |
| the forge answers `401` | `502` | `forge_unauthenticated` |
| the forge answers `403`, not a rate limit | `502` | `forge_forbidden` |
| the forge answers another `4xx` | `422` | `forge_rejected` |
| the forge answers `5xx`, or a successful answer whose body the hub cannot read (over 8,388,608 bytes, not the JSON expected) | `502` | `forge_error` |
| the connection fails (name resolution, TCP, TLS) | `502` | `forge_unreachable` |

The message is `the forge answered <status>: <the forge's message>`, the
forge's message being the body's member `message`, else (when `message` is
absent, `null` or `""`) `error`, a value that is not a string in its JSON
form (`the forge answered 400:
{"title":["is too long"]}`); `the forge answered <status>` when it gave
none; a plain description when the forge gave no answer (`the forge did not
answer within the request's deadline`, `the forge could not be reached: …`).
A `429` carries `Retry-After` in whole seconds, the wait of the last
rate-limited answer rounded up, when the forge stated one. Every message is
scrubbed before it leaves the hub: the token is replaced by `[redacted]`,
URL user information is removed and the text is cut to 1,024 bytes.

```json
{"error":{"code":404,"message":"the forge answered 404: 404 Not Found","error_type":"forge_not_found"}}
```

**Timestamps** are RFC 3339 UTC with whole seconds; an absent one is
`null`. An absent `body` or `summary` is `""`. `url` is the web URL of the
object. On GitLab a number is the project-level `iid`, a pull request is a
merge request, and a project is addressed by its URL-encoded path.

**Lists** answer `{"items":[…],"next_cursor":…}` and take `limit` (1 to
200, default 50) and `cursor`, as [Lists](#lists) says, with one rule of
their own: a forge pages by page number and page size, so a request with a
cursor reads with the cursor's page size, and a `limit` given with a cursor
must equal it (`400` `invalid_request`, `limit must be left out or equal the
cursor's page size, <n>`). Because the forge pages by number, an entry added
or removed on the forge between two requests can make the next page repeat
an entry or skip one: the guarantee of [Lists](#lists) that a page continues
after the last item written does not hold for these lists. `next_cursor` is
`null` on the last page, when the forge names no next page (`Link` header,
`rel="next"`). A page may hold fewer than `limit` items when the hub drops
entries after the forge paged (GitHub's pull requests from the issue list,
GitLab's system notes), and is not refilled.

## `GET /api/v1/workspaces/{slug}/forge`

Reports how the hub reaches the workspace's forge, without contacting it.
It takes no `target`; one that is given is ignored. For a task fork the
whole document describes the parent, with the parent's slug in
`webhook.url`.

| Member | Value |
|---|---|
| `forge` | `github`, `gitlab` or `unknown` (the remote names no repository, or the kind cannot be told) |
| `api_url` | the API URL, `null` when `forge` is `unknown` |
| `repository` | `{host, owner, name}`, `null` when the remote names none |
| `credential` | the credential kind, `pat`, `basic` or `none`, never the token |
| `upstream` | the same four members for the upstream of a carry-patch workspace with an `upstream_url`, else `null` |
| `webhook.configured` | whether the workspace secret `FORGE_WEBHOOK_SECRET` exists and is not empty |
| `webhook.url` | `[server].external_url` without a trailing `/`, then `/hooks/forge/<slug>`; `null` when `external_url` is empty |
| `webhook.last_delivery_at` | the time of the newest recorded webhook delivery, `null` when there is none |

| Status | When |
|---|---|
| `200 OK` | the document |
| `422` `forge_api_url_invalid` | `forge_api_url`, or `upstream_forge_api_url` where the upstream applies, is not a valid API URL |
| `500` `internal` | a secret cannot be read, or the webhook delivery provider fails |

```json
{"forge":"github","api_url":"https://api.github.com","repository":{"host":"github.com","owner":"acme","name":"widgets"},"credential":"pat","upstream":null,"webhook":{"configured":false,"url":"https://hub.example.com/hooks/forge/widgets","last_delivery_at":null}}
```

## `GET /api/v1/workspaces/{slug}/forge/repository`

The forge's repository: `full_name`, `default_branch`, `private`,
`archived`, `permissions {pull, push, admin}` and `allow_merge_commit`,
`allow_squash_merge`, `allow_rebase_merge`, each of the last three `null`
when the forge does not report it. On GitHub a repository without
`permissions` reads `{pull: true, push: false, admin: false}`. On GitLab
`full_name` is `path_with_namespace`, `private` is a `visibility` other than
`public`, `push` is an access level of 30 or more and `admin` of 40 or more,
the higher of project and group access; `allow_merge_commit` is
`merge_method` `merge`, `allow_rebase_merge` `merge_method` `rebase_merge`
or `ff`, and `allow_squash_merge` a `squash_option` other than `never`.

```json
{"full_name":"acme/widgets","default_branch":"main","private":true,"archived":false,"permissions":{"pull":true,"push":true,"admin":false},"allow_merge_commit":true,"allow_squash_merge":false,"allow_rebase_merge":true}
```

## `GET /api/v1/workspaces/{slug}/issues`

A list of issues, never a pull request. Filters, each at most once:

| Parameter | Values | Default |
|---|---|---|
| `state` | `open`, `closed`, `all` | `open` |
| `sort` | `created`, `updated` | `created` |
| `direction` | `asc`, `desc` | `desc` |
| `labels` | label names separated by commas; an issue must carry all of them | none |
| `assignee` | a login | none |

A parameter given twice, a value outside its set, an empty label name
(`a,,b`, or empty) or an empty assignee is `400` `invalid_request`. An issue
is `{number, title, body, state, url, author {login}, labels, created_at,
updated_at, closed_at, is_pr}`; `state` is `open` or `closed` (GitLab's
`opened` and `locked` read `open`) and `labels` the names.

```json
{"items":[{"number":7,"title":"Crash","body":"","state":"open","url":"https://github.com/acme/widgets/issues/7","author":{"login":"kim"},"labels":["bug","ui"],"created_at":"2026-10-01T08:00:00Z","updated_at":"2026-10-02T09:30:00Z","closed_at":null,"is_pr":false}],"next_cursor":null}
```

## `GET /api/v1/workspaces/{slug}/issues/{number}`

The issue thread: `{issue, comments, comments_next_cursor,
comments_error}`, the issue with its first page of at most 100 comments,
oldest first. On GitHub the number of a pull request answers the issue with
`is_pr: true`; on GitLab `is_pr` is always `false`. A failed read of the
issue is the route's error. `comments_next_cursor` continues on the
comments route of the same issue and target, without a `limit`, and is
`null` when the page is complete. When the issue was read and its comments
were not, the answer is still `200`, with `comments` `[]`,
`comments_next_cursor` `null` and `comments_error` `{error_type, message}`
with the values the failure table gives, scrubbed alike; otherwise
`comments_error` is `null`.

## `GET /api/v1/workspaces/{slug}/issues/{number}/comments`

A list of the issue's comments, oldest first: `{id, body, author {login},
created_at, updated_at, url}`, `id` being the forge's numeric id. GitLab
notes are read with `sort=asc&order_by=created_at`, system notes are left
out, and a note's `url` is the issue's web URL followed by `#note_<id>`.

## `GET /api/v1/workspaces/{slug}/pulls/{number}`

The pull request: `{number, title, body, state, url, draft, merged,
head_sha, head_branch, base_branch, author {login}, created_at, updated_at,
closed_at, merged_at}`. `state` is `open`, `closed` or `merged`; it is
`merged`, with `merged` `true`, when the forge reports it merged. `head_sha`
is the full commit id.

## `GET /api/v1/workspaces/{slug}/pulls/{number}/state`

`{number, state, merged, head_sha}`, the values the pull request read gives.

```json
{"number":12,"state":"merged","merged":true,"head_sha":"0123456789abcdef0123456789abcdef01234567"}
```

## `GET /api/v1/workspaces/{slug}/pulls/{number}/files`

A list of the files the pull request changes: `{filename, status,
additions, deletions}`, `status` being `added`, `modified`, `deleted` or
`renamed`. GitHub's `removed` is `deleted`, `copied` is `added`, `changed`
and `unchanged` are `modified`. On GitLab `additions` and `deletions` count
the lines of the file's diff that begin with `+` and `-`.

## `GET /api/v1/workspaces/{slug}/pulls/{number}/checks`

A list of the check runs of the pull request's head (on GitLab the jobs of
the merge request's latest pipeline): `{name, status, conclusion, summary,
url}`, `status` being `queued`, `in_progress` or `completed`, and
`conclusion` `success`, `failure`, `cancelled`, `skipped`, `neutral`, or
`null` while not completed. A head without check runs, or a merge request
without pipelines, answers `{"items":[],"next_cursor":null}`. Every page of
one listing reads the head (commit id, or pipeline id) the first page was
read from, even when the head moved since.

## `GET /api/v1/workspaces/{slug}/pulls/{number}/reviews`

A list of reviews, oldest first: `{author {login}, state, body,
submitted_at}`, `state` being `approved`, `changes_requested`, `commented`
or `dismissed`. GitHub's pending reviews are left out. On GitLab the
approvals come first, in the forge's order, as `approved` with `body` `""`
and `submitted_at` `null` (the forge reports no time for them), then the
non-system notes as `commented`, oldest first; no page is longer than
`limit`. When the approvals end exactly at the end of a page, the hub asks
the forge for one note to learn whether any follows, and `next_cursor` is
`null` when none does.

## Forge writes

Nine routes change the issues, comments, labels and pull requests of a
workspace's forge repository with the credential the hub stores for the
workspace, so that no caller holds a forge token. They resolve the forge,
the target, a task fork's parent and the credential as the
[forge reads](#forge-reads) do, send with the same outbound rules and answer
a failed forge call with the same table of forge failures.

| Route | Does | Success |
|---|---|---|
| `POST /api/v1/workspaces/{slug}/issues` | create an issue | `201`, the issue |
| `PATCH /api/v1/workspaces/{slug}/issues/{number}` | update an issue | `200`, the issue |
| `POST /api/v1/workspaces/{slug}/issues/{number}/close` | close an issue | `200`, the issue |
| `POST /api/v1/workspaces/{slug}/issues/{number}/comments` | comment on an issue | `201`, `{"url"}` |
| `POST /api/v1/workspaces/{slug}/issues/{number}/labels` | add labels | `204` |
| `DELETE /api/v1/workspaces/{slug}/issues/{number}/labels/{name}` | remove a label | `204` |
| `POST /api/v1/workspaces/{slug}/labels` | ensure a label | `200` or `201`, the label |
| `POST /api/v1/workspaces/{slug}/pulls` | open a pull request | `201`, the pull request |
| `POST /api/v1/workspaces/{slug}/pulls/{number}/comments` | comment on a pull request | `201`, `{"url"}` |

Each takes the admin token, an API key or a PAT with the scope
`forge:write`, and needs write access to the workspace. A PAT without the
scope is `403` `insufficient_scope`, for an unknown slug too; a caller with
read access only is `403` `forbidden`, on an archived workspace too; a
caller with no access is `404` `not_found`. Called with a task fork's slug, a
write acts on the parent's forge repository with the parent's credential
and settings, and the caller needs write access on the parent (`403`
`forbidden` without it).

**Order of checks.** After access, the first failure answers, and a request
refused at any step sends nothing to the forge and records no event:

1. `target`, as for the reads (`400` `invalid_request` or `400`
   `workspace_mode_mismatch`).
2. The path parameters and the body (`400` `invalid_request`, below). A bad
   `{number}` is `number must be a positive decimal integer`.
3. The workspace is archived: `409` `workspace_archived` (`workspace "<slug>"
   is archived`); called through a task fork whose parent is archived: `409`
   `parent_archived` (`workspace "<fork>" is read-only: its parent workspace
   "<parent>" is archived`).
4. The forge configuration: `422` `forge_api_url_invalid` or `422`
   `forge_unknown`.
5. The rules of the forge kind: on GitLab an update that sets only
   `assignees` (`400` `invalid_request`), and the text limits (`413`).
6. The target's credential kind is `none`: `422` `forge_credential_missing`
   (`workspace "<slug>" stores no forge credential for the target <target>; a
   write needs one`). The hub never writes to a forge anonymously.
7. The write.

So malformed JSON is `400` on a workspace whose forge is unknown, and an
oversized text on such a workspace is `422` `forge_unknown`. No write route
uses the hub's copy of the repository.

**Bodies.** A body is a JSON object, except on the add-labels route (a bare
JSON array) and the close route (which may also be empty). A body of the
wrong shape is `400` `invalid_request`. A member whose value is `null` is
treated as absent, and unknown members are ignored. A required string must
be present and not blank (empty or Unicode white space only): `<field> is
required`, `<field> must not be blank`. A member of the wrong JSON type is
`<field> must be a string` (or `must be true or false`, `must be an array of
names`). A list of names holds at most 100 entries, each a non-blank string
(`<field> holds <n> names; at most 100 are allowed`, `<field>[<i>] must not
be blank`). A write route's body may be up to 16 MiB, so that a text at the
GitLab limit is measured by the hub rather than refused by the platform's
body limit, also when a client escapes every character beyond the Basic
Multilingual Plane as a JSON surrogate pair (twelve bytes each).

**Text limits.** The `body` of an issue (create and update), of a comment and
of a pull request, and the `comment` of a close, are counted in Unicode code
points: more than 65,536 on GitHub or 1,000,000 on GitLab is `413`
`payload_too_large` (`<field> is <n> characters long; the forge takes at
most <limit>`). A text of exactly the limit is sent, unchanged.

**What is sent.** A field that is not set is left out of the forge request.
On GitLab a list of label names is sent as one comma-separated string, and
`assignees` is never sent (GitLab takes user ids, which the hub does not
resolve). Paths are below the API URL; `R` is `/repos/<owner>/<name>` on
GitHub and `/projects/<owner/name, one escaped segment>` on GitLab.

| Write | GitHub | GitLab |
|---|---|---|
| create issue | `POST R/issues` `{title, body, labels, assignees}` | `POST R/issues` `{title, description, labels}` |
| update issue | `PATCH R/issues/<n>` `{title, body, state, labels, assignees}` | `PUT R/issues/<n>` `{title, description, state_event, labels}` |
| close issue | `PATCH R/issues/<n>` `{state: "closed"}` | `PUT R/issues/<n>` `{state_event: "close"}` |
| comment on an issue | `POST R/issues/<n>/comments` `{body}` | `POST R/issues/<n>/notes` `{body}` |
| comment on a pull request | `POST R/issues/<n>/comments` `{body}` | `POST R/merge_requests/<n>/notes` `{body}` |
| add labels | `POST R/issues/<n>/labels` `{labels}` | `PUT R/issues/<n>` `{add_labels}` |
| remove a label | `DELETE R/issues/<n>/labels/<name>`, on `404` `GET R/issues/<n>` | `PUT R/issues/<n>` `{remove_labels}` |
| ensure a label | `GET R/labels/<name>`, then `POST R/labels` `{name, color, description}` | `GET R/labels/<name>`, then `POST R/labels` `{name, color: "#<hex>", description}` |
| open a pull request | `POST R/pulls` `{title, head, base, body, draft}` | `POST R/merge_requests` `{title, source_branch, target_branch, description}` |

**Disconnects.** A write is not abandoned when the caller disconnects: its
forge requests, each with its own 30-second deadline, and its event are
carried out to their end.

**Retries.** The five routes that create something (create issue, the two
comment routes, ensure a label, open a pull request) accept
`Idempotency-Key` as [Idempotency](#idempotency) says: a replay answers
`200` with the first answer's body, sends nothing to the forge and records no
event; only a `2xx` answer is kept, so a create the forge failed is carried
out again on retry. The close and add-labels routes ignore the header and
carry out every request.

**Events.** A write that handed at least one request to the forge, a request
whose connection failed or whose deadline passed included, records exactly
one event before it answers, whether it succeeded or failed. The actor is
the caller; `workspace` is the workspace whose forge was written, for a call
through a task fork the parent, with the fork's slug in `metadata.fork`.
Every event's metadata holds `target`, `ok`, `error_type` (when `ok` is
false, the answer's `error_type`), `number` (the issue or pull request, when
known) and `url` (the web URL of the resulting issue, pull request or
comment, when known). No event holds a title, a body, a comment text, a
label description, a credential or a forge message. An event that cannot be
recorded makes the answer `500` `internal`; what the forge did is not
undone.

| Event | `resource_type` | `resource_id` | Extra metadata |
|---|---|---|---|
| `parley.forge.issue.create` | `issue` | the new number; `none` on failure | |
| `parley.forge.issue.update` | `issue` | the number | `fields`, the names of the fields sent, sorted |
| `parley.forge.issue.close` | `issue` | the number | `comment_url`, when a comment was posted |
| `parley.forge.comment.create` | `comment` | the issue's number | |
| `parley.forge.label.add` | `label` | the issue's number | `labels` |
| `parley.forge.label.remove` | `label` | the issue's number | `label` |
| `parley.forge.label.create` | `label` | the label's name | `label`, `created` (true only when this call created it); no `number`, no `url` |
| `parley.forge.pull_request.create` | `pull_request` | the new number; `none` on failure | `head`, `base`, `draft` |
| `parley.forge.pull_request.comment` | `pull_request` | the number | |

The three label events carry no `url`. The action of each event is the last
segment of its type (`create`, `update`, `close`, `add`, `remove`,
`comment`).

## `POST /api/v1/workspaces/{slug}/issues`

Body `{"title": string, "body"?: string, "labels"?: [string], "assignees"?:
[string]}`; `title` is required. Answers `201` with the issue, in the shape
of the [issue read](#get-apiv1workspacesslugissuesnumber).

## `PATCH /api/v1/workspaces/{slug}/issues/{number}`

Body with any of `title` (not blank), `body` (`""` clears it), `state`
(`open` or `closed`, else `state must be open or closed`), `labels` and
`assignees` (each replaces the whole set; `[]` clears it). Only the fields
that are set are sent; on GitLab `state` is sent as `state_event` `close` or
`reopen`. A body that sets none of them is `400` `invalid_request` (`an
update must set at least one of title, body, state, labels and assignees`),
and so is a GitLab update that sets only `assignees`. Answers `200` with the
issue as the forge returns it.

## `POST /api/v1/workspaces/{slug}/issues/{number}/close`

The body may be absent, empty or `{"comment": string}`; `{}`, a `null` and a
blank `comment` close without a comment, and a `comment` that is not a
string is `400`. A comment is posted first, and the issue is closed only
when the forge accepted it: when the comment fails, the answer is its
failure and the issue is untouched; when the close fails after the comment
was posted, the answer is the close's failure and the event carries
`comment_url`. Closing a closed issue succeeds. Answers `200` with the issue.

## `POST /api/v1/workspaces/{slug}/issues/{number}/comments`

Body `{"body": string}`, required and not blank, sent exactly as given.
Answers `201` with `{"url": "<web URL of the comment>"}` and nothing else.

## `POST /api/v1/workspaces/{slug}/issues/{number}/labels`

The body is a bare JSON array of 1 to 100 non-blank label names (anything
else is `400`: `the body must be a JSON array of label names`, `labels must
hold at least one name`). The labels are added and the issue keeps its
others. Answers `204` with no body.

## `DELETE /api/v1/workspaces/{slug}/issues/{number}/labels/{name}`

The label is the last path segment, percent-decoded once: `type%2Fbug`
names `type/bug`, `%252F` names `%2F`. A blank name is `400` (`the label
name must not be blank`). Answers `204` when the issue exists, whether or
not it carried the label, and `404` `forge_not_found` when it does not.

## `POST /api/v1/workspaces/{slug}/labels`

Body `{"name": string, "color"?: string, "description"?: string}`; `name` is
required, `color` is six hex digits in either case, with or without a
leading `#` (`color must be six hex digits, with or without a leading #`),
default `ededed`. When the repository has a label of that name, the answer
is `200` with it as the forge holds it, unchanged, even when `color` or
`description` differ. Otherwise the label is created and the answer is
`201`. When the forge refuses the create because the label now exists
(another caller won the race), the hub reads it again and answers `200` with
it. The label is `{"name", "color", "description"}`, `color` six lower-case
hex digits without `#`, an absent description `""`.

## `POST /api/v1/workspaces/{slug}/pulls`

Body `{"title": string, "head": string, "base": string, "body"?: string,
"draft"?: boolean}`; `title`, `head` and `base` are required, `draft`
defaults to `false`. `head` is sent as given, so `owner:branch` names a
branch of a forge-side fork. On GitLab a draft is a merge request whose
title begins with `Draft: `, which the hub adds once. The branches and
whether a pull request already exists are the forge's to judge: its refusal
is `422` `forge_rejected` (`the forge answered <status>: <message>`). Answers
`201` with the pull request, in the shape of the
[pull request read](#get-apiv1workspacesslugpullsnumber); on GitLab its
`number` is the merge request's `iid`.

## `POST /api/v1/workspaces/{slug}/pulls/{number}/comments`

Body `{"body": string}`, required and not blank, sent exactly as given, as a
comment in the pull request's conversation. Answers `201` with `{"url":
"<web URL of the comment>"}`.

## Forge webhooks

A forge can send a workspace's webhook deliveries (an issue opened, a label
added, a pull request merged, a push) to the hub, which proves each one
against a secret only that workspace holds, records it once and relays it as
`parley.forge.*` hub events (see [`GET /api/v1/events/log`](#get-apiv1eventslog)). The hub never registers,
changes or deletes a webhook on the forge, and no route of this section
sends a request to the forge: a person enters the URL and the secret in the
forge's settings.

The **webhook secret** is the workspace secret `FORGE_WEBHOOK_SECRET`, read
on every delivery, so a rotation applies to the next one. The **webhook
URL** is `[server].external_url` without a trailing `/`, followed by
`/hooks/forge/<slug>`; it is `null` when `external_url` is empty. The
[diagnostic route](#get-apiv1workspacesslugforge) reports both as
`webhook.configured` and `webhook.url`, and the newest delivery's
`received_at` as `webhook.last_delivery_at`.

| Route | Does | Success |
|---|---|---|
| `PUT /api/v1/workspaces/{slug}/forge/webhook` | set or rotate the secret | `200` |
| `DELETE /api/v1/workspaces/{slug}/forge/webhook` | remove the secret | `204` |
| `POST /hooks/forge/{slug}` | receive a delivery | `202` |
| `GET /api/v1/workspaces/{slug}/forge/deliveries` | list the recorded deliveries | `200`, a list |

## `PUT /api/v1/workspaces/{slug}/forge/webhook`

Takes the admin token, an API key or a PAT with the scope `forge:write`, and
needs write access to the workspace. The body is empty, or a JSON object
whose optional member `secret` is `null` or a string of 16 to 255
characters, each from `!` to `~` (printable ASCII without the space). With
no body, `{}` or `"secret": null` the hub generates the secret: 32 random
bytes written as 64 lower-case hex characters. Other members are ignored.
The value is stored as the workspace secret, replacing an earlier one whose
key matches ignoring case (the entry keeps its stored casing), and the event
`parley.forge.webhook.set` is recorded in the same transaction; neither
holds the value, and no `parley.secret.*` event is written.

```json
{"configured":true,"url":"https://hub.example.com/hooks/forge/demo","secret":"9f2c…(64 hex characters)"}
```

`secret` is present only when the hub generated it, and no later answer of
any route shows it. Failures, in the order they are decided; a refused
request stores nothing and records no event:

| Condition | Answer |
|---|---|
| a PAT without `forge:write` (an unknown slug too) | `403` `insufficient_scope` |
| no read access to the workspace | `404` `not_found` |
| read access only (an archived workspace too) | `403` `forbidden` |
| the slug names a task fork | `400` `fork_has_no_origin` (`workspace "<slug>" is a task fork, which has no origin to receive webhook deliveries for`) |
| the body breaks the rule above | `400` `invalid_request` (`the body must be empty or a JSON object whose optional member secret is null or a string of 16 to 255 characters, each from ! to ~`); the message never holds the value |
| the workspace is archived | `409` `workspace_archived` |
| no secret encryption key is configured | `503` `secrets_unavailable` |
| the workspace holds 100 secrets and none is `FORGE_WEBHOOK_SECRET` | `429` `secret_limit_reached` (`maximum of 100 entries per scope exceeded`) |
| the event cannot be recorded | `500` `internal`; the secret is as it was |

## `DELETE /api/v1/workspaces/{slug}/forge/webhook`

Takes the scope `forge:write` and write access, as the `PUT`, and works in
every workspace state, archived included. It removes the secret, whether or
not one existed, records `parley.forge.webhook.unset` in the same
transaction and answers `204` with no body. Recorded deliveries remain, and
from then on the ingress answers `404` for the workspace. A task fork is
`400` `fork_has_no_origin`; scope and access are refused as for the `PUT`,
and an event that cannot be recorded is `500` `internal` with the secret
kept.

## `POST /hooks/forge/{slug}`

The ingress lies outside `/api/v1` and is public: it takes no bearer
credential and ignores one that is sent, is not counted by the request rate
limit, does not check the content type, and holds the body to 1 MiB
(1,048,576 bytes) of its own. Its errors use the error envelope. An archived
workspace still relays. The first matching step answers:

1. An unknown slug, a task fork's slug, or a workspace whose
   `FORGE_WEBHOOK_SECRET` is absent or empty: `404` `not_found` with the
   message `not found`, the same bytes for all three. The hub reads the
   workspace and its secret for every slug, so the three also take the
   same reads. A secret that cannot be read, or that is stored but cannot
   be decrypted, is `500` `internal`.
2. A request with `X-Hub-Signature-256` is a GitHub delivery (its
   `X-Gitlab-Token` is not looked at); otherwise one with `X-Gitlab-Token`
   is a GitLab delivery; otherwise `401`.
3. GitLab: the token header appears exactly once and equals the secret,
   else `401`.
4. A body over 1,048,576 bytes, declared or not: `413` `payload_too_large`
   (`the delivery body is larger than 1048576 bytes`), for GitHub whatever
   the signature. The body is not read past the limit.
5. GitHub: the signature header appears exactly once and equals `sha256=`
   followed by the lower-case hex HMAC-SHA256 of the raw body under the
   secret, else `401`. A delivery with only the legacy `X-Hub-Signature`
   has no scheme and is `401`.
6. The delivery id is `X-GitHub-Delivery`, or on GitLab `Idempotency-Key`
   and else `X-Gitlab-Event-UUID`; the event name is `X-GitHub-Event` or
   `X-Gitlab-Event`; of a repeated header the first value is used. A
   missing or empty id, an id of more than 255 characters or with a
   character outside space to `~` is `400` `invalid_request` (`the delivery
   id header is missing, empty, longer than 255 characters or holds a
   character outside space to ~`); a missing or empty event name is `400`
   `invalid_request` (`the delivery's event name header is missing or
   empty`). Nothing is recorded.
7. The delivery is reduced and recorded. When the workspace already has a
   delivery with this id, the answer is `202` `{"outcome":"duplicate"}` and
   nothing else happens.
8. Outcome `malformed`: `400` `invalid_request` (`the delivery's payload is
   not a JSON object of the form its event has`); the delivery stays
   recorded, so its redelivery is a duplicate.
9. Otherwise `202` with `{"outcome": "relayed"}`, `"ignored"` or
   `"repository_mismatch"`.

Secrets are compared in constant time. Every `401` has the `error_type`
`unauthenticated` and the message `delivery signature missing or invalid`,
and records nothing. A `FORGE_WEBHOOK_SECRET` stored through the
[secrets routes](#secrets) is not held to the 16-to-255 rule: any non-empty
value works here. Recording a delivery and appending its events is one
transaction, completed before the `202` even when the forge closes the
connection early; a failure stores neither and is `500` `internal`, so the
forge's redelivery is processed as new. Of concurrent copies of one
delivery exactly one is recorded and the others are duplicates. The same id
in two workspaces is two deliveries.

**Reduction.** The body must be one JSON object, else the outcome is
`malformed` (a form-encoded payload too). The action is GitHub's `action` or
GitLab's `object_attributes.action` (`""` when it is missing or not a
string, cut to 64 bytes). The repository is GitHub's `repository.full_name`
or GitLab's `project.path_with_namespace`, compared ignoring case with the
origin repository (`owner/name` of `git_url`) and then, on a carry-patch
workspace with an `upstream_url`, with the upstream repository; the first
match is the `target`, `origin` or `upstream`. Then the first matching row
decides the outcome:

| Situation | Outcome |
|---|---|
| no mapping for the event and action | `ignored` |
| a required field is missing or of the wrong type | `malformed` |
| the mapping yields no events | `ignored` |
| events, but the payload names no repository | `malformed` |
| events, but the repository is neither origin nor upstream | `repository_mismatch` |
| otherwise | `relayed` |

A required number (`issue.number`, `pull_request.number`, an `iid`) is a
JSON integer of at least 1, a required string (`label.name`, `ref`) a
non-empty JSON string. Event names and actions match exactly, case
included; anything not listed is ignored, such as GitHub `ping` and edited
comments.

| Forge | Event and action | Hub event | Required |
|---|---|---|---|
| GitHub | `issues`: `opened`, `edited`, `closed`, `reopened` | `parley.forge.issue.<action>` | `issue.number` |
| GitHub | `issues`: `labeled`, `unlabeled` | `parley.forge.issue.<action>` | `issue.number`, `label.name` |
| GitHub | `issue_comment`: `created` | `parley.forge.comment.created` | `issue.number` |
| GitHub | `pull_request`: `opened`, `reopened`, `synchronize` | `parley.forge.pull_request.<action>` | `pull_request.number` |
| GitHub | `pull_request`: `closed` | `.merged` when `pull_request.merged` is `true`, else `.closed` | `pull_request.number` |
| GitHub | `push` | `parley.forge.push` | `ref` |
| GitLab | `Issue Hook`: `open`, `close`, `reopen` | `parley.forge.issue.opened`, `.closed`, `.reopened` | `object_attributes.iid` |
| GitLab | `Issue Hook`: `update` | `.edited` when `changes.title` or `changes.description` is present; then one `.unlabeled` per title of `changes.labels.previous` not in `.current`, then one `.labeled` per title the other way round, each group in byte order | `object_attributes.iid` |
| GitLab | `Note Hook` with `noteable_type` `Issue` or `MergeRequest` | `parley.forge.comment.created` | `issue.iid` or `merge_request.iid` |
| GitLab | `Merge Request Hook`: `open`, `close`, `reopen`, `merge` | `parley.forge.pull_request.opened`, `.closed`, `.reopened`, `.merged` | `object_attributes.iid` |
| GitLab | `Merge Request Hook`: `update` | `.synchronize` when `object_attributes.oldrev` is a non-empty string, else no event | `object_attributes.iid` |
| GitLab | `Push Hook`, `Tag Push Hook` | `parley.forge.push` | `ref` |

One delivery relays at most 100 label events.

**Events.** Every delivery event has `workspace` the slug, `actor_type`
`forge`, `actor_id` `null` and `action` the last segment of its type, and
the metadata `forge`, `target`, `delivery_id` and `actor` (GitHub
`sender.login`, GitLab `user.username` or `user_username` on push hooks,
`""` when the payload names none), plus:

| Event | `resource_type` | `resource_id` | Metadata |
|---|---|---|---|
| `parley.forge.issue.opened`, `.edited`, `.closed`, `.reopened` | `issue` | the number | `number`, `title` (cut to 256 bytes), `url`, `labels` (the first 100 names, each cut to 1024 bytes) |
| `parley.forge.issue.labeled`, `.unlabeled` | `issue` | the number | `number`, `label` (cut to 1024 bytes), `labels`, `url` |
| `parley.forge.comment.created` | `comment` | the issue or pull request number | `number`, `comment_url`, `pull_request` (boolean) |
| `parley.forge.pull_request.opened`, `.closed`, `.merged`, `.reopened`, `.synchronize` | `pull_request` | the number | `number`, `url`, `head`, `base`, `merged` (true exactly on `.merged`) |
| `parley.forge.push` | `ref` | the ref | `ref`, `before`, `after` |
| `parley.forge.webhook.set` | `webhook` | the slug | `generated` (boolean); the caller as actor |
| `parley.forge.webhook.unset` | `webhook` | the slug | `existed` (boolean); the caller as actor |

GitHub's values are read from `issue.title`, `issue.html_url`,
`issue.labels[].name`, `comment.html_url`, `pull_request.html_url`,
`pull_request.head.ref` and `pull_request.base.ref`; GitLab's from
`object_attributes.title`, `.url`, `.source_branch`, `.target_branch`, the
top-level `labels[].title` and the note's `object_attributes.url`; `ref`,
`before` and `after` are the payload's top-level members. A missing value
or one of the wrong type is `""`, `[]` or `false`. Every URL loses its user
information, and a value that is not an absolute URL is `""`. No event
carries a body, a description, a comment's or a note's text, a commit
message or the secret. `parley.forge.push` reports a push the forge
received and is unrelated to `parley.git.push`.

## `GET /api/v1/workspaces/{slug}/forge/deliveries`

Takes the scope `forge:read` (a PAT with `forge:write` satisfies it) and
read access, in every workspace state. It lists the workspace's recorded
deliveries, newest first (`received_at` descending, and for equal instants
the later recorded first), by the [list convention](#lists) (`limit` 1 to
200, default 50, and `cursor`). `since`, an RFC 3339 instant, keeps the
deliveries received at or after it; an empty or unparseable value is `400`
`invalid_request` (`since must be an RFC 3339 instant, such as
2026-10-21T09:00:00Z`). Other query parameters, `target` among them, are
ignored. A task fork is `400` `fork_has_no_origin`.

```json
{"items":[{"delivery_id":"d-1","forge":"github","event":"issues","action":"labeled","target":"origin","outcome":"relayed","relayed":["parley.forge.issue.labeled"],"received_at":"2026-10-21T09:12:03Z"}],"next_cursor":null}
```

| Member | Meaning |
|---|---|
| `delivery_id` | the forge's id of the delivery, unique per workspace |
| `forge` | `github` or `gitlab` |
| `event` | the forge's event name, cut to 64 bytes |
| `action` | the payload's action, `""` when it has none |
| `target` | `origin`, `upstream`, or `null` when no repository matched |
| `outcome` | `relayed`, `ignored`, `repository_mismatch` or `malformed` (`duplicate` is never recorded) |
| `relayed` | the event types relayed, in order; `[]` unless `outcome` is `relayed` |
| `received_at` | when the hub accepted it, in whole seconds |

Payloads and secrets are never stored. Deliveries are kept for 7 days: the
maintenance task `forge_delivery_pruning` deletes those received more than
604,800 seconds before its pass, once at start and then every hour, and a
pass that fails is logged and its work done at the next one; a pruned id
sent again is a new delivery. The retention is fixed. Deleting a workspace
deletes its deliveries in the same transaction, so a slug registered again
starts with an empty log.

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
| read | `sandboxes:read` | `read` | getting a sandbox, listing a workspace's sandboxes, its logs, downloading a file, listing policy proposals, and the three routes of [the tools of a sandbox](#the-tools-of-a-sandbox), which decide per caller which tools they serve |
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
gateway's credentials and any user information of a URL removed (on the
routes that [use a sandbox](#using-a-sandbox), also the sandbox's own secret
values); do not branch on it. The hub keeps serving: routes that read only the hub's own
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

## The sandbox object

A sandbox is an isolated environment, run by the sandbox runtime, attached
to a workspace (usually a task fork) and started with it checked out. Every
answer that shows a sandbox shows this object, with exactly these members:

```json
{"id":"sbx_3f2a9c0e7b1d4a5e8f6c2b9a0d1e4f7a","workspace":"demo--t1","session_id":null,"profile":"agent","purpose":"run tests","state":"ready","state_reason":null,"command":["make","test"],"env":{"CI":"1"},"exit_code":null,"checkout":{"path":"/workspace","ref":"main","sha":"9fceb02d0ae598e95dc970b74767f19372d61af8","branch":"t1"},"image":"img:1","cpu":"2","memory":"4Gi","gpu":0,"credentials":["model"],"exposed_secrets":["NPM_TOKEN"],"workspace_rules":[],"hub_access":{"scopes":["audit:write","changes:read","changes:write","git:read","git:write","sessions:write"],"paths":null,"expires_at":"2026-11-18T13:00:00Z"},"policy_revision":0,"expires_at":"2026-11-18T13:00:00Z","idle_timeout_seconds":1800,"last_activity_at":"2026-11-18T12:00:00Z","checkpoint":null,"restored_from":null,"runtime_reachable":true,"labels":{"team":"core"},"created_by":{"actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","username":"dana"},"created_at":"2026-11-18T12:00:00Z","updated_at":"2026-11-18T12:00:04Z","ended_at":null,"tools":false}
```

| Member | Value |
|---|---|
| `id` | `sbx_` and 32 lowercase hexadecimal characters |
| `workspace` | the slug of the workspace the sandbox is attached to |
| `session_id` | the agent session it belongs to, or `null` |
| `profile` | the name of the profile it was created from |
| `purpose` | a string of at most 200 characters, `""` when none was given |
| `state` | `provisioning`, `ready`, `stopped`, `completed`, `failed` or `deleted` |
| `state_reason` | why it is in its state, or `null`; after a failed provisioning `provision_failed`, `checkout_failed` or `credential_missing`; after a stop `api`, `idle` or `archived`; after a deletion `api`, `expired` or `workspace_deleted`; `exit` for a main process that ended with another code than 0 and `runtime_error` for a sandbox the runtime lost (see [the life of a sandbox](#the-life-of-a-sandbox)) |
| `command` | the main process's command, a list of strings, or `null` for a sandbox driven by exec |
| `env` | the `env` the creator gave, `{}` when none |
| `exit_code` | the main process's exit code once it ended, else `null` |
| `checkout` | `{path, ref, sha, branch}`: the working directory (`/workspace`), the starting ref, the commit checked out and the work branch (`null` on a workspace that is not a fork); `null` when no checkout is made |
| `image`, `cpu`, `memory`, `gpu` | the profile's values when the sandbox was created |
| `credentials` | the names of the credentials attached to it, sorted |
| `exposed_secrets` | the names of the secrets whose values its environment holds, sorted; never a value |
| `workspace_rules` | the ids of the workspace sandbox rules recorded at its creation |
| `hub_access` | `{scopes, paths, expires_at}` of its hub credential (`scopes` sorted, `paths` `null` without a limit), or `null` when it has none; never the credential's value |
| `policy_revision` | `0` at creation |
| `expires_at` | when it expires |
| `idle_timeout_seconds` | the profile's idle timeout in seconds, `0` when the idle stop is off |
| `last_activity_at` | the time of its last activity; at creation its creation time |
| `checkpoint` | its latest checkpoint, `{at, ok, error, refs}` (see [the life of a sandbox](#the-life-of-a-sandbox)), or `null` |
| `restored_from` | the id of the sandbox whose checkpoint it was restored from, or `null` |
| `runtime_reachable` | `false` while the sandbox runtime cannot be reached; the other members then show what the hub stored |
| `labels` | the creator's labels, `{}` when none |
| `created_by` | `{actor_type, actor_id, username}` of the creating credential's user |
| `created_at`, `updated_at` | [timestamps](#timestamps-and-durations) in UTC, whole seconds |
| `ended_at` | when it stopped, completed, failed or was deleted; `null` until then, and again after a start |
| `tools` | `true` when the sandbox's profile named a tool server when the sandbox was created, so that [the tools of a sandbox](#the-tools-of-a-sandbox) are served for it; the path is never shown. `false` for a sandbox created before profiles could name one |

The states move only so: a new sandbox is `provisioning`; `provisioning`
becomes `ready` or `failed`; `ready` becomes `completed`, `failed`,
`stopped` or `provisioning`; `stopped`, `completed` and `failed` become
`provisioning` again (a start); every state but `deleted` becomes
`deleted`, which is final. Each move is conditional on the state the
sandbox has, so two paths racing for one sandbox make one move and write
one event.

## `POST /api/v1/workspaces/{slug}/sandboxes`

Creates a sandbox on the workspace. It is a write route of the
[sandbox routes](#sandbox-routes) that acts for a user: it needs a PAT's
`sandboxes:write`, write access to the workspace, and a caller that is not
the admin token. It takes an `Idempotency-Key`.

Every body member is optional; a member that is `null` counts as absent, an
absent body is `{}`, and members the route does not define are ignored.

| Member | Value |
|---|---|
| `profile` | the name of a configured profile; absent, the profile marked `default` |
| `command` | a list of 1 to 256 non-empty strings, the main process; absent, `null`, an idle sandbox driven by exec |
| `env` | an object of at most 64 strings; each key matches `^[A-Za-z_][A-Za-z0-9_]*$`, does not start with `PARLEY_` or `OPENSHELL_` and is not a secret the profile exposes, and key and value together have at most 8,192 bytes |
| `checkout` | `false` for no checkout, or an object `{ref, branch}` of strings: `ref` a branch, tag or commit id of the workspace's repository (absent, the fork's base branch, or the default branch of a workspace that is not a fork); `branch` a valid branch name, the work branch, on a task fork only (absent, the fork's name) |
| `restore_from` | the id of a sandbox of the same workspace whose latest checkpoint has `ok` true; not with `checkout: false` or a profile whose `hub_access` is `none` |
| `ttl` | a [duration](#timestamps-and-durations) from `1m` to the profile's `ttl`; absent, the profile's `ttl` |
| `purpose` | a string of at most 200 characters; absent, `""` |
| `labels` | an object of at most 16 strings; a key is 1 to 63 letters, digits, `.`, `_` and `-`, begins and ends with a letter or digit and does not start with `parley.` or `openshell.`; a value has at most 256 bytes and no control character |
| `session_id` | a session the session-linking rule accepts for the caller and the workspace; absent, the fork's session when the workspace is a fork that has one, else `null` |

```json
{"profile":"agent","command":["make","test"],"env":{"CI":"1"},"ttl":"1h","purpose":"run tests","labels":{"team":"core"}}
```

`expires_at` is the creation time plus `ttl`, and never later than a task
fork's `expires_at`. The answer is `201 Created` with the sandbox object in
state `provisioning`; the hub then provisions it in the background (see
[Provisioning a sandbox](#provisioning-a-sandbox)), and the sandbox becomes
`ready` or `failed` without a further request. The sandbox and a
`parley.sandbox.create` event are written in one store transaction:

```json
{"seq":42,"id":"…","type":"parley.sandbox.create","time":"2026-11-18T12:00:00Z","workspace":"demo--t1","actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","resource_type":"sandbox","resource_id":"sbx_3f2a9c0e7b1d4a5e8f6c2b9a0d1e4f7a","action":"create","session_id":null,"metadata":{"command":"make","credentials":["model"],"expires_at":"2026-11-18T13:00:00Z","exposed_secrets":["NPM_TOKEN"],"hub_access":{"scopes":["audit:write","changes:read","changes:write","git:read","git:write","sessions:write"],"paths":null,"expires_at":"2026-11-18T13:00:00Z"},"profile":"agent","purpose":"run tests","restored_from":null,"workspace_rules":[]}}
```

`metadata.command` is the command's first argument, or `null`.

**Credentials and exposed secrets.** Before anything is created the hub
resolves, along the workspace's resolution chain (workspace, then the
parent for a fork whose owner can write to it, then the org, then the
owner; the secret half of [`GET /api/v1/workspaces/{slug}/vars/resolved`](#get-apiv1workspacesslugvarsresolved)), the secret of
every credential of the profile's `credentials` and `optional_credentials`
and every name of its `expose_secrets` and `optional_expose_secrets`. An
optional name that resolves nowhere is skipped. `credentials` then names
the attached credentials and `exposed_secrets` the placed secrets. Only the
profile decides which secrets a sandbox can read: no request member, label
or `env` exposes one. Values are read again when the sandbox is
provisioned.

**The hub credential.** With a profile whose `hub_access` is `fork`, a
creating credential that holds at least one of `git:read`, `git:write`,
`changes:read`, `changes:write`, `sessions:write` and `audit:write` (an API
key holds all six), and `[server].external_url` set, the hub mints in the
same transaction a PAT bound to the sandbox's workspace, owned by the
creating user and named `sandbox:<id>`, with exactly those of the six scopes
the creating credential holds by name, the creating PAT's `paths`, and the
sandbox's `expires_at` as its expiry; its `parley.token.bind` event is
written with it (see [Workspace-bound tokens](#workspace-bound-tokens)).
Otherwise `hub_access` is `null` and nothing is minted. The checkout is
made only for a sandbox whose hub credential can read git (`checkout` is
`null` otherwise). The credential's value is never stored and appears in no
answer, event or log line.

**Quotas.** The hub counts the sandboxes in state `provisioning` or `ready`
against four limits of the `[sandbox]` section (see
[configuration.md](configuration.md#sandboxes)), in this order:
`max_sandboxes` (the hub), `max_per_org` (the workspaces of the
workspace's org and their forks), `max_per_workspace` (the workspace and
its forks; a fork counts toward its parent's) and `max_per_user` (the
sandboxes the creating user created). The count and the insert are one
step, so concurrent requests never pass a limit. A request at a limit is
answered with the first limit reached:

```json
{"error":{"code":429,"message":"sandbox limit reached: max_per_workspace","error_type":"sandbox_capacity","limit":"max_per_workspace","maximum":50,"current":50}}
```

The checks run in this order, the first that fails answering, and a refused
request stores no sandbox, mints no token, writes no event and asks the
runtime nothing:

| Status | When |
|---|---|
| `201 Created` | the sandbox was created; the body is the sandbox object |
| `404` `sandboxes_disabled` | the hub has no `[sandbox]` section |
| `403` `insufficient_scope` | a PAT without `sandboxes:write` |
| `404` `not_found` | the caller cannot read the workspace, or no workspace has the slug |
| `403` `forbidden` | read access only; or the admin token (`the admin token cannot create a sandbox`) |
| `400` `invalid_request` | the body is not an object, or a member breaks its rule, the message naming the member: `profile: no profile "nope"; name one of agent, plain`, `profile: no profile is marked default; name one of …`, `ttl: must be from 1m to 4h`, `command: must have 1 to 256 arguments`, `env: key "PARLEY_X" must not start with PARLEY_ or OPENSHELL_`, `checkout: branch is taken on a task fork only`, `checkout: ref "nope" names no commit of workspace "demo"`, `restore_from: the sandbox "…" has no checkpoint to restore`, … |
| `400` `unknown_session` | the session-linking rule refuses `session_id` (`session_id: no session "<id>"`) |
| `409` `workspace_archived` | the workspace is archived |
| `409` `parent_archived` | the workspace is a fork whose parent is archived |
| `409` `workspace_busy` | the workspace's clone has not finished, or a maintenance operation runs on it |
| `409` `workspace_not_ready` | the workspace's clone failed |
| `503` `secrets_unavailable` | the secret store cannot be read: no encryption key, or a value it cannot decrypt |
| `422` `credential_missing` | a required credential's secret or a required exposed secret resolves nowhere: `credentials` and `secrets` name the missing ones, sorted, and `scopes_searched` the scopes of the chain searched, in order |
| `429` `sandbox_capacity` | a quota would be exceeded: `limit`, `maximum`, `current` |
| `500` `internal` | minting the hub credential, writing the sandbox or its event, or the commit failed; nothing is stored |

```json
{"error":{"code":422,"message":"missing credentials model; searched workspace, parent, org, user","error_type":"credential_missing","credentials":["model"],"secrets":[],"scopes_searched":["workspace","parent","org","user"]}}
```

The route never answers `502`: it does not wait for the runtime, and an
unreachable runtime shows as a sandbox that ends `failed` with
`provision_failed`.

## Provisioning a sandbox

Once the creating transaction has committed, the hub provisions the sandbox
in the background, at most 8 at once, each one at most once at a time. It
looks for sandboxes to provision when a create wakes it, every second, and
at every start, so a sandbox that was being provisioned when the hub
stopped is provisioned again from the first step at the next start, and
ends `ready` or `failed` with exactly one event. Before the first step the
hub removes whatever the runtime holds of the sandbox (the sandbox and the
providers labelled `parley.sandbox=<id>` or named after it), which a
provisioning the hub stopped in may have left, with the providers'
secret values.

1. It resolves the credentials and exposed secrets again; a required one
   that is gone ends the sandbox `failed` with `credential_missing`.
2. It registers at the runtime one provider per attached credential, named
   `<id>-<credential>`, and one for the hub credential, `<id>-hub`, each
   labelled `parley.sandbox=<id>`. After a restart, the hub credential
   minted earlier is revoked and replaced by one with the same scopes,
   paths and expiry.
3. It creates the sandbox at the runtime, named by its id, with the
   profile's `image`, `cpu`, `memory` and `gpu`, the working directory
   `/workspace`, the providers, the environment below and the policy: the
   profile's base policy with the protected rules — the hub rule (the hub
   credential's provider on the scheme, host and port of
   `[server].external_url`, for the API and git paths of the sandbox's
   workspace) when it has a hub credential, then one rule per attached
   credential naming its provider — and the added rules (see
   [The network policy of a sandbox](#the-network-policy-of-a-sandbox)).
   A base policy that refers to the provider of the sandbox's hub
   credential ends the sandbox `failed` with `provision_failed` before
   anything is created, and so does a policy the runtime rejects, with an
   error that names the first workspace sandbox rule the runtime rejects it
   with, or the profile when it rejects the policy without any.
4. It checks the repository out (below) and, with `restore_from`, has the
   working directory restored from the source sandbox's checkpoint: each
   checkpointed branch is set to its commit (a dirty one to the commit
   under the extra one), and the uncommitted changes of the checked-out
   branch are put back in the working tree, uncommitted, when that branch
   is the checkpoint's dirty one. A checkpoint without refs leaves the
   plain checkout. A checkpoint ref that no longer exists (a reclone
   discards them) or a git command of the restore that fails ends the
   sandbox `failed` with `checkout_failed`.
5. It starts `command` when there is one.
6. It moves the sandbox to `ready` and writes `parley.sandbox.ready` (actor
   `system`, metadata `checkout`) with that change.

The environment of the sandbox is, later entries winning: the creator's
`env`; the profile's `env`; each placed exposed secret under its own name
with its real value; `PARLEY_WORKSPACE` (the slug), `PARLEY_SANDBOX_ID`,
`PARLEY_SESSION_ID` (only with a session) and, with a hub credential,
`PARLEY_ENDPOINT_URL` (`[server].external_url`) and `PARLEY_REMOTE` (the
workspace's git URL on the hub). The variables an attached credential's
provider profile declares, and `PARLEY_API_KEY` for the hub credential, are
left to the runtime, which puts the credential's placeholder there; the
real value of a credential never enters the sandbox.

The checkout, when the sandbox has one, runs these git commands in the
sandbox: a credential helper for the hub's scheme and host that answers
with the username `x-token-auth` and the content of `PARLEY_API_KEY`; `git
clone --filter=blob:none --no-checkout` of `PARLEY_REMOTE` into
`/workspace`; on a fork, a checkout of the fork's own branch named
`checkout.branch` when it has one, else the creation of that branch at
`checkout.ref` and its checkout; on a workspace that is not a fork, a
detached checkout of `checkout.ref`; `user.name` and `user.email` set to
the creating user's; and `git rev-parse HEAD`, which becomes
`checkout.sha`.

A failure at any step, or a provisioning that has not ended after 10
minutes, moves the sandbox to `failed` with `state_reason`
`credential_missing` (step 1), `checkout_failed` (a git command that fails)
or `provision_failed` (every other step, an unreachable runtime included),
sets `ended_at`, removes whatever the runtime holds of the sandbox (by its
label and its names, also what an earlier run left), revokes the hub
credential and writes `parley.sandbox.fail` (actor `system`, metadata
`reason` and `error`). Any text the hub writes about a sandbox into an
event, an error or its log has every resolved value of that sandbox (its
credentials' secrets, its exposed secrets of at least 8 characters and its
hub credential) replaced by `[redacted]`, and is cut to 1,024 bytes. When
the sandbox was moved meanwhile (deleted), the provisioning changes nothing
in it, writes no event and removes what it created.

## `GET /api/v1/workspaces/{slug}/sandboxes/{id}`

Reads one sandbox of the workspace. It is a read route of the
[sandbox routes](#sandbox-routes): a PAT needs `sandboxes:read` (or
`sandboxes:write`, which the scope check counts for it), and the caller
read access to the workspace. The answer is `200` with the sandbox object
as stored, in every state, `deleted` included, with `runtime_reachable`
from the runtime client.

An id that is not `sbx_` and 32 lowercase hexadecimal characters, that no
sandbox has, or that names a sandbox of another workspace is answered with
the same body:

```json
{"error":{"code":404,"message":"the workspace has no such sandbox","error_type":"sandbox_not_found"}}
```

| Query parameter | Value |
|---|---|
| `wait_for` | `ready`, held until the state is anything but `provisioning`, or `ended`, held until it is `stopped`, `completed`, `failed` or `deleted` |
| `wait` | the most seconds to hold the request, a whole number from 1 to 60 |

The two are given together. The request is answered at once when the
condition holds, within a second of the change that makes it hold, or when
`wait` seconds have passed, always `200` with the sandbox as it is then. A
client that leaves ends the wait. One without the other, another
`wait_for`, or a `wait` outside 1 to 60 is `400 invalid_request` (`wait_for
and wait are given together`, `wait_for must be ready or ended`, `wait
must be a whole number of seconds from 1 to 60`), answered at once.

## `GET /api/v1/workspaces/{slug}/sandboxes`

Lists the workspace's sandboxes, a read route as above, as a
[list](#lists), newest first by `created_at` and then by `id`, both
descending.

| Query parameter | Value |
|---|---|
| `state` | repeatable; the sandboxes in any of the given states. Absent, every state but `deleted` |
| `session_id` | only the sandboxes of this session |
| `label` | repeatable, `key=value`; only the sandboxes that have every given label |
| `include_forks` | `true` or `false`; with `true` on a workspace that is not a fork, also the sandboxes of those of its forks the caller can read (and a bound PAT's binding covers); ignored on a fork |
| `limit`, `cursor` | as for every [list](#lists) |

An unknown `state` (`state must be one of provisioning, ready, stopped,
completed, failed, deleted`), a `label` without `=` or with an empty key
(`label must be key=value`) and an `include_forks` that is neither `true`
nor `false` are `400 invalid_request`. A workspace without sandboxes
answers `{"items":[],"next_cursor":null}`.

## The life of a sandbox

Besides being created and used, a sandbox is stopped and started again,
extended, deleted and checkpointed: on request, through the five routes
below, and by the hub on its own. Every one of them changes the sandbox
only by a move that is conditional on the state the sandbox has, so racing
requests and the hub's own work make one move and write one event.

**Checkpoints.** A checkpoint saves the git state of the working directory
to the sandbox's workspace on the hub: for every local branch whose head
commit the workspace's repository does not have, and for the checked-out
branch when the working tree has uncommitted changes, a ref
`refs/parley/sandboxes/<sandbox id>/<branch>`. The uncommitted changes, new
files that git does not ignore included, are saved as one extra commit on
top of the branch head, by `parley <parley@localhost>`; with no branch
checked out the ref's branch is `HEAD`. The branches, the index and the
working tree in the sandbox stay as they were. Files that git ignores are
not saved, nor is an untracked file larger than 100 MiB, whose path is then
listed in `error` (`skipped: <path>, <path>`) of a checkpoint that is still
`ok`. The commits a checkpoint carries to the hub travel as one git bundle,
which is held to [`[git].max_push_size`](configuration.md), the limit of a
push: a bundle larger than it fails the checkpoint, `ok` false with
`error` naming the maximum push size, and stores no ref (no limit when the
key is `"0"`, its default). The refs are written to the workspace's repository directly: they
are no version of a change and trigger nothing, and they live as long as
the workspace (a fork's go with the fork). Git keeps no ref `a/b` beside a
ref `a`: when a branch of a later checkpoint clashes so with the ref of a
branch an earlier checkpoint of the same sandbox saved (`feature/a` after
`feature`, or the reverse), the earlier ref is deleted and the later one
stored, and when the later refs cannot be stored the earlier ref is put
back. A checkpoint is limited to 5
minutes and stores its refs all at once or not at all. The sandbox's
`checkpoint` member then holds the latest attempt:

```json
{"at":"2026-11-18T12:30:00Z","ok":true,"error":null,"refs":[{"branch":"t1","sha":"4c1f0e9b7a2d3c5e6f8091a2b3c4d5e6f7081920","dirty":true}]}
```

`refs` are sorted by branch; `dirty` is true for a ref that ends in the
extra commit. A failed attempt has `ok` false, its reason in `error`
(scrubbed as every sandbox text is, at most 1,024 bytes) and the `refs` of
the last checkpoint that succeeded, which still exist. A sandbox without a
checkout, or whose working directory is not a git repository, gets `ok`
true and no refs; one without a hub credential (its profile's
`hub_access` is `none`) gets no checkpoint at all. Each attempt writes one
`parley.sandbox.checkpoint` event with metadata `{ok, refs, error}` and adds
one to `parley_sandbox_checkpoints_total{result}` (`ok` with refs, `empty`
without, `failed`). The hub takes a checkpoint, when the profile's
`checkpoint` is true and the sandbox is `ready`, before a stop, before a
deletion (the `checkpoint` query of `DELETE` decides for that one) and
when the main process has ended; a failed one never keeps the stop or the
deletion from happening. A deletion with the workspace takes none.

**What the hub does on its own** (actor `system`):

- Every minute it stops each `ready` sandbox whose `last_activity_at` is
  more than `idle_timeout_seconds` before the hub's time (`0` disables
  this), with `state_reason` `idle`; a stop the runtime refuses is tried
  again a minute later.
- At `expires_at`, whatever the state, it deletes the sandbox with
  `state_reason` `expired`, after a checkpoint of a `ready` one; it checks
  every minute and at a timer set to the earliest `expires_at`. A sandbox
  on a fork whose own `expires_at` has passed too goes with the fork:
  `workspace_deleted`, no checkpoint.
- Archiving a workspace stops the `ready` sandboxes of the workspace and
  of its forks, with `state_reason` `archived`, and a start is refused
  while it stays archived. Deleting a workspace or a fork, by request or by
  expiry, first deletes its sandboxes with `state_reason`
  `workspace_deleted` and no checkpoint, and then removes their records and
  the fork's checkpoint refs with it. A fork with a `provisioning` or
  `ready` sandbox is not removed early after its changes landed.
- At start, before it listens, and every `[sandbox].reconcile_interval` it
  compares its records with the runtime: a `ready` sandbox whose main
  process has exited is checkpointed, its compute released (the working
  directory stays) and recorded `completed` (exit code 0) or `failed` with
  `state_reason` `exit`, with `exit_code` and `parley.sandbox.exit`
  (metadata `{exit_code}`); a `ready` sandbox the runtime restarts reads
  `provisioning` until the runtime runs it again, with no event; a
  `ready` sandbox, or a `provisioning` one already created at the runtime,
  that the runtime no longer has is recorded `failed` with `state_reason`
  `runtime_error` and `parley.sandbox.fail` (metadata `reason` and
  `error`); what the runtime holds labelled `parley.sandbox=<id>` of no
  live sandbox is deleted there; the idle stops and expiries that came due
  meanwhile are done. While the runtime cannot be reached a pass changes
  nothing but by expiry.
- Every hour it removes the records of sandboxes deleted more than 24 hours
  before, with their stored logs; their ids then answer `404
  sandbox_not_found`.
- Stopping or restarting the hub stops and deletes nothing at the runtime:
  a `ready` sandbox is still `ready` after a restart.

The hub credential is revoked whenever a sandbox loses its compute: at a
stop, a deletion, the end of the main process and a failure the
reconciliation records. A start mints a new one.

## `POST /api/v1/workspaces/{slug}/sandboxes/{id}/checkpoint`

Takes a checkpoint of a `ready` sandbox now, with the caller as the event's
actor, whatever the profile's `checkpoint` says. It is a write route of the
[sandbox routes](#sandbox-routes), with no body. The answer is `200` with
the sandbox object, whose `checkpoint` is the new one (`at` the time of the
request) and whose `state` is still `ready`; for a sandbox without a hub
credential, `200` with `checkpoint` `null`. A sandbox in any other state,
`deleted` included, is refused before the runtime is asked:

```json
{"error":{"code":409,"message":"the sandbox is stopped, which this operation does not allow","error_type":"sandbox_not_ready"}}
```

A checkpoint that fails is recorded on the sandbox and answered `502
sandbox_runtime_error`, its message holding the reason.

## `POST /api/v1/workspaces/{slug}/sandboxes/{id}/stop`

Stops a `ready` sandbox: a checkpoint when the profile's `checkpoint` is
true, the release of its compute at the runtime (the runtime keeps the
working directory), the revocation of the hub credential, and the move to
`stopped` with `state_reason` `api` and `parley.sandbox.stop` (metadata
`{reason}`). A write route with no body; the admin token may stop any
sandbox. The answer is `200` with the sandbox object. A sandbox in any
other state is answered `200` as it is, and nothing changes. When the
runtime cannot be reached or refuses the stop, the answer is `502
sandbox_runtime_error` and the sandbox stays `ready` with its credential.

## `POST /api/v1/workspaces/{slug}/sandboxes/{id}/start`

Starts a `stopped`, `completed` or `failed` sandbox again. A write route
with no body. In one transaction it checks the workspace as a create does
(an archived workspace is `409 workspace_archived`, a fork of an archived
parent `409 parent_archived`), resolves the credentials and exposed secrets
of the sandbox's snapshot again (`422 credential_missing`, `503
secrets_unavailable`), applies the quotas (`429 sandbox_capacity` with
`limit`, `maximum` and `current`), mints a new hub credential with the
scopes and paths of `hub_access`, owned by the sandbox's creator and
expiring at `expires_at`, and moves the sandbox to `provisioning` with
`exit_code`, `state_reason` and `ended_at` cleared and
`parley.sandbox.start` (actor the caller). An optional credential or secret
that no longer resolves is dropped from `credentials` or
`exposed_secrets`. The answer is `200` with the sandbox in `provisioning`;
a refusal changes nothing, mints nothing and calls no runtime. The profile
is not consulted: a sandbox whose profile was removed from the
configuration starts from what it recorded at creation.

The sandbox is then provisioned as a start: the hub registers its
providers again with the values resolved now (a secret rotated while the
sandbox was stopped reaches it), starts the runtime's sandbox with the
composed policy and the environment, rebuilds the working directory from
the checkout and the sandbox's latest checkpoint when the runtime no
longer has it, starts `command` again when there is one, and moves the
sandbox to `ready` with `parley.sandbox.ready`. A failure ends it `failed`
as a create's does, but keeps the runtime's sandbox and its working
directory.

A `provisioning` or `ready` sandbox is answered `200` as it is; a `deleted`
one

```json
{"error":{"code":409,"message":"the sandbox is deleted and cannot be started","error_type":"sandbox_deleted"}}
```

## `PATCH /api/v1/workspaces/{slug}/sandboxes/{id}`

Changes a sandbox's `ttl`, `purpose` or `labels`. A write route; every
member is optional and an absent one leaves its value as it is.

| Member | Value |
|---|---|
| `ttl` | a [duration](#timestamps-and-durations) string from `1m` to the profile's `ttl` as recorded at creation, counted from now: `expires_at` becomes now plus `ttl`, cut to the fork's `expires_at` on a task fork, and the hub credential's expiry (`hub_access.expires_at`) moves with it. It may make `expires_at` earlier |
| `purpose` | a string of at most 200 characters |
| `labels` | the labels, replacing the old ones, by the rules of a create |

Every member is checked before anything changes; a violation is `400
invalid_request` naming the member (`ttl: must be a string`). The answer is
`200` with the sandbox object; no event is written. A deleted sandbox is
`409 sandbox_deleted` (`the sandbox is deleted and cannot be changed`).

## `DELETE /api/v1/workspaces/{slug}/sandboxes/{id}`

Deletes a sandbox in any state: a checkpoint of a `ready` one when the
query parameter `checkpoint` is `true`, or is absent and the profile's
`checkpoint` is true; the removal of the runtime's sandbox and its
providers; the revocation of the hub credential; and the move to `deleted`
with `state_reason` `api`, `ended_at` and `parley.sandbox.delete` (metadata
`{reason}`). A write route; the admin token may delete any sandbox. The
answer is `204` with no body, also for a sandbox already deleted, for which
nothing is done. A `checkpoint` other than `true` or `false` is `400
invalid_request` (`checkpoint must be true or false`). When the runtime
cannot be reached or fails, the sandbox is recorded `deleted` all the same
and the reconciliation removes what the runtime still holds. The record
stays readable for 24 hours.

## Using a sandbox

Five routes use a sandbox, all under
`/api/v1/workspaces/{slug}/sandboxes/{id}`: `POST …/exec`, `PUT …/files`,
`GET …/files`, `GET …/logs` and `GET …/attach`. `GET …/files` and
`GET …/logs` are read routes of the [sandbox routes](#sandbox-routes), the
other three write routes. After the pipeline's checks (authentication, the
rate limit, the body checks, the sandbox feature check, the binding guard,
the PAT scope and workspace access), each runs, in this order, and answers
with the first that fails:

1. the sandbox: an id that is not `sbx_` and 32 lowercase hexadecimal
   characters, that no sandbox has, or that names a sandbox of another
   workspace is `404 sandbox_not_found` (`the workspace has no such
   sandbox`);
2. for exec, `PUT …/files` and attach, the no-widening rule of the
   [sandbox routes](#sandbox-routes) against the sandbox's `hub_access`
   (`403 insufficient_scope`); a download and the logs are not subject to
   it;
3. the validation of the request (`400`, `413`, `415`, below);
4. the sandbox's state, `409 sandbox_not_ready` (`the sandbox is <state>,
   which this operation does not allow`) in every state the route does not
   take:

| Route | States |
|---|---|
| exec, `PUT …/files`, attach | `ready` |
| `GET …/files` | `ready`, `completed`, `failed` |
| `GET …/logs` | every state, `deleted` included, while the record exists |

When the runtime cannot be reached or fails while it serves exec, a file
transfer or attach, the answer is `502 sandbox_runtime_error`, its message
the runtime's text with the sandbox's secret values (the values of its
credentials, its exposed secrets of at least 8 bytes and its hub credential)
replaced by `[redacted]`, then cleared as on every sandbox route, and cut to
1,024 bytes. The logs are served from the hub's own store and do not need the
runtime. None of the five routes emits a hub event.

Three more routes pass the same checks and serve [the tools of a
sandbox](#the-tools-of-a-sandbox): `GET …/tools`, `POST …/tools/{name}` and
`POST …/mcp`.

**Activity.** `last_activity_at` of the sandbox is set to the hub's time when
an exec starts and when it ends, in both forms; when a file transfer starts
and when it ends; when an attach opens, when it closes, and for a frame from
the caller once at least 60 seconds have passed since the connection last set
it; and when a line of the main process's output is stored. It never moves
backwards. A request refused by any check, the exec limit included, a log
read, a policy line and a main process that prints nothing do not set it.

**Operation observers.** When an exec, an attach, an upload or a download
that passed every check ends, the hub tells each observer registered with
`(*sandbox.Service).RegisterObserver` once, after the answer: the kind
(`exec`, `attach`, `upload`, `download`), `sandbox_id`, `workspace`,
`session_id`, `actor_type`, `actor_id`, `started_at` and `duration_ms`; for
an exec also `command`, `exit_code`, `timed_out` and `result` (`ok` for exit
code 0, `nonzero`, `timeout`, or `error` for a runtime failure or a command
killed because its caller left or the hub stopped); for a transfer `path` and
`bytes`. The record holds no stdin, output, terminal bytes, file content or
environment value. An observer that fails or panics is logged (`a sandbox
operation observer failed`) and changes nothing for the caller. An exec that
passed every check also adds 1 to `parley_sandbox_exec_total{result}`.

## `POST /api/v1/workspaces/{slug}/sandboxes/{id}/exec`

Runs a command in a `ready` sandbox, as the workload user, whether or not its
main process runs, and answers once it has ended. Body (JSON, at most
8,388,608 bytes):

| Member | Value |
|---|---|
| `command` | required, a list of 1 to 256 strings, none empty |
| `workdir` | the directory the command runs in; the sandbox's working directory `/workspace` when absent. It is not confined to the working directory: the runtime's policy confines the command |
| `env` | an object of strings laid over the sandbox's environment, the request's value winning: at most 64 entries, each name a variable name that does not start with `PARLEY_` or `OPENSHELL_` and is not a name of the sandbox's `exposed_secrets`, name and value together at most 8,192 bytes |
| `stdin` | text written to the command's standard input, which is then closed |
| `stdin_base64` | the same as standard base64; not together with `stdin`. At most 4,194,304 bytes after decoding |
| `timeout` | a whole number followed by `s`, `m`, `h` or `d`, above `0s` and at most `[sandbox].max_exec_timeout`; `60s` (or that maximum when it is smaller) when absent |

```json
{"command":["sh","-c","echo hi; exit 3"],"env":{"CI":"1"},"timeout":"90s"}
```

The answer is `200`, also for a non-zero exit code:

```json
{"exit_code":3,"stdout":"hi\n","stderr":"","truncated":false,"timed_out":false,"duration_ms":12}
```

- At most 1,048,576 bytes of each stream are returned; more is read and
  dropped, `truncated` is `true`, and the stream is cut back to its last
  complete UTF-8 character.
- A stream whose returned bytes are not valid UTF-8 is returned in
  `stdout_base64` or `stderr_base64`, with its text member `""`; the
  `_base64` member is absent otherwise.
- A command still running when its timeout has passed is killed, and the
  answer has `timed_out: true`, `exit_code: null` and the output collected
  until then. `exit_code` is null only then.
- When the caller leaves or the hub begins to stop, the command is killed
  and no answer is written.

With an `Accept` header that names `text/event-stream` the answer is `200`
with `Content-Type: text/event-stream` and `Cache-Control: no-cache`, sent
once the runtime has started the command: frames `event: stdout` and
`event: stderr`, in the order the output arrives, each with data
`{"data":"…"}`, or `{"data_base64":"…"}` for a chunk that is not valid
UTF-8, and at most 32,768 bytes of output, then one frame `event: exit` with
data `{"exit_code","timed_out","duration_ms"}`, after which the stream ends.
Every byte of output is sent; there is no cap and no `truncated`. A timeout
kills the command and ends with `exit_code: null, timed_out: true`. Closing
the stream kills the command. A runtime failure after the stream began, the
caller leaving or the hub stopping ends it without an `exit` frame.

```
event: stdout
data: {"data":"hi\n"}

event: exit
data: {"exit_code":3,"timed_out":false,"duration_ms":12}
```

At most `[sandbox].max_concurrent_execs` commands run at once in one sandbox
through exec, collected and streamed together; a command that ended in any
way no longer counts, and neither the main process nor an attach terminal
counts.

| Status | When |
|---|---|
| `400` `invalid_request` | a member is refused, the message naming it (`command: is required`, `command: must be a list of strings`, `command: must have 1 to 256 arguments`, `env: must have at most 64 entries`, `stdin: must not be sent with stdin_base64`, `stdin_base64: must be base64`, `stdin: must have at most 4194304 bytes`, `timeout: must be a whole number followed by s, m, h or d, such as 60s`, `timeout: must be above 0s and at most 30m`, and the other `env` rules of the create request) |
| `413` `payload_too_large` | the body is over 8,388,608 bytes |
| `429` `rate_limited` | `[sandbox].max_concurrent_execs` commands already run in the sandbox (`the sandbox runs as many commands as it may at once; try again once one has ended`), without `Retry-After` |
| `502` `sandbox_runtime_error` | the runtime could not run the command |

Every refusal is the JSON envelope, also with `Accept: text/event-stream`:
it is decided before the command starts.

## `PUT /api/v1/workspaces/{slug}/sandboxes/{id}/files`

Writes into a `ready` sandbox's working directory.

| Query parameter | Value |
|---|---|
| `path` | required: absolute, or relative to the working directory `/workspace` |
| `archive` | `tar`, or absent |
| `mode` | the file's mode, an octal number from `0000` to `0777`; `0644` when absent; not with `archive=tar` |

Without `archive` the body (`Content-Type: application/octet-stream`)
becomes the content of the file at `path`: missing parent directories are
created and an existing file is replaced. With `archive=tar` the body
(`application/x-tar` or `application/octet-stream`) is a tar stream unpacked
under the directory `path`, which is created when missing; its directories
and regular files are written with the modes of their entries. The body is
streamed to the runtime as it arrives. The answer is `200`:

```json
{"path":"/workspace/proj","bytes":21,"entries":3}
```

`path` is the absolute path, `bytes` the file bytes written and `entries` the
number of files and directories written (`1` for a single file).

`path` is cleaned of `.` and `..` without following links in the sandbox, and
must be `/workspace` or below it; a tar entry's name must stay below `path`.
Unpacking stops at the first refused entry, and what was written before it
stays, as does what was written before the size limit was passed.

| Status | When |
|---|---|
| `400` `invalid_request` | `path: is required`; `archive: must be tar`; `mode: must be an octal number from 0000 to 0777`; `mode: must not be sent with archive=tar, whose entries carry their modes`; `request body is not a readable tar stream`; `tar entry "<name>" is neither a regular file nor a directory` (a link or a device) |
| `400` `path_outside_workdir` | `path must be inside the working directory /workspace`; `tar entry "<name>" is outside the upload's path` |
| `413` `payload_too_large` | a `Content-Length` above `[sandbox].max_transfer_size` (nothing is written), or a body without one that passes it (`the transfer exceeds [sandbox].max_transfer_size`); the connection is closed |
| `415` `unsupported_media_type` | another `Content-Type` (`request body must be application/octet-stream`, or `… or application/x-tar` with `archive=tar`) |
| `502` `sandbox_runtime_error` | the runtime failed |

## `GET /api/v1/workspaces/{slug}/sandboxes/{id}/files`

Reads from a sandbox in state `ready`, `completed` or `failed`, with the
`path` and `archive` of the upload. A file is answered `200` with its bytes,
`Content-Type: application/octet-stream` and its `Content-Length`. With
`archive=tar` the answer is `200` with `Content-Type: application/x-tar`: for
a directory a tar stream of its directories and regular files, named
relative to it, with their modes; for a file a tar stream of one entry named
by its base name. Entry names come from the requested path: an entry the
runtime reports outside the requested directory is left out, so no name is
absolute or leads out of the directory it is unpacked in.

| Status | When |
|---|---|
| `400` `invalid_request` | `path: is required`; `archive: must be tar` |
| `400` `path_outside_workdir` | as for the upload |
| `400` `path_is_directory` | a directory without `archive=tar` (`path is a directory; ask for it with archive=tar`) |
| `404` `path_not_found` | the path does not exist, or the runtime no longer has the working directory (`the sandbox has no such path`) |
| `413` `payload_too_large` | a file larger than `[sandbox].max_transfer_size`; no byte is sent |
| `502` `sandbox_runtime_error` | the runtime failed before the answer began |

A tar stream that passes `[sandbox].max_transfer_size`, or that the runtime
fails after the answer began, is cut off: the hub aborts the connection, so
the caller's read ends with an error instead of a clean end of the body.

## `GET /api/v1/workspaces/{slug}/sandboxes/{id}/logs`

The stored lines of a sandbox, in every state, `deleted` included, from the
hub's own store; the runtime is never asked. While a sandbox is `ready` the
hub reads its main process's output and the runtime's policy log for it
(looking for ready sandboxes every 5 seconds, and opening a broken stream
again within 5 seconds, from the second of the last stored line, without
storing a line twice) and stores each line, oldest first:

| Member | Value |
|---|---|
| `time` | when the runtime produced the line, in whole seconds |
| `source` | `process` or `policy` |
| `stream` | `stdout` or `stderr` for a process line; `null` for a policy line |
| `line` | the text without its line end; a line longer than 16,384 bytes is stored as consecutive lines of at most 16,384 bytes. A policy entry without text gets `<action> <host>:<port> <program>` |
| `action`, `program`, `host`, `port` | policy lines only: `allow` or `deny` and what was decided on |
| `credential` | policy lines of a credentialed request only: the provider name |

Each exact match of a value of the sandbox's exposed secrets of at least 8
characters is replaced by `[redacted]` before a line is stored. Lines older
than `[sandbox].log_retention`, and those of a sandbox whose record is gone,
are removed by the hourly maintenance task `sandbox_log_retention`; the lines
of a sandbox also go in the transaction that removes its record.

```json
{"items":[{"time":"2026-01-02T03:04:05Z","source":"process","stream":"stdout","line":"token=[redacted]"},{"time":"2026-01-02T03:04:06Z","source":"policy","stream":null,"line":"deny example.com:443 /usr/bin/curl","action":"deny","program":"/usr/bin/curl","host":"example.com","port":443}],"next_cursor":null}
```

| Query parameter | Value |
|---|---|
| `source` | `process` or `policy`; both when absent |
| `since` | an RFC 3339 timestamp; only lines at or after it |
| `tail` | an integer of at least 1; only the last that many matching lines, still paged |
| `follow` | `true` or `false` |
| `limit`, `cursor` | as for every [list](#lists); not with `follow=true` |

With `follow=true` the answer is `200` with `Content-Type: text/event-stream`
and `Cache-Control: no-cache`: one frame `event: log` per line, its data the
line's object, first the stored lines the other parameters select, then each
matching line within a second of its storage. It ends when the caller closes
it, when the hub stops, or when the sandbox becomes `deleted`, after the
lines stored until then; it stays open while the sandbox is `stopped`,
`completed` or `failed`.

`400 invalid_request`, before any stream: `source: must be process or
policy`, `since: must be an RFC 3339 timestamp`, `tail: must be an integer
of at least 1`, `follow: must be true or false`, `cursor and limit: must not
be sent with follow=true, which sends every selected line`, and a `limit` or
`cursor` a list refuses.

## `GET /api/v1/workspaces/{slug}/sandboxes/{id}/attach`

A WebSocket upgrade (version 13) that opens a new shell, `/bin/sh`, in a
`ready` sandbox, as the workload user, in the working directory, with the
sandbox's environment, on a terminal of `cols` by `rows` (query parameters,
each an integer from 1 to 1,000; 80 by 24 when absent). The credential is the
`Authorization` header, as on every route; a request without an `Origin`
header is accepted.

- Binary frames carry terminal bytes in both directions. A frame from the
  caller larger than 1 MiB (1,048,576 bytes) ends the connection and the
  shell.
- A text frame `{"resize":{"cols":…,"rows":…}}` with both values from 1 to
  1,000 resizes the terminal; any other text frame is ignored.
- When the caller closes the connection, or the hub begins to stop, the hub
  ends that shell and nothing else: the main process, running execs and
  other terminals go on, and the sandbox stays `ready`.
- When the shell ends or the runtime's terminal fails, the hub sends the
  output it received and closes the connection.

The terminal is opened before the upgrade, so every refusal is an HTTP error
envelope and no `101`: the checks of [Using a sandbox](#using-a-sandbox),
`400 invalid_request` for a request that is not a WebSocket upgrade (`this
route takes a WebSocket upgrade (version 13)`: judged on the first value of
each header, so `Upgrade` must be `websocket` and the first `Connection`
header must hold `upgrade`; `Sec-WebSocket-Key` must be present,
`Sec-WebSocket-Version` must be `13`, and `Sec-WebSocket-Protocol` may offer
at most one protocol) or a `cols` or `rows` outside
1 to 1,000 (`cols: must be an integer from 1 to 1000`), and `502
sandbox_runtime_error` when the runtime cannot open the terminal.

## The tools of a sandbox

A sandbox whose profile names a tool server (`tool_server`, see
[configuration.md](configuration.md#sandboxes)) serves that program's tools
to callers outside it: reading, searching and editing files and running
commands in its working directory. The tools run inside the sandbox, so what
they can read, write and reach is what the sandbox allows. The sandbox
object's `tools` says whether it has them. Three routes under
`/api/v1/workspaces/{slug}/sandboxes/{id}` serve them: `GET …/tools`,
`POST …/tools/{name}` and `POST …/mcp`.

All three are read routes of the [sandbox routes](#sandbox-routes): a PAT
needs `sandboxes:read` (which `sandboxes:write` satisfies) and the caller
read access to the workspace, and a workspace-bound token may use them on
its workspace. After the pipeline's checks each runs, in this order, and
answers with the first that fails:

1. the sandbox, `404 sandbox_not_found`, as for [using a
   sandbox](#using-a-sandbox);
2. its tools: a sandbox with `tools: false` is `404 tools_unavailable` (`the
   sandbox's profile names no tool server`);
3. the validation of the body (`400`, `413`);
4. the sandbox's state: only `ready` is served, any other is `409
   sandbox_not_ready`;
5. for a call, the tool's name, and then the place among the sandbox's
   commands.

**Tool sets.** What a caller may list and call is decided for each request:

| Set | Who | Tools |
|---|---|---|
| `full` | a caller who would be allowed to exec in the sandbox: a PAT holds `sandboxes:write`, the caller has write access to the workspace, and its credential covers the sandbox's `hub_access` by the no-widening rule of the [sandbox routes](#sandbox-routes). The admin token and an API key hold every scope | every tool the tool server lists |
| `read_only` | every other caller the route lets through, a caller with write access whose credential does not cover the hub credential included | the tools the tool server lists under `--read-only`: the full set without `write_file`, `edit_file`, `execute` and `run_command` |

**Definitions.** The hub reads a set's tool definitions from the sandbox by
running `<tool_server> tools`, with `--read-only` for the read-only set, in
`/workspace`. It keeps them in memory per sandbox and set until the sandbox
leaves `ready` or the hub restarts, and then reads them again on the next
request. A listing does not set `last_activity_at`, takes no place among the
sandbox's commands and emits nothing.

**A call.** For one call the hub takes a place among the sandbox's commands,
shared with exec (`[sandbox].max_concurrent_execs`), writes `{"name",
"input"}` to the file `/tmp/parley-tools/requests/<32 hex characters>.json`
of the sandbox with mode `0600`, runs

```
<tool_server> call --request <file> --dir /workspace --index-dir /tmp/parley-tools/index --timeout <seconds>s
```

in `/workspace` with the sandbox's environment and no input, with
`--read-only` added for the read-only set, and removes the file with `rm -f`
when the command has ended, however it ended; a removal that fails is logged
(`cannot remove a tool call's request file`) and changes nothing else. Both
directories are outside the working directory, so neither is part of a
checkpoint. The time bound is the route's (below), cut to
`[sandbox].max_exec_timeout` when it is longer. `last_activity_at` is set
when the call starts and when it ends.

The one JSON object the tool server prints, with a boolean `ok`, is the
tool's result, whatever `ok` says: a tool that fails (a file not found, an
edit that is not unique) is a result with `ok: false` and the tool's `error`
and `detail`. A call still running when its bound passes is killed and gives
the result `{"ok": false, "error": "timeout", "detail": "the call did not
end within <bound>", "data": {}}`. Anything else is a **failed call**: a
tool server that cannot be started, exits with a status other than 0, prints
something that is not one such object or prints more than 4 MiB is
`tool_server_failed`, and a runtime that cannot be reached
`sandbox_runtime_error`, each with a message cleared of the sandbox's secret
values and the hub's and cut to 1,024 bytes. The same holds for a listing
whose output is not one JSON array of definitions, each with a name no
other has and an input schema that is an object. A result is not redacted: like the output of
an exec it may hold whatever the sandbox holds.

A call whose caller leaves, or that runs when the hub begins to stop, is
killed and gets no answer.

**The event.** Every call that took its place emits one
`parley.sandbox.tool_call` event when it ends: resource type `sandbox`, the
sandbox's id, workspace and `session_id`, the caller as actor, and the
metadata

```json
{"tool":"read_file","ok":true,"duration_ms":41,"via":"rest","set":"full"}
```

`via` is `rest` or `mcp` and `set` the caller's set. A call with `ok: false`
adds `error`: the tool's error code, cut to 64 characters, `timeout`, `tool_server_failed` (also
for a call nobody waited for any more) or `sandbox_runtime_error`. The
arguments and the result never appear. A `code_mode` call is one event,
whatever its script called. The call also adds 1 to
`parley_sandbox_tool_calls_total{tool, outcome}`, where `outcome` is `ok`,
`tool_error` for a result with `ok: false`, `timeout`, or `failed`, and its
duration is observed in `parley_sandbox_tool_call_duration_seconds{tool}`.
A request refused before it took its place, and a listing, emits and counts
nothing. Tool calls are not told to the operation observers of [using a
sandbox](#using-a-sandbox) and do not count in `parley_sandbox_exec_total`.

## `GET /api/v1/workspaces/{slug}/sandboxes/{id}/tools`

Lists the caller's tool set, in the tool server's order: `200`

```json
{"set":"read_only","tools":[{"name":"read_file","description":"…","input_schema":{"type":"object","properties":{"path":{"type":"string"}}},"output_schema":null}]}
```

`set` is `full` or `read_only`. `input_schema` and `output_schema` are the
tool server's, member for member; `output_schema` is `null` for a tool
without one.

Errors beside those of the checks above: `502 tool_server_failed` and `502
sandbox_runtime_error`.

## `POST /api/v1/workspaces/{slug}/sandboxes/{id}/tools/{name}`

Calls the tool `name`. The body is empty or a JSON object of at most 8 MiB:

| Member | Value |
|---|---|
| `input` | the tool's input, a JSON object of at most 4 MiB as JSON; `{}` when absent or `null` |
| `timeout` | the call's time bound, a [duration](#timestamps-and-durations) above `0s`; `60s` when absent; one above `[sandbox].max_exec_timeout` is cut to it |

It answers `200` with the tool's result and how long the call took, also
when the tool failed or the call timed out:

```json
{"ok":false,"data":{},"error":"not_found","detail":"no such file","duration_ms":38}
```

`data` is a JSON object, `{}` when the tool gave none. `error` and `detail`
are absent for a result without them, and `text`, the text the tool means
for a model, when the tool server gave none.

| Status | When |
|---|---|
| `400` `invalid_request` | a body that is not a JSON object, an `input` that is not an object (`input: must be a JSON object`), a `timeout` that is not a string or not a duration above `0s` |
| `403` `insufficient_scope` | a tool the full set holds, called by a caller with the read-only set (`the tool "execute" changes the sandbox, which this credential may only read`) |
| `404` `tool_not_found` | a name no set holds (`the sandbox has no tool "<name>"`) |
| `413` `payload_too_large` | an `input` over 4 MiB as JSON, or a body over 8 MiB |
| `429` `rate_limited` | `[sandbox].max_concurrent_execs` commands already run in the sandbox, without `Retry-After` |
| `502` `tool_server_failed`, `sandbox_runtime_error` | a failed call |

A refused call writes nothing to the sandbox and runs nothing in it for the
call; finding the tool may read the definitions once.

## `POST /api/v1/workspaces/{slug}/sandboxes/{id}/mcp`

An MCP server over Streamable HTTP for the one sandbox, for a harness that
speaks MCP: the URL and a bearer credential are all it needs. The credential
is any the two routes above accept. The route is stateless: it sets and
reads no `Mcp-Session-Id`, every request is answered on its own, as
`application/json`, with or without an `initialize` before it, and a restart
of the hub between two requests is not visible. `GET` and `DELETE` are `405
method_not_allowed`. As an MCP client does, a request sends `Content-Type:
application/json` and an `Accept` that names both `application/json` and
`text/event-stream`. A body may have 8 MiB.

| Method | Answer |
|---|---|
| `initialize` | the server name `parley`, the hub's version as `GET /version` reports it, the capability `tools` alone, and `instructions` of one sentence naming the sandbox and the workspace |
| `ping` | an empty result |
| `tools/list` | the caller's tool set, in the tool server's order and on one page: each tool's `name`, `description`, `inputSchema` and, where it has one, `outputSchema`, as the tool server gave them |
| `tools/call` | the call, with `arguments` as the input (`{}` when absent) under a bound of 10 minutes |
| a notification | accepted |
| any other | the JSON-RPC error `-32601` |
| a batch (a JSON array) | the JSON-RPC error `-32600` (`batches are not served`) |

The result of `tools/call` has `isError` the negation of `ok`; one text
content, the tool server's `text` when it gave one, otherwise the JSON of
`data` for a success and `<error>: <detail>` for a failure, a timeout
included; and `structuredContent`, `data`, when the tool has an output
schema.

| JSON-RPC error | When |
|---|---|
| `-32602` | a tool outside the caller's set or one no set holds (`unknown tool "<name>"`); `arguments` that are not an object or over 4 MiB as JSON |
| `-32603` | a failed call, with the message of `tool_server_failed` or `sandbox_runtime_error` |
| `-32000` `sandbox busy` | `[sandbox].max_concurrent_execs` commands already run in the sandbox |

What is decided before the MCP message is read is answered as plain HTTP
with the hub's error object, not as JSON-RPC: `401`, `403`, `404`
(`sandbox_not_found`, `tools_unavailable`, `sandboxes_disabled`), `409
sandbox_not_ready`, `413` for a body over 8 MiB, `429` for the [rate
limit](#rate-limit), and `502 tool_server_failed` or `sandbox_runtime_error`
while the tool definitions cannot be read. The route offers no resources,
prompts, completions or logging and no MCP authorization flow.

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
{"items":[{"name":"agent","default":true,"description":"an agent","image":"agent:1","cpu":"2","memory":"4Gi","gpu":1,"ttl":"4h","ttl_seconds":14400,"idle_timeout":"30m","policy_summary":["a.test","b.test"],"credentials":["model"],"optional_credentials":["search"],"exposed_secrets":[{"name":"A_TOKEN","optional":true},{"name":"Z_TOKEN","optional":false}],"hub_access":"none","env_keys":["A","B"],"checkpoint":false,"allow_proposals":false,"tools":true}],"next_cursor":null}
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
| `tools` | `true` when the profile sets `tool_server` |

The path of a policy file, the path of the tool server, the values of `env`, and the lists
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
runtime nothing, so it answers the same while the runtime is unreachable;
each object's `runtime_reachable` is the runtime client's flag (see
[The sandbox object](#the-sandbox-object)).

| Query parameter | Value |
|---|---|
| `state` | only the sandboxes in this state, one of `provisioning`, `ready`, `stopped`, `completed`, `failed` and `deleted`, written exactly so |
| `user` | only the sandboxes created by the user with this id |
| `limit`, `cursor` | as for every [list](#lists): `limit` from 1 to 200, default 50 |

`state` and `user` together select the sandboxes that match both. Any other
`state`, `limit` or `cursor`, and a cursor of a page with other filters, is
answered `400 invalid_request`. A failure to read the records is `500
internal` with the message `internal error`, and no items.

## The network policy of a sandbox

A sandbox reaches the hub, the endpoints of its credentials and what its
operator allowed, and nothing else. The runtime gets its policy in three
parts, for a create and again for every start:

1. the base policy of the sandbox's profile (its `policy` file, or the
   built-in policy);
2. the protected rules: the hub rule when the sandbox has a hub credential,
   then one rule per attached credential, in the order of `credentials`.
   No base policy, workspace rule or approved rule removes or narrows them;
3. the added rules: the workspace sandbox rules recorded when the sandbox
   was created, in the order of `workspace_rules`, then the rules approved
   for the sandbox itself, in the order they were approved. An approved
   rule equal, as a JSON value, to a recorded workspace rule is added once.

A rule is an object in OpenShell's policy schema that the hub stores and
passes on without reading it. At the runtime a workspace rule is named by
its id (`sbr_…`) and an approved rule by its proposal's id (`sbp_…`).

When a sandbox is stored, the hub records, in the same transaction, the
workspace sandbox rules its workspace holds at that moment (for a task
fork, its parent's), oldest first: their ids are the sandbox's
`workspace_rules` and the `workspace_rules` of its `parley.sandbox.create`,
and a copy of each body is kept with the sandbox. The set never changes: a
rule saved later never reaches the sandbox, and a rule deleted later stays
in its policy, across stop and start. This holds whatever the profile and
`[sandbox].allow_workspace_rules`. A sandbox created with `restore_from` is
a new sandbox: it gets the workspace rules that exist at its own creation
and none of the rules approved for its source alone.

### Denials

While a sandbox is `ready`, the hub follows its policy log at the runtime
and takes each entry whose action is `deny`. Every denial adds 1 to
`parley_sandbox_denials_total`, once: how far each sandbox's log was taken
is stored with each denial, so a log read again, after a restart or once
the sandbox is `ready` again, counts none of it twice. The first denial of
each (program, host, port) of a sandbox writes one event, and later
denials of the same three write none, whatever their method and path;
which ones were reported is stored, so a restart reports none twice:

```json
{"seq":51,"id":"…","type":"parley.sandbox.deny","time":"2026-11-18T12:01:00Z","workspace":"demo--t1","actor_type":"system","actor_id":null,"resource_type":"sandbox","resource_id":"sbx_3f2a9c0e7b1d4a5e8f6c2b9a0d1e4f7a","action":"deny","session_id":null,"metadata":{"program":"npm","host":"registry.example.org","port":443,"method":null,"path":null}}
```

`method` and `path` are `null` for a blocked connection. The sandbox's
secret values in the texts read `[redacted]`; while they cannot be known,
a denial is not taken, and its log is read again 5 seconds later. A policy
log that breaks, or a runtime that cannot be reached, is logged and read
again 5 seconds later; the hub keeps serving. A sandbox's log stops being followed once it leaves
`ready`. Its denials stay visible in its logs (see
[`GET …/logs`](#get-apiv1workspacesslugsandboxesidlogs)).

### Proposals

When a workload is blocked, it can ask, with the runtime's own mechanism,
for one more rule. Every 5 seconds the hub lists the runtime's pending
proposals. A proposal of a sandbox whose profile had `allow_proposals` when
the sandbox was created is stored as `open`, once, with this event. Its
`summary` is stored with the sandbox's secret values `[redacted]` (at most
1,024 bytes), in the event, the proposal and a workspace sandbox rule saved
from it; its `rule` is stored as the runtime gave it, since the hub hands
it back to the runtime unchanged:

```json
{"seq":52,"id":"…","type":"parley.sandbox.proposal","time":"2026-11-18T12:01:05Z","workspace":"demo--t1","actor_type":"system","actor_id":null,"resource_type":"sandbox","resource_id":"sbx_3f2a9c0e7b1d4a5e8f6c2b9a0d1e4f7a","action":"proposal","session_id":null,"metadata":{"proposal_id":"sbp_0c9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f","summary":"npm → registry.example.org:443","max_severity":"high"}}
```

`max_severity` is the highest severity of the findings in the order `low`,
`medium`, `high` (a severity the runtime names otherwise ranks below
`low`), `null` without findings. A proposal of a sandbox whose profile did
not allow proposals is rejected at the runtime, and nothing is stored or
written; one of a sandbox the hub does not hold, or holds as `deleted`, is
left alone. When the sandbox is deleted, its `open` proposals become
`withdrawn` in the same transaction, with no event; they are removed with
its record.

A proposal has exactly these members:

```json
{"id":"sbp_0c9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f","sandbox_id":"sbx_3f2a9c0e7b1d4a5e8f6c2b9a0d1e4f7a","rule":{"host":"registry.example.org","port":443},"summary":"npm → registry.example.org:443","findings":[{"severity":"high","message":"a package registry can serve arbitrary code"}],"state":"approved","scope":"workspace","workspace_rule_id":"sbr_5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b","decided_by":{"actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","username":"dana"},"decided_at":"2026-11-18T12:03:00Z","comment":"npm registry","created_at":"2026-11-18T12:01:05Z"}
```

| Member | Value |
|---|---|
| `id` | `sbp_` and 32 lowercase hexadecimal characters |
| `sandbox_id` | the sandbox that asked |
| `rule` | the proposed rule, as the runtime gave it |
| `summary` | the runtime's one line about it, the sandbox's secret values `[redacted]` |
| `findings` | the runtime's findings, `{severity, message}` each, `[]` when none |
| `state` | `open`, `approved`, `rejected` or `withdrawn` |
| `scope` | `sandbox` or `workspace` once approved, else `null` |
| `workspace_rule_id` | with `scope` `workspace`, the workspace sandbox rule the approval saved or found identical; else `null` |
| `decided_by` | `{actor_type, actor_id, username}` of the decider, or `null`; for the admin token `actor_id` and `username` are `""` |
| `decided_at`, `comment` | set by a decision, else `null` |
| `created_at` | when the hub stored it |

### Workspace sandbox rules

A workspace sandbox rule is an approved rule saved on a repository
workspace, which every sandbox created later on the workspace or on any of
its forks gets. A task fork holds none of its own: its slug stands for its
parent's rules. A workspace holds at most 50. Deleting a workspace deletes
its rules, with no event. A rule has exactly these members:

```json
{"id":"sbr_5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b","rule":{"host":"registry.example.org","port":443},"summary":"npm → registry.example.org:443","findings":[{"severity":"high","message":"a package registry can serve arbitrary code"}],"from_proposal":{"sandbox_id":"sbx_3f2a9c0e7b1d4a5e8f6c2b9a0d1e4f7a","proposal_id":"sbp_0c9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f","workspace":"demo--t1"},"created_by":{"actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","username":"dana"},"created_at":"2026-11-18T12:03:00Z"}
```

| Member | Value |
|---|---|
| `id` | `sbr_` and 32 lowercase hexadecimal characters |
| `rule` | the rule as it was approved |
| `summary`, `findings` | the proposal's, as they were acknowledged |
| `from_proposal` | `{sandbox_id, proposal_id, workspace}`: the proposal it came from and the workspace of its sandbox, which may be a fork |
| `created_by` | `{actor_type, actor_id, username}` of the approver |
| `created_at` | when it was saved |

## `GET /api/v1/workspaces/{slug}/sandboxes/{id}/proposals`

Lists a sandbox's proposals. A read route of the [sandbox
routes](#sandbox-routes): a PAT needs `sandboxes:read`, and a token bound to
the workspace may call it. The sandbox is looked up in the workspace: an
unknown id, the id of another workspace's sandbox and a removed record are
`404 sandbox_not_found`; a `deleted` sandbox is served.

| Query parameter | Value |
|---|---|
| `state` | `open` (the default), `approved`, `rejected` or `withdrawn`, written exactly so and given once |
| `limit`, `cursor` | as for every [list](#lists) |

The answer is `200` with a [list](#lists) of [proposals](#proposals) in that
state, oldest first by `created_at` and then by `id`. It asks the runtime
nothing. Any other `state` is `400 invalid_request`.

## `POST /api/v1/workspaces/{slug}/sandboxes/{id}/proposals/{proposal_id}/approve`

Approves an open proposal. A write route of the [sandbox
routes](#sandbox-routes) that refuses every workspace-bound token: the
binding guard answers such a token, the sandbox's own hub credential
included, `403 token_bound` before anything else is checked. The body is a
JSON object, all members optional (no body counts as `{}`, a `null` member
as absent):

| Member | Value |
|---|---|
| `comment` | a string |
| `acknowledge_findings` | a boolean; `true` is needed to approve a proposal with a finding of severity `high` |
| `scope` | `sandbox` (the default) or `workspace` |

Approving adds the rule to the sandbox's policy at the runtime when the
sandbox is `provisioning` or `ready`; in `stopped`, `completed` and
`failed` the runtime is not asked and the rule is in the policy of the next
start. A start and the decisions of the sandbox take turns from the start's
composition of the policy until the runtime has started the sandbox with
it, so an approval made while the sandbox starts is either in the policy
it starts with or added to it after. Then, in one transaction, the proposal becomes `approved` with
`scope`, `decided_by`, `decided_at` and `comment`, the rule is kept for the
sandbox for its life, its `policy_revision` grows by one, and this event is
written with the caller as actor:

```json
{"seq":53,"id":"…","type":"parley.sandbox.proposal_decided","time":"2026-11-18T12:03:00Z","workspace":"demo--t1","actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","resource_type":"sandbox","resource_id":"sbx_3f2a9c0e7b1d4a5e8f6c2b9a0d1e4f7a","action":"proposal_decided","session_id":null,"metadata":{"proposal_id":"sbp_0c9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f","decision":"approved","acknowledged_findings":true,"scope":"workspace","rule_id":"sbr_5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b"}}
```

With `scope` `workspace` the approval also saves the rule on the sandbox's
workspace, or on its parent when that is a task fork, in the same
transaction, and records its id as the proposal's `workspace_rule_id` and
the event's `rule_id` (`null` for scope `sandbox`). After the decision it
writes, in the same transaction:

```json
{"seq":54,"id":"…","type":"parley.sandbox.rule_add","time":"2026-11-18T12:03:00Z","workspace":"demo","actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","resource_type":"sandbox_rule","resource_id":"sbr_5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b","action":"rule_add","session_id":null,"metadata":{"rule_id":"sbr_5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b","summary":"npm → registry.example.org:443","max_severity":"high","from_proposal":{"sandbox_id":"sbx_3f2a9c0e7b1d4a5e8f6c2b9a0d1e4f7a","proposal_id":"sbp_0c9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f","workspace":"demo--t1"}}}
```

`workspace` is the workspace that holds the rule and `session_id` the
sandbox's. A rule equal, as a JSON value (key order and whitespace aside),
to one the workspace holds is not saved twice: the approval names that rule
and writes no `parley.sandbox.rule_add`, also when the workspace holds 50.

The answer is `200` with the proposal as stored. Once the runtime has
taken the rule, the decision is stored even when the caller leaves before
the answer, since the runtime cannot remove the rule again; a transaction
that fails all the same is logged with the sandbox's and the proposal's
ids. The checks come in this order, and a refused approval changes
nothing:

| Case | Answer |
|---|---|
| a workspace-bound token | `403 token_bound` |
| a PAT without `sandboxes:write` | `403 insufficient_scope` |
| read-only access to the sandbox's workspace | `403 forbidden` |
| a body that is not an object, a `comment` that is not a string, an `acknowledge_findings` that is not a boolean, a `scope` other than `sandbox` or `workspace` | `400 invalid_request` |
| `scope` `workspace` while `[sandbox].allow_workspace_rules` is false | `403 workspace_rules_disabled` |
| `scope` `workspace` without write access to the workspace that would hold the rule (a fork owner who can only read the parent) | `403 forbidden` |
| `scope` `workspace` on a fork whose parent is archived | `409 parent_archived` |
| an unknown sandbox id | `404 sandbox_not_found` |
| an unknown proposal id, or one of another sandbox | `404 not_found` |
| a proposal that is not `open` | `409 proposal_decided` |
| a finding of severity `high` without `acknowledge_findings` `true` | `409 findings_not_acknowledged` |
| `scope` `workspace`, 50 rules and none identical | `429 sandbox_rule_limit_reached` |
| the runtime rejects the rule | `409 conflict`, the runtime's reason in `message` (secret values `[redacted]`, at most 1,024 bytes) |
| the runtime cannot be reached | `502 sandbox_runtime_error` |

Decisions of one sandbox are made one at a time, and approvals that save
on one workspace one at a time: of two decisions of one proposal sent
together, one is applied and the other answered `409 proposal_decided`,
and of two promotions racing for a workspace's last place, one saves its
rule and the other is answered `429`.

## `POST /api/v1/workspaces/{slug}/sandboxes/{id}/proposals/{proposal_id}/reject`

Rejects an open proposal. Declared like the approve route; the body is a
JSON object with an optional `comment` (a string), and its other members
are ignored. In one transaction the proposal becomes `rejected` with
`decided_by`, `decided_at` and `comment`, and `parley.sandbox.proposal_decided`
is written with `decision` `rejected`, `acknowledged_findings` `false`,
`scope` `null` and `rule_id` `null`. A finding of severity `high` needs no
acknowledgement, and `policy_revision` does not change. Then the runtime is
told, in any state of the sandbox; when it cannot be told, the hub logs a
line with the sandbox's and the proposal's ids and still answers `200` with
the proposal. The refusals are the approve route's for a bound token, the
scope, read-only access, the body, an unknown sandbox or proposal and a
proposal that is not `open`.

## `GET /api/v1/workspaces/{slug}/sandbox-rules`

Lists the [workspace sandbox rules](#workspace-sandbox-rules) of a
workspace; with a task fork's slug, its parent's. A read route of the
[sandbox routes](#sandbox-routes): a PAT needs `sandboxes:read`, and a
token bound to the fork may call it under the fork's slug. `limit` and
`cursor` are those of every [list](#lists). The answer is `200` with the
rules, oldest first by `created_at` and then by `id`; `items` is `[]` for a
workspace without rules.

## `DELETE /api/v1/workspaces/{slug}/sandbox-rules/{rule_id}`

Deletes a workspace sandbox rule. A write route of the [sandbox
routes](#sandbox-routes) that refuses every workspace-bound token (`403
token_bound`), and works in any state of the workspace, archived included.
It deletes the rule and writes, in one transaction, with the caller as
actor:

```json
{"seq":60,"id":"…","type":"parley.sandbox.rule_remove","time":"2026-11-18T13:00:00Z","workspace":"demo","actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","resource_type":"sandbox_rule","resource_id":"sbr_5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b","action":"rule_remove","session_id":null,"metadata":{"rule_id":"sbr_5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b","summary":"npm → registry.example.org:443"}}
```

and answers `204` with no body. No sandbox changes: the ones that have the
rule keep it. After the scope and access checks, a task fork's slug is
`400 not_supported_on_fork` (delete the rule on the parent), and an
unknown rule id, a rule of another workspace or one already deleted is
`404 sandbox_rule_not_found`.

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
[secrets](#secrets) and [variables](#variables) among them, is removed in the same
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
[secrets](#secrets) and [variables](#variables) among them, is removed in the same
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

## Variables

A variable is a named value that is readable through the API. Each
variable belongs to one of three scopes, each with its own routes:

| Scope | Routes | Owner |
|---|---|---|
| user | `/api/v1/user/vars` | the caller's user |
| org | `/api/v1/orgs/{slug}/vars` | the org `{slug}` names, by its slug or its id (see [Org selectors](#org-selectors)) |
| workspace | `/api/v1/workspaces/{slug}/vars` | the workspace `{slug}`, and no other: a task fork's own variables are not its parent's |

Variables are stored in the clear and apart from [secrets](#secrets): a key
may name a secret and a variable in the same scope, the 100 entries of a
scope are counted for each apart, and every variable route works on a hub
without an encryption key. A PAT needs `vars:read` for a list and for the
[resolved](#get-apiv1workspacesslugvarsresolved) variables, which `vars:write`
satisfies, and `vars:write` for a change. The user and org scopes answer
exactly as the [secret](#secrets) routes do: the admin token is refused on
`/user/vars` (`403` `forbidden`, `user-scoped endpoints require a user
credential`), and one `404` `not_found` body (`org not found`) answers an org
that does not exist, one the user is not a member of and a blocked one,
while the admin token reaches every org. On a workspace the lists and the
resolved variables need `read` and the changes need `write` (see
[Workspace access](#workspace-access)): the owner and the admin token read
and change, a member of the workspace's org reads, and a member's change is
`403` `forbidden` with the message `org members have read-only access to
this workspace`; anyone else gets the `404` `workspace not found` of a slug
with no workspace. The workspace's status and a maintenance operation play
no part. A workspace-bound PAT is confined by the binding guard first (see
[Workspace-bound tokens](#workspace-bound-tokens)).

No route reads one variable by key: `GET` on `…/vars/{key}` is `405`
`method_not_allowed` with `Allow: DELETE, PATCH`. Under
`/api/v1/workspaces/{slug}/vars/` the segment `resolved` names the resolved
variables: `PATCH` and `DELETE` on it are `405` `method_not_allowed` with
`Allow: GET`, before authentication and every other check but the rate
limit and the body limit, and never reach a variable. A variable stored
under the key `resolved` is reached under another casing, such as
`…/vars/RESOLVED`.

The variables of a user go with the user's deletion
([`DELETE /api/v1/users/{id}`](#delete-apiv1usersid)), those of an org with
the org's ([`DELETE /api/v1/orgs/{id}`](#delete-apiv1orgsid)), and those of a
workspace or a task fork with its deletion or expiry, in the transaction of
the delete and without a `parley.variable.delete` event; a delete that is
refused or undone keeps them, and a workspace created again under the slug
starts with none.

Every create, update and delete writes, in the transaction of the change,
one event per variable it changes: `parley.variable.create`,
`parley.variable.update` or `parley.variable.delete`, with `resource_type`
`variable`, `resource_id` the key as it is stored, `metadata` exactly
`{"scope","scope_id","key"}` (`scope_id` the user id, the org id or the
workspace slug), `workspace` the slug on the workspace routes and `null`
otherwise, and the caller as the actor. No event holds a value. If the event
cannot be written, nothing changes and the answer is `500` `internal`. A
refused request and a replayed create write none.

## `GET /api/v1/user/vars`, `GET /api/v1/orgs/{slug}/vars`, `GET /api/v1/workspaces/{slug}/vars`

Lists the variables of the scope, with their values, a [list](#lists) in the
standard pages. Who may call each route is in [Variables](#variables).

The list holds the scope's own variables, a task fork's without its
parent's, ordered by `key` ascending, compared byte by byte with `A` to `Z`
lowered. Each item has exactly `key`, as it was first stored, `value`,
`created_at` and `updated_at`, RFC 3339 in UTC with whole seconds. An empty
scope gets `items` `[]`.

```json
{"items":[{"key":"LOG_LEVEL","value":"info","created_at":"2026-11-17T10:30:15Z","updated_at":"2026-11-17T10:30:15Z"}],"next_cursor":null}
```

| Status | When |
|---|---|
| `200 OK` | the body is a page: `items` are variables as above |
| `400` `invalid_request` | a bad `limit` or `cursor` (see [Lists](#lists)) |
| `403` `insufficient_scope` | a PAT without `vars:read` or `vars:write` (`this route needs the scope vars:read`) |
| `403` `forbidden` | the admin token on `/user/vars` (`user-scoped endpoints require a user credential`) |
| `404` `not_found` | an org the caller may not reach or that does not exist (`org not found`); a workspace the caller has no level on or that does not exist (`workspace not found`) |

## `POST /api/v1/user/vars`, `POST /api/v1/orgs/{slug}/vars`, `POST /api/v1/workspaces/{slug}/vars`

Creates variables in the scope, all of them or none. Who may call each route
is in [Variables](#variables). It is a create route (see
[Idempotency](#idempotency)).

The body is a JSON object with `entries`, an array of at least one object
with `key` and `value`, held to the rules of a
[secret](#post-apiv1usersecrets-post-apiv1orgsslugsecrets-post-apiv1workspacesslugsecrets)'s
entries: a `key` of 1 to 255 ASCII letters, digits and `_`, not beginning
with a digit, stored as given; a `value` string of at most 262144 bytes,
absent the empty string.

```json
{"entries":[{"key":"LOG_LEVEL","value":"info"},{"key":"REGION","value":"eu"}]}
```

The body is checked in this order, and the first fault is answered:
`entries` is a non-empty array; every entry's key, in request order; every
entry's value, in request order; then no two keys are equal ignoring the
case of `A` to `Z`. So a request whose first entry has a bad value and whose
second a bad key is answered naming the second's key (a secret create
checks entry by entry instead). Then, in one
transaction: when the scope's variables and the entries together would be
more than 100, `429`, also when a key already exists; otherwise, when the
scope already holds a key equal to one of the entries ignoring case, `409`
naming the first such key in request order, as the request spells it.
Otherwise every entry is stored with one creation time, and one
`parley.variable.create` event is written per entry, in request order. Of
two creates of one key at the same time, one is answered `201` and the
other `409`.

The answer holds the entries in request order: exactly `key`, `value`,
`created_at` and `updated_at`, the times equal and the same for every item.

```json
{"items":[{"key":"LOG_LEVEL","value":"info","created_at":"2026-11-17T10:30:15Z","updated_at":"2026-11-17T10:30:15Z"},{"key":"REGION","value":"eu","created_at":"2026-11-17T10:30:15Z","updated_at":"2026-11-17T10:30:15Z"}]}
```

| Status | When |
|---|---|
| `201 Created` | every entry was stored; the body is `items` as above |
| `200 OK` | a replay of a recorded `Idempotency-Key` with the same body: the first answer; nothing is stored |
| `400` `invalid_request` | the body is refused, with the messages of a secret create (such as `key "1A" must not begin with a digit` or `the value of key "K" is longer than 262144 bytes`); nothing is stored |
| `403` `insufficient_scope` | a PAT without `vars:write` (`this route needs the scope vars:write`) |
| `403` `forbidden` | the admin token on `/user/vars`; a member of the workspace's org on `/workspaces/{slug}/vars` (`org members have read-only access to this workspace`) |
| `404` `not_found` | as for the list (see above) |
| `409` `conflict` | the scope holds one of the keys, ignoring case (`key "TOKEN" already exists`); nothing is stored |
| `413` `payload_too_large` | the body is over the hub's body limit (see [Request bodies](#request-bodies)) |
| `429` `secret_limit_reached` | the scope would hold more than 100 variables (`maximum of 100 entries per scope exceeded`); nothing is stored |

## `PATCH /api/v1/user/vars/{key}`, `PATCH /api/v1/orgs/{slug}/vars/{key}`, `PATCH /api/v1/workspaces/{slug}/vars/{key}`

Replaces the value of the scope's variable whose key equals `{key}` ignoring
the case of `A` to `Z`. Who may call each route is in
[Variables](#variables). The body is a JSON object with `value`, a string of
at most 262144 bytes, the empty string included; it is checked before the
variable is looked up. The key keeps the casing it was first stored with
and its `created_at`; `updated_at` is the time of the update. One
`parley.variable.update` event is written, naming the key as it is stored.

```json
{"key":"LOG_LEVEL","value":"debug","created_at":"2026-11-17T10:30:15Z","updated_at":"2026-11-17T10:31:45Z"}
```

| Status | When |
|---|---|
| `200 OK` | the value was replaced; the body is the variable as above |
| `400` `invalid_request` | `value` is absent, `null`, not a string (`the value of key "K" must be a string`) or longer than 262144 bytes (`the value of key "K" is longer than 262144 bytes`), `K` the key as the path gives it, whether or not the scope holds it. Nothing changes |
| `403` `insufficient_scope` | a PAT without `vars:write` (`this route needs the scope vars:write`) |
| `403` `forbidden` | as for the create (see above) |
| `404` `not_found` | as for the list (see above); or the scope holds no such variable (`key "K" not found`, `K` as the path gives it). A key held only by another scope, a task fork's parent included, or only as a secret is not found |

## `DELETE /api/v1/user/vars/{key}`, `DELETE /api/v1/orgs/{slug}/vars/{key}`, `DELETE /api/v1/workspaces/{slug}/vars/{key}`

Removes the scope's variable whose key equals `{key}` ignoring the case of
`A` to `Z`, and writes one `parley.variable.delete` event naming the key as
it was stored. Who may call each route is in [Variables](#variables). A
removed key can be created again.

| Status | When |
|---|---|
| `204 No Content` | the variable was removed; the body is empty |
| `403` `insufficient_scope` | a PAT without `vars:write` (`this route needs the scope vars:write`) |
| `403` `forbidden` | as for the create (see above) |
| `404` `not_found` | as for the update (see above) |

## `GET /api/v1/workspaces/{slug}/vars/resolved`

The variables the workspace or task fork `{slug}` runs with, a [list](#lists)
in the standard pages. It needs `read` on `{slug}` alone, and for a PAT
`vars:read` or `vars:write` (see [Variables](#variables)); it needs no access
to the other scopes it reads.

The workspace's chain of scopes is worked out at every request from its
record, never from the caller, so every caller gets the same answer. Highest
priority first:

| `origin` | Scope | When |
|---|---|---|
| `workspace` | the workspace's own | always |
| `parent` | the parent's workspace scope | a task fork whose parent still exists |
| `org` | the org of the workspace (of a fork, its parent's) | while the workspace's owner is a member of it and it is `active` |
| `user` | the user scope of the workspace's owner (of a fork, its creator) | while the owner exists |

For each key, compared ignoring the case of `A` to `Z`, the variable of the
first scope that holds it is answered whole, with its key casing, value and
times and that scope's `origin`; the items are ordered by key as on every
variable list. Deleting a fork's own variable shows its parent's again at
the next read. The chain is worked out first; the variables of all its
scopes are then read in one consistent read.

```json
{"items":[{"key":"LOG_LEVEL","value":"info","origin":"org","created_at":"2026-11-17T10:30:15Z","updated_at":"2026-11-17T10:30:15Z"},{"key":"REGION","value":"eu","origin":"parent","created_at":"2026-11-17T10:32:00Z","updated_at":"2026-11-17T10:32:00Z"}],"next_cursor":null}
```

| Status | When |
|---|---|
| `200 OK` | the body is a page of the merged variables, each with `origin` |
| `400` `invalid_request` | a bad `limit` or `cursor` (see [Lists](#lists)) |
| `403` `insufficient_scope` | a PAT without `vars:read` or `vars:write` |
| `404` `not_found` | a workspace the caller has no level on or that does not exist (`workspace not found`) |
| `405` `method_not_allowed` | `PATCH` or `DELETE` on this path (`Allow: GET`) |
| `500` `internal` | a scope could not be read (`internal error`); no items are answered |

## Sessions

An agent session ties together an agent's work on a workspace (forks,
pushes, commits, change versions, claims, lands and sandboxes) and the
tokens it spent. An agent opens a session for a workspace, reports token
usage while it works and completes the session with an outcome. The other
surfaces that take a `session_id` (a fork, a push option
`parley.session`, an API commit or rebase, a change, a land, a claim, a
sandbox) ask the session-linking rule below, and those that create
something record a link to the session in their own transaction.

Session ids and usage ids are UUIDs in the 8-4-4-4-12 hexadecimal form, in
any letter case in a body, a path or a query, stored and returned in lower
case; a create without an `id` gets a version 4 UUID. A `run_id` has the
form `YYYYMMDD_HHMMSS_` followed by six lower-case hexadecimal digits.
`node_id`, `archetype` and `model` are labels of at most 255 characters.

**The session object.** Every session route but the usage routes, which
answer usage records, answers this object, built when
the answer is built, so a usage report or a recorded link shows in the next
read:

```json
{"id":"0b7e1c52-3f4a-4d8e-9b21-5c6d7e8f9a01","workspace_slug":"demo","run_id":null,"node_id":null,"archetype":null,"model":"m1","status":"completed","user_id":"…","credential_type":"api_key","started_at":"2026-11-19T10:00:00Z","ended_at":"2026-11-19T10:01:30Z","duration_ms":90000,"error_message":null,"cache_creation_input_tokens":0,"metadata":null,"token_summary":{"total_input_tokens":10,"total_output_tokens":5,"total_cache_read_tokens":0,"models_used":["m1"]},"forks":["demo--t1"],"changes":[{"workspace":"demo","number":1}],"sandboxes":[],"counts":{"forks":1,"changes":1,"sandboxes":0,"versions":1,"pushes":0,"commits":0,"claims":0,"lands":0}}
```

| Member | Value |
|---|---|
| `id` | the session id |
| `workspace_slug` | the workspace the session works in, a repository or a task fork |
| `run_id`, `node_id`, `archetype`, `model` | as sent at creation, `null` when not sent |
| `status` | `active`, `completed`, `failed`, `timeout` or `terminated` |
| `user_id` | the user who created the session; `null` for the admin token |
| `credential_type` | `admin_token`, `api_key` or `pat`, the credential that created it |
| `started_at`, `ended_at` | [timestamps](#timestamps-and-durations); `ended_at` is `null` while `active` |
| `duration_ms` | an integer, `null` until a complete sets it; a forced close leaves it `null` |
| `error_message` | a string or `null` |
| `cache_creation_input_tokens` | an integer, `0` until a complete sets it |
| `metadata` | the JSON value sent at creation, as JSON, `null` when none |
| `token_summary` | the sums of the session's usage records: `total_input_tokens`, `total_output_tokens`, `total_cache_read_tokens`, and `models_used`, the distinct models in ascending order |
| `forks` | the slugs of the task forks created in the session, oldest first |
| `changes` | `{workspace, number}` of each change with a version recorded in the session, in the order each was first linked |
| `sandboxes` | `{workspace, id}` of each sandbox created in the session, oldest first |
| `counts` | the numbers of links per kind: `forks`, `changes` (distinct changes), `sandboxes`, `versions`, `pushes`, `commits`, `claims`, `lands` |

`forks`, `changes` and `sandboxes` hold at most their first 500 entries;
`counts` always holds the true totals. A link stays when the fork, sandbox,
change or workspace it names is removed.

A session is `active` from its creation; a complete moves it to
`completed`, `failed` or `timeout`, and a forced close to `terminated`. The
retention run ends a session still `active` after `[sessions].max_active_age`
(default `7d`) as `timeout`, with `error_message` `session exceeded maximum
active age`, and deletes ended sessions after `[sessions].max_age` (see
[configuration.md](configuration.md#retention)). The four end states are
final.

**Who may read a session.** Its creator, with any credential of that user;
any caller who may read its workspace; for a session on a task fork, any
caller who may read the fork's parent, also after the fork is gone; and the
admin token. The workspace's status plays no part, and a session whose
workspace no longer exists stays readable by its creator, the admin token
and, for a fork, the readers of its parent. A PAT bound to a workspace
reads only sessions whose workspace its binding covers, also those its
user created. A caller who may not read a session gets `404` `not_found`
with the message `session not found`, the same answer as for an id that
names no session; a path id that is not a UUID is `400` `invalid_request`.
A PAT needs `sessions:read` or `sessions:write` to read and
`sessions:write` to write (`403` `insufficient_scope`).

**Who may change a session.** A complete and a usage report are accepted
from the creator, with any credential of that user, and from the admin
token; a session the admin token opened is changed only by the admin
token. Anyone else who may read the session gets `403` `forbidden`.

**The session-linking rule.** A `session_id` named for an operation on a
workspace is accepted when the session exists, the caller created it or is
the admin token, and the session's workspace is the operation's workspace,
or one of the two is a task fork of the other; the session's own parent
is remembered, so a session on a fork that is gone still links to the
parent. Two forks of one parent do not link to each other. The session's
status is not read, so work may be linked to an ended session. Anything
else is `400` `unknown_session` on the surface that asked, with one
answer whatever the reason.

**A forced close.** Archiving a workspace ends the `active` sessions of the
workspace and of its forks, and deleting a workspace or removing or
expiring a task fork ends those of the workspace removed, in the same store
transaction: `status` `terminated`, `ended_at` the time of the operation,
`error_message` `workspace archived`, `workspace deleted` or `fork
expired`. Each closed session gets one `parley.session.force_closed` event
(`resource_type` `session`, `resource_id` and `session_id` the session id,
actor `system`, metadata `{"reason": …}`). After it a usage report is `409`
`session_not_active` and a complete answers `200` with the `terminated`
session.

## `POST /api/v1/sessions`

Opens a session. The body:

| Member | Value |
|---|---|
| `workspace_slug` | required, the workspace (a repository or a task fork) the caller may write to |
| `id` | a UUID; a new version 4 UUID when absent |
| `run_id`, `node_id`, `archetype`, `model` | optional labels, as above |
| `metadata` | any JSON value, kept as sent |

A JSON `null` member counts as absent. The answer is `201` with the session
object, `status` `active`, `started_at` the current time.

Sending an `id` that exists, created by the same user (or by the admin
token, both times) for the same workspace, answers `200` with the stored
session and changes nothing, also when the other members differ and when
the workspace has been archived since. Of several creates with one `id` at
once, exactly one stores the session and answers `201`; the others answer
`200`.

| Status | When, in this order |
|---|---|
| `403` `insufficient_scope` | a PAT without `sessions:write` |
| `400` `invalid_request` | `workspace_slug` missing or not a non-empty string (`workspace_slug is required and must be a non-empty string`) |
| `404` `not_found` | a PAT bound to a workspace whose binding does not cover `workspace_slug`; no such workspace; the caller's level on it is none (`workspace not found`, the same bytes in each case) |
| `403` `forbidden` | an org member, who may only read the workspace |
| `400` `invalid_request` | `id` not a UUID, `run_id` not of the run id form, a label not a string or longer than 255 characters, the message naming the member |
| `409` `conflict` | the `id` exists for another creator or another workspace |
| `409` `workspace_archived` | the workspace is archived and the `id` is not one this caller may replay; a workspace in another status accepts sessions |

## `POST /api/v1/sessions/{id}/complete`

Ends an `active` session. The body: `status` (required: `completed`,
`failed` or `timeout`), `error_message` (a string), `duration_ms` and
`cache_creation_input_tokens` (integers of at least 0). The status, the
end, the message, the duration (the milliseconds between `started_at` and
`ended_at` when absent) and the cache tokens (`0` when absent) are set in
one step, and the answer is `200` with the session. A session that has
ended already, `terminated` included, is answered `200` as it is, and
nothing changes; of two completes at once exactly one takes effect.

| Status | When, in this order |
|---|---|
| `400` `invalid_request` | the path id is not a UUID |
| `404` `not_found` | the caller may not read the session, or no such session (`session not found`) |
| `403` `forbidden` | the caller may read it and may not change it |
| `400` `invalid_request` | `status` missing or not one of the three (`terminated` and `active` included); `duration_ms` or `cache_creation_input_tokens` negative or not an integer; `error_message` not a string |

## `POST /api/v1/sessions/{id}/usage`

Reports one token usage increment of an `active` session. The body:
`model` (required, a non-blank string of at most 255 characters), `id` (a
UUID; a new one when absent) and `input_tokens`, `output_tokens`,
`cache_read_tokens` (integers from 0 to 1099511627776, 2^40, `0` when
absent). The answer
is `201` with the usage record:

```json
{"id":"…","session_id":"…","workspace_slug":"demo","model":"m1","input_tokens":10,"output_tokens":5,"cache_read_tokens":0,"reported_at":"2026-11-19T10:00:30Z"}
```

`workspace_slug` is the session's, and `reported_at` the hub's time.
Sending the `id` of a record of the same session answers `200` with the
stored record and stores nothing, also when the body differs and when the
session has ended since.

| Status | When, in this order |
|---|---|
| `400`, `404`, `403` | as for a complete |
| `400` `invalid_request` | `model` missing, blank, not a string or too long; `id` not a UUID; a count negative, above 2^40 or not an integer; the message naming the member |
| `409` `conflict` | the `id` is a record of another session |
| `409` `session_not_active` | the session has ended |

## `GET /api/v1/sessions/{id}`

Answers `200` with the session object to a caller who may read it (see
[Sessions](#sessions)).

## `GET /api/v1/sessions/{id}/usage`

Lists the session's usage records, readable as the session is, in the
[list envelope](#lists), ordered by `reported_at` and then `id`, newest
first, or oldest first with `order=asc` (`order` other than `asc` and
`desc` is `400` `invalid_request`), with `limit` and `cursor`. The answer
adds `session_id` and `totals`, which has the shape of `token_summary` and
covers every record of the session, whatever the page:

```json
{"items":[{"id":"…","session_id":"…","workspace_slug":"demo","model":"m1","input_tokens":10,"output_tokens":5,"cache_read_tokens":0,"reported_at":"2026-11-19T10:00:30Z"}],"next_cursor":null,"session_id":"…","totals":{"total_input_tokens":10,"total_output_tokens":5,"total_cache_read_tokens":0,"models_used":["m1"]}}
```

## `GET /api/v1/sessions`

Lists, in the [list envelope](#lists), the session objects the caller may
read: every session for the admin token; otherwise the sessions the caller
created, those of every workspace the caller may read, and those on the
task forks, removed ones included, of a workspace the caller may read. A
PAT bound to a workspace lists only sessions whose workspace its binding
covers. The order is `started_at` and then `id`, newest first.

| Parameter | Effect |
|---|---|
| `workspace` | only the sessions of this workspace |
| `include_forks` | `true` adds the sessions on the task forks of `workspace`, forks that no longer exist included; `false` (the default) does not; it needs `workspace` |
| `run_id`, `status`, `credential_type` | only the sessions with this value |
| `since`, `until` | only the sessions with `since` ≤ `started_at` < `until`; RFC 3339 with any offset |
| `order` | `desc` (the default) or `asc` |
| `limit`, `cursor` | [paging](#lists) |

Filters combine. A `status`, `credential_type`, `order` or `include_forks`
outside its values, `include_forks` without `workspace`, a `run_id` that is
not of the run id form, a `since` or `until` that is not RFC 3339, and a
filter parameter given twice are `400` `invalid_request` naming the
parameter. A
`workspace` the caller may not read, one that does not exist, or one a
bound PAT's binding does not cover is `404` `not_found` (`workspace not
found`).

## `GET /api/v1/workspaces/{slug}/cost`

Sums the token usage reported for the workspace over a window, in token
counts: no price is applied. Any reader of the workspace may call it,
whatever its status; a PAT needs `sessions:read` or `sessions:write`.

| Parameter | Effect |
|---|---|
| `group_by` | `day` (the default; the key is the UTC date `YYYY-MM-DD`), `session` (the session id) or `model` |
| `since` | the start of the window, included; 24 hours before now when absent |
| `until` | the end of the window, excluded; now, rounded up to a whole second, when absent |
| `limit`, `cursor` | paging of the groups |

`since` and `until` take any offset and are moved to UTC and cut to whole
seconds. Only the workspace's own records count: those of sessions on its
task forks belong to the forks.

```json
{"workspace":"demo","period":{"since":"2026-09-01T00:00:00Z","until":"2026-09-02T00:00:00Z"},"group_by":"day","totals":{"input_tokens":10,"output_tokens":0,"cache_read_tokens":0,"sessions":1},"items":[{"key":"2026-09-01","input_tokens":10,"output_tokens":0,"cache_read_tokens":0,"sessions":1}],"next_cursor":null}
```

`period` holds the window used; `totals` covers the whole window on every
page; `sessions` is the number of distinct sessions; `items` are ordered by
`key`, `[]` for a window without records. A `group_by` outside the three,
a `since` or `until` that is not RFC 3339, a `since` that is not before
`until`, a `limit` outside 1 to 200 and a cursor of another query are `400`
`invalid_request`; no access is `404` `not_found` (`workspace not found`).

## Agent telemetry

Agents push records about their runs to the hub, so that a run can be
audited later: run events, session outcomes, tool calls, tool errors, trace
records and one postmortem per run. They are stored in the analytical store
(`analytics.db`) and never in the event log: ingestion writes no hub event.
They are read back with the [run reads](#reading-a-run) and, for run
events, with [`GET /api/v1/audit`](#get-apiv1audit).

Every ingestion route is a `POST` under
`/api/v1/workspaces/{slug}/runs/{run_id}`:

| Route | Stores |
|---|---|
| `POST …/events` | one run event |
| `POST …/events/batch` | 1 to 1000 run events |
| `POST …/sessions/outcomes` | one session outcome |
| `POST …/tools/calls` | one tool call |
| `POST …/tools/errors` | one tool error |
| `POST …/traces` | one trace record |
| `POST …/traces/batch` | 1 to 1000 trace records |
| `POST …/postmortem` | the postmortem of the run |

**Order of checks.** Every route declares the scope `audit:write` for a PAT
and needs write access to the workspace, and decides in this order, the
first failure answering:

| Status | When |
|---|---|
| `403` `insufficient_scope` | a PAT without `audit:write`, whether or not the workspace exists |
| `404` `not_found` | no such workspace, or one the caller has no access to (`workspace not found`, the same bytes in both cases, for every caller, the admin token included) |
| `403` `forbidden` | a member of the workspace's org who is not its owner, also on an archived workspace |
| `400` `invalid_request` | the path's `run_id` is not of the run id form (`run_id must have the form YYYYMMDD_HHMMSS_ and six lower-case hexadecimal digits`); the body breaks a rule of its members, the message naming the member; a `run_id` member of the body differs from the path's |
| `409` `workspace_archived` | the workspace is archived (`the workspace is archived and accepts no telemetry`); a workspace in any other status, a task fork included, accepts ingestion |
| `503` `unavailable` | the write did not finish within `[audit].write_timeout` (default `2s`, see [configuration.md](configuration.md#keys)): it is rolled back, nothing of the request is stored, and the answer carries `Retry-After: 5`; a retry with the same ids stores the records as new |
| `500` `internal` | the store failed; nothing of the request is stored |

A refused request stores nothing. The six single-record routes are creating
`POST`s and take an `Idempotency-Key` (see [Idempotency](#idempotency)); the
two batch routes do not.

**Members and their rules.** Every record has `id`, `workspace` (the slug
of the path), `run_id` (the path's), `timestamp` and `ingested_at`, and
every member of its kind is present in every answer, an optional member
that was not sent being `null`.

- `id`: a UUID in the 8-4-4-4-12 hexadecimal form in any letter case,
  stored and returned in lower case; a version 4 UUID when absent.
- `timestamp`: RFC 3339 with any offset, returned in UTC with `Z`, its
  fraction cut to microseconds and without trailing zeros
  (`2026-01-02T05:04:05.1234567+02:00` is returned as
  `2026-01-02T03:04:05.123456Z`); the hub's time when absent. Its year in
  UTC must be `0000` to `9999` (`9999-12-31T23:59:59-01:00` is refused),
  and a fraction of a second takes a full stop, not a comma
  (`2026-01-02T03:04:05,5Z` is refused).
- `ingested_at`: the hub's time, in whole seconds.
- `session_id`: a UUID, returned in lower case; it is not checked against
  the stored sessions.
- `node_id` and `tool_name`: strings of at most 255 characters.
- `duration_ms` and `sequence`: whole numbers of at least `0`; a fraction,
  a string or a minus sign (also `-0`) is refused.
- `payload`, `token_usage`, `input`, `output` and `data`: stored and
  returned as JSON, never as a string holding JSON; `payload` must be an
  object, the others may be any JSON value.
- A required string may not be empty. A JSON `null` member counts as
  absent; members the kind does not define are ignored.

| Kind | Route | Members beyond the common ones |
|---|---|---|
| run event | `…/events` | `event_type` (required, free-form), `severity` (`info`, `warning`, `error`, `critical`), `node_id`, `session_id`, `payload` |
| session outcome | `…/sessions/outcomes` | `session_id` (required), `status` (required, free-form), `node_id`, `duration_ms`, `token_usage` |
| tool call | `…/tools/calls` | `tool_name` (required), `node_id`, `session_id`, `duration_ms`, `input`, `output` |
| tool error | `…/tools/errors` | `tool_name` (required), `error_msg` (required), `error_code` (a string), `node_id`, `session_id` |
| trace record | `…/traces` | `event_type` (required: `session.init`, `assistant.message`, `tool.use`, `tool.error` or `session.result`), `node_id`, `session_id`, `sequence`, `data` |

A run event sent without `severity` gets `error` for the type
`session.fail`, `warning` for `run.limit_reached`, `git.conflict`,
`harvest.empty` and `review.parse_failure`, and `info` for every other
type.

```json
{"id":"0a1b2c3d-0000-4000-8000-000000000001","workspace":"demo","run_id":"20260102_030405_a1b2c3","timestamp":"2026-01-02T03:00:00Z","ingested_at":"2026-01-02T03:04:05Z","event_type":"run.start","severity":"info","node_id":null,"session_id":null,"payload":{"a":{"b":1}}}
```

**Once per id.** A new record answers `201` with the stored record. A
request whose `id` names a stored record of the same kind in the same
workspace and run answers `200` with the **stored** record, also when the
body differs, and changes nothing; the body is still validated first, so
an invalid replay is `400`. An `id` that names a record of the same kind in
another workspace or run answers `409` `conflict` (`id <id> names a
<kind> of another workspace or run`). Ids are compared without regard to
letter case, and each kind has its own ids: one UUID may name a run event
and a tool call. Of several requests with one new `id` at once, exactly
one stores the record and answers `201`; the others answer `200` with it.

## `POST /api/v1/workspaces/{slug}/runs/{run_id}/events/batch` and `…/traces/batch`

Take a JSON array of 1 to 1000 run events or trace records, each judged
like the single record of its kind. A body that is not a JSON array, or an
empty array, is `400` `invalid_request`; more than 1000 items are `413`
`payload_too_large` (`a batch holds at most 1000 items, not <n>`), whether
or not they are valid. Otherwise the answer is always `200`:

```json
{"accepted":2,"duplicates":1,"errors":[{"index":3,"id":null,"message":"event_type is required and must be a non-empty string"}]}
```

- An invalid item, an item that is not a JSON object included, is skipped
  and reported in `errors` with its zero-based `index`, its `id` (lower
  case when the item carries a valid UUID, else `null`) and a `message`
  naming the member.
- An item whose `id` names a record of another workspace or run is
  reported in `errors` with its `id`.
- An item whose `id` is stored in this workspace and run, and an item that
  repeats the `id` of an earlier item of the batch, count in `duplicates`
  and change nothing.
- Every other item is stored and counted in `accepted`. The accepted items
  are stored in one transaction, all or none: a store failure is `500`
  `internal` and a timeout `503`, with nothing stored.
- `errors` is in index order, `[]` when there are none.

## `POST /api/v1/workspaces/{slug}/runs/{run_id}/postmortem`

Stores the one postmortem of a run in a workspace. The body:

| Member | Value |
|---|---|
| `run_status` | required: `stalled`, `block_limit`, `cost_limit` or `session_limit` |
| `started_at`, `completed_at` | required, RFC 3339 under the rules of `timestamp`, returned like it; their order is not checked |
| `task_summary` | required, a JSON object whose key `total` is present and not `null` |
| `cost_summary` | required, a JSON object |
| `blocked_tasks`, `session_history` | JSON arrays, `[]` when absent |
| `schema_version` | the number `1`; `1` when absent or `null` |

The hub generates the `id` and sets `timestamp` and `ingested_at` to its
own time; an `id` or `timestamp` member of the body is ignored. The first
postmortem of the run in the workspace answers `201`:

```json
{"id":"…","workspace":"demo","run_id":"20260102_030405_a1b2c3","timestamp":"2026-01-02T03:04:05Z","ingested_at":"2026-01-02T03:04:05Z","schema_version":1,"run_status":"stalled","started_at":"2026-01-02T02:00:00Z","completed_at":"2026-01-02T02:30:00Z","task_summary":{"total":4,"done":3},"cost_summary":{},"blocked_tasks":[],"session_history":[]}
```

A later valid postmortem of the run in the workspace, also several at
once, answers `200` with the stored one and changes nothing; the same run
id in another workspace has its own. A `schema_version` other than the
number `1` is `400` `unknown_schema_version` (`schema_version must be the
number 1, the one version of a postmortem the hub reads`); every other
violation is `400` `invalid_request` naming the member, also when the run
has a postmortem already.

## Sandbox use as run events

The hub records what is done in a sandbox that belongs to a run. When an
exec, an attach, an upload or a download that passed every check ends in a
sandbox whose session is stored and has a `run_id`, the hub stores one run
event of the sandbox's workspace under that `run_id` and `session_id`, with
a generated `id`, `severity` `info` and the time the operation started as
its `timestamp`:

| Operation | `event_type` | `payload` members |
|---|---|---|
| exec | `sandbox.exec` | `sandbox_id`, `actor_type`, `actor_id`, `command` (the argument list), `exit_code`, `timed_out`, `duration_ms` |
| attach | `sandbox.attach` | `sandbox_id`, `actor_type`, `actor_id`, `duration_ms` |
| upload | `sandbox.upload` | `sandbox_id`, `actor_type`, `actor_id`, `path`, `bytes` |
| download | `sandbox.download` | `sandbox_id`, `actor_type`, `actor_id`, `path`, `bytes` |

`actor_id` is `null` for the admin token and `exit_code` `null` for an
exec that gave none. The payload never holds standard input, output,
terminal content, file content or environment values. A sandbox without a
session, or whose session has no `run_id` or is not stored, leaves no
record. The record checks neither access nor the workspace's status, is
held to `[audit].write_timeout`, and writes no hub event; a failure to
write it is logged and changes nothing about the operation.

## Reading a run

Seven `GET` routes under `/api/v1/workspaces/{slug}/runs/{run_id}` read
what agents pushed about one run:

| Route | Answers |
|---|---|
| `GET …/events` | the run's run events, a list |
| `GET …/sessions/outcomes` | its session outcomes, a list |
| `GET …/tools/calls` | its tool calls, a list |
| `GET …/tools/errors` | its tool errors, a list |
| `GET …/traces` | its trace records, a list |
| `GET …/postmortem` | its postmortem |
| `GET …/transcript?node_id=…` | the conversation of one node, a list |

**Order of checks.** Every route declares the scope `audit:read` for a PAT,
which `audit:write` satisfies, and needs read access to the workspace,
whatever its status: an archived workspace is read like any other.

| Status | When |
|---|---|
| `401` `unauthenticated` | no valid credential |
| `403` `insufficient_scope` | a PAT with neither `audit:read` nor `audit:write` (`this route needs the scope audit:read`), whether or not the workspace exists |
| `404` `not_found` | no such workspace, or one the caller cannot read (`workspace not found`, the same bytes in both cases) |
| `400` `invalid_request` | a parameter below breaks its rule, before any record is read; the message names the parameter |

**The lists.** Each list holds the records of its kind stored for the
path's workspace and run, never one of another run or of the same run id in
another workspace, each with every member of its record as
[ingestion](#agent-telemetry) returns it. They are ordered by `timestamp`,
then `id`, both ascending, or both descending with `order=desc`, and paged
as in [Lists](#lists). A run without records answers
`{"items":[],"next_cursor":null}`. The filters match a member's value
exactly and combine with AND; a parameter a route does not take is ignored:

| Route | Filters |
|---|---|
| `…/events` | `event_type`, `severity`, `node_id`, `session_id` |
| `…/sessions/outcomes` | `node_id`, `session_id`, `status` |
| `…/tools/calls` | `node_id`, `session_id`, `tool_name` |
| `…/tools/errors` | `node_id`, `session_id`, `tool_name` |
| `…/traces` | `event_type`, `node_id`, `session_id` |

All five also take `since` (at or after) and `until` (before) on
`timestamp`, `order`, `limit` and `cursor`. These are refused `400`
`invalid_request`:

- a path `run_id` that is not `YYYYMMDD_HHMMSS_` and six lower-case
  hexadecimal digits (`run_id must have the form YYYYMMDD_HHMMSS_ and six
  lower-case hexadecimal digits`);
- `severity` outside `info`, `warning`, `error`, `critical`, `order` outside
  `asc`, `desc`, and on `…/traces` an `event_type` outside the five trace
  types (`<name> must be one of …`);
- a `session_id` that is not a UUID (`session_id must be a UUID in the
  8-4-4-4-12 hexadecimal form`); it matches in any letter case;
- a `since` or `until` that is not RFC 3339 (`<name> must be an RFC 3339
  timestamp whose year in UTC is 0000 to 9999`), and a `since` not before
  `until` (`since must be before until`); any offset is accepted;
- a `limit` or `cursor` as in [Lists](#lists): a cursor belongs to its
  route, the workspace, the run and every other parameter, `order`
  included.

**`GET …/postmortem`** answers `200` with the run's postmortem in this
workspace, every member as [ingestion](#post-apiv1workspacesslugrunsrun_idpostmortem)
returns it. A run with no postmortem in this workspace, also one whose
postmortem is in another workspace, is `404` with `error_type`
`postmortem_not_found` (`run <run_id> has no postmortem in workspace
<slug>`).

**`GET …/transcript?node_id=…`** rebuilds the conversation of one node
from the run's trace records of that node, ordered by `timestamp`, then
`sequence` (an absent one counting as `0`), then `id`. A `session.result`
is left out; every other trace becomes one item:

| Member | Value |
|---|---|
| `role` | `system` for `session.init`, `assistant` for `assistant.message`, `tool_use` for `tool.use`, `tool_error` for `tool.error` |
| `content` | the trace's `data.content` when it is a JSON string, else `""` |
| `tool_name` | the trace's `data.tool_name` when it is a JSON string, else `null` |
| `timestamp` | the trace's `timestamp` |

The answer is a list with two members after `next_cursor`, the path's
`run_id` and the `node_id`, paged with `limit` and `cursor`; every page but
the last holds `limit` items. A run or node with no traces answers an empty
list. A missing or empty `node_id` is `400` (`node_id is required and must
not be empty`), as are a malformed `run_id`, `limit` or `cursor`.

```json
{"items":[{"role":"system","content":"","tool_name":null,"timestamp":"2026-06-01T12:00:00Z"},{"role":"assistant","content":"hello","tool_name":null,"timestamp":"2026-06-01T12:00:01Z"},{"role":"tool_use","content":"","tool_name":"bash","timestamp":"2026-06-01T12:00:02Z"}],"next_cursor":null,"run_id":"20260601_120000_c0ffee","node_id":"coder"}
```

## `GET /api/v1/audit`

One list of the hub events and the agents' run events the caller may see,
newest first. It names no workspace, so a workspace-bound PAT may call it.
The route declares the scope `audit:read` for a PAT, which `audit:write`
satisfies: without a valid credential it is `401` `unauthenticated`, a PAT
with neither scope `403` `insufficient_scope`.

**What a caller sees.** The admin token sees every stored record, those of
workspaces that no longer exist included. Any other caller sees the hub
events the [visibility rule](#get-apiv1events) of the event stream lets
them see, and the run events of the workspaces they can read, plus those
of a deleted task fork (`<parent>--<name>`) whose parent they can read. A
workspace-bound PAT sees only the records of the workspaces its binding
covers, no deleted fork's and no hub event without a workspace. A
`parley.webhook.ping` is never in the list. The list holds what is stored:
records leave it when [retention](configuration.md#retention) deletes them.

**Order.** By time, then `id`, both descending, then a hub event before a
run event; a hub event's time is its `time`, a run event's its
`timestamp`. Paged as in [Lists](#lists); every item that exists
throughout the walk comes once, across both sources.

**Items.** Every item has the same seventeen members; those of the other
source are `null`:

| Member | Hub event | Run event |
|---|---|---|
| `source` | `hub` | `agent` |
| `id`, `workspace`, `session_id` | the event's | the run event's |
| `type` | the event's `type` | the run event's `event_type` |
| `time` | the event's `time`, whole seconds | the run event's `timestamp`, with its fraction of a second |
| `seq`, `actor_type`, `actor_id`, `resource_type`, `resource_id`, `action`, `metadata` | the event's | `null` |
| `run_id`, `node_id`, `severity`, `payload` | `null` | the run event's |

`metadata` and `payload` are JSON objects or `null`, never strings.

```json
{"items":[{"source":"agent","id":"0a1b2c3d-0000-4000-8000-000000000001","type":"tool.started","time":"2026-06-01T11:59:58.123456Z","workspace":"demo","session_id":null,"seq":null,"actor_type":null,"actor_id":null,"resource_type":null,"resource_id":null,"action":null,"metadata":null,"run_id":"20260601_120000_c0ffee","node_id":null,"severity":"info","payload":{"k":"v"}},{"source":"hub","id":"…","type":"parley.workspace.create","time":"2026-06-01T11:59:00Z","workspace":"demo","session_id":null,"seq":7,"actor_type":"api_key","actor_id":"…","resource_type":"workspace","resource_id":"demo","action":"create","metadata":{},"run_id":null,"node_id":null,"severity":null,"payload":null}],"next_cursor":null}
```

**Filters.** They combine with AND; a parameter the route does not know is
ignored.

| Parameter | Applies to | Meaning |
|---|---|---|
| `source` | both | `hub` or `agent`; both when absent |
| `workspace` | both | the exact slug |
| `types` | both | a comma-separated list of at most 20 entries, which combine with OR: an exact type, or a prefix ending in `*`; `*` alone matches every type. A run event's type is its `event_type` |
| `session_id` | both | a UUID, in any letter case |
| `since`, `until` | both | RFC 3339: at or after `since`, before `until` |
| `actor_id`, `actor_type`, `resource_type`, `resource_id`, `action` | hub events | exact match; giving one leaves the run events out |
| `run_id`, `node_id`, `severity` | run events | exact match; giving one leaves the hub events out |
| `limit`, `cursor` | both | as in [Lists](#lists) |

A type no record has gives an empty list, not an error, and so does a hub
event filter given with a run event filter.

| Status | When |
|---|---|
| `400` `invalid_request` | before any record is read, naming the parameter: `source` outside `hub`, `agent`; `actor_type` outside `admin_token`, `api_key`, `pat`, `forge`, `system`; `severity` outside `info`, `warning`, `error`, `critical` (`<name> must be one of …`); a `session_id` that is not a UUID; a `run_id` that is not a run id; a `since` or `until` that is not RFC 3339, or a `since` not before `until`; a `types` list with more than 20 entries (`types has more than 20 entries`), an empty entry (`types has an empty entry`) or a `*` before its end (`types entry "<entry>" has a * that is not its last character`); a bad `limit` or `cursor`; a hub event filter with `source=agent` (`<name> filters hub events and cannot be given with source=agent`) or a run event filter with `source=hub` (`<name> filters run events and cannot be given with source=hub`) |
| `404` `not_found` | `workspace` names a workspace the caller cannot read, one outside a bound token's binding, or one that does not exist (`workspace not found`, the same bytes in every case). The admin token is never refused, and any caller may name a task fork's slug whose parent they can read, so a deleted fork's records stay reachable |

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
events that left. Events leave the log once they are older than
`[events].retention` (default `30d`), when the hourly retention run's
`event_log` step runs; the newest event never leaves it, and the events
that left stay stored for [`GET /api/v1/audit`](#get-apiv1audit) (see
[configuration.md](configuration.md#retention)):

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

## Event webhooks

An event webhook makes the hub push the [events](#get-apiv1events) of one
workspace, and of its task forks, to a URL as signed HTTP `POST`s, in `seq`
order, one at a time, at least once, and keep trying until the receiver
accepts each one or a fixed time has passed. Nine routes manage them, all
under `/api/v1/workspaces/{slug}/webhooks`:

| Route | Scope of a PAT | Answers |
|---|---|---|
| [`POST …/webhooks`](#post-apiv1workspacesslugwebhooks) | `workspaces:write` | `201` the subscription with its `secret` |
| [`GET …/webhooks`](#get-apiv1workspacesslugwebhooks) | `workspaces:read` | `200` a list of subscriptions |
| [`GET …/webhooks/{id}`](#get-apiv1workspacesslugwebhooksid) | `workspaces:read` | `200` the subscription with `pending` |
| [`PATCH …/webhooks/{id}`](#patch-apiv1workspacesslugwebhooksid) | `workspaces:write` | `200` the subscription |
| [`DELETE …/webhooks/{id}`](#delete-apiv1workspacesslugwebhooksid) | `workspaces:write` | `204` |
| [`POST …/webhooks/{id}/rotate-secret`](#post-apiv1workspacesslugwebhooksidrotate-secret) | `workspaces:write` | `200` the subscription with its new `secret` |
| [`POST …/webhooks/{id}/ping`](#post-apiv1workspacesslugwebhooksidping) | `workspaces:write` | `200` a delivery record |
| [`GET …/webhooks/{id}/deliveries`](#get-apiv1workspacesslugwebhooksiddeliveries) | `workspaces:read` | `200` a list of delivery records |
| [`POST …/webhooks/{id}/deliveries/{delivery_id}/redeliver`](#post-apiv1workspacesslugwebhooksiddeliveriesdelivery_idredeliver) | `workspaces:write` | `200` the delivery record |

`workspaces:write` satisfies `workspaces:read`. Every route needs `owner`
(see [Workspace access](#workspace-access)): the workspace's owner and the
admin token reach them; a member of the workspace's org is answered `403`
`forbidden` (`org members have read-only access to this workspace`), anyone
else the `404` `not_found` of a slug with no workspace. A workspace-bound PAT
is refused `403` `token_bound` on every route, whoever its user is (see
[Workspace-bound tokens](#workspace-bound-tokens)). The checks run in this
order, after those of the [order of checks](#order-of-checks):

1. A task fork has no event webhooks of its own: every route called with a
   fork's slug is answered `400` `not_supported_on_fork` (`a task fork has
   no event webhooks; subscribe on its parent <parent> with include_forks`),
   whatever its body. A subscription of the parent with `include_forks`
   covers the fork.
2. An `{id}` or `{delivery_id}` that is not a UUID is `400`
   `invalid_request` (`id must be a UUID`, `delivery_id must be a UUID`).
3. An `{id}` that names no subscription of the workspace, also one of
   another workspace, is `404` `webhook_not_found` with the same bytes:

   ```json
   {"error":{"code":404,"message":"webhook not found","error_type":"webhook_not_found"}}
   ```

4. The body, then the state of the workspace or the record.

### The subscription

| Member | Value |
|---|---|
| `id` | UUID |
| `workspace` | the slug, never a task fork |
| `url` | the URL the hub posts to |
| `types` | the event types sent, each an exact type or a prefix ending in `*`; `["*"]` for every type |
| `include_forks` | whether the events of the workspace's task forks are sent too |
| `description` | free text, `""` by default |
| `state` | `active`, `failing` (an event was given up) or `disabled` (no delivery) |
| `created_by` | the creator's user id; `null` for the admin token |
| `created_at`, `updated_at` | RFC 3339 in UTC with whole seconds |
| `last_delivery_at` | the time of the last attempt of the delivery loop, `null` before the first |
| `last_status` | `succeeded` or `failed`, the outcome of that attempt; `null` before the first |
| `failing_since` | the time of the first failed attempt since the last success; `null` while attempts succeed |

```json
{"id":"0b6a4bd1-0a3c-4d55-9b1e-2a5e1f0f6c11","workspace":"demo","url":"https://ci.example/hooks/parley","types":["*"],"include_forks":true,"description":"","state":"active","created_by":"u_1","created_at":"2026-11-19T10:00:00Z","updated_at":"2026-11-19T10:00:00Z","last_delivery_at":null,"last_status":null,"failing_since":null}
```

The signing secret is not a member. It is `whsec_` followed by 64
lower-case hexadecimal characters, made from 32 random bytes, and appears in
the answers of the create and of [rotate-secret](#post-apiv1workspacesslugwebhooksidrotate-secret)
alone: in no other answer, no event and no log line (the hub's scrubber
replaces it). It is stored sealed with the key that seals
[secrets](#secrets), bound to the subscription, and is opened for each
attempt.

### What a subscription receives

A subscription receives the events an owner of its workspace may see that
match `types`: every event of the workspace, the owner-only types
`parley.secret.*`, `parley.token.*` and `parley.webhook.*` included, and with
`include_forks` every event of a task fork of the workspace, also after the
fork is gone; never an event of no workspace and never one of another
workspace. Its own `parley.webhook.*` events are sent to it like any other.
It starts with the first event written after its `parley.webhook.create`
and never sends older events.

The hub keeps, per subscription, a position in the event log. A delivery
loop runs for every `active` and `failing` subscription, woken by each new
event and by its own timer. It reads the next events after the position;
an event the subscription does not receive moves the position on without a
delivery. For the next event it does receive, it records a `pending`
delivery and makes the first attempt at once. The next event is sent only
when the current one has succeeded or been given up, so a receiver never
gets event B before event A when A has the lower `seq` (except through a
[redelivery](#post-apiv1workspacesslugwebhooksiddeliveriesdelivery_idredeliver)),
may get an event more than once, and has at most one request of the
subscription at a time. A slow or dead receiver delays only its own
subscription. An event that left the event log before its turn came is
skipped and counted `failed` in `parley_webhook_deliveries_total`, without
making the subscription `failing`.

**Retries.** After a failed attempt the hub waits 10 s, then twice as long
after each further failure, up to 1 hour, but never past 24 hours after the
event's first attempt, so the waits are 10, 20, 40, 80, 160, 320, 640, 1280,
2560, 3600, 3600, … seconds. A wait is counted from the moment the failed
attempt ended, so the receiver never gets a retry sooner than the wait
after the request it failed; `next_attempt_at`, written in whole seconds
like every timestamp, can read up to a second earlier than the retry.
Every attempt reads the subscription's current
`url`, `types` and secret. A failed attempt sets `last_status` `failed` and
`last_delivery_at`, and `failing_since` when it is `null`. An attempt that
fails 24 hours or more after the event's first attempt gives the event up:
its delivery becomes `failed`, the position moves past it, an `active`
subscription becomes `failing` and writes `parley.webhook.failing` (actor
`system`), and the loop goes on with the next event, which gets its own 24
hours. A successful attempt sets `last_status` `succeeded` and
`last_delivery_at`, clears `failing_since` and makes a `failing`
subscription `active` again, without an event.

**Disabling.** At least once a minute the hub disables every `active` or
`failing` subscription whose `failing_since` is 72 hours or more in the
past: its `pending` delivery becomes `failed` and it writes
`parley.webhook.disabled` (actor `system`). A `disabled` subscription gets no
delivery, and the events written while it is disabled are not kept for it.

**Restarts.** The position and the `pending` delivery are stored: at start
the hub resumes every `active` and `failing` subscription from its position,
an attempt that was in flight at the stop is made again under its delivery
id, and a scheduled retry keeps its time. At the stop the attempts in flight
are abandoned without waiting.

**The workspace's life.** While the workspace is archived its subscriptions
keep delivering; they cannot be created, changed or have their secret
rotated (`409` `workspace_archived`) and can be read, pinged, redelivered and
deleted. When the workspace is deleted, its subscriptions, their deliveries
and secrets go in the transaction of the delete, without an event of their
own.

### The delivery request

Each attempt is one `POST` to `url`, without following a redirect, and
without a credential of the hub's or any header that identifies a user. Its
body is the event exactly as [`GET /api/v1/events/log`](#get-apiv1eventslog)
returns it, one JSON object. Its headers:

| Header | Value |
|---|---|
| `Content-Type` | `application/json` |
| `Parley-Event` | the event's `type` |
| `Parley-Delivery` | the delivery id; the same on every retry and on a redelivery |
| `Parley-Seq` | the event's `seq`; `0` for a ping |
| `Parley-Timestamp` | the Unix time in seconds at which this attempt is sent |
| `Parley-Signature` | `sha256=` followed by the lower-case hexadecimal HMAC-SHA256 of `<Parley-Timestamp>.<body>`, keyed with the secret exactly as shown, `whsec_…` included |
| `User-Agent` | `parley-webhook/<hub version>` |

A receiver verifies a delivery by computing the HMAC over the timestamp it
received, a dot and the raw body, and comparing it with the header; each
attempt carries a new timestamp and so a new signature. The hub resolves the
URL's host at every attempt and checks the address it connects to (see
[Destinations](#destinations)); TLS certificates are always verified.

An attempt **succeeds** when a `2xx` status arrives within
`[webhooks].timeout` (`10s` by default; see
[configuration.md](configuration.md#keys)); the response body is not read
and never stored. Everything else **fails**, recorded on the delivery as:

| Outcome | `response_code` | `error` |
|---|---|---|
| a `3xx` status | the status | `redirect` |
| any other status | the status | `null` |
| no status within the timeout | `null` | `timeout` |
| the host does not resolve, or the connection cannot be opened | `null` | `connection_failed` |
| the TLS handshake fails, an untrusted certificate included | `null` | `tls_error` |
| the address is refused (see [Destinations](#destinations)) | `null` | `destination_refused` |

An informational status (`1xx`) other than `101 Switching Protocols` does not
end an attempt: the hub waits on for the final status, and an attempt that
gets none within the timeout is a `timeout`. A `101` ends it as any other
status does: `response_code` `101`, `error` `null`.

A signing secret that cannot be decrypted (no key is configured, or the
stored value opens under neither key) sends nothing unsigned: the loop makes
no attempt, leaves the delivery as it is and tries again within a minute,
and ping and redeliver are answered `503` `secrets_unavailable`.

### Destinations

The hub refuses a URL whose host is, or resolves to, an address in
`127.0.0.0/8`, `169.254.0.0/16` (with the metadata address
`169.254.169.254`), `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`,
`100.64.0.0/10` (shared address space), `0.0.0.0/8` (this network, with
`0.0.0.0`), `224.0.0.0/4`, `::1`, `fe80::/10`, `fc00::/7` (with
`fd00:ec2::254`), `::` or `ff00::/8`; an IPv6 address that carries an IPv4
address is judged by that IPv4 address: IPv4-mapped (`::ffff:a.b.c.d`),
IPv4-compatible (`::a.b.c.d`, other than `::` and `::1`), NAT64
(`64:ff9b::/96`, the last 32 bits) and 6to4 (`2002::/16`, bits 16 to 47),
so `64:ff9b::a00:1` is refused as `10.0.0.1`. `localhost` is judged as
`127.0.0.1`.
`[webhooks].allow_private = true` lifts the refusal for all of them;
`[webhooks].allow_insecure = true` accepts `http://` URLs whose host is a
loopback address and lifts the refusal for exactly those. At the create and
at a `PATCH` that gives `url`, after the shape of the URL is accepted, the
host is resolved and the URL is refused when any address it resolves to is
refused; a name that does not resolve is accepted, and its attempts fail
with `connection_failed`. The refusal is:

```json
{"error":{"code":400,"message":"url is refused: its host inside.example is, or resolves to, an address of the hub's own networks, which [webhooks].allow_private allows","error_type":"webhook_destination_refused"}}
```

At every attempt, ping and redelivery the addresses are checked again just
before the connection: one refused address fails the attempt with
`destination_refused` before any connection is opened, and the attempt is
retried like any failure.

### Events

| Type | Written when | Actor | `metadata` |
|---|---|---|---|
| `parley.webhook.create` | a subscription is created | the caller | `webhook_id`, `host`, `types`, `include_forks` |
| `parley.webhook.update` | a `PATCH`, or a rotation (`fields` `["secret"]`) | the caller | `webhook_id`, `fields` (the names given, in the order `url`, `types`, `include_forks`, `description`, `active`), `state` (after the change) |
| `parley.webhook.delete` | a subscription is deleted | the caller | `webhook_id`, `host` |
| `parley.webhook.failing` | a subscription becomes `failing` | `system` | `webhook_id`, `host`, `seq` (the event given up), `failing_since`, `response_code`, `error` |
| `parley.webhook.disabled` | the 72-hour check disables a subscription | `system` | `webhook_id`, `host`, `failing_since` |

Each has `resource_type` `event_webhook`, `resource_id` the subscription id
and `workspace` its workspace, and is written in the transaction of its
change. Metadata holds the URL's host, never the URL, and never the secret.
The ping's type, `parley.webhook.ping`, is never written to the event log.

## `POST /api/v1/workspaces/{slug}/webhooks`

Creates a subscription and answers `201` with it and its `secret`. It is a
create route (see [Idempotency](#idempotency)): a replay answers `200` with
the first answer without `secret` and creates nothing. The body:

| Member | Required | Value |
|---|---|---|
| `url` | yes | an absolute `https://` URL with a host, without userinfo or fragment, at most 2,048 characters; `http://` only with `[webhooks].allow_insecure` and a loopback host |
| `types` | no | a list of 1 to 20 strings, each non-empty, an exact type or a prefix ending in `*` (`*` only last), without a comma; default `["*"]` |
| `include_forks` | no | a boolean; default `true` |
| `description` | no | a string of at most 1,024 characters; default `""` |

Other members are ignored; a member given as `null` is a bad value. The new
subscription is `active`, and its position is the `seq` of its own
`parley.webhook.create`, so the first event it can receive is the next one
written.

```json
{"id":"0b6a4bd1-0a3c-4d55-9b1e-2a5e1f0f6c11","workspace":"demo","url":"https://ci.example/hooks/parley","types":["*"],"include_forks":true,"description":"","state":"active","created_by":"u_1","created_at":"2026-11-19T10:00:00Z","updated_at":"2026-11-19T10:00:00Z","last_delivery_at":null,"last_status":null,"failing_since":null,"secret":"whsec_9f2c…"}
```

| Status | When |
|---|---|
| `201 Created` | the subscription with `secret` |
| `200 OK` | a replay of the same `Idempotency-Key`, caller and body: the first answer without `secret` |
| `400` `invalid_request` | a field breaks its rule; the message names the field, such as `url must use https`, `url is longer than 2048 characters`, `types must not be empty`, `types has more than 20 entries`, `include_forks must be true or false`, `description is longer than 1024 characters` |
| `400` `webhook_destination_refused` | the host is, or resolves to, a refused address (see [Destinations](#destinations)) |
| `409` `workspace_archived` | the workspace is archived |
| `429` `webhook_limit_reached` | the workspace holds `[webhooks].max_per_workspace` subscriptions in whatever state (`workspace demo holds 20 event webhooks, the most [webhooks].max_per_workspace allows`) |
| `503` `secrets_unavailable` | no encryption key is configured (see [Secrets](#secrets)) |

A refused create stores nothing and writes no event.

## `GET /api/v1/workspaces/{slug}/webhooks`

Lists the workspace's subscriptions, a [list](#lists) in the standard pages,
ordered by `created_at`, then `id`, ascending. `state` (`active`, `failing`
or `disabled`) keeps only the subscriptions in that state; another value is
`400` `invalid_request` (`state must be one of active, failing, disabled`).
No item carries `secret`.

## `GET /api/v1/workspaces/{slug}/webhooks/{id}`

Answers the subscription with `pending`: the number of events after its
position that it receives, the one of its `pending` delivery included
(counted once whatever the filter now says); `0` for a `disabled`
subscription.

## `PATCH /api/v1/workspaces/{slug}/webhooks/{id}`

Changes the fields given and answers `200` with the subscription, its
`updated_at` set to now; fields not given keep their value. The body has at
least one of `url`, `types`, `include_forks`, `description` (each held to the
rules of the create) and `active` (a boolean). A changed `url`, `types` or
`include_forks` applies from the next attempt, and to every event not yet
examined; the `pending` delivery is kept and still sent, under its id. It
writes `parley.webhook.update`.

- `active: true` sets a `disabled` or `failing` subscription `active` and
  clears `failing_since`. A `disabled` one resumes with the first event
  written after this request's `parley.webhook.update`; events written while
  it was disabled are not sent. A `failing` one keeps its position and its
  `pending` delivery.
- `active: false` sets the subscription `disabled` and marks its `pending`
  delivery `failed` with `next_attempt_at` `null`; `failing_since` is kept
  until a re-enable clears it.
- Asking for the state the subscription is already in changes nothing else
  and still answers `200` with the event.

| Status | When |
|---|---|
| `200 OK` | the subscription |
| `400` `invalid_request` | no field (`the body must give at least one of url, types, include_forks, description and active`), an unknown member (`color is not a field of an event webhook; a change gives url, types, include_forks, description or active`), or a bad value |
| `400` `webhook_destination_refused` | a given `url` names a refused destination |
| `409` `workspace_archived` | the workspace is archived |

A refused change keeps every field and writes no event.

## `DELETE /api/v1/workspaces/{slug}/webhooks/{id}`

Deletes the subscription with its `pending` delivery, its delivery records
and its signing secret, in any workspace status, writes
`parley.webhook.delete` and answers `204` without a body. Its deliveries
stop.

## `POST /api/v1/workspaces/{slug}/webhooks/{id}/rotate-secret`

Takes no body. Replaces the signing secret with a new one and answers `200`
with the subscription and the new `secret`; the old secret is no longer used
from the next attempt, also for a delivery being retried. It writes
`parley.webhook.update` with `fields` `["secret"]`. It is answered `409`
`workspace_archived` for an archived workspace and `503`
`secrets_unavailable` when no encryption key is configured, changing
nothing.

## `POST /api/v1/workspaces/{slug}/webhooks/{id}/ping`

Takes no body. Sends one test delivery at once, outside the order of the
subscription, in any subscription state and workspace status, and answers
`200` with its delivery record once the attempt has ended, whether it
`succeeded` or `failed`. Its body has the shape of an event:

```json
{"seq":0,"id":"…","type":"parley.webhook.ping","time":"2026-11-19T10:05:00Z","workspace":"demo","actor_type":"api_key","actor_id":"u_1","resource_type":"event_webhook","resource_id":"0b6a4bd1-0a3c-4d55-9b1e-2a5e1f0f6c11","action":"ping","session_id":null,"metadata":{}}
```

It is signed like any delivery, written to no event log, never retried, and
changes nothing on the subscription: not its `state`, `last_status`,
`last_delivery_at`, `failing_since` or position. `503` `secrets_unavailable`
answers a secret that cannot be decrypted, recording nothing. A caller that
disconnects does not end the attempt, which is bounded by
`[webhooks].timeout`, and its record holds the receiver's answer. A
subscription deleted while the attempt is in flight is answered `404`
`webhook_not_found` after it, recording nothing.

### The delivery record

| Member | Value |
|---|---|
| `id` | UUID, sent as `Parley-Delivery` |
| `seq` | the event's `seq`; `0` for a ping |
| `type` | the event's type, `parley.webhook.ping` for a ping |
| `status` | `pending`, `succeeded` or `failed` |
| `attempts` | the attempts made |
| `response_code` | the status of the last attempt's response; `null` when none arrived |
| `error` | `timeout`, `connection_failed`, `tls_error`, `destination_refused`, `redirect` or `null` |
| `duration_ms` | the last attempt's duration; `null` before the first |
| `created_at` | when the delivery was recorded |
| `last_attempt_at` | the last attempt's time; `null` before the first |
| `next_attempt_at` | when the next attempt is due; `null` unless `pending` |

It holds no payload and no response body.

## `GET /api/v1/workspaces/{slug}/webhooks/{id}/deliveries`

Lists the subscription's delivery records, pings included, a
[list](#lists) in the standard pages, newest first: `created_at`, then `id`,
descending. `status` (`pending`, `succeeded` or `failed`) keeps only the
records with that status; another value is `400` `invalid_request`
(`status must be one of pending, succeeded, failed`). Records are kept for 7
days after their last attempt or, without an attempt, its creation, by the
retention step `webhook_deliveries`
(see [configuration.md](configuration.md#retention)); a `pending` record is
never deleted.

## `POST /api/v1/workspaces/{slug}/webhooks/{id}/deliveries/{delivery_id}/redeliver`

Takes no body. Sends the event of a `succeeded` or `failed` delivery once
more, at once and outside the order, under the same delivery id, to the
subscription's current `url` with its current secret, in any subscription
state, without a retry; answers `200` with the record: `status` the outcome,
`attempts` one higher, `response_code`, `error`, `duration_ms` and
`last_attempt_at` of this attempt, `next_attempt_at` `null`. It changes
nothing on the subscription. A caller that disconnects does not end the
attempt, which is bounded by `[webhooks].timeout`, and the record holds the
receiver's answer.

The refusals are checked in the order of the table: the delivery, then
whether it is a ping, then the signing secret, then its status, then its
event.

| Status | When |
|---|---|
| `200 OK` | the record, whether the attempt succeeded or failed |
| `404` `delivery_not_found` | the id names no delivery of the subscription |
| `400` `invalid_request` | `delivery_id` is not a UUID (checked before the lookup); the delivery is a ping (`a ping cannot be redelivered; send a new ping`) |
| `503` `secrets_unavailable` | the signing secret cannot be decrypted |
| `409` `delivery_pending` | the delivery is `pending` (`the delivery is still pending; the hub keeps trying it`) |
| `410` `event_expired` | the event has left the event log (`the event of the delivery has left the event log`) |

A record that goes while the attempt is in flight is answered `404` after
it: `webhook_not_found` when the subscription was deleted,
`delivery_not_found` when the record was pruned.

A refused redelivery sends nothing and leaves the record as it was.
