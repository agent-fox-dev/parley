---
spec_id: "37"
spec_name: "carry_patch_push_control"
title: "Control pushes to integration and patch branches"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "99e6412d5f5616f9a01fc199ce0dd72b9cf6c605a10708254b048a92c5c2a7fc"
schema_version: 3
source: "docs/prd/10_carry_patch.md"
---

# Control pushes to integration and patch branches

## Intent

In a carry-patch workspace two kinds of branch are not ordinary branches: the integration branch, which only the hub may write, and the patch branches, whose authority is either the hub or origin, as the workspace's settings say. Parley decides every client write to these branches by one set of rules, whether the write arrives as a git push, through the source control API or as a landing: it refuses, accepts, or first forwards the update to origin, never lets an update through unchecked, and after every accepted update of a patch branch queues a rebuild and mirrors the branch to origin when the settings ask for it. A team can so choose where its patches live and still trust that the integration branch is always what the hub built.

## Goals

- A carry-patch controller in `internal/carrypatch`, registered with the push rule engine of package 18 as its rule 4, that answers for every ref update on a carry-patch workspace: allow, refuse with a message and the `error_type` `integration_branch` or `patch_branch`, or allow after a forward to origin.
- Every create, update and delete of the integration branch by a client is refused, for every credential and every combination of settings.
- A patch branch whose authority is origin is refused with a pointer to origin, or, with `push_patches_to_origin`, forwarded to origin as a non-forced push before the hub accepts it; a forward that fails refuses the update.
- A decision that cannot be made (the patch list or the settings cannot be read) refuses the update with a retryable error. No update passes unchecked.
- Landing a change on a patch branch obeys the same rules: `blocked` without forwarding, forwarded before the target moves with forwarding, `failed` with `error: "origin_rejected"` when origin does not take it.
- After every accepted update of a patch branch that is not a delete, the hub queues one rebuild when `auto_rebuild_after_push` is true, and, when the hub is the authority and `push_patches_to_origin` is true, mirrors each updated patch branch to origin with force-with-lease. Neither step delays or changes the answer the client got.
- A failed mirror is reported as one `parley.patch.mirror_failed` event per branch.

## Non-goals

- The rule order, rules 1, 2, 2a, 3 and 5, the owner exemption, atomicity of a push, the compare-and-swap, when the forwards of a push run, the 120 s limit the engine puts on a forward of a push, the `parley.git.push` event and the post-push listener registry (GIT-FR-020 to GIT-FR-035, GIT-FR-060 to GIT-FR-062): package 18 `git_push_rules`. This package supplies the decision of rule 4 and the forward itself.
- How the source control API turns a refusal into `403` with the `error_type`, and a rule that cannot be evaluated into `500 internal` (SCM-FR-020 to SCM-FR-028): package 21 `scm_write_api`.
- `POST /workspaces/:slug/branches/:name/push` and its refusal of the integration branch and of registered patch branches with `403` `integration_branch` or `patch_branch` (WS-FR-053, named in CP-FR-088): package 13 `workspace_push_to_origin`. This package uses the push operation that package put into `internal/gitrepo`.
- Refusing a change whose target is the integration branch (`403 integration_branch`, CHG-FR-005, named in CP-FR-077), and opening a change that targets a patch branch from the workspace or from a task fork (CP-FR-085): package 22 `change_opening_and_versions`. Versions, policy, approvals and checks of such a change: packages 23, 27 and 28.
- The land attempt, its states, the target guard and after-landing hook registration points, and how a guard's answer becomes `blocked` or `failed` (LAND-FR-010 to LAND-FR-021): package 25 `optimistic_landing`. This package registers one guard and one hook.
- The patch list, the patch object, the carry-patch route guard and patch routes (CP-FR-001 to CP-FR-018): package 31 `carry_patch_setup_and_patches`. Patch update, delete, restore, reorder and reset to origin: package 32 `patch_maintenance`.
- The durable job model, the enqueue with its dedup and the `parley.rebuild.enqueue` event (CP-FR-034 to CP-FR-039): package 33 `rebuild_jobs`. The rebuild itself, its stale-input check and the push of the integration branch to origin (CP-FR-040 to CP-FR-055): package 34 `rebuild_execution`.
- The carry-patch part of sync, including the return of a forwarded branch through sync and the fields `origin_sync_state`, `origin_sha` and `origin_synced_at` of a patch (CP-FR-067 to CP-FR-076): package 36 `carry_patch_sync`.
- The settings keys `patch_branch_source`, `push_patches_to_origin`, `auto_rebuild_after_push` and `protected_branches`, their defaults and validation: package 11 `workspace_read_update_settings`. This package reads them.
- The event log: package 52 `durable_event_log_and_stream`. The credential scrubber (PLAT-FR-098): package 03 `server_lifecycle_and_configuration`.
- Any REST route and any CLI command. This package has none.

