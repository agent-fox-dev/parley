[Parley PRD](00_overview.md) · §3

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

`parleyd` needs a `git` binary on its `PATH`. The minimum version is stated in §15 (NFR-004).

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
| `workspaces` | `workspaces:read`, `workspaces:write` | §4; replacing the policy (§8); remembered resolutions and the patch-status summary (§10); event webhooks (§14) |
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
| `admin` | the admin token | everything that does not need a user: it cannot create workspaces, forks or tokens (§4, PLAT-FR-082), use `/user` routes or user-scope secrets and variables (§11), or record a verdict (§8) |
| `owner` | the user who owns the workspace; for a task fork, also the owner of its parent | everything |
| `member` | a member of the workspace's org, while the org is `active` | read; create task forks; open changes from an own fork and edit, land and revert them where §7 and §8 allow the author to; record verdicts (§8); make claims (§9) |
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
- **PLAT-FR-002 (start).** The server loads and validates its configuration (§3.9), opens its data, runs the bootstrap (PLAT-FR-003), resumes durable work, starts the maintenance loop (PLAT-FR-005) and then listens on `[server].bind:[server].port`. Any failure before listening stops the process with exit status 1 and one error line on stderr. Durable work that is resumed at start includes clones left `pending` or `cloning` (§4) , pending restacks (§7), running land attempts (§8), rebuild jobs (§10) and sandbox provisioning (§13). When sandboxes are enabled, the start also checks the sandbox runtime once and stops on a failure (SBX-FR-003). When it starts listening, the server logs the address, the configuration path and the data directory.
- **PLAT-FR-003 (bootstrap).** The hub is **bootstrapped** once an admin token hash is stored. The token file is `<config dir>/admin_token`, where `<config dir>` is the directory of `config.toml` (§3.9).

  | Situation | Behaviour |
  |---|---|
  | not bootstrapped, no flags | start normally, without an admin token |
  | not bootstrapped, `--admin-email` | store the e-mail, generate the token, write the file, exit |
  | `--reset-admin-token` (bootstrapped or not) | generate a new token, replace the stored hash, write the file (overwriting), exit |
  | bootstrapped, token file exists | refuse to start: exit 1, naming the file |
  | bootstrapped, no token file | start normally; `--admin-email` is ignored |

  The token file has mode 0600 and holds the token with no trailing newline. After writing it, the server logs the file's absolute path at warn level, tells the operator to save the token and delete the file, and exits with status 0 without serving. The token itself is never logged.
- **PLAT-FR-004 (shutdown).** On SIGTERM or SIGINT the server stops accepting connections and gives in-flight requests 15 seconds to finish. Event streams are closed. Background work stops within the grace its section sets (running rebuild jobs: 30 s, §10). Sandboxes are not touched: a shutdown ends the open exec, attach, log and transfer requests and nothing else (SBX-FR-063). The process exits when requests and background work have ended or their grace has passed, at most 30 s after the signal. Anything durable resumes at the next start. A clean shutdown exits 0. A failure to bind the listener exits 1.
- **PLAT-FR-005 (maintenance loop).** The loop runs once right after start and then every hour. A pass runs these tasks in order:

  | Task | Defined in |
  |---|---|
  | retention: sessions, run telemetry, hub events, the event log and the delivery log of event webhooks (one task, steps of §14.7.2) | §14 |
  | secret re-encryption under the current key | §11 |
  | claim cleanup | §9 |
  | forge delivery pruning | §12 |
  | token binding sweep | PLAT-FR-086 |
  | patch purge | §10 |
  | fork expiry | §4 |
  | sandbox record cleanup | §13 |

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
  - An error may carry extra members inside `error` when a section says so: a named member (for example `oldest_seq` with `cursor_expired`) or a `details` object (for example the conflict report with `rebase_conflict`, the land attempt with `land_conflict` and `policy_blocked`, `current_sha` with `ref_moved`).
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

  Specific types that several sections share, defined where noted: `workspace_busy` (409, §4), `workspace_not_ready` (409, §4), `fork_has_no_origin` (400, §4), `unknown_session` (400, §14), `ref_moved` (409, §6), `land_conflict` (409, §8), `policy_blocked` (409, §8), `cursor_expired` (410, §14).
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

  Consequences: a PAT without the scope gets 403 whether or not the workspace exists, so the answer leaks nothing. A member's write to an archived workspace is 403, not 409. Steps 1–4 on public routes skip authentication. One section inserts a step of its own: the sandbox feature check runs after step 4 and before step 5 (SBX-FR-010).
