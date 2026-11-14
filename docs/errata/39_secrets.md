# Errata: 39_secrets

Where the implementation of package 39 (`.specs/39_secrets`) differs from
what the package says.

## The component's operations take a context first

The package writes the internal operations without a context:
`Get(scope, scope_id, name)`, `Put(tx, …)`, `Delete(tx, …)`,
`RemoveScope(tx, scope, scope_id)` and `RemoveWorkspace(tx, slug)`. Every
store operation of the hub takes the caller's `context.Context` first, and
the removers of owned data the identity service runs
(`identity.Remover func(ctx context.Context, tx *sql.Tx, id string) error`)
pass one. So the operations of `internal/secret` take it first:

- `(*Component).Get(ctx, scope, scopeID, name) (string, bool, error)`
- `(*Component).Put(ctx, tx, scope, scopeID, name, value) error`
- `(*Component).Delete(ctx, tx, scope, scopeID, name) (bool, error)`
- `(*Component).RemoveScope(ctx, tx, scope, scopeID) error`
- `RemoveWorkspace(ctx, tx, slug) error`

The behaviour is the one the package specifies. A value that no configured
key opens makes `Get` return an error matching `ErrUnreadable` together with
`found == true`, so a caller that looks only at `found` still does not treat
the secret as missing.
