---
spec_id: "14"
spec_name: "workspace_archive_reclone_delete"
title: "Archive, reclone and delete a workspace"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "d42a62622e2cc72c08eb5a973e012abdc52ae2110ab7d84229fc0b33c70ea925"
schema_version: 3
source: "docs/prd/04_workspaces_and_forks.md"
---

# Archive, reclone and delete a workspace

## Intent

The owner of a repository workspace can retire it, revive it, repair its copy and finally remove it. Archiving takes the workspace out of service without losing anything the hub holds; reactivating brings it back; recloning throws the hub's copy away and fetches origin again; deleting removes an archived workspace, its task forks and everything they own in one step or not at all. Each of these offers the hub's branches to origin first where commits could otherwise be lost, and none of them leaves a workspace half changed.

## Goals

- `POST /workspaces/:slug/archive` on a `ready` or `failed` repository workspace answers `200` with `status: "archived"`, keeps the hub's copy, and emits `parley.workspace.archive` with `head_sha`, `origin_push` and the number of forks.
- Before archiving and before recloning, the hub offers every branch to origin as a fast-forward; whatever origin answers, the operation goes on, and the event says `origin_push: ok`, `partial` or `failed`.
- Archive ends every active agent session of the workspace and of its forks with reason `workspace archived`; a failure to end one does not fail the archive.
- `POST /workspaces/:slug/reactivate` on an `archived` workspace answers `200` with `status: "pending"`, and clone work brings it back to `ready` from the kept copy.
- `POST /workspaces/:slug/reclone` on a `ready` or `failed` repository workspace without forks answers `200` with `status: "pending"`, `sync_error: null` and `origin_head_sha: null`; the new copy holds what origin holds and nothing else.
- `DELETE /workspaces/:slug` on an `archived` repository workspace answers `204`; afterwards the workspace, its forks, the tokens bound to them and everything they own are gone, the slug can be registered again, and one `parley.workspace.delete` exists for each fork and one for the parent. When the removal fails the answer is `500` and nothing has changed.
- While one of archive, reclone and delete runs on a workspace, every other request that needs its repository answers `409 workspace_busy`.
- `parley workspace archive|reactivate|reclone|delete` drive the four routes; `reclone` and `delete` send nothing without `--confirm`.

## Non-goals

- The access rule, the binding guard and scope checks as logic: packages 07 workspace_access_rule, 08 workspace_bound_tokens and 06 api_keys_and_access_tokens. The four routes call them and add nothing.
- The state guard that answers `409 workspace_busy`, `workspace_not_ready` and `workspace_archived` on other routes, clone work itself, the check of a kept copy (WS-FR-026) and credential resolution: package 10 workspace_create_and_clone. This package sets and releases the maintenance marker that guard reads, and puts a workspace back to `pending` through package 10's entry point.
- `GET /workspaces` hiding archived workspaces and `GET /workspaces/:slug` (the read half of WS-FR-059): package 11 workspace_read_update_settings. `PATCH` on an archived workspace (WS-FR-034): package 11.
- Refusing git service on an archived workspace (WS-FR-059): package 17 git_smart_http. Refusing writes on it through other routes: the package that owns each route.
- `POST /workspaces/:slug/branches/:name/push`, its error catalogue and `parley.workspace.push`: package 13 workspace_push_to_origin. The safety copy made here uses none of them and emits no push event.
- The rows of WS-FR-061 that concern other routes (sync, push to origin, forge webhooks, carry-patch routes, `org_id` and `settings` on a fork, forking a fork, policy, sandbox rules, event webhooks): the package that owns each route. This package covers archive, reactivate and reclone on a fork.
- Deleting one fork (`DELETE` on a fork slug, WS-FR-078, WS-FR-079), fork expiry, `parley.fork.expire`, `parley fork delete`, and what a fork answers while its parent is archived or in maintenance (WS-FR-066): packages 15 task_fork_creation and 16 task_fork_lifetime. This package registers the `DELETE` route and declares the `ForkRemover` interface that package 16 implements: a fork slug on `DELETE` is handed to its `DeleteFork`, and the forks of a deleted workspace to its `RemoveForksOf`.
- What is removed for each owner of workspace data. This package runs the removal as one step and removes the workspace records; each owning package supplies the remover for its own data: secrets (39), variables (40), changes (22), policy (27), claims (30), patches (31), workspace sandbox rules (48), event webhook subscriptions and their undelivered events (53).
- Ending sessions and the `parley.session.force_closed` event (AUD-FR-009): package 49 agent_sessions_and_usage, which supplies the session closer this package calls.
- Deleting sandboxes before a workspace is deleted and stopping them on archive (SBX-FR-045): package 47 sandbox_lifecycle_and_checkpoints, which supplies the sandbox remover and an archive observer.
- What a change does when its commits are gone after a reclone, or its source fork after a delete: package 22 change_opening_and_versions and package 23 change_lifecycle_and_mergeability.
- The error envelope, the check order and the CLI's output, exit codes and waiting rules (PLAT-FR-010 to PLAT-FR-019, PLAT-FR-090 to PLAT-FR-095): packages 02 http_api_conventions and 09 cli_foundation. This package obeys them.

