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

## Adding scopes to a token also holds a PAT to the token's paths

The package applies the subset rule on `PUT` and `PATCH
/user/tokens/:token_id/permissions` to the scopes the change adds and to
nothing else, since a token's paths cannot change. That leaves a way to
widen a token that its glossary rules out ("a PAT can never create or widen
a token beyond the scopes it holds exactly and the paths its globs cover"):
a PAT holding `tokens:write` and `git:write` with `paths ['docs/**']` could
add `git:write` to another token of its user that has no `paths`, which
could then write anywhere.

The hub closes it. When the change adds at least one scope and the calling
PAT is limited to paths, the target token's `paths` must be covered by the
caller's globs, as the paths of a new token must be: a target without
`paths` is refused with `403 insufficient_scope` (`paths: this token is
limited to paths, so it cannot add scopes to a token that is not`), and a
target with a glob none of the caller's globs covers is refused naming that
glob. The scope half is checked first, as on creation. A change that adds
nothing (a `DELETE`, or a `PUT` that only keeps or drops scopes) is never
refused, and API keys are not held to the rule. The check is
`identity.CheckWidenSubset(caller, added, targetPaths)` in
`internal/identity/subset.go`, which replaces `CheckPermissionSubset`.
