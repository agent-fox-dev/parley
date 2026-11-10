---
spec_id: "36"
spec_name: "carry_patch_sync"
title: "Sync a carry-patch workspace with origin and upstream"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "e37b82484872fcc2a0f5c21d7efde4726fdc04b8251cd2840e01ffde8bfc8c25"
schema_version: 3
source: "docs/prd/10_carry_patch.md"
---

# Sync a carry-patch workspace with origin and upstream

## Intent

A team that carries patches on top of a project it does not control needs one call that tells the hub what changed outside it. On a carry-patch workspace, sync is that call: it fetches upstream as well as origin, brings patch branches in line with origin when origin is their authority, recognises patches that upstream has merged, and queues the rebuild that follows, so that the integration branch is rebuilt from current inputs without anyone watching for upstream movement by hand.

## Goals

- `POST /api/v1/workspaces/:slug/sync` on a carry-patch workspace fetches origin and upstream before it moves anything; a failure of either answers `502 upstream_error` naming the remote, with nothing moved.
- The per-branch pass of the general sync never compares, moves or lists the branch of a non-deleted patch.
- With `patch_branch_source: origin`, every patch in `active`, `conflict` or `disabled` gets exactly one refresh outcome (`created`, `fast_forwarded`, `replaced` or `none`, with state `in_sync`, `diverged` or `missing_on_origin`), stored on the patch and reported in `patches_synced`; a replaced tip stays reachable at `refs/parley/replaced/<branch>`.
- Patches in `active` or `conflict` that upstream has merged (by ancestry, by equivalent content or by pull request number, as `squash_merge_detection` selects) become `merged_upstream` and are named in `patches_merged`.
- A sync that saw upstream advance, a patch branch move or a patch merge queues one rebuild when `auto_rebuild_after_sync` is true, and reports it as `rebuild_triggered` and `rebuild_job_id`.
- A refresh that fails part-way answers `500` naming the branch, keeps what was done, and leaves `upstream_head_sha` and `last_sync_at` unwritten so that the next sync repeats the rest.
- The answer is the general sync answer plus eight members: `upstream_advanced`, `force_push_detected`, `origin_fetched`, `patches_merged`, `patches_synced`, `patches_diverged`, `rebuild_triggered`, `rebuild_job_id`.

## Non-goals

- The sync route itself, its authentication, scope, access and state checks, the origin fetch, the per-branch pass, `reset_to_origin`, tags, `last_sync_at`, `sync_error`, the `parley.workspace.sync` event, the excluded-branch provider interface, the branch-move observers and the command `parley workspace sync` with `--wait` and `--fail-on-diverged` (WS-FR-036 to WS-FR-048): package 12 `workspace_sync_from_origin`. This package registers a provider there and adds the carry-patch part that runs inside that sync.
- Carry-patch workspace creation, the upstream remote, the upstream credential order (CP-FR-003), the set-up after a clone, the patch record with its `origin_sync_state`, `origin_sha` and `origin_synced_at` fields, patch add and patch read (CP-FR-001 to CP-FR-018): package 31 `carry_patch_setup_and_patches`.
- Patch update, delete, restore, reorder and `POST …/patches/:id/reset-to-origin` (CP-FR-019 to CP-FR-033): package 32 `patch_maintenance`.
- The durable job model, the enqueue with its dedup, `parley.rebuild.enqueue` and the rebuild routes (§10.3.4, CP-FR-034 to CP-FR-039, CP-FR-056 to CP-FR-059): package 33 `rebuild_jobs`. This package calls that enqueue.
- What a rebuild does, including the soft delete of `merged_upstream` patches and the stale-input follow-up (CP-FR-040 to CP-FR-055): package 34 `rebuild_execution`.
- Rollback and preview (CP-FR-060 to CP-FR-066): package 35 `rebuild_rollback_and_preview`.
- Push control, forwarding and mirroring patch branches to origin, and the triggers after a client updates a patch branch (CP-FR-077 to CP-FR-088): package 37 `carry_patch_push_control`. A refresh by sync is not a client update and runs none of those triggers.
- The 10-minute limit of one git operation, the `https`/`ssh` rule for remotes, option-injection safety, remembered resolutions and the purge of deleted patches (CP-FR-089 to CP-FR-097): package 38 `carry_patch_rerere_and_maintenance`.
- The settings document and the validation of `patch_branch_source`, `patch_divergence_policy`, `auto_rebuild_after_sync`, `rebuild_strategy`, `rebuild_fail_mode` and `squash_merge_detection`: package 11 `workspace_read_update_settings`. This package reads their effective values.
- Recording a new version of a change whose source branch moved: package 22 `change_opening_and_versions`, through the branch-move observers of package 12.
- Serving `refs/parley/replaced/*` to git clients and refusing pushes to it: packages 17 `git_smart_http` and 18 `git_push_rules`.
- The event log: package 52 `durable_event_log_and_stream`. This package emits through it.
- A scheduled sync. The hub does not sync a carry-patch workspace on its own (§10.7).

