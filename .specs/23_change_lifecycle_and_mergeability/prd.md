---
spec_id: "23"
spec_name: "change_lifecycle_and_mergeability"
title: "Edit, abandon and restore changes and track mergeability"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "9ac67c7d1c5c6c45261795a08e7368b3554d7ad45f1517db584ef584c5e6d941"
schema_version: 3
source: "docs/prd/07_changes_and_stacks.md"
---

# Edit, abandon and restore changes and track mergeability

## Intent

A change lives longer than the push that opened it: its author rewords it, marks it ready, points it at another branch, gives it up and takes it back, and all the while the target branch moves under it. This package lets the people and agents who may modify a change do those things through the REST API and the `parley` CLI, and makes the hub tell them, without being asked, whether the change still applies to what it sits on, so that a conflict is known before anyone tries to land.

## Goals

- `PATCH /workspaces/:slug/changes/:number` edits `title`, `intent`, `labels`, `auto_land`, `state` (`draft` ⇄ `open`) and `target_branch`, for the author, for anyone with write access to the target workspace and for the admin token, and for nobody else.
- `POST …/abandon` abandons a change, with an explicit choice for its dependants; `POST …/restore` brings an abandoned change back to `open` at its last version, recreating a deleted source branch.
- A landed change accepts only a change of `labels`; an abandoned change accepts no edit.
- Every `draft` or `open` change carries `mergeable` (`clean`, `conflict` or `unknown`) for its current head against the current head of what it sits on, recomputed in the background after every new version and every move of the target branch, coalesced per change and spaced by `[changes].mergeable_debounce`.
- A change that starts to conflict gets a stored conflict report and one `parley.change.conflict` event; a change that becomes landable gets one `parley.change.ready` event.
- After a crash between a ref update and its bookkeeping, the next start records the missed head as a version.
- `parley change edit|ready|draft|abandon|restore|wait` drive the routes and wait for a change to be landed, ready or mergeable.

## Non-goals

- The change object, opening a change, versions and version refs, interdiff, reading and listing changes, the by-commit lookup, the session link, retention, the abandonment of a change when its source branch is deleted by a push or its source fork is deleted, the metric `parley_changes_open`, and `parley change open|list|show|find|versions|diff`: package 22 change_opening_and_versions.
- `depends_on` in the `PATCH` body with its error `invalid_dependency`, the flags `--depends-on` and `--no-depends-on` of `parley change edit`, the automatic stack link, stack depth, the restack itself (automatic and manual), `needs_restack`, the stack route and `parley change restack|stack`: package 24 change_stacks_and_restack. This package only hands a `depends_on` member of the `PATCH` body to the `StackLinker` of package 22, which package 24 implements, relinks the dependants of an abandoned change and asks for their restack through the interface `Restacker`, which package 24 implements too.
- Landing, the land attempt and the rule that landing rebases for real whatever `mergeable` says: package 25 optimistic_landing.
- The full shape of the conflict report, `GET …/conflict`, `GET …/conflict/file` and `parley change conflict|resolve`: package 26 conflict_reports_and_resolution.
- The policy document and the rules that produce `landing.status`: package 27 landing_policy. Approvals and checks: package 28 approvals_and_checks.
- Starting a land attempt when a change with `auto_land` becomes landable, and the refusal of `auto_land` for a path-scoped token (`403 path_not_allowed`, LAND-FR-081): package 29 auto_land_revert_and_path_limits. This package stores the flag and who set it (user, credential type and token globs, read by package 29 through the accessor `AutoLandSetter`), and asks the `auto_land` guard that package 29 registers (the `AutoLandGuard` of package 22) before a `PATCH` applies anything.
- The overlap index and claims that react to abandon, restore and retarget: package 30 awareness.
- The early expiry clock of a fork whose changes are all landed or abandoned: package 16 task_fork_lifetime.
- Abandonment with reason `source_lost` after a reclone: package 14 workspace_archive_reclone_delete.
- The binding guard that lets a fork-bound token reach these routes on the fork's parent: package 08 workspace_bound_tokens.
- The error envelope, the order of checks, the CLI's output, exit codes and waiting rules: packages 02 http_api_conventions and 09 cli_foundation. This package obeys them and does not restate them.
- An event for an edit of `title`, `intent`, `labels` or `auto_land`: the PRD defines none.

