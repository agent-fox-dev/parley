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

Neither `PARLEY_BUILD_TIME` nor `PARLEY_COMMIT` affects behaviour; a
deployment that sets neither answers `GET /version` successfully with both
as the empty string.

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

## Local development

No configuration is needed to run the tests: they build the service in
process and pass their own settings. See [the README](../README.md#develop).
