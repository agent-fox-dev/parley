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
| `internal/access` | the workspace access rule, the one place that decides what a caller may do on a workspace; the minimal workspace record (slug, owner, org, kind, parent, status, the creating token of a fork) in the transactional store, the caller's level on a workspace, the decision for a declared need and the REST access check, the readable set for reads across workspaces, what the binding of a workspace-bound PAT covers, and the binding guard that confines such a PAT on every REST route and, for the git server, on git, and the minting, listing and revoking of bound tokens (`MintBound`, `POST` and `GET /workspaces/:slug/tokens`, `RevokeBoundTo`, and the maintenance task `token_binding_sweep`, `SweepBindings`) |
| `internal/buildinfo` | the version, commit and build time the binary was built with, shared by both programs |
| `internal/cli` | the `parley` client: its root command and its subcommands, built with `github.com/spf13/cobra`, each declared through one helper ([ADR 02](adr/02-declare-every-cli-command-through-one-helper.md)), the client configuration file, the resolution of the hub URL and the credential, the one HTTP client the commands send their requests with, the printing of a command's result on stdout and of its warnings on stderr, the shared wait for a long operation (`--wait`, `--timeout`, `--poll-interval`), and the error envelopes and exit statuses every command ends with |
| `internal/config` | the configuration loader: `config.toml` from `$XDG_CONFIG_HOME` or the working directory, `${NAME}` references, and a registry of keys and whole sections that decides what the file may hold |
| `internal/event` | the hub events: every significant state change as one event, written in the caller's store transaction with a hub-wide `seq` that is never reused; the event record and its JSON form, the emit calls `Emit` and `EmitNow`, the log's bounds, the read by position and the expiry of old positions, the following of commits for waiters and `OnCommit` callbacks, the counters, the keys of `[events]`, the route `GET /api/v1/events/log` with its long-poll and the Server-Sent Events stream `GET /api/v1/events` with its limits ([ADR 05](adr/05-deliver-events-by-reading-the-log-by-position.md)) |
| `internal/gitrepo` | the hub's one way to run git, the `git` command as a subprocess with no git library; it imports nothing of the project. `Git` (its zero value runs git from `PATH`; `Env` adds variables such as `GIT_SSL_CAINFO` to every git process, `Trace` sees the arguments of every invocation) runs each git process with a fixed minimal environment (`PATH`, `HOME` set to an empty private directory made, mode 0700, for that one process and removed when it returns, `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`, `GIT_TERMINAL_PROMPT=0`, `GIT_SSH_COMMAND=ssh -o BatchMode=yes`, so that the `ssh` of a `git@` origin fails rather than prompts, unless `Env` sets its own, `LC_ALL=C`), outside any repository, keeps git's standard error in the error it returns and, when the context ends, kills git with every process it started (its own process group) and waits for them. `ListRemote(ctx, url, cred)` returns origin's branches, tags and default branch as a `Remote`; `Clone(ctx, url, cred, dir)` makes a bare copy with every branch and tag and full history; both, like `FetchOrigin` below, run with `-c credential.helper=`. A `Credential` (user name and password; the zero value is none) reaches git only through the environment of that one process: `GIT_ASKPASS` names a script, written into that private directory, that prints `PARLEY_GIT_USERNAME` or `PARLEY_GIT_PASSWORD`, so the credential is in no URL, no argument and no configuration file of the copy. `FetchOrigin(ctx, dir, url, cred, deadline)` fetches every branch and tag of origin into the copy at `dir` in one `git fetch --atomic --force --no-tags` into a namespace of that one fetch, `refs/parley/fetch/<random>/`, which is read and removed right after the fetch, also after a failure, so that the tags' objects are in the copy and no extra ref outlives the fetch; only once the fetch has succeeded and the namespace is gone are the origin heads `refs/remotes/origin/<name>` set to origin's branches exactly (created, moved, removed for a branch origin no longer has) in one `update-ref --stdin` transaction, or, when git refuses it because a new name is a directory of a removed one (`dev` replaced by `dev/next`), with the removals first; a fetch that fails, or whose namespace cannot be removed, changes no origin head; no branch or tag of the copy is written; it returns origin's branch heads and tags as `Fetched`, and after `deadline` kills git with every process it started and returns an error matching `ErrDeadline`. `Push(ctx, PushRequest)` pushes one commit of the copy at `PushRequest.Dir` to one branch of origin in one `git push --porcelain --no-follow-tags` of the single refspec `<commit>:refs/heads/<branch>`, with `-c credential.helper=` and the credential as above, so that no tag and no other branch is sent; `PushRequest.Force` adds the explicit lease `--force-with-lease=refs/heads/<branch>:` (origin must not have the branch), never a plain force; the result is read from the status git reports for the ref, also when git exits with an error: `PushPushed` for a new branch or a fast-forward, `PushUpToDate` when origin's branch is at the commit already, an error matching `ErrNonFastForward` for git's non-fast-forward and fetch-first refusals, and for every other failure a `*PushError` (`Branch`, `Class`, `Reason`, `Err`) whose class `errors.Is` matches: `ErrOriginRejected` for a `[remote rejected]` status of the ref, its `Reason` origin's reason followed by the `remote:` lines its hooks wrote; otherwise, when git reports no status for the ref, the class read from git's standard error, for the HTTPS transport and for the SSH transport's and forges' messages: `ErrUnauthenticated` (`Authentication failed`, `could not read Username`, HTTP 401, `Permission denied (publickey`), `ErrForbidden` (HTTP 403, `Permission to … denied`, `not allowed to push`), `ErrRepositoryNotFound` (HTTP 404, `not found`, `could not be found`), `ErrUnreachable` (name resolution, connection, TLS and certificate failures, `Host key verification failed`) and `ErrPushFailed` for anything else, its `Reason` git's standard error without its boilerplate; a push whose context ended returns the error wrapping the context's error, git killed with every process it started. `Open(ctx, dir)` opens a copy as a `Repo` (an error for a file, an empty directory or nothing), whose methods never contact origin: `DefaultBranch` and `SetDefaultBranch`, `ResolveBranch`, `Branches`, `Tags`, `OriginURL`, `Config` and `SetConfig` (the copy's own configuration), the origin heads, kept as the refs `refs/remotes/origin/<name>`: `OriginHeads`, `OriginHead`, `SetOriginHead` and `DeleteOriginHead`, `CompareAndSwapRef(ctx, ref, new, expectedOld)`, which moves a ref only from `expectedOld` (`""`: creates it only while it does not exist) and otherwise, or when another writer holds the ref's lock, changes nothing and returns an error matching `ErrRefMoved`, and `IsAncestor`; `Remove(dir)` removes a copy |
| `internal/gitrepo/gitrepotest` | test support, imported by test code only, and the repository's one test origin: `NewOrigin(t, opts...)` makes an empty bare repository whose default branch is `main` and serves it over TLS with `net/http/httptest` and git's file-based HTTP transport (`git update-server-info` after every change), or with git's smart transport through `git http-backend` under `WithSmartHTTP()` (git cannot read a repository without refs over the file-based one, so an empty origin needs it); `SmartOrigin(t, opts...)` is `NewOrigin` with the smart transport and `http.receivepack` enabled, so that it takes pushes; `WithBasicAuth(user, password)` makes it answer `401` without them, and `Accept(user, password)` changes the credential it takes while it serves; `Commit(branch, msg)`, `Branch(name, ref)`, `Tag(name, ref)` and `SetDefaultBranch(branch)` change it, `History()` builds the tests' usual history on it (`main` with three commits, `dev` branching off its second with one more, the tag `v1` on its first), `Head(branch)` reads it; `URL()` is its https URL and `Env()` (`GIT_SSL_CAINFO=<CAFile()>`) makes git trust it; `Requests()` counts the requests it received, `Block()` holds every later request until `Release()`, `Refuse(status)` answers every request with a status, `Reject(message)` installs a `pre-receive` hook that refuses every update writing `message` (`""` removes it), `ServeUntrusted(true)` makes it serve, on every later connection, a self-signed certificate that `CAFile()` does not vouch for, and `Close()` shuts it down; `Git(t, dir, args...)` runs git on a repository directory, such as a copy the code under test made, away from the user's git configuration, and returns its trimmed output, and `Refs(t, dir, prefix)` maps the refs below `prefix` of such a directory, by their names without it (`""`: every ref, by its full name), to their object ids |
| `internal/hub` | the composition of the `parleyd` process: argument parsing, the scrubbing logger, the fixed start order, the bootstrap of the admin token, the HTTP server's options, the idempotency records in the transactional store, signals and the stop, and the `backup` command; the one place later packages plug their parts in ([ADR 01](adr/01-compose-the-hub-in-one-package.md)) |
| `internal/maintenance` | the maintenance loop: a registry of named tasks, and `Loop`, which runs one pass at once and one at each tick of an hourly timer, the tasks of a pass one after the other in a fixed order, and logs a failing task without stopping the others |
| `internal/hubtest` | test support, imported by test code only: `Start` bootstraps a hub with an admin token and serves it in-process on a free loopback port, `StartWith` does the same with more in `config.toml` (`Config.Server` inside `[server]`, `Config.TOML` after it), files in the config dir (`Config.Files`), variables added to the hub's environment (`Config.Env`) and a function run on the composition (`Config.Compose`), `(*Hub).Restart` serves the hub again on the same data directory and port and `RestartEnv` does so with other added variables, `(*Hub).IssuePAT` stores a PAT, `CreateOrg` and `AddMember` make an org and a membership through the admin routes and `AddWorkspace` stores a workspace record, `Serve` runs a hub with given options until the test ends, `(*Served).Call` sends a request with a bearer credential and a body and returns the answer's status and body, and `LogBuffer` holds a running hub's log for the test to read |
| `internal/identity` | who may use the hub: the admin settings in the transactional store (the bootstrap e-mail and the admin token's hash), the generation and hashing of admin tokens, and `VerifyAdminToken`, which compares a presented token's hash with the stored one in constant time; the identity records (users, orgs, memberships, API keys, PATs), the generation of API keys and PATs, and `Service`, the pipeline's authenticator |
| `internal/sandbox` | the hub's side of the sandbox feature: `Register` makes it the owner of the whole `[sandbox]` section of the configuration, and `FromConfig` checks that section after the load, with the config dir and `[server].external_url` in view, and returns its `Settings` with every default filled in, or one error listing every failure; the `Settings` give the profiles and the configured credentials by name (`Profile`, `DefaultProfile`, `Credential`) and in name order (`Profiles`, `Credentials`), as copies, so that they do not change after the load; `RunStartCheck` checks the runtime once at start (see [The start-up check](#the-start-up-check)), `ReleaseRange` and `SupportedReleases` hold and compare the supported OpenShell releases, `Service` is the feature of one hub (settings, runtime client, connected release) and declares the sandbox routes (see [Sandbox route declarations](#sandbox-route-declarations)), `ProfilesRoute` is the route `GET /sandbox-profiles`, `SandboxesRoute` the route `GET /sandboxes`, which lists for the admin token the sandboxes of the `Source` a later package registers with `(*Service).RegisterSource`, `RuntimeError` is the `502 sandbox_runtime_error` answer, `Covers` and `RequireCovers` are the no-widening decision over a sandbox's `HubAccess`, and `Recorder` is the metrics recorder of the sandbox packages |
| `internal/sandbox/runtime` | the hub's one boundary to the sandbox runtime: the `Client` interface with every operation the sandbox packages call, the plain data it exchanges, the four error kinds (`ErrNotFound`, `ErrInvalid`, `ErrUnsupported`, `ErrUnavailable`), the reachability flag (`Reachability`) and the metrics recorder (`Recorder`, `NopRecorder`) it reports to; it imports nothing of the project |
| `internal/sandbox/runtime/openshell` | the runtime client over the OpenShell Go SDK (`github.com/ai-openshell/sdk-go`, package `openshell/v1`), the only production package that imports the SDK (`openshelltest` and tests import it too): `BuildConfig` turns the checked `[sandbox]` section into the SDK's configuration, `New` builds the client of a section and `NewWithClient` the client over a given SDK client, such as the SDK's in-memory one |
| `internal/sandbox/runtime/openshell/openshelltest` | test support, imported by test code only: `Gateway`, which stands in for the OpenShell gateway: the SDK's in-memory client with provider profiles kept per workspace and a switch that makes it unreachable and reachable again |
| `internal/sandbox/runtime/runtimetest` | test support, imported by test code only: `Fake`, the in-memory runtime and the only test double of the runtime, with its test controls, and `RunContract`, the contract tests every implementation of the runtime client passes |
| `internal/scrub` | the one scrubbing function for text that leaves the hub: `Scrubber.Scrub` replaces registered secret values and the hub's own credential formats by `[redacted]`, removes user information from URLs and cuts the text to 1024 bytes; a leaf package that imports nothing of the project |
| `internal/secret` | the hub's secrets, whose values leave the hub through no response, log line or error: `LoadKeys` reads the operator's current key (`PARLEY_SECRETS_KEY`, or the file `[secrets].key_file` names) and previous key (`PARLEY_SECRETS_KEY_PREVIOUS`), each standard padded base64 of 32 bytes, into `Keys`, whose keys print, log and encode as JSON as `[key]` (an absent key encodes as `null`); `Seal` and `Open` seal a value with AES-256-GCM under the current key, a fresh 12-byte nonce and a binding of its owner as additional data, and open it with the current or the previous key by the `KeyID` (the first 8 bytes of the key's SHA-256 in hex) it carries; `SealValue`, `OpenValue` and `NeedsReseal` do the same for values that are not named secrets, with the key id in front of the sealed bytes; `Component` (the hub uses its zero value, bound once at start by `Bind(Deps)` to the store, the keys, the clock, the scrubber and the event log; `New(Deps)` builds a bound one; table `secrets`, created by `SchemaStep`, unique on scope, scope id and the key with A to Z lowered) is the one place secrets are read, stored and removed, in the scopes `ScopeUser`, `ScopeOrg` and `ScopeWorkspace`: `Get` reads one secret of exactly one scope on every call, matching the key ignoring ASCII case, registers the value with the scrubber and fails a value no key opens with `ErrUnreadable`, never as absent; `Put`, `Delete`, `RemoveScope` and the package function `RemoveWorkspace` change secrets inside the caller's transaction and write no event; `CreateEntries` (all or nothing, the limit of 100 checked before a conflict), `ListPage`, `UpdateEntry`, `DeleteEntry` and `Count` serve the secret routes; `Routes` are the secret routes and `ScopeCheck` the access check that resolves a route's scope from its `MarkScope` mark; `Available` reports whether a current key is configured; `StartReport` counts the stored values whose key id is neither configured key's and `(StartReport).Log` writes the start lines; `Reencrypt` is the maintenance task that re-seals the values under the previous key with the current one and runs the `Converter`s other parts add with `RegisterConverter`; `ErrUnavailable` (no current key), `ErrUnreadable` (no configured key opens the value), `ErrInvalid`, `ErrLimitReached`, `ErrConflict` and `ErrNotFound` are the errors to match, and `UnavailableError` is the `503 secrets_unavailable` the REST API answers for `ErrUnavailable` |
| `internal/store` | the hub's two embedded SQLite stores (`modernc.org/sqlite`, no cgo): `Open` creates the data directory and the workspace root, opens the transactional store and the analytical store (`analytics.db` beside it) and applies the registered schema steps; `Tx` runs a transaction of the transactional store; `Snapshot` writes a consistent copy of one store file without changing it |
| `internal/server` | the hub's HTTP server: its built-in routes, the REST API's route registration and check pipeline, the error envelope, request ids, response headers and the access log, list paging, idempotency keys, and the readiness probes |
| `internal/server/timefmt` | the one timestamp format (`Format`, and the response member type `Time`) and the one duration grammar (`ParseDuration`); a leaf package that imports nothing of the project, so that the configuration loader can use it as well as the REST API |
| `internal/workspace` | the place for workspace records, their validation rules, the state guard, and the create and clone work; so far the whole workspace record with its store functions, the state guard `RequireReady`, the resolution of a workspace's origin credential from its secrets `ResolveOriginCredential`, the workspace service with `POST /api/v1/workspaces`, `GET /api/v1/workspaces`, `GET /api/v1/workspaces/:slug`, `PATCH /api/v1/workspaces/:slug`, `POST /api/v1/workspaces/:slug/sync` and `POST /api/v1/workspaces/:slug/branches/:name/push` and the clone work `Clone` with the copy's place `RepoPath` and the post-clone steps, the sync of a workspace with its origin `(*Service).Sync` (see Sync), the clone workers `Workers` and `(*Service).Enqueue`, which hands a workspace put back to `pending` to them (see Clone work), the one encoder of the workspace object `RenderWorkspace` and the settings registry with the settings table of the 21 setting keys and `RegisterSettingKeys` (see Workspace records), the one branch-glob matcher `MatchBranchGlob`, `MatchAnyBranchGlob` and `ValidateBranchGlob`, and the rules every workspace route judges its values by, each deciding on the text alone and returning an error that says which rule broke: `ValidateSlug` (3 to 64 characters of a to z, digits and hyphens, starting with a letter), `ValidateGitURL` (`https://<host>/<path>` without user information, or `git@<host>:<path>`, with no lookup or connection), `ValidateBranchName` (git's branch-name rule), `ValidateForkName` (1 to 32 characters of a to z, digits and hyphens) and `ParseDuration` (a whole number above zero followed by `m`, `h` or `d`, unlike `timefmt.ParseDuration`, which also takes seconds) |
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

The built-in `GET /healthz`, `GET /readyz` and `GET /version`, and the
surfaces outside the REST API that later packages mount with
`(*Server).Handle(pattern, handler)`, are registered on one standard library
`http.ServeMux` with its method and path patterns; `Handle` bypasses the REST
API's checks. The REST API routes are held in the server's own route table
and matched by the dispatcher (below), because the mux refuses two patterns
of which neither is more specific, such as `/changes/by-commit/:sha` and
`/changes/:number/stack`. Success bodies are
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
with the `404 not_found` envelope.

A path under `/api/v1/` is matched against the REST routes first, segment by
segment on the unescaped segments: a literal segment must be equal, a
parameter takes any segment that is not empty. Of the routes that match the
path and are registered for the method (or for `GET` when the method is
`HEAD`), the most specific serves it: reading left to right, the first
segment where one pattern has a literal and the other a parameter decides,
the literal winning, so `/changes/by-commit/stack` goes to
`/changes/by-commit/:sha` and `/changes/7/stack` to `/changes/:number/stack`.
Of a `GET` and a `HEAD` route with the same pattern, a `HEAD` request goes to
the `HEAD` route. When routes match the path but none is registered for the
method, the answer is `405 method_not_allowed` with their methods, sorted, in
`Allow`. When none matches, the path goes on to the mux.

For every other path the dispatcher asks the mux (`ServeMux.Handler`) which
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
`Register` adds the route to the server's route table and panics on a
pattern that does not start with a slash, ends in one, has an empty, `.` or
`..` segment, a brace, or a parameter without a valid name or named twice, on
a method that is not upper-case letters, on a missing handler and on a route
already registered, which includes one that differs from a registered route
of the same method only in its parameter names.

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
| `AdjustWorkspace(func(*workspace.Deps))` | a change to the workspace service's dependencies, applied after the start hook `workspaces` builds them and before it binds them, in the order added; tests replace a collaborator with it |

`(*Composition).Sandbox()` returns the `*sandbox.Service` that
`composeSandbox` (`internal/hub/sandbox.go`) creates: a part listed after it,
or `Options.Compose`, keeps it and reads its settings and runtime client
once the hub serves. Two unexported registrations serve the parts of the hub
itself: `addConfigCheck(func(*config.Config) error)` adds a check run on the
loaded configuration before any store is opened, for rules that span more
than one key or section, `addVersionMember(func() map[string]any)` adds
members to the document of `GET /version` (the server's
`Options.VersionMembers`), after the build's, and
`addStartReport(func(*slog.Logger))` adds lines a part writes about its
start, which the hub writes once the listener is bound, right before the
`listening` line, so that a failed start still writes only its one error
line. The composition keeps the hub's environment (`Options.Env`, an empty
one when nil) in its unexported field `env`, which a configuration check
reads ([ADR 06](adr/06-load-the-secret-keys-before-the-stores-open.md)).

`(*Composition).Secrets()` returns the `*secret.Component` that
`composeSecrets` (`internal/hub/secrets.go`) creates: a part keeps it when
it is composed and reads, stores and removes secrets through it from its
start hook on, if it is listed after `composeSecrets`, or from
`Options.Compose` (see [Secrets](#secrets)).

`(*Composition).Events()` returns the `*event.Component` that `composeEvents`
(`internal/hub/events.go`) creates: a part keeps it when it is composed and
emits through it from its start hook on, if it is listed after
`composeEvents`, or from `Options.Compose` (see [Hub events](#hub-events)).

`(*Composition).Workspaces()` returns the `*workspace.Service` that
`composeWorkspaces` (`internal/hub/workspace.go`) creates, with the keys of
the settings table registered on `Workspaces().Settings()` by
`workspace.RegisterSettingKeys` (a failure panics the composition, as an
invalid route does): a part keeps it when it is composed (see
[Workspace records](#workspace-records)).

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
   (the `[sandbox]` section's, see [The start-up check](#the-start-up-check),
   and the loading of the secret keys, see [Secrets](#secrets)),
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
10. `Serve`, whose `OnListen` writes the parts' start reports
    (`addStartReport`), in the order they were added, and then the
    `listening` line with `addr`, `config` (`cfg.Path`) and `data_dir`
    (`cfg.DataDir()`).

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
`composeBootstrap` in `parts`. It adds the schema steps
`identity.SchemaStep` (`identity_records`) and, after it,
`identity.PersonalOrgStep` (`identity_personal_orgs`), which adds the flag
`personal` to `orgs` and, in a store that has records already, marks the org
each user owns under their username, installs the `Authenticate`
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
`identity.Org`, active, named after the username, owned by the user and
marked `personal`) and
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
return an org or `identity.ErrNotFound`, comparing the slug exactly;
`identity.PersonalOrg(ctx, q, userID)` returns the user's personal org, the
one owned by the user and marked `personal` by `CreateUserAndPersonalOrg`,
whatever the username and the org's name are now, or `identity.ErrNotFound`;
and
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
`composeIdentity` in `parts`. It adds the schema steps `access.SchemaStep`
(`workspace_records`) and, after it, `access.CreatingTokenStep`
(`workspace_creating_token`), registers a zero `access.Service`, installs
its `Guard` as the pipeline's binding guard (`SetBindingGuard`), adds its
`SubsetCheck` and then its `Check` to the access checks, after identity's
admin check, registers its `TokenRoutes()`, registers its
`SweepBindings` as the maintenance task `token_binding_sweep`, and binds it in
the start hook `access` to `access.Deps{Records:
access.StoreRecords(rt.Store.Main()), Memberships: ..., Refusals:
access.NewRefusalLimiter(events, rt.Now, rt.Logger), Events: events, Store:
rt.Store, Now: rt.Now, Logger: rt.Logger}`, the
membership source being `identity.ActiveOrgIDs` over the same store and
`events` the adapter `accessEvents`, which implements
`access.EventEmitter` over the event component: it converts an
`access.Event` to an `event.Input` (an empty `Workspace` or `ActorID`
becomes nil, `Metadata` is passed as is) and calls `c.Events().Emit` or
`EmitNow`, reading the component from the composition at each emit, since
`composeEvents` comes after `composeAccess`. The limiter is built in the
hook, so every start of the hub begins with no refusal counted. The same hook
installs the covered set of workspace-bound tokens in the event component,
`c.Events().SetCoverage(...)`: an `event.CoverageFunc` that answers whether
`(*access.Service).CoveredSet` of the caller contains the slug, with the
`event.Caller`'s `CredentialID` (the PAT's token id) as `TokenID` and its
`Workspace` as the binding. The hook runs before the `events` hook, whose
bind keeps what `SetCoverage` installed. It adds the route
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
| `creating_token_id` | the `token_id` of the PAT whose request created a fork; NULL for a repository and for a fork no token created, and no foreign key. Added by the second step, `ALTER TABLE workspaces ADD COLUMN creating_token_id TEXT`, so that a store whose table exists gains it |

with an index on each of `owner_id`, `org_id` and `parent_slug`, for the
list reads. A dangling `owner_id` matches no caller and a dangling `org_id`
grants no membership, because the memberships of a deleted org are deleted
with it.

`internal/access/record.go` holds the record type `access.Record` (`Slug`,
`OwnerID`, `OrgID`, `Kind`, `ParentSlug`, `Status`, `CreatingTokenID`;
`ParentSlug` and `CreatingTokenID` are `""` for a repository and stored as
NULL, and `CreatingTokenID` has no JSON name, so no answer shows it), the constants `KindRepository`,
`KindFork` and `StatusPending` to `StatusArchived`, and the store functions.
The insert takes the caller's `*sql.Tx`; the reads take a `Querier` (the
store's `*sql.DB` or an open `*sql.Tx`):

- `InsertRecord(ctx, tx, rec)` stores a record in the caller's transaction.
  It refuses, with an error wrapping `access.ErrInvalidRecord` and without
  writing, a record with an empty slug, owner or org, an unknown kind or
  status, a repository with a parent or a creating token, a fork without a
  parent, and a fork whose
  parent has no record, is a fork itself or is in another org; the parent is
  read in the same transaction. A slug that has a record already is
  refused with `access.ErrDuplicateSlug` (the insert is `ON CONFLICT (slug)
  DO NOTHING`), and the stored record stays as it was. Fork creation records
  the creating token through it, in `Record.CreatingTokenID`.
- `RecordBySlug(ctx, q, slug)` returns the record and `true`, or `false`
  and a nil error for any string no record has as its slug.
- `ForkParent(ctx, q, slug)` returns the parent's slug when `slug` names a
  task fork, and `""` for a repository or a slug no record has.
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
`access.Caller{Admin, UserID, TokenID, Workspace}`, built from the
pipeline's `server.Caller` by `access.CallerFrom`. `TokenID` and `Workspace`
are set only for a PAT bound to a workspace, from the `identity.Principal`
in the caller's `Detail` (`Type` `pat` with a `Workspace`): the token's id
and the bound slug. `Level` and `Decide` do not read them. The kind of credential and the user's role are not
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
- a PAT bound to a workspace gets its user's set cut down to the covered
  set of its binding (below), each entry at the user's level: a covered
  workspace the user cannot read stays out;
- a failed read returns the error and the empty set, never part of one.

`Level` and `Readable` share one resolver (`userLevels` in `level.go`); they
differ only in where the user's active orgs and a fork's parent come from.
`Level` reads them from the sources as the answer needs them; `Readable`
takes the orgs it has read and looks a parent up among the records it has
collected, which hold every repository the user has a level on, so a parent
that is not among them is one the user has no level on. A workspace is
therefore in a user's set exactly when `Level` resolves a level other than
`LevelNone` for it, with that level. The visibility decision of the hub
events calls `Readable` (see [Hub events](#hub-events)), and so does the
workspace list (`GET /workspaces`, see Workspace records); the audit query
will.

#### Workspace-bound tokens

`internal/access/coverage.go` says what the binding of a PAT bound to a
workspace reaches. `(*Service).CoveredSet(ctx, caller)` returns, sorted, the
covered workspaces: for a token bound to a repository W, W and every fork of
W whose `creating_token_id` is the token's id; for a token bound to a fork,
the fork alone; for a bound slug with no record, that slug alone. A caller
without a binding covers nothing (`nil`). `(*Service).RelationOf(ctx,
caller, slug)` answers how a workspace a request names stands to the
binding, as an `access.Relation` (`internal/access/binding.go`):
`RelationBound` (`"bound"`) for the bound slug, record or not;
`RelationCreatedFork` (`"created_fork"`) for a fork of it the token created;
`RelationParentOfBoundFork` (`"parent_of_bound_fork"`) for the parent of the
fork the token is bound to; and `""` (none) for every other slug and every
caller without a binding. Both read the records on every call, so a fork the
token has just created is covered at once. `access.Binding{TokenID,
Workspace, Relation}` is the value a binding guard attaches to a request
for its handler (ADR 04), read with `access.BindingOf(r)`; it reports false
for every caller but a bound PAT.

`internal/access/guard.go` holds the binding guard, `(*Service).Guard`, a
`server.Check` for the binding-guard position of the pipeline, after the
route's feature check and before the scope check; `composeAccess` installs
it. It runs only on a route that matched, for a caller that authenticated,
since routing and authentication answer first, and never on a public route.
It does nothing for the admin token, an API key and a PAT that is not
bound. A route names a workspace when its pattern is `/workspaces/:slug` or
lies under it; the slug is the path value as typed. On a route that names
no workspace a bound PAT passes when the method and the pattern, relative to
`/api/v1`, are in the allowlist of `internal/access/allowlist.go`, with the
`Binding` of the bound slug and no relation attached, from which the
route's handler reads the covered set it confines its result to
(`CoveredSet(ctx, access.Caller{TokenID: b.TokenID, Workspace:
b.Workspace})`); every other such route, every identity and admin route
among them, is answered `403 token_bound` with the message `a token bound
to a workspace cannot use this route`, before the scope check, whatever
scopes the token holds. The allowlist is `GET /workspaces`, `GET /events`,
`GET /events/log`, `GET /audit`, `GET /sandbox-profiles`, `POST` and `GET
/sessions`, `GET /sessions/:id`, `POST /sessions/:id/complete`, and `POST`
and `GET /sessions/:id/usage`; like the contributor table it matches the
pattern as registered, so a route reaches a bound token only when it is
registered with these parameter names. On a route that names a workspace,
for a bound PAT the guard asks `RelationOf` and:

- for `bound` and `created_fork`, attaches the `Binding` and passes;
- for `parent_of_bound_fork`, attaches the `Binding` and passes when the
  method and the pattern relative to `/workspaces/:slug` are in the
  contributor table, and answers `403 token_bound` otherwise;
- for none, answers `404 not_found` with the message `workspace not found`,
  the bytes the access check answers for a workspace the caller cannot read,
  for an existing and an unknown slug alike, and before the scope check, so
  a token without the route's scope gets the same `404`.

It makes no access or scope decision: a request it passes reaches the scope
check and the access checks as an unbound PAT of the same user would. Each
`403` and `404` it answers is reported to `Deps.Refusals`, an
`access.RefusalReporter` (`Refused(ctx, access.Refusal)`; nil discards),
as `access.Refusal{TokenID, UserID, BoundWorkspace, RequestedWorkspace,
Method, Route, Status, Surface}`, where `Route` is `/api/v1` followed by the
matched pattern, `RequestedWorkspace` is nil on a route that names no
workspace, and `Surface` is `access.SurfaceAPI` (`"api"`). A failed read of
the records is an error, answered `500 internal`.

`(*Service).GuardGit(ctx, caller, slug, method, pattern)` is the same guard
for the git server, which calls it after authentication with the
`server.Caller`, the slug of the repository, the method and the matched git
route's pattern, and returns an `access.GitDecision{Pass, Status, Binding}`.
A bound PAT passes with its `*Binding` on the bound workspace and on a fork
the token created; on every other slug, the parent of the bound fork
included, `Pass` is false and `Status` `404`, and the refusal is reported
with `Surface` `access.SurfaceGit` (`"git"`), `Route` the git pattern as
given and `RequestedWorkspace` the slug. The admin token, an API key and an
unbound PAT pass with a nil `Binding`. A failed read is a zero decision and
the error. No git server calls it yet.

`internal/access/contributor_routes.go` holds the contributor table, the 25
routes a token bound to a fork reaches on the fork's parent, as method and
pattern relative to `/workspaces/:slug`, unexported and read by the guard
alone: `POST` and `GET /changes`, `GET` and `PATCH
/changes/:number`, `GET /changes/by-commit/:sha`, `POST
/changes/:number/abandon`, `/restore`, `/restack` and `/land`, `GET
/changes/:number/versions`, `/versions/:version`, `/diff`, `/stack`,
`/lands`, `/conflict`, `/conflict/file`, `/approvals`, `/checks` and
`/overlaps`, `GET /lands/:id`, `GET /policy`, `POST /policy/evaluate`, `GET
/activity`, `GET /claims` and `GET /claims/:id`. The guard matches the
pattern as registered, so a route reaches a fork-bound token on the parent
only when it is registered with these parameter names.

`internal/access/events.go` declares `access.EventEmitter` (`Emit(ctx, tx,
access.Event)` in the caller's transaction, `EmitNow(ctx, access.Event)` in
one of its own) over the value `access.Event{Type, Workspace, ActorType,
ActorID, ResourceType, ResourceID, Metadata}`. The event component imports
`internal/access`, so this package cannot name `event.Input`, and the hub
passes the event component behind the interface (`accessEvents` in
`internal/hub/access.go`). There is no discarding default.

`internal/access/mint.go` holds the one minting component of bound tokens,
`(*Service).MintBound(ctx, tx, access.MintInput{Creator, Workspace, Name,
Permissions, Paths, Expires, ExpiresAt})`, which returns the
`identity.PAT` and its value. In the caller's transaction, which it does not
commit, it mints through `identity.MintPAT` with `Workspace` set to the
slug, so the token is owned by `Creator.User`, validated as for `POST
/user/tokens` and, for a PAT creator, held to the subset rule, and then
emits `access.BindEventType` (`parley.token.bind`) through `Deps.Events` in
the same transaction: workspace the bound slug, actor type the creator's
`identity.Principal.Type` (`api_key` or `pat`) and actor the owner,
resource `token` and the token id, and metadata `token_id`, `workspace`,
`permissions`, `paths` (or null) and `expires_at` (or null, in the Token
object's time format), never the value. `identity.MintPAT`'s errors are
returned unchanged (`400 invalid_request`, `403 insufficient_scope`), with
nothing written; a failure of the emitter is returned wrapped, and the
caller rolls back the token with its transaction. A service without an
emitter refuses to mint. `ExpiresAt` is the instant a hub-internal caller
(fork creation, the hub credential of a sandbox) sets in place of
`Expires`. No production caller besides the route below uses it yet.

`internal/access/bound_tokens.go` holds `(*Service).TokenRoutes()`, which
`composeAccess` registers: `POST /workspaces/:slug/tokens` with `Scope`
`tokens:manage`, `Create`, `ShowOnce` `token` and the marks
`access.MarkNeed` `owner`, `access.MarkSubsetRule` and
`access.MarkRefusesBound`, and `GET /workspaces/:slug/tokens` with `Scope`
`tokens:read` and the marks `access.MarkNeed` `owner` and
`access.MarkRefusesBound`. On a route marked `access.MarkRefusesBound`
(`"access.refuses_bound"`) the binding guard answers a bound PAT `403
token_bound` on every workspace whose relation to the binding is not none
(bound, created fork, parent of the bound fork), and on a route that names
no workspace even when it is allowlisted; outside the binding the `404`
comes first. The list handler reads the page (`server.ReadPage`), then, in
one read-only transaction, the ids of the PATs whose `workspace` is the
slug in any state, ordered by the whole second of `created_at` (stored in
nanoseconds) and then `token_id` after the cursor's position, which holds
that second and the id, and each PAT with `identity.PATByID`, and answers them
with `server.WritePage`; the admin token passes, since the access rule
gives it owner, and an archived workspace is listed. On a route marked
`access.MarkSubsetRule` (`"access.subset_rule"`) the access check
`(*Service).SubsetCheck`, installed before `Check`,
decodes the body with `identity.DecodeMintRequest` and applies
`identity.CheckSubset` to a PAT caller, so the `403 insufficient_scope` of
the subset rule comes before the workspace is looked up; a body that does
not decode, or whose paths are not valid, passes to the handler. The
handler answers the admin token `403 forbidden` (`this route needs a user`)
before it reads the body, decodes the body (`400`), and then, in one store
transaction, mints with `MintBound`, refusing an archived workspace with
`409 workspace_archived` after the token is validated and before its event
is emitted, so the refusal rolls back the token and emits nothing. Any other
failure of the insert, the event or the commit rolls back and is answered
`500 internal`. The answer is `201` with the Token object followed by
`token`, the value.

`internal/access/revoke.go` holds the revoking component of bound tokens,
`(*Service).RevokeBoundTo(ctx, tx, slugs)`, for the components that remove
workspaces (workspace delete, fork expiry), which call it in the
transaction that removes them, so the removal and the revocation commit or
roll back together. In that transaction, which it does not commit, one
`UPDATE pats ... RETURNING token_id` sets `revoked_at` to the instant of
`Deps.Now` on every PAT whose `workspace` is one of `slugs`, whose
`revoked_at` is NULL and whose `expires_at` is NULL or after that instant,
and it returns their token ids sorted ascending, a non-nil list that is
empty for no slug or no live token. A token already revoked keeps its
`revoked_at` and an expired one is left as it is (unlike
`identity.RevokeTokenByID`, which revokes an expired token too), and
neither is returned. On a failed write it returns the error and a nil
list, and the caller rolls back. It emits no event: the removal's own event
carries the ids. A revoked token is answered `401 credential revoked` by
authentication from the next request on, before the binding guard, so it
is never reported as a refusal. No production caller uses it yet.

`internal/access/sweep.go` holds the token binding sweep,
`(*Service).SweepBindings(ctx)`, which the hub runs as the maintenance task
`token_binding_sweep`, so that a bound token a removal left behind stops
working within one maintenance interval. In one transaction of `Deps.Store`
it reads the distinct `workspace` of the PATs that are bound and not revoked
and have no row in `workspaces`, and revokes their live tokens with
`RevokeBoundTo`, so a token that is unbound, bound to a workspace with a
record in any status (archived included), revoked or expired is left as it
is. It returns how many it revoked and, when that is more than 0, logs one
info line `revoked the tokens bound to workspaces that no longer exist` with
`count` to `Deps.Logger` (nil discards); it emits no event. On a store
failure the transaction is rolled back and it returns 0 and the error, which
the maintenance loop logs as `maintenance task failed`. A context cancelled
before it begins returns the context's error and writes nothing; once
begun, the transaction runs to its end with a context that a stop does not
cancel, because `database/sql` rolls a cancelled transaction back in a
goroutine of its own, which could close the store's last connection after
the hub has closed the store and returned from `Run`.

`internal/access/refusal.go` holds the hub's `RefusalReporter`,
`access.RefusalLimiter`, built with `access.NewRefusalLimiter(events, now,
logger)` (nil `now` is `time.Now`, nil `logger` discards). It keeps, in
memory and under a mutex, for each token id the time of the token's last
event and the count of refusals suppressed since. A refusal of a token
with no event in the preceding 60 seconds is emitted with `EmitNow` as
`access.RefusedEventType` (`parley.token.bound_refused`): workspace the
bound slug, actor type `pat` and actor the token's user, resource `token`
and the token id, and metadata `token_id`, `bound_workspace`, `method`,
`route`, `status`, `surface`, `suppressed` (the count, then reset to 0) and,
only when the request named a workspace, `requested_workspace`, the slug as
typed; the token's minute starts then. A refusal inside the minute emits
nothing and adds 1 to the count. Each refusal first drops every entry
whose minute has passed with a count of 0, so the map holds only the
tokens refused in the last minute or with refusals still to report. The
emit runs after the guard has decided the answer, with the request's
context detached from its cancellation, and cannot change it: an error, or
a panic, which it recovers, is logged once at warn level (`the refusal of
a workspace-bound token was not recorded`, with `token_id`,
`bound_workspace` and `error`, never a token value) and not retried, and
the token's minute counts as started. A limiter without an emitter logs
the same warning.

### Workspace records

`internal/workspace/store.go` holds `workspace.SchemaStep`
(`workspace_fields`), which extends the table `workspaces` of
`access.SchemaStep` instead of keeping a second table, so that the access
rule and the workspace routes read one row; it applies after
`access.CreatingTokenStep`. It adds one column per `ALTER TABLE workspaces
ADD COLUMN` statement: `workspace_mode` (`standard` or `carry_patch`, a
`CHECK` constraint, default `standard`), `git_url`, `default_branch`,
`display_name` and `description` (default `''`), `head_sha`,
`origin_head_sha`, `clone_error`, `last_sync_at`, `sync_error`,
`upstream_url`, `integration_branch`, `upstream_head_sha`, `fork_point_ref`
and `fork_point_sha` (both NULL unless the workspace is a fork),
`expires_at`, `auto_change`, `purpose`, `session_id`, `settings` (the stored
settings document as one JSON object, default `'{}'`), `created_at` and
`updated_at` (default `0`), `instance_id` (default `''`) and `maintenance`
(`''`, `reclone`, `archive` or `delete`, a `CHECK` constraint). Times are
UnixNano integers. The slug stays the primary key, unique across
repositories and forks. Origin heads have no column: they are refs of the
workspace's copy. `composeWorkspaces` registers the step.

`internal/workspace/record.go` holds `workspace.Workspace`, every field of
a workspace (a nil pointer for a null field) plus `InstanceID`, `Marker` and
`CreatingTokenID`, which no response shows; the type `Status` with
`StatusPending` to `StatusArchived` (the access rule's strings); the type
`Marker` with `MarkerNone` (`""`), `MarkerReclone`, `MarkerArchive` and
`MarkerDelete`; `ModeStandard` and `ModeCarryPatch`; `ForkPoint` (`Ref`,
`SHA`); and `NewInstanceID`, 128 random bits as 32 hex digits. The store
functions take the caller's transaction:

- `Insert(ctx, tx, w)` refuses an empty `InstanceID` with
  `access.ErrInvalidRecord` before writing, stores the access rule's columns
  with `access.InsertRecord` (so a taken slug is `access.ErrDuplicateSlug`)
  and then the other fields with an `UPDATE` in the same transaction; a nil
  `Settings` is stored as `{}`.
- `BySlug(ctx, q, slug)` reads the record through an `access.Querier`; an
  unknown slug is found = false with a nil error, and `Settings` is never
  nil.
- `StartClone(ctx, ex, slug, now)` moves a `pending` or `cloning` workspace
  to `cloning` and clears its `clone_error` in one `UPDATE`; it reports
  started = false, with no error, for any other status or no workspace.
- `UpdateClone(ctx, ex, slug, c)` writes `CloneFields` (`Status`,
  `DefaultBranch`, `HeadSHA`, `OriginHeadSHA`, `CloneError`, `UpdatedAt`),
  every one of them, a nil one as NULL, through an `Execer` (a `*sql.Tx` or
  the `*sql.DB`); an unknown slug is `workspace.ErrNotFound`.

`internal/hub/workspace.go` adds the part `composeWorkspaces`, after
`composeSecrets`. It creates the `*workspace.Service`, registers
`workspace.SchemaStep` and the workspace routes, and adds the start hook
`workspaces`, which builds the service's `workspace.Deps` (the stores, the
secret and event components through the `Secrets` and `Events` interfaces,
the access part's `*access.Service` as `Access`, an `AccessRule` that gives
the workspace list the caller's readable set, the hub's scrubber, logger and clock, `[server].external_url` as
`ExternalURL` and `[workspace].path` as `Root`, and `Lister`, an
`OriginLister` that lists an origin's refs with a `Credential`, the type
`gitrepo.Credential`, so that a resolved credential goes to git unchanged),
applies every function `AdjustWorkspace` added, and binds the service with
`Bind`. Before it builds them, the hook builds the clone workers with
`workspace.NewWorkers(cfg.Workspace.Workers, svc, nil)`, whose refusal stops
the start; after `Bind` it calls their `Start`, so that the clone work left
from before the start is queued before the hub listens (see Clone work),
but runs no clone. The background work `workspace_clones`, which begins
only once the hub listens, launches the workers with `Launch`, so that a
start that fails after the hook (a later start hook, the bind) leaves no
clone running over stores that are closed; it then waits for the stop and
calls the workers' `Stop` with a 10-second deadline, the grace the stop
gives it. The part's one `gitrepo.Git` passes on to git the variables
`GIT_SSL_CAINFO`, `SSL_CERT_FILE`, `HTTPS_PROXY`, `HTTP_PROXY` and
`NO_PROXY` of the hub's environment, read when the hub is composed, and no
other but `PATH` (see [configuration.md](configuration.md#environment-variables)); the
production `Lister` runs its `ListRemote`, and it is `Git`, the
`workspace.Git` (`Clone`, `Open`) of the clone work, and `Fetcher`, the
`workspace.Fetcher` (`FetchOrigin`) of a sync, and `Pusher`, the
`workspace.Pusher` (`Push`) of a push (nil is `gitrepo.Git{}` for
all three; a nil `Scrubber` is a scrubber that knows no value). `Deps` also holds
`InsertWorkspace`, which writes the record in the create transaction (nil
is `workspace.Insert`; a test makes it fail), `BeforeRefSwap`, which a
sync calls between a branch's classification and its swap (nil in
production; a test moves the branch there), and `Notify`, called with the
slug after a create commits so that clone work is picked up; the hub sets
it to the workers' `Enqueue`. `(*Service).Enqueue(slug)` calls `Notify`
(nothing without one): a feature that puts a workspace back to `pending`
calls it once its transaction has committed, since the workers themselves
are the hub's.

`internal/workspace/credential.go` holds `ResolveOriginCredential(ctx,
secrets, slug)`, which every operation that contacts a workspace's origin
calls. It reads the workspace secrets (scope `workspace`, the slug as scope
id) through a `SecretReader` (`Get`, which `*secret.Component` and the
service's `Secrets` have) on every call, one read per name, so that a
changed secret counts at its next use: a non-empty `GIT_PAT` is the user
name `x-token-auth` with the token as password; otherwise `GIT_USERNAME`
and `GIT_PASSWORD`, when both are set; otherwise no credential (the zero
`Credential`). A read error is returned as an error and never taken for no
credential, so that a broken secret store does not turn into an anonymous
request to origin. The names are `SecretGitPAT`, `SecretGitUsername` and
`SecretGitPassword`.

`internal/workspace/create.go` serves `POST /api/v1/workspaces` (scope
`workspaces:write`, a create route, no workspace need; see
[api.md](api.md#post-apiv1workspaces)). The handler refuses the admin token
first, reads the body by an ordered list of members (`createMembers`, each
answering `400 invalid_request` with `<member>: <reason>`, a member able to
depend on those before it; `settings` is applied with
`SettingsRegistry.ApplySettings` from an empty document, and its own
answers pass through unchanged), takes the org `org_id` names
(`identity.OrgBySelector`; unknown is `400`) or else the caller's personal
org (`identity.PersonalOrg`) and checks the caller is an active member of
it (`403` otherwise), answers a slug in use `409 conflict` after a read with
`access.RecordBySlug`. Only a request with a credential then asks the
secret component's `Available()` (false: `secret.UnavailableError`, `503`,
before origin is contacted) and lists origin's refs with the credential
through `Lister` under a 10-second deadline (an error: `400
credential_check_failed` with a fixed message, git's text logged only
through the scrubber as `workspace credential check refused`). Then, in one
store transaction, it inserts the record through `InsertWorkspace` (status
`pending`, a fresh instance id, `created_at` = `updated_at` cut to the
second), stores the credential with the secret component's `Put` as the
workspace secrets `GIT_PAT`, or `GIT_USERNAME` and `GIT_PASSWORD`, and
writes `parley.workspace.create` through the event component; an error of
any of the three rolls all of them back (`500 internal`). A slug taken by a
racing request is `access.ErrDuplicateSlug` from the insert and the same
`409`. The pending record is the clone work; there is no separate queue.

`internal/workspace/response.go` holds `(*Service).RenderWorkspace(ctx, w,
level)`, the one place a workspace object (`workspace.Object`) is built:
every field of the record, a nil one encoded as `null`, plus `org_slug`
(read from the workspace's org), `hub_url` (`ExternalURL` without one
trailing slash, then `/git/<org_slug>/<slug>.git`, or `null` without an
external URL), `access` (the caller's level) and `settings`, the effective
settings of the record, or for a fork those of its parent, read with
`BySlug` at render time so that a change of the parent shows at once (a
fork whose parent has no record shows its own). A record of
kind `fork` goes last through the `ForkView` registered with
`(*Service).RegisterForkView`, so the fork feature completes its object
without this package importing it.

`internal/workspace/read.go` holds `GET /workspaces/:slug`, declared with
`Scope` `workspaces:read` and `Marks` `access.need: read`, so the scope
check and then the access check answer before the handler: the access
check's `404` is the one answer for a workspace the caller cannot read and
for a slug no workspace has. The handler takes the caller's level from
`access.DecisionOf`, reads the whole record with `BySlug` (a record deleted
since the check is the same `404`) and answers it through
`RenderWorkspace`, in every status and for both kinds.

The same file holds `GET /workspaces`, declared with `Scope`
`workspaces:read` and no access need, since it names no workspace; it is on
the binding guard's allowlist. The handler reads the page (`server.ReadPage`),
refuses `include_archived` or `include_forks` with a value other than `true`
or `false` (`400`), and takes the caller's readable set from
`Deps.Access.Readable(ctx, access.CallerFrom(caller))`, which for a
workspace-bound PAT is already cut to its covered set, so the list narrows
with the set and needs no rule of its own. One query (`readList`) then reads
the page's rows: the set's slugs as one JSON parameter (`slug IN (SELECT value
FROM json_each(?))`, every row for the admin token), `status <> 'archived'`
unless asked, `kind = 'repository'` unless forks are asked, or with `parent`
the forks whose `parent_slug` it is when it names a repository workspace
(a parent outside the readable set reads nothing), the cursor's position
`(created_at, slug)` and `ORDER BY created_at DESC, slug ASC LIMIT` the page
plus one. Each row is answered through `RenderWorkspace` with the set's
level for it, and `server.WritePage` writes the page with a cursor of the
last item's stored `created_at` (UnixNano) and slug.

`internal/workspace/update.go` holds `PATCH /workspaces/:slug`, declared
with `Scope` `workspaces:write` and `Marks` `access.need: write`, so the
scope check and then the access check answer before the handler (a member
of the org is `403` in every status, archived included). The handler
decodes the body as an object of raw members, so that a member given as
`null` is told from one not given, then in one store transaction reads the
record with `BySlug` (gone: the same `404`) and judges the body against it
in `applyUpdate`. One table, `updateFields`, says what an update may do with
each field by the workspace's kind: `display_name` and `description` are
mutable on both kinds, `org_id` and `settings` on a repository workspace
only (on a fork `400 not_supported_on_fork`), `ttl`, `purpose` and
`auto_change` on a fork only, through a handler registered for them, and
`slug`, `kind`, `git_url`, `default_branch`, `owner_id`, `workspace_mode`,
`upstream_url`, `integration_branch`, `parent_slug` and `fork_point` on
neither. The checks run in this order, the first failing one answering: an
immutable field, whatever its value; a field that does not apply to the
kind (a fork-only field on a repository workspace, or on a fork with no
handler registered, is `400 invalid_request`); no mutable field; the values
in table order (`display_name`, `description`, `org_id`, `settings`, then
the fork-only fields), each applied to a copy of the record or answering
`400 invalid_request` with `<field>: <reason>` (a value's reader reads
through the update's transaction, and a store error it meets is a `500`,
never a refusal); for an `org_id`, the workspace's owner, not the caller,
an active member of the target org (`identity.ActiveOrgIDs`), else `403
forbidden`; an archived workspace
(`409 workspace_archived`), since validation comes before state; and for a
fork the registered guard. Then it writes `display_name`, `description`,
`org_id`, `settings` and `updated_at` (the hub's time cut to the second),
when the org changed sets `org_id` on every record whose `parent_slug` is
the workspace, runs the fork-only fields' writes and writes the
`parley.workspace.update` event through `Deps.Events` (`updateEventInput`:
the actor from `identity.PrincipalOf`, mapped by `eventActor`, which the
sync event shares: the admin principal to `event.ActorAdminToken` with no
actor id, a user's credential to its type with the user as actor id; `metadata.fields`, the mutable
fields the body carried in `updateFields` order, and for a fork
`metadata.parent_slug`, never a value), all in the same transaction, so a
refusal writes nothing, neither record nor event, and a failed write, the
event's included, keeps nothing. A `null` or empty `display_name` stores the slug, a `null`
`description` the empty string. `org_id` (`moveTarget`) is `null`, the
owner's personal org (`identity.PersonalOrg`), or a string looked up with
`identity.OrgBySelector` (an id in UUID form, else a slug); anything else,
or a string no org has, is `400`. The access rule reads the org from the
same row, so a move changes who may read the workspace and its forks with
no other write. After the commit it reads the record
back and answers it through `RenderWorkspace` with the level
`access.DecisionOf` gives, so the answer is the one a following `GET`
gives.

The fork features add to an update without changing its rules through two
registrations on the service, each under a mutex: `RegisterForkField(name,
ForkFieldHandler)`, for `ttl`, `purpose` or `auto_change` only (another
name panics), whose handler judges the field's raw value and returns the
`ForkFieldWrite` that stores it inside the update's transaction, and
`RegisterForkUpdateGuard(ForkUpdateGuard)`, asked about every update of a
fork after the body is valid and the fork not archived, before anything is
written, its `*server.Error` answered unchanged. `nil` removes either.

`internal/workspace/settings.go` holds `SettingsRegistry`, the setting keys
in registration order, empty in its zero value; the service holds one,
returned by `(*Service).Settings()`. `RegisterSetting(SettingKey)` adds a
key with its `Name`, its `Default` (a function of the default branch), its
`Validate` rule and an optional `Normalize`, the form of an accepted value
that is stored (nil stores the value as given), refusing a duplicate or
incomplete key;
`ApplySettings(stored, raw)` applies a request's settings value (`null`
resets every key; otherwise an object whose unknown keys are refused with
`unknown setting "<key>"`, a `null` value resets that key, every other
value passes its key's rule and is stored normalized; all or nothing, the
first unknown key in byte order named before any refused value, else the
first refused key in registration order); `EffectiveSettings(stored, defaultBranch)`
returns every registered key with its stored value, decoded into the Go
type of the key's default (`bool`, `string`, `[]string`), or its default.
The same file holds the settings table, the one definition of the 21 keys
(name, kind: bool, enum with its strings, URL string or list of branch
globs, and default) in the order they are judged in, and
`RegisterSettingKeys(registry)`, which registers each with its default and
a rule built from its kind: a bool key takes a JSON boolean, an enum key one
of its strings, a URL key a string that is empty or an absolute `https` URL
with a host and no user information, query or fragment once trimmed, stored
trimmed through `Normalize`, and
`protected_branches` an array of at most 50 distinct strings that pass
`ValidateBranchGlob`. Its default is a list holding the default branch, the
empty list (never `null`) while the default branch is `null`.

`internal/workspace/branchglob.go` holds the one branch-glob matcher of the
hub, which every feature reading a list of branch globs calls:
`MatchBranchGlob(glob, branch)` matches the whole name, case-sensitive, `*`
standing for any run of characters except `/`, `**` for any run, every other
character for itself; it runs over the bytes in time proportional to the
product of the two lengths, whatever the stars. `MatchAnyBranchGlob(globs,
branch)` reports whether one glob of a list matches (an empty list matches
nothing), and `ValidateBranchGlob(glob)` accepts 1 to 255 characters that
pass `ValidateBranchName` with each `*` taken as an ordinary character.

`internal/workspace/guard.go` holds `RequireReady(w)`, the state check of
every route that needs a workspace's repository. It reads the marker first:
any marker is `409 workspace_busy`, because a reclone or a delete of a
failed or archived workspace is work the caller should wait out. Then the
status: `pending` or `cloning` is `409 workspace_busy`, `archived` is
`409 workspace_archived` (`server.WorkspaceArchived`), `failed` is
`409 workspace_not_ready`, and `ready` passes. It reads only the record it
is given and takes no lock, so a ready workspace serves any number of
requests at once.

#### Clone work

`internal/workspace/clone.go` holds the clone work. `(*Service).RepoPath(slug)`
is the place of a workspace's copy, `<[workspace].path>/<slug>.git`, and the
one place the path is built. `(*Service).Clone(ctx, slug)` does the clone
work of one workspace:

1. `StartClone` moves a `pending` workspace to `cloning` and clears its
   `clone_error` before origin is contacted; a workspace already in
   `cloning` is taken up again; any other status, or no workspace, is left
   alone, so a failed clone is never tried again on its own.
2. A copy of this workspace at `RepoPath` is kept: one that opens as a
   repository, whose `parley.workspace` is the workspace's instance id,
   whose `remote.origin.url` is its `git_url`, and whose default branch (the
   copy's own `HEAD` branch while the record has no `default_branch`)
   resolves to a commit. Its head of that branch becomes `head_sha`, the
   branch `default_branch`, the origin head it keeps for it, if any,
   `origin_head_sha`, and the status `ready`, in one write; origin is not
   asked and no post-clone step runs. This is how a hub that ended without
   its stop (killed) between the copy's rename and `ready` is resumed. Anything else at `RepoPath` (a file, an
   empty directory, a repository failing one of the tests, the copy of an
   earlier workspace of the same slug) goes on to a fresh clone.
3. `ResolveOriginCredential` reads the credential; a read error fails the
   clone before git runs.
4. Whatever lies at `RepoPath` is removed, and origin is cloned with
   `Git.Clone` into a directory `.clone-<random>` in the repository root
   (no slug starts with a dot, so no copy carries the prefix).
5. A copy without a branch fails with `origin has no branches`; a
   `default_branch` given at creation that the copy does not have fails with
   `origin has no branch "<name>", the default_branch given at creation`.
   Without one, the default branch is the branch the copy's `HEAD` names
   (origin's default branch), or, when that branch does not exist, the first
   branch by name.
6. `SetOriginHead` writes `refs/remotes/origin/<name>` at the head of every
   branch, the workspace's instance id is written to the copy's
   configuration as `parley.workspace`, the copy's `HEAD` is set to the
   default branch, and the directory is renamed to `RepoPath`.
7. One `UpdateClone` records `default_branch`, `head_sha` and
   `origin_head_sha` (the default branch's head) with the status still
   `cloning`.
8. The post-clone step registered for the workspace's `workspace_mode` with
   `(*Service).RegisterPostCloneStep(mode, step)` runs, given the record and
   the copy opened at `RepoPath`; an error it returns is logged as
   `workspace post-clone step failed` and does not change the outcome.
9. The status is set to `ready`.

Any failure removes the clone directory and whatever lies at `RepoPath`,
logs `workspace clone failed` and sets the status to `failed` with
`clone_error` the failure's text through the hub's scrubber, with the
resolved password as a per-call secret value (secret values replaced by
`[redacted]`, URL userinfo removed, at most 1024 bytes); `head_sha` and
`origin_head_sha` become null and `default_branch` keeps its stored value.
No event is written and no further attempt is scheduled; `Clone` returns
nil once the failure is recorded. When `ctx` ends first (the hub stops),
the clone directory is removed, and so is the copy at `RepoPath` once it
has been renamed there (steps 7 to 9), the status is left as it is so that
the work is taken up again, and `Clone` returns the context's error: a stop
leaves nothing at the copy's place, and the next start clones afresh and
runs the post-clone step. A workspace that is ready is recorded with one
write after the step, so a hub that ends without its stop between the
copy's rename and `ready` leaves the workspace in `cloning`, with a copy
step 2 keeps at the next start.

`internal/workspace/worker.go` runs the clone work in the background.
`NewWorkers(size, svc, work)` refuses a size below 1 with an error naming
`[workspace].workers`; `work` is the work run for each slug, `svc.Clone`
when nil. `Enqueue(slug)` appends a slug to a first-in-first-out queue in
memory; a slug already waiting or running is not queued again, and nothing
is queued after `Stop`. `Start(ctx)` removes every `.clone-*` entry of the
repository root (clones a stop interrupted) and queues every workspace in
`pending` or `cloning` in the order of `CloneWork` (by `created_at`, then
slug); it runs no clone. `Launch(ctx)` then starts `size` goroutines that
take slugs from the queue one at a time; the clones run under a context
derived from `ctx`, so they end with it as with `Stop`. Each runs once; a
second call is an error. The queue holds no state of its own: a workspace
in `pending` or `cloning` is its own record of the work, and the next
`Start` reads it again. An error of the work that is not the
end of its context means its outcome could not be recorded: it is logged
as `workspace clone work failed`, and the worker, still holding the slug,
sets the workspace to `failed` with `clone_error` "the outcome of the clone
could not be recorded: <the error>" (scrubbed) and null heads, in one write
that only changes a workspace still in `pending` or `cloning`. When the
store refuses that write too, the worker logs `workspace clone failure not
recorded` and tries again, after 100 ms and then twice as long each time,
up to 30 s, until the store takes it or the workers stop, which leaves the
work to the next `Start`. Once the workspace is failed, its copy at
`RepoPath` is removed. `Stop(ctx)` stops taking work, cancels the
context of the running clones, so that their git processes are killed and
`Clone` leaves their status as it is, waits for the goroutines, then
removes every `.clone-*` entry again and returns the error of that removal
(nil when it succeeds); it returns `ctx`'s error when `ctx` ends before the
goroutines, and nil at once for workers never launched.

#### Sync

`internal/workspace/sync.go` holds the sync of a workspace with its origin.
`(*Service).Sync(ctx, slug, SyncOptions)` reads the record, resolves the
origin credential with `ResolveOriginCredential` at that moment, so that a
changed secret counts at the next sync, and fetches origin into the copy at
`RepoPath` through `Deps.Fetcher` with a deadline of 2 minutes. Two fetches
into one copy both set the origin heads (and `packed-refs`), and must not
interleave there or meet on git's ref locks, so the fetch runs under the
copy's entry of `Service.fetchLocks`, an in-process mutex per
slug (`keyedLocks`, an entry removed when no one holds or waits for it): a
second sync of the workspace waits for the first one's fetch and then runs
its own. Nothing else of a sync takes a lock, and a sync never writes the
workspace's status: tag creation is create-only (a tag another sync
created first is skipped, not an error) and branch moves are single-ref
compare-and-swaps, so syncs, pushes and landings meet only there. It then
creates in the copy, in name order, every tag origin has and the copy
lacks, at origin's value (an annotated tag as its tag object), each with
`CompareAndSwapRef` expecting the tag absent, so that a tag another writer
creates meanwhile keeps that writer's value; a tag the copy has keeps its
value, whether origin has it at another value, has deleted it or never had
it. It then evaluates every branch origin has, in name order, against the
hub's head of it: `created` when the hub has no such branch, `up_to_date`
when the heads are equal, `fast_forward` when the hub's head is an ancestor
of origin's, `ahead` when origin's head is an ancestor of the hub's, and
otherwise `diverged`, or `reset` when `SyncOptions.ResetToOrigin` is set.
`created`, `fast_forward` and `reset` move the branch to origin's head with
`CompareAndSwapRef` expecting the head that was classified (absent for
`created`); the others move nothing, and a diverged branch does not stop
the walk. A swap that answers `gitrepo.ErrRefMoved` means another writer
moved the branch after it was classified: `(*Service).syncBranch` keeps
that writer's head, reads the branch again, classifies it against the same
origin head and acts on the new result, up to `maxBranchEvaluations` (5)
evaluations; the reported result, `local_sha` and the `Before` of the move
are those of the last evaluation, and only a swap that succeeded is told to
the observers and listed in the event's `updated`. When the fifth swap is
lost too, `Sync` returns `server.Internal` naming the branch, before
`completeSync`: no sync field and no event is written, and the moves made
before stay, their observers already told. `Deps.BeforeRefSwap`, nil in
production, is called between a branch's classification and its swap; it
is for tests, which move the branch there as another writer would. A branch only the hub has, never on origin or deleted there, is
neither looked at nor reported. Protected branches are treated as any
other: sync reads neither the push rules nor `protected_branches` and
`protect.owner_push`. It returns a `SyncResult` holding the record read
again after the completion (see below), `CreatedTags`, the tags it created, and `Branches`, one `BranchResult`
(`name`, `result`, `local_sha`, `origin_sha` as JSON, the hub's head after
the branch was handled and origin's head, full commit ids) per origin
branch it did not skip, sorted by name, empty and never nil when there is
none.

Before the walk, after the fetch and the tags, `excludedBranches` builds
the set of branches the walk skips. For a record whose `Mode` is
`ModeCarryPatch` it holds the record's `IntegrationBranch` (when set) and
the names the registered `ExcludedBranchProvider` returns
(`ExcludedBranches(ctx, slug) ([]string, error)`, asked once per sync);
for any other mode it is empty and the provider is not asked. The provider
is registered with `(*Service).RegisterExcludedBranchProvider`, one at a
time, guarded by its own mutex, nil removing it; this package registers
none, the patch feature does when the hub is composed. A skipped branch is
not classified, moved, listed in `Branches` or the event, or told to the
observers; its origin head is still set by the fetch, and `head_sha` and
`origin_head_sha` are read as usual, also when the default branch is the
integration branch. A provider error stops the sync before the walk with
`server.Internal`, its text scrubbed by `Deps.Scrubber`: no branch moved,
no sync field written, no event (tags created by then stay).

A credential that cannot be resolved (nothing is then fetched), a fetch
error and a fetch cut off at its deadline (`gitrepo.ErrDeadline`) are one
failure, handled by `failFetch`: the text `origin fetch failed: ` plus the
error, scrubbed by `Deps.Scrubber` with the credential's password, is
logged (`workspace sync could not fetch`), written as the record's
`sync_error` alone by `recordSyncError` (no other field, no event), and
returned as `server.UpstreamError` with the same text. Nothing in the copy
has changed at that point: a fetch that fails sets no origin head and tags
are created after it.

`internal/workspace/sync_extension.go` declares the extension point of the
carry-patch part of sync: `SyncExtension` with `AfterFetch(ctx, SyncRun)
error` and `BeforeCompletion(ctx, SyncRun, []BranchResult) (SyncAddition,
error)`, registered with `(*Service).RegisterSyncExtension`, one at a time,
guarded by its own mutex, nil removing it; this package imports no
carry-patch package and registers none. `SyncRun` holds the record as read
before the fetch, the caller (`Actor`), `ResetToOrigin` and `OriginHeads`,
a copy of origin's branch heads as fetched. `Sync` takes the extension once,
after the fetch, and only for a record whose `Mode` is `ModeCarryPatch`;
with none, or for a standard workspace, nothing changes. The order is: the
fetch, `AfterFetch`, the tags, `excludedBranches`, the walk,
`BeforeCompletion` (given a copy of `Branches`), `completeSync`. Both
stages run on the sync's context, which the route has detached from the
request. An `AfterFetch` error goes to `failFetch` without the `origin fetch
failed: ` prefix, so a caller tells the two apart: `sync_error` and `502
upstream_error` with the scrubbed text, no branch or tag moved, no event;
the origin heads stay as the fetch, which had succeeded, set them. A
`BeforeCompletion` error is `server.Internal` with its text scrubbed with
the credential's password, as is a member whose value is not valid JSON;
`completeSync` does not run (no field, no event) and the moves made stay.
On success `SyncAddition.UpstreamHeadSHA`, when not nil, is written as
`upstream_head_sha` by `recordSync` in the completion transaction
(`COALESCE`, so nil leaves it), and `SyncAddition.Members` reach the route as
`SyncResult.Members`.

Each `created`, `fast_forward` or `reset` move is reported right after the
ref changed through the exported `(*Service).NotifyBranchMove(ctx,
BranchMove{Workspace, Branch, Before, After, Cause})`, `Before` `""` for a
created branch and `Cause` `MoveCauseSync` (`"sync"`), which a sync
extension calls for its own moves too. It calls every `BranchMoveObserver`
(`BranchMoved(ctx, BranchMove) error`) registered with
`(*Service).RegisterBranchMoveObserver`, once each in the order of
registration (the list is guarded by its own mutex; nil registers nothing);
an observer's error is logged through `Deps.Logger` (`workspace
branch-move observer failed`, with `slug`, `branch`, `cause` and `error`),
and the remaining observers are still called and the sync goes on, since
the branch has moved already. With no observer a sync behaves the same.

When every branch is handled, `completeSync` writes in one `Store.Tx`
`recordSync` (`last_sync_at` = `Deps.Now` in UTC cut to the second,
`sync_error` `NULL`, `head_sha` the copy's head of the record's default
branch and `origin_head_sha` the copy's origin head of it, each `NULL`
when there is none, both read from the copy at that moment rather than
taken from this sync's fetch, so that a sync that completes after an
overlapping later one does not record an older origin than the copy holds; `updated_at` is not written) and the one
`parley.workspace.sync` event (`syncEventInput`), whose actor is
`SyncOptions.Actor` mapped by `eventActor`; a `SyncOptions.Actor` that is
neither the admin token nor a user's credential is an error before
anything is fetched. Its metadata holds the lists `created`,
`fast_forward`, `ahead`, `diverged` and `reset` of branch names (sorted, `[]`
when empty, `up_to_date` branches in none) and `updated`, one `{ref,
before, after}` per moved or created branch and per created tag with full
ref names, `before` `null` for a created ref, sorted by `ref`. `Sync`
returns the record read again after that transaction as
`SyncResult.Workspace`.

`internal/workspace/sync_handler.go` holds `POST /workspaces/:slug/sync`,
declared with `Scope` `workspaces:write` and `Marks` `access.need: write`,
so the scope check and then the access check answer before the handler (a
member of the org is `403` in every status). The handler reads no body. It
reads `reset_to_origin` with `boolParam` (absent or `false`, `true`, else
`400 invalid_request`), reads the record with `BySlug` (gone: the same
`404`), and judges it in `requireSyncable`: a task fork is
`400 fork_has_no_origin`, then `RequireReady`, then the effective setting
`sync_enabled` from the settings registry, `false` being
`400 sync_disabled`, so that nothing is fetched before every check has
passed. It runs `Sync` on `context.WithoutCancel` of the request's context,
so a caller who disconnects does not stop it, with the caller from
`identity.PrincipalOf` as `SyncOptions.Actor`, and answers `200` with the
`RenderWorkspace` object of `SyncResult.Workspace`, the record after the
completion, for the caller's level (`access.DecisionOf`) and the member
`branches`; `syncAnswerJSON` appends each of `SyncResult.Members` after
them, in byte order of the names, leaving out a name the object or
`branches` has already. An error of `Sync` that is a `*server.Error`,
wrapped or not, is the answer; any other is `500 internal`.

`internal/workspace/push.go` holds
`POST /workspaces/:slug/branches/:name/push`, declared like the sync route
with `Scope` `workspaces:write` and `Marks` `access.need: write`, so the
scope check and then the access check answer before the handler looks at
the body. `pushForce` reads the body: none or empty is `force` `false`,
otherwise `server.DecodeJSON` into a map, and `force`, when given, must be
`true` or `false` (`400 invalid_request` otherwise, `null` included). The
handler validates the name the router decoded once with
`ValidateBranchName`, reads the record with `BySlug`, and asks
`pushRefusal` in this order: `RequireReady`, a fork (`400
fork_has_no_origin`), for a `carry_patch` workspace its
`integration_branch` (`403 integration_branch`) and then the registered
`PatchBranchLookup` (`403 patch_branch`; an error is `503 unavailable`,
logged scrubbed), and for a forced push `MatchAnyBranchGlob` over the
effective `protected_branches` of the settings registry (`403
protected_branch`); none of them needs the branch or contacts origin. The
lookup is registered with `(*Service).RegisterPatchBranchLookup`, one at a
time, by the feature that keeps a carry-patch workspace's patch list;
without one no branch is a patch branch. The handler then reads the
branch's head once with `ResolveBranch` (no branch is `404
branch_not_found`), resolves the credential with
`ResolveOriginCredential` and hands one `gitrepo.PushRequest` to
`Deps.Pusher`; it takes no lock. For a forced push the request's `Lease`
is the branch's recorded origin head (`OriginHead`), or `""`, origin must
not have the branch, when none is recorded; `gitrepo.Git.Push` passes it as
`--force-with-lease=refs/heads/<name>:<lease>` and never a plain force, and
reads git's `+` status as `forced` and its stale-info refusal as an error
matching `gitrepo.ErrOriginMoved`, which the handler answers `409
origin_moved` without touching the recorded origin head. The push runs
under a context with `Deps.PushDeadline` (`Bind` makes 0 the fixed 2
minutes, `pushDeadline`); when it passes, before the caller's request
ends, the answer is `504 origin_timeout`, after `gitrepo` killed git's
process group and waited for it. `pushFailure` maps a `*gitrepo.PushError`
by its class: `ErrOriginRejected` to `422 origin_rejected` with origin's
reason, `ErrUnauthenticated` to `502 origin_unauthenticated` (the message
says `no credential` when the resolved credential is zero, otherwise that
origin `rejected` the stored one), `ErrForbidden`, `ErrRepositoryNotFound`
and `ErrUnreachable` to `502 origin_forbidden`, `origin_not_found` and
`origin_unreachable`, and anything else to `502 upstream_error`. A
credential that cannot be read is `500 internal` before the push
operation is called. It answers `200` with `branch`, `sha`,
`remote` `origin`, `remote_url` (`withoutUserinfo` of `git_url`: an https
URL without user information, any other form as stored) and `result`; an
error matching `gitrepo.ErrNonFastForward` is `409 non_fast_forward`,
whose message names the origin head the copy keeps for the branch
(`OriginHead`), read without asking origin. The handler reads that origin
head once, after the credential and the caller (`eventActor`), and from
there on works on `context.WithoutCancel` of the request's context, so a
caller who disconnects ends neither the push, which only
`Deps.PushDeadline` bounds, nor its records. Every push handed to origin
writes one `parley.workspace.push` event (`pushOp.event`, the one place
its texts are put together: `branch`, `sha`, `force`, `ok`, `remote_url`
and `result` or `error_type`) with `writePushRecords`, one `Store.Tx`. A
failure writes the event alone. A push origin took goes through
`recordPushed`: `SetOriginHead` of the branch to the pushed commit first,
since a ref cannot be part of a store transaction, then the transaction
with the event and, for the default branch, `recordOriginHead`
(`origin_head_sha` alone, never `updated_at` or `status`); when the
transaction fails the ref is put back (`SetOriginHead` of the value read
before the push, or `DeleteOriginHead`) and the answer is `500 internal`.

### Hub events

`internal/hub/events.go` adds the part `composeEvents`, after `composeAccess`
in `parts`. It creates a zero `event.Component`, adds the schema step
`event.SchemaStep` (`hub_events`) and binds the component in the start hook
`events` to `event.Deps{Store: rt.Store, Clock: event.SystemClock(rt.Now),
Scrubber: rt.Scrubber, Parents: ..., Access: c.access, Memberships: ...,
Shutdown: c.ShuttingDown(), Authenticate: c.identity.Authenticate,
MaxStreams: ..., Retention: ...}`, the fork-parent lookup being
`access.ForkParent` read in the emitting transaction, `c.access` the
`*access.Service` that `composeAccess` creates and keeps on the composition,
the memberships the same `identity.ActiveOrgIDs` source that service is bound
to, `Shutdown` the hub's shutdown notification, which releases the
requests the component holds, `Authenticate` the identity service's
authenticator, with which a stream checks its credential again at every
heartbeat, and `MaxStreams` and `Retention` the `[events]` settings
`event.SettingsFrom(rt.Config)` reads. The part registers the keys of
`[events]` with `event.Register` (`internal/event/config.go`) when it is
composed: `retention`, a string read with `timefmt.ParseDuration`, at least
`1h`, default `30d`, and `max_sse_connections`, an integer of at least 1,
default 500; a value that breaks its rule fails the load with an error that
names the key, so the hub exits with status 1 before it listens.
`(*Component).Retention()` returns the loaded retention for the retention
run. The part registers the routes of
`(*Component).Routes()` with `AddRoute` when it is composed. `Bind` reads
`last_seq`, and its error fails the start. The start hooks that run after it,
those of the parts listed after `composeEvents` and of `Options.Compose`, may
emit before the hub listens; a call made before the component is bound
returns an error. The part also registers the background work `events`
(grace one second), which runs `(*Component).Run` until the stop, so the
stop waits for it before the stores close
([ADR 05](adr/05-deliver-events-by-reading-the-log-by-position.md)).

The step creates three tables in the transactional store:

- `hub_events`, one row per committed event, keyed by `seq`, with the
  record's fields, `metadata` as JSON text and `parent_slug`, the parent of a
  task fork, which no consumer sees; indexes on `(workspace, seq)` and on
  `time`, which is stored as the text `timefmt.Format` writes;
- `hub_event_seq`, one row with `last_seq`, the last `seq` assigned, starting
  at 0;
- `hub_event_log_bound`, one row with `oldest_seq`, the lowest `seq` the log
  addresses, starting at 1.

`(*Component).Emit(ctx, tx, in)` stores one event inside `tx`, the caller's
transaction of `(*store.Store).Tx`, so the event commits or rolls back with
the state change it reports. It validates `in` first and writes nothing on a
refusal, then increments `hub_event_seq` inside `tx` (the store runs one
write transaction at a time, so commit order is `seq` order; a rolled-back
transaction gives its number back, and deleting event rows never lowers the
counter, across restarts too), takes a version 4 UUID as `id` (`uuid.NewV4`),
the clock's time in UTC at whole seconds as `time` and the last segment of the
type as `action`, and returns the stored `event.Record`. When `in.Parent` is
empty and the workspace is a task fork, the parent's slug is read from the
workspace record and stored with the event; a caller that deletes the fork's
record before it emits passes `Parent` itself. `(*Component).EmitNow(ctx,
in)` runs `Emit` in a transaction of its own, for what is not a store change.

An `event.Input` is refused with an error wrapping `event.ErrInvalidEvent`
whose text names the field (`invalid event: <field>: <reason>`) when: `type`
does not begin with `parley.`, has fewer than three dot-separated segments or
an empty one; `workspace` is an empty string; `resource_type` or
`resource_id` is empty; `actor_type` is not one of `admin_token`, `api_key`,
`pat`, `forge` and `system`; `actor_id` is missing or empty for `api_key` and
`pat`, or present for the other three; `session_id` is not a UUID in the
8-4-4-4-12 form (it is stored in lower case); or `metadata` is not a JSON
object (`Metadata` is anything `encoding/json` encodes, nil and JSON null
being `{}`). Every failure to write returns an error that is not
`ErrInvalidEvent`; a route that returns it answers `500 internal`.

A `Record` marshals to one line with exactly the members `seq`, `id`, `type`,
`time`, `workspace`, `actor_type`, `actor_id`, `resource_type`,
`resource_id`, `action`, `session_id` and `metadata`, in that order: `null`
for an absent `workspace`, `actor_id` or `session_id`, an object for
`metadata` and `time` as `timefmt.Format` writes it.

Metadata leaves the hub with every event, so `Emit` passes every string value
of it, at any depth (object values and array elements, not member names),
through `Deps.Scrubber`, the hub's scrubber, before the row is written: URL
user information and registered secret values are removed and the string is
cut to 1024 bytes. Member names, numbers (kept as written), booleans and
`null` are written back unchanged, in their order. The record `Emit` returns
holds the scrubbed metadata, the same the log serves.

The component cannot tell which metadata member is a list of paths, so an
emitting package builds one with `event.PathList(meta, key, paths)`: it stores
the first 100 paths, in their order, under `key`, and sets `truncated` to
`true` only when it cut the list. Other lists in metadata are stored whole.

The event log is read by position
([ADR 05](adr/05-deliver-events-by-reading-the-log-by-position.md)).
`(*Component).Bounds(ctx)` returns `event.Bounds{OldestSeq, LastSeq}`, both 0
while no event was ever written; `LastSeq` is the counter, not the highest
stored row, and `OldestSeq` is `hub_event_log_bound`, 1 until something has
expired. `(*Component).ReadAfter(ctx, after, limit)` returns the events
with a higher `seq` than `after`, ascending, at most `limit` (all when
`limit` <= 0), and the position examined: the last record's `seq` when the
limit was reached, else `last_seq`. When `after` is below `oldest_seq` minus
1, at least one event after it has left the log, and it returns no record
but an `*event.PositionExpiredError` carrying `OldestSeq`, which matches
`event.ErrPositionExpired`, so a caller never misses events without knowing.
The routes read the log through the same function, which also returns the
bounds of its snapshot. The bounds and the
records are read in one read-only transaction, which begins without the write
lock and sees one snapshot. Since commit order is `seq` order, every read is a
gap-free run of the log, also while other transactions emit.

`(*Component).Expire(ctx, cutoff)` makes the events older than `cutoff`
leave the log: in a write transaction it sets `oldest_seq` to the lowest
`seq` whose `time` is not older than `cutoff`, or to `last_seq` when every
event is older, so the newest event stays. It never lowers the bound, deletes
no row, does nothing on an empty log (returning 0), and returns the new
`oldest_seq`. Its schedule is the retention run's.

`(*Component).Run(ctx)` follows the commits. The store has no after-commit
hook, so while anything waits it reads `last_seq` every 100 ms of real time,
and at once when `Emit` or `EmitNow` kicks it. While nothing waits, it reads
nothing. When `last_seq` has risen it counts the new events by type, calls the
callbacks registered with `(*Component).OnCommit(fn)` with each new record,
in `seq` order, from its one goroutine, and wakes the waiters. A waiter (the
stream and the long-poll) takes a channel from the unexported
`waitAfter(position)`. The channel is closed once a commit of a higher `seq`
has been observed, and in any case within a second. The waiter then reads the
log by position again, so a missed wake-up delays an event and never loses it.
When its context ends, `Run` wakes every waiter and returns.

`(*Component).Counters()` returns `event.Counters{LastSeq, OpenStreams,
WrittenByType}`. It first catches up with the commits itself, so the answer
is exact. `WrittenByType` counts the events committed since the bind, by
type; a rolled-back emit is never counted. `OpenStreams` counts the stream
places taken with the unexported `takePlace(limit)`, which refuses once
`limit` are taken, and not yet given back with `givePlace()`; the limit of
the stream is `Deps.MaxStreams`, none when it is 0.

`internal/event/visibility.go` decides who may see an event, at the time it
is delivered. `(*Component).Visible(ctx, caller, rec)` returns `(bool,
error)` for an `event.Caller{Admin, UserID, CredentialID, Workspace}`, where
`Workspace` is the binding of a workspace-bound PAT and `""` otherwise;
`event.CallerOf(r)` builds it from `server.CallerOf` and
`identity.PrincipalOf` (the zero `Caller`, who sees nothing, on a public
route). The rules:

- the admin token sees every event, also one of a workspace that no longer
  exists; a caller with no user sees none;
- an event of a workspace is seen by a user whose level, from the readable
  set of `Deps.Access` (`access.Service.Readable`), is `admin`, `owner` or
  `member` on the workspace or on the fork parent stored with the event, so a
  fork's events stay visible to the parent's readers after the fork's record
  is deleted. A slug with no record gives no level. The types beginning with
  `parley.secret.`, `parley.token.` or `parley.webhook.` need `owner` or
  `admin` there (`parley.forge.webhook.set` is not one of them);
- an event of no workspace is seen by the user whose id is
  `metadata.scope_id` when `metadata.scope` is `user`, by the members of the
  active org `metadata.scope_id` names when it is `org` (asked through
  `Deps.Memberships`, the access rule's own membership question, so a blocked
  org grants nothing), and by nobody else when there is no other scope;
- a workspace-bound PAT never sees an event of no workspace. It sees an event
  of a workspace its binding covers, asked through the `event.Coverage`
  installed with `(*Component).SetCoverage`, under its user's level. A token
  bound to a task fork also sees, under its user's level, the events of the
  fork's parent whose `resource_type` is `change` and whose change, numbered
  `resource_id`, has the fork as its source, asked through the
  `event.ChangeSources` installed with `(*Component).SetChangeSources`.
  Without a covered set a bound token sees nothing; without a change lookup
  it sees none of the parent's events. The hub installs the covered set of
  `access.Service` (see [Workspace access](#workspace-access)); nothing
  installs a change lookup yet, so a fork-bound token sees none of the
  parent's events.

A read that fails returns `false` with the error, never a grant. The
covered set and the change lookup are kept apart from `Deps`, so the bind in
the start hook does not reset what a part installed when it was composed. A
batch of events (a page of the log, a stream delivery) is decided by one
unexported viewer, which reads the readable set and the active orgs at most
once each, when an event first needs them, asks the coverage once per
distinct slug and the change sources once per distinct change of the batch,
and is dropped with the batch, so a change of access takes effect at
the next delivery. `(*Component).VisibleToOwnerOf(ctx, workspace, rec)`, the
form event webhooks use, is true exactly for the events whose workspace, or
stored fork parent, is `workspace`, of every type, and reads nothing.

The component's `event.Clock` has `Now()` and `NewTimer(d)`, so that tests
control the time events are stamped with and the timers of the waits;
`event.SystemClock(now)` is the hub's.

`internal/event/poll.go` serves `GET /api/v1/events/log` (see
[the API reference](api.md#get-apiv1eventslog)), declared with the scope
`audit:read` and no access mark. It parses `limit` itself rather than through
`server.ReadPage`, which would read `cursor` as the generic list's opaque
cursor, where here it is another name for `after`. The request parsing the
two event routes share is in `internal/event/filter.go`: the query string,
the filters `workspace`, `types` and `session_id` with their matcher (all
filters together, any one `types` entry), and a resume position. A page is
collected by reading the log after the position in batches of 200, each in
one snapshot with the bounds, so `next_after` and `last_seq` agree, keeping
the events that match and that one viewer decides visible, until `limit`
items or the end of the log. An empty page with `wait` above 0 waits on
`waitAfter`, a timer of the component's clock, the request's context and
`Deps.Shutdown`, and reads the log again after every wake-up; when the timer
fires it reads once more, so `next_after` is the position the log holds then.
The page is written without HTML escaping, so its items are the bytes of
`Record`'s JSON form.

`internal/event/stream.go` serves `GET /api/v1/events` (see
[the API reference](api.md#get-apiv1events)), declared with the scope
`audit:read`, no access mark and the route's own `Cache-Control: no-cache`.
It reads the filters with the same parsing as the log, and the resume
position from the `Last-Event-ID` header, which wins, or from `after`;
without either it starts at `last_seq` at open. It takes a stream place
(`takePlace(Deps.MaxStreams)`) before it answers, and when none is free
answers `503 unavailable` with `Retry-After: 5`; it gives the place back
when the stream ends, for any reason. It answers `200` with
`Content-Type: text/event-stream` and starts a writer goroutine. Between the
handler and the writer is the stream's outbox: the handler queues frames,
the writer writes them in order and flushes whenever the outbox is empty, so
a client that reads slowly holds up neither the heartbeats nor the checks.
The handler queues a heartbeat at once and then runs one loop: it reads the
log after the stream's position in batches of 200 through the same snapshot
read as the log, queues each event that matches the filters and that one
viewer per batch decides visible as a `hub_event` frame, and moves the
position to the position examined, also past the events it did not send.
Replay and live delivery are this same loop, so there is no gap and no
repeat between them. When the log has nothing further it waits on
`waitAfter`, one timer on the component's clock, the request's context,
`Deps.Shutdown` and the writer's failure. The timer is due at the next
heartbeat, 30 seconds after the previous one, or earlier when the write
under way reaches 60 seconds before that. At a heartbeat the handler first
authenticates the original request again with `Deps.Authenticate`; a
credential that no longer authenticates ends the stream before any further
frame is queued. It then reads the log once more and queues the heartbeat,
whose `seq` is the position examined.

The stream ends when the client goes away, the hub begins to stop, the
credential no longer authenticates, an event is due while 256 events wait in
the outbox (the event is not queued, so nothing is dropped from the open
stream), or the write under way, which the writer stamps with the
component's clock, has not completed for 60 seconds. On every end the handler
stops the writer and waits for it, so nothing writes to the response after
the handler returns; a write that is blocked on a client that does not read
is ended first with a write deadline in the past through
`http.ResponseController`, so a stop never waits for a client. The one
exception is a response that cannot take a write deadline while a write is
under way: the handler then returns without waiting, rather than wait on the
client, and the writer ends after that write without writing another frame.
A read that fails after the answer began ends the stream too, and the server
logs the error. So does a read that finds the stream's position below
`oldest_seq` minus 1, because events after it left the log since the
previous read: the stream ends without another frame and without a log
line, rather than skip them, and the client that resumes with the last `id`
it received is refused `410` `cursor_expired`.

### Secrets

`internal/hub/secrets.go` adds the part `composeSecrets`, after
`composeEvents` in `parts`. It creates a zero `secret.Component`, registers
`[secrets].key_file` (a string, default `""`), and adds the schema step
`secret.SchemaStep` (`secrets`). Its configuration check calls
`secret.LoadKeys(c.env, path)` on the hub's environment and the path of
`[secrets].key_file`, a relative path taken from `cfg.Dir` as the
`[sandbox]` section's path keys are; an error stops the start as the one
`cannot start` line, before any store is opened, also for the bootstrap, and
stops `backup`, which loads the configuration the same way, as its one
`backup failed` line
([ADR 06](adr/06-load-the-secret-keys-before-the-stores-open.md)). The start
hook `secrets` binds the component to `secret.Deps{Store: rt.Store, Keys:
<the loaded keys>, Now: rt.Now, Scrubber: rt.Scrubber, Events: c.events}`
and reads `(*secret.Component).StartReport`: whether a current and a
previous key are configured, and the number of rows of `secrets` whose
`key_id` is neither key's `secret.KeyID`, counted in SQL without decrypting
anything. A failing count fails the hook. The part's start report
(`addStartReport`) writes it with `(secret.StartReport).Log` once the
listener is bound: the info line `secret encryption` with `key_configured`,
`previous_key_present` and `undecryptable_secrets`, and without a current key
the warn line `secrets cannot be stored: no encryption key configured`. Key
material is never logged: `secret.Key` logs, prints and encodes as JSON as
`[key]`, also inside a struct such as `secret.Keys` that slog's JSON handler
encodes, and the report and the loading errors carry booleans, counts and the names of the
variable or the file only.

The part registers the routes of `(*secret.Component).Routes` and adds
`(*secret.Component).ScopeCheck` to the access checks, after identity's admin
check and access's subset and workspace checks. Every secret route carries
the mark `secret.MarkScope` (`secret.scope`: `user`, `org` or `workspace`),
and the workspace routes also `access.MarkNeed` `owner`, so the workspace
access check answers the member's `403` and the stranger's `404` before the
scope is resolved. The scope check resolves the scope with one function per
scope (`internal/secret/scope.go`): the caller's user id for `user` (the
admin token is refused `403 forbidden`); for `org` the org of
`identity.OrgBySelector`, which the admin token reaches in any status and a
user only when it is among `identity.ActiveOrgIDs`, every other case one
constant `404 org not found`; for `workspace` the path's slug once the
workspace access check has attached its decision. It attaches the scope
with `server.Attach` ([ADR 04](adr/04-let-a-check-attach-a-value-for-the-handler.md)),
and the handlers read it. The list handler pages
`(*secret.Component).ListPage` with `server.ReadPage` and `server.WritePage`,
the lowered key as the position, and reads no value. The create handler
checks the body (`internal/secret/validate.go`), answers
`secret.UnavailableError` without a current key, and then, in one
`store.Tx`, calls `(*secret.Component).CreateEntries` and writes one
`parley.secret.create` event per entry through the component's
`secret.Emitter` (`*event.Component`), so the entries and their events
commit together or not at all. The update handler checks the body's `value`,
answers `secret.UnavailableError` without a current key, and then, in one
`store.Tx`, calls `(*secret.Component).UpdateEntry`, which seals the new
value without opening the old one, and writes `parley.secret.update`; the
delete handler needs no key and calls `(*secret.Component).DeleteEntry`,
which never opens the value, and writes `parley.secret.delete`. Both events
name the key as it is stored, whatever casing the path used.

The part also registers the removal of the secrets of a user and of an org
on the identity service, with `c.identity.RegisterUserRemover` and
`c.identity.RegisterOrgRemover`
([ADR 03](adr/03-register-removers-of-owned-data-on-the-identity-service.md)):
each remover calls `(*secret.Component).RemoveScope` with `secret.ScopeUser`
or `secret.ScopeOrg` and the id the delete gives it. `DELETE /users/:id` and
`DELETE /orgs/:id` call them in their own transaction, before the record is
deleted, so the secrets go with the user or the org; the user delete's
`guardLastAdmin` refuses the only active admin with `409 conflict` before
any remover runs, and a delete that is refused or rolls back keeps them. The
removal reads no value and writes no `parley.secret.*` event.

The part registers the maintenance task `secret_reencryption`, which calls
`(*secret.Component).Reencrypt(ctx, rt.Logger)` (`internal/secret/reencrypt.go`)
in every pass. With a current and a previous key configured, and the two
different, it reads the rows of `secrets` whose `key_id` is the previous
key's `secret.KeyID`, 100 at a time in the order of their `id`, opens each
under the previous key with the binding of its entry and seals it under the
current key with the same binding. It writes each row in a `store.Tx` of its
own with `UPDATE … WHERE id = ? AND key_id = ? AND sealed = ?`, the key id
and sealed bytes it read, so that a row a request updated or deleted in the
meantime keeps the request's result; it never changes `key`, `created_at` or
`updated_at` and writes no event. A value that fails to open stays as it is
and counts as remaining. Without a current or a previous key it converts
nothing. Then it runs the converters other parts registered with
`(*secret.Component).RegisterConverter(fn secret.Converter)`, in
registration order: a `Converter` (`func(ctx, keys secret.Keys) (converted,
remaining int, err error)`) re-seals the values a part keeps outside the
secret table, such as those it stored with `SealValue`, and nothing
registers one yet. The task ends with the info line `secret re-encryption`
with `converted` (the rows it re-sealed plus the converters' counts) and
`remaining` (the rows whose `key_id` is not the current key's, every row
without a current key, plus the converters' counts). A store failure stops
it before the converters and the line and returns the error, which the
maintenance loop logs as `maintenance task failed`; the rows written until
then stay converted, and every value still opens with the two keys. A
converter's error is returned after the line.

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
  `composeSandbox` adds the profile list, `(*sandbox.Service).ProfilesRoute()`,
  this way, with the scope `sandboxes:read` and no workspace need; its
  handler answers from the loaded settings and calls no runtime. It adds
  the hub-wide sandbox list, `(*sandbox.Service).SandboxesRoute()`, the
  same way with no scope: its handler refuses every caller but the admin
  token (`server.Caller.Admin`, not identity's admin rule, which also admits
  the API keys of admin users) with `403 forbidden`, reads the page with
  `server.ReadPage` and the `state` and `user` filters, and asks the
  registered `sandbox.Source` for `limit + 1` items after the cursor's
  position (`created_at`, `id`, both descending) before writing them with
  `server.WritePage`. The source is an interface with the one operation
  `List(ctx, Filter, after *Position, limit)`; the package that stores
  sandboxes registers it with `RegisterSource` while the hub composes or in
  a start hook. Without one the list is empty, and an error of the source is
  `500 internal`, never the runtime-error answer: the list calls no runtime.
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

The no-widening decision is for the handlers that run a command in a
sandbox, attach to it or write a file into it, so that a weak token cannot
act through a sandbox a stronger credential created. `sandbox.HubAccess`
(`Scopes`, `Paths`, `Paths` nil for no paths limit) is what the sandbox's
hub credential may do, and nil is a sandbox without one.
`sandbox.Covers(identity.Principal, *sandbox.HubAccess)` answers it: every
caller covers nil, the admin token and an API key cover everything, a PAT
covers a hub credential when `identity.Satisfies` passes for each of its
scopes and, when the PAT is limited to paths, the hub credential is limited
too and `identity.FirstUncovered` finds none of its globs outside the PAT's;
a caller of any other type covers nothing. `sandbox.RequireCovers(r, ha)`
reads the caller with `identity.PrincipalOf` and returns `403
insufficient_scope` when it does not cover `ha`; the handler calls it once
it has found the sandbox and before it validates the body, and returns the
error without acting.

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
switch that makes the backend unreachable, where the backend can, a way to
reject policy values and to add a proposal, and `EndpointsWithoutPaths`,
which a backend sets when its provider profiles keep no endpoint paths (get
is then expected to return the profile without them). The assertions about
an operation that returns `ErrUnsupported` are skipped, so the contract
holds for a runtime that lacks the operation. Only the policy value test
creates its sandbox in a working directory with a base document; every other
test creates one with neither. `(*Fake).Backend()` gives the fake
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
operation that calls it fail as `ErrUnavailable`, while the operations the
SDK lacks keep answering `ErrUnsupported` without calling it.

`BuildConfig` takes the address from `gateway_url`: its host and port (when
it gives none, 443, or 80 for an `http` gateway), with the `http://` scheme kept for a loopback gateway, which is
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
part of the request, a request with that part is `ErrUnsupported` too and
does not reach the SDK, so that no sandbox runs without the policy or
outside the directory it was asked for: `CreateSandbox` with a working
directory or a base document (the SDK takes typed policy rules: the
protected and added rules go as named network rules, and a rule its network
rule cannot hold is `ErrUnsupported`), and `StartSandbox` with a policy
value or an environment. With SDK v0.1.8 no sandbox can therefore be created
with a base policy or in a working directory; the caller answers
`ErrUnsupported` as its own package says.
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

The contract tests run against this implementation over the test gateway
with what the SDK's in-memory client does not implement added in the
package's tests: commands run as processes of the host, files and policy
drafts kept in memory, and the provider profile `anthropic` the contract's
providers name. The backend sets `EndpointsWithoutPaths`. The assertions
about the operations this implementation answers `ErrUnsupported` are
skipped, and so is the policy value test, whose sandbox has a working
directory and a base document.

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

`parley secrets` (`secrets.go`) is `create`, `list`, `update` and `delete`,
each with a run step of its own. `withSecretArgs` wraps a command's argument
check so that the scope flags and the entries are refused as usage errors
before any setting is resolved: an empty `--org` or `--workspace`, more
than one scope flag on all but `create`, and arguments that `parseEntries`
or the `--from-stdin` check refuses. `secretScopes` turns the flags into the
scopes' routes in the order user, org, workspace, the user's alone without
a flag. `create` sends one request per scope, keeps going after a failure,
prints the items of the scopes that succeeded, writes every failure but the
last with `printError` and returns the last, which `run` prints, so each is
written once and the exit status is the last failure's. `list` reads the
whole list with `printWholeList`, which asks for pages of 200, follows
`next_cursor` and prints `{"items":[…]}`; `delete` prints its `Confirm`
document `{"deleted":"<KEY>"}`.

`parley workspace` (`workspace.go`) is `create`, `list`, `get` and
`update`. `get` is a declaration alone. `list` sends a query parameter for
each flag given (`listWorkspaces`) and prints one page. `create` and
`update` build their bodies from their flags (`workspaceCreateBody`,
`workspaceUpdateBody`) and wrap their argument check so that the body is
built there too: a command line it refuses (an update with nothing to
change, a `--setting` without `=`) is a usage error before any setting is
resolved. Both read `--setting`, registered by `addSettingFlag` as a string
array, through `addSettings`, which parses the values with
`ParseSettingFlags`, the one parser of `--setting`. `create --wait` polls
`GET /workspaces/<slug>` through the shared wait.

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