## Background

The repository holds no code yet; every file named here is new. Package 22 creates `internal/change/` with the change record, version recording and the read routes; this package adds files beside it: `internal/change/authz.go` (who may modify), `internal/change/edit.go` (the `PATCH` route), `internal/change/lifecycle.go` (abandon and restore), `internal/change/mergeable.go` (the background check), `internal/change/landable.go` (the `ready` announcement) and `internal/change/reconcile.go` (start-up reconciliation), each with its `_test.go`. The CLI commands go into `internal/cli/change.go`, which package 22 starts. The configuration key is read in `internal/config/`, the wiring is in `internal/hub/change.go`, and the end-to-end tests are in `test/e2e/change_lifecycle_test.go`.

The source is `docs/prd/07_changes_and_stacks.md`: CHG-FR-020 to CHG-FR-024, §7.3.4, the `PATCH`, abandon and restore routes of §7.5, the six commands of §7.6, the mergeability and reconciliation parts of §7.7, the events `open`, `version` (retarget), `conflict`, `ready`, `abandon` and `restore` of §7.8, `[changes].mergeable_debounce` of §7.9 and the edge cases of §7.10 that concern them.

## Requirements

### Who may modify a change

`PATCH`, abandon and restore need `changes:write` for a PAT (`403 insufficient_scope` otherwise, whether or not the workspace exists) and read access to the target workspace (`404 not_found`, "workspace not found", otherwise). A number that names no change of the workspace answers `404`, `error_type: change_not_found`. For a token bound to a fork F used on F's parent, a change whose source workspace is not F also answers `404 change_not_found` (CHG-FR-040).

Among the callers who can read the change, three may modify it (CHG-FR-020): the author, meaning the user whose id is `author.actor_id`, with any credential of that user; anyone with write access to the target workspace; and the admin token. Every other reader gets `403 forbidden` and the change is unchanged. This is what lets an org member who has only read access to a workspace edit the change they opened from their fork.

After that the target workspace's status is checked (WS-FR-007): `pending`, `cloning` or a running reclone, archive or delete answers `409 workspace_busy`; `failed` answers `409 workspace_not_ready`; `archived` answers `409 workspace_archived`.

### Editing title, intent, labels and auto_land

`PATCH /workspaces/:slug/changes/:number` takes any of `title`, `intent`, `labels`, `auto_land`, `state` and `target_branch`. It answers `200` with the change object. Members that are absent keep their value; `labels` replaces the whole list. All members of one request are applied together or not at all.

- `title`: 1 to 200 characters, no line break. `intent`: at most 16,000 characters. `labels`: at most 20 after duplicates are removed, each matching `^[a-z0-9][a-z0-9_.:/-]{0,62}$`. A wrong JSON type, a `state` other than `draft` or `open`, or a value outside these limits is `400 invalid_request`.
- The intent may be empty only while the change is `draft` or when the workspace setting `changes.require_intent` is false. A request whose result would be an `open` change with an empty intent under `require_intent: true` is `400`, `error_type: intent_required`.
- `auto_land: true` stores the flag and the calling user as the one who set it, with the credential type and the token's `paths` globs (LAND-FR-060 checks that user later); `false` clears them. A `PATCH` whose body has an `auto_land` member asks the registered `auto_land` guard first, with the caller, the change and the value, before any member of the request is applied; when the guard refuses (`403 path_not_allowed` with the path, for a path-scoped token) the whole request is refused and nothing changes.
- An abandoned change answers `409`, `error_type: change_closed`, to every edit. A landed change answers `409 change_closed` when the body has any of these members other than `labels`, and accepts `labels` alone (CHG-FR-024).
- A body with none of the members changes nothing and answers `200` with `updated_at` unchanged.

### Draft and open

`state: "open"` on a draft moves it to `open` when the title and intent rules hold, re-evaluates `landing` (the structural reason `draft` goes away) and emits `parley.change.open` with `was_draft: true`, `title`, `target_branch`, `source_workspace`, `source_branch` and `depends_on`. `state: "draft"` on an open change moves it to `draft`; its `landing.status` becomes `blocked` with reason `draft`; no event is emitted. Asking for the state the change already has changes nothing.

### Retargeting

