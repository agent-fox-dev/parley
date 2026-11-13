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
so that no code path creates a user without a personal org. That function,
`identity.CreateUserAndPersonalOrg`, is built with the first login (the
package's fifth task) and is the one exported way to create a user. The
tests of `internal/identity` seed bare users, without a personal org,
directly with an `INSERT` in `internal/identity/testhub_test.go`, because
their fixtures need users that the first login would never create.

## The providers' public endpoints stay in internal/config

The package's third task describes the two providers in `internal/identity`
with their default authorize, token and user URLs, and resolves each
configured `[[oauth.providers]]` entry against them. `internal/config`
already resolves them when it loads the file: a provider's endpoints arrive
in `config.Provider` with the public ones filled in where the file gives
none. A second table in `internal/identity` could drift from the first, so
the endpoints stay in `internal/config`, which exports
`config.Provider.WithPublicEndpoints` for code that builds a provider
without loading a file, and `internal/identity/provider.go` holds only what
the configuration does not: the supported names, the scopes and Google's
`response_type=code`.
