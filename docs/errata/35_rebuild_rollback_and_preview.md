# Errata: 35_rebuild_rollback_and_preview

Where the implementation of package 35
(`.specs/35_rebuild_rollback_and_preview`) differs from what the package
says, or decides what it leaves open.

## The commit test of a rollback is ObjectInfo

The package adds a test that an object is a commit to
`internal/gitrepo/refs.go`. `(*gitrepo.Repo).ObjectInfo` already answers
it: it takes a full 40-hex id alone, checked before git runs, so the
recorded `previous_integration_head_sha` is never read as an option or a
ref, and its `Type` is `ObjectCommit` for a commit. A blob, a tree or a
missing object is `409 rollback_unavailable`. `refs.go` is unchanged.

## What a rollback does beyond the package

- When the store transaction of `parley.rebuild.rollback` fails after the
  compare-and-swap won, the branch is swapped back from the target to the
  head read (removed again when the rollback created it), logged when the
  undo fails, and the answer is `500 internal`, so that no move goes
  unreported. From the swap on the rollback runs under
  `context.WithoutCancel`.
- A missing integration branch whose name another ref is in the way of
  (`refs/heads/deploy/next` beside a missing `deploy`) is `409 conflict`,
  as for a reset to origin.
- The `ref_moved` answer is the reset's (`refMovedNow`), with the
  operation's wording: `the branch "<branch>" moved while it was rolled
  back; read it again and retry`.

## The preview is a read route that checks the workspace's status

`Guard` serves read routes in every state, so the preview, declared a read
route (`rebuilds:read`, read access), calls `workspace.RequireReady` itself
after the guard.

## The trial merge needs git 2.40 for its other callers

The package names git 2.38 for `git merge-tree --write-tree`. The preview's
`TrialMerge` runs it without `--merge-base` when the two commits share an
ancestor, so that it merges from every merge base, as the rebuild's `git
merge` does: with one base of a criss-cross alone, the preview could
predict a clean merge where the rebuild conflicts, or the other way round.
Commits that share no ancestor are merged from an empty-tree commit with
`--merge-base`, which needs git 2.40, the floor the hub already requires
(errata 20 and 22).

## A tip the predicted state already holds

A patch whose tip the predicted state already holds (its merge base is the
tip) is `would_succeed` and leaves the state as it is, as a rebuild's merge
does, rather than adding a merge commit of the state and the tip.

## Checks

- The refusals of both routes by scope, access, mode and workspace status
  are rows of the route-wide tables of `rebuild_routes_test.go`, which test
  them for every rebuild route, not tests of their own.
- The upstream base check records the upstream's default branch in
  `parley.upstreamDefaultBranch`, which `UpstreamBase` reads (erratum 31),
  rather than as a `refs/remotes/upstream/HEAD` symref.
- The first execution path's two rebuilds are submitted over REST and
  polled, not with `parley rebuild submit --wait`, whose polling has a 1 s
  floor; the rollback itself runs through the CLI. Both paths use the slug
  `acme-fork` of the shared end-to-end helpers.
- The checks' "recording event emitter" is the hub's event log, read
  through `hub_events`.
