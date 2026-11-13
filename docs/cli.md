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
argument other than `backup`, and `backup` followed by no directory or by
more than one argument, print one error line, then the same usage, on
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

In this version `parleyd backup <directory>` writes one error line,
`backup <directory>: the backup command is not available yet`, and exits
`1` without reading the configuration or touching the data.

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
6. it starts the maintenance work in the background, without waiting for
   it;
7. it writes the `listening` line and serves.

A failure in stages 1 to 5 writes exactly one error line to stderr, closes
the stores when they were open, and exits `1` without listening.

### Log lines

`parleyd` logs JSON objects to stderr, one per line, with `log/slog`. Every
line has `time`, `level` and `msg`. `level` is `TRACE`, `DEBUG`, `INFO`,
`WARN` or `ERROR`; lines below `[logging].level` (`info` by default) are not
written, and with `trace` every line is. Before the configuration is loaded
the level is `info`.

The message and every string attribute of every line, and the text of an
error attribute, are scrubbed before the line is written: the
`client_secret` of every configured OAuth provider, other secret values the
hub knows and the hub's own credentials (admin tokens, personal access
tokens and API keys) become `[redacted]`, the user information is removed
from URLs (`https://u:p@host/x` becomes `https://host/x`), and each value is
cut to at most 1024 bytes. A value from which a secret cannot be removed
cleanly is replaced by `[redacted]` as a whole. Attribute names, `time` and
`level` are written as they are. No `Authorization` header and no request
body is logged.

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

A maintenance task that fails writes one line with the task's name and does
not stop the others:

```json
{"time":"2026-11-13T10:13:00.000000+01:00","level":"ERROR","msg":"maintenance task failed","task":"claim_cleanup","error":"remove claim: database is locked"}
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

### Signals

On `SIGTERM` or `SIGINT` `parleyd` stops accepting connections, lets the
requests in flight finish for up to 15 seconds, closes the connections of the
requests that are still running after that, cancels the running
maintenance work and waits up to 15 seconds for it to end, closes the stores
and exits `0`. A further signal
during the stop changes nothing.

### Exit statuses

| Status | When |
|---|---|
| `0` | `--help`, a run that wrote the admin token file, or a stop by `SIGTERM` or `SIGINT`, whether or not the grace passed |
| `1` | the start failed before listening (the configuration, the data, a schema step, the admin token file that could not be written or is still there, the start work or the listen address), or `backup` |
| `2` | a usage error: an unknown flag, `--admin-email` without a value, a positional argument other than `backup`, or `backup` without exactly one directory |

## `parley`

```
Usage:
  parley [command]
```

Run without arguments (or with `--help`), `parley` prints its help on stdout
and exits `0`. It reads no credential and sends no request:

```
parley is the command-line client of the Parley hub

Usage:
  parley [command]

Available Commands:
  help        Help about any command
  version     Print the client's build and, given a hub URL, the hub's build

Flags:
      --endpoint-url string   URL of the hub (default: $PARLEY_ENDPOINT_URL)
  -h, --help                  help for parley

Use "parley [command] --help" for more information about a command.
```

### Global flags

| Flag | Effect |
|---|---|
| `--endpoint-url <url>` | the URL of the hub, for example `http://127.0.0.1:8080`. When it is absent or empty, `PARLEY_ENDPOINT_URL` gives it (see [configuration.md](configuration.md)) |
| `-h`, `--help` | print the help of the command on stdout and exit `0` |

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

With a hub URL it sends `GET <hub URL>/version` (a trailing slash on the URL is
dropped) without an `Authorization` header and with a timeout of 5 seconds.
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

`parley version` takes no arguments.

### Exit statuses

| Status | When |
|---|---|
| `0` | help, or `parley version`, whether or not the hub answered |
| `2` | an unknown command or flag, or an argument a command does not take; `parley` writes one line `error: <message>` to stderr, for example `error: unknown command "nope" for "parley"` |
