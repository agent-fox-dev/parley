---
spec_id: "31"
spec_name: "carry_patch_setup_and_patches"
title: "Set up a carry-patch workspace and register patches"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "13a5c8600aafef905a0616d3da353138ef3f4c707c4b6551d470c0b359797081"
schema_version: 3
source: "docs/prd/10_carry_patch.md"
---

# Set up a carry-patch workspace and register patches

## Intent

A team that maintains its own variant of a repository it does not control needs the hub to track both its own copy and the upstream, and to know which branches are its patches and in which order they apply. This package makes a freshly cloned carry-patch workspace usable (upstream fetched, integration branch present, remembered resolutions on) and lets developers and agents register patch branches, one at a time or as an atomic batch, and read the patch list back.

## Goals

- After every fresh clone of a carry-patch workspace and before it becomes `ready`, the hub has fetched upstream into `refs/remotes/upstream/*`, turned on remembered resolutions and created the integration branch at the upstream base; when upstream cannot be fetched the workspace is still `ready` and the integration branch sits at the head of origin's default branch.
- A carry-patch workspace created without `integration_branch` uses `deploy`.
- Upstream credentials are read from the workspace's secrets on every use, in a fixed order, with the origin credentials as fallback.
- `POST /workspaces/:slug/patches` registers one patch or an all-or-nothing batch, finds or creates the patch branch, places each patch at its position and emits one `parley.patch.create` per patch.
- `GET /workspaces/:slug/patches` and `GET /workspaces/:slug/patches/:id` return patches with every field present.
- Every route of this package refuses a workspace that is not in carry-patch mode with `400 workspace_mode_mismatch`, and write routes refuse a workspace whose status does not allow the write.
- `parley patch add`, `parley patch list` and `parley patch show` call these routes.

## Non-goals

- Validation and storage of `workspace_mode`, `upstream_url` and `integration_branch` in `POST /workspaces`, the clone itself, the post-clone step registry and `parley workspace create`: package 10 workspace_create_and_clone.
- The settings keys (`patch_branch_source` and the other carry-patch keys), their defaults and validation, and the refusal to change the three carry-patch fields by `PATCH /workspaces/:slug`: package 11 workspace_read_update_settings. This package only reads `patch_branch_source`.
- Patch update, soft delete, restore, reorder and reset to origin (CP-FR-019 to CP-FR-033): package 32 patch_maintenance.
- The durable job model and rebuild submit, list, get, cancel and requeue: package 33 rebuild_jobs. The rebuild algorithm, including its own upstream fetch and its repeat of a missing set-up step: package 34 rebuild_execution.
- Rollback and preview: package 35 rebuild_rollback_and_preview.
- The carry-patch part of sync (upstream fetch at sync, `upstream_head_sha`, origin refresh, merge detection, the repeat of a missing set-up step, the excluded-branch provider): package 36 carry_patch_sync.
- Refusing client writes to the integration branch, to patch branches and to `refs/parley/replaced/*`, forwarding, mirroring and rebuild triggers: package 37 carry_patch_push_control (with package 18 git_push_rules).
- Remembered-resolution routes, the patch-status summary, the git safety rules as a whole and the purge of patches deleted for 7 days: package 38 carry_patch_rerere_and_maintenance.
- The error envelope, check order, list paging and `Idempotency-Key` handling (package 02), the workspace access rule (package 07), the CLI's client, output and exit codes (package 09), the event log (package 52): used, not specified here.

## Background

The repository holds no code yet; every file named here is new. Package 10 delivers the workspace record (with `workspace_mode`, `upstream_url`, `integration_branch` stored as given), the state guard for routes that need the repository, the clone, a registry of post-clone steps keyed by workspace mode, the origin credential resolver and `internal/gitrepo` with the test origin helper `internal/gitrepo/gitrepotest/`. Package 12 adds the origin fetch and the compare-and-swap update of a ref to `internal/gitrepo`. This package adds `internal/carrypatch/` (set-up, upstream credentials, upstream base, the patch store, the patch routes, the guard every carry-patch route uses) and `internal/cli/patch.go`.

The source is §10 of the product PRD (`docs/prd/10_carry_patch.md`): CP-FR-001 to CP-FR-018, the data model of §10.3.1 to §10.3.3, the routes of §10.5 and the commands of §10.6 that belong to them.