## Background

The repository holds no code yet; every file named here is new. The source is `docs/prd/10_carry_patch.md`: CP-FR-077 to CP-FR-088 with the parts of §10.3.2 (refs), §10.7 (triggers), §10.8 (`parley.patch.mirror_failed`), §10.9 (settings) and §10.10 (edge cases) that belong to them. `docs/prd/05_git_server.md` GIT-FR-031 gives the same decisions as a table and fixes that forwarding runs last.

Package 18 evaluates the push rules for every writer of a ref. For a workspace in mode `carry_patch` its fourth rule asks a registered controller once per ref update that passed rules 1 to 3; the controller answers allow, refuse (message and `error_type`), allow after a forward (a function the ref writer runs later, after every rule has passed for every ref), or reports a failure, which the engine turns into `internal error; try again`. After an accepted request that wrote refs through its ref writer is answered, package 18 calls its registered post-push listeners in the background, with the slug, the accepted updates, `via` and the session id. Package 31 delivers the patch list in `internal/carrypatch`. Package 13 delivers the push operation of `internal/gitrepo` (a commit, a branch name, a lease, a deadline) and the test origin that accepts pushes. Package 25 calls registered target guards before the target of a land moves and registered after-landing hooks afterwards. Package 33 delivers the rebuild enqueue.

Code of this package:

| Path | Holds |
|---|---|
| `internal/carrypatch/pushcontrol.go` | the controller: the decision for one ref update |
| `internal/carrypatch/forward.go` | the forward of one patch branch to origin |
| `internal/carrypatch/trigger.go` | what runs after accepted updates: the rebuild trigger, then the mirror |
| `internal/carrypatch/mirror.go` | the mirror of patch branches to origin |
| `internal/carrypatch/landguard.go` | the target guard and the after-landing hook for package 25 |
| `internal/hub/` | registration of the controller, the listener, the guard and the hook |

Tests live beside each file. Tests with a stock `git` client, a real hub and the test origin are in `internal/carrypatch/push_integration_test.go`.

This package stores nothing of its own: it reads the workspace record, the settings, the patch list and the recorded origin heads, and writes recorded origin heads and events. It registers no remover for workspace deletion.

## Requirements

### What the controller is asked, and its terms

The controller is asked about one ref update `{ref, before, after}` of a workspace in mode `carry_patch`. A create has an all-zero `before`, a delete an all-zero `after`. A task fork of a carry-patch workspace has mode `standard` (§10.3.1), so the controller is never asked about a fork.

- The **integration branch** is `refs/heads/<name>`, where `<name>` is what `IntegrationBranch` of package 31 returns for the workspace: the stored `integration_branch`, or `deploy` while the record holds none.
- A **patch branch** is `refs/heads/<branch_name>` of a patch of the workspace whose status is not `deleted` (CP-FR-079). The branch of a deleted patch is an ordinary branch.
- The settings are read each time a decision is made (§10.9): `patch_branch_source` (`hub` or `origin`; a missing key means `hub`), `push_patches_to_origin` (missing: `false`), `auto_rebuild_after_push` (missing: `true`). A changed setting takes effect at the next decision without a restart.

