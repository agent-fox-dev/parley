---
spec_id: "34"
spec_name: "rebuild_execution"
title: "Rebuild the integration branch"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "9943c6b4a89cf654740cb0bcc93750a058e03f12973147dd989a09dfc348ea2f"
schema_version: 3
source: "docs/prd/10_carry_patch.md"
---

# Rebuild the integration branch

## Intent

A team that carries its own patches on top of a project it does not control needs one branch that always equals "the upstream project plus every patch that still applies, in order". The hub recomputes that branch, the integration branch, in a rebuild: it works out of sight of every client, tells the team which patch stopped applying and why, reuses conflict resolutions it has seen before, and replaces the branch in one step, so that a reader sees either the old branch or the new one and never a half-built one.

## Goals

- One attempt of a rebuild job fetches upstream, takes the patch list and the tips of the patch branches as they are at that moment, and builds the new integration head from those alone.
- Both strategies work: `rebase` replays the commits of each patch, `merge` merges each patch tip with a merge commit.
- A patch that conflicts is reported with a conflict report in the shape landing uses, and the run stops (`fail_fast`) or goes on without it (`continue`).
- Remembered resolutions are replayed before a conflict is reported, and learned from the patch branch.
- The integration branch moves once, by compare-and-swap, together with the changes of the patch list that belong to it.
- The job record shows progress after every patch and the full result at the end; every outcome emits its event.
- A rebuild that worked on inputs that have changed since queues one follow-up rebuild.
- The integration branch is pushed to origin after the publish when the workspace asks for it.

## Non-goals

- The durable job model, retries with backoff, dedup, crash recovery, the worker pool, the rebuild job record's storage and the routes and commands that submit, list, read, cancel and requeue a rebuild (§10.3.4, CP-FR-034 to CP-FR-039, CP-FR-056 to CP-FR-059): package 33 rebuild_jobs. This package is the handler those jobs run.
- Creating a carry-patch workspace, the upstream set-up, and adding and reading patches (CP-FR-001 to CP-FR-018): package 31 carry_patch_setup_and_patches.
- Updating, removing, restoring, reordering and resetting patches through the API (CP-FR-019 to CP-FR-033): package 32 patch_maintenance.
- Rollback of the integration branch and the rebuild preview (CP-FR-060 to CP-FR-066): package 35 rebuild_rollback_and_preview.
- The carry-patch part of sync, merge detection and the rebuild queued after a sync (CP-FR-067 to CP-FR-076): package 36 carry_patch_sync. This package only skips a patch that is already `merged_upstream` and removes it at the publish.
- Push control on the integration branch and on patch branches, the mirror of patch branches, and the rebuild trigger after a patch branch update (CP-FR-077 to CP-FR-088): package 37 carry_patch_push_control.
- Listing and forgetting remembered resolutions, the patch-status summary, the purge of deleted patches, and the discard of an interrupted rebuild's leftovers at start and the git safety rules (CP-FR-089 to CP-FR-097): package 38 carry_patch_rerere_and_maintenance. This package records the entries that package lists, in the transactional store, and exports the operation that discards scratch working trees. Crash recovery of jobs and the shutdown grace are package 33's.
- The conflict report's file entries and kinds (§8.3.3): package 26 conflict_reports_and_resolution. This package fills the report's rebuild-specific members and takes the file entries from that component.
- The event log itself: package 52 durable_event_log_and_stream.

## Background

The repository holds no code yet; every file named here is new. The source is `docs/prd/10_carry_patch.md`, requirements CP-FR-040 to CP-FR-055, with the data model of §10.3.2 (refs), §10.3.3 (patch), §10.3.5 (job record and patch result), §10.3.6 (conflict report) and §10.3.7 (remembered resolutions), the events of §10.8 and the configuration of §10.9.

The rebuild lives in `internal/carrypatch/`. It runs git through `internal/gitrepo/`, is called by the durable queue of `internal/jobs/` (package 33), emits events through `internal/event/` (package 52), and is wired into the process by `internal/hub/` (package 03). A workspace's bare repository is at `<[workspace].path>/<slug>.git` (package 10).

