---
spec_id: "12"
spec_name: "workspace_sync_from_origin"
title: "Bring a workspace up to date with its origin"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "b4ec0ac7e53c4ab56d2b4a4e6bbec33985726e34dbfcbb6b2aa85ced44e9dba5"
schema_version: 3
source: "docs/prd/04_workspaces_and_forks.md"
---

# Bring a workspace up to date with its origin

## Intent

A workspace is the hub's copy of a repository whose origin keeps moving. An owner needs one call that brings origin's new commits and tags into the hub, tells them for every branch what happened, and never destroys work that exists only in the hub unless they ask for exactly that. Sync is that call: it fast-forwards what can be fast-forwarded, reports branches that are ahead or diverged without failing, and leaves the workspace in a known state whether it completes or cannot reach origin.

## Goals

- `POST /api/v1/workspaces/:slug/sync` fetches origin with the workspace's origin credential and answers `200` with the workspace object plus `branches`, one entry per branch on origin, ordered by name, each with one of six results: `created`, `up_to_date`, `fast_forward`, `ahead`, `diverged`, `reset`.
- A diverged branch is left untouched and does not fail the call; `reset_to_origin=true` moves diverged branches, and only those, to origin's head.
- A protected branch moves under sync by `created`, `fast_forward` or `reset`, and being `ahead` is reported as a normal state.
- Every branch move is a compare-and-swap, so a sync running beside a landing, a push or another sync loses no update and never answers `409` because of the other request.
- A completed sync records `last_sync_at`, clears `sync_error`, updates `head_sha` and `origin_head_sha` and emits one `parley.workspace.sync` event; a sync that cannot fetch moves nothing, sets `sync_error`, answers `502 upstream_error` and emits nothing.
- `parley workspace sync <slug>` runs the call from the shell, with `--reset-to-origin`, `--fail-on-diverged` (exit 3 when a branch diverged) and `--wait` for a rebuild the answer names.

## Non-goals

- Creating a workspace, cloning it, the status state machine, resolving and storing the origin credential, and the workspace record's sync fields as columns: package 10 `workspace_create_and_clone`. This package reads the credential through that package's resolver and writes the sync fields.
- The workspace response object, the settings document and changing `sync_enabled`: package 11 `workspace_read_update_settings`. This package reads the effective value of `sync_enabled` and returns the workspace object that package defines.
- Pushing a branch to origin, force-with-lease and the use of the recorded origin heads as a lease: package 13 `workspace_push_to_origin`.
- Archive, reclone (which also clears `sync_error`) and delete: package 14 `workspace_archive_reclone_delete`.
- Task forks: packages 15 `task_fork_creation` and 16 `task_fork_lifetime`. Here a fork is only refused with `400 fork_has_no_origin`.
- Which branches are protected, glob matching and the push rules that refuse other ways of moving a protected branch: packages 18 `git_push_rules` and 27 `landing_policy`. Sync does not consult the push rules.
- Recording a new version of an open change whose source branch sync moved (WS-FR-047, version kind `sync`): package 22 `change_opening_and_versions`. This package reports every branch move to registered observers; that package registers one.
- The carry-patch part of sync (upstream fetch, origin refresh of patch branches, merge detection, automatic rebuild, the extra response fields of §10, CP-FR-067 to CP-FR-076): package 36 `carry_patch_sync`. This package leaves the integration branch and the branches a registered provider names out of its per-branch pass, and declares the two-stage extension point that package calls its part through.
- Rebuild jobs and their routes: package 33 `rebuild_jobs`. The CLI here only polls `GET /workspaces/:slug/rebuilds/:id`.
- The event log and delivery of `parley.workspace.sync`: package 52 `durable_event_log_and_stream`. This package emits through it.
- The error envelope, the order of checks before validation, authentication, scopes, the binding guard and the access rule: packages 02, 04, 06, 07 and 08. The CLI's connection settings, output, exit codes and `--wait` mechanics: package 09 `cli_foundation`.
- A scheduled or automatic sync. Sync runs only when a caller asks for it.

