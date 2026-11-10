[Parley PRD](00_overview.md) · §4

## 4. Workspaces and task forks

### 4.1 Purpose and scope

A **workspace** is the hub's record of one git repository. It has a globally unique slug, an owner, an organization, a status, settings, and a copy of the repository that the hub keeps as a bare repository. Agents and people reach that copy through the git server (§5) and the source control API (§6). Work on it is proposed as changes (§7) and lands under policy (§8).

There are two kinds of workspace:

- A **repository** workspace is registered from a git URL, its **origin**. The hub clones origin, can sync from it, and can push branches back to it.
- A **task fork** is a cheap, short-lived workspace created from a repository workspace, its **parent**. It is created in one call, is ready at once whatever the size of the repository, and expires. A fork is where one agent does one task.

In scope here: the workspace entity and its response object; workspace settings (keys, types, defaults, validation); the status state machine; git credentials for origin; create, list, get, update, archive, reactivate, sync, reclone, push to origin and delete; task forks in full; the `parley workspace` and `parley fork` commands; the `parley.workspace.*` and `parley.fork.expire` events.

Out of scope, with hand-off points only: the git protocol and `parley.git.push` (§5); refs, commits and files over the API (§6); changes (§7); landing, protected-branch enforcement and policy (§8); carry-patch sync, patches and rebuild (§10); secret and variable storage (§11); sessions and the event log (§14).

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
- A protected branch with landed or directly pushed commits that origin does not have yet is reported as `ahead`. That is normal and is not an error.
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
- `POST /workspaces/:slug/forks` answers `201` with a workspace in `status: "ready"`, its git remote URL and, if I asked, a token bound to the fork and a sandbox with the fork checked out (§13).
- The call takes the same time for a small and a very large repository.
- The fork shows the parent's branches, read-only and always current. I push my work to new branches.

**WS-US-10: Contribute without write access (Org member).** As a member with read-only access to a workspace, I want to fork it, push to my fork and have a change opened against the parent.
- A member may fork any workspace they can read and is the owner of the fork.
- With `auto_change`, a push to a fork branch that has no open change (normally the first push of a new branch) opens a change that targets the parent (§5, §7).

**WS-US-11: Forks clean up after themselves (Operator, Developer).** As an operator, I want forks to disappear without anyone deleting them.
- A fork is deleted when its `expires_at` passes.
- A fork whose changes have all landed or been abandoned is deleted 24 hours later.
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
| `auto_change` | bool or null | null | Fork only: whether a push or API commit to an own branch that has no `draft` or `open` change opens one (§5, §6). |
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
| `protect.owner_push` | bool | | `true` | §8: `false` refuses direct pushes of protected branches for the owner and the admin token too. |
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

This section defines the keys, their types, defaults and validation. What `protected_branches`, `protect.owner_push`, the `land.*` keys, `changes.require_intent` and `changes.auto_restack` do is defined in §7 and §8.

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
- **Automatic change.** With `auto_change` (default true), a push or API commit that creates or moves an own branch which is not the source of a `draft` or `open` change opens a change (§7) in the parent, with the fork branch as source and the fork's base branch as target. Normally that is the first push of a new branch.
- **Lifetime.** A fork has an `expires_at` and is deleted when it passes. The default lifetime is 7 days and the longest is 30 days. It can also be deleted early by the hub, 24 hours after its work has landed (WS-FR-070).
- **Sandboxes.** A fork can have sandboxes (§13), and one can be created with it in the same call (WS-FR-082). A sandbox never outlives its fork: deleting the fork deletes its sandboxes first.
- **Ownership and access.** The caller owns the fork. The fork is in the parent's organization. Every reader of the parent can read the fork.
- **Configuration data.** Variables and secrets resolve in the order fork → parent workspace → organization → user (§11). For secrets the parent step applies only when the fork's owner has write access to the parent, so a fork owned by a read-only org member never receives the parent's secrets (§11.3). The same chain decides which secrets become credentials of a fork's sandbox (SBX-FR-024) and which values a profile's `expose_secrets` places in it (SBX-FR-065).
- **Carry-patch parents.** A fork of a carry-patch workspace is a plain fork. It has no patches, no upstream and no integration branch of its own. The parent's integration branch and patch branches are ordinary tracking branches in it.

