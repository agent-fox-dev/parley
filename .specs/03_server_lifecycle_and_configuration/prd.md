---
spec_id: "03"
spec_name: "server_lifecycle_and_configuration"
title: "Start, configure, maintain and stop the hub"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "947a0e6d2fcd7a1d6cae4071f6028df45d910f026b9329d40274030e311fd0c4"
schema_version: 3
source: "docs/prd/03_platform.md"
---

# Start, configure, maintain and stop the hub

## Intent

An operator runs the hub from one binary, one configuration file and one data directory. This package gives `parleyd` its life as a process: it reads and strictly validates its configuration, opens its data, hands the operator a first admin token, resumes durable work, runs hourly maintenance, stops within a bounded time, never lets a credential leave in text, and can snapshot its data while it runs. Every later package plugs its configuration keys, schema, start-up work, background work and maintenance tasks into what this package builds.

## Goals

- `parleyd` reads `config.toml` from `$XDG_CONFIG_HOME` or the working directory, replaces `${NAME}` references in string values, and stops the start with an error that names the key on invalid TOML, an unknown section, an unknown key or an invalid value. A missing file means all defaults.
- `parleyd` creates its data directory (mode 0700) and workspace root, opens a transactional store and an analytical store, and brings both to the current schema on every start without a separate step.
- `parleyd --admin-email=a@x` on a new hub writes an admin token file (mode 0600), logs its path and exits 0 without serving; the hub refuses to start while the file exists; `--reset-admin-token` replaces the token.
- The start runs in one fixed order (configuration, data, bootstrap, resumed durable work, maintenance loop, listener), and any failure before listening exits 1 with one error line on stderr.
- The maintenance loop runs one pass right after start and then one every hour, runs its tasks in a fixed order, never overlaps passes, and a failing task never stops the others.
- On SIGTERM or SIGINT the process exits with status 0 at most 30 seconds after the signal, after giving requests 15 seconds and each piece of background work its own grace.
- One scrubbing function removes credentials from any text: known secret values and hub credentials become `[redacted]`, user information is removed from URLs, and the text is cut to 1024 bytes. The hub's logger applies it to every line.
- `parleyd backup <directory>` writes a consistent snapshot of the data of a running hub into an empty directory and exits 1 when the directory is not empty.

## Non-goals

- The listener, `/healthz`, `/readyz` and `/version` as endpoints, the 15-second grace for in-flight requests, `--help` and the refusal of an unknown flag as first delivered: package 01 hub_skeleton. This package extends that start and stop.
- The body limit, the rate limit, the access-log line and the behaviour of `Idempotency-Key` themselves: package 02 http_api_conventions. This package reads `[server].max_body_size`, `[limits].requests_per_minute` and `[logging].log_health_probes` and hands the values to the server, and it gives the server a place in the transactional store for its idempotency records.
- Authenticating a request with the admin token, and using the OAuth providers this package reads from configuration: package 04 authentication_and_login.
- The configuration keys of other sections (`[forks]`, the other keys of `[limits]`, `[events]`, `[audit]`, `[sessions]`, `[webhooks]`, `[git]`, `[scm]`, `[changes]`, `[land]`, `[awareness]`, `[secrets]`, `[sandbox]` and its tables): the package that owns each registers its keys with the loader this package builds (packages 10 to 53).
- The tables of every feature: each package registers its own schema steps with the store this package builds.
- What each maintenance task does (retention, secret re-encryption, claim cleanup, forge delivery pruning, token binding sweep, patch purge, fork expiry, sandbox record cleanup): packages 51 audit_query_and_retention, 39 secrets, 30 awareness, 43 forge_webhook_ingress, 08 workspace_bound_tokens, 38 carry_patch_rerere_and_maintenance, 16 task_fork_lifetime and 47 sandbox_lifecycle_and_checkpoints. This package runs whatever is registered.
- What is resumed at start (clones, restacks, land attempts, rebuild jobs, sandbox provisioning) and the reconciliation of refs with records: packages 10, 24, 25, 33, 45 and 54. This package runs the registered start hooks in order.
- The sandbox runtime check at start (SBX-FR-003): package 44 sandbox_runtime_and_profiles, as a start hook.
- Closing event streams and stopping rebuild jobs at shutdown: packages 52 durable_event_log_and_stream and 33 rebuild_jobs. This package gives them the shutdown notification and the grace mechanism.
- The `git` version check at start (NFR-004), the guarantee that a newer binary opens the data of the previous release (NFR-003) with its tests, restoring a snapshot and the proof that a hub started on a restored snapshot starts cleanly under load (NFR-007), log fields beyond those named here (NFR-042), container images and manifests: package 54 operations_and_packaging.
- `PARLEY_SECRETS_KEY` and `PARLEY_SECRETS_KEY_PREVIOUS`: package 39 secrets. Proxy variables are honoured by the standard library's HTTP client in the packages that make outgoing calls.
- The client configuration file and the `parley` environment variables: package 09 cli_foundation.
- Refusing a second `parleyd` on the same data: not covered by this package; the PRD says only that it is not supported (§3.3.1).

