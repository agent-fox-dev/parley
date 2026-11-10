---
spec_id: "25"
spec_name: "optimistic_landing"
title: "Land changes without a queue"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "f4b384e285ce4b88323032aab609739b9825dd8762957f61699fa1ce55272f43"
schema_version: 3
source: "docs/prd/08_landing_and_policy.md"
---

# Land changes without a queue

## Intent

Landing adds a change's commits to its target branch, and many agents land on one repository at the same time. Parley lands optimistically: `parleyd` rebases the change onto the current head of the target on the server and moves the target with a compare-and-swap, with no queue and no per-workspace lock, so that changes touching different things land side by side, a moved target only causes another try, and only a real conflict stops a land and comes back as data. Every land is recorded as a durable land attempt whose bookkeeping is atomic, so a land is either fully recorded or did not happen, also across a restart.

## Goals

- One call, `POST /workspaces/:slug/changes/:number/land`, lands a change or its whole stack and answers `200` (landed), `202` (still running), `409` (precondition, policy block or conflict) or `503` (failed).
- Two lands on the same target that do not conflict both end `landed`, in either order, without either caller retrying (LAND-US-01).
- A conflicting land leaves the target where it was and returns a conflict report naming the files and the landed changes collided with (LAND-US-02).
- The ref move, the change's transition to `landed`, the commit mapping, the attempt's final status and the `parley.change.land` event are one atomic step; after a crash the hub is not landed, or landed with all of them (LAND-FR-015, LAND-US-11).
- An attempt that was `running` when the hub stopped ends in a final status after the next start (LAND-FR-019).
- Land attempts are readable per change, per workspace and by id, and the CLI offers `parley change land`, `parley change lands`, `parley land list` and `parley land show`.

## Non-goals

- Policy documents, their evaluation into requirements and the stored `landing` of a change: package 27 `landing_policy`. This package calls a policy gate interface whose default answers `ready` with revision 0.
- Approvals and checks: package 28 `approvals_and_checks`.
- Who may pass `override` and what it skips (LAND-FR-002), automatic landing (LAND-FR-060 to LAND-FR-062), revert (LAND-FR-070 to LAND-FR-072) and path-scoped tokens at landing (LAND-FR-080 to LAND-FR-082): package 29 `auto_land_revert_and_path_limits`. Package 29 registers the override rule and a path-limit request guard with this package; this package validates the shape of `override`, asks the rule, records what it reports, and offers a start call that takes the trigger `auto_land`.
- `GET …/changes/:number/conflict`, `GET …/conflict/file`, restack with resolutions and `parley change conflict|resolve` (LAND-FR-030 to LAND-FR-032): package 26 `conflict_reports_and_resolution`. This package produces the conflict report of a failed land in the one conflict report type, `ConflictReport`, which package 20 `scm_read_api` declares in `internal/gitrepo/conflictreport.go`; it declares no second type, and package 26 reuses its attribution of landed changes.
- The change object, its versions and the by-commit lookup: package 22 `change_opening_and_versions`. Mergeability computation and its events: package 23 `change_lifecycle_and_mergeability`.
- Restacking dependants after a land (CHG-FR-031): package 24 `change_stacks_and_restack`. The early expiry clock of a fork: package 16 `task_fork_lifetime`. The rebuild after a land on a patch branch and the extra preconditions of a carry-patch target (CP-FR-086 to CP-FR-088): package 37 `carry_patch_push_control`. Each registers an after-landing hook (package 37 also a target guard) with this package, which depends on none of them: package 24 its landing entry point, which clears `depends_on` of the landed change's dependants and queues their restack onto the landed commit; package 16 its activity hook, called for every landed change whose source workspace is a task fork; package 37 its rebuild trigger.
- The push rules for protected branches, the owner exemption and the `parley.git.push` event writer (§8.3.1, GIT-FR-034): package 18 `git_push_rules`. The branch glob matcher (`internal/workspace/branchglob.go`) and the settings keys `protected_branches`, `protect.owner_push` and `land.*`, their types and defaults: package 11 `workspace_read_update_settings`.
- The binding guard that lets a fork-bound token reach the parent's landing routes: package 08 `workspace_bound_tokens`. This package applies the per-change confinement of LAND-FR-003 behind it.
- The metrics `parley_lands_total`, `parley_land_attempts` and `parley_land_duration_seconds`: package 51 `audit_query_and_retention`. This package reports every final attempt to registered observers.
- Deleting land attempts with their workspace: package 14 `workspace_archive_reclone_delete`.
- Publishing a landed branch to origin: package 13 `workspace_push_to_origin`. Landing never pushes (LAND-FR-018).

