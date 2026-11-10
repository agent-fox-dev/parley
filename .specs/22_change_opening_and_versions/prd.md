---
spec_id: "22"
spec_name: "change_opening_and_versions"
title: "Track work as changes with versions"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "56b037efccb05ae953139f83fc228e0f57eea1022d20679d95f28bd14e54fbb7"
schema_version: 3
source: "docs/prd/07_changes_and_stacks.md"
---

# Track work as changes with versions

## Intent

A change is Parley's unit of work: a durable, numbered proposal to add the commits of a source branch to a target branch, for a stated reason. Agents and developers need the change to keep its number while its branch is rewritten, to remember every head the branch ever had as a fetchable version, and to say what moved between two versions, so that everything downstream (policy, approvals, landing, the audit record, the session record) can address one stable object. This package delivers that object: opening it, recording its versions together with the ref update that caused them, and reading it.

## Goals

- `POST /workspaces/:slug/changes` opens a change with a per-workspace number that is never reused, records version 1 and answers `201` with the change object.
- Every move of a `draft` or `open` change's source branch to a new head is recorded as a version in the same store transaction as the ref update, by a stage this package registers with the ref writer of package 18; a move to the current head records nothing.
- Every version's head stays fetchable at `refs/parley/changes/<number>/<version>` in the target workspace, whatever happens to the source branch.
- A caller reads one change, lists changes with the eleven filters of CHG-FR-041, lists and reads versions, finds a change by commit id, and reads a version's diff or the interdiff of two versions.
- The component functions that open a change, record a version, react to a deleted source branch, a deleted fork and a deleted workspace, and look a change up by commit, branch or session are callable inside a store transaction that the caller owns, so that packages 19, 21, 24, 25 and others call them with their own ref updates.
- `parley change open|list|show|find|versions|diff` call these routes.

## Non-goals

- `PATCH` of a change, abandon, restore, the draft/open switch, retargeting, the mergeability computation and its events, start-up reconciliation of missed heads, `parley change edit|ready|draft|abandon|restore|wait`: package 23 change_lifecycle_and_mergeability. This package only sets `mergeable` to `unknown` and notifies registered observers.
- `depends_on` (explicit and automatic), `invalid_dependency`, stack depth, restack, `needs_restack`, `GET …/stack`, the `stack` expansion, `parley change restack|stack`: package 24 change_stacks_and_restack. This package stores the stack fields, declares the `StackLinker` interface that package 24 implements, hands the request's `depends_on` to it and serves `stack_of` from the stored links.
- Opening a change from a git push, the `parley.*` push options and the `remote:` lines: package 19 git_push_to_change. It calls this package's component to open; the version of a push to an existing change is recorded by this package's ref-updated stage.
- The API commit, ref and rebase routes themselves, their `change` object and `change_error`, and `409 ref_in_use`: package 21 scm_write_api. The `change` member of ref and commit objects: package 20 scm_read_api. Both read this package's lookups; package 21 reaches this package through the interface `ChangeLinker` it declares in `internal/scm/linker.go`, with which this package registers.
- Landing, the `landed` object and the landed-commit records: package 25 optimistic_landing. It writes them through this package's store.
- Conflict reports: package 26 conflict_reports_and_resolution.
- Policy evaluation into `landing.requirements`: package 27 landing_policy. This package derives only the structural reasons and otherwise asks the registered evaluator.
- Approvals, checks, approval carry-over and the `approvals` and `checks` expansions: package 28 approvals_and_checks.
- The path check of LAND-FR-080 at ref move, rebase and restack, and LAND-FR-081 on `auto_land`: package 29 auto_land_revert_and_path_limits, which registers the `auto_land` guard this package declares and calls at open. The revert route (LAND-FR-070): package 29 too; it opens the reverting change through `Open` with `reverts` and version kind `revert`. The check at git push: package 18 git_push_rules. This package checks the path limit only when a change is opened through `POST …/changes`.
- Overlaps and the `overlaps` expansion: package 30 awareness.
- The session record and the session-linking rule itself: package 49 agent_sessions_and_usage, reached through the interface `SessionLinker` of `internal/change`.
- A fork's last-activity time and its early removal: package 16 task_fork_lifetime, which registers the recorder behind the interface `ForkActivity` of `internal/change`. The event log: package 52 durable_event_log_and_stream.
- What a reclone does to changes (`source_lost`, `sync` versions): package 14 workspace_archive_reclone_delete; a `sync` version at a sync from origin: package 12 workspace_sync_from_origin, whose branch-move observer this package registers with to record it. Package 14 calls this package's component.
- The metric `parley_changes_open{workspace}` (§7.7): not covered by this package; package 51 audit_query_and_retention exports it from the change store.