A rebuild job is created by package 33 with `strategy`, `fail_mode` and `integration_branch` captured when it was queued. A worker claims it and calls the handler of this package once per attempt.

## Requirements

### Starting an attempt (CP-FR-040, CP-FR-045, CP-FR-053, CP-FR-054)

An attempt first reads the workspace. If it is `archived` or `failed` the attempt ends as a permanent failure (reasons `workspace_archived`, `workspace_not_ready`) and the job is dead-lettered without retry. If it is `pending` or `cloning` the attempt ends as a retryable failure (reason `workspace_busy`). If the workspace no longer exists, its jobs were removed with it: the attempt ends without writing anything and without an event.

The same test is made again before every patch and inside the publish. A rebuild never blocks an archive, a reclone or a delete; when one happens during a run, the attempt ends at its next step in the way just described, and it has published nothing.

On a `ready` workspace the attempt fetches upstream with the upstream credentials (`UPSTREAM_GIT_PAT`, else `UPSTREAM_GIT_USERNAME` with `UPSTREAM_GIT_PASSWORD`, else the origin credentials of the workspace) into `refs/remotes/upstream/*`, without tags, and resolves the **upstream base**: the head of the upstream's default branch, or, when that is unknown, the upstream branch named like the workspace's `default_branch`. The base is stored on the job as `upstream_head_sha`. A fetch that fails is a retryable failure (reason `upstream_fetch_failed`); so is a base that does not resolve (reason `upstream_unknown`). In both cases no patch is touched and nothing is published.

Each attempt starts with an empty `patch_results`.

### The patch list of a run (CP-FR-041, CP-FR-046, CP-FR-047)

After the fetch, the run lists the non-deleted patches in position order and records the present tip of each patch branch as `source_sha` (`null` when the branch does not exist). Only these recorded commits are used from then on. A push to a patch branch during the run is accepted as at any other time (no lock, CP-FR-053) and does not change what the run builds.

Patches are taken in position order:

- `disabled` and `merged_upstream` patches are `skipped` with that reason;
- a patch whose branch does not exist is `skipped` with `branch_not_found`;
- `active` and `conflict` patches are applied. A `conflict` patch is tried again by every rebuild.

After every patch the results so far are stored on the job, so that a read of a running job shows progress. A patch result has `patch_id`, `branch_name` and `position` as listed at the start, `status` (`success`, `conflict`, `skipped`), `skipped_reason` (`null` unless skipped), `source_sha`, `new_head_sha` (`null` unless `success`), `conflict_files` (`[]` unless `conflict`) and `conflict` (`null` unless `conflict`). The four counts of the job stay 0 until the job is `completed`.

### Building the new head (CP-FR-042, CP-FR-043)

The new head is built in a scratch working tree that no ref points to. Until the publish, no ref a client can read changes except `refs/remotes/upstream/*`.

The head under construction starts at the upstream base.

With strategy `rebase`, the commits of the patch are those reachable from `source_sha` and not from the upstream base, oldest first. Merge commits are left out. Each remaining commit is replayed onto the head under construction with its author, author date and message; the committer is the hub's landing identity (`[land].committer_name`, `[land].committer_email`, default `Parley <parley@localhost>`), never the process environment. A commit whose change is already present in the head under construction (from upstream or from an earlier patch) is dropped. A patch with no commit left to replay is `skipped` with `nothing_to_apply`.

With strategy `merge`, the patch tip is merged into the head under construction with a merge commit, even when a fast-forward is possible. The message is `Merge branch '<branch_name>'`; the first parent is the head under construction and the second is `source_sha`; author and committer are the hub's landing identity. A tip that is already reachable from the head under construction is `skipped` with `nothing_to_apply`.

A patch that applies is `success` and `new_head_sha` is the head under construction after it.

### Conflicts and fail modes (CP-FR-044, §10.3.6)

When a commit (or, with `merge`, the tip) does not apply, the hub first replays remembered resolutions (next section). If no path is left in conflict the patch goes on as if it had applied.

