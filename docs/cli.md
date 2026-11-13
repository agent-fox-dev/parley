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

1. it loads and checks the configuration;
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

Once it listens it writes:

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

When a stop stops waiting for something whose grace or the 30-second limit
has passed, it writes one line naming it in `work`: `requests`,
`maintenance loop` or the name of a piece of background work:

```json
{"time":"2026-11-13T14:02:31.401227+01:00","level":"WARN","msg":"abandoned at the stop","work":"event_streams"}
```

When the start fails before listening it writes one line and exits `1`.
The line is `cannot start` with the cause in `error`, which names the
configuration key, the path, the schema step, the admin token file or the
start work that failed:

```json
{"time":"2026-11-13T13:11:38.609972+01:00","level":"ERROR","msg":"cannot start","error":"configuration file /srv/parley/config.toml: server.prot: unknown key"}
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
| `1` | the start failed before listening (the configuration, the data, a schema step, the admin token file that could not be written or is still there, the start work or the listen address), or `backup` failed (see [`parleyd backup <directory>`](#parleyd-backup-directory)) |
| `2` | a usage error: an unknown flag, `--admin-email` without a value, a positional argument other than `backup`, `backup` without exactly one directory, or `backup` with `--admin-email` or `--reset-admin-token` |

## `parley`

```
Usage:
  parley [flags]
  parley [command]
```

Run without arguments (or with `--help`), `parley` prints its help on stdout
and exits `0`. It reads no credential and sends no request:

```
parley is the command-line client of the Parley hub

Usage:
  parley [flags]
  parley [command]

Available Commands:
  help        Help about any command
  login       Sign in to the hub and save the hub URL and the credential
  version     Print the client's build and, given a hub URL, the hub's build

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
| `--json` | accepted on every command; it changes neither the request a command sends nor what it prints |
| `-h`, `--help` | print the help of the command on stdout and exit `0` |

A global flag may stand before, between or after the command's words, with
its value as the next word or after `=`: `parley --endpoint-url <url> version`,
`parley version --endpoint-url <url>` and `parley version --endpoint-url=<url>`
are the same command.

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
host; anything else (`localhost:8080`, `ftp://host`, `http://`, `/api`)
fails before any request:

```json
{
  "error": {
    "code": 0,
    "message": "the hub URL \"localhost:8080\" is not an absolute http or https URL with a host",
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
or it answers a status other than `200` or a body that is not a JSON object,
`parley version` writes one warning line that names the hub URL to stderr and
prints the document without `server`:

```
warning: cannot read the hub's version from http://127.0.0.1:1: Get "http://127.0.0.1:1/version": dial tcp 127.0.0.1:1: connect: connection refused
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
first reads that file, and a file that cannot be read, is not valid TOML
or holds `endpoint_url`, `api_key` or `user_id` with a value that is not a
string fails the login before any request, leaving the file as it is. It
then takes the hub URL from the first non-empty of `--endpoint-url`,
`PARLEY_ENDPOINT_URL` and the file's `endpoint_url`, and drops trailing
slashes from it. With none it fails before any request:

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
{"error":{"code":0,"message":"--expires must be 0, 30, 60 or 90, got \"45\"","error_type":"usage_error"}}
```

The login then:

1. sends `GET <hub URL>/api/v1/auth/providers` without an `Authorization`
   header and takes the item whose `name` is `--provider` (`github` by
   default). A provider the hub does not list fails the login, naming the
   ones it does:

   ```json
   {"error":{"code":0,"message":"the hub does not offer the identity provider \"google\"; it offers github","error_type":"client_error"}}
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
   15 seconds each. On `200` it saves `endpoint_url`, the new key
   (`api_key.key`) and the user's `id` in the config file, prints the
   `user` alone on stdout, indented by two spaces and followed by a
   newline, writes `Logged in as <username>` to stderr and exits `0`. The
   key is never printed.

When the hub answers either request with an error envelope, for example
`429` on the provider list, or `401` (the provider rejected the code),
`403` (`user is blocked`), `409` or `502` on the callback, `parley login`
writes that envelope to stderr exactly as received, saves nothing and
exits `1`.

#### With a credential: `--api-key`

`parley login --api-key <credential>` stores a credential the user already
holds, an API key or a personal access token, without a browser. With
`--api-key -` it reads the credential from stdin, which keeps it out of the
process list. White space around the credential is dropped; an empty one
is a `usage_error` and sends nothing. `--provider` and `--expires` have no
effect here.

It sends `GET <hub URL>/api/v1/user` with `Authorization: Bearer
<credential>` and a timeout of 30 seconds, and nothing else, so no key is
created or revoked. When the hub answers `200` with a JSON object that has
an `id`, it saves the file, prints the User as one JSON document on stdout,
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
writes that envelope to stderr exactly as received, saves nothing and
exits `1`:

```json
{"error":{"code":401,"message":"invalid credentials","error_type":"unauthenticated"}}
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
expects (on `200`: a JSON object with an `id`, a provider list, a callback
answer with a key) nor an error envelope, a provider the hub does not list, a browser that
comes back with the wrong state, an error or no code, or does not come
back within 120 seconds, and a config file that cannot be written after
the hub accepted the credential or issued the key. In that last case the
credential stays valid and is not saved:

```json
{
  "error": {
    "code": 0,
    "message": "cannot reach the hub: Get \"http://127.0.0.1:1/api/v1/user\": dial tcp 127.0.0.1:1: connect: connection refused",
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

### Errors and exit statuses

stdout carries the result, and nothing on a failure. Every error goes to
stderr as one error envelope, a JSON document indented by two spaces and
followed by a newline, and nothing else: no usage text follows it. An error
the hub answered is written as the hub sent it. An error `parley` raises
itself has `code` `0`, a message and one of five `error_type`s:

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
| `usage_error` | the command line cannot be accepted: an unknown command, at the top level or under a group, an unknown flag, a flag without its value or with a value that does not parse as the flag's type, fewer or more arguments than the command takes, or a value the command refuses (`parley login --expires 45`). It is raised before any setting is read and before any request |
| `client_error` | a failure on the client's side that is not the command line's: for example no hub URL, a configuration file that cannot be read, a hub that cannot be reached, an answer that is not the hub's. A failure inside the CLI of no other kind is reported as a `client_error` too, with its text as the message |
| `wait_failed` | a wait for a long operation ended in a state that is not a success |
| `wait_timeout` | a wait for a long operation ran out of time |
| `condition_not_met` | a `--fail-on-…` condition held in the result |

The exit status follows from the error alone, on every command:

| Status | When |
|---|---|
| `0` | success, including help and `parley version` whether or not the hub answered |
| `1` | the hub answered with an error envelope (written to stderr as received), or `wait_failed` or `wait_timeout` |
| `2` | `usage_error` or `client_error` |
| `3` | `condition_not_met` |
