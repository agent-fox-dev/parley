[Parley PRD](00_overview.md) · §7

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
| `needs_restack` | bool | True when the change is not built on the current head of the change below it (or on the target, once that change landed): the automatic restack conflicted, `changes.auto_restack` is false, or the stack is too deep |
| `restack_reason` | enum or null | Why `needs_restack` is true: `conflict`, `auto_restack_off` or `stack_too_deep`. Null otherwise |
| `stats` | object | `{commits, files_changed, insertions, deletions}` of the current version against `base_sha` |
| `paths` | string[] | Files touched by the current version, at most 2,000; `paths_truncated` says whether there were more |
| `landing` | object | Policy evaluation (§8): `{status, reason, requirements[]}`. `status` is `ready`, `pending` or `blocked`. `reason` is null unless the block is structural (`nothing_to_land`, `target_not_found`, `draft`, `merge_commits`) |
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
| `draft`, `open` | `abandoned` | abandon; or the source branch is deleted by a git push (CHG-FR-013); or the source workspace (a fork) is deleted |
| `abandoned` | `open` | restore, while the source workspace exists and the source branch or the last version's commit still exists |

`landed` is terminal. A draft is never landed and is never restacked onto by `auto_land`.

One source branch is the source of at most one change in state `draft` or `open`. Opening a second is `409`, `error_type: change_exists`, naming the existing change.

#### 7.3.2 Version

| Field | Type | Meaning |
|---|---|---|
| `number` | int | 1, 2, 3 … within the change |
| `head_sha` | string | Head commit of this version |
| `base_sha` | string | What this version was built on |
| `kind` | enum | `push` (git push), `commit` (API commit), `ref` (API ref move), `rebase` (server-side rebase requested by a client), `restack` (hub restack in a stack), `revert` (generated by a revert), `sync` (the source branch was moved by a sync from origin, §4) |
| `patch_equivalent_to` | int or null | An earlier version whose diff against its own base is identical to this one's. A pure rebase is patch-equivalent to its predecessor |
| `created_by` | object | `{actor_type, actor_id, username}`; `actor_type: "system"` for hub restacks |
| `session_id` | string or null | |
| `stats` | object | As on the change |
| `created_at` | timestamp | |

- Every version's head stays fetchable at `refs/parley/changes/<number>/<version>` in the target workspace for as long as the change exists, whatever happens to the source branch. The one exception is a reclone of the workspace (§7.10). These refs are hub-owned (§5).
- A new version is recorded whenever the source branch's head changes while the change is `draft` or `open`, by any means. Moving the branch to a commit that is already the head of the current version records nothing.
- Versions are immutable.

#### 7.3.3 Stack

- A change **depends on** another when its commits sit on top of that change's commits. `depends_on` is set:
  - explicitly, at open or by `PATCH`; or
  - automatically at open and on every new version: if the nearest ancestor of the head that is the current head of another `draft` or `open` change with the same target branch exists, that change becomes `depends_on`; if none exists, `depends_on` is cleared.
- The **stack** of a change is the chain of `depends_on` links down to a change with none. A stack is at most `[changes].max_stack_depth` changes deep (default 20); a version that would make it deeper is still recorded, and the change gets `needs_restack: true` with `restack_reason: "stack_too_deep"`.
- A dependent change's `base_sha` is the head of the change below. Its diff, stats, paths and policy evaluation cover only its own commits.
- All changes of a stack have the same target branch. They may come from different source workspaces.

#### 7.3.4 Mergeability

- `mergeable` answers: would the current version rebase cleanly onto the current head of the target branch (for a dependent change: onto the current head of the change below)?
- The hub recomputes it after every new version and after every move of the target branch. Recomputation is asynchronous. Until it finishes for the new pair of commits the value is `unknown`.
- A transition to `conflict` stores the conflict report on the change and emits `parley.change.conflict`. A transition back to `clean` clears it and emits `parley.change.ready` when the landing status is also `ready`.
- Landing never trusts a stored `mergeable` value. It always rebases for real (§8).

### 7.4 Functional requirements

Opening

