# CLI

Parley ships as two programs: `parleyd`, the hub, and `parley`, its
command-line client.

## parleyd

```
Usage: parleyd [flags]
       parleyd backup <directory>

Flags:
  --admin-email <address>   on a new hub, store the admin's e-mail, write a
                            first admin token to admin_token and exit
  --reset-admin-token       write a new admin token, replacing the old one,
                            and exit
  -h, --help                print this help and exit

Commands:
  backup <directory>        write a consistent snapshot of the data into an
                            empty directory while the hub runs
```

| Exit status | Meaning |
|---|---|
| 0 | a clean run: stopped by SIGTERM or SIGINT, a token written, a backup written, or `--help` |
| 1 | a failure: one JSON line at level `ERROR` on stderr names the cause |
| 2 | a usage error: an unknown flag, `--admin-email` without a value, a positional argument other than `backup`, `backup` without exactly one directory, or a token flag together with `backup` |

### Starting and stopping

`parleyd` with no arguments reads its [configuration](configuration.md),
opens its data, runs the bootstrap, resumes durable work, starts the hourly
maintenance loop and listens on `[server].bind:[server].port`. Once it
listens it writes one JSON line on stderr:

```json
{"time":"…","level":"INFO","msg":"listening","addr":"127.0.0.1:8080","config":"/etc/parley/config.toml","data_dir":"/var/lib/parley"}
```

Logs are JSON lines on stderr, one object per line, and lines below
`[logging].level` are dropped. Every line passes through the scrubber, which
replaces known secret values and the hub's credentials by `[redacted]`,
removes user information from URLs and cuts the text to 1024 bytes.

On SIGTERM or SIGINT the hub stops accepting connections, tells long-lived
responses to end, gives in-flight requests 15 seconds and each piece of
background work its own grace, closes its stores and exits with status 0, at
most 30 seconds after the signal. A second signal changes nothing.

### The first admin token

The hub is bootstrapped once an admin token hash is stored. The token file is
`admin_token` in the config dir.

| Situation | Behaviour |
|---|---|
| not bootstrapped, no flags | starts without an admin token |
| not bootstrapped, `--admin-email <address>` | stores the address and a new token's hash, writes the file, exits 0 |
| `--reset-admin-token` (any state) | writes a new token, replacing the stored hash, exits 0 |
| bootstrapped, `admin_token` exists | refuses to start (exit 1, naming the file) |
| bootstrapped, no `admin_token` | starts; `--admin-email` is ignored |