## Background

The repository holds no code yet; every file named here is new. The source is `docs/prd/04_workspaces_and_forks.md`: WS-FR-036 to WS-FR-048, the route entry `POST /workspaces/:slug/sync` in §4.5, the command `parley workspace sync` in §4.6, the `parley.workspace.sync` row of §4.8, and the sync entries of §4.10.

Package 10 gives this package a repository workspace: a record in the transactional store, a bare repository on disk whose origin is `git_url`, the origin credential resolved from the workspace's secrets (§4.3.5), and the fields `head_sha`, `origin_head_sha`, `last_sync_at` and `sync_error`. Sync is the second operation, after clone, that talks to origin.

The code lives in `internal/workspace/`: `sync.go` (the sync operation and its per-branch pass), `sync_handler.go` (the route), each with its test file beside it. Fetching, ancestry tests and compare-and-swap ref updates are added to `internal/gitrepo/` (`fetch.go`, `refs.go`). The command is added to `internal/cli/workspace.go`. End-to-end tests are in `test/e2e/workspace_sync_test.go`.

## Requirements

### Who may sync, and when

`POST /api/v1/workspaces/:slug/sync` (§4.5). The route needs scope `workspaces:write` for a PAT and write access to the workspace (WS-FR-001 to WS-FR-003): a PAT without the scope gets `403` with `error_type` `insufficient_scope`; an org member gets `403 forbidden`; a caller with no access gets `404 not_found`, the same as for an unknown slug. The route takes no body.

The query parameter `reset_to_origin` takes `true` or `false`; absent means `false`. Any other value is `400` with `error_type` `invalid_request`.

After access and validation the route checks, in this order, and the first failure decides the answer (WS-FR-036, WS-FR-007):

| Check | Answer |
|---|---|
| The workspace is a task fork | `400`, `error_type` `fork_has_no_origin` |
| Status `pending` or `cloning`, or a reclone, archive or delete is running | `409`, `error_type` `workspace_busy` |
| Status `failed` | `409`, `error_type` `workspace_not_ready` |
| Status `archived` | `409`, `error_type` `workspace_archived` |
| The setting `sync_enabled` is `false` | `400`, `error_type` `sync_disabled` |

A refused request fetches nothing, moves nothing, changes no workspace field and emits no event.

### Fetching origin

WS-FR-037, §4.3.5. Sync fetches every branch and tag of origin with the origin credential. The credential is read from the workspace's secrets at the moment of the fetch, so a secret changed between two syncs is used by the second. The fetch has a deadline of 2 minutes.

After a successful fetch the hub holds origin's head for every branch that exists on origin (the origin head of that branch), kept as `refs/remotes/origin/<name>`. A branch that no longer exists on origin has no origin head any more. This holds for every branch, whatever its result.

Tags: a tag that origin has and the hub does not is created at origin's value. A tag that exists in the hub is never moved, also when origin now has another value for it, and a tag that exists only in the hub is kept.

### Per-branch results

WS-FR-038, WS-FR-040, WS-FR-042. For each branch that exists on origin, sync compares the hub's head with origin's head and reports exactly one result:

| Result | Condition | Effect in the hub |
|---|---|---|
| `created` | The hub has no such branch. | The branch is created at origin's head. |
| `up_to_date` | Both heads are equal. | None. |
| `fast_forward` | The hub's head is an ancestor of origin's. | The branch moves to origin's head. |
| `ahead` | Origin's head is an ancestor of the hub's. | None. |
| `diverged` | Each side has commits the other lacks. | None. |
| `reset` | The branch diverged and `reset_to_origin=true` was given. | The branch moves to origin's head. The hub's commits on it are no longer on the branch. |

`diverged` does not fail the call, and the other branches are still processed. With `reset_to_origin=true` only diverged branches are reset; a branch that is `ahead` keeps its commits and is still reported `ahead` (§4.10).

