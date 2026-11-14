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

## A converter is given the keys, and a store failure writes no re-encryption line

The package has the re-encryption run converters that re-seal a part's own
values and return `(converted, remaining)`. A converter needs the keys the
hub loaded to open and seal those values, and the component keeps them to
itself, so a `secret.Converter` is `func(ctx context.Context, keys
secret.Keys) (converted, remaining int, err error)`: it is given the
component's keys, and converts nothing when either the current or the
previous key is absent, as the task does for the secrets.

The package ends every run of `secret_reencryption` with the line `secret
re-encryption`. A run that a store failure stops returns the error before
the converters run and before the line is written: the counts of a store
that failed mid-run are not known, and the maintenance loop logs the error
as `maintenance task failed` with `task` `secret_reencryption`. Every run
that the store does not stop writes the line, one that a converter's error
fails included.

## A create body is held to the server's body limit first

The package checks a create's values against the limit of 262,144 bytes
each and leaves the size of a request body to the server. The secret routes
keep the server's body limit, `[server].max_body_size`, 1 MiB (1,048,576
bytes) by default, and the server reads the body before the create checks
it. So a create whose body exceeds that limit is answered `413`
`payload_too_large` (`request body exceeds 1048576 bytes`) before any entry
is checked, although each of its values is within 262,144 bytes: four values
of 262,144 bytes in one request are refused that way. One value of 262,144
bytes fits, and is stored.

## A create's exit status is its last failure's

The package has `parley secrets create` with several scope flags exit with
status `1` when a scope failed. The command sends to every scope, prints the
results of those that succeeded, writes the error of each failed scope to
stderr once and ends with the last failure, so the exit status is that
failure's: `1` when the hub answered an error, as the package says, but `2`
when the last failure is on the client's side, such as a hub that cannot be
reached or an answer without items.

## An empty KEY is refused as a usage error for delete and --from-stdin

The package lists the malformed command lines that exit `2` with a
`usage_error`. The CLI also refuses, in the same way and before any request,
an empty KEY for `parley secrets delete` (`delete ''`), which would leave the
route's last segment empty and so name the list route, and an empty KEY with
`--from-stdin` (`create '' --from-stdin`, `update '' --from-stdin`), which
names no secret.