## Background

The repository holds no code yet; every file named here is new. The source is `docs/prd/07_changes_and_stacks.md`: CHG-FR-001 to CHG-FR-014, CHG-FR-040 to CHG-FR-043 and CHG-FR-050, with §7.3.1, §7.3.2, the routes of §7.5, the commands of §7.6, the events of §7.8 and the edge cases of §7.10 that belong to them.

The code lives in `internal/change/` (the store tables, the component, the REST handlers) and `internal/cli/change.go` (the commands). It builds on: the route registration, error envelope, list paging and idempotency of `internal/server` (package 02); the authenticated principal, scopes and path-scoped tokens of `internal/identity` (packages 04 and 06); the access rule and the binding information of `internal/access` (packages 07 and 08); the store and its transactions in `internal/store` (package 03); the `git` wrapper `internal/gitrepo`; the push rules and hub-owned refs of package 18; and the diff computation and the commit, ref and rebase routes of `internal/scm` (packages 20 and 21).

A change is opened in three ways with the same result (CHG-FR-001): the REST route of this package, a git push (package 19) and an API commit (package 21). The last two reach this package as calls to its component. Every later move of a source branch reaches it through the ref writer of package 18, which runs this package's ref-updated stage inside the commit step of the ref update, for a git push and for the commit, ref and rebase routes of package 21 alike.

## Requirements

### The change and version objects (§7.3.1, §7.3.2)

A change belongs to the workspace that holds its target branch. The change object has the fields of §7.3.1: `workspace_slug`, `number`, `ref` (`<workspace_slug>#<number>`), `title`, `intent`, `state` (`draft`, `open`, `landed`, `abandoned`), `target_branch`, `source` (`{workspace_slug, branch}`), `version`, `head_sha`, `base_sha`, `depends_on`, `dependants`, `author` (`{actor_type, actor_id, username}`), `session_id`, `sessions`, `mergeable` (`clean`, `conflict`, `unknown`), `conflict`, `needs_restack`, `restack_reason`, `stats` (`{commits, files_changed, insertions, deletions}`), `paths` (at most 2,000) with `paths_truncated`, `landing` (`{status, reason, requirements}`), `auto_land`, `labels`, `landed`, `reverts`, `reverted_by`, `created_at`, `updated_at`. A change this package has just opened has `depends_on` null, `dependants` `[]`, `mergeable` `unknown`, `conflict` null, `needs_restack` false, `restack_reason` null, `landed`, `reverts` and `reverted_by` null, unless a registered collaborator set them.

A version has `number` (1, 2, 3 …), `head_sha`, `base_sha`, `kind` (`push`, `commit`, `ref`, `rebase`, `restack`, `revert`, `sync`), `patch_equivalent_to`, `created_by`, `session_id`, `stats`, `created_at`. Versions are immutable. Commit ids are full 40-character ids.

### Who may open (CHG-FR-002, CHG-FR-012, CHG-FR-040)

`POST /workspaces/:slug/changes` needs the scope `changes:write` for a PAT (`403 insufficient_scope`), at least read access to the target workspace `:slug` (`404 not_found` otherwise) and write access to the source workspace. An org member can therefore open a change from their own fork into a workspace they can only read. A source workspace the caller can read but not write answers `403 forbidden`.