Branches that exist only in the hub are not touched and not reported. A branch that was deleted on origin is not deleted in the hub.

The response is `200` with the workspace object, re-read after the sync, and one more member, `branches`: one entry per branch on origin, ordered by name, each `{"name", "result", "local_sha", "origin_sha"}`. `local_sha` is the hub's head of that branch after the sync and `origin_sha` is origin's head; both are full 40-character commit ids. An origin with no branches gives `branches: []`.

### Protected branches

WS-FR-039, WS-FR-041. Sync is one of the three ways a protected branch may move. It moves one only by `created`, `fast_forward` or `reset`, and it does so whatever the value of `protect.owner_push` is. A protected branch with landed or directly pushed commits that nobody has pushed to origin is `ahead`; that is its normal state, the call answers `200`, and `sync_error` stays `null`. Resetting a protected branch discards landed and directly pushed commits from the branch; change records are not touched (§4.10).

### Concurrency

WS-FR-043, WS-FR-008, §4.10. Each branch move is a compare-and-swap that expects the head sync compared against. If the branch moved in the hub in between, the swap fails, nothing is overwritten, and sync evaluates that branch again against its new head; the reported result is the one of the last evaluation. A branch that a landing moved first is therefore usually reported `ahead`. A branch is evaluated at most 5 times in one sync; if the fifth swap is also lost, the call answers `500` with `error_type` `internal` and a message that names the branch, the sync fields are not written and no event is emitted; moves already made stay.

Two concurrent syncs of one workspace both answer `200`, and afterwards every branch that could be fast-forwarded is at origin's head. Sync is never answered with `409 workspace_busy` because a push, a landing or another sync is running. Sync never changes the workspace's `status`.

### Completion

WS-FR-044, WS-FR-046, WS-FR-047, §4.8. A sync that completes, also when branches are `ahead` or `diverged`:

- sets `last_sync_at` to the completion time and `sync_error` to `null`;
- sets `head_sha` to the hub's head of the default branch and `origin_head_sha` to origin's head of the default branch (`null` when origin has no such branch);
- emits exactly one `parley.workspace.sync` event whose actor is the caller, with metadata `created`, `fast_forward`, `ahead`, `diverged` and `reset` (each a list of branch names, sorted) and `updated`, a list of `{ref, before, after}` with one entry per ref the sync created or moved.

A sync runs to completion when the caller disconnects: the moves, the fields and the event are the same as if the caller had waited.

Every branch that sync creates or moves is reported once, after the move, to the registered branch-move observers with the workspace slug, the branch name, the heads before and after and the cause `sync`. This is how an open change whose source branch moved gets its new version (package 22). An observer's failure does not undo the move and does not fail the sync.

### Failure to fetch

WS-FR-045. When the fetch fails (the credential is refused, the secrets cannot be read, the network fails, origin refuses, or the 2-minute deadline passes), sync moves no branch and creates no tag, leaves the origin heads, `last_sync_at`, `head_sha` and `origin_head_sha` as they were, sets `sync_error` to the failure text and answers `502` with `error_type` `upstream_error`. No event is emitted. A failure to read the secrets never falls back to an anonymous fetch (§4.3.5). `sync_error` and the response message contain no credential and no URL userinfo, and are at most 1024 bytes (PLAT-FR-098). The next sync that completes clears `sync_error`.

### Carry-patch workspaces

WS-FR-048. On a workspace with `workspace_mode` `carry_patch`, the per-branch pass leaves out the integration branch and the branches of registered patches: they are not compared, not moved and not listed in `branches`, although origin's heads for them are still fetched. The integration branch is the workspace's `integration_branch`; the patch branches are named by a provider that package 36 registers. If the provider fails, the sync stops before the per-branch pass with `500` and `error_type` `internal`: nothing is moved, no sync field is written and no event is emitted. Everything else a carry-patch sync does is defined in §10 and is not part of this package.

### The sync extension point