- **PLAT-FR-014 (lists).** Every list endpoint answers

  ```json
  {"items":[…],"next_cursor":"<opaque string>"}
  ```

  - `items` is always an array, never null. `next_cursor` is null on the last page.
  - `limit` defaults to 50 and a value outside 1–200 is `400 invalid_request`, unless a route states other bounds (the tree listing of §6).
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
  - Request and response bodies on the REST API are JSON, unless a route states another content type: raw file content and patch text (§6), a conflicted file as text (§8) and the event stream (§14).
  - A request body larger than `[server].max_body_size` (default 1 MiB) is `413 payload_too_large`, whether or not it declares its length. Git routes, the webhook ingress and routes that carry file content set their own limits (§5, §6, §12).
  - A `POST`, `PUT` or `PATCH` with a body whose `Content-Type` is not `application/json` is `415 unsupported_media_type`. A request without a body is exempt.
  - A body that is not valid JSON, or not an object where one is expected, is `400 invalid_request`. Unknown members are ignored.
- **PLAT-FR-018 (request id and headers).**
  - An incoming `X-Request-ID` that is a valid UUID is reused; otherwise the hub generates one. Every response carries `X-Request-ID`, and every access-log line carries it.
  - Every response carries `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY` and `Referrer-Policy: no-referrer`.
  - API responses carry `Cache-Control: no-store`. Exceptions: `GET /auth/providers` and `GET /version` carry `public, max-age=300`; `/healthz` and `/readyz` carry `no-cache`; source control reads set their own caching headers (§6, SCM-FR-002); the event stream carries `no-cache` (§14).
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
  1. Resolve the caller's level (§3.3.6): `admin` for the admin token; `owner` for the workspace's owner and, for a task fork, the owner of its parent; `member` for a member of the workspace's active org; otherwise none. A task fork never grants more than its parent: a caller whose level on the parent is none has level none on the fork too, even as the fork's creator.
  2. Level none, or no such workspace: `404 not_found` with the message `workspace not found`.
  3. Level `member` on a `write` or `owner` route: `403 forbidden` with the message `org members have read-only access to this workspace`.
  4. Otherwise access is granted.

  A failure while deciding is `500 internal`, never a grant.
- **PLAT-FR-061 (anti-enumeration).** A caller with no read access gets exactly the same response for an existing workspace as for a nonexistent slug: same status, same body. This holds on every surface, including git, and in every state of the workspace. A member's 403 is not a leak, because the member can already read the workspace.
- **PLAT-FR-062 (member contributions).** These routes are open to members although they change state. The owning sections declare them with need `read` plus the stated condition:
  - creating a task fork of a workspace the member can read (§4). The member owns the fork;
  - opening a change from a fork the member owns into the fork's parent, and editing, restacking, abandoning or restoring that change (§7);
  - landing and reverting a change the member authored, while the workspace setting `land.by_author` is true, always under the parent's policy and never with an override (§8);
  - recording a verdict on a change (§8); whether it counts is decided by policy;
  - creating, renewing and releasing the member's own claims (§9).

  Nothing else in the parent is writable by a member. In particular, reporting a check needs write access (§8).
- **PLAT-FR-063 (readable set).** Cross-workspace reads (the workspace list, events, audit) use one set: all workspaces for the admin token; otherwise the workspaces the user owns, the forks of those workspaces, and the workspaces of the user's active orgs. Ownership wins over membership when both apply. For a bound PAT the set is cut down to the workspaces its binding covers (PLAT-FR-081). For events and audit, a token bound to a task fork also sees the events of the fork's parent whose resource is a change with that fork as its source (§14).

#### Workspace-bound tokens

- **PLAT-FR-080 (binding).** A PAT may be bound to exactly one workspace. The binding is set at creation and never changes. A bound token is an ordinary PAT of the minting user, with that user's access, restricted further by the binding. There are two ways for a caller to mint one, and one way for the hub:
  - `POST /workspaces/:slug/tokens` binds a new token to that workspace;
  - the fork-create call can mint a token bound to the new fork (§4);
  - the hub itself mints the hub credential of a sandbox: a token bound to the sandbox's workspace (§13.3.4). It is owned by the user who created the sandbox and carries `git:read`, `git:write`, `changes:read`, `changes:write`, `sessions:write` and `audit:write`, each only if the creating credential holds it. It inherits the `paths` limit of the creating credential, expires with the sandbox, is returned by no route, and emits `parley.token.bind`. One is minted when the sandbox is created and at each start; it is revoked when the sandbox is stopped or deleted.