## Requirements

### Integration branch name (CP-FR-002, CP-FR-005)

A carry-patch workspace's integration branch is the stored `integration_branch`, or `deploy` when none was given at creation. The set-up after the first clone writes `deploy` into the workspace record, so that the workspace object shows it from then on. Everything in this package that needs the name uses the stored value or, while it is still `null`, `deploy`.

### Upstream credentials (CP-FR-003)

Upstream credentials are workspace secrets. Each time the hub contacts upstream it reads them and uses the first that applies:

1. `UPSTREAM_GIT_PAT` set and not empty: HTTP Basic authentication, user name `x-token-auth`, the token as password.
2. `UPSTREAM_GIT_USERNAME` and `UPSTREAM_GIT_PASSWORD` both set and not empty.
3. The origin credentials of the workspace (`GIT_PAT`, else `GIT_USERNAME` with `GIT_PASSWORD`).
4. No credentials.

A user name without a password is skipped. Nothing is cached, so a rotated secret takes effect at the next use without a restart. When the secrets cannot be read, the resolution fails; the hub never falls back to an anonymous fetch.

### Upstream fetch and upstream base (§10.3.1, §10.3.2)

An upstream fetch brings every branch of `upstream_url` into `refs/remotes/upstream/*` of the hub's bare copy, removes upstream refs that upstream no longer has, and fetches no tags. It also records the upstream's default branch. A single upstream fetch is limited to 10 minutes; past the limit the git process is stopped and the fetch has failed. The credential is handed to git outside the URL and the arguments, and no error text or log line of a fetch contains it (URL user information is replaced by `***@`).

The upstream base is the head of the upstream's default branch as of the last upstream fetch. If the default branch is unknown, it is the upstream branch with the same name as the workspace's `default_branch`. If neither resolves there is no upstream base.

### Set-up after a fresh clone (CP-FR-004, CP-FR-005)

After every fresh clone of a carry-patch workspace (the first clone and each reclone) and before the workspace becomes `ready`, the hub:

1. records `deploy` as `integration_branch` when the record has none;
2. fetches upstream with the upstream credentials;
3. turns on remembered resolutions in the workspace's copy;
4. sets the integration branch to the upstream base, or to the head of origin's default branch (the workspace's `head_sha`) when the upstream fetch failed or there is no upstream base.

Each step is attempted even when an earlier one failed. A failure is logged and never prevents `ready`; the workspace's `clone_error` stays `null`. The integration branch is the hub's alone: when origin has a branch with the integration branch's name, the clone's copy of it is replaced by step 4, and the hub's integration branch does not follow origin's. The set-up does not write `upstream_head_sha`; the first sync does (package 36). It emits no event. It does not run for a `standard` workspace or for a copy that clone work kept instead of cloning.

The same set-up can be run again later on a `ready` workspace (packages 34 and 36 do so). Run again, it repeats only what is missing: it fetches upstream, turns remembered resolutions on if they are off, and creates the integration branch only when it does not exist; an existing integration branch is never moved by it.

### Access and common preconditions (CP-FR-006 to CP-FR-008)

Every route follows the hub's order of checks. `GET` routes need the scope `patches:read` and read access to the workspace; `POST /workspaces/:slug/patches` needs `patches:write` and write access. A PAT without the scope gets `403 insufficient_scope`; a caller with no access to the workspace gets `404 not_found`; a caller with read access only gets `403 forbidden` on the write route.

After request validation, in this order:

- the workspace is in carry-patch mode, else `400` with `error_type` `workspace_mode_mismatch`. A `standard` workspace and every task fork (also the fork of a carry-patch workspace) get this answer;
- for write routes, the workspace's status allows the write, else `409 workspace_busy` (status `pending` or `cloning`, or a reclone, archive or delete is running), `409 workspace_not_ready` (`failed`) or `409 workspace_archived` (`archived`).

Read routes work in every workspace state.

### The patch object (§10.3.3)