## Background

The repository holds no code yet; every file named here is new. Package 01 hub_skeleton delivers `cmd/parleyd` with a run function, `internal/server` with the three unauthenticated endpoints, a list of readiness probes and a 15-second shutdown grace, and `internal/buildinfo`. Until this package, `parleyd` listens on `0.0.0.0:8080`, has no configuration, no data and no background work.

The source PRD (`docs/prd/03_platform.md`) puts the process's life in PLAT-FR-001 to PLAT-FR-005, scrubbing in PLAT-FR-098 and configuration in §3.9; `docs/prd/15_non_functional.md` adds the one-process, embedded-store model (NFR-001, NFR-002), automatic schema evolution (NFR-003), the backup command (NFR-007), restart survival (NFR-013) and the maintenance guarantee (NFR-043).

This package builds in these places:

| Path | Holds |
|---|---|
| `internal/config/` | the configuration loader: location, TOML, environment references, the key registry, the keys of §3.9 |
| `internal/store/` | the two embedded stores: path resolution, open, schema steps, transactions, snapshot |
| `internal/scrub/` | the scrubbing function (new directory; a leaf that imports nothing of the project) |
| `internal/identity/admin.go` | the admin settings: bootstrap e-mail, admin token generation, the stored hash |
| `internal/maintenance/` | the maintenance loop and its task registry |
| `internal/hub/` | the composition of the process (new directory): start order, start hooks, background work, logger, shutdown, the backup command |
| `cmd/parleyd/` | `main`: flags, the `backup` subcommand, exit status |

## Requirements

### Flags and the subcommand (PLAT-FR-001, §3.9 "Server flags")

`parleyd` takes `--admin-email <string>` and `--reset-admin-token`. `--help` prints usage on stdout and exits 0. An unknown flag prints usage on stderr and exits 2. The one subcommand is `parleyd backup <directory>`; any other positional argument, and `backup` with no directory or more than one, prints usage on stderr and exits 2. Nothing is loaded, opened or bound in those cases.

### Configuration file (§3.9)

**Location.** `$XDG_CONFIG_HOME/config.toml` when `XDG_CONFIG_HOME` is set and non-empty, otherwise `./config.toml`. The directory of that file is the **config dir**; it also holds the `admin_token` file. A missing file means all defaults. The file is read once, at start. A file that exists and cannot be read is a start error that names its path.

**Strictness.** Invalid TOML, an unknown section, an unknown key or an invalid value stops the start with an error that names the key (as `section.key`; for invalid TOML, the line). The process exits 1 with one error line on stderr.

**Environment references.** `${NAME}` inside a string value is replaced by the environment variable `NAME`. A reference to an unset variable is a start error that names the variable and the key. `$$` yields a literal `$`. A variable that is set to the empty string yields the empty string. References are replaced after the file is parsed and only in string values, so a variable's content can never add a key or a section. The replaced value is validated like a literal one.

**Keys this package owns.**

