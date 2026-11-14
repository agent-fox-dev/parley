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

## The start report is written once the hub listens

The package has a start hook write the start report: the `secret
encryption` line and, without a current key, the warning that secrets
cannot be stored. The hub promises that a start that fails before listening
writes exactly one line, and start hooks run before the bind and before the
hooks of later parts, so a line written by the hook would precede the error
line of a later hook or of a bind that fails. So the start hook `secrets`
binds the component and reads the report (`(*secret.Component).StartReport`),
and the hub writes it (`(secret.StartReport).Log`) once the listener is
bound, right before the `listening` line
([ADR 06](../adr/06-load-the-secret-keys-before-the-stores-open.md)). The
lines, their members and the count are the ones the package specifies; a
start that fails after the hook has run writes no report.
