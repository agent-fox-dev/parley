# Architecture

Parley ships as two programs: `parleyd`, the hub (server), and `parley`, the
command-line client. Both are written in Go, in the module
`github.com/agent-fox-dev/parley`, and the server uses the standard library's
`net/http` with no web framework.

## Package layout

| Package | Holds |
|---|---|
| `cmd/parley` | the `parley` program: it calls `cli.Run` and exits with its status |
| `cmd/parleyd` | the `parleyd` program: `main` calls `hub.Run` with the process's arguments, environment and working directory and exits with its status |
| `internal/buildinfo` | the version, commit and build time the binary was built with, shared by both programs |
| `internal/cli` | the `parley` client: its root command and its subcommands, built with `github.com/spf13/cobra`, each declared through one helper ([ADR 02](adr/02-declare-every-cli-command-through-one-helper.md)), the client configuration file, the resolution of the hub URL and the credential, the one HTTP client the commands send their requests with, the printing of a command's result on stdout and of its warnings on stderr, the shared wait for a long operation (`--wait`, `--timeout`, `--poll-interval`), and the error envelopes and exit statuses every command ends with |
| `internal/config` | the configuration loader: `config.toml` from `$XDG_CONFIG_HOME` or the working directory, `${NAME}` references, and a registry of keys and whole sections that decides what the file may hold |
| `internal/hub` | the composition of the `parleyd` process: argument parsing, the scrubbing logger, the fixed start order, the bootstrap of the admin token, the HTTP server's options, the idempotency records in the transactional store, signals and the stop, and the `backup` command; the one place later packages plug their parts in ([ADR 01](adr/01-compose-the-hub-in-one-package.md)) |
| `internal/maintenance` | the maintenance loop: a registry of named tasks, and `Loop`, which runs one pass at once and one at each tick of an hourly timer, the tasks of a pass one after the other in a fixed order, and logs a failing task without stopping the others |
| `internal/identity` | who may use the hub: the admin settings in the transactional store (the bootstrap e-mail and the admin token's hash), the generation and hashing of admin tokens, and `VerifyAdminToken`, which compares a presented token's hash with the stored one in constant time; the identity records (users, orgs, memberships, API keys, PATs), the generation of API keys and PATs, and `Service`, the pipeline's authenticator |
| `internal/scrub` | the one scrubbing function for text that leaves the hub: `Scrubber.Scrub` replaces registered secret values and the hub's own credential formats by `[redacted]`, removes user information from URLs and cuts the text to 1024 bytes; a leaf package that imports nothing of the project |
| `internal/store` | the hub's two embedded SQLite stores (`modernc.org/sqlite`, no cgo): `Open` creates the data directory and the workspace root, opens the transactional store and the analytical store (`analytics.db` beside it) and applies the registered schema steps; `Tx` runs a transaction of the transactional store; `Snapshot` writes a consistent copy of one store file without changing it |
| `internal/server` | the hub's HTTP server: its built-in routes, the REST API's route registration and check pipeline, the error envelope, request ids, response headers and the access log, list paging, idempotency keys, and the readiness probes |
| `internal/server/timefmt` | the one timestamp format (`Format`, and the response member type `Time`) and the one duration grammar (`ParseDuration`); a leaf package that imports nothing of the project, so that the configuration loader can use it as well as the REST API |
| `test/e2e` | tests that build the binaries and run them |

## The HTTP server

`server.New(server.Options)` builds the server, and `(*Server).Handler()`
returns the `http.Handler` that serves its routes. `Options` holds the listen
address (`Addr`) or a listener already bound (`Listener`), the shutdown grace (`ShutdownGrace`), the readiness
probes (`ReadinessProbes`) and `OnListen`, a function `Serve` calls once with
the address it bound (with port `0` in `Addr`, the port the system chose),
the REST body limit (`MaxBodySize`), the rate limit (`RequestsPerMinute`,
`RateLimitDisabled`, and `Now`, the clock it and the idempotency records
read, which tests replace), the `Logger` (a `*slog.Logger`; nil means
`slog.Default()`), `LogHealthProbes` and the `IdempotencyStore` (nil means
an in-memory store).
`Serve` serves `Handler()`, so the running hub and in-process tests answer
the same way.

Every route is registered on one standard library `http.ServeMux` with
method and path patterns: the built-in `GET /healthz`, `GET /readyz` and
`GET /version`, the REST API routes, and surfaces outside the REST API that
later packages mount with `(*Server).Handle(pattern, handler)`, which takes
the mux's own patterns and bypasses the REST API's checks. Success bodies are
written with `server.WriteJSON`, errors with `server.WriteError`, both with
`Content-Type: application/json; charset=utf-8`. The routes are described in
[api.md](api.md).

### Request ids, headers and the access log

`Handler()` returns the dispatcher below wrapped in one outermost middleware
(`internal/server/headers.go`), so it covers every surface: the built-in
routes, the REST API, unknown paths and surfaces mounted with `Handle`. Before
anything else runs it:

1. takes the request id from an incoming `X-Request-ID` that is a UUID in the
   36-character hyphenated hexadecimal form (any version, either case),
   unchanged, or else generates a random version 4 UUID from `crypto/rand`;
   puts it in the request's context, where `server.RequestID(ctx)` reads it;
2. sets `X-Request-ID`, `X-Content-Type-Options: nosniff`,
   `X-Frame-Options: DENY` and `Referrer-Policy: no-referrer` on the response;
3. sets `Cache-Control: no-store` when the path is `/api/v1` or under it. The
   pipeline replaces it with the matched route's `CacheControl` when the route
   states one; the `/healthz` and `/readyz` handlers set `no-cache` and the
   `/version` handler `public, max-age=300`.

It hands the request on with a `ResponseWriter` that records the status. The
wrapper keeps `Unwrap()`, so `http.ResponseController` (flush, deadlines)
still reaches the connection's writer. When the request is answered, it writes
the access-log line (`internal/server/accesslog.go`) through `Options.Logger`:
message `request` with `method`, `path` (`r.URL.Path`, never the query
string, so a credential sent as a query parameter never reaches the log),
`status`, `duration_ms` and `request_id`, at info level. Requests to
`/healthz` and `/readyz` get a line only when `Options.LogHealthProbes` is
set, and then at debug level.

### Dispatch

The dispatcher sits in front of the mux. The mux alone would
redirect a path that is not clean (`//x`, `/a/../x`) or that lacks the
trailing slash of a subtree pattern, and would answer an unknown path or a
wrong method in plain text. The dispatcher answers a path that is not clean
with the `404 not_found` envelope, asks the mux (`ServeMux.Handler`) which
pattern matches, and answers no match with the `404 not_found` envelope, or
with `405 method_not_allowed` and the mux's `Allow` list when the path is
registered under other methods. It removes the `HEAD` that the mux adds for
every `GET` pattern unless a `HEAD` pattern exists for the path. A redirect
the mux would send for a missing trailing slash is a `404` too. Everything
else goes to `ServeMux.ServeHTTP`.

### REST routes

A feature registers a REST route with `(*Server).Register(server.Route)`.
`Route.Method` is an upper-case method, `Route.Pattern` a path relative to
`/api/v1` with `:name` parameters (`/workspaces/:slug/tokens`), and
`Route.Handler` a `server.HandlerFunc`,
`func(http.ResponseWriter, *http.Request) error`: it writes its own response
on success, and an error it returns is written as the envelope. The route's
declarations are `Public`, `Create` with its `ShowOnce` members, its own
`MaxBodySize`, `ContentType` and `CacheControl`, its `Feature` check, the one
`Scope` a personal access token needs (a name the server does not interpret)
and `Marks`, named values whose meaning the installed checks define.
`Register` turns the pattern into the mux's form
(`POST /api/v1/workspaces/{slug}/tokens`) and panics on a pattern that does
not start with a slash, ends in one, has an empty, `.` or `..` segment, a
brace, or a parameter without a valid name or named twice, on a method that
is not upper-case letters, on a missing handler and on a route already
registered.

Each check and the handler read the route their request matched with
`server.MatchedRoute(r)`: a `server.Matched` with the method and pattern as
registered, the `Route` and the path parameter values (`Params`);
`server.Param(r, name)` returns one value.

### The check pipeline

After the mux has matched a REST route, the pipeline runs the route's checks
in a fixed order and stops at the first error:

1. authentication, through the installed `server.Authenticator`,
   `func(*http.Request) (server.Caller, error)`; with none installed, a route
   that is not public is answered `401 unauthenticated`;
2. the rate-limit stage (`internal/server/ratelimit.go`): it counts the
   request against the caller's credential id, or against the connection's
   remote IP address on a public route, and answers `429 rate_limited` with
   `Retry-After` when the key is over the limit;
3. the body stage (`internal/server/body.go`): it reads the whole body, up to
   one byte past the limit that applies (the route's `MaxBodySize`, else
   `Options.MaxBodySize`, else 1048576 bytes), answers `413
   payload_too_large` when the declared length or the bytes read exceed it,
   then, for a non-empty `POST`, `PUT` or `PATCH` body, answers `415
   unsupported_media_type` unless the `Content-Type` media type is the
   route's `ContentType` or, when it states none, `application/json`. The
   buffered bytes replace the request's body and stay in its context, so a
   later stage can read them without consuming the body the handler reads;
4. the route's `Feature` check, the binding guard (`SetBindingGuard`), the
   scope check (`SetScopeCheck`) and the access checks (`AddAccessCheck`), in
   that order; each is a `server.Check`, `func(*http.Request) error`, and an
   empty position passes;
5. on a route declared `Create`, the idempotency stage
   (`internal/server/idempotency.go`), described below;
6. the handler.

A public route skips authentication and step 4. The access position holds a
list: every check added with `AddAccessCheck` runs, in the order it was
added, and decides from the matched route whether it applies. The
authenticator reports a `server.Caller` (`CredentialID`, and `UserID` or
`Admin` for the admin token, and `Detail`, what the authenticator knows
beyond the ids, which the server does not read), which the pipeline puts in
the request's context; later checks and the handler read it with
`server.CallerOf(r.Context())`. An `*server.Error` (also when wrapped) is
written as its envelope; any other error as `500 internal` with the message
`internal error`, and its text is logged through `Options.Logger` as an
error line `internal error` with the `request_id` and the `error`. When the
handler had already started its answer, nothing is written after it: the
error is logged as `error after the answer began` with the `request_id` and
the `error`. The
pipeline also recovers a panic in a check or the handler: it logs an error
line `panic` with the `request_id`, the panic value as `error` and the
`stack`, and answers `500 internal` unless the handler had already started
its answer. A panic with `http.ErrAbortHandler` is passed on, so net/http
drops the connection as the handler asked. A panic on a surface outside the
REST API is not recovered: net/http drops that connection, and the access
log records the request with status `500`.

A request that authentication refuses, also for want of an authenticator,
is counted against its remote IP address and then answered with
authentication's error: authentication comes before the rate limit in the
order of checks, so the address's count decides the answer only on public
routes.

The rate limit is a `server.Limiter`: `NewLimiter(perMinute, now)` counts
requests per string key in fixed windows of 60 seconds, each starting at the
key's first counted request, and `(*Limiter).Allow(key)` reports whether a
request is admitted and, when not, the whole seconds to the window's end
(rounded up, at least 1). A refused request does not change the window, and
a limit of 0 admits everything. It is safe for concurrent use, keeps its
counts in memory only, and drops ended windows at most once a minute. The
server builds one from `RequestsPerMinute` (0 means 600) unless
`RateLimitDisabled` is set, and `(*Server).RateLimiter()` returns it so that a
surface mounted with `Handle`, such as the git server, counts against the same
keys. Its keys are `credential:<id>` (a caller with a credential id) and
`address:<ip>` (no caller, or a caller without a credential id). Built-in routes and
`Handle` surfaces do not pass the pipeline, and `404`/`405` are answered by
the dispatcher before it, so none of them is counted.

A handler decodes a JSON body with `server.DecodeJSON(r, &v)`, which
returns `400 invalid_request` for an empty body, invalid JSON, a value that
is not an object or data after the object, and ignores unknown members.

A list handler pages through `internal/server/list.go`:

1. `server.ReadPage(r, bounds...)` reads `limit` and `cursor` from the query
   and returns a `server.Page`, or a `400 invalid_request` error that the
   handler returns before it reads its collection. The limit defaults to 50
   within 1–200; a route with other bounds passes its own
   `server.PageBounds{Default, Min, Max}` as the argument (there is no `Route`
   field for it).
2. `(server.Page).After(&pos)` decodes the position the page continues after
   into a value of the handler's own type, the keyset of its total order, and
   reports false on the first page.
3. The handler fetches up to `page.Limit+1` items after that position and
   calls `server.WritePage(w, page, items, position)`, which writes the first
   `Limit` items as `items` (`[]` for none) and, when the extra item exists,
   `next_cursor` from `position` of the last item written, else `null`.

A cursor is the unpadded URL-safe base64 form of the JSON document
`{"p": <position>, "f": <fingerprint>}`. The fingerprint is a truncated
SHA-256 of the matched route's method and pattern as registered, its path
parameter values and every query parameter except `cursor` and `limit`, so a
cursor is refused on another query but accepted with another limit. Cursors
are not signed: a cursor grants nothing, since every page passes the full
order of checks again.

The idempotency stage runs on create routes only, after every check, so a
replay is authenticated and authorised like the request it repeats. A
request without an `Idempotency-Key` header, every request to a public
route, which has no caller to record for, and a request whose caller has no
admin flag, user id or credential id, goes straight to the handler. A key that is empty or longer
than 255 characters (counted in characters, not bytes) is `400
invalid_request`. Otherwise the record is identified by three strings: the
key, the caller (`admin-token` when `Caller.Admin`, else `user:<UserID>`,
else `credential:<CredentialID>`) and
the route (the method, one space and the concrete `r.URL.Path`, such as
`POST /api/v1/workspaces/w1/tokens`). The stage then:

1. takes the per-record gate for that identity (below);
2. finds the record; one whose `WrittenAt` is 24 hours or more before
   `Options.Now()` is treated as absent. A valid record with the SHA-256 of
   the buffered request body is replayed as `200` with its `Header` and its
   body; one with another hash is `409 conflict`, and the record is left as
   it is;
3. otherwise runs the handler through a writer that passes the response
   through unchanged and keeps a copy of its status, its body and the
   headers the handler set before it answered: those it added or changed,
   without `Content-Length`, since the recorded body can be shorter (it has
   `Unwrap()`, so the status-capturing writer of the headers middleware below
   it and `http.ResponseController` keep working);
4. when the handler returned no error and answered `2xx`, removes the route's
   `ShowOnce` members from a copy of the body (a member name, or a dotted path
   into nested objects such as `token.token`; a path the body lacks is
   skipped; an object it changes is re-encoded with its members in sorted
   order and without white space between them, and every value kept is
   unchanged), calls
   `DeleteExpired` with the cutoff `now - 24h` when no sweep has started in
   the last minute (a sweep of the in-memory store visits every record, so
   it does not run on every write), and then `Write`s the record.
   A body that is not a JSON object while `ShowOnce` names members, or a store
   error, is logged as `idempotency record not written` with the
   `request_id` and the `error`, and nothing is recorded: the caller already
   has the handler's answer;
5. records nothing for any other status or for an error.

The records live behind `server.IdempotencyStore`, which `internal/hub`
implements over the transactional store and installs as
`Options.IdempotencyStore`:

```go
type IdempotencyStore interface {
	Find(ctx context.Context, key, caller, route string) (IdempotencyRecord, bool, error)
	Write(ctx context.Context, rec IdempotencyRecord) error
	DeleteExpired(ctx context.Context, cutoff time.Time) error
}

type IdempotencyRecord struct {
	Key         string            // the Idempotency-Key, 1–255 characters
	Caller      string            // "admin-token", "user:<id>" or "credential:<id>"
	Route       string            // "POST /api/v1/..." with the concrete path
	RequestHash [sha256.Size]byte // SHA-256 of the request body's bytes
	Body        []byte            // the response body without its show-once members
	Header      http.Header       // the headers the handler set, without Content-Length
	WrittenAt   time.Time         // the server's clock when written
}
```

- `Find` returns the record whose `Key`, `Caller` and `Route` are all equal
  to its arguments, and `false` when there is none. It may return an
  expired record that is not deleted yet; the server decides from
  `WrittenAt` and never replays it.
- `Write` stores the record, replacing any record with the same `Key`,
  `Caller` and `Route` (a key whose record expired is recorded anew). A
  store keeps every field, `Header` too: a replay sends only the headers
  the record holds.
- `DeleteExpired` removes every record whose `WrittenAt` is at or before
  `cutoff`, that is, every record 24 hours old or older.
- An error from `Find` ends the request as `500 internal`; an error from
  `DeleteExpired` or `Write` is logged, as above. Every method must be safe
  for concurrent use. The stage calls only these three methods and keeps no
  record of its own.

`server.NewMemoryIdempotencyStore()` is the in-memory implementation and the
server's default; its records are lost when the process ends. `parleyd` uses
the store-backed implementation of `internal/hub` instead (see
[The hub](#the-hub)).

The gate that orders requests with one identity is in memory in either case,
since the hub is one process: a map from (key, caller, route) to a slot with
a one-token channel and a count of the requests holding or awaiting it. A
request takes the token before it reads the store and gives it back after the
record is written or the request has failed, so a request waiting on the same
identity sees the record when the first one succeeded, and exactly one waiter
runs the handler when it failed. The map's mutex is held only to find or drop
a slot, never while a request waits or runs, so requests with different keys,
callers or paths never wait for each other; a slot is dropped when its count
reaches zero. A waiting request whose client goes away (its context ends)
stops waiting and ends unanswered: nothing is written, nothing is logged at
error level, and a debug line `request ended while waiting for idempotency key`
carries the `request_id` and the `error`. Its access-log line shows status
`200`, the status of a request that wrote nothing.

The installed checks are copied under a read lock at the start of each
request, and no lock is held while a check or a handler runs, so a request
blocked in its handler never holds up another one.

### Timestamps and durations

Timestamps and durations are written and read in `internal/server/timefmt`,
not in `internal/server` itself, because the package imports nothing of the
project: the configuration loader can parse its duration keys with it
without importing the HTTP server. `timefmt.Format(t)` writes an instant as
RFC 3339 in UTC with the fraction of a second truncated. A response struct
declares a timestamp member as `timefmt.Time` (a defined type over
`time.Time`, converted with `timefmt.Time(t)`), whose `MarshalJSON` writes
the same string, so no handler formats a timestamp by hand.
`timefmt.ParseDuration(s)` reads a whole number with one of the units `s`,
`m`, `h` or `d`; it checks the digits itself, because `strconv` would accept
a sign, and refuses a value whose product with the unit would overflow a
`time.Duration`. No route or configuration key uses the package yet.

### Serving and stopping

`(*Server).Serve(ctx)` serves on `Listener` when it is set, and otherwise
binds `Addr` with `net.Listen("tcp", …)` and returns the error
(`listen on <addr>: …`) when it cannot; it calls `OnListen` and serves until
`ctx` is cancelled. Then it stops in this order:

1. It closes the listener, so new connections are refused.
2. It waits up to 100 ms for connections that are accepted but whose request
   has not been read yet. `http.Server.Shutdown` would drop those, and a
   request the client had already sent would get no answer.
3. It calls `http.Server.Shutdown` with a deadline of `ShutdownGrace`, which
   waits for in-flight requests to finish and closes idle connections.
4. When the grace passes first, it calls `http.Server.Close`, which closes the
   remaining connections; their clients get no response. `Serve` does not
   wait for those handlers to return.

`Serve` returns `nil` after a stop, whether or not the grace passed.

## The stores

`store.Open(ctx, store.Options{DatabasePath, WorkspaceRoot}, steps)` returns a
`*store.Store`. It:

1. refuses a step list with a step without a name or a function, with an
   unknown target, or with a name given twice, before it creates anything;
2. creates the data directory (the directory of `DatabasePath`) and
   `WorkspaceRoot`, with missing parents, with mode 0700; a directory that
   exists keeps its mode;
3. refuses a store file that exists, is not empty and does not begin with
   SQLite's header, by reading it only, so the file keeps its bytes; both files
   are looked at before either is opened;
4. opens the transactional store at `DatabasePath` and the analytical store at
   `store.AnalyticsFile` (`analytics.db`) in the same directory, each with
   `busy_timeout` 10000 ms, `journal_mode` WAL, `foreign_keys` on and
   `_txlock=immediate` (every transaction takes the write lock when it
   begins);
5. applies, in registration order, every step its target store (`store.Main`
   or `store.Analytics`) has not recorded in its `schema_steps` table, each in
   one transaction together with its record. A step that returns an error or
   panics is rolled back, no later step runs, and `Open` fails with an error
   naming the step; the steps before it stay applied and are skipped on the
   next open.

Every failure returns no store and an error naming the path or the step.

A `store.Step` is `{Name, Target, Apply func(ctx, *sql.Tx) error}`. Its name
is what the store records, so a step is never renamed.

`(*Store).Tx(ctx, fn)` runs `fn` in a transaction of the transactional store:
it commits when `fn` returns nil, and rolls back when `fn` returns an error
(returned as it is) or panics (returned as an error carrying the panic value).
Transactions of `Tx` take one token in turn, so the process's writers wait for
each other in memory and never fail with "database is locked"; `fn` must not
call `Tx` again. `Main()` and `Analytics()` return the two `*sql.DB` handles:
the first for reading, the second for the packages that read and write the
analytical store directly. `Ping` returns nil when both stores answer
`SELECT 1`, and an error naming the store that did not. `Close` closes both;
a second `Close` returns nil, and `Tx` and `Ping` return `store.ErrClosed`
after it.

`store.Snapshot(ctx, src, dst)` writes a copy of the store file `src` to the
new file `dst` with `VACUUM INTO`, which reads the source in one read
transaction: the copy holds every transaction committed before it began,
each whole, while other connections, of this process or another, go on
committing, and it has no write-ahead log. The source is opened read-only
(`mode=ro`), so the engine never checkpoints its write-ahead log. A source
without a write-ahead log (a store no connection has open) is read with
`immutable=1` as well, because a read-only connection would leave an empty
`-wal` and a `-shm` file beside it; when a log appears or the file changes
during that copy, the copy is discarded and taken again read-only. `dst`
must not exist, and a failure removes what was written of it.

## The hub

`internal/hub` composes the `parleyd` process, so that `cmd/parleyd` stays a
thin `main` and tests start a whole hub in-process. `hub.Run(ctx,
hub.Options)` returns the exit status. `Options` holds the process's inputs,
`Args`, `Env` (a `config.Env`, the shape of `os.LookupEnv`), `WorkDir`,
`Stdout` and `Stderr`, and the fields tests set and `parleyd` leaves zero:
`Compose` (parts added after the hub's own), `Port` (replaces
`[server].port`), `Now` (the hub's clock), `RequestGrace` (replaces
`hub.DefaultRequestGrace`, 15 seconds), `MaintenanceInterval` (replaces
`maintenance.Interval`, one hour, the time between two maintenance passes),
`StopLimit` (replaces `hub.DefaultStopLimit`, 30 seconds, the longest a stop
waits) and `LoopGrace` (replaces `hub.DefaultLoopGrace`, 15 seconds, how long
a stop waits for the maintenance loop). `cmd/parleyd`'s `run(ctx, args, env,
wd, stdout, stderr)` builds the options and calls `Run`; `main` passes
`os.Args[1:]`, `os.LookupEnv` and the working directory.

### The composition

A part is a `func(*hub.Composition)`. The hub's own parts are listed, in
order, in the `parts` variable of `internal/hub/hub.go`; a package that plugs
into the hub writes its part in a file of its own in `internal/hub` and
appends it there ([ADR 01](adr/01-compose-the-hub-in-one-package.md)).
`Options.Compose` runs after them. On the composition a part registers:

| Method | What |
|---|---|
| `RegisterKey(config.Key)`, `RegisterSection(name, validate)` | configuration keys and whole sections |
| `AddSchemaStep(store.Step)` | schema steps, applied in the order they were added |
| `AddStartHook(name, func(ctx, *hub.Runtime) error)` | start work, run in order, each to completion |
| `AddMaintenanceTask(name, func(ctx, *hub.Runtime) error)` | maintenance tasks (see [The maintenance loop](#the-maintenance-loop)) |
| `AddBackgroundWork(name, grace, func(ctx, *hub.Runtime))` | work that runs while the hub serves, until its context is cancelled at the stop (see [The stop](#the-stop)) |
| `AddRoute(server.Route)`, `Handle(pattern, handler)` | REST routes and surfaces outside the REST API |
| `SetAuthenticator`, `SetBindingGuard`, `SetScopeCheck`, `AddAccessCheck` | the checks of the pipeline's positions |

`hub.Runtime` holds the loaded `*config.Config`, the open `*store.Store`, the
`*slog.Logger`, the `*scrub.Scrubber` and the clock `Now`. Start hooks,
maintenance tasks and background work receive it; a route handler or a check keeps
`(*Composition).Runtime()`, which the hub fills once the stores are open and
before the first hook runs or the first request is served. A start hook or a
maintenance task without a name or a function, or with a name already
registered, a configuration key or section the registry refuses, and a
route the server refuses, stop the start before anything is opened; so does
background work without a name or a function, or with a name already
registered. `(*Composition).ShuttingDown()` returns the shutdown
notification, a channel the hub closes once when the stop begins.

### The start

`Run` parses the arguments with the standard `flag` package first: `--help`
prints the usage on stdout and returns `0`; an unknown flag,
`--admin-email` without a value, a positional argument other than `backup`,
`backup` without exactly one directory, and `backup` with `--admin-email` or
`--reset-admin-token` print the usage on stderr and return `2`. None of these
loads, opens or creates anything. `backup <directory>` runs
[the backup](#the-backup) instead of the start.

Then it builds the logger and starts in this order, stopping at the first
failure:

1. compose: the hub's parts, then `Options.Compose`;
2. load the configuration with the composition's registry (`config.toml`,
   see [configuration.md](configuration.md)), set the logger's level from
   `[logging].level`, and register every OAuth provider's `client_secret`
   with the scrubber;
3. open the stores with the composition's schema steps (`store.Open`);
4. the bootstrap (`internal/hub/bootstrap.go`), which may end the run with
   `0` after writing the admin token file, or refuse the start (see
   [Bootstrap](#bootstrap));
5. the start hooks, in the order they were added, each to completion, with
   `Run`'s context;
6. catch `SIGTERM` and `SIGINT` with `signal.Notify` on a channel kept
   registered until `Run` returns, so that a second signal during the stop
   does not end the process;
7. bind `[server].bind`:`[server].port` with `net.Listen`;
8. build the server from `hub.ServerOptions(cfg, st)`, with the bound
   listener, the request grace, the clock and the logger, and install the
   composition's routes and checks;
9. start the maintenance loop in the background, without waiting for its
   first pass (see [The maintenance loop](#the-maintenance-loop)), and each
   piece of background work in a goroutine of its own, with one context
   shared with the server and the loop;
10. `Serve`, whose `OnListen` writes the `listening` line with `addr`,
    `config` (`cfg.Path`) and `data_dir` (`cfg.DataDir()`).

The listener is bound before the maintenance loop starts, so that a bind
failure is the only line of a failed start. A failure before listening is
one line at error level, `cannot start` with the cause in `error`, or
`cannot listen` with `addr` and `error`; the stores are closed when they were
open, and `Run` returns `1`. The first signal, or the cancellation of `ctx`,
begins [the stop](#the-stop).

### The backup

`parleyd backup <directory>` (`internal/hub/backup.go`) composes the hub and
loads the configuration as the start does (steps 1 and 2), so the keys and
sections the parts register apply, and then only reads the data: it opens no
store for writing, applies no schema step, runs no bootstrap, start hook or
maintenance task, catches no signal and binds nothing. In order, stopping at
the first failure:

1. check that the database file, `analytics.db` and the workspace root
   exist, so that a backup run where no hub ever started creates no database;
2. check the target, a relative one taken relative to `WorkDir`, which
   `Run` makes absolute first: a directory inside the workspace root (which
   it would copy into itself) or inside the data directory (which a backup
   leaves untouched), a path that is not a directory and a non-empty
   directory are refused; a missing one is created with `os.Mkdir` and mode
   0700, its parent not;
3. copy the transactional store under the database file's own name and the
   analytical store as `analytics.db`, each with `store.Snapshot`;
4. copy the workspace root under its own name, keeping relative paths, file
   permission bits and symbolic links; the `objects` directory of each git
   directory (one that holds a `HEAD` file) is copied after the rest of the
   tree, so its refs, `packed-refs` and `HEAD` are read before its objects
   and every copied ref points at a copied object.

A failure after the target was created or written to removes what the
backup wrote (the target itself when it created it, else its entries) and
logs one `backup failed` line naming the cause; `Run` returns `1`. Success
logs `backup written` with `dir` and returns `0`, with nothing on stdout.

### The stop

`internal/hub/shutdown.go` stops the hub on the first `SIGTERM` or `SIGINT`,
or when `Run`'s context is cancelled:

1. it cancels the context the server, the maintenance loop and the
   background work share, so the server closes its listener and stops
   accepting connections (see [Serving and stopping](#serving-and-stopping)),
   the loop starts no further task and cancels the running one, and every
   piece of background work sees its context done;
2. it closes the shutdown notification, before it waits for anything, so a
   handler of a long-lived response that watches `ShuttingDown()` ends it
   and the server need not wait out the request grace for it;
3. it waits for everything at the same time: the server, which gives the
   requests in flight the request grace and then closes their connections;
   the maintenance loop, up to `LoopGrace`; each piece of background work,
   up to its own grace, which `AddBackgroundWork` cuts to 30 seconds;
4. it stops waiting for anything once `StopLimit` has passed since the
   signal, and writes one `abandoned at the stop` warning, with `work`, for
   each thing it stopped waiting for; abandoned work keeps running and finds
   the stores closed;
5. it closes the stores, last, and `Run` returns `0`, whether or not a grace
   passed (`1` only when the server had failed while serving).

The signal channel stays registered until `Run` returns and nothing reads it
after the first signal, so a further signal is dropped: no grace starts
again and the process does not end early.

### The maintenance loop

`internal/maintenance` holds a `Registry` of tasks, each a name and a
`maintenance.Task` (`func(ctx) error`); `Register` refuses an empty name, a
nil function and a name registered before, with an error that names it, and
keeps the first task. The composition's `AddMaintenanceTask` registers on the
hub's registry, so a refused task is a start error like any other refused
registration.

`maintenance.New(reg, logger)` builds a `Loop` over the registered tasks.
`(*Loop).Run(ctx, ticks)` runs one pass at once and one at each tick it
receives; the hub passes the channel of a `time.Ticker` of
`maintenance.Interval`, one hour, which no configuration key changes
(`Options.MaintenanceInterval` replaces it in tests). A pass
(`(*Loop).Pass`) runs the tasks one after the other: those registered under
`retention`, `secret_reencryption`, `claim_cleanup`,
`forge_delivery_pruning`, `token_binding_sweep`, `patch_purge`,
`fork_expiry` and `sandbox_record_cleanup` in that order, a name nobody
registered skipped, then every task registered under another name in the
order it was registered. Passes never overlap: a tick that arrives while a
pass runs is received and dropped, and the next pass starts at the next
tick. A task that returns an error or panics is logged at error level as
`maintenance task failed` with `task` and `error` (its text passed through
the scrubber), and the pass goes on with the next task. When `ctx` is
cancelled the running task's context is done, no further task or pass
starts, and `Run` returns once the running task has returned; a task that
ends with an error after the cancellation is not logged as failed.

`hub.ServerOptions(cfg, st)` returns the options that come from the
configuration and the stores: `MaxBodySize` from `[server].max_body_size`,
`RequestsPerMinute` from `[limits].requests_per_minute` with
`RateLimitDisabled` set when it is `0` (the server reads a
`RequestsPerMinute` of 0 as 600), `LogHealthProbes` from
`[logging].log_health_probes`, one readiness probe, `st.Ping`, which fails
while either store does not answer a query, and the store-backed
`IdempotencyStore`.

### Bootstrap

`internal/hub/bootstrap.go` adds the part `composeBootstrap`, after
`composeIdempotency` in `parts`, which adds the schema step
`identity.AdminSettingsStep`: a table `admin_settings` with one row (`id` 1,
`email`, `token_hash`) that exists once the hub is bootstrapped.
`identity.AdminSettings(ctx, st)` returns the row as an `identity.Admin`, the
zero value when none is stored. `identity.GenerateAdminToken` returns
`parley_admin_` and 32 bytes of `crypto/rand` as 64 lowercase hex
characters; `identity.HashAdminToken` is the lowercase hex SHA-256 of the
whole token; `identity.VerifyAdminToken(ctx, st, token)` reports whether
the token's hash is the stored one, compared with
`crypto/subtle.ConstantTimeCompare`, and false when none is stored.
`identity.IssueAdminToken(ctx, st, email, deliver)` generates a token,
upserts its hash (the e-mail only when the row is new) and calls `deliver`
with the token inside the same `Tx`, so an error from `deliver` rolls the
hash back.

The bootstrap stage reads the settings and, by the table in
[cli.md](cli.md#the-admin-token), issues a token whose `deliver` writes it
to `<config dir>/admin_token` (a 0600 temporary file in the config dir,
synced and renamed over the path; a file already there is first given a
second name by a hard link, put back when the transaction then fails to
commit and removed once it has committed), refuses with an error naming the file
while a bootstrapped hub's file exists, or goes on to the start hooks. After
writing the file it logs the path at warn level and `Run` returns `0`
without running a start hook or binding the listener.

### Identity

`internal/hub/identity.go` adds the part `composeIdentity`, after
`composeBootstrap` in `parts`. It adds the schema step
`identity.SchemaStep` (`identity_records`), installs the `Authenticate`
method of an `identity.Service` as the pipeline's authenticator, adds its
`AdminCheck` method with `AddAccessCheck`, registers every route of `(*identity.Service).Routes()` with `AddRoute`, and adds the
start hook `identity`, which binds the service with
`(*identity.Service).Bind(identity.Deps{Store, Now, Providers,
ExternalURL, Client})` from the `Runtime` before the hub serves its first
request. `Deps.Client` is the `identity.ProviderClient` a login reads the
user's identity through; the hub binds
`identity.NewOAuthClient(rt.Config.OAuth.Providers, http.DefaultClient,
identity.DefaultProviderTimeout)`, and a service without one answers every
login `502 upstream_error`.

The service's routes are, so far, the public `GET /auth/providers` (see
[api.md](api.md#get-apiv1authproviders)), the public `POST
/auth/callback` (see [api.md](api.md#post-apiv1authcallback)), which is not
a create route, the admin route `POST /users` (see
[api.md](api.md#post-apiv1users)), a create route, the admin routes `GET /users`, `GET /users/:id`
and `PATCH /users/:id` (see [api.md](api.md#get-apiv1users)), the admin
routes `POST /users/:id/promote`, `/demote`, `/block` and `/unblock` (see
[api.md](api.md#role-and-status-of-a-user)), and the
caller's own profile, `GET /user` and `PATCH /user` (see
[api.md](api.md#get-apiv1user)), which need a user, the admin routes
`POST /orgs`, `GET /orgs` and `PATCH /orgs/:id`, and `GET /orgs/:id`, which
an admin or a member reads (see [api.md](api.md#post-apiv1orgs)).
`(*identity.Service).Routes()` in `internal/identity/routes.go` returns
them all: it appends the route list of each handlers file, such as
`userRoutes()` in `internal/identity/handlers_users.go` and
`ownUserRoutes()` in `internal/identity/handlers_user.go` and `orgRoutes()` in
`internal/identity/handlers_orgs.go`, so the hub and the
tests register the same routes. The providers in
`identity.Deps.Providers` come with their endpoints resolved:
`internal/config` fills an endpoint the file does not give with the
provider's public one, through `config.Provider.WithPublicEndpoints`, and
holds the only table of those endpoints. `internal/identity/provider.go`
holds what the configuration does not: the providers the hub can log in
with, the scopes it asks each for and Google's extra `response_type=code`.

`internal/identity/oauth.go` reads a user's identity from a provider, with
the standard library only. `identity.NewOAuthClient(providers, hc,
timeout)` returns an `*identity.OAuthClient`, the `identity.ProviderClient`
whose `Identity(ctx, provider, code, redirectURI)` returns an
`identity.Identity` (`Provider`, `ProviderID`, `Username`, `FullName`,
`Email`). It exchanges the code with one form-encoded `POST` to the token
URL (`client_id`, `client_secret`, `code`, `redirect_uri`,
`grant_type=authorization_code`, `Accept: application/json`), then reads
the user URL with `Authorization: Bearer <access token>`. For GitHub the id
is the decimal `id`, the username `login` and the full name `name`; when
the profile's `email` is null or empty it reads the user URL with `/emails`
appended and takes the entry whose `primary` and `verified` are both true.
For Google the id is `sub`, `name` is both the username and the full name,
and `email` counts only when `email_verified` is true. The username is
trimmed, and an empty one becomes `<provider>-<provider id>`; the full name
is the provider's as given.
Every error is `identity.ErrRejected` (the token endpoint answered a JSON
object with an `error` member and a status below 500) or
`identity.ErrUpstream` (everything else: no connection, a timeout, a status
of 500 or above, a body that is not the expected JSON, no access token, a
user or e-mail list answer other than `200`, no user id, no verified
address), and its text is fixed words only, never the code, the client
secret, the access token or the provider's body. Each call to the provider
is bounded by the timeout (0 is `identity.DefaultProviderTimeout`, 15
seconds); when it passes, the request's context ends and the transport
closes the connection. `hc` nil is `http.DefaultClient`.

The schema step creates, in the transactional store:

| Table | Columns |
|---|---|
| `users` | `id` (primary key), `username` (unique), `email` (unique), `full_name`, `role` (`user` or `admin`), `status` (`active` or `blocked`), `provider` and `provider_id` (unique as a pair), `created_at`, `updated_at` |
| `orgs` | `id` (primary key), `name` (unique), `slug` (unique), `url`, `owner_id` (set to NULL when that user is deleted), `status`, `created_at`, `updated_at` |
| `memberships` | `org_id` and `user_id` (primary key as a pair; deleted with the org or the user), `created_at` |
| `api_keys` | `key_id` (primary key), `user_id` (deleted with the user), `secret_hash`, `expires_days`, `expires_at`, `revoked_at`, `created_at` |
| `pats` | `token_id` (primary key), `user_id` (deleted with the user), `secret_hash`, `name`, `permissions` (a JSON array), `paths` (a JSON array, NULL without path limits), `workspace` (NULL when not bound), `expires_at`, `revoked_at`, `created_at` |

Timestamps are nanoseconds since the Unix epoch, UTC; `expires_at` and
`revoked_at` are NULL when unset. `secret_hash` is the lowercase hex SHA-256
of the secret part of the credential; the secret is never stored.

`identity.IssueAPIKey(ctx, tx, userID, expiresDays, now)` and
`identity.StorePAT(ctx, tx, identity.PATSpec{UserID, Name, Permissions,
Paths, Workspace, ExpiresAt, CreatedAt})` generate a credential from
`crypto/rand` (an API key `parley_<8>_<32>` over `[0-9A-Za-z]`, a PAT
`parley_pat_<8>_<32>` over `[a-z0-9]`, each character drawn uniformly),
draw the id again while it is taken, store the record in the transaction
they are given and return the credential, which is shown once, with its
record (`identity.APIKey`, `identity.PAT`). An API key expires `expiresDays`
× 24 hours after `now`, never with 0. `StorePAT` validates nothing beyond
the record's shape. `identity.UserByID`, `identity.APIKeyByID` and
`identity.PATByID` read a record through an `identity.Querier` (the store's
`*sql.DB` or a `*sql.Tx`) and return `identity.ErrNotFound` when there is
none. `identity.User` marshals to the REST API's User object.

`(*identity.Service).Authenticate` reads the `Authorization` header (see
[api.md](api.md#authentication)), and `(*identity.Service).VerifyCredential(ctx,
credential)` checks the credential, the same check for a credential read
elsewhere: it recognises the format, verifies an admin token with
`identity.VerifyAdminToken`, or looks the key or PAT up by its id and
compares the SHA-256 of the presented secret with the stored hash with
`crypto/subtle.ConstantTimeCompare`, against a fixed dummy hash when the id
is unknown, so that an unknown id and a wrong secret take the same path.
Only after the secret matched does it look at the credential's state, in
this order: `revoked_at` set is `401 credential revoked`, the clock of
`identity.Deps.Now` at or after `expires_at` is `401 credential expired`,
and a user whose status is `blocked` is `403 user is blocked`. It reads the
record and the user from the store on every call and caches nothing.
It returns an `identity.Principal` (`Type` `admin`, `api_key` or `pat`,
`CredentialID`, the `User`, and a PAT's `Permissions`, `Paths` and
`Workspace`) or the `*server.Error` that answers the request. The
authenticator reports a `server.Caller` whose `CredentialID` is `admin`, the
`key_id` or the `token_id`, whose `UserID` is the user's id, whose `Admin`
marks the admin token, and whose `Detail` holds the `Principal`; checks and
handlers read it with `identity.PrincipalOf(r.Context())`. A route declares
that it needs a user with the mark `identity.MarkNeedsUser` in its
`server.Route.Marks`; the authenticator reads it right after accepting the
credential and answers the admin token `403 forbidden` there.

The login, in `internal/identity/login.go`, decodes the callback's body
and checks it before the provider is called: `provider`, `code` and
`redirect_uri` are non-empty strings, `provider` is one of
`Deps.Providers`, `expires` is absent, `null` or one of 0, 30, 60 and 90,
and `redirect_uri` passes `allowedRedirect` in
`internal/identity/redirect.go` (http on `localhost` or `127.0.0.1`, or the
scheme, host and port of `Deps.ExternalURL`; never with user information);
each violation is `400 invalid_request`. It then reads the identity through
`Deps.Client`, answering `identity.ErrRejected` `401 unauthenticated` and
`identity.ErrUpstream` `502 upstream_error`, each with the error's fixed
text, and signs the user in, in one
`(*store.Store).Tx`: it looks the user up with `identity.UserByProvider`,
and at a first login picks the username (the provider's, else the first
free of `<name>-2` to `<name>-10`) and the role (`admin` when the e-mail is
the `admin_email` of `admin_settings`, read in the same transaction,
ignoring letter case, and no user has the role `admin`), then calls
`identity.CreateUserAndPersonalOrg`. A user it finds is refreshed instead:
a blocked user is answered `server.Forbidden("user is blocked")`, an e-mail
another user has is an `ErrConflict`, a username another user has keeps
the old one, and the new e-mail, username and `updated_at` are written;
role, full name, creation time and the personal org are left alone. Then,
for a new and an existing user alike, it sets `revoked_at` on every API key
of the user with `revoked_at` NULL and `expires_at` NULL or after the
login time, and last it issues the API key with `identity.IssueAPIKey`,
`expires` days (90 when absent) from the clock of `Deps.Now`. Any error
rolls the whole transaction back.

`identity.CreateUserAndPersonalOrg(ctx, tx, identity.NewUser{Username,
Email, FullName, Provider, ProviderID, CreatedAt}, role)` is the one way a
user is created: it writes the active user, the user's personal org (an
`identity.Org`, active, named after the username, owned by the user) and
the membership in the transaction it is given, and returns the user and
the org. The org's slug comes from the pure function `slug(username,
userID)` in `internal/identity/slug.go`, or the first of `<slug>-1` to
`<slug>-10` no org has. User and org ids are UUID version 4 strings from
the standard library's `uuid`. It checks before it writes, and returns an
error that is `identity.ErrConflict` (`errors.Is`), writing nothing, when the
username, the e-mail or the provider identity is another user's, an org has
the username as its name, or no slug is free; a unique-constraint violation
of SQLite on the insert is reported the same way. The callback answers that
error `409 conflict` with its text. Its caller chooses the username and the
role.

The admin routes carry the mark `identity.MarkAdmin` (`identity.admin`) in
their `server.Route.Marks` and declare no scope. `(*identity.Service).AdminCheck`,
in `internal/identity/admincheck.go`, is an access check: on a route without
the mark it lets the request through; on one with it, it passes the
`identity.Principal` whose `Type` is `admin`, or `api_key` with a `User`
whose `Role` is `admin`, and answers every other caller, PATs included,
`server.Forbidden("this route needs an admin")`. It reads the role of the
`Principal` the authenticator built for this request from the store, so a
change of role holds from the next request on. Because it runs in the access
position, after the body's size and media type and before the handler, a
refused request is not validated, looks nothing up and changes nothing.

`POST /users` (`createUser`) decodes the body into a
`map[string]json.RawMessage`, so that a missing member, `null` and a value
of another type are told apart: `username`, `email`, `provider` and
`provider_id` must be non-empty JSON strings and `full_name`, when present,
a JSON string, else `400 invalid_request`. It then calls
`identity.CreateUserAndPersonalOrg` with the role `user` in one
`(*store.Store).Tx`, with `CreatedAt` from `Deps.Now`, answers its
`ErrConflict` `409 conflict` with the error's text, and answers `201` with
the User object. It never promotes: the first-admin rule is the login's.

`identity.UserBySelector(ctx, q, sel)` in `internal/identity/users.go`
resolves a user selector by its shape alone: a value of 36 characters that
the standard library's `uuid.Parse` accepts is looked up as the `id`, any
other value with an `@` as the `email`, and every other value as the
`username`, each with SQLite's default binary comparison, so exactly. It
returns `identity.ErrNotFound` without trying another column, and the
handlers answer that `404 not_found` with the message `user not found`.
`GET /users/:id` reads through the store's `*sql.DB`; `PATCH /users/:id`
reads the body first, with the rule of `PATCH /user` (`fullNameOf` in
`handlers_users.go`), then resolves the selector and calls `updateFullName`
in the same `(*store.Store).Tx`. `GET /users` reads one page with
`listUsers(ctx, q, includeBlocked, after, limit)` in `users.go`: `status =
'active'` unless the query parameter `include_blocked` is exactly `true`,
`ORDER BY created_at DESC, id DESC`, continuing after the position
`{created_at in nanoseconds, id}` that the cursor of `server.ReadPage`
carries, and writes it with `server.WritePage`.

Promote, demote, block and unblock (`setRole` and `setStatus` in
`handlers_users.go`) go through `changeUser`, which resolves the selector
and applies the change in one `(*store.Store).Tx`. A user who already has
the requested role or status is answered as read, without a write, so
`updated_at` stays; otherwise `updateRole` or `updateStatus` in `users.go`
sets the column and `updated_at` (from `Deps.Now`). No credential is
touched: authentication refuses a blocked user's credentials on its own.
The last-admin guard is `guardLastAdmin(ctx, tx, u)` in `users.go`: it
returns the conflict `the user is the only active admin` when `u` has the
role `admin` and the status `active` and no other user has both, and nil
otherwise. Demote calls it before `updateRole`, in the same transaction
that read `u`; since the store admits one writer at a time, the count and
the change cannot interleave with another demotion or deletion. Block has
no guard.

`GET /user` and `PATCH /user`, in `internal/identity/handlers_user.go`,
carry the mark `identity.MarkNeedsUser` and declare the scopes `users:read`
and `users:write`. Both take the caller's user from the `identity.Principal`
of the request. `PATCH /user` decodes the body into a
`map[string]json.RawMessage` and needs `full_name` as a JSON string, else
`400 invalid_request`; it stores it in one `(*store.Store).Tx` through
`updateFullName(ctx, tx, id, fullName, now)` in
`internal/identity/users.go`, which sets `full_name` and `updated_at` (from
`Deps.Now`) and returns the stored user, or `identity.ErrNotFound`.

`internal/identity/orgs.go` holds the org reads: `identity.Org` marshals to
the REST API's Org object (`id`, `name`, `slug`, `url`, `owner_id`, `null`
when `OwnerID` is empty, `status`, `created_at`, `updated_at`);
`identity.OrgByID(ctx, q, id)` and `identity.OrgBySlug(ctx, q, slug)`
return an org or `identity.ErrNotFound`, comparing the slug exactly; and
`identity.Memberships(ctx, q, orgID)` returns an org's
`identity.Membership`s (`OrgID`, `UserID`, `CreatedAt`), oldest first, then
by user id. It reads through an `identity.RowsQuerier`, an
`identity.Querier` that also has `QueryContext` (the store's `*sql.DB` or a
`*sql.Tx`).

`identity.OrgBySelector(ctx, q, sel)` in `orgs.go` resolves an org selector
by its shape alone, as `UserBySelector` does a user's: a value `isUUID`
accepts is looked up as the `id`, every other value as the `slug`, exactly,
and `identity.ErrNotFound` is returned without a second lookup. The org
routes are `orgRoutes()` in `internal/identity/handlers_orgs.go`, which
`Routes()` appends. `POST /orgs`, `GET /orgs` and `PATCH /orgs/:id` are admin
routes. `POST /orgs` (`createOrg`) decodes the body into raw members, trims
the name (`orgNameMember`), checks the slug against `^[a-z0-9](?:[a-z0-9_-]{0,126}[a-z0-9])?$`
(`validSlug`), and in one `(*store.Store).Tx` reads the `owner_id` with
`UserByID` (not found is `400`) and calls `createOrg(ctx, tx, o)`, which
answers a taken name or slug with a conflict and inserts the org with a
new UUID, `owner_id` NULL when there is none, and no membership.
`PATCH /orgs/:id` checks the body first, then resolves the selector and
calls `updateOrg(ctx, tx, id, name, url, now)`, which refuses a name another
org has (`id <> ?`) and sets only the members given and `updated_at`.
`GET /orgs` reads one page with `listOrgs(ctx, q, includeBlocked, after,
limit)`: `ORDER BY name`, continuing after the position `{name}` of the
cursor. `GET /orgs/:id` carries no admin mark and declares the scope
`orgs:read`; it goes through `visibleOrg(r)`, the one rule for reading an
org: an admin (`isAdmin`) reads every org, any other caller an active org
of which its user is a member (`isMember`), and every other case, a missing
org included, is the one answer `errOrgNotFound()`, `404 not_found` with the
message `org not found`.

### The logger

`hub.NewLogger(w, level, scrubber)` returns a `*slog.Logger` with
`log/slog`'s JSON handler on `w`, one object per line. The levels are
`hub.LevelTrace` (`slog.Level(-8)`, written as `TRACE`), debug, info, warn
and error; lines below the configured level are dropped. Its `ReplaceAttr`,
which the JSON handler calls for the message and every attribute, those of
`With` and of groups included, passes every string value and the text of
every `error` value through `scrubber.Scrub`. Any other value is scrubbed
as its `fmt` text (a `[]byte` as text): when scrubbing changes that text
(it removes something, or cuts a text of more than 1024 bytes), the
scrubbed text is written in the value's place, and otherwise the value keeps
its JSON form. The values of an `http.Header`'s `Authorization` and
`Proxy-Authorization` headers are written as `[redacted]`, whatever their
shape. Keys, `time` and `level` are written as they are. `Run` builds it on `Stderr` before the configuration is
loaded, at info, sets its level once the configuration is loaded, installs it
with `slog.SetDefault`, and hands it to the server as `Options.Logger`.

### Idempotency records

`internal/hub/idempotency.go` adds the schema step `idempotency_records` to
the transactional store: a table `idempotency_records` with `key`, `caller`,
`route` (together the primary key), `request_hash` (the 32-byte SHA-256),
`body`, `header` (the headers as JSON) and `written_at` (nanoseconds since
the Unix epoch). Its `server.IdempotencyStore` finds a record by key, caller
and route, writes one with an upsert in a transaction of `Tx`, and deletes
the records written at or before a cutoff. The server calls `DeleteExpired`
with its clock, the hub's `Now`, so a record written before a stop is
replayed after the next start until it is 24 hours old.

The flags, exit statuses and log lines are described in [cli.md](cli.md),
the configuration in [configuration.md](configuration.md).

### Readiness probes

A readiness probe is a `server.Probe`, a `func(context.Context) error` that
returns an error when one part of the hub is not ready. `GET /readyz` calls
every probe with the request's context, including those after one has failed,
and answers `503` with `{"status":"not ready"}` if any failed. With no probe
registered the hub is ready. A probe's error text never goes into the
response.

## parley

`cmd/parley` calls `cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)` and passes
its result to `os.Exit`. `Run` is `cli.RunWith` with zero `cli.Hooks`;
`RunWith` lets a caller that runs the client in-process, such as an
end-to-end test, replace what it takes from the process (`Hooks.Stdin`, where
`parley login --api-key -` reads the credential; `Hooks.OpenBrowser`, which
opens the sign-in page, by default the system's opener in
`internal/cli/browser.go`; and `Hooks.LoginWait`, how long the browser login
waits for the redirect, by default 120 s). It builds a fresh cobra
command tree on each call: the root command `parley` with the persistent
global flags `--endpoint-url`, `--api-key` and `--json`, and the `help`,
`version` and `login` subcommands, plus the commands the unexported `options.extra`
builds, which tests use to add probe commands. It executes the tree with the
arguments; when a command returns an error, `run` writes it to stderr with
`printError` and returns `exitCode(err)` (both in `internal/cli/errors.go`).
Cobra's own error and usage output is silenced, so stderr holds the one
envelope.

A command is declared through one helper, `declare` in
`internal/cli/command.go` (see
[ADR 02](adr/02-declare-every-cli-command-through-one-helper.md)): a
`command` value with its last word, its description, the route it calls
(`Method` and `Path` relative to `/api/v1`, empty when it calls no single
route), its named arguments (`argSpec`), its `auth` level (`none`, `user`,
`admin`), its own flags and its run step. `declare` keeps the declaration as
JSON in the cobra command's `Annotations` (`parley.declaration`), refuses a
wrong number of arguments, and a flag marked required with
`cobra.MarkFlagRequired` that is not given, as a `usage_error`
(`checkCommandLine`, which runs before cobra's own check of required flags,
whose error has no kind), and runs the run step with a
`call`: the context, the arguments by name, stdout, stderr and the run's
options, which `run` passes down in the context. `group` builds a command
group, which prints its help without a word and refuses a word that is none
of its children as a `usage_error` (cobra alone would print the help); the
root is a group too, and its flag error handler turns every flag that is
unknown or does not parse, on any command, into a `usage_error`.
`parley version` and `parley login` are declared through `declare`.

Help lives in `internal/cli/help.go`. `installHelp` keeps cobra's own text
help, sets the root's help function, which `--help` on any command and a
group run without a word reach, and replaces cobra's help command with
`help [command]...`, declared through `declare` (auth `none`, no route; its
argument `Repeats`, taking every remaining word). The help command finds its
words with the root's `Find`; words left over are a `usage_error` naming
the first, and otherwise it prints what `--help` on that command prints,
after adding cobra's `--help` flag to it as cobra does when it runs it. With
the global `--json` the help is one JSON document instead: `describe`
builds a command's object from its declaration (`declarationOf`, which
decodes the `parley.declaration` annotation; an undeclared command has an
empty description and auth), with `method` and `path` `null` when empty and
`flagsOf` listing its local and inherited flags sorted by name, without the
flags cobra adds itself; for a group (`isGroup`, the `parley.group`
annotation), the root included, `describeAll` lists every command under it
that is not a group, at any depth, sorted by name. Help resolves no setting,
reads no file and sends no request.

Before the run step of a command whose `auth` is `user` or `admin`,
`declare` resolves its settings with `resolveSettings`
(`internal/cli/config.go`): the hub URL from the first non-empty of the
global flag `--endpoint-url`, `PARLEY_ENDPOINT_URL` (these two through
`givenHubURL`, which `parley login` and `parley version` share) and the
file's `endpoint_url`, the credential from `--api-key`, `PARLEY_API_KEY` and
`api_key`. It calls `readConfig` only when the flags and the environment
leave one empty. A missing setting (a missing hub URL with the message of
`noHubURL`, which `parley login` shares), a file that cannot be parsed and
a hub URL that is not an absolute `http` or `https` URL with a host and
without a query or fragment (`checkHubURL`) are `client_error`s, returned before the run step starts,
so no request is sent. The resolved settings build the call's `client`
(`internal/cli/client.go`) with `call.newClient`, which gives it the
declaration's `Timeout` or else `options.requestTimeout`
(`defaultRequestTimeout`, 30 s; tests shorten it). A command whose `auth` is
`none` gets no client: it needs no credential and builds one itself for the
hub URL it resolves.

`client` is the one HTTP client of the commands, `parley login` and
`parley version` included. `newClient(hubURL, credential, timeout)` drops
the URL's trailing slashes. An `apiRequest` holds the method, the path
(relative to `/api/v1`, or with `outsideAPI` to the hub URL itself, as
`GET /version` is), the query, the body and an optional timeout of its own.
`raw(ctx, r)` sends it to `<hub URL>/api/v1<path>` with
`Authorization: Bearer <credential>` (none when the credential is empty),
`Accept: application/json`, and the body encoded as JSON with
`Content-Type: application/json` only when there is one, and returns the
status and the whole body whatever they hold, for a command whose output is
not one JSON document. The timeout is a context around connecting, sending
and reading the whole body; when it runs out the answer is a `client_error`
(`the hub at <hub URL> gave no complete answer within <timeout>`), and any
other failure is a `client_error` naming the hub URL: to connect or send,
`cannot reach the hub at <hub URL>: <reason>`; to read the answer,
`cannot read the answer of the hub at <hub URL>: <reason>`. `send(ctx, r)` is `raw`
that accepts only an answer of the REST API: a status of `400` or above
whose body is an object with an `error` member holding `error_type`
(`isHubEnvelope`, in `errors.go`) becomes a `hubError`; any other status
outside `2xx`, and a `2xx` body that is neither empty nor JSON, a
`client_error` naming the status. `request(ctx, method, path, query, body)`
is `send` for the common case. Each client has its own transport, a clone
of `http.DefaultTransport` with keep-alives disabled: Go's transport can
send a request again on a new connection when a reused one breaks, and the
hub may have received it the first time, so every connection serves one
request and nothing in the client retries one. Its `CheckRedirect`
(`keepRedirect`) returns a redirect as the answer instead of following it,
which would send the request again (a `POST` with its body after a `307`
or `308`); `send` then refuses it as a status outside `2xx`.

`internal/cli/errors.go` holds the two kinds of error a command ends with.
`hubError` is an error envelope the hub answered, with the HTTP status and
the body's bytes as received, which `printError` writes indented again by
two spaces with `json.Indent`, so its member order and extra members stay
as the hub sent them. `cliError`
is an error the CLI raises itself, built by `usageError`, `clientError`,
`waitFailed`, `waitTimeout` and `conditionNotMet`; `printError` writes it as
`{"error":{"code":0,"message":…,"error_type":…}}` indented by two spaces,
and any other error as a `client_error` with its text as the message.
`exitCode` maps no error to `0`, a `hubError`, `wait_failed` and
`wait_timeout` to `1`, `condition_not_met` to `3` and everything else to
`2`. `parley login` returns these errors too.

`internal/cli/output.go` prints what a command answers. A command declared
without a run step gets the default one, `sendAndPrint`: it fills each
`:param` segment of `Path` with the argument of that name, percent-encoded
as one segment (`routePath`, `pathSegment`: `url.PathEscape`, and `.` and
`..` as `%2E` and `%2E%2E`), sends `Method` to it with the call's client's
`send` and passes the success to `printAnswer`. A `2xx` answer without a
body is replaced by `{}`, or by the document the declaration's `Confirm`
builds. `printResult` then writes the result with `writeResult`, which
compacts the body and indents it by two spaces with `json.Indent`, never
decoding it, and adds one newline; writes the line `warning: this value is
shown once and will not be shown again` to stderr for a declaration marked
`ShowsOnce`; and, for each of the declaration's `FailOn` conditions whose
flag `--fail-on-<Name>` (registered by `declare`) is given, returns
`condition_not_met` with the condition's message when `Check` says it holds
in the result, so the result is on stdout before `run` prints the error.
`warn` and `progress` write a plain line to stderr, a warning prefixed with
`warning: `; `parley version` writes its warnings with `warn`. `parley
login` prints the user with `printResult`.

`internal/cli/wait.go` holds the shared wait for a long operation. A
command declares `Wait`, a `waitFor`: `Start` (optional; the request that
starts the operation, by default `Method` to `Path`), `Poll` (the request
that asks for the operation's state, built from the record last received),
`Classify` (a record's state and whether it is not terminal, `successful`
or `unsuccessful`; `byState` builds one from a string member and the lists
of successful and unsuccessful states) and `Document` (optional; the
document printed in place of the final record, from the start's answer and
the final record). `declare` registers `--wait` (bool), `--timeout` and
`--poll-interval` (`durationValue`, a `pflag.Value` whose `Type` is
`duration`, defaults `5m` and `5s`) and makes `call.wait` the run step of a
command that declares no `Run`. `durationValue.Set` parses with
`parseDuration` (a whole number above 0 and one of `s`, `m`, `h`, `d`), so
a value it refuses is a flag that does not parse, a `usage_error` from the
root's flag error handler before any request, with or without `--wait`.
The client does not import the hub's `internal/server/timefmt`; the parser
is its own. `call.wait` sends the start request through the call's client;
without `--wait` it prints the answer with `printAnswer`. With `--wait` it
classifies the start's record, writes `waiting: <state>` with `progress`,
and until a record is terminal waits one poll interval (or the
`options.pollInterval` tests set below one second), sends the poll under a
context whose deadline is `--timeout` after the start's answer came, and
writes `waiting: <state>` when the state changes. The final record, or
`Document`'s document, is printed with `printResult`; an unsuccessful end
then returns `wait_failed` naming the state. When the deadline passes,
while sleeping or with a poll in flight (the client's error then names the
abandoned request, not the reason), `waitEnded` returns `wait_timeout`;
any other error of a poll (the hub's envelope, a `client_error`, a poll's
own request timeout before the deadline) ends the wait as it is. Nothing
is printed on stdout then, and no request cancels the operation.

`internal/cli/config.go` is the one implementation of the client
configuration file. `configPath` gives `$PARLEY_CONFIG_DIR/config.toml`, or
`~/.parley/config.toml`, and an error when it needs the home directory and
`os.UserHomeDir` has none. `readConfig` returns the file's `endpoint_url`,
`api_key` and `user_id` as a `configSettings`; a missing file or no path
gives empty settings, and a file that cannot be read or parsed is a
`client_error` naming the path. `saveConfig` takes a `configSettings`,
decodes the existing file with `github.com/BurntSushi/toml` into a map, so
that keys it does not know survive, sets the three keys and encodes the map;
it writes a temporary file in the same directory (created with mode `0700`
when missing), syncs it, sets its mode to `0600` and renames it over
`config.toml`. Every failure, including a file it cannot parse, is a
`client_error` naming the path, with the temporary file removed and the old
file left as it was; without a path, because there is no home directory,
it is `cannot save the config file: <configPath's error>`.

`parley login --api-key`, with the global flag (`internal/cli/login.go`),
takes the credential with `loginCredential` (an empty one is a
`usage_error` before anything is read or sent), reads the client
configuration file with `readConfig`, resolves the hub URL
from the flag, `PARLEY_ENDPOINT_URL` and the file, and asks the hub who the
credential belongs to with `GET /api/v1/user` through a `client` that
presents it (30 s timeout). On a `2xx` answer it saves `endpoint_url`,
`api_key` and `user_id` with `saveConfig`. A hub URL that is not an absolute
`http` or `https` URL with a host and without a query or fragment is
refused with `checkHubURL`, as for every command.

Without `--api-key`, `parley login` signs in through the browser
(`browserLogin` in `internal/cli/login.go`): it validates `--expires`,
reads the config file and resolves the hub URL as above, takes the
provider's `authorize_url` from `GET /api/v1/auth/providers`, listens on
`127.0.0.1:0`, and opens the `authorize_url` with `redirect_uri` (that
listener's `/callback`) and a 32-hex-character `state` from `crypto/rand`
added. A `net/http` server on the listener serves `GET /callback` once: it
checks the state in constant time, answers the browser with an HTML page,
and hands the code back; the server is shut down and the listener closed on
every way out, including the wait's expiry. The code goes to
`POST /api/v1/auth/callback` without a credential, and the answer's key and
user are saved as in the key flow. Both flows send their requests through
the shared `client` with its `request` and `send`; the requests before the
credential is known carry no `Authorization` header, and the callback states
its own timeout of 60 s. A hub's error envelope comes back as a `hubError`,
as it was received.

`parley version` reads its build from `internal/buildinfo`, takes the hub URL
from `--endpoint-url`, `PARLEY_ENDPOINT_URL` and then `readConfig` (a file it
cannot parse is a warning, not a failure), and asks the hub for its build
through the shared `client` (`GET /version` with `outsideAPI`, no
credential, its own 5 s timeout from `options.versionTimeout`); every
failure of that request, and an answer other than `200` with a JSON object,
is a warning. `internal/cli` imports `internal/buildinfo` and nothing from
`internal/server`, so the client carries no server code. The commands, output
and exit statuses are described in [cli.md](cli.md).

## Build information

`internal/buildinfo` has three package-level string variables, `Version`,
`Commit` and `BuildTime`, that are set when the binary is linked, with one `-X` linker flag each:

```sh
go build -ldflags "\
  -X github.com/agent-fox-dev/parley/internal/buildinfo.Version=1.4.0 \
  -X github.com/agent-fox-dev/parley/internal/buildinfo.Commit=3f2a1bc \
  -X github.com/agent-fox-dev/parley/internal/buildinfo.BuildTime=2026-01-02T03:04:05Z" \
  ./cmd/parleyd
```

The same flags build `./cmd/parley`. `test/e2e` builds both programs this way
and checks that `parley version` reports the values.

`buildinfo.Get()` returns them as a `buildinfo.Info`, with the Go toolchain's
version (`runtime.Version()`) as `GoVersion`. A value that was not set is
reported as `dev` (version) or `unknown` (commit and build time); a value that
was set is reported unchanged. `GET /version` calls `Get()` on every request.

The package lives under `internal/` and outside `internal/server` so that the
client can report its own build without importing server code.
