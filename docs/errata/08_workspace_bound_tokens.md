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
