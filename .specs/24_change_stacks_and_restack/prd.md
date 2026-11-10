---
spec_id: "24"
spec_name: "change_stacks_and_restack"
title: "Stack changes and restack them automatically"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "e60c083d70415cbc88ed42d8a4bfcd83d50cf6004c0e1f681a7bf7cab8b2d8fc"
schema_version: 3
source: "docs/prd/07_changes_and_stacks.md"
---

# Stack changes and restack them automatically

## Intent

An agent that builds a second change on top of an unlanded first one should not have to rebase by hand every time the first one moves. This package lets changes depend on one another as a stack, and makes the hub keep every change of a stack built on the change below it: when a lower change gets a new version or lands, the hub rebases the changes above it, records the result as a new version, and, when the rebase cannot be done, says so on the change with a conflict report and moves nothing.

## Goals

- A change's `depends_on` is set by the caller (at open or by `PATCH`) or by the hub from the commit graph at open and on every new version, and `dependants` lists the `draft` and `open` changes directly above.
- An explicit `depends_on` that names an unknown change, a change with another target branch, a change that is not `draft` or `open`, or a change that would close a cycle is refused with `400 invalid_dependency` and changes nothing.
- A stack deeper than `[changes].max_stack_depth` (default 20) is still recorded; the change past the limit carries `needs_restack: true` with `restack_reason: "stack_too_deep"`.
- A new version of a change with dependants causes, in the background and without client action, a `restack` version of every change above it, bottom first.
- Landing a change rebases each direct dependant onto the landed commit, clears its `depends_on`, and restacks the changes further up in turn.
- A restack that conflicts moves no branch for that change or any change above it, sets `needs_restack: true`, `restack_reason: "conflict"`, `mergeable: "conflict"` and a conflict report, and emits `parley.change.conflict` once.
- With the workspace setting `changes.auto_restack` false, the hub only sets `needs_restack` with `restack_reason: "auto_restack_off"`.
- `POST /api/v1/workspaces/:slug/changes/:number/restack` does the same rebase on demand and answers `200` with `restacked` or `up_to_date`, or `409 rebase_conflict` with the conflict report.
- `GET /api/v1/workspaces/:slug/changes/:number/stack` returns the whole stack, bottom first.
- A restack that was pending when the hub stopped runs after the next start.
- `parley change restack` and `parley change stack` drive the two routes.

## Non-goals