Otherwise the patch is undone: the head under construction returns to what it was before the patch. The patch's status becomes `conflict` and its `conflict_files` are set to the paths left in conflict, ordered by path; when the status changed, `parley.patch.update` is emitted with actor `system`, `changed: ["status"]`, `from`, `to: "conflict"` and `reason: "rebuild_conflict"`. The patch result is `conflict` with the same `conflict_files` and a conflict report:

| Member | Value |
|---|---|
| `patch_id` | the patch |
| `target_sha` | the head under construction before the patch: the upstream base for the first applied patch |
| `head_sha` | the patch's `source_sha` |
| `base_sha` | the merge base of `source_sha` and `target_sha` |
| `commit_sha` | `rebase`: the first commit that did not apply; `merge`: `source_sha` |
| `files` | one entry per path still in conflict after remembered resolutions were replayed, with `path`, `kind`, `base`, `ours` (the state under construction), `theirs` (the patch) and `landed_by: []` |
| `landed_changes` | `[]` |
| `change` | `null` |

Then:

- `fail_fast`: the run stops. Nothing is published. The attempt is a permanent failure with reason `conflict`: the job is `dead_letter`, its `conflict` is the report, its `error` names the patch branch, and it is not retried. `patch_results` ends with the conflicting patch.
- `continue`: the run goes on with the next patch from the state before the conflicting patch, and the integration branch is published without it.

A git failure that is not a conflict (reason `git_error`), a single git operation that runs longer than 10 minutes (reason `timeout`, CP-FR-093) and a shutdown of the hub (reason `shutdown`) are retryable failures. They are never recorded as a skip, a conflict or a success, they change no patch and they publish nothing.

### Remembered resolutions (CP-FR-044, §10.3.7)

Resolutions are remembered per workspace with git rerere, in the workspace's repository. When a commit does not apply, git replays every recorded resolution that matches a conflict of that commit. A path whose conflict is resolved this way is no longer in conflict.

For every conflict git records during a run, the hub keeps an entry: the id git gave the conflict, the path it was in, whether a resolution is recorded, and `recorded_at` (the time of the conflict while unresolved, the time of the resolution afterwards). Package 38 lists and forgets these entries.

A resolution is learned from the patch branch. When paths are left in conflict and the patch has two-parent merge commits among the commits the upstream base lacks, the hub redoes each of those merges (the newest 50 at most) in the scratch working tree. Where a redone merge conflicts, the content the merge commit has for the conflicted files is the developer's resolution, and git records it for that conflict. The hub then tries the commit that did not apply once more. This is how "a later rebuild sees the same conflict resolved": a developer who merges upstream into the patch branch and resolves the conflict there has taught the hub, and every later rebuild that meets the same conflict applies the patch without help.

A workspace's resolutions are never replayed in another workspace.

### Publish (CP-FR-048, CP-FR-049, CP-FR-055)

When all patches are processed, the hub reads the integration branch's head and moves the branch to the new head with one compare-and-swap against the head just read (creating the branch when it does not exist). This is the only moment a rebuild changes a ref that clients read. The job gets `integration_head_sha` (the new head) and `previous_integration_head_sha` (the head replaced; `null` when the branch did not exist).

In the same step, and only when the compare-and-swap wins:

- every patch the run skipped as `merged_upstream`, and that still has that status, is soft-deleted (`parley.patch.delete`, actor `system`, `reason: "merged_upstream"`);
- every patch the run applied and whose status is `conflict` becomes `active` with empty `conflict_files` (`parley.patch.update`, actor `system`, `from: "conflict"`, `to: "active"`, `reason: "rebuild_clean"`);
- the remaining non-deleted patches are renumbered 1..N in their present order.

This also holds for a `continue` run in which other patches conflicted.

A lost compare-and-swap (someone else moved the integration branch after the read) is a retryable failure with reason `ref_moved`; the patch list is as it was and the branch is as the other writer left it.

A run with no applicable patch still publishes: the integration branch moves to the upstream base.

### Push of the integration branch (CP-FR-050)

