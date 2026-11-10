---
spec_id: "09"
spec_name: "cli_foundation"
title: "Give every CLI command one behaviour"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "73e6489927d5dd0035830e3f6cb1d69cb867caac8b3294dfd81087b0727fb33f"
schema_version: 3
source: "docs/prd/03_platform.md"
---

# Give every CLI command one behaviour

## Intent

The `parley` client has close to two hundred commands, and the people and agents who script it must be able to rely on all of them behaving alike. This package fixes that behaviour once: where a command finds the hub URL and the credential, how it sends a request, what it writes to stdout and to stderr, which exit status it ends with, how it waits for a long operation, and how it describes itself in help. Every command of every later package is built on this foundation and states only what differs.

## Goals

- A command takes the hub URL and the credential from the first non-empty value of flag, environment variable and configuration file, and fails with exit status 2 before any request when one is missing.
- The client configuration file is found at `$PARLEY_CONFIG_DIR/config.toml` or `~/.parley/config.toml`, is never created by reading, and is replaced atomically with mode 0600 and its unknown keys kept when it is saved.
- One HTTP client sends every request to `<hub URL>/api/v1<path>` with `Authorization: Bearer <credential>`, a 30-second timeout and every user-supplied path segment percent-encoded as one segment.
- stdout carries exactly one JSON document indented by 2 spaces, or nothing; every error is the error envelope on stderr; the exit status is 0, 1, 2 or 3 by one table.
- `--wait`, `--timeout` and `--poll-interval` behave the same on every command that has them.
- `parley`, a command group without a subcommand, `parley help` and `--help` print help with exit status 0 and without a credential; with `--json` the help is machine-readable.

## Non-goals

- `parley login`, its browser flow and its key flow: package 04 `authentication_and_login`. This package delivers the save of the configuration file that `login` calls and the client it sends its requests with.
- The server side of the error envelope and the `error_type` catalogue (PLAT-FR-011, PLAT-FR-012): package 02 `http_api_conventions`. This package prints what the hub answers.
- The root command, bare `parley` and `parley version` with the flag `--endpoint-url` and the variable `PARLEY_ENDPOINT_URL`: package 01 `hub_skeleton`. This package adds the configuration file as the third source of the hub URL for `parley version`.
- Every command group and its flags, calls and confirmation documents: the package that owns the route (`parley user`, `parley orgs`, `parley admin users`, `parley admin orgs`: package 05 `users_and_organizations`; `parley keys`, `parley tokens`: package 06 `api_keys_and_access_tokens`; `parley workspace`: packages 10 to 14; and so on through package 53).
- The terminal states a `--wait` ends on, and the condition of each `--fail-on-…` flag: the package that owns the command (for example package 10 `workspace_create_and_clone`, package 12 `workspace_sync_from_origin`, package 33 `rebuild_jobs`). This package delivers the shared waiting and the shared exit with status 3.
- The commands that pass a sandbox's own output or files through instead of one JSON document (`parley sandbox exec`, `shell`, `logs`, `download`): package 46 `sandbox_exec_files_and_logs`.
- `parley credential-helper`, which reads the same settings: package 17 `git_smart_http`.
- `parley events tail`, which streams: package 52 `durable_event_log_and_stream`.

## Background

The repository holds no code yet; every file named here is new, except the files package 01 creates and this package extends: `cmd/parley/main.go`, `internal/cli/root.go`, `internal/cli/root_test.go`, `internal/cli/version.go` and `internal/cli/version_test.go`. Package 01 `hub_skeleton` gives the client a root cobra command with the persistent flag `--endpoint-url`, the function `cli.Run(ctx, args, stdout, stderr)` that returns the exit status, and `parley version`. Package 04 `authentication_and_login` adds `parley login` in `internal/cli/login.go` and a first read and save of the configuration file in `internal/cli/config.go` (with `config_test.go`); this package extends those two files too and becomes the single implementation of the file, the client and the exit codes.

The source PRD (`docs/prd/03_platform.md`) states the client's conventions once: §3.3.7 (the configuration file), PLAT-FR-090 to PLAT-FR-095 (settings, requests, output, exit codes, waiting, help), §3.6 (global flags, `parley help`), the client's rows in the environment table of §3.9, and the sentence of §3.10 that the CLI does not retry on `429`. Every other section's CLI entry says only "Calls", "Flags" and "Output" and relies on these rules: "Unless an entry says otherwise, a command prints the route's response body and exits 0, 1 or 2 by those rules" (§3.6).