Every patch route returns this object with every field present (`null` when unset): `id` (UUID), `workspace_slug`, `branch_name`, `position` (integer; `null` for a deleted patch), `status` (`active`, `conflict`, `disabled`, `merged_upstream`, `deleted`), `conflict_files` (array of strings, `[]` unless the status is `conflict`), `upstream_pr_url`, `description`, `added_at`, `updated_at`, `deleted_at`, `origin_sync_state`, `origin_sha`, `origin_synced_at`. Non-deleted patches of a workspace hold the positions 1..N without gaps. `branch_name` is unique among the non-deleted patches of a workspace. A new patch is `active` with `conflict_files` `[]` and the three origin fields `null`. The patches of a workspace are removed with it, by a remover this package registers with package 14.

### Add a patch (CP-FR-009 to CP-FR-016)

`POST /workspaces/:slug/patches` takes one patch object or an array of them (a batch) and accepts `Idempotency-Key`.

| Member | Type | Notes |
|---|---|---|
| `branch_name` | string, required | A valid git branch name (WS-FR-011); not the integration branch |
| `position` | integer, at least 1 | Default: append |
| `upstream_pr_url` | string | Free text |
| `description` | string | Free text |
| `skip_branch_check` | boolean | Default false |
| `if_not_exists` | boolean | Default false; single add only |

**Validation**, before anything else happens: a body that is neither an object nor an array, an empty array, an array element that is not an object, a missing or invalid `branch_name`, a `branch_name` equal to the integration branch, a `position` that is not an integer of at least 1, a member of the wrong JSON type, `if_not_exists: true` in a batch, and a branch named twice in one batch are each `400 invalid_request`. In a batch the message starts with `patch[<i>]: `, where `<i>` is the zero-based index of the offending element (for a duplicate, the later one). Nothing is stored and no branch is created.

**Already registered.** A branch that is the branch of a non-deleted patch is `409` with `error_type` `patch_exists`. In a single add with `if_not_exists: true` the answer is `200` with the existing patch instead; nothing changes and no event is emitted. This check runs before branch resolution.

**Branch resolution.** Unless `skip_branch_check` is true, the branch must exist. The hub looks, in order, for:

1. the branch in the hub's copy (`refs/heads/<branch>`): reported as `local`;
2. the branch on origin as of the last origin fetch (`refs/remotes/origin/<branch>`): the hub creates the branch at that commit; `origin_tracking`;
3. when the workspace setting `patch_branch_source` is `origin`: the branch on origin now. The hub fetches that one branch with the origin credentials, under a limit of 2 minutes, and creates the branch at its tip; `origin_fetch`.

If none is found the answer is `404` with `error_type` `branch_not_found`. A failed origin fetch (origin unreachable or refusing, the credential unreadable, the limit passed) is `502` with `error_type` `origin_fetch_failed` and a message without credentials. With `skip_branch_check: true` nothing is looked up and the resolution is `skipped`; the patch is registered although its branch may not exist. A branch created by resolution stays even if the request then fails.

**A soft-deleted patch with the same branch** does not count as registered. The add removes it for good, together with `refs/parley/replaced/<branch>`, and creates a new patch with a new id.

**Position.** With N non-deleted patches, an omitted `position` or one greater than N+1 appends the patch at N+1. Otherwise the patch takes that position and the patches at and after it move down by one. In a batch, the elements with an explicit position are placed first, in array order, each into the list as it then stands; the others are then appended in array order.

**Result.** A single add answers `201` with the patch. A batch answers `201` with `{"items":[patch,…]}` in request order, each with its final position. A batch is all or nothing: when any element is refused (`409 patch_exists`, `404 branch_not_found`, `502 origin_fetch_failed`) or the storage fails, no patch of the batch exists afterwards and no event is emitted. An absent, `null` or empty `description` or `upstream_pr_url` is stored as `null`.

**Event.** Each created patch emits `parley.patch.create`, written with the patch: `resource_type` `patch`, the patch id as `resource_id`, the caller as actor, metadata `branch_name`, `position` (the final one) and `branch_resolution` (`local`, `origin_tracking`, `origin_fetch` or `skipped`).

**Concurrency.** Adds on one workspace never fail because another add is running. Of two concurrent adds of the same branch exactly one creates the patch and the other is answered as already registered. After any number of adds the positions are 1..N without gaps.

### Read patches (CP-FR-017, CP-FR-018)

