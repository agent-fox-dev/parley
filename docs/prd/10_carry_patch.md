[Parley PRD](00_overview.md) · §10

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
| `last_sync_at` | timestamp, nullable | The §4 field. On a carry-patch workspace it is written only when the whole sync, including the carry-patch part, completed (CP-FR-071, CP-FR-073). |

**Upstream base.** The commit every rebuild starts from: the head of the upstream's default branch as of the last upstream fetch. If the upstream's default branch is unknown, the hub uses the upstream branch with the same name as the workspace's `default_branch`. If neither resolves, there is no upstream base and operations that need one fail (CP-FR-045, CP-FR-063).

**Task forks.** A task fork (§4) of a carry-patch workspace is a plain fork. It has `workspace_mode: standard`, no patch list, no integration branch of its own and no rebuilds. The parent's patch branches and integration branch appear in the fork as read-only tracking branches. An agent in a fork contributes to a patch by pushing a new branch and opening a change that targets the patch branch in the parent (CP-FR-085).

#### 10.3.2 Refs the hub maintains

The hub's copy of the repository is bare. These refs are part of the contract because clients can fetch them.

| Ref | Meaning | Written by |
|---|---|---|
| `refs/heads/<integration_branch>` | The integration branch. Hub-owned: no client may create, update or delete it by any means (§5). | Rebuild, rollback |
| `refs/heads/<patch branch>` | The hub's copy of each patch branch. | Pushes, API commits, landing, sync refresh, reset to origin, branch resolution |
| `refs/remotes/upstream/*` | Upstream branches as of the last upstream fetch. Tags are not fetched. | Sync, rebuild |
| `refs/remotes/origin/*` | Origin branches as of the last origin fetch. | Sync, reset to origin, branch resolution |
| `refs/parley/replaced/<branch>` | Backup of a patch tip the hub discarded when it replaced the branch. One per branch; a later replace overwrites it. Fetchable, never pushable (§5). | Sync refresh, reset to origin |

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

A rebuild conflict is reported in the shape landing uses (§8.3.3), with every field of that shape present, plus the patch id. The example is abbreviated:

```json
{
  "patch_id": "5c0e…",
  "target_sha": "<commit the patch was applied onto>",
  "head_sha": "<tip of the patch branch used>",
  "files": [{"path": "src/net/dial.go", "kind": "content"}]
}
```

`target_sha` is the integration head under construction at the moment the patch was applied: the upstream base for the first patch, otherwise the result of the patches before it. `head_sha` is the patch's `source_sha`. `base_sha` is the merge base of the patch branch and `target_sha`. `commit_sha` is the first commit of the patch that did not apply (with strategy `merge`: the patch tip). `landed_changes` is `[]` and `change` is `null`. Each `files[]` entry has all fields of §8.3.3: `path`, `kind`, `base`, `ours` (the state under construction), `theirs` (the patch) and `landed_by` (`[]`). The report lists only the paths still in conflict after remembered resolutions were applied.

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
- CP-FR-004: After every fresh clone of a carry-patch workspace (the first clone and each reclone, §4.7.1) and before it becomes `ready`, the hub fetches upstream, turns on remembered resolutions, and creates the integration branch at the upstream base, or at the head of origin's default branch when the upstream fetch failed. A failure of any of these steps is logged and does not prevent `ready`. The next sync or rebuild repeats what is missing.
- CP-FR-005: The integration branch is created by the hub only. A workspace whose origin already has a branch with the integration branch's name is still created; the hub's integration branch does not follow origin's.

**Access, scopes and common preconditions**

- CP-FR-006: Every route in this section follows the global check order (§3). Read routes need read access to the workspace, all other routes need write access.
- CP-FR-007: PAT scopes are `patches:read` / `patches:write` for the patch list, `rebuilds:read` / `rebuilds:write` for rebuild jobs, preview and rollback, and `workspaces:read` / `workspaces:write` for remembered resolutions and the patch-status summary. 10.5 gives the scope of each route.
- CP-FR-008: After request validation, every route in this section checks, in order: the workspace is in carry-patch mode, else `400` `workspace_mode_mismatch`; for write routes and for preview, the workspace's status allows it, else the answer of WS-FR-007: `409` `workspace_busy` for `pending`, `cloning` or a running reclone, archive or delete; `409` `workspace_not_ready` for `failed`; `409` `workspace_archived` for `archived`. Read routes other than preview work in every workspace state.

