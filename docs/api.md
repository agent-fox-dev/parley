# API

`parleyd` serves the REST API under `/api/v1` and a few unauthenticated
endpoints beside it. Feature routes are listed with each feature; this page
gives the rules every route follows.

## Health, readiness and version

| Route | Answer | Cache-Control |
|---|---|---|
| `GET /healthz` | `200 {"status":"ok"}` | `no-cache` |
| `GET /readyz` | `200 {"status":"ready"}` while both stores answer a query, otherwise `503 {"status":"not ready"}` | `no-cache` |
| `GET /version` | `200 {"version":"…","commit":"…","build_time":"…","go_version":"…"}`; a build without values reports `dev`, `unknown`, `unknown` | `public, max-age=300` |

None of them needs a credential or is rate-limited.

## Authentication

Every route under `/api/v1` needs a credential, except `GET /auth/providers`
and `POST /auth/callback`. Present it as `Authorization: Bearer <credential>`
(the scheme in any letter case). The hub stores only the SHA-256 of each
secret.

| Type | Wire format | Identity |
|---|---|---|
| Admin token | `parley_admin_<64 lowercase hex>` | none (no user) |
| API key | `parley_<key_id>_<secret>`, 8 and 32 characters of `[0-9A-Za-z]` | a user |
| Personal access token | `parley_pat_<token_id>_<secret>`, 8 and 32 characters of `[a-z0-9]` | a user |

The first failing check decides the answer:

| Check | Answer |
|---|---|
| a Bearer credential of a known format | `401 unauthenticated`, `authentication required` |
| the id exists and the secret matches | `401 unauthenticated`, `invalid credentials` |
| not revoked | `401 unauthenticated`, `credential revoked` |
| not expired | `401 unauthenticated`, `credential expired` |
| the user is not blocked | `403 forbidden`, `user is blocked` |

Revocation, expiry and blocking are reported only after the secret matched.
A route that needs a user answers the admin token `403 forbidden`, `this route
needs a user`.

### GET /auth/providers

Public. `200 {"items":[{"name","authorize_url"}],"next_cursor":null}`, ordered
by name, with `Cache-Control: public, max-age=300`. `authorize_url` carries
`client_id` and `scope` (`user:email` for `github`; `openid email profile` and
`response_type=code` for `google`); the caller adds `redirect_uri` and
`state`. Client secrets are never shown.

### POST /auth/callback

Public; `Idempotency-Key` is ignored. Body: `provider` (a configured one),
`code` (not empty), `redirect_uri`, and optional `expires` (0, 30, 60 or 90
days; 90 when absent). `redirect_uri` must be `http` on `localhost` or
`127.0.0.1`, or share scheme, host and port with `[server].external_url`.

The hub exchanges the code with the provider (15-second timeout), reads the
user's id and verified e-mail, and in one transaction creates the user (with
a personal org and membership) or refreshes an existing one, revokes the
user's live API keys and issues a new one:

```json
{"user":{"id":"…","username":"alice","email":"a@x","full_name":"","role":"user","status":"active","provider":"github","provider_id":"123","created_at":"…","updated_at":"…"},
 "api_key":{"key":"parley_<key_id>_<secret>","key_id":"<key_id>","expires_at":"…"}}
```

| Status | error_type | When |
|---|---|---|
| 400 | `invalid_request` | a missing or invalid field, an unknown provider, a `redirect_uri` that is not allowed |
| 401 | `unauthenticated` | the provider rejected the code |
| 403 | `forbidden` | the user is blocked |
| 409 | `conflict` | the e-mail, username or org name is taken |
| 502 | `upstream_error` | the provider failed, timed out, or gave no id or verified e-mail |

A failed callback changes nothing.

## Every response

Every response carries `X-Request-ID`, `X-Content-Type-Options: nosniff`,
`X-Frame-Options: DENY` and `Referrer-Policy: no-referrer`. An incoming
`X-Request-ID` in the 36-character hyphenated UUID form is reused; otherwise
the hub generates a random version 4 UUID. Responses under `/api/v1` carry
`Cache-Control: no-store` unless the route states its own value.

Each request writes one access-log line with `method`, `path` (never the
query string), `status`, `duration_ms` and `request_id`. `/healthz` and
`/readyz` are logged, at debug level, only with
`[logging].log_health_probes = true`.

## Errors

Every error is JSON with `Content-Type: application/json; charset=utf-8`:

```json
{"error":{"code":409,"message":"workspace is busy","error_type":"workspace_busy"}}
```

`code` equals the HTTP status; `error_type` is the machine contract. An error
may carry extra members inside `error`, a named one or a `details` object.

| error_type | Status |
|---|---|
| `invalid_request` | 400 |
| `unauthenticated` | 401 |
| `forbidden`, `insufficient_scope`, `token_bound`, `path_not_allowed` | 403 |
| `not_found` | 404 |
| `method_not_allowed` | 405 |
| `conflict`, `workspace_archived` | 409 |
| `gone` | 410 |
| `payload_too_large` | 413 |
| `unsupported_media_type` | 415 |
| `rate_limited` | 429 |
| `internal` | 500 (message `internal error`; the cause goes to the log only) |
| `upstream_error` | 502 |
| `unavailable` | 503 |

Features add their own types with their own statuses.

## Order of checks

A request is checked in this order; the first failing check is the answer:

1. route match: an unknown path is `404 not_found`, a known path under another
   method `405 method_not_allowed` with `Allow` (also outside `/api/v1`;
   paths are never redirected);
2. authentication (a route that is not public and has no authenticator is
   `401`);
3. rate limit;
4. body size, then content type;
5. the route's feature check, then the binding guard;
6. personal access token scope;
7. workspace access or admin check;
8. the route's own validation, preconditions and action.

A public route skips steps 2 and 5–7.

## Request bodies

A body over the limit (`[server].max_body_size`, default 1 MiB, or the
route's own) is `413`, whether or not its length is declared. A `POST`,
`PUT` or `PATCH` with a body whose media type is not `application/json` (or
the route's own type) is `415`; a request without a body is exempt. A body
that is not one JSON object is `400`; unknown members are ignored.

## Lists

```json
{"items":[…],"next_cursor":"<opaque>"}
```

`items` is always an array; `next_cursor` is `null` on the last page.
`limit` defaults to 50 and must be 1–200 unless the route states other
bounds. `cursor` is the previous page's `next_cursor`; a malformed cursor, or
one issued for another route, path or filter, is `400`.

## Idempotency

A `POST` that creates a resource accepts `Idempotency-Key` (1–255
characters; empty or longer is `400`). A successful create is recorded under
the key, the caller and the route for 24 hours, in the transactional store,
so the record survives a restart. The same request again is answered `200`
with the recorded body, without show-once members such as a token value; the
same key with another body is `409 conflict`. Failed requests are not
recorded.

## Rate limit

Each credential gets `[limits].requests_per_minute` requests (default 600; 0
turns it off) per 60-second window, which starts at its first request.
Requests without a valid credential count against the client address. A
request over the limit is `429 rate_limited` with `Retry-After` in whole
seconds. Counters live in memory and reset at restart.

## Timestamps and durations

Timestamps are RFC 3339 in UTC with whole seconds (`2026-01-02T03:04:05Z`).
Durations are a whole number followed by `s`, `m`, `h` or `d` (`30s`, `15m`,
`24h`, `30d`).