## Background

The repository holds no code yet; every file named here is new. The source is `docs/prd/08_landing_and_policy.md`: §8.3.1 (protected branches, as far as landing moves them), §8.3.2 (land attempt), LAND-FR-001 and LAND-FR-003 (who may land), LAND-FR-010 to LAND-FR-021 (the algorithm), the four land routes of §8.5, the four commands of §8.6, §8.7 to §8.10 as they concern land attempts. The conflict report shape is §8.3.3.

Code of this package:

| Path | Holds |
|---|---|
| `internal/land/attempt.go` | the land attempt record, its store and its JSON form |
| `internal/land/replay.go`, `squash.go`, `conflict.go` | building the commits to land on a target head, the squash commit, the conflict report |
| `internal/land/engine.go`, `commit.go` | the run of one attempt: gate, replay, compare-and-swap loop, the atomic step and its journal |
| `internal/land/gate.go`, `hooks.go` | the registration points: policy gate with its default, override rule, session interface, request guards, target guards, after-landing hooks, final observers |
| `internal/land/pool.go`, `recover.go` | the worker pool, the attempt deadline, recovery at start |
| `internal/land/routes.go`, `authz.go` | the four routes, who may land, fork-bound confinement |
| `internal/gitrepo/replay.go` | replaying one commit onto another in a bare repository, with conflict detail |
| `internal/config/land.go` | the `[land]` keys |
| `internal/hub/land.go` | the composition: the routes, the pool with its start and stop, recovery as a start hook |
| `internal/cli/land.go` | `parley change land`, `parley change lands`, `parley land list`, `parley land show` |
| `test/e2e/land_test.go` | end-to-end tests against a real hub |

Tests live beside each file. Packages 20 and 21 need a server-side rebase too; if `internal/gitrepo` already holds a replay primitive when this package is implemented, it is reused instead of adding a second one.

## Requirements

### Who may land (LAND-FR-001, LAND-FR-003, PLAT-FR-062)

`POST /workspaces/:slug/changes/:number/land` names the target workspace. A PAT needs `changes:write`. The route's need is `read` plus one condition: the caller has write access to the target workspace (owner, or the admin token), or the caller's user is the change's author (the user behind the credential that opened the change) while the workspace setting `land.by_author` is true (default). An org member who is not the author, or who is the author while `land.by_author` is false, gets `403 forbidden`. A caller with no read access gets `404 not_found`. An unknown change number is `404 change_not_found`. An archived target workspace is `409 workspace_archived`.

A token bound to a fork F reaches, on F's parent, the land route and `GET …/changes/:number/lands` only for a change whose source workspace is F, and `GET …/lands/:id` only for an attempt that lands such a change; with `stack: true` every change the attempt would cover must have source F. Otherwise the answer is `403 token_bound`, as it is for `GET …/lands` on the parent.

The body is `{"expected_version"?: int, "stack"?: bool, "override"?: {"reason": string}, "session_id"?: string}`; the query `wait` is whole seconds, 0 to 60, default 30. A wrong type, an `expected_version` below 1, an `override` without a non-empty `reason`, or a `wait` outside 0 to 60 is `400 invalid_request`. A `session_id` that fails the session-linking rule of §14 is `400 unknown_session`. None of these creates a land attempt. The route is a create route for `Idempotency-Key` (PLAT-FR-015).