| Section | Key | Type | Default | Valid |
|---|---|---|---|---|
| `[server]` | `bind` | string | `"0.0.0.0"` | non-empty |
| `[server]` | `port` | int | `8080` | 0–65535; 0 picks a free port |
| `[server]` | `external_url` | string | `""` | empty, or an absolute `http` or `https` URL |
| `[server]` | `max_body_size` | string | `"1MB"` | a positive integer followed by `KB`, `MB` or `GB`, case-insensitive, powers of 1024 |
| `[database]` | `path` | string | see below | non-empty |
| `[workspace]` | `path` | string | see below | non-empty |
| `[workspace]` | `workers` | int | `4` | at least 1 |
| `[logging]` | `level` | string | `"info"` | `trace`, `debug`, `info`, `warn` or `error`, case-insensitive |
| `[logging]` | `log_health_probes` | bool | `false` | |
| `[limits]` | `requests_per_minute` | int | `600` | 0 or more; 0 turns the limit off |
| `[[oauth.providers]]` | `name` | string | required | `github` or `google`; unique among the providers |
| `[[oauth.providers]]` | `client_id`, `client_secret` | string | required | non-empty |
| `[[oauth.providers]]` | `authorize_url`, `token_url`, `userinfo_url` | string | the provider's public endpoints | absolute `http` or `https` URL |

**Keys other packages own.** A later package registers each key it owns with the loader: section, key, type, default and a validation rule. A key in the file that nobody registered is unknown. Registering the same `section.key` twice is an error at start.

**Sections other packages own as a whole.** A later package may instead register a whole section by name with a validator of its own. The loader does not check the keys of such a section. When the file holds the section, the loader hands the validator the section's parsed content, after the environment references in its string values are replaced, and the validator returns one error that reports all the section's failures together; that error stops the start. When the file does not hold the section, the validator is not called and the configuration reports the section as absent. Registering a section twice, or as a whole when keys are registered in it, is an error at start.

### Data directory and stores (§3.9 `[database]`, `[workspace]`; NFR-001, NFR-002, NFR-003)

`database.path` defaults to `./data/parley.db`, or `$XDG_DATA_HOME/parley.db` when `XDG_DATA_HOME` is set and non-empty. A bare file name is placed under `$XDG_DATA_HOME` when that is set. `workspace.path` defaults to `./data/workspaces`, or `$XDG_DATA_HOME/workspaces`, with the same resolution. The directory of the database file is the **data directory**.

At start the hub creates the data directory with mode 0700 and the workspace root, opens the transactional store at `database.path` and the analytical store beside it, and applies every schema step that the data has not yet had, in order, each in one transaction. A second start applies none. A step that fails is rolled back, the steps before it stay, and the start stops with exit status 1 and an error that names the step. A data directory that cannot be created, or a store file that cannot be opened, stops the start the same way, naming the path.

Work on the transactional store runs in transactions: a transaction whose function returns without error is committed, and one whose function returns an error or panics leaves no trace. Two transactions running at the same time both complete; neither fails because the other is running (PLAT-FR-019).

`GET /readyz` answers `200 {"status":"ready"}` while both stores answer a query and `503 {"status":"not ready"}` when one does not (§3.5, §3.10).

### Start (PLAT-FR-002)

The server loads and validates its configuration, opens its data, runs the bootstrap, resumes durable work, starts the maintenance loop and then listens on `[server].bind:[server].port`. Durable work is resumed through **start hooks** that later packages register; they run in registration order, each to completion, before the listener is bound. Any failure before listening stops the process with exit status 1 and one error line on stderr, with no `listening` line.

When it starts listening, the server logs one line with `msg` `listening`, the bound address as `addr`, the configuration path as `config` and the data directory as `data_dir`.

Logs are JSON lines on stderr. A line below `[logging].level` is not written. The configured body limit, request rate and health-probe logging are handed to the HTTP server.

### Bootstrap (PLAT-FR-003, PLAT-US-01)

The hub is **bootstrapped** once an admin token hash is stored. The token file is `<config dir>/admin_token`.

| Situation | Behaviour |
|---|---|
| not bootstrapped, no flags | start normally, without an admin token |
| not bootstrapped, `--admin-email` | store the e-mail, generate the token, write the file, exit |
| `--reset-admin-token` (bootstrapped or not) | generate a new token, replace the stored hash, write the file (overwriting), exit |
| bootstrapped, token file exists | refuse to start: exit 1, naming the file |
| bootstrapped, no token file | start normally; `--admin-email` is ignored |

