---
spec_id: "32"
spec_name: "patch_maintenance"
title: "Update, remove, reorder and reset patches"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "15978654303daa6431ac6a05c650806e82aab57bb2d4d0699533fac4c78b2cbd"
schema_version: 3
source: "docs/prd/10_carry_patch.md"
---

# Update, remove, reorder and reset patches

## Intent

A team that carries patches on top of an upstream it does not control has to keep the patch list in step with reality: a patch is switched off, described, moved, dropped, brought back, or its branch is forced back to what origin holds. This package lets a developer or an agent do each of those in one call, over the REST API or from the shell, with the patch order always gap-free, every change recorded as a hub event, and nothing a caller removes lost before it can be restored.

## Goals

- `PATCH /api/v1/workspaces/:slug/patches/:id` changes a patch's status, position, description and upstream pull request URL, and changes only the fields the body names.
- `DELETE /api/v1/workspaces/:slug/patches/:id` soft-deletes a patch and `POST …/patches/:id/restore` brings it back as the last active patch; the patch branch and its backup ref survive both.
- `POST /api/v1/workspaces/:slug/patches/reorder` renumbers the whole list in one step from a complete list of ids.
- `POST /api/v1/workspaces/:slug/patches/:id/reset-to-origin` forces the hub's copy of a patch branch to origin's current tip, reports which of four actions it took, keeps the discarded tip fetchable under `refs/parley/replaced/<branch>`, and queues a rebuild when the branch moved.
- After any sequence of these calls, the non-deleted patches of a workspace hold the positions 1..N without gaps and every deleted patch holds position `null`.
- Each successful change emits its hub event (`parley.patch.update`, `.delete`, `.restore`, `.reorder`, `.reset`, `.replace`) together with the state change.
- The commands `parley patch update`, `remove`, `restore`, `reorder` and `reset-to-origin` call these routes; `reset-to-origin --wait` follows the rebuild the reset queued.

## Non-goals

- Creating a carry-patch workspace, the patch record and its store, patch add, patch list and get, `parley patch add|list|show`, and the definition of the access and precondition rules of CP-FR-006 to CP-FR-008: package 31 `carry_patch_setup_and_patches`. This package applies those rules to its own five routes.
- The durable job model, the enqueue with its dedup, the rebuild job record and `GET /workspaces/:slug/rebuilds/:id`: package 33 `rebuild_jobs`. This package calls the enqueue through the interface `RebuildEnqueuer` that package 31 declares and package 33 implements, and reads the job route from the CLI only.
- What a rebuild does with a patch (setting `conflict`, clearing it, soft-deleting a merged patch): package 34 `rebuild_execution`.
- Rollback and preview: package 35 `rebuild_rollback_and_preview`.
- The origin refresh of patch branches during sync, the divergence policy and `parley.patch.sync`: package 36 `carry_patch_sync`.
- Push control on patch branches, the mirror to origin and the triggers after a push: package 37 `carry_patch_push_control`.
- The purge of patches deleted for more than 7 days, the rule that a restore wins over a purge at the same moment, the patch-status summary, remembered resolutions and the git safety rules CP-FR-093 and CP-FR-094 as a whole: package 38 `carry_patch_rerere_and_maintenance`.
- Serving `refs/parley/replaced/*` to a git client and refusing pushes to it: packages 17 `git_smart_http` and 18 `git_push_rules`.
- The workspace settings document and its validation: package 11 `workspace_read_update_settings`. This package only reads `patch_branch_source` and `auto_rebuild_after_sync`.
- The error envelope, the order of checks, list paging and idempotency (PLAT-FR-010 to PLAT-FR-019): package 02 `http_api_conventions`. The CLI's settings, output, exit codes and shared waiting (PLAT-FR-090 to PLAT-FR-095): package 09 `cli_foundation`.
- The event log itself: package 52 `durable_event_log_and_stream`.

## Background

The repository holds no code yet; every file named here is new, or is created by a package this one depends on and extended here. The source is `docs/prd/10_carry_patch.md`: CP-FR-019 to CP-FR-033, the routes of §10.5 for update, delete, restore, reset-to-origin and reorder, the five `parley patch` commands of §10.6 that call them, the patch events of §10.8 and the patch state machine of §10.3.3.