### Preconditions (LAND-FR-010)

Checked in this order, the first failure deciding; none creates an attempt:

1. the change is `open`, else `409 change_not_open`;
2. `expected_version`, when given, equals the current version, else `409 version_mismatch`;
3. no other land attempt covering this change is `running`, else `409 land_in_progress` with that attempt in `details`;
4. every change below it in the stack is landed, unless `stack` is true, else `409 depends_on_unlanded`.

Of two simultaneous land calls for one change, exactly one creates an attempt; the other gets `409 land_in_progress`. After the first finished `landed`, a further call gets `409 change_not_open`.

### The land attempt (§8.3.2)

A land that passes the preconditions creates a durable attempt before anything else happens: `id` (UUID), `workspace_slug`, `target_branch`, `changes` (numbers, bottom first), `status` (`running`, then one of the final `landed`, `conflict`, `blocked`, `failed`), `trigger` (`manual`; `auto_land` when package 29 starts it), `strategy` (`rebase` or `squash`, read once from `land.strategy`), `attempts`, `onto_sha`, `landed_sha`, `noop`, `commits` (`{change, version_sha, landed_sha}`), `conflict`, `blocked` (`{change, status, requirements[]}`), `error` (`contention`, `timeout`, `origin_rejected`, `internal`), `override`, `policy_revision`, `requested_by` (`{actor_type, actor_id, username}`), `session_id`, `created_at`, `finished_at`. A final attempt never changes. The attempt lands the version each change had when the attempt was created; a version that arrives later is not landed by it.

### Policy gate (LAND-FR-011)

For each change of the attempt, bottom first, the gate is asked once with the workspace, the change, its version, the caller and the `override` of the request. It answers a status (`ready`, `pending`, `blocked`), an optional reason, the requirements, the policy revision and whether an override was applied. The gate is a registration point: package 27 registers its evaluator; with none registered the default answers `ready`, revision 0. The override rule is a second registration point, which package 29 registers: for a request that carries `override` and a gate answer that is not `ready`, it says whether the override lets that answer pass; with no rule registered no override is ever applied. A passed answer counts as `ready`. When any answer is not `ready` the attempt ends `blocked` with `blocked` naming the first such change; the target is untouched, the change stays `open`, and the HTTP answer is `409 policy_blocked` with the attempt in `details`. The attempt records the revision the gate reported and `override: {reason}` only when the override rule let at least one change pass. The gate is not asked again when the compare-and-swap is retried, so a policy change or a withdrawn approval during a land does not affect it (§8.10).

### Replay (LAND-FR-012, LAND-FR-013)

Read the target head *T*. The change's own commits are the commits of its version that are not in its base.

- `rebase`: each commit is replayed in order; author, author date and message are kept; the committer is `[land].committer_name` and `[land].committer_email`. When the version already sits directly on *T* the commits are used as they are.
- `squash`: one commit per change, with the tree of the replayed head. Message: the change title, a blank line, the intent, a blank line, the trailers `Parley-Change: <slug>#<number>`, `Parley-Session: <id>` when the change has a session, and one `Co-authored-by: Name <email>` per other distinct author of the change's commits. Author: the author of the version's head commit.
- A commit whose content is already in *T* is dropped. If nothing is left the attempt ends `landed` with `noop: true`, `landed_sha` = *T*, `commits` empty; the target does not move and the change becomes `landed` with `landed.sha` = *T*.
- If a commit does not apply the attempt ends `conflict` with a conflict report: `target_sha`, `head_sha`, `base_sha`, `commit_sha`, `files` (`path`, `kind`, `base`, `ours`, `theirs`, `landed_by`), `landed_changes` (`{number, title, landed_sha, session_id}` of the changes landed between `base_sha` and `target_sha` that touched a conflicted file) and `change`. The target is untouched, the change stays `open` and reads `mergeable: conflict` with that report, and the HTTP answer is `409 land_conflict` with the attempt in `details`.

