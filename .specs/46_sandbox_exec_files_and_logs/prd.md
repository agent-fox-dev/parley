---
spec_id: "46"
spec_name: "sandbox_exec_files_and_logs"
title: "Use a sandbox through the REST API"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "b6cace1c4101dcaf848f32e88ce0eb13b9839db1dbb42ba68682f751ef61bc49"
schema_version: 3
source: "docs/prd/13_sandboxes.md"
---

# Use a sandbox through the REST API

## Intent

An agent or a person who has a sandbox must be able to work in it with nothing but the hub's REST API and the `parley` CLI: run commands and read their output, move files in and out, read what the main process and the network policy logged, and open an interactive terminal. No client of the sandbox runtime is needed, a weak credential cannot act through a sandbox that a stronger one created, and the hub knows when a sandbox was last used.

## Goals

- `POST /workspaces/:slug/sandboxes/:id/exec` runs a command and answers with its exit code and its output, collected (up to 1 MiB per stream) or as an event stream without that cap.
- A command that runs longer than its `timeout` is killed and reported with `timed_out: true`; a command whose caller goes away is killed.
- At most `[sandbox].max_concurrent_execs` commands (default 8) run at once in one sandbox; one more is answered `429`.
- `PUT` and `GET /workspaces/:slug/sandboxes/:id/files` move one file or a tar stream, only under the sandbox's working directory, up to `[sandbox].max_transfer_size` (default 1 GiB).
- The hub stores the output of the main process and the runtime's policy log for each sandbox, with exposed secret values replaced by `[redacted]`, keeps the last `[sandbox].log_retention` (default 7 days) and serves it as pages or as a followed stream.
- `GET /workspaces/:slug/sandboxes/:id/attach` gives an interactive terminal over WebSocket on a new shell.
- Exec, attach, file transfer and output of the main process each set the sandbox's `last_activity_at`.
- `parley sandbox exec`, `shell`, `logs`, `upload` and `download` do all of this from a shell, and `parley sandbox exec` exits with the command's exit code.

## Non-goals

- The `[sandbox]` configuration (including `max_exec_timeout`, `max_concurrent_execs`, `max_transfer_size` and `log_retention`), its validation, the start-up runtime check, profiles, the runtime client interface with its fake, the check order of sandbox routes and the no-widening rule itself: package 44 sandbox_runtime_and_profiles. This package reads those values, calls that rule and the runtime-error answer, and calls the runtime client by the operation names that package declares, adding none.
- The sandbox record, its state machine, creation, credentials, exposed secrets and their resolution, the checkout, and `parley sandbox create|list|get`: package 45 sandbox_creation_and_credentials.
- Stop, start, idle stop (which reads the `last_activity_at` this package writes), expiry, delete, checkpoints, reconciliation, the removal of `deleted` records after 24 hours and hub shutdown: package 47 sandbox_lifecycle_and_checkpoints. When it removes a record it runs the purge hooks of package 45, through which this package removes the sandbox's stored lines.
- Denial events (`parley.sandbox.deny`), proposals and workspace sandbox rules: package 48 sandbox_network_policy. This package only stores the policy log lines and serves them.
- The telemetry record of sandbox use (`sandbox.exec`, `sandbox.attach`, `sandbox.upload`, `sandbox.download`, AUD-FR-026): package 50 telemetry_ingestion. This package offers the observer registry that record is written from; `parleyd` registers the recorder of package 50 with it where that package exists.
- The metric catalogue and `/metrics`: package 51 audit_query_and_retention. This package reports `parley_sandbox_exec_total{result}` to the metrics recorder that package 44 declares.
- The error envelope, list paging, body limits and the check order of every route: package 02 http_api_conventions. The CLI's settings, output and exit codes: package 09 cli_foundation.
- Hub events: exec, attach, file transfer and log reads emit none (§13.8).
- Redaction of exec output, the exec stream, the attach terminal and downloaded files: the source PRD excludes it (SBX-FR-068).
- Following symbolic links inside the sandbox when a path is resolved (design decision 9).

## Background