## Background

The repository holds no code yet; every file named here is new. This package changes no file of `internal/workspace/`: it registers with what package 12 declares there. The source is `docs/prd/10_carry_patch.md`: the paragraph that opens "Sync (carry-patch part)" in §10.4, CP-FR-067 to CP-FR-076, the `POST /workspaces/:slug/sync` entry of §10.5, the rows `parley.patch.sync`, `parley.patch.replace` and `parley.patch.update` of §10.8, the settings of §10.9 and the "Upstream force-push" entry of §10.10. WS-FR-048 of `docs/prd/04_workspaces_and_forks.md` hands this part of sync to §10.

Package 12 gives a sync that fetches origin, walks the branches origin has, records completion and answers. It declares an excluded-branch provider (the branch names to leave out of the walk on a carry-patch workspace), branch-move observers, and a sync extension point with two hooks for a carry-patch workspace, and registers none of them. Package 31 gives the patch list and the upstream fetch with its credentials; package 33 (reached through package 34) implements the rebuild enqueue behind the interface `RebuildEnqueuer` of package 31; package 32 gives the function that writes a backup ref.

The code of this package lives in `internal/carrypatch/`: `sync.go` (the carry-patch part of a sync and its answer members), `sync_refresh.go` (origin refresh), `merge_detect.go` (merge detection) and `excluded.go` (the provider), each with its test file beside it. `internal/gitrepo/cherry.go` adds the two git reads merge detection needs. Registration with package 12 is in `internal/hub/`. End-to-end tests are in `test/e2e/carry_patch_sync_test.go`.

## Requirements

### Branches left out of the general pass

WS-FR-048 and the opening paragraph of the sync part of §10.4. On a carry-patch workspace the per-branch pass of package 12 leaves out the integration branch and the branches of non-deleted patches. This package supplies the second set: the provider returns the `branch_name` of every patch of the workspace whose status is `active`, `conflict`, `disabled` or `merged_upstream`, and no deleted patch. The caller observes that such a branch is not compared, not moved and not in `branches`, whatever `patch_branch_source` is, and that `reset_to_origin=true` does not reset a diverged patch branch. The branch of a deleted patch is an ordinary branch and is synced like any other.

If the patch list cannot be read, the provider returns an error, and package 12 answers `500` with `error_type` `internal`, having moved nothing.

### Fetching upstream

CP-FR-067. The checks of package 12 come first; a refused request (for example `400 sync_disabled` or `409 workspace_archived`) contacts neither remote. Then the hub fetches origin, and after that fetch succeeded and before any branch is created or moved it fetches upstream, with the upstream credentials resolved at that moment (CP-FR-003, package 31). The fetch updates `refs/remotes/upstream/*` and fetches no tags (§10.3.2).

Failures, each `502` with `error_type` `upstream_error`:

| Failure | Message and `sync_error` |
|---|---|
| The origin fetch fails (package 12) | contain the word `origin`; upstream is not contacted |
| The upstream credentials cannot be read, or the upstream fetch fails | contain the word `upstream` |
| The upstream fetch succeeded and no upstream base resolves (§10.3.1) | contain `upstream base cannot be resolved` |

In each case `sync_error` is set to the message, no branch under `refs/heads/` is created or moved, no patch field, `upstream_head_sha` or `last_sync_at` is written, no event is emitted and no rebuild is queued. The message contains no credential and no URL user information (PLAT-FR-098).

### Upstream movement