**Patch add**

- CP-FR-009: The body is one patch object or an array of them (a batch). An empty array is `400`.
- CP-FR-010: `branch_name` is required, must be a valid git branch name (WS-FR-011), and must not be the integration branch. `position`, when given, must be at least 1. `if_not_exists` is refused in a batch. A branch named twice in one batch is `400`. All of this is validated before anything else happens; in a batch the message is prefixed `patch[<i>]: `.
- CP-FR-011: **Branch resolution.** Unless `skip_branch_check` is true, the branch must exist. The hub looks, in order, for: (1) the branch in the hub's copy; (2) the branch on origin as of the last origin fetch, in which case the hub creates the branch at that commit; (3) when `patch_branch_source` is `origin`, the branch on origin now, by fetching that one branch, and creates it. If none is found the answer is `404` `branch_not_found`. A failed origin fetch is `502` `origin_fetch_failed`. A branch created by resolution stays even if the request then fails. The way the branch was found is reported as `branch_resolution` in the event: `local`, `origin_tracking`, `origin_fetch` or `skipped`.
- CP-FR-012: A branch that is already registered as a non-deleted patch is `409` `patch_exists`. In a single add with `if_not_exists: true` the answer is `200` with the existing patch instead, and nothing changes.
- CP-FR-013: A soft-deleted patch with the same branch does not count as existing. It is purged as part of the add, together with its backup ref, and a new patch is created.
- CP-FR-014: **Position.** An omitted `position`, or one greater than N+1, appends the patch. Otherwise the patch takes that position and the patches at and after it move down by one. In a batch, elements with an explicit position are placed first, in array order, each into the list as it then stands; the others are appended in array order.
- CP-FR-015: A batch is all or nothing. The answer lists the created patches in request order, each with its final position.
- CP-FR-016: A new patch is `active`. Each created patch emits `parley.patch.create`.

**Patch read**

- CP-FR-017: The list returns the non-deleted patches in position order. With `include_deleted=true` the deleted patches follow, newest `deleted_at` first.
- CP-FR-018: Get returns any patch of the workspace by id, deleted or not, and adds `replaced_sha` when `refs/parley/replaced/<branch>` exists.

**Patch update**

- CP-FR-019: `PATCH …/patches/:id` accepts `status`, `position`, `description` and `upstream_pr_url`. Fields that are absent or null are left alone. An empty string clears `description` or `upstream_pr_url`. `branch_name` and unknown fields are ignored. An empty object changes nothing and answers `200`.
- CP-FR-020: `status` must be `active`, `disabled`, `merged_upstream` or `conflict`. `deleted` cannot be set here; use delete. `position` must be between 1 and the number of non-deleted patches.
- CP-FR-021: Any update of a deleted patch is `409` `patch_deleted`.
- CP-FR-022: A position change removes the patch from the order, inserts it at the new position and renumbers the list in one step.
- CP-FR-023: A status other than `conflict` clears `conflict_files`. An update that changes anything emits `parley.patch.update`. A status or position change does not queue a rebuild; the caller submits one.

**Patch delete and restore**

- CP-FR-024: Delete is a soft delete. The patch gets `status: deleted`, `deleted_at` is set, its position becomes `null`, and the remaining patches are renumbered. The patch branch and the backup ref are kept. It emits `parley.patch.delete`. Deleting a patch that is already deleted answers `204` and changes nothing.
- CP-FR-025: Restore applies to a `deleted` patch only; otherwise `409` `patch_not_deleted`. If another non-deleted patch now uses the same branch, restore is `409` `patch_exists`. The patch becomes `active`, is appended at the end, and `deleted_at` and `conflict_files` are cleared. It works the same for patches removed by a caller and for patches a rebuild removed as merged upstream. It emits `parley.patch.restore`.

**Patch reorder**

- CP-FR-026: `POST …/patches/reorder` takes `patch_ids`, which must name every non-deleted patch of the workspace exactly once. A duplicate, an unknown id, a deleted patch or a missing patch is `400`, and the message names the offending id or the two counts. The patches are renumbered in list order in one step. It emits one `parley.patch.reorder`.

**Reset to origin**

