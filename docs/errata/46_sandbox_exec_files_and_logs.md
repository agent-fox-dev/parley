# Errata: 46_sandbox_exec_files_and_logs

Where the implementation of package 46
(`.specs/46_sandbox_exec_files_and_logs`) differs from what the package says,
or decides what it leaves open.

## A PAT with `sandboxes:write` reads logs and downloads files

The package lists a PAT holding `sandboxes:write` but not `sandboxes:read` as
refused `403 insufficient_scope` on `GET …/files` and `GET …/logs`. The hub's
one scope check (`identity.Satisfies`) lets `<area>:write` satisfy
`<area>:read` for every area, and the two routes declare `sandboxes:read`
through package 44's `WorkspaceRoute`, so such a PAT is answered `200`, as on
the read routes of package 45 ([its errata](45_sandbox_creation_and_credentials.md)).
A PAT with no sandbox scope is refused. Test:
`TestUseRoutes_RefuseACallerWithoutTheScopeOrTheAccessBeforeTheRuntime`.

## The upload streams its body past the body stage

The body stage of the server reads every body into memory, up to a limit
fixed when the route is declared, and the hub declares its routes before the
configuration that holds `[sandbox].max_transfer_size` is loaded. The route
`PUT …/files` therefore declares the new `server.Route.StreamBody`: the body
stage reads nothing and checks no content type for it, and the upload checks
the content type (`415`) in its validation step, as the package's step 4.4
says, and counts the body against the limit itself. A refused upload is
answered with `Connection: close`, so that `net/http` does not read on into
the body before it writes the answer. Code: `internal/server/body.go`,
`internal/sandbox/files.go`; tests:
`TestBodyStage_LeavesTheBodyOfAStreamingRouteToItsHandler`,
`TestUpload_RefusesABodyOverTheTransferLimit`.

## The runtime holds the sandbox's environment

The package runs a command and opens a terminal "with the sandbox's
environment plus the request's env". The runtime keeps the environment a
sandbox was created with and lays a request's environment over it
(`runtime.ExecRequest`), so exec sends the request's `env` alone and attach
sends none. The request's value wins over the sandbox's for the same name.
Code: `internal/sandbox/exec.go`, `internal/sandbox/attach.go`.

## Exec's `env` follows the create request's rule

The package refuses an `env` value over 8 KiB. Exec judges `env` with the
validator of the create request (package 45's `readEnv`), as the package's
step 2.3 says: at most 64 entries, each name a variable name that does not
start with `PARLEY_` or `OPENSHELL_` and is not a name of the sandbox's
`exposed_secrets`, and the name and value of an entry together at most 8,192
bytes. Test: `TestExec_RefusesAnInvalidBodyAndRunsNothing`.

## The main process's output reaches the hub as lines

The package has the hub split the main process's output into lines and hold
an unfinished line until its end arrives. The runtime delivers the output as
lines already (`runtime.LogEntry`), so the collector stores each entry as one
line, splits an entry that holds line ends at them and drops a trailing line
end, and holds nothing between entries. A line the process writes in two
writes is one line. Test: `TestLogCollector_StoresWhatASandboxLogs`.

## A policy entry without text gets a line of its fields

The runtime's policy entry need not carry a text. The collector stores its
text as `line` when it has one, and otherwise the line
`<action> <host>:<port> <program>`, followed by the method and the path when
the entry has them. Every text column of a stored line (`line`, `program`,
`host`, `credential`) is redacted, not only `line`. Code:
`internal/sandbox/logs_collect.go`.

## Log lines are redacted without the 1,024-byte cut

The scrubbing function of package 45 cuts its result to 1,024 bytes, which
fits error messages and not log lines of up to 16 KiB. The collector replaces
the exposed values of at least 8 bytes, the credential values and the hub
credential's value, as that function does, without cutting the line. Error
messages, the runtime-error answer included, go through the function itself
and then the hub's scrubber. Code: `internal/sandbox/logs_collect.go`,
`internal/sandbox/use.go`.

## `parley sandbox exec --json` prints the answer of a command that timed out

The package says `--json` prints the hub's answer, and that a timeout exits
without printing a result document. With `--json` a command that timed out
prints the hub's answer, which holds the output received, on stdout, then the
`wait_timeout` envelope on stderr, with exit status 1. The streamed form
prints the output received and then `wait_timeout`. Test:
`TestSandboxExec_JSONPrintsTheCollectedAnswer`.

## The CLI's commands are tested against the CLI's stub hub

The package's checks of the five commands start a hub with the fake runtime.
`internal/cli`'s tests run every command against the package's stub hub,
which records what a command sends and answers what the hub answers; what the
whole hub does with it is tested in `internal/sandbox`, and the paths with
the CLI against a whole hub in `test/e2e/sandbox_use_test.go`.

## The modules of the WebSocket and the terminal

`golang.org/x/net` stays at v0.60.0, which go.mod held already; its
`websocket` package has the signatures the package read in v0.61.0. v0.60.0
requires `golang.org/x/term` v0.46.0, so the CLI uses v0.46.0, not v0.45.0;
`IsTerminal`, `MakeRaw`, `Restore` and `GetSize` have the package's
signatures in it.

## Smaller choices

- `websocket.Server` takes the hijacked connection only from a writer that
  implements `http.Hijacker`, and the server's writers do not; the attach
  handler hands it a writer that hijacks through `http.ResponseController`.
- A tar entry the hub writes on a download carries the hub's time, in whole
  seconds, as its modification time: the runtime reports none.
- A followed log also wakes on every state change of a sandbox, so that its
  end at `deleted` does not wait for the next second.
- `parley sandbox download` writes a file with mode `0644`, since the hub
  sends no mode for a file, and writes the tar stream itself to stdout for a
  directory and `-`.
- `parley sandbox shell` asks again with the upgrade headers, not as a plain
  `GET`, to read the hub's refusal: the hub judges a plain `GET` as `400`
  before it looks at the sandbox's state.
