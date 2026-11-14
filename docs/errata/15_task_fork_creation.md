# Errata: 15_task_fork_creation

Where the implementation of package 15 (`.specs/15_task_fork_creation`)
differs from what the package says, or decides what it leaves open.

## `[forks].max_per_workspace` is at least 1

The package gives `[forks].max_per_workspace` its default, 200, and says
nothing about the values a file may set. A value below 1 would refuse every
fork of every workspace, so the loader refuses it like any other value out
of range: the start stops with one error line naming
`forks.max_per_workspace` (`must be at least 1`).

## The ref view, the head and the write check are methods of the fork component

The package names `Refs(fork slug)`, `HeadSHA(fork slug)` and
`CheckBranchWrite(workspace slug, branch)` as functions. They are methods of
`fork.Component`, which the hub binds to its stores, its workspace service
and its git, and each takes a `context.Context` first; a part reaches them
through `Composition.Forks()`. Where the package is silent: `Refs` and
`HeadSHA` of a slug that names no fork (a repository workspace, or no
workspace) return an error matching `fork.ErrNotFork`, and
`CheckBranchWrite` answers nil for a slug no workspace has, as for a
repository workspace, since only a fork has tracking branches.

## Tracking names are columns of the workspace record

The names of the parent's branches and tags at a fork's creation are stored
in two columns, `tracking_branches` and `tracking_tags`, added to the table
`workspaces` by the schema step `fork_fields`, each a JSON array of names,
rather than in a table of their own, so that they are removed with the
fork's record by whatever removes it. The base branch is the record's
`default_branch`.

## Creates of one parent's forks run one at a time

The package makes a fork's repository before the transaction and has a
create discard a directory found at the new fork's place without a record;
it leaves to the store's single writer that concurrent creates neither share
a name nor pass the limit. The single writer serialises the transactions
only, not the directory work before them, so one create could discard the
repository another create of the same name had just made. The create
therefore holds an in-process lock per parent slug from the name check to
the commit: under it, a name a workspace has is refused (`409 conflict`) or,
when generated, drawn again; only then is whatever lies at the fork's place
removed as a leftover and the repository made. A duplicate slug at the
insert is still answered `409 conflict`.

The fork limit, which the package counts in the transaction before the
insert, is counted under the same lock before the name check as well, so
that a parent at its limit is refused (`429 fork_limit_reached`) before a
repository directory is made; the count in the transaction stays.

## The parent's state is judged twice

The package judges the parent's state once, at its place in the order of
refusals. The create judges it there, before the repository is made, and
again on the parent's record read inside the creation transaction, with the
same `workspace.RequireReady`, so that a reclone, archive or delete whose
maintenance marker was set between the two refuses the create (`409
workspace_busy`, or the answer of the state it left) instead of racing it.

## Test seams of the create are fields of `fork.Deps`

The fault injection the package asks for (the repository creator, the
store's commit, the event writer) and the injectable name generator are
fields of `fork.Deps`: `CreateRepo`, `Transact`, `Events` and `NewName`,
with the production values the start hook `forks` sets (the hub git's
`CreateFork`, `Store.Tx`, the event component) or `Bind` fills in (the
`crypto/rand` generator). A test changes them with
`(*hub.Composition).AdjustForks`, as `AdjustWorkspace` does for the
workspace service.
