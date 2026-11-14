# Errata: 16_task_fork_lifetime

Where the implementation of package 16 (`.specs/16_task_fork_lifetime`)
differs from what the package says, or decides what it leaves open.

## The pending discards are a schema step of `internal/fork`

The package places the table of pending discards in `internal/store`. Every
feature's tables are schema steps of the feature's own package, registered
by its part of the hub, and so is this one: `fork.DiscardStep`
(`fork_discards`: `slug` primary key, `parent_slug`, `recorded_at` in
UnixNano), registered in `internal/hub/fork.go` after `fork.SchemaStep`.
`internal/store` is unchanged.

## A fork's own branches are discarded with its repository

A fork's own branches live only in the fork's own repository, at its
`RepoPath`, which shares the parent's objects and holds no ref of the
parent. Discarding them after the removal's commit is removing that
directory (`fork.Deps.RemoveRepo`, `gitrepo.Remove` by default); the
parent's repository is never written. The removal holds the parent's create
lock from its transaction to the discard, so that a create of a fork of the
same name, which replaces whatever it finds at the fork's place, never has
its new repository discarded.

## A fork stays removable whatever its parent's clone state

The package lets a `DELETE` of a fork through while the parent is `ready` or
`archived` and refuses it with `409 workspace_busy` while the parent is
`pending` or `cloning` or carries a maintenance marker; it says nothing of a
parent whose clone has `failed`. Only the three busy states refuse the
removal; every other parent status, `failed` included, lets the fork go,
since a fork must not outlive its usefulness because its parent cannot be
cloned. The check is made before the sandboxes are deleted and again on the
parent read through the removal's transaction. A removal with the parent
does not judge the parent, whose delete is running.

## The sandbox collaborator is registered on the fork component

The package has `internal/hub/fork.go` pass the sandbox collaborator and
the teardown set into the fork component. The component's collaborators
are registered on it while the hub is composed (ADR 07), as the create's
are: the sandbox feature calls `(*fork.Component).RegisterSandboxes` with
its `fork.SandboxLifecycle` (`DeleteSandboxes(ctx, slug, reason,
checkpoint)`, `HasLiveSandbox(ctx, slug)`) from its own part of the hub.
None is registered while the hub has no sandbox feature, and a fork then
has no sandboxes. The teardown set needs no passing: the component reaches
it through the workspace service it already holds, whose `RemoveOwned`
runs that set.

## A failed removal is answered `internal error`

`DeleteFork` answers every failure other than the not-found result and the
busy parent with `500 internal` and the message `internal error`, as the
hub answers any internal failure, and logs the cause (`fork not removed`,
with `slug` and `error`), so that no detail of a remover's failure reaches
the caller.

## The handler of `ttl` is a method of the fork component

The package registers a handler for `ttl` with package 11's
`RegisterForkField` and has it call `ResolveTTL` at the request time. A
`workspace.ForkFieldHandler` receives the fork and the raw value only, and
`ResolveTTL` needs the hub's clock and the `[forks]` limits, which exist
only once the hub has started. The handler is therefore
`(*fork.Component).TTLField`, registered in `internal/hub/fork.go` while
the hub is composed and reading `Deps.Now` and `Deps.Limits` of the bound
component each time it is called. Its refusal of a value that is not a
string (`null`, a number, a boolean) is `ttl: must be a string`; a string
`ResolveTTL` refuses is answered with `ResolveTTL`'s own `400`.

## The sweep's maintenance task is `fork_expiry`

The package names the maintenance task of the sweep `fork expiry`. Package
`internal/maintenance` already orders its known tasks by name, and the name
it holds for this one is `fork_expiry`; the task is registered under that
name, with `Composition.AddMaintenanceTask` in `internal/hub/fork.go`, so
that it runs in its place in a pass. `internal/maintenance` is unchanged:
the sweep's loop is background work of the hub
(`Composition.AddBackgroundWork`, `fork_sweep`), not a part of that
package.

## A fork given a new lifetime during a pass is not expired

The package has a pass remove every fork whose `expires_at` was at or
before its time when it read them. A `ttl` update can land between that
read and the fork's removal. The removal therefore reads the fork again,
before its sandboxes are deleted and inside the removal's transaction, and
leaves it alone when its `expires_at` is no longer at or before the pass's
time; the pass writes no event for it and logs nothing.

## A pending discard yields to a fork created again under the slug

The package has every later pass repeat a failed discard until it succeeds.
A fork may be created again under the removed fork's slug before then; its
create replaces whatever it finds at the fork's place with its own
repository. A pending discard is therefore repeated only while no workspace
record has the slug, under the create lock of the parent, and its record is
dropped without a removal once one does, so that the new fork keeps its
repository.

## The removal a shutdown interrupts runs on to its end

The package has the sweep complete or roll back the removal in progress
when the hub stops. The pass checks its context before each fork, and the
removal of a fork it has begun, the sandbox deletion included, runs on a
context the stop does not cancel, so that it reaches its commit or
rollback and its discard. The hub waits for the sweep up to 10 seconds
(`fork_sweep`'s grace) before it closes the stores.
