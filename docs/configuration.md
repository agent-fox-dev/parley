# Configuration

How to configure a hub and its client: where settings come from, what each
group of keys controls and when you would change it, and the tables of
keys with their types and defaults.

## Contents

| Section | What you find there |
|---|---|
| [How the hub is configured](#how-the-hub-is-configured) | Where the file is looked for, environment references, what a wrong file does at start |
| [The configuration file](#the-configuration-file) | A minimal file, a production file, the value formats |
| [Server and storage](#server-and-storage) | Listening, storage, logging, and the keys of forks, git, source control, the code index, changes, landing, claims and events |
| [Sign-in providers](#sign-in-providers) | Setting up sign-in end to end, and its keys |
| [Sandboxes](#sandboxes) | Enabling sandboxes: the gateway, limits, profiles, credentials, exposed secrets |
| [Secret keys](#secret-keys) | What the keys protect, running without one, rotating, losing one |
| [Retention](#retention) | What the hub removes by itself, and when |
| [Environment variables](#environment-variables) | Everything the hub reads from its environment |
| [Configuring the client](#configuring-the-client) | The client's configuration file and environment |

## How the hub is configured

The hub, `parleyd`, takes its whole configuration from one file,
`config.toml`, read once when it starts. Everything has a default, so a hub
without a file runs; you add a key only where the default does not fit. To
change a value you edit the file and restart the hub.

A setting reaches the hub in one of four ways, and each has one job:

| Source | What it carries | Why |
|---|---|---|
| Defaults | every key | a new hub runs without a file |
| `config.toml` | every setting an operator chooses | one file to review, version and diff |
| `${NAME}` references in the file | values you do not want in the file, such as a client secret | the file can be committed or put in a ConfigMap while the secret stays in the environment |
| Environment variables read directly | where the file is, where the data goes, the secret keys, proxies | they are needed before the file is read, or must never be in it |

No environment variable overrides a key of the file, and no flag sets one:
the file is the only place a key's value comes from, so what you read in it
is what the hub runs with. The two flags of `parleyd`, `--admin-email` and
`--reset-admin-token`, manage the admin token and exit without serving (see
[cli.md](cli.md#running-the-hub-parleyd)).

### Where the file is

`parleyd` looks for `config.toml` in `$XDG_CONFIG_HOME` when that variable
is set and not empty (a relative value is taken from the working
directory), and in the working directory otherwise. That directory is the
*config dir*. Relative paths in the file (a key file, a TLS certificate, a
token file) are taken from it, and the hub writes its admin token file,
`admin_token`, there, so the config dir must exist and be writable when you
bootstrap or reset the admin token. The hub does not start while that file
is there: save the token and delete the file.

When the hub starts, its `listening` log line carries the absolute path it
looked for as `config`, also when no file exists there.

### Environment references

In any string value, `${NAME}` is replaced by the value of the environment
variable `NAME`, and `$$` by one `$`. A `$` followed by anything else stays
as it is.

```toml
[[oauth.providers]]
name = "github"
client_id = "Iv1.0123456789abcdef"
client_secret = "${GITHUB_CLIENT_SECRET}"
```

| Value | Result |
|---|---|
| `"${GH_SECRET}"` | the value of `GH_SECRET` |
| `"https://${HOST}"` | `https://` and the value of `HOST` |
| `"pa$$word"` | `pa$word` |
| `"cost $5"` | `cost $5` |

References are replaced after the file is parsed and in string values only,
so a variable can never add, remove or rename a key, and never changes an
integer or a boolean. A reference to a variable that is not set stops the
start (a variable set to the empty string is set), and so do a `${` without
its `}` and a name that is not a letter or `_` followed by letters, digits
or `_`. The error names the key and the variable, never a value.

### What a wrong file does

The file is checked strictly, because a misspelled key that is silently
ignored is a setting you believe you made. Each of these stops the start
with exit status `1` and one `cannot start` line that names the line or the
key as `section.key`:

- invalid TOML;
- a section or a key the hub does not know;
- a value of the wrong type or outside its range;
- two keys that contradict each other, such as `[forks].max_ttl` shorter
  than `[forks].default_ttl`.

```json
{"time":"2026-11-13T12:11:38.609972Z","level":"ERROR","msg":"cannot start","error":"configuration file /srv/parley/config.toml: server.prot: unknown key"}
```

Nothing is opened or created before the file is accepted, so you can fix
the key and start again. An error about a key does not quote the key's
value. An error about invalid TOML carries the parser's message, which can
quote the text at the mistake, so read it before you paste it anywhere.
The `[sandbox]` and `[code_index]` sections report all their mistakes
together; elsewhere the first one is reported.

## The configuration file

### A minimal file

A hub with no file listens on `0.0.0.0:8080`, keeps its data under `data/`
in the working directory and is administered with the admin token. The
smallest file that lets people sign in adds one provider:

```toml
[[oauth.providers]]
name = "github"
client_id = "Iv1.0123456789abcdef"
client_secret = "${GITHUB_CLIENT_SECRET}"
```

### A production file

A hub that other people and agents reach needs its public URL, a place for
its data that you back up, a secret key, and usually a code index and
sandboxes:

```toml
[server]
bind = "0.0.0.0"
port = 8080
external_url = "https://parley.example.com"

[database]
path = "/var/lib/parley/data/parley.db"

[workspace]
path = "/var/lib/parley/data/workspaces"
workers = 4

[logging]
level = "info"

[secrets]
key_file = "/etc/parley/secrets.key"

[[oauth.providers]]
name = "github"
client_id = "Iv1.0123456789abcdef"
client_secret = "${GITHUB_CLIENT_SECRET}"

[land]
committer_name = "Parley"
committer_email = "parley@parley.example.com"

[code_index]

[sandbox]
gateway_url = "https://openshell.internal:8443"
tls_cert = "/etc/parley/openshell-client.crt"
tls_key = "/etc/parley/openshell-client.key"
tls_ca = "/etc/parley/openshell-ca.crt"

[[sandbox.credentials]]
name = "model"
provider_profile = "anthropic"
secret = "ANTHROPIC_API_KEY"

[[sandbox.profiles]]
name = "agent"
default = true
image = "registry.example.com/agent:1.4"
cpu = "2"
memory = "4Gi"
ttl = "4h"
credentials = ["model"]
```

The hub runs as one process on one data directory: never start a second
hub on the same data. In a container, the hub image sets
`XDG_CONFIG_HOME=/var/lib/parley/config` and
`XDG_DATA_HOME=/var/lib/parley/data` on one volume, so the file needs no
`[database]` or `[workspace]` path. The Kubernetes manifests in
`deploy/kubernetes/` mount `config.toml` from a ConfigMap as a single file
inside that config dir (so the admin token file can still be written beside
it), give the hub the keys of a Secret as environment variables for the
`${NAME}` references, and run exactly one replica with the `Recreate`
strategy.

### Value formats

| Format | Written as | Examples |
|---|---|---|
| Duration | a whole number followed by `s`, `m`, `h` or `d` (24 hours) | `"30s"`, `"5m"`, `"7d"` |
| Size | a positive whole number followed by `KB`, `MB` or `GB`, in any letter case, in powers of 1024 | `"1MB"`, `"32mb"` |
| Path | absolute, or relative to the config dir | `"secrets.key"` |

Durations and sizes are TOML strings. `[database].path` and
`[workspace].path` resolve differently from other paths (see
[Storage](#storage)).

## Server and storage

These sections decide where the hub listens, where it keeps its data, what
it logs, and the limits and timeouts of its features. The defaults suit a
hub of the size the [performance targets](performance.md) describe; change
a key when the reason given for it applies to you.

### Listening and the public URL

Set `external_url` on every hub that someone other than you reaches: the
hub cannot know the address clients see it at behind a proxy or an ingress,
and it needs that address for workspace URLs, for sign-in redirects and for
sandboxes.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `[server].bind` | string | `"0.0.0.0"` | the address the hub listens on; not empty |
| `[server].port` | integer | `8080` | the TCP port, `0` to `65535`; `0` lets the system choose |
| `[server].external_url` | string | `""` | the URL clients reach the hub at: empty, or an absolute `http` or `https` URL. A workspace's `hub_url` is built from it and is `null` while it is empty; a sign-in redirect to a page of the hub must match it; sandboxes require it |
| `[server].max_body_size` | size | `"1MB"` | the largest REST request body; a larger one is answered `413`. Commits and git pushes have their own limits, below |
| `[limits].requests_per_minute` | integer | `600` | how many REST requests one credential, or one client address without a valid credential, may send in 60 seconds; `0` turns the rate limit off (see [api.md](api.md#using-the-api)) |

### Storage

The hub keeps everything on local disk: a transactional store, an
analytical store (`analytics.db`, beside the transactional one) and the
repositories. The directory of `[database].path` is the *data directory*;
it is what `parleyd backup` copies. Put it on a disk you back up.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `[database].path` | string | see below | the transactional store's file; its directory is the data directory |
| `[workspace].path` | string | see below | the workspace root, where the repositories live |
| `[workspace].workers` | integer | `4` | how many workspace jobs, such as clones, run at once; at least `1`. Further work waits and starts in the order it was recorded. Raise it when many workspaces are created at the same time and the disk and network keep up |

| `path` is | With `XDG_DATA_HOME` set | Otherwise |
|---|---|---|
| not set | `$XDG_DATA_HOME/parley.db` and `$XDG_DATA_HOME/workspaces` | `data/parley.db` and `data/workspaces` in the working directory |
| a bare name, such as `hub.db` | under `$XDG_DATA_HOME` | under the working directory |
| a relative path with a directory | under the working directory | under the working directory |
| an absolute path | kept | kept |

The hub creates the data directory and the workspace root when they are
missing, with mode `0700`; a directory that exists keeps its mode.

### Logging

| Key | Type | Default | Meaning |
|---|---|---|---|
| `[logging].level` | string | `"info"` | `trace`, `debug`, `info`, `warn` or `error`, in any letter case; lines below it are not written |
| `[logging].log_health_probes` | boolean | `false` | write an access-log line, at debug level, for `/healthz` and `/readyz`. Off by default because probes would drown the log |

### Task forks

Task forks are cheap and short-lived, and these keys keep them that way:
every fork expires, and a workspace cannot collect them without bound (see
[api.md](api.md#task-forks-and-workspace-tokens)).

| Key | Type | Default | Meaning |
|---|---|---|---|
| `[forks].default_ttl` | duration | `"7d"` | the lifetime of a fork created without a `ttl`; at least `5m` |
| `[forks].max_ttl` | duration | `"30d"` | the longest `ttl` a fork may ask for (a request gives it in `m`, `h` or `d`, from `5m`); not shorter than `default_ttl` |
| `[forks].max_per_workspace` | integer | `200` | how many forks one workspace may have; at least `1`. Raise it for a workspace many agents work on at once |
| `[forks].expire_on_land` | boolean | `true` | remove a fork before its lifetime ends once its work is finished: all its changes landed or abandoned, every branch of its own the source of one of them, no activity for 24 hours and no sandbox `provisioning` or `ready`. `false` keeps every fork until it expires |

### Git and source control

The limits here protect the hub from one request that takes too long or
sends too much. Raise a size when your repositories need it; raise a
timeout when clones of a large repository over a slow link are cut off.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `[git].timeout` | duration | `"30m"` | how long one git fetch or push may take, above zero. A fetch that runs out is cut off; a push that runs out before its refs move changes nothing and reports every ref as failed; a push whose refs have started to move is finished |
| `[git].max_push_size` | string | `"0"` | the largest pack one push may send: `"0"` for no limit, a number of bytes such as `"104857600"`, or a size such as `"100MB"`. A larger push is refused whole. The same limit bounds a sandbox checkpoint's bundle |
| `[scm].read_timeout` | duration | `"30s"` | how long one source control read may take, above zero; one that runs out is answered `503` `scm_timeout`, or cut off when its answer has begun |
| `[scm].max_file_size` | size | `"32MB"` | the largest file the file route serves; a larger one is answered `413` `payload_too_large` |
| `[scm].max_commit_size` | size | `"32MB"` | the largest body of a commit through the API, in place of `[server].max_body_size`; a larger one is answered `413` `payload_too_large` |

See [api.md](api.md#git-over-http) and [api.md](api.md#source-control).

### Code index

The code index gives agents code search and symbol lookup over the default
branch of a workspace. It is off unless the file holds the `[code_index]`
section; the section alone, even empty, turns it on. The hub then keeps a
tree and a search index for every repository workspace whose `code_index`
setting is on, under `code-index/` in the data directory, and rebuilds them
in the background whenever the default branch moves. Leave it off on a hub
whose disk cannot hold a second copy of each default branch. On a platform
where the index cannot run, a hub with the section refuses to start.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `[code_index].max_files` | integer | `100000` | the most files one index holds, `1` or more; a workspace with more gets a partial index |
| `[code_index].max_size` | size | `"1GB"` | the most bytes of content one index holds; past it the index is partial |
| `[code_index].build_timeout` | duration | `"5m"` | the longest one index may take to build, above zero; an index that reaches it keeps what it has as a partial index |
| `[code_index].workers` | integer | `2` | how many builds run at once, `1` to `16`; others wait. Builds share the hub's job queue and never take its last place, so a value above `3` acts as `3` |

See [api.md](api.md#source-control).

### Changes and landing

| Key | Type | Default | Meaning |
|---|---|---|---|
| `[changes].mergeable_debounce` | duration | `"5s"` | the shortest time between two mergeability checks of one change; `"0s"` for none. It keeps a burst of pushes from computing the same answer many times; lower it when you want `mergeable` sooner and have CPU to spare |
| `[changes].max_stack_depth` | integer | `20` | the deepest a change may sit in a stack, itself included; at least `1`. A deeper change is still recorded, flagged `needs_restack` with the reason `stack_too_deep` |
| `[land].max_attempts` | integer | `10` | how many times one land attempt tries to move its target branch when other writes keep moving it first; at least `1`. When every try loses, the attempt fails with `contention` |
| `[land].timeout` | duration | `"5m"` | how long one land attempt may take, waiting for a worker included; above zero. One that runs out fails with `timeout` and never moves the target |
| `[land].workers` | integer | `8` | how many land attempts are computed at once; at least `1`. Further attempts wait and none fails because all workers are busy |
| `[land].committer_name` | string | `"Parley"` | the committer name of every commit a land or a rebuild makes; not empty |
| `[land].committer_email` | string | `"parley@localhost"` | the committer e-mail address of those commits; not empty. Set it to an address of your domain so the commits are attributable |

See [api.md](api.md#changes) and [api.md](api.md#landing).

### Claims

A claim tells other agents that someone is working on a path. Claims expire
so that a crashed agent does not block a path for ever (see
[api.md](api.md#awareness)).

| Key | Type | Default | Meaning |
|---|---|---|---|
| `[awareness].default_claim_ttl` | duration | `"1h"` | the lifetime of a claim created without a `ttl`; above zero, not longer than `max_claim_ttl` |
| `[awareness].max_claim_ttl` | duration | `"24h"` | the longest `ttl` a claim may ask for, at creation and at renewal; above zero |
| `[awareness].max_claims_per_workspace` | integer | `1000` | how many active claims one workspace, its task forks included, may hold; at least `1`. A further claim is answered `429` `claim_limit_reached` |

### Events, telemetry and event webhooks

| Key | Type | Default | Meaning |
|---|---|---|---|
| `[events].retention` | duration | `"30d"` | how long an event stays in the event log that streams and long-polls read; at least `1h` and not longer than `[audit].max_age` (see [Retention](#retention)) |
| `[events].max_sse_connections` | integer | `500` | how many event streams may be open at once; at least `1`. A further stream is answered `503` with `Retry-After: 5`; held long-polls do not count |
| `[audit].write_timeout` | duration | `"2s"` | how long one write of agent telemetry may take; also takes `ms`, at least `100ms`. A write that runs out stores nothing and is answered `503` `unavailable` with `Retry-After: 5` |
| `[webhooks].max_per_workspace` | integer | `20` | how many event webhooks one workspace may have; `0` or more, and `0` refuses every create. One beyond it is answered `429` `webhook_limit_reached` |
| `[webhooks].timeout` | duration | `"10s"` | how long one delivery attempt may take until the receiver's status, from `1s` to `60s` |
| `[webhooks].allow_private` | boolean | `false` | let a webhook's URL name or resolve to a loopback, link-local, private, shared or otherwise internal address. Off by default so that a webhook cannot be pointed at your internal network or a cloud metadata address; turn it on only when the receivers live there |
| `[webhooks].allow_insecure` | boolean | `false` | accept `http://` URLs to a loopback host (`127.0.0.0/8`, `::1`, `localhost`), for a receiver on the hub's own machine; every other URL still needs `https` |

See [api.md](api.md#audit-and-events) and [api.md](api.md#event-webhooks).

### Fixed values

These are not configurable; plan around them.

| Setting | Value |
|---|---|
| Shutdown | in-flight requests get 15 seconds after `SIGTERM` or `SIGINT`, and the whole stop takes at most 30 seconds; give the hub a little longer before you kill it |
| Idempotency records | kept for 24 hours, across restarts |
| Event webhook delivery records | kept for 7 days after their last attempt |
| Claims per session | at most 50 active |
| Workspace sandbox rules | at most 50 per workspace |

## Sign-in providers

People sign in to the hub through an identity provider; the hub has no
passwords. Each `[[oauth.providers]]` table configures one provider, and a
hub without any is used with the admin token and the tokens it issues.

When someone signs in, the hub exchanges the provider's code, reads a
stable user id and a verified e-mail address, creates the user on first
sign-in, and issues an API key. The first user whose e-mail equals the
address given to `parleyd --admin-email` becomes an admin, as long as no
admin user exists.

### Setting one up

1. Register an OAuth application with the provider (GitHub or Google). As
   its redirect URI, allow what your clients use: `parley login` listens on
   `http://127.0.0.1:<port>/callback`, and the hub accepts a redirect that
   is `http` on `localhost` or `127.0.0.1`, or that has the scheme, host
   and port of `[server].external_url`.
2. Give the hub the client id in the file and the client secret through
   the environment:

   ```toml
   [server]
   external_url = "https://parley.example.com"

   [[oauth.providers]]
   name = "github"
   client_id = "Iv1.0123456789abcdef"
   client_secret = "${GITHUB_CLIENT_SECRET}"
   ```

3. Restart the hub with `GITHUB_CLIENT_SECRET` set, and check that it
   offers the provider:

   ```sh
   curl https://parley.example.com/api/v1/auth/providers
   ```

4. Sign in:

   ```sh
   parley login --endpoint-url https://parley.example.com --provider github
   ```

See [cli.md](cli.md#parley-login-and-your-account) and
[api.md](api.md#signing-in-and-your-account).

### Keys

| Key | Type | Default | Meaning |
|---|---|---|---|
| `name` | string | required | `github` or `google`; each at most once |
| `client_id` | string | required | the application's client id; not a secret |
| `client_secret` | string | required | the application's client secret; the hub replaces it by `[redacted]` in every log line |
| `authorize_url`, `token_url`, `userinfo_url` | string | the provider's public endpoints | absolute `http` or `https` URLs; set them for a self-hosted provider such as GitHub Enterprise |

The `[oauth]` section holds nothing but these tables. The default endpoints
are:

| Provider | `authorize_url` | `token_url` | `userinfo_url` |
|---|---|---|---|
| `github` | `https://github.com/login/oauth/authorize` | `https://github.com/login/oauth/access_token` | `https://api.github.com/user` |
| `google` | `https://accounts.google.com/o/oauth2/v2/auth` | `https://oauth2.googleapis.com/token` | `https://openidconnect.googleapis.com/v1/userinfo` |

## Sandboxes

A sandbox is where an agent runs: an isolated environment with a task fork
checked out. Parley does not isolate anything itself. Sandboxes run on an
OpenShell gateway that you operate, and the hub is its control plane: it
decides who gets a sandbox of which shape, connects it to the fork and the
secrets, enforces lifetimes and quotas, and saves the work.

Two defaults shape everything below:

- **No network.** A sandbox reaches the hub and nothing else. Code an
  agent writes or downloads is untrusted, so every other destination is
  one you allowed: through a profile's policy, an attached credential, or
  a rule a person approved.
- **No real credentials.** A sandbox sees placeholders. The gateway puts
  the real value on a request outside the sandbox, and only on requests
  to the endpoints that credential is for. A leaked environment therefore
  leaks nothing, unless you chose to expose a secret.

Sandboxes are off unless the file holds a `[sandbox]` section; without it
every sandbox route answers `404` `sandboxes_disabled`. See
[architecture.md](architecture.md#sandboxes) for the model and
[api.md](api.md#sandboxes) for using one.

### Enabling sandboxes

1. **Point the hub at the gateway** and choose exactly one way to
   authenticate. Sandboxes also need `[server].external_url`, because a
   sandbox reaches the hub at that address.

   ```toml
   [server]
   external_url = "https://parley.example.com"

   [sandbox]
   gateway_url = "https://openshell.internal:8443"
   token_file = "openshell.token"
   ```

2. **Define at least one profile**, the template of a sandbox. The
   profile's image must hold `git` and a writable working directory
   `/workspace`; `containers/sandbox/Dockerfile` builds a reference image.

   ```toml
   [[sandbox.profiles]]
   name = "agent"
   default = true
   image = "registry.example.com/agent:1.4"
   ttl = "4h"
   ```

3. **Attach credentials** the agent needs, such as a model key. A
   credential joins a secret stored in the hub to a provider profile of
   the gateway, which says which variable holds the placeholder, which
   hosts the credential is for and which programs may use it.

   ```toml
   [[sandbox.credentials]]
   name = "model"
   provider_profile = "anthropic"
   secret = "ANTHROPIC_API_KEY"

   [[sandbox.profiles]]
   name = "agent"
   default = true
   image = "registry.example.com/agent:1.4"
   ttl = "4h"
   credentials = ["model"]
   ```

   Store the secret where the sandbox's workspace finds it (see
   [cli.md](cli.md#parley-secrets-and-parley-vars)); storing needs a
   [secret key](#secret-keys):

   ```sh
   printf '%s' "$ANTHROPIC_API_KEY" | parley secrets create ANTHROPIC_API_KEY --from-stdin --org acme
   ```

4. **Expose a secret only when a tool cannot work with a placeholder**,
   such as a client that signs requests itself (see
   [Exposed secrets](#exposed-secrets)).

5. **Restart the hub.** Before it listens, it checks the gateway, within
   30 seconds, and does not start if a check fails: the gateway is healthy
   and its release is supported; the OpenShell workspace exists (the hub
   creates it); every `provider_profile` you named is known to the gateway;
   no exposed secret has the name of a variable an attached provider
   profile declares; every `policy` file is valid. Then list the profiles
   and create a sandbox:

   ```sh
   parley sandbox profiles
   parley sandbox create acme-app --wait
   ```

### The gateway

| Key | Type | Default | Meaning |
|---|---|---|---|
| `runtime` | string | `"openshell"` | the sandbox runtime; `openshell` only |
| `gateway_url` | string | required | the gateway: an `https` URL, or an `http` URL to `localhost`, `127.0.0.0/8` or `::1`; without user information, query, fragment, or a path other than `/` |
| `tls_cert`, `tls_key` | path | `""` | the client certificate and its key for mutual TLS; both or neither |
| `tls_ca` | path | `""` | CA certificates the gateway's certificate is checked against; may accompany any way to authenticate |
| `token_file` | path | `""` | a file holding a bearer token, not empty; read once, at start |
| `oidc_issuer`, `oidc_client_id`, `oidc_client_secret_file` | string, string, path | `""` | OIDC client credentials, all three or none; the hub obtains a token from the issuer and renews it before it expires |
| `openshell_workspace` | string | `"parley"` | the OpenShell workspace the hub owns: 1 to 63 lower-case letters, digits, `_` or `-`, starting with a letter or a digit. A gateway of release 0.1.9 accepts at most 19 lower-case letters, digits and `-` |

Configure exactly one of mutual TLS, `token_file` and OIDC; none or more
than one stops the start. The hub supports OpenShell releases from `0.1.0`
up to, but not including, `0.2.0`. It owns everything in its OpenShell
workspace, so two hubs must not share one.

### Limits

| Key | Type | Default | Meaning |
|---|---|---|---|
| `max_ttl` | duration | `"24h"` | the ceiling of every profile's `ttl`; above zero |
| `idle_timeout` | duration | `"30m"` | how long a `ready` sandbox may go without activity before the hub stops it, for profiles that do not say; `0` disables the idle stop |
| `max_sandboxes` | integer | `200` | how many sandboxes may be `provisioning` or `ready` on the hub; at least `1` |
| `max_per_org` | integer | `100`, or `max_sandboxes` when smaller | the same count per organization; at least `1`, at most `max_sandboxes` |
| `max_per_workspace` | integer | `50` | the same count per workspace with its task forks; at least `1` |
| `max_per_user` | integer | `20` | the same count per creating user; at least `1` |
| `max_exec_timeout` | duration | `"30m"` | the longest `timeout` an exec may ask for; a longer one is refused. An exec without one gets 60 seconds, or this value when it is smaller |
| `max_concurrent_execs` | integer | `8` | how many execs may run at once in one sandbox; another is answered `429` `rate_limited` |
| `max_transfer_size` | size | `"1GB"` | the largest upload or download |
| `log_retention` | duration | `"7d"` | how long the stored log lines of a sandbox are kept; above zero |
| `reconcile_interval` | duration | `"1m"` | how often the hub brings its records back in line with the runtime (exits, lost sandboxes, leftovers), from `10s` to `10m`. Idle stops and expiries are checked every minute whatever this says |
| `allow_workspace_rules` | boolean | `true` | let an approval of a network proposal save its rule on the workspace, for every later sandbox there. With `false` such an approval is answered `403` `workspace_rules_disabled`; rules saved earlier still apply and can be deleted |

A create that would exceed a quota is answered `429` `sandbox_capacity`.
The quotas exist because every sandbox holds resources of the runtime: size
them to what the gateway's cluster can run.

### Profiles

A profile is fixed for the life of the hub process: to change one, edit the
file and restart. A `[[sandbox.profiles]]` table holds:

| Key | Type | Default | Meaning |
|---|---|---|---|
| `name` | string | required | 1 to 63 lower-case letters, digits, `_` or `-`, starting with a letter or a digit; unique |
| `default` | boolean | `false` | the profile a sandbox gets when its creator names none; at most one profile sets it |
| `description` | string | `""` | shown in the profile list |
| `image` | string | `""` | the sandbox image; empty leaves it to the runtime |
| `cpu`, `memory` | string | `""` | resource limits as the runtime understands them, such as `"2"` and `"4Gi"`; empty leaves them to the runtime |
| `gpu` | integer | `0` | the number of GPUs asked for |
| `ttl` | duration | required | the default and longest lifetime of a sandbox of this profile; above zero, at most `max_ttl` |
| `idle_timeout` | duration | the section's `idle_timeout` | as the section's key, for this profile |
| `policy` | path | `""` | an OpenShell policy file, the base network policy; empty is the built-in policy, which allows no egress and lets the workload write only its working directory |
| `credentials` | list of strings | `[]` | names of `[[sandbox.credentials]]` tables every sandbox of the profile gets; a secret that is missing fails the creation |
| `optional_credentials` | list of strings | `[]` | as `credentials`, skipped when the secret does not exist; a name may not be in both lists |
| `expose_secrets` | list of strings | `[]` | names of secrets whose real values go into the sandbox's environment (see below) |
| `optional_expose_secrets` | list of strings | `[]` | as `expose_secrets`, skipped when the secret does not exist; a name may not be in both lists |
| `hub_access` | string | `"fork"` | `fork`: the sandbox gets a credential for its workspace on the hub, as a placeholder in `PARLEY_API_KEY`. `none`: it cannot reach the hub |
| `env` | table of strings | `{}` | fixed, non-secret environment; shown in responses. A key is a valid variable name and does not start with `PARLEY_` or `OPENSHELL_` |
| `checkpoint` | boolean | `true` | save the sandbox's work to the fork when it stops, expires or is deleted, and when its main process ends |
| `allow_proposals` | boolean | `true` | let sandboxes of the profile ask a person for more network access; with `false` their proposals are rejected at the runtime |
| `tool_server` | string | `""` | the absolute path of the tool server (`agentkit-tools`) in the image, at most 256 bytes; empty gives the sandboxes no tools. The path is not checked at start: an image without the file answers `502` `tool_server_failed` on first use |

The hub adds the rule for its own API and git endpoints, and the rules of
the attached credentials, to whatever base policy a profile has. The hub
validates a `policy` file against the gateway at start, and a gateway that
cannot validate one stops the start. Current OpenShell gateways cannot, so
leave `policy` empty and widen network access through credentials and
approved proposals.

### Credentials

A `[[sandbox.credentials]]` table holds exactly:

| Key | Type | Meaning |
|---|---|---|
| `name` | string | what profiles refer to; the shape of a profile name, unique |
| `provider_profile` | string | the id of a provider profile the gateway knows; not empty |
| `secret` | string | the name of the secret holding the value; not empty |

### Exposed secrets

`expose_secrets` puts the real value of a secret into the sandbox's
environment, in a variable with the secret's name. It is off unless you
list a name, because it gives up the protection above:

- code in the sandbox, anything the agent downloads included, can read
  the value and send it wherever the sandbox's network policy allows;
- a program can write it to a file, and a checkpoint or a commit can then
  carry it into the fork;
- the hub redacts the value only in the sandbox logs it stores and in the
  text of events and errors, not in exec output, the terminal, downloaded
  files or checkpoints.

Prefer a credential whenever the tool can work with a placeholder. The
profile list and every sandbox show the names of exposed secrets, never a
value. A name must be a valid variable name, must not start with `PARLEY_`
or `OPENSHELL_`, must not be a key of the profile's `env`, and must not be
one of the reserved secret names `GIT_PAT`, `GIT_USERNAME`, `GIT_PASSWORD`,
`UPSTREAM_GIT_PAT`, `UPSTREAM_GIT_USERNAME`, `UPSTREAM_GIT_PASSWORD` and
`FORGE_WEBHOOK_SECRET`, in any letter case. Whether the secret exists is
checked when a sandbox is created, not when the hub starts.

### Mistakes in the section

The whole section is checked before any store is opened, and every mistake
is reported in one error, each naming its key: `sandbox.<key>`,
`sandbox.profiles[<name>].<key>`, `sandbox.credentials[<name>].<key>` or
`server.external_url`. The error quotes configured names where that helps,
and never a secret, the content of a file or an `env` value.

```text
configuration file /srv/parley/config.toml: sandbox.max_ttl: must be more than 0; sandbox.gatway_url: unknown key; server.external_url: must be set when the [sandbox] section is present
```

## Secret keys

The hub stores secrets for you: origin and forge credentials, the secrets
sandboxes use, webhook signing secrets. It encrypts every one of them at
rest with a key that only you hold, so that a copy of the data directory or
a backup does not give the secrets away. The key is not in the data
directory unless you put it there, and the hub never logs it.

A key is 32 random bytes in standard base64 with padding. Create one with:

```sh
openssl rand -base64 32
```

White space around it is ignored; the URL-safe alphabet is not accepted.

| Source | Holds |
|---|---|
| `PARLEY_SECRETS_KEY` | the current key |
| `[secrets].key_file` | a file holding the current key, read only when `PARLEY_SECRETS_KEY` is unset or empty; a relative path is taken from the config dir |
| `PARLEY_SECRETS_KEY_PREVIOUS` | the previous key, during a rotation |

The keys are loaded before the hub opens its stores, for a start, for
`--admin-email`, `--reset-admin-token` and `backup` alike. A malformed key
or a key file that cannot be read stops the hub with one error line naming
the variable or the file, never the value, and nothing has been touched
(see [adr/06](adr/06-load-the-secret-keys-before-the-stores-open.md)).

When the hub starts it writes the line `secret encryption` with
`key_configured`, `previous_key_present` and `undecryptable_secrets`, the
number of stored values neither key opens. Watch that last number after
every change of keys.

### Without a key

A hub without a current key starts and warns that secrets cannot be
stored. It lists and deletes secrets and refuses to store one, and features
that need to store or read a secret answer `503` `secrets_unavailable`.
That is fine for a first try, not for a hub people use.

### Rotating the key

1. Restart the hub with the new key as the current key and the former one
   as `PARLEY_SECRETS_KEY_PREVIOUS`. New values are sealed under the new
   key at once, and values under the former key still open.
2. Wait. Right after the start and then every hour, the hub seals the
   values under the former key again under the new one, and writes the
   line `secret re-encryption` with `remaining`, the number of values not
   yet under the new key.
3. Once a pass reports `remaining` `0`, restart the hub without
   `PARLEY_SECRETS_KEY_PREVIOUS`.

A value the former key does not open stays as it is and keeps `remaining`
above `0`.

### Losing a key

There is no recovery: a value sealed under a key you no longer have cannot
be opened by anyone. The hub still starts, counts those values in
`undecryptable_secrets`, and lets you list and delete them; you store each
secret again under the new key. Keep the key where your backups of the data
directory are not, and keep it as long as any backup sealed under it.

## Retention

The hub's audit records, telemetry and sessions grow with every agent run.
Retention removes what is older than you need, so the stores stay bounded.
Each kind of record has its own limit, so you can keep postmortems and the
audit trail longer than the bulkier traces.

A retention run starts right after the hub starts and then every hour. It
removes:

| What | When | Key | Default |
|---|---|---|---|
| A session that is still `active` | ended as `timeout` once it started longer ago than | `[sessions].max_active_age`, at least `1h` | `"7d"` |
| Run events, session outcomes, tool calls, tool errors | older than | `[audit].max_age`, at least `1d` | `"90d"` |
| The telemetry of a workspace's oldest runs (postmortems and sessions are kept) | when the workspace has telemetry of more runs than | `[audit].max_runs`, at least `1` | `50` |
| Trace records | older than | `[audit].trace_max_age`, at least `1d` | `"30d"` |
| Postmortems | ingested longer ago than | `[audit].postmortem_max_age`, at least `1d` | `"180d"` |
| Ended sessions with their usage records | started longer ago than | `[sessions].max_age`, at least `1d` | `"90d"` |
| Events in the event log (they stay readable through the audit route) | older than | `[events].retention`, at least `1h` | `"30d"` |
| Event webhook delivery records that are not `pending` | last attempted more than 7 days ago | fixed | |
| Hub events | older than | `[audit].max_age` | `"90d"` |
| Everything recorded for a workspace that no longer exists | each record older than | `[audit].orphan_retention`, at least `1d` | `"30d"` |

Two things are separate from this run: a sandbox's stored log lines are
kept for `[sandbox].log_retention`, and no environment variable sets any
retention value.

What you can rely on:

- A record is removed only when it is strictly older than its limit.
- An event leaves the event log before it can be deleted, so the log never
  has a hole. A client that resumes a stream from a position that has left
  the log is answered `410` `cursor_expired`; the events themselves stay
  readable through the audit route until `[audit].max_age`. This is why
  `[events].retention` may not be longer than `[audit].max_age`.
- The newest event is never removed, whatever its age.
- An `active` session is never deleted, only ended.
- The records of a task fork count as orphaned only when its parent
  workspace is gone too.

Lower `[audit].max_age` or `[audit].max_runs` when the analytical store
grows faster than your disk allows; raise them when you audit further
back. Raise `[events].retention` when clients stay disconnected for longer
than 30 days and must resume their stream.

### Watching retention

A run has eleven steps: `stale_sessions`, `run_records`, `max_runs`,
`traces`, `postmortems`, `sessions`, `event_log`, `webhook_deliveries`,
`hub_events`, `orphans` and `gauges`, which refreshes the metrics
`parley_audit_records` and `parley_agent_sessions_active`. A step that
fails is logged as `retention step failed`, counted in
`parley_retention_errors_total{step}`, and does not stop the steps after
it. `parley_retention_last_run_timestamp_seconds` moves only when every
step succeeded, so alert on it standing still (see
[api.md](api.md#health-version-and-metrics)).

## Environment variables

The hub reads these variables itself, once, when it starts:

| Variable | Effect |
|---|---|
| `XDG_CONFIG_HOME` | the config dir, where `config.toml` is read and `admin_token` is written; default: the working directory |
| `XDG_DATA_HOME` | where the stores and the workspace root go when `[database].path` and `[workspace].path` do not say (see [Storage](#storage)) |
| `PARLEY_SECRETS_KEY` | the current secret key (see [Secret keys](#secret-keys)) |
| `PARLEY_SECRETS_KEY_PREVIOUS` | the previous secret key, during a rotation |
| any variable a `${NAME}` reference names | its value replaces the reference (see [Environment references](#environment-references)) |
| `PATH` | where `git` is found |
| `GIT_SSL_CAINFO`, `SSL_CERT_FILE` | passed on to the `git` the hub runs against an origin, for an origin whose certificate a private authority signed |
| `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` | passed on to that `git`, and used (also under their lower-case names) for the hub's requests to a forge |

The hub runs `git` with a minimal environment of its own, away from the
configuration and credential stores of the account that runs it. Apart from
`PATH` and the variables above, nothing of the hub's environment reaches
`git`, so set them for `parleyd` itself when an origin sits behind a
private certificate authority or a proxy. Two consequences are worth
knowing: an `ssh` origin is contacted in batch mode, so a call that would
need a password or a host key answer fails instead of waiting; and a
remote must be `https://`, `ssh://` or `[user@]host:path`, with no other
transport reachable through a redirect or a helper.

The hub reads no forge token or API URL from its environment:
`GITHUB_TOKEN`, `GH_TOKEN`, `GITLAB_TOKEN`, `GITHUB_API_URL` and
`GITLAB_API_URL` are ignored. Forge credentials are stored as secrets (see
[api.md](api.md#forge)).

## Configuring the client

`parley` needs two things to talk to a hub: the hub's URL and a credential.
Each comes from the first of these that is not empty:

| Setting | Flag | Variable | Key in the client file |
|---|---|---|---|
| Hub URL | `--endpoint-url` | `PARLEY_ENDPOINT_URL` | `endpoint_url` |
| Credential | `--api-key` | `PARLEY_API_KEY` | `api_key` |

The order lets one machine hold a default in the file, a script or a
sandbox override it with variables, and a single command override both with
a flag. Values are not trimmed. The credential is an API key, a personal
access token or the admin token. The hub URL must be an absolute `http` or
`https` URL with a host and without a query or fragment.

```sh
# once, on your machine: writes the file
parley login --endpoint-url https://parley.example.com

# in automation: no file needed
export PARLEY_ENDPOINT_URL=https://parley.example.com
export PARLEY_API_KEY="$PARLEY_PAT"
parley workspace list
```

A command that needs a setting and finds none fails with a `client_error`
before it sends anything. `parley version` without a URL prints the
client's build alone.

### The client configuration file

The file is `config.toml` in the client's config directory:
`$PARLEY_CONFIG_DIR` when that is set and not empty, `.parley` in your home
directory otherwise.

| Key | Value |
|---|---|
| `endpoint_url` | the hub URL, without trailing slashes |
| `api_key` | the credential of the last login |
| `user_id` | the `id` of the user the credential belongs to |

```toml
api_key = "parley_abcd1234_0123456789abcdefghijABCDEFGHIJ01"
endpoint_url = "http://127.0.0.1:8080"
user_id = "0b5c6f2e-8d1a-4c3b-9e7f-2a4d6c8e0f13"
```

The file holds a credential, so it is private: `parley login` is the only
command that writes it, and it always leaves it with mode `0600` (and
creates a missing config directory with mode `0700`). A login replaces the
file as a whole or not at all. It sets the three keys and keeps every other
key and table, but not comments or the order of keys. `parley login` takes
a credential from `--api-key` alone, never from `PARLEY_API_KEY` or the
file.

A command reads the file only when its flags and the variables leave a
setting empty, so a command given everything by flags or variables never
touches it. A missing file counts as empty. A file that cannot be read, is
not valid TOML, or holds one of the three keys with a value that is not a
string is a `client_error` naming the file's path, never its text: the
command fails before any request, and `parley version` prints a warning and
the client's build. A login that cannot read the file fails and leaves it
as it is.

See [cli.md](cli.md#using-parley) and
[cli.md](cli.md#parley-login-and-your-account).