A token bound to a fork F reaches the route on F's parent only with `source.workspace_slug` equal to F; any other source answers `403 token_bound`.

A path-scoped token opens a change only when every file of the resulting version against its base matches one of the token's globs; otherwise `403 path_not_allowed` and nothing is written (CHG-FR-012, LAND-FR-080).

The target and the source workspace must be usable (WS-FR-007): status `pending`, `cloning` or an exclusive operation answers `409 workspace_busy`, `failed` answers `409 workspace_not_ready`, `archived` answers `409 workspace_archived`.

### Validation at open (CHG-FR-003, CHG-FR-005, CHG-FR-043)

The body is `{"source": {"workspace_slug"?, "branch"}, "target_branch"?, "title"?, "intent"?, "draft"?, "depends_on"?, "auto_land"?, "labels"?, "session_id"?}`. A missing `source.branch`, a member of the wrong type, more than 20 labels, a label that does not match `^[a-z0-9][a-z0-9_.:/-]{0,62}$`, or a `depends_on` that is not a positive integer is `400 invalid_request`. A `session_id` that fails the session-linking rule of §14 is `400 unknown_session`.

Then, in this order, the first failure decides: the target branch exists in the target workspace (`400 target_not_found`); in a carry-patch workspace the target is not the integration branch (`403 integration_branch`); the source workspace is the target workspace or a fork of it (`400 source_not_related`); the source branch exists (`400 source_not_found`); source and target are not the same branch of the same workspace (`400 invalid_request`); the source has at least one commit the target does not contain (`400 nothing_to_land`); none of those commits is a merge commit (`400 merge_commits_unsupported`); no other `draft` or `open` change has this source (`409 change_exists`, naming the existing change); the title is 1 to 200 characters on one line (`400 invalid_request`); the intent is at most 16,000 characters (`400 invalid_request`); and, unless `draft` is true or the workspace setting `changes.require_intent` is false, the intent is not empty (`400 intent_required`).

A refused open writes nothing: no change, no version, no ref, no event. Of two concurrent opens with the same source, exactly one succeeds and the other answers `409 change_exists`.

### Defaults and the result of opening (CHG-FR-004, CHG-FR-006)

`source.workspace_slug` defaults to `:slug`. `title` defaults to the subject of the head commit. `intent` defaults to the body of the head commit, then to the `purpose` of the source fork. `target_branch` defaults to the fork's base branch for a fork source, otherwise to the workspace's default branch. `session_id` defaults to the session given on the push or commit that triggered the open, then to the session recorded on the source fork.

Opening allocates the next number of the workspace (starting at 1, never reused, not necessarily contiguous), records version 1, writes `refs/parley/changes/<number>/1`, hands the change to the stack linker, derives and stores `landing`, sets `mergeable` to `unknown`, then notifies the version observers and emits `parley.change.version` for version 1. The state is `draft` with `draft: true`, else `open`; `parley.change.open` (with `title`, `target_branch`, `source_workspace`, `source_branch`, `depends_on`, `was_draft: false`) is emitted only for a change created open. Every change event has `resource_type` `change`, `resource_id` the number, the target workspace, the session when known, and `ref`, `version` and `head_sha` in its metadata.

`landing` for a draft is `{status: "blocked", reason: "draft"}`; otherwise it is what the landing evaluator answers, which without a registered evaluator is `{status: "ready", reason: null, requirements: []}`.

An open with `auto_land: true` asks the registered `auto_land` guard before the flag is stored, with the caller (user and credential) and the change; the guard's refusal (`403 path_not_allowed` with the path, from package 29) refuses the open and nothing is written. Without a registered guard the flag is stored as given.

`Open` also takes `reverts`, the number of the change being reverted, and the version kind `revert`. Only the revert of package 29 passes them; the body of `POST …/changes` has no such member. The opened change then reads `reverts: <number>` and its version 1 has kind `revert`.