The carry-patch part of sync (CP-FR-067 to CP-FR-076) belongs to package 36, which is built after this package. Sync therefore calls a registered sync extension, and only for a workspace whose `workspace_mode` is `carry_patch`:

- **First stage**, after the fetch of origin has succeeded and before any tag or branch is created or moved. An error ends the sync as a failed fetch does: `502 upstream_error` with the scrubbed error text, `sync_error` set to it, nothing moved, no event.
- **Second stage**, after the per-branch pass and before the completion is recorded. It receives the per-branch results and may return extra members for the answer and a value for `upstream_head_sha`. The extra members are added to the top level of the `200` answer (a member of the workspace object or `branches` is never replaced) and the `upstream_head_sha` value is written in the same transaction as `last_sync_at`. An error answers `500 internal` with the scrubbed error text; the completion is not recorded, no event is emitted, and branch moves already made stay.

Both stages run on the sync's own context, so a caller who disconnects does not stop them. With no extension registered, and for a `standard` workspace, sync behaves as described above. The text of a failed origin fetch begins with `origin fetch failed: `, so that it can be told from a failure the first stage reports.

### The command

`parley workspace sync <slug>` (§4.6), flags `--reset-to-origin`, `--fail-on-diverged`, `--wait`, `--timeout`, `--poll-interval`.