- **PLAT-FR-081 (coverage).** A token bound to workspace W is valid on:
  - W;
  - every task fork of W that was created by a request authenticated with this token. The hub records the creating token on the fork.

  A token bound to a task fork F is valid on F. On F's parent P it is valid only for the routes a contributor needs: the change routes (§7) and the landing routes (§8) for changes whose source is F, the policy read routes (§8), and the awareness read routes (§9). CHG-FR-040, LAND-FR-003 and AWR-FR-001 list them. Every other route on P answers `403 token_bound`. Routes addressed with F's own slug that act on P's data (awareness and claims §9, forge §12, resolved variables §11) are routes of F and pass the guard; the access of the token's user on P still applies.
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
  | names the parent of the fork the token is bound to | the routes listed under PLAT-FR-081 pass to their own checks; every other route is `403 token_bound` |
  | names any other workspace, existing or not | `404 not_found`, `workspace not found` |
  | names no workspace and is on the allowlist | passes; the route confines its result to the covered workspaces |
  | anything else, including every identity route | `403 token_bound` |

  The allowlist is: `GET /workspaces`, `GET /events`, `GET /events/log`, `GET /audit`, `GET /sandbox-profiles`, and the session routes `POST /sessions`, `GET /sessions`, `GET /sessions/:id`, `POST /sessions/:id/complete`, `POST /sessions/:id/usage`, `GET /sessions/:id/usage`.

  The token is always authenticated first. A request that carries a bound token's id with a wrong secret gets 401, like any other bad credential.
- **PLAT-FR-084 (listing).** `GET /workspaces/:slug/tokens` lists the tokens bound to the workspace, including revoked and expired ones. Its need is `owner`. PAT callers need `tokens:read`. A bound PAT is refused (`403 token_bound`).
- **PLAT-FR-085 (workspace removal).** When a workspace is deleted, or a task fork expires, the hub revokes every live token bound to it in the same transaction as the removal. The revoked token ids appear in the removal's event metadata as `revoked_tokens` (§4). Archiving does not touch bound tokens: a token bound to an archived workspace still reaches it and gets the routes' usual archived answers.
- **PLAT-FR-086 (binding sweep).** Each maintenance pass revokes any live token whose bound workspace no longer exists. It is a backstop for PLAT-FR-085.
- **PLAT-FR-087 (refusal events).** Each request the binding guard refuses is reported as `parley.token.bound_refused` (§3.8), at most one event per token per minute. Refusals inside that minute are counted, and the count is carried as `suppressed` on the token's next event. Filtering a list is not a refusal. The limiter state is not durable. A failure to emit never changes the HTTP answer.

#### CLI global behaviour

- **PLAT-FR-090 (settings).** Every command except `login`, `version` and `help` needs a hub URL and a credential. Each takes the first non-empty value of flag, environment variable, config file:

  | Setting | Flag | Environment | File key |
  |---|---|---|---|
  | Hub URL | `--endpoint-url` | `PARLEY_ENDPOINT_URL` | `endpoint_url` |
  | Credential | `--api-key` | `PARLEY_API_KEY` | `api_key` |

  A missing value fails before any request, with exit status 2 and a message that names the flag, the variable and the file.
- **PLAT-FR-091 (requests).** Requests go to `<hub URL without trailing slash>/api/v1<path>` with `Authorization: Bearer <credential>`. Each request times out after 30 seconds unless a command states otherwise. Every user-supplied path segment is percent-encoded as one segment, so a `/` inside an argument becomes `%2F`.
- **PLAT-FR-092 (output).**
  - **stdout** carries the result and nothing else: one JSON document, indented by 2 spaces, followed by a newline. A command whose route answers `204` prints `{}`, unless its entry states a confirmation document (for example `{"status":"deleted","slug":"<slug>"}`). The commands that pass a sandbox's own output or files through (`parley sandbox exec`, `shell`, `logs` and `download`, §13.6) are the exception and say so in their entries.
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

