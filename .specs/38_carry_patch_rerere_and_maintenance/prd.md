---
spec_id: "38"
spec_name: "carry_patch_rerere_and_maintenance"
title: "Remember resolutions and keep carry-patch state tidy"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "18d228c6891e5916657f0c927b17611b600fa3d6b0fbe2e3d123906d17628489"
schema_version: 3
source: "docs/prd/10_carry_patch.md"
---

# Remember resolutions and keep carry-patch state tidy

## Intent

A team that carries patches for months needs to see what the hub has learned and stored on its behalf, to take back a wrong lesson, and to trust that the hub cleans up after itself. This package lets a developer list the conflict resolutions the hub remembers and forget one, gives every reader one summary of a carry-patch workspace, keeps the git commands of carry-patch work inside safe transports, time limits and argument rules, removes soft-deleted patches after their retention, and makes a stop and a start of the hub leave rebuild work in a state from which it runs again.

## Goals

- `GET /workspaces/:slug/rerere` lists every remembered-resolution entry of a carry-patch workspace, resolved or not, newest first, each with `id`, `path`, `resolved` and `recorded_at`.
- `DELETE /workspaces/:slug/rerere/*pathspec` removes one entry by id, or every entry of one file path, and answers `404` when nothing matches.
- `GET /workspaces/:slug/patch-status` answers one document with the workspace, the integration branch, the latest rebuild, every non-deleted patch and eight counts, from stored state alone.
- `parley rerere list`, `parley rerere forget` and `parley workspace patch-status` call these routes.
- The hub contacts upstream and origin over `https` and `ssh` only; one git operation of carry-patch work runs for at most 10 minutes.
- No credential appears in an error message, a job `error`, an event or a log line of carry-patch work, and no caller-supplied value is read by git as an option.
- A patch that has been `deleted` for more than 7 days is removed by the hourly maintenance pass, with its backup ref.
- After a start of the hub, nothing an interrupted rebuild left behind in a repository remains, and start-up never fails because of a leftover.

## Non-goals

- Turning remembered resolutions on after a clone, the upstream fetch and the upstream credentials: package 31 carry_patch_setup_and_patches.
- Recording a conflict, replaying a remembered resolution during a rebuild and learning a resolution: package 34 rebuild_execution. This package reads and removes the entries that package stores (id, path, time) and writes none.
- Soft delete and restore of a patch, patch update, reorder and reset to origin: package 32 patch_maintenance. This package removes what has stayed `deleted` for more than 7 days.
- The durable job model: job states, the worker pool, retries with backoff, dedup, the return of `running` jobs to `queued` at start, the stop of claiming at shutdown and the 30-second grace (the first half of CP-FR-095, and CP-FR-096), and the routes and commands for rebuild jobs: package 33 rebuild_jobs. This package specifies no queue behaviour; of CP-FR-095 it owns the discard of what an interrupted rebuild left behind.
- The rebuild algorithm and its conflict reports: package 34 rebuild_execution. Rollback and preview: package 35 rebuild_rollback_and_preview. The carry-patch part of sync: package 36 carry_patch_sync. Those packages run their git commands through the carry-patch git runner of package 31, which holds the limits whose requirements (CP-FR-093, CP-FR-094) this package owns and proves.
- Refusing client writes to the integration branch, patch branches and backup refs: package 37 carry_patch_push_control.
- Validation of `git_url` and `upstream_url` at workspace creation: package 10 workspace_create_and_clone.
- The maintenance loop, its task order, start hooks, the shutdown notification and the scrubbing of log lines: package 03 server_lifecycle_and_configuration.
- The error envelope, check order and list paging (package 02), the workspace access rule (package 07), the CLI's client, output and exit codes (package 09): used, not specified here.
- The metric `parley_jobqueue_depth`: package 51 audit_query_and_retention.

## Background

