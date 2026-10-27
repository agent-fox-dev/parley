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
