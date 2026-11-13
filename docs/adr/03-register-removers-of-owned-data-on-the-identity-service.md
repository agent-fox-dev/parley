# 03. Register removers of owned data on the identity service

## Status

Accepted.

## Context

Deleting a user or an org is one transaction of the store: the record, its
memberships and everything else that belongs to it go together, or nothing
goes. Packages built later keep data of their own for a user or an org,
such as secrets and variables, and that data must go in the same
transaction. `internal/identity` cannot import those packages, because they
are built on it, so they must hand it a function that removes their data.

The plan named two package-level functions, `identity.RegisterUserRemover`
and `identity.RegisterOrgRemover`, taking a function of a store transaction
and an id. A package-level list is shared by every identity service in a
process. The identity tests run in parallel, each with a test hub and a
service of its own; a test that registers a remover which fails would make
the deletes of every other test running beside it fail too, and a remover
registered by one hub would be called with another hub's transaction.
A remover also needs a context for `(*sql.Tx).ExecContext`.

## Decision

The removers are registered on the `*identity.Service`, not in the package:

- `identity.Remover` is `func(ctx context.Context, tx *sql.Tx, id string)
  error`, where `tx` is the transaction of the delete and `id` the id of the
  user or org being deleted.
- `(*identity.Service).RegisterUserRemover(fn)` and
  `(*identity.Service).RegisterOrgRemover(fn)`, in
  `internal/identity/removers.go`, keep the removers in registration order.
  Registration is safe from several goroutines, and is meant to happen
  while the hub is composed, before any request.
- The delete of an org calls every org remover, in order, with its
  transaction and the org's id, after it has found the org and before it
  deletes the memberships and the org. A remover's error stops the delete,
  rolls the transaction back, everything earlier removers wrote included,
  and is answered `500 internal`, whatever error the remover returned. A
  delete that finds no org calls no remover. An org delete calls no user
  remover, and the delete of a user calls the user removers and no org
  remover, because the user's orgs are kept.
- `hub.Composition` keeps the service `composeIdentity` creates, in the
  unexported field `identity`. A part listed after `composeIdentity` in
  `internal/hub/hub.go` that keeps data for a user or an org registers its
  remover through it, for example
  `c.identity.RegisterOrgRemover(secrets.RemoveOrg)`.

## Consequences

- Each service, and so each test hub, has its own removers; a test
  registers removers on the service of its hub and affects nothing else.
- A package that keeps data for a user or an org adds nothing to
  `internal/identity`: it writes its remover and registers it from its part
  in `internal/hub`.
- Registration depends on the part order of `internal/hub/hub.go`: a part
  that registers a remover must come after `composeIdentity`.
