[Parley PRD](00_overview.md) · §11

## 11. Secrets and variables

### 11.1 Purpose and scope

The hub keeps named key/value pairs for three kinds of owner: a user, an organization and a workspace. **Secrets** are write-only. Their values never leave the hub through any API response, and they are encrypted at rest. The hub consumes them itself: as git and forge credentials, as the webhook secret, and as the credentials of sandboxes, where the workload never receives a value (§13). The one path on which a real value leaves the hub is opt-in: a secret that an operator names in a sandbox profile's `expose_secrets` is placed in the environment of that profile's sandboxes (§13). **Variables** follow the same rules and scopes, but their values are readable, and a workspace's effective variables can be read merged along a precedence chain.

In scope: scopes, validation, access rules, encryption and key rotation, resolution chains (including task forks), REST routes, events and the `parley secrets` / `parley vars` commands. Out of scope: how a secret becomes a credential of a sandbox or is exposed in one (§13), how git credentials are used (§4, §10) and the forge webhook (§12). Variables never change hub behaviour; behaviour switches are workspace settings (§4).

### 11.2 User stories

- **SEC-US-01** As a Developer, I want to store a git PAT for my workspace (`GIT_PAT`) without it ever being readable again, so that the hub can clone, push and call the forge for me.
- **SEC-US-02** As an Org member, I want to store secrets and variables at the organization level, so that every workspace of the org shares them.
- **SEC-US-03** As an Agent with a workspace-bound token and `vars:read`, I want the effective variables of my workspace or fork, each with its origin, so that I can configure myself.
- **SEC-US-04** As an Org member working in a task fork, I want my fork to run with the parent's variables, while the parent's secrets stay out of my reach: not readable, not listable, not usable as credentials in my fork's sandboxes and never exposed in them.
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
- **Secrets** resolve by name for the credentials of a sandbox (SBX-FR-024) and for the secrets a sandbox profile exposes (SBX-FR-065): the first scope in the chain that has the name wins. Values are read when the sandbox is created and again at each start. A value reaches a sandbox in one of two ways:
  - **As a credential**, attached through an OpenShell provider. This is the normal way. The hub hands the value to the runtime, which adds it outside the sandbox on requests to authorized endpoints. The workload sees a placeholder and never receives the value (§13.3.4).
  - **As an exposed secret**, only when the operator listed the secret's name in the profile's `expose_secrets` or `optional_expose_secrets`. The real value is then placed in the sandbox's environment under that name, and code in the sandbox can read it and send it wherever the network policy allows (§13.3.4). Nothing is exposed unless a profile lists it. Reserved names can never be exposed.

  The rules below hold for both ways. For secrets, step 2 applies only when the fork's owner has write access to the parent workspace (the parent's owner). The test is on the fork's owner, never on the caller: an admin token acts as the fork owner here. In a fork owned by a read-only org member, step 2 is skipped for secrets, so the parent's secrets never reach that fork's sandboxes. Reserved names are skipped in step 2 for everyone, so no fork's sandbox receives the parent's origin, upstream or webhook credentials.

Resolution is use, not read access. A fork owner who is only an org member of the parent gets the parent's variables in the fork's resolved set, and nothing of the parent's secrets: no value, no credential in a sandbox, no exposed secret in a sandbox, and no secret names (SEC-FR-006). No route returns a stored secret value to anyone. A secret exposed in a sandbox (§13) is the one case in which a value is handed to a workload, and it happens only by the operator's configuration. The only value the hub ever shows is a webhook secret it generated itself, once, in the response that creates it (§12).

### 11.4 Functional requirements