## Background

The repository holds no code yet; every file named here is new. Source: `docs/prd/04_workspaces_and_forks.md`, WS-FR-058 to WS-FR-065 with the rows of §4.3.4 for archive, reactivate, reclone and delete, the four routes of §4.5, the four commands of §4.6, the four events of §4.8 and the edge cases of §4.10 that name archive and reactivate. WS-FR-080 (one delete event for each fork) is described under WS-FR-065 and is covered here for the delete of a parent.

Package 10 workspace_create_and_clone delivers the workspace record, the state guard with the read side of the maintenance marker, clone work (a workspace in `pending` is its own clone work), credential resolution from workspace secrets, and in `internal/gitrepo/` the operations to open and to remove a copy. This package adds to `internal/workspace/`: the maintenance marker's set and release side (`maintenance.go`), the safety copy (`safetycopy.go`), the collaborators other packages supply (`teardown.go`), and one file for each operation with its handler (`archive.go`, `reactivate.go`, `reclone.go`, `delete.go`), each with its test file beside it. It adds to `internal/gitrepo/` the push of many branches to a remote as fast-forwards (`pushall.go`), and to `internal/cli/workspace.go` the commands `archive`, `reactivate`, `reclone` and `delete`. An end-to-end test in `test/e2e/workspace_retire_test.go` starts a real hub in-process and drives it over HTTP, through the CLI and against a test origin.

## Requirements

All paths are relative to `/api/v1`. Errors are the error envelope of the platform conventions; "`409 has_forks`" means HTTP 409 with `error_type` `has_forks`. The four routes take no body and need the scope `workspaces:write` and write access: the owner and the admin token may call them; a member of the organization gets `403 forbidden`, also on an archived workspace; a caller without access gets `404 not_found`, the same answer as for an unknown slug.

### Exclusive maintenance (§4.3.4, WS-FR-007)

Archive, reclone and delete each put the workspace in exclusive maintenance for the length of the call: the maintenance marker is set after the checks have passed and released when the call ends, whether it succeeded or failed. While the marker is set, a second archive, reactivate, reclone or delete of the same workspace answers `409 workspace_busy`, and so does every route that asks package 10's state guard. Of two concurrent calls on one workspace, exactly one runs and the other answers `409 workspace_busy`. Workspaces do not block each other.

Once the checks have passed, each of the three operations runs to its end when the caller disconnects.

### The safety copy to origin (WS-FR-058, WS-FR-063)

Before an archive and before a reclone the hub offers every branch under `refs/heads/` of its copy to the branch of the same name on origin, with the origin credential resolved from the workspace's secrets. The offer is fast-forward only: it never forces, never deletes a branch on origin and sends no tags. On a carry-patch workspace the integration branch is left out, because the hub rebuilds it; patch branches are offered.

The outcome is one of three values and goes into the event of the operation:

| `origin_push` | Condition |
|---|---|
| `ok` | origin accepted every offered branch (a branch origin already had at that commit counts as accepted), or there was no branch to offer |
| `partial` | origin accepted at least one branch and refused at least one |
| `failed` | origin accepted none of at least one offered branch: every branch was refused, origin could not be reached, the credential was refused or could not be read, or the 2-minute deadline passed before any branch was accepted |

The outcome never changes what the operation does or answers. For every branch origin accepted, the hub records the pushed commit as the origin head of that branch, the ref `refs/remotes/origin/<name>` of its copy. The safety copy has a deadline of 2 minutes; branches not accepted by then count as refused. No credential and no text from origin appears in a response or an event.

### Archive (WS-FR-058 to WS-FR-061)

