---
spec_id: "01"
spec_name: "hub_skeleton"
title: "Ship the hub as two programs"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "368e4624811479ba7c7dee3c7dfe94aec329d390fdfaf74c62828a39ac6801f5"
schema_version: 3
source: "docs/prd/00_overview.md"
---

# Ship the hub as two programs

## Intent

Parley ships as two programs: `parleyd`, the server, and `parley`, the command-line client. This package makes both exist in their smallest useful form, so that an operator can start the hub, see that it is alive and ready, read which build is running, and stop it cleanly, and so that every later package has a running server, a client and a fixed repository layout to add its behaviour to.

## Goals

- `parleyd` starts, listens for HTTP, logs the address it listens on, and exits with status 1 and one error line when it cannot listen.
- `GET /healthz`, `GET /readyz` and `GET /version` answer without a credential, with the bodies the PRD gives.
- `parleyd` stops on SIGTERM or SIGINT: no new connections, in-flight requests get 15 seconds, exit status 0.
- `parley` alone prints help and exits 0; `parley version` prints the client's build and, when a hub URL is given and the hub answers, the hub's build; it exits 0 whether or not the hub answers.
- The version, commit and build time set when a binary is built are the ones `GET /version` and `parley version` report.
- The directories `cmd/parleyd`, `cmd/parley`, `internal/server`, `internal/cli`, `internal/buildinfo` and `test/e2e` exist with working code and tests, so later packages add to them.

## Non-goals

- The error envelope, the answers for unknown paths and wrong methods (`404`, `405`), `Cache-Control` and security headers, request ids, the body limit and rate limits: package 02 `http_api_conventions`.
- The configuration file and its `[server].bind` and `[server].port` keys, the data directory and the stores, the admin-token bootstrap with the flags `--admin-email` and `--reset-admin-token`, the maintenance loop, resuming durable work at start, the configuration path and data directory in the start log line, scrubbing, and `parleyd backup`: package 03 `server_lifecycle_and_configuration`.
- The readiness probe that checks the hub's data: package 03 registers it. This package delivers the endpoint and the way to register a probe.
- The client configuration file, the global flag `--api-key`, the `--json` help form, `parley help`, the error envelope on stderr and the exit codes 1, 2 and 3 for usage, client and hub errors: package 09 `cli_foundation`. Until then only `parley` and `parley version` have specified behaviour.
- The `sandbox_runtime` member of `GET /version`: package 44 `sandbox_runtime_and_profiles`.
- `GET /metrics`: package 51 `audit_query_and_retention`.
- The `git` version check at start (NFR-004): not covered by this package, which never runs `git`; it belongs to package 54 `operations_and_packaging`.
- Structured logging configuration (`[logging]`), container images and manifests: packages 03 and 54 `operations_and_packaging`.
- The REST API under `/api/v1`, the git server and the webhook ingress: packages 04 to 53.

## Background

The repository holds no code yet; every file named here is new. It has `go.mod` (module `github.com/agent-fox-dev/parley`, `go 1.27`), `.golangci.yml`, `README.md`, `LICENSE` and the product requirements under `docs/prd/`.

The product overview (`docs/prd/00_overview.md` §1.3) says the hub ships as two programs. `parleyd` is the server: it serves a REST API under `/api/v1`, a git smart-HTTP server under `/git`, a forge webhook ingress under `/hooks/forge`, and metrics and health endpoints. `parley` is the command-line client; it speaks only the REST API. §2.3 fixes the convention that paths are relative to `/api/v1` unless they start with `/git`, `/hooks`, `/metrics`, `/healthz`, `/readyz` or `/version`. Parley runs as one `parleyd` process (§3.3.1, NFR-001).

This package is the foundation of 55. It builds the two programs with only the behaviour that needs nothing else: start, three unauthenticated endpoints, stop, and the client's version command. The layout it starts is:

| Path | Holds |
|---|---|
| `cmd/parleyd/` | the server binary's `main`: argument handling, signals, exit status |
| `cmd/parley/` | the CLI binary's `main` |
| `internal/server/` | the HTTP server: router, the health, readiness and version handlers, serve and shut down |
| `internal/cli/` | the CLI root command and one file per command group (`internal/cli/version.go`) |
| `internal/buildinfo/` | version, commit and build time set at build time, shared by both programs |
| `test/e2e/` | end-to-end tests that build and run the real binaries |

