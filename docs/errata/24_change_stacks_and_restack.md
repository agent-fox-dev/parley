# Errata: 24_change_stacks_and_restack

Where the implementation of package 24
(`.specs/24_change_stacks_and_restack`) differs from what the package says,
or decides what it leaves open.

## The new state lives in schema steps of the change component

The package asks for a store migration in `internal/store`. As for package
23, each owning package declares its own `store.Step`: `StackStep` adds the
column `restack_onto` (the landed commit a change must sit on once the
change below it landed) and `RestackStep` the table `change_restacks` (one
pending restack per change: its trigger and, after a landing, the commit to
sit on). Both are added in `internal/hub/change.go` after `AnnouncedStep`.
`internal/store` and `internal/server` are unchanged; the new error types
come from `server.NewError`.

## `[changes].max_stack_depth` is a key of the changes section

The key is declared in `internal/config/changes.go` beside
`mergeable_debounce` and tested in `changes_test.go`. It reaches the change
component as `change.Deps.MaxStackDepth`; zero there means the default, 20.

## Ancestry is asked of the target repository

The automatic link and the "is the change behind" questions run in the target
workspace's repository, which holds every version head of its changes,
including those whose source is a fork. A fork's repository would not hold
the heads of another fork's changes.

## A restack replays the commits after the change's base

`gitrepo.Rebase` replays `onto..head`. After the change below was
force-pushed, landed or abandoned, that range still holds the old lower
commits. `(*gitrepo.Repo).RebaseFrom(ctx, base, head, onto, at)` replays
`base..head` through the same replay, and its conflict report's `base_sha` is
`base`, the change's base. A change counts as up to date when its head has
the commit it must sit on and that commit has the change's `base_sha`. Head
ancestry alone is not enough: a dependant of an abandoned change reaches the
new lower head through the abandoned commits.

## A restack moves the branch itself and records one version

A restack does not go through the ref writer. It moves the source branch with
a compare-and-swap inside its own store transaction, records the version
with `RecordVersion` (kind `restack`), and schedules the mergeability checks
of the changes that target the moved branch, as restore does. No push rules
run and no `parley.git.push` is written. If the transaction fails after the
swap, the branch is set back. `VersionInput.SessionChecked` keeps the
replaced version's session without asking the session linker again.
Without a registered linker, that session is dropped.

## `SetBase` measures `patch_equivalent_to` again

The stack linker moves a version's base after package 22 recorded it, so
`SetBase` also recomputes the current version's `patch_equivalent_to`, and
`parley.change.version` is emitted from the version as stored after the
linker ran. A restack that changes no patch is therefore
`patch_equivalent_to` the version it replaces.

## The mergeability check yields to a restack

A change with a pending restack is not checked: its check stores nothing
until the restack ends, and the restack's outcome schedules it again.
Otherwise the check, which the new version below scheduled, would replay the
old lower commits and announce a conflict before the restack runs. A check
also stores nothing for a change that holds the conflict its restack stored,
so the restack's report (the change's own commits) is not replaced by the
check's trial.

A check triggered after that conflict for another reason, such as a push to
the target or a retarget, is not held back. It stores its own trial report:
`mergeable` stays `conflict` and no second event is written, but the
report's `base_sha` and `commit_sha` then describe the check's trial.

## Too deep is counted, not read from the reason

A restack conflict replaces `stack_too_deep` with `conflict` on a too-deep
change until the next link recomputes the depth. So `pendRestack` and the
worker's skip count the change's depth against the limit instead of reading
`restack_reason`. A change back within the limit that already has a pending
restack keeps that record, so the `manual` trigger of an explicit link made
by the same `PATCH` is not replaced.

## The landing entry point has no caller yet

Package 25 (`internal/land`, its after-landing hook list) does not exist.
`(*change.Component).OnLanded(ctx, workspace, number, landed)` runs in a
store transaction of its own, since a hook failure must never change the
land. It is tested directly and registered nowhere until package 25 adds
the hook list.

## The version guard has no registrant yet

`VersionGuard` and `RegisterVersionGuard` are an interface and a method of
`*change.Component`. Nothing registers a guard until package 29 does, so
every manual restack's version passes.

## The manual restack's refusals

- A merge commit among the change's own commits answers
  `400 merge_commits_unsupported`, as the source control rebase route does.
  A background restack logs it and keeps its record.
- `409 ref_moved` carries in `details.current_sha` the source branch head
  read again after the last lost swap.
- `400 target_not_found` is answered when there is no commit to sit on: no
  live `depends_on`, no landed commit below, and a missing target branch.
  A change whose change below landed is still restacked onto the landed
  commit.
- A background restack whose target branch is missing is an error: it is
  logged and kept pending.

## The stack of a fork-bound token

The stack route, and the `include=stack` member that returns the same
answer, list for a token bound to a fork only the changes whose source is
that fork, as `stack_of` does. `stack_of` keeps its own walk.
