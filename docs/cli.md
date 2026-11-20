# CLI reference

Parley ships two programs: `parleyd`, the hub, and `parley`, the
command-line client. This page describes both.

## `parleyd`

```
Usage: parleyd [flags]
       parleyd backup <directory>
```

Without a command, `parleyd` starts the hub. It reads `config.toml` (see
[configuration.md](configuration.md)), opens its data, listens for HTTP on
`[server].bind` and `[server].port` (`0.0.0.0:8080` by default) and runs
until it receives `SIGTERM` or `SIGINT`.

### Flags

| Flag | Effect |
|---|---|
| `--admin-email <address>` | on a hub that has no admin token yet, store the address, write a new admin token to the token file and exit `0` without serving (see [The admin token](#the-admin-token)); ignored once the hub has one. It takes one value; `--admin-email` without one is a usage error, and `--admin-email=` counts as not given |
| `--reset-admin-token` | write a new admin token to the token file, replacing the stored one, and exit `0` without serving |
| `-h`, `--help` | print the usage on stdout and exit `0` without reading the configuration or listening |

`--help` prints:

```
Usage: parleyd [flags]
       parleyd backup <directory>

parleyd is the Parley hub. It reads config.toml from $XDG_CONFIG_HOME or the
working directory, opens its data, listens for HTTP on [server].bind and
[server].port, and runs until it receives SIGTERM or SIGINT.

Commands:
  backup <directory>   write a snapshot of the hub's data into an empty directory

Flags:
  --admin-email <address>   on a new hub, store the admin's e-mail, write an
                            admin token file and exit
  --reset-admin-token       replace the admin token, write it to the token
                            file and exit
  -h, --help                print this help and exit
```

A flag it does not define, `--admin-email` without a value, a positional
argument other than `backup`, `backup` followed by no directory or by
more than one argument, and `backup` with `--admin-email` or
`--reset-admin-token`, print one error line, then the same usage, on
stderr, and exit `2` without reading the configuration, creating anything
or listening:

```
parleyd: argument "--nope": flag provided but not defined: -nope
parleyd: argument "--admin-email": flag needs an argument: -admin-email
parleyd: unexpected argument "serve"
parleyd: backup takes one directory, got 0 arguments
```

Flags come before the command: in `parleyd backup a --nope`, `--nope` is a
second argument of `backup`.

### `parleyd backup <directory>`

`parleyd backup <directory>` writes a snapshot of the hub's data into
`<directory>`, whether or not a hub is running on that data. Run it with the
environment and working directory of the hub: it reads `config.toml` exactly
as the hub does, so it finds the same data. A relative `<directory>` is
resolved against the working directory.

`<directory>` must be an empty directory or not exist; a missing one is
created with mode `0700` (its parent is not). The snapshot holds three
entries:

| Entry | What |
|---|---|
| the database file's own name (`parley.db` by default) | a copy of the transactional store |
| `analytics.db` | a copy of the analytical store |
| the workspace root's own name (`workspaces` by default) | a copy of every file, directory and symbolic link under the workspace root but files whose name ends in `.lock`, with the same relative paths |

Each store is copied first, as one read of the engine (SQLite's
`VACUUM INTO`): the copy holds every transaction committed before the copy
began, each with all of its rows, while the hub goes on committing, and it
is a single database file without a write-ahead log. The workspace root is
copied next; within each git repository the refs, `packed-refs` and `HEAD`
are copied before the objects, so every copied ref points at a copied
object. Files whose name ends in `.lock` (git's locks of a write in
progress) are left out, so a repository restored from the snapshot takes
pushes.

The command only reads the hub's data: it binds no listener, runs no
bootstrap, start work or maintenance task, applies no schema step and
writes nothing under the data directory or the config dir, not even the
write-ahead log of a stopped store. `--admin-email` or
`--reset-admin-token` together with `backup` is a usage error (exit `2`):

```
parleyd: backup does not take --admin-email or --reset-admin-token
```

On success it writes one log line naming the directory, nothing on stdout,
and exits `0`:

```json
{"time":"2026-11-13T12:44:23.060364Z","level":"INFO","msg":"backup written","dir":"/srv/backups/2026-11-13"}
```

Otherwise it writes one line, `backup failed` with the cause in `error`,
and exits `1`. It refuses, writing nothing, an invalid configuration (the
error names the key), a missing database file (it names the path and
creates no database), a directory that is not empty, a path that is not a
directory, and a directory inside the workspace root or inside the data
directory (the directory of `[database].path`):

```json
{"time":"2026-11-13T12:44:22.522501Z","level":"ERROR","msg":"backup failed","error":"database file /srv/parley/data/parley.db does not exist"}
{"time":"2026-11-13T12:44:23.075944Z","level":"ERROR","msg":"backup failed","error":"backup directory /srv/backups/2026-11-13 is not empty"}
{"time":"2026-11-13T12:44:23.081202Z","level":"ERROR","msg":"backup failed","error":"backup directory /srv/parley/data/snapshot is inside the data directory /srv/parley/data"}
```

A relative directory is taken relative to the working directory.

When a copy fails part-way, for example on a file under the workspace root
it cannot read, it removes everything it wrote (the directory too, when it
created it), and the line names the path that failed.

There is no restore command: restoring is copying. Copy the snapshot's
database file to `[database].path`, its `analytics.db` beside it and its
workspace directory to `[workspace].path` on the new host, put the
configuration file in place (the snapshot holds none) and start `parleyd`
with no flags. It applies no schema step, runs its start work on the
restored data, listens and keeps the source hub's admin token. A snapshot
taken while the hub commits and git clients push restores the same way:
both stores pass an integrity check, and every repository passes `git fsck`
with each ref at the source's commit or an ancestor of it.

### The admin token

A new hub has no users, so its operator administers it with an admin token.
The hub stores only the token's SHA-256 hash and the address given with
`--admin-email`; the token itself is written to the file `admin_token` in
the config dir (the directory `config.toml` is looked for in, see
[configuration.md](configuration.md#the-configuration-file)), with mode
`0600`, as `parley_admin_` followed by 64 lowercase hex characters and no
trailing newline. The token is never written to stdout, to stderr or under
the data directory.

After the stores are open and before any start work runs, `parleyd`
decides:

| Flags | Hub | What happens |
|---|---|---|
| `--reset-admin-token` | any | a new token replaces the stored one; the address of `--admin-email` is stored too when the hub had no token before. The token file is written, over one that is there, and `parleyd` exits `0` |
| `--admin-email <address>` | no token stored | the address and the new token's hash are stored, the token file is written, over a leftover one, and `parleyd` exits `0` |
| none | no token stored | `parleyd` starts and serves without a token |
| none or `--admin-email` | a token stored, token file there | `parleyd` refuses to start: one error line naming the file, exit `1` |
| none or `--admin-email` | a token stored, no token file | `parleyd` starts and serves; `--admin-email` is ignored |

A run that writes the token file logs its absolute path at warn level and
exits `0` without listening or running any start work:

```json
{"time":"2026-11-13T12:20:41.118034Z","level":"WARN","msg":"admin token written: save the token and delete the file","path":"/srv/parley/admin_token"}
```

Save the token and delete the file: the hub does not start while the file
is there. The hash and the file are written together, so when the file
cannot be written (a missing config dir, no permission, a directory in its
place) nothing is stored, and `parleyd` writes one `cannot start` line naming
the file and exits `1`. While the file is there, a start without
`--reset-admin-token` writes:

```json
{"time":"2026-11-13T12:21:02.530118Z","level":"ERROR","msg":"cannot start","error":"admin token file /srv/parley/admin_token exists: save the token, delete the file and start the hub again"}
```

### The start

`parleyd` starts in this order and stops at the first stage that fails:

1. it loads and checks the configuration, and loads the secret keys (see
   [configuration.md](configuration.md#secret-keys));
2. it creates the data directory and the workspace root when they are
   missing, opens both stores and brings them to the current schema;
3. the bootstrap: it writes the admin token file and exits, refuses to
   start while the file is there, or goes on (see
   [The admin token](#the-admin-token));
4. it runs the start work the hub's parts registered, in order, each to
   completion, beginning with the git check (see [Git](#git));
5. it binds `[server].bind`:`[server].port`;
6. it starts the maintenance loop in the background, without waiting for
   its first pass: one pass at once and one every hour, each running the
   registered maintenance tasks one after the other; and it starts the
   background work the hub's parts registered;
7. it writes the `listening` line and serves.

A failure in stages 1 to 5 writes exactly one error line to stderr, closes
the stores when they were open, and exits `1` without listening.

### Git

The hub needs the `git` command, version 2.38 or later. The first start work
runs `git --version` once and reads the version from the first line of its
output, `git version <major>.<minor>` followed by anything
(`git version 2.39.3 (Apple Git-146)` and `git version 2.45.1.windows.1` are
read); only major and minor are compared. `git` is looked up on the `PATH` of
the environment `parleyd` was started with.

When the version is accepted, the line `git version checked` is one of the
start reports written right before the `listening` line, its `git_version`
the text after `git version ` up to the first space:

```json
{"time":"2026-11-20T09:14:03.201544Z","level":"INFO","msg":"git version checked","git_version":"2.39.3"}
```

Otherwise the start stops with one error line and exit status `1`, before
any other start work runs and without listening:

| Case | The `error` contains |
|---|---|
| no `git` on `PATH` | `git not found on PATH; parleyd needs git 2.38 or later` |
| a version below 2.38 | `git 2.30.2 is too old; parleyd needs git 2.38 or later` |
| a first line not of the form above | `cannot read the git version from "<first line, cut to 100 bytes>"` |
| `git --version` exits with a status other than 0 | `git --version failed: exit status 3` |
| no answer within 10 seconds (the process is killed) | `git --version failed: no answer within 10s` |

```json
{"time":"2026-11-20T09:13:41.007812Z","level":"ERROR","msg":"cannot start","error":"start hook \"git-version\": git 2.30.2 is too old; parleyd needs git 2.38 or later"}
```

`--help`, a usage error, `parleyd backup` and the bootstrap runs that write
the admin token file and exit end before the start work, so they run no
`git` and work on a host without it.

### Log lines

`parleyd` logs JSON objects to stderr, one per line, with `log/slog`. Every
line has `time`, an RFC 3339 timestamp in UTC ending in `Z`, `level` and
`msg`; a line break inside a message or a value is written escaped, so a
logged line is always one line. `level` is `TRACE`, `DEBUG`, `INFO`,
`WARN` or `ERROR`; lines below `[logging].level` (`info` by default) are not
written, and with `trace` every line is. Before the configuration is loaded
the level is `info`. A line that cannot be written to stderr is dropped:
the request that caused it is answered as it would have been, and the hub
goes on.

A line logged while a request is handled, by the request's handler, a
middleware or code they call with the request's context, carries
`request_id`, the request's `X-Request-ID`. A line logged outside a request
(the start, the maintenance loop, background work) has none.

The message and every attribute of every line are scrubbed before the line
is written (an error as its text; a value that is neither a string nor an
error as its printed text, which takes the value's place when scrubbing
changes it, and otherwise every string of its JSON form, map keys included;
numbers, booleans and durations are written as they are): the
`client_secret` of every configured OAuth provider, other secret values the
hub knows and the hub's own credentials (admin tokens, personal access
tokens and API keys) become `[redacted]`, the user information is removed
from URLs (`https://u:p@host/x` becomes `https://host/x`), and each value is
cut to at most 1024 bytes. A value from which a secret cannot be removed
cleanly is replaced by `[redacted]` as a whole. Attribute names, `time` and
`level` are written as they are. No `Authorization` header and no request
body is logged; a set of HTTP headers given to the logger shows its
`Authorization` and `Proxy-Authorization` values as `[redacted]`.

Once it has bound the address, it writes what its parts report about the
start, then the `listening` line. The secret store reports whether a
current key is configured, whether a previous key is, and how many stored
secrets were sealed under a key that is neither (counted by the key id
stored with each, without decrypting any): those cannot be read until their
key is configured again.

```json
{"time":"2026-11-14T05:20:11.402117Z","level":"INFO","msg":"secret encryption","key_configured":true,"previous_key_present":false,"undecryptable_secrets":0}
```

Without a current key it also writes a warning and goes on starting:

```json
{"time":"2026-11-14T05:20:11.402121Z","level":"WARN","msg":"secrets cannot be stored: no encryption key configured"}
```

```json
{"time":"2026-11-13T12:11:38.622658Z","level":"INFO","msg":"listening","addr":"127.0.0.1:52887","config":"/srv/parley/config.toml","data_dir":"/srv/parley/data"}
```

`addr` is the address the listener bound, with the port the system chose
when `[server].port` is `0`; Go binds the wildcard `0.0.0.0` as a dual-stack
listener, so the default address is logged as `[::]:8080`. `config` is the
absolute path the configuration file was looked for at, also when it does
not exist, and `data_dir` the absolute data directory, the directory of
`[database].path`.

While it serves, it writes one access-log line for every request it has
answered, except requests to `/healthz` and `/readyz` (which get one, at
debug level, when `[logging].log_health_probes` is `true`):

```json
{"time":"2026-11-13T09:12:03.104211Z","level":"INFO","msg":"request","method":"GET","path":"/version","status":200,"duration_ms":0.087,"request_id":"0b5c6f2e-8d1a-4c3b-9e7f-2a4d6c8e0f13"}
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
{"time":"2026-11-13T09:12:09.551873Z","level":"ERROR","msg":"internal error","request_id":"0b5c6f2e-8d1a-4c3b-9e7f-2a4d6c8e0f13","error":"open workspace: permission denied"}
```

A maintenance task that returns an error or panics writes one line with the
task's name and the scrubbed error, and does not stop the others; it runs
again in the next hourly pass:

```json
{"time":"2026-11-13T09:13:00.000000Z","level":"ERROR","msg":"maintenance task failed","task":"claim_cleanup","error":"remove claim: database is locked"}
```

The maintenance task `token_binding_sweep` writes one info line with the
number of tokens it revoked, in a pass that revoked any: tokens bound to a
workspace that no longer exists:

```json
{"time":"2026-11-14T09:13:00.000000Z","level":"INFO","msg":"revoked the tokens bound to workspaces that no longer exist","count":2}
```

The maintenance task `retention` writes one line for each of its steps
that fails or panics, naming the step, and goes on with the next; the task
then fails too, with the `maintenance task failed` line naming the failed
steps (see [configuration.md](configuration.md#retention)):

```json
{"time":"2026-11-14T09:13:00.000000Z","level":"ERROR","msg":"retention step failed","step":"run_records","error":"delete the records with timestamp before …: database is locked"}
{"time":"2026-11-14T09:13:00.000000Z","level":"ERROR","msg":"maintenance task failed","task":"retention","error":"retention steps failed: run_records"}
```

The maintenance task `secret_reencryption` writes one info line in every
pass that ends without a store error: `converted` is the number of values it
sealed again under the current key, `remaining` the number of stored values
still not under the current key (every value when no current key is
configured), the values that fail to open under the previous key included.
Once `remaining` is `0` the previous key can be removed (see
[configuration.md](configuration.md#secret-keys)):

```json
{"time":"2026-11-14T09:13:00.000000Z","level":"INFO","msg":"secret re-encryption","converted":5,"remaining":0}
```

When a stop stops waiting for something whose grace or the 30-second limit
has passed, it writes one line naming it in `work`: `requests`,
`maintenance loop` or the name of a piece of background work:

```json
{"time":"2026-11-13T13:02:31.401227Z","level":"WARN","msg":"abandoned at the stop","work":"event_streams"}
```

When the start fails before listening it writes one line and exits `1`.
The line is `cannot start` with the cause in `error`, which names the
configuration key, the secret key's variable or file, the path, the schema
step, the admin token file or the start work that failed:

```json
{"time":"2026-11-13T12:11:38.609972Z","level":"ERROR","msg":"cannot start","error":"configuration file /srv/parley/config.toml: server.prot: unknown key"}
```

A secret key that cannot be loaded is named and never quoted:

```json
{"time":"2026-11-14T05:20:11.391337Z","level":"ERROR","msg":"cannot start","error":"PARLEY_SECRETS_KEY: must be standard padded base64 of exactly 32 bytes"}
```

With sandboxes on (a `[sandbox]` section, see
[configuration.md](configuration.md#sandboxes)), the start checks the
OpenShell gateway once, after the stores are open and before it listens, in
the start hook `sandbox_runtime_check`. The check stops at its first
failure, and the line names the key to look at: `sandbox.gateway_url` for a
gateway that cannot be reached, refuses the hub's credential, is not
healthy, runs a release outside the supported range or does not answer
within 30 seconds; `sandbox.credentials[<name>].provider_profile` for a
provider profile the gateway does not know; `sandbox.profiles[<name>].expose_secrets`
or `optional_expose_secrets` for an exposed secret whose name is a variable
of a credential the profile attaches; `sandbox.profiles[<name>].policy` for
a policy file the gateway rejects or cannot validate. The gateway token and
the OIDC client secret never appear in it:

```json
{"time":"2026-11-13T12:11:39.120534Z","level":"ERROR","msg":"cannot start","error":"start hook \"sandbox_runtime_check\": sandbox.gateway_url: the gateway runs OpenShell release \"0.3.1\", outside the supported range >=0.1.0 <0.2.0"}
```

When it cannot bind the address the line is `cannot listen`:

```json
{"time":"2026-11-13T09:11:41.348421Z","level":"ERROR","msg":"cannot listen","addr":"0.0.0.0:8080","error":"listen tcp 0.0.0.0:8080: bind: address already in use"}
```

`parleyd backup` writes `backup written` with `dir` on success, or
`backup failed` with `error` (see
[`parleyd backup <directory>`](#parleyd-backup-directory)).

### Signals

On `SIGTERM` or `SIGINT` `parleyd` stops accepting connections, tells the
handlers of long-lived responses to end them, cancels the running
maintenance task (and starts no further one) and every piece of background
work the hub's parts registered, and then waits for all of them at the same
time: up to 15 seconds for the requests in flight, after which it closes
their connections, up to 15 seconds for the maintenance loop, and for each
piece of background work up to its own grace (at most 30 seconds). It waits
for nothing longer than 30 seconds after the signal. Then it closes the
stores and exits `0`, also when a grace passed; what it stopped waiting for
is logged (see [Log lines](#log-lines)). With nothing in flight it exits at
once. A further `SIGTERM` or `SIGINT` during the stop changes nothing: no
grace starts again and the process does not exit early.

### Exit statuses

| Status | When |
|---|---|
| `0` | `--help`, a run that wrote the admin token file, a written backup, or a stop by `SIGTERM` or `SIGINT`, whether or not the grace passed |
| `1` | the start failed before listening (the configuration, a secret key, the data, a schema step, the admin token file that could not be written or is still there, the start work or the listen address), or `backup` failed (see [`parleyd backup <directory>`](#parleyd-backup-directory)) |
| `2` | a usage error: an unknown flag, `--admin-email` without a value, a positional argument other than `backup`, `backup` without exactly one directory, or `backup` with `--admin-email` or `--reset-admin-token` |

## `parley`

```
Usage:
  parley [flags]
  parley [command]
```

Run without arguments (or with `--help`, or as `parley help`), `parley`
prints its help on stdout and exits `0`. It reads no credential and sends no
request (see [Help](#help)):

```
parley is the command-line client of the Parley hub

Usage:
  parley [flags]
  parley [command]

Available Commands:
  admin             Administer the hub's users, organizations, API keys and access tokens
  audit             Query who did what on the hub and in the agents' runs
  change            Open, list and read the changes of a workspace, their versions and their diffs
  credential-helper Give git the hub credential for the hub's own URL (git's credential helper protocol)
  events            Follow the hub's events, or read them a page at a time
  fork              Create, list, extend and delete the task forks of a workspace
  help              Print the help of parley or of a command
  keys              List, refresh and revoke your API keys
  login             Sign in to the hub and save the hub URL and the credential
  orgs              List and read the organizations you belong to
  sandbox           Create, list and read the sandboxes of a workspace, run commands and shells in them, copy files in and out, read their logs, checkpoint, stop, start, extend and delete them, and list their profiles
  scm               Read the refs, commits, files, diffs and blame of a workspace, and commit and rebase, without a clone
  secrets           Create, list, update and delete secrets, whose values the hub never shows
  session           Start, report the token usage of, complete and read agent sessions
  tokens            Create, read, change and revoke your personal access tokens
  user              Read and update your own profile
  vars              Create, list, update, delete and resolve variables
  version           Print the client's build and, given a hub URL, the hub's build
  webhook           Subscribe a URL to a workspace's events, and see and resend what the hub delivered to it
  workspace         Register, list, read, change, sync, push, archive, reactivate, reclone and delete the workspaces of the hub

Flags:
      --api-key string        credential presented to the hub (default: $PARLEY_API_KEY, then api_key in the config file); parley login takes it from this flag alone and stores it (- reads it from stdin)
      --endpoint-url string   URL of the hub (default: $PARLEY_ENDPOINT_URL, then endpoint_url in the config file)
  -h, --help                  help for parley
      --json                  print the help as JSON

Use "parley [command] --help" for more information about a command.
```

### Global flags

Every command takes the global flags, before or after its own words.

| Flag | Effect |
|---|---|
| `--endpoint-url <url>` | the URL of the hub, for example `http://127.0.0.1:8080`. When it is absent or empty, `PARLEY_ENDPOINT_URL` gives it, then the key `endpoint_url` of the client configuration file (see [Settings](#settings-the-hub-url-and-the-credential)) |
| `--api-key <credential>` | the credential presented to the hub as `Authorization: Bearer`. When it is absent or empty, `PARLEY_API_KEY` gives it, then the key `api_key` of the client configuration file. `parley login --api-key` stores it and takes it from this flag alone (see [With a credential](#with-a-credential---api-key)) |
| `--json` | with `--help` or `parley help`, print the help as one JSON document (see [Help](#help)). Accepted on every command; without help it changes neither the request a command sends nor what it prints |
| `-h`, `--help` | print the help of the command on stdout and exit `0` (see [Help](#help)) |

A global flag may stand before, between or after the command's words, with
its value as the next word or after `=`: `parley --endpoint-url <url> version`,
`parley version --endpoint-url <url>` and `parley version --endpoint-url=<url>`
are the same command.

### Help

```
Usage:
  parley help [command]... [flags]
```

Help is printed on stdout with exit status `0`, and it never reads a
setting, never reads the client configuration file (a file that is not
valid TOML does not matter to it) and never sends a request. It comes in
three forms:

- `parley` without arguments, and a command group without a subcommand,
  print the help of the root or of the group, which lists each of its
  direct subcommands with its description;
- `parley <words> --help` prints the help of the command `<words>` names:
  its description, its usage line with its arguments (`<name>` required,
  `[name]` optional), its own flags with their defaults and the global
  flags. A flag the command requires need not be given;
- `parley help <words>` prints exactly what `parley <words> --help` prints,
  and `parley help` alone the help of the root.

With the global flag `--json`, each form prints one JSON document instead,
indented by two spaces and followed by a newline, built from the command's
declaration. For a command that is not a group it is one object:

```json
{
  "name": "version",
  "description": "Print the client's build and, given a hub URL, the hub's build",
  "method": "GET",
  "path": "/version",
  "args": [],
  "flags": [
    {
      "name": "api-key",
      "type": "string",
      "required": false,
      "default": "",
      "description": "credential presented to the hub (default: $PARLEY_API_KEY, then api_key in the config file); parley login takes it from this flag alone and stores it (- reads it from stdin)"
    },
    {
      "name": "endpoint-url",
      "type": "string",
      "required": false,
      "default": "",
      "description": "URL of the hub (default: $PARLEY_ENDPOINT_URL, then endpoint_url in the config file)"
    },
    {
      "name": "json",
      "type": "bool",
      "required": false,
      "default": "false",
      "description": "print the help as JSON"
    }
  ],
  "auth": "none"
}
```

| Member | Value |
|---|---|
| `name` | the command's words after `parley`, joined by single spaces (`help`, `version`) |
| `description` | the command's description |
| `method`, `path` | the route the command calls, `path` relative to `/api/v1` with `:param` for its parameters (`parley version`'s `/version` lies outside it); both `null` for a command that calls no single route (`help`, `login`) |
| `args` | the positional arguments in order, each `{"name", "required"}`; `[]` for none |
| `flags` | every flag the command takes, sorted by `name`: its own flags (its `--fail-on-…` flags included) and the three global flags `api-key`, `endpoint-url` and `json`, not `--help`. Each is `{"name"` (without dashes)`, "type"` (the flag library's name for it: `string`, `bool`, `int`, …)`, "required", "default"` (as a string)`, "description"}` |
| `auth` | `none`: the command needs neither a hub URL nor a credential before it runs (`help`, `version`, `login`); `user` or `admin`: it needs both |

For a command group, and for `parley` itself (`parley help --json`,
`parley --help --json`, `parley --json`), the document is
`{"commands": [...]}`: the object of every command under the group or the
root that is not itself a group, at any depth, sorted by `name`; a group
appears only in the names of its commands.

`parley help` with words that name no command fails with a `usage_error`
naming the first word it cannot place, with or without `--json`, and prints
nothing on stdout:

```json
{
  "error": {
    "code": 0,
    "message": "unknown command \"nosuch\" for \"parley\"",
    "error_type": "usage_error"
  }
}
```

### Settings: the hub URL and the credential

A command that calls the hub's REST API as a user or an admin needs two
settings, and takes each from the first source that gives it a non-empty
value:

| Setting | Flag | Variable | Key of the client configuration file |
|---|---|---|---|
| hub URL | `--endpoint-url` | `PARLEY_ENDPOINT_URL` | `endpoint_url` |
| credential | `--api-key` | `PARLEY_API_KEY` | `api_key` |

An empty flag or an empty variable is passed over for the next source.
Values are taken as given: white space is not trimmed. The configuration
file (see [configuration.md](configuration.md#the-client-configuration-file))
is read only when the flags and the variables leave one of the two settings
empty, so a command given both by flags or variables runs whatever the file
holds. When it is read, a file that cannot be read, is not valid TOML or
holds `endpoint_url`, `api_key` or `user_id` with a value that is not a
string fails the command before any request with a `client_error` naming
the file's path. A missing file, and a missing home directory when
`PARLEY_CONFIG_DIR` is unset or empty, count as an empty file; reading
creates neither the directory nor the file.

A setting no source gives fails the command before any request, with a
`client_error` whose message names the flag, the variable and the
configuration file of each missing setting (the file is
`~/.parley/config.toml` when there is no home directory):

```json
{
  "error": {
    "code": 0,
    "message": "no hub URL: pass --endpoint-url, set PARLEY_ENDPOINT_URL or set endpoint_url in /home/alice/.parley/config.toml; no credential: pass --api-key, set PARLEY_API_KEY or set api_key in /home/alice/.parley/config.toml",
    "error_type": "client_error"
  }
}
```

The hub URL must be an absolute URL with the scheme `http` or `https` and a
host, and without a query or a fragment; anything else (`localhost:8080`,
`ftp://host`, `http://`, `/api`, `http://host?x`, `http://host#f`) fails
before any request:

```json
{
  "error": {
    "code": 0,
    "message": "the hub URL \"localhost:8080\" is not an absolute http or https URL with a host and without a query or fragment",
    "error_type": "client_error"
  }
}
```

Every request goes to `<hub URL>/api/v1<path>`, with every trailing slash of
the hub URL removed and a path it carries kept
(`https://host/parley` sends to `https://host/parley/api/v1/…`), with the
command's method and query string, the header
`Authorization: Bearer <credential>`, and `Accept: application/json`. A
request with a body sends it as JSON with `Content-Type: application/json`;
one without sends neither a body nor `Content-Type`.

A value from the command line that fills a segment of the path (a name, a
slug, a branch) is percent-encoded as one segment: a `/` in it is sent as
`%2F`, and `?`, `#`, `%` and spaces are encoded too, so `parley … get
'feature/x y'` sends `…/feature%2Fx%20y`. A value that is `.` or `..` is
sent as `%2E` or `%2E%2E`.

Each request has 30 seconds to complete, from connecting to reading the
whole answer, unless the command states another timeout (`parley version`:
5 seconds; the callback of `parley login`: 60 seconds; `parley events
list`: 30 seconds plus its `--wait`; `parley change land`: 70 seconds;
`parley events tail`: none, see [`parley events`](#parley-events)). A request that has
not completed by then is abandoned with a `client_error`:

```json
{
  "error": {
    "code": 0,
    "message": "the hub at https://hub.example gave no complete answer within 30s",
    "error_type": "client_error"
  }
}
```

Every request is sent exactly once. `parley` does not send it again after
a `429`, a `5xx`, a timeout or a broken connection, does not act on
`Retry-After`, and does not follow a redirect; the error goes to stderr and
the command ends with its exit status, so a script decides whether to try
again. The one exception is `parley events tail`, which follows a stream
until it is interrupted and connects again by itself (see
[`parley events`](#parley-events)).

A hub that cannot be reached (the connection is refused, the host name does
not resolve, the TLS handshake fails) is a `client_error` whose message
names the hub URL:

```json
{
  "error": {
    "code": 0,
    "message": "cannot reach the hub at http://no-such-host.invalid: dial tcp: lookup no-such-host.invalid: no such host",
    "error_type": "client_error"
  }
}
```

An answer that is not the hub's REST API's is a `client_error` whose message
names the HTTP status, because it came from something else (a proxy, a wrong
URL): a status of `400` or above whose body is not an error envelope (a JSON
object with an `error` member holding `error_type`), a `2xx` status whose
body is neither empty nor JSON (`the hub answered 200 with a body that is
not JSON`), and a status outside `2xx` below `400`, a redirect included
(`the hub answered 302, which is neither a success nor an error`). So is
an answer whose body is larger than 8 MiB (8388608 bytes), whatever its
status: parley stops reading at the first byte past that size instead of
waiting for the rest (`the hub answered 200 with a body larger than 8388608
bytes`).

`parley version` and `parley login` need no credential and read the hub URL
themselves, as their sections say; help needs neither setting.

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

It takes the hub URL from the first non-empty of `--endpoint-url`,
`PARLEY_ENDPOINT_URL` and the key `endpoint_url` of the client configuration
file, which it reads only when the flag and the variable give none. With a
hub URL it sends `GET <hub URL>/version` (trailing slashes on the URL are
dropped) without an `Authorization` header, even when the file holds an
`api_key`, and with a timeout of 5 seconds.
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
or it answers a status other than `200` (a redirect, which it does not
follow, included) or a body that is not a JSON object,
`parley version` writes one warning line that names the hub URL to stderr and
prints the document without `server`:

```
warning: cannot read the hub's version from http://127.0.0.1:1: cannot reach the hub at http://127.0.0.1:1: dial tcp 127.0.0.1:1: connect: connection refused
```

When it reads the configuration file and the file cannot be read or parsed,
`parley version` writes one warning line that names the file to stderr,
prints the client's build alone and exits `0`:

```
warning: the config file /home/alice/.parley/config.toml is not valid TOML: line 1, column 8; printing the client's build alone
```

A hub URL that is not an absolute `http` or `https` URL is not refused
either: the request fails and `parley version` warns as above, with exit
status `0`.

`parley version` takes no arguments: `parley version extra` is a
`usage_error` with exit status `2`.

### `parley login`

```
Usage:
  parley login [flags]

Flags:
      --expires string    days the browser login's API key lives: 0, 30, 60 or 90 (0: no expiry) (default "90")
  -h, --help              help for login
      --provider string   identity provider of the browser login (default "github")

Global Flags:
      --api-key string        credential presented to the hub (default: $PARLEY_API_KEY, then api_key in the config file); parley login takes it from this flag alone and stores it (- reads it from stdin)
      --endpoint-url string   URL of the hub (default: $PARLEY_ENDPOINT_URL, then endpoint_url in the config file)
      --json                  print the help as JSON
```

`parley login` saves the hub URL, a credential and the user's id in the
client configuration file (see
[configuration.md](configuration.md#the-client-configuration-file)). It
first checks its flags: a bad `--expires` and an empty credential are a
`usage_error` before the file is read (see below). It then reads that
file, and a file that cannot be read, is not valid TOML
or holds `endpoint_url`, `api_key` or `user_id` with a value that is not a
string fails the login before any request, leaving the file as it is. It
then takes the hub URL from the first non-empty of `--endpoint-url`,
`PARLEY_ENDPOINT_URL` and the file's `endpoint_url`, and drops trailing
slashes from it. A hub URL that is not an absolute `http` or `https` URL
with a host and without a query or fragment fails before any request, as for every command (see
[Settings](#settings-the-hub-url-and-the-credential)). With none it fails
before any request:

```json
{
  "error": {
    "code": 0,
    "message": "no hub URL: pass --endpoint-url, set PARLEY_ENDPOINT_URL or set endpoint_url in /home/alice/.parley/config.toml",
    "error_type": "client_error"
  }
}
```

Only the global flag `--api-key` selects the key flow; `PARLEY_API_KEY` and
the file's `api_key` do not. Without `--api-key`, `parley login` signs in
through the browser.

#### Through the browser

`parley login` without `--api-key` signs the user in at an identity
provider and has the hub issue a new API key. `--expires` sets the key's
lifetime in days and must be `0` (no expiry), `30`, `60` or `90` (the
default); any other value is a `usage_error` before the config file is read
or any request is sent:

```json
{
  "error": {
    "code": 0,
    "message": "--expires must be 0, 30, 60 or 90, got \"45\"",
    "error_type": "usage_error"
  }
}
```

The login then:

1. sends `GET <hub URL>/api/v1/auth/providers` without an `Authorization`
   header and takes the item whose `name` is `--provider` (`github` by
   default). A provider the hub does not list fails the login, naming the
   ones it does:

   ```json
   {
     "error": {
       "code": 0,
       "message": "the hub does not offer the identity provider \"google\"; it offers github",
       "error_type": "client_error"
     }
   }
   ```

2. listens on `127.0.0.1` on a free port and opens in the browser the
   provider's `authorize_url` with two query parameters added, keeping the
   ones it has: `redirect_uri=http://127.0.0.1:<port>/callback` and `state`,
   32 random hexadecimal characters drawn for this login. The browser is
   opened with `open` on macOS, `xdg-open` on Linux and
   `rundll32 url.dll,FileProtocolHandler` on Windows. When that fails
   (no opener, or one that exits with an error within 5 seconds), the URL is
   written to stderr and the login keeps waiting, so it can be opened by
   hand:

   ```
   Cannot open a browser (exec: "xdg-open": executable file not found in $PATH). Open this URL to sign in:
   https://github.com/login/oauth/authorize?client_id=gh-id&redirect_uri=http%3A%2F%2F127.0.0.1%3A53124%2Fcallback&scope=user%3Aemail&state=9f86d081884c7d659a2feaa0c55ad015
   ```

3. waits up to 120 seconds for the provider to send the browser back to
   `GET /callback` on that port, and serves that one request. A request
   that carries the same `state` (compared in constant time) and a `code`
   is answered `200` with a short HTML page saying the sign-in worked. A
   different or missing `state`, an `error` parameter or a missing `code`
   is answered `400` with a short HTML page saying it failed, and fails the
   login with a `client_error`, without calling the hub. When no request
   comes within 120 seconds the login fails with a `client_error`. The
   listener is closed on every way out.

4. sends `POST <hub URL>/api/v1/auth/callback` without an `Authorization`
   header and with the body
   `{"provider":…,"code":…,"redirect_uri":…,"expires":…}`, the
   `redirect_uri` being the one it gave the provider (see
   [api.md](api.md)), and waits up to 60 seconds for the answer, since the
   hub may itself wait on the identity provider for up to three calls of
   15 seconds each. On a `2xx` answer it saves `endpoint_url`, the new key
   (`api_key.key`) and the user's `id` in the config file, prints the
   `user` alone on stdout, indented by two spaces and followed by a
   newline, writes `Logged in as <username>` to stderr and exits `0`. The
   key is never printed.

When the hub answers either request with an error envelope, for example
`429` on the provider list, or `401` (the provider rejected the code),
`403` (`user is blocked`), `409` or `502` on the callback, `parley login`
writes that envelope to stderr as received, indented by two spaces (see
[Output](#output)), saves nothing and exits `1`.

#### With a credential: `--api-key`

`parley login --api-key <credential>` stores a credential the user already
holds, an API key or a personal access token, without a browser. With
`--api-key -` it reads the credential from stdin, which keeps it out of the
process list. White space around the credential is dropped; an empty one
is a `usage_error`, raised before the config file is read, and sends
nothing. `--provider` and `--expires` have no
effect here.

It sends `GET <hub URL>/api/v1/user` with `Authorization: Bearer
<credential>` and a timeout of 30 seconds, and nothing else, so no key is
created or revoked. When the hub answers a `2xx` status with a JSON object
that has an `id`, it saves the file, prints the User as one JSON document on stdout,
indented by two spaces and followed by a newline, writes
`Logged in as <username>` to stderr and exits `0`:

```json
{
  "id": "0b5c6f2e-8d1a-4c3b-9e7f-2a4d6c8e0f13",
  "username": "alice",
  "email": "alice@example.com",
  "full_name": "Alice Smith",
  "role": "user",
  "status": "active",
  "provider": "github",
  "provider_id": "123",
  "created_at": "2026-11-13T10:00:00Z",
  "updated_at": "2026-11-13T10:00:00Z"
}
```

When the hub answers with an error envelope, for example `401` for a
credential it does not know, or `403` for the admin token, `parley login`
writes that envelope to stderr as received, indented by two spaces, saves
nothing and exits `1`:

```json
{
  "error": {
    "code": 401,
    "message": "invalid credentials",
    "error_type": "unauthenticated"
  }
}
```

#### Failures

Every failure prints nothing on stdout. A failure on the client's side
writes one error envelope with `code` `0` to stderr (see
[Errors and exit statuses](#errors-and-exit-statuses)) and exits `2`; its
`error_type` is `usage_error` for an unknown flag, a flag without its
value, a positional argument, an empty credential and a bad `--expires`,
none of which sends a request, and `client_error` for everything else: a
config file that cannot be read, is not valid TOML (the message gives the
line and column, not the text) or holds a known key with a value that is
not a string, no home directory to find the config file in, no hub URL, a
hub that cannot be reached or does not answer within 30 seconds (60
seconds for the callback), an answer that is neither what the request
expects (on a `2xx` status: a JSON object with an `id`, a provider list, a
callback answer with a key) nor an error envelope holding `error_type`, a provider the hub does not list, a browser that
comes back with the wrong state, an error or no code, or does not come
back within 120 seconds, and a config file that cannot be written after
the hub accepted the credential or issued the key. In that last case the
credential stays valid and is not saved:

```json
{
  "error": {
    "code": 0,
    "message": "cannot reach the hub at http://127.0.0.1:1: dial tcp 127.0.0.1:1: connect: connection refused",
    "error_type": "client_error"
  }
}
{
  "error": {
    "code": 0,
    "message": "the hub accepted the credential, but it was not saved: cannot write the config file /home/alice/.parley/config.toml: permission denied",
    "error_type": "client_error"
  }
}
{
  "error": {
    "code": 0,
    "message": "the hub issued an API key, but it was not saved: cannot write the config file /home/alice/.parley/config.toml: permission denied",
    "error_type": "client_error"
  }
}
{
  "error": {
    "code": 0,
    "message": "the sign-in failed: the browser came back without the state this login sent",
    "error_type": "client_error"
  }
}
{
  "error": {
    "code": 0,
    "message": "the browser did not come back from the sign-in within 2m0s",
    "error_type": "client_error"
  }
}
```

Nothing is saved on any failure: the config file is neither created nor
changed.

### `parley user`, `parley orgs`, `parley admin users` and `parley admin orgs`

These commands each call one route of the hub's user and organization API
(see [api.md](api.md)): they send one request with the credential as
`Authorization: Bearer` and print the hub's answer as described under
[Output](#output). A `2xx` answer without a body, such as the `204` of
`parley admin users delete`, `parley admin orgs delete`,
`parley admin orgs members add` and `parley admin orgs members remove`,
prints `{}` followed by a newline, and nothing on stderr. An error the hub answers is
written to stderr as received, with exit status `1`; for example, demoting
the only active admin:

```json
{
  "error": {
    "code": 409,
    "message": "the user is the only active admin",
    "error_type": "conflict"
  }
}
```

The commands of `parley user` and `parley orgs` act as the caller's own
user; the commands of `parley admin` call the
[admin routes](api.md#admin-routes). A group run without a command, such as
`parley admin`, `parley admin users`, `parley admin orgs` or
`parley admin orgs members`, prints its help and exits `0`.

| Command | Request |
|---|---|
| `parley user show` | `GET /api/v1/user` |
| `parley user update --full-name <name>` | `PATCH /api/v1/user` with `{"full_name": <name>}`; `--full-name ""` sends the empty string, which clears the name |
| `parley orgs list` | `GET /api/v1/user/orgs` |
| `parley orgs show <org>` | `GET /api/v1/orgs/<org>` |
| `parley orgs members <org>` | `GET /api/v1/orgs/<org>/members` |
| `parley admin users list [--include-blocked]` | `GET /api/v1/users`; with `--include-blocked`, the query `include_blocked=true` |
| `parley admin users show <user>` | `GET /api/v1/users/<user>` |
| `parley admin users create --username <u> --email <e> --provider <p> --provider-id <id> [--full-name <name>]` | `POST /api/v1/users` with the members `username`, `email`, `provider` and `provider_id`, and `full_name` only when `--full-name` is given |
| `parley admin users update <user> --full-name <name>` | `PATCH /api/v1/users/<user>` with `{"full_name": <name>}` |
| `parley admin users delete <user>` | `DELETE /api/v1/users/<user>` |
| `parley admin users promote <user>` | `POST /api/v1/users/<user>/promote`, without a body |
| `parley admin users demote <user>` | `POST /api/v1/users/<user>/demote`, without a body |
| `parley admin users block <user>` | `POST /api/v1/users/<user>/block`, without a body |
| `parley admin users unblock <user>` | `POST /api/v1/users/<user>/unblock`, without a body |
| `parley admin orgs list [--include-blocked]` | `GET /api/v1/orgs`; with `--include-blocked`, the query `include_blocked=true` |
| `parley admin orgs create --name <name> --slug <slug> [--url <url>] [--owner <user id>]` | `POST /api/v1/orgs` with the members `name` and `slug`, `url` only when `--url` is given and `owner_id` only when `--owner` is given; `--owner` takes a user's id, not a selector |
| `parley admin orgs update <org> [--name <name>] [--url <url>]` | `PATCH /api/v1/orgs/<org>` with only the members whose flags are given; `--url ""` sends the empty string, which clears the URL |
| `parley admin orgs delete <org>` | `DELETE /api/v1/orgs/<org>` |
| `parley admin orgs block <org>` | `POST /api/v1/orgs/<org>/block`, without a body |
| `parley admin orgs unblock <org>` | `POST /api/v1/orgs/<org>/unblock`, without a body |
| `parley admin orgs members list <org>` | `GET /api/v1/orgs/<org>/members` |
| `parley admin orgs members add <org> <user>` | `PUT /api/v1/orgs/<org>/members/<user>`, without a body |
| `parley admin orgs members remove <org> <user>` | `DELETE /api/v1/orgs/<org>/members/<user>` |

`<user>` is a user's id, e-mail or username (see
[User selectors](api.md#user-selectors)) and `<org>` an organization's id
or slug (see [Org selectors](api.md#org-selectors)). Each is sent as one
path segment, percent-encoded: `a/b` is sent as `a%2Fb`, and `?`, `#`, `%`
and spaces are encoded too, so the hub reads the argument exactly as given.

The list commands print one page of the hub's [list](api.md#lists), with
the hub's default `limit`; they send no `limit` or `cursor`.

A command run without one of its required flags (`--full-name` on
`parley user update` and `parley admin users update`; `--username`,
`--email`, `--provider` and `--provider-id` on `parley admin users create`;
`--name` and `--slug` on `parley admin orgs create`), `parley admin orgs
update` given neither `--name` nor `--url`, or a command run with a number
of arguments other than the ones it takes, is a `usage_error` with exit
status `2`, and sends no request:

```json
{
  "error": {
    "code": 0,
    "message": "parley user update needs the flag --full-name",
    "error_type": "usage_error"
  }
}
```

### `parley keys` and `parley admin keys`

These commands call the [API key routes](api.md#get-apiv1userkeys) of the
hub and print its answer as described under [Output](#output); an error the
hub answers is written to stderr as received, with exit status `1`. A group
run without a command, `parley keys` or `parley admin keys`, prints its help
and exits `0`.

| Command | Request |
|---|---|
| `parley keys list` | `GET /api/v1/user/keys` |
| `parley keys refresh` | `POST /api/v1/user/keys/<key_id>/refresh`, without a body |
| `parley keys revoke` | `DELETE /api/v1/user/keys/<key_id>` |
| `parley admin keys list <user>` | `GET /api/v1/users/<user>/keys` |
| `parley admin keys revoke <user> <key_id>` | `DELETE /api/v1/users/<user>/keys/<key_id>` |

`parley admin keys` takes `<user>` as the other admin commands do (see
[User selectors](api.md#user-selectors)), and sends `<user>` and `<key_id>`
each as one percent-encoded path segment: `a/b` is sent as `a%2Fb`. Run with
a number of arguments other than the ones it takes, a command is a
`usage_error` with exit status `2` and sends no request.

`parley keys refresh` and `parley keys revoke` act on the API key the command
presents, the credential resolved as described under
[Settings](#settings-the-hub-url-and-the-credential), and take its
`<key_id>` from it: an API key is `parley_<key id>_<secret>`, an 8-character
key id and a 32-character secret over `0-9`, `A-Z` and `a-z`. Any other
credential, a personal access token or the admin token among them, is a
`client_error` with exit status `2`, and the command sends no request:

```json
{
  "error": {
    "code": 0,
    "message": "parley keys refresh needs an API key (parley_<key id>_<secret>) as the credential, and the credential given is not one",
    "error_type": "client_error"
  }
}
```

`parley keys refresh` prints the hub's answer, `{"key", "key_id",
"expires_at"}`, writes the show-once warning on stderr

```
warning: this value is shown once and will not be shown again
```

and then saves the new `key` as `api_key` in the config file, keeping
`endpoint_url`, `user_id` and every other key of the file, with mode `0600`;
it exits `0`. The old key stops working as soon as the hub answers, so if
the answer is lost on the way, the new key is lost with it: sign in again
with `parley login`. When the config file does not exist (the credential
came from `--api-key` or `PARLEY_API_KEY`), holds an `api_key` other than
the refreshed key (the credential came from `--api-key` or `PARLEY_API_KEY`,
and the file's own key, which the refresh did not touch, is left in place),
or cannot be written, the command creates and changes no file, and after the
printed answer it writes on stderr

```
warning: the new key was not saved; it is printed above, and parley login --api-key - saves it
```

followed by a `client_error` envelope whose message begins with `the new key
was not saved: ` and gives the reason, for example `there is no config file
/home/alice/.parley/config.toml` or `the config file
/home/alice/.parley/config.toml holds another API key`; the exit status is
`2`.

`parley keys revoke` prints the revoked key the hub answers, then, when the
config file exists and its `api_key` is the revoked key (or empty), sets its
`api_key` and `user_id` to the empty string, keeping `endpoint_url` and
every other key, and writes on stderr

```
warning: the API key is revoked; run parley login to sign in again
```

It exits `0`; without a config file it writes no file and still writes the
warning.
When the config file holds another API key (the revoked key came from
`--api-key` or `PARLEY_API_KEY`), the file is left as it was, since its key
still works, nothing is written on stderr, and the exit status is `0`. When
the hub answers an error, the error is written to stderr as received, the
exit status is `1`, and the config file is left as it was.

### `parley tokens` and `parley admin tokens`

These commands call the [personal access token routes](api.md#post-apiv1usertokens)
of the hub and print its answer as described under [Output](#output); an
error the hub answers is written to stderr as received, with exit status
`1`. A group run without a command, `parley tokens` or
`parley admin tokens`, prints its help and exits `0`.

| Command | Request |
|---|---|
| `parley tokens list` | `GET /api/v1/user/tokens` |
| `parley tokens list --workspace <slug>` | `GET /api/v1/workspaces/<slug>/tokens` |
| `parley tokens create --name <name> --permissions <scopes> [--paths <globs>]... [--expires <days>]` | `POST /api/v1/user/tokens` with the members `name`, `permissions` and `expires`, and `paths` only when `--paths` is given |
| `parley tokens create --name <name> --permissions <scopes> [--paths <globs>]... [--expires <days>] --workspace <slug>` | `POST /api/v1/workspaces/<slug>/tokens` with the same body |
| `parley tokens show <token_id>` | `GET /api/v1/user/tokens/<token_id>` |
| `parley tokens revoke <token_id>` | `DELETE /api/v1/user/tokens/<token_id>` |
| `parley tokens replace <token_id> --permissions <scopes>` | `PUT /api/v1/user/tokens/<token_id>/permissions` with `{"permissions": [...]}` |
| `parley tokens add <token_id> --permissions <scopes>` | `PATCH /api/v1/user/tokens/<token_id>/permissions` with `{"permissions": [...]}` |
| `parley tokens remove <token_id> --permissions <scopes>` | `DELETE /api/v1/user/tokens/<token_id>/permissions` with `{"permissions": [...]}` |
| `parley admin tokens list <user>` | `GET /api/v1/users/<user>/tokens` |
| `parley admin tokens revoke <user> <token_id>` | `DELETE /api/v1/users/<user>/tokens/<token_id>` |

`--permissions` and `--paths` take lists separated by commas, split at
every comma as given, without quoting, so a glob keeps every other
character; an empty value is the empty list, so `--permissions ""` sends
`[]`. `--paths` may be
given more than once: the lists of all its values are sent as one array,
in the order given, so `--paths 'docs/**,src/*.go' --paths README.md`
sends `["docs/**","src/*.go","README.md"]`. `--expires` is the lifetime in
days, an integer, `90` when the flag is not given. The client sends scope
names, globs and lifetimes as given and leaves them to the hub to judge.
For example,

```
parley tokens create --name ci --permissions git:read,changes:write --paths 'docs/**' --expires 30
```

sends

```json
{"expires":30,"name":"ci","paths":["docs/**"],"permissions":["git:read","changes:write"]}
```

`parley tokens create` prints the hub's answer, the Token with its `token`,
and writes the show-once warning on stderr:

```
warning: this value is shown once and will not be shown again
```

`parley tokens replace` and `parley tokens remove` print the Token after
the change; when its `revoked_at` is not `null`, because the change left
the token without scopes, they then write on stderr

```
warning: the token was revoked: it has no permissions left
```

and exit `0`. `parley tokens add` writes no such note.

`--workspace <slug>` on `parley tokens create` and `parley tokens list`
turns them to the tokens bound to the workspace `<slug>` (see
[Workspace-bound tokens](api.md#workspace-bound-tokens)): `create` sends its
body, built from the same flags, to
[`POST /api/v1/workspaces/<slug>/tokens`](api.md#post-apiv1workspacesslugtokens)
in place of `POST /api/v1/user/tokens`, and prints the bound Token with its
`token` and the show-once warning on stderr, as above; `list` sends
[`GET /api/v1/workspaces/<slug>/tokens`](api.md#get-apiv1workspacesslugtokens)
in place of `GET /api/v1/user/tokens` and prints the hub's list, whose
Tokens never carry a `token`. The slug is sent as one percent-encoded path
segment: `--workspace a/b` is sent as `/api/v1/workspaces/a%2Fb/tokens`. For
example,

```
parley tokens create --name agent --permissions git:read --workspace w1
```

sends `{"expires":90,"name":"agent","permissions":["git:read"]}` to
`POST /api/v1/workspaces/w1/tokens`. An error the hub answers, such as
`403 token_bound` for a bound token or `409 workspace_archived`, is written
to stderr as received with exit status `1`, and nothing is printed on
stdout. Without `--workspace` both commands call the `/api/v1/user/tokens`
routes.

`parley admin tokens` takes `<user>` as the other admin commands do (see
[User selectors](api.md#user-selectors)). Every command sends `<user>` and
`<token_id>` each as one percent-encoded path segment: `a/b` is sent as
`a%2Fb`.

A command run without one of its required flags (`--name` and
`--permissions` on `parley tokens create`; `--permissions` on `replace`,
`add` and `remove`), with a number of arguments other than the ones it
takes, with an `--expires` that is not an integer, or with `--workspace`
given an empty value on `parley tokens create` or `parley tokens list` is
a `usage_error` with exit status `2`, and sends no request, so an empty
slug never falls back to an unbound token. The empty `--workspace` reads
`parley tokens list needs a workspace slug as the value of --workspace`
(`create` in place of `list` for the other command); an `--expires` that is
not an integer reads:

```json
{
  "error": {
    "code": 0,
    "message": "invalid argument \"soon\" for \"--expires\" flag: strconv.ParseInt: parsing \"soon\": invalid syntax",
    "error_type": "usage_error"
  }
}
```

### `parley sandbox`

`parley sandbox profiles` calls `GET /api/v1/sandbox-profiles` once (see
[api.md](api.md#get-apiv1sandbox-profiles)) with the credential as
`Authorization: Bearer`, prints the hub's answer on stdout as described
under [Output](#output), and exits `0`. The list is one page, in name order,
and the command has no paging flags. An error the hub answers is written to
stderr as received, with exit status `1`; on a hub without sandboxes:

```json
{
  "error": {
    "code": 404,
    "message": "sandboxes are not enabled on this hub",
    "error_type": "sandboxes_disabled"
  }
}
```

| Command | Request |
|---|---|
| `parley sandbox profiles` | `GET /api/v1/sandbox-profiles` |
| `parley sandbox create <workspace-slug> [flags] [-- command...]` | `POST /api/v1/workspaces/<slug>/sandboxes` |
| `parley sandbox list <workspace-slug> [flags]` | `GET /api/v1/workspaces/<slug>/sandboxes` |
| `parley sandbox get <workspace-slug> <id>` | `GET /api/v1/workspaces/<slug>/sandboxes/<id>` |
| `parley sandbox exec <workspace-slug> <id> [flags] -- <command...>` | `POST /api/v1/workspaces/<slug>/sandboxes/<id>/exec` |
| `parley sandbox shell <workspace-slug> <id>` | `GET /api/v1/workspaces/<slug>/sandboxes/<id>/attach` (WebSocket) |
| `parley sandbox logs <workspace-slug> <id> [flags]` | `GET /api/v1/workspaces/<slug>/sandboxes/<id>/logs` |
| `parley sandbox upload <workspace-slug> <id> <local-path> <sandbox-path>` | `PUT /api/v1/workspaces/<slug>/sandboxes/<id>/files` |
| `parley sandbox download <workspace-slug> <id> <sandbox-path> <local-path>` | `GET /api/v1/workspaces/<slug>/sandboxes/<id>/files` |
| `parley sandbox checkpoint <workspace-slug> <id>` | `POST /api/v1/workspaces/<slug>/sandboxes/<id>/checkpoint` |
| `parley sandbox stop <workspace-slug> <id>` | `POST /api/v1/workspaces/<slug>/sandboxes/<id>/stop` |
| `parley sandbox start <workspace-slug> <id> [--wait]` | `POST /api/v1/workspaces/<slug>/sandboxes/<id>/start` |
| `parley sandbox extend <workspace-slug> <id> --ttl <duration>` | `PATCH /api/v1/workspaces/<slug>/sandboxes/<id>` |
| `parley sandbox delete <workspace-slug> <id> [--no-checkpoint]` | `DELETE /api/v1/workspaces/<slug>/sandboxes/<id>` |
| `parley sandbox proposals <workspace-slug> <id> [--state <state>]` | `GET /api/v1/workspaces/<slug>/sandboxes/<id>/proposals` |
| `parley sandbox approve <workspace-slug> <id> <proposal-id> [flags]` | `POST /api/v1/workspaces/<slug>/sandboxes/<id>/proposals/<proposal-id>/approve` |
| `parley sandbox reject <workspace-slug> <id> <proposal-id> [--comment <text>]` | `POST /api/v1/workspaces/<slug>/sandboxes/<id>/proposals/<proposal-id>/reject` |
| `parley sandbox rules <workspace-slug> [--limit <n>] [--cursor <cursor>]` | `GET /api/v1/workspaces/<slug>/sandbox-rules` |
| `parley sandbox rule-delete <workspace-slug> <rule-id>` | `DELETE /api/v1/workspaces/<slug>/sandbox-rules/<rule-id>` |

`parley sandbox` run without a command prints its help and exits `0`; it
reads no credential and sends no request. The slug and the id are sent as
one path segment each, percent-encoded (`a/b` becomes `a%2Fb`).

#### `parley sandbox create`

```
parley sandbox create <workspace-slug> [--profile <name>] [--env key=value]... [--label key=value]...
                      [--no-checkout | [--ref <ref>] [--branch <branch>]] [--restore-from <id>]
                      [--ttl <duration>] [--purpose <text>] [--session <id>]
                      [--wait | --wait-exit] [--timeout <duration>] [--poll-interval <duration>]
                      [-- command...]
```

Sends [`POST /api/v1/workspaces/<slug>/sandboxes`](api.md#post-apiv1workspacesslugsandboxes)
with a JSON body that holds exactly the members its flags name, and prints
the hub's answer, the sandbox object. Without a flag the body is `{}`, and
the hub's defaults apply.

| Flag | Body member | Value |
|---|---|---|
| `--profile <name>` | `profile` | the string as given |
| `--env key=value` | `env` | repeatable; an object of every pair, each split at its first `=` (`--env A=b=c` sends `{"A":"b=c"}`) |
| `--label key=value` | `labels` | repeatable; as `--env` |
| `--no-checkout` | `checkout` | `false` |
| `--ref <ref>`, `--branch <branch>` | `checkout` | `{"ref": …, "branch": …}` with the members given |
| `--restore-from <id>` | `restore_from` | the string as given |
| `--ttl <duration>` | `ttl` | the string as given, such as `1h` |
| `--purpose <text>` | `purpose` | the string as given |
| `--session <id>` | `session_id` | the string as given |
| `-- command...` | `command` | the list of the words after `--` |

`parley sandbox create demo--t1 --profile agent --env CI=1 --label team=core -- make test` sends

```json
{"command":["make","test"],"env":{"CI":"1"},"labels":{"team":"core"},"profile":"agent"}
```

These are usage errors (`usage_error`, exit status `2`), before any
setting is read or request sent: no slug, or more than one word before
`--`; an `--env` or `--label` value without `=` or with an empty key;
`--no-checkout` with `--ref` or `--branch`; `--wait-exit` without a
command; and a `--timeout` or `--poll-interval` the CLI cannot read (see
[Waiting](#waiting)).

**`--wait`.** After the hub answers the create with `201`, the command
polls [`GET /api/v1/workspaces/<slug>/sandboxes/<id>`](api.md#get-apiv1workspacesslugsandboxesid)
every `--poll-interval` until the state is no longer `provisioning`, within
`--timeout`. A sandbox that ends `ready` or `completed` is printed on
stdout, the only document there, with exit status `0`. One that ends
`failed`, `stopped` or `deleted` is printed on stdout too, `wait_failed` is
written to stderr, and the exit status is `1`. Running out of time is
`wait_timeout` with nothing on stdout and exit status `1`.

**`--wait-exit`.** Waits as `--wait` does (it turns `--wait` on), but
until the main process has ended: it keeps polling while the state is
`provisioning` or `ready`, and succeeds only for `completed`; `failed`,
`stopped` and `deleted` print the sandbox and end with `wait_failed`, exit
status `1`.

#### `parley sandbox list`

```
parley sandbox list <workspace-slug> [--state <state>]... [--session <id>] [--label key=value]...
                    [--include-forks] [--limit <n>] [--cursor <cursor>]
```

Sends [`GET /api/v1/workspaces/<slug>/sandboxes`](api.md#get-apiv1workspacesslugsandboxes)
with one `state` and one `label` query parameter per flag given, and
`session_id`, `include_forks=true`, `limit` and `cursor` for the flags
given, and prints the list document. A flag that is not given adds no
parameter, so the hub's defaults apply (every state but `deleted`).

#### `parley sandbox get`

`parley sandbox get <workspace-slug> <id>` sends
[`GET /api/v1/workspaces/<slug>/sandboxes/<id>`](api.md#get-apiv1workspacesslugsandboxesid)
and prints the sandbox object. Any other number of arguments is a usage
error.

#### `parley sandbox checkpoint`, `stop`, `start`, `extend` and `delete`

```
parley sandbox checkpoint <workspace-slug> <id>
parley sandbox stop <workspace-slug> <id>
parley sandbox start <workspace-slug> <id> [--wait [--timeout <duration>] [--poll-interval <duration>]]
parley sandbox extend <workspace-slug> <id> --ttl <duration>
parley sandbox delete <workspace-slug> <id> [--no-checkpoint]
```

Each takes exactly the two arguments and sends one request (see
[api.md](api.md#the-life-of-a-sandbox)); any other number of arguments, and
`extend` without `--ttl`, is a usage error (exit status `2`) and sends
nothing.

- `checkpoint` sends `POST …/sandboxes/<id>/checkpoint` and prints the
  sandbox with its new `checkpoint`.
- `stop` sends `POST …/sandboxes/<id>/stop` and prints the sandbox.
- `start` sends `POST …/sandboxes/<id>/start` and prints the sandbox. With
  `--wait` it then polls `GET …/sandboxes/<id>` through the
  [shared wait](#waiting) until the sandbox is `ready` or `completed` (the
  final sandbox on stdout, exit status `0`) or `failed`, `stopped` or
  `deleted` (the final sandbox on stdout, `wait_failed` on stderr, exit
  status `1`).
- `extend` sends `PATCH …/sandboxes/<id>` with the body `{"ttl":"<duration>"}`
  and prints the sandbox.
- `delete` sends `DELETE …/sandboxes/<id>`, with the query
  `checkpoint=false` when `--no-checkpoint` is given and none otherwise, and
  on the hub's `204` prints `{"id":"<id>","status":"deleted"}`.

An error the hub answers, such as `409 sandbox_deleted` for a start of a
deleted sandbox, is written to stderr as received, with nothing on stdout
and exit status `1`.

#### `parley sandbox proposals`, `approve`, `reject`, `rules` and `rule-delete`

```
parley sandbox proposals <workspace-slug> <id> [--state <state>]
parley sandbox approve <workspace-slug> <id> <proposal-id> [--comment <text>] [--acknowledge-findings] [--scope sandbox|workspace]
parley sandbox reject <workspace-slug> <id> <proposal-id> [--comment <text>]
parley sandbox rules <workspace-slug> [--limit <n>] [--cursor <cursor>]
parley sandbox rule-delete <workspace-slug> <rule-id>
```

They read and decide a sandbox's network-access proposals and the
workspace sandbox rules (see
[api.md](api.md#the-network-policy-of-a-sandbox)). Each sends one request
and prints the hub's answer on stdout:

- `proposals` sends `GET …/sandboxes/<id>/proposals`, with `state` only
  when `--state` is given (the hub then lists the open ones).
- `approve` sends `POST …/proposals/<proposal-id>/approve` with a JSON
  object holding `comment` for `--comment`, `acknowledge_findings: true` for
  `--acknowledge-findings` and `scope` for `--scope`, each only when its
  flag is given, and `{}` when none is; it prints the proposal.
- `reject` sends `POST …/proposals/<proposal-id>/reject` with `comment` for
  `--comment`, else `{}`, and prints the proposal.
- `rules` sends `GET /api/v1/workspaces/<slug>/sandbox-rules`, with `limit`
  and `cursor` only for the flags given.
- `rule-delete` sends `DELETE /api/v1/workspaces/<slug>/sandbox-rules/<rule-id>`
  and on the hub's `204` prints `{"id":"<rule-id>","status":"deleted"}`.

A wrong number of arguments, and a `--scope` other than `sandbox` or
`workspace`, is a usage error (exit status `2`) and sends nothing. An error
the hub answers, such as `409 findings_not_acknowledged`, `429
sandbox_rule_limit_reached` or `404 sandbox_rule_not_found`, is written to
stderr as received, with nothing on stdout and exit status `1`.

#### `parley sandbox exec`

```
parley sandbox exec <workspace-slug> <id> [--workdir <dir>] [--env key=value]... [--timeout <duration>] [--stdin] [--json] -- <command...>
```

Runs the words after `--` as a command in the sandbox through
[`POST /api/v1/workspaces/<slug>/sandboxes/<id>/exec`](api.md#post-apiv1workspacesslugsandboxesidexec),
without the 30-second request timeout: the request lasts until the hub ends
it. The body is `{"command": [...]}` with `workdir`, `env` (from the repeated
`--env`; a value keeps everything after the first `=`) and `timeout` (as
given) only for the flags given; `--stdin` reads standard input to its end and
sends it as `stdin_base64`.

By default it asks for the event stream and writes the command's stdout to
stdout and its stderr to stderr, byte for byte, as the frames arrive; stdout
is not JSON. With `--json` it asks for the collected answer and prints it as
under [Output](#output), read up to 16 MiB (16,777,216 bytes) instead of
8 MiB: the hub keeps up to 1 MiB of each stream and JSON escapes a control
byte to six bytes.

| Exit status | When |
|---|---|
| the command's exit code | the command ran; a code above 125 is `125` |
| `1` | the hub refused the request (its envelope on stderr, as received), or the command timed out: `wait_timeout` on stderr after the output received (with `--json`, after the answer, which holds that output) |
| `2` | a `usage_error`: no command after `--` (`parley sandbox exec needs a command after --`), an `--env` value without `=`; or a `client_error`: standard input over 4,194,304 bytes with `--stdin`, sent nowhere, or a stream that ends without its `exit` frame |

```
$ parley sandbox exec demo--t1 sbx_0123456789abcdef0123456789abcdef -- sh -c 'echo out; echo err >&2; exit 3'
out
err
$ echo $?
3
```

#### `parley sandbox shell`

```
parley sandbox shell <workspace-slug> <id>
```

Opens [`GET /api/v1/workspaces/<slug>/sandboxes/<id>/attach`](api.md#get-apiv1workspacesslugsandboxesidattach)
as a WebSocket (`ws://` or `wss://` from the hub's URL) with the credential as
`Authorization: Bearer` and `cols` and `rows` of the local terminal (80 and 24
when standard input is not a terminal; each at most 1,000). When standard
input is a terminal it is put in raw mode, and restored when the connection
ends, also on an interrupt. Standard input goes to the sandbox's shell in
binary frames, the shell's output comes to stdout, and a change of the local
terminal's size (`SIGWINCH`) sends a resize frame. Exiting the shell ends that
shell only; the sandbox keeps running.

| Exit status | When |
|---|---|
| `0` | the hub closed the connection |
| `1` | the hub refused the upgrade: the request is sent again to read the hub's envelope, which is printed on stderr as received |
| `2` | the hub could not be reached (`client_error`) |

#### `parley sandbox logs`

```
parley sandbox logs <workspace-slug> <id> [--source process|policy] [--since <time>] [--tail <n>] [--follow]
```

Calls [`GET /api/v1/workspaces/<slug>/sandboxes/<id>/logs`](api.md#get-apiv1workspacesslugsandboxesidlogs)
with `source`, `since` and `tail` for the flags given and prints one compact
JSON object per stored line, in the hub's order, with no enclosing document.
Without `--follow` it reads every page, following `next_cursor` until it is
null. With `--follow` it opens the event stream without the 30-second request
timeout, prints each line as it is stored, and exits `0` when the hub ends the
stream (the sandbox was deleted, or the hub stopped) or on an interrupt.

```
{"time":"2026-01-02T03:04:05Z","source":"process","stream":"stdout","line":"token=[redacted]"}
{"time":"2026-01-02T03:04:06Z","source":"policy","stream":null,"line":"deny example.com:443 /usr/bin/curl","action":"deny","program":"/usr/bin/curl","host":"example.com","port":443}
```

A `--source` other than `process` or `policy` and a `--tail` below 1 are
`usage_error`s, exit status `2`, and nothing is sent; a hub error is printed
as received with exit status `1`; a followed stream that breaks is a
`client_error`.

#### `parley sandbox upload`

```
parley sandbox upload <workspace-slug> <id> <local-path> <sandbox-path>
```

Sends [`PUT /api/v1/workspaces/<slug>/sandboxes/<id>/files`](api.md#put-apiv1workspacesslugsandboxesidfiles)
without the 30-second request timeout and prints the hub's answer,
`{path, bytes, entries}`. A local file is the body, with `path=<sandbox-path>`
and its permission bits as `mode` (such as `0750`). A local directory is sent
with `archive=tar` as a tar stream of its directories and regular files,
named relative to it, written while it is sent; a symbolic link (or anything
else) in it is left out with a `warning: ` line on stderr naming it. A local
path that does not exist or cannot be read, or a file in the directory that
cannot be read, is a `client_error`, exit status `2`, and nothing is sent.

#### `parley sandbox download`

```
parley sandbox download <workspace-slug> <id> <sandbox-path> <local-path>
```

Calls [`GET /api/v1/workspaces/<slug>/sandboxes/<id>/files`](api.md#get-apiv1workspacesslugsandboxesidfiles)
with `path=<sandbox-path>`, without the 30-second request timeout. A file is
written to `<local-path>` through a temporary file in the same directory,
renamed once complete (mode `0644`), or to stdout when `<local-path>` is `-`.
When the hub answers `path_is_directory` it asks again with `archive=tar` and
unpacks the stream into the directory `<local-path>`, which it creates, with
the entries' modes. It prints `{"path": "<local-path>", "bytes": <n>}`, the
sum of the file sizes for a directory, unless the bytes went to stdout. A tar
entry whose name is absolute or would land outside `<local-path>`, and a tar
stream that is cut off, are `client_error`s with exit status `2`; any other
hub error is printed as received with exit status `1`, and nothing is left at
`<local-path>`.

### `parley events`

`parley events tail` follows the hub's event stream,
`GET /api/v1/events` (see [api.md](api.md#get-apiv1events)), with the
credential as `Authorization: Bearer` and `Accept: text/event-stream`, and
runs until it is interrupted. `parley events list` reads one page of the
event log, `GET /api/v1/events/log` (see
[api.md](api.md#get-apiv1eventslog)). `parley events` run without a command
prints its help and exits `0`; it reads no credential and sends no request.

Both commands take these flags:

| Flag | Query parameter |
|---|---|
| `--workspace <slug>` | `workspace`: only the events of this workspace |
| `--types <types>` | `types`: only the events of these comma-separated types; an entry ending in `*` matches every type that begins with the text before it |
| `--session <id>` | `session_id`: only the events of this agent session |
| `--after <seq>` | `after`: only the events with a higher `seq`, the position to resume from |

A flag that is given is sent as given, an empty value included, and the
hub judges it; a flag that is not given is not sent, so without `--after`
the request has no `after`: the stream starts with the events committed
after it opens, and the page with the oldest event in the log. `--after`
takes a non-negative integer; any other value (`-1`, `x`, `1.5`) is a
`usage_error` with exit status `2`, and nothing is sent:

```json
{
  "error": {
    "code": 0,
    "message": "invalid argument \"-1\" for \"--after\" flag: want a non-negative integer, such as 4",
    "error_type": "usage_error"
  }
}
```

#### `parley events tail`

The request has no timeout. For each `hub_event` frame the command writes
the frame's `data`, the event record exactly as the hub sent it, followed
by a newline, to stdout, so stdout holds one line of compact JSON per
event in the order received, and nothing else; a heartbeat prints nothing:

```
{"seq":5,"id":"…","type":"parley.change.open","time":"2026-11-14T09:30:20Z","workspace":"api","actor_type":"pat","actor_id":"u_1","resource_type":"change","resource_id":"42","action":"open","session_id":null,"metadata":{}}
{"seq":6,"id":"…","type":"parley.change.open","time":"2026-11-14T09:30:41Z","workspace":"api","actor_type":"pat","actor_id":"u_1","resource_type":"change","resource_id":"43","action":"open","session_id":null,"metadata":{}}
```

The command remembers the `id` of the last frame it received, heartbeats
included, as its resume position. When the stream ends in any other way
than an interrupt (the connection closes or fails, the hub cannot be
reached, or the hub answers a status other than `200`, `400`, `401`, `403`,
`404` and `410`: a `503` when every stream place is taken, a `429`, any
other `5xx`), the command writes one progress line to stderr, waits and
connects again with `after` set to that position (or to `--after`, or
without `after`, when no frame was received yet):

```
reconnecting in 1s: the stream ended
reconnecting in 2s: cannot reach the hub at http://127.0.0.1:8080: dial tcp 127.0.0.1:8080: connect: connection refused
reconnecting in 4s: the hub answered 503
```

The wait is 1 second before the first attempt and twice as long before
each further one, up to 30 seconds (1, 2, 4, 8, 16, 30, 30, … seconds);
`Retry-After` does not change it. After a connection that delivered at
least one frame it is 1 second again. An event is printed only when its
`seq` (the frame's `id`) is higher than that of the last event printed, so
an event the hub sends again after a reconnect is not printed twice.

An interrupt (`SIGINT`, Ctrl-C) ends the stream; the command exits `0`
and writes nothing more. The hub's answers `400`, `401`, `403`, `404` and
`410` end the command without another request: the error envelope is
written to stderr as received, `oldest_seq` of a `410` `cursor_expired`
included, after any progress lines, and the exit status is `1`. Such an
answer without an error envelope is a `client_error` (`the hub answered 404
without an error envelope`) with exit status `2`, and so is a line of the
stream longer than 8 MiB (8388608 bytes), which would come again on every
reconnect.

#### `parley events list`

`parley events list` also takes two flags of its own, each sent only when
given:

| Flag | Query parameter |
|---|---|
| `--limit <n>` | `limit`: the most events the page holds; the hub's default, 50, when not given |
| `--wait <seconds>` | `wait`: how long the hub may hold the request while there is no event to return |

Both take an integer; a value that is not one (`--limit x`) is a
`usage_error` with exit status `2`, and nothing is sent. An integer is sent
as given, also one the hub refuses (`--limit 0`, `--wait 61`), so the hub's
`400` names what is wrong with it.

The command sends exactly one request. Its timeout is 30 seconds plus the
`--wait` seconds (75 seconds with `--wait 45`), so a page the hub holds back
for the whole wait is still received. The hub's page is printed on stdout as
one JSON document indented by two spaces, like every result (see
[Output](#output)), and the exit status is `0`:

```json
{
  "items": [
    {
      "seq": 5,
      "id": "…",
      "type": "parley.change.open",
      "time": "2026-11-14T09:30:20Z",
      "workspace": "api",
      "actor_type": "pat",
      "actor_id": "u_1",
      "resource_type": "change",
      "resource_id": "42",
      "action": "open",
      "session_id": null,
      "metadata": {}
    }
  ],
  "next_cursor": null,
  "next_after": 5,
  "oldest_seq": 1,
  "last_seq": 5
}
```

To read the next page, run the command again with `--after` set to
`next_after`. An error the hub answers (`400` for a parameter it refuses,
`404` for a `workspace` the caller may not read, `410` `cursor_expired` for
a position older than the log) is written to stderr as received,
`oldest_seq` of a `410` included, nothing is written to stdout, and the
exit status is `1` (see [Errors and exit statuses](#errors-and-exit-statuses)).

| Command | Request |
|---|---|
| `parley events tail [--workspace <slug>] [--types <types>] [--session <id>] [--after <seq>]` | `GET /api/v1/events` with the query parameters of the flags given |
| `parley events list [--workspace <slug>] [--types <types>] [--session <id>] [--after <seq>] [--limit <n>] [--wait <seconds>]` | `GET /api/v1/events/log` with the query parameters of the flags given |

### `parley audit`

`parley audit query` reads one page of the audit query, `GET
/api/v1/audit` (see [api.md](api.md#get-apiv1audit)): the hub events and
the agents' run events the credential may see, newest first. It sends one
request with the credential as `Authorization: Bearer` and a query
parameter for each flag given, and prints the hub's page on stdout with
exit status `0`. `parley audit` run without a command prints its help and
exits `0`.

| Flag | Parameter |
|---|---|
| `--source <hub\|agent>` | `source` |
| `--workspace <slug>` | `workspace` |
| `--types <list>` | `types`, comma-separated; an entry ending in `*` matches every type beginning with the text before it |
| `--session <id>` | `session_id` |
| `--run-id <id>` | `run_id` |
| `--actor-id <id>` | `actor_id` |
| `--resource-type <type>` | `resource_type` |
| `--resource-id <id>` | `resource_id` |
| `--action <action>` | `action` |
| `--since <time>`, `--until <time>` | `since`, `until`, RFC 3339 |
| `--limit <n>` | `limit` |
| `--cursor <c>` | `cursor`, the `next_cursor` of the previous page |

A flag not given sends no parameter; a value is sent as given, and one the
hub refuses is its `400` or `404` on stderr as received, with nothing on
stdout and exit status `1`. A positional argument, and a `--limit` that is
not an integer, are usage errors (exit status `2`) before any request.

```sh
parley audit query --workspace demo --types 'parley.change.*,tool.started' --limit 20
```

### `parley secrets`

These commands call the [secret routes](api.md#secrets) of the hub with the
credential as `Authorization: Bearer`. No route answers a secret's value,
so no command prints one. `parley secrets` run without a command prints its
help and exits `0`; it reads no credential and sends no request.

| Command | Request | Prints |
|---|---|---|
| `parley secrets create <KEY=VALUE[,KEY=VALUE…]>…` | `POST /api/v1/<scope>/secrets` with `{"entries":[{"key","value"},…]}`, once per scope flag | `{"results":[{"scope","items"},…]}` |
| `parley secrets create <KEY> --from-stdin` | the same, with the one entry `KEY` and the value read from stdin | the same |
| `parley secrets list` | `GET /api/v1/<scope>/secrets?limit=200`, then every following page | `{"items":[…]}` |
| `parley secrets update <KEY=VALUE>` | `PATCH /api/v1/<scope>/secrets/<KEY>` with `{"value"}` | the hub's answer, the entry |
| `parley secrets update <KEY> --from-stdin` | the same, with the value read from stdin | the same |
| `parley secrets delete <KEY>` | `DELETE /api/v1/<scope>/secrets/<KEY>` | `{"deleted":"<KEY>"}` |

The scope flags choose `<scope>`:

| Flag | Scope |
|---|---|
| `--user` | `user`: your own secrets, `/api/v1/user/secrets`; the scope when no flag is given |
| `--org <slug>` | `org`: the secrets of the organization with this slug or id, `/api/v1/orgs/<slug>/secrets` |
| `--workspace <slug>` | `workspace`: the secrets of the workspace with this slug, `/api/v1/workspaces/<slug>/secrets` |

`list`, `update` and `delete` take at most one scope flag. `create` takes
any of them together and sends one request per scope, in the order `user`,
`org`, `workspace`, each with every entry. The slug and `<KEY>` are each
sent as one percent-encoded path segment: `delete a/b` is sent as
`DELETE /api/v1/user/secrets/a%2Fb`.

**Entries.** The arguments of `create` are joined as if separated by
commas, split at every comma, and each entry is split at its first `=`, so
a value may hold `=` but not a comma: `create A=1,B=x=y C=` sends

```json
{"entries":[{"key":"A","value":"1"},{"key":"B","value":"x=y"},{"key":"C","value":""}]}
```

`update` takes exactly one entry, split the same way. Key names are left to
the hub to judge. A value with a comma, or one that should not appear in the
argument list, is given with `--from-stdin`: the command takes one bare
`KEY` as its argument, reads stdin to its end and drops one trailing line
ending, a line feed or a carriage return and line feed, so `abc\n` and
`abc\r\n` are sent as `abc`, `abc\n\n` as `abc\n`, and commas, `=` and inner
line breaks are kept:

```
printf '%s' "$TOKEN" | parley secrets create GIT_PAT --from-stdin --workspace acme
```

**create.** A scope that fails does not stop the others. stdout holds one
document with the hub's `items` of each scope that succeeded, in the order
`user`, `org`, `workspace`, and `{"results":[]}` when none did:

```json
{
  "results": [
    {
      "scope": "user",
      "items": [
        {
          "key": "GIT_PAT",
          "created_at": "2026-11-14T10:30:15Z",
          "updated_at": "2026-11-14T10:30:15Z"
        }
      ]
    }
  ]
}
```

The error of each scope that failed is written to stderr once, in the same
order, the hub's envelope as received. The exit status is `0` when every
scope succeeded and otherwise that of the last failure: `1` for an error
the hub answered (a `409` `conflict`, a `503` `secrets_unavailable`), `2`
for a failure on the client's side such as a hub that cannot be reached.

**list** reads the whole list: it asks for pages of 200 and follows
`next_cursor` until it is `null`, then prints the items of every page in the
hub's order as one document, without `next_cursor`:

```json
{
  "items": [
    {
      "key": "GIT_PAT",
      "created_at": "2026-11-14T10:30:15Z",
      "updated_at": "2026-11-14T10:30:15Z"
    }
  ]
}
```

**update** prints the hub's answer, the entry without a value; **delete**
prints `{"deleted":"<KEY>"}` with the key as it was typed:

```json
{
  "deleted": "GIT_PAT"
}
```

An error the hub answers to `list`, `update` or `delete` (`404` `not_found`
for a key the scope does not hold, an org or a workspace the caller may not
reach) is written to stderr as received, nothing is written to stdout, and
the exit status is `1`.

A malformed command line is a `usage_error` with exit status `2`, and
nothing is sent: no argument, or for `delete` more than one; an entry
without `=` (`parley secrets create: entry 2 has no =: give KEY=VALUE, or
one KEY with --from-stdin`) or with nothing before it (`entry 1 has no key
before =`); more than one entry for `update`
(`parley secrets update takes one KEY=VALUE, got 2 entries`); with
`--from-stdin`, anything but one non-empty `KEY` without `=` or a comma
(`parley secrets create --from-stdin takes one KEY without = or a comma`);
an empty `<KEY>` for `delete`; more than one scope flag on `list`, `update`
or `delete` (`parley secrets list takes at most one of --user, --org and
--workspace`); and `--org` or `--workspace` with an empty value
(`parley secrets list needs a slug as the value of --org`). A message names
an entry by its position, never by its text, which may hold a value.

### `parley vars`

These commands call the [variable routes](api.md#variables) of the hub with
the credential as `Authorization: Bearer`. Unlike a secret's, a variable's
value is shown. `parley vars` run without a command prints its help and
exits `0`; it reads no credential and sends no request.

| Command | Request | Prints |
|---|---|---|
| `parley vars create <KEY=VALUE[,KEY=VALUE…]>…` | `POST /api/v1/<scope>/vars` with `{"entries":[{"key","value"},…]}`, once per scope flag | `{"results":[{"scope","items"},…]}` |
| `parley vars create <KEY> --from-stdin` | the same, with the one entry `KEY` and the value read from stdin | the same |
| `parley vars list` | `GET /api/v1/<scope>/vars?limit=200`, then every following page | `{"items":[…]}` |
| `parley vars update <KEY=VALUE>` | `PATCH /api/v1/<scope>/vars/<KEY>` with `{"value"}` | the hub's answer, the variable |
| `parley vars update <KEY> --from-stdin` | the same, with the value read from stdin | the same |
| `parley vars delete <KEY>` | `DELETE /api/v1/<scope>/vars/<KEY>` | `{"deleted":"<KEY>"}` |
| `parley vars resolve <WORKSPACE>` | `GET /api/v1/workspaces/<WORKSPACE>/vars/resolved?limit=200`, then every following page | `{"items":[…]}`, each item with its `origin` |

The scope flags `--user`, `--org <slug>` and `--workspace <slug>` choose
`<scope>` as for [`parley secrets`](#parley-secrets): the user's scope when
no flag is given; `list`, `update` and `delete` take at most one; `create`
sends one request per scope in the order `user`, `org`, `workspace`.
`resolve` takes no scope flag. Entries, `--from-stdin`, the percent-encoding
of the slug and `<KEY>`, and the usage errors are those of `parley secrets`,
with `parley vars` in the messages (`parley vars list takes at most one of
--user, --org and --workspace`, `parley vars resolve needs the argument
<WORKSPACE>`).

```
parley vars create LOG_LEVEL=info,REGION=eu --org acme --workspace acme-api
```

**create** prints the hub's `items` of every scope that succeeded, values
included, in the order `user`, `org`, `workspace`, and `{"results":[]}`
when none did; each failed scope's error is written to stderr once, as
received. The exit status is `0` when every scope succeeded and otherwise
that of the last failure: `1` for an error the hub answered, `2` for a
failure on the client's side such as a hub that cannot be reached.

**list** and **resolve** read the whole list, pages of 200 followed to the
last, and print the items of every page as one document without
`next_cursor`:

```json
{
  "items": [
    {
      "key": "LOG_LEVEL",
      "value": "info",
      "origin": "org",
      "created_at": "2026-11-17T10:30:15Z",
      "updated_at": "2026-11-17T10:30:15Z"
    }
  ]
}
```

**update** prints the hub's answer, the variable with its value; **delete**
prints `{"deleted":"<KEY>"}` with the key as it was typed. An error the hub
answers to `list`, `update`, `delete` or `resolve`, on any page, is written
to stderr as received, nothing is written to stdout, and the exit status is
`1`.

### `parley workspace`

`parley workspace` run without a command prints its help and exits `0`; it
reads no credential and sends no request.

#### `parley workspace create`

```
parley workspace create --slug <slug> --git-url <url> [flags]
```

Registers a git repository as a workspace: it sends one
[`POST /api/v1/workspaces`](api.md#post-apiv1workspaces) with the
credential as `Authorization: Bearer` and prints the hub's answer, the
workspace in `status` `pending`, on stdout. The hub clones the repository
in the background; `--wait` waits for that clone to end (see below).

| Flag | Body member | Value |
|---|---|---|
| `--slug <slug>` | `slug` | required: the workspace's slug |
| `--git-url <url>` | `git_url` | required: `https://<host>/<path>` or `git@<host>:<path>` |
| `--default-branch <name>` | `default_branch` | the branch the workspace treats as its default (default: origin's) |
| `--org <id or slug>` | `org_id` | the organization the workspace is recorded in (default: your personal organization) |
| `--display-name <text>` | `display_name` | the workspace's display name (default: the slug) |
| `--description <text>` | `description` | the workspace's description |
| `--workspace-mode <mode>` | `workspace_mode` | `standard` or `carry_patch` (default: `standard`) |
| `--upstream-url <url>` | `upstream_url` | with `carry_patch`, the upstream project the workspace tracks |
| `--integration-branch <name>` | `integration_branch` | with `carry_patch`, the branch the hub rebuilds as upstream plus the patches |
| `--git-pat <token>` | `git_pat` | a token origin accepts, stored as the workspace secret `GIT_PAT` |
| `--git-username <name>` | `git_username` | with `--git-password`, the user name origin accepts, stored as `GIT_USERNAME` |
| `--git-password <password>` | `git_password` | with `--git-username`, the password origin accepts, stored as `GIT_PASSWORD` |
| `--setting <key=value>` | `settings` | repeatable: one workspace setting each (see below) |
| `--wait`, `--timeout`, `--poll-interval` | | see [Waiting](#waiting) |

The body holds a member for each flag given and none for a flag not given,
so the hub applies its own default to what is left out. A flag given with
an empty value is sent with the empty string, which the hub judges
(`--description ""` sends `"description":""`):

```
parley workspace create --slug demo --git-url https://example.com/a.git
```

sends

```json
{"slug":"demo","git_url":"https://example.com/a.git"}
```

**Settings.** Each `--setting`, here and on
[`parley workspace update`](#parley-workspace-list-get-and-update), is
split at its first `=` into a key and a
value, and sets `settings[key]` to: `null` when the value is empty, which
keeps the setting's default; for the key `protected_branches`, the list of
the value's comma-separated parts; otherwise `true` or `false` as a boolean
when the value is one of them; otherwise the value as a string. When a key
is given more than once, the last one wins. Which keys exist, and which
values each takes, is the hub's to judge (see
[Workspace settings](api.md#workspace-settings)). `--setting a=true --setting
protected_branches=main,release/* --setting u=https://x/y?a=b --setting
b=1 --setting c=` sends

```json
{"settings":{"a":true,"protected_branches":["main","release/*"],"u":"https://x/y?a=b","b":"1","c":null}}
```

`protected_branches=true` sends `["true"]`, and `protected_branches=` sends
`null`. Without `--setting` the body has no `settings` member.

**Client checks.** These command lines are refused with a `usage_error`
and exit status `2` before any setting is read or any request sent:

- `--slug` or `--git-url` not given (`parley workspace create needs the
  flag --slug`);
- `--git-pat`, `--git-username` or `--git-password` given with an empty
  value (`parley workspace create needs a value for --git-pat`);
- `--git-pat` with `--git-username` or `--git-password`
  (`parley workspace create takes --git-pat or --git-username with
  --git-password, not both`);
- `--git-username` without `--git-password` (`parley workspace create
  needs --git-password with --git-username`), or the other way round
  (`parley workspace create needs --git-username with --git-password`);
- a `--setting` without `=` (`--setting "abc" has no =: give key=value`,
  also for an empty `--setting ""`) or with nothing before it
  (`--setting "=v" has no key before =`);
- a `--timeout` or `--poll-interval` the CLI cannot read (see
  [Waiting](#waiting)).

Every other rule, a slug's form among them, is the hub's: its refusal is
written to stderr as received (`400` `invalid_request`, `400`
`credential_check_failed`, `409` `conflict` for a slug in use), nothing is
written to stdout and the exit status is `1`.

**`--wait`.** After the hub answers the create with `201`, the command
polls `GET /api/v1/workspaces/<slug>`, the slug sent as one percent-encoded
path segment, under the rules of [Waiting](#waiting), reading the
workspace's `status`: `ready` is the successful end, `failed` the
unsuccessful one, and `pending` and `cloning` keep the wait going. The
progress lines read `waiting: pending`, `waiting: cloning`, `waiting:
ready`. On `ready` the final workspace is the only document on stdout and
the exit status is `0`; on `failed` the final workspace, with its
`clone_error`, is printed on stdout, `wait_failed` (`the operation ended in
the state "failed", which is not a success`) is written to stderr and the
exit status is `1`. Running out of time is `wait_timeout` with exit status
`1` and nothing on stdout, and a poll the hub answers with an error (a
`404` for a workspace the caller cannot read) is written to stderr as
received with exit status `1`.

| Command | Request |
|---|---|
| `parley workspace create --slug <slug> --git-url <url> [flags]` | `POST /api/v1/workspaces` with a member for each flag given; with `--wait`, then `GET /api/v1/workspaces/<slug>` until `ready` or `failed` |

#### `parley workspace list`, `get` and `update`

```
parley workspace list [--include-archived] [--include-forks] [--parent <slug>] [--limit <n>] [--cursor <cursor>]
parley workspace get <slug>
parley workspace update <slug> [flags]
```

Each sends one request with the credential as `Authorization: Bearer` and
prints the hub's answer on stdout with exit status `0`; a refusal of the
hub's is written to stderr as received, with nothing on stdout and exit
status `1`. `<slug>` is sent as one percent-encoded path segment:
`parley workspace get a/b` sends `GET /api/v1/workspaces/a%2Fb`.

`parley workspace list` sends
[`GET /api/v1/workspaces`](api.md#get-apiv1workspaces) with one query
parameter for each flag given and none for a flag not given, and prints the
page the hub answers, `{"items": [...], "next_cursor": ...}`, one page per
run. `--include-archived` sends `include_archived=true` and
`--include-forks` `include_forks=true`; `--parent`, `--limit` and
`--cursor` send `parent`, `limit` and `cursor` with their values as given,
which the hub judges. `parley workspace list --include-forks --parent demo
--limit 10 --cursor abc` sends
`GET /api/v1/workspaces?cursor=abc&include_forks=true&limit=10&parent=demo`.

`parley workspace get <slug>` sends
[`GET /api/v1/workspaces/<slug>`](api.md#get-apiv1workspacesslug) and
prints the workspace. A workspace the caller cannot read is the hub's
`404` `not_found`, exit status `1`.

`parley workspace update <slug>` sends
[`PATCH /api/v1/workspaces/<slug>`](api.md#patch-apiv1workspacesslug) with
a JSON body that holds exactly the members its flags name, and prints the
updated workspace:

| Flag | Body member | Value |
|---|---|---|
| `--display-name <text>` | `display_name` | the string as given; `""` is sent as `""` |
| `--description <text>` | `description` | the string as given |
| `--org <id or slug>` | `org_id` | the organization to move the workspace and its forks to |
| `--clear-display-name` | `display_name` | `null`, which the hub makes the slug |
| `--clear-description` | `description` | `null`, which the hub makes `""` |
| `--clear-org` | `org_id` | `null`, which the hub makes the owner's personal organization |
| `--setting <key=value>` | `settings` | repeatable: one workspace setting each, read as for `create` (see above) |

A clear flag wins over its value flag: `--display-name D
--clear-display-name` sends `{"display_name":null}`. `parley workspace
update demo --description "payments service" --setting land.strategy=squash`
sends

```json
{"description":"payments service","settings":{"land.strategy":"squash"}}
```

**Client checks.** An update with none of the flags above
(`parley workspace update needs at least one of --display-name,
--description, --org, --clear-display-name, --clear-description,
--clear-org and --setting`), a `--setting` without `=` or with nothing
before it, a missing `<slug>` and a second argument are refused with a
`usage_error` and exit status `2` before any setting is read or any request
sent. Everything else, an immutable member, an unknown setting key or an
invalid value among them, is the hub's to refuse.

| Command | Request |
|---|---|
| `parley workspace list [flags]` | `GET /api/v1/workspaces` with a query parameter for each flag given |
| `parley workspace get <slug>` | `GET /api/v1/workspaces/<slug>` |
| `parley workspace update <slug> [flags]` | `PATCH /api/v1/workspaces/<slug>` with a member for each flag given |

#### `parley workspace sync`

```
parley workspace sync <slug> [--reset-to-origin] [--fail-on-diverged] [--wait] [--timeout <duration>] [--poll-interval <duration>]
```

Brings a workspace up to date with its origin: it sends one
[`POST /api/v1/workspaces/<slug>/sync`](api.md#post-apiv1workspacesslugsync)
with the credential as `Authorization: Bearer`, no body, and `<slug>` as one
percent-encoded path segment (`parley workspace sync a/b` sends
`POST /api/v1/workspaces/a%2Fb/sync`), and prints the hub's answer, the
workspace with its `branches`, as the one document on stdout with exit
status `0`. The hub fetches origin before it answers, so each request of
this command has a timeout of 3 minutes instead of 30 seconds.

| Flag | Effect |
|---|---|
| `--reset-to-origin` | sends `reset_to_origin=true`, which moves the branches that diverged from origin to origin's head |
| `--fail-on-diverged` | after printing, exits `3` when a branch or a patch diverged (see below) |
| `--wait`, `--timeout`, `--poll-interval` | wait for the rebuild the answer names (see below and [Waiting](#waiting)) |

A refusal of the hub's (`409` `workspace_busy`, `400` `sync_disabled`,
`502` `upstream_error` when origin cannot be fetched, among others) is
written to stderr as received, with nothing on stdout and exit status `1`.
A missing `<slug>`, a second argument and a `--timeout` or
`--poll-interval` the CLI cannot read are refused with a `usage_error` and
exit status `2` before any request.

**`--fail-on-diverged`.** After the answer is printed, the command collects
the `name` of every `branches` entry whose `result` is `diverged` and every
name in `patches_diverged`, which a carry-patch workspace's answer holds.
When there is any, it writes a `condition_not_met` error naming them
(`diverged from origin: exp, patch/a`) to stderr and exits `3`; otherwise
it exits `0`. Without the flag a divergence changes nothing.

**`--wait`.** When the answer has a `rebuild_job_id` that is not `null`,
the command reads the job at once with
`GET /api/v1/workspaces/<slug>/rebuilds/<rebuild_job_id>` and then polls it
under the rules of [Waiting](#waiting), `--timeout` counted from the first
answer about the job, reading its `status`: `completed` is the successful
end, `dead_letter` and `cancelled` the unsuccessful ones, and `queued`,
`running` and `failed` (a job waiting to retry) keep the wait going. When
the wait ends in a terminal state, stdout receives one document: the sync
answer with one more member, `rebuild`, holding the final job record, after
the answer's own members. `completed` exits `0`; `dead_letter` and
`cancelled` write `wait_failed` to stderr and exit `1`, also when
`--fail-on-diverged` is given and a branch diverged. Running out of time is
`wait_timeout` with nothing on stdout and exit status `1`, and a job read
the hub answers with an error is written to stderr as received with exit
status `1`. When the answer has no `rebuild_job_id`, or a `null` one,
`--wait` changes nothing: the answer is printed and no job is read.

| Command | Request |
|---|---|
| `parley workspace sync <slug> [flags]` | `POST /api/v1/workspaces/<slug>/sync`, with `reset_to_origin=true` for `--reset-to-origin`; with `--wait` and a `rebuild_job_id`, then `GET /api/v1/workspaces/<slug>/rebuilds/<id>` until `completed`, `dead_letter` or `cancelled` |

#### `parley workspace push`

```
parley workspace push <slug> <branch> [--force]
```

Pushes one branch of a workspace to the same branch of its origin: it sends
one
[`POST /api/v1/workspaces/<slug>/branches/<branch>/push`](api.md#post-apiv1workspacesslugbranchesnamepush)
with the credential as `Authorization: Bearer`, `<slug>` and `<branch>`
each as one percent-encoded path segment (`parley workspace push demo
fix/42` sends `POST /api/v1/workspaces/demo/branches/fix%2F42/push`), and
always a body, `{"force":false}`, or `{"force":true}` with `--force`. It
prints the hub's answer, with `branch`, `sha`, `remote`, `remote_url` and
`result` (`pushed`, `forced` or `up_to_date`), as the one document on
stdout with exit status `0`. The hub pushes to origin within its own
deadline of 2 minutes before it answers, so the request has a timeout of 3
minutes instead of 30 seconds.

| Flag | Effect |
|---|---|
| `--force` | sends `{"force":true}`: origin's branch is replaced, but only while it is where the hub last saw it |

With `--force` the command first writes one line to stderr, before the
request is sent:

```
warning: origin's branch "main" may be replaced by the hub's
```

A refusal of the hub's (`409` `non_fast_forward`, `409` `origin_moved`,
`403` `protected_branch`, `504` `origin_timeout`, among others) is written
to stderr as received, after the warning of `--force`, with nothing on
stdout and exit status `1`. A missing `<slug>` or `<branch>` and a third
argument are refused with a `usage_error` and exit status `2` before any
request.

| Command | Request |
|---|---|
| `parley workspace push <slug> <branch> [--force]` | `POST /api/v1/workspaces/<slug>/branches/<branch>/push` with `{"force":false}`, or `{"force":true}` for `--force` |

#### `parley workspace archive`, `reactivate`, `reclone` and `delete`

```
parley workspace archive <slug>
parley workspace reactivate <slug> [--wait] [--timeout <duration>] [--poll-interval <duration>]
parley workspace reclone <slug> --confirm [--wait] [--timeout <duration>] [--poll-interval <duration>]
parley workspace delete <slug> --confirm
```

These four commands retire, revive, repair and remove a repository
workspace. Each sends one request with the credential as
`Authorization: Bearer`, no body, and `<slug>` as one percent-encoded path
segment (`parley workspace archive a/b` sends
`POST /api/v1/workspaces/a%2Fb/archive`).

- `archive` sends
  [`POST /api/v1/workspaces/<slug>/archive`](api.md#post-apiv1workspacesslugarchive)
  and prints the hub's answer, the workspace with `status` `archived`, with
  exit status `0`.
- `reactivate` sends
  [`POST /api/v1/workspaces/<slug>/reactivate`](api.md#post-apiv1workspacesslugreactivate)
  and prints the workspace with `status` `pending`.
- `reclone` sends
  [`POST /api/v1/workspaces/<slug>/reclone`](api.md#post-apiv1workspacesslugreclone)
  and prints the workspace with `status` `pending`.
- `delete` sends
  [`DELETE /api/v1/workspaces/<slug>`](api.md#delete-apiv1workspacesslug).
  The hub answers `204` with no body, so the command prints a document that
  names the workspace, with exit status `0`:

  ```json
  {
    "status": "deleted",
    "slug": "demo"
  }
  ```

Before an archive and a reclone the hub offers every branch to origin within
its own deadline of 2 minutes, so the requests of these two commands have a
timeout of 3 minutes instead of 30 seconds. `reactivate` and `delete` keep
the default.

**`--confirm`.** `reclone` throws the hub's copy away, and `delete` removes
the workspace, its forks and everything they own; neither can be undone.
Without `--confirm`, or with `--confirm=false`, they send nothing and exit
`2` with a `usage_error`:

```json
{
  "error": {
    "code": 0,
    "message": "parley workspace delete needs --confirm, because it cannot be undone",
    "error_type": "usage_error"
  }
}
```

**`--wait`.** `reactivate` and `reclone` put the workspace back to
`pending`, and the hub's clone work then makes it `ready` or `failed`. With
`--wait`, once the route has answered `200`, the command polls
`GET /api/v1/workspaces/<slug>` under the rules of [Waiting](#waiting),
reading its `status`: `ready` is the successful end, with the final
workspace on stdout and exit status `0`; `failed` prints the final
workspace, writes `wait_failed` to stderr and exits `1`; running out of time
is `wait_timeout` with nothing on stdout and exit status `1`.

A refusal of the hub's (`409` `workspace_busy`, `409` `workspace_archived`,
`409` `workspace_not_archived`, `409` `has_forks`, `400`
`not_supported_on_fork`, `400` `fork_has_no_origin`, among others) is
written to stderr as received, with nothing on stdout and exit status `1`.
A missing `<slug>` and a second argument are refused with a `usage_error`
and exit status `2` before any request.

| Command | Request |
|---|---|
| `parley workspace archive <slug>` | `POST /api/v1/workspaces/<slug>/archive` |
| `parley workspace reactivate <slug> [--wait]` | `POST /api/v1/workspaces/<slug>/reactivate`; with `--wait`, then `GET /api/v1/workspaces/<slug>` until `ready` or `failed` |
| `parley workspace reclone <slug> --confirm [--wait]` | `POST /api/v1/workspaces/<slug>/reclone`; with `--wait`, then `GET /api/v1/workspaces/<slug>` until `ready` or `failed` |
| `parley workspace delete <slug> --confirm` | `DELETE /api/v1/workspaces/<slug>` |

#### `parley workspace patch-status`

```
parley workspace patch-status <slug>
```

Sends [`GET /api/v1/workspaces/<slug>/patch-status`](api.md#get-apiv1workspacesslugpatch-status)
with the credential as `Authorization: Bearer` and `<slug>` as one
percent-encoded path segment, and prints the hub's answer, the summary of a
carry-patch workspace, on stdout with exit status `0`. The hub builds it
from what it holds, so it answers while origin and upstream are down. A
refusal of the hub's (`400` `workspace_mode_mismatch` for a workspace not
in mode `carry_patch`, among others) is written to stderr as received, with
nothing on stdout and exit status `1`; a wrong number of arguments is a
`usage_error` with exit status `2`, before any request is sent.

### `parley fork`

`parley fork` run without a command prints its help and exits `0`; it reads
no credential and sends no request. Its commands create a task fork of a
workspace, list a workspace's forks, and give a fork a new lifetime or
delete it. Each sends one request with the credential as
`Authorization: Bearer`, `<parent>` or `<slug>` as one percent-encoded path
segment (`parley fork list a/b` sends `GET /api/v1/workspaces/a%2Fb/forks`),
and prints the hub's answer on stdout with exit status `0`; a refusal of the
hub's is written to stderr as received, with nothing on stdout and exit
status `1`.

| Command | Request |
|---|---|
| `parley fork create <parent> [flags] [-- command...]` | `POST /api/v1/workspaces/<parent>/forks` with a member for each flag given |
| `parley fork list <parent> [flags]` | `GET /api/v1/workspaces/<parent>/forks` with a query parameter for each flag given |
| `parley fork extend <slug> --ttl <duration>` | `PATCH /api/v1/workspaces/<slug>` with `{"ttl": "<duration>"}` |
| `parley fork delete <slug>` | `DELETE /api/v1/workspaces/<slug>` |

#### `parley fork create`

```
parley fork create <parent> [--name <name>] [--ref <branch>] [--ttl <duration>] [--no-auto-change]
                            [--purpose <text>] [--session <id>]
                            [--token-permissions <scopes> [--token-paths <globs>]]
                            [--sandbox-profile <profile> [-- command...]]
```

Sends [`POST /api/v1/workspaces/<parent>/forks`](api.md#post-apiv1workspacesslugforks)
with a JSON body that holds exactly the members its flags name, and prints
the hub's answer, `{"workspace": ..., "remote": ...}` with `token` and
`sandbox` when they were asked for. Without a flag the body is `{}`, and the
hub's defaults apply: a generated name, the parent's default branch, the
hub's `[forks].default_ttl`, `auto_change` true.

| Flag | Body member | Value |
|---|---|---|
| `--name <name>` | `name` | the string as given; the fork's slug is `<parent>--<name>` |
| `--ref <branch>` | `ref` | the string as given |
| `--ttl <duration>` | `ttl` | the string as given, such as `2h` or `7d` |
| `--no-auto-change` | `auto_change` | `false` |
| `--purpose <text>` | `purpose` | the string as given |
| `--session <id>` | `session_id` | the string as given |
| `--token-permissions <scopes>` | `token.permissions` | the list of its parts split at every comma |
| `--token-paths <globs>` | `token.paths` | the list of its parts split at every comma; only with `--token-permissions` |
| `--sandbox-profile <profile>` | `sandbox.profile` | the string as given |
| `-- command...` | `sandbox.command` | the list of the words after `--`; only with `--sandbox-profile` |

A flag given with an empty value is sent empty, for the hub to judge.
`parley fork create demo --name t1 --token-permissions git:read,git:write
--token-paths 'src/**,docs/**' --sandbox-profile default -- make test`
sends

```json
{"name":"t1","sandbox":{"command":["make","test"],"profile":"default"},"token":{"paths":["src/**","docs/**"],"permissions":["git:read","git:write"]}}
```

When the answer holds the value of the token minted with the fork
(`token.token`), the value is printed on stdout with the rest of the answer,
and stderr gets one line that does not hold it:

```
warning: the token's value is shown once and will not be shown again
```

Without a token in the answer stderr stays empty.

**Client checks.** A missing `<parent>`, a second argument before `--`,
`--token-paths` without `--token-permissions` (`parley fork create needs
--token-permissions with --token-paths`) and words after `--` without
`--sandbox-profile` (`parley fork create needs --sandbox-profile with a
command after --`) are refused with a `usage_error` and exit status `2`
before any setting is read or any request sent. Everything else, a name,
ref, ttl, scope or profile the hub does not accept among them, is the hub's
to refuse.

#### `parley fork list`

```
parley fork list <parent> [--mine] [--session <id>] [--limit <n>] [--cursor <cursor>]
```

Sends [`GET /api/v1/workspaces/<parent>/forks`](api.md#get-apiv1workspacesslugforks)
with one query parameter for each flag given and none for a flag not given,
and prints the page the hub answers, `{"items": [...], "next_cursor": ...}`,
one page per run. `--mine` sends `owner=me`; `--session`, `--limit` and
`--cursor` send `session_id`, `limit` and `cursor` with their values as
given, which the hub judges. `parley fork list demo --session <id> --limit
10 --cursor abc` sends
`GET /api/v1/workspaces/demo/forks?cursor=abc&limit=10&session_id=<id>`. A
missing `<parent>` and a second argument are refused with a `usage_error`
and exit status `2` before any request.

#### `parley fork extend` and `parley fork delete`

```
parley fork extend <slug> --ttl <duration>
parley fork delete <slug>
```

`parley fork extend` sends
[`PATCH /api/v1/workspaces/<slug>`](api.md#patch-apiv1workspacesslug) with
the body `{"ttl": "<duration>"}` and prints the fork the hub answers, whose
`expires_at` is now the hub's time of the request plus the duration, longer
or shorter than before. The value of `--ttl` is sent as given: its form and
its bounds, at least `5m` and at most the hub's `[forks].max_ttl`, are the
hub's to judge, and its `400` `invalid_request` (such as
`ttl: must be at least 5m`) is printed as received.

`parley fork delete` sends
[`DELETE /api/v1/workspaces/<slug>`](api.md#delete-apiv1workspacesslug) with
no body. A fork is disposable, so the command takes no `--confirm` and the
hub needs no archive first: it removes the fork, its own branches, its
secrets and the tokens bound to it. The hub answers `204` with no body, and
the command prints a document that names the fork deleted, with exit status
`0`:

```json
{
  "status": "deleted",
  "slug": "demo--t1"
}
```

A refusal of the hub's (`409` `parent_archived` for an extend of a fork
whose parent is archived, `400` `invalid_request` for a ttl it refuses,
`404` `not_found`, `409` `workspace_busy` while the parent is cloning or
under maintenance, among others) is written to stderr as received, with
nothing on stdout and exit status `1`.

**Client checks.** Both commands take only the slug of a task fork,
`<parent>--<name>`. A `<slug>` without `--` (`parley fork delete takes the
slug of a task fork, <parent>--<name>, got "demo"`), a missing `<slug>`, a
second argument, and for `extend` a missing `--ttl` (`parley fork extend
needs the flag --ttl`) or an empty one (`parley fork extend needs a value for
--ttl`) are refused with a `usage_error` and exit status `2` before any
setting is read or any request sent. `--confirm` is an unknown flag of
`parley fork delete`, refused the same way.

### `parley change`

`parley change` run without a command prints its help and exits `0`; it
reads no credential and sends no request. Its twenty-five commands open,
read, edit, abandon, restore, restack, resolve, land, revert and wait for the
changes of a workspace, read their stacks, conflicts and land attempts,
and record and read the verdicts and check results on their versions,
through the
[change routes](api.md#changes), the [land routes](api.md#landing) and
the [approval and check routes](api.md#approvals-and-checks) of the hub. Each sends one request (`edit` with a label flag two, `wait` and
`land --wait` one more per poll, see below) with the credential as
`Authorization: Bearer` and `<slug>` (and the `<sha>` of `find`) as one
percent-encoded path segment (`parley change open a/b --source feat` sends
`POST /api/v1/workspaces/a%2Fb/changes`).

| Command | Request | Flags sent as |
|---|---|---|
| `parley change open <slug> --source <branch> [--source-workspace <slug>] [--target <branch>] [--title <t>] [--intent <i>\|@<file>\|-] [--draft] [--depends-on <n>] [--auto-land] [--label <l>]... [--session <id>]` | `POST …/changes` | the body below |
| `parley change list <slug> [--state <s>]... [--target <branch>] [--author <id>\|me] [--session <id>] [--label <l>] [--path <glob>] [--mergeable <m>] [--landing-status <s>] [--stack-of <n>] [--limit <n>] [--cursor <c>]` | `GET …/changes` | one `state` per `--state`, `--target` `target_branch`, `author`, `--session` `session_id`, `label`, `path`, `mergeable`, `--landing-status` `landing_status`, `--stack-of` `stack_of`, `limit`, `cursor` |
| `parley change show <slug> <number> [--include <list>]` | `GET …/changes/<number>` | `include`, as given |
| `parley change find <slug> <sha>` | `GET …/changes/by-commit/<sha>` | |
| `parley change versions <slug> <number>` | `GET …/changes/<number>/versions` | |
| `parley change stack <slug> <number>` | `GET …/changes/<number>/stack` | |
| `parley change overlaps <slug> <number> [--fail-on-overlap]` | `GET …/changes/<number>/overlaps` | |
| `parley change diff <slug> <number> [--version <n>\|latest] [--against <n>] [--path <glob>]... [--patch \| --name-only]` | `GET …/changes/<number>/diff` | `version`, `against`, one `path` per `--path`, `--patch` `format=patch`, `--name-only` `patches=false` |
| `parley change edit <slug> <number> [--title <t>] [--intent <i>\|@<file>\|-] [--label <l>]... [--remove-label <l>]... [--auto-land \| --no-auto-land] [--target <branch>]` | `PATCH …/changes/<number>` | the body below |
| `parley change ready <slug> <number>` | `PATCH …/changes/<number>` | the body `{"state":"open"}` |
| `parley change draft <slug> <number>` | `PATCH …/changes/<number>` | the body `{"state":"draft"}` |
| `parley change abandon <slug> <number> [--reason <text>] [--dependants reparent\|abandon]` | `POST …/changes/<number>/abandon` | `reason` and `dependants`, as given; `{}` without a flag |
| `parley change restore <slug> <number>` | `POST …/changes/<number>/restore` | no body |
| `parley change restack <slug> <number>` | `POST …/changes/<number>/restack` | the body `{}` |
| `parley change conflict <slug> <number> [--fail-on-conflict \| --file <path>]` | `GET …/changes/<number>/conflict`; with `--file`, `GET …/changes/<number>/conflict/file` | with `--file`, `path` alone, percent-encoded |
| `parley change resolve <slug> <number> [--file <repo-path>=<local-file>]... [--delete <repo-path>]...` | `POST …/changes/<number>/restack` | the body below |
| `parley change land <slug> <number> [--stack] [--expected-version <n>] [--override <reason>] [--session <id>] [--wait] [--timeout <duration>] [--poll-interval <duration>]` | `POST …/changes/<number>/land`; with `--wait`, then `GET …/lands/<id>` once per poll | the body below |
| `parley change lands <slug> <number>` | `GET …/changes/<number>/lands` | |
| `parley change revert <slug> <number> [--title <t>] [--intent <i>] [--draft] [--auto-land] [--session <id>]` | `POST …/changes/<number>/revert` | the body below |
| `parley change wait <slug> <number> [--for landed\|ready\|mergeable] [--timeout <duration>] [--poll-interval <duration>]` | `GET …/changes/<number>`, once per poll | |
| `parley change approve <slug> <number> [--version <n>] [--comment <text>]` | `POST …/changes/<number>/approvals` | the body `{"verdict":"approve"}` with `version` (a number) and `comment` when given |
| `parley change reject <slug> <number> --comment <text> [--version <n>]` | `POST …/changes/<number>/approvals` | the body `{"verdict":"reject","comment":…}` with `version` when given |
| `parley change approvals <slug> <number> [--version <n> \| --withdraw <id>]` | `GET …/changes/<number>/approvals`; with `--withdraw`, `DELETE …/changes/<number>/approvals/<id>` | `version` |
| `parley change check <slug> <number> <name> --status pending\|passed\|failed [--version <n>] [--summary <text>] [--url <url>]` | `PUT …/changes/<number>/checks/<name>` | the body `{"status":…}` with `version` (a number), `summary` and `url` when given |
| `parley change checks <slug> <number> [--version <n>]` | `GET …/changes/<number>/checks` | `version` |

A flag not given sends no parameter, so that the hub's default applies;
text values are sent as given, for the hub to judge.

The body of `parley change open` holds `source.branch` from `--source`, and
one member for each other flag given, none for a flag not given:
`--source-workspace` `source.workspace_slug`, `--target` `target_branch`,
`--title` `title`, `--intent` `intent`, `--draft` `draft` (`true`),
`--depends-on` `depends_on` (a number), `--auto-land` `auto_land` (`true`),
the `--label` values `labels` and `--session` `session_id`.
`--intent @<file>` sends the content of the file as the intent, and
`--intent -` all of standard input; any other value is sent as it is, so an
intent that starts with `@` is given through a file or stdin. A file or
stdin that cannot be read is a `client_error` (`cannot read the intent:
open /nonexist: no such file or directory`) with exit status `2`, and no
request is sent.

The body of `parley change edit` holds one member for each flag given and
none for a flag not given, so that a member not named is left as it is:
`--title` `title` (an empty `--title ""` is sent as `""`, for the hub to
judge), `--intent` `intent` (read as for `open`, before any request),
`--auto-land` `auto_land` `true`, `--no-auto-land` `auto_land` `false`,
and `--target` `target_branch`. With `--label` or `--remove-label`, each
repeatable, `edit` first reads the change with `GET …/changes/<number>`,
takes its `labels`, removes every `--remove-label` value, appends every
`--label` value it does not hold yet, in order, and sends the result as
`labels` in the one `PATCH` (`--remove-label a --label a` on `["a","b"]`
sends `["b","a"]`; removing every label sends `[]`). When that read fails,
its error is printed as for any refusal and no `PATCH` is sent.

Every command but `diff --patch` and `conflict --file` prints the hub's JSON answer on stdout, as
every result is printed (see [Output](#output)), with exit status `0`;
`open` prints the `201` change. `parley change diff --patch` prints the
unified diff text the hub answers (`Content-Type: text/x-diff`) as
received; `--name-only` prints the JSON answer, whose files carry no
`patch`. `list` prints the one page the hub answered and does not follow
`next_cursor`. `stack` prints the stack the hub answers (see
[The stack of a change](api.md#the-stack-of-a-change)): `target_branch` and
the summaries of its changes, bottom first. `restack` asks the hub to
rebase the change onto what it must sit on now (see
[Manual restack](api.md#manual-restack)) and prints its answer,
`{"status": "restacked" | "up_to_date", "change": …}`; a restack that
conflicts is the refusal `409` `rebase_conflict`, whose envelope holds the
conflict report in `details`.

`parley change conflict` prints the change's current conflict report, as
the hub computes it against what the change would be rebased onto now
(see [the conflict route](api.md#get-apiv1workspacesslugchangesnumberconflict)):
`{"mergeable": …, "conflict": …}`, `conflict` `null` when the change is
clean. With `--fail-on-conflict` and `mergeable` `conflict` it still
prints the answer, then ends with a `condition_not_met` envelope on
stderr (`the change does not rebase onto what it would sit on without a
conflict: mergeable is conflict`) and exit status `3`; a clean answer
exits `0`. With `--file <path>` it writes the text the hub answers for
that content-conflicted file (see
[the conflict file route](api.md#get-apiv1workspacesslugchangesnumberconflictfilepath)),
between `<<<<<<< target` and `>>>>>>> change` markers, to stdout byte for
byte: no JSON framing and no newline added. A refusal of the file route
(`404` `path_not_found`, `409` `not_text_conflict`, `400`
`invalid_request` for an empty path) writes nothing to stdout.

`parley change resolve` restacks the change as `restack` does, with the
resolutions of its conflicted files (see
[Manual restack](api.md#manual-restack)): it needs at least one `--file`
or `--delete`, each repeatable. Its body is
`{"resolutions": [...]}`, one entry per flag in the order of the command
line: `--file <repo-path>=<local-file>`, split at its first `=` so that
the local file's name may hold one, sends
`{"path": "<repo-path>", "content_base64": "<the local file's bytes in
standard base64>"}` (`""` for an empty file), never `content`, so that a
binary file travels unchanged; `--delete <repo-path>` sends
`{"path": "<repo-path>", "delete": true}`. Every local file is read
before anything is sent. It prints the answer,
`{"status": "restacked" | "up_to_date", "change": …}`, with exit status
`0`; a conflicted path left without a resolution is the refusal `409`
`rebase_conflict`, whose envelope, with the conflict report in `details`,
is written to stderr as received, exit status `1`.

`parley change land` lands the change, or with `--stack` its whole stack
(see [`POST …/land`](api.md#post-apiv1workspacesslugchangesnumberland)).
Its body holds one member for each flag given and none for a flag not
given, `{}` without a flag: `--stack` `stack` (`true`),
`--expected-version` `expected_version` (a number), `--override <reason>`
`override` (`{"reason": "<reason>"}`) and `--session` `session_id`. It
sends no `wait` query, so the hub holds the call for its default 30
seconds, and its request has 70 seconds to complete. A `200` (the attempt
`landed`) or `202` (the attempt still `running`) is printed as the land
attempt on stdout with exit status `0`; without `--wait` a `202` is a
success, since the hub took the land and its outcome is not known yet. A
refusal (`409` `land_conflict` with the attempt and its conflict report in
`error.details`, `409` `policy_blocked`, `503` `land_failed`, `409`
`change_not_open`, `version_mismatch`, `land_in_progress` or
`depends_on_unlanded`, `403` `forbidden` or `token_bound`, among others) is
written to stderr as received, with nothing on stdout and exit status `1`,
and is never polled. With `--wait` and a `202`, the command polls
`GET /api/v1/workspaces/<slug>/lands/<id>`, `<id>` the attempt's `id`, as
for [Waiting](#waiting), on the attempt's `status`: `landed` ends the wait
with exit status `0`, `conflict`, `blocked` and `failed` end it with the
attempt on stdout and `wait_failed` on stderr (`the operation ended in the
state "conflict", which is not a success`), and `running` keeps it going. A
land answered `200` is not polled.

`parley change revert` opens a change that undoes the landed change, on
the new branch `revert/<number>` (see
[`POST …/revert`](api.md#post-apiv1workspacesslugchangesnumberrevert)).
Its body holds one member for each flag given and none for a flag not
given, `{}` without a flag: `--title` `title`, `--intent` `intent` (sent
as given), `--draft` `draft`, `--auto-land` `auto_land` and `--session`
`session_id`. A `<number>` that is not a positive integer, a missing
argument and an extra one are a `usage_error`, exit status `2`, before any
request. The `201` answer, the new change, is printed on stdout with exit
status `0`; a refusal (`409` `revert_conflict` with the conflict report in
`error.details.conflict`, `409` `already_reverted` with
`error.details.reverted_by`, `409` `change_not_landed`, `403`
`path_not_allowed` with `error.path`, `403` `forbidden`, among others) is
written to stderr as received, with nothing on stdout and exit status `1`.

`parley change lands` prints the page of the change's land attempts the
hub answers, newest first (see
[`GET …/lands`](api.md#get-apiv1workspacesslugchangesnumberlands)).

`parley change wait` reads the change at once, then again one
`--poll-interval` (default `5s`) after each answer, until the change meets
the condition of `--for` or can no longer meet it, under one deadline:
`--timeout` (default `5m`) counted from the start of the command. Durations
are written as for [Waiting](#waiting). The command changes nothing on the
hub. Each read decides the wait from the change's `state`,
`landing.status` and `mergeable`:

| `--for` | Ends with exit status `0` | Fails with `wait_failed` |
|---|---|---|
| `landed` (the default) | `state` `landed` | `state` `abandoned` |
| `ready` | `state` `landed`, or `landing.status` `ready` with `mergeable` `clean` | `state` `abandoned`, `landing.status` `blocked` (a draft's always is) or `mergeable` `conflict` |
| `mergeable` | `state` `landed`, or `mergeable` `clean` | `state` `abandoned` or `mergeable` `conflict` |

Any other change keeps the wait going. Each read writes one progress line
to stderr, `waiting: <state>, landing <status>, mergeable <mergeable>`
(`waiting: open, landing pending, mergeable unknown`; a member the change
does not carry is left out, as in `waiting: landed`). On a success the
change of the last read is the only document on stdout and the exit status
is `0`. On a failure the change of the last read is printed on stdout and
`wait_failed` is written to stderr (`the change is open, landing blocked,
mergeable clean, which ends the wait for ready`), with exit status `1`.
When the deadline passes first, nothing is printed on stdout and
`wait_timeout` is written to stderr (`the operation did not end within 5m;
its last state was "open, landing pending, mergeable unknown"`), with exit
status `1`; a read in flight at the deadline is abandoned. A read the hub
answers with an error ends the wait at once: its envelope is written to
stderr as received, nothing to stdout, exit status `1`.

A refusal of the hub's (`404` `change_not_found`, `400`
`source_not_related`, `409` `change_exists`, `400` `invalid_request`, `409`
`change_closed`, `400` `intent_required`, `409` `has_dependants`, `409`
`source_workspace_gone`, `409` `rebase_conflict` with its conflict report,
`404` `path_not_found`, `409` `not_text_conflict`, among others) is written to stderr as received, with nothing on stdout and exit
status `1`. `open` is sent once and never retried. `approvals
--withdraw`, whose route answers `204`, prints `{}`.

**Client checks.** A missing `--source`, a wrong number of arguments, a
`<number>`, `--against`, `--depends-on` or `--stack-of` that is not a
positive integer in decimal digits (`parley change show takes a <number>
that is a positive integer, got "x"`), a `land --expected-version` that is
not one (`parley change land takes --expected-version <n>, a positive
integer, got "0"`), a blank `land --override` (`parley change land takes
--override <reason>, a reason that is not blank`), a `--version` that is neither that
nor `latest` (`parley change diff takes --version <n>, a positive integer
or latest, got "x"`), `--patch` with `--name-only` (`parley change diff
takes --patch or --name-only, not both`), `conflict --file` with
`--fail-on-conflict` (`parley change conflict takes --file or
--fail-on-conflict, not both`), `resolve` without `--file` or `--delete`
(`parley change resolve needs at least one --file or --delete`), a
`resolve --file` without `=` or with an empty side (`invalid argument
"a.txt" for "--file" flag: want <repo-path>=<local-file>, neither side
empty`) or whose local file cannot be read (`invalid argument
"a.txt=/nonexist" for "--file" flag: cannot read the local file: open
/nonexist: no such file or directory`), an empty `resolve --delete`
(`invalid argument "" for "--delete" flag: want a <repo-path>, not an
empty one`), a repository path named twice across `--file` and
`--delete` (`parley change resolve names the repository path "a.txt"
twice; a path takes one resolution`), a `--limit` that is not an
integer, `edit` without any of its edit flags (`parley change edit needs
at least one of --title, --intent, --label, --remove-label, --auto-land,
--no-auto-land, --target`), `edit` with both `--auto-land` and
`--no-auto-land` (`parley change edit takes --auto-land or --no-auto-land,
not both`), an `abandon --dependants` other than `reparent` and
`abandon` (`parley change abandon takes --dependants reparent or abandon,
got "delete"`), a `wait --for` other than `landed`, `ready` and
`mergeable` (`parley change wait takes --for landed, ready or mergeable,
got "done"`), a `--version` of `approve`, `reject`, `approvals`, `check`
or `checks` that is not a positive integer (`parley change approve takes
--version <n>, a positive integer, got "0"`), `reject` without `--comment`
(a required flag) or with an empty one (`parley change reject takes a
--comment that says why, not an empty one`), `check` without `--status` or with another status
(`parley change check takes --status pending, passed or failed, got
"ok"`), `approvals --withdraw` with `--version` (`parley change approvals
takes --withdraw or --version, not both`) or with an empty id (`parley
change approvals takes --withdraw <id>, not an empty one`) and a `wait` or `land` `--timeout` or `--poll-interval` that is not a
duration above zero (see [Waiting](#waiting)) are refused with a
`usage_error` and exit status `2` before any setting is read or any request
sent.

### `parley activity` and `parley claim`

These commands drive the [awareness routes](api.md#awareness): what else
touches a set of paths, and the claims that announce work on them. Each
sends one request with the credential as `Authorization: Bearer` and
`<slug>` (and `<id>`) as one percent-encoded path segment, and prints the
hub's answer on stdout with exit status `0`; a refusal of the hub's is
written to stderr as received, with nothing on stdout and exit status `1`.
`parley claim` run without a command prints its help and exits `0`.
`parley change overlaps` is listed with [`parley change`](#parley-change).

| Command | Request | Flags sent as |
|---|---|---|
| `parley activity <slug> --paths <globs> [--target <branch>] [--since <sha>\|<time>] [--exclude-session <id>] [--exclude-change <n>] [--fail-on-overlap]` | `GET /api/v1/workspaces/<slug>/activity` | `--paths` as one `paths` value, comma list as given; `target`, `since`, `--exclude-session` `exclude_session`, `--exclude-change` `exclude_change` |
| `parley claim create <slug> --paths <globs> --intent <text> [--target <branch>] [--ttl <duration>] [--session <id>]` | `POST /api/v1/workspaces/<slug>/claims` | the body `{"paths":[…],"intent":…}`, `paths` the comma list split into an array, with `--target` `target_branch`, `ttl` and `--session` `session_id` when given |
| `parley claim list <slug> [--state <s>] [--session <id>] [--path <glob>] [--limit <n>] [--cursor <c>]` | `GET /api/v1/workspaces/<slug>/claims` | `state`, `--session` `session_id`, `path`, `limit`, `cursor` |
| `parley claim renew <slug> <id> [--ttl <duration>]` | `PATCH /api/v1/workspaces/<slug>/claims/<id>` | the body `{"ttl":…}`, `1h` without `--ttl` |
| `parley claim release <slug> <id>` | `DELETE /api/v1/workspaces/<slug>/claims/<id>` | no body; prints `{}` |

A flag not given sends no parameter or member, so that the hub's default
applies (`--target`: the workspace's default branch; `--since`: 24 hours
ago; `--ttl` of `create`: `[awareness].default_claim_ttl`).

**`--fail-on-overlap`**, on `parley activity` and `parley change overlaps`,
prints the answer first and then, when something overlaps, writes a
`condition_not_met` envelope on stderr and exits `3`: for `activity` when
its `changes` or its `claims` holds an item (changes in `landed` do not
count), for `change overlaps` when `items` holds an entry. Without the flag,
or when the lists are empty, the exit status is `0`.

`parley activity` and `parley claim create` without `--paths`, `parley claim
create` without `--intent`, any of these commands with a wrong number of
arguments, and an `--exclude-change` that is not a positive integer are a
`usage_error` with exit status `2`, before any request is sent.

### `parley patch`

These commands drive the [patch routes](api.md#carry-patch-workspaces) of a
carry-patch workspace. Each sends one request (`reset-to-origin --wait`
then polls, below) with the credential as
`Authorization: Bearer` and `<slug>` (and `<patch-id>`) as one
percent-encoded path segment (`parley patch list a/b` sends
`/api/v1/workspaces/a%2Fb/patches`), and prints the hub's answer on stdout
with exit status `0`, for a `201` and a `200` alike; a refusal of the
hub's is written to stderr as received, with nothing on stdout and exit
status `1`. `parley patch` run without a command prints its help and exits
`0`.

| Command | Request | Flags sent as |
|---|---|---|
| `parley patch add <slug> --branch <name> [--position <n>] [--upstream-pr <url>] [--description <text>] [--skip-branch-check] [--if-not-exists]` | `POST /api/v1/workspaces/<slug>/patches` | the body `{"branch_name":…}`, with `position` (a number), `--upstream-pr` `upstream_pr_url`, `description`, `skip_branch_check` and `if_not_exists` (`true`) when given |
| `parley patch list <slug> [--include-deleted]` | `GET /api/v1/workspaces/<slug>/patches` | `include_deleted=true` with `--include-deleted` |
| `parley patch show <slug> <patch-id>` | `GET /api/v1/workspaces/<slug>/patches/<patch-id>` | |
| `parley patch update <slug> <patch-id> [--status <s>] [--position <n>] [--description <text>] [--upstream-pr <url>]` | `PATCH /api/v1/workspaces/<slug>/patches/<patch-id>` | a body with one member per flag given: `status` (sent as typed; the hub judges it), `position` (a number), `description`, `--upstream-pr` `upstream_pr_url` |
| `parley patch remove <slug> <patch-id>` | `DELETE /api/v1/workspaces/<slug>/patches/<patch-id>` | |
| `parley patch restore <slug> <patch-id>` | `POST /api/v1/workspaces/<slug>/patches/<patch-id>/restore` | no body |
| `parley patch reorder <slug> <patch-id>...` | `POST /api/v1/workspaces/<slug>/patches/reorder` | the body `{"patch_ids":[…]}`, the ids in argument order |
| `parley patch reset-to-origin <slug> <patch-id> [--wait] [--timeout <duration>] [--poll-interval <duration>]` | `POST /api/v1/workspaces/<slug>/patches/<patch-id>/reset-to-origin`; with `--wait` and a `rebuild_job_id`, then `GET /api/v1/workspaces/<slug>/rebuilds/<id>` until `completed`, `dead_letter` or `cancelled` | no body |

A flag not given sends no member: `--description ''` sends `""`, which the
hub stores as `null` (on an update it clears the field), and leaving it out
sends nothing. `parley patch remove` answers the hub's `204` with
`{"status":"removed","patch_id":"<patch-id>"}`, the id as typed.

`parley patch reset-to-origin` prints the hub's answer as it is without
`--wait`, and also with `--wait` when the answer names no
`rebuild_job_id` (no rebuild was queued); it then sends nothing more. With
`--wait` and a `rebuild_job_id` it follows the rebuild job through the
shared [wait](#waiting): `queued`, `running` and `failed` (waiting to
retry) go on, `completed` ends it with exit status `0`, and `dead_letter`
or `cancelled` with `wait_failed` and exit status `1`. Either way it
prints one document, the reset answer with its members untouched plus
the member `rebuild`, the final job record. A wait that passes `--timeout`
prints nothing on stdout, reports `wait_timeout` and exits `1`, and asks
the hub to cancel nothing.

`parley patch add` without `--branch`, `parley patch update` with none of
its four flags (`parley patch update needs at least one of --status,
--position, --description, --upstream-pr`), `parley patch reorder` without
a patch id, a `--position` that is not an integer, a `--timeout` or
`--poll-interval` that is not a duration, and any of these commands with a
wrong number of arguments, are a `usage_error` with exit status `2`, before
any request is sent.

### `parley rebuild`

These commands drive the [rebuild job routes](api.md#rebuild-jobs) of a
carry-patch workspace. Each sends one request (`submit --wait` and
`requeue --wait` then poll, below) with the credential as
`Authorization: Bearer` and `<slug>` and `<rebuild-id>` as one
percent-encoded path segment each, and prints the hub's answer on stdout
with exit status `0`, for a `202` and a `200` alike; a refusal of the
hub's is written to stderr as received, with nothing on stdout and exit
status `1`. `parley rebuild` run without a command prints its help and
exits `0`.

| Command | Request | Flags sent as |
|---|---|---|
| `parley rebuild submit <slug> [--strategy <rebase\|merge>] [--fail-mode <fail_fast\|continue>] [--wait] [--timeout <duration>] [--poll-interval <duration>]` | `POST /api/v1/workspaces/<slug>/rebuild` | a body with `strategy` and `fail_mode` for the flags given, `{}` with neither |
| `parley rebuild list <slug> [--status <status>]` | `GET /api/v1/workspaces/<slug>/rebuilds` | `status=<status>` only when `--status` is given |
| `parley rebuild status <slug> <rebuild-id>` | `GET /api/v1/workspaces/<slug>/rebuilds/<rebuild-id>` | |
| `parley rebuild cancel <slug> <rebuild-id>` | `DELETE /api/v1/workspaces/<slug>/rebuilds/<rebuild-id>` | |
| `parley rebuild requeue <slug> <rebuild-id> [--wait] [--timeout <duration>] [--poll-interval <duration>]` | `POST /api/v1/workspaces/<slug>/rebuilds/<rebuild-id>/requeue` | no body |
| `parley rebuild rollback <slug> <rebuild-id>` | `POST /api/v1/workspaces/<slug>/rebuilds/<rebuild-id>/rollback` | no body |
| `parley rebuild preview <slug> [--fail-on-conflict]` | `GET /api/v1/workspaces/<slug>/rebuild-preview` | |

With `--wait`, `submit` and `requeue` follow the job the answer names
through the shared [wait](#waiting), polling
`GET /api/v1/workspaces/<slug>/rebuilds/<id>`: `queued`, `running` and
`failed` (waiting to retry) go on, `completed` ends it with exit status
`0`, and `dead_letter` or `cancelled` with the final job record on stdout,
`wait_failed` on stderr and exit status `1`. A wait that passes
`--timeout` prints nothing on stdout, reports `wait_timeout` and exits
`1`, and asks the hub to cancel nothing. Without `--wait` they print the
hub's answer and exit `0`.

**`--fail-on-conflict`.** After `preview` prints the answer, it collects
the `branch_name` of every entry of `patch_results` whose `status` is
`would_conflict`. When there is any, it writes a `condition_not_met` error
naming them in the preview's order (`would conflict with the upstream:
patch/b`) and exits `3`; otherwise, and without the flag, it exits `0`. A
refusal of the hub's still exits `1` with nothing on stdout.

A `--strategy` other than `rebase` or `merge`, a `--fail-mode` other than
`fail_fast` or `continue`, a `--status` other than `queued`, `running`,
`failed`, `completed`, `dead_letter` or `cancelled` (`parley rebuild
submit takes --strategy rebase or merge, got "squash"`), a `--timeout` or
`--poll-interval` that is not a positive [duration](#waiting) (`0s` and
`-1s` are refused too), and a wrong number of arguments are a
`usage_error` with exit status `2`, before any request is sent.

### `parley rerere`

```
parley rerere list <slug>
parley rerere forget <slug> <id-or-path>
```

These commands read and forget the conflict resolutions the rebuilds of a
carry-patch workspace remember (see [the hub's
routes](api.md#remembered-resolutions-and-the-patch-status)). Each sends one
request with the credential as `Authorization: Bearer` and `<slug>` as one
percent-encoded path segment. `parley rerere` run without a command prints
its help and exits `0`.

- `list` sends `GET /api/v1/workspaces/<slug>/rerere` and prints the hub's
  answer, the first page of entries, newest first, with exit status `0`. It
  takes no `--limit` or `--cursor` and does not follow `next_cursor`.
- `forget` sends `DELETE /api/v1/workspaces/<slug>/rerere/<id-or-path>`.
  The argument is split at `/`, each part percent-encoded on its own and
  the parts joined with `/` again, so the hub reads it as the path it is
  (`parley rerere forget demo "docs/read me.md"` sends
  `…/rerere/docs/read%20me.md`, `a/b?c#d` sends `…/rerere/a/b%3Fc%23d`; a
  part `.` or `..` is sent as `%2E` or `%2E%2E`). The hub answers `204`
  with no body, so the command prints a document that carries the argument
  as given, with exit status `0`:

  ```json
  {
    "status": "forgotten",
    "pathspec": "src/net/dial.go"
  }
  ```

A refusal of the hub's (`404` `not_found` `resolution not found`, `400`
`invalid_request` for a pathspec that starts with `-`, given after `--`,
`400` `workspace_mode_mismatch`, `403` `insufficient_scope`, among others)
is written to stderr as received, with nothing on stdout and exit status
`1`. A wrong number of arguments is a `usage_error` with exit status `2`,
before any request is sent.

### `parley land`

`parley land` run without a command prints its help and exits `0`; it
reads no credential and sends no request. Its two commands read the land
attempts of a workspace through the [land routes](api.md#landing) of the
hub. Each sends one `GET` with the credential as `Authorization: Bearer`,
`<slug>` and `<land-id>` as one percent-encoded path segment each, and
each flag given as query parameters, none for a flag not given, so that
the hub's default applies; text values are sent as given, for the hub to
judge.

| Command | Request | Flags sent as |
|---|---|---|
| `parley land list <slug> [--status <s>]... [--target <branch>] [--since <time>] [--limit <n>] [--cursor <c>]` | `GET …/workspaces/<slug>/lands` | one `status` per `--status`, `--target` `target_branch`, `since`, `limit`, `cursor` |
| `parley land show <slug> <land-id>` | `GET …/workspaces/<slug>/lands/<land-id>` | |

`list` prints the one page of attempts the hub answered, newest first, and
does not follow `next_cursor`; `show` prints the one attempt. Both print
the JSON answer on stdout with exit status `0`. A refusal of the hub's
(`404` `land_not_found`, `400` `invalid_request` for an unknown `status`
or a `since` that is not an RFC 3339 timestamp, among others) is written to
stderr as received, with nothing on stdout and exit status `1`.

**Client checks.** A wrong number of arguments (`parley land show needs
the argument <land-id>`) and a `--limit` that is not an integer are
refused with a `usage_error` and exit status `2` before any setting is read
or any request sent.

### `parley policy`

`parley policy` run without a command prints its help and exits `0`; it
reads no credential and sends no request. Its three commands read, replace
and try out the landing policy of a workspace through the
[landing policy routes](api.md#landing-policy) of the hub. Each sends one
request with the credential as `Authorization: Bearer` and `<slug>` as one
percent-encoded path segment, and prints the hub's JSON answer on stdout
with exit status `0`.

| Command | Request | Flags sent as |
|---|---|---|
| `parley policy get <slug> [--revision <n>]` | `GET …/workspaces/<slug>/policy` | `--revision` as the query `revision` |
| `parley policy set <slug> --file <path>\|- [--if-revision <n>]` | `PUT …/workspaces/<slug>/policy` | the document of the file as the body; `--if-revision <n>` as the header `If-Match: "<n>"` |
| `parley policy evaluate <slug> --paths <list>... \| --change <n> [--target <branch>] [--policy-file <path>\|-]` | `POST …/workspaces/<slug>/policy/evaluate` | the body below |

`set` prints the stored policy with its new `revision`; without
`--if-revision` it sends no `If-Match` and replaces whatever revision is
stored. `get` prints the current policy, or with `--revision` one of the
revisions the hub keeps.

The body of `parley policy evaluate` holds one member for each flag given,
none for a flag not given: `paths`, a list of the values of every `--paths`
flag split at commas, in command-line order (`--paths billing/a.go,docs/x.md
--paths src/b.go` sends `["billing/a.go","docs/x.md","src/b.go"]`);
`change`, the `--change` number; `target_branch`, the `--target` value; and
`policy`, the document of `--policy-file`. It prints what the paths, or the
change's files, would need, and writes nothing.

**Policy files.** `set --file` and `evaluate --policy-file` read the
document the same way: `-` reads standard input; a name ending in `.toml`
is read as TOML; anything else is read as JSON and, when it is not valid
JSON, as TOML. A JSON document is sent as it is. A TOML document is sent
as the JSON document of the same content: its table `default` and its
array of tables `rules` become the members `default` and `rules`,
integers stay JSON numbers and booleans JSON booleans, so

```toml
[default]
approvals = 0

[[rules]]
name = "billing"
paths = ["billing/**"]

[rules.require]
approvals = 1
checks = ["test"]
```

is sent as `{"default":{"approvals":0},"rules":[{"name":"billing","paths":["billing/**"],"require":{"approvals":1,"checks":["test"]}}]}`.
The document is read before any request; whether it is a valid policy is
the hub's to judge. A file or standard input that is empty or holds only
white space is refused, although it is valid TOML: its document would be
the empty one, which lets every change land, and an empty pipe or a
truncated file must not replace the policy with it.

A refusal of the hub's (`400` `invalid_policy` with its `details`, `412`
`revision_mismatch`, `403` `token_bound` for a token bound to a task fork
that runs `set` on the fork's parent, `400` `not_supported_on_fork`, `409`
`workspace_archived`, `404` `not_found` for a revision the hub does not
keep, `404` `change_not_found`, `400` `invalid_request`, among others) is
written to stderr as received, with nothing on stdout and exit status `1`.

**Client checks.** A wrong number of arguments (`parley policy get needs
the argument <slug>`), `set` without `--file` (`parley policy set needs the
flag --file`), a `--revision`, `--if-revision` or `--change` that is not a
non-negative integer in decimal digits (`parley policy get takes
--revision <n>, a non-negative integer, got "x"`), `evaluate` with both or
neither of `--paths` and `--change` (`parley policy evaluate takes exactly
one of --paths and --change`), a policy file that cannot be read (`parley
policy set cannot read the policy of --file: open /nonexist: no such file
or directory`), a `.toml` file that is not valid TOML (`parley policy set
takes --file <path>, a policy document: j.toml is not valid TOML: …`), a
file or standard input that is empty or blank (`parley policy set takes
--file <path>, a policy document: standard input is empty`) and any other
file, or standard input, that is neither valid JSON nor valid
TOML (`parley policy set takes --file <path>, a policy document: bad.json
is neither valid JSON nor valid TOML: …`) are refused with a `usage_error`
and exit status `2` before any setting is read or any request sent.

### `parley scm`

`parley scm` run without a command prints its help and exits `0`; it reads
no credential and sends no request. Its nine read commands read a
workspace's repository without a clone, through the
[source control reads](api.md#source-control-reads) of the hub. Each sends
one `GET` with the credential as `Authorization: Bearer`, `<slug>` (and the
`<rev>` of `show`) as one percent-encoded path segment
(`parley scm show demo feature/x` sends
`GET /api/v1/workspaces/demo/scm/commits/feature%2Fx`), its other arguments
and each flag given as query parameters, and no parameter for a flag not
given, so that the hub's default applies. Text flags are sent as given, for
the hub to judge; a boolean flag sends its value below when given.

| Command | Request | Arguments and flags sent as |
|---|---|---|
| `parley scm refs <slug> [--prefix <p>] [--limit <n>] [--cursor <c>]` | `GET …/scm/refs` | `prefix`, `limit`, `cursor` |
| `parley scm log <slug> [--ref <rev>] [--path <path>] [--follow] [--since <time>] [--until <time>] [--author <text>] [--stats] [--limit <n>] [--cursor <c>]` | `GET …/scm/commits` | `ref`, `path`, `follow=true`, `since`, `until`, `author`, `stats=true`, `limit`, `cursor` |
| `parley scm show <slug> <rev> [--files]` | `GET …/scm/commits/<rev>` | `files=true` |
| `parley scm ls <slug> [path] [--ref <rev>] [--recursive]` | `GET …/scm/tree` | `path`, `ref`, `recursive=true` |
| `parley scm cat <slug> <path> [--ref <rev>] [--output <file>]` | `GET …/scm/file` | `path`, `ref` |
| `parley scm diff <slug> <base> <head> [--mode merge_base\|direct] [--path <glob>]... [--patch \| --name-only]` | `GET …/scm/diff` | `base`, `head`, `mode`, one `path` per `--path`, `--patch` `format=patch`, `--name-only` `patches=false` |
| `parley scm compare <slug> <base> <head> [--fail-on-conflict]` | `GET …/scm/compare` | `base`, `head` |
| `parley scm blame <slug> <path> [--ref <rev>] [--lines <start>:<end>]` | `GET …/scm/blame` | `path`, `ref`, `--lines` `start_line` and `end_line` |
| `parley scm search <slug> <query> [--ref <rev>] [--path <glob>]... [--regex] [--ignore-case]` | `GET …/scm/search` | `q`, `ref`, one `path` per `--path`, `regex=true`, `--ignore-case` `case_sensitive=false` |

`parley scm search demo 'to do' --ref v1 --path 'src/**' --path '*.go'
--regex --ignore-case` sends
`GET /api/v1/workspaces/demo/scm/search?case_sensitive=false&path=src%2F%2A%2A&path=%2A.go&q=to+do&ref=v1&regex=true`.

Every command but `cat` and `diff --patch` prints the hub's JSON answer on
stdout, as every result is printed (see [Output](#output)), with exit status
`0`. `ls`, `search`, `refs` and `log` print the one page the hub answered
and do not follow `next_cursor`; `refs` and `log` take `--cursor` to read
the next one.

- `parley scm cat` asks for the raw form of the file (`Accept: */*`) and
  writes the file's bytes, unchanged and without JSON framing, to stdout,
  or with `--output <file>` to that file, created or replaced, with stdout
  empty. The file is written only once the hub has answered `200`: after a
  refusal nothing is written, no file is created and an existing one is left
  as it was. A file that cannot be written is a `client_error` (`cannot
  write the file: …`) with exit status `2`.
- `parley scm diff --patch` prints the unified diff text the hub answers
  (`Content-Type: text/x-diff`) as received; `--name-only` prints the JSON
  answer, whose files carry no `patch`.
- `parley scm compare --fail-on-conflict` prints the answer and, when its
  `mergeable` is `conflict`, ends with a `condition_not_met` envelope on
  stderr (`head does not rebase onto base without a conflict: mergeable is
  conflict`) and exit status `3`. With `mergeable` `clean`, or without the
  flag, the exit status is `0`.

A refusal of the hub's (`404` `ref_not_found` or `path_not_found`, `409`
`workspace_busy`, `413` `payload_too_large`, `503` `scm_timeout`, among
others) is written to stderr as received, with nothing on stdout and exit
status `1`.

**Client checks.** A wrong number of arguments, a `--mode` other than
`merge_base` or `direct` (`parley scm diff takes --mode merge_base or
direct, got "sideways"`), `--patch` with `--name-only` (`parley scm diff
takes --patch or --name-only, not both`), a `--lines` that is not two
positive integers `<start>:<end>` (`parley scm blame takes --lines
<start>:<end>, two positive integers, got "3"`) and a `--limit` that is not
an integer are refused with a `usage_error` and exit status `2` before any
setting is read or any request sent.

**Writes.** `parley scm commit` and `parley scm rebase` write to the
repository through the [source control writes](api.md#source-control-writes)
of the hub, each with one `POST`, `<slug>` as one percent-encoded path
segment and the body below as JSON.

| Command | Request | Body |
|---|---|---|
| `parley scm commit <slug> --branch <b> --message <m> [--base <rev>] [--expected-head <sha>] [--put <repo-path>=<local-file>]... [--delete <repo-path>]... [--move <from>=<to>]... [--session <id>] [--change-target <b>] [--title <t>] [--intent <i>] [--draft]` | `POST …/scm/commits` | `branch`, `message`, `operations`, `--base` `base_ref`, `--expected-head` `expected_head_sha`, `--session` `session_id`, `change` |
| `parley scm rebase <slug> <branch> --onto <rev> [--expected-head <sha>]` | `POST …/scm/rebase` | `branch`, `onto`, `--expected-head` `expected_head_sha` |

- `--put`, `--delete` and `--move` repeat, and become the commit's
  `operations` in the order they are given on the command line, across the
  three flags: `--put <repo-path>=<local-file>` is
  `{"op":"put","path":…,"content_base64":…}`, the local file's bytes
  base64-encoded and no `mode`; `--delete <repo-path>` is
  `{"op":"delete","path":…}`; `--move <from>=<to>` is
  `{"op":"move","from":…,"to":…}`. A pair is split at its first `=`.
  Without any of them `operations` is `[]`.
- `change` is sent when at least one of `--change-target`, `--title`,
  `--intent` and `--draft` is given, and holds `target_branch`, `title`,
  `intent` and `draft` only for the flags given. `base_ref`, `session_id`
  and `expected_head_sha` are sent only for their flag.
- Without `--expected-head`, `parley scm commit` first reads the branch with
  `GET …/scm/refs/<branch>`, the branch as one percent-encoded segment
  (`--branch feat/x` reads `…/scm/refs/feat%2Fx`), and sends its `sha` as
  `expected_head_sha`, so the commit lands only on the head it read. A `404`
  `ref_not_found` means a new branch, and the commit is sent without
  `expected_head_sha`; any other refusal of the read is written to stderr
  as received, with exit status `1` and no commit sent.

Both print the hub's answer on stdout with exit status `0`: the commit's
`201` body, and the rebase's `200` body whether its `status` is `rebased` or
`up_to_date`. A refusal (`409` `ref_moved` with `details.current_sha`, `409`
`rebase_conflict` with the conflict report in `details`, `403` of a push
rule, among others) is written to stderr as received, `details` included,
with nothing on stdout and exit status `1`. Each write is sent once and
never retried.

A missing `--branch`, `--message` or `--onto`, a wrong number of arguments,
a `--put` or `--move` without `=` or with an empty side (`invalid argument
"nopath" for "--put" flag: want <repo-path>=<local-file>, neither side
empty`) and a `--put` local file that cannot be read (`invalid argument
"p=/nonexist" for "--put" flag: cannot read the local file: open /nonexist:
no such file or directory`) are refused with a `usage_error` and exit status
`2` before any setting is read or any request sent.

### `parley forge`, `parley issue` and `parley pr`

These groups read a workspace's forge through the hub's
[forge reads](api.md#forge-reads); run without a command each prints its
help and exits `0`. Each command sends one `GET` with the credential as
`Authorization: Bearer`, `<slug>` and `<number>` each as one
percent-encoded path segment, and a query parameter only for a flag that
was given, and prints the hub's JSON answer (see [Output](#output)).

| Command | Request | Flags |
|---|---|---|
| `parley forge status <slug>` | `GET …/forge` | none |
| `parley forge repository <slug>` | `GET …/forge/repository` | `--target` |
| `parley issue list <slug>` | `GET …/issues` | `--state`, `--label` (repeatable), `--assignee`, `--limit`, `--cursor`, `--target` |
| `parley issue get <slug> <number>` | `GET …/issues/<number>` | `--target` |
| `parley issue comments <slug> <number>` | `GET …/issues/<number>/comments` | `--limit`, `--cursor`, `--target` |
| `parley pr get <slug> <number>` | `GET …/pulls/<number>` | `--target` |
| `parley pr state <slug> <number>` | `GET …/pulls/<number>/state` | `--target` |
| `parley pr files <slug> <number>` | `GET …/pulls/<number>/files` | `--limit`, `--cursor`, `--target` |
| `parley pr checks <slug> <number>` | `GET …/pulls/<number>/checks` | `--limit`, `--cursor`, `--target` |
| `parley pr reviews <slug> <number>` | `GET …/pulls/<number>/reviews` | `--limit`, `--cursor`, `--target` |

Each flag is sent as the query parameter of its name, and the values of
every `--label` as one `labels` parameter joined by commas (`parley issue
list widgets --label bug --label ui` sends the one parameter `labels` with
the value `bug,ui`). A flag a command
does not take, a wrong number of arguments, or a `<number>` that is not a
positive decimal integer of digits only, without sign or leading zero and
at most 2,147,483,647, is a `usage_error` with exit status `2`, before any
request. The list commands print the one page the hub answered and take
`--cursor` to read the next.

The write commands change the forge through the hub's
[forge writes](api.md#forge-writes), which need a credential with the scope
`forge:write`. Each sends one request, `<slug>`, `<number>` and a label each
as one percent-encoded path segment, with a JSON body holding only the
members whose flags were given and `target` in the query only when
`--target` was given, and prints the hub's answer. Every write command takes
`--target <origin|upstream>`. None sends an `Idempotency-Key`.

| Command | Request | Flags |
|---|---|---|
| `parley issue create <slug>` | `POST …/issues` | `--title` (required), `--body` or `--body-file`, `--label` (repeatable), `--assignee` (repeatable) |
| `parley issue update <slug> <number>` | `PATCH …/issues/<number>` | `--title`, `--body` or `--body-file`, `--state`, `--label` (repeatable), `--assignee` (repeatable); at least one |
| `parley issue close <slug> <number>` | `POST …/issues/<number>/close` | `--comment` |
| `parley issue comment <slug> <number>` | `POST …/issues/<number>/comments` | `--body` or `--body-file` (one is required) |
| `parley issue label <slug> <number> <label>...` | `POST …/issues/<number>/labels` | none |
| `parley issue unlabel <slug> <number> <label>` | `DELETE …/issues/<number>/labels/<label>` | none |
| `parley label create <slug> <name>` | `POST …/labels` | `--color`, `--description` |
| `parley pr create <slug>` | `POST …/pulls` | `--title`, `--head`, `--base` (all required), `--body` or `--body-file`, `--draft` |
| `parley pr comment <slug> <number>` | `POST …/pulls/<number>/comments` | `--body` or `--body-file` (one is required) |

- `--body <text>` gives a text; `--body-file <path>` reads it from the file,
  or from standard input when the path is `-`. The text is sent exactly as
  given or read, with nothing trimmed; `--body ""` on `issue update` sends
  `body` as `""`, which clears the issue's body. A text that is not valid
  UTF-8 is a usage error, since JSON cannot carry it unchanged.
- `--label` and `--assignee` are sent as the lists `labels` and `assignees`,
  one name per flag; on `issue update` each replaces the whole set.
- `issue close` sends no body without `--comment`, and `{"comment":
  <text>}` with it. `--state` is sent as given; the hub checks it. `--draft`
  sends `draft: true` and, when not given, no `draft` member.
- `issue label` sends its labels, in the order given, as a bare JSON array,
  and prints `{"status":"labeled","number":<n>,"labels":[…]}` when the hub
  answers `204`; `issue unlabel` prints
  `{"status":"unlabeled","number":<n>,"label":"<label>"}`. `number` is a
  JSON number.
- `label create` exits `0` both when the label existed (`200`) and when it
  was created (`201`).

A missing required flag, both `--body` and `--body-file`, a `--body-file`
that cannot be read, `issue update` without a field flag, `issue label`
without a label and a `<number>` that breaks the rule above are each a
`usage_error` with exit status `2`, raised before any request.

The webhook commands manage the secret a forge signs a workspace's webhook
deliveries with, and read what the hub recorded of them, through the hub's
[forge webhook routes](api.md#forge-webhooks).

| Command | Request | Flags |
|---|---|---|
| `parley forge webhook set <slug>` | `PUT …/forge/webhook` | one of `--secret <string>`, `--from-stdin`, `--generate` (the default) |
| `parley forge webhook unset <slug>` | `DELETE …/forge/webhook` | none |
| `parley forge deliveries <slug>` | `GET …/forge/deliveries` | `--since <rfc3339>`, `--limit`, `--cursor` |

- `webhook set` with no flag or `--generate` sends no body, and the hub
  generates the secret; `--secret <value>` sends `{"secret": <value>}`, and
  `--from-stdin` sends the same with the value read from standard input
  without one trailing line ending (`\n` or `\r\n`). The CLI does not check
  the value; the hub does. It prints the hub's answer, and when the answer
  holds `secret` it writes the warning `this value is shown once and will
  not be shown again` on stderr.
- `webhook unset` prints `{"status":"unset","workspace":"<slug>"}` when the
  hub answers `204`.
- `deliveries` sends a query parameter only for a flag that was given and
  prints the one page the hub answered.

More than one of `--secret`, `--from-stdin` and `--generate`, or a missing
`<slug>`, is a `usage_error` with exit status `2`, raised before any request.

### `parley webhook`

These commands call the [event webhook routes](api.md#event-webhooks) of
the hub with the credential as `Authorization: Bearer`, and print the hub's
answer, indented by two spaces. `parley webhook` run without a command
prints its help and exits `0`; it reads no credential and sends no request.

| Command | Request | Flags |
|---|---|---|
| `parley webhook create <workspace-slug>` | `POST /api/v1/workspaces/<slug>/webhooks` | `--url <url>` (required) as `url`; `--types <list>` as `types`, split at commas; `--no-forks` as `include_forks` `false`; `--description <text>` as `description`. A member is sent only for a flag given |
| `parley webhook list <workspace-slug>` | `GET …/webhooks` | `--state`, `--limit`, `--cursor`, each sent only when given |
| `parley webhook get <workspace-slug> <id>` | `GET …/webhooks/<id>` | |
| `parley webhook update <workspace-slug> <id>` | `PATCH …/webhooks/<id>` | `--url`, `--types`, `--forks` or `--no-forks` (`include_forks` `true` or `false`), `--description`, `--enable` or `--disable` (`active` `true` or `false`); at least one flag |
| `parley webhook delete <workspace-slug> <id>` | `DELETE …/webhooks/<id>` | prints `{"status":"deleted","id":"<id>"}` |
| `parley webhook rotate-secret <workspace-slug> <id>` | `POST …/webhooks/<id>/rotate-secret` | |
| `parley webhook ping <workspace-slug> <id>` | `POST …/webhooks/<id>/ping` | |
| `parley webhook deliveries <workspace-slug> <id>` | `GET …/webhooks/<id>/deliveries` | `--status`, `--limit`, `--cursor`, each sent only when given |
| `parley webhook redeliver <workspace-slug> <id> <delivery-id>` | `POST …/webhooks/<id>/deliveries/<delivery-id>/redeliver` | |

`create` and `rotate-secret` print the answer with its `secret`, then, when
the answer carries one, the line

```
warning: this value is shown once and will not be shown again
```

on stderr, which never repeats the secret; an `Idempotency-Key` replay
answers without the secret and writes no warning.

`update` with no flag, `update` with both `--forks` and `--no-forks` or both
`--enable` and `--disable`, and `create` without `--url` are a
`usage_error` (exit status `2`) before any request, as are a wrong number of
arguments and an unknown flag. A flag of a pair, and `create`'s
`--no-forks`, counts only when it is true: `--enable=false` is no flag, so
`update` with it alone is a `usage_error` and nothing is switched on.

`ping` and `redeliver` print the delivery record on stdout. When its
`status` is `succeeded` they exit `0` with nothing on stderr; when it is
`failed` they write a `delivery_failed` envelope on stderr and exit `1`:

```json
{
  "error": {
    "code": 0,
    "message": "…",
    "error_type": "delivery_failed"
  }
}
```

A hub error, such as `404` `webhook_not_found`, is written to stderr as
received, with nothing on stdout and exit status `1`.

### `parley session`

These commands drive the [session routes](api.md#sessions): an agent opens
a session on a workspace, reports the tokens it spends and completes it
with its outcome. Each sends one request with the credential as
`Authorization: Bearer` and the session id as one percent-encoded path
segment, and prints the hub's answer on stdout with exit status `0`, for a
`201` and a `200` alike; a refusal of the hub's is written to stderr as
received, with nothing on stdout and exit status `1`. `parley session` run
without a command prints its help and exits `0`.

| Command | Request | Flags sent as |
|---|---|---|
| `parley session start <workspace-slug> [--id <uuid>] [--run-id <id>] [--node-id <id>] [--archetype <name>] [--model <name>] [--metadata <json>]` | `POST /api/v1/sessions` | the body `{"workspace_slug":…}` with `id`, `run_id`, `node_id`, `archetype`, `model` and `metadata` when given; `metadata` is the JSON value, not a string holding it |
| `parley session usage <session-id> --model <name> [--input <n>] [--output <n>] [--cache-read <n>] [--id <uuid>]` | `POST /api/v1/sessions/<session-id>/usage` | the body `{"model":…}` with `input_tokens`, `output_tokens`, `cache_read_tokens` and `id` when given |
| `parley session complete <session-id> [--status completed\|failed\|timeout] [--error <text>] [--duration-ms <n>]` | `POST /api/v1/sessions/<session-id>/complete` | the body `{"status":…}`, `completed` without `--status`, with `error_message` and `duration_ms` when given |
| `parley session get <session-id>` | `GET /api/v1/sessions/<session-id>` | no body |
| `parley session list [--workspace <slug>] [--include-forks] [--run-id <id>] [--status <s>] [--since <time>] [--limit <n>] [--cursor <c>]` | `GET /api/v1/sessions` | `workspace`, `include_forks=true` for `--include-forks`, `run_id`, `status`, `since`, `limit`, `cursor` |

A flag not given sends no member or parameter, so that the hub's default
applies; the commands check nothing else, and a value the hub refuses is
its `400` as received.

A command without its argument, `parley session usage` without `--model`,
a `--metadata` that is not JSON (`parley session start takes --metadata as
a JSON document, got "{oops"`), and an `--input`, `--output`,
`--cache-read`, `--duration-ms` or `--limit` that is not an integer are a
`usage_error` with exit status `2`, before any request is sent.

### `parley mcp`

```
parley mcp [--list] [--groups <groups>] [--read-only] [--workspace <slug>]
           [--sandbox <id>] [--no-code-mode] [--script-timeout <duration>]
```

Serves the commands of `parley` as tools to a harness that speaks MCP, in
one session on standard input and output, until the harness closes its end;
the exit status is then `0`. A harness starts it with the endpoint and the
credential every command takes ([Settings](#settings-the-hub-url-and-the-credential)),
which are read once, at start: a call the hub later answers `401` is that
call's failed result. Without one of the two it ends like any command, with
a `client_error` and exit status `2`. Only the protocol is written to
stdout. SIGINT and SIGTERM end the session and cancel the calls that run.
The server names itself `parley`, with the CLI's version, and its
instructions say in at most three sentences what the tools are, which
workspace and sandbox the session is fixed on, and that `code_mode` chains
them.

`--list` prints the tools that would be served as one JSON array of
`{name, description, input_schema}`, in the order they are served, and
exits `0` without a session; it needs a hub and a credential only with
`--sandbox`.

**Which commands are tools.** `--groups` names command groups (the first
word of a command), separated by commas. A name alone serves the group's
commands; `name:read` only those whose route is a `GET`. The default is
`activity,change,claim,fork,land,sandbox,scm,workspace:read`, 68 tools
today. `--read-only` narrows whatever is selected to `GET` commands.

- Never served, whatever is selected: a command whose `auth` is `admin` or
  `none`, one that shows a secret once, and one that needs a terminal or
  runs until interrupted (`sandbox shell`, `events tail`). The groups
  `admin`, `keys`, `tokens`, `secrets`, `login`, `credential-helper`,
  `help`, `version` and `mcp` cannot be named: `--groups` with one of them
  is a `usage_error` naming it.
- A name that is no group, an empty element and a suffix other than `:read`
  are a `usage_error` listing the groups.
- More than 150 tools is a `usage_error` that says how many.
- A command added to a served group is a tool with nothing else done.

**A command as a tool.** The tool's name is the command's words joined by
`_`, with `-` replaced by `_` (`change land` is `change_land`), and its
description the command's. Its parameters are the command's arguments and
flags, named in lower case with every character that is not a letter or a
digit replaced by `_` (`sandbox-path` is `sandbox_path`, `KEY=VALUE` is
`key_value`), and no other parameter is allowed:

| Of the command | Parameter |
|---|---|
| an argument | a string, required as the command requires it |
| an argument that takes every remaining word | an array of strings, one word each |
| the words after `--` (`sandbox exec`, `sandbox create`, `fork create`) | the array of strings `command`; `sandbox_exec` requires it |
| a `bool` flag | a boolean |
| a `string` or `duration` flag | a string (`90s`) |
| a `stringArray` flag | an array of strings |
| an `int`, `int64` or `integer` flag | an integer |

The flags `--api-key`, `--endpoint-url`, `--json` and `--poll-interval` are
no parameters, nor is a flag that makes its command run until interrupted
(`--follow` of `sandbox logs`). With `--workspace <slug>`, the workspace
argument of every tool (`slug`, `workspace_slug`, `workspace` or `parent`)
is optional and is that workspace when a call leaves it out; a call may
still name another, such as a fork.

**A call** runs the command's own run step in the `parley mcp` process,
with `--json`, and is bounded at 30 minutes (`timeout` past that).

| The command | The tool's result |
|---|---|
| prints a JSON object | that object as the data |
| prints another JSON value | `{"result": value}` |
| prints nothing | `{}` |
| prints what is not one JSON document (the lines of `sandbox logs`) | `{"result": "<text>"}` |
| ends with the hub's error envelope | failed: `error` is the envelope's `error_type`, `detail` its message |
| gets an error answer without an envelope | failed: `error` is `http_<status>` |
| ends with one of the CLI's own errors | failed: `error` is `usage_error`, `client_error`, `wait_failed`, `wait_timeout` or `condition_not_met`; the document printed, if any, is still the data |
| exits with a status of its own (`sandbox exec`) | failed: `error` is `command_exit`, with the collected answer as the data |

A failed result is a result, not a protocol error. A tool never reads
standard input or a terminal: a value of `-` where the command would read
standard input, and a flag such as `--stdin`, are a `usage_error`. A command
that destroys data needs its `confirm` parameter, as from the shell. A
command with `--wait` waits inside the call, bounded by its `timeout`
parameter. A local path (`sandbox_upload`, `sandbox_download`, `--put` of
`scm_commit`) is one of the machine `parley mcp` runs on, relative to its
working directory. A command that takes words after `--` takes its
arguments before it, so there an argument that begins with a dash is a
`usage_error`.

The calls go through agentkit-go's runner, which checks a call's parameters
against the tool's schema before the tool runs: a parameter the tool does
not have, one of the wrong type and a missing required one are a failed
result with the error `invalid_arguments`, whose detail names each, and
nothing is sent. A number given for a string parameter is taken as its text.

**Code mode.** `code_mode` is served after the other tools and is bound to
all of them: one call runs a Starlark script that calls tools as functions,
some concurrently, and returns only what the script prints and returns. Its
rules and limits are agentkit-go's, except a script's wall time: 10 minutes,
or `--script-timeout`, from `1s` to `30m` (a `usage_error` outside that).
A script's calls are calls like direct ones. `--no-code-mode` leaves it out.

**A sandbox's tools.** `--sandbox <id>` names a sandbox of the workspace of
`--workspace`, which it needs (`usage_error` without). Its tools are read
once at start (`GET …/sandboxes/{id}/tools`, see
[api.md](api.md#the-tools-of-a-sandbox)) and served under their own names,
descriptions and input schemas after the command tools and before
`code_mode`, to which they are bound, so one script can search and edit in
the sandbox and read a change on the hub. If the hub refuses the listing
(`tools_unavailable`, `sandbox_not_ready`, `sandbox_not_found`, no access),
`parley mcp` ends with the hub's envelope and exit status `1`. A call is
one `POST …/tools/{name}`; the hub's result is the tool's as it is, and an
error the hub answers instead (the sandbox stopped, a limit) a failed
result with its `error_type`. The hub bounds such a call at 60 seconds.

- The set is the one the hub gives the credential. With `--read-only`,
  `write_file`, `edit_file`, `execute` and `run_command` are not served.
- The sandbox's own `code_mode` is not served while the session serves
  its own; with `--no-code-mode` it is a sandbox tool like the others.
- A sandbox tool with the name of a command tool is a `client_error`
  naming both, exit status `2`.

**Stderr.** Before it serves, a session writes one line,
`{"tools": 69, "code_mode_description_bytes": 35201}`: the number of tools
and the size of the `code_mode` description, `0` without it. Every tool
call that ends, a direct one and one a script makes, writes one line with
the tool, the outcome and the duration, never the arguments or the result:

```json
{"tool":"change_show","ok":true,"duration_ms":41}
{"tool":"change_land","ok":false,"error":"wait_timeout","duration_ms":60012}
```

`error` is the failed result's error, or `not_run` for a call refused
before its tool ran. In a session, `tools/list` lists the tools by name;
`--list` prints them in the order they are served.

### `parley credential-helper`

```
Usage:
  parley credential-helper <verb> [flags]
```

A [git credential helper](https://git-scm.com/docs/gitcredentials) that
hands the hub credential to git for the hub's own URL, so that the plain
URLs of the [git routes](api.md#git-over-http), such as
`<hub URL>/git/acme/demo.git`, work without typing a credential. Set it up
once for the hub's URL:

```sh
git config --global credential.<hub-url>.helper '!parley credential-helper'
```

The credential may be an API key or a PAT. The command makes no request to
the hub, takes no flags of its own and reads no setting when the verb is not
`get`.

**`get`.** It reads git's request from stdin: `key=value` lines up to the
first empty line or the end of input. It keeps the value (the text after the
first `=`) of `protocol` and of `host`, the last one when a key comes twice,
and ignores every other key, every line without `=` and everything after the
empty line. It then takes the hub URL and the credential as every command
does (see [Settings](#settings-the-hub-url-and-the-credential)): the
global flags, then `PARLEY_ENDPOINT_URL` and `PARLEY_API_KEY`, each when set
and not empty, then `endpoint_url` and `api_key` of the configuration file,
read only when one of the two is still empty. It answers only when the
request's `protocol` equals the hub URL's scheme, its host name equals the
hub URL's without regard to case, and the ports are equal, a missing port
being `443` for `https` and `80` for `http`; neither path is looked at. The
answer is four lines on stdout, `protocol` and `host` as the request gave
them:

```
protocol=https
host=hub.example:8443
username=x-token-auth
password=<credential>
```

For hub `https://hub.example`, `host=HUB.example:443` and `host=hub.example`
are answered; `protocol=http`, `host=hub.example:444` and
`host=hub.example.evil` are not.

**No answer.** When the request has no `protocol` or no `host`, names
another scheme, host or port, or the settings lack the hub URL or the
credential, the configuration file cannot be read or parsed, or the hub URL
is not an absolute `http` or `https` URL with a host and without a query or
fragment, the command prints nothing, on stdout or stderr, and exits `0`, so
that git goes on to its next helper.

**Other verbs.** `store`, `erase` and any other verb (`GET` included: the
verb is matched exactly) print nothing, read neither stdin nor the
configuration file, and exit `0`: the credential lives in parley's settings,
not in git.

**Client checks.** No verb, or a second argument, is a `usage_error` with
exit status `2`, the error envelope on stderr and nothing on stdout.

### Output

stdout carries exactly one JSON document, indented by two spaces and
followed by one newline, or nothing, except for `parley events tail`,
which prints one line per event (see [`parley events`](#parley-events)).
A command that calls one route prints
the hub's answer to a `2xx` status as that document. The body is indented
again without being decoded, so the order of its members and the text of
its numbers stay as the hub sent them: the hub's
`{"id":"x","n":1.50,"big":12345678901234567890}` is printed as

```json
{
  "id": "x",
  "n": 1.50,
  "big": 12345678901234567890
}
```

A `2xx` answer without a body (a `204`, or an empty `200`) prints `{}`, or
the confirmation document of a command that states one, in the same form,
for example:

```json
{
  "status": "deleted",
  "slug": "demo"
}
```

stderr carries everything else. An error is one JSON document (see
[Errors and exit statuses](#errors-and-exit-statuses)); warnings, hints
and progress are plain text lines, and a warning begins with `warning: `.
A command whose result carries a secret the hub shows only once (a token or
key value) prints the result unchanged and then writes the line

```
warning: this value is shown once and will not be shown again
```

to stderr. It writes no such line when the hub answers an error.

A command with a `--fail-on-…` flag, given the flag, checks the flag's
condition in the result. When the condition holds, it prints the result on
stdout as for a success, then writes a `condition_not_met` error with the
command's message to stderr and exits `3`. When the flag is not given, or
the condition does not hold, it prints the result, writes nothing to stderr
and exits `0`.

### Waiting

A command that starts a long operation (a clone, a rebuild, a landing) can
wait for it to end. Every such command has the same three flags:

| Flag | Default | Effect |
|---|---|---|
| `--wait` | `false` | wait until the operation reaches a terminal state and print its final record |
| `--timeout <duration>` | `5m` | with `--wait`, how long to wait for the operation to end |
| `--poll-interval <duration>` | `5s` | with `--wait`, how long to wait between two requests for the operation's state |

A duration is a whole number above `0` followed by exactly one unit: `s`
(seconds), `m` (minutes), `h` (hours) or `d` (24 hours), for example `90s`,
`5m`, `2h` or `1d`. Leading zeros are allowed (`05s` is five seconds), as
in the hub's own durations. Zero (`0s`, `0`), a sign (`-5s`), a fraction
(`1.5m`), a missing or unknown unit (`5`, `5w`), spaces (`5 m`), a compound
form (`1m30s`), an empty value and a value too large to hold are refused
with a `usage_error` naming the flag, before any request, whether or not
`--wait` is given:

```json
{
  "error": {
    "code": 0,
    "message": "invalid argument \"0s\" for \"--timeout\" flag: want a whole number above 0 followed by s, m, h or d, such as 90s or 5m",
    "error_type": "usage_error"
  }
}
```

Without `--wait` the command sends the request that starts the operation,
prints its answer whatever state it holds, and sends nothing else;
`--timeout` and `--poll-interval` then have no other effect.

With `--wait` the command sends the same request and reads the state in
its answer. When that state is already terminal, the wait ends without
asking again. Otherwise it asks the hub for the operation's state one poll
interval after the answer came, and again one interval after each answer,
under one deadline: `--timeout` counted from the moment the start
request's answer came. A state that is not terminal never ends the wait,
"waiting to retry" (a job's `failed`) included. Which states are terminal,
and which of them are a success, each command states. Each poll is an
ordinary request: sent once, with the command's request timeout (30
seconds unless the command or the poll states another).

stdout receives nothing until the wait ends. stderr receives one progress
line, `waiting: <state>`, when the wait begins and each time the state
differs from the one before:

```
waiting: queued
waiting: running
waiting: done
```

The wait ends in one of these ways:

| End | stdout | stderr, after the progress | Exit status |
|---|---|---|---|
| a terminal state that is a success | the final record | nothing | `0` |
| a terminal state that is not a success | the final record | `wait_failed` naming the state | `1` |
| `--timeout` passes first | nothing | `wait_timeout` | `1` |
| a poll answered with the hub's error envelope | nothing | the envelope as received | `1` |
| a poll that fails on the client's side: the hub cannot be reached, gives no complete answer within the request timeout, or answers something that is not the hub's | nothing | `client_error` | `2` |

The errors a wait raises itself read, for example:

```json
{
  "error": {
    "code": 0,
    "message": "the operation ended in the state \"dead\", which is not a success",
    "error_type": "wait_failed"
  }
}
```

```json
{
  "error": {
    "code": 0,
    "message": "the operation did not end within 1s; its last state was \"running\"",
    "error_type": "wait_timeout"
  }
}
```

A command that states its own final document (for example one that prints
its answer with the final job added) prints that document in place of the
final record, under the same rules.

At the deadline a poll still in flight is abandoned, so the command
returns within moments of the timeout. Running out of time stops only the
waiting: the client sends no request that cancels or changes the
operation, which goes on on the hub.

### Errors and exit statuses

stdout carries the result, and nothing on a failure, except the result a
`--fail-on-…` condition held in and the final record of a wait that ended
in a state that is not a success (`wait_failed`, see [Waiting](#waiting)),
the events `parley events tail` printed before an error ended it (see
[`parley events`](#parley-events)), the results of the scopes in which
`parley secrets create` succeeded (see [`parley secrets`](#parley-secrets)),
and the delivery record of a failed `parley webhook ping` or
`parley webhook redeliver` (see [`parley webhook`](#parley-webhook)).
Every error goes to stderr as one error envelope (`parley secrets create`
writes one per scope that failed), a JSON document indented by two spaces
and followed by a newline, and nothing else: no usage text follows it. The only other lines on stderr
are the progress lines of a wait (see [Waiting](#waiting)) and the
`reconnecting in …` lines of [`parley events tail`](#parley-events-tail),
which come before any error envelope. An error envelope the hub
answered (a status of `400` or above whose body is a JSON object with an
`error` member holding `error_type`) is written as the hub sent it, indented
again by two spaces without being decoded: its `code` is the HTTP status,
and members such as `error.details` or `error.oldest_seq` are kept with
their values and in their order. An error `parley` raises itself has `code`
`0`, a message and one of six `error_type`s:

```json
{
  "error": {
    "code": 0,
    "message": "unknown command \"nosuch\" for \"parley\"",
    "error_type": "usage_error"
  }
}
```

| `error_type` | When |
|---|---|
| `usage_error` | the command line cannot be accepted: an unknown command, at the top level or under a group, an unknown flag, a flag without its value or with a value that does not parse as the flag's type, fewer or more arguments than the command takes, a flag the command requires that is not given, `parley help` with words that name no command, or a value the command refuses (`parley login --expires 45`). It is raised before any setting is read and before any request |
| `client_error` | a failure on the client's side that is not the command line's: for example no hub URL, a configuration file that cannot be read, a hub that cannot be reached, an answer that is not the hub's. A failure inside the CLI of no other kind is reported as a `client_error` too, with its text as the message |
| `wait_failed` | a wait for a long operation ended in a state that is not a success (see [Waiting](#waiting)), or `parley change wait` read a change that ends its wait without meeting it (see [`parley change`](#parley-change)) |
| `wait_timeout` | a wait for a long operation, or `parley change wait`, ran out of time (see [Waiting](#waiting)) |
| `condition_not_met` | a `--fail-on-…` condition held in the result |
| `delivery_failed` | the test delivery of `parley webhook ping`, or the attempt of `parley webhook redeliver`, failed; the delivery record is on stdout (see [`parley webhook`](#parley-webhook)) |

The exit status follows from the error alone, on every command:

| Status | When |
|---|---|
| `0` | success, including help and `parley version` whether or not the hub answered |
| `1` | the hub answered with an error envelope (written to stderr as received), or `wait_failed`, `wait_timeout` or `delivery_failed` |
| `2` | `usage_error` or `client_error` |
| `3` | `condition_not_met` |