### Compare-and-swap and the atomic step (LAND-FR-014, LAND-FR-015, LAND-FR-021)

The target moves from *T* to the new head only if it still is *T*. If it moved (another land, a sync, a direct push), the replay starts again on the new head and `attempts` grows by one. After `[land].max_attempts` tries (default 10) the attempt ends `failed` with `error: "contention"`. A target branch that no longer exists ends the attempt `failed` with `error: "internal"`.

The ref move, each change's transition to `landed` with its `landed` object, the mapping of each landed commit to its change, the attempt's final status and one `parley.change.land` event per landed change are one atomic step. The move also produces `parley.git.push` for the target ref with `via: "land"` and `direct: false`. Landing moves protected and unprotected targets alike; the push rules do not apply to it.

Attempts for different changes run concurrently, also on one target; nothing but the compare-and-swap excludes them. They run on a pool of `[land].workers` workers (default 8) that bounds how many replays are computed at once and never orders them; an attempt waiting for a worker is `running`.

### Stack landing (LAND-FR-016)

With `stack: true` the attempt covers the change and every unlanded change below it; a change below that is already landed is skipped. Preconditions 1 and 3 hold for every covered change. All are replayed bottom first onto *T* (with `squash`, one commit per change, in order) and the target moves once, to the top. One conflict or one gate miss ends the whole attempt with nothing landed, and `conflict.change` or `blocked.change` names the change.

### After landing (LAND-FR-017, LAND-FR-018)

After the atomic step, each best effort and never changing the attempt or the answer: delete the source branch when `land.delete_source` is true, its head still is the landed version's head (compare-and-swap) and no other `draft` or `open` change uses it; schedule mergeability checks for the other `draft` and `open` changes on the target; call the registered after-landing hooks (restack of dependants, fork early expiry, carry-patch rebuild). Nothing is pushed to origin or a forge. A registered target guard may, before the target moves, end the attempt `blocked` with its reason or `failed` with `error: "origin_rejected"`; a guard that cannot decide returns an error, and the attempt ends `failed` with `error: "internal"`. In none of the three cases does the target move. A registered request guard is asked in the land route before the preconditions and may refuse the request with its own status and `error_type`; no attempt is created then (package 29 registers its path-limit check and its override refusal this way). Each after-landing hook receives the landed attempt with the target workspace, the update of the target branch (`ref`, `before`, `after`), the attempt's session id (or none) and, for every landed change, its number, its source workspace and branch, and its landed commit.

### Durability, time and the answer (LAND-FR-019, LAND-FR-020)

An attempt that was `running` when the hub stopped is run again from the preconditions at the next start. One attempt is bounded by `[land].timeout` (default 5 min) and then ends `failed` with `error: "timeout"`. The land call waits up to `wait` seconds: `200` with the attempt when `landed`; `409 land_conflict` or `409 policy_blocked`; `503 land_failed` with the attempt in `details`; `202` with the attempt while it is still `running`. Every attempt ending `conflict`, `blocked` or `failed` emits `parley.change.land_fail` (`land_id`, `status`, `reason`, `files`).

`[land]` keys: `max_attempts` (int, 10), `timeout` (duration, `5m`), `workers` (int, 8), `committer_name` (`Parley`), `committer_email` (`parley@localhost`).

### Reading land attempts (§8.5)

`GET …/changes/:number/lands` lists a change's attempts; `GET …/lands` lists the workspace's, filtered by `status` (repeatable), `target_branch` and `since`, paged with `limit` and `cursor`; both newest first. `GET …/lands/:id` reads one, or `404 land_not_found`. All need `changes:read` and read access. Attempts are kept for the life of the workspace.

### CLI (§8.6)