### Versions (CHG-FR-010, CHG-FR-011, §7.10)

The component records a version when it is told, inside the transaction of a ref update, that a branch moved to a new head and that branch is the source of a `draft` or `open` change. It is told by the ref-updated stage it registers with the ref writer of package 18: the stage gets the store transaction of the ref update, the accepted updates, `via`, the version kind the surface named (`push`, `commit`, `ref`, `rebase`), the session and the caller's credential, records the version, and reports `{workspace_slug, number, version}` back for the surface to show. An update flagged as a direct push, a land and a version kind of none record nothing; a deleted branch is handed to `SourceDeleted`. A move to the commit that is already the head of the current version records nothing; a branch that is not such a source records nothing. A complete rewrite of the branch (unrelated history) is still a new version of the same change.

A new version gets the next number, the `kind` the caller names, `created_by`, the creating credential (credential type and id, stored on the version record, not shown in the version object, and none for a version the hub creates), the session and its stats; the change's `version`, `head_sha`, `base_sha`, `stats`, `paths`, `session_id`, `sessions` and `updated_at` follow it; `mergeable` becomes `unknown`; the stack linker, the landing derivation and the version observers run in that order, each observer with the change, the version and its creating credential, and only after the new `landing` is stored, in the same transaction, so that an observer reads the evaluation of the version it is told about; `parley.change.version` is emitted with `kind`, `base_sha`, `patch_equivalent_to` and `draft`. `base_sha` is the merge base of the new head and the target branch. `patch_equivalent_to` names the latest earlier version whose diff against its own base is identical to this one's: a pure rebase is patch-equivalent to its predecessor.

A version whose head is equal to or behind the target is recorded with `landing` `{blocked, nothing_to_land}`; one with a merge commit among its commits with `{blocked, merge_commits}`; one recorded while the target branch does not exist with `{blocked, target_not_found}`. The change stays in its state.

A session on a version that fails the session-linking rule refuses the triggering operation with `unknown_session`. When the caller's transaction rolls back, no version, change field or event of that call remains.

The ref `refs/parley/changes/<number>/<version>` resolves to the version's head in the target workspace for as long as the change exists, also for a fork source and after the source branch was deleted.

### Source deletion and retention (CHG-FR-013, CHG-FR-050)

When the source branch of a `draft` or `open` change is deleted (a git push; the API refuses the delete in package 21), the change becomes `abandoned` with reason `source_deleted` and `parley.change.abandon` is emitted. When a fork is deleted or expires, every `draft` or `open` change whose source workspace it was becomes `abandoned`; landed and abandoned changes and all version refs stay in the target workspace. When a workspace is deleted, its changes and versions are deleted with it. Abandonment never deletes a version or a version ref, and a new change may then be opened from the same source.

### Reading (CHG-FR-040, CHG-FR-042, §7.5)

Reading needs `changes:read` (`403 insufficient_scope`) and read access to the workspace (`404 not_found`).

- `GET …/changes/:number` answers `200` with the change object. `include` is a comma list of `versions`, `approvals`, `checks`, `stack`, `overlaps`; another name is `400 invalid_request`. `versions` adds the version list.
- `GET …/changes/:number/versions` lists the versions, oldest first.
- `GET …/changes/:number/versions/:version` answers the version object plus `ref`; `:version` is a number or `latest`.
- `GET …/changes/by-commit/:sha` answers `200 {"change", "version", "landed"}` for a commit that is a version head or was landed by a change, else `404 change_not_found`.
- An unknown change is `404 change_not_found`.
- For a token bound to a fork F, on F's parent, a change whose source workspace is not F answers `404 change_not_found`, and the list holds only F's changes.

### Listing (CHG-FR-040, CHG-FR-041)