The repository holds no code yet; every file named here is new. Package 44 puts the runtime client interface over the OpenShell SDK, its fake for tests and the no-widening rule in `internal/sandbox/`. Package 45 adds the sandbox record (with `state`, `hub_access`, `last_activity_at`, the fixed working directory `/workspace` and the environment of the sandbox), the scrubbing function for a sandbox's secret values and the purge hooks and its routes in the same directory, and the first `parley sandbox` commands in `internal/cli/sandbox.go`. This package adds the routes that use a sandbox, in `internal/sandbox/`, and five commands to `internal/cli/sandbox.go`, built on the client of `internal/cli/` (package 09).

The source is `docs/prd/13_sandboxes.md`: SBX-FR-030 to SBX-FR-036, the routes `exec`, `files`, `logs` and `attach` of §13.5, the commands `exec`, `shell`, `logs`, `upload` and `download` of §13.6, and the edge cases of §13.10 that concern them.

## Requirements

All routes are under `/api/v1` and follow the conventions of §3 (PLAT-FR-010 to PLAT-FR-019).

### Who may use a sandbox, and when (SBX-FR-010 to SBX-FR-012, §13.3.3)

Five routes are covered, all under `/workspaces/:slug/sandboxes/:id`: `POST …/exec` (collected and streamed), `PUT …/files`, `GET …/files`, `GET …/logs` and `GET …/attach`.

Every request passes the checks in this order; the first failing one decides the answer:

1. steps 1 to 4 of the global order (path and method, authentication, rate limit, body size and content type);
2. sandboxes enabled, else `404`, `error_type` `sandboxes_disabled`;
3. the binding guard for bound tokens (package 08);
4. PAT scope: `sandboxes:read` for `GET …/files` and `GET …/logs`, `sandboxes:write` for exec, `PUT …/files` and attach; else `403 insufficient_scope`;
5. workspace access: read for the two read routes, write for the others; a workspace the caller cannot see is `404 not_found`; a read-only member on a write route is `403 forbidden`;
6. the sandbox: an id that is unknown, belongs to another workspace, or whose record was removed is `404`, `error_type` `sandbox_not_found`;
7. no widening (SBX-FR-012), for exec, attach and `PUT …/files` only: the caller's credential must cover the sandbox's hub credential, else `403 insufficient_scope`. Download and logs are not subject to it;
8. validation of the request, `400`;
9. state, `409`, `error_type` `sandbox_not_ready`:

   | Route | Allowed states |
   |---|---|
   | exec, attach, `PUT …/files` | `ready` |
   | `GET …/files` | `ready`, `completed`, `failed` |
   | `GET …/logs` | every state, `deleted` included, while the record exists |

When the runtime cannot be reached or fails while it serves exec, a file transfer or attach, the answer is the runtime-error answer of package 44: `502`, `error_type` `sandbox_runtime_error`, with the runtime's text scrubbed of the sandbox's secret values and cut to 1,024 bytes as its message (SBX-FR-064). Logs are served from the hub's own store and do not need the runtime.

### Exec, collected (SBX-FR-030, SBX-FR-032)

`POST /workspaces/:slug/sandboxes/:id/exec` with body `{"command": string[], "workdir"?: string, "env"?: map, "stdin"?: string, "stdin_base64"?: string, "timeout"?: duration}`.

The command runs in the sandbox as the workload user, in the sandbox's working directory unless `workdir` names another directory, with the sandbox's environment plus the request's `env`. `stdin`, when given, is written to the command's standard input, which is then closed. Exec is independent of the main process: it is accepted whether or not a main process runs.

The request waits for the command. The answer is `200`:

```json
{"exit_code": 0, "stdout": "…", "stderr": "…", "truncated": false, "timed_out": false, "duration_ms": 12}
```

- A non-zero exit code is not an error: the answer is `200` with that `exit_code`.
- **Timeout.** `timeout` defaults to 60 s. When the command still runs after it, the hub kills the command and answers `200` with `timed_out: true`, `exit_code: null` and the output collected until then.
- **Output cap.** At most 1 MiB (1,048,576 bytes) of each of stdout and stderr is returned. More is dropped, and `truncated` is `true` when either stream lost bytes.
- **Output that is not UTF-8.** A stream whose returned bytes are not valid UTF-8 is returned in `stdout_base64` or `stderr_base64`, and its text member is `""`. The `_base64` member is absent otherwise.
- **The caller disconnects.** The command is killed (§13.10).

