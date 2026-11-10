# Parley — Product Requirements Document

Parley — the hub where agents negotiate with your code.

## Contents

1. [Product overview](#1-product-overview)
2. [Personas, glossary and conventions](#2-personas-glossary-and-conventions)
3. [Platform, identity and access](#3-platform-identity-and-access)
4. [Workspaces and task forks](#4-workspaces-and-task-forks)
5. [Git server](#5-git-server)
6. [Source control API](#6-source-control-api)
7. [Changes and stacks](#7-changes-and-stacks)
8. [Landing and policy](#8-landing-and-policy)
9. [Awareness](#9-awareness)
10. [Carry-patch workspaces](#10-carry-patch-workspaces)
11. [Secrets and variables](#11-secrets-and-variables)
12. [Forge integration](#12-forge-integration)
13. [Sandboxes](#13-sandboxes)
14. [Sessions, audit, events and metrics](#14-sessions-audit-events-and-metrics)
15. [Non-functional requirements](#15-non-functional-requirements)
16. [Acceptance criteria](#16-acceptance-criteria)
17. [Sequencing and open decisions](#17-sequencing-and-open-decisions)
- [Appendix A: REST route index](#appendix-a-rest-route-index)
- [Appendix B: CLI command index](#appendix-b-cli-command-index)

---

## 1. Product overview

### 1.1 Problem

When coding agents work on real repositories, the same questions come up every time:

- **Where does the agent's copy of the code live?** And how can many agents work on one repository without getting in each other's way?
- **How do their changes get back to the main branch?** Ideally without a person rebasing and merging every branch by hand, and without every agent waiting in one queue.
- **Who decides what may land?** Reviewing every line an agent writes does not scale. Reviewing nothing is not acceptable for the sensitive parts of a codebase.
- **How does one agent learn what the others are doing?** Two agents editing the same file find out too late.
- **Which credentials does an agent get?** Forge tokens and API keys copied into an agent's environment are hard to scope and harder to revoke.
- **Where does the agent run?** It should be isolated from the machine it was started on.
- **What did the agent do, why, and what did it cost?**

Git hosting was designed for people: a few long-lived clones, branches named by hand, pull requests read line by line, one merge at a time. Agents are different. They are always online, they start from a clean copy for every task, they produce many small changes in parallel, and they can fix a conflict faster than a person notices it. Adding agents on top of a human workflow leaves each of the questions above to ad-hoc scripts.

### 1.2 Product thesis

Parley is a git-native hub built around how agents work. Eight ideas shape it:

1. **The change is the unit of work, not the branch.** A change has a stable number, versions, a statement of intent and a link to the agent session that produced it. Landing, review, audit and events all address changes (§7).
2. **Changes stack.** A change may depend on another. When the lower one moves, the hub restacks the ones above it (§7).
3. **Repositories are cheap.** A task fork is created in one call, without copying history, together with a token that works only there. It expires by itself (§4).
4. **Source control is an API.** Files, trees, history, diffs and blame can be read, and commits created, without a clone. Git over HTTP stays available for everything else (§5, §6).
5. **Landing is optimistic.** Landing is a server-side rebase and a compare-and-swap on the target branch. Changes that do not conflict never wait for each other. A conflict comes back as data an agent can act on (§8).
6. **Policy decides what may land, by path.** Rules per workspace say which paths land automatically, which need checks and which need a person. Tokens can be limited to paths (§8).
7. **Agents can see each other.** One call answers which in-flight changes and claims touch a set of paths (§9).
8. **Events are durable.** Every push, fork, version, land and conflict is an ordered, replayable event, so review agents and CI can subscribe reliably (§14).

Three things stay fixed. Stock `git` clients keep working. Git remains the storage and exchange format, so leaving Parley is a `git clone`. And Parley is sized for a team and its agents, not for a hyperscale monorepo: it runs as one process.

### 1.3 Product

The hub holds everything an agent run needs except the agent itself:

- the repository, and a cheap private copy of it for each task
- the path from a finished task back to the main branch
- the credentials and the secrets
- a sandbox to run in
- a record of what happened, why, and what it cost

It ships as two programs:

- **`parleyd`** is the server. It serves:
  - a REST API under `/api/v1`
  - a git smart-HTTP server under `/git`
  - a forge webhook ingress under `/hooks/forge`
  - Prometheus metrics and health endpoints
  - optionally, an AgentKit-compatible sandbox gateway (gRPC) on a second listener
- **`parley`** is the command-line client. It speaks only the REST API, and everything it does can also be done over REST.

| Capability | What it gives the user | Section |
|---|---|---|
| Identity and scoped access | GitHub OAuth sign-in, API keys and personal access tokens (PATs) with read/write scopes, PATs bound to one workspace and limited to paths, organizations, an admin account | §3 |
| Workspaces | Register a repository once. The hub clones it, keeps it in sync with its remote, serves it over git and pushes branches back | §4, §5 |
| Task forks | One call gives an agent its own copy of a workspace and a token for it. The fork expires when the work has landed | §4 |
| Source control API | Read files, trees, history, diffs and blame, search, and create commits and move refs, without a clone | §6 |
| Changes and stacks | Stable, versioned units of work with intent, linked to sessions, optionally stacked | §7 |
| Landing and policy | Optimistic server-side landing, structured conflict reports, path-based rules for approvals and checks, automatic landing, one-call revert | §8 |
| Awareness | See which in-flight changes and claims touch the paths you are about to edit | §9 |
| Carry-patch workspaces | Maintain a long-lived set of patches on top of an upstream. When upstream moves, the hub rebuilds the integration branch and remembers conflict resolutions | §10 |
| Secrets and variables | Stored per user, organization or workspace. Secret values can be written but never read back | §11 |
| Forge access | Read and write issues and pull requests on GitHub or GitLab through the hub, using the workspace's own credential. Receive the forge's webhooks as hub events | §12 |
| Sandboxes | Lease an isolated container shaped by an operator-defined profile. The hub injects secrets so the caller never sees them | §13 |
| Accounting, audit and events | Agent sessions with token usage, agent run telemetry, an audit log, a durable and replayable event stream, Prometheus metrics | §14 |

### 1.4 End-to-end example

A developer registers a repository once:

```sh
parley login --endpoint-url http://localhost:8080          # browser-based GitHub sign-in
parley workspace create --slug demo --git-url https://github.com/acme/demo.git \
    --git-pat "$GITHUB_TOKEN" --wait                       # returns once the hub has cloned it
```

An agent takes a task. It gets its own fork and a token that works only there:

```sh
parley fork create demo --name greeting --token-permissions git:write,changes:write \
    --purpose "Add a greeting endpoint"
# → { "workspace": { "slug": "demo--greeting", … }, "remote": "http://localhost:8080/git/acme/demo--greeting.git", "token": "parley_pat_…" }

parley activity demo --paths 'api/**'                      # is anyone else working here?

git clone --filter=blob:none "$REMOTE" && cd demo--greeting
git switch -c greeting && …commit…
git push -u origin greeting \
    -o parley.title="Add greeting endpoint" \
    -o parley.intent="Issue #41 asks for a health-style greeting route"
# remote: change demo#12 version 1 opened, landing: ready
```

The change lands when policy allows it. If someone else landed first and the two changes do not conflict, nothing waits:

```sh
parley change land demo 12 --wait
# → { "status": "landed", "landed_sha": "…", "attempts": 2 }
parley workspace push demo main                            # publish main from the hub to GitHub
```

An agent with no local checkout can do the same through the API alone: `POST …/scm/commits` to create the commit, `POST …/changes` to open the change, `POST …/changes/12/land` to land it.

### 1.5 System context

```
  parley, HTTP clients      git clients        GitHub / GitLab        AgentKit clients
         │                   │              webhooks │                    │
         ▼                   ▼                       ▼                    ▼
 ┌─────────────────────── parleyd: main HTTP listener ─────────────────────┐  ┌─ sandbox listener ─┐
 │ /api/v1/...            identity, workspaces and forks, source control   │  │ sandbox gateway    │
 │                        API, changes, landing, policy, awareness,        │  │ (gRPC, lease token)│
 │                        secrets, forge, sandboxes, sessions, events      │  └─────────┬──────────┘
 │ /git/<org>/<slug>.git  git smart HTTP (v2), push rules, push → change   │            │
 │ /hooks/forge/:slug     webhook signature → delivery log → hub event     │            ▼
 │ /metrics, /healthz, /readyz, /version                                   │    Podman or a remote
 └──────┬───────────────────┬─────────────────────┬────────────────────────┘    AgentKit gateway
        ▼                   ▼                     ▼
   hub state            event log and        repositories (bare),
   (identity, changes,  audit store          forks share objects
   policy, jobs, …)                          with their parent
        └── forge REST API ──▶ GitHub / GitLab          git push/fetch ──▶ origin, upstream
```

### 1.6 Goals

1. **Agent-first, git-native.** An agent can do everything through the REST API and stock `git`. No custom client is required on the agent's side.
2. **No waiting on other agents.** Work that does not conflict proceeds in parallel at every step: forking, pushing, landing.
3. **Legible to an agent.** Every refusal, conflict and policy decision is returned as structured data that says what to do next.
4. **Reviewable by intent.** People review what a change is for and whether policy was met, and read lines only where policy says they must.
5. **Traceable.** Every commit on a protected branch can be traced to a change, its versions, its approvals and the session that produced it.
6. **Simple to run.** One server process, embedded storage, one configuration file. No external database or message broker.

### 1.7 Non-goals

- **Replacing git.** Parley does not define a new storage format, a virtual filesystem or a custom wire protocol.
- **Hyperscale.** Several replicas, regional caches and repositories beyond what one host can serve are out of scope (§15).
- **A code review user interface.** Parley provides the review data model and API (changes, versions, approvals, checks). It ships no web interface.
- **Built-in CI.** Checks are reported to the hub by external runners. The hub does not run builds or tests itself.
- **A built-in issue tracker or message board.** Issues stay on the forge (§12). Coordination between agents happens through changes, claims and events (§9, §14).
- **Compatibility with the earlier hub.** The earlier hub gave Parley its shape. Its API, CLI, configuration names and data files are not supported.

### 1.8 Prior art

| Source | What Parley takes from it |
|---|---|
| Cloudflare Artifacts | Repositories as cheap, programmable primitives; one fork per task with a scoped token; repository events as triggers; recording why a change was made |
| Anthropic's Scion and its review tool | Source control as a service with server-side commits, rebases and history queries; optional working copies; compare-and-swap ref updates instead of a globally locked merge queue; review of intent with policy by path; directory-level permissions |
| Sapling | Stacks without branch juggling; automatic restack; recorded rewrite history; server-side rebase on push |
| Twigg | Changes with stable identifiers and versions, each reviewed on its own; per-directory ownership; mirroring landed work out to plain git |
| AgentHub | A coordination surface for agent swarms; per-agent limits |

Parley deliberately does not take Scion's storage engine, AgentHub's branchless graph or Twigg's built-in tracker and CI.

---

## 2. Personas, glossary and conventions

### 2.1 Personas

| Persona | Who | Typical credential | Main goals |
|---|---|---|---|
| **Operator** | Runs and administers the hub | Admin token (written at first boot) | Deploy and configure the hub; manage users, orgs, keys and tokens; define sandbox profiles; watch metrics and the audit log |
| **Developer** | A person who registers repositories and directs agents | API key, from `parley login` | Create workspaces; write landing policy; approve the changes that need a person; maintain carry-patch workspaces; manage secrets; mint scoped tokens for agents |
| **Agent** | An AI coding agent running on a developer's behalf | PAT bound to one workspace or task fork, often limited to paths | Fork, read and write code through git or the API; open changes and state their intent; fix conflicts; land; report sessions, usage and telemetry; lease a sandbox |
| **Review agent / check runner** | Automation that reacts to changes | PAT with `changes:read` and `checks:write`, possibly `changes:approve` | Follow the event stream; report check results; approve or reject versions where policy allows |
| **Org member** | A user who belongs to the organization that owns a workspace | API key or PAT | Read the org's workspaces; contribute through a task fork and a change |
| **Forge** | GitHub or GitLab | Webhook secret | Deliver webhooks that the hub turns into hub events |
| **Sandbox client** | An AgentKit client that holds a sandbox lease | Lease token | Create and drive exactly one sandbox through the gateway |

### 2.2 Glossary

| Term | Meaning |
|---|---|
| Hub | The `parleyd` server |
| Workspace | A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork |
| Origin | A registered workspace's remote (`git_url`) |
| Task fork (fork) | A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires |
| Tracking branch | In a fork, a branch that existed in the parent at fork time. It is read-only in the fork and always equals the parent's branch |
| Protected branch | A branch that moves only by landing a change or by sync from origin. By default the default branch |
| Change | The unit of work: a proposal to add the commits of a source branch to a target branch, with a number, a title, an intent, versions and a state |
| Version | One head of a change's source branch. Every push, API commit or restack that moves the source creates a new version. Old versions stay fetchable |
| Intent | The author's statement of why the change exists |
| Stack | Changes linked by `depends_on`, lowest first |
| Restack | The hub rebasing a dependent change onto the new head of the change below it, or onto the target once that change has landed |
| Landing | Adding a change's commits to its target branch: a server-side rebase followed by a compare-and-swap of the target ref |
| Land attempt | One request to land a change (or a stack), with its outcome |
| Conflict report | The structured description of why a rebase could not be done: files, kinds, and the landed changes involved |
| Policy | A workspace's ordered rules that map touched paths to landing requirements |
| Approval | A user's verdict (`approve` or `reject`) on one version of a change |
| Check | A named result (`pending`, `passed`, `failed`) reported on one version by an external runner |
| Claim | An advisory, expiring statement that a session intends to work on some paths |
| Carry-patch workspace | A workspace that tracks an upstream project and keeps an ordered list of patches on top of it |
| Upstream | For a carry-patch workspace, the project it tracks (`upstream_url`) |
| Integration branch | For a carry-patch workspace, the branch the hub rebuilds as upstream plus the ordered patches |
| Patch | A carry-patch entry: a branch with a position in the patch order and a status |
| Rebuild | A durable job that recreates the integration branch |
| Durable job | Background work that survives restarts and is retried with backoff (rebuilds, clones) |
| Scope / permission | A string carried by a PAT that grants read or write access to an area |
| Bound token | A PAT confined to one workspace (and the forks created with it) |
| Path-scoped token | A PAT whose writes are limited to files matching a list of globs |
| Lease | A short-lived right to create and use one sandbox, shaped by a profile |
| Profile | An operator-defined sandbox template |
| Session | An agent session: who ran, on which workspace, with what token usage, and which changes it produced |
| Hub event | A record of something the hub did (`hub.*` types), with a sequence number. Hub events form the audit log and the event stream |
| Agent telemetry | Run events, tool calls, traces, outcomes and postmortems that agents send to the hub |

### 2.3 Conventions used in this document

- Paths are written relative to the API root `/api/v1` unless they start with `/git`, `/hooks`, `/metrics`, `/healthz`, `/readyz` or `/version`.
- `:param` is a path parameter.
- Requirement IDs (`WS-FR-012`) are stable references for specs and tests. The prefixes are `PLAT`, `WS`, `GIT`, `SCM`, `CHG`, `LAND`, `AWR`, `CP`, `SEC`, `FRG`, `SBX`, `AUD` and `NFR`. User stories use `-US-`.
- "Error `xyz`" means the error envelope (§3) with `error_type` `xyz`.
- **MUST** marks a strict requirement.
- The conventions that hold for every route and command are specified once, in §3: the error envelope, the order of checks, list paging, idempotency keys, timestamps, and the CLI's output and exit codes. Sections state only what differs.
- A commit id (`sha`) is always the full 40-character hexadecimal form in responses. Requests accept any revision the repository can resolve where a field is called `ref`, and only a full id where it is called `sha`.

---

## 3. Platform, identity and access

### 3.1 Purpose and scope

This section is the base every other section sits on. It covers:

- the two binaries, `parleyd` (server) and `parley` (CLI): how the server starts, creates its first admin credential, runs its maintenance loop and shuts down;
- identity: OAuth login, users, organizations and memberships, API keys, personal access tokens (PATs), the admin token, and the scope registry;
- access: the workspace access rule, anti-enumeration, workspace-bound tokens, path-scoped tokens and rate limits;
- the **global conventions** that every route and every CLI command obeys: the error envelope and the `error_type` catalogue, the check order, lists and cursors, idempotency, timestamps, body limits, request ids and response headers, and the CLI's output, exit codes, waiting and configuration;
- the health, version and metrics endpoints;
- server configuration and environment variables.

Other sections refer here for these conventions and do not restate them.

Out of scope: the per-feature routes that apply the access rule (§4–§14), the enforcement of path-scoped tokens on writes (§5–§8), the fork-create call (§4), the metric families and the event log (§14), and the keys inside `[sandbox]` (§13).

### 3.2 User stories

**PLAT-US-01 (Operator): first boot.** As an Operator, I want the first start to give me an admin token, so that I can administer the hub without a user account.
- `parleyd --admin-email=a@x` on a new hub writes an admin token file, prints its path and exits without serving.
- The hub refuses to start while the token file still exists.
- `--reset-admin-token` replaces the token. The old one stops working at once.

**PLAT-US-02 (Developer): log in.** As a Developer, I want `parley login` to open my identity provider in the browser and store an API key, so that later commands just work.
- A login returns a new API key and revokes the user's older API keys.
- The key, the hub URL and the user id are saved to `~/.parley/config.toml` (mode 0600).
- A new user gets a personal organization.

**PLAT-US-03 (Developer): mint scoped tokens.** As a Developer, I want to mint PATs with selected scopes, so that CI jobs and agents get only what they need.
- An unknown scope is refused with 400.
- A PAT can mint PATs only with `tokens:manage`, and only with scopes and paths it holds itself.
- The token value appears only in the creation response.

**PLAT-US-04 (Developer / Agent): workspace-bound token.** As a Developer, I want to mint a token bound to one workspace for an agent, so that a leaked token reaches nothing else.
- The bound token works on its workspace and on the task forks it creates.
- Every other workspace answers it `404 workspace not found`.
- Identity routes answer it `403 token_bound`.

**PLAT-US-05 (Developer / Agent): path-scoped token.** As a Developer, I want to limit a token's writes to some paths, so that an agent assigned to `docs/**` cannot change `src/**`.
- The token carries `paths`. Reads are not restricted.
- A token can never mint a token with wider paths than its own.

**PLAT-US-06 (Org member): read and contribute.** As an Org member, I want to read my org's workspaces and contribute through a task fork, so that I can work on a repository I do not own.
- Read routes answer a member as they answer the owner. The workspace object carries `access: "member"`.
- Write routes answer `403 forbidden`.
- A member may create a task fork of a workspace they can read and open changes from it into the parent.

**PLAT-US-07 (any caller): anti-enumeration.** As any caller, I must not learn whether a workspace I cannot see exists.
- An unknown slug and an invisible workspace get the same `404 workspace not found` on every surface.

**PLAT-US-08 (Operator): administration.** As an Operator, I want to manage users, orgs, keys and tokens.
- `parley admin …` calls the admin routes. Non-admins get `403 forbidden`.
- The last active admin can be neither demoted nor deleted.

**PLAT-US-09 (Agent / CI): scriptable CLI.** As an Agent or CI job, I want machine-readable output and stable exit codes.
- stdout carries only JSON results. Errors, hints and progress go to stderr.
- Exit codes are 0, 1, 2 and 3 with fixed meanings.

**PLAT-US-10 (Operator): probes.** As an Operator, I want liveness, readiness, version and metrics endpoints without credentials.

### 3.3 Concepts and data model

#### 3.3.1 Process model

Parley runs as one `parleyd` process. Running two processes against the same data is not supported. The hub is built for a team, not for hyperscale (§15).

`parleyd` needs a `git` binary on its `PATH`. The minimum version is stated in §5.

#### 3.3.2 Credential types

| Type | Wire format | Identity | Scope-checked | Admin |
|---|---|---|---|---|
| Admin token | `parley_admin_<64 lowercase hex>` | none (no user) | no | yes, on every route |
| API key | `parley_<key_id>_<secret>`; `key_id` 8 and `secret` 32 characters from `[0-9A-Za-z]` | a user | no | on identity admin routes, when the user's role is `admin` |
| PAT | `parley_pat_<token_id>_<secret>`; `token_id` 8 and `secret` 32 characters from `[a-z0-9]` | a user | yes | never |

- The hub stores only a hash of each secret. A secret is shown once, in the response that creates it.
- REST calls present the credential as `Authorization: Bearer <credential>`. The git server takes it as the HTTP Basic password (§5).
- An API key represents a person at a terminal. A PAT represents automation: it is the only credential that carries scopes, a workspace binding or path limits.

#### 3.3.3 Entities

**User**

| Field | Type | Meaning |
|---|---|---|
| `id` | UUID | identifier |
| `username` | string | unique |
| `email` | string | unique |
| `full_name` | string | may be empty |
| `role` | enum | `user` (default) or `admin` |
| `status` | enum | `active` (default) or `blocked` |
| `provider`, `provider_id` | string | the identity provider and its stable user id; the pair is unique |
| `created_at`, `updated_at` | timestamp | |

**API key**

| Field | Type | Meaning |
|---|---|---|
| `key_id` | string | identifier, part of the wire format |
| `user_id` | UUID | owner |
| `expires_days` | int | 0, 30, 60 or 90; 0 means never |
| `expires_at` | timestamp or null | |
| `revoked_at` | timestamp or null | |
| `created_at` | timestamp | |

**PAT**

| Field | Type | Meaning |
|---|---|---|
| `token_id` | string | identifier, part of the wire format |
| `user_id` | UUID | owner |
| `name` | string | 1–255 characters |
| `permissions` | string[] | scopes (§3.3.5), without duplicates, in the order given |
| `paths` | string[] or null | write path limits (PLAT-FR-057); null means unrestricted; fixed at creation |
| `workspace` | string or null | the slug the token is bound to (PLAT-FR-080); fixed at creation |
| `expires_at` | timestamp or null | |
| `revoked_at` | timestamp or null | |
| `created_at` | timestamp | |

**Org**

| Field | Type | Meaning |
|---|---|---|
| `id` | UUID | identifier |
| `name` | string | unique |
| `slug` | string | unique; immutable |
| `url` | string | free text, default empty |
| `owner_id` | UUID or null | informational; cleared when that user is deleted |
| `status` | enum | `active` (default) or `blocked` |
| `created_at`, `updated_at` | timestamp | |

**Membership:** `(org_id, user_id)`, unique, with `created_at`. Deleting the org or the user deletes the membership.

**Admin settings:** the bootstrap `admin_email` and the hash of the admin token.

#### 3.3.4 States

- **User role.** `user` ⇄ `admin`, through promote and demote.
- **User status.** `active` ⇄ `blocked`. Blocking revokes nothing. While a user is blocked, every credential of that user fails authentication with `403`. Unblocking restores every credential that is neither revoked nor expired.
- **Org status.** `active` ⇄ `blocked`. While an org is blocked, its memberships grant no workspace access, and the org is hidden from `GET /user/orgs` and, by default, from `GET /orgs`. Workspace owners are not affected.
- **API key and PAT.** `live` → `revoked` (final) or `live` → `expired` (by time). Records are kept after either.
  - An API key is revoked by its owner, by an admin, or by the owner's next OAuth login.
  - A PAT is revoked by its owner, by an admin, by a permission change that leaves no scope, or, when bound, by the deletion or expiry of its workspace.

#### 3.3.5 Scope registry

A scope is `resource:action`. The registry is fixed:

| Resource | Scopes | Used by |
|---|---|---|
| `users` | `users:read`, `users:write` | `GET /user`, `PATCH /user` |
| `orgs` | `orgs:read` | org reads |
| `keys` | `keys:read`, `keys:manage` | API key list; API key revoke |
| `tokens` | `tokens:read`, `tokens:write`, `tokens:manage` | PAT reads; PAT permission changes; PAT create and revoke |
| `workspaces` | `workspaces:read`, `workspaces:write` | §4 |
| `git` | `git:read`, `git:write` | git protocol (§5) and source control API (§6) |
| `changes` | `changes:read`, `changes:write`, `changes:approve` | §7, §8, §9 |
| `checks` | `checks:write` | §8 |
| `patches` | `patches:read`, `patches:write` | §10 |
| `rebuilds` | `rebuilds:read`, `rebuilds:write` | §10 |
| `secrets` | `secrets:read`, `secrets:write` | §11 |
| `vars` | `vars:read`, `vars:write` | §11 |
| `forge` | `forge:read`, `forge:write` | §12 |
| `sandboxes` | `sandboxes:read`, `sandboxes:write` | §13 |
| `audit` | `audit:read`, `audit:write` | §14 |
| `sessions` | `sessions:read`, `sessions:write` | §14 |

All scopes are registered whether or not a feature (for example sandboxes) is enabled.

Implication rules:

- A read check accepts `R:read` or `R:write`. A write check accepts only `R:write`.
- `keys:manage` implies `keys:read`. `tokens:manage` implies `tokens:write` and `tokens:read`. `tokens:write` implies `tokens:read`.
- `changes:approve` and `checks:write` are never implied by another scope, and they imply nothing. A token that may write changes cannot approve them unless it also holds `changes:approve`.
- Only PATs are scope-checked. Admin tokens and API keys pass every scope check.

#### 3.3.6 Workspace access levels

| Level | Who | May |
|---|---|---|
| `admin` | the admin token | everything |
| `owner` | the user who owns the workspace; for a task fork, also the owner of its parent | everything |
| `member` | a member of the workspace's org, while the org is `active` | read; create task forks; open changes from an own fork |
| none | everyone else | nothing; the workspace does not exist for them |

Each workspace route declares one **need**: `read`, `write` or `owner`. `owner` marks routes that are not writes but are still private to the owner, such as listing bound tokens or secrets. Every workspace object carries the caller's level as `access`.

An API key of an admin-role user is **not** `admin` here. The admin role governs identity administration only.

#### 3.3.7 CLI configuration file

- **Location.** `$PARLEY_CONFIG_DIR/config.toml` when `PARLEY_CONFIG_DIR` is set and non-empty, otherwise `~/.parley/config.toml`.
- **Keys.**

  | Key | Meaning |
  |---|---|
  | `endpoint_url` | the hub's base URL |
  | `api_key` | the credential (API key or PAT) |
  | `user_id` | the logged-in user's id; informational |
- **Creation.** `parley login` creates the directory (mode 0700) and the file (mode 0600). Other commands never create it.
- **Writing.** The file is replaced atomically and keeps mode 0600. Keys the CLI does not know are preserved.
- An unparseable file is a client error (exit 2).

### 3.4 Functional requirements

#### Server start, bootstrap and shutdown

- **PLAT-FR-001 (flags).** `parleyd` takes two flags: `--admin-email <string>` and `--reset-admin-token`. `--help` prints usage and exits 0. An unknown flag prints usage and exits 2. There are no subcommands.
- **PLAT-FR-002 (start).** The server loads and validates its configuration (§3.9), opens its data, runs the bootstrap (PLAT-FR-003), resumes durable work, starts the maintenance loop (PLAT-FR-005) and then listens on `[server].bind:[server].port`. Any failure before listening stops the process with exit status 1 and one error line on stderr. Durable work that is resumed at start includes clones left `pending` or `cloning` (§4) and queued jobs (§10, §12). When it starts listening, the server logs the address, the configuration path and the data directory.
- **PLAT-FR-003 (bootstrap).** The hub is **bootstrapped** once an admin token hash is stored. The token file is `<config dir>/admin_token`, where `<config dir>` is the directory of `config.toml` (§3.9).

  | Situation | Behaviour |
  |---|---|
  | not bootstrapped, no flags | start normally, without an admin token |
  | not bootstrapped, `--admin-email` | store the e-mail, generate the token, write the file, exit |
  | `--reset-admin-token` (bootstrapped or not) | generate a new token, replace the stored hash, write the file (overwriting), exit |
  | bootstrapped, token file exists | refuse to start: exit 1, naming the file |
  | bootstrapped, no token file | start normally; `--admin-email` is ignored |

  The token file has mode 0600 and holds the token with no trailing newline. After writing it, the server logs the file's absolute path at warn level, tells the operator to save the token and delete the file, and exits with status 0 without serving. The token itself is never logged.
- **PLAT-FR-004 (shutdown).** On SIGTERM or SIGINT the server stops accepting connections and gives in-flight requests 15 seconds to finish. Event streams are closed. Background work stops; anything durable resumes at the next start. A clean shutdown exits 0. A failure to bind the listener exits 1.
- **PLAT-FR-005 (maintenance loop).** The loop runs once right after start and then every hour. A pass runs these tasks in order:

  | Task | Defined in |
  |---|---|
  | audit retention | §14 |
  | event-log retention | §14 |
  | forge delivery pruning | §12 |
  | token binding sweep | PLAT-FR-086 |
  | patch purge | §10 |
  | fork expiry | §4 |

  A failing task is logged and does not stop the others. Passes never overlap. The interval is not configurable. A section may also act on its own, finer timer; the loop is the guaranteed backstop.

#### HTTP conventions

- **PLAT-FR-010 (surfaces).** The REST API is mounted at `/api/v1`. The mount point is fixed. Outside it are `/healthz`, `/readyz`, `/version`, `/metrics`, the git server under `/git` (§5) and the webhook ingress under `/hooks` (§12). Trailing slashes are not normalised.
- **PLAT-FR-011 (error envelope).** Every error response on the REST API is JSON:

  ```json
  {"error":{"code":409,"message":"workspace is busy","error_type":"workspace_busy"}}
  ```

  - `code` equals the HTTP status.
  - `error_type` is always present. It is the machine contract, together with the status.
  - `message` is for people. Clients MUST NOT parse it.
  - An error may carry extra members inside `error` when a section says so (for example `oldest_seq` with `cursor_expired`).
  - The content type is `application/json; charset=utf-8`.

  Exceptions: `/healthz` and `/readyz` answer plain status JSON, and git routes answer in git's own text conventions where a git client reads the response (§5).
- **PLAT-FR-012 (`error_type` catalogue).** The generic types below are used whenever no specific type applies. Sections define specific types in their own N.10; a specific type always keeps the status of its generic class unless the section states another.

  | `error_type` | Status | Meaning |
  |---|---|---|
  | `invalid_request` | 400 | malformed or invalid input |
  | `unauthenticated` | 401 | missing, unknown, revoked or expired credential |
  | `forbidden` | 403 | authenticated, but not allowed |
  | `not_found` | 404 | missing or invisible resource, or unknown path |
  | `method_not_allowed` | 405 | wrong method on an existing path |
  | `conflict` | 409 | the request conflicts with current state |
  | `gone` | 410 | the resource existed and is permanently unavailable |
  | `payload_too_large` | 413 | body over the limit |
  | `unsupported_media_type` | 415 | body is not JSON |
  | `rate_limited` | 429 | a rate or quantity limit was hit |
  | `internal` | 500 | hub failure; never a grant |
  | `upstream_error` | 502 | an external system (identity provider, origin, forge) failed |
  | `unavailable` | 503 | the hub cannot serve the request now |

  Specific types defined in this section:

  | `error_type` | Status | Meaning |
  |---|---|---|
  | `insufficient_scope` | 403 | a PAT lacks the scope the route needs, or tries to grant a scope or path it does not hold |
  | `token_bound` | 403 | a workspace-bound token used a route it may not use |
  | `path_not_allowed` | 403 | a path-scoped token tried to write outside its `paths` (raised in §5–§8) |
  | `workspace_archived` | 409 | the route needs a workspace that is not archived |

  Specific types that several sections share, defined where noted: `workspace_busy` (409, §4), `fork_has_no_origin` (400, §4), `ref_moved` (409, §6), `land_conflict` (409, §8), `policy_blocked` (409, §8), `cursor_expired` (410, §14).
- **PLAT-FR-013 (check order).** Every request is checked in this order. The first failing check decides the answer.

  | # | Check | Failure |
  |---|---|---|
  | 1 | The path exists; the method is allowed on it | `404 not_found`; `405 method_not_allowed` with an `Allow` header |
  | 2 | Authentication (PLAT-FR-020) | `401 unauthenticated`; `403 forbidden` for a blocked user |
  | 3 | Rate limit (PLAT-FR-025) | `429 rate_limited` |
  | 4 | Body size and content type (PLAT-FR-017) | `413`; `415` |
  | 5 | Binding guard, bound PATs only (PLAT-FR-083) | `404 not_found` or `403 token_bound` |
  | 6 | PAT scope | `403 insufficient_scope` |
  | 7 | Workspace access (PLAT-FR-060) or admin check (PLAT-FR-045) | `404 not_found`; `403 forbidden` |
  | 8 | Request validation | `400` |
  | 9 | State preconditions | `409` or `400` |
  | 10 | Act | |

  Consequences: a PAT without the scope gets 403 whether or not the workspace exists, so the answer leaks nothing. A member's write to an archived workspace is 403, not 409. Steps 1–4 on public routes skip authentication.
- **PLAT-FR-014 (lists).** Every list endpoint answers

  ```json
  {"items":[…],"next_cursor":"<opaque string>"}
  ```

  - `items` is always an array, never null. `next_cursor` is null on the last page.
  - `limit` defaults to 50. A value outside 1–200 is `400 invalid_request`.
  - `cursor` is the `next_cursor` of the previous page. It is opaque. A cursor that is malformed or belongs to another query is `400 invalid_request`.
  - Each endpoint states its order. The order is total, so paging neither repeats nor skips an item that existed throughout.
- **PLAT-FR-015 (idempotency).** Every `POST` that creates a resource accepts an optional `Idempotency-Key` header of 1–255 characters.
  - The hub records the response of a successful create under the key, the caller (the user, or the admin token) and the route, for 24 hours.
  - A replay with the same key, caller and route within that time creates nothing and answers status `200` with the original body.
  - If the original body carried a secret that is shown once (a token or key value), the replay omits that member.
  - A replay with a different request body is `409 conflict`.
  - Failed requests are not recorded; a retry with the same key runs again.
  - Of several concurrent requests with the same key, exactly one creates; the others answer as replays.
  - A longer or empty key is `400 invalid_request`.
  - `POST /auth/callback` does not take the header, because an OAuth code is single-use.
- **PLAT-FR-016 (timestamps).** Timestamps are RFC 3339 in UTC with whole seconds (`2026-01-02T03:04:05Z`) unless a field says otherwise. Durations in requests and configuration use a number with a unit suffix (`30s`, `15m`, `24h`, `30d`).
- **PLAT-FR-017 (bodies).**
  - Request and response bodies on the REST API are JSON.
  - A request body larger than `[server].max_body_size` (default 1 MiB) is `413 payload_too_large`, whether or not it declares its length. Git routes, the webhook ingress and routes that carry file content set their own limits (§5, §6, §12).
  - A `POST`, `PUT` or `PATCH` with a body whose `Content-Type` is not `application/json` is `415 unsupported_media_type`. A request without a body is exempt.
  - A body that is not valid JSON, or not an object where one is expected, is `400 invalid_request`. Unknown members are ignored.
- **PLAT-FR-018 (request id and headers).**
  - An incoming `X-Request-ID` that is a valid UUID is reused; otherwise the hub generates one. Every response carries `X-Request-ID`, and every access-log line carries it.
  - Every response carries `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY` and `Referrer-Policy: no-referrer`.
  - API responses carry `Cache-Control: no-store`. Exceptions: `GET /auth/providers` and `GET /version` carry `public, max-age=300`; `/healthz` and `/readyz` carry `no-cache`.
- **PLAT-FR-019 (concurrency).** No request fails only because another operation is running. Ref updates are compare-and-swap. `409 workspace_busy` remains only while a workspace is in an exclusive maintenance operation (§4).

#### Authentication and rate limits

- **PLAT-FR-020 (authentication).** Every route under `/api/v1` needs a credential, except `GET /auth/providers` and `POST /auth/callback`. The scheme name `Bearer` is matched without regard to case. Checks run in this order:

  | Check | Failure |
  |---|---|
  | header present, scheme `Bearer`, token non-empty and of a known format | `401 unauthenticated`, `authentication required` |
  | credential id exists and the secret matches | `401 unauthenticated`, `invalid credentials` |
  | not revoked | `401 unauthenticated`, `credential revoked` |
  | not expired | `401 unauthenticated`, `credential expired` |
  | user not blocked | `403 forbidden`, `user is blocked` |

  Revocation and expiry are reported only after the secret matched, so a caller who knows only an id cannot probe a credential's state. Secrets are compared in constant time.
- **PLAT-FR-021 (admin token and user routes).** The admin token has no user. Routes under `/user` and the bound-token mint need a user; they answer the admin token `403 forbidden`.
- **PLAT-FR-025 (rate limit).** The hub limits requests per credential to `[limits].requests_per_minute` (default 600; 0 turns the limit off). Requests without a valid credential are counted per client address. A request over the limit is answered `429 rate_limited` with a `Retry-After` header in whole seconds. The limit covers the REST API and the git server. `/healthz`, `/readyz`, `/version`, `/metrics` and the webhook ingress are exempt. Counters are not durable and reset at restart.

#### OAuth login

- **PLAT-FR-030 (providers).** The supported providers are `github` and `google`. `GET /auth/providers` lists the configured ones with an `authorize_url` that carries the client id and the scopes the hub needs (`user:email` for GitHub; `openid email profile` for Google). The caller adds `redirect_uri` and `state`. Client secrets are never exposed.
- **PLAT-FR-031 (callback).** `POST /auth/callback` validates the body, validates `redirect_uri`, exchanges the code with the provider, reads the user's identity and then, atomically, upserts the user and issues an API key. Calls to the provider time out after 15 seconds.
  - `redirect_uri` is allowed when it is `http` on `localhost` or `127.0.0.1` (any port and path), or when its scheme, host and port equal those of `[server].external_url`.
  - The identity MUST include a stable provider user id and a verified e-mail. GitHub's primary verified e-mail is used when the profile hides the address. A missing id or e-mail is `502 upstream_error`.
  - The username is the provider's login or display name, or `<provider>-<provider_id>` when the provider gives none.
- **PLAT-FR-032 (upsert).**
  - **Existing user** (matched by `provider` and `provider_id`): a blocked user is refused with 403 and nothing changes. Otherwise `email` and `username` are refreshed from the provider. If the new username is taken, the old one is kept.
  - **New user:** created `active`. If the username is taken, the hub tries `<name>-2` … `<name>-10`. The role is `admin` when the e-mail equals the bootstrap `admin_email` (ignoring case) and no admin user exists yet; otherwise `user`. The personal org is created (PLAT-FR-035).
  - **Both:** every live API key of the user is revoked and a new key is issued with the requested lifetime. Automation should therefore use PATs, not API keys.
  - A uniqueness conflict (for example an e-mail that belongs to another account) is `409 conflict` and changes nothing.
- **PLAT-FR-033.** Only the OAuth callback creates API keys. There is no password login.

#### Users and organizations

- **PLAT-FR-035 (personal org).** Every new user, from OAuth or `POST /users`, gets an org in the same transaction: `name` is the username, `owner_id` is the user, and the user is its first member. The slug is derived from the username:
  1. lowercase; replace every character outside `[a-z0-9_-]` with `-`; collapse runs of `-`; trim leading and trailing `-` and `_`;
  2. if fewer than 2 characters remain, use `u-<first 8 characters of the user id>`;
  3. if it starts with a digit, prefix `u-`;
  4. if the slug is taken, try `<slug>-1` … `<slug>-10`.

  Examples: `Alice.Smith` → `alice-smith`; `42bob` → `u-42bob`. If no free slug is found, or an org already has the user's name, the user creation fails with `409 conflict` and nothing is created.
- **PLAT-FR-040 (selectors).** On user routes, `:id` and `:user_id` accept the user's UUID, e-mail (a value containing `@`) or username. On org routes, `:id` accepts the org's UUID or slug.
- **PLAT-FR-045 (admin check).** "Admin" on the user and org routes means the admin token, or an API key whose user has role `admin`. PATs are never admin. A failed admin check is `403 forbidden`.
- **PLAT-FR-046 (last admin).** Demoting or deleting a user who is the only `active` user with role `admin` is `409 conflict`. The check and the change are atomic. Blocking has no such safeguard, because the admin token can always unblock.
- **PLAT-FR-047 (idempotent state changes).** Promote, demote, block and unblock answer 200 with the unchanged object when the target is already in the requested state. Revoking a credential that is already revoked answers 200 with the unchanged object.
- **PLAT-FR-048 (user delete).** Deleting a user deletes the user's API keys, PATs and memberships and clears `owner_id` on orgs the user owned, in one transaction. Workspaces the user owned are kept; they remain reachable by the admin token and by org members.
- **PLAT-FR-049 (org delete).** Deleting an org deletes its memberships. Personal orgs can be deleted. Workspaces of the org are kept and lose their member access.

#### API keys and PATs

- **PLAT-FR-050 (PAT create).** `POST /user/tokens` validates:
  - `name`: 1–255 characters;
  - `permissions`: at least one entry, each a registered scope; duplicates are dropped;
  - `expires`: 0, 30, 60 or 90 days (default 90; 0 means never);
  - `paths`: PLAT-FR-057.

  A PAT caller needs `tokens:manage` and is held to the subset rule (PLAT-FR-051).
- **PLAT-FR-051 (subset rule).** A PAT can never create or widen a token beyond itself. When the caller is a PAT:
  - every requested permission MUST be held by the caller exactly (implication does not count: a caller with `git:write` cannot grant `git:read` unless it holds it);
  - the requested `paths` MUST be equal to or narrower than the caller's (PLAT-FR-059).

  A violation is `403 insufficient_scope` and names the first offending permission or path. API keys are not restricted.
- **PLAT-FR-052 (permission changes).** `PUT`, `PATCH` and `DELETE` on `/user/tokens/:token_id/permissions` replace, add and remove scopes on one of the caller's own tokens.
  - PAT callers need `tokens:write`. The subset rule applies to every scope the change would add.
  - A revoked or expired token is `409 conflict`.
  - `PUT` and `PATCH` validate scopes against the registry. `DELETE` ignores scopes the token does not have.
  - A change that leaves no scope revokes the token.
  - `paths` and the workspace binding cannot be changed.
- **PLAT-FR-053 (PAT revoke).** `DELETE /user/tokens/:token_id` revokes one of the caller's own tokens. PAT callers need `tokens:manage`. A PAT may revoke itself.
- **PLAT-FR-054 (API key refresh).** `POST /user/keys/:key_id/refresh` replaces the secret of one of the caller's own live keys, keeps `key_id`, and restarts the key's lifetime from now. It needs an API key; a PAT is `403 forbidden`. A revoked or expired key is `409 conflict`.
- **PLAT-FR-055 (API key revoke).** `DELETE /user/keys/:key_id` revokes one of the caller's own keys. PAT callers need `keys:manage`. An expired key may be revoked. A key may revoke itself.
- **PLAT-FR-056 (ownership).** On `/user/keys/…` and `/user/tokens/…`, a key or token that belongs to another user is answered like a missing one: `404 not_found`.

#### Path-scoped tokens

- **PLAT-FR-057 (the `paths` field).** A PAT may carry `paths`, a list of globs over repository paths. It is set at creation, stored on the token and returned in every token object. It is allowed on unbound and on bound PATs. Validation:
  - absent or null means unrestricted; an empty array is `400 invalid_request`;
  - at most 50 globs;
  - each glob is 1–255 characters, uses `/` as the separator, and does not start with `/`;
  - `*` matches any run of characters within one path segment; `**` matches any run of characters including `/`; every other character matches itself. Matching is case-sensitive and covers the whole path.

  Examples: `docs/**` matches `docs/a.md` and `docs/api/b.md`; `src/*.go` matches `src/main.go` but not `src/x/y.go`.
- **PLAT-FR-058 (meaning).** A path-scoped token can only create commits, change versions and pushes whose changed files all match at least one of its globs. `paths` restricts **writes only**. It never restricts reads, and it does not restrict writes that touch no repository file. Enforcement, including what counts as a changed file, is specified in §5–§8; a refusal is `403 path_not_allowed`.
- **PLAT-FR-059 (narrowing).** A caller without `paths` may mint any `paths`. A caller with `paths` MUST request `paths`, and each requested glob MUST be **covered** by one of the caller's globs. Glob `C` covers glob `G` when `C`, used as a pattern, matches the text of `G` taken literally, with one restriction: a `*` in `C` never matches the sequence `**`. Equal globs always cover each other.

  | Caller glob | Requested glob | Covered |
  |---|---|---|
  | `src/**` | `src/api/*.go` | yes |
  | `src/**` | `src/**` | yes |
  | `src/*` | `src/**` | no |
  | `src/**` | `docs/**` | no |

  The test is conservative: it may refuse a glob that is narrower in fact, and it never accepts a wider one.

#### Workspace access rule

- **PLAT-FR-060 (decision).** Every route that names a workspace by slug decides access as follows, after authentication, the binding guard and the scope check:
  1. Resolve the caller's level (§3.3.6): `admin` for the admin token; `owner` for the workspace's owner and, for a task fork, the owner of its parent; `member` for a member of the workspace's active org; otherwise none.
  2. Level none, or no such workspace: `404 not_found` with the message `workspace not found`.
  3. Level `member` on a `write` or `owner` route: `403 forbidden` with the message `org members have read-only access to this workspace`.
  4. Otherwise access is granted.

  A failure while deciding is `500 internal`, never a grant.
- **PLAT-FR-061 (anti-enumeration).** A caller with no read access gets exactly the same response for an existing workspace as for a nonexistent slug: same status, same body. This holds on every surface, including git, and in every state of the workspace. A member's 403 is not a leak, because the member can already read the workspace.
- **PLAT-FR-062 (member contributions).** Two routes are open to members although they change state:
  - creating a task fork of a workspace the member can read (§4). The member owns the fork;
  - opening a change from a fork the member owns into the fork's parent, and updating or abandoning that change (§7).

  Nothing else in the parent is writable by a member. Landing follows the parent's policy (§8).
- **PLAT-FR-063 (readable set).** Cross-workspace reads (the workspace list, events, audit) use one set: all workspaces for the admin token; otherwise the workspaces the user owns, the forks of those workspaces, and the workspaces of the user's active orgs. Ownership wins over membership when both apply. For a bound PAT the set is cut down to the workspaces its binding covers (PLAT-FR-081).

#### Workspace-bound tokens

- **PLAT-FR-080 (binding).** A PAT may be bound to exactly one workspace. The binding is set at creation and never changes. A bound token is an ordinary PAT of the minting user, with that user's access, restricted further by the binding. There are two ways to mint one:
  - `POST /workspaces/:slug/tokens` binds a new token to that workspace;
  - the fork-create call can mint a token bound to the new fork (§4).
- **PLAT-FR-081 (coverage).** A token bound to workspace W is valid on:
  - W;
  - every task fork of W that was created by a request authenticated with this token. The hub records the creating token on the fork.

  A token bound to a task fork F is valid on F. On F's parent it is valid only for the routes a contributor needs: the change routes for changes whose source is F (§7) and the awareness read routes (§9). Those sections list the routes.
- **PLAT-FR-082 (minting).** `POST /workspaces/:slug/tokens` takes the same body as `POST /user/tokens` and applies the same rules, including `tokens:manage` and the subset rule for PAT callers. In addition:
  - the route's need is `owner`;
  - the admin token is refused (`403 forbidden`), because a bound token needs a user as its owner;
  - a bound PAT is refused (`403 token_bound`). A bound token can mint only through fork-create, and only a token bound to the fork it creates, under the subset rule;
  - an archived workspace is `409 workspace_archived`.

  Minting is atomic: either a bound token exists and is returned, or nothing was created. A token is never left unbound by a failure.
- **PLAT-FR-083 (binding guard).** The guard runs after authentication on every API route and on git, for bound PATs only:

  | Request | Answer |
  |---|---|
  | names a workspace the binding covers | passes to the route's own checks |
  | names any other workspace, existing or not | `404 not_found`, `workspace not found` |
  | names no workspace and is on the allowlist | passes; the route confines its result to the covered workspaces |
  | anything else, including every identity route | `403 token_bound` |

  The allowlist is: `GET /workspaces`, `GET /events`, `GET /events/log`, `GET /audit`, `GET /sandbox-profiles`, and the session routes `POST /sessions`, `GET /sessions`, `GET /sessions/:id`, `POST /sessions/:id/complete`, `POST /sessions/:id/usage`, `GET /sessions/:id/usage`.

  The token is always authenticated first. A request that carries a bound token's id with a wrong secret gets 401, like any other bad credential.
- **PLAT-FR-084 (listing).** `GET /workspaces/:slug/tokens` lists the tokens bound to the workspace, including revoked and expired ones. Its need is `owner`. PAT callers need `tokens:read`. A bound PAT is refused (`403 token_bound`).
- **PLAT-FR-085 (workspace removal).** When a workspace is deleted, or a task fork expires, the hub revokes every live token bound to it in the same transaction as the removal. The revoked token ids appear in the removal's event metadata as `revoked_tokens` (§4). Archiving does not touch bound tokens: a token bound to an archived workspace still reaches it and gets the routes' usual archived answers.
- **PLAT-FR-086 (binding sweep).** Each maintenance pass revokes any live token whose bound workspace no longer exists. It is a backstop for PLAT-FR-085.
- **PLAT-FR-087 (refusal events).** Each request the binding guard refuses is reported as `hub.token.bound_refused` (§3.8), at most one event per token per minute. Refusals inside that minute are counted, and the count is carried as `suppressed` on the token's next event. Filtering a list is not a refusal. The limiter state is not durable. A failure to emit never changes the HTTP answer.

#### CLI global behaviour

- **PLAT-FR-090 (settings).** Every command except `login`, `version` and `help` needs a hub URL and a credential. Each takes the first non-empty value of flag, environment variable, config file:

  | Setting | Flag | Environment | File key |
  |---|---|---|---|
  | Hub URL | `--endpoint-url` | `PARLEY_ENDPOINT_URL` | `endpoint_url` |
  | Credential | `--api-key` | `PARLEY_API_KEY` | `api_key` |

  A missing value fails before any request, with exit status 2 and a message that names the flag, the variable and the file.
- **PLAT-FR-091 (requests).** Requests go to `<hub URL without trailing slash>/api/v1<path>` with `Authorization: Bearer <credential>`. Each request times out after 30 seconds unless a command states otherwise. Every user-supplied path segment is percent-encoded as one segment, so a `/` inside an argument becomes `%2F`.
- **PLAT-FR-092 (output).**
  - **stdout** carries the result and nothing else: one JSON document, indented by 2 spaces, followed by a newline. A command whose route answers `204` prints `{}`.
  - **stderr** carries errors, warnings, hints and progress.
  - An error is printed on stderr as the error envelope (PLAT-FR-011). A hub error is printed as received, including `error_type`. An error raised by the CLI itself uses `code: 0` and one of these types:

    | `error_type` | Meaning | Exit |
    |---|---|---|
    | `usage_error` | unknown command or flag, wrong argument count, invalid flag value | 2 |
    | `client_error` | missing settings, unreadable config, connection failure, request timeout, undecodable response | 2 |
    | `wait_failed` | a `--wait` ended in an unsuccessful terminal state | 1 |
    | `wait_timeout` | a `--wait` passed its `--timeout` | 1 |
    | `condition_not_met` | a `--fail-on-…` condition held | 3 |
  - A command that prints a secret shown once (a token or key) also warns on stderr that it will not be shown again.
- **PLAT-FR-093 (exit codes).**

  | Code | Meaning |
  |---|---|
  | 0 | success |
  | 1 | the hub answered an error; or a `--wait` ended unsuccessfully or timed out |
  | 2 | usage or client error: bad flags or arguments, no credentials, connection failure, undecodable response |
  | 3 | a requested condition was not met (`--fail-on-…` flags) |
- **PLAT-FR-094 (waiting).** Commands with `--wait` also take `--timeout` (default `5m`) and `--poll-interval` (default `5s`). Both MUST be positive, or the command fails with a usage error.
  - The CLI starts the operation, then polls its status under one overall deadline.
  - Only terminal states end the wait. Each section names the terminal states of its resources; a state that means "waiting to retry" is never terminal.
  - stdout gets exactly one document: the final record. Progress goes to stderr.
  - A successful terminal state exits 0. An unsuccessful one prints the final record, reports `wait_failed` on stderr and exits 1.
  - On timeout, nothing is printed on stdout; `wait_timeout` is reported and the exit status is 1. The operation itself keeps running on the hub.
  - A hub error during polling ends the wait with that error and exit status 1.
- **PLAT-FR-095 (help).** `parley` alone, and a command group without a subcommand, print help and exit 0 without needing credentials.

#### Scrubbing

- **PLAT-FR-098.** Text that leaves the hub in logs, events, audit records or error messages MUST NOT contain credentials. Before such text is emitted, known secret values (tokens, secrets, forge and gateway credentials) are replaced by `[redacted]`, user information is removed from URLs (`https://user:pass@host/…` becomes `https://host/…`), and the text is cut to at most 1024 bytes on a character boundary. If a secret cannot be removed cleanly, the whole text becomes `[redacted]`. The hub never logs an `Authorization` header or a request body that carries a secret.

### 3.5 REST API

Paths are relative to `/api/v1` unless they start with `/healthz`, `/readyz`, `/version` or `/metrics`. All routes follow the conventions of §3.4: the check order, the error envelope, lists and idempotency are not repeated per route. "Admin" means PLAT-FR-045. "Scope" names what a PAT needs; other credentials are not scope-checked.

**Shapes**

- `User`

  ```json
  {"id":"uuid","username":"alice","email":"a@x","full_name":"","role":"user","status":"active",
   "provider":"github","provider_id":"123","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}
  ```
- `Org`

  ```json
  {"id":"uuid","name":"acme","slug":"acme","url":"","owner_id":"uuid","status":"active",
   "created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}
  ```

  `owner_id` is null when unset.
- `Key`

  ```json
  {"key_id":"Ab3dE6gH","user_id":"uuid","expires_at":null,"revoked_at":null,"created_at":"2026-01-01T00:00:00Z"}
  ```
- `Token`

  ```json
  {"token_id":"k3x9q2mz","user_id":"uuid","name":"agent","permissions":["git:write","changes:write"],
   "paths":["docs/**"],"workspace":"my-ws","expires_at":"2026-04-01T00:00:00Z","revoked_at":null,
   "created_at":"2026-01-01T00:00:00Z"}
  ```

  `paths` and `workspace` are null when unset. A creation response adds `"token":"parley_pat_<token_id>_<secret>"`.

#### `GET /healthz`
- **Purpose:** liveness probe. No auth.
- **Response:** `200` `{"status":"ok"}`.
- **Notes:** not rate-limited. Logged only when `[logging].log_health_probes` is true.

#### `GET /readyz`
- **Purpose:** readiness probe. No auth.
- **Response:** `200` `{"status":"ready"}` when the hub can reach its data; otherwise `503` `{"status":"not ready"}`. The 503 body is not the error envelope.

#### `GET /version`
- **Purpose:** build information. No auth.
- **Response:** `200`

  ```json
  {"version":"1.4.0","commit":"3f2a1bc","build_time":"2026-01-02T03:04:05Z","go_version":"go1.25.1"}
  ```

  `version` is the release version, or `dev` for an untagged build. `go_version` is the toolchain that built the binary.

#### `GET /metrics`
- **Purpose:** Prometheus text exposition. No auth. The metric families are listed in §14.
- **Response:** `200`.

#### `GET /auth/providers`
- **Purpose:** list the login providers. Public.
- **Response:** `200` list of `{"name":"github","authorize_url":"https://github.com/login/oauth/authorize?client_id=…&scope=user%3Aemail"}`, ordered by `name`.

#### `POST /auth/callback`
- **Purpose:** exchange an OAuth code for the user and a new API key. Public.
- **Request body:**

  | Field | Type | Required | Default | Validation |
  |---|---|---|---|---|
  | `provider` | string | yes | | a configured provider |
  | `code` | string | yes | | non-empty |
  | `redirect_uri` | string | yes | | PLAT-FR-031 |
  | `expires` | int | no | 90 | 0, 30, 60 or 90 (days) |
- **Behaviour:** PLAT-FR-031, PLAT-FR-032.
- **Response:** `200`

  ```json
  {"user":{…User…},"api_key":{"key":"parley_<key_id>_<secret>","key_id":"<key_id>","expires_at":"2026-04-01T00:00:00Z"}}
  ```
- **Errors:**

  | Status | `error_type` | When |
  |---|---|---|
  | 400 | `invalid_request` | missing field, bad `expires`, unknown provider, `redirect_uri` not allowed |
  | 401 | `unauthenticated` | the provider rejected the code |
  | 403 | `forbidden` | the user is blocked |
  | 409 | `conflict` | uniqueness conflict (PLAT-FR-032, PLAT-FR-035) |
  | 502 | `upstream_error` | the provider failed, timed out, or returned no user id or verified e-mail |
- **Side effects:** user created or updated; personal org for a new user; the user's older API keys revoked; possible first-admin promotion.

#### `GET /user`
- **Auth / scope:** any user credential; `users:read`.
- **Response:** `200` `User`.

#### `PATCH /user`
- **Auth / scope:** any user credential; `users:write`.
- **Request body:** `{"full_name": string}`, required; `""` clears it.
- **Response:** `200` `User`.

#### `GET /user/orgs`
- **Auth / scope:** any user credential; `orgs:read`.
- **Response:** `200` list of `Org`: the active orgs the caller is a member of, ordered by `name`.

#### `GET /user/keys`
- **Auth / scope:** any user credential; `keys:read`.
- **Response:** `200` list of `Key`, newest first.

#### `POST /user/keys/:key_id/refresh`
- **Auth / scope:** API key only (PLAT-FR-054).
- **Response:** `200` `{"key":"parley_<key_id>_<new secret>","key_id":"…","expires_at":…}`.
- **Errors:** `403 forbidden` (PAT or admin token); `404 not_found`; `409 conflict` (revoked or expired).

#### `DELETE /user/keys/:key_id`
- **Auth / scope:** any user credential; `keys:manage`.
- **Response:** `200` `Key` with `revoked_at` set.
- **Errors:** `404 not_found`.

#### `POST /user/tokens`
- **Purpose:** create a PAT for the caller.
- **Auth / scope:** any user credential; `tokens:manage` and the subset rule.
- **Request body:**

  | Field | Type | Required | Default | Validation |
  |---|---|---|---|---|
  | `name` | string | yes | | 1–255 characters |
  | `permissions` | string[] | yes | | at least one; each a registered scope |
  | `paths` | string[] | no | null | PLAT-FR-057 |
  | `expires` | int | no | 90 | 0, 30, 60 or 90 (days) |
- **Response:** `201` `Token` with `token`. `workspace` is null.
- **Errors:** `400 invalid_request`; `403 insufficient_scope` (missing `tokens:manage`, or the subset rule); `403 forbidden` (admin token).

#### `GET /user/tokens`
- **Auth / scope:** any user credential; `tokens:read`.
- **Response:** `200` list of `Token`: all of the caller's PATs in any state, bound ones included, newest first.

#### `GET /user/tokens/:token_id`
- **Auth / scope:** any user credential; `tokens:read`.
- **Response:** `200` `Token`.
- **Errors:** `404 not_found`.

#### `DELETE /user/tokens/:token_id`
- **Auth / scope:** any user credential; `tokens:manage`.
- **Response:** `200` `Token` with `revoked_at` set.
- **Errors:** `404 not_found`.

#### `PUT /user/tokens/:token_id/permissions`
- **Purpose:** replace the token's scopes.
- **Auth / scope:** any user credential; `tokens:write`; subset rule for added scopes.
- **Request body:** `{"permissions": string[]}`, required. An empty array revokes the token.
- **Response:** `200` `Token` after the change. `revoked_at` is set when the change revoked it.
- **Errors:** `400 invalid_request`; `403 insufficient_scope`; `404 not_found`; `409 conflict` (revoked or expired).

#### `PATCH /user/tokens/:token_id/permissions`
- **Purpose:** add scopes. Scopes already present are kept as they are.
- **Request body:** `{"permissions": string[]}`, at least one entry.
- **Auth, response, errors:** as `PUT`.

#### `DELETE /user/tokens/:token_id/permissions`
- **Purpose:** remove scopes. Removing the last scope revokes the token.
- **Request body:** `{"permissions": string[]}`, at least one entry. No subset check.
- **Auth, response, errors:** as `PUT`.

#### `POST /users`
- **Purpose:** create a user without OAuth. Admin.
- **Request body:** `username`, `email`, `provider`, `provider_id`: strings, all required and non-empty. `full_name`: string, optional.
- **Behaviour:** creates the user with role `user`, status `active`, and the personal org. No first-admin promotion.
- **Response:** `201` `User`.
- **Errors:** `400 invalid_request`; `409 conflict` (username, e-mail, provider identity or personal org taken).

#### `GET /users`
- **Auth:** admin.
- **Query:** `include_blocked=true` adds blocked users. By default only active users are listed.
- **Response:** `200` list of `User`, newest first.

#### `GET /users/:id`
- **Auth:** admin. `:id` is a user selector (PLAT-FR-040), here and on every route below.
- **Response:** `200` `User`.
- **Errors:** `404 not_found`.

#### `PATCH /users/:id`
- **Auth:** admin.
- **Request body:** `{"full_name": string}`, required.
- **Response:** `200` `User`.

#### `DELETE /users/:id`
- **Auth:** admin.
- **Behaviour:** PLAT-FR-048, PLAT-FR-046.
- **Response:** `204`.
- **Errors:** `404 not_found`; `409 conflict` (last admin).

#### `POST /users/:id/promote`
- **Auth:** admin. No body.
- **Response:** `200` `User` with role `admin`. Idempotent (PLAT-FR-047).

#### `POST /users/:id/demote`
- **Auth:** admin. No body.
- **Response:** `200` `User` with role `user`. Idempotent.
- **Errors:** `409 conflict` (last admin).

#### `POST /users/:id/block`
- **Auth:** admin. No body.
- **Response:** `200` `User` with status `blocked`. Idempotent.

#### `POST /users/:id/unblock`
- **Auth:** admin. No body.
- **Response:** `200` `User` with status `active`. Idempotent.

#### `GET /users/:id/keys`
- **Auth:** admin.
- **Response:** `200` list of `Key`, newest first.

#### `DELETE /users/:id/keys/:key_id`
- **Auth:** admin.
- **Response:** `200` `Key` with `revoked_at` set. Idempotent.
- **Errors:** `404 not_found` (user, or key not of that user).

#### `GET /users/:id/tokens`
- **Auth:** admin.
- **Response:** `200` list of `Token`, newest first.

#### `DELETE /users/:id/tokens/:token_id`
- **Auth:** admin.
- **Response:** `200` `Token` with `revoked_at` set. Idempotent.
- **Errors:** `404 not_found` (user, or token not of that user).

#### `POST /orgs`
- **Auth:** admin.
- **Request body:**

  | Field | Type | Required | Validation |
  |---|---|---|---|
  | `name` | string | yes | non-empty after trimming |
  | `slug` | string | yes | 1–128 characters of `[a-z0-9_-]`, starting and ending with a letter or digit |
  | `url` | string | no | |
  | `owner_id` | string | no | an existing user's id |
- **Behaviour:** creates the org. No membership is created.
- **Response:** `201` `Org`.
- **Errors:** `400 invalid_request`; `409 conflict` (name or slug taken).

#### `GET /orgs`
- **Auth:** admin.
- **Query:** `include_blocked=true` adds blocked orgs.
- **Response:** `200` list of `Org`, ordered by `name`.

#### `GET /orgs/:id`
- **Auth / scope:** an admin, or a member of the org; `orgs:read`. `:id` is the UUID or slug, here and below.
- **Response:** `200` `Org`. A blocked org is returned to admins only.
- **Errors:** `404 not_found` for a missing org and for a caller who is neither admin nor member.

#### `PATCH /orgs/:id`
- **Auth:** admin.
- **Request body:** `name` (string, non-empty if present) and `url` (string). At least one is required. `slug` cannot be changed.
- **Response:** `200` `Org`.
- **Errors:** `400 invalid_request`; `404 not_found`; `409 conflict` (name taken).

#### `DELETE /orgs/:id`
- **Auth:** admin.
- **Behaviour:** PLAT-FR-049.
- **Response:** `204`.

#### `POST /orgs/:id/block`
- **Auth:** admin. No body.
- **Response:** `200` `Org` with status `blocked`. Idempotent.

#### `POST /orgs/:id/unblock`
- **Auth:** admin. No body.
- **Response:** `200` `Org` with status `active`. Idempotent.

#### `GET /orgs/:id/members`
- **Auth / scope:** an admin, or a member of the org; `orgs:read`. Non-members get `404 not_found`.
- **Response:** `200` list of `{"org_id","user_id","username","email","created_at"}`, ordered by `username`. `created_at` is the membership time.

#### `PUT /orgs/:id/members/:user_id`
- **Auth:** admin. `:user_id` is a user selector.
- **Response:** `204`. Idempotent: an existing membership is left as it is.
- **Errors:** `404 not_found` (org or user).

#### `DELETE /orgs/:id/members/:user_id`
- **Auth:** admin.
- **Response:** `204`.
- **Errors:** `404 not_found` (org, user or membership).

#### `POST /workspaces/:slug/tokens`
- **Purpose:** mint a PAT bound to the workspace.
- **Auth / scope:** an API key, or an unbound PAT with `tokens:manage` (subset rule applies). Need `owner`.
- **Request body:** as `POST /user/tokens` (`name`, `permissions`, `paths`, `expires`).
- **Behaviour:** PLAT-FR-082.
- **Response:** `201` `Token` with `token` and `workspace` set to the slug.
- **Errors:**

  | Status | `error_type` | When |
  |---|---|---|
  | 403 | `token_bound` | the caller is a bound PAT |
  | 403 | `insufficient_scope` | missing `tokens:manage`, or the subset rule |
  | 404 | `not_found` | no access, or no such workspace |
  | 403 | `forbidden` | admin token; or an org member |
  | 400 | `invalid_request` | body validation |
  | 409 | `workspace_archived` | the workspace is archived |
- **Side effects:** `hub.token.bind`.

#### `GET /workspaces/:slug/tokens`
- **Purpose:** list the tokens bound to the workspace.
- **Auth / scope:** an API key, the admin token, or an unbound PAT with `tokens:read`. Need `owner`.
- **Response:** `200` list of `Token`, oldest first. Works on archived workspaces.
- **Errors:** `403 token_bound`; `403 insufficient_scope`; `404 not_found`; `403 forbidden` (org member).

### 3.6 CLI

Global flags, on every command:

| Flag | Meaning |
|---|---|
| `--endpoint-url <url>` | hub URL (PLAT-FR-090) |
| `--api-key <credential>` | API key or PAT |
| `--json` | with `help` or `--help`: print help as JSON |

Global behaviour (output, exit codes, waiting, path encoding) is in PLAT-FR-090 to PLAT-FR-095. Unless an entry says otherwise, a command prints the route's response body and exits 0, 1 or 2 by those rules. Paths below are relative to `/api/v1`.

#### `parley login`
- **Purpose:** authenticate and save the credentials.
- **Flags:**

  | Flag | Default | Meaning |
  |---|---|---|
  | `--provider <name>` | `github` | provider to sign in with |
  | `--expires <days>` | `90` | key lifetime: 0, 30, 60 or 90 |
  | `--api-key <key \| ->` | | skip the browser and store this credential; `-` reads it from stdin |
- **Browser flow:**
  1. Resolve the hub URL (flag, `PARLEY_ENDPOINT_URL`, file). Fetch `GET /auth/providers` and pick the provider.
  2. Listen on `127.0.0.1` on a free port. Open the provider's authorize URL in the browser with `redirect_uri=http://127.0.0.1:<port>/callback` and a random `state`. If no browser can be opened, print the URL on stderr.
  3. Wait up to 120 seconds for the redirect. A `state` mismatch fails the login. The browser tab shows a short success or failure page.
  4. Call `POST /auth/callback`. Save `endpoint_url`, `api_key` and `user_id` to the config file.
- **Key flow (`--api-key`):** call `GET /user` with the credential and, on success, save it. No key is created or revoked. A PAT needs `users:read`. The admin token cannot be stored this way, because it has no user.
- **Output:** the `User` on stdout; `Logged in as <username>` on stderr.
- **Exit codes:** 0; 1 when the hub refuses; 2 for every client-side failure (no endpoint, timeout, state mismatch).

#### `parley user show`
- **Calls:** `GET /user`.

#### `parley user update`
- **Flags:** `--full-name <string>`, required; an empty string clears it.
- **Calls:** `PATCH /user`.

#### `parley keys list`
- **Calls:** `GET /user/keys`.

#### `parley keys refresh`
- **Calls:** `POST /user/keys/<key_id>/refresh`, with the `key_id` of the configured API key. A configured credential that is not an API key is a client error.
- **Behaviour:** prints the response, then saves the new key to the config file. If the save fails, the response has still been printed, so the new key is never lost; the command warns on stderr and exits 2.

#### `parley keys revoke`
- **Calls:** `DELETE /user/keys/<key_id>` for the configured API key.
- **Behaviour:** prints the response, then clears `api_key` and `user_id` in the config file. stderr tells the user to run `parley login`.

#### `parley tokens list`
- **Flags:** `--workspace <slug>`: list the tokens bound to that workspace instead.
- **Calls:** `GET /user/tokens`, or `GET /workspaces/<slug>/tokens`.

#### `parley tokens create`
- **Flags:**

  | Flag | Default | Meaning |
  |---|---|---|
  | `--name <string>` | required | token name |
  | `--permissions <list>` | required | comma-separated scopes |
  | `--paths <list>` | | comma-separated globs (PLAT-FR-057); repeatable |
  | `--expires <days>` | `90` | 0, 30, 60 or 90 |
  | `--workspace <slug>` | | bind the token to this workspace |
- **Calls:** `POST /user/tokens`, or `POST /workspaces/<slug>/tokens` with `--workspace`.
- **Output:** the `Token` with its value. stderr warns that the value will not be shown again.

#### `parley tokens show <token_id>`
- **Calls:** `GET /user/tokens/<token_id>`.

#### `parley tokens revoke <token_id>`
- **Calls:** `DELETE /user/tokens/<token_id>`.

#### `parley tokens replace <token_id>`
- **Flags:** `--permissions <list>`, required.
- **Calls:** `PUT /user/tokens/<token_id>/permissions`.
- **Notes:** stderr notes when the change revoked the token. The same holds for `remove`.

#### `parley tokens add <token_id>`
- **Flags:** `--permissions <list>`, required.
- **Calls:** `PATCH /user/tokens/<token_id>/permissions`.

#### `parley tokens remove <token_id>`
- **Flags:** `--permissions <list>`, required.
- **Calls:** `DELETE /user/tokens/<token_id>/permissions`.

#### `parley orgs list`
- **Calls:** `GET /user/orgs`: the caller's own orgs.

#### `parley orgs show <org>`
- **Calls:** `GET /orgs/<org>`. `<org>` is an id or slug.

#### `parley orgs members <org>`
- **Calls:** `GET /orgs/<org>/members`.

#### `parley admin users list`
- **Flags:** `--include-blocked`.
- **Calls:** `GET /users`.

#### `parley admin users show <user>`
- **Calls:** `GET /users/<user>`. `<user>` is an id, username or e-mail, here and below.

#### `parley admin users create`
- **Flags:** `--username`, `--email`, `--provider`, `--provider-id` (all required); `--full-name`.
- **Calls:** `POST /users`.

#### `parley admin users update <user>`
- **Flags:** `--full-name <string>`, required.
- **Calls:** `PATCH /users/<user>`.

#### `parley admin users delete <user>`
- **Calls:** `DELETE /users/<user>`. Prints `{}`.

#### `parley admin users promote <user>`
- **Calls:** `POST /users/<user>/promote`.

#### `parley admin users demote <user>`
- **Calls:** `POST /users/<user>/demote`.

#### `parley admin users block <user>`
- **Calls:** `POST /users/<user>/block`.

#### `parley admin users unblock <user>`
- **Calls:** `POST /users/<user>/unblock`.

#### `parley admin orgs list`
- **Flags:** `--include-blocked`.
- **Calls:** `GET /orgs`.

#### `parley admin orgs create`
- **Flags:** `--name`, `--slug` (required); `--url`; `--owner <user id>`.
- **Calls:** `POST /orgs`.

#### `parley admin orgs update <org>`
- **Flags:** `--name`, `--url`; at least one is required (usage error otherwise).
- **Calls:** `PATCH /orgs/<org>`.

#### `parley admin orgs delete <org>`
- **Calls:** `DELETE /orgs/<org>`. Prints `{}`.

#### `parley admin orgs block <org>`
- **Calls:** `POST /orgs/<org>/block`.

#### `parley admin orgs unblock <org>`
- **Calls:** `POST /orgs/<org>/unblock`.

#### `parley admin orgs members list <org>`
- **Calls:** `GET /orgs/<org>/members`.

#### `parley admin orgs members add <org> <user>`
- **Calls:** `PUT /orgs/<org>/members/<user>`. Prints `{}`.

#### `parley admin orgs members remove <org> <user>`
- **Calls:** `DELETE /orgs/<org>/members/<user>`. Prints `{}`.

#### `parley admin keys list <user>`
- **Calls:** `GET /users/<user>/keys`.

#### `parley admin keys revoke <user> <key_id>`
- **Calls:** `DELETE /users/<user>/keys/<key_id>`.

#### `parley admin tokens list <user>`
- **Calls:** `GET /users/<user>/tokens`.

#### `parley admin tokens revoke <user> <token_id>`
- **Calls:** `DELETE /users/<user>/tokens/<token_id>`.

#### `parley version`
- **Purpose:** show the client and server versions. No credential is needed.
- **Behaviour:** when a hub URL resolves, calls `GET /version` with a 5-second timeout. If the hub cannot be reached, stderr gets a warning and `server` is omitted.
- **Output:** `{"cli":{"version":"…","commit":"…"},"server":{"version":"…","commit":"…","build_time":"…","go_version":"…"}}`.
- **Exit codes:** always 0.

#### `parley help [command]`
- **Purpose:** show help. `parley <command> --help` is equivalent. No credential is needed.
- **Output:** text by default. With `--json`, a machine-readable description of the command, or of every command under a group: `name`, `description`, `method`, `path`, `args`, `flags` (`name`, `type`, `required`, `default`, `description`) and `auth` (`none`, `user` or `admin`).

### 3.7 Background behaviour

- **Idempotency records** (PLAT-FR-015): kept for 24 hours, then dropped.
- **Rate-limit counters and the refusal limiter** (PLAT-FR-025, PLAT-FR-087): in memory; reset at restart.

### 3.8 Events

Events are stored and streamed by §14 and carry the common fields listed there (`seq`, `id`, `type`, `time`, `workspace`, `actor_type`, `actor_id`, `resource_type`, `resource_id`, `action`, `session_id`, `metadata`).

| Event | When | Actor | Resource / action / workspace | `metadata` |
|---|---|---|---|---|
| `hub.token.bind` | once per bound token minted, by `POST /workspaces/:slug/tokens` or by fork-create | the minting user; `api_key` or `pat` | `token` / token id / `bind` / the bound slug | `token_id`, `workspace`, `permissions`, `paths` (or null), `expires_at` (or null). Never the token value. |
| `hub.token.bound_refused` | the binding guard refused a request, rate-limited (PLAT-FR-087) | the token's user; `pat` | `token` / token id / `refuse` / the bound slug | `token_id`, `bound_workspace`, `requested_workspace` (when the request named one), `method`, `route`, `status` (403 or 404), `surface` (`api` or `git`), `suppressed` (int) |

`hub.token.bound_refused` is visible to the owner of the bound workspace even when the requested workspace is not. `requested_workspace` is the slug as the caller typed it and says nothing about whether it exists.

User, org, API key and unbound PAT operations emit no events.

### 3.9 Configuration

#### Server configuration file

- **Location.** `$XDG_CONFIG_HOME/config.toml` when `XDG_CONFIG_HOME` is set and non-empty, otherwise `./config.toml`. The same directory holds the bootstrap `admin_token` file. A missing file means all defaults. The file is read once, at start.
- **Environment references.** `${NAME}` inside a string value is replaced by the environment variable `NAME`. A reference to an unset variable is a start error. `$$` yields a literal `$`.
- **Strictness.** Invalid TOML, an unknown section, an unknown key or an invalid value stops the start with an error that names the key.

**`[server]`**

| Key | Type | Default | Meaning |
|---|---|---|---|
| `bind` | string | `"0.0.0.0"` | listen address |
| `port` | int | `8080` | 0–65535; 0 picks a free port |
| `external_url` | string | `""` | the hub's public base URL. Used for the OAuth redirect allowlist, git remote URLs and webhook URLs. |
| `max_body_size` | string | `"1MB"` | a positive integer followed by `KB`, `MB` or `GB` (case-insensitive, powers of 1024) |

**`[database]`**

| Key | Type | Default | Meaning |
|---|---|---|---|
| `path` | string | `./data/parley.db`, or `$XDG_DATA_HOME/parley.db` when `XDG_DATA_HOME` is set | where the hub keeps its data (§15). A bare file name is placed under `$XDG_DATA_HOME` when set. The directory is created with mode 0700. |

**`[workspace]`**

| Key | Type | Default | Meaning |
|---|---|---|---|
| `path` | string | `./data/workspaces`, or `$XDG_DATA_HOME/workspaces` | root directory of the repositories; same resolution as `database.path`; created at start |
| `workers` | int | `4` | concurrent clone workers (§4); at least 1 |

**`[logging]`**

| Key | Type | Default | Meaning |
|---|---|---|---|
| `level` | string | `"info"` | `trace`, `debug`, `info`, `warn` or `error` (case-insensitive) |
| `log_health_probes` | bool | `false` | log `/healthz` and `/readyz` requests at debug level |

Logs are JSON lines on stderr. Each request produces one access-log line with `method`, `path`, `status`, `duration_ms` and `request_id`.

**`[limits]`**

| Key | Type | Default | Meaning |
|---|---|---|---|
| `requests_per_minute` | int | `600` | per-credential request rate (PLAT-FR-025); 0 turns it off |

**`[[oauth.providers]]`** (repeatable)

| Key | Type | Default | Meaning |
|---|---|---|---|
| `name` | string | required | `github` or `google`; unique |
| `client_id` | string | required | |
| `client_secret` | string | required | |
| `authorize_url`, `token_url`, `userinfo_url` | string | the provider's public endpoints | overrides, for example for GitHub Enterprise |

**Keys defined in other sections**

| Section | Defined in | Covers |
|---|---|---|
| `[forks]` | §4 | task fork TTLs and limits (`default_ttl`, `max_ttl`, `max_per_workspace`) |
| `[limits]` (other keys) | the section that owns each limit | size and quantity limits |
| `[events]`, `[audit]`, `[sessions]` | §14 | event-log retention and stream limits, audit and session retention |
| `[git]` | §5 | git server timeouts and push size |
| `[scm]` | §6 | source control API size limits and read timeout |
| `[changes]` | §7 | mergeability debounce, stack depth |
| `[land]` | §8 | landing attempts, timeout, workers, committer identity |
| `[awareness]` | §9 | claim TTLs and limits |
| `[secrets]` | §11 | encryption key file |
| `[sandbox]`, `[[sandbox.profiles]]` | §13 | sandboxes |

#### Server flags

`--admin-email <string>` and `--reset-admin-token` (PLAT-FR-001, PLAT-FR-003).

#### Environment variables

| Variable | Read by | Meaning |
|---|---|---|
| `XDG_CONFIG_HOME` | `parleyd` | directory of `config.toml` and `admin_token`; default: the working directory |
| `XDG_DATA_HOME` | `parleyd` | base directory for `database.path` and `workspace.path` |
| any `${NAME}` referenced in `config.toml` | `parleyd` | substituted into the configuration (for example OAuth client secrets) |
| `PATH` | `parleyd` | locates `git` and, with sandboxes, the container engine |
| `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` | `parleyd` | standard proxy behaviour for outgoing calls (identity providers, origins, forges) |
| `PARLEY_CONFIG_DIR` | `parley` | CLI config directory; default `~/.parley` |
| `PARLEY_ENDPOINT_URL` | `parley` | hub URL |
| `PARLEY_API_KEY` | `parley` | credential |

Retention settings read from the environment are listed in §14. The hub never reads forge tokens from its environment (§12).

### 3.10 Edge cases and error handling

- **Unknown paths and wrong methods** are answered before authentication (`404`, `405`). The set of routes is public knowledge, so this leaks nothing. A bound token gets the same answers there as any caller.
- **Bound tokens.**
  - A bound token's `tokens:*`, `keys:*`, `users:*` and `orgs:*` scopes have no use: identity routes answer it `403 token_bound`. `tokens:manage` matters only for minting through fork-create.
  - The owner can change a bound token's permissions or revoke it with an API key. The binding and `paths` stay.
  - A bound token whose owner loses access to the workspace (for example the owner leaves the org of a fork's parent) gets what the owner would get. The binding never grants access; it only removes it.
  - A token bound to W does not reach forks of W that someone else, or another token, created.
  - Deleting a user deletes the user's tokens, bound ones included.
- **Path-scoped tokens.**
  - `paths` on a token without any write scope is accepted and has no effect.
  - A fork-create call by a path-scoped token can mint only tokens with covered `paths`; omitting `paths` in the request is refused, not defaulted.
- **Admin token.**
  - It is not a user: `/user…` routes and the bound-token mint answer `403 forbidden`.
  - It passes every scope check and has `admin` access on every workspace.
  - It may list bound tokens of any workspace.
- **OAuth.**
  - Every login revokes the user's other API keys, including one in use on another machine.
  - A username change at the provider that collides with another user keeps the old username silently.
- **Idempotent replays of token creation** return the token object without the `token` value. A client that lost the value must revoke the token and create a new one.
- **Rate limiting** counts refused requests too. The CLI does not retry on 429.
- **Readiness** reports only whether the hub can reach its own data. It does not check origins, forges, identity providers or the sandbox provider.

---

## 4. Workspaces and task forks

### 4.1 Purpose and scope

A **workspace** is the hub's record of one git repository. It has a globally unique slug, an owner, an organization, a status, settings, and a copy of the repository that the hub keeps as a bare repository. Agents and people reach that copy through the git server (§5) and the source control API (§6). Work on it is proposed as changes (§7) and lands under policy (§8).

There are two kinds of workspace:

- A **repository** workspace is registered from a git URL, its **origin**. The hub clones origin, can sync from it, and can push branches back to it.
- A **task fork** is a cheap, short-lived workspace created from a repository workspace, its **parent**. It is created in one call, is ready at once whatever the size of the repository, and expires. A fork is where one agent does one task.

In scope here: the workspace entity and its response object; workspace settings (keys, types, defaults, validation); the status state machine; git credentials for origin; create, list, get, update, archive, reactivate, sync, reclone, push to origin and delete; task forks in full; the `parley workspace` and `parley fork` commands; the `hub.workspace.*` and `hub.fork.expire` events.

Out of scope, with hand-off points only: the git protocol and `hub.git.push` (§5); refs, commits and files over the API (§6); changes (§7); landing, protected-branch enforcement and policy (§8); carry-patch sync, patches and rebuild (§10); secret and variable storage (§11); sessions and the event log (§14).

### 4.2 User stories

**WS-US-01: Register a repository (Developer).** As a developer, I want to register a repository under a slug so that the hub clones it and agents can work on it.
- `POST /workspaces` with `slug` and `git_url` answers `201` with `status: "pending"` and `access: "owner"`.
- The clone runs in the background: `pending` → `cloning` → `ready` or `failed`.
- `parley workspace create --wait` returns when the workspace is `ready` (exit 0) or `failed` (exit 1).

**WS-US-02: Private repository (Developer).** As a developer, I want to give a token or a username and password at creation so that the hub can clone, sync and push a private repository.
- The credential is checked against origin before anything is stored. A refused credential creates nothing.
- The credential is stored as workspace secrets and never appears in a response, an event or a log.

**WS-US-03: See my workspaces (Developer, Org member, Agent).** As a user, I want to list and read the workspaces I may see.
- The list holds my own workspaces and those of my organizations. Each item says what my access is.
- Archived workspaces and forks are hidden unless I ask for them.

**WS-US-04: Change a workspace (Developer).** As an owner, I want to change the display name, description, organization and settings.
- Only the fields I send change. `null` returns a field to its default. Settings merge key by key.
- Immutable fields are refused.

**WS-US-05: Keep the hub current with origin (Developer, Agent).** As an owner, I want one call that brings origin's new commits into the hub.
- Branches with no local commits fast-forward.
- A protected branch with landed commits that origin does not have yet is reported as `ahead`. That is normal and is not an error.
- A branch that moved on both sides is reported as `diverged` and is left untouched. The call still answers `200` with a result for every branch.
- `parley workspace sync --fail-on-diverged` exits 3 when any branch diverged.

**WS-US-06: Publish a branch (Agent, Developer).** As a writer, I want to push a branch from the hub to origin, for example to publish landed work or open a pull request on the forge.
- The push never overwrites commits on origin that the hub has not seen.
- Every attempt is recorded as an event, also when I disconnect.

**WS-US-07: Retire and revive (Developer, Operator).** As an owner, I want to archive a workspace, reactivate it later, and finally delete it.
- Archive ends the workspace's active agent sessions and makes its forks read-only.
- Delete is possible only on an archived repository workspace. It removes the workspace, its forks and everything they own, in one step or not at all.

**WS-US-08: Recover a damaged copy (Developer).** As an owner, I want `reclone` to discard the hub's copy and clone origin again.

**WS-US-09: Fork for a task (Agent).** As an agent, I want my own disposable copy of a repository in one call, so that I can start work immediately and cannot disturb anyone else.
- `POST /workspaces/:slug/forks` answers `201` with a workspace in `status: "ready"`, its git remote URL and, if I asked, a token bound to the fork.
- The call takes the same time for a small and a very large repository.
- The fork shows the parent's branches, read-only and always current. I push my work to new branches.

**WS-US-10: Contribute without write access (Org member).** As a member with read-only access to a workspace, I want to fork it, push to my fork and have a change opened against the parent.
- A member may fork any workspace they can read and is the owner of the fork.
- With `auto_change`, the first push of a new branch opens a change that targets the parent (§7).

**WS-US-11: Forks clean up after themselves (Operator, Developer).** As an operator, I want forks to disappear without anyone deleting them.
- A fork is deleted when its `expires_at` passes.
- A fork whose changes have all landed or been abandoned is deleted one hour later.
- An owner can extend a fork or delete it early.

**WS-US-12: Hand an agent a narrow credential (Developer, Orchestrator).** As an orchestrator, I want fork creation to mint a token that works only on that fork, optionally limited to some paths, so that I can give it to an untrusted agent.
- The token is shown once, in the fork response.
- It is revoked when the fork is deleted or expires.

### 4.3 Concepts and data model

#### 4.3.1 Workspace

| Field | Type | Default | Meaning and constraints |
|---|---|---|---|
| `slug` | string | n/a | Globally unique. Immutable. Rules in WS-FR-009. A fork's slug is `<parent slug>--<fork name>`. |
| `kind` | enum | `repository` | `repository` or `fork`. Immutable. |
| `workspace_mode` | enum | `standard` | `standard` or `carry_patch` (§10). Immutable. Always `standard` for a fork. |
| `git_url` | string or null | n/a | Origin. `https://<host>/<path>` or `git@<host>:<path>`. Immutable. `null` for a fork. |
| `default_branch` | string or null | origin's default branch | The workspace's main branch. Given at creation or taken from origin when the clone completes; `null` until then. Immutable afterwards. For a fork: the branch it was forked from. |
| `owner_id` | string | caller's user id | Immutable. |
| `org_id` | string | owner's personal org | Canonical organization id. A fork always has its parent's organization. |
| `status` | enum | `pending` | `pending`, `cloning`, `ready`, `failed`, `archived` (4.3.4). |
| `display_name` | string | slug | At most 128 characters. Empty is stored as the slug. |
| `description` | string | `""` | At most 1024 characters. |
| `head_sha` | string or null | null | Current head of `default_branch` in the hub. `null` until the first clone completes. |
| `origin_head_sha` | string or null | null | Origin's head of `default_branch` when the hub last fetched or pushed. `null` for a fork. |
| `clone_error` | string or null | null | Text of the last failed clone. Cleared when a clone starts, on archive and on reactivate. |
| `last_sync_at` | timestamp or null | null | Time of the last successful sync. |
| `sync_error` | string or null | null | Text of the last failed sync. Cleared by a successful sync and by reclone. |
| `upstream_url`, `integration_branch`, `upstream_head_sha` | string or null | null | Carry-patch fields. Set only for `carry_patch` workspaces. Defined in §10. |
| `parent_slug` | string or null | null | Fork only: the slug of the parent. Immutable. |
| `fork_point` | object or null | null | Fork only: `{ "ref": "<branch>", "sha": "<40 hex>" }`, the parent branch the fork was taken from and its head at that moment. Immutable. |
| `expires_at` | timestamp or null | null | Fork only: when the fork is deleted. |
| `auto_change` | bool or null | null | Fork only: whether the first push of a new branch opens a change. |
| `purpose` | string or null | null | Fork only: free text that says what the fork is for. At most 1024 characters. |
| `session_id` | string or null | null | Fork only: the agent session (§14) that created the fork. |
| `settings` | object | defaults | Settings document (4.3.3). |
| `created_at`, `updated_at` | timestamp | now | |

The hub also remembers, for each branch of a repository workspace, the last head it saw on origin (the **origin head** of that branch). Sync and a successful push to origin update it. `origin_head_sha` is that value for the default branch.

#### 4.3.2 Workspace response object

Every route that returns a workspace returns an object with all fields of 4.3.1 (nullable ones as `null`, never omitted) and these computed fields:

| Field | Type | Meaning |
|---|---|---|
| `org_slug` | string | Slug of the workspace's organization. |
| `hub_url` | string or null | The git remote URL of the workspace on the hub: `<server.external_url>/git/<org_slug>/<slug>.git`, with a trailing `/` of `external_url` removed. `null` when `server.external_url` is not set. |
| `access` | string | The caller's level: `admin` (admin token), `owner` or `member`. It describes the caller, not a token's scopes. |
| `settings` | object | Every setting key with its effective value. |

#### 4.3.3 Settings

Settings are a flat document of named keys. A key that was never set has its default. A response always shows every key with its effective value.

| Key | Type | Values | Default | Defined in |
|---|---|---|---|---|
| `sync_enabled` | bool | | `true` | This section: `false` refuses every sync. |
| `protected_branches` | list of string | 0 to 50 branch globs | `[<default branch>]` | §8 |
| `land.strategy` | enum | `rebase`, `squash` | `rebase` | §8 |
| `land.by_author` | bool | | `true` | §8 |
| `land.delete_source` | bool | | `true` | §8 |
| `land.allow_override` | bool | | `true` | §8 |
| `changes.require_intent` | bool | | `true` | §7 |
| `changes.auto_restack` | bool | | `true` | §7 |
| `audit_fetches` | bool | | `false` | §5 |
| `forge_api_url` | URL string | `""` or an absolute `https://` URL | `""` | §12 |
| `upstream_forge_api_url` | URL string | same | `""` | §12 |
| `patch_branch_source` | enum | `hub`, `origin` | `hub` | §10 |
| `patch_divergence_policy` | enum | `replace`, `report` | `replace` | §10 |
| `push_patches_to_origin` | bool | | `false` | §10 |
| `auto_rebuild_after_push` | bool | | `true` | §10 |
| `auto_rebuild_after_sync` | bool | | `true` | §10 |
| `rebuild_strategy` | enum | `rebase`, `merge` | `rebase` | §10 |
| `rebuild_fail_mode` | enum | `fail_fast`, `continue` | `fail_fast` | §10 |
| `rebuild_push_integration_branch` | bool | | `false` | §10 |
| `squash_merge_detection` | enum | `ancestry_only`, `content_based`, `both` | `both` | §10 |

This section defines the keys, their types, defaults and validation. What `protected_branches`, the `land.*` keys and `changes.require_intent` do is defined in §7 and §8.

The default of `protected_branches` follows the workspace's `default_branch`. While `default_branch` is still `null` the effective value is `[]`. Once the key is set explicitly, including to `[]`, the stored list is used as it is.

**Applying a `settings` value** (create and update):

- `settings: null` returns every key to its default.
- Otherwise the value must be a JSON object, else `400`.
- An unknown key is `400` (`unknown setting "<key>"`).
- A key with value `null` returns to its default.
- A bool key needs a JSON boolean. An enum key needs one of the listed strings.
- A URL key needs a JSON string that, after trimming surrounding whitespace, is empty or an absolute `https://` URL with a host and without userinfo, query or fragment. The trimmed value is stored.
- `protected_branches` needs a JSON array of at most 50 distinct strings. Each is a branch glob of 1 to 255 characters: a valid branch name (WS-FR-011) in which `*` and `**` may also appear. How globs match is defined in §8.
- Keys not given keep their current value (on create: the default).
- The request is all-or-nothing. When several keys are invalid, the error names the first invalid key in the order of the table above.

A fork has no settings of its own. Its `settings` shows the parent's effective values, and a request to change them is refused (WS-FR-061).

#### 4.3.4 Status state machine

| From | Trigger | To | Notes |
|---|---|---|---|
| (none) | create a repository workspace | `pending` | Clone work recorded. |
| (none) | create a fork | `ready` | No clone. |
| `pending` | clone work starts | `cloning` | `clone_error` cleared. |
| `cloning` | a valid existing copy is found | `ready` | No clone. `head_sha` is read from the copy. |
| `cloning` | clone succeeded | `ready` | `head_sha` and `default_branch` recorded. |
| `cloning` | clone failed | `failed` | `clone_error` set. |
| `ready`, `failed` | archive | `archived` | |
| `archived` | reactivate | `pending` | The kept copy is found valid in the normal case, so the workspace is `ready` again quickly. |
| `ready`, `failed` | reclone | `pending` | The copy is discarded. |
| `archived` | delete | (removed) | Repository workspaces only. |
| any | delete or expiry of a fork | (removed) | |

A workspace in `pending` or `cloning` is in exclusive maintenance. So is a workspace for the duration of a reclone, an archive or a delete call. Sync never changes `status`.

A fork is always `ready`. It cannot be archived. Whether it is writable depends on its parent (WS-FR-066).

#### 4.3.5 Git credentials for origin

Credentials for origin are workspace secrets (§11). They are read on every use: clone, sync, archive, reclone, push to origin and the forge client. Create stores them. Afterwards they change only through the secrets API.

Resolution:

1. `GIT_PAT` is set: HTTP Basic authentication with user name `x-token-auth` and the token as password.
2. Otherwise `GIT_USERNAME` and `GIT_PASSWORD` are both set: HTTP Basic authentication with them.
3. Otherwise: no credential (anonymous). A user name without a password counts as no credential.

A failure to read the secrets is an error and the operation that needed them fails. It never falls back to anonymous.

A carry-patch workspace has a second credential for its upstream, defined in §10.

A fork has no origin and uses no git credential.

#### 4.3.6 Task forks

A task fork is a workspace with `kind: "fork"` and a `parent_slug`.

- **Parent.** The parent is a repository workspace in `ready` status. A fork cannot be forked.
- **Name and slug.** The slug is `<parent slug>--<name>`. `name` defaults to a generated 8-character id of lower-case letters and digits. Plain slugs never contain `--`, so a fork slug cannot collide with a repository slug and the parent can be read from it.
- **Fork point.** `ref` names a branch of the parent and defaults to the parent's default branch. The fork records `fork_point` (that branch and its head at creation). The branch is the fork's **base branch** and its `default_branch`.
- **Tracking branches.** Every branch that exists in the parent when the fork is created appears in the fork as a tracking branch. A tracking branch is read-only in the fork and always equals the parent's branch of the same name: when the parent's branch moves, the fork's view moves with it, with no sync call. If the parent's branch is deleted, the tracking branch disappears. Branches created in the parent later do not appear in the fork. The parent's tags are visible in the fork under the same rules.
- **Own branches.** Agents push their work to new branch names. Own branches belong to the fork alone and are not visible in the parent.
- **No copy.** Creating a fork copies no history. All commits of the parent that are reachable from its branches and tags can be fetched from the fork.
- **Automatic change.** With `auto_change` (default true), the first push of a new branch to the fork opens a change (§7) in the parent, with the fork branch as source and the fork's base branch as target.
- **Lifetime.** A fork has an `expires_at` and is deleted when it passes. It can also be deleted early by the hub once its work has landed (WS-FR-070).
- **Ownership and access.** The caller owns the fork. The fork is in the parent's organization. Every reader of the parent can read the fork.
- **Configuration data.** Variables and secrets resolve in the order fork → parent workspace → organization → user (§11).
- **Carry-patch parents.** A fork of a carry-patch workspace is a plain fork. It has no patches, no upstream and no integration branch of its own. The parent's integration branch and patch branches are ordinary tracking branches in it.

### 4.4 Functional requirements

**Authentication and access**

- WS-FR-001 Every route follows the global check order (§3): authenticate (`401`), PAT scope (`403`), workspace access (`404` or `403`), validation (`400`), state (`409` or `400`), act.
- WS-FR-002 Scopes are `workspaces:read` and `workspaces:write`. A read check accepts either. Only PATs are scope-checked.
- WS-FR-003 Access to a repository workspace: an admin token has `admin`; the owner has `owner`; a member of the workspace's organization has `member`. Anyone else gets `404 workspace not found`, identical to an unknown slug. A `member` is read-only: a write route answers `403 forbidden`. Two routes are open to members on purpose: fork creation and fork listing.
- WS-FR-004 Access to a fork: the user who created it has `owner`. The owner of the parent also has `owner`, so that they can inspect and remove forks of their workspace. Members of the organization have `member`. Admin tokens have `admin`.
- WS-FR-005 A token bound to workspace W (§3) is valid on the routes of W and of the forks that this token created. On any other workspace it gets `404 workspace not found`. `GET /workspaces` shows it W and those forks. `POST /workspaces` answers `403`, `error_type: token_bound`.
- WS-FR-006 An admin token cannot create a workspace or a fork, because both need a user as owner: `403 forbidden`.

**State preconditions**

- WS-FR-007 Routes that need the repository answer by status as follows, unless the route says otherwise:

  | Status | Answer | `error_type` |
  |---|---|---|
  | `pending`, `cloning`, or a reclone, archive or delete is running | `409` | `workspace_busy` |
  | `failed` | `409` | `workspace_not_ready` |
  | `archived` | `409` | `workspace_archived` |

- WS-FR-008 No workspace request fails only because another request is running on the same workspace. There is no per-workspace lock that a caller can observe. Every ref update by sync, landing, a push or the API is a compare-and-swap on that ref. `409 workspace_busy` is returned only in the cases of WS-FR-007.

**Validation rules**

- WS-FR-009 Slug of a repository workspace: 3 to 64 characters; lower-case letters, digits and hyphens; starts with a letter; ends with a letter or digit; no `--`.
- WS-FR-010 `git_url` is `https://<host>/<path>` or `git@<host>:<path>` with non-empty host and path. `http://`, `ssh://`, `file://` and other forms are refused. An `https` URL with userinfo is refused; credentials go in the credential fields. The check is on the form only; nothing is resolved.
- WS-FR-011 A branch name must be a valid git branch name: not empty; no control characters, space, `~`, `^`, `:`, `?`, `*`, `[` or `\`; no `..`, `//` or `@{`; not `@`; does not start with `-` or `/`; does not end with `/`, `.` or `.lock`; no path component that starts with `.` or ends with `.lock`.
- WS-FR-012 A fork name is 1 to 32 characters: lower-case letters, digits and hyphens; starts and ends with a letter or digit; no `--`. A fork slug may therefore be up to 98 characters long.
- WS-FR-013 A duration (`ttl`, token `expires`) is a string of a positive whole number and a unit `m`, `h` or `d`, for example `90m`, `24h`, `7d`.

**Create**

- WS-FR-014 Creating a repository workspace needs `slug` and `git_url`. Unknown body fields are ignored.
- WS-FR-015 Credential fields: `git_pat` excludes `git_username` and `git_password`; user name and password come together; an empty string is an error; any credential needs an `https` `git_url`.
- WS-FR-016 A given credential is checked by listing origin's refs with it, with a 10-second deadline, before anything is stored. Any failure answers `400`, `error_type: credential_check_failed`, and creates nothing. The message does not repeat the credential.
- WS-FR-017 `workspace_mode` defaults to `standard`. `carry_patch` needs `upstream_url` and takes an optional `integration_branch`; both are validated per §10. Both are refused for `standard`.
- WS-FR-018 `org_id` takes an organization id or slug. The caller must be a member, else `403 forbidden`. An unknown organization is `400`. Without `org_id` the workspace goes to the caller's personal organization.
- WS-FR-019 A slug that is in use answers `409 conflict`. Of two concurrent creates with the same slug, one succeeds and the other gets this `409`.
- WS-FR-020 Create is all-or-nothing: the workspace, its credential secrets and its clone work are recorded together. If any of them cannot be stored, nothing is created and the answer is `500`.
- WS-FR-021 Create answers `201` with `status: "pending"` and `access: "owner"`, and emits `hub.workspace.create`.

**Clone**

- WS-FR-022 A clone brings all branches and tags of origin with full history into the hub's bare copy. On success the hub records `default_branch` (origin's default branch unless one was given), `head_sha` and the origin head of every branch, then sets `ready`.
- WS-FR-023 If `default_branch` was given at creation and origin has no such branch, the clone fails with a `clone_error` that says so.
- WS-FR-024 A failed clone sets `failed` and `clone_error`, and leaves no partial copy behind. `clone_error` never contains a credential; userinfo in URLs is removed. A failed clone is not retried automatically. The owner recovers with reclone.
- WS-FR-025 Clone work is durable. When the hub starts, every workspace in `pending` or `cloning` is taken up again, in `created_at` order, before the hub serves requests for it.
- WS-FR-026 When clone work finds an existing copy, it checks whether the copy is valid: it opens as a repository, its default branch resolves to a commit, and its origin is the workspace's `git_url`. A valid copy is kept and the workspace becomes `ready` with `head_sha` read from the copy. Anything else is discarded and cloned fresh. A copy left by an earlier workspace with the same slug is never adopted.
- WS-FR-027 At most `[workspace].workers` clones run at the same time. Further clone work waits in order.

**List and get**

- WS-FR-028 The list holds the workspaces the caller can read: own workspaces as `owner`, workspaces of the caller's organizations as `member` (ownership wins), and every workspace as `admin` for an admin token. A bound token sees the set of WS-FR-005.
- WS-FR-029 Archived workspaces are left out unless `include_archived=true`. Forks are left out unless `include_forks=true` or `parent` is given. Order is `created_at` descending, then `slug` ascending.

**Update**

- WS-FR-030 The mutable fields of a repository workspace are `display_name`, `description`, `org_id` and `settings`. The body must be a JSON object with at least one of them. Unknown fields are ignored.
- WS-FR-031 An immutable field in the body is refused with `400`, whatever its value: `slug`, `kind`, `git_url`, `default_branch`, `owner_id`, `workspace_mode`, `upstream_url`, `integration_branch`, `parent_slug`, `fork_point`.
- WS-FR-032 `display_name: null` or `""` resets it to the slug. `description: null` resets it to `""`. `org_id: null` moves the workspace to its owner's personal organization. For any other `org_id`, the workspace's owner must be a member of the target organization, else `403 forbidden`. `settings` is applied per 4.3.3.
- WS-FR-033 Moving a workspace to another organization moves its forks with it.
- WS-FR-034 An archived workspace cannot be updated: `409 workspace_archived`. A workspace in any other status can.
- WS-FR-035 A successful update emits `hub.workspace.update` with the list of fields that were given.

**Sync from origin**

- WS-FR-036 Sync needs a repository workspace in `ready` status with `sync_enabled` true. With `sync_enabled` false it answers `400`, `error_type: sync_disabled`.
- WS-FR-037 Sync fetches origin with the origin credential and records the origin head of every branch. New tags are created. An existing tag is never moved.
- WS-FR-038 For each branch that exists on origin, sync compares the hub's head with origin's head and reports one result:

  | Result | Condition | Effect in the hub |
  |---|---|---|
  | `created` | The hub has no such branch. | The branch is created at origin's head. |
  | `up_to_date` | Both heads are equal. | None. |
  | `fast_forward` | The hub's head is an ancestor of origin's. | The branch moves to origin's head. |
  | `ahead` | Origin's head is an ancestor of the hub's. | None. |
  | `diverged` | Each side has commits the other lacks. | None. |
  | `reset` | The branch diverged and `reset_to_origin=true` was given. | The branch moves to origin's head. The hub's commits on it are no longer on the branch. |

- WS-FR-039 `ahead` is the normal state of a protected branch (§8) that has landed commits which nobody has pushed to origin yet. It is not an error and sets no `sync_error`.
- WS-FR-040 `diverged` changes nothing for that branch and does not fail the call. The other branches are still processed.
- WS-FR-041 Sync is one of the two ways a protected branch may move (§8). It moves one only by `created`, `fast_forward` or `reset`.
- WS-FR-042 Branches that exist only in the hub are not touched and not reported. A branch that was deleted on origin is not deleted in the hub.
- WS-FR-043 Each branch move is a compare-and-swap. If a branch moves in the hub while sync runs, sync evaluates that branch again against the new head. Two concurrent syncs are safe.
- WS-FR-044 A sync that completes sets `last_sync_at`, clears `sync_error`, updates `head_sha` and `origin_head_sha`, and emits `hub.workspace.sync`. This holds also when branches are `ahead` or `diverged`.
- WS-FR-045 A sync that cannot fetch (credential, network, origin refusal) moves nothing, sets `sync_error` and answers `502 upstream_error`. No event is emitted.
- WS-FR-046 A sync runs to completion when the caller disconnects.
- WS-FR-047 A branch that sync moves and that is the source of an open change gets a new version, as for any new head (§7).
- WS-FR-048 For a carry-patch workspace, sync also fetches the upstream, refreshes patches and may start a rebuild. This and the extra response fields are defined in §10.

**Push to origin**

- WS-FR-049 Push to origin sends exactly one branch of the hub, `refs/heads/<name>`, to the same branch on origin with the origin credential. It needs a repository workspace in `ready` status.
- WS-FR-050 Without `force`, the push is fast-forward only: origin accepts it only if its branch is an ancestor of the hub's.
- WS-FR-051 With `force`, the push uses force-with-lease. The lease is the origin head of that branch that the hub last saw (or "no such branch" if it never saw one). Origin's branch is replaced only if it still has that value. If origin moved since, nothing is overwritten and the answer is `409`, `error_type: origin_moved`. The hub never sends a blind force.
- WS-FR-052 A forced push of a protected branch (§8) is refused with `400`, `error_type: protected_branch`. The landed history of a protected branch only ever goes to origin as a fast-forward.
- WS-FR-053 On a carry-patch workspace, the integration branch and registered patch branches are refused (`400`, `error_type: integration_branch` or `patch_branch`). They reach origin through the settings of §10.
- WS-FR-054 The push has a 2-minute deadline. It runs to completion or to its deadline when the caller disconnects.
- WS-FR-055 After a successful push the hub records the pushed commit as the origin head of that branch.
- WS-FR-056 Each push handed to origin emits exactly one `hub.workspace.push` event, on success and on failure. Refusals before the push emit none.
- WS-FR-057 Every message from this route is scrubbed: the credential is replaced by `[redacted]`, URL userinfo is removed, and the text is cut to 1024 characters.

**Archive, reactivate, reclone, delete**

- WS-FR-058 Archive applies to a repository workspace in `ready` or `failed`. It first tries to push every branch to origin, fast-forward only, as a safety copy. The outcome of that push never blocks the archive. The hub keeps its copy of the repository, so unpushed branches and change versions survive.
- WS-FR-059 An archived workspace is not served over git (§5), accepts no writes, and is hidden from the default list. It can still be read with `GET`.
- WS-FR-060 Archive force-closes every active agent session of the workspace and of its forks with reason `workspace archived` (§14). A failure to close a session does not fail the archive. Tokens, secrets, variables, patches and changes are untouched.
- WS-FR-061 Operations that do not apply to a fork are refused with `400` and one of these types:

  | Operation on a fork | `error_type` |
  |---|---|
  | sync, reclone, push to origin | `fork_has_no_origin` |
  | archive, reactivate, change of `org_id` or `settings`, creating a fork of it | `not_supported_on_fork` |

- WS-FR-062 Reactivate applies to an `archived` workspace, else `409 conflict`. It sets `pending` and records clone work, which finds the kept copy (WS-FR-026).
- WS-FR-063 Reclone applies to a repository workspace in `ready` or `failed`. It is refused with `409`, `error_type: has_forks`, while the workspace has forks, because forks depend on the parent's copy. It first tries to push every branch to origin, fast-forward only; a failure of that push does not stop it. It then discards the hub's copy, clears `sync_error`, `origin_head_sha` and all origin heads, sets `pending` and records clone work. Whatever did not reach origin is lost, including refs under `refs/hub/`. Change records stay; what a change does when its commits are gone is defined in §7.
- WS-FR-064 Delete of a repository workspace applies only when it is `archived`, else `409`, `error_type: workspace_not_archived`.
- WS-FR-065 Delete is transactional. In one step it removes the workspace and its forks, revokes every token bound to any of them, and removes everything they own: workspace secrets and variables, patches, changes, policy, claims and settings. If the step fails the answer is `500` and nothing has changed. Only after it succeeded does the hub force-close remaining active sessions (reason `workspace deleted`) and discard the repository data. The slug is free again. Audit records and the event log are kept.

**Task forks**

- WS-FR-066 A fork is writable only while its parent is `ready`. While the parent is `archived`, every write to the fork (git push, source control API, update of the fork) answers `409`, `error_type: parent_archived`. Reads and fetches keep working, and delete and expiry keep working. While the parent is in a reclone, archive or delete, the fork answers `409 workspace_busy`.
- WS-FR-067 Fork creation is synchronous. The answer is `201` with the fork in `status: "ready"`, and the fork is usable over git and the API from that moment.
- WS-FR-068 Fork creation copies no history. Its duration and the storage it adds MUST NOT depend on the size of the parent repository (number of commits, files, branches of history, or bytes). The target is under one second for any repository; the measured bound is set in §15. The storage a fork uses grows only with what is pushed to it.
- WS-FR-069 `ttl` defaults to `[forks].default_ttl` and may not exceed `[forks].max_ttl`. The shortest `ttl` is 5 minutes. `expires_at` is the creation time plus `ttl`.
- WS-FR-070 Early expiry. When `[forks].expire_on_land` is true, a fork is deleted before `expires_at` once all of these have held for one hour without interruption: the fork is the source of at least one change; every such change is `landed` or `abandoned`; every own branch that still exists in the fork is the source of such a change. A push to the fork, a new change from it, or a restored change starts the hour again.
- WS-FR-071 The owner can set a new lifetime at any time: `ttl` in an update sets `expires_at` to now plus `ttl`, within the same bounds as at creation. This may also shorten the lifetime.
- WS-FR-072 A parent may have at most `[forks].max_per_workspace` forks at one time. A create beyond that answers `429`, `error_type: fork_limit_reached`. Forks that were deleted or expired do not count.
- WS-FR-073 A tracking branch cannot be written in the fork by any means (git push, API ref update, API commit, delete). The refusal is `403`, `error_type: tracking_branch`; over git it is a per-ref rejection (§5). A fork cannot create a branch with the name of a tracking branch.
- WS-FR-074 A fork's `head_sha` is the parent's current head of the fork's base branch. `fork_point.sha` does not change.
- WS-FR-075 With `auto_change` true, the first push that creates a branch in the fork opens a change in the parent with that branch as source and the fork's base branch as target. Title, intent, session and draft state come from push options or defaults (§7). Later pushes to the same branch add versions. With `auto_change` false, nothing is opened unless the push asks for it (§7).
- WS-FR-076 Fork creation may mint a token. When the body has `token`, the hub creates a PAT owned by the caller and bound to the fork, and returns its secret once, in this response only.
  - `permissions` is a non-empty list of scopes. If the caller authenticates with a PAT, the list must be a subset of that PAT's scopes, else `403 forbidden`.
  - `paths` is an optional list of path globs that limits what the token may write (§8). If the caller's own token is path-limited, `paths` is required and may not be wider.
  - `expires` is an optional duration. Without it the token lives as long as the fork.
  - `name` defaults to `fork:<fork slug>`.
  - The token is revoked when the fork is deleted or expires.
- WS-FR-077 `session_id` at creation must name an active agent session that the caller may use (§14), else `400`. It is stored on the fork and carried on the fork's events.
- WS-FR-078 Deleting a fork needs no archive first. It removes the fork, its own branches, its variables and secrets, and revokes the tokens bound to it, in one transactional step, then force-closes its active sessions (reason `workspace deleted`, or `fork expired`).
- WS-FR-079 Deleting or expiring a fork does not delete changes. Changes that have the fork as source belong to the parent and their versions stay fetchable there (§7). What an open change does when its source is gone is defined in §7.
- WS-FR-080 Deleting a parent deletes all its forks in the same step (WS-FR-065). One `hub.workspace.delete` is emitted for each fork and one for the parent.
- WS-FR-081 A fork never changes its parent's branches. The only path from a fork into the parent is a change that lands (§7, §8).

### 4.5 REST API

All paths are relative to `/api/v1`. Errors use the error envelope (§3). Every workspace-scoped route can answer `401`, `403` (scope, or a member on a write route), `404 workspace not found` and `500`; these are not repeated below. `POST` routes that create a resource accept `Idempotency-Key`.

#### `POST /workspaces`

- **Purpose:** register a repository and start its clone.
- **Scope:** `workspaces:write`. Not for admin tokens or bound tokens.
- **Body:**

| Field | Type | Required | Default | Validation |
|---|---|---|---|---|
| `slug` | string | yes | | WS-FR-009 |
| `git_url` | string | yes | | WS-FR-010 |
| `default_branch` | string or null | no | origin's default | WS-FR-011 |
| `org_id` | string or null | no | personal org | id or slug, WS-FR-018 |
| `display_name` | string or null | no | slug | ≤ 128 characters |
| `description` | string or null | no | `""` | ≤ 1024 characters |
| `workspace_mode` | string or null | no | `standard` | `standard`, `carry_patch` |
| `upstream_url`, `integration_branch` | string or null | carry-patch | | §10 |
| `settings` | object or null | no | defaults | 4.3.3 |
| `git_pat` | string | no | | WS-FR-015 |
| `git_username`, `git_password` | string | no | | WS-FR-015 |

- **Response:** `201` workspace object with `status: "pending"`, `access: "owner"`.
- **Errors:** `400 invalid_request`; `400 credential_check_failed`; `403 forbidden` (admin token, or not a member of the organization); `403 token_bound`; `409 conflict` (slug in use).
- **Side effects:** credential secrets stored; clone work recorded; `hub.workspace.create`.

#### `GET /workspaces`

- **Purpose:** list readable workspaces.
- **Scope:** `workspaces:read`.
- **Query:** `limit`, `cursor`; `include_archived` (`true` to include archived workspaces); `include_forks` (`true` to include forks); `parent=<slug>` (only the forks of that workspace; a parent the caller cannot read gives an empty list).
- **Response:** `200` `{"items":[workspace…],"next_cursor":…}`, ordered by `created_at` descending, then `slug`.

#### `GET /workspaces/:slug`

- **Purpose:** read one workspace or fork.
- **Scope:** `workspaces:read`. Read access.
- **Response:** `200` workspace object.

#### `PATCH /workspaces/:slug`

- **Purpose:** change mutable fields.
- **Scope:** `workspaces:write`. Write access.
- **Body:** a JSON object with at least one field that applies to the workspace's kind.

| Field | Type | Applies to | `null` means | Validation |
|---|---|---|---|---|
| `display_name` | string or null | both | back to the slug | ≤ 128 characters |
| `description` | string or null | both | `""` | ≤ 1024 characters |
| `org_id` | string or null | repository | owner's personal org | WS-FR-032 |
| `settings` | object or null | repository | all defaults | 4.3.3 |
| `ttl` | duration | fork | not allowed | WS-FR-071 |
| `purpose` | string or null | fork | cleared | ≤ 1024 characters |
| `auto_change` | bool | fork | not allowed | |

- **Response:** `200` workspace object after the update.
- **Errors:** `400 invalid_request` (bad body, immutable field, no updatable field, bad value); `400 not_supported_on_fork`; `400` for `ttl`, `purpose` or `auto_change` on a repository workspace; `403 forbidden`; `409 workspace_archived`; `409 parent_archived`.
- **Side effects:** `hub.workspace.update`.

#### `POST /workspaces/:slug/archive`

- **Purpose:** archive a repository workspace.
- **Scope:** `workspaces:write`. Write access. No body.
- **Response:** `200` workspace object with `status: "archived"`.
- **Errors:** `400 not_supported_on_fork`; `409 workspace_archived` (already archived); `409 workspace_busy` (`pending`, `cloning`, or another maintenance operation).
- **Side effects:** branches offered to origin (WS-FR-058); sessions force-closed; forks become read-only; `hub.workspace.archive`.

#### `POST /workspaces/:slug/reactivate`

- **Purpose:** bring an archived workspace back.
- **Scope:** `workspaces:write`. Write access. No body.
- **Response:** `200` workspace object with `status: "pending"`.
- **Errors:** `400 not_supported_on_fork`; `409 conflict` (not archived).
- **Side effects:** clone work recorded; `hub.workspace.reactivate`.

#### `POST /workspaces/:slug/sync`

- **Purpose:** bring origin's commits into the hub.
- **Scope:** `workspaces:write`. Write access. No body.
- **Query:** `reset_to_origin` (`true` moves diverged branches to origin's head).
- **Response:** `200` the workspace object, re-read after the sync, with one more field:

```json
{
  "slug": "my-project", "…": "…",
  "branches": [
    {"name": "main", "result": "ahead", "local_sha": "9f1c…", "origin_sha": "4b82…"},
    {"name": "release/1.x", "result": "fast_forward", "local_sha": "77aa…", "origin_sha": "77aa…"},
    {"name": "exp", "result": "diverged", "local_sha": "0c3d…", "origin_sha": "e41b…"}
  ]
}
```

  `branches` has one entry per branch on origin, ordered by name. `local_sha` is the hub's head after the sync. Carry-patch workspaces add the fields of §10.
- **Errors:** `400 fork_has_no_origin`; `400 sync_disabled`; `409` per WS-FR-007; `502 upstream_error` (fetch failed; `sync_error` is set).
- **Side effects:** branches and tags created or moved; origin heads recorded; sync fields updated; `hub.workspace.sync`.

#### `POST /workspaces/:slug/reclone`

- **Purpose:** discard the hub's copy and clone origin again.
- **Scope:** `workspaces:write`. Write access. No body.
- **Response:** `200` workspace object with `status: "pending"`, `sync_error: null`, `origin_head_sha: null`.
- **Errors:** `400 fork_has_no_origin`; `409 workspace_archived`; `409 workspace_busy`; `409 has_forks`.
- **Side effects:** WS-FR-063; `hub.workspace.reclone`.

#### `POST /workspaces/:slug/branches/:name/push`

- **Purpose:** push one branch of the hub to origin.
- **Scope:** `workspaces:write`. Write access.
- **Path:** `:name` is the branch name as one path segment, with `/` written as `%2F`. It is percent-decoded once.
- **Body (optional):** `{"force": <bool>}`, default `false`. An empty body means `false`. Anything that is not a JSON object, or a `force` that is not a boolean, is `400`.
- **Order of checks after access:** body → branch name valid (`400`) → status (WS-FR-007) → fork (`400 fork_has_no_origin`) → carry-patch refusals (WS-FR-053) → forced push of a protected branch (WS-FR-052) → branch exists (`404`, `error_type: branch_not_found`) → push.
- **Response:** `200`

```json
{"branch": "fix/42-nil-map", "sha": "<40 hex pushed>", "remote": "origin",
 "remote_url": "<git_url without userinfo>", "result": "pushed"}
```

  `result` is `up_to_date` (origin already had the commit, also with `force`), `forced` (a forced push replaced origin's branch) or `pushed`.
- **Errors of the push itself:**

| Status | `error_type` | Condition |
|---|---|---|
| `409` | `non_fast_forward` | Not forced, and origin's branch is not an ancestor of the hub's. The message names origin's head when it is known and says to sync or to push with force. |
| `409` | `origin_moved` | Forced, and origin's branch is no longer at the head the hub last saw. Sync, then decide again. |
| `422` | `origin_rejected` | Origin refused the update, for example by a branch rule. The message carries origin's reason, scrubbed. |
| `502` | `origin_unauthenticated` | Origin wants authentication and the workspace has no credential, or origin rejected the stored one. The message says which. |
| `502` | `origin_forbidden` | The credential may not push to this repository. |
| `502` | `origin_not_found` | Origin has no repository at `git_url`, or the credential cannot see it. |
| `502` | `origin_unreachable` | DNS, connection or TLS failure. |
| `502` | `upstream_error` | Any other failure of the push. |
| `504` | `origin_timeout` | The push did not finish within 2 minutes. |

- **Side effects:** origin's branch updated; origin head recorded; one `hub.workspace.push` per attempt.

#### `DELETE /workspaces/:slug`

- **Purpose:** permanently delete an archived repository workspace, or delete a fork.
- **Scope:** `workspaces:write`. Write access.
- **Response:** `204`, empty body.
- **Errors:** `409 workspace_not_archived` (repository workspace that is not archived); `409 workspace_busy`; `500 internal` (nothing was deleted).
- **Side effects:** WS-FR-065 for a repository workspace, WS-FR-078 for a fork. Tokens bound to a deleted workspace answer `401` from then on. `hub.workspace.delete`.

#### `POST /workspaces/:slug/forks`

- **Purpose:** create a task fork of `:slug`.
- **Scope:** `workspaces:write`. **Read** access to the parent is enough: owners and members may fork. A token bound to the parent may fork it. Not for admin tokens.
- **Body (all fields optional; an empty body is valid):**

| Field | Type | Default | Validation |
|---|---|---|---|
| `name` | string | generated 8-character id | WS-FR-012 |
| `ref` | string | parent's default branch | A branch of the parent. |
| `ttl` | duration | `[forks].default_ttl` | 5 minutes to `[forks].max_ttl` |
| `auto_change` | bool | `true` | |
| `token` | object | none | `{name?, permissions, paths?, expires?}`, WS-FR-076 |
| `purpose` | string | null | ≤ 1024 characters |
| `session_id` | string | null | WS-FR-077 |

- **Response:** `201`

```json
{
  "workspace": { "slug": "my-project--k3f9x2ab", "kind": "fork", "status": "ready", "…": "…" },
  "remote": "https://hub.example.com/git/acme/my-project--k3f9x2ab.git",
  "token": { "id": "tok-91", "name": "fork:my-project--k3f9x2ab", "secret": "parley_pat_…",
             "permissions": ["git:read", "git:write", "changes:write"], "paths": ["src/loader/**"],
             "expires_at": null }
}
```

  `workspace` is the fork's workspace object with `access: "owner"`. `remote` equals its `hub_url`. `token` is present only when it was requested; `secret` is never shown again.
- **Errors:** `400 invalid_request` (bad `name`, `ttl`, `token`, `session_id`); `400 ref_not_found` (`ref` is not a branch of the parent); `400 not_supported_on_fork` (the parent is itself a fork); `403 forbidden` (admin token; token permissions or paths wider than the caller's); `409` per WS-FR-007 (parent not `ready`); `409 conflict` (fork name in use); `429 fork_limit_reached`.
- **Side effects:** fork created; optional token created; `hub.workspace.fork`.

#### `GET /workspaces/:slug/forks`

- **Purpose:** list the forks of a workspace.
- **Scope:** `workspaces:read`. Read access to the parent.
- **Query:** `limit`, `cursor`; `owner=me` (only forks the caller owns); `session_id` (only forks created by that session).
- **Response:** `200` `{"items":[workspace…],"next_cursor":…}`, ordered by `created_at` descending, then `slug`. A fork is not a parent, so this list is empty for a fork.

### 4.6 CLI

The commands use the global connection and credential flags and the conventions of §3: JSON on stdout, the error envelope on stderr, exit codes 0, 1, 2 and 3, request timeout 30 seconds unless stated. Slugs and branch names are sent as one percent-encoded path segment each.

`--setting key=value` (create and update, repeatable) is parsed as follows. The text is split at the first `=`. A missing `=` or an empty key is a usage error. The value `true` or `false` is sent as a boolean. An empty value is sent as `null` (back to the default). For `protected_branches` the value is split at commas into a list. Anything else is sent as a string. A repeated key: the last one wins.

#### `parley workspace create`

- **Flags:** `--slug` (required), `--git-url` (required), `--default-branch`, `--org`, `--display-name`, `--description`, `--workspace-mode`, `--upstream-url`, `--integration-branch`, `--git-pat`, `--git-username`, `--git-password`, `--setting key=value`, `--wait`, `--timeout`, `--poll-interval`.
- **Client checks (exit 2, nothing sent):** required flags; credential flags that exclude or need each other; empty credential values.
- **Calls:** `POST /workspaces`. With `--wait`, then polls `GET /workspaces/:slug` until `ready` (exit 0) or `failed` (exit 1).
- **Output:** the workspace. With `--wait`, the final workspace.

#### `parley workspace list`

- **Flags:** `--include-archived`, `--include-forks`, `--parent <slug>`, `--limit`, `--cursor`.
- **Calls:** `GET /workspaces`. **Output:** the list page.

#### `parley workspace get <slug>`

- **Calls:** `GET /workspaces/:slug`. **Output:** the workspace.

#### `parley workspace update <slug>`

- **Flags:** `--display-name`, `--description`, `--org`, `--clear-display-name`, `--clear-description`, `--clear-org` (send the field as `null`; a clear flag wins over the matching value flag), `--setting key=value`.
- **Client checks:** at least one flag (exit 2).
- **Calls:** `PATCH /workspaces/:slug`. **Output:** the updated workspace.

#### `parley workspace archive <slug>`

- **Calls:** `POST /workspaces/:slug/archive`. Timeout 3 minutes, because archive pushes to origin first. **Output:** the workspace.

#### `parley workspace reactivate <slug>`

- **Flags:** `--wait`, `--timeout`, `--poll-interval`.
- **Calls:** `POST /workspaces/:slug/reactivate`. With `--wait`, polls as `create` does. **Output:** the workspace.

#### `parley workspace sync <slug>`

- **Flags:** `--reset-to-origin`, `--fail-on-diverged`, `--wait`, `--timeout`, `--poll-interval`.
- **Calls:** `POST /workspaces/:slug/sync`, with a 3-minute request timeout. With `--wait`, if the answer names a rebuild job (§10), polls that job until it is final.
- **Output:** the sync answer. With `--wait` and a rebuild, then the final job record.
- **`--fail-on-diverged`:** after printing, if any entry of `branches` has `result: "diverged"`, or the carry-patch field `patches_diverged` (§10) is not empty, the command names the diverged branches on stderr and exits **3**. A failed wait (exit 1) takes precedence. Without the flag, divergence does not change the exit code.
- **Exit codes:** 0, 1, 2, 3.

#### `parley workspace reclone <slug>`

- **Flags:** `--confirm` (required; without it exit 2 and nothing is sent), `--wait`, `--timeout`, `--poll-interval`.
- **Calls:** `POST /workspaces/:slug/reclone`. Timeout 3 minutes. **Output:** the workspace.

#### `parley workspace push <slug> <branch>`

- **Flags:** `--force`.
- **Calls:** `POST /workspaces/:slug/branches/:name/push` with `{"force": <bool>}`. The request timeout is **3 minutes**, longer than the hub's 2-minute push deadline, so the command always receives the hub's answer, including `504 origin_timeout`.
- **Output:** the push answer. With `--force`, a warning on stderr first that origin's branch may be replaced.

#### `parley workspace delete <slug>`

- **Flags:** `--confirm` (required; without it exit 2 and nothing is sent).
- **Calls:** `DELETE /workspaces/:slug`.
- **Output:** `{"status": "deleted", "slug": "<slug>"}`.

#### `parley fork create <parent>`

- **Flags:**

| Flag | Sent as | Notes |
|---|---|---|
| `--name <name>` | `name` | |
| `--ref <branch>` | `ref` | |
| `--ttl <duration>` | `ttl` | |
| `--no-auto-change` | `auto_change: false` | |
| `--token-permissions <list>` | `token.permissions` | Comma-separated scopes. Giving this flag is what requests a token. |
| `--token-paths <globs>` | `token.paths` | Comma-separated. Needs `--token-permissions` (else exit 2). |
| `--purpose <text>` | `purpose` | |
| `--session <id>` | `session_id` | |

- **Calls:** `POST /workspaces/:slug/forks`.
- **Output:** the response: `workspace`, `remote` and, when requested, `token` with its secret. The secret is printed this once only; the command says so on stderr.
- **Exit codes:** 0, 1, 2.

#### `parley fork list <parent>`

- **Flags:** `--mine` (sends `owner=me`), `--session <id>`, `--limit`, `--cursor`.
- **Calls:** `GET /workspaces/:slug/forks`. **Output:** the list page.

#### `parley fork extend <slug>`

- **Flags:** `--ttl <duration>` (required).
- **Calls:** `PATCH /workspaces/:slug` with `{"ttl": "<duration>"}`. **Output:** the fork, with its new `expires_at`.
- **Client checks:** a slug without `--` is not a fork slug (exit 2, nothing sent).

#### `parley fork delete <slug>`

- **Calls:** `DELETE /workspaces/:slug`. No confirmation flag: forks are disposable.
- **Client checks:** a slug without `--` is not a fork slug (exit 2, nothing sent), so this command can never delete a repository workspace.
- **Output:** `{"status": "deleted", "slug": "<slug>"}`.

### 4.7 Background behaviour

#### 4.7.1 Clone work

Clone work is recorded durably by create, reactivate and reclone, and runs with at most `[workspace].workers` clones at a time. At start the hub resumes all work for workspaces in `pending` or `cloning` (WS-FR-025, WS-FR-026). On shutdown a running clone is interrupted; it is resumed at the next start and leaves no half-made copy that could be mistaken for a valid one.

For a carry-patch workspace, a fresh clone is followed by the upstream set-up of §10 before the workspace becomes `ready`.

#### 4.7.2 Fork expiry

A sweep runs at least once a minute. It deletes, as in WS-FR-078:

- every fork whose `expires_at` has passed (reason `ttl`);
- when `[forks].expire_on_land` is true, every fork that has met the conditions of WS-FR-070 for one hour (reason `landed`).

Each deletion emits `hub.fork.expire`. A fork stays usable until the sweep removes it, so a fork may live up to a minute past `expires_at`. Forks of an archived parent expire as usual. A deletion that fails is retried by the next sweep.

#### 4.7.3 Carry-patch hand-off points

- After a fresh clone: upstream set-up (§10).
- During sync: upstream fetch, patch refresh, merge detection and rebuild (§10).
- On push to origin: refusal of the integration branch and patch branches (§10).

### 4.8 Events

All are hub events with the common fields of §14. `workspace` is the slug of the workspace acted on; for a fork event it is the fork's slug, and `metadata.parent_slug` names the parent. No event ever carries a credential.

| Event | When | Metadata |
|---|---|---|
| `hub.workspace.create` | A repository workspace was created. | `kind`, `git_url` (without userinfo), `workspace_mode`, `default_branch` (may be null) |
| `hub.workspace.update` | A successful update. | `fields`: the fields given |
| `hub.workspace.sync` | A sync completed. | `created`, `fast_forward`, `ahead`, `diverged`, `reset`: lists of branch names; `updated`: list of `{ref, before, after}` |
| `hub.workspace.push` | Each push handed to origin, success or failure. | `branch`, `sha`, `force`, `ok`, `remote_url` (without userinfo), and `result` when ok or `error_type` when not |
| `hub.workspace.archive` | A successful archive. | `head_sha`, `origin_push` (`ok`, `partial` or `failed`), `forks` (count) |
| `hub.workspace.reactivate` | A successful reactivate. | none |
| `hub.workspace.reclone` | A reclone was accepted. | `origin_push` (`ok`, `partial` or `failed`) |
| `hub.workspace.delete` | A workspace or fork was deleted by a caller, or a fork was deleted with its parent. | `kind`, `parent_slug`, `revoked_tokens` (sorted ids), `deleted_forks` (slugs; parent only) |
| `hub.workspace.fork` | A fork was created. `resource_id` is the fork's slug. | `parent_slug`, `fork_point`, `expires_at`, `auto_change`, `purpose`, `token_id` (when a token was minted) |
| `hub.fork.expire` | The sweep deleted a fork. Actor is `system`. | `parent_slug`, `reason` (`ttl` or `landed`), `expires_at`, `revoked_tokens` |

A fork that expires emits `hub.fork.expire` and not `hub.workspace.delete`. Events about a fork carry the fork's `session_id` when it has one.

Pushes of agents and people into a workspace or fork emit `hub.git.push` (§5). Session terminations emit the session events of §14. Failed syncs, failed clones and refused requests emit no workspace event.

### 4.9 Configuration

| Key | Type | Default | Meaning |
|---|---|---|---|
| `[workspace].workers` | integer | `4` | Clones that may run at the same time. Must be at least 1; any other value stops the start with a clear message. |
| `[forks].default_ttl` | duration | `24h` | Lifetime of a fork when the request gives no `ttl`. |
| `[forks].max_ttl` | duration | `30d` | Longest `ttl` a request may ask for, at creation and on extension. Must not be shorter than `default_ttl`. |
| `[forks].max_per_workspace` | integer | `200` | Forks a parent may have at one time. |
| `[forks].expire_on_land` | bool | `true` | Delete a fork early once its changes have all landed or been abandoned. |
| `[server].external_url` | string | `""` | Base of `hub_url` and `remote` (defined in §3). |

Fixed values: credential check deadline 10 seconds; push-to-origin deadline 2 minutes; early-expiry grace period 1 hour; shortest fork `ttl` 5 minutes; expiry sweep at least once a minute; scrubbed message limit 1024 characters.

### 4.10 Edge cases and error handling

- **Generated fork name collides.** The hub picks another name. The caller never sees a conflict for a name it did not choose.
- **Archive when origin refuses the safety push.** The workspace is archived anyway. Nothing is lost, because the hub keeps its copy. The event says `origin_push: failed` or `partial`.
- **Reactivate when the kept copy is damaged or missing.** The workspace is cloned fresh from origin. Commits that existed only in the hub are gone.
- **Sync with `reset_to_origin`.** Only `diverged` branches are reset. `ahead` branches keep their commits. Resetting a protected branch discards landed commits from that branch; the changes that landed them keep their state and versions (§7).
- **Sync while a change lands.** Both are compare-and-swap. If landing moves the branch first, sync evaluates it again and will usually report `ahead`. If sync moves it first, landing retries onto the new head (§8).
- **Forced push when the hub never saw the branch on origin.** The lease is "no such branch". If origin has the branch after all, the answer is `409 origin_moved`.
- **Branch name with `%` in the path.** It is decoded once: `fix%2F42` is `fix/42`, and `a%2525b` is `a%25b`.
- **Fork of a branch that is deleted in the parent later.** The fork stays. Its base branch no longer resolves, so `head_sha` is `null` and a new change cannot target it. Existing own branches stay readable.
- **Parent branch created after the fork, with a name the fork already uses.** Nothing happens in the fork. The fork's branch stays an own branch.
- **Fork request with a commit id or a tag as `ref`.** Refused with `400 ref_not_found`. A fork needs a base branch that a change can target.
- **Extending a fork after `expires_at` but before the sweep.** Allowed. The fork lives on.
- **Fork with work that has no change.** With `auto_change` false and no change opened by hand, early expiry never applies. Unlanded work in a fork is lost at `expires_at`; the fork's owner is expected to extend it or open a change.
- **Parent archived with live forks.** Forks become read-only, their sessions are closed, and they still expire on time. Reactivating the parent makes the remaining forks writable again.

---

## 5. Git server

### 5.1 Purpose and scope

The hub serves every workspace and every task fork as a git remote over smart HTTP, on the same port as the REST API. Stock `git` clients clone, fetch and push with their hub credentials. The git server authenticates with HTTP Basic, applies the same access rule and scopes as the REST API, enforces the push rules (hub-owned refs, protected branches, fork tracking branches, carry-patch push control, path-scoped tokens), and turns accepted pushes into changes and change versions (§7).

In scope: the three smart-HTTP routes, authentication and authorization on them, protocol support, push rules, the push-to-change trigger and its client-visible output, push events, and the `parley credential-helper` command.

Out of scope: SSH transport and the dumb HTTP protocol (not offered); change semantics (§7); landing and policy (§8); the carry-patch policy behind push control and the rebuild that follows a patch push (§10); the source control API, which reads and writes the same repositories without a git client (§6).

### 5.2 User stories

**GIT-US-01** As a Developer, I want to clone a workspace with `git clone <external_url>/git/<org>/<slug>.git` and my API key, so that I can work with ordinary git tooling.
- A task fork is cloned the same way, with the fork's slug.

**GIT-US-02** As an Agent, I want to fetch only what I need (a shallow history, no blobs until I read them), so that starting work on a large repository is fast.

**GIT-US-03** As an Agent with a PAT, I want my push to open a change, or to add a version to the change my branch already belongs to, so that I do not need a second call.
- `git push -o parley.change` opens a change; in a task fork with `auto_change` the option is not needed.
- The push output names the change and the version.

**GIT-US-04** As an Org member with read-only access, I want to fetch my org's workspaces. A push is refused with a clear message; I contribute through a task fork (§4).

**GIT-US-05** As a Workspace owner, I want direct pushes to protected branches refused, so that those branches move only by landing (§8) or sync from origin (§4).

**GIT-US-06** As an Operator, I want a path-scoped token to be unable to push commits that touch files outside its paths, so that an agent's write access is bounded.

**GIT-US-07** As an Agent, I want to force-push my own unprotected branch after a rebase, and to fetch earlier versions of any change by ref.

**GIT-US-08** As a Developer, I want `git` to obtain my hub credential through `parley credential-helper`, so that plain hub URLs work and the credential is never sent to another host.

**GIT-US-09** As a consumer of the event stream, I want one event per updated ref, linked to the session that pushed, so that I can follow what agents do.

### 5.3 Concepts and data model

**Git URL.** `<external_url>/git/<org-slug>/<workspace-slug>.git`. `<org-slug>` is the slug of the organization the workspace belongs to. A task fork inherits its parent's org and is served at the same shape with its own slug (`…/git/acme/demo--k3x9w2ab.git`). The workspace object's `hub_url` carries this URL; it is null when `[server].external_url` is empty or the workspace has no organization. A workspace without an organization has no git URL.

**Repository.** The hub's copy is a bare repository. There is no worktree and no checked-out branch. `HEAD` is a symbolic ref to the workspace's default branch, so a clone checks that branch out. The repository is served while the workspace status is `ready`.

**Credential.** The Basic password is a PAT, an API key or the admin token (§3). Bearer tokens are not accepted on git routes. Only PATs are scope-checked: `git:read` for fetch, `git:write` for push; `git:write` satisfies a read check.

**Access.** The workspace access rule of §3 applies unchanged:

| Caller | Fetch | Push |
|---|---|---|
| Admin token | yes | yes |
| Workspace owner | yes | yes |
| Org member (not owner) | yes | no (`403`) |
| Anyone else | `404` | `404` |

A PAT bound to a workspace is valid only on that workspace and on the forks it created (§4). On any other repository it is answered like a stranger, and the refusal is reported as `hub.token.bound_refused` with `surface: "git"` (§3).

**Hub-owned refs.** Everything under `refs/hub/` is written only by the hub. Two families are defined elsewhere: `refs/hub/changes/<number>/<version>` (every version of every change, §7) and `refs/hub/replaced/<name>` (backups of branch heads the hub replaced, §4 and §10). Hub-owned refs are listed and fetchable by name. A default clone does not fetch them, because git's default refspec covers only branches and tags.

**Ref update.** One command of a push: `{ref, before, after}`. `before` is the all-zero id for a create, `after` is the all-zero id for a delete. An update is **forced** when it is neither a create nor a delete and `before` is not an ancestor of `after`.

The git server has no entities of its own. It reads workspaces, tokens, settings and patches, and it writes objects, refs, the workspace's `head_sha` (§4), changes and versions (§7) and events.

### 5.4 Functional requirements

#### Routing, authentication and authorization

- **GIT-FR-001** Exactly three routes exist, outside `/api/v1`: `GET /git/:org/:slug.git/info/refs`, `POST /git/:org/:slug.git/git-upload-pack` and `POST /git/:org/:slug.git/git-receive-pack`.
- **GIT-FR-002** The REST body size limit (`[server].max_body_size`) does not apply to git routes. `[git].max_push_size` applies instead.
- **GIT-FR-003** Checks run in the order of §3: authenticate (`401`) → PAT scope (`403`) → repository resolution and access (`404`, or `403` for a read-only member on a push route) → request validation (`400`) → workspace state (`409`) → act.
- **GIT-FR-004** A request with missing, malformed or wrong credentials, or from a blocked user, is answered `401` with `WWW-Authenticate: Basic realm="parley"`, so that git can ask its credential helper. The Basic username is never consulted.
- **GIT-FR-005** Need is derived from the route: `git-receive-pack`, and `info/refs` with `service=git-receive-pack`, need write and `git:write`. Everything else needs read and `git:read`.
- **GIT-FR-006** The repository resolves only when the slug names a workspace, the workspace has an organization, and `:org` is that organization's slug. A path segment that does not end in `.git`, an unknown slug, a wrong org and a caller without read access all get the same answer: `404 repository not found`. The answer does not depend on the workspace's status, so a workspace that is not ready does not reveal its existence to callers without access.
- **GIT-FR-007** A caller with read access who addresses a workspace that is not `ready` gets `409` with the message `repository not available: workspace is <status>`.
- **GIT-FR-008** Membership and token bindings are read on every request. A removed member is a stranger on the next request.
- **GIT-FR-009** Errors before the git exchange starts are answered with the HTTP status and a one-line `text/plain` body, which git prints as `remote: <message>`. The statuses and messages are listed in §5.5. Errors after the exchange has started are in-band: `ERR` packets on fetch, the status report and `remote:` lines on push, with HTTP `200`.

#### Protocol

- **GIT-FR-010** Fetch supports git protocol version 2 when the client asks for it with the `Git-Protocol: version=2` header, and falls back to version 0 otherwise. Push uses the version 0 exchange, as git does.
- **GIT-FR-011** Partial clone MUST work. The filters `blob:none`, `blob:limit=<n>` and `tree:<n>` are accepted; a client can later fetch the omitted objects on demand. Other filter kinds may be refused with an in-band error.
- **GIT-FR-012** Shallow fetch MUST work: `--depth`, `--deepen`, `--shallow-since`, `--shallow-exclude` and `--unshallow`.
- **GIT-FR-013** Incremental fetch negotiates haves and wants and sends only missing objects. Gzip-compressed request bodies are accepted. Progress is streamed over the side band.
- **GIT-FR-014** Every ref is advertised, including `refs/hub/**`. Under version 2 the client's ref-prefix filter keeps the listing small. A client may also request by object id any commit that is reachable from an advertised ref.
- **GIT-FR-015** In a task fork, tracking branches are advertised at the parent's current heads (§4).
- **GIT-FR-016** The push advertisement offers at least `report-status`, `side-band-64k`, `delete-refs`, `ofs-delta`, `atomic` and `push-options`. Thin packs are accepted.

#### Push processing

- **GIT-FR-020** A push is processed in this order: read the commands and push options → validate the push options (GIT-FR-051) → receive the pack into quarantine → evaluate the push rules for every ref (GIT-FR-030) → update all refs together → map branches to changes (GIT-FR-040) → emit events → answer.
- **GIT-FR-021** A push is atomic across refs, whether or not the client asked for `--atomic`. If any ref is refused, no ref is updated, the pushed objects are discarded, no event is emitted and no change is touched. The refused refs carry their own message; every other ref of the push is reported as `not updated: another ref in this push was refused`.
- **GIT-FR-022** Every ref update is a compare-and-swap against the `before` value the client sent. A ref that has moved, a create of an existing ref, and an update or delete of a missing ref are refused with `ref changed since it was read; fetch and try again`. A push never waits for, and is never refused because of, another running operation, except while the workspace is not `ready` (GIT-FR-007).
- **GIT-FR-023** A refusal is reported twice: as the per-ref status (`ng <ref> <message>`, which git shows as `! [remote rejected] <ref> (<message>)`), and as a side-band line `error: <ref>: <message>`, which git shows prefixed with `remote:`. The HTTP status is `200`.
- **GIT-FR-024** A pack that cannot be read, or that exceeds `[git].max_push_size`, fails the push with `unpack <reason>`; nothing changes.
- **GIT-FR-025** A push with no commands is a successful no-op: no event, no change activity.
- **GIT-FR-026** Any ref name outside `refs/hub/` may be pushed: branches, tags, notes and other namespaces. The push rules below decide.
- **GIT-FR-027** When an accepted push moves the default branch, the workspace's `head_sha` (§4) is updated before the push is answered.

#### Push rules

- **GIT-FR-030** For each ref update the rules are evaluated in the order of the table. The first rule that refuses decides the message. Rules are evaluated for all refs before any ref is written or any side effect happens. `<branch>` is the ref name without `refs/heads/`.

| # | Rule | Refused updates | Message |
|---|---|---|---|
| 1 | **Hub-owned refs.** `refs/hub/**` is never client-writable, for any credential including the admin token. | create, update, delete | `refs/hub/ is maintained by the hub and cannot be pushed to` |
| 2 | **Protected branches.** A branch matched by the workspace setting `protected_branches` (§4, §8; default: the default branch) moves only by landing or by sync from origin. | create, update, forced update, delete | `<branch> is protected; push another branch and open a change (git push -o parley.change)` |
| 2a | **Default branch.** The default branch cannot be deleted, even when it is not protected. | delete | `<branch> is the default branch and cannot be deleted` |
| 3 | **Tracking branches.** In a task fork, every branch inherited from the parent is read-only (§4). | create, update, delete | `<branch> tracks the parent workspace and is read-only in this fork; push a new branch` |
| 4 | **Carry-patch push control.** On a carry-patch workspace the integration branch is hub-owned and registered patch branches follow the workspace's patch settings (GIT-FR-031). | see GIT-FR-031 | see GIT-FR-031 |
| 5 | **Path-scoped tokens.** If the pushing PAT has `paths` (§3, §8), every file changed by the pushed commits must match one of its globs (GIT-FR-032). | the whole push | `path <path> is outside this token's allowed paths` |
| 6 | **Everything else is allowed.** Creates, fast-forward updates, non-fast-forward updates and deletes of unprotected refs are accepted. Agents rewrite their own branches. | none | n/a |

- **GIT-FR-031** Carry-patch push control (policy in §10):

| Ref | Workspace settings | Result |
|---|---|---|
| Integration branch | any | Refused: `integration branch <branch> is maintained by the hub; push a patch branch and rebuild instead` |
| Registered patch branch (patch not deleted) | `patch_branch_source = hub` | Allowed |
| Registered patch branch | `patch_branch_source = origin`, `push_patches_to_origin = false` | Refused: `branch is synced from origin; push to <origin URL without credentials> instead` |
| Registered patch branch, delete | `patch_branch_source = origin`, `push_patches_to_origin = true` | Refused: `branch is synced from origin; delete it on origin instead` |
| Registered patch branch, create or update | `patch_branch_source = origin`, `push_patches_to_origin = true` | Forwarded to origin without force, then allowed. Refused with `failed to resolve origin credentials` or `origin rejected push: <reason without credentials>` when forwarding fails |
| Any other ref | any | Rule 5 and 6 apply |

  Forwarding to origin is the only rule with an outside effect. It runs last, only when every ref of the push has passed every other rule, with a 120 s timeout per branch. If a forward fails, the whole push is refused; branches already forwarded in the same push stay on origin and return through the next sync.

- **GIT-FR-032** Path check for a path-scoped token. The checked commits are those reachable from the pushed heads and not reachable from any ref the repository had before the push (in a fork this includes the parent's refs). For each such commit the changed files are its differences against its first parent, or its whole tree for a root commit; a rename counts with both its old and its new path. If any changed path matches none of the token's globs, the whole push is refused and the message names the first offending path in commit order, then path order. Tag and note pushes are checked the same way. Deletes and updates that add no new commits change no files and pass. Path limits never restrict fetch.
- **GIT-FR-033** If a rule cannot be evaluated because of an internal failure, the push is refused with `internal error; try again`. Rules never fail open.

#### Push to change mapping

- **GIT-FR-040** After the refs of an accepted push are written, the hub looks at each created or updated branch (`refs/heads/*`; tags, other refs and deletes are skipped), in command order:
  1. If the branch is the source of a change in state `open` or `draft`, a new version is recorded for that change (§7). No option is needed.
  2. Otherwise a change is opened when the push carries `parley.change` or `parley.target`, or when the workspace is a task fork with `auto_change` on (§4).
  3. Otherwise nothing happens. The branch is just a branch.
- **GIT-FR-041** The change belongs to the workspace of its target branch. For a push to a task fork that is the parent workspace, and the source is the fork's branch. The default target is the fork's base branch (`fork_point.ref`) in a fork, and the workspace's default branch otherwise.
- **GIT-FR-042** The mapping runs before the push is answered, so that its result can be reported. For each branch the client receives one side-band line, shown by git with the `remote:` prefix:

| Outcome | Line |
|---|---|
| New version of an existing change | `change <workspace>#<number> version <n>` |
| Change opened | `change <workspace>#<number> version 1 (opened)` |
| Change opened as draft | `change <workspace>#<number> version 1 (opened, draft)` |
| Change not opened or version not recorded | `warning: no change for <branch>: <reason>` |

  `<workspace>` is the slug of the workspace the change belongs to. The lines carry no URL. Example:

```
$ git push -o parley.change -o parley.title="Fix retry loop" origin fix-retry
remote: change demo#12 version 1 (opened)
To https://hub.example/git/acme/demo.git
 * [new branch]      fix-retry -> fix-retry
```

- **GIT-FR-043** A failure in the mapping never fails the push: the refs stay updated, `hub.git.push` is emitted, and the reason is reported on the `warning:` line. Reasons include: the target branch does not exist, the branch is its own target, the branch has no commits ahead of the target, and a PAT without `changes:write` asking to open a change. Recording a version on an existing change needs only `git:write`.

#### Push options

- **GIT-FR-050** The hub understands these options. Options without the `parley.` prefix are ignored.

| Option | Value | Effect |
|---|---|---|
| `parley.change` | none | Open a change for every pushed branch that is not already the source of an open or draft change |
| `parley.target=<branch>` | branch name | Target branch of the changes this push opens. Implies `parley.change` |
| `parley.title=<text>` | 1–200 characters | Title of the changes this push opens. Default: the subject line of the branch's head commit |
| `parley.intent=<text>` | up to 1,024 bytes | Intent of the changes this push opens. Default: the body of the head commit's message |
| `parley.draft` | none | Open the changes in state `draft` instead of `open` |
| `parley.session=<id>` | session id (§14) | Session recorded on the push events, on new versions and on opened changes |

- **GIT-FR-051** Options are validated before the pack is received. An invalid option refuses the whole push with `error: invalid push option <name>: <reason>` and nothing is written. Invalid means: an unknown `parley.*` name, the same option given twice, a value on `parley.change` or `parley.draft`, a missing or empty value on the others, a `parley.target` that is not a valid branch name, a value over its length limit, or a `parley.session` that does not name a session the caller may use (§14). A push carries at most 16 options of at most 1,024 bytes each.
- **GIT-FR-052** `parley.target`, `parley.title`, `parley.intent` and `parley.draft` affect only changes opened by this push. They never alter an existing change; for that the client uses the changes API (§7). When a push opens several changes, the options apply to each.
- **GIT-FR-053** `parley.target` naming a branch that does not exist is not a validation error. It is a failure to open the change (GIT-FR-043), because the push itself is still valid.
- **GIT-FR-054** `parley.session` takes effect on every push, whether or not a change is involved.

#### After a push

- **GIT-FR-060** One `hub.git.push` event is emitted per updated ref (§5.8). Event delivery problems never fail a push.
- **GIT-FR-061** On a carry-patch workspace, an accepted update of a registered patch branch triggers the post-push behaviour of §10 (rebuild of the integration branch, optional mirror to origin). It runs in the background and never delays the answer.
- **GIT-FR-062** Every git request is counted in `parley_git_requests_total{service,result}` with `service` ∈ `upload-pack`, `receive-pack` and `result` ∈ `ok`, `refused` (a push refused by a rule or an invalid option), `denied` (`401`, `403`, `404`), `error`.

#### Credential helper

- **GIT-FR-070** `parley credential-helper <verb>` implements git's credential helper protocol. Only `get` produces output. `store`, `erase` and any other verb print nothing and exit 0.
- **GIT-FR-071** The helper takes the endpoint and the credential from the CLI's normal configuration (§3): `config.toml` in `~/.parley` or `PARLEY_CONFIG_DIR`, overridden field by field by `PARLEY_ENDPOINT_URL` and `PARLEY_API_KEY`. If either value is missing or the URL has no host, it prints nothing and exits 0.
- **GIT-FR-072** It reads `key=value` lines from stdin up to the first empty line. It answers only when the request's `protocol` equals the hub URL's scheme and the host and port match (host compared case-insensitively; a missing port means 443 for `https` and 80 for `http`). The credential is never offered to another scheme, host or port. The path is not considered.
- **GIT-FR-073** On a match it prints exactly `protocol=<protocol>`, `host=<host as given>`, `username=x-token-auth` and `password=<credential>`, one per line.

### 5.5 REST API

The three endpoints speak git smart HTTP with HTTP Basic authentication. They do not use the JSON error envelope: a git client reads them, so refusals are `text/plain` with one line of text.

Common refusals, in check order:

| Status | Body | Condition |
|---|---|---|
| `401` + `WWW-Authenticate: Basic realm="parley"` | `authentication required` | No, malformed or non-Basic `Authorization`; unknown, revoked or expired credential; blocked user |
| `403` | `token lacks scope git:read` / `token lacks scope git:write` | PAT without the needed scope |
| `404` | `repository not found` | Unknown slug, no org, wrong org, missing `.git`, caller without read access, PAT bound elsewhere |
| `403` | `org members have read-only access to this workspace; create a task fork to contribute` | Read-only member on a push route |
| `400` | `invalid or missing service parameter` | `info/refs` without `service=git-upload-pack` or `service=git-receive-pack` |
| `409` | `repository not available: workspace is <status>` | Caller has access, workspace not `ready` |
| `500` | `internal error` | Lookup or repository failure before the exchange starts |

#### `GET /git/:org/:slug.git/info/refs`

- **Purpose:** ref discovery for fetch or push.
- **Auth / scope:** Basic. `service=git-upload-pack`: read access, `git:read`. `service=git-receive-pack`: write access, `git:write`.
- **Query:** `service` (required; the first value counts).
- **Response:** `200`, `Content-Type: application/x-<service>-advertisement`, `Cache-Control: no-cache`. For version 2 fetch, the capability advertisement; otherwise the service line and the ref advertisement (GIT-FR-014, GIT-FR-016).
- **Errors:** common refusals.

#### `POST /git/:org/:slug.git/git-upload-pack`

- **Purpose:** fetch and clone.
- **Auth / scope:** Basic; read access; `git:read`.
- **Request:** a git upload-pack request (version 0 or version 2 command), optionally `Content-Encoding: gzip`.
- **Response:** `200`, `Content-Type: application/x-git-upload-pack-result`, `Cache-Control: no-cache`, streamed.
- **Errors:** common refusals; in-band `ERR <message>` for an empty or invalid request, an invalid gzip body, an unsupported filter, a want that is not reachable, or a timeout.

#### `POST /git/:org/:slug.git/git-receive-pack`

- **Purpose:** push.
- **Auth / scope:** Basic; write access (owner or admin token); `git:write`.
- **Request:** command list, optional push options, pack.
- **Response:** `200`, `Content-Type: application/x-git-receive-pack-result`, with the status report (`unpack ok` or `unpack <reason>`, then `ok <ref>` or `ng <ref> <message>` per ref) and side-band lines for refusals and change mapping.
- **Errors:** common refusals; in-band refusals per GIT-FR-021 to GIT-FR-033 and GIT-FR-051.

### 5.6 CLI

#### `parley credential-helper <get|store|erase>`

- **Purpose:** git credential helper that hands the configured hub credential to git for the hub's own URL. It makes no request to the hub.
- **Arguments:** exactly one, the git credential verb. No flags of its own.
- **Output:** for `get` with a match, the four lines of GIT-FR-073 on stdout; otherwise nothing.
- **Exit codes:** 0 in every handled case, including no match and missing configuration; 2 for a wrong number of arguments.
- **Setup:** `git config --global credential.<hub-url>.helper '!parley credential-helper'`. The credential may be an API key or a PAT.

### 5.7 Background behaviour

- Objects of a refused or failed push are held in quarantine and discarded; they never become reachable or fetchable.
- The carry-patch post-push work (§10) and everything a new change version triggers (restack, mergeability, overlap detection; §7, §9) run after the push is answered. What deleting a change's source branch means is specified in §7.

### 5.8 Events

| Event type | When | Fields |
|---|---|---|
| `hub.git.push` | Once per updated ref of an accepted push, in command order | `resource_type: "workspace"`, `resource_id: <slug>`, `action: "push"`, `session_id` from `parley.session` (else null); metadata `ref` (full name), `before`, `after` (40-hex; all-zero for create and delete respectively), `created`, `deleted`, `forced` (booleans), `via` (`git` here; the same event is emitted with `api` for ref updates through the source control API, §6, and with `land` when a land moves the target, §8) |
| `hub.git.fetch` | Once per served upload-pack request that sends a pack, only when the workspace setting `audit_fetches` is true | `resource_type: "workspace"`, `resource_id: <slug>`, `action: "fetch"`; metadata `shallow` (boolean), `filter` (string or null) |

Actor fields follow the credential: `actor_type` is `pat`, `api_key` or `admin_token`. For a push to a fork, `workspace` is the fork's slug. Events of the same push have consecutive `seq` values. `hub.token.bound_refused` is specified in §3; the change events that follow a push in §7; the rebuild and mirror events in §10.

### 5.9 Configuration

| Key | Type | Default | Effect |
|---|---|---|---|
| `[server].external_url` | string | `""` | Prefix of `hub_url`. Empty means `hub_url` is null; routing does not depend on it |
| `[git].timeout` | duration | `30m` | Upper bound for one fetch or push request |
| `[git].max_push_size` | size | `0` (unlimited) | Largest accepted pack in one push |
| Workspace setting `protected_branches` | list of branch globs | the default branch | Push rule 2 (owned by §4 and §8) |
| Workspace setting `audit_fetches` | bool | `false` | Emit `hub.git.fetch` |
| `PARLEY_ENDPOINT_URL`, `PARLEY_API_KEY`, `PARLEY_CONFIG_DIR` | env (CLI side) | | Credential helper inputs |

Fixed values: Basic realm `parley`; forward-to-origin timeout 120 s per branch; at most 16 push options of 1,024 bytes each.

### 5.10 Edge cases and error handling

- A PAT without any git scope gets `403` on every git URL, whether or not the repository exists, because the scope check precedes resolution. This reveals nothing.
- `info/refs?service=git-upload-pack&service=git-receive-pack` is treated as upload-pack.
- Two concurrent pushes to the same branch: one wins, the other is refused with `ref changed since it was read; fetch and try again`. Pushes to different refs do not interfere.
- A force-push that rebases a branch onto a newer target does not trip the path check for the target's commits: they are already in the repository, so only the agent's own commits are checked.
- A client without `push-options` support cannot send options. In a fork with `auto_change` the push still opens a change; elsewhere the client opens the change through the API (§7).
- `parley.change` on a push that updates only tags or deletes branches has no effect and is not an error.
- A branch whose earlier change has landed or been abandoned is not the source of an open change. A later push with `parley.change`, or in a fork with `auto_change`, opens a new change.
- A client without `report-status` or side-band support gets no messages; the rules are enforced all the same.
- Credential helper: hub URL `https://hub` matches a request for `host=HUB:443`; an `http` request for an `https` hub gets no answer.

---

## 6. Source control API

### 6.1 Purpose and scope

The source control API lets a client read a repository and write to it without a clone. An agent that wants one file, the history of one path or a diff between two commits asks for exactly that. An agent that has produced three edited files can commit them with one request. Review agents, check runners and revert tooling need no working copy at all.

The API and the git server (§5) are two doors to the same repository. Everything written through one is visible through the other, the same push rules apply to both, and both emit the same events.

In scope: reading refs, commits, trees, files, diffs, comparisons and blame; searching file contents; creating commits from file operations; creating, moving and deleting refs; server-side rebase of a branch. Out of scope: changes and landing (§7, §8), which build on this API; anything that needs a working tree, such as running builds.

### 6.2 User stories

**SCM-US-01** As an Agent, I want to read one file at a ref without cloning, so that small tasks cost one request.

**SCM-US-02** As an Agent, I want to commit a set of file edits through the API and be told if the branch moved under me, so that I never overwrite someone else's work.
- A stale `expected_head_sha` answers `409 ref_moved` with the current head.

**SCM-US-03** As a Review agent, I want the diff and the touched paths of a commit range as JSON, so that I can review without a checkout.

**SCM-US-04** As a Developer, I want path history and blame from the hub, so that tools can answer "who changed this and in which change" directly.
- Commit objects name the change that landed them, when there is one.

**SCM-US-05** As an Agent, I want to rebase my branch onto the latest target on the server and get a conflict report if it fails, so that I can keep a branch current without fetching.

**SCM-US-06** As an Operator, I want read responses addressed by commit id to be cacheable, so that repeated reads are cheap.

### 6.3 Concepts and data model

**Revision parameters.** A parameter named `ref` accepts a branch name, a tag name, a full ref name (`refs/heads/main`, `refs/hub/changes/12/3`) or a full or abbreviated commit id. An ambiguous or unknown revision answers `404`, `error_type: ref_not_found`. Responses always name the resolved commit in `resolved_sha`.

**Ref object.**

| Field | Type | Meaning |
|---|---|---|
| `name` | string | Full ref name, for example `refs/heads/main` |
| `kind` | string | `branch`, `tag` or `hub` |
| `sha` | string | The commit the ref points at |
| `protected` | bool | True when the ref is a protected branch (§8) or hub-owned |
| `tracking` | bool | True for a tracking branch of a fork (§4) |
| `change` | object or null | `{workspace_slug, number}` when the branch is the source of an open or draft change |

**Commit object.**

| Field | Type | Meaning |
|---|---|---|
| `sha` | string | Commit id |
| `parents` | string[] | Parent commit ids |
| `tree_sha` | string | Root tree id |
| `author`, `committer` | object | `{name, email, time}` |
| `message` | string | Full commit message |
| `stats` | object | `{files_changed, insertions, deletions}`; present on single-commit reads and when `stats=true` on lists |
| `change` | object or null | `{workspace_slug, number, version}` when the hub knows the commit as a version head or as a commit landed by a change |
| `session_id` | string or null | The session recorded for the push, API commit or land that introduced the commit |

**Tree entry.** `{path, name, type: "blob"|"tree"|"commit"|"symlink", mode, sha, size}`. `size` is present for blobs only.

**File change** (in diffs and comparisons). `{path, old_path, status: "added"|"modified"|"deleted"|"renamed"|"copied"|"type_changed", insertions, deletions, binary, old_sha, new_sha}`. `old_path` is set for renames and copies.

**File operation** (in commit creation).

| `op` | Fields | Effect |
|---|---|---|
| `put` | `path`; `content` (UTF-8 string) or `content_base64`; optional `mode` (`100644` default, `100755`, `120000`) | Create or replace the file |
| `delete` | `path` | Remove the file. The path must exist |
| `move` | `from`, `to` | Rename. `from` must exist and `to` must not |
| `chmod` | `path`, `mode` | Change the mode only |

Paths are repository-relative, use `/`, and MUST NOT be empty, start with `/`, contain `.` or `..` segments, or name anything under `.git`.

**Limits.**

| Limit | Value | Configuration |
|---|---|---|
| File read size | 32 MiB; larger files answer `413` | `[scm].max_file_size` |
| Commit request body | 32 MiB | `[scm].max_commit_size` |
| Operations per commit | 1,000 | fixed |
| Diff size returned | 5,000 files or 8 MiB of patch text, then `truncated: true` | fixed |
| Blame file size | 2 MiB | fixed |
| Search results | 200 matches per page | fixed |
| Read timeout | 30 s, answered `503`, `error_type: scm_timeout` | `[scm].read_timeout` |

### 6.4 Functional requirements

Reading

- **SCM-FR-001** All read routes need `git:read` for PATs and read access to the workspace. They work on any workspace whose status allows reads (§4), including archived workspaces and forks.
- **SCM-FR-002** A read addressed only by full commit ids (every revision parameter is a 40-character id) answers with `Cache-Control: private, max-age=31536000, immutable` and an `ETag`. Any other read answers with an `ETag` derived from the resolved commit ids and honours `If-None-Match` with `304`.
- **SCM-FR-003** Reads never observe a partial update. A ref read during a land, a push or a rebuild returns either the old or the new commit.
- **SCM-FR-004** In a fork, reads see the fork's own branches and the tracking branches at the parent's current heads.
- **SCM-FR-005** The file route returns raw bytes by default, with `Content-Type: application/octet-stream`, `X-Parley-Blob-Sha`, `X-Parley-Resolved-Sha`, `X-Parley-File-Mode` and support for a single `Range`. With `Accept: application/json` it returns `{path, sha, mode, size, encoding, content}` where `encoding` is `utf8` when the content is valid UTF-8 and `base64` otherwise.
- **SCM-FR-006** Diff and compare use the merge base of `base` and `head` as the left side by default (`mode=merge_base`), which shows what `head` adds. `mode=direct` compares the two commits as they are. Rename detection is on.
- **SCM-FR-007** Commit listing takes an optional `path` and then returns only commits that changed that path, following renames of a single file when `follow=true`.
- **SCM-FR-008** Blame returns ranges, not lines: `{start_line, end_line, commit: <commit object without stats>, original_path, original_start_line}`. Each range's commit carries `change` when known.
- **SCM-FR-009** Search matches a literal string or, with `regex=true`, an extended regular expression against file contents at one ref, optionally limited by `path` globs. It returns `{path, line, text}` per match with `text` cut to 500 characters. Binary files are skipped. A search that exceeds the read timeout answers `503 scm_timeout`.

Writing

- **SCM-FR-020** All write routes need `git:write` for PATs and write access to the workspace. They are refused on an archived workspace (`409`, `error_type: workspace_archived`).
- **SCM-FR-021** Every write obeys the push rules of §5 exactly as a git push would: hub-owned refs, protected branches, tracking branches of forks, carry-patch push control and path-scoped tokens. A refusal is `403` with the same `error_type` the rule defines (`hub_owned_ref`, `protected_branch`, `tracking_branch`, `integration_branch`, `patch_branch`, `path_not_allowed`), and for `path_not_allowed` the offending `path`.
- **SCM-FR-022 (commit creation).** `POST …/scm/commits` builds one commit and moves one branch:
  1. Resolve the parent. If the branch exists, the parent is its head and `expected_head_sha` is required and MUST equal it; otherwise `409 ref_moved`. If the branch does not exist, `base_ref` is required and the parent is the commit it resolves to.
  2. Apply the operations in order to the parent's tree. An operation that cannot apply (deleting a missing path, moving onto an existing path, a path used as both file and directory) fails the whole request with `400`, `error_type: invalid_operation`, naming the operation's index.
  3. A commit whose tree equals its parent's tree is refused with `400`, `error_type: empty_commit`, unless `allow_empty` is true.
  4. Create the commit with the given `message`, `author` (default: the caller's name and e-mail) and the hub as committer.
  5. Move the branch from the parent to the new commit with a compare-and-swap. A lost race answers `409 ref_moved` and creates nothing visible.
- **SCM-FR-023** A successful commit has the same consequences as a push of that commit (§5): `hub.git.push` and `hub.scm.commit` are emitted, a new version is recorded when the branch is the source of a change, and a change is opened when the request carries a `change` object or the workspace is a fork with `auto_change` on (§7).
- **SCM-FR-024** `session_id`, when given, MUST satisfy the session-linking rule of §14; otherwise `400`, `error_type: unknown_session`, and nothing is written. It is recorded on the commit, the events and any version created.
- **SCM-FR-025 (refs).** Creating a ref needs `name` and `sha` or `ref`. Moving a ref needs `expected_sha` and is a compare-and-swap. Deleting a ref takes an optional `expected_sha`. Only `refs/heads/*` and `refs/tags/*` can be written. Moving a branch backwards or sideways is allowed on unprotected branches, as with a forced push. Deleting the default branch is refused.
- **SCM-FR-026 (server-side rebase).** `POST …/scm/rebase` replays the commits of `branch` that are not in `onto` on top of `onto` and moves `branch` to the result with a compare-and-swap guarded by `expected_head_sha`. Nothing changes on conflict: the answer is `409`, `error_type: rebase_conflict`, with a conflict report (§8.3). A branch already based on `onto` answers `200` with `status: "up_to_date"`. Merge commits in the range are refused (`400`, `error_type: merge_commits_unsupported`). Rebasing the source branch of a change creates a new version of kind `rebase`.
- **SCM-FR-027** Write routes accept `Idempotency-Key` (§3). A replayed commit creation returns the commit made the first time.

### 6.5 REST API

All routes are under `/workspaces/:slug/scm`. The conventions of §3 apply. Common errors: `401`; `403` missing scope; `404 workspace not found`; `404 ref_not_found`; `404 path_not_found`; `409 workspace_busy` during exclusive maintenance; `503 scm_timeout`.

#### `GET /workspaces/:slug/scm/refs`
- **Purpose:** list refs.
- **Query:** `prefix` (default `refs/heads/`; `refs/tags/`, `refs/hub/changes/` and any other prefix are allowed), `limit`, `cursor`.
- **Response:** `200` list of ref objects, ordered by name.

#### `GET /workspaces/:slug/scm/refs/*name`
- **Purpose:** read one ref by full name or by branch name.
- **Response:** `200` ref object. **Errors:** `404 ref_not_found`.

#### `POST /workspaces/:slug/scm/refs`
- **Purpose:** create a branch or a tag.
- **Body:** `{"name": "refs/heads/x" | "x", "sha"?: string, "ref"?: string}`. A bare name is a branch. Exactly one of `sha`, `ref`.
- **Response:** `201` ref object. **Errors:** `400` invalid name; `409`, `error_type: ref_exists`; push-rule refusals (SCM-FR-021).
- **Side effects:** `hub.git.push` with `created: true`.

#### `PATCH /workspaces/:slug/scm/refs/*name`
- **Purpose:** move a branch.
- **Body:** `{"sha": string, "expected_sha": string}`.
- **Response:** `200` ref object. **Errors:** `409 ref_moved` with `{"current_sha"}` in the envelope's `details`; push-rule refusals.
- **Side effects:** `hub.git.push` (`forced: true` when the move is not a fast-forward); a new version when the branch is a change's source.

#### `DELETE /workspaces/:slug/scm/refs/*name`
- **Purpose:** delete a branch or tag.
- **Query:** `expected_sha` (optional).
- **Response:** `204`. **Errors:** `409 ref_moved`; `409`, `error_type: ref_in_use` when the branch is the source of an open change (abandon the change first); push-rule refusals.
- **Side effects:** `hub.git.push` with `deleted: true`.

#### `GET /workspaces/:slug/scm/commits`
- **Purpose:** history of a ref, optionally of one path.
- **Query:** `ref` (default: the default branch), `path`, `follow` (bool), `since`, `until` (timestamps), `author` (substring of name or e-mail), `first_parent` (bool, default true), `stats` (bool), `limit`, `cursor`.
- **Response:** `200` list of commit objects, newest first, plus `resolved_sha`.

#### `GET /workspaces/:slug/scm/commits/:sha`
- **Purpose:** read one commit. `:sha` accepts any revision.
- **Query:** `files` (bool, default false) adds `files`: the file changes against the first parent.
- **Response:** `200` commit object with `stats`.

#### `POST /workspaces/:slug/scm/commits`
- **Purpose:** create a commit from file operations (SCM-FR-022).
- **Body:**

  ```json
  {
    "branch": "greeting",
    "expected_head_sha": "…",          // required when the branch exists
    "base_ref": "main",                // required when it does not
    "message": "Add greeting endpoint",
    "author": {"name": "…", "email": "…"},
    "operations": [
      {"op": "put", "path": "api/greeting.go", "content": "package api\n…"},
      {"op": "delete", "path": "api/old.go"}
    ],
    "allow_empty": false,
    "session_id": "…",
    "change": {"target_branch": "main", "title": "…", "intent": "…", "draft": false}
  }
  ```
- **Response:** `201 {"commit": <commit object>, "branch": <ref object>, "change": {"workspace_slug","number","version"} | null}`.
- **Errors:** `400` validation (`invalid_request`, `invalid_operation`, `empty_commit`); `403` push-rule refusals; `409 ref_moved`; `413`.
- **Side effects:** SCM-FR-023.

#### `GET /workspaces/:slug/scm/tree`
- **Purpose:** list a directory.
- **Query:** `ref`, `path` (default: the root), `recursive` (bool, default false), `limit` (default 1,000, max 10,000), `cursor`.
- **Response:** `200` list of tree entries ordered by path, plus `resolved_sha` and `tree_sha`. **Errors:** `404 path_not_found`; `400` when `path` is a file.

#### `GET /workspaces/:slug/scm/file`
- **Purpose:** read one file (SCM-FR-005).
- **Query:** `ref`, `path` (required).
- **Response:** `200` bytes or JSON; `206` for a range. **Errors:** `404 path_not_found`; `400` when `path` is a directory; `413` over the size limit (the JSON form names `size`).

#### `GET /workspaces/:slug/scm/diff`
- **Purpose:** the difference between two revisions.
- **Query:** `base` (required), `head` (required), `mode` (`merge_base` default, `direct`), `path` (repeatable glob filter), `format` (`json` default, `patch`), `context` (lines, default 3, max 20), `patches` (bool, default true; false returns the file list only).
- **Response (`json`):** `200 {"base_sha","head_sha","merge_base_sha","files":[<file change> + "patch"?], "stats", "truncated"}`. **Response (`patch`):** `200 text/x-diff`.

#### `GET /workspaces/:slug/scm/compare`
- **Purpose:** how two revisions relate.
- **Query:** `base`, `head` (both required).
- **Response:** `200 {"base_sha","head_sha","merge_base_sha","ahead","behind","commits":[…up to 200 commit objects of head not in base, oldest first…],"files":[<file change>…],"mergeable":"clean"|"conflict","conflict": <conflict report> | null}`. `mergeable` is the result of a trial rebase of `head` onto `base`.

#### `GET /workspaces/:slug/scm/blame`
- **Purpose:** who last changed each line (SCM-FR-008).
- **Query:** `ref`, `path` (required), `start_line`, `end_line`.
- **Response:** `200 {"resolved_sha","path","ranges":[…]}`. **Errors:** `404 path_not_found`; `413` over the blame limit; `400` for a binary file.

#### `GET /workspaces/:slug/scm/search`
- **Purpose:** search file contents at a ref (SCM-FR-009).
- **Query:** `q` (required, 1–500 characters), `ref`, `path` (repeatable glob), `regex` (bool), `case_sensitive` (bool, default true), `limit`, `cursor`.
- **Response:** `200` list of matches plus `resolved_sha`.

#### `POST /workspaces/:slug/scm/rebase`
- **Purpose:** rebase a branch on the server (SCM-FR-026).
- **Body:** `{"branch": string, "onto": string, "expected_head_sha"?: string, "session_id"?: string}`.
- **Response:** `200 {"status":"rebased"|"up_to_date","branch": <ref object>,"old_head_sha","new_head_sha","onto_sha","change": {...} | null}`.
- **Errors:** `409 ref_moved`; `409 rebase_conflict` with the conflict report in `details`; `400 merge_commits_unsupported`; push-rule refusals.

### 6.6 CLI

The CLI covers the calls a person or a script needs most. The remaining routes (ref create, move and delete) are REST only.

#### `parley scm refs <slug>`
- **Flags:** `--prefix <string>`, `--limit`, `--cursor`. **Calls:** `GET …/scm/refs`.

#### `parley scm log <slug>`
- **Flags:** `--ref`, `--path`, `--follow`, `--since`, `--until`, `--author`, `--stats`, `--limit`, `--cursor`. **Calls:** `GET …/scm/commits`.

#### `parley scm show <slug> <rev>`
- **Flags:** `--files`. **Calls:** `GET …/scm/commits/:sha`.

#### `parley scm ls <slug> [path]`
- **Flags:** `--ref`, `--recursive`. **Calls:** `GET …/scm/tree`.

#### `parley scm cat <slug> <path>`
- **Flags:** `--ref`, `--output <file>` (default: stdout, raw bytes). **Calls:** `GET …/scm/file`. This is the one command whose stdout is not JSON.

#### `parley scm diff <slug> <base> <head>`
- **Flags:** `--mode merge_base|direct`, `--path` (repeatable), `--patch` (print unified diff text instead of JSON), `--name-only`. **Calls:** `GET …/scm/diff`.

#### `parley scm compare <slug> <base> <head>`
- **Calls:** `GET …/scm/compare`. **Flags:** `--fail-on-conflict` (exit 3 when `mergeable` is `conflict`).

#### `parley scm blame <slug> <path>`
- **Flags:** `--ref`, `--lines <start>:<end>`. **Calls:** `GET …/scm/blame`.

#### `parley scm search <slug> <query>`
- **Flags:** `--ref`, `--path` (repeatable), `--regex`, `--ignore-case`. **Calls:** `GET …/scm/search`.

#### `parley scm commit <slug>`
- **Purpose:** commit local files through the API without a clone.
- **Flags:** `--branch <name>` (required), `--base <ref>` (for a new branch), `--expected-head <sha>` (default: the branch's head read just before the commit), `--message <text>` (required), `--put <repo-path>=<local-file>` (repeatable), `--delete <repo-path>` (repeatable), `--move <from>=<to>` (repeatable), `--session <id>`, `--change-target <branch>`, `--title`, `--intent`, `--draft`.
- **Calls:** `GET …/scm/refs/*name` when `--expected-head` is absent, then `POST …/scm/commits`.
- **Exit codes:** `409 ref_moved` exits 1 with the envelope; the command does not retry by itself.

#### `parley scm rebase <slug> <branch>`
- **Flags:** `--onto <ref>` (required), `--expected-head <sha>`. **Calls:** `POST …/scm/rebase`. A conflict prints the envelope with the conflict report and exits 1.

### 6.7 Events

| Event type | When | Metadata |
|---|---|---|
| `hub.scm.commit` | A commit was created through the API | `branch`, `sha`, `parent_sha`, `files` (paths touched, at most 100) |
| `hub.git.push` | Any ref was created, moved or deleted through the API (same event as a git push, §5) | `ref`, `before`, `after`, `created`, `deleted`, `forced`, `via: "api"` |

Reads emit no events. They are counted in `parley_scm_requests_total{op,result}` (§14).

### 6.8 Configuration

| Key | Type | Default | Effect |
|---|---|---|---|
| `[scm].max_file_size` | size | `32MiB` | Largest file the file route returns |
| `[scm].max_commit_size` | size | `32MiB` | Largest commit-creation request body |
| `[scm].read_timeout` | duration | `30s` | Deadline of a read request |

### 6.9 Edge cases and error handling

- A `put` whose content equals the existing file is allowed; the commit is refused as empty only when no operation changes the tree.
- Two operations on the same path apply in order, so `put a` then `delete a` leaves no `a`.
- A `put` under a path that is currently a file (`a` is a file, put `a/b`) is `invalid_operation`. A `put` onto a path that is currently a directory is `invalid_operation`.
- Symlink content (`mode: 120000`) is the link target as text. Submodule entries can be read as tree entries of type `commit` and cannot be written.
- Committing to a branch of a fork that is a tracking branch is refused with `tracking_branch`; the response names the fork's base so the client can create its own branch.
- A request that names both `expected_head_sha` and `base_ref` for an existing branch ignores `base_ref`.
- A rebase where every commit of the branch is already contained in `onto` moves the branch to `onto` and reports `rebased` with zero replayed commits.
- Reads at a commit that is reachable only from a deleted branch still succeed by full commit id while the object exists. The hub keeps every commit that a change version or a land attempt refers to.
- A workspace in `pending` or `cloning` status answers every source control route with `409 workspace_busy`.

---

## 7. Changes and stacks

### 7.1 Purpose and scope

A change is Parley's unit of work. It says: "add the commits of this source branch to that target branch, for this reason". Everything downstream addresses the change: policy evaluates it, people and review agents approve its versions, landing consumes it, the audit log and the session record point at it.

A branch is still how commits travel. An agent pushes a branch with stock git, or creates commits through the API (§6). The change is the durable object on top: it keeps its number while the branch is rewritten, remembers every version, and records why the work was done and which session did it.

In scope: the change object and its lifecycle, versions, intent, the session link, stacks and restack, mergeability, the REST API and CLI for changes. Out of scope: landing, policy, approvals and checks (§8); overlap queries and claims (§9); the push options that open changes over git (§5).

### 7.2 User stories

**CHG-US-01** As an Agent, I want my pushed branch to become a change with a stable number, so that I and others can refer to the work while I keep rewriting the branch.

**CHG-US-02** As an Agent, I want to state the intent of a change once and have it travel with the change, so that a reviewer sees why before reading what.
- A change cannot leave `draft` without an intent when the workspace requires one.

**CHG-US-03** As a Developer, I want every push to a change's branch kept as a version, and to see what changed between two versions, so that I only re-read what moved.

**CHG-US-04** As an Agent, I want to build a second change on top of an unlanded first one, and have the hub keep the second current when the first is amended or lands.

**CHG-US-05** As an Agent, I want to be told as soon as my open change stops applying to its target, with the conflicting files, so that I can fix it before anyone tries to land it.

**CHG-US-06** As a Developer, I want to go from a session to the changes it produced and from a commit on `main` back to its change and session.

**CHG-US-07** As an Org member, I want to open a change against a workspace I can only read, from my own fork.

**CHG-US-08** As a Developer, I want to abandon a change and restore it later without losing its versions.

### 7.3 Concepts and data model

#### 7.3.1 Change

A change belongs to the workspace that holds its target branch. Its source branch is in the same workspace or in a task fork of it.

| Field | Type | Meaning |
|---|---|---|
| `workspace_slug` | string | Workspace of the target branch |
| `number` | int | Per-workspace sequence starting at 1. Never reused |
| `ref` | string | Display reference `<workspace_slug>#<number>`, for example `demo#12` |
| `title` | string | 1–200 characters, one line |
| `intent` | string | Why the change exists. Up to 16,000 characters. May be empty only while `draft`, or when the workspace setting `changes.require_intent` is false |
| `state` | enum | `draft`, `open`, `landed`, `abandoned` |
| `target_branch` | string | Branch name in `workspace_slug` |
| `source` | object | `{workspace_slug, branch}`. `workspace_slug` is the target workspace or one of its forks |
| `version` | int | Number of the current version |
| `head_sha` | string | Head commit of the current version |
| `base_sha` | string | Commit the current version is built on: the merge base with the target, or the head of the change below in a stack |
| `depends_on` | int or null | Number of the change directly below in a stack |
| `dependants` | int[] | Numbers of open or draft changes directly above |
| `author` | object | `{actor_type, actor_id, username}` of the credential that opened the change |
| `session_id` | string or null | Session of the current version |
| `sessions` | string[] | Every session that contributed a version |
| `mergeable` | enum | `clean`, `conflict` or `unknown` (not yet computed against the current target head) |
| `conflict` | object or null | Conflict report (§8.3) when `mergeable` is `conflict` |
| `needs_restack` | bool | True when the change below moved and the automatic restack failed |
| `stats` | object | `{commits, files_changed, insertions, deletions}` of the current version against `base_sha` |
| `paths` | string[] | Files touched by the current version, at most 2,000; `paths_truncated` says whether there were more |
| `landing` | object | Policy evaluation (§8): `{status, requirements[]}` |
| `auto_land` | bool | Land automatically when `landing.status` is `ready` and `mergeable` is `clean` (§8) |
| `labels` | string[] | Free-form, at most 20, each matching `^[a-z0-9][a-z0-9_.:/-]{0,62}$` |
| `landed` | object or null | `{sha, at, by, land_id, commits: [{version_sha, landed_sha}]}` once landed |
| `reverts` | int or null | Number of the change this one reverts (§8) |
| `reverted_by` | int or null | Number of an open or landed change that reverts this one |
| `created_at`, `updated_at` | timestamp | |

**State machine.**

| From | To | Trigger |
|---|---|---|
| (new) | `draft` | opened with `draft: true` |
| (new) | `open` | opened otherwise |
| `draft` | `open` | marked ready (`PATCH state: "open"`), provided title and intent rules hold |
| `open` | `draft` | `PATCH state: "draft"` |
| `open` | `landed` | a land attempt succeeds (§8) |
| `draft`, `open` | `abandoned` | abandon; or the source workspace (a fork) is deleted |
| `abandoned` | `open` | restore, while the source branch or the last version's commit still exists |

`landed` is terminal. A draft is never landed and is never restacked onto by `auto_land`.

One source branch is the source of at most one change in state `draft` or `open`. Opening a second is `409`, `error_type: change_exists`, naming the existing change.

#### 7.3.2 Version

| Field | Type | Meaning |
|---|---|---|
| `number` | int | 1, 2, 3 … within the change |
| `head_sha` | string | Head commit of this version |
| `base_sha` | string | What this version was built on |
| `kind` | enum | `push` (git push), `commit` (API commit), `ref` (API ref move), `rebase` (server-side rebase requested by a client), `restack` (hub restack in a stack), `revert` (generated by a revert) |
| `patch_equivalent_to` | int or null | An earlier version whose diff against its own base is identical to this one's. A pure rebase is patch-equivalent to its predecessor |
| `created_by` | object | `{actor_type, actor_id, username}`; `actor_type: "system"` for hub restacks |
| `session_id` | string or null | |
| `stats` | object | As on the change |
| `created_at` | timestamp | |

- Every version's head stays fetchable at `refs/hub/changes/<number>/<version>` in the target workspace for as long as the change exists, whatever happens to the source branch. These refs are hub-owned (§5).
- A new version is recorded whenever the source branch's head changes while the change is `draft` or `open`, by any means. Moving the branch to a commit that is already the head of the current version records nothing.
- Versions are immutable.

#### 7.3.3 Stack

- A change **depends on** another when its commits sit on top of that change's commits. `depends_on` is set:
  - explicitly, at open or by `PATCH`; or
  - automatically at open and on every new version: if the nearest ancestor of the head that is the current head of another `draft` or `open` change with the same target branch exists, that change becomes `depends_on`; if none exists, `depends_on` is cleared.
- The **stack** of a change is the chain of `depends_on` links down to a change with none. A stack is at most 20 changes deep; a version that would make it deeper is still recorded, and the change gets `needs_restack: true` with `error_type: stack_too_deep` in its status.
- A dependent change's `base_sha` is the head of the change below. Its diff, stats, paths and policy evaluation cover only its own commits.
- All changes of a stack have the same target branch. They may come from different source workspaces.

#### 7.3.4 Mergeability

- `mergeable` answers: would the current version rebase cleanly onto the current head of the target branch (for a dependent change: onto the current head of the change below)?
- The hub recomputes it after every new version and after every move of the target branch. Recomputation is asynchronous. Until it finishes for the new pair of commits the value is `unknown`.
- A transition to `conflict` stores the conflict report on the change and emits `hub.change.conflict`. A transition back to `clean` clears it and emits `hub.change.ready` when the landing status is also `ready`.
- Landing never trusts a stored `mergeable` value. It always rebases for real (§8).

### 7.4 Functional requirements

Opening

- **CHG-FR-001** A change is opened in three ways, all with the same result: `POST /workspaces/:slug/changes`; a git push with change options or into an `auto_change` fork (§5); an API commit with a `change` object or into an `auto_change` fork (§6).
- **CHG-FR-002 (who may open).** The caller needs `changes:write` (PATs), write access to the source workspace and at least read access to the target workspace. This lets an org member open a change from their fork into a workspace they can only read.
- **CHG-FR-003 (validation at open).** In this order: target branch exists in the target workspace (`400`, `error_type: target_not_found`); source workspace is the target workspace or a fork of it (`400`, `error_type: source_not_related`); source branch exists (`400`, `error_type: source_not_found`); source and target are not the same branch of the same workspace (`400`); source has at least one commit that the target does not contain (`400`, `error_type: nothing_to_land`); no other `draft` or `open` change has this source (`409 change_exists`); title rules; intent rule unless `draft`.
- **CHG-FR-004 (defaults at open).** `title` defaults to the subject of the head commit. `intent` defaults to the body of the head commit, then to the `purpose` of the source fork. `target_branch` defaults to the fork's base branch for a fork source, otherwise to the workspace's default branch. `session_id` defaults to the session given on the push or commit that triggered the open, then to the session recorded on the source fork.
- **CHG-FR-005** A change may target any branch of the workspace, protected or not. In a carry-patch workspace it may target a patch branch but never the integration branch (`400`, `error_type: hub_owned_ref`).
- **CHG-FR-006** Opening records version 1, computes the stack link (§7.3.3), evaluates policy (§8), schedules the mergeability check and emits `hub.change.version` for version 1. `hub.change.open` is emitted when the change enters `open`: at once for a change created open, and when a draft is marked ready.

Versions

- **CHG-FR-010** A version is recorded by the hub as a consequence of the ref update, in the same transaction, so that a change never misses a head. The client does nothing extra.
- **CHG-FR-011** A new version: updates `head_sha`, `base_sha`, `stats`, `paths`, `session_id`; recomputes the stack link; re-evaluates policy; resets `mergeable` to `unknown` and schedules the check; carries approvals over only when it is patch-equivalent to the approved version (§8); marks earlier check results as belonging to their own version; emits `hub.change.version`.
- **CHG-FR-012** A version created with a path-scoped token (§3) is refused before the ref moves when it touches a path outside the token's `paths`; the refusal is the push rule of §5.
- **CHG-FR-013** When the source branch is deleted while the change is `draft` or `open`, the change is abandoned with reason `source_deleted`. The API refuses such a delete (§6); a git push that deletes the branch is accepted and has this effect.
- **CHG-FR-014 (interdiff).** The difference between two versions of a change is the difference between their diffs, with rebases factored out: a version that is patch-equivalent to another has an empty interdiff. The response lists per file whether it changed between the versions and gives the patch of that difference.

Editing and lifecycle

- **CHG-FR-020** `title`, `intent`, `labels`, `auto_land`, `state` (`draft` ⇄ `open`), `target_branch` and `depends_on` can be edited by the author, by anyone with write access to the target workspace, and by the admin token. Editing needs `changes:write`.
- **CHG-FR-021** Retargeting (`target_branch`) is allowed while `draft` or `open` and when the change has no `depends_on` and no dependants. It re-evaluates policy and mergeability and emits `hub.change.version` with `kind: "retarget"` in the metadata without creating a version.
- **CHG-FR-022 (abandon).** Abandoning a change with dependants needs `dependants=reparent` (the dependants are restacked onto the change below, or onto the target, without the abandoned change's commits) or `dependants=abandon` (they are abandoned too). Without the parameter the answer is `409`, `error_type: has_dependants`. Abandon never deletes the source branch or any version.
- **CHG-FR-023 (restore).** Restoring an abandoned change moves it to `open` at its last version. If the source branch no longer exists it is recreated at that version's head. If another `draft` or `open` change now uses that source, the answer is `409 change_exists`.
- **CHG-FR-024** A landed change is read-only except for `labels`.

Stacks and restack

- **CHG-FR-030 (restack on a new version below).** When a change with dependants gets a new version, the hub rebases each direct dependant's own commits onto the new head, moves the dependant's source branch with a compare-and-swap, and records a version of kind `restack`. This continues up the stack. It runs in the background shortly after the triggering version and is not part of the push or commit that caused it.
- **CHG-FR-031 (restack on landing below).** When a change lands, each direct dependant is rebased onto the landed commit on the target, its `depends_on` is cleared, and a `restack` version is recorded. Changes further up keep depending on the change directly below them and are restacked in turn.
- **CHG-FR-032 (restack failure).** If the rebase conflicts, nothing moves for that change or anything above it. The change gets `needs_restack: true`, `mergeable: conflict` and a conflict report, and `hub.change.conflict` is emitted. A later version pushed by the author that sits on the new head below clears the flag.
- **CHG-FR-033 (restack and the author's branch).** A restack moves the source branch on the hub. The author's local branch is then behind; a `git pull --rebase` or a fetch of `refs/hub/changes/<number>/<version>` brings it up to date. If the source branch moved between the hub reading it and the compare-and-swap, the restack is retried on the new head.
- **CHG-FR-034 (manual restack).** `POST …/changes/:number/restack` asks for the same operation on demand and returns its result synchronously: the new version, or `409 rebase_conflict` with the report. For a change with no `depends_on` it rebases onto the current target head.
- **CHG-FR-035** `auto_restack` is a workspace setting (`changes.auto_restack`, default true). When false, CHG-FR-030 and CHG-FR-031 only set `needs_restack` and leave the rebase to the manual call.

Reading

- **CHG-FR-040** Reading changes needs `changes:read` and read access to the target workspace. A caller with access only to a fork sees the changes whose source is that fork: through the fork's slug (`GET /workspaces/<fork>/changes` lists changes whose source workspace is the fork), and, with a token bound to the fork, through the parent's change routes for exactly those changes (§3). The same token may edit, restack, land (§8) and abandon them where this section and §8 allow the author to.
- **CHG-FR-041** List filters: `state` (repeatable; default `draft` and `open`), `target_branch`, `source_workspace`, `author` (user id or `me`), `session_id`, `label`, `path` (glob; changes touching a matching file), `mergeable`, `landing_status`, `stack_of=<number>` (every change in that change's stack, bottom first). Order: `updated_at` descending, or `number` ascending with `sort=number`.
- **CHG-FR-042** `GET /workspaces/:slug/scm/commits/:sha` (§6) names the change of any commit that is a version head or was landed by a change. `GET …/changes/by-commit/:sha` returns that change directly.
- **CHG-FR-043 (session link).** A session record (§14) lists the changes it contributed versions to. A change lists every session that contributed. A `session_id` on a change or version MUST satisfy the session-linking rule of §14; otherwise the triggering request is refused with `400`, `error_type: unknown_session`.

Retention

- **CHG-FR-050** Changes and their versions are kept for the life of the workspace. Deleting the workspace deletes them. Deleting a fork abandons the `draft` and `open` changes whose source it was; landed and abandoned changes and all version refs stay in the target workspace.

### 7.5 REST API

All routes are under `/workspaces/:slug`. The conventions of §3 apply. Common errors: `401`; `403` missing scope or read-only member on a write; `404 workspace not found`; `404`, `error_type: change_not_found`.

#### `POST /workspaces/:slug/changes`
- **Purpose:** open a change targeting a branch of `:slug`.
- **Auth / scope:** `changes:write`; CHG-FR-002.
- **Body:** `{"source": {"workspace_slug"?: string, "branch": string}, "target_branch"?: string, "title"?: string, "intent"?: string, "draft"?: bool, "depends_on"?: int, "auto_land"?: bool, "labels"?: string[], "session_id"?: string}`. `source.workspace_slug` defaults to `:slug`.
- **Response:** `201` change object.
- **Errors:** CHG-FR-003; `400`, `error_type: intent_required`.
- **Side effects:** `hub.change.version`; `hub.change.open` unless `draft`.

#### `GET /workspaces/:slug/changes`
- **Purpose:** list changes (CHG-FR-040, CHG-FR-041).
- **Response:** `200` list of change objects without `conflict` and with `paths` omitted unless `include=paths`.

#### `GET /workspaces/:slug/changes/:number`
- **Purpose:** read one change.
- **Query:** `include` (comma list of `versions`, `approvals`, `checks`, `stack`, `overlaps`).
- **Response:** `200` change object, with the requested expansions.

#### `GET /workspaces/:slug/changes/by-commit/:sha`
- **Purpose:** find the change that a commit belongs to.
- **Response:** `200 {"change": <change object>, "version": int | null, "landed": bool}`. **Errors:** `404 change_not_found`.

#### `PATCH /workspaces/:slug/changes/:number`
- **Purpose:** edit a change (CHG-FR-020, CHG-FR-021).
- **Body:** any of `title`, `intent`, `labels`, `auto_land`, `state` (`draft` or `open`), `target_branch`, `depends_on` (int or null).
- **Response:** `200` change object.
- **Errors:** `400 intent_required`; `400`, `error_type: invalid_dependency` (unknown change, different target, would form a cycle, not `draft` or `open`); `409`, `error_type: change_closed` for a landed or abandoned change.

#### `POST /workspaces/:slug/changes/:number/abandon`
- **Purpose:** abandon a change (CHG-FR-022).
- **Body:** `{"reason"?: string, "dependants"?: "reparent" | "abandon"}`.
- **Response:** `200` change object. **Errors:** `409 has_dependants`; `409 change_closed`.
- **Side effects:** `hub.change.abandon`; restack of reparented dependants.

#### `POST /workspaces/:slug/changes/:number/restore`
- **Purpose:** restore an abandoned change (CHG-FR-023).
- **Response:** `200` change object. **Errors:** `409 change_exists`; `409`, `error_type: change_not_abandoned`.
- **Side effects:** `hub.change.restore`.

#### `POST /workspaces/:slug/changes/:number/restack`
- **Purpose:** rebase the change onto what it now sits on (CHG-FR-034).
- **Body:** `{"session_id"?: string, "resolutions"?: [...]}`. `resolutions` supplies the content of conflicted files (§8.4).
- **Response:** `200 {"status": "restacked" | "up_to_date", "change": <change object>}`.
- **Errors:** `409 rebase_conflict` with the conflict report in `details`; `409 change_closed`.
- **Side effects:** a `restack` version; `hub.change.restack`.

#### `GET /workspaces/:slug/changes/:number/versions`
- **Purpose:** list the versions of a change, oldest first.
- **Response:** `200` list of version objects.

#### `GET /workspaces/:slug/changes/:number/versions/:version`
- **Purpose:** read one version. `:version` is a number or `latest`.
- **Response:** `200` version object plus `ref` (`refs/hub/changes/<number>/<version>`).

#### `GET /workspaces/:slug/changes/:number/diff`
- **Purpose:** the change's own diff, or the difference between two versions.
- **Query:** `version` (default `latest`), `against` (a version number: return the interdiff of CHG-FR-014; absent: the version against its base), `path` (repeatable glob), `format` (`json` default, `patch`), `patches` (bool).
- **Response:** as `GET …/scm/diff` (§6), plus `version`, `against`.

#### `GET /workspaces/:slug/changes/:number/stack`
- **Purpose:** the whole stack the change is part of.
- **Response:** `200 {"target_branch", "changes": [<change summary>…]}`, bottom first. A summary has `number`, `title`, `state`, `version`, `head_sha`, `mergeable`, `needs_restack`, `landing.status`.

### 7.6 CLI

`<slug>` is the target workspace. `<number>` is the change number.

#### `parley change open <slug>`
- **Flags:** `--source <branch>` (required), `--source-workspace <slug>`, `--target <branch>`, `--title`, `--intent` (text, or `@file`, or `-` for stdin), `--draft`, `--depends-on <number>`, `--auto-land`, `--label` (repeatable), `--session <id>`.
- **Calls:** `POST …/changes`.

#### `parley change list <slug>`
- **Flags:** `--state` (repeatable), `--target`, `--author`, `--session`, `--label`, `--path`, `--mergeable`, `--landing-status`, `--stack-of <number>`, `--limit`, `--cursor`.
- **Calls:** `GET …/changes`.

#### `parley change show <slug> <number>`
- **Flags:** `--include <list>`. **Calls:** `GET …/changes/:number`.

#### `parley change find <slug> <sha>`
- **Calls:** `GET …/changes/by-commit/:sha`.

#### `parley change edit <slug> <number>`
- **Flags:** `--title`, `--intent`, `--label` / `--remove-label` (repeatable), `--auto-land` / `--no-auto-land`, `--target`, `--depends-on <number>` / `--no-depends-on`.
- **Calls:** `PATCH …/changes/:number`.

#### `parley change ready <slug> <number>`
- **Purpose:** move a draft to `open`. **Calls:** `PATCH …/changes/:number` with `{"state":"open"}`.

#### `parley change draft <slug> <number>`
- **Purpose:** move an open change back to `draft`. **Calls:** `PATCH …/changes/:number` with `{"state":"draft"}`.

#### `parley change abandon <slug> <number>`
- **Flags:** `--reason`, `--dependants reparent|abandon`. **Calls:** `POST …/changes/:number/abandon`.

#### `parley change restore <slug> <number>`
- **Calls:** `POST …/changes/:number/restore`.

#### `parley change restack <slug> <number>`
- **Calls:** `POST …/changes/:number/restack`. A conflict prints the envelope with the conflict report and exits 1.

#### `parley change versions <slug> <number>`
- **Calls:** `GET …/changes/:number/versions`.

#### `parley change diff <slug> <number>`
- **Flags:** `--version <n>`, `--against <n>`, `--path` (repeatable), `--patch`, `--name-only`.
- **Calls:** `GET …/changes/:number/diff`.

#### `parley change stack <slug> <number>`
- **Calls:** `GET …/changes/:number/stack`.

#### `parley change wait <slug> <number>`
- **Purpose:** block until the change reaches a condition.
- **Flags:** `--for landed|ready|mergeable` (default `landed`), `--timeout`, `--poll-interval`.
- **Exit codes:** 0 when the condition holds; 1 when the change is abandoned, or on timeout; 3 when waiting for `ready` or `mergeable` and the change becomes `blocked` or `conflict`.

### 7.7 Background behaviour

- **Mergeability checks** run after each new version and each move of a target branch that has `draft` or `open` changes. Checks for the same change coalesce: only the latest pair of commits is computed. On a busy target, checks are debounced by `[changes].mergeable_debounce` (default 5 s).
- **Restacks** (CHG-FR-030, CHG-FR-031) are durable: one that was pending at shutdown is run after the next start.
- **Start-up reconciliation.** At start the hub compares each `draft` or `open` change's current version with its source branch head and records a version for any head it missed, so that a crash between a ref update and its bookkeeping cannot lose a version.
- **Fork deletion and expiry** abandon the fork's `draft` and `open` changes (CHG-FR-050).

### 7.8 Events

All events have `resource_type: "change"`, `resource_id: "<number>"`, the target workspace as `workspace`, and `session_id` when known. Every event's metadata includes `ref` (`demo#12`), `version` and `head_sha`.

| Event type | When | Further metadata |
|---|---|---|
| `hub.change.open` | A change entered `open`: it was created open, or a draft was marked ready | `title`, `target_branch`, `source_workspace`, `source_branch`, `depends_on`, `was_draft` |
| `hub.change.version` | A version was recorded (including version 1 of a draft), or the change was retargeted | `kind`, `base_sha`, `patch_equivalent_to`, `draft` |
| `hub.change.restack` | The hub restacked the change | `onto_sha`, `trigger` (`version`, `land`, `manual`, `abandon`) |
| `hub.change.conflict` | `mergeable` became `conflict`, or a restack failed | `files` (paths), `needs_restack` |
| `hub.change.ready` | The change became landable: `landing.status` is `ready` and `mergeable` is `clean` | |
| `hub.change.abandon` | Abandoned | `reason` |
| `hub.change.restore` | Restored | |

Landing, approval, check, overlap and revert events are listed in §8 and §9.

### 7.9 Configuration

| Key | Type | Default | Effect |
|---|---|---|---|
| `[changes].mergeable_debounce` | duration | `5s` | Minimum spacing of mergeability recomputation per change |
| `[changes].max_stack_depth` | int | `20` | Deepest allowed stack |

Workspace settings (§4): `changes.require_intent` (default true), `changes.auto_restack` (default true).

### 7.10 Edge cases and error handling

- **A push that rewrites the branch completely** (unrelated history) is still a new version of the same change. Its interdiff against the previous version is simply large.
- **A push that makes the source equal to or behind the target** (nothing left to land) records the version and sets `landing.status` to `blocked` with reason `nothing_to_land`. The change stays open so that the next push can revive it.
- **The target branch is deleted.** The change stays `open` with `mergeable: unknown` and `landing.status: blocked`, reason `target_not_found`. It can be retargeted.
- **Two changes with the same commits** (one branch pushed under two names) are two changes. The second to land finds its commits already in the target and lands as a no-op (§8).
- **Stack links and force pushes.** If a dependant is force-pushed onto the target directly, its `depends_on` is cleared by the automatic rule. If the lower change is force-pushed to an unrelated head, dependants are restacked onto it by patch replay; this may conflict and set `needs_restack`.
- **Cycles** cannot arise from the automatic rule. An explicit `depends_on` that would create one is refused with `invalid_dependency`.
- **A change from a fork whose owner loses read access to the target workspace** stays open. The author can no longer read or edit it through the target's slug; the workspace owner can abandon it.
- **Number allocation** is gap-free under normal operation, but a failed open may consume a number. Clients MUST NOT assume numbers are contiguous.
- **Very large changes.** `paths` is capped at 2,000 entries; policy (§8) is still evaluated over every touched path.

---

## 8. Landing and policy

### 8.1 Purpose and scope

Landing adds a change's commits to its target branch. In Parley it is **optimistic**: the hub rebases the change onto the current head of the target on the server and then moves the target with a compare-and-swap. There is no queue and no per-workspace lock. Two changes that touch different things land at the same time. If the target moved in between, the hub simply tries again on the new head. Only a real conflict stops a land, and it comes back as a structured report that an agent can act on.

Because nothing is serialized, safety comes from two other places. **Policy** decides before landing what each change needs, by the paths it touches: nothing, named checks, a person's approval, or a block. And **after** landing, every land is an event, every landed commit maps back to its change, and a landed change can be reverted with one call.

In scope: land attempts, the landing algorithm, conflict reports and server-side resolution, protected branches, policy and its evaluation, approvals, checks, automatic landing, revert, path-scoped tokens as they affect landing. Out of scope: the change object (§7); running checks (external runners report them); publishing landed branches to origin (§4).

### 8.2 User stories

**LAND-US-01** As an Agent, I want to land my change with one call and not wait behind unrelated changes.
- Two lands on the same target that do not conflict both succeed, in either order, without either caller retrying.

**LAND-US-02** As an Agent, I want a conflict returned as data: which files, what kind of conflict, and which landed change I collided with, so that I can fix it and try again.
- A conflicting land changes nothing on the target.

**LAND-US-03** As an Agent without a clone, I want to fetch the conflicted file with markers and submit my resolution through the API.

**LAND-US-04** As a Developer, I want to say "docs land by themselves, `billing/**` needs me, everything needs the `test` check", once, per workspace.

**LAND-US-05** As a Developer, I want to approve a version of a change, and have my approval survive a pure rebase but not a content change.

**LAND-US-06** As a Check runner, I want to report `passed` or `failed` for a named check on a version using a token that can do nothing else.

**LAND-US-07** As an Agent, I want to set `auto_land` and walk away: the change lands as soon as policy is satisfied.

**LAND-US-08** As a Developer, I want to ask "what would this change need?" before it exists, and I want agents to be able to ask the same.

**LAND-US-09** As a Developer, I want to revert a landed change with one call when something breaks afterwards.

**LAND-US-10** As a Developer, I want to give an agent a token that can only change files under `services/search/**`.

**LAND-US-11** As an Operator, I want a land that was in flight during a restart to finish or fail cleanly afterwards, never to half-apply.

### 8.3 Concepts and data model

#### 8.3.1 Protected branches

- The workspace setting `protected_branches` (§4) is a list of branch globs. Its default is the default branch.
- A protected branch moves in exactly two ways: a successful land, and a sync from origin (§4). Pushes, API commits, API ref moves and deletes are refused for every caller, including the owner (§5, §6).
- Changes may also target unprotected branches. They land the same way; the branch can additionally be pushed directly.

#### 8.3.2 Land attempt

| Field | Type | Meaning |
|---|---|---|
| `id` | string | UUID |
| `workspace_slug` | string | Target workspace |
| `target_branch` | string | |
| `changes` | int[] | The changes this attempt lands, bottom first. One element unless a stack is landed |
| `status` | enum | `running`, `landed`, `conflict`, `blocked`, `failed` |
| `trigger` | enum | `manual`, `auto_land` |
| `strategy` | enum | `rebase` or `squash` (from the workspace setting `land.strategy`) |
| `attempts` | int | How many times the compare-and-swap was tried |
| `onto_sha` | string or null | The target head the successful (or last) try was built on |
| `landed_sha` | string or null | New target head when `landed` |
| `noop` | bool | True when every commit was already in the target and the target did not move |
| `commits` | object[] | `{change, version_sha, landed_sha}` for each landed commit; with `squash`, one entry per change |
| `conflict` | object or null | Conflict report when `status` is `conflict` |
| `blocked` | object or null | `{change, status, requirements[]}` when `status` is `blocked` |
| `error` | string or null | Reason when `failed` (`contention`, `timeout`, `internal`) |
| `override` | object or null | `{reason}` when policy was overridden |
| `policy_revision` | int | Revision of the policy that was evaluated |
| `requested_by` | object | `{actor_type, actor_id, username}` |
| `session_id` | string or null | |
| `created_at`, `finished_at` | timestamp | |

`landed`, `conflict`, `blocked` and `failed` are final. A new attempt is a new call.

#### 8.3.3 Conflict report

The same shape is used wherever a rebase can fail: landing, restack (§7), server-side rebase (§6), comparison (§6), revert, and carry-patch rebuilds (§10).

| Field | Type | Meaning |
|---|---|---|
| `target_sha` | string | The commit the work was being rebased onto |
| `head_sha` | string | The head that was being rebased |
| `base_sha` | string | The commit the work was previously based on |
| `commit_sha` | string | The first commit of the work that did not apply |
| `files` | object[] | One entry per conflicted file, see below |
| `landed_changes` | object[] | `{number, title, landed_sha, session_id}`: changes landed between `base_sha` and `target_sha` that touched a conflicted file. Empty when unknown |
| `change` | int or null | In a stack land, the change that conflicted |

File entry:

| Field | Type | Meaning |
|---|---|---|
| `path` | string | Path in the target |
| `kind` | enum | `content`, `add_add`, `modify_delete`, `delete_modify`, `rename`, `mode`, `binary`, `submodule` |
| `base`, `ours`, `theirs` | object or null | `{sha, mode, path}` of the file in the common ancestor, in the target (`ours`) and in the change (`theirs`). Null where the file does not exist on that side |
| `landed_by` | int[] | Numbers of the landed changes that touched this path |

For `content` conflicts the hub can produce the file with conflict markers (`GET …/changes/:number/conflict/file`).

#### 8.3.4 Policy

A workspace has one policy document. It is data, set through the API, and versioned by a `revision` that increases on every update.

```json
{
  "revision": 7,
  "default": {"approvals": 0, "checks": ["test"]},
  "rules": [
    {"name": "docs",    "paths": ["docs/**", "**/*.md"], "require": {"approvals": 0, "checks": []}},
    {"name": "billing", "paths": ["billing/**"], "targets": ["main", "release/*"],
     "require": {"approvals": 1, "approvers": ["owner", "user:dana"], "checks": ["test", "billing-e2e"]}},
    {"name": "secrets", "paths": ["deploy/secrets/**"], "require": {"block": true}}
  ]
}
```

| Element | Type | Meaning |
|---|---|---|
| `rules[].name` | string | Unique within the policy, `^[a-z0-9][a-z0-9_-]{0,62}$` |
| `rules[].paths` | string[] | Globs (`*`, `**`, `?`) matched against repository-relative file paths. Required, non-empty |
| `rules[].exclude` | string[] | Globs that take files back out of the rule |
| `rules[].targets` | string[] | Branch globs. Absent means every target branch |
| `require.approvals` | int | 0–10. Number of counted approvals needed |
| `require.approvers` | string[] | Who may give counted approvals: `owner`, `member` (any member of the workspace's organization), `user:<username>`. Default `["owner"]` |
| `require.checks` | string[] | Names of checks that must be `passed` |
| `require.fresh_checks` | bool | Default false. When true, check results reported for an earlier, patch-equivalent version do not count |
| `require.block` | bool | Default false. When true, a change touching a matching file cannot land |
| `default` | requirement | Applies to files that no rule matches. Same fields as `require` |

Limits: at most 200 rules; the document is at most 64 KiB. A workspace that never set a policy has `{"revision": 0, "default": {"approvals": 0, "checks": []}, "rules": []}`: everything may land.

**Evaluation.** For a change:

1. Take every file the current version touches (both paths of a rename).
2. For each file, select the first rule in order whose `targets` match the change's target branch and whose `paths` match the file and whose `exclude` does not. If none, select `default`.
3. Each distinct selected rule (and `default`, if selected) yields one **requirement** on the change: `{rule, paths (the change's files it was selected for, at most 50), approvals: {required, counted, approvers}, checks: [{name, status}], block, satisfied}`.
4. A requirement is satisfied when it does not block, its counted approvals reach `required`, no counted rejection is outstanding, and every named check is `passed`.
5. `landing.status` is:
   - `blocked` when any requirement blocks, or the change cannot land for a structural reason (`reason`: `nothing_to_land`, `target_not_found`, `draft`);
   - `pending` when no requirement blocks and at least one is unsatisfied;
   - `ready` when all are satisfied.

Policy is evaluated against the change's own commits only. In a stack, each change is evaluated separately.

#### 8.3.5 Approval

| Field | Type | Meaning |
|---|---|---|
| `id` | string | UUID |
| `change`, `version` | int | The version the verdict is on |
| `verdict` | enum | `approve` or `reject` |
| `comment` | string | Up to 16,000 characters |
| `by` | object | `{actor_type, actor_id, username}` |
| `counts_for` | string[] | Names of the requirements this approval currently counts toward |
| `carried_from` | int or null | The version the verdict was first given on, when it was carried over |
| `created_at` | timestamp | |

- A user has at most one verdict per change version. A new verdict replaces the earlier one.
- **Counting.** An approval counts toward a requirement when its user is among the requirement's approvers and it was **not** given with the same credential that created the approved version. A person approving their agent's work with their own API key counts. An agent approving the version it just pushed does not.
- **Carry-over.** When a new version is patch-equivalent to an approved or rejected version (§7), the verdicts are carried to it. Any other new version starts with none.
- A counted `reject` keeps its requirement unsatisfied until its author replaces or withdraws it, or a version that is not patch-equivalent arrives.

#### 8.3.6 Check

| Field | Type | Meaning |
|---|---|---|
| `name` | string | `^[a-z0-9][a-z0-9_.-]{0,62}$` |
| `change`, `version` | int | |
| `status` | enum | `pending`, `passed`, `failed` |
| `summary` | string | Up to 2,000 characters |
| `url` | string | Optional link to the run |
| `details` | object | Optional free-form JSON, up to 16 KiB |
| `reported_by` | object | |
| `updated_at` | timestamp | |

One result per (version, name); a later report replaces it. A required check with no result counts as `pending`. Results carry over to patch-equivalent versions unless the requirement sets `fresh_checks`.

Checks are reported on a version, not on the rebased result of a land. This is deliberate: verifying every rebased result before it lands would serialize landing. Workspaces that need that guarantee for some paths use `fresh_checks` together with a runner that rebases before testing, and accept the wait.

### 8.4 Functional requirements

Who may land

- **LAND-FR-001** Landing needs `changes:write` (PATs) and one of: write access to the target workspace; or being the change's author while the workspace setting `land.by_author` is true (default). The second case is what lets an agent holding only a fork token land its own change once policy is satisfied.
- **LAND-FR-002** Nobody lands past policy, with one exception: the workspace owner or the admin token may pass `override: {"reason": "<text>"}`. Overrides skip unmet approvals and checks, never `block` rules, are recorded on the land attempt and in `hub.change.land`, and can be disabled per workspace with `land.allow_override: false`.

The landing algorithm

- **LAND-FR-010 (preconditions).** In this order: the change is `open` (`409`, `error_type: change_not_open`); `expected_version`, when given, equals the current version (`409`, `error_type: version_mismatch`); no other land attempt for this change is `running` (`409`, `error_type: land_in_progress`, naming it); every change below it in the stack is landed, unless `stack` is true (`409`, `error_type: depends_on_unlanded`).
- **LAND-FR-011 (policy gate).** Policy is evaluated for the change (each change, for a stack). If any is not `ready` and no valid override is given, the attempt ends `blocked`. The HTTP answer is `409`, `error_type: policy_blocked`, with the land attempt, including its requirements, in `details`.
- **LAND-FR-012 (rebase).** Read the target head *T*. Replay the change's own commits (the commits of the current version that are not in its base) onto *T*:
  - With `rebase`, each commit is replayed in order. Authors and messages are kept. The committer is the hub's landing identity. When the version already sits directly on *T*, the commits are used as they are and nothing is rewritten.
  - With `squash`, one commit is created per change. Its message is the change title, a blank line, the intent, and the trailers `Parley-Change: <slug>#<number>` and, when known, `Parley-Session: <id>`. Its author is the author of the version's head commit; other authors are listed as `Co-authored-by` trailers.
  - Commits whose content is already in *T* are dropped. If nothing is left, the attempt ends `landed` with `noop: true`, `landed_sha` = *T*, and the target is not moved.
  - Merge commits in the change are refused when the change is opened into a protected branch (`400`, `error_type: merge_commits_unsupported`), so they do not occur here.
- **LAND-FR-013 (conflict).** If a commit does not apply, the attempt ends `conflict` with a conflict report. The target is untouched. The change stays `open` with `mergeable: conflict`. The HTTP answer is `409`, `error_type: land_conflict`.
- **LAND-FR-014 (compare-and-swap).** Move the target from *T* to the new head only if it still is *T*. If it moved, go back to LAND-FR-012 with the new head. After `[land].max_attempts` tries (default 10) the attempt ends `failed` with `error: "contention"`; the caller may simply try again.
- **LAND-FR-015 (atomic bookkeeping).** The ref move, the change's transition to `landed`, the commit mapping, the land attempt's final status and the `hub.change.land` event are one atomic step. After a crash, the hub is in exactly one of two states: not landed, or landed with all of these recorded.
- **LAND-FR-016 (stack landing).** With `stack: true` the attempt covers the change and every unlanded change below it. All of them are replayed bottom first onto *T* and the target moves once, to the top. It is all or nothing: one conflict or one policy miss stops the whole attempt, and the report names the change.
- **LAND-FR-017 (after landing).** After a successful land, outside the atomic step and each best effort:
  1. delete the source branch when `land.delete_source` is true and no other `draft` or `open` change uses it;
  2. restack the change's dependants (§7);
  3. schedule mergeability checks for the other changes on the target (§7);
  4. start the early expiry clock of the source fork when all its changes are landed or abandoned (§4);
  5. in a carry-patch workspace, when the target is a patch branch, trigger a rebuild (§10).
- **LAND-FR-018 (no publication).** Landing changes only the hub's repository. Nothing is pushed to origin or to a forge. Publishing is the explicit branch push of §4.
- **LAND-FR-019 (durability and time).** A land attempt is durable. An attempt that was `running` when the hub stopped is run again from LAND-FR-010 at the next start; LAND-FR-015 makes this safe. One attempt is bounded by `[land].timeout` (default 5 min) and then ends `failed` with `error: "timeout"`.
- **LAND-FR-020 (answering the caller).** The land call waits for the attempt up to `wait` seconds (default 30, max 60). A final attempt is answered `200` when `landed` and with the `409` forms above for `conflict` and `blocked`; `failed` is `503`, `error_type: land_failed`. An attempt still running is answered `202` with the attempt, to be polled.
- **LAND-FR-021 (no waiting on others).** Land attempts for different changes MUST run concurrently, including on the same target branch. The only point of mutual exclusion is the compare-and-swap of LAND-FR-014.

Conflict resolution without a clone

- **LAND-FR-030** `GET …/changes/:number/conflict` returns the change's current conflict report, recomputed against the current target head when the stored one is stale.
- **LAND-FR-031** `GET …/changes/:number/conflict/file?path=` returns a `content`-conflicted file as text with standard conflict markers (`<<<<<<< target`, `=======`, `>>>>>>> change`), or `409`, `error_type: not_text_conflict` for other kinds.
- **LAND-FR-032** `POST …/changes/:number/restack` (§7) accepts `resolutions`: a list of `{path, content | content_base64}` or `{path, delete: true}`. During the replay, wherever a listed path conflicts, the hub takes the given resolution instead of failing. A conflict on a path that is not listed still fails the restack. Success records a new version of kind `restack`, authored by the caller, which then goes through policy like any other version.

Policy

- **LAND-FR-040** Reading the policy needs read access to the workspace and `changes:read`. Replacing it needs `workspaces:write` and owner or admin access. A fork has no policy of its own: changes are governed by the policy of their target workspace.
- **LAND-FR-041** `PUT` replaces the whole document, validates it (names, globs, limits, known approver forms, existing usernames), increases `revision`, and takes an optional `If-Match: <revision>` that fails with `412` when the stored revision differs. The last 50 revisions are kept and readable.
- **LAND-FR-042** After a policy update the hub re-evaluates every `draft` and `open` change of the workspace. Changes whose status changes emit `hub.change.ready` or get their new requirements silently; no approval or check is discarded.
- **LAND-FR-043 (evaluate).** `POST …/policy/evaluate` answers what a set of paths would need, without a change: input `{paths, target_branch?}` or `{change: <number>}`, optionally with a candidate `policy` document to try; output the requirements and the resulting status as they would be with no approvals and no checks.
- **LAND-FR-044** Policy is re-evaluated on: a new version, a retarget, an approval or its withdrawal, a check report, a policy update. The result is stored on the change as `landing`.

Approvals and checks

- **LAND-FR-050** Recording a verdict needs `changes:approve` (PATs) and read access to the target workspace. `changes:approve` is never implied by another scope. Anyone who can read the change may record a verdict; whether it counts is decided by §8.3.5.
- **LAND-FR-051** A verdict is given on the current version unless `version` names another. A verdict on a version that is neither current nor patch-equivalent to the current one is recorded and has no effect on landing.
- **LAND-FR-052** Reporting a check needs `checks:write` (PATs) and read access to the target workspace. `checks:write` is never implied by another scope. A report on an unknown version is `404`.
- **LAND-FR-053** Verdicts and checks cannot be given on `landed` or `abandoned` changes (`409 change_closed`), and can be given on drafts.

Automatic landing

- **LAND-FR-060** When a change with `auto_land: true` becomes `ready` and `clean`, the hub starts a land attempt with `trigger: "auto_land"`, acting as the user who set the flag. That user must be allowed to land at that moment (LAND-FR-001); otherwise the flag is cleared and `hub.change.land_fail` is emitted with reason `not_permitted`.
- **LAND-FR-061** An automatic attempt that ends `conflict` or `blocked` leaves `auto_land` on; the next transition to `ready` and `clean` tries again. One that ends `failed` is retried with backoff (2 s doubling to 5 min) up to 10 times, then `auto_land` is cleared.
- **LAND-FR-062** In a stack, automatic landing proceeds bottom up: a change is landed automatically only when the change below it is landed.

Revert

- **LAND-FR-070** `POST …/changes/:number/revert` on a landed change creates, on the server, a branch `revert/<number>` in the target workspace holding commits that undo the landed commits in reverse order, and opens a change from it with `reverts: <number>`, a generated title (`Revert "<title>"`) and an intent that names the original change. A suffix (`revert/<number>-2`) is used when the branch exists.
- **LAND-FR-071** The reverting change is an ordinary change: policy applies, and `auto_land` may be requested in the same call. If the landed commits can no longer be undone cleanly the answer is `409`, `error_type: revert_conflict`, with a conflict report, and nothing is created.
- **LAND-FR-072** Reverting needs `changes:write` and write access to the target workspace, or authorship of the original change when `land.by_author` is true.

Path-scoped tokens

- **LAND-FR-080** A PAT with `paths` (§3) can create a version only if every file that version changes matches one of its globs. This is enforced where the version is created: at git push (§5), at API commit and ref move (§6), and for `resolutions` in a restack. It is not re-checked at landing.
- **LAND-FR-081** A path-scoped token can land and revert changes only when every file of the change matches its globs.
- **LAND-FR-082** Path limits and policy are independent. A token limited to `services/search/**` still needs whatever policy requires for those paths.

### 8.5 REST API

All routes are under `/workspaces/:slug`, where `:slug` is the target workspace. The conventions of §3 apply. Common errors: `401`; `403`; `404 workspace not found`; `404 change_not_found`.

#### `POST /workspaces/:slug/changes/:number/land`
- **Purpose:** land a change, or its stack.
- **Auth / scope:** `changes:write`; LAND-FR-001.
- **Body:** `{"expected_version"?: int, "stack"?: bool, "override"?: {"reason": string}, "session_id"?: string}`. **Query:** `wait` (seconds, 0–60, default 30).
- **Response:** `200` land attempt (`landed`); `202` land attempt (`running`).
- **Errors:** `409 change_not_open`, `version_mismatch`, `land_in_progress`, `depends_on_unlanded`, `policy_blocked`, `land_conflict` (each with the land attempt in `details` where one was created); `403`, `error_type: override_not_allowed`; `503 land_failed`.
- **Side effects:** LAND-FR-015, LAND-FR-017; `hub.change.land` or `hub.change.land_fail`.

#### `GET /workspaces/:slug/changes/:number/lands`
- **Purpose:** list the land attempts of a change, newest first.
- **Auth / scope:** `changes:read`. **Response:** `200` list of land attempts.

#### `GET /workspaces/:slug/lands`
- **Purpose:** list land attempts of the workspace.
- **Query:** `status` (repeatable), `target_branch`, `since`, `limit`, `cursor`. Newest first.
- **Response:** `200` list of land attempts.

#### `GET /workspaces/:slug/lands/:id`
- **Purpose:** read one land attempt (used to poll a `202`).
- **Response:** `200` land attempt. **Errors:** `404`, `error_type: land_not_found`.

#### `GET /workspaces/:slug/changes/:number/conflict`
- **Purpose:** the change's current conflict report (LAND-FR-030).
- **Response:** `200 {"mergeable": "clean" | "conflict", "conflict": <conflict report> | null}`.

#### `GET /workspaces/:slug/changes/:number/conflict/file`
- **Purpose:** one conflicted file with markers (LAND-FR-031).
- **Query:** `path` (required). **Response:** `200 text/plain`. **Errors:** `404 path_not_found` when the path is not conflicted; `409 not_text_conflict`.

#### `GET /workspaces/:slug/policy`
- **Purpose:** read the policy. **Query:** `revision` (default: current).
- **Response:** `200` policy document, with `ETag: "<revision>"`, `updated_at`, `updated_by`.

#### `PUT /workspaces/:slug/policy`
- **Purpose:** replace the policy (LAND-FR-041).
- **Auth / scope:** `workspaces:write`; owner or admin.
- **Body:** `{"default": requirement, "rules": [...]}`.
- **Response:** `200` policy document with the new `revision`.
- **Errors:** `400`, `error_type: invalid_policy`, naming the rule and field; `412` revision mismatch; `400` on a fork (`error_type: not_supported_on_fork`).
- **Side effects:** `hub.policy.update`; re-evaluation of open changes.

#### `POST /workspaces/:slug/policy/evaluate`
- **Purpose:** what would these paths, or this change, need (LAND-FR-043).
- **Auth / scope:** `changes:read`.
- **Body:** `{"paths"?: string[], "target_branch"?: string, "change"?: int, "policy"?: <policy document>}`. Exactly one of `paths`, `change`.
- **Response:** `200 {"policy_revision", "status", "requirements": [...]}`.

#### `POST /workspaces/:slug/changes/:number/approvals`
- **Purpose:** approve or reject a version.
- **Auth / scope:** `changes:approve`.
- **Body:** `{"verdict": "approve" | "reject", "version"?: int, "comment"?: string}`.
- **Response:** `201` approval (or `200` when it replaces the caller's earlier verdict on that version).
- **Errors:** `409 change_closed`; `404` unknown version.
- **Side effects:** `hub.change.approve` or `hub.change.reject`; policy re-evaluation.

#### `GET /workspaces/:slug/changes/:number/approvals`
- **Purpose:** list verdicts. **Query:** `version` (default: all). **Response:** `200` list of approvals, newest first.

#### `DELETE /workspaces/:slug/changes/:number/approvals/:id`
- **Purpose:** withdraw a verdict. Only its author, the workspace owner or the admin token may.
- **Response:** `204`. **Side effects:** policy re-evaluation.

#### `PUT /workspaces/:slug/changes/:number/checks/:name`
- **Purpose:** report a check result.
- **Auth / scope:** `checks:write`.
- **Body:** `{"status": "pending" | "passed" | "failed", "version"?: int, "summary"?: string, "url"?: string, "details"?: object}`.
- **Response:** `200` check.
- **Side effects:** `hub.change.check`; policy re-evaluation.

#### `GET /workspaces/:slug/changes/:number/checks`
- **Purpose:** list check results. **Query:** `version` (default: current, including carried-over results). **Response:** `200` list of checks, by name.

#### `POST /workspaces/:slug/changes/:number/revert`
- **Purpose:** open a change that reverts a landed change (LAND-FR-070).
- **Body:** `{"title"?: string, "intent"?: string, "draft"?: bool, "auto_land"?: bool, "session_id"?: string}`.
- **Response:** `201` the new change.
- **Errors:** `409`, `error_type: change_not_landed`; `409 revert_conflict`; `409`, `error_type: already_reverted`, naming the open or landed reverting change.
- **Side effects:** a new branch and change; `hub.change.revert` on the original; `hub.change.open` on the new one.

### 8.6 CLI

#### `parley change land <slug> <number>`
- **Flags:** `--stack`, `--expected-version <n>`, `--override <reason>`, `--session <id>`, `--wait` (poll a running attempt to the end), `--timeout`, `--poll-interval`.
- **Calls:** `POST …/changes/:number/land`, then `GET …/lands/:id` with `--wait`.
- **Output:** the land attempt. A conflict or policy block prints the envelope, which contains the attempt with its report or requirements.
- **Exit codes:** 0 `landed`; 1 any other final status, or timeout; 2 usage.

#### `parley change lands <slug> <number>`
- **Calls:** `GET …/changes/:number/lands`.

#### `parley land list <slug>`
- **Flags:** `--status` (repeatable), `--target`, `--since`, `--limit`, `--cursor`. **Calls:** `GET …/lands`.

#### `parley land show <slug> <land-id>`
- **Calls:** `GET …/lands/:id`.

#### `parley change conflict <slug> <number>`
- **Flags:** `--file <path>` (print that file with conflict markers instead of the report), `--fail-on-conflict` (exit 3).
- **Calls:** `GET …/changes/:number/conflict` or `GET …/changes/:number/conflict/file`.

#### `parley change resolve <slug> <number>`
- **Purpose:** restack with resolutions, without a clone.
- **Flags:** `--file <repo-path>=<local-file>` (repeatable), `--delete <repo-path>` (repeatable).
- **Calls:** `POST …/changes/:number/restack` with `resolutions`.

#### `parley change approve <slug> <number>`
- **Flags:** `--version <n>`, `--comment <text>`. **Calls:** `POST …/changes/:number/approvals` with `verdict: "approve"`.

#### `parley change reject <slug> <number>`
- **Flags:** `--version <n>`, `--comment <text>` (required). **Calls:** `POST …/changes/:number/approvals` with `verdict: "reject"`.

#### `parley change approvals <slug> <number>`
- **Flags:** `--version <n>`, `--withdraw <id>`. **Calls:** `GET …/changes/:number/approvals` or `DELETE …/approvals/:id`.

#### `parley change check <slug> <number> <name>`
- **Flags:** `--status pending|passed|failed` (required), `--version <n>`, `--summary`, `--url`. **Calls:** `PUT …/changes/:number/checks/:name`.

#### `parley change checks <slug> <number>`
- **Flags:** `--version <n>`. **Calls:** `GET …/changes/:number/checks`.

#### `parley change revert <slug> <number>`
- **Flags:** `--title`, `--intent`, `--draft`, `--auto-land`, `--session <id>`. **Calls:** `POST …/changes/:number/revert`.

#### `parley policy get <slug>`
- **Flags:** `--revision <n>`. **Calls:** `GET …/policy`.

#### `parley policy set <slug>`
- **Flags:** `--file <path>` (JSON or TOML; `-` for stdin; required), `--if-revision <n>`. **Calls:** `PUT …/policy`.

#### `parley policy evaluate <slug>`
- **Flags:** `--paths <list>` or `--change <number>`; `--target <branch>`; `--policy-file <path>`. **Calls:** `POST …/policy/evaluate`.

### 8.7 Background behaviour

- **Land attempts** run on a bounded pool (`[land].workers`, default 8). The pool limits how many rebases are computed at once; it never orders them. An attempt waiting for a worker is still `running`.
- **Recovery.** At start, every `running` attempt is run again (LAND-FR-019).
- **Automatic landing** reacts to `hub.change.ready` transitions (LAND-FR-060 to LAND-FR-062).
- **Retention.** Land attempts are kept for the life of the workspace. Policy revisions beyond the last 50 are dropped.

### 8.8 Events

All have `resource_type: "change"` and `resource_id: "<number>"` except `hub.policy.update` (`resource_type: "policy"`, `resource_id: "<revision>"`).

| Event type | When | Metadata |
|---|---|---|
| `hub.change.approve` | An approving verdict was recorded | `version`, `approval_id`, `counts_for` |
| `hub.change.reject` | A rejecting verdict was recorded | `version`, `approval_id`, `counts_for` |
| `hub.change.check` | A check result was reported | `version`, `name`, `status` |
| `hub.change.land` | A land attempt ended `landed` (one event per landed change) | `land_id`, `target_branch`, `onto_sha`, `landed_sha`, `strategy`, `attempts`, `noop`, `trigger`, `override`, `policy_revision` |
| `hub.change.land_fail` | A land attempt ended `conflict`, `blocked` or `failed` | `land_id`, `status`, `reason`, `files` (for conflicts) |
| `hub.change.revert` | A reverting change was opened for this change | `reverted_by` |
| `hub.policy.update` | The policy was replaced | `revision`, `rules` (count) |

A landed target branch also produces `hub.git.push` for that ref with `via: "land"` (§5), so that consumers following pushes see every movement of a branch in one place.

### 8.9 Configuration

| Key | Type | Default | Effect |
|---|---|---|---|
| `[land].max_attempts` | int | `10` | Compare-and-swap tries per attempt |
| `[land].timeout` | duration | `5m` | Upper bound of one attempt |
| `[land].workers` | int | `8` | Concurrent rebases |
| `[land].committer_name`, `[land].committer_email` | string | `Parley`, `parley@localhost` | Committer of rebased and squashed commits |

Workspace settings (§4): `protected_branches`, `land.strategy` (`rebase`), `land.by_author` (true), `land.delete_source` (true), `land.allow_override` (true).

### 8.10 Edge cases and error handling

- **Two lands race on one target, no conflict.** Both end `landed`. The second has `attempts: 2` and its commits sit on top of the first's.
- **Two lands race and conflict.** One ends `landed`. The other ends `conflict`, and its report's `landed_changes` names the first.
- **The same change is landed twice at once.** The second call gets `409 land_in_progress`. After the first finished, a further call gets `409 change_not_open`.
- **A new version arrives during a land.** The attempt lands the version it started with. The new version finds the change `landed`; it is recorded on the source branch only, and the push output says the change has already landed. Callers that must not land a stale version pass `expected_version`.
- **Policy changes during a land.** The attempt uses the revision read at LAND-FR-011 and records it.
- **Approval withdrawn during a land.** As above: the gate was passed.
- **The target branch is deleted or unprotected mid-attempt.** A deleted target ends the attempt `failed`. Protection is read once at the start.
- **Sync from origin moves a protected branch** (§4). To landing this is just another mover: a running attempt sees the compare-and-swap fail and retries on the new head.
- **A stack land where a lower change is already landed** skips it.
- **Squash and stacks.** With `squash`, each change of a stack becomes one commit, in order.
- **Revert of a revert** is allowed and restores the original content.
- **A check named in policy that no runner reports** leaves changes `pending` forever. `POST …/policy/evaluate` shows the missing check; the hub does not time checks out.
- **An approver named `user:<username>` who is later deleted** stays in the policy text; the requirement can then only be met by its other approvers. `PUT` validates usernames at write time only.
- **Self-approval through a second token.** The counting rule compares credentials, not users. A person who wants agent work reviewed by someone else names approvers other than the agent's owner in the rule.
- **Landing onto a carry-patch patch branch** follows the extra preconditions of §10 (for example, a workspace whose patch branches live on origin needs `push_patches_to_origin`). A failed precondition ends the attempt `blocked` with that reason.
- **Empty change after rebase** (`noop`) still moves the change to `landed`, with `landed.sha` equal to the unchanged target head.

---

## 9. Awareness

### 9.1 Purpose and scope

When many agents work on one repository, the expensive conflicts are the ones nobody saw coming: two agents rewrite the same file for an hour and one of them finds out at landing. Awareness makes in-flight work visible early. Before an agent starts, it can ask who else is touching the paths it is about to edit. While it works, it is told when another change starts to overlap its own.

Awareness is advisory. It never refuses a push, a commit or a land. Agents and people decide what to do with what they see: wait, coordinate through the other change, stack on top of it, or carry on and resolve the conflict later.

In scope: the activity query, overlaps between changes, claims, and the events that announce overlaps. Out of scope: a message board or chat between agents; locking of paths; line-level merge prediction.

### 9.2 User stories

**AWR-US-01** As an Agent, I want to ask "who is working on `api/**` right now?" before I start, and get the open changes, their intent and their sessions.

**AWR-US-02** As an Agent, I want to announce what I am about to work on before I have any code, so that others see it.
- A claim expires by itself if I crash.

**AWR-US-03** As an Agent, I want to be told when another change starts touching files my open change touches.

**AWR-US-04** As a Developer, I want to see, for one change, every other open change that touches the same files, to decide the order in which they should land.

**AWR-US-05** As an Agent, I want to know what landed in my paths since I forked, so that I can rebase before I go further.

### 9.3 Concepts and data model

**Overlap.** Two changes overlap when they have the same target branch and at least one touched file in common. Overlap is computed on file paths (both paths of a rename), not on lines. Changes in one stack do not overlap each other.

**Overlap entry.**

| Field | Type | Meaning |
|---|---|---|
| `change` | object | Summary of the other change: `number`, `ref`, `title`, `intent` (first 500 characters), `state`, `author`, `session_id`, `version`, `updated_at`, `mergeable`, `landing.status` |
| `paths` | string[] | The files in common, at most 100; `paths_truncated` |
| `count` | int | Number of files in common |

**Claim.** An advisory, expiring statement of intent on paths.

| Field | Type | Meaning |
|---|---|---|
| `id` | string | UUID |
| `workspace_slug` | string | The workspace whose paths are claimed. Claims made through a fork's slug are stored on the parent |
| `paths` | string[] | 1–50 globs |
| `target_branch` | string | Default: the default branch |
| `intent` | string | 1–2,000 characters |
| `by` | object | `{actor_type, actor_id, username}` |
| `session_id` | string or null | |
| `change` | int or null | Set when a change of the same session and target is opened while the claim is live |
| `expires_at` | timestamp | Default 1 h after creation, at most 24 h |
| `state` | enum | `active`, `released`, `expired`, `fulfilled` (its change landed or was abandoned) |
| `created_at` | timestamp | |

**Activity.** The answer to "what touches these paths": in-flight changes, live claims, and recently landed changes, each matched against a list of path globs.

### 9.4 Functional requirements

- **AWR-FR-001** All awareness reads need `changes:read` and read access to the workspace. Creating, renewing and releasing claims needs `changes:write`. Through a fork's slug, awareness routes act on the fork's parent, so an agent holding only a fork token can use them; they then need the access the fork-bound token has by §3.
- **AWR-FR-002 (activity).** `GET …/activity` takes one or more `paths` globs and an optional `target` branch and returns:
  - `changes`: `draft` and `open` changes with a touched file matching any glob, as overlap entries;
  - `claims`: `active` claims with a glob that can match the same files as a requested glob. Two globs are treated as matching when either one matches the other's literal prefix; this errs on the side of reporting;
  - `landed`: changes landed on the target after `since` (a commit id, typically the caller's fork point or base, or a timestamp; default: 24 h ago) that touched a matching file, newest first, at most 100.
- **AWR-FR-003** With `exclude_session=<id>` or `exclude_change=<number>`, the caller's own work is left out of the activity answer.
- **AWR-FR-004 (overlaps of a change).** `GET …/changes/:number/overlaps` returns the overlap entries of every other `draft` or `open` change that overlaps this one, ordered by `count` descending.
- **AWR-FR-005 (overlap events).** When a new version makes two changes overlap that did not overlap before, the hub emits one `hub.change.overlap` for each of the two changes, naming the other. The pair is announced once; further versions that keep them overlapping emit nothing. If they stop overlapping and later overlap again, it is announced again.
- **AWR-FR-006** Overlap computation is asynchronous and never delays the push or commit that caused it.
- **AWR-FR-007 (claims).** Creating a claim returns the claim together with the current activity for its paths, so one call both announces and checks. Claims never conflict with each other: two sessions may claim the same paths.
- **AWR-FR-008** A claim can be renewed (`expires_at` moved forward, at most 24 h from now) and released by its creator, the workspace owner or the admin token. An expired or released claim is kept for 7 days for audit and then removed.
- **AWR-FR-009** When a change is opened with a `session_id` that has `active` claims on the same target branch, those claims get `change` set. When that change lands or is abandoned, the claims become `fulfilled`.
- **AWR-FR-010** Claims have no effect on pushes, commits, policy or landing.
- **AWR-FR-011** Limits: at most `[awareness].max_claims_per_workspace` (default 1,000) active claims per workspace and 50 per session; beyond that, `429`.

### 9.5 REST API

All routes are under `/workspaces/:slug`. The conventions of §3 apply.

#### `GET /workspaces/:slug/activity`
- **Purpose:** what touches these paths (AWR-FR-002).
- **Query:** `paths` (required; comma list or repeated), `target`, `since`, `exclude_session`, `exclude_change`.
- **Response:** `200 {"target_branch", "target_sha", "changes": [<overlap entry>…], "claims": [<claim>…], "landed": [{"number","ref","title","landed_sha","landed_at","session_id","paths"}…]}`.
- **Errors:** `400` when `paths` is missing or holds more than 50 globs.

#### `GET /workspaces/:slug/changes/:number/overlaps`
- **Purpose:** other in-flight changes touching the same files (AWR-FR-004).
- **Response:** `200` list of overlap entries.

#### `POST /workspaces/:slug/claims`
- **Purpose:** announce intended work (AWR-FR-007).
- **Body:** `{"paths": string[], "intent": string, "target_branch"?: string, "ttl"?: duration, "session_id"?: string}`.
- **Response:** `201 {"claim": <claim>, "activity": <activity answer for the claim's paths, excluding the claim itself>}`.
- **Errors:** `400` validation; `429` limits.
- **Side effects:** `hub.claim.create`.

#### `GET /workspaces/:slug/claims`
- **Purpose:** list claims.
- **Query:** `state` (default `active`), `session_id`, `path` (glob), `limit`, `cursor`. Newest first.
- **Response:** `200` list of claims.

#### `GET /workspaces/:slug/claims/:id`
- **Purpose:** read one claim. **Errors:** `404`, `error_type: claim_not_found`.

#### `PATCH /workspaces/:slug/claims/:id`
- **Purpose:** renew a claim or edit its intent.
- **Body:** `{"ttl"?: duration, "intent"?: string}`.
- **Response:** `200` claim. **Errors:** `409`, `error_type: claim_not_active`.

#### `DELETE /workspaces/:slug/claims/:id`
- **Purpose:** release a claim. Releasing a claim that is not active is a no-op.
- **Response:** `204`. **Side effects:** `hub.claim.release`.

### 9.6 CLI

#### `parley activity <slug>`
- **Flags:** `--paths <list>` (required), `--target <branch>`, `--since <sha|time>`, `--exclude-session <id>`, `--exclude-change <number>`, `--fail-on-overlap` (exit 3 when `changes` or `claims` is not empty).
- **Calls:** `GET …/activity`.

#### `parley change overlaps <slug> <number>`
- **Flags:** `--fail-on-overlap`. **Calls:** `GET …/changes/:number/overlaps`.

#### `parley claim create <slug>`
- **Flags:** `--paths <list>` (required), `--intent <text>` (required), `--target <branch>`, `--ttl <duration>`, `--session <id>`. **Calls:** `POST …/claims`.

#### `parley claim list <slug>`
- **Flags:** `--state`, `--session`, `--path`, `--limit`, `--cursor`. **Calls:** `GET …/claims`.

#### `parley claim renew <slug> <id>`
- **Flags:** `--ttl <duration>` (default `1h`). **Calls:** `PATCH …/claims/:id`.

#### `parley claim release <slug> <id>`
- **Calls:** `DELETE …/claims/:id`.

### 9.7 Background behaviour

- **Overlap index.** The hub keeps, per workspace and target branch, which `draft` and `open` changes touch which files. It is updated after each new version, retarget, land, abandon and restore.
- **Claim expiry** runs every minute: `active` claims past `expires_at` become `expired` and emit `hub.claim.expire`.
- **Claim cleanup** (AWR-FR-008) runs hourly.

### 9.8 Events

| Event type | When | `resource_type` / `resource_id` | Metadata |
|---|---|---|---|
| `hub.change.overlap` | Two changes started to overlap (AWR-FR-005); one event per change | `change` / `<number>` | `with` (the other change's number and ref), `with_session_id`, `paths` (at most 100), `count` |
| `hub.claim.create` | A claim was created | `claim` / `<id>` | `paths`, `intent`, `target_branch`, `expires_at` |
| `hub.claim.release` | A claim was released | `claim` / `<id>` | `paths` |
| `hub.claim.expire` | A claim expired | `claim` / `<id>` | `paths` |

An agent that wants to hear about overlaps subscribes to `hub.change.overlap` filtered by its `session_id` (§14).

### 9.9 Configuration

| Key | Type | Default | Effect |
|---|---|---|---|
| `[awareness].default_claim_ttl` | duration | `1h` | TTL of a claim without `ttl` |
| `[awareness].max_claim_ttl` | duration | `24h` | Longest TTL |
| `[awareness].max_claims_per_workspace` | int | `1000` | Active claims per workspace |

### 9.10 Edge cases and error handling

- **File-level only.** Two changes that edit different functions of one file overlap. They may still both land cleanly. `mergeable` (§7) and the trial rebase of `GET …/scm/compare` (§6) answer whether they actually conflict.
- **Very large changes.** A change touching more than 2,000 files is indexed in full; only the lists returned to clients are capped.
- **Drafts** are included in activity and overlaps, so that early work is visible.
- **A claim on paths that do not exist yet** is valid. Agents use it for new directories.
- **Stale claims.** A crashed agent's claim disappears at `expires_at`. Long tasks renew.
- **Visibility.** Activity shows changes whose source is a fork the caller cannot read. This is intended: the change itself lives in the target workspace, which the caller can read.

---

## 10. Carry-patch workspaces

### 10.1 Purpose and scope

A **carry-patch workspace** is a long-lived, ordered set of patches kept on top of a moving upstream. A **stack** (§7) is a short-lived, ordered set of changes that lands and disappears. Both replay ordered work onto a new base, both report conflicts in the same **conflict report** format (§8.3), and both rest on the same restack idea. They stay separate features: a stack ends when its changes land; a carry-patch workspace is rebuilt again and again for as long as the team carries the patches.

A team uses a carry-patch workspace when it maintains its own variant of a repository it does not control. The workspace tracks two remotes: **origin** (the team's copy of the repository) and **upstream** (the repository it follows). Each patch is a **patch branch**. The hub keeps an **integration branch** (default `deploy`) equal to "upstream base plus every applicable patch, in position order". A **rebuild** recomputes that branch. The hub also detects patches that upstream has merged (including squash merges), replays **remembered conflict resolutions** (git rerere), predicts conflicts (**preview**), and can **roll back** the integration branch.

In scope: the carry-patch fields of workspace creation and the setup after the first clone; the patch list; rebuilds, preview and rollback; the **durable job model** that rebuild jobs run on; remembered resolutions; the patch-status summary; the carry-patch part of `POST /workspaces/:slug/sync`; push control and the triggers that follow an update of a patch branch; the `parley patch`, `parley rebuild`, `parley rerere` and `parley workspace patch-status` commands.

Out of scope, and referenced here: workspace creation, sync preconditions, archive and reclone (§4); the git server and the refs clients may write (§5); changes and landing (§7, §8); secrets (§11); the event log (§14).

### 10.2 User stories

**CP-US-01: Create a carry-patch workspace.** As a developer, I want to create a workspace with `workspace_mode: carry_patch`, an `upstream_url` and an optional `integration_branch`, so that the hub tracks both my copy and the upstream.

**CP-US-02: Register patches.** As a developer or an agent, I want to register patch branches in order, one at a time or as an atomic batch, so that rebuilds apply them in that order.

**CP-US-03: Maintain the patch list.** As a developer, I want to reorder, disable, re-enable, mark as merged, describe, remove and restore patches.

**CP-US-04: Rebuild.** As a developer or an agent, I want to queue a rebuild, optionally overriding strategy and fail mode, and follow its progress.
- At most one rebuild per workspace is queued, running or waiting to retry.
- On a conflict in `fail_fast` mode the integration branch is unchanged and the job is not retried.

**CP-US-05: Understand a conflict.** As an agent, I want a rebuild conflict in the same structured format as a landing conflict, so that one piece of code handles both.

**CP-US-06: Preview.** As a developer, I want a read-only prediction of which patches would conflict with the current upstream base.

**CP-US-07: Roll back.** As an operator, I want to move the integration branch back to the head it had before a given completed rebuild.

**CP-US-08: Sync.** As a developer or a scheduler, I want one sync call to fetch upstream, refresh patch branches from origin when origin is the authority, detect merged patches and queue a rebuild.

**CP-US-09: Push control.** As an agent pushing to the hub, I want a push to a patch branch to be accepted, refused with a pointer to origin, or forwarded to origin, according to the workspace settings, and I want an accepted push to trigger a rebuild.

**CP-US-10: Review patch work.** As a team lead, I want patch branches to be protected so that every update of a patch arrives as a change, passes policy and lands, and I want the landing to trigger a rebuild like a push does.

**CP-US-11: Recover a replaced tip.** As a developer, I want the tip of a patch branch that the hub replaced to stay fetchable, and I want to be able to force a patch branch to origin's tip.

**CP-US-12: Manage remembered resolutions.** As a developer, I want to list the recorded conflict resolutions and forget a wrong one.

**CP-US-13: See the state at a glance.** As an org member with read access, I want one call that summarises the workspace, the latest rebuild, every patch and its latest result.

### 10.3 Concepts and data model

#### 10.3.1 Workspace fields

A carry-patch workspace is a workspace (§4) with these extra fields.

| Field | Type | Meaning |
|---|---|---|
| `workspace_mode` | `standard` \| `carry_patch` | Default `standard`. Immutable after creation. |
| `upstream_url` | string, nullable | Required for `carry_patch`, refused for `standard`. Same validation as `git_url` (§4). Immutable. |
| `integration_branch` | string, nullable | `carry_patch` only. Default `deploy`. Valid branch name. Immutable. |
| `upstream_head_sha` | string, nullable | Upstream base recorded by the last sync that saw upstream move. |
| `last_sync_at` | timestamp, nullable | Time of the last completed carry-patch sync. |

**Upstream base.** The commit every rebuild starts from: the head of the upstream's default branch as of the last upstream fetch. If the upstream's default branch is unknown, the hub uses the upstream branch with the same name as the workspace's `branch`. If neither resolves, there is no upstream base and operations that need one fail (CP-FR-045, CP-FR-063).

**Task forks.** A task fork (§4) of a carry-patch workspace is a plain fork. It has `workspace_mode: standard`, no patch list, no integration branch of its own and no rebuilds. The parent's patch branches and integration branch appear in the fork as read-only tracking branches. An agent in a fork contributes to a patch by pushing a new branch and opening a change that targets the patch branch in the parent (CP-FR-085).

#### 10.3.2 Refs the hub maintains

The hub's copy of the repository is bare. These refs are part of the contract because clients can fetch them.

| Ref | Meaning | Written by |
|---|---|---|
| `refs/heads/<integration_branch>` | The integration branch. Hub-owned: no client may create, update or delete it by any means (§5). | Rebuild, rollback |
| `refs/heads/<patch branch>` | The hub's copy of each patch branch. | Pushes, API commits, landing, sync refresh, reset to origin, branch resolution |
| `refs/remotes/upstream/*` | Upstream branches as of the last upstream fetch. Tags are not fetched. | Sync, rebuild |
| `refs/remotes/origin/*` | Origin branches as of the last origin fetch. | Sync, reset to origin, branch resolution |
| `refs/hub/replaced/<branch>` | Backup of a patch tip the hub discarded when it replaced the branch. One per branch; a later replace overwrites it. Fetchable, never pushable (§5). | Sync refresh, reset to origin |

#### 10.3.3 Patch

| Field | Type | Meaning |
|---|---|---|
| `id` | UUID | Patch id. |
| `workspace_slug` | string | Owning workspace. |
| `branch_name` | string | The patch branch. Unique among the non-deleted patches of the workspace. Immutable. |
| `position` | integer, nullable | Order of application. Non-deleted patches hold 1..N without gaps. `null` for a deleted patch. |
| `status` | enum | `active`, `conflict`, `disabled`, `merged_upstream`, `deleted`. |
| `conflict_files` | string[] | Paths that conflicted in the last rebuild that tried this patch. Always present; `[]` unless `status` is `conflict`. A convenience copy of the paths in the conflict report (10.3.6). |
| `upstream_pr_url` | string, nullable | Free text. Used by merge detection (CP-FR-072). |
| `description` | string, nullable | Free text. |
| `added_at`, `updated_at` | timestamp | |
| `deleted_at` | timestamp, nullable | Set by a soft delete, cleared by a restore. |
| `origin_sync_state` | enum, nullable | `in_sync`, `diverged`, `missing_on_origin`. Set only when `patch_branch_source` is `origin`. |
| `origin_sha` | string, nullable | Origin's tip of the branch at the last comparison. `null` when the branch is missing on origin. |
| `origin_synced_at` | timestamp, nullable | Time of that comparison. |

Every patch route returns this object with every field present (`null` when unset). `GET …/patches/:id` and reset-to-origin add `replaced_sha` when a backup ref exists for the branch.

**Patch status state machine**

| From | To | Trigger |
|---|---|---|
| (new) | `active` | Add. |
| any non-deleted | `active`, `disabled`, `merged_upstream`, `conflict` | `PATCH …/patches/:id` with `status`. |
| `active`, `conflict` | `conflict` | A rebuild could not apply the patch (either fail mode). `conflict_files` is set. |
| `conflict` | `active` | A rebuild applied the patch cleanly and published. `conflict_files` is cleared. |
| `active`, `conflict` | `merged_upstream` | Merge detection during sync. Sync never reverts it. |
| `merged_upstream` | `deleted` | A rebuild that published. The patch is no longer needed. |
| any non-deleted | `deleted` | `DELETE …/patches/:id`. |
| `deleted` | `active` | Restore. The patch is appended at the end of the list. |
| `deleted` for 7 days | (removed) | Purge by the maintenance loop (CP-FR-097). |

`conflict_files` is also cleared whenever the status becomes anything other than `conflict`.

**Origin sync state.** `in_sync`: the hub's tip equals origin's tip. `diverged`: the tips differ, the hub's tip is not an ancestor of origin's, and the policy is `report`. `missing_on_origin`: origin has no such branch.

#### 10.3.4 Durable jobs

Rebuilds run as **durable jobs**. A job is recorded before the request that created it returns, survives a restart of the hub, and is retried on transient failure. Rebuild is the only job type in this version; the model below is written so that it can carry other types later.

| Status | Meaning | Final |
|---|---|---|
| `queued` | Waiting for a worker. | no |
| `running` | A worker is executing it. | no |
| `failed` | The last attempt failed with a retryable error. The job is **waiting to retry**. | no |
| `completed` | Succeeded. The result is stored. | yes |
| `dead_letter` | Failed for good: a permanent error, or retries exhausted. Can be requeued. | yes, until requeued |
| `cancelled` | Cancelled while queued. | yes |

| From | To | Trigger |
|---|---|---|
| (new) | `queued` | Enqueue. |
| `queued` | `running` | A worker claims the job. One worker at most claims a given job. |
| `queued` | `cancelled` | Cancel. |
| `running` | `completed` | The attempt succeeded. |
| `running` | `failed` | Retryable error with retries left. `retry_count` goes up by one and `next_attempt_at` is set. |
| `running` | `dead_letter` | Permanent error, or a retryable error with no retries left. |
| `failed` | `queued` | `next_attempt_at` is reached. |
| `running` | `queued` | The hub starts and finds the job still marked running (crash recovery). `retry_count` is unchanged. |
| `dead_letter` | `queued` | Requeue. `retry_count` returns to 0. |

Rules:

- **Retry policy.** The delay after the n-th retryable failure is `min(2 s × 2ⁿ, 2 h)`: 4 s, 8 s, 16 s and so on. A job is retried at most 20 times; the 21st retryable failure dead-letters it.
- **`failed` is never final.** Nothing in the hub or the CLI treats `failed` as an end state. A client that waits for a job waits through `failed`.
- **Dedup.** At most one job per workspace may be `queued`, `running` or `failed`. An enqueue that would create a second one is refused (10.4, CP-FR-039). The single exception is the follow-up job a running rebuild may enqueue for itself (CP-FR-051); it starts only after the running job ends.
- **One at a time.** At most one rebuild runs per workspace. Rebuilds of different workspaces run in parallel, up to 4 at once across the hub.
- **Crash recovery.** A job that was running when the hub stopped runs again from the start after the restart. This is safe for rebuilds because a rebuild changes nothing visible until it publishes (CP-FR-048).
- **Retention.** Job records are kept for as long as the workspace exists and are removed with it.

#### 10.3.5 Rebuild job record

Every rebuild route that returns a job returns this object.

| Field | Type | Meaning |
|---|---|---|
| `id` | UUID | Job id. |
| `workspace_slug` | string | |
| `status` | enum | See 10.3.4. |
| `strategy` | `rebase` \| `merge` | Captured at enqueue. |
| `fail_mode` | `fail_fast` \| `continue` | Captured at enqueue. |
| `integration_branch` | string | Captured at enqueue. |
| `submitted_by` | string | A user id, or `system:push`, `system:land`, `system:followup`. |
| `retry_count` | integer | Retryable failures so far. |
| `next_attempt_at` | timestamp, nullable | Set only while `failed`. |
| `error` | string, nullable | Text of the last failure. Cleared when the job completes or is requeued. |
| `conflict` | conflict report, nullable | Set when the last failure was a patch conflict (10.3.6). |
| `created_at`, `updated_at` | timestamp | |
| `completed_at` | timestamp, nullable | Set when the job reaches `completed`, `dead_letter` or `cancelled`. `null` otherwise, including `failed`. |
| `upstream_head_sha` | string, nullable | Upstream base the run started from. |
| `integration_head_sha` | string, nullable | Integration head the run published. Set only when `completed`. |
| `previous_integration_head_sha` | string, nullable | Integration head replaced by the publish. `null` on the first rebuild. |
| `patches_applied`, `patches_skipped`, `patches_conflicted`, `patches_removed` | integer | Counts over `patch_results`. `patches_removed` counts merged patches the run soft-deleted. Zero until `completed`. |
| `integration_branch_pushed` | boolean | True when the run pushed the integration branch to origin. |
| `patch_results` | patch result[] | In position order. While `running`, the results so far. `[]` before the first patch finishes. |

**Patch result**

| Field | Type | Meaning |
|---|---|---|
| `patch_id`, `branch_name`, `position` | | The patch as listed when the run started. |
| `status` | `success` \| `conflict` \| `skipped` | |
| `skipped_reason` | enum, nullable | `merged_upstream`, `disabled`, `branch_not_found`, `nothing_to_apply`. |
| `source_sha` | string, nullable | Tip of the patch branch the run used. |
| `new_head_sha` | string, nullable | Integration head under construction after this patch. `null` unless `success`. |
| `conflict_files` | string[] | Paths in conflict. `[]` unless `conflict`. |
| `conflict` | conflict report, nullable | Set when `status` is `conflict`. |

#### 10.3.6 Conflict report

A rebuild conflict is reported in the shape landing uses (§8.3), plus the patch id:

```json
{
  "patch_id": "5c0e…",
  "target_sha": "<commit the patch was applied onto>",
  "head_sha": "<tip of the patch branch used>",
  "files": [{"path": "src/net/dial.go", "kind": "content"}]
}
```

`target_sha` is the integration head under construction at the moment the patch was applied: the upstream base for the first patch, otherwise the result of the patches before it. `head_sha` is the patch's `source_sha`. `files[].kind` takes the values defined in §8.3. The report lists only the paths still in conflict after remembered resolutions were applied.

#### 10.3.7 Remembered resolutions

The hub records how a conflict was resolved and replays the resolution when the same conflict appears again (git rerere). A rebuild that meets a conflict records it as unresolved; the resolution is learned when a later rebuild sees the same conflict resolved. The record is per workspace.

| Field | Type | Meaning |
|---|---|---|
| `id` | string | Identifier of the recorded conflict. |
| `path` | string, nullable | File the conflict was in, when known. |
| `resolved` | boolean | True when a resolution is recorded, false when only the conflict is. |
| `recorded_at` | timestamp, nullable | Time the resolution was recorded, or the conflict when unresolved. |

### 10.4 Functional requirements

**Workspace creation and setup**

- CP-FR-001: `POST /workspaces` (§4) accepts `workspace_mode`. A missing, null or empty value means `standard`. Any other value than `standard` or `carry_patch` is `400`.
- CP-FR-002: In carry-patch mode `upstream_url` is required and validated like `git_url`; `integration_branch` defaults to `deploy` and must be a valid branch name. In standard mode the presence of either field is `400`. The three fields cannot be changed by `PATCH /workspaces/:slug`.
- CP-FR-003: Upstream credentials are workspace secrets (§11), not part of the create request. The hub uses, in this order: `UPSTREAM_GIT_PAT`; `UPSTREAM_GIT_USERNAME` with `UPSTREAM_GIT_PASSWORD`; the origin credentials of the workspace; no credentials. A username without a password is skipped. Credentials are resolved on every use, so a rotated secret takes effect without a restart.
- CP-FR-004: After the first clone of a carry-patch workspace and before it becomes `ready`, the hub fetches upstream, turns on remembered resolutions, and creates the integration branch at the upstream base, or at the head of origin's default branch when the upstream fetch failed. A failure of any of these steps is logged and does not prevent `ready`. The next sync or rebuild repeats what is missing.
- CP-FR-005: The integration branch is created by the hub only. A workspace whose origin already has a branch with the integration branch's name is still created; the hub's integration branch does not follow origin's.

**Access, scopes and common preconditions**

- CP-FR-006: Every route in this section follows the global check order (§3). Read routes need read access to the workspace, all other routes need write access.
- CP-FR-007: PAT scopes are `patches:read` / `patches:write` for the patch list, `rebuilds:read` / `rebuilds:write` for rebuild jobs, preview and rollback, and `workspaces:read` / `workspaces:write` for remembered resolutions and the patch-status summary. 10.5 gives the scope of each route.
- CP-FR-008: After request validation, every route in this section checks, in order: the workspace is in carry-patch mode, else `400` `workspace_mode_mismatch`; for write routes and for preview, the workspace is `ready`, else `409` `workspace_not_ready`; the workspace is not in an exclusive maintenance operation, else `409` `workspace_busy`. Read routes other than preview work in every workspace state.

**Patch add**

- CP-FR-009: The body is one patch object or an array of them (a batch). An empty array is `400`.
- CP-FR-010: `branch_name` is required, must be a valid git branch name (§5), and must not be the integration branch. `position`, when given, must be at least 1. `if_not_exists` is refused in a batch. A branch named twice in one batch is `400`. All of this is validated before anything else happens; in a batch the message is prefixed `patch[<i>]: `.
- CP-FR-011: **Branch resolution.** Unless `skip_branch_check` is true, the branch must exist. The hub looks, in order, for: (1) the branch in the hub's copy; (2) the branch on origin as of the last origin fetch, in which case the hub creates the branch at that commit; (3) when `patch_branch_source` is `origin`, the branch on origin now, by fetching that one branch, and creates it. If none is found the answer is `400` `branch_not_found`. A failed origin fetch is `502` `origin_fetch_failed`. A branch created by resolution stays even if the request then fails. The way the branch was found is reported as `branch_resolution` in the event: `local`, `origin_tracking`, `origin_fetch` or `skipped`.
- CP-FR-012: A branch that is already registered as a non-deleted patch is `409` `patch_exists`. In a single add with `if_not_exists: true` the answer is `200` with the existing patch instead, and nothing changes.
- CP-FR-013: A soft-deleted patch with the same branch does not count as existing. It is purged as part of the add, together with its backup ref, and a new patch is created.
- CP-FR-014: **Position.** An omitted `position`, or one greater than N+1, appends the patch. Otherwise the patch takes that position and the patches at and after it move down by one. In a batch, elements with an explicit position are placed first, in array order, each into the list as it then stands; the others are appended in array order.
- CP-FR-015: A batch is all or nothing. The answer lists the created patches in request order, each with its final position.
- CP-FR-016: A new patch is `active`. Each created patch emits `hub.patch.create`.

**Patch read**

- CP-FR-017: The list returns the non-deleted patches in position order. With `include_deleted=true` the deleted patches follow, newest `deleted_at` first.
- CP-FR-018: Get returns any patch of the workspace by id, deleted or not, and adds `replaced_sha` when `refs/hub/replaced/<branch>` exists.

**Patch update**

- CP-FR-019: `PATCH …/patches/:id` accepts `status`, `position`, `description` and `upstream_pr_url`. Fields that are absent or null are left alone. An empty string clears `description` or `upstream_pr_url`. `branch_name` and unknown fields are ignored. An empty object changes nothing and answers `200`.
- CP-FR-020: `status` must be `active`, `disabled`, `merged_upstream` or `conflict`. `deleted` cannot be set here; use delete. `position` must be between 1 and the number of non-deleted patches.
- CP-FR-021: Any update of a deleted patch is `409` `patch_deleted`.
- CP-FR-022: A position change removes the patch from the order, inserts it at the new position and renumbers the list in one step.
- CP-FR-023: A status other than `conflict` clears `conflict_files`. An update that changes anything emits `hub.patch.update`. A status or position change does not queue a rebuild; the caller submits one.

**Patch delete and restore**

- CP-FR-024: Delete is a soft delete. The patch gets `status: deleted`, `deleted_at` is set, its position becomes `null`, and the remaining patches are renumbered. The patch branch and the backup ref are kept. It emits `hub.patch.delete`. Deleting a patch that is already deleted answers `204` and changes nothing.
- CP-FR-025: Restore applies to a `deleted` patch only; otherwise `409` `patch_not_deleted`. If another non-deleted patch now uses the same branch, restore is `409` `patch_exists`. The patch becomes `active`, is appended at the end, and `deleted_at` and `conflict_files` are cleared. It works the same for patches removed by a caller and for patches a rebuild removed as merged upstream. It emits `hub.patch.restore`.

**Patch reorder**

- CP-FR-026: `POST …/patches/reorder` takes `patch_ids`, which must name every non-deleted patch of the workspace exactly once. A duplicate, an unknown id, a deleted patch or a missing patch is `400`, and the message names the offending id or the two counts. The patches are renumbered in list order in one step. It emits one `hub.patch.reorder`.

**Reset to origin**

- CP-FR-027: Reset to origin forces the hub's copy of a patch branch to origin's current tip. It is allowed for patches in `active`, `conflict` or `disabled`; other statuses are `409` `patch_status_invalid`. It works whatever `patch_branch_source` and `patch_divergence_policy` say.
- CP-FR-028: The hub fetches that one branch from origin. Origin has no such branch: `409` `missing_on_origin`. Any other fetch failure: `502` `origin_fetch_failed`.
- CP-FR-029: The outcome (`action`) is the first that applies: the hub has no such branch, so it is created at origin's tip (`created`); the tips are equal and nothing is written (`none`); the hub's tip is an ancestor of origin's and the branch moves forward (`fast_forwarded`); otherwise the hub's tip is saved to `refs/hub/replaced/<branch>` and the branch is moved (`replaced`).
- CP-FR-030: Every move is a compare-and-swap against the tip the hub compared. If the branch moved in the meantime the answer is `409` `ref_moved`; the caller retries. A backup already written stays.
- CP-FR-031: When `patch_branch_source` is `origin`, the patch's origin state becomes `in_sync` with the new `origin_sha`.
- CP-FR-032: If the branch moved, the patch is `active` or `conflict`, and `auto_rebuild_after_sync` is true, a rebuild is queued (CP-FR-039). The answer carries `action`, `rebuild_triggered` and, when a job was queued, `rebuild_job_id`.
- CP-FR-033: Success emits `hub.patch.reset`, and `hub.patch.replace` when the action is `replaced`.

**Rebuild submit**

- CP-FR-034: `POST …/rebuild` queues a rebuild. The body is optional. `strategy` must be `rebase` or `merge`; `fail_mode` must be `fail_fast` or `continue`. Omitted or empty values fall back to the workspace settings `rebuild_strategy` and `rebuild_fail_mode`.
- CP-FR-035: A workspace with no patch in `active` or `conflict` has nothing to rebuild: `400` `no_active_patches`. Automatic triggers do not apply this check (CP-FR-055).
- CP-FR-036: Strategy, fail mode and integration branch are captured when the job is queued. Later changes of the settings do not affect a job that already exists.
- CP-FR-037: If the workspace already has a rebuild that is `queued`, `running` or `failed`, the answer is `409` `concurrent_rebuild` and names the existing job id in the message.
- CP-FR-038: The answer is `202` with the job record. It emits `hub.rebuild.enqueue`.
- CP-FR-039: **Enqueue sources.** Every source uses the same enqueue and the same dedup.

  | Source | `submitted_by` | On duplicate |
  |---|---|---|
  | `POST …/rebuild` | caller's user id | `409` `concurrent_rebuild` |
  | Sync | caller's user id | Skipped; `rebuild_triggered: false` |
  | Reset to origin | caller's user id | Skipped; `rebuild_triggered: false` |
  | Update of a patch branch (push, API commit, ref move, server-side rebase) | `system:push` | Skipped silently |
  | Landing on a patch branch | `system:land` | Skipped silently |
  | Stale-input follow-up | `system:followup` | Skipped silently |

  A skipped trigger loses nothing: a queued or retrying job reads its inputs when it starts, and a running job checks for stale inputs when it ends (CP-FR-051).

**Rebuild execution**

- CP-FR-040: A rebuild first fetches upstream with the upstream credentials and resolves the upstream base. A failure here is retryable.
- CP-FR-041: It then lists the non-deleted patches in position order and records the current tip of each patch branch (`source_sha`). Only these recorded tips are used for the rest of the run, so a push during the run cannot produce a mixed result.
- CP-FR-042: The run builds the new integration head away from every ref a client can see. It starts at the upstream base and takes the patches in position order:
  - `disabled` and `merged_upstream` patches are `skipped` with that reason.
  - A patch whose branch does not exist is `skipped` with `branch_not_found`.
  - With strategy `rebase`, every commit of the patch branch that is not in the upstream base is replayed in order. Merge commits are left out. A commit whose change is already present, in upstream or in an earlier patch, is dropped. A patch with no commit left to replay is `skipped` with `nothing_to_apply`.
  - With strategy `merge`, the patch tip is merged with a merge commit, even when a fast-forward would be possible. The message is `Merge branch '<branch_name>'`.
  - A patch that applies is `success`, with `new_head_sha`.
- CP-FR-043: Commits the hub creates keep the original author and carry the hub's committer identity (10.9).
- CP-FR-044: **Conflicts.** When a patch conflicts, the hub first replays remembered resolutions. If no path is left in conflict, the patch counts as applied and the run goes on. Otherwise the patch is undone, its status becomes `conflict`, its `conflict_files` are set, and a conflict report is produced (10.3.6). Then:
  - `fail_fast`: the run stops. Nothing is published. The job goes to `dead_letter` with the conflict report in `conflict`. It is not retried, because a retry would meet the same conflict.
  - `continue`: the patch result is `conflict`, the run goes on with the next patch from the state before the conflicting patch, and the integration branch is published without it.
- CP-FR-045: A missing upstream base, a failed fetch, a git failure that is not a conflict, a timeout and a shutdown of the hub are retryable failures. They are never recorded as a skip, a conflict or a success.
- CP-FR-046: After every patch the results so far are stored, so that `GET …/rebuilds/:id` of a running job shows progress.
- CP-FR-047: A `conflict` patch is tried again by every rebuild.
- CP-FR-048: **Publish.** When all patches are processed, the hub moves the integration branch to the new head with one compare-and-swap against the head it read immediately before. Readers see either the old integration branch or the new one, never a partial one. This is the only moment a rebuild changes a ref that clients can read. A lost compare-and-swap is a retryable failure.
- CP-FR-049: In the same step as the publish, the hub soft-deletes every patch the run skipped as `merged_upstream`, returns every patch that applied cleanly and was `conflict` to `active` with empty `conflict_files`, and renumbers the remaining patches. This also holds for `continue` runs in which other patches conflicted.
- CP-FR-050: If `rebuild_push_integration_branch` is true, the hub then pushes the integration branch to origin with force-with-lease against the value it last saw there. Success sets `integration_branch_pushed`. A failure emits `hub.rebuild.push_failed` and does not fail the rebuild.
- CP-FR-051: **Stale-input check.** After a publish and after a `fail_fast` conflict, the hub compares its inputs with the present. A patch is stale when its branch tip differs from the recorded `source_sha`. Upstream is stale when the upstream base differs from the one the run used. Stale patches are ignored when `auto_rebuild_after_push` is false; stale upstream is ignored when `auto_rebuild_after_sync` is false. If anything stale remains, the hub queues one follow-up rebuild with the same strategy and fail mode and emits `hub.rebuild.followup`. This follow-up is what makes it safe to skip triggers while a rebuild runs.
- CP-FR-052: A completed rebuild emits `hub.rebuild.complete`. Every failed attempt emits `hub.rebuild.fail`, with `will_retry` telling whether the job waits to retry or is dead-lettered.
- CP-FR-053: A rebuild does not block other work. Pushes, API commits, landing, sync, preview and rollback all proceed while it runs. Archive, reclone and delete of the workspace wait for a running rebuild to end or answer `409` `workspace_busy` (§4).
- CP-FR-054: A job whose workspace was deleted or is no longer in a usable state is dead-lettered without retry.
- CP-FR-055: A rebuild with no applicable patch still publishes: the integration branch moves to the upstream base. This happens when an automatic trigger fires while every patch is disabled or merged.

**Rebuild list, get, cancel, requeue**

- CP-FR-056: The list returns the rebuild jobs of the workspace, newest `created_at` first, and accepts a `status` filter.
- CP-FR-057: An unknown job id, or the id of a job of another workspace, is `404` on every job route.
- CP-FR-058: Cancel moves a `queued` job to `cancelled` and emits `hub.rebuild.cancel`. Any other status, including a job a worker claimed a moment earlier, is `409` `job_not_cancellable`.
- CP-FR-059: Requeue moves a `dead_letter` job back to `queued`, with `retry_count` 0 and `error` and `conflict` cleared, and emits `hub.rebuild.requeue`. Any other status is `409` `job_not_requeueable`. Requeue is refused with `409` `concurrent_rebuild` while another rebuild of the workspace is `queued`, `running` or `failed`. A requeued job uses the strategy and fail mode it was created with and reads fresh inputs.

**Rollback**

- CP-FR-060: Rollback moves the integration branch to the `previous_integration_head_sha` of a completed rebuild. A job that is not `completed`, or that has no previous head (the first rebuild), is `409` `rollback_unavailable`. So is a previous head whose commit no longer exists in the repository.
- CP-FR-061: The move is a compare-and-swap against the current integration head. A miss is `409` `ref_moved`.
- CP-FR-062: Rollback changes the integration branch only. Patches, positions, deleted patches and origin are untouched. The integration branch is not pushed to origin. Rollback emits `hub.rebuild.rollback`. The next rebuild that publishes replaces the rolled-back head, so an operator who wants the rollback to hold first turns off `auto_rebuild_after_push` and `auto_rebuild_after_sync`, or disables the patch at fault.

**Preview**

- CP-FR-063: Preview predicts the outcome of a rebuild against the upstream base as of the last fetch. It does not fetch. Without an upstream base the answer is `409` `upstream_unknown`; the caller syncs first.
- CP-FR-064: For the non-deleted patches in position order, leaving out `disabled` and `merged_upstream`: a patch whose branch does not exist is `would_skip`; a patch that merges cleanly onto the predicted state so far is `would_succeed` and becomes part of that state; a patch that does not is `would_conflict`, with `conflict_files` and a conflict report, and is left out of the state the later patches are tested against.
- CP-FR-065: Preview is a prediction. It tests a merge of each patch tip whatever the rebuild strategy, and it does not apply remembered resolutions. A patch reported `would_conflict` may therefore apply in a real rebuild.
- CP-FR-066: Preview changes no ref, no patch and no job, and can run at any time, including during a rebuild.

**Sync (carry-patch part)**

`POST /workspaces/:slug/sync` is defined in §4. Its authentication, scope, access, `ready` and `sync_enabled` checks apply first. On a carry-patch workspace the rest of the call behaves as follows. `reset_to_upstream` has no meaning here and is `400`.

- CP-FR-067: The hub fetches upstream. When `patch_branch_source` is `origin` it also fetches all branches of origin, pruning branches origin no longer has. A credential failure or a fetch failure is `502` (`origin_fetch_failed` for origin, `upstream_error` for upstream) and nothing else changes.
- CP-FR-068: `upstream_advanced` is true when the upstream base differs from the stored `upstream_head_sha`; it is true on the first sync. `force_push_detected` is true when upstream advanced and the stored head is not an ancestor of the new base.
- CP-FR-069: **Origin refresh** (only when `patch_branch_source` is `origin`). For every patch in `active`, `conflict` or `disabled`, in position order, the first rule that applies:

  | Situation | `action` | `state` |
  |---|---|---|
  | Origin has no such branch | `none` | `missing_on_origin` |
  | The hub has no such branch | `created` | `in_sync` |
  | Tips are equal | `none` | `in_sync` |
  | The hub's tip is an ancestor of origin's | `fast_forwarded` | `in_sync` |
  | Otherwise, policy `report` | `none` | `diverged` |
  | Otherwise, policy `replace` | `replaced` | `in_sync` |

  A replace saves the hub's tip to `refs/hub/replaced/<branch>` first. Every move is a compare-and-swap. The refresh moves protected patch branches too; sync from origin is one of the two ways a protected branch moves (§8).
- CP-FR-070: Each outcome is stored on the patch (`origin_sync_state`, `origin_sha`, `origin_synced_at`). When `patch_branch_source` is `hub`, and for patches in `merged_upstream` or `deleted`, the three origin fields are `null`.
- CP-FR-071: If the refresh of a branch fails (for example a lost compare-and-swap), the sync stops there. Outcomes reached so far are stored and their events emitted, a rebuild is queued if a refreshed branch needs one, and the answer is `500` naming the branch. `upstream_head_sha` and `last_sync_at` are not written and merge detection does not run. The caller syncs again.
- CP-FR-072: **Merge detection** runs when upstream advanced or a patch branch moved. It examines patches in `active` and `conflict`. The setting `squash_merge_detection` selects the tests:

  | Test | Used by | A patch is merged when |
  |---|---|---|
  | Ancestry | `ancestry_only`, `both` | The patch tip is an ancestor of the upstream base. |
  | Content | `content_based`, `both` | The branch has at least one commit beyond the upstream base and every such commit has an equivalent change in upstream. |
  | PR number | `content_based`, `both` | `upstream_pr_url` contains `/pull/<n>` and an upstream commit added since the stored upstream head has a subject ending in `(#<n>)`. On the first sync the last 50 upstream commits are examined. |

  A merged patch becomes `merged_upstream`, with `hub.patch.update`. A patch whose branch is missing is not examined.
- CP-FR-073: The hub then writes `last_sync_at`, and `upstream_head_sha` when upstream advanced.
- CP-FR-074: If upstream advanced, or a branch of an `active` or `conflict` patch moved, or a patch was detected as merged, and `auto_rebuild_after_sync` is true, a rebuild is queued (CP-FR-039).
- CP-FR-075: When `patch_branch_source` is `origin`, the sync emits `hub.patch.sync` for each patch whose branch moved or whose origin state changed, and `hub.patch.replace` for each replaced branch.
- CP-FR-076: The answer is the workspace object plus the carry-patch fields listed in 10.5.

**Push control**

These rules apply to every way a client can write a ref: git push (§5) and the source control API (§6). Over git a refusal is per ref, in git's own format, and the other refs of the same push proceed. Over the API it is `403` with the `error_type` given.

- CP-FR-077: **Integration branch.** A create, update or delete of `refs/heads/<integration_branch>` on a carry-patch workspace is always refused (`integration_branch_protected`), whatever the settings, with `integration branch <name> is maintained by the hub; push a patch branch and rebuild instead`. The integration branch cannot be the target of a change.
- CP-FR-078: **Backup refs.** `refs/hub/replaced/*` cannot be written by clients (§5).
- CP-FR-079: The rules below apply to the branch of a non-deleted patch.
- CP-FR-080: `patch_branch_source: hub`: the update is accepted, subject to branch protection (CP-FR-084).
- CP-FR-081: `patch_branch_source: origin` and `push_patches_to_origin: false`: every create, update and delete is refused (`patch_branch_from_origin`) with `branch is synced from origin; push to <git_url> instead`. The URL is shown without credentials.
- CP-FR-082: `patch_branch_source: origin` and `push_patches_to_origin: true`: the hub forwards the update to origin before accepting it, as a non-forced push with a 120 s limit. If origin accepts, the update is accepted on the hub. If origin refuses or cannot be reached, the update is refused with `origin rejected push: <reason>` (credentials removed). A delete is refused with `branch is synced from origin; delete it on origin instead`.
- CP-FR-083: If the hub cannot decide (for example the patch list cannot be read), the update is refused with a retryable error. Push control never lets an update through unchecked.

**Protected patch branches and changes**

- CP-FR-084: A patch branch may be listed in the workspace's `protected_branches` (§8). A direct update of it is then refused by §5 and §8, before the rules above are consulted.
- CP-FR-085: A change (§7) may target a patch branch, from the same workspace or from a task fork. It follows the normal change life cycle: versions, policy, approvals, checks.
- CP-FR-086: Landing a change on a patch branch (§8) updates the patch branch and has the same consequences as an accepted push to it: the triggers of CP-FR-087 and CP-FR-088 run. When `patch_branch_source` is `origin`, landing obeys CP-FR-081 and CP-FR-082: it is refused without forwarding, and with forwarding the new head is forwarded to origin before the target moves, and a refusal by origin fails the landing.

**Triggers after an update of a patch branch**

- CP-FR-087: After any accepted update of the branch of a non-deleted patch that is not a delete, and when `auto_rebuild_after_push` is true, the hub queues a rebuild (CP-FR-039). The trigger runs after the client has its answer and never changes the outcome of the push, commit or landing.
- CP-FR-088: **Mirror.** When `patch_branch_source` is `hub` and `push_patches_to_origin` is true, the hub then pushes each updated patch branch to origin, with force-with-lease against the value it last saw there and a 120 s limit. A failure emits `hub.patch.mirror_failed` for that branch and does not stop the others. The mirror does not depend on `auto_rebuild_after_push`.

**Remembered resolutions**

- CP-FR-089: The list returns every recorded entry, resolved or not, newest `recorded_at` first.
- CP-FR-090: Forget takes one path parameter, which may contain slashes. If it equals the `id` of an entry, that entry is removed. Otherwise every entry whose `path` equals it is removed. If nothing matches the answer is `404`. An empty value, or one that starts with `-`, is `400`.
- CP-FR-091: Forgetting an entry affects later rebuilds only.

**Patch status**

- CP-FR-092: `GET …/patch-status` returns one summary of the workspace, the integration branch, the latest rebuild and every non-deleted patch (10.5). It reads stored state only and fetches nothing.

**Git safety**

- CP-FR-093: The hub contacts upstream and origin over `https` and `ssh` only. A single git operation of a rebuild, a sync or a preview is limited to 10 minutes; exceeding the limit is a retryable failure in a job and a `500` in a request.
- CP-FR-094: Credentials never appear in an error message, a job `error`, an event or a log line; URL user information is replaced by `***@`. Branch names and commit ids supplied by callers are never interpreted as options of a git command.

**Maintenance**

- CP-FR-095: At start, every job found `running` returns to `queued` before workers start (10.3.4), and whatever a rebuild interrupted by the stop left behind is discarded. Start-up never fails because of it.
- CP-FR-096: At shutdown the hub stops starting jobs and gives running jobs 30 s to finish. A job that does not finish is run again from the start at the next start.
- CP-FR-097: **Purge.** The hourly maintenance loop (§3) removes every patch that has been `deleted` for more than 7 days, together with `refs/hub/replaced/<branch>` unless a non-deleted patch uses the same branch. A restore that happens at the same moment wins. Failures are logged and retried on the next pass.

### 10.5 REST API

All paths are relative to `/api/v1`. Errors use the error envelope (§3). The errors below are common to every route and are not repeated: `401`; `403` (scope, or a read-only member on a write route); `404` `workspace not found`; `400` `workspace_mode_mismatch`; `409` `workspace_not_ready` (write routes and preview); `409` `workspace_busy`.

#### `POST /workspaces/:slug/patches`
- **Purpose:** register one patch or a batch.
- **Scope:** `patches:write`. Accepts `Idempotency-Key`.
- **Body:** an object, or an array of objects:

  | Field | Type | Notes |
  |---|---|---|
  | `branch_name` | string, required | |
  | `position` | integer ≥ 1 | Default: append. |
  | `upstream_pr_url` | string | |
  | `description` | string | |
  | `skip_branch_check` | boolean | Default false. |
  | `if_not_exists` | boolean | Default false. Single add only. |

- **Response:** `201` with the patch (single) or `{"items":[patch,…]}` in request order (batch). `200` with the existing patch when `if_not_exists` matched.
- **Errors:** `400` `invalid_request` (CP-FR-009, CP-FR-010); `400` `branch_not_found`; `409` `patch_exists`; `502` `origin_fetch_failed`.

#### `GET /workspaces/:slug/patches`
- **Scope:** `patches:read`.
- **Query:** `include_deleted` (default false), `limit`, `cursor`.
- **Response:** `200` `{"items":[patch,…],"next_cursor":…}` in position order (CP-FR-017).

#### `GET /workspaces/:slug/patches/:id`
- **Scope:** `patches:read`.
- **Response:** `200` with the patch, deleted or not, plus `replaced_sha` when a backup ref exists.
- **Errors:** `404` `patch not found`.

#### `PATCH /workspaces/:slug/patches/:id`
- **Scope:** `patches:write`.
- **Body:** `status`, `position`, `description`, `upstream_pr_url`, all optional (CP-FR-019 to CP-FR-023).
- **Response:** `200` with the updated patch.
- **Errors:** `404` `patch not found`; `400` `invalid_request` (bad status, position out of range); `409` `patch_deleted`.

#### `DELETE /workspaces/:slug/patches/:id`
- **Scope:** `patches:write`.
- **Response:** `204`. Also `204` for a patch that is already deleted.
- **Errors:** `404` `patch not found`.

#### `POST /workspaces/:slug/patches/:id/restore`
- **Scope:** `patches:write`. No body.
- **Response:** `200` with the restored patch (`status: active`, last position, `deleted_at: null`).
- **Errors:** `404` `patch not found`; `409` `patch_not_deleted`; `409` `patch_exists`.

#### `POST /workspaces/:slug/patches/:id/reset-to-origin`
- **Scope:** `patches:write`. No body.
- **Behaviour:** CP-FR-027 to CP-FR-033.
- **Response:** `200` with the patch plus `action` (`none`, `created`, `fast_forwarded`, `replaced`), `replaced_sha` (when a backup ref exists), `rebuild_triggered` (boolean) and `rebuild_job_id` (when a job was queued).
- **Errors:** `404` `patch not found`; `409` `patch_status_invalid`; `409` `missing_on_origin`; `502` `origin_fetch_failed`; `409` `ref_moved`.

#### `POST /workspaces/:slug/patches/reorder`
- **Scope:** `patches:write`.
- **Body:** `{"patch_ids":["<id>",…]}`.
- **Response:** `200` `{"items":[patch,…],"next_cursor":null}` with all non-deleted patches in the new order.
- **Errors:** `400` `invalid_request` (CP-FR-026).

#### `GET /workspaces/:slug/patch-status`
- **Scope:** `workspaces:read`.
- **Response:** `200`:

  | Field | Meaning |
  |---|---|
  | `workspace_slug`, `workspace_mode`, `status` | From the workspace. |
  | `git_url`, `upstream_url`, `integration_branch` | From the workspace. |
  | `clone_error`, `sync_error` | String or `null`. |
  | `upstream_head_sha`, `last_sync_at` | String or `null`. |
  | `integration_head_sha` | Current head of the integration branch, or `null` when the branch does not exist. |
  | `last_rebuild` | `{id, status, created_at, completed_at}` of the newest rebuild job in any status, or `null`. |
  | `last_completed_rebuild` | The same for the newest `completed` rebuild, or `null`. |
  | `patches` | Non-deleted patches in position order: `id`, `branch_name`, `position`, `status`, `conflict_files`, `last_rebuild_result` (status of this patch in the newest job that has a result for it, or `null`), `origin_sync_state`, `origin_sha`, `origin_synced_at`. |
  | `summary` | `total_patches`, `active`, `conflict`, `disabled`, `merged_upstream`, `rerere_resolutions` (entries, resolved or not), `patches_diverged`, `patches_missing_on_origin`. All always present. |

#### `GET /workspaces/:slug/rerere`
- **Scope:** `workspaces:read`.
- **Query:** `limit`, `cursor`.
- **Response:** `200` `{"items":[{"id","path","resolved","recorded_at"},…],"next_cursor":…}`, newest first.

#### `DELETE /workspaces/:slug/rerere/*pathspec`
- **Scope:** `workspaces:write`.
- **Path:** `pathspec` is an entry id or a file path; it may contain slashes.
- **Response:** `204`.
- **Errors:** `400` `invalid_request` (empty, or starts with `-`); `404` `resolution not found`.

#### `POST /workspaces/:slug/rebuild`
- **Purpose:** queue a rebuild.
- **Scope:** `rebuilds:write`. Accepts `Idempotency-Key`.
- **Body (optional):** `strategy` (`rebase` \| `merge`), `fail_mode` (`fail_fast` \| `continue`).
- **Response:** `202` with the rebuild job record, `status: queued`.
- **Errors:** `400` `invalid_request`; `400` `no_active_patches`; `409` `concurrent_rebuild`.

#### `GET /workspaces/:slug/rebuilds`
- **Scope:** `rebuilds:read`.
- **Query:** `status` (one job status), `limit`, `cursor`.
- **Response:** `200` `{"items":[job,…],"next_cursor":…}`, newest first.

#### `GET /workspaces/:slug/rebuilds/:id`
- **Scope:** `rebuilds:read`.
- **Response:** `200` with the rebuild job record (10.3.5).
- **Errors:** `404` `rebuild job not found`.

#### `DELETE /workspaces/:slug/rebuilds/:id`
- **Purpose:** cancel a queued rebuild.
- **Scope:** `rebuilds:write`.
- **Response:** `200` with the job record, `status: cancelled`.
- **Errors:** `404` `rebuild job not found`; `409` `job_not_cancellable`.

#### `POST /workspaces/:slug/rebuilds/:id/requeue`
- **Scope:** `rebuilds:write`. No body.
- **Response:** `200` with the job record, `status: queued`.
- **Errors:** `404` `rebuild job not found`; `409` `job_not_requeueable`; `409` `concurrent_rebuild`.

#### `POST /workspaces/:slug/rebuilds/:id/rollback`
- **Scope:** `rebuilds:write`. No body.
- **Response:** `200` `{"rolled_back_to":"<sha>","previous_head_sha":"<sha>"}`.
- **Errors:** `404` `rebuild job not found`; `409` `rollback_unavailable`; `409` `ref_moved`.

#### `GET /workspaces/:slug/rebuild-preview`
- **Scope:** `rebuilds:read`.
- **Response:** `200`:

  `{"upstream_head_sha":"<base used>","patch_results":[{"patch_id","branch_name","position","status","conflict_files":[…],"conflict":<conflict report or null>}]}`

  `status` is `would_succeed`, `would_conflict` or `would_skip`.
- **Errors:** `409` `upstream_unknown`.

#### `POST /workspaces/:slug/sync`
- **Purpose:** the route is defined in §4. This entry lists what a carry-patch workspace adds (CP-FR-067 to CP-FR-076).
- **Response:** `200` with the workspace object plus:

  | Field | Type | Meaning |
  |---|---|---|
  | `upstream_advanced` | boolean | |
  | `force_push_detected` | boolean | |
  | `origin_fetched` | boolean | True only when `patch_branch_source` is `origin`. |
  | `patches_merged` | string[] | Branch names detected as merged in this sync. |
  | `patches_synced` | object[] | One entry per refreshed candidate: `branch_name`, `action`, `state`, `local_sha`, `origin_sha`, `replaced_sha` (nullable). `[]` when `patch_branch_source` is `hub`. |
  | `patches_diverged` | string[] | Branch names with state `diverged`. |
  | `rebuild_triggered` | boolean | |
  | `rebuild_job_id` | string, nullable | Set when a job was queued. |

- **Errors added:** `400` `invalid_request` (`reset_to_upstream`); `502` `upstream_error`; `502` `origin_fetch_failed`; `500` `internal` (refresh failure, CP-FR-071).

### 10.6 CLI

The global CLI conventions apply (§2): JSON on stdout, the error envelope on stderr, exit codes 0, 1, 2 and 3. Unless stated, a command prints the hub's response.

#### `parley patch add <workspace-slug>`
- **Flags:** `--branch` (required), `--position <int>`, `--upstream-pr <url>`, `--description <text>`, `--skip-branch-check`, `--if-not-exists`.
- **Calls:** `POST /workspaces/:slug/patches` with one object. A flag that was not given is not sent.

#### `parley patch list <workspace-slug>`
- **Flags:** `--include-deleted`.
- **Calls:** `GET /workspaces/:slug/patches`.

#### `parley patch show <workspace-slug> <patch-id>`
- **Calls:** `GET /workspaces/:slug/patches/:id`.

#### `parley patch update <workspace-slug> <patch-id>`
- **Flags:** `--status`, `--position <int>`, `--description`, `--upstream-pr`. Only the flags given are sent; an empty string is sent and clears the field. With no flag the command exits 2.
- **Calls:** `PATCH /workspaces/:slug/patches/:id`.

#### `parley patch remove <workspace-slug> <patch-id>`
- **Calls:** `DELETE /workspaces/:slug/patches/:id`.
- **Output:** `{"status":"removed","patch_id":"<id>"}`. The removal is a soft delete; `parley patch restore` brings the patch back within 7 days.

#### `parley patch restore <workspace-slug> <patch-id>`
- **Calls:** `POST /workspaces/:slug/patches/:id/restore`.

#### `parley patch reorder <workspace-slug> <patch-id>...`
- **Args:** every non-deleted patch id, in the new order.
- **Calls:** `POST /workspaces/:slug/patches/reorder`.

#### `parley patch reset-to-origin <workspace-slug> <patch-id>`
- **Flags:** `--wait`, `--timeout`, `--poll-interval`. With `--wait`, and when the response names a `rebuild_job_id`, the command waits for that job as `parley rebuild submit --wait` does.
- **Calls:** `POST /workspaces/:slug/patches/:id/reset-to-origin`.

#### `parley rebuild submit <workspace-slug>`
- **Flags:** `--strategy <rebase|merge>`, `--fail-mode <fail_fast|continue>`, `--wait`, `--timeout` (default 5m), `--poll-interval` (default 5s).
- **Calls:** `POST /workspaces/:slug/rebuild`; with `--wait`, then `GET /workspaces/:slug/rebuilds/:id` until the job is `completed`, `dead_letter` or `cancelled`. `failed` means the job waits to retry, so polling continues.
- **Output:** the job record; with `--wait`, the final job record.
- **Exit codes:** 0 when the job is queued (no `--wait`) or `completed`; 1 when the wait ends in `dead_letter` or `cancelled`, or times out.

#### `parley rebuild list <workspace-slug>`
- **Flags:** `--status <status>`.
- **Calls:** `GET /workspaces/:slug/rebuilds`.

#### `parley rebuild status <workspace-slug> <rebuild-id>`
- **Calls:** `GET /workspaces/:slug/rebuilds/:id`.

#### `parley rebuild preview <workspace-slug>`
- **Flags:** `--fail-on-conflict`: exit 3 when any patch is `would_conflict`.
- **Calls:** `GET /workspaces/:slug/rebuild-preview`.

#### `parley rebuild cancel <workspace-slug> <rebuild-id>`
- **Calls:** `DELETE /workspaces/:slug/rebuilds/:id`.

#### `parley rebuild requeue <workspace-slug> <rebuild-id>`
- **Flags:** `--wait`, `--timeout`, `--poll-interval`, as for `submit`.
- **Calls:** `POST /workspaces/:slug/rebuilds/:id/requeue`.

#### `parley rebuild rollback <workspace-slug> <rebuild-id>`
- **Calls:** `POST /workspaces/:slug/rebuilds/:id/rollback`.

#### `parley rerere list <workspace-slug>`
- **Calls:** `GET /workspaces/:slug/rerere`.

#### `parley rerere forget <workspace-slug> <id-or-path>`
- **Calls:** `DELETE /workspaces/:slug/rerere/<id-or-path>`. Slashes in a path are kept as separators.
- **Output:** `{"status":"forgotten","pathspec":"<arg>"}`.

#### `parley workspace patch-status <workspace-slug>`
- **Calls:** `GET /workspaces/:slug/patch-status`.

Two commands defined in §4 carry carry-patch options: `parley workspace create --workspace-mode carry_patch --upstream-url <url> [--integration-branch <name>]`, and `parley workspace sync <slug> [--wait] [--fail-on-diverged]`. With `--wait`, sync waits for the job named by `rebuild_job_id`, if any. With `--fail-on-diverged` it exits 3 when `patches_diverged` is not empty; a failed wait (exit 1) takes precedence.

### 10.7 Background behaviour

- **Workers.** Durable jobs are executed by the hub itself; there is no separate worker process. A queued job starts within a few seconds when a worker is free. Up to 4 jobs run at once across the hub, at most one per workspace.
- **Triggers.** After every accepted update of a patch branch: the rebuild trigger, then the mirror (CP-FR-087, CP-FR-088). After a publish or a `fail_fast` conflict: the stale-input check (CP-FR-051).
- **No schedule.** The hub does not sync a carry-patch workspace on its own. A scheduler or a forge webhook consumer calls `POST /workspaces/:slug/sync`.

### 10.8 Events

All events follow the envelope of §14. Rebuild events have `resource_type: rebuild` and the job id as `resource_id`. Patch events have `resource_type: patch` and the patch id as `resource_id`. The actor is the caller, or `system` for work the hub does on its own. A failure to record an event never changes the outcome of the operation.

| Event | When | Actor | Metadata |
|---|---|---|---|
| `hub.patch.create` | A patch was created (single add and each element of a batch) | caller | `branch_name`, `position`, `branch_resolution` |
| `hub.patch.update` | A patch's status, position, description or PR URL changed | caller, or `system` | `branch_name`, `changed` (field names), and for a status change `from`, `to`, `reason` (`api`, `rebuild_conflict`, `rebuild_clean`, `merged_upstream`) |
| `hub.patch.reorder` | The list was reordered. One event; `resource_id` is the first patch of the new order | caller | `patch_ids` (new order) |
| `hub.patch.delete` | A patch was soft-deleted | caller, or `system` when a rebuild removed a merged patch | `branch_name`, `reason` (`api`, `merged_upstream`) |
| `hub.patch.restore` | A patch was restored | caller | `branch_name`, `position` |
| `hub.patch.reset` | Reset to origin succeeded, whatever the action | caller | `branch_name`, `action`, `local_sha`, `origin_sha`, `replaced_sha` |
| `hub.patch.replace` | The hub replaced a diverged patch tip and wrote a backup ref | caller | `branch_name`, `replaced_sha`, `origin_sha`, `trigger` (`sync`, `reset_to_origin`) |
| `hub.patch.sync` | An origin refresh moved a patch branch or changed its origin state | caller | `branch_name`, `action`, `state`, `local_sha`, `origin_sha` |
| `hub.patch.mirror_failed` | The mirror of a patch branch to origin failed | `system` | `branch_name`, `error` |
| `hub.rebuild.enqueue` | A rebuild job was queued, from any source | caller, or `system` | `submitted_by`, `strategy`, `fail_mode`, `patch_count` |
| `hub.rebuild.complete` | A rebuild published | `system` | `upstream_head_sha`, `integration_head_sha`, `previous_integration_head_sha`, `patches_applied`, `patches_skipped`, `patches_conflicted`, `patches_removed` |
| `hub.rebuild.fail` | An attempt failed | `system` | `reason`, `will_retry`, `retry_count`, `conflict` (the conflict report, when the failure was a conflict) |
| `hub.rebuild.push_failed` | The push of the integration branch to origin failed | `system` | `integration_branch`, `reason` |
| `hub.rebuild.followup` | The stale-input check queued a follow-up. `resource_id` is the job that ended | `system` | `follow_up_job_id`, `stale_patches` (branch names), `upstream_stale` |
| `hub.rebuild.cancel` | A queued job was cancelled | caller | none |
| `hub.rebuild.requeue` | A dead-lettered job was requeued | caller | none |
| `hub.rebuild.rollback` | The integration branch was rolled back | caller | `integration_branch`, `rolled_back_to`, `previous_head_sha` |

Updates of a patch branch and of the integration branch also produce the general `hub.git.push` and change events of §5, §7 and §8. Events caused by a session carry its `session_id`; a rebuild queued by a push or a landing carries the session of that push or landing.

### 10.9 Configuration

**Workspace settings** (in the workspace's `settings`, §4). They are read each time they are needed; no restart is required. §4 validates the values when the settings are written.

| Setting | Values | Default | Effect |
|---|---|---|---|
| `patch_branch_source` | `hub`, `origin` | `hub` | Which side is the authority for patch branches: branch resolution, sync refresh, push control, mirror. |
| `patch_divergence_policy` | `replace`, `report` | `replace` | What sync does with a diverged patch branch when origin is the authority. |
| `push_patches_to_origin` | boolean | `false` | With `origin`: forward pushes to origin. With `hub`: mirror patch branches to origin. |
| `auto_rebuild_after_push` | boolean | `true` | Rebuild after an update of a patch branch; follow-ups for stale patches. |
| `auto_rebuild_after_sync` | boolean | `true` | Rebuild after sync and reset to origin; follow-ups for stale upstream. |
| `rebuild_strategy` | `rebase`, `merge` | `rebase` | Default strategy, captured when a job is queued. |
| `rebuild_fail_mode` | `fail_fast`, `continue` | `fail_fast` | Default fail mode, captured when a job is queued. |
| `rebuild_push_integration_branch` | boolean | `false` | Push the integration branch to origin after a publish. Read when the rebuild publishes. |
| `squash_merge_detection` | `ancestry_only`, `content_based`, `both` | `both` | Tests used by merge detection. |
| `protected_branches` | branch list (§8) | default branch | May include patch branches (CP-FR-084). |

**Secrets** (§11): `UPSTREAM_GIT_PAT`, `UPSTREAM_GIT_USERNAME`, `UPSTREAM_GIT_PASSWORD` for upstream; the origin credentials of the workspace for origin and as the fallback for upstream.

**Server.** The identity on commits the hub creates is `parleyd <parleyd@localhost>` unless the server process is started with `GIT_COMMITTER_NAME` and `GIT_COMMITTER_EMAIL`.

**Fixed values.** The retry policy, the job concurrency, the shutdown grace period, the git and push time limits, the 7-day retention of deleted patches and the 50-commit window of merge detection are not configurable in this version.

### 10.10 Edge cases and error handling

- **Rollback after patches were removed.** Rollback restores the integration branch only. Patches a later rebuild soft-deleted as merged stay deleted; restore them one by one if needed.
- **`fail_fast` conflict.** The job is `dead_letter` with a conflict report. The integration branch is unchanged. The patch is `conflict`. After the branch is fixed, a push queues a new rebuild (if `auto_rebuild_after_push` is true); the dead-lettered job may also be requeued.
- **Deleting a patch branch.** With `patch_branch_source: hub` a client may delete an unprotected patch branch. The patch stays registered and is skipped until the branch returns. A delete does not trigger a rebuild.
- **Push to the branch of a deleted patch.** It is an ordinary branch: push control and the rebuild trigger do not apply.
- **Upstream force-push.** Sync reports `force_push_detected` and proceeds. The next rebuild starts from the new base. Merge detection by PR number falls back to the commits reachable from the new base.
- **Upstream unreachable.** A rebuild fails with a retryable error and waits to retry, for up to 20 retries. Preview keeps working from the last fetched base.
- **Setup incomplete.** If the upstream fetch failed during creation, the integration branch sits at origin's default branch head until the first successful rebuild moves it to "upstream base plus patches".
- **Origin has an integration branch of its own.** The hub does not read it. With `rebuild_push_integration_branch` the hub pushes with force-with-lease; if someone else moved the branch on origin since the hub last saw it, the push fails with `hub.rebuild.push_failed` and the hub's integration branch is still correct.
- **Task fork of a carry-patch workspace.** Every route in this section answers `400` `workspace_mode_mismatch` on the fork. The fork's tracking copies of the patch branches and of the integration branch follow the parent.
- **Workspace archived, recloned or deleted.** Write routes answer `409` `workspace_not_ready` or `workspace_busy`. A queued job for a workspace that is archived or deleted is dead-lettered without retry. After a reclone, patches, jobs and settings remain; remembered resolutions and backup refs do not survive the reclone.
- **One hub process.** Dedup and "one rebuild per workspace" are guaranteed for one `parleyd` process on a data directory (§15). Running two processes on the same data directory is not supported.

---

## 11. Secrets and variables

### 11.1 Purpose and scope

The hub keeps named key/value pairs for three kinds of owner: a user, an organization and a workspace. **Secrets** are write-only. Their values never leave the hub through any API response, and they are encrypted at rest. The hub consumes them itself: as git and forge credentials, as the webhook secret, and as values injected into sandboxes. **Variables** follow the same rules and scopes, but their values are readable, and a workspace's effective variables can be read merged along a precedence chain.

In scope: scopes, validation, access rules, encryption and key rotation, resolution chains (including task forks), REST routes, events and the `parley secrets` / `parley vars` commands. Out of scope: how sandboxes use injected secrets (§13), how git credentials are used (§4, §10) and the forge webhook (§12). Variables never change hub behaviour; behaviour switches are workspace settings (§4).

### 11.2 User stories

- **SEC-US-01** As a Developer, I want to store a git PAT for my workspace (`GIT_PAT`) without it ever being readable again, so that the hub can clone, push and call the forge for me.
- **SEC-US-02** As an Org member, I want to store secrets and variables at the organization level, so that every workspace of the org shares them.
- **SEC-US-03** As an Agent with a workspace-bound token and `vars:read`, I want the effective variables of my workspace or fork, each with its origin, so that I can configure myself.
- **SEC-US-04** As an Org member working in a task fork, I want my fork to run with the parent's variables, while the parent's secrets stay out of my reach: not readable, not listable and not injected into my fork's sandboxes.
- **SEC-US-05** As an Operator, I want secrets encrypted with a key I supply and can rotate, so that a copy of the hub's storage does not disclose them.
- **SEC-US-06** As a user, I want every change to my secrets and variables recorded without values and visible to me in `/audit` and `/events`.

### 11.3 Concepts and data model

**Scopes**

| Scope | Route prefix | `scope_id` | Who may access |
|---|---|---|---|
| `user` | `/user` | the caller's user id | any credential with a user identity (API key, PAT). Admin tokens have no user and are refused |
| `org` | `/orgs/:slug` | the organization id | members of the org, admin tokens |
| `workspace` | `/workspaces/:slug` | the workspace slug | the workspace access rule, see SEC-FR-006 |

A task fork (§4) is a workspace and has its own `workspace` scope.

**Entry.** Secrets and variables are two separate collections with the same shape: an owner (`scope`, `scope_id`), a `key` (stored and returned with the casing given at create), a `value`, and `created_at` / `updated_at` (equal on create). Responses never carry the owner (the route states it) and never carry a secret's `value`. Resolved variable entries add `origin`: `workspace`, `parent`, `org` or `user`.

**Limits**

| Rule | Value |
|---|---|
| Key characters | ASCII letters, digits and `_`; the first character is not a digit |
| Key length | 1 to 255 |
| Key matching | case-insensitive for uniqueness, update, delete and resolution |
| Value size | at most 262,144 bytes (256 KiB), counted in bytes; the empty string is allowed |
| Entries per scope | at most 100 secrets and 100 variables per `(scope, scope_id)` |

**Encryption at rest.** Secret values are stored only under authenticated encryption with a key the operator supplies (§11.9). Each stored value records which key encrypted it. Variable values are not encrypted.

**Reserved secret names.** Any valid key may be created; the hub blocks no name. The hub itself reads these names, and only from the `workspace` scope of the workspace concerned. They are never taken from a user, an org or a parent workspace.

| Name | Used for |
|---|---|
| `GIT_PAT` | Origin credential (basic auth, username `x-token-auth`, password the PAT). Also the forge API token |
| `GIT_USERNAME` + `GIT_PASSWORD` | Origin credential when `GIT_PAT` is absent. The forge API gets the password as token. A username without a password counts as no credential |
| `UPSTREAM_GIT_PAT` | Upstream credential of a carry-patch workspace (§10) |
| `UPSTREAM_GIT_USERNAME` + `UPSTREAM_GIT_PASSWORD` | Upstream credential when `UPSTREAM_GIT_PAT` is absent |
| `FORGE_WEBHOOK_SECRET` | Secret that verifies deliveries on the forge webhook (§12). An empty value counts as not set |

Credential resolution happens on every use and is never cached, so a rotated secret applies to the next operation:

- origin: `GIT_PAT`, else `GIT_USERNAME` + `GIT_PASSWORD`, else none (public repository);
- upstream: `UPSTREAM_GIT_PAT`, else `UPSTREAM_GIT_USERNAME` + `UPSTREAM_GIT_PASSWORD`, else the origin resolution.

A failure to read or decrypt a secret fails the operation; it is never treated as "no credential".

A task fork has no origin and therefore no origin credential. Reserved names stored in a fork's scope are never read by the hub, and a fork never uses its parent's credentials: `sync`, `reclone` and push to origin are refused on forks (§4).

**Resolution chain.** The chain of a workspace W, highest priority first:

| Step | Scope | Included when |
|---|---|---|
| 1 | W's `workspace` scope | always |
| 2 | the parent's `workspace` scope | W is a task fork |
| 3 | the `org` scope of W's organization | W has an org and W's owner is currently a member of it |
| 4 | the `user` scope of W's owner | always |

The user is always the workspace owner, never the caller. For a fork, the owner is whoever created the fork, and the org is the parent's. The rule is the same for every caller, including admin tokens.

- **Variables** resolve by merge: for each key, compared case-insensitively, the entry of the highest-priority scope that has it wins entirely (its key casing, value, timestamps and `origin`).
- **Secrets** resolve by name for sandbox injection (§13): the first scope in the chain that has the name wins. Values are read when the sandbox is created and go only into the sandbox environment. For secrets, step 2 applies only when the fork's owner has write access to the parent workspace (the parent's owner). The test is on the fork's owner, never on the caller: an admin token acts as the fork owner here. In a fork owned by a read-only org member, step 2 is skipped for secrets, so the parent's secrets never reach that fork's sandboxes. Reserved names are skipped in step 2 for everyone, so no fork's sandbox receives the parent's origin, upstream or webhook credentials.

Resolution is use, not read access. A fork owner who is only an org member of the parent gets the parent's variables in the fork's resolved set, and nothing of the parent's secrets: no value, no injection, and no secret names (SEC-FR-006). No route returns a secret value to anyone.

### 11.4 Functional requirements

- **SEC-FR-001** Every route follows the global check order (§3): authenticate → PAT scope → scope access → request validation → state → act. PAT scopes are `secrets:read`, `secrets:write`, `vars:read`, `vars:write`; a read accepts either, a write only `write`. API keys and admin tokens are not scope-checked.
- **SEC-FR-002** A workspace-bound token may use only the `workspace` scope of a workspace it is valid on (its workspace and the forks it created, §3). On `/user/…` and `/orgs/…` routes it gets `403 token_bound`; on another workspace `404 workspace not found`.
- **SEC-FR-003** Secret values are returned by no route, event, log line or error message. Only credential resolution, webhook verification and sandbox injection read them.
- **SEC-FR-004** User scope: a credential without a user identity (admin token) gets `403 forbidden`, message `user-scoped endpoints require a user credential`.
- **SEC-FR-005** Org scope: an unknown org and a non-member both get `404 not_found` with the same body. Admin tokens bypass membership.
- **SEC-FR-006** Workspace scope uses the workspace access rule (§3):

  | Caller | Secrets (all operations) | Variables read (list, resolved) | Variables write |
  |---|---|---|---|
  | owner, admin token | allowed | allowed | allowed |
  | org member | `404 workspace not found` | allowed | `403 forbidden` |
  | anyone else, or unknown slug | `404 workspace not found` | `404` | `404` |

  A member's `404` on a secret route is identical to the answer for a nonexistent workspace. This holds on a parent workspace for the owner of one of its forks. The workspace's status (for example `archived`) does not matter; an exclusive maintenance operation does not block these routes.
- **SEC-FR-007** Create takes `{"entries":[{"key","value"}, …]}` and is all-or-nothing. A missing `value` is the empty string. Checks, in order: `entries` non-empty; each key valid; each value within the size limit; no two keys of the request equal ignoring case (all `400 invalid_request`, the message names the key); the scope's count plus the new entries within the limit (`400 invalid_request`, `maximum of 100 entries per scope exceeded`); no key already present ignoring case (`409 conflict`, `key "<k>" already exists`). All entries get the same `created_at`.
- **SEC-FR-008** Update takes `{"value": string}`. An absent or `null` value is `400`; `""` is valid. The key is matched ignoring case; no match is `404 not_found`, `key "<k>" not found`. The stored key casing and `created_at` are kept; `updated_at` becomes now.
- **SEC-FR-009** Delete matches the key ignoring case. No match is `404 not_found`. Success is `204` with no body.
- **SEC-FR-010** Lists are ordered by key, ascending, case-insensitive, and follow the list convention (§3). Secret lists carry no value.
- **SEC-FR-011** Secrets are encrypted at rest with the operator's key. When no key is configured, every operation that would store a secret value is refused with `503`, `error_type: secrets_unavailable`: secret create and update here, and the routes elsewhere that store a secret on the caller's behalf (workspace creation with git credentials, §4; webhook secret setup, §12). Listing and deleting secrets, and all variable routes, keep working.
- **SEC-FR-012** Key rotation. The hub accepts a second key, `PARLEY_SECRETS_KEY_PREVIOUS`, for decryption only. New and updated values always use the current key. The hourly maintenance pass re-encrypts every value still under the previous key (§11.7).
- **SEC-FR-013** A stored secret that cannot be decrypted (no key, or a key that matches neither configured key) stays listed, can be overwritten by an update or deleted, and fails any operation that needs its value with a clear error naming the key but not the value.
- **SEC-FR-014** Deleting a workspace deletes its secrets and variables in the same transaction; this includes expiry of a task fork. Deleting an org or a user deletes that scope's entries. A workspace re-created under the same slug starts empty.
- **SEC-FR-015** `GET /workspaces/:slug/vars/resolved` applies the resolution chain and needs only variable read access on `:slug`. A fork's resolved set includes the parent's variables with `origin: "parent"`.
- **SEC-FR-016** Each created entry, each update and each delete emits one event (§11.8). A failed request emits none.
- **SEC-FR-017** `resolved` is reserved under `/workspaces/:slug/vars/`: it cannot be addressed as a key by `PATCH` or `DELETE` on that path (`405`). There is no route that reads a single entry by key.

### 11.5 REST API

All paths are under `/api/v1`. Errors use the envelope and types of §3. The scopes differ only in access (SEC-FR-004 to SEC-FR-006), so each operation is specified once on the user secret route and the others refer to it.

Errors common to all routes: `401 unauthenticated`; `403 forbidden` (PAT scope, admin token on user scope, member on a workspace variable write); `403 token_bound`; `404` (org, workspace); `415`; `413`; `500 internal` (the cause is never sent).

#### `POST /user/secrets`
- **Scope:** `secrets:write`.
- **Body:** `{"entries":[{"key": string, "value": string}, …]}`, validated per SEC-FR-007. Accepts `Idempotency-Key`.
- **Response:** `201`, `{"items":[{key, created_at, updated_at}, …]}` in request order.
- **Errors:** `400 invalid_request`; `409 conflict`; `503 secrets_unavailable`.
- **Side effects:** one `hub.secret.create` per entry.

#### `GET /user/secrets`
- **Scope:** `secrets:read`.
- **Response:** `200`, `{"items":[…],"next_cursor":…}` of secret entries, ordered by key. Never contains values.

#### `PATCH /user/secrets/:key`
- **Scope:** `secrets:write`.
- **Body:** `{"value": string}`, per SEC-FR-008.
- **Response:** `200`, the secret entry.
- **Errors:** `400 invalid_request`; `404 not_found`; `503 secrets_unavailable`.
- **Side effects:** `hub.secret.update`.

#### `DELETE /user/secrets/:key`
- **Scope:** `secrets:write`.
- **Response:** `204`. **Errors:** `404 not_found`.
- **Side effects:** `hub.secret.delete`.

#### `POST /orgs/:slug/secrets`
As `POST /user/secrets`, in the org scope.

#### `GET /orgs/:slug/secrets`
As `GET /user/secrets`, in the org scope.

#### `PATCH /orgs/:slug/secrets/:key`
As `PATCH /user/secrets/:key`, in the org scope.

#### `DELETE /orgs/:slug/secrets/:key`
As `DELETE /user/secrets/:key`, in the org scope.

#### `POST /workspaces/:slug/secrets`
As `POST /user/secrets`, in the workspace scope. Owner or admin token only.

#### `GET /workspaces/:slug/secrets`
As `GET /user/secrets`, in the workspace scope. Lists only this workspace's own secrets, never those of a parent.

#### `PATCH /workspaces/:slug/secrets/:key`
As `PATCH /user/secrets/:key`, in the workspace scope.

#### `DELETE /workspaces/:slug/secrets/:key`
As `DELETE /user/secrets/:key`, in the workspace scope. Deleting `FORGE_WEBHOOK_SECRET` disables the webhook for the workspace.

#### `POST /user/vars`
As `POST /user/secrets`, with scope `vars:write`. The `201` items include `value`. Never answers `503`. Emits one `hub.variable.create` per entry.

#### `GET /user/vars`
As `GET /user/secrets`, with scope `vars:read`. Items include `value`.

#### `PATCH /user/vars/:key`
As `PATCH /user/secrets/:key`, with scope `vars:write`. Answers the variable entry. Emits `hub.variable.update`.

#### `DELETE /user/vars/:key`
As `DELETE /user/secrets/:key`, with scope `vars:write`. Emits `hub.variable.delete`.

#### `POST /orgs/:slug/vars`
As `POST /user/vars`, in the org scope.

#### `GET /orgs/:slug/vars`
As `GET /user/vars`, in the org scope.

#### `PATCH /orgs/:slug/vars/:key`
As `PATCH /user/vars/:key`, in the org scope.

#### `DELETE /orgs/:slug/vars/:key`
As `DELETE /user/vars/:key`, in the org scope.

#### `POST /workspaces/:slug/vars`
As `POST /user/vars`, in the workspace scope. Owner or admin token; an org member gets `403 forbidden`.

#### `GET /workspaces/:slug/vars`
As `GET /user/vars`, in the workspace scope. Owner, admin token or org member. Lists only this workspace's own variables.

#### `PATCH /workspaces/:slug/vars/:key`
As `PATCH /user/vars/:key`, in the workspace scope. An org member gets `403 forbidden`.

#### `DELETE /workspaces/:slug/vars/:key`
As `DELETE /user/vars/:key`, in the workspace scope. An org member gets `403 forbidden`.

#### `GET /workspaces/:slug/vars/resolved`
- **Purpose:** the variables a workspace or fork runs with (resolution chain, §11.3).
- **Scope:** `vars:read`. Workspace read access: owner, admin token or org member.
- **Response:** `200`, list of resolved entries `{key, value, origin, created_at, updated_at}`, ordered by key.

### 11.6 CLI

`parley secrets` and `parley vars` follow the global CLI conventions (§3). Specific to these commands:

- **Scope flags:** `--user`, `--org <slug>`, `--workspace <slug>`. Without a flag the scope is the user. `list`, `update` and `delete` accept at most one (more is a usage error, exit 2).
- **Multi-scope create:** `create` accepts several scope flags. One request is sent per scope, in the order user → org → workspace, and each result is printed in turn. A failing scope does not stop the others. Each failed scope's error envelope is printed exactly once, on stderr, and the command exits 1 if any scope failed.
- **Values:** `KEY=VALUE` is split at the first `=`, so a value may contain `=`. A comma separates entries, so a value with a comma needs `--from-stdin`. `--from-stdin` takes one bare `KEY` and reads the value from standard input, dropping one trailing line ending.
- Key naming rules are checked by the hub. Malformed arguments are usage errors (exit 2) and nothing is sent.

#### `parley secrets create <KEY=VALUE[,KEY=VALUE…]>… | <KEY> --from-stdin`
Calls `POST /<scope>/secrets` once per scope flag. Prints each scope's created entries.

#### `parley secrets list`
Calls `GET /<scope>/secrets`. Prints keys and timestamps, all pages.

#### `parley secrets update <KEY=VALUE> | <KEY> --from-stdin`
Calls `PATCH /<scope>/secrets/<KEY>`. Prints the updated entry.

#### `parley secrets delete <KEY>`
Calls `DELETE /<scope>/secrets/<KEY>`. Prints `{"deleted":"<KEY>"}`.

#### `parley vars create <KEY=VALUE[,KEY=VALUE…]>… | <KEY> --from-stdin`
As `parley secrets create`, on `/vars`. The output includes values.

#### `parley vars list`
As `parley secrets list`, on `/vars`. The output includes values.

#### `parley vars update <KEY=VALUE> | <KEY> --from-stdin`
As `parley secrets update`, on `/vars`.

#### `parley vars delete <KEY>`
As `parley secrets delete`, on `/vars`.

#### `parley vars resolve <workspace-slug>`
Calls `GET /workspaces/<slug>/vars/resolved`. Takes no scope flags. Prints the resolved entries with `origin`.

### 11.7 Background behaviour

- **At start** the hub reports the state of secret encryption in its log: key configured or not, whether a previous key is present, and the number of stored secrets that cannot be decrypted with the configured keys. Without a key it logs a warning that secrets cannot be stored. A malformed key stops the start with an error.
- **Hourly maintenance pass** (§3): every secret still encrypted under the previous key is re-encrypted under the current key. The pass does not change `updated_at`, emits no events and logs how many values it converted and how many remain.

### 11.8 Events

| Event type | `resource_type` | `action` | When |
|---|---|---|---|
| `hub.secret.create` | `secret` | `create` | per entry of a successful create |
| `hub.secret.update` | `secret` | `update` | successful update |
| `hub.secret.delete` | `secret` | `delete` | successful delete |
| `hub.variable.create` / `.update` / `.delete` | `variable` | `create` / `update` / `delete` | as above |

Every event carries `resource_id` = the stored key and `metadata` `{"scope": "user"|"org"|"workspace", "scope_id": <user id, org id or workspace slug>, "key": <key>}`. For the workspace scope the event's `workspace` field is the slug; otherwise it is null. Values never appear, for variables either.

Visibility in `/audit` and `/events` (§14) follows access to the scope: user-scope events are visible to that user; org-scope events to the org's members; workspace-scope variable events to everyone who can read the workspace; workspace-scope secret events only to callers with secret access on it (owner, admin token), so secret names do not leak to members. Admin tokens see all.

Secrets stored through other routes (workspace creation, webhook setup) emit those routes' events instead.

### 11.9 Configuration

| Key | Default | Meaning |
|---|---|---|
| `PARLEY_SECRETS_KEY` (environment) | unset | Current encryption key: 32 random bytes, base64-encoded |
| `[secrets].key_file` | unset | Path of a file holding the same key. The environment variable wins when both are set |
| `PARLEY_SECRETS_KEY_PREVIOUS` (environment) | unset | Former key, accepted for decryption only, during rotation |

To rotate: set the new key as `PARLEY_SECRETS_KEY` and the old one as `PARLEY_SECRETS_KEY_PREVIOUS`, wait for the maintenance pass to report zero remaining, then remove the previous key. The hub never generates, stores or prints a key. Losing a key loses the values encrypted with it.

### 11.10 Edge cases and error handling

- A full scope answers `400` on create even when a key of the request also conflicts.
- An empty `GIT_PAT` counts as no credential.
- No key configured: secret create and update answer `503 secrets_unavailable`; git and forge operations that need a stored credential fail with an error that says the secret is unreadable; public repositories keep working; a sandbox whose profile requires an unreadable secret fails as for a store error (§13).
- A fork with a variable of the same name as its parent's wins over the parent; deleting the fork's entry exposes the parent's again.
- A fork owner who loses write access to the parent loses the parent step for secrets at the next resolution; sandboxes already created keep what they were given. The parent step for variables is unaffected.
- A fork owner who leaves the org loses the org step of the fork's chain; the parent step stays.

---

## 12. Forge integration

### 12.1 Purpose and scope

A workspace's repository usually lives on a forge: GitHub or GitLab. The hub is the forge client of each workspace. Callers read and write the issues, labels, comments and pull requests (GitLab: merge requests) of that repository **through the hub**, with the credential the hub stores for the workspace. The caller never holds a forge token. The hub also receives the forge's webhook deliveries for a workspace, verifies them against a per-workspace secret, records each one once and relays it as `hub.forge.*` events.

**Relation to changes.** Parley's own unit of review and landing is the change (§7, §8). The forge remains the system of record for issues, and for teams that review on pull requests. The two are independent:

- The hub never acts on a forge event. A delivery does not open, update, approve or land a change, and it does not trigger a sync.
- A change is not a pull request and does not create one. A pull request opened through this section is an ordinary forge pull request between branches that exist on the forge.
- Forge objects carry no reference to changes, and changes carry no reference to forge objects. A caller that wants to connect them does so in text (a title, an intent, a comment).

Out of scope: pushing branches to origin (§4); registering the webhook on the forge (the hub never does this; a person enters the URL and the secret in the forge's settings); forges other than GitHub and GitLab.

### 12.2 User stories

- **FRG-US-01** As an agent with a `forge:read` token, I want to list and read the issues and pull requests of my workspace's repository, so that I can triage without a forge credential.
- **FRG-US-02** As an agent with `forge:write`, I want to create issues, comment, label and open pull requests with the workspace's stored credential. Every write that reached the forge leaves exactly one event, whether it succeeded or failed.
- **FRG-US-03** As a developer, I want to set a webhook secret, generated or my own, and get the URL to enter on the forge. A generated secret is shown once.
- **FRG-US-04** As the forge, I post deliveries to `/hooks/forge/:slug`. A valid delivery is acknowledged quickly and relayed once. A redelivery is answered `duplicate`.
- **FRG-US-05** As an org member with read-only access, I can read the forge through the workspace's credential but not write.
- **FRG-US-06** As a carry-patch developer, I want `--target upstream` to read and write the upstream repository with the upstream credential.
- **FRG-US-07** As an agent working in a task fork, I want to read the parent's issues through my fork's slug, so that I do not need to know the parent's forge.

### 12.3 Concepts and data model

**Remote target.** The query parameter `target` selects the repository a route works on.

| Value | Remote | Credential | API URL setting |
|---|---|---|---|
| absent or `origin` | the workspace's `git_url` | the workspace's origin git credential (§4, §11) | workspace setting `forge_api_url` |
| `upstream` | the workspace's `upstream_url`; only for a carry-patch workspace (§10) that has one | the workspace's upstream git credential, which falls back to the origin credential (§10) | workspace setting `upstream_forge_api_url` (no fallback to `forge_api_url`) |

**Repository.** The hub derives `host`, `owner` and `name` from the remote URL. It accepts `https://`, `http://`, `ssh://` and `git://` URLs and the scp form `user@host:owner/name`. The host is lower-cased, a leading `www.` and any port are dropped, userinfo is ignored, and a trailing `.git` is removed. The path needs at least two segments: `name` is the last one, `owner` is everything before it (GitLab subgroups are kept: `group/subgroup`).

**Forge kind.** `github` or `gitlab`, decided in this order:

1. If the API URL setting is set and contains exactly one of `github` and `gitlab`, that forge.
2. Otherwise, if the remote's host contains exactly one of `github` and `gitlab`, that forge.
3. Otherwise the forge is unknown.

A self-hosted forge whose host name names neither product is made known by setting the API URL.

**API URL.** The setting, when set, with a trailing `/` removed. Otherwise the default for the host: `https://api.github.com` for `github.com`, `https://<host>/api/v3` for another GitHub host, `https://<host>/api/v4` for GitLab. A GitLab API URL that does not end in `/api/v4` gets it appended. The setting must be an absolute `https` URL with a host and without userinfo, query or fragment; §4 validates it when it is stored.

**Credential kind.** What the hub sends to the forge, derived from the workspace's git credential for the target:

| Kind | Stored credential | Sent to the forge |
|---|---|---|
| `pat` | an access token | the token |
| `basic` | a username and a password | the password, as the token |
| `none` | nothing | no authentication |

The hub sends only a credential stored for the workspace. It never reads a forge token from its own process environment, neither to authenticate nor to decide whether it is authenticated.

**Result types.** Timestamps follow §3; an absent timestamp is `null`. `url` is always the web URL of the object.

| Type | Fields |
|---|---|
| repository | `full_name`, `default_branch`, `private`, `archived`, `permissions {pull, push, admin}`, `allow_merge_commit`, `allow_squash_merge`, `allow_rebase_merge` (the last three `null` when the forge does not report them) |
| issue | `number`, `title`, `body`, `state` (`open`, `closed`), `url`, `author {login}`, `labels` (names), `created_at`, `updated_at`, `closed_at`, `is_pr` |
| issue thread | `issue`, `comments` (the first page, oldest first), `comments_next_cursor`, `comments_error` |
| comment | `id`, `body`, `author {login}`, `created_at`, `updated_at`, `url` |
| pull request | `number`, `title`, `body`, `state` (`open`, `closed`, `merged`), `url`, `draft`, `merged`, `head_sha`, `head_branch`, `base_branch`, `author {login}`, `created_at`, `updated_at`, `closed_at`, `merged_at` |
| pull request state | `number`, `state`, `merged`, `head_sha` |
| changed file | `filename`, `status` (`added`, `modified`, `deleted`, `renamed`), `additions`, `deletions` |
| check run | `name`, `status` (`queued`, `in_progress`, `completed`), `conclusion` (`success`, `failure`, `cancelled`, `skipped`, `neutral`, or `null` while not completed), `summary`, `url` |
| review | `author {login}`, `state` (`approved`, `changes_requested`, `commented`, `dismissed`), `body`, `submitted_at` |
| label | `name`, `color` (six lower-case hex digits, no `#`), `description` |

On GitLab, a number is the project-level `iid`; check runs are the jobs of the merge request's latest pipeline; reviews are the approvals (as `approved`) and the non-system notes (as `commented`), ordered by time.

**Webhook secret.** The workspace-scope secret `FORGE_WEBHOOK_SECRET` (§11). It is read on every delivery, so a rotation applies to the next delivery. The **webhook URL** is `server.external_url` without a trailing `/`, followed by `/hooks/forge/<slug>`; it is `null` when `external_url` is empty.

**Delivery.** One record per accepted webhook delivery. Payloads and secrets are never stored.

| Field | Type | Meaning |
|---|---|---|
| `delivery_id` | string | the forge's id for the delivery; unique per workspace |
| `forge` | string | `github` or `gitlab` |
| `event` | string | the forge's event name, cut to 64 bytes |
| `action` | string | the payload's action, `""` when it has none |
| `target` | string or null | `origin`, `upstream`, or `null` when no repository matched |
| `outcome` | string | `relayed`, `ignored`, `repository_mismatch` or `malformed` |
| `relayed` | array of strings | the event types relayed; `[]` unless `outcome` is `relayed` |
| `received_at` | timestamp | when the hub accepted it |

A fifth outcome, `duplicate`, is only ever answered to the forge. It is never recorded.

### 12.4 Functional requirements

**Access and order of checks**

- **FRG-FR-001** Read routes need `forge:read`; write routes and the webhook `PUT` and `DELETE` need `forge:write` (§3). Read routes need read access to the workspace; write and webhook routes need write access, so a read-only org member gets `403 forbidden`.
- **FRG-FR-002** Every forge route follows the check order of §3. Within it, the steps specific to this section run in this order: (1) `target`; (2) path and query parameters and the request body, as far as they do not depend on the forge kind; (3) the archived check; (4) forge configuration (FRG-FR-005); (5) checks that depend on the forge kind (FRG-FR-017, FRG-FR-024); (6) for writes, the credential check (FRG-FR-025); (7) the call. A request refused before step 7 sends nothing to the forge and records no event.
- **FRG-FR-003** `target` may be given at most once. `origin` or absent selects origin. `upstream` selects upstream when the workspace is a carry-patch workspace with an `upstream_url`; otherwise the answer is `400`, `error_type: workspace_mode_mismatch`. Any other value is `400 invalid_request`. The diagnostic route, the deliveries route and the webhook routes take no `target`.
- **FRG-FR-004** Writes and the webhook `PUT` are refused on an archived workspace with the archived-workspace error of §4. Reads, the deliveries list and the webhook `DELETE` work in any workspace state. No forge route needs the hub's copy of the repository, and none is affected by `workspace_busy`.
- **FRG-FR-005** Forge configuration is resolved in a fixed order, and the first failure wins: the API URL setting is not a valid URL → `422`, `error_type: forge_api_url_invalid`; the remote URL names no repository → `422`, `error_type: forge_unknown`; the forge kind is unknown → `422`, `error_type: forge_unknown`, with a message that names the setting to set.
- **FRG-FR-006** A `:number` path parameter is a positive decimal integer: digits only, no sign, no leading zero, at most 2,147,483,647. Anything else is `400 invalid_request` with the message `number must be a positive decimal integer`. The rule is the same on every route, read or write, and in the CLI.
- **FRG-FR-007** **Task forks.** A task fork (§4) has no forge of its own. Every forge route called with a fork's slug, except the webhook and deliveries routes, acts on the parent's forge repository with the parent's credential and the parent's settings, including `target=upstream`. The caller must have, on the parent, the access the route needs: read access for reads, write access for writes. A caller without it gets `403 forbidden`. A token bound to the fork is accepted for this purpose; the access that counts is that of the user behind it. The webhook `PUT` and `DELETE` and the deliveries list are refused on a fork with `400`, `error_type: fork_has_no_origin`.

**Outbound calls**

- **FRG-FR-008** Every forge request carries `User-Agent: parleyd/<version>` and honours the proxy variables of §12.9. A redirect is followed only to the same scheme, host and port, at most 10 times. Each request has a 30-second deadline. A response is read up to 8 MiB.
- **FRG-FR-009** A rate-limited request is retried once when the wait the forge states fits inside the request's deadline. Otherwise the caller gets `429` (FRG-FR-029). The rule is the same for every request the hub sends.

**Reads**

- **FRG-FR-010** A read without a stored credential is sent anonymously. Reads record no events.
- **FRG-FR-011** Lists follow the list convention of §3 (`items`, `next_cursor`, `limit`, `cursor`). A cursor is opaque and valid only for the same route with the same parameters; any other use is `400 invalid_request`.
- **FRG-FR-012** The issue list takes `state` (`open`, `closed`, `all`; default `open`), `sort` (`created`, `updated`; default `created`), `direction` (`asc`, `desc`; default `desc`), `labels` (comma-separated names; an issue must carry all of them) and `assignee` (a login). Each may be given at most once; a repeated parameter or a value outside the set is `400`. Pull requests are never part of the issue list.
- **FRG-FR-013** The issue thread is the issue plus its first page of comments (at most 100). `comments_next_cursor` continues on the comments route and is `null` when the page is complete. If the issue was read but its comments were not, the answer is still `200`, `comments` is `[]`, and `comments_error` is `{"error_type","message"}` with the values FRG-FR-029 would have given. Otherwise `comments_error` is `null`.

**Writes**

- **FRG-FR-014** A write body is a JSON object unless stated. An absent field and a `null` field both mean "not set". Unknown fields are ignored. A required string must not be blank. A list of names has at most 100 entries, none blank. A violation is `400 invalid_request` and names the field.
- **FRG-FR-015** **Create issue.** `title` required; `body`, `labels`, `assignees` optional. Answers `201` with the issue.
- **FRG-FR-016** **Update issue.** Any of `title` (not blank), `body` (`""` clears it), `state` (`open` or `closed`), `labels` and `assignees` (each replaces the whole set; `[]` clears it). No field set → `400`. Answers `200` with the issue as the forge returns it.
- **FRG-FR-017** **GitLab assignees.** GitLab takes assignees as user ids, which the hub does not resolve. On GitLab, `assignees` is not sent on create or update. An update whose only field is `assignees` is refused with `400 invalid_request`.
- **FRG-FR-018** **Close issue.** The body may be empty or `{"comment": string}`. A non-blank comment is posted first; the issue is closed only if the comment succeeded. If the comment fails, the answer is that failure and the issue is untouched. If the close fails after the comment was posted, the answer is the close's failure and the event carries `comment_url`. Closing a closed issue succeeds. Answers `200` with the issue.
- **FRG-FR-019** **Comment** on an issue or a pull request. `body` required. Answers `201` with `{"url": "<web URL of the comment>"}`.
- **FRG-FR-020** **Add labels.** The body is a bare JSON array of 1 to 100 non-blank names. The labels are added and the issue keeps its others. Answers `204`.
- **FRG-FR-021** **Remove label.** The label is the last path segment, percent-decoded once, so `type%2Fbug` names `type/bug`. A blank or undecodable name is `400`. The route is idempotent: it answers `204` when the issue exists, whether or not it carried the label, and `404 forge_not_found` when the issue does not exist.
- **FRG-FR-022** **Ensure label.** `name` required; `color` optional, six hex digits with or without a leading `#`, default `ededed`; `description` optional. If the repository has a label of that name, the answer is `200` with the label as the forge holds it, unchanged. Otherwise the label is created and the answer is `201`. If two callers race, the loser answers `200` with the winner's label.
- **FRG-FR-023** **Open pull request.** `title`, `head` and `base` required; `body` optional; `draft` optional, default `false`. `head` is sent as given, so `owner:branch` names a branch of a forge-side fork. Both branches must exist on the forge (§4). A pull request that already exists for the same head and base is the forge's refusal (`422 forge_rejected`). Answers `201` with the pull request.
- **FRG-FR-024** **Body limits.** Long text is refused before anything is sent: 65,536 characters (Unicode code points) on GitHub, 1,000,000 on GitLab. The measured fields are the `body` of an issue, a comment or a pull request, and a close's `comment`. Over the limit → `413 payload_too_large`, with a message that names the field, its length and the limit.
- **FRG-FR-025** **Missing credential.** When the credential kind for the target is `none`, every write route is refused with `422`, `error_type: forge_credential_missing`. Nothing is sent to the forge, including the lookups that some writes begin with, and no event is recorded.
- **FRG-FR-026** A write is not abandoned when the caller disconnects. It runs to its end and its event is recorded.
- **FRG-FR-027** The `POST` routes that create something (an issue, a comment, a pull request, a label) accept `Idempotency-Key` as §3 specifies. A replay sends nothing to the forge and records no event.
- **FRG-FR-028** **Events.** A write route records exactly one `hub.forge.*` event when it sent at least one request to the forge, whether the write succeeded or failed. The event is recorded before the response is sent.

**Error mapping**

- **FRG-FR-029** A failed forge call maps as follows; the first matching row wins.

| Condition | Status | `error_type` |
|---|---|---|
| redirect to another host or scheme, or more than 10 redirects | 502 | `forge_error` |
| no answer within the deadline | 504 | `forge_timeout` |
| rate limited, after the retry of FRG-FR-009 | 429 | `forge_rate_limited` |
| the forge answers 404 | 404 | `forge_not_found` |
| the forge answers 401 | 502 | `forge_unauthenticated` |
| the forge answers 403 (not a rate limit) | 502 | `forge_forbidden` |
| the forge answers any other 4xx | 422 | `forge_rejected` |
| the forge answers 5xx or something the hub cannot read | 502 | `forge_error` |
| the connection fails (DNS, TCP, TLS) | 502 | `forge_unreachable` |

- **FRG-FR-030** The message is `the forge answered <status>: <the forge's message>`, or a plain description when the forge gave no answer. A `429` carries a `Retry-After` header in whole seconds when the forge stated a wait.
- **FRG-FR-031** Every message is scrubbed before it leaves the hub: the workspace's token is replaced by `[redacted]`, URL userinfo is removed, and the text is cut to 1,024 bytes. No response, event or log line carries a forge credential.

**Webhook management**

- **FRG-FR-032** `PUT …/forge/webhook` stores the webhook secret. With no body, `{}` or `{"secret": null}`, the hub generates one: 32 random bytes as 64 lower-case hex characters. A given `secret` must be a string of 16 to 255 printable ASCII characters without spaces (`!` to `~`); otherwise `400 invalid_request`. An error message never contains the value. A generated secret is returned in this one response and never again.
- **FRG-FR-033** `DELETE …/forge/webhook` removes the secret and answers `204` whether or not one existed. From then on the ingress answers `404` for the workspace. Recorded deliveries remain.
- **FRG-FR-034** The hub never registers, changes or deletes a webhook on the forge.

**Ingress**

- **FRG-FR-035** `POST /hooks/forge/:slug` is public: it takes no bearer credential, and the delivery's signature or token is its authentication. It never contacts the forge. An archived workspace still relays.
- **FRG-FR-036** The ingress decides in this order:
  1. Unknown slug, a task fork's slug, or a workspace without a webhook secret → `404 not_found`, with one identical body for all three.
  2. Scheme: a request with `X-Hub-Signature-256` is a GitHub delivery; otherwise one with `X-Gitlab-Token` is a GitLab delivery; otherwise `401`.
  3. GitLab: the token header must appear exactly once and equal the secret, else `401`.
  4. Body larger than 1 MiB → `413 payload_too_large`.
  5. GitHub: the signature header must appear exactly once and equal `sha256=` followed by the lower-case hex HMAC-SHA256 of the raw body under the secret, else `401`.
  6. Identity: the delivery id is `X-GitHub-Delivery` on GitHub, and `Idempotency-Key` or else `X-Gitlab-Event-UUID` on GitLab. The event name is `X-GitHub-Event` or `X-Gitlab-Event`. A missing id, a missing event name, or an id that is not 1 to 255 printable ASCII characters → `400 invalid_request`; nothing is recorded.
  7. The delivery is reduced (FRG-FR-037) and recorded. If the workspace already has a delivery with this id, the answer is `202 {"outcome":"duplicate"}` and nothing else happens.
  8. Outcome `malformed` → `400 invalid_request`. The delivery stays recorded.
  9. Otherwise `202 {"outcome": …}`.
- **FRG-FR-037** Secrets are compared in constant time. Every `401` has the same message, `delivery signature missing or invalid`, with `error_type: unauthenticated`, and records nothing.
- **FRG-FR-038** **Reduction.** The body must be one JSON object, else `malformed`. The hub reads the action (GitHub `action`; GitLab `object_attributes.action`) and the repository path (GitHub `repository.full_name`; GitLab `project.path_with_namespace`). The path is compared, ignoring case, with the origin repository and then, for a carry-patch workspace, with the upstream repository; this gives `target`. Then:

| Situation | Outcome |
|---|---|
| no mapping for the event and action (FRG-FR-040) | `ignored` |
| a field the mapping requires is missing or of the wrong type | `malformed` |
| the mapping yields no events | `ignored` |
| events, but the payload names no repository | `malformed` |
| events, but the repository is neither origin nor upstream | `repository_mismatch` |
| otherwise | `relayed` |

- **FRG-FR-039** Recording a delivery and appending its relayed events to the event log (§14) is one atomic step, completed before the `202`. A delivery recorded as `relayed` therefore always has its events, even if the forge closed the connection early. Of several concurrent copies of one delivery, exactly one is recorded; the others are answered `duplicate`.
- **FRG-FR-040** **Mapping.** Anything not listed is ignored, for example GitHub `ping` and edited comments.

| Forge | Event and action | Hub event | Required |
|---|---|---|---|
| GitHub | `issues`: `opened`, `edited`, `closed`, `reopened` | `hub.forge.issue.<action>` | `issue.number` |
| GitHub | `issues`: `labeled`, `unlabeled` | `hub.forge.issue.<action>` | `issue.number`, `label.name` |
| GitHub | `issue_comment`: `created` | `hub.forge.comment.created` | `issue.number` |
| GitHub | `pull_request`: `opened`, `reopened`, `synchronize` | `hub.forge.pull_request.<action>` | `pull_request.number` |
| GitHub | `pull_request`: `closed` | `.merged` when `pull_request.merged` is true, else `.closed` | `pull_request.number` |
| GitHub | `push` | `hub.forge.push` | `ref` |
| GitLab | `Issue Hook`: `open`, `close`, `reopen` | `hub.forge.issue.opened`, `.closed`, `.reopened` | `object_attributes.iid` |
| GitLab | `Issue Hook`: `update` | `.edited` when the title or description changed; then one `.unlabeled` per removed label and one `.labeled` per added label, each group in name order | `object_attributes.iid` |
| GitLab | `Note Hook` on an issue or a merge request | `hub.forge.comment.created` | the `iid` of the issue or merge request |
| GitLab | `Merge Request Hook`: `open`, `close`, `reopen`, `merge` | `hub.forge.pull_request.opened`, `.closed`, `.reopened`, `.merged` | `object_attributes.iid` |
| GitLab | `Merge Request Hook`: `update` | `.synchronize` when new commits were pushed (`oldrev` present), else ignored | `object_attributes.iid` |
| GitLab | `Push Hook`, `Tag Push Hook` | `hub.forge.push` | `ref` |

One delivery relays at most 100 label events.

**Delivery log**

- **FRG-FR-041** Deliveries are kept for 7 days. Deleting a workspace deletes its deliveries in the same step, so a slug that is used again starts with an empty log.

### 12.5 REST API

All routes except the ingress are under `/api/v1`, use the error envelope and check order of §3, and follow FRG-FR-002. Unless a route says otherwise:

- A **read** route needs `forge:read` and read access, takes `target`, and fails with the forge errors: the `422` configuration errors of FRG-FR-005 and the mapped failures of FRG-FR-029.
- A **list** route is a read route that also takes `limit` and `cursor` and answers a list (§3).
- A **write** route needs `forge:write` and write access and takes `target`. It fails with the forge errors, the archived refusal, `413 payload_too_large` where text is measured, and `422 forge_credential_missing`.

#### `GET /workspaces/:slug/forge`
Diagnostic: says how the hub reaches the forge, without contacting it. Needs `forge:read` and read access; takes no `target`. Answers `200`:
```json
{"forge": "github",
 "api_url": "https://api.github.com",
 "repository": {"host": "github.com", "owner": "acme", "name": "widgets"},
 "credential": "pat",
 "upstream": null,
 "webhook": {"configured": true,
             "url": "https://hub.example.com/hooks/forge/widgets",
             "last_delivery_at": "2026-10-21T09:12:03Z"}}
```
`forge` is `github`, `gitlab` or `unknown`. A remote that names no repository or an undetectable forge is reported (`forge: "unknown"`, `api_url: null`, and `repository: null` when there is none); it is not an error here. `upstream` has the same four fields for a carry-patch workspace with an `upstream_url`, else `null`. `webhook.url` and `webhook.last_delivery_at` may be `null`. For a task fork the whole document describes the parent. The only forge error is `422 forge_api_url_invalid`.

#### `GET /workspaces/:slug/forge/repository`
Read route. Answers `200` with the repository as the forge reports it.

#### `GET /workspaces/:slug/issues`
List route, with the filters `state`, `sort`, `direction`, `labels` and `assignee` (FRG-FR-012). Answers a list of issues.

#### `GET /workspaces/:slug/issues/:number`
Read route. Answers `200` with the issue thread (FRG-FR-013). On GitHub, the number of a pull request answers the issue with `is_pr: true`.

#### `GET /workspaces/:slug/issues/:number/comments`
List route. Answers the issue's comments, oldest first.

#### `GET /workspaces/:slug/pulls/:number`
Read route. Answers `200` with the pull request.

#### `GET /workspaces/:slug/pulls/:number/state`
Read route; the short form for polling. Answers `200` with the pull request state.

#### `GET /workspaces/:slug/pulls/:number/files`
List route. Answers the files the pull request changes.

#### `GET /workspaces/:slug/pulls/:number/checks`
List route. Answers the check runs of the pull request's head; empty when it has none.

#### `GET /workspaces/:slug/pulls/:number/reviews`
List route. Answers the pull request's reviews, oldest first.

#### `POST /workspaces/:slug/issues`
Write route (FRG-FR-015). Body: `{"title": string, "body"?: string, "labels"?: [string], "assignees"?: [string]}`. Answers `201` with the issue. Records `hub.forge.issue.create`.

#### `PATCH /workspaces/:slug/issues/:number`
Write route (FRG-FR-016, FRG-FR-017). Body: `{"title"?: string, "body"?: string, "state"?: "open"|"closed", "labels"?: [string], "assignees"?: [string]}`. Answers `200` with the issue. Records `hub.forge.issue.update`.

#### `POST /workspaces/:slug/issues/:number/close`
Write route (FRG-FR-018). Body: empty or `{"comment"?: string}`. Answers `200` with the issue. Records `hub.forge.issue.close`.

#### `POST /workspaces/:slug/issues/:number/comments`
Write route (FRG-FR-019). Body: `{"body": string}`. Answers `201` with `{"url": string}`. Records `hub.forge.comment.create`.

#### `POST /workspaces/:slug/issues/:number/labels`
Write route (FRG-FR-020). Body: a bare array `["name", …]`. Answers `204`. Records `hub.forge.label.add`.

#### `DELETE /workspaces/:slug/issues/:number/labels/:name`
Write route (FRG-FR-021). Answers `204`, or `404 forge_not_found` when the issue does not exist. Records `hub.forge.label.remove`.

#### `POST /workspaces/:slug/labels`
Write route (FRG-FR-022). Body: `{"name": string, "color"?: string, "description"?: string}`. Answers `200` with the existing label or `201` with the created one. Records `hub.forge.label.create` in both cases.

#### `POST /workspaces/:slug/pulls`
Write route (FRG-FR-023). Body: `{"title": string, "head": string, "base": string, "body"?: string, "draft"?: boolean}`. Answers `201` with the pull request. Records `hub.forge.pull_request.create`.

#### `POST /workspaces/:slug/pulls/:number/comments`
Write route (FRG-FR-019); the comment goes to the pull request's conversation. Body: `{"body": string}`. Answers `201` with `{"url": string}`. Records `hub.forge.pull_request.comment`.

#### `PUT /workspaces/:slug/forge/webhook`
Sets or rotates the webhook secret (FRG-FR-032). Needs `forge:write` and write access; refused on an archived workspace and on a task fork. Body: optional `{"secret"?: string}`. Answers `200` with `{"configured": true, "url": string|null, "secret"?: string}`; `secret` is present only when the hub generated it. `400` for a bad secret, or when the workspace's secret scope is full (§11). Records `hub.forge.webhook.set`.

#### `DELETE /workspaces/:slug/forge/webhook`
Removes the webhook secret (FRG-FR-033). Needs `forge:write` and write access; works in any workspace state; refused on a task fork. Answers `204`. Records `hub.forge.webhook.unset`.

#### `GET /workspaces/:slug/forge/deliveries`
Lists recorded deliveries (§12.3), newest first. Needs `forge:read` and read access; refused on a task fork. Query: `since` (RFC 3339; only deliveries received at or after it; a bad value is `400`), `limit`, `cursor`.
```json
{"items": [{"delivery_id": "72d3162e-cc78-11e3-81ab-4c9367dc0958", "forge": "github",
            "event": "issues", "action": "labeled", "target": "origin",
            "outcome": "relayed", "relayed": ["hub.forge.issue.labeled"],
            "received_at": "2026-10-21T09:12:03Z"}],
 "next_cursor": null}
```

#### `POST /hooks/forge/:slug`
Webhook ingress for the forge (FRG-FR-035 to FRG-FR-040). No bearer credential; the delivery's signature (GitHub) or token (GitLab) authenticates it. Body: the forge's payload, at most 1 MiB. Answers `202` with `{"outcome": "relayed"|"ignored"|"repository_mismatch"|"duplicate"}`. Errors: `404 not_found`, `401 unauthenticated`, `413 payload_too_large`, `400 invalid_request` (no delivery id, no event name, malformed payload). Side effects: a delivery record and the relayed events.

### 12.6 CLI

The conventions of §3 apply. Every command prints the hub's answer unless a different output is stated. Slugs, numbers and label names are sent as single percent-encoded path segments. A flag that is not given sends nothing, so the hub's defaults apply.

- `<number>` is checked with the rule of FRG-FR-006 before anything is sent; a violation is a usage error (exit 2).
- `--target <origin|upstream>` is available on every command that calls a route with a `target` parameter.
- `--limit <n>` and `--cursor <cursor>` are available on every list command.
- Text input: `--body <text>` gives the text; `--body-file <path>` reads it from a file, or from stdin when the path is `-`. The text is sent exactly as given. Giving both is a usage error.

#### `parley forge status <workspace-slug>`
Calls `GET /workspaces/:slug/forge`.

#### `parley forge repository <workspace-slug>`
Calls `GET /workspaces/:slug/forge/repository`. Flags: `--target`.

#### `parley forge webhook set <workspace-slug>`
Calls `PUT /workspaces/:slug/forge/webhook`. Flags, mutually exclusive: `--secret <string>`; `--from-stdin` (reads the secret from stdin and drops one trailing line break); `--generate` (the default). When the answer contains `secret`, a note on stderr says that it will not be shown again.

#### `parley forge webhook unset <workspace-slug>`
Calls `DELETE /workspaces/:slug/forge/webhook`. Prints `{"status":"unset","workspace":"<slug>"}`.

#### `parley forge deliveries <workspace-slug>`
Calls `GET /workspaces/:slug/forge/deliveries`. Flags: `--since <rfc3339>`, `--limit`, `--cursor`.

#### `parley issue list <workspace-slug>`
Calls `GET /workspaces/:slug/issues`. Flags: `--state`, `--label` (repeatable; sent as one `labels` parameter), `--assignee`, `--limit`, `--cursor`, `--target`.

#### `parley issue get <workspace-slug> <number>`
Calls `GET /workspaces/:slug/issues/:number`. Flags: `--target`.

#### `parley issue comments <workspace-slug> <number>`
Calls `GET /workspaces/:slug/issues/:number/comments`. Flags: `--limit`, `--cursor`, `--target`.

#### `parley issue create <workspace-slug>`
Calls `POST /workspaces/:slug/issues`. Flags: `--title` (required), `--body` or `--body-file`, `--label` (repeatable, one name each), `--assignee` (repeatable), `--target`.

#### `parley issue update <workspace-slug> <number>`
Calls `PATCH /workspaces/:slug/issues/:number` with only the given fields. Flags: `--title`, `--body` or `--body-file`, `--state`, `--label` (repeatable), `--assignee` (repeatable), `--target`. With no field flag it is a usage error. `--body ""` clears the body.

#### `parley issue close <workspace-slug> <number>`
Calls `POST /workspaces/:slug/issues/:number/close`. Flags: `--comment <text>`, `--target`.

#### `parley issue comment <workspace-slug> <number>`
Calls `POST /workspaces/:slug/issues/:number/comments`. Flags: `--body` or `--body-file` (one is required), `--target`.

#### `parley issue label <workspace-slug> <number> <label>...`
Calls `POST /workspaces/:slug/issues/:number/labels` with the labels. Flags: `--target`. Prints `{"status":"labeled","number":<n>,"labels":[…]}`.

#### `parley issue unlabel <workspace-slug> <number> <label>`
Calls `DELETE /workspaces/:slug/issues/:number/labels/:name`. Flags: `--target`. Prints `{"status":"unlabeled","number":<n>,"label":"<label>"}`.

#### `parley label create <workspace-slug> <name>`
Calls `POST /workspaces/:slug/labels`. Flags: `--color <hex>`, `--description <text>`, `--target`. Exit 0 whether the label existed or was created.

#### `parley pr get <workspace-slug> <number>`
Calls `GET /workspaces/:slug/pulls/:number`. Flags: `--target`.

#### `parley pr state <workspace-slug> <number>`
Calls `GET /workspaces/:slug/pulls/:number/state`. Flags: `--target`.

#### `parley pr files <workspace-slug> <number>`
Calls `GET /workspaces/:slug/pulls/:number/files`. Flags: `--limit`, `--cursor`, `--target`.

#### `parley pr checks <workspace-slug> <number>`
Calls `GET /workspaces/:slug/pulls/:number/checks`. Flags: `--limit`, `--cursor`, `--target`.

#### `parley pr reviews <workspace-slug> <number>`
Calls `GET /workspaces/:slug/pulls/:number/reviews`. Flags: `--limit`, `--cursor`, `--target`.

#### `parley pr create <workspace-slug>`
Calls `POST /workspaces/:slug/pulls`. Flags: `--title`, `--head`, `--base` (all required), `--body` or `--body-file`, `--draft`, `--target`.

#### `parley pr comment <workspace-slug> <number>`
Calls `POST /workspaces/:slug/pulls/:number/comments`. Flags: `--body` or `--body-file` (one is required), `--target`.

### 12.7 Background behaviour

- The maintenance loop (§3) deletes deliveries older than 7 days, once at start and then every hour. A failure is logged and retried at the next pass.
- Nothing else in this section runs in the background. Forge reads and writes are synchronous within the API request; nothing is cached or queued, and the hub does not poll the forge.

### 12.8 Events

All events carry the common fields of §14. No forge event ever carries a title of a write, a body, a comment text, a credential, a webhook secret or a forge error message.

**Write events.** The actor is the caller. `workspace` is the workspace whose forge was written; for a call through a task fork that is the parent, and `metadata.fork` holds the fork's slug. Common metadata: `target`, `ok`, `error_type` (when `ok` is false), `number` (when known) and `url` (the web URL of the resulting issue, pull request or comment, when known).

| Event | `resource_type` | `resource_id` | `action` | Extra metadata |
|---|---|---|---|---|
| `hub.forge.issue.create` | `issue` | the new number; empty on failure | `create` | none |
| `hub.forge.issue.update` | `issue` | number | `update` | `fields` (names of the fields sent, sorted) |
| `hub.forge.issue.close` | `issue` | number | `close` | `comment_url` (when a comment was posted) |
| `hub.forge.comment.create` | `comment` | issue number | `create` | none |
| `hub.forge.label.add` | `label` | issue number | `add` | `labels` |
| `hub.forge.label.remove` | `label` | issue number | `remove` | `label` |
| `hub.forge.label.create` | `label` | label name | `create` | `label`, `created` (true only when this call created it) |
| `hub.forge.pull_request.create` | `pull_request` | the new number; empty on failure | `create` | `head`, `base`, `draft` |
| `hub.forge.pull_request.comment` | `pull_request` | number | `comment` | none |

**Webhook events.** The actor is the caller; `resource_type` is `webhook` and `resource_id` is the slug.

| Event | `action` | Metadata |
|---|---|---|
| `hub.forge.webhook.set` | `set` | `generated` (boolean) |
| `hub.forge.webhook.unset` | `unset` | `existed` (boolean) |

**Delivery events.** `actor_type` is `forge` and `actor_id` is empty. `action` is the last segment of the type. Common metadata: `forge`, `target`, `delivery_id` and `actor` (the forge login that caused the delivery, `""` when the payload names none).

| Event | `resource_type` | `resource_id` | Extra metadata |
|---|---|---|---|
| `hub.forge.issue.opened`, `.edited`, `.closed`, `.reopened` | `issue` | number | `number`, `title` (cut to 256 bytes), `url`, `labels` (at most 100 names) |
| `hub.forge.issue.labeled`, `.unlabeled` | `issue` | number | `number`, `label`, `labels` (the current set, at most 100), `url` |
| `hub.forge.comment.created` | `comment` | issue or pull request number | `number`, `comment_url`, `pull_request` (boolean) |
| `hub.forge.pull_request.opened`, `.closed`, `.merged`, `.reopened`, `.synchronize` | `pull_request` | number | `number`, `url`, `head`, `base`, `merged` (boolean) |
| `hub.forge.push` | `repository` | the ref | `ref`, `before`, `after` |

A merged pull request yields `.merged` and never also `.closed`. `hub.forge.push` reports a push that the forge received. It is unrelated to `hub.git.push` (§5), which reports a push that the hub received.

### 12.9 Configuration

| Key | Type | Default | Effect |
|---|---|---|---|
| `server.external_url` | string | `""` | base of the webhook URL. When empty, the URL is reported as `null`; a secret can still be set |
| workspace setting `forge_api_url` | URL | unset | API URL of the origin's forge; also decides the forge kind |
| workspace setting `upstream_forge_api_url` | URL | unset | the same for the upstream |
| workspace secret `FORGE_WEBHOOK_SECRET` | string | unset | the webhook secret |
| `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` (environment) | | unset | proxying of forge calls |

The hub reads no other environment variable for the forge. In particular it ignores `GITHUB_TOKEN`, `GH_TOKEN`, `GITLAB_TOKEN`, `GITHUB_API_URL` and `GITLAB_API_URL`.

The limits stated in §12.4 (deadlines, redirects, sizes, lengths, retention) are fixed and not configurable.

### 12.10 Edge cases and error handling

- A token without the forge scope gets `403` even for a slug that does not exist, because the scope check comes first (§3). A read-only member's write is `403` even on an archived workspace, because access comes before state.
- Malformed JSON is `400` even when the forge cannot be detected. An oversized body on a workspace with an unknown forge is `422`, because the limit depends on the forge kind.
- Ingress with both scheme headers is treated as GitHub; a wrong GitLab token is then irrelevant. A scheme header given twice is `401`.
- An oversized GitHub delivery answers `413` whatever its signature, because the signature covers the body and the body is not read past the limit.
- A correctly signed GitHub delivery in the legacy form encoding is recorded as `malformed` and answered `400`. A delivery with only the legacy SHA-1 signature header is `401`.
- A redelivery of a `malformed` delivery is answered `202` with `duplicate`.
- A `FORGE_WEBHOOK_SECRET` stored through the secrets API (§11) is not bound by the 16-to-255 rule; any non-empty value works at the ingress.
- A task fork never receives deliveries. Events for the parent's repository arrive on the parent's slug.

---

## 13. Sandboxes

### 13.1 Purpose and scope

The hub lends isolated sandboxes (containers, optionally micro-VMs) to agents. A sandbox comes filled with the secrets the agent needs, and the caller never sees those secrets.

An operator defines the allowed shapes as **profiles** in the `[sandbox]` section of the hub configuration. A caller obtains a **lease** over REST and receives a one-time **lease token**. With that token the caller connects to the hub's **sandbox gateway**, a gRPC listener separate from the REST port that speaks the AgentKit-compatible gateway protocol, creates exactly one sandbox (a **box**) for the lease, and works in it. The hub enforces lifetimes and a hub-wide capacity limit, and it cleans up after crashes and at shutdown.

A lease belongs to a workspace. When that workspace is a task fork (§4), the box is told where the fork's git remote is and can be given a short-lived git credential for it, so an agent can go from "fork" to "working checkout in a clean box" without handling a credential.

The feature is optional. Without a `[sandbox]` section every sandbox REST route answers `404` with `error_type: sandboxes_disabled`, and no gateway listens.

In scope: configuration and start-up checks, profile listing, the lease lifecycle, secret injection, the gateway as clients see it, expiry, retention, recovery, shutdown, events, metrics and the `parley sandbox` commands.

Out of scope: building or pulling images, a CLI command for the admin-wide lease list, more than one box per lease, and running checks in sandboxes (a later candidate, §17).

### 13.2 User stories

**SBX-US-01 (Operator): enable sandboxes.** As an operator, I want to declare a provider and profiles in the configuration, so that agents get sandboxes only of shapes I control.
- Every static error in `[sandbox]`, unknown keys included, is reported in one message and stops the start.
- The provider is probed once at start. A provider that cannot honour a profile stops the start.
- Without the section the hub starts normally and logs that sandboxes are disabled.

**SBX-US-02 (Developer, Agent): discover profiles.** As a developer, I want to list profiles, so that I can choose one by name.
- The list shows secret names and environment key names, never values.

**SBX-US-03 (Agent): get a lease.** As an agent with a `sandboxes:write` token, I want a lease in my workspace or task fork, so that I can create a box with the profile's secrets.
- The answer carries the lease and its token. The token is shown only here.
- Required secrets must exist, and the hub must be under its capacity limit.

**SBX-US-04 (Agent): work on a fork inside a box.** As an agent that created a task fork, I want the box to know the fork's git remote and, if the operator allows it, to hold a credential for it, so that I can clone and push from inside the box without ever holding that credential myself.

**SBX-US-05 (Sandbox client): create and use the box.** As a client holding a lease token, I want to dial `gateway_url` and create my box, so that I can run code in it.
- The image, network, resources and environment come from the lease, never from my request.
- The first create makes the lease `active`. A second create is refused.

**SBX-US-06 (Developer, Agent): end the lease.** As a lease holder, I want to end the lease by `DELETE` or by closing the box, so that the box is destroyed and the token is revoked.

**SBX-US-07 (Operator): nothing leaks or outlives its time.** As an operator, I want unused leases to lapse, boxes to be destroyed at `expires_at`, leases left behind by a crash to be reconciled at start, and all boxes to be destroyed at shutdown.

**SBX-US-08 (Operator): inspect all leases.** As an operator with the admin token, I want to list the leases of every workspace.

**SBX-US-09 (Org member): read only.** As an org member with read-only access to a workspace, I can list and read its leases, but I cannot issue or end one there. I can fork the workspace and lease a sandbox on my fork.

### 13.3 Concepts and data model

#### 13.3.1 Profile

A profile is defined in configuration and is never stored. It is fixed for the life of the process.

| Field | Type | Default | Constraints |
|---|---|---|---|
| `name` | string | required | `^[a-z0-9][a-z0-9_-]{0,62}$`, unique |
| `default` | bool | false | at most one profile may set it |
| `image` | string | required | non-empty; the hub does not pull or check it |
| `cpus` | number | 0 | ≥ 0; fractions allowed |
| `memory_mb` | integer (MiB) | 0 | ≥ 0 |
| `pids` | integer | 0 | ≥ 0 |
| `disk_mb` | integer (MiB) | 0 | ≥ 0; refused at start if the provider cannot limit disk |
| `network` | string | `none` | one of `[sandbox].network_modes` |
| `ttl` | duration | required | > 0 and ≤ `[sandbox].max_ttl` |
| `secrets` | list of strings | `[]` | secret names (§11 naming rules); required at issue and at create |
| `optional_secrets` | list of strings | `[]` | same rules; injected when present |
| `env` | map of strings | `{}` | fixed environment; keys follow secret naming rules; values are private |
| `require_landlock` | bool | false | the box must run under Landlock |
| `inject_git_credential` | bool | false | allow a git credential in boxes leased on a task fork (§13.4, fork integration) |

A zero resource value means "provider default". Names in `secrets`, `optional_secrets` and `env` are compared without regard to case and must be unique across all three. `PARLEY_REMOTE` and `PARLEY_GIT_TOKEN` are reserved and may not appear in any of them.

#### 13.3.2 Lease

| Field | Type | Meaning |
|---|---|---|
| `id` | string | `sbx_` + 32 lowercase hex characters |
| `workspace` | string | workspace slug; may be a task fork |
| `session_id` | string or null | the agent session the lease belongs to (§14) |
| `profile` | string | profile name at issue |
| `purpose` | string | caller text, ≤ 64 characters, no control characters |
| `state` | enum | `leased`, `active`, `destroyed`, `expired`, `failed` |
| `gateway_url` | string | where to dial the gateway; the same for every lease |
| `expires_at` | timestamp | `created_at` + effective TTL; the box and the token end here |
| `lease_expires_at` | timestamp | `created_at` + min(`lease_timeout`, TTL); the deadline for creating the box |
| `image`, `network`, `resources`, `require_landlock` | | snapshot of the profile at issue; `resources` is `{cpus, memory_mb, pids, disk_mb}` |
| `secrets`, `optional_secrets` | list of strings | secret names from the profile, sorted |
| `remote` | string or null | the fork's git URL when the workspace is a task fork |
| `git_credential` | bool | whether a git credential is injected into the box |
| `labels` | map of strings | the caller's labels |
| `created_by` | string | the caller's user id, or `admin` for the admin token |
| `created_at`, `updated_at` | timestamp | |
| `sandbox_id` | string or null | the provider's box id once `active` |
| `error` | string or null | failure message of a `failed` lease |
| `ended_at` | timestamp or null | when the lease reached a terminal state |
| `end_reason` | string or null | `client`, `api`, `expired`, `shutdown`, `restart`, `workspace_deleted`; null for `failed` |

The hub also keeps, and never returns: a hash of the lease token, and the profile's fixed `env` with its values. A lease keeps its snapshot; a later change to the profile does not alter it.

**Lease token.** `sbl_` + 44 base64url characters, from a cryptographic random source. It is returned once, in the `201` of the lease request. The hub stores only a hash, and removes that hash when the lease ends.

**Live** means `leased` or `active`. **Terminal** means `destroyed`, `expired` or `failed`.

#### 13.3.3 State machine

| From | To | Trigger | `end_reason` | Event |
|---|---|---|---|---|
| (new) | `leased` | lease request | n/a | `hub.sandbox.lease` |
| `leased` | `active` | gateway Create succeeded; sets `sandbox_id` | n/a | `hub.sandbox.create` |
| `leased` | `failed` | Create could not make the box (profile gone, required secret missing, provider error); sets `error` | null | `hub.sandbox.failed` |
| `leased` | `expired` | no box by `lease_expires_at` | `expired` | `hub.sandbox.expire` |
| `active` | `expired` | `expires_at` reached; box destroyed first | `expired` | `hub.sandbox.expire` |
| `leased`, `active` | `destroyed` | `DELETE` | `api` | `hub.sandbox.destroy` |
| `active` | `destroyed` | the holder closes the box through the gateway | `client` | `hub.sandbox.destroy` |
| `leased`, `active` | `destroyed` | the workspace is deleted, or the fork expires | `workspace_deleted` | `hub.sandbox.destroy` |
| `active` | `destroyed` | hub shutdown | `shutdown` | `hub.sandbox.destroy` |
| `active` | `destroyed` | start-up recovery | `restart` | `hub.sandbox.destroy` |

Rules:

- Every transition is conditional on the expected current state. When two paths race, exactly one wins and exactly one event is emitted.
- A terminal lease never changes.
- Ending a lease and revoking its token are one atomic act. `leased → active` keeps the token valid.
- At most one box ever exists per lease.

### 13.4 Functional requirements

**Configuration and start-up**

- SBX-FR-001: The feature is on when the configuration has a `[sandbox]` section, and off otherwise. When off, the hub logs once that sandboxes are disabled, runs no sandbox background work and binds no gateway. Stored leases are left as they are.
- SBX-FR-002: An unknown key anywhere under `[sandbox]` is an error that names the key by its full path (for example `sandbox.profiles[1].memroy_mb`).
- SBX-FR-003: `${NAME}` in any string value of the section is replaced by the value of that environment variable, or by an empty string when it is unset. Map keys are not expanded. Replacements are not expanded again.
- SBX-FR-004: Static validation (§13.9) collects every failure and reports them together, one per line. A profile is named by its `name`, or by its index when it has none. No message contains an `env` value or any part of `gateway_token`. The provider is not contacted for a statically invalid section.
- SBX-FR-005: Keys that belong to one provider are refused under the other (§13.9).
- SBX-FR-006: After static validation the hub probes the provider once, within 10 s:
  - `podman`: the hub runs the container engine once and removes leftover boxes whose own expiry has passed.
  - `gateway`: the hub connects to the remote gateway with `gateway_token`, reads its capabilities and lists its boxes. Nothing is created or destroyed. The capabilities are read once and used for the life of the process.

  Then every entry of `network_modes` must be a mode the provider honours, and every profile must be a box the provider can make (image, resources, network, TTL, Landlock). Any failure stops the start with a message that names the key or profile. Messages never contain the gateway token or URL credentials.
- SBX-FR-007: Any sandbox start-up failure is fatal: the hub exits with status 1 before it serves any route.
- SBX-FR-008: The scopes `sandboxes:read` and `sandboxes:write` exist whether or not sandboxes are enabled.
- SBX-FR-009: When enabled, the hub binds the gateway on `[sandbox].listen` (TLS 1.2 or later when a key pair is configured). A key pair that cannot be loaded or an address that cannot be bound is fatal. The gateway accepts no connection until start-up recovery (SBX-FR-070) and the first sweep (SBX-FR-071) have run; failures in those two are logged as warnings and do not stop the start. If the gateway listener later fails, the hub logs the error and exits with status 1.
- SBX-FR-010: `gateway_url` on every lease is `external_url` when set. Otherwise it is `https://` (with TLS) or `http://` followed by the address the listener actually bound. It is computed by the running process, not stored.

**Authorization**

- SBX-FR-020: Check order on every sandbox route: authenticate (`401`) → feature enabled (`404 sandboxes_disabled`) → PAT scope (`403`) → workspace access → validation → state. From the scope check on, the order is the global one (§3). The feature check comes early so that a disabled hub answers every authenticated caller the same way.
- SBX-FR-021: Reads need `sandboxes:read` or `sandboxes:write`. Issuing and ending a lease need `sandboxes:write`. Only PATs are scope-checked.
- SBX-FR-022: Workspace access follows §3: reads need read access; issuing and ending need write access. A caller without read access gets `404 workspace not found`. A read-only member gets `403 forbidden` on a write route.
- SBX-FR-023: `GET /sandbox-profiles` needs no workspace access, and a workspace-bound token may call it. `GET /sandboxes` needs the admin token: any other credential gets `403 forbidden`, and a workspace-bound token gets `403 token_bound`.

**Issuing a lease**

- SBX-FR-030: A lease request on an archived workspace is refused with the archived-workspace error of §4. Reading and ending leases work on archived workspaces.
- SBX-FR-031: The body is an optional JSON object. An empty body counts as `{}`. Accepted fields are `profile`, `purpose`, `ttl`, `labels` and `session_id`; any other field is a `400` that names it. A `null` value counts as absent.
- SBX-FR-032: `profile` defaults to the profile marked `default`. Without a default, an absent `profile` is a `400` that lists the known profile names. A given name must match exactly, else `400` with the same list.
- SBX-FR-033: `ttl` defaults to the profile's TTL. When given, it is a duration string (`30m`, `1h30m`) of at least 1 minute and at most the profile's TTL. The effective TTL is cut to whole seconds.
- SBX-FR-034: `purpose` is at most 64 characters and has no control characters. It defaults to `""`.
- SBX-FR-035: `labels` holds at most 16 entries. A key is 1 to 63 characters of letters, digits, `.`, `_` and `-`, starting and ending with a letter or digit. The keys `lease` and `workspace` (any case) are reserved. A value is a string of at most 256 bytes without control characters; it may be empty. Violations are `400` and name the key.
- SBX-FR-036: `session_id` is optional and is validated like every session reference (§14). When it is absent and the workspace is a task fork that has a `session_id`, the lease takes the fork's.
- SBX-FR-037: Required secrets are checked before the lease is stored. The hub looks up the names in the profile's `secrets` along the resolution chain of §11 for the lease's workspace (for a fork: fork → parent workspace → org → user, with the reserved names of §11 skipped at the parent). The user scope is the workspace owner's, never the caller's. Names match without regard to case. Values are not read. If any name is missing the answer is `422`, `error_type: secret_missing`, with a message that lists the missing names and the scopes searched. Optional secrets are not checked.
- SBX-FR-038: Capacity. A lease counts as live for capacity while it is `leased` and before `lease_expires_at`, or `active` and before `expires_at`. If the hub-wide count has reached `max_sandboxes`, the answer is `429`, `error_type: sandbox_capacity`. The count and the insert are atomic: concurrent requests never exceed the limit.
- SBX-FR-039: On success the hub stores the lease in state `leased` with a full snapshot of the profile and answers `201` with the lease plus `token`. `hub.sandbox.lease` is emitted after the lease is stored. A failure to emit does not undo the lease.
- SBX-FR-040: A replay with the same `Idempotency-Key` (§3) answers `200` with the lease as it is now and **without** `token`, because the hub does not keep the token. A caller that lost the token ends the lease and requests a new one.

**Task fork integration**

- SBX-FR-045: When the lease's workspace is a task fork, the box environment contains `PARLEY_REMOTE`, the fork's git remote URL (§5). The lease shows the same value as `remote`. For any other workspace `remote` is null and the variable is not set.
- SBX-FR-046: When the workspace is a task fork and the profile sets `inject_git_credential = true`, the hub mints a PAT at box creation and places it in the box environment as `PARLEY_GIT_TOKEN`. The PAT:
  - is bound to the fork and acts as the lease's creator (as the fork's owner when the lease was issued with the admin token);
  - carries `git:read`, and `git:write` only if the credential that issued the lease may write to the fork's git;
  - inherits the `paths` limit of the issuing PAT, if it had one (§8);
  - expires at the lease's `expires_at` and is revoked when the lease ends, whichever comes first.

  The PAT is treated as a secret: no REST response, event, log line or gateway answer contains it. The lease shows only `git_credential: true`.
- SBX-FR-047: A lease ends when its workspace is deleted. For a task fork this includes expiry and early deletion after landing (§4). The hub destroys the box, then ends the lease with reason `workspace_deleted`, before the workspace is removed. A box that cannot be destroyed does not block the deletion; the sweep retries it (SBX-FR-071).

**Reading**

- SBX-FR-050: The workspace list returns the workspace's leases, newest first (`created_at`, then `id`, descending). By default it shows only live leases. `all=true` adds ended leases.
- SBX-FR-051: An ended lease stays readable for 24 hours after `ended_at`. After that it is gone: lists omit it and a read answers `404`. The hub removes it from storage at the next sweep.
- SBX-FR-052: A lease id that is malformed, unknown, removed, or that belongs to another workspace answers `404 sandbox lease not found`.
- SBX-FR-053: No response other than the `201` of the lease request contains `token`. No response contains `env` values, secret values or the injected git credential. List and map fields are never null.

**Ending a lease**

- SBX-FR-055: `DELETE` ends a lease and is idempotent:
  - `leased`: the lease becomes `destroyed`. If a Create is in flight, its box is destroyed as soon as it exists and that Create fails.
  - `active`: the hub destroys the box (bounded to 30 s), then the lease becomes `destroyed`. A box that is already gone counts as destroyed.
  - terminal: nothing happens.

  The answer is `204` in all three cases.
- SBX-FR-056: If the provider fails or times out while destroying the box, the answer is `502`, `error_type: sandbox_provider_error`, and the lease is unchanged: still `active`, token still valid. The call can be retried.
- SBX-FR-057: Scrubbing. Any provider error text that is stored, logged, emitted or returned has these replaced by `[redacted]`: every resolved secret value, every fixed `env` value, the injected git credential, and for the `gateway` provider the hub's gateway token, the remote URL and its host. URL user information is removed. The text is cut to 1,024 bytes. If a value cannot be removed with certainty, the whole text becomes `[redacted]`.

**Gateway**

- SBX-FR-060: Authentication. Every RPC must carry one `authorization: Bearer <lease token>` metadata entry. The token is accepted only if it belongs to a lease in state `leased` or `active` whose `expires_at` is in the future. This is checked on every RPC, so a lease that ends is refused from the next call on, including calls relayed to an existing box. Every refusal is gRPC `Unauthenticated` with one fixed message that does not say why.
- SBX-FR-061: Capabilities answers the provider's capabilities, with network modes limited to `network_modes`, the maximum TTL `max_ttl` and the capacity `max_sandboxes`.
- SBX-FR-062: Create builds the box **from the lease**. Image, fixed environment, resources, network mode and the Landlock setting come from the lease snapshot. The box's lifetime is the time left until `expires_at`. Only the working directory and labels come from the caller. Whatever else the caller's spec says is ignored, not rejected.
- SBX-FR-063: Labels on the box are the caller's labels, minus every key in the `hub.` namespace and every key reserved by the gateway protocol, plus each lease label as `hub.<key>`, plus `hub.lease=<lease id>` and `hub.workspace=<slug>`.
- SBX-FR-064: Secret injection. At Create the hub resolves the values of the profile's required and optional secrets along the chain of SBX-FR-037 and adds them to the environment under the profile's spelling of each name. The first scope that has a name wins. Values are read at Create, so a rotation between issue and Create is honoured. Only secrets are injected; variables (§11) are not. On a fork, `PARLEY_REMOTE` and, when applicable, `PARLEY_GIT_TOKEN` are added (SBX-FR-045, SBX-FR-046).
- SBX-FR-065: Create outcomes:

  | Condition | Lease | gRPC status |
  |---|---|---|
  | hub is shutting down | unchanged | `Unavailable` |
  | another Create for the lease is in progress | unchanged | `Aborted` |
  | lease is not `leased` | unchanged | `FailedPrecondition` (names the state) |
  | `lease_expires_at` has passed | `→ expired` | `FailedPrecondition` |
  | profile no longer configured, or its network mode no longer honoured | `→ failed` | `FailedPrecondition` |
  | a required secret is missing | `→ failed` | `FailedPrecondition` |
  | the secret store cannot be read | unchanged; retry allowed | `Internal` |
  | the provider refuses or fails | `→ failed` | `Unavailable` (scrubbed message) |
  | the lease ended while the box was being made | unchanged; new box destroyed | `FailedPrecondition` |
  | success | `→ active` | OK |

  If the hub cannot record `active` after the provider made the box, it destroys the box, the lease stays `leased` and the call answers `Internal`.
- SBX-FR-066: The box returned by Create reports its effective spec with every environment value replaced by `[redacted]`. Keys are kept.
- SBX-FR-067: Isolation. A lease holder sees and reaches only its own box. List returns zero or one box. Destroy of an unknown or foreign id answers OK and does nothing. Calls relayed into a box (tool calls, file sync in and out, git sync) reach only the caller's box; a missing or foreign box id is refused. Messages are limited to 16 MiB. Reap from a lease holder does nothing.
- SBX-FR-068: Client close. When the holder destroys its box through the gateway, the hub destroys the box at the provider and the lease becomes `destroyed` with reason `client`. If the provider fails, the client gets the scrubbed error, the lease stays `active`, and a later `DELETE` or sweep retries.
- SBX-FR-069: If the `gateway` provider's remote refuses the hub's own token after start, the caller sees `Unavailable`, never `Unauthenticated`, and the hub logs an error that names `sandbox.gateway_token`.

**Lifecycle**

- SBX-FR-070: Start-up recovery. A lease found `active` at start was left by a previous process, and this process cannot relay to its box. The hub destroys the box through the provider by `sandbox_id`, then ends the lease with reason `restart`. If the destroy fails the lease is ended all the same, a warning lists the box ids, and the sweep keeps retrying them. `leased` leases are untouched and remain usable after a restart.
- SBX-FR-071: Sweep. A sweep runs once at start and then every `reap_interval`. Sweeps never overlap. A failing step is logged and the later steps still run. Steps, in order:
  1. **Reap**: ask the provider to remove boxes whose own expiry has passed.
  2. **Lapse**: every `leased` lease past `lease_expires_at` becomes `expired`.
  3. **Overdue**: for every `active` lease past `expires_at`, destroy the box, then mark the lease `expired`. The box is always destroyed first. If the destroy fails the lease stays `active` and the next sweep retries. Such a lease is already unusable: its token is refused and it no longer counts against capacity.
  4. **Orphans**: destroy every box that carries a `hub.lease` label naming a terminal or unknown lease. Boxes without that label are left alone.
  5. **Prune**: remove leases that ended more than 24 hours ago.
  6. **Recalibrate**: reset the active-box gauge from the stored leases.
- SBX-FR-072: The hub does not wait for the sweep to enforce `expires_at`. It destroys the box of an `active` lease when `expires_at` arrives and then marks the lease `expired`. The sweep is the backstop for anything this misses.
- SBX-FR-073: Shutdown. When the hub stops, within a total of 20 s:
  1. New Creates are refused and no further sweep starts. A running sweep is allowed to finish.
  2. For each `active` lease, in parallel, the hub destroys the box and ends the lease with reason `shutdown`.
  3. The gateway listener stops.

  `leased` leases are not changed. If the time runs out, the leases still `active` are logged and are ended by recovery at the next start.
- SBX-FR-074: A crash runs none of SBX-FR-073. The next start applies SBX-FR-070.
- SBX-FR-075: Every provider call made while ending a lease is bounded to 30 s. A timeout counts as a provider failure.

### 13.5 REST API

All routes follow the conventions of §3. The feature check of SBX-FR-020 applies to every route below and is not repeated in the error lists.

**Lease object.** Every lease response has all the fields of §13.3.2. Example:

```json
{"id":"sbx_3f2a9c0e7b1d4c5a8e6f0b2d1c3a4e5f","workspace":"widgets--a1b2c3d4","session_id":"ses_01HZX","profile":"tools",
 "purpose":"triage","state":"leased","gateway_url":"https://sandboxes.example.com",
 "expires_at":"2026-01-02T04:04:05Z","lease_expires_at":"2026-01-02T03:09:05Z",
 "image":"registry.example.com/tools:1","network":"none","resources":{"cpus":1,"memory_mb":512,"pids":100,"disk_mb":0},
 "require_landlock":true,"secrets":["GIT_PAT"],"optional_secrets":[],
 "remote":"https://hub.example.com/git/widgets--a1b2c3d4.git","git_credential":true,
 "labels":{"team":"red"},"created_by":"user-1","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z",
 "sandbox_id":null,"error":null,"ended_at":null,"end_reason":null}
```

#### `GET /sandbox-profiles`

- **Purpose:** List the hub's profiles.
- **Auth:** any credential; a PAT needs `sandboxes:read`. No workspace access is needed. Workspace-bound tokens are allowed.
- **Behaviour:** The list comes from configuration only. It keeps answering when the provider is down. Every caller gets the same list.
- **Response:** `200`, a list ordered by `name`:
  ```json
  {"items":[{"name":"agent","default":true,"image":"registry.example.com/agent:1.4","network":"none","ttl_seconds":1200,
    "resources":{"cpus":2,"memory_mb":4096,"pids":512,"disk_mb":0},
    "secrets":["ANTHROPIC_API_KEY","GIT_PAT"],"optional_secrets":["GH_TOKEN"],"env_keys":["LANG","TZ"],
    "require_landlock":true,"inject_git_credential":true}],"next_cursor":null}
  ```
  `secrets`, `optional_secrets` and `env_keys` are sorted and are always arrays. Nothing about the provider or the hub's limits is included.
- **Errors:** `401`; `404 sandboxes_disabled`; `403` scope.

#### `POST /workspaces/:slug/sandboxes`

- **Purpose:** Issue a lease.
- **Auth:** `sandboxes:write`; workspace write access.
- **Request body:** optional JSON object.

  | Field | Type | Default | Validation |
  |---|---|---|---|
  | `profile` | string | the default profile | exact name (SBX-FR-032) |
  | `purpose` | string | `""` | ≤ 64 characters, no control characters |
  | `ttl` | duration string | the profile's TTL | 1 m ≤ `ttl` ≤ profile TTL |
  | `labels` | object of strings | `{}` | SBX-FR-035 |
  | `session_id` | string | the fork's session, else null | SBX-FR-036 |

- **Behaviour:** SBX-FR-030 to SBX-FR-046. Accepts `Idempotency-Key` (SBX-FR-040).
- **Response:** `201` with the lease object plus `"token":"sbl_…"`.
- **Errors:** `401`; `403` scope; `404 workspace not found`; `403 forbidden` (read-only member); archived workspace (§4); `413`; `400` body and field errors; `422 secret_missing`; `429 sandbox_capacity`.
- **Side effects:** stores the lease; emits `hub.sandbox.lease`.

#### `GET /workspaces/:slug/sandboxes`

- **Purpose:** List the workspace's leases.
- **Auth:** `sandboxes:read`; workspace read access.
- **Query:** `all` (`true` or `false`, default `false`; anything else is `400`), `limit`, `cursor`.
- **Response:** `200`, a list of lease objects, newest first.
- **Errors:** `401`; `403` scope; `404 workspace not found`; `400`.

#### `GET /workspaces/:slug/sandboxes/:id`

- **Purpose:** Read one lease in any state.
- **Auth:** as for the list.
- **Response:** `200` with the lease object. No `token`.
- **Errors:** `401`; `403` scope; `404 workspace not found`; `404 sandbox lease not found`.

#### `DELETE /workspaces/:slug/sandboxes/:id`

- **Purpose:** End a lease, destroy its box and revoke its token.
- **Auth:** `sandboxes:write`; workspace write access.
- **Behaviour:** SBX-FR-055 and SBX-FR-056.
- **Response:** `204`, no body, whether or not anything changed.
- **Errors:** `401`; `403` scope; `404 workspace not found`; `403 forbidden` (read-only member); `404 sandbox lease not found`; `502 sandbox_provider_error` (lease unchanged).
- **Side effects:** one `hub.sandbox.destroy` per actual change, with the caller as actor.

#### `GET /sandboxes`

- **Purpose:** List the leases of every workspace.
- **Auth:** admin token only.
- **Query:** `all`, `limit`, `cursor`, as for the workspace list.
- **Response:** `200`, a list of lease objects, newest first. Leases that ended in the last 24 hours remain listed with `all=true` even when their workspace has since been deleted.
- **Errors:** `401`; `403 forbidden`; `403 token_bound`; `400`.

#### Gateway protocol surface (gRPC, not REST)

The gateway serves the AgentKit-compatible gateway protocol, version 1.0, on `[sandbox].listen`, in plaintext on loopback or over TLS.

| Call | Behaviour on this hub |
|---|---|
| Capabilities | SBX-FR-061 |
| Create | makes the lease's one box (SBX-FR-062 to SBX-FR-066); answers the box id, the redacted spec, and creation and expiry times |
| List | the caller's zero or one box |
| Destroy | closes the caller's box and ends the lease (SBX-FR-068); OK for unknown or foreign ids |
| SyncGit | relayed to the caller's box |
| Reap | no effect for lease holders |
| Agent calls (hello, list tools, call a tool, tar in, tar out, rsync) | relayed to the caller's box |

The only credential is the lease token, sent as a bearer token on every call (SBX-FR-060). There are no static gateway tokens.

A client dials `gateway_url` with the token and calls Create. Any spec it sends is advisory except for the working directory and labels. Inside a box leased on a fork, `git clone "$PARLEY_REMOTE"` works without further setup when the lease has `git_credential: true` and the image configures git to use `PARLEY_GIT_TOKEN`.

### 13.6 CLI

All commands live under `parley sandbox` and follow the CLI conventions of §3.

#### `parley sandbox profiles`

- **Purpose:** List the hub's sandbox profiles.
- **Calls:** `GET /sandbox-profiles`.
- **Output:** the response JSON.

#### `parley sandbox create <workspace-slug>`

- **Purpose:** Issue a sandbox lease in a workspace or task fork.
- **Flags:**

  | Flag | Type | Description |
  |---|---|---|
  | `--profile` | string | Profile name (default: the hub's default profile) |
  | `--purpose` | string | What the sandbox is for (at most 64 characters) |
  | `--ttl` | string | Lifetime of the lease, such as `30m` (default: the profile's) |
  | `--label` | `key=value`, repeatable | Label for the lease |
  | `--session` | string | Session id to link the lease to |
  | `--token-file` | path | Write the lease token to this file (mode 0600) instead of printing it |

- **Client checks (exit 2, before any request):** a label without `=`; a label key given twice; a token file that cannot be opened for writing.
- **Calls:** `POST /workspaces/<slug>/sandboxes`. Only flags that were given are sent.
- **Output:**
  - Without `--token-file`: a warning on stderr that the token will not be shown again, then the lease including `token` on stdout.
  - With `--token-file`: the token is written to the file with no trailing newline, and stdout shows the lease without `token`.
  - If the token cannot be written, the CLI ends the lease it just created and exits 1. The message says whether the lease was released and, if it was not, gives the `parley sandbox destroy` command to run. The token is never printed as a fallback.
- **Exit codes:** 0; 1 (hub error or token write failure); 2 (usage).

#### `parley sandbox list <workspace-slug>`

- **Purpose:** List the sandbox leases of a workspace.
- **Flags:** `--all` (include leases that have ended), `--limit`, `--cursor`.
- **Calls:** `GET /workspaces/<slug>/sandboxes`.
- **Output:** the response JSON.

#### `parley sandbox get <workspace-slug> <lease-id>`

- **Purpose:** Show one sandbox lease.
- **Calls:** `GET /workspaces/<slug>/sandboxes/<id>`.
- **Output:** the lease JSON, without a token.

#### `parley sandbox destroy <workspace-slug> <lease-id>`

- **Purpose:** End a sandbox lease and revoke its token.
- **Calls:** `DELETE /workspaces/<slug>/sandboxes/<id>`.
- **Output:** `{"id":"<id>","status":"destroyed"}`. The output is the same for a lease that had already ended.
- **Exit codes:** 0; 1 (for example `404`, or `502 sandbox_provider_error`, after which the command can be run again); 2.

### 13.7 Background behaviour

| Activity | When | Effect |
|---|---|---|
| Provider probe | start, if enabled | SBX-FR-006; fatal on failure |
| Gateway bind | start, if enabled | SBX-FR-009; fatal on failure |
| Recovery | start, before the gateway serves | SBX-FR-070 |
| First sweep | start, before the gateway serves | SBX-FR-071 |
| Periodic sweep | every `reap_interval` (default 10 m) | SBX-FR-071 |
| Box expiry | at each active lease's `expires_at` | box destroyed, then lease `expired` (SBX-FR-072) |
| Workspace deletion, fork expiry | when it happens | live leases of that workspace are ended (SBX-FR-047) |
| Shutdown | when the hub stops; 20 s in total | SBX-FR-073 |

**Metrics** (on `/metrics`, exported from start whenever sandboxes are enabled):

| Metric | Type | Meaning |
|---|---|---|
| `parley_sandboxes_active{workspace}` | gauge | leases in state `active`; recalibrated from stored leases at every sweep and after recovery |
| `parley_sandbox_creates_total{result}` | counter | gateway Creates that reached the provisioning step; `result` is `created` or `failed` |
| `parley_sandbox_lease_seconds` | histogram | `ended_at − created_at` of every lease that had a box, observed when it ends; buckets 60, 300, 600, 1200, 1800, 3600, 7200, 14400, 28800 |

### 13.8 Events

All sandbox events have `resource_type: "sandbox"`, `resource_id` = the lease id, `workspace` = the lease's workspace, and `session_id` = the lease's `session_id` (null when the lease has none). Metadata never contains a secret value, an `env` value, the lease token or the injected git credential. A failure to emit is logged and never changes the outcome. The catalogue is in §14.

| Type | Actor | When | Metadata |
|---|---|---|---|
| `hub.sandbox.lease` | the caller | lease stored | `profile`, `purpose`, `ttl_seconds`, `secret_names`, `optional_secret_names`, `git_credential` |
| `hub.sandbox.create` | `lease` / the lease's `created_by` | `leased → active` | `sandbox_id`, `image`, `network` |
| `hub.sandbox.failed` | `lease` / `created_by` | `leased → failed` | `error` (scrubbed) |
| `hub.sandbox.destroy` | `DELETE`: the caller. Client close: `lease` / `created_by`. Otherwise `system` | every change into `destroyed` | `sandbox_id` (string or null), `reason` (`api`, `client`, `shutdown`, `restart`, `workspace_deleted`) |
| `hub.sandbox.expire` | `system` | every change into `expired` | `sandbox_id` (string or null; non-null when a box was destroyed) |

An expiry emits no `hub.sandbox.destroy`. A `DELETE` that changes nothing emits nothing.

### 13.9 Configuration

The `[sandbox]` table. Durations are strings such as `30m` or `4h`.

| Key | Type | Default | Rules |
|---|---|---|---|
| `provider` | string | required | `podman` (local container engine) or `gateway` (a remote sandbox gateway) |
| `engine_binary` | string | `podman` | `podman` provider only: the engine executable, a path or a name on `PATH` |
| `runtime` | string | `""` | `podman` only: an alternative OCI runtime, for example a micro-VM runtime |
| `userns` | string | `""` | `podman` only: user-namespace mode for boxes |
| `gateway_url` | URL | required under `gateway` | `gateway` only: `https://host[:port]`, or `http` to a loopback host only; no user information, query, fragment or path |
| `gateway_token` | string | required under `gateway` | `gateway` only: at least 16 printable ASCII characters; never echoed in any message or log |
| `max_ttl` | duration | `4h` | > 0; the ceiling for every profile TTL |
| `network_modes` | list of strings | `["none"]` | non-empty, unique entries; each must be honoured by the provider (`podman` honours `none` and `unrestricted`) |
| `max_sandboxes` | integer | `16` | ≥ 1; hub-wide limit on live leases |
| `listen` | `host:port` | `127.0.0.1:8443` | the gateway listener; a non-loopback address requires `tls_cert`, `tls_key` and `external_url` |
| `tls_cert`, `tls_key` | path | `""` | PEM files; both or neither |
| `external_url` | URL | `""` | absolute `http` or `https` URL with a host; no user information, query, fragment or path |
| `lease_timeout` | duration | `5m` | ≥ 1 s and ≤ `max_ttl`; how long a lease may wait for its box |
| `reap_interval` | duration | `10m` | 10 s to 1 h; the sweep interval |
| `[[sandbox.profiles]]` | list of tables | at least one | fields per §13.3.1 |

Further static rules:

- A `podman`-only key under `gateway`, or a `gateway`-only key under `podman`, is an error.
- At most one profile sets `default = true`.
- Each profile's `network` is one of `network_modes`, and its `ttl` is at most `max_ttl`.
- A loopback address is `localhost` or a literal loopback IP. An empty host, `0.0.0.0` and `::` are not loopback.

No environment variable configures the feature directly. `${NAME}` expansion (SBX-FR-003) is how secrets such as `gateway_token` are supplied.

The hub does not build or pull images. A profile image must contain a sandbox agent that speaks the AgentKit-compatible protocol, and, to use `PARLEY_GIT_TOKEN`, a git credential setup that reads it.

### 13.10 Edge cases and error handling

- **Feature disabled.** Every sandbox route answers `404 sandboxes_disabled` to any authenticated caller, before scope checks. `401` still comes first.
- **Lapsed lease before the sweep.** A `leased` lease past `lease_expires_at` stops counting against capacity at once. Its token still authenticates until the sweep marks the lease, but its Create is refused and marks it `expired`.
- **Overdue lease before its box is gone.** An `active` lease past `expires_at` has its token refused at once and stops counting against capacity. It stays `active` only until its box is destroyed.
- **Restart with `leased` leases.** They remain usable; the token is still valid.
- **Profile changed or removed after issue.** A lease keeps its snapshot. If the profile's name is no longer configured at Create, the lease fails.
- **Secret deleted or rotated between issue and Create.** Deleted: the lease fails with a message that names the secret. Rotated: the new value is injected.
- **`DELETE` racing a Create.** Whichever transition wins decides. A box made for an ended lease is destroyed at once, no `hub.sandbox.create` is emitted, and the Create fails.
- **`DELETE` racing a client close or a sweep.** The lease ends once, with one event and one reason: that of the path that won.
- **Provider cannot destroy a box.** The lease stays `active` (`DELETE` answers `502`; client close answers an error). The next `DELETE` or sweep retries. Shutdown and recovery are the exceptions: there the lease is ended and the orphan step of the sweep keeps retrying the box.
- **Remote gateway unreachable (`gateway` provider).** Profiles are still listed and leases are still issued. Create fails the lease. `DELETE` of an active lease answers `502`. Sweeps log their failures. The hub does not retry a Create. Connection errors are reported without the remote's address.
- **Fork expires while a box is running.** The box is destroyed and the lease ends with reason `workspace_deleted`. Work that was not pushed is lost; extending the fork's TTL (§4) before it expires avoids this. A lease's TTL is not shortened to fit the fork's `expires_at`.
- **Lease on a fork without `inject_git_credential`.** `PARLEY_REMOTE` is still set. The box needs its own way to authenticate, for example a profile secret.

---

## 14. Sessions, audit, events and metrics

### 14.1 Purpose and scope

This section covers how Parley records what happened, why, and by whom.

- **Agent sessions.** An agent opens a session for a workspace, reports token usage while it works, and closes it with an outcome. Forks, pushes, API commits, change versions, claims and lands can name the session, so the session record is the thread that ties one piece of agent work together.
- **Run telemetry.** Agents push records about their runs: run events, session outcomes, tool calls, tool errors, conversation traces and a postmortem. The hub stores them and serves them back per run, including a reconstructed transcript.
- **Hub events.** Every significant state change in the hub produces one event. Events are durable: each gets a hub-wide sequence number, is kept in an event log, and can be consumed live or replayed after a disconnect without loss.
- **Audit query.** One query spans hub events and agent run events.
- **Retention.** A background task keeps all of the above bounded.
- **Metrics.** A Prometheus endpoint exposes operational metrics.

Out of scope: pricing. "Cost" in this section always means token counts. There is no price table and no model catalogue. Outbound webhooks are also out of scope: consumers pull events from the stream or the log.

### 14.2 User stories

**AUD-US-01: Agent reports a session.** As an agent, I want to open a session for a workspace, report token usage increments and close it with an outcome, so that the operator can attribute token spend.
- `POST /sessions` creates an `active` session (201). Repeating the same `id` returns the stored session (200).
- `POST /sessions/:id/usage` adds one usage record while the session is `active` (201). A replay answers 200 and changes nothing.
- `POST /sessions/:id/complete` ends the session once. Repeating it answers 200 with the unchanged record.

**AUD-US-02: Session as the thread.** As a developer, I want to open one session record and see the forks it created, the changes it produced and how many commits, claims and lands it made, so that I can follow one piece of agent work from start to finish.

**AUD-US-03: Agent pushes run telemetry.** As an agent, I want to push run events, outcomes, tool calls, tool errors, traces and a postmortem under a run id, so that the run can be audited later.
- Each record is idempotent on its `id`.
- Batches of up to 1000 items are accepted item by item with a per-item error list.

**AUD-US-04: Developer inspects a run.** As a developer with read access, I want to list a run's records with filters and cursors, read its postmortem and a transcript per node, so that I can debug what an agent did.

**AUD-US-05: Operator audits the hub.** As an operator, I want one query over hub and agent events across all workspaces, newest first, so that I can reconstruct who did what.

**AUD-US-06: Durable live feed.** As an integrator, I want to subscribe to hub events, and after a disconnect or a restart resume from the last event I saw, so that my automation never misses an event and never has to rescan state.

**AUD-US-07: Polling consumer.** As an integrator that cannot hold a stream open, I want to page through the same event log as JSON with a long-poll, so that a simple loop gets the same guarantees.

**AUD-US-08: Cost report.** As an org member, I want token usage of a workspace summed per day, session or model over a window.

**AUD-US-09: Retention.** As an operator, I want old data deleted on a schedule, stale active sessions timed out, and data of deleted workspaces removed after a grace period.

**AUD-US-10: Monitoring.** As an operator, I want `GET /metrics` in Prometheus text format, so that I can alert on the hub.

### 14.3 Concepts and data model

#### 14.3.1 Identifiers and time

- Session ids, usage ids, telemetry record ids and hub event ids are UUIDs. A client-supplied id MUST be a UUID in the 8-4-4-4-12 hexadecimal form. Any letter case is accepted; ids are stored and returned in lower case. When the client sends none, the hub generates a version 4 UUID.
- A **run id** identifies one agent run. Its format is `YYYYMMDD_HHMMSS_xxxxxx`, where `xxxxxx` is six lower-case hexadecimal digits.
- `node_id`, `archetype`, `model` and `tool_name` are free-form labels of at most 255 characters.
- Timestamps generated by the hub follow §3 (RFC 3339 UTC, whole seconds). Timestamps supplied by clients in telemetry MUST be RFC 3339 with any offset. They are normalised to UTC and keep fractional seconds up to microseconds, because they order records inside a run.
- JSON-valued fields are stored and returned as JSON, never as strings holding JSON.

#### 14.3.2 Session

| Field | Type | Meaning |
|---|---|---|
| `id` | UUID | Client-chosen or generated |
| `workspace_slug` | string | The workspace the session works in. May be a task fork |
| `run_id`, `node_id`, `archetype`, `model` | string or null | Labels set at creation |
| `status` | enum | `active`, `completed`, `failed`, `timeout`, `terminated` |
| `user_id` | string or null | The user who created the session. Null for an admin token |
| `credential_type` | enum | `admin_token`, `api_key`, `pat` |
| `started_at` | timestamp | Set at creation |
| `ended_at` | timestamp or null | Set on any terminal transition |
| `duration_ms` | integer or null | From `complete`. When the client sends none, the hub computes it from `started_at` and `ended_at` |
| `error_message` | string or null | From `complete`, or the reason of a forced close |
| `cache_creation_input_tokens` | integer | From `complete`. Default 0 |
| `metadata` | any JSON or null | As sent at creation |
| `token_summary` | object | `total_input_tokens`, `total_output_tokens`, `total_cache_read_tokens`, `models_used` (distinct model names, sorted ascending) |
| `forks` | list | Slugs of the task forks created with this session (§4), oldest first |
| `changes` | list | `{workspace, number}` of each change that has at least one version attributed to this session (§7), ordered by first attribution |
| `counts` | object | `forks`, `changes`, `versions`, `pushes`, `commits`, `claims`, `lands` |

Every field is present in every session response. `forks` and `changes` hold at most 500 entries each; `counts` always holds the true totals. `counts.pushes` counts ref updates by git push, `counts.commits` counts commits created through the source control API (§6), `counts.versions` counts change versions, `counts.lands` counts successful lands.

**State machine.**

```
            POST /sessions            POST /sessions/:id/complete
  (none) ─────────────────▶ active ─────────────────────────────▶ completed | failed | timeout
                              │  workspace archived or deleted, fork expired ─▶ terminated
                              └─ active longer than [sessions].max_active_age ─▶ timeout
```

The four terminal states are absorbing. `terminated` cannot be set by a client. Usage may be reported only while the session is `active`.

**Linking work to a session.** A fork request, a git push (`-o parley.session=<id>`), an API commit, a change version, a claim and a land may name a session. The reference is accepted when:

- the session exists,
- it was created by the caller's user, or the caller is an admin token, and
- the session's workspace and the workspace of the operation are the same workspace, or one is a task fork of the other.

The session's status is not checked: work may be attributed after the session has ended, for example a land that happens later. A reference that fails these rules is refused with `400`, `error_type: unknown_session`, and nothing is changed. On a git push the refusal is reported in git's text form (§5).

Every hub event caused by an operation that names a session carries that `session_id`. An event about a change that is caused by the hub itself (restack, conflict detection, automatic land) carries the session of the change version it concerns.

#### 14.3.3 Token usage record

`id` (UUID), `session_id`, `workspace_slug` (copied from the session), `model` (required), `input_tokens`, `output_tokens`, `cache_read_tokens` (integers ≥ 0, default 0), `reported_at` (hub time).

#### 14.3.4 Run telemetry records

All telemetry records carry `id`, `workspace`, `run_id`, `timestamp` (client time, or hub time when absent) and `ingested_at` (hub time). `session_id`, where present, MUST be a UUID; it is not required to name an existing session.

| Record | Fields beyond the common ones |
|---|---|
| Run event | `event_type` (required, free-form), `severity` (`info`, `warning`, `error`, `critical`), `node_id`, `session_id`, `payload` (JSON object) |
| Session outcome | `session_id` (required), `status` (required, free-form), `node_id`, `duration_ms` (integer ≥ 0), `token_usage` (any JSON) |
| Tool call | `tool_name` (required), `node_id`, `session_id`, `duration_ms` (integer ≥ 0), `input`, `output` (any JSON) |
| Tool error | `tool_name` (required), `error_msg` (required), `error_code`, `node_id`, `session_id` |
| Trace | `event_type` (required; one of `session.init`, `assistant.message`, `tool.use`, `tool.error`, `session.result`), `node_id`, `session_id`, `sequence` (integer ≥ 0), `data` (any JSON) |
| Postmortem | One per workspace and run. `schema_version` (only `1`), `run_status` (`stalled`, `block_limit`, `cost_limit`, `session_limit`), `started_at`, `completed_at`, `task_summary` (JSON object with key `total`), `cost_summary` (JSON object), `blocked_tasks`, `session_history` (JSON arrays, default `[]`) |

Default severity of a run event when none is sent: `session.fail` is `error`; `run.limit_reached`, `git.conflict`, `harvest.empty` and `review.parse_failure` are `warning`; every other type is `info`.

#### 14.3.5 Hub event

A hub event reports one state change in the hub. Every event has the same shape:

| Field | Type | Meaning |
|---|---|---|
| `seq` | integer | Hub-wide sequence number. Strictly increasing, never reused, never reset |
| `id` | UUID | Stable identity of the event. Use it to drop duplicates |
| `type` | string | For example `hub.change.land`. Every type starts with `hub.` |
| `time` | timestamp | When the event was written |
| `workspace` | string or null | Slug of the workspace the event belongs to. Null for an event that is not workspace-scoped |
| `actor_type` | enum | `admin_token`, `api_key`, `pat`, `lease`, `forge`, `system` |
| `actor_id` | string or null | User id of the actor. Null for `admin_token`, `forge` and `system` |
| `resource_type` | string | Kind of the resource the event is about. Never empty |
| `resource_id` | string | Identity of that resource inside its workspace. Never empty |
| `action` | string | The last dot-separated segment of `type` (`hub.change.land` has action `land`). Never empty |
| `session_id` | UUID or null | The session the change is attributed to (§14.3.2) |
| `metadata` | object | Type-specific keys (§14.8). `{}` when none |

Rules that hold for every event:

- Metadata never holds a credential, a secret value, a token, or free text taken from a forge message. URLs carry no userinfo.
- A list of paths in metadata holds at most 100 entries. When it was cut, the metadata also has `truncated: true`.
- Unless §14.8 says otherwise, the actor is the caller of the request that caused the event. Work done by the hub on its own (expiry, restack, automatic land, retention) has actor `system`.

#### 14.3.6 Event log, audit log and delivery guarantees

Hub events are stored once and served through two windows:

- The **event log** is the part addressable by `seq`: events newer than `[events].retention` (default `30d`). `GET /events` and `GET /events/log` read it.
- The **audit log** is every hub event newer than `[audit].max_age` (default `90d`). `GET /audit` reads it.

Guarantees:

- **Durable.** An event is written in the same transaction as the state change it reports, wherever that state lives in the hub's own store. Either both are committed or neither is. For a state change that lives in a git repository (a ref update), the hub MUST still write the event: when the hub stops between the ref update and the event, the missing event is written at the next start, before the hub serves requests.
- **Ordered.** Events are delivered in `seq` order. An event never becomes visible after an event with a higher `seq` has been delivered to any consumer.
- **At-least-once.** A consumer that resumes from the last `seq` it processed receives every visible event after it. It may receive an event twice across a reconnect; `seq` and `id` identify duplicates. Within one connection or one page no event repeats.
- **Gaps are normal.** A consumer sees only the events it may see and asked for, so the `seq` values it receives are not contiguous. Consumers MUST NOT treat a gap as loss.

#### 14.3.7 Visibility

One rule decides who may see a hub event, on the stream, the log and the audit query:

| Event | Visible to |
|---|---|
| `workspace` is set | Callers with read access to that workspace. For a task fork, also callers with read access to its parent; this still holds after the fork is deleted |
| `workspace` is null, `metadata.scope` is `user` | The user named by `actor_id` |
| `workspace` is null, `metadata.scope` is `org` | Members of the org named in `metadata.org` |
| Any event | Admin tokens |

A token bound to a workspace (§3) sees only events of that workspace and of the forks the token is valid on (§4). It never sees an event without a workspace.

Access is evaluated when an event is delivered, not when the stream was opened. A workspace or fork created later is included from its first event; a caller who loses access stops receiving events of that workspace.

Run telemetry is never part of the event log. It is visible to callers with read access to its workspace.

#### 14.3.8 Scopes

`sessions:read`, `sessions:write`, `audit:read`, `audit:write`. The rules of §3 apply: a read check accepts either scope of the pair, and only PATs are scope-checked. `GET /events`, `GET /events/log` and `GET /audit` need `audit:read`.

Bound tokens may call the routes of this section that are not under `/workspaces/:slug` (`/sessions…`, `/audit`, `/events`, `/events/log`). Their results are confined as §14.3.7 describes.

### 14.4 Functional requirements

**Sessions**

- AUD-FR-001: `POST /sessions` needs `sessions:write` and write access to the workspace named by `workspace_slug`. The workspace may be a task fork. An archived workspace refuses new sessions with `409 workspace_archived`.
- AUD-FR-002: A new session has `status: active`, `started_at` = now, and records the caller's user id and credential type.
- AUD-FR-003: Session creation is idempotent on `id`. A replay by the same user for the same workspace answers 200 with the stored session and changes nothing. An `id` that exists for another user or workspace answers `409 conflict`.
- AUD-FR-004: Only the user who created a session, or an admin token, may complete it or report usage. Any credential of that user qualifies. A caller who can read the session but did not create it gets `403 forbidden`.
- AUD-FR-005: `complete` accepts `completed`, `failed` or `timeout`. Completing an `active` session sets the status, `ended_at`, `error_message`, `duration_ms` and `cache_creation_input_tokens` in one atomic step. Completing a terminal session answers 200 with the unchanged record; when two completes race, exactly one takes effect and both answer 200.
- AUD-FR-006: A usage report needs a non-blank `model` and token counts ≥ 0, and an `active` session (else `409 session_not_active`). A new record answers 201. A replay of an existing usage `id` in the same session answers 200 with the stored record and changes nothing: no record, no total, no metric. A usage `id` that exists in another session answers `409 conflict`.
- AUD-FR-007: A session is readable by its creator, by any caller with read access to its workspace, and by admin tokens. A session on a task fork is also readable by callers with read access to the parent, also after the fork is gone. Workspace status does not affect reads. A caller who may not read a session gets `404 session not found`.
- AUD-FR-008: Every session response carries `token_summary`, `forks`, `changes` and `counts`, computed at read time.
- AUD-FR-009: Archiving or deleting a workspace, and the expiry of a task fork, closes all its `active` sessions: `status: terminated`, `ended_at` = the time of the operation, `error_message` = `workspace archived`, `workspace deleted` or `fork expired`. One `hub.session.force_closed` event is written per session, in the same transaction as the workspace change.
- AUD-FR-010: The cost report sums the usage records of one workspace with `since ≤ reported_at < until`. Totals and each group give the three token sums and the number of distinct sessions. `day` groups are UTC calendar dates. No prices are applied.

**Ingestion**

- AUD-FR-020: Every ingestion route needs `audit:write` and write access to the workspace, and follows the check order of §3. Ingestion into a workspace that does not exist is refused with `404 workspace not found` for every caller, admin tokens included. An archived workspace refuses ingestion with `409 workspace_archived`.
- AUD-FR-021: The `run_id` in the path MUST match the run id format, on every per-run route including the transcript. A `run_id` in a body, when present, MUST equal the one in the path.
- AUD-FR-022: Every field is validated as §14.3.4 documents: required fields, enums, UUIDs, RFC 3339 timestamps, non-negative integers, and the JSON type of `payload`, `task_summary`, `cost_summary`, `blocked_tasks` and `session_history`. A JSON `null` counts as absent. A violation answers `400 invalid_request` with a message that names the field.
- AUD-FR-023: Each record is idempotent on its `id` (a postmortem: on workspace and run). A new record answers 201. A replay answers 200 with the **stored** record and changes nothing. An `id` that already exists in another workspace or run answers `409 conflict`.
- AUD-FR-024: Batch routes take a JSON array of 1 to 1000 items. Each item is validated like a single record. Invalid items are reported in `errors` and skipped. Valid items are stored together or not at all. The response is always 200 with `{accepted, duplicates, errors}`.
- AUD-FR-025: An ingestion write that does not finish within `[audit].write_timeout` answers `503 unavailable` with `Retry-After: 5`, and stores nothing.

**Reads**

- AUD-FR-030: Per-run reads need `audit:read` and read access to the workspace, whatever its status.
- AUD-FR-031: Per-run lists are ordered by (`timestamp`, `id`), ascending by default; `order=desc` reverses. `since` is inclusive and `until` exclusive. Results are always confined to the workspace and run of the path.
- AUD-FR-032: The audit query merges hub events and run events, newest first (by time, then `id`, both descending). `since` is inclusive, `until` exclusive, and `since` must be before `until`.
- AUD-FR-033: The audit query returns hub events under the visibility rule of §14.3.7 and run events of workspaces the caller can read. A `workspace` filter that names a workspace the caller cannot read answers `404 workspace not found`.
- AUD-FR-034: Every query parameter is validated. An unknown enum value, a malformed timestamp, UUID or cursor answers `400 invalid_request`; nothing is silently ignored or turned into an empty result.

**Durable events**

- AUD-FR-040: Every state change listed in §14.8 writes exactly one event per row of the catalogue, with a new `seq`, with the guarantees of §14.3.6. A request that fails or is refused writes no event, unless the catalogue says the event reports the failure.
- AUD-FR-041: `seq` starts at 1 and survives restarts. The hub MUST NOT hand out a `seq` lower than one it has already delivered.
- AUD-FR-042: The stream and the log accept the same filters. `workspace`: one slug, exact match. `types`: a comma-separated list of at most 20 entries; an entry is an exact type or a prefix ending in `*` (`hub.change.*`; `*` alone matches everything); `*` is allowed only as the last character. `session_id`: one UUID. Filters combine with AND; the entries of `types` combine with OR. A type that no event has is not an error.
- AUD-FR-043: A resume position (`after`, or `Last-Event-ID`) is a `seq`: the consumer receives events with a higher `seq`. A position older than the event log answers `410`, `error_type: cursor_expired`, with `oldest_seq` in the error object. A position ahead of the newest `seq` answers `400 invalid_request`. A position is expired when at least one event after it has left the event log, whether or not the caller could have seen that event.
- AUD-FR-044: `GET /events` first replays every visible, matching event after the resume position, then continues with live events on the same connection. There is no gap and no reordering between the two phases. Without a resume position the stream starts with live events only.
- AUD-FR-045: The stream sends a heartbeat when it opens and then every 30 s. A heartbeat carries the `seq` up to which the stream has examined the log for this client. Because it is sent as the SSE event id, a client whose filters match rarely still advances its resume position and does not run into `cursor_expired`.
- AUD-FR-046: Each stream has a buffer of 256 undelivered events. When a client reads too slowly and the buffer is full, or when the client accepts no data for 60 s, the hub closes the stream. It never drops an event from an open stream. The client reconnects with its last `seq` and receives everything it missed.
- AUD-FR-047: At most `[events].max_sse_connections` streams are open at once. A further request answers `503 unavailable` in the error envelope, with `Retry-After: 5`. Long-polls on `GET /events/log` do not count.
- AUD-FR-048: The hub closes a stream when its credential is revoked or expires, and when the hub shuts down. Shutdown closes streams promptly and does not wait for them.
- AUD-FR-049: `GET /events/log` serves the same events as JSON pages in ascending `seq`. With `wait`, a request that would return no event is held until a matching event is written or the wait ends.

**Retention and metrics**

- AUD-FR-060: Retention runs once at start and then every hour, as §14.7.2 describes.
- AUD-FR-061: `GET /metrics` exports every metric of §14.7.3. Each one has at least one series from start-up, so a documented metric is never absent from the output.

### 14.5 REST API

Conventions of §3 apply to every route: authentication, check order, error envelope, list envelope (`items`, `next_cursor`, `limit` default 50 and maximum 200, `cursor`), and `Idempotency-Key` on creating `POST`s. Only errors specific to a route are listed. "Session object" means the shape of §14.3.2.

#### `POST /sessions`
- **Purpose:** open an agent session.
- **Scope / access:** `sessions:write`; write access to the workspace.
- **Body:** `workspace_slug` (required), `id` (UUID), `run_id` (run id format), `node_id`, `archetype`, `model`, `metadata` (any JSON).
- **Response:** 201 session object. 200 with the stored session on a replay of `id`.
- **Errors:** 400 (missing `workspace_slug`, malformed `id` or `run_id`); 404 `workspace not found`; 403 `forbidden` (read-only member); 409 `workspace_archived`; 409 `conflict` (the `id` belongs to another user or workspace).

#### `GET /sessions`
- **Purpose:** list sessions.
- **Scope / access:** `sessions:read`. Results are limited to sessions the caller may read (AUD-FR-007).
- **Query:** `workspace` (slug); `include_forks` (`true` also lists sessions on the task forks of `workspace`, including expired forks; default `false`); `run_id`; `status`; `credential_type`; `since`, `until` (on `started_at`; inclusive, exclusive); `order` (`desc` default, `asc`); `limit`; `cursor`.
- **Order:** (`started_at`, `id`) in the chosen direction.
- **Response:** 200 list envelope of session objects.
- **Errors:** 400 (bad parameter; `include_forks` without `workspace`); 404 `workspace not found` (the `workspace` filter names one the caller cannot read).

#### `GET /sessions/:id`
- **Purpose:** read one session with its token summary, forks, changes and counts.
- **Scope / access:** `sessions:read`; AUD-FR-007.
- **Response:** 200 session object.
- **Errors:** 400 (`:id` is not a UUID); 404 `session not found`.

#### `POST /sessions/:id/complete`
- **Purpose:** end an active session.
- **Scope / access:** `sessions:write`; creator or admin token.
- **Body:** `status` (required: `completed`, `failed`, `timeout`), `error_message`, `duration_ms` (integer ≥ 0), `cache_creation_input_tokens` (integer ≥ 0).
- **Response:** 200 session object, updated, or unchanged when it was already terminal.
- **Errors:** 400 (bad `status`, including `terminated`; negative numbers); 404 `session not found`; 403 `forbidden`.

#### `POST /sessions/:id/usage`
- **Purpose:** report one token usage increment.
- **Scope / access:** `sessions:write`; creator or admin token.
- **Body:** `model` (required), `id` (UUID), `input_tokens`, `output_tokens`, `cache_read_tokens` (integers ≥ 0, default 0).
- **Response:** 201 usage record. 200 with the stored record on a replay of `id`; a replay has no effect of any kind.
- **Errors:** 400; 404 `session not found`; 403 `forbidden`; 409 `session_not_active`; 409 `conflict` (the usage `id` belongs to another session).

#### `GET /sessions/:id/usage`
- **Purpose:** list the usage records of a session.
- **Scope / access:** as `GET /sessions/:id`.
- **Query:** `order` (`desc` default, `asc`), `limit`, `cursor`.
- **Order:** (`reported_at`, `id`).
- **Response:** 200 list envelope of usage records, plus `session_id` and `totals` (same shape as `token_summary`, over all records regardless of paging).

#### `GET /workspaces/:slug/cost`
- **Purpose:** token usage of a workspace over a window.
- **Scope / access:** `sessions:read`; read access to the workspace, whatever its status.
- **Query:** `group_by` (`day` default, `session`, `model`); `since` (default: 24 h ago); `until` (default: now); `limit`; `cursor`.
- **Response:** 200

```json
{
  "workspace": "w",
  "period": {"since": "2026-09-01T00:00:00Z", "until": "2026-09-02T00:00:00Z"},
  "group_by": "day",
  "totals": {"input_tokens": 10, "output_tokens": 0, "cache_read_tokens": 0, "sessions": 1},
  "items": [{"key": "2026-09-01", "input_tokens": 10, "output_tokens": 0, "cache_read_tokens": 0, "sessions": 1}],
  "next_cursor": null
}
```

  `key` is a UTC date, a session id or a model name, according to `group_by`. Items are ordered by `key` ascending. `period` holds the effective, normalised bounds. `totals` cover the whole window regardless of paging.
- **Errors:** 400 (bad `group_by`, bad timestamp, `since` not before `until`).

#### `POST /workspaces/:slug/runs/:run_id/events`
- **Purpose:** ingest one run event.
- **Scope / access:** `audit:write`; AUD-FR-020.
- **Body:** `event_type` (required), `id`, `severity`, `node_id`, `session_id`, `timestamp`, `payload` (JSON object), `run_id` (must equal the path).
- **Response:** 201 the stored run event; 200 on a replay.
- **Errors:** 400; 404 `workspace not found`; 403 `forbidden`; 409 `workspace_archived`; 409 `conflict` (the `id` exists in another workspace or run); 503 `unavailable` (write timeout, `Retry-After: 5`).

#### `POST /workspaces/:slug/runs/:run_id/events/batch`
- **Purpose:** ingest up to 1000 run events.
- **Body:** JSON array of run event objects.
- **Response:** 200 `{"accepted": <int>, "duplicates": <int>, "errors": [{"index": <int>, "id": "<id or null>", "message": "<text>"}]}`. `errors` is `[]` when there are none. An item whose `id` exists in another workspace or run is reported in `errors`.
- **Errors:** as the single route, plus 400 (not an array; empty array) and 413 `payload_too_large` (more than 1000 items). A 503 means nothing of the batch was stored.

#### `POST /workspaces/:slug/runs/:run_id/sessions/outcomes`
- **Purpose:** ingest one session outcome.
- **Body:** `session_id` (required), `status` (required), `id`, `node_id`, `timestamp`, `duration_ms`, `token_usage`.
- **Response / errors:** as `POST …/events`, with the stored outcome.

#### `POST /workspaces/:slug/runs/:run_id/tools/calls`
- **Purpose:** ingest one tool call.
- **Body:** `tool_name` (required), `id`, `node_id`, `session_id`, `timestamp`, `duration_ms`, `input`, `output`.
- **Response / errors:** as `POST …/events`, with the stored tool call.

#### `POST /workspaces/:slug/runs/:run_id/tools/errors`
- **Purpose:** ingest one tool error.
- **Body:** `tool_name` (required), `error_msg` (required), `id`, `error_code`, `node_id`, `session_id`, `timestamp`.
- **Response / errors:** as `POST …/events`, with the stored tool error.

#### `POST /workspaces/:slug/runs/:run_id/traces`
- **Purpose:** ingest one trace record.
- **Body:** `event_type` (required, closed set of §14.3.4), `id`, `node_id`, `session_id`, `sequence`, `timestamp`, `data`.
- **Response / errors:** as `POST …/events`, with the stored trace.

#### `POST /workspaces/:slug/runs/:run_id/traces/batch`
- **Purpose:** ingest up to 1000 trace records.
- **Body, response, errors:** as `POST …/events/batch`, with trace objects.

#### `POST /workspaces/:slug/runs/:run_id/postmortem`
- **Purpose:** store the postmortem of a run.
- **Body:** `run_status` (required), `started_at`, `completed_at` (required), `task_summary` (required), `cost_summary` (required), `schema_version` (optional; must be 1), `blocked_tasks`, `session_history`.
- **Response:** 201 the stored postmortem. 200 with the stored postmortem when the run already has one in this workspace; the new body is ignored.
- **Errors:** as `POST …/events`; an unsupported `schema_version` answers 400 with `error_type: unknown_schema_version`.

#### `GET /workspaces/:slug/runs/:run_id/events`
- **Purpose:** list the run events of a run.
- **Scope / access:** `audit:read`; read access to the workspace.
- **Query:** `event_type`, `severity`, `node_id`, `session_id` (exact match); `since`, `until`; `order` (`asc` default, `desc`); `limit`; `cursor`.
- **Response:** 200 list envelope of run events (all fields of §14.3.4).
- **Errors:** 400 (bad `run_id` or parameter); 404 `workspace not found`.

#### `GET /workspaces/:slug/runs/:run_id/sessions/outcomes`
- As the run event list. Filters: `node_id`, `session_id`, `status`. Items are session outcomes.

#### `GET /workspaces/:slug/runs/:run_id/tools/calls`
- As the run event list. Filters: `node_id`, `session_id`, `tool_name`. Items are tool calls.

#### `GET /workspaces/:slug/runs/:run_id/tools/errors`
- As the run event list. Filters: `node_id`, `session_id`, `tool_name`. Items are tool errors.

#### `GET /workspaces/:slug/runs/:run_id/traces`
- As the run event list. Filters: `event_type`, `node_id`, `session_id`. Items are trace records.

#### `GET /workspaces/:slug/runs/:run_id/postmortem`
- **Purpose:** read the postmortem of a run.
- **Response:** 200 the postmortem (all fields of §14.3.4).
- **Errors:** 404 with `error_type: postmortem_not_found` when the run has none in this workspace.

#### `GET /workspaces/:slug/runs/:run_id/transcript`
- **Purpose:** rebuild the conversation of one node from the run's trace records.
- **Query:** `node_id` (required).
- **Behaviour:** takes the traces of the run and node, ordered by `timestamp` then `sequence`. `session.init` becomes role `system`, `assistant.message` becomes `assistant`, `tool.use` becomes `tool_use`, `tool.error` becomes `tool_error`; `session.result` is skipped. `content` is `data.content` when that is a string, else `""`. `tool_name` is `data.tool_name` when that is a string, else null.
- **Response:** 200 `{"run_id", "node_id", "items": [{"role", "content", "tool_name", "timestamp"}], "next_cursor"}`. An unknown run or node gives an empty list.
- **Errors:** 400 (bad `run_id`; missing `node_id`); 404 `workspace not found`.

#### `GET /audit`
- **Purpose:** one query over hub events and run events.
- **Scope / access:** `audit:read`; AUD-FR-033.
- **Query:**

| Parameter | Applies to | Meaning |
|---|---|---|
| `source` | both | `hub` or `agent`. Default: both |
| `workspace` | both | Exact slug |
| `types` | both | Same syntax as AUD-FR-042 |
| `session_id` | both | UUID |
| `since`, `until` | both | Inclusive, exclusive |
| `actor_id`, `actor_type`, `resource_type`, `resource_id`, `action` | hub | Exact match. Using one of them excludes run events |
| `run_id`, `node_id`, `severity` | agent | Exact match. Using one of them excludes hub events |
| `limit`, `cursor` | both | List paging |

- **Response:** 200 list envelope. Every item has `source`, `id`, `type`, `time`, `workspace` and `session_id`. A hub item has all other fields of the hub event record (§14.3.5), including `seq`. An agent item has `run_id`, `node_id`, `severity` and `payload`; its `type` is the run event's `event_type` and its `time` the run event's `timestamp`. Fields of the other source are null.
- **Errors:** 400 (bad parameter; `since` not before `until`; a hub-only filter combined with `source=agent`, or an agent-only filter with `source=hub`); 404 `workspace not found`.

#### `GET /events`
- **Purpose:** hub events as a resumable Server-Sent Events stream.
- **Scope / access:** `audit:read`; visibility of §14.3.7.
- **Request:** query `workspace`, `types`, `session_id` (AUD-FR-042) and `after` (a `seq`). The header `Last-Event-ID` also gives a resume position and wins over `after`, so that a client library that reconnects by itself resumes correctly.
- **Response:** 200 with `Content-Type: text/event-stream` and `Cache-Control: no-cache`. The stream holds two kinds of frame:

```
id: 48213
event: hub_event
data: {"seq":48213,"id":"…","type":"hub.change.land","time":"2026-01-02T03:04:05Z","workspace":"api","actor_type":"pat","actor_id":"u_1","resource_type":"change","resource_id":"42","action":"land","session_id":null,"metadata":{…}}

id: 48290
event: heartbeat
data: {"time":"2026-01-02T03:04:35Z","seq":48290}
```

  The `id` of a `hub_event` frame is the event's `seq`. The `id` of a heartbeat is the position up to which the log has been examined for this client (AUD-FR-045). `data` is always one line of JSON. Behaviour: AUD-FR-044 to AUD-FR-048.
- **Errors (all before the stream starts, all in the error envelope):** 400 (bad filter or position); 404 `workspace not found` (the `workspace` filter names one the caller cannot read); 410 `cursor_expired`; 503 `unavailable` (connection limit, `Retry-After: 5`).

  ```json
  {"error": {"code": 410, "message": "event cursor expired", "error_type": "cursor_expired", "oldest_seq": 31007}}
  ```

  To continue after a 410, a consumer resumes with `after = oldest_seq − 1` and accepts that it has missed events.

#### `GET /events/log`
- **Purpose:** the event log as JSON pages, for consumers that poll.
- **Scope / access:** as `GET /events`.
- **Query:** `after` (a `seq`; when absent the page starts at the oldest event in the log); `workspace`, `types`, `session_id`; `limit`; `wait` (whole seconds, 0 to 60, default 0); `cursor` (accepted as another name for `after`).
- **Behaviour:** returns visible, matching events with `seq > after` in ascending `seq`. When there are none and `wait` is above 0, the request is held until one is written or the wait ends, then answers 200 (possibly with no items). A held request is released at once when the hub shuts down.
- **Response:** 200

```json
{"items": [ <hub event>, … ], "next_cursor": "48213", "next_after": 48213, "oldest_seq": 31007, "last_seq": 48290}
```

  `next_after` is always present: pass it as `after` in the next request. It is the `seq` of the last item, or, when the page is not full, the position up to which the log was examined, which may be higher. `next_cursor` is `next_after` as a string when the page was full and more events may follow, else null. `oldest_seq` and `last_seq` bound the event log at the time of the answer.
- **Errors:** 400; 404 `workspace not found`; 410 `cursor_expired` with `oldest_seq`.

#### `GET /metrics`
- **Purpose:** Prometheus text exposition of the metrics of §14.7.3.
- **Auth:** none. The route is outside `/api/v1`. Label values include workspace slugs, so operators should not expose it to untrusted networks.
- **Response:** 200 `text/plain; version=0.0.4`.

### 14.6 CLI

The conventions of §3 apply: JSON on stdout, the error envelope on stderr, exit codes 0, 1 and 2.

#### `parley session start <workspace-slug>`
Opens a session and prints the session object. Flags: `--id <uuid>`, `--run-id <id>`, `--node-id <id>`, `--archetype <text>`, `--model <name>`, `--metadata <json>`. The printed `id` is what `git push -o parley.session=<id>` and the `--session` flags of other commands take.

#### `parley session usage <session-id>`
Reports one usage increment. Flags: `--model <name>` (required), `--input <n>`, `--output <n>`, `--cache-read <n>`, `--id <uuid>`.

#### `parley session complete <session-id>`
Ends a session. Flags: `--status completed|failed|timeout` (default `completed`), `--error <text>`, `--duration-ms <n>`.

#### `parley session get <session-id>`
Prints the session object, with its forks, changes and counts.

#### `parley session list`
Lists sessions. Flags: `--workspace <slug>`, `--include-forks`, `--run-id <id>`, `--status <status>`, `--since <time>`, `--limit <n>`, `--cursor <c>`.

#### `parley audit query`
Calls `GET /audit`. Flags: `--source hub|agent`, `--workspace <slug>`, `--types <list>`, `--session <id>`, `--run-id <id>`, `--actor-id <id>`, `--resource-type <t>`, `--resource-id <id>`, `--action <a>`, `--since <time>`, `--until <time>`, `--limit <n>`, `--cursor <c>`.

#### `parley events tail`
Follows the event stream. Flags: `--workspace <slug>`, `--types <list>`, `--session <id>`, `--after <seq>`.
- Prints one hub event per line as compact JSON (not indented), so the output can be piped line by line. Heartbeats are not printed.
- Without `--after` it starts with live events. With `--after` it replays first.
- Runs until interrupted, without a request timeout. An interrupt exits 0.
- When the stream ends for any reason other than an interrupt (hub restart, slow-consumer close, network error, `503`), the command reconnects by itself, waiting 1 s at first and doubling up to 30 s, and resumes from the last `seq` it received. No event is lost and none is printed twice.
- It stops with exit 1 on an error that a retry cannot fix: `401`, `403`, `404`, `400`, and `410 cursor_expired`. On `cursor_expired` the error on stderr includes `oldest_seq`; the command never skips events silently.

#### `parley events list`
Calls `GET /events/log` once and prints the response. Flags: `--workspace <slug>`, `--types <list>`, `--session <id>`, `--after <seq>`, `--limit <n>`, `--wait <seconds>`. The request timeout is extended by the wait.

### 14.7 Background behaviour

#### 14.7.1 Start-up and shutdown

- At start, before serving requests, the hub writes any event that a ref update still owes (§14.3.6).
- Sessions need no recovery at start. Stale active sessions are handled by retention.
- At shutdown the hub closes all event streams and releases all long-polls. Consumers resume from their last `seq` after the restart.

#### 14.7.2 Retention

Retention runs once at start and then every hour. Its steps run in the order below. A failing step is logged and counted on `parley_retention_errors_total{step}`; the remaining steps still run. `parley_retention_last_run_timestamp_seconds` is set only when every step succeeded.

| Step (`step` label) | Action |
|---|---|
| `stale_sessions` | `active` sessions started before now − `[sessions].max_active_age` become `timeout`, with `ended_at` = now and `error_message` = `session exceeded maximum active age` |
| `run_records` | Delete run events, session outcomes, tool calls and tool errors with `timestamp` older than `[audit].max_age` |
| `max_runs` | Per workspace, when more than `[audit].max_runs` runs have telemetry, delete the run events, outcomes, tool calls, tool errors and traces of the oldest excess runs. A run's age is its earliest record. Postmortems are kept |
| `traces` | Delete trace records older than `[audit].trace_max_age` |
| `postmortems` | Delete postmortems ingested before now − `[audit].postmortem_max_age` |
| `sessions` | Delete terminal sessions started before now − `[sessions].max_age`, together with their usage records. Active sessions are never deleted |
| `event_log` | Events older than `[events].retention` leave the event log. `oldest_seq` moves forward accordingly |
| `hub_events` | Delete hub events older than `[audit].max_age` |
| `orphans` | Delete sessions, usage records, telemetry and hub events of workspaces that no longer exist, once they are older than `[audit].orphan_retention`. Records of a deleted task fork count as belonging to its parent while the parent exists, so they are kept under the normal limits |
| `gauges` | Recompute `parley_audit_records{kind}` and `parley_agent_sessions_active{workspace}` from the stored data |

The newest event always stays in the event log, whatever its age, so `last_seq` is always known.

#### 14.7.3 Prometheus metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `parley_http_requests_total` | counter | `method`, `path` (route template), `status` | Every HTTP request, API, git and webhook routes included |
| `parley_http_request_duration_seconds` | histogram | `method`, `path` | Buckets 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10 |
| `parley_git_requests_total` | counter | `service` (`info-refs`, `upload-pack`, `receive-pack`), `result` (`ok`, `refused`, `error`) | Git protocol requests (§5). `refused` covers authentication, access and policy refusals |
| `parley_scm_requests_total` | counter | `op` (`refs`, `commits`, `tree`, `file`, `diff`, `compare`, `blame`, `search`, `commit`, `rebase`), `result` (`ok`, `ref_moved`, `refused`, `error`) | Source control API requests (§6) |
| `parley_changes_open` | gauge | `workspace` | Changes in state `draft` or `open` (§7) |
| `parley_lands_total` | counter | `result` (`landed`, `conflict`, `blocked`, `failed`) | Land requests by final result (§8) |
| `parley_land_attempts` | histogram | none | Compare-and-swap attempts per land. Buckets 1, 2, 3, 5, 8, 13 |
| `parley_land_duration_seconds` | histogram | none | Time from land request to final result. Buckets 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60 |
| `parley_forks_active` | gauge | none | Task forks that exist (§4) |
| `parley_events_seq` | gauge | none | The last `seq` written |
| `parley_sse_connections` | gauge | none | Open event streams |
| `parley_audit_events_total` | counter | `source` (`hub`, `agent`), `type` | Hub events written and run events ingested. Run events ingested in a batch are counted under their own type |
| `parley_agent_sessions_active` | gauge | `workspace` | Sessions in state `active` |
| `parley_agent_tokens_total` | counter | `workspace`, `model`, `direction` (`input`, `output`, `cache_read`) | Tokens reported in usage records. Replays are not counted |
| `parley_audit_records` | gauge | `kind` (`hub_event`, `run_event`, `outcome`, `tool_call`, `tool_error`, `trace`, `postmortem`, `session`, `usage`) | Stored records, sampled by retention |
| `parley_jobqueue_depth` | gauge | `type`, `status` | Background jobs by type and status |
| `parley_retention_errors_total` | counter | `step` | Failed retention steps |
| `parley_retention_last_run_timestamp_seconds` | gauge | none | Unix time of the last fully successful retention run |
| `parley_sandboxes_active` | gauge | `workspace` | See §13 |
| `parley_sandbox_creates_total` | counter | `result` (`created`, `failed`) | See §13 |
| `parley_sandbox_lease_seconds` | histogram | none | Buckets 60, 300, 600, 1200, 1800, 3600, 7200, 14400, 28800. See §13 |

Label values with a closed set are all present from start-up with value 0.

### 14.8 Events

This is the single catalogue of hub event types. Other sections describe when their events fire in more detail; the types, resource fields and metadata keys here are authoritative. Every event has the record shape of §14.3.5. `action` is always the last segment of the type and is not repeated in the tables. "Slug" means the slug of the event's workspace. Unless a row says otherwise, the event's `workspace` is the workspace of the route that caused it, the actor is the caller, and the event is written only on success.

Run event types are free-form strings chosen by agents and are not part of this catalogue.

#### 14.8.1 Workspaces and task forks (§4)

| Type | Emitted when | `resource_type` / `resource_id` | Metadata |
|---|---|---|---|
| `hub.workspace.create` | A workspace was created | `workspace` / slug | `git_url` (no userinfo), `branch` |
| `hub.workspace.update` | A workspace was updated | `workspace` / slug | `fields` (names of the changed fields) |
| `hub.workspace.archive` | A workspace was archived | `workspace` / slug | `head_sha` (when known) |
| `hub.workspace.reactivate` | An archived workspace was reactivated | `workspace` / slug | none |
| `hub.workspace.delete` | A workspace was deleted by request. `workspace` is the deleted slug | `workspace` / slug | `revoked_tokens` (ids of the bound tokens revoked) |
| `hub.workspace.sync` | A sync from origin finished | `workspace` / slug | `result` (the sync result of §4) |
| `hub.workspace.fork` | A task fork was created. `workspace` is the fork's slug | `workspace` / fork slug | `parent`, `fork_point` (`{ref, sha}`), `expires_at` |
| `hub.fork.expire` | The hub deleted a task fork. `workspace` is the fork's slug. Actor `system`. A fork deleted by request emits `hub.workspace.delete` instead | `workspace` / fork slug | `reason` (`ttl`, `landed`, `parent_deleted`), `parent` |
| `hub.branch.push` | A push of a branch to origin was attempted. Written on success and on failure of the push itself; a request the hub refuses emits nothing | `branch` / branch name | `branch`, `sha`, `ok`, `remote_url` (no userinfo), then `result` (when `ok`) or `error_type` (when not) |
| `hub.session.force_closed` | An active session was terminated because its workspace was archived or deleted, or its fork expired. One per session. Actor `system` | `session` / session id | `reason` (`workspace archived`, `workspace deleted`, `fork expired`) |

Fork events are visible to readers of the parent (§14.3.7), so the owner of a workspace sees its forks come and go.

#### 14.8.2 Tokens (§3)

| Type | Emitted when | `resource_type` / `resource_id` | Metadata |
|---|---|---|---|
| `hub.token.bind` | A token bound to a workspace was created, including the token issued with a fork | `token` / token id | `permissions`, `paths` (or null), `expires_at` (or null). Never the token |
| `hub.token.bound_refused` | A bound token was refused because the request was outside its workspace. At most one event per token per minute. `workspace` is the bound workspace; the actor is the token's user | `token` / token id | `requested_workspace` (when the request named one), `method`, `route`, `status`, `surface` (`api`, `git`), `suppressed` (refusals not reported since the previous event) |

#### 14.8.3 Git server and source control API (§5, §6)

| Type | Emitted when | `resource_type` / `resource_id` | Metadata |
|---|---|---|---|
| `hub.git.push` | A ref was updated: by a git push (§5), through the source control API (§6: ref create, move, delete, commit, rebase), or by a land moving its target (§8). One event per accepted ref update, in order; a rejected update emits nothing | `ref` / full ref name | `ref`, `before`, `after` (shas; null on the side that does not exist), `created`, `deleted`, `forced` (booleans; `forced` means the update was not a fast-forward), `via` (`git`, `api` or `land`) |
| `hub.git.fetch` | A fetch or clone was served. Only when the workspace setting `audit_fetches` is on (default off) | `workspace` / slug | `protocol` (`v0`, `v2`), `shallow` (boolean), `filter` (partial-clone filter or null) |
| `hub.scm.commit` | A commit was created through the source control API | `commit` / new sha | `branch`, `sha`, `parent_sha`, `files` (paths touched, at most 100) |

All events of one push are written together and have consecutive positions among the workspace's events.

#### 14.8.4 Changes and landing (§7, §8)

For every type in this table `resource_type` is `change`, `resource_id` is the change number as a decimal string, and `workspace` is the workspace the change belongs to (the workspace of its target branch). Every event also has `number` in its metadata.

| Type | Emitted when | Actor | Metadata beyond `number` |
|---|---|---|---|
| `hub.change.open` | A change entered state `open`: created open, or a draft was published | caller | `title`, `target_branch`, `source_workspace`, `source_branch`, `depends_on` (numbers), `was_draft` |
| `hub.change.version` | A change got a new version, including version 1 of a draft | the pusher or committer; `system` for a restack | `version`, `head_sha`, `kind` (`push`, `commit`, `restack`) |
| `hub.change.restack` | The hub restacked a change after a change below it moved or landed. Written after the `hub.change.version` of the new version | `system` | `version`, `head_sha`, `trigger` (number of the lower change) |
| `hub.change.ready` | The change became landable: its `landing.status` is `ready` and its `mergeable` is `clean`, for its current version | `system` | `version` |
| `hub.change.conflict` | The change's `mergeable` became `conflict`. Not repeated while it stays so | `system` | `version`, `files` |
| `hub.change.overlap` | A new version made the change touch files that another open change also touches. One event per pair, on the change that got the new version, the first time the pair overlaps | `system` | `with` (number of the other change), `paths` |
| `hub.change.approve` | A version was approved | caller | `version` |
| `hub.change.reject` | A version was rejected | caller | `version` |
| `hub.change.check` | A check result was reported | caller | `version`, `name`, `status` |
| `hub.change.land` | The change landed | the caller of the land; `system` for an automatic land | `landed_sha`, `target_branch`, `strategy`, `attempts` |
| `hub.change.land_fail` | A land ended without landing | as above | `reason` (`conflict`, `blocked`, `failed`), `attempts`, `files` (on `conflict`) |
| `hub.change.abandon` | The change was abandoned | caller | none |
| `hub.change.restore` | An abandoned change was restored | caller | none |
| `hub.change.revert` | A reverting change was opened for a landed change. Written on the new change | caller | `reverts` (number of the landed change) |
| `hub.policy.update` | The landing policy of a workspace was replaced. `resource_type` `policy`, `resource_id` slug | caller | `rules` (number of rules) |

A land that loses a compare-and-swap race and is retried emits nothing for the lost attempt; `attempts` on the final event counts them.

#### 14.8.5 Awareness (§9)

`resource_type` is `claim` and `resource_id` the claim id.

| Type | Emitted when | Actor | Metadata |
|---|---|---|---|
| `hub.claim.create` | A claim was made | caller | `paths`, `intent`, `expires_at` |
| `hub.claim.release` | A claim was released by request | caller | `paths`, `intent` |
| `hub.claim.expire` | A claim reached its expiry | `system` | `paths`, `intent` |

#### 14.8.6 Carry-patch (§10)

`resource_type` is `patch` with `resource_id` the patch branch name, or `rebuild` with `resource_id` the rebuild job id.

| Type | Emitted when | Actor | `resource_type` | Metadata |
|---|---|---|---|---|
| `hub.patch.create` | A patch was registered. One event per patch, also for a batch | caller | `patch` | `branch_name`, `position`, `branch_resolution` |
| `hub.patch.delete` | A patch was soft-deleted | caller | `patch` | `branch_name` |
| `hub.patch.reset` | A patch branch was reset to origin | caller | `patch` | `branch_name`, `result` (`none`, `created`, `fast_forwarded`, `replaced`), `origin_sha`, `local_sha`, `replaced_sha` (the last two when they apply) |
| `hub.patch.replace` | A patch branch was replaced by origin's, by a reset or by a sync | caller | `patch` | `branch_name`, `replaced_sha`, `origin_sha`, `trigger` (`reset_to_origin`, `sync`) |
| `hub.patch.sync` | A carry-patch sync in origin mode finished. `resource_type` `workspace`, `resource_id` slug | caller | `workspace` | `created`, `fast_forwarded`, `replaced`, `diverged`, `missing_on_origin` (lists of branch names) |
| `hub.patch.mirror_failed` | Mirroring a patch branch to origin failed. One per branch | `system` | `patch` | `branch_name`, `error` |
| `hub.rebuild.enqueue` | A rebuild was requested through the API | caller | `rebuild` | `patch_count` |
| `hub.rebuild.complete` | A rebuild succeeded | `system` | `rebuild` | `patches_applied` |
| `hub.rebuild.fail` | A rebuild attempt failed, attempts that will be retried included | `system` | `rebuild` | `reason`, `conflict_files` (when a conflict) |
| `hub.rebuild.push_failed` | Pushing the integration branch to origin failed after a rebuild | `system` | `rebuild` | `integration_branch`, `reason` |
| `hub.rebuild.followup` | A follow-up rebuild was enqueued because inputs moved during a rebuild | `system` | `rebuild` | `stale_patches`, `upstream_stale`, `follow_up_job_id` |

#### 14.8.7 Secrets and variables (§11)

Six types: `hub.secret.create`, `hub.secret.update`, `hub.secret.delete`, `hub.variable.create`, `hub.variable.update`, `hub.variable.delete`. One event per key. `resource_type` is `secret` or `variable`; `resource_id` is the key. Metadata: `scope` (`user`, `org`, `workspace`), `key`, and `org` (org slug) at org scope. Never a value.

At workspace scope the event's `workspace` is the slug. At user and org scope `workspace` is null, and the scope decides who sees the event (§14.3.7). Credentials that the hub stores itself while creating a workspace or setting a webhook secret emit no secret event.

#### 14.8.8 Forge integration (§12)

**Writes to the forge.** One event per write request that reached the forge, on success and on failure. Common metadata: `target` (`origin`, `upstream`), `ok`, `error_type` (when not `ok`), `number` and `url` (when known). When the forge assigned no number because the call failed, `resource_id` is `none`.

| Type | `resource_type` / `resource_id` | Extra metadata |
|---|---|---|
| `hub.forge.issue.create` | `issue` / number | none |
| `hub.forge.issue.update` | `issue` / number | `fields` |
| `hub.forge.issue.close` | `issue` / number | `comment_url` (when a comment was posted) |
| `hub.forge.comment.create` | `comment` / issue number | none |
| `hub.forge.label.add` | `label` / issue number | `labels` |
| `hub.forge.label.remove` | `label` / issue number | `label` |
| `hub.forge.label.create` | `label` / label name | `label`, `created` |
| `hub.forge.pull_request.create` | `pull_request` / number | `head`, `base`, `draft` |
| `hub.forge.pull_request.comment` | `pull_request` / number | none |

**Webhook configuration.** `hub.forge.webhook.set` (metadata `generated`) and `hub.forge.webhook.unset` (metadata `existed`). `resource_type` `webhook`, `resource_id` slug.

**Deliveries from the forge.** Written once per verified, newly recorded delivery, in the same transaction that records the delivery. A duplicate delivery emits nothing. One delivery may produce several events, in a fixed order. Actor `forge`. Common metadata: `forge` (`github`, `gitlab`), `target`, `delivery_id`, `actor` (the forge login).

| Type | `resource_type` / `resource_id` | Extra metadata |
|---|---|---|
| `hub.forge.issue.opened`, `.edited`, `.closed`, `.reopened` | `issue` / number | `number`, `title` (first 256 bytes), `url`, `labels` |
| `hub.forge.issue.labeled`, `.unlabeled` | `issue` / number | `number`, `label`, `labels`, `url` |
| `hub.forge.comment.created` | `comment` / issue or pull request number | `number`, `comment_url`, `pull_request` (boolean) |
| `hub.forge.pull_request.opened`, `.closed`, `.merged`, `.reopened`, `.synchronize` | `pull_request` / number | `number`, `url`, `head`, `base`, `merged` |
| `hub.forge.push` | `ref` / pushed ref | `ref`, `before`, `after` |

#### 14.8.9 Sandboxes (§13)

`resource_type` is `sandbox` and `resource_id` the lease id.

| Type | Emitted when | Actor | Metadata |
|---|---|---|---|
| `hub.sandbox.lease` | A lease was issued | caller | `profile`, `purpose`, `ttl`, `secret_names`, `optional_secret_names` |
| `hub.sandbox.create` | The sandbox was created and the lease became active | `lease` (the lease's creator) | `sandbox_id`, `image`, `network` |
| `hub.sandbox.failed` | The lease failed before a sandbox existed | `lease` | `error` |
| `hub.sandbox.destroy` | The sandbox was destroyed other than by expiry | `lease`, caller, or `system` | `sandbox_id` (or null), `reason` (`client`, `api`, `restart`, `shutdown`) |
| `hub.sandbox.expire` | Time ended the lease | `system` | `state` (`expired`, `destroyed`), `sandbox_id` (when one was destroyed) |

### 14.9 Configuration

Keys of the hub configuration file (§3). Durations take a number and a unit (`s`, `m`, `h`, `d`). A value that is malformed or out of range stops the hub at start with a message that names the key.

| Key | Default | Range | Effect |
|---|---|---|---|
| `[events].retention` | `30d` | `1h` to `[audit].max_age` | How long an event stays in the event log and can be resumed from |
| `[events].max_sse_connections` | `100` | ≥ 1 | Open event streams allowed at once |
| `[audit].max_age` | `90d` | ≥ `1d` | Hub events, run events, outcomes, tool calls, tool errors |
| `[audit].max_runs` | `50` | ≥ 1 | Runs with telemetry kept per workspace |
| `[audit].trace_max_age` | `30d` | ≥ `1d` | Trace records |
| `[audit].postmortem_max_age` | `180d` | ≥ `1d` | Postmortems |
| `[audit].orphan_retention` | `30d` | ≥ `1d` | Grace period for records of deleted workspaces |
| `[audit].write_timeout` | `2s` | ≥ `100ms` | Limit for one ingestion write |
| `[sessions].max_age` | `90d` | ≥ `1d` | Terminal sessions and their usage records |
| `[sessions].max_active_age` | `7d` | ≥ `1h` | Active sessions older than this are timed out |

Fixed, not configurable: heartbeat interval 30 s; per-stream buffer 256 events; idle-write limit 60 s; maximum long-poll 60 s; retention interval 1 h; batch maximum 1000 items; `types` filter maximum 20 entries.

The workspace setting `audit_fetches` (boolean, default `false`, §4) turns on `hub.git.fetch`.

### 14.10 Edge cases and error handling

- **Event write fails.** The state change and its event share a transaction, so a failed event write fails the request with 500 and nothing is changed. There is no state change without its event.
- **Crash after a ref update.** The owed event is written at the next start (§14.7.1) with the original ref values. Its `time` is the time of writing, so it may be later than the ref update.
- **Consumer falls behind.** The stream is closed (AUD-FR-046). The consumer reconnects with its last `seq` and gets the backlog by replay. Nothing is lost unless the consumer stays away longer than `[events].retention`.
- **Consumer stays away too long.** It gets `410 cursor_expired` with `oldest_seq`. It must decide for itself: resume at `oldest_seq − 1` and rebuild its state from the REST API. The hub never skips silently.
- **Filters that match rarely.** Heartbeats advance the resume position (AUD-FR-045), and `next_after` does the same for polling, so a quiet filter does not expire.
- **Duplicates.** A consumer that stores its position after processing may see the last events again after a crash of its own. Handlers should be idempotent on `seq` or `id`.
- **Access changes while streaming.** Events are checked against the caller's access when delivered. Events written while the caller had no access are not delivered later, also not by replay, if access is still missing at replay time. Replay uses current access, not past access.
- **Deleted workspaces.** Events and sessions of a deleted workspace remain until retention removes them. They are visible to admin tokens; those of a task fork are also visible to readers of its parent.
- **Empty log.** With no events newer than the resume position, the stream sends heartbeats only, and the log answers an empty page. `after` equal to `last_seq` is valid.
- **Both resume inputs.** `Last-Event-ID` wins over `after`. A `Last-Event-ID` that is not a non-negative integer answers 400.
- **Session ids reused.** A session, usage or telemetry `id` identifies one record hub-wide. Reuse in another place is `409 conflict`, never a silent no-op.
- **Replays.** A replay answers 200 with the stored record, even when the replayed body differs. The stored record is never changed by a replay.
- **Different credential, same user.** Session ownership is by user: a session opened with a PAT may be completed with an API key of the same user. Sessions opened by an admin token can be completed only by an admin token.
- **Session on an archived workspace.** New sessions and ingestion are refused (`409 workspace_archived`). Reads keep working.
- **Session named after it ended.** Allowed (§14.3.2). The counts of a terminal session can therefore still grow.
- **Fork expiry and sessions.** Active sessions on the fork are terminated. The session records stay readable by their creator and by readers of the parent, and still list the fork's slug and the changes produced, which live in the parent.
- **Batch with only invalid items.** 200 with `accepted: 0`, `duplicates: 0` and one error per item. Nothing is stored.
- **Write timeout.** 503 with `Retry-After: 5`; a batch is stored completely or not at all, so a retry with the same ids is safe.
- **Audit query filters that exclude a source.** A hub-only filter returns hub events only, an agent-only filter run events only. Combining one of each returns an empty list; combining a filter with the opposite explicit `source` is a 400.
- **`max_runs`.** Removing the telemetry of an old run keeps its postmortem and its sessions, which follow their own limits.
- **Metrics after retention.** A workspace without active sessions disappears from `parley_agent_sessions_active`.

---

## 15. Non-functional requirements

### 15.1 Deployment model

- **NFR-001** The hub runs as **one process**. Several replicas are not supported. Nothing in the product may require an external database, cache or message broker.
- **NFR-002** State is kept in embedded stores on local disk under one data directory: the hub's transactional state (identity, workspaces, changes, policy, land attempts, claims, jobs, secrets, leases, the event log), the analytical store for audit queries and agent telemetry, and the repositories. The choice of storage engines is an implementation decision. The repositories MUST remain ordinary git repositories that stock `git` can read.
- **NFR-003** Schema evolution is automatic. Every start brings the stores to the current schema without a separate migration step, and a newer binary can always open the data of the previous release.
- **NFR-004** The server needs the `git` command at run time (version 2.38 or later, checked at start). The `parley` client is a single static binary with no dependency on the server's stacks.
- **NFR-005** Version, commit and build time are set at build time and reported by `GET /version` and `parley version`.
- **NFR-006** Deliverables: the two binaries, a hub container image, a sandbox base image, and Kubernetes manifests (configuration, deployment with one replica, persistent volume claim, service, an example for secrets).
- **NFR-007** Backup is a consistent copy of the data directory. The hub provides a way to take one while running (a documented command or signal that produces a consistent snapshot), and a restored copy starts cleanly.

### 15.2 Concurrency and consistency

- **NFR-010** There is no global or per-workspace lock that a request waits on or fails on. Ref updates are compare-and-swap. The only exclusive periods are the maintenance operations of §4 (initial clone, reclone, archive, delete).
- **NFR-011** Readers never see a partial update: a ref has its old or its new value, a change is landed or not, a rebuild's integration branch is the old or the new one.
- **NFR-012** State that must agree is changed atomically: a ref move with its version record (§7), a land with its bookkeeping and event (§8), a state change with the event that reports it (§14).
- **NFR-013** Every piece of background work that a client was told about (clone, rebuild, land attempt, restack, automatic landing) survives a restart and is resumed or cleanly failed. After a crash the hub reconciles refs with records at start (§7.7).
- **NFR-014** Only these things live in memory and are lost at restart without harm: open event-stream connections (clients resume by `seq`), rate-limit counters, and caches.

### 15.3 Security

- **NFR-020** Secret values are write-only through every interface and encrypted at rest (§11). Tokens and keys are stored only as hashes.
- **NFR-021** Text that can carry credentials is scrubbed before it reaches a response, an event or a log: git errors, remote URLs, forge errors.
- **NFR-022** Anti-enumeration: a caller without read access to a workspace gets the same answer as for a workspace that does not exist, on every surface.
- **NFR-023** Git subprocesses run with validated ref names, a scrubbed environment, timeouts and process-group termination. Repository content is never executed by the hub: no hooks from repositories run, and server-side rebases do not run filters or drivers defined in repository configuration or attributes.
- **NFR-024** Least privilege is expressible end to end: a token can be limited to scopes, to one workspace or fork, to paths, and in time. A task fork's token is useless outside the fork except for its own changes (§3).
- **NFR-025** Forge webhooks are authenticated by signature or token and de-duplicated (§12). The ingress is outside bearer authentication and has its own body limit.
- **NFR-026** Policy cannot be bypassed by choice of surface: the push rules apply identically to git pushes and to the source control API, and landing is the only way a protected branch moves.

### 15.4 Performance targets

Targets are for one hub on a host with 8 cores, 32 GB of memory and local SSD storage, with repositories up to 5 GB of history. They are the initial bar and are to be revisited with measurements.

| Operation | Target |
|---|---|
| Task fork creation, any repository size | p95 under 1 s; no history is copied |
| Read of one file or tree through the API | p95 under 100 ms |
| Commit creation through the API (up to 100 files) | p95 under 500 ms |
| Land of a change of up to 20 commits, no contention | p95 under 2 s |
| Lands per minute on one target branch, non-conflicting changes | at least 60 sustained |
| Concurrent land attempts in flight | 8 computing, the rest waiting without failing |
| Mergeability known after a target move, 100 open changes | within 60 s |
| Event visible to a live subscriber after its state change | within 1 s |
| Active task forks per hub | 5,000 |
| Open changes per workspace | 2,000 |
| Concurrent event-stream connections | 500 (configurable) |

### 15.5 Limits

Each limit is defined in the section that owns it. The main ones:

| Limit | Value | Section |
|---|---|---|
| Request body under `/api/v1` | 1 MiB (commit creation: 32 MiB) | §3, §6 |
| List page size | default 50, max 200 | §3 |
| Idempotency-key window | 24 h | §3 |
| Request rate per credential | 600 per minute (configurable, 0 = off) | §3 |
| Fork TTL | default 24 h, max 30 d | §4 |
| Active forks per workspace | 200 | §4 |
| Operations per API commit | 1,000 | §6 |
| File read through the API | 32 MiB | §6 |
| Stack depth | 20 | §7 |
| Compare-and-swap tries per land | 10 | §8 |
| Land attempt duration | 5 min | §8 |
| Policy document | 200 rules, 64 KiB | §8 |
| Claim TTL | default 1 h, max 24 h | §9 |
| Durable job retries | 20, backoff 2 s doubling to 2 h | §10 |
| Event log retention | 30 d | §14 |
| `parley` request timeout | 30 s (3 min for sync and push to origin) | §3, §4 |
| `--wait` defaults | `--timeout 5m`, `--poll-interval 5s` | §3 |

### 15.6 Observability

- **NFR-040** `GET /metrics` exposes the Prometheus metrics catalogued in §14. Every documented metric is exported.
- **NFR-041** `/healthz` reports liveness, `/readyz` readiness, `/version` build information (§3).
- **NFR-042** Logs are structured (one JSON object per line), carry the request id, and never contain credentials or secret values.
- **NFR-043** The hourly maintenance loop (§3) runs retention and cleanup tasks. One failing task never stops the others.

---

## 16. Acceptance criteria

Parley is accepted when the following hold, demonstrated by black-box tests against a running hub using only the REST API, the `parley` CLI and stock `git`.

1. **Change as the unit of work (§7).** A pushed branch becomes a change with a stable number. Each of five successive pushes, including a force push, yields a new version; all five heads are fetchable by version ref. The interdiff between a version and its pure rebase is empty. A commit on a protected branch resolves to its change, version and session.
2. **Stacks (§7).** Three stacked changes are opened by pushing three branches. Amending the bottom one produces `restack` versions of the two above without client action. Landing the bottom one rebases the next onto the target and clears its `depends_on`. A restack that cannot apply sets `needs_restack` with a conflict report and moves nothing.
3. **Task forks (§4).** One call returns a fork, its remote and a token. The fork is usable at once for a repository of at least 1 GB, and creation time does not grow with repository size. The token is refused everywhere outside the fork except for its own changes. A push to a tracking branch is refused. The fork disappears at its TTL, and earlier once its changes have landed.
4. **Source control API (§6).** Without any clone: read a file, a tree, a path's history, a diff and blame; create a commit of several files; observe `409 ref_moved` on a stale `expected_head_sha`; open and land a change. The resulting repository is identical to one produced by the equivalent git operations.
5. **Optimistic landing (§8).** Twenty changes touching twenty different files are landed concurrently on one target: all land, none of the callers retries, the history is linear and contains each change once. Two changes editing the same lines are landed concurrently: exactly one lands, the other ends `conflict` with a report naming the file and the landed change, and the target contains only the first. A hub killed during a land restarts into a state where the change is either fully landed with its event, or not landed at all.
6. **Conflict resolution without a clone (§8).** The conflicted file is fetched with markers, a resolution is submitted, and the change then lands.
7. **Policy by path (§8).** With a policy of three rules (automatic, check required, approval required, plus one blocked path): a docs-only change lands with no approval; a change needing a check is `pending` until the check is reported `passed`; a change needing approval does not count the approval given with the credential that pushed it, and counts one from another credential; a change touching the blocked path cannot land, even with an override. An approval survives a pure rebase and does not survive a content change. `evaluate` predicts each of these before the change exists.
8. **Path-scoped tokens (§3, §5, §6).** A token limited to `a/**` can push and commit changes under `a/` and is refused, with the offending path named, for a commit touching `b/x`, over git and over the API alike.
9. **Protected branches (§5, §6, §8).** A protected branch cannot be moved by git push, API commit, API ref move or delete with any credential. It moves by landing and by sync from origin.
10. **Automatic landing and revert (§8).** A change with `auto_land` lands by itself once its required check passes. A landed change is reverted by one call, and landing the reverting change restores the previous content.
11. **Awareness (§9).** With two open changes touching the same file, each receives one `hub.change.overlap` event naming the other. `activity` for that path returns both changes and a live claim, and no longer returns the claim after it expires. No awareness state ever causes a push or land to be refused.
12. **Durable events (§14).** A consumer that disconnects, misses fifty events and reconnects with its last `seq` receives exactly those fifty, in order, then live events. A cursor older than retention gets `410 cursor_expired`. Events for a push, a fork, a version, a land and a conflict each carry the session given on the operation. No event is missing after the hub is killed and restarted under load.
13. **Sessions as the thread (§14).** From a session id, the fork, the changes, their versions and their land attempts are reachable. From a landed commit, the session is reachable.
14. **Git compatibility (§5).** Stock `git` clones (full, shallow, and with `--filter=blob:none`), fetches and pushes through the hub with a PAT and the credential helper. A workspace the caller cannot read is indistinguishable from one that does not exist.
15. **Carried features.** Workspaces (§4), carry-patch workspaces (§10), secrets and variables (§11), forge integration (§12), sandboxes (§13) and sessions, audit and metrics (§14) each pass a test of every listed success and error case of their REST routes and CLI commands.
16. **Conventions (§3).** Every error on every route is the envelope with an `error_type`. Every list is paged the same way. Every creating `POST` honours `Idempotency-Key`. A wrong method is `405`.
17. **Operations (§15).** The hub runs from one binary, one configuration file and one data directory. A backup taken while running restores to a hub that starts cleanly. The performance targets of §15.4 are met on the reference host.

---

## 17. Sequencing and open decisions

### 17.1 Sequencing

The sections are written as one product, but they are meant to be built in this order. Each stage ends with something usable.

| Stage | Contents | Result |
|---|---|---|
| 0. Foundation | Platform, identity and access (§3); workspaces without forks (§4); git server without change mapping (§5); secrets and variables (§11); the event log and basic audit (§14) | A hub that hosts repositories and serves git with scoped tokens |
| 1. First cut | Changes without stacks (§7); optimistic landing with the default policy, protected branches and conflict reports (§8); task forks (§4); source control API (§6); push-to-change mapping (§5) | An agent can fork, work, open a change and land it, over git or over the API alone |
| 2. Coordination | Stacks and restack (§7); policy rules, approvals, checks, automatic landing, revert, path-scoped tokens (§8, §3); awareness (§9); event replay, filters and long-poll (§14); sessions linked to changes (§14) | Many agents on one repository, with rules about what lands and visibility of each other |
| 3. Carried features | Forge integration (§12); sandboxes (§13); carry-patch workspaces (§10); agent telemetry ingestion and the full audit query (§14) | The complete product |

Stage 3 has no dependency on stage 2 and can run in parallel with it.

### 17.2 Later candidates

Not part of this document's requirements. Listed so that the design leaves room for them.

- **Parallel attempts.** Several forks started from one point for the same task, compared side by side, one chosen to land.
- **An undo log** for server-side operations (land, restack, abandon, ref moves), in the manner of Sapling's `undo`.
- **Checks run by the hub** in its own sandboxes (§13), on the rebased result of a change.
- **Automatic mirror to origin** after each land, as a workspace setting.
- **Exporting a change to the forge** as a pull request, and importing forge review state as approvals.
- **Per-agent quotas** beyond the request rate limit: forks, lands and storage per user or session.
- **Outbound webhooks** as a second delivery method for events.
- **Ownership files in the repository** (`OWNERS` style) as an additional source of policy rules.
- **Hunk-level overlap** in awareness.
- **Sparse working copies** managed by the `parley` client.

### 17.3 Open decisions

Choices made while writing this document that the product owner should confirm or change. Each is implemented as written unless decided otherwise.

| # | Decision taken | Where | Alternative |
|---|---|---|---|
| 1 | Names: credential prefix `parley_`, environment prefix `PARLEY_`, client configuration in `~/.parley`, metrics `parley_*`; event types keep `hub.*` and refs keep `refs/hub/` | §3, §14 | Shorter credential prefix; `parley.*` events |
| 2 | The default branch is protected by default, so even the owner cannot push to it directly | §4, §8 | Protection off by default, or an owner exemption |
| 3 | The default policy requires nothing: every change may land | §8 | A default that requires one approval for agent-authored changes |
| 4 | Checks are reported on a version, not on the rebased result, and carry over a pure rebase | §8 | `fresh_checks` on by default |
| 5 | An approval is not counted when given with the same credential that created the version; the rule compares credentials, not users | §8 | Compare users, with an explicit "agent" marker on tokens |
| 6 | Owners may override unmet approvals and checks (never blocks), with a recorded reason | §8 | No override at all |
| 7 | Landing strategy defaults to `rebase` (commits kept); `squash` is a setting | §8 | Squash by default, one commit per change |
| 8 | In a fork, the first push of a new branch opens a change automatically | §4, §5 | Explicit opening only |
| 9 | Task forks live 24 h by default and are removed 1 h after their changes have landed | §4 | Longer defaults |
| 10 | A fork owned by a read-only org member does not receive the parent workspace's secrets | §11 | No parent secrets for any fork |
| 11 | Org members can fork and open changes against workspaces they can only read, and can land their own change when policy is satisfied (`land.by_author`) | §3, §7, §8 | Members may propose but never land |
| 12 | Overlap is computed per file, and awareness never blocks | §9 | Optional exclusive claims |
| 13 | Events are delivered by resumable stream and long-poll only | §14 | Outbound webhooks in the first release |
| 14 | No compatibility with the earlier hub: API, CLI, configuration names and data files all change | §1 | A one-time import tool for existing deployments |
| 15 | This document stays one file | — | Split into one file per section over a shared core |

---

## Appendix A: REST route index

Generated from the route headings of §3–§14: 205 routes. Paths under `/api/v1` are shown in full.

| Method | Path | Section |
|---|---|---|
| GET | `/healthz` | §3 |
| GET | `/readyz` | §3 |
| GET | `/version` | §3 |
| GET | `/metrics` | §3 |
| GET | `/api/v1/auth/providers` | §3 |
| POST | `/api/v1/auth/callback` | §3 |
| GET | `/api/v1/user` | §3 |
| PATCH | `/api/v1/user` | §3 |
| GET | `/api/v1/user/orgs` | §3 |
| GET | `/api/v1/user/keys` | §3 |
| POST | `/api/v1/user/keys/:key_id/refresh` | §3 |
| DELETE | `/api/v1/user/keys/:key_id` | §3 |
| POST | `/api/v1/user/tokens` | §3 |
| GET | `/api/v1/user/tokens` | §3 |
| GET | `/api/v1/user/tokens/:token_id` | §3 |
| DELETE | `/api/v1/user/tokens/:token_id` | §3 |
| PUT | `/api/v1/user/tokens/:token_id/permissions` | §3 |
| PATCH | `/api/v1/user/tokens/:token_id/permissions` | §3 |
| DELETE | `/api/v1/user/tokens/:token_id/permissions` | §3 |
| POST | `/api/v1/users` | §3 |
| GET | `/api/v1/users` | §3 |
| GET | `/api/v1/users/:id` | §3 |
| PATCH | `/api/v1/users/:id` | §3 |
| DELETE | `/api/v1/users/:id` | §3 |
| POST | `/api/v1/users/:id/promote` | §3 |
| POST | `/api/v1/users/:id/demote` | §3 |
| POST | `/api/v1/users/:id/block` | §3 |
| POST | `/api/v1/users/:id/unblock` | §3 |
| GET | `/api/v1/users/:id/keys` | §3 |
| DELETE | `/api/v1/users/:id/keys/:key_id` | §3 |
| GET | `/api/v1/users/:id/tokens` | §3 |
| DELETE | `/api/v1/users/:id/tokens/:token_id` | §3 |
| POST | `/api/v1/orgs` | §3 |
| GET | `/api/v1/orgs` | §3 |
| GET | `/api/v1/orgs/:id` | §3 |
| PATCH | `/api/v1/orgs/:id` | §3 |
| DELETE | `/api/v1/orgs/:id` | §3 |
| POST | `/api/v1/orgs/:id/block` | §3 |
| POST | `/api/v1/orgs/:id/unblock` | §3 |
| GET | `/api/v1/orgs/:id/members` | §3 |
| PUT | `/api/v1/orgs/:id/members/:user_id` | §3 |
| DELETE | `/api/v1/orgs/:id/members/:user_id` | §3 |
| POST | `/api/v1/workspaces/:slug/tokens` | §3 |
| GET | `/api/v1/workspaces/:slug/tokens` | §3 |
| POST | `/api/v1/workspaces` | §4 |
| GET | `/api/v1/workspaces` | §4 |
| GET | `/api/v1/workspaces/:slug` | §4 |
| PATCH | `/api/v1/workspaces/:slug` | §4 |
| POST | `/api/v1/workspaces/:slug/archive` | §4 |
| POST | `/api/v1/workspaces/:slug/reactivate` | §4 |
| POST | `/api/v1/workspaces/:slug/sync` | §4 |
| POST | `/api/v1/workspaces/:slug/reclone` | §4 |
| POST | `/api/v1/workspaces/:slug/branches/:name/push` | §4 |
| DELETE | `/api/v1/workspaces/:slug` | §4 |
| POST | `/api/v1/workspaces/:slug/forks` | §4 |
| GET | `/api/v1/workspaces/:slug/forks` | §4 |
| GET | `/git/:org/:slug.git/info/refs` | §5 |
| POST | `/git/:org/:slug.git/git-upload-pack` | §5 |
| POST | `/git/:org/:slug.git/git-receive-pack` | §5 |
| GET | `/api/v1/workspaces/:slug/scm/refs` | §6 |
| GET | `/api/v1/workspaces/:slug/scm/refs/*name` | §6 |
| POST | `/api/v1/workspaces/:slug/scm/refs` | §6 |
| PATCH | `/api/v1/workspaces/:slug/scm/refs/*name` | §6 |
| DELETE | `/api/v1/workspaces/:slug/scm/refs/*name` | §6 |
| GET | `/api/v1/workspaces/:slug/scm/commits` | §6 |
| GET | `/api/v1/workspaces/:slug/scm/commits/:sha` | §6 |
| POST | `/api/v1/workspaces/:slug/scm/commits` | §6 |
| GET | `/api/v1/workspaces/:slug/scm/tree` | §6 |
| GET | `/api/v1/workspaces/:slug/scm/file` | §6 |
| GET | `/api/v1/workspaces/:slug/scm/diff` | §6 |
| GET | `/api/v1/workspaces/:slug/scm/compare` | §6 |
| GET | `/api/v1/workspaces/:slug/scm/blame` | §6 |
| GET | `/api/v1/workspaces/:slug/scm/search` | §6 |
| POST | `/api/v1/workspaces/:slug/scm/rebase` | §6 |
| POST | `/api/v1/workspaces/:slug/changes` | §7 |
| GET | `/api/v1/workspaces/:slug/changes` | §7 |
| GET | `/api/v1/workspaces/:slug/changes/:number` | §7 |
| GET | `/api/v1/workspaces/:slug/changes/by-commit/:sha` | §7 |
| PATCH | `/api/v1/workspaces/:slug/changes/:number` | §7 |
| POST | `/api/v1/workspaces/:slug/changes/:number/abandon` | §7 |
| POST | `/api/v1/workspaces/:slug/changes/:number/restore` | §7 |
| POST | `/api/v1/workspaces/:slug/changes/:number/restack` | §7 |
| GET | `/api/v1/workspaces/:slug/changes/:number/versions` | §7 |
| GET | `/api/v1/workspaces/:slug/changes/:number/versions/:version` | §7 |
| GET | `/api/v1/workspaces/:slug/changes/:number/diff` | §7 |
| GET | `/api/v1/workspaces/:slug/changes/:number/stack` | §7 |
| POST | `/api/v1/workspaces/:slug/changes/:number/land` | §8 |
| GET | `/api/v1/workspaces/:slug/changes/:number/lands` | §8 |
| GET | `/api/v1/workspaces/:slug/lands` | §8 |
| GET | `/api/v1/workspaces/:slug/lands/:id` | §8 |
| GET | `/api/v1/workspaces/:slug/changes/:number/conflict` | §8 |
| GET | `/api/v1/workspaces/:slug/changes/:number/conflict/file` | §8 |
| GET | `/api/v1/workspaces/:slug/policy` | §8 |
| PUT | `/api/v1/workspaces/:slug/policy` | §8 |
| POST | `/api/v1/workspaces/:slug/policy/evaluate` | §8 |
| POST | `/api/v1/workspaces/:slug/changes/:number/approvals` | §8 |
| GET | `/api/v1/workspaces/:slug/changes/:number/approvals` | §8 |
| DELETE | `/api/v1/workspaces/:slug/changes/:number/approvals/:id` | §8 |
| PUT | `/api/v1/workspaces/:slug/changes/:number/checks/:name` | §8 |
| GET | `/api/v1/workspaces/:slug/changes/:number/checks` | §8 |
| POST | `/api/v1/workspaces/:slug/changes/:number/revert` | §8 |
| GET | `/api/v1/workspaces/:slug/activity` | §9 |
| GET | `/api/v1/workspaces/:slug/changes/:number/overlaps` | §9 |
| POST | `/api/v1/workspaces/:slug/claims` | §9 |
| GET | `/api/v1/workspaces/:slug/claims` | §9 |
| GET | `/api/v1/workspaces/:slug/claims/:id` | §9 |
| PATCH | `/api/v1/workspaces/:slug/claims/:id` | §9 |
| DELETE | `/api/v1/workspaces/:slug/claims/:id` | §9 |
| POST | `/api/v1/workspaces/:slug/patches` | §10 |
| GET | `/api/v1/workspaces/:slug/patches` | §10 |
| GET | `/api/v1/workspaces/:slug/patches/:id` | §10 |
| PATCH | `/api/v1/workspaces/:slug/patches/:id` | §10 |
| DELETE | `/api/v1/workspaces/:slug/patches/:id` | §10 |
| POST | `/api/v1/workspaces/:slug/patches/:id/restore` | §10 |
| POST | `/api/v1/workspaces/:slug/patches/:id/reset-to-origin` | §10 |
| POST | `/api/v1/workspaces/:slug/patches/reorder` | §10 |
| GET | `/api/v1/workspaces/:slug/patch-status` | §10 |
| GET | `/api/v1/workspaces/:slug/rerere` | §10 |
| DELETE | `/api/v1/workspaces/:slug/rerere/*pathspec` | §10 |
| POST | `/api/v1/workspaces/:slug/rebuild` | §10 |
| GET | `/api/v1/workspaces/:slug/rebuilds` | §10 |
| GET | `/api/v1/workspaces/:slug/rebuilds/:id` | §10 |
| DELETE | `/api/v1/workspaces/:slug/rebuilds/:id` | §10 |
| POST | `/api/v1/workspaces/:slug/rebuilds/:id/requeue` | §10 |
| POST | `/api/v1/workspaces/:slug/rebuilds/:id/rollback` | §10 |
| GET | `/api/v1/workspaces/:slug/rebuild-preview` | §10 |
| POST | `/api/v1/user/secrets` | §11 |
| GET | `/api/v1/user/secrets` | §11 |
| PATCH | `/api/v1/user/secrets/:key` | §11 |
| DELETE | `/api/v1/user/secrets/:key` | §11 |
| POST | `/api/v1/orgs/:slug/secrets` | §11 |
| GET | `/api/v1/orgs/:slug/secrets` | §11 |
| PATCH | `/api/v1/orgs/:slug/secrets/:key` | §11 |
| DELETE | `/api/v1/orgs/:slug/secrets/:key` | §11 |
| POST | `/api/v1/workspaces/:slug/secrets` | §11 |
| GET | `/api/v1/workspaces/:slug/secrets` | §11 |
| PATCH | `/api/v1/workspaces/:slug/secrets/:key` | §11 |
| DELETE | `/api/v1/workspaces/:slug/secrets/:key` | §11 |
| POST | `/api/v1/user/vars` | §11 |
| GET | `/api/v1/user/vars` | §11 |
| PATCH | `/api/v1/user/vars/:key` | §11 |
| DELETE | `/api/v1/user/vars/:key` | §11 |
| POST | `/api/v1/orgs/:slug/vars` | §11 |
| GET | `/api/v1/orgs/:slug/vars` | §11 |
| PATCH | `/api/v1/orgs/:slug/vars/:key` | §11 |
| DELETE | `/api/v1/orgs/:slug/vars/:key` | §11 |
| POST | `/api/v1/workspaces/:slug/vars` | §11 |
| GET | `/api/v1/workspaces/:slug/vars` | §11 |
| PATCH | `/api/v1/workspaces/:slug/vars/:key` | §11 |
| DELETE | `/api/v1/workspaces/:slug/vars/:key` | §11 |
| GET | `/api/v1/workspaces/:slug/vars/resolved` | §11 |
| GET | `/api/v1/workspaces/:slug/forge` | §12 |
| GET | `/api/v1/workspaces/:slug/forge/repository` | §12 |
| GET | `/api/v1/workspaces/:slug/issues` | §12 |
| GET | `/api/v1/workspaces/:slug/issues/:number` | §12 |
| GET | `/api/v1/workspaces/:slug/issues/:number/comments` | §12 |
| GET | `/api/v1/workspaces/:slug/pulls/:number` | §12 |
| GET | `/api/v1/workspaces/:slug/pulls/:number/state` | §12 |
| GET | `/api/v1/workspaces/:slug/pulls/:number/files` | §12 |
| GET | `/api/v1/workspaces/:slug/pulls/:number/checks` | §12 |
| GET | `/api/v1/workspaces/:slug/pulls/:number/reviews` | §12 |
| POST | `/api/v1/workspaces/:slug/issues` | §12 |
| PATCH | `/api/v1/workspaces/:slug/issues/:number` | §12 |
| POST | `/api/v1/workspaces/:slug/issues/:number/close` | §12 |
| POST | `/api/v1/workspaces/:slug/issues/:number/comments` | §12 |
| POST | `/api/v1/workspaces/:slug/issues/:number/labels` | §12 |
| DELETE | `/api/v1/workspaces/:slug/issues/:number/labels/:name` | §12 |
| POST | `/api/v1/workspaces/:slug/labels` | §12 |
| POST | `/api/v1/workspaces/:slug/pulls` | §12 |
| POST | `/api/v1/workspaces/:slug/pulls/:number/comments` | §12 |
| PUT | `/api/v1/workspaces/:slug/forge/webhook` | §12 |
| DELETE | `/api/v1/workspaces/:slug/forge/webhook` | §12 |
| GET | `/api/v1/workspaces/:slug/forge/deliveries` | §12 |
| POST | `/hooks/forge/:slug` | §12 |
| GET | `/api/v1/sandbox-profiles` | §13 |
| POST | `/api/v1/workspaces/:slug/sandboxes` | §13 |
| GET | `/api/v1/workspaces/:slug/sandboxes` | §13 |
| GET | `/api/v1/workspaces/:slug/sandboxes/:id` | §13 |
| DELETE | `/api/v1/workspaces/:slug/sandboxes/:id` | §13 |
| GET | `/api/v1/sandboxes` | §13 |
| POST | `/api/v1/sessions` | §14 |
| GET | `/api/v1/sessions` | §14 |
| GET | `/api/v1/sessions/:id` | §14 |
| POST | `/api/v1/sessions/:id/complete` | §14 |
| POST | `/api/v1/sessions/:id/usage` | §14 |
| GET | `/api/v1/sessions/:id/usage` | §14 |
| GET | `/api/v1/workspaces/:slug/cost` | §14 |
| POST | `/api/v1/workspaces/:slug/runs/:run_id/events` | §14 |
| POST | `/api/v1/workspaces/:slug/runs/:run_id/events/batch` | §14 |
| POST | `/api/v1/workspaces/:slug/runs/:run_id/sessions/outcomes` | §14 |
| POST | `/api/v1/workspaces/:slug/runs/:run_id/tools/calls` | §14 |
| POST | `/api/v1/workspaces/:slug/runs/:run_id/tools/errors` | §14 |
| POST | `/api/v1/workspaces/:slug/runs/:run_id/traces` | §14 |
| POST | `/api/v1/workspaces/:slug/runs/:run_id/traces/batch` | §14 |
| POST | `/api/v1/workspaces/:slug/runs/:run_id/postmortem` | §14 |
| GET | `/api/v1/workspaces/:slug/runs/:run_id/events` | §14 |
| GET | `/api/v1/workspaces/:slug/runs/:run_id/sessions/outcomes` | §14 |
| GET | `/api/v1/workspaces/:slug/runs/:run_id/tools/calls` | §14 |
| GET | `/api/v1/workspaces/:slug/runs/:run_id/tools/errors` | §14 |
| GET | `/api/v1/workspaces/:slug/runs/:run_id/traces` | §14 |
| GET | `/api/v1/workspaces/:slug/runs/:run_id/postmortem` | §14 |
| GET | `/api/v1/workspaces/:slug/runs/:run_id/transcript` | §14 |
| GET | `/api/v1/audit` | §14 |
| GET | `/api/v1/events` | §14 |
| GET | `/api/v1/events/log` | §14 |

---
## Appendix B: CLI command index

Generated from the command headings of §3–§14: 163 commands. Flags, calls, output and exit codes are in the section named.

| Command | Section |
|---|---|
| `parley login` | §3 |
| `parley user show` | §3 |
| `parley user update` | §3 |
| `parley keys list` | §3 |
| `parley keys refresh` | §3 |
| `parley keys revoke` | §3 |
| `parley tokens list` | §3 |
| `parley tokens create` | §3 |
| `parley tokens show <token_id>` | §3 |
| `parley tokens revoke <token_id>` | §3 |
| `parley tokens replace <token_id>` | §3 |
| `parley tokens add <token_id>` | §3 |
| `parley tokens remove <token_id>` | §3 |
| `parley orgs list` | §3 |
| `parley orgs show <org>` | §3 |
| `parley orgs members <org>` | §3 |
| `parley admin users list` | §3 |
| `parley admin users show <user>` | §3 |
| `parley admin users create` | §3 |
| `parley admin users update <user>` | §3 |
| `parley admin users delete <user>` | §3 |
| `parley admin users promote <user>` | §3 |
| `parley admin users demote <user>` | §3 |
| `parley admin users block <user>` | §3 |
| `parley admin users unblock <user>` | §3 |
| `parley admin orgs list` | §3 |
| `parley admin orgs create` | §3 |
| `parley admin orgs update <org>` | §3 |
| `parley admin orgs delete <org>` | §3 |
| `parley admin orgs block <org>` | §3 |
| `parley admin orgs unblock <org>` | §3 |
| `parley admin orgs members list <org>` | §3 |
| `parley admin orgs members add <org> <user>` | §3 |
| `parley admin orgs members remove <org> <user>` | §3 |
| `parley admin keys list <user>` | §3 |
| `parley admin keys revoke <user> <key_id>` | §3 |
| `parley admin tokens list <user>` | §3 |
| `parley admin tokens revoke <user> <token_id>` | §3 |
| `parley version` | §3 |
| `parley help [command]` | §3 |
| `parley workspace create` | §4 |
| `parley workspace list` | §4 |
| `parley workspace get <slug>` | §4 |
| `parley workspace update <slug>` | §4 |
| `parley workspace archive <slug>` | §4 |
| `parley workspace reactivate <slug>` | §4 |
| `parley workspace sync <slug>` | §4 |
| `parley workspace reclone <slug>` | §4 |
| `parley workspace push <slug> <branch>` | §4 |
| `parley workspace delete <slug>` | §4 |
| `parley fork create <parent>` | §4 |
| `parley fork list <parent>` | §4 |
| `parley fork extend <slug>` | §4 |
| `parley fork delete <slug>` | §4 |
| `parley credential-helper <get|store|erase>` | §5 |
| `parley scm refs <slug>` | §6 |
| `parley scm log <slug>` | §6 |
| `parley scm show <slug> <rev>` | §6 |
| `parley scm ls <slug> [path]` | §6 |
| `parley scm cat <slug> <path>` | §6 |
| `parley scm diff <slug> <base> <head>` | §6 |
| `parley scm compare <slug> <base> <head>` | §6 |
| `parley scm blame <slug> <path>` | §6 |
| `parley scm search <slug> <query>` | §6 |
| `parley scm commit <slug>` | §6 |
| `parley scm rebase <slug> <branch>` | §6 |
| `parley change open <slug>` | §7 |
| `parley change list <slug>` | §7 |
| `parley change show <slug> <number>` | §7 |
| `parley change find <slug> <sha>` | §7 |
| `parley change edit <slug> <number>` | §7 |
| `parley change ready <slug> <number>` | §7 |
| `parley change draft <slug> <number>` | §7 |
| `parley change abandon <slug> <number>` | §7 |
| `parley change restore <slug> <number>` | §7 |
| `parley change restack <slug> <number>` | §7 |
| `parley change versions <slug> <number>` | §7 |
| `parley change diff <slug> <number>` | §7 |
| `parley change stack <slug> <number>` | §7 |
| `parley change wait <slug> <number>` | §7 |
| `parley change land <slug> <number>` | §8 |
| `parley change lands <slug> <number>` | §8 |
| `parley land list <slug>` | §8 |
| `parley land show <slug> <land-id>` | §8 |
| `parley change conflict <slug> <number>` | §8 |
| `parley change resolve <slug> <number>` | §8 |
| `parley change approve <slug> <number>` | §8 |
| `parley change reject <slug> <number>` | §8 |
| `parley change approvals <slug> <number>` | §8 |
| `parley change check <slug> <number> <name>` | §8 |
| `parley change checks <slug> <number>` | §8 |
| `parley change revert <slug> <number>` | §8 |
| `parley policy get <slug>` | §8 |
| `parley policy set <slug>` | §8 |
| `parley policy evaluate <slug>` | §8 |
| `parley activity <slug>` | §9 |
| `parley change overlaps <slug> <number>` | §9 |
| `parley claim create <slug>` | §9 |
| `parley claim list <slug>` | §9 |
| `parley claim renew <slug> <id>` | §9 |
| `parley claim release <slug> <id>` | §9 |
| `parley patch add <workspace-slug>` | §10 |
| `parley patch list <workspace-slug>` | §10 |
| `parley patch show <workspace-slug> <patch-id>` | §10 |
| `parley patch update <workspace-slug> <patch-id>` | §10 |
| `parley patch remove <workspace-slug> <patch-id>` | §10 |
| `parley patch restore <workspace-slug> <patch-id>` | §10 |
| `parley patch reorder <workspace-slug> <patch-id>...` | §10 |
| `parley patch reset-to-origin <workspace-slug> <patch-id>` | §10 |
| `parley rebuild submit <workspace-slug>` | §10 |
| `parley rebuild list <workspace-slug>` | §10 |
| `parley rebuild status <workspace-slug> <rebuild-id>` | §10 |
| `parley rebuild preview <workspace-slug>` | §10 |
| `parley rebuild cancel <workspace-slug> <rebuild-id>` | §10 |
| `parley rebuild requeue <workspace-slug> <rebuild-id>` | §10 |
| `parley rebuild rollback <workspace-slug> <rebuild-id>` | §10 |
| `parley rerere list <workspace-slug>` | §10 |
| `parley rerere forget <workspace-slug> <id-or-path>` | §10 |
| `parley workspace patch-status <workspace-slug>` | §10 |
| `parley secrets create <KEY=VALUE[,KEY=VALUE…]>… | <KEY> --from-stdin` | §11 |
| `parley secrets list` | §11 |
| `parley secrets update <KEY=VALUE> | <KEY> --from-stdin` | §11 |
| `parley secrets delete <KEY>` | §11 |
| `parley vars create <KEY=VALUE[,KEY=VALUE…]>… | <KEY> --from-stdin` | §11 |
| `parley vars list` | §11 |
| `parley vars update <KEY=VALUE> | <KEY> --from-stdin` | §11 |
| `parley vars delete <KEY>` | §11 |
| `parley vars resolve <workspace-slug>` | §11 |
| `parley forge status <workspace-slug>` | §12 |
| `parley forge repository <workspace-slug>` | §12 |
| `parley forge webhook set <workspace-slug>` | §12 |
| `parley forge webhook unset <workspace-slug>` | §12 |
| `parley forge deliveries <workspace-slug>` | §12 |
| `parley issue list <workspace-slug>` | §12 |
| `parley issue get <workspace-slug> <number>` | §12 |
| `parley issue comments <workspace-slug> <number>` | §12 |
| `parley issue create <workspace-slug>` | §12 |
| `parley issue update <workspace-slug> <number>` | §12 |
| `parley issue close <workspace-slug> <number>` | §12 |
| `parley issue comment <workspace-slug> <number>` | §12 |
| `parley issue label <workspace-slug> <number> <label>...` | §12 |
| `parley issue unlabel <workspace-slug> <number> <label>` | §12 |
| `parley label create <workspace-slug> <name>` | §12 |
| `parley pr get <workspace-slug> <number>` | §12 |
| `parley pr state <workspace-slug> <number>` | §12 |
| `parley pr files <workspace-slug> <number>` | §12 |
| `parley pr checks <workspace-slug> <number>` | §12 |
| `parley pr reviews <workspace-slug> <number>` | §12 |
| `parley pr create <workspace-slug>` | §12 |
| `parley pr comment <workspace-slug> <number>` | §12 |
| `parley sandbox profiles` | §13 |
| `parley sandbox create <workspace-slug>` | §13 |
| `parley sandbox list <workspace-slug>` | §13 |
| `parley sandbox get <workspace-slug> <lease-id>` | §13 |
| `parley sandbox destroy <workspace-slug> <lease-id>` | §13 |
| `parley session start <workspace-slug>` | §14 |
| `parley session usage <session-id>` | §14 |
| `parley session complete <session-id>` | §14 |
| `parley session get <session-id>` | §14 |
| `parley session list` | §14 |
| `parley audit query` | §14 |
| `parley events tail` | §14 |
| `parley events list` | §14 |
