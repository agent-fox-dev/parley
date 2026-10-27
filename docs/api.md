# Parley REST API Reference

This is the reference of Parley's HTTP API: every route a client can call,
what it takes, what it answers and how it fails. It is for people writing
clients of Parley and for operators checking what a call does.

Parley is a port of `af-hub` and keeps its contract. Where this document is
silent, [the hub's API reference](prd/api.md) is the specification being
implemented; routes appear here as they are built.

## Contents

- [Conventions](#conventions)
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

**Response:** `200 OK` with `{"status": "ready"}`; `503` with the error
envelope once a relational store is wired and does not answer.

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
supplied neither.

### GET /metrics

Operational metrics in the Prometheus text format, for a scraper.

**Response:** `200 OK`, `Content-Type: text/plain; version=0.0.4`.