- CP-FR-027: Reset to origin forces the hub's copy of a patch branch to origin's current tip. It is allowed for patches in `active`, `conflict` or `disabled`; other statuses are `409` `patch_status_invalid`. It works whatever `patch_branch_source` and `patch_divergence_policy` say.
- CP-FR-028: The hub fetches that one branch from origin. Origin has no such branch: `409` `missing_on_origin`. Any other fetch failure: `502` `origin_fetch_failed`.
- CP-FR-029: The outcome (`action`) is the first that applies: the hub has no such branch, so it is created at origin's tip (`created`); the tips are equal and nothing is written (`none`); the hub's tip is an ancestor of origin's and the branch moves forward (`fast_forwarded`); otherwise the hub's tip is saved to `refs/parley/replaced/<branch>` and the branch is moved (`replaced`).
- CP-FR-030: Every move is a compare-and-swap against the tip the hub compared. If the branch moved in the meantime the answer is `409` `ref_moved`; the caller retries. A backup already written stays.
- CP-FR-031: When `patch_branch_source` is `origin`, the patch's origin state becomes `in_sync` with the new `origin_sha`.
- CP-FR-032: If the branch moved, the patch is `active` or `conflict`, and `auto_rebuild_after_sync` is true, a rebuild is queued (CP-FR-039). The answer carries `action`, `rebuild_triggered` and, when a job was queued, `rebuild_job_id`.
- CP-FR-033: Success emits `parley.patch.reset`, and `parley.patch.replace` when the action is `replaced`.

**Rebuild submit**

- CP-FR-034: `POST …/rebuild` queues a rebuild. The body is optional. `strategy` must be `rebase` or `merge`; `fail_mode` must be `fail_fast` or `continue`. Omitted or empty values fall back to the workspace settings `rebuild_strategy` and `rebuild_fail_mode`.
- CP-FR-035: A workspace with no patch in `active` or `conflict` has nothing to rebuild: `400` `no_active_patches`. Automatic triggers do not apply this check (CP-FR-055).
- CP-FR-036: Strategy, fail mode and integration branch are captured when the job is queued. Later changes of the settings do not affect a job that already exists.
- CP-FR-037: If the workspace already has a rebuild that is `queued`, `running` or `failed`, the answer is `409` `concurrent_rebuild` and names the existing job id in the message.
- CP-FR-038: The answer is `202` with the job record. It emits `parley.rebuild.enqueue`.
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
- CP-FR-050: If `rebuild_push_integration_branch` is true, the hub then pushes the integration branch to origin with force-with-lease against the value it last saw there. Success sets `integration_branch_pushed`. A failure emits `parley.rebuild.push_failed` and does not fail the rebuild.
- CP-FR-051: **Stale-input check.** After a publish and after a `fail_fast` conflict, the hub compares its inputs with the present. A patch is stale when its branch tip differs from the recorded `source_sha`. Upstream is stale when the upstream base differs from the one the run used. Stale patches are ignored when `auto_rebuild_after_push` is false; stale upstream is ignored when `auto_rebuild_after_sync` is false. If anything stale remains, the hub queues one follow-up rebuild with the same strategy and fail mode and emits `parley.rebuild.followup`. This follow-up is what makes it safe to skip triggers while a rebuild runs.
- CP-FR-052: A completed rebuild emits `parley.rebuild.complete`. Every failed attempt emits `parley.rebuild.fail`, with `will_retry` telling whether the job waits to retry or is dead-lettered.
- CP-FR-053: A rebuild does not block other work. Pushes, API commits, landing, sync, preview and rollback all proceed while it runs. Archive, reclone and delete of the workspace neither wait for a running rebuild nor are refused because of one (WS-FR-008). The running attempt fails at its next step and is handled per CP-FR-054; it has published nothing (CP-FR-048).
- CP-FR-054: A job whose workspace is `archived` or `failed` when an attempt starts is dead-lettered without retry. A workspace in `pending` or `cloning` is a retryable failure. The jobs of a deleted workspace are removed with it (10.3.4).
- CP-FR-055: A rebuild with no applicable patch still publishes: the integration branch moves to the upstream base. This happens when an automatic trigger fires while every patch is disabled or merged.

**Rebuild list, get, cancel, requeue**

