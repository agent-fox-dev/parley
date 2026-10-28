---
spec_id: "09"
spec_name: "carry_patch"
title: "Carry a patch stack on top of an upstream and rebuild it"
status: "active"
created_at: "2026-10-27T18:46:58Z"
updated_at: "2026-10-28T10:06:51Z"
intent_hash: "11c56eb674e65f449b483b348d1fb0cc0f58b0e7077fd0883e6bc44ad486ed86"
schema_version: 2
source: "docs/prd/01-port-af-hub-to-typescript-on-cloudflare.md"
---

# Carry a patch stack on top of an upstream and rebuild it

## Intent

Some teams run a fork of someone else's project with a handful of changes
they cannot upstream yet, and the expensive part is not making those changes
but keeping them applied as the upstream moves. A `carry_patch` workspace
turns that into something a machine does: an ordered list of branches, an
integration branch rebuilt from the upstream head plus those branches, and a
record of which patches have landed upstream and which now conflict. This is
the most stateful corner of the hub, and the one where a wrong answer costs a
person an afternoon of rebasing by hand.

## Goals

- The patch list behaves as `docs/prd/api.md` § Patch Endpoints specifies:
  ordered, contiguous, with the documented branch resolution, batch
  semantics, and `reset-to-origin`.
- A `carry_patch` sync performs the eight steps of § Carry-Patch Sync
  Extension in order and answers with its documented fields.
- A rebuild recreates the integration branch as § Rebuild Endpoints
  describes, with the two strategies, the two failure modes, replayed
  resolutions, follow-up rebuilds, and a lock held only at its start and end.
- The rebuild, rerere and patch-status routes answer as their entries
  specify, including the preview that predicts without writing.
- `afc patch`, `afc rebuild`, `afc rerere` and `afc workspace patch-status`
  behave as `docs/prd/cli.md` specifies.

## Non-goals

- **Computing history.** The three-way merge, the rebase, the merge strategy
  and the recorded resolutions are `06_history_engine`. This package decides
  what to apply, in what order and what to do about a conflict; it does not
  compute the result.
- **The standard sync.** `08_sync_and_branch_push` owns the sync route; this
  package takes over its documented branch point for `carry_patch`
  workspaces.
- **Push control.** `07_git_server` already refuses, forwards or mirrors a
  pushed patch branch. This package adds only the rebuild that an accepted
  push triggers.
- **Audit events.** `hub.patch.*`, `hub.rebuild.*` and the rest are
  `12_audit_and_events`.
- **Merge jobs and batch rebase**, which are `10_merges`.

## Background

Everything this package needs exists: the patch-bearing workspace and its
settings (`03`), the repository and its credentials (`05`), the engine that
applies a patch and replays a resolution (`06`), the receive path that
accepts a patch branch (`07`), and the sync route it extends (`08`).

Three parts of `api.md` are unusually exacting and are where the work is.
The **batch add** validates every element's syntax in array order before any
branch is looked up, then checks every branch before adding anything, and the
first branch failure refuses the whole batch — while branches already
created for earlier elements stay, and are found as `local` next time. The
**carry-patch sync** is eight numbered steps whose failure in step 5 is a
`500` that still records the outcomes already reached, still queues a rebuild
for branches already moved, and writes neither `upstream_head_sha` nor
`last_sync_at`. And a **rebuild** holds the workspace lock only at its start
and its end, so a sync, a merge or a push may run while it applies patches —
which is what makes the stale-snapshot follow-up necessary.

`refs/hub/replaced/` is where a replaced patch tip is kept. Whether the store
accepts that namespace at all is established by `05_repository_store`;
decision D16 of the source PRD fixes the fallback, and this package follows
whichever answer `05` recorded.

## Requirements

### The patch list

- A patch is added to a `carry_patch` workspace with a branch name, an
  optional position, an optional upstream pull request and description, and
  gets status `active`. A position inserts and shifts the rest down; past the
  end, and omitted, it appends.
- Unless `skip_branch_check` is set, the branch must exist, found in this
  order: the workspace's own repository; the fork's copy already fetched;
  and, only with `patch_branch_source` `origin`, a fetch from the fork. The
  first that succeeds wins, and the resolution is reported.
- `if_not_exists` answers `200` with the existing patch instead of `409`.
- A batch adds several in one transaction: every element's syntax is checked
  first, in array order, before any branch is looked up; then every branch is
  checked before anything is added; the first failure refuses the whole
  batch, and branches created for earlier elements stay.
- Patches are listed in position order with deleted ones left out, and a
  standard workspace answers an empty list. One patch is read including a
  deleted one, with the SHA of a replaced tip where one is kept.
- A patch's position, status, description and upstream pull request can be
  changed; its branch cannot. Positions stay contiguous after every change.
- A patch is removed, restored to the end as `active`, and the whole list
  reordered at once.

### Resetting a patch to the fork

- `reset-to-origin` sets a patch branch to the fork's tip whatever the
  divergence policy, by the first rule that applies: the workspace has no
  such branch (created), the commits match (nothing written), the fork's tip
  descends from the workspace's (fast-forwarded), otherwise replaced with the
  old tip kept.
- It answers with the patch, the replaced SHA where one was kept, and whether
  a rebuild was queued and which.

### Syncing a carry-patch workspace

- The sync fetches `upstream`, not the fork, unless `patch_branch_source` is
  `origin`, in which case it fetches the fork too and brings every registered
  patch branch to the fork's tip under the divergence policy.