Refusals:

- `400 invalid_request`: `command` absent, empty, not a list of strings, or longer than 256 entries; `env` with more than 64 entries, a value over 8 KiB, a name starting with `PARLEY_` or `OPENSHELL_`, or a name the sandbox's `exposed_secrets` lists (SBX-FR-066); both `stdin` and `stdin_base64`; `stdin_base64` that is not base64; stdin over 4 MiB (4,194,304 bytes) after decoding; `timeout` that is not a duration, is zero, or exceeds `[sandbox].max_exec_timeout` (default 30 min).
- `413 payload_too_large`: a body over 8 MiB.
- `429 rate_limited`: `[sandbox].max_concurrent_execs` (default 8) commands already run in this sandbox through exec. The count covers collected and streamed execs of one sandbox and nothing else. A command that ended for any reason no longer counts.
- `502 sandbox_runtime_error`: the runtime could not run the command.

### Exec, streamed (SBX-FR-031)

The same request with an `Accept` header that names `text/event-stream` is answered `200` with `Content-Type: text/event-stream`:

- frames `event: stdout` and `event: stderr`, each with data `{"data": string}` or, for a chunk that is not valid UTF-8, `{"data_base64": string}`; a frame carries at most 32 KiB of output;
- one final frame `event: exit` with data `{"exit_code", "timed_out", "duration_ms"}`, after which the hub closes the stream.

There is no 1 MiB cap: every byte of output is delivered. The timeout applies as for the collected form and ends the stream with `timed_out: true`, `exit_code: null`. Closing the stream kills the command. Every refusal of the collected form is answered the same way here, as a JSON error envelope and not as a stream, because it is decided before the command starts. A runtime failure after the stream began ends the stream without an `exit` frame.

### Files (SBX-FR-033)

**Upload.** `PUT /workspaces/:slug/sandboxes/:id/files?path=…[&archive=tar][&mode=…]`, body `application/octet-stream` (with `archive=tar`, `application/x-tar` is accepted too).

- Without `archive`, the body becomes the content of the file at `path`. Missing parent directories are created, an existing file is replaced, and the file gets `mode` (octal, `0000` to `0777`; default `0644`).
- With `archive=tar`, the body is a tar stream that is unpacked under the directory `path`, which is created when missing. Regular files and directories are written with the modes of their entries.
- Answer: `200 {"path", "bytes", "entries"}`: the absolute path, the file bytes written, and the number of files and directories written (1 for a single file).

**Download.** `GET /workspaces/:slug/sandboxes/:id/files?path=…[&archive=tar]`.

- A file is answered `200` with its bytes, `Content-Type: application/octet-stream` and its `Content-Length`.
- With `archive=tar`, the answer is `200` with `Content-Type: application/x-tar`: for a directory, a tar stream of its files and directories with names relative to it; for a file, a tar stream with that one entry.

**Paths.** `path` is absolute or relative to the working directory, which is `/workspace` for every sandbox (package 45). A path that resolves outside the working directory is `400`, `error_type` `path_outside_workdir`, and so is a tar entry whose name resolves outside `path`.

**Limits.** A transfer is streamed and limited to `[sandbox].max_transfer_size` (default 1 GiB), counted over the request body of an upload and over the bytes sent of a download.

Refusals:

- `400 invalid_request`: `path` absent or empty; `archive` other than `tar`; `mode` not octal or given together with `archive=tar`; a tar stream that cannot be read or that holds an entry that is neither a regular file nor a directory.
- `400`, `error_type` `path_is_directory`: a download of a directory without `archive=tar`.
- `404`, `error_type` `path_not_found`: a download of a path that does not exist, or of a sandbox whose working directory the runtime no longer has.
- `413 payload_too_large`: an upload whose declared length exceeds the limit (nothing is written); an upload without a declared length that passes it (the hub stops reading and writing; what was written stays); a download of a file larger than the limit (no byte is sent). A tar download that passes the limit is cut off: the hub aborts the connection, so the caller sees an incomplete transfer.
- `415 unsupported_media_type`: an upload with another content type.
- `502 sandbox_runtime_error`.

### Logs (SBX-FR-034, SBX-FR-068)