- CP-FR-056: The list returns the rebuild jobs of the workspace, newest `created_at` first, and accepts a `status` filter.
- CP-FR-057: An unknown job id, or the id of a job of another workspace, is `404` on every job route.
- CP-FR-058: Cancel moves a `queued` job to `cancelled` and emits `parley.rebuild.cancel`. Any other status, including a job a worker claimed a moment earlier, is `409` `job_not_cancellable`.
- CP-FR-059: Requeue moves a `dead_letter` job back to `queued`, with `retry_count` 0 and `error` and `conflict` cleared, and emits `parley.rebuild.requeue`. Any other status is `409` `job_not_requeueable`. Requeue is refused with `409` `concurrent_rebuild` while another rebuild of the workspace is `queued`, `running` or `failed`. A requeued job uses the strategy and fail mode it was created with and reads fresh inputs.

**Rollback**

- CP-FR-060: Rollback moves the integration branch to the `previous_integration_head_sha` of a completed rebuild. A job that is not `completed`, or that has no previous head (the first rebuild), is `409` `rollback_unavailable`. So is a previous head whose commit no longer exists in the repository.
- CP-FR-061: The move is a compare-and-swap against the current integration head. A miss is `409` `ref_moved`.
- CP-FR-062: Rollback changes the integration branch only. Patches, positions, deleted patches and origin are untouched. The integration branch is not pushed to origin. Rollback emits `parley.rebuild.rollback`. The next rebuild that publishes replaces the rolled-back head, so an operator who wants the rollback to hold first turns off `auto_rebuild_after_push` and `auto_rebuild_after_sync`, or disables the patch at fault.

**Preview**

- CP-FR-063: Preview predicts the outcome of a rebuild against the upstream base as of the last fetch. It does not fetch. Without an upstream base the answer is `409` `upstream_unknown`; the caller syncs first.
- CP-FR-064: For the non-deleted patches in position order, leaving out `disabled` and `merged_upstream`: a patch whose branch does not exist is `would_skip`; a patch that merges cleanly onto the predicted state so far is `would_succeed` and becomes part of that state; a patch that does not is `would_conflict`, with `conflict_files` and a conflict report, and is left out of the state the later patches are tested against.
- CP-FR-065: Preview is a prediction. It tests a merge of each patch tip whatever the rebuild strategy, and it does not apply remembered resolutions. A patch reported `would_conflict` may therefore apply in a real rebuild.
- CP-FR-066: Preview changes no ref, no patch and no job, and can run at any time, including during a rebuild.

**Sync (carry-patch part)**

`POST /workspaces/:slug/sync` is defined in §4. Its authentication, scope, access, `ready` and `sync_enabled` checks apply first. On a carry-patch workspace the origin fetch and the per-branch pass of §4 (WS-FR-037 to WS-FR-047) run as usual, with one difference: the integration branch and the branches of non-deleted patches are left out of that pass and of its `branches` list. The integration branch never follows origin (CP-FR-005), and patch branches are moved only by the origin refresh below (CP-FR-069). `reset_to_origin` therefore applies to the other branches only; a diverged patch branch is governed by `patch_divergence_policy` or reset with `POST …/patches/:id/reset-to-origin`. The rest of the call behaves as follows.

- CP-FR-067: The hub fetches origin (§4) and upstream before it moves anything. A credential failure or a fetch failure of either remote is `502` `upstream_error` (WS-FR-045): `sync_error` is set, the message names the remote, and nothing is moved.
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

  A replace saves the hub's tip to `refs/parley/replaced/<branch>` first. Every move is a compare-and-swap. The refresh moves protected patch branches too; sync from origin is one of the three ways a protected branch moves (§8).
- CP-FR-070: Each outcome is stored on the patch (`origin_sync_state`, `origin_sha`, `origin_synced_at`). When `patch_branch_source` is `hub`, and for patches in `merged_upstream` or `deleted`, the three origin fields are `null`.
- CP-FR-071: If the refresh of a branch fails (for example a lost compare-and-swap), the sync stops there. Outcomes reached so far are stored and their events emitted, a rebuild is queued if a refreshed branch needs one, and the answer is `500` naming the branch. `upstream_head_sha` and `last_sync_at` are not written and merge detection does not run. The caller syncs again.
- CP-FR-072: **Merge detection** runs when upstream advanced or a patch branch moved. It examines patches in `active` and `conflict`. The setting `squash_merge_detection` selects the tests:

  | Test | Used by | A patch is merged when |
  |---|---|---|
  | Ancestry | `ancestry_only`, `both` | The patch tip is an ancestor of the upstream base. |
  | Content | `content_based`, `both` | The branch has at least one commit beyond the upstream base and every such commit has an equivalent change in upstream. |
  | PR number | `content_based`, `both` | `upstream_pr_url` contains `/pull/<n>` and an upstream commit added since the stored upstream head has a subject ending in `(#<n>)`. On the first sync the last 50 upstream commits are examined. |

  A merged patch becomes `merged_upstream`, with `parley.patch.update`. A patch whose branch is missing is not examined.
