# API reference

The hub, `parleyd`, answers the routes below over HTTP. The REST API is
mounted at `/api/v1`; no feature has registered a route there yet, so this
page describes the rules every REST route follows and the three built-in
routes. It grows as features land.

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
Paths are matched exactly, and a path that does not match is never
redirected:

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
   guard.
6. The scope check.
7. The access checks.
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
| `200 OK` | an object with exactly the string members below |

The answer carries `Cache-Control: public, max-age=300`.

| Member | Value |
|---|---|
| `version` | the version the binary was built with, or `dev` when none was set |
| `commit` | the commit the binary was built from, or `unknown` when none was set |
| `build_time` | the build time as given at build time, or `unknown` when none was set |
| `go_version` | the version of the Go toolchain that built the binary, for example `go1.27.3` |

Example:

```json
{"version":"1.4.0","commit":"3f2a1bc","build_time":"2026-01-02T03:04:05Z","go_version":"go1.27.3"}
```

How the build values are set is in [architecture.md](architecture.md#build-information).