- The change object, opening a change, recording versions, version refs, reading and listing changes (including the `stack_of` list filter and `include=stack`), and the rule that confines a fork-bound token to changes of its fork (CHG-FR-001 to CHG-FR-014, CHG-FR-040 to CHG-FR-043): package 22 `change_opening_and_versions`. This package implements the `StackLinker` interface that package 22 declares (`LinkVersion` at open and on every version, `SetDependsOn` for `PATCH`), and uses the stack fields, the setters and the `dependants` derivation that package 22 owns.
- The `PATCH` route and its other fields, retargeting, the abandon and restore routes, the `has_dependants` refusal and the `dependants` parameter, the mergeability computation and its debounce, start-up reconciliation of missed versions (CHG-FR-020 to CHG-FR-024, §7.3.4): package 23 `change_lifecycle_and_mergeability`. This package gives `depends_on` in the `PATCH` body its meaning through `SetDependsOn`, which the `PATCH` of package 23 calls, and implements the interface `Restacker` that package 23 declares, which its abandon with `dependants=reparent` calls after it has relinked the dependants itself.
- The landing algorithm and its after-landing step: package 25 `optimistic_landing`. This package provides the entry point for the dependants of a landed change; `internal/hub/change.go` registers it as an after-landing hook of `internal/land`, the hook list package 25 declares, so this package does not depend on package 25.
- Restack with `resolutions`, `GET …/conflict`, `GET …/conflict/file`, the `landed_changes` and `landed_by` members of a conflict report, `parley change conflict|resolve` (LAND-FR-030 to LAND-FR-032): package 26 `conflict_reports_and_resolution`. This package ignores a `resolutions` member and returns the report with `landed_changes` and every `landed_by` empty, in the one report type `ConflictReport` of `internal/gitrepo` that package 20 declares.
- Refusing a restack whose resulting version touches a path outside a path-scoped token's `paths` (CHG-FR-012, LAND-FR-080): package 29 `auto_land_revert_and_path_limits`. This package declares the version guard, calls it in the manual restack before the source branch moves and answers its refusal; package 29 registers the guard and owns the check.
- Policy evaluation of the new version and approval carry-over on patch-equivalent versions: packages 27 `landing_policy` and 28 `approvals_and_checks`.
- The server-side rebase itself, compare-and-swap of refs and the conflict data a failed rebase returns: `internal/gitrepo`, delivered with packages 18 `git_push_rules`, 20 `scm_read_api` and 21 `scm_write_api`.
- The workspace setting key `changes.auto_restack`, its type, default and validation: package 11 `workspace_read_update_settings`. This package reads it.
- The session-linking rule behind `unknown_session`: package 49 `agent_sessions_and_usage`, reached through the `SessionLinker` of package 22.
- Computing `mergeable`, storing it with its conflict report and emitting `parley.change.conflict`: package 23. This package stores the conflict of a failed restack through that package's storing function.
- Storing and streaming events: package 52 `durable_event_log_and_stream`. The error envelope, check order and CLI output and exit codes (PLAT-FR-010 to PLAT-FR-019, PLAT-FR-090 to PLAT-FR-095): packages 02 `http_api_conventions` and 09 `cli_foundation`.
- The acceptance scenario for stacks (§16, item 2): package 55 `acceptance_suite`.

## Background

The repository holds no code yet; every file named here is new. The change record, its store, version recording and the read routes arrive with package 22 in `internal/change/`; the `PATCH`, abandon and restore routes and the mergeability check with package 23 in the same directory; the rebase, ref compare-and-swap and ancestry queries with `internal/gitrepo/`; the event component with package 52 in `internal/event/`; the router and error envelope with package 02 in `internal/server/`; the CLI root, HTTP client and output rules with package 09 in `internal/cli/`.

