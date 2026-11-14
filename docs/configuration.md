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

### Sandboxes

The `[sandbox]` section turns sandboxes on: a file that holds it, even
empty, enables them, and a file without it leaves them off. An empty
section is invalid, since `gateway_url` is required. With the section,
`[server].external_url` must be set.

```toml
[server]
external_url = "https://parley.example"

[sandbox]
gateway_url = "https://openshell.example:8443"
token_file = "openshell.token"

[[sandbox.profiles]]
name = "agent"
ttl = "4h"
```

| Key | Type | Default | Meaning |
|---|---|---|---|
| `runtime` | string | `"openshell"` | the sandbox runtime; `openshell` only |
| `gateway_url` | string | required | the OpenShell gateway: an `https` URL, or an `http` URL to `localhost`, an address in `127.0.0.0/8` or `::1`; without user information, a query, a fragment, or a path other than `/` |
| `tls_cert`, `tls_key` | string | `""` | the client certificate and its key for mutual TLS, both or neither; each a readable file |
| `tls_ca` | string | `""` | a readable file of CA certificates the gateway's certificate is checked against |
| `token_file` | string | `""` | a readable file holding the bearer token; its content without surrounding white space must not be empty. It is read once, when `parleyd` starts |
| `oidc_issuer`, `oidc_client_id`, `oidc_client_secret_file` | string | `""` | OIDC client credentials, all three or none: the issuer by the rule of `gateway_url` except that it may have a path, and a readable, non-empty file holding the client secret |
| `openshell_workspace` | string | `"parley"` | the OpenShell workspace the hub owns: 1 to 63 lower-case letters, digits, `_` or `-`, starting with a letter or a digit |
| `max_ttl` | duration | `"24h"` | more than `0` |
| `idle_timeout` | duration | `"30m"` | `0` or more; `0` disables the idle stop |
| `max_sandboxes` | integer | `200` | at least `1` |
| `max_per_org` | integer | `100`, or `max_sandboxes` when that is smaller | at least `1` and at most `max_sandboxes` |
| `max_per_workspace` | integer | `50` | at least `1` |
| `max_per_user` | integer | `20` | at least `1` |
| `allow_workspace_rules` | boolean | `true` | |
| `max_exec_timeout` | duration | `"30m"` | more than `0` |
| `max_concurrent_execs` | integer | `8` | at least `1` |
| `max_transfer_size` | string | `"1GB"` | in the size format of `[server].max_body_size` |
| `log_retention` | duration | `"7d"` | more than `0` |
| `reconcile_interval` | duration | `"1m"` | from `10s` to `10m`, both included |
| `[[sandbox.profiles]]` | tables | required | the sandbox profiles, at least one |
| `[[sandbox.credentials]]` | tables | none | the credentials profiles may attach |

A duration is a whole number followed by one of `s`, `m`, `h` or `d` (24
hours), such as `30m` or `7d`. Relative paths in `tls_cert`, `tls_key`,
`tls_ca`, `token_file`, `oidc_client_secret_file` and a profile's `policy`
are taken from the config dir.

Exactly one way to authenticate to the gateway is configured: mutual TLS
(`tls_cert` with `tls_key`), `token_file`, or the three OIDC keys. `tls_ca`
may accompany any of them. With none, or with more than one, the failure
names `sandbox.gateway_url` and the ways found, such as
`sandbox.gateway_url: configure exactly one way to authenticate, found:
mutual TLS, token_file`. A pair or a triple that is incomplete names the keys
it misses instead.

The hub connects to the gateway's host and port (when `gateway_url` gives
none, 443, or 80 for an `http` gateway), over TLS for an `https` URL and without it for an `http` one.
With mutual TLS it presents the client certificate; with `token_file` it
sends the file's content as a bearer token, also over `http` to a loopback
gateway; with OIDC it obtains a token from `oidc_issuer` with the client
credentials grant on its first call to the gateway and renews it before it
expires. The hub supports the OpenShell releases from `0.1.0`, included, to
`0.2.0`, excluded, compared as semantic versions after an optional leading
`v`; a release that is not a semantic version is outside the range.

Once the file is accepted and the stores are open, and before it listens,
`parleyd` checks the gateway once, within 30 seconds in all, in this order,
and does not start on the first failure (see [cli.md](cli.md) for the
error line): it reads the gateway's health and release and refuses an
unhealthy gateway or a release outside the range; it makes sure the
`openshell_workspace` exists, creating it when it does not; it installs the
provider profile `parley-hub` of the hub credential a sandbox reaches the hub
with, its placeholder in `PARLEY_API_KEY`, for the scheme, host and port of
`[server].external_url` (443 for `https` and 80 for `http` when the URL gives
none) and any program; it reads, once each, the provider profiles the
`[[sandbox.credentials]]` name, and refuses one the gateway does not know;
it refuses a name in a profile's `expose_secrets` or
`optional_expose_secrets` that is a variable declared by the provider
profile of a credential the same profile attaches through `credentials` or
`optional_credentials`; and it sends each profile's `policy` file to the
gateway for validation and keeps the hosts the gateway reports it allows.
The built-in policy is not sent. A gateway that cannot validate a policy
file fails the start for a profile that names one: the OpenShell SDK the hub
is built with has no call for it, so against an OpenShell gateway every
profile stays on the built-in policy. A second start finds the workspace and
replaces `parley-hub`, so nothing is created twice.