- CP-FR-073: The hub then writes `last_sync_at`, and `upstream_head_sha` when upstream advanced.
- CP-FR-074: If upstream advanced, or a branch of an `active` or `conflict` patch moved, or a patch was detected as merged, and `auto_rebuild_after_sync` is true, a rebuild is queued (CP-FR-039).
- CP-FR-075: When `patch_branch_source` is `origin`, the sync emits `parley.patch.sync` for each patch whose branch moved or whose origin state changed, and `parley.patch.replace` for each replaced branch.
- CP-FR-076: The answer is the workspace object plus the carry-patch fields listed in 10.5.

**Push control**

These rules apply to every way a client can write a ref: git push (§5) and the source control API (§6). Over git a refusal is reported per ref, in git's own format, and the whole push is refused: no ref of that push is updated (GIT-FR-021). Over the API it is `403` with the `error_type` given.

- CP-FR-077: **Integration branch.** A create, update or delete of `refs/heads/<integration_branch>` on a carry-patch workspace is always refused (`integration_branch`), whatever the settings, with `integration branch <name> is maintained by the hub; push a patch branch and rebuild instead`. The integration branch cannot be the target of a change (`403` `integration_branch`, CHG-FR-005).
- CP-FR-078: **Backup refs.** `refs/parley/replaced/*` cannot be written by clients (§5).
- CP-FR-079: The rules below apply to the branch of a non-deleted patch.
- CP-FR-080: `patch_branch_source: hub`: the update is accepted, subject to branch protection (CP-FR-084).
- CP-FR-081: `patch_branch_source: origin` and `push_patches_to_origin: false`: every create, update and delete is refused (`patch_branch`) with `branch is synced from origin; push to <git_url> instead`. The URL is shown without credentials.
- CP-FR-082: `patch_branch_source: origin` and `push_patches_to_origin: true`: the hub forwards the update to origin before accepting it, as a non-forced push with a 120 s limit. If origin accepts, the update is accepted on the hub. If origin refuses or cannot be reached, the update is refused (`patch_branch`) with `origin rejected push: <reason>` (credentials removed), or with `failed to resolve origin credentials` when the origin credential cannot be read. A delete is refused (`patch_branch`) with `branch is synced from origin; delete it on origin instead`. Over git the forward runs last, and a failed forward refuses the whole push (GIT-FR-031).
- CP-FR-083: If the hub cannot decide (for example the patch list cannot be read), the update is refused with a retryable error. Push control never lets an update through unchecked.

**Protected patch branches and changes**

- CP-FR-084: A patch branch may be listed in the workspace's `protected_branches` (§8). A direct update of it is then refused by §5 and §8, before the rules above are consulted. The one exception is a direct push under the owner exemption (GIT-FR-034): it passes the protection, and the rules above then apply to it.
- CP-FR-085: A change (§7) may target a patch branch, from the same workspace or from a task fork. It follows the normal change life cycle: versions, policy, approvals, checks.
- CP-FR-086: Landing a change on a patch branch (§8) updates the patch branch and has the same consequences as an accepted push to it: the triggers of CP-FR-087 and CP-FR-088 run. When `patch_branch_source` is `origin`, landing obeys CP-FR-081 and CP-FR-082: without forwarding the land attempt ends `blocked` with that reason (§8), and with forwarding the new head is forwarded to origin before the target moves; if origin refuses or cannot be reached, the attempt ends `failed` with `error: "origin_rejected"` and the target does not move.

**Triggers after an update of a patch branch**

