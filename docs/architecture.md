# Architecture

This document explains how Parley works and why it is built the way it is.
It is written for someone who will operate a hub, integrate with one or
start contributing to the code, and it stays at the level of ideas,
objects and guarantees: what the hub promises, what happens when something
fails, and the reasons behind the design. For the exact contract of a
route, a command or a configuration key, follow the links to the
[API reference](api.md), the [CLI guide](cli.md) and
[Configuration](configuration.md); for the detail of an implementation,
read the code, which [Code map and decisions](#code-map-and-decisions)
helps you find.

## Contents

| Section | The question it answers |
|---|---|
| [Parley in one page](#parley-in-one-page) | What is Parley, what does it talk to, and who uses it? |
| [The ideas behind it](#the-ideas-behind-it) | Which ideas shape the hub, and what does it deliberately not do? |
| [The life of a task](#the-life-of-a-task) | How do the parts connect, from a fork to a landed change? |
| [One process and its stores](#one-process-and-its-stores) | Why one process, and what does it keep where? |
| [The path of a request](#the-path-of-a-request) | What happens to a request before a handler sees it, and why in that order? |
| [Identity and access](#identity-and-access) | Who is a caller, which credential do they hold, and what may it reach? |
| [Workspaces and task forks](#workspaces-and-task-forks) | How does a repository get into the hub, stay in step with origin, and get forked cheaply? |
| [The git server](#the-git-server) | How are fetches and pushes served, and where are the rules for moving a ref? |
| [The source control API and the code index](#the-source-control-api-and-the-code-index) | How does an agent read, search and commit without a clone? |
| [Changes and stacks](#changes-and-stacks) | What is a change, how do versions arise, and how do changes build on each other? |
| [Landing and policy](#landing-and-policy) | How does work reach the target branch without a merge queue, and who decides whether it may? |
| [Awareness](#awareness) | How do agents find out what the others are working on? |
| [Carry-patch workspaces](#carry-patch-workspaces) | How is a patch set kept on top of an upstream that moves? |
| [Secrets and variables](#secrets-and-variables) | How are values stored, resolved and protected at rest? |
| [Forge integration](#forge-integration) | Why do agents reach GitHub and GitLab through the hub? |
| [Sandboxes](#sandboxes) | Where does an agent run, and what do the hub and OpenShell each do? |
| [Sessions, telemetry and audit](#sessions-telemetry-and-audit) | How is what an agent did, and what it cost, recorded? |
| [Events and event webhooks](#events-and-event-webhooks) | How does everything the hub does become an ordered, durable record that others can follow? |
| [Background work](#background-work) | What runs outside requests, and what survives a restart? |
| [Running the hub](#running-the-hub) | How is a hub bootstrapped, started, stopped, backed up and upgraded? |
| [The client](#the-client) | How is `parley` built, and why does it speak only the REST API? |
| [Code map and decisions](#code-map-and-decisions) | Where is what in the code, and which decisions are recorded? |

## Parley in one page

Parley is a hub that AI coding agents, and the people who run them, work
through. It ships as two programs:

- **`parleyd`**, the hub: one server process that holds the repositories
  and everything around them.
- **`parley`**, the client: a command-line program that speaks the hub's
  REST API and nothing else.

The hub answers on one HTTP listener:

```text
  parley, HTTP clients      git clients        GitHub / GitLab
         │                       │               webhooks │
         ▼                       ▼                        ▼
 ┌──────────────────────── parleyd ─────────────────────────────┐
 │ /api/v1/…           the REST API: identity, workspaces,      │     ┌───────────┐
 │                     source control, changes, landing,        │     │ OpenShell │
 │                     policy, secrets, forge, sandboxes,       │ ──▶ │ gateway   │
 │                     sessions, events                         │     │ (optional)│
 │ /git/<org>/<slug>   git smart HTTP for stock git clients     │     └─────┬─────┘
 │ /hooks/forge/<slug> the forge's webhooks, turned into events │           ▼
 │ /healthz /readyz /version /metrics                           │       sandboxes
 └───────┬──────────────────┬──────────────────┬────────────────┘
         ▼                  ▼                  ▼
     state store       telemetry store    bare git repositories;
     (records and      (what agents       forks share objects
     the event log)    report of runs)    with their parent
```

Outwards, the hub fetches from and pushes to each repository's origin,
calls the forge's REST API with the workspace's own credential, sends event
webhooks to the URLs that workspace owners subscribe, and, when sandboxes
are enabled, drives one OpenShell gateway.

Who uses it:

| Who | With which credential | To do what |
|---|---|---|
| Operator | the admin token | deploy and configure the hub, manage users and organizations, define sandbox profiles |
| Developer | an API key from `parley login` | register repositories, write landing policy, approve what needs a person, mint tokens for agents |
| Agent | an access token bound to one workspace or fork | fork, read and write code, open changes, fix conflicts, land, report its session |
| Review agent or check runner | an access token with the review scopes | follow events, report checks, approve or reject versions |
| Forge | a webhook secret | deliver webhooks the hub turns into events |

The rest of this document follows that picture: first the ideas, then one
task from fork to landed change, then each part of the hub in turn.

## The ideas behind it

Git hosting was designed for people: a few long-lived clones, branches
named by hand, pull requests read line by line, one merge at a time. Agents
are always online, start from a clean copy for every task, produce many
small changes in parallel and can fix a conflict faster than a person
notices it. Eight ideas follow from that, and each one explains a part of
the hub.

1. **The change is the unit of work, not the branch.** A change has a
   stable number, versions, a statement of intent and a link to the agent
   session that produced it. Landing, review, audit and events all address
   changes. See [Changes and stacks](#changes-and-stacks).
2. **Changes stack.** A change may depend on another. When the lower one
   moves, the hub rebases the ones above it, so an agent does not wait for
   a review before building on its own work.
3. **Repositories are cheap.** A task fork is created in one call, without
   copying history, together with a token that works only there, and it
   expires by itself. See
   [Workspaces and task forks](#workspaces-and-task-forks).
4. **Source control is an API.** Files, history, diffs and blame can be
   read, and commits created, without a clone. Git over HTTP stays
   available for everything else. See [The git server](#the-git-server)
   and
   [The source control API and the code index](#the-source-control-api-and-the-code-index).
5. **Landing is optimistic.** Landing is a rebase on the server and a
   compare-and-swap on the target branch. Changes that do not conflict
   never wait for each other, and a conflict comes back as data an agent
   can act on. See [Landing and policy](#landing-and-policy).
6. **Policy decides what may land, by path.** Rules per workspace say
   which paths land automatically, which need checks and which need a
   person, so people review where it matters and nowhere else.
7. **Agents can see each other.** One call answers which in-flight changes
   and claims touch a set of paths. See [Awareness](#awareness).
8. **Events are durable.** Every push, fork, version, land and conflict is
   an ordered, replayable event, so review agents and CI can react
   reliably. See [Events and event webhooks](#events-and-event-webhooks).

Three things stay fixed, and they rule out a number of designs:

- **Stock git keeps working.** There is no custom client, wire protocol or
  virtual filesystem.
- **Git remains the storage and exchange format.** Leaving Parley is a
  `git clone`.
- **The hub is one process.** It is sized for a team and its agents, not
  for a hyperscale monorepo, and needs no external database or message
  broker. See [One process and its stores](#one-process-and-its-stores).

What Parley deliberately is not: a code review user interface (it provides
the review data and the API), a CI system (external runners report checks),
an issue tracker (issues stay on the forge) or a sandbox runtime of its own
(isolation is OpenShell's job; the hub is the control plane).

## The life of a task

One task, from a registered repository to a commit on the main branch,
touches most of the hub. The sections after this one explain each step;
this one shows how they connect.

**A developer registers a repository once.** The hub records a workspace,
clones the repository in the background and from then on keeps it in step
with its origin.

```sh
parley workspace create --slug demo \
    --git-url https://github.com/acme/demo.git --git-pat "$GITHUB_TOKEN" --wait
```

**An agent takes a task and gets a fork.** The fork is a workspace of its
own, `demo--greeting`, that shares the parent's objects, so creating it
costs the same for a large repository as for a small one. The call also
returns a token that works only in that fork: whatever the agent does with
it, it cannot reach another repository.

```sh
parley fork create demo --name greeting \
    --token-permissions git:write,changes:write \
    --purpose "Add a greeting endpoint"
```

**Before it starts, the agent can look around.** It asks which in-flight
changes and claims touch the paths it is about to edit, and may leave a
claim of its own.

```sh
parley activity demo --paths 'api/**'
```

**The agent does the work.** It clones the fork with stock git, or reads
and commits through the source control API without a clone. When the hub
has sandboxes enabled, the fork can come with a sandbox that has the fork
checked out, and the agent runs in there, with placeholders instead of
credentials and no network unless a person allows it.

**A push opens a change.** The push passes the hub's push rules (the
protected branch cannot be pushed to, the parent's branches are read-only
in the fork, a path-limited token stays inside its paths) and becomes
version 1 of a change with a number, a title and an intent. A later push
to the same branch is version 2 of the same change.

```sh
git push -u origin greeting \
    -o parley.title="Add greeting endpoint" \
    -o parley.intent="Issue #41 asks for a greeting route"
```

**Policy says what the change needs.** The workspace's rules map the paths
the change touches to requirements: nothing, a passing check from an
external runner, or an approval from a person. A check runner and a review
agent learn about the new version from the event stream or a webhook and
report back on exactly that version.

**The change lands.** The hub rebases the change onto the current head of
the target branch and moves the branch with a compare-and-swap. If another
change landed in between and the two do not conflict, the hub tries again
and nothing waits. If they do conflict, the answer says which files and
which landed changes are involved, and the agent fixes its branch and
lands again.

```sh
parley change land demo 12 --wait
```

**The result leaves the hub.** The landed branch is pushed to origin, the
fork is removed a day after its work has landed, and the events of every
step stay in the log, tied to the agent's session, so that "who changed
this line, why, and what did it cost" has an answer.

```sh
parley workspace push demo main
```

An agent with no local checkout does the same through the API alone: it
creates the commit, opens the change and lands it with three requests. The
[API reference](api.md) documents those routes and the
[CLI guide](cli.md#getting-started) the commands used here.

## One process and its stores

Parley is built for a team, not for hyperscale, and it has to be something
one person can install, back up and move. So the hub, `parleyd`, is one
process that embeds its storage. It needs no external database, cache or
message broker; the only programs it depends on are `git` on its `PATH`
and, when sandboxes are turned on, the sandbox runtime's gateway. The price
is stated plainly: one hub per set of data. Two processes on the same data
are not supported, and there is no replica to fail over to.

```text
                 HTTP (REST, git, webhooks, probes)
                               |
                     +---------+---------+
                     |      parleyd      |
                     |  requests + work  |
                     +---------+---------+
          +----------------+---+------------+----------------+
          |                |                |                |
     state store     telemetry store   workspace root    secret keys
     (parley.db)     (analytics.db)    (bare git repos)  (environment
          \________ data directory _______/               or key file)
```

What the hub keeps, and what losing each one costs:

| What | Holds | If it is lost |
|---|---|---|
| The state store | Everything with a state: users, orgs, credentials (as hashes), workspace records, changes, lands, policies, claims, sessions, jobs, sealed secrets, the event log and its audit trail, idempotency records | The hub is a new hub. The repositories are still on disk, but nothing says whose they are |
| The telemetry store | What agents report about their runs: run events, outcomes, tool calls, traces, postmortems | History of agent runs. The hub works without it and starts collecting again |
| The workspace root | One bare git repository per workspace and task fork | The code that was only on the hub: unlanded changes, fork branches, patch branches. A workspace with an origin can be cloned again |
| The secret keys | The key that seals stored secrets, given by the environment or a key file, never stored in the data | Stored secrets cannot be opened any more. Everything else works; the secrets have to be written again |

Both stores are embedded SQLite databases in the data directory. They are
separate files so that the high-volume telemetry never competes with the
writes that decide what the hub's state is. Writes to the state store take
turns inside the process: one write transaction at a time. That is what
makes several guarantees cheap. An event is written in the same
transaction as the change it reports, so the log never disagrees with the
state, and "check, then insert" needs no retry loop.

Anything else on disk is derived and rebuilt when it is missing, such as
the code index. Anything in memory is safe to lose: rate-limit counters,
caches and open streams (clients resume the event stream by position).

### How the features are put together

Each feature (identity, workspaces, changes, landing, sandboxes and so on)
brings its own configuration keys, schema steps, start-up work, background
work, maintenance tasks, routes and request checks. All of them are
assembled in one place, from one explicit, ordered list
([ADR 01](adr/01-compose-the-hub-in-one-package.md)). The order is written
down because it matters: schema steps and start-up work run in the order of
the list, not in an order that follows file names or imports.

This gives three things. The start has one fixed sequence (configuration,
data, bootstrap, start-up work, listener). A mistake in the assembly, such
as two features claiming one name, is a start error with one log line, not
a crash later. And a test starts the complete hub in-process, the same way
the binary does, so what is tested is what runs.

Features reach each other the same way: a feature that owns data for a
user, an org or a workspace registers a remover with the feature that
deletes those, and a feature that must hear about a push registers with the
one component that writes refs. See
[ADR 03](adr/03-register-removers-of-owned-data-on-the-identity-service.md),
[ADR 07](adr/07-register-the-teardown-collaborators-on-the-workspace-service.md)
and [ADR 08](adr/08-register-the-push-collaborators-on-the-ref-writer.md).

## The path of a request

Every REST route passes the same checks in the same order. You do not have
to learn per route what happens to a bad credential or an oversized body,
and no route can forget a check. The exact contract is in
[Using the API](api.md#using-the-api); this section explains the order.

Before any check, every response on every path gets a request id and the
security headers. The hub takes the id from your `X-Request-ID` when it is
a UUID and generates one otherwise, and returns it in the same header.

1. **Route matching.** An unknown path is `404`, a known path with the
   wrong method is `405`. Paths are matched exactly and never redirected.
   This comes before the credential is read, so the answer tells nothing
   about who is asking.
2. **Authentication.** A route that is not public needs a credential. The
   credential and its user are read on every request, so a revocation or a
   block holds from the next request on.
3. **Rate limit.** Counted per credential, or per client address on a
   public route. It runs before the body is read, so a caller over the
   limit costs the hub almost nothing.
4. **Body size and content type.** The body is read once, up to the limit.
5. **Feature check and binding guard.** A route of a feature that is turned
   off answers here. A token bound to a workspace is confined here, before
   its scopes are looked at.
6. **Scope check.** A personal access token must hold the scope the route
   declares. This comes before the access checks, so the answer is the
   same whether or not the resource exists.
7. **Access checks.** The admin check on admin routes, then the workspace
   access rule on workspace routes (see
   [Identity and access](#identity-and-access)).
8. **Idempotency**, on routes that create a resource.
9. **The handler.**

The first check that fails answers the request and nothing later runs. The
order is fixed for two reasons. Cheap checks come before expensive ones.
And a check never reveals what a later check protects: a caller without a
valid credential learns nothing about scopes, and a caller without the
scope learns nothing about which workspaces exist. A check may hand what it
found to the handler, so the access decision is taken once and the handler
does not repeat it
([ADR 04](adr/04-let-a-check-attach-a-value-for-the-handler.md)).

Surfaces outside the REST API, such as git over HTTP, the forge webhook
ingress, the probes and the metrics, do not pass this pipeline. Git
resolves the same credentials and asks the same access rule, and counts
against the same rate limit.

### What a client gets from it

- **One error envelope.** Every error, from any check or handler, is a JSON
  object with a status code, a message and a stable `error_type`. Branch
  on `error_type`, not on the message.
- **Request ids.** An unexpected failure is answered `500` with no detail.
  The detail goes to the hub's log with the request id, so the id from the
  response is what you give the operator.
- **A rate limit you can obey.** 600 requests per minute per credential by
  default. A refused request is `429` with `Retry-After`. The counts are in
  memory and start from zero when the hub starts.
- **Safe retries of creates.** Send an `Idempotency-Key` with a create. If
  the answer is lost, send the same request again: the hub replays the
  recorded answer instead of creating a second resource. Records are kept
  for 24 hours in the state store, so a retry works across a restart. A
  secret shown once is left out of a replay. Because the key is checked
  after every other check, a replay is authorised like the original.

### The access log

The hub writes one log line per answered request: method, path, status,
duration and request id. The path is logged without its query string. No
request body and no `Authorization` header is ever logged. Requests to the
health probes are left out unless you turn them on.

## Identity and access

The hub has to answer two questions for every request: who is calling, and
what may they do on this workspace. Both are answered in one place each, so
the REST API, the git server and the event stream cannot disagree.

### Users, orgs and the admin token

A **user** is a person who signed in through a configured identity
provider. The first sign-in creates the user and a personal organization.
A user has a role (`user` or `admin`) and a status (`active` or `blocked`).

An **org** groups users. Every workspace is recorded in one org and has one
owner. Membership of the org is what gives other users access to the
workspace. An org can be blocked, which suspends its members' access
without deleting anything.

A new hub has no users, so the operator administers it with the **admin
token**, created at bootstrap (see [Running the hub](#running-the-hub)). It
belongs to no user. It passes every admin route and reaches every
workspace, but it cannot do what needs a user, such as owning a workspace.

### Three credentials

| Credential | Stands for | Carries |
|---|---|---|
| Admin token | The operator | Nothing: no user, no scopes |
| API key | A person at a terminal | The user. Issued by sign-in; a new sign-in replaces the older keys |
| Personal access token (PAT) | Automation: an agent, a CI job | The user, plus scopes, optional path limits and an optional workspace binding |

There are three because the three callers need different limits. The
operator must be able to act before any user exists. A person should be
able to do everything they are allowed to do without managing scopes. An
agent should get as little as its task needs, and a leaked agent token
should reach as little as possible. Each credential is recognised by its
format. The hub stores only a hash of each secret and shows a secret once,
when it is created.

### Scopes, bindings and path limits

A PAT is narrowed in three independent ways:

- **Scopes** say which kinds of route it may call (`changes:write`,
  `git:read` and so on). A write scope includes the read scope of its area.
  The admin token and API keys are not held to scopes.
- **A workspace binding** confines the token to one workspace and the task
  forks it creates. Every other workspace answers it as if it did not
  exist, and it cannot use account routes, so it can neither read itself
  nor mint another token. A token bound to a task fork also reaches the
  contributor routes of the fork's parent, enough to open and land changes
  there. Refusals are reported as events, so an owner sees a confined token
  probing.
- **Path limits** are globs that the token's writes must stay inside
  (`docs/**`). Reads are not limited.

All three only take access away. A token never has more than its user, and
a PAT that mints another PAT can only hand on scopes and paths it holds
itself.

### The workspace access rule

A caller has one of four levels on a workspace:

| Level | Who | May |
|---|---|---|
| admin | The admin token | Everything an owner may, except what needs a user |
| owner | The workspace's owner | Read, write, change settings, delete |
| member | An active member of the workspace's org | Read |
| none | Everyone else | Nothing |

A member who wants to contribute creates a task fork and opens changes
from it into the parent. On a fork, the fork's owner and the parent's owner
are owners and everyone who reads the parent is a member; a fork never
grants more than its parent. The level does not depend on the kind of
credential: a user has the same level by API key as by PAT. A user's
`admin` role opens the admin routes and gives no level on any workspace.

A caller with level none gets `404 not_found`, with exactly the answer an
unknown slug gets, on every surface. This is deliberate: a `403` would
confirm that the workspace exists, and workspace names often say what a
team is working on. A member who tries to write gets `403`, because they
can already see the workspace.

### Deleting a user or an org

Deleting a user removes the user's API keys, tokens and memberships, and
whatever other features keep for that user, such as the user's secrets.
Orgs the user owned are kept, without an owner. Deleting an org removes its
memberships and what features keep for the org; its users are untouched.
Workspace records outlive both: a workspace whose owner is gone matches no
caller, and one whose org is gone grants no membership, until an admin
deals with it.

Each delete is one transaction. Features register what they own for a user
or an org with the identity feature, and their removal runs inside the
delete's transaction
([ADR 03](adr/03-register-removers-of-owned-data-on-the-identity-service.md)).
If any part fails, nothing is deleted, so you never get a user who is half
gone.

## Workspaces and task forks

A workspace is the hub's record of one git repository, and the object
every other feature hangs its data on. A **repository workspace** is
registered once from a git URL, its **origin**: the hub clones it, can
sync from it and can push branches back. A **task fork** is a short-lived
workspace made from a repository workspace, its **parent**, for one agent
and one task. Git hosting assumes a few long-lived clones; agents start
from a clean copy for every task, so that copy has to cost almost nothing.

### Record, repository and status

A workspace is a record in the store (slug, owner, organization, status,
settings, default branch and its head) and a bare repository on disk under
`[workspace].path`. Beside the branches, the repository holds the **origin
heads**, the last value the hub saw on origin for each branch, and the
hub-owned refs under `refs/parley/`. The credential for origin is a
workspace secret, read at every use, so a rotated secret counts from the
next operation; a secret that cannot be read fails the operation and never
turns into an anonymous request.

| Status | Meaning |
|---|---|
| `pending`, `cloning` | registered; the clone is waiting or running |
| `ready` | the repository is served |
| `failed` | the clone failed; `clone_error` says why |
| `archived` | kept and readable through the API, not served over git |

Only `ready` serves git and takes writes. While a workspace is `pending`
or `cloning`, or an archive, reclone or delete runs on it, routes that
need the repository answer `409 workspace_busy`: wait and try again.

### Clone, sync and push to origin

**Clone.** Creating a workspace stores it as `pending` and returns; the
record is the work queue. At most `[workspace].workers` clones run at
once, and a starting hub picks up every workspace it finds `pending` or
`cloning`. A clone is built aside and renamed into place only when
complete, so a stop never leaves a half-made copy that looks valid. A
failed clone is not retried on its own: fix the cause and reclone.

**Sync** fetches origin and decides branch by branch. A branch the hub
lacks is created, one that is behind is fast-forwarded, one that is ahead
is left, and a **diverged** branch is reported and left, unless the caller
asked for `reset_to_origin`. Tags are only ever added. Each move is a
compare-and-swap, so a sync never overwrites a push or a land that arrived
meanwhile: it looks again and re-decides. If origin is unreachable or
refuses the credential, the sync answers `502 upstream_error`, records the
reason in `sync_error` and has changed nothing.

**Push to origin** publishes one branch, explicitly: landing changes only
the hub. A normal push never forces; when origin has commits the hub
lacks, the answer is `409 non_fast_forward` and the fix is a sync. A
forced push is a force-with-lease against the origin head the hub last
recorded, so it cannot discard commits the hub never saw
(`409 origin_moved`), and it is refused for protected branches. Origin's
own refusals come back as distinct errors (`origin_rejected`,
`origin_unauthenticated`, `origin_unreachable`, `origin_timeout` and so
on).

### Archive, reclone and delete

**Archive** retires a workspace without losing anything. The hub first
pushes every branch to origin as a best-effort safety copy and records in
the event how much origin took. **Reactivate** brings it back; the kept
copy is reused, so origin is not contacted. **Reclone** discards the copy
and clones afresh, after the same safety copy; it is refused while the
workspace has forks, which borrow its objects. **Delete** removes an
archived repository workspace, or a fork at any time.

A delete takes everything that belongs to the workspace and its forks:
bound tokens, sandboxes, secrets and variables, changes, land attempts,
policy, approvals and checks, claims, patches, jobs, sessions and the code
index. The store's part is **one transaction**: either all of it is gone,
or nothing changed and the delete can be repeated. Each feature registers
how to remove its own data with the workspace service, so a new feature
cannot be forgotten, and an expiring fork is removed by exactly the same
path
([ADR 07](adr/07-register-the-teardown-collaborators-on-the-workspace-service.md)).
Sandboxes go first, since they live outside the store; the repositories
are removed from disk after the commit. The event log is not touched, and
the slug can be registered again.

### Task forks

A fork is created in one call and is ready at once, whatever the size of
the parent, because nothing is copied: its repository starts empty and
**borrows the parent's objects**, storing only what is pushed to it.

```text
parent: demo                            fork: demo--greeting
  refs/heads/main     ── tracking ──▶     main     (read-only, live)
  refs/tags/v1.0      ── tracking ──▶     v1.0
  objects             ◀── borrowed ──     objects  (only what the fork adds)
                                          refs/heads/greeting  (own branch)
  refs/parley/changes/12/…  ─ shown ─▶    versions of the fork's own changes
```

**Tracking branches** are the branches and tags the parent had when the
fork was made. In the fork each always equals the parent's branch of that
name: when the parent moves, the fork's view moves with it, with no sync
call. They are read-only, because a fork is a place to propose work, not a
second `main` that can drift. Work goes to **own branches**, which exist
in the fork alone; with `auto_change` on (the default) the first push of
one opens a change in the parent, targeting the branch the fork was taken
from. A fork has no settings or policy of its own, can come with a token
that works only there and with a sandbox (see
[Identity and access](#identity-and-access) and [Sandboxes](#sandboxes)),
and is read-only while its parent is archived.

Forks **expire**, so nobody cleans up after agents. A fork lives
`[forks].default_ttl` (7 days) unless asked otherwise, at most
`[forks].max_ttl`, and can be extended. A sweep runs every minute. With
`[forks].expire_on_land` it also removes a fork early once its work is
done: every change from it is landed or abandoned, every own branch
belongs to one of them, nothing has happened on it for 24 hours and it has
no live sandbox.

Reference: [api.md](api.md#workspaces) and
[api.md](api.md#task-forks-and-workspace-tokens).

## The git server

Agents and people already know git, and every tool speaks it. The hub
therefore serves each workspace and fork as an ordinary git remote over
smart HTTP, on the API's port, at `/git/<org>/<slug>.git`. There is no
custom client or wire protocol, and leaving Parley is a `git clone`. The
credential is the HTTP Basic password; access and scopes are those of the
REST API.

### Fetch and push

A **fetch** is answered by stock git on the workspace's repository, so
shallow, incremental and partial clones work as git defines them. The hub
adds two things: it refuses a request for an object no ref of the
repository reaches, and before serving a fork it brings the fork's view of
its tracking refs up to date with the parent, so a fork is never served
stale.

A **push** is processed by the hub itself, because it must pass the hub's
rules and may become a change. The hub reads the ref updates and the
`parley.*` push options, receives the pack into **quarantine** apart from
the repository, evaluates the push rules for every update, moves all refs
together by compare-and-swap against the values the client sent, maps the
pushed branches to changes, writes the events and answers. A push is
atomic across refs whether or not the client asked: if one ref is refused,
none moves and the objects are discarded. It never waits for another
operation; if a ref moved since the client read it, the client is told to
fetch and try again. `[git].timeout` bounds one request and
`[git].max_push_size` one pack.

### One place where refs are written

A branch can move by a git push, by a write through the source control
API, or by a land. If each surface applied the rules itself they would
drift apart, and an agent would find the surface that forgot one. So there
is a single **ref writer**, and every one of those moves goes through it
([ADR 08](adr/08-register-the-push-collaborators-on-the-ref-writer.md)).
Whatever the surface, the same push rules decide (a land skips them:
policy has already decided what may reach a protected branch), the move is
a compare-and-swap, and what follows a move happens in the same step: the
next version of the change whose source branch moved, the abandon of a
change whose branch was deleted, a fresh mergeability check for the
changes into the branch. Every move is journalled and writes one
`parley.git.push` event that names its surface (`via`: `git`, `api` or
`land`), so a consumer of that event sees every movement of every branch.

### The push rules

The rules run in this order; the first that refuses decides the message.

| Rule | Refuses |
|---|---|
| Hub-owned refs | any write under `refs/parley/`, for every credential |
| Protected branches | creating, force-pushing or deleting a branch matched by the setting `protected_branches` (default: the default branch); a fast-forward too, unless it is an owner direct push |
| Default branch | deleting it |
| Tracking branches | any write to a fork's tracking branch |
| Carry-patch control | the integration branch, and patch branches as the settings say (see [Carry-patch workspaces](#carry-patch-workspaces)) |
| Path limits | for a PAT limited to paths, new commits that change a file outside its globs |

Everything else is allowed: agents create, rewrite and delete their own
branches freely. A rule that cannot be evaluated refuses; rules never fail
open.

An **owner direct push** is the one exemption. While the setting
`protect.owner_push` is on (the default), the owner or the admin token may
fast-forward a protected branch with an API key, the admin token or a PAT
that is neither bound to a workspace nor limited to paths. An agent's
token is never exempt. A direct push is not a land: no policy is
evaluated, its event carries `direct: true`, and its commits belong to no
change. The path limit also covers a write that adds no commit but would
make a version out of existing ones, so a limited token cannot adopt
commits made with another credential.

### How a push becomes a change

After the refs moved, each pushed branch is looked at. If it is the source
of a draft or open change, the push is that change's next version.
Otherwise a change is opened when the push asks for one (`-o
parley.change` or `-o parley.target=<branch>`) or goes to a fork with
`auto_change` on. Otherwise it is just a branch. `parley.title`,
`parley.intent` and `parley.draft` shape the change, and `parley.session`
links the push to an agent session. git shows the outcome:

```text
$ git push -o parley.change -o parley.title="Fix retry loop" origin fix-retry
remote: change demo#12 version 1 (opened)
```

A change that cannot be opened (nothing ahead of the target, a required
intent missing) never fails the push: the refs stay moved and the client
gets a `warning:` line with the reason.

### What the journal guarantees

A git ref and a store record cannot change in one transaction. The ref
writer closes the gap with a journal: it records a write before any ref
moves and clears the entry once the events are written. A starting hub
settles what a stop left, before it serves: a write whose refs moved gets
its events then, and one whose refs did not move is dropped. After a crash
at any point, a write either did not happen or happened with its events.
Changes follow the same rule: a source branch found at a commit its change
has not recorded gets that version at start.

Reference: [api.md](api.md#git-over-http).

## The source control API and the code index

A clone is the expensive way to read one file or make a one-line commit.
An agent orienting itself, a review agent reading a diff or a tool with no
disk can do it over HTTP instead: files, trees, history, diffs, blame and
search can be read, and commits created and refs moved, without a clone.
Git over HTTP and this API are two doors to the same repository.

### Reads and writes

A **read** names a revision, and the hub resolves it once, at the start;
everything after reads by commit id, so a branch that moves during a
request cannot mix two commits into one answer. A read that names a full
commit id is immutable and cacheable; one that names a branch is
revalidated with an `ETag`. Reads are bounded: `[scm].read_timeout` ends
one with `503 scm_timeout`, files over `[scm].max_file_size` are refused,
and large diffs and listings are cut and paged. Two reads do what would
otherwise need a checkout: **compare** tries the rebase of one revision
onto another and returns a conflict report if it does not apply, and
**outline** returns the declarations of one file at any revision;
**declaration** returns the lines of the declarations of one name.

The **write** routes create a commit from a list of file operations,
create, move and delete refs, and rebase a branch on the server. A
commit's operations may also address a declaration by name (replace,
delete, insert beside it): before the tree is built, each becomes a `put`
of its file's new bytes, placed by the outline of the file as the
operations before it left it and refused unless the file parses before
and after, so everything after sees an ordinary commit. None of
them writes refs itself: each goes through the ref writer (see
[One place where refs are written](#one-place-where-refs-are-written)), so
the same push rules decide, a commit to a change's source branch records a
version exactly as a push would, and the same `parley.git.push` event is
written with `via: api`.

Every write is a **compare-and-swap**. The caller states the head it
believes the branch has (`expected_head_sha` on a commit or rebase,
`expected_sha` on a ref move or delete); if the branch is elsewhere, the
answer is `409 ref_moved` with the current head in `details.current_sha`.
Nothing is locked and nothing waits: of two commits on one head the first
wins, and the second re-reads and tries again. The creating routes take an
`Idempotency-Key`, so a retried request does not commit twice.

### The code index

`GET …/scm/search` is an exact match over one revision that reads every
candidate file on each request. Orientation wants ranked search and "where
is this declared", so the hub keeps a **code index** per repository
workspace behind `GET …/scm/code-search` and `GET …/scm/symbols`.

- **What it holds.** The files of the head commit of the default branch,
  written out on disk, and an index over them: ranked text search grouped
  by file, and the symbols declared in those files. Outlines parse one
  file on request and need no index.
- **When it exists.** On a hub whose configuration has `[code_index]`, for
  workspaces whose setting `code_index` is on (the default). A fork is
  answered from its parent's index, and the answer names the parent.
- **When it is built.** When a workspace becomes ready and whenever the
  head of its default branch moves. A build is a durable job (see
  [Background work](#background-work)): it survives a restart and is
  retried. It always takes the head as it is when it starts, and at most
  one more waits beside the one running. A build stopped by its size or
  time bounds leaves a **partial** index, which answers and says so. The
  library that builds it cannot load a partial index from disk, so after
  a restart a partial index is behind and is built again as a job.
- **How it learns a head moved.** That head is written in several places:
  the ref writer, a clone, a sync and a carry-patch rebuild. Each tells
  the index
  ([ADR 11](adr/11-tell-the-code-index-where-a-head-is-written.md)). A
  notice is only a hint: the build reads the real head itself, and a
  maintenance pass, also run at start, queues every index that is behind.
  A lost notice delays the index and never makes it wrong.
- **What a reader gets while it builds.** A search never answers from a
  commit older than the head at the moment the request arrived. If the
  index is behind, the request waits for the build, up to the read
  timeout; when the wait runs out or the last build failed, it gets
  `503 code_index_not_ready` with `Retry-After` and the state. Every
  answer names the commit it searched (`indexed_sha`).
- **Why it can be dropped.** It is derived data: everything in it comes
  from the repository, and it lives in its own directory under the data
  directory. Remove it and the hub builds it again; an index that cannot
  be loaded is treated as missing. A build never writes over the index in
  use, so a failed build leaves the previous one answering.

Reference: [api.md](api.md#source-control).

## Changes and stacks

A branch is how commits travel, but a poor unit of work: it has no stable
name once rewritten, no memory of its earlier states and no record of why
it exists. The **change** is the durable object on top. It says "add the
commits of this source branch to that target branch, for this reason", and
everything downstream addresses it: policy evaluates it, reviewers approve
its versions, landing consumes it, and the audit log points at it.

### The change and its versions

A change belongs to the workspace that holds its target branch and has a
number there (`demo#12`) that is never reused. Its source branch is in the
same workspace or one of its forks, and a branch is the source of at most
one draft or open change. It carries a title, an **intent** (why it
exists) and what the hub computes for it: the files it touches, whether it
rebases cleanly, and what policy still requires. It is opened by a push,
by an API commit, or by `POST …/changes`.

| State | Meaning | Moves to |
|---|---|---|
| `draft` | work in progress; never landed | `open`, `abandoned` |
| `open` | proposed for landing | `landed`, `draft`, `abandoned` |
| `landed` | its commits are in the target; final | |
| `abandoned` | given up, or its source branch or fork was deleted | `open` (restore) |

Each time the source branch's head changes while the change is draft or
open (a push, an API write, a sync, or a restack by the hub) the change
gets a new immutable **version**, with its head, its base, the credential
that made it and the session. The hub notes when a version is
**patch-equivalent** to an earlier one, the same diff on a new base, so
that review survives a rebase. Every version's head is kept in the target
workspace under `refs/parley/changes/<number>/<version>`. That is why old
versions stay fetchable after a force-push, after the branch is deleted
and after the fork has expired, and why an abandoned change can be
restored. Only a reclone of the workspace loses them.

### Mergeability

`mergeable` answers one question: would the current version rebase cleanly
onto the head of its target (in a stack, onto the change below)? It is
`clean`, `conflict` with a conflict report, or `unknown` while being
computed. The hub recomputes it in the background after every new version
and every move of the target, by trying the rebase without moving
anything; `[changes].mergeable_debounce` spaces the checks of one change
on a busy target. A move to `conflict` writes `parley.change.conflict`,
and an open change that is clean and ready under policy writes
`parley.change.ready`. Landing never trusts the stored value: it rebases
for real.

### Stacks and restack

A change **depends on** another when its commits sit on that change's
commits. The hub finds the link from the commit graph at every version, so
stacking needs no ceremony: branch from the other branch and push. A
dependent change is measured against the change below, so its diff, paths
and policy cover only its own commits.

When a lower change gets a new version, the hub **restacks** the changes
above it: it replays each one's own commits onto the new head below, moves
its source branch by compare-and-swap and records a version of kind
`restack`. A stack is restacked bottom first, and when a lower change
lands its dependants are restacked onto the landed commit. Pending
restacks are stored and continue after a restart. The setting
`changes.auto_restack` turns this off, and a stack deeper than
`[changes].max_stack_depth` is flagged instead of restacked.

When the replay conflicts, the restack stops: no branch moves, the change
is flagged `needs_restack` with reason `conflict`, its `mergeable` becomes
`conflict` with the report, and nothing above it is touched. The author
either pushes a version that sits on the new base, or calls
`POST …/changes/{number}/restack` with `resolutions`, the content for each
conflicted path, and lets the hub finish the replay.

Reference: [api.md](api.md#changes).

## Landing and policy

Landing adds a change's commits to its target branch. The usual way to
make that safe is a merge queue: one change at a time, each tested on the
last. With many agents producing small changes in parallel, a queue makes
every agent wait for all the others. Parley lands **optimistically** and
puts the safety elsewhere: policy decides beforehand what a change needs,
and afterwards every commit traces to its change and can be undone.

### The land attempt

`POST …/changes/{number}/land` creates a durable **land attempt** and
waits for it (30 seconds by default).

```text
 land request ─▶ change open? expected_version? none running? stack below landed?
      ▼
 attempt running ─▶ policy ── not ready, no override ──▶ blocked
      │ ready
 ┌─▶ read target head T
 │   replay the change's commits onto T ── does not apply ──▶ conflict
 │   compare-and-swap the target: T ─▶ new head
 └──── target moved; try again ([land].max_attempts) ──▶ failed
      │ swapped
      ▼
   landed: target, change, commit map, attempt and event, in one step
```

Policy is evaluated for the version being landed. The hub then rebases on
the server: it replays the change's own commits onto the target's head,
keeping authors and messages (with the setting `land.strategy` at
`squash`, one commit per change). The target moves only if it is still
where it was read; if another land or push got there first, the attempt
rebases again on the new head. Only a commit that does not apply stops it.

The caller gets `200` when it landed, `202` while it runs (poll the
attempt), `409 land_conflict` or `409 policy_blocked` with the attempt in
`details`, and `503 land_failed` (`contention`, `timeout`,
`origin_rejected`, `internal`). The move of the target, the change's state
and the event are recorded as one step, journalled first: after a crash
the target is at its old head with the change open, or at the new head
with the land recorded once. Attempts running at a stop are run again.
After a land the source branch is deleted (setting `land.delete_source`),
the dependants are restacked and the other changes into the target are
re-checked. Nothing is pushed to origin.

A **conflict report** has one shape wherever a rebase can fail: the commit
replayed onto, the first commit that did not apply, each conflicted file
with its kind and three sides, and the **landed changes** that touched
those files, so an agent knows whose work it met.
`GET …/changes/{number}/conflict` recomputes it, and the conflict is
resolved by a new version or a restack with `resolutions`.

### Why no merge queue, and what it costs

Nothing is held across an attempt and attempts are never ordered. Two
changes that touch different things land at the same moment; they meet
only at the compare-and-swap, and the loser pays one more rebase.
`[land].workers` bounds how many rebases run at once, not their order.

The price: checks are reported on a version, not on the rebased result
that lands, so two changes that each pass alone can break together.
Testing every rebased result first is exactly what would serialize
landing. Parley keeps the damage small and visible instead: every land is
an event, every landed commit maps to its change and session, and a revert
is one call. A workspace that needs more for some paths sets
`fresh_checks` on their rule and accepts the wait.

### Stacks, auto-land and revert

- With `stack: true`, an attempt covers the change and every unlanded
  change below it, replayed bottom first; the target moves once, all or
  nothing.
- A change with `auto_land` is landed by the hub when it becomes ready and
  clean, with the permission of the user who set the flag, checked at that
  moment. A failed attempt is retried with backoff; a conflict or block
  waits for the change to become ready again.
- `POST …/changes/{number}/revert` on a landed change creates a branch
  `revert/<number>` that undoes its commits and opens an ordinary change
  from it.

Landing needs write access to the target workspace or, while the setting
`land.by_author` is on, being the change's author, which lets an
organization member land their own work from a fork.

### Policy, approvals and checks

Reviewing every line an agent writes does not scale; reviewing nothing is
not acceptable for the sensitive parts of a codebase. A workspace's
**policy** says, by path, what a change needs. It is one document of
ordered rules, replaced as a whole by the owner and numbered by revision
(`If-Match` guards against overwriting someone else's edit).

```json
{
  "default": {"approvals": 0, "checks": ["test"]},
  "rules": [
    {"name": "billing", "paths": ["billing/**"], "targets": ["main"],
     "require": {"approvals": 1, "approvers": ["owner", "user:dana"],
                 "checks": ["test", "billing-e2e"]}},
    {"name": "secrets", "paths": ["deploy/secrets/**"],
     "require": {"block": true}}
  ]
}
```

For each file a change touches, the first rule whose paths and target
branches match is selected, else `default`. Each selected rule is one
**requirement**: so many counted approvals, these checks passed, or a
block. The result is stored on the change as `landing`, with status
`ready`, `pending` or `blocked`, and re-evaluated on every version,
verdict, check report and policy update. `POST …/policy/evaluate` previews
what a set of paths or a change would need, without writing anything.

Approvals and checks attach to a **version**, so evidence refers to exact
commits. An **approval** is a user's verdict on a version. It counts when
the user is among the rule's approvers and the verdict was not given with
a credential that created any version of the change: a person approving
their agent's work counts, an agent approving its own push does not. A
**check** is a named result reported by an external runner; the hub runs
no builds. Both carry over to a patch-equivalent version, so a rebase or
restack does not discard review.

A blocked land says what is missing: the attempt's `blocked` member lists
each requirement with its rule, the paths it was selected for, approvals
required and counted, and each check's status. The owner or the admin
token may land past unmet approvals and checks with an `override` and a
reason, recorded on the attempt, but never past a block; an agent's bound
token can never override.

Reference: [api.md](api.md#landing) and
[api.md](api.md#landing-policy-approvals-and-checks).

## Awareness

Optimistic landing makes conflicts cheap to detect, not cheap to have: two
agents can rewrite the same file for an hour, and one finds out at
landing. Awareness makes in-flight work visible early. Before it starts,
an agent asks who else touches the paths it is about to edit; while it
works, it is told when another change begins to overlap its own.

### What the hub knows and what you can ask

The hub keeps an index of which files each draft and open change touches.
A background worker builds it from the event log, about a second behind,
so a push is never slowed by it and a restart resumes where it stopped.
Two changes **overlap** when they have the same target branch and a file
in common; changes of one stack do not overlap each other. Overlap is by
path, not by line, and errs towards reporting.

- `GET …/activity?paths=<glob>` answers "what touches these paths": the
  in-flight changes with a matching file, the live claims on matching
  paths and the changes that landed there recently.
- `GET …/changes/{number}/overlaps` lists the changes that overlap one
  change, most shared files first.
- When a new version makes two changes overlap that did not before, each
  gets one `parley.change.overlap` event naming the other. An agent
  subscribes to these for its session (see
  [Events and event webhooks](#events-and-event-webhooks)).

### Claims

A change shows work only once there are commits. A **claim** covers the
time before: a session states that it intends to work on some path globs,
and why. Creating one returns the current activity on those paths, so a
single call both announces and checks.

Claims are **advisory**: they never refuse a push, a commit or a land, and
two sessions may claim the same paths. A lock would let a stalled agent
block everyone and bring back the waiting that optimistic landing removes,
so the hub shows what it knows and leaves the decision (wait, coordinate,
stack on the other change, carry on) to the agent. Claims **expire** for
the same reason: an agent that dies releases nothing. A claim lives
`[awareness].default_claim_ttl` (one hour) unless it asks otherwise, at
most `[awareness].max_claim_ttl`, and can be renewed or released. When its
session opens a change on the same target, the claim is linked to that
change and ends, `fulfilled`, when the change lands or is abandoned. A
workspace holds at most `[awareness].max_claims_per_workspace` active
claims; beyond that the answer is `429`.

Reference: [api.md](api.md#awareness).

## Carry-patch workspaces

Some teams maintain their own variant of a repository they do not control:
an upstream project plus local patches. Every time upstream moves, someone
has to rebase the patches, fix the same conflicts as last time and notice
which patches upstream has merged. A carry-patch workspace gives that job
to the hub.

### The model

```text
upstream (upstream_url) ── fetched by sync and by every rebuild
   ▼
upstream base ─▶ + patch 1 ─▶ + patch 2 ─▶ … ─▶ integration branch
                 patch branches, in position order
```

A workspace created with `workspace_mode: carry_patch` follows a second
remote, the **upstream**, beside its origin. It keeps an ordered list of
**patches**, each a branch with a position and a status (`active`,
`conflict`, `disabled`, `merged_upstream`, `deleted`), and an
**integration branch** (`deploy` by default) that always equals the
upstream base plus every applicable patch, in order. The integration
branch belongs to the hub: no client can write it and no change can target
it. Patches are worked on like any code: a change, from the workspace or a
fork, targets a patch branch and lands under policy.

### The rebuild

A **rebuild** recomputes the integration branch. It is a durable job (see
[Background work](#background-work)): recorded before the request returns,
kept across restarts, retried with growing delays on transient failures
and parked in `dead_letter`, from where it can be requeued, when a retry
cannot help. One rebuild per workspace is pending at a time.

A run fetches upstream, notes the tip of every patch branch, and builds
the new head **away from every ref a client can see**, applying the
patches in order by replaying their commits (`rebase`) or merging their
tips (`merge`). Only at the end does it **publish**, with one
compare-and-swap of the integration branch. Readers see the old branch or
the new one, never a half-built one, and an interrupted run has changed
nothing and starts again. Pushes, lands and syncs go on meanwhile: when
the run ends it compares the tips it used with the present and queues one
follow-up if they moved.

A rebuild is triggered by `POST …/rebuild`; by a patch branch moving
through a push, an API write or a land (setting
`auto_rebuild_after_push`); and by a sync that saw upstream advance, a
patch branch move or a patch merged upstream (setting
`auto_rebuild_after_sync`). Sync detects merged patches, squash merges
included; the next rebuild skips them and removes them when it publishes.

### Conflicts, preview and rollback

When a patch does not apply, the hub first replays the **remembered
resolutions**: it records how each conflict was resolved and applies the
same resolution when the same conflict returns, as it usually does at the
next upstream release. If a conflict is left, the patch becomes `conflict`
with a conflict report. Under `fail_fast` (the default fail mode) the run
stops and publishes nothing, and the job goes to `dead_letter`; under
`continue` the run publishes without that patch. Fix the patch branch, by
a push or a landed change, and the rebuild this triggers tries it again
and learns the resolution. Resolutions are listed and forgotten under
`…/rerere`.

`GET …/rebuild-preview` predicts a rebuild patch by patch without fetching
or changing anything; it ignores remembered resolutions, so it can report
a conflict the real run resolves. `POST …/rebuilds/{id}/rollback` moves
the integration branch back to the head a completed rebuild replaced and
touches nothing else. The next rebuild that publishes replaces it, so turn
the triggers off or disable the patch at fault first.

### How pushes are controlled

After the ordinary push rules, writes to a carry-patch workspace pass one
more, on every surface (git, the API and landing):

| Ref | Settings | Result |
|---|---|---|
| Integration branch | any | refused |
| Patch branch | `patch_branch_source: hub` (default) | allowed |
| Patch branch | `origin`, `push_patches_to_origin` off | refused: push to origin instead |
| Patch branch | `origin`, `push_patches_to_origin` on | forwarded to origin first, allowed only if origin accepts |

`patch_branch_source` names the authority for patch branches. With `hub`
they are written on the hub, and `push_patches_to_origin` mirrors them to
origin afterwards. With `origin` the hub follows them at sync, and a tip
it replaces stays fetchable under `refs/parley/replaced/`. If the hub
cannot decide, the write is refused; push control never lets one through
unchecked.

Reference: [api.md](api.md#carry-patch-workspaces).

## Secrets and variables

The hub keeps named values for three kinds of owner: a user, an organization
and a workspace. There are two kinds because they answer two different
needs. A **secret** is something the hub must use but nobody should read
again: a git token, a model API key, a webhook secret. It is write-only: no
route, event, log line or error message returns its value. A **variable** is
configuration an agent has to read: a model name, a base URL, a feature
switch of your own. Its value is stored in the clear and is readable by
everyone who can read its owner. Variables never change how the hub behaves.

### Levels and resolution

Both kinds exist at three levels, with the same key rules (letters, digits
and `_`, matched without regard to case, at most 100 entries per owner):

| Level | Who manages it |
|---|---|
| user | the user, with any credential of their own |
| organization | the members of the organization |
| workspace | secrets: the owner only; variables: the owner writes, members read |

A task fork is a workspace and has a level of its own. When the hub needs a
value for a workspace, it walks a chain and takes the first level that has
the key:

1. the workspace itself;
2. for a task fork, its parent workspace;
3. the organization of the workspace, while the owner is a member of it;
4. the user who owns the workspace (for a fork: who created it).

The chain depends on the workspace and its owner, never on the caller.
Variables are merged along it: you read the effective set of a workspace,
each entry with the level it came from. Secrets are resolved along it by
name when a sandbox is created or started (see [Sandboxes](#sandboxes)),
with two restrictions that keep a parent's secrets away from its forks. The
parent step is skipped unless the fork's owner may write to the parent, and
the names the hub reads for its own credentials are never taken from a
parent. Resolving is use, not read access: a fork owner who can only read
the parent gets the parent's variables and nothing of its secrets.

### Where the hub itself uses secrets

The hub reads a few names itself, and only from the workspace level of the
workspace concerned:

| Name | Used as |
|---|---|
| `GIT_PAT`, or `GIT_USERNAME` with `GIT_PASSWORD` | the credential for the workspace's origin (clone, sync, push) and its forge API token |
| `UPSTREAM_GIT_PAT`, or `UPSTREAM_GIT_USERNAME` with `UPSTREAM_GIT_PASSWORD` | the upstream credential of a carry-patch workspace; it falls back to the origin's |
| `FORGE_WEBHOOK_SECRET` | the secret that verifies the forge's webhook deliveries |

A credential is resolved on every use and never cached, so a rotated secret
applies to the next operation. A secret that cannot be read fails the
operation; it is never taken for "no credential". Every value the hub reads
is also registered for redaction, so it is removed from any log line, error
or event text it might reach.

### Encryption and the secret keys

Secret values are stored only under authenticated encryption (AES-256-GCM)
with a key you supply, so a copy of the hub's storage, a backup included,
discloses no secret. The hub takes up to two keys: the **current** key,
which encrypts every new and updated value, and an optional **previous**
key, which only decrypts. Each stored value records the fingerprint of the
key that encrypted it. The signing secrets of
[event webhooks](#events-and-event-webhooks) are encrypted with the same
keys.

The keys are loaded before any store is opened
([ADR 06](adr/06-load-the-secret-keys-before-the-stores-open.md)). For you
as the operator this means:

- **A malformed key or an unreadable key file stops the start** with the
  one `cannot start` line, naming the variable or the file and never the
  value. Nothing has been opened or created by then. `backup` and the
  bootstrap refuse the same way.
- **Without a current key the hub runs**, but every request that would
  store a secret value is answered `503` `secrets_unavailable`: creating
  and updating secrets, creating a workspace with git credentials, setting
  a forge webhook secret, creating an event webhook or rotating its secret.
  Listing and deleting secrets, and all variable routes, keep working.
- **Each start reports what it found** in the log line `secret encryption`:
  whether a current and a previous key are configured, and how many stored
  values neither key opens.
- **To rotate**, make the new key the current one and the old key the
  previous one, and restart. Every maintenance pass then re-encrypts the
  values still under the previous key and logs `secret re-encryption` with
  the numbers converted and remaining. Once nothing remains, remove the
  previous key.
- **A lost key cannot be replaced.** A value that no configured key opens
  stays listed, can be overwritten or deleted, and fails every operation
  that needs it with an error naming the key. Keep the key with the same
  care as the backups, and apart from them.

The key format and where the keys are read from are in
[the configuration reference](configuration.md#secret-keys); the routes are
in [the API reference](api.md#secrets-and-variables).

## Forge integration

A workspace's repository usually lives on GitHub or GitLab, and the issues
and pull requests an agent works from live there too. The hub is the forge
client of each workspace: callers read and write issues, comments, labels
and pull requests through the hub. There are two reasons for putting the
hub in between.

- **The credential stays in the hub.** An agent holds a Parley token with
  `forge:read` or `forge:write` and never a forge token. What it may do is
  decided by its access to the workspace, and you revoke it in one place.
- **One shape for two forges.** An issue, a comment, a pull request, a
  check run, a review and a label look the same whichever forge answers. A
  GitLab merge request is a pull request, its project-level number is the
  number, and the jobs of its latest pipeline are its check runs.

The forge stays independent of Parley's own changes. A pull request opened
through the hub is an ordinary forge pull request, and nothing the forge
reports opens, approves or lands a change.

### How a call is made

The hub derives the repository from the workspace's remote URL and the
forge kind from the host name, or from the workspace's forge API URL
setting, which is how a self-hosted forge under another name is made known.
The token is the workspace's git credential, read from its secrets on each
call (see [Secrets and variables](#secrets-and-variables)). The hub never
takes a forge token from its own environment.

- `target=upstream` selects the upstream repository of a carry-patch
  workspace, with the upstream credential.
- A task fork has no forge of its own: a call with a fork's slug acts on
  the parent's repository with the parent's credential, and the caller
  needs the matching access on the parent.
- A read without a stored credential is sent anonymously. A write without
  one is refused before anything is sent.
- Each request to the forge has a 30-second deadline, follows redirects
  only on the same host, and is retried once when the forge states a
  rate-limit wait that fits the deadline. A failure comes back as one of a
  small set of `forge_*` error types with the token removed from the text.
- A write is not abandoned when the caller disconnects. A write that
  reached the forge leaves exactly one `parley.forge.*` event, whether it
  succeeded or failed, so the audit log shows every attempt. Reads leave
  none.

### Webhooks from the forge

To learn what happens on the forge, a workspace takes its webhook
deliveries. You set a webhook secret on the workspace (the hub can generate
one and shows it once), and enter the URL `/hooks/forge/<slug>` and the
secret in the forge's settings yourself: the hub never registers a webhook
on the forge.

```text
forge ──POST /hooks/forge/<slug>──▶ verify ──▶ record + events ──▶ 202
                                      │         (one transaction)
                                      └─ 401, nothing recorded
```

The delivery route takes no Parley credential. The delivery authenticates
itself: GitHub signs the body with HMAC-SHA256 under the secret, GitLab
sends the secret as a token, and the hub compares in constant time. An
unknown workspace, a fork and a workspace without a secret are answered the
same `404`.

A verified delivery is reduced to what Parley relays (issues, comments,
pull requests and pushes of the origin or upstream repository) and written
to the workspace's **delivery log** together with the `parley.forge.*`
events it produces. The outcome is `relayed`, `ignored`,
`repository_mismatch` or `malformed`. The log holds no payload and no
secret, and is kept for 7 days.

The delivery is logged before the forge gets its answer, in the same
transaction as its events, for three reasons. A delivery recorded as
`relayed` always has its events, even when the forge closed the connection
early. The forge's delivery id is unique per workspace, so a redelivery, or
two copies racing, is recorded once and the others are answered
`duplicate`. And when a webhook seems silent, the log tells you whether the
forge delivered at all and what the hub made of it.

From there the forge's events are ordinary hub events: consumers read them
like any other (see [Events and event webhooks](#events-and-event-webhooks)).
The hub itself never acts on them.

The routes, the error types and the event mapping are in
[the API reference](api.md#forge).

## Sandboxes

A sandbox is where an agent runs. It is born attached to a workspace,
normally a task fork: it starts with the fork checked out, reaches the hub
and nothing else unless you allowed more, holds no real credential unless
you exposed one on purpose, and has its work saved back to the fork before
it goes away.

### Who does what

Parley does not implement isolation. Sandboxes run on OpenShell, an
open-source sandbox runtime; the hub is the control plane on top of one
OpenShell gateway, and callers need only the hub's API.

```text
   agent, person, CI ── REST, MCP, WebSocket; one Parley credential
            │
            ▼
 ┌─ hub: control plane ─────────────────────────────────────────────┐
 │ who may have a sandbox, of which profile; quotas and lifetimes   │
 │ secrets → credentials; the hub credential; checkout, checkpoints │
 │ approval of network access; records, logs, events                │
 └──────────┬───────────────────────────────────────────────────────┘
            │ gateway API (one OpenShell workspace, owned by the hub)
            ▼
 ┌─ OpenShell: runtime ─────────────────────────────────────────────┐
 │ isolates filesystem, processes and network; enforces the policy  │
 │  ┌─ sandbox ────────────────┐                                    │
 │  │ /workspace: the checkout │── egress ──▶ real credentials are  │
 │  │ agent, exec, tools;      │             added here, outside    │
 │  │ sees placeholders only   │             the sandbox; anything  │
 │  └──────────────────────────┘             not allowed is denied  │
 └──────────────────────────────────────────────────────────────────┘
```

The feature is optional. **Without a `[sandbox]` section** the hub contacts
no gateway, runs no sandbox work, and answers every sandbox route `404`
`sandboxes_disabled`. With one, it checks the gateway once at start
(authentication, a supported release, every profile's credentials and
policy file) and refuses to start on a failure, naming the key.

A **profile** is your template for a sandbox, defined in the configuration:
image and resources, lifetime and idle timeout, base network policy,
credentials, exposed secrets, access to the hub, checkpointing, proposals
and the tool server. Callers pick a profile by name and cannot change what
it grants. A sandbox keeps a snapshot of its profile.

### Creating and provisioning

A create request is judged completely before anything exists: write access
to the workspace, its state, the session it names, every secret the profile
needs, and four quotas (hub, organization, workspace family, user). The
sandbox is stored as `provisioning` and the request answers. A fork and its
sandbox can be created in one call; if the sandbox is refused, neither
exists. In the background the hub registers the credentials with the
runtime, creates the sandbox there with its policy, clones the repository
into `/workspace` on a branch for the work, sets git up to push to the hub
as the creating user, restores a checkpoint if `restore_from` names one,
starts the main process if there is a command, and marks the sandbox
`ready`. A failure ends in `failed` with a reason, and what was created at
the runtime is removed. A push from the sandbox opens or updates a change
like any other push, with the sandbox's session attached.

### Credentials

By default a sandbox holds no real credential.

- **Configured credentials** connect a secret, resolved along the chain of
  [Secrets and variables](#secrets-and-variables) at creation and at every
  start, to an OpenShell provider profile. The workload sees a placeholder
  in an environment variable. The runtime puts the real value in its place
  outside the sandbox, only on requests to the endpoints that provider
  profile authorizes, so code in the sandbox can neither read the value
  nor send it elsewhere.
- **The hub credential** is a token the hub mints for the sandbox, bound to
  its workspace and owned by the creating user, with no more scopes or
  paths than the creating credential. It is a placeholder too, and is
  revoked at stop and deletion. A profile can leave it out.
- **Exposed secrets** are for tools that cannot work with a placeholder
  because they sign requests themselves or check their key's format. A
  profile can list secret names whose real values are placed in the
  sandbox's environment. This is opt-in, and only a profile can do it,
  because it gives up the protection above: code in the sandbox can read
  the value, send it wherever the network policy allows, and write it into
  a file that a checkpoint or a commit then carries. The hub redacts such
  values in the logs and texts it stores, not in exec output, the
  terminal, downloaded files or checkpoints.

Running a command, attaching or uploading a file needs a credential that
covers the sandbox's hub credential, so a weak token cannot act through a
sandbox a stronger one created.

### The life of a sandbox

| State | Meaning |
|---|---|
| `provisioning` | being created or started, or the runtime is restarting it |
| `ready` | running; exec, files, attach and tools are available |
| `stopped` | compute released, working directory kept |
| `completed`, `failed` | the main process exited with 0; or it exited otherwise, provisioning failed or the runtime lost the sandbox |
| `deleted` | gone; the record stays readable for 24 hours |

Every transition is conditional on the state it expects and writes one
event, so racing requests and the hub's timers produce one transition.

- **Idle time.** A `ready` sandbox without activity (exec, attach, file
  transfer, tool call, output of the main process) for its idle timeout is
  stopped.
- **Maximum time.** At `expires_at` (at most the profile's `ttl` and, on a
  fork, the fork's expiry) a sandbox is deleted whatever its state.
- **Stop and start.** A stop takes a checkpoint, revokes the hub credential
  and releases the compute. A start is judged like a create, reads the
  secrets again, mints a new hub credential, and rebuilds the working
  directory from the last checkpoint when the runtime no longer has it.
- **Delete.** A deletion takes a checkpoint and removes the sandbox and its
  credentials at the runtime. Deleting a workspace or expiring a fork
  deletes its sandboxes first, without a checkpoint; archiving stops them.

**A checkpoint** saves the git state of the working directory into the
sandbox's workspace on the hub: every local branch with commits the hub
does not have, and the uncommitted changes of the checked-out branch as one
extra commit, under the hub-owned refs
`refs/parley/sandboxes/<id>/<branch>`. They can be fetched, are not
versions of a change and trigger nothing; files that git ignores are not
saved. A checkpoint is taken at stop, at deletion, when the main process
ends and on request. It is best effort and never blocks a stop or deletion.

**When the hub restarts**, no sandbox ends: the runtime owns them. Only
open exec, attach, log and transfer requests end, and an interrupted
provisioning is taken up again. At start and every `reconcile_interval` the
hub compares its records with the runtime: it records main processes that
exited meanwhile, marks as `failed` the sandboxes the runtime no longer
knows, removes what the runtime holds for no live record, and does the
idle stops and expiries that became overdue.

**While the gateway is unreachable**, the hub keeps running. Reads answer
from the stored state with `runtime_reachable: false`, and operations that
need the runtime answer `502` `sandbox_runtime_error`. Expiry and deletion
still apply on the hub's side; the remains at the runtime are removed once
the gateway is back.

### Using a sandbox

- **Exec** runs a command in the working directory, next to the main
  process, and answers its collected output or streams it. The command is
  killed at its timeout or when the caller leaves.
- **Files** go in and out as single files or tar streams, confined to the
  working directory.
- **Logs** are the main process's output and the runtime's policy log,
  stored by the hub with secret values redacted, read in pages or followed.
- **Attach** is an interactive terminal over a WebSocket, meant for people.
- **Tools.** A profile can name a tool server, a program in its image, so
  that an agent outside the sandbox need not build file reading, searching
  and editing out of exec. The hub serves the sandbox's tools as REST
  routes and as an MCP endpoint that needs only the URL and a Parley
  credential. Each call is one process in the sandbox. A caller who may
  exec gets every tool; one who may only read the workspace gets the tools
  that change nothing.

A tool call writes one hub event, without its arguments or result. Exec,
attach and transfers write none; when the sandbox belongs to a session with
a run id, the hub records them as run events of that run, without input,
output or content.

### The network policy

A sandbox's network is **denied by default**. Its policy is the profile's
base policy (the built-in one allows no egress), a rule for the hub's own
API and git endpoints, the rules of each attached credential, and the
workspace sandbox rules below. A profile cannot narrow the hub rule or the
credential rules. Parley defines no policy language: policy files and rules
are OpenShell's, and the hub passes them on.

- **Denials.** The first connection the runtime blocks for each program,
  host and port becomes an event; all of them are in the sandbox's logs.
- **Proposals.** A blocked workload can ask for a new rule through the
  runtime's own mechanism. The hub lists it with the runtime's risk
  findings and announces it with an event.
- **Approval by a person.** Approving or rejecting needs write access to
  the workspace and a credential that is not a workspace-bound token, so
  an agent cannot approve its own request. A finding of high severity must
  be acknowledged. An approved rule lasts for the sandbox's life.
- **Workspace sandbox rules.** An approval with `scope: workspace` also
  keeps the rule for the workspace (for a fork's sandbox: for its parent).
  It is added to every sandbox created later on that workspace or its
  forks, so the next agent does not ask again. Rules never reach sandboxes
  that already exist, a workspace keeps at most 50, and you can switch the
  promotion off for the whole hub.

**What the current OpenShell client carries.** The hub never imitates an
operation the runtime lacks: the request fails, so that no sandbox runs
without what it was asked for. With the OpenShell client release the hub is
built on (0.1.9), a real gateway cannot take a base policy file, start a
main process, restart a stopped sandbox with its policy and environment,
serve the process and policy logs, download files, or run a command with
standard input, which checkpoints need. Against such a gateway, use
profiles without a `policy` file and without a command, and drive sandboxes
through exec, file upload, attach and the tool routes.

The configuration keys are in
[the configuration reference](configuration.md#sandboxes) and the routes in
[the API reference](api.md#sandboxes).

## Sessions, telemetry and audit

Many agents working at once produce forks, pushes, changes and lands that
nobody watched happen. These records answer the questions that come
afterwards: which piece of agent work produced this change, what did it
cost, what did the agent do, and who did what on the hub.

### Sessions

A **session** is the thread that ties one piece of agent work together. An
agent opens it for a workspace (which may be a task fork), with optional
labels such as a run id and a model, and closes it with an outcome.

```text
(none) ──open──▶ active ──complete──▶ completed | failed | timeout
                   ├─ workspace archived or deleted, fork expired ─▶ terminated
                   └─ active longer than [sessions].max_active_age ─▶ timeout
```

Work is linked by naming the session where the work is done: when creating
a fork or a sandbox, on a git push (`-o parley.session=<id>`), on a commit
through the source control API, on a claim and on a land. The hub accepts
the reference when the session exists, was created by the caller's user,
and belongs to the same workspace or to its fork or parent. It does not
require the session to be active, so a land that happens after the agent
finished is still attributed. Every hub event such an operation causes
carries the session id.

Reading a session gives you the whole thread: the forks it created, the
changes it has versions on, its sandboxes, and counts of pushes, commits,
versions, claims and lands. A session outlives its workspace: one opened on
a fork stays readable by the parent's readers after the fork is gone.

### Usage and cost

While a session is active the agent reports **usage records**: a model and
the input, output and cache-read token counts of one step. A record is
stored once per id, so a retried report changes nothing. The session shows
the totals, and the workspace's cost report sums the records over a time
window per day, session or model. "Cost" means token counts: the hub has no
price table.

### Telemetry

Sessions say that work happened; **run telemetry** says what the agent did.
Under a run id an agent pushes run events, session outcomes, tool calls,
tool errors, conversation traces and one postmortem, singly or in batches.
The hub serves them back per run, including a transcript rebuilt from the
traces. Every record is idempotent on its id, so an agent can retry safely.

Telemetry is kept apart from the hub's own state. It lives in a separate
store and is never part of the event log, and a write that cannot finish in
time is answered `503` with `Retry-After`: a burst of telemetry slows the
reporting agents, not pushes or lands.

### The audit query

The audit query is the one place to ask "who did what": it merges the hub's
events and the agents' run events, newest first, with filters for
workspace, type, session and time. You see the hub events the visibility
rule of [Events and event webhooks](#events-and-event-webhooks) grants you
and the run events of the workspaces you can read; the admin token sees
everything, also of workspaces that no longer exist. It reaches further
back than the event log: `[audit].max_age` (90 days by default) against
`[events].retention` (30 days).

### Retention

Nothing here grows without bound. At start and with every maintenance pass
a retention run times out sessions left active too long and deletes ended
ones after `[sessions].max_age`; deletes run records by age and beyond
`[audit].max_runs` runs per workspace; moves the start of the event log
forward by `[events].retention` and deletes hub events after
`[audit].max_age`, never one the event log still addresses; deletes old
webhook delivery records; and deletes what belongs to workspaces that are
gone after `[audit].orphan_retention`. A step that fails is logged and
counted, and the others still run.

### Metrics

`GET /metrics` exposes the hub's operational state in the Prometheus text
format. The metrics are for watching and alerting on the hub, not for
accounting: usage and audit questions go to the routes above. Every metric
has a series from the start, so a dashboard never meets an absent one.

| Metrics | What they tell you |
|---|---|
| `parley_http_*`, `parley_git_requests_total`, `parley_scm_requests_total` | request volume, status and latency |
| `parley_changes_open`, `parley_forks_active`, `parley_lands_total`, `parley_land_*` | how much work is open, and how landing behaves under contention |
| `parley_events_seq`, `parley_sse_connections`, `parley_webhook_*` | event flow: last position, open streams, deliveries made and waiting |
| `parley_agent_*`, `parley_audit_*` | active sessions, reported tokens, stored records |
| `parley_sandbox*` | sandboxes by state, creations, exec and tool calls, denials, checkpoints, whether the gateway is reachable |
| `parley_code_index_*`, `parley_jobqueue_depth` | background work: index builds and queued jobs |
| `parley_retention_*` | failed retention steps and the time of the last clean run |

The routes are in [Sessions and telemetry](api.md#sessions-and-telemetry),
[Audit and events](api.md#audit-and-events) and
[Health, version and metrics](api.md#health-version-and-metrics); the
retention keys in [the configuration reference](configuration.md#retention).

## Events and event webhooks

Automation around Parley (a CI system, a chat bot, another agent) needs to
know what happened without rescanning state, and must miss nothing when it
disconnects or the hub restarts. The hub therefore records every
significant state change as one **event** in one durable, ordered log.
Everything that reports what happened reads that same log: the event
stream, the pollable log, event webhooks and the audit query. There is no
second copy that could disagree with it.

### One log

Every event has the same shape: a hub-wide sequence number `seq`, an `id`,
a `type` such as `parley.change.land`, the time, the workspace, the actor,
the resource, the session it is attributed to, and metadata specific to the
type, which never holds a credential or a secret value.

**An event is written in the same transaction as the state change it
describes.** Both are committed or neither is: there is no change without
its event and no event for a change that was rolled back. Where the state
lives in a git repository (a ref update), the hub writes the event it still
owes at the next start, before it serves requests. `seq` is assigned inside
that transaction, and the store commits one write at a time, so the order
of `seq` is the order in which things happened. It is never reused and
never goes back, across restarts too.

The events are served through two windows. The **event log** is the part
addressable by `seq`, `[events].retention` long. The **audit log** is every
event up to `[audit].max_age`, read newest first by the
[audit query](#the-audit-query).

### Why delivery reads the log by position

The hub does not push an event to consumers when it is written: at that
moment the transaction is still open and may roll back. Instead every
consumer holds a **position**, the last `seq` it has processed, and reads
the log after it; the hub only tells waiting consumers when to read again
([ADR 05](adr/05-deliver-events-by-reading-the-log-by-position.md)). Every
consumer gets the same guarantees from this:

- **Order.** Events arrive in `seq` order, and none appears after one with
  a higher `seq` has been delivered.
- **Resume.** After a disconnect you continue from your last `seq` with the
  same read you use live, with no gap and no repeat between replay and
  live delivery.
- **No loss across restarts.** Nothing is held only in memory: the log
  carries the events, and a missed wake-up only delays one, by at most a
  second. Across a reconnect you may see an event twice; `seq` and `id`
  identify duplicates.
- **Explicit expiry.** A position older than the log is refused with `410`
  `cursor_expired` and the oldest `seq` still held. The hub never skips
  silently.

### Who sees which event

One rule decides visibility, applied when an event is delivered, not when a
stream was opened, so access gained or lost takes effect at once. An event
of a workspace is visible to those who can read the workspace and, for a
task fork, to the readers of its parent, also after the fork is gone.
Events about secrets, bound tokens and event webhooks are visible to the
workspace's owner and administrators only. An event without a workspace
belongs to a user or an organization and is visible to them. A
workspace-bound token sees the events of the workspaces it covers, and one
bound to a fork also the parent's events about changes that come from that
fork. The admin token sees everything. Because you receive only what you
may see and asked for, the `seq` values you receive have gaps; a gap is not
a loss.

### Three ways to consume

| Way | For | Resumes from |
|---|---|---|
| The stream, `GET /api/v1/events` | a consumer that holds a connection open (server-sent events) | `Last-Event-ID` or `after` |
| The log, `GET /api/v1/events/log` | a simple loop: JSON pages, with a long-poll | `after` |
| Event webhooks | a receiver that can neither hold a connection nor poll | the position the hub keeps |

The stream and the log take the same filters (workspace, types, session).
The stream's heartbeat carries the position examined so far, so a consumer
whose filter rarely matches still advances. A stream whose client reads too
slowly, or whose credential stops being valid, is closed, never thinned
out: the client reconnects and misses nothing.

### Event webhooks

An event webhook is a subscription of a workspace: the hub sends each
matching event of the workspace, and of its forks if you want, to your URL
as one HTTP `POST` whose body is the event as the log holds it. Only the
owner or an administrator manages a workspace's subscriptions, and a
subscription receives what the owner could see.

- **Signing.** The hub generates a signing secret and shows it once, at
  creation and at rotation. Each request carries an HMAC-SHA256 signature
  over a timestamp and the body, and a delivery id that stays the same on
  every retry, so the receiver can verify the sender and drop duplicates.
- **Order per subscription.** Each subscription has its own position in the
  log and sends one event at a time: the next goes out only when the
  current one succeeded or was given up. Subscriptions do not wait for
  each other. The position and the pending delivery are stored, so an
  attempt in flight when the hub stops is made again after the start.
- **Retries.** Anything but a `2xx` answer within the timeout is a failure.
  The hub retries after 10 seconds, doubling up to an hour between
  attempts, for 24 hours. Then it gives that event up, marks the
  subscription `failing` and goes on with the next event.
- **Pausing.** After 72 hours without a success the hub sets the
  subscription `disabled`; you can also switch one off and on yourself. A
  disabled subscription keeps nothing: re-enabled, it starts with the next
  event, and you read what you missed from the event log.
- **Redelivery.** The hub keeps a record of each delivery for 7 days,
  without payload. You can send a test delivery and redeliver an ended one
  while its event is still in the log; both go out at once, outside the
  order.
- **Safe destinations.** A URL must be `https`, and one that resolves to a
  loopback, private, link-local or cloud-metadata address is refused, at
  creation and at every attempt, unless `[webhooks]` allows it.

The stream and the log are in [Audit and events](api.md#audit-and-events),
the subscription routes in [Event webhooks](api.md#event-webhooks).

## Background work

Much of what the hub does takes longer than a request should: cloning an
origin, rebuilding a patch stack, landing a queue of changes, delivering
webhooks. This work runs outside requests, in three forms. Which form a
piece of work uses decides what happens to it when the hub stops.

### Long-running workers

Each feature with ongoing work runs a worker for the life of the process:
clones, the land queue and auto-land, mergeability checks and restacks, the
awareness index and claim expiry, the fork sweep, event and webhook
delivery, the code index, and the sandbox provisioner and collectors.

A worker keeps no state of its own. It reads what there is to do from the
state store, so a restart loses nothing: the worker starts again and finds
the same work. Event delivery shows the pattern. Consumers read the event
log by position and the worker only tells them when to read again, so a
missed wake-up delays an event and never loses one
([ADR 05](adr/05-deliver-events-by-reading-the-log-by-position.md)).

Cloning works the same way. A new workspace is `pending`; the clone worker
moves it to `cloning` and then to `ready` or `failed`. A failed clone is
recorded and not retried on its own: you ask for a reclone. If the hub
stops during a clone, the partial copy is removed and the workspace stays
`cloning`, and the next start takes it up again.

### The durable job queue

Work that must not be lost and may need retries goes on the job queue:
today, the rebuild of a carry-patch workspace and the builds of the code
index ([ADR 09](adr/09-run-durable-jobs-on-a-queue-the-hub-composes.md)).
"Durable" means:

- A job is recorded in the same transaction as the request that asked for
  it. If the request succeeded, the job exists.
- At most one job of a type is pending per workspace. Asking again while
  one waits or runs returns the existing job instead of queueing a second.
- A job is `queued`, then `running`, then `completed`. An attempt that
  fails is `failed` and retried with a growing delay, from 2 seconds up to
  2 hours. After 20 retries, or on an error that cannot succeed later, it
  is `dead_letter` and waits for you.
- A job that was `running` when the hub stopped or crashed is `queued`
  again at the next start. A handler can therefore run more than once for
  one job and is written to tolerate that.
- Deleting a workspace deletes its jobs and cancels the one that runs.

### The maintenance loop

Cleaning up runs in one loop: a pass right after the start and one every
hour. A pass runs its tasks one after the other, and passes never overlap.
A task that fails is logged and the pass goes on; the task runs again in
the next pass. What a pass does:

| Task | What it cleans up |
|---|---|
| Retention | Ends stale sessions; deletes telemetry, sessions, delivery records and hub events older than the configured limits; moves the start of the event log |
| Secret re-encryption | Seals stored secrets under the current key after a key rotation |
| Claims, forge deliveries, patches | Removes ended claims, old forge delivery records and deleted patches past their keep time |
| Token sweep | Revokes tokens bound to workspaces that no longer exist |
| Fork expiry | Removes task forks past their expiry |
| Sandboxes | Removes old sandbox records and logs |
| Code index | Queues builds for indexes that are behind; removes those of workspaces that are gone |

The loop is also the safety net for notices. A feature such as the code
index is told when a head moves, but treats that as a hint: the pass
compares the index with the repositories and queues what is behind, so a
notice lost in a crash is repaired within the hour
([ADR 11](adr/11-tell-the-code-index-where-a-head-is-written.md)).

Git housekeeping is not a task of the loop. Git repacks a repository when
its own commands decide to, and the hub makes that housekeeping run inside
the command that started it instead of detaching into the background
([ADR 10](adr/10-keep-git-housekeeping-inside-the-command-that-starts-it.md)).
No git process outlives the call that started it, so a stop leaves no
orphan writing to a repository, and the container needs no init process.

### Across a restart

| Work | What happens |
|---|---|
| A request in flight | Gets up to 15 seconds to finish, then its connection is closed |
| A clone in progress | Removed and taken up again at the next start |
| A running job | Queued again at the next start |
| A maintenance task | Cancelled; runs in the first pass after the start |
| Undelivered events and webhooks | Still in the store; delivery continues |
| An open event stream | Closed; the client resumes from its last position |

## Running the hub

You run the hub as one long-lived process with one data directory. The
commands, flags, exit statuses and log lines are in
[Running the hub: `parleyd`](cli.md#running-the-hub-parleyd); the keys are
in [How the hub is configured](configuration.md#how-the-hub-is-configured).
This section explains what the hub does at each stage and why.

### Bootstrap and the admin token

A new hub has no users, so nobody could sign in to administer it. You start
it once with `--admin-email`:

```sh
parleyd --admin-email ops@example.com
```

This stores a hash of a new admin token, writes the token to the file
`admin_token` in the config directory, readable by the owner only, and
exits without serving. Save the token and delete the file: the hub refuses
to start while it exists, so a token cannot be left on disk by accident.
The token never appears on stdout or in a log. `--reset-admin-token`
replaces a lost or leaked token the same way.

### The start and its checks

The hub starts in a fixed order: configuration and secret keys, the stores
and their schema steps, the bootstrap, each feature's start-up work, the
listen address, then the maintenance loop, the workers and serving. It
stops at the first stage that fails, with exactly one error line that names
what to fix (the configuration key, the file, the schema step) and exit
status `1`, without listening. The checks are placed so that a broken hub
fails early and cheaply rather than half-working later:

- **Secret keys** load before the stores open
  ([ADR 06](adr/06-load-the-secret-keys-before-the-stores-open.md)). A
  malformed key stops the start before anything is created or migrated. A
  missing key is not an error: the hub starts, warns, and refuses to store
  secrets.
- **Git** must be version 2.38 or later. The check runs before any other
  start-up work, so nothing ever runs git on a host where it is missing or
  too old.
- **The sandbox runtime**, when sandboxes are on, is contacted once: its
  release, the configured provider profiles and policies. A hub that could
  not create sandboxes does not start pretending it can.

### Readiness and liveness

`GET /healthz` says the process is alive. `GET /readyz` says it can do its
job: it answers `503` while either store does not answer a query. Both need
no credential and are not rate limited. Use the first to restart a hung
process and the second to decide whether to send it traffic.

### Stopping

On `SIGTERM` or `SIGINT` the hub stops accepting connections, tells
long-lived responses to end, and cancels the maintenance loop and the
workers. It waits for all of them at once: up to 15 seconds for requests in
flight, never longer than 30 seconds in total. The stores are closed last,
so nothing that was given time to finish finds its storage gone. Give the
process at least 30 seconds before you kill it. A hub that is killed loses
nothing committed; work in progress is recovered as described in
[Background work](#background-work).

### Backup and restore

```sh
parleyd backup /srv/backups/2026-11-21
```

`backup` writes a snapshot into an empty directory, whether or not the hub
is running on the data. Run it with the hub's environment and working
directory so that it finds the same configuration. It only reads.
The snapshot is consistent without stopping the hub, for two reasons. Each
store is copied in one read, so the copy holds every transaction committed
before it began, each of them whole. Each repository's refs are copied
before its objects, so every copied ref points at an object that is in the
copy; git's lock files are left out, so a restored repository accepts
pushes. The stores and the repositories are copied one after the other and
need not show the same instant: a ref may be an ancestor of where the live
hub had it.

The snapshot holds the two stores and the workspace root, not the
configuration file or the secret keys. Back those up separately: a restored
hub without the keys cannot open its stored secrets. There is no restore
command. Copy the stores and the workspace root into place, add the
configuration and start `parleyd` with no flags. It keeps the source hub's
admin token.

### Upgrades

An upgrade is: stop the old binary, start the new one on the same data. The
stores change through named schema steps. Each step is applied once, in one
transaction together with the record that it was applied, so a step that
fails leaves the store as it was and the start stops, naming the step. A
released step is never renamed, removed or reordered, and each release is
tested against the recorded data of the release before it. Take a backup
before you upgrade.

### Logs

The hub logs JSON to stderr, one object per line, with the time in UTC.
Lines written while a request is handled carry its request id. Every line
is scrubbed before it is written: the hub's own credentials, configured
client secrets, stored secret values and the user part of URLs are replaced
or removed. `[logging].level` sets the level.

### Images and manifests

The **hub image** (`containers/hub`) holds `parleyd`, `parley` and git. It
runs as an unprivileged user with `parleyd` as its entry point, listens on
8080 and keeps the config directory and the data directory under one
volume, `/var/lib/parley`. The config directory is on the volume because
the bootstrap writes the admin token file there. Container arguments are
`parleyd`'s, so the same image runs `backup`. The **sandbox image**
(`containers/sandbox`) is a reference image for [sandboxes](#sandboxes):
git, a shell and `parley`, with a writable `/workspace`.

`deploy/kubernetes/` holds a ConfigMap with `config.toml`, an example
Secret that reaches the container as environment variables, a
PersistentVolumeClaim, a Deployment and a Service. They encode the hub's
assumptions; keep these when you adapt them:

- **One replica, strategy `Recreate`.** An update stops the old hub before
  the new one opens the same data. Never scale the Deployment up.
- **One `ReadWriteOnce` volume** for the configuration directory and the
  data.
- **A startup probe** long enough for a first start that applies every
  schema step, then liveness on `/healthz` and readiness on `/readyz`.
- **A termination grace period of 40 seconds**, above the 30-second stop.

The Deployment runs `parleyd` with no flags. Bootstrapping the admin token
on the volume is a step you do yourself, once; the manifests do not cover
it.

## The client

`parley` is the command-line client for people, scripts and agents. Its
commands are described in [Using `parley`](cli.md#using-parley).

### Why it speaks only the REST API

`parley` does nothing a `curl` call could not do. Every command that talks
to the hub calls REST routes and prints the hub's answer. It shares no code
with the server and never touches the hub's data. This keeps one contract:
whatever the client can do, your own integration can do, with the same
credentials, checks and errors. It also keeps the client a single static
binary that you can drop into a CI image or a sandbox.

For git itself you use stock git against the hub's git URLs. The client
only helps git find the credential.

### Commands declared once

Every command is declared the same way: its words, its description, the
route it calls, its arguments, the credential it needs and its flags
([ADR 02](adr/02-declare-every-cli-command-through-one-helper.md)). Help,
argument checking, output and error handling are all derived from that one
declaration. For you this means:

- **Uniform behaviour.** Results go to stdout as JSON; errors go to stderr
  as one envelope of the same shape as the hub's, with a stable
  `error_type` and exit status. A usage error is found before any setting
  is read or any request is sent.
- **Uniform waiting.** Every command that starts a long operation takes
  the same `--wait`, `--timeout` and `--poll-interval`, and fails the same
  way when the operation fails or the wait times out.
- **Machine-readable help.** `parley help <command> --json` prints the
  declaration: description, method and path, arguments and flags. An agent
  can discover the whole command set without a manual, and the help cannot
  drift from what the command does.

```sh
parley help change land --json
```

### How it finds the hub and the credential

A command needs the hub's URL and a credential. It takes each from the
first source that gives one:

| Setting | Flag | Environment variable | Client configuration file |
|---|---|---|---|
| Hub URL | `--endpoint-url` | `PARLEY_ENDPOINT_URL` | `endpoint_url` |
| Credential | `--api-key` | `PARLEY_API_KEY` | `api_key` |

A person runs `parley login` once. It signs in through the browser, or
checks a credential you pass, and saves the URL and the API key to
`~/.parley/config.toml`, readable by the owner only. An agent or a CI job
gets the two environment variables instead and needs no file. See
[Configuring the client](configuration.md#configuring-the-client).

### The git credential helper

`parley credential-helper` implements git's credential helper protocol.
Configure it once for the hub's URL:

```sh
git config --global credential.https://hub.example.helper \
  '!parley credential-helper'
```

After that, git works against the hub without a credential typed or put
into a remote URL. The helper resolves the credential as every command
does, makes no request, and answers only for the hub's own scheme, host and
port, so it never hands the credential to another host.

### The MCP server mode

`parley mcp` serves the client's commands as tools to an agent harness that
speaks the Model Context Protocol, over standard input and output. It
exists so that an agent can use Parley without shelling out and parsing
text. Each tool is one command, with the command's arguments and flags as
its parameters, so the tool set follows the command set with no second
definition to maintain.

You choose which command groups are served, can restrict a session to
reads, and can fix it on one workspace or sandbox. Commands that need an
admin, print a one-time secret or need a terminal are never served. The
session takes its credential as any command does, so a workspace-bound
token confines it on the hub's side as well. See
[`parley mcp`](cli.md#parley-mcp).

## Code map and decisions

### Packages

All packages are under `internal/` except the two programs.

| Package | Responsible for |
|---|---|
| **Programs** | |
| `cmd/parleyd` | The hub binary: passes the process's inputs to `internal/hub` |
| `cmd/parley` | The client binary: passes them to `internal/cli` |
| **Platform** | |
| `hub` | Composing the process from its features; start, bootstrap, backup, stop, the logger |
| `server` | HTTP: routing, the check pipeline, error envelope, lists, rate limit, idempotency (`server/timefmt`: timestamps and durations) |
| `store` | Opening the two embedded stores, schema steps, serialized write transactions, snapshots |
| `config` | Loading and checking `config.toml` and the environment |
| `maintenance` | The maintenance loop and its task registry |
| `jobs` | The durable job queue |
| `metrics` | The metric catalogue and registry behind `GET /metrics` |
| `scrub` | Removing credentials from text before it is logged or stored |
| `buildinfo` | Version, commit and build time of a binary |
| **Identity and access** | |
| `identity` | Users, orgs, sign-in, the admin token, API keys, PATs, scopes, authentication |
| `access` | The workspace access rule, workspace records for access, bound tokens |
| `secret` | Sealed secrets and variables, the secret keys |
| **Workspaces and git** | |
| `workspace` | Workspace records, settings, clone, sync, archive and delete |
| `fork` | Task forks: creation, tokens, expiry |
| `gitrepo` | Running the git command on the hub's bare repositories (`gitrepo/gitrepotest`: test repositories) |
| `gitserver` | Git over HTTP for stock git clients (`gitserver/pushrule`: the rules every ref write passes) |
| `scm` | The source control API: refs, trees, files, commits, outline, search |
| `codeindex` | The tree and search index of each default branch's head |
| `carrypatch` | Carry-patch workspaces: upstream tracking, patches, rebuilds |
| **Changes and landing** | |
| `change` | Changes, their versions, stacks, mergeability, restacks |
| `land` | Land requests, the land queue, auto-land and reverts |
| `policy` | Landing policy by path, approvals and checks |
| `awareness` | Activity, overlaps and claims: who works on which files |
| `forge` | The client of GitHub or GitLab and the forge webhook ingress (`forge/number`: the issue number rule; `forge/forgetest`: a fake forge) |
| **Agents and observation** | |
| `sandbox` | Sandbox records, provisioning and lifecycle (`sandbox/runtime`: the boundary to the runtime; `runtime/openshell`: the OpenShell implementation; `runtime/runtimetest`, `openshell/openshelltest`: fakes) |
| `session` | Agent sessions, usage and links |
| `telemetry` | What agents report about their runs |
| `event` | The event log, positions and the event stream |
| `audit` | The audit query over the hub's events |
| `webhook` | Event webhooks and their deliveries |
| **Client and tests** | |
| `cli` | Every `parley` command, the client configuration, the credential helper, the MCP server |
| `hubtest` | Starting a real hub in-process for tests |

### Decisions

| ADR | Decision |
|---|---|
| [01](adr/01-compose-the-hub-in-one-package.md) | One package composes the hub from an explicit, ordered list of features |
| [02](adr/02-declare-every-cli-command-through-one-helper.md) | Every CLI command is declared through one helper, which help, checks and output derive from |
| [03](adr/03-register-removers-of-owned-data-on-the-identity-service.md) | Features register what they remove for a user or an org; removal runs in the delete's transaction |
| [04](adr/04-let-a-check-attach-a-value-for-the-handler.md) | A request check may hand what it found to the handler |
| [05](adr/05-deliver-events-by-reading-the-log-by-position.md) | Event consumers read the log by position; notifications only say when to read |
| [06](adr/06-load-the-secret-keys-before-the-stores-open.md) | The secret keys load before the stores open, so a bad key stops the start before anything changes |
| [07](adr/07-register-the-teardown-collaborators-on-the-workspace-service.md) | Features register what must happen when a workspace is archived or deleted |
| [08](adr/08-register-the-push-collaborators-on-the-ref-writer.md) | Everything that takes part in a ref write registers with the one ref writer |
| [09](adr/09-run-durable-jobs-on-a-queue-the-hub-composes.md) | Retryable work runs on one general, durable job queue |
| [10](adr/10-keep-git-housekeeping-inside-the-command-that-starts-it.md) | Git housekeeping runs inside the command that starts it, never detached |
| [11](adr/11-tell-the-code-index-where-a-head-is-written.md) | Each place that moves a head tells the code index; maintenance catches what was missed |

### Tests

Unit and integration tests sit beside the code they test and are organised
by behaviour. Tests that need a running hub start a real one in-process
through `internal/hubtest`, which is the same composition the binary runs.
Beyond that:

| Where | What it proves |
|---|---|
| `test/acceptance` | User scenarios against the built binaries, driven only over HTTP, through `parley` and through stock git |
| `test/e2e` | What only the built binaries prove: build information, sign-in, backup and restore under load, `parley` against a served hub |
| `test/upgrade` | The current binary opens the recorded data of earlier releases, and no released schema step was renamed, removed or reordered |
| `test/perf` | The performance targets, measured as a black-box client against a hub you name |
| `test/image` | The container images build and behave; run only when asked, since they need a container engine |
