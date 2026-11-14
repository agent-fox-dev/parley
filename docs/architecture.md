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
| `internal/access` | the workspace access rule, the one place that decides what a caller may do on a workspace; the minimal workspace record (slug, owner, org, kind, parent, status) in the transactional store, the caller's level on a workspace, the decision for a declared need and the REST access check, and the readable set for reads across workspaces |
| `internal/buildinfo` | the version, commit and build time the binary was built with, shared by both programs |
| `internal/cli` | the `parley` client: its root command and its subcommands, built with `github.com/spf13/cobra`, each declared through one helper ([ADR 02](adr/02-declare-every-cli-command-through-one-helper.md)), the client configuration file, the resolution of the hub URL and the credential, the one HTTP client the commands send their requests with, the printing of a command's result on stdout and of its warnings on stderr, the shared wait for a long operation (`--wait`, `--timeout`, `--poll-interval`), and the error envelopes and exit statuses every command ends with |
| `internal/config` | the configuration loader: `config.toml` from `$XDG_CONFIG_HOME` or the working directory, `${NAME}` references, and a registry of keys and whole sections that decides what the file may hold |
| `internal/hub` | the composition of the `parleyd` process: argument parsing, the scrubbing logger, the fixed start order, the bootstrap of the admin token, the HTTP server's options, the idempotency records in the transactional store, signals and the stop, and the `backup` command; the one place later packages plug their parts in ([ADR 01](adr/01-compose-the-hub-in-one-package.md)) |
| `internal/maintenance` | the maintenance loop: a registry of named tasks, and `Loop`, which runs one pass at once and one at each tick of an hourly timer, the tasks of a pass one after the other in a fixed order, and logs a failing task without stopping the others |
| `internal/hubtest` | test support, imported by test code only: `Start` bootstraps a hub with an admin token and serves it in-process on a free loopback port, `StartWith` does the same with more in `config.toml` (`Config.Server` inside `[server]`, `Config.TOML` after it) and files in the config dir, `(*Hub).IssuePAT` stores a PAT, `CreateOrg` and `AddMember` make an org and a membership through the admin routes and `AddWorkspace` stores a workspace record, `Serve` runs a hub with given options until the test ends, and `LogBuffer` holds a running hub's log for the test to read |
| `internal/identity` | who may use the hub: the admin settings in the transactional store (the bootstrap e-mail and the admin token's hash), the generation and hashing of admin tokens, and `VerifyAdminToken`, which compares a presented token's hash with the stored one in constant time; the identity records (users, orgs, memberships, API keys, PATs), the generation of API keys and PATs, and `Service`, the pipeline's authenticator |
| `internal/sandbox` | the hub's side of the sandbox feature: `Register` makes it the owner of the whole `[sandbox]` section of the configuration, and `FromConfig` checks that section after the load, with the config dir and `[server].external_url` in view, and returns its `Settings` with every default filled in, or one error listing every failure; the `Settings` give the profiles and the configured credentials by name (`Profile`, `DefaultProfile`, `Credential`) and in name order (`Profiles`, `Credentials`), as copies, so that they do not change after the load; `RunStartCheck` checks the runtime once at start (see [The start-up check](#the-start-up-check)), `ReleaseRange` and `SupportedReleases` hold and compare the supported OpenShell releases, `Service` is the feature of one hub (settings, runtime client, connected release) and declares the sandbox routes (see [Sandbox route declarations](#sandbox-route-declarations)), `RuntimeError` is the `502 sandbox_runtime_error` answer, and `Recorder` is the metrics recorder of the sandbox packages |
| `internal/sandbox/runtime` | the hub's one boundary to the sandbox runtime: the `Client` interface with every operation the sandbox packages call, the plain data it exchanges, the four error kinds (`ErrNotFound`, `ErrInvalid`, `ErrUnsupported`, `ErrUnavailable`), the reachability flag (`Reachability`) and the metrics recorder (`Recorder`, `NopRecorder`) it reports to; it imports nothing of the project |
| `internal/sandbox/runtime/openshell` | the runtime client over the OpenShell Go SDK (`github.com/ai-openshell/sdk-go`, package `openshell/v1`), the only package that imports the SDK: `BuildConfig` turns the checked `[sandbox]` section into the SDK's configuration, `New` builds the client of a section and `NewWithClient` the client over a given SDK client, such as the SDK's in-memory one |
| `internal/sandbox/runtime/openshell/openshelltest` | test support, imported by test code only: `Gateway`, which stands in for the OpenShell gateway: the SDK's in-memory client with provider profiles kept per workspace and a switch that makes it unreachable and reachable again |
| `internal/sandbox/runtime/runtimetest` | test support, imported by test code only: `Fake`, the in-memory runtime and the only test double of the runtime, with its test controls, and `RunContract`, the contract tests every implementation of the runtime client passes |
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
`server.CallerOf(r.Context())`. A check returns only an error and cannot
hand on a request of its own, so the router's handler of each REST route
puts an empty holder in the request's context before the pipeline runs
(ADR 04): a check leaves a value for the later checks and the handler with
`server.Attach(r, key, value)`, and they read it with
`server.Attached(r, key)`, which reports `false` when nothing was attached
under `key`; outside a request to a REST route `Attach` does nothing and
`Attached` reports `false`. Each request has its own holder. An `*server.Error` (also when wrapped) is
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
| `SetSandboxRuntime(sandbox.RuntimeFactory)`, `SetSandboxRecorder(sandbox.Recorder)` | the factory of the sandbox runtime client (default: the OpenShell implementation) and the recorder of the sandbox metrics (default: none) |

`(*Composition).Sandbox()` returns the `*sandbox.Service` that
`composeSandbox` (`internal/hub/sandbox.go`) creates: a part listed after it,
or `Options.Compose`, keeps it and reads its settings and runtime client
once the hub serves. Two unexported registrations serve the parts of the hub
itself: `addConfigCheck(func(*config.Config) error)` adds a check run on the
loaded configuration before any store is opened, for rules that span more
than one key or section, and `addVersionMember(func() map[string]any)` adds
members to the document of `GET /version` (the server's
`Options.VersionMembers`), after the build's.

The composition also keeps the identity service in its unexported field
`identity`, which `composeIdentity` sets. A part listed after it that keeps
data for a user or an org registers the remover of that data there, with
`c.identity.RegisterUserRemover` or `c.identity.RegisterOrgRemover`
([ADR 03](adr/03-register-removers-of-owned-data-on-the-identity-service.md)).

`hub.Runtime` holds the loaded `*config.Config`, the open `*store.Store`, the
`*slog.Logger`, the `*scrub.Scrubber` and the clock `Now`. Start hooks,
maintenance tasks and background work receive it; a route handler or a check keeps
`(*Composition).Runtime()`, which the hub fills once the stores are open and
before the first hook runs or the first request is served. A start hook or a
maintenance task without a name or a function, or with a name already
registered, a configuration key or section the registry refuses, a
route the server refuses, and a route that breaks a route rule a part
added with the unexported `addRouteRule`, stop the start before anything is
opened; so does background work without a name or a function, or with a
name already registered. A route rule applies to every route of the
composition, including those `Options.Compose` adds after the hub's own
parts. `(*Composition).ShuttingDown()` returns the shutdown
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
   `[logging].level`, run the configuration checks of `addConfigCheck`
   (the `[sandbox]` section's, see [The start-up check](#the-start-up-check)),
   and register every OAuth provider's `client_secret` with the scrubber;
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
method of an `identity.Service` as the pipeline's authenticator, installs
`identity.ScopeCheck` with `SetScopeCheck`, adds the service's
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
[api.md](api.md#role-and-status-of-a-user)), the admin route
`DELETE /users/:id` (see [api.md](api.md#delete-apiv1usersid)), the admin
routes `GET /users/:id/keys`, `GET /users/:id/tokens`,
`DELETE /users/:id/keys/:key_id` and `DELETE /users/:id/tokens/:token_id`
(see [api.md](api.md#keys-and-tokens-of-a-user)), and the
caller's own profile, `GET /user`, `PATCH /user` and `GET /user/orgs` (see
[api.md](api.md#get-apiv1user)), which need a user, the admin routes
`POST /orgs`, a create route, `GET /orgs`, `PATCH /orgs/:id`,
`PUT /orgs/:id/members/:user_id`, `DELETE /orgs/:id/members/:user_id`,
`POST /orgs/:id/block`, `POST /orgs/:id/unblock` and `DELETE /orgs/:id`,
and `GET /orgs/:id` and `GET /orgs/:id/members`, which an admin or a member
reads (see [api.md](api.md#post-apiv1orgs)).
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

`internal/identity/scope.go` holds the scope registry and the scope check.
`identity.RegisteredScopes()` returns a copy of the 32 registered scopes in
their documented order, and `identity.IsRegisteredScope(s)` tells a
registered name from any other string, exactly. `identity.Satisfies(held,
needed)` is the one implication rule: `needed` itself, `R:write` for an
`R:read` where both are registered, `keys:manage` for `keys:read`, and
`tokens:manage` for `tokens:write` and `tokens:read`; nothing else, so
`changes:approve` and `checks:write` are granted only by name.
`identity.ScopeCheck` is a package-level `server.Check`, since it needs no
service state, which the hub installs in the pipeline's scope position. It
reads the `Scope` of `server.MatchedRoute(r).Route`, the one declaration a
route of any part makes, and the `identity.Principal` of the request: a
route without a scope, the admin token and an API key pass, and a PAT whose
`Permissions` do not satisfy the scope is answered
`server.InsufficientScope("this route needs the scope <scope>")`. A route
that declares a scope and whose request carries no `identity.Principal` (an
authenticator other than the identity service's) fails closed with
`server.Internal("the scope check found no authenticated principal")`
rather than passing. It runs before the access checks and the handler, so a
refused token learns nothing about the resource it addressed.

`internal/identity/pathglob.go` is the one implementation of a token's path
limits, pure functions with no state. `identity.ValidatePaths(v)` checks a
`paths` value as `encoding/json` decoded it (or a `[]string`): `nil` is no
path limit and returns `nil`; otherwise an array of 1 to 50 strings, each 1
to 255 characters and not starting with `/`, returned in the order given
with duplicates kept, or an error whose text names the entry it refuses
(`paths[1] is not a string`). `identity.MatchPath(glob, path)` matches the
whole path, case-sensitively: `*` is any run without `/`, `**` any run
including `/` (both may be empty, and both are wildcards wherever they
stand), a run of three or more asterisks reads as `**`, and every other
character, `?`, `[` and `\` included, is itself. `identity.MatchAny(globs,
path)` is the check of a token's list: a `nil` list allows every path,
otherwise one glob must match. The parts that refuse a write outside a
token's paths (git push, the SCM write API, opening changes and auto-land)
call `MatchAny` with the `Paths` of the request's `identity.Principal` and
match no glob of their own. `identity.Covers(c, g)` is the cover test that
minting holds a path-limited token to: `c` equals `g`, or `c` read as a
pattern matches the text of `g`, where a `*` of `c` takes no `/` and no
asterisk of a `**` in `g`, and a `**` of `c` takes anything, so `c` matches
every path `g` does. `identity.CoveredByAny(globs, g)` and
`identity.FirstUncovered(held, requested)` (the first requested glob no held
glob covers, and `true`) apply it to lists, a `nil` list covering
everything. Matching and covering are one pass over the subject per element
of the glob, never exponential.

`internal/identity/token.go` holds minting, the one way a PAT is created.
`identity.MintPAT(ctx, tx, caller, req, now)` takes the caller's
`identity.Principal`, an `identity.MintRequest{Name, Permissions, Paths,
Expires *int, ExpiresAt *time.Time, Workspace}` and the clock's `now`, and
writes through the transaction it is given without committing, so a caller
can mint a token together with changes of its own. It validates the values
(`Name` 1 to 255 characters, `Permissions` at least one registered scope,
duplicates dropped and the order kept, `Paths` through `ValidatePaths`,
`Expires` 0, 30, 60 or 90 with `nil` meaning 90), each failure a
`400 invalid_request` that stores nothing, then applies the subset rule
(`CheckSubset`, below), then stores the token with
`StorePAT` for `caller.User`, created at `now`. `expires_at` is `Expires` ×
24 hours after `now` truncated to whole seconds, or `nil` for 0, so the
instant the API shows is the instant the token stops authenticating; a
hub-internal caller sets `ExpiresAt` instead, kept to whole seconds.
`Workspace` is `""` on `POST /user/tokens`; binding a token is the
workspace-token minting component's, which calls `MintPAT` with it.
`identity.PAT` marshals to the REST API's Token object (`token_id`,
`user_id`, `name`, `permissions`, `paths`, `workspace`, `expires_at`,
`revoked_at`, `created_at`, `null` for what is unset), which never holds
the token's value; only the creation answer adds `token`.

`internal/identity/subset.go` holds the subset rule, so that every way of
creating or widening a token applies the same code.
`identity.CheckSubset(caller, permissions, paths)` returns `nil` unless
`caller.Type` is `TypePAT`; for a PAT it returns a `403
insufficient_scope` `*server.Error` naming the first requested permission,
in request order, that is not in `caller.Permissions` (exact membership, no
implication), else, when `caller.Paths` is not `nil`, saying `paths` is
required for a `nil` request or naming the first requested glob
`FirstUncovered` reports. `identity.CheckWidenSubset(caller, added,
targetPaths)` is the rule for a request that adds scopes to an existing
token, whose paths do not change: it returns `nil` when `added` is empty,
then refuses, in the same order, the first added scope not in
`caller.Permissions`, and, when `caller.Paths` is not `nil`, a target
without paths (`paths: this token is limited to paths, so it cannot add
scopes to a token that is not`) or the first of the target's globs
`FirstUncovered` reports. Without the paths half a PAT limited to paths
could give a scope it holds to a token without that limit. Both expect
validated values: validation answers first.

`POST /user/tokens`, in `internal/identity/token_routes.go`, is a create
route with `ShowOnce: []string{"token"}`, so a replay under the same
`Idempotency-Key` answers `200` without the value. It declares the scope
`tokens:manage`, which the scope check holds a PAT to before the body is
read, and carries `identity.MarkNeedsUser`. Its handler decodes the body
into a `map[string]json.RawMessage` and checks only each member's JSON type
(`name` a string, `permissions` and `paths` arrays of strings, `paths` also
`null`, `expires` an integer), leaving the values to `MintPAT`, and runs
`MintPAT` in one `(*store.Store).Tx` with `Deps.Now()`.

`token.go` also holds the reads and revocations of PATs.
`identity.RevokeTokenByID(ctx, tx, tokenID, now)` sets `revoked_at` to
`now` when it is `NULL`, so a revoked token keeps its first `revoked_at`,
and `identity.SetTokenExpiryByID(ctx, tx, tokenID, at)` sets `expires_at`
to `at` kept to whole seconds and nothing else. Both write through the
transaction they are given without committing, check no owner, write no
event and return `ErrNotFound` for an unknown id: they are for hub-internal
callers (the hub credential of a sandbox), which change a token together
with changes of their own. Keys and tokens share one lookup by owner and
one list, in `internal/identity/owned.go`: `ownedTable[T]` names a
credential table (`keyTable` for `api_keys`, `tokenTable` for `pats`), its
`ofUser` answers a missing credential and another user's alike with the
table's `404` (`API key not found`, `token not found`), and its `writePage`
answers a page of one user's credentials in any state, newest first
(`created_at`, then the id, both descending) with keyset paging on those
two. The token routes look a token up with `tokenTable.ofUser` and list
with `tokenTable.writePage`; the admin routes call the same functions with
the selected user's id. `GET /user/tokens` and `GET /user/tokens/:token_id` declare
`tokens:read`, `DELETE /user/tokens/:token_id` declares `tokens:manage`
and revokes through `RevokeTokenByID` after the owner lookup, in one
transaction; every `/user/tokens` route carries `identity.MarkNeedsUser`,
so the admin token is refused by authentication and no handler refuses it
again.

`PUT`, `PATCH` and `DELETE /user/tokens/:token_id/permissions` share one
handler, `changeOwnTokenPermissions`, declaring `tokens:write`. In one
`(*store.Store).Tx` it looks the token up with `tokenTable.ofUser` (404), reads
the body's `permissions` with `permissionsChangeOf` (400; registered scopes
for `PUT` and `PATCH`, at least one entry for `PATCH` and `DELETE`),
computes the new list with `changedPermissions` (`PUT` the list without
duplicates, `PATCH` the current list with the new scopes appended, `DELETE`
the current list without the given ones), passes the scopes the new list
adds to the token's current ones and the token's paths to
`CheckWidenSubset` (403), refuses a
revoked or expired token with `409 conflict`, and writes with
`setTokenPermissions`, which stores `[]` and sets `revoked_at` in the same
`UPDATE` when the list is empty. Expiry is decided by `expired(expiresAt,
now)` in `authenticate.go`, the predicate authentication's `checkState`
uses, so a token is `409` here exactly when it is `401` there. `paths`,
`workspace`, `name` and `expires_at` are never written.

`internal/identity/key_routes.go` holds the routes of the caller's own API
keys, each carrying `identity.MarkNeedsUser`. `APIKey.MarshalJSON` writes
the Key object (`key_id`, `user_id`, `expires_at`, `revoked_at`,
`created_at`), so no answer built from a record can carry a secret.
`GET /user/keys` declares `keys:read` and lists with `keyTable.writePage`,
newest first (`created_at`, then `key_id`, both descending). `DELETE /user/keys/:key_id` declares `keys:manage` and revokes
with `revokeKeyOfUser`, which sets `revoked_at` only while it is `NULL`, in
one transaction with the owner lookup. `POST /user/keys/:key_id/refresh`
declares no scope and is not a create route: its handler refuses any caller
but an API key with `403 forbidden` before the lookup, refuses a revoked or
expired key (the same `expired` predicate) with `409 conflict`, and in one
transaction stores the hash of a new `randomString(apiKeyAlphabet,
secretLength)` secret and the new `expires_at` with `setKeySecret`;
authentication reads the stored hash on every request, so the old secret
fails from the next one. Every `:key_id` route resolves the id with
`keyTable.ofUser`, which answers a missing key and another user's alike, `404`
`API key not found`. `s.writeKeys(w, r, userID)` and `s.revokeKey(w, r,
userID)` take the user id, so the admin routes reuse them.

`internal/identity/admin_credential_routes.go` holds the admin routes of
any user's keys and tokens. They carry the admin mark and declare no
scope, so the admin check refuses every PAT. Each handler is
`forSelectedUser` around `writeKeys`, `writeTokens`, `revokeKey` or
`revokeToken`: it resolves the path's `:id` with `UserBySelector`
(`404 user not found`) and passes the user's id on, so an admin's list and
revocation run the same code, and give the same answers, as the user's own.

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

`DELETE /users/:id` (`deleteUser` in `handlers_users.go`) is an admin route.
In one `(*store.Store).Tx` it resolves the selector (`errUserNotFound()`
before anything else), calls `guardLastAdmin` (`409 conflict`, before any
remover), calls the user removers through `removeUserData` in
`removers.go`, then `deleteUser(ctx, tx, id, now)` in `users.go`. That
function deletes the user's `api_keys` and `pats`, bound ones included, and
`memberships`, runs `UPDATE orgs SET owner_id = NULL, updated_at = ? WHERE
owner_id = ?` and deletes the user, each an explicit statement rather than
left to the foreign keys' `ON DELETE` actions, so that an org's
`updated_at` moves with its `owner_id`. The orgs are kept, so a user delete
calls no org remover. It answers `204`.

`GET /user` and `PATCH /user`, in `internal/identity/handlers_user.go`,
carry the mark `identity.MarkNeedsUser` and declare the scopes `users:read`
and `users:write`. Both take the caller's user from the `identity.Principal`
of the request. `PATCH /user` reads the body with `fullNameOf(r)`, the rule
`PATCH /users/:id` shares: a `map[string]json.RawMessage` that needs
`full_name` as a JSON string, else `400 invalid_request`; it stores it in one `(*store.Store).Tx` through
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
by user id; `identity.ActiveOrgIDs(ctx, q, userID)` returns, by id, the
ids of the orgs with status `active` of which a user is a member, the
membership source of the workspace access rule (see Workspace access). Both
read through an `identity.RowsQuerier`, an `identity.Querier` that also has
`QueryContext` (the store's `*sql.DB` or a `*sql.Tx`).

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
`GET /orgs` reads one page with `listOrgs(ctx, q, includeBlocked, "", after,
limit)`: `ORDER BY name`, continuing after the position `{name}` of the
cursor. `GET /orgs/:id` carries no admin mark and declares the scope
`orgs:read`; it goes through `visibleOrg(r)`, the one rule for reading an
org: an admin (`isAdmin`) reads every org, any other caller an active org
of which its user is a member (`isMember`), and every other case, a missing
org included, is the one answer `errOrgNotFound()`, `404 not_found` with the
message `org not found`.

`GET /orgs/:id/members` (`listMembers`) also carries no admin mark and
declares `orgs:read`. It reads the page (`server.ReadPage` and `pageAfter`)
before it looks the org up, so a bad `limit` or `cursor` is `400
invalid_request` for every caller and tells nothing of the org; then it goes
through `visibleOrg(r)`, so a blocked org's member list is an admin's alone.
It reads one page with `listMembers(ctx, q, orgID,
after, limit)` in `orgs.go`, a join of `memberships` and `users` `ORDER BY
username`, continuing after the position `{username}`; an item is a
`memberItem` (`org_id`, `user_id`, `username`, `email`, and the membership's
`created_at`). `PUT` and `DELETE /orgs/:id/members/:user_id` (`addMember`,
`removeMember`) resolve, in one `(*store.Store).Tx`, the org selector and
then the user selector (`orgAndUser`: `errOrgNotFound()` or
`errUserNotFound()`), then call `addMembership(ctx, tx, orgID, userID, now)`,
an `INSERT … ON CONFLICT DO NOTHING` that leaves an existing membership as
it is, or `removeMembership(ctx, tx, orgID, userID)`, which returns
`identity.ErrNotFound` when no row was deleted (`404 not_found`, `membership
not found`) and leaves `owner_id` alone; both answer `204`.
`GET /user/orgs` (`listOwnOrgs` in `handlers_user.go`) carries the
needs-a-user mark and declares `orgs:read`; it reads the active orgs of
which the caller is a member through `listOrgs(ctx, q, includeBlocked,
memberID, after, limit)`, the reader of `GET /orgs`, whose `memberID` limits
the list to that user's orgs. `pageAfter[T](page)` decodes a page's
position for these lists and for `GET /users`.

`POST /orgs/:id/block` and `/unblock` (`setOrgStatus` in
`handlers_orgs.go`) are admin routes: in one `(*store.Store).Tx` they
resolve the selector and, unless the org has the requested status already,
call `updateOrgStatus(ctx, tx, id, status, now)` in `orgs.go`, which sets
`status` and `updated_at` and leaves the memberships alone; an org that has
the status is answered as read, so `updated_at` stays. A blocked org leaves
`GET /user/orgs` and the default `GET /orgs` because both lists read
`status = 'active'`, and `visibleOrg` answers it to admins only.
`GET /orgs/:id` and `GET /orgs/:id/members` both go through `visibleOrg`, so
a missing org, an org of which the caller is no member and a blocked org of
which it is a member get one and the same `errOrgNotFound()`.

`DELETE /orgs/:id` (`deleteOrg` in `handlers_orgs.go`) is an admin route.
In one `(*store.Store).Tx` it resolves the selector (`errOrgNotFound()`
before anything else), calls the org removers, then `deleteOrg(ctx, tx,
id)` in `orgs.go`, which deletes the org's memberships and the org; users
are never touched, so the owner and the members stay. A remover is an
`identity.Remover`, `func(ctx context.Context, tx *sql.Tx, id string)
error`, registered on the service with
`(*identity.Service).RegisterOrgRemover` or `RegisterUserRemover` in
`internal/identity/removers.go`, and kept in registration order under a
mutex ([ADR 03](adr/03-register-removers-of-owned-data-on-the-identity-service.md)).
`removeOrgData` and `removeUserData` call the org or the user removers,
a copy of the list taken under the mutex, in order with the delete's
transaction and the id and stop at the first error, which
`callRemovers` returns as text only, not wrapped, so that whatever a
remover returns the delete answers `500 internal` and the transaction rolls
back, with everything earlier removers wrote through it. A package that
keeps data for a user or an org registers its remover from its part in
`internal/hub` through the `identity` field of `hub.Composition`, which
`composeIdentity` sets.

### Workspace access

`internal/hub/access.go` adds the part `composeAccess`, after
`composeIdentity` in `parts`. It adds the schema step `access.SchemaStep`
(`workspace_records`), registers a zero `access.Service`, adds its
`Check` to the access checks, after identity's admin check, and binds it in
the start hook `access` to `access.Deps{Records:
access.StoreRecords(rt.Store.Main()), Memberships: ...}`, the membership
source being `identity.ActiveOrgIDs` over the same store. It adds the route
rule `refusePublicNeed`: a route that is `Public` and declares
`access.MarkNeed` stops the start with `compose the routes: <method>
<pattern>: a public route cannot declare the workspace need access.need:
the pipeline runs no access check on it`, because the pipeline skips the
access checks on a public route and its handler would serve every caller
without a decision. It registers no
remover on the identity service: a workspace record outlives the deletion of
its owner and of its org.

The step creates the table `workspaces` in the transactional store:

| Column | Holds |
|---|---|
| `slug` | the workspace's slug, the primary key, compared byte for byte (no case folding) |
| `owner_id` | the id of the user who owns it; no foreign key to `users` |
| `org_id` | the id of its org; no foreign key to `orgs` |
| `kind` | `repository` or `fork` (a `CHECK` constraint) |
| `parent_slug` | the slug of the repository a fork was created from; NULL for a repository, and no foreign key |
| `status` | `pending`, `cloning`, `ready`, `failed` or `archived` (a `CHECK` constraint) |

with an index on each of `owner_id`, `org_id` and `parent_slug`, for the
list reads. A dangling `owner_id` matches no caller and a dangling `org_id`
grants no membership, because the memberships of a deleted org are deleted
with it.

`internal/access/record.go` holds the record type `access.Record` (`Slug`,
`OwnerID`, `OrgID`, `Kind`, `ParentSlug`, `Status`; `ParentSlug` is `""`
for a repository and stored as NULL), the constants `KindRepository`,
`KindFork` and `StatusPending` to `StatusArchived`, and the store functions.
The insert takes the caller's `*sql.Tx`; the reads take a `Querier` (the
store's `*sql.DB` or an open `*sql.Tx`):

- `InsertRecord(ctx, tx, rec)` stores a record in the caller's transaction.
  It refuses, with an error wrapping `access.ErrInvalidRecord` and without
  writing, a record with an empty slug, owner or org, an unknown kind or
  status, a repository with a parent, a fork without one, and a fork whose
  parent has no record, is a fork itself or is in another org; the parent is
  read in the same transaction. A slug that has a record already is
  refused with `access.ErrDuplicateSlug` (the insert is `ON CONFLICT (slug)
  DO NOTHING`), and the stored record stays as it was.
- `RecordBySlug(ctx, q, slug)` returns the record and `true`, or `false`
  and a nil error for any string no record has as its slug.
- `RecordsOwnedOrIn(ctx, q, ownerID, orgIDs)` lists the records owned by a
  user or in one of a list of orgs, and `ForksOf(ctx, q, parents)` the forks
  of a list of parents, both ordered by slug; an empty list of parents
  lists nothing.

The rule reads records through the interface `access.RecordReader`
(`Record`, `OwnedOrIn`, `ForksOf`), so that a test can make a read fail;
`access.StoreRecords(q)` is the one over these functions.

#### Levels

`internal/access/level.go` resolves what a caller is on one workspace,
`access.Level`: `LevelAdmin` (`"admin"`), `LevelOwner` (`"owner"`),
`LevelMember` (`"member"`) or `LevelNone` (`""`). The caller is
`access.Caller{Admin, UserID}`, built from the pipeline's `server.Caller`
by `access.CallerFrom`; the kind of credential and the user's role are not
part of it, so a user has the same level by API key as by PAT, and a user
with the role admin has no level from the role.

`(*Service).Level(ctx, caller, slug)` returns the level, the record and
whether the slug has one. It reads the record first: a slug with no record
is `LevelNone` and found `false`, for every caller. For a user, though, a
missing slug still reads the user's active orgs, as a repository the user
cannot see would, and returns that read's error if it fails. A missing slug
therefore costs the same reads as an invisible repository, and fails the
same way. An invisible fork costs one record read more (its parent), and
that difference is not matched. Then:

- the admin token is `LevelAdmin` on every workspace, repository or fork;
- on a repository, a user whose id is the record's `owner_id` is
  `LevelOwner`, decided before any membership is read; otherwise a user who
  is a member of the record's `org_id`, as the membership source answers
  it, is `LevelMember`; everyone else is `LevelNone`;
- on a fork, the user's level on the parent record is resolved first, as on
  a repository. `LevelNone` on the parent, a parent with no record, or a
  parent that is not a repository gives `LevelNone` on the fork, even to the
  fork's owner: a fork never grants more than its parent. Otherwise the
  fork's owner and the parent's owner are `LevelOwner` on the fork, and
  every other user `LevelMember`.

The workspace's status is never read, so a caller has the same level in
every status. A failed read of a record or of the memberships returns
`LevelNone`, no record and the error, never a grant.

The membership source is the narrow interface `access.Memberships`, one
method `ActiveOrgIDs(ctx, userID)`; `access.MembershipsFunc` adapts a
function to it. The hub wires `identity.ActiveOrgIDs(ctx, q, userID)` from
`internal/identity/orgs.go`, which joins `memberships` with `orgs` and
returns, by id, the ids of the orgs with status `active` the user is a
member of. A blocked org is left out, so its members are `LevelNone` on its
workspaces while it is blocked (its owner is not), and a deleted org has no
memberships left, its rows being deleted with it.

#### Decision and check

`internal/access/decide.go` turns a level into an answer for what a route
needs, `access.Need`: `NeedRead` (`"read"`), `NeedWrite` (`"write"`) or
`NeedOwner` (`"owner"`). `(*Service).Decide(ctx, caller, slug, need)`
returns an `access.Decision{Outcome, Level, Record}`, a comparable value:

- level none, or a slug with no record: `Outcome` `access.NotFound`
  (`"not_found"`) with `LevelNone` and the zero record, for every need, so
  the decision for a workspace the caller cannot read equals the one for a
  slug with no workspace;
- `LevelMember` with `NeedWrite` or `NeedOwner`: `access.ReadOnly`
  (`"read_only"`) with the level and the record;
- everything else: `access.Granted` (`"granted"`) with the level and the
  record.

A need other than the three, and a failed read, return the zero decision
and an error, never a grant. The slug's shape is not checked: a string that
cannot be a slug has no record. A surface other than the REST API, such as
the git server, calls `Decide` directly.

`internal/access/middleware.go` holds the REST side. A workspace route
declares its need in its `Marks` under `access.MarkNeed`
(`"access.need"`), with the value `read`, `write` or `owner`, and names its
workspace with the path parameter `slug`. `(*Service).Check` is a
`server.Check` in the access position: on a route without the mark it
passes; otherwise it takes the caller from `server.CallerOf` through
`access.CallerFrom` and the slug from the matched route, and decides. A
grant attaches the decision with `server.Attach`, and the handler reads it
with `access.DecisionOf(r)`; `ReadOnly` is answered
`server.Forbidden("org members have read-only access to this workspace")`
and `NotFound` `server.NotFound("workspace not found")`, built from
constants only, so its bytes cannot differ between an invisible workspace
and a missing one. A caller that is neither the admin token nor a user, a
mark other than the three needs, a marked route without a `slug`
parameter, and an error from `Decide` are returned as plain errors, which
the pipeline logs and answers `500 internal` with `internal error`. No
route declares the mark yet.

The check never reads the request's method: a route that needs `read`
grants a member with `POST`, `PATCH`, `PUT` or `DELETE` exactly as with
`GET`, because a member's own contributions (forks, changes, verdicts,
claims) change state on routes that need `read`, and those routes check
their own conditions. The decision `access.DecisionOf(r)` returns holds the
caller's level and all six fields of the record as read while deciding, so
such a condition (the fork's owner, its parent, its kind) needs no second
lookup.

`(*Service).CanWrite(ctx, userID, slug)` answers whether a named user, who
need not be the caller, has write access to a workspace: `true` exactly
when that user's level on it, resolved by `Level` as for a caller with that
`UserID`, is `LevelOwner`; `false` for `LevelMember`, `LevelNone` and a slug
with no record; `false` and the error when a read fails. No production code
calls it yet; the routes whose conditions ask about another user, such as a
change's author, will.

#### Readable set

`internal/access/readable.go` answers which workspaces a caller can read,
for the reads that span workspaces (the workspace list, events, audit), so
that they show exactly the workspaces the per-workspace decision lets
through. `(*Service).Readable(ctx, caller)` returns an
`access.ReadableSet{All, Entries}`:

- the admin token gets `All` set and no entries; `Contains` answers yes and
  `Level` `LevelAdmin` for every slug;
- a user gets `Entries`, the user's level by slug. It reads the user's
  active orgs (`Memberships.ActiveOrgIDs`), then the records the user owns
  or that are in one of those orgs (`RecordReader.OwnedOrIn`, which brings
  the forks of those orgs' repositories with them, a fork being in its
  parent's org), then the forks of the repositories the user owns
  (`RecordReader.ForksOf`). Each record, in every status, gets the level
  the resolver of `Level` gives it, and those at `LevelNone` are left out:
  a fork whose owner has no level on its parent, a workspace of a blocked
  org that the user does not own. A slug appears once; an owned workspace
  in one of the user's orgs is `LevelOwner`;
- a caller that is neither, and a user with nothing, get the empty set and
  no error;
- a failed read returns the error and the empty set, never part of one.

`Level` and `Readable` share one resolver (`userLevels` in `level.go`); they
differ only in where the user's active orgs and a fork's parent come from.
`Level` reads them from the sources as the answer needs them; `Readable`
takes the orgs it has read and looks a parent up among the records it has
collected, which hold every repository the user has a level on, so a parent
that is not among them is one the user has no level on. A workspace is
therefore in a user's set exactly when `Level` resolves a level other than
`LevelNone` for it, with that level. No production code calls `Readable`
yet; the workspace list, the event stream and the audit query will.

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

## The start-up check

`composeSandbox` (`internal/hub/sandbox.go`) registers the `[sandbox]`
section (`sandbox.Register`), checks it with `(*sandbox.Service).Configure`
as a configuration check, right after the load, and adds the start hook
`sandbox_runtime_check`. With sandboxes off the hook builds no client and
contacts no runtime. With sandboxes on it registers the gateway token and the
OIDC client secret with the hub's scrubber, builds the client with the
runtime factory (`sandbox.RuntimeFactory`, given the settings, the scrubber
and the recorder; the default is `openshell.New`) and calls
`(*sandbox.Service).Start`, which runs `sandbox.RunStartCheck` and keeps the
client and the release once it has succeeded. A failure is the hook's error
and stops the start before the listener is bound.

`RunStartCheck(ctx, settings, client, CheckOptions{ExternalURL, Now, Budget,
Releases, Recorder})` makes, under one deadline `Budget` (30 seconds,
`sandbox.StartCheckBudget`) after it began on the clock `Now`, these calls
in order and stops at the first failure: `Release`, then the release against
the range (`ReleaseRange.Contains`; `SupportedReleases` unless `Releases`
replaces it); `EnsureWorkspace`; `InstallProviderProfile` of `parley-hub`
(`sandbox.HubProviderProfile`) with the variable `PARLEY_API_KEY`, one
endpoint of the scheme, host and port of `[server].external_url` with the
paths `/api/v1/` and `/git/`, and no program; `GetProviderProfile` once per
distinct `provider_profile` of the credentials; the exposed-secret names of
each profile against the variables of the provider profiles of the
credentials it attaches; `ValidatePolicy` with the base document of each
profile that names a `policy` file. A result of kind unavailable, and the
deadline passing, name `sandbox.gateway_url`; a missing provider profile
names `sandbox.credentials[<name>].provider_profile`; a rejected policy, or
a client without the validation (`ErrUnsupported`), names
`sandbox.profiles[<name>].policy`. The error's text is scrubbed of the
gateway token and the OIDC client secret. On success the check sets each
validated profile's `PolicyHosts` to the hosts the runtime reported, sorted,
sets `parley_sandbox_runtime_up` to `1` on the recorder and returns the
release. `(*sandbox.Service).Version()` then gives `GET /version` its
`sandbox_runtime` member, `supported` (`ReleaseRange.String()`,
`>=0.1.0 <0.2.0`) and `connected`; it makes no call, so it does not change
while the runtime is unreachable.

## Sandbox route declarations

The sandbox packages add their routes through the `*sandbox.Service` of
`(*Composition).Sandbox()`, which declares them on top of the pipeline's
positions rather than adding positions of its own:

- `Route(server.Route)` declares a route of the whole hub, such as the
  profile list: it sets `Route.Feature` to the service's feature check and
  wraps the handler with the runtime-error answer, and keeps the route's
  scope and marks.
- `WorkspaceRoute(kind, server.Route, opts...)` declares a route under
  `/workspaces/:slug/sandboxes`: on top of what `Route` adds it sets
  `Route.Scope` to `sandboxes:read` or `sandboxes:write` and the mark
  `access.MarkNeed` to `read` or `write`, for `sandbox.KindRead` and
  `sandbox.KindWrite`. The doc comment of `Kind` names the kind of each
  route of the sandbox packages. With `sandbox.NeedsUser()` the handler is
  wrapped once more, so that the admin token is answered `403 forbidden`
  with the message `the admin token cannot create a sandbox`.

Routes are declared while the hub composes, before the configuration is
loaded, so the feature check reads whether sandboxes are on when a request
comes and answers `404 sandboxes_disabled` while they are off. It runs in
the pipeline's feature position: after the body checks, before the binding
guard. The needs-a-user rule and the runtime-error answer wrap the handler,
which runs after every access check, so they come after workspace access and
before the handler reads its body. The runtime-error wrapper turns an error
of kind `ErrUnavailable` or `ErrUnsupported` that the handler returns, and
did not turn into another answer, into `sandbox.RuntimeError` with the
error's text passed through the hub's scrubber, which the start hook
`sandbox_runtime_check` gives the service (`SetScrubber`) and which knows
the gateway token and the OIDC client secret; a `*server.Error` and every
other error are left to the pipeline. A handler that answers a runtime error
itself calls `sandbox.RuntimeError` with a message it scrubbed.

## The sandbox runtime client

All of the hub's traffic to the sandbox runtime goes through
`runtime.Client` (`internal/sandbox/runtime`). A client acts in one OpenShell
workspace, the one it was built for, and in no other. Its operations, each
taking a context, are grouped as follows; their names in snake case
(`runtime.OpRelease` = `release`, `runtime.OpGetSandbox` = `get_sandbox`, …)
are the operation names errors and the fake's call list carry:

| Group | Operations |
|---|---|
| Gateway | `Release`, `EnsureWorkspace`, `Reachable` (the flag; no call) |
| Provider profiles | `GetProviderProfile`, `InstallProviderProfile` (create or replace) |
| Providers | `EnsureProvider` (create or replace), `DeleteProvider`, `AttachProvider`, `DetachProvider`, `SandboxProviders`, `ListProviders` (without values) |
| Policy | `ValidatePolicy`, `GetPolicy`, `AddPolicyRule`, `ListProposals`, `ApproveProposal`, `RejectProposal` |
| Sandboxes | `CreateSandbox`, `GetSandbox`, `ListSandboxes`, `DeleteSandbox`, `StopSandbox`, `StartSandbox`, `WaitReady`, `WatchSandboxes`, `RunMain` |
| Exec | `Exec`, `ExecStream`, `ExecInteractive` |
| Files | `Upload`, `MakeDir`, `Stat`, `Download`, `Walk` |
| Logs | `Logs` |

A sandbox is named by its name within the workspace. A policy value
(`runtime.Policy`) has three parts: the base document, the protected rules
and the added rules; a rule (`runtime.Rule`) is a name and a JSON object in
OpenShell's schema that the hub does not interpret. The streams
(`SandboxWatch`, `ExecStream`, `Terminal`, `LogStream`) end with `io.EOF`
when the caller's context ends, and with an error of kind `ErrUnavailable`
when they break.

A failure is a `*runtime.Error` of exactly one kind, which callers test with
`errors.Is`: `ErrNotFound`, `ErrInvalid` (the runtime's reason is kept in
the text), `ErrUnsupported` (the runtime has no such operation; it is never
imitated with other calls) or `ErrUnavailable` (everything else). Its text
reads `sandbox runtime: <op>: <kind>: <reason>`. It keeps text only, not the
error it was made from, so that nothing an implementation scrubbed can be
reached through `errors.Unwrap`. `runtime.KindOf` gives an error's kind, and
takes an error of none of them for `ErrUnavailable`.

Every implementation keeps its reachability flag in a `runtime.Reachability`:
a result of kind `ErrUnavailable` clears it, every other result sets it, and
each change is reported once, in order, to the `runtime.Recorder` as
`parley_sandbox_runtime_up` (`runtime.MetricUp`) `1` or `0`. The flag starts
set. The recorder has the method set of the hub's metrics registry (`Add`,
`Set`, `Observe`, each with a name, a label map and a value, each returning
an error); `runtime.NopRecorder` records nothing.

`runtimetest.New(t, opts...)` returns the in-memory runtime, `Fake`, and
`(*Fake).Client(workspace, recorder)` a client of it. It keeps per workspace
the provider profiles, the providers, the sandboxes with their status, the
policy values, files, log entries and pending proposals. A sandbox's files
are held in memory, or with `WithHostDirs()` in a directory of the host
(`HostDir`) in which `Exec` and `RunMain` run real commands; otherwise
commands are run by a script (`SetExec`), and without one a command exits 0
with no output and a main process runs until `EndMain`. Its terminal echoes
its input. A new fake reports a healthy gateway of release `0.1.8`;
approving a proposal adds its rule to the sandbox's added rules. Its
controls are `SetUnreachable`, `FailOperation` (an operation by name, with a
kind and a text, until cleared with a nil kind), `SetRelease`,
`AddProviderProfile`, `RejectPolicies`, `SetPolicyValidation`, `SetExec`,
`SetState`, `EndMain`, `DropWorkDir`, `EmitOutput`, `EmitPolicyLog`,
`AddProposal` and `BreakStream`; `Calls` returns the calls its clients
received in order, `Streams` the open watches, followed logs, exec streams
and terminals, and `Spec` and `Provider` what a sandbox was created with and
a provider with its value. Its processes and streams end with the test.

`runtimetest.RunContract(t, newBackend)` runs the contract tests against the
`runtimetest.Backend` that `newBackend` returns for every test: a function
that returns a client of the backend for a workspace and a recorder, a
switch that makes the backend unreachable, and, where the backend can, a
way to reject policy values and to add a proposal. The assertions about an
operation that returns `ErrUnsupported` are skipped, so the contract holds
for a runtime that lacks the operation. `(*Fake).Backend()` gives the fake
in that form.

### The OpenShell implementation

`openshell.New(settings, openshell.Deps{Scrubber, Recorder, Dial})` builds
the client the hub uses. It registers the gateway token and the OIDC client
secret with the scrubber, builds the SDK configuration with
`openshell.BuildConfig(settings)` and the SDK client with `Deps.Dial`
(`v1.NewClient` when nil), which connects on the first call, and acts in
`openshell_workspace`. `openshell.NewWithClient(sdkClient, workspace, deps)`
builds the client over an SDK client the caller has, such as the SDK's
in-memory `fake.NewClient()`; closing that SDK client makes every later
operation fail as `ErrUnavailable`.

`BuildConfig` takes the address from `gateway_url`: its host and port, 443
by default, with the `http://` scheme kept for a loopback gateway, which is
how the SDK is told to connect without TLS. Mutual TLS gives the SDK the
certificate and key files and no token source; `token_file` gives a static
bearer token; the OIDC keys give the SDK's client-credentials token source,
which obtains a token on the first call and renews it before it expires;
`tls_ca` is the only CA the gateway's certificate is checked against. Over
`http` to a loopback gateway the token is sent without TLS, which the SDK
otherwise refuses.

Every operation passes the workspace to its SDK call. An operation that
does not stream runs with a context that ends after 30 seconds or with the
caller's, whichever is sooner, and the client stops waiting for the SDK when
it ends; `ExecStream`, `ExecInteractive`, `Upload` and `Download` run with
the caller's context. The SDK's errors map to the kinds as follows: its
not-found is `ErrNotFound`; an invalid argument, a resource that exists
already and a conflict are `ErrInvalid` with the gateway's message and the
fields it names; a call that is not implemented, and a file transfer in an
SDK built without its transport, are `ErrUnsupported`; everything else, a
refused connection, an unauthenticated or refused credential, a server error
and a call that did not end in time included, is `ErrUnavailable`. Every
error's text is scrubbed.

The SDK in `go.mod` (v0.1.8) has no call for `ValidatePolicy`, `GetPolicy`,
`WatchSandboxes` (it watches one named sandbox), `RunMain`, `MakeDir`,
`Stat`, `Walk` and `Logs` (its logs are the gateway's and the supervisor's
log lines), nor for `Exec` and `ExecStream` with input on stdin; they answer
`ErrUnsupported` without calling it. Where it has a call but cannot carry a
part of the request, the part is not sent: `CreateSandbox` sends no working
directory and no base document (the SDK takes typed policy rules: the
protected and added rules go as named network rules, and a rule its network
rule cannot hold is `ErrUnsupported`), and `StartSandbox` sends neither the
policy nor the environment, so the sandbox keeps those the gateway holds.
`InstallProviderProfile` installs a profile with one credential named after
the profile; the SDK's profile endpoint is a host, a port and a protocol,
which takes the endpoint's scheme, and has no path, so an endpoint's paths
are not sent and the credential is authorized on every path of its host. `EnsureProvider` stores the value under the one credential
of the provider's profile. `AttachProvider` first reads the provider in the
workspace, so a provider of another workspace is not found.
`ListProposals` reads the pending draft chunks of every sandbox of the
workspace; `AddPolicyRule` uses the SDK's merge operation that adds a named
network rule. `Upload` and `Download` go through a temporary file, because
the SDK transfers from and to local paths. A sandbox's state follows its
phase: `Ready` is `running`; `Stopping`, `Stopped`, `Suspending`,
`Suspended`, `Completed`, `Error` and `Deleting` are `stopped`; a sandbox
waiting to restart is `restarting`; the rest is `pending`. The SDK reports
nothing of the working directory, which counts as present while the sandbox
exists.

The contract tests run against this implementation over the SDK's in-memory
client, which keeps no provider profiles and answers its profile, exec, file
and policy draft calls as not implemented, so the assertions about those
operations are skipped there.

`openshelltest.New(opts...)` (`internal/sandbox/runtime/openshell/openshelltest`,
test support) returns a `Gateway`: a `v1.ClientInterface` that stands in for
the gateway in the tests of this implementation, of the hub and end to end.
It runs over `fake.NewClient(opts...)` (`Fake()` returns it), keeps provider
profiles per workspace (`AddProfile` preloads one, `Profile` reads back what
was installed) and can be made unreachable and reachable again
(`SetUnreachable`), every call then failing as unavailable; `Close` closes
the in-memory client for good. `runtimetest.Fake` stays the double of the
runtime that the other sandbox packages test against.

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
global flags `--endpoint-url`, `--api-key` and `--json`, the `help`,
`version` and `login` subcommands and the groups `user`, `orgs` and `admin`,
plus the commands the unexported `options.extra`
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

The commands of the user and organization routes are declarations without
client code of their own: `parley user` (`internal/cli/user.go`), `parley
orgs` (`orgs.go`), `parley admin users` (`admin_users.go`) and `parley
admin orgs` (`admin_orgs.go`), the last two in the group `parley admin`
that `newAdminCommand` in `root.go` builds. Those that only call their route
use the default run step; a command whose request carries a query or a body
(`--include-blocked`, `--full-name`, the flags of `parley admin users
create`, and those of `parley admin orgs create` and `update`) has a run
step that builds them from its flags and sends them with `sendWith`
(`user.go`), the default run step with a query and a body. The two `list`
commands with `--include-blocked` share the run step `sendIncludeBlocked`
(`admin_users.go`), and `parley admin orgs create` and `update` send a
member only when its flag is given (`addChanged` in `admin_orgs.go`).

`parley admin keys` (`admin_keys.go`, also in `parley admin`) is declarations
alone too. `parley keys` (`keys.go`) is `list`, a declaration, and `refresh`
and `revoke`, whose run steps take the route's `:key_id` from the command's
own credential (`apiKeyID`, which repeats the hub's API key format, since
the client does not import the hub's code) and refuse any other credential
before sending. After the hub's answer is printed, `refresh` saves the new
key and `revoke` clears `api_key` and `user_id` through `saveConfig`, both
only in a config file that already exists, so neither creates one.

`parley admin tokens` (`admin_tokens.go`, also in `parley admin`) is
declarations alone. `parley tokens` (`tokens.go`) is `list`, `show` and
`revoke`, declarations, and `create`, `replace`, `add` and `remove`, whose
run steps build the body from the flags: `splitList` splits `--permissions`
and each `--paths` value at every comma, without the quoting of pflag's
comma-separated flags, and `create` sends `paths` only when `--paths` is
given and declares `ShowsOnce`. `changePermissions` builds the three
permission commands; after the answer is printed, `replace` and `remove`
note on stderr a Token whose `revoked_at` is not null.

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
`client_error` naming the path, with the temporary file and the directories
the save created removed, and the old file left as it was; without a path, because there is no home directory,
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