- **PLAT-FR-098.** Text that leaves the hub in logs, events, audit records or error messages MUST NOT contain credentials. Before such text is emitted, known secret values (tokens, secrets, forge and sandbox runtime credentials) are replaced by `[redacted]`, user information is removed from URLs (`https://user:pass@host/…` becomes `https://host/…`), and the text is cut to at most 1024 bytes on a character boundary. If a secret cannot be removed cleanly, the whole text becomes `[redacted]`. The hub never logs an `Authorization` header or a request body that carries a secret.

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

  `version` is the release version, or `dev` for an untagged build. `go_version` is the toolchain that built the binary. When sandboxes are enabled the document also has `sandbox_runtime: {supported, connected}`: the OpenShell releases this hub supports and the release of the connected gateway (§13.9).

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
- **Side effects:** `parley.token.bind`.

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
| `parley.token.bind` | once per bound token minted, by `POST /workspaces/:slug/tokens`, by fork-create, or by the hub as the hub credential of a sandbox, at create and at each start (§13) | the minting user; `api_key` or `pat` | `token` / token id / `bind` / the bound slug | `token_id`, `workspace`, `permissions`, `paths` (or null), `expires_at` (or null). Never the token value. |
| `parley.token.bound_refused` | the binding guard refused a request, rate-limited (PLAT-FR-087) | the token's user; `pat` | `token` / token id / `bound_refused` / the bound slug | `token_id`, `bound_workspace`, `requested_workspace` (when the request named one), `method`, `route`, `status` (403 or 404), `surface` (`api` or `git`), `suppressed` (int) |

`parley.token.*` events are visible only to callers with level `owner` or `admin` on the bound workspace (§14.3.7). `parley.token.bound_refused` is visible to them even when the requested workspace is not. `requested_workspace` is the slug as the caller typed it and says nothing about whether it exists.

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
| `[forks]` | §4 | task fork TTLs and limits (`default_ttl`, `max_ttl`, `max_per_workspace`, `expire_on_land`) |
| `[limits]` (other keys) | the section that owns each limit | size and quantity limits |
| `[events]`, `[audit]`, `[sessions]` | §14 | event-log retention and stream limits, audit and session retention |
| `[webhooks]` | §14 | event webhooks: subscriptions per workspace, delivery timeout, destination rules (`max_per_workspace`, `timeout`, `allow_private`, `allow_insecure`) |
| `[git]` | §5 | git server timeouts and push size |
| `[scm]` | §6 | source control API size limits and read timeout |
| `[changes]` | §7 | mergeability debounce, stack depth |
| `[land]` | §8 | landing attempts, timeout, workers, committer identity |
| `[awareness]` | §9 | claim TTLs and limits |
| `[secrets]` | §11 | encryption key file |
| `[sandbox]`, `[[sandbox.profiles]]`, `[[sandbox.credentials]]` | §13 | the sandbox runtime, limits, profiles and credentials |

#### Server flags

`--admin-email <string>` and `--reset-admin-token` (PLAT-FR-001, PLAT-FR-003). The subcommand `parleyd backup <directory>` writes a consistent snapshot of the data directory while the hub runs (NFR-007); it exits 1 when the directory is not empty.

#### Environment variables

| Variable | Read by | Meaning |
|---|---|---|
| `XDG_CONFIG_HOME` | `parleyd` | directory of `config.toml` and `admin_token`; default: the working directory |
| `XDG_DATA_HOME` | `parleyd` | base directory for `database.path` and `workspace.path` |
| any `${NAME}` referenced in `config.toml` | `parleyd` | substituted into the configuration (for example OAuth client secrets) |
| `PATH` | `parleyd` | locates `git` |
| `PARLEY_SECRETS_KEY`, `PARLEY_SECRETS_KEY_PREVIOUS` | `parleyd` | secret encryption key and the former key during rotation (§11) |
| `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` | `parleyd` | standard proxy behaviour for outgoing calls (identity providers, origins, forges) |
| `PARLEY_CONFIG_DIR` | `parley` | CLI config directory; default `~/.parley` |
| `PARLEY_ENDPOINT_URL` | `parley` | hub URL |
| `PARLEY_API_KEY` | `parley` | credential |

Retention is configured only through the `[events]`, `[audit]` and `[sessions]` keys of the configuration file (§14); no environment variable sets it. The hub never reads forge tokens from its environment (§12).

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
  - It is not a user: `/user…` routes and the bound-token mint answer `403 forbidden`. So do workspace and fork creation (§4), user-scope secrets and variables (§11) and recording a verdict (§8).
  - It passes every scope check and has `admin` access on every workspace.
  - It may list bound tokens of any workspace.
- **OAuth.**
  - Every login revokes the user's other API keys, including one in use on another machine.
  - A username change at the provider that collides with another user keeps the old username silently.
- **Idempotent replays of token creation** return the token object without the `token` value. A client that lost the value must revoke the token and create a new one.
- **Rate limiting** counts refused requests too. The CLI does not retry on 429.
- **Readiness** reports only whether the hub can reach its own data. It does not check origins, forges, identity providers or the sandbox runtime.