- **SEC-FR-001** Every route follows the global check order (§3): authenticate → binding guard (bound PATs) → PAT scope → scope access → request validation → state → act. PAT scopes are `secrets:read`, `secrets:write`, `vars:read`, `vars:write`; a read accepts either, a write only `write`. API keys and admin tokens are not scope-checked.
- **SEC-FR-002** A workspace-bound token may use only the `workspace` scope of a workspace it is valid on (its workspace and the forks it created, §3). On `/user/…` and `/orgs/…` routes it gets `403 token_bound`; on another workspace `404 workspace not found`.
- **SEC-FR-003** Secret values are returned by no route, event, log line or error message, except the webhook secret the hub generates, which `PUT …/forge/webhook` shows once (§12). Only credential resolution, webhook verification, the attachment of credentials to sandboxes and the exposure of a secret that a sandbox profile names in `expose_secrets` (§13) read them. The signing secret of an event webhook (§14) is not a secret of this section: it has no name and no scope. It is encrypted with the same key, and the hub shows it once, when it is created and when it is rotated.
- **SEC-FR-004** User scope: a credential without a user identity (admin token) gets `403 forbidden`, message `user-scoped endpoints require a user credential`.
- **SEC-FR-005** Org scope: an unknown org and a non-member both get `404 not_found` with the same body. Admin tokens bypass membership.
- **SEC-FR-006** Workspace scope uses the workspace access rule (§3):

  | Caller | Secrets (all operations) | Variables read (list, resolved) | Variables write |
  |---|---|---|---|
  | owner, admin token | allowed | allowed | allowed |
  | org member | `403 forbidden` | allowed | `403 forbidden` |
  | anyone else, or unknown slug | `404 workspace not found` | `404` | `404` |

  Secret routes have need `owner` (§3): a member's `403` on a secret route is the answer of PLAT-FR-060 and leaks nothing, because the member can already read the workspace. This holds on a parent workspace for the owner of one of its forks. The workspace's status (for example `archived`) does not matter; an exclusive maintenance operation does not block these routes.
- **SEC-FR-007** Create takes `{"entries":[{"key","value"}, …]}` and is all-or-nothing. A missing `value` is the empty string. Checks, in order: `entries` non-empty; each key valid; each value within the size limit; no two keys of the request equal ignoring case (all `400 invalid_request`, the message names the key); the scope's count plus the new entries within the limit (`429`, `error_type: secret_limit_reached`, `maximum of 100 entries per scope exceeded`); no key already present ignoring case (`409 conflict`, `key "<k>" already exists`). All entries get the same `created_at`.
- **SEC-FR-008** Update takes `{"value": string}`. An absent or `null` value is `400`; `""` is valid. The key is matched ignoring case; no match is `404 not_found`, `key "<k>" not found`. The stored key casing and `created_at` are kept; `updated_at` becomes now.
- **SEC-FR-009** Delete matches the key ignoring case. No match is `404 not_found`. Success is `204` with no body.
- **SEC-FR-010** Lists are ordered by key, ascending, case-insensitive, and follow the list convention (§3). Secret lists carry no value.
- **SEC-FR-011** Secrets are encrypted at rest with the operator's key. When no key is configured, every operation that would store a secret value is refused with `503`, `error_type: secrets_unavailable`: secret create and update here, and the routes elsewhere that store a secret on the caller's behalf (workspace creation with git credentials, §4; webhook secret setup, §12; creating an event webhook and rotating its signing secret, §14). Listing and deleting secrets, and all variable routes, keep working.
- **SEC-FR-012** Key rotation. The hub accepts a second key, `PARLEY_SECRETS_KEY_PREVIOUS`, for decryption only. New and updated values always use the current key. The hourly maintenance pass re-encrypts every value still under the previous key (§11.7).
- **SEC-FR-013** A stored secret that cannot be decrypted (no key, or a key that matches neither configured key) stays listed, can be overwritten by an update or deleted, and fails any operation that needs its value with a clear error naming the key but not the value.
- **SEC-FR-014** Deleting a workspace deletes its secrets and variables in the same transaction; this includes expiry of a task fork. Deleting an org or a user deletes that scope's entries. A workspace re-created under the same slug starts empty.
- **SEC-FR-015** `GET /workspaces/:slug/vars/resolved` applies the resolution chain and needs only variable read access on `:slug`. A fork's resolved set includes the parent's variables with `origin: "parent"`.
- **SEC-FR-016** Each created entry, each update and each delete emits one event (§11.8). A failed request emits none.
- **SEC-FR-017** `resolved` is reserved under `/workspaces/:slug/vars/`: it cannot be addressed as a key by `PATCH` or `DELETE` on that path (`405`). There is no route that reads a single entry by key.

