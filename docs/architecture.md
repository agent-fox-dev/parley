# Architecture

Parley ships as two programs: `parleyd`, the hub (server), and `parley`, the
command-line client. Both are written in Go, in the module
`github.com/agent-fox-dev/parley`, and the server uses the standard library's
`net/http` with no web framework.

## Package layout

| Package | Holds |
|---|---|
| `cmd/parley` | the `parley` program: it calls `cli.Run` and exits with its status |
| `cmd/parleyd` | the `parleyd` program: argument parsing, signals, the start log line and the exit status |
| `internal/buildinfo` | the version, commit and build time the binary was built with, shared by both programs |
| `internal/cli` | the `parley` client: its root command and its subcommands, built with `github.com/spf13/cobra` |
| `internal/server` | the hub's HTTP server: its routes and its readiness probes |
| `test/e2e` | tests that build the binaries and run them |

## The HTTP server

`server.New(server.Options)` builds the server, and `(*Server).Handler()`
returns the `http.Handler` that serves its routes. `Options` holds the listen
address (`Addr`), the shutdown grace (`ShutdownGrace`), the readiness
probes (`ReadinessProbes`) and `OnListen`, a function `Serve` calls once with
the address it bound (with port `0` in `Addr`, the port the system chose).
Later packages mount their routes with `(*Server).Handle(pattern, handler)`,
which takes the same patterns as `http.ServeMux`, before `Serve` is called.

Routes are registered on a standard library `http.ServeMux` with method and
path patterns (`GET /healthz`, `GET /readyz`, `GET /version`). Unknown paths
and wrong methods are left to the mux's own `404` and `405` answers. Every
route writes its body through one JSON helper, which sets
`Content-Type: application/json; charset=utf-8`. The routes are described in
[api.md](api.md).

### Serving and stopping

`(*Server).Serve(ctx)` binds `Addr` with `net.Listen("tcp", …)`, returns the
error (`listen on <addr>: …`) when it cannot, calls `OnListen`, and serves
until `ctx` is cancelled. Then it stops in this order:

1. It closes the listener, so new connections are refused.
2. It waits up to 100 ms for connections that are accepted but whose request
   has not been read yet. `http.Server.Shutdown` would drop those, and a
   request the client had already sent would get no answer.
3. It calls `http.Server.Shutdown` with a deadline of `ShutdownGrace`, which
   waits for in-flight requests to finish and closes idle connections.
4. When the grace passes first, it calls `http.Server.Close`, which closes the
   remaining connections; their clients get no response. `Serve` does not
   wait for those handlers to return.

`Serve` returns `nil` after a stop, whether or not the grace passed.

## parleyd

`cmd/parleyd` holds `main` and a `run` function that does the work:
`run(ctx, args, addr, grace, stdout, stderr)` returns the exit status, and
`main` calls it with the defaults `0.0.0.0:8080` and 15 seconds and passes the
result to `os.Exit`. Tests call `run` with `127.0.0.1:0` and a grace of
milliseconds. `run`:

1. parses the arguments with the standard `flag` package (`--help` prints the
   usage on stdout and returns `0`; an unknown flag or any positional
   argument prints an error and the usage on stderr and returns `2`);
2. derives the serving context with `signal.NotifyContext` for `SIGTERM` and
   `SIGINT`, before the listener is bound;
3. builds the server and calls `Serve`, with an `OnListen` that writes the
   start line, a JSON object with `"msg":"listening"` and the bound
   `"addr"`, to stderr with `log/slog`'s JSON handler;
4. returns `1` after one `cannot listen` error line when the address cannot
   be bound, and `0` when `Serve` returns after a stop.

The flags, exit statuses and log lines are described in [cli.md](cli.md),
the fixed address and grace in [configuration.md](configuration.md).

### Readiness probes

A readiness probe is a `server.Probe`, a `func(context.Context) error` that
returns an error when one part of the hub is not ready. `GET /readyz` calls
every probe with the request's context, including those after one has failed,
and answers `503` with `{"status":"not ready"}` if any failed. With no probe
registered the hub is ready. A probe's error text never goes into the
response.

## parley

`cmd/parley` calls `cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)` and passes
its result to `os.Exit`. `Run` builds a fresh cobra command tree on each call:
the root command `parley` with the persistent flag `--endpoint-url`, and the
`version` subcommand. It executes the tree with the arguments, and returns `0`,
or `2` after one `error: <message>` line on stderr when cobra cannot parse the
arguments. Cobra's own error and usage output is silenced.

`parley version` reads its build from `internal/buildinfo` and asks the hub
for its build with a plain `net/http` client over HTTP (`GET /version`, 5 s
timeout). `internal/cli` imports `internal/buildinfo` and nothing from
`internal/server`, so the client carries no server code. The commands, output
and exit statuses are described in [cli.md](cli.md).

## Build information

`internal/buildinfo` has three package-level string variables, `Version`,
`Commit` and `BuildTime`, that are set when the binary is linked, with one `-X` linker flag each:

```sh
go build -ldflags "\
  -X github.com/agent-fox-dev/parley/internal/buildinfo.Version=1.4.0 \
  -X github.com/agent-fox-dev/parley/internal/buildinfo.Commit=3f2a1bc \
  -X github.com/agent-fox-dev/parley/internal/buildinfo.BuildTime=2026-01-02T03:04:05Z" \
  ./cmd/parleyd
```

The same flags build `./cmd/parley`. `test/e2e` builds both programs this way
and checks that `parley version` reports the values.

`buildinfo.Get()` returns them as a `buildinfo.Info`, with the Go toolchain's
version (`runtime.Version()`) as `GoVersion`. A value that was not set is
reported as `dev` (version) or `unknown` (commit and build time); a value that
was set is reported unchanged. `GET /version` calls `Get()` on every request.

The package lives under `internal/` and outside `internal/server` so that the
client can report its own build without importing server code.
