# Architecture

Parley runs as one `parleyd` process with its data in one directory: two
embedded SQLite stores and the repositories it holds. `parley` is a client
of its REST API.

## Layout

| Path | Holds |
|---|---|
| `cmd/parleyd/` | `main`: hands the arguments, environment, working directory and signals to `internal/hub` |
| `cmd/parley/` | the CLI's `main` |
| `internal/hub/` | the composition of the process: start order, bootstrap, start hooks, background work, maintenance, shutdown, backup, the logger |
| `internal/config/` | the configuration file: location, TOML, environment references, the key registry |
| `internal/store/` | the transactional and analytical stores: open, schema steps, transactions, snapshots |
| `internal/server/` | the HTTP server and the REST conventions: routes, the check pipeline, errors, bodies, lists, idempotency, rate limit, headers, access log |
| `internal/server/timefmt/` | timestamps and durations (a leaf package) |
| `internal/scrub/` | removes credentials from text (a leaf package) |
| `internal/identity/` | the identity records (users, orgs, memberships, API keys, personal access tokens), the admin token, authentication of every request, the OAuth client and the login routes |
| `internal/maintenance/` | the hourly maintenance loop and its task registry |
| `internal/cli/` | the CLI: every command is declared through `Declare` (its word, route, arguments, auth and flags), which gives it the shared settings resolution, the one HTTP client, the output and exit-status rules, help as text and JSON, and `--wait`; it imports no server package |
| `internal/buildinfo/` | version, commit and build time set by the linker |
| `test/e2e/` | end-to-end tests of the built binaries and the process |

## Composition

Every feature registers on `hub.Composition` in a file of its own under
`internal/hub/`, and `compose` in `internal/hub/hub.go` calls it:

- configuration keys and sections on `Composition.Config`;
- schema steps with `AddStep` (a name, the target store, a function run in one
  transaction; applied once, in registration order);
- start hooks with `AddStartHook` (run in order before the listener is bound;
  an error stops the start);
- background work with `AddWork` (runs until the stop, then gets its grace,
  at most 30 seconds);
- maintenance tasks with `AddTask`;
- routes and the checks of the request pipeline with `OnServer`.

Hooks, work, tasks and server functions receive the `hub.Runtime`: the
configuration, the stores, the server, the logger, the scrubber, the clock
and the shutdown notification `ShuttingDown()`.

## Start

1. Parse the arguments.
2. Load and validate the configuration; register OAuth client secrets with the
   scrubber; build the logger.
3. Create the data directory and the workspace root; open both stores and
   apply pending schema steps.
4. Bootstrap: write a first or replacement admin token and exit, refuse to
   start while a token file exists, or go on.
5. Run the start hooks in registration order.
6. Start the maintenance loop and the background work.
7. Build the HTTP server from the configuration and listen.

Any failure before listening closes what was opened, writes one error line
and exits 1.

## Maintenance

One pass right after start and one every hour. A pass runs the registered
tasks one at a time in the order `retention`, `secret_reencryption`,
`claim_cleanup`, `forge_delivery_pruning`, `token_binding_sweep`,
`patch_purge`, `fork_expiry`, `sandbox_record_cleanup`, then any other task
in registration order. A failing task is logged and the pass goes on; passes
never overlap.

## Stop

On SIGTERM or SIGINT: close the shutdown notification, cancel the background
work and the loop, stop the server (15 seconds for in-flight requests), wait
for the loop (15 seconds) and each piece of work (its grace), all counted
from the signal and cut to 30 seconds, close the stores, exit 0.

## Stores

The transactional store (`database.path`) and the analytical store
(`analytics.db` beside it) are SQLite files opened through
`modernc.org/sqlite` in write-ahead-log mode with foreign keys on, a busy
timeout and immediate transactions, so concurrent writers wait for each
other. Each store records the schema steps it has applied. `parleyd backup`
copies each store with `VACUUM INTO`, which reads one consistent state while
the hub writes.
