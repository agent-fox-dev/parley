---
spec_id: "33"
spec_name: "rebuild_jobs"
title: "Queue rebuilds as durable jobs"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "058684f54235dfd3024a2d8b6d3f828f0bf574cb917c2ec9ff6d50b2a95205f7"
schema_version: 3
source: "docs/prd/10_carry_patch.md"
---

# Queue rebuilds as durable jobs

## Intent

A rebuild of a carry-patch workspace can take minutes and can fail for reasons that pass: an upstream that is unreachable, a lost race on a ref, a restart of the hub. A caller must be able to ask for a rebuild once and trust that it will run: the request is recorded before it is answered, it survives a restart, it is retried with growing delays, and there is never more than one pending rebuild per workspace. This package gives the hub a durable job queue with exactly those guarantees, and puts the rebuild job in front of callers: submit, list, read, cancel and requeue, over the REST API and the `parley rebuild` commands.

## Goals

- A job is stored in the transactional store before the request that created it is answered, and is readable after the store is closed and opened again.
- A job moves only along the state machine `queued`, `running`, `failed`, `completed`, `dead_letter`, `cancelled`; `failed` is never an end state.
- A retryable failure is retried after `min(2 s × 2ⁿ, 2 h)` at most 20 times; the 21st retryable failure, and any permanent failure, dead-letters the job.
- At most one rebuild job per workspace is `queued`, `running` or `failed`, with the single exception of the follow-up a running rebuild queues for itself.
- At most 4 jobs run at once across the hub and at most one per workspace; a queued job starts within 5 seconds when a worker is free.
- A job found `running` when the hub starts returns to `queued` with its `retry_count` unchanged and runs again from the start.
- `POST /workspaces/:slug/rebuild` answers `202` with the rebuild job record; `GET …/rebuilds`, `GET …/rebuilds/:id`, `DELETE …/rebuilds/:id` and `POST …/rebuilds/:id/requeue` list, read, cancel and requeue jobs.
- `parley rebuild submit|list|status|cancel|requeue` call those routes; `--wait` waits through `failed` and ends only on `completed`, `dead_letter` or `cancelled`.
- Every other package that queues a rebuild uses one enqueue operation with the same dedup.

## Non-goals

- What a rebuild does (upstream fetch, patch order, conflicts, publication, the stale-input check, the push of the integration branch), the events `parley.rebuild.complete`, `parley.rebuild.fail`, `parley.rebuild.push_failed` and `parley.rebuild.followup`, and which failures are permanent or retryable (CP-FR-040 to CP-FR-055): package 34 `rebuild_execution`. This package runs jobs through a handler interface and gives the handler the hooks those events need.
- `POST …/rebuilds/:id/rollback`, `GET …/rebuild-preview`, `parley rebuild rollback` and `parley rebuild preview` (CP-FR-060 to CP-FR-066): package 35 `rebuild_rollback_and_preview`.
- The callers of the shared enqueue other than `POST …/rebuild`: sync (package 36 `carry_patch_sync`), reset to origin (package 32 `patch_maintenance`), updates of a patch branch and landing on one (package 37 `carry_patch_push_control`), the stale-input follow-up (package 34). Each decides what it answers when the enqueue reports a duplicate.
- Carry-patch workspace creation, the patch list and the common carry-patch precondition check (CP-FR-001 to CP-FR-018): package 31 `carry_patch_setup_and_patches`. This package calls that check.
- Discarding what an interrupted rebuild left in the repository at start (the second half of CP-FR-095), the purge of deleted patches and the patch-status summary (CP-FR-089 to CP-FR-097): package 38 `carry_patch_rerere_and_maintenance`.
- The workspace settings `rebuild_strategy` and `rebuild_fail_mode` (their storage and validation): package 11 `workspace_read_update_settings`. This package reads them.
- The removal step of a workspace delete: package 14 `workspace_archive_reclone_delete`. This package supplies the remover for job records.
- The exposition of `parley_jobqueue_depth` on `/metrics`: package 51 `audit_query_and_retention`. This package supplies the counts.
- The error envelope, the order of checks, list paging and `Idempotency-Key` (package 02 `http_api_conventions`); the access rule (package 07 `workspace_access_rule`); the CLI's settings, output, exit codes and shared waiting (package 09 `cli_foundation`); the event log (package 52 `durable_event_log_and_stream`).
- `parley workspace sync --wait` and `parley patch reset-to-origin --wait` (packages 12, 36 and 32). Neither package depends on this one; each hands the terminal states of a rebuild job (`completed` successful; `dead_letter` and `cancelled` unsuccessful; `failed` not terminal) to the shared wait of package 09, as `submit --wait` does.
- Job types other than `rebuild`, configurable retry or concurrency values, and more than one `parleyd` process on a data directory: not in this version (§10.9, §10.10).