`POST /workspaces/:slug/archive` applies to a repository workspace in `ready` or `failed`. The first row that applies decides:

| Workspace | Answer |
|---|---|
| a fork | `400 not_supported_on_fork` |
| the maintenance marker is set | `409 workspace_busy` |
| `archived` | `409 workspace_archived` |
| `pending` or `cloning` | `409 workspace_busy` |

On a workspace that passes, the hub makes the safety copy (a `failed` workspace has no copy, so nothing is offered), then in one transaction sets `status: "archived"`, clears `clone_error`, and writes `parley.workspace.archive`. The answer is `200` with the workspace object, `status: "archived"`.

The hub's copy of the repository is not touched: every branch, every ref under `refs/parley/` and every change version it held before the archive is still in it afterwards. Bound tokens, secrets, variables, patches, changes and forks are not touched. An archived workspace is still read with `GET /workspaces/:slug`.

The event `parley.workspace.archive` has the caller as actor, the slug as `workspace`, and metadata `head_sha` (the workspace's `head_sha`, `null` when it has none), `origin_push` and `forks` (the number of forks the workspace has). A refused archive emits no event.

**Sessions.** Inside the archive's transaction the hub asks the session closer to end every active agent session of the workspace and of each of its forks with reason `workspace archived`. When the closer fails, its own writes are undone and the archive still commits and answers `200`.

**Observers.** After the transaction has committed the hub tells each archive observer the slug and the slugs of its forks (package 47 stops sandboxes this way). An observer's failure is logged and changes nothing in the answer.

### Reactivate (WS-FR-062)

`POST /workspaces/:slug/reactivate` applies to an `archived` repository workspace. The first row that applies decides:

| Workspace | Answer |
|---|---|
| a fork | `400 not_supported_on_fork` |
| the maintenance marker is set | `409 workspace_busy` |
| any status other than `archived` | `409 workspace_not_archived` |

On an archived workspace the hub, in one transaction, sets `status: "pending"`, clears `clone_error` and writes `parley.workspace.reactivate` (caller as actor, no metadata); it then wakes the clone workers through package 10's entry point. The answer is `200` with the workspace object, `status: "pending"`. Clone work finds the kept copy and the workspace becomes `ready` without a clone; when the copy is damaged or missing it is cloned fresh from origin, and commits that existed only in the hub are gone (package 10, WS-FR-026).

### Reclone (WS-FR-063)

`POST /workspaces/:slug/reclone` applies to a repository workspace in `ready` or `failed`, checked in this order:

| Workspace | Answer |
|---|---|
| a fork | `400 fork_has_no_origin` |
| the maintenance marker is set | `409 workspace_busy` |
| `archived` | `409 workspace_archived` |
| `pending` or `cloning` | `409 workspace_busy` |
| has at least one fork | `409 has_forks` |

On a workspace that passes, the hub makes the safety copy, then in one transaction sets `status: "pending"`, clears `sync_error` and `origin_head_sha`, marks the existing copy as not belonging to the record any more, and writes `parley.workspace.reclone` with metadata `origin_push`. It then discards the copy on disk, and with it the origin head of every branch (the refs `refs/remotes/origin/<name>`), and wakes the clone workers. The answer is `200` with the workspace object: `status: "pending"`, `sync_error: null`, `origin_head_sha: null`.

When the transaction fails the answer is `500 internal`, the status and the copy are as before, and no event exists. After the clone that follows, the copy holds exactly origin's branches and tags: a branch that existed only in the hub and was not accepted by origin is gone, and so is every ref under `refs/parley/`. Change records are not removed.

### Delete (WS-FR-064, WS-FR-065, WS-FR-080)

`DELETE /workspaces/:slug` on a repository workspace applies only when it is `archived`. The first row that applies decides:

| Workspace | Answer |
|---|---|
| a fork | handed to `DeleteFork` of the fork remover that package 16 registers; `500 internal` when none is registered |
| the maintenance marker is set | `409 workspace_busy` |
| a repository workspace in any status other than `archived` | `409 workspace_not_archived` |

The delete of an archived repository workspace W with forks F1…Fn runs in three stages.

1. **Sandboxes.** The hub asks the sandbox remover to delete the sandboxes of W and of every fork. When it fails the answer is `500 internal` and no workspace record, token or other data has changed.
2. **The removal step, one transaction.** For each fork and then for W, the hub revokes the live tokens bound to the slug through the token revoker, runs every registered remover of owned data (workspace secrets and variables, patches, changes, policy, claims, workspace sandbox rules and event webhook subscriptions with their undelivered events are removed), asks the session closer to end the remaining active sessions with reason `workspace deleted`, removes the workspace record with its settings, and writes one `parley.workspace.delete` event. When any part fails, the whole transaction is rolled back, the answer is `500 internal`, and nothing has changed: W is still `archived`, its forks exist, no token is revoked, no event was written.
3. **Repository data.** Only after the transaction has committed does the hub discard the repository data of W, which holds the branches of its forks. A failure here is logged; the answer is still `204`, and the leftover is never taken for the copy of a later workspace with the same slug (package 10).

The answer is `204` with an empty body. Afterwards `GET /workspaces/<slug>` answers `404 not_found` for W and for each fork, and `POST /workspaces` with W's slug succeeds. Audit records and the event log are kept.

Each `parley.workspace.delete` event has the caller as actor, the deleted slug as `workspace`, and metadata `kind` (`repository` or `fork`), `parent_slug` (`null` for W), `revoked_tokens` (the ids of the tokens that were bound to that workspace and were revoked by this step, sorted ascending, `[]` when none) and, on W's event only, `deleted_forks` (the fork slugs, sorted ascending, `[]` when none).

### CLI (§4.6)

The commands follow the CLI's common rules: the route's response on stdout as one JSON document, a hub error on stderr with exit 1, a usage error with exit 2.

| Command | Request | Flags | Request timeout | stdout |
|---|---|---|---|---|
| `parley workspace archive <slug>` | `POST /workspaces/:slug/archive` | none | 3 minutes | the workspace |
| `parley workspace reactivate <slug>` | `POST /workspaces/:slug/reactivate` | `--wait`, `--timeout`, `--poll-interval` | 30 seconds | the workspace |
| `parley workspace reclone <slug>` | `POST /workspaces/:slug/reclone` | `--confirm` (required), `--wait`, `--timeout`, `--poll-interval` | 3 minutes | the workspace |
| `parley workspace delete <slug>` | `DELETE /workspaces/:slug` | `--confirm` (required) | 30 seconds | `{"status": "deleted", "slug": "<slug>"}` |

`reclone` and `delete` without `--confirm` exit 2 with a `usage_error` on stderr and send no request. With `--wait`, `reactivate` and `reclone` poll `GET /workspaces/:slug` after the `200` as `parley workspace create` does: `ready` prints the final workspace and exits 0; `failed` prints the final workspace, reports `wait_failed` on stderr and exits 1. The slug is sent as one percent-encoded path segment.

### End-to-end flows

1. **Retire and revive.** A developer runs `parley workspace archive demo`; the CLI posts to the hub; `parleyd` offers the branches to origin, archives the workspace and answers `200`; origin now has the commit that existed only in the hub; the developer runs `parley workspace reactivate demo --wait`; `parleyd` sets `pending`, the clone work finds the kept copy, and the command prints the workspace with `status: "ready"` and the same `head_sha` as before the archive.
2. **Delete with forks.** A developer archives a workspace that has a fork and a token bound to the fork, then runs `parley workspace delete demo --confirm`; `parleyd` removes the fork and the workspace in one step and discards the repository data; the command prints `{"status": "deleted", "slug": "demo"}`; two `parley.workspace.delete` events exist; the developer registers `demo` again and gets `201`.
3. **Reclone.** A developer runs `parley workspace reclone demo --confirm --wait` on a workspace that has one branch origin refuses; `parleyd` offers the branches, discards its copy, sets `pending` and clones origin; the command prints the workspace with `status: "ready"`; the refused branch is not in the new copy.

## Design Decisions

1. **Stack (project-wide).** Go; the hub runs the `git` command (2.38 or later) as a subprocess through `internal/gitrepo`, with no git library; HTTP on the standard library `net/http` router; the store is embedded SQLite through `modernc.org/sqlite` behind `internal/store`; the CLI uses `github.com/spf13/cobra`; tests use `testing` with tables and `net/http/httptest`, and repositories made with `git init` in `t.TempDir()`.
2. **Other packages' data is removed through collaborators.** Delete must revoke tokens, close sessions, delete sandboxes and remove data of more than ten other packages, most of them built later. `internal/workspace/teardown.go` therefore defines one collaborator set (`Teardown`) that the hub's composition fills at start, in `internal/hub/workspace.go` for this package's own entries and in each owner's `internal/hub/<area>.go` for the others (package 03 owns process composition in `internal/hub`; nothing is wired in `cmd/parleyd`): a `SandboxRemover` (called before the removal step), a `TokenRevoker` (called inside the removal transaction; it returns the ids of the tokens it revoked), `Remover`s of owned data (called inside the removal transaction with one slug; they return an error only), a `SessionCloser` (called inside the archive and the removal transaction with a reason), `ArchiveObserver`s (called after an archive committed) and a `ForkRemover`. With none registered the operations still work and touch only the workspace records. Why: it keeps the plan's dependencies and makes "one step or nothing" a property of one transaction. This package wires, in `internal/hub/workspace.go`, what its own dependencies provide: `RevokeBoundTo` of package 08 as the token revoker (tokens have no remover of their own) and `secret.RemoveWorkspace(tx, slug)` of package 39 as a remover of owned data. Package 39 cannot depend on this package, so it only exports the function and this package registers it; this package reaches package 39 through package 10. Every other owner registers itself in `Teardown` from its own `internal/hub/<area>.go`. The removers of owned data are: secrets (39, registered here), variables (40), forge deliveries (43), event webhooks (53), sessions (49), sandboxes and sandbox rules (47, 48), changes (22), policy (27), claims (30), and patches, jobs and rerere records (31, 33, 34). Package 47 also registers the sandbox remover and an archive observer (which stops sandboxes), package 49 the session closer and package 16 the fork remover.
3. **Forks: one interface, implemented by package 16.** This package registers `DELETE /workspaces/:slug` for both kinds and declares `ForkRemover` with two methods. `DeleteFork` serves the route for a fork slug; until a fork remover is registered the route answers `500 internal` for a fork. `RemoveForksOf` removes the forks of a workspace inside this package's removal transaction, writes their events and returns their slugs and a function that discards their repository data after the commit. Fork removal thus has one place, package 16, once that package is built; while no fork remover is registered this package removes fork records by the same per-workspace removal it applies to the workspace, so that it can be built and tested without package 16. One registry serves both packages: `RemoveOwned` (token revoker, removers of owned data, session closer) is exported here and package 16 removes a fork's owned data through it, by request, by expiry and with its parent, and keeps no registry of its own. Why: one route serves both kinds, fork deletion has its own rules (WS-FR-078), and a package that stores data per workspace registers one remover and is called for every way a workspace or a fork goes away.
4. **The maintenance marker lives in memory.** It is a per-slug entry in the `parleyd` process, set after the checks and released when the call ends. A hub that stops in the middle of a call starts with no marker. Why: the hub is one process, and each operation changes the store in one transaction, so there is no half-done state to guard after a restart.
5. **Order of the refusals.** Fork first (`400`), then the maintenance marker (`409 workspace_busy`, as package 10's guard checks the marker first), then the status (`409`), then `has_forks`. Reactivate and delete answer `409 workspace_not_archived` for every status other than `archived`, including `pending` and `cloning`; `409 workspace_busy` is answered by them only while the marker is set. Why: the PRD lists `workspace_busy` and `workspace_not_archived` for these routes without an order; the check order of the platform puts validation before state.
6. **Meaning of `origin_push`.** The PRD names the three values and not their conditions; the table under "The safety copy to origin" defines them. No branch to offer is `ok`, because nothing was refused.
7. **The safety copy has one 2-minute deadline for all branches.** The PRD gives none; 2 minutes is the push-to-origin deadline (WS-FR-054), and the CLI's 3-minute timeout for archive and reclone then always receives the hub's answer.
8. **The safety copy records origin heads (the refs `refs/remotes/origin/<name>`, through `SetOriginHead` of `internal/gitrepo`) and emits no `parley.workspace.push`.** WS-FR-056 counts pushes of the push route; the outcome of the safety copy is in the archive or reclone event. Recording the accepted heads keeps a later forced push's lease truthful.
9. **Archive commits even when the session closer fails.** WS-FR-060 says a failure to close a session does not fail the archive; AUD-FR-009 puts the closing in the workspace's transaction. The closer runs inside the archive's transaction under a savepoint: its failure undoes only its own writes.
10. **Reclone is safe against a crash.** The transaction that sets `pending` also replaces the record's instance id (package 10, decision 4), so a copy that survives a crash between the commit and the discard is not valid for the record and is cloned fresh. `head_sha`, `default_branch` and `last_sync_at` are left as they are until the clone completes; the PRD names only the fields that are cleared.
11. **The three operations run to completion when the caller disconnects.** The PRD says so for sync and push; an archive or delete that stopped half-way at a closed connection would leave the caller unable to know the state.
12. **A failing sandbox remover stops the delete with `500 internal`.** The PRD says sandboxes are deleted first and nothing about a failure. Sandboxes that were already deleted stay deleted; no workspace data has changed, and the delete can be repeated.
13. **A failure to discard repository data after the removal committed is logged, and the answer stays `204`.** The workspace no longer exists; a `500` would say nothing was deleted.
14. **A body sent to the four routes is ignored** once it passes the platform's content-type and size rules; the PRD says "No body".
15. **`--wait` polls `GET /workspaces/:slug`,** which package 11 delivers, as `parley workspace create --wait` does (package 10, decision 15); package 11 is a dependency.
16. **`deleted_forks` and the fork events are ordered by slug ascending;** the parent's event is written last.

17. **Origin heads are refs.** They are `refs/remotes/origin/<name>` of the copy (packages 10 and 12), not rows. A reclone therefore clears them by discarding the copy, and a delete removes them with the repository data; the transaction clears only `origin_head_sha`.
18. **One test origin.** The safety copy is tested against `internal/gitrepo/gitrepotest` of package 10 in the push mode package 13 adds; this package adds no helper and no mode.

## Dependencies

- **10 workspace_create_and_clone** (draft): the workspace record and its store, the state guard and the read side of the maintenance marker, the entry point that wakes clone work for a `pending` workspace, credential resolution for origin, the copy's path, opening and removing a copy in `internal/gitrepo`, and the test origin helper.
- Through it: 07 workspace_access_rule (caller identity and access levels), 39 secrets (reading `GIT_PAT`, `GIT_USERNAME`, `GIT_PASSWORD`; `secret.RemoveWorkspace`, registered here as a remover of owned data), 52 durable_event_log_and_stream (writing an event in the transaction of its state change), 02 http_api_conventions (route registration and the error envelope), 09 cli_foundation (the CLI's client, output and waiting).
- **11 workspace_read_update_settings** (draft): `GET /workspaces/:slug`, which `--wait` and the end-to-end flows read.
- **08 workspace_bound_tokens** (draft): `RevokeBoundTo`, wired here as the token revoker.
- **13 workspace_push_to_origin** (draft): the push mode of the test origin helper.
- Registered at start by their owners from their own `internal/hub/<area>.go`, not required to build or test this package: the session closer and remover of 49, the sandbox remover, archive observer and remover of 47, the fork remover of 16, and the removers of 22, 27, 30, 31, 33, 34, 40, 43, 48 and 53.
- External: the `git` command, 2.38 or later, on the hub's `PATH`.

## Verified External API

- `github.com/spf13/cobra` — **unverified** (not yet in `go.mod`): `cobra.Command` with `Use`, `Short`, `Args`, `RunE func(cmd *cobra.Command, args []string) error`; `(*Command).AddCommand(cmds ...*Command)`; `(*Command).Flags() *pflag.FlagSet`; `cobra.ExactArgs(n int) cobra.PositionalArgs`.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Origin**: A registered workspace's remote (`git_url`).
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires: after 7 days by default, or 24 hours after all its changes have landed or been abandoned.
- **Carry-patch workspace**: A workspace that tracks an upstream project and keeps an ordered list of patches on top of it.
- **Integration branch**: For a carry-patch workspace, the branch the hub rebuilds as upstream plus the ordered patches.
- **Bound token**: A PAT confined to one workspace (and the forks created with it). A token bound to a fork also reaches its own changes, and the awareness and policy reads, on the fork's parent (§3).
- **Sandbox**: An isolated environment in which an agent runs, attached to a workspace (normally a task fork) and started with it checked out. It runs on OpenShell; the hub controls it (§13).
- **Session**: An agent session: who ran, on which workspace, with what token usage, and which changes it produced.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
- **Origin head**: For a branch of a repository workspace, the last head the hub saw on origin.
- **Clone work**: The durable instruction to make or re-check the hub's copy of a workspace; a workspace in `pending` or `cloning` has clone work outstanding.
- **Maintenance marker**: The record that a reclone, archive or delete is running on a workspace.
- **Safety copy**: The fast-forward offer of every branch to origin that the hub makes before an archive and before a reclone.