`target_branch` on a `draft` or `open` change that has no `depends_on` and no dependants moves the change to another branch of the same workspace (CHG-FR-021). The new target passes the target rules of opening (CHG-FR-003, CHG-FR-005): it exists (`400 target_not_found`), it is not the change's own source branch (`400 invalid_request`), and it is not a carry-patch integration branch (`403 integration_branch`). A change in a stack answers `409 conflict`.

A retarget creates no version. It sets `base_sha` to the merge base of the head with the new target and recomputes `stats` and `paths`, re-evaluates `landing`, sets `mergeable` to `unknown`, schedules the check, and emits `parley.change.version` with `kind: "retarget"`. When the new target already contains every commit of the change, or a merge commit lies between the new target and the head, the retarget is still accepted and `landing.status` is `blocked` with reason `nothing_to_land` or `merge_commits` (§7.10). A change whose target branch was deleted can be retargeted. Naming the current target changes nothing.

### Abandon

`POST …/abandon` with `{"reason"?: string, "dependants"?: "reparent" | "abandon"}` (or no body) moves a `draft` or `open` change to `abandoned`, answers `200` with the change object and emits `parley.change.abandon` with `reason`. It never deletes the source branch or a version (CHG-FR-022). An abandoned or landed change answers `409 change_closed`. A `dependants` value other than the two, or a `reason` that is not a string, is `400 invalid_request`.

A change with dependants needs the choice: without `dependants` the answer is `409`, `error_type: has_dependants`, and nothing changes. With `abandon`, every change above it in the stack is abandoned in the same step, each with its own event. With `reparent`, each direct dependant's `depends_on` becomes the abandoned change's own `depends_on` (or null), and the hub asks, in the same transaction, for a restack of each with trigger `abandon`, so that it no longer holds the abandoned change's commits. The restack itself is package 24's.

### Restore

`POST …/restore` moves an abandoned change to `open` at its last version, answers `200` with the change object and emits `parley.change.restore` (CHG-FR-023). `landing` is re-evaluated, `mergeable` is `unknown` and a check is scheduled. If the source branch no longer exists it is recreated at the last version's head.

Refusals, in this order: the change is not abandoned (`409`, `error_type: change_not_abandoned`); the source workspace no longer exists (`409`, `error_type: source_workspace_gone`; the versions stay fetchable); another `draft` or `open` change uses the source branch (`409 change_exists`); the source branch is gone and the last version's commit is gone too, as after a reclone (`409 conflict`); the intent is empty while `changes.require_intent` is true (`400 intent_required`).

### Mergeability

`mergeable` answers whether the current version's own commits (`base_sha` to `head_sha`) would rebase cleanly onto the current head of the target branch, or, for a change whose `depends_on` names a `draft` or `open` change, onto the current head of that change (§7.3.4). The hub finds out with a trial rebase that writes no ref.

It is recomputed after every new version (a `restack` version of package 24 included), after every move of a target branch that has `draft` or `open` changes, after a new version of the change below, after a retarget and after a restore. A restack therefore needs no rule of its own: the version it records sets `mergeable` to `unknown` and the check runs for the new head against the head it now sits on. When a restack fails with a conflict, package 24 stores `conflict` and the report through this package's storing function, for the pair of the change's head and the commit it was replayed onto; when package 24 clears `needs_restack` without a new version, it calls this package's trigger and the value is computed again. Until the result for the new pair of commits is stored the value is `unknown`. Checks of one change coalesce: only the latest pair is computed, and a result for a pair that is no longer current is thrown away. Computations of one change start at least `[changes].mergeable_debounce` apart (default `5s`; `0s` means no spacing; a negative or unparsable value stops `parleyd` at start with a configuration error naming the key).

When the target branch does not exist, `mergeable` stays `unknown` and `landing.status` becomes `blocked` with reason `target_not_found`. When the trial rebase fails for another reason than a conflict, or runs longer than 30 seconds, `mergeable` stays `unknown`, the failure is logged and the next trigger tries again.

### Conflict and ready events

When a computation finds a conflict and the last computed value was not `conflict`, the hub stores the conflict report (the type `ConflictReport` of `internal/gitrepo`, declared by package 20) on the change and emits `parley.change.conflict` with `files` (the conflicted paths) and `needs_restack`. While the change stays in conflict, across versions, the report is replaced and no further event is emitted. A computation that finds `clean` clears the report.

