# Parley — Product Requirements Document

Parley — the hub where agents negotiate with your code.

## Contents

| § | Section | File |
|---|---|---|
| 1 | Product overview | this file |
| 2 | Personas, glossary and conventions | this file |
| 3 | Platform, identity and access | [03_platform.md](03_platform.md) |
| 4 | Workspaces and task forks | [04_workspaces_and_forks.md](04_workspaces_and_forks.md) |
| 5 | Git server | [05_git_server.md](05_git_server.md) |
| 6 | Source control API | [06_source_control_api.md](06_source_control_api.md) |
| 7 | Changes and stacks | [07_changes_and_stacks.md](07_changes_and_stacks.md) |
| 8 | Landing and policy | [08_landing_and_policy.md](08_landing_and_policy.md) |
| 9 | Awareness | [09_awareness.md](09_awareness.md) |
| 10 | Carry-patch workspaces | [10_carry_patch.md](10_carry_patch.md) |
| 11 | Secrets and variables | [11_secrets_and_variables.md](11_secrets_and_variables.md) |
| 12 | Forge integration | [12_forge.md](12_forge.md) |
| 13 | Sandboxes | [13_sandboxes.md](13_sandboxes.md) |
| 14 | Sessions, audit, events and metrics | [14_sessions_audit_events.md](14_sessions_audit_events.md) |
| 15 | Non-functional requirements | [15_non_functional.md](15_non_functional.md) |
| 16 | Acceptance criteria | [16_acceptance.md](16_acceptance.md) |
| 17 | Sequencing and decisions | [17_sequencing_and_decisions.md](17_sequencing_and_decisions.md) |
| A | REST route index | [appendix_a_routes.md](appendix_a_routes.md) |
| B | CLI command index | [appendix_b_cli.md](appendix_b_cli.md) |

Sections refer to each other by number ("§7", "§8.3"). Requirement IDs (`CHG-FR-012`) are unique across all files.

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
3. **Repositories are cheap.** A task fork is created in one call, without copying history, together with a token that works only there. It expires by itself: after 7 days by default, or 24 hours after its work has landed (§4).
4. **Source control is an API.** Files, trees, history, diffs and blame can be read, and commits created, without a clone. Git over HTTP stays available for everything else (§5, §6).
5. **Landing is optimistic.** Landing is a server-side rebase and a compare-and-swap on the target branch. Changes that do not conflict never wait for each other. A conflict comes back as data an agent can act on (§8).
6. **Policy decides what may land, by path.** Rules per workspace say which paths land automatically, which need checks and which need a person. Tokens can be limited to paths (§8).
7. **Agents can see each other.** One call answers which in-flight changes and claims touch a set of paths (§9).
8. **Events are durable.** Every push, fork, version, land and conflict is an ordered, replayable event, so review agents and CI can subscribe reliably. Consumers pull events from a resumable stream or a long-poll, or have the hub push them to a URL as signed event webhooks (§14).

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

  It also sends event webhooks: HTTP `POST`s of hub events to the URLs that workspace owners subscribe (§14).
- **`parley`** is the command-line client. It speaks only the REST API, and everything it does can also be done over REST.