The repository holds no code yet; every file named here is new. The packages this one builds on deliver: `internal/gitrepo` with the git subprocess wrapper and the test origin helper `internal/gitrepo/gitrepotest/` (package 10); `internal/carrypatch/` with the patch store, the guard every carry-patch route uses, the integration branch accessor, the carry-patch git runner (`gitrun.go`, with `SanitizeGitText` in `internal/gitrepo/safety.go`) and the set-up that turns remembered resolutions on (package 31); the rebuild job queue in `internal/jobs/` (package 33); the rebuild executor in `internal/carrypatch/`, with the remembered-resolution entry store (`rerere_entries.go`) and the operation that discards scratch working trees (package 34); the maintenance task registry, start hooks and background work of `internal/maintenance/` and `internal/hub/` (package 03); the CLI client of `internal/cli/` (package 09).

This package adds `internal/carrypatch/rerere.go` (list and forget), `internal/carrypatch/patchstatus.go`, `internal/carrypatch/purge.go`, `internal/carrypatch/recovery.go`, the transport rule and the argument guard in `internal/gitrepo/safety.go`, and the commands in `internal/cli/rerere.go` and `internal/cli/patchstatus.go`.

The source is §10 of the product PRD (`docs/prd/10_carry_patch.md`): CP-FR-089 to CP-FR-097, the entry shape of §10.3.7, the three routes of §10.5, the three commands of §10.6 and the edge cases of §10.10 that concern them.

## Requirements

### Access and common preconditions (CP-FR-006 to CP-FR-008)

The three routes follow the hub's order of checks.

| Route | Scope | Access |
|---|---|---|
| `GET /workspaces/:slug/rerere` | `workspaces:read` | read |
| `DELETE /workspaces/:slug/rerere/*pathspec` | `workspaces:write` | write |
| `GET /workspaces/:slug/patch-status` | `workspaces:read` | read |

A PAT without the scope gets `403 insufficient_scope`. A caller with no access to the workspace gets `404 not_found`. A caller with read access only gets `403 forbidden` on the `DELETE`.

After request validation, in this order:

- the workspace is in carry-patch mode, else `400` with `error_type` `workspace_mode_mismatch`. A `standard` workspace and every task fork, also the fork of a carry-patch workspace, get this answer;
- for the `DELETE` only, the workspace's status allows the write, else `409 workspace_busy` (status `pending` or `cloning`, or a reclone, archive or delete is running), `409 workspace_not_ready` (`failed`) or `409 workspace_archived` (`archived`).

The two `GET` routes work in every workspace state.

### Remembered-resolution entries (§10.3.7)

The hub records how a conflict was resolved and replays the resolution when the same conflict appears again (git rerere). The resolutions are per workspace and live in the workspace's copy; they do not survive a reclone. The hub keeps one entry per recorded conflict in its store, written by the rebuild of package 34 and removed by it after a reclone and with the workspace. An entry has:

| Member | Type | Meaning |
|---|---|---|
| `id` | string | Identifier of the recorded conflict |
| `path` | string or `null` | File the conflict was in, when known |
| `resolved` | boolean | `true` when a resolution is recorded, `false` when only the conflict is |
| `recorded_at` | timestamp or `null` | Time the resolution was recorded, or the conflict when unresolved |

When a rebuild meets a conflict that no remembered resolution settles, package 34 stores an entry with the id git gave the conflict, the file it was in and the time; when a resolution is learned it sets the time anew. This package reads those entries and asks git, for each id, whether a resolution is recorded.

### List (CP-FR-089)

`GET /workspaces/:slug/rerere` answers `200` with `{"items":[entry,…],"next_cursor":…}`: every recorded entry, resolved or not, newest `recorded_at` first; entries with equal times are ordered by `id` ascending. `limit` and `cursor` follow the hub's list rules. While the workspace has no entry, which is always so before its first clone, the answer is `200` with `items: []` and `next_cursor: null`. The list changes nothing and contacts no remote.

### Forget (CP-FR-090, CP-FR-091)

`DELETE /workspaces/:slug/rerere/*pathspec` takes one path parameter, which may contain slashes; it is compared after percent-decoding, byte for byte.

- An empty value, or one that starts with `-`, is `400 invalid_request`.
- If the value equals the `id` of an entry, that entry is removed and nothing else.
- Otherwise every entry whose `path` equals the value is removed.
- If nothing matches, the answer is `404 not_found` with the message `resolution not found`, and nothing is removed. A prefix of a path, a path in another letter case and a value such as `../x` match nothing.
- Success is `204` with no body.

