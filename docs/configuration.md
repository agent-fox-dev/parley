# Configuration

`parleyd` reads one TOML file once, at start. Everything else it needs comes
from the environment variables named on this page.

## Where the file is

| Condition | File |
|---|---|
| `XDG_CONFIG_HOME` is set and not empty | `$XDG_CONFIG_HOME/config.toml` |
| otherwise | `config.toml` in the working directory |

The directory of that file is the **config dir**; it also receives the
`admin_token` file (see [CLI](cli.md)). A missing file means every key has its
default. A file that exists but cannot be read stops the start with an error
naming its path.

## Strictness

Invalid TOML, an unknown section, an unknown key, a key outside a section, a
value of the wrong type and a value outside its valid set all stop the start.
The process exits with status 1 and one JSON error line on stderr that names
the key as `section.key` (for invalid TOML, the line). A partly valid file is
never used.

## Environment references

`${NAME}` inside a **string value** is replaced by the environment variable
`NAME`; `$$` gives a literal `$`; a `$` followed by anything else stays as it
is. References are replaced after the file is parsed, so a variable's content
can never add a key or a section, and the replaced value is validated like a
literal one.

- A reference to a variable that is not set stops the start, naming the
  variable and the key.
- A variable set to the empty string gives the empty string.
- `${` without `}`, or a name that does not match `[A-Za-z_][A-Za-z0-9_]*`, is
  an invalid value of its key.
- Error messages never contain a variable's value.

## Keys of the server, the data and the logs

| Section | Key | Type | Default | Valid |
|---|---|---|---|---|
| `[server]` | `bind` | string | `"0.0.0.0"` | not empty |
| `[server]` | `port` | int | `8080` | 0–65535; 0 picks a free port |
| `[server]` | `external_url` | string | `""` | empty, or an absolute `http`/`https` URL |
| `[server]` | `max_body_size` | string | `"1MB"` | a positive integer followed by `KB`, `MB` or `GB`, any case, powers of 1024 |
| `[database]` | `path` | string | see below | not empty |
| `[workspace]` | `path` | string | see below | not empty |
| `[workspace]` | `workers` | int | `4` | at least 1 |
| `[logging]` | `level` | string | `"info"` | `trace`, `debug`, `info`, `warn`, `error`, any case |
| `[logging]` | `log_health_probes` | bool | `false` | |
| `[limits]` | `requests_per_minute` | int | `600` | 0 or more; 0 turns the limit off |

`max_body_size`, `requests_per_minute` and `log_health_probes` are handed to
the HTTP server (see [API](api.md)).

### OAuth providers

```toml
[[oauth.providers]]
name = "github"            # github or google, each at most once
client_id = "..."          # required, not empty
client_secret = "${GITHUB_SECRET}"   # required, not empty
# authorize_url, token_url, userinfo_url: absolute http(s) URLs
```

When the three URLs are not given, the provider's public endpoints are used:

| Provider | authorize_url | token_url | userinfo_url |
|---|---|---|---|
| `github` | `https://github.com/login/oauth/authorize` | `https://github.com/login/oauth/access_token` | `https://api.github.com/user` |
| `google` | `https://accounts.google.com/o/oauth2/v2/auth` | `https://oauth2.googleapis.com/token` | `https://openidconnect.googleapis.com/v1/userinfo` |

Every `client_secret` is registered with the log scrubber, so it never
appears in a log line.

### Data paths

| Key | Not set | Set to a bare file name | Set to another relative path | Absolute |
|---|---|---|---|---|
| `database.path` | `./data/parley.db`, or `$XDG_DATA_HOME/parley.db` when `XDG_DATA_HOME` is set and not empty | under `$XDG_DATA_HOME` when set, else the working directory | against the working directory | kept |
| `workspace.path` | `./data/workspaces`, or `$XDG_DATA_HOME/workspaces` | same | same | kept |

The directory of the database file is the **data directory**. It holds the
transactional store (`database.path`) and the analytical store
`analytics.db`. The data directory and the workspace root are created with
mode 0700 when missing; an existing directory keeps its mode.

## Keys and sections of other packages

Feature packages register their own keys with `config.Registry.Register`
(section, name, type, default, validation rule), or a whole section with
`RegisterSection(name, validator)`, whose validator receives the parsed
section and reports all of its failures in one error. A key or section
nobody registered is unknown. Registering the same `section.key` twice, a
section twice, or a section both key by key and as a whole stops the start.
The sections they own are described with each feature.