### 4.4 Functional requirements

**Authentication and access**

- WS-FR-001 Every route follows the global check order (§3): authenticate (`401`), binding guard for bound PATs (`404` or `403 token_bound`), PAT scope (`403 insufficient_scope`), workspace access (`404` or `403`), validation (`400`), state (`409` or `400`), act.
- WS-FR-002 Scopes are `workspaces:read` and `workspaces:write`. A read check accepts either. Only PATs are scope-checked.
- WS-FR-003 Access to a repository workspace: an admin token has `admin`; the owner has `owner`; a member of the workspace's organization has `member`. Anyone else gets `404 workspace not found`, identical to an unknown slug. A `member` is read-only: a write route answers `403 forbidden`. Two routes are open to members on purpose: fork creation and fork listing.
- WS-FR-004 Access to a fork: the user who created it has `owner`. The owner of the parent also has `owner`, so that they can inspect and remove forks of their workspace. Members of the organization have `member`. Admin tokens have `admin`. A caller with no access to the parent has none on the fork either, even as its creator (§3): a member who leaves the organization loses the forks they created in it. Such a fork stays until it expires or the parent's owner deletes it.
- WS-FR-005 A token bound to workspace W (§3) is valid on the routes of W and of the forks that this token created. On any other workspace it gets `404 workspace not found`, with one exception defined in §3: a token bound to a fork may use, on the fork's parent, the change and landing routes for changes whose source is the fork (§7, §8), the policy read routes (§8) and the awareness read routes (§9); every other route on the parent answers `403 token_bound`. `GET /workspaces` shows it W and those forks. `POST /workspaces` answers `403`, `error_type: token_bound`.
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
- WS-FR-021 Create answers `201` with `status: "pending"` and `access: "owner"`, and emits `parley.workspace.create`.

**Clone**

- WS-FR-022 A clone brings all branches and tags of origin with full history into the hub's bare copy. On success the hub records `default_branch` (origin's default branch unless one was given), `head_sha` and the origin head of every branch, then sets `ready`.
- WS-FR-023 If `default_branch` was given at creation and origin has no such branch, the clone fails with a `clone_error` that says so.
- WS-FR-024 A failed clone sets `failed` and `clone_error`, and leaves no partial copy behind. `clone_error` never contains a credential; userinfo in URLs is removed. A failed clone is not retried automatically. The owner recovers with reclone.
- WS-FR-025 Clone work is durable. When the hub starts, every workspace in `pending` or `cloning` is taken up again, in `created_at` order, before the hub serves requests for it.
- WS-FR-026 When clone work finds an existing copy, it checks whether the copy is valid: it opens as a repository, its default branch resolves to a commit, and its origin is the workspace's `git_url`. A valid copy is kept and the workspace becomes `ready` with `head_sha` read from the copy. Anything else is discarded and cloned fresh. A copy left by an earlier workspace with the same slug is never adopted.
- WS-FR-027 At most `[workspace].workers` clones run at the same time. Further clone work waits in order.

**List and get**

- WS-FR-028 The list holds the workspaces the caller can read: own workspaces and the forks of own workspaces as `owner`, workspaces of the caller's active organizations as `member` (ownership wins), and every workspace as `admin` for an admin token. A bound token sees the set of WS-FR-005.
- WS-FR-029 Archived workspaces are left out unless `include_archived=true`. Forks are left out unless `include_forks=true` or `parent` is given. Order is `created_at` descending, then `slug` ascending.

**Update**