Package 31 delivers, in `internal/carrypatch/`, the patch record with every field of §10.3.3, its table in the store, the patch list and get routes and the route guard that applies scope, access, workspace mode and workspace status. This package adds the five writing routes beside them (`internal/carrypatch/patch_update.go`, `patch_remove.go`, `patch_reorder.go`, `patch_reset.go`) and five commands to the patch command group (`internal/cli/patch.go`). Git work goes through `internal/gitrepo/` (single-branch fetch from origin, ancestry test, compare-and-swap of a ref), events through `internal/event/`, and the end-to-end test lives in `test/e2e/patch_maintenance_test.go`.

A patch is one row: `id`, `workspace_slug`, `branch_name`, `position` (1..N for non-deleted patches, `null` for a deleted one), `status` (`active`, `conflict`, `disabled`, `merged_upstream`, `deleted`), `conflict_files` (always present, `[]` unless the status is `conflict`), `upstream_pr_url`, `description`, `added_at`, `updated_at`, `deleted_at`, `origin_sync_state`, `origin_sha`, `origin_synced_at`. Every route here that answers with a patch returns all of these fields, `null` when unset.

## Requirements

### Access and common preconditions (CP-FR-006 to CP-FR-008)

All five routes are write routes. They need the PAT scope `patches:write` and write access to the workspace, and follow the global order of checks. What a caller observes, first failure wins:

