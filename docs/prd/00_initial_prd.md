# Hub — Product Requirements Document (re-implementation baseline)

Parley - the hub where agents negotiate with your code.

## Contents

1. [Product overview](#1-product-overview)
2. [Personas, glossary and conventions](#2-personas-glossary-and-conventions)
3. [Platform, identity and access](#3-platform-identity-and-access)
4. [Workspaces](#4-workspaces)
5. [Git server](#5-git-server)
6. [Merge queue and batch rebase](#6-merge-queue-and-batch-rebase)
7. [Carry-patch forks](#7-carry-patch-forks)
8. [Secrets and variables](#8-secrets-and-variables)
9. [Forge integration](#9-forge-integration)
10. [Sandboxes](#10-sandboxes)
11. [Audit, sessions, events and metrics](#11-audit-sessions-events-and-metrics)
12. [Non-functional requirements](#12-non-functional-requirements)
13. [Re-implementation acceptance criteria](#13-re-implementation-acceptance-criteria)
- [Appendix A: REST route index](#appendix-a-rest-route-index)
- [Appendix B: `parley` command reference (generated from the built binary)](#appendix-b-parley-command-reference)
- [Appendix C: Summary of documentation discrepancies](#appendix-c-summary-of-documentation-discrepancies)

---

## 1. Product overview

### 1.1 Problem

When coding agents work on real repositories, the same questions come up every time:

- **Where does the agent's copy of the code live?** And how can several agents work on one repository without getting in each other's way?
- **How do their changes get back to the main branch?** Ideally without a person rebasing and merging every branch by hand.
- **Which credentials does an agent get?** Forge tokens and API keys copied into an agent's environment are hard to scope and harder to revoke.
- **Where does the agent run?** It should be isolated from the machine it was started on.
- **What did the agent do, and what did it cost?**

Without a shared answer, every team writes its own scripts. Tokens end up in environment files, branches get merged by whoever notices them, and the record of what an agent did is spread across terminals.

### 1.2 Product

The hub holds everything an agent run needs except the agent itself:

- the repository
- the credentials
- the secrets
- a sandbox to run in
- a record of what happened and what it cost

It ships as two programs:

- **`parleyd`** is the server. It serves:
  - a REST API under `/api/v1`
  - a git smart-HTTP server under `/git`
  - a forge webhook ingress under `/hooks/forge`
  - Prometheus metrics and health endpoints
  - optionally, an AgentKit-compatible sandbox gateway (gRPC) on a second listener
- **`parley`** is the command-line client. It speaks only the REST API, and everything it does can also be done over REST.

| Capability | What it gives the user | PRD section |
|---|---|---|
| Identity and scoped access | GitHub OAuth sign-in, API keys and personal access tokens (PATs) with read/write scopes, PATs bound to one workspace, organizations whose members get read-only access to the org's workspaces, an admin account | §3 |
| Workspaces | Register a repository once. The hub clones it, keeps it in sync with its remote, serves it over git and pushes branches back to the remote | §4, §5 |
| Merge queue | Submit a branch. The hub rebases it onto the target and fast-forwards, one job at a time per workspace. Conflicts are reported and never half-applied. There is also a synchronous batch rebase | §6 |
| Carry-patch forks | Maintain a fork as an ordered list of patch branches on top of an upstream. When upstream moves, the hub rebuilds the integration branch, detects patches that were merged upstream and remembers conflict resolutions (rerere) | §7 |
| Secrets and variables | Stored per user, organization or workspace. Secret values can be written but never read back. Variables resolve along a precedence chain | §8 |
| Forge access | Read and write issues and pull requests on GitHub or GitLab through the hub, using the workspace's own credential. Receive the forge's webhooks as hub events | §9 |
| Sandboxes | Lease an isolated container shaped by an operator-defined profile, run locally (Podman) or through a remote gateway. The hub injects secrets so the caller never sees them | §10 |
| Accounting and audit | Agent sessions with token usage and cost, ingestion of agent run telemetry, an audit log of every change, a unified audit query, a live event stream (SSE) and Prometheus metrics | §11 |

### 1.3 End-to-end example (must keep working unchanged)

```sh
parley login --endpoint-url http://localhost:8080          # browser-based GitHub sign-in
parley workspace create --slug demo --git-url https://github.com/acme/demo.git \
    --git-pat "$GITHUB_TOKEN" --wait                     # returns once the hub has cloned it; JSON includes hub_url
git config --global credential.http://localhost:8080.helper '!parley credential-helper'
git clone http://localhost:8080/git/acme/demo.git && cd demo
git switch -c feature/greeting && …commit… && git push -u origin feature/greeting
parley merge submit demo --source feature/greeting --target main --wait
parley workspace push demo main                             # push main from the hub to GitHub
```

### 1.4 System context

```
  parley, HTTP clients      git clients        GitHub / GitLab        AgentKit clients
         │                   │              webhooks │                    │
         ▼                   ▼                       ▼                    ▼
 ┌─────────────────────── parleyd: main HTTP listener ([server].port) ─────┐  ┌─ [sandbox].listen ─┐
 │ /api/v1/...          bearer auth → binding guard → handler → access    │  │ sandbox gateway    │
 │ /git/<org>/<slug>.git  Basic auth → access rule → git protocol         │  │ (gRPC, lease token)│
 │ /hooks/forge/:slug   webhook signature → delivery log → hub event      │  └─────────┬──────────┘
 │ /metrics, /healthz, /readyz, /version                                  │            │
 └──────┬───────────────┬──────────────────┬──────────────────┬───────────┘            ▼
        ▼               ▼                  ▼                  ▼                Podman or a remote
   SQLite (identity,  DuckDB (audit,   <workspace root>/   clone queue (memory),  AgentKit gateway
   workspaces, jobs,  sessions, usage) <slug>/trunk        durable job queue
   secrets, leases…)                        │
        └── forge REST API ──▶ GitHub / GitLab     git push/fetch ──▶ origin, upstream
```

### 1.5 Goals of the re-implementation

1. **Wire compatibility.** The REST API is identical: routes, methods, path and query parameters, JSON field names and types, status codes, the error envelope and its `type` values, pagination, and the SSE event format. Existing `parley` binaries, agents, AgentKit clients and forge webhooks keep working against the new server without changes.
2. **CLI compatibility.** The new `parley` has the same command tree, arguments, flags, defaults, output (JSON on stdout, errors on stderr) and exit codes. Scripts that use `parley` keep working.
3. **Git compatibility.** Stock `git` clients can clone, fetch and push through `/git/...` with the same authentication, the same authorization and the same push rules, including protected refs and carry-patch push control.
4. **Semantic equivalence.** Same state machines, validation rules, defaults, limits, ordering, locking, idempotency, background schedules, retention and emitted events.
5. **Configuration compatibility.** The same `config.toml` keys, the same environment variables and the same first-boot procedure.

### 1.6 Non-goals

- Changing or extending behaviour. The only intended changes are the defect fixes listed in §1.8. Everywhere else, the current behaviour is the requirement.
- Supporting several replicas. The hub runs as one process (see §12).
- Byte-for-byte on-disk compatibility of the SQLite and DuckDB files. This PRD gives the logical data model. Reusing the current table layout is recommended so existing deployments can be upgraded in place, but it is not required. Migrating an existing deployment's data is a separate decision (see §13).
- The bundled `containers/agents` image and its coding-agent tooling, other than the hub image and the sandbox image contract described in §10 and §12.

### 1.7 Building blocks the hub reuses

The current hub is built on three sibling modules. Their observable behaviour is part of the hub's surface and is specified in this PRD. The re-implementation may keep using them (recommended for `apikit`, which owns identity, configuration, the server scaffold and the CLI scaffold) or replace them, as long as the behaviour is the same.

| Module | What the hub gets from it |
|---|---|
| `apikit` | Configuration loading, the SQLite database, first-boot admin bootstrap, the HTTP server (Echo) and its middleware, OAuth login, users, organizations, API keys, PATs and the scope registry, the error envelope, the `parley` scaffold (root command, `login`, `user`, `keys`, `tokens`, `orgs`, `admin`, `version`), credential storage and exit-code mapping. Build-time setting: token prefix `af` (credentials look like `af_pat_…`) |
| `aflib` | `issuex`, the GitHub/GitLab client; `jobqueue`, the durable, SQLite-backed job queue used for merge and rebuild jobs |
| `agentkit-go` | Sandbox providers (Podman, remote gateway), the sandbox spec, and `gatewayd`, the gRPC sandbox gateway the hub embeds |
| `carrypatch` (nested module in this repository) | The carry-patch feature, built as a separately embeddable Go module with a host contract. Its exported Go API (see `docs/generated/carrypatch`) is a secondary compatibility surface for other hosts |

### 1.8 Deliberate changes from the baseline (defect fixes)

The baseline has 15 behaviours that are defects. The re-implementation fixes them instead of copying them. Each fixed requirement is tagged **[FIX-nn]** where it is specified. A conformance test against the baseline must expect these differences, and only these.

| ID | Area | Baseline behaviour (defect) | Required behaviour | Specified in |
|---|---|---|---|---|
| FIX-01 | CLI | `parley orgs list` calls the admin-only `GET /orgs`, so non-admins get 403 | `parley orgs list` lists the caller's own orgs through `GET /user/orgs` | §3.6 |
| FIX-02 | CLI | `parley keys refresh` and `parley keys revoke` save to `./config.toml` in the working directory, and a failed save loses a refreshed key | Save to the config file `parley` reads (`$AF_CONFIG_DIR/config.toml`, default `~/.af/config.toml`), atomically. A failed save still prints the new key | §3.6, §3.10 |
| FIX-03 | Workspaces | Delete removes the directory and closes sessions before the database transaction, so a 500 can leave a partly deleted workspace | The database transaction runs first. A 500 means nothing was deleted. Directory removal and session closing follow the commit | §4.4 (WS-FR-075), §4.5 |
| FIX-04 | Workspaces | A clone job that finds an existing directory marks the workspace `ready` with `head_sha = NULL`, so a standard sync never fast-forwards | Reuse an existing clone only when it is valid and belongs to the workspace, and record its `HEAD` as `head_sha`. Otherwise remove the directory and clone fresh | §4.3, §4.4 (WS-FR-045, WS-FR-047) |
| FIX-05 | Workspaces | Clone jobs live only in memory. After a restart, `pending`/`cloning` workspaces are stuck, and a stuck `cloning` workspace can't be recovered | At start, every `pending` or `cloning` workspace is re-enqueued | §4.4 (WS-FR-048), §4.7.1, §12 |
| FIX-06 | CLI | The 30 s request timeout of `parley workspace push` is shorter than the server's 2 min push deadline | `parley workspace push` uses a 3 min request timeout | §4.6 |
| FIX-07 | CLI | `--wait` on job submissions treats `failed` (retry pending) as final and exits 1 | `--wait` keeps polling through `failed`. A job is final only at `completed`, `dead_letter` or `cancelled` | §3.4 (PLAT-FR-083/084), §6.6, §7.6 |
| FIX-08 | Merge, carry-patch | A merge or rebuild job waiting to retry (`failed`) does not count as a duplicate. The same merge can be queued twice, and a second rebuild can be queued for a workspace whose rebuild is waiting to retry | Dedup of merge and rebuild jobs covers `queued`, `running` and `failed`, for submits, automatic triggers and requeue. The 409 messages stay as they are | §6.4 (MRG-FR-004, MRG-FR-023), §7.3.4, §7.4 (CP-FR-110, CP-FR-143), §7.7 |
| FIX-09 | Carry-patch | `DELETE …/patches/:id` hard-deletes the row, so `parley patch remove` can't be undone with `parley patch restore` | DELETE soft-deletes, so the patch can be restored. Soft-deleted rows are purged after 7 days (FIX-12) | §7.3, §7.4 (CP-FR-070), §7.5, §7.6 |
| FIX-10 | Carry-patch | `conflict_files` is never cleared, and a `conflict` patch never returns to `active` | A patch that applies cleanly in a rebuild that reaches the success path returns to `active` with its `conflict_files` cleared. A `PATCH` that sets a status other than `conflict`, and a restore, also clear `conflict_files` | §7.3, §7.4 (CP-FR-125/127, CP-FR-061, CP-FR-080) |
| FIX-11 | Carry-patch / git | The git server accepts client pushes to a carry-patch workspace's integration branch | Push control rejects client updates of `refs/heads/<integration_branch>` | §7.4 (CP-FR-189, CP-FR-193) |
| FIX-12 | Carry-patch | The 7-day purge of soft-deleted patches exists but never runs | The hourly maintenance loop runs it | §3.4 (PLAT-FR-005), §7.4 (CP-FR-231) |
| FIX-13 | CLI | `parley secrets create` / `parley vars create` with several scopes print an extra `{"error":{"code":1,"message":""}}` after a partial failure | Each failed scope's envelope is printed once and nothing else. Exit status 1 | §8.6 |
| FIX-14 | Forge | A write without a workspace credential is sent to the forge without auth when the hub process has `GITHUB_TOKEN`/`GH_TOKEN`/`GITLAB_TOKEN`. Hub-sent writes always reach the forge | The forge client never uses process-environment tokens. Every write route without a workspace credential is refused before anything is sent: 502 `forge_error`, no event | §9.4 (FRG-FR-031) |
| FIX-15 | Audit | Replaying a usage report with an existing id answers 201 and increments the token counters again | A replay answers **200** with the stored record and changes no counter | §11.4 (AUD-FR-017) |

---

## 2. Personas, glossary and conventions

### 2.1 Personas

| Persona | Who | Typical credential | Main goals |
|---|---|---|---|
| **Operator** | Runs and administers the hub | Admin token (written at first boot) | Deploy and configure the hub; manage users, orgs, keys and tokens; define sandbox profiles; watch metrics and the audit log |
| **Developer** | A person who registers repositories and directs agents | API key, from `parley login` | Create and manage workspaces; merge branches; maintain carry-patch forks; manage secrets and variables; mint scoped tokens for agents |
| **Agent** | An AI coding agent running on a developer's behalf | PAT, often bound to a single workspace | Clone and push through the hub; submit merges; read and write issues and PRs; report sessions, usage and telemetry; lease a sandbox |
| **Org member** | A user who belongs to the organization that owns a workspace | API key or PAT | Read the org's workspaces (read-only access) |
| **Forge** | GitHub or GitLab | Webhook secret (HMAC signature or token) | Deliver webhooks that the hub turns into hub events |
| **Sandbox client** | An AgentKit client that holds a sandbox lease | Lease token | Create and drive exactly one sandbox through the gateway |

### 2.2 Glossary

| Term | Meaning |
|---|---|
| Workspace | A registered repository. The hub keeps a clone of it at `<workspace root>/<slug>/trunk` and identifies it by its `slug` |
| Origin | The workspace's own remote (`git_url`) |
| Upstream | For a carry-patch workspace, the project the fork tracks (`upstream_url`) |
| Workspace mode | `standard` or `carry_patch` |
| Integration branch | For a carry-patch workspace, the branch the hub rebuilds as upstream plus the ordered patches |
| Patch | A carry-patch entry: a branch with a position in the patch order and a status |
| Rebuild | A durable job that recreates the integration branch from upstream and the active patches |
| Merge job | A durable job that rebases a source branch onto a target and fast-forwards the target |
| Durable job queue | A SQLite-backed queue for merge and rebuild jobs. It runs at most one job at a time per workspace and survives restarts |
| Clone queue | An in-memory queue of initial clone and reclone work |
| Workspace lock | An in-memory lock per workspace. Operations that touch the clone take it with try-lock semantics; if it is held, the request fails with 409 `workspace_busy` |
| Scope / permission | A string carried by a PAT that grants read or write access to an area |
| Bound token | A PAT confined to one workspace |
| Lease | A short-lived right to create and use one sandbox, shaped by a profile and authenticated by a lease token |
| Profile | An operator-defined sandbox template: image, resources, injected secrets, and so on |
| Hub event | An audit event the hub records about its own activity (`hub.*` types). Hub events are also published on the SSE stream |
| Agent telemetry | Run events, tool calls, tool errors, traces, session outcomes and postmortems that agents send to the hub |
| Session | An agent session with token usage and cost |

### 2.3 Conventions used in this PRD

- Paths are written relative to the API mount point `/api/v1` unless they start with `/git`, `/hooks`, `/metrics`, `/healthz`, `/readyz` or `/version`.
- `:param` is a path parameter. `*` in a route matches the rest of the path.
- Requirement IDs (`WS-FR-012`) are stable references for tests and specs. The prefixes are `PLAT`, `WS`, `GIT`, `MRG`, `CP`, `SEC`, `FRG`, `SBX`, `AUD` and `NFR`.
- "Error `xyz`" means the error envelope (§3) with `type` (or code) `xyz`.
- **MUST** means a requirement of compatibility. **[FIX-nn]** marks a requirement that deliberately differs from the baseline to fix a defect (see §1.8). Notes marked *Re-implementation note* describe other current behaviour that looks unintended; that behaviour is still required unless the product owner decides otherwise.


---

## 3. Platform, identity and access

### 3.1 Purpose and scope

This area is the base every other feature sits on. It covers the two binaries (the `parleyd` server and the `parley` client); how the server starts, bootstraps its admin credential, runs its hourly maintenance and shuts down; the server configuration file and every environment variable; and the identity layer. The identity layer is supplied by the apikit library, but it is part of the hub's public surface: the OAuth login, users, API keys, personal access tokens (PATs), the admin token, organizations and memberships, the personal organization each new user gets, and the permission-scope registry. Also in scope: the shared workspace access rule and the anti-enumeration answers, workspace-bound tokens, the error envelope and the server-wide HTTP conventions, the health, version and metrics endpoints, `parley`'s global behaviour (credential storage, global flags, output, exit codes, waiting, path arguments) and credential scrubbing.

Out of scope are the per-feature routes that apply the access rule (workspaces, git, secrets, carry-patch, merge, sandboxes, forge, audit). Their own sections cover them. This section names only the rule they share. The metric families are described by the audit area, and the keys inside `[sandbox]` by the sandbox area.

### 3.2 User stories

**US-PLAT-01 (Operator): first boot.** As an Operator, I want the first start to give me an admin token, so that I can administer the hub without a user account.
- `parleyd --admin-email=a@x` on a database with no `admin_token_hash` does four things. It stores `admin_email`, writes `<config dir>/admin_token` (mode 0600, token without a trailing newline) and logs its absolute path at warn level. Then it exits with status 1 and the message `admin token generated; save the token securely and delete the file before restarting`, without serving.
- A later start while `admin_token` still exists exits 1 with the message `admin_token file exists at <abs>: save the token securely and delete the file before restarting`.
- A later start after the file is deleted serves requests. `--admin-email` is then ignored.
- `--reset-admin-token` writes a new token and exits the same way. The old token stops working at once.
- A start without either flag on a fresh database serves at once, with no admin token.

**US-PLAT-02 (Developer): log in.** As a Developer, I want `parley login` to open GitHub in my browser and store an API key, so that later commands just work.
- A browser login returns a new API key and revokes every older API key of the user.
- The key, the hub URL and the user id are saved to `~/.af/config.toml` (mode 0600).
- The user JSON is printed on stdout and `Logged in as <username>` on stderr.
- A new user gets a personal organization in the same transaction.
- The first user whose e-mail matches the bootstrap `admin_email`, ignoring case, gets role `admin`, provided no admin user exists yet.

**US-PLAT-03 (Developer): mint PATs.** As a Developer, I want to mint PATs with selected scopes, so that CI jobs and agents get only what they need.
- An unknown or retired scope is refused with 400.
- A PAT can create PATs only with `tokens:manage`, and only with scopes it holds itself.
- The token value appears only in the creation response.

**US-PLAT-04 (Developer / Agent): workspace-bound token.** As a Developer, I want to mint a token bound to one workspace for an agent, so that a leaked token reaches nothing else.
- `parley tokens create --workspace S --name n --permissions a,b` prints the token once. On stderr it writes `warning: the token in this output will not be shown again; save it now`.
- The bound token gets `404 workspace not found` on every other workspace.
- On non-workspace routes that are not allowlisted, and on all apikit routes, it gets `403` with `error_type: token_bound`.
- Refusals are audited as `hub.token.bound_refused`, at most once per token per minute.

**US-PLAT-05 (Org member): read a workspace.** As an Org member, I want to read the workspaces of my org, so that I can follow work I do not own.
- Read routes answer a member exactly as they answer the owner. The workspace object carries `access: "member"`.
- Write routes answer `403 org members have read-only access to this workspace` with `error_type: forbidden`.
- Private routes answer a member `404 workspace not found`.

**US-PLAT-06 (any caller): anti-enumeration.** As any caller, I must not learn whether a workspace I cannot see exists.
- An unknown slug and an invisible workspace get the same `404 workspace not found`.

**US-PLAT-07 (Operator): user and org administration.** As an Operator (admin token, or an API key of an admin-role user), I want to manage users and orgs.
- `parley admin users ...` and `parley admin orgs ...` call the admin routes.
- Non-admins get `403 forbidden`.
- The last active admin can be neither demoted nor deleted (409).

**US-PLAT-08 (Agent / CI): scriptable CLI.** As an Agent or CI job, I want machine-readable `parley` output and stable exit codes.
- stdout carries only JSON documents. Hints go to stderr.
- Exit codes: 0 success, 1 API error or wait failure, 2 client or usage error, 3 the diverged sync.

**US-PLAT-09 (Operator): probes.** As an Operator, I want liveness, readiness, version and metrics endpoints without credentials, for orchestration and monitoring.

### 3.3 Concepts and data model

#### 3.3.1 Process model

- There is one OS process and one replica. Several state items live only in memory: per-workspace locks, the rebuild-active flag, the clone queue, the SSE connections and the bound-refusal rate limiter. A second replica is unsupported (`deploy/deployment.yaml` sets `replicas: 1`).
- Two stores are used. The SQLite database (`[database].path`) holds identity, workspaces and queues. A DuckDB audit database sits next to it, at `AF_AUDIT_DB_PATH` or `<dir of SQLite file>/audit.duckdb`.
- SQLite is opened with these settings:
  - the parent directory is created with mode 0700;
  - WAL journal (required, or the start fails);
  - `busy_timeout=5000`, `foreign_keys=ON` on every connection, immediate transaction locking;
  - a pool of 4 connections.
- If an existing `users` table holds duplicate e-mails, the start fails. The error is `FATAL migration failed: duplicate emails detected — deduplicate before restarting: <email> (<n> rows), ...`.

#### 3.3.2 Credential types

| Type | Wire format (prefix `af`, set at build time) | Stored | Identity | Scope-checked | Admin? |
|---|---|---|---|---|---|
| Admin token | `af_admin_<64 lowercase hex>` (32 random bytes) | SHA-256 hex of the whole token string in `admin_config.admin_token_hash` | none (`user_id` empty, role `admin`) | no | yes, everywhere |
| API key | `af_<key_id>_<secret>`: `key_id` 8 chars, `secret` 32 chars, both from `[0-9A-Za-z]` | `api_keys` row; SHA-256 hex of the secret part | a user | no | only on apikit admin routes, and only when the user's role is `admin` |
| PAT | `af_pat_<token_id>_<secret>`: `token_id` 8 chars, `secret` 32 chars, both from `[a-z0-9]` | `pats` row; SHA-256 hex of the secret part; permissions as a JSON array | a user | yes | never |

- **Presentation.** REST calls use `Authorization: Bearer <credential>`. The scheme must be exactly `Bearer ` (case-sensitive, one space). One further leading `Bearer ` inside the token is also stripped, so `Bearer Bearer <cred>` authenticates. The git server takes the credential as the HTTP Basic password; the git area covers this.
- **Classification order.** A token is matched against these prefixes in order:
  1. `af_admin_`: admin token.
  2. `af_pat_`: PAT. The rest is split at the first `_`. The id and the secret must both be non-empty, or the token is unrecognised.
  3. `af_`: API key, split the same way.
  4. Anything else is unrecognised.
- **Validation of an API key or PAT.** The checks run in this order:
  1. Look up the row by id. A missing row is `401 invalid credentials`.
  2. Compare the secret hash in constant time. A mismatch is `401 invalid credentials`.
  3. A non-null `revoked_at` is `401 credential revoked`.
  4. An `expires_at` in the past (now > `expires_at`) is `401 credential expired`.
  5. A user with `status = blocked` is `403 user is blocked`.

  Because revocation and expiry are checked only after the secret, a caller who knows only the id cannot probe a token's state.
- **Validation of an admin token.** The hex suffix must be exactly 64 hex characters, or the token is `401 invalid credentials` without a database lookup. Then the SHA-256 of the full token is compared in constant time. A missing hash row is `401 invalid credentials`.

#### 3.3.3 Entities (SQLite, created by the hub's identity layer)

**users**

| Field | Type | Meaning / default / constraints |
|---|---|---|
| `id` | text (UUID v4) | primary key |
| `username` | text | NOT NULL, UNIQUE |
| `email` | text | NOT NULL, UNIQUE (index `idx_users_email`) |
| `full_name` | text, nullable | NULL for OAuth-created users, `""` for admin-created users |
| `role` | text | `user` (default) or `admin` |
| `status` | text | `active` (default) or `blocked` |
| `provider` | text | e.g. `github`, `google`, or any string given to `POST /users` |
| `provider_id` | text | the provider's stable user id; `(provider, provider_id)` is UNIQUE |
| `created_at`, `updated_at` | text | `YYYY-MM-DDTHH:MM:SSZ` (UTC, whole seconds) |

**api_keys**

| Field | Type | Meaning / default / constraints |
|---|---|---|
| `key_id` | text | primary key |
| `user_id` | text | references `users(id)`, cascades on delete |
| `secret_hash` | text | SHA-256 of the secret |
| `expires_days` | int | 0, 30, 60 or 90 |
| `expires_at` | text or NULL | NULL means never |
| `revoked_at` | text or NULL | |
| `created_at` | text | |

**pats**

| Field | Type | Meaning / default / constraints |
|---|---|---|
| `token_id` | text | primary key |
| `user_id` | text | references `users`, cascades on delete |
| `name` | text | 1 to 255 bytes |
| `secret_hash` | text | |
| `permissions` | text | JSON array of `resource:action` strings, kept in the order given |
| `expires_days` | int | |
| `expires_at` | text or NULL | |
| `revoked_at` | text or NULL | |
| `created_at` | text | |

**orgs**

| Field | Type | Meaning / default / constraints |
|---|---|---|
| `id` | text (UUID v4) | primary key |
| `name` | text | NOT NULL, UNIQUE |
| `slug` | text | NOT NULL, UNIQUE; immutable after creation |
| `url` | text | `""` default |
| `owner_id` | text or NULL | free text, no foreign key; set to NULL when the owner user is deleted |
| `status` | text | `active` (default) or `blocked` |
| `created_at`, `updated_at` | text | |

**org_members**

| Field | Type | Meaning / default / constraints |
|---|---|---|
| `org_id` | text | references `orgs`, cascades on delete |
| `user_id` | text | references `users`, cascades on delete |
| `created_at` | text | |

The primary key is `(org_id, user_id)`.

**admin_config**: key/value rows. Keys in use: `admin_email` and `admin_token_hash`.

**token_workspace_bindings** (hub): `token_id` text (primary key, so a token has at most one binding), `slug` text NOT NULL (indexed), `created_at` text. The table has **no foreign keys**: deleting or archiving a workspace leaves the binding row alone, unless the delete path removes it explicitly (see PLAT-FR-062).

#### 3.3.4 Enumerations and state machines

- **User role.** `user` ⇄ `admin`, through promote and demote. The last active admin cannot be demoted.
- **User status.** `active` ⇄ `blocked`, through block and unblock. Blocking revokes nothing. It makes every credential of the user fail authentication with `403 user is blocked` until the user is unblocked.
- **Org status.** `active` ⇄ `blocked`. Blocking an org has no effect on workspace access or membership checks. It hides the org from `GET /user/orgs` and, by default, from `GET /orgs`.
- **API key and PAT lifecycle.** `live` → `revoked`, which is final (`revoked_at` is set). `live` → `expired` happens by time, and the row stays.
  - PATs become `revoked` in five ways: a DELETE, an empty permission set, deletion of their workspace when bound, the binding sweep, or a failed bind.
  - API keys become `revoked` by a DELETE, by the next OAuth login of the user, or by an admin revoke.
- **Workspace access level** (`access` field values): `admin` (admin token), `owner`, `member`, or none. None is never rendered, because such a caller gets 404.
- **Workspace need** (per route): `read`, `write` or `private`.
- **Refusal surface.** `api` or `git`.

#### 3.3.5 Permission registry (scopes)

A scope is `resource:action`. Both parts match `^[a-z0-9_]+$`. Registering a pair twice is an error at start. The registry holds exactly these 30 scopes:

| Scope | Registered by | Notes |
|---|---|---|
| `users:read` | built-in | `GET /user`, `PATCH /user` |
| `orgs:read` | built-in | `GET /user/orgs`, `GET /orgs/:id`, `GET /orgs/:id/members` |
| `orgs:write` | built-in | checked by no route |
| `keys:read` | built-in | `GET /user/keys` |
| `keys:manage` | built-in | `DELETE /user/keys/:key_id` |
| `tokens:read` | built-in | `GET /user/tokens`, `GET /user/tokens/:token_id`, `GET /workspaces/:slug/tokens` |
| `tokens:write` | built-in | permission-change routes |
| `tokens:manage` | built-in | create and revoke PATs; implies `tokens:write`; implies `tokens:read` only on `GET /workspaces/:slug/tokens` |
| `workspaces:read`, `workspaces:write` | hub (workspaces) | |
| `patches:read`, `patches:write` | hub (workspaces/carry-patch) | |
| `git:read`, `git:write` | hub (git) | |
| `secrets:read`, `secrets:write` | hub (secrets) | |
| `vars:read`, `vars:write` | hub (secrets) | |
| `merges:read`, `merges:write` | hub (merge) | |
| `rebuilds:read`, `rebuilds:write` | hub (carry-patch) | |
| `audit:read`, `audit:write` | hub (audit) | |
| `sessions:read`, `sessions:write` | hub (audit) | |
| `sandboxes:read`, `sandboxes:write` | hub (sandboxes) | always registered, even with sandboxes off |
| `forge:read`, `forge:write` | hub (forge) | always registered |

Implication rules:
- **Hub resources.** A read check accepts `R:read` or `R:write`. A write check accepts only `R:write`.
- **Built-in scopes.** These are exact-match checks with two exceptions: `tokens:write` OR `tokens:manage` on the three permission-change routes, and `tokens:read` OR `tokens:manage` on `GET /workspaces/:slug/tokens`. `GET /user/tokens` and `GET /user/tokens/:id` accept **only** `tokens:read`.
- **Who is checked.** Only PATs are scope-checked. Admin tokens and API keys pass every scope check.

Retired scopes are rewritten at start. They are not registered, so `POST /user/tokens` refuses them with `400 unknown permission: <p>`.

| Old | New |
|---|---|
| `workspaces:create`, `workspaces:delete`, `workspaces:sync` | `workspaces:write` |
| `secrets:list` | `secrets:read` |
| `secrets:manage`, `secrets:delete` | `secrets:write` |
| `vars:manage`, `vars:delete` | `vars:write` |

#### 3.3.6 CLI configuration file (`~/.af/config.toml`)

- **Location.** `$AF_CONFIG_DIR/config.toml` when `AF_CONFIG_DIR` is set and non-empty, otherwise `$HOME/.af/config.toml`. There is no bare `CONFIG_DIR` fallback.
- **Keys.**

  | Key | Meaning |
  |---|---|
  | `endpoint_url` | the hub's base URL |
  | `user_id` | the logged-in user's id |
  | `api_key` | the API key or PAT |
- **Creation.** The directory (mode 0700) and a template file (mode 0600) are created by `login` and before every authenticated command, if missing. The template text is `endpoint_url = ""\nuser_id = ""\napi_key = ""\n`.
- **Reading.** `$VAR` and `${VAR}` in the text are expanded from the environment before TOML parsing. An unparseable file fails with `config file is unparseable: <err>`.
- **Writing.** The file is written atomically: a temp file `config-*.toml.tmp`, chmod 0600, then rename. The TOML encoder writes the keys in the order endpoint_url, user_id, api_key.

### 3.4 Functional requirements

#### Server start, bootstrap, shutdown

- **PLAT-FR-001 (flags).** `parleyd` takes exactly two flags, parsed with Go's `flag` package:
  - `--admin-email <string>` (default `""`);
  - `--reset-admin-token` (bool, default false).

  An unknown flag prints the usage and exits 2. `-h`/`-help` prints the usage and exits 0. Positional arguments are ignored. There are no subcommands.
- **PLAT-FR-002 (start sequence).** Each step's failure stops the process with exit status 1 and the error on stderr (Go `log.Fatal`). The steps run in this order:
  1. Load the configuration (§3.9).
  2. Open SQLite and apply the identity schema. Duplicate e-mails are fatal.
  3. Run the bootstrap (PLAT-FR-003).
  4. Rewrite retired PAT scopes (PLAT-FR-040). When anything was rewritten, log `rewrote PAT scopes to read/write tokens=<n>`.
  5. Load, validate and probe the `[sandbox]` section (sandbox area).
  6. Create the workspace root directory.
  7. Start the clone workers.
  8. Prepare the durable job queue: schema and migrations, handler registration, removal of stale rebuild worktrees, queue start.
  9. Open the DuckDB audit database and create its schema.
  10. Build the HTTP server and register all scopes and routes. The workspace and `token_workspace_bindings` tables, the secrets and variables tables, the forge delivery table and the sandbox lease table are created or migrated along the way. A database that predates workspace settings has its variables moved into settings.
  11. Start the maintenance loop (PLAT-FR-005). It runs concurrently with the remaining steps and with serving.
  12. Start the sandbox gateway when it is configured.
  13. Bind `[server].bind:[server].port` and serve. Two lines are logged: `paths` (with the absolute config path and the data directory) and `server listening` (with `addr` and `mount_point`).
- **PLAT-FR-003 (bootstrap).** "Bootstrapped" means the `admin_config` row `admin_token_hash` exists. The cases are:
  - **No flags and not bootstrapped:** nothing happens. The server starts without an admin token.
  - **`--reset-admin-token`** (it takes priority): generate a new token, upsert its hash, write the file, log a warn line and exit 1 with the "admin token generated" message. The file-presence check is skipped, and an existing file is overwritten. It also works on a never-bootstrapped database, where it stores no `admin_email`.
  - **Bootstrapped** (with or without `--admin-email`): if `<config dir>/admin_token` exists, exit 1 (the US-PLAT-01 message). Otherwise continue. `--admin-email` is ignored, with no write and no log.
  - **Not bootstrapped with `--admin-email`:** upsert `admin_email`, generate and store the token, write the file, warn and exit 1 as above.

  `<config dir>` is `$XDG_CONFIG_HOME` when set, otherwise the working directory. The token file is written with mode 0600 and holds the token without a newline. The warn line is `Admin token written to <abs path> — save the token securely and delete the file`.
- **PLAT-FR-004 (shutdown).** On SIGTERM or SIGINT the HTTP server shuts down gracefully with a fixed 15-second drain window. Then, in order:
  1. The sandbox gateway is shut down (a no-op without sandboxes).
  2. The background context is cancelled. Clone workers stop: a clone in progress is interrupted and queued clone jobs are dropped. The maintenance loop and the SSE manager stop.
  3. The durable queue stops.
  4. The audit database and SQLite close.

  A clean shutdown exits 0. If binding the listener fails, the server still runs the gateway shutdown and then exits 1.
- **PLAT-FR-005 (maintenance loop).** The loop runs these four tasks in order, once right after start and then every 1 hour (ticker) until shutdown:
  1. `audit retention` (audit area);
  2. `forge delivery log` pruning (forge area: rows older than 7 days or of deleted workspaces);
  3. `token binding sweep` (PLAT-FR-063);
  4. `patch purge` (carry-patch area, CP-FR-231). **[FIX-12]** The baseline had only the first three tasks.

  A failing task is logged as `maintenance task failed task=<name> error=<err>`, and the other tasks still run. A task cut short by shutdown is not logged as a failure. A pass is never run twice at once. The interval is not configurable.

#### Configuration

- **PLAT-FR-010.** The configuration path is `$XDG_CONFIG_HOME/config.toml` when `XDG_CONFIG_HOME` is non-empty, otherwise `./config.toml`. A missing file means all defaults. A read error other than "not exist" is fatal. The file is read once, at start.
- **PLAT-FR-011.** Before parsing, the whole file text is expanded with shell-style `$VAR` and `${VAR}` substitution:
  - an unset variable becomes `""`;
  - `$` followed by a character that cannot start a name stays as it is;
  - there is no escape syntax, so a literal `$name` cannot be written;
  - a value containing `"` breaks the TOML.

  The `[sandbox]` section is re-read from the raw file with its own `${NAME}` rules (sandbox area).
- **PLAT-FR-012.** Unknown keys and sections are ignored silently, except inside `[sandbox]`. Invalid TOML is fatal: `parsing config.toml: <err>`.
- **PLAT-FR-013 (validation).** The following values are fatal at start:
  - `port` outside 0..65535: `invalid port <n>: must be in range 0-65535`;
  - `logging.level` not in {trace, debug, info, warn, error, fatal, panic}, compared case-insensitively. `warning` is rejected. The error is `invalid log level "<v>": must be one of trace, debug, info, warn, error, fatal, panic`;
  - `max_body_size` not matching `^[1-9][0-9]*(KB|MB|GB)$` after upper-casing (so `1mb` is accepted). Units are powers of 1024. The error is `invalid max_body_size "<v>": must be a positive integer followed by KB, MB, or GB (e.g. '1MB', '512KB')`;
  - an `[[oauth.providers]]` entry missing `name`, `client_id` or `client_secret`: `oauth.providers[<i>]: name is required` / `oauth.providers[<i>] (<name>): client_id is required` / `... client_secret is required`;
  - a duplicate provider name: `oauth.providers: duplicate provider name "<n>"`;
  - a provider name other than `github` or `google`, rejected later at route mounting: `mount handlers: unknown provider: <n>`.

#### Identity: OAuth login

- **PLAT-FR-020.** `GET /auth/providers` lists the configured providers, sorted by name. Each `authorize_url` is the provider's authorize endpoint with `client_id`, `scope` (`user:email` for GitHub; `openid email profile` and `response_type=code` for Google), `state=` (empty) and `redirect_uri=` (empty) as query parameters. Secrets, the token URL and the userinfo URL are never exposed.
- **PLAT-FR-021 (callback).** `POST /auth/callback` processes the request in this order:
  1. Bind the JSON body.
  2. Require `provider`, `code` and `redirect_uri`.
  3. Check `expires` (default 90; must be 0, 30, 60 or 90).
  4. Look up the provider.
  5. Validate `redirect_uri`. It is allowed when its host is `localhost` or `127.0.0.1` and the scheme is `http` (any port and path; `https` on localhost is refused), or when its scheme and host:port equal those of `[server].external_url`.
  6. Exchange the code at the token URL: a form POST of `client_id`, `client_secret`, `code` and `redirect_uri` (Google adds `grant_type=authorization_code`), with `Accept: application/json`, and a 15-second timeout per provider call.
  7. Fetch the userinfo.
  8. Validate the identity.
  9. Run one transaction for the user upsert and key issue.
- **PLAT-FR-022 (provider identity).**
  - **GitHub.** `GET <userinfo_url>` (default `https://api.github.com/user`) with `Authorization: Bearer <token>` gives `login`, `email` and `id`.
    - `id == 0` is an error, answered 502.
    - An empty `email` is filled from `GET <userinfo_url minus trailing /user>/user/emails`: the first entry that is primary and verified, else the first verified one, else an error (502).
    - `provider_id` is the decimal `id`.
  - **Google.** The userinfo gives `sub`, `name`, `given_name`, `email` and `email_verified`. A missing `sub`, a missing `email` or an unverified e-mail is an error (502). The username is `name`, else `given_name`.
  - **Default endpoints.** GitHub: `https://github.com/login/oauth/authorize`, `https://github.com/login/oauth/access_token`, `https://api.github.com/user`. Google: `https://accounts.google.com/o/oauth2/v2/auth`, `https://oauth2.googleapis.com/token`, `https://www.googleapis.com/oauth2/v3/userinfo`. Each is overridable per provider.
  - An empty provider username is replaced by `<provider>-<provider_id>`.
- **PLAT-FR-023 (upsert).** Inside the transaction:
  - **Existing user** (matched by `(provider, provider_id)`):
    - if blocked, answer `403 user is blocked` and roll back;
    - otherwise set `username` to the provider username, `email` and `updated_at`;
    - if the new username collides with another user's, keep the old username and update only `email` and `updated_at`.
  - **New user:**
    - The role is `admin` when the e-mail equals `admin_email` ignoring case and there are 0 users with role `admin`. Otherwise it is `user`.
    - Insert with `full_name` NULL and `status` `active`.
    - On a username collision, retry with `<name>-2`, `<name>-3`, … up to `<name>-10` (10 attempts in total). After that the error propagates.
    - Then run the personal-org hook (PLAT-FR-030). A hook failure rolls everything back.
  - **Both:** revoke every live API key of the user (`revoked_at = now`), generate a new key and insert it with `expires_days` and `expires_at` (NULL when 0).
  - **Failure mapping:** a UNIQUE, PRIMARY KEY or FOREIGN KEY failure anywhere (for example a duplicate e-mail, or a personal-org name collision) answers `409 user already exists`. Any other error answers `500 internal server error`.
- **PLAT-FR-024.** Only the OAuth callback creates API keys. There is no password login.

#### Personal organization

- **PLAT-FR-030.** Every user creation path (the OAuth callback and `POST /users`) creates, in the same transaction, an org with these fields: `name = <final username>`, `slug = sanitize(username)`, made unique, `url = ""`, `owner_id = <user id>`, `status = active`, both timestamps now (RFC 3339 UTC, whole seconds). It also inserts the membership row `(org, user)`.
- **PLAT-FR-031 (slug sanitising).** The steps run in this order:
  1. Lowercase.
  2. Replace every character outside `[a-z0-9_-]` with `-`.
  3. Collapse runs of `-` into one.
  4. Trim leading and trailing `-` and `_`.
  5. If the result is shorter than 2 bytes, use `u-<first 8 chars of the user id>` and stop.
  6. If it starts with a digit, prefix `u-`.

  Examples: `Alice.Smith` → `alice-smith`; `7` → `u-1b2c3d4e`; `42bob` → `u-42bob`.
- **PLAT-FR-032 (collisions).** If the slug exists in `orgs`, the hook tries `<slug>-1` through `<slug>-10` in order and takes the first free one. If all 10 are taken, the hook fails with `could not find unique slug for user <username> after 10 attempts`. The OAuth callback then answers 500 and `POST /users` answers 500. If an org already has the name equal to the username, the org INSERT fails on `orgs.name` UNIQUE. The OAuth callback then answers `409 user already exists` and `POST /users` answers `500 internal server error`.

#### PATs, API keys

- **PLAT-FR-035 (PAT create).** `POST /user/tokens` runs these checks in order:
  1. Scope `tokens:manage` (PATs only): `403 insufficient permissions`.
  2. Bind the body: `400 invalid request body`.
  3. `name` empty: `400 name is required`. `len(name) > 255` bytes: `400 name must be 255 characters or fewer`.
  4. `permissions` empty: `400 permissions are required`.
  5. Each permission must contain exactly one `:` (`400 invalid permission format: <p>`) and be registered (`400 unknown permission: <p>`). The first failing entry is reported.
  6. `expires` (default 90) must be in {0, 30, 60, 90}: `400 expires must be 0, 30, 60, or 90`.
  7. Privilege escalation, for PAT callers only: every requested permission must be held by the caller, else `403 cannot grant permission: <p>`.
  8. Generate the id and secret and insert. On a token_id conflict, retry with a new id, up to 3 attempts in total. After that, `500 internal server error`. An admin token has no user, so its insert fails and the answer is 500.

  Duplicates in `permissions` are stored as given. The name is not trimmed.
- **PLAT-FR-036 (PAT permission changes).** These are `PUT`, `PATCH` and `DELETE` on `/user/tokens/:token_id/permissions`. The checks run in this order:
  1. Scope `tokens:write` or `tokens:manage`, for PATs: `403 insufficient permissions`.
  2. Bind the body: `400 invalid request body`.
  3. The `permissions` field: absent or null gives 400 `permissions are required`. An empty array gives the same 400 on PATCH and DELETE.
  4. Look up the token for the caller's user: `404 token not found`. A revoked token is `400 token is revoked`. A token with `expires_at <= now` is `400 token is expired`.
  5. Validate the permissions. PUT and PATCH check format and registry. DELETE checks the format only, so unknown names are ignored.
  6. Escalation check, for PAT callers. PUT checks only the permissions not already on the token. PATCH checks every requested permission, but those already on the token are exempt. DELETE has no check.
  7. Write the change:
     - PUT replaces the list. An empty list sets `revoked_at = now`, so the token is auto-revoked.
     - PATCH appends the requested permissions that are missing, keeping the existing order.
     - DELETE removes the named ones. If nothing remains, the token is auto-revoked.
- **PLAT-FR-037 (PAT revoke).** `DELETE /user/tokens/:token_id` needs `tokens:manage` for PATs. It is a conditional update on the caller's own live row. The answers are 200 with the metadata, `404 token not found`, or `400 token already revoked`. A PAT may revoke itself.
- **PLAT-FR-038 (API key refresh).** `POST /user/keys/:key_id/refresh`:
  1. A PAT credential gets `401 API key authentication required`.
  2. Look up `key_id`. A missing key, or one owned by another user (including any key for an admin token), is `404 key not found`.
  3. A revoked key is `400 cannot refresh a revoked key`. An expired key is `400 cannot refresh an expired key`.
  4. A new 32-character secret is generated. The new `expires_at` is now + `expires_days` (NULL when 0). `created_at` is reset to now with **nanosecond** precision. `key_id` is kept.
- **PLAT-FR-039 (API key revoke).** `DELETE /user/keys/:key_id` needs `keys:manage` for PATs. A key that is missing or not the caller's is `404 key not found`. An already revoked key is `400 key is already revoked`. An expired key may be revoked. A key may revoke itself.
- **PLAT-FR-040 (scope migration).** At every start, every `pats` row (revoked ones included) is read:
  - Each retired scope is replaced by its successor (§3.3.5).
  - Duplicates are dropped, keeping the first occurrence in the original order. A row that only has duplicates is rewritten too.
  - Rows whose permissions are not a JSON string array are skipped.
  - The run is idempotent.
  - Hub resources treat write as implying read, so a rewritten token may read what it could not read before.

#### Admin / ownership checks (apikit routes)

- **PLAT-FR-045.** "Admin" on the user and org management routes means an admin token, or an **API key** whose user has role `admin`. PATs are never admin. A failed admin check answers `403 forbidden`.
- **PLAT-FR-046 (selectors).** `:id` and `:user_id` on user routes are resolved like this:
  - a string that parses as a UUID matches `id`;
  - otherwise a string containing `@` matches `email` exactly;
  - otherwise it matches `username`.

  `:id` on org routes matches the UUID `id`, else `slug`.
- **PLAT-FR-047 (last-admin safeguard).**
  - **Demote** is one atomic update that succeeds only while more than 1 user is role `admin` with status `active`. Otherwise it answers `409 cannot demote the last admin`. Demoting a user who is already role `user` is an idempotent 200.
  - **Delete** of an active admin while ≤1 active admin exists answers `409 cannot delete the last admin`. The check runs before the transaction and again inside it.
  - **Block** has no safeguard.

#### Workspace access rule

- **PLAT-FR-050.** Every route that names a workspace by slug decides access with this rule, in this order:
  1. There is no credential context: `401 authentication required`. This cannot happen behind the auth middleware.
  2. The slug names no `workspaces` row: `404 workspace not found`.
  3. The caller's access is decided:
     - an admin token is `admin`;
     - otherwise a credential without a user id is none;
     - otherwise the user id equal to `owner_id` is `owner`;
     - otherwise, when the workspace has an `org_id` and `org_members` has `(org_id, user_id)`, the caller is `member`. This is read on every request, whatever the org's status.
     - otherwise none.
  4. Access none: `404 workspace not found`. No binding lookup or refusal report is made.
  5. The PAT is bound to a different slug: `404 workspace not found`, and the refusal is reported (PLAT-FR-066).
  6. A member on a `write` need: `403 org members have read-only access to this workspace`, with `error_type: forbidden`. A member on a `private` need: `404 workspace not found`.
  7. Otherwise access is granted and the caller's level is returned.

  Any database error is `500 internal server error`, never a grant. An admin-role API key is **not** `admin` here.
- **PLAT-FR-051 (ordering against scopes and preconditions).** On hub routes the PAT scope check comes **before** the access decision. A PAT without the scope therefore gets the scope answer whatever its access, and even for a nonexistent slug. The workspace CRUD routes answer a missing scope with `404 workspace not found` instead of 403. Workspace-state preconditions (archived and so on) come **after** access, so a member's write to an archived workspace is 403, not 400.
- **PLAT-FR-052 (readable set).** The workspace listing and similar cross-workspace reads use one readable set:
  - an admin token reads all workspaces;
  - a credential without a user id reads none;
  - otherwise every workspace of any status the user owns (`owner`), plus those whose non-empty `org_id` is one of the user's orgs (`member`). Ownership wins over membership.
  - A bound PAT's set is cut down to at most its bound slug.

  Every workspace object carries `access` (`admin`, `owner` or `member`) and `org_slug`.
- **PLAT-FR-053 (anti-enumeration).** A workspace that is not visible, or that is invisible at the route's level, is answered exactly like a nonexistent slug: the same status, the same message `workspace not found`, and no `error_type`. A member's write is 403, because the member already knows the workspace exists.

#### Workspace-bound tokens

- **PLAT-FR-060 (mint).** `POST /workspaces/:slug/tokens` runs these checks in order:
  1. No credential: `401 authentication required`.
  2. The caller is a PAT that is bound (to any workspace): `403 this token is bound to workspace <bound slug>`, `token_bound`, reported. A PAT bound to a **different** slug never gets here, because the binding guard answers 404 first.
  3. Admin token: `403 admin tokens cannot mint bound tokens; a real user is required as the token's owner`.
  4. Access with need `private`: `404 workspace not found` for a stranger or a member.
  5. Workspace `status == archived`: `400 workspace is archived`.
  6. The body is read. A read failure is `400 invalid request body`.
  7. The request is served in-process by `POST <mount>/user/tokens` through the full HTTP stack, with:
     - the original body;
     - the caller's `Authorization` header verbatim;
     - `Content-Type` (default `application/json`) and `Content-Length`;
     - the remote address.

     All PAT-create rules of PLAT-FR-035 apply unchanged, including the `tokens:manage` check and the subset rule for PAT callers. The hub does no `tokens:manage` check of its own.
  8. Any status other than 201 is passed back unchanged: status, `Content-Type` and body. Nothing is bound.
  9. On 201, the answer is parsed. If it is not JSON, or it lacks `token_id` or `token`, the token is revoked (when its id can be read from the partial JSON) and the answer is `500 failed to bind the token to the workspace`.
  10. The binding row is inserted only while the workspace row still exists (one INSERT…SELECT). A database error, a primary-key conflict included, means revoke and `500 failed to bind the token to the workspace`. When the workspace is gone, the token is revoked and the answer is `404 workspace not found`.
  11. Emit `hub.token.bind` (PLAT-FR-067). Answer `201` with `Cache-Control: no-store` and the body of §3.5.

  No path logs the token, apikit's answer body or the `Authorization` header. A failed revocation during rollback is logged with both errors and does not change the answer.
- **PLAT-FR-061 (list).** `GET /workspaces/:slug/tokens` runs these checks in order:
  1. No credential: 401.
  2. For PATs: bound gives `403 token_bound` (reported). Then, without `tokens:read` or `tokens:manage`: `403 PAT requires tokens:read scope`, without an `error_type`.
  3. Access with need `private`: 404.
  4. List the tokens bound to the slug, joined with `pats`. Revoked and expired tokens are included. Bindings whose `pats` row is gone are skipped. Order by `pats.created_at`, then `token_id`. The list works on archived workspaces. An admin token may list.
- **PLAT-FR-062 (workspace delete).** In the same transaction as the workspace deletion, the hub revokes the live tokens bound to the slug (`revoked_at = now`) and deletes every binding row of the slug. The revoked ids, sorted, appear in the delete's audit metadata as `revoked_tokens` (workspaces area). Archiving does not touch bindings.
- **PLAT-FR-063 (binding sweep).** One transaction per maintenance pass:
  - for every distinct bound slug with no `workspaces` row, revoke the live bound tokens and delete those bindings;
  - delete every binding whose `token_id` has no `pats` row (for example after a user is deleted);
  - keep the bindings of revoked or expired tokens whose workspace exists.

  Any error rolls the whole pass back.
- **PLAT-FR-064 (binding guard, hub routes).** The guard covers every route registered on the API group after the identity routes, which means every hub route and every unmatched method/path under the mount point. It runs after authentication:
  - A non-PAT, or a request without credentials, passes without a lookup.
  - A PAT with no token id in its auth context gets the id parsed from its Bearer header (PLAT-FR-065). If no id can be derived, the PAT counts as unbound.
  - For a bound PAT:
    - When the route template contains the segments `workspaces/:slug` and the `:slug` value differs from the bound slug, the answer is `404 workspace not found` (no `error_type`), reported. This comes before any scope, existence or status check.
    - When the value equals the bound slug, the request passes to the route's own checks.
    - Otherwise, when the route (method + template) is on the bound-token allowlist, the request passes and the handler confines the result.
    - Otherwise the answer is `403 this token is bound to workspace <slug>`, `error_type: token_bound`, reported. This also covers paths that match no route; such paths would otherwise get `404 Not Found`.
  - A lookup error is `500 internal server error`.

  The allowlist, with paths relative to the mount point, is: `GET /workspaces`, `POST /sessions`, `POST /sessions/:id/complete`, `POST /sessions/:id/usage`, `GET /sessions`, `GET /sessions/:id`, `GET /sessions/:id/usage`, `GET /audit`, `GET /events`, `GET /sandbox-profiles`.
- **PLAT-FR-065 (identity-route guard).** It covers all routes the identity layer registers: `/auth/*`, `/user*`, `/users*` and `/orgs*` without `/secrets` or `/vars` (§3.5). It runs **before** authentication on the root router:
  - If the request's method and template match one of these routes and the `Authorization` header holds a PAT id, the binding is looked up.
  - The header format is `<Bearer> [<Bearer> ...] af_pat_<id>_<anything>`. Each `Bearer` is matched case-insensitively and repeated schemes are skipped. The id must be non-empty and followed by `_`.
  - When the id is bound, the answer is `403 this token is bound to a workspace`, `error_type: token_bound` (the workspace is not named). This is reported without a user id.
  - **The secret is not checked**, so any header carrying a bound token's id is refused this way, before a 401 would be given.
  - A lookup error is 500. Requests without a PAT id cause no query.
- **PLAT-FR-066 (refusal reporting).** Every refusal point reports exactly one refusal per refused request. Filtering a list is not a refusal. A report carries:
  - token id;
  - user id (empty from the identity-route guard);
  - bound slug;
  - requested slug, when the request named one;
  - method and route template (both empty when the refusal point has no request);
  - status (403 or 404);
  - surface (`api` or `git`).
- **PLAT-FR-067 (rate limit of `hub.token.bound_refused`).** The limiter keeps per-token state in memory, so it resets at restart:
  - A token's first refusal emits an event.
  - Refusals within 1 minute of the token's last emitted event are only counted.
  - The first refusal ≥1 minute after the last event emits an event with `suppressed` = the count held back since then.
  - On every report, entries with nothing held back and a last event ≥1 minute old are dropped. An entry with a held-back count stays until that token's next refusal.
  - An emit error is logged and never changes the HTTP answer.

#### HTTP conventions

- **PLAT-FR-070 (middleware order).** Requests pass these middlewares in this order:
  1. **Root**, on every route:
     1. panic recovery: `500 internal server error`, with the panic and stack logged;
     2. request id: an incoming `X-Request-ID` is reused if it is a valid UUID v4, else a new v4 is generated. It is echoed in the `X-Request-ID` response header;
     3. security headers on every response: `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`;
     4. access log;
     5. the identity-route guard;
     6. HTTP metrics.
  2. **API group**, under the mount point:
     1. `Cache-Control: no-store`;
     2. the body-size limit;
     3. Content-Type enforcement;
     4. authentication. This applies to all routes except `GET /auth/providers` and `POST /auth/callback`;
     5. the binding guard, on hub routes and unmatched paths.

  So 413 and 415 are answered before 401.
- **PLAT-FR-071 (body limit).** The limit applies only under the mount point.
  - A request whose `Content-Length` exceeds `max_body_size` (default 1 MiB) gets `413 payload too large` before routing reaches the handler.
  - A streamed body is capped at the same size. Handlers usually turn the overflow into their own `400 invalid request body`. If the overflow error escapes, the answer is 413 `payload too large`.
  - Git routes and the webhook ingress have their own limits (other areas).
- **PLAT-FR-072 (Content-Type).** On POST, PUT and PATCH under the mount point with `Content-Length != 0`, a `Content-Type` that does not start with `application/json` gets `415 unsupported media type`. GET, DELETE, HEAD and OPTIONS are never checked. A request without a body is exempt.
- **PLAT-FR-073 (authentication answers).** All of these carry no `error_type`:
  - no header: `401 missing authorization header`;
  - a header that does not start with `Bearer `: `401 invalid authorization header format`;
  - `Bearer ` and nothing more: `401 missing token`;
  - an unclassifiable token: `401 unrecognized token format`;
  - then the validation answers of §3.3.2: `401 invalid credentials`, `401 credential revoked`, `401 credential expired`, `403 user is blocked`, or `500 internal server error`.
- **PLAT-FR-074 (routing misses).** Anything outside the mount point that matches no route, including a wrong method on an existing root path, gets `404` `{"error":{"code":404,"message":"Not Found"}}`. A miss under the mount point, a wrong method on an existing API path included, goes through the API group chain: `401`, or `403 token_bound` for a bound PAT, or `404 Not Found`. No route ever answers 405. Trailing slashes are not normalised.
- **PLAT-FR-075 (caching headers).**
  - API group: `no-store`.
  - `GET /auth/providers`: `public, max-age=300`, overriding `no-store`.
  - `/healthz` and `/readyz`: `no-cache`.
  - `/version`: `public, max-age=300`.
  - `/metrics`: no Cache-Control.
  - The bound-token mint 201 explicitly sets `no-store`.
- **PLAT-FR-076 (ETags).**
  - `GET /user`, `GET /users/:id` and `GET /orgs/:id` set the weak ETag `W/"<updated_at RFC 3339 UTC>"`.
  - `GET /user/keys` sets `W/"<max(created_at, revoked_at) over the user's keys>-<count of revoked keys>"`, and only when the user has a key.
  - An `If-None-Match` that matches the value exactly gets `304` with no body. The ETag header is still sent.
- **PLAT-FR-077 (timestamps).** Identity timestamps are stored and rendered as `YYYY-MM-DDTHH:MM:SSZ` in UTC with whole seconds. There are two exceptions: the API key revoke response's `revoked_at`, and the `created_at` after a key refresh, both RFC 3339 with nanoseconds.
- **PLAT-FR-078 (list limits).** These identity lists return at most 200 rows:
  - `GET /users`: `created_at DESC`;
  - `GET /orgs`: `name ASC`;
  - `GET /orgs/:id/members`: `username ASC`;
  - `GET /user/keys`, `GET /user/tokens`, `GET /users/:id/keys`, `GET /users/:id/tokens`: `created_at DESC`.

  `GET /user/orgs` is unbounded and unordered. Lists are always `[]`, never `null`.

#### CLI global behaviour

- **PLAT-FR-080 (credential resolution).** Every command except `login`, `version` and `help` resolves three settings before it runs. Each setting takes the first non-empty value in this order:
  1. the flag, when given and non-empty;
  2. the `AF_`-prefixed environment variable;
  3. the bare environment variable;
  4. the config file.

  | Setting | Flag | Env (prefixed, then bare) | File key | Required |
  |---|---|---|---|---|
  | Hub URL | `--endpoint-url` | `AF_ENDPOINT_URL`, `ENDPOINT_URL` | `endpoint_url` | yes |
  | Credential | `--api-key` | `AF_API_KEY`, `API_KEY` | `api_key` | yes |
  | User id | `--user-id` | `AF_USER_ID`, `USER_ID` | `user_id` | no |

  A missing required value fails before any request with `<field> is not set: provide via <flag>, $<AF_VAR>, or config file`, for example `endpoint_url is not set: provide via --endpoint-url, $AF_ENDPOINT_URL, or config file`. It is printed as `{"error":{"code":0,"message":...}}` and the exit status is 2. The order of checks is: positional-argument validation, then config dir and file creation, then credential resolution, then cobra-required flags, then the command body.
- **PLAT-FR-081 (requests).**
  - Requests go to `<endpoint_url without trailing "/">` + `/api/v1` + path. The `/api/v1` part is **hard-coded**, so a server with another `mount_point` is not reachable by `parley`.
  - Headers: `Authorization: Bearer <credential>`, `Accept: application/json`, and `Content-Type: application/json` when there is a body.
  - Each request has a 30-second timeout, unless the command's context already has a deadline (the `--wait` loop does).
  - A non-JSON success body is a client error, exit 2.
- **PLAT-FR-082 (output).** Success prints one JSON document on stdout, indented 2 spaces, with HTML characters not escaped, followed by a newline. An error prints a JSON envelope on stdout:
  - **API error** (status ≥ 400): `{"error":{"code":<status from envelope or HTTP>,"message":<server message or HTTP status text>}}`. The `error_type` the server sent is **dropped**.
  - **Client errors** raised by command code: code `2` (or another chosen code) with the message.
  - **Other errors** (cobra usage errors, `login` failures, credential-resolution failures): `{"error":{"code":0,"message":<text>}}`.

  Hints, warnings and progress go to stderr.
- **PLAT-FR-083 (exit codes).**

  | Code | When |
  |---|---|
  | 0 | success |
  | 1 | an API error (the envelope's code is ≥ 400) |
  | 1 | a `--wait` job ending `dead_letter` or `cancelled` **[FIX-07]** |
  | 1 | a clone ending `failed` |
  | 1 | a wait timeout |
  | 2 | a usage error |
  | 2 | a client-side validation error |
  | 2 | a missing endpoint or credential |
  | 2 | network failure or a client timeout |
  | 2 | an undecodable response |
  | 2 | any error from `login` |
  | 2 | a Go runtime panic |
  | 3 | only `parley workspace sync --fail-on-diverged` with diverged branches |
- **PLAT-FR-084 (waiting).** Commands with `--wait` also take `--timeout` (duration, default `5m0s`) and `--poll-interval` (duration, default `5s`). Both flags are registered whether or not `--wait` is given, and have no effect without it. With `--wait`:
  1. The first answer is printed as one compact JSON line.
  2. The status endpoint is polled at once, then every poll interval, all under one overall deadline of `--timeout`.
  3. A job is done at `completed`, `dead_letter` or `cancelled`. **[FIX-07]** `failed` means the job is waiting to retry, so polling continues; in the baseline the wait stopped there and exited 1. A clone is done at `ready` or `failed`, because a clone's `failed` is final.
  4. When it is done, the final record is printed as indented JSON. For any state other than `completed` or `ready`, stderr gets `Job ended with status <s>.` (or `Workspace clone failed.`) and the exit status is 1.
  5. When the deadline passes, stderr gets `Timed out waiting for <job|workspace clone> to complete after <timeout>`, stdout gets `{"error":{"code":1,"message":"timed out after <timeout>"}}`, and the exit status is 1.
  6. A poll that returns an API error ends the wait with that error's envelope and exit status.
  7. A response that is not a JSON object keeps the poll going.
- **PLAT-FR-085 (path arguments).** Hub commands percent-encode every user-supplied path segment as one segment (Go `url.PathEscape`), so `/` becomes `%2F`. The one exception is `parley rerere forget`, covered by its own section. The identity commands (`user`, `keys`, `tokens` without `--workspace`, `orgs`, `admin`) put their arguments into the path **unescaped**.

#### Scrubbing

- **PLAT-FR-090 (Text).** `Text(text, values...)` makes text safe to log, audit or return:
  1. Invalid UTF-8 is replaced with U+FFFD.
  2. The non-empty values, deduplicated and sorted longest first, are each replaced by `[redacted]`.
  3. URL user information is removed (PLAT-FR-091).
  4. Steps 2 and 3 repeat until the text stops changing, at most 8 more passes. These passes leave out values that are substrings of `[redacted]`.
  5. If any such value is still present, the result is exactly `[redacted]`.
  6. Otherwise the text is cut to at most **1024 bytes** on a rune boundary. The cut comes last, so it never splits a value.
- **PLAT-FR-091 (Userinfo).** Every match of `(?i)([a-z][a-z0-9+.\-]*://)[^/\s?#]*@` is replaced by its scheme part. This removes everything up to the last `@` before the first `/`, `?`, `#` or whitespace. Scheme-less forms (`user@host:path`, e-mail addresses) are left alone. The function is idempotent.
- **PLAT-FR-092 (TruncateUTF8).** It cuts to at most N bytes without splitting a rune.
- **PLAT-FR-093 (where applied).** Each area owns the details. Scrubbing is applied in these places:
  - the forge client: forge error messages and forwarded forge texts scrubbed of the workspace token; comment URLs and event URLs in audit metadata; payload URLs stripped of userinfo; issue and PR titles cut to 256 bytes; webhook event names cut to 64 bytes;
  - sandboxes: provider and remote-gateway error texts and notes scrubbed of secret values and the gateway token; profile `env` values always render as `[redacted]`;
  - the workspace branch push: error messages, audit fields `branch` and `remote_url` and `resource_id`, logs; the remote URL is stripped of userinfo;
  - carry-patch push control: error messages stripped of userinfo.

### 3.5 REST API

Paths are relative to the server root. `<mp>` is `[server].mount_point`, default `/api/v1`. All JSON error bodies use the envelope of §3.10. "Admin" means PLAT-FR-045.

**Shapes used below:**

- `User` (the users routes and `/user`):

  ```json
  {"id":"uuid","username":"alice","email":"a@x","full_name":"","role":"user","status":"active",
   "provider":"github","provider_id":"123","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}
  ```

  `full_name` is always a string here; NULL renders as `""`.
- `Org`:

  ```json
  {"id":"uuid","name":"acme","slug":"acme","url":"","owner_id":"uuid","status":"active","created_at":"...","updated_at":"..."}
  ```

  `owner_id` is omitted when NULL. It is always omitted in `GET /user/orgs`.
- `PAT`: `{"token_id":"k3x9q2mz","name":"ci","permissions":["git:read"],"expires_at":"..."|null,"created_at":"...","revoked_at":"..."|null}`.

#### `GET /healthz`
- **Purpose:** liveness probe.
- **Auth / scope:** none. It is not subject to either binding guard.
- **Response:** `200` `{"status":"ok"}`, with `Content-Type: application/json; charset=utf-8` and `Cache-Control: no-cache`.
- **Errors:** none.
- **Side effects:** none. It is access-logged at debug level only when `logging.log_health_probes = true`, and otherwise not at all.

#### `GET /readyz`
- **Purpose:** readiness probe.
- **Auth / scope:** none.
- **Behaviour:** pings SQLite.
- **Response:** `200` `{"status":"ready"}`. When the ping fails: `503` `{"status":"not ready"}`. The 503 body is not the error envelope. `Cache-Control: no-cache`.

#### `GET /version`
- **Purpose:** build information.
- **Auth / scope:** none.
- **Response:** `200`, `Cache-Control: public, max-age=300`:

  ```json
  {"go_version":"<build version string>","build_time":"<UTC timestamp or empty>","commit":"<short sha or 'dev'>","mount_point":"/api/v1"}
  ```

  `go_version` carries the **release version** set at build time (default `dev`; the release build uses `git describe --tags`, falling back to `0.1.0`). It is not the Go toolchain version.

#### `GET /metrics`
- **Purpose:** Prometheus exposition. The metric families are described by the audit area.
- **Auth / scope:** none. It is not cached and sits outside the mount point.
- **Response:** `200`, Prometheus text format.

#### `GET <mp>/auth/providers`
- **Purpose:** list the OAuth providers for the login.
- **Auth / scope:** public, no authentication middleware. The identity-route guard still applies: a request whose Bearer header carries a bound PAT id gets 403 `token_bound`.
- **Response:** `200` `[{"name":"github","authorize_url":"https://github.com/login/oauth/authorize?client_id=...&redirect_uri=&scope=user%3Aemail&state="}]`. The list is sorted by name and is `[]` when no provider is configured. `Cache-Control: public, max-age=300`.

#### `POST <mp>/auth/callback`
- **Purpose:** exchange an OAuth code for the user and a new API key.
- **Auth / scope:** public. The identity-route guard applies. Content-Type JSON is required when there is a body.
- **Request body:**

  | Field | Type | Required | Default | Validation |
  |---|---|---|---|---|
  | `provider` | string | yes | | must be a configured provider name |
  | `code` | string | yes | | non-empty |
  | `redirect_uri` | string | yes | | PLAT-FR-021 allowlist |
  | `expires` | int | no | 90 | 0, 30, 60 or 90 |
- **Behaviour:** PLAT-FR-021–023, PLAT-FR-030.
- **Response:** `200`:

  ```json
  {"user":{"id":"...","username":"...","email":"...","full_name":null,"status":"active","role":"user",
           "provider":"github","provider_id":"123","created_at":"...","updated_at":"..."},
   "api_key":{"key":"af_<key_id>_<secret>","key_id":"<key_id>","expires_at":"..."|null}}
  ```

  `full_name` is `null` for new users. For existing users it is the stored value, which may be null.
- **Errors** (in check order; the bodies use the envelope keys, without an explicit charset):
  - 400, the binder's error text: an unparseable body.
  - 400 `provider is required`, `code is required`, `redirect_uri is required`.
  - 400 `expires must be 0, 30, 60, or 90`.
  - 400 `unknown provider: <p>`.
  - 400 `redirect_uri is not allowed`.
  - 401 `authorization code exchange failed`: a non-2xx answer, an `error` field or an empty `access_token`.
  - 502 `failed to retrieve user info from provider`.
  - 400 `provider returned empty email; email is required`.
  - 502 `provider returned empty user id`.
  - 403 `user is blocked`.
  - 409 `user already exists`: any constraint violation.
  - 500 `internal server error`.
- **Side effects:**
  - user insert or update;
  - personal org and membership for a new user;
  - every live API key of the user is revoked;
  - a new `api_keys` row;
  - for a new user only, a possible auto-promotion to admin.

#### `POST <mp>/users`
- **Purpose:** create a user without OAuth.
- **Auth / scope:** admin only. Otherwise `403 forbidden`.
- **Request body:** `username`, `email`, `provider`, `provider_id`, all strings and all required (non-empty).
- **Behaviour:**
  1. Admin check.
  2. Bind: `400 invalid request body`.
  3. Required fields, in the order username, email, provider, provider_id: `400 missing required field: <field>`.
  4. Insert in a transaction with `role user`, `status active`, `full_name ""` and a new UUID. There is no auto-promotion.
  5. Run the personal-org hook.
  6. Commit.
- **Response:** `201` `User`.
- **Errors:**
  - 403 `forbidden`;
  - 400s as above;
  - 409 `username already exists` / `provider identity already exists` / `email already exists`;
  - 500 `internal server error`, which includes a hook failure such as the personal-org name or slug collision.
- **Side effects:** user, personal org and membership.

#### `GET <mp>/users`
- **Auth / scope:** admin.
- **Query:** `include_blocked`. Only the exact string `true` includes blocked users. By default only `status = active` users are listed.
- **Response:** `200` `[User]`, at most 200, `created_at DESC`.
- **Errors:** 403 `forbidden`; 500.

#### `GET <mp>/users/:id`
- **Auth / scope:** admin.
- **Path/query params:** `:id` is a user selector (PLAT-FR-046).
- **Response:** `200` `User` with an ETag, or `304` (no body) when `If-None-Match` matches.
- **Errors:** 403 `forbidden`; 404 `user not found`; 500.

#### `PATCH <mp>/users/:id`
- **Auth / scope:** admin.
- **Request body:** `{"full_name": string}`, required. Null or absent is refused. `""` clears it.
- **Behaviour:** admin check → resolve `:id` (404) → bind (400 `invalid request body`) → `full_name` present (400 `missing required field: full_name`) → update `full_name` and `updated_at`.
- **Response:** `200` `User`.
- **Errors:** 403, 404 `user not found`, 400, 500.

#### `DELETE <mp>/users/:id`
- **Auth / scope:** admin.
- **Behaviour:**
  1. Resolve `:id` (404 `user not found`).
  2. The last-admin check, when the target is an active admin: 409 `cannot delete the last admin`.
  3. In a transaction:
     1. recheck the last-admin rule;
     2. `orgs.owner_id := NULL` where it is this user;
     3. delete the user's `api_keys`, `pats` and `org_members` rows;
     4. delete the user.

  The hub registers no delete hooks. The user's workspaces stay, with a dangling `owner_id`, and are reachable only by the admin token. The user's bindings are later removed by the sweep.
- **Response:** `204`.
- **Errors:** 403, 404, 409, 500.

#### `POST <mp>/users/:id/promote` · `POST <mp>/users/:id/demote` · `POST <mp>/users/:id/block` · `POST <mp>/users/:id/unblock`
- **Auth / scope:** admin. No body is needed.
- **Behaviour:** resolve the user. When the target already has the requested role or status, the call is idempotent: 200 with the unchanged user and no write. Otherwise set `role` (`admin` or `user`) or `status` (`blocked` or `active`) and `updated_at = now`. Demote is atomic with the last-admin rule.
- **Response:** `200` `User` with the new values.
- **Errors:** 403 `forbidden`; 404 `user not found`; 409 `cannot demote the last admin` (demote only); 500.

#### `GET <mp>/users/:id/keys`
- **Auth / scope:** admin.
- **Response:** `200` `[{"key_id","user_id","created_at","expires_at"|null,"revoked_at"|null}]`, at most 200, `created_at DESC`.
- **Errors:** 403; 404 `user not found`; 500.

#### `DELETE <mp>/users/:id/keys/:key_id`
- **Auth / scope:** admin.
- **Behaviour:** resolve the user (404 `user not found`) → the key must belong to that user (404 `api key not found`). An already revoked key gives 204 with no write. Otherwise set `revoked_at`.
- **Response:** `204`.

#### `GET <mp>/users/:id/tokens`
- **Auth / scope:** admin.
- **Response:** `200` `[{"token_id","name","permissions":[...],"user_id","created_at","expires_at"|null,"revoked_at"|null}]`, at most 200, `created_at DESC`.
- **Errors:** 403, 404 `user not found`, 500.

#### `DELETE <mp>/users/:id/tokens/:token_id`
- **Auth / scope:** admin.
- **Behaviour:** resolve the user (404 `user not found`) → the token must belong to that user (404 `token not found`). An already revoked token gives 204 with no write. Otherwise set `revoked_at`. The binding row, if any, stays.
- **Response:** `204`.

#### `GET <mp>/user`
- **Auth / scope:** any credential. PATs need `users:read` (else `403 insufficient permissions`).
- **Response:** `200` `User` with an ETag, or `304`.
- **Errors:** 403; 404 `user not found` (always the answer for an admin token, which has no user); 500.

#### `PATCH <mp>/user`
- **Auth / scope:** PATs need `users:read`. No write scope exists for this route.
- **Request body:** `{"full_name": string}`, required.
- **Behaviour:** scope → bind (400 `invalid request body`) → presence (400 `missing required field: full_name`) → user exists (404) → update.
- **Response:** `200` `User`.

#### `GET <mp>/user/orgs`
- **Auth / scope:** PATs need `orgs:read`.
- **Response:** `200` `[Org without owner_id]`: the active orgs the caller is a member of. Admin tokens get `[]`.
- **Errors:** 403 `insufficient permissions`; 500.

#### `GET <mp>/user/keys`
- **Auth / scope:** PATs need `keys:read`.
- **Response:** `200` `[{"key_id","created_at","expires_at"|null,"revoked_at"|null}]` with the keys ETag (PLAT-FR-076), or `304`.

#### `POST <mp>/user/keys/:key_id/refresh`
- **Auth / scope:** API key (or admin token, which always gets 404). PATs get `401 API key authentication required`.
- **Behaviour:** PLAT-FR-038.
- **Response:** `200` `{"key":"af_<key_id>_<new secret>","key_id":"...","expires_at":"<RFC 3339, whole seconds>"|null}`.
- **Errors:** 401; 404 `key not found`; 400 `cannot refresh a revoked key` / `cannot refresh an expired key`; 500.

#### `DELETE <mp>/user/keys/:key_id`
- **Auth / scope:** PATs need `keys:manage`.
- **Response:** `200` `{"key_id":"...","revoked_at":"<RFC 3339 with nanoseconds>"}`.
- **Errors:** 403 `insufficient permissions`; 404 `key not found`; 400 `key is already revoked`; 500.

#### `POST <mp>/user/tokens`
- **Auth / scope:** PATs need `tokens:manage`, and the subset rule applies to them.
- **Request body:**

  | Field | Type | Required | Default | Validation |
  |---|---|---|---|---|
  | `name` | string | yes | | 1–255 bytes |
  | `permissions` | string[] | yes | | ≥1 entry; each registered (§3.3.5) |
  | `expires` | int | no | 90 | 0, 30, 60 or 90 |
- **Behaviour:** PLAT-FR-035.
- **Response:** `201` `{"token_id","name","token":"af_pat_<id>_<secret>","permissions":[...],"expires_at":"..."|null,"created_at":"..."}`.
- **Errors:** PLAT-FR-035, in that order.

#### `GET <mp>/user/tokens`
- **Auth / scope:** PATs need `tokens:read`. `tokens:manage` does not satisfy this check.
- **Response:** `200` `[PAT]`: all of the caller's PATs, any status, bound ones included (with no binding information), at most 200.

#### `GET <mp>/user/tokens/:token_id`
- **Auth / scope:** PATs need `tokens:read`.
- **Response:** `200` `PAT`.
- **Errors:** 404 `token not found`, which is also the answer for another user's token.

#### `DELETE <mp>/user/tokens/:token_id`
- **Auth / scope:** PATs need `tokens:manage`.
- **Response:** `200` `PAT` with `revoked_at` set.
- **Errors:** 403; 404 `token not found`; 400 `token already revoked`; 500.

#### `PUT | PATCH | DELETE <mp>/user/tokens/:token_id/permissions`
- **Auth / scope:** PATs need `tokens:write` or `tokens:manage`.
- **Request body:** `{"permissions": [string]}`.
- **Behaviour:** PLAT-FR-036.
- **Response:** `200` `PAT` after the change. `revoked_at` is set when the change auto-revoked the token, and `permissions` is then `[]`.
- **Errors:** 403 `insufficient permissions` / `cannot grant permission: <p>`; 400 `invalid request body` / `permissions are required` / `token is revoked` / `token is expired` / `invalid permission format: <p>` / `unknown permission: <p>`; 404 `token not found`; 500.

#### `POST <mp>/orgs`
- **Auth / scope:** admin.
- **Request body:**

  | Field | Type | Required | Validation |
  |---|---|---|---|
  | `name` | string | yes | trimmed; must be non-empty |
  | `slug` | string | yes | 1–128 bytes; a 1-byte slug is `[a-z0-9]`; otherwise `^[a-z0-9][a-z0-9_-]*[a-z0-9]$`; not trimmed |
  | `url` | string | no | not validated |
  | `owner_id` | string | no | not validated |
- **Behaviour:** admin → bind (400 `invalid request body`) → `name is required` → `slug is required` → `invalid slug format` → insert. No membership row is created.
- **Response:** `201` `Org`.
- **Errors:** 403 `forbidden`; 400s as above; 409 `organization name already exists` / `organization slug already exists`; 500.

#### `GET <mp>/orgs`
- **Auth / scope:** admin.
- **Query:** `include_blocked=true`.
- **Response:** `200` `[Org]`, at most 200, `name ASC`. Active orgs only by default.

#### `GET <mp>/orgs/:id`
- **Auth / scope:** PATs need `orgs:read` (else `403 insufficient permissions`). Admins see any org. Other callers must be members.
- **Path/query params:** `:id` is a UUID or a slug.
- **Behaviour:** scope → resolve (404 `organization not found`) → membership (403 `forbidden`). An existing org therefore gets 403 and a missing one 404.
- **Response:** `200` `Org` with an ETag, or `304`. A blocked org is still returned.

#### `PATCH <mp>/orgs/:id`
- **Auth / scope:** admin.
- **Request body:** `name` (string, optional, trimmed, non-empty if present) and `url` (string, optional). At least one must be present. `slug` is ignored.
- **Behaviour:** admin → resolve (404) → bind (400) → `no fields to update` (400) → `name is required` (400) → update.
- **Response:** `200` `Org`.
- **Errors:** 409 `organization name already exists`.

#### `DELETE <mp>/orgs/:id`
- **Auth / scope:** admin.
- **Behaviour:** delete the row; memberships cascade. Personal orgs are deletable. The workspaces of the org keep their `org_id`, so members lose access.
- **Response:** `204`.
- **Errors:** 404 `organization not found`.

#### `POST <mp>/orgs/:id/block` · `POST <mp>/orgs/:id/unblock`
- **Auth / scope:** admin.
- **Behaviour:** idempotent. Already in the requested state gives 200 unchanged.
- **Response:** `200` `Org`.
- **Errors:** 404.

#### `GET <mp>/orgs/:id/members`
- **Auth / scope:** PATs need `orgs:read`. Admins, or members of the org.
- **Behaviour:** scope → resolve (404 `organization not found`) → membership (403 `forbidden`).
- **Response:** `200` `[{"org_id","user_id","username","email","role":"<the user's global role>","created_at":"<membership time>"}]`, at most 200, `username ASC`.

#### `PUT <mp>/orgs/:id/members/:user_id`
- **Auth / scope:** admin.
- **Behaviour:** resolve the org (404 `organization not found`) → resolve the user selector (404 `user not found`) → insert. When the membership already exists, the answer is `204` (idempotent).
- **Response:** `204`, no body.

#### `DELETE <mp>/orgs/:id/members/:user_id`
- **Auth / scope:** admin.
- **Behaviour:** resolve both → delete.
- **Response:** `204`.
- **Errors:** 404 `organization not found` / `user not found` / `membership not found`.

#### `POST <mp>/workspaces/:slug/tokens`
- **Purpose:** mint a PAT bound to the workspace.
- **Auth / scope:**
  - An API key, or an unbound PAT. The PAT needs `tokens:manage`, enforced by the delegated create.
  - Need `private`: owner only. A member or stranger gets 404.
  - Admin tokens are refused.
  - A bound PAT on its own slug gets `403 token_bound`. On another slug it gets 404 from the guard.
- **Path/query params:** `:slug`, the workspace slug.
- **Request body:** identical to `POST <mp>/user/tokens` (`name`, `permissions`, `expires`).
- **Behaviour:** PLAT-FR-060.
- **Response:** `201`, `Cache-Control: no-store`:

  ```json
  {"token_id":"k3x9q2mz","token":"af_pat_k3x9q2mz_<secret>","name":"agent","permissions":["workspaces:read"],
   "workspace":"my-ws","expires_at":"2027-01-19T10:00:00Z","created_at":"2026-10-21T10:00:00Z"}
  ```

  `permissions` and `expires_at` are copied verbatim from the create answer. `expires_at` is null when the token never expires.
- **Errors** (in check order):
  - 401 `authentication required`;
  - 403 `this token is bound to workspace <S>` (`token_bound`);
  - 403 `admin tokens cannot mint bound tokens; a real user is required as the token's owner`;
  - 404 `workspace not found`;
  - 400 `workspace is archived`;
  - 400 `invalid request body`: the body could not be read;
  - any non-201 answer of the delegated create, passed through: 400 `name is required`, 403 `insufficient permissions`, 403 `cannot grant permission: <p>`, and so on;
  - 500 `failed to bind the token to the workspace` (the token is revoked);
  - 404 `workspace not found`: the workspace vanished during the mint (the token is revoked).
- **Side effects:**
  - a `pats` row for the caller's user;
  - a `token_workspace_bindings` row;
  - the `hub.token.bind` event;
  - a second access-log line for the in-process `POST <mp>/user/tokens`.

#### `GET <mp>/workspaces/:slug/tokens`
- **Purpose:** list the tokens bound to the workspace.
- **Auth / scope:** API key, admin token, or unbound PAT with `tokens:read` or `tokens:manage`. Need `private`. Bound PATs get `403 token_bound` on their own slug and 404 on another.
- **Response:** `200` `[{"token_id","name","permissions":[...],"workspace":"<slug>","expires_at":string|null,"revoked_at":string|null,"created_at"}]`:
  - `[]` when there is nothing to list;
  - permissions that cannot be parsed render as `[]`;
  - an empty stored timestamp renders as null.
- **Errors:** 401; 403 `token_bound`; 403 `PAT requires tokens:read scope`; 404 `workspace not found`; 500.

### 3.6 CLI

Global flags, available on every command:

| Flag | Type | Default | Meaning |
|---|---|---|---|
| `--endpoint-url` | string | `""` | hub URL (PLAT-FR-080) |
| `--api-key` | string | `""` | credential (API key or PAT) |
| `--user-id` | string | `""` | user id; used only when `keys refresh` saves the config |
| `--json` | bool | false | help output as JSON (see `parley help`) |

There is no shell-completion command. `parley` on its own prints the help and exits 0. A group command without a subcommand (for example `parley tokens`) prints its help and exits 0 without credentials. Unknown commands and flags print `{"error":{"code":0,"message":"unknown command ..."}}` and exit 2.

#### `parley login`
- **Purpose:** authenticate and save the credentials.
- **Arguments / flags:**

  | Flag | Default | Meaning |
  |---|---|---|
  | `--provider <name>` | `github` | provider to sign in with |
  | `--expires <int>` | `90` | key lifetime in days; validated client-side to 0/30/60/90, for the browser flow only |
  | `--api-key <key \| ->` | | local flag that shadows the global one; `-` reads stdin, which is trimmed |
  | `--endpoint-url` | | the global flag |

  The endpoint comes from the flag, else `AF_ENDPOINT_URL`, `ENDPOINT_URL`, then the file. The environment credential variables are **not** used for the key.
- **Calls:**
  - browser flow: `GET /api/v1/auth/providers`, then `POST /api/v1/auth/callback` (each with a 30-second timeout);
  - key flow: `GET /api/v1/user`.
- **Behaviour, browser flow:**
  1. Check that an endpoint is set (`endpoint URL is required for login — use --endpoint-url or set AF_ENDPOINT_URL`).
  2. Validate `--expires` (`--expires must be 0, 30, 60, or 90`).
  3. Fetch the providers. A non-200 answer is `failed to fetch providers: server returned <status>`. A provider that is not listed is `provider '<p>' not found`.
  4. Generate a 64-hex-character state and listen on `127.0.0.1:<random port>`.
  5. Build the authorize URL from the provider's `authorize_url`, setting `redirect_uri=http://127.0.0.1:<port>/callback`, `state` and `response_type=code`.
  6. stderr gets `Opening browser for authentication...`. The browser is opened with `open` on darwin or `xdg-open` on linux. On failure, or on another OS, stderr gets `Open this URL in your browser: <url>`.
  7. Wait up to 120 s for `GET /callback`:
     - a wrong `state` answers 400 with the HTML `<html><body><h1>Login failed</h1><p>OAuth state mismatch. Please try again.</p></body></html>`, and the command fails with `OAuth state mismatch — possible CSRF attack: OAuth state mismatch`;
     - success answers 200 with `<html><body><h1>Login successful</h1><p>You may close this tab.</p></body></html>`;
     - other paths answer 404;
     - a timeout fails with `login timed out waiting for browser callback`.
  8. POST the callback with `{provider, code, redirect_uri, expires}`. A non-200 answer fails with `login failed: <server message>`, or `login failed: server returned HTTP <n>`.
  9. Save `endpoint_url`, `user_id` and `api_key`.
- **Behaviour, key flow:** `GET /user` with the key. Failure gives `login failed: <message>`. A PAT without `users:read`, and an admin token (404), therefore cannot log in. Then the config is saved. No key is created or revoked.
- **Output:** the user object, indented, on stdout; `Logged in as <username>` on stderr.
- **Exit codes:** 0 on success. Every failure exits 2, with `{"error":{"code":0,"message":...}}` on stdout.

#### `parley user show` · `parley user update --full-name <s>`
- **Arguments / flags:** none for `show`. `update` takes `--full-name <string>`, required by cobra (an empty string is allowed).
- **Calls:** `GET /api/v1/user` and `PATCH /api/v1/user` (`{"full_name":...}`).
- **Output:** the user JSON.
- **Exit codes:** 0/1/2.

#### `parley keys list` · `parley keys refresh` · `parley keys revoke`
- **Arguments / flags:** none. No positional arguments.
- **Calls:**
  - `list`: `GET /user/keys`.
  - `refresh`: `POST /user/keys/<key_id>/refresh`, where `key_id` is the second-to-last `_`-separated part of the configured credential (fewer than 3 parts gives `invalid API key format`, exit 2).
  - `revoke`: `DELETE /user/keys/<key_id>`.
- **Output:**
  - `list`: the array.
  - `refresh`: saves `{endpoint_url, api_key: <new key>, user_id: <resolved user id>}` and then prints the response, with `API key refreshed` on stderr.
  - `revoke`: saves `{endpoint_url, api_key: "", user_id: ""}` and then prints `{"key_id","revoked_at"}`, with `API key revoked. Run 'akc login' to obtain a new key.` on stderr.
  - **[FIX-02]** The save writes the config file `parley` reads its credentials from: `$AF_CONFIG_DIR/config.toml`, default `~/.af/config.toml`. It writes a temporary file in that directory and renames it over the file, mode 0600. Other keys in the file are kept.
  - **[FIX-02]** If the save fails, the response is still printed on stdout, so the new key of a refresh is never lost. stderr gets `failed to save config: <err>; update your config manually` and the exit status is 2.
- **Notes:** the baseline saved to `./config.toml` in the working directory, and on a failed save it printed only the error, losing a refreshed key. See §1.8.

#### `parley tokens list [--workspace <slug>]`
- **Calls:** `GET /user/tokens`. With `--workspace`, `GET /workspaces/<escaped slug>/tokens`.
- **Arguments / flags:** `--workspace` is a string. Giving it as empty or whitespace is `--workspace must not be empty` (code 2).
- **Output:** the JSON array.

#### `parley tokens create --name <n> --permissions <list> [--expires <d>] [--workspace <slug>]`
- **Arguments / flags:**
  - `--name` (string, required by cobra);
  - `--permissions` (string, required: comma-separated, each trimmed, empties dropped);
  - `--expires` (int, default 90);
  - `--workspace` (string).
- **Behaviour without `--workspace`:**
  1. Validate the permissions client-side (`--permissions must not be empty`, code 2).
  2. Validate `--expires` (`--expires must be 0, 30, 60, or 90`, code 2).
  3. Send `POST /user/tokens` `{name, permissions, expires}`.
  4. Print the response. stderr gets `Token created. Save the token value — it cannot be retrieved later.`
- **Behaviour with `--workspace`:**
  1. An empty or whitespace `--workspace` is `--workspace must not be empty`. An empty or whitespace `--name` is `--name must not be empty`. No permission left is `--permissions must name at least one permission`. Each exits 2.
  2. `--expires` is **not** validated client-side.
  3. Send `POST /workspaces/<slug>/tokens` `{name, permissions, expires}`.
  4. Print the response. stderr gets `warning: the token in this output will not be shown again; save it now`.

#### `parley tokens show <token_id>` · `parley tokens revoke <token_id>`
- **Arguments / flags:** exactly one argument. A different count is a cobra error, exit 2.
- **Calls:** `GET /user/tokens/<id>`; `DELETE /user/tokens/<id>`.
- **Output:**
  - `show`: the PAT.
  - `revoke`: prints `{}` on stdout (the server's 200 body is discarded) and `Token <id> revoked` on stderr.

#### `parley tokens replace|add|remove <token_id> --permissions <list>`
- **Arguments / flags:** one argument. `--permissions` is required, parsed as for create (`--permissions must not be empty`, exit 2).
- **Calls:** `PUT`, `PATCH` or `DELETE` on `/user/tokens/<id>/permissions` with `{"permissions":[...]}`.
- **Output:** the PAT. When `replace` or `remove` returns a non-null `revoked_at`, stderr gets `Token <id> has been revoked (no permissions remaining)`. With the client-side check, `replace` cannot send an empty list.

#### `parley orgs list` · `parley orgs show <id|slug>` · `parley orgs members <id|slug>`
- **Calls:** `list`: `GET /user/orgs`, the caller's own organizations. Any credential works; a PAT needs `orgs:read`. **[FIX-01]** The baseline called the admin-only `GET /orgs`, so non-admins got `403 forbidden`. The listing of all orgs stays at `parley admin orgs list`. `show`: `GET /orgs/<arg>`. `members`: `GET /orgs/<arg>/members`.
- **Output:** JSON.

#### `parley admin users list [--include-blocked]` · `show|promote|demote|block|unblock <id|username|email>` · `create --username --email --provider --provider-id` · `update <sel> --full-name <s>`
- **Calls:**
  - `list`: `GET /users`, with `?include_blocked=true` when the flag is given;
  - `show`: `GET /users/<sel>`;
  - `promote|demote|block|unblock`: `POST /users/<sel>/<verb>`;
  - `create`: `POST /users` `{username,email,provider,provider_id}`;
  - `update`: `PATCH /users/<sel>` `{full_name}`.
- **Arguments / flags:** a missing positional argument prints `{"error":{"code":0,"message":"missing required argument: id"}}` (exit 2). Extra arguments give `accepts 1 arg(s), received <n>` (exit 2). A required flag that is not given prints `{"error":{"code":0,"message":"missing required flag: --<name>"}}` (exit 2). Flags may be given empty.
- **Output:** JSON via standard marshalling (HTML characters escaped). An API error prints `{"error":{"code":<status>,"message":...}}`, exit 1.
- **Notes:** there is **no** `admin users delete` command, although the route exists.

#### `parley admin orgs list [--include-blocked]` · `create --name --slug [--url]` · `update <id|slug> [--name] [--url]` · `delete|block|unblock <id|slug>` · `members list <id|slug>` · `members add|remove <org> <user>`
- **Calls:**
  - `create`: `POST /orgs` `{name, slug, url?}`. `url` is sent only when the flag is given. `owner_id` cannot be set.
  - `update`: `PATCH /orgs/<id>` with only the given flags. With neither, stderr gets `Warning: no fields specified for update` and `{}` is sent, which the server answers with 400.
  - `delete`, `block`, `unblock`: as named.
  - `members list`: `GET /orgs/<id>/members`.
  - `members add` / `members remove`: `PUT` / `DELETE` `/orgs/<org>/members/<user>`.
- **Arguments / flags:** missing arguments give `missing required argument: id` / `org_id` / `user_id`.
- **Output:** `delete` and `members add|remove` print `{}`. The others print the response.

#### `parley admin keys list <user>` · `parley admin keys revoke <user> <key_id>` · `parley admin tokens list <user>` · `parley admin tokens revoke <user> <token_id>`
- **Calls:** `GET /users/<u>/keys`; `DELETE /users/<u>/keys/<k>`; `GET /users/<u>/tokens`; `DELETE /users/<u>/tokens/<t>`.
- **Output:** `revoke` prints `{}`.
- **Arguments / flags:** missing arguments give `missing required argument: user_id` / `key_id` / `token_id`.

#### `parley version`
- **Purpose:** show the client and server versions. No credential is needed.
- **Behaviour:** when an endpoint resolves (flag, env or file), `GET <endpoint>/version` is called with a 5-second timeout. On failure, stderr gets `warning: could not reach server: <err>` and `server_version` is omitted.
- **Output:** `{"cli_version":"<ver>","build":"<commit>","prefix":"af","server_version":{"go_version","build_time","commit","mount_point"}}`.
- **Exit codes:** 0.

#### `parley help [command]`, `parley <cmd> --help`
- **Purpose:** show the built-in help.
- **Output:**
  - Text help by default.
  - With `--json`, a group or the root prints `{"name":"parley","version":"<cli ver>","commands":[{name, description, method, path, args:[{name}], flags:[{name:"--x", type, required, default?, description}], auth, composite?}]}`. Only commands with annotations and a body appear.
  - With `--json` on a runnable annotated command, only that command's entry is printed.
  - The `auth` values are `none`, `api_key` and `admin`.

### 3.7 Background behaviour

- **Maintenance loop:** PLAT-FR-005. The first pass starts right after the tables exist, concurrently with the rest of the start, so it may overlap with the first requests. The four tasks are audit retention, forge delivery pruning, the binding sweep and the patch purge **[FIX-12]**. There is no jitter, and the interval is fixed at 1 hour.
- **Binding sweep:** PLAT-FR-063.
- **Bound-refusal limiter:** in memory, per token (PLAT-FR-067). Pruning is lazy, on each report.
- **Start-up migrations** that touch this area:
  - the duplicate-e-mail check and `idx_users_email` (fatal on duplicates);
  - the PAT scope rewrite (PLAT-FR-040);
  - `token_workspace_bindings` creation (idempotent).
- **Bootstrap:** PLAT-FR-003. When the bootstrap generates a token, the process exits before anything else starts.
- **Shutdown:** PLAT-FR-004. The 15-second drain is not configurable. In-flight requests that are still running after 15 s are cut off.

### 3.8 Events

These are hub audit events, stored and streamed by the audit area. Common fields are `id`, `event_type`, `actor_id`, `actor_type`, `resource_type`, `resource_id`, `action`, `workspace`, `metadata` and `timestamp`.

| Event | When | actor_id / actor_type | resource_type / resource_id / action / workspace | metadata |
|---|---|---|---|---|
| `hub.token.bind` | once per successful bound mint, after the binding is committed; never for a refused, passed-through or rolled-back mint | the caller's user id / its credential type (`api_key` or `pat`) | `token` / token id / `bind` / slug | `token_id`, `workspace`, `permissions` (string array, `[]` if unparseable), `expires_at` (string, or null when the token never expires). Never the token. |
| `hub.token.bound_refused` | a refusal of a bound token on any surface, rate-limited (PLAT-FR-067) | the token's user id, or `""` from the identity-route guard / `pat` | `token` / token id / `refuse` / bound slug | `token_id`, `bound_workspace`, `requested_workspace` (only when non-empty), `method`, `route`, `status` (int), `surface` (`api` or `git`), `suppressed` (int) |

`hub.token.bind` is emitted on a context that a client disconnect does not cancel. An emit failure is logged (`boundtoken: emitting hub.token.bind failed`) and does not change the answer. No audit events are emitted for user, org, API key or unbound PAT operations.

### 3.9 Configuration

#### config.toml (server)

**`[server]`**

| Key | Type | Default | Validation / meaning |
|---|---|---|---|
| `port` | int | `8080` | 0..65535; 0 picks a free port. An explicit `0` stays 0. |
| `bind` | string | `"0.0.0.0"` | an empty value becomes the default; not validated |
| `external_url` | string | `""` | not validated. Used for the OAuth redirect allowlist (scheme + host:port), workspace `hub_url`, webhook URLs. |
| `mount_point` | string | `"/api/v1"` | an empty value becomes the default; not validated. `parley` always uses `/api/v1`. |
| `max_body_size` | string | `"1MB"` | `<positive int><KB\|MB\|GB>`, case-insensitive, 1024-based |

**`[database]`**

| Key | Type | Default | Meaning |
|---|---|---|---|
| `path` | string | `./data/apikit.db`, or `$XDG_DATA_HOME/apikit.db` when `XDG_DATA_HOME` is set | A value with a directory part is used as is. A bare file name is placed under `$XDG_DATA_HOME` when set, else resolved relative to the working directory. The parent directory is created with mode 0700. The audit DB defaults to `<dir>/audit.duckdb`. |

**`[logging]`**

| Key | Type | Default | Meaning |
|---|---|---|---|
| `level` | string | `"info"` | trace, debug, info, warn, error, fatal or panic (case-insensitive) |
| `log_health_probes` | bool | `false` | `true` logs `/healthz` and `/readyz` at debug level. `false` does not log them. |

The access log is JSON with the fields `method`, `path`, `status`, `duration` (milliseconds, float) and `request_id`.

**`[workspace]`**

| Key | Type | Default | Meaning |
|---|---|---|---|
| `path` | string | `./data/workspaces`, or `$XDG_DATA_HOME/workspaces` | Same resolution rule as `database.path`. Created at start. |
| `workers` | int | `4` | `0` becomes 4. Other values are not validated here. |

**`[[oauth.providers]]`** (repeatable)

| Key | Type | Default | Meaning |
|---|---|---|---|
| `name` | string | required | `github` or `google`; unique |
| `client_id` | string | required | |
| `client_secret` | string | required | |
| `authorize_url` | string | provider default | |
| `token_url` | string | provider default | |
| `userinfo_url` | string | provider default | for GitHub, the e-mails URL is derived from it by replacing a trailing `/user` with `/user/emails` |

**`[sandbox]`** and **`[[sandbox.profiles]]`** are owned by the sandbox area and parsed strictly by it. They are listed here only to complete the key catalogue:
- `[sandbox]`: `provider`, `engine_binary`, `runtime`, `userns`, `max_ttl`, `network_modes`, `max_sandboxes`, `listen`, `tls_cert`, `tls_key`, `external_url`, `lease_timeout`, `reap_interval`, `gateway_url`, `gateway_token`.
- `[[sandbox.profiles]]`: `name`, `default`, `image`, `cpus`, `memory_mb`, `pids`, `disk_mb`, `network`, `ttl`, `secrets`, `optional_secrets`, `env` (table), `require_landlock`.

Any other section or key is ignored.

#### Command-line flags (server)

`--admin-email <string>` and `--reset-admin-token`, as in PLAT-FR-001 and PLAT-FR-003.

#### Environment variables (every variable the hub or parley reads)

| Variable | Read by | Owner area | Default / meaning |
|---|---|---|---|
| `XDG_CONFIG_HOME` | parleyd | platform | Directory of `config.toml` and `admin_token`. Unset means the working directory. Removed from the environment of the podman engine. |
| `XDG_DATA_HOME` | parleyd | platform | Base directory for bare `database.path` and `workspace.path`. Removed from the podman engine environment. |
| any `$VAR` / `${VAR}` referenced in `config.toml` (conventionally `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET`, `AGENTKIT_GATEWAY_TOKEN`) | parleyd | platform / sandbox | expanded into the file text |
| `AF_AUDIT_DB_PATH` | parleyd | audit | path of the DuckDB file; default `<SQLite dir>/audit.duckdb` |
| `AF_SSE_MAX_CONNECTIONS` | parleyd | audit | default `100`. A non-integer or ≤0 value logs a warning and uses the default. |
| `AF_AUDIT_MAX_AGE_DAYS` | parleyd | audit | `90` |
| `AF_AUDIT_MAX_RUNS` | parleyd | audit | `50` |
| `AF_TRACE_MAX_AGE_DAYS` | parleyd | audit | `30` |
| `AF_SESSION_MAX_AGE_DAYS` | parleyd | audit | `90` |
| `AF_SESSION_MAX_ACTIVE_AGE_DAYS` | parleyd | audit | `7` |
| `AF_POSTMORTEM_MAX_AGE_DAYS` | parleyd | audit | `180` |
| `AF_AUDIT_ORPHAN_RETENTION_DAYS` | parleyd | audit | `30`. For each `AF_*_DAYS` and `AF_AUDIT_MAX_RUNS` variable, a non-integer or <1 value logs a warning and uses the default. |
| `AF_AUDIT_WRITE_TIMEOUT_MS` | parleyd | audit | `2000`. An invalid or ≤0 value silently uses the default. |
| `GITHUB_API_URL`, `GITLAB_API_URL` | parleyd (forge client library) | forge | used to decide which forge serves a host |
| `GITHUB_TOKEN`, `GH_TOKEN`, `GITLAB_TOKEN` | — | forge | **[FIX-14]** Not read by the hub. The baseline's forge library consulted them (FRG-FR-031). The hub sends only the workspace's stored credential. |
| `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` | the Go HTTP transport (forge, OAuth) and git | forge / git | standard proxy behaviour |
| `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME`, `GIT_COMMITTER_EMAIL`, `GIT_EDITOR`, `GIT_PAGER` | git subprocesses | carry-patch / merge | Defaulted when absent: identity `parleyd <parleyd@localhost>`, `GIT_EDITOR=true`, `GIT_PAGER=cat`. Repository-redirecting variables (`GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE`, `GIT_COMMON_DIR`, `GIT_NAMESPACE`, `GIT_CONFIG_PARAMETERS`, `GIT_CONFIG_COUNT`, …) are stripped. `GIT_ALLOW_PROTOCOL`, `GIT_TERMINAL_PROMPT=0`, `GIT_CONFIG_NOSYSTEM=1` and `LC_ALL=C` are forced. |
| `PATH` | parleyd | git / sandbox | locates `git` (≥ 2.38) and the container engine |
| `AF_CONFIG_DIR` | parley | platform | CLI config directory; default `$HOME/.af` |
| `HOME` | parley | platform | home directory for `~/.af` |
| `AF_ENDPOINT_URL`, then `ENDPOINT_URL` | parley | platform | hub URL |
| `AF_API_KEY`, then `API_KEY` | parley | platform | credential |
| `AF_USER_ID`, then `USER_ID` | parley | platform | user id |
| `ENDPOINT_URL`, `API_KEY` (bare only) | `parley credential-helper` | git / CLI | the helper ignores the `AF_` variables and `AF_CONFIG_DIR` (credential-helper section) |
| any `$VAR` in `~/.af/config.toml` | parley | platform | expanded when the file is read |

`ADMIN_EMAIL` and `ADMIN_TOKEN` appear in deployment files but are not read by either binary.

#### Build-time constants (externally visible)

- **Token prefix `af`.** It sets the credential formats, the `~/.af` directory and the `AF_` environment prefix. A server and a client built with different prefixes cannot talk to each other.
- **Version, commit and build time.** They appear in `/version` and `parley version`.

### 3.10 Edge cases and error handling

- **Error envelope.** Every JSON error is:

  ```json
  {"error":{"code":<int = HTTP status>,"message":"<text>","error_type":"<type>"}}
  ```

  `error_type` is omitted when empty. The Content-Type is `application/json; charset=utf-8`; the OAuth callback errors and the secrets module omit the explicit charset. Known deviations:
  - `/readyz` 503 is `{"status":"not ready"}`;
  - the SSE 503 is `{"error":"too many SSE connections"}`;
  - git errors are pkt-line or text (git area).
- **All `error_type` values in use** (apikit emits none of its own):

  | error_type | Status | Area |
  |---|---|---|
  | `forbidden` | 403 | access rule (member on a write); carry-patch host |
  | `token_bound` | 403 | binding guards, bound-token routes |
  | `workspace_busy` | 409 | workspace (archive, reclone, branch push, sync), merge (batch rebase), carry-patch (rerere forget, patch reset, sync, patch add, rollback) |
  | `workspace_mode_mismatch` | 400 | carry-patch (rebuild), forge (`target=upstream`) |
  | `no_active_patches` | 400 | carry-patch |
  | `concurrent_rebuild` | 409 | carry-patch |
  | `duplicate_merge` | 409 | merge |
  | `origin_fetch_failed` | 502 | carry-patch (sync, reset-to-origin, patch add) |
  | `missing_on_origin` | 409 | carry-patch |
  | `ref_changed` | 409 | carry-patch |
  | `branch_not_found` | 404 | branch push |
  | `integration_branch` | 400 | branch push |
  | `patch_branch` | 400 | branch push |
  | `protected_branch` | 400 | branch push |
  | `non_fast_forward` | 409 | branch push |
  | `sandboxes_disabled` | 404 | sandboxes |
  | `sandbox_capacity` | 409 | sandboxes |
  | `secret_missing` | 422 | sandboxes |
  | `sandbox_provider_error` | 502 | sandboxes |
  | `body_too_large` | 413 | forge writes, webhook ingress |
  | `forge_api_url_invalid` | 422 | forge |
  | `forge_unknown` | 422 | forge |
  | `forge_not_found` | 404 | forge, branch push |
  | `forge_unauthenticated` | 502 | forge, branch push |
  | `forge_forbidden` | 502 | forge, branch push |
  | `forge_rate_limited` | 429 (+`Retry-After`) | forge |
  | `forge_rejected` | 422 | forge, branch push |
  | `forge_error` | 502 | forge, branch push |
  | `forge_unreachable` | 502 | forge, branch push |
  | `forge_timeout` | 504 | forge, branch push |
  | `workspace_archived` | 409 | audit ingestion |
  | `workspace_access_denied` | 403 | audit ingestion |
  | `unknown_schema_version` | 422 | audit postmortem |
  | `postmortem_not_found` | 404 | audit |
- **Status conventions.**
  - **401:** authentication failed.
  - **403:** three distinct cases:
    - without an `error_type`: a PAT scope refusal, an admin-only route, or a blocked user;
    - `forbidden`: a member writing;
    - `token_bound`: a bound token on a route it may not use.
  - **404:** a missing or invisible resource. It is also the scope answer on the workspace CRUD routes.
  - **409:** a conflict or busy state.
  - **413 / 415:** the body limit and Content-Type rules.
  - **500 `internal server error`:** any database or internal failure, never a grant.
- **Bound token edge cases.**
  - A token bound to an archived workspace still reaches it and gets the route's usual archived answers.
  - A bound token's `tokens:*` scopes do nothing: it gets `403 token_bound` on its own `/tokens` routes and 404 on another workspace's.
  - Revoking a bound token through `DELETE /user/tokens/:id` (with an API key) or the admin route leaves the binding row. The token then gets 401.
  - Changing a bound token's permissions with the owner's API key works. The binding stays.
  - Deleting the user deletes the PATs. The sweep then removes the bindings.
  - A header carrying a bound PAT's id with any secret gets 403 `token_bound` on identity routes, before a 401 would be given (PLAT-FR-065).
  - A bound PAT on a path that matches no route under the mount point, including a wrong method on a workspace path, gets 403 `token_bound`, not 404.
- **Admin token edge cases.**
  - `GET /user` answers 404 `user not found`.
  - `POST /user/tokens` answers 500, because the token has no user.
  - The key routes answer 404 `key not found`.
  - `GET /user/orgs` and `GET /user/tokens` return `[]`.
  - The token cannot mint bound tokens (403).
  - It cannot be stored by `parley login --api-key`.
- **Blocking.**
  - A blocked user's API keys and PATs give `403 user is blocked` on every authenticated route. An OAuth login gives 403.
  - Unblocking restores every credential that is neither revoked nor expired.
  - Blocking an org changes no access decision.
- **OAuth edge cases.**
  - A provider rename of the username to one that is taken keeps the old username silently.
  - A new user whose provider e-mail is already used by another account gets 409 `user already exists`.
  - Every login revokes all other API keys of the user, including keys used by automation.
- **Slug and naming collisions.** The personal org's name must be unique. A pre-existing org named like the user makes the OAuth signup fail with 409, and makes `POST /users` fail with 500 (PLAT-FR-032).
- **Waiting edge cases.**
  - `--poll-interval` ≤ 0 makes `parley` panic (exit 2), because the ticker rejects a non-positive interval.
  - `--timeout` ≤ 0 times out at once (exit 1).
- **parley config writes by `keys refresh` and `keys revoke`.** **[FIX-02]** The save goes to the config file in use, through a temp file in the same directory, so the rename never crosses file systems. If the save fails after the server has rotated or revoked the key, the response is still printed (see §3.6). In the baseline, the save went to `./config.toml` in the working directory, and a failed save lost a refreshed key.
- **parley error output.**
  - The server's `error_type` never reaches `parley`'s output (PLAT-FR-082).
  - A network failure is exit 2, not 1.
  - Several messages name the apikit binary: `no API key configured — run 'akc login' first` and `API key revoked. Run 'akc login' to obtain a new key.`
- **Config edge cases.**
  - Unknown keys are ignored, so a typo silently takes the default.
  - `$` sequences in secrets are expanded and cannot be escaped.
  - `port = 0` binds a random port.
- **Readiness** checks only SQLite. DuckDB, the queue and the sandbox gateway are not checked.

### 3.11 Discrepancies between docs and code

Code wins in every case.

1. **`/readyz` failure body.** `docs/api.md` §Server Endpoints says `503` "with the error envelope". The code returns `503 {"status":"not ready"}` (`apikit/server.go:191`).
2. **`/version` `go_version`.** The `docs/api.md` example shows a Go toolchain version (`"1.27.0"`). The field carries `apikit.Version`, set from `git describe` / `0.1.0` (`apikit/server.go:205`, `hub2/Makefile:14-20`).
3. **`redirect_uri` is required.** `docs/api.md` §Login marks it "no". The code answers `400 redirect_uri is required` (`apikit/internal/oauth/handler.go:77`). The callback errors are also wider than documented: 502 for a userinfo failure or empty provider id, and 400 for an empty e-mail (`handler.go:107-131`).
4. **`full_name` in the callback.** The `docs/api.md` example shows a string. New users get `null` (`handler.go:270`, `callback.go:24`). The users routes always render a string (`users.go:23-33`, `COALESCE`).
5. **`DELETE /user/keys/:key_id`.** `docs/api.md` says `204 No Content`, with only a 404 error. The code returns `200 {"key_id","revoked_at"}` and also `400 key is already revoked` (`apikit/internal/keys/handlers.go:301-360`, message at `:331`).
6. **`GET /user/keys` scope.** `docs/permissions.md` (scope reference "keys:read: None: no route checks it yet", and the account matrix "not checked") is wrong. The route requires `keys:read` for PATs (`keys/handlers.go:94`). `docs/api.md` is right.
7. **`orgs:read` and org reads by members.** `docs/permissions.md` lists `GET /orgs/:id` and `GET /orgs/:id/members` as admin-only ("Admin endpoints"), and `orgs:read` as covering only `GET /user/orgs`. The code admits org members (and admins) on both, with `orgs:read` checked for PATs (`apikit/internal/handlers/orgs.go:221-270, 589-620`). `docs/api.md` lists `GET /orgs/:id/members` under Administration too.
8. **`PUT /orgs/:id/members/:user_id`.** `docs/api.md` says "returns the membership". The code returns `204` with no body, idempotently (`orgs.go:651-692`).
9. **`GET /user/orgs` `owner_id`.** The `docs/api.md` example includes `owner_id`. The code never selects it, so it is omitted (`users.go:1111-1140`, `orgs.go:27` `omitempty`).
10. **`GET /users` and `GET /orgs`.** `docs/api.md` says "List all users / organizations". By default only active rows are listed; `include_blocked=true` adds blocked ones. Both are capped at 200 rows (`users.go:286-289`, `orgs.go:188-191`).
11. **`POST /users` conflicts.** `docs/api.md` names username and provider identity. A duplicate e-mail also gives `409 email already exists` (`users.go:244-246`).
12. **Generic 401 message.** `docs/api.md` §Common answers says `401 ... (authentication required)`. The auth middleware answers `missing authorization header`, `invalid authorization header format`, `missing token`, `unrecognized token format`, `invalid credentials`, `credential revoked` or `credential expired` (`apikit/internal/auth/auth.go:38-56`, `credentials.go:37-44`). `authentication required` appears only in hub handlers whose auth context is missing.
13. **`parley orgs list`.** `docs/cli.md` says "List your organizations". It calls the admin-only `GET /orgs`, so non-admins get `403 forbidden` (`apikit/internal/cli/orgs.go:42-48`). Members' orgs are only at `GET /user/orgs`, which has no CLI command. → **Resolved in this PRD by FIX-01:** the re-implementation follows the corrected behaviour described here (see §1.8).
14. **`parley tokens revoke` output.** The apikit code comment and the general docs imply the 204-style status document. The server returns 200 with the PAT, but the CLI discards it and prints `{}` (`apikit/internal/cli/tokens.go:360-368`).
15. **`parley keys refresh` / `revoke` save location.** `docs/cli.md` says the new key is "saved to the config file". The code saves to `./config.toml` in the working directory (empty `configPath`; `apikit/internal/cli/user.go:45-50`, `keys.go:111,173`, `config.go:70-98`). See §3.10. → **Resolved in this PRD by FIX-02:** the re-implementation follows the documented behaviour described here (see §1.8).
16. **CLI error envelopes.** `docs/cli.md` §Output describes the envelope without `error_type`, which matches the code. `docs/api.md` presents `error_type` as the machine-readable class. Clients should know that `parley` drops it: `CmdClient.DoRequest` decodes only `code` and `message` (`apikit/internal/cli/user.go:192-205`).
17. **Mount point.** `docs/configuration.md` presents `mount_point` as freely configurable. `parley` hard-codes `/api/v1` (`user.go:156`, `login.go:96`), so a non-default mount point breaks the CLI.
18. **Bound-token apikit refusal.** `docs/api.md` says the identity routes refuse a bound token "because the token is refused before it is authenticated". The docs do not say that the secret is never checked: any header carrying a bound token id gets 403 `token_bound` instead of 401 (`hub2/internal/wsaccess/apikit_guard.go:36-56`).
19. **`docs/permissions.md` "Admin token ... passes every admin check"** is correct. The same file's statement that an admin-role API key "is not admin for workspaces" also matches (`wsaccess.go:155-158`). No discrepancy; noted for completeness.
20. **Unmatched methods.** The docs do not mention 405. The code never answers 405: a wrong method on a known API path goes through authentication and the binding guard and ends in `404 Not Found` (or `403 token_bound`). This was verified against echo v4.16 routing (`Group.Use` registers catch-all not-found routes).


---

## 4. Workspaces

### 4.1 Purpose and scope

A **workspace** is the hub's record of one git repository: a globally unique slug, the repository URL (`git_url`, the workspace's `origin`), an optional branch, an owner and an organization, and a full non-bare clone that the hub keeps on disk (the *trunk*). Agents clone and push through the hub's git server (section on the git server), the hub syncs the trunk with `origin`, pushes branches of the trunk back to `origin`, and retires the workspace through archive / reactivate / delete. A workspace is either `standard` or `carry_patch` (a fork that also tracks an `upstream` and rebuilds an integration branch from upstream plus a patch list).

In scope here: the workspace entity, its settings, the ten workspace REST routes, the clone queue, the standard sync and the sync extension hook point, archive / reactivate / reclone / delete, the branch push to `origin`, the per-workspace lock and rebuild-active flag, the on-disk layout, the `parley workspace` commands (except `patch-status`) and the `hub.workspace.*` / `hub.branch.push` events. Out of scope (described by other sections, only the hand-off points are given here): the carry-patch sync, rebuild, patches, rerere and `patch-status`; the git smart-HTTP server; secrets/variables; workspace-bound tokens; orgs and the personal-org hook; the audit store.

### 4.2 User stories

**US-WS-1 — Register a repository (Developer).** As a Developer, I want to register a repository under a slug so that the hub clones it and agents can work on it through the hub.
- `POST /api/v1/workspaces` with `slug` and `git_url` answers `201` with `status: "pending"`, `access: "owner"`, `owner_id` = caller's user id; the clone runs in the background (`pending` → `cloning` → `ready` | `failed`).
- Without `org_id` the workspace is placed in the caller's personal org; with `org_id` (id or slug) the caller must be a member.
- `parley workspace create --wait` returns once the clone is `ready` (exit 0) or `failed` (exit 1).

**US-WS-2 — Private repository (Developer).** As a Developer, I want to give a PAT or username/password at creation so that the hub can clone, sync and push a private repository.
- Credentials are verified with an `ls-remote` against `git_url` before anything is stored; a refusal is `400 credential validation failed for <git_url>: unable to authenticate` and no row is created.
- They are stored as workspace secrets `GIT_PAT`, or `GIT_USERNAME` + `GIT_PASSWORD`, and never echoed in any answer, event or log.

**US-WS-3 — See my workspaces (Developer, Org member, Agent).** As a user I want to list and read the workspaces I may see.
- The list holds the caller's own workspaces (`access: "owner"`) and those of the caller's orgs (`access: "member"`); an admin token sees all (`access: "admin"`); a bound PAT sees at most its own workspace.
- Archived workspaces are hidden unless `include_archived=true`.

**US-WS-4 — Change a workspace (Developer).** As an owner, I want to change display name, description, org and settings without touching the immutable fields.
- Only fields present in the PATCH body change; `null` resets a field to its default; settings merge key by key.
- Immutable fields (`slug`, `git_url`, `branch`, `owner_id`, `workspace_mode`, `upstream_url`, `integration_branch`) and the retired `sync_mode` are refused with `400`.

**US-WS-5 — Keep the trunk current (Developer, Agent).** As an owner I want `POST .../sync` to fast-forward the trunk to `origin`.
- Up to date / fast-forward answer `200` with refreshed sync fields; a diverged origin answers `409` and records `sync_error`; `?reset_to_upstream=true` force-moves the branch to origin's tip.
- A carry_patch workspace is synced by the carry-patch module (hand-off) and the response carries its extra fields; `parley workspace sync --fail-on-diverged` exits 3 when `patches_diverged` is non-empty.

**US-WS-6 — Publish a branch (Agent).** As an Agent with a workspace-bound PAT I want to push a branch I pushed to the hub on to `origin`, so that I can open a pull request.
- `POST .../branches/:name/push` pushes exactly `refs/heads/<name>` with the workspace's stored credential, fast-forward only unless `force`.
- The workspace's own branch can never be force-pushed; on carry_patch workspaces the integration branch and registered patch branches are refused.
- Each push attempt produces exactly one `hub.branch.push` event, also when the caller disconnects.

**US-WS-7 — Retire and revive (Developer, Operator).** As an owner I want to archive a workspace (keeping its record), reactivate it later, and finally delete it.
- Archive pushes all local branches to `origin` before removing the clone; if that push fails the clone is kept.
- Archive and delete end the workspace's active agent sessions; delete revokes all tokens bound to the workspace and removes its patches, workspace secrets and variables.
- Delete is only possible on an archived workspace.

**US-WS-8 — Recover a broken clone (Developer).** As an owner I want `reclone` to throw the trunk away and clone again.
- Refused while a clone is pending/cloning, while another trunk operation holds the lock, and for the whole duration of a rebuild.

### 4.3 Concepts and data model

#### 4.3.1 Workspace (table `workspaces`, SQLite)

| Column / JSON field | Type | Default | Meaning / constraints |
|---|---|---|---|
| `slug` | string, PK | — | 3–64 chars; regex `^[a-z][a-z0-9-]{1,62}[a-z0-9]$`; no `--`. Globally unique; immutable. |
| `git_url` | string, NOT NULL | — | `https://<host>/<path>` (regex `^https://([^/]+)/(.+)$`) or `git@<host>:<path>` (regex `^git@([^:]+):(.+)$`). `http://`, `ssh://`, `file://` etc. are refused. Immutable. Userinfo in an https URL is not rejected. |
| `branch` | string or null | null | The workspace's own branch; null = the remote's default branch. Validated as a git branch name (4.4). Immutable. |
| `owner_id` | string, NOT NULL | caller's user id | Immutable. |
| `org_id` | string or null | caller's personal org | Canonical org id (a slug given in requests is resolved to the id). |
| `status` | enum, NOT NULL | `pending` | `pending`, `cloning`, `ready`, `failed`, `archived` (DB CHECK constraint). |
| `display_name` | string, NOT NULL | slug | ≤ 128 **bytes**; empty/null stored as the slug. |
| `description` | string, NOT NULL | `""` | ≤ 1024 bytes. |
| `head_sha` | string or null | null | 40-hex SHA. Written by: clone success (clone HEAD), archive of a ready workspace (trunk HEAD), sync fast-forward/reset (origin tip), and a push to the hub's git server (trunk HEAD after the push; that write does not touch `updated_at`). Set to NULL whenever a clone job starts (`cloning`). **[FIX-04]** A clone job that reuses an existing clone sets it to that clone's `HEAD` (WS-FR-047); in the baseline it stayed NULL. |
| `clone_error` | string or null | null | Error text of the last failed clone. Cleared by every clone-job status write, by archive and by reactivate. |
| `created_at` / `updated_at` | string, NOT NULL | now | UTC, fixed-width `YYYY-MM-DDTHH:MM:SS.ffffffZ` (microseconds, always 6 digits). |
| `upstream_head_sha` | string or null | null | Standard: origin's tip of the workspace branch at the last fetch (also written on divergence). Carry_patch: upstream HEAD, written by the carry-patch sync. Cleared by reclone. |
| `last_sync_at` | string or null | null | Time of the last successful sync. |
| `sync_error` | string or null | null | Error text of the last failed standard sync; cleared by a successful sync and by reclone. |
| `workspace_mode` | enum, NOT NULL | `standard` | `standard` or `carry_patch`; a NULL/empty stored value reads as `standard`. Immutable. |
| `upstream_url` | string or null | null | carry_patch only (required there, refused for standard); same URL rules as `git_url`. Immutable. |
| `integration_branch` | string or null | null / `deploy` | carry_patch only; defaults to `deploy`; validated as a branch name. Immutable. |
| `settings` | JSON text, NOT NULL | `'{}'` | The settings document (4.3.3). |

The same schema init also creates the `patches` table (used by the carry-patch module; columns `id, workspace_slug, branch_name, position, status (default 'active'), conflict_files, upstream_pr_url, description, deleted_at, added_at, updated_at, origin_sync_state, origin_sha, origin_synced_at`, UNIQUE(workspace_slug, branch_name), UNIQUE(workspace_slug, position)) and the token-workspace-bindings table.

**Schema migration at start (idempotent):** missing columns `upstream_head_sha`, `last_sync_at`, `sync_error`, `workspace_mode` (NOT NULL DEFAULT 'standard'), `upstream_url`, `integration_branch`, `settings` (NOT NULL DEFAULT '{}') are added; patches columns `conflict_files`, `deleted_at`, `origin_sync_state`, `origin_sha`, `origin_synced_at` are added. A legacy table that still has a `clone_status` column is rebuilt in one transaction: `status` := `'archived'` if old `status = 'archived'` else `clone_status`; if `sync_mode` existed, `sync_mode = 'disabled'` sets `settings.sync_enabled = false`; if `sync_status` existed, `sync_status = 'syncing'` becomes `sync_error = 'sync interrupted by server restart'`; the columns `clone_status`, `sync_status`, `sync_mode` disappear.

#### 4.3.2 Workspace response object

Every route that returns a workspace returns a JSON object with **all** of these keys (nullable ones as `null`, never omitted):

| Field | Type | Source |
|---|---|---|
| `slug`, `git_url`, `branch`, `owner_id`, `org_id`, `status`, `display_name`, `description`, `head_sha`, `clone_error`, `created_at`, `updated_at`, `upstream_head_sha`, `last_sync_at`, `sync_error`, `upstream_url`, `integration_branch` | as stored | 4.3.1 |
| `workspace_mode` | string | stored value; empty → `"standard"` |
| `org_slug` | string or null | `orgs.slug` of `org_id`; null when `org_id` is null or names no org (or the org row cannot be read) |
| `hub_url` | string or null | `<server.external_url> + "/git/" + org_slug + "/" + slug + ".git"`, concatenated verbatim (a trailing `/` in `external_url` is not trimmed). Null when `server.external_url` is empty or `org_slug` is null. |
| `access` | string | The caller's level: `"admin"` (admin token), `"owner"`, `"member"` (member of the workspace's org). Create always answers `"owner"`. Describes the caller, not a PAT's scopes. |
| `settings` | object | Every setting key with its effective value (4.3.3), in the order listed there. |

Example:

```json
{
  "slug": "my-project", "git_url": "https://github.com/acme/repo.git",
  "hub_url": "https://hub.example.com/git/acme/my-project.git",
  "branch": null, "owner_id": "u-1", "org_id": "o-1", "org_slug": "acme", "access": "owner",
  "status": "ready", "display_name": "my-project", "description": "",
  "head_sha": "4b825dc6...", "clone_error": null,
  "created_at": "2026-11-10T09:00:00.000000Z", "updated_at": "2026-11-10T09:00:03.123456Z",
  "upstream_head_sha": null, "last_sync_at": null, "sync_error": null,
  "workspace_mode": "standard", "upstream_url": null, "integration_branch": null,
  "settings": {"patch_branch_source": "hub", "patch_divergence_policy": "replace",
    "push_patches_to_origin": false, "auto_rebuild_after_push": true, "auto_rebuild_after_sync": true,
    "rebuild_strategy": "rebase", "rebuild_fail_mode": "fail_fast", "rebuild_push_integration_branch": false,
    "squash_merge_detection": "both", "forge_api_url": "", "upstream_forge_api_url": "", "sync_enabled": true}
}
```

#### 4.3.3 Settings

Stored as one JSON document in `workspaces.settings`. Reading: start from the defaults and overlay the keys the document has; an unreadable document reads as all defaults. Writing always stores the full document (all 12 keys).

| Key | Type | Values | Default | Effect (consumer) |
|---|---|---|---|---|
| `patch_branch_source` | enum | `hub`, `origin` | `hub` | carry-patch: who is authoritative for patch branches |
| `patch_divergence_policy` | enum | `replace`, `report` | `replace` | carry-patch, origin mode |
| `push_patches_to_origin` | bool | | `false` | carry-patch push control |
| `auto_rebuild_after_push` | bool | | `true` | carry-patch post-push hook |
| `auto_rebuild_after_sync` | bool | | `true` | carry-patch sync |
| `rebuild_strategy` | enum | `rebase`, `merge` | `rebase` | rebuild |
| `rebuild_fail_mode` | enum | `fail_fast`, `continue` | `fail_fast` | rebuild |
| `rebuild_push_integration_branch` | bool | | `false` | rebuild |
| `squash_merge_detection` | enum | `ancestry_only`, `content_based`, `both` | `both` | carry-patch sync |
| `forge_api_url` | url-string | `""` or absolute `https://` URL with host, no userinfo/query/fragment | `""` | forge client (origin) |
| `upstream_forge_api_url` | url-string | same | `""` | forge client (upstream); no fallback to `forge_api_url` |
| `sync_enabled` | bool | | `true` | `POST .../sync` (false → every sync refused 400) |

**Applying a `settings` value (create and PATCH):**
- `settings: null` → every key returns to its default.
- Otherwise the value must be a JSON object (`[]`, a string, etc. → `400 settings must be an object`).
- For each given key: unknown key → `400 unknown setting "<key>"`; value `null` → that key returns to its default; bool keys need a JSON boolean (`400 setting <key> must be true or false`); enum keys need one of the listed strings (`400 setting <key> must be one of "a", "b"[, "c"]`); url keys need a JSON string (`400 setting <key> must be a string`) that is `""` or valid after trimming surrounding whitespace (`400 setting <key> must be an absolute https:// URL with a host and without userinfo, query or fragment, or empty`). The untrimmed string is what is stored.
- Keys not given keep the base value (the current settings on PATCH, the defaults on create).
- The request is all-or-nothing. When several keys are invalid, which error is reported is not deterministic.

**Legacy variable names.** Settings used to be workspace variables. Internal consumers (carry-patch module, forge client) still read them by the old upper-case names; a name maps to a setting as follows, and a missing workspace, a name that is not in the table, or an empty string value reads as "not set":

| Legacy variable | Setting |
|---|---|
| `PATCH_BRANCH_SOURCE` | `patch_branch_source` |
| `PATCH_DIVERGENCE_POLICY` | `patch_divergence_policy` |
| `PUSH_PATCHES_TO_ORIGIN` | `push_patches_to_origin` |
| `AUTO_REBUILD_AFTER_PUSH` | `auto_rebuild_after_push` |
| `AUTO_REBUILD_AFTER_SYNC` | `auto_rebuild_after_sync` |
| `REBUILD_STRATEGY` | `rebuild_strategy` |
| `REBUILD_FAIL_MODE` | `rebuild_fail_mode` |
| `REBUILD_PUSH_INTEGRATION_BRANCH` | `rebuild_push_integration_branch` |
| `SQUASH_MERGE_DETECTION` | `squash_merge_detection` |
| `FORGE_API_URL` | `forge_api_url` |
| `UPSTREAM_FORGE_API_URL` | `upstream_forge_api_url` |

(`sync_enabled` has no legacy variable; it came from the old `sync_mode` column, see 4.3.1.) Booleans read as `"true"` / `"false"`.

**One-time migration at every start** (after the variables table exists): every workspace-scope variable whose key matches a legacy name case-insensitively is moved into that workspace's settings and the variable row is deleted. Value conversion: for a bool setting, `true` iff the trimmed value equals `true` case-insensitively (anything else is `false`); for other settings, the trimmed string. A value the setting cannot take (e.g. `REBUILD_STRATEGY=squash`) or an undecodable variable value is dropped with a warning; the setting keeps its previous value. Variables of a workspace that no longer exists are deleted without a change. Other variables are untouched. A second run finds nothing. A failure aborts start (`migrate workspace settings: ...`). Variables named like settings created later have no effect on behaviour.

#### 4.3.4 Status state machine

| From | Event | To | Notes |
|---|---|---|---|
| — | `POST /workspaces` | `pending` | clone job enqueued |
| `pending` | clone worker picks the job | `cloning` | `head_sha`, `clone_error` set NULL |
| `cloning` | clone succeeded | `ready` | `head_sha` = clone HEAD |
| `cloning` | `reuse` job and `<root>/<slug>/trunk` is a valid clone of `git_url` (WS-FR-047) | `ready` | no clone; `head_sha` = the clone's `HEAD` **[FIX-04]** |
| `cloning` | credential lookup failed / mkdir failed / clone failed | `failed` | `clone_error` set; partial directory removed only on clone failure |
| `ready` | archive | `archived` | lock + rebuild check; push all branches; `head_sha` = trunk HEAD |
| `pending`, `failed` | archive | `archived` | no lock, no push; directory removed; `head_sha` NULL |
| `cloning` | archive | (refused 409) | |
| `archived` | reactivate | `pending` | clone job enqueued; kept directory is reused |
| `ready`, `failed` | reclone | `pending` | lock + rebuild check; push; directory removed; clone job enqueued |
| `pending`, `cloning` | reclone | (refused 409) | |
| `archived` | delete | (row removed) | |
| any non-`archived` | delete | (refused 409) | |
| `archived` (archived while `pending`) | the still-queued clone job runs | `cloning` → `ready`/`failed` | the clone job does not check the current status (race, see 4.10) |

A sync never changes `status`. A "sync in progress" is not stored; it is only the held lock.

#### 4.3.5 Git credentials of a workspace

Credentials are workspace secrets (secrets store, owner_type `workspace`, owner_id = slug), resolved on **every** use (clone, sync, archive/reclone push, branch push, merge fetch, forge client). Create stores them; afterwards they can be changed only through the secrets API (the names are conventional, not enforced as reserved by the workspace core).

Origin credential resolution:
1. `GIT_PAT` present → HTTP Basic auth, username `x-token-auth`, password = PAT.
2. else `GIT_USERNAME` present: with `GIT_PASSWORD` → Basic auth (username, password); without `GIT_PASSWORD` → no credential (warning logged).
3. else → no credential (anonymous).
A secrets-store failure other than "not found" is an error (the operation fails; see each operation).

Upstream credential (carry_patch): `UPSTREAM_GIT_PAT` (Basic `x-token-auth`) → `UPSTREAM_GIT_USERNAME` + `UPSTREAM_GIT_PASSWORD` → otherwise (none, or username without password) fall back to the origin resolution above. Store failures propagate.

#### 4.3.6 On-disk layout

```
<workspace.path>/                 (created with parents at start, mode 0755)
  <slug>/                         (presence of this directory = "already cloned" for the clone job)
    trunk/                        non-bare clone with working tree; remote "origin" = git_url
                                  (carry_patch: also remote "upstream", rerere enabled, integration branch)
    rebuild/<rebuild job id>/     carry-patch rebuild worktrees (removed by the rebuild; stale ones
                                  removed at start before the job queue runs)
```

#### 4.3.7 Per-workspace lock and rebuild-active flag (in-memory, per process)

- **Lock**: one mutex per slug. `TryLock` (non-blocking) is used by request handlers, which answer `409` with `error_type: workspace_busy` and message `another operation is running on this workspace; retry later`. Blocking `Lock` is used by background jobs (merge job for its whole run; rebuild job for its first (fetch) and final phases only).
- **Rebuild-active flag**: per slug, set by a rebuild (while it holds the lock, at the start of phase one) and cleared when its worktree is removed; independent of the mutex. A second set while one is active fails. Cleared by process exit.

| Operation | Takes the lock | Reads rebuild-active flag |
|---|---|---|
| Standard sync | TryLock → 409 | no |
| Carry-patch sync (extension) | TryLock (its own) → 409 | no |
| Archive of a `ready` workspace | TryLock → 409 | yes → 409 `workspace_busy` |
| Archive of `pending`/`failed` | no | no |
| Reclone | TryLock → 409 | yes → 409 `workspace_busy` |
| Branch push | TryLock → 409 (after the DB-only refusals) | no |
| Reactivate, delete, PATCH, create, get, list | no | no |
| Merge job / batch rebase / rebuild rollback / rerere forget / patch reset-to-origin / patch branch resolution | Lock / TryLock (§6, §7) | no |
| Worktree reset after a push to the git server | TryLock; skipped when held (the holder restores) | no |

### 4.4 Functional requirements

**Authentication and authorization (common)**
- WS-FR-001 Every route needs an authenticated credential; none → `401 authentication required`.
- WS-FR-002 Scope checks apply to PATs only (admin tokens and API keys are not scope-checked). `workspaces:write` implies `workspaces:read`. Legacy PAT scopes `workspaces:create`, `workspaces:delete`, `workspaces:sync` are rewritten to `workspaces:write` at start.
- WS-FR-003 Per-workspace access is decided in this order: unknown slug → `404 workspace not found`; caller is neither admin token, owner, nor member of the workspace's org (membership read from `org_members` on every call) → `404 workspace not found`; PAT bound to another workspace → `404 workspace not found` (refusal reported to the bound-token audit); a member on a write route → `403 org members have read-only access to this workspace` with `error_type: forbidden`. A DB failure is `500 internal server error`. If the row disappears between the access decision and the full load → `404 workspace not found`.
- WS-FR-004 Bound PATs (workspace-bound tokens): on any `/workspaces/:slug/...` route naming another slug → `404 workspace not found` before the handler (so before the scope check); on their own slug → normal handler checks; on non-workspace-scoped routes only `GET /workspaces` is allowed (handler confines the list); every other one (e.g. `POST /workspaces`) → `403` `error_type: token_bound`, message `this token is bound to workspace <slug>`.
- WS-FR-005 Admin tokens have `access: "admin"` on every workspace but cannot create one (no user id).

**Create**
- WS-FR-010 Validation order is exactly as in 4.5 `POST /workspaces`; the first failing check answers.
- WS-FR-011 Credential fields: a field is "given" when present with a non-null value; `""` is an error; `git_pat` excludes `git_username`/`git_password`; username and password come together; any credential needs `git_url` starting with `https://` (checked before slug/URL validation).
- WS-FR-012 Credentials are verified with an anonymous-storage `ls-remote` of `git_url` with a 10-second deadline (PAT as Basic `x-token-auth:<pat>`); any failure (bad credential, network, timeout) → `400 credential validation failed for <git_url>: unable to authenticate`; the raw error is logged only.
- WS-FR-013 `display_name` null/empty → slug; length limits are byte counts.
- WS-FR-014 `workspace_mode` null or `""` → `standard`. For `carry_patch`: `upstream_url` required non-empty and valid; `integration_branch` null/`""` → `deploy`, validated. For `standard`: `upstream_url` / `integration_branch` given with a non-null value → 400 (JSON `null` is accepted).
- WS-FR-015 `org_id` non-empty: resolved first as an org id, then as an org slug; not found → `400 organization not found`; caller not in `org_members` → `403 user is not a member of the specified organization`; DB failure → `500 organization membership check failed`. `org_id` absent/null/`""`: the caller's personal org (`orgs.owner_id = caller`; first row if several, warning logged); none → `400 user has no personal organization; contact an administrator`; DB failure → `500 internal server error`.
- WS-FR-016 Slug uniqueness is checked after all validation and the credential check (`409 workspace with slug '<slug>' already exists`); a concurrent duplicate insert (SQLite UNIQUE/PK violation) gives the same 409.
- WS-FR-017 After the row is inserted, credentials are stored as secrets `GIT_PAT` or `GIT_USERNAME`+`GIT_PASSWORD`. If storing fails, the row is deleted again (compensating delete of the row only) and the answer is `500 failed to store credentials`; if the compensating delete also fails it is logged (`CRITICAL`, slug only) and the answer is the same 500.
- WS-FR-018 Then a clone job `{slug, git_url, branch}` is enqueued (may block while the queue buffer is full), `hub.workspace.create` is emitted, and `201` returns the workspace with `access: "owner"`.
- WS-FR-019 Unknown JSON fields in the create body are ignored.

**Validation rules**
- WS-FR-020 Slug errors (prefix `invalid slug: `), checked in order: `slug must be at least 3 characters, got N`; `slug must be at most 64 characters, got N`; `slug must start with a lowercase letter`; `slug must not end with a hyphen` (also for any non-`[a-z0-9]` last char); `slug must not contain consecutive hyphens`; `slug must contain only lowercase letters, digits, and hyphens`.
- WS-FR-021 git_url errors (prefix `invalid git_url: `, or `invalid upstream_url: `): `git_url must not be empty`; `git_url must use HTTPS or SSH, not plain HTTP` (prefix `http://`); `git_url HTTPS format must be https://<host>/<path> with non-empty host and path`; `git_url SSH format must be git@<host>:<path> with non-empty host and path`; `git_url must use HTTPS (https://) or SSH (git@) format`. Pattern only, no DNS.
- WS-FR-022 Branch errors (prefix `invalid branch: ` / `invalid integration_branch: `), in order: empty; ASCII control char (< 0x20 or 0x7f); space, `~`, `^`, `:`, `?`, `*`, `[`, `\`; `..`; ends with `.lock`; ends with `.`; any `/`-component starting with `.`; then git ref-format rules: must not start with `-` (`branch must not start with '-'`), must not be `@`, must not contain `@{`, must not start/end with `/` or contain `//`, no component ending in `.lock`.

**Update (PATCH)**
- WS-FR-030 The body must decode as a JSON object (`400 invalid request body: <err>`; empty body → `invalid request body: EOF`; `[1]` is also an "invalid request body").
- WS-FR-031 Immutable keys are checked in this order and refused if present (any value, including null): `slug`, `git_url`, `branch`, `owner_id`, `workspace_mode`, `upstream_url`, `integration_branch` → `400 <field> is immutable and cannot be updated`; then `sync_mode` → `400 sync_mode is no longer a field; set the sync_enabled setting instead`.
- WS-FR-032 Mutable keys: `display_name`, `description`, `org_id` (string or null; another JSON type → `400 invalid <field> value`) and `settings`. No mutable key present → `400 request body must contain at least one updatable field`. Other unknown keys are ignored.
- WS-FR-033 Body checks WS-FR-030..032 run **before** the workspace lookup (so an unknown slug with a bad body gets 400, not 404).
- WS-FR-034 After access (write) is granted: archived → `400 workspace is archived and must be reactivated before updating`; then `display_name` (null/`""` → slug; > 128 bytes → 400), `description` (null → `""`; > 1024 → 400), `org_id` (non-empty → membership check of the **caller** as in WS-FR-015, resolved to the canonical id; null or `""` → org removed), `settings` (merge per 4.3.3).
- WS-FR-035 All mutable columns plus `updated_at` are written in one UPDATE (even if values are unchanged); `hub.workspace.update` is emitted with `fields` = the given mutable keys in the order `display_name`, `description`, `org_id`, `settings`.

**List / get**
- WS-FR-040 List: a PAT without `workspaces:read`/`write` → `404 workspace not found`. Admin token → all workspaces ordered by `created_at` DESC. Others → the readable set (owned workspaces of any status as `owner`, workspaces whose non-empty `org_id` is one of the caller's orgs as `member`; ownership wins), restricted to the bound workspace for a bound PAT; ordered by `created_at` DESC then `slug` ASC. A credential without user id that is not an admin token → `[]`. Archived workspaces are excluded unless the query parameter `include_archived` is exactly `true`. Empty result is `[]`.
- WS-FR-041 Get: a PAT without read scope → 404; read access (member suffices).

**Clone job**
- WS-FR-045 The clone job: (1) set `status=cloning`, `head_sha=NULL`, `clone_error=NULL` (if this write fails the job stops, logged); (2) **[FIX-04]** if `<root>/<slug>` exists: a `reuse` job whose directory is a valid clone (WS-FR-047) sets `status=ready` and `head_sha` = the clone's `HEAD`, and stops there. Otherwise (a `fresh` job, or an invalid or foreign directory) it removes `<root>/<slug>`; a failure sets `failed` with `clone_error = "remove stale directory: <err>"`. Then it continues with step 3; (3) resolve the origin credential — failure → `failed`, `clone_error = "credential lookup failed: <err>"`; (4) create `<root>/<slug>/trunk` — failure → `failed`, `clone_error` = OS error; (5) clone `git_url` into `trunk`: full history (no depth limit); with `branch` set, a single-branch clone of `refs/heads/<branch>`, else all branches with the remote default checked out; (6) on clone error remove `<root>/<slug>` and set `failed` with `clone_error = 'clone "<git_url>": <git error>'` (or `read HEAD after clone: ...`); (7) for a carry_patch workspace run the post-clone setup hand-off (4.7.2), whose failures are only logged; (8) set `status=ready`, `head_sha` = clone HEAD.
- WS-FR-046 The job carries the `git_url`/`branch` captured at enqueue time and does not check the workspace's current status (a job for a deleted or archived workspace still runs; status writes to a missing row affect nothing).
- WS-FR-047 **[FIX-04]** Each clone job has a mode.
  - **Modes.** A job is `reuse` when it is enqueued by reactivate, or by start-up recovery of a `pending` workspace. A job is `fresh` when it is enqueued by create, by reclone, or by start-up recovery of a `cloning` workspace.
  - **Valid clone.** An existing directory counts as a valid clone when all of these hold:
    - `<root>/<slug>/trunk` opens as a git repository;
    - its `HEAD` resolves to a commit;
    - its `origin` remote URL equals the workspace's `git_url`, compared with userinfo removed.
  - **Reuse.** A `reuse` job with a valid clone keeps it. It does not run the carry-patch post-clone setup again (the kept clone already has it) and records `head_sha` = the clone's `HEAD`.
  - **Fresh clone.** Every other existing directory is removed before a fresh clone, so a leftover directory (for example from a deleted workspace with the same slug) is never adopted.
  - **Baseline.** Any existing directory was adopted as `ready` with `head_sha = NULL`, so a later standard sync always answered `up_to_date`.
- WS-FR-048 **[FIX-05]** Start-up recovery runs after the clone queue starts and before the HTTP listener serves:
  - every workspace in `pending` is enqueued as a `reuse` job;
  - every workspace in `cloning` is set to `pending` and enqueued as a `fresh` job, because its clone was interrupted.
  - Jobs are enqueued in `created_at` order. A failure to list or update workspaces is logged and does not stop the start.
  - In the baseline, nothing re-enqueued at start, and such workspaces stayed stuck.

**Sync (standard)**
- WS-FR-050 Order of checks in 4.5. Preconditions: `status == ready` and `settings.sync_enabled == true`; both are checked before the lock and before the carry-patch hand-off.
- WS-FR-051 Carry_patch workspaces with a registered sync extension are handed to the extension (4.7.3) before the standard lock is taken and before `reset_to_upstream` is looked at.
- WS-FR-052 The standard sync holds the workspace lock (TryLock) for its whole duration.
- WS-FR-053 Fetch: open `<root>/<slug>/trunk`, fetch remote `origin` with its configured refspecs and the origin credential; "already up to date" is not an error. A fetch error whose text contains `shallow`, `missing object` or `object not found` is retried once as a full-history fetch.
- WS-FR-054 Origin tip: `refs/remotes/origin/<branch>` where `<branch>` is the `branch` column, or the short name of the branch the trunk's HEAD points to, or `main` if HEAD is unreadable. Not resolvable → fetch failure.
- WS-FR-055 Comparison is against the stored `head_sha` (not the local ref): stored head empty or equal to origin tip → `up_to_date`; stored head is an ancestor of origin tip → `fast_forward`; otherwise (including local ahead of origin, unknown commits, or an ancestry-check error) → `diverged`.
- WS-FR-056 `up_to_date`: record success (`sync_error=NULL`, `last_sync_at=now`, `upstream_head_sha=tip`, `head_sha` unchanged); no ref is moved.
- WS-FR-057 `fast_forward`: set `refs/heads/<branch>` (or the branch HEAD points to) to the tip, then best-effort hard reset of the trunk's working tree to the tip (a reset failure is logged only); record success with `head_sha = upstream_head_sha = tip`.
- WS-FR-058 `diverged`: no ref is moved; `upstream_head_sha` := origin tip and `sync_error` := `upstream history has diverged; use --reset-to-upstream to recover` (`head_sha`, `last_sync_at` unchanged); answer 409 with that message (no `error_type`).
- WS-FR-059 `?reset_to_upstream=true` (exactly `true`): fetch as above (outcome ignored), then set the branch ref to origin tip and hard-reset as in WS-FR-057 regardless of ancestry, record success with `head_sha = upstream_head_sha = tip`.
- WS-FR-060 Every failure path writes `sync_error` (and `updated_at`), never `head_sha` or `last_sync_at`. A deferred guard writes `sync_error = "sync interrupted unexpectedly"` (or `sync interrupted: <context error>` if the request context ended) when a path ends without recording an outcome (e.g. a panic).
- WS-FR-061 Successful standard syncs emit `hub.workspace.sync` with `result` ∈ {`up_to_date`, `fast_forward`, `reset_to_upstream`}; failures emit nothing.

**Archive / reactivate / reclone / delete**
- WS-FR-070 Archive of `ready`: TryLock (409 busy), rebuild-active (409 busy), read trunk HEAD (failure → `500` with the raw git error text as message; nothing changes), resolve origin credential and push `refs/heads/*:refs/heads/*` (non-force, no deadline) to `git_url`; if credential resolution fails or the push fails (other than "already up to date"), keep `<root>/<slug>`; otherwise remove `<root>/<slug>` (removal failure logged only). Then `status=archived`, `head_sha` = HEAD read, `clone_error=NULL`.
- WS-FR-071 Archive of `pending`/`failed`: remove `<root>/<slug>` (errors ignored), `status=archived`, `head_sha=NULL`, `clone_error=NULL`. No lock, no push.
- WS-FR-072 After the DB update, archive force-closes all active agent sessions of the workspace with reason `workspace archived` (failure logged, archive still succeeds). Bound tokens, patches, secrets and variables are untouched. Sync fields are untouched.
- WS-FR-073 Reactivate: only `archived` (else `409 workspace is not archived`); sets `status=pending`, `clone_error=NULL` (keeps `head_sha` until the job starts), enqueues a clone job. A DB failure → `500 failed to reactivate workspace` and no job.
- WS-FR-074 Reclone: refuses `archived` (400) and `pending`/`cloning` (409) before the lock; then TryLock and rebuild-active (409 busy); reads HEAD (failure logged only); pushes all branches like the archive with the resolved origin credential — a credential-resolution failure is logged and the push is attempted anonymously; a push failure is logged and the reclone continues; removes `<root>/<slug>` (failure → `500 failed to delete workspace directory: <err>`, nothing else changes); then one UPDATE sets `status=pending`, `sync_error=NULL`, `upstream_head_sha=NULL` (head_sha/clone_error/last_sync_at kept until the job starts); enqueues a clone job. Sessions, patches, tokens are untouched. No event is emitted.
- WS-FR-075 Delete: only `archived` (else `409 workspace must be archived before deletion`). **[FIX-03]** First, one transaction: revoke every bound token whose PAT row is live (`revoked_at` = now), delete all bindings of the slug, delete the workspace row, delete its `patches`, workspace-scope `secrets` and workspace-scope `variables`. Any failure rolls the whole transaction back and answers `500 failed to delete workspace`, and **nothing** has changed: no session was closed and the directory is untouched. Only after the commit: force-close active sessions with reason `workspace deleted` (failure logged), then remove `<root>/<slug>` if present (failure logged; a leftover directory is never adopted by a later workspace with the same slug, WS-FR-047). In the baseline, sessions were closed and the directory was removed before the transaction, so a 500 left a partly deleted workspace. Audit data (DuckDB) is kept. `hub.workspace.delete` carries the sorted revoked token ids. The slug becomes free.

**Branch push**
- WS-FR-080 Order of checks as in 4.5. DB-only refusals (carry_patch integration branch, registered patch branch, forced push of the `branch` column) precede the lock so they answer the same whether busy or not.
- WS-FR-081 The branch name is the raw (escaped) path segment between `/branches/` and the final `/push`, percent-decoded exactly once (`a%2525b` → `a%25b`; `fix%2F42` → `fix/42`). A decode error or a name failing git ref-name rules → `404 branch_not_found`.
- WS-FR-082 Under the lock: open the trunk (failure → 500), determine the own branch (`branch` column, or the branch HEAD points to; detached → none); with `force`, an unreadable HEAD → 500 and own branch == name → `400 protected_branch`; `refs/heads/<name>` missing → `404 branch_not_found`; resolve the origin credential (store failure → 500, nothing sent).
- WS-FR-083 Push exactly `refs/heads/<name>:refs/heads/<name>` (prefixed `+` and push-force when `force`) to remote `origin` at `git_url`. Force is an unconditional force (no lease / expected-old-value check). Deadline 2 minutes; the push and the follow-up ref listing run to completion even if the client disconnects (the handler, and the lock, wait).
- WS-FR-084 After the push, if the trunk's HEAD moved during the operation (a git-server push skipped its worktree reset because of the lock), the worktree is hard-reset to the new HEAD (failure logged once, secret scrubbed).
- WS-FR-085 Every message of this route that has a route-specific `error_type` starts with `<error_type>: `. All texts are scrubbed: the credential secret replaced by `[redacted]`, URL userinfo removed, cut to 1024 bytes.
- WS-FR-086 One `hub.branch.push` event per push attempt handed to the transport (success or failure, recorded on a context that survives the caller's disconnect); none for refusals or 500s.

**Lock**
- WS-FR-090 The lock and the rebuild flag are process-local; the hub runs as a single replica. A refusal releases nothing it did not take; locks are always released at handler exit.

### 4.5 REST API

All paths are under the API mount point (default `/api/v1`). Errors use the envelope `{"error":{"code":<status>,"message":"<text>"[,"error_type":"<type>"]}}` (`error_type` omitted when none). Common answers on every workspace-scoped route: `401 authentication required`; `404 workspace not found` (unknown slug, no access, PAT bound elsewhere); `403 org members have read-only access to this workspace` + `error_type: forbidden` (member on a write); `500 internal server error`.

#### `POST /workspaces`
- **Purpose:** create a workspace and enqueue its clone.
- **Auth / scope:** any non-admin credential with a user id; PAT needs `workspaces:write`. Bound PATs → `403 token_bound` (route not allowlisted).
- **Path/query params:** none.
- **Request body:**

| Field | Type | Req. | Default | Validation |
|---|---|---|---|---|
| `slug` | string | yes | — | WS-FR-020 |
| `git_url` | string | yes | — | WS-FR-021 |
| `branch` | string/null | no | null | WS-FR-022 when non-null |
| `org_id` | string/null | no | personal org | org id or slug (WS-FR-015) |
| `display_name` | string/null | no | slug | ≤ 128 bytes |
| `description` | string/null | no | `""` | ≤ 1024 bytes |
| `workspace_mode` | string/null | no | `standard` | `standard`, `carry_patch` (`""` = standard) |
| `upstream_url` | string/null | carry_patch | null | required non-empty for carry_patch; refused (non-null) for standard; WS-FR-021 |
| `integration_branch` | string/null | no | `deploy` (carry_patch) | refused (non-null) for standard; WS-FR-022 |
| `settings` | object/null | no | defaults | 4.3.3 |
| `git_pat` | string | no | — | non-empty; excludes username/password; https only |
| `git_username`, `git_password` | string | no | — | non-empty; both or neither; https only |

- **Behaviour (order of checks):**
  1. no credential → 401 `authentication required`
  2. admin token → 403 `admin tokens cannot create workspaces; a real user is required as owner`
  3. PAT without `workspaces:write` → 403 `PAT requires workspaces:write scope to create workspaces`
  4. body decode error → 400 `invalid request body: <decoder error>`
  5. `git_pat` / `git_username` / `git_password` present as `""` → 400 `<field> must not be empty` (in that order)
  6. PAT with username/password → 400 `git_pat and git_username/git_password are mutually exclusive`
  7. username xor password → 400 `git_username and git_password must both be provided together`
  8. any credential with `git_url` not starting `https://` → 400 `git credentials require an HTTPS git_url`
  9. slug → 400 `invalid slug: ...`; git_url → 400 `invalid git_url: ...`; branch → 400 `invalid branch: ...`
  10. display_name → 400 `display_name must not exceed 128 characters`; description → 400 `description must not exceed 1024 characters`
  11. workspace_mode → 400 `invalid workspace_mode value: must be 'standard' or 'carry_patch'`
  12. carry_patch: 400 `upstream_url is required when workspace_mode is 'carry_patch'` / `invalid upstream_url: ...` / `invalid integration_branch: ...`; standard: 400 `upstream_url is not allowed when workspace_mode is 'standard'` / `integration_branch is not allowed when workspace_mode is 'standard'`
  13. settings → 400 (4.3.3 messages)
  14. credentials given → ls-remote (10 s) → 400 `credential validation failed for <git_url>: unable to authenticate`
  15. org (WS-FR-015): 400 `organization not found` / 403 `user is not a member of the specified organization` / 500 `organization membership check failed` / 400 `user has no personal organization; contact an administrator` / 500 `internal server error`
  16. slug exists → 409 `workspace with slug '<slug>' already exists` (lookup failure → 500 `internal server error`)
  17. insert (unique violation → same 409; other → 500 `failed to create workspace`)
  18. store credential secrets (failure → compensating row delete → 500 `failed to store credentials`)
  19. enqueue clone job; emit event; answer.
- **Response:** `201` workspace object, `status: "pending"`, `access: "owner"`, `owner_id` = caller.
- **Errors:** as listed in the order above.
- **Side effects:** row inserted; secrets `GIT_PAT` or `GIT_USERNAME`/`GIT_PASSWORD`; clone job queued; `hub.workspace.create`.

#### `GET /workspaces`
- **Purpose:** list readable workspaces.
- **Auth / scope:** any credential; PAT needs `workspaces:read` or `workspaces:write`, else `404 workspace not found`. Allowed for bound PATs (only their workspace).
- **Path/query params:** `include_archived` — only the exact string `true` includes archived workspaces; default excluded.
- **Request body:** none.
- **Behaviour:** WS-FR-040; each item's `access` is the caller's level (`admin` for admin tokens).
- **Response:** `200` JSON array of workspace objects (possibly `[]`).
- **Errors:** 401; 404 (PAT scope); 500 `internal server error`.
- **Side effects:** none.

#### `GET /workspaces/:slug`
- **Purpose:** read one workspace.
- **Auth / scope:** PAT needs read scope (else 404 before lookup); read access (owner, member, admin).
- **Response:** `200` workspace object.
- **Errors:** 401; 404 (scope, unknown, no access, bound elsewhere); 500.
- **Side effects:** none.

#### `PATCH /workspaces/:slug`
- **Purpose:** change mutable fields and settings.
- **Auth / scope:** PAT needs `workspaces:write`, else `404 workspace not found` (checked first, before the body); write access (owner or admin; member → 403 forbidden).
- **Request body:** JSON object with at least one of:

| Field | Type | `null` means | Validation |
|---|---|---|---|
| `display_name` | string/null | back to slug (`""` too) | ≤ 128 bytes |
| `description` | string/null | back to `""` | ≤ 1024 bytes |
| `org_id` | string/null | remove org (`""` too) | id or slug; caller must be a member |
| `settings` | object/null | reset all settings | 4.3.3 |

- **Behaviour (order):** 401 → PAT scope (404) → body decode (400 `invalid request body: ...`) → immutable keys (400 `<field> is immutable and cannot be updated`) → `sync_mode` (400) → mutable field types (400 `invalid display_name value` / `invalid description value` / `invalid org_id value`) → none present (400 `request body must contain at least one updatable field`) → workspace access (404/403/500) → archived (400 `workspace is archived and must be reactivated before updating`) → display_name length → description length → org membership (400/403/500) → settings (400) → UPDATE (500 `failed to update workspace`) → event → answer.
- **Response:** `200` workspace object as stored after the update, with the caller's `access`.
- **Side effects:** row updated (`updated_at` refreshed); `hub.workspace.update {fields}`.

#### `POST /workspaces/:slug/archive`
- **Purpose:** archive a workspace.
- **Auth / scope:** PAT needs `workspaces:write`, else 404; write access.
- **Request body:** none (ignored).
- **Behaviour (order):** 401 → PAT scope (404) → access → `archived` → 400 `workspace is already archived` → `cloning` → 409 `clone in progress; try again after it completes` → `ready`: WS-FR-070 (409 `workspace_busy` for lock or active rebuild; HEAD read failure → 500 `<git error text>`) / `pending`,`failed`: WS-FR-071 → DB update (500 `failed to archive workspace`) → force-close sessions → event → answer.
- **Response:** `200` workspace object with `status: "archived"`.
- **Side effects:** clone directory removed or kept (4.4); sessions terminated (`hub.session.force_closed` each); `hub.workspace.archive`.

#### `POST /workspaces/:slug/reactivate`
- **Purpose:** bring an archived workspace back.
- **Auth / scope:** PAT needs `workspaces:write`, else 404; write access.
- **Behaviour (order):** 401 → scope (404) → access → not archived → 409 `workspace is not archived` → UPDATE (500 `failed to reactivate workspace`, no job) → enqueue clone job → event → answer.
- **Response:** `200` workspace object, `status: "pending"`, `clone_error: null` (head_sha still the archived value).
- **Side effects:** clone job; `hub.workspace.reactivate`.

#### `POST /workspaces/:slug/sync`
- **Purpose:** sync the trunk with its remote.
- **Auth / scope:** PAT needs `workspaces:write`, else `403 PAT requires workspaces:write scope to sync workspaces` (before lookup); write access.
- **Path/query params:** `reset_to_upstream` — exactly `true` enables reset (standard only; a carry_patch workspace with a registered extension never reads it).
- **Request body:** none.
- **Behaviour (order):**
  1. 401; 403 scope; access (404/403/500)
  2. `status != ready` → 400 `workspace is not ready; sync requires a ready workspace`
  3. `sync_enabled == false` → 400 `sync is disabled for this workspace; set the sync_enabled setting to true to enable`
  4. `carry_patch` and an extension is registered → hand-off (4.7.3): if the extension handled it, re-read the row (500 `failed to read workspace after sync`) and answer `200` with the workspace object plus the extension's fields merged on top (extension keys win); if the extension wrote an error, that is the answer; if it declined, continue with 5.
  5. TryLock → 409 `workspace_busy`
  6. resolve origin credential → failure: 502 `sync failed: credential resolution error` (`sync_error = "credential resolution failed: <err>"`)
  7. `reset_to_upstream=true` → reset path (below)
  8. request context already ended → 504 `sync interrupted: request context cancelled` (`sync_error = "sync interrupted: <ctx err>"`)
  9. fetch functions not initialised → 502 `sync system not initialized` (sync_error same)
  10. fetch + compare; context ended meanwhile → 504 as in 8; fetch error → 502 `sync failed: upstream fetch error` (`sync_error = "upstream fetch failed: <err>"`)
  11. outcome: `up_to_date` → 200; `fast_forward` → ref update (failure: 502 `sync failed: could not fast-forward integration branch`, `sync_error = "ref update failed: <err>"`) → 200; `diverged` → 409 `upstream history has diverged; use --reset-to-upstream to recover` (sync_error same, upstream_head_sha = origin tip); other → 500 `unexpected sync outcome` (`sync_error = "unexpected sync outcome: <x>"`). Success-record failure → 500 `failed to update sync status`; re-read failure → 500 `failed to read workspace after sync`.
  - Reset path: not initialised → 502 `sync system not initialized`; fetch error → 502 `reset-to-upstream failed: upstream fetch error` (`sync_error = "reset-to-upstream fetch failed: <err>"`); ref update error → 502 `reset-to-upstream failed: could not update integration branch` (`sync_error = "reset-to-upstream ref update failed: <err>"`); record failure → 500 `reset-to-upstream failed: could not update workspace state`; re-read failure → 500 `failed to read workspace after reset-to-upstream`; success → 200.
- **Response:** `200` workspace object (re-read after the sync). Carry_patch responses additionally carry `patches_merged`, `rebuild_triggered`, `force_push_detected`, `origin_fetched`, optional `rebuild_job_id`, and in origin mode `patches_synced`, `patches_diverged` (defined by the carry-patch section).
- **Errors:** 400, 403, 404, 409 (`workspace_busy` or diverged without error_type), 500, 502, 504 as above; carry-patch-specific ones per the carry-patch section.
- **Side effects:** sync fields and possibly `head_sha` / trunk refs and working tree; `hub.workspace.sync` on standard success.

#### `POST /workspaces/:slug/reclone`
- **Purpose:** discard the trunk and clone again.
- **Auth / scope:** PAT needs `workspaces:write`, else `403 PAT requires workspaces:write scope to reclone workspaces` (before lookup); write access.
- **Request body:** none.
- **Behaviour (order):** 401 → 403 scope → access → `archived` → 400 `workspace is archived and must be reactivated before recloning` → `pending`/`cloning` → 409 `clone operation already in progress; cannot reclone while status is '<status>'` → TryLock → 409 `workspace_busy` → rebuild active → 409 `workspace_busy` → WS-FR-074 (500 `failed to delete workspace directory: <err>`; 500 `failed to update workspace state for reclone`) → enqueue → re-read (500 `failed to read workspace after reclone`) → answer.
- **Response:** `200` workspace object with `status: "pending"`, `sync_error: null`, `upstream_head_sha: null`.
- **Side effects:** push of all branches to origin (best effort), directory removed, clone job queued. No event.

#### `POST /workspaces/:slug/branches/:name/push`
- **Purpose:** push one trunk branch to `origin`.
- **Auth / scope:** PAT needs `workspaces:write`, else `403 PAT requires workspaces:write scope to push branches` (before lookup; a PAT bound to another workspace already got 404 from the binding guard); write access.
- **Path/query params:** `:name` — branch name as one path segment (`/` escaped as `%2F`), decoded once (WS-FR-081). An unescaped `/` matches no route (404).
- **Request body (optional):** at most 64 KiB is read. Empty / whitespace-only → `force=false`. Otherwise must be a JSON object (`[]`, `null`, non-JSON → 400 `invalid request body: the body must be a JSON object`); `force` absent → false; `force` must be a JSON boolean (`"yes"`, `null` → 400 `invalid request body: force must be a boolean`); other keys ignored.
- **Behaviour (order):**
  1. 401; PAT scope 403
  2. workspace with write access (404 / 403 forbidden / 500)
  3. body → 400
  4. name → 404 `branch_not_found: "<name>" is not a valid branch name: <rule>` (`error_type: branch_not_found`)
  5. `status != ready` → 400 `workspace is not ready; push requires a ready workspace`
  6. DB-only refusals: carry_patch and name == integration branch (column, or `deploy` when empty) → 400 `integration_branch: hub builds "<name>", this workspace's integration branch, on every rebuild; it reaches the fork through REBUILD_PUSH_INTEGRATION_BRANCH` (`error_type: integration_branch`); carry_patch and a `patches` row for (slug, name) with `status != 'deleted'` → 400 `patch_branch: "<name>" is a registered patch of this workspace; patches reach the fork through PUSH_PATCHES_TO_ORIGIN` (`error_type: patch_branch`); `force` and name == `branch` column → 400 `protected_branch: "<name>" is the workspace's own branch; hub does not force-push it to origin` (`error_type: protected_branch`); patches lookup failure → 500.
  7. TryLock → 409 `workspace_busy` (an active rebuild between its locked phases does not block)
  8. open trunk → 500; with `force`: HEAD unreadable (and no `branch` column) → 500, name == HEAD's branch → 400 `protected_branch`
  9. `refs/heads/<name>` missing → 404 `branch_not_found: branch "<name>" does not exist in the workspace`
  10. resolve origin credential → 500 on store failure
  11. push (2-minute deadline) → result mapping below; trunk-open/no-origin failure inside the push → 500, no event.
- **Response:** `200`

```json
{"branch": "fix/42-nil-map", "sha": "<40-hex tip offered>", "remote": "origin",
 "remote_url": "<git_url with URL userinfo removed; scp-style unchanged>", "result": "pushed"}
```
`result`: `up_to_date` (origin already had it; also with force), `forced` (force given and origin accepted), `pushed` otherwise.
- **Errors of the push attempt** (checked in this order; all with `error_type` equal to the prefix):

| Status | error_type | Condition | Message |
|---|---|---|---|
| 504 | `forge_timeout` | deadline exceeded | `forge_timeout: origin did not finish the push within 2m0s` |
| 409 | `non_fast_forward` | git error text contains `non-fast-forward` or `fetch first` | `non_fast_forward: origin's <name>[ is at <sha>, which] is not in the workspace's branch; ` + one of: `origin refuses non-fast-forward updates of it, also with force` (force given) / `sync the workspace to bring it up to date` (name is the own branch) / `push with force to overwrite it`. `<sha>` comes from one extra ls-remote with the same credential/deadline; omitted if that fails |
| 502 | `forge_unauthenticated` | transport "authentication required" | `forge_unauthenticated: ` + `origin requires authentication and the workspace has no stored git credential; store GIT_PAT, or GIT_USERNAME and GIT_PASSWORD` / `origin rejected the workspace's GIT_PAT` / `origin rejected the workspace's GIT_USERNAME/GIT_PASSWORD` |
| 502 | `forge_forbidden` | transport "authorization failed" | `forge_forbidden: origin does not let the workspace's git credential push to this repository` |
| 404 | `forge_not_found` | transport "repository not found" | `forge_not_found: origin has no repository at the workspace's git_url, or the credential cannot see it` |
| 422 | `forge_rejected` | per-ref refusal (`command error on ...`) | `forge_rejected: origin refused the update: <reason>[ (<remote sideband messages, ≤4 KiB, whitespace-collapsed>)]`, scrubbed |
| 502 | `forge_unreachable` | DNS/TCP/TLS failure (or text with `no such host`, `connection refused`, `connection reset`, `network is unreachable`, `dial tcp`, `tls:`, `x509:`) | `forge_unreachable: origin could not be reached (a DNS, connection or TLS failure)` |
| 502 | `forge_error` | anything else | `forge_error: the push to origin failed` |

- **Side effects:** origin's branch updated; trunk worktree possibly restored (WS-FR-084); one `hub.branch.push` per attempt; one warning log per failed attempt.

#### `DELETE /workspaces/:slug`
- **Purpose:** permanently delete an archived workspace.
- **Auth / scope:** PAT needs `workspaces:write`, else 404; write access.
- **Behaviour (order):** 401 → scope (404) → access → not archived → 409 `workspace must be archived before deletion` → transaction (WS-FR-075; failure 500 `failed to delete workspace`, nothing changed) → force-close sessions → remove directory → event → answer. **[FIX-03]**
- **Response:** `204 No Content`, empty body.
- **Side effects:** see WS-FR-075; bound tokens subsequently get `401`; `hub.workspace.delete {revoked_tokens}`.

#### Permission scopes registered by this area
`workspaces:read`, `workspaces:write`, `patches:read`, `patches:write` (the latter two are used by the carry-patch routes).

### 4.6 CLI

All commands below are subcommands of `parley workspace` and use the global connection/credential flags and config of `parley` (see §3.6). Common behaviour: requests go to `<endpoint>/api/v1/...` with `Authorization: Bearer <key>`; path segments (slug, branch) are percent-encoded as one segment each; a request without its own deadline times out after 30 s. Success output is the response JSON, indented 2 spaces, no HTML escaping, on stdout. Errors print `{"error":{"code":N,"message":"..."}}` (indented) on stdout — for an API error `code` is the HTTP status from the server envelope and **`error_type` is not included** (the client does not carry it); for a client error `code` is 2 (or the command's code). Exit codes: 0 success; 1 API error (code ≥ 400) or wait failure/timeout; 2 usage/client error (bad flags, no credentials, unreachable, client timeout, undecodable answer); 3 only for `sync --fail-on-diverged`.

`--wait` flags (create, sync): `--wait` (bool, false), `--timeout` (duration, `5m0s`), `--poll-interval` (duration, `5s`). With `--wait`, the first answer is printed as **compact one-line JSON**, then the status URL is polled immediately and then every interval, each GET bounded by the remaining timeout; the final record is printed indented. On a failure state the final record is printed, then a line on stderr (`Workspace clone failed.` / `Job ended with status <s>.`), then the error envelope `{"error":{"code":1,"message":"<failure>"}}` on stdout, exit 1. On timeout: stderr `Timed out waiting for <what> to complete after <timeout>`, envelope `{"error":{"code":1,"message":"timed out after <timeout>"}}`, exit 1. A poll GET error is reported as an API/client error. A non-positive `--poll-interval` is not validated (the ticker panics).

#### `parley workspace create`
- **Purpose:** create a workspace.
- **Arguments / flags:** `--git-url` (string, required), `--slug` (string, required), `--branch` (string; sent only if non-empty), `--org` (string; sent as `org_id` only if non-empty), `--display-name`, `--description` (sent whenever the flag is given, even empty), `--workspace-mode` (sent when given), `--upstream-url`, `--integration-branch` (sent when given), `--git-pat`, `--git-username`, `--git-password` (sent when given), `--setting key=value` (repeatable; see `update`), `--wait`, `--timeout`, `--poll-interval`.
- **Client-side checks (exit 2, nothing sent), in order:** `--git-url flag is required`; `--slug flag is required`; `--git-pat and --git-username/--git-password are mutually exclusive`; `--git-username and --git-password must be provided together`; `--git-pat value must not be empty`; `--git-username value must not be empty`; `--git-username and --git-password values must not be empty` (empty password); `git credentials require an HTTPS git_url`; `--workspace-mode` given and not `standard`/`carry_patch` → `invalid --workspace-mode value: must be 'standard' or 'carry_patch'`; mode `carry_patch` and empty `--upstream-url` → `--upstream-url is required when --workspace-mode is 'carry_patch'`; mode not `carry_patch` (including not given) and `--upstream-url` given → `--upstream-url is not allowed when --workspace-mode is 'standard'`; same for `--integration-branch`. Then credentials are loaded (failure exit 2), then `--setting` is parsed (`--setting takes key=value; got "<raw>"`, exit 2).
- **Calls:** `POST /workspaces`; with `--wait`, then `GET /workspaces/:slug` until `status` is `ready` (exit 0) or `failed` (exit 1).
- **Output:** workspace JSON (with `--wait`: compact create answer, then final workspace).
- **Exit codes:** 0, 1, 2.

#### `parley workspace list`
- **Flags:** `--include-archived` (bool, false → appends `?include_archived=true`).
- **Calls:** `GET /workspaces`. **Output:** JSON array. **Exit:** 0/1/2.

#### `parley workspace get <slug>`
- **Arguments:** exactly 1. **Calls:** `GET /workspaces/:slug`. **Output:** workspace JSON.

#### `parley workspace update <slug>`
- **Arguments / flags:** exactly 1 arg; `--display-name`, `--description`, `--org` (sent as `org_id`; sent when the flag is given, even empty), `--clear-display-name`, `--clear-description`, `--clear-org` (bool; send the field as `null` and win over the matching value flag), `--setting key=value` (repeatable).
- **`--setting` parsing:** split at the first `=`; key trimmed; missing `=` or empty key → exit 2 `--setting takes key=value; got "<raw>"`; value exactly `true`/`false` → JSON boolean; empty value → `null` (reset to default); anything else → JSON string (value not trimmed; `True` is sent as a string and refused by the hub). A repeated key: last wins.
- **Client-side checks:** no update flag at all → exit 2 `at least one update flag must be provided (--display-name, --description, --org, --setting, --clear-display-name, --clear-description, --clear-org)`.
- **Calls:** `PATCH /workspaces/:slug` with a 10-second request timeout.
- **Output:** the updated workspace. An answer that is not a JSON object → exit 2 `unexpected response format`; one without `slug` → exit 2 `unexpected response format: missing required field 'slug'`.

#### `parley workspace archive <slug>`
- **Calls:** `POST /workspaces/:slug/archive` (no body). **Output:** workspace JSON.

#### `parley workspace reactivate <slug>`
- **Calls:** `POST /workspaces/:slug/reactivate` (no body). **Output:** workspace JSON (`pending`). No wait option.

#### `parley workspace delete <slug>`
- **Flags:** `--confirm` (bool, required; missing → exit 2 `--confirm flag is required to permanently delete a workspace`, nothing sent).
- **Calls:** `DELETE /workspaces/:slug`.
- **Output:** stdout `{"status": "deleted", "slug": "<slug>"}` (indented 2 spaces); stderr `Workspace '<slug>' has been permanently deleted.`

#### `parley workspace sync <slug>`
- **Flags:** `--reset-to-upstream` (bool → `?reset_to_upstream=true`), `--fail-on-diverged` (bool), `--wait`, `--timeout`, `--poll-interval`.
- **Calls:** `POST /workspaces/:slug/sync` (no body); with `--wait` and a non-empty string `rebuild_job_id` in the answer, polls `GET /workspaces/:slug/rebuilds/:id` until `status` ∈ {`completed` (exit 0), `dead_letter`, `cancelled` (exit 1)}. **[FIX-07]** `failed` (retry pending) keeps the poll going.
- **Output:** without `--wait`: indented sync answer. With `--wait`: compact sync answer, then (if a rebuild) the final job record.
- **`--fail-on-diverged`:** after printing (and after the wait), if `patches_diverged` is an array with ≥ 1 string, stderr gets `diverged patch branches: <a>, <b>` and the process exits **3** with no additional JSON on stdout. A wait failure (exit 1) takes precedence. Without the flag the field is ignored.
- **Exit codes:** 0, 1, 2, 3.

#### `parley workspace reclone <slug>`
- **Flags:** `--confirm` (bool, required; missing → exit 2 `--confirm flag is required to reclone a workspace`, nothing sent).
- **Calls:** `POST /workspaces/:slug/reclone`. **Output:** workspace JSON.

#### `parley workspace push <slug> <branch>`
- **Arguments / flags:** exactly 2 args; `--force` (bool, false).
- **Calls:** `POST /workspaces/:slug/branches/<escaped branch>/push` with body `{"force": <bool>}` (always sent).
- **Output:** with `--force`, first stderr `warning: --force: origin's <branch> may be overwritten with the workspace's branch`; then the push answer JSON.
- **Notes:** **[FIX-06]** This command uses a 3-minute request timeout instead of the default 30 seconds, so it outlasts the server's 2-minute push deadline and always receives the push's answer, including `504 forge_timeout`. In the baseline, a slow push ended the CLI with exit 2 while the push went on.

(`parley workspace patch-status` is specified in the carry-patch section.)

### 4.7 Background behaviour

#### 4.7.1 Clone queue
- In-memory FIFO (buffered channel of capacity `workers × 10`) with `[workspace].workers` worker goroutines (default 4), created at start after the workspace root exists. Up to `workers` clones run concurrently; each worker processes one job at a time.
- Enqueued by create and reclone (`fresh` jobs), by reactivate (`reuse` job), and by start-up recovery (WS-FR-048), each after its DB write. `Enqueue` blocks the request while the buffer is full; after shutdown has begun jobs are silently discarded.
- Failure handling: see WS-FR-045; nothing is retried automatically. Recovery from `failed`: `reclone`, or archive + reactivate.
- Shutdown: the queue context is cancelled when the server process ends; queued jobs are dropped, an in-flight clone is interrupted; the hub does not wait for workers.
- **Restart:** the queue itself is not durable, but **[FIX-05]** start-up recovery (WS-FR-048) enqueues every `pending` workspace again (`reuse`), and every `cloning` workspace again as `pending` (`fresh`). In the baseline these workspaces stayed stuck, and a `cloning` one could not be recovered through the API.
- Start-up order relevant here: workspace root created (failure aborts start), clone queue and sync functions initialised, stale rebuild worktrees removed, durable job queue started, schema init + routes mounted, settings migration run.

#### 4.7.2 Carry-patch post-clone setup (hand-off)
For a `carry_patch` workspace, after a successful clone and before `ready`: add remote `upstream` = `upstream_url`; fetch it with the upstream credential using refspecs `+refs/heads/*:refs/remotes/upstream/*` and `+HEAD:refs/remotes/upstream/HEAD`, no tags; set `rerere.enabled=true`, `rerere.autoupdate=true`; create the integration branch at `refs/remotes/upstream/HEAD` if it resolves, else at `HEAD`. Every failure is logged; the workspace still becomes `ready`.

#### 4.7.3 Sync extension hook point (hand-off)
One extension may be registered at start (the hub registers the carry-patch module's). It is invoked only for `workspace_mode == carry_patch`, after authentication, scope, write access, `ready` and `sync_enabled` checks, with the request, the slug and the trunk path `<root>/<slug>/trunk`. It returns either (a) handled with a map of extra top-level fields — the hub re-reads the row and answers `200` with the workspace object plus the extras (extras override equal keys); (b) not handled — the standard sync runs (the carry-patch module declines when the stored mode is not `carry_patch`); or (c) an error after it has written its own response. The extension takes its own TryLock and owns the upstream fetch, patch refresh, merge detection, `upstream_head_sha` / `last_sync_at` writes and rebuild enqueue; it does not use `sync_error`, and it ignores `reset_to_upstream`.

#### 4.7.4 Session force-close
On archive and delete, if the audit store is configured: all active agent sessions of the slug are set `terminated` with the given reason; the `afhub_agent_sessions_active{workspace=<slug>}` gauge is decremented per session; one `hub.session.force_closed` per session. Errors are logged; the operation continues.

### 4.8 Events

All are hub audit events (stored in the audit DB and broadcast on the event stream) with `event_type`, `actor_id` (caller's user id; `""` for admin tokens), `actor_type` (caller's credential type: `admin_token`, `api_key`, `pat`; `system` without auth), `resource_type`, `resource_id`, `action`, `workspace` (slug), `metadata`, `timestamp`. Emission failures are logged and never change the answer; no event when no emitter is configured.

| Event | resource_type / resource_id / action | When | Metadata |
|---|---|---|---|
| `hub.workspace.create` | `workspace` / slug / `create` | successful create | `git_url`, `branch` (`""` when null) |
| `hub.workspace.update` | `workspace` / slug / `update` | successful PATCH | `fields`: array of given mutable keys (`display_name`, `description`, `org_id`, `settings` order) |
| `hub.workspace.archive` | `workspace` / slug / `archive` | successful archive | `head_sha` only when non-null (ready archives) |
| `hub.workspace.reactivate` | `workspace` / slug / `reactivate` | successful reactivate | `{}` |
| `hub.workspace.sync` | `workspace` / slug / `sync` | successful **standard** sync | `result`: `up_to_date` / `fast_forward` / `reset_to_upstream` |
| `hub.workspace.delete` | `workspace` / slug / `delete` | successful delete | `revoked_tokens`: sorted array of revoked token ids (`[]` when none) |
| `hub.session.force_closed` | `session` / session id / `terminate`; actor `system`, actor_id `""` | per session closed by archive/delete | `session_id`, `reason` (`workspace archived` / `workspace deleted`) |
| `hub.branch.push` | `branch` / branch name (scrubbed) / `push` | every push attempt handed to the transport (success or failure; recorded even after client disconnect) | `branch`, `sha` (tip offered), `force` (bool), `ok` (bool), `remote` (`origin`), `remote_url` (git_url without URL userinfo, scrubbed), and `result` (`pushed`/`up_to_date`/`forced`) when ok, or `error_type` when not. Never a credential or a forge message |

Reclone, failed syncs, the clone job and refusals emit no workspace event.

### 4.9 Configuration

| Key | Type | Default | Validation / notes |
|---|---|---|---|
| `[workspace].path` | string | `./data/workspaces`; `$XDG_DATA_HOME/workspaces` when `XDG_DATA_HOME` is set | A bare name (no directory component) is placed under `$XDG_DATA_HOME` when set; anything else is used as is. Created (with parents, 0755) at start; failure aborts start. |
| `[workspace].workers` | integer | `4` | `0` (or absent) → 4. Not otherwise validated (a negative value crashes the start). |
| `[server].external_url` | string | `""` | Used verbatim to build `hub_url`; empty → `hub_url: null`. |
| `XDG_DATA_HOME` | env | — | See `path`. |

`$VAR` / `${VAR}` references in `config.toml` are expanded from the environment before parsing. Fixed (not configurable): credential check timeout 10 s, branch-push deadline 2 min, clone queue buffer `workers × 10`, branch-push body limit 64 KiB, remote-message capture 4 KiB, scrubbed message limit 1024 bytes, list chunk 500 slugs per query.

### 4.10 Edge cases and error handling

- **NULL head_sha.** A standard sync with a NULL `head_sha` reports `up_to_date`, records `upstream_head_sha`, and neither moves the branch nor sets `head_sha`. **[FIX-04]** A `ready` workspace can no longer have a NULL `head_sha` because of a reused clone, since reuse records the clone's `HEAD`.
- **Local ahead of origin is "diverged".** The comparison uses the stored `head_sha` (which a push to the hub's git server advances). Unpushed hub commits therefore make a standard sync answer `409 upstream history has diverged...`; `reset_to_upstream` would discard them.
- **Archive of a `pending` workspace races with its queued clone job**: the job later sets `cloning` and then `ready`/`failed`, silently un-archiving it. The same applies to a workspace archived while its clone job is still queued after reactivate.
- **Workspaces after a restart** (4.7.1): **[FIX-05]** `pending` and `cloning` workspaces are recovered at start (WS-FR-048). A `cloning` workspace still refuses archive and reclone (409) while its clone runs.
- **Delete is not atomic with its file and session effects**: on a 500, sessions are already terminated and the directory already removed, while the row, secrets, tokens remain.
- **Archive HEAD failure** returns the raw git error text as the 500 message.
- **Archive keeps the clone** when credential resolution fails or the push fails (including non-fast-forward rejection by origin); the workspace is still archived; reactivation reuses it (with NULL head_sha, see above).
- **Fast-forward / reset move the checked-out branch**: the hard reset after a ref update resets the working tree (and the branch HEAD points to) to the origin tip.
- **Reset-to-upstream** does not check request cancellation before fetching and does not record `diverged`.
- **PATCH validates the body before the lookup**; a PAT without scope gets 404 before body validation.
- **PATCH `org_id` by an admin token** to a non-empty value is refused with `403 user is not a member of the specified organization` (membership of the caller is checked, and an admin token has no user id); clearing works.
- **Settings errors** with several invalid keys report an arbitrary one. URL settings are validated trimmed but stored untrimmed.
- **Credential check messages** do not distinguish auth failure from network failure or timeout.
- **Branch push**: body > 64 KiB is truncated and fails JSON parsing (400); a name with `%` sequences is decoded exactly once; `up_to_date` is reported even with `force`; the CLI waits up to 3 minutes, longer than the 2-minute server deadline **[FIX-06]**.
- **Branch push to a workspace whose trunk has no `origin` remote** → 500, no event, no warning.
- **Sync of a workspace whose trunk is missing on disk** → `502 sync failed: upstream fetch error` with `sync_error = "upstream fetch failed: open repository at ...: ..."`.
- **Clone errors** are stored verbatim in `clone_error` (including any URL in the error text).
- **Display name / description limits** are byte lengths (multi-byte characters count more than once).
- **Rebuild in progress** blocks archive (ready) and reclone for its whole duration, but not sync, branch push, PATCH, reactivate or delete; delete of an archived workspace removes the directory without checking the lock or the rebuild flag.
- **Concurrent create with the same slug**: one gets 201, the other 409.
- **`include_archived`** accepts only the literal `true` (`1`, `TRUE` → excluded).

### 4.11 Discrepancies between docs and code

1. **Delete failure "nothing was deleted".** `docs/api.md` (DELETE errors: "500 `failed to delete workspace`; nothing was deleted"). Code removes the workspace directory and force-closes sessions before the transaction; only the DB part rolls back (`internal/workspace/handlers.go:1102-1128`). → **Resolved in this PRD by FIX-03:** the re-implementation follows the documented behaviour described here (see §1.8).
2. **Reactivation of a kept clone and `head_sha`.** `docs/api.md` Workspace Response Schema and `docs/openapi.yaml` (`head_sha` "set when `status` is `ready` or `archived`") and architecture.md ("the job finds a kept directory and sets `ready` at once"). Code sets `cloning` with `head_sha = NULL`, then `ready` with `head_sha = NULL` when the directory exists (`internal/workspace/queue.go:141-155`, `setStatus` at `queue.go:272-282`). Consequence: standard sync then always answers `up_to_date` (`sync_git.go:63-66`, `sync_handler.go:410`). → **Resolved in this PRD by FIX-04:** the re-implementation follows the documented behaviour described here (see §1.8).
3. **PATCH `org_id` membership.** `docs/api.md` PATCH table: "the owner must be a member". Code checks the **caller's** membership (`handlers.go:765-771`, `checkOrgMembership(db, auth.UserID, ...)`), so an admin token cannot move a workspace into an org.
4. **Order "access before state" for PATCH.** `docs/permissions.md` §request order (scope → workspace access → state/preconditions). PATCH validates body shape (immutable fields, types, empty body) before the workspace lookup (`handlers.go:661-735` before `lookupWorkspaceForAuth` at `:738`), so an unknown or invisible slug can get 400 instead of 404.
5. **Carry-patch sync scope message.** `docs/permissions.md:226` says a carry-patch sync answers `403 missing required scope: workspaces:write`. The workspace handler checks the scope first and answers `403 PAT requires workspaces:write scope to sync workspaces` for every mode (`sync_handler.go:83-86`); the extension's own message (`carrypatch/sync_handlers.go:100`) is unreachable.
6. **`parley workspace list` scope.** `docs/cli.md` ("List the workspaces you own"). The API returns owned workspaces plus those of the caller's orgs (`wsaccess/readable.go:14-19`), and all for an admin token.
7. **Archive `workspace_busy`.** `docs/api.md` archive errors list `409 workspace_busy` without qualification. Only a `ready` workspace takes the lock / checks the rebuild flag; `pending`/`failed` archives never answer busy (`handlers.go:922-997`).
8. **Integration/patch branch push messages.** `docs/api.md` describes these refusals in terms of the `rebuild_push_integration_branch` / `push_patches_to_origin` settings; the actual messages name the legacy variables `REBUILD_PUSH_INTEGRATION_BRANCH` and `PUSH_PATCHES_TO_ORIGIN` (`branch_push.go:458-471`).
9. **"Shallow" clone.** Code comments (`queue.go`, `clone.go`) and the sync's unshallow retry suggest shallow clones; the clone uses depth 0 = full history (`queue.go:176-185`). Docs do not claim shallow; recorded for clarity.
10. **Reclone event.** No `hub.workspace.reclone` exists; docs are consistent (no event listed), recorded for completeness (`reclone_handler.go` has no emit).
11. **CLI error envelopes and `error_type`.** `docs/api.md` branch push notes "`parley` shows only the message"; consistent: the CLI client decodes only `code` and `message` (`apikit/internal/cli/user.go:193-204`), so `error_type` never appears in `parley` output for API errors (the error-type prefix in branch-push messages exists for this reason).
12. **Wait output format.** `docs/cli.md` says the first answer is printed "on one line" — consistent (compact JSON) — but does not mention that on a failed clone/job or timeout an additional error envelope is printed on stdout after the final record (`internal/cli/wait.go:108-118`).


---

## 5. Git server

### 5.1 Purpose and scope

The hub serves every workspace's single local clone (the "trunk", `<workspace.path>/<slug>/trunk/`) over git's smart HTTP protocol, so that developers and agents can `git clone`, `git fetch` and `git push` against the hub with their hub credentials, on the same port as the REST API. It authenticates with HTTP Basic (password = hub credential), applies the hub's shared workspace access rule (owner / org member / admin token / bound token), applies PAT `git:*` scopes, protects the hub-internal ref namespace `refs/hub/replaced/`, and offers two extension points (a per-ref pre-receive hook and an asynchronous post-push hook) that the carry-patch module uses for push control and auto-rebuild. After every push it refreshes the workspace's `head_sha` and resets the trunk worktree. In scope: the three smart-HTTP routes, authentication, authorization, push semantics, push side effects, and the `parley credential-helper` git credential helper. Out of scope: SSH transport, the dumb HTTP protocol, protocol v2, the carry-patch push-control policy itself (see the carry-patch section; only the hook contract is described here).

### 5.2 User stories

**GIT-US-1** As a Developer, I want to clone a workspace with `git clone <external_url>/git/<org-slug>/<slug>.git` using my API key, so that I can work on the hub's copy of the repository.
- The URL is `/git/<org slug>/<workspace slug>.git`; the org is the organization the workspace belongs to (not the owner).
- Username is ignored; the Basic password is the hub credential.
- A workspace that has no organization has no git URL (always `404 repository not found`).

**GIT-US-2** As an Agent with a PAT, I want to push my branch to the hub so that it can be merged by the merge queue.
- The PAT needs `git:write` (which implies `git:read`); a PAT with only `git:read` can fetch but not push (`403`).
- A PAT bound to a different workspace gets `404 repository not found`, exactly as a stranger, and the refusal is reported as a `hub.token.bound_refused` event (surface `git`).

**GIT-US-3** As an Org member, I want read-only access to my org's workspaces so that I can fetch but not change them.
- Clone/fetch is served as for the owner; any push (including the push ref discovery `info/refs?service=git-receive-pack`) is refused with pkt-line `403 ERR org members have read-only access to this workspace`, whatever the PAT scopes.

**GIT-US-4** As an Operator with the admin token, I want to clone and push any workspace that belongs to an org.

**GIT-US-5** As a Developer, I want incremental fetches to transfer only missing objects, and large fetch negotiations (gzip-compressed by git) to work.

**GIT-US-6** As a Developer, I want the hub to refuse writes to hub-internal backup refs (`refs/hub/replaced/*`) but let me fetch them by name.

**GIT-US-7** As the carry-patch module, I want to be consulted for every pushed ref update before it is written (to refuse / forward it) and to be told which branches were pushed afterwards (to enqueue a rebuild / mirror), so that push control works without the git server knowing about patches.

**GIT-US-8** As a Developer, I want `git` to obtain my hub credential automatically through `parley credential-helper`, so that plain hub URLs work without embedding the token, and the key is never sent to a different scheme/host/port.

### 5.3 Concepts and data model

**Git URL.** `<external_url>/git/<org-slug>/<workspace-slug>.git/` followed by `info/refs`, `git-upload-pack` or `git-receive-pack`. The workspace record's `hub_url` field carries this URL (null when `server.external_url` is empty or the workspace has no org). `<org-slug>` must be the slug of the org whose id equals the workspace's `org_id`.

**Trunk repository.** One non-bare git repository per workspace at `<[workspace].path>/<slug>/trunk/`. Its checked-out branch (`HEAD`) is what `head_sha` reflects. Served only while the workspace `status` is `ready`.

**Hub credential (git side).** Read from the HTTP Basic password; classified by prefix:

| Prefix / form | Credential type (`actor_type`) | Lookup |
|---|---|---|
| `af_pat_<rest>` | `pat` | Table `pats`: first try `token_id = <rest>` with secret `<rest>`; if no match and `<rest>` contains `_` (not first/last char), split at the first `_` into `token_id` and `secret`. Row must have `revoked_at IS NULL` and (`expires_at IS NULL` or `expires_at > now`); `secret_hash` must equal hex SHA-256 of the secret (constant-time compare). Scopes = JSON array `permissions`. |
| `af_key_<rest>` | `api_key` | Table `api_keys`, same two-try scheme on `key_id`/secret, same revoked/expired rules. |
| `af_admin_<...>` | `admin_token` | hex SHA-256 of the whole token must equal the `value` of an `admin_config` row whose `key` starts with `admin_token_hash`. No user id. |
| `af_<key_id>_<secret>` (any other `af_` token; OAuth-login format) | `api_key` | Split after `af_` at the first `_`; `api_keys` lookup as above. |
| anything else | — | unrecognized → 401 |

For PATs and API keys the user's `role` and `status` are read from `users`; a user with `status = 'blocked'` is refused (401). If the user row cannot be read, role defaults to `user` and the credential is accepted. A user whose role is `admin` is **not** an admin token for git purposes.

**Git scopes** (registered with apikit for PAT issuance): `git:read` (clone, fetch), `git:write` (push; implies `git:read`). Only PATs are scope-checked; API keys and admin tokens implicitly hold both.

**Access level** (shared workspace access rule): `admin` (admin token), `owner` (credential's user = workspace `owner_id`), `member` (row in `org_members` for the workspace's org and the user), none. Need: `read` for upload-pack and info/refs except `service=git-receive-pack`; `write` for receive-pack and `info/refs?service=git-receive-pack`.

**Token binding.** A PAT may be bound to one workspace (`token_workspace_bindings`, created by `MountGitHandlers` at start if missing — the git routes are not mounted if the table cannot be created). A bound PAT addressing another workspace is treated as not found.

**Protected ref namespace.** Exact prefix `refs/hub/replaced/`. Other `refs/hub/*` names are not protected.

**Ref update** (hook input): `{name: full ref name, old: 40-hex (zero hash if the ref does not exist), new: 40-hex (zero hash for a delete)}`.

**Persisted state touched:** `workspaces.head_sha` (updated after pushes), the trunk's objects/refs/worktree. No git-server-specific tables.

### 5.4 Functional requirements

Routing and request chain
- **GIT-FR-001** Exactly three routes exist, mounted on the root HTTP server outside `/api/v1`: `GET /git/:org/:slug.git/info/refs`, `POST /git/:org/:slug.git/git-upload-pack`, `POST /git/:org/:slug.git/git-receive-pack`.
- **GIT-FR-002** Every request under the group passes, in order: (1) HTTP Basic authentication, (2) `.git` suffix check, (3) workspace resolution, (4) workspace access check, (5) repository open; then the handler checks (6) the `service` parameter (info/refs only) and (7) the PAT scope. Any refusal stops the chain.
- **GIT-FR-003** The second path segment must end in `.git`; otherwise the answer is the router's JSON 404 `{"error":{"code":404,"message":"Not Found"}}` (only reached after successful authentication; without credentials the answer is the 401 challenge). Unknown sub-paths under a valid repo prefix run the same middleware chain and then get the JSON 404.
- **GIT-FR-004** Git routes are exempt from the API's request body size limit (`server.max_body_size`, default 1MB): for any path starting with `/git/` the original body is used unbounded, so pushes of any size are accepted.

Authentication
- **GIT-FR-010** No `Authorization` header → `401`, empty body, header `WWW-Authenticate: Basic realm="parleyd"`.
- **GIT-FR-011** Header not starting with `Basic `, undecodable base64, no `:` in the decoded value, or an unrecognized/revoked/expired credential, or a blocked user → `401`, empty body, **no** `WWW-Authenticate` header. (Bearer tokens are not accepted on git routes.)
- **GIT-FR-012** The Basic username is never consulted (empty, `x`, `x-token-auth`, anything).
- **GIT-FR-013** Authentication runs before any workspace lookup: a blocked user gets 401 even if the workspace tables are unreadable.

Resolution and authorization
- **GIT-FR-020** Workspace resolution: look up the workspace by `<slug>`. Not found, workspace with no org (`org_id` NULL/empty), org slug unknown, or org id mismatch → `404` with pkt-line body `ERR repository not found\n` (Content-Type `text/plain`). A database error → `500` with empty body, Content-Type `text/plain`.
- **GIT-FR-021** Workspace found and org matches but `status != "ready"` (`cloning`, `failed`, `archived`, …) → `404` pkt-line `ERR repository not available\n`. This check precedes the access decision: every caller (including strangers and bound PATs) gets this answer and no bound-token refusal is reported.
- **GIT-FR-022** Access decision (shared rule, in order): stranger (no access) → 404 `repository not found` (no binding lookup, no report); PAT bound to another workspace → 404 `repository not found` and one bound-refusal report `{token_id, user_id, bound_slug, requested_slug, method, route: "/git/:org/:slug.git/<route>", status: 404, surface: "git"}`; member with need `write` → `403` pkt-line `ERR org members have read-only access to this workspace\n`; otherwise allowed. Membership is read on every request (a removed member is a stranger on the next request). A failed membership or binding lookup → `500` pkt-line `ERR internal server error\n`. API keys and admin tokens never consult the binding table.
- **GIT-FR-023** Need is derived from the route: `git-receive-pack` → write; `info/refs` → write iff the **first** `service` query value is `git-receive-pack`, else read (including a missing/invalid service); `git-upload-pack` → read.
- **GIT-FR-024** After access is granted the trunk is opened; failure (missing on disk, not a repo) → `500`, empty body, Content-Type `text/plain`.
- **GIT-FR-025** info/refs `service` must be exactly `git-upload-pack` or `git-receive-pack`; otherwise `403` with pkt-line body `ERR invalid or missing service parameter\n`, Content-Type `text/plain` (checked after access, before the scope).
- **GIT-FR-026** PAT scope check (after access): `git:read` needed for info/refs?service=git-upload-pack and upload-pack; `git:write` for info/refs?service=git-receive-pack and receive-pack; `git:write` satisfies `git:read`; no other scope grants either. Refusal → `403` JSON envelope `{"error":{"code":403,"message":"insufficient git permissions"}}`. Because access precedes scope, a member's push is the pkt-line read-only 403 regardless of scopes, and a stranger's is 404 regardless of scopes.

Ref discovery (`info/refs`)
- **GIT-FR-030** Success: `200`, `Content-Type: application/x-<service>-advertisement`, body = pkt-line `# service=<service>\n`, flush `0000`, then the advertisement, ending with a flush.
- **GIT-FR-031** For `git-upload-pack` the advertisement is produced by the git CLI (`git upload-pack --stateless-rpc --advertise-refs <trunk>`), so it carries the capabilities of the installed git (e.g. `multi_ack_detailed`, `side-band-64k`, `ofs-delta`, `shallow`, `symref=HEAD:...`, …) and lists all refs including `refs/hub/replaced/*`. If git is not found → body continues with pkt-line `ERR git executable not found: ...`; if git fails → pkt-line `ERR failed to advertise references` (status remains 200).
- **GIT-FR-032** For `git-receive-pack` the advertisement is produced by the hub: a `<sha> HEAD` line first (when HEAD resolves), then every ref sorted by name; the first line carries `\0` + capabilities `agent=go-git/5.x ofs-delta delete-refs report-status` (no `side-band`, `atomic`, `push-options`, `quiet`). Errors while building it → pkt-line `ERR <message>` in a 200 body.
- **GIT-FR-033** Protocol: v0 only. A client's `Git-Protocol: version=2` request header is ignored; git upload-pack is run with an empty `GIT_PROTOCOL` so the client falls back to v0.

Fetch (`git-upload-pack`)
- **GIT-FR-040** Response headers `200`, `Content-Type: application/x-git-upload-pack-result`, `Cache-Control: no-cache` are written before processing; all errors after that are in-band pkt-line `ERR …` messages with HTTP 200.
- **GIT-FR-041** `Content-Encoding: gzip` (case-insensitive) request bodies are gunzipped; an invalid gzip stream → `ERR invalid gzip request body: <err>`.
- **GIT-FR-042** An empty request body → `ERR empty upload-pack request`.
- **GIT-FR-043** The body is piped to `git upload-pack --stateless-rpc <trunk>` (env `GIT_TERMINAL_PROMPT=0`, `GIT_PROTOCOL=` empty) and its stdout streamed to the client with a flush after every write (progress via side-band streams in real time). Full have/ACK/NAK negotiation, multi_ack_detailed, side-band-64k, shallow and (if the trunk's git config allows) filter are supported, so incremental fetches transfer only missing objects.
- **GIT-FR-044** If git produced no output and failed → `ERR <first line of git's stderr>` (or `ERR upload-pack failed` when stderr is empty); no output and success (malformed request) → `ERR invalid upload-pack request`; git missing → `ERR git executable not found: …`.
- **GIT-FR-045** A single upload-pack invocation is bounded by 30 minutes when the request context has no deadline; the subprocess is given 5 s to exit after cancellation.
- **GIT-FR-046** Any ref, including `refs/hub/replaced/<name>`, can be fetched by name.

Push (`git-receive-pack`)
- **GIT-FR-050** Response headers `200`, `Content-Type: application/x-git-receive-pack-result` are written before processing; errors are in-band.
- **GIT-FR-051** An empty request body is a successful no-op: body is exactly pkt-line `unpack ok\n` (`000eunpack ok\n`) followed by flush `0000`; `head_sha` is refreshed (GIT-FR-060); no hook is called, no event emitted.
- **GIT-FR-052** The command list is decoded, then the pack is unpacked into the trunk's object store (thin packs with REF_DELTA against existing objects are supported). A command list that cannot be decoded → pkt-line `ERR <decode error>`, nothing changes (`head_sha` not refreshed, no event). An unpack failure: with report-status the client receives the report `unpack <error>` with no per-ref lines (no ref is written; `head_sha` is still refreshed per GIT-FR-060); without report-status the client receives pkt-line `ERR <error>` and `head_sha` is not refreshed; in both cases no event and no hook.
- **GIT-FR-053** Ref updates are applied one by one in command order (not atomic). Per ref: create of an existing ref, update or delete of a non-existing ref → `ng <ref> failed to update ref`; otherwise the protected-ref rule (GIT-FR-054), then the pre-receive hook (GIT-FR-055), then the write. Any ref name is accepted (branches, tags, `refs/notes/*`, other `refs/hub/*`), not only the workspace's branch.
- **GIT-FR-054** Any create, update or delete of a ref under `refs/hub/replaced/` (any workspace, any credential, including admin) is rejected with `ng <ref> refs/hub/replaced/ is maintained by the hub and cannot be pushed to`; the pre-receive hook is not consulted for it; the ref keeps its value; other refs of the same push proceed; the report still says `unpack ok`.
- **GIT-FR-055** Pre-receive hook point: if a hook is registered, it is called once per remaining ref update, after the pack's objects are in the trunk and before the ref is written, with (request context, database, workspace slug, authenticated actor, `{name, old, new}`), where `old` is the ref's current value (zero hash if absent) and `new` is the zero hash for deletes. A non-nil error rejects only that update: the ref is not written and the error text becomes the per-ref message (`ng <ref> <message>`, shown by git as `! [remote rejected] … (<message>)`). With no hook registered, all updates that pass GIT-FR-053/054 are written.
- **GIT-FR-056** No fast-forward check and no compare-and-swap on the old value: non-fast-forward updates (`git push --force`) and deletes (including the trunk's checked-out branch) are accepted by the server. (The git client itself refuses a non-forced non-fast-forward push before sending.)
- **GIT-FR-057** Client without report-status capability: rejections are still enforced but no per-ref messages are sent; if only hook/protected-ref rejections occurred, no `ERR` line is written, and accepted refs keep their side effects.
- **GIT-FR-058** Accepted updates = refs with report-status `ok`; without report-status, all commands not rejected by the protected-ref rule or the hook.

After a push
- **GIT-FR-060** After every push that got past decoding (including pushes where every ref was rejected), the hub opens the trunk, reads `HEAD`, and, if it can acquire the workspace lock without waiting, hard-resets the worktree to `HEAD` and releases the lock; if the workspace is busy (merge, rebuild, sync, … holding the lock) the reset is skipped (the running operation restores the tree). Then `workspaces.head_sha` is set to `HEAD`'s SHA. Failures (e.g. `HEAD` dangling after deleting the checked-out branch) are logged; `head_sha` is then left unchanged; the push response is unaffected. Ref writes themselves do not take the workspace lock.
- **GIT-FR-061** If at least one update was accepted: emit one `hub.git.push` event (5.8) synchronously; then, if a post-push hook is registered and at least one accepted, non-delete update is under `refs/heads/`, call the hook asynchronously (fire-and-forget goroutine) with (database, slug, branch names without `refs/heads/`, in command order). If no update was accepted: no event and no hook call.
- **GIT-FR-062** Audit emission failures are logged and never fail the push; without an emitter nothing is emitted.

Hook contract as wired in production (details in the carry-patch section)
- **GIT-FR-070** Pre-receive hook = carry-patch push control: on a `carry_patch` workspace, **[FIX-11]** every update of the integration branch is refused (CP-FR-189), and an update of a registered patch branch (`refs/heads/<patch name>`, patch not `deleted`, not the integration branch) is refused, forwarded to origin, or accepted depending on the workspace settings `patch_branch_source` / `push_patches_to_origin`; all other updates and all standard workspaces pass through unchanged.
- **GIT-FR-071** Post-push hook = carry-patch auto-rebuild/mirror: for accepted patch-branch updates on a `carry_patch` workspace, enqueue a rebuild (unless setting `auto_rebuild_after_push` is `false`) and, in hub mode with `push_patches_to_origin`, mirror to the fork. The push answer never waits for it.

Credential helper
- **GIT-FR-080** `parley credential-helper <verb>` implements git's credential helper protocol. Only `get` produces output; `store`, `erase` and any other verb read nothing, print nothing and exit 0.
- **GIT-FR-081** Configuration resolution: start from `~/.af/config.toml` (keys `endpoint_url`, `api_key`; `${VAR}`/`$VAR` references in the file are environment-expanded before parsing; a missing/unparsable file means empty values), then env `ENDPOINT_URL` overrides the URL and env `API_KEY` overrides the key, each independently when non-empty. `AF_`-prefixed variables, `AF_CONFIG_DIR` and CLI flags are not consulted. If URL or key is empty, or the URL does not parse or has no host → print nothing, exit 0.
- **GIT-FR-082** Input: lines `key=value` on stdin until the first empty line or EOF; lines without `=` ignored; later duplicates overwrite earlier.
- **GIT-FR-083** Matching: answer only if input `protocol` equals the hub URL's scheme exactly, input `host` is non-empty, and the normalized host:port of both match: host lowercased; a missing port defaults to 443 for `https` and 80 for `http`. (So `https` hub + `protocol=http` request → no answer; another port or host → no answer.) The path is not considered.
- **GIT-FR-084** Output on match, exactly four lines: `protocol=<input protocol>\n`, `host=<input host verbatim>\n`, `username=x-token-auth\n`, `password=<api key>\n`.

### 5.5 REST API

All three endpoints are git smart-HTTP endpoints (not under `/api/v1`), with HTTP Basic authentication.

Common refusals (in check order; identical for the three routes):

| # | Status | Body | Content-Type | Condition |
|---|---|---|---|---|
| 1 | 401 + `WWW-Authenticate: Basic realm="parleyd"` | empty | — | no Authorization header |
| 2 | 401 | empty | — | not Basic / bad base64 / no colon / unknown, revoked, expired credential / blocked user |
| 3 | 404 | `{"error":{"code":404,"message":"Not Found"}}` | JSON | second segment does not end in `.git` |
| 4 | 500 | empty | text/plain | DB error during workspace/org lookup |
| 5 | 404 | pkt `ERR repository not found\n` | text/plain | unknown workspace, no org, wrong/unknown org |
| 6 | 404 | pkt `ERR repository not available\n` | text/plain | workspace status ≠ `ready` |
| 7 | 404 | pkt `ERR repository not found\n` | text/plain | caller has no access, or PAT bound to another workspace |
| 8 | 403 | pkt `ERR org members have read-only access to this workspace\n` | text/plain | member and need = write |
| 9 | 500 | pkt `ERR internal server error\n` | text/plain | membership/binding lookup failed |
| 10 | 500 | empty | text/plain | trunk cannot be opened |

(pkt `X` = 4-hex-digit length prefix including itself, followed by `X`; e.g. `ERR repository not found\n` → `001dERR repository not found\n`.)

#### `GET /git/:org/:slug.git/info/refs`
- **Purpose:** smart-HTTP ref discovery for fetch/clone or push.
- **Auth / scope:** HTTP Basic with PAT, API key (all formats) or admin token. Need read (write for `service=git-receive-pack`). PAT: `git:read` or `git:write` for upload-pack; `git:write` for receive-pack. Bound PAT: only its own workspace.
- **Path/query params:** `org` (org slug), `slug.git` (workspace slug + `.git`); query `service` (required; first value used): `git-upload-pack` | `git-receive-pack`.
- **Request body:** none.
- **Behaviour:** common chain → service check → scope check → `200`, `Content-Type: application/x-<service>-advertisement`, pkt `# service=<service>\n`, `0000`, advertisement (upload-pack: git CLI's; receive-pack: hub's, HEAD first, refs sorted, caps `agent=go-git/… ofs-delta delete-refs report-status`), final `0000`.
- **Response:** 200 as above.
- **Errors:** common refusals; then `403` pkt `ERR invalid or missing service parameter\n` (text/plain); then `403` JSON `{"error":{"code":403,"message":"insufficient git permissions"}}`; in-band (200) `ERR git executable not found: …` / `ERR failed to advertise references` / `ERR <go-git error>`.
- **Side effects:** possibly one `hub.token.bound_refused` (rate-limited) for a bound PAT refusal.

#### `POST /git/:org/:slug.git/git-upload-pack`
- **Purpose:** fetch/clone data transfer (stateless RPC).
- **Auth / scope:** as above, need read; PAT `git:read` (or `git:write`).
- **Path/query params:** as above.
- **Request body:** git upload-pack request (pkt-lines: wants, haves, done…), optionally `Content-Encoding: gzip`. No size limit.
- **Behaviour:** common chain → scope → headers `200`, `application/x-git-upload-pack-result`, `Cache-Control: no-cache` → GIT-FR-041…045.
- **Response:** 200 with git upload-pack output (ACK/NAK, side-band pack data).
- **Errors:** common refusals; JSON 403 scope; in-band `ERR invalid gzip request body: …`, `ERR empty upload-pack request`, `ERR invalid upload-pack request`, `ERR <git stderr first line>`, `ERR upload-pack failed`, `ERR git executable not found: …`.
- **Side effects:** none.

#### `POST /git/:org/:slug.git/git-receive-pack`
- **Purpose:** push data transfer.
- **Auth / scope:** as above, need write (owner or admin token; members 403); PAT `git:write`.
- **Path/query params:** as above.
- **Request body:** receive-pack request: command list (`<old> <new> <ref>`, capabilities on the first line), flush, packfile. No size limit; empty body allowed (no-op).
- **Behaviour:** common chain → scope → headers `200`, `application/x-git-receive-pack-result` → GIT-FR-051…058 → GIT-FR-060…062.
- **Response:** 200 with report-status (`unpack ok|<err>`, `ok <ref>` / `ng <ref> <msg>` lines, flush) when requested by the client; otherwise empty body (or `ERR` line on session failure). Empty request: `unpack ok` + flush.
- **Errors:** common refusals; JSON 403 scope; in-band `ERR <decode error>`, `ERR <session error>`; per-ref `ng` messages: `failed to update ref`, `refs/hub/replaced/ is maintained by the hub and cannot be pushed to`, or the pre-receive hook's message (e.g. carry-patch `branch is synced from origin; push to <git_url> instead`).
- **Side effects:** objects added to trunk; accepted refs written; worktree hard reset (if lock free); `workspaces.head_sha` updated; `hub.git.push` event; post-push hook (async).

### 5.6 CLI

#### `parley credential-helper <get|store|erase>`
- **Purpose:** git credential helper that hands the configured hub credential to git for the hub's own URL. Hidden from `parley --help`; needs no login/authentication pre-check.
- **Arguments / flags:** exactly one positional argument (the git credential verb). No flags of its own (global flags are not used for its configuration).
- **Calls:** no endpoint (local only).
- **Output:** for `get` with a match: `protocol=…\nhost=…\nusername=x-token-auth\npassword=<key>\n` on stdout; otherwise nothing.
- **Exit codes:** 0 in all handled cases (no match, missing config, store/erase/unknown verb). Wrong argument count → cobra usage error printed as JSON envelope `{"error":{"code":0,"message":"accepts 1 arg(s), received N"}}` on stdout, exit 2.
- **Notes:** configure with `git config --global credential.<hub-url>.helper '!parley credential-helper'`. Matching rules GIT-FR-081…084. The credential sent is whatever `api_key`/`API_KEY` holds (API key or PAT).

### 5.7 Background behaviour
- The post-push hook runs in its own goroutine per push; the HTTP response has already been written; its errors are only logged.
- At startup `MountGitHandlers` ensures the token-binding table exists; failure aborts startup (no git routes are registered).
- No workers, schedules or retries in the git server itself.

### 5.8 Events

| Event type | When | Fields |
|---|---|---|
| `hub.git.push` | After a receive-pack with ≥1 accepted update | `resource_type: "workspace"`, `action: "push"`, `workspace: <slug>`, `resource_id: ""`, `actor_id: <user id>` (`""` for admin token), `actor_type: pat|api_key|admin_token`; `metadata.head_sha`: `new` SHA of the last accepted non-delete update in command order (`""` if all accepted updates are deletes); `metadata.refs_updated`: array of all accepted full ref names in command order (deletes included). |
| `hub.token.bound_refused` | Bound PAT addresses another workspace (rate-limited: ≤1 per token per minute; held-back refusals counted in `suppressed`) | `actor_type: "pat"`, `actor_id: <user>`, `resource_type: "token"`, `resource_id: <token_id>`, `action: "refuse"`, `workspace: <bound slug>`, metadata `{token_id, bound_workspace, requested_workspace, method, route, status: 404, surface: "git", suppressed}`. |

Events emitted by the hooks (carry-patch: `hub.patch.mirror_failed`, rebuild events) are described in the carry-patch section.

### 5.9 Configuration

| Key / env | Type | Default | Effect here |
|---|---|---|---|
| `[workspace].path` | string | `./data/workspaces` (bare name under `$XDG_DATA_HOME`) | Root of trunks: `<path>/<slug>/trunk/`. |
| `[server].external_url` | string | `""` | Prefix of `hub_url` (`<external_url>/git/<org>/<slug>.git`); empty → `hub_url` null. Not used for routing. |
| `[server].max_body_size` | string | `1MB` | Does **not** apply to `/git/*`. |
| `git` on `PATH` | binary | — | Required for upload-pack (no version check there). |
| `ENDPOINT_URL`, `API_KEY` (parley side) | string | — | Credential helper overrides of `~/.af/config.toml` `endpoint_url` / `api_key`. |
| `HOME` (parley side) | path | — | Location of `~/.af/config.toml` for the credential helper. |

Hard-coded: upload-pack timeout 30 min; realm `parleyd`; protected prefix `refs/hub/replaced/`.

### 5.10 Edge cases and error handling
- Not-ready workspaces answer `repository not available` to every caller, including strangers — the existence of a non-ready workspace in a known org is observable (no anti-enumeration for that state). Unknown workspace, wrong org and no access are indistinguishable (`repository not found`).
- The same workspace slug under a different org slug is `repository not found`.
- `info/refs?service=git-upload-pack&service=git-receive-pack` is treated as upload-pack (first value decides both need and service); a member gets 200.
- A PAT with no scopes on its own workspace: read → JSON 403 scope; push by a member with that PAT → pkt-line read-only 403.
- Scope refusals are JSON (git shows `The requested URL returned error: 403`), access refusals are pkt-line text.
- A user row missing for an API key/PAT does not block authentication (role `user`).
- Pushing a delete of the trunk's checked-out branch succeeds; afterwards `HEAD` is dangling, `head_sha` stays stale and later operations may fail.
- Pushing while a merge/rebuild holds the workspace lock: refs and `head_sha` are updated immediately, worktree reset is skipped.
- Pre-receive hook rejections still leave the pushed objects in the trunk's object store (unreferenced).
- Without report-status, a client gets no message for rejected refs (silent rejection).
- A failed/unreadable command list (and an unpack failure without report-status) leaves `head_sha` untouched; a decoded push with all refs rejected, or an unpack failure with report-status, still refreshes `head_sha` (no event).
- Credential helper: hub URL `https://hub` and request `host=HUB:443` → match; `http` request for an `https` hub → no answer; config file with `${VAR}` placeholders is expanded from the environment.

### 5.11 Discrepancies between docs and code
1. **Missing `.git` suffix.** `docs/api.md` (Git Server Endpoints): "a URL without it is `404`". Code: the suffix check runs after authentication (`hub2/internal/gitserver/handlers.go:73-77`, auth middleware first), so an unauthenticated request gets the 401 challenge; only an authenticated one gets the JSON 404.
2. **Worktree after push.** `docs/api.md` "After a push" item 1: "the clone's checked-out files follow the new HEAD". Code skips the reset when the workspace lock is held (`handlers.go:437-447`); refs and `head_sha` still update. (`docs/architecture.md` documents this correctly.)
3. **Scope 403 format.** `docs/api.md` describes git refusals as pkt-line errors; the PAT scope refusal is a JSON envelope (`handlers.go:481`, rendered by apikit's error handler `apikit/errors.go:57-79`).
4. **Not-ready precedence.** `docs/api.md` errors table for info/refs lists 404 "has no ready clone" together with "the caller may not see it"; code returns a distinct message (`repository not available`) and returns it before the access check, to strangers too (`resolver.go:136-138`).
5. **Credential helper precedence.** `docs/cli.md` (parley credential-helper): reads `ENDPOINT_URL`/`API_KEY` "when they are set, otherwise from `~/.af/config.toml`". Code merges per field: the file is read first and each env var overrides only its own field (`hub2/internal/cli/credential_cmd.go:78-90`), so e.g. `API_KEY` from env with `endpoint_url` from the file works.
6. **Protocol header.** `docs/api.md` says the upload-pack server "speaks protocol v0 (a client asking for v2 falls back to v0)": correct for upload-pack; also true for receive-pack (go-git, v0 only) — not stated.

---

## 6. Merge queue and batch rebase

### 6.1 Purpose and scope

The merge queue integrates an agent's branch into a target branch of the hub's trunk with rebase-then-fast-forward semantics, asynchronously, durably and serialized, so that concurrent agents can submit merges without corrupting the shared clone and without merge commits. Jobs are persisted in a durable SQLite job queue (shared with carry-patch rebuild jobs) that supports retries with exponential backoff, dead-lettering, cancel of queued jobs, requeue of dead-lettered jobs, and crash recovery. A separate synchronous endpoint rebases a list of branches onto a ref in one request. In scope: the merge REST API, the merge algorithm, the batch rebase endpoint, the `parley merge` and `parley rebase` commands, the observable job queue semantics. Out of scope: pushing to origin or a forge (merges change only the hub's trunk), pull requests, carry-patch rebuild semantics (another section; only the shared queue behaviour is described).

### 6.2 User stories

**MRG-US-1** As an Agent, I want to submit "merge `feature/x` into `main`" and get a job id immediately, so that I can continue while the hub integrates my work.
- `POST /api/v1/workspaces/:slug/merges` answers `202` with the queued job.
- A second identical submission while the first is queued, running or waiting to retry answers `409 duplicate_merge` **[FIX-08]**.

**MRG-US-2** As a Developer, I want `parley merge submit --wait` to block until the merge finishes and exit non-zero unless it completed, so that scripts can rely on it.

**MRG-US-3** As a Developer, I want a conflicting merge to fail with the list of conflicting files and never leave the target half-applied.
- The job ends `dead_letter` with `conflict_files` set and the target branch unchanged.

**MRG-US-4** As an Org member, I want to read a workspace's merge jobs but not submit, cancel or requeue them (403).

**MRG-US-5** As a Developer, I want to cancel a merge that has not started, and requeue a dead-lettered one after fixing the cause.

**MRG-US-6** As an Agent, I want to rebase several branches onto `main` in one synchronous call and get a per-branch result, so that I can keep a stack of branches current.
- A conflict on one branch does not stop the others; the call is refused with `409 workspace_busy` while another trunk operation runs.

**MRG-US-7** As an Operator, I want queued and running jobs to survive a hub restart.

### 6.3 Concepts and data model

**Merge job** (a row of the durable queue's `jobs` table, `type = "merge"`):

| Field | Type | Meaning |
|---|---|---|
| `id` | UUID v4 string | Job id. |
| `type` | string | `"merge"`. |
| `key` | string | `<slug>:<target_branch>:<source_ref>` — dedup key. |
| `group_key` | string | `<slug>:<target_branch>` — serialization group. |
| `nonce` | string | Random UUID per submission (so nonce idempotency never applies to API submissions). |
| `status` | enum | see state machine. |
| `payload` | JSON | `{"workspace_slug","target_branch","source_ref","submitted_by"}`. |
| `result` | JSON or NULL | On success `{"base_sha","merged_sha"}`. |
| `error` | text or NULL | Last failure: JSON `{"reason":"…","conflict_files":[…]}` (conflict_files omitted when empty) for structured failures, else plain text. Not cleared on later success or requeue. |
| `progress` | JSON or NULL | Unused by merges (used by rebuilds). |
| `retry_count` | int | Retryable failures counted; reset to 0 by requeue. |
| `available_at` | RFC3339 (seconds, UTC) | Earliest time the job may be claimed. |
| `submitted_by` | string | User id of the API key / PAT owner, or `"admin"` for the admin token. |
| `created_at`, `updated_at` | RFC3339 (seconds, UTC) | Creation / last change. |

Persistence: table `jobs` in the hub's main SQLite database (created idempotently at start, plus migrations adding `group_key TEXT NOT NULL DEFAULT ''` and `progress TEXT`). Indexes: unique on `nonce`; `(type,key,status)`; `(status,available_at)`; `(type,created_at)`. No retention: rows are never deleted.

**Job status state machine:**

| From | To | Trigger |
|---|---|---|
| (new) | `queued` | enqueue (`available_at = now`) |
| `queued` | `running` | worker claim (atomic) |
| `queued` | `cancelled` | cancel |
| `running` | `completed` | handler success (result stored) |
| `running` | `failed` | retryable error with retries left (`retry_count+1`, `available_at = now + backoff`, error stored) |
| `running` | `dead_letter` | permanent error (retry_count unchanged); retryable error with retries exhausted (`retry_count+1` stored); handler panic (`handler panicked: …`); result not serializable (`result serialization failed: …`); no handler for the type (`no handler registered for type "…"`) |
| `running` | `queued` | hub startup recovery (`available_at = now`, retry_count unchanged) |
| `failed` | `queued` | promotion when `available_at <= now` (checked by each worker before each poll) |
| `dead_letter` | `queued` | requeue (`retry_count = 0`, `available_at = now`) |

`completed` and `cancelled` are terminal. `failed` is a transient "waiting for retry" state, not final.

**Merge job response** (`MergeJobResponse`, all fields always present):

| Field | Type | Source / rule |
|---|---|---|
| `id` | string | job id |
| `workspace_slug` | string | payload (`""` if payload unparsable) |
| `target_branch` | string | payload |
| `source_ref` | string | payload |
| `status` | string | `queued`, `running`, `completed`, `failed`, `dead_letter`, `cancelled` |
| `base_sha` | string or null | result `base_sha` when non-empty, else null |
| `merged_sha` | string or null | result `merged_sha` when non-empty, else null |
| `conflict_files` | string[] | from structured error's `conflict_files`, else `[]` (never null) |
| `error` | string or null | the raw stored error text (the JSON string itself for structured errors), null when none |
| `retry_count` | int | |
| `submitted_by` | string | payload |
| `created_at`, `updated_at` | string | RFC3339 UTC, second precision (`2026-01-02T03:04:05Z`) |

**Error reasons** (structured `error.reason`): `WouldConflict` (permanent; with `conflict_files`), `BranchNotReady` (retryable; source head == target head), `invalid_payload` (permanent), `""` (retryable; target ref update failed or trunk unavailable at that step). Plain-text errors: e.g. `merge: fetch from upstream: …` (retryable), `merge: resolve clone auth for "<slug>": …` (retryable), `cannot resolve source ref "<ref>": git rev-parse …` (permanent), `cannot resolve target ref …` (permanent), `cannot check ancestry: …` (permanent), `merge: checkout "<ref>": …` (permanent), `merge: rebase "<src>" onto "<tgt>": …` (permanent), `merge: resolve target head: …` / `merge: resolve rebased source head: …` (retryable), `workspace trunk dir not found: …`, `merge: invalid base_sha length …` (permanent). Git error texts have the form `git <args>: exit code <n>: <stderr>` with URL credentials redacted.

**Batch rebase result** (`RebaseResult`): `branch` (string), `status` (`ok` | `conflict` | `error`), `new_head` (40-hex, only when `ok`), `conflict_files` (string[], only when `conflict`; `["(unresolved conflict)"]` if git reported a conflict without parseable paths).

**Retry policy** (merge jobs register with the default): base 2 s, multiplier 2, cap 2 h, max retries 20. Delay after the n-th failure (n = new retry_count) = min(2 s × 2ⁿ, 2 h): 4 s, 8 s, 16 s, …; the 21st retryable failure dead-letters with `retry_count = 21`.

**Workspace lock.** In-process per-workspace mutex shared by all trunk-mutating operations (push worktree reset, sync, archive, reclone, merge, batch rebase, rebuild phases, rollback, rerere, …). Merge jobs wait for it (blocking); request handlers try it and answer `409 workspace_busy`. The separate rebuild-active flag is not read by merges or batch rebase.

### 6.4 Functional requirements

Submission
- **MRG-FR-001** Check order for `POST …/merges`: no credential → 401; PAT without `merges:write` → 403; body decode → 400; `target_branch` empty → 400; `source_ref` empty → 400; invalid `target_branch` name → 400; invalid `source_ref` name → 400; workspace access (need write) → 404/403; workspace `status != ready` → 400; target does not resolve → 400; source does not resolve → 400; enqueue failure → 500; duplicate → 409; success → 202. The body is therefore validated before the workspace is looked up.
- **MRG-FR-002** Ref-name validation (applies to `target_branch`, `source_ref`, `target_ref`, every `branches[i]`), first failing rule reported as `invalid <field>: <rule>`: not empty (`ref name must not be empty`); not starting with `-` (`ref name must not start with '-'`); not `@`; no control chars (< 0x20 or 0x7f) (`ref name must not contain control characters`); none of space `~ ^ : ? * [ \` (`ref name must not contain '<c>'`, Go `%q` rune quoting e.g. `' '`); no `..`; no `@{`; no leading/trailing `/` or `//`; not ending in `.`; no path component starting with `.` or ending in `.lock`.
- **MRG-FR-003** Existence check: each name must be resolvable by `git rev-parse --verify` in the trunk (so any revision passes: local branch, tag, `origin/…` remote-tracking ref, full SHA). Not resolvable → `400 branch "<name>" does not exist in workspace` (target checked first). Failure to start git in the trunk → 500 `internal server error`.
- **MRG-FR-004** Dedup: if a merge job with the same key (`slug:target:source`) is `queued`, `running` or **`failed` (awaiting retry) [FIX-08]**, the submit is refused with `409`, `error_type: "duplicate_merge"`. Jobs in `dead_letter`, `completed` and `cancelled` do not count. In the baseline a `failed` job did not count either, so a second job could be created next to one waiting to retry. The 409 message keeps its baseline text (`…is already queued or running`) for compatibility, also when the blocking job is `failed`. Rebuild dedup follows the same rule (CP-FR-110).
- **MRG-FR-005** `submitted_by` = user id for API keys and PATs, `"admin"` for the admin token; a credential without user id → 500.
- **MRG-FR-006** On success: `hub.merge.enqueue` emitted (failure only logged), response `202` with the projected job (status `queued`, `retry_count 0`, SHAs null, `conflict_files []`, `error null`), a worker is woken immediately.

Serialization and execution
- **MRG-FR-010** A worker claims the oldest eligible job across all types (order: `available_at` ascending, then `created_at`), where eligible = `queued`, `available_at <= now`, and no `running` job of the same type with the same effective group (`group_key`, or `key` when `group_key` is empty). Hence at most one merge per (workspace, target branch) runs at a time; merges into different targets of the same workspace, and merges vs. rebuilds, may be claimed concurrently but then serialize on the workspace lock. FIFO within a group is not guaranteed when an older job is in `failed` backoff.
- **MRG-FR-011** The merge job holds the workspace lock (blocking acquire) for its whole run.
- **MRG-FR-012** Algorithm (all git operations in the trunk, via the git CLI ≥ 2.38, identity `parleyd <parleyd@localhost>` unless `GIT_AUTHOR_*`/`GIT_COMMITTER_*` are set):
  1. Parse payload; unparsable → permanent `{"reason":"invalid_payload"}`.
  2. Pre-check: resolve source head and target head (failure → permanent plain error). Equal → retryable `{"reason":"BranchNotReady"}`. Source is an ancestor of target → idempotent success: result `{base_sha: target head, merged_sha: target head}`; no fetch, no ref change, source branch NOT deleted, no `hub.merge.complete` event. Otherwise dry-run `git merge-tree --write-tree <target> <source>`; conflicts → permanent `{"reason":"WouldConflict","conflict_files":[…]}` (paths from `CONFLICT … Merge conflict in <path>` lines, de-duplicated); other merge-tree failure → permanent.
  3. Fetch `origin` with the workspace's origin credential (default refspec: updates remote-tracking refs only; local target branch is not moved by the fetch). Failure → retryable.
  4. `base_sha` = current local target head.
  5. Remember the currently checked-out branch. Clear leftover state (`rebase --abort`, `cherry-pick --abort`, `merge --abort`, `reset --hard HEAD`, best effort), check out the source, `git rebase <target>`. Conflict → rebase is aborted, permanent `WouldConflict` with files; other failure → permanent (a cancelled context aborts the rebase first).
  6. `merged_sha` = rebased source head.
  7. `git update-ref refs/heads/<target> <merged_sha>` (fast-forward by construction). Failure → retryable `{"reason":""}`.
  8. Check out the target, then delete `refs/heads/<source>`. Failures here are logged only (source kept); the job still succeeds.
  9. Validate both SHAs are 40 characters (else permanent), emit `hub.merge.complete`, return `{base_sha, merged_sha}`.
  10. Always (deferred, on success or failure after step 4): check out the remembered branch, or the target if none was checked out or it was the source; if that fails, detach HEAD.
- **MRG-FR-013** A merge never half-applies: the target ref changes only in step 7, after a successful rebase; on any earlier failure target and source are unchanged (the source may have been rebased only if step 7 or later failed — in that case a retry sees the rebased source).
- **MRG-FR-014** A merge only changes the hub's trunk: nothing is pushed to origin or any forge; `workspaces.head_sha` is not updated by the merge; no merge commit is ever created.
- **MRG-FR-015** On every handler error (each failed attempt, retryable or not) `hub.merge.fail` is emitted with `metadata.reason` = the error text.
- **MRG-FR-016** Individual git commands are bounded by 10 minutes; the fetch has no own timeout.

Read / cancel / requeue
- **MRG-FR-020** List returns the workspace's merge jobs (key prefix `<slug>:`), newest `created_at` first, at most 200, as a JSON array (`[]` when none). No pagination or filters.
- **MRG-FR-021** Get/cancel/requeue answer `404 merge job not found` when the id is unknown, the job's key does not start with `<slug>:`, or its type is not `merge`.
- **MRG-FR-022** Cancel: allowed only when status is `queued` (else `409 job cannot be cancelled in its current state`, including already `cancelled`, `failed`, `running`, `completed`, `dead_letter`); the transition is atomic (a concurrent claim → 409). Running jobs cannot be interrupted.
- **MRG-FR-023** Requeue: allowed only for `dead_letter` and when no other job with the same key is `queued`, `running` or `failed` **[FIX-08]**; sets `queued`, `retry_count 0`, `available_at now`, wakes a worker; `error`/`result` are kept. Refusals are `409` with the queue's message.
- **MRG-FR-024** Cancel and requeue need neither a ready workspace nor the lock, and emit no event.

Batch rebase
- **MRG-FR-030** Check order: 401; PAT without `merges:write` → 403; body decode → 400; `target_ref` empty → 400; `branches` missing/empty → 400; `target_ref` invalid → 400; first invalid `branches[i]` → 400; access (need write) → 404/403; workspace not `ready` → 400; not configured → 500; workspace lock busy (non-blocking try) → `409 workspace_busy`; run; failure → 500.
- **MRG-FR-031** Execution, synchronous in the request, holding the workspace lock: remember the checked-out branch (if HEAD is a branch); for each branch in order: resolve it (failure → `status: "error"`), `git checkout <branch> --` (failure → `error`), `git rebase <target_ref>` (conflict → aborted, `status: "conflict"` + `conflict_files`; other failure → `error`; request cancellation aborts the rebase), success → `status: "ok"`, `new_head`. Processing continues after conflicts/errors. Finally re-checkout the remembered branch.
- **MRG-FR-032** Batch rebase rewrites the listed local branch refs in place; it does not fetch, does not touch `target_ref`, does not push, does not clear leftover state first, does not update `head_sha`, enqueues nothing and emits no event. Duplicate branch names are processed twice; there is no maximum count.

### 6.5 REST API

Common for all routes below: under `/api/v1`, apikit authentication (Bearer API key, PAT, admin token). A PAT bound to another workspace is answered `404 workspace not found` by the binding guard before the handler. Access refusals (shared rule): unknown workspace / stranger → `404 workspace not found`; member on a write route → `403 org members have read-only access to this workspace` with `error_type: "forbidden"`; lookup failure → `500 internal server error`. Errors use `{"error":{"code","message"[,"error_type"]}}`. Scope refusals carry no `error_type`.

#### `POST /api/v1/workspaces/:slug/merges`
- **Purpose:** enqueue a merge of `source_ref` into `target_branch`.
- **Auth / scope:** any credential; PAT needs `merges:write`; write access (owner, admin token).
- **Path/query params:** `slug` (workspace slug).
- **Request body:** `{"target_branch": string, required, ref-name rules; "source_ref": string, required, ref-name rules}`. Unknown fields ignored.
- **Behaviour:** MRG-FR-001…006.
- **Response:** `202` merge job response (status `queued`).
- **Errors:** `401 authentication required`; `403 PAT requires merges:write scope`; `400 invalid request body: <decoder error>` (e.g. `unexpected EOF`, `EOF` for empty body); `400 target_branch is required`; `400 source_ref is required`; `400 invalid target_branch: <rule>`; `400 invalid source_ref: <rule>`; 404/403/500 access; `400 workspace is not ready`; `500 internal server error` (branch check); `400 branch "<name>" does not exist in workspace`; `500 internal server error` (submitted_by); `500 failed to enqueue merge job`; `409 a merge job for this source and target branch is already queued or running` (`error_type: duplicate_merge`); `500 internal server error` (re-read).
- **Side effects:** job row inserted; `hub.merge.enqueue`.

#### `GET /api/v1/workspaces/:slug/merges`
- **Purpose:** list the workspace's merge jobs.
- **Auth / scope:** PAT needs `merges:read` or `merges:write`; read access (members allowed).
- **Path/query params:** `slug`. No query parameters.
- **Request body:** none.
- **Behaviour:** MRG-FR-020.
- **Response:** `200` JSON array of merge job responses (≤200, newest first; `[]` when none).
- **Errors:** `401 authentication required`; `403 PAT requires merges:read scope`; access 404/500; `500 internal server error`.
- **Side effects:** none.

#### `GET /api/v1/workspaces/:slug/merges/:id`
- **Purpose:** read one merge job.
- **Auth / scope:** as list.
- **Path/query params:** `slug`, `id` (job UUID).
- **Response:** `200` merge job response.
- **Errors:** 401; `403 PAT requires merges:read scope`; access 404/500; `404 merge job not found`.
- **Side effects:** none.

#### `DELETE /api/v1/workspaces/:slug/merges/:id`
- **Purpose:** cancel a queued merge job.
- **Auth / scope:** PAT needs `merges:write`; write access.
- **Path/query params:** `slug`, `id`.
- **Behaviour:** MRG-FR-021/022.
- **Response:** `200 {"status":"cancelled"}`.
- **Errors:** 401; `403 PAT requires merges:write scope`; access 404/403/500; `404 merge job not found`; `409 job cannot be cancelled in its current state`; `500 failed to cancel job`.
- **Side effects:** job → `cancelled`.

#### `POST /api/v1/workspaces/:slug/merges/:id/requeue`
- **Purpose:** put a dead-lettered merge job back in the queue.
- **Auth / scope:** PAT needs `merges:write`; write access.
- **Path/query params:** `slug`, `id`. Body ignored.
- **Behaviour:** MRG-FR-021/023.
- **Response:** `200` merge job response (status `queued`, `retry_count 0`, previous `error` still present).
- **Errors:** 401; `403 PAT requires merges:write scope`; access 404/403/500; `404 merge job not found`; `409 jobqueue: job "<id>" is not in dead_letter status (current: "<status>")`; `409 jobqueue: active job "<other id>" exists for (merge, <key>)`; `409 jobqueue: job "<id>" was concurrently modified`; `500 failed to retrieve requeued job`.
- **Side effects:** job → `queued`; worker woken.

#### `POST /api/v1/workspaces/:slug/rebase`
- **Purpose:** synchronously rebase several branches onto a ref.
- **Auth / scope:** PAT needs `merges:write`; write access.
- **Path/query params:** `slug`.
- **Request body:** `{"target_ref": string, required, ref-name rules; "branches": string[], required, non-empty, each ref-name rules}`.
- **Behaviour:** MRG-FR-030…032.
- **Response:** `200 {"results":[{"branch":"feature/a","status":"ok","new_head":"<40hex>"},{"branch":"feature/b","status":"conflict","conflict_files":["x"]},{"branch":"feature/c","status":"error"}]}` — one entry per requested branch, in request order.
- **Errors:** `401 authentication required`; `403 PAT requires merges:write scope`; `400 invalid request body: <err>`; `400 target_ref is required`; `400 branches list must not be empty`; `400 invalid target_ref: <rule>`; `400 invalid branches[<i>]: <rule>`; access 404/403/500; `400 workspace is not ready`; `500 batch rebase not configured`; `409 another operation is running on this workspace; retry later` (`error_type: workspace_busy`); `500 batch rebase failed: <err>` (e.g. trunk missing / git too old).
- **Side effects:** listed branch refs rewritten in the trunk.

### 6.6 CLI

Common: `<workspace-slug>` and `<merge-id>` are percent-encoded as single path segments. Successful JSON responses are printed to stdout indented with two spaces. Errors are printed to stdout as `{"error":{"code":<n>,"message":"…"}}` (the hub's `error_type` is not carried over). Exit codes: 0 success; 1 hub answered ≥400 (code = HTTP status) or `--wait` failure/timeout; 2 usage/client errors (missing flag, wrong argument count, no credentials, connection failure, request timeout). Non-wait requests have a 30 s client timeout.

#### `parley merge submit <workspace-slug>`
- **Purpose:** enqueue a merge job.
- **Arguments / flags:** `--target <string>` (required), `--source <string>` (required), `--wait` (bool, default false), `--timeout <duration>` (default `5m0s`), `--poll-interval <duration>` (default `5s`). Exactly one positional argument.
- **Calls:** `POST /api/v1/workspaces/:slug/merges` with `{"target_branch": --target, "source_ref": --source}`; with `--wait`, then `GET …/merges/:id` repeatedly.
- **Output:** without `--wait`: the 202 job record (indented). With `--wait`: the submit response on one line (compact JSON), then the final job record (indented) once a terminal status is seen.
- **Exit codes:** missing `--target` → `{"error":{"code":2,"message":"--target flag is required"}}`, exit 2 (checked before `--source`); missing `--source` → `--source flag is required`, exit 2; nothing sent in either case. Hub error → exit 1. `--wait`: exit 0 only for `completed`; for `dead_letter` and `cancelled` prints the record (**[FIX-07]** `failed` is not final, so the poll goes on), stderr `Job ended with status <s>.`, stdout envelope `{"error":{"code":1,"message":"job ended with status <s>"}}`, exit 1; timeout → stderr `Timed out waiting for job to complete after <timeout>`, stdout envelope `{"error":{"code":1,"message":"timed out after <timeout>"}}`, exit 1. A missing `id` in the submit response → envelope code 2, exit 2.
- **Notes:** the first poll is immediate, then every `--poll-interval`; the whole wait (all polls) is bounded by `--timeout`. **[FIX-07]** `failed` means the hub will retry the job, so `--wait` keeps polling until `completed`, `dead_letter` or `cancelled`, or until the timeout. In the baseline it stopped at the first retryable failure and exited 1.

#### `parley merge list <workspace-slug>`
- **Purpose:** list merge jobs. **Arguments:** exactly one. **Calls:** `GET /api/v1/workspaces/:slug/merges`. **Output:** JSON array. **Exit codes:** 0 / 1 / 2.

#### `parley merge status <workspace-slug> <merge-id>`
- **Purpose:** show one job. **Arguments:** exactly two. **Calls:** `GET /api/v1/workspaces/:slug/merges/:id`. **Output:** job record. **Exit codes:** 0 / 1 / 2.

#### `parley merge cancel <workspace-slug> <merge-id>`
- **Purpose:** cancel a queued job. **Arguments:** exactly two. **Calls:** `DELETE /api/v1/workspaces/:slug/merges/:id`.
- **Output:** the hub's body is ignored; stdout `{"status":"cancelled","merge_id":"<id>"}` (indented), stderr `Merge job '<id>' has been cancelled.`
- **Exit codes:** 0; 1 on hub refusal (e.g. 409); 2 usage/client.

#### `parley merge requeue <workspace-slug> <merge-id>`
- **Purpose:** requeue a dead-lettered job. **Arguments:** exactly two. **Calls:** `POST /api/v1/workspaces/:slug/merges/:id/requeue` (no body). **Output:** job record. **Exit codes:** 0 / 1 / 2.

#### `parley rebase submit <workspace-slug>`
- **Purpose:** synchronous batch rebase.
- **Arguments / flags:** `--target-ref <string>` (required), `--branches <string>` (required; split on `,` without trimming — empty elements are sent as `""` and refused by the hub). Exactly one positional argument. No `--wait` (the call itself is synchronous).
- **Calls:** `POST /api/v1/workspaces/:slug/rebase` with `{"target_ref", "branches": [...]}`.
- **Output:** the hub's `{"results":[…]}` (indented).
- **Exit codes:** missing `--target-ref` → `--target-ref flag is required` (exit 2, checked first); missing `--branches` → `--branches flag is required` (exit 2); hub error → 1; client timeout (30 s) or connection error → 2.

### 6.7 Background behaviour
- **Queue instance:** one durable queue for the hub (merge and rebuild job types), 4 worker goroutines, 5 s poll interval, woken immediately on enqueue/requeue (one wake-up token), 30 s shutdown grace period. Not configurable.
- **Startup:** schema creation and migrations (idempotent) → handlers registered → stale rebuild worktrees cleaned → crash recovery: every `running` job reset to `queued` (`available_at = now`, `retry_count` unchanged, logged) → workers start. A merge recovered mid-flight is re-run from the start; the algorithm is idempotent (leftover rebase state cleared; a target already fast-forwarded yields AlreadyMerged success).
- **Worker loop:** promote due `failed` jobs to `queued`; claim one eligible job (MRG-FR-010) with an atomic `queued→running` update; run the handler with a non-cancellable context (cancelled only at shutdown after the grace period); finalize (state machine 6.3). Panics are caught and dead-letter the job.
- **Shutdown:** stop claiming; wait up to 30 s for in-flight handlers; then cancel their contexts, wait 500 ms more and return. Jobs whose finalization did not happen stay `running` and are recovered at next start.
- **Retention:** none; job rows accumulate indefinitely (list shows the newest 200).

### 6.8 Events

| Event type | When | Fields |
|---|---|---|
| `hub.merge.enqueue` | Successful submit | `resource_type: "merge"`, `resource_id: <job id>`, `action: "enqueue"`, `workspace: <slug>`, `actor_id: <user id>` (`""` for admin), `actor_type: api_key|pat|admin_token`; metadata `{target_branch, source_ref, job_id}`. |
| `hub.merge.complete` | Merge succeeded through the fast-forward path (not emitted for AlreadyMerged) | `resource_type: "merge"`, `actor_type: "system"`, `workspace: <slug>`, `resource_id: ""`, `action: ""`, `actor_id: ""`; metadata `{base_sha, merged_sha}`. |
| `hub.merge.fail` | Every failed attempt (retryable or permanent) | as complete; metadata `{reason: <error text>}` (JSON string for structured errors, e.g. `{"reason":"WouldConflict","conflict_files":["a.txt"]}`). |

Cancel, requeue and batch rebase emit no events. Emission failures are logged and never change the outcome.

### 6.9 Configuration

| Key / env | Type | Default | Effect |
|---|---|---|---|
| `[workspace].path` | string | `./data/workspaces` | Trunk location `<path>/<slug>/trunk`. |
| `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME`, `GIT_COMMITTER_EMAIL` (hub process env) | string | `parleyd` / `parleyd@localhost` | Identity used for rebased commits (committer; authors preserved by rebase). |
| `git` ≥ 2.38 on `PATH` | binary | — | Required (merge-tree `--write-tree`); older git fails merge jobs / batch rebase. |
| Workspace secrets (origin credential, e.g. `GIT_PAT` or `GIT_USERNAME`+`GIT_PASSWORD`) | secret | — | Used for the merge's `origin` fetch. |

No queue configuration keys: workers 4, poll 5 s, grace 30 s, retry policy (2 s ×2, cap 2 h, 20 retries), list limit 200, per-git-command timeout 10 min are fixed.

### 6.10 Edge cases and error handling
- `BranchNotReady` (source == target head) is retried with backoff up to 20 times before dead-lettering.
- An unreachable origin (fetch failure) makes every merge retry and eventually dead-letter, even though the merge itself is local.
- A source that is a tag or raw SHA passes submission; the rebase then runs on a detached HEAD, `merged_sha` equals the unchanged SHA, the target ref is set to it, and the source deletion fails (logged).
- A target like `origin/main` (resolvable remote-tracking ref) passes submission; step 7 writes `refs/heads/origin/main`.
- **[FIX-08]** While a job for key K is `failed` (awaiting retry), a new submission for K is refused with `409 duplicate_merge`, and requeue of a dead-lettered job for K is refused too. In the baseline both were accepted, giving two jobs for K.
- A `completed` job may still show the `error` of an earlier failed attempt; a requeued job keeps its last error until it fails again.
- Hub shutdown during a rebase cancels the handler context; the rebase is aborted and the job is dead-lettered if finalization happens, otherwise recovered as `queued` at next start.
- A merge into the checked-out branch updates the trunk's files (the target is checked out at the end), but `workspaces.head_sha` is not refreshed until the next push/sync.
- Batch rebase with a dirty trunk or a branch that does not exist yields `status: "error"` for that branch without detail; a client disconnect cancels the remaining work.
- Batch rebase on a large set may exceed the CLI's 30 s request timeout (exit 2) while the hub continues until the request context is cancelled.
- Unknown job ids, jobs of other workspaces and rebuild jobs are all `404 merge job not found` (no cross-workspace disclosure).
- **[FIX-07]** `parley merge submit --wait` keeps polling through `failed`. In the baseline it exited 1 there.

### 6.11 Discrepancies between docs and code
1. **Pre-check rejections all permanent.** `docs/architecture.md` ("A merge job", step 1): "Rejections are permanent." Code: `BranchNotReady` is retryable (`hub2/internal/merge/handler.go:151-154`, `Permanent: false`; mapped to retryable in `merge.go:220-224`); confirmed by `hub/internal/merge/job_test.go` ("not ready" retryable).
2. **Merge events.** `docs/api.md` (Merge Job Response Schema): "`hub.merge.complete` or `hub.merge.fail` when it ends." Code emits `hub.merge.fail` on every failed attempt including retryable ones (`merge.go:75-81`), and emits no `hub.merge.complete` for the AlreadyMerged idempotent success (`merge.go:99-115`). The merge complete/fail events carry no `resource_id` (job id) and no `action` (`handler.go:84-101`).
3. **Branch existence.** `docs/api.md` (POST merges): names "must exist in the workspace" as branches. Code accepts any revision `git rev-parse --verify` resolves (tags, SHAs, remote-tracking refs) (`hub2/internal/merge/wire.go:39-54`).
4. **`failed` as terminal.** `docs/cli.md` (Waiting for a job) and the code's `isTerminalStatus` (`hub2/internal/cli/wait.go:49-56`) treat `failed` as final; in the queue `failed` is the retry-pending state that is promoted back to `queued` (`aflib/jobqueue/jobqueue.go:524-570`, promotion in the worker loop). Docs and CLI agree; the behaviour (early exit 1) is what must be reproduced. → **Resolved in this PRD by FIX-07:** the re-implementation follows the corrected behaviour described here (see §1.8).
5. **Cancel response.** Code comment in `hub2/internal/cli/merge_cmd.go:173` says the server returns 204; the server returns `200 {"status":"cancelled"}` (`hub2/internal/merge/api.go:539`), as `docs/api.md` states. The CLI ignores the body either way.
6. **Cancel 409 list.** `docs/api.md` lists "running, completed, failed or cancelled"; code also refuses `dead_letter` (any status other than `queued`, `api.go:525-528`).
7. **Dedup scope.** `docs/architecture.md` ("The durable job queue") says a job "still queued or running" makes a duplicate — matches code; note that `failed` jobs are not counted, which docs do not mention (`aflib/jobqueue/jobqueue.go:345-357`). → **Resolved in this PRD by FIX-08:** the re-implementation follows the corrected (`failed` counts) behaviour described here (see §1.8).


---

## 7. Carry-patch forks

### 7.1 Purpose and scope

A `carry_patch` workspace maintains a fork of a repository the team does not control as an ordered stack of **patch branches** applied on top of an **upstream** repository. The hub keeps an **integration branch** (default `deploy`) that is mechanically rebuilt as "upstream base + every applicable patch, in position order"; it detects patches that upstream has merged (including squash merges), replays recorded conflict resolutions (git rerere), predicts conflicts (preview), and can roll the integration branch back. Either the hub or the fork (`origin`) can be the authority for patch branches (`patch_branch_source`).

In scope: the carry-patch fields of workspace creation and the post-clone setup; the patch list (8 routes); rebuilds (6 routes + preview) and the rebuild job; rerere management (2 routes); the patch-status dashboard; the carry-patch extension of `POST /workspaces/:slug/sync`; push control on the hub's git server (pre-receive), the post-push auto-rebuild and hub-mode mirroring; reset-to-origin with backup refs; startup cleanup; the hardened git runner (`gitcmd`) whose guarantees are externally observable; the `parley patch|rebuild|rerere` commands and `parley workspace patch-status`.
Out of scope (covered by other sections, referenced here): general workspace CRUD/sync/clone (§4), secrets storage (§8), the durable job queue mechanics (§6), audit storage/SSE (§11), git smart-HTTP auth (§5).

### 7.2 User stories

**US-CP-1 — Create a fork workspace.** As a Developer, I want to create a workspace with `workspace_mode=carry_patch`, an `upstream_url` and an optional `integration_branch`, so that the hub tracks both my fork and the upstream.
- `upstream_url` is required in carry_patch mode and forbidden in standard mode; `integration_branch` defaults to `deploy`.
- After the clone, the hub's clone has an `upstream` remote, rerere enabled, and the integration branch created at the upstream default branch (or at the fork's HEAD if the upstream fetch failed).
- Setup failures never fail the clone; the workspace still becomes `ready`.

**US-CP-2 — Register patches.** As a Developer or Agent (PAT with `patches:write`), I want to register patch branches in order (singly or as an atomic batch), so that rebuilds apply them in that order.
- The branch must exist (local, origin tracking ref, or — in origin mode — on the fork) unless `skip_branch_check`.
- `if_not_exists` makes registration idempotent (200 with the existing record).
- The integration branch cannot be registered.

**US-CP-3 — Maintain the stack.** As a Developer, I want to reorder, move, disable, re-enable, mark merged, describe, remove and restore patches.
- Positions stay contiguous 1..N over non-deleted patches after every change.
- `deleted` cannot be set directly; soft-deleted patches are restored with `/restore`.

**US-CP-4 — Rebuild.** As a Developer or Agent (PAT `rebuilds:write`), I want to queue a rebuild with an optional strategy/fail-mode override and follow its progress, so that the integration branch reflects the latest upstream plus patches.
- Only one rebuild per workspace may be queued, running or waiting to retry (409 `concurrent_rebuild`) **[FIX-08]**.
- A running job exposes per-patch progress in `patch_results`.
- On `fail_fast` conflict the integration branch is unchanged and the job is dead-lettered (not retried).

**US-CP-5 — Preview.** As a Developer, I want a read-only prediction of which patches would conflict against the current upstream base.

**US-CP-6 — Roll back.** As an Operator/Developer, I want to move the integration branch back to the head it had before a given completed rebuild.

**US-CP-7 — Sync.** As a Developer (or a scheduler), I want `POST /sync` to fetch upstream (and the fork in origin mode), refresh patch branches, detect merged patches, and auto-queue a rebuild.
- Response carries `patches_merged`, `rebuild_triggered`, `rebuild_job_id`, `force_push_detected`, `origin_fetched` and, in origin mode, `patches_synced`/`patches_diverged`.

**US-CP-8 — Push control.** As an Agent pushing to the hub's git server, I want pushes to registered patch branches to be accepted (hub mode), rejected with a pointer to the fork, or forwarded to the fork (origin mode), and to trigger a rebuild automatically.

**US-CP-9 — Recover replaced tips.** As a Developer, I want a replaced patch tip to be kept under `refs/hub/replaced/<branch>` (fetchable, not pushable) and to be able to force a patch branch to the fork's tip (`reset-to-origin`).

**US-CP-10 — Rerere management.** As a Developer, I want to list recorded rerere entries and forget a wrong one (by id or path).

**US-CP-11 — Dashboard.** As an Org member (read access), I want one call summarising workspace state, the latest rebuild, each patch with its latest result, and counts.

### 7.3 Concepts and data model

#### 7.3.1 Carry-patch workspace fields (workspaces table)

| Field | Type | Meaning / constraints |
|---|---|---|
| `workspace_mode` | text | `standard` (default) or `carry_patch`; immutable after create |
| `upstream_url` | text, nullable | Required for carry_patch (same validation as `git_url`: `https://<host>/<path>` or `git@<host>:<path>`, no `http://`); must be absent for standard; immutable |
| `integration_branch` | text, nullable | carry_patch only; default `deploy` when omitted or `""`; validated with the workspace branch-name rules; immutable |
| `upstream_head_sha` | text, nullable | Upstream base recorded by the last carry-patch sync that saw upstream advance |
| `last_sync_at` | text RFC3339, nullable | Written by every completed carry-patch sync |
| `branch` | text, nullable | The workspace branch; used only as a fallback for the upstream base (`refs/remotes/upstream/<branch>`) |
| `status` | enum | `pending`, `cloning`, `ready`, `failed`, `archived` |
| `settings` | JSON | Behaviour switches (see 7.9) |

#### 7.3.2 On-disk layout and git refs (observable through the git server and behaviour)

| Item | Meaning |
|---|---|
| `<workspace.path>/<slug>/trunk/` | The hub's clone ("trunk"). Remote `origin` = `git_url` (fork); remote `upstream` = `upstream_url` |
| `<workspace.path>/<slug>/rebuild/<job id>/` | Detached linked worktree of one rebuild run (random 16-hex id if no job id). Removed at the end of every run |
| `refs/remotes/upstream/*` | All upstream branches (`+refs/heads/*:refs/remotes/upstream/*`, no tags) |
| `refs/remotes/upstream/HEAD` | The commit the upstream's HEAD (default branch) pointed at on the last fetch (`+HEAD:refs/remotes/upstream/HEAD`); **the upstream base** |
| `refs/remotes/origin/*` | Fork branches (origin-mode sync fetch: `+refs/heads/*:refs/remotes/origin/*`, prune, no tags; single-branch fetches `+refs/heads/<b>:refs/remotes/origin/<b>`) |
| `refs/heads/<integration_branch>` | Integration branch, written only by rebuild (update-ref) and rollback (`branch -f`) |
| `refs/heads/<patch branch>` | The hub's copy of each patch branch (written by git pushes, branch resolution, origin-mode sync refresh, reset-to-origin) |
| `refs/hub/replaced/<branch>` | Backup of a patch tip discarded by a replace (sync) or reset-to-origin. One per branch (overwritten). Protected: no client push may create/update/delete it; fetchable |
| `refs/hub/forward/<branch>` | Transient ref used while forwarding a push to the fork; removed after |
| `.git/rr-cache/<id>/` | rerere cache shared by trunk and all worktrees; `rerere.enabled=true`, `rerere.autoupdate=true` in repo config |

**Upstream base resolution** (used by rebuild, sync, preview, stale check), first that resolves to a commit: (1) `refs/remotes/upstream/HEAD`; (2) `refs/remotes/upstream/<workspace.branch>` when `branch` is set; (3) `FETCH_HEAD`. None → error.

#### 7.3.3 Patch entity (table `patches`)

| Column / JSON field | Type | Default | Constraints / meaning |
|---|---|---|---|
| `id` | text UUID (v4) | generated | PK |
| `workspace_slug` | text | — | JSON field `workspace_slug` |
| `branch_name` | text | — | Required; UNIQUE(workspace_slug, branch_name) — including soft-deleted rows; immutable (PATCH ignores it) |
| `position` | int | append | UNIQUE(workspace_slug, position). Non-deleted patches hold contiguous 1..N; soft-deleted rows are parked at `-rowid` (negative, visible in `GET .../patches/:id` of a deleted patch) |
| `status` | text | `active` | `active`, `conflict`, `disabled`, `merged_upstream`, `deleted` |
| `conflict_files` | JSON text array, nullable | null | Set by a rebuild conflict; `"[]"` written when status is set by the store with no files; response field present only when non-empty |
| `upstream_pr_url` | text, nullable | null | Free text (not validated); used for PR-number squash detection |
| `description` | text, nullable | null | Free text (no length limit) |
| `deleted_at` | text RFC3339, nullable | null | Set on soft delete, cleared on restore |
| `added_at`, `updated_at` | text RFC3339 UTC | now | |
| `origin_sync_state` | text, nullable | null | `in_sync`, `diverged`, `missing_on_origin` (origin mode only) |
| `origin_sha` | text, nullable | null | Fork tip at last origin-mode sync/reset; null for `missing_on_origin` |
| `origin_synced_at` | text, nullable | null | Time of that comparison |

**Patch response object** (all patch routes): `id`, `workspace_slug`, `branch_name`, `position`, `status`, `added_at`, `updated_at` always; `upstream_pr_url`, `description`, `deleted_at` always (JSON `null` when unset); `conflict_files` only when non-empty; `origin_sync_state`, `origin_sha`, `origin_synced_at` only when non-null. Some routes add `replaced_sha`, `rebuild_triggered`, `rebuild_job_id`.

**Patch status state machine**

| From | To | Trigger |
|---|---|---|
| (new) | `active` | add |
| any non-deleted | `active`/`merged_upstream`/`conflict`/`disabled` | `PATCH .../patches/:id` with `status` (not allowed when current is `deleted`: 409) |
| `active`, `conflict` (attempted in a rebuild) | `conflict` | unresolved rebuild conflict (both fail modes) |
| `active` | `merged_upstream` | sync merge detection (only `active` patches are checked; monotonic — never reverted by sync) |
| `merged_upstream` | `deleted` (soft delete: `deleted_at`=now, position=-rowid) | a rebuild that reaches the success path (incl. `continue` mode) and lists the patch as skipped/merged_upstream |
| `deleted` | `active` (position = max+1, `deleted_at`=null) | `POST .../restore` |
| any non-deleted | `deleted` (soft delete: `deleted_at`=now, position=-rowid) | `DELETE .../patches/:id` **[FIX-09]** (the baseline removed the row) |
| `conflict` (applied cleanly) | `active`, `conflict_files` cleared | a rebuild that reaches the success path **[FIX-10]** |
| `deleted` for 7 days | (row removed, backup ref removed) | hourly patch purge (CP-FR-231) **[FIX-12]** |

Notes: **[FIX-10]** A `conflict` patch is attempted by every rebuild. When it applies cleanly and the rebuild reaches the success path, it returns to `active` and `conflict_files` is cleared (CP-FR-127). `conflict_files` is also cleared by a `PATCH` that sets any status other than `conflict` (CP-FR-061), and by a restore (CP-FR-080). In the baseline, nothing moved `conflict` back to `active`, and `conflict_files` was never cleared.

**Origin sync state** (per patch, origin mode): `in_sync` (hub tip equals fork tip after the refresh/reset), `diverged` (policy `report` left a diverged branch), `missing_on_origin` (fork lacks the branch; patch status unchanged).

#### 7.3.4 Rebuild job

Rebuilds are durable jobs of type `rebuild` in the shared job queue. `key` = workspace slug (dedup: at most one `queued`/`running`/`failed` rebuild per slug **[FIX-08]**; in the baseline, `failed` did not count), `group_key` = `<slug>:<integration_branch>` (serialization; `"<slug>:"` if the column is null), `nonce` = fresh UUID per enqueue. Retry policy = queue defaults (base 2s, ×2, cap 2h, max 20 retries, then `dead_letter`); a non-retryable error dead-letters immediately.

**Payload** (`payload` JSON, captured at enqueue):

| Field | Type | Meaning |
|---|---|---|
| `workspace_slug` | string | |
| `strategy` | string | `rebase` or `merge` (always present) |
| `submitted_by` | string | user id, `system:push-hook`, or `system:stale-snapshot` |
| `integration_branch` | string, omitempty | from the workspace column; executor/rollback default `deploy` when empty |
| `fail_mode` | string, omitempty | `fail_fast` or `continue`; executor default `fail_fast` when empty |

**Result** (stored on completion):

| Field | Type | Notes |
|---|---|---|
| `upstream_head_sha` | string | base the run started from |
| `integration_head_sha` | string | new integration tip |
| `previous_integration_head_sha` | string, omitempty | tip before the update (absent on the first rebuild) |
| `strategy` | string | |
| `fail_mode` | string, omitempty | effective fail mode |
| `patches_applied`, `patches_skipped`, `patches_conflicted`, `patches_removed` | int | removed = merged_upstream patches soft-deleted |
| `patch_results` | PatchResult[] | in position order, every listed patch |
| `integration_branch_pushed` | bool, omitempty | push to origin succeeded |

**PatchResult**: `patch_id`, `branch_name`, `position`, `status` (`success`\|`conflict`\|`skipped`), `skipped_reason` (omitempty: `merged_upstream`\|`disabled`\|`deleted`\|`branch_not_found`), `new_head_sha` (string or **null**, always present), `conflict_files` (omitempty), `source_sha` (omitempty; snapshot of `refs/heads/<branch>^{commit}` used for the run).

**Rebuild job record** (API rendering of a job, list/detail/requeue):

| Field | Type | Rule |
|---|---|---|
| `id` | string | |
| `status` | string | `queued`, `running`, `completed`, `failed` (retry pending), `dead_letter`, `cancelled` |
| `strategy` | string, omitempty | from payload |
| `error` | string, omitempty | job error text |
| `created_at` | string RFC3339 UTC | |
| `completed_at` | string or null | `updated_at` when status is `completed`, `failed` or `dead_letter`; **null** for `queued`, `running`, `cancelled` |
| `patch_results` | array, omitempty | from the result when it has ≥1 entry; for a `running` job without result, the latest progress array |
| `integration_head_sha` | string, omitempty | from result |
| `previous_integration_head_sha` | string, omitempty | from result |

#### 7.3.5 Locks and guards (process-local, per slug)

- **Workspace lock**: one mutex per slug shared with sync, archive, reclone, merge, branch push, etc. API routes use *try-lock* (fail fast `409 workspace_busy`, message `another operation is running on this workspace; retry later`); the rebuild job uses a *blocking* lock.
- **Rebuild-active guard**: set for the whole rebuild run; while set, archive and reclone answer `409 workspace_busy`. A second guard attempt fails (rebuild retried).

### 7.4 Functional requirements

**Workspace creation and setup**
- CP-FR-001: `POST /workspaces` accepts `workspace_mode` (`standard`\|`carry_patch`; null/`""` → `standard`; other → 400 `invalid workspace_mode value: must be 'standard' or 'carry_patch'`).
- CP-FR-002: In carry_patch mode, missing/empty `upstream_url` → 400 `upstream_url is required when workspace_mode is 'carry_patch'`; invalid → 400 `invalid upstream_url: <git_url rule message>`; `integration_branch` default `deploy`, invalid → 400 `invalid integration_branch: <reason>`. In standard mode, presence (even null-valued key that decodes non-nil) of `upstream_url` → 400 `upstream_url is not allowed when workspace_mode is 'standard'`; of `integration_branch` → 400 `integration_branch is not allowed when workspace_mode is 'standard'`. The three fields are immutable on PATCH.
- CP-FR-003: Upstream credentials are not part of the create request; they are workspace secrets `UPSTREAM_GIT_PAT` (BasicAuth user `x-token-auth`), else `UPSTREAM_GIT_USERNAME`+`UPSTREAM_GIT_PASSWORD`, else the origin credentials (`GIT_PAT` / `GIT_USERNAME`+`GIT_PASSWORD`), else anonymous. A username without password at either level falls through to the next level. Store errors (not "not found") are errors.
- CP-FR-004: After a successful clone of a carry_patch workspace, before marking it `ready`: add remote `upstream` = `upstream_url`; fetch upstream (refspecs of 7.3.2) with upstream credentials; set `rerere.enabled=true` and `rerere.autoupdate=true`; create the integration branch at `refs/remotes/upstream/HEAD` if it resolves, else at `HEAD`. Every failure is logged as a warning and does not prevent `ready`.

**Access and scopes (all module routes)**
- CP-FR-010: Order of checks on every route: credential present (401 `authentication required`) → PAT scope (403) → workspace access via the hub rule (404 `workspace not found` for unknown slug / no access / PAT bound to another workspace; 403 `org members have read-only access to this workspace` with `error_type: forbidden` for a member on a write route; 500 on DB error) → route-specific checks. Only PATs are scope-checked; API keys and admin tokens pass. A `*:write` scope satisfies the matching read scope.
- CP-FR-011: Scope table — patch read routes: `patches:read`\|`patches:write` (msg `PAT requires patches:read scope`); patch write routes: `patches:write` (`PAT requires patches:write scope`); rebuild list/get/preview: `rebuilds:read`\|`rebuilds:write` (`missing required scope: rebuilds:read`); rebuild submit/cancel/requeue/rollback: `rebuilds:write` (`missing required scope: rebuilds:write`); patch-status and rerere list: `workspaces:read`\|`workspaces:write` (`missing required scope: workspaces:read`); rerere forget: `workspaces:write` (`missing required scope: workspaces:write`). Workspace need: GET routes need read; all other routes need write (owner or admin token).
- CP-FR-012: The scopes `rebuilds:read`, `rebuilds:write`, `patches:read`, `patches:write` are registered so PATs can be issued with them.

**Branch-name validation (patch add)**
- CP-FR-020: `branch_name` must be non-empty and must not: contain ASCII control chars (<0x20, 0x7f), space, `~ ^ : ? * [ \`; contain `..`; end with `.lock`; end with `.`; have any `/`-component starting with `.`; start with `-`; equal `@`; contain `@{`; start or end with `/` or contain `//`; have any component ending with `.lock`. Error message `invalid branch_name: branch must not ...` (one reason per rule, checked in that order).

**Branch resolution (patch add, unless `skip_branch_check: true`)**
- CP-FR-030: Mode = `PATCH_BRANCH_SOURCE` read **once per request** (shared by all batch elements); only exact `origin` selects origin mode, anything else is hub.
- CP-FR-031: Step 1 (no lock): `refs/heads/<name>^{commit}` resolves → method `local`.
- CP-FR-032: Otherwise take the workspace try-lock (busy → 409 `workspace_busy`), re-check step 1 (→ `local`), then step 2: `refs/remotes/origin/<name>^{commit}` resolves → create `refs/heads/<name>` at it with compare-and-swap against the all-zero SHA → `origin_tracking` (if the CAS fails but the ref now exists → `local`; other failure → 500 `internal server error`).
- CP-FR-033: Step 3 (origin mode only): resolve origin credentials (failure → 502 `failed to resolve origin credentials`); single-branch fetch of `<name>` from `origin` (no tags, no prune; already-up-to-date = success); fork lacks the branch → 400 `branch does not exist in repository or on origin`; any other fetch failure → 502 `origin fetch failed`, `error_type: origin_fetch_failed`; tracking ref still absent → 400 not-found; else create local branch by CAS → `origin_fetch`.
- CP-FR-034: Hub mode and not found in steps 1–2 → 400 `branch does not exist in repository or on origin` (no fork contact). Any unclassified resolver error (e.g. trunk missing) → 500 `internal server error`. In a batch the 400 message is prefixed `patch[<i>]: `; 409/502/500 messages are not prefixed.
- CP-FR-035: Branches created by resolution stay even if the request later fails (e.g. a later batch element fails).

**Patch add**
- CP-FR-040: Checks in order: auth/scope/access → workspace `archived` → 400 `workspace is archived` → mode not carry_patch → 400 `workspace is not in carry_patch mode` → body decode (first JSON value; error → 400 `invalid request body: <decoder error>`, an empty body gives `...: EOF`) → if first non-space char is `[` → batch path, else single path. Workspace status other than archived (pending/cloning/failed) is not refused.
- CP-FR-041 (single): `branch_name` empty → 400 `branch_name is required`; invalid → 400 per CP-FR-020; equal to the workspace integration branch → 400 `branch_name cannot be the integration branch`; branch resolution (CP-FR-030..034); then existing row with the same branch whose status is not `deleted`: with `if_not_exists: true` → **200** with the existing record (no event), else 409 `branch already exists in patch list`. **[FIX-09]** A row for the same branch whose status *is* `deleted` does not count as existing. The insert transaction removes that row, then inserts the new patch as usual (201, `hub.patch.create`), and `refs/hub/replaced/<branch>` is removed best effort after the commit. So remove followed by add behaves as it did in the baseline. In the baseline, a `deleted` row counted, which only occurred for patches a rebuild had soft-deleted; then `position` < 1 → 400 `position must be >= 1` (note: checked after resolution and the duplicate check).
- CP-FR-042 (single insert, one transaction): `position` omitted, 0 (only reachable when omitted), or > max+1 → appended at max+1 (max over all rows of the workspace; soft-deleted rows are negative); otherwise rows at `>= position` shift +1 and the new row takes `position`. New row: status `active`, `added_at`=`updated_at`=now. Response 201 with the patch. Emits `hub.patch.create`.
- CP-FR-043 (batch): empty array → 400 `batch request must contain at least one patch`. Pass 1 (array order, per element): `patch[i]: branch_name is required`, `patch[i]: invalid branch_name: <reason>`, `patch[i]: branch_name cannot be the integration branch`, `patch[i]: position must be >= 1` (all 400). Pass 2: resolve every element not skipping the check, in array order (errors per CP-FR-034). Pass 3: duplicate within batch → 409 `patch[i]: duplicate branch_name "<b>" in batch`. Pass 4: already registered with a status other than `deleted` → 409 `patch[i]: branch "<b>" already exists in patch list`. **[FIX-09]** `deleted` rows for a batch branch are removed in the insert transaction, as in CP-FR-041. `if_not_exists` is ignored in a batch.
- CP-FR-044 (batch insert, one transaction, all-or-nothing): start from existing non-deleted order; process elements with explicit `position` p ≤ len(order)+1 in batch order, inserting each at index p-1 of the evolving order; then append all others (and those with p > len+1) in batch order; renumber everything 1..N. Response 201 with the inserted patches **in request order** (each with its final position). No audit events. Insert failure → 500 `failed to add patches: <err>`.

**Patch read/list**
- CP-FR-050: List returns non-deleted patches of the workspace ordered by position; a standard workspace returns `[]` (200). Never null.
- CP-FR-051: Get returns any patch of the workspace by id including `deleted` (404 `patch not found` otherwise; no mode check). Adds `replaced_sha` (40-hex) when `refs/hub/replaced/<branch>` exists in the trunk; a missing trunk/ref omits it; a lookup failure is logged and omits it.

**Patch update (`PATCH`)**
- CP-FR-060: Order: auth/scope/access → patch lookup (404 `patch not found`) → body decode (400 `invalid request body: <err>`; an empty body is an error, `{}` is a no-op that still bumps `updated_at`) → `status` (if given) must be `active`\|`merged_upstream`\|`conflict`\|`disabled`, else 400 `invalid status value; must be one of: active, merged_upstream, conflict, disabled` (`deleted` included) → current status `deleted` with a status given → 409 `patch is soft-deleted; restore it first` → `position` (if given) must be 1..count(non-deleted), else 400 `position out of range`.
- CP-FR-061: `description` / `upstream_pr_url` set verbatim (`""` stores empty string; JSON null means "not given"). `branch_name` and unknown fields ignored. **[FIX-10]** A `status` other than `conflict` also clears `conflict_files`. Position change: remove the patch from the non-deleted order, insert at index position-1, renumber 1..N in one transaction. No mode/archived check; no audit event; `conflict_files` not touched. Response 200 with the re-read patch.

**Patch delete**
- CP-FR-070: **[FIX-09]** Soft-deletes the patch: `status=deleted`, `deleted_at=now`, `updated_at=now`, position parked at `-rowid`. In the same transaction, the remaining non-deleted patches are renumbered 1..N. `conflict_files` and the origin fields are kept. Errors and outcomes:
  - missing → 404 `patch not found`;
  - a patch already `deleted` → 204, with no change and no event;
  - other failure → 500 `failed to delete patch`.

  Then it emits `hub.patch.delete`. The backup ref `refs/hub/replaced/<branch>` is **kept**, so a restore finds it; the purge removes it (CP-FR-231). Response 204. No mode or archived check. Patch branches are not deleted. The patch can be brought back with `POST …/restore` (CP-FR-080) until it is purged. In the baseline, DELETE hard-deleted the row and removed the backup ref, so a removed patch could not be restored.

**Patch restore**
- CP-FR-080: Order: access → `archived` → 400 `workspace is archived` → not carry_patch → 400 `workspace is not in carry_patch mode` → patch lookup (404 `patch not found`) → status ≠ `deleted` → 400 `patch is not in deleted status`. Sets `status=active`, `deleted_at=NULL`, `conflict_files=NULL` **[FIX-10]**, `position = max(non-deleted positions)+1` (1 if none), `updated_at=now`. Does not clear the origin fields. **[FIX-09]** This works for patches removed with DELETE as well as for patches a rebuild soft-deleted as merged upstream. Response 200. No event.

**Patch reorder**
- CP-FR-090: Body `{"patch_ids": [...]}` (decode error → 400 `invalid request body: <err>`). Validation against the non-deleted set, in order: empty list while patches exist → 400 `patch_ids list is empty but workspace has N patches`; duplicate → 400 `duplicate patch ID "<id>" in reorder list`; foreign/unknown id → 400 `patch ID "<id>" does not belong to workspace "<slug>"`; count mismatch → 400 `patch_ids list has N entries but workspace has M patches`. Any DB failure is also reported as 400 with its error text. Success renumbers in list order (one transaction) and returns 200 with all non-deleted patches in order. No mode check (a standard workspace with `[]` → 200 `[]`). No event.

**Reset to origin**
- CP-FR-100: Order: access → `archived` (400 `workspace is archived`) → not carry_patch (400 `workspace is not in carry_patch mode`) → status ≠ `ready` (409 `workspace clone is not ready`) → reset service not configured (500 `patch reset is not configured`) → patch lookup (404) → branch equals integration branch (400 `patch branch is the integration branch`) → status not in {active, conflict, disabled} → 409 `patch status <status> cannot be reset`.
- CP-FR-101: Under the workspace try-lock (busy → 409 `workspace_busy`): resolve origin credentials (failure → 502 `failed to resolve origin credentials`); single-branch fetch from origin (fork lacks branch → 409 `branch does not exist on origin`, `error_type: missing_on_origin`; other → 502 `origin fetch failed`, `error_type: origin_fetch_failed`); then by first matching rule: no local branch → create at fork tip (CAS vs zero) = `created`; equal tips = `none` (nothing written); local tip is ancestor of fork tip → CAS move = `fast_forwarded`; otherwise force-write `refs/hub/replaced/<branch>` = local tip, then CAS move = `replaced`. A lost CAS (ref now differs from expected / exists for a create / vanished) → 409 `patch branch changed during reset; retry`, `error_type: ref_changed` (a backup already written stays). Other failures → 500 `failed to update patch branch <branch>`.
- CP-FR-102: If the moved branch is the trunk's checked-out branch, the trunk is `reset --hard HEAD`; a failure there is logged and the request still succeeds.
- CP-FR-103: Works regardless of `patch_branch_source` and `patch_divergence_policy`. In origin mode only, the patch's origin state becomes `in_sync`/`origin_sha`=fork tip/`origin_synced_at`=now.
- CP-FR-104: If the branch moved and the patch is `active`/`conflict` and `AUTO_REBUILD_AFTER_SYNC` ≠ `false`, enqueue a rebuild (same enqueue as sync, `submitted_by` = caller's user id); dedup suppresses silently.
- CP-FR-105: On success emit `hub.patch.reset`, plus `hub.patch.replace` (trigger `reset_to_origin`) for `replaced`, before re-reading the patch (re-read failure → 500 `failed to fetch updated patch`). Response 200: patch + `replaced_sha` (if the backup ref exists after the reset, even from an earlier replace) + `rebuild_triggered` (bool) + `rebuild_job_id` (only when enqueued).

**Rebuild submit**
- CP-FR-110: Order: auth/scope/access → workspace not carry_patch → 400 `rebuild is only supported for carry_patch workspaces`, `error_type: workspace_mode_mismatch` → status ≠ `ready` → 400 `workspace is not ready` → count of patches with status `active`/`conflict` = 0 → 400 `no patches with status active or conflict`, `error_type: no_active_patches` → body (parsed only when `Content-Length > 0`; bind error → 400 `invalid request body`; `strategy` not `rebase`/`merge` → 400 `strategy must be 'rebase' or 'merge'`; `fail_mode` not `fail_fast`/`continue` → 400 `fail_mode must be 'fail_fast' or 'continue'`; empty strings = not given) → enqueue → duplicate → 409 `a rebuild job is already queued or running for this workspace`, `error_type: concurrent_rebuild`. **[FIX-08]** This also happens when the workspace's rebuild is `failed` (waiting to retry); the message text is unchanged.
- CP-FR-111: Payload capture: strategy = body override, else setting `REBUILD_STRATEGY` if `rebase`/`merge`, else `rebase`; fail_mode = body override, else setting `REBUILD_FAIL_MODE` if valid, else omitted. (In production the settings reader always answers a value, so `fail_mode` is present.) `submitted_by` = caller user id. Response 202 `{id, type:"rebuild", key:<slug>, group_key:"<slug>:<ib>", status:"queued", payload:<payload object>}`. Emits `hub.rebuild.enqueue`.

**Rebuild execution** (job handler; returns result / retryable / error)
- CP-FR-120: Invalid payload JSON → non-retryable failure (dead letter), no event.
- CP-FR-121: Every other error exit emits `hub.rebuild.fail` (each attempt, including retryable ones and cancellations).
- CP-FR-122: Resolve upstream credentials (error → retryable). Take the **blocking** workspace lock, then the rebuild-active guard (already set → retryable error `a rebuild is already active for workspace "<slug>"`). Phase 1 under lock: fetch upstream (failure → retryable); open trunk runner (failure → retryable); resolve upstream base (failure → retryable); delete every directory under `<slug>/rebuild/` and `git worktree prune`; set `rerere.enabled=true`, `rerere.autoupdate=true`. Release the lock.
- CP-FR-123: Create a detached worktree at `<root>/<slug>/rebuild/<job id>` at the upstream base (failure → directory removed, prune, retryable error `create rebuild worktree: ...`). The trunk checkout is never touched (except CP-FR-128).
- CP-FR-124: List non-deleted patches ordered by position. Snapshot `refs/heads/<branch>^{commit}` for every patch not `merged_upstream`/`disabled`/`deleted`; an unresolvable branch gets no snapshot. Only snapshot SHAs are used afterwards (`source_sha`).
- CP-FR-125: For each patch in position order: `merged_upstream`/`disabled`/`deleted` → `skipped` with that reason (merged ones remembered for removal); no snapshot → `skipped`/`branch_not_found`; else apply with the payload strategy (empty → `rebase`):
  - `rebase`: commits = `git log --reverse --format=%H --no-merges --right-only --cherry-pick <base>...<sha>`; log failure or empty list → `skipped`/`branch_not_found`. Cherry-pick each in order (`cherry-pick --end-of-options <c>`); a cherry-pick that stops with nothing unmerged and nothing staged is skipped (`cherry-pick --skip`) as already-applied.
  - `merge`: `merge --no-ff -m "Merge branch '<branch_name>'" --end-of-options <sha>`.
  - On conflict: run `git rerere`, then `git diff --name-only --diff-filter=U`. None left → `cherry-pick --continue` / `commit --no-edit` and go on. Some left → `<op> --abort`, set the patch `status=conflict`, `conflict_files=<files>`; then `fail_fast` → **non-retryable** error `conflict in patch "<branch>": <file1>, <file2>` (no result stored, integration ref unchanged, job dead-lettered); `continue` → record `conflict` with files, `reset --hard` the worktree to the pre-patch HEAD, continue.
  - Success → `success`, `new_head_sha` = worktree HEAD.
  - After every patch the cumulative `patch_results` are written as job progress (best effort).
  - Context cancellation or a context error from git → retryable failure (never recorded as skip/conflict/success); other unknown git errors → retryable.
- CP-FR-126: Success path: cancellation check; final HEAD from worktree; take the blocking lock again (phase 2); read previous `refs/heads/<integration>` (absent → no `previous_integration_head_sha`); `update-ref` the integration ref to the final HEAD (failure → retryable error `update refs/heads/<ib>: ...`, and nothing below runs).
- CP-FR-127: Then soft-delete every patch that was skipped as `merged_upstream` (`patches_removed` = count), and **[FIX-10]** in the same transaction set every patch whose result is `success` and whose stored status is `conflict` back to `status=active`, with `conflict_files=NULL`. This includes `continue` runs in which other patches conflicted. Then renumber remaining non-deleted patches 1..N; if `REBUILD_PUSH_INTEGRATION_BRANCH` = `true`, force-push `+refs/heads/<ib>:refs/heads/<ib>` to origin with origin credentials (success → `integration_branch_pushed: true`; failure → log + `hub.rebuild.push_failed`, run still succeeds).
- CP-FR-128: When the trunk's HEAD is the integration branch, `reset --hard HEAD` in the trunk after the ref update (failure logged only).
- CP-FR-129: Release the lock, remove the worktree (`worktree remove --force` then `worktree prune`; falls back to deleting the directory; uses a non-cancelled context bounded by 2 min), run the stale-input check, emit `hub.rebuild.complete`, end the guard. The worktree is removed and the guard ended on every exit path.
- CP-FR-130 (stale-input check; after success and after a fail-fast conflict only): a patch is stale if `refs/heads/<branch>^{commit}` no longer equals its snapshot (including deleted); upstream is stale if the resolved upstream base differs. Ignore stale patches when `AUTO_REBUILD_AFTER_PUSH`=`false`; ignore stale upstream when `AUTO_REBUILD_AFTER_SYNC`=`false`. If anything remains, enqueue one follow-up rebuild: same payload with `submitted_by=system:stale-snapshot`, same group key as the running job, dedup excluding the running job itself. Duplicate → logged and skipped (no event). Enqueued → `hub.rebuild.followup`.
- CP-FR-131: Atomicity: the integration branch changes only via the single `update-ref` in phase 2; clones/fetches from the hub see the old integration branch until then. Patch application holds the guard but not the workspace lock (syncs, pushes, rollbacks may run concurrently).

**Rebuild list/get/cancel/requeue**
- CP-FR-140: List returns `{"jobs":[records]}` of type `rebuild`, key = slug, newest `created_at` first; `[]` when none. No mode check.
- CP-FR-141: Get/cancel/requeue/rollback: unknown id, a job of another workspace, or a non-rebuild job → 404 `rebuild job not found`.
- CP-FR-142: Cancel: status ≠ `queued` (including already `cancelled`) → 409 `job cannot be cancelled in its current state`; a race lost to a worker → same 409. Success → 200 `{"status":"cancelled"}`.
- CP-FR-143: Requeue: **[FIX-08]** refused while another rebuild of the workspace is `queued`, `running` or `failed`. Queue errors → 409 with the queue's message verbatim, e.g. `jobqueue: job "<id>" is not in dead_letter status (current: "queued")` or `jobqueue: active job "<id>" exists for (rebuild, <slug>)`. Success → 200 with the record (status `queued`, `completed_at` null).

**Rollback**
- CP-FR-150: Job has no result → 409 `no previous integration head to roll back to`; result unparsable → 500 `failed to parse rebuild result`; `previous_integration_head_sha` empty → 409 same message. Integration branch = payload `integration_branch` or `deploy`. Open runner (failure → 500 `failed to initialize git runner`), then try-lock (busy → 409 `workspace_busy`), then `git branch -f <ib> <previous sha>` (failure → 500 `failed to roll back integration branch`). Response 200 `{"rolled_back_to":"<sha>"}`. Patches, positions, soft-deleted rows and the fork are not touched; no event; no mode check.

**Preview**
- CP-FR-160: Not carry_patch → 400 `rebuild-preview is only supported for carry_patch workspaces`. Runner failure → 500 `failed to initialize git runner`. Base = upstream base resolved without fetching (failure → 500 `failed to resolve upstream HEAD; sync the workspace first`). Patch list failure → 500 `failed to list patches`.
- CP-FR-161: For non-deleted patches in position order, skipping `disabled`/`merged_upstream` (they are absent from the output; `active` and `conflict` are predicted): `rev-parse --verify <branch_name>` (unqualified) fails → `would_succeed` with no `tree_sha`; else `git merge-tree --write-tree --end-of-options <current> <patchHead>`: clean → `would_succeed` + `tree_sha`, and current := `commit-tree <tree> -p <current> -p <patchHead> -m preview` (cumulative; failure leaves current unchanged); conflict → `would_conflict` + `conflict_files` (paths parsed from `CONFLICT` lines), current unchanged; other error → `would_conflict` with `[]`. `conflict_files` is always present (`[]` on success). No refs, statuses or checkouts change (only unreferenced objects are written). No lock, no ready check.

**Sync extension** (on `POST /workspaces/:slug/sync` when the workspace is carry_patch, after the standard handler's own auth, scope (`workspaces:write`), write access, `ready` and `sync_enabled` checks)
- CP-FR-170: Try-lock (busy → 409 `workspace_busy`). Read `PATCH_BRANCH_SOURCE` (origin only on exact `origin`) and `PATCH_DIVERGENCE_POLICY` (`report` only on exact `report`, else `replace`) on every sync.
- CP-FR-171: Order: origin mode without an origin fetcher → 500 `origin fetch is not configured`; origin mode: resolve origin credentials (failure → 502 `failed to resolve origin credentials`); resolve upstream credentials (failure → 502 `failed to resolve upstream credentials`); open runner (500 `failed to create git runner`); fetch upstream (502 `upstream fetch failed`); origin mode: fetch all fork branches with prune (502 `origin fetch failed`, `error_type: origin_fetch_failed`); resolve upstream base (500 `failed to resolve upstream HEAD`). None of these failures write workspace or patch rows (fetched refs stay). `reset_to_upstream` is ignored. `sync_error` is neither set nor cleared.
- CP-FR-172: `upstream_advanced` = new base ≠ stored `upstream_head_sha` (true on the first sync). `force_push_detected` = advanced AND stored non-empty AND stored is not an ancestor of the new base (an ancestry error → false).
- CP-FR-173 (origin mode refresh): candidates = patches with status active/conflict/disabled whose branch ≠ integration branch, in position order. Per candidate, first rule: fork lacks `refs/remotes/origin/<b>` → action `none`, state `missing_on_origin`; no local branch → create (CAS vs zero) → `created`/`in_sync`; equal → `none`/`in_sync`; local is ancestor of fork → CAS fast-forward → `fast_forwarded`/`in_sync`; otherwise diverged (including fork behind): policy `report` → `none`/`diverged`; policy `replace` → CAS-write `refs/hub/replaced/<b>` = local tip (expected old = current backup or zero), then CAS move → `replaced`/`in_sync` with `replaced_sha`. After a move, if the branch is checked out in the trunk, `reset --hard HEAD`.
- CP-FR-174: A refresh failure stops at that branch: CAS/write failure (message `failed to update patch branch <b>`), ancestry-check failure (`failed to compare patch branch <b> with origin`) produce no outcome for the branch; a reset failure after the move (`failed to reset working tree for patch branch <b>`) keeps the outcome and counts as moved. Outcomes so far are persisted, merged/deleted origin columns cleared, a rebuild is enqueued if a moved active/conflict branch exists and auto-rebuild-after-sync is on (result not reported), origin events are emitted, and the answer is 500 with that message; `upstream_head_sha`/`last_sync_at` are not written and merge detection does not run.
- CP-FR-175: `patch_advanced` = some branch of an active/conflict patch moved (created/fast_forwarded/replaced). Persist each outcome's origin state (`origin_sha` null when missing) with `origin_synced_at`=now; then clear the origin columns of `merged_upstream` and `deleted` rows. Hub mode: clear the origin columns of every patch.
- CP-FR-176 (merge detection, when `upstream_advanced || patch_advanced`): `SQUASH_MERGE_DETECTION` ∈ {`ancestry_only`, `content_based`, `both`} (invalid → `both`). For each **`active`** patch: unless `content_based`, ancestry = `merge-base --is-ancestor <branch_name> <new base>`; an ancestry **error skips the patch entirely**; if not merged and mode ≠ `ancestry_only`: content = `git cherry <new base> <branch>` reports ≥1 commit and none pending (`+`); or PR scan = `upstream_pr_url` contains `/pull/<digits>` (trailing path/query stripped) and some subject of `git log --format=%s <stored>..<new>` (or `-50 <new>` when stored is empty) ends with `(#<n>)` after trimming. Merged → `status=merged_upstream`, `conflict_files="[]"`; branch name appended to `patches_merged`. In origin mode, origin columns of newly merged patches are cleared again.
- CP-FR-177: Write `last_sync_at`=`updated_at`=now, plus `upstream_head_sha` when advanced (DB failure → 500 `failed to update workspace`).
- CP-FR-178: If `upstream_advanced || patch_advanced || patches_merged non-empty` and `AUTO_REBUILD_AFTER_SYNC` ≠ `false`: enqueue (payload per CP-FR-111 without overrides, `submitted_by`=caller user id); newly enqueued → `rebuild_triggered: true`, `rebuild_job_id`; duplicate or enqueue error → `false` (error logged). The sync does not check for active patches.
- CP-FR-179: Origin mode only, emit `hub.patch.replace` per replaced branch then one `hub.patch.sync`. Hub mode emits nothing. Response: workspace object re-read after the sync + carry-patch fields (7.5).

**Push control (git server pre-receive, per ref update, before the ref is written)**
- CP-FR-189: **[FIX-11]** Push control first protects the integration branch. On a carry_patch workspace, a client update (create, update or delete) of `refs/heads/<integration_branch>` is rejected per ref, with `integration branch <name> is maintained by the hub; push a patch branch and rebuild instead`. `<integration_branch>` is the workspace's `integration_branch`, or `deploy` when it is empty. This applies in both `PATCH_BRANCH_SOURCE` modes and is independent of every setting. A failed workspace lookup fails open, as in CP-FR-190. The hub's own writes of the integration branch (rebuild, rollback) do not pass through the git server and are unaffected. In the baseline, the git server accepted these pushes and only the API branch push refused the integration branch.
- CP-FR-190: Applies only to `refs/heads/<name>` of a carry_patch workspace, `<name>` ≠ integration branch, registered as a patch with status ≠ `deleted`. Any lookup failure (workspace or patch query, including unknown workspace) fails **open** (update allowed, warning logged). Standard workspaces read no setting.
- CP-FR-191: Hub mode → allowed. Origin mode with `PUSH_PATCHES_TO_ORIGIN` ≠ `true` → rejected (creates, updates and deletes) with per-ref message `branch is synced from origin; push to <git_url without userinfo> instead`.
- CP-FR-192: Origin mode with forwarding: delete → rejected `branch is synced from origin; delete it on the fork instead` (fork not contacted); credential failure → rejected `failed to resolve origin credentials`; else write `refs/hub/forward/<name>` = new SHA in the trunk, non-force push `refs/hub/forward/<name>:refs/heads/<name>` to origin (timeout 120 s), remove the temp ref; failure → rejected `origin rejected push: <error with URL userinfo removed>`; success/up-to-date → allowed.
- CP-FR-193: Rejection is per ref (other refs in the same push proceed). Pushes to `refs/hub/replaced/*` are rejected by the git server before this hook with `refs/hub/replaced/ is maintained by the hub and cannot be pushed to`. **[FIX-11]** The integration branch is protected on the git server by CP-FR-189.

**Post-push hook (async, after the push response, only accepted non-delete `refs/heads/*` updates)**
- CP-FR-200: Workspace carry_patch; any pushed branch matching a patch row (any status, deleted included; integration branch not excluded); `AUTO_REBUILD_AFTER_PUSH` ≠ `false` → enqueue a rebuild (payload per CP-FR-111, `submitted_by` and job `submitted_by` = `system:push-hook`); duplicate silently ignored; errors logged. No ready/active-patch check.
- CP-FR-201 (mirror, after the enqueue, same goroutine, independent of auto-rebuild): carry_patch AND `PUSH_PATCHES_TO_ORIGIN`=`true` AND hub mode; applicable = pushed branches ≠ integration branch and registered non-deleted; if any, resolve origin credentials once (failure → `hub.patch.mirror_failed` per applicable branch with error `failed to resolve origin credentials: <sanitized>`); force-push `+refs/heads/<b>:refs/heads/<b>` to origin per branch (timeout 120 s); failure → log + `hub.patch.mirror_failed`; one failure does not stop the others. No lock.

**Rerere**
- CP-FR-210: List reads `<trunk>/.git/rr-cache/`: each subdirectory with at least one `preimage*` or `postimage*` file (variants `name.N` included) is an entry; `id` = directory name; `resolved` = a postimage variant exists; `recorded_at` = mtime of the newest postimage variant if resolved, else of the newest preimage variant (RFC3339 UTC); `path` = path from `.git/MERGE_RR` (NUL-separated `<id>\t<path>` records) if present, else from the first labelled conflict marker line `<<<<<<< <path>` of `preimage` then `postimage`, else null. Order = directory-name order. Missing cache → `[]`. No mode check.
- CP-FR-211: Forget: pathspec = wildcard remainder with leading `/` trimmed; empty → 400 `pathspec is required`; starts with `-` → 400 `pathspec must not start with '-'` (both checked after scope but before workspace access). Then access → try-lock (409 `workspace_busy`). If pathspec is a single component (no `/` or `\`, not starting with `.`) naming an existing rr-cache subdirectory → remove that directory (failure → 500 `failed to remove rerere entry`). Else if some listed entry has `path` == pathspec → `git rerere forget <pathspec>` (runner failure → 500 `failed to create git runner`; git failure → 500 `git rerere forget failed`); else → 404 `no recorded resolution for pathspec`. Success 204.

**Patch-status dashboard**
- CP-FR-220: Not carry_patch → 400 `patch-status is only available for carry_patch workspaces`. Aggregates (7.5). Rebuild-list failure → treated as no rebuild. Unreadable rr-cache → 0.

**Startup / maintenance**
- CP-FR-230: At startup, before the job queue starts: for every directory `<workspace.path>/<slug>/` that has a `rebuild/` directory, delete `rebuild/` entirely and run `git worktree prune` in `<slug>/trunk` (missing trunk → warning, skip). Never fails startup.
- CP-FR-231: **[FIX-12]** The hourly maintenance loop (PLAT-FR-005) runs the patch purge. For every patch with `status=deleted` and `deleted_at` older than 7 days, it removes `refs/hub/replaced/<branch>` from the workspace's trunk (missing ref or missing trunk → skipped), then deletes the row, but only if the row is still `deleted` (a concurrent restore wins). Failures are logged per patch and retried on the next pass. In the baseline the purge routine existed, but nothing invoked it.

**Hardened git runner (`gitcmd`) — observable guarantees**
- CP-FR-240: Requires git ≥ 2.38 (`git --version` checked at runner creation, 30 s timeout); the working directory must exist and be a directory.
- CP-FR-241: Environment: process environment minus `GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE`, `GIT_OBJECT_DIRECTORY`, `GIT_ALTERNATE_OBJECT_DIRECTORIES`, `GIT_COMMON_DIR`, `GIT_NAMESPACE`, `GIT_CONFIG_PARAMETERS`, `GIT_CONFIG_COUNT`; defaults added only when unset: `GIT_AUTHOR_NAME=parleyd`, `GIT_AUTHOR_EMAIL=parleyd@localhost`, `GIT_COMMITTER_NAME=parleyd`, `GIT_COMMITTER_EMAIL=parleyd@localhost`, `GIT_EDITOR=true`, `GIT_PAGER=cat`; always forced last: `GIT_ALLOW_PROTOCOL=file:https:ssh`, `GIT_TERMINAL_PROMPT=0`, `GIT_CONFIG_NOSYSTEM=1`, `LC_ALL=C`.
- CP-FR-242: Each git invocation without a context deadline gets a 10-minute timeout; on cancellation the whole process group is killed (SIGKILL; Unix), and waiting for pipes is bounded by 5 s.
- CP-FR-243: Ref/commit arguments are passed after `--end-of-options`. Failures surface as `git <args>: exit code <n>: <stderr>` with URL userinfo replaced by `***@` (args and stderr). Upstream/origin fetches and pushes are done in-process (go-git), not through the CLI.

### 7.5 REST API

All paths are under `/api/v1`. Error envelope: `{"error":{"code":<status>,"message":"...","error_type":"..."?}}`. Access errors per CP-FR-010 are omitted from per-endpoint tables unless special.

#### `POST /workspaces/:slug/patches`
- **Purpose:** Register one patch or a batch.
- **Auth / scope:** any credential; PAT `patches:write`; write access.
- **Request body:** object or array of objects:
  `branch_name` string required (CP-FR-020); `position` int ≥1 optional; `upstream_pr_url` string optional; `description` string optional; `skip_branch_check` bool default false; `if_not_exists` bool default false (single only). Unknown fields ignored.
- **Behaviour:** CP-FR-040..044, resolution CP-FR-030..035.
- **Response:** 201 patch (single) / 201 array of patches in request order (batch) / 200 existing patch (`if_not_exists` hit).
- **Errors (in order):** 400 `workspace is archived`; 400 `workspace is not in carry_patch mode`; 400 `invalid request body: ...`; single: 400 `branch_name is required`, 400 `invalid branch_name: ...`, 400 `branch_name cannot be the integration branch`, resolver errors (400 `branch does not exist in repository or on origin`, 409 `workspace_busy`, 502 `origin fetch failed`/`origin_fetch_failed`, 502 `failed to resolve origin credentials`, 500 `internal server error`), 409 `branch already exists in patch list`, 400 `position must be >= 1`, 500 `failed to add patch: <err>`; batch: per CP-FR-043, 500 `failed to add patches: <err>`.
- **Side effects:** row(s) inserted; local branches may be created; `hub.patch.create` (single only).

#### `GET /workspaces/:slug/patches`
- **Auth / scope:** PAT `patches:read` or `patches:write`; read access.
- **Response:** 200 array of non-deleted patches by position; `[]` for standard workspaces/no patches.
- **Errors:** 500 `internal server error`.

#### `GET /workspaces/:slug/patches/:id`
- **Auth / scope:** `patches:read`|`patches:write`; read access.
- **Response:** 200 patch (deleted included) + optional `replaced_sha`.
- **Errors:** 404 `patch not found`; 500 `internal server error`.

#### `PATCH /workspaces/:slug/patches/:id`
- **Auth / scope:** `patches:write`; write access.
- **Request body:** `position` int, `status` string, `description` string, `upstream_pr_url` string — all optional.
- **Behaviour:** CP-FR-060/061.
- **Response:** 200 updated patch.
- **Errors:** 404 `patch not found`; 400 `invalid request body: ...`; 400 `invalid status value; must be one of: active, merged_upstream, conflict, disabled`; 409 `patch is soft-deleted; restore it first`; 400 `position out of range`; 500 `failed to update patch position` / `failed to update patch` / `failed to fetch updated patch` / `internal server error`.

#### `DELETE /workspaces/:slug/patches/:id`
- **Auth / scope:** `patches:write`; write access.
- **Response:** 204 no body.
- **Errors:** 404 `patch not found`; 500 `failed to delete patch`.
- **Side effects:** **[FIX-09]** soft delete (`status: deleted`) and renumber; `hub.patch.delete`; the backup ref is kept until the purge. A repeated DELETE of a deleted patch answers 204 with no event.

#### `POST /workspaces/:slug/patches/:id/restore`
- **Auth / scope:** `patches:write`; write access. No body.
- **Response:** 200 restored patch (`status: active`, appended, `deleted_at: null`).
- **Errors:** 400 `workspace is archived`; 400 `workspace is not in carry_patch mode`; 404 `patch not found`; 400 `patch is not in deleted status`; 500 `internal server error` / `failed to restore patch` / `failed to fetch restored patch`.

#### `POST /workspaces/:slug/patches/:id/reset-to-origin`
- **Auth / scope:** `patches:write`; write access. No body.
- **Behaviour:** CP-FR-100..105.
- **Response:** 200 patch + `replaced_sha`? + `rebuild_triggered` + `rebuild_job_id`?.
- **Errors (in order):** 400 `workspace is archived`; 400 `workspace is not in carry_patch mode`; 409 `workspace clone is not ready`; 500 `patch reset is not configured`; 404 `patch not found`; 400 `patch branch is the integration branch`; 409 `patch status <s> cannot be reset`; 409 `another operation is running on this workspace; retry later` (`workspace_busy`); 502 `failed to resolve origin credentials`; 409 `branch does not exist on origin` (`missing_on_origin`); 502 `origin fetch failed` (`origin_fetch_failed`); 409 `patch branch changed during reset; retry` (`ref_changed`); 500 `failed to update patch branch <branch>`; 500 `failed to fetch updated patch`.
- **Side effects:** branch/backup refs; origin state (origin mode); possible rebuild enqueue; `hub.patch.reset` (+ `hub.patch.replace`).

#### `POST /workspaces/:slug/patches/reorder`
- **Auth / scope:** `patches:write`; write access.
- **Request body:** `{"patch_ids": ["<id>", ...]}`.
- **Response:** 200 array of all non-deleted patches in new order.
- **Errors:** 400 for every failure (messages CP-FR-090).

#### `GET /workspaces/:slug/rerere`
- **Auth / scope:** PAT `workspaces:read`|`workspaces:write`; read access.
- **Response:** 200 `{"resolutions":[{"id":string,"path":string|null,"recorded_at":string|null,"resolved":bool}]}`.

#### `DELETE /workspaces/:slug/rerere/*pathspec`
- **Auth / scope:** PAT `workspaces:write`; write access.
- **Path params:** wildcard (slashes allowed, each segment percent-decoded by routing).
- **Response:** 204.
- **Errors (in order):** 400 `pathspec is required`; 400 `pathspec must not start with '-'`; access errors; 409 `workspace_busy`; 500 `failed to remove rerere entry`; 404 `no recorded resolution for pathspec`; 500 `failed to create git runner`; 500 `git rerere forget failed`.

#### `POST /workspaces/:slug/rebuild`
- **Auth / scope:** PAT `rebuilds:write`; write access.
- **Request body (optional):** `strategy` (`rebase`|`merge`), `fail_mode` (`fail_fast`|`continue`). Must be JSON; parsed only when Content-Length > 0.
- **Response:** 202
```json
{"id":"<uuid>","type":"rebuild","key":"<slug>","group_key":"<slug>:<ib>","status":"queued",
 "payload":{"workspace_slug":"<slug>","strategy":"rebase","submitted_by":"<user id>","integration_branch":"deploy","fail_mode":"fail_fast"}}
```
- **Errors (in order):** 400 `rebuild is only supported for carry_patch workspaces` (`workspace_mode_mismatch`); 400 `workspace is not ready`; 400 `no patches with status active or conflict` (`no_active_patches`); 400 `invalid request body`; 400 `strategy must be 'rebase' or 'merge'`; 400 `fail_mode must be 'fail_fast' or 'continue'`; 500 `failed to enqueue rebuild job`; 409 `a rebuild job is already queued or running for this workspace` (`concurrent_rebuild`); 500 `database error` / `failed to marshal payload`.
- **Side effects:** job queued; `hub.rebuild.enqueue`.

#### `GET /workspaces/:slug/rebuilds`
- **Auth / scope:** `rebuilds:read`|`rebuilds:write`; read access.
- **Response:** 200 `{"jobs":[<rebuild job record>...]}` newest first.
- **Errors:** 500 `failed to list rebuild jobs`.

#### `GET /workspaces/:slug/rebuilds/:id`
- **Auth / scope:** `rebuilds:read`|`rebuilds:write`; read access.
- **Response:** 200 rebuild job record (7.3.4).
- **Errors:** 404 `rebuild job not found`.

#### `DELETE /workspaces/:slug/rebuilds/:id`
- **Auth / scope:** `rebuilds:write`; write access.
- **Response:** 200 `{"status":"cancelled"}`.
- **Errors:** 404 `rebuild job not found`; 409 `job cannot be cancelled in its current state`; 500 `failed to cancel job`.

#### `POST /workspaces/:slug/rebuilds/:id/requeue`
- **Auth / scope:** `rebuilds:write`; write access.
- **Response:** 200 rebuild job record.
- **Errors:** 404 `rebuild job not found`; 409 `<queue error text>`; 500 `failed to retrieve requeued job`.

#### `POST /workspaces/:slug/rebuilds/:id/rollback`
- **Auth / scope:** `rebuilds:write`; write access.
- **Response:** 200 `{"rolled_back_to":"<sha>"}`.
- **Errors (in order):** 404 `rebuild job not found`; 409 `no previous integration head to roll back to`; 500 `failed to parse rebuild result`; 409 (same message); 500 `failed to initialize git runner`; 409 `workspace_busy`; 500 `failed to roll back integration branch`.

#### `GET /workspaces/:slug/rebuild-preview`
- **Auth / scope:** `rebuilds:read`|`rebuilds:write`; read access.
- **Response:** 200 `{"patch_results":[{"patch_id","branch_name","position","status":"would_succeed"|"would_conflict","tree_sha"?:string,"conflict_files":[...]}]}`.
- **Errors:** 400 `rebuild-preview is only supported for carry_patch workspaces`; 500 `failed to initialize git runner`; 500 `failed to resolve upstream HEAD; sync the workspace first`; 500 `failed to list patches`; 500 `database error`.

#### `GET /workspaces/:slug/patch-status`
- **Auth / scope:** PAT `workspaces:read`|`workspaces:write`; read access.
- **Response:** 200:

| Field | Rule |
|---|---|
| `workspace_slug`, `workspace_mode` (`carry_patch`), `status` | from workspace |
| `clone_error`, `sync_error` | omitted when null/empty |
| `head_sha`, `git_url`, `upstream_url`, `upstream_head_sha`, `integration_branch` | string, `""` when null |
| `integration_head_sha` | from the result of the **latest** rebuild job (by `created_at`); `""` when that job has no result (queued/running/failed/...) or no job |
| `last_sync_at` | string or null |
| `last_rebuild` | `{id, status}` of the latest rebuild job (any status), or null |
| `patches[]` | non-deleted patches by position: `id`, `branch_name`, `position`, `status`, `last_rebuild_result` (status of this patch in the latest job's result, else null), `conflict_files` (from that patch result, omitted when empty), `origin_sync_state`/`origin_sha`/`origin_synced_at` (omitted when null) |
| `summary` | `total_patches` (= len(patches)), `active`, `merged_upstream`, `conflict`, `disabled`, `total_rerere_resolutions` (rr-cache entries incl. unresolved), `patches_diverged`, `patches_missing_on_origin` (counts of origin_sync_state) — all always present |

- **Errors:** 400 `patch-status is only available for carry_patch workspaces`; 500 `database error` / `failed to list patches` / `failed to scan patch` / `failed to iterate patches`.

#### `POST /workspaces/:slug/sync` (carry-patch extension)
- **Purpose:** carry-patch sync (CP-FR-170..179); standard preconditions belong to §4.
- **Response:** 200 workspace object (re-read) plus: `patches_merged` string[] (always, `[]` if none), `rebuild_triggered` bool, `force_push_detected` bool, `origin_fetched` bool (true only in origin mode), `rebuild_job_id` (only when enqueued), and in origin mode only `patches_synced` (array, `[]` when no candidates; elements `{branch_name, action: none|created|fast_forwarded|replaced, state: in_sync|diverged|missing_on_origin, local_sha?, origin_sha?, replaced_sha?}`, one per candidate incl. `none`) and `patches_diverged` (branch names with state `diverged`).
- **Errors:** see CP-FR-171/174 (409 `workspace_busy`; 500 `origin fetch is not configured`; 502 `failed to resolve origin credentials`; 502 `failed to resolve upstream credentials`; 500 `failed to create git runner`; 502 `upstream fetch failed`; 502 `origin fetch failed`/`origin_fetch_failed`; 500 `failed to resolve upstream HEAD`; 500 `failed to list patches`; 500 refresh messages; 500 `failed to update workspace`; 500 `failed to read workspace after sync`).

### 7.6 CLI

Common: all commands print the hub's JSON answer with two-space indentation on stdout; errors print `{"error":{"code","message","error_type"?}}` on stdout; exit 0 success, 1 hub error / failed wait / timeout, 2 usage/client error. Every argument placed in the URL is percent-encoded as one path segment (except `rerere forget`). Usage errors produced by commands themselves use exit 2 with the message in the envelope.

#### `parley patch add <workspace-slug>`
- **Flags:** `--branch` string (required; missing → exit 2 `--branch flag is required`); `--position` int (sent only if the flag was given, any value); `--upstream-pr` string (sent only if non-empty, as `upstream_pr_url`); `--description` string (sent only if non-empty); `--skip-branch-check` bool (sends `true` only when set); `--if-not-exists` bool (sends `true` only when set).
- **Calls:** `POST /workspaces/:slug/patches` (single object).
- **Output:** the patch JSON.

#### `parley patch list <workspace-slug>`
- **Calls:** `GET /workspaces/:slug/patches`. **Output:** JSON array.

#### `parley patch remove <workspace-slug> <patch-id>`
- **Calls:** `DELETE /workspaces/:slug/patches/:id`.
- **Output:** stdout `{"status":"removed","patch_id":"<id>"}` (indented); stderr `Patch '<id>' has been removed.`
- **Notes:** **[FIX-09]** The removal is a soft delete: `parley patch restore <workspace-slug> <patch-id>` brings the patch back until it is purged, 7 days after removal.

#### `parley patch update <workspace-slug> <patch-id>`
- **Flags:** `--status`, `--description`, `--upstream-pr` (→ `upstream_pr_url`), `--position` int; each sent only when the flag was given (empty strings are sent). None given → exit 2 `at least one of --status, --description, --upstream-pr, --position is required`.
- **Calls:** `PATCH /workspaces/:slug/patches/:id`. **Output:** updated patch.

#### `parley patch reorder <workspace-slug> <patch-id-1> [patch-id-2] ...`
- **Args:** at least 2 positional args (slug + ≥1 id).
- **Calls:** `POST .../patches/reorder` with `{"patch_ids":[...]}` in argument order. **Output:** reordered list.

#### `parley patch restore <workspace-slug> <patch-id>`
- **Calls:** `POST .../patches/:id/restore` (no body). **Output:** restored patch.

#### `parley patch reset-to-origin <workspace-slug> <patch-id>`
- **Calls:** `POST .../patches/:id/reset-to-origin` (no body). **Output:** response object.

#### `parley rebuild submit <workspace-slug>`
- **Flags:** `--strategy` string (sent when non-empty; values not validated client-side), `--fail-mode` string (sent as `fail_mode` when non-empty), `--wait` bool false, `--timeout` duration `5m0s`, `--poll-interval` duration `5s`.
- **Calls:** `POST /workspaces/:slug/rebuild` (no body when neither flag), then with `--wait` `GET /workspaces/:slug/rebuilds/:id`.
- **Output:** without `--wait`: indented job. With `--wait`: compact one-line submit response, then polls immediately and every interval until status ∈ {completed, dead_letter, cancelled} (**[FIX-07]** not `failed`); prints the final record (indented). Non-completed terminal → stderr `Job ended with status <s>.`, error envelope `job ended with status <s>`, exit 1. Timeout → stderr `Timed out waiting for job to complete after <timeout>`, envelope `timed out after <timeout>`, exit 1. Response without `id` → exit 2.
- **Notes:** **[FIX-07]** `failed` (retry pending) is not final, so polling continues. In the baseline it was treated as final.

#### `parley rebuild list <workspace-slug>` / `parley rebuild status <workspace-slug> <rebuild-id>` / `parley rebuild preview <workspace-slug>`
- **Calls:** `GET .../rebuilds`, `GET .../rebuilds/:id`, `GET .../rebuild-preview`. **Output:** the response.

#### `parley rebuild cancel <workspace-slug> <rebuild-id>`
- **Calls:** `DELETE .../rebuilds/:id` (response body ignored).
- **Output:** stdout `{"status":"cancelled","rebuild_id":"<id>"}`; stderr `Rebuild job '<id>' has been cancelled.`

#### `parley rebuild requeue <workspace-slug> <rebuild-id>` / `parley rebuild rollback <workspace-slug> <rebuild-id>`
- **Calls:** `POST .../rebuilds/:id/requeue`, `POST .../rebuilds/:id/rollback` (no body). **Output:** response.

#### `parley rerere list <workspace-slug>`
- **Calls:** `GET .../rerere`. **Output:** response.

#### `parley rerere forget <workspace-slug> <pathspec>`
- **Calls:** `DELETE .../rerere/<pathspec>` where the pathspec keeps its `/` separators and each segment is percent-encoded.
- **Output:** stdout `{"status":"forgotten","pathspec":"<arg>"}`; stderr `Rerere resolution for '<arg>' has been forgotten.`

#### `parley workspace patch-status <workspace-slug>`
- **Calls:** `GET .../patch-status`. **Output:** response.

Related (defined in §4, carry-patch relevant): `parley workspace create --workspace-mode carry_patch --upstream-url <url> [--integration-branch <b>] [--setting k=v ...]` (client-side: invalid mode, missing `--upstream-url` in carry_patch, or `--upstream-url`/`--integration-branch` in standard → exit 2); `parley workspace sync <slug> [--wait] [--fail-on-diverged] [--reset-to-upstream]` — with `--wait` it polls `rebuilds/<rebuild_job_id>` when present; `--fail-on-diverged` exits 3 (message `diverged patch branches: a, b` on stderr, no envelope) when `patches_diverged` is non-empty, after the wait (a wait failure wins).

### 7.7 Background behaviour

- **Rebuild worker:** jobs of type `rebuild` run on the hub's shared job queue, serialized per `group_key` (`<slug>:<integration_branch>`), deduplicated per (type, key=slug) over `queued`/`running`/`failed` **[FIX-08]**. Retryable failures go to `failed` with backoff (2 s × 2^n, cap 2 h) and are re-queued when due; after 20 retries or on a non-retryable error (fail-fast conflict, bad payload) → `dead_letter`. Enqueue sources: API submit (user id), sync (user id), reset-to-origin (user id), post-push hook (`system:push-hook`), stale-input follow-up (`system:stale-snapshot`).
- **Post-push goroutine:** after every push with ≥1 accepted non-delete branch update: rebuild enqueue then hub-mode mirror (CP-FR-200/201).
- **Startup:** stale rebuild directory cleanup (CP-FR-230) runs before the queue starts; the queue then resets jobs left `running` to `queued`. The carry-patch module must be bound to its host (locks, access rule) before routes/jobs are used.
- **Shutdown:** a cancelled rebuild is a retryable failure; its worktree is removed with a non-cancelled 2-minute context.
- **Schedules:** there is no periodic sync and no fork webhook reaction. **[FIX-12]** Soft-deleted patches are purged after 7 days by the hourly maintenance loop (CP-FR-231).

### 7.8 Events

All are hub audit events (stored and streamed); emission failures are logged and never change the outcome. `resource_type` is `patch` for all.

| Event | When | resource_id / action / actor | Metadata |
|---|---|---|---|
| `hub.patch.create` | single add created a row (not batch, not `if_not_exists` hit) | branch / `create` / caller | `branch_name`, `position`, `branch_resolution` (`local`\|`origin_tracking`\|`origin_fetch`\|`skipped`) |
| `hub.patch.delete` | DELETE soft-deleted a patch **[FIX-09]** | branch / `delete` / caller | `branch_name` |
| `hub.patch.reset` | reset-to-origin succeeded (any action incl. `none`) | branch / `reset` / caller | `branch_name`, `action`, `origin_sha`, `local_sha` (absent when no local branch), `replaced_sha` (only `replaced`) |
| `hub.patch.replace` | reset with action `replaced`; or each branch replaced by an origin-mode sync (also on the refresh-failure path) | branch / `replace` / caller | `branch_name`, `replaced_sha`, `origin_sha`; `trigger: "reset_to_origin"` for resets only |
| `hub.patch.sync` | each origin-mode sync that produced outcomes (success, and the refresh-failure path), after the replace events | slug / `sync` / caller | `origin_fetched: true`, `created`, `fast_forwarded`, `replaced`, `diverged`, `missing_on_origin` (branch-name arrays, `[]` when empty) |
| `hub.patch.mirror_failed` | hub-mode mirror of a branch failed | none / `mirror` / `system` | `branch_name`, `error` (userinfo stripped) |
| `hub.rebuild.enqueue` | `POST .../rebuild` enqueued (only this route) | job id / `enqueue` / caller | `job_id`, `patch_count` (active+conflict count) |
| `hub.rebuild.complete` | rebuild success | none / none / `system` | `patches_applied` |
| `hub.rebuild.fail` | every failed attempt (except payload parse errors) | none / none / `system` | `reason` (error text) |
| `hub.rebuild.push_failed` | integration push to origin failed | none / none / `system` | `integration_branch`, `reason` |
| `hub.rebuild.followup` | stale-input follow-up enqueued | none / none / `system` | `stale_patches` (string[], may be `[]`), `upstream_stale` (bool), `follow_up_job_id` |

Actor type values for caller-actor events: `admin_token`, `api_key`, `pat`.

### 7.9 Configuration

**Workspace settings** (JSON `settings` of the workspace; read by the module under legacy names; booleans read as `"true"`/`"false"`):

| Setting (legacy name) | Values | Default | Effect |
|---|---|---|---|
| `patch_branch_source` (`PATCH_BRANCH_SOURCE`) | `hub`, `origin` | `hub` | Authority for patch branches (resolution step 3, sync refresh, push control, mirror) |
| `patch_divergence_policy` (`PATCH_DIVERGENCE_POLICY`) | `replace`, `report` | `replace` | Sync handling of diverged branches |
| `push_patches_to_origin` (`PUSH_PATCHES_TO_ORIGIN`) | bool | `false` | Origin mode: forward; hub mode: mirror |
| `auto_rebuild_after_push` (`AUTO_REBUILD_AFTER_PUSH`) | bool | `true` | Post-push enqueue; stale-patch follow-ups |
| `auto_rebuild_after_sync` (`AUTO_REBUILD_AFTER_SYNC`) | bool | `true` | Sync enqueue; reset-to-origin enqueue; stale-upstream follow-ups |
| `rebuild_strategy` (`REBUILD_STRATEGY`) | `rebase`, `merge` | `rebase` | Captured at enqueue |
| `rebuild_fail_mode` (`REBUILD_FAIL_MODE`) | `fail_fast`, `continue` | `fail_fast` | Captured at enqueue |
| `rebuild_push_integration_branch` (`REBUILD_PUSH_INTEGRATION_BRANCH`) | bool | `false` | Force-push integration branch to origin after success (read at run time) |
| `squash_merge_detection` (`SQUASH_MERGE_DETECTION`) | `ancestry_only`, `content_based`, `both` | `both` | Sync merge detection |
| `sync_enabled` | bool | `true` | `false` refuses every sync (standard handler) |

Settings are read on every sync, push, registration and job (no restart). Module-side defensive parsing: only exact `origin`/`report`/`true`/`false` strings select non-default behaviour; anything else is the default.

**Secrets:** `UPSTREAM_GIT_PAT`, `UPSTREAM_GIT_USERNAME`, `UPSTREAM_GIT_PASSWORD` (upstream), `GIT_PAT`, `GIT_USERNAME`, `GIT_PASSWORD` (origin; also upstream fallback). Resolved on every use.

**Server config / env:** `[workspace].path` (root of `<slug>/trunk` and `<slug>/rebuild`); git CLI ≥ 2.38 on `PATH`; `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME`, `GIT_COMMITTER_EMAIL` (identity of rebuild commits; default `parleyd` / `parleyd@localhost`). Hard-coded limits: forward/mirror push timeout 120 s; git default timeout 10 min; cleanup timeout 2 min; purge retention 7 days (unused); PR scan window 50 commits on first sync.

### 7.10 Edge cases and error handling

- A soft-deleted patch blocks re-registering its branch (UNIQUE): add → 409 `branch already exists in patch list`; with `if_not_exists` → 200 returning the **deleted** record.
- Single add checks `position < 1` only after resolution and the duplicate check (duplicate + `position: 0` → 409/200, not 400).
- `PATCH` with only `position` on a `deleted` patch is not refused (only `status` triggers the 409); the deleted row is inserted into the 1..N order and keeps status `deleted`.
- Empty request body on add/reorder/PATCH → 400 `invalid request body: EOF`; `PATCH` with `{}` → 200.
- Add on a workspace that is `pending`/`cloning`/`failed` is not refused; without `skip_branch_check` the resolver fails (no trunk) → 500 `internal server error`.
- Rebuild `skipped/branch_not_found` is also reported when every commit of the branch is already upstream (empty `log --cherry-pick` list) or the log fails.
- `conflict` patches stay `conflict` after a clean apply; operators must `PATCH status=active`. `conflict_files` remain in the patch object until overwritten.
- In continue mode a conflicted patch is set `conflict` and the run completes; merged patches are still soft-deleted.
- A rebuild with zero applicable patches (e.g. queued by sync/push while all are disabled) still moves the integration branch to the upstream base.
- Sync merge detection: an ancestry error (e.g. branch missing locally) skips content/PR detection for that patch; only `active` patches are checked (not `conflict`).
- Post-push matches patch rows of any status (including `deleted`) and does not exclude the integration branch, so a push to a soft-deleted patch's branch enqueues a rebuild. Pushes to the integration branch never get this far, because CP-FR-189 rejects them first **[FIX-11]**.
- Rollback uses `git branch -f`, which git refuses when the integration branch is checked out in the trunk → 500 `failed to roll back integration branch`. Rollback does not verify the SHA still exists.
- `GET rebuilds/:id` of a `failed` (retry pending) job shows `completed_at`; a `cancelled` job shows `completed_at: null`; progress is only shown while `running`.
- Submit body sent with chunked encoding (unknown Content-Length) is ignored (settings apply).
- **[FIX-08]** Duplicate enqueue considers `queued`, `running` and `failed`. While a rebuild is waiting to retry, a manual submit answers 409 `concurrent_rebuild`. Automatic triggers (sync, reset-to-origin, post-push and the stale-input follow-up) are silently suppressed, as for any duplicate. No input is lost: the retry takes fresh snapshots of upstream and of every patch branch when it runs (CP-FR-124). In the baseline, a new submit succeeded and both jobs ran one after the other.
- Preview resolves branch names unqualified (`rev-parse --verify <name>`), so a tag or other ref with that name may be used; a missing branch is reported `would_succeed`.
- Rerere forget by path only works when the path is known (MERGE_RR or labelled marker); otherwise 404 even if git would know it.
- Origin-mode sync with an SSH `git_url`: rejection message keeps the URL as given when it is not parseable as a URL.
- All locks/guards are in-process; multiple hub processes on one data directory are not coordinated.

### 7.11 Discrepancies between docs and code

1. **`parley patch remove` "can be brought back with `parley patch restore`"** (docs/cli.md "parley patch remove", "parley patch restore: Bring a removed patch back"). Code: DELETE hard-deletes the row (`carrypatch/patch_store.go:474` `DELETE FROM patches`); restore requires status `deleted` (`patch_handlers.go:641`), which only a rebuild sets for merged patches. A removed patch cannot be restored. → **Resolved in this PRD by FIX-09:** the re-implementation follows the documented behaviour described here (see §1.8).
2. **Rebuild job record fields** (api.md "Rebuild Job Response Schema" lists `upstream_head_sha`, `integration_branch_pushed`; `error: ""`). Code `RebuildJobRecord` (`carrypatch/api.go:98-108`) has neither field; `error` is omitted when empty.
3. **`completed_at` "null until it ends"** (api.md). Code sets it for `completed`, `failed` (a retry-pending, non-final state) and `dead_letter`, but leaves it null for `cancelled` (`api.go:871`).
4. **`hub.rebuild.followup`** (api.md Hub Audit Events: resource type `rebuild`, metadata `submitted_by`, slug). Code: resource type `patch`, metadata `stale_patches`, `upstream_stale`, `follow_up_job_id` (`rebuild_executor.go:657` via `emitRebuildAudit`, which sets `ResourceType: "patch"`, `carrypatch.go`).
5. **Rerere list "newest first"** (api.md GET rerere). Code lists in directory-name order (`os.ReadDir`, `rerere_handlers.go:139`), no time sort.
6. **Rerere id "40 or 64 hex characters"** (api.md DELETE rerere). Code accepts any single path component naming an existing rr-cache subdirectory (`rerere_handlers.go:220`).
7. **`conflict_files` "present only while the patch has unresolved conflicts"** (api.md Patch Response Schema). Code shows it whenever the stored array is non-empty; it is not cleared by a successful rebuild or by `PATCH status` (`patch_store.go:42`; `PATCH` never writes `conflict_files`, `patch_handlers.go` handleUpdatePatch). → **Resolved in this PRD by FIX-10:** the re-implementation follows the documented behaviour described here (see §1.8).
8. **`patches_synced` actions** (carry_patch_workflow.md lists only `created`/`fast_forwarded`/`replaced`). Code also emits `none` entries for every unchanged/missing/diverged candidate (`patch_refresh.go:154ff` refreshOnePatch; api.md is correct).
9. **Rebuild submit payload example / "Without a body the settings apply"** (api.md shows payload `{workspace_slug, strategy}`). Code payload also has `submitted_by`, `integration_branch`, `fail_mode` (`carrypatch.go:100`, `wire.go:552`).
10. **Patch-status `last_rebuild.status` values** (api.md: `queued`, `running`, `completed`, `failed`). Code passes the latest job's status, so `dead_letter` and `cancelled` also occur, and `integration_head_sha` is `""` whenever the latest job has no result even if an earlier rebuild succeeded (`api.go:993-1077`).
11. **Batch add ordering** (api.md: "A branch_name given twice in one batch is 409" listed with syntax checks). Code checks in-batch duplicates only after all branch resolutions (`patch_handlers.go:300-332`), so resolution side effects (created local branches) happen before the 409.
12. **Single add validation order** (api.md implies `position` validated with the other constraints). Code checks `position < 1` after branch resolution and the duplicate/`if_not_exists` check (`patch_handlers.go:195-232`).
13. **Workspace Lock list "git pushes take turns"** (carry_patch_workflow.md "What can run alongside a rebuild"). Code: pushes through the git server and the post-push mirror take no workspace lock; only rollback, rerere forget, sync, reset, branch resolution writes and the rebuild phases do (`push_control.go`, `push_mirror.go`).
14. **Post-push trigger "a registered patch branch"** (api.md/workflow). Code matches any patch row, including `deleted` ones, and does not exclude the integration branch (`push_hook.go:84-95`).
15. **CLI comment/docs on rebuild cancel "server returns 204"** (cli code comment `cli/rebuild_cmd.go:203`). Server returns 200 `{"status":"cancelled"}` (`api.go:699`); the CLI ignores it and prints its own document (docs/cli.md describes the CLI output correctly).
16. **Purge of soft-deleted patches**: docs (workflow "Soft delete and restore") correctly say no purge runs; code has `PurgeExpiredDeletedPatchesWithRefs` (`purge_refs.go`) but no caller in `cmd/parleyd/main.go` — consistent, recorded for completeness. → **Resolved in this PRD by FIX-12:** the re-implementation follows the corrected (the purge runs hourly) behaviour described here (see §1.8).


---

## 8. Secrets and variables

### 8.1 Purpose and scope

The hub keeps named key/value pairs for three kinds of owner: a user, an organization and a workspace. **Secrets** are write-only (their values never leave the hub through any API response) and are consumed by the hub itself: git credentials for clone/fetch/push and forge API calls, the forge webhook secret, and secrets injected into sandboxes. **Variables** have the same storage, rules and scopes but their values are readable, and a workspace's effective variables can be read merged across the three tiers. In scope: the 25 REST routes, validation, storage, access rules, resolution chains, audit events and the `parley secrets` / `parley vars` CLI. Out of scope: how sandboxes use secrets (section on sandboxes), workspace settings (formerly variables, see 8.7), and how git credentials are used by clone/push (workspace section).

### 8.2 User stories

- **US-SEC-1** As a Developer, I want to store a git PAT for my workspace (`GIT_PAT`), so that the hub can clone, push and call the forge on my behalf.
  - `POST /api/v1/workspaces/:slug/secrets` with `{"entries":[{"key":"GIT_PAT","value":"..."}]}` answers 201 with `[{key, created_at, updated_at}]` and no value.
  - Listing secrets never returns a value.
  - A value can be piped in via `parley secrets create GIT_PAT --from-stdin --workspace <slug>` so it stays out of shell history.
- **US-SEC-2** As an Org member, I want to store secrets/variables at the organization level, so that every workspace of the org shares them.
  - Members of the org (and admin tokens) may create, list, update and delete org entries; non-members get 404 `not found`.
- **US-SEC-3** As an Agent (workspace-bound PAT with `vars:read`), I want to read the effective variables of my workspace, so that I can configure myself.
  - `GET /api/v1/workspaces/:slug/vars/resolved` merges user (workspace owner's) < org < workspace tiers, each entry carrying `origin`.
  - A bound token can only address its own workspace; user/org routes answer 403 `token_bound`.
- **US-SEC-4** As an Org member who is not the workspace owner, I want to read a workspace's variables but not its secrets.
  - Member: workspace variables list/resolved 200; variable write 403 `forbidden`; any workspace secret route 404 `workspace not found`.
- **US-SEC-5** As an Operator (admin token), I want to manage org and workspace entries of any owner, but user-scoped routes reject me because an admin token has no user.
  - Admin on `/user/...` routes: 403 `user-scoped endpoints require a user credential; admin tokens have no user identity`.
- **US-SEC-6** As an auditor, I want every mutation recorded without values.
  - Each successful create (one per entry), update and delete emits `hub.secret.<op>` / `hub.variable.<op>` with `scope` and `key` only.

### 8.3 Concepts and data model

**Owner tiers (scopes)**

| `owner_type` | Route prefix | `owner_id` stored | Who may access |
|---|---|---|---|
| `user` | `/api/v1/user` | the caller's user id (`auth.UserID`) | any credential with a user identity (API key, PAT); admin tokens refused |
| `org` | `/api/v1/orgs/:slug` | the org's **id** (resolved from slug via `orgs.slug`) | members of the org (`org_members`), admin tokens |
| `workspace` | `/api/v1/workspaces/:slug` | the workspace **slug** (not an id) | workspace access rule (see SEC-FR-010) |

**Entities.** Two SQLite tables in the main hub database, identical shape, created at every start (`CREATE TABLE IF NOT EXISTS`):

| Table | Column | Type | Meaning / constraint |
|---|---|---|---|
| `secrets`, `variables` | `owner_type` | TEXT NOT NULL | CHECK in (`user`,`org`,`workspace`) |
| | `owner_id` | TEXT NOT NULL | see tier table |
| | `key` | TEXT NOT NULL | stored with the casing given at create |
| | `value` | TEXT NOT NULL | **standard base64 (RFC 4648, padded) of the raw value bytes; not encrypted** |
| | `created_at` | TEXT NOT NULL | RFC 3339 UTC, whole seconds, `Z` suffix (e.g. `2026-11-10T09:00:00Z`) |
| | `updated_at` | TEXT NOT NULL | same format; equals `created_at` on create |
| | PK | `(owner_type, owner_id, key)` | uniqueness is additionally enforced case-insensitively by the application |

**Response objects**

Secret entry (never has `value`):
```json
{"key": "GIT_PAT", "created_at": "2026-11-10T09:00:00Z", "updated_at": "2026-11-10T09:00:00Z"}
```
Variable entry:
```json
{"key": "MY_VAR", "value": "plain text", "created_at": "...", "updated_at": "..."}
```
Resolved variable entry:
```json
{"key": "MY_VAR", "value": "...", "origin": "user|org|workspace", "created_at": "...", "updated_at": "..."}
```

**Limits (constants)**

| Rule | Value |
|---|---|
| Key charset | ASCII letters `a-z A-Z`, digits `0-9`, `_` only |
| Key first char | not a digit (underscore allowed) |
| Key length | 1..255 bytes |
| Value size | ≤ 262,144 bytes (256 KiB), counted in bytes; empty string allowed |
| Entries per scope | ≤ 100 per `(owner_type, owner_id)` per table (secrets and variables counted separately) |
| Key matching | case-insensitive (ASCII upper-casing) for uniqueness, update, delete and internal lookups; stored casing preserved and returned |

**Reserved (meaningful) secret names.** Any valid key may be created; the hub never blocks a name. The following **workspace-scope** secret names are read by the hub itself (a user's or org's secret of the same name is never read for these purposes):

| Name | Used for |
|---|---|
| `GIT_PAT` | Origin credential: HTTP basic auth username `x-token-auth`, password = PAT. Used for clone, fetch, push, merge/rebase fetches, branch push, carry-patch origin operations, and as the forge API token (credential kind `pat`). |
| `GIT_USERNAME` + `GIT_PASSWORD` | Origin credential when `GIT_PAT` absent: basic auth. Forge API gets the password as token (credential kind `basic`). Username without password → logged warning, treated as no credential. |
| `UPSTREAM_GIT_PAT` | Upstream credential of a `carry_patch` workspace (username `x-token-auth`). |
| `UPSTREAM_GIT_USERNAME` + `UPSTREAM_GIT_PASSWORD` | Upstream basic auth when `UPSTREAM_GIT_PAT` absent. Half-set → falls back to origin credential. |
| `FORGE_WEBHOOK_SECRET` | Webhook secret for `POST /hooks/forge/:slug` (GitHub HMAC key / GitLab token). See §9. |

Credential resolution (looked up on every use, never cached):
- origin: `GIT_PAT` → else `GIT_USERNAME`+`GIT_PASSWORD` → else none (public).
- upstream: `UPSTREAM_GIT_PAT` → else `UPSTREAM_GIT_USERNAME`+`UPSTREAM_GIT_PASSWORD` → else the origin resolution.
- A store failure (other than not-found) during resolution is an error (propagated, not treated as "no credential").

`parley workspace create --git-pat` stores `GIT_PAT`; `--git-username/--git-password` store `GIT_USERNAME`/`GIT_PASSWORD` (workspace section).

**Sandbox injection.** A sandbox profile's `secrets` (required) and `optional_secrets` are looked up by name (case-insensitively) along the chain **workspace → org (only if the workspace has an org AND the workspace owner is a member of it) → workspace owner's user scope**; the first scope that has the name wins. The user is always the workspace owner, never the caller. Values go only into the sandbox environment, under the name as the profile spells it (details in the sandbox section).

**Variable resolution** (`/vars/resolved`): tiers merged lowest→highest priority: owner's `user` tier, then `org` tier, then `workspace` tier; a key (compared case-insensitively) in a higher tier replaces the lower one entirely (its `key` casing, `value`, timestamps and `origin`). The org tier is included when the workspace has an org and (the caller is an admin token OR the workspace owner is currently a member of that org).

### 8.4 Functional requirements

- **SEC-FR-001** Every one of the 25 routes requires an authenticated credential; without one → 401 `authentication required` (normally produced by the API auth middleware before the handler).
- **SEC-FR-002** Order of checks in every handler: (1) credential present; (2) if the credential is a PAT, the resource scope (read: `secrets:read` or `secrets:write` / `vars:read` or `vars:write`; write: `secrets:write` / `vars:write`) else 403 `insufficient permission scope`; (3) the tier owner (SEC-FR-008..010); (4) the operation (body parse, validation, store). API keys and admin tokens are never scope-checked.
- **SEC-FR-003** Before the handler, the workspace-binding guard applies to bound PATs: on `/user/...` and `/orgs/:slug/...` routes → 403 `this token is bound to workspace <slug>` with `error_type: token_bound`; on `/workspaces/:slug/...` with a different slug → 404 `workspace not found` (both also recorded as `hub.token.bound_refused`). On its own workspace a bound token proceeds normally.
- **SEC-FR-004** Create validation, in order: (a) body must bind as JSON (`{"entries":[...]}`) else 400 `invalid request body`; (b) `entries` non-empty else 400 `entries array must not be empty`; (c) for each entry in order: key empty → `key must not be empty`; key > 255 → `key "<k>" exceeds maximum length of 255 characters`; first char digit → `key "<k>" must not start with a digit`; invalid char → `key "<k>" contains invalid character "<c>"; only alphanumeric and underscore allowed`; value > 262144 bytes → `value size <n> bytes exceeds maximum of 262144 bytes (256 KB)`; key duplicates (case-insensitive) an earlier entry of the same request → `duplicate key "<k>" in entries (case-insensitive)`; all 400. A missing `value` in an entry is the empty string. (d) in one transaction: if existing count + number of entries > 100 → 400 `maximum of 100 entries per scope exceeded`; (e) for each entry in order, an existing key ignoring case → 409 `key "<k>" already exists` (k as given in request); (f) insert all with the same `created_at=updated_at=now`. All-or-nothing.
- **SEC-FR-005** Update (`PATCH .../:key`): body `{"value": string}`; unbindable body → 400 `invalid request body`; `value` absent or `null` → 400 `value field is required`; `""` is a valid value; value too large → 400 (message as SEC-FR-004); key looked up ignoring case → 404 `key "<k>" not found` (k as in path). On success the stored key casing and `created_at` are kept, `updated_at` = now. The path key is not validated against the naming rules.
- **SEC-FR-006** Delete: key matched ignoring case; no row → 404 `key "<k>" not found`; success 204 empty body.
- **SEC-FR-007** List: entries of the scope sorted ascending by upper-cased key; empty scope → `[]`. Secrets lists contain no value; variable lists contain the decoded value.
- **SEC-FR-008** User tier: a credential with empty user id (admin token) → 403 `user-scoped endpoints require a user credential; admin tokens have no user identity` (no error_type). Owner id = caller's user id.
- **SEC-FR-009** Org tier: org slug unknown → 404 `not found`; caller not a member (non-admin) → 404 `not found`; admin tokens bypass membership. Owner id = org id.
- **SEC-FR-010** Workspace tier uses the workspace access rule (credential types: admin → `admin`; user id == owner → `owner`; member of the workspace's org → `member`; else none):
  - unknown slug, no access, or bound to another workspace → 404 `workspace not found`;
  - secrets (all 4 ops, "private" need): owner and admin only; a member → 404 `workspace not found` (same body as unknown workspace);
  - variables read (list, resolved): owner, admin, member;
  - variables write (create/update/delete): owner and admin; member → 403 `org members have read-only access to this workspace` with `error_type: forbidden`;
  - DB failure → 500 `internal server error`.
  - The workspace's status (e.g. `archived`) does not matter.
- **SEC-FR-011** Store/DB failures → 500 `internal server error` (the cause is never sent).
- **SEC-FR-012** Secret values are returned by no route. Only internal consumers (credential resolution, webhook verification, sandbox injection) read them.
- **SEC-FR-013** Storage: values are base64-encoded (standard alphabet, padded) in the `value` column. There is **no encryption at rest**, no encryption key and no config/env var for one.
- **SEC-FR-014** Deleting a workspace deletes all its `workspace`-tier secrets and variables in the same transaction. Deleting a user or org does **not** delete their entries (they become unreachable orphans). Re-creating a workspace with the same slug starts with an empty workspace tier (rows were deleted).
- **SEC-FR-015** Resolved variables: response is sorted by upper-cased key; empty → `[]`. A member of the org gets exactly the owner's document (user tier is the owner's, not the caller's).
- **SEC-FR-016** Audit: one event per created entry (in request order) after a successful create, one per successful update/delete; none on failure; audit failures are logged and never change the response.
- **SEC-FR-017** The literal path segment `resolved` under `/workspaces/:slug/vars/` is reserved only for GET: `PATCH`/`DELETE /workspaces/:slug/vars/resolved` address a variable named `resolved` like any other key. `GET .../vars/:key` and `GET .../secrets/:key` do not exist (405).

### 8.5 REST API

Common error envelope (all routes): `{"error":{"code":<int>,"message":"<text>"}}`, plus `"error_type"` only where stated (`forbidden`, `token_bound`). Timestamps RFC 3339 UTC whole seconds. All paths under `/api/v1`. The API group enforces `Content-Type: application/json` on POST/PUT/PATCH with a non-empty body (415 `unsupported media type`) and the server body size limit (413 `payload too large`).

The 25 routes:

| # | Method | Path |
|---|---|---|
| 1-4 | POST, GET / PATCH, DELETE | `/user/secrets`, `/user/secrets/:key` |
| 5-8 | POST, GET / PATCH, DELETE | `/user/vars`, `/user/vars/:key` |
| 9-12 | POST, GET / PATCH, DELETE | `/orgs/:slug/secrets`, `/orgs/:slug/secrets/:key` |
| 13-16 | POST, GET / PATCH, DELETE | `/orgs/:slug/vars`, `/orgs/:slug/vars/:key` |
| 17-20 | POST, GET / PATCH, DELETE | `/workspaces/:slug/secrets`, `/workspaces/:slug/secrets/:key` |
| 21-24 | POST, GET / PATCH, DELETE | `/workspaces/:slug/vars`, `/workspaces/:slug/vars/:key` |
| 25 | GET | `/workspaces/:slug/vars/resolved` |

#### `POST /user/secrets` · `POST /orgs/:slug/secrets` · `POST /workspaces/:slug/secrets`
- **Purpose:** create one or more secrets in one scope.
- **Auth / scope:** any credential; PAT needs `secrets:write`. User tier: user credential only. Org tier: member or admin. Workspace tier: owner or admin (member/stranger 404). Bound PAT: workspace tier of its own workspace only.
- **Path params:** `slug` (org slug / workspace slug).
- **Request body:** `{"entries":[{"key":string,"value":string}, ...]}`; `entries` required non-empty; `value` optional (default `""`); unknown fields ignored. Validation per SEC-FR-004.
- **Behaviour:** SEC-FR-002 checks → validate → transaction: count limit → per-key conflict → insert.
- **Response:** 201, JSON array of secret entries in request order: `[{"key","created_at","updated_at"}]` (key as given).
- **Errors:** 401 `authentication required`; 403 `insufficient permission scope` (PAT); 403 admin on user tier; 404 `not found` (org unknown/non-member); 404 `workspace not found`; 400 `invalid request body` / validation messages / `maximum of 100 entries per scope exceeded`; 409 `key "<k>" already exists`; 500 `internal server error`.
- **Side effects:** rows inserted; one `hub.secret.create` per entry.

#### `POST /user/vars` · `POST /orgs/:slug/vars` · `POST /workspaces/:slug/vars`
- As the secrets create, with: PAT scope `vars:write`; workspace tier: owner or admin, a member gets 403 `org members have read-only access to this workspace` (`error_type: forbidden`), stranger 404.
- **Response:** 201, `[{"key","value","created_at","updated_at"}]` in request order (value = plaintext as sent).
- **Side effects:** one `hub.variable.create` per entry.

#### `GET /user/secrets` · `GET /orgs/:slug/secrets` · `GET /workspaces/:slug/secrets`
- **Auth / scope:** PAT `secrets:read` or `secrets:write`; tier rules as create (workspace: owner/admin only).
- **Query params:** none.
- **Response:** 200, array of secret entries sorted by upper-cased key; `[]` if none. Never contains values.
- **Errors:** 401; 403 scope; 403 admin on user tier; 404 org/workspace; 500.

#### `GET /user/vars` · `GET /orgs/:slug/vars` · `GET /workspaces/:slug/vars`
- **Auth / scope:** PAT `vars:read` or `vars:write`; workspace tier: owner, admin, **member**.
- **Response:** 200, array of variable entries (decoded `value`) sorted by upper-cased key; `[]` if none.
- **Errors:** 401; 403 scope; 403 admin on user tier; 404; 500 (also if a stored value is not valid base64).

#### `PATCH /user/secrets/:key` · `PATCH /orgs/:slug/secrets/:key` · `PATCH /workspaces/:slug/secrets/:key`
- **Auth / scope:** PAT `secrets:write`; tier rules as create.
- **Path params:** `key` (URL-decoded; matched ignoring case; not validated).
- **Request body:** `{"value": string}` required (null = missing).
- **Response:** 200 `{"key":<stored casing>,"created_at":<original>,"updated_at":<now>}`.
- **Errors:** 401; 403; 404 tier; 400 `invalid request body`; 400 `value field is required`; 400 value too large; 404 `key "<k>" not found`; 500.
- **Side effects:** value and `updated_at` replaced; `hub.secret.update` (metadata `key` = path key as given).

#### `PATCH /user/vars/:key` · `PATCH /orgs/:slug/vars/:key` · `PATCH /workspaces/:slug/vars/:key`
- As secrets update; PAT `vars:write`; workspace member → 403 `forbidden`.
- **Response:** 200 `{"key":<stored casing>,"value":<new>,"created_at","updated_at"}`.
- **Side effects:** `hub.variable.update`.

#### `DELETE /user/secrets/:key` · `DELETE /orgs/:slug/secrets/:key` · `DELETE /workspaces/:slug/secrets/:key`
- **Auth / scope:** PAT `secrets:write`; tier rules as create.
- **Request body:** none (ignored).
- **Response:** 204, no body.
- **Errors:** 401; 403; 404 tier; 404 `key "<k>" not found`; 500.
- **Side effects:** row deleted; `hub.secret.delete`. Deleting `FORGE_WEBHOOK_SECRET` disables the webhook ingress for that workspace; deleting `GIT_PAT` etc. changes the next git/forge operation.

#### `DELETE /user/vars/:key` · `DELETE /orgs/:slug/vars/:key` · `DELETE /workspaces/:slug/vars/:key`
- As secrets delete; PAT `vars:write`; workspace member → 403 `forbidden`. Side effect `hub.variable.delete`.

#### `GET /workspaces/:slug/vars/resolved`
- **Purpose:** the variables a workspace runs with, merged across tiers.
- **Auth / scope:** PAT `vars:read` or `vars:write`; workspace read access (owner, admin, member); bound PAT only on its own workspace.
- **Behaviour:** (1) 401 if no credential; (2) PAT scope → 403 `insufficient permission scope`; (3) workspace access (read need) → 404 `workspace not found` / 500; (4) org tier used iff workspace `org_id` non-empty and (caller is admin token or the workspace **owner** is a member of that org); (5) merge user(owner) < org < workspace by upper-cased key.
- **Response:** 200, `[{"key","value","origin","created_at","updated_at"}]` sorted by upper-cased key; `[]` if none. `origin` ∈ `user`,`org`,`workspace`.
- **Errors:** 401; 403 `insufficient permission scope`; 404 `workspace not found`; 500 `internal server error`.
- **Side effects:** none.

### 8.6 CLI

Common behaviour of all `parley secrets` / `parley vars` commands:
- Successful output: the hub's JSON answer pretty-printed (2-space indent, no HTML escaping) to stdout, one document per request.
- Errors: a JSON envelope `{"error":{"code":<int>,"message":...}}` on stdout. Hub error → code = HTTP status, exit 1. Usage error → code 2, exit 2, nothing sent. Network/connection failure → code 2, exit 2. The hub's `error_type` is not carried into the CLI envelope.
- Cobra-level errors (unknown flag) → envelope with `code: 0` and cobra's message, exit 2.
- Scope flags: `--user` (bool), `--org <slug>` (string), `--workspace <slug>` (string). No flag → user scope. An empty string value counts as not given. Slugs and keys are percent-encoded as single path segments.
- `list`/`update`/`delete`: more than one scope flag → usage error `only one of --user, --org, --workspace may be specified`.
- `create`: several scope flags allowed; requests are sent in fixed order user → org → workspace, each printed in turn; a failing scope prints its error envelope and the remaining scopes are still tried; afterwards the command exits 1. **[FIX-13]** Nothing else is printed: each failed scope's envelope appears exactly once. The baseline printed one more envelope, `{"error":{"code":1,"message":""}}`. This applies to `parley vars create` too.
- Extra positional arguments beyond those used are ignored (no arity check).

#### `parley secrets create <KEY=VALUE[,KEY2=VALUE2,...]> [KEY3=VALUE3 ...]` / `parley secrets create <KEY> --from-stdin`
- **Arguments / flags:** one or more args, each a comma-separated list of `KEY=VALUE` (split on first `=`; value may contain `=`; a comma always separates entries, so values cannot contain commas); `--user`, `--org`, `--workspace` (multi); `--from-stdin` (bool): exactly one bare KEY arg, value read from stdin with one trailing `\n` and then one trailing `\r` removed.
- **Calls:** `POST /api/v1/<scope>/secrets` with `{"entries":[{"key","value"}...]}` per scope.
- **Output:** each scope's 201 array.
- **Exit codes / usage errors (exit 2):** no args → `required argument: KEY=VALUE`; token without `=` → `invalid argument: missing '=' in '<arg>'`; empty or whitespace token or key → `empty key in argument at position <i>` (0-based position within that argument); `--from-stdin` with ≠1 arg or an arg containing `=` or `,` → `--from-stdin expects exactly one KEY argument`; `--from-stdin` with blank key → `empty key`; stdin read failure → `failed to read value from stdin: ...`. Key naming rules are checked only by the hub.

#### `parley secrets list`
- **Flags:** one scope flag at most.
- **Calls:** `GET /api/v1/<scope>/secrets`. **Output:** the array (keys and timestamps). Exit 0/1/2.

#### `parley secrets update <KEY=VALUE>` / `parley secrets update <KEY> --from-stdin`
- **Arguments / flags:** first arg only; one scope flag at most; `--from-stdin` reads the value as in create.
- **Calls:** `PATCH /api/v1/<scope>/secrets/<KEY>` with `{"value": ...}`.
- **Output:** the updated entry.
- **Usage errors:** no arg → `required argument: KEY=VALUE`; missing `=` → `invalid argument: missing '=' in '<arg>'`; `--from-stdin` with `=` → `--from-stdin expects a bare KEY argument`; blank key with `--from-stdin` → `empty key`; >1 scope flag. (Arg parsing happens before the scope check.)

#### `parley secrets delete <KEY>`
- **Calls:** `DELETE /api/v1/<scope>/secrets/<KEY>`.
- **Output:** nothing on stdout; `Secret '<KEY>' has been deleted.` on stderr.
- **Usage errors:** no arg → `required argument: KEY`; >1 scope flag.

#### `parley vars create|list|update|delete`
- Identical to the secrets commands with `/vars` paths, PAT scopes `vars:*`, variable entries (with values) in the output, and delete message `Variable '<KEY>' has been deleted.` on stderr.

#### `parley vars resolve <workspace-slug>`
- **Arguments / flags:** slug required; no scope flags (passing one is an unknown-flag error, exit 2); args after the first ignored.
- **Calls:** `GET /api/v1/workspaces/<slug>/vars/resolved`.
- **Output:** the resolved array.
- **Usage errors:** no arg → `required argument: workspace slug`.

### 8.7 Background behaviour

- At every start: `secrets` and `variables` tables created if missing.
- At start, PAT scope migration rewrites stored PAT permission names: `secrets:list`→`secrets:read`, `secrets:manage`→`secrets:write`, `secrets:delete`→`secrets:write`, `vars:manage`→`vars:write`, `vars:delete`→`vars:write` (duplicates collapsed).
- At start (one-time effect), legacy behaviour-switch **workspace variables** are moved into the workspace `settings` document and their variable rows deleted. Keys (matched case-insensitively): `PATCH_BRANCH_SOURCE`, `PATCH_DIVERGENCE_POLICY`, `PUSH_PATCHES_TO_ORIGIN`, `AUTO_REBUILD_AFTER_PUSH`, `AUTO_REBUILD_AFTER_SYNC`, `REBUILD_STRATEGY`, `REBUILD_FAIL_MODE`, `REBUILD_PUSH_INTEGRATION_BRANCH`, `SQUASH_MERGE_DETECTION`, `FORGE_API_URL`, `UPSTREAM_FORGE_API_URL` → settings `patch_branch_source` … `upstream_forge_api_url`. A value a setting cannot take is dropped with a warning. After this, variables never change hub behaviour.
- No workers, queues or schedules otherwise.

### 8.8 Events

| Event type | resource_type | action | When | Fields |
|---|---|---|---|---|
| `hub.secret.create` | `secret` | `create` | per entry of a successful create | `actor_id` = caller user id (`""` for admin), `actor_type` = credential type; `resource_id` empty; `workspace` empty; metadata `{"scope": "user"|"org"|"workspace", "key": <key as in request>}` |
| `hub.secret.update` | `secret` | `update` | successful update | as above, `key` = path key as given |
| `hub.secret.delete` | `secret` | `delete` | successful delete | as above |
| `hub.variable.create` / `.update` / `.delete` | `variable` | `create`/`update`/`delete` | same | same metadata |

Values never appear. Owner id (user/org id, workspace slug) is not recorded. The forge webhook routes write `FORGE_WEBHOOK_SECRET` directly and emit `hub.forge.webhook.*` instead (no `hub.secret.*`).

### 8.9 Configuration

None. No config key or env var for secrets/variables (no encryption key exists). PAT scopes registered: `secrets:read`, `secrets:write`, `vars:read`, `vars:write` (write implies read at the handler level).

### 8.10 Edge cases and error handling

- Create body with duplicate keys differing only in case → 400 before any DB access; a key existing in the scope with different case → 409.
- Count limit checked before conflict: a full scope answers 400 even when the key also conflicts.
- Values are bytes: multi-byte UTF-8 counts by bytes toward 256 KiB.
- `PATCH` with `{"value":""}` sets an empty value (allowed). An empty `FORGE_WEBHOOK_SECRET` is treated as "no webhook secret" by the ingress; an empty `GIT_PAT` yields credential kind `pat` with no token sent.
- Admin token on resolved: org tier included even if the owner left the org (non-admin callers lose it).
- Workspace tier on an archived workspace works.
- Bound token with wrong scope on another workspace gets 404 (guard runs before the scope check).
- A member requesting a workspace secret receives a body byte-identical to the unknown-workspace 404.
- A non-JSON `Content-Type` with a body → 415 from the API group; `Bind` failure → 400 `invalid request body`.
- CLI create values cannot contain `,` except via `--from-stdin`.

### 8.11 Discrepancies between docs and code

1. **Resolved variables, admin token.** `docs/api.md` (GET .../vars/resolved) and `docs/permissions.md` (vars:read note) say the org tier is used only while the owner is a member. Code includes the org tier for an admin token regardless: `internal/secrets/handlers.go:371` (`orgID != "" && !wsaccess.IsAdmin(auth) && !isOrgMember(...)`); confirmed by `hub/internal/secrets/handlers_test.go:308`.
2. **Audit event fields.** `docs/api.md` Hub Audit Events says each event has the `workspace`; secret/variable events carry no `workspace` and no `resource_id` even for the workspace tier (`internal/secrets/handler_audit_config.go:43-55`).
3. **CLI create partial failure output.** `docs/cli.md` (parley secrets create) says a failed scope prints its error and exits 1; the real binary additionally prints a final `{"error":{"code":1,"message":""}}` because `RunE` returns an unprinted `NewCLIError(1,"")` that `cmd/parley/main.go:23` prints (`internal/cli/secrets_cmd.go:79-81`). → **Resolved in this PRD by FIX-13:** the re-implementation follows the documented behaviour described here (see §1.8).
4. **Encryption (not a discrepancy, recorded for clarity).** No document claims encryption at rest; code and `docs/architecture.md:863` / `docs/openapi.yaml:2111` agree values are only base64-encoded (`internal/secrets/validation.go:68-79`). A re-implementation must not add encryption if it is to be behaviour-equal on the stored table.
5. `docs/api.md` PATCH errors list only `value missing` for 400; code also answers 400 for unparseable body and oversize value (`handlers.go:300-309`, `store.go:127`).

---

## 9. Forge integration

### 9.1 Purpose and scope

The hub is the forge client of each workspace: callers read and write the issues, labels, comments and pull requests of the workspace's repository on GitHub or GitLab **through the hub**, using the credential the hub stores for the workspace (never the caller's own). The hub also receives the forge's webhook deliveries for a workspace, verifies them against a per-workspace secret, records each once in a delivery log and relays them as `hub.forge.*` audit events (it never acts on them). In scope: forge detection, credentials, the diagnostic route, 9 read routes, 9 write routes, 3 webhook-management routes, the public ingress, error mapping, retention, events and the `parley forge|issue|pr|label` CLI. Out of scope: git push of branches (workspace section), registering the webhook on the forge (the hub never does; a person enters the URL and secret in the forge UI), Forgejo/Gitea.

### 9.2 User stories

- **US-FRG-1** As an Agent with a `forge:read` PAT, I want to list and read issues and PRs of my workspace's repo, so that I can triage without forge credentials.
  - `GET .../issues?labels=af:triage` returns a bare array; reads work on archived workspaces; no event is recorded.
- **US-FRG-2** As an Agent with `forge:write`, I want to create issues, comment, label, and open PRs with the workspace's stored token.
  - Each write that reached the forge records exactly one `hub.forge.*` event, success or failure.
  - Body-like fields over the forge limit are refused with 413 before contacting the forge.
- **US-FRG-3** As a Developer, I want `parley forge status` to explain how the hub reaches the forge (kind, API URL, repository, credential kind, webhook state) without contacting it.
- **US-FRG-4** As a Developer, I want to set a webhook secret (generated or mine) and get the URL to enter in the forge.
  - A generated secret is shown exactly once (in the PUT response).
- **US-FRG-5** As the Forge, I post deliveries to `POST /hooks/forge/:slug`; valid ones are acknowledged with 202 quickly and relayed once; redeliveries answer `duplicate`.
- **US-FRG-6** As a Developer, I want `parley forge deliveries` to show what the hub received in the last 7 days and what it made of each.
- **US-FRG-7** As an Org member, I may read the forge through the workspace's credential but not write (403 `forbidden`).
- **US-FRG-8** As a carry-patch Developer, I want `--target upstream` to read/write the upstream repository with the upstream credential.

### 9.3 Concepts and data model

**Remote target.** `?target=` selects which repository a route works on:

| Value | Remote URL | Credential | API URL setting |
|---|---|---|---|
| absent or `origin` | workspace `git_url` | origin chain (`GIT_PAT` → `GIT_USERNAME`+`GIT_PASSWORD` → none) | workspace setting `forge_api_url` |
| `upstream` | workspace `upstream_url`; only when `workspace_mode = carry_patch` and `upstream_url` non-empty | upstream chain (`UPSTREAM_GIT_PAT` → `UPSTREAM_GIT_USERNAME`+`UPSTREAM_GIT_PASSWORD` → origin chain) | workspace setting `upstream_forge_api_url` (no fallback to `forge_api_url`) |

A NULL/empty `workspace_mode` is `standard`. Workspace settings are read by their legacy variable names `FORGE_API_URL` / `UPSTREAM_FORGE_API_URL`; an empty setting is "not set".

**Repository parsing** from the remote URL (case-folded host, `www.` stripped, port dropped, `.git` suffix and surrounding `/` stripped, userinfo ignored). Accepted forms: `https://`, `http://`, `ssh://`, `git://` URLs, and scp-style `user@host:owner/name`. Path must have ≥ 2 non-empty segments; `name` = last segment, `owner` = all preceding segments joined by `/` (GitLab subgroups kept: `group/subgroup`). Any host is accepted for parsing (if not a known forge host, a generic split is used).

**Forge kind detection** (in order):
1. If an API URL setting is set: contains `github` and not `gitlab` → GitHub; `gitlab` and not `github` → GitLab; both → ambiguous; neither → classify by the remote's host (contains `github` xor `gitlab`) → else ambiguous.
2. Else by the remote host: host matching the host (or `api.`+host) of env `GITHUB_API_URL` / `GITLAB_API_URL` (exactly one) → that forge; else host contains `github` xor `gitlab`; else ambiguous.
Ambiguous → 422 `forge_unknown`.

**API URL** (no setting): GitHub `github.com` → `https://api.github.com`; other GitHub host → `https://<host>/api/v3`; GitLab → `https://<host>/api/v4`. For **reads**, the forge client may instead use env `GITHUB_API_URL`/`GITLAB_API_URL` when it names the host; for **writes** the hub always pins the API URL explicitly (setting or the default above), so env API URLs are ignored for the target. A GitLab API URL not ending in `/api/v4` gets `/api/v4` appended. A setting is trimmed of surrounding whitespace and trailing `/`.

**API URL setting validity:** absolute `https` URL with a host, no userinfo, no `?`/`#`, no whitespace/control chars (≤ U+0020 or U+007F), not opaque. Invalid → 422 `forge_api_url_invalid`, message `<FORGE_API_URL|UPSTREAM_FORGE_API_URL> must be an absolute https:// URL with a host and without userinfo, query or fragment`.

**Credential kind** (from the resolved credential): basic-auth username `x-token-auth` → `pat` (token = password); any other username → `basic` (password sent as the token); none → `none` (no token sent). The hub's own `GITHUB_TOKEN`/`GH_TOKEN`/`GITLAB_TOKEN` are never sent: when the workspace has no token, `Authorization` and `PRIVATE-TOKEN` headers are stripped from every forge request.

**Outbound HTTP rules** (every forge request): `User-Agent: parleyd/<version>`; proxy settings from the environment (`HTTPS_PROXY`, `NO_PROXY`, ...); redirects followed only to the same host+port and scheme, at most 10, else the call fails (`502 forge_error`); 30-second deadline per call including the client's retries (a write's every individual request gets its own 30 s); responses read up to 8 MiB. Auth headers: GitHub `Authorization: Bearer <token>`, `Accept: application/vnd.github+json`, `X-GitHub-Api-Version: 2022-11-28`; GitLab `PRIVATE-TOKEN: <token>`. The forge client retries a rate-limited request once when the wait (Retry-After + 1 s, or reset time + 1 s) is ≤ 120 s; requests the hub sends itself (see FRG-FR-030) are not retried. Nothing is cached between requests.

**Result types** (JSON encodings; `?` = omitted when empty):

| Type | Fields |
|---|---|
| repository | `full_name`, `default_branch`, `private`, `archived`, `permissions{push,pull,admin}`, `allow_merge_commit?`, `allow_squash_merge?`, `allow_rebase_merge?` |
| issue | `number`, `title`, `body`, `state` (`open`/`closed`; GitLab `opened`→`open`), `url?`, `html_url?` (both the web URL), `author{login}`, `labels?` (names), `created_at`, `updated_at`, `closed_at?`, `is_pr` |
| issue thread | `issue`, `comments` (array, `[]` not null), `truncated` (bool), `comments_err?` |
| comment | `id?`, `body`, `user{login}`, `created_at`, `updated_at?`, `url?`, `html_url?` |
| pull request | `number`, `title`, `body`, `state`, `url?`, `html_url?`, `draft`, `merged`, `head_sha`, `head_branch`, `base_branch`, `author{login}`, `created_at`, `updated_at`, `closed_at?`, `merged_at?` |
| pull request state | `number`, `state` (`open`/`closed`/`merged`), `merged`, `head_sha` |
| changed file | `filename`, `status` (`added`/`modified`/`deleted`), `additions`, `deletions` |
| check run | `name`, `status`, `conclusion`, `summary?`, `url?` (GitLab: jobs of the latest MR pipeline; status `in_progress`/`completed`, conclusion `success`/`failure`/`cancelled`/`skipped`) |
| review | `author{login}`, `state`, `body`, `submitted_at` (GitLab: approvals as `approved` plus non-system notes as `commented`, sorted by time) |
| label | `name`, `color?` (GitHub without `#`, GitLab with), `description?` |

Note: Go's zero time encodes as `"0001-01-01T00:00:00Z"` for absent required timestamps.

**Delivery log** table `forge_deliveries` (main DB, created at every start):

| Column | Type | Meaning |
|---|---|---|
| `workspace_slug` | TEXT NOT NULL | PK part |
| `delivery_id` | TEXT NOT NULL | the forge's id; PK part |
| `forge` | TEXT NOT NULL | `github` / `gitlab` |
| `event` | TEXT NOT NULL | event header value, invalid UTF-8 replaced by U+FFFD, truncated to 64 bytes on a rune boundary |
| `action` | TEXT NOT NULL | payload action (GitHub `action`, GitLab `object_attributes.action`), `""` when none |
| `target` | TEXT NULL | `origin` / `upstream` / NULL |
| `outcome` | TEXT NOT NULL | `relayed`, `ignored`, `repository_mismatch`, `malformed` |
| `relayed` | TEXT NOT NULL | JSON array of relayed event types (`[]` unless relayed) |
| `received_at` | TEXT NOT NULL | RFC 3339 UTC, whole seconds |
| PK | `(workspace_slug, delivery_id)` | insert conflict ⇒ duplicate |
| Index | `idx_forge_deliveries_received (workspace_slug, received_at)` | |

Payloads and secrets are never stored.

**Delivery outcomes:** `relayed`, `ignored`, `repository_mismatch`, `malformed` (recorded); `duplicate` (answered only, never recorded).

**Webhook secret:** workspace-scope secret `FORGE_WEBHOOK_SECRET`; read on every delivery (rotation applies to the next delivery). Webhook URL = `server.external_url` with trailing `/` trimmed + `/hooks/forge/<slug>`; `null` when `external_url` is empty.

### 9.4 Functional requirements

**Access and order of checks**

- **FRG-FR-001** Credential acceptance: read routes — PAT with `forge:read` or `forge:write`, `api_key`, `admin_token`; write routes and webhook set/unset — PAT with `forge:write` (read alone is refused), `api_key`, `admin_token`. Any other credential type is refused.
- **FRG-FR-002** Shared checks for every `/api/v1/workspaces/:slug/...` forge route, in order:
  1. no credential → 401 `authentication required`;
  2. credential not allowed (FRG-FR-001) → 403, message `PAT requires forge:<read|write> scope` for a PAT, else `credential type "<type>" may not <read|write> a workspace's forge` — **before the workspace is looked up** (so also for unknown slugs);
  3. workspace access: unknown slug, no access, PAT bound to another workspace → 404 `workspace not found`; member of the workspace's org on a write/webhook route → 403 `org members have read-only access to this workspace` with `error_type: forbidden`; DB error → 500;
  4. writes and webhook `PUT` only: workspace `status == archived` → 400 `workspace is archived` (after the access check; `DELETE .../forge/webhook` exempt);
  5. reading the workspace remotes; vanished row → 404 `workspace not found`; error → 500.
- **FRG-FR-003** Read routes (except `GET .../forge` and `.../forge/deliveries`) then: (6) `target` (FRG-FR-004); (7) route parameters (`:n`, query) → 400; (8) build client (FRG-FR-006) → 422/500; (9) call the forge; failure mapped per FRG-FR-040.
- **FRG-FR-004** `target`: absent → origin; exactly one value `origin` → origin; exactly one value `upstream` → upstream if the workspace is `carry_patch` with non-empty `upstream_url`, else 400 `target=upstream needs a carry_patch workspace with an upstream_url` with `error_type: workspace_mode_mismatch`; anything else (other value, wrong case, empty, repeated) → 400 `target must be "origin" or "upstream"`. `GET .../forge`, `.../forge/deliveries` and the webhook routes ignore `target`.
- **FRG-FR-005** Write routes then: (6) target; (7) read full body (unreadable → 400 `the request body cannot be read`); (8) route-specific parse/validation, path number first (→ 400); (9) build client (→ 422/500); (10) forge-specific refusal (GitLab assignee-only update → 400); (11) body limits (→ 413); (12) send; (13) record event; (14) answer. A request refused at any step before (12) sends nothing and records no event.
- **FRG-FR-006** Client build order: (a) parse the remote; no repository → 422 `forge_unknown` `cannot read a forge repository from the remote URL "<redacted remote>" (expected https://<host>/<owner>/<name>)` (userinfo removed); (b) API URL setting: settings read failure → 500; invalid → 422 `forge_api_url_invalid`; (c) resolve credential; store failure → 500 (only the error type is logged); (d) detect forge; ambiguous → 422 `forge_unknown` with `cannot tell which forge serves <host>; set <VAR> to its API URL, an https URL that contains github or gitlab` (no setting) or `cannot tell which forge serves <host>: neither the host nor the API URL in <VAR> names exactly one of github and gitlab` (setting set), where `<VAR>` is `FORGE_API_URL` / `UPSTREAM_FORGE_API_URL`; other detection/client errors → 500.
- **FRG-FR-007** `:n` is a positive decimal integer (digits only, leading zeros allowed, no sign, fits int, > 0) else 400 `n must be a positive decimal integer`. On GitLab it is the project's `iid`.

**Reads**

- **FRG-FR-010** `GET .../issues` query: `state` ∈ `open|closed|all`; `sort` ∈ `created|updated`; `direction` ∈ `asc|desc` — each optional; if present must be given exactly once and be one of the values, else 400 `<name> must be one of: <values comma-space-joined>`. Checked in order state, sort, direction, then labels/assignee, then limit. `labels`: every occurrence split on `,`, items trimmed, empty dropped. `assignee`: first value, free text. `limit`: absent → 30; given more than once → 400 `limit must be given once`; not an integer 1..100 → 400 `limit must be an integer from 1 to 100`. Unknown parameters ignored.
- **FRG-FR-011** Absent `state` lists open issues (forge client default). GitHub list excludes pull requests and pages (100 per page) until `limit` issues are collected; GitLab maps `open`→`opened`, sort `updated`→`updated_at` else `created_at`, default direction `desc`. The answer is a bare array (`[]` when empty) and does not say whether it was cut short.
- **FRG-FR-012** `GET .../issues/:n` returns an issue thread: the issue plus up to 5 pages × 100 comments; `truncated` true when more pages existed. If the issue was read but comments failed, the answer is still 200 with `comments_err`: when the comments error is a plain forge HTTP error it is encoded as an object `{"Method","Path","Status","Message","RetryAfterHeader","RateLimitReset","RateLimitRemaining"}` with `Message` scrubbed (token → `[redacted]`, URL userinfo removed, ≤1024 bytes); any other error (including wrapped not-found/rate-limit/conflict and transport errors) encodes as `{}`. `comments` is `[]` in that case.
- **FRG-FR-013** `GET .../issues/:n/comments` returns the comments array (same 5-page cap; truncation not signalled).
- **FRG-FR-014** PR reads: `pulls/:n` (pull request), `/state`, `/files` (≤30 pages × 100), `/checks` (GitHub: check-runs of the head SHA; GitLab: jobs of the latest MR pipeline, `[]` if none), `/reviews`. Lists are bare arrays, `[]` when empty.
- **FRG-FR-015** Reads need no clone, take no workspace lock, record no events, and work on any workspace status.

**Writes** (shared body rules: request body must be a JSON object except where noted; a field that is absent or JSON `null` is "not set"; unknown fields ignored; type errors → 400 `<field> must be a string` / `<field> must be true or false` / `<field> must be an array of strings`; name lists ≤ 100 entries (`<field> must have at most 100 entries`), no blank entries (`<field> must not have a blank entry`); required strings: absent → `<field> is required`, whitespace-only → `<field> must not be blank`; non-object body → `the request body must be a JSON object`)

- **FRG-FR-020** Create issue: `title` required non-blank; `body` optional string (may be empty); `labels`, `assignees` optional name lists. Sent via the forge client (GitHub: `title`, `body`, `labels`/`assignees` only when non-empty; GitLab: `title`, `description`, `labels` comma-joined, **assignees never sent**). 201 with the issue.
- **FRG-FR-021** Update issue: path `:n` first; fields `title` (non-blank if set), `body` (`""` clears), `state` ∈ `open|closed` (else 400 `state must be open or closed`), `labels`/`assignees` (replace whole set; `[]` clears). No field set → 400 `nothing to update`. On GitLab: sent as `PUT` with `title`, `description`, `state_event` (`reopen`/`close`), `labels` comma-joined (`""` clears); assignees never sent; if `assignees` is the only field → 400 `nothing to update: GitLab takes assignees as user ids, which the hub does not send` (after client build, before limits). On GitHub: `PATCH` with exactly the set fields (lists sent as given, `[]` included). 200 with the issue as returned.
- **FRG-FR-022** Close issue: `:n` first; body may be empty/whitespace (no comment) or a JSON object with optional `comment` string. A non-blank comment is posted first (counts toward body limits as field `comment`); only if it succeeds is the issue updated to `state=closed`. If the comment fails → that error (issue untouched). If the close fails after the comment → the close's error, and the event carries `comment_url`. Closing an already closed issue → 200. Answer: 200 with the issue after the close.
- **FRG-FR-023** Comment on issue / PR: `:n` first; `body` required non-blank; posted on the issue / PR conversation (GitLab: MR notes); 201 `{"url": "<comment web URL>"}`.
- **FRG-FR-024** Add labels: `:n` first; body must be a bare JSON array of 1..100 non-blank strings (object, `null`, non-string → `labels must be an array of strings`; `[]` → `labels must have at least one entry`). Labels are added; the issue keeps its others; unknown label names are the forge's business. 204 no body.
- **FRG-FR-025** Remove label: `:n` first; the label is the **last path segment of the raw escaped request path, percent-decoded exactly once** (`type%2Fbug` → `type/bug`); undecodable or blank → 400 `the label name must be a name that is not blank`. GitHub: hub sends `DELETE /repos/<o>/<n>/issues/<n>/labels/<seg>` itself; GitLab: forge client removal. When the forge answers not-found, the hub reads the issue once (`GET` issue); issue exists → 204; issue missing → 404 `forge_not_found`. 204 also when the issue never had the label. Label path segments `.`/`..` are sent as `%2E`/`%2E%2E`.
- **FRG-FR-026** Ensure label (`POST .../labels`): `name` required non-blank; `color` optional, must match `^#?[0-9a-fA-F]{6}$` (else 400 `color must be six hex digits, with or without a leading #`), default `ededed`; `description` optional. Steps: GET label by name → found → 200 with the label as the forge holds it (never modified); not found → create (`color` without `#` on GitHub, with `#` on GitLab; `description` only if non-empty) → 201 with the created label; create failed → look up once more → found → 200; else the create's error. Lookup error other than not-found → that error.
- **FRG-FR-027** Open PR: `title`, `head`, `base` required non-blank (checked in that order, then `body`, `draft`); `body` optional; `draft` optional boolean, default false. `head` sent as given (`owner:branch` for forks). GitLab: `source_branch`=head, `target_branch`=base, `description`=body, draft expressed by prefixing title with `Draft: `. No idempotency: an existing PR for head/base is refused by the forge (422 `forge_rejected`). 201 with the pull request.
- **FRG-FR-028** Body limits (code points, after client build, first offending field wins): GitHub (and any non-GitLab) 65,536; GitLab 1,000,000. Fields measured: issue create `body`, issue update `body`, close `comment` (only when non-blank), comment `body`, PR `body`. Over → 413 `body_too_large`, message `<field> is <n> characters; <github|gitlab> accepts at most <limit>`. Titles, head/base, label names/descriptions, assignees are not measured.
- **FRG-FR-029** A write call continues when the caller disconnects (cancellation detached) and its event is still recorded; each forge request of a multi-request write has its own 30 s deadline.
- **FRG-FR-030** Requests the hub sends itself (no retry on rate limit): issue update and the close's state change (`PATCH` GitHub / `PUT` GitLab), GitHub label removal, the issue existence check, label lookup and label creation. Issue create, comments, add labels, PR create and GitLab label removal go through the forge client (one rate-limit retry).
- **FRG-FR-031** **[FIX-14]** Missing credential on a write: when the workspace's resolved credential kind is `none` (for the `target` in use), every write route refuses before sending anything. That covers forge-client writes and the hub-sent writes of FRG-FR-030, including the existence check and label lookup that precede them. The answer is 502 `forge_error` with message `no authentication token configured`, and no event is recorded. The forge client never reads `GITHUB_TOKEN`, `GH_TOKEN` or `GITLAB_TOKEN` from the hub's environment, neither for authentication nor to decide whether it is authenticated. Reads without a credential are still sent anonymously, as before. In the baseline, an environment token made the client consider itself authenticated, so the write was sent with the auth header stripped and answered 502 `forge_unauthenticated` with an event. Hub-sent writes always reached the forge.
- **FRG-FR-032** Event recording: exactly one event per write route invocation in which at least one request was sent (any request other than a pre-send `no token` refusal), success or failure, emitted before the response is written. Emitter failure is logged and does not change the answer.

**Error mapping**

- **FRG-FR-040** Forge call failures map as follows (first match wins):

| Condition | Status | error_type | message |
|---|---|---|---|
| redirect to another host/scheme or >10 redirects | 502 | `forge_error` | `the forge redirected the request to another host, which hub does not follow` |
| deadline exceeded | 504 | `forge_timeout` | `the forge did not answer in time` |
| context cancelled | 502 | `forge_error` | `the request was cancelled` |
| rate limited (429, or 403 with `X-RateLimit-Remaining: 0` or message containing "secondary rate limit") after the client's retry | 429 | `forge_rate_limited` | forge text; `Retry-After` header = ceil(wait seconds) when known (Retry-After+1 s or reset−now+1 s, min 1 s) |
| forge 404 / not found | 404 | `forge_not_found` | forge text |
| forge 401 | 502 | `forge_unauthenticated` | forge text; + ` The forge API needs a token: store one as GIT_PAT, or as GIT_PASSWORD.` for kind `basic` |
| forge 403 (not rate limit) | 502 | `forge_forbidden` | forge text |
| other forge 4xx (incl. 409, 405, 422) | 422 | `forge_rejected` | forge text |
| forge 5xx / other status | 502 | `forge_error` | forge text |
| conflict sentinel without HTTP status | 422 | `forge_rejected` | error text |
| connection failure (DNS/TCP/TLS, `url.Error`) | 502 | `forge_unreachable` | error text |
| anything else (decode error, no token) | 502 | `forge_error` | error text |

"Forge text" = `the forge answered <status>: <message>` or `the forge answered <status>` when the forge gave no message (message = JSON `message` or GitLab `error` string, or their raw JSON when an object, else HTTP status text). Every message is scrubbed: the workspace token replaced by `[redacted]`, URL userinfo removed, invalid UTF-8 replaced, cut to ≤ 1024 bytes on a rune boundary. 5xx and 429 answers are logged once (slug, target, operation, forge status, status, error_type; no message/token/body).

- **FRG-FR-041** Hub-side refusals: 400 (target/params/body/archived; `workspace_mode_mismatch` only for upstream target), 413 `body_too_large`, 422 `forge_api_url_invalid` / `forge_unknown`, 500 `internal server error` (secrets/settings/DB failures, no error_type). Refusal envelopes include `error_type` only when non-empty.

**Webhook management**

- **FRG-FR-050** `PUT .../forge/webhook` order: shared checks (write, active workspace) → secrets store configured (else 500) → read body → parse: empty/whitespace body, `{}`, `{"secret":null}` → generate; non-object → 400 `the request body must be a JSON object with an optional "secret" string`; non-string secret → 400 `secret must be a string`; length (code points) outside 16..255 → 400 `secret must be 16 to 255 characters long` (so `""` is refused, not generated); a char outside `!`..`~` → 400 `secret must only have the printable ASCII characters from ! to ~, so no space and no control character`. Messages never contain the value. Generated secret = 32 random bytes as 64 lowercase hex chars. Store: update `FORGE_WEBHOOK_SECRET` (case-insensitive); if not found create it; if create conflicts, update once more. Scope full (100 workspace secrets) → 400 `maximum of 100 entries per scope exceeded`; other store error → 500 (no event). Then emit `hub.forge.webhook.set`, answer 200.
- **FRG-FR-051** `DELETE .../forge/webhook`: shared checks with write need but any status → secrets store configured → delete `FORGE_WEBHOOK_SECRET` (not-found is fine) → store error 500 → emit `hub.forge.webhook.unset` (`existed`) → 204. Recorded deliveries remain.
- **FRG-FR-052** The hub never registers, updates or deletes a webhook on the forge.

**Ingress**

- **FRG-FR-060** `POST /hooks/forge/:slug` is mounted at the server root, outside `/api/v1`, with no bearer auth, no API body-size limit and no content-type enforcement; it never contacts the forge, needs no clone and takes no lock. Workspace status is irrelevant (archived workspaces relay).
- **FRG-FR-061** Ingress order:
  1. secrets store not configured → 500; workspace lookup: unknown slug → 404 `not found`; DB error → 500; webhook secret not found or empty → 404 `not found` (identical body to unknown slug); store error → 500. (Body not read yet.)
  2. Scheme: header `X-Hub-Signature-256` present (any count) → GitHub; else `X-Gitlab-Token` present → GitLab; else 401.
  3. GitLab: token header must appear exactly once and equal the secret (constant-time comparison of SHA-256 digests) else 401.
  4. Read body: `Content-Length` > 1,048,576 or more than 1,048,576 bytes read → 413 `the delivery is larger than 1 MiB` with `error_type: body_too_large`; read error → 400 `the delivery body cannot be read`.
  5. GitHub: signature header exactly once and equal (constant-time) to `sha256=` + lowercase hex HMAC-SHA256(secret, raw body) else 401.
  6. Identity: GitHub id `X-GitHub-Delivery`, event `X-GitHub-Event`; GitLab id `Idempotency-Key` else `X-Gitlab-Event-UUID`, event `X-Gitlab-Event`. Missing id → 400 `the delivery has no id (X-GitHub-Delivery)` / `the delivery has no id (Idempotency-Key or X-Gitlab-Event-UUID)`; missing event → 400 `the delivery has no event name`; id > 255 bytes or containing a char outside space..`~` → 400 `the delivery id must be at most 255 printable ASCII characters`. Nothing recorded.
  7. Reduce (FRG-FR-062) and insert the log row with `ON CONFLICT DO NOTHING`; DB error → 500, nothing emitted. Conflict → 202 `{"outcome":"duplicate"}`, nothing emitted.
  8. Outcome `malformed` → 400 `the delivery payload is not what its event needs` (row recorded).
  9. Otherwise 202 `{"outcome":"relayed"|"ignored"|"repository_mismatch"}` with explicit `Content-Length`, flushed; then the events are emitted in order (even if the response write failed).
  All 401s use the single message `delivery signature missing or invalid`, record nothing and log a warning with slug and scheme (`github`/`gitlab`/`none`) only.
- **FRG-FR-062** Reduction: body must be exactly one JSON object (numbers kept exact; trailing data or non-object → `malformed`, target NULL, action ""). Action read (GitHub `action`; GitLab `object_attributes.action`). Repository path: GitHub `repository.full_name`, GitLab `project.path_with_namespace`; matched case-insensitively against the origin's `owner/name`, then (carry_patch with upstream_url) the upstream's → target `origin`/`upstream`/none (origin wins when both match). Then: no mapping for (forge, event, action) → `ignored` (target kept); mapping fails on a required field → `malformed` (target NULL); mapping yields no events → `ignored`; events but no repository path → `malformed` (target NULL); events but no matching repository → `repository_mismatch`; else `relayed` with the events.
- **FRG-FR-063** Mapping (event header, action → events; required fields in brackets; other fields default to empty):

| Forge | Event / action | Hub event(s) | Required |
|---|---|---|---|
| GitHub | `issues` `opened`/`edited`/`closed`/`reopened` | `hub.forge.issue.opened`/`.edited`/`.closed`/`.reopened` | `issue.number` (integer ≥1) |
| GitHub | `issues` `labeled`/`unlabeled` | `hub.forge.issue.labeled`/`.unlabeled` | `issue.number`, `label.name` |
| GitHub | `issue_comment` `created` | `hub.forge.comment.created` | `issue.number` |
| GitHub | `pull_request` `opened`/`reopened`/`synchronize` | `hub.forge.pull_request.opened`/`.reopened`/`.synchronize` | `pull_request.number` |
| GitHub | `pull_request` `closed` | `.merged` if `pull_request.merged == true` else `.closed` | `pull_request.number` |
| GitHub | `push` (any action) | `hub.forge.push` | non-empty `ref` |
| GitLab | `Issue Hook` `open`/`close`/`reopen` | `hub.forge.issue.opened`/`.closed`/`.reopened` | `object_attributes.iid` |
| GitLab | `Issue Hook` `update` | `.edited` if `changes.title` or `changes.description` present; then one `.unlabeled` per label in `changes.labels.previous` not in `.current`, then one `.labeled` per label in current not in previous (each group sorted by name, deduplicated); total label events capped at 100 (warning logged); none → ignored | `object_attributes.iid` |
| GitLab | `Note Hook` action `""`/`create` | `hub.forge.comment.created` for `noteable_type` `Issue` (`issue.iid`) or `MergeRequest` (`merge_request.iid`); other noteable types → ignored | the iid |
| GitLab | `Merge Request Hook` `open`/`close`/`reopen`/`merge` | `hub.forge.pull_request.opened`/`.closed`/`.reopened`/`.merged` | `object_attributes.iid` |
| GitLab | `Merge Request Hook` `update` | `.synchronize` when `object_attributes.oldrev` non-empty, else ignored | `object_attributes.iid` |
| GitLab | `Push Hook`, `Tag Push Hook` | `hub.forge.push` | non-empty `ref` |

Everything else is ignored (e.g. GitHub `ping`, `pull_request` labeled, `issue_comment` edited; GitLab confidential hooks; unknown events).

**Deliveries list / retention**

- **FRG-FR-070** `GET .../forge/deliveries`: read access, any status; `since` optional, given once (else 400 `since must be given once`), RFC 3339 (else 400 `since must be an RFC 3339 timestamp, for example 2026-10-21T09:00:00Z`; an empty value is invalid); converted to UTC; a fractional second rounds up to the next whole second; `limit` as FRG-FR-010 (default 30, 1..100). Rows with `received_at >= since`, ordered by `received_at` DESC then insertion order DESC, at most `limit`.
- **FRG-FR-071** Retention: at hub start and every hour, rows with `received_at` older than 7 days (now − 168 h) and rows whose `workspace_slug` no longer exists in `workspaces` are deleted. Not configurable.

### 9.5 REST API

All `/api/v1/workspaces/:slug/...` routes: envelope `{"error":{"code","message","error_type"?}}`; shared checks FRG-FR-002; bound PATs may use them only on their own workspace (other slug → 404). The API group's JSON content-type (415) and body-size (413 `payload too large`) checks apply before the handler.

#### `GET /api/v1/workspaces/:slug/forge`
- **Purpose:** diagnostic: how the hub reaches the forge, without contacting it.
- **Auth / scope:** read credential (FRG-FR-001); read access (member allowed); any status.
- **Params:** none (`target` ignored).
- **Behaviour:** origin description (API URL setting validated first → credential kind → parse remote → detect forge); if carry_patch with upstream_url, the same for upstream; webhook block: `configured` = `FORGE_WEBHOOK_SECRET` exists and non-empty; `url` per webhook URL rule; `last_delivery_at` = MAX(`received_at`) of the workspace's log rows. An unparsable remote or undetectable forge is reported (`forge: "unknown"`), not an error.
- **Response:** 200
```json
{"forge":"github|gitlab|unknown","api_url":"https://api.github.com"|null,
 "repository":{"host":"github.com","owner":"acme","name":"widgets"}|null,
 "credential":"pat|basic|none",
 "upstream":{ "forge":..., "api_url":..., "repository":..., "credential":... }|null,
 "webhook":{"configured":true,"url":"https://hub.example.com/hooks/forge/widgets"|null,"last_delivery_at":"2026-10-21T09:12:03Z"|null}}
```
`api_url` is null when forge is `unknown`; it is the setting (trailing `/` trimmed, `/api/v4` appended for GitLab) or the host default — env `GITHUB_API_URL`/`GITLAB_API_URL` is never shown as the URL (though it can influence detection). `repository` is null when the remote names none (then `forge` is `unknown`).
- **Errors:** shared; 422 `forge_api_url_invalid` (origin or upstream setting); 500 (settings/credential store, secrets store not configured, secret read failure, delivery log read failure — no partial answer).
- **Side effects:** none.

#### `GET /api/v1/workspaces/:slug/forge/repository`
- **Purpose:** the repository. **Auth:** read. **Params:** `target`.
- **Response:** 200 repository object. **Errors:** shared, target 400, 422/500 build, forge mapping.

#### `GET /api/v1/workspaces/:slug/issues`
- **Purpose:** list issues. **Auth:** read. **Query:** `target`, `state`, `sort`, `direction`, `labels`, `assignee`, `limit` (FRG-FR-010).
- **Response:** 200 array of issues (`[]` if none).
- **Errors:** shared; 400 target/params (`state must be one of: open, closed, all`, `sort must be one of: created, updated`, `direction must be one of: asc, desc`, `limit must be given once`, `limit must be an integer from 1 to 100`); 422/500; forge mapping.

#### `GET /api/v1/workspaces/:slug/issues/:n`
- **Purpose:** issue thread. **Auth:** read. **Params:** `n`, `target`.
- **Response:** 200 `{"issue":{...},"comments":[...],"truncated":false,"comments_err"?:...}` (FRG-FR-012).
- **Errors:** shared; 400 `n must be a positive decimal integer`; 422/500; forge mapping (issue fetch failure).

#### `GET /api/v1/workspaces/:slug/issues/:n/comments`
- **Response:** 200 array of comments. Otherwise as above.

#### `GET /api/v1/workspaces/:slug/pulls/:n` · `/pulls/:n/state` · `/pulls/:n/files` · `/pulls/:n/checks` · `/pulls/:n/reviews`
- **Auth:** read. **Params:** `n`, `target`.
- **Response:** 200 pull request / PR state object / array of changed files / array of check runs / array of reviews (`[]` when empty).
- **Errors:** shared; 400 `n`; 422/500; forge mapping.

#### `POST /api/v1/workspaces/:slug/issues`
- **Auth:** write (owner/admin/api key of owner; member 403 `forbidden`); active workspace. **Query:** `target`.
- **Body:** `{"title": string (req, non-blank), "body"?: string, "labels"?: [string ≤100], "assignees"?: [string ≤100]}`.
- **Response:** 201 issue.
- **Errors:** shared; 400 archived/target/body; 422/500 build; 413 body; forge mapping.
- **Side effects:** `hub.forge.issue.create`.

#### `PATCH /api/v1/workspaces/:slug/issues/:n`
- **Body:** `{"title"?, "body"?, "state"?: "open"|"closed", "labels"?: [..], "assignees"?: [..]}` (FRG-FR-021).
- **Response:** 200 issue. **Errors:** as create + 400 `n`, `nothing to update`, GitLab assignee-only. **Side effects:** `hub.forge.issue.update` (`fields`).

#### `POST /api/v1/workspaces/:slug/issues/:n/close`
- **Body:** empty or `{"comment"?: string}`.
- **Response:** 200 issue (closed). **Side effects:** `hub.forge.issue.close` (`comment_url` when posted). See FRG-FR-022.

#### `POST /api/v1/workspaces/:slug/issues/:n/comments`
- **Body:** `{"body": string (req, non-blank)}`. **Response:** 201 `{"url": "..."}`. **Side effects:** `hub.forge.comment.create`.

#### `POST /api/v1/workspaces/:slug/issues/:n/labels`
- **Body:** bare JSON array `["name", ...]` (1..100, non-blank). **Response:** 204. **Side effects:** `hub.forge.label.add`.

#### `DELETE /api/v1/workspaces/:slug/issues/:n/labels/:name`
- **Params:** `:name` = last raw path segment decoded once. **Response:** 204 (FRG-FR-025). **Errors:** + 404 `forge_not_found` when the issue does not exist. **Side effects:** `hub.forge.label.remove`.

#### `POST /api/v1/workspaces/:slug/labels`
- **Body:** `{"name": string (req), "color"?: "#?RRGGBB", "description"?: string}`. **Response:** 200 existing label (unchanged) / 201 created label. **Side effects:** `hub.forge.label.create` (`created`).

#### `POST /api/v1/workspaces/:slug/pulls`
- **Body:** `{"title" (req), "head" (req), "base" (req), "body"?, "draft"?: bool}`. **Response:** 201 pull request. **Side effects:** `hub.forge.pull_request.create`.

#### `POST /api/v1/workspaces/:slug/pulls/:n/comments`
- **Body:** `{"body" (req)}`. **Response:** 201 `{"url": "..."}`. **Side effects:** `hub.forge.pull_request.comment`.

#### `PUT /api/v1/workspaces/:slug/forge/webhook`
- **Purpose:** set the webhook secret. **Auth:** write credential; write access; active workspace.
- **Body:** optional `{"secret"?: string}` (FRG-FR-050). `target` ignored.
- **Response:** 200 `{"configured": true, "url": "<external_url>/hooks/forge/<slug>"|null, "secret"?: "<64 hex>"}` — `secret` only when generated.
- **Errors:** shared (incl. 400 archived); 400 body rules / scope full; 500.
- **Side effects:** secret stored; `hub.forge.webhook.set` `{"generated": bool}`.

#### `DELETE /api/v1/workspaces/:slug/forge/webhook`
- **Auth:** write credential; write access; **any status**. **Response:** 204 whether or not a secret existed. **Errors:** shared (no archived check); 500. **Side effects:** secret deleted; `hub.forge.webhook.unset` `{"existed": bool}`.

#### `GET /api/v1/workspaces/:slug/forge/deliveries`
- **Auth:** read; read access; any status. **Query:** `since`, `limit` (FRG-FR-070).
- **Response:** 200 array:
```json
[{"delivery_id":"72d3...","forge":"github","event":"issues","action":"labeled","target":"origin"|null,
  "outcome":"relayed","relayed":["hub.forge.issue.labeled"],"received_at":"2026-10-21T09:12:03Z"}]
```
- **Errors:** shared; 400 since/limit; 500 log read failure.

#### `POST /hooks/forge/:slug`
- **Purpose:** forge webhook ingress. **Auth:** none (signature/token). **Body:** raw forge payload (≤ 1 MiB).
- **Behaviour:** FRG-FR-061..063.
- **Response:** 202 `{"outcome":"relayed"|"ignored"|"repository_mismatch"|"duplicate"}`.
- **Errors:** 404 `not found`; 401 `delivery signature missing or invalid`; 413 `body_too_large`; 400 (body unreadable, identity, malformed payload); 500.
- **Side effects:** a `forge_deliveries` row (not for 4xx other than malformed, not for duplicates); relayed events after the answer.

### 9.6 CLI

Common: output = the hub's JSON answer pretty-printed on stdout (or a status document where stated); errors = JSON envelope on stdout; hub error exit 1 (code = HTTP status; `error_type` not carried); usage error exit 2 (code 2; nothing sent); network failure exit 2; cobra arity/flag errors exit 2 (envelope code 0, e.g. `accepts 2 arg(s), received 1`). Slugs, numbers and label names are percent-encoded as single path segments. Query parameters are sent only for flags explicitly given (`Changed`), values as given (validated by the hub). `parley` binary does not include the forge client.

Body input (commands with `--body`/`--body-file`): `--body <text>` literal; `--body-file <path>` reads the file, or stdin when `-`; content sent exactly (no trimming, trailing newline kept). Both given → usage error `--body and --body-file cannot be used together`; unreadable file → usage error `cannot read the body from <path>: <err>`. `--target <origin|upstream>` (writes: help "Remote to write to") sent as `?target=` only when given. Write commands validate `<number>` client-side with `strconv.Atoi` > 0 (so `+3`/`007` are accepted and sent normalized as `3`/`7`); read commands send the number as typed.

#### `parley forge status <workspace-slug>`
- **Args:** exactly 1. No flags (no `--target`). **Calls:** `GET /workspaces/<slug>/forge`. **Output:** the diagnostic document.

#### `parley forge webhook set <workspace-slug> [--secret s | --from-stdin | --generate]`
- **Flags:** `--secret <string>`; `--from-stdin` (bool; one trailing `\n` then `\r` removed); `--generate` (bool, the default behaviour). More than one given → usage error `--secret, --from-stdin and --generate are mutually exclusive`.
- **Calls:** `PUT /workspaces/<slug>/forge/webhook` with `{"secret": ...}` for `--secret`/`--from-stdin` (an explicit `--secret ""` sends `""`, refused by the hub), else `{}`.
- **Output:** the hub's answer; when it contains `secret`, stderr gets `note: hub will not show this secret again; enter it in the webhook settings of the forge now`.

#### `parley forge webhook unset <workspace-slug>`
- **Calls:** `DELETE /workspaces/<slug>/forge/webhook`. **Output:** `{"status":"unset","workspace":"<slug>"}`.

#### `parley forge deliveries <workspace-slug> [--since <rfc3339>] [--limit <n>]`
- **Flags:** `--since` (string), `--limit` (string). **Calls:** `GET /workspaces/<slug>/forge/deliveries[?since=&limit=]`. **Output:** the array.

#### `parley issue list <workspace-slug>`
- **Flags:** `--state` (string), `--label` (repeatable; joined with `,` into one `labels` param), `--assignee` (string), `--limit` (string), `--target`. (No `--sort`/`--direction`.)
- **Calls:** `GET /workspaces/<slug>/issues?...`. **Output:** the array.

#### `parley issue get <workspace-slug> <number> [--target]`
- **Calls:** `GET /workspaces/<slug>/issues/<number>`. **Output:** the issue thread.

#### `parley pr get|state|files|checks|reviews <workspace-slug> <number> [--target]`
- **Calls:** `GET /workspaces/<slug>/pulls/<number>` and `/state`, `/files`, `/checks`, `/reviews`. **Output:** the hub's answer.

#### `parley issue create <workspace-slug> --title <t> [--body|--body-file] [--label]... [--assignee]... [--target]`
- **Validation:** `--title` empty/missing → usage error `--title flag is required`.
- **Calls:** `POST /workspaces/<slug>/issues` with `title`, `body` (if a body flag given), `labels` (if `--label` given; each value one name, commas kept), `assignees` (if `--assignee` given). **Output:** created issue.

#### `parley issue update <workspace-slug> <number> [--title] [--body|--body-file] [--state] [--label]... [--assignee]... [--target]`
- **Validation:** number → `the issue number must be a positive integer`; no field flag given → `nothing to update: set at least one of --title, --body, --body-file, --state, --label and --assignee`.
- **Calls:** `PATCH /workspaces/<slug>/issues/<n>` with only the given fields (`--body ""` sends `""`). **Output:** the issue.

#### `parley issue close <workspace-slug> <number> [--comment <text>] [--target]`
- **Calls:** `POST /workspaces/<slug>/issues/<n>/close` with `{"comment": ...}` if `--comment` given, else no body. **Output:** the issue.

#### `parley issue comment <workspace-slug> <number> (--body|--body-file) [--target]`
- **Validation:** number (`the issue number must be a positive integer`); no body flag → `--body or --body-file flag is required`.
- **Calls:** `POST /workspaces/<slug>/issues/<n>/comments` `{"body":...}`. **Output:** `{"url": ...}`.

#### `parley issue label <workspace-slug> <number> <label>... [--target]`
- **Args:** ≥ 3. **Calls:** `POST /workspaces/<slug>/issues/<n>/labels` with the labels as a bare array. **Output:** `{"status":"labeled","number":<n>,"labels":[...]}`.

#### `parley issue unlabel <workspace-slug> <number> <label> [--target]`
- **Args:** exactly 3. **Calls:** `DELETE /workspaces/<slug>/issues/<n>/labels/<label percent-encoded>`. **Output:** `{"status":"unlabeled","number":<n>,"label":"<label>"}`.

#### `parley label create <workspace-slug> <name> [--color <hex>] [--description <text>] [--target]`
- **Validation:** empty name → `the label name must not be empty`. **Calls:** `POST /workspaces/<slug>/labels` `{"name", "color"?, "description"?}` (fields only when given). **Output:** the label (exit 0 for both 200 and 201).

#### `parley pr create <workspace-slug> --title --head --base [--body|--body-file] [--draft] [--target]`
- **Validation:** `--title`, `--head`, `--base` each required non-empty, checked in that order → `--<name> flag is required`.
- **Calls:** `POST /workspaces/<slug>/pulls` with `title`, `head`, `base`, `body` (if given), `draft` (if `--draft` given, incl. `--draft=false`). **Output:** the pull request.

#### `parley pr comment <workspace-slug> <number> (--body|--body-file) [--target]`
- As `issue comment` (`the pull request number must be a positive integer`); calls `POST /workspaces/<slug>/pulls/<n>/comments`.

Routes without CLI commands: `GET .../forge/repository`, `GET .../issues/:n/comments`.

### 9.7 Background behaviour

- At every start: `forge_deliveries` table and index created if missing (before the maintenance loop starts).
- Maintenance loop (in-process, once at start then every hour, sequential with the audit retention and token binding sweep): delete delivery rows older than 7 days or of non-existent workspaces; failures logged, retried next hour.
- Ingress emits relayed events after the response is flushed; no queue, no retry of emission.
- Forge reads/writes are synchronous within the request; nothing is queued or cached.

### 9.8 Events

Write events (actor = caller: `actor_id` user id, `actor_type` credential type; `workspace` = slug). Common metadata: `target` (`origin`/`upstream`), `ok` (bool), `error_type` (on failure), `number` (when > 0), `url` (web URL of the resulting issue/PR/comment, scrubbed, when known). Never a title, body, comment, credential or forge message.

| Event | resource_type | resource_id | action | Extra metadata |
|---|---|---|---|---|
| `hub.forge.issue.create` | `issue` | new number (empty on failure) | `create` | — |
| `hub.forge.issue.update` | `issue` | `n` | `update` | `fields` (sorted names set; GitLab excludes `assignees`) |
| `hub.forge.issue.close` | `issue` | `n` | `close` | `comment_url` (when a comment was posted) |
| `hub.forge.comment.create` | `comment` | `n` | `create` | (`url` = comment URL) |
| `hub.forge.label.add` | `label` | `n` | `add` | `labels` |
| `hub.forge.label.remove` | `label` | `n` | `remove` | `label` |
| `hub.forge.label.create` | `label` | label name | `create` | `label`, `created` (true only if this call created it) |
| `hub.forge.pull_request.create` | `pull_request` | new number (empty on failure) | `create` | `head`, `base`, `draft` |
| `hub.forge.pull_request.comment` | `pull_request` | `n` | `comment` | (`url` = comment URL) |

Webhook events (actor = caller; resource_type `webhook`; resource_id = slug; workspace = slug): `hub.forge.webhook.set` (action `set`, `{"generated": bool}`), `hub.forge.webhook.unset` (action `unset`, `{"existed": bool}`). Never the secret.

Delivery events (actor_type `forge`, actor_id `""`, workspace = slug, action = last dot-segment of the type). Common metadata: `forge`, `target`, `delivery_id`, `actor` (GitHub `sender.login`; GitLab `user.username` else `user_username`; `""` if absent).

| Event | resource_type | resource_id | Extra metadata |
|---|---|---|---|
| `hub.forge.issue.opened` / `.edited` / `.closed` / `.reopened` | `issue` | number (GitLab iid) | `number`, `title` (≤256 bytes, valid UTF-8), `url` (userinfo removed), `labels` (≤100 names; GitHub `issue.labels[].name`, GitLab top-level `labels[].title`) |
| `hub.forge.issue.labeled` / `.unlabeled` | `issue` | number | `number`, `label`, `labels` (current labels ≤100; GitLab: top-level labels, else `changes.labels.current`), `url` |
| `hub.forge.comment.created` | `comment` | issue/PR number | `number`, `comment_url`, `pull_request` (GitHub: `issue.pull_request` present; GitLab: noteable MergeRequest) |
| `hub.forge.pull_request.opened` / `.closed` / `.merged` / `.reopened` / `.synchronize` | `pull_request` | number | `number`, `url`, `head`, `base` (GitHub `head.ref`/`base.ref`; GitLab `source_branch`/`target_branch`), `merged` (GitHub `pull_request.merged`; GitLab true for merge or `state == merged`) |
| `hub.forge.push` | `repository` | ref | `ref`, `before`, `after` |

### 9.9 Configuration

| Key / variable | Type | Default | Effect |
|---|---|---|---|
| `server.external_url` (config.toml) | string | `""` | base of the webhook URL; empty → `url: null` (secret still settable) |
| workspace setting `forge_api_url` | string | unset | origin API URL override (validation FRG concepts) |
| workspace setting `upstream_forge_api_url` | string | unset | upstream API URL override, no fallback |
| `GITHUB_API_URL`, `GITLAB_API_URL` (env) | URL | unset | forge detection for hosts they name; API URL for reads when they name the host |
| `GITHUB_TOKEN`, `GH_TOKEN`, `GITLAB_TOKEN` (env) | string | unset | **[FIX-14]** ignored by the hub: never sent to a forge and never consulted (FRG-FR-031) |
| `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` (env) | | | proxying of forge calls |
| PAT scopes | | | `forge:read`, `forge:write` registered at every start |

Fixed (not configurable): call deadline 30 s; ≤10 same-host redirects; ingress body 1 MiB; delivery id ≤255; event name stored ≤64 bytes; retention 7 days, pruning hourly; list limit default 30 max 100; name lists ≤100; webhook secret 16..255 printable ASCII, generated 32 bytes hex; body limits 65,536 / 1,000,000 code points; message scrub cap 1024 bytes.

### 9.10 Edge cases and error handling

- Scope check precedes workspace lookup: a PAT without the scope gets 403 even for an unknown slug; a member's write is 403 even on an archived workspace (access before archived check).
- A write to a workspace whose forge cannot be detected is 422 even when the body would be oversized (limit depends on forge); malformed JSON is 400 before the forge is detected.
- `issues/:n` on GitHub with a PR number returns the issue with `is_pr: true`.
- `PATCH` with only `assignees` on GitLab → 400 after client build (no event).
- `ensure label` always performs at least one forge GET, so an event is recorded even when the label already exists (`created: false`).
- Remove label: GitHub 404 for a missing label is followed by an issue check; GitLab removal of an absent label succeeds directly.
- Ingress with both headers: GitHub scheme decides (a wrong GitLab token is irrelevant). A header given twice → 401. GitHub: body size is checked before the signature (an oversized unsigned body gets 413, not 401); GitLab: token checked before the body is read.
- A delivery signed correctly but form-encoded (GitHub legacy) → recorded `malformed`, 400. Legacy `X-Hub-Signature` (SHA-1) alone → 401 (no scheme header).
- A redelivery of a `malformed` delivery answers 202 `duplicate`.
- A `FORGE_WEBHOOK_SECRET` set via the secrets API bypasses the 16..255 rule (any non-empty value works at the ingress).
- If the forge connection closes before the 202 is written, the row stays `relayed` and the events are still emitted (a redelivery would be a duplicate).
- Several concurrent copies of one delivery: exactly one insert succeeds; others answer `duplicate`.
- Slug reuse: rows of a deleted workspace are pruned at the next hourly pass, until then a re-created slug sees them.
- No response, log line or event carries the token or URL userinfo; store errors are logged by error type only.

### 9.11 Discrepancies between docs and code

1. **`comments_err` encoding.** `docs/api.md` ("issue thread … comments_err (with the token and URL userinfo removed)") implies a readable error. Code returns the scrubbed `*issuex.HTTPError` struct only when the comments error is a bare HTTP error; any wrapped error (not-found, rate limit, conflict, transport) is replaced by `errors.New(...)`, which JSON-encodes as `{}` (`internal/forge/handlers_read.go:136-143`; issuex wraps 404/409/rate-limit in `transport.go:classifyError`).
2. **Missing-credential writes.** `docs/api.md` (Failures of the forge) says a write prevented by a missing credential is `502 forge_error` with nothing sent and no event. That holds only when the hub's environment has no `GITHUB_TOKEN`/`GH_TOKEN`/`GITLAB_TOKEN`: writes pin `BaseURL`, so `NewGitHub` falls back to the env token (`aflib/issuex/github.go:39-46`), the client is "authenticated", and the request is sent with the header stripped (`internal/forge/httpclient.go:46-53`) → forge 401 → 502 `forge_unauthenticated` with an event. Hub-sent writes (update, close, label lookup/create, GitHub label removal) never use `ErrNoToken` and always reach the forge (`internal/forge/write_raw.go:248-294`). → **Resolved in this PRD by FIX-14:** the re-implementation follows the documented behaviour described here (see §1.8).
3. **Retry of label removal.** `docs/api.md` (Retries) lists only create issue, comment, add labels and open PR as retried; GitLab label removal goes through issuex `RemoveLabel` and is retried once on rate limit (`internal/forge/write_raw.go:65-70`).
4. **Package doc** mentions `StartDeliveryRetention`; no such function exists — pruning is the hourly maintenance task in `cmd/parleyd/main.go:340-343` / `maintenance.go` (`internal/forge/doc.go:15`). Docs (`api.md`, `configuration.md`) correctly say hourly / 7 days.
5. **main.go comment** says "forge:write is registered although no route needs it yet" (`cmd/parleyd/main.go:247-249`); nine write routes and the webhook routes do need it. Comment only; docs are correct.
6. **Order of checks (reads).** `docs/api.md` lists for reads "3. The workspace … 4. target"; the code additionally loads the workspace remotes (404/500) before target, and for `GET .../forge` validates the API URL setting before parsing the remote, whereas for other routes the remote is parsed (422 `forge_unknown`) before the setting is validated (`internal/forge/client.go:85-97` vs `299-308`). So a workspace with both an unparsable remote and an invalid setting gets `forge_unknown` on reads/writes but `forge_api_url_invalid` on `GET .../forge`.
7. **CLI number validation.** `docs/cli.md` (Errors of the forge commands) says a number that is not a positive integer is a usage error for writes; `strconv.Atoi` accepts `+3` and leading zeros and the CLI sends the normalized number (`internal/cli/forge_write_cmd.go:26-32`), while the hub would reject `+3` on reads (`internal/forge/params.go:29-35`).


---

## 10. Sandboxes

### 10.1 Purpose and scope

The hub lends isolated sandboxes (containers, optionally micro-VMs) to agents, filled with the secrets the agent needs, without ever handing those secrets to the caller. An operator defines the allowed shapes as **profiles** in the `[sandbox]` section of `config.toml`. A caller obtains a **lease** over REST (`POST /api/v1/workspaces/:slug/sandboxes`), which returns a one-time **lease token**. The caller then uses an AgentKit client against the hub's embedded **sandbox gateway**, a gRPC listener separate from the REST port, to create exactly one sandbox for that lease and to work in it. The hub enforces lifetimes (TTL, a deadline for the create, a sweeper, recovery after a crash, and cleanup at shutdown) and a global capacity limit. The feature is optional. Without a `[sandbox]` section every sandbox REST route answers `404 sandboxes_disabled` and no gateway is bound.

In scope: config/validation/startup probing, profile listing, lease CRUD, secret injection, the gateway behaviour as far as it is visible to clients, the lifecycle (sweep, expiry, retention, recovery, shutdown), events, metrics and the `parley sandbox` commands. Out of scope: building or pulling images, an `parley` command for the admin-wide list, multi-replica operation (the claim that serialises Create is in-process), and configurable allowlist endpoints.

### 10.2 User stories

**US-SBX-1 (Operator): enable sandboxes.** As an Operator, I want to declare a provider and profiles in `config.toml`, so that agents can get sandboxes of shapes I control.
- Every static error in `[sandbox]`, unknown keys included, is reported together and stops the start. The provider is never contacted for a statically invalid section.
- The provider is probed once within 10 s. Network modes and profiles are checked against its capabilities. Any failure stops the start.
- A missing section logs `sandboxes are disabled: the configuration has no [sandbox] section` and the hub starts normally.
- On success, one info line `sandboxes are enabled` is logged with `provider` and `profiles` (names only).

**US-SBX-2 (Developer/Agent): discover profiles.** As a Developer, I want to list profiles, so that I can choose one by name.
- `GET /api/v1/sandbox-profiles` returns every profile sorted by name, with secret names and env key names but never values. A bound PAT may call it.

**US-SBX-3 (Agent): get a lease.** As an Agent with a `sandboxes:write` PAT, I want to issue a lease in my workspace, so that I can create a sandbox with the profile's secrets.
- 201 with the lease plus `token` (shown only here). Required secrets must exist (else 422 `secret_missing`). Live leases are capped at `max_sandboxes` (else 409 `sandbox_capacity`).

**US-SBX-4 (Sandbox client): create and use the sandbox.** As a Sandbox client holding a lease token, I want to dial `gateway_url` and create my sandbox, so that I can run code in it.
- The token is accepted only while the lease is `leased`/`active` and before `expires_at`.
- The first Create makes the lease `active`. A second Create is refused.
- The sandbox's image, network, resources, Landlock setting and env come from the lease, never from the caller's spec. Only `Workdir` and labels come from the caller.
- Environment values in the effective spec returned to the client are `[redacted]`.

**US-SBX-5 (Developer/Agent): end the lease.** As a lease holder, I want to end the lease (DELETE, or by closing the sandbox), so that the box is destroyed and the token revoked.
- DELETE is idempotent (always 204). If the provider fails to destroy an active box, the answer is 502 `sandbox_provider_error` and nothing changes.

**US-SBX-6 (Operator): inspect all leases.** As an Operator with the admin token, I want to list every workspace's leases (`GET /api/v1/sandboxes`).

**US-SBX-7 (Operator): nothing leaks or outlives its time.** As an Operator, I want leases that nobody uses to lapse, boxes to die at `expires_at`, crashed-process leases to be reconciled at start, and boxes to be destroyed on shutdown.

**US-SBX-8 (Org member): read-only.** As an Org member of a workspace's org, I can list and read its leases, but POST and DELETE answer `403 forbidden`.

### 10.3 Concepts and data model

#### 10.3.1 Profile (in memory, from config; never persisted)

| Field | Type | Default | Constraints |
|---|---|---|---|
| `name` | string | required | `^[a-z0-9][a-z0-9_-]{0,62}$`, unique (case-sensitive) |
| `default` | bool | false | at most one profile may be `true`; none is valid |
| `image` | string | required | non-empty; not checked/pulled |
| `cpus` | float | 0 | ≥0, finite, not NaN; fractions allowed |
| `memory_mb` | int (MiB) | 0 | whole number 0..2^53; must fit int64 bytes (×2^20) |
| `pids` | int | 0 | whole number 0..2^53 |
| `disk_mb` | int (MiB) | 0 | whole number 0..2^53; podman provider refuses any non-zero value at start |
| `network` | string | `"none"` | must be in effective `network_modes` |
| `ttl` | Go duration string | required | >0, ≤ `max_ttl` |
| `secrets` | []string | [] | each `secrets.ValidateKey` (non-empty, ≤255 chars, `[A-Za-z0-9_]`, not starting with digit); unique case-insensitively across `secrets`+`optional_secrets` |
| `optional_secrets` | []string | [] | same rules |
| `env` | map[string]string | {} | keys follow ValidateKey; no key equal (case-insensitive) to any secret name; values private |
| `require_landlock` | bool | false | |

Zero resources mean "provider default". In the listing, `ttl_seconds` = TTL truncated to whole seconds.

#### 10.3.2 Lease (persisted, SQLite table `sandbox_leases` in the main DB)

| Field (column) | Type | Meaning |
|---|---|---|
| `id` | TEXT PK | `sbx_` + lowercase hex of 16 random bytes (36 chars total). Matches `^sbx_[0-9a-f]{32}$` |
| `workspace` | TEXT | workspace slug |
| `profile` | TEXT | profile name at issue |
| `purpose` | TEXT, default `''` | caller text, ≤64 runes, no control chars |
| `state` | TEXT CHECK in (`leased`,`active`,`destroyed`,`expired`,`failed`) | lifecycle state |
| `token_hash` | TEXT UNIQUE, nullable | lowercase hex SHA-256 of the token. Set to NULL when the lease ends. The plaintext token is never stored |
| `image`, `network` | TEXT | profile snapshot |
| `cpus` REAL, `memory_mb`, `pids`, `disk_mb` INTEGER | numbers | profile snapshot |
| `require_landlock` | INTEGER 0/1 | snapshot |
| `secrets`, `optional_secrets` | TEXT JSON array, default `[]` | profile's secret names in profile order (not sorted) |
| `env` | TEXT JSON object, default `{}` | the profile's fixed env, **values included** (stored for the gateway, never returned by any response) |
| `labels` | TEXT JSON object, default `{}` | the caller's labels |
| `ttl_seconds` | INTEGER | effective TTL in whole seconds |
| `expires_at` | TEXT RFC 3339 UTC (whole seconds, `Z`) | `created_at + ttl` |
| `lease_expires_at` | TEXT nullable | `created_at + min(lease_timeout, ttl)`. Never changes. NULL for rows predating the column (those use `expires_at` as the create deadline) |
| `created_by` | TEXT | `"admin"` for the admin token, otherwise the caller's user id |
| `created_at`, `updated_at` | TEXT RFC 3339 UTC | |
| `sandbox_id` | TEXT nullable | provider box id once `active` |
| `error` | TEXT nullable | failure message of a `failed` lease |
| `ended_at` | TEXT nullable | when the lease reached a terminal state |
| `end_reason` | TEXT nullable | `client`, `api`, `expired`, `shutdown`, `restart`. NULL for `failed` |

Indexes: `idx_sandbox_leases_workspace(workspace)` and `idx_sandbox_leases_state(state)`. `token_hash` is indexed by its UNIQUE constraint. The table is created (`CREATE TABLE IF NOT EXISTS`) and migrated (adds `lease_expires_at TEXT` if missing, checked via `PRAGMA table_info`, idempotent) at **every** start, whether or not sandboxes are enabled. A failure is fatal.

All timestamps are whole-second RFC 3339 UTC strings of fixed width, so text comparison orders them by time.

**Token:** `sbl_` + unpadded base64url (RFC 4648 §5) of 33 random bytes (crypto/rand) = 48 chars. It matches `^sbl_[A-Za-z0-9_-]{44}$`. It is returned only in the 201 of POST.

**Live** = `leased` or `active`. **Terminal** = `destroyed`, `expired`, `failed`.

#### 10.3.3 State machine

| From | To | Trigger | end_reason / error | Event |
|---|---|---|---|---|
| (new) | `leased` | POST | — | `hub.sandbox.lease` |
| `leased` | `active` | gateway Create succeeded and recorded (sets `sandbox_id`) | — | `hub.sandbox.create` |
| `leased` | `failed` | Create: profile no longer configured / spec cannot be built / required secret missing / provider error | `error` = message; end_reason NULL | `hub.sandbox.failed` |
| `leased` | `expired` | Create at/after `lease_expires_at` (or with <1 s left); sweep "lapse" | `expired` | `hub.sandbox.expire` (state `expired`) |
| `leased` | `destroyed` | DELETE | `api` (`shutdown` in shutdown mode) | `hub.sandbox.destroy` (sandbox_id null) |
| `active` | `destroyed` | DELETE after box destroyed | `api` / `shutdown` | `hub.sandbox.destroy` |
| `active` | `destroyed` | holder closes box via gateway (not within 2 s of `expires_at`) | `client` / `shutdown` | `hub.sandbox.destroy` (actor lease) |
| `active` | `destroyed` | sweep "overdue" (`expires_at` ≤ now) | `expired` | `hub.sandbox.expire` (state `destroyed`, with sandbox_id) |
| `active` | `destroyed` | hub shutdown | `shutdown` | `hub.sandbox.destroy` (actor system) |
| `active` | `destroyed` | startup recovery (lease left active by a previous process) | `restart` | `hub.sandbox.destroy` (actor system) |

Every transition is a single UPDATE conditioned on the expected current state (`... WHERE id=? AND state IN (...)`). A lease in a terminal state is never changed. Every ending stamps `ended_at` and `updated_at` and clears `token_hash` in the same statement, so revoking the token and ending the lease are one act. `leased→active` stamps `updated_at` and keeps the token valid. A transition that changes 0 rows emits nothing.

### 10.4 Functional requirements

**Configuration and startup**

- SBX-FR-001: The `[sandbox]` section is read from the same config file apikit loads. If the file does not exist, or has no `[sandbox]` table, the feature is off (`Init` returns nothing, and an info log is written). An unreadable file → fatal `sandbox: reading <path>: ...`. Undecodable TOML → fatal `sandbox: decoding <path>: ...`.
- SBX-FR-002: Every key under `[sandbox]` (including inside profiles) that is not in §10.9 is an error `sandbox: unknown key sandbox.<path>`. For profile keys the path carries the array index, e.g. `sandbox.profiles[1].memroy_mb`, with one error per profile holding the key. A key inside an unknown table is reported only as the table. Unknown keys are reported **together with** all other static validation failures. Keys outside `[sandbox]` are ignored by this loader.
- SBX-FR-003: After decoding, `${NAME}` (NAME matching `[A-Za-z_][A-Za-z0-9_]*`) is replaced in every string value of the section: scalars, list entries, and env map values (not map keys). The replacement is the env var's value, or empty if it is unset. A bare `$` or a malformed reference is kept. Replacements are not re-expanded. The rest of the file is not expanded by this loader.
- SBX-FR-004: Static validation collects every failure and joins them one per line. A profile is named by `profile "<name>"`, or `sandbox.profiles[i]` when it has no name. No error contains an env value or any part of `gateway_token`. The fatal start error text is `sandbox: cannot enable sandboxes with the configuration in <path>:\n<errors>`. Exact failure messages are listed in §10.9.
- SBX-FR-005: Provider-specific keys are refused under the other provider: under `gateway`, non-empty `engine_binary`/`runtime`/`userns` → `sandbox.<key> applies only to sandbox.provider "podman"`. Under `podman`, non-empty `gateway_url`/`gateway_token` → `... applies only to sandbox.provider "gateway"`.
- SBX-FR-006: Startup order, after static validation passes:
  - (a) `podman`: build the Podman/Docker provider with `engine_binary` (default `podman`), `runtime`, `userns`, and an engine environment equal to the hub's environment **minus `XDG_CONFIG_HOME` and `XDG_DATA_HOME`** (no other variable is removed). A build failure gives `sandbox: cannot build sandbox.provider "podman" from sandbox.engine_binary "<bin>": <err>`. Then probe with one provider `Reap` within 10 s; that removes AgentKit-labelled containers/volumes whose expiry label has passed. A failure gives `sandbox: sandbox.provider "podman" failed its start-up check through sandbox.engine_binary "<bin>": <err>`.
  - (b) `gateway`: dial the remote (`gateway.Dial` with `gateway_token`; reads capabilities) and call `List` once, both under one shared 10 s limit. Nothing is created or destroyed. A failure gives `sandbox: sandbox.provider "gateway" cannot use the remote gateway at sandbox.gateway_url "<url>" with sandbox.gateway_token: <scrubbed text>`, prefixed with `no answer within 10s: ` on timeout. The token is replaced by `[redacted]` and URL userinfo is removed. The remote's capabilities are snapshotted once for the life of the process, and its `Notes` are scrubbed of the token, URL and host.
  - (c) Every `network_modes[i]` must be a mode the provider honours, else `sandbox.network_modes[i] "<m>" is not a network mode of provider "<p>" (it honours [...])`.
  - (d) Each profile is turned into a sandbox spec (image, env, resources with MiB→bytes ×1048576, network, ttl, landlock) and checked with AgentKit `CheckSpec` against the capabilities. A refusal gives `profile "<name>": <reason>`. A memory or disk value that overflows gives `memory_mb N is too large` / `disk_mb N is too large`.
- SBX-FR-007: Sandbox init runs after apikit bootstrap (first-boot admin token) and the PAT scope migration, and before the workspace root, queues, workers and routes are set up. Any failure is fatal (exit 1).
- SBX-FR-008: The `sandboxes:read` and `sandboxes:write` PAT scopes are registered at every start, whether or not sandboxes are enabled.
- SBX-FR-009: Gateway construction (only when enabled), after the lease table exists. Steps in order:
  1. Load the TLS key pair if configured; failure → fatal `sandbox.tls_cert and sandbox.tls_key: cannot load the key pair: <err>`. TLS min version is 1.2.
  2. Map `network_modes` to provider modes.
  3. Bind TCP `listen`; failure → fatal `sandbox.listen "<addr>": cannot bind the sandbox gateway: <err>`.
  4. Build the gatewayd server in callback-auth mode, with `MaxSandboxes = max_sandboxes`, `MaxTTL = max_ttl`, network ceiling `network_modes` and `ReapInterval = reap_interval`.
  5. Run startup recovery (SBX-FR-060) and the first sweep, and only then start serving, so no gateway connection is accepted before that work finishes. Failures in recovery or the sweep are logged as a warning (`the sandbox gateway starts all the same, but its start-up work reported failures`) and do not stop the start.
  6. Log `the sandbox gateway is serving` with `url`.
  7. If serving later ends with an error, log it and exit the process with status 1.
- SBX-FR-010: `gateway_url` shown on every lease is `external_url` verbatim when set. Otherwise it is `https://` (with TLS) or `http://` followed by the listener's actual address (so `:0` shows the bound port). It is the same on every lease in every state and is computed by the running process, not stored.

**Authorization (REST)**

- SBX-FR-020: Credential scoping. API keys and the admin token are not scope-checked. A PAT needs `sandboxes:read` or `sandboxes:write` for reads (write implies read), and `sandboxes:write` for POST/DELETE (read does not imply write). Any other credential type is refused with the same 403 message.
- SBX-FR-021: Check order on per-workspace lease routes:
  1. No credential → 401 `authentication required`.
  2. Feature off → 404 `sandboxes are not enabled on this hub`, `error_type: sandboxes_disabled`.
  3. Scope → 403 `PAT requires sandboxes:write scope` / `PAT requires sandboxes:read scope` (no error_type).
  4. Workspace access via the shared workspace rule (read for GETs, write for POST/DELETE). A missing or invisible workspace, or a PAT bound to another workspace, → 404 `workspace not found`. An org member on write → 403 `org members have read-only access to this workspace`, `error_type: forbidden`. A DB error → 500.

  The bound-token guard middleware runs before the handler: a bound PAT addressing another slug gets 404 before any of these checks.
- SBX-FR-022: `GET /api/v1/sandbox-profiles` order: 401 → 404 `sandboxes_disabled` → 403 scope. It is on the bound-token allowlist.
- SBX-FR-023: `GET /api/v1/sandboxes` order: (bound-token guard: a bound PAT → 403 `token_bound` `this token is bound to workspace <S>`) → 401 → 404 `sandboxes_disabled` → non-admin credential → 403 `admin token required` (no error_type) → `all` parse.

**Issuing a lease**

- SBX-FR-030: POST check order: authorization (SBX-FR-021) → archived workspace → 400 `workspace is archived` → body parsing → parameter resolution → required-secret check → id/token generation → capacity+insert → event.
- SBX-FR-031: Body parsing:
  - The body is read fully. Exceeding the server body limit gives 413 `request body is too large`. Any other read error gives 400 `could not read the request body`.
  - After trimming whitespace, an empty body counts as `{}`. A first byte that is not `{` gives 400 `request body must be a JSON object`; that covers `null`, arrays and scalars.
  - The body is decoded as a JSON object of raw fields. Invalid JSON, including trailing data such as `{} {}` or `{"a":1}]`, gives 400 `request body is not valid JSON`.
  - Field names are matched exactly and case-sensitively. Any name other than `profile`, `purpose`, `ttl`, `labels` gives 400 `unknown field "<name>": only profile, purpose, ttl and labels are accepted`. The first unknown name in sorted order is the one reported.
  - A type mismatch gives 400 `field "<f>" must be a string|an object, not a number|an array|a string|...`.
  - A JSON `null` for `profile` or `ttl` counts as absent. `null` for `purpose` counts as `""`, and `null` for `labels` as no labels.
- SBX-FR-032: Profile. If absent, the default profile is used; with no default the error is 400 `profile is required: no profile is marked as the default; known profiles: <names in config order, ", "-joined>`. If given, it must match a name exactly (case-sensitive; `""` is unknown), else 400 `unknown sandbox profile "<p>"; known profiles: <names>`.
- SBX-FR-033: TTL. If absent, the profile's TTL is used. If given, it is a Go duration:
  - An unparseable value gives 400 `ttl "<v>" is not a valid duration such as 30m or 1h`.
  - Less than 1 m (negative values included) gives 400 `ttl "<v>" is shorter than the minimum of 1m`.
  - More than the profile TTL gives 400 `ttl "<v>" is longer than the ttl of profile "<p>", <short>`, where `<short>` is the duration without zero units (`1h`, `30m`, `1h30m`).
  - The effective TTL is truncated to whole seconds after validation.
- SBX-FR-034: Purpose. More than 64 runes gives 400 `purpose is longer than 64 characters`. Any Unicode control character (including U+0085) gives 400 `purpose must not contain control characters`. The default is `""`.
- SBX-FR-035: Labels. Keys are processed in ascending byte order; the first violation is reported.
  - A key at index ≥16 gives `label "<k>" is over the limit of 16 labels`. This is checked first for that key, so it names the first key past the 16th in sorted order.
  - A key not matching `^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$` gives `label key "<k>" is not 1 to 63 characters of letters, digits, '.', '_' and '-', starting and ending with a letter or digit`.
  - A key equal case-insensitively to `lease` or `workspace` gives `label key "<k>" is reserved`.
  - A non-string value gives `value of label "<k>" must be a string`.
  - A value over 256 bytes gives `value of label "<k>" is longer than 256 bytes`.
  - A control character in the value gives `value of label "<k>" must not contain control characters`.

  All of these are 400. An empty value is allowed.
- SBX-FR-036: Required-secret presence check, before the insert. Build the resolution chain for the workspace:
  1. `workspace` scope (owner id = slug).
  2. `org` scope (the workspace's org id), only if the workspace has an org **and the workspace owner** is a member of it.
  3. `user` scope = the **workspace owner's** user id, never the caller's.

  List key names of every scope (values are never read) and match the profile's `secrets` case-insensitively. Optional secrets are not checked. If any are missing: 422 `required secrets not found: <missing in profile order, ", "-joined> (searched scopes: <scope types in chain order>)`, `error_type: secret_missing`. Nothing is stored.
- SBX-FR-037: The clock is truncated to whole seconds. `created_at = updated_at = now`, `expires_at = now + ttl`, `lease_expires_at = now + min(lease_timeout, ttl)`, `ttl_seconds = ttl/1s`. The full profile is snapshotted (image, network, resources, landlock, secrets, optional_secrets, env), along with the caller's labels. `created_by` is `admin` or the user id.
- SBX-FR-038: Capacity. Under an in-process mutex and inside one DB transaction:
  - Count leases, across all workspaces, where `state IN (leased, active) AND expires_at > now AND NOT (state='leased' AND lease_expires_at IS NOT NULL AND lease_expires_at <= now)`.
  - If `count >= max_sandboxes`, answer 409 `sandbox capacity reached: at most <N> leases may be live at once (max_sandboxes)`, `error_type: sandbox_capacity`.
  - Otherwise insert and commit.

  Concurrent POSTs never exceed the limit. Any failure rolls back and leaves no row.
- SBX-FR-039: Failures of the database or the random source → 500 `internal server error`. The cause is logged; it never contains a token.
- SBX-FR-040: Response 201: the lease representation plus `token`. A `hub.sandbox.lease` event is emitted after commit. An emit failure is logged and does not undo the lease.

**Reading**

- SBX-FR-041: The workspace list returns leases of the workspace, newest first (`created_at DESC, id DESC`), capped at 200. By default only live leases are included. `?all=<bool>` uses Go `strconv.ParseBool` (`1,t,T,TRUE,true,True,0,f,F,FALSE,false,False`), and only the first value of a repeated param is used. A present but unparseable value, including empty, gives 400 `query parameter all must be true or false`. `all=true` includes every terminal row still in the table (rows are pruned 24 h after ending, see SBX-FR-075). It works on archived workspaces.
- SBX-FR-042: GET one: the id must match `^sbx_[0-9a-f]{32}$` and belong to the workspace. A malformed id, an unknown id, another workspace's id or a pruned lease all give 404 `sandbox lease not found` (no error_type). Any state is returned. It works on archived workspaces.
- SBX-FR-043: Admin list: like SBX-FR-041 but across all workspaces.
- SBX-FR-044: Lists are bare JSON arrays (`[]` when empty). In every lease representation, list/map fields are never null (`[]`, `{}`). No response other than the POST 201 contains `token`. None contains `token_hash` or `env`.

**Ending a lease via DELETE**

- SBX-FR-050: Order: authorization (write) → lookup (404 `sandbox lease not found`, as in SBX-FR-042) → end. It works on archived workspaces.
- SBX-FR-051: The end reason is `api`, or `shutdown` while the hub is in shutdown mode.
  - **leased**: transition `leased→destroyed` with no provider call. If a Create for it is in flight, its box is destroyed as soon as it exists and that Create fails.
  - **active**: first destroy the box, bounded to 30 s, then `active→destroyed`. Box destruction tries the gatewayd registry's Destroy first. If gatewayd does not hold the box, the hub uses its own retained handle (and for a remote provider after an earlier failed close, destroys by id through the remote client). A box that is already gone, or one the hub holds no handle for (for example from a previous process), counts as destroyed. If the provider fails or times out, the answer is 502 `<scrubbed provider message>`, `error_type: sandbox_provider_error`, and the lease is unchanged (still `active`, token still valid). The DELETE is retryable.
  - **terminal**: nothing happens (no provider call, no event, original `ended_at`/`end_reason` kept).
  - Race handling: if the conditional update finds the lease in a different state, the lease is re-read once and the new state is acted on (a box that appeared is destroyed first; a lease that ended means there is nothing left to do).
  - The answer is always 204 with no body on success, including the no-op case. Each actual change to `destroyed` emits one `hub.sandbox.destroy` with the HTTP caller as actor.
- SBX-FR-052: Scrubbing rule for any provider error text that is stored, logged, emitted or returned:
  - Every value from the lease's resolved secrets and fixed env is replaced, along with (for a remote provider) the hub's gateway token, the `gateway_url` string and its host (with and without port). The replacement is `[redacted]`, longest value first.
  - URL userinfo (`scheme://user:pass@`) is removed.
  - The text is repeated until stable (≤8 passes). If a value still remains, the whole text becomes `[redacted]`.
  - Finally the text is truncated to 1024 bytes on a UTF-8 boundary.

**Gateway (gRPC), as seen by clients**

- SBX-FR-060: Startup recovery. Every lease in state `active` at start belongs to a previous process. Its box is destroyed if this process holds it (it never does after a restart), and the lease goes `active→destroyed` with reason `restart` and one `hub.sandbox.destroy` with actor system. A failure to destroy a box does not keep the lease. A warning is logged with the count and the box ids (those boxes live until their provider TTL or the provider's reap). Then the active gauge is recalibrated. `leased` leases are left untouched and remain usable after a restart.
- SBX-FR-061: Authentication. Every RPC on both gRPC services (Gateway and the relayed Agent) must carry exactly one `authorization: Bearer <token>` metadata entry. The token is accepted only if it matches `^sbl_[A-Za-z0-9_-]{44}$`, its SHA-256 matches `token_hash` of a lease in state `leased`/`active` (DB lookup on every RPC, nothing cached), and `expires_at` is after now. The principal is the lease id. Anything else gives gRPC `Unauthenticated` `gatewayd: a valid bearer token is required`, which does not say why. It applies to Dial (Capabilities) too. A lease that DELETE or a failure has ended is refused from the next RPC, including relayed calls to an existing box.
- SBX-FR-062: Capabilities RPC: the provider's capabilities (the start-time snapshot for a remote), with network modes restricted to `network_modes` in provider order. Gateway protocol is 1.0, `max_ttl_nanos = max_ttl`, `max_sandboxes = max_sandboxes`.
- SBX-FR-063: Before the hub sees a Create, gatewayd itself checks the **caller's** spec:
  - `Spec.Provider` JSON guard (InvalidArgument).
  - The network mode (zero = `none`) must be offered, else an UNSUPPORTED refusal on `Network.Mode`.
  - The TTL (zero = 1 h) must be ≤ `max_ttl`, else an UNSUPPORTED refusal on `TTL`.
  - The gateway must hold fewer than `max_sandboxes` boxes, else a CAPACITY refusal `gatewayd: the gateway holds its maximum of N sandboxes`.

  Clients therefore must send a TTL and a network mode that fit, even though the hub ignores both.
- SBX-FR-064: Create (hub backend), in order:
  1. If in shutdown mode, refuse: `hub is shutting down and creates no more sandboxes`.
  2. Claim the lease in-process. A concurrent Create on the same lease gives `a Create for lease <id> is already in progress`.
  3. Load the lease. A missing lease gives `lease <id> not found`. A state other than `leased` gives `lease <id> is <state>: only a leased lease can create its box`.
  4. Compute `remaining = expires_at − now`, truncated to seconds. If `lease_expires_at` (or `expires_at` for old rows) ≤ now, or remaining ≤ 0, end the lease `leased→expired` (reason `expired`, event expire) and refuse with `lease <id> has expired`. If another path ended it first, refuse with `lease <id> is <state>`.
  5. If the profile name is no longer configured, the lease goes to `failed` with `profile "<p>" is no longer configured`.
  6. Build the spec **from the lease**:
     - image, fixed env, resources (MiB→bytes), network mode (must still be honoured, else the lease fails with `network mode "<m>" is not honoured by provider "<p>"`), and landlock from the lease;
     - TTL = remaining;
     - Workdir = caller's;
     - Labels = the caller's labels minus every key starting `hub.`, `agentkit.sandbox` and keys starting `agentkit.sandbox.`; then each lease label as `hub.<key>`; then `hub.lease=<lease id>` and `hub.workspace=<slug>` (these override).

     The caller's image, env, network, resources, TTL and landlock are ignored.
  7. Resolve the secret values (required and optional) via the same chain as SBX-FR-036. The first scope having the name, matched case-insensitively, wins. Values are added to the env under the **profile's spelling** of the name. A missing required secret sends the lease to `failed` with `secret_missing: <names> (searched scopes: <types>)`. A missing optional one is omitted. A secrets-store error leaves the lease `leased` (Internal `internal error`) and the call can be retried. Values are read at create time, so a rotation between issue and create is honoured. Only **secrets** are injected; workspace/user/org *variables* are not.
  8. Call the provider. On error, the lease goes `leased→failed` with the scrubbed message and the caller gets the scrubbed message. A remote that refuses the hub's token is returned as Unavailable and logged once, naming `sandbox.gateway_token`.
  9. On success, record `leased→active` with `sandbox_id`. If the record fails (DB error), destroy the new box (30 s bound, warning logged); the lease stays `leased` and the caller gets Internal `internal error`. If the lease is no longer `leased` (DELETE won the race), destroy the new box; there is no create event, and the call fails with `lease <id> is <state>`.
  10. Emit `hub.sandbox.create`. Return the box; its reported spec has every env value replaced by `[redacted]` (keys kept).

  Every "goes to failed" emits `hub.sandbox.failed` and returns the message to the caller. If the lease is no longer `leased` at that point, nothing changes and the caller gets `lease <id> is <state>`.

  Wire mapping: gatewayd turns every hub backend error into gRPC `Unavailable` with message `gatewayd: backend create: <message>`. Exactly one box ever exists per lease.
- SBX-FR-065: List (Gateway.List) returns only boxes owned by the caller's lease (0 or 1). Gateway.Destroy of another owner's id, or an unknown id, answers OK and does nothing. Agent-service relays (`Hello`, `Tools`, `Call`, `SyncTar`, `ExportTar`, `Rsync`) and `SyncGit` reach only the caller's own box, named by metadata `agentkit-sandbox-id`. A missing, malformed or foreign id gives Unavailable. Messages are limited to 16 MiB.
- SBX-FR-066: Client close (Gateway.Destroy of own box, or the client's `Close`): the hub destroys the provider box.
  - On failure the client gets the scrubbed error (Unavailable from gatewayd). The lease stays `active`. gatewayd forgets the box but the hub keeps its handle, so a later DELETE or sweep can retry.
  - On success, if no DELETE/sweep/shutdown is concurrently ending the same lease, and `expires_at` is more than 2 s in the future, the lease goes `active→destroyed` with reason `client` (`shutdown` in shutdown mode) and one `hub.sandbox.destroy` with actor `lease`.
  - A close within the last 2 s of the lease, or after it, destroys the box but leaves the lease to the sweep, which ends it with reason `expired`. That is the case when gatewayd's own TTL timer closes the box.
  - A failure to record the end is logged, not returned.
- SBX-FR-067: Gateway.Reap from a lease holder does nothing and returns an empty response. A Reap without a principal (gatewayd's own loop) runs the sweep.

**Lifecycle**

- SBX-FR-070: Sweep. Sweeps are serialised (a caller waits; a waiter whose context ends returns). None runs in shutdown mode. Steps run in this order; a failing step is logged (`sandbox expiry: a step of the sweep failed`, with `step`) and later steps still run:
  1. `reap`: the provider's Reap (podman: removes expired AgentKit-labelled leftovers; remote: the remote's Reap RPC), 30 s bound.
  2. `lapse`: every `leased` lease with `COALESCE(lease_expires_at, expires_at) <= now`, oldest first → `expired`/`expired`, event expire.
  3. `overdue`: every `active` lease with `expires_at <= now`. Destroy its box (30 s bound; a failure is logged and does not keep the lease, but a cancelled context does), then `active→destroyed` reason `expired`, one `hub.sandbox.expire` (state `destroyed`, `sandbox_id`), no destroy event.
  4. `orphans`: for every box the hub still holds a handle of, if its id is the `sandbox_id` of a terminal lease, or its `hub.lease` label names a terminal lease, destroy it (30 s bound; a failure is logged and retried next sweep). Other boxes are left alone.
  5. `prune`: `DELETE` terminal leases with `COALESCE(ended_at, updated_at) < now − 24h` (exactly 24 h stays). Live leases are never deleted.
  6. `recalibrate`: set `afhub_sandboxes_active` from `SELECT workspace, COUNT(*) ... WHERE state='active' GROUP BY workspace`.
- SBX-FR-071: Sweep schedule. One sweep runs synchronously at startup (before serving). The hub's reaper sweeps every `reap_interval`. gatewayd also calls Reap (and so the sweep) when it starts serving and every `reap_interval` (its context is bounded by `reap_interval`). The two schedules are independent but serialised.
- SBX-FR-072: Shutdown. This runs after the HTTP server's Start returns (signal or error), while the DBs are still open, with a 20 s total limit:
  1. Enter shutdown mode permanently: Creates are refused, sweeps are skipped, and every ending uses reason `shutdown`.
  2. Stop the reaper and wait for any running sweep.
  3. Concurrently, for each `active` lease, destroy its box and `active→destroyed` reason `shutdown`, emitting one `hub.sandbox.destroy` with actor system.
  4. Stop the gatewayd server, which closes any box it still holds.
  5. Close the remote client, if any. That client destroys boxes it still holds.

  `leased` leases are not changed. When the limit runs out, unfinished leases stay `active` for recovery at the next start, and an error is logged: `the sandbox shutdown did not finish; the leases still active are ended at the next start`. A second call does nothing.
- SBX-FR-073: A crash runs none of SBX-FR-072. The next start applies SBX-FR-060.
- SBX-FR-074: While sandboxes are disabled, the lease table still exists (and is migrated), but no sweep, prune, recovery or gateway runs. Rows stay as they are.
- SBX-FR-075: Retention is fixed at 24 h after `ended_at` (or `updated_at`). Because pruning happens in the sweep, a terminal lease is readable for between 24 h and 24 h + sweep spacing.

### 10.5 REST API

All paths are under `/api/v1`. Error envelope: `{"error":{"code":<status>,"message":"...","error_type":"..."}}` (`error_type` only where stated).

#### `GET /api/v1/sandbox-profiles`
- **Purpose:** List the deployment's profiles.
- **Auth / scope:** Any credential. A PAT needs `sandboxes:read` or `sandboxes:write`. No workspace access needed. A bound PAT is allowed (allowlisted route).
- **Path/query params:** none.
- **Request body:** none.
- **Behaviour:** The response is served from memory: no DB read and no provider call, so it keeps answering when the engine is down. Every caller gets the same list.
- **Response:** 200
  ```json
  {"profiles":[{"name":"agent","default":true,"image":"registry.example.com/agent:1.4","network":"none","ttl_seconds":1200,
    "resources":{"cpus":2,"memory_mb":4096,"pids":512,"disk_mb":0},
    "secrets":["ANTHROPIC_API_KEY","GIT_PAT"],"optional_secrets":["GH_TOKEN"],"env_keys":["LANG","TZ"],"require_landlock":true}]}
  ```
  Profiles are sorted by `name`. `secrets`, `optional_secrets` and `env_keys` are each sorted ascending and are always arrays. Nothing of the provider or the deployment limits is included.
- **Errors:** 401 `authentication required`. 404 `sandboxes are not enabled on this hub` (`sandboxes_disabled`). 403 `PAT requires sandboxes:read scope`.
- **Side effects:** none.

#### `POST /api/v1/workspaces/:slug/sandboxes`
- **Purpose:** Issue a lease.
- **Auth / scope:** API key, admin token, or PAT with `sandboxes:write`. Workspace **write** access (owner or admin). A bound PAT works only on its own slug.
- **Path params:** `slug`.
- **Request body:** optional JSON object; an empty body counts as `{}`.

  | Field | Type | Req | Default | Validation |
  |---|---|---|---|---|
  | `profile` | string | no | the default profile | exact name (SBX-FR-032) |
  | `purpose` | string | no | `""` | ≤64 runes, no control chars |
  | `ttl` | string | no | profile TTL | Go duration, 1m ≤ ttl ≤ profile TTL, truncated to seconds |
  | `labels` | object of strings | no | `{}` | SBX-FR-035 |

  Any other field → 400.
- **Behaviour:** SBX-FR-030…040.
- **Response:** 201 with the lease representation (see GET one) plus `"token":"sbl_<44>"`:
  ```json
  {"id":"sbx_3f2a9c0e7b1d4c5a8e6f0b2d1c3a4e5f","workspace":"widgets","profile":"tools","purpose":"triage","state":"leased",
   "gateway_url":"https://sandboxes.example.com","expires_at":"2030-01-02T04:04:05Z","lease_expires_at":"2030-01-02T03:09:05Z",
   "image":"registry.example.com/tools:1","network":"none","resources":{"cpus":1,"memory_mb":512,"pids":100,"disk_mb":0},
   "require_landlock":true,"secrets":["GIT_PAT"],"optional_secrets":[],"labels":{"team":"red"},"created_by":"user-1",
   "created_at":"2030-01-02T03:04:05Z","updated_at":"2030-01-02T03:04:05Z","sandbox_id":null,"error":null,"ended_at":null,
   "end_reason":null,"token":"sbl_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
  ```
- **Errors (in check order):**
  - 401
  - 404 `sandboxes_disabled`
  - 403 `PAT requires sandboxes:write scope`
  - 404 `workspace not found`
  - 403 `forbidden` (org member)
  - 400 `workspace is archived`
  - 413 `request body is too large`
  - 400 body/field errors (SBX-FR-031…035)
  - 422 `secret_missing`
  - 409 `sandbox_capacity`
  - 500 `internal server error`
- **Side effects:** inserts a `sandbox_leases` row (`leased`, token hash) and emits `hub.sandbox.lease`.

#### `GET /api/v1/workspaces/:slug/sandboxes`
- **Purpose:** List the workspace's leases.
- **Auth / scope:** PAT `sandboxes:read` or `sandboxes:write`; API key/admin. Workspace **read** access (members included).
- **Query:** `all` (bool, default false; ParseBool rules; invalid or empty → 400 `query parameter all must be true or false`).
- **Behaviour:** SBX-FR-041. Archived workspaces are allowed.
- **Response:** 200, a JSON array of lease representations (`[]` if none), newest first, ≤200.
- **Errors:** 401, 404 `sandboxes_disabled`, 403 scope, 404 workspace, 400 `all`, 500.
- **Side effects:** none.

#### `GET /api/v1/workspaces/:slug/sandboxes/:id`
- **Purpose:** Read one lease in any state.
- **Auth / scope:** as for the list (read).
- **Path params:** `id` must match `^sbx_[0-9a-f]{32}$` (otherwise 404).
- **Response:** 200 with the lease representation. Fields, in order, all always present:
  - `id`, `workspace`, `profile`, `purpose` (string)
  - `state` (enum)
  - `gateway_url` (string)
  - `expires_at` (string)
  - `lease_expires_at` (string|null)
  - `image`, `network` (string)
  - `resources` {`cpus` number, `memory_mb`, `pids`, `disk_mb` int}
  - `require_landlock` bool
  - `secrets`, `optional_secrets` (string[] in profile order)
  - `labels` (object)
  - `created_by`, `created_at`, `updated_at` (string)
  - `sandbox_id`, `error`, `ended_at`, `end_reason` (string|null)

  No `token`, no env values.
- **Errors:** 401, 404 `sandboxes_disabled`, 403 scope, 404 workspace, 404 `sandbox lease not found`, 500.

#### `DELETE /api/v1/workspaces/:slug/sandboxes/:id`
- **Purpose:** End a lease, destroy its box and revoke the token.
- **Auth / scope:** PAT `sandboxes:write`; API key/admin. Workspace **write** access.
- **Behaviour:** SBX-FR-050/051. Archived workspaces are allowed.
- **Response:** 204, no body, whether or not anything changed.
- **Errors:**
  - 401
  - 404 `sandboxes_disabled`
  - 403 `PAT requires sandboxes:write scope`
  - 404 `workspace not found`
  - 403 `forbidden` (org member)
  - 404 `sandbox lease not found`
  - 502 `<scrubbed provider message>` `sandbox_provider_error` (lease unchanged)
  - 500
- **Side effects:** the lease moves to `destroyed` (reason `api`/`shutdown`), `token_hash` is cleared, the box is destroyed, and one `hub.sandbox.destroy` is emitted (actor = HTTP credential) per actual change.

#### `GET /api/v1/sandboxes`
- **Purpose:** Admin-wide list of leases.
- **Auth / scope:** admin token only. An API key or any PAT → 403 `admin token required`. A bound PAT → 403 `token_bound` from the guard.
- **Query:** `all` as above (parsed after the admin check).
- **Response:** 200 array, newest first, ≤200, all workspaces.
- **Errors:** 401, 404 `sandboxes_disabled`, 403, 400, 500.

#### Gateway protocol surface (gRPC, not REST)

The gateway serves package `agentkit.sandbox.v1`, plaintext or TLS (≥1.2), on `[sandbox].listen`.
- Service `Gateway`:
  - `Capabilities` (no request fields). Returns protocol_major/minor, version, provider, isolation, network[], resource flags, image/workdir/landlock flags, notes, `max_ttl_nanos`, `max_sandboxes`.
  - `Create(spec)` returns either a `refusal` or `{id, spec (env redacted), created_unix_nanos, expires_unix_nanos}`.
  - `SyncGit(id,url,ref,depth)`
  - `List` returns the caller's 0 or 1 entries `{id, labels, created, expires}`.
  - `Destroy(id)` is OK for unknown or foreign ids.
  - `Reap` is a no-op for lease holders.
- Service `Agent` (relayed to the box): `Hello`, `Tools`, `Call`, `SyncTar` (client stream), `ExportTar` (server stream), `Rsync` (bidi). The box is selected by metadata `agentkit-sandbox-id`.
- Credential: `authorization: Bearer sbl_...` on every RPC (SBX-FR-061). There are no static tokens. Errors are listed in SBX-FR-061…067.
- Client reference: `gateway.Dial(ctx, lease.gateway_url, gateway.Options{Token: token})`, then `Create(ctx, sandbox.Spec{Image: lease.image, TTL: time-left, Network: lease's mode if not none})`. The AgentKit client refuses `http://` to a non-loopback host.

### 10.6 CLI

All commands are subcommands of `parley sandbox` (`Short: Work with sandboxes`). Credentials are resolved by the apikit scaffolding, and requests go to `<endpoint>/api/v1<path>` with `Authorization: Bearer`, using a 30 s default timeout.
- Success output is indented JSON (2 spaces, no HTML escaping) on stdout. Objects decoded from responses print with **alphabetically sorted keys**.
- API errors print `{"error":{"code":<HTTP status>,"message":"..."}}` on stdout and exit 1. The `error_type` from the hub is not shown by parley, because the client decodes only code and message.
- Client and usage errors print with code 2 and exit 2. Cobra argument-count errors print as `{"error":{"code":0,"message":"accepts 1 arg(s), received 0"}}` and exit 2.
- Path segments are URL path-escaped.

#### `parley sandbox profiles`
- **Purpose:** List profiles.
- **Arguments / flags:** none (`NoArgs`).
- **Calls:** `GET /api/v1/sandbox-profiles`.
- **Output:** the response JSON on stdout.
- **Exit codes:** 0, 1 (API error, e.g. 404 `sandboxes are not enabled on this hub`), 2.

#### `parley sandbox create <workspace-slug>`
- **Purpose:** Issue a lease.
- **Arguments / flags:**
  - `<workspace-slug>` (exactly 1 argument)
  - `--profile string` ("Profile name (default: the hub's default profile)")
  - `--purpose string` ("What the sandbox is for (at most 64 characters)")
  - `--ttl string` ("Lifetime of the lease, such as 30m (default: the profile's)"); sent unparsed
  - `--label key=value` (repeatable string array)
  - `--token-file path` ("Write the lease token to this file (mode 0600) instead of printing it")

  Only flags explicitly given (`Changed`) are sent, so `--profile ""` sends `"profile":""`. `labels` is sent only when at least one `--label` is given.
- **Validation (before any request, before the client is built), exit 2:**
  - A label without `=` → `invalid argument: missing '=' in '<arg>'`. The split is on the first `=`, so values may contain `=`, and an empty key is passed to the server.
  - A duplicate label key → `label "<k>" is given more than once`.
  - With `--token-file`, the file is opened **before** the request with `O_CREATE|O_TRUNC|O_WRONLY` mode 0600 plus an explicit chmod 0600. Failure → exit 2 `cannot open the token file <path>: <err>`.
- **Calls:** `POST /api/v1/workspaces/<slug>/sandboxes`. On a token write failure, `DELETE .../sandboxes/<id>`.
- **Output:**
  - Without `--token-file`: stderr `warning: the token in this output will not be shown again; save it now`, then stdout the full lease **including** `token`.
  - With `--token-file`: the token is written with no trailing newline, the file is closed, and stdout shows the lease **without** `token`.
  - If the token is empty, or the write or close fails, the CLI tries to DELETE the lease and exits 1 with `cannot write the token to <path>: <err>; <outcome>`. `<outcome>` is one of:
    - `the lease <id> was released`
    - `the lease <id> could not be released; end it with: parley sandbox destroy <slug> <id>`
    - `the hub returned no lease ID`

    An empty token gives the error `the hub returned no token`. The token is never printed to stdout as a fallback.
  - If the hub refuses the POST, the token file is left truncated (empty).
- **Exit codes:** 0, 1 (API error or token write failure), 2 (usage).

#### `parley sandbox list <workspace-slug>`
- **Arguments / flags:** `<workspace-slug>` (exactly 1); `--all` bool, default false ("Include leases that have ended"). It appends `?all=true`.
- **Calls:** `GET /api/v1/workspaces/<slug>/sandboxes[?all=true]`.
- **Output:** the JSON array on stdout.
- **Exit codes:** 0/1/2.
- **Notes:** no parley command exists for `GET /api/v1/sandboxes`.

#### `parley sandbox get <workspace-slug> <lease-id>`
- **Arguments:** exactly 2.
- **Calls:** `GET /api/v1/workspaces/<slug>/sandboxes/<id>`.
- **Output:** the lease JSON (no token).
- **Exit codes:** 0, 1 (e.g. `sandbox lease not found`), 2.

#### `parley sandbox destroy <workspace-slug> <lease-id>`
- **Purpose:** "End a sandbox lease and revoke its token".
- **Arguments:** exactly 2.
- **Calls:** `DELETE /api/v1/workspaces/<slug>/sandboxes/<id>`.
- **Output:** stderr `Sandbox lease <id> destroyed.` and stdout `{"id":"<id>","status":"destroyed"}` (pretty-printed, keys `id`, `status`). The output is the same for an already ended lease.
- **Exit codes:** 0, 1 (404, 502, ...), 2.

### 10.7 Background behaviour

| Activity | When | Effect |
|---|---|---|
| Schema init + `MigrateLeaseExpiry` | every start, always | creates `sandbox_leases` and its indexes; adds `lease_expires_at` if missing; fatal on error |
| Provider probe | start, if enabled | podman: one `Reap` (10 s); gateway: Dial + List (10 s shared) |
| Gateway bind | start, if enabled | TLS load, bind `listen`; fatal on error |
| Recovery | start, before serving | SBX-FR-060 |
| First sweep | start, before serving, synchronous | SBX-FR-070 |
| Reaper sweep | every `reap_interval` (default 10 m) | SBX-FR-070 |
| gatewayd reap → sweep | at serve start and every `reap_interval` | same sweep, serialised |
| gatewayd per-box TTL timer | at box `expires` (created + effective TTL) | closes the box; lease left to the sweep (2 s margin rule) |
| Shutdown | after the HTTP server stops; 20 s | SBX-FR-072 |
| Gateway serve error | any time | log + `exit(1)` |

Every provider call in endings and sweeps is bounded to 30 s, and a timeout is logged as `<call> timed out after 30s`. A DELETE box destruction is bounded to 30 s. Discarding a box created for nothing is bounded to 30 s.

Metrics (Prometheus, on `/metrics`, listed from the start):
- `afhub_sandboxes_active{workspace}` gauge: +1 on `leased→active`; −1 when a lease that had a `sandbox_id` ends; recalibrated from the table each sweep and after recovery. The placeholder `workspace=""` is kept while no lease is active.
- `afhub_sandbox_creates_total{result="created"|"failed"}`: counts Creates that passed the claim, state and deadline checks. A failure in any later step counts as `failed`, store errors included.
- `afhub_sandbox_lease_seconds` histogram (buckets 60, 300, 600, 1200, 1800, 3600, 7200, 14400, 28800): `ended_at − created_at`, observed for every transition into `destroyed` (any reason). `expired` and `failed` endings are not observed.

### 10.8 Events

All events have `resource_type: "sandbox"`, `resource_id` = lease id and `workspace` = lease workspace. The metadata never contains a secret value, an env value or the token. Emit failures are logged and never change the outcome. Emission is not cancelled by the caller's context going away.

| event_type | action | actor_type / actor_id | When | metadata |
|---|---|---|---|---|
| `hub.sandbox.lease` | `lease` | caller credential type (`api_key`/`pat`/`admin_token`) / caller user id | POST committed | `lease_id`, `profile`, `purpose`, `ttl` (Go `Duration.String()` of the effective TTL, e.g. `1h0m0s`, `20m0s`), `secret_names` ([] profile order), `optional_secret_names` |
| `hub.sandbox.create` | `create` | `lease` / lease `created_by` | `leased→active` | `lease_id`, `sandbox_id`, `image`, `network` |
| `hub.sandbox.failed` | `fail` | `lease` / `created_by` | `leased→failed` | `lease_id`, `error` (the stored message) |
| `hub.sandbox.destroy` | `destroy` | DELETE: HTTP credential type / user id. Client close: `lease` / `created_by`. Shutdown/restart: `system` / `""` | each change into `destroyed` except by time | `lease_id`, `sandbox_id` (string or `null`), `reason` (`api`, `client`, `shutdown`, `restart`) |
| `hub.sandbox.expire` | `expire` | `system` / `""` | time ended a lease (sweep lapse/overdue, or a late Create) | `lease_id`, `state` (`expired` or `destroyed`), `sandbox_id` (key present only when a box was destroyed) |

An expiry emits no `hub.sandbox.destroy`. A no-op DELETE emits nothing.

### 10.9 Configuration

`[sandbox]` table. Keys and their decoded types:

| Key | Type | Default | Validation / notes |
|---|---|---|---|
| `provider` | string | required | `podman` or `gateway`. Empty → `sandbox.provider is required (accepted values: "podman", "gateway")`. Other → `sandbox.provider "<v>" is not supported (accepted values: ...)` |
| `engine_binary` | string | `podman` (podman only) | passed unchanged to the provider (bare name looked up in PATH). Refused under gateway |
| `runtime` | string | `""` | e.g. `krun` (micro-VM). Refused under gateway. Provider rejects unsupported values at build |
| `userns` | string | `""` | passed as `--userns=<v>`. Refused under gateway |
| `max_ttl` | duration string | `4h` | parse error → `sandbox.max_ttl: <err>`. ≤0 → `sandbox.max_ttl "<v>" must be greater than zero`. Empty string is an error (not "absent") |
| `network_modes` | []string | `["none"]` | entries non-empty (`sandbox.network_modes[i] must not be empty`) and unique (`sandbox.network_modes[i] "<m>" repeats sandbox.network_modes[j]`). `[]` is accepted (then every profile fails). Each must be honoured by the provider (podman: `none`, `unrestricted`; remote: what it offers) |
| `max_sandboxes` | number (whole) | `16` | `sandbox.max_sandboxes must be a whole number of 1 or more (got <v>)`. Also gatewayd's box limit |
| `listen` | `host:port` | `127.0.0.1:8443` (also when empty) | must split as host:port (`sandbox.listen "<v>" is not a host:port address: <err>`), port 0–65535 (`... the port must be a number from 0 to 65535`). Non-loopback (loopback = `localhost` or a literal loopback IP; no DNS; empty host, `0.0.0.0` and `::` are not loopback) requires TLS (`sandbox.listen "<v>" is not on loopback: a plaintext gateway is allowed only on loopback (set sandbox.tls_cert and sandbox.tls_key)`) **and** `external_url` (`sandbox.external_url is required when sandbox.listen "<v>" is not on loopback`) |
| `tls_cert`, `tls_key` | path | `""` | both or neither (`sandbox.tls_key is required when sandbox.tls_cert is set` and vice versa). PEM loaded at gateway build |
| `external_url` | URL | `""` | absolute `http`/`https`, host required, no userinfo/query/fragment, path empty or `/`. Messages: `not a valid URL`, `must be an absolute http or https URL`, `must name a host`, `must not contain user information`, `must not have a query or a fragment`, `must not have a path other than /` (the value is never echoed) |
| `lease_timeout` | duration | `5m` | ≥1s (`must be at least 1s`), ≤ `max_ttl` when max_ttl is valid (`must be at most sandbox.max_ttl (<d>)`). The default is not compared with max_ttl |
| `reap_interval` | duration | `10m` | 10s–1h (`sandbox.reap_interval "<v>" must be between 10s and 1h0m0s`) |
| `gateway_url` | URL | required under gateway | `https://host[:port]`, or `http` only to a loopback host. Port 1–65535. No userinfo/query/fragment. Path empty or `/`. The first broken rule is reported, without echoing the value (the plaintext rule shows `scheme://host`). Refused under podman |
| `gateway_token` | string | required under gateway | ≥16 runes; only 0x21–0x7E; never echoed; prints as `[redacted]` everywhere. Refused under podman |
| `[[sandbox.profiles]]` | array of tables | ≥1 required (`sandbox.profiles: at least one profile is required`) | per §10.3.1 |

Profile messages (prefix `profile "<n>": ` or `sandbox.profiles[i]: `):
- `name is required`
- `name must match ^[a-z0-9][a-z0-9_-]{0,62}$`
- `name is already used by sandbox.profiles[j]`
- `image is required and must not be empty`
- `cpus must be a number of 0 or more (got v)`
- `memory_mb|pids|disk_mb must be a whole number of 0 or more (got v)`
- `network "<n>" is not one of sandbox.network_modes [...]`
- `ttl is required`
- `ttl: <err>`
- `ttl "<v>" must be greater than zero`
- `ttl "<v>" is greater than sandbox.max_ttl (<d>)`
- `secrets[i]: <key error>` / `optional_secrets[i]: <key error>`
- `<list>[i] "<n>" repeats <list>[j] "<m>" (names are compared ignoring case)`
- `env: <key error>`
- `env key "<k>" has the same name as <list>[i] "<n>" (names are compared ignoring case)`

Section-level: `default: more than one profile sets default = true ("a", "b"); at most one may`.

Environment for the engine: the hub's environment minus `XDG_CONFIG_HOME` and `XDG_DATA_HOME`. The hub uses those two itself for its own config and data directories, and they would redirect podman's machine registry. No env var configures this feature directly; `${NAME}` expansion is the mechanism (e.g. `gateway_token = "${AGENTKIT_GATEWAY_TOKEN}"`).

Container note: `hub2/containers/sandbox/Containerfile` is a RHEL 10 s2i-core development toolchain image (user 1001, `WORKDIR /opt/app-root/workspace`). The hub does not build images. A profile image must contain AgentKit's sandbox agent. The hub's own container exposes 80/8080 only, not the gateway port.

### 10.10 Edge cases and error handling

- A `null` body → 400 `request body must be a JSON object`. `{"profile":null}` = default profile. `{"PROFILE":"x"}` → unknown field `"PROFILE"`.
- 17+ labels: the error names the first key past the 16th in sorted order, unless an earlier key is invalid.
- `ttl` exactly equal to the profile TTL (`1h` or `3600s`) is accepted. `59s` is refused. `"1m0.9s"` is accepted and stored as 60 s.
- A leased lease whose `lease_expires_at` has passed still authenticates (until `expires_at`) and stops counting against capacity before any sweep. Its Create expires it.
- An active lease past `expires_at` is refused by the authenticator immediately. It stops counting toward capacity, but stays `active` in the table until the sweep.
- Restart with `leased` leases: they remain usable, because the token hash persists.
- A profile removed from config after issue: Create fails the lease (`profile "<p>" is no longer configured`). Existing leases keep their snapshot otherwise; a changed image/env does not alter them.
- A secret deleted between issue and Create → the lease goes `failed` with `secret_missing: ...`. A secret rotated → the new value is injected.
- A DELETE racing a Create: whichever conditional update wins decides. A box made for an ended lease is destroyed immediately, with no create event, and the Create fails.
- A DELETE racing a client close or a sweep on the same lease: in-process "ending" counters keep the client-close path from recording a `client` end while a DELETE/sweep/shutdown is destroying the box.
- A provider close failure: gatewayd forgets the box, the hub keeps its handle, and a later DELETE or sweep retries. For a remote provider, the retry destroys by id via the remote client (idempotent).
- A remote gateway that becomes unreachable: leases are still issued and profiles listed. Create fails the lease. DELETE of an active lease → 502. A client close fails and the lease stays active. Sweeps log failures. There is no create retry.
- A remote that refuses the hub token after start: the caller sees Unavailable (never Unauthenticated), and the hub logs `sandbox gateway: the remote gateway refused hub's token; check sandbox.gateway_token`.
- A failed dial to a remote is reported as `cannot connect to the remote gateway: <reason>`, with no address (`connection refused`, `no such host`, `i/o timeout`, `canceled`, `timed out`, `failed`).
- Feature disabled: all six REST routes exist and answer 404 `sandboxes_disabled` to any authenticated caller, before scope checks. 401 still comes first. Bound-token guard refusals still precede the handler. No gateway is bound.
- An unknown credential type → 403 with the scope message.
- `?all=` (empty) → 400. Uppercase-hex lease ids → 404.
- At shutdown, a client close in progress records reason `shutdown`, and new Creates get `hub is shutting down and creates no more sandboxes`.

### 10.11 Discrepancies between docs and code

1. **`?all=true` retention wording.** api.md (GET list: "adds those that ended in the last 24 hours"; GET one: 404 for a lease that "ended more than 24 hours ago") and cli.md (list/get/destroy) say ended leases disappear at exactly 24 h. Code: rows are deleted only by the sweep's prune step (`hub2/internal/sandboxes/sweeper.go:263-276`), and `ListLeases(all=true)` returns every terminal row still present (`store_query.go:18-29`). They therefore remain visible for up to 24 h + sweep spacing. configuration.md (`reap_interval` row) states this correctly.
2. **gRPC message text of a failed Create.** api.md "The sandbox gateway" lists bare messages such as `lease <id> has expired`. Code: the hub backend returns those texts (`backend.go:200-260`), but gatewayd wraps every non-refusal backend error as `Unavailable` with the prefix `gatewayd: backend create: ` (`agentkit-go/sandbox/gatewayd/gateway.go:88-94`). The code is unchanged; the client-visible message is prefixed.
3. **413 not documented for POST.** Code answers 413 `request body is too large` when the body exceeds the server limit (`lease_validate.go:70-76`). The api.md POST error table and openapi.yaml list no 413.
4. **CLI error_type.** cli.md says `parley sandbox destroy` reports `502 with error_type: sandbox_provider_error`. Code: the hub sends `error_type`, but the parley client decodes only `code` and `message` (`apikit/internal/cli/user.go:186-196`), so parley's printed envelope has no `error_type`.
5. **ADR 03 says the hub does not stop sandboxes when it stops.** ADR 03 "Consequences" says "This change never calls `Shutdown`" and that the gateway reaps "at start and every ten minutes". Code: `Gateway.Shutdown` runs after the HTTP server stops and destroys every active lease's box with reason `shutdown` (`gateway.go:274-293`, `sweeper.go Shutdown`, `cmd/parleyd/main.go:499-507`). The reap period is `reap_interval`. The ADR describes an earlier stage; the code wins.
6. **ADR 03 expiry ordering.** ADR 03 "Expiry" says "The expiry path ends the lease first and retries the box on later sweeps". Code: `endExpired` for an active lease calls `destroyBox` **first** (failure ignored, except a cancelled context, which aborts), then ends the lease (`expire.go:81-92`). The net effect (lease ended regardless, box retried by the orphan step) matches.
7. **Brief vs code on env scrubbing.** Only `XDG_CONFIG_HOME` and `XDG_DATA_HOME` are removed from the engine's environment (`provider.go:49-59`), not every `XDG_*` variable. The docs do not mention this.
8. **Lease `secrets` order.** api.md shows lease `secrets`/`optional_secrets` without stating an order. Code: leases keep the profile's order (`lease_service.go:175-176`), while the profile listing sorts them (`handlers.go:97-98`). This is an observable difference, not a doc error.
9. **DELETE after a restart.** api.md DELETE says that after a restart "a `DELETE` ends that lease without destroying the sandbox". In code, such leases are already ended with reason `restart` by recovery before any route or gateway serves (`gateway.go:189-193`, `cmd/parleyd/main.go:380-381, 523`). The DELETE path only applies if recovery failed for that lease, in which case `server.Destroy` returns not-held, the hub has no handle, and the lease is ended with reason `api` (`handle.go:160-180`). The docs describe a case that is normally unreachable.
10. **The examples README's failure text.** `examples/sandbox-lease/README.md` "When it fails" shows `only a leased lease can create its sandbox`. The hub's backend message is `... only a leased lease can create its box` (`backend.go:248`). The README line is the example program's own pre-check wording.


---

## 11. Audit, sessions, events and metrics

### 11.1 Purpose and scope

The hub keeps a second, embedded analytical database (DuckDB, "the audit store") next to its SQLite database. It records (a) the hub's own audit events ("hub events") for every significant mutation or relayed forge delivery, (b) telemetry that agents push about their runs (agent audit events, session outcomes, tool calls, tool errors, conversation traces, postmortems), and (c) agent sessions with their incremental token usage, aggregated into a per-workspace token "cost" report. Callers read this data through per-run listings, a unified hub+agent audit query, a transcript reconstruction, and a live Server-Sent-Events stream of hub events. A background retention task prunes the store hourly, and a Prometheus endpoint exposes operational metrics.
Out of scope: monetary pricing (the "cost" endpoint reports token counts only, there is no price table and no model-name catalogue), CLI commands (none exist for this area), and replay/history on the SSE stream.

### 11.2 User stories

**US-AUD-1 — Agent reports a session.** As an Agent, I want to open a session for a workspace, report token usage increments and close it with an outcome, so that the operator can attribute token spend.
- `POST /api/v1/sessions` with `workspace_slug` creates an `active` session owned by my user (201); repeating the same `id` returns the stored session (200) without double-counting.
- `POST /sessions/:id/usage` adds one usage row per call while the session is `active` (201). A replay of an existing usage id answers 200 and adds nothing **[FIX-15]**. A terminal session answers 409.
- `POST /sessions/:id/complete` with `completed|failed|timeout` ends it once; repeating answers 200 with the unchanged terminal record.

**US-AUD-2 — Agent pushes run telemetry.** As an Agent (PAT with `audit:write`, owner of the workspace), I want to push audit events, outcomes, tool calls/errors, traces and a postmortem under `/workspaces/:slug/runs/:run_id/...`, so that the run can be audited later.
- Each record is idempotent on its `id` (201 first time, 200 on replay, nothing stored twice).
- Batches of 1..1000 events/traces are accepted item by item with a per-item error list.
- Writes into an archived workspace are refused with 409 `workspace_archived`.

**US-AUD-3 — Developer inspects a run.** As a Developer (owner or org member of the workspace), I want to list a run's events/outcomes/tool calls/tool errors/traces with filters and cursors, read its postmortem and a transcript per node, so that I can debug what an agent did.
- Members may read, whatever the workspace status (also archived).
- An unknown or unreadable workspace is 404 (no slug enumeration).

**US-AUD-4 — Operator audits the hub.** As an Operator (admin token), I want one query over hub and agent events across all workspaces, newest first, with filters and cursor paging, so that I can reconstruct who did what.
- Hub events without a workspace (e.g. user-level) are visible to admin tokens only.

**US-AUD-5 — Live feed.** As a Developer, I want to subscribe to `GET /api/v1/events` and receive hub events of workspaces I can read as SSE frames, with heartbeats, so that dashboards update live.

**US-AUD-6 — Cost report.** As an Org member, I want `GET /workspaces/:slug/cost?group_by=day|session|model` to sum token usage over a window, so that I can see spend per day/session/model.

**US-AUD-7 — Retention.** As an Operator, I want old audit data deleted automatically according to env-configured ages, stale active sessions timed out, and data of deleted workspaces removed after a grace period, so that the store stays bounded.

**US-AUD-8 — Monitoring.** As an Operator, I want `GET /metrics` in Prometheus text format with request, session, token, audit, SSE, retention and sandbox metrics, so that I can alert on the hub.

### 11.3 Concepts and data model

#### 11.3.1 The audit store

- Engine: DuckDB, a single file. Path = env `AF_AUDIT_DB_PATH`; when unset or empty: `<directory of [database].path>/audit.duckdb` (the SQLite file's directory). The parent directory is created (mode 0755) at start. Failure to open/ping or to create the schema aborts hub start-up (fatal).
- Schema creation runs at every start in one transaction: `CREATE TABLE IF NOT EXISTS` for the nine tables, then additive migrations: `hub_audit_events.severity VARCHAR DEFAULT 'info'`, `hub_audit_events.timestamp VARCHAR DEFAULT ''`, `agent_audit_events.archetype VARCHAR DEFAULT ''` (all `ADD COLUMN IF NOT EXISTS`). Any failure rolls back everything.
- The SQLite database is consulted (read-only) for workspace existence, owner, org, status, org membership and token bindings. The two databases share no transaction.
- All primary keys are global (not per workspace/run). Inserts use "insert or ignore" semantics: an existing key means "duplicate", nothing is changed.
- Timestamps in `TIMESTAMPTZ` columns are returned as RFC 3339 UTC with fractional seconds only when non-zero (Go `RFC3339Nano`, e.g. `2026-09-01T08:00:00.5Z`; DuckDB keeps microsecond precision).
- JSON-valued text columns are returned decoded (any JSON value); a stored text that is not valid JSON is returned as a JSON string.

#### 11.3.2 Tables (logical entities)

`hub_audit_events` — the hub's own events (see 11.8).

| Field | Type | Meaning / default |
|---|---|---|
| `id` | string PK | UUIDv4 generated by the hub |
| `event_type` | string, required | e.g. `hub.workspace.create` |
| `actor_id` | string, default `""` | user id of the caller; `""` for system/forge/admin-token actors |
| `actor_type` | string, default `""` | `admin_token`, `api_key`, `pat`, `system`, `lease`, `forge` |
| `resource_type`, `resource_id`, `action` | string, default `""` | |
| `workspace` | string, default `""` | workspace slug; `""` = not workspace-scoped |
| `severity` | string, default `info` | The hub never sets it: every hub event is `info` |
| `timestamp` | string (RFC 3339 Nano UTC) | when emitted; legacy rows may hold `""` |
| `metadata` | JSON text, default `{}` | event-specific map; `{}` when none or not serialisable |
| `ingested_at` | timestamptz | same instant as `timestamp` |

`agent_audit_events` — agent-reported events.

| Field | Type | Meaning / default |
|---|---|---|
| `id` | string PK | client UUID or generated UUIDv4 |
| `run_id` | string | from the URL path |
| `workspace` | string | from the URL path |
| `event_type` | string, required | free-form; known: `session.start`, `session.end`, `session.fail`, `run.limit_reached`, `git.conflict`, `harvest.empty`, `review.parse_failure` (unknown types are accepted, only logged as a warning) |
| `severity` | enum `info`,`warning`,`error`,`critical` | default by type (11.4) |
| `node_id`, `session_id` | string, default `""` | |
| `archetype` | string, default `""` | never written by any endpoint; always `""` |
| `timestamp` | timestamptz | client value normalised to UTC, else server now |
| `payload` | JSON text, default `{}` | |
| `ingested_at` | timestamptz | server now (seconds precision) |

`session_outcomes`: `id` PK, `run_id`, `workspace`, `session_id` (required), `node_id`, `status` (required, free-form string), `timestamp`, `duration_ms` (32-bit integer, default 0), `token_usage` (JSON text, default `{}`), `ingested_at`.

`tool_calls`: `id` PK, `run_id`, `workspace`, `tool_name` (required), `node_id`, `session_id`, `timestamp`, `duration_ms` (32-bit integer, default 0), `input` (JSON, default `{}`), `output` (JSON, default `{}`), `ingested_at`.

`tool_errors`: `id` PK, `run_id`, `workspace`, `tool_name` (required), `node_id`, `session_id`, `error_code` (default `""`), `error_msg` (required), `timestamp`, `ingested_at`.

`agent_traces`: `id` PK, `run_id`, `workspace`, `event_type` (closed enum: `session.init`, `assistant.message`, `tool.use`, `tool.error`, `session.result`), `node_id`, `session_id`, `sequence` (32-bit integer, default 0), `timestamp`, `data` (JSON, default `{}`), `ingested_at`.

`postmortems`: `run_id` **PK (global)**, `workspace`, `schema_version` (int, default 1; only 1 accepted), `run_status` (enum `stalled`, `block_limit`, `cost_limit`, `session_limit`), `started_at`, `completed_at` (timestamptz), `task_summary` (JSON, must contain key `total` when an object), `cost_summary` (JSON), `blocked_tasks` (JSON, default `[]`), `session_history` (JSON, default `[]`), `ingested_at`.

`agent_sessions`:

| Field | Type | Meaning / default |
|---|---|---|
| `id` | string PK | client-chosen (any non-empty string, not validated) or generated UUIDv4 |
| `run_id`, `node_id`, `archetype`, `model` | string, default `""` | free-form labels |
| `workspace_slug` | string, required | must name an existing workspace the caller may write |
| `status` | enum | `active`, `completed`, `failed`, `timeout`, `terminated` |
| `started_at` | timestamptz | server now, seconds precision |
| `credential_id` | string | the caller's **user id** (`""` for admin tokens), not a token id |
| `credential_type` | string | `admin_token`, `api_key` or `pat` |
| `error_message` | text, nullable | from complete; reason on force-close |
| `ended_at` | timestamptz, nullable | set on any terminal transition |
| `duration_ms` | int64, nullable | only from complete |
| `cache_creation_input_tokens` | int64, default 0 | only from complete |
| `metadata` | JSON, nullable | any JSON value the client sent |
| `ingested_at` | timestamptz | |

`token_usage`: `id` PK (client-chosen any string or UUIDv4), `session_id`, `workspace_slug` (copied from the session), `model` (required, non-blank), `input_tokens`, `output_tokens`, `cache_read_tokens` (int64 >= 0, default 0), `reported_at` (server now, nanosecond string at insert, stored at microseconds), `ingested_at`.

#### 11.3.3 Session state machine

```
            POST /sessions            POST /complete {completed|failed|timeout}
  (none) ─────────────────▶ active ──────────────────────────────────────────▶ completed | failed | timeout
                              │  workspace archive / delete (force-close)   ─▶ terminated  (error_message = reason)
                              └─ retention pre-step (started_at older than AF_SESSION_MAX_ACTIVE_AGE_DAYS) ─▶ timeout
```
- Terminal states (`completed`, `failed`, `timeout`, `terminated`) are absorbing. `terminated` is never client-settable.
- Usage may be reported only while `active`.

#### 11.3.4 JSON shapes

Session (as returned by every session endpoint):
```json
{
  "id": "…", "workspace_slug": "w",
  "run_id": "…", "node_id": "…", "archetype": "…", "model": "…",        // omitted when ""
  "status": "active",
  "credential_id": "user-id", "credential_type": "pat",                  // always present ("" for admin)
  "error_message": "…",                                                   // omitted when null/""
  "started_at": "2026-09-01T10:00:00Z",
  "ended_at": "…",                                                        // omitted when null
  "duration_ms": 12,                                                      // omitted when null
  "cache_creation_input_tokens": 100,                                     // omitted when 0
  "metadata": {…},                                                        // omitted when null
  "token_summary": {"total_input_tokens":0,"total_output_tokens":0,"total_cache_read_tokens":0,"models_used":[]}
                                                                          // only on GET /sessions and GET /sessions/:id
}
```
`models_used`: distinct non-empty model names of the session's usage rows, sorted ascending, `[]` when none.

Token usage record: `{"id","session_id","workspace_slug","model","input_tokens","output_tokens","cache_read_tokens","reported_at"}` (all always present).

Hub event (SSE `audit_event` frame payload and emitter input): `{"id","event_type","actor_id","actor_type","resource_type","resource_id","action","workspace","metadata"?,"timestamp"}` — `metadata` omitted when the event has none; there is no `severity` field.

Cursor: base64url without padding (RFC 4648 §5) of the JSON `{"ts":"<RFC3339 timestamp>","id":"<row id>"}` of the last row of the page. Valid only if it decodes and both fields are non-empty (and, for the audit routes, `ts` parses as RFC 3339).

#### 11.3.5 Permission scopes

PAT scopes registered by this area: `audit:read`, `audit:write`, `sessions:read`, `sessions:write`. `*:write` implies `*:read`. Admin tokens and API keys are never scope-checked (they pass every scope test). Only PATs are scope-checked.

### 11.4 Functional requirements

**Store and emitter**
- AUD-FR-001: The audit store path is `AF_AUDIT_DB_PATH` or `<dir of SQLite path>/audit.duckdb`; the parent directory is created; open/ping/schema errors stop start-up.
- AUD-FR-002: Schema init is idempotent, transactional, and applies the three additive column migrations listed in 11.3.1.
- AUD-FR-003: Emitting a hub event assigns a new UUIDv4 `id` and `timestamp` = `ingested_at` = current UTC time in RFC 3339 with nanoseconds (trailing zeros trimmed); `metadata` is serialised as JSON (`{}` when absent or unserialisable). Severity is not set (stored `info`).
- AUD-FR-004: A failed hub-event insert is logged and swallowed: the operation that emitted it is never affected; the event is then neither counted nor broadcast. On success the event is counted on `afhub_audit_events_total{source="hub",event_type=<type>}` and published to the SSE manager (non-blocking).
- AUD-FR-005: Emission is synchronous within the triggering request/job (an extra DuckDB insert); the emitter itself never returns an error.

**Sessions**
- AUD-FR-010: `POST /sessions` requires `sessions:write`; `workspace_slug` must be non-blank (after trimming spaces); the caller needs write access to the workspace (owner or admin token; an org member gets 403 `forbidden`; unknown/unreadable/bound-elsewhere gets 404 `workspace not found`). Workspace status is not checked (archived workspaces accept new sessions).
- AUD-FR-011: Session id = request `id` if non-empty, else a UUIDv4. `status`=`active`, `started_at`=now (seconds), `credential_id`=caller user id, `credential_type`=caller credential type.
- AUD-FR-012: Session creation is idempotent on `id`: if the id exists, nothing is changed and the stored session is returned with 200. If the stored session's workspace differs from the requested one, it is returned only when the caller may read the stored workspace (owner/member/admin), otherwise the read-access refusal (404 `workspace not found`) is returned. The active-sessions gauge is incremented only for a newly created session.
- AUD-FR-013: `POST /sessions/:id/complete` accepts `status` ∈ {`completed`,`failed`,`timeout`}; `terminated` → 400 with its own message; anything else (incl. empty) → 400 `invalid status`. Status is validated before the session is looked up.
- AUD-FR-014: Only the session's creator (caller user id == `credential_id`) or an admin token may complete or report usage; otherwise 403 `not session owner`. A PAT bound to a workspace other than the session's workspace also gets 403 `not session owner`.
- AUD-FR-015: Completing a terminal session returns 200 with the unchanged stored session (no update, no gauge change). Completing an active session atomically (conditional update on `status='active'`) sets `status`, `ended_at`=now (seconds), `error_message` (NULL when empty), `duration_ms` (NULL when absent), `cache_creation_input_tokens` (0 when absent), and decrements the active gauge once. A session whose status is neither active nor terminal answers 409 `session is not active`.
- AUD-FR-016: `POST /sessions/:id/usage`: `model` must be non-blank (400 `model is required`); all token counts must be >= 0 (422 `token counts must be non-negative`); these body checks precede the session lookup. The session must exist (404), be owned (AUD-FR-014), and be `active` (409 `session is not active`).
- AUD-FR-017: Usage id = request `id` or UUIDv4. Insert is idempotent on `id` (global). A new record answers **201**, and `afhub_agent_tokens_total{workspace,model,direction}` is incremented by each positive count of the request (`input`, `output`, `cache_read`). **[FIX-15]** A duplicate replay answers **200** with the stored record. It changes nothing: no counter, no metric, no session total. This matches the general rule that an idempotent replay answers 200. In the baseline, a replay answered 201 and incremented the token counters again.
- AUD-FR-018: Session reads need `sessions:read` (or `sessions:write`) and, for non-admins, read access to a **non-archived** workspace (owner or org member; bound PAT only its own). Any failed lookup denies access.
- AUD-FR-019: `GET /sessions`: default order `desc`, any value other than `asc` (case-insensitive) is `desc`; default limit 50, values <= 0 or unparseable → 50, max 500. Ordering by (`started_at`, `id`) in the chosen direction; cursor = (started_at, id) of the last row; `has_more` computed by fetching limit+1.
- AUD-FR-020: `GET /sessions/:id/usage`: default order `desc`, default limit 100 (<= 0 or unparseable → 100), max 1000, ordering by (`reported_at`, `id`); `totals` aggregate all rows of the session regardless of paging.
- AUD-FR-021: Cost: `group_by` default `day`, enum `day|session|model`; `since` default now−24h, `until` default now (both formatted RFC 3339 seconds); both must parse as RFC 3339; `since` must be strictly before `until`. Rows match `workspace_slug = slug AND reported_at >= since AND reported_at < until`.
- AUD-FR-022: Cost totals: sums of the three token counts and `COUNT(DISTINCT session_id)`; breakdown: one entry per group key, ordered ascending by the key, each with the same sums and distinct-session count. `day` keys are UTC calendar dates `YYYY-MM-DD`. No prices are applied.
- AUD-FR-023: Archiving (`POST /workspaces/:slug/archive`) or deleting (`DELETE /workspaces/:slug`) a workspace force-closes all its `active` sessions: `status`=`terminated`, `ended_at`=the operation time (RFC 3339 seconds), `error_message`=`"workspace archived"` / `"workspace deleted"`; the gauge is decremented once per closed session and one `hub.session.force_closed` event is emitted per session. On delete this runs before the workspace row is removed. A force-close failure is logged and does not fail archive/delete.

**Ingestion**
- AUD-FR-030: Every ingestion POST checks, in order: authenticated (401) → `audit:write` (403 `insufficient scope: audit:write required`) → workspace write rule (AUD-FR-031) → `run_id` path format → body bind (400 `invalid request body`) → field validation → insert.
- AUD-FR-031: Ingestion workspace rule: admin tokens bypass all checks (even a non-existent slug is accepted). Otherwise: workspace must exist (404 `workspace not found`; lookup error → 500 `failed to check workspace access`); a PAT bound to a different workspace → 404 `workspace not found`; archived → 409 `workspace is archived` / `workspace_archived` (checked before ownership); caller user id must equal the workspace owner id, else 403 `token owner does not have write access to this workspace` / `workspace_access_denied` (org members and strangers alike).
- AUD-FR-032: `run_id` must match `^\d{8}_\d{6}_[0-9a-f]{6}$` (lowercase hex). Otherwise 400 `invalid run_id format: "<value>" (expected YYYYMMDD_HHMMSS_6hexchars)`.
- AUD-FR-033: Client `id`, when present, must match `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$` (lowercase only) → else 400 `invalid UUID format: "<id>"`. Absent → generated UUIDv4.
- AUD-FR-034: Client timestamps must be RFC 3339 (fractional seconds optional, any offset) → else 400 `invalid timestamp: "<v>" (must be RFC 3339)`; accepted values are normalised to UTC (`RFC3339Nano`, trailing zeros trimmed). Absent → server now (RFC 3339 seconds).
- AUD-FR-035: Default severity of agent events (when `severity` absent/empty): `session.fail`→`error`; `run.limit_reached`, `git.conflict`, `harvest.empty`, `review.parse_failure`→`warning`; all other types→`info`. Explicit severity must be one of `info|warning|error|critical` → else 400 `invalid severity: "<v>" (must be one of: info, warning, error, critical)`.
- AUD-FR-036: Idempotency: each record is inserted with "insert or ignore" on its primary key (`id`, or `run_id` for postmortems; keys are global across workspaces and runs). New → 201; existing key → 200 and nothing changes. The response body on 200 is computed from the **request** (not the stored row).
- AUD-FR-037: Batch routes accept a JSON array of 1..1000 items: not an array → 400 `invalid request body`; empty (or empty body) → 400 `batch array must not be empty`; > 1000 → 413 `batch exceeds maximum of 1000 items`. Each item is validated independently; invalid items are reported in `errors` (with `index`, the item's `id` if any, `message`) and skipped; valid items are inserted in one transaction (timeout = write timeout × (valid items + 1)); response always 200 `{accepted, duplicates, errors}`. If no item is valid, nothing is written and `accepted`=`duplicates`=0.
- AUD-FR-038: Writes of single-item ingestion use a per-statement timeout of `AF_AUDIT_WRITE_TIMEOUT_MS` (default 2000 ms; independent of the client connection). A timeout (single or batch) answers 503 `write timeout: retry after 5 seconds` with header `Retry-After: 5`. Other store errors → 500 `internal server error`. In a batch, any insert error rolls back the whole batch.
- AUD-FR-039: Counting: a new single agent event increments `afhub_audit_events_total{source="agent",event_type=<type>}`; a batch increments `{source="agent",event_type="batch"}` by the number accepted. Other ingestion kinds are not counted.

**Reads**
- AUD-FR-050: Per-run reads (GET events/outcomes/calls/errors/traces/postmortem and transcript) check: 401 → `audit:read` or `audit:write` (403 `insufficient scope: audit:read required`) → workspace read access (owner, org member, admin; any workspace status; unknown/unreadable/bound-elsewhere → 404 `workspace not found`; lookup failure → 500) → `run_id` format (not for transcript) → query params → query.
- AUD-FR-051: Per-run list paging: default order **ascending** (only `desc`, case-insensitive, gives descending); default limit 100 (<= 0 or unparseable → 100), max 1000; ordering by (`timestamp`, `id`); `since` inclusive (`>=`), `until` **inclusive** (`<=`); `since`/`until` must be RFC 3339 (400 `invalid since timestamp` / `invalid until timestamp`); invalid cursor → 400 `invalid cursor`. Rows are always confined to the path's workspace and run.
- AUD-FR-052: Unified query merges hub and agent events, ordered by event instant descending then `id` descending (fixed, no `order` parameter); limit default 100, values <= 0 or unparseable → 100, max 1000; `since` inclusive, `until` exclusive; `since` must be before `until` when both given.
- AUD-FR-053: Workspace confinement for `GET /audit` and `GET /events`: admin tokens are unrestricted; other callers see only events whose `workspace` is in their readable set (owned + org-member workspaces of any status; a bound PAT at most its bound workspace; a credential without user id: empty set). Events with `workspace=""` are visible to admin tokens only. A `workspace` parameter outside the set → 404 `workspace not found` (and, when the caller's binding is the cause, a `hub.token.bound_refused` report).
- AUD-FR-054: SSE stream sends only hub events (agent ingestion is never broadcast). The readable set is computed once at connect and kept for the life of the connection.

**Retention**
- AUD-FR-070: Retention runs once at start and then every hour (shared maintenance loop), executing the steps of 11.7 in order; a failing step is logged and counted, the remaining steps still run; the last-run gauge is set only if all steps succeeded; cancellation stops before the next step.

### 11.5 REST API

All routes below are under the API mount point `/api/v1` (except `/metrics`) and pass apikit's API middleware (authentication: missing/invalid credentials are refused by the auth middleware with 401 before the handler; `POST/PUT/PATCH` with a non-empty body must have `Content-Type: application/json…` else 415 `unsupported media type`; body size limit → 413 `payload too large`; `Cache-Control: no-store`). Errors use the envelope `{"error":{"code":<int>,"message":"<text>","error_type":"<type>"?}}`; `error_type` is present only where listed. "Bound token" = PAT bound to a workspace: on any route whose template contains `/workspaces/:slug` the binding guard answers 404 `workspace not found` before the handler when `:slug` is not the bound workspace; on other routes it answers 403 `token_bound` unless the route is on the bound-token allow-list. Allow-listed here: the six `/sessions…` routes, `GET /audit`, `GET /events`.

#### `POST /api/v1/sessions`
- **Purpose:** open an agent session.
- **Auth / scope:** any credential; PAT needs `sessions:write`. Workspace write access (owner or admin token). Bound token allowed; confined by the workspace check.
- **Request body:** `id` string optional (any string); `workspace_slug` string **required** (non-blank after trim); `run_id`, `node_id`, `archetype`, `model` strings optional; `metadata` any JSON optional.
- **Behaviour:** see AUD-FR-010..012.
- **Response:** 201 new session (shape 11.3.4, no `token_summary`); 200 existing session on id replay.
- **Errors (in order):** 401 `authentication required`; 403 `sessions:write scope required`; 400 `invalid request body`; 400 `workspace_slug is required`; 404 `workspace not found` (unknown, no access, bound elsewhere); 403 `org members have read-only access to this workspace` (`error_type: forbidden`); 500 `internal server error` (lookup/store failure); on replay of a foreign-workspace id: 404 `workspace not found` (or 500) when the caller may not read it.
- **Side effects:** `agent_sessions` row; `afhub_agent_sessions_active{workspace}` +1 (new only). No hub event.

#### `POST /api/v1/sessions/:id/complete`
- **Purpose:** end an active session.
- **Auth / scope:** PAT needs `sessions:write`; caller must be the session's creator (same user id) or admin token. Bound token allowed (must be bound to the session's workspace).
- **Request body:** `status` string **required** ∈ `completed|failed|timeout`; `error_message` string optional; `duration_ms` int64 optional (no range check); `cache_creation_input_tokens` int64 optional.
- **Behaviour:** AUD-FR-013..015.
- **Response:** 200 session (updated, or unchanged if already terminal).
- **Errors (in order):** 401; 403 `sessions:write scope required`; 400 `invalid request body`; 400 `terminated is not a valid client-settable status`; 400 `invalid status`; 404 `session not found`; 500; 403 `not session owner` (not creator, or bound to another workspace); 500 (binding lookup); 409 `session is not active` (status neither active nor terminal).
- **Side effects:** row update; gauge −1 on an actual active→terminal transition.

#### `POST /api/v1/sessions/:id/usage`
- **Purpose:** report one token-usage increment.
- **Auth / scope:** PAT `sessions:write`; creator or admin; bound token as above.
- **Request body:** `id` string optional; `model` string **required** non-blank; `input_tokens`, `output_tokens`, `cache_read_tokens` int64 optional, default 0, >= 0.
- **Response:** 201 token-usage record. **[FIX-15]** A duplicate id answers **200** with the stored record and changes nothing; the baseline answered 201.
- **Errors (in order):** 401; 403 `sessions:write scope required`; 400 `invalid request body`; 400 `model is required`; 422 `token counts must be non-negative`; 404 `session not found`; 500; 403 `not session owner`; 409 `session is not active`; 500.
- **Side effects:** `token_usage` row (`workspace_slug` copied from session, `reported_at` = now); token counter increments (AUD-FR-017). A duplicate replay has no side effects **[FIX-15]**.

#### `GET /api/v1/sessions`
- **Purpose:** list sessions with token summaries.
- **Auth / scope:** PAT `sessions:read` or `sessions:write`. Bound token allowed (sees only its workspace).
- **Query params:** `workspace_slug`, `run_id`, `status`, `credential_type` (exact-match filters, not validated); `since` (sessions with `started_at >= since`; passed to the database unvalidated — a value it cannot parse yields 500); `order` (`asc`|other→`desc`); `limit` (default 50, max 500); `cursor`.
- **Behaviour:** cursor syntax validated first (400). Non-admin with `workspace_slug`: must have read access to that non-archived workspace, else 403. Non-admin without it: results restricted to readable non-archived workspaces (none → empty page). Admin: unrestricted.
- **Response:** 200 `{"sessions":[Session+token_summary…],"next_cursor":string|null,"has_more":bool}`; `token_summary` always present (zeros and `[]` when no usage).
- **Errors:** 401; 403 `sessions:read scope required`; 400 `invalid cursor`; 403 `workspace access denied`; 500.

#### `GET /api/v1/sessions/:id`
- **Auth / scope:** `sessions:read`; non-admin needs read access to the session's workspace and that workspace must not be archived (or must still exist).
- **Response:** 200 Session with `token_summary`.
- **Errors (in order):** 401; 403 `sessions:read scope required`; 404 `session not found`; 500; 403 `workspace access denied` (no access, archived, deleted, bound elsewhere, lookup failure).

#### `GET /api/v1/sessions/:id/usage`
- **Auth / scope:** as `GET /sessions/:id`.
- **Query params:** `order` (`asc`|other→`desc`), `limit` (default 100, max 1000), `cursor`.
- **Response:** 200 `{"session_id","records":[TokenUsage…],"totals":{"total_input_tokens","total_output_tokens","total_cache_read_tokens","models_used"},"next_cursor","has_more"}`.
- **Errors (in order):** 401; 403 scope; 404 `session not found`; 500; 403 `workspace access denied`; 400 `invalid cursor`; 500.

#### `GET /api/v1/workspaces/:slug/cost`
- **Auth / scope:** `sessions:read` (or `sessions:write`); non-admin needs read access to a non-archived existing workspace (bound tokens for another slug are stopped earlier by the binding guard with 404). Admin may query any slug, even unknown (zeros).
- **Query params:** `group_by` (`day` default, `session`, `model`); `since` (default now−24h), `until` (default now), RFC 3339.
- **Response:** 200
```json
{"workspace":"w","period":{"since":"<as given or default>","until":"<as given or default>"},
 "totals":{"input_tokens":0,"output_tokens":0,"cache_read_tokens":0,"sessions":0},
 "breakdown":[{"date":"2026-09-01","input_tokens":10,"output_tokens":0,"cache_read_tokens":0,"sessions":1}]}
```
  Each breakdown entry carries exactly one of `date` / `session_id` / `model` (the others omitted); `breakdown` is `[]` when empty. `period` echoes the raw parameter strings.
- **Errors (in order):** 401; 403 `sessions:read scope required`; 403 `workspace access denied` (no access, unknown, archived); 400 `invalid group_by value`; 400 `invalid since timestamp`; 400 `invalid until timestamp`; 400 `since must be before until`; 500.

#### `POST /api/v1/workspaces/:slug/runs/:run_id/events`
- **Purpose:** ingest one agent audit event.
- **Auth / scope:** `audit:write`; ingestion workspace rule (AUD-FR-031).
- **Request body:** `id` (UUID, optional), `run_id` (optional; must equal path), `event_type` **required**, `severity` (enum, optional), `node_id`, `session_id`, `timestamp` (RFC 3339), `payload` (JSON object or null).
- **Behaviour/validation order:** event_type missing → run_id mismatch → id format → timestamp → severity → payload must be an object.
- **Response:** 201/200 `{"id","run_id","event_type","severity","created_at"}` (`created_at` = normalised client timestamp or server now; severity = effective severity).
- **Errors:** 401; 403 `insufficient scope: audit:write required`; 404 `workspace not found`; 500 `failed to check workspace access`/`internal server error`; 409 `workspace is archived` (`workspace_archived`); 403 `token owner does not have write access to this workspace` (`workspace_access_denied`); 400 invalid run_id; 400 `invalid request body`; 400 `missing required field: event_type`; 400 `run_id mismatch between URL path and request body`; 400 `invalid UUID format: "…"`; 400 `invalid timestamp: "…" (must be RFC 3339)`; 400 `invalid severity: …`; 400 `invalid payload: must be a JSON object`; 503 write timeout; 500.
- **Side effects:** `agent_audit_events` row; agent counter.

#### `POST /api/v1/workspaces/:slug/runs/:run_id/events/batch`
- **Request body:** JSON array of event objects (as above). Per-item checks in order: `event_type` non-empty (`missing required field: event_type`) → id UUID → severity → timestamp. **Not** checked per item: `run_id` mismatch, payload type (any JSON stored).
- **Response:** 200 `{"accepted":int,"duplicates":int,"errors":[{"index":int,"id":"…"(omitted if empty),"message":"…"}]}` (`errors` is `[]` when none).
- **Errors:** auth/workspace/run_id as above; 400 `invalid request body`; 400 `batch array must not be empty`; 413 `batch exceeds maximum of 1000 items`; 503; 500.

#### `POST /api/v1/workspaces/:slug/runs/:run_id/sessions/outcomes`
- **Request body:** `id` (UUID), `run_id` (ignored, not compared), `session_id` **required**, `node_id`, `status` **required** (free-form), `timestamp`, `duration_ms` int (no range check), `token_usage` any JSON.
- **Validation order:** session_id → status → timestamp → id.
- **Response:** 201/200 `{"id","run_id","node_id","status","created_at"}`.
- **Errors:** as events + `missing required field: session_id` / `missing required field: status`.

#### `POST /api/v1/workspaces/:slug/runs/:run_id/tools/calls`
- **Request body:** `id`, `run_id` (ignored), `tool_name` **required**, `node_id`, `session_id`, `timestamp`, `duration_ms`, `input` (any JSON), `output` (any JSON).
- **Validation order:** tool_name → timestamp → id.
- **Response:** 201/200 `{"id","run_id","tool_name","called_at"}`.

#### `POST /api/v1/workspaces/:slug/runs/:run_id/tools/errors`
- **Request body:** `id`, `run_id` (ignored), `tool_name` **required**, `node_id`, `session_id`, `error_code`, `error_msg` **required**, `timestamp`.
- **Validation order:** tool_name → error_msg → timestamp → id.
- **Response:** 201/200 `{"id","run_id","tool_name","failed_at"}`.

#### `POST /api/v1/workspaces/:slug/runs/:run_id/traces`
- **Request body:** `id`, `run_id` (ignored), `event_type` **required** ∈ {`session.init`,`assistant.message`,`tool.use`,`tool.error`,`session.result`}, `node_id`, `session_id`, `sequence` int, `timestamp`, `data` any JSON.
- **Validation order:** event_type missing → `unknown trace event_type: "<v>"` → timestamp → id.
- **Response:** 201/200 `{"id","run_id","event_type","timestamp"}`.

#### `POST /api/v1/workspaces/:slug/runs/:run_id/traces/batch`
- As events/batch; per-item checks: event_type missing → unknown trace type → id → timestamp. Not counted on the audit-events metric.

#### `POST /api/v1/workspaces/:slug/runs/:run_id/postmortem`
- **Request body:** `schema_version` int optional (must be 1 if present); `run_status` **required** enum; `started_at`, `completed_at` **required** RFC 3339; `task_summary` **required** (non-null JSON; if an object it must have key `total`); `cost_summary` **required** (non-null JSON); `blocked_tasks`, `session_history` optional any JSON (stored as given, defaults `[]`).
- **Validation order:** 422 `unsupported schema_version` (`error_type: unknown_schema_version`) → `missing required field: run_status` → `invalid run_status: must be one of stalled, block_limit, cost_limit, session_limit` → `missing required field: task_summary` → `missing required field: cost_summary` → `missing required field: started_at` → `missing required field: completed_at` → `invalid started_at: must be RFC 3339` → `invalid completed_at: must be RFC 3339` → `task_summary missing required field: total`.
- **Response:** 201/200 `{"run_id","run_status","created_at"}` (`created_at` = server now, also on duplicate; `run_status` echoes the request).
- **Side effects:** one row per run_id (global key; a second POST, even with different content or from another workspace, is a no-op 200).

#### `GET /api/v1/workspaces/:slug/runs/:run_id/events` | `/sessions/outcomes` | `/tools/calls` | `/tools/errors` | `/traces`
- **Auth / scope:** `audit:read` or `audit:write`; workspace read access (AUD-FR-050).
- **Query params (common):** `since`, `until` (RFC 3339, both inclusive), `order` (`asc` default | `desc`), `limit` (default 100, max 1000), `cursor`. Route filters (exact match):
  | Route | Filters | Items key |
  |---|---|---|
  | `/events` | `event_type`, `severity`, `node_id`, `session_id` | `events` |
  | `/sessions/outcomes` | `node_id`, `session_id`, `status` | `outcomes` |
  | `/tools/calls` | `node_id`, `session_id`, `tool_name` | `calls` |
  | `/tools/errors` | `node_id`, `session_id`, `tool_name` | `errors` |
  | `/traces` | `event_type`, `node_id` (no `session_id`) | `traces` |
- **Response:** 200 `{"<items key>":[…],"next_cursor":string|null,"has_more":bool}`; item fields = all table columns of 11.3.2 except `archetype` (events), with `timestamp` and `ingested_at` as RFC 3339 UTC strings and JSON columns decoded. Empty → `[]`.
- **Errors (in order):** 401; 403 `insufficient scope: audit:read required`; 404 `workspace not found`; 500; 400 `invalid run_id format…`; 400 `invalid cursor` | `invalid since timestamp` | `invalid until timestamp`; 500 `internal server error`.

#### `GET /api/v1/workspaces/:slug/runs/:run_id/postmortem`
- **Auth / scope:** as per-run reads. Query parameters are ignored.
- **Response:** 200 `{"run_id","workspace","schema_version","run_status","started_at","completed_at","task_summary","cost_summary","blocked_tasks","session_history","ingested_at"}`.
- **Errors:** as per-run reads; 404 `no postmortem found for the given run_id` (`error_type: postmortem_not_found`) when none for this run in this workspace; 500.

#### `GET /api/v1/workspaces/:slug/runs/:run_id/transcript`
- **Purpose:** rebuild one node's conversation from `agent_traces`.
- **Auth / scope:** as per-run reads. `run_id` format is **not** validated (an unknown or malformed run just yields `[]`).
- **Query params:** `node_id` **required** (400 `node_id is required`, checked after workspace access).
- **Behaviour:** selects traces with `workspace=slug AND run_id AND node_id`, ordered by `timestamp` asc then `sequence` asc. Mapping: `session.init`→`system`, `assistant.message`→`assistant`, `tool.use`→`tool_use`, `tool.error`→`tool_error`; `session.result` and anything else is skipped. `content` = `data.content` when it is a string, else `""`; `tool_name` = `data.tool_name` when a string, else `null`; non-JSON data → empty content, null tool_name.
- **Response:** 200 `{"run_id":"<path>","node_id":"<query>","messages":[{"role","content","tool_name":string|null,"timestamp"}]}`; `timestamp` is DuckDB's text rendering of the instant, e.g. `2026-09-01 08:00:00.5+00` (not RFC 3339).
- **Errors:** 401; 403 scope; 404 `workspace not found`; 500; 400 `node_id is required`; 500.

#### `GET /api/v1/audit`
- **Purpose:** unified query over hub and agent events.
- **Auth / scope:** `audit:read` or `audit:write`; confinement AUD-FR-053. Bound token allowed.
- **Query params:** `source` (`hub`|`agent`|empty=both; any other value → empty result); `workspace`; `event_type` (exact); `event_type_prefix` (SQL `LIKE '<prefix>%'`, so `_`/`%` inside act as wildcards); `severity`; `actor_id`, `actor_type`, `resource_type`, `action` (hub-only: any of them excludes agent events); `run_id` (agent-only: excludes hub events); `since` (inclusive), `until` (exclusive) RFC 3339; `limit` (default 100, max 1000); `cursor`. Note: there is no `resource_id` filter and no `order` parameter.
- **Behaviour:** order of checks: scope → param parsing (cursor, since/until, since<until) → readable set / workspace param → query. Hub event instant = parsed `timestamp`, falling back to `ingested_at` when unparseable/empty. Results sorted by instant desc, then id desc; cursor encodes (RFC 3339 Nano timestamp, id) of the last event; the next page returns strictly older events (or same instant with smaller id).
- **Response:** 200
```json
{"events":[{"id","event_type","source":"hub|agent","timestamp":"RFC3339Nano UTC","severity","workspace",
            "actor_id","actor_type","resource_type","resource_id","action",   // strings for hub, null for agent
            "run_id","node_id","session_id","archetype"}],                    // strings for agent (archetype ""), null for hub
 "next_cursor":string|null,"has_more":bool}
```
- **Errors (in order):** 401; 403 `insufficient scope: audit:read required`; 400 `invalid cursor`; 400 `invalid query parameter: since/until must be RFC 3339`; 400 `since must be before until`; 500 (readable lookup); 404 `workspace not found`; 500.

#### `GET /api/v1/events`
- **Purpose:** live SSE stream of hub events.
- **Auth / scope:** `audit:read` or `audit:write`; confinement AUD-FR-053; bound token allowed.
- **Query params:** `workspace` (only this workspace; outside readable set → 404 before streaming); `category` (`hub`: drop events whose type does not start with `hub.` (heartbeats pass); `agent`: drop events whose type starts with `hub.` — since only hub events are broadcast, an `agent` stream receives only heartbeats; other values: no filter); `run_id` accepted and ignored.
- **Behaviour:** checks in order: scope → readable set (500/404) → connection limit (503) → `200` with headers `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`; immediately writes `event: heartbeat\ndata: {"timestamp":"<RFC3339 seconds UTC>"}\n\n` and flushes. Then for each queued item: heartbeat → same heartbeat frame; hub event → `event: audit_event\ndata: <hub event JSON 11.3.4>\n\n`. No `id:` lines, no replay of past events, no `Last-Event-ID` support. The connection ends when the client disconnects, a write fails, or the manager reaps it.
- **Errors:** 401; 403 `insufficient scope: audit:read required`; 500 `internal server error`; 404 `workspace not found`; 503 with body `{"error":"too many SSE connections"}` (NOT the standard error envelope).

#### `GET /metrics`
- **Purpose:** Prometheus text exposition of the hub's own registry (no Go runtime/process collectors).
- **Auth:** none (outside the API group, unauthenticated); not under `/api/v1`.
- **Response:** 200 `text/plain; version=0.0.4…` with the metrics of 11.7.4.

### 11.6 CLI

None. No `parley` command calls the session, cost, audit, transcript, event-stream or metrics endpoints (verified in `internal/cli`, `carrypatch/cli`, `cmd/parley`). The CLI documentation only points users to `GET /api/v1/events?workspace=<slug>` and the audit query for forge events.

### 11.7 Background behaviour

#### 11.7.1 SSE manager
- One process-wide manager started at boot; limit = `AF_SSE_MAX_CONNECTIONS` (default 100; unparseable or <= 0 → 100 with a warning log).
- Broadcast channel capacity 512: publishing never blocks; when full the event is dropped (debug log).
- Fan-out: per connection, apply filters (workspace equality, readable set membership — events with `workspace=""` only reach unrestricted connections, category). Per-client buffer 256: when full, the oldest queued item is dropped to make room for the new one.
- Heartbeat: every 30 s (fixed) a heartbeat item is queued to every connection, bypassing filters, non-blocking (skipped for a full buffer). Then connections whose last successful read from their buffer is older than 60 s (fixed) are closed (their stream ends).
- Each read of an item by the connection's writer refreshes its "last read" time.
- The broadcaster recovers from panics and restarts. Stopped on shutdown.
- `afhub_sse_connections` = number of registered connections (updated on register/unregister).

#### 11.7.2 Retention (hourly, plus once at start)
Steps in execution order (cutoff = now − N days, UTC):
| # | Label (`step`) | Action |
|---|---|---|
| 1 | `step_1_agent_records` | delete from `agent_audit_events`, `session_outcomes`, `tool_calls`, `tool_errors` where `timestamp < now−AF_AUDIT_MAX_AGE_DAYS` |
| 2 | `step_2_max_runs` | if distinct `run_id` count in `agent_audit_events` > `AF_AUDIT_MAX_RUNS`, delete all `agent_audit_events` rows of the oldest excess runs (ranked by their earliest event `timestamp`); other tables untouched |
| 3 | `step_3_hub_events` | delete `hub_audit_events` with `ingested_at < now−AF_AUDIT_MAX_AGE_DAYS` |
| 0 | `pre_step_force_close` | `active` sessions with `started_at < now−AF_SESSION_MAX_ACTIVE_AGE_DAYS` → `status=timeout`, `ended_at=now` (no event, no error_message, gauge fixed in step 9) |
| 4 | `step_4_aged_sessions` | delete terminal sessions (`completed`,`failed`,`timeout`,`terminated`) with `started_at < now−AF_SESSION_MAX_AGE_DAYS`; active never deleted |
| 5 | `step_5_orphaned_usage` | delete `token_usage` whose `session_id` no longer exists |
| 6 | `step_6_aged_traces` | delete `agent_traces` with `timestamp < now−AF_TRACE_MAX_AGE_DAYS` |
| 7 | `step_7_aged_postmortems` | delete `postmortems` with `ingested_at < now−AF_POSTMORTEM_MAX_AGE_DAYS` |
| 8 | `step_8_orphaned_workspace` | for all nine tables: delete rows with non-empty workspace (`workspace` / `workspace_slug`) not present in SQLite `workspaces` (any status) and `ingested_at < now−AF_AUDIT_ORPHAN_RETENTION_DAYS`; if SQLite has no workspaces at all, every row with a non-empty workspace older than the grace period goes |
| 9 | `step_9_recalibrate_gauges` | reset and set `afhub_audit_table_rows{table}` = `COUNT(*)` for the nine tables; reset and set `afhub_agent_sessions_active{workspace}` from active-session counts (workspaces with none disappear; no placeholder is restored) |
| 10 | — | if no step failed: `afhub_retention_last_run_timestamp_seconds` = now (Unix s) and log rows deleted per table |
A failing step increments `afhub_retention_errors_total{step=<label>}` and logs; the run returns an error (logged by the maintenance loop) but continues with the next steps.

#### 11.7.3 Start-up / shutdown
- Order at start: metrics registry created and installed as process-wide sink → audit store opened and schema created → SSE manager started → emitter created → routes registered → maintenance loop (retention first) started.
- Prometheus middleware is installed on the root router (all requests, including `/metrics`, git and webhook routes).
- No recovery work for sessions at start (stale active sessions are only handled by retention).

#### 11.7.4 Prometheus metrics (own registry)
| Metric | Type | Labels | Semantics |
|---|---|---|---|
| `afhub_http_requests_total` | counter | `method`, `path` (route template, e.g. `/api/v1/sessions/:id`), `status` | every request, counted after the handler returns |
| `afhub_http_request_duration_seconds` | histogram | `method`, `path` | buckets 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10 |
| `afhub_agent_sessions_active` | gauge | `workspace` | +1 on new session, −1 on complete transition and per force-closed session; recalibrated by retention |
| `afhub_agent_tokens_total` | counter | `workspace`, `model`, `direction` (`input`,`output`,`cache_read`) | per usage report |
| `afhub_audit_events_total` | counter | `source` (`hub`,`agent`), `event_type` (`batch` for agent batches) | |
| `afhub_sse_connections` | gauge | — | open SSE streams |
| `afhub_jobqueue_depth` | gauge | `type`, `status` | registered but never set: no series, so it does not appear in the output |
| `afhub_retention_errors_total` | counter | `step` | |
| `afhub_retention_last_run_timestamp_seconds` | gauge | — | |
| `afhub_audit_table_rows` | gauge | `table` | sampled by retention |
| `afhub_sandboxes_active` | gauge | `workspace` | see sandboxes section |
| `afhub_sandbox_creates_total` | counter | `result` (`created`,`failed`) | see sandboxes section |
| `afhub_sandbox_lease_seconds` | histogram | — | buckets 60, 300, 600, 1200, 1800, 3600, 7200, 14400, 28800 |
At start every vec metric except `afhub_jobqueue_depth` is pre-initialised with empty-string label values (`""`) so it is listed immediately; `afhub_sandbox_creates_total` with `result="created"` and `"failed"`.

### 11.8 Events

#### 11.8.1 Envelope
Stored row / query fields: `id` (UUIDv4), `event_type`, `actor_id`, `actor_type`, `resource_type`, `resource_id`, `action`, `workspace` (`""` when not workspace-scoped), `severity` (always `info`), `timestamp` (RFC 3339 Nano UTC, emission time), `metadata` (object). SSE frames carry `id,event_type,actor_id,actor_type,resource_type,resource_id,action,workspace,metadata?,timestamp`. Unless stated otherwise the actor is the HTTP caller: `actor_id` = caller user id (`""` for admin tokens), `actor_type` = caller credential type (`admin_token`|`api_key`|`pat`). All hub event types start with `hub.`.

Agent-side event types (stored in `agent_audit_events`, never broadcast) are free-form strings chosen by agents; the hub only knows the severity defaults of AUD-FR-035. Trace event types are the closed set of 11.3.2.

#### 11.8.2 Catalogue of hub event types

Sixty event types, grouped by area. The tables give the exact field values and when each event fires. "Caller" means the actor is the HTTP caller (if there is no auth: `system` with an empty id). "Detached" means the emission does not depend on the request staying open. Every emission happens after the state change it reports, unless the table says otherwise. No event is emitted by apikit-provided routes: creating or deleting a user, org changes, org membership, API keys, and unbound PAT mint or revoke all emit nothing. The SSE `heartbeat` is not an audit event; it is never stored.

**Tokens**
| event_type | Trigger | Actor | resource_type / resource_id / action | workspace | metadata |
|---|---|---|---|---|---|
| `hub.token.bind` | `POST /workspaces/:slug/tokens` minted a token and the binding committed; success only | caller | `token` / token id / `bind` | slug | `token_id`, `workspace`, `permissions` ([]string, `[]` when unreadable), `expires_at` (string or `null`). Never the token itself |
| `hub.token.bound_refused` | A bound PAT is refused by the API binding guard, a handler binding check, or the git server. At most one event per token per minute (in-memory limiter, reset on restart) | `pat` / bound token's user id | `token` / token id / `refuse` | bound slug | `token_id`, `bound_workspace`, `requested_workspace` (only when the request named a slug), `method` (may be `""`), `route` (route template or git route), `status` (int 403/404), `surface` (`api`\|`git`), `suppressed` (int: refusals held back since the previous event) |

**Sandboxes.** resource_type is always `sandbox`, resource_id the lease id, workspace the lease's workspace.
| event_type | Trigger | Actor | action | metadata |
|---|---|---|---|---|
| `hub.sandbox.lease` | `POST /workspaces/:slug/sandboxes` issued a lease (after insert) | caller | `lease` | `lease_id`, `profile`, `purpose`, `ttl` (Go duration string, e.g. `1h0m0s`), `secret_names`, `optional_secret_names` ([]string). Never secret or env values, never the lease token |
| `hub.sandbox.create` | Gateway created the box and the lease went leased→active. Nothing is emitted if the lease ended during the provider call | `lease` / lease `created_by` (`admin` for admin-token leases) | `create` | `lease_id`, `sandbox_id`, `image`, `network` |
| `hub.sandbox.failed` | Lease went leased→failed: profile gone, required secret missing, spec/env error, or the provider create failed. Nothing is emitted if a DELETE won the race | `lease` / `created_by` | `fail` | `lease_id`, `error` (scrubbed message stored on the lease) |
| `hub.sandbox.destroy` | One event per change into `destroyed`: (a) the holder closed the box through the gateway; skipped within 2 s of `expires_at`, which is left to expiry; (b) `DELETE /workspaces/:slug/sandboxes/:id`; (c) start-up recovery or the shutdown sweep | (a) `lease` / `created_by`; (b) caller; (c) `system` / `""` | `destroy` | `lease_id`, `sandbox_id` (string or `null`), `reason`: (a) `client`, (b) `api`, (c) `restart` at start-up recovery; `shutdown` replaces the reason while the hub shuts down |
| `hub.sandbox.expire` | Time ended a lease: a `leased` lease got no box before its deadline, or an `active` lease's box was destroyed at `expires_at`. Fired by the sweeper, or by a create that arrives late. No `destroy` event in this case | `system` / `""` | `expire` | `lease_id`, `state` (`expired`\|`destroyed`), `sandbox_id` (only when a box was destroyed) |

**Forge writes.** One event per write request that sent at least one request to the forge. It fires on success **and** on failure, before the answer is written. A request the hub refuses itself (401/403/404/400/413/422) emits nothing. Actor: caller. Workspace: slug. Detached. Common metadata: `target` (`origin`\|`upstream`); `ok` (bool); `error_type` (only when `ok` is false: the error_type the hub answered with); `number` (int, when known and > 0); `url` (issue/PR/comment URL with userinfo removed, when known). The event never holds a credential, title, body, comment text, or forge message.
| event_type | Route | resource_type / resource_id / action | extra metadata |
|---|---|---|---|
| `hub.forge.issue.create` | `POST /workspaces/:slug/issues` | `issue` / new number (`""` on failure) / `create` | — |
| `hub.forge.issue.update` | `PATCH …/issues/:n` | `issue` / n / `update` | `fields` (sorted names of changed fields from title, body, state, labels, assignees) |
| `hub.forge.issue.close` | `POST …/issues/:n/close` (one event, even with a comment) | `issue` / n / `close` | `comment_url` (when a comment was posted, also if the close then failed) |
| `hub.forge.comment.create` | `POST …/issues/:n/comments` | `comment` / n / `create` | (`url` is the comment's) |
| `hub.forge.label.add` | `POST …/issues/:n/labels` | `label` / n / `add` | `labels` ([]string given) |
| `hub.forge.label.remove` | `DELETE …/issues/:n/labels/:name` | `label` / n / `remove` | `label` |
| `hub.forge.label.create` | `POST /workspaces/:slug/labels` (created or found) | `label` / label name / `create` | `label`, `created` (bool; false when it existed or the call failed) |
| `hub.forge.pull_request.create` | `POST …/pulls` | `pull_request` / new number (`""` on failure) / `create` | `head`, `base`, `draft` |
| `hub.forge.pull_request.comment` | `POST …/pulls/:n/comments` | `pull_request` / n / `comment` | (`url` is the comment's) |

**Forge webhook configuration.** Actor: caller. resource_type `webhook`, resource_id = slug, workspace = slug. Detached. The secret is written to the secrets store directly, so no `hub.secret.*` event fires.
| event_type | Trigger | action | metadata |
|---|---|---|---|
| `hub.forge.webhook.set` | `PUT /workspaces/:slug/forge/webhook` stored the secret (success only) | `set` | `generated` (bool, true when the hub generated it) |
| `hub.forge.webhook.unset` | `DELETE /workspaces/:slug/forge/webhook`, whether or not a secret existed (a 500 emits nothing) | `unset` | `existed` (bool) |

**Forge deliveries** (relayed by `POST /hooks/forge/:slug`). Emitted only for verified, newly recorded deliveries, **after** the HTTP answer to the forge. Duplicate delivery ids and unmappable payloads emit nothing. One delivery may map to several events, emitted in order: a GitLab issue update emits `unlabeled` events first, then `labeled` events, one per label sorted by name. Actor: `actor_type` `forge`, `actor_id` `""`. Workspace: slug. Action: the last segment of the type. Detached. Common metadata: `forge` (`github`\|`gitlab`), `target` (`origin`\|`upstream`), `delivery_id`, `actor` (the forge login, `""` when absent). URLs carry no userinfo, and no other payload field is stored.
| event_type | resource_type / resource_id | extra metadata |
|---|---|---|
| `hub.forge.issue.opened`, `hub.forge.issue.edited`, `hub.forge.issue.closed`, `hub.forge.issue.reopened` | `issue` / number in decimal (GitLab `iid`) | `number`, `title` (cut to 256 bytes), `url`, `labels` (label names) |
| `hub.forge.issue.labeled`, `hub.forge.issue.unlabeled` | `issue` / number | `number`, `label`, `labels` (all current labels), `url` |
| `hub.forge.comment.created` | `comment` / issue or PR number | `number`, `comment_url`, `pull_request` (bool) |
| `hub.forge.pull_request.opened`, `hub.forge.pull_request.closed` (closed without merge), `hub.forge.pull_request.merged` (never also `closed`), `hub.forge.pull_request.reopened`, `hub.forge.pull_request.synchronize` (commits pushed; GitLab update with `oldrev`) | `pull_request` / number | `number`, `url`, `head`, `base`, `merged` (bool) |
| `hub.forge.push` | `repository` / pushed ref | `ref`, `before`, `after` |

**Workspaces, branches, git and sessions.** These use the request context. Actor: caller, unless stated otherwise.
| event_type | Trigger | resource_type / resource_id / action | workspace | metadata |
|---|---|---|---|---|
| `hub.workspace.create` | `POST /workspaces` succeeded (after insert, credential secrets, clone-job enqueue) | `workspace` / slug / `create` | slug | `git_url` (as given, not scrubbed), `branch` (`""` if none). The credential secrets created here emit no `hub.secret.create` |
| `hub.workspace.update` | `PATCH /workspaces/:slug` succeeded | `workspace` / slug / `update` | slug | `fields` ([]string, in the order `display_name`, `description`, `org_id`, `settings`, only those changed) |
| `hub.workspace.archive` | `POST /workspaces/:slug/archive` succeeded (also when the push to origin failed and the directory was kept). Emitted after the session force-close events | `workspace` / slug / `archive` | slug | `head_sha` (only when the workspace was `ready`) |
| `hub.workspace.reactivate` | `POST /workspaces/:slug/reactivate` succeeded | `workspace` / slug / `reactivate` | slug | `{}` |
| `hub.workspace.delete` | `DELETE /workspaces/:slug` (archived only) committed | `workspace` / slug / `delete` | slug | `revoked_tokens` (sorted []string of the bound-token ids revoked; `[]` when none) |
| `hub.workspace.sync` | `POST /workspaces/:slug/sync` on a standard (non-`carry_patch`) workspace succeeded. Failures emit nothing, and so do carry-patch workspaces | `workspace` / slug / `sync` | slug | `result` (`up_to_date`\|`fast_forward`\|`reset_to_upstream`) |
| `hub.branch.push` | `POST /workspaces/:slug/branches/:name/push`: one event per push attempt handed to the transport, success or failure, recorded even if the caller went away. Hub refusals (401/403/404/400/409) and 500s emit nothing. Detached | `branch` / branch name / `push` | slug | `branch`, `sha` (trunk tip offered), `force` (bool), `ok` (bool), `remote` (`origin`), `remote_url` (workspace git_url without userinfo), then either `result` (`pushed`\|`up_to_date`\|`forced`, when ok) or `error_type` (when not ok). Never a credential or a forge message |
| `hub.git.push` | `POST /git/:org/:slug.git/git-receive-pack` with at least one accepted ref update. One event per push; none if every update was rejected | `workspace` / `""` / `push` | slug | `head_sha` (new sha of the last non-delete command, `""` if the push only deleted refs), `refs_updated` ([]string of all accepted refs, deletions included) |
| `hub.session.force_closed` | Archive or delete of the workspace terminated an active agent session. One event per session, emitted before the workspace row is removed on delete | `session` / session id / `terminate` | slug | `session_id`, `reason` (`"workspace archived"`\|`"workspace deleted"`). Actor `system` / `""` |

**Secrets and variables.** Six types: `hub.secret.create`, `hub.secret.update`, `hub.secret.delete`, `hub.variable.create`, `hub.variable.update`, `hub.variable.delete`. Each fires at three scopes: `/user/{secrets|vars}`, `/orgs/:slug/{secrets|vars}` and `/workspaces/:slug/{secrets|vars}`. `create` = `POST <base>`, one event **per entry** created, after the transaction commits. `update` = `PATCH <base>/:key`. `delete` = `DELETE <base>/:key`. Success only, so reads emit nothing. Actor: caller. resource_type: `secret` or `variable`. **resource_id is `""` and workspace is `""` at every scope.** These events are therefore visible only to admin tokens in `GET /audit` and `GET /events`, and the orphan retention step never removes them. Metadata: `scope` (`user`\|`org`\|`workspace`), `key`. Never the value.

**Merge queue.** Workspace = slug.
| event_type | Trigger | Actor | resource_type / resource_id / action | metadata |
|---|---|---|---|---|
| `hub.merge.enqueue` | `POST /workspaces/:slug/merges` enqueued a job. A 409 duplicate, a requeue, and `POST /rebase` emit nothing | caller | `merge` / job id / `enqueue` | `target_branch`, `source_ref`, `job_id` |
| `hub.merge.complete` | A merge job merged and updated the ref. The idempotent "already merged" outcome emits nothing | `system` / `""` | `merge` / `""` / `""` | `base_sha`, `merged_sha` |
| `hub.merge.fail` | **Every** failed attempt, retryable ones included, so one job may emit several. An invalid payload emits nothing | `system` / `""` | `merge` / `""` / `""` | `reason` (error text: either a JSON string `{"reason":"WouldConflict\|BranchNotReady\|…","conflict_files":[…]}` or raw git error text, not scrubbed) |

**Carry-patch.** resource_type is always `patch`, workspace the slug.
| event_type | Trigger | Actor | resource_id / action | metadata |
|---|---|---|---|---|
| `hub.patch.create` | `POST /workspaces/:slug/patches` with a single JSON object, after insert. An array (batch) body and an `if_not_exists` hit emit nothing | caller | branch name / `create` | `branch_name`, `position` (int), `branch_resolution` (`local`\|`origin_tracking`\|`origin_fetch`\|`skipped`) |
| `hub.patch.delete` | `DELETE /workspaces/:slug/patches/:id` soft-deleted a non-deleted patch **[FIX-09]**. Update, reorder and restore emit nothing | caller | branch name / `delete` | `branch_name` |
| `hub.patch.reset` | `POST …/patches/:id/reset-to-origin` succeeded. Emitted before the row is re-read, so it remains even if the request then answers 500 | caller | branch name / `reset` | `branch_name`, `action` (`none`\|`created`\|`fast_forwarded`\|`replaced`), `origin_sha`, `local_sha` (only if a local branch existed), `replaced_sha` (only for `replaced`) |
| `hub.patch.replace` | (a) A reset with action `replaced` (emitted after `hub.patch.reset`). (b) A carry-patch sync in origin mode, one per replaced branch | caller | branch name / `replace` | `branch_name`, `replaced_sha`, `origin_sha`; (a) also `trigger: "reset_to_origin"`, (b) no `trigger` |
| `hub.patch.sync` | `POST /workspaces/:slug/sync` on a `carry_patch` workspace with `PATCH_BRANCH_SOURCE=origin`, once per sync, after its replace events. Fires on completion, and also on the ref-write-failure 500 path with the outcomes produced so far. Other failures and hub mode emit nothing | caller | slug / `sync` | `origin_fetched` (bool, true), `created`, `fast_forwarded`, `replaced`, `diverged`, `missing_on_origin` ([]string each, never null) |
| `hub.patch.mirror_failed` | After a git push, carry_patch workspaces with `PUSH_PATCHES_TO_ORIGIN=true` and `PATCH_BRANCH_SOURCE=hub` mirror each registered non-integration patch branch to origin. One event per branch whose force-push failed; if credential resolution fails, one per applicable branch. Asynchronous (background goroutine) | `system` / `""` | `""` / `mirror` | `branch_name`, `error` (userinfo removed) |
| `hub.rebuild.enqueue` | `POST /workspaces/:slug/rebuild` enqueued a rebuild. A 409 emits nothing, and so do automatic rebuilds (after sync, git push, recovery) | caller | job id / `enqueue` | `job_id`, `patch_count` (int: active + conflict patches) |
| `hub.rebuild.complete` | A rebuild job succeeded, including `continue` mode with conflicts. Emitted after the follow-up check | `system` / `""` | `""` / `""` | `patches_applied` (int) |
| `hub.rebuild.fail` | **Every** failed rebuild attempt, including retryable transient errors and fail-fast conflicts. An invalid payload emits nothing. Detached | `system` / `""` | `""` / `""` | `reason` (error text, e.g. `conflict in patch "<branch>": <files>`, not scrubbed) |
| `hub.rebuild.push_failed` | `REBUILD_PUSH_INTEGRATION_BRANCH=true` and pushing the integration branch to origin failed. The job still succeeds, so `complete` follows | `system` / `""` | `""` / `""` | `integration_branch`, `reason` (raw text) |
| `hub.rebuild.followup` | After a success or a fail-fast conflict, when a patch tip or the upstream base moved during the run and a follow-up rebuild was actually enqueued (not a duplicate). Gated by `AUTO_REBUILD_AFTER_PUSH` / `AUTO_REBUILD_AFTER_SYNC`. The follow-up job's submitter is `system:stale-snapshot` | `system` / `""` | `""` / `""` | `stale_patches` ([]string), `upstream_stale` (bool), `follow_up_job_id` |

Merge and rebuild job events are emitted from queue workers with the job's context. `hub.token.bound_refused` and `hub.patch.mirror_failed` use a background context. All other events are emitted synchronously in the request handler.

### 11.9 Configuration

All are environment variables (no `config.toml` keys exist for this area); read once at start.

| Variable | Type | Default | Validation / effect |
|---|---|---|---|
| `AF_AUDIT_DB_PATH` | path | `<dir of [database].path>/audit.duckdb` | empty → default |
| `AF_AUDIT_MAX_AGE_DAYS` | int >= 1 | 90 | agent events, outcomes, tool calls, tool errors (by `timestamp`) and hub events (by `ingested_at`) |
| `AF_AUDIT_MAX_RUNS` | int >= 1 | 50 | runs kept in `agent_audit_events` |
| `AF_TRACE_MAX_AGE_DAYS` | int >= 1 | 30 | traces |
| `AF_SESSION_MAX_AGE_DAYS` | int >= 1 | 90 | terminal sessions (by `started_at`) |
| `AF_SESSION_MAX_ACTIVE_AGE_DAYS` | int >= 1 | 7 | active sessions timed out |
| `AF_POSTMORTEM_MAX_AGE_DAYS` | int >= 1 | 180 | postmortems (by `ingested_at`) |
| `AF_AUDIT_ORPHAN_RETENTION_DAYS` | int >= 1 | 30 | grace period for data of deleted workspaces |
| `AF_AUDIT_WRITE_TIMEOUT_MS` | int >= 1 | 2000 | ingestion write timeout; invalid → default silently |
| `AF_SSE_MAX_CONNECTIONS` | int >= 1 | 100 | invalid → default with warning |
For the seven retention variables a non-integer or a value < 1 falls back to the default with a warning log. Fixed (not configurable): SSE heartbeat 30 s, stale timeout 60 s, per-client buffer 256, broadcast buffer 512, retention interval 1 h, batch max 1000, list limits.

### 11.10 Edge cases and error handling

- Admin tokens ingest into any slug, including non-existent ones; rows of unknown workspaces are later removed by the orphan step.
- API keys are treated like PATs for ingestion ownership (user id must be the owner) but never scope-checked. Without user id (admin) `credential_id` is `""`.
- Session ownership is by user id: a different credential of the same user (PAT vs API key) may complete the session.
- Global primary keys: re-using an `id` (session, usage, any ingestion record) or a postmortem `run_id` that already exists in another workspace/run/session yields a "duplicate" (200, for usage too **[FIX-15]**) and nothing is stored for the new location; for `POST /sessions/:id/usage` the stored record of the other session is returned; for postmortems a later GET in the second workspace answers 404 `postmortem_not_found`.
- Duplicate-replay responses of ingestion are built from the request, so they may differ from the stored row (e.g. a different `timestamp`/`severity` in the replay).
- `duration_ms` and `sequence` of outcomes/tool calls/traces are 32-bit columns: values outside the int32 range fail the insert → 500.
- Empty request body on POST → fields zero-valued (e.g. `workspace_slug is required`, `missing required field: event_type`, `batch array must not be empty`).
- JSON fields `input`, `output`, `token_usage`, `data`, `blocked_tasks`, `session_history`, `task_summary` (non-object), `cost_summary`, batch `payload` accept any JSON type; JSON `null` counts as absent (stored as default; `null` task/cost summary → "missing required field").
- `GET /sessions?since=` with a value the database cannot parse → 500 (no 400 validation); DuckDB also accepts forms like `2026-09-01`.
- `GET /audit?source=<other>` → 200 empty list. `?source=hub&run_id=…` and `?source=agent&actor_id=…` → empty lists.
- Unified query: hub rows with empty/unparseable `timestamp` sort by `ingested_at` and report it as their timestamp.
- SSE: a client whose writer is blocked for more than 60 s is reaped; buffered events beyond 256 drop the oldest; events published while the 512-slot broadcast buffer is full are lost; heartbeats may be skipped for a full buffer. Reconnects get no backlog.
- After retention recalibration, a workspace with no active sessions disappears from `afhub_agent_sessions_active`, and the `""` placeholder is removed by the reset.
- Archive/delete force-close happens even if later steps of delete fail; failures are logged only.
- Retention step 2 removes only agent audit events of the excess runs; their outcomes, tool calls, tool errors, traces and postmortems remain until their own age limits.

### 11.11 Discrepancies between docs and code

In every row, the code is right.

| # | Docs say | Code does | Evidence |
|---|---|---|---|
| D1 | The per-run GET routes default to `order` `desc` and `limit` 50 (max 500), and `until` is exclusive (api.md:4092-4095) | Default `asc`, limit 100 (max 1000), and `until` is inclusive (`<=`). Tests confirm "oldest first, 100 by default, 1000 at most" | store_audit.go:375-388, 418-420; hub/internal/audit/query_test.go:18-21, 49-50 |
| D2 | `GET …/traces` filters on `session_id` (api.md:4317) | No `session_id` filter; only `event_type` and `node_id` | store_audit.go:771-806 |
| D3 | `duration_ms` of outcomes must not be negative (api.md:4190) | Not validated | handlers_audit.go:94-137 |
| D4 | `blocked_tasks` is an array of objects with `node_id` and `reason` (api.md:4352) | Any JSON is stored as given. A test stores `["t1"]` | handlers_audit.go:293-334; query_test.go:160 |
| D5 | The batch error example message is `"missing event_type"` (api.md:4163) | The message is `missing required field: event_type` | handlers_audit.go:404, 475 |
| D6 | Complete returns 409 when "another close of the session is under way" (api.md:1286) | 409 `session is not active` only when the status is neither active nor terminal. A concurrent close returns 200 with the terminal record | handlers.go:146-161; store.go:187-199 |
| D7 | The `POST /sessions` errors are only 400 and 404 (api.md:1245-1250) | Also 403 `sessions:write scope required`, and 403 `org members have read-only access to this workspace` with `error_type: forbidden` for org members | handlers.go:27-28, 45; auth.go:131-133; sessions_test.go:61 |
| D8 | Session and usage `id` is a "Client-chosen UUID" (api.md:1223, 1307) | Any string is accepted; there is no UUID validation. Only the ingestion routes validate UUIDs | handlers.go:49-51, 234-236 |
| D9 | Idempotent records answer 200 on replay (general rule) | `POST /sessions/:id/usage` answers **201** with the stored record on a duplicate id. Token counters are incremented again by the request's counts. → **Resolved by FIX-15:** the documented behaviour (200, no change) is required | handlers.go:251-271; usage_test.go:34-36 |
| D10 | Transcript `timestamp` is RFC 3339, e.g. `2026-09-01T12:00:00Z` (api.md:4482) | DuckDB's text cast, e.g. `2026-09-01 12:00:00+00`. Verified against go-duckdb v1.8.5 | handlers_unified_query.go:560 |
| D11 | The transcript is reconstructed "from its agent events" (architecture.md:2002) | It is reconstructed from `agent_traces`; `run_id` is not validated | handlers_unified_query.go:544-561 |
| D12 | `AF_SESSION_MAX_AGE_DAYS` deletes "completed sessions" (configuration.md:595) | Deletes every terminal status (`completed`, `failed`, `timeout`, `terminated`) by `started_at` | retention.go:331-333 |
| D13 | `AF_AUDIT_MAX_AGE_DAYS` deletes "agent and hub audit events" (configuration.md:592) | Also deletes `session_outcomes`, `tool_calls` and `tool_errors`. Agent rows go by client `timestamp`, hub rows by `ingested_at` | retention.go:220-226, 314-316 |
| D14 | `afhub_jobqueue_depth` is "always 0" (api.md:4993) | It has no series and is absent from the output | metrics.go:154-157; metrics_test.go:52-54 |
| D15 | `GET /events` 503 when at the limit (api.md, error table) | The 503 body is `{"error":"too many SSE connections"}`, not the standard error envelope | handlers_unified_query.go:641-643 |
| D16 | `hub.rebuild.followup` metadata is `submitted_by` and the workspace slug (api.md hub events table) | Metadata is `stale_patches`, `upstream_stale` and `follow_up_job_id` (resource_id `""`, action `""`, actor `system`) | carrypatch/rebuild_executor.go:657-661; carrypatch/carrypatch.go:255-262 |
| D17 | The hub events table implies that secret/variable events belong to their workspace (api.md "Other hub events") | `workspace` and `resource_id` are `""` at every scope, so only admin tokens see them in `/audit` and `/events` | internal/secrets/handler_audit_config.go:40-90 |
| D18 | Cost: any other caller gets `403 workspace access denied` (api.md:1416-1419) | In production, a PAT bound to another workspace is answered 404 `workspace not found` by the binding guard first, because the route is workspace-scoped. The handler's 403 applies to unbound callers | internal/wsaccess/guard.go:67-73; workspace/routes.go:84 |
| D19 | `GET /sessions` `since` returns "sessions started after this time" (api.md:1341) | Inclusive (`started_at >= since`). The value is not validated, so an unparseable value yields 500 | store.go:299-302; sessions_test.go:157 |
| D20 | Code comment in types.go:52-59: `hub.sandbox.destroy` reasons are `client` and `api` | Also `restart` and `shutdown`, with actor `system` for recovery and shutdown. api.md is correct; the comment is stale | internal/sandboxes/expire.go:112, handle.go:230 |
| D21 | architecture.md and api.md do not mention it | `agent_audit_events.archetype` is never written; it is always `""` in the unified query | store_audit.go:81-87 |


---

## 12. Non-functional requirements

### 12.1 Deployment model

- **NFR-001** The hub runs as **one process and one replica**. Several things exist only in process memory and are never shared between processes:
  - the per-workspace lock and the rebuild-active flag
  - the clone queue
  - the SSE connections
  - the bound-refusal limiter
  - the sandbox gateway's box handles and Create claims

  The re-implementation MUST keep these semantics. A restart drops queued clone jobs from memory. **[FIX-05]** At start, every workspace left in `pending` or `cloning` is enqueued again (WS-FR-048).
- **NFR-002** State is kept in four places:
  - **SQLite** at `[database].path`, for identity, workspaces, patches, bindings, secrets and variables, the durable job queue, forge deliveries and sandbox leases. Every write transaction begins `IMMEDIATE`.
  - **DuckDB** at `AF_AUDIT_DB_PATH` (default `audit.duckdb`, next to the SQLite file), for audit events, agent telemetry, sessions and token usage.
  - **The workspace root on disk** at `[workspace].path`, holding `<root>/<slug>/trunk` and `<root>/<slug>/rebuild/<job id>`.
  - **Memory**, for the items listed in NFR-001.
- **NFR-003** Schema evolution is automatic. Every start creates and migrates its tables idempotently, with `CREATE TABLE IF NOT EXISTS` and idempotent column additions. No separate migration step is needed. The data-shape migrations that exist today MUST run at start as well, because older databases depend on them:
  - old PAT scopes → `read`/`write` scopes (§3)
  - workspace behaviour variables → settings (§4)
  - workspace status-column folding (§4)
  - the job-queue group-key and progress columns (§6)
  - lease expiry (§10)
- **NFR-004** The server needs the `git` CLI at run time (version ≥ 2.38, checked at start by the carry-patch runner) and CGo (for DuckDB). The `parley` client is a static binary (`CGO_ENABLED=0`). It MUST NOT depend on the server-only stacks: DuckDB, gRPC, AgentKit or the forge client.
- **NFR-005** Build-time identity: the token prefix is `af`, so credentials look like `af_admin_…`, `af_<key_id>_…` (API keys) and `af_pat_…` (see §3 for the exact formats), the client's config directory is `~/.af` and its environment prefix is `AF_`. Version, commit and build time are injected at link time and reported by `GET /version` and `parley version`.
- **NFR-006** Packaging deliverables:
  - the hub container image (`containers/hub`: config template and run script)
  - the sandbox image (`containers/sandbox`)
  - the agents image (`containers/agents`)
  - Kubernetes manifests (`deploy/`: configmap, deployment, PVC, route, service, secrets example)

  A re-implementation MUST provide an equivalent hub image and manifests that use the same config/env contract.

### 12.2 Security

- **NFR-010** Secret values are write-only through every interface: API, CLI, events and logs. They are stored base64-encoded and not encrypted (§8). *Re-implementation note:* adding encryption at rest is compatible as long as the API is unchanged.
- **NFR-011** Text that can carry credentials is scrubbed before it reaches a response, an event or a log. This covers git errors, remote URLs and forge errors. The rules are in §3.
- **NFR-012** Anti-enumeration: a caller without read access to a workspace gets `404 workspace not found`, exactly as if the workspace did not exist. The known exceptions, listed in their sections, MUST be preserved or explicitly decided.
- **NFR-013** Git subprocesses run through a hardened runner (§7: ref-name validation, a scrubbed environment, timeouts, process-group kill). The sandbox engine runs with `XDG_CONFIG_HOME` and `XDG_DATA_HOME` removed from its environment (§10).
- **NFR-014** Forge webhooks are authenticated by signature (GitHub HMAC) or by token (GitLab), and duplicate deliveries are rejected through the delivery log (§9). The ingress has its own 1 MiB body limit and is outside the API's bearer authentication.

### 12.3 Performance and limits

All fixed limits that clients can observe are listed in the configuration subsections of each area. The main ones:

| Limit | Value |
|---|---|
| Durable queue workers | 4 |
| Durable queue poll interval | 5 s |
| Durable queue shutdown grace | 30 s |
| Durable queue retries | up to 20, exponential backoff from 2 s, capped at 2 h |
| Per git command timeout | 10 min |
| `upload-pack` timeout | 30 min |
| Workspace credential check | 10 s |
| Branch push to origin | 2 min |
| Forge call deadline | 30 s |
| Sandbox provider calls | 30 s |
| `parley` request timeout | 30 s |
| `--wait` defaults | `--timeout 5m`, `--poll-interval 5s` |
| SSE | heartbeat 30 s, per-client buffer 256 |
| Ingestion batches | ≤ 1000 items |
| Audit write timeout | `AF_AUDIT_WRITE_TIMEOUT_MS`, default 2000 ms |

### 12.4 Observability

- **NFR-020** `GET /metrics` exposes the Prometheus metrics listed in §11. The metric names, types and labels are part of the compatibility surface, because dashboards and alerts depend on them.
- **NFR-021** `/healthz` reports liveness and pings SQLite. `/readyz` reports readiness. `/version` reports build info (§3).
- **NFR-022** The maintenance loop runs once at start and then every hour. It runs audit retention, forge delivery-log pruning, the binding sweep and the purge of soft-deleted patches **[FIX-12]**. One task failing never stops the others (§3, §11).

### 12.5 Compatibility of clients

- **NFR-030** Both clients MUST work unchanged against the new server:
  - the current `parley` binary
  - any AgentKit client that uses the sandbox gateway (gRPC services and lease-token authentication, §10)

  Old clients are the conformance oracle, except for the behaviours §1.8 fixes on purpose.

---

## 13. Re-implementation acceptance criteria

The re-implementation is accepted when all of the following hold. "Same as the current server" always excludes the 15 defect fixes of §1.8, which are deliberate differences (criterion 8).

1. **Route parity.** Every route in Appendix A exists with the same method and path, and no others exist. Unknown routes and wrong methods answer the way the current server does: never 405 (PLAT-FR-074).
2. **Contract parity.** For every endpoint in §§3–11, a black-box test of each listed success and error case gets the same status code, the same error envelope (`code`, `message`, `error_type`) and the same JSON field names, types and presence (null versus omitted). This must hold in the documented order of checks.
3. **CLI parity.** For every command in Appendix B, the same arguments and flags with the same defaults are accepted. The output on stdout and stderr and the exit codes match the current `parley` for success, hub errors, usage errors, timeouts and `--fail-on-diverged` (exit 3).
4. **Git parity.** Stock `git` can clone, fetch and push through `/git/<org>/<slug>.git`, using `parley credential-helper` and using a PAT as the password. The push rules give the same accept/reject results and the same messages: protected `refs/hub/replaced/`, carry-patch push control and post-push hooks. The git client sees the same status codes for auth failures, access refusals and unready workspaces.
5. **Job semantics parity.** Each of these behaves the same, including under restart:
   - merge and rebuild jobs: states, retries, serialization per workspace, cancel, requeue, deduplication and progress
   - the merge algorithm: rebase then fast-forward, no partial application
   - the rebuild algorithm: batch order, detection of patches merged upstream, rerere, conflict reporting, rollback and preview
6. **Event parity.** The same hub event types are emitted under the same conditions, with the same actor, resource, workspace and metadata keys (catalogue in §11.8). The SSE stream frames events identically.
7. **Config parity.** An existing `config.toml` and environment start the new server with the same behaviour. The first-boot admin-token procedure is identical.
8. **Defect fixes in place.** Each of the 15 fixes in §1.8 (FIX-01 to FIX-15) behaves as specified and has its own test. A conformance run against the baseline expects exactly these differences and no others. Every other item in the *Discrepancies* subsections and in Appendix C keeps the code behaviour, unless a recorded decision says otherwise.
9. **Data migration (decision required).** Decide whether the new hub must open an existing deployment's SQLite and DuckDB files in place. If yes, the logical data model in each section becomes a physical schema requirement: the same table and column names, and the same encodings, such as base64 secret values and settings JSON.


---

## Appendix A: REST route index

This list was extracted from the route registrations in the code: hub routes from `hub2/`, identity routes from `apikit/`. It is the complete HTTP surface. Paths under `/api/v1` are shown without the prefix.

**Bound tokens.** Routes marked **B** accept a workspace-bound token even though they have no `:slug`; the handler confines the token to its workspace. Every route with `:slug` accepts a bound token only for its own workspace. Every other route refuses bound tokens (see §3).

### A.1 Unauthenticated / infrastructure (root, outside `/api/v1`)

| Method | Path | Section |
|---|---|---|
| GET | `/healthz` | §3 |
| GET | `/readyz` | §3 |
| GET | `/version` | §3 |
| GET | `/metrics` | §11 |
| POST | `/hooks/forge/:slug` | §9 |
| GET | `/git/:org/:slug.git/info/refs` | §5 |
| POST | `/git/:org/:slug.git/git-upload-pack` | §5 |
| POST | `/git/:org/:slug.git/git-receive-pack` | §5 |

### A.2 Identity and access (from apikit, under `/api/v1`)

| Method | Path | Section |
|---|---|---|
| GET | `/auth/providers` | §3 |
| POST | `/auth/callback` | §3 |
| GET, PATCH | `/user` | §3 |
| GET | `/user/orgs` | §3 |
| GET | `/user/keys` | §3 |
| POST | `/user/keys/:key_id/refresh` | §3 |
| DELETE | `/user/keys/:key_id` | §3 |
| POST, GET | `/user/tokens` | §3 |
| GET, DELETE | `/user/tokens/:token_id` | §3 |
| PUT, PATCH, DELETE | `/user/tokens/:token_id/permissions` | §3 |
| POST, GET | `/users` | §3 (admin) |
| GET, PATCH, DELETE | `/users/:id` | §3 (admin) |
| POST | `/users/:id/promote`, `/users/:id/demote`, `/users/:id/block`, `/users/:id/unblock` | §3 (admin) |
| GET | `/users/:id/keys` | §3 (admin) |
| DELETE | `/users/:id/keys/:key_id` | §3 (admin) |
| GET | `/users/:id/tokens` | §3 (admin) |
| DELETE | `/users/:id/tokens/:token_id` | §3 (admin) |
| POST, GET | `/orgs` | §3 |
| GET, PATCH, DELETE | `/orgs/:id` | §3 |
| POST | `/orgs/:id/block`, `/orgs/:id/unblock` | §3 (admin) |
| GET | `/orgs/:id/members` | §3 |
| PUT, DELETE | `/orgs/:id/members/:user_id` | §3 |

### A.3 Hub routes (under `/api/v1`)

| Method | Path | Section |
|---|---|---|
| POST | `/workspaces` | §4 |
| GET | `/workspaces` **B** | §4 |
| GET, PATCH, DELETE | `/workspaces/:slug` | §4 |
| POST | `/workspaces/:slug/archive` | §4 |
| POST | `/workspaces/:slug/reactivate` | §4 |
| POST | `/workspaces/:slug/sync` | §4 |
| POST | `/workspaces/:slug/reclone` | §4 |
| POST | `/workspaces/:slug/branches/:name/push` | §4 |
| POST, GET | `/workspaces/:slug/tokens` | §3 |
| POST, GET | `/workspaces/:slug/merges` | §6 |
| GET, DELETE | `/workspaces/:slug/merges/:id` | §6 |
| POST | `/workspaces/:slug/merges/:id/requeue` | §6 |
| POST | `/workspaces/:slug/rebase` | §6 |
| POST, GET | `/workspaces/:slug/patches` | §7 |
| GET, PATCH, DELETE | `/workspaces/:slug/patches/:id` | §7 |
| POST | `/workspaces/:slug/patches/reorder` | §7 |
| POST | `/workspaces/:slug/patches/:id/restore` | §7 |
| POST | `/workspaces/:slug/patches/:id/reset-to-origin` | §7 |
| POST | `/workspaces/:slug/rebuild` | §7 |
| GET | `/workspaces/:slug/rebuilds` | §7 |
| GET, DELETE | `/workspaces/:slug/rebuilds/:id` | §7 |
| POST | `/workspaces/:slug/rebuilds/:id/requeue` | §7 |
| POST | `/workspaces/:slug/rebuilds/:id/rollback` | §7 |
| GET | `/workspaces/:slug/rebuild-preview` | §7 |
| GET | `/workspaces/:slug/rerere` | §7 |
| DELETE | `/workspaces/:slug/rerere/*` | §7 |
| GET | `/workspaces/:slug/patch-status` | §7 |
| POST, GET | `/user/secrets`, `/orgs/:slug/secrets`, `/workspaces/:slug/secrets` | §8 |
| PATCH, DELETE | `/user/secrets/:key`, `/orgs/:slug/secrets/:key`, `/workspaces/:slug/secrets/:key` | §8 |
| POST, GET | `/user/vars`, `/orgs/:slug/vars`, `/workspaces/:slug/vars` | §8 |
| PATCH, DELETE | `/user/vars/:key`, `/orgs/:slug/vars/:key`, `/workspaces/:slug/vars/:key` | §8 |
| GET | `/workspaces/:slug/vars/resolved` (registered before `/:key`, so `resolved` is never a key) | §8 |
| GET | `/workspaces/:slug/forge` | §9 |
| GET | `/workspaces/:slug/forge/repository` | §9 |
| GET | `/workspaces/:slug/issues` | §9 |
| GET | `/workspaces/:slug/issues/:n` | §9 |
| GET | `/workspaces/:slug/issues/:n/comments` | §9 |
| GET | `/workspaces/:slug/pulls/:n`, `…/state`, `…/files`, `…/checks`, `…/reviews` | §9 |
| POST | `/workspaces/:slug/issues` | §9 |
| PATCH | `/workspaces/:slug/issues/:n` | §9 |
| POST | `/workspaces/:slug/issues/:n/close` | §9 |
| POST | `/workspaces/:slug/issues/:n/comments` | §9 |
| POST | `/workspaces/:slug/issues/:n/labels` | §9 |
| DELETE | `/workspaces/:slug/issues/:n/labels/:name` | §9 |
| POST | `/workspaces/:slug/labels` | §9 |
| POST | `/workspaces/:slug/pulls` | §9 |
| POST | `/workspaces/:slug/pulls/:n/comments` | §9 |
| GET | `/workspaces/:slug/forge/deliveries` | §9 |
| PUT, DELETE | `/workspaces/:slug/forge/webhook` | §9 |
| GET | `/sandbox-profiles` **B** | §10 |
| POST, GET | `/workspaces/:slug/sandboxes` | §10 |
| GET, DELETE | `/workspaces/:slug/sandboxes/:id` | §10 |
| GET | `/sandboxes` (admin) | §10 |
| POST | `/sessions` **B** | §11 |
| GET | `/sessions` **B** | §11 |
| GET | `/sessions/:id` **B** | §11 |
| POST | `/sessions/:id/complete` **B** | §11 |
| POST, GET | `/sessions/:id/usage` **B** | §11 |
| GET | `/workspaces/:slug/cost` | §11 |
| POST, GET | `/workspaces/:slug/runs/:run_id/events` | §11 |
| POST | `/workspaces/:slug/runs/:run_id/events/batch` | §11 |
| POST, GET | `/workspaces/:slug/runs/:run_id/sessions/outcomes` | §11 |
| POST, GET | `/workspaces/:slug/runs/:run_id/tools/calls` | §11 |
| POST, GET | `/workspaces/:slug/runs/:run_id/tools/errors` | §11 |
| POST, GET | `/workspaces/:slug/runs/:run_id/traces` | §11 |
| POST | `/workspaces/:slug/runs/:run_id/traces/batch` | §11 |
| POST, GET | `/workspaces/:slug/runs/:run_id/postmortem` | §11 |
| GET | `/workspaces/:slug/runs/:run_id/transcript` | §11 |
| GET | `/audit` **B** | §11 |
| GET | `/events` (SSE) **B** | §11 |

### A.4 Sandbox gateway (separate listener, gRPC)

See §10. Calls authenticate with a lease token. The gRPC service is the AgentKit `gatewayd` sandbox API, wrapped by the hub's lease authenticator.


---

## Appendix B: `parley` command reference

This reference was generated from the `--help` output of an `parley` built from the baseline code (`CGO_ENABLED=0 go build ./cmd/parley`). The built binary has the build-time defaults, so the version strings are not the release ones. Every leaf command and every flag is listed. A re-implemented `parley` MUST accept the same commands, arguments and flags, with the same defaults. `parley credential-helper` is hidden from the root help but available. The root help text still reads "akc is the CLI client for the apikit API server." (apikit's default text).

**Global flags** (on every command): `--api-key string` (API key), `--endpoint-url string` (server endpoint URL), `--json` (output in JSON format, for help commands), `--user-id string` (authenticated user UUID), `-h, --help`.

#### `parley admin keys list`
List all API keys for a user identified by UUID, username, or email address.

- Usage: `parley admin keys list <user_id|username|email> [flags]`

#### `parley admin keys revoke`
Revoke an API key for a user identified by UUID, username, or email address.

- Usage: `parley admin keys revoke <user_id|username|email> <key_id> [flags]`

#### `parley admin orgs block`
Block an organization identified by UUID or slug.

- Usage: `parley admin orgs block <id|slug> [flags]`

#### `parley admin orgs create`
Create a new organization

- Usage: `parley admin orgs create [flags]`

| Flag | Type | Description |
|---|---|---|
| `--name` | string | Organization name |
| `--slug` | string | Organization slug |
| `--url` | string | Organization URL |

#### `parley admin orgs delete`
Delete an organization identified by UUID or slug.

- Usage: `parley admin orgs delete <id|slug> [flags]`

#### `parley admin orgs list`
List all organizations

- Usage: `parley admin orgs list [flags]`

| Flag | Type | Description |
|---|---|---|
| `--include-blocked` | bool | Include blocked organizations in the response |

#### `parley admin orgs members add`
Add a user to an organization. The organization can be specified by UUID or slug. The user can be specified by UUID, username, or email address.

- Usage: `parley admin orgs members add <org_id|slug> <user_id|username|email> [flags]`

#### `parley admin orgs members list`
List all members of an organization identified by UUID or slug.

- Usage: `parley admin orgs members list <id|slug> [flags]`

#### `parley admin orgs members remove`
Remove a user from an organization. The organization can be specified by UUID or slug. The user can be specified by UUID, username, or email address.

- Usage: `parley admin orgs members remove <org_id|slug> <user_id|username|email> [flags]`

#### `parley admin orgs unblock`
Unblock an organization identified by UUID or slug.

- Usage: `parley admin orgs unblock <id|slug> [flags]`

#### `parley admin orgs update`
Update an organization identified by UUID or slug.

- Usage: `parley admin orgs update <id|slug> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--name` | string | Organization name |
| `--url` | string | Organization URL |

#### `parley admin tokens list`
List all personal access tokens for a user identified by UUID, username, or email address.

- Usage: `parley admin tokens list <user_id|username|email> [flags]`

#### `parley admin tokens revoke`
Revoke a personal access token for a user identified by UUID, username, or email address.

- Usage: `parley admin tokens revoke <user_id|username|email> <token_id> [flags]`

#### `parley admin users block`
Block a user by UUID, username, or email address.

- Usage: `parley admin users block <id|username|email> [flags]`

#### `parley admin users create`
Create a new user

- Usage: `parley admin users create [flags]`

| Flag | Type | Description |
|---|---|---|
| `--email` | string | Email address for the new user |
| `--provider` | string | OAuth provider name |
| `--provider-id` | string | Provider-specific user ID |
| `--username` | string | Username for the new user |

#### `parley admin users demote`
Demote a user from admin by UUID, username, or email address.

- Usage: `parley admin users demote <id|username|email> [flags]`

#### `parley admin users list`
List all users

- Usage: `parley admin users list [flags]`

| Flag | Type | Description |
|---|---|---|
| `--include-blocked` | bool | Include blocked users in the response |

#### `parley admin users promote`
Promote a user to admin by UUID, username, or email address.

- Usage: `parley admin users promote <id|username|email> [flags]`

#### `parley admin users show`
Retrieve and display a user by UUID, username, or email address.

- Usage: `parley admin users show <id|username|email> [flags]`

#### `parley admin users unblock`
Unblock a user by UUID, username, or email address.

- Usage: `parley admin users unblock <id|username|email> [flags]`

#### `parley admin users update`
Update a user identified by UUID, username, or email address.

- Usage: `parley admin users update <id|username|email> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--full-name` | string | Full name of the user |

#### `parley forge deliveries`
List the deliveries the hub received from a workspace's forge, newest first

- Usage: `parley forge deliveries <workspace-slug> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--limit` | string | Number of deliveries, 1 to 100 (default: the hub's) |
| `--since` | string | Only deliveries received at this RFC 3339 time or later (default: the hub's) |

#### `parley forge status`
Show how the hub reaches the forge of a workspace

- Usage: `parley forge status <workspace-slug> [flags]`

#### `parley forge webhook set`
Set the webhook secret of a workspace's forge and show the URL for the forge's settings

- Usage: `parley forge webhook set <workspace-slug> [--secret s | --from-stdin | --generate] [flags]`

| Flag | Type | Description |
|---|---|---|
| `--from-stdin` | bool | Read the secret from stdin; one trailing newline is dropped |
| `--generate` | bool | Let the hub make up a secret (the default) |
| `--secret` | string | The secret to set, 16 to 255 printable ASCII characters (it ends up in the shell history: prefer --from-stdin) |

#### `parley forge webhook unset`
Remove the webhook secret of a workspace's forge, so that the hub refuses its deliveries

- Usage: `parley forge webhook unset <workspace-slug> [flags]`

#### `parley issue close`
Close an issue on a workspace's forge, with a comment if given

- Usage: `parley issue close <workspace-slug> <number> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--comment` | string | Comment to post on the issue before it is closed |
| `--target` | string | Remote to write to: origin (default) or upstream (carry_patch workspaces only) |

#### `parley issue comment`
Comment on an issue on a workspace's forge

- Usage: `parley issue comment <workspace-slug> <number> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--body` | string | Text of the comment |
| `--body-file` | string | Read the text from this file, or from stdin for -; it is sent as it is, with no trailing newline removed |
| `--target` | string | Remote to write to: origin (default) or upstream (carry_patch workspaces only) |

#### `parley issue create`
Create an issue on a workspace's forge

- Usage: `parley issue create <workspace-slug> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--assignee` | stringArray | User to assign the issue to (repeatable) |
| `--body` | string | Body of the issue |
| `--body-file` | string | Read the text from this file, or from stdin for -; it is sent as it is, with no trailing newline removed |
| `--label` | stringArray | Label to put on the issue (repeatable) |
| `--target` | string | Remote to write to: origin (default) or upstream (carry_patch workspaces only) |
| `--title` | string | Title of the issue (required) |

#### `parley issue get`
Show an issue with its comments

- Usage: `parley issue get <workspace-slug> <number> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--target` | string | Remote to read: origin (default) or upstream (carry_patch workspaces only) |

#### `parley issue label`
Add labels to an issue on a workspace's forge

- Usage: `parley issue label <workspace-slug> <number> <label>... [flags]`

| Flag | Type | Description |
|---|---|---|
| `--target` | string | Remote to write to: origin (default) or upstream (carry_patch workspaces only) |

#### `parley issue list`
List the issues of a workspace's repository

- Usage: `parley issue list <workspace-slug> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--assignee` | string | Only issues assigned to this user |
| `--label` | stringArray | Only issues with this label (repeatable) |
| `--limit` | string | Number of issues, 1 to 100 (default: the hub's) |
| `--state` | string | Issue state: open, closed or all (default: the hub's) |
| `--target` | string | Remote to read: origin (default) or upstream (carry_patch workspaces only) |

#### `parley issue unlabel`
Remove a label from an issue on a workspace's forge

- Usage: `parley issue unlabel <workspace-slug> <number> <label> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--target` | string | Remote to write to: origin (default) or upstream (carry_patch workspaces only) |

#### `parley issue update`
Change an issue on a workspace's forge

- Usage: `parley issue update <workspace-slug> <number> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--assignee` | stringArray | User to assign (repeatable); the users given replace the issue's whole set |
| `--body` | string | New body; an empty --body clears it |
| `--body-file` | string | Read the text from this file, or from stdin for -; it is sent as it is, with no trailing newline removed |
| `--label` | stringArray | Label to set (repeatable); the labels given replace the issue's whole set |
| `--state` | string | New state: open or closed |
| `--target` | string | Remote to write to: origin (default) or upstream (carry_patch workspaces only) |
| `--title` | string | New title |

#### `parley keys list`
List all API keys associated with the authenticated user.

- Usage: `parley keys list [flags]`

#### `parley keys refresh`
Rotate the current API key — generates a new secret while keeping the same key_id. The new key is automatically saved to the config file.

- Usage: `parley keys refresh [flags]`

#### `parley keys revoke`
Invalidate the current API key immediately and clear it from the config file. Run 'akc login' afterwards to obtain a new key.

- Usage: `parley keys revoke [flags]`

#### `parley label create`
Make sure a label exists in a workspace's repository

- Usage: `parley label create <workspace-slug> <name> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--color` | string | Color as six hex digits, with or without a leading # (default: ededed) |
| `--description` | string | Description of the label |
| `--target` | string | Remote to write to: origin (default) or upstream (carry_patch workspaces only) |

#### `parley login`
Authenticate via browser-based OAuth or API key and persist credentials to the config file.

- Usage: `parley login [flags]`

| Flag | Type | Description |
|---|---|---|
| `--api-key` | string | API key for non-interactive authentication (use '-' to read from stdin) |
| `--expires` | int | Credential expiry in days (0, 30, 60, or 90) (default 90) |
| `--provider` | string | OAuth provider name (default "github") |

#### `parley merge cancel`
Cancel a merge job

- Usage: `parley merge cancel <workspace-slug> <merge-id> [flags]`

#### `parley merge list`
List merge jobs

- Usage: `parley merge list <workspace-slug> [flags]`

#### `parley merge requeue`
Requeue a dead-lettered merge job

- Usage: `parley merge requeue <workspace-slug> <merge-id> [flags]`

#### `parley merge status`
Get merge job status

- Usage: `parley merge status <workspace-slug> <merge-id> [flags]`

#### `parley merge submit`
Submit a merge job

- Usage: `parley merge submit <workspace-slug> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--poll-interval` | duration | Interval between status polls (e.g. 5s, 10s) (default 5s) |
| `--source` | string | Source branch (required) |
| `--target` | string | Target branch (required) |
| `--timeout` | duration | Maximum time to wait for completion (e.g. 60s, 5m) (default 5m0s) |
| `--wait` | bool | Block until the async operation reaches a terminal state |

#### `parley orgs list`
List all organizations the authenticated user belongs to.

- Usage: `parley orgs list [flags]`

#### `parley orgs members`
List all members of an organization identified by UUID or slug.

- Usage: `parley orgs members <id|slug> [flags]`

#### `parley orgs show`
Retrieve and display details for an organization identified by UUID or slug.

- Usage: `parley orgs show <id|slug> [flags]`

#### `parley patch add`
Add a patch branch to a carry-patch workspace.

- Usage: `parley patch add <workspace-slug> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--branch` | string | Branch name for the patch (required) |
| `--description` | string | Patch description (optional) |
| `--if-not-exists` | bool | Return existing patch instead of error if branch already registered (optional) |
| `--position` | int | Position in the patch list (optional) |
| `--skip-branch-check` | bool | Skip all branch resolution: no ref lookup, no fetch, no local branch creation and no workspace lock is taken (optional) |
| `--upstream-pr` | string | Upstream PR URL (optional) |

#### `parley patch list`
List patches for a workspace

- Usage: `parley patch list <workspace-slug> [flags]`

#### `parley patch remove`
Remove a patch from a workspace

- Usage: `parley patch remove <workspace-slug> <patch-id> [flags]`

#### `parley patch reorder`
Reorder patches for a workspace

- Usage: `parley patch reorder <workspace-slug> <patch-id-1> [patch-id-2] ... [flags]`

#### `parley patch reset-to-origin`
Reset a patch branch to the fork's current tip, regardless of

- Usage: `parley patch reset-to-origin <workspace-slug> <patch-id> [flags]`

#### `parley patch restore`
Restore a soft-deleted patch

- Usage: `parley patch restore <workspace-slug> <patch-id> [flags]`

#### `parley patch update`
Update a patch

- Usage: `parley patch update <workspace-slug> <patch-id> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--description` | string | Patch description |
| `--position` | int | New position |
| `--status` | string | Patch status (active, merged_upstream, conflict, disabled) |
| `--upstream-pr` | string | Upstream PR URL |

#### `parley pr checks`
List the CI checks of a pull request

- Usage: `parley pr checks <workspace-slug> <number> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--target` | string | Remote to read: origin (default) or upstream (carry_patch workspaces only) |

#### `parley pr comment`
Comment on a pull request on a workspace's forge

- Usage: `parley pr comment <workspace-slug> <number> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--body` | string | Text of the comment |
| `--body-file` | string | Read the text from this file, or from stdin for -; it is sent as it is, with no trailing newline removed |
| `--target` | string | Remote to write to: origin (default) or upstream (carry_patch workspaces only) |

#### `parley pr create`
Open a pull request on a workspace's forge

- Usage: `parley pr create <workspace-slug> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--base` | string | Branch to merge into (required) |
| `--body` | string | Body of the pull request |
| `--body-file` | string | Read the text from this file, or from stdin for -; it is sent as it is, with no trailing newline removed |
| `--draft` | bool | Open the pull request as a draft |
| `--head` | string | Branch with the changes; owner:branch for a fork (required) |
| `--target` | string | Remote to write to: origin (default) or upstream (carry_patch workspaces only) |
| `--title` | string | Title of the pull request (required) |

#### `parley pr files`
List the files a pull request changes

- Usage: `parley pr files <workspace-slug> <number> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--target` | string | Remote to read: origin (default) or upstream (carry_patch workspaces only) |

#### `parley pr get`
Show a pull request

- Usage: `parley pr get <workspace-slug> <number> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--target` | string | Remote to read: origin (default) or upstream (carry_patch workspaces only) |

#### `parley pr reviews`
List the reviews of a pull request

- Usage: `parley pr reviews <workspace-slug> <number> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--target` | string | Remote to read: origin (default) or upstream (carry_patch workspaces only) |

#### `parley pr state`
Show the state of a pull request

- Usage: `parley pr state <workspace-slug> <number> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--target` | string | Remote to read: origin (default) or upstream (carry_patch workspaces only) |

#### `parley rebase submit`
Submit a batch rebase

- Usage: `parley rebase submit <workspace-slug> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--branches` | string | Comma-separated list of branches to rebase (required) |
| `--target-ref` | string | Target ref to rebase onto (required) |

#### `parley rebuild cancel`
Cancel a rebuild job

- Usage: `parley rebuild cancel <workspace-slug> <rebuild-id> [flags]`

#### `parley rebuild list`
List rebuild jobs

- Usage: `parley rebuild list <workspace-slug> [flags]`

#### `parley rebuild preview`
Preview rebuild conflicts without executing

- Usage: `parley rebuild preview <workspace-slug> [flags]`

#### `parley rebuild requeue`
Requeue a dead-lettered rebuild job

- Usage: `parley rebuild requeue <workspace-slug> <rebuild-id> [flags]`

#### `parley rebuild rollback`
Roll back the integration branch to its previous state

- Usage: `parley rebuild rollback <workspace-slug> <rebuild-id> [flags]`

#### `parley rebuild status`
Get rebuild job status

- Usage: `parley rebuild status <workspace-slug> <rebuild-id> [flags]`

#### `parley rebuild submit`
Submit a rebuild job

- Usage: `parley rebuild submit <workspace-slug> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--fail-mode` | string | conflict handling mode (fail_fast\|continue) |
| `--poll-interval` | duration | Interval between status polls (e.g. 5s, 10s) (default 5s) |
| `--strategy` | string | rebuild strategy override (rebase\|merge) |
| `--timeout` | duration | Maximum time to wait for completion (e.g. 60s, 5m) (default 5m0s) |
| `--wait` | bool | Block until the async operation reaches a terminal state |

#### `parley rerere forget`
Forget a recorded rerere resolution

- Usage: `parley rerere forget <workspace-slug> <pathspec> [flags]`

#### `parley rerere list`
List recorded rerere resolutions

- Usage: `parley rerere list <workspace-slug> [flags]`

#### `parley sandbox create`
Issue a sandbox lease in a workspace

- Usage: `parley sandbox create <workspace-slug> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--label` | stringArray | Label as key=value (repeatable) |
| `--profile` | string | Profile name (default: the hub's default profile) |
| `--purpose` | string | What the sandbox is for (at most 64 characters) |
| `--token-file` | string | Write the lease token to this file (mode 0600) instead of printing it |
| `--ttl` | string | Lifetime of the lease, such as 30m (default: the profile's) |

#### `parley sandbox destroy`
End a sandbox lease and revoke its token

- Usage: `parley sandbox destroy <workspace-slug> <lease-id> [flags]`

#### `parley sandbox get`
Show one sandbox lease

- Usage: `parley sandbox get <workspace-slug> <lease-id> [flags]`

#### `parley sandbox list`
List the sandbox leases of a workspace

- Usage: `parley sandbox list <workspace-slug> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--all` | bool | Include leases that have ended |

#### `parley sandbox profiles`
List the sandbox profiles of the hub

- Usage: `parley sandbox profiles [flags]`

#### `parley secrets create`
Create secrets

- Usage: `parley secrets create <KEY=VALUE[,KEY2=VALUE2,...]> [KEY3=VALUE3 ...] | create <KEY> --from-stdin [flags]`

| Flag | Type | Description |
|---|---|---|
| `--from-stdin` | bool | Read the value of the single KEY argument from stdin |
| `--org` | string | Target organization scope (by slug) |
| `--user` | bool | Target user scope |
| `--workspace` | string | Target workspace scope (by slug) |

#### `parley secrets delete`
Delete a secret

- Usage: `parley secrets delete <KEY> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--org` | string | Target organization scope (by slug) |
| `--user` | bool | Target user scope |
| `--workspace` | string | Target workspace scope (by slug) |

#### `parley secrets list`
List secrets

- Usage: `parley secrets list [flags]`

| Flag | Type | Description |
|---|---|---|
| `--org` | string | Target organization scope (by slug) |
| `--user` | bool | Target user scope |
| `--workspace` | string | Target workspace scope (by slug) |

#### `parley secrets update`
Update a secret

- Usage: `parley secrets update <KEY=VALUE> | update <KEY> --from-stdin [flags]`

| Flag | Type | Description |
|---|---|---|
| `--from-stdin` | bool | Read the new value of KEY from stdin |
| `--org` | string | Target organization scope (by slug) |
| `--user` | bool | Target user scope |
| `--workspace` | string | Target workspace scope (by slug) |

#### `parley tokens add`
Add one or more permissions to an existing PAT without replacing the current set.

- Usage: `parley tokens add <token_id> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--permissions` | string | Comma-separated permissions (e.g., orgs:read) |

#### `parley tokens create`
Create a new Personal Access Token with the specified name, permissions, and expiry.

- Usage: `parley tokens create [flags]`

| Flag | Type | Description |
|---|---|---|
| `--expires` | int | Token expiry in days (0, 30, 60, or 90) (default 90) |
| `--name` | string | Token name |
| `--permissions` | string | Comma-separated permissions (e.g., users:read,orgs:read) |
| `--workspace` | string | Bind the token to this workspace (POST /workspaces/<slug>/tokens) |

#### `parley tokens list`
List all Personal Access Tokens associated with the authenticated user.

- Usage: `parley tokens list [flags]`

| Flag | Type | Description |
|---|---|---|
| `--workspace` | string | List the tokens bound to this workspace (GET /workspaces/<slug>/tokens) |

#### `parley tokens remove`
Remove one or more permissions from an existing PAT without revoking it entirely.

- Usage: `parley tokens remove <token_id> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--permissions` | string | Comma-separated permissions (e.g., users:read) |

#### `parley tokens replace`
Replace the entire permissions set on a PAT with the specified permissions.

- Usage: `parley tokens replace <token_id> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--permissions` | string | Comma-separated permissions (e.g., users:read,orgs:read) |

#### `parley tokens revoke`
Invalidate a specific Personal Access Token immediately.

- Usage: `parley tokens revoke <token_id> [flags]`

#### `parley tokens show`
Retrieve and display metadata for a specific Personal Access Token.

- Usage: `parley tokens show <token_id> [flags]`

#### `parley user show`
Retrieve and display the authenticated user's profile information.

- Usage: `parley user show [flags]`

#### `parley user update`
Update the authenticated user's display name.

- Usage: `parley user update [flags]`

| Flag | Type | Description |
|---|---|---|
| `--full-name` | string | Full display name |

#### `parley vars create`
Create variables

- Usage: `parley vars create <KEY=VALUE[,KEY2=VALUE2,...]> [KEY3=VALUE3 ...] | create <KEY> --from-stdin [flags]`

| Flag | Type | Description |
|---|---|---|
| `--from-stdin` | bool | Read the value of the single KEY argument from stdin |
| `--org` | string | Target organization scope (by slug) |
| `--user` | bool | Target user scope |
| `--workspace` | string | Target workspace scope (by slug) |

#### `parley vars delete`
Delete a variable

- Usage: `parley vars delete <KEY> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--org` | string | Target organization scope (by slug) |
| `--user` | bool | Target user scope |
| `--workspace` | string | Target workspace scope (by slug) |

#### `parley vars list`
List variables

- Usage: `parley vars list [flags]`

| Flag | Type | Description |
|---|---|---|
| `--org` | string | Target organization scope (by slug) |
| `--user` | bool | Target user scope |
| `--workspace` | string | Target workspace scope (by slug) |

#### `parley vars resolve`
Resolve variables for a workspace

- Usage: `parley vars resolve <workspace-slug> [flags]`

#### `parley vars update`
Update a variable

- Usage: `parley vars update <KEY=VALUE> | update <KEY> --from-stdin [flags]`

| Flag | Type | Description |
|---|---|---|
| `--from-stdin` | bool | Read the new value of KEY from stdin |
| `--org` | string | Target organization scope (by slug) |
| `--user` | bool | Target user scope |
| `--workspace` | string | Target workspace scope (by slug) |

#### `parley version`
Show CLI and server version information

- Usage: `parley version [flags]`

#### `parley workspace archive`
Archive a workspace

- Usage: `parley workspace archive <slug> [flags]`

#### `parley workspace create`
Create a new workspace

- Usage: `parley workspace create [flags]`

| Flag | Type | Description |
|---|---|---|
| `--branch` | string | Git branch (optional) |
| `--description` | string | Workspace description (optional) |
| `--display-name` | string | Workspace display name (optional) |
| `--git-password` | string | Git password for private repo (optional) |
| `--git-pat` | string | Personal access token for private repo (optional) |
| `--git-url` | string | Git repository URL (required) |
| `--git-username` | string | Git username for private repo (optional) |
| `--integration-branch` | string | Integration branch name for carry_patch mode (default: 'deploy') |
| `--org` | string | Organization slug (optional) |
| `--poll-interval` | duration | Interval between status polls (e.g. 5s, 10s) (default 5s) |
| `--setting` | stringArray | Workspace setting as key=value (repeatable), e.g. patch_branch_source=origin |
| `--slug` | string | Workspace slug (required) |
| `--timeout` | duration | Maximum time to wait for completion (e.g. 60s, 5m) (default 5m0s) |
| `--upstream-url` | string | Upstream repo URL for carry_patch mode (required for carry_patch) |
| `--wait` | bool | Block until the async operation reaches a terminal state |
| `--workspace-mode` | string | Workspace mode: 'standard' (default) or 'carry_patch' (optional) |

#### `parley workspace delete`
Permanently delete an archived workspace

- Usage: `parley workspace delete <slug> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--confirm` | bool | Confirm permanent deletion |

#### `parley workspace get`
Get workspace details

- Usage: `parley workspace get <slug> [flags]`

#### `parley workspace list`
List workspaces

- Usage: `parley workspace list [flags]`

| Flag | Type | Description |
|---|---|---|
| `--include-archived` | bool | Include archived workspaces |

#### `parley workspace patch-status`
View carry-patch status dashboard

- Usage: `parley workspace patch-status <workspace-slug> [flags]`

#### `parley workspace push`
Push a branch of a workspace's trunk to its origin

- Usage: `parley workspace push <slug> <branch> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--force` | bool | Overwrite origin's branch even when it has commits the workspace lacks |

#### `parley workspace reactivate`
Reactivate an archived workspace

- Usage: `parley workspace reactivate <slug> [flags]`

#### `parley workspace reclone`
Archive and re-clone a workspace from upstream

- Usage: `parley workspace reclone <slug> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--confirm` | bool | Confirm reclone operation (required) |

#### `parley workspace sync`
Trigger upstream sync for a workspace

- Usage: `parley workspace sync <slug> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--fail-on-diverged` | bool | Exit with code 3 when patches_diverged is not empty |
| `--poll-interval` | duration | Interval between status polls (e.g. 5s, 10s) (default 5s) |
| `--reset-to-upstream` | bool | Force-reset the local integration branch to match upstream HEAD (recovery after force-push) |
| `--timeout` | duration | Maximum time to wait for completion (e.g. 60s, 5m) (default 5m0s) |
| `--wait` | bool | Block until the async operation reaches a terminal state |

#### `parley workspace update`
Update workspace properties

- Usage: `parley workspace update <slug> [flags]`

| Flag | Type | Description |
|---|---|---|
| `--clear-description` | bool | Clear description to default (empty) |
| `--clear-display-name` | bool | Clear display name to default (slug) |
| `--clear-org` | bool | Remove organization association |
| `--description` | string | Set workspace description |
| `--display-name` | string | Set workspace display name |
| `--org` | string | Set organization (by slug) |
| `--setting` | stringArray | Workspace setting as key=value (repeatable); key= alone resets the setting to its default |

#### `parley credential-helper`
Git credential helper for hub authentication

- Usage: `parley credential-helper <get|store|erase> [flags]`


---

## Appendix C: Summary of documentation discrepancies

The full, evidenced lists are in each section's subsection *N.11*. The items below are the ones that most affect a re-implementation. For unmarked items **the code behaviour is the requirement**. Items marked ✅ were defects in the baseline. **This PRD fixes them** (see §1.8), and the description after the marker is the old baseline behaviour.

### C.1 Platform, identity and access (§3.11)

- `/readyz` fails with `503 {"status":"not ready"}`, not with the error envelope.
- `/version`'s `go_version` holds the release version, not the Go version.
- `redirect_uri` is required on `POST /auth/callback`.
- `DELETE /user/keys/:key_id` answers `200 {"key_id","revoked_at"}`, not 204.
- `PUT /orgs/:id/members/:user_id` answers 204 with no body.
- `keys:read` is enforced for PATs.
- Org members (not only admins) may call `GET /orgs/:id` and `GET /orgs/:id/members`.
- 401 messages are specific (`missing authorization header`, `invalid credentials`, `credential expired`, …).
- No route ever answers 405. A wrong method ends in 404, or in 403 `token_bound` for a bound token.
- Identity routes refuse any header that carries a bound token's id with 403 `token_bound`, without checking the secret.
- `parley` drops `error_type` from its error output and hard-codes `/api/v1`.
- ✅ **FIX-01** (fixed) `parley orgs list` calls the admin-only `GET /orgs`, so non-admins get 403.
- ✅ **FIX-02** (fixed) `parley keys refresh` and `parley keys revoke` save the config to `./config.toml` in the working directory, not to `~/.af`.

### C.2 Workspaces (§4.11)

- ✅ **FIX-03** (fixed) Delete removes the directory and closes sessions before the database transaction. A 500 can therefore leave a partially deleted workspace, although the docs say "nothing was deleted".
- ✅ **FIX-04** (fixed) A clone job that finds the directory already present marks the workspace `ready` with no `head_sha`. A standard sync of such a workspace then always answers `up_to_date`.
- PATCH checks the *caller's* membership of a new org, not the owner's, so an admin token cannot move a workspace into an org.
- PATCH validates the body before looking up the slug, so an unknown slug can get 400 before 404.
- Local commits ahead of `origin` are reported as diverged (409).
- ✅ **FIX-05** (fixed) Clone jobs live only in memory. After a restart, `pending`/`cloning` workspaces stay stuck, and archive and reclone both refuse a stuck `cloning` workspace.
- Branch push uses a plain force, with no lease.
- ✅ **FIX-06** (fixed) The CLI's 30 s timeout is shorter than the server's 2 min push deadline.

### C.3 Git server (§5.11)

- `:org` in `/git/:org/:slug.git` is the slug of the workspace's *organization*.
- A URL without `.git` gets 404 only after authentication.
- A missing PAT scope gets a JSON 403, not git's text format.
- A workspace that is not ready answers "repository not available" to any caller, which leaks its existence.
- Pushes get no fast-forward check: force pushes and deletes are accepted, including deleting the checked-out branch.
- Only protocol v0 is supported.
- The worktree reset after a push is skipped while another operation holds the workspace lock.
- In `parley credential-helper`, environment variables override the config file field by field.

### C.4 Merge queue (§6.11)

- The `BranchNotReady` pre-check rejection is retried, up to 20 times, not treated as permanent.
- `hub.merge.fail` is emitted on every failed attempt.
- No `hub.merge.complete` is emitted for a source that is already merged.
- Merge events carry neither the job id nor an action.
- Source and target accept any revision that `git rev-parse` resolves.
- ✅ **FIX-07** (fixed) `parley merge submit --wait` treats `failed` (which means "waiting to retry") as final and exits 1.
- ✅ **FIX-08** (fixed) A job that is waiting to retry does not count as a duplicate, so the same merge can be queued twice. The same holds for rebuilds (see C.5).
- Merges change only the hub's clone: nothing is pushed and `head_sha` is not updated.
- Merge jobs are never purged.

### C.5 Carry-patch (§7.11)

- ✅ **FIX-08** (fixed) A rebuild that is waiting to retry did not count as a duplicate, so a second rebuild of the same workspace could be queued.
- ✅ **FIX-09** (fixed) `parley patch remove` deletes the row and cannot be undone with `parley patch restore`. Restore only works on patches that a rebuild soft-deleted as merged upstream.
- The rebuild job record has no `upstream_head_sha` and no `integration_branch_pushed`.
- `completed_at` is set for `failed` (retry pending) and is null for `cancelled`.
- `hub.rebuild.followup` uses resource type `patch` and the metadata `stale_patches`, `upstream_stale` and `follow_up_job_id`.
- The rerere list is in directory-name order. A rerere id can be any rr-cache directory name.
- ✅ **FIX-10** (fixed) `conflict_files` is never cleared, so a `conflict` patch never returns to `active` by itself.
- Validation order differs from the docs:
  - single add: `position < 1` is checked after the branch lookup and the duplicate check.
  - batch add: duplicates inside the batch are checked after the branch lookups.
- The post-push hook matches patch rows of any status and does not skip the integration branch.
- ✅ **FIX-11** (fixed) The git server does not block pushes to the integration branch. Only the API branch push refuses them.
- ✅ **FIX-12** (fixed) Soft-deleted patches are never purged: the 7-day purge exists but nothing calls it.

### C.6 Secrets and variables (§8.11)

- Values are base64-encoded, not encrypted. The docs agree.
- With an admin token, resolved variables include the org tier even when the owner has left the org.
- Secret and variable audit events carry no workspace and no resource id, so only admin tokens see them in `/audit` and `/events`.
- ✅ **FIX-13** (fixed) When one scope fails, `parley secrets create` and `parley vars create` with several scope flags print an extra `{"error":{"code":1,"message":""}}`.

### C.7 Forge (§9.11)

- In `GET …/issues/:n`, `comments_err` is a real error object only for a plain forge HTTP error. Otherwise it is `{}`.
- ✅ **FIX-14** (fixed) A write without a workspace credential reaches the forge (with no auth header) when the hub process has `GITHUB_TOKEN`, `GH_TOKEN` or `GITLAB_TOKEN` set. The result is 502 `forge_unauthenticated` with an event.
- GitLab label removal is retried once on a rate limit.
- An unparsable remote combined with an invalid API URL setting gives `forge_unknown` on reads and writes, but `forge_api_url_invalid` on `GET …/forge`.
- The CLI normalises issue numbers such as `+3` or `007` for writes, while the hub rejects `+3` on reads.
- `GET …/forge/repository` and `GET …/issues/:n/comments` have no CLI command.

### C.8 Sandboxes (§10.11)

- Ended leases are deleted only by the sweep, so they stay visible for up to 24 h plus one sweep interval.
- Create errors from the gateway carry the prefix `gatewayd: backend create: `.
- POST can answer 413 `request body is too large`.
- `parley` never prints `error_type: sandbox_provider_error`.
- Boxes are destroyed at shutdown, and expiry destroys the box first. ADR 03 says otherwise and is out of date.
- Only `XDG_CONFIG_HOME` and `XDG_DATA_HOME` are scrubbed from the sandbox engine's environment.

### C.9 Audit, sessions and events (§11.11)

- Per-run lists default to oldest first, with limit 100 (max 1000) and an inclusive `until`.
- The traces list has no `session_id` filter.
- Transcript timestamps are in DuckDB text form (`2026-09-01 08:00:00.5+00`), not RFC 3339.
- ✅ **FIX-15** (fixed) Replaying a usage report with an existing id answers 201 and increments the token counters again.
- Session and usage ids are not validated as UUIDs.
- Primary keys are global, including the postmortem `run_id`, so reusing an id elsewhere is silently a duplicate.
- Admin tokens can ingest into workspaces that do not exist.
- The SSE stream carries hub events only, with no replay.
- The SSE 503 is `{"error":"too many SSE connections"}`, not the envelope.
- `afhub_jobqueue_depth` is never exported.
- There is no pricing: "cost" means token counts.
- Retention variables delete more than the configuration doc says. For example, `AF_SESSION_MAX_AGE_DAYS` deletes sessions in every terminal state.
- The documented validation of outcome `duration_ms` and `blocked_tasks` is not done.