After a publish, the workspace setting `rebuild_push_integration_branch` is read. When it is true, the hub pushes the integration branch to origin with the origin credentials and force-with-lease against the value it last saw on origin for that branch (`refs/remotes/origin/<integration_branch>`; "must not exist" when it never saw one), within 120 seconds. Success sets `integration_branch_pushed` to true on the job and records the pushed head as the value last seen. A failure (stale lease, origin unreachable, time limit) emits `parley.rebuild.push_failed` with `integration_branch` and `reason` and does not fail the rebuild: the job still completes, `integration_branch_pushed` stays false and the hub's integration branch stays at the new head. When the setting is false, origin is not contacted.

### Stale-input check (CP-FR-051)

After a publish (and the push, when there is one) and after a `fail_fast` conflict, the hub compares its inputs with the present, without fetching:

- a patch listed at the start, and not deleted since, is stale when its branch exists and its tip differs from the recorded `source_sha`;
- upstream is stale when the upstream base resolved now differs from the one the run used.

Stale patches are ignored when `auto_rebuild_after_push` is false; stale upstream is ignored when `auto_rebuild_after_sync` is false. If anything stale remains, the hub queues one follow-up rebuild through the enqueue of package 33, with the job's strategy and fail mode and `submitted_by: "system:followup"`, and emits `parley.rebuild.followup` on the job that ended, with `follow_up_job_id`, `stale_patches` (branch names) and `upstream_stale`. If the enqueue is refused as a duplicate, it is skipped silently: no job, no event. No check is made after a retryable failure, because the job itself runs again and reads fresh inputs.

### Outcome and events (CP-FR-052, §10.8)

A rebuild that published ends `completed`: the four counts are set from `patch_results` (`patches_removed` is the number of merged patches the run soft-deleted), `error` and `conflict` are cleared, and `parley.rebuild.complete` is emitted with `upstream_head_sha`, `integration_head_sha`, `previous_integration_head_sha` and the four counts.

Every failed attempt emits `parley.rebuild.fail` with `reason`, `will_retry` (true when the job waits to retry, false when it is dead-lettered), `retry_count` and, when the failure was a conflict, `conflict`. The job's `error` holds the text of the failure.

Rebuild events have `resource_type: "rebuild"`, the job id as `resource_id`, actor `system`, and the job's session id when it has one. Each is written with the state change it reports. Text that goes into `error` or an event passes through the hub's scrubber first, so that no credential and no URL user information appears in it.

At the end of every attempt, whatever its outcome, the scratch working tree is removed. An attempt begins by discarding a scratch working tree left by an earlier attempt of the same workspace.

### End-to-end flows

**A rebuild that publishes.** A developer runs `parley rebuild submit acme-fork --wait`. `parleyd` queues the job; a worker starts the attempt; the hub fetches upstream, replays the first patch, skips the patch that sync marked `merged_upstream`, replays the third, moves `deploy` to the new head, soft-deletes the merged patch, pushes `deploy` to origin and completes the job. The developer reads the final job record: `completed`, two patches applied, one skipped and removed, `integration_branch_pushed: true`.

**A conflict that the hub learns to resolve.** An agent submits a rebuild after upstream changed a line its patch also changes. The attempt stops at the patch (`fail_fast`): the job is `dead_letter` with a conflict report, the patch is `conflict`, `deploy` has not moved. A developer merges upstream into the patch branch, resolving the conflict, and the dead-lettered job is requeued. The new attempt meets the same conflict, learns the resolution from the merge commit, applies the patch, publishes, and returns the patch to `active`.

## Design Decisions