## Background

The repository holds no code yet; every file named here is new, except the files earlier packages create and this package extends (`internal/hub/` of package 03 for the start hook and background work, the router of `internal/server/`, the root command of `internal/cli/`).

The source PRD (`docs/prd/10_carry_patch.md`) describes the durable job model in §10.3.4, the rebuild job record in §10.3.5, rebuild submit in CP-FR-034 to CP-FR-039, list, get, cancel and requeue in CP-FR-056 to CP-FR-059, the routes in §10.5, the commands in §10.6, the workers and the metric in §10.7 and the events in §10.8. Rebuild is the only job type in this version; the model is written so that it can carry other types later.

The work falls into two places:

- `internal/jobs/` holds the general queue: the job record (`job.go`), its persistence and the schema migration registered with `internal/store/` (`store.go`), enqueue and dedup (`enqueue.go`), the dispatcher and the workers (`queue.go`), the settlement of an attempt and the retry policy (`settle.go`), crash recovery and shutdown (`lifecycle.go`). It knows nothing about rebuilds: a job has a type, a workspace, a payload document and a result document, and is executed by the handler registered for its type.
- `internal/carrypatch/` holds what is specific to rebuild jobs: the shared enqueue (`rebuild_enqueue.go`), the rebuild job record (`rebuild_record.go`) and the five routes (`rebuild_routes.go`). The handler that performs a rebuild is package 34's and is registered under the job type `rebuild`.

The commands live in `internal/cli/rebuild.go`. Tests live beside the code (`internal/jobs/queue_test.go`, `internal/carrypatch/rebuild_routes_test.go`, `internal/cli/rebuild_test.go`); the end-to-end tests are in `test/e2e/rebuild_jobs_test.go`.

## Requirements

### The job and its states (§10.3.4)

A job has an id (UUID), a type, a workspace slug, a status, a payload document fixed at enqueue, a result document the handler writes, `retry_count`, `next_attempt_at`, the text of the last failure (`error`), an optional detail document of the last failure, `created_at`, `updated_at` and `completed_at`.

| Status | Meaning | Final |
|---|---|---|
| `queued` | Waiting for a worker. | no |
| `running` | A worker is executing it. | no |
| `failed` | The last attempt failed with a retryable error. The job is waiting to retry. | no |
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
| `running` | `queued` | The hub starts and finds the job still marked running. `retry_count` is unchanged. |
| `dead_letter` | `queued` | Requeue. `retry_count` returns to 0. |

No other transition exists. `failed` is never final: nothing in the hub or the CLI treats it as an end state.

`completed_at` is set when the job reaches `completed`, `dead_letter` or `cancelled`, and is null otherwise, also after a requeue. `next_attempt_at` is set only while the job is `failed`. `error` is cleared when the job completes or is requeued; it is kept while the job is retried.

### Enqueue and dedup (§10.3.4, CP-FR-039)

Enqueue stores a new job as `queued` with `retry_count` 0, inside the store transaction of its caller, so that the job and the event that reports it are committed together and before the request is answered.

