# CLI guide

Parley is two programs. **`parleyd`** is the hub: the server an operator
runs. **`parley`** is its client: everything a developer, a script or an
agent does with a hub from a terminal. This guide shows what to type to get
something done and explains why the commands behave as they do.

`parley` speaks only the hub's REST API, so every command here has a route
behind it. The guide says what a command is for and how to use it; the
[API reference](api.md) holds the semantics (states, errors, policy), and
each section links to the part it drives.

## Contents

| Section | What you find there |
|---|---|
| [Getting started](#getting-started) | From nothing to a landed change: start a hub, sign in, register, fork, push, land, publish |
| [Running the hub: `parleyd`](#running-the-hub-parleyd) | First start and the admin token, flags, start checks, backup and restore, logs, stopping, exit statuses |
| [Using `parley`](#using-parley) | How it finds the hub and the credential, output, exit statuses, `--wait`, help, the git credential helper |
| [`parley login` and your account](#parley-login-and-your-account) | Signing in, your API keys, access tokens for agents |
| [`parley admin`](#parley-admin) | Users, organizations, and other people's keys and tokens |
| [`parley workspace`](#parley-workspace) | Registering a repository, sync, push to origin, archive, reclone, delete |
| [`parley fork`](#parley-fork) | A task fork, its token and optionally its sandbox |
| [`parley scm`](#parley-scm) | Reading and writing a repository without a clone |
| [`parley change`](#parley-change) | Opening, reading, reviewing, restacking and landing changes |
| [`parley land` and `parley policy`](#parley-land-and-parley-policy) | Land attempts of a workspace and its landing policy |
| [`parley activity` and `parley claim`](#parley-activity-and-parley-claim) | Seeing who else touches a set of paths, and announcing your own work |
| [`parley patch` and `parley rebuild`](#parley-patch-and-parley-rebuild) | Carry-patch workspaces: patches, rebuilds, remembered resolutions |
| [`parley secrets` and `parley vars`](#parley-secrets-and-parley-vars) | Secrets and variables per user, organization and workspace |
| [`parley forge`](#parley-forge) | Issues, pull requests and labels on the forge, and its webhook |
| [`parley sandbox`](#parley-sandbox) | Creating sandboxes, running commands, copying files, network access |
| [`parley session`](#parley-session) | Recording an agent run and its token usage |
| [`parley events`, `parley audit` and `parley webhook`](#parley-events-parley-audit-and-parley-webhook) | Following events, asking who did what, subscribing a URL |
| [`parley mcp`](#parley-mcp) | Serving the commands as tools to an agent over MCP |

## Getting started

This page takes you from nothing to a landed change. It needs `git` 2.38 or
later on the hub's host, and one sign-in provider in the hub's configuration
for the browser sign-in (see
[configuration.md](configuration.md#sign-in-providers)).

**1. Start a hub.** The first run creates the admin token and exits; the
second one serves.

```sh
parleyd --admin-email ops@example.com   # writes the file admin_token, exits 0
# save the token somewhere safe, then delete the file
parleyd                                 # listens on 0.0.0.0:8080
```

The token file's path is in the log line of the first run. The hub refuses
to start while the file is still there (see
[First start and the admin token](#first-start-and-the-admin-token)).

**2. Sign in.** This opens the browser, has the hub issue you an API key,
and saves the hub URL and the key in `~/.parley/config.toml`, so later
commands need no flags.

```sh
parley login --endpoint-url http://localhost:8080
```

**3. Register a repository.** A workspace is the hub's copy of one git
repository. `--wait` returns once the hub has cloned it.

```sh
parley workspace create --slug demo \
    --git-url https://github.com/acme/demo.git \
    --git-pat "$GITHUB_TOKEN" --wait
```

**4. Fork it for a task.** A task fork is a cheap, short-lived copy for one
piece of work. Ask for a token with it: the token works only on this fork,
which is what you hand to an agent.

```sh
parley fork create demo --name greeting \
    --token-permissions git:write,changes:write \
    --purpose "Add a greeting endpoint" > fork.json
REMOTE=$(jq -r .remote fork.json)        # http://localhost:8080/git/<org>/demo--greeting.git
TOKEN=$(jq -r .token.token fork.json)    # shown once; this is the agent's credential

parley activity demo --paths 'api/**'    # is anyone else working here?
```

**5. Push.** Let git take its credential from `parley` (once per machine),
clone the fork, commit, and push. The agent works with the fork's token, so
it goes into the environment. A push to a fork opens a change; the push
options give it a title and an intent.

```sh
git config --global credential.http://localhost:8080.helper '!parley credential-helper'
export PARLEY_API_KEY=$TOKEN

git clone --filter=blob:none "$REMOTE" && cd demo--greeting
git switch -c greeting
# ... edit, git commit ...
git push -u origin greeting \
    -o parley.title="Add greeting endpoint" \
    -o parley.intent="Issue #41 asks for a greeting route"
# remote: change demo#12 version 1 (opened)
```

**6. Land.** Landing puts the change on its target branch on the hub, when
the workspace's policy allows it. If someone else landed first and the two
changes do not conflict, the hub rebases and lands anyway. Here you land it
yourself, with your own key.

```sh
unset PARLEY_API_KEY
parley change land demo 12 --wait
```

**7. Publish.** The hub's `main` now holds the change; push it to the
repository's origin.

```sh
parley workspace push demo main
```

An agent without a checkout does steps 5 and 6 with
[`parley scm commit`](#parley-scm) and [`parley change open`](#parley-change)
instead of git. On a hub with sandboxes, `parley fork create demo
--sandbox-profile agent -- my-agent …` starts the agent in a sandbox that has
the fork checked out (see [`parley sandbox`](#parley-sandbox)).

## Running the hub: `parleyd`

```text
parleyd [flags]
parleyd backup <directory>
```

Without a command, `parleyd` reads its configuration, opens its data, listens
for HTTP on `[server].bind` and `[server].port` (`0.0.0.0:8080` by default)
and runs until it receives `SIGTERM` or `SIGINT`. Everything you can configure
is in [configuration.md](configuration.md#how-the-hub-is-configured); the
command line only bootstraps the admin token and takes backups.

### Flags

| Flag | Effect |
|---|---|
| `--admin-email <address>` | on a hub that has no admin token yet: store the address, write a new admin token to the token file and exit `0` without serving. Ignored once the hub has a token. `--admin-email=` counts as not given |
| `--reset-admin-token` | write a new admin token to the token file, replacing the stored one, and exit `0` without serving |
| `-h`, `--help` | print the usage and exit `0`, without reading the configuration |

Flags come before the command. A flag `parleyd` does not know, a positional
argument other than `backup`, or `backup` with anything but one directory is a
usage error: one line and the usage on stderr, exit status `2`, nothing read
or created.

### First start and the admin token

A new hub has no users, so somebody has to be able to administer it before
anyone can sign in. That is the admin token: a credential of the form
`parley_admin_` plus 64 hex characters, which the
[`parley admin`](#parley-admin) commands accept. The hub keeps only its hash.

`parleyd --admin-email <address>` creates it. The token is written to the
file `admin_token` in the hub's config directory (the directory `config.toml`
is read from, see [configuration.md](configuration.md#the-configuration-file)),
with mode `0600`. It is never written to stdout, to the log or under the data
directory. The run logs the file's path and exits:

```json
{"time":"2026-11-13T12:20:41.118034Z","level":"WARN","msg":"admin token written: save the token and delete the file","path":"/srv/parley/admin_token"}
```

To keep it safe:

1. Copy the token into your secret manager.
2. Delete the file. The hub refuses to start while it exists, so a token
   cannot sit forgotten on disk next to a running hub.
3. Start `parleyd` without flags.

If you lose the token, stop the hub and run `parleyd --reset-admin-token`: it
replaces the stored token, writes the new one to the same file and exits. The
old token stops working.

A hub that never got a token starts and serves without one. `--admin-email`
is ignored once a token exists. The token and the file are written together:
when the file cannot be written (no config directory, no permission), nothing
is stored and the run exits `1`.

### What the hub checks at start

`parleyd` checks its surroundings before it listens, so that a hub that is up
is a hub that works. It stops at the first check that fails, writes exactly
one `cannot start` line whose `error` names what to fix, and exits `1`
without listening.

| Check | Why it can fail | What to do |
|---|---|---|
| the configuration | an unknown key, a value out of range, a file that is not TOML; the error names the file and the key | fix the key (see [configuration.md](configuration.md#the-configuration-file)) |
| the secret keys | a key that is not base64 of exactly 32 bytes; the error names the variable or file and never quotes the key | fix the key (see [configuration.md](configuration.md#secret-keys)) |
| the data | the data directory or workspace root cannot be created or opened, or a store cannot be brought to the current schema | check the paths and their permissions |
| the admin token file | it is still there, or it could not be written | save the token and delete the file |
| git | no `git` on the `PATH` of the hub's environment, a version below 2.38, or `git --version` gives no usable answer within 10 seconds | install git 2.38 or later |
| the sandbox gateway, when `[sandbox]` is configured | the gateway cannot be reached, refuses the hub's credential, runs an unsupported release, or does not know a profile, a credential or a policy file; the error names the configuration key | fix that key (see [configuration.md](configuration.md#sandboxes)) |
| the listen address | the port is taken; this line is `cannot listen` with `addr` | free the port or change `[server].port` |

Git is checked because the hub runs every repository operation through the
`git` command; only the major and minor version are compared. A missing
secret key is not a failure: the hub starts, warns that secrets cannot be
stored, and refuses secret writes until a key is configured.

`--help`, a usage error, `parleyd backup` and the runs that write the admin
token file end before the git and gateway checks, so they work on a host
without git.

### Backup and restore

`parleyd backup <directory>` writes a consistent snapshot of the hub's data
while the hub keeps running. You do not stop the hub for it, and a push or a
commit that happens during the backup cannot leave the snapshot half-written:
each store is copied as of one moment, and in every repository the refs are
copied before the objects, so every ref in the snapshot points at an object
that is there.

**To back up:**

1. Run the command with the hub's environment and working directory, so that
   it reads the same `config.toml` and finds the same data:

   ```sh
   parleyd backup /srv/backups/2026-11-13
   ```

2. Check the exit status. `0` and a `backup written` log line mean the
   snapshot is complete. On any failure the command exits `1`, logs
   `backup failed` with the cause, and removes what it wrote.
3. Keep the hub's `config.toml` and its secret keys with the snapshot, in a
   safe place. The snapshot holds neither, and stored secrets cannot be read
   without their key.

The directory must be empty or not exist (it is created with mode `0700`),
and must not lie inside the hub's data directory or workspace root. The
command only reads: it binds no port, changes no schema and writes nothing
under the data directory. The snapshot holds the database file under its
own name (`parley.db` by default), the analytical store `analytics.db`, and
the workspace root with every repository (`workspaces` by default).

**To restore**, on the new host:

1. Copy the database file to `[database].path`, `analytics.db` beside it,
   and the workspace directory to `[workspace].path`.
2. Put `config.toml` and the secret keys in place.
3. Start `parleyd` with no flags.

There is no restore command: restoring is copying. The restored hub keeps the
source hub's users, credentials and admin token. A snapshot taken under load
restores the same way; each branch is at the commit it had when it was
copied, or an ancestor of the source's later one.

### Logs

`parleyd` logs to stderr, one JSON object per line. Every line has `time`
(UTC), `level` (`TRACE`, `DEBUG`, `INFO`, `WARN` or `ERROR`) and `msg`; the
rest depends on the line. `[logging].level` sets the lowest level written
(`info` by default).

```json
{"time":"2026-11-13T12:11:38.622658Z","level":"INFO","msg":"listening","addr":"127.0.0.1:52887","config":"/srv/parley/config.toml","data_dir":"/srv/parley/data"}
{"time":"2026-11-13T09:12:03.104211Z","level":"INFO","msg":"request","method":"GET","path":"/version","status":200,"duration_ms":0.087,"request_id":"0b5c6f2e-8d1a-4c3b-9e7f-2a4d6c8e0f13"}
```

Secrets do not reach the log. Before a line is written, the hub replaces its
own credentials, the configured provider secrets and stored secret values
with `[redacted]`, strips user information from URLs and cuts long values. No
`Authorization` header and no request body is logged.

What to look for:

| `msg` | Level | Meaning |
|---|---|---|
| `listening` | info | the hub is up. `addr` is the address it bound (the default shows as `[::]:8080`), `config` the configuration file it looked for, `data_dir` its data directory |
| `secret encryption` | info | at start: whether a current and a previous key are configured, and `undecryptable_secrets`, the number of stored secrets whose key is not configured |
| `secrets cannot be stored: no encryption key configured` | warn | at start, without a current key |
| `request` | info | one line per answered request, with `method`, `path` (no query string), `status`, `duration_ms` and `request_id`. Health probes are left out unless `[logging].log_health_probes` is on |
| `internal error`, `panic` | error | a request failed in a way the hub did not expect; the client got a `500`. Find the line by the `request_id` the client received in `X-Request-ID` |
| `maintenance task failed` | error | one hourly task failed (`task`, `error`); the others ran, and it runs again in the next pass |
| `retention step failed` | error | one step of the retention task failed (`step`); see [configuration.md](configuration.md#retention) |
| `secret re-encryption` | info | after a key rotation: `converted` values moved to the current key, `remaining` still under another. At `remaining` `0` you can remove the previous key |
| `abandoned at the stop` | warn | the stop gave up waiting for `work` (see [Signals and stopping](#signals-and-stopping)) |
| `cannot start`, `cannot listen` | error | the start failed (see [What the hub checks at start](#what-the-hub-checks-at-start)) |

A line written while a request is handled carries that request's
`request_id`, so one id ties the access line, any error line and the
client's response together.

### Signals and stopping

`SIGTERM` and `SIGINT` stop the hub gracefully. It stops accepting
connections, ends long-lived responses such as event streams, cancels the
running maintenance task and its background work, and waits for what is in
flight: up to 15 seconds for requests, and never longer than 30 seconds
after the signal for anything. Then it closes its stores and exits `0`,
also when it gave up waiting on something (that is logged). With nothing in
flight it exits at once. A second signal during the stop changes nothing.

### Exit statuses

| Status | When |
|---|---|
| `0` | `--help`, a run that wrote the admin token file, a written backup, or a stop by signal |
| `1` | the start failed before listening, or `backup` failed |
| `2` | a usage error on the command line |

## Using `parley`

```text
parley <command> [subcommand] [arguments] [flags]
```

Almost every command is one call to the hub's REST API: it sends one request
and prints the hub's answer. So `parley` adds little of its own, and what a
command means (states, permissions, errors) is described once, in
[api.md](api.md#using-the-api). This section covers what the client itself
does, and holds for every command.

### Finding the hub and the credential

A command needs two settings: the hub's URL and a credential. Each comes from
the first source that gives a non-empty value:

| Setting | 1. Flag | 2. Environment | 3. Configuration file |
|---|---|---|---|
| hub URL | `--endpoint-url <url>` | `PARLEY_ENDPOINT_URL` | `endpoint_url` |
| credential | `--api-key <credential>` | `PARLEY_API_KEY` | `api_key` |

The order is from most to least specific. The file is what
[`parley login`](#parley-login-and-your-account) wrote, your everyday
identity. The environment overrides it for one shell or one process, which
is how an agent is given the token of its fork without touching your file.
A flag overrides both for one command, which is how you run a single
[`parley admin`](#parley-admin) command with the admin token.

The credential is an API key, a personal access token or the admin token,
sent as a bearer token. The file is `config.toml` in `$PARLEY_CONFIG_DIR`,
or in `~/.parley` (see
[configuration.md](configuration.md#configuring-the-client)). It is read
only when a flag or variable leaves a setting empty.

The hub URL must be an absolute `http` or `https` URL with a host, such as
`http://127.0.0.1:8080` or `https://hub.example/parley`. A missing setting,
a bad URL or an unreadable configuration file fails the command before any
request, with a `client_error` that names the flag, the variable and the
file to set.

`--endpoint-url` and `--api-key` are global flags, as are `--json` and
`-h`/`--help` (see [Help and JSON help](#help-and-json-help)). They may
stand anywhere on the command line.

### Output

A script can rely on this split:

- **stdout** carries the result: exactly one JSON document, indented, or
  nothing. It is the hub's answer as the hub sent it. An answer without a
  body prints `{}` or a small confirmation such as
  `{"status": "deleted", "slug": "demo"}`.
- **stderr** carries everything else: an error as one JSON document, and
  warnings and progress as plain text lines (warnings start with
  `warning: `).

A few commands print something other than one JSON document, because their
result is not one: `parley events tail` and `parley sandbox logs` print one
JSON object per line; `parley scm cat`, `diff --patch`, `parley change
conflict --file`, `parley sandbox exec` and `parley sandbox download … -`
print raw bytes or text.

When a result holds a secret the hub shows only once (a new token, key or
webhook secret), it is printed in the result and stderr gets
`warning: this value is shown once and will not be shown again`. Capture
stdout then; there is no way to read the value again.

List commands print one page, `{"items": […], "next_cursor": …}`. Those that
page take `--limit <n>` and `--cursor <cursor>`: pass `next_cursor` back for
the next page. The flag tables below do not repeat these two. The lists of
`parley secrets` and `parley vars` read every page by themselves.

### Errors and exit statuses

On a failure stdout stays empty and stderr gets one error document. When the
hub refused the request, it is the hub's own error, unchanged: `code` is the
HTTP status and `error_type` says what went wrong (see
[api.md](api.md#using-the-api)). When `parley` itself raises the error,
`code` is `0`:

```json
{
  "error": {
    "code": 0,
    "message": "parley user update needs the flag --full-name",
    "error_type": "usage_error"
  }
}
```

| Status | `error_type` | Meaning |
|---|---|---|
| `0` | | success; also help, and `parley version` whether or not the hub answered |
| `1` | the hub's | the hub answered with an error |
| `1` | `wait_failed`, `wait_timeout` | a `--wait` ended in a state that is not a success, or ran out of time |
| `1` | `delivery_failed` | `parley webhook ping` or `redeliver`: the receiver did not accept the delivery |
| `2` | `usage_error` | the command line is wrong: an unknown command or flag, a missing argument or required flag, a value the command cannot read. Nothing was sent |
| `2` | `client_error` | a failure on the client's side: no hub URL or credential, an unreadable file, a hub that cannot be reached or does not answer in time, an answer that is not the hub's (a proxy, a wrong URL) |
| `3` | `condition_not_met` | a `--fail-on-…` flag was given and its condition held |

So a script branches like this:

```sh
parley change land demo 12 --wait > land.json 2> err.json
case $? in
  0) echo "landed" ;;
  1) jq -r .error.error_type < err.json ;;   # the hub said no, or the wait failed: read why
  2) echo "fix the command line or the connection" ;;
esac
```

Status `1` is about the operation and is worth reading; status `2` means the
request never produced an answer from the hub. Status `3` exists so that a
check can be a one-liner: a `--fail-on-…` flag prints the result as usual
and then exits `3` when the result shows the condition, for example
`parley change overlaps demo 12 --fail-on-overlap` in a pre-land script.

`parley` sends every request exactly once. It does not retry after a `429`,
a `5xx`, a timeout or a broken connection, and does not follow redirects,
because only the caller knows whether repeating a write is safe. A request
has 30 seconds to complete; the commands that make the hub talk to an origin
(`workspace sync`, `push`, `archive`, `reclone`) have 3 minutes, and
`change land` 70 seconds. An answer larger than 8 MiB is refused.

Arguments that become part of a URL (a slug, a branch, a key) are encoded
for you: pass `feature/x` as it is.

### Waiting with `--wait`

Some operations take longer than a request: a clone, a rebuild, a land, a
sandbox coming up. Their commands return at once with a record whose state
is "in progress". With `--wait`, the command polls that record until it
reaches a final state, so a script gets the outcome from one command
instead of writing a polling loop.

| Flag | Default | Effect |
|---|---|---|
| `--wait` | off | wait until the operation ends and print its final record |
| `--timeout <duration>` | `5m` | with `--wait`, how long to wait |
| `--poll-interval <duration>` | `5s` | with `--wait`, the pause between two reads |

A duration is a whole number above zero and one unit, `s`, `m`, `h` or `d`:
`90s`, `5m`, `2h`, `1d`. Compound forms such as `1m30s` are not accepted.

While it waits, the command prints nothing on stdout and writes a progress
line such as `waiting: cloning` to stderr each time the state changes. It
ends in one of these ways:

| End | stdout | stderr | Exit |
|---|---|---|---|
| a final state that is a success | the final record | | `0` |
| a final state that is not a success | the final record | `wait_failed`, naming the state | `1` |
| the timeout passes | nothing | `wait_timeout`, naming the last state | `1` |
| a poll the hub answers with an error | nothing | the hub's error | `1` |
| a poll that cannot reach the hub | nothing | `client_error` | `2` |

Running out of time stops only the waiting. The operation goes on on the
hub, and you can read it later or wait again.

Commands with `--wait`: `workspace create`, `reactivate`, `reclone` and
`sync`; `change land`; `patch reset-to-origin`; `rebuild submit` and
`requeue`; `sandbox create` and `start`. `parley change wait` is a command
of its own that waits for a change to land or become landable.

### Help and JSON help

```sh
parley                          # the top-level commands
parley change                   # a group lists its subcommands
parley change land --help       # one command: arguments, flags, defaults
parley help change land         # the same
```

Help never needs a hub, a credential or a valid configuration file, and
always exits `0`.

With `--json`, help is a machine-readable description of the commands. It
exists so that an agent, or a tool built around `parley`, can discover what
it can do without parsing help text: every command comes with its
arguments, its flags with types and defaults, the API route it calls and
the kind of credential it needs.

```sh
parley help --json              # every command: {"commands": [ … ]}
parley help scm --json          # every command of one group
parley help scm log --json      # one command, as one object
```

| Member | Value |
|---|---|
| `name` | the command's words, such as `change land` |
| `description` | one sentence saying what it does |
| `method`, `path` | the route it calls, `path` relative to `/api/v1` with `:name` for its parameters; `null` for a command that calls no single route |
| `args` | the positional arguments in order, each with `name` and `required` |
| `flags` | every flag, sorted by name, each with `name`, `type`, `required`, `default` and `description`; the global flags are included |
| `auth` | `none` (needs no hub credential: `help`, `version`, `login`, `credential-helper`), `user` or `admin` |

The same declarations feed the tools of [`parley mcp`](#parley-mcp), so the
help, the JSON help and the MCP tools never disagree (see
[adr/02-declare-every-cli-command-through-one-helper.md](adr/02-declare-every-cli-command-through-one-helper.md)).

### The git credential helper

Git talks to the hub over HTTP and needs a credential for it (see
[api.md](api.md#git-over-http)). `parley credential-helper` gives git the
credential `parley` already has, so `git clone`, `fetch` and `push` against
the hub work without a prompt and without a token in the remote URL or in
git's own credential store.

Set it up once per machine, for the hub's URL only:

```sh
git config --global credential.https://hub.example.helper '!parley credential-helper'
```

From then on git asks the helper whenever it talks to that URL. The helper
finds the hub URL and the credential like every command (flags, environment,
configuration file), so `PARLEY_API_KEY=<fork token> git push` pushes with
the fork's token. It answers only when git asks about the hub's own scheme,
host and port, and gives the user name `x-token-auth` with the credential as
the password. For any other host, or when no credential is configured, it
stays silent and exits `0`, and git moves on to its next helper. It never
contacts the hub and never stores anything: git's `store` and `erase`
requests do nothing, because the credential lives in `parley`'s settings.

### `parley version`

```sh
parley version
```

Prints the client's build as `{"cli": {"version", "commit"}}`. When a hub
URL is configured it also asks the hub, without a credential, and adds the
hub's build as `server` (`version`, `commit`, `build_time`, `go_version`).
Use it to check that a client reaches the hub and that the two match. It
always exits `0`: when the hub cannot be read, it prints the client's build
alone and a `warning: ` line on stderr.

## `parley login` and your account

These commands manage who you are on the hub: the sign-in, your profile,
your organizations, and the two kinds of credential you can hold. An **API
key** is what `parley login` gets you and carries all your rights. A
**personal access token** (PAT) carries only the scopes you give it, can be
limited to paths or bound to one workspace, and is what you hand to an
agent or a CI job. They drive
[api.md](api.md#signing-in-and-your-account).

```sh
parley login --endpoint-url https://hub.example         # sign in through the browser
printf '%s' "$TOKEN" | parley login --api-key -          # store a credential you already hold
parley tokens create --name ci --permissions git:read,changes:write --expires 30
```

| Command | What it does |
|---|---|
| `parley login` | sign in and save the hub URL and the credential |
| `parley user show` | print your profile |
| `parley user update --full-name <name>` | change your full name; `""` clears it |
| `parley orgs list` | list the organizations you belong to |
| `parley orgs show <org>` | print an organization |
| `parley orgs members <org>` | list an organization's members |
| `parley keys list` | list your API keys |
| `parley keys refresh` | replace the secret of the key you are using and save the new key |
| `parley keys revoke` | revoke the key you are using and remove it from the configuration file |
| `parley tokens list [--workspace <slug>]` | list your PATs, or the tokens bound to a workspace |
| `parley tokens create` | create a PAT; its value is shown once |
| `parley tokens show <token_id>` | show one PAT |
| `parley tokens revoke <token_id>` | revoke one PAT |
| `parley tokens replace <token_id> --permissions <scopes>` | replace a PAT's scopes |
| `parley tokens add <token_id> --permissions <scopes>` | add scopes to a PAT |
| `parley tokens remove <token_id> --permissions <scopes>` | remove scopes from a PAT |

`<org>` is an organization's id or slug.

### `parley login`

`parley login` has two ways in, and both end the same: the hub URL, the
credential and your user id are saved in the client configuration file,
your user is printed on stdout and `Logged in as <username>` on stderr. The
credential itself is never printed. On any failure nothing is saved.

**Through the browser** (no `--api-key`). `parley` asks the hub for its
sign-in providers, opens the provider's page in your browser and waits up
to two minutes for you to finish there. The hub then issues a new API key.
When no browser can be opened, the URL is printed on stderr for you to open
by hand; the browser must be able to reach `127.0.0.1` on the machine
`parley` runs on.

**With a credential** (`--api-key <credential>`, or `--api-key -` to read it
from stdin, which keeps it out of the process list). `parley` checks the
credential against the hub and stores it. Nothing is created or revoked. Use
this on a machine without a browser, or to store a PAT. Only the flag
selects this way; `PARLEY_API_KEY` does not. The hub refuses the admin
token here: pass that one per command instead.

| Flag | Default | Effect |
|---|---|---|
| `--provider <name>` | `github` | the sign-in provider of the browser login; it must be one the hub offers |
| `--expires <days>` | `90` | the lifetime of the new API key: `0` (no expiry), `30`, `60` or `90` |
| `--api-key <credential>\|-` | | store this credential instead of signing in through the browser |

### `parley keys`

`refresh` and `revoke` take no argument: they act on the API key the command
itself presents. They need an API key as the credential, not a PAT.

`refresh` rotates the key: the old one stops working as soon as the hub
answers, the new one is printed (shown once) and saved in the configuration
file. When the key did not come from that file (it came from `--api-key` or
`PARLEY_API_KEY`), the file is left alone, the command says the new key was
not saved and exits `2`: take the key from stdout, or store it with
`parley login --api-key -`. If the answer is lost on the way, the new key is
lost with it; sign in again.

`revoke` revokes the key and clears it from the configuration file when the
file holds it. Run `parley login` to sign in again.

### `parley tokens`

| Flag | On | Effect |
|---|---|---|
| `--name <name>` | `create` | required: a name to recognise the token by |
| `--permissions <scopes>` | `create`, `replace`, `add`, `remove` | required: scopes separated by commas, such as `git:read,changes:write` |
| `--paths <globs>` | `create` | limit the token to files matching these globs, separated by commas; repeatable |
| `--expires <days>` | `create` | the lifetime in days; `90` when not given |
| `--workspace <slug>` | `create`, `list` | create a token bound to this workspace, or list the tokens bound to it |

A token left without scopes by `replace` or `remove` is revoked; the command
says so on stderr. Which scopes exist, what a path limit covers and what a
workspace-bound token may do are in
[api.md](api.md#task-forks-and-workspace-tokens).

## `parley admin`

The hub's administrators use these commands to manage users, organizations
and other people's credentials: create or block an account, make someone an
admin, revoke a leaked key. They need the admin token or the API key of a
user with the role `admin`, and drive [api.md](api.md#administration).

```sh
# with the admin token, for one command
parley admin users promote alice --api-key "$PARLEY_ADMIN_TOKEN"

parley admin orgs create --name "Acme" --slug acme
parley admin orgs members add acme alice
parley admin keys revoke alice AbCd1234       # a leaked key
```

| Command | What it does |
|---|---|
| `parley admin users list [--include-blocked]` | list the active users, and the blocked ones when asked |
| `parley admin users show <user>` | print a user |
| `parley admin users create` | create a user and the user's personal organization |
| `parley admin users update <user> --full-name <name>` | change a user's full name |
| `parley admin users delete <user>` | delete a user, keeping the user's organizations |
| `parley admin users promote <user>`, `demote <user>` | give a user the role `admin`, or `user` |
| `parley admin users block <user>`, `unblock <user>` | block a user, or lift the block |
| `parley admin orgs list [--include-blocked]` | list the active organizations, and the blocked ones when asked |
| `parley admin orgs create` | create an organization |
| `parley admin orgs update <org>` | change an organization's name or URL |
| `parley admin orgs delete <org>` | delete an organization and its memberships |
| `parley admin orgs block <org>`, `unblock <org>` | block an organization, or lift the block |
| `parley admin orgs members list <org>` | list an organization's members |
| `parley admin orgs members add <org> <user>` | make a user a member |
| `parley admin orgs members remove <org> <user>` | remove a member |
| `parley admin keys list <user>` | list a user's API keys |
| `parley admin keys revoke <user> <key_id>` | revoke one of a user's API keys |
| `parley admin tokens list <user>` | list a user's PATs |
| `parley admin tokens revoke <user> <token_id>` | revoke one of a user's PATs |

`<user>` is a user's id, e-mail or username; `<org>` an organization's id or
slug.

| Command | Flag | Effect |
|---|---|---|
| `users create` | `--username`, `--email`, `--provider`, `--provider-id` | required: the account's name and address, and the sign-in provider and the account's id at that provider |
| `users create` | `--full-name <name>` | the user's full name |
| `orgs create` | `--name`, `--slug` | required |
| `orgs create` | `--url <url>` | the organization's URL |
| `orgs create` | `--owner <user id>` | the owner; a user's id, not a username |
| `orgs update` | `--name <name>`, `--url <url>` | at least one; `--url ""` clears the URL |

## `parley workspace`

A workspace is the hub's copy of one git repository, registered once. Agents
do not work in it directly: they fork it (see [`parley fork`](#parley-fork))
and land changes back into it. These commands register a repository, keep
the hub's copy and the origin in step in both directions, and retire it.
They drive [api.md](api.md#workspaces).

```sh
# register a repository and wait for the clone
parley workspace create --slug demo --git-url https://github.com/acme/demo.git \
    --git-pat "$GITHUB_TOKEN" --wait

# take in what happened on origin; fail the script if a branch diverged
parley workspace sync demo --fail-on-diverged

# publish the hub's main to origin
parley workspace push demo main
```

| Command | What it does |
|---|---|
| `parley workspace create --slug <slug> --git-url <url>` | register a repository; the hub clones it in the background |
| `parley workspace list` | list the workspaces you can read |
| `parley workspace get <slug>` | print a workspace with its settings and your access to it |
| `parley workspace update <slug>` | change the display name, description, organization or settings |
| `parley workspace sync <slug>` | fetch origin and bring the hub's branches up to date |
| `parley workspace push <slug> <branch>` | push one branch to the same branch of origin |
| `parley workspace archive <slug>` | take a workspace out of service, keeping the hub's copy |
| `parley workspace reactivate <slug>` | bring an archived workspace back |
| `parley workspace reclone <slug> --confirm` | discard the hub's copy and clone origin again |
| `parley workspace delete <slug> --confirm` | delete an archived workspace with its forks and everything they own |
| `parley workspace patch-status <slug>` | summarise a carry-patch workspace (see [`parley patch` and `parley rebuild`](#parley-patch-and-parley-rebuild)) |

### `parley workspace create`

| Flag | Effect |
|---|---|
| `--slug <slug>` | required: the workspace's name on the hub |
| `--git-url <url>` | required: the origin, `https://<host>/<path>` or `git@<host>:<path>` |
| `--default-branch <name>` | the branch the workspace treats as its default; origin's when not given |
| `--org <id or slug>` | the organization the workspace belongs to; your personal one when not given |
| `--display-name <text>`, `--description <text>` | how the workspace is shown |
| `--git-pat <token>` | a token origin accepts; stored as the workspace secret `GIT_PAT` |
| `--git-username <name>`, `--git-password <password>` | a user name and password origin accepts, given together and instead of `--git-pat`; stored as `GIT_USERNAME` and `GIT_PASSWORD` |
| `--workspace-mode <mode>` | `standard` (the default) or `carry_patch` |
| `--upstream-url <url>`, `--integration-branch <name>` | for `carry_patch`: the upstream project the workspace tracks, and the branch the hub rebuilds as upstream plus the patches |
| `--setting <key=value>` | one workspace setting; repeatable |
| `--wait`, `--timeout`, `--poll-interval` | wait until the clone ends: `ready` is the success, `failed` prints the workspace with its `clone_error` |

Without `--wait` the command prints the workspace in status `pending` and
returns; the clone goes on on the hub.

**Settings.** `--setting`, here and on `update`, takes `key=value`.
`true` and `false` are sent as booleans, `protected_branches` as a list
split at commas, anything else as text, and an empty value (`key=`) puts
the setting back to its default:

```sh
parley workspace update demo --setting land.strategy=squash \
    --setting 'protected_branches=main,release/*'
```

The settings and their values are listed in [api.md](api.md#workspaces).

### `parley workspace list` and `update`

| Command | Flag | Effect |
|---|---|---|
| `list` | `--include-archived` | include archived workspaces |
| `list` | `--include-forks` | include task forks |
| `list` | `--parent <slug>` | only the forks of this workspace |
| `update` | `--display-name <text>`, `--description <text>` | new values |
| `update` | `--org <id or slug>` | move the workspace and its forks to this organization |
| `update` | `--clear-display-name`, `--clear-description`, `--clear-org` | back to the default: the slug, an empty description, the owner's personal organization |
| `update` | `--setting <key=value>` | as for `create`; repeatable |

`update` needs at least one flag.

### `parley workspace sync` and `push`

The hub's copy and origin move independently: people push to origin, changes
land on the hub. `sync` brings origin's commits in, `push` sends the hub's
out. Neither loses work silently: a branch that moved on both sides is
reported as `diverged`, and a push is fast-forward only unless you say
otherwise.

| Command | Flag | Effect |
|---|---|---|
| `sync` | `--reset-to-origin` | move the branches that diverged to origin's head, giving up the hub's side |
| `sync` | `--fail-on-diverged` | after printing the answer, exit `3` when a branch (or, in a carry-patch workspace, a patch) diverged |
| `sync` | `--wait`, `--timeout`, `--poll-interval` | when the sync queued a rebuild of a carry-patch workspace, wait for it and print the answer with the final job as `rebuild` |
| `push` | `--force` | replace origin's branch with the hub's, but only while origin is where the hub last saw it; a warning is written first |

`push` prints `result`: `pushed`, `forced` or `up_to_date`.

### Archiving, recloning and deleting

`archive` takes a workspace out of service and keeps the hub's copy;
`reactivate` brings it back. `reclone` repairs a workspace by cloning origin
afresh. `delete` removes an archived workspace for good, with its forks.
Before an archive and a reclone the hub first offers every branch to origin,
so that commits that exist only on the hub are not lost.

`reclone` and `delete` cannot be undone and do nothing without `--confirm`.
`reactivate` and `reclone` take `--wait`, `--timeout` and `--poll-interval`
and wait for the clone as `create` does.

## `parley fork`

A task fork is a copy of a workspace for one piece of work: one agent, one
task, one fork. It costs almost nothing to create, has its own branches,
expires on its own, and can come with a token that works nowhere else, so
an agent can do no harm outside it. A push to a fork opens a change against
the parent. These commands drive
[api.md](api.md#task-forks-and-workspace-tokens).

```sh
# a fork with a token that can push to it and open changes
parley fork create demo --name greeting \
    --token-permissions git:write,changes:write --purpose "Add a greeting endpoint"

# a fork with a sandbox running an agent in it
parley fork create demo --name greeting --sandbox-profile agent -- my-agent --task "…"

parley fork extend demo--greeting --ttl 2d
parley fork delete demo--greeting
```

| Command | What it does |
|---|---|
| `parley fork create <parent> [-- command...]` | create a fork of the workspace `<parent>` |
| `parley fork list <parent>` | list the forks of a workspace |
| `parley fork extend <slug> --ttl <duration>` | give a fork a new lifetime, counted from now; it may be shorter |
| `parley fork delete <slug>` | delete a fork at once, with its branches, its secrets and the tokens bound to it |

A fork's slug is `<parent>--<name>`. `extend` and `delete` accept only such
a slug, so they cannot hit a repository workspace by accident; that is also
why `delete` needs no `--confirm`.

`create` prints `{"workspace": …, "remote": …}`: the fork and the git URL to
clone. With a token it adds `token`, whose `token` member is the value,
shown once; with a sandbox it adds `sandbox`.

| Command | Flag | Effect |
|---|---|---|
| `create` | `--name <name>` | the fork's name; generated when not given |
| `create` | `--ref <branch>` | the branch to fork from; the parent's default branch when not given |
| `create` | `--ttl <duration>` | the fork's lifetime, such as `2h` or `7d`; the hub's default when not given |
| `create` | `--purpose <text>` | what the fork is for, shown in lists |
| `create` | `--session <id>` | the agent session the fork belongs to (see [`parley session`](#parley-session)) |
| `create` | `--no-auto-change` | do not open a change when a branch is pushed to the fork |
| `create` | `--token-permissions <scopes>` | also create a token bound to the fork, with these scopes, separated by commas |
| `create` | `--token-paths <globs>` | limit that token to these paths; only with `--token-permissions` |
| `create` | `--sandbox-profile <profile>` | also create a sandbox on the fork from this profile |
| `create` | `-- command...` | the command the sandbox runs; only with `--sandbox-profile` |
| `list` | `--mine` | only the forks you own |
| `list` | `--session <id>` | only the forks of this session |

## `parley scm`

These commands read and write a workspace's repository on the hub, without a
clone. An agent that only needs to look at three files, or to commit a
two-line fix, does not have to fetch a repository for it; and the hub can
answer questions a clone cannot answer cheaply, such as a ranked code search
or "does this branch still rebase?". They drive
[api.md](api.md#source-control). Every command takes the workspace's slug
first; a fork's slug works too.

```sh
# orient yourself
parley scm code-search demo 'greeting handler' --limit 10
parley scm outline demo api/server.go
parley scm cat demo api/server.go --ref main

# commit a file to a branch and open a change for it, in one call
parley scm commit demo--greeting --branch greeting --base main \
    --message "Add greeting endpoint" --put api/greeting.go=./greeting.go \
    --title "Add greeting endpoint" --intent "Issue #41 asks for a greeting route"

# would this branch still rebase onto main?
parley scm compare demo main greeting --fail-on-conflict
```

| Command | What it does |
|---|---|
| `parley scm refs <slug>` | list branches, tags and the hub's own refs |
| `parley scm log <slug>` | list the history of a revision, newest first |
| `parley scm show <slug> <rev>` | show one commit |
| `parley scm ls <slug> [path]` | list a directory of a revision |
| `parley scm cat <slug> <path>` | write one file's raw bytes to stdout or a file |
| `parley scm diff <slug> <base> <head>` | show the file changes between two revisions |
| `parley scm compare <slug> <base> <head>` | tell whether `head` still rebases onto `base`, with the conflict as data |
| `parley scm blame <slug> <path>` | show which commit last changed each line |
| `parley scm outline <slug> <path>` | show a source file's declarations: kinds, names, signatures, lines |
| `parley scm search <slug> <query>` | search the file contents of any revision, exactly |
| `parley scm code-search <slug> <query>` | search the default branch's code, ranked, from the code index |
| `parley scm symbols <slug> <name>` | find the declarations on the default branch whose name starts with `<name>` |
| `parley scm code-index <slug>` | show which commit the code index holds and its state |
| `parley scm commit <slug>` | commit files to a branch, creating or moving it |
| `parley scm rebase <slug> <branch> --onto <rev>` | rebase a branch onto another revision, on the hub |

`search` reads the revision you name and finds every literal or regex match.
`code-search` and `symbols` answer from an index of the default branch: they
are ranked and fast, and good for finding where to look. `code-index` shows
which commit the index holds.

### Reading a repository

| Command | Flag | Effect |
|---|---|---|
| `refs` | `--prefix <p>` | only refs whose name starts with this |
| `log` | `--ref <rev>` | the revision to start from; the default branch when not given |
| `log` | `--path <path>`, `--follow` | only commits that touch this path; follow it across renames |
| `log` | `--since <time>`, `--until <time>`, `--author <text>` | narrow the history |
| `log` | `--stats` | include each commit's change statistics |
| `show` | `--files` | include the files the commit changed |
| `ls` | `--ref <rev>`, `--recursive` | the revision; list the whole tree below the path |
| `cat` | `--ref <rev>` | the revision |
| `cat` | `--output <file>` | write to this file instead of stdout; nothing is written when the hub refuses |
| `diff` | `--mode merge_base\|direct` | compare `head` with its merge base with `base`, or the two revisions directly |
| `diff` | `--path <glob>` | only these files; repeatable |
| `diff` | `--patch`, `--name-only` | print a unified diff as text, or JSON without the patches; not both |
| `compare` | `--fail-on-conflict` | exit `3` when `head` does not rebase onto `base` cleanly |
| `blame` | `--ref <rev>`, `--lines <start>:<end>` | the revision; only this line range |
| `outline` | `--ref <rev>` | the revision |
| `search` | `--ref <rev>`, `--path <glob>` (repeatable) | where to search |
| `search` | `--regex`, `--ignore-case` | read the query as a regular expression; ignore case |
| `code-search` | `--limit <n>` | the most results |
| `symbols` | `--exact` | only names equal to `<name>` |
| `symbols` | `--kind <kind>`, `--path <path>`, `--limit <n>` | narrow by kind of declaration or by path |

### Writing to a repository

`commit` builds one commit from files on your machine and puts it on a
branch. `rebase` moves a branch onto another revision. Both happen on the
hub and both are safe against a concurrent writer: the write goes through
only if the branch is still where it was.

| Command | Flag | Effect |
|---|---|---|
| `commit` | `--branch <b>` | required: the branch to commit to; created when it does not exist |
| `commit` | `--message <m>` | required: the commit message |
| `commit` | `--base <rev>` | the revision a new branch starts from; required when the branch does not exist yet |
| `commit` | `--put <repo-path>=<local-file>` | write the local file's content at this path; repeatable |
| `commit` | `--delete <repo-path>` | delete this path; repeatable |
| `commit` | `--move <from>=<to>` | rename a path; repeatable |
| `commit` | `--expected-head <sha>` | commit only if the branch is at this commit |
| `commit` | `--session <id>` | the agent session the commit belongs to |
| `commit` | `--change-target <b>`, `--title <t>`, `--intent <i>`, `--draft` | also open a change of the branch, into this target branch, with this title and intent, as a draft when asked |
| `rebase` | `--onto <rev>` | required: the revision to rebase onto |
| `rebase` | `--expected-head <sha>` | rebase only if the branch is at this commit |

`--put`, `--delete` and `--move` are applied in the order you give them.
Without `--expected-head`, `commit` reads the branch's head first and
commits only onto that head. If the branch moved in between, the hub answers
`ref_moved` with the current head: look at what arrived, then run the
command again. A rebase that cannot be done answers `rebase_conflict` with a
conflict report. Neither write is ever retried by the client.

## `parley change`

A change is a proposal to land the commits of a branch on a target branch:
what a pull request is on a forge, built for agents. It carries an intent
(why), versions (each push is one), and a state the hub keeps up to date:
whether it still applies cleanly and whether policy lets it land. Changes
can be stacked on one another. These commands follow a change through its
life and drive [api.md](api.md#changes), [api.md](api.md#landing) and
[api.md](api.md#landing-policy-approvals-and-checks).

Every command takes the workspace's slug first, and all but `open`, `list`
and `find` the change's number.

```sh
# open a change for a pushed branch (a push to a fork does this by itself)
parley change open demo --source greeting --source-workspace demo--greeting \
    --title "Add greeting endpoint" --intent @intent.md

# review it
parley change show demo 12
parley change diff demo 12 --patch
parley change approve demo 12 --comment "reads well"

# land it and wait for the outcome
parley change land demo 12 --wait
```

| Command | What it does |
|---|---|
| `parley change open <slug> --source <branch>` | open a change for a branch |
| `parley change list <slug>` | list changes; the draft and open ones unless `--state` says otherwise |
| `parley change show <slug> <number>` | show one change |
| `parley change find <slug> <sha>` | show the change a commit belongs to |
| `parley change versions <slug> <number>` | list a change's versions, oldest first |
| `parley change diff <slug> <number>` | show the diff of a version, or what changed between two versions |
| `parley change stack <slug> <number>` | show the stack the change is in, bottom first |
| `parley change overlaps <slug> <number>` | list other changes that touch the same files (see [`parley activity` and `parley claim`](#parley-activity-and-parley-claim)) |
| `parley change edit <slug> <number>` | edit the title, intent, labels, auto-land flag or target branch |
| `parley change ready <slug> <number>` | mark a draft ready for review and landing |
| `parley change draft <slug> <number>` | move an open change back to draft |
| `parley change abandon <slug> <number>` | abandon a change |
| `parley change restore <slug> <number>` | bring an abandoned change back to open |
| `parley change restack <slug> <number>` | rebase the change onto what it sits on, now |
| `parley change conflict <slug> <number>` | show what would conflict in that rebase |
| `parley change resolve <slug> <number>` | restack, supplying the resolved files |
| `parley change approve <slug> <number>` | approve a version |
| `parley change reject <slug> <number> --comment <text>` | reject a version, saying why |
| `parley change approvals <slug> <number>` | list the verdicts on a version, or withdraw one |
| `parley change check <slug> <number> <name> --status <s>` | report the result of a named check |
| `parley change checks <slug> <number>` | list the check results of a version |
| `parley change land <slug> <number>` | land the change, or its whole stack |
| `parley change lands <slug> <number>` | list the change's land attempts, newest first |
| `parley change revert <slug> <number>` | open a change that undoes a landed change |
| `parley change wait <slug> <number>` | wait until the change is landed, ready or mergeable |

### Opening and editing

| Command | Flag | Effect |
|---|---|---|
| `open` | `--source <branch>` | required: the branch whose commits the change proposes |
| `open` | `--source-workspace <slug>` | the workspace that holds the branch, when it is a fork of `<slug>` |
| `open` | `--target <branch>` | the branch to land on; the workspace's default branch when not given |
| `open`, `edit` | `--title <t>` | the change's title |
| `open`, `edit` | `--intent <i>\|@<file>\|-` | why the change exists: the text, the content of a file, or stdin |
| `open` | `--draft` | open as a draft, which cannot land yet |
| `open` | `--depends-on <n>` | stack the change on change `<n>` |
| `open`, `edit` | `--auto-land`, and on `edit` `--no-auto-land` | have the hub land the change by itself once it can; switch that off |
| `open`, `edit` | `--label <l>` | add a label; repeatable |
| `edit` | `--remove-label <l>` | remove a label; repeatable |
| `edit` | `--target <branch>` | move the change to another target branch |
| `open` | `--session <id>` | the agent session the change belongs to |
| `abandon` | `--reason <text>` | why |
| `abandon` | `--dependants reparent\|abandon` | what becomes of the changes stacked on it |

`edit` changes only what you name and needs at least one flag.

### Listing and reading changes

| Command | Flag | Effect |
|---|---|---|
| `list` | `--state <s>` | only changes in this state, `draft`, `open`, `landed` or `abandoned`; repeatable |
| `list` | `--target <branch>`, `--author <id>\|me`, `--session <id>`, `--label <l>` | narrow the list |
| `list` | `--path <glob>` | only changes that touch matching files |
| `list` | `--mergeable <m>`, `--landing-status <s>` | only changes whose `mergeable` is `clean`, `conflict` or `unknown`, or whose landing status is `ready`, `pending` or `blocked` |
| `list` | `--stack-of <n>` | only the changes in the stack of change `<n>` |
| `show` | `--include <list>` | extra members to add to the change, separated by commas, such as `versions` |
| `diff` | `--version <n>\|latest` | the version to show; the latest when not given |
| `diff` | `--against <n>` | show what changed since version `<n>` instead |
| `diff` | `--path <glob>` | only these files; repeatable |
| `diff` | `--patch`, `--name-only` | a unified diff as text, or JSON without the patches; not both |
| `overlaps` | `--fail-on-overlap` | exit `3` when another change overlaps |

### Review: approvals and checks

A workspace's policy can require approvals and passing checks before a
change lands (see [`parley land` and `parley policy`](#parley-land-and-parley-policy)).
A reviewer, human or agent, records a verdict; a CI job or a test agent
reports a check. Both attach to a version of the change, the latest unless
`--version` names another.

```sh
parley change check demo 12 test --status passed --url https://ci.example/runs/881
```

| Command | Flag | Effect |
|---|---|---|
| `approve`, `reject` | `--comment <text>` | a comment; required for `reject` |
| `approvals` | `--withdraw <id>` | withdraw this verdict instead of listing |
| `check` | `--status pending\|passed\|failed` | required: the check's result |
| `check` | `--summary <text>`, `--url <url>` | a short result and a link to the details |
| all five | `--version <n>` | the version it applies to |

### Conflicts and restacking

When the target branch, or the change below in a stack, moves, a change has
to be rebased onto it. The hub does this by itself when it can. When the
rebase conflicts, the change shows `mergeable` `conflict`, and you resolve
it without a clone:

```sh
parley change conflict demo 12                         # which files, and why
parley change conflict demo 12 --file api/server.go > server.go
# ... edit server.go, removing the conflict markers ...
parley change resolve demo 12 --file api/server.go=./server.go
```

| Command | Flag | Effect |
|---|---|---|
| `conflict` | `--fail-on-conflict` | exit `3` when the change does not rebase cleanly |
| `conflict` | `--file <path>` | print that conflicted file's text, with conflict markers, instead of the report |
| `resolve` | `--file <repo-path>=<local-file>` | use the local file's content as the resolution of this path; repeatable |
| `resolve` | `--delete <repo-path>` | resolve this path by deleting it; repeatable |

`restack` and `resolve` print `{"status": "restacked" | "up_to_date",
"change": …}`. When paths are still conflicted, the hub answers
`rebase_conflict` with the conflict report, and nothing is changed.

### Landing

`land` asks the hub to put the change on its target branch. The hub rebases
it if the target moved, checks the policy and writes the branch. The command
prints the land attempt. A land usually finishes within the call; when it is
still `running`, the command exits `0` without `--wait`, because the hub has
taken the land and will finish it, and with `--wait` it follows the attempt
to its end: `landed` is the success, and `conflict`, `blocked` and `failed`
end with `wait_failed` and the attempt on stdout. A land the hub refuses
outright (a conflict, a policy that blocks it, a change that is not open) is
the hub's error with exit status `1`; what each means is in
[api.md](api.md#landing).

| Command | Flag | Effect |
|---|---|---|
| `land` | `--stack` | land the change and every change below it in its stack, in order |
| `land` | `--expected-version <n>` | land only if the change's latest version is `<n>`: what you reviewed is what lands |
| `land` | `--override <reason>` | land past the landing policy, recording the reason, where the workspace allows it |
| `land` | `--session <id>` | the agent session the land belongs to |
| `land` | `--wait`, `--timeout`, `--poll-interval` | wait until the attempt is final |
| `revert` | `--title <t>`, `--intent <i>` | the title and intent of the reverting change |
| `revert` | `--draft`, `--auto-land`, `--session <id>` | as for `open` |
| `wait` | `--for landed\|ready\|mergeable` | what to wait for; `landed` when not given |
| `wait` | `--timeout <duration>`, `--poll-interval <duration>` | as for `--wait`; `5m` and `5s` |

`revert` opens a new change on the branch `revert/<number>` that undoes the
landed one; it lands like any other change.

`wait` changes nothing on the hub. It reads the change until the condition
holds or can no longer hold:

| `--for` | Succeeds when | Fails with `wait_failed` when |
|---|---|---|
| `landed` | the change is landed | it is abandoned |
| `ready` | it is landed, or policy is satisfied and it rebases cleanly | it is abandoned, blocked by policy (a draft always is) or in conflict |
| `mergeable` | it is landed, or it rebases cleanly | it is abandoned or in conflict |

Use it to block a script on someone else's action: an agent whose change
has `--auto-land` runs `parley change wait demo 12` and continues once the
reviewers and the checks have let it through.

## `parley land` and `parley policy`

`parley land` reads the record of what was landed in a workspace: every
land attempt, whichever change it belonged to. `parley policy` manages the
rules a change must meet before it lands: how many approvals and which
checks, by default and for particular paths. Landing a change is
[`parley change land`](#landing). These commands drive
[api.md](api.md#landing) and
[api.md](api.md#landing-policy-approvals-and-checks).

```sh
# what landed on main since yesterday?
parley land list demo --target main --since 2026-11-20T00:00:00Z

# try a policy against a change before storing it
parley policy evaluate demo --change 12 --policy-file policy.toml
parley policy set demo --file policy.toml
```

| Command | What it does |
|---|---|
| `parley land list <slug>` | list the land attempts of a workspace, newest first |
| `parley land show <slug> <land-id>` | show one land attempt |
| `parley policy get <slug>` | show the landing policy, or an earlier revision |
| `parley policy set <slug> --file <path>\|-` | replace the landing policy |
| `parley policy evaluate <slug>` | show what a set of paths, or a change, would need to land; writes nothing |

| Command | Flag | Effect |
|---|---|---|
| `land list` | `--status <s>` | only attempts with this status; repeatable |
| `land list` | `--target <branch>`, `--since <time>` | narrow by target branch or time (RFC 3339) |
| `policy get` | `--revision <n>` | one of the revisions the hub keeps |
| `policy set` | `--file <path>\|-` | required: the policy document, from a file or stdin |
| `policy set` | `--if-revision <n>` | replace only if the stored policy is still revision `<n>` |
| `policy evaluate` | `--paths <list>` | the paths to evaluate, separated by commas; repeatable |
| `policy evaluate` | `--change <n>` | evaluate the files of this change instead; exactly one of the two |
| `policy evaluate` | `--target <branch>` | the target branch to evaluate for |
| `policy evaluate` | `--policy-file <path>\|-` | evaluate against this document instead of the stored policy |

A policy file is JSON or TOML; a name ending in `.toml` is read as TOML.

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

Two safeguards protect a policy from accidents. An empty file or an empty
stdin is refused, because the empty policy lets every change land and a
broken pipe must not install it. And `--if-revision` makes an edit safe
against a concurrent one: read the policy, note its `revision`, and `set`
with that number; if someone changed it in between, the hub answers
`revision_mismatch` and nothing is replaced.

## `parley activity` and `parley claim`

Many agents work on one repository at the same time, and none of them sees
the others. These commands are how they look before they start: `activity`
shows what else touches a set of paths, and a claim announces "I am working
here" to everyone who asks after you. A claim is advisory. It stops nobody;
it lets others choose different work or coordinate. They drive
[api.md](api.md#awareness).

```sh
# before starting: is anyone working under api/?
parley activity demo --paths 'api/**' --fail-on-overlap

# announce the work, for two hours
parley claim create demo --paths 'api/**' --intent "Add greeting endpoint" --ttl 2h

# before landing: does another open change touch my files?
parley change overlaps demo 12 --fail-on-overlap
```

| Command | What it does |
|---|---|
| `parley activity <slug> --paths <globs>` | show the draft and open changes, the active claims and the recently landed changes that touch these paths |
| `parley claim create <slug> --paths <globs> --intent <text>` | claim paths for the work you intend, and show what else touches them |
| `parley claim list <slug>` | list a workspace's claims, the active ones unless `--state` says otherwise |
| `parley claim renew <slug> <id>` | extend an active claim's lifetime, counted from now |
| `parley claim release <slug> <id>` | end a claim before it expires |
| `parley change overlaps <slug> <number>` | list the other draft and open changes into the same target branch that touch a file of this change |

| Command | Flag | Effect |
|---|---|---|
| `activity`, `claim create` | `--paths <globs>` | required: path globs, separated by commas |
| `activity`, `claim create` | `--target <branch>` | the target branch to look at; the default branch when not given |
| `activity` | `--since <sha>\|<time>` | how far back landed changes count; 24 hours when not given |
| `activity` | `--exclude-session <id>`, `--exclude-change <n>` | leave out your own session or change |
| `activity` | `--fail-on-overlap` | exit `3` when an unlanded change or an active claim touches the paths |
| `claim create` | `--intent <text>` | required: what you intend to do there |
| `claim create`, `claim renew` | `--ttl <duration>` | the claim's lifetime; the hub's default on `create`, `1h` on `renew` |
| `claim create` | `--session <id>` | the agent session the claim belongs to |
| `claim list` | `--state <s>`, `--session <id>`, `--path <glob>` | narrow the list |

A claim expires by itself, so an agent that dies does not block a path for
ever. Renew it while the work goes on and release it when the work is done.

## `parley patch` and `parley rebuild`

A carry-patch workspace maintains a set of your own patches on top of
somebody else's project: a fork of an upstream that you keep rebasing. Each
patch is a branch. The hub keeps an *integration branch*, which is upstream
plus every patch in order, and rebuilds it whenever upstream or a patch
moves. `parley patch` manages the patches and their order, `parley rebuild`
the rebuild jobs, and `parley rerere` the conflict resolutions the hub
remembers so that the same conflict is resolved only once. They drive
[api.md](api.md#carry-patch-workspaces).

You create such a workspace with `parley workspace create --workspace-mode
carry_patch --upstream-url … --integration-branch …`.

```sh
# register a branch as a patch
parley patch add fork-of-x --branch patch/tls-fix \
    --upstream-pr https://github.com/x/x/pull/881

# would the next rebuild conflict? exit 3 if so
parley rebuild preview fork-of-x --fail-on-conflict

# rebuild now and wait for the result
parley rebuild submit fork-of-x --wait

# where does everything stand?
parley workspace patch-status fork-of-x
```

### `parley patch`

| Command | What it does |
|---|---|
| `parley patch add <slug> --branch <name>` | register a branch as a patch, last in the order unless `--position` says where |
| `parley patch list <slug>` | list the patches in their order |
| `parley patch show <slug> <id>` | show one patch |
| `parley patch update <slug> <id>` | change a patch's status, position, description or upstream pull request |
| `parley patch remove <slug> <id>` | take a patch out of the order; its branch stays |
| `parley patch restore <slug> <id>` | bring a removed patch back, last in the order |
| `parley patch reorder <slug> <id>...` | put every patch in the order of the ids given |
| `parley patch reset-to-origin <slug> <id>` | move a patch's branch to origin's tip; the hub keeps the discarded tip |

| Command | Flag | Effect |
|---|---|---|
| `add` | `--branch <name>` | required: the patch's branch |
| `add`, `update` | `--position <n>` | the patch's place in the order |
| `add`, `update` | `--upstream-pr <url>` | the upstream pull request that would make the patch unnecessary |
| `add`, `update` | `--description <text>` | what the patch is for; `''` clears it |
| `add` | `--skip-branch-check` | register the patch without checking that its branch exists |
| `add` | `--if-not-exists` | succeed with the existing patch when the branch is already registered |
| `update` | `--status <s>` | the patch's new status: `active`, `disabled`, `conflict` or `merged_upstream` |
| `list` | `--include-deleted` | include removed patches |
| `reset-to-origin` | `--wait`, `--timeout`, `--poll-interval` | wait for the rebuild the reset queued |

`update` needs at least one flag.

### `parley rebuild`

| Command | What it does |
|---|---|
| `parley rebuild submit <slug>` | queue a rebuild of the integration branch |
| `parley rebuild list <slug>` | list the rebuild jobs, newest first |
| `parley rebuild status <slug> <id>` | show one job |
| `parley rebuild preview <slug>` | predict which patches a rebuild would apply, skip or stop at, changing nothing |
| `parley rebuild cancel <slug> <id>` | cancel a queued job |
| `parley rebuild requeue <slug> <id>` | queue a job again after it failed for good |
| `parley rebuild rollback <slug> <id>` | move the integration branch back to the head it had before a completed rebuild |

| Command | Flag | Effect |
|---|---|---|
| `submit` | `--strategy rebase\|merge` | how the patches are put on upstream; the workspace's setting when not given |
| `submit` | `--fail-mode fail_fast\|continue` | stop at the first patch that conflicts, or go on with the rest; the workspace's setting when not given |
| `submit`, `requeue` | `--wait`, `--timeout`, `--poll-interval` | wait until the job ends |
| `list` | `--status <status>` | `queued`, `running`, `failed`, `completed`, `dead_letter` or `cancelled` |
| `preview` | `--fail-on-conflict` | exit `3` when a patch would conflict with upstream |

A waited job succeeds as `completed`. `dead_letter` (it failed and will not
be retried) and `cancelled` end the wait with `wait_failed`. A job in
`failed` is waiting to be retried, so the wait goes on.

### `parley rerere`

| Command | What it does |
|---|---|
| `parley rerere list <slug>` | list the conflict resolutions the workspace remembers, newest first |
| `parley rerere forget <slug> <id-or-path>` | forget one resolution by id, or every one recorded at a path |

Forget a resolution when it was wrong: the next rebuild then stops at that
conflict again and you resolve it afresh.

## `parley secrets` and `parley vars`

Agents and sandboxes need configuration: tokens for a forge or a model
provider, and plain settings. A **secret** is a value the hub encrypts and
never shows again; no command prints one. A **variable** is a plain value
you can read back. Both live in one of three scopes, your user, an
organization or a workspace, and a workspace runs with the combination of
all three. They drive [api.md](api.md#secrets-and-variables).

```sh
# a secret for one workspace, kept out of the argument list
printf '%s' "$GITHUB_TOKEN" | parley secrets create GIT_PAT --from-stdin --workspace demo

# the same variables for an organization and one workspace
parley vars create LOG_LEVEL=info,REGION=eu --org acme --workspace demo

# what does the workspace actually run with, and where does each value come from?
parley vars resolve demo
```

| Command | What it does |
|---|---|
| `parley secrets create <KEY=VALUE>…` | create secrets |
| `parley secrets list` | list a scope's secret keys, never the values |
| `parley secrets update <KEY=VALUE>` | replace one secret's value |
| `parley secrets delete <KEY>` | delete one secret |
| `parley vars create <KEY=VALUE>…` | create variables |
| `parley vars list` | list a scope's variables with their values |
| `parley vars update <KEY=VALUE>` | replace one variable's value |
| `parley vars delete <KEY>` | delete one variable |
| `parley vars resolve <WORKSPACE>` | list the variables a workspace runs with, each with the scope it comes from |

| Flag | On | Effect |
|---|---|---|
| `--user` | all but `resolve` | your own scope; the default when no scope flag is given |
| `--org <slug>` | all but `resolve` | the scope of this organization |
| `--workspace <slug>` | all but `resolve` | the scope of this workspace |
| `--from-stdin` | `create`, `update` | take one bare `KEY` as the argument and read its value from stdin |

**Entries.** `create` takes one or more `KEY=VALUE` entries, as separate
arguments or separated by commas. A value may contain `=` but not a comma.
For a value with a comma, with line breaks, or one that should not appear in
the process list, use `--from-stdin`; one trailing line ending is dropped,
everything else is kept.

**Scopes.** `list`, `update` and `delete` act on one scope. `create` takes
several scope flags at once and creates the entries in each. One scope
failing does not stop the others: stdout holds
`{"results": [{"scope", "items"}, …]}` for the scopes that succeeded, stderr
one error per scope that failed, and the exit status is non-zero when any
failed.

`list` and `resolve` read every page and print one `{"items": […]}`.

## `parley forge`

A workspace's origin usually lives on a forge such as GitHub, with issues
and pull requests around the code. These commands let an agent read and
write them through the hub, with the credential the workspace already holds
for its origin, so the agent needs no forge token of its own. The groups
are `parley forge` (the connection itself), `parley issue`, `parley pr` and
`parley label`. They drive
[api.md](api.md#forge).

```sh
# read the task
parley issue get demo 41

# publish a landed branch and open a pull request for it
parley workspace push demo greeting
parley pr create demo --title "Add greeting endpoint" --head greeting --base main \
    --body-file pr.md

# report back
parley issue comment demo 41 --body "Opened as a pull request."
```

| Command | What it does |
|---|---|
| `parley forge status <slug>` | show the forge, API URL and kind of credential the hub derives for the workspace, without asking the forge |
| `parley forge repository <slug>` | show the forge's repository: name, default branch, visibility, the credential's permissions |
| `parley forge webhook set <slug>` | set or rotate the secret the forge signs its webhook deliveries with |
| `parley forge webhook unset <slug>` | remove that secret; the hub then refuses the forge's deliveries |
| `parley forge deliveries <slug>` | list the forge webhook deliveries the hub recorded, newest first |
| `parley issue list <slug>` | list issues, newest first, without pull requests |
| `parley issue get <slug> <number>` | show an issue with its first comments |
| `parley issue comments <slug> <number>` | list an issue's comments, oldest first |
| `parley issue create <slug> --title <t>` | open an issue |
| `parley issue update <slug> <number>` | change the title, body, state, labels or assignees |
| `parley issue close <slug> <number>` | close an issue, with a comment first when `--comment` is given |
| `parley issue comment <slug> <number>` | post a comment on an issue |
| `parley issue label <slug> <number> <label>...` | add labels, keeping the ones the issue has |
| `parley issue unlabel <slug> <number> <label>` | remove one label |
| `parley label create <slug> <name>` | create a label in the repository; succeeds unchanged when it exists |
| `parley pr get <slug> <number>` | show a pull request |
| `parley pr state <slug> <number>` | show whether it is open, closed or merged, and its head |
| `parley pr files <slug> <number>` | list the files it changes |
| `parley pr checks <slug> <number>` | list the check runs of its head |
| `parley pr reviews <slug> <number>` | list its reviews, oldest first |
| `parley pr create <slug>` | open a pull request from a branch the forge holds |
| `parley pr comment <slug> <number>` | post a comment on a pull request |

A pull request is made from a branch on the forge, so push the branch with
`parley workspace push` first.

| Flag | On | Effect |
|---|---|---|
| `--target origin\|upstream` | every `issue`, `pr` and `label` command, and `forge repository` | which forge to address in a carry-patch workspace; origin when not given |
| `--state <s>`, `--assignee <name>` | `issue list` | narrow the list; the state is `open` (the default), `closed` or `all` |
| `--label <name>` | `issue list`, `issue create`, `issue update` | filter by, or set, a label; repeatable |
| `--title <t>` | `issue create` (required), `issue update`, `pr create` (required) | the title |
| `--body <text>`, `--body-file <path>\|-` | `issue create`, `issue update`, `issue comment`, `pr create`, `pr comment` | the text, given directly or read from a file or stdin; not both. A comment requires one |
| `--assignee <name>` | `issue create`, `issue update` | an assignee; repeatable |
| `--state <s>` | `issue update` | the new state |
| `--comment <text>` | `issue close` | a comment to post before closing |
| `--head <branch>`, `--base <branch>` | `pr create` | required: the branch to merge, and the branch to merge into |
| `--draft` | `pr create` | open as a draft |
| `--color <c>`, `--description <text>` | `label create` | the label's colour and description |
| `--since <time>` | `forge deliveries` | only deliveries since this time (RFC 3339) |
| `--generate`, `--secret <string>`, `--from-stdin` | `forge webhook set` | have the hub generate the secret (the default), or give one; at most one of the three |

On `issue update`, `--label` and `--assignee` replace the whole set; use
`issue label` and `issue unlabel` to change one label. `issue update` needs
at least one flag.

**The forge webhook.** The forge can tell the hub what happens on its side
by sending it webhooks, signed with a shared secret. `parley forge webhook
set` creates or rotates that secret and
prints it once; enter it in the forge's webhook settings. `parley forge
deliveries` shows what arrived, which is the first place to look when the
hub seems not to notice the forge.

## `parley sandbox`

A sandbox is an isolated machine the hub starts for a workspace or a fork,
with the repository checked out and a network policy that lets it reach
only what is allowed. It is where an agent runs. These commands create sandboxes, run
things in them, move files, read their logs and decide what more network
access they get. They need a hub with sandboxes configured (see
[configuration.md](configuration.md#sandboxes)) and drive
[api.md](api.md#sandboxes).

```sh
# which kinds of sandbox does the hub offer?
parley sandbox profiles

# a sandbox on a fork, ready to use
ID=$(parley sandbox create demo--greeting --profile agent --wait | jq -r .id)

# run a command in it; the exit status is the command's
parley sandbox exec demo--greeting "$ID" -- make test

# run one command to its end and collect the result
parley sandbox create demo--greeting --profile agent --wait-exit -- make test
```

| Command | What it does |
|---|---|
| `parley sandbox profiles` | list the sandbox profiles the hub offers |
| `parley sandbox create <workspace-slug> [-- command...]` | create a sandbox |
| `parley sandbox list <workspace-slug>` | list a workspace's sandboxes, newest first |
| `parley sandbox get <workspace-slug> <id>` | print one sandbox |
| `parley sandbox exec <workspace-slug> <id> -- <command...>` | run a command in a sandbox |
| `parley sandbox shell <workspace-slug> <id>` | open an interactive shell in a sandbox |
| `parley sandbox logs <workspace-slug> <id>` | print the stored lines of the main process and of the network policy |
| `parley sandbox upload <workspace-slug> <id> <local-path> <sandbox-path>` | copy a local file or directory into a sandbox |
| `parley sandbox download <workspace-slug> <id> <sandbox-path> <local-path>` | copy a file or directory out of a sandbox |
| `parley sandbox checkpoint <workspace-slug> <id>` | save the working directory as the sandbox's checkpoint |
| `parley sandbox stop <workspace-slug> <id>` | stop a sandbox, keeping its working directory |
| `parley sandbox start <workspace-slug> <id>` | start a stopped or ended sandbox again |
| `parley sandbox extend <workspace-slug> <id> --ttl <duration>` | give a sandbox a new time limit, counted from now |
| `parley sandbox delete <workspace-slug> <id>` | delete a sandbox |
| `parley sandbox proposals <workspace-slug> <id>` | list a sandbox's requests for more network access |
| `parley sandbox approve <workspace-slug> <id> <proposal-id>` | approve a request |
| `parley sandbox reject <workspace-slug> <id> <proposal-id>` | reject a request |
| `parley sandbox rules <workspace-slug>` | list the network rules saved for a workspace |
| `parley sandbox rule-delete <workspace-slug> <rule-id>` | delete a saved rule; running sandboxes keep it |

### Creating a sandbox and its life

| Command | Flag | Effect |
|---|---|---|
| `create` | `--profile <name>` | the profile to create it from; the hub's default when not given |
| `create` | `-- command...` | the sandbox's main process |
| `create` | `--env key=value` | an environment variable; repeatable |
| `create` | `--label key=value` | a label to find the sandbox by; repeatable |
| `create` | `--ref <ref>`, `--branch <branch>` | what to check out, and the branch to work on |
| `create` | `--no-checkout` | start without the repository; not with `--ref` or `--branch` |
| `create` | `--restore-from <id>` | start from the checkpoint of this earlier sandbox |
| `create` | `--ttl <duration>` | the time limit, such as `1h` |
| `create` | `--purpose <text>`, `--session <id>` | what it is for, and the agent session it belongs to |
| `create`, `start` | `--wait`, `--timeout`, `--poll-interval` | wait until the sandbox is up: `ready` or `completed` is the success, `failed`, `stopped` and `deleted` are not |
| `create` | `--wait-exit` | wait until the main process has ended; only `completed` is the success. Needs a command |
| `list` | `--state <state>` | only sandboxes in this state; repeatable. Deleted ones are left out unless asked for |
| `list` | `--session <id>`, `--label key=value` (repeatable) | narrow the list |
| `list` | `--include-forks` | include the sandboxes of the workspace's forks |
| `extend` | `--ttl <duration>` | required |
| `delete` | `--no-checkpoint` | do not save the working directory first |

A sandbox's working directory outlives the machine: `stop` keeps it,
`checkpoint` saves it, and `delete` saves a last checkpoint unless you say
otherwise, so `create --restore-from` can pick the work up in a new sandbox.

### Running commands

`exec` runs the words after `--` in the sandbox and behaves like running
them locally: the command's stdout goes to stdout and its stderr to stderr
as they arrive, and `parley` exits with the command's exit code.

```sh
parley sandbox exec demo--greeting "$ID" -- sh -c 'echo out; echo err >&2; exit 3'
echo $?    # 3
```

| Flag | Effect |
|---|---|
| `--workdir <dir>` | the directory to run in |
| `--env key=value` | an environment variable for this command; repeatable |
| `--timeout <duration>` | how long the command may run before it is killed, such as `90s`; the hub's default when not given |
| `--stdin` | send standard input, read to its end, to the command (at most 4 MiB) |
| `--json` | print one JSON document with the collected output and the exit code, instead of streaming |

A command's exit code above 125 is reported as `125`. Status `1` still means
the hub refused the request, or that the command timed out (`wait_timeout`);
status `2` a usage error, or a connection that broke before the command's
end was reported.

`shell` opens an interactive shell on your terminal. Leaving the shell ends
only that shell; the sandbox keeps running.

`logs` prints one JSON object per stored line: the output of the main
process, and the decisions of the network policy.

| Flag | Effect |
|---|---|
| `--source process\|policy` | only the main process's output, or only the network policy's lines |
| `--since <time>` | only lines stored at or after this time (RFC 3339) |
| `--tail <n>` | only the last `<n>` lines |
| `--follow` | keep printing new lines until the sandbox is deleted or you interrupt |

### Copying files

`upload` copies a local file, keeping its mode, or a whole directory into
the sandbox. Symbolic links in a directory are left out, with a warning.
`download` copies a file or a directory out; with `-` as the local path a
file goes to stdout. Both print a small summary of what was written. A
download is complete or absent: a failed one leaves nothing at the local
path.

### Network access

A sandbox can reach only what its policy allows. When a process inside
tries something else, the sandbox records a *proposal* to allow it, and a
person or a supervising agent decides. This keeps an agent from sending
your code anywhere it likes, without making you list every host up front.

```sh
parley sandbox logs demo--greeting "$ID" --source policy --tail 20   # what was denied
parley sandbox proposals demo--greeting "$ID"
parley sandbox approve demo--greeting "$ID" "$PROPOSAL" --scope workspace
```

| Command | Flag | Effect |
|---|---|---|
| `proposals` | `--state <state>` | proposals in this state; the open ones when not given |
| `approve` | `--scope sandbox\|workspace` | allow it for this sandbox only (the default), or also save it as a rule for later sandboxes of the workspace |
| `approve` | `--acknowledge-findings` | approve a proposal although the sandbox runtime reported a serious finding on it |
| `approve`, `reject` | `--comment <text>` | a note for the record |

## `parley session`

A session is the record of one agent run on a workspace: who ran, with
which model, how many tokens it spent, how it ended. Forks, commits,
changes, lands, claims and sandboxes created with `--session <id>` are
linked to it, so that afterwards you can see everything one run did and
what it cost. An agent harness calls these commands at the start, during
and at the end of a run. They drive
[api.md](api.md#sessions-and-telemetry).

```sh
SESSION=$(parley session start demo --archetype coder --model "$MODEL" | jq -r .id)
parley fork create demo --name greeting --session "$SESSION"
# ... the run ...
parley session usage "$SESSION" --model "$MODEL" --input 48211 --output 3920
parley session complete "$SESSION" --status completed --duration-ms 184000
```

| Command | What it does |
|---|---|
| `parley session start <workspace-slug>` | start a session on a workspace you may write to |
| `parley session usage <session-id> --model <name>` | report token usage of an active session |
| `parley session complete <session-id>` | end a session with its outcome; an ended session stays as it is |
| `parley session get <session-id>` | show a session with its token summary and the work linked to it |
| `parley session list` | list the sessions you may read, newest first |

| Command | Flag | Effect |
|---|---|---|
| `start` | `--id <uuid>` | the session's id, when the harness chooses it; generated when not given. Starting the same id again answers the stored session |
| `start` | `--run-id <id>`, `--node-id <id>` | the run the session belongs to, and the node of that run it works on |
| `start` | `--archetype <name>`, `--model <name>` | the kind of agent and the model it runs on |
| `start` | `--metadata <json>` | any JSON value to keep with the session |
| `usage` | `--model <name>` | required: the model the tokens were spent on |
| `usage` | `--input <n>`, `--output <n>`, `--cache-read <n>` | the token counts |
| `usage` | `--id <uuid>` | an id for this report, so that sending it twice counts once |
| `complete` | `--status completed\|failed\|timeout` | the outcome; `completed` when not given |
| `complete` | `--error <text>`, `--duration-ms <n>` | the error message and the run's duration |
| `list` | `--workspace <slug>`, `--include-forks` | only this workspace's sessions, with those of its forks |
| `list` | `--run-id <id>`, `--status <s>`, `--since <time>` | narrow the list |

## `parley events`, `parley audit` and `parley webhook`

Everything that happens on the hub is recorded as an event: a change
opened, a land, a fork created, a sandbox stopped. These three groups are
three ways to use that record. `parley events` follows it as it happens,
for an agent or a script that reacts. `parley audit` queries it afterwards,
together with what agents reported about their own runs, to answer "who did
what". `parley webhook` has the hub deliver events to a URL, for a service
that should not hold a connection open. They drive
[api.md](api.md#audit-and-events) and [api.md](api.md#event-webhooks).

```sh
# react to every change event of one workspace, as it happens
parley events tail --workspace demo --types 'parley.change.*' |
    while read -r event; do echo "$event" | jq -r .type; done

# what did this session do?
parley audit query --session "$SESSION" --limit 50

# deliver landings to a service
parley webhook create demo --url https://ci.example/hooks/parley --types parley.change.land
```

| Command | What it does |
|---|---|
| `parley events tail` | follow the hub's events, one JSON line each, until interrupted |
| `parley events list` | read one page of the event log |
| `parley audit query` | list hub events and agent run events, newest first, one page |
| `parley webhook create <slug> --url <url>` | subscribe a URL to the events of a workspace and its forks |
| `parley webhook list <slug>` | list a workspace's webhooks |
| `parley webhook get <slug> <id>` | show a webhook, with the number of events it has yet to deliver |
| `parley webhook update <slug> <id>` | change a webhook's URL, types or description, or switch it off and on |
| `parley webhook delete <slug> <id>` | delete a webhook and its delivery records |
| `parley webhook rotate-secret <slug> <id>` | replace the signing secret |
| `parley webhook ping <slug> <id>` | send a test delivery |
| `parley webhook deliveries <slug> <id>` | list the deliveries of the last 7 days, newest first |
| `parley webhook redeliver <slug> <id> <delivery-id>` | send one delivery again |

### `parley events`

Every event has a `seq`, its position in the log. That number is how you
never miss one: remember the `seq` of the last event you handled and pass
it as `--after` the next time.

`tail` runs until you interrupt it. It prints each event as one line of
JSON on stdout and nothing else there. When the connection breaks or the
hub restarts, it reconnects by itself, with growing pauses of up to 30
seconds, and continues after the last event it received, so no event is
lost or printed twice; a `reconnecting in …` line on stderr tells you. It
stops only for an answer that a retry cannot fix, such as a bad credential
or a position the log no longer holds (`cursor_expired`, which names the
oldest `seq` still available).

`list` reads one page and returns. Its answer holds `next_after`, the value
for `--after` of the next call. With `--wait` it is a long poll: the hub
holds the request until an event arrives or the time passes, which gives a
script without a streaming client the same promptness.

| Flag | On | Effect |
|---|---|---|
| `--workspace <slug>` | both | only this workspace's events |
| `--types <types>` | both | only these event types, separated by commas; `parley.change.*` matches every type that starts so |
| `--session <id>` | both | only the events of this agent session |
| `--after <seq>` | both | only events after this position. Without it, `tail` starts with what happens from now, and `list` with the oldest event in the log |
| `--limit <n>` | `list` | the most events in the page; 50 when not given |
| `--wait <seconds>` | `list` | how long the hub may hold the request when there is nothing to return |

### `parley audit`

`audit query` reads one page, newest first, of what the credential may see.

| Flag | Effect |
|---|---|
| `--source hub\|agent` | only the hub's events, or only what agents reported about their runs |
| `--workspace <slug>`, `--session <id>`, `--run-id <id>` | narrow by where it happened |
| `--types <list>` | event types, separated by commas, with `*` at the end of an entry as a wildcard |
| `--actor-id <id>` | who did it |
| `--resource-type <type>`, `--resource-id <id>`, `--action <action>` | what it was done to, and what was done |
| `--since <time>`, `--until <time>` | a time range (RFC 3339) |

```sh
parley audit query --workspace demo --types 'parley.change.*,tool.started' --limit 20
```

### `parley webhook`

A webhook delivers a workspace's events to your URL as signed HTTP
requests. `create` prints the signing secret once; your receiver uses it to
check that a delivery really comes from the hub. The hub tries a failed
delivery again and keeps a record of each (see
[api.md](api.md#event-webhooks)).

| Command | Flag | Effect |
|---|---|---|
| `create` | `--url <url>` | required: where to deliver |
| `create`, `update` | `--types <list>` | only these event types, separated by commas; every type when not given |
| `create`, `update` | `--no-forks`, and on `update` `--forks` | leave out, or include, the events of the workspace's forks |
| `create`, `update` | `--description <text>` | a note |
| `update` | `--url <url>` | a new URL |
| `update` | `--enable`, `--disable` | switch deliveries on or off |
| `list`, `deliveries` | `--state <s>` on `list`, `--status <s>` on `deliveries` | narrow the list |

`ping` and `redeliver` are for testing and repair. Each makes one delivery
now and prints its record. When the receiver did not accept it, the record
is still printed, stderr gets `delivery_failed` and the exit status is `1`,
so `parley webhook ping demo "$ID" && echo ok` tests a receiver end to end.

## `parley mcp`

```text
parley mcp [--list] [--groups <groups>] [--read-only] [--workspace <slug>]
           [--sandbox <id>] [--no-code-mode] [--script-timeout <duration>]
```

`parley mcp` gives an agent the commands of `parley` as tools. It is an MCP
server on standard input and output: an agent harness that speaks MCP
starts it, and the agent can then read code, open changes, land and use
sandboxes with typed tool calls instead of shell commands and output
parsing. Every tool is one `parley` command, with the same arguments, the
same checks and the same result, so what you learn from this document holds
for the tools too.

A harness configuration runs it like any command, with the hub URL and the
credential in its environment:

```sh
PARLEY_ENDPOINT_URL=https://hub.example PARLEY_API_KEY="$FORK_TOKEN" \
    parley mcp --workspace demo--greeting
```

`parley mcp --list --groups scm:read,change` prints what a session would
offer, without starting one.

| Flag | Effect |
|---|---|
| `--list` | print the tools that would be served, as a JSON array of `{name, description, input_schema}`, and exit |
| `--groups <groups>` | the command groups to serve, separated by commas; `name:read` serves only a group's reading commands. Default: `activity,change,claim,fork,land,sandbox,scm,workspace:read` |
| `--read-only` | serve only commands that read from the hub, whatever the groups say |
| `--workspace <slug>` | make this the default workspace of every tool, so that calls may leave the workspace out; a call may still name another |
| `--sandbox <id>` | also serve the tools of this sandbox of `--workspace` (searching, editing and running commands inside it) |
| `--no-code-mode` | do not serve the `code_mode` tool |
| `--script-timeout <duration>` | the longest a `code_mode` script may run, from `1s` to `30m`; 10 minutes when not given |

### Which commands become tools

A tool is named after its command, with `_` between the words:
`parley change land` is `change_land`. Its parameters are the command's
arguments and flags, with `_` for every other character (`--expected-version`
is `expected_version`); the words after `--` are the array `command`.

Some commands are never tools, to keep an agent away from what it should
not hold or cannot use:

- anything that needs an administrator, and the groups `admin`, `keys`,
  `tokens`, `secrets`, `login`, `credential-helper`, `help`, `version` and
  `mcp`, which cannot be named in `--groups`;
- commands that print a secret shown once: `webhook create`,
  `webhook rotate-secret`, `forge webhook set`. `fork create` is served,
  because an agent that forks needs the fork's token;
- commands that need a terminal or run until interrupted: `sandbox shell`,
  `events tail`, and `--follow` of `sandbox logs`.

The flags `--api-key`, `--endpoint-url`, `--json` and `--poll-interval` are
not parameters: the hub and the credential are fixed when the session
starts. A session may serve at most 150 tools.

`--read-only` means the session sends the hub nothing but reads. It does
not confine the machine `parley mcp` runs on: tools that take a local path
(`scm_cat` with `output`, `sandbox_download`, `scm_commit` with `put`, and
others) read and write files there as the user who started it. Start it as
a user, and in a directory, that the agent may read and write.

### What a call returns

Every call returns one JSON object, `{"ok", "data", "error", "detail"}`:

| The command | The result |
|---|---|
| prints a JSON object | `ok` true, the object as `data` |
| prints another JSON value or plain text | `ok` true, `data` is `{"result": …}` |
| prints nothing | `ok` true, `data` is `{}` |
| is refused by the hub | `ok` false, `error` the hub's `error_type`, `detail` its message |
| ends with one of the client's own errors | `ok` false, `error` is `usage_error`, `client_error`, `wait_failed`, `wait_timeout` or `condition_not_met`; what the command printed is still in `data` |
| exits with a status of its own (`sandbox_exec`) | `ok` false, `error` is `command_exit`, the collected output in `data` |
| is called with a wrong, unknown or missing parameter | `ok` false, `error` is `invalid_arguments`; nothing was sent |

A failed call is a result the agent can read and act on, not a protocol
error. A call is bounded at 30 minutes; a command with `wait` waits inside
the call. A tool never reads standard input, so `-` where a command would
read stdin is refused. A command that destroys data still needs its
`confirm` parameter.

### Code mode

`code_mode` is one more tool, bound to all the others. A call runs a short
Starlark script that calls the tools as functions, several at once if it
likes, and returns only what the script prints and returns. It exists
because many agent tasks are a chain: list the changes, read three of them,
compare. Done as single calls, every intermediate result passes through the
model. Done as one script, only the conclusion does, which is faster and
costs far fewer tokens.

### A sandbox's tools

With `--sandbox <id>`, the session also serves the tools the sandbox itself
offers, under their own names: reading, searching and editing files, and
running commands inside it. They are bound to `code_mode` too, so one
script can edit in the sandbox and read a change on the hub. With
`--read-only`, the sandbox's writing tools are not served. The sandbox must
be up when the session starts; otherwise `parley mcp` ends with the hub's
error (see [api.md](api.md#sandboxes)).

### What the session logs

Standard output carries only the protocol. On stderr, a session writes one
JSON line at start with the number of tools served, and one per finished
call with the tool, the outcome and the duration, never the arguments or the
result: `{"tool":"change_land","ok":false,"error":"wait_timeout","duration_ms":60012}`.

The session ends, with exit status `0`, when the harness closes its end.
`SIGINT` and `SIGTERM` end it and cancel the calls that are running.