A profile is the template of a sandbox. Its table may hold these keys and
no other:

| Key | Type | Default | Meaning |
|---|---|---|---|
| `name` | string | required | 1 to 63 lower-case letters, digits, `_` or `-`, starting with a letter or a digit; unique among the profiles |
| `default` | boolean | `false` | the profile a sandbox gets when its creator names none; at most one profile sets it |
| `description` | string | `""` | |
| `image` | string | `""` | the sandbox image; `""` leaves it to the runtime |
| `cpu`, `memory` | string | `""` | the sandbox's resources; `""` leaves them to the runtime |
| `gpu` | integer | `0` | `0` or more |
| `ttl` | duration | required | more than `0` and at most `max_ttl` |
| `idle_timeout` | duration | the section's `idle_timeout` | `0` or more; `0` disables the idle stop |
| `policy` | string | `""` | a readable file holding the base network policy; `""` is the built-in policy |
| `credentials` | list of strings | `[]` | names of `[[sandbox.credentials]]` tables the sandbox gets |
| `optional_credentials` | list of strings | `[]` | as `credentials`; a name may not be in both lists |
| `expose_secrets` | list of strings | `[]` | names of secrets whose values the sandbox's environment gets, by the rules below |
| `optional_expose_secrets` | list of strings | `[]` | as `expose_secrets`; a name may not be in both lists |
| `hub_access` | string | `"fork"` | `fork` or `none` |
| `env` | table of strings | `{}` | the sandbox's environment: each key matches `^[A-Za-z_][A-Za-z0-9_]*$` and does not start with `PARLEY_` or `OPENSHELL_` |
| `checkpoint` | boolean | `true` | |
| `allow_proposals` | boolean | `true` | |

A name in `expose_secrets` or `optional_expose_secrets` matches
`^[A-Za-z_][A-Za-z0-9_]*$`, does not start with `PARLEY_` or `OPENSHELL_`, is
none of the reserved secret names `GIT_PAT`, `GIT_USERNAME`, `GIT_PASSWORD`,
`UPSTREAM_GIT_PAT`, `UPSTREAM_GIT_USERNAME`, `UPSTREAM_GIT_PASSWORD` and
`FORGE_WEBHOOK_SECRET` in any letter case, and is not a key of the profile's
`env`. Whether the secret exists is not checked when `parleyd` starts.

A configured credential is a secret a profile may attach to its sandboxes.
Its table holds exactly:

| Key | Type | Default | Meaning |
|---|---|---|---|
| `name` | string | required | the pattern of a profile's name; unique among the credentials |
| `provider_profile` | string | required | the id of the OpenShell provider profile that says how the runtime may use it; not empty |
| `secret` | string | required | the name of the secret holding its value; not empty |

The section is checked as a whole, after the rest of the file is accepted
and before any store is opened. Every failure is reported in one error, the
failures separated by `; `, each naming its key: `sandbox.<key>`,
`sandbox.profiles[<name>].<key>`, `sandbox.credentials[<name>].<key>` (the
zero-based index in place of a name that is missing or not a valid name), or
`server.external_url`. An unknown key in the section, a profile or a
credential is a failure like the others. A failure about an `env` key, a
listed credential or an exposed secret quotes that key or name, such as
`sandbox.profiles[agent].credentials: "nope" is not the name of a
[[sandbox.credentials]] table`. Beyond those configured names, no failure
quotes a value, so the error holds no secret and neither the content of `token_file` or `oidc_client_secret_file`, nor
the user information of a URL, nor the value of an `env` entry. For
example:

```
configuration file /srv/parley/config.toml: sandbox.runtime: must be openshell; sandbox.max_ttl: must be more than 0; sandbox.reconcile_interval: must be from 10s to 10m; sandbox.gatway_url: unknown key; server.external_url: must be set when the [sandbox] section is present
```

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
`client_error`, and so does one given a hub URL that is not an absolute
`http` or `https` URL with a host and without a query or fragment. `parley version` without a URL prints the client's build
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
A save that cannot write the new file or replace the old one, or finds a
file it cannot parse, fails with a `client_error` naming the path; one
that has no path because there is no home directory fails with the
`client_error` `cannot save the config file: cannot find the config
directory: set PARLEY_CONFIG_DIR or HOME: …`. Either way it removes its
temporary file and leaves the old file byte for byte as it was. `parley login` does not read
`api_key`, and `PARLEY_API_KEY` does not change it. See
[cli.md](cli.md#parley-login).