At most one job per job type and workspace may be `queued`, `running` or `failed`. An enqueue that would create a second one is refused: it stores nothing and reports a duplicate that names the existing job's id. Of several concurrent enqueues for one workspace exactly one stores a job.

The single exception is the follow-up: an enqueue that names a job of the same type and workspace that is `running`, while that job is the only one of the workspace that is `queued`, `running` or `failed`. It is stored as `queued` and starts only after the running job has ended. A follow-up that names a job that is not `running`, or that comes while another follow-up is already queued, is a duplicate.

### Workers (§10.3.4, §10.7)

The hub runs the jobs itself; there is no separate worker process. A dispatcher looks for work at least once per second and whenever a job was queued. A queued job starts within 5 seconds when a worker is free.

- At most 4 jobs run at once across the hub.
- At most one job per job type and workspace runs at a time: a queued job is not claimed while another job of its type and workspace is `running`.
- Queued jobs are claimed oldest first.
- A given job is claimed by at most one worker. A claim sets the status to `running` and empties the result document.
- A queued job whose type has no registered handler is not claimed and stays `queued`.
- A `failed` job whose `next_attempt_at` is reached returns to `queued` with `next_attempt_at` null, and is then claimed like any queued job.

### The outcome of an attempt (§10.3.4)

The handler receives the job and a context, may store a result document on the running job at any time (readers see it at once), and returns nil or an error. An error is retryable unless the handler marked it permanent; an error may carry a detail document.

- **Success.** The job becomes `completed`; `error` and the failure detail are cleared; `completed_at` is set.
- **Retryable failure with `retry_count` below 20.** The job becomes `failed`; `retry_count` goes up by one; `next_attempt_at` is the present plus the delay; `error` holds the failure's text. The delay after the n-th retryable failure is `min(2 s × 2ⁿ, 2 h)`: 4 s, 8 s, 16 s, … 4096 s after the 11th, and 2 h from the 12th on.
- **Retryable failure with `retry_count` 20** (the 21st). The job becomes `dead_letter` with `retry_count` still 20.
- **Permanent failure.** The job becomes `dead_letter` at once; `retry_count` is unchanged; it is not retried.
- **A handler that panics** counts as a retryable failure whose `error` names the panic; the hub keeps running.

On every failure the detail document is replaced by the one the error carries, or cleared when it carries none. `dead_letter` sets `completed_at`.

Each settlement is one store transaction. Inside it the queue calls the settlement hook the handler's type registered, with the job as it now is and whether it will be retried, so that the type's events are written with the state change. A hook that fails has its own writes undone and is logged; the settlement still commits.

### Crash recovery and shutdown (§10.3.4, CP-FR-095, CP-FR-096)

At start, before any worker claims a job, every job found `running` returns to `queued` with `retry_count`, `error` and the payload unchanged. It then runs again from the start. This is safe for rebuilds because a rebuild changes nothing visible until it publishes.

At shutdown the queue claims no further job and gives running handlers 30 seconds. The outcome of a handler that returns within that time is settled as usual. After 30 seconds the contexts of the remaining handlers are cancelled and the queue stops without settling them: their jobs stay `running` in the store and are recovered at the next start.

### Counts and removal (§10.3.4, §10.7)

The queue reports the number of jobs for each job type and each of the six statuses, zero included; this is the source of the metric `parley_jobqueue_depth{type,status}`.

Job records are kept for as long as the workspace exists. The queue removes all jobs of a workspace inside the transaction of the workspace's removal. A handler still running for a removed job has its context cancelled, and its outcome is dropped.

### The rebuild job record (§10.3.5)

Every rebuild route that returns a job returns this object with every field present (`null` when unset).