Tests live beside the code (`internal/server/server_test.go`).

Later packages add to this layout: package 02 `http_api_conventions` adds the request pipeline to `internal/server/` and the leaf package `internal/server/timefmt` (timestamps and durations); package 03 `server_lifecycle_and_configuration` adds `internal/config/`, `internal/store/`, `internal/scrub/` and `internal/hub/`. From package 03 on, `internal/hub/` composes the process (its `Run` takes over from the run function of `cmd/parleyd`, which becomes a thin caller), and every later package registers its routes, checks, schema steps and tasks there, not in `cmd/parleyd`.

## Requirements

### Starting `parleyd`

An operator starts `parleyd` with no arguments. It listens for HTTP on `0.0.0.0:8080`, the PRD's default for `[server].bind` and `[server].port` (§3.9), and when it starts listening it writes one JSON log line to stderr whose `msg` is `listening` and whose `addr` is the host and port it actually bound (PLAT-FR-002). A port of 0 in the listen address means a free port chosen by the operating system, and `addr` then carries the chosen port.

If the listener cannot be bound (the port is in use, the address is not local), `parleyd` writes one error line to stderr and exits with status 1 without serving (PLAT-FR-002, PLAT-FR-004).

`parleyd --help` prints usage on stdout and exits 0 without listening. An unknown flag, or any positional argument, prints usage on stderr and exits 2 without listening (PLAT-FR-001).

### Health, readiness and version

All three routes are outside `/api/v1`, need no credential, and answer `GET` with a JSON body and the content type `application/json; charset=utf-8` (§3.5, NFR-041).

- `GET /healthz` is the liveness probe: `200` with `{"status":"ok"}`.
- `GET /readyz` is the readiness probe: `200` with `{"status":"ready"}` when the hub can reach its data, otherwise `503` with `{"status":"not ready"}`. The 503 body is not the error envelope. Readiness reports only whether the hub can reach its own data (§3.10). The server holds a list of readiness probes; the hub is ready when every registered probe succeeds, and with none registered it is ready. This package registers none.
- `GET /version` is the build information: `200` with `{"version":"…","commit":"…","build_time":"…","go_version":"…"}`. `version` is the release version, or `dev` for an untagged build. `go_version` is the toolchain that built the binary.

Version, commit and build time are set at build time and reported by `GET /version` and `parley version` (NFR-005). A binary built without them reports `version` `dev`, `commit` `unknown` and `build_time` `unknown`.

### Stopping `parleyd`

On SIGTERM or SIGINT the server stops accepting connections and gives in-flight requests 15 seconds to finish. A clean shutdown exits 0 (PLAT-FR-004). A request that is in flight when the signal arrives and finishes within the 15 seconds gets its complete response. A request still running after 15 seconds has its connection closed, and the process exits with status 0 no later than 16 seconds after the signal.

### The `parley` client

`parley` alone prints help that lists the `version` command, on stdout, and exits 0 without a credential and without a request (PLAT-FR-095).

`parley version` shows the client and server versions (§3.6). No credential is needed. The hub URL is the first non-empty value of the flag `--endpoint-url` and the environment variable `PARLEY_ENDPOINT_URL` (PLAT-FR-090).

- With a hub URL, it calls `GET <hub URL without trailing slash>/version` with a 5-second timeout and prints `{"cli":{"version":"…","commit":"…"},"server":{"version":"…","commit":"…","build_time":"…","go_version":"…"}}` on stdout.
- With no hub URL, it makes no request and prints `{"cli":{…}}` with no `server` member and nothing on stderr.
- If the hub cannot be reached, stderr gets a warning and `server` is omitted. The same holds when the hub does not answer within 5 seconds, answers a status other than 200, or answers a body that is not a JSON object.
- stdout carries one JSON document, indented by 2 spaces, followed by a newline (PLAT-FR-092).
- The exit status is 0 in every one of these cases: a hub that cannot be read is never a failure of the command. Package 09 `cli_foundation` adds the client configuration file as a third source of the hub URL (an unparseable file is a warning there, still exit 0) and makes an extra argument or an unknown flag a usage error with exit 2.

### End-to-end flow

