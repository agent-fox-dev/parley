---
spec_id: "35"
spec_name: "rebuild_rollback_and_preview"
title: "Roll back and preview rebuilds"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "96a41f29ab841cdfb4df1d93380dd71a4564ce15cb84d5933c0b845614bc02f6"
schema_version: 3
source: "docs/prd/10_carry_patch.md"
---

# Roll back and preview rebuilds

## Intent

A team that carries patches on a moving upstream needs two safeguards around the rebuild of its integration branch. Before a rebuild, it needs to know which patches would conflict with the upstream it last fetched, without changing anything. After a rebuild that published a bad result, it needs to put the integration branch back to the head it had before that rebuild, in one call. Preview is the first safeguard and rollback is the second; neither touches the patch list, the jobs or origin.

## Goals

- `POST /api/v1/workspaces/:slug/rebuilds/:id/rollback` moves the integration branch to the `previous_integration_head_sha` of a completed rebuild with one compare-and-swap and answers `200` with `{"rolled_back_to","previous_head_sha"}`.
- A rollback that cannot be done answers `409` with `error_type` `rollback_unavailable` (job not completed, no previous head, previous commit gone) or `ref_moved` (the integration branch moved during the call), and leaves the integration branch as it was.
- A rollback changes the integration branch and nothing else: no patch, no position, no job, no patch branch, nothing on origin. It emits one `parley.rebuild.rollback` event.
- `GET /api/v1/workspaces/:slug/rebuild-preview` answers `200` with the upstream base it used and one result per applicable patch in position order: `would_succeed`, `would_conflict` (with `conflict_files` and a conflict report) or `would_skip`.
- Preview never fetches, never applies remembered resolutions, tests a merge whatever the rebuild strategy, and changes no ref, no patch and no job. It answers while a rebuild runs.
- `parley rebuild rollback <workspace-slug> <rebuild-id>` and `parley rebuild preview <workspace-slug>` run the two calls from the shell; `--fail-on-conflict` makes preview exit 3 when a patch would conflict.

## Non-goals

- Creating a carry-patch workspace, the upstream fetch at setup, creating the integration branch, the patch list and its storage: package 31 `carry_patch_setup_and_patches`. This package reads the patch list and the workspace's carry-patch fields.
- Updating, removing, restoring and reordering patches: package 32 `patch_maintenance`. Rollback restores no patch; a patch a later rebuild removed as merged is restored there, one by one.
- The durable job model, the rebuild job record, submit, list, get, cancel and requeue, and the commands `parley rebuild submit|list|status|cancel|requeue`: package 33 `rebuild_jobs`. This package reads one job record and adds two commands to the `rebuild` command group.
- The rebuild itself: upstream fetch, replaying patches, remembered resolutions, the publish and what it records in `integration_head_sha` and `previous_integration_head_sha`, the optional push of the integration branch, follow-up rebuilds: package 34 `rebuild_execution`. That the next rebuild that publishes replaces a rolled-back head is that package's publish.
- The carry-patch part of sync, which is what refreshes the upstream base preview reads: package 36 `carry_patch_sync`.
- Refusing client writes to the integration branch: packages 18 `git_push_rules` and 37 `carry_patch_push_control`.
- Listing and forgetting remembered resolutions, the patch-status summary, the 10-minute limit on one git operation and the removal of credentials from messages (CP-FR-093, CP-FR-094): package 38 `carry_patch_rerere_and_maintenance`, with the carry-patch git runner of package 31 as the mechanism. Preview and rollback run git through that runner, so those rules apply to them; here a git failure is only mapped to an answer.
- The fields of the conflict report as landing defines them (§8.3.3) and reading a conflicted file: package 26 `conflict_reports_and_resolution`. Preview fills the same shape for a rebuild, as §10.3.6 states it.
- The error envelope, the order of checks before state preconditions, authentication, scopes, the binding guard and the access rule: packages 02, 04, 06, 07 and 08. The CLI's settings, output, exit codes and the shared `--fail-on-…` exit: package 09 `cli_foundation`.
- The event log and delivery of `parley.rebuild.rollback`: package 52 `durable_event_log_and_stream`. This package emits through it.
- Pushing a rolled-back integration branch to origin. Rollback never pushes (CP-FR-062).
- Holding a rollback against later rebuilds. An operator who wants the rollback to hold turns off `auto_rebuild_after_push` and `auto_rebuild_after_sync` (package 11 `workspace_read_update_settings`) or disables the patch at fault (package 32).

