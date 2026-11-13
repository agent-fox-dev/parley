# Configuration

Parley has no configuration file yet. `parleyd` reads no environment
variable; `parley` reads one, `PARLEY_ENDPOINT_URL`.

## `parleyd`

| Setting | Value |
|---|---|
| Listen address | `0.0.0.0:8080` (all interfaces, port 8080) |
| Shutdown grace | 15 seconds: how long in-flight requests get to finish after `SIGTERM` or `SIGINT` |
| Body limit | 1048576 bytes (1 MiB): the largest REST request body; a larger one is answered `413` (see [api.md](api.md#request-bodies)) |

These values are fixed; `parleyd` takes no flag for them. See
[cli.md](cli.md) for how `parleyd` is started and stopped.

## `parley`

| Variable | Effect |
|---|---|
| `PARLEY_ENDPOINT_URL` | the URL of the hub, for example `http://127.0.0.1:8080`, when the flag `--endpoint-url` is absent or empty |

The flag `--endpoint-url` comes first; an empty or unset `PARLEY_ENDPOINT_URL`
gives no URL. `parley version` without a URL prints the client's build alone.
See [cli.md](cli.md).