**What is stored.** While a sandbox is `ready`, the hub reads from the runtime the output of the main process and the runtime's policy log for the sandbox, and stores each line:

| Member | Meaning |
|---|---|
| `time` | when the runtime produced the line |
| `source` | `process` or `policy` |
| `stream` | `stdout` or `stderr` for a process line; `null` for a policy line |
| `line` | the text, without its line end |
| `action`, `program`, `host`, `port` | policy lines only: `allow` or `deny` and what was decided on |
| `credential` | policy lines of credentialed requests only: the provider name |

- Before a line is stored, each exact match of a value of the sandbox's exposed secrets is replaced by `[redacted]`. A value shorter than 8 characters is not matched (SBX-FR-068).
- A line longer than 16 KiB is stored as consecutive lines of at most 16 KiB.
- When the hub restarts, or the runtime's stream breaks, collection resumes within 5 seconds after the last stored line; no line is stored twice.
- Collection stops when the sandbox leaves `ready`. Stored lines stay readable.
- The hourly maintenance run removes lines older than `[sandbox].log_retention` (default 7 days) and the lines of sandboxes whose record no longer exists. The lines of a sandbox also go in the transaction that removes its record, through a purge hook.

**Reading.** `GET /workspaces/:slug/sandboxes/:id/logs` answers `200 {"items": […], "next_cursor"}` with the stored lines oldest first, paged by `limit` and `cursor` (PLAT-FR-014). It works in every state of the sandbox and while the runtime is unreachable.

| Query | Meaning |
|---|---|
| `source` | `process` or `policy`; default both |
| `since` | a timestamp; only lines at or after it |
| `tail` | a positive integer; only the last that many matching lines |
| `follow` | `true`: the answer is an event stream |

**Following.** With `follow=true` the answer is `200` with `Content-Type: text/event-stream`: one frame `event: log` per line, its data the line's JSON object; first the stored lines the other parameters select, then each new line as it is stored. The stream ends when the caller closes it, when the hub stops, or when the sandbox becomes `deleted`.

Refusals, all `400 invalid_request`: an unknown `source`; a `since` that is not a timestamp; a `tail` below 1 or not an integer; a `follow` that is not `true` or `false`; `follow=true` together with `cursor` or `limit`.

### Attach (SBX-FR-035)

`GET /workspaces/:slug/sandboxes/:id/attach?cols=…&rows=…` is a WebSocket upgrade. The hub starts a new shell (`/bin/sh`) in the sandbox as the workload user, in the working directory, with the sandbox's environment, on a terminal of `cols` by `rows` (default 80 by 24; each 1 to 1,000).

- Binary frames carry terminal bytes in both directions.
- A text frame `{"resize": {"cols", "rows"}}` from the caller resizes the terminal. Any other text frame is ignored.
- When the caller closes the connection, the hub ends that shell and nothing else: the main process, running execs and other terminals go on.
- When the shell ends, or the runtime's terminal fails, the hub closes the connection.

Every refusal is a plain HTTP error envelope before the upgrade, and no shell is started: the checks above, `400 invalid_request` for a request that is not a WebSocket upgrade or has `cols` or `rows` outside 1 to 1,000, and `502 sandbox_runtime_error` when the runtime cannot open the terminal.

### Activity (SBX-FR-036)

`last_activity_at` of a sandbox is set to the current time:

- when an exec starts and when it ends, in both forms;
- when an attach opens, when it closes, and while it is open whenever a frame arrives from the caller (stored at most once a minute);
- when a file transfer starts and when it ends;
- when a line of main-process output is stored.

It never moves backwards. A running main process that produces no output, a policy log line, a log read and a refused request do not set it.

**Operation observers.** When an exec, an attach or a file transfer that passed every check ends, the hub tells each registered observer once: the kind (`exec`, `attach`, `upload`, `download`), the sandbox id, its workspace and session, the caller (`actor_type`, `actor_id`), the start time and `duration_ms`; for an exec also the `command`, `exit_code`, `timed_out` and the result (`ok`, `nonzero`, `timeout`, `error`); for a transfer the `path` and `bytes`. Never stdin, output, terminal content, file content or environment values. An observer that fails changes nothing for the caller. The recorder of package 50 is registered as an observer; this package registers none. When an exec ends, the hub also adds one to `parley_sandbox_exec_total{result}`.

