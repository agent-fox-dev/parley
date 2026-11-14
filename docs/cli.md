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
| the workspace root's own name (`workspaces` by default) | a copy of every file, directory and symbolic link under the workspace root, with the same relative paths |

Each store is copied first, as one read of the engine (SQLite's
`VACUUM INTO`): the copy holds every transaction committed before the copy
began, each with all of its rows, while the hub goes on committing, and it
is a single database file without a write-ahead log. The workspace root is
copied next; within each git repository the refs, `packed-refs` and `HEAD`
are copied before the objects, so every copied ref points at a copied
object.

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
{"time":"2026-11-13T13:44:23.060364+01:00","level":"INFO","msg":"backup written","dir":"/srv/backups/2026-11-13"}
```

Otherwise it writes one line, `backup failed` with the cause in `error`,
and exits `1`. It refuses, writing nothing, an invalid configuration (the
error names the key), a missing database file (it names the path and
creates no database), a directory that is not empty, a path that is not a
directory, and a directory inside the workspace root or inside the data
directory (the directory of `[database].path`):

```json
{"time":"2026-11-13T13:44:22.522501+01:00","level":"ERROR","msg":"backup failed","error":"database file /srv/parley/data/parley.db does not exist"}
{"time":"2026-11-13T13:44:23.075944+01:00","level":"ERROR","msg":"backup failed","error":"backup directory /srv/backups/2026-11-13 is not empty"}
{"time":"2026-11-13T13:44:23.081202+01:00","level":"ERROR","msg":"backup failed","error":"backup directory /srv/parley/data/snapshot is inside the data directory /srv/parley/data"}
```

A relative directory is taken relative to the working directory.

When a copy fails part-way, for example on a file under the workspace root
it cannot read, it removes everything it wrote (the directory too, when it
created it), and the line names the path that failed.

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
{"time":"2026-11-13T13:20:41.118034+01:00","level":"WARN","msg":"admin token written: save the token and delete the file","path":"/srv/parley/admin_token"}
```

Save the token and delete the file: the hub does not start while the file
is there. The hash and the file are written together, so when the file
cannot be written (a missing config dir, no permission, a directory in its
place) nothing is stored, and `parleyd` writes one `cannot start` line naming
the file and exits `1`. While the file is there, a start without
`--reset-admin-token` writes:

```json
{"time":"2026-11-13T13:21:02.530118+01:00","level":"ERROR","msg":"cannot start","error":"admin token file /srv/parley/admin_token exists: save the token, delete the file and start the hub again"}
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
   completion;
5. it binds `[server].bind`:`[server].port`;
6. it starts the maintenance loop in the background, without waiting for
   its first pass: one pass at once and one every hour, each running the
   registered maintenance tasks one after the other; and it starts the
   background work the hub's parts registered;
7. it writes the `listening` line and serves.

A failure in stages 1 to 5 writes exactly one error line to stderr, closes
the stores when they were open, and exits `1` without listening.

### Log lines

`parleyd` logs JSON objects to stderr, one per line, with `log/slog`. Every
line has `time`, `level` and `msg`. `level` is `TRACE`, `DEBUG`, `INFO`,
`WARN` or `ERROR`; lines below `[logging].level` (`info` by default) are not
written, and with `trace` every line is. Before the configuration is loaded
the level is `info`.

The message and every attribute of every line are scrubbed before the line
is written (an error as its text; a value that is neither a string nor an
error as its printed text, which takes the value's place when scrubbing
changes it): the
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
{"time":"2026-11-14T06:20:11.402117+01:00","level":"INFO","msg":"secret encryption","key_configured":true,"previous_key_present":false,"undecryptable_secrets":0}
```

Without a current key it also writes a warning and goes on starting:

```json
{"time":"2026-11-14T06:20:11.402121+01:00","level":"WARN","msg":"secrets cannot be stored: no encryption key configured"}
```