`parley change land <slug> <number>` with `--stack`, `--expected-version <n>`, `--override <reason>`, `--session <id>`, `--wait`, `--timeout`, `--poll-interval` calls the land route and, with `--wait`, polls `GET …/lands/:id`. It prints the attempt; a conflict or block prints the envelope. Exit 0 when `landed`, 1 for any other final status or a timeout, 2 for usage. `parley change lands <slug> <number>`, `parley land list <slug>` (`--status` repeatable, `--target`, `--since`, `--limit`, `--cursor`) and `parley land show <slug> <land-id>` print the routes' answers.

### End-to-end flows

1. An agent runs `parley change land` on an open change; `parleyd` creates the attempt, replays, moves the target and records everything in one step; a git client fetching the target sees the commits.
2. Two agents land non-conflicting changes on one target at the same moment; both attempts end `landed` and the target holds both.
3. An agent lands a change that conflicts with a change landed meanwhile; the answer is `409 land_conflict` with the report, and the target has not moved.
4. An operator stops `parleyd` while an attempt is `running` and starts it again; the attempt ends `landed`.

## Design Decisions

1. **Language Go, git as a subprocess (2.38 or later), SQLite through `modernc.org/sqlite`, `net/http`, `cobra`**: the project-wide decisions; replay uses `git merge-tree --write-tree` and `git commit-tree`, which need no working tree.
2. **The atomic step is a journal entry, the ref compare-and-swap, then one store transaction.** The journal entry (attempt, ref, *T*, new head, commits) is committed first. At start, before requests are served, an entry whose ref holds the new head gets its bookkeeping completed; any other entry is dropped and the attempt runs again. A git ref and a store row cannot share a transaction, and this gives the two states LAND-FR-015 demands.
3. **The policy gate interface lives in `internal/land`**, so this package does not import `internal/policy`; package 27 `landing_policy`, which depends on this package, registers its evaluator as the gate. With no gate registered every change is `ready` (decision 3 of §17.3).
4. **`override` is validated for shape only; the rule is package 29's (LAND-FR-002).** `internal/land` declares an override rule (does this override let this gate answer pass) and request guards (asked before the preconditions; a refusal is returned as given). Package 29 registers the rule, a request guard that refuses an override the caller may not use, and a request guard for path limits. With nothing registered an override is accepted, never applied, and the attempt records `override: null`.
5. **Preconditions run in the request, before an attempt exists.** So `change_not_open`, `version_mismatch` and `depends_on_unlanded` carry no `details`; `land_in_progress` carries the running attempt. The PRD says "with the land attempt in `details` where one was created".
6. **A re-run after a restart whose change is no longer landable** (not `open`, target missing) ends `failed` with `error: "internal"`: the error enumeration has no closer value.
7. **`[land].timeout` counts from the attempt's creation**, and from the re-queue after a restart; waiting for a worker is inside it.
8. **`reason` of `parley.change.land_fail`**: `conflict` for a conflict; the gate's or guard's reason for a block, `policy` when it gives none; the `error` value for a failure. The PRD lists the member without values.
9. **`parley change land` without `--wait` exits 0 on a `202`** and prints the running attempt: the hub answered success and no final status exists yet.
10. **The CLI's land request times out after 70 seconds** instead of 30, because the hub may hold the call for up to 60.
11. **A fork-bound token asking for a change or attempt that is not its fork's gets `403 token_bound`**; an unknown number or id still gets its `404`.
12. **Invalid `[land]` values stop the start**: `max_attempts` and `workers` below 1, a `timeout` that is not positive, an empty committer name or email.
13. **Hooks, guards and observers are registration points of `internal/land`**, so packages 16, 24, 29, 37 and 51 attach without this package depending on them. A final observer receives the final attempt (trigger, changes, status, `attempts`, `created_at`, `finished_at`); package 51 derives its metrics from it and package 29 the outcome of an `auto_land` attempt.
14. **The session-linking rule is asked through a session interface of `internal/land`** with two operations, check and record, which package 49 `agent_sessions_and_usage` installs from the linking check and the recording operation that `internal/session` exports. An attempt with a session records one `land` link in the transaction that creates it. Until an implementation is installed, every `session_id` is refused with `unknown_session`: accepting an unchecked reference would attribute work to a session nobody verified.
15. **One conflict report type.** A failed replay is reported in `ConflictReport`, the type package 20 declares in `internal/gitrepo/conflictreport.go` and packages 21, 23 and 24 already use; the kind and the three sides of each file come from the replay of `internal/gitrepo`. This package adds, in `internal/land/conflict.go`, the attribution that fills `landed_changes` and each `landed_by` from the landed-commit mapping, and sets `change` in a stack land. Package 26 reuses that attribution for the other producers.
16. **The target moves through the ref writer of package 18** (`pushrule.Apply` in `internal/gitserver/pushrule`, `via: "land"`) with an allowed verdict for the one update that this package builds itself, without calling `Evaluate`, because the push rules do not apply to a land.
17. **A target guard answers one of three things or returns an error (with package 37).** The answers are allow, refuse with a reason (`blocked`) and origin rejected with a message (`failed`, `origin_rejected`); an error, which package 37's guard returns when it cannot decide, ends the attempt `failed` with `error: "internal"`. A guard error never allows. The after-landing hook carries the target workspace, the branch update (`ref`, `before`, `after`) and the session id because package 37's rebuild trigger and mirror need them.
18. **Branch globs are package 11's.** The one matcher is `internal/workspace/branchglob.go` of package 11; package 18 calls it and has none of its own, and this package matches no glob.
19. **Composition is in `internal/hub/land.go`**, as package 03 lays down: the routes, the pool and recovery are registered there and nowhere else. This package registers no after-landing hook itself. Packages built earlier than this one (16, 24) add their registration line to `internal/hub/land.go` when this package exists, and packages built later (29, 37) register from their own `internal/hub/<area>.go`.