A change is landable when it is `open`, its `landing.status` is `ready` and its `mergeable` is `clean`. When a change becomes landable for a head it has not been announced for, the hub emits one `parley.change.ready` (actor `system`). The announcement is forgotten when the change stops being landable with a computed `mergeable`, and when it is retargeted; a target move that leaves the change `clean` emits nothing. The announcement runs after each mergeability result and after each edit of this package that re-evaluates `landing`. It is one exported function, `AnnounceLandable` in `internal/change/landable.go`, and the only writer of `parley.change.ready`: package 27 runs it in every transaction in which it stores a `landing`, and no other package writes the event.

Every event of this package has `resource_type: "change"`, the change number as `resource_id`, the target workspace, the session when known, and `number`, `ref`, `version` and `head_sha` in its metadata. An event and the state change it reports are written together.

### Start-up reconciliation

Before `parleyd` accepts requests it compares, for every `draft` or `open` change, the head of the current version with the head of the source branch. A differing head is recorded as a new version, with everything a new version brings (CHG-FR-011). A missing source branch abandons the change with reason `source_deleted`. Every `draft` or `open` change whose `mergeable` is `unknown` gets a check scheduled. A change that cannot be reconciled is logged and skipped; the start goes on.

### CLI

- `parley change edit <slug> <number>` with `--title`, `--intent` (text, `@file`, or `-` for stdin), `--label` and `--remove-label` (repeatable), `--auto-land` or `--no-auto-land`, `--target`: sends one `PATCH` with the members the flags name. With label flags it first reads the change and sends the resulting list.
- `parley change ready <slug> <number>` and `parley change draft <slug> <number>` send `PATCH` with `{"state":"open"}` and `{"state":"draft"}`.
- `parley change abandon <slug> <number>` with `--reason` and `--dependants reparent|abandon`; `parley change restore <slug> <number>`.
- `parley change wait <slug> <number>` with `--for landed|ready|mergeable` (default `landed`), `--timeout` (default `5m`) and `--poll-interval` (default `5s`): polls the change. Exit 0 with the change on stdout when the condition holds; exit 1 with `wait_failed` and the change on stdout when it can no longer hold; exit 1 with `wait_timeout` and nothing on stdout on timeout.

Each command prints the route's response and exits by the rules of PLAT-FR-092 and PLAT-FR-093. `edit` without an edit flag, a flag together with its `--no-` form, an unknown `--for` or `--dependants` value, and a `--timeout` or `--poll-interval` that is not positive are usage errors (exit 2) and send no request.

### End-to-end flows

1. An agent opens a draft from its fork, runs `parley change edit` to set the intent and `parley change ready`; `parleyd` moves the change to `open`, emits `parley.change.open`, computes `mergeable: clean` in the background and emits `parley.change.ready`; `parley change wait --for ready` exits 0.
2. A developer's push moves the target branch so that an open change no longer applies; `parleyd` recomputes, stores the conflict report and emits one `parley.change.conflict`; the agent pushes a fixed version; `parleyd` computes `clean` and clears the report.
3. A developer abandons a change, deletes its source branch with git and restores the change; `parleyd` recreates the branch at the last version's head and the change is `open` again.
4. An operator restarts `parleyd` after a source branch moved without its bookkeeping; `parleyd` records the missed head as a version before it serves requests.

## Design Decisions