This package turns those rules into shared code in `internal/cli/` that later packages call: the settings and the configuration file (`internal/cli/config.go`), the HTTP client (`internal/cli/client.go`), output and errors (`internal/cli/output.go`, `internal/cli/errors.go`), waiting (`internal/cli/wait.go`), the declaration of a command with its route and help (`internal/cli/command.go`, `internal/cli/help.go`). Tests live beside the code (`internal/cli/client_test.go`); the end-to-end test is `test/e2e/cli_test.go`.

The client is a single static binary with no dependency on the server's stacks (NFR-004): the non-test code of `internal/cli` imports nothing from `internal/server` or any other server package.

## Requirements

### Settings (PLAT-FR-090)

Every command except `login`, `version` and `help` needs a hub URL and a credential. Each takes the first non-empty value of flag, environment variable, configuration file:

| Setting | Flag | Environment | File key |
|---|---|---|---|
| Hub URL | `--endpoint-url` | `PARLEY_ENDPOINT_URL` | `endpoint_url` |
| Credential | `--api-key` | `PARLEY_API_KEY` | `api_key` |

`--endpoint-url`, `--api-key` and `--json` are global flags, accepted on every command (§3.6). An empty flag value or an empty variable counts as absent, and the next source is used.

A missing value fails before any request: exit status 2, nothing on stdout, and on stderr a `client_error` whose message names, for each missing setting, its flag, its variable and the configuration file's path.

A hub URL that is not an absolute `http` or `https` URL with a host fails the same way, with a `client_error` that names the value.

`parley version` needs no credential. It now takes its hub URL from the configuration file too, as the third source. Once its arguments are accepted its exit status is always 0: a configuration file it cannot read gives a warning on stderr and no hub URL from the file. Like every command, it answers an extra argument or an unknown flag with a usage error and exit status 2; package 01 agrees with both.

### The client configuration file (§3.3.7, §3.9)

- **Location.** `$PARLEY_CONFIG_DIR/config.toml` when `PARLEY_CONFIG_DIR` is set and non-empty, otherwise `~/.parley/config.toml`.
- **Keys.** `endpoint_url` (the hub's base URL), `api_key` (the credential: an API key or a PAT), `user_id` (the logged-in user's id; informational, never sent).
- **Reading.** A file that does not exist is not an error: the settings then come from flags and environment alone. Reading never creates the directory or the file; only `parley login` creates them.
- **An unparseable file is a client error:** exit status 2, a `client_error` on stderr naming the file's path, nothing on stdout, no request. A file that cannot be read (for example no permission), is not valid TOML, or holds a known key that is not a string counts as unparseable.
- **Saving.** `parley login` saves through this package. The directory is created with mode 0700 when missing; the file is replaced atomically (a reader sees the old content or the new, never a part) and has mode 0600 afterwards, whatever its mode was before. Keys the CLI does not know are kept with their values. If the save fails (the directory cannot be written, or the existing file cannot be parsed), the previous file is left as it was and no temporary file remains.

### Requests (PLAT-FR-091, §3.10)

Requests go to `<hub URL without trailing slash>/api/v1<path>` with `Authorization: Bearer <credential>`. A request with a body sends it as JSON with `Content-Type: application/json`; a request without a body sends no `Content-Type`.

Every user-supplied path segment is percent-encoded as one segment, so a `/` inside an argument becomes `%2F`.

Each request times out after 30 seconds unless a command states otherwise (for example `parley workspace sync` states 3 minutes). Failures of the request itself are client errors, exit status 2 with `client_error` on stderr and nothing on stdout:

- the connection fails (refused, unknown host, TLS failure);
- no complete response arrives within the timeout;
- the response cannot be decoded: a success whose body is not JSON, or an error status whose body is not the error envelope.

The CLI sends each request once. It does not retry, not on `429` and not on any other answer (§3.10).

### Output (PLAT-FR-092)

- **stdout** carries the result and nothing else: one JSON document, indented by 2 spaces, followed by a newline. A command whose route answers `204` prints `{}`, unless its entry states a confirmation document (for example `{"status":"deleted","slug":"<slug>"}`), which is then printed in the same form.
- **stderr** carries errors, warnings, hints and progress.
- An error is printed on stderr as the error envelope (PLAT-FR-011). A hub error is printed as received, including `error_type` and any extra members (`details`, `oldest_seq`); stdout stays empty and the exit status is 1.
- An error raised by the CLI itself uses `code: 0` and one of these types:

  | `error_type` | Meaning | Exit |
  |---|---|---|
  | `usage_error` | unknown command or flag, wrong argument count, invalid flag value | 2 |
  | `client_error` | missing settings, unreadable config, connection failure, request timeout, undecodable response | 2 |
  | `wait_failed` | a `--wait` ended in an unsuccessful terminal state | 1 |
  | `wait_timeout` | a `--wait` passed its `--timeout` | 1 |
  | `condition_not_met` | a `--fail-on-…` condition held | 3 |