| Field | Type | Meaning |
|---|---|---|
| `id` | UUID | Job id. |
| `workspace_slug` | string | |
| `status` | enum | One of the six statuses. |
| `strategy` | `rebase` \| `merge` | Captured at enqueue. |
| `fail_mode` | `fail_fast` \| `continue` | Captured at enqueue. |
| `integration_branch` | string | Captured at enqueue. |
| `submitted_by` | string | A user id, or `system:push`, `system:land`, `system:followup`. |
| `retry_count` | integer | Retryable failures so far. |
| `next_attempt_at` | timestamp, nullable | Set only while `failed`. |
| `error` | string, nullable | Text of the last failure. |
| `conflict` | conflict report, nullable | The detail document of the last failure. |
| `created_at`, `updated_at` | timestamp | |
| `completed_at` | timestamp, nullable | |
| `upstream_head_sha`, `integration_head_sha`, `previous_integration_head_sha` | string, nullable | From the result document. |
| `patches_applied`, `patches_skipped`, `patches_conflicted`, `patches_removed` | integer | From the result document; 0 when it has none. |
| `integration_branch_pushed` | boolean | From the result document; `false` when it has none. |
| `patch_results` | array | From the result document; `[]` when it has none. |

The first four captured fields are the job's payload. The fields "from the result document" are written by the rebuild handler of package 34 under these same names; this package returns what the document holds and the stated defaults for what it lacks.

### Rebuild submit (CP-FR-034 to CP-FR-039)

`POST /workspaces/:slug/rebuild` queues a rebuild. Scope `rebuilds:write`; write access. The body is optional: no body, an empty body and `{}` are equal. `strategy` must be `rebase` or `merge`; `fail_mode` must be `fail_fast` or `continue`; any other value is `400` `invalid_request`. An omitted, null or empty value falls back to the workspace setting `rebuild_strategy` (default `rebase`) or `rebuild_fail_mode` (default `fail_fast`).

After validation and the common carry-patch checks (below), in this order:

1. A workspace with no patch in `active` or `conflict` has nothing to rebuild: `400` `no_active_patches`.
2. A workspace that already has a rebuild job `queued`, `running` or `failed`: `409` `concurrent_rebuild`, and the message names the existing job's id.

Otherwise the answer is `202` with the job record: `status` `queued`, `submitted_by` the caller's user id, `retry_count` 0. Strategy, fail mode and integration branch are captured in the job; later changes of the settings do not affect a job that exists. The hub writes `parley.rebuild.enqueue` in the same transaction as the job: `resource_type` `rebuild`, `resource_id` the job id, actor the caller, metadata `submitted_by`, `strategy`, `fail_mode`, `patch_count`.

The route accepts `Idempotency-Key`: a replay answers `200` with the original job record and queues nothing.

**The shared enqueue.** Every source of a rebuild uses one operation with the same dedup. It takes the workspace, an optional strategy and fail mode (falling back to the settings), `submitted_by`, the actor and session of the event, and, for a follow-up, the running job it follows. It does not apply the `no_active_patches` check: automatic triggers queue a rebuild also when every patch is disabled or merged (CP-FR-055). On a duplicate it stores nothing, writes no event and reports the existing job's id; the source decides what to answer:

| Source | `submitted_by` | Event actor | On duplicate |
|---|---|---|---|
| `POST …/rebuild` | caller's user id | caller | `409` `concurrent_rebuild` |
| Sync, reset to origin | caller's user id | caller | skipped by the source |
| Update of a patch branch | `system:push` | `system` | skipped by the source |
| Landing on a patch branch | `system:land` | `system` | skipped by the source |
| Stale-input follow-up | `system:followup` | `system` | skipped by the source |

### Rebuild list and get (CP-FR-056, CP-FR-057)

`GET /workspaces/:slug/rebuilds` returns the rebuild jobs of the workspace as `{"items":[job,…],"next_cursor":…}`, newest `created_at` first. Scope `rebuilds:read`; read access. Query: `status` (one of the six statuses; any other value is `400` `invalid_request`), `limit`, `cursor`.

`GET /workspaces/:slug/rebuilds/:id` returns the job record. A job that is `running` shows the result document as the handler last stored it.

On every job route an id that names no job, names a job of another workspace, or is not a UUID is `404` `not_found` with the message `rebuild job not found`.

### Cancel and requeue (CP-FR-058, CP-FR-059)