CP-FR-068, CP-FR-073. The upstream base is the head of upstream's default branch as of this fetch, or of the upstream branch named like the workspace's `default_branch` (§10.3.1).

- `upstream_advanced` is `true` when the upstream base differs from the stored `upstream_head_sha`. It is `true` on the first sync, when nothing is stored.
- `force_push_detected` is `true` when upstream advanced, a head is stored, and that head is not an ancestor of the new base (this includes a stored head whose commit no longer exists). It is `false` on the first sync. A force-push does not fail the sync (§10.10).
- A sync that completes writes `upstream_head_sha` to the new base when upstream advanced, together with `last_sync_at`. When upstream did not advance, `upstream_head_sha` keeps its value and `last_sync_at` is still written. The workspace object in the answer shows the values after the write.

### Origin refresh

CP-FR-069. The refresh runs only when `patch_branch_source` is `origin`, after the per-branch pass. For every patch in `active`, `conflict` or `disabled`, in position order, it compares the hub's tip of the patch branch with origin's tip as of this sync's fetch and applies the first rule that fits:

| Situation | `action` | `state` |
|---|---|---|
| Origin has no such branch | `none` | `missing_on_origin` |
| The hub has no such branch | `created` | `in_sync` |
| Tips are equal | `none` | `in_sync` |
| The hub's tip is an ancestor of origin's | `fast_forwarded` | `in_sync` |
| Otherwise, `patch_divergence_policy` `report` | `none` | `diverged` |
| Otherwise, `patch_divergence_policy` `replace` | `replaced` | `in_sync` |

A replace first saves the hub's tip to `refs/parley/replaced/<branch>`, overwriting an earlier backup of that branch, then moves the branch to origin's tip. Every create and move is a compare-and-swap that expects the tip the hub compared (for a create: that the branch does not exist). The refresh moves protected patch branches too: sync is one of the three ways a protected branch moves, and no push rule is consulted. Patches in `merged_upstream` or `deleted` are not refreshed. With `patch_branch_source` `hub` no patch branch is compared or moved.

Every branch the refresh creates or moves is reported to the branch-move observers of package 12, with cause `sync`, as that package's own moves are; an observer's failure does not fail the sync.

### Stored origin state and events

