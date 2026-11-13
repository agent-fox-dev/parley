# CLI reference

Parley ships two programs: `parleyd`, the hub, and `parley`, the
command-line client. This page describes `parleyd`; `parley` is not built yet.

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
