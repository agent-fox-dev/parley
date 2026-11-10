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

`parley` alone prints its help and exits 0.

### parley login

Signs in and saves the hub URL, the credential and the user's id to the
client configuration file.

| Flag | Default | |
|---|---|---|
| `--provider <name>` | `github` | the identity provider to sign in with |
| `--expires <days>` | `90` | the new key's lifetime: 0, 30, 60 or 90 |
| `--api-key <key \| ->` | | store this credential instead of signing in; `-` reads it from stdin |

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
| 1 | the hub refused; its error envelope is printed on stderr as received |
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

The hub URL is the first non-empty value of `--endpoint-url` and
`PARLEY_ENDPOINT_URL`. Without one, only `cli` is printed. When the hub
cannot be read (refused, no answer within 5 seconds, a status other than 200,
a body that is not a JSON object), one line starting with `warning: ` and
naming the URL goes to stderr and `server` is left out. The exit status is
always 0.