## Background

The repository holds no code yet; every file named here is new, or is created by a package this one depends on and extended here. The source is `docs/prd/10_carry_patch.md`: CP-FR-060 to CP-FR-066, the common rules CP-FR-006 to CP-FR-008 and CP-FR-057 as they apply to the two routes, the route entries `POST /workspaces/:slug/rebuilds/:id/rollback` and `GET /workspaces/:slug/rebuild-preview` in §10.5, the commands `parley rebuild rollback` and `parley rebuild preview` in §10.6, the `parley.rebuild.rollback` row of §10.8, and the rollback, upstream-unreachable and task-fork entries of §10.10.

A carry-patch workspace (package 31) is a workspace with an upstream, an ordered patch list and an integration branch (default `deploy`) that only the hub writes. A rebuild (packages 33 and 34) is a durable job that recomputes the integration branch as "upstream base plus every applicable patch" and publishes it with one compare-and-swap. A completed job records the head it published, `integration_head_sha`, and the head it replaced, `previous_integration_head_sha`. Rollback reads the second; preview predicts what the next rebuild would meet.

The code lives in `internal/carrypatch/`: `rollback.go` (the rollback operation and its route) and `preview.go` (the preview operation and its route), each with its test file beside it. The two routes are mounted where package 33 mounts the other rebuild routes (`internal/carrypatch/rebuild_routes.go`). The trial merge that preview needs is new, in `internal/gitrepo/merge.go`: package 34 merges in a scratch working tree (`internal/gitrepo/scratch.go`) and has no merge without one. The compare-and-swap of rollback is package 12's, in `internal/gitrepo/refs.go`. The two commands are added to the `rebuild` command group in `internal/cli/rebuild.go`, which package 33 creates. The end-to-end tests are in `test/e2e/rebuild_rollback_preview_test.go`.

## Requirements

### Who may call the two routes, and when

Both routes follow the global check order (CP-FR-006). Rollback is a write route: a PAT needs scope `rebuilds:write` and the caller needs write access to the workspace. Preview is a read route: a PAT needs `rebuilds:read` and the caller needs read access (CP-FR-007). A PAT without the scope gets `403` with `error_type` `insufficient_scope`; an org member without write access gets `403` `forbidden` on rollback and may call preview; a caller with no access gets `404` `not_found`, the same answer as for an unknown slug.

After that, both routes check in this order, and the first failure decides the answer (CP-FR-008):

| Check | Answer |
|---|---|
| The workspace is in carry-patch mode. A standard workspace and every task fork fail this check (§10.10) | `400` `workspace_mode_mismatch` |
| The workspace is `pending` or `cloning`, or a reclone, archive or delete of it is running | `409` `workspace_busy` |
| The workspace is `failed` | `409` `workspace_not_ready` |
| The workspace is `archived` | `409` `workspace_archived` |

Preview is the one read route of §10 that applies the status checks. Neither route takes a body or a query parameter, and neither accepts `Idempotency-Key`: they create no resource.

Rollback then looks up the job. An unknown job id, an id that is not a UUID, and the id of a job of another workspace are `404` `not_found` with the message `rebuild job not found` (CP-FR-057).

### Rollback

`POST /api/v1/workspaces/:slug/rebuilds/:id/rollback` (CP-FR-060 to CP-FR-062).

The target is the `previous_integration_head_sha` of the job `:id`. Rollback is unavailable, `409` with `error_type` `rollback_unavailable`, when:

- the job's status is not `completed` (`queued`, `running`, `failed`, `dead_letter` or `cancelled`);
- the job has no previous head (`previous_integration_head_sha` is `null`, the first rebuild);
- the commit of the previous head no longer exists in the repository (for example after a reclone).