1. A PAT without `patches:write`: HTTP 403, `error_type` `insufficient_scope`, whether or not the workspace exists.
2. A caller with no access to the workspace, or an unknown slug: HTTP 404, `error_type` `not_found`. A caller with read access but not write access: HTTP 403, `error_type` `forbidden`.
3. A body that fails validation of its shape (see each route): HTTP 400, `error_type` `invalid_request`.
4. The workspace is not in carry-patch mode, which includes every task fork: HTTP 400, `error_type` `workspace_mode_mismatch`.
5. The workspace status does not allow a write (WS-FR-007): HTTP 409 with `workspace_busy` for `pending`, `cloning` or a running reclone, archive or delete; `workspace_not_ready` for `failed`; `workspace_archived` for `archived`.
6. On the four routes that name a patch: no patch with that id in this workspace (an id of another workspace's patch and a value that is not a UUID included): HTTP 404, `error_type` `not_found`, message `patch not found`.
7. The route's own state checks (deleted, not deleted, status, position range, list completeness).

A refused request changes no patch, moves no ref, contacts no remote and emits no event.

### Patch update (CP-FR-019 to CP-FR-023)

`PATCH /api/v1/workspaces/:slug/patches/:id` takes a JSON object with the optional members `status`, `position`, `description` and `upstream_pr_url`.

- A member that is absent or `null` leaves its field alone. An empty string in `description` or `upstream_pr_url` clears the field to `null`. `branch_name` and unknown members are ignored. An empty object changes nothing. The answer is always HTTP 200 with the patch as it now stands.
- `status` must be one of `active`, `disabled`, `merged_upstream`, `conflict`. Any other value, `deleted` included, is HTTP 400 `invalid_request`. A status other than `conflict` clears `conflict_files` to `[]`; setting `conflict` leaves `conflict_files` as it is.
- `position` must be an integer between 1 and the number of non-deleted patches; otherwise HTTP 400 `invalid_request`. A position change takes the patch out of the order, inserts it at the new position and renumbers the list, in one transaction: a reader sees the old order or the new one, never a list with a gap or a duplicate.
- A body that is not a JSON object, or a member of the wrong JSON type, is HTTP 400 `invalid_request`.
- Any update of a deleted patch, the empty object included, is HTTP 409 `patch_deleted`.
- An update that changes at least one field sets `updated_at` and emits one `parley.patch.update` (resource type `patch`, resource id the patch id, actor the caller) with metadata `branch_name`, `changed` (the names of the fields that changed, from `status`, `position`, `description`, `upstream_pr_url`) and, when the status changed, `from`, `to` and `reason: "api"`. An update that changes nothing emits no event and leaves `updated_at` alone.
- No update queues a rebuild; the caller submits one.

### Patch delete and restore (CP-FR-024, CP-FR-025)

`DELETE …/patches/:id` is a soft delete. The patch gets `status: deleted`, `deleted_at` is set to now, `position` becomes `null`, `conflict_files` becomes `[]`, and the remaining non-deleted patches are renumbered 1..N-1 in their existing order, all in one transaction. The patch branch and `refs/parley/replaced/<branch>` are not touched. The answer is HTTP 204 with no body. It emits one `parley.patch.delete` with metadata `branch_name` and `reason: "api"`. Deleting a patch that is already deleted answers 204, changes nothing (`deleted_at` keeps its value) and emits nothing. A deleted patch is still returned by the get route and by the list with `include_deleted=true` (package 31).

`POST …/patches/:id/restore` takes no body. It applies to a `deleted` patch only: for any other status the answer is HTTP 409 `patch_not_deleted`. If another non-deleted patch of the workspace now has the same `branch_name`, the answer is HTTP 409 `patch_exists`. Otherwise the patch becomes `active`, takes position N+1 (the end of the list), `deleted_at` becomes `null`, `conflict_files` becomes `[]`, and the answer is HTTP 200 with the patch. It emits one `parley.patch.restore` with metadata `branch_name` and `position`. Restore does the same for a patch a caller deleted and for a patch a rebuild removed as merged upstream.

### Patch reorder (CP-FR-026)

`POST …/patches/reorder` takes `{"patch_ids": ["<id>", …]}`. The list must name every non-deleted patch of the workspace exactly once. A missing or non-array `patch_ids`, a duplicate id, an id that names no patch of the workspace, the id of a deleted patch, or a list that leaves a non-deleted patch out is HTTP 400 `invalid_request`; the message names the offending id, or, for an incomplete list, the two counts (ids given, patches expected). Nothing is changed.

On success the patches are renumbered 1..N in list order in one transaction, and the answer is HTTP 200 `{"items": [patch, …], "next_cursor": null}` with all non-deleted patches in the new order. It emits exactly one `parley.patch.reorder` whose resource id is the first patch of the new order and whose metadata is `patch_ids` (the new order).

Two position-changing requests on one workspace that arrive together both succeed, one after the other (PLAT-FR-019); neither fails because of the other, and the list holds 1..N afterwards.

### Reset to origin (CP-FR-027 to CP-FR-033)

`POST …/patches/:id/reset-to-origin` takes no body. It forces the hub's copy of the patch branch to origin's current tip, whatever the settings `patch_branch_source` and `patch_divergence_policy` say.

- It is allowed for a patch in `active`, `conflict` or `disabled`. For `merged_upstream` or `deleted` the answer is HTTP 409 `patch_status_invalid`, and origin is not contacted.
- The hub fetches that one branch from origin into `refs/remotes/origin/<branch>`, with the workspace's origin credential resolved at the time of the call. Origin has no such branch: HTTP 409 `missing_on_origin`. Any other failure of the fetch (origin unreachable, authentication refused, the credential cannot be resolved, the fetch passes its time limit of 2 minutes): HTTP 502 `origin_fetch_failed`, with a message in which URL user information reads `***@`. In both cases no branch moves and the patch is unchanged.
- The outcome, `action`, is the first that applies:

  | Situation | What the hub does | `action` |
  |---|---|---|
  | The hub has no such branch | creates it at origin's tip | `created` |
  | The hub's tip equals origin's | writes nothing | `none` |
  | The hub's tip is an ancestor of origin's | moves the branch forward | `fast_forwarded` |
  | Otherwise (the tips diverged, or the hub is ahead) | saves the hub's tip to `refs/parley/replaced/<branch>`, overwriting an earlier backup, then moves the branch | `replaced` |

- Every move is a compare-and-swap against the tip the hub compared (for `created`: against "no such ref"). If the branch moved in the meantime the answer is HTTP 409 `ref_moved` with the branch's current commit as `current_sha` in the envelope's `details`; the caller retries. A backup ref already written stays. The patch record is unchanged, nothing is emitted and no rebuild is queued.
- When `patch_branch_source` is `origin`, the patch's `origin_sync_state` becomes `in_sync`, `origin_sha` origin's tip and `origin_synced_at` now. When it is `hub` these three fields are left alone.
- If the branch moved (`created`, `fast_forwarded` or `replaced`), the patch is `active` or `conflict`, and the setting `auto_rebuild_after_sync` is true, the hub queues a rebuild through the enqueue of CP-FR-039 with the caller as `submitted_by`. If the workspace already has a rebuild that is queued, running or waiting to retry, the trigger is skipped.
- The answer is HTTP 200 with the patch plus `action`, `rebuild_triggered` (boolean; true only when this call queued a job), `rebuild_job_id` (present only when a job was queued) and `replaced_sha` (present only when `refs/parley/replaced/<branch>` exists after the call, with the commit it points at).
- Success emits `parley.patch.reset`, whatever the action, with metadata `branch_name`, `action`, `local_sha` (the hub's tip before the call, `null` for `created`), `origin_sha` and `replaced_sha` (the tip this call saved, `null` unless the action is `replaced`). When the action is `replaced` it also emits `parley.patch.replace` with `branch_name`, `replaced_sha`, `origin_sha` and `trigger: "reset_to_origin"`.

### CLI (§10.6)

The global CLI conventions apply. Unless stated, a command prints the hub's response on stdout and exits 0; a hub error is printed on stderr as received and the exit status is 1.

- `parley patch update <workspace-slug> <patch-id>` with `--status`, `--position <int>`, `--description`, `--upstream-pr`: calls the PATCH route with exactly the members whose flags were given (`--upstream-pr` as `upstream_pr_url`). A flag given with an empty string is sent and clears the field. With no flag the command sends nothing, reports `usage_error` and exits 2.
- `parley patch remove <workspace-slug> <patch-id>`: calls the DELETE route and prints `{"status":"removed","patch_id":"<id>"}`.
- `parley patch restore <workspace-slug> <patch-id>`: calls the restore route.
- `parley patch reorder <workspace-slug> <patch-id>...`: calls the reorder route with the ids in argument order. With no patch id it is a usage error (exit 2).
- `parley patch reset-to-origin <workspace-slug> <patch-id>` with `--wait`, `--timeout`, `--poll-interval`: calls the reset route. With `--wait`, and when the response names a `rebuild_job_id`, the command polls `GET /workspaces/:slug/rebuilds/:id` until the job is `completed`, `dead_letter` or `cancelled` (`failed` means waiting to retry, so polling goes on) and prints one document: the reset response with one more member, `rebuild`, holding the final job record. `completed` exits 0; `dead_letter` and `cancelled` print the document, report `wait_failed` and exit 1; a timeout prints nothing on stdout, reports `wait_timeout` and exits 1. With `--wait` and no `rebuild_job_id` the command prints the response as it is and exits 0 without polling.

### End-to-end flows

1. **Maintaining the list from the shell.** A developer with three registered patches runs `parley patch update` to disable the second, `parley patch reorder` to put the third first, `parley patch remove` on the first, then `parley patch restore` on it. Each command sends one request; the hub checks access, mode and status, changes the list in one transaction with its event, and answers. At the end `parley patch list` shows three patches at positions 1, 2, 3, the restored one last and `active`, and the event log holds one update, one reorder, one delete and one restore event.
2. **Recovering a patch branch from origin.** The hub's copy of a patch branch and origin's have diverged. The developer runs `parley patch reset-to-origin`. The hub fetches the branch from origin, saves its own tip under `refs/parley/replaced/<branch>`, moves the branch to origin's tip by compare-and-swap, records the events and answers with `action: "replaced"` and `replaced_sha`. The hub's branch now equals origin's and the old tip is still in the repository under the backup ref.

## Design Decisions

1. **Position of the patch checks in the order of checks.** Shape validation of the body (JSON types, the `status` enumeration, `position` at least 1, `patch_ids` an array of strings) runs before the mode and status checks, as CP-FR-008 says. The patch lookup (404), the deleted check and the checks that need the list (upper bound of `position`, completeness of `patch_ids`) run after them, because they read the patch list, which only a carry-patch workspace has. So an invalid `status` on a deleted patch is 400, and an out-of-range `position` on a deleted patch is 409 `patch_deleted`.
2. **A value equal to the current one is not a change.** It is left out of `changed`; an update whose every member equals the stored value emits nothing and leaves `updated_at` alone. CP-FR-023 emits "when anything changes".
3. **Setting `status: conflict` by hand leaves `conflict_files` as it is.** CP-FR-023 only says other statuses clear it.
4. **Soft delete clears `conflict_files`.** §10.3.3 clears it whenever the status becomes anything other than `conflict`.
5. **Reorder with the current order** succeeds and emits its event (CP-FR-026 says it emits one, without a condition); only patches whose position changed get a new `updated_at`. **Reorder on a workspace with no non-deleted patch** accepts `patch_ids: []`, answers 200 with no items and emits nothing, because there is no first patch to name as the resource.
6. **Refusals of reset leave everything as it was**, including `origin_sync_state` on `missing_on_origin` and a stale `refs/remotes/origin/<branch>`. The PRD names no side effect; sync (package 36) is what records the origin state.
7. **Time limit of the single-branch fetch: 2 minutes**, reported as `origin_fetch_failed`. CP-FR-093's 10 minutes covers rebuild, sync and preview only; 2 minutes is the limit package 12 uses for a request-time origin fetch.
8. **`ref_moved` carries `details.current_sha`**, as PLAT-FR-011 and §6 show for this error type. The lost compare-and-swap is `ErrRefMoved` of `internal/gitrepo` (package 12); rollback (package 35) answers it in the same shape, and a rebuild (package 34) records it as the retryable failure reason `ref_moved`.
9. **`replaced_sha` in the response versus the event.** In the response it reports the backup ref whenever one exists after the call (§10.5), so also one left by an earlier sync or reset. In `parley.patch.reset` it is the tip this call saved, `null` otherwise.
10. **The rebuild enqueue is the interface `RebuildEnqueuer` of package 31** (`EnqueueRebuild(ctx, slug, options)`), which package 33 implements and registers when `parleyd` is composed; until then package 31's default queues nothing and the answer says `rebuild_triggered: false`. The plan makes this package depend on 31 only, and 33 depends on 31, so the interface keeps the order free. A duplicate reported by the enqueue, and any enqueue error, leave the reset in place: the answer is still 200 with `rebuild_triggered: false`, and an error is logged.
11. **Bodies on routes that take none** (restore, reset-to-origin) are ignored. No route here creates a resource, so `Idempotency-Key` has no effect on them.
12. **The CLI does not validate `--status`**; it sends the value and the hub answers. `--position` must parse as an integer, which the flag type enforces as a usage error.
13. **One writer of the backup ref.** `WriteBackupRef` in `internal/carrypatch/backup_ref.go` is added here and is the only function that writes `refs/parley/replaced/<branch>`; the origin refresh of sync (package 36, which depends on this package for it) calls it.
14. **Git runs through the carry-patch runner of package 31**, which holds the time limit (here the 2 minutes of decision 7) and cleans error text (`***@`).
15. Project decisions relied on: git operations run the `git` binary through `internal/gitrepo`; the store is SQLite behind `internal/store`; HTTP uses `net/http`; the CLI uses cobra; code takes a replaceable clock; tests create repositories with `git init` in `t.TempDir()` and use `internal/gitrepo/gitrepotest/` for origins.

## Dependencies

- Package 31 `carry_patch_setup_and_patches` (draft): the patch record and its store, the route guard for carry-patch routes, the patch read routes, single-branch fetch from origin, the carry-patch git runner, the `RebuildEnqueuer` interface and the `parley patch` command group.
- Package 09 `cli_foundation` (draft): the HTTP client, output, exit codes and the shared `--wait` behaviour the five commands are built on.
- Package 52 `durable_event_log_and_stream` (draft): the event component every patch event is written through, in the transaction of the state change.
- Not dependencies, called through interfaces or over HTTP only: package 33 `rebuild_jobs` (the enqueue, the job route the CLI polls) and package 10 `workspace_create_and_clone` (the origin credential resolver, reached through package 31).
- External: the `git` command, 2.38 or later, at run time.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Origin**: A registered workspace's remote (`git_url`).
- **Carry-patch workspace**: A workspace that tracks an upstream project and keeps an ordered list of patches on top of it.
- **Upstream**: For a carry-patch workspace, the project it tracks (`upstream_url`).
- **Integration branch**: For a carry-patch workspace, the branch the hub rebuilds as upstream plus the ordered patches.
- **Patch**: A carry-patch entry: a branch with a position in the patch order and a status.
- **Patch branch**: The branch a patch names in `branch_name`.
- **Backup ref**: `refs/parley/replaced/<branch>`, the tip of a patch branch the hub discarded when it replaced the branch. One per branch; a later replace overwrites it.
- **Soft delete**: Marking a patch `deleted` without removing its row, its branch or its backup ref, so that it can be restored.
- **Rebuild**: A durable job that recreates the integration branch.
- **Durable job**: Background work that is recorded before its request returns, survives restarts and is retried with backoff.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number.
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