- CP-FR-087: After any accepted update of the branch of a non-deleted patch that is not a delete, and when `auto_rebuild_after_push` is true, the hub queues a rebuild (CP-FR-039). The trigger runs after the client has its answer and never changes the outcome of the push, commit or landing.
- CP-FR-088: **Mirror.** When `patch_branch_source` is `hub` and `push_patches_to_origin` is true, the hub then pushes each updated patch branch to origin, with force-with-lease against the value it last saw there and a 120 s limit. A failure emits `parley.patch.mirror_failed` for that branch and does not stop the others. The mirror does not depend on `auto_rebuild_after_push`. Forwarding, the mirror and `rebuild_push_integration_branch` (CP-FR-050) are the only ways these branches reach origin: `POST /workspaces/:slug/branches/:name/push` refuses the integration branch and registered patch branches with `403` `integration_branch` or `patch_branch` (WS-FR-053).

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
- CP-FR-097: **Purge.** The hourly maintenance loop (§3) removes every patch that has been `deleted` for more than 7 days, together with `refs/parley/replaced/<branch>` unless a non-deleted patch uses the same branch. A restore that happens at the same moment wins. Failures are logged and retried on the next pass.

### 10.5 REST API

All paths are relative to `/api/v1`. Errors use the error envelope (§3). The errors below are common to every route and are not repeated: `401`; `403` (scope, or a read-only member on a write route); `404` `workspace not found`; `400` `workspace_mode_mismatch`; on write routes and preview the status answers of WS-FR-007 (`409` `workspace_busy`, `workspace_not_ready` or `workspace_archived`).

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
- **Errors:** `400` `invalid_request` (CP-FR-009, CP-FR-010); `404` `branch_not_found`; `409` `patch_exists`; `502` `origin_fetch_failed`.

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
  | `origin_fetched` | boolean | Always `true`: every sync fetches origin (§4). Kept for clients that test it. |
  | `patches_merged` | string[] | Branch names detected as merged in this sync. |
  | `patches_synced` | object[] | One entry per refreshed candidate: `branch_name`, `action`, `state`, `local_sha`, `origin_sha`, `replaced_sha` (nullable). `[]` when `patch_branch_source` is `hub`. |
  | `patches_diverged` | string[] | Branch names with state `diverged`. |
  | `rebuild_triggered` | boolean | |
  | `rebuild_job_id` | string, nullable | Set when a job was queued. |

- **Errors added:** `502` `upstream_error` also when the upstream fetch fails; `500` `internal` (refresh failure, CP-FR-071).

### 10.6 CLI

The global CLI conventions apply (§3): JSON on stdout, the error envelope on stderr, exit codes 0, 1, 2 and 3. Unless stated, a command prints the hub's response.

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
- **Flags:** `--wait`, `--timeout`, `--poll-interval`. With `--wait`, and when the response names a `rebuild_job_id`, the command waits for that job as `parley rebuild submit --wait` does, and prints one document: the response with one more member, `rebuild`, holding the final job record.
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
- **Metric.** `parley_jobqueue_depth{type,status}` (§14) is the number of durable jobs per job type (`type`: `rebuild`, the only job type in this version) and per job status (`status`: `queued`, `running`, `failed`, `completed`, `dead_letter`, `cancelled`).

### 10.8 Events

All events follow the envelope of §14. Rebuild events have `resource_type: rebuild` and the job id as `resource_id`. Patch events have `resource_type: patch` and the patch id as `resource_id`. The actor is the caller, or `system` for work the hub does on its own. Each event is written with the state change it reports (§14).