```json
{"time":"2026-11-13T13:11:38.622658+01:00","level":"INFO","msg":"listening","addr":"127.0.0.1:52887","config":"/srv/parley/config.toml","data_dir":"/srv/parley/data"}
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

A maintenance task that returns an error or panics writes one line with the
task's name and the scrubbed error, and does not stop the others; it runs
again in the next hourly pass:

```json
{"time":"2026-11-13T10:13:00.000000+01:00","level":"ERROR","msg":"maintenance task failed","task":"claim_cleanup","error":"remove claim: database is locked"}
```

The maintenance task `token_binding_sweep` writes one info line with the
number of tokens it revoked, in a pass that revoked any: tokens bound to a
workspace that no longer exists:

```json
{"time":"2026-11-14T10:13:00.000000+01:00","level":"INFO","msg":"revoked the tokens bound to workspaces that no longer exist","count":2}
```

The maintenance task `secret_reencryption` writes one info line in every
pass that ends without a store error: `converted` is the number of values it
sealed again under the current key, `remaining` the number of stored values
still not under the current key (every value when no current key is
configured), the values that fail to open under the previous key included.
Once `remaining` is `0` the previous key can be removed (see
[configuration.md](configuration.md#secret-keys)):

```json
{"time":"2026-11-14T10:13:00.000000+01:00","level":"INFO","msg":"secret re-encryption","converted":5,"remaining":0}
```

When a stop stops waiting for something whose grace or the 30-second limit
has passed, it writes one line naming it in `work`: `requests`,
`maintenance loop` or the name of a piece of background work:

```json
{"time":"2026-11-13T14:02:31.401227+01:00","level":"WARN","msg":"abandoned at the stop","work":"event_streams"}
```

When the start fails before listening it writes one line and exits `1`.
The line is `cannot start` with the cause in `error`, which names the
configuration key, the secret key's variable or file, the path, the schema
step, the admin token file or the start work that failed:

```json
{"time":"2026-11-13T13:11:38.609972+01:00","level":"ERROR","msg":"cannot start","error":"configuration file /srv/parley/config.toml: server.prot: unknown key"}
```

A secret key that cannot be loaded is named and never quoted:

```json
{"time":"2026-11-14T06:20:11.391337+01:00","level":"ERROR","msg":"cannot start","error":"PARLEY_SECRETS_KEY: must be standard padded base64 of exactly 32 bytes"}
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
{"time":"2026-11-13T13:11:39.120534+01:00","level":"ERROR","msg":"cannot start","error":"start hook \"sandbox_runtime_check\": sandbox.gateway_url: the gateway runs OpenShell release \"0.3.1\", outside the supported range >=0.1.0 <0.2.0"}
```

When it cannot bind the address the line is `cannot listen`:

```json
{"time":"2026-11-13T10:11:41.348421+01:00","level":"ERROR","msg":"cannot listen","addr":"0.0.0.0:8080","error":"listen tcp 0.0.0.0:8080: bind: address already in use"}
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
  admin       Administer the hub's users, organizations, API keys and access tokens
  events      Follow the hub's events, or read them a page at a time
  help        Print the help of parley or of a command
  keys        List, refresh and revoke your API keys
  login       Sign in to the hub and save the hub URL and the credential
  orgs        List and read the organizations you belong to
  sandbox     Work with the hub's sandboxes and their profiles
  secrets     Create, list, update and delete secrets, whose values the hub never shows
  tokens      Create, read, change and revoke your personal access tokens
  user        Read and update your own profile
  version     Print the client's build and, given a hub URL, the hub's build
  workspace   Register, list, read, change, sync, push, archive, reactivate, reclone and delete the workspaces of the hub

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
list`: 30 seconds plus its `--wait`; `parley events tail`: none, see
[`parley events`](#parley-events)). A request that has
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

`parley sandbox` run without a command prints its help and exits `0`; it
reads no credential and sends no request.

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
[`parley events`](#parley-events)), and the results of the scopes in which
`parley secrets create` succeeded (see [`parley secrets`](#parley-secrets)).
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
`0`, a message and one of five `error_type`s:

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
| `wait_failed` | a wait for a long operation ended in a state that is not a success (see [Waiting](#waiting)) |
| `wait_timeout` | a wait for a long operation ran out of time (see [Waiting](#waiting)) |
| `condition_not_met` | a `--fail-on-…` condition held in the result |

The exit status follows from the error alone, on every command:

| Status | When |
|---|---|
| `0` | success, including help and `parley version` whether or not the hub answered |
| `1` | the hub answered with an error envelope (written to stderr as received), or `wait_failed` or `wait_timeout` |
| `2` | `usage_error` or `client_error` |
| `3` | `condition_not_met` |