Forgetting an entry affects later rebuilds only. It changes no rebuild job, no patch and no ref, queues no rebuild and emits no event. A rebuild that starts afterwards and meets the same conflict finds no resolution to replay: the patch conflicts. Two forgets of the same entry at the same time never produce a `500`: one answers `204`, the other `204` or `404`.

### Patch status (CP-FR-092)

`GET /workspaces/:slug/patch-status` answers `200` with one document:

| Member | Meaning |
|---|---|
| `workspace_slug`, `workspace_mode`, `status` | From the workspace |
| `git_url`, `upstream_url`, `integration_branch` | From the workspace |
| `clone_error`, `sync_error` | String or `null` |
| `upstream_head_sha`, `last_sync_at` | String or `null` |
| `integration_head_sha` | Current head of the integration branch, or `null` when the branch does not exist |
| `last_rebuild` | `{id, status, created_at, completed_at}` of the newest rebuild job in any status, or `null` |
| `last_completed_rebuild` | The same for the newest `completed` rebuild, or `null` |
| `patches` | Non-deleted patches in position order: `id`, `branch_name`, `position`, `status`, `conflict_files`, `last_rebuild_result`, `origin_sync_state`, `origin_sha`, `origin_synced_at` |
| `summary` | `total_patches`, `active`, `conflict`, `disabled`, `merged_upstream`, `rerere_resolutions`, `patches_diverged`, `patches_missing_on_origin` |

`last_rebuild_result` is the status (`success`, `conflict` or `skipped`) of this patch in the newest rebuild job that has a result for it, or `null`. `total_patches` counts the non-deleted patches; the four status counts count them by status; `rerere_resolutions` counts the remembered-resolution entries, resolved or not; `patches_diverged` and `patches_missing_on_origin` count the non-deleted patches with that `origin_sync_state`. Every member is always present; a count with nothing to count is `0`. `patches` is not paged.

The route reads stored state only: the records and the workspace's copy. It fetches nothing, contacts neither origin nor upstream, and changes nothing. It answers in every workspace state; without a copy `integration_head_sha` is `null`.

### CLI (§10.6)

- `parley rerere list <workspace-slug>` calls `GET /workspaces/:slug/rerere` and prints the response.
- `parley rerere forget <workspace-slug> <id-or-path>` calls `DELETE /workspaces/:slug/rerere/<id-or-path>`. Slashes in the argument are kept as separators: each part between slashes is percent-encoded on its own. On success it prints `{"status":"forgotten","pathspec":"<arg>"}`.
- `parley workspace patch-status <workspace-slug>` calls `GET /workspaces/:slug/patch-status` and prints the response.

Each exits 0 on success. A hub error is printed on stderr as received and the command exits 1. A wrong number of arguments is a usage error: exit 2, nothing sent.

### Git safety: transports and time limit (CP-FR-093)

The hub contacts upstream and origin over `https` and `ssh` only. A git operation that would contact a remote by another transport (`http`, `git`, `file`, a local path, a remote helper such as `ext::`) is refused before any connection is made, and a remote that redirects to another transport is not followed.

A single git operation of a rebuild, a sync or a preview is limited to 10 minutes. Past the limit the git process and the processes it started are stopped. In a rebuild job this is a retryable failure: the attempt ends, the job waits to retry (or is dead-lettered when no retry is left), nothing is published, and the attempt is never recorded as a skip, a conflict or a success. In a request it is `500 internal`. The limit is on each operation, not on the rebuild as a whole.

### Git safety: credentials and arguments (CP-FR-094)

Credentials never appear in an error message, a job `error`, an event or a log line. In text taken from git, URL user information is replaced by `***@` (`https://user:secret@host/x` becomes `https://***@host/x`), and every credential value the hub resolved for the operation is removed.

Branch names, commit ids and path values supplied by callers are never interpreted as options of a git command: a value that begins with `-` is refused before git starts, and every other value is handed to git in a position where git reads it as a revision, a ref or a path.

### Leftovers of an interrupted rebuild (CP-FR-095)