| Event | When | Actor | Metadata |
|---|---|---|---|
| `parley.patch.create` | A patch was created (single add and each element of a batch) | caller | `branch_name`, `position`, `branch_resolution` |
| `parley.patch.update` | A patch's status, position, description or PR URL changed | caller, or `system` | `branch_name`, `changed` (field names), and for a status change `from`, `to`, `reason` (`api`, `rebuild_conflict`, `rebuild_clean`, `merged_upstream`) |
| `parley.patch.reorder` | The list was reordered. One event; `resource_id` is the first patch of the new order | caller | `patch_ids` (new order) |
| `parley.patch.delete` | A patch was soft-deleted | caller, or `system` when a rebuild removed a merged patch | `branch_name`, `reason` (`api`, `merged_upstream`) |
| `parley.patch.restore` | A patch was restored | caller | `branch_name`, `position` |
| `parley.patch.reset` | Reset to origin succeeded, whatever the action | caller | `branch_name`, `action`, `local_sha`, `origin_sha`, `replaced_sha` |
| `parley.patch.replace` | The hub replaced a diverged patch tip and wrote a backup ref | caller | `branch_name`, `replaced_sha`, `origin_sha`, `trigger` (`sync`, `reset_to_origin`) |
| `parley.patch.sync` | An origin refresh moved a patch branch or changed its origin state | caller | `branch_name`, `action`, `state`, `local_sha`, `origin_sha` |
| `parley.patch.mirror_failed` | The mirror of a patch branch to origin failed | `system` | `branch_name`, `error` |
| `parley.rebuild.enqueue` | A rebuild job was queued, from any source | caller, or `system` | `submitted_by`, `strategy`, `fail_mode`, `patch_count` |
| `parley.rebuild.complete` | A rebuild published | `system` | `upstream_head_sha`, `integration_head_sha`, `previous_integration_head_sha`, `patches_applied`, `patches_skipped`, `patches_conflicted`, `patches_removed` |
| `parley.rebuild.fail` | An attempt failed | `system` | `reason`, `will_retry`, `retry_count`, `conflict` (the conflict report, when the failure was a conflict) |
| `parley.rebuild.push_failed` | The push of the integration branch to origin failed | `system` | `integration_branch`, `reason` |
| `parley.rebuild.followup` | The stale-input check queued a follow-up. `resource_id` is the job that ended | `system` | `follow_up_job_id`, `stale_patches` (branch names), `upstream_stale` |
| `parley.rebuild.cancel` | A queued job was cancelled | caller | none |
| `parley.rebuild.requeue` | A dead-lettered job was requeued | caller | none |
| `parley.rebuild.rollback` | The integration branch was rolled back | caller | `integration_branch`, `rolled_back_to`, `previous_head_sha` |

Updates of a patch branch and of the integration branch also produce the general `parley.git.push` and change events of §5, §7 and §8. Events caused by a session carry its `session_id`; a rebuild queued by a push or a landing carries the session of that push or landing.

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

**Server.** The committer of the commits a rebuild creates is the hub's landing identity, `[land].committer_name` and `[land].committer_email` (§8; default `Parley <parley@localhost>`). It is not taken from the process environment (NFR-023).

**Fixed values.** The retry policy, the job concurrency, the shutdown grace period, the git and push time limits, the 7-day retention of deleted patches and the 50-commit window of merge detection are not configurable in this version.

### 10.10 Edge cases and error handling

- **Rollback after patches were removed.** Rollback restores the integration branch only. Patches a later rebuild soft-deleted as merged stay deleted; restore them one by one if needed.
- **`fail_fast` conflict.** The job is `dead_letter` with a conflict report. The integration branch is unchanged. The patch is `conflict`. After the branch is fixed, a push queues a new rebuild (if `auto_rebuild_after_push` is true); the dead-lettered job may also be requeued.
- **Deleting a patch branch.** With `patch_branch_source: hub` a client may delete an unprotected patch branch. The patch stays registered and is skipped until the branch returns. A delete does not trigger a rebuild.
- **Push to the branch of a deleted patch.** It is an ordinary branch: push control and the rebuild trigger do not apply.
- **Upstream force-push.** Sync reports `force_push_detected` and proceeds. The next rebuild starts from the new base. Merge detection by PR number falls back to the commits reachable from the new base.
- **Upstream unreachable.** A rebuild fails with a retryable error and waits to retry, for up to 20 retries. Preview keeps working from the last fetched base.
- **Setup incomplete.** If the upstream fetch failed during creation, the integration branch sits at origin's default branch head until the first successful rebuild moves it to "upstream base plus patches".
- **Origin has an integration branch of its own.** The hub does not read it. With `rebuild_push_integration_branch` the hub pushes with force-with-lease; if someone else moved the branch on origin since the hub last saw it, the push fails with `parley.rebuild.push_failed` and the hub's integration branch is still correct.
- **Task fork of a carry-patch workspace.** Every route in this section answers `400` `workspace_mode_mismatch` on the fork. The fork's tracking copies of the patch branches and of the integration branch follow the parent.
- **Workspace archived, recloned or deleted.** Write routes answer `409` `workspace_archived` on an archived workspace and `409` `workspace_busy` while a reclone, an archive or a delete runs (WS-FR-007). A queued job for an archived workspace is dead-lettered without retry; the jobs of a deleted workspace are removed with it (10.3.4). After a reclone, patches, jobs and settings remain; remembered resolutions and backup refs do not survive the reclone.
- **One hub process.** Dedup and "one rebuild per workspace" are guaranteed for one `parleyd` process on a data directory (§15). Running two processes on the same data directory is not supported.
