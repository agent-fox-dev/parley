# Errata: 36_carry_patch_sync

Where the implementation of package 36 (`.specs/36_carry_patch_sync`)
differs from what the package says, or decides what it leaves open.

## A refresh never moves a patch branch back to an older origin head

The package compares each patch branch with "origin's tip as of this sync's
fetch". Package 12 releases its fetch lock right after the origin fetch, so
a later sync can fetch newer heads and refresh a patch branch while an
earlier sync is still in its per-branch pass. Compared with its own, older
fetch, the earlier sync would see the hub's tip neither equal to nor behind
origin's, and replace it (or, under `report`, call it diverged): the branch
would go back to the older head.

So when the hub's tip would be replaced or reported diverged, the refresh
reads the copy's current `refs/remotes/origin/<branch>` as well. When a
later fetch moved it and the hub's tip equals it or is its ancestor, the
outcome is taken against that newer head (`none` or `fast_forwarded`,
`in_sync`), and `origin_sha` in the entry, the stored field and the event is
that newer head (`syncPass.compare`, `internal/carrypatch/sync_refresh.go`).

## The backup of a replace is written only if it did not change

A replace writes `refs/parley/replaced/<branch>` before it swaps the
branch. The refresh reads the backup ref when it compares, and writes the
backup by a compare-and-swap against that value (`SwapBackupRef`,
`internal/carrypatch/backup_ref.go`): a reset to origin that saved another
head there in between keeps its backup, and the refresh of that branch
fails as a lost swap. A reset to origin still writes its backup
unconditionally (`WriteBackupRef`).

## The set-up at sync leaves the integration branch until the base resolves

The package has the sync repeat the set-up with `EnsureSetup`, which also
creates a missing integration branch. That step would run when the upstream
fetch fails or no base resolves, and the sync would answer `502` after
creating a branch. The sync runs the set-up without the branch step and
creates a missing integration branch at the upstream base only after the
base resolves; a failure there is logged and does not fail the sync.

## Upstream fetches of one workspace take turns

A sync and a rebuild, or two syncs, fetched upstream into the same copy at
once and could collide on git's ref and config locks, answering `502` for
nothing. The repeated set-up (upstream fetch, config, integration branch)
of a workspace holds a per-workspace lock of the component, shared by sync
and rebuild; waiting for it ends with the caller's context.

## A rebuild owed by a sync that fails after its refresh is still queued

The package queues a rebuild on a failed refresh when the branch of an
`active` or `conflict` patch moved before the failure. The same holds when
merge detection or the clearing of origin fields fails after the refresh:
the moves (and a patch marked merged) stay, and the next sync would not see
them again. Upstream movement does not count, since a failed sync records
no upstream head and the next sync sees it again. A failed enqueue is
logged; the sync answers the first failure.

## Other decisions

- The first refresh of a patch (stored `origin_sync_state` `null`) is a
  state change and emits `parley.patch.sync`.
- `submitted_by` of a rebuild queued by a sync is the caller's user id, or
  the admin token's actor type when the caller is the admin token, as for a
  reset to origin.
- The pull request number of `upstream_pr_url` is the decimal number after
  `/pull/` followed by `/`, `?`, `#` or the end, compared as a number:
  `/pull/042` matches a subject ending `(#42)`.
- `git cherry` leaves merge commits of a patch branch out, and counts a
  patch commit whose upstream equivalent is also reachable from the patch
  branch as not equivalent, as git computes it.