1. Language Go; git operations run the `git` binary through `internal/gitrepo`; no git library. The trial rebase is the one `internal/gitrepo` offers to the compare route of package 20. (Project decisions 1 and 6.)
2. State is in the embedded SQLite store behind `internal/store`; an event is written in the same transaction as its state change through `internal/event`. (Project decision 2.)
3. HTTP uses `net/http`; the CLI uses `github.com/spf13/cobra`; tests use `testing`, `httptest` and repositories made with `git init` in `t.TempDir()`. (Project decisions 3, 4 and 7.)
4. `depends_on` in the `PATCH` body is package 24's. This package hands a present member to `SetDependsOn` of the `StackLinker` interface that package 22 declares and package 24 implements, inside the `PATCH` transaction, and specifies nothing about it: its validation (`400 invalid_dependency`), its effect and its restack are in package 24. While no stack linker is registered the member is ignored like any unknown member (PLAT-FR-017). Why: the plan gives `depends_on` to package 24, which depends on this one.
5. The restack of reparented dependants is done by a restack collaborator that package 24 supplies. This package declares the interface `Restacker` in `internal/change/lifecycle.go` (request the restack of one change with a trigger name, inside a store transaction) and calls it inside the abandon's transaction, after it has relinked the dependants itself; while none is registered, abandon relinks the dependants, sets their `mergeable` to `unknown` and schedules their check. Why: this package cannot depend on package 24, and package 24 writes its pending restack in the transaction of the event that caused it.
6. `dependants=abandon` abandons the whole stack above, not only the direct dependants, each with the request's `reason`. Why: a change left depending on an abandoned change has nothing to sit on.
7. The `reason` of an abandon is stored and emitted as given; absent, it is the empty string. `dependants` on a change with no dependants is accepted and has no effect. Why: the PRD names no default and no limit other than the body size.
8. A retarget of a change that is in a stack answers `409 conflict`. Why: the PRD forbids it and names no specific type.
9. Restoring a change whose source branch and last version's commit are both gone answers `409 conflict`. Why: §7.10 says it cannot be restored and names no type.
10. Restoring a change with an empty intent under `changes.require_intent: true` answers `400 intent_required`. Why: restore moves to `open`, and an open change may not have an empty intent; a new change can be opened from the branch.
11. Restoring a change whose source branch exists at a head other than the last version's records that head as a new version of kind `ref`, created by the restoring caller. Why: an open change's current version is always the head of its source branch, and `ref` is the kind of every version recorded for a head that was already on the source branch (decision 23 of package 22: a REST open, a restore, start-up reconciliation).
12. A version recorded by start-up reconciliation has kind `ref` and `created_by.actor_type` `system`, with no creating credential. Why: the original cause is unknown after a crash, and the rule of decision 11 applies. This is also what closes the gap the ref writer of package 18 leaves when the hub stops between its ref commit and its store commit.
13. Start-up reconciliation abandons a `draft` or `open` change whose source branch is missing, with reason `source_deleted` and actor `system`. Why: it is the bookkeeping of CHG-FR-013 that a crash lost.
14. Start-up reconciliation finishes before the listener accepts requests, and schedules a check for every change left `unknown`. Why: no push can race it, and mergeability checks are not durable.
15. Transitions are judged against the last computed value; `unknown` in between does not count. `parley.change.conflict` is therefore not repeated for a new version that still conflicts (§14.8: "not repeated while it stays so"), and `parley.change.ready` is emitted once per head and landable period, not after every target move.
16. A failed or timed-out trial rebase (30 seconds, the read deadline of §6) leaves `mergeable` at `unknown` and is not retried on a timer. Why: the next version or target move triggers it again; the PRD defines no retry.
17. `[changes].mergeable_debounce` accepts `0s`; a negative or unparsable value is a configuration error at start.
18. A `PATCH` with none of the editable members answers `200` and changes nothing, `updated_at` included. Editing `title`, `intent`, `labels` or `auto_land` emits no event. Duplicate labels are removed, keeping the first occurrence, before the limit of 20 is applied.
19. The three write routes apply the workspace status answers of WS-FR-007 to the target workspace.
20. `parley change wait`: `--for ready` holds when `landing.status` is `ready` and `mergeable` is `clean`, and fails on `landing.status: blocked` or `mergeable: conflict`; `--for mergeable` holds on `mergeable: clean` and fails on `conflict` only. An abandoned change fails every wait; a landed change ends every wait with exit 0. Why: the PRD's sentence about `blocked` and `conflict` is read per condition, since a draft is always `blocked`.
21. `parley change edit --intent` takes the same three forms as `parley change open --intent`.
22. Triggers come from two registrations (with packages 18 and 22): a version observer of package 22 for every new version, and a ref-updated stage of package 18's ref writer for target branches moved by a push, an API write or a land; a sync reaches this package through the branch-move observer of package 12. Why: those are the places where the hub learns of the move inside the transaction that makes it.
23. One function stores a mergeability result with its conflict report and the `parley.change.conflict` event, and package 24 uses it for a restack conflict (with package 24). Why: two writers of `mergeable` and of the event would announce one conflict twice.
24. Abandon and restore of a change whose source is a fork report fork activity through the `ForkActivity` interface of package 22 (with package 16): abandon through package 22's move to `abandoned`, restore by a call of its own. A landing is reported by package 16's own after-landing hook in package 25.
25. `AnnounceLandable` is the single writer of `parley.change.ready` (with packages 27 and 29). It takes the store transaction and the change (workspace and number), writes the event and remembers the announced head, or forgets the announcement, as REQ-8 says, and does nothing while `mergeable` is `unknown`. Package 27 calls it in every transaction that stores a `landing`; package 29 starts automatic lands from the event alone. Why: the condition has two inputs with two owners, and one writer that remembers the announced head gives one event per transition.
26. `PATCH` asks the `auto_land` guard before it applies anything (with packages 22 and 29). The guard is the `AutoLandGuard` that package 22 declares in `internal/change/collaborators.go` and package 29 registers. It is called inside the `PATCH` transaction, after validation and the state rules and before the first member is written, for every body that has an `auto_land` member, with the caller (user and credential), the change and the value; a refusal is answered unchanged and rolls the request back. Only `true` can be refused; the call for `false` lets package 29 drop what it keeps about the flag. Without a registered guard the flag is stored as given. The flag has one store: this package extends `SetAutoLand(tx, change, value, caller)` of `internal/change`, through which the `Open` of package 22 and this `PATCH` both write the flag, to store the setter beside it (the user's id, the credential type and the token's `paths` globs or null) and exposes the accessor `AutoLandSetter(tx, change)`; package 29 keeps no setter row of its own and reads the accessor.
27. Wiring is in `internal/hub/change.go`: the routes, the worker's start and stop, the observer and stage registrations and the start-up reconciliation are registered there, beside package 22's.