An operator starts `parleyd`; it listens and logs its address. The operator's monitor reads `GET /healthz` and `GET /readyz` and gets `200` from both. A developer runs `parley version --endpoint-url <hub URL>`; the CLI calls `GET /version`; `parleyd` answers with its build information; the CLI prints both builds and exits 0. The operator sends SIGTERM; `parleyd` stops accepting connections and exits 0.

## Design Decisions

1. **Language Go, standard library HTTP.** The server uses `net/http` with its method-and-pattern router and no web framework (project decisions 1 and 3).
2. **The CLI uses `github.com/spf13/cobra`** (project decision 4). `parleyd` has no subcommands in this package and parses its flags with the standard `flag` package.
3. **Tests** use the standard `testing` package with table-driven tests; handlers are tested with `net/http/httptest` (project decision 7).
4. **Listen address before configuration exists.** The configuration file is package 03. Until then `parleyd` listens on the PRD's defaults, `0.0.0.0:8080`. The listen address and the shutdown grace are parameters of the function `cmd/parleyd` calls, so tests listen on `127.0.0.1:0` and use a grace of milliseconds, and package 03 passes the configured values.
5. **Readiness is a list of probes.** The PRD says `/readyz` answers 503 when the hub cannot reach its data, and the stores arrive with package 03. The server takes readiness probes (a function that returns an error when not ready); none registered means ready. Why: the endpoint's full contract is testable now and package 03 only registers a probe.
6. **Build values that were not set.** The PRD names `dev` for an untagged build's `version` and is silent on the other two; `commit` and `build_time` are then the string `unknown`. Why: a fixed, non-empty value is the simplest thing a reader can tell apart from a real one.
7. **Build values are set with the linker.** `go build -ldflags "-X github.com/agent-fox-dev/parley/internal/buildinfo.Version=… -X …buildinfo.Commit=… -X …buildinfo.BuildTime=…"`. `commit` and `build_time` are reported exactly as given. `internal/buildinfo` is a directory the brief's layout does not list; both programs need the values and the client must not import server code (NFR-004).
8. **The start log line** is one JSON object on stderr written with `log/slog`, with `msg` `listening` and `addr`. The PRD's configuration path and data directory are added to it by package 03.
9. **Positional arguments to `parleyd`** are refused like an unknown flag (usage, exit 2), because PLAT-FR-001 says there are no subcommands. The one exception the product has is `parleyd backup <directory>`, which package 03 `server_lifecycle_and_configuration` adds; this package's criteria and tests therefore say nothing about the word `backup` as first argument, so package 03 changes no test of this package.
10. **Exit status after an expired grace** is 0. The PRD says a clean shutdown exits 0 and gives no other status for a signalled stop.
11. **Hub URL for `parley version`.** This package reads the flag `--endpoint-url` and the variable `PARLEY_ENDPOINT_URL`, in that order; the client configuration file as a third source is package 09. Why: the flow from client to server needs a URL, and these two sources need no file format.
12. **Every failure to read the hub's version** (connection refused, timeout, a status other than 200, an undecodable body) is treated as "cannot be reached": a warning and no `server` member. The PRD names only the unreachable case and says the exit status is always 0. Agreed with package 09 `cli_foundation`: the warning is one plain line that begins with `warning: `, the form package 09 gives every CLI warning; an unparseable client configuration file is likewise a warning and exit 0 for this command, and only a usage error (package 09) gives it another exit status.
13. **Content type** of the three endpoints is `application/json; charset=utf-8`, the one PLAT-FR-011 gives for errors; the PRD gives none for these routes.
14. **Profile: full**, with one execution path, although the package is small: it is the flow every later package extends.

## Dependencies

None. This is the first package; packages 02, 03 and 09 build directly on it.

## Verified External API

`github.com/spf13/cobra` v1.10.2, read in the module cache (`command.go`):

- `func (c *Command) Execute() error`
- `func (c *Command) ExecuteContext(ctx context.Context) error`
- `func (c *Command) SetArgs(a []string)`
- `func (c *Command) SetOut(newOut io.Writer)`
- `func (c *Command) SetErr(newErr io.Writer)`
- `func (c *Command) AddCommand(cmds ...*Command)`
- `func (c *Command) PersistentFlags() *flag.FlagSet` (`flag` is `github.com/spf13/pflag`)

`github.com/spf13/pflag` v1.0.10 (`string.go`):

- `func (f *FlagSet) StringVar(p *string, name string, value string, usage string)`

## Glossary

- **Hub**: The `parleyd` server.
