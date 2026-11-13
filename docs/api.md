# API reference

The hub, `parleyd`, answers the routes below over HTTP. The REST API under
`/api/v1` does not exist yet; this page grows as it lands.

## Conventions so far

- The three routes below need no credential. They never read the
  `Authorization` header, so a request with or without one, valid or not, gets
  the same answer.
- Every body is one JSON object, sent with
  `Content-Type: application/json; charset=utf-8`.
- An unknown path or a wrong method gets the Go standard library router's own
  answer: `404 Not Found` or `405 Method Not Allowed` with a plain-text body.
  The JSON error envelope is not in place yet.

## `GET /healthz`

Liveness: the process is up and serving HTTP.

| Status | Body |
|---|---|
| `200 OK` | `{"status":"ok"}` |

## `GET /readyz`

Readiness: whether the hub can do its work. Each request calls every
registered readiness probe. The hub is ready when every probe succeeds, and
also when no probe is registered.

| Status | When | Body |
|---|---|---|
| `200 OK` | every probe succeeded, or none is registered | `{"status":"ready"}` |
| `503 Service Unavailable` | at least one probe failed | `{"status":"not ready"}` |

The `503` body is this plain status document, not an error envelope, and it
never carries the probe's error text. `GET /healthz` still answers `200` while
the hub is not ready.

## `GET /version`

The build of the running hub.

| Status | Body |
|---|---|
| `200 OK` | an object with exactly the string members below |

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