Otherwise the hub reads the current head of the integration branch and moves the branch to the target with one compare-and-swap against the head it read (CP-FR-061). If the branch moved in between, because a rebuild published or another rollback ran, the answer is `409` `ref_moved` with the branch's current head as `current_sha` in the envelope's `details`, and the branch keeps the value the other writer gave it. The caller reads the state again and decides.

On success the answer is `200`:

```json
{"rolled_back_to": "<the job's previous_integration_head_sha>", "previous_head_sha": "<the head the rollback replaced>"}
```

Both are full 40-character commit ids. `previous_head_sha` is `null` when the integration branch did not exist; the rollback then creates it. When the branch already points at the target, the answer is `200` with both members equal.

Rollback changes the integration branch only (CP-FR-062). Patch statuses, positions, `conflict_files`, deleted patches, patch branches, backup refs, every job record and origin are untouched. The integration branch is not pushed to origin, even when `rebuild_push_integration_branch` is true. No rebuild is queued. A patch that a later rebuild soft-deleted as merged stays deleted (§10.10).

A rollback does not wait for a rebuild and is not refused because of one (CP-FR-053): it is served the same while a rebuild of the workspace is `queued`, `running` or `failed`.

A successful rollback emits one `parley.rebuild.rollback` event (§10.8): `resource_type` `rebuild`, `resource_id` the job id, the caller as actor, the workspace's slug, and metadata `integration_branch`, `rolled_back_to` and `previous_head_sha`. A refused rollback emits nothing.

### Preview

`GET /api/v1/workspaces/:slug/rebuild-preview` (CP-FR-063 to CP-FR-066).

Preview predicts the outcome of a rebuild against the upstream base as of the last upstream fetch: the head of the upstream's default branch under `refs/remotes/upstream/`, or the upstream branch named like the workspace's `default_branch` when the upstream's default branch is unknown (§10.3.1). Preview does not fetch and contacts neither upstream nor origin, so it keeps working from the last fetched base while upstream is unreachable (§10.10). When neither ref resolves there is no upstream base: `409` with `error_type` `upstream_unknown`; the caller syncs first.

The hub takes the non-deleted patches in position order, leaves out those in `disabled` and `merged_upstream`, and starts a predicted state at the upstream base. For each remaining patch (CP-FR-064):

| Situation | `status` | Effect on the predicted state |
|---|---|---|
| The patch branch does not exist | `would_skip` | none |
| The patch tip merges onto the predicted state without a conflict | `would_succeed` | the merge result becomes the predicted state |
| The merge has a conflict | `would_conflict` | none: later patches are tested without this patch |

The answer is `200`:

```json
{"upstream_head_sha": "<base used>",
 "patch_results": [{"patch_id": "…", "branch_name": "…", "position": 1, "status": "would_succeed",
                    "conflict_files": [], "conflict": null}]}
```

`patch_results` holds one entry per tested patch, in position order; a workspace with no patch to test answers `200` with `patch_results: []`. `conflict_files` is always present and is `[]` unless the status is `would_conflict`; then it lists the conflicted paths in ascending order. `conflict` is `null` unless the status is `would_conflict`; then it is the conflict report of §10.3.6: `patch_id`; `target_sha`, the predicted state the patch was tested against (the upstream base for the first patch); `head_sha`, the patch tip; `base_sha`, the merge base of the two; `commit_sha`, the patch tip; `files`, one entry per conflicted path with `path`, `kind`, `base`, `ours` (the predicted state), `theirs` (the patch) and `landed_by: []`; `landed_changes: []`; `change: null`.

Preview is a prediction (CP-FR-065). It tests a merge of each patch tip whatever `rebuild_strategy` says, and it does not apply remembered resolutions, so a patch reported `would_conflict` may apply in a real rebuild.

Preview changes nothing a caller can observe (CP-FR-066): no ref, no patch field (a patch reported `would_conflict` keeps its status and its `conflict_files`), no job, no remembered resolution, and it emits no event. It can run at any time, including while a rebuild runs.

