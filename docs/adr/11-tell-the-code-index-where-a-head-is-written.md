# 11. Tell the code index where a head is written

## Status

Accepted.

## Context

The code index (package `internal/codeindex`) holds the head commit of a
workspace's default branch and must follow it whatever moves it. The hub
writes that head in several places that share no code: the ref writer (a
land, a git push, the source control API), the clone that makes a workspace
ready (the first one and a reclone), a sync, and a rebuild of a carry-patch
workspace. The index must also react when a workspace's setting
`code_index` changes, and no event carries a setting's value.

A single place to learn of every move does not exist, and a poll alone
would make a search wait up to a maintenance interval for a commit that
landed a second ago.

## Decision

Each place that writes a head tells the code index through the notice that
place already offers; where it offers none, it gains the smallest one, and
no more. The component judges every notice itself: it queues nothing for a
fork, for a branch that is not the default branch, or on a hub without
`[code_index]`, so the callers stay unaware of its rules.

- **The ref writer** (ADR 08): a post-push listener reports every branch
  an accepted write moved (`BranchMoved`).
- **A clone**: a `workspace.CloneObserver` queues the build in the
  transaction that makes the workspace ready (`QueueIn`), for the first
  clone and for a reclone.
- **A sync**: a `workspace.BranchMoveObserver` reports each branch it
  moved. It runs before the workspace's record has the new head; a build
  reads the head from the repository, not from the record.
- **A rebuild**: `carrypatch.Deps.AfterRebuildPublish`, added for this, is
  told the workspace and the integration branch once the publish has
  committed. A rebuild moves the integration branch, so it queues a build
  only where that branch is the default branch.
- **The setting**: `workspace.Service.RegisterUpdateObserver`, added for
  this, is told of every committed update with the record before and
  after. Off to on queues a build; on to off cancels the waiting builds
  and interrupts the running one.
- **Everything else**: the maintenance task and the start hook compare
  every indexed workspace's record with its head and queue what is behind.
  This is what makes the index follow a head written by a route added
  later, or whose notice was lost with a crash, and what removes the
  directories of workspaces that are gone or turned off.

The job queue gained what a build needs and no job had: `Limit` (the most
attempts of one type at once, for `[code_index].workers`), `Hasten` (a
failed job due at once, when the head it failed on moved), and
`CancelPending` with `Interrupt` (the jobs of one type withdrawn for a
workspace that stays).

## Consequences

- A new place that writes the head of a default branch outside the ref
  writer should tell the component; one that does not is still followed,
  within a maintenance interval.
- A notice is a hint, never the truth: a build always reads the head when
  it starts, and a build that ends with the head elsewhere queues the
  next. Notices may be lost, doubled or late without the index holding a
  wrong commit under a right name.
- The update observer and the rebuild notice run after their commits and
  outside any transaction; their failure is logged and changes nothing in
  what they follow.
