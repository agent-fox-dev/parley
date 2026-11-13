# Errata: 06_api_keys_and_access_tokens

Where the implementation of package 06
(`.specs/06_api_keys_and_access_tokens`) differs from what the package says.

## The hub-internal token operations take a context and the instant

The package names the two operations hub-internal callers use on one token
`RevokeTokenByID(tx, token_id)` and `SetTokenExpiryByID(tx, token_id,
instant)`. They are, in `internal/identity/token.go`:

```go
func RevokeTokenByID(ctx context.Context, tx *sql.Tx, tokenID string, now time.Time) error
func SetTokenExpiryByID(ctx context.Context, tx *sql.Tx, tokenID string, at time.Time) error
```

Every store function of `internal/identity` takes the request's
`context.Context` first, so that a cancelled request stops its statements,
and these two follow it. The revocation also takes the instant it records:
the clock belongs to the caller, as it does for `IssueAPIKey`, `StorePAT`
and `MintPAT`, so the caller's change of a sandbox and the revocation of its
hub credential carry the one time the caller read, and a test moves that
time without a clock inside the store. What the package says of them
otherwise holds: both write through the given transaction and do not
commit, check no owner and write no event, a revoked token keeps its
`revoked_at`, the expiry update changes `expires_at` (kept to whole
seconds) and nothing else, and an unknown id is `identity.ErrNotFound`
(compare with `errors.Is`). Packages 45 and 47 call these forms.