`DELETE /workspaces/:slug/rebuilds/:id` cancels a queued job. Scope `rebuilds:write`. A `queued` job becomes `cancelled`, `completed_at` is set, the answer is `200` with the job record, and `parley.rebuild.cancel` is written (actor the caller, no metadata). Any other status, including a job a worker claimed a moment earlier, is `409` `job_not_cancellable`. Cancel and claim exclude each other: a cancelled job's handler is never called.

`POST /workspaces/:slug/rebuilds/:id/requeue` (no body; scope `rebuilds:write`) moves a `dead_letter` job back to `queued` with `retry_count` 0, `error`, `conflict` and `completed_at` null, and writes `parley.rebuild.requeue` (actor the caller, no metadata). The answer is `200` with the job record. The job keeps its id, `created_at`, strategy, fail mode, integration branch and `submitted_by`, and reads fresh inputs when it runs. Any status other than `dead_letter` is `409` `job_not_requeueable`. While another rebuild job of the workspace is `queued`, `running` or `failed`, requeue is `409` `concurrent_rebuild`.

### Access and common checks (CP-FR-006 to CP-FR-008, as they apply to these five routes)

The routes follow the global check order. What a caller observes:

| Case | Answer |
|---|---|
| PAT without `rebuilds:read` on a read route, or without `rebuilds:write` on a write route | `403` `insufficient_scope` |
| No access to the workspace, or unknown slug | `404` `not_found` |
| Read access only, on a write route | `403` `forbidden` |
| Workspace not in carry-patch mode (a standard workspace, a task fork) | `400` `workspace_mode_mismatch` |
| Write route, workspace `pending`, `cloning`, or with a running reclone, archive or delete | `409` `workspace_busy` |
| Write route, workspace `failed` | `409` `workspace_not_ready` |
| Write route, workspace `archived` | `409` `workspace_archived` |

The two read routes work in every workspace state. Request validation (`400` `invalid_request`) comes before the mode check; the mode check before the status check; both before the job lookup (`404`) and before `no_active_patches`, `concurrent_rebuild`, `job_not_cancellable` and `job_not_requeueable`.

### CLI (§10.6)

The global CLI conventions apply. Unless stated, a command prints the hub's response.

| Command | Flags | Calls |
|---|---|---|
| `parley rebuild submit <workspace-slug>` | `--strategy <rebase\|merge>`, `--fail-mode <fail_fast\|continue>`, `--wait`, `--timeout` (default 5m), `--poll-interval` (default 5s) | `POST /workspaces/:slug/rebuild`; a flag that was not given is not sent |
| `parley rebuild list <workspace-slug>` | `--status <status>` | `GET /workspaces/:slug/rebuilds` |
| `parley rebuild status <workspace-slug> <rebuild-id>` | | `GET /workspaces/:slug/rebuilds/:id` |
| `parley rebuild cancel <workspace-slug> <rebuild-id>` | | `DELETE /workspaces/:slug/rebuilds/:id` |
| `parley rebuild requeue <workspace-slug> <rebuild-id>` | `--wait`, `--timeout`, `--poll-interval`, as for `submit` | `POST /workspaces/:slug/rebuilds/:id/requeue` |

With `--wait`, `submit` and `requeue` then poll `GET /workspaces/:slug/rebuilds/:id` until the job is `completed`, `dead_letter` or `cancelled`. `failed` means the job waits to retry, so polling continues. The output is the final job record. Exit status 0 when the job is queued (no `--wait`) or `completed`; 1 when the wait ends in `dead_letter` or `cancelled` (the final record on stdout, `wait_failed` on stderr) or times out (nothing on stdout, `wait_timeout` on stderr).

A value of `--strategy`, `--fail-mode` or `--status` outside its list, and a wrong number of arguments, is a usage error: exit status 2, no request.

### End-to-end flows

