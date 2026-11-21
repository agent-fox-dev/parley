# parley

Where agents negotiate with your code - Rarr !

Parley is a git-native hub for AI coding agents and the people who run
them. It holds everything an agent run needs except the agent itself: the
repository and a private copy of it for each task, the way back to the main
branch, the credentials and secrets, a sandbox to run in, and a record of
what happened and what it cost.

**Contents:** [Why](#why) · [How it works](#how-it-works) ·
[Quick start](#quick-start) · [What you get](#what-you-get) ·
[Build and deploy](#build-and-deploy) · [Tests](#tests) ·
[Documentation](#documentation)

## Why

Pointing coding agents at real repositories raises the same questions every
time:

- **Where does the agent's copy of the code live**, and how do several
  agents work on one repository without stepping on each other?
- **How do their changes get back** into the main branch without a person
  rebasing and merging every branch by hand?
- **Who decides what may land?** Reviewing every line an agent writes does
  not scale; reviewing nothing is not acceptable for the sensitive parts of
  a codebase.
- **Which credentials does an agent get?** Forge tokens and API keys copied
  into an agent's environment are hard to scope and harder to revoke.
- **Where does the agent run**, isolated from the machine it was started on?
- **What did it do, and what did it cost?**

Git hosting was designed for people: a few long-lived clones, branches
named by hand, pull requests read line by line, one merge at a time. Agents
start from a clean copy for every task, produce many small changes in
parallel and can fix a conflict faster than a person notices it. Without a
common answer every team scripts its own: tokens end up in environment
files, branches are merged by whoever notices, and the trail of what an
agent did is scattered across terminals.

Parley is that common answer, as one server you run yourself.

## How it works

You register a repository once; the hub clones it and keeps it in step with
its origin. That is a **workspace**. From then on a task goes round this
loop:

```text
   origin (GitHub, GitLab, any git remote)
      │  sync                         ▲  push the landed branch
      ▼                               │
  ┌─ workspace ───────────────────────┴───────────────────────┐
  │                                                           │
  │  1 fork ──▶ 2 work ──▶ 3 change ──▶ 4 policy ──▶ 5 land   │
  │  a private   git push   a numbered   by path:     rebase  │
  │  copy and    or the     unit with    checks,      on the  │
  │  a token     API, in    an intent    approvals    server, │
  │  for one     a sandbox  and          or nothing   no      │
  │  task        if wanted  versions                  queue   │
  └───────────────────────────────────────────────────────────┘
        every step is a durable, ordered event you can follow
```

1. **Fork.** One call gives the agent a task fork: its own copy of the
   workspace, made without copying history, and a token that works only
   there. The fork expires by itself.
2. **Work.** The agent pushes with stock `git`, or reads and commits
   through the source control API without a clone. It can run inside a
   sandbox that has the fork checked out, no real credentials and no
   network unless someone allows it.
3. **Change.** A push opens a change: the unit of work, with a stable
   number, a statement of intent and a version for every push.
4. **Policy.** The workspace's rules say, by path, what may land
   automatically, what needs a passing check and what needs a person.
5. **Land.** The hub rebases the change on the server and moves the target
   branch with a compare-and-swap. Changes that do not conflict never wait
   for each other; a conflict comes back as data the agent can act on.

Three things stay fixed: stock `git` clients keep working, git remains the
storage and exchange format (leaving Parley is a `git clone`), and the hub
is one process with embedded storage, sized for a team and its agents.

[Architecture](docs/architecture.md) explains each step and the reasons
behind it.

## Quick start

Parley is two programs: `parleyd`, the hub, and `parley`, its client. You
need Go 1.27 and, where the hub runs, `git` 2.38 or later.

```sh
go build ./cmd/...                        # builds ./parleyd and ./parley

./parleyd --admin-email you@example.com   # first run: writes ./admin_token and exits
#  save the token somewhere safe and delete the file; the hub does not
#  start while it is there
./parleyd                                 # serves on 0.0.0.0:8080
```

The admin token administers the hub. People sign in through an identity
provider you [configure](docs/configuration.md#sign-in-providers), which
gives each of them an API key:

```sh
parley login --endpoint-url http://localhost:8080      # browser sign-in
```

Register a repository, then give an agent a fork and a token for one task:

```sh
parley workspace create --slug demo \
    --git-url https://github.com/acme/demo.git --git-pat "$GITHUB_TOKEN" --wait

parley fork create demo --name greeting \
    --token-permissions git:write,changes:write \
    --purpose "Add a greeting endpoint"
# → the fork "demo--greeting", its git remote and a token that works only there
```

The agent works in the fork with plain git. Its push opens a change:

```sh
git clone "$REMOTE" && cd demo--greeting
git switch -c greeting                    # …edit, commit…
git push -u origin greeting \
    -o parley.title="Add greeting endpoint" \
    -o parley.intent="Issue #41 asks for a greeting route"
```

Land the change when policy allows it, and publish the branch to origin:

```sh
parley change land demo 12 --wait
parley workspace push demo main
```

The [CLI guide](docs/cli.md#getting-started) walks through the same path
step by step, and the [API reference](docs/api.md) shows
how an agent does all of it over HTTP alone.

## What you get

| Capability | What it gives you | Read more |
|---|---|---|
| Identity and scoped access | Sign-in, API keys, and access tokens limited by scope, by workspace and by path | [API](docs/api.md#using-the-api) |
| Workspaces and task forks | A repository registered once and kept in sync; a cheap, expiring copy per task | [API](docs/api.md#workspaces) |
| Git and source control API | Stock git over HTTP, or files, history, diffs, search and commits without a clone | [API](docs/api.md#source-control) |
| Changes and stacks | Versioned units of work with an intent, optionally stacked on each other | [API](docs/api.md#changes) |
| Landing and policy | Optimistic server-side landing, conflict reports, rules by path, approvals and checks | [API](docs/api.md#landing) |
| Awareness | See which in-flight work touches the paths you are about to edit | [API](docs/api.md#awareness) |
| Carry-patch workspaces | A patch set kept on top of a moving upstream, rebuilt when upstream moves | [API](docs/api.md#carry-patch-workspaces) |
| Secrets, variables, forge | Values that can be written but never read back; GitHub and GitLab through the hub's credential | [API](docs/api.md#secrets-and-variables) |
| Sandboxes | Isolated environments on OpenShell, attached to a fork, without credentials or network by default | [API](docs/api.md#sandboxes) |
| Sessions, audit, events | What each agent run did and cost; a durable event stream and signed webhooks | [API](docs/api.md#audit-and-events) |

## Build and deploy

- **Binaries.** `go build ./cmd/...`. `parleyd` uses a C toolchain (cgo)
  to outline source files in languages other than Go; built without one it
  still runs and outlines Go alone. `parley` needs none and builds with
  `CGO_ENABLED=0`.
- **State.** The hub keeps two embedded SQLite stores and a directory of
  git repositories under its data directory. There is no external
  database or broker to run.
- **Backup.** `parleyd backup <directory>` writes a consistent snapshot
  while the hub runs; restoring is copying the snapshot into place.
- **Containers.** `containers/hub/Dockerfile` builds the hub image and
  `containers/sandbox/Dockerfile` a reference sandbox image;
  `deploy/kubernetes/` runs one hub on a persistent volume.

[Running the hub](docs/architecture.md#running-the-hub) explains start,
stop, backup and upgrades; [Configuration](docs/configuration.md) lists
every key.

## Tests

| Command | What it proves |
|---|---|
| `go test ./...` | the unit and in-process tests of every package |
| `go test ./test/acceptance/...` | the product end to end: builds both programs, starts real hubs and drives them only over HTTP, the `parley` binary and stock `git` |
| `go test ./test/perf/` with `PARLEY_PERF_URL` set | the [performance targets](docs/performance.md) against a hub you provide |

The longer acceptance scenarios run only when asked for:

| Variable | Turns on |
|---|---|
| `PARLEY_ACCEPTANCE_LONG=1` | scenarios that wait minutes to an hour |
| `PARLEY_ACCEPTANCE_LARGE_REPO=1` | a 1 GiB repository |
| `PARLEY_ACCEPTANCE_REFERENCE_HOST=1` | the latency targets |
| `PARLEY_ACCEPTANCE_OPENSHELL_GATEWAY_URL`, with `PARLEY_ACCEPTANCE_OPENSHELL_TOKEN_FILE` or `PARLEY_ACCEPTANCE_OPENSHELL_TLS_DIR` (mutual TLS), and `PARLEY_ACCEPTANCE_SANDBOX_IMAGE` | the sandbox scenarios, against a real OpenShell gateway (see [errata 55](docs/errata/55_acceptance_suite.md#gaps-tracked-as-issues) for the image and a local gateway) |

## Documentation

| Document | Read it to |
|---|---|
| [Architecture](docs/architecture.md) | understand how Parley works and why it is built this way |
| [CLI guide](docs/cli.md) | run the hub with `parleyd` and work with it through `parley` |
| [API reference](docs/api.md) | write a client or an agent against the REST API and git |
| [Configuration](docs/configuration.md) | configure the hub: sign-in, sandboxes, secret keys, retention |
| [Performance targets](docs/performance.md) | measure a hub against its targets |
| [Decisions](docs/adr/) | read why a structural choice was made |
| [Product requirements](docs/prd/00_overview.md) | read what the product is meant to do, section by section |