At start, before any worker claims a job, whatever a rebuild interrupted by the stop left behind in a repository (its scratch working trees and their worktree registrations) is discarded, in every carry-patch workspace. No ref a client can read changes. Start-up never fails because of this: a leftover that cannot be removed, or a workspace whose copy is missing, is logged and the start goes on.

The other half of CP-FR-095 (every job found `running` returns to `queued` before workers start) and CP-FR-096 (no new job at shutdown, 30 seconds for running jobs, a stopped job runs again from the start) are specified by package 33, which owns the queue.

### Purge (CP-FR-097)

The hourly maintenance pass runs the task `patch_purge`. It removes every patch that has been `deleted` for more than 7 days (its `deleted_at` is more than 7 days before the pass), in every workspace whatever its status. A patch deleted exactly 7 days ago stays. With the patch the task deletes `refs/parley/replaced/<branch>`, unless a non-deleted patch of the workspace uses the same branch. The patch branch itself is kept. A workspace without a copy has no backup ref; its patch records are removed all the same.

A restore that happens at the same moment wins: the patch is `active` afterwards and its backup ref is still there. A failure on one patch is logged with the workspace and the patch id; that patch stays `deleted` and is tried again on the next pass, and the other patches are still purged. The purge emits no event.

### End-to-end flows

1. A developer runs `parley rerere list demo` after a rebuild that conflicted and a later one that saw the conflict resolved; `parleyd` reads the workspace's entries from its store, asks git which are resolved, and the command prints one resolved entry with its path. The developer runs `parley rerere forget demo src/net/dial.go`; `parleyd` removes the entry; a second list prints no entry.
2. An org member with read access runs `parley workspace patch-status demo`; `parleyd` builds the summary from its records and its copy without contacting origin or upstream, and the command prints it.
3. A developer soft-deleted a patch more than 7 days ago. The hour passes; `parleyd` runs the maintenance pass; the patch and its backup ref are gone, and a patch deleted yesterday is still there.
4. An operator stops `parleyd` while a rebuild runs and the rebuild does not finish within the grace. The operator starts `parleyd` again; it returns the job to `queued`, discards what the interrupted rebuild left, and a worker runs the job from the start; the integration branch is published once.

## Design Decisions