- It sends `POST /workspaces/<slug>/sync`, the slug as one percent-encoded path segment, with a request timeout of 3 minutes instead of 30 seconds. `--reset-to-origin` adds `reset_to_origin=true`.
- It prints one document, the sync answer, on stdout and exits 0. A hub error is printed on stderr as received and the command exits 1. The command takes exactly one argument; any other count is a usage error (exit 2) and sends nothing.
- `--fail-on-diverged`: after printing, if any entry of `branches` has `result` `diverged`, or the carry-patch member `patches_diverged` is present and not empty, the command names the diverged branches on stderr and exits 3. Without the flag, divergence does not change the exit code.
- `--wait`: if the answer has a non-null `rebuild_job_id`, the command itself polls the REST route `GET /workspaces/<slug>/rebuilds/<id>` (package 33's route; no CLI code of that package is used) until the job is `completed`, `dead_letter` or `cancelled` (`failed` means the job waits to retry, so polling continues), then prints the sync answer with one more member, `rebuild`, holding the final job record. `completed` exits 0; `dead_letter` and `cancelled` report `wait_failed` and exit 1; passing `--timeout` prints nothing on stdout, reports `wait_timeout` and exits 1. A failed wait (exit 1) takes precedence over exit 3. When the answer names no rebuild job, `--wait` changes nothing.

### End-to-end flows

1. A developer owns a workspace whose origin has gained a commit on `release/1.x`, a new branch `exp` and a new tag, while `main` in the hub has a landed commit origin lacks. The developer runs `parley workspace sync my-project`; the parley CLI posts the sync; `parleyd` fetches origin, fast-forwards `release/1.x`, creates `exp` and the tag, reports `main` as `ahead`, records the sync fields and emits `parley.workspace.sync`; the CLI prints the answer and exits 0. Then `exp` moves on both sides; `parley workspace sync my-project --fail-on-diverged` prints the answer with `exp` as `diverged` and exits 3; with `--reset-to-origin` the branch is reported `reset` and stands at origin's head.
2. An agent with a `workspaces:write` token syncs a workspace whose origin has become unreachable. `parleyd` answers `502 upstream_error`, moves nothing and sets `sync_error`. Origin comes back; the agent syncs again; `parleyd` answers `200`, `sync_error` is `null` and `last_sync_at` is set.

## Design Decisions

1. **Profile: full.** Ten requirements and two end-to-end flows across the CLI, the route, the workspace component, git and the store.
2. **Order of the state checks: fork, then status, then `sync_disabled`.** The PRD lists the errors without an order. WS-FR-036 names "repository", "ready" and "`sync_enabled`" in that order, and the push route of §4.5 also checks status before its other refusals. An archived workspace with sync disabled therefore answers `409 workspace_archived`.
3. **`reset_to_origin` accepts `true` and `false`; any other value is `400 invalid_request`.** The PRD names only `true`. Refusing a misspelt value is safer than silently not resetting, or resetting.
4. **The fetch has a 2-minute deadline**, the same as the push-to-origin deadline of §4.9. The PRD gives none; a fetch without one could hold a request for ever, and the CLI's 3-minute request timeout leaves room for it.
5. **A failure to read the workspace's secrets is a fetch failure**: `502 upstream_error` with `sync_error` set. §4.3.5 says the operation fails and WS-FR-045 and CP-FR-067 class a credential failure as `upstream_error`.
6. **A branch is evaluated at most 5 times per sync; then `500 internal` naming the branch.** WS-FR-043 asks for re-evaluation and gives no bound. A bound keeps a sync from looping under constant writes, and `500` is what CP-FR-071 answers for a lost compare-and-swap in the carry-patch part. The caller syncs again.
7. **Origin heads are the refs `refs/remotes/origin/<name>`**, as §10.3 describes them ("origin branches as of the last origin fetch"). The fetch updates them all together or not at all and removes those of branches origin no longer has, so that a later forced push (package 13) uses "no such branch" as its lease.
8. **Tags that exist only in the hub are kept, and a tag whose value differs on origin is left without an error or a report.** WS-FR-037 says an existing tag is never moved and is silent on the rest; this mirrors WS-FR-042 for branches.
9. **Event metadata.** The five name lists are sorted by name and `up_to_date` branches appear in none. `updated` uses full ref names (`refs/heads/<name>`, `refs/tags/<name>`), includes created tags, and has `before: null` for a created ref, as `parley.git.push` does (§14).
10. **A failed sync leaves `last_sync_at`, `head_sha` and `origin_head_sha` unchanged.** WS-FR-045 says only that it moves nothing and sets `sync_error`; `last_sync_at` is "the time of the last successful sync" (§4.3.1).
11. **Branch-move observers.** WS-FR-047 belongs to package 22, which this package does not depend on. `internal/workspace` declares an observer interface; `parleyd` registers observers; with none registered sync behaves the same. An observer's failure is logged and does not fail the sync, because the branch has already moved.
12. **Excluded-branch provider.** WS-FR-048 leaves out patch branches, which packages 31 and 36 define. `internal/workspace` declares a provider interface; with none registered only the integration branch is left out. A provider that fails stops the sync with `500 internal` before any branch is compared, because moving a patch branch by mistake would be worse than a sync the caller repeats.
13. **Sync does not call the push rules.** Protection is enforced where pushes, API writes and ref moves enter (package 18); sync is exempt by WS-FR-041, so it needs no knowledge of which branches are protected.
14. **Project decisions relied on:** Go; git operations run the `git` binary (2.38 or later) as a subprocess through `internal/gitrepo`, with terminal prompts disabled; the store is embedded SQLite behind `internal/store`; HTTP is `net/http` with its method-and-pattern router; the CLI uses `github.com/spf13/cobra`; tests use the standard `testing` package, table-driven, with `net/http/httptest` and repositories made by `git init` in `t.TempDir()`.

15. **Sync extension point (package 36).** Package 36 needs to run inside sync at two places and must not edit this package's code, and this package cannot depend on it. `internal/workspace/sync_extension.go` declares `SyncExtension` (`AfterFetch`, `BeforeCompletion`) and `RegisterSyncExtension`; package 36 implements and registers it. The first stage fails the sync as a fetch failure (`502`), the second as an internal error (`500`), which are the answers package 36 specifies for its upstream fetch and its patch refresh. The branch-move observers and the excluded-branch provider stay separate registrations (`RegisterBranchMoveObserver`, `RegisterExcludedBranchProvider`); `NotifyBranchMove` is exported so that the extension reports its own moves to the same observers.
16. **Settings and the workspace object come from packages 10 and 11.** `sync_enabled` is read with `EffectiveSettings` of package 10's settings registry; its key is registered by package 11, which is therefore a dependency, as are the workspace object and `GET /workspaces/:slug` that the end-to-end flows read.
17. **Origin heads are shared with packages 10, 13 and 14.** The refs `refs/remotes/origin/<name>` of decision 7 are written first by the clone of package 10, read as the lease by package 13 and written by the pushes of packages 13 and 14, all through `internal/gitrepo`.
18. **Composition.** The route and its collaborators are wired on the composition value of `internal/hub` (package 03).
19. **`--wait` polls the rebuild job itself (package 33).** This package does not depend on package 33, which owns rebuild jobs, the route `GET /workspaces/:slug/rebuilds/:id` and a CLI function that waits for a rebuild job. `parley workspace sync --wait` therefore does not call that function: the command polls the REST route itself with the CLI foundation's client and shared wait (package 09), giving it the terminal states `completed` (successful), `dead_letter` and `cancelled` (unsuccessful); `failed`, `queued` and `running` continue the wait. Package 33 states the same split. Until package 33 is built the route answers `404` and the wait ends with that hub error; this package tests the wait against a scripted `httptest` server.
20. **The extension point as package 36 registers it.** Checked against package 36: an `AfterFetch` error answers `502 upstream_error` with `sync_error` set; `BeforeCompletion` returns the extra answer members and the `upstream_head_sha` value written in the completion transaction, and its error answers `500 internal`; the text of a failed origin fetch begins with `origin fetch failed: ` and so contains the word `origin`, which package 36 requires; `NotifyBranchMove` is exported and package 36 calls it for the moves of its origin refresh, with cause `sync`.

## Dependencies

- Package 10 `workspace_create_and_clone` (draft): the workspace record and its sync fields, the bare repository and its origin, the origin credential resolver, the state preconditions of WS-FR-007, and `internal/cli/workspace.go`.
- Package 11 `workspace_read_update_settings` (draft): the setting key `sync_enabled`, the workspace object and `GET /workspaces/:slug`.
- Through them: package 07 `workspace_access_rule` (write access), package 39 `secrets` (the credential's storage), package 52 `durable_event_log_and_stream` (emitting `parley.workspace.sync`), and packages 02 and 09 (the REST and CLI conventions).
- External system: the `git` command, 2.38 or later, on the server.

## Verified External API

- `github.com/spf13/cobra`: **unverified** (not read; no code or module cache in the repository). Assumed: `cobra.Command` with `Use`, `Args`, `RunE`; `(*Command).Flags() *pflag.FlagSet`; `cobra.ExactArgs(n int) cobra.PositionalArgs`.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Origin**: A registered workspace's remote (`git_url`).
- **Origin head**: For one branch of a repository workspace, the last head the hub saw on origin. Sync and a successful push to origin update it (§4.3.1).
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It has no origin.
- **Protected branch**: A branch that moves only by landing a change, by sync from origin, or by a direct fast-forward push from the workspace's owner or the admin token (§8). Nobody can force-push or delete it. By default the default branch.
- **Direct push**: A fast-forward update of a protected branch by the workspace's owner or the admin token, over git or the source control API, without a change.
- **Change**: The unit of work: a proposal to add the commits of a source branch to a target branch, with a number, a title, an intent, versions and a state.
- **Version**: One head of a change's source branch.
- **Landing**: Adding a change's commits to its target branch: a server-side rebase followed by a compare-and-swap of the target ref.
- **Carry-patch workspace**: A workspace that tracks an upstream project and keeps an ordered list of patches on top of it.
- **Integration branch**: For a carry-patch workspace, the branch the hub rebuilds as upstream plus the ordered patches.
- **Patch**: A carry-patch entry: a branch with a position in the patch order and a status.
- **Rebuild**: A durable job that recreates the integration branch.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number.