1. **Submit and wait through a retry.** A developer runs `parley rebuild submit <slug> --wait` on a carry-patch workspace with one active patch. The CLI posts; `parleyd` stores the job and `parley.rebuild.enqueue` and answers `202`; a worker claims the job and runs the handler, which fails with a retryable error; the job is `failed` with `retry_count` 1 and `next_attempt_at` 4 seconds later; the CLI keeps polling; the job returns to `queued`, runs again and completes; the CLI prints the final record with `status` `completed` and exits 0.
2. **Restart during a run.** An agent posts a rebuild; a worker claims it; the hub stops before the handler returns; the hub starts again; the job returns to `queued` with `retry_count` 0, is claimed and its handler runs from the start; the agent reads the job and sees `completed`.
3. **Dead letter, cancel and requeue.** An agent posts a rebuild; the handler fails with a permanent error and the job is `dead_letter`; the agent posts a second rebuild and gets `202`; a requeue of the first job is `409` `concurrent_rebuild`; the agent cancels the second job (`200`, `cancelled`); a second cancel is `409` `job_not_cancellable`; the requeue of the first job now answers `200` with `status` `queued` and `retry_count` 0; the event log holds two `parley.rebuild.enqueue`, one `parley.rebuild.cancel` and one `parley.rebuild.requeue`.

## Design Decisions

1. **Project decisions relied on.** Go; the transactional store is SQLite through `internal/store`; HTTP is `net/http`; the CLI is cobra through the command helper of package 09; tests use `testing`, `net/http/httptest` and a real store in `t.TempDir()`; code takes a replaceable clock.
2. **Two layers.** `internal/jobs` is a general queue that knows a job's type, workspace, payload and result as opaque JSON documents; `internal/carrypatch` gives rebuild jobs their fields and routes. Why: §10.3.4 says the model must carry other types later.
3. **The handler interface.** A type registers a handler with two operations: one that runs an attempt (context and job in, error out) and one settlement hook called inside the transaction of each settlement with the job and whether it will be retried. An error is retryable unless wrapped as permanent; an error may carry a detail document. Why: package 34 must write `parley.rebuild.complete` and `parley.rebuild.fail` with the state change (§10.8), and only the queue knows `will_retry`.
4. **A failing settlement hook does not block the settlement.** Its writes are undone, the failure is logged, the state change commits. Why: a job stuck in `running` would block the workspace until a restart; package 14 treats its collaborators the same way.
5. **Dedup and "one at a time" are per job type and workspace.** The PRD says "per workspace" with one job type; this is the same rule for a second type.
6. **`retry_count` stays 20 when the 21st retryable failure dead-letters the job.** Why: the transition table raises it only on `running` to `failed`.
7. **A claim empties the result document.** Why: `patch_results` is "the results so far" of the running attempt, and an attempt runs from the start.
8. **`error` survives a retry and crash recovery** and is cleared only by completion and requeue, as §10.3.5 states.
9. **A panic in a handler is a retryable failure.** Why: library code must not end the process, and the cause may pass.
10. **A type without a handler leaves its jobs `queued`.** Why: the simplest behaviour, and it lets this package work before package 34 registers the rebuild handler.
11. **Dispatcher.** It passes at least once per second and is woken by an enqueue, a requeue and the end of an attempt; tests run single passes with a controlled clock. Why: "a queued job starts within a few seconds" (§10.7), fixed here at 5 seconds.
12. **Claim order** is `created_at` ascending, then id. The PRD does not state one.
13. **Shutdown.** Outcomes that arrive within the 30-second grace are settled; handlers still running at its end are cancelled and not settled, so their jobs are recovered at the next start with `retry_count` unchanged (CP-FR-096). Package 03 names this package as the one that stops rebuild jobs at shutdown; the queue is registered as background work with a 30-second grace and crash recovery as a start hook.
14. **Order of the submit checks:** `no_active_patches` before `concurrent_rebuild`, the order of CP-FR-035 and CP-FR-037.
15. **Requeue checks the job's own status before the workspace's other jobs:** a job that is not `dead_letter` answers `job_not_requeueable` also when another job is pending.
16. **The job lookup comes after the mode and status checks** (CP-FR-008 puts those directly after validation). A job id that is not a UUID is `404`, like an unknown id.
17. **List order** is `created_at` descending, then id descending, so that the order is total (PLAT-FR-014).
18. **`patch_count`** in `parley.rebuild.enqueue` is the number of patches in `active` or `conflict` when the job is queued. The PRD names the member only.
19. **An empty or null `strategy` or `fail_mode`** falls back to the setting ("omitted or empty", CP-FR-034); a value of another JSON type is `400` `invalid_request`.
20. **The CLI checks enumerated flag values itself** (exit status 2, no request), because PLAT-FR-092 lists "invalid flag value" as a usage error.
21. **The command that reads one job is `parley rebuild status`**, as §10.6 and appendix B name it.
22. **Job ids** are random UUIDs (version 4) made with `crypto/rand`; no library.
23. **Removal with the workspace.** The queue exports the remover and registers it with the removers of owned data of package 14 (`internal/workspace/teardown.go`), which run inside the workspace's delete transaction.
24. **The shared enqueue implements `RebuildEnqueuer` of package 31.** Package 31 declares the interface, its options and result types and a default that queues nothing; `EnqueueRebuild` here is the implementation, registered when the hub is composed. Packages 32, 34, 36 and 37 call the interface, so package 32 needs no dependency on this package.
25. **The session of a trigger is kept in the job's payload**, beside the four captured fields, and is not part of the job record. §10.8 says a rebuild queued by a push or a landing carries that session; package 34 reads it for the job's events.
26. **One owner for start and shutdown (CP-FR-095, CP-FR-096).** This package owns the return of `running` jobs to `queued` at start, the stop of claiming at shutdown and the 30-second grace, with their criteria. Package 38 owns only the discard of what an interrupted rebuild left in the repository, as a start hook of its own, and specifies no queue behaviour.
27. **A follow-up is queued while the job it follows is still `running`.** The settlement hook sees the job as settled, so package 34 calls the enqueue with the follow-up-of id before its run operation returns, and writes `parley.rebuild.followup` in the hook.
28. **Profile: full.**

