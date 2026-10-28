# Parley Configuration

Everything Parley reads from its environment, and what it does when a
setting is absent.

## Contents

- [Deployment settings](#deployment-settings)
- [Bindings](#bindings)
- [Local development](#local-development)

## Deployment settings

Set as Worker variables, in `wrangler.toml` or by the deployment.

| Variable              | Default   | Description                                                                                                                                                                                                                                                         |
| --------------------- | --------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `PARLEY_MOUNT_POINT`  | `/api/v1` | The path prefix every versioned route is served under. The four [server endpoints](api.md#server-endpoints) are served at the server root whatever this is set to, and `GET /version` reports the value in use.                                                     |
| `PARLEY_BUILD_TIME`   | `""`      | The build's timestamp, reported by `GET /version`. Empty where the deployment supplies none.                                                                                                                                                                        |
| `PARLEY_COMMIT`       | `""`      | The commit the build came from, reported by `GET /version`. Empty where the deployment supplies none.                                                                                                                                                               |
| `PARLEY_ADMIN_TOKEN`  | unset     | The deployment's admin token. A caller presenting exactly this value is an admin token: every scope, access to every user, and no user identity of its own. Where it is unset, no admin token is accepted.                                                          |
| `PARLEY_SECRET_KEY`   | unset     | Thirty-two bytes, base64, that every stored secret is sealed under ([ADR 4](adr/04-seal-secrets-with-aes-gcm-under-a-deployment-key.md)). Where it is unset the deployment has no secret store, and creating a workspace with repository credentials answers `500`. |
| `PARLEY_EXTERNAL_URL` | unset     | The deployment's own base URL, used for the `hub_url` a workspace advertises. Where it is unset, `hub_url` is `null`.                                                                                                                                               |

Neither `PARLEY_BUILD_TIME` nor `PARLEY_COMMIT` affects behaviour; a
deployment that sets neither answers `GET /version` successfully with both
as the empty string.

## Bindings

A variable is not enough on Cloudflare: the store and the repositories are
bindings, and `wrangler.toml` must name them.

| Binding     | What it is           | Without it                                                                                                                                                    |
| ----------- | -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `DB`        | the D1 database      | Every route that touches data answers `500`, the schema migrations are skipped, and `GET /readyz` answers `503` — which is how a deployment says it has none. |
| `ARTIFACTS` | the repository store | A workspace cannot be cloned; the rest of the service works.                                                                                                  |

The D1 database is created once and bound by its id:

```
wrangler d1 create parley
```

```toml
[[d1_databases]]
binding = "DB"
database_name = "parley"
database_id = "<the id that command printed>"
```

The schema needs no separate step: both targets apply their migrations
before serving a request.

## Signing in

Sign-in is configured per provider. GitHub is the only one implemented.
Where the client id or the client secret is unset, the provider list is
empty and `POST /api/v1/auth/callback` refuses every provider name.

| Variable                            | Default  | Description                                                                                 |
| ----------------------------------- | -------- | ------------------------------------------------------------------------------------------- |
| `PARLEY_OAUTH_GITHUB_CLIENT_ID`     | unset    | The OAuth application's client id. Appears in the `authorize_url` the provider list serves. |
| `PARLEY_OAUTH_GITHUB_CLIENT_SECRET` | unset    | The OAuth application's client secret, sent only to the provider's token endpoint.          |
| `PARLEY_OAUTH_GITHUB_REDIRECT_URIS` | unset    | Comma-separated redirect URIs a callback may name. Unset allows any.                        |
| `PARLEY_OAUTH_GITHUB_AUTHORIZE_URL` | GitHub's | Where a person is sent to authorise. Set it to point at a stub while testing.               |
| `PARLEY_OAUTH_GITHUB_TOKEN_URL`     | GitHub's | Where a code is exchanged for an access token.                                              |
| `PARLEY_OAUTH_GITHUB_PROFILE_URL`   | GitHub's | Where the signed-in person's profile is read.                                               |

## Limits

| Setting                  | Default         | Description                                                                                                                                                       |
| ------------------------ | --------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| maximum request body     | 1,048,576 bytes | A request whose body exceeds it is answered `413` in the error envelope before any route handler runs. Set it with `PARLEY_MAX_BODY_BYTES`.                       |
| concurrent event streams | 100             | How many [event streams](api.md#the-live-event-stream) one component serves at once; the next connection is answered `503`. Set it with `AF_SSE_MAX_CONNECTIONS`. |

## The second target

The non-Cloudflare target reads the same settings from the process
environment, and two of its own.

| Variable           | Default        | Description                                                      |
| ------------------ | -------------- | ---------------------------------------------------------------- |
| `PARLEY_PORT`      | `8787`         | The port the host process listens on.                            |
| `PARLEY_DB_PATH`   | `parley.db`    | Where SQLite keeps the relational store on the host's disk.      |
| `PARLEY_REPO_PATH` | `repositories` | Where the second target keeps one bare repository per workspace. |

Both targets apply the same schema migrations to their store before serving
a request, so a deployment on Cloudflare and one on a host hold the same
tables.

## Checking a deployment

The smoke suites (`make test-smoke`) and the conformance harness
(`make conformance`) run against a deployment named by `PARLEY_BASE_URL`.
Each check says what it needs and skips naming it rather than passing on
nothing.

| Variable                         | Needed by                              | Description                                                                                                           |
| -------------------------------- | -------------------------------------- | --------------------------------------------------------------------------------------------------------------------- |
| `PARLEY_BASE_URL`                | both                                   | The deployment to exercise, for example `https://parley.example.com`.                                                 |
| `PARLEY_SMOKE_OAUTH_CODE`        | the sign-in, token and blocking suites | An authorization code the deployment's provider accepts, which a stub authorization server supplies.                  |
| `PARLEY_SMOKE_OAUTH_PROVIDER`    | the same                               | The provider name to sign in with. Defaults to `github`.                                                              |
| `PARLEY_ADMIN_TOKEN`             | the blocking suite                     | The deployment's admin token, so the suite can block and unblock.                                                     |
| `PARLEY_SMOKE_API_KEY`           | two conformance checks                 | An API key of any user, to check that a listing carries no secret and that administration refuses an ordinary caller. |
| `PARLEY_SMOKE_BROWSER`           | the web interface suite                | A Chromium with its debugging port open, for example `http://127.0.0.1:9222`. Without it the browser checks skip.     |
| `PARLEY_SMOKE_WORKSPACE`         | several suites                         | A workspace slug the key's owner may read.                                                                            |
| `PARLEY_SMOKE_PATCH_WORKSPACE`   | the web interface and rebuild suites   | A `carry_patch` workspace, whose patch stack the browser reads.                                                       |
| `PARLEY_SMOKE_ORG_WORKSPACE`     | the web interface suite                | A workspace the person reaches as a member of an organization.                                                        |
| `PARLEY_SMOKE_FOREIGN_WORKSPACE` | the same                               | A workspace the person may not read, whose address must say only that it is not available.                            |

## Local development

No configuration is needed to run the tests: they build the service in
process and pass their own settings. See [the README](../README.md#develop).
