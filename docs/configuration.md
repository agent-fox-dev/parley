# Configuration

Parley has no configuration file yet, and `parleyd` reads no environment
variable.

## `parleyd`

| Setting | Value |
|---|---|
| Listen address | `0.0.0.0:8080` (all interfaces, port 8080) |
| Shutdown grace | 15 seconds: how long in-flight requests get to finish after `SIGTERM` or `SIGINT` |

Both values are fixed; `parleyd` takes no flag for them. See
[cli.md](cli.md) for how `parleyd` is started and stopped.