- WS-FR-030 The mutable fields of a repository workspace are `display_name`, `description`, `org_id` and `settings`. The body must be a JSON object with at least one of them. Unknown fields are ignored.
- WS-FR-031 An immutable field in the body is refused with `400`, whatever its value: `slug`, `kind`, `git_url`, `default_branch`, `owner_id`, `workspace_mode`, `upstream_url`, `integration_branch`, `parent_slug`, `fork_point`.
- WS-FR-032 `display_name: null` or `""` resets it to the slug. `description: null` resets it to `""`. `org_id: null` moves the workspace to its owner's personal organization. For any other `org_id`, the workspace's owner must be a member of the target organization, else `403 forbidden`. `settings` is applied per 4.3.3.
- WS-FR-033 Moving a workspace to another organization moves its forks with it.
- WS-FR-034 An archived workspace cannot be updated: `409 workspace_archived`. A workspace in any other status can.
- WS-FR-035 A successful update emits `parley.workspace.update` with the list of fields that were given.

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

- WS-FR-039 `ahead` is the normal state of a protected branch (§8) that has landed or directly pushed commits which nobody has pushed to origin yet. It is not an error and sets no `sync_error`.
- WS-FR-040 `diverged` changes nothing for that branch and does not fail the call. The other branches are still processed.
- WS-FR-041 Sync is one of the three ways a protected branch may move (§8). It moves one only by `created`, `fast_forward` or `reset`.
- WS-FR-042 Branches that exist only in the hub are not touched and not reported. A branch that was deleted on origin is not deleted in the hub.
- WS-FR-043 Each branch move is a compare-and-swap. If a branch moves in the hub while sync runs, sync evaluates that branch again against the new head. Two concurrent syncs are safe.
- WS-FR-044 A sync that completes sets `last_sync_at`, clears `sync_error`, updates `head_sha` and `origin_head_sha`, and emits `parley.workspace.sync`. This holds also when branches are `ahead` or `diverged`.
- WS-FR-045 A sync that cannot fetch (credential, network, origin refusal) moves nothing, sets `sync_error` and answers `502 upstream_error`. No event is emitted.
- WS-FR-046 A sync runs to completion when the caller disconnects.
- WS-FR-047 A branch that sync moves and that is the source of an open change gets a new version, as for any new head (§7).
- WS-FR-048 For a carry-patch workspace, sync also fetches the upstream, refreshes patches and may start a rebuild. The per-branch pass leaves out the integration branch and the branches of registered patches. This and the extra response fields are defined in §10.

**Push to origin**

- WS-FR-049 Push to origin sends exactly one branch of the hub, `refs/heads/<name>`, to the same branch on origin with the origin credential. It needs a repository workspace in `ready` status.
- WS-FR-050 Without `force`, the push is fast-forward only: origin accepts it only if its branch is an ancestor of the hub's.
- WS-FR-051 With `force`, the push uses force-with-lease. The lease is the origin head of that branch that the hub last saw (or "no such branch" if it never saw one). Origin's branch is replaced only if it still has that value. If origin moved since, nothing is overwritten and the answer is `409`, `error_type: origin_moved`. The hub never sends a blind force.
- WS-FR-052 A forced push of a protected branch (§8) is refused with `403`, `error_type: protected_branch`. The landed history of a protected branch only ever goes to origin as a fast-forward.
- WS-FR-053 On a carry-patch workspace, the integration branch and registered patch branches are refused (`403`, `error_type: integration_branch` or `patch_branch`). They reach origin through the settings of §10.
- WS-FR-054 The push has a 2-minute deadline. It runs to completion or to its deadline when the caller disconnects.
- WS-FR-055 After a successful push the hub records the pushed commit as the origin head of that branch.
- WS-FR-056 Each push handed to origin emits exactly one `parley.workspace.push` event, on success and on failure. Refusals before the push emit none.
- WS-FR-057 Every message from this route is scrubbed: the credential is replaced by `[redacted]`, URL userinfo is removed, and the text is cut to 1024 characters.