`GET /workspaces/:slug/patches` answers `200` with `{"items":[patch,…],"next_cursor":…}`: the non-deleted patches in position order. With `include_deleted=true` the deleted patches follow, newest `deleted_at` first (ties by `id`). `include_deleted` accepts `true` and `false`; another value is `400 invalid_request`. `limit` and `cursor` follow the hub's list rules.

`GET /workspaces/:slug/patches/:id` answers `200` with any patch of the workspace, deleted or not. It adds the member `replaced_sha` (a full commit id) when `refs/parley/replaced/<branch>` exists in the workspace's copy; otherwise the member is absent, also when the workspace has no copy at the moment. An id that names no patch of this workspace (unknown, a patch of another workspace, or not a UUID) is `404 not_found` with the message `patch not found`.

### CLI (§10.6)

- `parley patch add <workspace-slug>` with `--branch` (required), `--position <int>`, `--upstream-pr <url>`, `--description <text>`, `--skip-branch-check`, `--if-not-exists`: calls `POST /workspaces/:slug/patches` with one object. A flag that was not given is not sent.
- `parley patch list <workspace-slug>` with `--include-deleted`: calls `GET /workspaces/:slug/patches`, with `include_deleted=true` when the flag is given.
- `parley patch show <workspace-slug> <patch-id>`: calls `GET /workspaces/:slug/patches/:id`.

Each prints the hub's response on stdout and exits 0; a hub error is printed on stderr as received and exits 1. A missing `--branch`, a wrong number of arguments or a `--position` that is not an integer is a usage error: exit 2, nothing sent.

### End-to-end flows

1. A developer creates a workspace with `workspace_mode: carry_patch` and an `upstream_url`, without `integration_branch`. `parleyd` clones origin, fetches upstream, turns on remembered resolutions, creates `deploy` at the upstream base and sets the workspace `ready`. The developer reads the workspace and sees `integration_branch: "deploy"`; a git client that looks at the copy finds `refs/heads/deploy` at the head of upstream's default branch.
2. A developer runs `parley patch add` for a branch that exists on origin, then an agent registers a batch of two branches over the API with one explicit position. `parleyd` creates the missing hub branches, stores the patches and emits three `parley.patch.create` events. `parley patch list` shows the three patches in position order and `parley patch show` shows one of them.

## Design Decisions