- A command that prints a secret shown once (a token or key) also warns on stderr that it will not be shown again.

### Exit codes (PLAT-FR-093)

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | the hub answered an error; or a `--wait` ended unsuccessfully or timed out |
| 2 | usage or client error: bad flags or arguments, no credentials, connection failure, undecodable response |
| 3 | a requested condition was not met (`--fail-on-…` flags) |

A usage error (an unknown command, an unknown flag, a wrong number of arguments, a flag value that does not parse) sends no request and prints nothing on stdout. An error that is none of the kinds in the tables is reported as `client_error` and exits 2.

A command with a `--fail-on-…` flag prints its result on stdout first; when the flag is given and its condition holds, it then reports `condition_not_met` on stderr and exits 3. Without the flag the same result exits 0.

### Waiting (PLAT-FR-094)

Commands with `--wait` also take `--timeout` (default `5m`) and `--poll-interval` (default `5s`). Both MUST be positive, or the command fails with a usage error (exit status 2) before it starts the operation.

- The CLI starts the operation, then polls its status under one overall deadline.
- Only terminal states end the wait. Each command names the terminal states of its resource and which of them are successful; a state that means "waiting to retry" is never terminal.
- stdout gets exactly one document: the final record. Progress goes to stderr.
- A successful terminal state exits 0. An unsuccessful one prints the final record, reports `wait_failed` on stderr and exits 1.
- On timeout, nothing is printed on stdout; `wait_timeout` is reported and the exit status is 1. The operation itself keeps running on the hub: the CLI sends no request to cancel it.
- A hub error during polling ends the wait with that error and exit status 1.
- Without `--wait`, the command prints the answer of the request that starts the operation and does not poll.

### Help (PLAT-FR-095, §3.6)

`parley` alone, and a command group without a subcommand, print help on stdout and exit 0 without needing credentials, without reading the configuration file and without a request.

`parley help [command]` shows help; `parley <command> --help` is equivalent. No credential is needed. The output is text by default. With `--json`, it is a machine-readable description of the command, or of every command under a group: `name`, `description`, `method`, `path`, `args`, `flags` (`name`, `type`, `required`, `default`, `description`) and `auth` (`none`, `user` or `admin`).

`parley help` followed by words that name no command is a usage error (exit status 2).

### End-to-end flows

1. A developer has logged in, so the configuration file holds `endpoint_url` and `api_key`. The developer runs `parley version` with no flag and no variable; the CLI reads the hub URL from the file and calls `GET /version`; `parleyd` answers; the CLI prints the client's and the hub's build as one JSON document on stdout and exits 0.
2. A developer runs `parley login --api-key <a credential the hub does not know> --endpoint-url <hub>`; the CLI sends its request under `/api/v1` with `Authorization: Bearer <credential>`; `parleyd` refuses with the error envelope; the CLI prints the envelope on stderr as received, prints nothing on stdout, exits 1 and leaves no configuration file behind.

## Design Decisions