The token is `parley_admin_` followed by 64 lowercase hexadecimal characters (§3.3.2). The token file has mode 0600 and holds the token with no trailing newline. After writing it, the server logs the file's absolute path at warn level, tells the operator to save the token and delete the file, and exits with status 0 without serving. The token itself is never logged; the hub stores only its hash. After a reset the old token's hash is gone, so the old token stops working at once. If the token file cannot be written, the process exits 1 naming the file and the stored e-mail and hash are unchanged.

### Maintenance loop (PLAT-FR-005, NFR-043)

The loop runs one pass right after start and then one every hour. The interval is not configurable. A pass runs the registered tasks in this order, skipping a name nobody registered: `retention`, `secret_reencryption`, `claim_cleanup`, `forge_delivery_pruning`, `token_binding_sweep`, `patch_purge`, `fork_expiry`, `sandbox_record_cleanup`. A failing task is logged and does not stop the others. Passes never overlap. The loop is the guaranteed backstop; a section may also act on its own, finer timer.

### Shutdown (PLAT-FR-004)

On SIGTERM or SIGINT the server stops accepting connections and gives in-flight requests 15 seconds to finish. Long-lived responses are told to end at once. Background work stops within the grace its section sets (running rebuild jobs: 30 s). The process exits when requests and background work have ended or their grace has passed, at most 30 s after the signal. Sandboxes are not touched. Anything durable resumes at the next start: what a transaction committed before the signal is there after a restart. A clean shutdown exits 0. A failure to bind the listener exits 1.

### Scrubbing (PLAT-FR-098, NFR-021, NFR-042)

Text that leaves the hub in logs, events, audit records or error messages MUST NOT contain credentials. Before such text is emitted:

- known secret values (tokens, secrets, forge and sandbox runtime credentials) are replaced by `[redacted]`;
- user information is removed from URLs: `https://user:pass@host/…` becomes `https://host/…`;
- the text is cut to at most 1024 bytes on a character boundary;
- if a secret cannot be removed cleanly, the whole text becomes `[redacted]`.

The hub never logs an `Authorization` header or a request body that carries a secret. Every line the hub's logger writes, and the one error line of a failed start, passes through the scrubber.

### Backup (§3.9 "Server flags", NFR-007)

`parleyd backup <directory>` writes a consistent snapshot of the data directory while the hub runs. It reads the same configuration as the server to find the data. The directory must be empty; the command exits 1 when it is not. The snapshot holds the transactional store, the analytical store and the repositories under the workspace root. Each store in the snapshot holds whole transactions only. The hub keeps serving while the command runs.

### End-to-end flows

**First boot.** An operator writes `config.toml` with an OAuth client secret given as `${GITHUB_SECRET}` and runs `parleyd --admin-email=a@x`. `parleyd` loads the configuration, creates the data directory, opens the stores, stores the e-mail and the token's hash, writes `admin_token`, logs its path and exits 0. The operator starts `parleyd` again: it refuses, naming the file. The operator saves the token, deletes the file and starts `parleyd`: it runs the start hooks, starts the maintenance loop, listens and logs the address, the configuration path and the data directory. `GET /readyz` answers 200. The operator sends SIGTERM and `parleyd` exits 0.

**Backup of a running hub.** While `parleyd` serves, the operator runs `parleyd backup /backups/today`. The command reads the configuration, copies both stores and the repositories into the empty directory and exits 0. The running hub still answers `GET /readyz` with 200.

**Hourly maintenance.** The hour passes. `parleyd` starts a pass and runs the registered tasks in the fixed order. One task fails; `parleyd` logs it with the task's name and runs the remaining tasks. The next pass runs an hour later.

## Design Decisions