A git command that fails for another reason than a merge conflict, a command stopped by the time limit of CP-FR-093 included, makes the answer `500` with `error_type` `internal`. It is never reported as `would_conflict` or `would_skip`, and no partial result is returned.

### The commands

`parley rebuild rollback <workspace-slug> <rebuild-id>` sends the rollback request and prints the answer (§10.6). `parley rebuild preview <workspace-slug>` sends the preview request and prints the answer. Both take exactly the arguments shown; another count is a usage error, exit 2, with no request. A hub error is printed on stderr as received and the exit code is 1.

`parley rebuild preview --fail-on-conflict` prints the answer first, then, when any entry of `patch_results` has status `would_conflict`, reports `condition_not_met` on stderr with the names of the conflicting branches and exits 3. Without a conflicting patch, or without the flag, the exit code is 0.

### End-to-end flows

**An operator rolls back a rebuild.** A carry-patch workspace has been rebuilt twice; the second rebuild added a patch that broke the build. The operator runs `parley rebuild rollback my-fork <id of the second job>`. The CLI sends the request; `parleyd` authenticates the caller, finds write access, a ready carry-patch workspace and a completed job with a previous head, moves the integration branch by compare-and-swap, writes the event and answers `200`. The CLI prints `rolled_back_to` and `previous_head_sha`. The integration branch now equals the head the first rebuild published; both patches are still registered and active.

**A developer previews before rebuilding.** A carry-patch workspace has two patches; upstream, as last fetched, changed a line that the second patch also changes. The developer runs `parley rebuild preview my-fork --fail-on-conflict`. `parleyd` resolves the upstream base without fetching, tests the two patches in order and answers `200`: the first `would_succeed`, the second `would_conflict` with its file and a conflict report. The CLI prints the answer, names the conflicting branch on stderr and exits 3. No ref, patch or job has changed.

## Design Decisions