1. **Libraries.** The CLI uses `github.com/spf13/cobra` (project decision 4) and the standard library `net/http` client. Tests use the standard `testing` package with table-driven tests and `net/http/httptest` (project decision 7).
2. **TOML library.** The configuration file is read and written with `github.com/BurntSushi/toml` v1.6.0, the module package 03 `server_lifecycle_and_configuration` adds to `go.mod` for the server's `config.toml` and package 04 already uses for `parley login`. No second TOML module is added.
3. **The file is read only when it is needed.** A command reads the configuration file only when a flag and the environment leave one of the settings it needs without a value. Why: an agent that passes both settings in its environment must not fail on a file it never uses. `parley help` and `--help` never read it.
4. **What "unparseable" covers.** Invalid TOML, a file that exists and cannot be read, and a known key (`endpoint_url`, `api_key`, `user_id`) whose value is not a string. Why: each leaves the CLI unable to say what the setting is.
5. **What a save keeps.** Unknown keys and their values, tables included. Comments and the order of keys are not kept. Why: the source PRD promises the keys, and keeping them needs no more than decoding into a map and encoding it again.
6. **No home directory.** When `PARLEY_CONFIG_DIR` is unset or empty and the home directory cannot be determined, reading treats the file as absent and a save fails with a `client_error`.
7. **Hub URL form.** An absolute URL with scheme `http` or `https` and a host. Every trailing slash is removed. A path in it is kept (`https://host/parley` sends to `https://host/parley/api/v1…`). Values are compared with the empty string as given; white space is not trimmed.
8. **The message of a missing setting** names, for each missing setting, its flag, its variable and the path of the configuration file as resolved (decision 6: `~/.parley/config.toml` when there is no home directory).
9. **How stdout is formed.** The hub's body is indented again without being decoded into values and encoded back, so the order of members and the text of numbers stay as the hub sent them.
10. **How stderr is formed.** An error is one JSON document, indented by 2 spaces and followed by a newline: a hub envelope as received, with member order and extra members kept; a CLI error as `{"error":{"code":0,"message":"…","error_type":"…"}}`. Warnings, hints and progress are plain text lines; a warning begins with `warning: `.
11. **Any success without a body prints `{}`** (or the command's confirmation document), not only status `204`. Why: the caller of a script needs one document on stdout either way.
12. **An error status without an envelope is a client error** (exit status 2), and its message names the HTTP status. Why: the body did not come from the hub's REST API (a proxy or a wrong URL), so "the hub answered an error" would be false. The same holds for a success whose body is not JSON. A command whose entry states an output other than one JSON document (raw file content, a sandbox's own output) reads the raw response through the same client and is not subject to this rule.
13. **Per-command timeout.** A command that states another request timeout passes it with the request; the default is 30 seconds. The value is held where tests can shorten it.
14. **The show-once warning** is the line `warning: this value is shown once and will not be shown again`. A command declares that its output carries such a secret; the document on stdout is not changed.
15. **`--fail-on-…`.** The shared part is: the result is printed first, then `condition_not_met` on stderr with the command's message, exit status 3. This order is the one `parley workspace sync --fail-on-diverged` states (§4).
16. **Form of `--timeout` and `--poll-interval`.** A whole number followed by exactly one of `s`, `m`, `h`, `d` (PLAT-FR-016; `d` is 24 hours). Zero, a negative number, a fraction, a missing unit and a compound form (`1m30s`) are usage errors. The parser lives in `internal/cli`, because the client must not import server code (the server's own parser is `internal/server/timefmt` of package 02).
17. **The two flags are checked whether or not `--wait` is given.** Without `--wait` they have no other effect.
18. **How a wait proceeds.** The record the start request returns is examined first: if it is already terminal the wait ends with no poll. Otherwise the first poll is sent one poll interval after the start request returned, and each later poll one interval after the previous answer. The deadline counts from the moment the start request returned. A poll still in flight at the deadline is abandoned and the wait times out. Each poll is an ordinary request (30-second timeout, sent once).
19. **Progress** is one line on stderr, `waiting: <state>`, when the wait begins and each time the observed state differs from the one before.
20. **A client error during polling** (connection failure, timeout of one poll, undecodable answer) ends the wait with `client_error` and exit status 2; nothing is printed on stdout.
21. **A command that states its own final document** for a wait (for example `parley workspace sync --wait`, which prints its answer with the final job added) receives the final record from the shared wait and prints its own document; the rules for stdout, `wait_failed` and `wait_timeout` do not change.
22. **How a command declares itself.** Each command is declared through one helper of `internal/cli/command.go` with its description, its HTTP method and path (relative to `/api/v1`, with `:param` as the source PRD writes them), its named arguments and its `auth` value. `auth` `user` and `admin` mean the command needs a hub URL and a credential before it runs; `none` means it needs neither (`login`, `version`, `help`). A command that calls no single route (`help`, `login`) has `method` and `path` `null`; `parley version` has `GET` and `/version`.
23. **Shape of JSON help.** For one command, stdout is one object `{name, description, method, path, args, flags, auth}`. `name` is the command's words after `parley` joined by single spaces (`tokens create`). `args` is an array of `{name, required}` in order. `flags` is an array sorted by `name` of `{name, type, required, default, description}`, where `name` has no leading dashes, `type` is the type name the flag library reports for the value (`string`, `bool`, `int`, `duration`), and `default` is the default as a string. The three global flags are part of every command's `flags`. For a group, and for `parley help --json` with no command, stdout is one object `{"commands":[…]}` holding the object of every command under it that is not a group, at any depth, sorted by `name`.
24. **`--json` without `help` or `--help` has no effect.** It is accepted on every command because it is a global flag.
25. **`parley version` and an unreadable configuration file.** The command writes one warning line naming the file and goes on without a hub URL from the file. Why: its exit status is always 0 (§3.6). This holds for every state of the hub and of the file; a usage error (an extra argument, an unknown flag) is exit status 2 for `parley version` as for every command. Package 01 `hub_skeleton` states the same: exit 0 whatever the hub answers, warnings as lines that begin with `warning: `, and usage errors left to this package.
26. **`parley login` moves onto this foundation, and this package is the single implementation.** Package 04 `authentication_and_login` is implemented before this one, so it creates `internal/cli/config.go` with a read and a save of its own, keeps an error type of its own in `internal/cli/login.go` and sends its own requests. Agreed with package 04: this package extends `internal/cli/config.go` into the one read and save, replaces login's error type, request code and printing with the shared client, printers and exit-code mapping, and deletes what it replaces, so that `login` obeys the same rules as every other command and no second copy remains; its flows, its flags and the behaviour package 04 specifies do not change. Packages 05, 06 and 08 depend on this package and build their commands on it.
27. **An error of no named kind** (a failure inside the CLI that is neither a hub error nor one of the five CLI error types) is reported as `client_error` with exit status 2. Why: the exit status must be one of 0, 1, 2 and 3, and library code returns an error instead of ending the process.
28. **A usage error prints the envelope and no usage text.** stderr then holds exactly one JSON document, so a caller can parse it whole; the text is one `parley help <command>` away.
29. **A save over a file that cannot be parsed fails** and leaves that file as it is, because its unknown keys could not be kept.
30. **Profile: full**, with two execution paths.

## Dependencies

- Package 02 `http_api_conventions` (draft): the error envelope and the `error_type` catalogue the hub answers with, which the CLI prints as received and mirrors for its own errors; the `404` answer for an unknown path.
- Package 04 `authentication_and_login` (draft): the credentials the client sends as `Authorization: Bearer`, the hub's answer to a credential it does not know, and `parley login`, the one command that creates the configuration file this package reads and saves.
- Package 01 `hub_skeleton` (draft), reached through 02: the root command, `cli.Run` and `parley version`, which this package extends.

## Verified External API

`github.com/spf13/cobra` v1.10.2, read in the module cache (`command.go`, `args.go`):

- `func (c *Command) SetFlagErrorFunc(f func(*Command, error) error)`
- `func (c *Command) SetHelpFunc(f func(*Command, []string))`
- `func (c *Command) SetHelpCommand(cmd *Command)`
- `func (c *Command) Find(args []string) (*Command, []string, error)`
- `func (c *Command) Commands() []*Command`
- `func (c *Command) CommandPath() string`
- `func (c *Command) Runnable() bool`
- `func (c *Command) HasSubCommands() bool`
- `func (c *Command) Flags() *flag.FlagSet`, `func (c *Command) InheritedFlags() *flag.FlagSet`, `func (c *Command) PersistentFlags() *flag.FlagSet` (`flag` is `github.com/spf13/pflag`)
- fields of `Command`: `Args PositionalArgs`, `Annotations map[string]string`, `RunE func(cmd *Command, args []string) error`, `SilenceErrors bool`, `SilenceUsage bool`
- `type PositionalArgs func(cmd *Command, args []string) error`; `func ExactArgs(n int) PositionalArgs`; `func RangeArgs(min int, max int) PositionalArgs`; `func NoArgs(cmd *Command, args []string) error`

`github.com/spf13/pflag` v1.0.10 (`flag.go`, `bool.go`):

- `func (f *FlagSet) VisitAll(fn func(*Flag))`
- `func (f *FlagSet) Changed(name string) bool`
- `func (f *FlagSet) BoolVar(p *bool, name string, value bool, usage string)`

`github.com/BurntSushi/toml` v1.6.0 (`decode.go`, `encode.go`):

- `func Unmarshal(data []byte, v any) error`
- `func Marshal(v any) ([]byte, error)`

## Glossary

- **Hub**: The `parleyd` server.
- **Error envelope**: The JSON body of every REST API error, `{"error":{"code","message","error_type"}}`.
- **Hub error**: An error envelope the hub answered; the CLI prints it as received and exits 1.
- **CLI error**: An error the CLI raises itself, printed as an envelope with `code` 0 and one of the types `usage_error`, `client_error`, `wait_failed`, `wait_timeout`, `condition_not_met`.
- **Settings**: The hub URL and the credential a command needs.
- **Credential**: What the client presents as `Authorization: Bearer`: an API key, a PAT or the admin token.
- **Terminal state**: A state of a resource that ends a `--wait`; each command names its own, and which of them are successful.
- **Command group**: A command that only holds subcommands (`parley tokens`).