Over git a refusal reaches the client per ref in git's own format and the whole push is refused (GIT-FR-021). Over the source control API it is `403` with the `error_type` given here. Both are done by packages 18 and 21 from the controller's answer.

### Integration branch and backup refs (CP-FR-077, CP-FR-078)

A create, update or delete of the integration branch is always refused, for every caller including the admin token and whatever the settings: message `integration branch <name> is maintained by the hub; push a patch branch and rebuild instead`, `error_type` `integration_branch`. The check comes before the patch list is looked at.

`refs/parley/replaced/*`, the backups of replaced patch tips, cannot be written by clients: rule 1 of package 18 refuses every ref under `refs/parley/` with `refs/parley/ is maintained by the hub and cannot be pushed to` before the controller is asked. This package proves that it holds on a carry-patch workspace.

Every other ref that is not a patch branch is allowed by the controller: a tag or a note with the integration branch's name, the branch of a deleted patch, any unregistered branch.

### Patch branches (CP-FR-079 to CP-FR-083)

| `patch_branch_source` | `push_patches_to_origin` | Update | Answer |
|---|---|---|---|
| `hub` | any | create, update, forced update, delete | allowed (CP-FR-080) |
| `origin` | `false` | create, update, delete | refused, `patch_branch`: `branch is synced from origin; push to <git_url> instead` (CP-FR-081) |
| `origin` | `true` | delete | refused, `patch_branch`: `branch is synced from origin; delete it on origin instead` |
| `origin` | `true` | create, update | allowed after a forward to origin (CP-FR-082) |

`<git_url>` is the workspace's origin URL without user information; a `git@host:path` origin is shown as stored.

With `hub` a client may delete an unprotected patch branch; the patch stays registered (§10.10).

If the controller cannot decide, because the workspace record, the settings or the patch list cannot be read, it reports a failure and never an allow (CP-FR-083). The git client then sees `internal error; try again` on that ref and the API caller `500` with `error_type` `internal`; both may be retried.

### Forwarding to origin (CP-FR-082)

Evaluating an update does not contact origin. The forward is carried by the allowed decision and run by the ref writer of package 18 after every ref of the request has passed every rule, in command order.

A forward pushes the update's `after` commit to `refs/heads/<branch>` on origin as a non-forced push, authenticated with the workspace's origin credential resolved at that moment, under a limit of 120 s.