- **CHG-FR-001** A change is opened in three ways, all with the same result: `POST /workspaces/:slug/changes`; a git push with change options or into an `auto_change` fork (§5); an API commit with a `change` object or into an `auto_change` fork (§6).
- **CHG-FR-002 (who may open).** The caller needs `changes:write` (PATs), write access to the source workspace and at least read access to the target workspace. This lets an org member open a change from their fork into a workspace they can only read.
- **CHG-FR-003 (validation at open).** In this order: target branch exists in the target workspace (`400`, `error_type: target_not_found`); source workspace is the target workspace or a fork of it (`400`, `error_type: source_not_related`); source branch exists (`400`, `error_type: source_not_found`); source and target are not the same branch of the same workspace (`400`); source has at least one commit that the target does not contain (`400`, `error_type: nothing_to_land`); none of those commits is a merge commit (`400`, `error_type: merge_commits_unsupported`); no other `draft` or `open` change has this source (`409 change_exists`); title rules; intent rule unless `draft`.
- **CHG-FR-004 (defaults at open).** `title` defaults to the subject of the head commit. `intent` defaults to the body of the head commit, then to the `purpose` of the source fork. `target_branch` defaults to the fork's base branch for a fork source, otherwise to the workspace's default branch. `session_id` defaults to the session given on the push or commit that triggered the open, then to the session recorded on the source fork.
- **CHG-FR-005** A change may target any branch of the workspace, protected or not. In a carry-patch workspace it may target a patch branch but never the integration branch (`403`, `error_type: integration_branch`).
- **CHG-FR-006** Opening records version 1, computes the stack link (§7.3.3), evaluates policy (§8), schedules the mergeability check and emits `parley.change.version` for version 1. `parley.change.open` is emitted when the change enters `open`: at once for a change created open, and when a draft is marked ready.

Versions

- **CHG-FR-010** A version is recorded by the hub as a consequence of the ref update, in the same transaction, so that a change never misses a head. The client does nothing extra.
- **CHG-FR-011** A new version: updates `head_sha`, `base_sha`, `stats`, `paths`, `session_id`; recomputes the stack link; re-evaluates policy; resets `mergeable` to `unknown` and schedules the check; carries approvals over only when it is patch-equivalent to the approved version (§8); marks earlier check results as belonging to their own version; emits `parley.change.version`.
- **CHG-FR-012** A version created with a path-scoped token (§3) is refused before the ref moves when it touches a path outside the token's `paths`; the refusal is the push rule of §5. Opening a change, a ref move onto existing commits and a restack are checked the same way, against the files of the resulting version (LAND-FR-080).
- **CHG-FR-013** When the source branch is deleted while the change is `draft` or `open`, the change is abandoned with reason `source_deleted`. The API refuses such a delete (§6); a git push that deletes the branch is accepted and has this effect.
- **CHG-FR-014 (interdiff).** The difference between two versions of a change is the difference between their diffs, with rebases factored out: a version that is patch-equivalent to another has an empty interdiff. The response lists per file whether it changed between the versions and gives the patch of that difference.

Editing and lifecycle