1. **Project decisions relied on.** Go; the stores are embedded SQLite through `modernc.org/sqlite` (no cgo) behind `internal/store`, the analytical store being the same engine in a second file; `parleyd` parses its flags with the standard `flag` package; tests use the standard `testing` package, table-driven, with `net/http/httptest` and `t.TempDir()`.
2. **TOML parser.** `github.com/BurntSushi/toml` v1.6.0, decoding into a generic map that the key registry then checks. Why: strictness is decided by the registry, not by struct tags, because other packages add keys.
3. **Two directories beyond the brief's layout.** `internal/scrub/` is a leaf every package can import without a cycle. `internal/hub/` composes the process, so that `cmd/parleyd` stays a thin `main` and tests and `test/e2e` start a hub in-process.
4. **The run function's inputs.** The hub's run function takes the arguments, an environment lookup, the working directory, stdout and stderr, and returns the exit status. The listen address now comes from configuration; tests set `[server].bind` to `127.0.0.1` and `port` to 0. Why: tests run in parallel without changing the process environment.
5. **Key registry.** The PRD says an unknown key stops the start and also lets a dozen sections define keys. A package registers each key it owns; several packages may register keys in one section (`[limits]`). A duplicate registration is a start error.
6. **Environment references.** Replacement happens after parsing and only in string values. A `$` followed by neither `{` nor `$` stays as it is. `${` without `}`, and a name that does not match `[A-Za-z_][A-Za-z0-9_]*`, are invalid values of their key. The PRD is silent on all three.
7. **Config path when the file is missing.** The `config` member of the listening line is the path that was looked for.
8. **Relative paths.** A `database.path` or `workspace.path` with a directory part is resolved against the working directory. A bare file name with `XDG_DATA_HOME` unset is resolved against the working directory too.
9. **Analytical store file.** `analytics.db` in the data directory. The workspace root is created with mode 0700 like the data directory. The mode of a directory that already exists is not changed.
10. **Schema steps.** A step has a unique name, a target store and a function; steps are applied in registration order and the applied names are recorded in the store they target. Registering a name twice is a start error. This is the mechanism only; the compatibility guarantee of NFR-003 is package 54's.
11. **Admin token.** 32 bytes from the system's random source, written as 64 lowercase hex characters after `parley_admin_`. The stored hash is the lowercase hex SHA-256 of the whole token. Package 04 authenticates against it.
12. **Bootstrap corners the PRD leaves open.** `--reset-admin-token` wins over an existing token file (the PRD says it overwrites). With both flags on a hub that is not bootstrapped, the e-mail is stored too. An `--admin-email` with an empty value counts as not given. A token file left over on a hub that is not bootstrapped is overwritten by `--admin-email`. The file is written inside the transaction that stores the hash, so a failed write stores nothing.
13. **Start hooks and background work.** Later packages register a start hook (a function that returns an error) and background work (a function that runs until its context is cancelled, with a grace). Hooks run in registration order; a hook's error stops the start. The first maintenance pass runs in the background and does not delay the listener.
14. **Maintenance details.** The eight task names are fixed by this package. A task registered under any other name runs after the eight, in registration order, so that a package with a cleanup the PRD's table does not list has a place. A task that panics counts as failed. An hourly tick that arrives while a pass runs is dropped; the next pass starts at the next tick. At shutdown no further task starts, the running task's context is cancelled, and the loop is given 15 seconds.
15. **Shutdown details.** The hub offers a shutdown notification that fires when the signal arrives; handlers of long-lived responses end on it. Background work whose grace passes is abandoned. The stores are closed last. The exit status is 0 also when a grace passed, as package 01 decided for requests. A grace above 30 seconds is cut to the 30-second total. A second SIGTERM or SIGINT during the shutdown changes nothing: no grace restarts and the process does not exit early.
16. **Start errors are one JSON log line** at error level on stderr, written through the scrubbing logger.
17. **`trace`** is a level below `debug`; with it every line is written.
18. **How the scrubber knows a secret.** Two ways: values a package registers (configuration secrets such as `client_secret`, forge and runtime credentials, decrypted secrets) and values passed with one call; and the hub's own credential formats, recognised by shape because the hub keeps only their hashes: `parley_admin_<64 hex>`, `parley_pat_<8>_<32>` and `parley_<8>_<32>`. Longer values are replaced first. An empty value, and a value that is a part of the marker `[redacted]`, are ignored.
19. **"Cannot be removed cleanly"** is decided by looking again: if the text after replacement still holds a known secret value, a hub credential or a URL with user information, the whole text becomes `[redacted]`. The order is: replace values, remove user information, look again, cut.
20. **User information in URLs** is everything between `://` and the last `@` before the next `/`, whitespace or end of text, for any scheme.
21. **Backup details.** The snapshot directory holds the database file under its own name, `analytics.db` and the workspace root under its own name. Stores are copied first (each as one consistent read), then the repositories, refs before objects. A directory that does not exist is created; a path that is a file is refused like a non-empty directory. On a failure the command removes what it wrote. It opens no listener, applies no schema step and writes nothing to the data. On success it writes one log line and nothing on stdout. `--admin-email` or `--reset-admin-token` together with `backup` is a usage error (exit 2).
22. **`parleyd backup` supersedes** package 01's refusal of every positional argument, for the word `backup` only (brief, note 2). Package 01 has no criterion and no test about `backup` as first argument, so this package changes none of its tests.
23. **OAuth endpoint defaults.** `github`: `https://github.com/login/oauth/authorize`, `https://github.com/login/oauth/access_token`, `https://api.github.com/user`. `google`: `https://accounts.google.com/o/oauth2/v2/auth`, `https://oauth2.googleapis.com/token`, `https://openidconnect.googleapis.com/v1/userinfo`. The PRD says "the provider's public endpoints" and shows only GitHub's authorize URL.
24. **Profile: full**, with three execution paths.
25. **Idempotency records are kept in the transactional store** (decided with package 02). The source PRD lists them as "kept for 24 hours, then dropped" and only the rate-limit counters as "in memory; reset at restart" (§3.7). Package 02 defines the record store interface and the replay behaviour; this package, which owns the store and composes the process, registers one schema step for the records and installs a store-backed implementation of that interface through the server option package 02 provides (`internal/hub/idempotency.go`). Why here: no package owns both sides, and this one already hands package 02's server its other options.
26. **Where later packages plug in.** `internal/hub/` is the one place that composes the process: a package registers its configuration keys, schema steps, start hooks, background work, maintenance tasks, routes and the checks of package 02's pipeline positions on the composition, in a file of its own (`internal/hub/identity.go`, `internal/hub/access.go`, `internal/hub/events.go`). `cmd/parleyd/main.go` stays a thin caller and is not edited by later packages. The admin-token code in `internal/identity/admin.go` offers a verification function (a presented token against the stored hash, compared in constant time, false when none is stored) that package 04 authenticates with.
27. **Whole-section registration** (decided with package 44 sandbox_runtime_and_profiles). Beside per-key registration, `internal/config/registry.go` offers `RegisterSection(name, validator)`: the registrant's validator receives the whole parsed section, after environment references are replaced, and its single error, which lists every failure, fails the load; the loaded configuration reports whether the file held the section and its content. Why: the per-key walk stops at the first bad key, and SBX-FR-002 wants every failure of `[sandbox]` together, with tables and lists the key registry does not describe. Package 44 registers `[sandbox]` through it and adds nothing to the loader. A section is registered either key by key or as a whole, never both.

