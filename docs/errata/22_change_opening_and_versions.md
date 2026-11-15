# Errata: 22_change_opening_and_versions

Where the implementation of package 22 (`.specs/22_change_opening_and_versions`)
differs from what the package says, or decides what it leaves open.

## The collaborators are registered on the change component

The package declares registration functions for the stack linker, the
landing evaluator, the version observers, the `auto_land` guard, the fork
activity recorder, the session linker and the include expanders. Each is a
method of `*change.Component` (`RegisterStackLinker`,
`RegisterLandingEvaluator`, `AddVersionObserver`, `RegisterAutoLandGuard`,
`RegisterForkActivity`, `RegisterSessionLinker`, `RegisterExpander`),
reached through `Composition.Changes()`, as ADR 08 has every collaborator
registered on the instance that calls it: a package-level registry would be
shared by every hub of a test binary. Until package 49 registers a session
linker, an explicit `session_id`, and a session given to `RecordVersion`,
is `400 unknown_session`, and a defaulted session is dropped.

## The fork activity recorder takes the caller's transaction

`fork.RecordActivity` opens a store transaction of its own, and the store
runs write transactions one at a time, so calling it inside the transaction
of an open or a version would wait forever. `change.ForkActivity` is
`RecordActivity(ctx, tx, slug)`, and the hub's adapter calls
`(*fork.Component).RecordActivityIn`, the same update in the caller's
transaction (`internal/fork/early.go`); `RecordActivity` is now a
transaction of its own around it. A recorder error is logged and changes
nothing.

## A version of a fork's branch is measured in the fork's repository

The merge base, the stats, the touched paths and the patch id of a version
are computed in the source workspace's repository, which for a fork reads
its parent's objects through its alternates, so nothing is written to the
parent before the open or the version is accepted. The head is brought into
the parent's repository only when the version ref is written
(`gitrepo.(*Repo).SetRefFrom`, which pushes the commit from the fork's
repository).

## A draft still runs the stack linker and the observers

A draft's landing is `{blocked, draft}` and the landing evaluator is not
asked for it; the stack linker and the version observers still run at open
and for every version of a draft, as design decision 32 has them run for
every version.

## The path limit of a path-scoped token is checked on the REST open only

`POST …/changes` holds a path-scoped token to the files of version 1
against its base (`OpenInput.CheckPaths`). An open through the source
control API's commit route, or later through a git push, does not check
them again: the write that moved the branch was held to them by the push
rules of package 18, and the package's non-goals keep its own path check to
the REST route.

## A bound token opens only from what its binding covers

A token bound to a fork, on the fork's parent, opens only from that fork
(`403 token_bound`). A token bound to a workspace, used on it, that names a
source workspace its binding does not cover is answered `400
source_not_related`, as a workspace it cannot see, so a binding never widens
what the token reaches (`internal/change/authorize.go`).

## Deleting a workspace starts its numbering again

`WorkspaceDeleted` deletes the workspace's number counter with its changes,
as the package's step 5.4 says. A later workspace of the same slug is
another workspace and numbers its changes from 1; a number is never given
twice while the workspace exists. The event log keeps the old
`<slug>#<number>` references.

## Deleting a source branch tells the fork activity recorder

A change abandoned by `SourceDeleted` is moved through the setter
(`SetAbandoned`), so a fork source's last activity is recorded, as design
decision 25 has every move to abandoned through this package's setter do.
`ForkDeleted` tells no recorder: the fork is going.

## The list orders by the time it shows

`updated_at` is stored in nanoseconds and shown in whole seconds. The list
orders by the shown seconds, descending, then by number, descending, so the
order is that of the values the caller reads and the tie-break of design
decision 19 holds among them.

## `stack_of` keeps the other filters

`stack_of` ignores `sort` and every `state` filter, as design decision 19
says. The other filters still apply to the members of the stack
(`stack_of=5&source_workspace=demo--f1` lists those of the stack whose
source is that fork), which also keeps a fork's slug and a fork-bound token
confined on a stack.

## A changed file the same at both heads

Design decision 21 gives a changed file the patch from its content at the
head of `against` to its content at the head of `version`. A file whose
patch against its own base differs between the two versions while its
content is the same at both heads (only the bases differ) is answered as
`changed: true`, status `modified`, 0 insertions and 0 deletions,
`old_sha` and `new_sha` both the blob at the heads, and an empty patch; it
counts in `stats.files_changed`.

## A rename between the two heads is one entry of the interdiff

The interdiff describes a changed file by the diff of the two heads, which
finds renames. When that diff pairs a changed file with another path as a
rename, the rename's one entry (`renamed`, with `old_path`) describes both
paths, as the plain diff does: it is listed at its new path when that is a
changed file, else at its old path, and the old path has no entry of its
own (version 1 edits `x.txt`, version 2 renames it to `y.txt` with the
same edit: `?version=2&against=1` lists `y.txt` renamed from `x.txt`). A
copy's old path is still at the head of `version` and keeps its own
entry. Only a changed file the heads' diff does not name is the same at
both heads; when that diff cut its list of files, such a file is left out
of the cut answer rather than called the same.

## Patch equivalence compares what the interdiff compares

Design decision 14 named `git patch-id --stable` over the version's diff.
That ignores white space, which the interdiff, comparing each file's patch
without its `index` line and hunk positions, does not: a version could be
`patch_equivalent_to` one whose interdiff against it shows a changed file.
The patch id a version stores is instead a SHA-256 over the path and the
compared patch of each file of its diff against its own base, in path
order (`empty` for a diff that changes nothing), computed by Open,
RecordVersion and SetBase. Two versions are patch-equivalent exactly when
the interdiff of one against the other changes no file; a rebase that
applied cleanly keeps the patch id, while a version that only changes the
white space of a line does not. `git patch-id --verbatim` would also keep
white space but needs a newer git than the project's floor (2.40), and the
interdiff's comparison is the one the criterion holds the two to.

## A title of white space only is empty

A title, given or taken from the head commit, that holds only white space
is `400 invalid_request` (`title: must not be empty`), as an intent of
white space only counts as no intent.

## The workspaces' state is checked before the source's relation

After the caller's access, an open reads the target and the source
workspace and answers `409` for the first one that is not usable (busy,
not ready or archived) before it judges whether the source is the target
or a fork of it, and before the branches. A source workspace the caller
may write (the owner's, or any with the admin token) that is unrelated to
the target and archived is answered `409 workspace_archived`, not `400
source_not_related`; one the caller cannot see is still `400
source_not_related`, and one the caller reads but may not write `403
forbidden`, both answered by the access check before any workspace is
read.

## The commit object's change may have no version

`scm.CommitChange.Version` is a pointer: a commit a change landed whose
landed-commit record names no version head of the change answers `version:
null` on the source control API's commit objects, as on
`GET …/changes/by-commit/:sha`.

## The change a commit opens takes the commit's session

`scm.ChangeOpening` gained `SessionID`, which the commit route passes, so a
change the commit route opens records the commit's session on version 1 and
links it through the session linker (design decision 34).

## `parley change diff --version` takes `latest`

`--version` takes a positive integer or `latest`, the two forms the diff
route takes; any other value is a usage error before any request.
