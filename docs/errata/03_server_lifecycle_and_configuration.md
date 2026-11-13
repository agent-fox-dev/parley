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