1. **Order of checks on rollback: mode, workspace status, job lookup, job state, commit exists, compare-and-swap.** CP-FR-008 puts the mode and status checks first "after request validation" and the PRD does not place the `404` for the job. The job lookup follows them, so the two workspace-level answers are the same on every route of §10.
2. **An id that is not a UUID is `404` `not_found`, like an unknown id.** CP-FR-057 names only "an unknown job id"; a malformed id names no job either, and one answer is simpler than two.
3. **`ref_moved` carries `details.current_sha`.** §10 gives only the type. `ref_moved` is the shared type of §6, where it carries the current head (PLAT-FR-011); the same shape here lets one client handle both. The lost compare-and-swap is `ErrRefMoved` of `internal/gitrepo` (package 12); reset to origin (package 32) answers it in the same shape, and a rebuild (package 34) records it as the retryable failure reason `ref_moved`.
4. **Rollback to the head the branch already has succeeds.** The PRD is silent. The compare-and-swap holds, the answer is `200` with both members equal and the event is emitted, so a repeated rollback is harmless.
5. **Rollback creates a missing integration branch.** The PRD is silent on an integration branch that does not exist (setup failed and no rebuild since). The compare-and-swap then expects the branch to be absent, the branch is created at the target and `previous_head_sha` is `null`. Refusing would leave the operator with no way to restore the head.
6. **No `parley.git.push` for a move of the integration branch by the hub.** §10.8 says updates of the integration branch also produce the general `parley.git.push` event, but that event's `via` is `git`, `api` or `land` (§5.8) and it is defined for accepted client pushes (GIT-FR-060); no client can push the integration branch. A rebuild's publish is reported by `parley.rebuild.complete`, which carries both heads, and a rollback by `parley.rebuild.rollback` (package 34 records the same decision).
7. **The event is written after the compare-and-swap succeeded.** A ref is not part of the store's transaction; writing the event first could record a rollback that then loses the compare-and-swap.
8. **A patch whose tip is already contained in the predicted state is `would_succeed`.** The rebuild's `nothing_to_apply` skip belongs to the rebase strategy; preview tests a merge (CP-FR-065), a merge of a contained tip has no conflict, and CP-FR-064 reserves `would_skip` for a missing branch.
9. **Preview of a workspace with no patch to test answers `200` with an empty list.** `no_active_patches` is an error of rebuild submit only (CP-FR-035); the preview route lists no such error.
10. **`target_sha` of a preview conflict is the predicted state, which no ref points to.** §10.3.6 defines `target_sha` as the head under construction. For the first tested patch it is the upstream base; after a `would_succeed` patch it is the commit preview built for the prediction. A client can compare it but cannot fetch it. Preview may leave such unreferenced objects in the repository; they are not refs, and git's own housekeeping removes them.
11. **`commit_sha` of a preview conflict is the patch tip.** Preview always tests a merge, and §10.3.6 gives the patch tip for strategy `merge`.
12. **A git failure that is not a conflict is `500` `internal` for the whole preview.** CP-FR-045 forbids recording such a failure as a skip, a conflict or a success in a rebuild; preview follows the same rule, and CP-FR-093 gives `500` for a request.
13. **Preview reads each patch tip once.** A push that moves a patch branch during the preview does not produce a result mixed from two tips, as in a rebuild (CP-FR-041).
14. **`parley rebuild preview` uses the CLI's default request timeout of 30 seconds.** §10.6 states no other value (PLAT-FR-091).
15. **The preview's conflict report is the rebuild's.** It is built with the report builder package 34 exports, whose file entries come from the report component of package 26 (`internal/land/`) through that package's conflict-file describer; preview supplies the predicted state, the patch tip and the conflicted paths of its trial merge.
16. **Shared pieces are read, not copied.** The integration branch name comes from `IntegrationBranch` of package 31; the upstream base from `UpstreamBase` of package 31; every git operation runs through the carry-patch git runner of package 31, which holds the 10-minute limit of CP-FR-093 and cleans error text. The command that reads one job is `parley rebuild status` (package 33).
17. **Project decisions relied on:** the server runs the `git` command, 2.38 or later, through `internal/gitrepo` and uses no git library (preview's trial merge is `git merge-tree --write-tree`, which that version provides and which needs no working tree); repositories are bare; the store is SQLite behind `internal/store`; HTTP is `net/http`; the CLI is built on `github.com/spf13/cobra` through the foundation of package 09; tests use `testing`, `net/http/httptest` and repositories made with `git init` in `t.TempDir()`; code takes a replaceable clock.

## Dependencies

- Package 34 `rebuild_execution` (draft): publishes the integration branch and records `integration_head_sha` and `previous_integration_head_sha` on the job; provides the upstream base rule and the conflict report of a rebuild. Through it this package builds on package 33 (the job record and the `rebuild` command group) and package 31 (the carry-patch workspace, the patch list, the route set of `internal/carrypatch`).
- Package 09 `cli_foundation` (draft): the HTTP client, output, exit codes and the shared exit 3 of `--fail-on-…` flags that the two commands are built on.
- Package 52 `durable_event_log_and_stream` (draft): the event component `parley.rebuild.rollback` is emitted through.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Origin**: A registered workspace's remote (`git_url`).
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task.
- **Carry-patch workspace**: A workspace that tracks an upstream project and keeps an ordered list of patches on top of it.
- **Upstream**: For a carry-patch workspace, the project it tracks (`upstream_url`).
- **Upstream base**: The commit every rebuild starts from: the head of the upstream's default branch as of the last upstream fetch.
- **Integration branch**: For a carry-patch workspace, the branch the hub rebuilds as upstream plus the ordered patches.
- **Patch**: A carry-patch entry: a branch with a position in the patch order and a status.
- **Rebuild**: A durable job that recreates the integration branch.
- **Durable job**: Background work that is recorded before its request returns, survives restarts and is retried with backoff.
- **Conflict report**: The structured description of why a rebase could not be done: files, kinds, and the landed changes involved.
- **Remembered resolution**: A recorded way a conflict was resolved, which a rebuild replays when the same conflict appears again (git rerere).
- **Preview**: A read-only prediction of the outcome of a rebuild against the upstream base as of the last fetch.
- **Rollback**: Moving the integration branch back to the head it had before a given completed rebuild.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number.