| Capability | What it gives the user | Section |
|---|---|---|
| Identity and scoped access | GitHub OAuth sign-in, API keys and personal access tokens (PATs) with read/write scopes, PATs bound to one workspace and limited to paths, organizations, an admin account | §3 |
| Workspaces | Register a repository once. The hub clones it, keeps it in sync with its remote, serves it over git and pushes branches back | §4, §5 |
| Task forks | One call gives an agent its own copy of a workspace and a token for it. The fork lives 7 days by default and is removed 24 hours after its work has landed | §4 |
| Source control API | Read files, trees, history, diffs and blame, search, and create commits and move refs, without a clone | §6 |
| Changes and stacks | Stable, versioned units of work with intent, linked to sessions, optionally stacked | §7 |
| Landing and policy | Optimistic server-side landing, structured conflict reports, path-based rules for approvals and checks, automatic landing, one-call revert | §8 |
| Awareness | See which in-flight changes and claims touch the paths you are about to edit | §9 |
| Carry-patch workspaces | Maintain a long-lived set of patches on top of an upstream. When upstream moves, the hub rebuilds the integration branch and remembers conflict resolutions | §10 |
| Secrets and variables | Stored per user, organization or workspace. Secret values can be written but never read back | §11 |
| Forge access | Read and write issues and pull requests on GitHub or GitLab through the hub, using the workspace's own credential. Receive the forge's webhooks as hub events | §12 |
| Sandboxes | An isolated environment, running on OpenShell, born attached to a task fork with the fork checked out and shaped by an operator-defined profile. It has no real credentials inside by default, has no network egress by default, and its work is checkpointed to the fork. An operator can expose named secrets to a profile's sandboxes, and an owner can keep an approved network rule for a whole workspace | §13 |
| Accounting, audit and events | Agent sessions with token usage, agent run telemetry, an audit log, a durable and replayable event stream, event webhooks that deliver the same events to a URL, signed, in order and with retries, Prometheus metrics | §14 |

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
# → { "workspace": { "slug": "demo--greeting", … }, "remote": "http://localhost:8080/git/acme/demo--greeting.git", "token": { "token_id": "…", "token": "parley_pat_…", … } }

parley activity demo --paths 'api/**'                      # is anyone else working here?

git clone --filter=blob:none "$REMOTE" && cd demo--greeting
git switch -c greeting && …commit…
git push -u origin greeting \
    -o parley.title="Add greeting endpoint" \
    -o parley.intent="Issue #41 asks for a health-style greeting route"
# remote: change demo#12 version 1 (opened)
```

The change lands when policy allows it. If someone else landed first and the two changes do not conflict, nothing waits:

```sh
parley change land demo 12 --wait
# → { "status": "landed", "landed_sha": "…", "attempts": 2 }
parley workspace push demo main                            # publish main from the hub to GitHub
```

An agent with no local checkout can do the same through the API alone: `POST …/scm/commits` to create the commit, `POST …/changes` to open the change, `POST …/changes/12/land` to land it.

When the hub has sandboxes enabled (§13), the fork can come with a sandbox that has the fork checked out, and the agent runs inside it:

```sh
parley fork create demo --name greeting --sandbox-profile agent -- my-agent --task "Add a greeting endpoint"
parley sandbox exec demo--greeting "$SANDBOX_ID" -- git status   # look at the work from outside
```

### 1.5 System context

```
  parley, HTTP clients      git clients        GitHub / GitLab
         │                   │              webhooks │
         ▼                   ▼                       ▼
 ┌─────────────────────── parleyd: HTTP listener ──────────────────────────┐
 │ /api/v1/...            identity, workspaces and forks, source control   │       ┌───────────────────┐
 │                        API, changes, landing, policy, awareness,        │       │ OpenShell gateway │
 │                        secrets, forge, sandboxes, sessions, events      │ ────▶ │ (optional, §13)   │
 │ /git/<org>/<slug>.git  git smart HTTP (v2), push rules, push → change   │       └─────────┬─────────┘
 │ /hooks/forge/:slug     webhook signature → delivery log → hub event     │                 ▼
 │ /metrics, /healthz, /readyz, /version                                   │             sandboxes
 └──────┬───────────────────┬─────────────────────┬────────────────────────┘
        ▼                   ▼                     ▼
   hub state            event log and        repositories (bare),
   (identity, changes,  audit store          forks share objects
   policy, jobs, …)                          with their parent
        └── forge REST API ──▶ GitHub / GitLab          git push/fetch ──▶ origin, upstream
        └── event webhooks (signed POST per event, §14) ──▶ subscribers' URLs
