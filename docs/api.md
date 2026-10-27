# Parley REST API Reference

This is the reference of Parley's HTTP API: every route a client can call,
what it takes, what it answers and how it fails. It is for people writing
clients of Parley and for operators checking what a call does.

Parley is a port of `af-hub` and keeps its contract. Where this document is
silent, [the hub's API reference](prd/api.md) is the specification being
implemented; routes appear here as they are built.

## Contents

- [Conventions](#conventions)
- [Authentication](#authentication)
- [Permission Scopes](#permission-scopes)
- [Error Response Schema](#error-response-schema)
- [Signing in](#signing-in)
- [The caller's profile](#the-callers-profile)
- [API keys](#api-keys)
- [Personal access tokens](#personal-access-tokens)
- [Server Endpoints](#server-endpoints)

## Conventions

- **Base URL.** Versioned routes live under the mount point, `/api/v1` by
  default (`PARLEY_MOUNT_POINT`; see [configuration](configuration.md)). The
  four [server endpoints](#server-endpoints) are served outside it, at the
  server root, whatever the mount point is configured to.
- **Unmatched paths.** A request for a path that matches no route is
  answered `404` in the error envelope, whether or not it is under the mount
  point.
- **Methods.** A server endpoint requested with a method other than `GET` is
  answered `405` in the error envelope, and its handler does not run.
- **Bodies** are JSON. A request body larger than the configured maximum,
  1,048,576 bytes by default, is answered `413` before any route handler
  runs.
- **Timestamps** are RFC 3339, rendered in UTC and always ending in `Z`. A
  timestamp a client sends may carry any offset and is converted to UTC,
  preserving the instant; one that is not valid RFC 3339 is answered `400`
  with a message naming the field.

## Authentication

Every route except the public ones needs a credential in the
`Authorization: Bearer <credential>` header. The public routes are the four
[server endpoints](#server-endpoints), `GET /api/v1/auth/providers`,
`POST /api/v1/auth/callback` and the forge webhook ingress under
`/hooks/forge/`.

Three kinds of credential are accepted:

| Kind                  | Shape                             | Identity               | Scopes                    |
| --------------------- | --------------------------------- | ---------------------- | ------------------------- |
| API key               | `af_<key_id>_<secret>`            | the user it belongs to | every scope, implicitly   |
| Personal access token | `af_pat_<token_id>_<secret>`      | the user it belongs to | only those it was granted |
| Admin token           | the deployment's configured value | none — it owns nothing | every scope, implicitly   |

A request that carries no credential, or one the service does not accept, is
answered:

```json
{ "error": { "code": 401, "message": "authentication required" } }
```

The same answer is given whether the credential was absent, malformed,
unknown, revoked, expired, or belongs to a blocked user. A caller learns
nothing from it about which applied.

Blocking a user stops every credential of theirs working **without revoking
any of them**: the block is read on each request, so unblocking makes the
same credentials work again.

No credential, and no `Authorization` header, is written to a log record or
a response body. Secrets are stored only as a hash
([ADR 3](adr/03-hash-credential-secrets-with-one-pass-of-sha-256.md)).

## Permission Scopes

Every hub resource has one `read` and one `write` scope, and the identity
routes use their own. API keys and admin tokens hold them all; only a
personal access token is checked.

| Resource scopes                      |                                    |
| ------------------------------------ | ---------------------------------- |
| `workspaces:read` `workspaces:write` | `patches:read` `patches:write`     |
| `rebuilds:read` `rebuilds:write`     | `merges:read` `merges:write`       |
| `git:read` `git:write`               | `secrets:read` `secrets:write`     |
| `vars:read` `vars:write`             | `sessions:read` `sessions:write`   |
| `audit:read` `audit:write`           | `sandboxes:read` `sandboxes:write` |
| `forge:read` `forge:write`           |                                    |

The identity scopes are `users:read`, `keys:read`, `keys:manage`,
`tokens:read`, `tokens:write`, `tokens:manage`, `orgs:read` and
`orgs:write`.

`<resource>:write` grants `<resource>:read`. Nothing else is implied: a read
scope never reaches a write route, and `tokens:manage` does not grant
`tokens:read`.

A token that does not hold the scope a route needs is answered `403` in the
error envelope, with **no** `error_type`, and the route's handler does not
run.

These names of earlier hub releases are refused when a token asks for them:
`workspaces:create`, `workspaces:delete`, `workspaces:sync`, `secrets:list`,
`secrets:manage`, `secrets:delete`, `vars:manage` and `vars:delete`. So is
any name outside the registered set; either is answered `400` naming the
scope.

## Error Response Schema

Every error is answered as JSON in one envelope:

```json
{
  "error": {
    "code": 409,
    "message": "a rebuild job is already queued or running for this workspace",
    "error_type": "concurrent_rebuild"
  }
}
```

`code` repeats the HTTP status and `message` describes the error for a
person. `error_type` is a machine-readable class, present only where the
contract lists one for that condition and **absent** — not null — otherwise.

An unhandled failure is answered `500` with the message exactly
`internal server error`. No part of the underlying failure reaches the
response body; it goes to the log.

## Signing in

OAuth is the only source of an API key; there is no password login. Both
routes are public.

### GET /api/v1/auth/providers

The OAuth providers this deployment is configured with, and where to send a
person to authorise. Cached publicly (`Cache-Control: public, max-age=300`).

**Response:** `200 OK`

```json
[{ "name": "github", "authorize_url": "https://github.com/login/oauth/authorize?client_id=..." }]
```

A deployment that configured no provider answers `200` with an empty array:
sign-in is unavailable, every other route still works.

### POST /api/v1/auth/callback

Exchange a provider's authorization code for the user and a new API key.

**Request:**

| Field          | Required | Type    | Description                                                         |
| -------------- | -------- | ------- | ------------------------------------------------------------------- |
| `provider`     | yes      | string  | A provider from the list above                                      |
| `code`         | yes      | string  | The provider's authorization code                                   |
| `redirect_uri` | no       | string  | Must be on the provider's allowlist, where it has one               |
| `expires`      | no       | integer | Key lifetime in days: `0` (never), `30`, `60` or `90`. Default `90` |

**Behaviour.** The user is created or found, **every** API key that user
already holds is revoked, and one new key is issued — the revocation and the
issue in a single transaction, so a failure leaves neither half done.
Automation holding an older key of the same user stops working after a
sign-in; this is the hub's behaviour and Parley reproduces it. A user
signing in for the first time also gets a personal organization, owned by
them.

**Response:** `200 OK`. **The key is returned here and by no other route.**

```json
{
  "user": {
    "id": "uuid-string",
    "username": "alice",
    "email": "alice@example.com",
    "full_name": "Alice Example",
    "status": "active",
    "role": "user",
    "provider": "github",
    "provider_id": "12345",
    "created_at": "2026-03-01T12:00:00Z",
    "updated_at": "2026-03-01T12:00:00Z"
  },
  "api_key": {
    "key": "af_<key_id>_<secret>",
    "key_id": "<key_id>",
    "expires_at": "2026-05-30T12:00:00Z"
  }
}
```

`expires_at` is `null` for a key issued with `expires` of `0`.

**Errors.** None of them creates a user, issues a key or revokes one.

| Status | Condition                                                                                                                                                                 |
| ------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 400    | A body that is not a JSON object; `provider` or `code` missing; an unconfigured provider; a `redirect_uri` outside the allowlist; an `expires` other than 0, 30, 60 or 90 |
| 401    | The authorization code is invalid or expired                                                                                                                              |
| 403    | The user is blocked                                                                                                                                                       |
| 409    | The username belongs to another provider identity                                                                                                                         |

## The caller's profile

### GET /api/v1/user

The profile of the user behind the credential.

**Access:** `users:read`.

**Response:** `200 OK` with `id`, `username`, `email`, `full_name`,
`status`, `role`, `provider`, `provider_id`, `created_at` and `updated_at`.

**Errors:** `404` when the credential has no user of its own, which is the
case for an admin token.

### PATCH /api/v1/user

Change `full_name`, the only mutable field.

**Access:** `users:read`. **Request:** `{"full_name": "Alice Example"}`.

**Response:** `200 OK` with the updated profile.

**Errors:** `400` for a body that is not a JSON object, or a `full_name`
that is absent, null, empty or not a string; the stored profile is
unchanged.

## API keys

A key is created only by [signing in](#post-apiv1authcallback); these
routes list, rotate and revoke them.

### GET /api/v1/user/keys

**Access:** `keys:read`.

**Response:** `200 OK` with one entry per key — `key_id`, `created_at`,
`expires_at`, `revoked_at` — and an `ETag`. Revoked and expired keys are
listed too. **No entry carries a secret or its hash.** A repeat request
whose `If-None-Match` still matches is answered `304` with no body.

### POST /api/v1/user/keys/:key_id/refresh

Issue a new secret under the same `key_id`. The expiry is computed again
from the span the key was originally issued for, counted from now.

**Access:** an API key only.

**Response:** `200 OK` with `key`, `key_id` and `expires_at`. The previous
secret is refused from the next request on.

| Status | Condition                              |
| ------ | -------------------------------------- |
| 400    | The key is revoked or has expired      |
| 401    | The caller is not an API key           |
| 404    | No such key, or it is not the caller's |

### DELETE /api/v1/user/keys/:key_id

Revoke a key. A key may revoke itself; the request still completes, and the
next request made with it is refused `401`.

**Access:** `keys:manage`. **Response:** `204 No Content`.

**Errors:** `404` when no such key exists or it belongs to another user.
The two are answered identically, so a caller cannot learn which
identifiers exist.

## Personal access tokens

A token belongs to a user and is limited to the scopes it was granted.

### GET /api/v1/user/tokens

**Access:** `tokens:read`.

**Response:** `200 OK` with one entry per token — `token_id`, `name`,
`permissions`, `expires_at`, `created_at`, `revoked_at`. No entry carries
the token string or its hash.

### POST /api/v1/user/tokens

**Access:** `tokens:manage`. A token may grant only scopes it holds itself;
an API key may grant any registered scope.

**Request:**

| Field         | Required | Type     | Description                                                     |
| ------------- | -------- | -------- | --------------------------------------------------------------- |
| `name`        | yes      | string   | Label, at most 255 characters                                   |
| `permissions` | yes      | string[] | [Scopes](#permission-scopes) to grant, at least one             |
| `expires`     | no       | integer  | Lifetime in days: `0` (never), `30`, `60` or `90`. Default `90` |

**Response:** `201 Created` with the metadata and `token`, in the form
`af_pat_<token_id>_<secret>`. **The token is returned here and nowhere
else.**

| Status | Condition                                                                                                                                                             |
| ------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 400    | A body that is not an object; `name` absent, empty or too long; `permissions` absent, empty, not an array, or naming a retired or unregistered scope; a bad `expires` |
| 403    | A token granting a scope it does not hold                                                                                                                             |

### GET /api/v1/user/tokens/:token_id

**Access:** `tokens:read`. **Response:** `200 OK` with the metadata.
`404` when no such token exists or it is not the caller's.

### DELETE /api/v1/user/tokens/:token_id

**Access:** `tokens:manage`.

**Response:** `200 OK` with the token's metadata and `revoked_at` set —
**not** `204`. The token is refused `401` from the next request on.

| Status | Condition                                |
| ------ | ---------------------------------------- |
| 400    | The token is already revoked             |
| 404    | No such token, or it is not the caller's |

### PUT / PATCH / DELETE /api/v1/user/tokens/:token_id/permissions

Change a token's scopes: `PUT` replaces the set, `PATCH` adds to it,
`DELETE` removes from it.

**Access:** `tokens:write`. A token may add only scopes it holds itself;
removing needs no such check.

**Request:** `{"permissions": ["workspaces:read"]}`. `permissions` must be
present and not null.

**Response:** `200 OK` with the token's metadata after the change.
Removing a scope the token does not hold succeeds and changes nothing.

**`PUT` with an empty array** removes every permission **and revokes the
token**: the answer carries `"permissions": []` with `revoked_at` set, and
the token is refused `401` from the next request on. `PATCH` and `DELETE`
refuse an empty array.

| Status | Condition                                                                                                                          |
| ------ | ---------------------------------------------------------------------------------------------------------------------------------- |
| 400    | `permissions` absent or null; an empty array on `PATCH` or `DELETE`; a retired or unregistered scope; the token is already revoked |
| 403    | A token adding a scope it does not hold                                                                                            |
| 404    | No such token, or it is not the caller's                                                                                           |

## Server Endpoints

These four routes are served at the server root, outside the mount point,
and need no credential.

### GET /healthz

Liveness: the process is up. Answers without reading any store.

**Response:** `200 OK` with `{"status": "ok"}`.

### GET /readyz

Readiness: the service can serve.

**Response:** `200 OK` with `{"status": "ready"}` when the relational store
answers; `503` with the error envelope when it does not. The reason goes to
the log, not to the probe's answer.

### GET /version

Build information and the mount point in use. Cached publicly
(`Cache-Control: public, max-age=300`).

**Response:** `200 OK` with exactly these four keys:

```json
{
  "build_time": "2026-10-27T00:00:00Z",
  "commit": "abc1234",
  "mount_point": "/api/v1",
  "runtime_version": "Node.js/26"
}
```

`runtime_version` is the JavaScript runtime's version, reported under a
truthful key in place of the hub's `go_version`. `build_time` and `commit`
come from the deployment's environment and are the empty string where it
supplied neither. `build_time` is rendered as RFC 3339 in UTC whatever
offset the deployment supplied, and is the empty string when it supplied
something unreadable.

### GET /metrics

Operational metrics in the Prometheus text format, for a scraper.

**Response:** `200 OK`, `Content-Type: text/plain; version=0.0.4`.

Every metric the hub's reference names is exposed with the type and the
label names that reference gives it. A metric this service has no source
for yet is exposed with the value `0` rather than omitted, so a scraper's
queries do not start failing the day it gains one.

`afhub_http_requests_total` counts every answered request by `method`,
`path` and `status`, and `afhub_http_request_duration_seconds` observes its
latency in the documented buckets. The `path` label is the **route
template** the request matched, never the requested URI; every request that
matched no route is counted under one fixed value, so no caller can grow the
label set.