1. **Language and layout.** Go; git runs as a subprocess through `internal/gitrepo`; routes use the standard library router; the CLI uses cobra on the client of package 09; tests use `testing`, `net/http/httptest` and repositories made with `git init` in `t.TempDir()` (project decisions 1, 3, 4, 6, 7).
2. **An entry is the record package 34 stores for a conflict git recorded**: `id` (git's id of the conflict, the name of its directory under `rr-cache` in the copy), `path` and `recorded_at` in the transactional store, and `resolved` read from git. There is one record and this package keeps no other: it lists, counts and forgets through the reads and the removal package 34 exports. `path` and `recorded_at` stay nullable in the answer, as §10.3.7 gives them, although the hub always has both.
3. **Forget removes both halves of an entry**: the stored record, and git's own record of the conflict and its resolution (`rr-cache/<id>`), so that a later rebuild finds nothing to replay. Removal with the workspace and after a reclone is package 34's (a remover registered with package 14 and a post-clone step registered with package 10); this package registers neither.
4. **Order of the list.** `recorded_at` descending, then `id` ascending. The PRD says "newest first"; the tie-break makes the order total, which paging needs.
5. **Forget never builds a file path from the caller's value.** The value is compared with the ids and paths of the listed entries, and the entry is removed by its own id. A value such as `../x` therefore matches nothing and answers `404`.
6. **The `404` of forget uses `error_type` `not_found`** with the message `resolution not found`; §10.5 gives the message and no specific type.
7. **Forget emits no event and purge emits no event.** §10.8 lists none for either.
8. **`git_url` and `upstream_url` in the patch status are the stored values**, as the workspace object shows them; they carry no credentials because package 10 refuses a URL with user information.
9. **`ssh` covers both `ssh://` URLs and the `git@host:path` form.** Package 10 accepts only `https://` and `git@host:path` at creation; the transport rule here is the second line of defence at the moment git runs, and also stops a redirect to another transport.
10. **The 10-minute limit is applied by the carry-patch git runner of package 31** (`internal/carrypatch/gitrun.go`), with a limit tests can shorten. Every package from 31 to 38 runs git through it and through nothing else; none of 32 to 37 depends on this package, which is why the runner lives in the package they all depend on. This package owns CP-FR-093 and CP-FR-094: it adds the transport rule and the argument guard and proves the limit and the cleaning on a rebuild and on a request. Package 12's 2-minute deadline for the origin fetch of a sync and package 31's 2-minute single-branch fetch are shorter limits of their own and stay; the runner applies the smaller.
11. **`***@` and the hub's scrubber.** CP-FR-094 replaces URL user information by `***@`; PLAT-FR-098 removes it. Text that carry-patch code produces (API error messages, job `error`, event metadata) uses `***@`. Log lines also pass through the scrubber of package 03, which may remove the marker; the requirement for a log line is that no credential is in it.
12. **A value that begins with `-` is refused, not escaped.** No valid branch name or commit id begins with `-`, so nothing legitimate is lost.
13. **One owner per behaviour of CP-FR-095 and CP-FR-096.** Package 33 owns the return of `running` jobs to `queued` at start, the stop of claiming at shutdown and the 30-second grace, with their criteria. This package owns the discard of what an interrupted rebuild left in a repository, as a start hook of its own that calls the scratch clean-up package 34 exports, and the purge. Neither package specifies the other's part.
14. **The rebuild creates no ref for a run** (package 34 works in a detached scratch working tree under `parley-rebuild/` in the repository), so the leftovers are directories and worktree registrations only.
15. **Purge order.** For each due patch, in one store transaction: remove the record only if it is still `deleted` and still due, delete the backup ref when no non-deleted patch uses the branch, then commit. A failed ref deletion rolls the transaction back, so the next pass retries both. A restore that committed first makes the removal match nothing.
16. **Purge keeps the patch branch.** CP-FR-097 names the patch and the backup ref only; a soft delete keeps the branch (CP-FR-024).
17. **Purge runs for workspaces in every status**, including archived ones and ones without a copy; it touches nothing a client can write.
18. **Profile: full.**

## Dependencies

- Package 34 rebuild_execution (draft): the rebuild executor, whose stored remembered-resolution entries this package lists, counts and forgets, whose git operations run under the limits proved here, and whose scratch working trees the start hook discards. Through it: package 33 rebuild_jobs (the job queue, which owns crash recovery and the shutdown grace), package 31 carry_patch_setup_and_patches (the patch store, the carry-patch route guard, the integration branch accessor and the carry-patch git runner), package 10 workspace_create_and_clone (`internal/gitrepo`, the workspace record) and package 03 server_lifecycle_and_configuration (maintenance tasks, start hooks).
- Package 09 cli_foundation (draft): the CLI's HTTP client, output and exit codes, on which the three commands are built.
- The `git` command, 2.38 or later, at run time.

## Verified External API

- `github.com/spf13/cobra`: `type Command struct`, `func (c *Command) AddCommand(cmds ...*Command)`, `func ExactArgs(n int) PositionalArgs`. **Unverified**: the module is not in `go.mod` yet and was not read; the signatures are the ones the library documents.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Origin**: A registered workspace's remote (`git_url`).
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task.
- **Carry-patch workspace**: A workspace that tracks an upstream project and keeps an ordered list of patches on top of it.
- **Upstream**: For a carry-patch workspace, the project it tracks (`upstream_url`).
- **Integration branch**: For a carry-patch workspace, the branch the hub rebuilds as upstream plus the ordered patches.
- **Patch**: A carry-patch entry: a branch with a position in the patch order and a status.
- **Rebuild**: A durable job that recreates the integration branch.
- **Durable job**: Background work that is recorded before its request returns, survives restarts and is retried with backoff.
- **Remembered resolutions**: Conflict resolutions the hub records and replays when the same conflict appears again (git rerere).
- **Backup ref**: `refs/parley/replaced/<branch>`, the patch tip the hub discarded when it replaced the branch.
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