```

### 1.6 Goals

1. **Agent-first, git-native.** An agent can do everything through the REST API and stock `git`. No custom client is required on the agent's side.
2. **No waiting on other agents.** Work that does not conflict proceeds in parallel at every step: forking, pushing, landing.
3. **Legible to an agent.** Every refusal, conflict and policy decision is returned as structured data that says what to do next.
4. **Reviewable by intent.** People review what a change is for and whether policy was met, and read lines only where policy says they must.
5. **Traceable.** Every commit the hub adds to a protected branch can be traced to a change, its versions, its approvals and the session that produced it, or to a recorded direct push by the workspace's owner or the admin token (§8).
6. **Simple to run.** One server process, embedded storage, one configuration file. No external database or message broker. The only optional external component is the OpenShell gateway that runs sandboxes (§13).

### 1.7 Non-goals

- **Replacing git.** Parley does not define a new storage format, a virtual filesystem or a custom wire protocol.
- **Hyperscale.** Several replicas, regional caches and repositories beyond what one host can serve are out of scope (§15).
- **A code review user interface.** Parley provides the review data model and API (changes, versions, approvals, checks). It ships no web interface.
- **Built-in CI.** Checks are reported to the hub by external runners. The hub does not run builds or tests itself.
- **Sandbox isolation of its own.** Sandboxes run on OpenShell, which provides the isolation and enforces policy. Parley is the control plane on top and implements no isolation itself (§13).
- **A built-in issue tracker or message board.** Issues stay on the forge (§12). Coordination between agents happens through changes, claims and events (§9, §14).
- **Compatibility with the earlier parley.** The earlier hub gave Parley its shape. Its API, CLI, configuration names and data files are not supported.

### 1.8 Prior art

| Source | What Parley takes from it |
|---|---|
| Cloudflare Artifacts | Repositories as cheap, programmable primitives; one fork per task with a scoped token; repository events as triggers; recording why a change was made |
| Anthropic's Scion and its review tool | Source control as a service with server-side commits, rebases and history queries; optional working copies; compare-and-swap ref updates instead of a globally locked merge queue; review of intent with policy by path; directory-level permissions |
| Sapling | Stacks without branch juggling; automatic restack; recorded rewrite history; server-side rebase on push |
| Twigg | Changes with stable identifiers and versions, each reviewed on its own; per-directory ownership; mirroring landed work out to plain git |
| AgentHub | A coordination surface for agent swarms; per-agent limits |
| OpenShell | The sandbox runtime itself; placeholder credentials that are resolved outside the sandbox; a network policy that denies by default and allows per program; proposals for new access |

Parley deliberately does not take Scion's storage engine, AgentHub's branchless graph or Twigg's built-in tracker and CI.

---

## 2. Personas, glossary and conventions

### 2.1 Personas

| Persona | Who | Typical credential | Main goals |
|---|---|---|---|
| **Operator** | Runs and administers the hub | Admin token (written at first boot) | Deploy and configure the hub; manage users, orgs, keys and tokens; connect the hub to an OpenShell gateway and define sandbox profiles and credentials; watch metrics and the audit log |
| **Developer** | A person who registers repositories and directs agents | API key, from `parley login` | Create workspaces; write landing policy; approve the changes that need a person; maintain carry-patch workspaces; manage secrets; mint scoped tokens for agents |
| **Agent** | An AI coding agent running on a developer's behalf | PAT bound to one workspace or task fork, often limited to paths | Fork, read and write code through git or the API; open changes and state their intent; fix conflicts; land; report sessions, usage and telemetry; create a sandbox on its fork, work in it through the REST API and ask for more network access when blocked |
| **Review agent / check runner** | Automation that reacts to changes | PAT with `changes:read`, `audit:read` and `checks:write`, possibly `changes:approve`; to report checks it belongs to a user with write access to the workspace (§8) | Follow the event stream or receive event webhooks; report check results; approve or reject versions where policy allows |
| **Org member** | A user who belongs to the organization that owns a workspace | API key or PAT | Read the org's workspaces; contribute through a task fork and a change |
| **Forge** | GitHub or GitLab | Webhook secret | Deliver webhooks that the hub turns into hub events |

### 2.2 Glossary

| Term | Meaning |
|---|---|
| Hub | The `parleyd` server |
| Workspace | A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork |
| Origin | A registered workspace's remote (`git_url`) |
| Task fork (fork) | A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires: after 7 days by default, or 24 hours after all its changes have landed or been abandoned |
| Tracking branch | In a fork, a branch that existed in the parent at fork time. It is read-only in the fork and always equals the parent's branch |
| Protected branch | A branch that moves only by landing a change, by sync from origin, or by a direct fast-forward push from the workspace's owner or the admin token (§8). Nobody can force-push or delete it. By default the default branch |
| Direct push | A fast-forward update of a protected branch by the workspace's owner or the admin token, over git or the source control API, without a change. Allowed while the workspace setting `protect.owner_push` is true (§8) |
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
| Durable job | Background work that is recorded before its request returns, survives restarts and is retried with backoff. Rebuilds are the only durable jobs (§10). Clone work, restacks and land attempts are also durable but follow their own rules and are not retried with backoff (§4, §7, §8) |
| Scope / permission | A string carried by a PAT that grants read or write access to an area |
| Bound token | A PAT confined to one workspace (and the forks created with it). A token bound to a fork also reaches its own changes, and the awareness and policy reads, on the fork's parent (§3) |
| Path-scoped token | A PAT whose writes are limited to files matching a list of globs |
| Sandbox | An isolated environment in which an agent runs, attached to a workspace (normally a task fork) and started with it checked out. It runs on OpenShell; the hub controls it (§13) |
| OpenShell | The open-source sandbox runtime Parley's sandboxes run on. It provides the isolation, the policy format and the credential mechanism. The hub is a client of one OpenShell gateway |
| Profile | The operator's template for a sandbox, defined in configuration: image, resources, time limits, base policy, credentials and the secrets it exposes |
| Exposed secret | A secret (§11) that a profile names in `expose_secrets`. Its real value is placed in the sandbox's environment, where code in the sandbox can read it. Off unless the operator lists the secret (§13) |
| Credential (sandbox) | A secret (§11) attached to a sandbox as an OpenShell provider. The workload sees only a placeholder; the runtime adds the real value outside the sandbox, on requests to the endpoints the provider's profile authorizes |
| Hub credential | The fork-bound PAT the hub mints for a sandbox so that it can reach the hub. The workload sees only its placeholder, in `PARLEY_API_KEY` |
| Checkpoint | The git state of a sandbox's working directory, saved to the workspace under `refs/parley/sandboxes/<id>/<branch>` |
| Proposal (network access) | A request from a sandbox's workload for an additional network rule. A person approves or rejects it; an approved rule applies to that sandbox only, unless the approver promotes it to a workspace sandbox rule |
| Workspace sandbox rule | An approved network rule saved on a workspace. It is added, on top of the profile's policy, to every sandbox created later on that workspace or its forks (§13) |
| Session | An agent session: who ran, on which workspace, with what token usage, and which changes it produced |
| Hub event | A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream |
| Event webhook | A workspace's subscription that makes the hub send each matching hub event to a URL as a signed HTTP `POST`, in order and with retries (§14). Not to be confused with the forge's webhooks, which the hub receives (§12) |
| Agent telemetry | Run events, tool calls, traces, outcomes and postmortems that agents send to the hub |

### 2.3 Conventions used in this document

- Paths are written relative to the API root `/api/v1` unless they start with `/git`, `/hooks`, `/metrics`, `/healthz`, `/readyz` or `/version`.
- `:param` is a path parameter.
- Requirement IDs (`WS-FR-012`) are stable references for specs and tests. The prefixes are `PLAT`, `WS`, `GIT`, `SCM`, `CHG`, `LAND`, `AWR`, `CP`, `SEC`, `FRG`, `SBX`, `AUD` and `NFR`. User stories use `-US-`.
- "Error `xyz`" means the error envelope (§3) with `error_type` `xyz`.
- **MUST** marks a strict requirement.
- The conventions that hold for every route and command are specified once, in §3: the error envelope, the order of checks, list paging, idempotency keys, timestamps, and the CLI's output and exit codes. Sections state only what differs.
- A commit id (`sha`) is always the full 40-character hexadecimal form in responses. Requests accept any revision the repository can resolve where a field is called `ref`, and only a full id where it is called `sha`.