**Archive, reactivate, reclone, delete**

- WS-FR-058 Archive applies to a repository workspace in `ready` or `failed`. It first tries to push every branch to origin, fast-forward only, as a safety copy. On a carry-patch workspace the integration branch is left out, because the hub rebuilds it; patch branches are included. The outcome of that push never blocks the archive. The hub keeps its copy of the repository, so unpushed branches and change versions survive.
- WS-FR-059 An archived workspace is not served over git (§5), accepts no writes, and is hidden from the default list. It can still be read with `GET`.
- WS-FR-060 Archive force-closes every active agent session of the workspace and of its forks with reason `workspace archived` (§14). A failure to close a session does not fail the archive. Tokens, secrets, variables, patches and changes are untouched.
- WS-FR-061 Operations that do not apply to a fork are refused with `400` and one of these types:

  | Operation on a fork | `error_type` |
  |---|---|
  | sync, reclone, push to origin, the forge webhook `PUT` and `DELETE` and the forge deliveries list (§12) | `fork_has_no_origin` |
  | every carry-patch route of §10 | `workspace_mode_mismatch` |
  | archive, reactivate, change of `org_id` or `settings`, creating a fork of it, replacing the policy (§8), deleting a workspace sandbox rule (§13), the event webhook routes (§14) | `not_supported_on_fork` |

- WS-FR-062 Reactivate applies to an `archived` workspace, else `409 workspace_not_archived`. It sets `pending` and records clone work, which finds the kept copy (WS-FR-026).
- WS-FR-063 Reclone applies to a repository workspace in `ready` or `failed`. It is refused with `409`, `error_type: has_forks`, while the workspace has forks, because forks depend on the parent's copy. It first tries to push every branch to origin, fast-forward only (on a carry-patch workspace: every branch except the integration branch); a failure of that push does not stop it. It then discards the hub's copy, clears `sync_error`, `origin_head_sha` and all origin heads, sets `pending` and records clone work. Whatever did not reach origin is lost, including refs under `refs/parley/`. Change records stay; what a change does when its commits are gone is defined in §7.
- WS-FR-064 Delete of a repository workspace applies only when it is `archived`, else `409`, `error_type: workspace_not_archived`.
- WS-FR-065 Delete is transactional. The sandboxes of the workspace and of its forks are deleted first (SBX-FR-045). In one step the hub then removes the workspace and its forks, revokes every token bound to any of them, and removes everything they own: workspace secrets and variables, patches, changes, policy, claims, settings, workspace sandbox rules (§13) and event webhook subscriptions with their undelivered events (§14). If the step fails the answer is `500` and nothing has changed. The same step force-closes the remaining active sessions of the workspace and its forks (reason `workspace deleted`, §14). Only after it succeeded does the hub discard the repository data. The slug is free again. Audit records and the event log are kept.

**Task forks**

