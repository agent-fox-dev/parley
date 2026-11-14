# Errata: 08_workspace_bound_tokens

Where the implementation of package 08
(`.specs/08_workspace_bound_tokens`) differs from what the package says.

## The event emitter takes this package's own event value

The package declares `EventEmitter` in `internal/access` "with the two emit
calls of package 52's event component". Those calls take an `event.Input`
and return an `event.Record`, and `internal/access` cannot name either:
`internal/event` imports `internal/access`, so the import back would be a
cycle. The interface in `internal/access/events.go` therefore takes a value
of its own and returns only the error:

```go
type EventEmitter interface {
	Emit(ctx context.Context, tx *sql.Tx, ev Event) error
	EmitNow(ctx context.Context, ev Event) error
}
```

`access.Event` has the members the two events of this package need (`Type`,
`Workspace`, `ActorType`, `ActorID`, `ResourceType`, `ResourceID`,
`Metadata`), with `""` for a missing workspace or actor where `event.Input`
has a nil pointer. The event component is to be passed behind it through
an adapter in `internal/hub` that converts `access.Event` to `event.Input`.
What the package says of the two calls
otherwise holds: one emits in the caller's transaction, the other in a
transaction of its own, and there is no discarding default.

## The creating token is written by the insert of the workspace record

The package adds "a store function that sets [the creating-token id] when a
workspace row is inserted", for package 15 to call at fork creation. It is
not a function of its own: the id is the member `CreatingTokenID` of
`access.Record`, which `access.InsertRecord` writes to the new column
`workspaces.creating_token_id` in the same statement as the rest of the
record, so a fork can never exist for a moment without its creator.
`InsertRecord` refuses a repository with a creating token
(`access.ErrInvalidRecord`). The column is added by its own schema step,
`access.CreatingTokenStep` (`workspace_creating_token`), after
`access.SchemaStep`, so that a store whose workspace table exists gains it.
`CreatingTokenID` has no JSON name, so no answer that shows a record shows
it.

## The binding reaches the handler attached to the request

The package says the binding guard places the binding "in the request
context", where the handler reads it, and that for an API key, the admin
token and an unbound PAT it places none there. The guard is a
`server.Check`, which cannot hand a new context to the handler, so it
attaches the binding to the request with `server.Attach`, as ADR 04 lets
every check do, and the handler reads it with `access.BindingOf(r)`, which
reports false for every caller but a bound PAT. On an allowlisted route,
which names no workspace, the attached `access.Binding` carries the token
id and the bound slug with no relation; the handler gets the covered set
with `CoveredSet(ctx, access.Caller{TokenID: b.TokenID, Workspace:
b.Workspace})`. On git, which has no pipeline check, `GuardGit` returns the
binding in its `access.GitDecision` instead.

## The subset rule of the mint route runs as an access check too

The package lets `POST /workspaces/:slug/tokens` hold a PAT caller to the
subset rule through the minting component, which the handler calls after
the pipeline's checks, and also asks for the rule's `403
insufficient_scope` "before the workspace is looked up", the same for an
existing workspace and an unknown slug. The handler runs after the access
check, which looks the workspace up and answers `404` for an unknown slug,
and the route's feature check runs before the binding guard, so neither can
place it. The route therefore carries the mark `access.MarkSubsetRule`
(`"access.subset_rule"`), and the access check `(*access.Service).SubsetCheck`,
installed before the workspace access check, decodes the body with
`identity.DecodeMintRequest` (the body parser of `POST /user/tokens`,
exported for this so both routes read one body format) and applies
`identity.CheckSubset` to a PAT caller. A body that does not decode, or
whose paths are not valid, passes, and the handler answers it `400`. The
minting component still applies the rule, through `identity.MintPAT`, for
its other callers.

## The minting component takes the creating credential, whose user owns the token

The package calls the minting component "with an owner, the creating
credential, a workspace slug, ...". `access.MintInput` has no owner of its
own: its `Creator` is the creating credential as an `identity.Principal`,
and the token is owned by `Creator.User`, because the component mints
through `identity.MintPAT`, the one way a PAT is created, which owns the
token by its caller's user and holds a PAT caller to the subset rule. A
caller that mints for a user without a credential of that user passes a
principal with that user and the type of its own credential.

## An archived workspace is refused inside the minting transaction

The package has the mint route's handler validate the body, answer `409
workspace_archived` for an archived workspace, and only then open the
transaction and call the minting component. The body's validation is
`identity.MintPAT`'s and runs as it mints, so the handler opens the
transaction first: the token is validated and written, an archived
workspace is then answered `409` before the `parley.token.bind` event is
emitted, and the transaction is rolled back. The answers are the ones the
package orders: `400` for an invalid body on an archived workspace, `409`
for a valid one, and nothing minted and no event emitted in either case.