## Dependencies

- Package 31 `carry_patch_setup_and_patches` (draft): carry-patch workspaces, the patch list (for `no_active_patches` and `patch_count`), the integration branch name (`IntegrationBranch`), the `RebuildEnqueuer` interface and the common carry-patch precondition check.
- Package 09 `cli_foundation` (draft): the command helper, the HTTP client, output, exit codes and the shared `--wait`.
- Package 52 `durable_event_log_and_stream` (draft): writing an event in the transaction of its state change.
- Package 14 `workspace_archive_reclone_delete` (draft): the removers of owned data, with which the remover of job records is registered.
- Used through those: package 02 (error envelope, lists, idempotency), package 03 (`internal/hub` start hooks and background work, `internal/store`), package 07 (access), package 11 (workspace settings).
- Package 34 `rebuild_execution` depends on this package and registers the rebuild handler.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Carry-patch workspace**: A workspace that tracks an upstream project and keeps an ordered list of patches on top of it.
- **Integration branch**: For a carry-patch workspace, the branch the hub rebuilds as upstream plus the ordered patches.
- **Patch**: A carry-patch entry: a branch with a position in the patch order and a status.
- **Rebuild**: A durable job that recreates the integration branch.
- **Durable job**: Background work that is recorded before its request returns, survives restarts and is retried with backoff. Rebuilds are the only durable jobs (§10).
- **Conflict report**: The structured description of why a rebase could not be done: files, kinds, and the landed changes involved.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number.
- **Handler**: The code registered for a job type that performs one attempt of a job.
- **Settlement**: The recording of an attempt's outcome: `completed`, `failed` or `dead_letter`.
- **Follow-up**: The one rebuild job a running rebuild may queue for the same workspace; it starts after the running job ends.
- **Waiting to retry**: The status `failed`; never an end state.
