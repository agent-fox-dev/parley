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