The token has the form `parley_admin_` followed by 64 lowercase hexadecimal
characters. The file has mode 0600 and no trailing newline. The hub logs the
file's path at level `WARN` (`admin token written: save the token and delete
the file`) and never logs the token; it stores only the token's SHA-256.
Save the token and delete the file before starting the hub again.

### parleyd backup

`parleyd backup <directory>` reads the same configuration and writes, into an
empty or missing directory, a copy of the transactional store under the
database file's name, the analytical store as `analytics.db` and the
workspace root under its own name. Each store copy holds whole transactions
only; in each repository, refs are copied before objects. The running hub
keeps serving. The command exits 1, writing nothing, when the directory is
not empty or is a file, or when no hub data exists; on any later failure it
removes what it wrote. It opens no listener and changes nothing in the data.

## parley

`parley` alone, or the words of a command group alone, prints that help on
stdout and exits 0.

### Every command

**Settings.** A command that talks to the hub takes the hub URL and the
credential each from the first non-empty value of its flag, its variable and
the configuration file:

| Setting | Flag | Variable | File key |
|---|---|---|---|
| hub URL | `--endpoint-url` | `PARLEY_ENDPOINT_URL` | `endpoint_url` |
| credential | `--api-key` | `PARLEY_API_KEY` | `api_key` |

The file is read only when a flag and the environment leave a setting empty
(`parley login` always reads it, since it saves it again). A setting still missing is a `client_error` (exit status 2) before any
request, naming its flag, its variable and the file. The global flags
`--endpoint-url`, `--api-key` and `--json` go before or after a command's
words.

**The configuration file** is `$PARLEY_CONFIG_DIR/config.toml` when the
variable is set and not empty, otherwise `~/.parley/config.toml`. Reading it
creates nothing; a missing file holds no settings. A file that is not valid
TOML, or whose `endpoint_url`, `api_key` or `user_id` is not a string, is a
`client_error` naming its path (for `parley version`, a warning). Saving it (as `parley login` does) creates the
directory with mode 0700, keeps every key it does not know, and replaces the
file atomically with mode 0600.

**Requests.** A command's requests go to `<hub URL>/api/v1<path>` with
`Authorization: Bearer <credential>` (`parley login` sends its provider list
and callback requests without one, and `parley version` reads
`<hub URL>/version` without one); each argument is percent-encoded as one
path segment (a `/` is sent as `%2F`). A request is sent once on a
connection of its own, never retried, and gets 30 seconds to connect, send
and read its answer (`parley version`: 5 seconds). A connection that
fails, a timeout, and a 2xx answer that is not JSON are `client_error`s
naming the hub's host or the status.

**Output.** Apart from help as text, stdout holds exactly one JSON document
indented by 2 spaces, or nothing; a success without a body prints `{}` (or the command's own
confirmation document). Warnings (`warning: `) and wait progress
(`waiting: `) go to stderr; a result that carries a secret shown once is
followed by `warning: this value is shown once and will not be shown again`.
Every error is one envelope on stderr, indented by 2 spaces: the hub's as it answered, member order kept, or the CLI's own
`{"error": {"code": 0, "message": …, "error_type": …}}`.

| Exit status | When |
|---|---|
| 0 | success |
| 1 | the hub refused (its envelope), `wait_failed`, `wait_timeout` |
| 2 | `usage_error` (an unknown command or flag, a bad argument count or value) or `client_error` |
| 3 | `condition_not_met`: a `--fail-on-…` condition held; the result is still printed on stdout |

### Help

`parley help <words>` prints exactly what `parley <words> --help` prints: the
command's description, its arguments, its own flags with their defaults and
the global flags `--endpoint-url`, `--api-key` and `--json`. Help never
resolves settings, never reads the configuration file and never sends a
request. Words that name no command are a usage error (exit status 2) naming
them.

With `--json`, help for a command that is not a group prints one object built
from its declaration:

```json
{
  "name": "login",
  "description": "Sign in through the browser, or store a credential with --api-key",
  "method": null,
  "path": null,
  "args": [],
  "flags": [
    {
      "name": "api-key",
      "type": "string",
      "required": false,
      "default": "",
      "description": "the credential (default from PARLEY_API_KEY, then the configuration file)"
    }
  ],
  "auth": "none"
}
```

(`flags` shortened here.) `name` is the words after `parley`; `method` and
`path` (relative to `/api/v1`) are both `null` for a command that calls no
single route; `args` lists `{name, required}` in order; `flags` holds the
command's own flags and the three global flags, sorted by name, without
`--help`, `type` being the flag library's name for it; `auth` is `none`,
`user` or `admin`.

With `--json`, help for a command group, or `parley help --json` and
`parley --help --json`, prints `{"commands": [...]}`: the object of every
command under it that is not a group, at any depth, sorted by name.

### Waiting for a long operation

A command that starts a long operation (a clone, a rebuild, a landing) has
three flags:

| Flag | Default | |
|---|---|---|
| `--wait` | `false` | wait until the operation ends |
| `--timeout <duration>` | `5m` | how long `--wait` waits |
| `--poll-interval <duration>` | `5s` | how often `--wait` reads the operation |

A duration is a positive whole number followed by one of `s`, `m`, `h` and
`d` (24 hours): `90s`, `5m`, `2h`, `1d`. Any other value (`0s`, `-5s`, `5`,
`1.5m`, `1m30s`, `5 m`, `5w`) is a usage error naming the flag, with or
without `--wait`, and nothing is sent.

Without `--wait` the command prints the answer of the request that starts the
operation, whatever its state, and sends nothing else. With `--wait` it
writes `waiting: <state>` on stderr when the wait begins and at each change of
state, reads the operation once per poll interval until it reaches a terminal
state or `--timeout` passes (counted from the start request's answer), and
then:

| End | stdout | stderr | Exit status |
|---|---|---|---|
| a successful terminal state | the final record | progress lines | 0 |
| an unsuccessful terminal state | the final record | an envelope with `error_type` `wait_failed` naming the state | 1 |
| `--timeout` passed (a poll in flight is abandoned) | nothing | an envelope with `error_type` `wait_timeout` | 1 |
| the hub refused a poll | nothing | the hub's envelope | 1 |
| a poll failed on the client side | nothing | an envelope with `error_type` `client_error` | 2 |

A state that is not terminal, waiting to retry included, never ends the wait,
and a timeout sends no request that cancels or changes the operation.

### parley login

Signs in and saves the hub URL, the credential and the user's id to the
client configuration file.

| Flag | Default | |
|---|---|---|
| `--provider <name>` | `github` | the identity provider to sign in with |
| `--expires <days>` | `90` | the new key's lifetime: 0, 30, 60 or 90 |
| `--api-key <key \| ->` (global) | | with login: store this credential instead of signing in; `-` reads it from stdin |

The hub URL is the first non-empty value of `--endpoint-url`,
`PARLEY_ENDPOINT_URL` and the file key `endpoint_url`.

- **Browser flow.** Lists the hub's providers, listens on `127.0.0.1`, opens
  the provider's authorize URL with `redirect_uri` and a random `state` (or
  prints it on stderr when no browser opens), waits up to 120 seconds for the
  redirect, and exchanges the code with `POST /api/v1/auth/callback`.
- **Key flow** (`--api-key`). Checks the credential with `GET /api/v1/user`
  and saves it; no key is created or revoked.

On success the user is printed on stdout (2-space indented JSON) and
`Logged in as <username>` on stderr; the key is never printed.

| Exit status | When |
|---|---|
| 0 | logged in |
| 1 | the hub refused; its error envelope is printed on stderr, indented by 2 spaces |
| 2 | a client-side failure: an envelope with `code` 0 and `error_type` `usage_error` (a bad flag or credential) or `client_error` (no hub URL, an unreachable hub, a state mismatch, a timeout, an unreadable or unwritable configuration file) |

The configuration file is `$PARLEY_CONFIG_DIR/config.toml`, or
`~/.parley/config.toml`; login creates the directory with mode 0700 and
replaces the file atomically with mode 0600, keeping keys it does not know.

### parley version

Prints the client's build and, when a hub URL is given, the hub's:

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

The hub URL is the first non-empty value of `--endpoint-url`,
`PARLEY_ENDPOINT_URL` and the configuration file's `endpoint_url`; a file
that cannot be read is one `warning: ` line on stderr. Without a hub URL,
only `cli` is printed. When the hub
cannot be read (refused, no answer within 5 seconds, a status other than 200,
a body that is not a JSON object), one line starting with `warning: ` and
naming the URL goes to stderr and `server` is left out; the exit status
stays 0. An argument or an unknown flag is a usage error (exit status 2).