`GET …/changes` answers a list of change objects without `conflict`, and without `paths` unless `include=paths`. Filters: `state` (repeatable; default `draft` and `open`), `target_branch`, `source_workspace`, `author` (user id or `me`), `session_id`, `label`, `path` (glob), `mergeable`, `landing_status`, `stack_of=<number>` (the stack of that change, bottom first). Order: `updated_at` descending, or `number` ascending with `sort=number`. On a fork's slug the route lists the changes whose source workspace is the fork.

### Diff and interdiff (CHG-FR-014, §7.10)

`GET …/changes/:number/diff` takes `version` (default `latest`), `against`, `path` (repeatable glob), `format` (`json`, `patch`) and `patches` (bool). Without `against` the answer is the version against its base, in the shape of `GET …/scm/diff` plus `version` and `against`. With `against` it is the interdiff: per file, whether it changed between the two versions, with the patch of that difference; a version that is patch-equivalent to the other has an empty interdiff. A version whose commit no longer exists answers `404 ref_not_found`.

### CLI (§7.6)

`parley change open <slug>` (`--source` required, `--source-workspace`, `--target`, `--title`, `--intent` as text, `@file` or `-` for stdin, `--draft`, `--depends-on`, `--auto-land`, `--label` repeatable, `--session`), `parley change list <slug>` (`--state` repeatable, `--target`, `--author`, `--session`, `--label`, `--path`, `--mergeable`, `--landing-status`, `--stack-of`, `--limit`, `--cursor`), `parley change show <slug> <number>` (`--include`), `parley change find <slug> <sha>`, `parley change versions <slug> <number>` and `parley change diff <slug> <number>` (`--version`, `--against`, `--path` repeatable, `--patch`, `--name-only`). Each prints the route's response and exits by the rules of PLAT-FR-092 and PLAT-FR-093.

### End-to-end flows

1. A developer opens a change with `parley change open`; `parleyd` validates it, records version 1 and its ref and answers `201`; the developer reads it with `parley change show`, and a git client fetches `refs/parley/changes/1/1`.
2. An agent opens a change, then commits to the source branch through the API; `parleyd` records version 2 in the transaction of the ref update; the agent reads the versions and the interdiff of 2 against 1.

## Design Decisions