- WS-FR-066 A fork is writable only while its parent is `ready`. While the parent is `archived`, every write to the fork (git push, source control API, update of the fork) answers `409`, `error_type: parent_archived`. Reads and fetches keep working, and delete and expiry keep working. While the parent is in an archive or delete call, or is `pending` or `cloning` after a reactivate, the fork answers `409 workspace_busy`.
- WS-FR-067 Fork creation is synchronous. The answer is `201` with the fork in `status: "ready"`, and the fork is usable over git and the API from that moment.
- WS-FR-068 Fork creation copies no history. Its duration and the storage it adds MUST NOT depend on the size of the parent repository (number of commits, files, branches of history, or bytes). The target is under one second for any repository; the measured bound is set in §15. The storage a fork uses grows only with what is pushed to it.
- WS-FR-069 `ttl` defaults to `[forks].default_ttl` and may not exceed `[forks].max_ttl`. The shortest `ttl` is 5 minutes. `expires_at` is the creation time plus `ttl`.
- WS-FR-070 Early expiry. When `[forks].expire_on_land` is true, a fork is deleted before `expires_at` once all of these have held for 24 hours without interruption: the fork is the source of at least one change; every such change is `landed` or `abandoned`; every own branch that still exists in the fork is the source of such a change. A push to the fork, a new change from it, or a restored change starts the 24 hours again. Early expiry also waits while the fork has a sandbox in state `provisioning` or `ready` (SBX-FR-045). The fork's `expires_at` still applies.
- WS-FR-071 The owner can set a new lifetime at any time: `ttl` in an update sets `expires_at` to now plus `ttl`, within the same bounds as at creation. This may also shorten the lifetime.
- WS-FR-072 A parent may have at most `[forks].max_per_workspace` forks at one time. A create beyond that answers `429`, `error_type: fork_limit_reached`. Forks that were deleted or expired do not count.
- WS-FR-073 A tracking branch cannot be written in the fork by any means (git push, API ref update, API commit, delete). The refusal is `403`, `error_type: tracking_branch`; over git it is a per-ref rejection (§5). A fork cannot create a branch with the name of a tracking branch.
- WS-FR-074 A fork's `head_sha` is the parent's current head of the fork's base branch. `fork_point.sha` does not change.
- WS-FR-075 With `auto_change` true, a push (§5) or API commit (§6) that creates or moves an own branch of the fork which is not the source of a `draft` or `open` change opens a change in the parent with that branch as source and the fork's base branch as target. Normally that is the first push of a new branch; a push after the earlier change landed or was abandoned opens a new change. Title, intent, session and draft state come from push options or defaults (§7). Later pushes to the same branch add versions. With `auto_change` false, nothing is opened unless the push asks for it (§7).
- WS-FR-076 Fork creation may mint a token. When the body has `token`, the hub creates a PAT owned by the caller and bound to the fork, and returns its secret once, in this response only.
  - `permissions` is a non-empty list of registered scopes. If the caller authenticates with a PAT, it needs `tokens:manage` and is held to the subset rule of §3 (PLAT-FR-051): it must hold every requested scope exactly, else `403 insufficient_scope`.
  - `paths` is an optional list of path globs that limits what the token may write (§3, PLAT-FR-057). If the caller's own token is path-limited, `paths` is required and every glob must be covered by one of the caller's (PLAT-FR-059), else `403 insufficient_scope`.
  - `expires` is an optional duration. Without it the token lives as long as the fork.
  - `name` defaults to `fork:<fork slug>`.
  - The token is revoked when the fork is deleted or expires.