## Dependencies

- Package 22 change_opening_and_versions (draft): the change record and object, version recording, version observers, the `StackLinker`, `ForkActivity` and `AutoLandGuard` interfaces, the target rules of opening, the `landing` re-evaluation step, the read route the CLI polls, and `internal/cli/change.go`.
- Package 18 git_push_rules (draft): the ref writer's ref-updated stages. Package 12 workspace_sync_from_origin (draft): the branch-move observer of sync.

Through package 22 this package also uses the access rule (07), the event log (52), the HTTP conventions (02), the CLI foundation (09) and `internal/gitrepo`.

## Verified External API

- `github.com/spf13/cobra` v1.10.2, read in the module cache: `type Command struct { Use string; Args PositionalArgs; RunE func(cmd *Command, args []string) error; … }`, `func (c *Command) AddCommand(cmds ...*Command)`, `func (c *Command) Flags() *pflag.FlagSet`, `func ExactArgs(n int) PositionalArgs`.
- `github.com/spf13/pflag` v1.0.10, read in the module cache: `func (f *FlagSet) Changed(name string) bool`, `func (f *FlagSet) StringVar(p *string, name string, value string, usage string)`, `func (f *FlagSet) StringArrayVar(p *[]string, name string, value []string, usage string)`, `func (f *FlagSet) BoolVar(p *bool, name string, value bool, usage string)`, `func (f *FlagSet) DurationVar(p *time.Duration, name string, value time.Duration, usage string)`.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires: after 7 days by default, or 24 hours after all its changes have landed or been abandoned.
- **Change**: The unit of work: a proposal to add the commits of a source branch to a target branch, with a number, a title, an intent, versions and a state.
- **Version**: One head of a change's source branch. Every push, API commit or restack that moves the source creates a new version. Old versions stay fetchable.
- **Intent**: The author's statement of why the change exists.
- **Stack**: Changes linked by `depends_on`, lowest first.
- **Restack**: The hub rebasing a dependent change onto the new head of the change below it, or onto the target once that change has landed.
- **Landing**: Adding a change's commits to its target branch: a server-side rebase followed by a compare-and-swap of the target ref.
- **Conflict report**: The structured description of why a rebase could not be done: files, kinds, and the landed changes involved.
- **Policy**: A workspace's ordered rules that map touched paths to landing requirements.
- **Bound token**: A PAT confined to one workspace (and the forks created with it). A token bound to a fork also reaches its own changes, and the awareness and policy reads, on the fork's parent (§3).
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
- **Landable**: An `open` change whose `landing.status` is `ready` and whose `mergeable` is `clean`.