1. **Profile: full.** Two end-to-end flows cross the CLI, the router, the change component, the store and git.
2. **The component runs in the caller's transaction.** `Open`, `RecordVersion`, `SourceDeleted`, `ForkDeleted` and `WorkspaceDeleted` take an open `internal/store` transaction and neither begin nor commit one. Why: CHG-FR-010 requires the version in the transaction of the ref update, which another package owns.
3. **Version refs are written by the component itself, through `internal/gitrepo`, before it returns.** A ref left behind by a transaction that rolls back names no stored version and is overwritten when that number is recorded. For a fork source the head commit is first made present in the target workspace's repository. Why: a git ref cannot join a store transaction, and an extra ref is harmless where a missing one is not.
4. **This package owns the whole change record**, including the fields other packages compute (`depends_on`, `dependants`, `mergeable`, `conflict`, `needs_restack`, `restack_reason`, `landing`, `landed`, landed commits, `reverts`, `reverted_by`), and offers setters for them. Why: one object is read here; writers elsewhere need one place.
5. **Collaborators are narrow interfaces declared in `internal/change/collaborators.go` and wired in `internal/hub/change.go`**: a workspace reader (status, kind, parent, default branch, fork base branch, purpose and session, `changes.require_intent`, integration branch), `SessionLinker` (package 49; the same name and check as in `internal/gitserver` and `internal/scm`, plus a record operation that only this package declares, decision 30), an event writer, `StackLinker` (package 24: `LinkVersion` at open and on every version, `SetDependsOn` for the `PATCH` of package 23), a landing evaluator, version observers, `AutoLandGuard` (package 29), `ForkActivity` (package 16) and include expanders. Defaults: no stack link; landing `ready`; no observers; `auto_land` stored as given; no activity recorded; an explicit `session_id` is refused and no session link is recorded while no session linker is registered; an `include` name with no expander adds no member. Why: the plan gives this package only packages 18 and 21 as dependencies, while packages 23, 24, 27, 28, 30, 49 and 52 supply these later.
6. **Versions are recorded in a ref-updated stage of package 18's ref writer (with packages 18, 19 and 21).** This package registers one stage; the ref writer runs it inside its commit step, in the store transaction of the ref update, for every surface: a git push (kind `push`), an API commit (`commit`), a ref move (`ref`) and a rebase (`rebase`). The stage reports the recorded version to the ref writer, from which package 19 words its `remote:` line and package 21 fills the `change` member. An error of the stage stops the ref from moving. This package also registers itself as the `ChangeLinker` that package 21 declares in `internal/scm/linker.go` (lookup, opening check, open), and package 19 calls `Open` for a push. Why: CHG-FR-010 asks for the version in the transaction of the ref update; packages 18 and 21 precede this one and cannot call it.
7. **Authorization is a component function** (`Authorize`) used by the REST route and offered to packages 19 and 21, so the rule of CHG-FR-002 exists once.
8. **A source workspace that does not exist, or on which the caller has no access, answers `400 source_not_related`**; one the caller reads but cannot write answers `403 forbidden`. Why: one answer for unknown and invisible leaks nothing. Source access is checked after body validation and before the checks of CHG-FR-003.
9. **`403 integration_branch` is checked directly after the target branch exists; `unknown_session` with the body; `path_not_allowed` after the whole order of CHG-FR-003.** The PRD gives these no position.
10. **Same source and target branch is `400 invalid_request`**; the PRD names no `error_type`.
11. **Defaults that are too long are cut; explicit values are refused.** A defaulted title is cut to 200 characters and a defaulted intent to 16,000. A head commit with an empty subject and no `title` is `400 invalid_request`. An intent of only white space counts as empty. Duplicate labels are dropped, keeping the first.
12. **A defaulted session that fails the session-linking rule is dropped** (`session_id` null), not refused; only an explicit `session_id` is refused.
13. **`409 change_exists` carries `details: {"workspace_slug", "number", "ref"}`** of the existing change.
14. **Patch equivalence uses `git patch-id --stable`** over the diff of base to head, stored per version. Two empty diffs are equivalent.
15. **A version recorded while the target is missing keeps the previous version's `base_sha`.**
16. **The fork-deletion reason is `source_workspace_deleted`.** The PRD names none. Dependants of a change abandoned by this package are left to package 24.
17. **By-commit resolution.** A change that landed the commit wins over one that has it as a version head; among several, the lowest number. `version` is the highest version of that change with this head (for a landed commit: the version whose head it was rebased from, or null); `landed` says whether the commit was landed by the change.
18. **Path parameters.** A `:number` that is not a positive integer is `404 change_not_found`. A `:version`, `version` or `against` that is neither a positive integer nor (where allowed) `latest` is `400 invalid_request`; one that names no version is `404 not_found`. A `:sha` that is not 40 hexadecimal characters is `400 invalid_request`.
19. **List details.** Ties in `updated_at` order by `number` descending. `stack_of` fixes the order (bottom first) and ignores `sort` and the default `state` filter; an unknown `stack_of` change is `404 change_not_found`. `author=me` with the admin token, an unknown `state`, `mergeable`, `landing_status` or `sort` value is `400 invalid_request`. `path` uses the glob syntax of path-scoped tokens (package 06) over the stored `paths`. `include` on the list accepts only `paths`.
20. **Change routes other than the list, on a fork's slug,** act on the fork as target workspace: a fork holds no changes, so reads answer `404 change_not_found`, and an open answers by the checks above.
21. **Interdiff.** A file is `changed` when its patch (base to head, hunk bodies compared without `index` lines and hunk positions) differs between the two versions or only one version touches it. The `patch` of a changed file is the unified diff of its content at the head of `against` to its content at the head of `version`. The answer lists every file either version touches; `base_sha` is the head of `against`, `head_sha` the head of `version`, `merge_base_sha` null; `stats` counts changed files only. The plain diff has `base_sha` and `merge_base_sha` equal to the version's `base_sha`. Both use the limits of `GET …/scm/diff` (5,000 files or 8 MiB, then `truncated: true`). `against` equal to `version` is an empty interdiff.
22. **CLI output of the diff.** `--patch` sends `format=patch` and prints the patch text as received; `--name-only` sends `patches=false` and prints the JSON answer. Giving both is a usage error. An `--intent @file` that cannot be read is a `client_error` (exit 2) before any request.
23. **A version recorded for a head that was already on the source branch has kind `ref`.** This one rule covers version 1 of a change opened through the REST route (here), the head a restore finds on the source branch and the head start-up reconciliation finds (both package 23, the latter with `created_by.actor_type` `system`). The PRD's kinds name no "recorded onto existing commits"; LAND-FR-080 groups that case with a ref move. Packages 19 and 21 pass `push` and `commit` for their opens.
24. **The creating credential is part of the version record (with package 28).** `Open` and `RecordVersion` take the credential type and id of the caller (`api_key` with its key id, `pat` with its token id, `admin_token`), store them with the version and pass them to every version observer; a version the hub creates (`system`) has none. The version object does not show them. Why: the counting rule of approvals compares credentials, not users, and `created_by` holds only the user.
25. **Fork activity (with package 16).** Opening a change from a fork, and moving such a change to `abandoned` through this package's setter, tells the recorder behind `ForkActivity`; package 23 does the same on restore. A landing is reported by the after-landing hook package 16 registers with package 25, not here.
26. **`ChangeNumbersOfSource` (with package 17).** The fork view of the git server asks which changes have a fork as their source workspace, in any state, to advertise their version refs, which this package writes as `refs/parley/changes/<number>/<version>` in the target workspace, the fork's parent.
27. **Branches are names, revisions are package 20's.** `source.branch` and `target_branch` are plain branch names. A version ref is readable through the source control API by its full ref name, one of the four revision forms of §6.3 that the resolver of package 20 accepts; this package resolves no other caller string as a revision.
28. **Store, HTTP, CLI and tests** follow the project decisions: SQLite through `modernc.org/sqlite` behind `internal/store`; `net/http` with its method-and-pattern router; `github.com/spf13/cobra`; git through the `git` binary in `internal/gitrepo`; the `testing` package with table-driven tests, `net/http/httptest`, and repositories made with `git init` in `t.TempDir()`.
29. **The `auto_land` guard (with packages 23 and 29).** `AutoLandGuard` is declared in `internal/change/collaborators.go` with `RegisterAutoLandGuard`; package 29 registers the one implementation. It is called inside the transaction, before `auto_land` is stored, with the caller (user and credential: type, id and `paths`), the change and the value to store. `Open` calls it when the flag is `true`, after version 1 is recorded, because the guard checks the files of the current version; the `PATCH` of package 23 calls it for every `auto_land` member, before it applies anything. A refusal is returned unchanged and nothing of the request remains. Why: LAND-FR-081 is package 29's rule, and this package cannot depend on it.
30. **One exported read: `ReadChange(workspace, number)` (with packages 27, 29 and 30).** It returns the change record as `GET …/changes/:number` shows it, without scope or access checks, inside the caller's transaction when it has one, and the change-not-found error otherwise. Packages 27, 29 and 30 name no function of their own for this read; this is the one they use.
31. **`Open` takes `reverts` and the kind `revert` (with package 29).** The revert of package 29 opens its change through `Open` with the number of the reverted change and version kind `revert`; `Open` stores `reverts` in the same transaction. `reverted_by` of the original stays package 29's, through the setter. The REST route passes neither.
32. **Observers run after the landing is stored (with packages 27 and 28).** At open and for every version the order is stack linker, landing derivation (the evaluator of package 27 stores `landing`), observers, all in the caller's transaction. Package 28's carry-over observer therefore sees the new evaluation and may ask for another.
33. **Wiring is in `internal/hub/change.go`**: the component, its routes, the ref-updated stage, the `ChangeLinker` and the branch-move observer are composed and registered there.
34. **The session link of a change is recorded here (with packages 15, 19, 21 and 49).** `SessionLinker` of `internal/change` has a check and a record operation. The record is called in the same transaction that opens a change or records a version, whenever that version has a session, with the session id, the workspace, the change number and the version number; package 49 fills it with `session.Record` (link kind `version`), and its error rolls the transaction back. The interfaces of the same name in `internal/gitserver` (package 19) and `internal/scm` (package 21) stay check-only: a push or an API commit becomes a session link only through the version this package records for it. Package 15 records the fork link itself. Why: the version is the one place where every writer of a source branch meets, and it already runs inside one transaction with the ref update.
35. **`OpenCounts` for the metrics (with package 51).** `internal/change` exports `OpenCounts(ctx)`, the number of `draft` and `open` changes per workspace slug. Package 51 registers it in `internal/hub` as the source of `parley_changes_open`; this package reports nothing to a recorder.
36. **`auto_land` has one store and one setter (with packages 23 and 29).** `Open` stores the flag through `SetAutoLand(tx, change, value, caller)` of `internal/change`, after the guard accepted it. This package's function stores the flag alone; package 23 extends the same function to store the setter beside it (user, credential type and the token's `paths` globs) and exposes the accessor `AutoLandSetter`, so a flag set at open and a flag set by `PATCH` have the same stored setter. Package 29 keeps no copy: it registers the guard and reads the accessor.

