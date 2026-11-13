# Errata: 03_server_lifecycle_and_configuration

Where the implementation of package 03
(`.specs/03_server_lifecycle_and_configuration`) differs from what the
package says.

## A configured rate limit of 0 is RateLimitDisabled

The package says the hub hands the server the configured requests per
minute. `server.Options.RequestsPerMinute` reads 0 as its default, 600 (see
[02_http_api_conventions.md](02_http_api_conventions.md)), so
`hub.ServerOptions` passes a configured `[limits].requests_per_minute = 0` as
`RateLimitDisabled: true` (with `RequestsPerMinute` 0). The values the
package's check names, 600 by default and 7 when configured, are passed as
they are; a third row of the check's table covers 0.

## The listener is bound before the maintenance loop starts

The package orders the start as "start the maintenance loop, and only then
bind the listener". `hub.Run` binds the socket right after the last start
hook, then starts the maintenance loop in the background, then writes the
`listening` line and serves (`internal/hub/hub.go`, `start`). What the order
is for still holds: no connection is accepted before the last start hook has
returned, and the first pass does not delay serving. Binding first means a
bind failure stops the start before any maintenance task has run, so the
failure's error line is the only line on stderr, as the package asks for
every failed start. The server serves on the bound listener through the new
option `server.Options.Listener`.

## Signals are caught with signal.Notify, from the bind on

The package says the hub stops on `SIGTERM` or `SIGINT`. `hub.Run` catches
them with `signal.Notify` on a channel that stays registered until `Run`
returns, not with `signal.NotifyContext`: the latter restores the default
action once the first signal has arrived, so a second signal during the stop
would end the process. The signals are caught from just before the bind, as
package 01 did, not during the configuration load, the bootstrap or the start
hooks, and not for a usage error.

## Every string attribute of a log line is cut to 1024 bytes

The package says the logger passes the message and every string attribute
through the scrubber, and the scrubber cuts its result to 1024 bytes. So
every string value of a log line is at most 1024 bytes, the `stack` of the
`panic` line of package 02 included.

## parleyd backup refuses a few more targets and sources

The package says `parleyd backup <directory>` refuses a non-empty directory
or a path that is not a directory, creates a missing directory, and fails
naming the database path when the database file does not exist.
`internal/hub/backup.go` also:

- refuses a target inside the workspace root, before writing anything: the
  copy of the workspace root would copy the snapshot into itself;
- creates a missing target with `os.Mkdir`, not its missing parents, and
  fails naming the target when the parent does not exist: what a failed
  backup wrote is removed, and parents it had created would be left behind;
- fails, before writing anything, when `analytics.db` or the workspace root
  does not exist, as for the database file, naming the path;
- fails, naming the path, on an entry of the workspace root that is not a
  regular file, a directory or a symbolic link (a socket, a device), rather
  than leave it out of the snapshot.

The copied directories of the workspace root get mode 0700; files keep their
permission bits. The store copies are single database files without a
write-ahead log.

## Where the checks of a few criteria live

- The rejected-arguments table of package 01 in
  `cmd/parleyd/main_test.go` holds no row `['backup', 'dir']` for the check
  of the usage errors to replace. Its rows are kept and the package's rows
  are added to it.
- The check of the start order waits, after the `listening` line, until the
  maintenance task has recorded its call, instead of reading the record at
  once: the pass runs in the background and may start a moment after the line
  is written. The order it asserts is the package's.
- The check of the second signal during a stop runs in
  `internal/hub/shutdown_test.go` through `hub.Run`, not through
  `cmd/parleyd`'s `run`: a test in package `hub_test` cannot call package
  `main`, and `run` only forwards the process's inputs to `hub.Run`, which
  holds the signal handling, so the path the signals take is the same.
- The check of the readiness probe obtains the stores from a start hook,
  which receives the hub's runtime, and closes one of them through
  `(*store.Store).Analytics()` or `(*store.Store).Main()`.
- The table of the check of the refused backup targets has a fourth row, a
  missing directory inside the workspace root, for the refusal above. The
  empty-workspace-root case of the check of the repository copy runs in the
  same test, on the same data, before the repositories are created.
- The end-to-end flows of the first boot and of the backup, in
  `test/e2e/lifecycle_test.go`, call `hub.Run`, not `cmd/parleyd`'s `run`:
  package `e2e` cannot call package `main`, and `run` only forwards the
  process's arguments, environment and working directory to `hub.Run`, which
  holds the flag and signal handling. The start hook, the maintenance task and
  the schema step the flows need are added through `hub.Options.Compose`.
- The flow of the failing maintenance task waits until three passes have run
  (at most 5 s) instead of sleeping a fixed 350 ms; with the 100 ms interval
  the third pass runs after about 200 ms.