1. **Contract with the queue of package 33.** This package implements the handler interface of `internal/jobs` as package 33 declares it: a run operation called once per attempt with the job and a context that is cancelled at shutdown, and a settlement hook the queue calls inside the store transaction of each settlement with the job as settled and whether it will be retried. The run operation returns nil (completed), a plain error (retryable), `Permanent(err)` (permanent) or an error with `WithDetail` (the conflict report, which the queue stores as the job's `conflict`); it stores progress and the final counts with `SetResult` while the job is `running`. The hook writes `parley.rebuild.complete`, `parley.rebuild.fail` (with the hook's `will_retry`) and `parley.rebuild.followup` with the state change they report. Nothing in `internal/jobs` is changed by this package.
2. **Order at the end of a run.** Publish with the patch bookkeeping (one store transaction around the compare-and-swap), then the push, then the stale-input check and the follow-up enqueue, then the return to the queue, whose settlement completes the job and calls the hook. The job stays `running` through the push, so that a caller who waits for `completed` reads the final `integration_branch_pushed`, and through the follow-up enqueue, because package 33 accepts a follow-up only for a job that is `running`. If the hub stops between the publish and the completion, the job runs again from the start and publishes again; nothing is lost.
3. **Store and ref together.** The patch bookkeeping is prepared in a store transaction, the compare-and-swap is made, and the transaction commits only when the swap won. A ref and a database cannot commit as one; this order makes a lost swap leave no trace in the patch list.
4. **Scratch working tree.** A detached git working tree of the workspace's repository at `<slug>.git/parley-rebuild/<job id>`. It gives rerere a working tree and adds no ref. One fixed place lets the next attempt, and package 38 at start-up, discard leftovers.
5. **Remembered resolutions are git rerere** with the repository's own `rr-cache`, so they are per workspace and vanish with a reclone, as §10.10 says. Git does not keep the path of a conflict, so the hub records id, path and time in the transactional store (`internal/carrypatch/rerere_entries.go`); `resolved` is read from git. This record is the only one: package 38 lists, counts and forgets these entries through the reads and the removal this package exports, and keeps no file of its own. The entries of a workspace are removed with it (a remover registered with package 14) and after every fresh clone (a post-clone step registered with package 10).
6. **How a resolution is learned.** The PRD says only that "the resolution is learned when a later rebuild sees the same conflict resolved". The hub never edits files itself, so the one place a resolution can be seen is the patch branch: a merge commit there is a conflict the developer resolved. Redoing that merge with rerere on is git's own training method and records exactly the developer's result. It is done only when a conflict remains, and for at most the newest 50 merge commits of the patch, so a clean rebuild costs nothing.
7. **Merge commits made by the hub** carry the hub's landing identity as author as well as committer: there is no original author to keep. With `merge`, a tip already contained in the head under construction is `nothing_to_apply`; the PRD defines that reason for `rebase` only.
8. **Patch status at a conflict** is written when the conflict is found, in both fail modes (state table of §10.3.3), not at the publish. `parley.patch.update` is emitted only when the status changed; a patch that was already `conflict` gets new `conflict_files` and no event.
9. **Bookkeeping follows the present patch list.** A patch whose status a caller changed during the run is treated by its present status at the publish: one no longer `merged_upstream` is not deleted, one no longer `conflict` is not touched. The renumbering after a removal emits no event of its own.
10. **No `parley.git.push` for a move of the integration branch by the hub.** §10.8 says updates of the integration branch also produce the general `parley.git.push` event, but that event's `via` is `git`, `api` or `land` (§5.8) and it is defined for accepted client pushes (GIT-FR-060); no client can push the integration branch. A rebuild's publish is reported by `parley.rebuild.complete`, which carries both heads, and a rollback by `parley.rebuild.rollback` (package 35 records the same decision).
11. **`reason` values of `parley.rebuild.fail`** (the PRD names the field, not its values): `conflict`, `upstream_fetch_failed`, `upstream_unknown`, `git_error`, `timeout`, `shutdown`, `ref_moved`, `workspace_busy`, `workspace_not_ready`, `workspace_archived`. The last three reuse the names of WS-FR-007.
12. **The stale-input check fetches nothing** and compares with the refs as they are. A patch branch that was deleted during the run is not stale, in line with CP-FR-087 (a delete triggers no rebuild).
13. **The push of the integration branch is limited to 120 seconds**, the limit CP-FR-088 gives the mirror; §10.9 calls the push limit fixed without stating it for this push.
14. **Conflict report files** come from the report component of package 26 (`internal/land/`) through a narrow interface of the rebuild, the conflict-file describer, which `internal/hub/` wires. The report builder of this package is exported; the preview of package 35 builds its reports with it, so both are one type.
15. **A lost compare-and-swap** is `ErrRefMoved` of `internal/gitrepo` (package 12). In a rebuild it is the retryable failure reason `ref_moved`; the request routes that meet it (reset to origin in package 32, rollback in package 35) answer `409 ref_moved` with `details.current_sha`.
16. **Git runs through the carry-patch runner of package 31** (`internal/carrypatch/gitrun.go`), which holds the 10-minute limit of CP-FR-093 and cleans error text; its `ErrGitTimeout` is the reason `timeout`. The push uses the lease push operation of package 13 under its 120-second limit, passed to the runner as the shorter deadline.
17. **The follow-up is queued through `RebuildEnqueuer`** (declared by package 31, implemented by package 33) with the follow-up-of id, while the job is still `running`; the event `parley.rebuild.followup` is written in the settlement hook. The upstream fetch of an attempt is `EnsureSetup` of package 31, which also repeats a missing set-up step (CP-FR-005).
18. Project-wide: Go; the `git` command (2.38 or later) run as a subprocess through `internal/gitrepo`, with no git library, a scrubbed environment and no hooks, filters or drivers from repository content (NFR-023); SQLite through `internal/store`; tests with the standard `testing` package, table-driven, with repositories made by `git init` in `t.TempDir()`; a replaceable clock.