## Dependencies

- Package 22 `change_opening_and_versions` (draft): the change record, its versions, `depends_on`, the author, the by-commit mapping this package writes landed commits into.
- Package 23 `change_lifecycle_and_mergeability` (draft): the change state machine (`open` to `landed`), recording `mergeable: conflict` with a report, scheduling mergeability checks.
- Package 11 `workspace_read_update_settings` (draft): the settings `land.strategy`, `land.by_author` and `land.delete_source` this package reads.
- Through them: package 18 `git_push_rules` (the ref writer and `parley.git.push`), package 20 `scm_read_api` (`ConflictReport` and the replay in `internal/gitrepo`), package 02 `http_api_conventions`, package 07 `workspace_access_rule`, package 09 `cli_foundation`, package 52 `durable_event_log_and_stream`, package 03 `server_lifecycle_and_configuration`.
- External: the `git` command, 2.38 or later.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task.
- **Protected branch**: A branch that moves only by landing a change, by sync from origin, or by a direct fast-forward push from the workspace's owner or the admin token. Nobody can force-push or delete it. By default the default branch.
- **Change**: The unit of work: a proposal to add the commits of a source branch to a target branch, with a number, a title, an intent, versions and a state.
- **Version**: One head of a change's source branch.
- **Stack**: Changes linked by `depends_on`, lowest first.
- **Landing**: Adding a change's commits to its target branch: a server-side rebase followed by a compare-and-swap of the target ref.
- **Land attempt**: One request to land a change (or a stack), with its outcome.
- **Conflict report**: The structured description of why a rebase could not be done: files, kinds, and the landed changes involved.
- **Policy**: A workspace's ordered rules that map touched paths to landing requirements.
- **Bound token**: A PAT confined to one workspace (and the forks created with it). A token bound to a fork also reaches its own changes, and the awareness and policy reads, on the fork's parent.
- **Session**: An agent session: who ran, on which workspace, with what token usage, and which changes it produced.
