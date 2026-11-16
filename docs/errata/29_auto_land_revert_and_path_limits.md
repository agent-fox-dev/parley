# Errata: 29_auto_land_revert_and_path_limits

Where the implementation of package 29
(`.specs/29_auto_land_revert_and_path_limits`) differs from what the package
says, or decides what it leaves open.

## Composed in `internal/hub`, in one part of its own

The package wires the lookup, the guards, the override rule, the final
observer and the automatic lander "where parleyd wires its collaborators"
and touches `cmd/parleyd`. The hub is composed in `internal/hub` (ADR 01),
and `cmd/parleyd` is not touched. One part, `composeLandRules` in
`internal/hub/landrules.go`, listed after `composePolicyEvaluation`,
registers every collaborator on the instance it plugs into (ADR 08). The
version-files lookup is not registered but bound: the part sets it on the
composition, and the git server's start hook hands it to the push rule
engine in `pushrule.EngineDeps.VersionFiles`, as ADR 08 has the engine's
lookups bound. The lookup is implemented in `internal/land`, not in
`internal/gitserver/pushrule`, which declares the interface: the change
component imports `pushrule`, so an implementation there that reads changes
would be an import cycle.

## The events of an automatic attempt are written by the hub

The land engine writes every land event with the attempt's `requested_by`
as actor. An attempt with trigger `auto_land` names the user who set the
flag as `requested_by` and writes its `parley.change.land` and
`parley.change.land_fail` with actor type `system` and no actor id
(`internal/land/engine.go`, `emit`).

## Write access is the owner's

The access rule has the levels none, member, owner and admin (erratum 21),
and only the owner writes. The checks that name "a member of the
organization with write access" are therefore run with the workspace's
owner (the revert route's accepted callers: the owner's PAT with
`changes:write`) or, where the row stands for a caller who may land but is
not the owner (the override's refusals), with the change's author landing by
`land.by_author`; a plain organization member, who only reads, is answered
`403 forbidden` by the land route before the override guard is asked. The
owner of an override is the access level owner, as design decision 6 says.

## `auto_land` on a landable change starts through a row due now

The `auto_land` guard runs inside the transaction of the open or the
`PATCH`, before it commits, so it cannot start a land itself. When the flag
goes from off to on, the guard writes the lander's row for the change, due
now, in that transaction; the lander's timer runs the start procedure for
it once the transaction has committed, within one interval (1 s). A rolled
back request takes the row with it. A start that succeeds clears a due time
already passed, so the same transition never starts twice, once from the
row and once from `parley.change.ready`.

## A stack's upper change is announced again when the change below lands

Step 6.3 starts the upper change of a stack again when its restack version
is recorded. When the change below lands as its own commits, the target
did not move, the upper change already sits on the landed commit, no
restack runs and no version is recorded; the change component announces a
change once per head, and its one announcement came while the change below
was unlanded and was turned away. So the lander notes, in its row
(`held_by`), the change below that held a start back, and its final
observer, told of every landed attempt whatever its trigger, clears those
notes and has each such change that sits on the landed commit announced
landable again (`ForgetLandable`, then `AnnounceLandable`, in one
transaction). Its land starts from that `parley.change.ready`, which stays
the one start signal. A change that does not sit on the landed commit is
started from the announcement of its restack version.

## `reverted_by` is derived where a change is read

Step 7.9 maintains `reverted_by` through the setter package 22 offers,
updated when a reverting change is opened, abandoned or restored. A change
leaves `draft`, `open` and `landed` in several places (abandon, a deleted
source, restore, a move between draft and open, a land), and a setter
called from each could miss one. Every read of a change derives
`reverted_by` instead, in its column list: the highest number of the
workspace's changes whose `reverts` is the change's number and whose state
is `draft`, `open` or `landed`. `SetRevertedBy` is removed, and the stored
column is no longer read.

## The undo commits are made without a worktree

Step 7.4 undoes the landed commits in a temporary worktree or index. They
are three-way merges in the object store (`gitrepo.(*Repo).Revert`, through
`git merge-tree` with the undone commit as merge base), so there is nothing
to remove after a conflict.

## The revert opens its change for an author who only reads

The open of package 22 asks write access to the source workspace, and the
revert's branch lives in the target workspace, which an author reverting by
`land.by_author` may only read. `change.OpenInput.SourceGranted`, set only
by the revert once it has decided who may revert, has the open ask read
access to the target instead; every other check of the open runs. The
branch creation keeps the caller's facts: the push rules have no level rule
for creating an unprotected branch, so the author passes them as the owner
does, and the push event and the change's author are the caller.

## Who may revert is decided before the session and the state

The package lists the revert's failures after authentication, scope,
access and the lookup, with the body, the session and the states, and
decides who may revert apart from them. The route decides it right after
the body, as the land route decides who may land after it reads its body,
so that a reader who may not revert learns nothing of the change's state
from the answer.