- Origin accepts: the forward succeeds, the recorded origin head of the branch becomes that commit, and the update is accepted on the hub.
- Origin refuses or cannot be reached: the forward fails with `origin rejected push: <reason>`. `<reason>` is origin's own message for the ref when origin refused the update by a rule or a hook; `non-fast-forward` when origin's branch is not an ancestor of the commit; `authentication failed`, `permission denied`, `repository not found` or `origin unreachable` for those failures; `push failed` for anything else. The whole message is scrubbed (the credential's value becomes `[redacted]`, user information is removed from URLs, at most 1024 bytes).
- The origin credential cannot be read: the forward fails with `failed to resolve origin credentials`. Origin is not contacted; there is no anonymous attempt.
- The limit passes or the caller cancels: the `git` process is ended and the forward fails with `origin rejected push: timed out`.

A failed forward refuses the update with `error_type` `patch_branch`. Over git it refuses the whole push; branches of the same push that were already forwarded stay on origin and come back through the next sync (GIT-FR-031).

### Protected patch branches (CP-FR-084)

A patch branch may be listed in `protected_branches`. A direct update of it is then refused by rule 2 of package 18 before the controller is asked. The one exception is a direct push under the owner exemption (GIT-FR-034): it passes rule 2, and the table above then applies to it. So on a workspace with `patch_branch_source: origin` and no forwarding even the owner's fast-forward of a protected patch branch is refused, with the `patch_branch` message.

### Landing on a patch branch (CP-FR-085, CP-FR-086)

A change may target a patch branch, from the same workspace or from a task fork, and follows the normal change life cycle. This package registers a target guard with package 25. Before the target of a land attempt moves, the guard is asked with the target workspace, the target branch, the old head and the new head, as package 25 calls it:

| Target | Settings | Guard's answer |
|---|---|---|
| not a patch branch, or the workspace is not in mode `carry_patch` | any | allow |
| patch branch | `patch_branch_source: hub` | allow |
| patch branch | `origin`, `push_patches_to_origin: false` | refuse: the attempt ends `blocked` with the reason `branch is synced from origin; push to <git_url> instead`; the target does not move |
| patch branch | `origin`, `push_patches_to_origin: true` | the new head is forwarded to origin as above; origin accepts: allow; any failure of the forward: origin rejected, and the attempt ends `failed` with `error: "origin_rejected"` and the target does not move |

If the guard cannot decide, it returns an error and never allows; package 25 ends the attempt `failed` with `error: "internal"` and the target does not move.

Landing on a patch branch has the same consequences as an accepted push to it: the after-landing hook runs the triggers below.

### Triggers after an update of a patch branch (CP-FR-087, CP-FR-088, §10.7)

After any accepted request that updated refs — a git push, a commit, ref move, ref create or server-side rebase through the source control API, or a landing — the hub looks at the updates that are not deletes and whose ref is a patch branch at that time. If there is none, nothing happens. Otherwise, in this order:

1. **Rebuild trigger.** When `auto_rebuild_after_push` is true, one rebuild is queued for the request (not one per branch), through the enqueue of package 33, with `submitted_by` `system:push` for a push or an API write and `system:land` for a landing, and with the session id of that push, commit or landing. When a rebuild is already queued, running or waiting to retry, the enqueue reports a duplicate and the trigger is skipped silently (CP-FR-039).
2. **Mirror.** When `patch_branch_source` is `hub` and `push_patches_to_origin` is true, each updated patch branch is pushed to origin, in the order of the updates: the head the branch has when the mirror runs, with force-with-lease against the recorded origin head of the branch (or "origin has no such branch" when none is recorded), under a limit of 120 s. On success the recorded origin head becomes the pushed commit and no event is written. On failure — origin refuses, the lease is stale, origin cannot be reached, the credential cannot be read, the limit passes — one `parley.patch.mirror_failed` event is written for that branch and the mirror goes on with the next branch. The mirror does not depend on `auto_rebuild_after_push`.

`parley.patch.mirror_failed` (§10.8): `resource_type: "patch"`, `resource_id` the patch id, the workspace's slug, actor `system`, metadata `branch_name` and `error`. `error` is the reason alone, scrubbed: one of the forward's reasons, `origin moved` for a stale lease, `timed out`, or `failed to resolve origin credentials`.

Both steps run after the client has its answer and never change the outcome of the push, commit or landing: a failure in them is logged, the refs stay as written, and the client's answer is the one it already has. A delete of a patch branch triggers neither a rebuild nor a mirror. A push to the branch of a deleted patch triggers nothing. The mirror pushes patch branches only: never the integration branch and never another branch of the push.

Forwarding, the mirror and the rebuild's push of the integration branch (package 34) are the only ways these branches reach origin.

### End-to-end flows

1. An agent pushes a new commit to patch branch `patch/tls` of a carry-patch workspace whose patches live on the hub and are mirrored (`push_patches_to_origin: true`). `parleyd` asks the controller, accepts the push and answers the git client; then it queues one rebuild with `submitted_by: "system:push"` and pushes `patch/tls` to origin. The agent then pushes to `deploy`; git prints `integration branch deploy is maintained by the hub; push a patch branch and rebuild instead` and nothing moves.
2. A developer pushes to patch branch `patch/tls` of a workspace with `patch_branch_source: origin`. git prints `branch is synced from origin; push to <git_url> instead`. The developer sets `push_patches_to_origin` to true and pushes again: `parleyd` forwards the commit to origin, origin accepts, the hub's branch moves and the push succeeds.
3. An agent opens a change that targets patch branch `patch/tls` and lands it. `parleyd` asks the guard, moves the patch branch, and afterwards queues a rebuild with `submitted_by: "system:land"`.

## Design Decisions

1. **Profile: full.** Three flows cross the git client, the rule engine, the controller, origin, landing and the rebuild queue.
2. **"Cannot decide" is reported to the engine as a failure.** CP-FR-083 asks for a retryable error; package 18 turns a controller failure into `internal error; try again` (`error_type` `internal`) and package 21 into `500 internal`. This package adds no message of its own.
3. **The reasons of `origin rejected push: <reason>` are fixed texts** (`non-fast-forward`, `authentication failed`, `permission denied`, `repository not found`, `origin unreachable`, `push failed`), except origin's own message for a refused update. The PRD gives no list; fixed texts can be tested and carry no credential.
4. **The forward enforces its own 120 s limit as well as honouring the caller's cancellation**, and names it `origin rejected push: timed out`, the text package 18 uses. The landing guard has no engine around it to keep the limit. The limit is a parameter of the component, 120 s in the hub, which tests shorten.
5. **A successful forward and a successful mirror record the origin head of the branch.** The PRD is silent. The record is the lease of the next mirror and of package 13's forced push, and after the push it is what origin holds.
6. **This package does not touch a patch's `origin_sync_state`, `origin_sha` or `origin_synced_at`.** They are the result of a sync comparison (package 36).
7. **The mirror pushes the head the branch has when the mirror runs, not the `after` of the update, and the triggers of one workspace run one at a time.** Two quick pushes would otherwise mirror in either order and report a false failure on a stale lease.
8. **A delete is not mirrored.** CP-FR-088 mirrors "each updated patch branch" and §10.10 says a delete triggers no rebuild; the hub never deletes a branch on origin.
9. **One rebuild is queued per accepted request, not per updated branch.** The dedup of CP-FR-039 would drop the others anyway.
10. **"Patch branch at that time" for the triggers is read after the request, from the patch list.** A branch whose patch was deleted between the push and the trigger triggers nothing.
11. **A failure to read the origin credential in the landing guard ends the attempt `failed` with `error: "origin_rejected"`**, like every other failed forward. CP-FR-086 names only "refuses or cannot be reached"; the land attempt has no closer error value.
12. **A guard that cannot decide returns an error**, which must end the attempt `failed` with `error: "internal"` in package 25. It must not allow (CP-FR-083), and a refusal would claim a policy reason. The guard's three answers are package 25's: allow, refuse with a reason (`blocked`), origin rejected (`failed`, `origin_rejected`).
13. **The guard does not refuse the integration branch as a target.** Package 22 refuses such a change when it is opened (CHG-FR-005), so no land attempt can name it.
14. **The triggers reach API writes through the same listeners as git pushes, and this package only consumes the listener.** The post-push listener registry is package 18's; calling it for accepted source control API writes as well as git pushes, and passing `via` and the session id with the call, is that package's behaviour (with package 21). This package registers one listener with `pushrule.RegisterPostPushListener` and maps `via` `git` and `api` to the source `push`. The listener ignores a call with `via: "land"`: package 18 calls its listeners for a land too, and the landing reaches the triggers through the after-landing hook of package 25, so acting on both would trigger twice for one land. It changes no file of `internal/gitserver` or `internal/scm`.
15. **`error` of `parley.patch.mirror_failed` is the reason without the `origin rejected push: ` prefix.** The event already says that a mirror failed.
16. **Dependencies beyond the plan's 18 and 31, and why each stays.** Wiring is by registration: the rule-4 controller and the post-push listener with package 18, the target guard and the after-landing hook with package 25, the patch-branch lookup with package 13. Kept as dependencies: 13, whose push operation the forward and the mirror call directly; 33, whose implementation of `RebuildEnqueuer` (the interface is package 31's) the triggers need and whose rebuild list route the smoke checks read; 25, whose guard and hook types this package implements and whose land route the third path drives; 52, because this package writes an event. Package 21 is not a dependency: nothing here calls or tests its routes. None of the kept packages depends on this one.
17. **The integration branch name is read through `IntegrationBranch` of package 31**, and git runs through the carry-patch git runner of package 31 with the 120-second limit as the caller's deadline.
18. **Project-wide:** Go; `git` 2.38 or later as a subprocess through `internal/gitrepo`, no git library; bare repositories; SQLite through `modernc.org/sqlite` behind `internal/store`; tests with `testing`, table-driven, repositories made with `git init` in `t.TempDir()`, origins served by `internal/gitrepo/gitrepotest/`; code takes a replaceable clock.