CP-FR-070, CP-FR-075. Each outcome is stored on the patch: `origin_sync_state` (the `state`), `origin_sha` (origin's tip, `null` when the branch is missing on origin) and `origin_synced_at` (the time of the comparison). After a sync with `patch_branch_source` `hub` the three fields are `null` on every patch of the workspace. Under `origin` they are `null` on every patch in `merged_upstream`, including one this sync detected as merged.

For each refreshed patch whose branch moved or whose `origin_sync_state` differs from the value stored before the sync, the hub emits `parley.patch.sync` (`resource_type` `patch`, `resource_id` the patch id, actor the caller, metadata `branch_name`, `action`, `state`, `local_sha`, `origin_sha`). A patch that was `in_sync` and is `in_sync` again with action `none` emits nothing. For each replaced branch the hub also emits `parley.patch.replace` (metadata `branch_name`, `replaced_sha`, `origin_sha`, `trigger` `sync`). Each event is written in the same store transaction as the patch fields it reports (§10.8). Setting the fields to `null` emits no event.

### Refresh failure

CP-FR-071. If the refresh of one branch fails (a lost compare-and-swap, a git failure, a failed store write), the sync stops there:

- the outcomes of the patches before it stay stored and their events stay emitted; a backup ref already written stays;
- the patches after it are not refreshed;
- merge detection does not run; `upstream_head_sha`, `last_sync_at` and `sync_error` are not written and `parley.workspace.sync` is not emitted;
- a rebuild is queued, as in "Automatic rebuild", when a branch of an `active` or `conflict` patch was moved before the failure and `auto_rebuild_after_sync` is true;
- the answer is `500` with `error_type` `internal` and a message that contains the branch name.

The lost compare-and-swap is not retried inside the call. The caller syncs again; because `upstream_head_sha` was not written, that sync still reports `upstream_advanced` when upstream had advanced.

### Merge detection

CP-FR-072. Merge detection runs when upstream advanced or the refresh of this sync created or moved a patch branch; otherwise it does not run and `patches_merged` is `[]`. It examines the patches in `active` and `conflict`, in position order. A patch whose branch does not exist in the hub is not examined. `squash_merge_detection` selects the tests, and a patch is merged when at least one selected test holds:

| Test | Used by | A patch is merged when |
|---|---|---|
| Ancestry | `ancestry_only`, `both` | The patch tip is the upstream base or an ancestor of it. |
| Content | `content_based`, `both` | The branch has at least one non-merge commit that is not in the upstream base, and every such commit has a commit with the same patch id in upstream (`git cherry <upstream base> <patch tip>` prints at least one line and every line starts with `-`). |
| PR number | `content_based`, `both` | `upstream_pr_url` contains `/pull/<n>` with `<n>` decimal digits, and an examined upstream commit has a subject line that ends with `(#<n>)`. The examined commits are those reachable from the new base and not from the stored upstream head. On the first sync, and when `force_push_detected` is true, they are the 50 newest commits reachable from the new base (§10.10). |

A merged patch becomes `merged_upstream`: `conflict_files` is cleared (§10.3.3), the three origin fields become `null`, the branch name is added to `patches_merged`, and the hub emits `parley.patch.update` with the caller as actor and metadata `branch_name`, `changed` `["status"]`, `from`, `to` `merged_upstream`, `reason` `merged_upstream`. Sync never moves a patch out of `merged_upstream`. The patch is soft-deleted later by the rebuild that publishes (package 34).

### Automatic rebuild

CP-FR-074, CP-FR-039. After merge detection the hub queues one rebuild when `auto_rebuild_after_sync` is `true` and at least one of these holds: upstream advanced; the refresh created or moved the branch of a patch that is `active` or `conflict`; a patch was detected as merged. The job is queued through the enqueue of package 33 with `submitted_by` the caller's user id and the strategy and fail mode of the workspace settings; the answer then has `rebuild_triggered: true` and the job id in `rebuild_job_id`.

- When none of the three holds, or `auto_rebuild_after_sync` is `false`, nothing is queued: `rebuild_triggered: false`, `rebuild_job_id: null`.
- When the workspace already has a rebuild that is `queued`, `running` or `failed`, the trigger is skipped: `rebuild_triggered: false`, `rebuild_job_id: null`, and the sync still answers `200`.
- The `no_active_patches` check of `POST …/rebuild` does not apply: a sync of a workspace whose patches are all `disabled` or `merged_upstream` still queues the rebuild (CP-FR-035, CP-FR-055).
- A branch move of a `disabled` patch alone queues nothing.
- If the enqueue fails for another reason, the answer is `500` with `error_type` `internal`, and `upstream_head_sha` and `last_sync_at` are not written.

### The answer

CP-FR-076, §10.5. `200` with the answer of package 12 (the workspace object and `branches`) plus:

| Field | Type | Meaning |
|---|---|---|
| `upstream_advanced` | boolean | |
| `force_push_detected` | boolean | |
| `origin_fetched` | boolean | Always `true`. |
| `patches_merged` | string[] | Branch names detected as merged in this sync, in position order. |
| `patches_synced` | object[] | One entry per refreshed patch, in position order: `branch_name`, `action`, `state`, `local_sha` (the hub's tip after the refresh, `null` when the hub has no such branch), `origin_sha` (`null` when missing on origin), `replaced_sha` (the saved tip when `action` is `replaced`, else `null`). `[]` when `patch_branch_source` is `hub`. |
| `patches_diverged` | string[] | Branch names with state `diverged`, in position order. |
| `rebuild_triggered` | boolean | |
| `rebuild_job_id` | string, nullable | Set when a job was queued. |

Commit ids are full 40-character ids. On a workspace with `workspace_mode` `standard` none of the eight members is present and upstream is never contacted. Like the rest of the sync, the carry-patch part runs to completion when the caller disconnects (WS-FR-046).

### End-to-end flows

1. A developer has a carry-patch workspace with patches `patch/a` and `patch/b` (`patch_branch_source` `hub`). Upstream merges the commits of `patch/a` and gains other commits. The developer runs `parley workspace sync cp --wait`; the parley CLI posts the sync; `parleyd` fetches origin and upstream, reports `upstream_advanced`, marks `patch/a` `merged_upstream`, writes `upstream_head_sha`, queues a rebuild and answers; the rebuild runs, publishes the integration branch as the new upstream base plus `patch/b` and soft-deletes `patch/a`; the CLI prints the sync answer with the final job record and exits 0.
2. An agent works on a carry-patch workspace with `patch_branch_source` `origin`. On origin, `patch/a` gains a commit and `patch/b` is rewritten. The agent posts the sync; `parleyd` fast-forwards `patch/a`, saves the hub's tip of `patch/b` to `refs/parley/replaced/patch/b` and moves `patch/b` to origin's tip, stores `in_sync` on both, emits `parley.patch.sync` twice and `parley.patch.replace` once, and queues a rebuild. The agent reads `GET /workspaces/cp/patches/<id of patch/b>` and sees `origin_sync_state` `in_sync` and `replaced_sha`. With `patch_divergence_policy` `report` instead, the same rewrite leaves `patch/b` untouched, the answer lists it in `patches_diverged`, and `parley workspace sync cp --fail-on-diverged` exits 3.

## Design Decisions

1. **Profile: full.** Nine requirements and two end-to-end flows across the CLI, the sync route, the carry-patch component, git, the store and the job queue.
2. **The carry-patch part registers with the sync extension point of package 12.** Package 12 declares, in `internal/workspace`, the excluded-branch provider, the branch-move observers and an extension point with two hooks, called only for a workspace whose `workspace_mode` is `carry_patch`. The first hook runs after the origin fetch succeeded and before anything is created or moved; its failure is handled as package 12 handles a failed fetch (`502 upstream_error`, `sync_error` set). The second runs after the per-branch pass and before the completion write; it returns the extra answer members and the value of `upstream_head_sha` to write in the completion transaction, and its failure answers `500 internal` without the completion write or the `parley.workspace.sync` event. This package implements both hooks in `internal/carrypatch` and registers them from `internal/hub/`; it edits nothing in `internal/workspace`, which does not import `internal/carrypatch`.
3. **Order inside one sync:** checks, origin fetch, upstream fetch, per-branch pass, origin refresh, merge detection, rebuild enqueue, completion write, answer. The PRD fixes "both fetches before anything moves" and "refresh before merge detection" and leaves the rest open. If the per-branch pass itself fails with `500` (package 12), the carry-patch part does not run.
4. **The rebuild is queued before the completion write**, although CP-FR-073 is listed before CP-FR-074. Nothing a successful caller observes depends on the order, and a failed enqueue then leaves `upstream_head_sha` unwritten, so the next sync sees upstream advanced again and queues the rebuild. The PRD does not say what a failed enqueue answers; `500 internal` is chosen.
5. **No upstream base after a successful upstream fetch is `502 upstream_error`**, handled like a failed upstream fetch. The PRD says only that operations that need a base fail; sync needs one to compute `upstream_advanced`.
6. **After a successful origin fetch and a failed upstream fetch, `refs/remotes/origin/*` keep the values the origin fetch gave them.** CP-FR-067 says nothing is moved; the origin heads are records of what origin has, not branches of the workspace.
7. **`force_push_detected` is `false` on the first sync**, because there is no stored head to compare with.
8. **"A patch branch moved" means: created or moved by the origin refresh of this sync.** Moves by pushes since the last sync are covered by the push trigger (package 37) and are not visible to sync.
9. **A lost compare-and-swap in the refresh is not retried**, unlike the per-branch pass of package 12 (which re-evaluates up to 5 times). CP-FR-071 names it as the example of a failure that stops the sync.
10. **Ancestry includes equality.** A patch branch whose tip is the upstream base itself counts as merged, as `git merge-base --is-ancestor` reports it.
11. **Content equivalence is git's patch id, per commit**, as `git cherry` computes it; merge commits on the patch branch are left out, as they are in a rebuild (CP-FR-042). A squash of several commits is found by the PR number test.
12. **PR number window under a force-push: the 50 newest commits of the new base**, the same window as on the first sync. §10.10 says detection "falls back to the commits reachable from the new base" and gives no bound; the 50-commit window is the only bound the PRD has (§10.9).
13. **Merge detection in a sync has the caller as event actor**, because the caller asked for the sync; §10.8 allows "caller, or `system`".
14. **A sync with `patch_branch_source` `hub` sets the three origin fields of every patch to `null`**, and so does a patch reaching `merged_upstream`. CP-FR-070 states the values and not who writes them; sync is the only writer of these fields besides reset to origin. Clearing emits no `parley.patch.sync`, which describes a refresh outcome.
15. **Patch branch moves are reported by `parley.patch.sync`, not in the `updated` list of `parley.workspace.sync`**, which covers the per-branch pass of package 12.
16. **The eight members are absent on a standard workspace.** Package 12's command tests whether `patches_diverged` is present.
17. **The backup ref of a replace is written by `WriteBackupRef` of package 32** (`internal/carrypatch/backup_ref.go`), the one function that writes `refs/parley/replaced/<branch>`; reset to origin uses the same. Package 32 is a dependency for that reason (it depends on packages 31, 09 and 52 only, so no cycle arises).
18. **The rebuild is queued through `RebuildEnqueuer`**, the interface package 31 declares and package 33 implements; the same call is used by reset to origin (package 32), push control (package 37) and the follow-up (package 34). `parley workspace sync --wait` (package 12) waits on `GET /workspaces/:slug/rebuilds/:id`, the route behind `parley rebuild status` (package 33).
19. **The upstream fetch of a sync is `EnsureSetup` of package 31**, which also repeats a set-up step that failed after the clone (remembered resolutions on, the integration branch created when absent and never moved). Only a failure of its fetch step fails the sync.
20. **Git runs through the carry-patch git runner of package 31**, which holds the 10-minute limit of CP-FR-093 and cleans error text; the limit ends a sync through the hook's error.
21. **Project decisions relied on:** Go; git operations run the `git` binary (2.38 or later) as a subprocess through `internal/gitrepo`; the store is embedded SQLite behind `internal/store`; HTTP is `net/http`; tests use the standard `testing` package, table-driven, with `net/http/httptest`, repositories made by `git init` in `t.TempDir()`, test remotes served by `internal/gitrepo/gitrepotest/`, and a replaceable clock.

## Dependencies

- Package 31 `carry_patch_setup_and_patches` (draft): the carry-patch workspace fields, the upstream remote, the upstream fetch with its credential order, the upstream base, the patch list and the patch record with its origin fields.
- Package 12 `workspace_sync_from_origin` (draft): the sync route and operation, the origin fetch, the per-branch pass, the completion write, the excluded-branch provider interface, the branch-move observers, the two-hook extension point and the command.
- Package 34 `rebuild_execution` (draft), and through it package 33 `rebuild_jobs`: the implementation of the rebuild enqueue with its dedup, and the rebuild that the first end-to-end flow waits for.
- Package 32 `patch_maintenance` (draft): `WriteBackupRef`. Added to the plan's list so that the backup ref has one writer.
- Package 52 `durable_event_log_and_stream` (draft): emitting `parley.patch.sync`, `parley.patch.replace` and `parley.patch.update`. Added to the plan's list by the brief's rule for packages that emit events.
- Package 11 `workspace_read_update_settings` (draft): the effective values of the carry-patch settings, and `PATCH /workspaces/:slug`, with which the end-to-end tests set them. Added to the plan's list for that reason.
- External system: the `git` command, 2.38 or later, on the server.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Origin**: A registered workspace's remote (`git_url`).
- **Carry-patch workspace**: A workspace that tracks an upstream project and keeps an ordered list of patches on top of it.
- **Upstream**: For a carry-patch workspace, the project it tracks (`upstream_url`).
- **Upstream base**: The commit every rebuild starts from: the head of the upstream's default branch as of the last upstream fetch (§10.3.1).
- **Integration branch**: For a carry-patch workspace, the branch the hub rebuilds as upstream plus the ordered patches.
- **Patch**: A carry-patch entry: a branch with a position in the patch order and a status.
- **Patch branch**: The branch of a patch.
- **Rebuild**: A durable job that recreates the integration branch.
- **Durable job**: Background work that is recorded before its request returns, survives restarts and is retried with backoff.
- **Protected branch**: A branch that moves only by landing a change, by sync from origin, or by a direct fast-forward push from the workspace's owner or the admin token (§8). Nobody can force-push or delete it. By default the default branch.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number.