## Dependencies

- Package 33 rebuild_jobs (draft): the durable queue that calls this handler, the rebuild job record this package fills, the enqueue the follow-up uses, and the submit, read and requeue routes the end-to-end flows drive. Through it package 31 (the carry-patch workspace, the patch list, the upstream fetch and credentials), package 12 (the origin fetch that records `refs/remotes/origin/*`) and package 10 (the workspace record, its repository and the origin credential resolver).
- Package 52 durable_event_log_and_stream (draft): the event component every event of this package is emitted through, in the transaction of the state change.
- Package 26 conflict_reports_and_resolution (draft), added to the plan's list: the conflict report component of `internal/land/` for the `files` of a report.
- Package 13 workspace_push_to_origin (draft), added to the plan's list: the push operation with a lease in `internal/gitrepo/push.go`, used for the push of the integration branch.
- Package 14 workspace_archive_reclone_delete (draft), added to the plan's list: the removers of owned data, with which the remover of remembered-resolution entries is registered.
- Used through those: the scrubber of package 03 (`internal/scrub/`) and the post-clone step registry of package 10.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Origin**: A registered workspace's remote (`git_url`).
- **Carry-patch workspace**: A workspace that tracks an upstream project and keeps an ordered list of patches on top of it.
- **Upstream**: For a carry-patch workspace, the project it tracks (`upstream_url`).
- **Integration branch**: For a carry-patch workspace, the branch the hub rebuilds as upstream plus the ordered patches.
- **Patch**: A carry-patch entry: a branch with a position in the patch order and a status.
- **Rebuild**: A durable job that recreates the integration branch.
- **Durable job**: Background work that is recorded before its request returns, survives restarts and is retried with backoff. Rebuilds are the only durable jobs (§10).
- **Conflict report**: The structured description of why a rebase could not be done: files, kinds, and the landed changes involved.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number.
- **Upstream base**: The commit every rebuild starts from: the head of the upstream's default branch as of the last upstream fetch.
- **Patch branch**: The branch that holds a patch's commits.
- **Head under construction**: The commit a run has built so far: the upstream base, then the result of each applied patch.
- **Attempt**: One execution of a rebuild job by a worker. A job that fails with a retryable failure gets another attempt.
- **Remembered resolution**: A recorded way a conflict was resolved, which the hub replays when the same conflict appears again (git rerere).
- **Scratch working tree**: The temporary working tree a run builds in; no ref points to it.
- **Stale input**: A patch tip or an upstream base that changed after the run read it.
