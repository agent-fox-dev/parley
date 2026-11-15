# Errata: 21_scm_write_api

Where the implementation of package 21 (`.specs/21_scm_write_api`) differs
from what the package says, or decides what it leaves open.

## The write gate grants the owner and the admin token, since no level is "write"

The package's checks speak of a caller with write access, beside callers
with no access and with read access only. The access rule has the levels
none, member, owner and admin, and no write level. Each write route
declares the PAT scope `git:write` and the access need `write`
(`internal/scm/write.go`, `writeRoute`), so the shared checks answer in the
conventions' order before the handler runs: `403 insufficient_scope` to a
PAT without `git:write` (also for a slug no workspace has), `404 not_found`
to a caller with no access, `403 forbidden` to an organization member, who
reads only. The owner and the admin token pass. The state part of the gate
then refuses the workspace with `workspace.RequireReady` and, for a fork,
with the fork component's parent-state check for a write
(`Deps.WriteParentState`, bound in `internal/hub/scm.go` to
`CheckParentState(…, fork.UseWrite)`).

## The change and session linkers are registered on the component

The package declares a registration function for `ChangeLinker` and the
same for `SessionLinker`. Both are registered on the instance that calls
them, `(*scm.Component).RegisterChangeLinker` and `RegisterSessionLinker`
(nil removes one), reached through `Composition.SCM()`, as ADR 08 has every
collaborator of a write registered on the instance that calls it: a
package-level registry would be shared by every hub of a test binary.
`SessionLinker.Check(ctx, caller identity.Principal, sessionID, slug)
(string, error)` has the shape of `fork.SessionLinker.Check`: the
lower-case id, an error matching `scm.ErrUnknownSession` for a refusal
(`400 unknown_session`), any other error for a failure (`500 internal`).
It only checks; this package records no session link.

Until package 22 registers a change linker, no branch is a change's
source, a commit with a `change` object answers `400 invalid_request`
(`this hub cannot open a change: no change component is registered`), and
a commit to a task fork whose `auto_change` is true (the default) is kept
and answers `change` null with that `change_error`. Until package 49
registers a session linker, every `session_id` is `400 unknown_session`.

## The hub's committer identity is `parley <parley@localhost>`

The package says the hub commits with the identity `internal/gitrepo` uses
for every commit the hub makes, and adds no setting for it. No such
identity existed; `gitrepo.HubName` and `gitrepo.HubEmail`
(`internal/gitrepo/treewrite.go`), the identity the trial rebase's objects
already carried, are now the committer of every commit and rebase the
source control API writes, and the author of a commit the admin token
makes without an `author`. A commit's message is stored with a final
newline, as git stores one, so the commit object answers `"m\n"` for a
message `m`.

## A fork's tracking branch is refused by the push rules

The package has the API refuse a write to a tracking branch of a task fork
with `403 tracking_branch` naming the fork's base. The refusal is the push
rule engine's rule 3, as for a git push; the API does not call the fork
component's branch-write check itself. A write reads the ref it moves
through the workspace's ref view (`(*target).lookup` in
`internal/scm/refupdate.go`), so in a fork a tracking branch, which the
fork's repository may not hold, is found at its parent's commit and reaches
the engine, which refuses it; `details.base_ref` is the fork record's
`fork_point.ref`. The rules run in their order, so deleting the fork's
default branch, which is both protected and tracking, answers rule 2's
`protected_branch`.

## An explicit change is checked between the rules and the ref transaction

The package checks the opening of an explicit change after the push rules
and before the compare-and-swap. The check runs after `Evaluate` allowed
the update and before `Apply` (`refUpdate.beforeWrite`), not inside the ref
writer's locked transaction, so no change component is called while refs
are locked. A compare-and-swap that then fails answers `409 ref_moved` and
opens nothing.

## `[scm].max_commit_size` is applied by the start hook

The commit route's body limit is `[scm].max_commit_size`, but the routes
are registered when the hub is composed, before the configuration loads.
The route is registered with the default, 32 MiB
(`scm.DefaultMaxCommitSize`), and the scm start hook, which runs after the
configuration loads and before the server installs the routes, sets the
configured limit (`scm.LimitCommitBody`).

## Session ids on events are UUIDs

The package's checks use the session id `s-1`. The event log stores only
session ids of the UUID form (`event.Input.SessionID`), so a session the
linker accepts is carried in the lower-case form it returns, and the tests
use UUIDs.

## A ref name in the way of a create is `409 ref_exists`

The ref writer answers a create whose name another ref's name is in the
way of (`refs/heads/feat` beside `refs/heads/feat/x`, either way round) as
a moved ref (erratum 18). A `409 ref_moved` carries the head to retry from,
and here there is none: no retry creates the ref while the other one
stands. When the ref writer answers `ref_moved` for a create and the name
still does not exist, the write asks `gitrepo.(*Repo).RefInTheWay` for the
ref in the way and, when there is one, answers `409 ref_exists` naming it
(`ref refs/heads/feat/x is in the way of refs/heads/feat; delete it to
create refs/heads/feat`), the error type the ref creation already gives to
a name that cannot be created (`refMovedNow` in
`internal/scm/refupdate.go`). This holds for `POST …/scm/refs` and for a
commit that creates its branch. A git push of the same create keeps the
ref writer's answer.

## A direct push names the version it recorded

The package answers `change` `null` for every direct push of a protected
branch, whatever a ref-updated stage reported. A protected branch that is
the source of a draft or open change gets a version when it is moved
directly (see erratum 22, *A direct push to the source of a change records
its version*), so a commit and a rebase that are direct pushes name that
version, as every other write does (`refUpdated.change()` in
`internal/scm/refupdate.go`). A direct push still asks the change linker
nothing and opens no change.
