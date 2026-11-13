# Configuration

`parleyd` reads one configuration file, `config.toml`, and the environment
variables named below. `parley` reads the environment variables
`PARLEY_ENDPOINT_URL` and `PARLEY_CONFIG_DIR`, and `parley login` writes its
own configuration file, also named `config.toml` (see [`parley`](#parley)).

## `parleyd`

### The configuration file

`parleyd` reads `config.toml` once, when it starts:

- from `$XDG_CONFIG_HOME/config.toml` when `XDG_CONFIG_HOME` is set and not
  empty (a relative `XDG_CONFIG_HOME` is taken relative to the working
  directory);
- otherwise from `config.toml` in the working directory.

A missing file means every key has its default. The directory the file is
looked for in is the *config dir*. The `config` member of the `listening`
line is the absolute path that was looked for, also when the file does not
exist (see [cli.md](cli.md#log-lines)).

The config dir also receives the admin token file, `admin_token` (mode
`0600`), when `parleyd` runs with `--admin-email` on a new hub or with
`--reset-admin-token`. It holds the hub's admin token in clear text; save the
token and delete the file, since the hub does not start while it is there
(see [cli.md](cli.md#the-admin-token)). The config dir must exist for the
file to be written.

The file is TOML and is checked strictly: invalid TOML, a section or a key
no part of the hub registers, and a value of the wrong type or out of range
each stop the start with exit status `1` and one error line that names the
line or the key (`section.key`). An error about a key does not quote the
key's value; the one exception is a provider `name` given twice, which can
only be `github` or `google`. An error about invalid TOML carries the TOML
parser's own message, which can quote the text at the mistake, such as
`expected value but found "hunter2" instead` for an unquoted string. For
example:

```json
{"time":"2026-11-13T13:11:38.609972+01:00","level":"ERROR","msg":"cannot start","error":"configuration file /srv/parley/config.toml: server.prot: unknown key"}
```

### Environment references

In a string value, `${NAME}` is replaced by the value of the environment
variable `NAME`, and `$$` by one `$`. A `$` followed by neither `{` nor `$`
stays as it is. References are replaced after the file is parsed, in string
values only: never in section or key names, integers or booleans, so a
variable cannot add, remove or rename a key.

| Value | Result |
|---|---|
| `"${GH_SECRET}"` | the value of `GH_SECRET` |
| `"https://${HOST}"` | `https://` and the value of `HOST` |
| `"pa$$word"` | `pa$word` |
| `"cost $5"` | `cost $5` |

A reference to a variable that is not set (set to the empty string is set),
a `${` without a closing `}`, and a name that is not a letter or `_` followed
by letters, digits or `_`, are errors of the key that holds them. The error
names the key. For a variable that is not set, it also names the variable.
For a malformed name, it quotes the reference as written, such as `${A B}`.
It never quotes a variable's value.

### Keys

| Key | Type | Default | Meaning |
|---|---|---|---|
| `[server].bind` | string | `"0.0.0.0"` | the address the hub listens on; not empty |
| `[server].port` | integer | `8080` | the TCP port the hub listens on, `0` to `65535`; `0` lets the system choose |
| `[server].external_url` | string | `""` | the URL clients reach the hub at; empty, or an absolute `http` or `https` URL |
| `[server].max_body_size` | string | `"1MB"` | the largest REST request body: a positive whole number followed by `KB`, `MB` or `GB`, in any letter case, in powers of 1024; a larger body is answered `413` (see [api.md](api.md#request-bodies)) |
| `[database].path` | string | see below | the transactional store's file; its directory is the data directory |
| `[workspace].path` | string | see below | the workspace root, where the repositories live |
| `[workspace].workers` | integer | `4` | how many workspace jobs run at the same time; at least `1` |
| `[logging].level` | string | `"info"` | `trace`, `debug`, `info`, `warn` or `error`, in any letter case; lines below it are not written |
| `[logging].log_health_probes` | boolean | `false` | write an access-log line, at debug level, for requests to `/healthz` and `/readyz` |
| `[limits].requests_per_minute` | integer | `600` | how many REST requests one credential, or one client address without a valid credential, may send in a window of 60 seconds; `0` turns the rate limit off (see [api.md](api.md#rate-limit)) |

`[database].path` and `[workspace].path`, set or not, are resolved to
absolute paths:

| `[database].path` / `[workspace].path` | `XDG_DATA_HOME` set and not empty | otherwise |
|---|---|---|
| not set | `$XDG_DATA_HOME/parley.db` / `$XDG_DATA_HOME/workspaces` | `data/parley.db` / `data/workspaces` in the working directory |
| a bare name, such as `hub.db` | under `$XDG_DATA_HOME` | under the working directory |
| a relative path with a directory, such as `var/hub.db` | under the working directory | under the working directory |
| an absolute path | kept | kept |

The analytical store is `analytics.db` in the data directory. `parleyd`
creates the data directory and the workspace root when they are missing,
with missing parents, with mode `0700`; a directory that exists keeps its
mode.

### OAuth providers

Each `[[oauth.providers]]` table configures one login provider:

```toml
[[oauth.providers]]
name = "github"
client_id = "Iv1.0123456789abcdef"
client_secret = "${GH_SECRET}"
```

| Key | Meaning |
|---|---|
| `name` | `github` or `google`; each at most once |
| `client_id` | not empty |
| `client_secret` | not empty; `parleyd` replaces its value by `[redacted]` in every log line |
| `authorize_url`, `token_url`, `userinfo_url` | absolute `http` or `https` URLs; default to the provider's public endpoints |

The default endpoints are, for `github`,
`https://github.com/login/oauth/authorize`,
`https://github.com/login/oauth/access_token` and
`https://api.github.com/user`, and for `google`,
`https://accounts.google.com/o/oauth2/v2/auth`,
`https://oauth2.googleapis.com/token` and
`https://openidconnect.googleapis.com/v1/userinfo`. The `[oauth]` section
holds nothing else.

### Environment variables

| Variable | Effect |
|---|---|
| `XDG_CONFIG_HOME` | the directory `config.toml` is read from |
| `XDG_DATA_HOME` | where the stores and the workspace root go when `[database].path` and `[workspace].path` do not say (see above) |
| any variable a `${NAME}` reference names | its value replaces the reference |

### Fixed values

| Setting | Value |
|---|---|
| Shutdown grace | 15 seconds: how long in-flight requests get to finish after `SIGTERM` or `SIGINT` |
| Maintenance loop grace | 15 seconds: how long a stop waits for the running maintenance task to end |
| Stop limit | 30 seconds: the longest a stop waits for anything after `SIGTERM` or `SIGINT`; a piece of background work's grace is at most this |
| Idempotency records | kept in the transactional store for 24 hours, so they survive a restart (see [api.md](api.md#idempotency)) |

See [cli.md](cli.md) for how `parleyd` is started and stopped.

## `parley`

| Variable | Effect |
|---|---|
| `PARLEY_ENDPOINT_URL` | the URL of the hub, for example `http://127.0.0.1:8080`, when the flag `--endpoint-url` is absent or empty |
| `PARLEY_API_KEY` | the credential a command presents to the hub (an API key, a personal access token or the admin token), when the flag `--api-key` is absent or empty. `parley login` does not take it |
| `PARLEY_CONFIG_DIR` | the directory of the client configuration file, when set and not empty; otherwise `.parley` in the home directory (`$HOME`) |

Each setting comes from the first non-empty of its flag, its variable and
its key in the client configuration file: the hub URL from
`--endpoint-url`, `PARLEY_ENDPOINT_URL` and `endpoint_url`, the credential
from `--api-key`, `PARLEY_API_KEY` and `api_key`. An empty or unset variable
gives nothing, and values are not trimmed. A command that needs the hub and
a credential and finds no value for one fails before any request with a
`client_error`. `parley version` without a URL prints the client's build
alone. See [cli.md](cli.md#settings-the-hub-url-and-the-credential).

### The client configuration file

The client configuration file is `config.toml` in the client's config
directory: `$PARLEY_CONFIG_DIR/config.toml`, or `~/.parley/config.toml`. It
is TOML and holds the hub URL and the credential, so it is private:

| Key | Value |
|---|---|
| `endpoint_url` | the hub URL, without trailing slashes; every command that talks to the hub, `parley version` and `parley login` included, uses it when neither `--endpoint-url` nor `PARLEY_ENDPOINT_URL` gives one |
| `api_key` | the credential of the last login, an API key or a personal access token; a command that needs a credential uses it when neither `--api-key` nor `PARLEY_API_KEY` gives one |
| `user_id` | the `id` of the user the credential belongs to |

For example:

```toml
api_key = "parley_abcd1234_0123456789abcdefghijABCDEFGHIJ01"
endpoint_url = "http://127.0.0.1:8080"
user_id = "0b5c6f2e-8d1a-4c3b-9e7f-2a4d6c8e0f13"
```

A command reads the file only when its flags and the variables leave a
setting it needs empty, so a command given every setting by flags or
variables never reads it. Reading the file creates nothing. A missing file,
and a missing home directory when `PARLEY_CONFIG_DIR` is unset or empty,
count as an empty file. A file that cannot be read, is not valid TOML, or holds
`endpoint_url`, `api_key` or `user_id` with a value that is not a string is
an error (`client_error`) whose message names the file's path; for invalid
TOML it gives the line and column, never the file's text, which holds a
credential. A command that needed a setting from it then fails before any
request; `parley version` instead writes one `warning: ` line naming the
file and prints the client's build alone, with exit status `0`.

Only `parley login` writes the file. It reads the file before it sends
anything, so a file it could not read fails the login and is left as it
is, and after a successful login sets these three keys and saves the file.
The save keeps every other key and table with its value, but not comments
or the order of the keys. It writes a temporary file in the same directory,
sets its mode to `0600` whatever the umask and the old file's mode, and
renames it over `config.toml`, so the file is replaced as a whole and ends
with mode `0600`; a missing config directory is created with mode `0700`.
A save that cannot write the new file or replace the old one, finds a file
it cannot parse, or has no path because there is no home directory fails
with a `client_error` naming the path, removes its temporary file and
leaves the old file byte for byte as it was. `parley login` does not read
`api_key`, and `PARLEY_API_KEY` does not change it. See
[cli.md](cli.md#parley-login).