## Dependencies

- Package 18 `git_push_rules` (draft): the rule engine, the controller interface of rule 4, the ref writer that runs forwards, the post-push listeners.
- Package 31 `carry_patch_setup_and_patches` (draft): the patch list and `internal/carrypatch`; through it package 10 (workspace record, settings reader, origin credential resolver, recorded origin heads) and package 12.
- Package 52 `durable_event_log_and_stream` (draft): writing `parley.patch.mirror_failed`.
- Package 13 `workspace_push_to_origin` (draft): the push operation of `internal/gitrepo` with lease and deadline, its typed outcomes, the test origin that accepts, refuses and stalls pushes, and the patch-branch lookup of its push route, which this package registers.
- Package 25 `optimistic_landing` (draft): the target guard and after-landing hook registration points of `internal/land`.
- Package 33 `rebuild_jobs` (draft): the implementation of the rebuild enqueue with its dedup, behind the `RebuildEnqueuer` interface of package 31, and `GET /workspaces/:slug/rebuilds`.
- Package 03 `server_lifecycle_and_configuration`, through the above: the scrubber in `internal/scrub/`.
- External: the `git` command, 2.38 or later.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Origin**: A registered workspace's remote (`git_url`).
- **Carry-patch workspace**: A workspace that tracks an upstream project and keeps an ordered list of patches on top of it.
- **Integration branch**: For a carry-patch workspace, the branch the hub rebuilds as upstream plus the ordered patches.
- **Patch**: A carry-patch entry: a branch with a position in the patch order and a status.
- **Patch branch**: The branch of a patch that is not deleted.
- **Rebuild**: A durable job that recreates the integration branch.
- **Protected branch**: A branch that moves only by landing a change, by sync from origin, or by a direct fast-forward push from the workspace's owner or the admin token. Nobody can force-push or delete it. By default the default branch.
- **Direct push**: A fast-forward update of a protected branch by the workspace's owner or the admin token, over git or the source control API, without a change. Allowed while the workspace setting `protect.owner_push` is true.
- **Change**: The unit of work: a proposal to add the commits of a source branch to a target branch, with a number, a title, an intent, versions and a state.
- **Landing**: Adding a change's commits to its target branch: a server-side rebase followed by a compare-and-swap of the target ref.
- **Land attempt**: One request to land a change (or a stack), with its outcome.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number.
- **Forward**: The non-forced push of an update of a patch branch to origin that the hub makes before it accepts the update, when origin is the authority for patch branches.
- **Mirror**: The push of a patch branch to origin that the hub makes after it accepted an update, when the hub is the authority for patch branches.
- **Origin head**: For a branch of a repository workspace, the last head the hub saw on origin.