## Dependencies

- Package 01 hub_skeleton (draft): this package extends its run function, its server options, its readiness probes and its shutdown.
- Package 02 http_api_conventions (draft): the server options this package fills from configuration (body limit, requests per minute, health-probe logging), the pipeline positions later packages install their checks in through the composition, and the idempotency record store interface this package implements over the transactional store.

## Verified External API

`github.com/BurntSushi/toml` v1.6.0, read in the module cache (`decode.go`, `meta.go`, `error.go`):

- `func Decode(data string, v any) (MetaData, error)`
- `func (md *MetaData) Keys() []Key`
- `func (k Key) String() string`
- `type ParseError struct` (carries the position of a syntax error)

`modernc.org/sqlite` v1.60.1, read in the module cache (`sqlite.go`, `driver.go`):

- the driver registers itself with `database/sql` under the name `sqlite` (`sql.Open("sqlite", dsn)`)
- the data source name accepts `_pragma=<name>(<value>)` and `_txlock=<mode>` query parameters

The SQL statement `VACUUM INTO '<file>'` used for the snapshot is SQLite's own and was not exercised here: **unverified** through this driver.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
- **Durable job**: Background work that is recorded before its request returns, survives restarts and is retried with backoff. Rebuilds are the only durable jobs (§10). Clone work, restacks and land attempts are also durable but follow their own rules and are not retried with backoff (§4, §7, §8).