- WS-FR-077 `session_id` at creation must satisfy the session-linking rule of §14 (the session's status is not checked), else `400`, `error_type: unknown_session`. It is stored on the fork and carried on the fork's events.
- WS-FR-078 Deleting a fork needs no archive first. The fork's sandboxes are deleted first, without a checkpoint (SBX-FR-045). The delete then removes the fork, its own branches, its variables and secrets, and revokes the tokens bound to it, and force-closes its active sessions (reason `workspace deleted`, or `fork expired` when the sweep deletes it), all in one transactional step.
- WS-FR-079 Deleting or expiring a fork does not delete changes. Changes that have the fork as source belong to the parent and their versions stay fetchable there (§7). What an open change does when its source is gone is defined in §7.
- WS-FR-080 Deleting a parent deletes all its forks in the same step (WS-FR-065). One `parley.workspace.delete` is emitted for each fork and one for the parent.
- WS-FR-081 A fork never changes its parent's branches. The only path from a fork into the parent is a change that lands (§7, §8).
- WS-FR-082 Fork creation may create a sandbox. When the body has `sandbox`, an object with the fields of the sandbox create request (§13.5), the hub creates the fork and then a sandbox on it, and the response carries both (SBX-FR-028). The caller also needs `sandboxes:write`. If the sandbox request is refused, the fork is not created either and the answer is the sandbox route's error. The sandbox's `session_id` and `purpose` default to the fork's. The fork is `ready` at once (WS-FR-067); the sandbox is returned in state `provisioning` and becomes `ready` later.

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
- **Errors:** `400 invalid_request`; `400 credential_check_failed`; `403 forbidden` (admin token, or not a member of the organization); `403 token_bound`; `409 conflict` (slug in use); `503 secrets_unavailable` (a credential was given and no encryption key is configured, §11).
- **Side effects:** credential secrets stored; clone work recorded; `parley.workspace.create`.

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
- **Side effects:** `parley.workspace.update`.

#### `POST /workspaces/:slug/archive`

- **Purpose:** archive a repository workspace.
- **Scope:** `workspaces:write`. Write access. No body.
- **Response:** `200` workspace object with `status: "archived"`.
- **Errors:** `400 not_supported_on_fork`; `409 workspace_archived` (already archived); `409 workspace_busy` (`pending`, `cloning`, or another maintenance operation).
- **Side effects:** branches offered to origin (WS-FR-058); sessions force-closed; forks become read-only; `parley.workspace.archive`.

#### `POST /workspaces/:slug/reactivate`

- **Purpose:** bring an archived workspace back.
- **Scope:** `workspaces:write`. Write access. No body.
- **Response:** `200` workspace object with `status: "pending"`.
- **Errors:** `400 not_supported_on_fork`; `409 workspace_not_archived`.
- **Side effects:** clone work recorded; `parley.workspace.reactivate`.

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
- **Side effects:** branches and tags created or moved; origin heads recorded; sync fields updated; `parley.workspace.sync`.

#### `POST /workspaces/:slug/reclone`

- **Purpose:** discard the hub's copy and clone origin again.
- **Scope:** `workspaces:write`. Write access. No body.
- **Response:** `200` workspace object with `status: "pending"`, `sync_error: null`, `origin_head_sha: null`.
- **Errors:** `400 fork_has_no_origin`; `409 workspace_archived`; `409 workspace_busy`; `409 has_forks`.
- **Side effects:** WS-FR-063; `parley.workspace.reclone`.

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

- **Side effects:** origin's branch updated; origin head recorded; one `parley.workspace.push` per attempt.

#### `DELETE /workspaces/:slug`

- **Purpose:** permanently delete an archived repository workspace, or delete a fork.
- **Scope:** `workspaces:write`. Write access.
- **Response:** `204`, empty body.
- **Errors:** `409 workspace_not_archived` (repository workspace that is not archived); `409 workspace_busy`; `500 internal` (nothing was deleted).
- **Side effects:** WS-FR-065 for a repository workspace, WS-FR-078 for a fork. Tokens bound to a deleted workspace answer `401` from then on. `parley.workspace.delete`.

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
| `sandbox` | object | none | The body of the sandbox create request (§13.5), WS-FR-082 |

- **Response:** `201`

```json
{
  "workspace": { "slug": "my-project--k3f9x2ab", "kind": "fork", "status": "ready", "…": "…" },
  "remote": "https://hub.example.com/git/acme/my-project--k3f9x2ab.git",
  "token": { "token_id": "k3x9q2mz", "user_id": "uuid", "name": "fork:my-project--k3f9x2ab", "token": "parley_pat_…",
             "permissions": ["git:read", "git:write", "changes:write"], "paths": ["src/loader/**"],
             "workspace": "my-project--k3f9x2ab", "expires_at": null, "revoked_at": null,
             "created_at": "2026-01-01T00:00:00Z" },
  "sandbox": { "id": "sbx_…", "workspace": "my-project--k3f9x2ab", "state": "provisioning", "…": "…" }
}
```

  `workspace` is the fork's workspace object with `access: "owner"`. `remote` equals its `hub_url`. `sandbox` is present only when it was requested and is the sandbox object of §13. `token` is present only when it was requested. It is the `Token` object of §3 with its value in the member `token`; the value is never shown again, and an idempotent replay omits it (§3).
- **Errors:** `400 invalid_request` (bad `name`, `ttl`, `token`, malformed `session_id`); `400 unknown_session` (`session_id` fails the session-linking rule of §14); `400`, `error_type: fork_ref_not_found` (`ref` is not a branch of the parent); `400 not_supported_on_fork` (the parent is itself a fork); `403 forbidden` (admin token); `403 insufficient_scope` (a PAT caller without `tokens:manage`, or token permissions or paths beyond the caller's); `409` per WS-FR-007 (parent not `ready`); `409 conflict` (fork name in use); `429 fork_limit_reached`. With `sandbox`, also the errors of the sandbox create route (§13.5), among them `404 sandboxes_disabled`, `422 credential_missing` and `429 sandbox_capacity`; the fork is then not created.
- **Side effects:** fork created; optional token created; `parley.workspace.fork`. With `sandbox`: sandbox created; `parley.sandbox.create`.

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
- **Output:** one document: the sync answer. With `--wait` and a rebuild, the same answer with one more member, `rebuild`, holding the final job record (§3: stdout carries exactly one document).
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
| `--sandbox-profile <name>` | `sandbox.profile` | Giving this flag is what requests a sandbox (SBX-FR-028). |
| `-- command...` | `sandbox.command` | Optional trailing command: the sandbox's main process. Needs `--sandbox-profile` (else exit 2). |

- **Calls:** `POST /workspaces/:slug/forks`.
- **Output:** the response: `workspace`, `remote` and, when requested, `sandbox` and `token` with its secret. The secret is printed this once only; the command says so on stderr.
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
- when `[forks].expire_on_land` is true, every fork that has met the conditions of WS-FR-070 for 24 hours (reason `landed`). A fork with a `provisioning` or `ready` sandbox is skipped.

Each deletion emits `parley.fork.expire`. A fork stays usable until the sweep removes it, so a fork may live up to a minute past `expires_at`. Forks of an archived parent expire as usual. A deletion that fails is retried by the next sweep. The number of forks that exist is exported as `parley_forks_active` (§14).

#### 4.7.3 Carry-patch hand-off points

- After a fresh clone: upstream set-up (§10).
- During sync: upstream fetch, patch refresh, merge detection and rebuild (§10).
- On push to origin: refusal of the integration branch and patch branches (§10).

### 4.8 Events

All are hub events with the common fields of §14. `workspace` is the slug of the workspace acted on; for a fork event it is the fork's slug, and `metadata.parent_slug` names the parent. No event ever carries a credential.

| Event | When | Metadata |
|---|---|---|
| `parley.workspace.create` | A repository workspace was created. | `kind`, `git_url` (without userinfo), `workspace_mode`, `default_branch` (may be null) |
| `parley.workspace.update` | A successful update. | `fields`: the fields given |
| `parley.workspace.sync` | A sync completed. | `created`, `fast_forward`, `ahead`, `diverged`, `reset`: lists of branch names; `updated`: list of `{ref, before, after}` |
| `parley.workspace.push` | Each push handed to origin, success or failure. | `branch`, `sha`, `force`, `ok`, `remote_url` (without userinfo), and `result` when ok or `error_type` when not |
| `parley.workspace.archive` | A successful archive. | `head_sha`, `origin_push` (`ok`, `partial` or `failed`), `forks` (count) |
| `parley.workspace.reactivate` | A successful reactivate. | none |
| `parley.workspace.reclone` | A reclone was accepted. | `origin_push` (`ok`, `partial` or `failed`) |
| `parley.workspace.delete` | A workspace or fork was deleted by a caller, or a fork was deleted with its parent. | `kind`, `parent_slug`, `revoked_tokens` (sorted ids), `deleted_forks` (slugs; parent only) |
| `parley.workspace.fork` | A fork was created. `resource_id` is the fork's slug. | `parent_slug`, `fork_point`, `expires_at`, `auto_change`, `purpose`, `token_id` (when a token was minted) |
| `parley.fork.expire` | The sweep deleted a fork. Actor is `system`. | `parent_slug`, `reason` (`ttl` or `landed`), `expires_at`, `revoked_tokens` |

A fork that expires emits `parley.fork.expire` and not `parley.workspace.delete`. Events about a fork carry the fork's `session_id` when it has one.

Pushes of agents and people into a workspace or fork emit `parley.git.push` (§5). Session terminations emit the session events of §14. Failed syncs, failed clones and refused requests emit no workspace event.

### 4.9 Configuration

| Key | Type | Default | Meaning |
|---|---|---|---|
| `[workspace].workers` | integer | `4` | Clones that may run at the same time. Must be at least 1; any other value stops the start with a clear message. |
| `[forks].default_ttl` | duration | `7d` | Lifetime of a fork when the request gives no `ttl`. |
| `[forks].max_ttl` | duration | `30d` | Longest `ttl` a request may ask for, at creation and on extension. Must not be shorter than `default_ttl`. |
| `[forks].max_per_workspace` | integer | `200` | Forks a parent may have at one time. |
| `[forks].expire_on_land` | bool | `true` | Delete a fork early, 24 hours after its changes have all landed or been abandoned. |
| `[server].external_url` | string | `""` | Base of `hub_url` and `remote` (defined in §3). |

Fixed values: credential check deadline 10 seconds; push-to-origin deadline 2 minutes; early-expiry grace period 24 hours; shortest fork `ttl` 5 minutes; expiry sweep at least once a minute; scrubbed message limit 1024 characters.

### 4.10 Edge cases and error handling

- **Generated fork name collides.** The hub picks another name. The caller never sees a conflict for a name it did not choose.
- **Archive when origin refuses the safety push.** The workspace is archived anyway. Nothing is lost, because the hub keeps its copy. The event says `origin_push: failed` or `partial`.
- **Reactivate when the kept copy is damaged or missing.** The workspace is cloned fresh from origin. Commits that existed only in the hub are gone.
- **Sync with `reset_to_origin`.** Only `diverged` branches are reset. `ahead` branches keep their commits. Resetting a protected branch discards landed and directly pushed commits from that branch; the changes that landed them keep their state and versions (§7).
- **Sync while a change lands.** Both are compare-and-swap. If landing moves the branch first, sync evaluates it again and will usually report `ahead`. If sync moves it first, landing retries onto the new head (§8).
- **Forced push when the hub never saw the branch on origin.** The lease is "no such branch". If origin has the branch after all, the answer is `409 origin_moved`.
- **Branch name with `%` in the path.** It is decoded once: `fix%2F42` is `fix/42`, and `a%2525b` is `a%25b`.
- **Fork of a branch that is deleted in the parent later.** The fork stays. Its base branch no longer resolves, so `head_sha` is `null` and a new change cannot target it. Existing own branches stay readable.
- **Parent branch created after the fork, with a name the fork already uses.** Nothing happens in the fork. The fork's branch stays an own branch.
- **Fork request with a commit id or a tag as `ref`.** Refused with `400`, `error_type: fork_ref_not_found`. A fork needs a base branch that a change can target.
- **Extending a fork after `expires_at` but before the sweep.** Allowed. The fork lives on.
- **Fork that lives longer than its sandbox.** A fork lives 7 days by default; a sandbox lives at most `[sandbox].max_ttl` (default 24 hours, §13). The sandbox ends first and leaves a checkpoint in the fork, from which a new sandbox can be restored while the fork exists.
- **Fork with work that has no change.** With `auto_change` false and no change opened by hand, early expiry never applies. Unlanded work in a fork is lost at `expires_at`; the fork's owner is expected to extend it or open a change.
- **Parent archived with live forks.** Forks become read-only, their sessions are closed, and they still expire on time. Reactivating the parent makes the remaining forks writable again.