### CLI (§13.6)

All five commands follow the CLI conventions (PLAT-FR-090 to PLAT-FR-095). None of them uses the 30-second request timeout: each request lasts until the hub ends it.

**`parley sandbox exec <workspace-slug> <id> -- <command...>`.** Flags: `--workdir`, `--env key=value` (repeatable), `--timeout`, `--stdin` (read standard input to its end and send it), `--json`.

- By default it calls `POST …/exec` as a stream and writes the command's stdout to stdout and its stderr to stderr, byte for byte. Stdout is not JSON.
- With `--json` it calls the collected form and prints the response document.
- Exit status: the command's exit code when it ran, with values above 125 mapped to 125; 1 for a hub error or a timeout (reported on stderr as `wait_timeout`); 2 for a usage or client error, which includes no command after `--`, an `--env` value without `=`, standard input over 4 MiB, and a stream that ends without an `exit` frame.

**`parley sandbox shell <workspace-slug> <id>`.** Opens `GET …/attach` with the size of the local terminal (80 by 24 when standard input is not a terminal), puts the local terminal in raw mode, copies bytes both ways, sends a resize frame when the local terminal changes size, and restores the terminal when the connection ends. Exit status 0 when the hub closes the connection; 1 for a hub error before the upgrade; 2 for a connection failure.

**`parley sandbox logs <workspace-slug> <id>`.** Flags: `--source process|policy`, `--since`, `--tail <n>`, `--follow`. Output: one JSON object per line. Without `--follow` it reads every page. With `--follow` it prints lines as they arrive until the hub ends the stream, then exits 0.

**`parley sandbox upload <workspace-slug> <id> <local-path> <sandbox-path>`.** A local file is sent as the body with its permission bits as `mode`; a local directory is sent as a tar stream with `archive=tar` (regular files and directories; a symbolic link is skipped with a warning on stderr). Output: the response document. A local path that cannot be read is a `client_error` (exit status 2) and no request is sent.

**`parley sandbox download <workspace-slug> <id> <sandbox-path> <local-path>`.** Calls `GET …/files`; when the hub answers `path_is_directory` it asks again with `archive=tar` and unpacks the stream into the directory `<local-path>`. A file is written to `<local-path>`, or to stdout when `<local-path>` is `-`. Output otherwise: `{"path": "<local-path>", "bytes": <n>}`. A tar entry that would be written outside `<local-path>` is a `client_error` (exit status 2).

### End-to-end flows

1. An agent with a `ready` sandbox on its fork uploads a script with `PUT …/files`, runs it with `POST …/exec`, and downloads the file the script wrote with `GET …/files`. `parleyd` checks the caller, passes each operation to the OpenShell gateway, and answers with the exit code, the output and the file. The sandbox's `last_activity_at` has moved.
2. A developer runs `parley sandbox exec <fork> <id> -- sh -c 'echo out; echo err >&2; exit 3'`. The CLI asks for a stream; `parleyd` relays the frames from the OpenShell gateway; the CLI prints `out` on stdout and `err` on stderr and exits 3.
3. A sandbox's main process prints a line that contains an exposed secret's value. `parleyd` reads it from the OpenShell gateway, stores it redacted, and a developer running `parley sandbox logs <fork> <id>` gets it as one JSON line with `[redacted]` in place of the value.
4. A developer opens `GET …/attach` over WebSocket, types `echo hi` and reads `hi` back from the shell the OpenShell gateway started; closing the connection ends that shell, and the sandbox stays `ready`.

## Design Decisions

