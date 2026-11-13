# CLI reference

Parley ships two programs: `parleyd`, the hub, and `parley`, the
command-line client. This page describes both.

## `parleyd`

```
Usage: parleyd [flags]
```

`parleyd` takes no arguments. It listens for HTTP on `0.0.0.0:8080` and runs
until it receives `SIGTERM` or `SIGINT`. The address and the shutdown grace are
fixed for now; see [configuration.md](configuration.md).

### Flags

| Flag | Effect |
|---|---|
| `-h`, `--help` | print the usage on stdout and exit `0` without listening |

`--help` prints:

```
Usage: parleyd [flags]

parleyd is the Parley hub. It takes no arguments, listens for HTTP on 0.0.0.0:8080
and runs until it receives SIGTERM or SIGINT; in-flight requests then get 15s
to finish.

Flags:
  -h, --help   print this help and exit
```

A flag it does not define, or any positional argument, prints one error line
that names the argument, then the same usage, on stderr, and exits `2` without
listening:

```
parleyd: argument "--nope": flag provided but not defined: -nope
parleyd: unexpected argument "serve"
```

### Log lines

`parleyd` logs JSON objects to stderr, one per line, with `log/slog`. Once it
listens it writes:

```json
{"time":"2026-11-13T10:11:20.772527+01:00","level":"INFO","msg":"listening","addr":"[::]:8080"}
```

`addr` is the address the listener bound, with the port the system chose when
the port is `0`. Go binds the wildcard `0.0.0.0` as a dual-stack listener, so
the default address is logged as `[::]:8080`.

While it serves, it writes one access-log line for every request it has
answered, except requests to `/healthz` and `/readyz`:

```json
{"time":"2026-11-13T10:12:03.104211+01:00","level":"INFO","msg":"request","method":"GET","path":"/version","status":200,"duration_ms":0.087,"request_id":"0b5c6f2e-8d1a-4c3b-9e7f-2a4d6c8e0f13"}
```

`path` is the request's URL path without the query string. `status` is the
status it answered with, `duration_ms` the time from receiving the request to
answering it in milliseconds, and `request_id` the `X-Request-ID` of the
response (see [api.md](api.md#request-ids-and-response-headers)).

When a REST request fails in a way the hub did not expect, the client gets
`500 internal error` and the cause goes to stderr in one line with the same
`request_id`; a panic is logged with `"msg":"panic"` and also carries the
goroutine's `stack`:

```json
{"time":"2026-11-13T10:12:09.551873+01:00","level":"ERROR","msg":"internal error","request_id":"0b5c6f2e-8d1a-4c3b-9e7f-2a4d6c8e0f13","error":"open workspace: permission denied"}
```

When it cannot bind the address it writes one line and exits `1`, without
serving:

```json
{"time":"2026-11-13T10:11:41.348421+01:00","level":"ERROR","msg":"cannot listen","addr":"0.0.0.0:8080","error":"listen on 0.0.0.0:8080: listen tcp 0.0.0.0:8080: bind: address already in use"}
```

### Signals

On `SIGTERM` or `SIGINT` `parleyd` stops accepting connections, lets the
requests in flight finish for up to 15 seconds, closes the connections of the
requests that are still running after that, and exits `0`.

### Exit statuses

| Status | When |
|---|---|
| `0` | `--help`, or a stop by `SIGTERM` or `SIGINT`, whether or not the grace passed |
| `1` | the listen address cannot be bound |
| `2` | an unknown flag or a positional argument |

## `parley`

```
Usage:
  parley [command]
```

Run without arguments (or with `--help`), `parley` prints its help on stdout
and exits `0`. It reads no credential and sends no request:

```
parley is the command-line client of the Parley hub

Usage:
  parley [command]

Available Commands:
  help        Help about any command
  version     Print the client's build and, given a hub URL, the hub's build

Flags:
      --endpoint-url string   URL of the hub (default: $PARLEY_ENDPOINT_URL)
  -h, --help                  help for parley

Use "parley [command] --help" for more information about a command.
```

### Global flags

| Flag | Effect |
|---|---|
| `--endpoint-url <url>` | the URL of the hub, for example `http://127.0.0.1:8080`. When it is absent or empty, `PARLEY_ENDPOINT_URL` gives it (see [configuration.md](configuration.md)) |
| `-h`, `--help` | print the help of the command on stdout and exit `0` |

### `parley version`

```
Usage:
  parley version [flags]
```

Prints the client's build as one JSON document on stdout, indented by two
spaces and followed by a newline. Without a hub URL it sends no request and
prints the client's build alone:

```json
{
  "cli": {
    "version": "1.4.0",
    "commit": "3f2a1bc"
  }
}
```

With a hub URL it sends `GET <hub URL>/version` (a trailing slash on the URL is
dropped) without an `Authorization` header and with a timeout of 5 seconds.
When the hub answers `200` with a JSON object, the document also holds the
hub's build in `server`:

```json
{
  "cli": {
    "version": "1.4.0",
    "commit": "3f2a1bc"
  },
  "server": {
    "version": "1.4.0",
    "commit": "3f2a1bc",
    "build_time": "2026-01-02T03:04:05Z",
    "go_version": "go1.27.3"
  }
}
```

When the connection is refused, the hub does not answer within the timeout,
or it answers a status other than `200` or a body that is not a JSON object,
`parley version` writes one warning line that names the hub URL to stderr and
prints the document without `server`:

```
warning: cannot read the hub's version from http://127.0.0.1:1: Get "http://127.0.0.1:1/version": dial tcp 127.0.0.1:1: connect: connection refused
```

`parley version` takes no arguments.

### Exit statuses

| Status | When |
|---|---|
| `0` | help, or `parley version`, whether or not the hub answered |
| `2` | an unknown command or flag, or an argument a command does not take; `parley` writes one line `error: <message>` to stderr, for example `error: unknown command "nope" for "parley"` |
