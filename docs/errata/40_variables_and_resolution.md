# Errata: 40_variables_and_resolution

Where the implementation of package 40
(`.specs/40_variables_and_resolution`) differs from what the package says,
or decides what it leaves open.

## Variables are a second collection of the secret component

The package lists files of their own for the key and value rules
(`var_validate.go`) and for scope access (`var_access.go`). The variable
rules are the secret rules, and the PRD specifies the variable routes "as
the secret routes", so the variables are kept by the same
`secret.Component`: the routes reuse its body checks (`decodeEntries`,
`decodeValue`, `CheckKey`, `CheckValue`), its scope check (`ScopeCheck`,
through the mark `secret.MarkScope`), its error mapping and its event
writing. Neither file exists; the routes are in `var_routes.go` and the
store operations in `var_store.go`. The org scope's `404` is the secret
routes' `org not found`.

## The removers take a context and return an error alone

The package writes `RemoveWorkspaceVars(tx, slug)`, returning the ids of
the tokens it revoked, and `RemoveVarScope(tx, scope, scope id)`. The
removers of owned data the workspace service and the identity service run
take the caller's context first and return an error alone
([ADR 03](../adr/03-register-removers-of-owned-data-on-the-identity-service.md),
[ADR 07](../adr/07-register-the-teardown-collaborators-on-the-workspace-service.md)),
so they are `RemoveWorkspaceVars(ctx, tx, slug) error` and
`RemoveVarScope(ctx, tx, scope, scopeID) error`. The secret reader of name
resolution is `Get(ctx, scope, scopeID, name)` (see
[the errata of package 39](39_secrets.md)).

## One registration removes a workspace's and a fork's variables

The package registers `RemoveWorkspaceVars` twice: as a remover of owned
data of the workspace delete (package 14) and as a removal participant of
the fork removal (package 16). A fork's removal runs the workspace
service's teardown set through `(*workspace.Service).RemoveOwned`
(ADR 07; [the errata of package 16](16_task_fork_lifetime.md)), so the
one registration `Teardown().RegisterRemover(secret.RemoveWorkspaceVars)`
in `internal/hub/workspace.go` covers both, and a fork's deletion and expiry
remove its variables in their transaction.

## PATCH and DELETE on the resolved path are routes that answer 405

The package leaves `PATCH` and `DELETE` unregistered on
`/workspaces/:slug/vars/resolved`, so that the router answers `405`. The
router ranks only the routes of the request's method, so such a request
would match the `PATCH` and `DELETE` routes of `…/vars/:key`, with the key
`resolved`. Both are registered on the literal path as public routes whose
handler sets `Allow: GET` and answers `405` `method_not_allowed`, before any
authentication, scope or access check, as the router's own `405` is; the
server's rate limit and body stages still run first.

## Some checks are set up differently

- The check of an event that cannot be written uses the SQLite trigger the
  secret routes' test uses (`RAISE(ABORT)` on `hub_events`) in place of an
  event-writer stub; the event component is the real one.
- The check of an org delete that fails after this package's remover ran
  registers no second remover: `hub.Composition` does not export the
  identity service. It makes the delete fail with SQLite triggers, once on
  `variables` and once on `orgs`, after every remover has run; both are
  answered `500` `internal` and keep the org's variables.
- Changing a workspace's owner has no operation of its own; the check of a
  fork owner who loses write access to the parent updates
  `workspaces.owner_id` in the store.
- The variable test helpers are in `internal/secret/helpers_test.go`, the
  one helpers file of the module, not in a `var_testhub_test.go`.

## A create's exit status is its last failure's

As for `parley secrets create`, `parley vars create` with several scope
flags exits with the status of its last failure: `1` when the hub answered
an error, as the package says, but `2` when the last failure is on the
client's side, such as a hub that cannot be reached. An empty `KEY` for
`delete` and with `--from-stdin`, and an empty workspace slug for
`resolve`, are usage errors.

## The secret resolution has no caller yet

`(*secret.Component).ResolveSecrets` is bound in the hub to the access
service's write-access question and the component's own `Get`, and runs in
the test of the second execution path on a composed hub. The packages that
call it, sandbox creation and the sandbox lifecycle, are not built yet.