## Dependencies

- Package 18 `git_push_rules` (draft): the ref writer with its ref-updated stages, in whose commit step this package records versions, and the rule that `refs/parley/*` is hub-owned, under which version refs are written.
- Package 21 `scm_write_api` (draft): the `ChangeLinker` interface this package registers with, the API commit, ref and rebase routes whose ref updates reach the stage, and, through package 20, the diff computation and response shape the diff route reuses.
- Packages 09 `cli_foundation` and 52 `durable_event_log_and_stream` (draft): the CLI helpers and the event writer. Package 12 `workspace_sync_from_origin` (draft): the branch-move observer of sync.
- Through them: packages 02, 03, 04, 06, 07, 08 and 17 (conventions, store, principal, path-scoped tokens, access rule, binding information, git over HTTP).
- External: the `git` command, 2.38 or later.

## Verified External API

- `github.com/spf13/cobra`: `cobra.Command`, `(*cobra.Command).Flags`, `cobra.ExactArgs`. **Unverified**: the module is not in `go.mod` yet and was not read.
- `modernc.org/sqlite`: used only through `internal/store`. **Unverified**.

## Glossary

- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires: after 7 days by default, or 24 hours after all its changes have landed or been abandoned.
- **Change**: The unit of work: a proposal to add the commits of a source branch to a target branch, with a number, a title, an intent, versions and a state.
- **Version**: One head of a change's source branch. Every push, API commit or restack that moves the source creates a new version. Old versions stay fetchable.
- **Intent**: The author's statement of why the change exists.
- **Stack**: Changes linked by `depends_on`, lowest first.
- **Integration branch**: For a carry-patch workspace, the branch the hub rebuilds as upstream plus the ordered patches.
- **Bound token**: A PAT confined to one workspace (and the forks created with it). A token bound to a fork also reaches its own changes, and the awareness and policy reads, on the fork's parent (§3).
- **Path-scoped token**: A PAT whose writes are limited to files matching a list of globs.
- **Session**: An agent session: who ran, on which workspace, with what token usage, and which changes it produced.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
- **Interdiff**: The difference between the diffs of two versions of one change, with rebases factored out.
- **Patch-equivalent**: Said of a version whose diff against its own base is identical to an earlier version's.
