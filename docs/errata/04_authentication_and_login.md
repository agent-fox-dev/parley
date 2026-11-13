# Errata: 04_authentication_and_login

Where the implementation of package 04
(`.specs/04_authentication_and_login`) differs from what the package says.

## The caller's detail travels in server.Caller.Detail

The package says authentication reports the whole caller to the routes
behind it: the credential type, the credential id, the user with role and
status, and a PAT's permissions, paths and workspace. `server.Caller`
carries only `CredentialID`, `UserID` and `Admin`, and an
`server.Authenticator` cannot change the request's context, so
`server.Caller` gained one field, `Detail any`: what the authenticator knows
beyond the ids, which the server does not read
(`internal/server/pipeline.go`). The identity service puts a pointer to its
`identity.Principal` there, and `identity.PrincipalOf(ctx)` reads it back.

## No user-creation function before the personal-org rule

The package's first task lists "create a user" among the store functions it
adds. The package's design makes one exported function create a user: the
one that creates the user, the personal org and the membership together,
so that no code path creates a user without a personal org. That function
is built with the first login (the package's fifth task), and no other
user-creation function is added before it. Until then the tests of
`internal/identity` seed users with an `INSERT` of their own, in
`internal/identity/testhub_test.go`.