- It performs the eight documented steps in order, detects an upstream
  force-push without treating it as an error, finds patches merged upstream
  by the configured strategies, writes `last_sync_at` and
  `upstream_head_sha`, and queues a rebuild under the documented conditions.
- A failure refreshing a patch branch ends the sync with `500`, records the
  outcomes already reached, queues a rebuild for branches already moved, and
  writes neither `upstream_head_sha` nor `last_sync_at`.
- `sync_error` is not used on this path: a failure is reported by the status.

### Rebuilding the integration branch

- A rebuild starts from the upstream head it fetches at its start and applies
  the patches in position order, apart from the workspace's checked-out
  state, skipping patches that are merged upstream, disabled, deleted or
  whose branch is missing, each with its reason.
- Each other patch is applied by the strategy — `rebase` or `merge` — and a
  conflict first replays a recorded resolution; if one remains, `fail_fast`
  fails the job and `continue` marks the patch `conflict`, leaves its changes
  out and goes on.
- At the end the integration branch is set to the result, patches merged
  upstream become deleted, positions close up, and with
  `rebuild_push_integration_branch` the branch is force-pushed to the fork; a
  failed push does not fail the job.
- A patch tip or upstream head that moves during a rebuild does not change
  the running job, and one follow-up rebuild is queued afterwards.
- The lock is held only at the start and the end, so other operations may run
  while patches are applied.
- Per-patch results are readable while the job runs.

### The rebuild, rerere and status routes

- Submit, list, read, cancel, requeue and roll back rebuild jobs, with the
  documented preconditions and refusals.
- The preview predicts, without writing anything, which patches would apply
  cleanly, trying every patch that is not disabled or merged upstream and
  letting a predicted conflict not stop the later predictions.
- Recorded resolutions are listed and forgotten by identity or by path.
- The patch-status dashboard answers the documented object with its summary
  counts.

### Triggering a rebuild

- An accepted push to a registered patch branch queues a rebuild unless
  `auto_rebuild_after_push` is false.
- A rebuild is never queued twice: when one is already queued or running,
  nothing more is queued and the caller is told so.

### End-to-end flows

1. **A patch is registered and the stack rebuilt.** An API client adds a
   branch as a patch; Parley finds it in the workspace's repository, appends
   it as `active` and reports how it was resolved. The client submits a
   rebuild; the job takes the lock, fetches the upstream head, releases the
   lock, applies each active patch in order through the history engine,
   retakes the lock, sets the integration branch and completes. The client
   polls the job and sees a result per patch.
2. **An upstream advance is absorbed.** The client syncs the workspace;
   Parley fetches upstream, finds it advanced, detects one patch as merged
   upstream, queues a rebuild and answers with the rebuild's id. The rebuild
   skips the merged patch, applies the rest, marks the merged patch deleted
   and closes the positions.
3. **A conflict is recorded once.** A rebuild with `continue` hits a conflict
   on one patch, marks it `conflict`, leaves it out and finishes. A person
   resolves it, and the next rebuild of the same stack replays the resolution
   and applies the patch cleanly.
4. **A diverged patch branch is reset.** With `patch_branch_source` `origin`
   and the policy set to report, a sync reports a patch branch as diverged;
   the client calls `reset-to-origin`, Parley moves the branch to the fork's
   tip, keeps the old tip under the protected namespace, and queues a
   rebuild.

## Design Decisions

1. **The rebuild is a job, not a request.** `api.md` answers `202` with a
   queued job and has clients poll it, and the work is far too long for one
   request on this platform. The job's progress is written as each patch
   finishes so a poller sees movement.
2. **The lock is taken twice, not held throughout.** The reference is
   explicit that a rebuild blocks other operations only at its start and its
   end. That is what makes the stale-snapshot follow-up necessary, and both
   halves are specified here rather than left to the implementation.
3. **The sync's step 5 failure is specified as a partial success.** It is the
   one place in the reference where a `500` still has effects, and writing it
   as "the sync failed" would lose the outcomes already reached and the
   rebuild already owed.
4. **Replaced tips follow whatever `05` found.** Where the store accepts the
   namespace they are kept as refs under it; where it does not, they are kept
   as `05`'s recorded fallback, and `replaced_sha` is reported either way.
5. **This package is at the size limit deliberately.** Ten requirements for
   one feature is the most the format allows, and splitting the patch list
   from the rebuild would put the sync's step 5, which writes both, across a
   package boundary.

## Dependencies

- `03_workspaces` for the workspace, its settings and the lock.
- `05_repository_store` for the repository, the credentials, and the
  capability report that decides how a replaced tip is kept.
- `06_history_engine` for applying a patch, detecting a conflict and
  replaying a recorded resolution.
- `07_git_server` for the accepted push this package turns into a rebuild
  trigger.
- `08_sync_and_branch_push` for the sync route this package extends at its
  documented branch point.
- `docs/prd/api.md` § Patch Endpoints, § Carry-Patch Sync Extension,
  § Rebuild Endpoints, § Rerere Management Endpoints and § Patch Status
  Dashboard Endpoint are normative.

## Verified External API

Nothing new. This package adds no external dependency: the git library is
verified in `06_history_engine`, the store binding in `05_repository_store`,
and the queue and scheduled-job facilities are platform interfaces following
the pattern of `01_service_foundation`.