- **CHG-FR-020** `title`, `intent`, `labels`, `auto_land`, `state` (`draft` ⇄ `open`), `target_branch` and `depends_on` can be edited by the author, by anyone with write access to the target workspace, and by the admin token. Editing needs `changes:write`. The same callers, with the same scope, may abandon, restore and restack the change (CHG-FR-022, CHG-FR-023, CHG-FR-034); any other reader gets `403 forbidden`. "Author" means the user behind the credential that opened the change (`author.actor_id`), with any credential of that user, within the limits of a bound token (§3).
- **CHG-FR-021** Retargeting (`target_branch`) is allowed while `draft` or `open` and when the change has no `depends_on` and no dependants. It re-evaluates policy and mergeability and emits `parley.change.version` with `kind: "retarget"` in the metadata without creating a version.
- **CHG-FR-022 (abandon).** Abandoning a change with dependants needs `dependants=reparent` (the dependants are restacked onto the change below, or onto the target, without the abandoned change's commits) or `dependants=abandon` (they are abandoned too). Without the parameter the answer is `409`, `error_type: has_dependants`. Abandon never deletes the source branch or any version.
- **CHG-FR-023 (restore).** Restoring an abandoned change moves it to `open` at its last version. If the source branch no longer exists it is recreated at that version's head. If another `draft` or `open` change now uses that source, the answer is `409 change_exists`. If the source workspace (a fork) no longer exists, the answer is `409`, `error_type: source_workspace_gone`; the versions stay fetchable and a new change can be opened from a branch at the last version's head.
- **CHG-FR-024** A landed change is read-only except for `labels`.

Stacks and restack

- **CHG-FR-030 (restack on a new version below).** When a change with dependants gets a new version, the hub rebases each direct dependant's own commits onto the new head, moves the dependant's source branch with a compare-and-swap, and records a version of kind `restack`. This continues up the stack. It runs in the background shortly after the triggering version and is not part of the push or commit that caused it.
- **CHG-FR-031 (restack on landing below).** When a change lands, each direct dependant is rebased onto the landed commit on the target, its `depends_on` is cleared, and a `restack` version is recorded. Changes further up keep depending on the change directly below them and are restacked in turn.
- **CHG-FR-032 (restack failure).** If the rebase conflicts, nothing moves for that change or anything above it. The change gets `needs_restack: true` with `restack_reason: "conflict"`, `mergeable: conflict` and a conflict report, and `parley.change.conflict` is emitted. A later version pushed by the author that sits on the new head below clears the flag.
- **CHG-FR-033 (restack and the author's branch).** A restack moves the source branch on the hub. The author's local branch is then behind; a `git pull --rebase` or a fetch of `refs/parley/changes/<number>/<version>` brings it up to date. If the source branch moved between the hub reading it and the compare-and-swap, the restack is retried on the new head.
- **CHG-FR-034 (manual restack).** `POST …/changes/:number/restack` asks for the same operation on demand and returns its result synchronously: the new version, or `409 rebase_conflict` with the report. For a change with no `depends_on` it rebases onto the current target head.
- **CHG-FR-035** `auto_restack` is a workspace setting (`changes.auto_restack`, default true). When false, CHG-FR-030 and CHG-FR-031 only set `needs_restack` (with `restack_reason: "auto_restack_off"`) and leave the rebase to the manual call.

Reading

- **CHG-FR-040** Reading changes needs `changes:read` and read access to the target workspace. On a fork's slug, `GET /workspaces/<fork>/changes` lists the changes whose source workspace is the fork. A token bound to a fork F (§3) reaches these routes of this section on F's parent, and only for changes whose source workspace is F: `POST …/changes` (with `source.workspace_slug` = F); `GET …/changes` (confined to those changes); `GET …/changes/:number`, `…/versions`, `…/versions/:version`, `…/diff`, `…/stack` and `GET …/changes/by-commit/:sha`; `PATCH …/changes/:number`; `POST …/abandon`, `…/restore` and `…/restack`. A change with another source answers `404 change_not_found`. Within these routes the token has the access of its user (CHG-FR-020). The routes of §8 and §9 it reaches are listed in LAND-FR-003 and AWR-FR-001.
- **CHG-FR-041** List filters: `state` (repeatable; default `draft` and `open`), `target_branch`, `source_workspace`, `author` (user id or `me`), `session_id`, `label`, `path` (glob; changes touching a matching file), `mergeable`, `landing_status`, `stack_of=<number>` (every change in that change's stack, bottom first). Order: `updated_at` descending, or `number` ascending with `sort=number`.
- **CHG-FR-042** `GET /workspaces/:slug/scm/commits/:sha` (§6) names the change of any commit that is a version head or was landed by a change. `GET …/changes/by-commit/:sha` returns that change directly.
- **CHG-FR-043 (session link).** A session record (§14) lists the changes it contributed versions to. A change lists every session that contributed. A `session_id` on a change or version MUST satisfy the session-linking rule of §14; otherwise the triggering request is refused with `400`, `error_type: unknown_session`.

Retention

- **CHG-FR-050** Changes and their versions are kept for the life of the workspace. Deleting the workspace deletes them. Deleting a fork abandons the `draft` and `open` changes whose source it was; landed and abandoned changes and all version refs stay in the target workspace.

### 7.5 REST API

All routes are under `/workspaces/:slug`. The conventions of §3 apply. Unless a route says otherwise, `GET` routes need `changes:read` and read access (CHG-FR-040); `PATCH`, abandon, restore and restack need `changes:write` and the caller rule of CHG-FR-020. Common errors: `401`; `403 insufficient_scope`; `403 forbidden` (a caller who may read the change but not modify it); `404 workspace not found`; `404`, `error_type: change_not_found`.

#### `POST /workspaces/:slug/changes`
- **Purpose:** open a change targeting a branch of `:slug`.
- **Auth / scope:** `changes:write`; CHG-FR-002.
- **Body:** `{"source": {"workspace_slug"?: string, "branch": string}, "target_branch"?: string, "title"?: string, "intent"?: string, "draft"?: bool, "depends_on"?: int, "auto_land"?: bool, "labels"?: string[], "session_id"?: string}`. `source.workspace_slug` defaults to `:slug`.
- **Response:** `201` change object.
- **Errors:** CHG-FR-003; `400`, `error_type: intent_required`.
- **Side effects:** `parley.change.version`; `parley.change.open` unless `draft`.

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
- **Errors:** `400 intent_required`; `400`, `error_type: invalid_dependency` (unknown change, different target, would form a cycle, not `draft` or `open`); `409`, `error_type: change_closed` for an abandoned change, and for a landed change when the body has any field other than `labels` (CHG-FR-024).

#### `POST /workspaces/:slug/changes/:number/abandon`
- **Purpose:** abandon a change (CHG-FR-022).
- **Body:** `{"reason"?: string, "dependants"?: "reparent" | "abandon"}`.
- **Response:** `200` change object. **Errors:** `409 has_dependants`; `409 change_closed`.
- **Side effects:** `parley.change.abandon`; restack of reparented dependants.

#### `POST /workspaces/:slug/changes/:number/restore`
- **Purpose:** restore an abandoned change (CHG-FR-023).
- **Response:** `200` change object. **Errors:** `409 change_exists`; `409`, `error_type: change_not_abandoned`; `409`, `error_type: source_workspace_gone`.
- **Side effects:** `parley.change.restore`.

#### `POST /workspaces/:slug/changes/:number/restack`
- **Purpose:** rebase the change onto what it now sits on (CHG-FR-034).
- **Body:** `{"session_id"?: string, "resolutions"?: [...]}`. `resolutions` supplies the content of conflicted files (§8.4).
- **Response:** `200 {"status": "restacked" | "up_to_date", "change": <change object>}`.
- **Errors:** `409 rebase_conflict` with the conflict report in `details`; `409 change_closed`.
- **Side effects:** a `restack` version; `parley.change.restack`.

#### `GET /workspaces/:slug/changes/:number/versions`
- **Purpose:** list the versions of a change, oldest first.
- **Response:** `200` list of version objects.

#### `GET /workspaces/:slug/changes/:number/versions/:version`
- **Purpose:** read one version. `:version` is a number or `latest`.
- **Response:** `200` version object plus `ref` (`refs/parley/changes/<number>/<version>`).

#### `GET /workspaces/:slug/changes/:number/diff`
- **Purpose:** the change's own diff, or the difference between two versions.
- **Query:** `version` (default `latest`), `against` (a version number: return the interdiff of CHG-FR-014; absent: the version against its base), `path` (repeatable glob), `format` (`json` default, `patch`), `patches` (bool).
- **Response:** as `GET …/scm/diff` (§6), plus `version`, `against`.

#### `GET /workspaces/:slug/changes/:number/stack`
- **Purpose:** the whole stack the change is part of.
- **Response:** `200 {"target_branch", "changes": [<change summary>…]}`, bottom first. A summary has `number`, `title`, `state`, `version`, `head_sha`, `mergeable`, `needs_restack`, `restack_reason`, `landing.status`.

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
- **Exit codes:** 0 when the condition holds; 1 with `wait_failed` when the change is abandoned, or when waiting for `ready` or `mergeable` and the change becomes `blocked` or `conflict`; 1 with `wait_timeout` on timeout (§3).

### 7.7 Background behaviour

- **Mergeability checks** run after each new version and each move of a target branch that has `draft` or `open` changes. Checks for the same change coalesce: only the latest pair of commits is computed. On a busy target, checks are debounced by `[changes].mergeable_debounce` (default 5 s).
- **Restacks** (CHG-FR-030, CHG-FR-031) are durable: one that was pending at shutdown is run after the next start.
- **Start-up reconciliation.** At start the hub compares each `draft` or `open` change's current version with its source branch head and records a version for any head it missed, so that a crash between a ref update and its bookkeeping cannot lose a version.
- **Fork deletion and expiry** abandon the fork's `draft` and `open` changes (CHG-FR-050).
- **Metric.** `parley_changes_open{workspace}` counts the changes in state `draft` or `open` (§14).

### 7.8 Events

All events have `resource_type: "change"`, `resource_id: "<number>"`, the target workspace as `workspace`, and `session_id` when known. Every event's metadata includes `ref` (`demo#12`), `version` and `head_sha`.

| Event type | When | Further metadata |
|---|---|---|
| `parley.change.open` | A change entered `open`: it was created open, or a draft was marked ready | `title`, `target_branch`, `source_workspace`, `source_branch`, `depends_on`, `was_draft` |
| `parley.change.version` | A version was recorded (including version 1 of a draft), or the change was retargeted | `kind`, `base_sha`, `patch_equivalent_to`, `draft` |
| `parley.change.restack` | The hub restacked the change | `onto_sha`, `trigger` (`version`, `land`, `manual`, `abandon`) |
| `parley.change.conflict` | `mergeable` became `conflict`, or a restack failed | `files` (paths), `needs_restack` |
| `parley.change.ready` | The change became landable: `landing.status` is `ready` and `mergeable` is `clean` | |
| `parley.change.abandon` | Abandoned | `reason` |
| `parley.change.restore` | Restored | |

Landing, approval, check, overlap and revert events are listed in §8 and §9.

### 7.9 Configuration

| Key | Type | Default | Effect |
|---|---|---|---|
| `[changes].mergeable_debounce` | duration | `5s` | Minimum spacing of mergeability recomputation per change |
| `[changes].max_stack_depth` | int | `20` | Deepest allowed stack |

Workspace settings (§4): `changes.require_intent` (default true), `changes.auto_restack` (default true).

### 7.10 Edge cases and error handling

- **A push that rewrites the branch completely** (unrelated history) is still a new version of the same change. Its interdiff against the previous version is simply large.
- **A push that makes the source equal to or behind the target** (nothing left to land) records the version and sets `landing.status` to `blocked` with reason `nothing_to_land`. The change stays open so that the next push can revive it. This reason is derived only when a version is recorded or the change is retargeted, from the pushed head and the target at that moment. It is not derived again when the target moves, so a change whose commits reach the target later through another change is not blocked and lands as a no-op (§8).
- **The target branch is deleted.** The change stays `open` with `mergeable: unknown` and `landing.status: blocked`, reason `target_not_found`. It can be retargeted.
- **Two changes with the same commits** (one branch pushed under two names) are two changes. The second to land finds its commits already in the target and lands as a no-op (§8).
- **Stack links and force pushes.** If a dependant is force-pushed onto the target directly, its `depends_on` is cleared by the automatic rule. If the lower change is force-pushed to an unrelated head, dependants are restacked onto it by patch replay; this may conflict and set `needs_restack`.
- **Cycles** cannot arise from the automatic rule. An explicit `depends_on` that would create one is refused with `invalid_dependency`.
- **A change from a fork whose owner loses read access to the target workspace** stays open. The author can no longer read or edit it, through the target's slug or through the fork's (§3); the workspace owner can abandon it.
- **Number allocation** is gap-free under normal operation, but a failed open may consume a number. Clients MUST NOT assume numbers are contiguous.
- **Very large changes.** `paths` is capped at 2,000 entries; policy (§8) is still evaluated over every touched path.
- **Reclone of the workspace (§4).** A reclone discards the version refs and every commit that did not reach origin. Change records, versions, approvals and land attempts stay as records. A `draft` or `open` change whose source branch does not exist after the reclone is abandoned with reason `source_lost`. One whose branch came back from origin with another head gets a new version of kind `sync`. Reading the diff or the ref of a version whose commit is gone answers `404 ref_not_found`, and a change whose last version is gone cannot be restored.