1. **Language and layout.** Go; the code lives in `internal/carrypatch/` and `internal/cli/patch.go`; git runs as a subprocess through `internal/gitrepo` (project decisions 1, 3, 4, 6, 7). The patch list is a table of the embedded transactional store (project decision 2).
2. **Where the default `deploy` is applied.** Package 10 stores `integration_branch` as given and answers `null` at creation. This package writes `deploy` into the record in the set-up after the first clone, and treats a stored `null` as `deploy` everywhere before that. CP-FR-002 says only that the member defaults to `deploy`; this keeps package 10's create answer as it is specified.
3. **`patch_exists` is checked before branch resolution.** CP-FR-012 says that `if_not_exists` changes nothing; resolving first could create a branch.
4. **`UPSTREAM_GIT_PAT` uses the user name `x-token-auth`**, as `GIT_PAT` does for origin (§4.3.5). The PRD names no user name for upstream.
5. **A failure to read the secrets fails the credential resolution** and so the upstream fetch or the origin fetch that needed it; no anonymous attempt is made (§4.3.5 for origin, applied to upstream as well).
6. **The integration branch falls back to origin's default head also when the fetch worked but no upstream base resolves.** CP-FR-004 names only the failed fetch; an integration branch must exist either way.
7. **The post-clone set-up replaces a cloned branch of the integration branch's name; a repeated set-up never moves an existing integration branch.** The first follows from CP-FR-005, the second from "the next sync or rebuild repeats what is missing".
8. **The upstream's default branch is remembered in the copy's git configuration** (`parley.upstreamDefaultBranch`), not in a ref, so no extra ref is advertised.
9. **The upstream fetch of the set-up has the 10-minute limit of CP-FR-093; the single-branch origin fetch of branch resolution has the 2-minute limit of the sync fetch (WS-FR-045).** The PRD gives no limit for either.
10. **`if_not_exists: false` in a batch is accepted**; only `true` is refused.
11. **The batch message index is zero-based.** The PRD shows `patch[<i>]` without saying.
12. **Empty `description` and `upstream_pr_url` are stored as `null`**, matching the update route where an empty string clears the field (CP-FR-019).
13. **A patch id that is not a UUID is `404 not_found`**, the same answer as an unknown id.
14. **`replaced_sha` is absent, not `null`, when no backup ref exists** (§10.3.3: the routes "add" it when a backup ref exists).
15. **`include_deleted` accepts exactly `true` and `false`.**
16. **The backup ref of a purged soft-deleted patch is deleted after the add's transaction commits**; a failure of that deletion is logged and does not fail the add.
17. **`patch_branch_source` is read from the workspace's effective settings by key; a missing key means `hub`.** Package 11 registers the key and is not a dependency of this package.
18. **One git runner for all carry-patch work (packages 32 to 38).** `internal/carrypatch/gitrun.go` runs every git operation of `internal/carrypatch` through `internal/gitrepo`: each operation gets a deadline of 10 minutes or the caller's shorter one (2 minutes for the single-branch fetch, 120 seconds for a push to origin), and the text of a failed operation is cleaned with `SanitizeGitText` of `internal/gitrepo/safety.go` (`***@` for URL user information, resolved credential values removed). This package needs both for its own upstream fetch; package 38, which owns CP-FR-093 and CP-FR-094, adds the transport rule and the argument guard and proves the limits across rebuild, sync and preview. No carry-patch package calls git with a limit or a cleaning of its own.
19. **The interface every rebuild trigger calls is declared here** (`RebuildEnqueuer` in `internal/carrypatch/rebuild_enqueuer.go`, with a default that queues nothing). Packages 32, 34, 36 and 37 call it and package 33 implements and registers it; declaring it in the package all of them depend on keeps package 32 free of a dependency on package 33.
20. **`IntegrationBranch(workspace)` is the one place the `deploy` fallback lives.** Packages 33 to 38 read the integration branch through it.
21. **Workspace deletion.** The patch rows are removed by a remover registered with package 14's removers of owned data, inside the workspace's delete transaction.
22. **Profile: full.**

## Dependencies

- Package 10 workspace_create_and_clone (draft): the workspace record and its carry-patch fields, the state guard, the post-clone step registry, the origin credential resolver, the settings reader, `internal/gitrepo` and its test origin helper.
- Package 12 workspace_sync_from_origin (draft): the origin fetch and the compare-and-swap ref update in `internal/gitrepo`.
- Package 14 workspace_archive_reclone_delete (draft): the removers of owned data, with which this package registers the remover of a workspace's patches.
- Package 09 cli_foundation (draft): the CLI's HTTP client, output and exit codes, on which the three commands are built.
- Package 52 durable_event_log_and_stream (draft): the event component that writes `parley.patch.create` with the patch.
- The `git` command, 2.38 or later, at run time.

## Verified External API

- `github.com/spf13/cobra` (**unverified**; the repository has no code and no module cache entry to read): `type Command struct { Use string; Short string; Args PositionalArgs; RunE func(cmd *Command, args []string) error; ... }`, `func ExactArgs(n int) PositionalArgs`, `func (c *Command) Flags() *pflag.FlagSet`. The commands are declared through package 09's command helper, which wraps these.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Origin**: A registered workspace's remote (`git_url`).
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task.
- **Carry-patch workspace**: A workspace that tracks an upstream project and keeps an ordered list of patches on top of it.
- **Upstream**: For a carry-patch workspace, the project it tracks (`upstream_url`).
- **Integration branch**: For a carry-patch workspace, the branch the hub rebuilds as upstream plus the ordered patches.
- **Patch**: A carry-patch entry: a branch with a position in the patch order and a status.
- **Upstream base**: The commit every rebuild starts from: the head of the upstream's default branch as of the last upstream fetch.
- **Remembered resolutions**: Conflict resolutions the hub records and replays when the same conflict appears again (git rerere).
- **Backup ref**: `refs/parley/replaced/<branch>`, the patch tip the hub discarded when it replaced the branch.
- **Rebuild**: A durable job that recreates the integration branch.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number.
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