1. **Project decisions relied on.** Go; `net/http` with its method-and-pattern router; the CLI on `github.com/spf13/cobra`; the sandbox runtime client is `github.com/ai-openshell/sdk-go`, reached only through the runtime client interface of package 44; the store is SQLite behind `internal/store`; tests use `testing`, `net/http/httptest` and the fake runtime of package 44; code takes a replaceable clock.
2. **Runtime operations.** This package calls the runtime client of package 44 by that package's names and adds nothing to the interface, its OpenShell implementation or its fake: `ExecStream` (both forms of exec), `Upload` and `MakeDir`, `Stat`, `Download` and `Walk`, `Logs` with the sources `process` and `policy`, and `ExecInteractive`. A result of kind `unsupported` is answered like an unreachable runtime, `502 sandbox_runtime_error`; a `not found` for a path is `404 path_not_found`.
3. **WebSocket library.** `golang.org/x/net/websocket` for the hub and the CLI. Why: the standard library has none, this module is in the module cache and its signatures were read; the route needs only binary and text frames. `golang.org/x/term` gives the CLI raw mode and the terminal size.
4. **Where the no-widening check sits.** After the sandbox is found and before validation and state. Why: the rule needs the sandbox's `hub_access`, and an authorization answer must not depend on the body.
5. **One state error.** Every state that does not allow the operation, `deleted` included, is `409 sandbox_not_ready`. Why: the routes of §13.5 list no other state error.
6. **The `429` of the exec limit** is the generic `rate_limited` without a `Retry-After` header. Why: the source PRD gives the status only, and the limit is a quantity, not a rate. Attach does not count toward it. The count is held in memory: a restart of the hub ends every exec request anyway (SBX-FR-060).
7. **Exec validation.** `command` and `env` follow the rules of the create request (256 entries; 64 entries of at most 8 KiB; reserved and exposed names refused). The request's value wins over the sandbox's for the same name. A `timeout` above the maximum is refused, not lowered. `exit_code` is `null` only when the command timed out. `workdir` is not confined to the working directory: the runtime's filesystem policy confines the command. The exec body limit is 8 MiB, so that 4 MiB of stdin fits as base64.
8. **Truncated text.** A truncated stream is cut back to the last complete UTF-8 character at or below 1 MiB before its validity is judged, so that a cut inside a character does not turn text into base64.
9. **Path resolution is lexical.** The hub cleans `.` and `..` and compares with the working directory. It does not follow symbolic links inside the sandbox; what a path reaches through a link is confined by the runtime. For that reason tar entries that are links or devices are refused on upload.
10. **A failed upload is not rolled back.** What was written before a refused tar entry or before the size limit stays in the sandbox. Why: the transfer is streamed.
11. **`path_is_directory`** (`400`) is a new specific error type. Why: `parley sandbox download` takes no flag that says whether the path is a directory, so the hub must say it in a way a client can act on. `archive=tar` on a file gives a one-entry tar instead of an error.
12. **Runtime errors use the answer of package 44.** That package owns `502 sandbox_runtime_error`; this package returns runtime failures through it and writes no second answer. The message is the runtime's text after the scrubbing function of package 45 replaced the sandbox's resolved secret values and its hub credential and cut it to 1,024 bytes (SBX-FR-064), as package 47 does for its routes.
13. **Log collection finds its sandboxes by looking.** Every 5 seconds the collector reads the sandboxes in state `ready` from the store, starts a follower for each that has none and stops the followers of the others. Why: it needs no hook in packages 45 and 47, and it resumes after a restart by the same rule. A follower asks the runtime for output from the time of the last stored line and skips the lines of that second it already has.
14. **Log line shape.** `time` has whole seconds (PLAT-FR-016); order is the order of storage. `stream` is `null` for policy lines. `tail` selects the last n matching lines and the result is still paged. Lines are capped at 16 KiB.
15. **Follow frames** are named `log`. The followed stream of a sandbox that is not `ready` stays open, because a start may produce new lines; it ends at `deleted`.
16. **Logs go with their record through a purge hook, and by retention.** This package registers a purge hook with the list package 45 declares; package 47, which removes records after 24 hours and with their workspace, runs it in the removing transaction. The maintenance task registered in the registry of package 03 removes lines by age and, as a backstop, lines whose sandbox record is gone. This package does not depend on package 47.
17. **Attach.** The shell is `/bin/sh`. A request that is not a WebSocket upgrade is `400 invalid_request`. A malformed text frame is ignored. Authentication is the `Authorization` header, as on every route.
18. **Activity during long operations.** Exec and transfers set `last_activity_at` at start and at end; attach also on input, at most once a minute. Why: SBX-FR-036 names the operations, not the moments, and an open terminal in use must not be stopped as idle.
19. **Operation observers and the exec metric.** An in-process registry in `internal/sandbox` with a registration function. Why: AUD-FR-026 belongs to package 50, which does not depend on this package; `internal/hub` registers that package's sandbox recorder as an observer, mapping the record's members one to one. The metric `parley_sandbox_exec_total{result}` is reported by this package itself through the metrics recorder interface of package 44, which package 51's recorder fills.
20. **CLI choices.** `--stdin` sends `stdin_base64`. A timed-out command is reported as `wait_timeout` (the CLI's existing type for a passed timeout) with exit status 1. A stream that ends without an `exit` frame is a `client_error`. `parley sandbox logs` without `--follow` reads all pages. `parley sandbox download` writes a file through a temporary file beside the target and prints `{"path", "bytes"}`; `-` sends the bytes to stdout.
21. **Profile: full.**

## Dependencies

- Package 45 sandbox_creation_and_credentials (draft): the sandbox record this package reads (`state`, `hub_access`, `exposed_secrets`, the working directory, the environment, the session) and writes (`last_activity_at`), and the `parley sandbox` command group.
- Package 09 cli_foundation (draft): the HTTP client, output and exit codes the five commands are built on.
- Through package 45: package 44 sandbox_runtime_and_profiles (the runtime client interface, its fake, the `[sandbox]` values, the check order and the no-widening rule), package 08 (binding guard), package 07 (workspace access), package 03 (maintenance task registry, store) and package 02 (conventions).
- External: an OpenShell gateway, reached through package 44's client.

## Verified External API

- `golang.org/x/net/websocket` v0.61.0, read in the module cache: `type Handler func(*Conn)`; `type Server struct { Config; Handshake func(*Config, *http.Request) error; Handler }` with `func (s Server) ServeHTTP(w http.ResponseWriter, req *http.Request)`; `func NewConfig(server, origin string) (config *Config, err error)`; `Config.Header http.Header`; `func (config *Config) DialContext(ctx context.Context) (*Conn, error)`; `type Codec struct { Marshal func(v interface{}) (data []byte, payloadType byte, err error); Unmarshal func(data []byte, payloadType byte, v interface{}) (err error) }` with `Send` and `Receive`; `var Message Codec` (a `string` is a text frame, a `[]byte` a binary frame); constants `TextFrame = 1`, `BinaryFrame = 2`. `Handler.ServeHTTP` refuses a request without an `Origin` header; `Server` with a `Handshake` that does not check the origin accepts it.
- `golang.org/x/term` v0.45.0, read in the module cache: `func IsTerminal(fd int) bool`; `func MakeRaw(fd int) (*State, error)`; `func Restore(fd int, oldState *State) error`; `func GetSize(fd int) (width, height int, err error)`.
- `github.com/ai-openshell/sdk-go` (package `openshell/v1`): **unverified**. It is not in the module cache. This package does not call it; the operations of design decision 2 are package 44's.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task.
- **Sandbox**: An isolated environment in which an agent runs, attached to a workspace (normally a task fork) and started with it checked out. It runs on OpenShell; the hub controls it (§13).
- **OpenShell**: The open-source sandbox runtime Parley's sandboxes run on. It provides the isolation, the policy format and the credential mechanism. The hub is a client of one OpenShell gateway.
- **Profile**: The operator's template for a sandbox, defined in configuration: image, resources, time limits, base policy, credentials and the secrets it exposes.
- **Exposed secret**: A secret (§11) that a profile names in `expose_secrets`. Its real value is placed in the sandbox's environment, where code in the sandbox can read it. Off unless the operator lists the secret (§13).
- **Hub credential**: The fork-bound PAT the hub mints for a sandbox so that it can reach the hub. The workload sees only its placeholder, in `PARLEY_API_KEY`.
- **Bound token**: A PAT confined to one workspace (and the forks created with it).
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
- **Session**: An agent session: who ran, on which workspace, with what token usage, and which changes it produced.
- **Agent telemetry**: Run events, tool calls, traces, outcomes and postmortems that agents send to the hub.
- **Main process**: The `command` a sandbox was created with. A sandbox without one is idle and driven by exec.
- **Working directory**: The directory in the sandbox that holds the checkout and that the workload may write.
- **Workload user**: The user the runtime runs the sandbox's processes as.
