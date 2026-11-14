# Errata: 15_task_fork_creation

Where the implementation of package 15 (`.specs/15_task_fork_creation`)
differs from what the package says, or decides what it leaves open.

## `[forks].max_per_workspace` is at least 1

The package gives `[forks].max_per_workspace` its default, 200, and says
nothing about the values a file may set. A value below 1 would refuse every
fork of every workspace, so the loader refuses it like any other value out
of range: the start stops with one error line naming
`forks.max_per_workspace` (`must be at least 1`).
