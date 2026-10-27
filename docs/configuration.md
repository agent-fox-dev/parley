# Parley Configuration

Everything Parley reads from its environment, and what it does when a
setting is absent.

## Contents

- [Deployment settings](#deployment-settings)
- [Local development](#local-development)

## Deployment settings

Set as Worker variables, in `wrangler.toml` or by the deployment.

| Variable             | Default   | Description                                                                                                                                                                                                     |
| -------------------- | --------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `PARLEY_MOUNT_POINT` | `/api/v1` | The path prefix every versioned route is served under. The four [server endpoints](api.md#server-endpoints) are served at the server root whatever this is set to, and `GET /version` reports the value in use. |
| `PARLEY_BUILD_TIME`  | `""`      | The build's timestamp, reported by `GET /version`. Empty where the deployment supplies none.                                                                                                                    |
| `PARLEY_COMMIT`      | `""`      | The commit the build came from, reported by `GET /version`. Empty where the deployment supplies none.                                                                                                           |
| `PARLEY_ADMIN_TOKEN` | unset     | The deployment's admin token. A caller presenting exactly this value is an admin token: every scope, access to every user, and no user identity of its own. Where it is unset, no admin token is accepted.      |

Neither `PARLEY_BUILD_TIME` nor `PARLEY_COMMIT` affects behaviour; a
deployment that sets neither answers `GET /version` successfully with both
as the empty string.

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

| Setting              | Default         | Description                                                                                                                                 |
| -------------------- | --------------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| maximum request body | 1,048,576 bytes | A request whose body exceeds it is answered `413` in the error envelope before any route handler runs. Set it with `PARLEY_MAX_BODY_BYTES`. |

## The second target

The non-Cloudflare target reads the same settings from the process
environment, and two of its own.

| Variable         | Default     | Description                                                 |
| ---------------- | ----------- | ----------------------------------------------------------- |
| `PARLEY_PORT`    | `8787`      | The port the host process listens on.                       |
| `PARLEY_DB_PATH` | `parley.db` | Where SQLite keeps the relational store on the host's disk. |

Both targets apply the same schema migrations to their store before serving
a request, so a deployment on Cloudflare and one on a host hold the same
tables.

## Local development

No configuration is needed to run the tests: they build the service in
process and pass their own settings. See [the README](../README.md#develop).