### 11.5 REST API

All paths are under `/api/v1`. Errors use the envelope and types of §3. The scopes differ only in access (SEC-FR-004 to SEC-FR-006), so each operation is specified once on the user secret route and the others refer to it.

Errors common to all routes: `401 unauthenticated`; `403 insufficient_scope` (PAT scope); `403 forbidden` (admin token on user scope, member on a workspace secret route or variable write); `403 token_bound`; `404` (org, workspace); `415`; `413`; `500 internal` (the cause is never sent).

#### `POST /user/secrets`
- **Scope:** `secrets:write`.
- **Body:** `{"entries":[{"key": string, "value": string}, …]}`, validated per SEC-FR-007. Accepts `Idempotency-Key`.
- **Response:** `201`, `{"items":[{key, created_at, updated_at}, …]}` in request order.
- **Errors:** `400 invalid_request`; `429 secret_limit_reached` (scope full); `409 conflict`; `503 secrets_unavailable`.
- **Side effects:** one `parley.secret.create` per entry.

#### `GET /user/secrets`
- **Scope:** `secrets:read`.
- **Response:** `200`, `{"items":[…],"next_cursor":…}` of secret entries, ordered by key. Never contains values.

#### `PATCH /user/secrets/:key`
- **Scope:** `secrets:write`.
- **Body:** `{"value": string}`, per SEC-FR-008.
- **Response:** `200`, the secret entry.
- **Errors:** `400 invalid_request`; `404 not_found`; `503 secrets_unavailable`.
- **Side effects:** `parley.secret.update`.

#### `DELETE /user/secrets/:key`
- **Scope:** `secrets:write`.
- **Response:** `204`. **Errors:** `404 not_found`.
- **Side effects:** `parley.secret.delete`.

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
As `POST /user/secrets`, with scope `vars:write`. The `201` items include `value`. Never answers `503`. Emits one `parley.variable.create` per entry.

#### `GET /user/vars`
As `GET /user/secrets`, with scope `vars:read`. Items include `value`.

#### `PATCH /user/vars/:key`
As `PATCH /user/secrets/:key`, with scope `vars:write`. Answers the variable entry. Emits `parley.variable.update`.

#### `DELETE /user/vars/:key`
As `DELETE /user/secrets/:key`, with scope `vars:write`. Emits `parley.variable.delete`.

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
- **Multi-scope create:** `create` accepts several scope flags. One request is sent per scope, in the order user → org → workspace, and the results are printed as one document, `{"results":[{"scope":"user"|"org"|"workspace","items":[…]}, …]}`, holding the scopes that succeeded. A failing scope does not stop the others. Each failed scope's error envelope is printed exactly once, on stderr, and the command exits 1 if any scope failed.
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
| `parley.secret.create` | `secret` | `create` | per entry of a successful create |
| `parley.secret.update` | `secret` | `update` | successful update |
| `parley.secret.delete` | `secret` | `delete` | successful delete |
| `parley.variable.create` / `.update` / `.delete` | `variable` | `create` / `update` / `delete` | as above |

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

- A full scope answers `429 secret_limit_reached` on create even when a key of the request also conflicts.
- An empty `GIT_PAT` counts as no credential.
- No key configured: secret create and update answer `503 secrets_unavailable`; git and forge operations that need a stored credential fail with an error that says the secret is unreadable; public repositories keep working; a sandbox whose profile requires a credential or an exposed secret whose value cannot be read is not created or started (§13).
- A fork with a variable of the same name as its parent's wins over the parent; deleting the fork's entry exposes the parent's again.
- A fork owner who loses write access to the parent loses the parent step for secrets at the next resolution; a sandbox that is already running keeps its credentials and its exposed secrets until it stops, and its next start resolves again (SBX-FR-041). The parent step for variables is unaffected.
- A fork owner who leaves the org loses access to the fork (§3). Until the fork expires or is deleted, its chain has no org step; the parent step for variables stays for the callers who can still reach the fork (the parent's owner, admin tokens).
