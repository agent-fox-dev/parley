# 07. Register the teardown collaborators on the workspace service

## Status

Accepted.

## Context

Archiving a workspace ends its agent sessions and those of its forks, and
tells the features that act on an archived workspace, such as the one that
stops its sandboxes. Deleting a workspace revokes the tokens bound to it
and to its forks, deletes their sandboxes, and removes, in one store
transaction or not at all, the data more than ten other packages keep per
workspace: secrets, variables, changes, policy, claims, patches, sessions
and more. Most of those packages are built after `internal/workspace`, on
top of it, so `internal/workspace` cannot import them; they must hand it
what removes or ends their data. Forks are removed by the task-fork
feature, which also removes a fork's owned data when the fork is deleted
on its own or expires, and must do so the same way a delete does.

As for the identity removers of ADR 03, a package-level registry would be
shared by every hub in a test binary: a test that registers a failing
collaborator would make the operations of every other test beside it fail.

## Decision

The collaborators are one set, `workspace.Teardown` in
`internal/workspace/teardown.go`, held by the `*workspace.Service` and
returned by `(*Service).Teardown()`:

- `TokenRevoker`, `RevokeBoundTo(ctx, tx *sql.Tx, slugs []string)
  ([]string, error)`, revokes the live tokens bound to the slugs in the
  caller's transaction and returns their ids. `*access.Service` satisfies
  it.
- `Remover`, `func(ctx context.Context, tx *sql.Tx, slug string) error`,
  removes one package's data for one workspace in the caller's
  transaction. `secret.RemoveWorkspace` is one.
- `SessionCloser`, `CloseSessions(ctx, tx *sql.Tx, slugs []string, reason
  string) error`, ends the active sessions of the slugs in the caller's
  transaction.
- `SandboxRemover`, `RemoveSandboxes(ctx, slugs []string) error`, deletes
  the sandboxes of the slugs, outside any transaction.
- `ArchiveObserver`, `WorkspaceArchived(ctx, slug string, forks []string)
  error`, is told after an archive has committed.
- `ForkRemover`, `DeleteFork(ctx, slug, caller identity.Principal)` and
  `RemoveForksOf(ctx, tx, parent, actor identity.Principal) (slugs
  []string, discard func(), err error)`, the one interface to the
  task-fork feature.

The token revoker, the session closer, the sandbox remover and the fork
remover are one each: `RegisterTokenRevoker`, `RegisterSessionCloser`,
`RegisterSandboxRemover` and `RegisterForkRemover` replace the one before,
and nil removes it. Removers and archive observers are lists:
`RegisterRemover` and `RegisterArchiveObserver` add after those before,
and are called in registration order. Registration is safe from several
goroutines and happens while the hub is composed, before any request. An
operation takes the set once, at its start. The zero value is valid: with
nothing registered an archive and a delete touch the workspace records
alone.

`(*Service).RemoveOwned(ctx, tx, slug, reason)` is the one way the owned
data of a workspace or a fork is removed: it calls the token revoker with
the slug, every remover in order and the session closer with the slug and
the reason, stops at the first error, which the caller rolls the
transaction back on, and returns the revoked token ids in ascending order.
The task-fork feature removes a fork's data through it and keeps no
registry of its own.

An archive calls the session closer inside its transaction under a
savepoint, so that a closer that fails has its writes undone and is
logged while the archive commits; it calls the archive observers after
the commit, logging an error and going on. It calls no remover, no token
revoker and no sandbox remover.

The workspaces part in `internal/hub/workspace.go` registers the
collaborators of the packages composed before it: `c.access` as the token
revoker, since tokens have no remover of their own, and
`secret.RemoveWorkspace` as a remover, since `internal/secret` cannot
import `internal/workspace` to register itself. Every other package that
keeps data for a workspace, or acts when one is archived or deleted,
registers its collaborator on `c.Workspaces().Teardown()` from its own
part in `internal/hub`.

## Consequences

- Each service, and so each test hub, has its own set; a test registers
  stubs on the set of its hub, through `hubtest.Config.Compose`, and
  affects nothing else.
- A package that keeps data per workspace adds nothing to
  `internal/workspace`: it writes one remover and registers it, and is
  then called for every way a workspace or a fork goes away.
- A delete is one transaction across every owner's data: a remover's
  error rolls back everything, the workspace records included.
- Registration depends on the part order of `internal/hub/hub.go`: a part
  that registers a collaborator must come after `composeWorkspaces`, which
  creates the service.