This package adds to `internal/change/`: the stack link and depth rule (`stack.go`), the restack operation and its durable pending records (`restack.go`, `restack_queue.go`), the manual restack handler (`restack_handler.go`) and the stack route (`stack_handler.go`), each with its test file beside it; to `internal/config/` the key `[changes].max_stack_depth`; and to `internal/cli/change.go` the commands `restack` and `stack`. The tasks do not edit the files of packages 22 and 23: they implement and register the interfaces those packages declare, `StackLinker` (`internal/change/collaborators.go`, package 22) and `Restacker` (`internal/change/lifecycle.go`, package 23), and call their setters, their version recording and the function that stores `mergeable` with its conflict report. The wiring (the registrations, the worker's start, the after-landing hook) is in `internal/hub/change.go`. An end-to-end test in `test/e2e/stack_restack_test.go` starts a real hub in-process and drives it with stock git, over HTTP and through the CLI.

Source: `docs/prd/07_changes_and_stacks.md`, §7.3.3, CHG-FR-030 to CHG-FR-035, the fields `depends_on`, `dependants`, `needs_restack` and `restack_reason` of §7.3.1, the routes `POST …/restack` and `GET …/stack` of §7.5, the two commands of §7.6, the restack line of §7.7, the events `parley.change.restack` and `parley.change.conflict` of §7.8, `[changes].max_stack_depth` of §7.9 and the stack entries of §7.10; `docs/prd/08_landing_and_policy.md` §8.3.3 for the conflict report.

## Requirements

All routes are under `/api/v1/workspaces/:slug`, where `:slug` is the workspace of the change's target branch.

### The stack link (§7.3.3)

A change depends on another when its commits sit on top of that change's commits. `depends_on` holds the number of the change directly below, or `null`. `dependants` holds the numbers of the `draft` and `open` changes whose `depends_on` is this change, ascending.

The automatic rule runs when a change is opened without an explicit `depends_on` and on every new version of a change, whatever created the version. Among the other `draft` or `open` changes of the same workspace with the same target branch, the hub looks for those whose current `head_sha` is a proper ancestor of the new head and takes the nearest one (the fewest commits between it and the new head). That change becomes `depends_on`, and the version's `base_sha` is its head. When there is none, `depends_on` is cleared and `base_sha` is the merge base with the target branch. A change whose head equals the new head is not below it. Changes in state `landed` or `abandoned`, and changes with another target branch, are never candidates. Cycles cannot arise from this rule. A dependant that is force-pushed directly onto the target therefore loses its link (§7.10).

A dependent change's `base_sha` is the head of the change below; its diff, stats and paths cover only its own commits. All changes of a stack have the same target branch; they may come from different source workspaces (the target workspace and its forks).

### Explicit `depends_on` (§7.3.3, §7.5 `PATCH`)

`depends_on` in the body of `POST …/changes` and of `PATCH …/changes/:number` sets the link by hand. The value is refused with `400`, `error_type: invalid_dependency`, and nothing is changed, when it names: a change number that does not exist in the workspace; the change itself or a change whose own stack already contains this change (a cycle); a change with a different target branch; a change that is not `draft` or `open`. `PATCH` with `depends_on: null` clears the link and sets `base_sha` to the merge base with the target.

After an accepted explicit link, when the lower change's head is an ancestor of the change's head the change's `base_sha`, `stats` and `paths` are recomputed against that head. When it is not, the change is not yet built on the change below: with `changes.auto_restack` true a restack of it is recorded as pending and runs in the background; with it false the change gets `needs_restack: true`, `restack_reason: "auto_restack_off"`.

### Stack depth (§7.3.3, §7.9)

The depth of a change is the number of changes in the chain of `depends_on` links from it down to a change with none, itself included. `[changes].max_stack_depth` (integer, default 20) is the deepest allowed stack; a value below 1 stops the server at start with exit status 1 and one error line on stderr. A version or an explicit link that puts a change at a depth above the limit is still recorded with its link, and the change gets `needs_restack: true`, `restack_reason: "stack_too_deep"`. A change at exactly the limit is not flagged. The hub does not restack a too-deep change, or anything above it, on its own. The flag is recomputed whenever the change's link or a link below it changes, and is cleared once the depth is at or under the limit.

### Restack on a new version below (CHG-FR-030, CHG-FR-033)

When a change that has dependants gets a new version, the hub records one pending restack per direct dependant in the same transaction as the version. The push or commit that caused the version is answered without waiting. A background worker then, for each dependant: reads the dependant's source branch head; replays the dependant's own commits (those after its `base_sha` up to its head) onto the new head of the change below; moves the dependant's source branch to the result with a compare-and-swap; and records a version of kind `restack` whose `created_by.actor_type` is `system`, whose `base_sha` is the new head below and whose `session_id` is that of the version it replaces. A restack that changes no patch is `patch_equivalent_to` the previous version. The hub emits `parley.change.version` with `kind: "restack"` and then `parley.change.restack` with `onto_sha` (the new head below) and `trigger: "version"`, both with actor `system`.

Because the restack is itself a new version of the dependant, it causes the restack of that change's dependants in the same way, so the work continues up the stack, bottom first.

A dependant whose head already has the new head below as an ancestor is left alone: no version, no event; a `needs_restack` flag with reason `conflict` or `auto_restack_off` is cleared.

The restack moves the source branch on the hub; the author's local branch is then behind, and a `git pull --rebase` or a fetch of `refs/parley/changes/<number>/<version>` brings it up to date. If the source branch moved between the hub reading it and the compare-and-swap, the restack is tried again on the new head, at most 5 times in one run. After the fifth lost compare-and-swap a background restack stays pending and nothing is changed.

If the lower change was force-pushed to an unrelated head, its dependants are restacked onto it by the same replay of their own commits; this may conflict (§7.10).

### Restack on landing below (CHG-FR-031)

When a change lands, the after-landing step (package 25, LAND-FR-017) calls this package with the landed change and the landed commit on the target. For each direct dependant the hub clears `depends_on` and records a pending restack onto the landed commit. The worker replays the dependant's own commits onto the landed commit, moves the source branch with a compare-and-swap and records a `restack` version whose `base_sha` is the landed commit; the events are as above with `trigger: "land"`. Changes further up keep their `depends_on` and are restacked in turn by the rule of the previous section.

When a change with dependants is abandoned with `dependants=reparent` (package 23, CHG-FR-022), package 23 gives each direct dependant the abandoned change's `depends_on` as its own and asks this package, through `Restacker` and inside the abandon's transaction, to restack it; it is restacked onto the head of that change, or onto the current head of the target branch when there is none, without the abandoned change's commits; the events carry `trigger: "abandon"`.

### Restack failure (CHG-FR-032, §8.3.3)

If the replay conflicts, no branch moves for that change, and no restack is started for any change above it. The change gets `needs_restack: true`, `restack_reason: "conflict"`, `mergeable: "conflict"` and `conflict` set to the conflict report. `parley.change.conflict` is emitted with actor `system` and metadata `files` (the conflicted paths) and `needs_restack: true`. The event is not emitted again while the change stays in conflict. `mergeable`, the report and the event are written by the storing function of package 23, called with the pair of the change's head and the commit replayed onto, so that the background check of that package and a restack never announce one conflict twice.

After a successful restack `mergeable` needs no write here: the `restack` version sets it to `unknown` and package 23 computes it for the new head against the head the change now sits on. When a flag is cleared without a new version (an `up_to_date` manual restack), this package asks package 23's trigger to compute it again.

The conflict report has the members `target_sha` (the commit the work was being replayed onto), `head_sha` (the dependant's head), `base_sha` (the commit the work was based on), `commit_sha` (the first commit that did not apply), `files`, `landed_changes` and `change`. Each file entry has `path`, `kind` (`content`, `add_add`, `modify_delete`, `delete_modify`, `rename`, `mode`, `binary` or `submodule`), `base`, `ours`, `theirs` (each `{sha, mode, path}` or `null` where the file does not exist on that side; `ours` is the commit replayed onto, `theirs` the change) and `landed_by`. In this package `landed_changes` is `[]`, every `landed_by` is `[]` and `change` is `null`.

A later version of the change whose head has the commit it must sit on (the current head of the change below, or the landed commit) as an ancestor clears `needs_restack` and `restack_reason`. A later version that leaves the change with no `depends_on` by the automatic rule, while the change was linked, clears them too, because there is no longer a change below.

### `changes.auto_restack` (CHG-FR-035)

`changes.auto_restack` is a workspace setting of the target workspace, default true. While it is false, a new version below and a landing below record no pending restack and move no branch: each direct dependant that is not built on the new head gets `needs_restack: true`, `restack_reason: "auto_restack_off"`. On landing, `depends_on` is still cleared. The rebase is left to the manual call.

### Manual restack (CHG-FR-034, §7.5)

`POST …/changes/:number/restack` with an optional body `{"session_id"?: string, "resolutions"?: [...]}` does the same operation on demand and answers synchronously. It needs `changes:write`, and the caller must be the change's author, have write access to the target workspace, or use the admin token (CHG-FR-020); a caller who can read the change but is none of these gets `403 forbidden`; a PAT without the scope gets `403 insufficient_scope`; an unknown number gets `404`, `error_type: change_not_found`.

The commit to sit on is the current head of the change named by `depends_on`, or the current head of the target branch when `depends_on` is `null`.

- The change's head already has that commit as an ancestor: `200 {"status": "up_to_date", "change": <change object>}`. No version and no event; a `needs_restack` flag with reason `conflict` or `auto_restack_off` is cleared.
- Otherwise the change's own commits are replayed and the source branch is moved: `200 {"status": "restacked", "change": <change object>}`. A version of kind `restack` is recorded whose `created_by` is the caller and whose `session_id` is the body's `session_id`, or that of the previous version when the body has none. `parley.change.version` and then `parley.change.restack` (`onto_sha`, `trigger: "manual"`) are emitted with the caller as actor. `needs_restack` is cleared unless its reason is `stack_too_deep`.
- A version guard is registered and refuses the version the replay would record (package 29: a path-scoped token and a file outside its `paths`): the guard's answer, `403`, `error_type: path_not_allowed` with the path. The guard is asked after the replay is computed and before the source branch moves, with the caller's credential and the base and head of the version; nothing is changed. A restack the hub makes on its own does not ask it.
- The replay conflicts: `409`, `error_type: rebase_conflict`, with the conflict report in the envelope's `details`. The change is left as in "Restack failure".
- The change is `landed` or `abandoned`: `409`, `error_type: change_closed`.
- The change has no `depends_on` and its target branch no longer exists: `400`, `error_type: target_not_found`.
- `session_id` fails the session-linking rule of §14: `400`, `error_type: unknown_session`; nothing is changed.
- The source branch moved under the hub five times in a row: `409`, `error_type: ref_moved`, with `current_sha` in `details`; nothing is changed.

A `resolutions` member is ignored here; package 26 gives it its meaning.

### Durability (§7.7, NFR-013)

A pending restack is a record in the transactional store, written in the transaction of the event that caused it. There is at most one per change; recording another for the same change replaces it. It is removed when the restack ends (restacked, up to date, conflict, or the change is no longer `draft` or `open`). At start the hub runs every pending restack it finds, so that one that was pending at shutdown is run after the next start. Restacks are not retried with backoff.

When the `git` command fails for a reason other than a conflict (it cannot be started, it exits with an error, the repository cannot be read), a background restack changes nothing, keeps its pending record and is logged; the record is run again at the next start or when another restack is recorded for the same change. The manual call answers `500`, `error_type: internal`, and changes nothing. The worker never stops the server process.

### The stack route (§7.5)

`GET …/changes/:number/stack` needs `changes:read` and read access to the target workspace. It answers `200 {"target_branch": string, "changes": [<summary>…]}`. The list holds the chain of `depends_on` links below the change, the change itself, and every `draft` or `open` change above it through `dependants`, ordered by depth from the bottom and then by number. Each summary has `number`, `title`, `state`, `version`, `head_sha`, `mergeable`, `needs_restack`, `restack_reason` and `landing` as `{"status": …}`. A change with no link and no dependants answers with a list of one. An unknown number answers `404 change_not_found`.

### CLI (§7.6)

- `parley change restack <slug> <number>` calls `POST …/changes/:number/restack` with an empty JSON object, prints the response on stdout and exits 0. A conflict prints the envelope with the conflict report on stderr and exits 1.
- `parley change stack <slug> <number>` calls `GET …/changes/:number/stack`, prints the response and exits 0.

### End-to-end flows

1. An agent pushes three branches with stock git, each one commit on top of the previous, and opens a change for each. The hub links them: 2 depends on 1, 3 depends on 2. The agent amends the bottom branch and pushes it. The push is answered; shortly after, changes 2 and 3 each have a new version of kind `restack` created by `system`, their source branches sit on the new heads below, and the stack route lists the three bottom first with `needs_restack: false`.
2. An agent has two stacked changes that edit the same line. It pushes a new version of the lower one that rewrites that line. The hub's restack of the upper change conflicts: the upper branch does not move, the change shows `needs_restack: true`, `restack_reason: "conflict"` and a conflict report. `parley change restack` prints the `rebase_conflict` envelope and exits 1. The agent rebases the upper branch onto the new lower head locally and pushes it; the flag is cleared and `parley change stack` shows both changes with `needs_restack: false`.

## Design Decisions

1. **Language, store and git.** Go; the store is embedded SQLite behind `internal/store`; git operations run the `git` binary through `internal/gitrepo`; no git library. These are the project-wide decisions.
2. **Tests.** Standard `testing` package, table-driven; handlers with `net/http/httptest`; repositories created with `git init` in `t.TempDir()`.
3. **A change's own commits are `base_sha..head_sha`.** Every restack replays exactly the commits after the change's recorded `base_sha`, whatever happened to the change below. This is what makes "patch replay" onto an unrelated head (§7.10) and the rebase without an abandoned change's commits the same operation.
4. **Automatic rule: proper ancestor, nearest first, lowest number on a tie.** The PRD says "nearest ancestor"; two changes may share a head, so the tie goes to the lower number, and a change with the same head as the new version is never its parent, which keeps cycles impossible.
5. **An explicit `depends_on` at open wins over the automatic rule for that version; every later version runs the automatic rule.** The PRD says the link is set "explicitly, at open or by `PATCH`; or automatically at open and on every new version" and does not rank them.
6. **`invalid_dependency` applies at open too.** The PRD lists it only under `PATCH`; the same four causes are refused the same way in `POST …/changes`.
7. **An explicit link to a change the caller's commits do not sit on schedules a restack** (trigger `manual`, version created by `system`), or sets `auto_restack_off` when the setting is false. The PRD does not say what happens; this keeps the definition of `needs_restack` true without a new reason value.
8. **A too-deep change keeps its link.** The PRD says the version "is still recorded" and the flag is set; the link is recorded too so that the stack route shows the real chain. Depth is counted downward from each change, so only the changes past the limit are flagged. An explicit link that exceeds the limit is flagged, not refused, because `invalid_dependency` does not list depth. A manual restack of a too-deep change rebases it and leaves the flag.
9. **`[changes].max_stack_depth` below 1 fails the start.** The PRD gives no bounds; a stack needs at least one change.
10. **`depends_on` is cleared whenever the change below lands**, including when `auto_restack` is false or the restack conflicts. A link is only ever to a `draft` or `open` change; the change remembers the landed commit as the commit it must sit on until it does.
11. **Clearing the flag.** `needs_restack` with reason `conflict` or `auto_restack_off` is cleared by a version that has the required commit as an ancestor (CHG-FR-032), and also when the automatic rule removes the link of a linked change, since the definition of the flag needs a change below.
12. **Compare-and-swap retries are capped at 5 per run.** The PRD says the restack "is retried on the new head" without a bound. A background restack that loses five times stays pending; the manual call answers `409 ref_moved` with `current_sha`, the type §6 defines for a lost compare-and-swap.
13. **Who made a restack version.** Hub restacks are `system` (§7.3.2). A manual restack's version is created by the caller, as §14.8 gives the caller as the actor of a manual restack's event. A restack version keeps the `session_id` of the version it replaces unless the manual call's body names one.
14. **A manual restack that conflicts leaves the same state as an automatic one** (flag, report, one `parley.change.conflict`), because CHG-FR-034 calls it "the same operation".
15. **Manual restack with a missing target branch is `400 target_not_found`**, the type and status CHG-FR-003 uses for the same fact.
16. **Conflict report content.** The report is the type `ConflictReport` of `internal/gitrepo`, declared by package 20 and returned by the compare route, the rebase of package 21 and the mergeability check of package 23 alike. This package fills `target_sha`, `head_sha`, `base_sha`, `commit_sha` and the file entries from the rebase result of `internal/gitrepo`, and leaves `landed_changes` and `landed_by` empty ("Empty when unknown", §8.3.3) and `change` null. Package 26 fills the rest.
17. **`resolutions` is ignored here.** Unknown members are ignored by PLAT-FR-017; package 26 extends the same handler.
18. **The stack route returns the chain below, the change, and all changes above.** The PRD says "the whole stack the change is part of"; where a change has several dependants all are listed, ordered by depth and then number.
19. **Wiring to packages built earlier and later.** Package 22 calls `LinkVersion` of its `StackLinker` at open and on every version and package 23's `PATCH` calls `SetDependsOn`; package 23's abandon calls `Restacker`. This package implements both interfaces and registers them; it specifies what they do, and those packages specify nothing about `depends_on` beyond handing it over (package 22 validates only that it is a positive integer; package 23 relinks dependants on reparent). The fields `depends_on`, `needs_restack`, `restack_reason` and the derived `dependants` are package 22's and are written through its setters. The landing trigger is one function in `internal/change/`, registered as an after-landing hook of package 25 as decision 23 says; this package's tests call it directly.
20. **A restack that fails without a conflict stays pending** and is run again at the next start or at the next restack recorded for the same change; the manual call answers `500 internal`. The PRD says restacks are durable and not retried with backoff, and names no failure other than a conflict.
21. **One pending restack per change.** A newer one replaces the older, because the commit to sit on is the newest one.
22. **Profile: full.** Ten requirements and two end-to-end flows.
23. **The landing entry point reaches `internal/land` through `internal/hub` (with package 25).** Package 25 declares the after-landing hook list in `internal/land/hooks.go`; a hook is called with the landed attempt (its workspace, the update of the target branch and the session) and, per landed change, its number, its source workspace and branch and its landed commit. `internal/hub/change.go` registers an adapter of that shape which calls the entry point once per landed change with the workspace, the number and that change's landed commit. `internal/change` imports nothing of `internal/land`, and package 25 is not a dependency of this package; a hook failure never changes the land.
24. **The version guard of the manual restack (with package 29).** This package declares `VersionGuard` with `RegisterVersionGuard` in `internal/change/restack_handler.go`. The manual restack calls it after the replay is computed and before the compare-and-swap of the source branch, with the caller's credential (type, id and `paths`) and the base and head of the version to record; a refusal is answered unchanged (`403 path_not_allowed` with the path) and leaves the change, its branch and its flags as they were. Background restacks, whose actor is `system`, do not call it. Without a registered guard every version passes. Package 29 registers the guard.
25. **Wiring is in `internal/hub/change.go`**, beside the wiring of packages 22 and 23.

## Dependencies

- Package 23 `change_lifecycle_and_mergeability` (draft): the `PATCH` and abandon routes that call this package through `StackLinker` and `Restacker`, the caller rule of CHG-FR-020, and the function that stores `mergeable` with its conflict report. Through it: package 22 (change record, version recording, read routes), packages 18 and 21 (`internal/gitrepo` rebase and compare-and-swap, git push), package 11 (workspace settings), package 07 (access rule), package 52 (events), packages 02 and 09 (HTTP and CLI conventions).
- By registration, not as dependencies: package 25 `optimistic_landing` (its after-landing hook list, filled from `internal/hub/change.go`) and package 29 `auto_land_revert_and_path_limits` (which registers the version guard).
- The `git` command, 2.38 or later, at run time.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task.
- **Change**: The unit of work: a proposal to add the commits of a source branch to a target branch, with a number, a title, an intent, versions and a state.
- **Version**: One head of a change's source branch. Every push, API commit or restack that moves the source creates a new version. Old versions stay fetchable.
- **Stack**: Changes linked by `depends_on`, lowest first.
- **Restack**: The hub rebasing a dependent change onto the new head of the change below it, or onto the target once that change has landed.
- **Landing**: Adding a change's commits to its target branch: a server-side rebase followed by a compare-and-swap of the target ref.
- **Conflict report**: The structured description of why a rebase could not be done: files, kinds, and the landed changes involved.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number.
- **Dependant**: A `draft` or `open` change whose `depends_on` names the change in question.
- **Pending restack**: The stored record that a change must be rebased onto a given commit, kept until the restack ends.
