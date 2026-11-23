# API reference

The hub, `parleyd`, is used through three doors: the REST API under
`/api/v1`, git smart HTTP under `/git`, and a handful of operational
endpoints. This document explains each feature area from a client's side:
why it exists, how it works, how to use it, and then its routes.

Read [Using the API](#using-the-api) first; it holds the rules every route
follows, and the other sections link back to it instead of repeating them.
For how the hub works inside, see [Architecture](architecture.md); for the
same operations from a terminal, see the [CLI guide](cli.md).

## Contents

| Section | What you find there |
|---|---|
| [Using the API](#using-the-api) | Your first request, credentials and scopes, bound tokens, workspace access, errors, paging, idempotency, rate limit |
| [Health, version and metrics](#health-version-and-metrics) | Probes for an orchestrator, the build of a hub, the Prometheus metrics |
| [Signing in and your account](#signing-in-and-your-account) | The sign-in flow, your profile, API keys, personal access tokens |
| [Administration](#administration) | Managing users, organizations and other people's keys and tokens with the admin token |
| [Workspaces](#workspaces) | Registering a repository, its life and settings, sync from origin, push to origin, archive, reclone, delete |
| [Task forks and workspace tokens](#task-forks-and-workspace-tokens) | A private, expiring copy of a workspace per task, and tokens that work in one workspace |
| [Git over HTTP](#git-over-http) | Cloning and pushing with stock git, what a push may do, opening a change with a push |
| [Carry-patch workspaces](#carry-patch-workspaces) | A patch set kept on top of a moving upstream: patches, rebuilds, remembered resolutions |
| [Source control](#source-control) | Reading files, history and diffs, searching code, committing and rebasing, all without a clone |
| [Changes](#changes) | The unit of work: versions, states, mergeability, stacks and restack |
| [Landing](#landing) | Landing a change or a stack, conflict reports, automatic landing, revert |
| [Landing policy, approvals and checks](#landing-policy-approvals-and-checks) | Rules by path, what blocks a land, approving a version, reporting a check |
| [Awareness](#awareness) | Who else touches these paths: activity, overlaps and claims |
| [Forge](#forge) | Issues and pull requests on GitHub or GitLab through the hub, and the forge's webhooks as hub events |
| [Secrets and variables](#secrets-and-variables) | Values per user, organization and workspace; secrets that can be written but never read back |
| [Sandboxes](#sandboxes) | Isolated environments attached to a fork: life cycle, exec, files, logs, tools, network policy |
| [Sessions and telemetry](#sessions-and-telemetry) | What an agent run did and cost, and the run telemetry an agent may send |
| [Audit and events](#audit-and-events) | The durable event log: stream, long-poll, the audit query, every event type |
| [Event webhooks](#event-webhooks) | Hub events sent to your URL: subscribing, verifying the signature, retries, redelivery |

## Using the API

The hub, `parleyd`, serves one REST API under `/api/v1`. Everything the
`parley` client does goes through it, so anything the client can do, your
own program or agent can do too. This section holds the rules that are the
same on every route: how you authenticate, what a credential may reach, how
errors, lists and retries work. The sections after it describe one feature
area each and link back here instead of repeating these rules.

Git itself is served outside the REST API and follows its own rules; see
[Git over HTTP](#git-over-http).

### Your first request

The base URL is the address of your hub plus `/api/v1`. A hub listens on
port `8080` unless its configuration says otherwise (see
[configuration.md](configuration.md#server-and-storage)). Send your
credential as a bearer token and read JSON back:

```sh
curl -H "Authorization: Bearer $PARLEY_KEY" \
  http://hub.example:8080/api/v1/user
```

```json
{"id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","username":"alice","email":"alice@example.com","full_name":"","role":"user","status":"active","provider":"github","provider_id":"alice","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}
```

Every answer is one JSON object, sent as
`application/json; charset=utf-8`. When a request fails, the object is an
error envelope, and the HTTP status says the same as its `code`:

```json
{"error":{"code":401,"message":"invalid credentials","error_type":"unauthenticated"}}
```

Branch on the status and on `error_type`, never on `message`. See
[Errors](#errors).

A few things to know before you write a client:

- Paths are matched exactly. A trailing slash, a doubled slash or a `.` or
  `..` segment is a `404 not_found`, and the hub never redirects.
- A known path with a method it does not have is `405`
  `method_not_allowed`, with an `Allow` header that lists the methods it
  has. A route that answers `GET` also answers `HEAD`, with the same status
  and headers and no body.
- `/healthz`, `/readyz`, `/version` and `/metrics` are outside `/api/v1`
  and need no credential; see
  [Health, version and metrics](#health-version-and-metrics).

### Credentials

Every route under `/api/v1` needs a credential, except the few public ones
(the sign-in routes; their entries say so). Send it as

```text
Authorization: Bearer <credential>
```

The hub reads a credential from this header only: not from a query
parameter, a cookie or another header. There are three kinds:

| Credential | Looks like | Acts for | Use it for |
|---|---|---|---|
| admin token | `parley_admin_` and 64 hex characters | nobody: it has no user | setting up and administering a hub: creating the first users and orgs. It is created when the hub is bootstrapped (see [cli.md](cli.md#running-the-hub-parleyd)) |
| API key | `parley_<key_id>_<secret>` | a user | a person at a terminal. You get one by signing in (see [Signing in and your account](#signing-in-and-your-account)) |
| personal access token (PAT) | `parley_pat_<token_id>_<secret>` | a user | automation: an agent, a CI job, a script. It is the only credential that can be limited: by [scopes](#scopes), to [one workspace](#workspace-bound-and-path-scoped-tokens), and to a set of paths |

The split follows who holds the credential. An API key is as powerful as
its user, which is right for the person and wrong for anything the person
starts. A PAT is what you hand on: it can never do more than its user, and
you choose how much less.

How they differ in practice:

- **The admin token has no user.** It can reach every workspace and every
  admin route, but a route that needs a user (your own account, creating a
  workspace, a fork or a token) answers it `403 forbidden`.
- **An API key passes every scope check.** Whether it may use the admin
  routes depends on its user's role (see [Administration](#administration));
  what it may do in a workspace depends on
  [workspace access](#workspace-access).
- **A PAT is checked against its scopes** on every route that declares one,
  and then against the same workspace access as its user. It never reaches
  the admin routes.

A secret is shown once, in the answer that creates it. The hub stores only
a hash, so a lost secret cannot be recovered: revoke the credential and
create a new one.

The hub reads the credential and its user on every request. A revocation,
an expiry or a blocked user takes effect from the next request on.

| Answer | When |
|---|---|
| `401 unauthenticated` | no credential, another scheme than `Bearer`, a credential the hub does not know or whose secret is wrong, or one that is revoked or expired |
| `403 forbidden` | the credential is good but its user is blocked, or it is the admin token on a route that needs a user |

A wrong secret gets the same answer as an unknown id, whatever the state of
the credential, so the answer does not tell which part was wrong. After a
`401`, do not retry with the same credential: get a new one.

### Scopes

A scope is a permission of the form `resource:action` that a PAT carries in
its `permissions`. Each route's entry names the scope it needs, if any. The
admin token and API keys carry no scopes and pass every scope check; a
route that names no scope is open to any PAT.

Give a token the scopes of the job it does and no others. An agent that
pushes work and opens changes needs `git:write` and `changes:write`; it
does not need `secrets:read` or `tokens:manage`.

| Scope | Lets a PAT |
|---|---|
| `users:read`, `users:write` | read and change its user's own account |
| `orgs:read` | read the orgs its user belongs to |
| `keys:read`, `keys:manage` | list its user's API keys; revoke one |
| `tokens:read`, `tokens:write`, `tokens:manage` | read PATs; change a PAT's permissions; create and revoke PATs |
| `workspaces:read`, `workspaces:write` | read workspaces; create and change them, their settings, landing policy and event webhooks |
| `git:read`, `git:write` | fetch and read source; push and write source, over git and the source control API |
| `changes:read`, `changes:write` | read changes; open, edit and land them |
| `changes:approve` | record a verdict on a change |
| `checks:write` | report a check on a change |
| `patches:read`, `patches:write` | read and manage the patches of a carry-patch workspace |
| `rebuilds:read`, `rebuilds:write` | read and run rebuilds of a carry-patch workspace |
| `secrets:read`, `secrets:write` | read and manage secrets |
| `vars:read`, `vars:write` | read and manage variables |
| `forge:read`, `forge:write` | read and use the forge integration |
| `sandboxes:read`, `sandboxes:write` | read and run sandboxes |
| `audit:read`, `audit:write` | read events and the audit trail; send agent telemetry |
| `sessions:read`, `sessions:write` | read sessions; start and complete them and report usage |

These 32 names are the whole list, matched exactly. The table says what
each area is for; the route's own entry is the authority on which scope it
needs.

A stronger scope satisfies a check for a weaker one in these cases only:

| The route needs | Also satisfied by |
|---|---|
| `R:read`, for any area `R` that has `R:write` | `R:write` |
| `keys:read` | `keys:manage` |
| `tokens:read` | `tokens:write`, `tokens:manage` |
| `tokens:write` | `tokens:manage` |

`changes:approve` and `checks:write` stand alone: no other scope grants
them and they grant nothing else. A token that may write changes cannot
approve them unless it also holds `changes:approve`, so an agent cannot
approve its own work as a side effect of being allowed to do it.

A PAT without the needed scope is answered `403 insufficient_scope`. The
answer comes before the hub looks at the resource, so it is the same
whether or not the resource exists, and nothing changes. The fix is a token
with the scope, not a retry.

A PAT can create another PAT only within what it holds itself (see
[Signing in and your account](#signing-in-and-your-account)), so a token
never creates a stronger one.

### Workspace-bound and path-scoped tokens

Scopes limit what a token may do. Two further limits say where. They exist
so that you can hand an agent a credential that works in one place: if the
agent leaks the token or goes wrong, the damage stops at the edge of its
task.

Both limits are set when the token is created and never change. Both only
take access away: a limited token is still held to its scopes and to its
user's [workspace access](#workspace-access).

#### Tokens bound to a workspace

A PAT whose `workspace` is a slug is bound to that workspace. You create
one for a workspace, or together with a task fork (see
[Task forks and workspace tokens](#task-forks-and-workspace-tokens)). The
usual pattern is one fork and one token per task: the agent gets the token
bound to its fork and can reach nothing else.

What a bound token reaches:

| Request | Answer |
|---|---|
| a route of the bound workspace, or of a task fork created with this token | goes on to the usual checks |
| a token bound to a task fork, on a contributor route of the fork's parent | goes on to the usual checks |
| a token bound to a task fork, on any other route of the parent | `403 token_bound` |
| a route of any other workspace, whether it exists or not | `404 not_found`, the same answer as for a workspace that does not exist |
| a route that names no workspace and is in the list below | passes; the result holds only what the binding covers |
| any other route | `403 token_bound` |

The contributor routes are what a fork's agent needs in order to propose
its work to the parent and see what happens to it: the change routes
([Changes](#changes)), the land routes ([Landing](#landing)), reading the
policy and evaluating it
([Landing policy, approvals and checks](#landing-policy-approvals-and-checks)),
and reading activity and claims ([Awareness](#awareness)). On the change
and land routes the token acts only on changes whose source is its fork;
the awareness reads show all of the parent's work.

The routes without a workspace that a bound token may call:

- `GET /api/v1/workspaces`
- `GET /api/v1/events`, `GET /api/v1/events/log` and `GET /api/v1/audit`
- `GET /api/v1/sandbox-profiles`
- `POST` and `GET /api/v1/sessions`, `GET /api/v1/sessions/{id}`,
  `POST /api/v1/sessions/{id}/complete`, and `POST` and
  `GET /api/v1/sessions/{id}/usage`

Everything else is closed, whatever scopes the token holds: your account
and its keys and tokens (`/api/v1/user…`), every admin route, creating a
workspace, and `GET /api/v1/sandboxes`. A bound token also cannot create or
list the tokens of a workspace (`…/workspaces/{slug}/tokens`). So a bound
token cannot read, revoke or widen itself, and cannot create another token
except, when it creates a task fork, one bound to that fork.

The same rules hold for git: a bound token can fetch from and push to the
workspaces its binding covers and gets the "not found" answer for any
other.

When a workspace is deleted, or a task fork expires, the tokens bound to it
are revoked with it. Archiving a workspace leaves them alone.

Each refusal of a bound token is reported as an event of type
`parley.token.bound_refused`, at most one per token per minute; the event
after a quiet minute carries the number of refusals in between as
`suppressed`. A token that keeps asking for other workspaces is a signal
worth watching for; see [Audit and events](#audit-and-events).

#### Tokens limited to paths

A PAT may carry `paths`, a list of globs over repository paths, for
example `["docs/**","src/*.go"]`. Such a token can only make writes whose
changed files all match one of its globs: pushes, commits through the
source control API, and the versions of the changes it opens or lands.
Reads are never limited, and neither are writes that touch no repository
file. Use it for an agent whose task is confined to part of a repository.

In a glob, `*` matches any run of characters within one path segment, `**`
any run including `/`, and every other character itself. Matching is
case-sensitive and covers the whole path: `docs/**` matches `docs/a.md`
and `docs/api/b.md`; `src/*.go` matches `src/main.go` but not `src/x/y.go`.

A write outside the globs is refused with `403 path_not_allowed`, and
nothing changes. The error names the first offending file as `error.path`:

```json
{"error":{"code":403,"message":"path src/b.go is outside this token's allowed paths","error_type":"path_not_allowed","path":"src/b.go"}}
```

A token with `paths` can only create tokens whose `paths` lie within its
own.

### Workspace access

Scopes and bindings limit a token. Workspace access is about the person
behind it: what a user may do in a workspace depends on their level there,
and is the same by API key and by PAT.

| Level | Who | May |
|---|---|---|
| `admin` | the admin token, on every workspace | everything that does not need a user |
| `owner` | the user who owns the workspace; on a task fork, also the owner of its parent | everything |
| `member` | a member of the workspace's org, while the org is active | read, and contribute: see below |
| none | everyone else | nothing: the workspace does not exist for them |

Each workspace route needs `read`, `write` or `owner`; its entry says
which. `owner` marks routes that are private to the owner even when they
change nothing, such as listing a workspace's tokens.

| Caller's level | Route needs `read` | Route needs `write` or `owner` |
|---|---|---|
| `admin`, `owner` | granted | granted |
| `member` | granted | `403 forbidden` |
| none | `404 not_found` | `404 not_found` |

Points that surprise people:

- **Members contribute through `read` routes.** Some routes that change
  state need only `read`, so that an org member can work without owning the
  workspace: creating a task fork, opening a change from their own fork,
  recording a verdict, making a claim. Each such route states its own
  condition.
- **A user's role `admin` gives no level.** It governs the admin routes
  only. An admin user who is neither owner nor member of a workspace cannot
  see it.
- **A task fork never opens more than its parent.** A user with no level on
  the parent has none on the fork, even the user who created it.
- **A workspace you cannot read looks exactly like one that does not
  exist**: the same status and the same body, on every route and in every
  state of the workspace. You cannot use the API to find out which slugs
  are taken. A member's `403` gives nothing away, because the member can
  already read the workspace.
- **Archiving does not change levels.** A route checks access first and the
  workspace's status after it.

Workspace objects carry the caller's level as `access`.

### The order of checks

The hub checks every request in a fixed order, and the first check that
fails answers it. The order is fixed so that an answer means the same thing
every time, and so that a caller learns nothing about what they may not
see: the checks that depend on the credential alone come before the ones
that look at the resource.

| Step | Check | Answer when it fails |
|---|---|---|
| 1 | the path and method name a route | `404 not_found`, `405 method_not_allowed` |
| 2 | the credential authenticates | `401 unauthenticated`; `403 forbidden` for a blocked user or for the admin token on a route that needs a user |
| 3 | the [rate limit](#rate-limit) | `429 rate_limited` |
| 4 | the [body](#request-bodies): its size, then its content type | `413 payload_too_large`, `415 unsupported_media_type` |
| 5 | the feature is on (sandbox routes on a hub without sandboxes: `404 sandboxes_disabled`, see [Sandboxes](#sandboxes)), then the [binding](#tokens-bound-to-a-workspace) of a bound PAT | `404 not_found`, `403 token_bound` |
| 6 | a PAT holds the route's [scope](#scopes) | `403 insufficient_scope` |
| 7 | the caller is an admin, on an admin route; the caller's [workspace access](#workspace-access), on a workspace route | `403 forbidden`, `404 not_found` |
| 8 | the [`Idempotency-Key`](#idempotency-keys), on a create route | `400 invalid_request`, `409 conflict`, or the recorded answer |
| 9 | the route itself: the request's fields and the state of the resource | the errors of the route's entry |

A public route skips steps 2 and 5 to 7.

What this tells a client:

- **`401`**: the hub does not accept the credential. Nothing about the
  request was looked at. Sign in again or replace the token.
- **`403`**: the hub knows who you are and the answer is no. `error_type`
  says why: `insufficient_scope` (the token lacks a scope), `token_bound`
  (the token is bound elsewhere), `path_not_allowed` (the token is limited
  to other paths), `forbidden` (you lack the role or the level). Sending
  the request again does not help; a different credential may.
- **`404` on a workspace route**: either there is no such workspace or you
  may not read it. The two are indistinguishable on purpose.
- A `403 insufficient_scope` says nothing about whether the resource
  exists, because scopes are checked before the resource is looked up.
- Step 1 comes before the credential is read, so a wrong path or method is
  `404` or `405` with any credential or none.
- A request body is validated last. A `400` about a field means every
  check above it passed.

### Errors

Every error of the REST API is one JSON envelope:

```json
{"error":{"code":409,"message":"workspace is busy","error_type":"conflict"}}
```

| Member | Value |
|---|---|
| `code` | always equal to the HTTP status |
| `message` | a sentence for people. It may change; do not branch on it |
| `error_type` | a stable, machine-readable type. Branch on this and the status |

An error may carry further members inside `error` that a client can act
on, such as `path` above or the `oldest_seq` of an expired event cursor.
The route's entry names them.

The error types every client should know:

| `error_type` | Status | Meaning | What to do |
|---|---|---|---|
| `invalid_request` | `400` | a field, a query parameter, the JSON or a header is wrong | fix the request; `message` says what is wrong |
| `unauthenticated` | `401` | no credential, or one the hub does not accept | get a new credential |
| `forbidden` | `403` | you lack the role or the workspace level, your user is blocked, or the route needs a user | use another credential, or ask the owner |
| `insufficient_scope` | `403` | the PAT lacks the route's scope | use a token with the scope |
| `token_bound` | `403` | the PAT is bound to a workspace and this route is outside it | use an unbound credential |
| `path_not_allowed` | `403` | a path-scoped PAT wrote outside its paths | drop the file named in `error.path` from the write, or use another token |
| `not_found` | `404` | no such route or resource, or one you may not see | check the path; do not retry |
| `method_not_allowed` | `405` | the path exists, the method does not | read the `Allow` header |
| `conflict` | `409` | the request does not fit the current state, or an `Idempotency-Key` was reused with another body | read the resource again and decide |
| `workspace_archived` | `409` | the workspace is archived and takes no writes | reactivate it first |
| `gone` | `410` | the resource existed and is no longer available | do not retry |
| `payload_too_large` | `413` | the body is over the limit | send less |
| `unsupported_media_type` | `415` | the body is not `application/json` | set `Content-Type` |
| `rate_limited` | `429` | too many requests in the window | wait for `Retry-After` seconds, then retry |
| `internal` | `500` | the hub failed in a way it did not expect | retry later; quote the `X-Request-ID` to the operator |
| `upstream_error` | `502` | a system the hub depends on for this request failed | retry later |
| `unavailable` | `503` | the hub cannot serve this request now | retry later |

A feature may add types of its own (for example `workspace_busy` with
`409`); its section names them and what to do. The envelope is the same for
every type, so a client that does not know a type can fall back on the
status.

Retrying:

- `GET` requests are safe to send again.
- A create (`POST`) is safe to send again when you send an
  [`Idempotency-Key`](#idempotency-keys). Use one whenever a lost answer
  would leave you unsure whether the resource exists.
- Retry `429`, `500`, `502`, `503` and network failures, with a pause that
  grows. Do not retry other `4xx` answers unchanged: the answer will be the
  same.
- A `500` never carries details. They are in the hub's log under the
  request id.

### Lists and paging

Every route that answers a list answers one page of it:

```json
{"items":[{"id":2},{"id":4}],"next_cursor":"eyJwIjo0LCJmIjoi..."}
```

| Member | Value |
|---|---|
| `items` | the page's items, in the list's order; `[]` when empty, never `null` |
| `next_cursor` | a string when more items follow, `null` on the last page |

| Query parameter | Meaning |
|---|---|
| `limit` | the most items in a page: 1 to 200, 50 when absent |
| `cursor` | the `next_cursor` of the previous page; absent for the first page |

To read a whole list, request the first page, then repeat the same request
with `cursor` set to the last `next_cursor`, until it is `null`:

```sh
curl -H "Authorization: Bearer $PARLEY_KEY" \
  "http://hub.example:8080/api/v1/workspaces?limit=100"
curl -H "Authorization: Bearer $PARLEY_KEY" \
  "http://hub.example:8080/api/v1/workspaces?limit=100&cursor=eyJwIjo0LCJmIjoi..."
```

The hub guarantees that every item which exists throughout the walk comes
exactly once and in order. An item added during the walk never makes
another repeat or disappear.

Rules for cursors:

- A cursor is opaque. Do not build or change one.
- A cursor belongs to its query: the route, its path parameters and its
  other query parameters. Keep them the same from page to page. Only
  `limit` may change between pages.
- A cursor grants nothing. Every page passes all the checks again.
- A bad `limit`, a malformed cursor or a cursor from another query is
  `400 invalid_request`.

Some lists have another default or other bounds for `limit`, or add
members after `next_cursor` (such as the commit the list was read from).
Their entries say so.

### Idempotency keys

A network failure can lose the answer to a request that succeeded. If you
then send the request again, you may create the same thing twice. An
`Idempotency-Key` lets you repeat a create safely: the hub remembers the
answer under the key and gives it back instead of running the request a
second time.

Send a key of your choosing, 1 to 255 characters, unique per thing you
want to create (a UUID works well), and reuse it for every retry of that
one request:

```sh
curl -X POST -H "Authorization: Bearer $PARLEY_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: 6b0f3c1e-2d0a-4a51-9a54-0e6f1a3b9c77" \
  -d '{"name":"ci","permissions":["git:write","changes:write"]}' \
  http://hub.example:8080/api/v1/user/tokens
```

The header works on the `POST` routes whose entries call them create
routes. Every other route ignores it.

| Request to a create route | Answer |
|---|---|
| no key | the request runs; nothing is remembered |
| a new key | the request runs; a `2xx` answer is remembered |
| a remembered key with the same body | `200` with the remembered body; nothing runs |
| a remembered key with another body | `409 conflict`; nothing runs |
| an empty key, or one over 255 characters | `400 invalid_request` |

What to rely on:

- **A replay answers `200`**, not the original `201` or `202`, with the
  original body and headers such as `Location`. Treat `200` on a create as
  "already done".
- **Only success is remembered.** After a failure the same key runs the
  request again, so you can keep the key while you retry.
- **A key is yours and the route's.** The same key from another user, or
  sent to another path, is a new request.
- **"The same body" means the same bytes.** Resend exactly what you sent;
  do not re-serialise it.
- **A key is remembered for 24 hours**, and across a restart of the hub.
  After that the key is new again.
- **Secrets are not replayed.** A value a create shows only once, such as
  a new token, is in the first answer only. If you lost that answer, the
  replay tells you the resource exists but not its secret: revoke it and
  create another.
- **Concurrent requests with one key do not race.** The others wait for
  the first and then get its answer.
- A replay passes every check again, so a credential that has lost its
  access since the first request does not get the remembered answer.

### Timestamps and durations

Every timestamp in an answer is an RFC 3339 string in UTC with whole
seconds and the suffix `Z`:

```json
{"created_at":"2026-01-02T03:04:05Z"}
```

There is never a fraction of a second or another offset. A fraction is cut
off, not rounded.

A duration, wherever Parley reads one (a request field, a query parameter,
a configuration key), is a non-negative whole number followed by exactly
one unit:

| Unit | Means | Example |
|---|---|---|
| `s` | seconds | `30s` |
| `m` | minutes | `15m` |
| `h` | hours | `24h` |
| `d` | days of 24 hours | `30d` |

`0s` is zero. Anything else is refused: no unit (`30`), another or an
upper-case unit (`30w`, `30S`), a sign, a fraction (`1.5h`), a compound
(`1h30m`) or spaces.

### Rate limit

The hub limits how many requests one caller may send in a window of 60
seconds, so that one runaway client cannot starve the others. The limit is
600 by default and the operator can change it or turn it off
(`[limits].requests_per_minute`, see
[configuration.md](configuration.md#server-and-storage)).

- The limit is per credential. Two tokens used from one machine each have
  their own count; give each agent its own token and they do not slow each
  other down.
- Requests to public routes, and requests whose credential was refused,
  are counted per client address. The address is that of the connection;
  `X-Forwarded-For` is not read.
- Every request counts, whatever its answer. Git requests count too,
  against the same credential.
- A window starts with the caller's first request and lasts 60 seconds.
- Over the limit, the answer is `429 rate_limited` with a `Retry-After`
  header: the whole seconds, 1 to 60, until the window ends. Wait that
  long and send the request again. Nothing of the refused request was
  processed.
- `/healthz`, `/readyz`, `/version`, `/metrics`, the forge webhook ingress
  (see [Forge](#forge)) and requests answered `404` or `405` for an unknown
  route are never counted or refused.
- The counts start from zero when the hub restarts.

### Request ids and response headers

Every response the hub sends carries an `X-Request-ID`. The id is in the
hub's log lines for the request, so quote it when you report a problem.

You can choose the id yourself, to follow one request through your own
logs and the hub's: send `X-Request-ID` with a UUID in the usual
36-character hyphenated form, and the hub uses it and returns it exactly
as received. Anything else is replaced by a new random UUID.

Every response also carries `X-Content-Type-Options: nosniff`,
`X-Frame-Options: DENY` and `Referrer-Policy: no-referrer`.

Caching:

| Response | `Cache-Control` |
|---|---|
| anything under `/api/v1`, errors included | `no-store`, unless the route's entry states another value |
| `GET /healthz`, `GET /readyz` | `no-cache` |
| `GET /version` | `public, max-age=300` |

### Request bodies

A request body is one JSON object, sent with
`Content-Type: application/json` (letter case and parameters such as
`; charset=utf-8` do not matter). A few routes take another content type;
their entries say so.

| Request | Answer |
|---|---|
| a body over the limit | `413 payload_too_large` |
| a `POST`, `PUT` or `PATCH` with a body and a missing or other content type | `415 unsupported_media_type` |
| a body that is empty where one is needed, is not valid JSON, is not an object, or has anything after the object | `400 invalid_request` |

- The limit is 1 MiB by default (`[server].max_body_size`, see
  [configuration.md](configuration.md#server-and-storage)). A route that
  takes larger bodies states its own limit.
- The size is checked before the content type, and both before anything
  about the resource.
- A request without a body needs no `Content-Type`.
- Members a route does not know are ignored. A misspelt optional field is
  therefore not an error: it is not there. Check the answer for the value
  you meant to set.

## Health, version and metrics

Four routes tell an operator and the tools around the hub how it is doing.
They are outside `/api/v1`, need no credential and ignore one that is
sent, and are never rate-limited, so a probe or a scraper works without
being given a secret and cannot be locked out.

| Route | What it is for |
|---|---|
| [`GET /healthz`](#get-healthz) | liveness: is the process up? |
| [`GET /readyz`](#get-readyz) | readiness: can the hub do its work? |
| [`GET /version`](#get-version) | which build is running? |
| [`GET /metrics`](#get-metrics) | Prometheus metrics |

```sh
curl http://hub.example:8080/readyz
```

```json
{"status":"ready"}
```

### `GET /healthz`

Answers `200` with `{"status":"ok"}` whenever the process serves HTTP. It
checks nothing else, and still answers `200` while the hub is not ready.

Use it as the liveness probe: a hub that does not answer it should be
restarted.

### `GET /readyz`

Tells whether the hub can do its work. It is ready when both of its stores
answer a query.

| Status | Body |
|---|---|
| `200` | `{"status":"ready"}` |
| `503` | `{"status":"not ready"}` |

The `503` body is this plain status document, not an error envelope, and
it does not say what failed: look in the hub's log.

Use it as the readiness probe: send traffic to the hub only while it
answers `200`. Do not restart a hub because of it; that is what `/healthz`
is for.

### `GET /version`

Answers `200` with the build of the running hub. Use it to check what is
deployed, or to see whether a client and a hub fit together.

| Member | Value |
|---|---|
| `version` | the version the binary was built with; `dev` when none was set |
| `commit` | the commit it was built from; `unknown` when none was set |
| `build_time` | the build time; `unknown` when none was set |
| `go_version` | the Go toolchain that built it, for example `go1.27.3` |
| `sandbox_runtime` | only on a hub with sandboxes on (see [configuration.md](configuration.md#sandboxes)): `supported`, the range of sandbox runtime releases the hub works with, and `connected`, the release the runtime reported when the hub started |

```json
{"version":"1.4.0","commit":"3f2a1bc","build_time":"2026-01-02T03:04:05Z","go_version":"go1.27.3","sandbox_runtime":{"supported":">=0.1.0 <0.2.0","connected":"0.1.8"}}
```

`connected` is read once, when the hub starts. The answer may be cached
for five minutes.

### `GET /metrics`

Answers the hub's metrics in the Prometheus text format (version 0.0.4),
to `GET` and `HEAD`. Point a Prometheus scrape job at it:

```text
# HELP parley_lands_total Land requests by final result.
# TYPE parley_lands_total counter
parley_lands_total{result="blocked"} 0
parley_lands_total{result="conflict"} 0
parley_lands_total{result="failed"} 0
parley_lands_total{result="landed"} 2
```

The route takes no credential, and its label values include workspace
slugs. Keep it unreachable from networks you do not trust.

What to rely on when you write queries and alerts:

- Every metric is present from the moment the hub serves. A label with a
  fixed set of values (such as `result`) has a series for each value, at
  `0` until something is counted, so a rate over a series that has never
  fired is `0`, not missing.
- A metric with an open label (`method`, `path`, `status`, `workspace`,
  `model`, `type`, `tool`) has a placeholder series with that label empty
  until its first real series exists.
- Counters start from zero when the hub restarts. Gauges show the current
  state.
- The sandbox metrics stay at `0` on a hub without sandboxes.

#### Requests

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `parley_http_requests_total` | counter | `method`, `path`, `status` | every HTTP request the hub answered, on every surface. `path` is the route's template (`/api/v1/workspaces/{slug}/sync`), never the request's own path, and `unmatched` when no route matched. Watch the rate of `5xx` and of `429` |
| `parley_http_request_duration_seconds` | histogram | `method`, `path` | how long answers take, per route |
| `parley_git_requests_total` | counter | `service` (`info-refs`, `upload-pack`, `receive-pack`), `result` (`ok`, `refused`, `error`) | fetches and pushes over git, and how many are refused or fail |
| `parley_scm_requests_total` | counter | `op` (`refs`, `commits`, `tree`, `file`, `diff`, `compare`, `blame`, `search`, `outline`, `declaration`, `code_index`, `code_search`, `symbols`, `commit`, `ref_create`, `ref_move`, `ref_delete`, `rebase`), `result` (`ok`, `ref_moved`, `refused`, `error`) | use of the source control API by operation. `ref_moved` counts writes that lost a race for a ref |

#### Changes, landing and forks

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `parley_changes_open` | gauge | `workspace` | changes in state `draft` or `open`: the review backlog per workspace |
| `parley_lands_total` | counter | `result` (`landed`, `conflict`, `blocked`, `failed`) | land attempts by how they ended. A rising share of `conflict` means agents work on stale bases; `failed` needs a look at the log |
| `parley_land_attempts` | histogram | none | how many tries a land needed to move its target branch. High values mean heavy contention on one branch |
| `parley_land_duration_seconds` | histogram | none | time from a land request to its final status |
| `parley_forks_active` | gauge | none | task forks that exist: roughly, the tasks in flight |

#### Events, webhooks and audit

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `parley_events_seq` | gauge | none | the last sequence number written to the event log; its rate is the hub's event rate |
| `parley_sse_connections` | gauge | none | open event streams |
| `parley_webhook_deliveries_total` | counter | `result` (`ok`, `retry`, `failed`) | event webhook delivery attempts: delivered, failed and to be tried again, or given up (see [Event webhooks](#event-webhooks)) |
| `parley_webhook_queue_depth` | gauge | none | events waiting to be delivered to event webhooks. A value that keeps growing means a receiver is down or slow |
| `parley_audit_events_total` | counter | `source` (`hub`, `agent`), `type` | hub events written and agent run events received, by type |
| `parley_audit_records` | gauge | `kind` (`hub_event`, `run_event`, `outcome`, `tool_call`, `tool_error`, `trace`, `postmortem`, `session`, `usage`) | stored audit and telemetry records, counted at each retention run: how much the hub keeps |

#### Sessions and agent usage

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `parley_agent_sessions_active` | gauge | `workspace` | sessions in state `active`, updated at each retention run; a workspace without one has no series |
| `parley_agent_tokens_total` | counter | `workspace`, `model`, `direction` (`input`, `output`, `cache_read`) | model tokens that agents reported: what the work costs, per workspace and model |

#### Background work

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `parley_jobqueue_depth` | gauge | `type` (`rebuild`), `status` (`queued`, `running`, `failed`, `completed`, `dead_letter`, `cancelled`) | background jobs by status. Alert on `dead_letter` above zero and on `queued` that does not drain |
| `parley_retention_errors_total` | counter | `step` | failed steps of the retention run (see [configuration.md](configuration.md#retention)) |
| `parley_retention_last_run_timestamp_seconds` | gauge | none | Unix time of the last retention run in which every step succeeded. Alert when it grows old |

#### Sandbox metrics

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `parley_sandbox_runtime_up` | gauge | none | `1` while the sandbox runtime is reachable. Alert on `0` |
| `parley_sandboxes` | gauge | `state` (`provisioning`, `ready`, `stopped`, `completed`, `failed`) | sandboxes by state |
| `parley_sandbox_creates_total` | counter | `result` (`ready`, `failed`) | sandbox creations by outcome |
| `parley_sandbox_seconds` | histogram | none | how long a sandbox ran, from `ready` to its stop, exit or deletion |
| `parley_sandbox_exec_total` | counter | `result` (`ok`, `nonzero`, `timeout`, `error`) | commands run through sandbox exec |
| `parley_sandbox_tool_calls_total` | counter | `tool`, `outcome` (`ok`, `tool_error`, `timeout`, `failed`) | tool calls made in sandboxes, by tool and how they ended |
| `parley_sandbox_tool_call_duration_seconds` | histogram | `tool` | how long such a call ran |
| `parley_sandbox_denials_total` | counter | none | connections and requests the sandbox runtime blocked: agents reaching for what their profile does not allow |
| `parley_sandbox_checkpoints_total` | counter | `result` (`ok`, `empty`, `failed`) | checkpoints attempted |

#### Code index

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `parley_code_index_builds_total` | counter | `outcome` (`ok`, `partial`, `failed`) | code index builds that ended; `partial` is a build stopped by a limit |
| `parley_code_index_build_duration_seconds` | histogram | none | how long a build ran |
| `parley_code_index_bytes` | gauge | none | disk space the code index uses, measured after builds and maintenance, not at every scrape |
| `parley_code_index_wait_seconds` | histogram | `outcome` (`ready`, `timeout`, `failed`) | how long a code search or symbol lookup waited for the index of the head. Many `timeout` means builds do not keep up with pushes |

## Signing in and your account

Parley separates the credential of a person from the credential of
automation. An **API key** stands for you at a terminal: it can do
everything you can, carries no scopes, and you get it by signing in with an
identity provider. A **personal access token (PAT)** stands for a CI job or
an agent: it acts as you, but only within the scopes you gave it, and
optionally only on the paths you named. You hand an agent a PAT, never your
API key, so that a leaked or misbehaving token can do no more than the task
needs and can be revoked on its own.

The formats of the credentials, how to send them, the scope names and what
implies what are in [Using the API](#using-the-api).

### How accounts and credentials work

- **Signing in creates the account.** There is no sign-up route. The first
  time you sign in with a provider, the hub creates your user, a personal
  org named after your username, and your membership of it. Later sign-ins
  find the same user by its identity at the provider and refresh the
  `username` and `email` from it.
- **One live API key per user.** Every sign-in issues a new API key and
  revokes every other live key of the user. Signing in on a second machine
  therefore signs the first one out. PATs are not touched by a sign-in.
- **A secret is shown once.** The hub stores only a hash. The answer that
  issues a key or mints a token is the only one that carries its value; no
  list or read shows it again. If you lose it, refresh the key or mint a new
  token.
- **Lifetimes.** A key or token lives 30, 60 or 90 days, or forever with
  `0`; the default is 90. An API key can be refreshed in place; a PAT cannot
  be extended, you mint a new one.
- **Revoked and expired records are kept.** They stay in your lists with
  `revoked_at` or a past `expires_at`, and no longer authenticate.
  Revocation is final and holds from the next request on.
- **A PAT can never create something stronger than itself.** A PAT that
  mints or widens another token is held to its own scopes and paths. An API
  key is not.
- **These routes need a user.** The admin token has none and is answered
  `403` `forbidden` on every route under `/api/v1/user`. A PAT bound to a
  workspace cannot use them either (see [Using the API](#using-the-api)).

### Signing in, step by step

`parley login` does all of this for you (see
[cli.md](cli.md#parley-login-and-your-account)). A client of your own
implements the same five steps.

1. Ask the hub which providers it has. The route is public.

   ```sh
   curl https://hub.example.com/api/v1/auth/providers
   ```

   ```json
   {"items":[{"name":"github","authorize_url":"https://github.com/login/oauth/authorize?client_id=gh-id&scope=user%3Aemail"}],"next_cursor":null}
   ```

2. Listen on a loopback port, for example `http://127.0.0.1:53124/callback`.
   That URL is your `redirect_uri`.
3. Add two query parameters to the `authorize_url`, your `redirect_uri` and
   a random `state` of your own, and open the result in the user's browser.
   The `state` is yours to check: the hub never sees it.
4. The provider sends the browser back to your `redirect_uri` with `code`
   and `state`. Check that `state` is the one you sent.
5. Hand the code to the hub, with the same `redirect_uri`:

   ```sh
   curl -X POST https://hub.example.com/api/v1/auth/callback \
     -H 'Content-Type: application/json' \
     -d '{"provider":"github","code":"<code>","redirect_uri":"http://127.0.0.1:53124/callback","expires":90}'
   ```

   ```json
   {"user":{"id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","username":"alice","email":"a@x","full_name":"Alice A","role":"user","status":"active","provider":"github","provider_id":"123","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"},"api_key":{"key":"parley_Ab3dE5gH_0123456789abcdefghijABCDEFGHIJKL","key_id":"Ab3dE5gH","expires_at":"2026-04-01T00:00:00Z"}}
   ```

Store `api_key.key` and send it as the bearer credential from now on.

### Minting a token for an agent

Give the token the scopes the job needs and, when the job should only write
part of the repository, the paths it may write:

```sh
curl -X POST https://hub.example.com/api/v1/user/tokens \
  -H "Authorization: Bearer $PARLEY_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name":"ci","permissions":["git:write","changes:write"],"paths":["docs/**","src/*.go"],"expires":90}'
```

```json
{"token_id":"k3x9m2pq","user_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","name":"ci","permissions":["git:write","changes:write"],"paths":["docs/**","src/*.go"],"workspace":null,"expires_at":"2026-04-01T00:00:00Z","revoked_at":null,"created_at":"2026-01-01T00:00:00Z","token":"parley_pat_k3x9m2pq_0123456789abcdefghijklmnopqrstuv"}
```

Pass `token` to the agent. A token minted here is not bound to a workspace;
to confine a token to one workspace or one task, mint it there instead (see
[Task forks and workspace tokens](#task-forks-and-workspace-tokens)).

### Rotating and revoking

| You want to | Call |
|---|---|
| replace an API key's secret before it expires, without a browser | [`POST /api/v1/user/keys/{key_id}/refresh`](#post-apiv1userkeyskey_idrefresh), with that or another live API key |
| replace a lost or expired API key | sign in again; the old keys are revoked |
| take a scope away from a running agent, or give it one more | [`…/tokens/{token_id}/permissions`](#apiv1usertokenstoken_idpermissions); the token's value stays the same |
| rotate a PAT | mint a new one, switch the agent over, then revoke the old one |
| cut off a key or token now | [`DELETE /api/v1/user/keys/{key_id}`](#delete-apiv1userkeyskey_id) or [`DELETE /api/v1/user/tokens/{token_id}`](#delete-apiv1usertokenstoken_id) |

### Account routes

The Scope column is what a PAT needs; an API key needs none.

| Route | Scope | What it does |
|---|---|---|
| [`GET /api/v1/auth/providers`](#get-apiv1authproviders) | public | lists the sign-in providers |
| [`POST /api/v1/auth/callback`](#post-apiv1authcallback) | public | signs a user in and issues an API key |
| [`GET /api/v1/user`](#get-apiv1user) | `users:read` | reads your user |
| [`PATCH /api/v1/user`](#patch-apiv1user) | `users:write` | sets your full name |
| [`GET /api/v1/user/orgs`](#get-apiv1userorgs) | `orgs:read` | lists the orgs you are a member of |
| [`GET /api/v1/user/keys`](#get-apiv1userkeys) | `keys:read` | lists your API keys |
| [`POST /api/v1/user/keys/{key_id}/refresh`](#post-apiv1userkeyskey_idrefresh) | API key only | gives a key a new secret and a new expiry |
| [`DELETE /api/v1/user/keys/{key_id}`](#delete-apiv1userkeyskey_id) | `keys:manage` | revokes a key |
| [`POST /api/v1/user/tokens`](#post-apiv1usertokens) | `tokens:manage` | mints a PAT |
| [`GET /api/v1/user/tokens`](#get-apiv1usertokens-and-get-apiv1usertokenstoken_id) | `tokens:read` | lists your PATs |
| [`GET /api/v1/user/tokens/{token_id}`](#get-apiv1usertokens-and-get-apiv1usertokenstoken_id) | `tokens:read` | reads one PAT |
| [`DELETE /api/v1/user/tokens/{token_id}`](#delete-apiv1usertokenstoken_id) | `tokens:manage` | revokes a PAT |
| [`PUT`, `PATCH`, `DELETE /api/v1/user/tokens/{token_id}/permissions`](#apiv1usertokenstoken_idpermissions) | `tokens:write` | replaces, adds or removes a PAT's scopes |

A `key_id` or `token_id` that does not exist and one that belongs to
another user get the same `404` `not_found`, so these routes tell you
nothing about other users' credentials.

### `GET /api/v1/auth/providers`

Lists the identity providers this hub can sign users in with, ordered by
name, so that a client knows where to send the browser. Public; the answer
may be cached for five minutes. A hub with no provider configured answers
an empty list, and nobody can sign in to it (see
[configuration.md](configuration.md#sign-in-providers)).

| Field | Meaning |
|---|---|
| `name` | `github` or `google` |
| `authorize_url` | the provider's authorize endpoint, already carrying the hub's `client_id` and the scopes the hub needs; add `redirect_uri` and `state` yourself |

The answer never holds a client secret.

### `POST /api/v1/auth/callback`

Exchanges an OAuth code for a signed-in user and a new API key. Public. At
a first sign-in it creates the user; at every sign-in it revokes the user's
other live API keys. An `Idempotency-Key` is ignored.

| Field | Required | Meaning |
|---|---|---|
| `provider` | yes | the `name` of a configured provider |
| `code` | yes | the code the provider gave your client |
| `redirect_uri` | yes | the redirect URI the code was issued for |
| `expires` | no | the key's lifetime in days: `0`, `30`, `60` or `90` as a JSON integer; default `90`, `0` never expires |

The hub accepts two kinds of `redirect_uri`: a loopback listener,
`http://localhost` or `http://127.0.0.1` with any port and path, and a URL
on the hub's own `[server].external_url` (same scheme, host and port). It
refuses anything else, `https://localhost` and `http://[::1]` included.

The answer has `user`, the [User object](#get-apiv1user), and `api_key`
with `key` (the credential, shown this once), `key_id` and `expires_at`
(`null` for a key that never expires).

What a first sign-in creates:

- The user, `active`, with the provider's username, verified e-mail and
  display name. If the username is taken, the hub uses the first free of
  `<name>-2` to `<name>-10`. The role is `user`, with one exception: the
  first person to sign in with the e-mail the hub was bootstrapped with
  becomes `admin`, as long as no admin exists yet.
- The user's personal org, named after the username. Its slug is the
  username in lower case with every character outside `[a-z0-9_-]` turned
  into `-` (`Alice.Smith` gives `alice-smith`); a slug that would start with
  a digit gets the prefix `u-` (`42bob` gives `u-42bob`). If the slug is
  taken, the hub uses the first free of `<slug>-1` to `<slug>-10`.

A later sign-in never changes your `id`, `role`, `full_name` or personal
org. If your new username at the provider is taken on the hub, you keep the
old one.

A sign-in the hub refuses changes nothing: no user is created and no key is
revoked.

| Error | When | What to do |
|---|---|---|
| `400` `invalid_request` | a field is missing or malformed, the provider is not configured, or the `redirect_uri` is not allowed | fix the request; the provider was not called |
| `401` `unauthenticated` | the provider rejected the code | start the flow again for a new code |
| `403` `forbidden` | the user is blocked | ask an admin |
| `409` `conflict` | the account cannot be created or refreshed: the e-mail belongs to another user, or no free username or org slug is left | ask an admin to resolve the clash |
| `502` `upstream_error` | the provider could not be reached, did not answer in 15 seconds, or gave no user id or no verified e-mail | retry; verify your e-mail at the provider |

### `GET /api/v1/user`

Answers your own user: the quickest way to check that a credential works
and whom it acts for.

```json
{"id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","username":"alice","email":"alice@example.com","full_name":"","role":"user","status":"active","provider":"github","provider_id":"alice","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}
```

| Field | Meaning |
|---|---|
| `id` | the user's UUID; it never changes |
| `username`, `email` | taken from the provider, refreshed at every sign-in |
| `full_name` | a display name; may be empty |
| `role` | `user` or `admin`; `admin` opens the [administration routes](#administration) to the user's API key |
| `status` | `active` or `blocked` |
| `provider`, `provider_id` | the identity the user signs in with |
| `created_at`, `updated_at` | timestamps |

### `PATCH /api/v1/user`

Sets your `full_name`, the one member of your user you can change
yourself. The body is `{"full_name":"Alice A."}`; the empty string clears
the name. The answer is the updated User object. A body without a string
`full_name` is `400` `invalid_request`.

### `GET /api/v1/user/orgs`

Lists the active orgs you are a member of, ordered by `name`, as
[Org objects](#post-apiv1orgs). A blocked org is left out until an admin
unblocks it.

### `GET /api/v1/user/keys`

Lists your API keys in every state, live, revoked and expired, newest
first. Use it to find the `key_id` to refresh or revoke, and to see when a
key expires.

```json
{"key_id":"Ab3dE5gH","user_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","expires_at":"2026-04-01T00:00:00Z","revoked_at":null,"created_at":"2026-01-01T00:00:00Z"}
```

`expires_at` is `null` for a key that never expires, `revoked_at` is `null`
for a key that is not revoked.

### `POST /api/v1/user/keys/{key_id}/refresh`

Gives one of your live API keys a new secret and a fresh lifetime, without
a new sign-in. The key keeps its `key_id` and the lifetime in days it was
issued with, counted again from now. The old secret stops working at once,
so store the new value before your next request. The body is ignored, and
every call refreshes again.

Only an API key may call this route: a PAT gets `403` `forbidden` whatever
its scopes.

```json
{"key":"parley_Ab3dE5gH_0123456789abcdefghijABCDEFGHIJKL","key_id":"Ab3dE5gH","expires_at":"2026-05-01T00:00:00Z"}
```

| Error | When |
|---|---|
| `403` `forbidden` | the caller is a PAT or the admin token |
| `404` `not_found` | you have no key with this id |
| `409` `conflict` | the key is revoked or expired; sign in again instead |

### `DELETE /api/v1/user/keys/{key_id}`

Revokes one of your API keys, the one the request came with included. The
answer is the Key object with `revoked_at` set. Revoking a key that is
already revoked answers it unchanged; an expired key can be revoked too.
`404` `not_found` when you have no key with this id.

### `POST /api/v1/user/tokens`

Mints a PAT that acts as you. A create route: it accepts an
`Idempotency-Key`.

| Field | Required | Meaning |
|---|---|---|
| `name` | yes | a label for you, 1 to 255 characters |
| `permissions` | yes | at least one scope, each a registered scope name (see [Using the API](#using-the-api)) |
| `paths` | no | 1 to 50 globs, relative to the repository root, that limit which files the token's writes may touch; absent or `null` means no limit |
| `expires` | no | lifetime in days: `0`, `30`, `60` or `90`; default `90`, `0` never expires |

`paths` restricts writes only; reads are not limited. A glob must match the
whole repository path, case-sensitively, and must not start with `/`:

| In a glob | Matches |
|---|---|
| `*` | any run of characters without `/`, also none |
| `**` | any run of characters including `/`, also none |
| anything else | itself |

`docs/**` matches `docs/a.md` and `docs/api/b.md` but not `docs`;
`src/*.go` matches `src/main.go` but not `src/x/y.go`. A token's `paths`
are fixed when it is minted.

The answer is `201` with the Token object plus `token`, the credential:

| Field | Meaning |
|---|---|
| `token_id` | the id you use to read, change and revoke the token |
| `user_id` | the user the token acts as |
| `name`, `permissions`, `paths` | as given |
| `workspace` | the workspace the token is bound to; `null` for a token minted here |
| `expires_at`, `revoked_at` | `null` when unset |
| `created_at` | when it was minted |
| `token` | `parley_pat_<token_id>_<secret>`, only in the answer that minted it |

A replay of the same `Idempotency-Key` answers `200` with the Token object
**without** `token`, and mints nothing. If the first answer was lost, revoke
that token and mint another.

**The subset rule.** When the caller is itself a PAT, it can only mint
within what it holds:

- Every requested scope must be in the caller's own `permissions`, by exact
  name. What a scope implies does not count: a PAT with `git:write` cannot
  mint one with `git:read`.
- A caller limited to `paths` must give the new token `paths`, and each
  requested glob must be covered by one of the caller's: `src/**` covers
  `src/api/*.go`; `docs/*` does not cover `docs/**`.

An API key may mint any scopes and any paths.

| Error | When |
|---|---|
| `400` `invalid_request` | a field is missing, of the wrong type or out of range, or `permissions` names something that is not a registered scope |
| `403` `insufficient_scope` | the calling PAT lacks `tokens:manage`, or asks for a scope or a path beyond its own; the message names the first one |

### `GET /api/v1/user/tokens` and `GET /api/v1/user/tokens/{token_id}`

List your PATs, or read one. The list holds every token in any state, the
ones bound to a workspace included, newest first. Both answer Token objects
without the token's value. `404` `not_found` when you have no token with
this id.

### `DELETE /api/v1/user/tokens/{token_id}`

Revokes one of your PATs, the one the request came with included. The
answer is the Token object with `revoked_at` set. Revoking a token that is
already revoked answers it unchanged. `404` `not_found` when you have no
token with this id.

### `/api/v1/user/tokens/{token_id}/permissions`

Change what a live PAT may do without issuing a new value, so the agent
that holds it keeps running. The change holds from the token's next
request. The body is the same for all three methods:

```json
{"permissions":["git:read","changes:write"]}
```

| Method | Effect | `permissions` |
|---|---|---|
| `PUT` | replaces the token's scopes with the list | registered scopes; may be empty |
| `PATCH` | adds the scopes the token does not have yet | at least one registered scope |
| `DELETE` | removes the scopes given; ones the token does not have are ignored | at least one string |

The answer is the Token object after the change. The token's `name`,
`paths`, `workspace` and `expires_at` never change this way.

**A token left with no scope is revoked.** A `PUT` with `[]`, or a `DELETE`
of every scope the token has, revokes it in the same step.

A calling PAT is held to the [subset rule](#post-apiv1usertokens) for the
scopes a change adds: it must hold each added scope itself, so a PAT cannot
widen another token or itself. A caller limited to `paths` can add scopes
only to a token whose own `paths` its globs cover. Removing scopes is never
restricted.

| Error | When |
|---|---|
| `400` `invalid_request` | `permissions` is missing or not an array of strings, a `PUT` or `PATCH` names an unregistered scope, or a `PATCH` or `DELETE` sends `[]` |
| `403` `insufficient_scope` | the calling PAT lacks `tokens:write`, or adds a scope or reaches a path beyond its own |
| `404` `not_found` | you have no token with this id |
| `409` `conflict` | the token is revoked or expired |

## Administration

An operator needs to manage accounts without depending on any of them:
create a user before that person has signed in, suspend an account or an
organization, remove one, and cut off a credential that leaked. The
administration routes do this, and they are open to two callers only.

- **The admin token** is the operator's credential. The hub writes it once,
  at its first start (see [cli.md](cli.md#running-the-hub-parleyd)). It
  belongs to no user, so it works when no user exists yet and cannot be
  locked out by blocking or deleting one. For the same reason it cannot do
  what needs a user, such as the routes under `/api/v1/user`.
- **An API key of a user with the role `admin`.** The role is read on every
  request: promote a user and their key passes from the next request,
  demote them and it is refused.

Every other caller gets `403` `forbidden`. That includes every PAT, also a
PAT of an admin that holds every scope: administration is never delegated
to automation. The refusal comes before the request is looked at, so it is
the same whatever the request names.

The role `admin` governs these routes only. It does not give a user access
to other people's workspaces.

### How administration works

**Users.** A user has a `role`, `user` or `admin`, and a `status`, `active`
or `blocked`. Users normally appear by signing in (see
[Signing in and your account](#signing-in-and-your-account)); you can also
create one ahead of time. The first admin is the person who signs in with
the e-mail the hub was bootstrapped with; after that, admins promote
others.

**Suspending.** Blocking a user revokes nothing. While the user is blocked,
every API key and PAT of theirs is answered `403` `forbidden`, and they
cannot sign in. Unblocking makes the same credentials work again, unless
they were revoked or have expired meanwhile. Use blocking for "stop now,
decide later", and revocation or deletion for "never again".

**Deleting a user** is all or nothing and cannot be undone. It removes the
user, every API key and PAT of theirs (workspace-bound ones included),
every membership, and the user's own secrets and variables. The orgs the
user owned, the personal org among them, are kept with their other members;
their `owner_id` becomes `null`. Delete those orgs yourself if you want
them gone.

**The last admin.** The hub refuses to demote or delete the only user who
is both `admin` and `active` (`409` `conflict`), also under concurrent
requests. Blocking has no such guard: if you block the last admin, the
admin token can still unblock them.

**Orgs.** An org groups users; a membership is the pair of an org and a
user and carries no role. An org's `owner_id` is informational: the owner
is not automatically a member, and removing the owner's membership leaves
`owner_id` as it is. Blocking an org keeps its memberships but switches
them off: the org disappears from its members' org lists, grants them no
workspace access, and only an admin can read it. Unblocking restores it
with the same members. Deleting an org removes it, its memberships and its
secrets and variables; the users stay, and the name and slug become free.

**Naming a user or an org in a path.** You do not need the UUID. The shape
of the value decides how it is looked up, and only that one lookup is
tried:

| `{id}` or `{user_id}` of a user | Looked up as |
|---|---|
| a hyphenated UUID | the user's `id` |
| anything else containing `@` | the user's `email` |
| anything else | the user's `username` |

| `{id}` of an org | Looked up as |
|---|---|
| a hyphenated UUID | the org's `id` |
| anything else | the org's `slug` (never its `name`) |

The match is exact, letter case included. A value that names nothing is
`404` `not_found`.

### Example: bring a team onto the hub

```sh
H=https://hub.example.com/api/v1
A="Authorization: Bearer $ADMIN_TOKEN"   # the admin token, or an admin's API key

# an org for the team
curl -X POST $H/orgs -H "$A" -H 'Content-Type: application/json' \
  -d '{"name":"Acme Inc","slug":"acme","url":"https://acme.example"}'

# a user who has not signed in yet, and their membership
curl -X POST $H/users -H "$A" -H 'Content-Type: application/json' \
  -d '{"username":"carol","email":"carol@example.com","provider":"github","provider_id":"1"}'
curl -X PUT $H/orgs/acme/members/carol -H "$A"

# someone left: suspend at once, look at what they hold, then remove
curl -X POST $H/users/dave@example.com/block -H "$A"
curl $H/users/dave@example.com/tokens -H "$A"
curl -X DELETE $H/users/dave@example.com -H "$A"
```

### Administration routes

"admin" means the admin token or an API key of a user with the role
`admin`.

| Route | Scope | What it does |
|---|---|---|
| [`POST /api/v1/users`](#post-apiv1users) | admin | creates a user and their personal org |
| [`GET /api/v1/users`](#get-apiv1users) | admin | lists users |
| [`GET /api/v1/users/{id}`](#get-apiv1usersid) | admin | reads a user |
| [`PATCH /api/v1/users/{id}`](#patch-apiv1usersid) | admin | sets a user's full name |
| [`POST /api/v1/users/{id}/promote`](#role-and-status-of-a-user) | admin | gives the role `admin` |
| [`POST /api/v1/users/{id}/demote`](#role-and-status-of-a-user) | admin | gives the role `user` |
| [`POST /api/v1/users/{id}/block`](#role-and-status-of-a-user) | admin | suspends the user |
| [`POST /api/v1/users/{id}/unblock`](#role-and-status-of-a-user) | admin | ends the suspension |
| [`DELETE /api/v1/users/{id}`](#delete-apiv1usersid) | admin | deletes the user and what they hold |
| [`GET /api/v1/users/{id}/keys`](#keys-and-tokens-of-a-user) | admin | lists a user's API keys |
| [`GET /api/v1/users/{id}/tokens`](#keys-and-tokens-of-a-user) | admin | lists a user's PATs |
| [`DELETE /api/v1/users/{id}/keys/{key_id}`](#keys-and-tokens-of-a-user) | admin | revokes a user's API key |
| [`DELETE /api/v1/users/{id}/tokens/{token_id}`](#keys-and-tokens-of-a-user) | admin | revokes a user's PAT |
| [`POST /api/v1/orgs`](#post-apiv1orgs) | admin | creates an org |
| [`GET /api/v1/orgs`](#get-apiv1orgs) | admin | lists orgs |
| [`GET /api/v1/orgs/{id}`](#get-apiv1orgsid) | `orgs:read` | reads an org; open to its members |
| [`PATCH /api/v1/orgs/{id}`](#patch-apiv1orgsid) | admin | renames an org or sets its URL |
| [`GET /api/v1/orgs/{id}/members`](#get-apiv1orgsidmembers) | `orgs:read` | lists an org's members; open to its members |
| [`PUT /api/v1/orgs/{id}/members/{user_id}`](#put-and-delete-apiv1orgsidmembersuser_id) | admin | adds a member |
| [`DELETE /api/v1/orgs/{id}/members/{user_id}`](#put-and-delete-apiv1orgsidmembersuser_id) | admin | removes a member |
| [`POST /api/v1/orgs/{id}/block`](#status-of-an-org) | admin | suspends the org |
| [`POST /api/v1/orgs/{id}/unblock`](#status-of-an-org) | admin | ends the suspension |
| [`DELETE /api/v1/orgs/{id}`](#delete-apiv1orgsid) | admin | deletes the org and its memberships |

### `POST /api/v1/users`

Creates a user without a sign-in, together with their personal org and
their membership of it, exactly as a first sign-in would (see
[`POST /api/v1/auth/callback`](#post-apiv1authcallback) for how the org's
slug is derived). Use it to set up memberships before someone's first day.
The user gets no credential: they receive their API key when they sign in
with the provider identity you name. A create route: it accepts an
`Idempotency-Key`.

| Field | Required | Meaning |
|---|---|---|
| `username` | yes | the user's name on the hub |
| `email` | yes | the user's e-mail |
| `provider` | yes | the identity provider the user will sign in with, such as `github` |
| `provider_id` | yes | the user's id at that provider; the sign-in finds the user by this pair |
| `full_name` | no | a display name; empty when absent |

The answer is `201` with the [User object](#get-apiv1user). The role is
always `user`, also for the hub's bootstrap e-mail: promote explicitly.

| Error | When |
|---|---|
| `400` `invalid_request` | a required field is missing, empty or not a string |
| `409` `conflict` | the username, the e-mail or the provider identity belongs to another user, or an org already has the username as its name, or no free slug is left for the personal org; the message says which |

### `GET /api/v1/users`

Lists the active users, newest first. Add `?include_blocked=true` to
include blocked users; any other value is ignored. A cursor is valid only
with the `include_blocked` it was issued for.

### `GET /api/v1/users/{id}`

Answers the User object of one user, active or blocked.

### `PATCH /api/v1/users/{id}`

Sets a user's `full_name`, as [`PATCH /api/v1/user`](#patch-apiv1user) does
for the caller: the body is `{"full_name":"Alice B."}`, and the empty string
clears the name. No other member of a user can be edited; `username` and
`email` follow the provider.

### Role and status of a user

`POST /api/v1/users/{id}/promote`, `/demote`, `/block` and `/unblock` set
the role `admin`, the role `user`, the status `blocked` and the status
`active`. They read no body and answer the updated User object. Each is
idempotent: a user who already has the role or status is answered
unchanged.

What blocking does to the user's credentials, and why the last active admin
cannot be demoted, is under [How it works](#how-administration-works).

| Error | When |
|---|---|
| `404` `not_found` | no such user |
| `409` `conflict` | `demote` only: the user is the only active admin; promote someone else first |

### `DELETE /api/v1/users/{id}`

Deletes a user, active or blocked, whatever their role, with everything
listed under [How it works](#how-administration-works). The answer is `204` with no
body. Afterwards the user's former keys and tokens are answered `401`
`unauthenticated`, like credentials that never existed. The delete is
atomic: if any part of it fails, nothing was removed and you can retry.

| Error | When |
|---|---|
| `404` `not_found` | no such user |
| `409` `conflict` | the user is the only active admin, also when the request comes from that user's own key |

### Keys and tokens of a user

`GET /api/v1/users/{id}/keys`, `GET /api/v1/users/{id}/tokens`,
`DELETE /api/v1/users/{id}/keys/{key_id}` and
`DELETE /api/v1/users/{id}/tokens/{token_id}` let an admin see and revoke
any user's credentials without that user: the way to cut off one leaked
token while the account keeps working.

They behave like the routes a user calls on their own credentials
([`GET /api/v1/user/keys`](#get-apiv1userkeys),
[`GET /api/v1/user/tokens`](#get-apiv1usertokens-and-get-apiv1usertokenstoken_id),
[`DELETE /api/v1/user/keys/{key_id}`](#delete-apiv1userkeyskey_id),
[`DELETE /api/v1/user/tokens/{token_id}`](#delete-apiv1usertokenstoken_id)):
the lists hold every key or token in any state, workspace-bound tokens
included, newest first and without secrets; a revocation answers the Key or
Token object with `revoked_at` set, and one that is already revoked is
answered unchanged.

There is no route to mint a credential for another user or to change
another user's token permissions. A user with the role `user` cannot
use these routes on their own credentials either; they use the routes under
`/api/v1/user`.

`404` `not_found` when the user does not exist, or the user has no key or
token with that id.

### `POST /api/v1/orgs`

Creates an org. It starts `active` and with no members, also when it has an
owner. A create route: it accepts an `Idempotency-Key`.

| Field | Required | Meaning |
|---|---|---|
| `name` | yes | the display name; must not be blank, stored trimmed, unique |
| `slug` | yes | the org's handle in paths: 1 to 128 characters of `a`-`z`, `0`-`9`, `_` and `-`, starting and ending with a letter or digit; unique, and fixed for the org's life |
| `url` | no | a link for the org; empty when absent |
| `owner_id` | no | the `id` (UUID, not a username or e-mail) of an existing user; no owner when absent |

The answer is `201` with the Org object:

```json
{"id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","name":"Acme Inc","slug":"acme","url":"https://acme.example","owner_id":null,"status":"active","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}
```

Names and slugs are compared exactly, so `acme inc` is free beside
`Acme Inc`.

| Error | When |
|---|---|
| `400` `invalid_request` | `name` or `slug` is missing or malformed, or `owner_id` is not the id of a user |
| `409` `conflict` | another org has the name or the slug |

### `GET /api/v1/orgs`

Lists the active orgs, ordered by `name` byte by byte (so `Zed` comes
before `alpha`). Add `?include_blocked=true` to include blocked orgs; any
other value is ignored. A cursor is valid only with the `include_blocked`
it was issued for.

### `GET /api/v1/orgs/{id}`

Answers one Org object. This is not an admin-only route: any authenticated
caller may send it, and a PAT needs `orgs:read`.

- An admin reads any org, a blocked one included.
- Everyone else, a PAT of an admin included, reads an org only when their
  user is a member and the org is `active`.

An org you may not read is answered exactly like one that does not exist,
`404` `not_found`, so the route does not reveal which orgs there are.

### `PATCH /api/v1/orgs/{id}`

Sets an org's `name`, its `url`, or both; send at least one. The slug never
changes: a `slug` in the body is ignored. The answer is the updated Org
object.

| Error | When |
|---|---|
| `400` `invalid_request` | neither field is sent, one is not a string, or the name is blank |
| `404` `not_found` | no such org |
| `409` `conflict` | another org has the name; nothing is changed, the `url` included |

### `GET /api/v1/orgs/{id}/members`

Lists an org's members, ordered by `username`. Who may read it follows
[`GET /api/v1/orgs/{id}`](#get-apiv1orgsid): admins for any org, members
for their own active orgs, and `404` `not_found` for everyone else. A PAT
needs `orgs:read`.

```json
{"items":[{"org_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","user_id":"1b2c3d4e-5f6a-4b7c-8d9e-0f1a2b3c4d5e","username":"alice","email":"alice@example.com","created_at":"2026-01-01T00:02:00Z"}],"next_cursor":null}
```

`created_at` is when the membership was created, not the user.

### `PUT` and `DELETE /api/v1/orgs/{id}/members/{user_id}`

`PUT` makes a user a member of an org; `DELETE` ends the membership. Both
read no body and answer `204`. `{id}` is an org selector and `{user_id}` a
user selector, so `PUT /api/v1/orgs/acme/members/carol` works.

`PUT` is idempotent: a user who is a member already stays one. Blocked
users and blocked orgs are accepted. `DELETE` keeps the user and the org and leaves the org's
`owner_id` alone.

| Error | When |
|---|---|
| `404` `not_found` | no such org or no such user; for `DELETE` also when the user is not a member |

### Status of an org

`POST /api/v1/orgs/{id}/block` suspends an org and
`POST /api/v1/orgs/{id}/unblock` ends the suspension. They read no body and
answer the updated Org object. Both are idempotent. While an org is blocked
its memberships are kept but inactive, as described under
[How it works](#how-administration-works). `404` `not_found` when there is no such
org.

### `DELETE /api/v1/orgs/{id}`

Deletes an org, a personal or a blocked one included, with all its
memberships and its secrets and variables. The users who were its members
or owner are kept. The answer is `204` with no body; afterwards the org is
`404` for everyone, and a new org may take its name and slug. The delete is
atomic: if any part of it fails, nothing was removed and you can retry.
`404` `not_found` when there is no such org.

## Workspaces

A workspace is the hub's record of one git repository. You register a
repository so that the hub holds its own copy of it: agents and people then
clone, push, read and commit against the hub instead of the forge, the hub
decides what may reach a protected branch, and every write is recorded.
The repository you register stays the source of truth outside the hub. It
is the workspace's **origin**: the hub clones it once, brings its new
commits in when you [sync](#post-apiv1workspacesslugsync), and sends work
back when you [push a branch](#post-apiv1workspacesslugbranchesnamepush).

There are two kinds. A **repository** workspace is registered from a git
URL and is what this section describes. A **task fork** is a short-lived
workspace made from a repository workspace for one task; see
[Task forks and workspace tokens](#task-forks-and-workspace-tokens). A
repository workspace in mode `carry_patch` also follows an upstream; see
[Carry-patch workspaces](#carry-patch-workspaces).

The rules every route follows (credentials, scopes, bound tokens, who may
read or write a workspace, the error envelope, lists, idempotency) are in
[Using the API](#using-the-api). In short: the owner of a workspace and the
admin token write it, the active members of its organization read it, and
everyone else gets `404 not_found`, the same answer as for a slug that does
not exist.

### The life of a workspace

Creating a workspace only records it. The clone runs in the background, so
you read the workspace again until its `status` settles.

| Status | Meaning | What moves it on |
|---|---|---|
| `pending` | recorded, waiting for its clone | the hub starts the clone: `cloning` |
| `cloning` | the hub is cloning origin | success: `ready`; failure: `failed`, with the reason in `clone_error` |
| `ready` | in service: git, the API, forks and changes work | archive: `archived`; reclone: `pending` |
| `failed` | the clone failed; the hub does not retry on its own | reclone: `pending`; archive: `archived` |
| `archived` | out of service, nothing lost | reactivate: `pending`; delete: gone |

A `pending` or `cloning` workspace survives a restart of the hub: the clone
is taken up again when the hub starts. While a workspace is not `ready`,
the routes that need its repository answer `409`: `workspace_busy` while it
is `pending` or `cloning`, or while an archive, reclone or delete runs on
it; `workspace_not_ready` when it is `failed` (reclone it);
`workspace_archived` when it is archived. Retry after a `workspace_busy`.

### Staying in step with origin

The hub never fetches origin on its own. A sync fetches every branch and
tag of origin and then treats each branch by how the two sides relate: a
branch only origin moved is fast-forwarded, a branch only the hub moved
(landed work that has not been pushed yet) is left alone and reported
`ahead`, and a branch that moved on both sides is reported `diverged` and
left alone unless you ask for `reset_to_origin=true`. Sync brings in
whatever origin has, so it also moves protected branches.

Work leaves the hub only when you push a branch to origin. The push sends
one branch, never overwrites a commit on origin the hub has not seen, and
is recorded as an event whether it succeeds or fails. Archive and reclone
also offer every branch to origin first, as a safety copy.

### Registering and syncing: an example

Register a private repository, wait for the clone, sync it later and
publish a branch:

```sh
curl -sS -X POST https://hub.example/api/v1/workspaces \
  -H "Authorization: Bearer $PARLEY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"slug":"demo","git_url":"https://example.com/a.git","git_pat":"<forge token>"}'
# 201, "status":"pending"

curl -sS https://hub.example/api/v1/workspaces/demo \
  -H "Authorization: Bearer $PARLEY_API_KEY"
# repeat until "status" is "ready" or "failed"

curl -sS -X POST https://hub.example/api/v1/workspaces/demo/sync \
  -H "Authorization: Bearer $PARLEY_API_KEY"

curl -sS -X POST https://hub.example/api/v1/workspaces/demo/branches/main/push \
  -H "Authorization: Bearer $PARLEY_API_KEY"
```

### Workspace routes

| Route | Scope | What it does |
|---|---|---|
| [`POST /api/v1/workspaces`](#post-apiv1workspaces) | `workspaces:write` | registers a repository |
| [`GET /api/v1/workspaces`](#get-apiv1workspaces) | `workspaces:read` | lists the workspaces you can read |
| [`GET /api/v1/workspaces/{slug}`](#get-apiv1workspacesslug) | `workspaces:read` | reads one workspace |
| [`PATCH /api/v1/workspaces/{slug}`](#patch-apiv1workspacesslug) | `workspaces:write` | changes its name, description, organization or settings |
| [`POST /api/v1/workspaces/{slug}/sync`](#post-apiv1workspacesslugsync) | `workspaces:write` | brings in origin's commits |
| [`POST /api/v1/workspaces/{slug}/branches/{name}/push`](#post-apiv1workspacesslugbranchesnamepush) | `workspaces:write` | pushes one branch to origin |
| [`POST /api/v1/workspaces/{slug}/archive`](#post-apiv1workspacesslugarchive) | `workspaces:write` | takes the workspace out of service |
| [`POST /api/v1/workspaces/{slug}/reactivate`](#post-apiv1workspacesslugreactivate) | `workspaces:write` | brings an archived workspace back |
| [`POST /api/v1/workspaces/{slug}/reclone`](#post-apiv1workspacesslugreclone) | `workspaces:write` | replaces the hub's copy with a fresh clone |
| [`DELETE /api/v1/workspaces/{slug}`](#delete-apiv1workspacesslug) | `workspaces:write` | deletes an archived workspace, or a task fork |

A scope applies to a personal access token (PAT) only; `workspaces:write`
also satisfies `workspaces:read`. All routes but the two reads and the
create need write access to the workspace. The errors that follow from the
shared rules (`403 insufficient_scope`, `403 forbidden` for an org member
on a write, `404 not_found`, the `409` states above) are not repeated per
route.

### The workspace object

Every route that answers a workspace answers this object. A field without
a value is `null`, never left out.

| Field | Meaning |
|---|---|
| `slug` | the workspace's name, unique on the hub |
| `kind` | `repository` or `fork` |
| `workspace_mode` | `standard` or `carry_patch` |
| `git_url` | origin; `null` for a fork |
| `default_branch` | the default branch; `null` until the clone takes it from origin. For a fork, its base branch |
| `owner_id`, `org_id`, `org_slug` | the owning user and the organization |
| `status` | see [The life of a workspace](#the-life-of-a-workspace) |
| `display_name`, `description` | free text |
| `head_sha` | the hub's head of the default branch. For a fork, the parent's current head of the fork's base branch |
| `origin_head_sha` | origin's head of the default branch as the hub last saw it |
| `clone_error` | why the clone failed |
| `last_sync_at`, `sync_error` | when the last sync completed, and why the last one failed; a completed sync clears the error |
| `upstream_url`, `integration_branch`, `upstream_head_sha` | carry-patch only |
| `parent_slug`, `fork_point`, `expires_at`, `auto_change`, `purpose`, `session_id` | fork only; see [Task forks and workspace tokens](#task-forks-and-workspace-tokens) |
| `settings` | every setting with its effective value; see [Workspace settings](#workspace-settings) |
| `hub_url` | the git remote of the workspace on the hub; `null` when the hub has no external URL configured |
| `access` | your level on it: `owner`, `member` or `admin` (the admin token) |
| `created_at`, `updated_at` | timestamps |

### Workspace settings

Settings tune how the hub treats one workspace. You set them at creation
or with [`PATCH`](#patch-apiv1workspacesslug); the hub stores only the keys
you set and answers all of them with their effective values. A task fork
has no settings of its own: it shows, and is governed by, its parent's.

| Key | Values | Default | What it is for |
|---|---|---|---|
| `sync_enabled` | bool | `true` | `false` makes the sync route refuse with `400 sync_disabled` |
| `protected_branches` | 0 to 50 distinct branch globs | the default branch | the branches that only landing, sync and the owner's direct push may move; see [What a push may do](#what-a-push-may-do) |
| `protect.owner_push` | bool | `true` | lets the owner fast-forward a protected branch by pushing directly |
| `land.strategy` | `rebase`, `squash` | `rebase` | how a change is put onto its target branch; see [Landing](#landing) |
| `land.by_author` | bool | `true` | lets a change's author land it, not only the workspace's owner |
| `land.delete_source` | bool | `true` | deletes the source branch of a landed change |
| `land.allow_override` | bool | `true` | lets the owner land a change whose policy requirements are still pending, with a reason |
| `changes.require_intent` | bool | `true` | a change that is not a draft must state its intent |
| `changes.auto_restack` | bool | `true` | the hub rebases dependent changes when what they sit on moves; see [Changes](#changes) |
| `audit_fetches` | bool | `false` | writes a `parley.git.fetch` event for every fetch that sends a pack |
| `forge_api_url` | `""` or an `https://` URL | `""` | the API endpoint of origin's forge when it cannot be derived from `git_url`; see [Forge](#forge) |
| `upstream_forge_api_url` | the same | `""` | the same for the upstream of a carry-patch workspace |
| `code_index` | bool | `true` | builds the code index for search and symbols; see [Source control](#source-control) |
| `patch_branch_source`, `patch_divergence_policy`, `push_patches_to_origin`, `auto_rebuild_after_push`, `auto_rebuild_after_sync`, `rebuild_strategy`, `rebuild_fail_mode`, `rebuild_push_integration_branch`, `squash_merge_detection` | | | carry-patch only; see [Carry-patch workspaces](#carry-patch-workspaces) |

The carry-patch keys and their values: `patch_branch_source` (`hub`,
`origin`; default `hub`), `patch_divergence_policy` (`replace`, `report`;
`replace`), `push_patches_to_origin` (bool; `false`),
`auto_rebuild_after_push` and `auto_rebuild_after_sync` (bool; `true`),
`rebuild_strategy` (`rebase`, `merge`; `rebase`), `rebuild_fail_mode`
(`fail_fast`, `continue`; `fail_fast`), `rebuild_push_integration_branch`
(bool; `false`) and `squash_merge_detection` (`ancestry_only`,
`content_based`, `both`; `both`).

A forge URL must be absolute, start with `https://`, and carry no user
information, query or fragment. A **branch glob** is 1 to 255 characters
and matches a whole branch name, case-sensitive: `*` matches any run of
characters except `/`, `**` any run including `/`, and every other
character only itself. `release/*` matches `release/1.x` but not
`release/1/x`; `release/**` matches both. While you have not set
`protected_branches`, it follows the default branch; once you set it, the
empty list included, your list stands.

### `POST /api/v1/workspaces`

Registers a repository as a workspace owned by you. It takes an API key or
an unbound PAT; the admin token cannot own a workspace and gets
`403 forbidden`. It honours `Idempotency-Key`.

| Field | Required | Meaning |
|---|---|---|
| `slug` | yes | 3 to 64 characters of `a`–`z`, digits and hyphens, starting with a letter, ending with a letter or digit, no two hyphens in a row |
| `git_url` | yes | origin: `https://<host>/<path>` without user information, or `git@<host>:<path>` |
| `default_branch` | no | a branch of origin; default: origin's own default branch |
| `org_id` | no | the id or slug of an organization you are an active member of; default: your personal organization |
| `display_name` | no | at most 128 characters; default: the slug |
| `description` | no | at most 1024 characters |
| `workspace_mode` | no | `standard` (default) or `carry_patch` |
| `upstream_url`, `integration_branch` | with `carry_patch` | see [Carry-patch workspaces](#carry-patch-workspaces); `upstream_url` is required there, and both are refused on a `standard` workspace |
| `settings` | no | an object of [settings](#workspace-settings); a key given as `null` keeps its default |
| `git_pat` | no | a token for a private origin |
| `git_username`, `git_password` | no | the alternative to `git_pat`, given together |

A credential is only accepted with an `https://` origin. The hub checks it
against origin before it stores anything (it lists origin's refs, with a
deadline of 10 seconds), stores it encrypted as the workspace's secrets
`GIT_PAT`, or `GIT_USERNAME` and `GIT_PASSWORD`, and never shows it again
in an answer, an event or a log. It is presented to the host of `git_url`
only, not to a host origin redirects to. To change it later, write those
secrets; see [Secrets and variables](#secrets-and-variables).

The answer is `201` with the [workspace object](#the-workspace-object) in
status `pending`. The create writes a `parley.workspace.create` event.

| Error | When | What to do |
|---|---|---|
| `400 invalid_request` | a field is refused; the message names it. Also an organization that does not exist | fix the body |
| `400 credential_check_failed` | origin refused the credential, could not be reached or did not answer in time | check the credential and the URL |
| `403 token_bound` | a PAT bound to a workspace | use an unbound credential |
| `403 forbidden` | the admin token, or you are not an active member of the organization | |
| `409 conflict` | a workspace has the slug, whatever its status | choose another slug |
| `503 secrets_unavailable` | a credential was given and the hub has no secret encryption key | configure one; see [Secret keys](configuration.md#secret-keys) |

### `GET /api/v1/workspaces`

Lists the workspaces you can read, newest first, as a
[list](#using-the-api) with `limit` and `cursor`. Each item is a workspace
object with your `access` on it. A user sees the workspaces they own and
those of their active organizations; the admin token sees all; a bound PAT
sees only what its binding covers.

| Parameter | Meaning |
|---|---|
| `include_archived` | `true` lists archived workspaces too |
| `include_forks` | `true` lists task forks too |
| `parent` | lists only the task forks of this workspace |

Archived workspaces and forks are left out by default. A boolean parameter
with any value other than `true` or `false` is `400 invalid_request`.

### `GET /api/v1/workspaces/{slug}`

Answers one workspace, repository or fork, in any status, to a caller who
can read it. Poll it after a create, a reactivate or a reclone to see the
clone reach `ready` or `failed`.

### `PATCH /api/v1/workspaces/{slug}`

Changes exactly the fields the body carries. It works in every status but
`archived`.

| Field | Meaning |
|---|---|
| `display_name` | at most 128 characters; `null` or `""` sets it back to the slug |
| `description` | at most 1024 characters; `null` clears it |
| `org_id` | an organization id or slug, or `null` for the owner's personal organization. The workspace's owner must be an active member of it. The workspace moves with all its forks, and read access follows: members of the new organization gain it, members of the old one lose it. The `hub_url` changes with the organization |
| `settings` | merged key by key: a value sets the key, `null` returns it to its default, a key not given keeps its value; `"settings": null` resets all. One refused key refuses the whole update |
| `ttl`, `purpose`, `auto_change` | task forks only (`400 invalid_request` on a repository workspace); see [Lifetime and expiry](#lifetime-and-expiry) |

```json
{"display_name":"Payments","settings":{"land.strategy":"squash","protected_branches":["main","release/**"]}}
```

`slug`, `kind`, `git_url`, `default_branch`, `owner_id`, `workspace_mode`,
`upstream_url`, `integration_branch`, `parent_slug` and `fork_point`
cannot be changed: a body that carries one is `400 invalid_request`, as is
a body with no field to change. A task fork takes its organization and its
settings from its parent, so `org_id` or `settings` on a fork is
`400 not_supported_on_fork`, and a fork can be updated only while its
parent is `ready` (otherwise `409 workspace_busy`, `workspace_not_ready`
or `parent_archived`).

The answer is the updated workspace object. Each update writes a
`parley.workspace.update` event that names the fields changed, never their
values.

### `POST /api/v1/workspaces/{slug}/sync`

Brings a `ready` repository workspace up to date with origin. It takes no
body and one query parameter, `reset_to_origin` (`true` or `false`,
default `false`).

The sync fetches origin with the workspace's credential (deadline: two
minutes), creates the tags the hub lacks without ever moving or deleting
one it has, and then decides every branch of origin:

| Result | When | The hub's branch |
|---|---|---|
| `created` | the hub has no such branch | created at origin's head |
| `up_to_date` | both heads are equal | unchanged |
| `fast_forward` | only origin has new commits | moved to origin's head |
| `ahead` | only the hub has new commits | unchanged |
| `diverged` | both sides have commits the other lacks | unchanged |
| `reset` | the same, with `reset_to_origin=true` | moved to origin's head; the hub's commits leave the branch |
| `name_conflict` | a branch only the hub has blocks the name (`feat` against origin's `feat/x`) | not created |

A branch that only the hub has is not touched and not reported. A branch
that is the source of a change gets a new version of that change when the
sync moves it. A sync is safe to run at any time: it takes no lock, two
syncs at once both complete, and a branch that a push or a landing moves
during the sync keeps that writer's head. Once started, it runs to its end
even if you disconnect.

The answer is `200` with the workspace object plus `branches`, one entry
per branch of origin with `name`, `result`, `local_sha` (the hub's head
after the sync) and `origin_sha`:

```json
{"slug":"demo","status":"ready","…":"…","branches":[{"name":"dev","result":"diverged","local_sha":"9fceb02d…","origin_sha":"e83c5163…"},{"name":"main","result":"fast_forward","local_sha":"a3f2b1c4…","origin_sha":"a3f2b1c4…"}]}
```

A sync with `ahead` or `diverged` branches is still a `200`: read
`branches` to decide what to do. A completed sync sets `last_sync_at`,
`head_sha` and `origin_head_sha`, clears `sync_error`, and writes one
`parley.workspace.sync` event that lists the branches per result and every
ref it created or moved, with the head before and after.

A carry-patch workspace also fetches its upstream during a sync, leaves
its integration branch and patch branches out of `branches`, and adds its
own members to the answer; see
[Carry-patch workspaces](#carry-patch-workspaces).

| Error | When | What to do |
|---|---|---|
| `400 fork_has_no_origin` | the workspace is a task fork | sync its parent |
| `400 sync_disabled` | the setting `sync_enabled` is `false` | turn it on |
| `502 upstream_error` | origin could not be fetched, or the credential could not be read. Nothing in the hub's repository changed; `sync_error` holds the same message, with secrets removed | fix the credential or origin, sync again |
| `500 internal` | the sync stopped part-way, for example because a branch kept moving under it. Branches moved before stay moved | sync again |

### `POST /api/v1/workspaces/{slug}/branches/{name}/push`

Pushes the branch `{name}` of a `ready` repository workspace to the branch
of the same name on origin, with the workspace's origin credential. Use it
to publish landed work, or a branch you want to open a pull request from.
Percent-encode a `/` in the name (`fix%2F42`).

The body is optional: `{"force": true}` asks for a forced push, anything
else is a plain one.

- A plain push only creates the branch on origin or fast-forwards it. A
  protected branch is pushed like any other.
- A forced push replaces origin's branch only while it is where the hub
  last saw it (a lease), so it never discards commits the hub does not
  know. A protected branch is never force-pushed.

The answer is `200`:

```json
{"branch":"feature","sha":"a3f2b1c4d5e6f708192a3b4c5d6e7f8091a2b3c4","remote":"origin","remote_url":"https://example.com/a.git","result":"pushed"}
```

`result` is `pushed` (created or moved forward), `forced` (replaced by a
commit that does not descend from it) or `up_to_date`. A push that fails
changes neither origin's branch nor what the hub knows of it. Once handed
to origin, a push runs to its end (deadline: two minutes) even if you
disconnect, and writes one `parley.workspace.push` event with `ok` and
either the `result` or the `error_type`. Error messages have secrets and
URL credentials removed.

| Error | When | What to do |
|---|---|---|
| `400 fork_has_no_origin` | the workspace is a task fork | land the work, push from the parent |
| `403 protected_branch` | a forced push of a protected branch | |
| `403 integration_branch`, `403 patch_branch` | a branch a carry-patch workspace pushes itself | see [Carry-patch workspaces](#carry-patch-workspaces) |
| `404 branch_not_found` | the hub has no such branch | |
| `409 non_fast_forward` | origin's branch has commits the hub's lacks | sync, then push again; or push with `force` |
| `409 origin_moved` | a forced push, and origin's branch is not where the hub last saw it | sync, then push again |
| `422 origin_rejected` | origin refused the update by a branch rule or a hook; the message carries origin's reason | |
| `502 origin_unauthenticated` | origin wants a credential and the workspace has none, or origin rejected the stored one | store `GIT_PAT`, or `GIT_USERNAME` and `GIT_PASSWORD`, as workspace secrets |
| `502 origin_forbidden`, `origin_not_found` | the credential may not push to the repository, or cannot see it | |
| `502 origin_unreachable`, `upstream_error` | DNS, connection or TLS failed, or origin failed in another way | retry |
| `503 unavailable` | the patch list of a carry-patch workspace could not be read | retry |
| `504 origin_timeout` | the push did not finish within two minutes | retry |

### `POST /api/v1/workspaces/{slug}/archive`

Takes a `ready` or `failed` repository workspace out of service without
losing anything the hub holds. It takes no body.

Before it archives, the hub offers every branch of its copy to origin as a
fast-forward (a carry-patch workspace's integration branch excepted). This
safety copy is best effort: the archive goes on whatever origin answers,
and the event's `origin_push` says `ok`, `partial` or `failed`. Then the
status becomes `archived`, every active agent session of the workspace and
of its forks is ended, and its sandboxes are stopped. The hub's copy, the
forks and all data stay. An archived workspace still answers reads; git
fetches, pushes and writes are refused, and its forks become read-only.

The answer is the workspace object. The archive writes a
`parley.workspace.archive` event. A task fork cannot be archived
(`400 not_supported_on_fork`): delete it instead.

### `POST /api/v1/workspaces/{slug}/reactivate`

Brings an `archived` repository workspace back. It takes no body. The
status becomes `pending` and the clone work starts: when the copy the
archive kept is intact the workspace is `ready` again from it, with its
branches, without contacting origin; otherwise the hub clones origin
afresh. Poll the workspace to see it reach `ready`.

The answer is the workspace object in status `pending`, and a
`parley.workspace.reactivate` event is written. A workspace that is not
archived answers `409 workspace_not_archived`; a task fork
`400 not_supported_on_fork`.

### `POST /api/v1/workspaces/{slug}/reclone`

Throws away the hub's copy of a `ready` or `failed` repository workspace
and clones origin again. Reach for it when a clone failed, or when the
hub's copy is damaged. It takes no body.

The hub first makes the same safety copy as an archive, then sets the
status to `pending`, removes the copy and clones. The new copy holds what
origin holds and nothing else: a branch origin did not take in the safety
copy is lost, and so are the hub's own refs, the stored versions of changes
among them. The change records and all other data of the workspace stay;
a draft or open change whose source branch did not come back is abandoned
with the reason `source_lost`.

The answer is the workspace object in status `pending`, and a
`parley.workspace.reclone` event carries `origin_push`. A workspace with
task forks answers `409 has_forks`, because forks read through the
parent's copy: delete them first. A task fork answers
`400 fork_has_no_origin`.

### `DELETE /api/v1/workspaces/{slug}`

Deletes a repository workspace, its task forks and everything they own, in
one step or not at all. The workspace must be `archived` first
(`409 workspace_not_archived` otherwise), so a delete is always two
deliberate calls. For a task fork the route deletes just the fork, at
once; see [Lifetime and expiry](#lifetime-and-expiry).

What goes: the sandboxes of the workspace and its forks, the tokens bound
to them (revoked), their secrets and variables, their changes, their
remaining agent sessions (ended), their settings and records, and the
repositories on disk. What stays: the events already written, and origin.
The slug is free for a new workspace afterwards.

The answer is `204` with no body. One `parley.workspace.delete` event is
written per removed workspace, forks first; it lists the revoked token ids,
and the repository workspace's event lists the deleted forks. If the
delete fails (`500 internal`) or meets a fork created meanwhile
(`409 workspace_busy`), nothing has changed and you can repeat it.

## Task forks and workspace tokens

A task fork is a disposable workspace for one task, made from a repository
workspace, its **parent**. It exists so that every agent, and every person
without write access, gets a place to push to that cannot disturb anyone
else: you may fork any workspace you can read, you own the fork, and your
work reaches the parent only as a [change](#changes) that lands under the
parent's policy. Creating a fork takes the same short time for any size of
repository, and a fork cleans up after itself.

### What a fork shares and what is its own

A fork copies nothing. It reads the parent's history through the parent's
repository, so every commit of the parent, including ones that arrive
later, is available in the fork.

- **Tracking branches and tags.** The branches and tags the parent had
  when the fork was created appear in the fork under their own names,
  always at the parent's current value. They are read-only in the fork. A
  branch the parent deletes disappears from the fork; a branch the parent
  gains later does not appear.
- **Own branches.** Every new branch you push to the fork is the fork's
  own. This is where the task's work goes.
- **Base branch.** The parent branch the fork was made from is the fork's
  `default_branch` and the default target of its changes. `fork_point`
  records that branch and its head at creation; `head_sha` follows the
  parent's current head of it.
- **Settings, organization.** A fork has the parent's organization and
  shows the parent's settings; neither can be set on the fork.
- **Its own**: its slug (`<parent>--<name>`), owner, git remote
  (`hub_url`), bound tokens, secrets and variables, sandboxes, `purpose`,
  `expires_at` and `auto_change`.

A fork depends on its parent's state. While the parent is not `ready`, or
an archive, reclone or delete runs on it, the fork is not served
(`409 workspace_busy` or `workspace_not_ready`). While the parent is
archived, the fork is read-only: fetches and reads work, writes answer
`409 parent_archived`. A fork has no origin, so it cannot be synced,
pushed to origin, recloned, archived or forked again.

### Lifetime and expiry

A fork is `ready` from the moment it is created and lives until its
`expires_at`: the creation time plus `ttl`, or plus the hub's
`[forks].default_ttl` (7 days unless configured). The hub removes expired
forks in the background, within about a minute of the expiry. Until that
pass removes it, an expired fork still works, and you can give it a new
lifetime with `ttl` in
[`PATCH /api/v1/workspaces/{slug}`](#patch-apiv1workspacesslug), which
sets `expires_at` to now plus `ttl`. The same route changes a fork's
`purpose` (at most 1024 characters, `null` clears it) and `auto_change`.

While `[forks].expire_on_land` is on (the default), the hub also removes a
fork early once it has served its purpose: every one of its own branches
is the source of a change, all its changes are landed or abandoned, it has
been inactive for 24 hours, and it has no sandbox that is provisioning or
ready.

You can delete a fork at any time with
[`DELETE /api/v1/workspaces/{slug}`](#delete-apiv1workspacesslug). The
fork's creator, the parent's owner and the admin token may do so; the
parent may be `ready`, `archived` or `failed`, but not busy.

Removing a fork, by delete or by expiry, takes with it its sandboxes, the
tokens bound to it (revoked), its secrets and variables, its agent
sessions (ended) and its own branches. Its draft and open changes are
abandoned; landed work is in the parent and stays. The parent's branches
are never touched. A delete writes `parley.workspace.delete`; an expiry
writes `parley.fork.expire` instead, with `reason` `ttl` or `landed`. Both
list the revoked token ids, and readers of the parent's events see them.
The fork limits and lifetimes are set under `[forks]`; see
[Server and storage](configuration.md#server-and-storage).

### Workspace tokens

A workspace token is a PAT bound to one workspace: it works there and
nowhere else, whatever scopes it holds, and the binding never changes.
Give an agent a token bound to its fork and the most it can do is limited
to that fork (plus opening a change from it against the parent). A token
bound to a parent also reaches the forks that were created with it. A
bound token cannot mint or list tokens. The full rule is under
[Using the API](#using-the-api).

You get one in two ways: ask for it in the same call that creates the
fork (`token`), or mint one for any workspace you own. The secret is shown
once, in the answer that creates it. Tokens bound to a workspace are
revoked when the workspace is removed.

### Forking for a task: an example

Create a fork for a task with a token for the agent, valid as long as the
fork:

```sh
curl -sS -X POST https://hub.example/api/v1/workspaces/demo/forks \
  -H "Authorization: Bearer $PARLEY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"task1","ttl":"24h","purpose":"fix loader",
       "token":{"permissions":["git:write","changes:write"],"expires":"24h"}}'
```

```json
{"workspace":{"slug":"demo--task1","kind":"fork","status":"ready","default_branch":"main","parent_slug":"demo","fork_point":{"ref":"main","sha":"9fceb02d…"},"expires_at":"2026-11-15T10:30:15Z","auto_change":true,"purpose":"fix loader","…":"…"},
 "remote":"https://hub.example/git/acme/demo--task1.git",
 "token":{"token_id":"k3x9m2pq","workspace":"demo--task1","permissions":["git:write","changes:write"],"…":"…","token":"parley_pat_k3x9m2pq_…"}}
```

The agent clones `remote` with the token, pushes a new branch, and that
push opens a change against `demo`; see [Git over HTTP](#git-over-http).

### Fork and token routes

| Route | Scope | What it does |
|---|---|---|
| [`POST /api/v1/workspaces/{slug}/forks`](#post-apiv1workspacesslugforks) | `workspaces:write` | creates a task fork of `{slug}` |
| [`GET /api/v1/workspaces/{slug}/forks`](#get-apiv1workspacesslugforks) | `workspaces:read` | lists the forks of `{slug}` |
| [`POST /api/v1/workspaces/{slug}/tokens`](#post-apiv1workspacesslugtokens) | `tokens:manage` | mints a token bound to `{slug}` |
| [`GET /api/v1/workspaces/{slug}/tokens`](#get-apiv1workspacesslugtokens) | `tokens:read` | lists the tokens bound to `{slug}` |

A fork is read, updated and deleted through the
[workspace routes](#workspace-routes) with its own slug.

### `POST /api/v1/workspaces/{slug}/forks`

Creates a task fork of the repository workspace `{slug}`, owned by you. It
needs only read access to the parent, so an org member can fork what they
cannot write, and a PAT bound to the parent can fork it. The admin token
cannot own a fork (`403 forbidden`). The parent must be `ready`. The route
honours `Idempotency-Key`; a replay answers without the token's secret.

The body is optional; every field has a default.

| Field | Meaning |
|---|---|
| `name` | 1 to 32 characters of `a`–`z`, digits and hyphens, starting and ending with a letter or digit, no two hyphens in a row. The slug is `<parent>--<name>`. Default: 8 random characters |
| `ref` | the branch of the parent to base the fork on. Default: the parent's default branch |
| `ttl` | the lifetime: a whole number and `m`, `h` or `d` (`90m`, `24h`, `7d`), from `5m` to `[forks].max_ttl` (30 days unless configured). Default: `[forks].default_ttl` |
| `auto_change` | `true` (default): a push of a new branch to the fork opens a change against the parent without a push option; see [Opening and updating a change with a push](#opening-and-updating-a-change-with-a-push) |
| `purpose` | what the fork is for, at most 1024 characters. It is the fallback intent of changes opened from the fork |
| `session_id` | the agent session the fork belongs to: a session you created on the parent or one of its forks; see [Sessions and telemetry](#sessions-and-telemetry) |
| `token` | mints a token bound to the fork: `{name, permissions, paths, expires}`. `permissions` and `paths` are as for [`POST …/tokens`](#post-apiv1workspacesslugtokens); `name` defaults to `fork:<fork slug>`; `expires` is a duration like `ttl` and defaults to never. A PAT caller needs `tokens:manage` and cannot grant more than it holds |
| `sandbox` | creates a sandbox with the fork checked out; the object is the body of the sandbox create route, see [Sandboxes](#sandboxes) |

The fork, its token, its sandbox record and its `parley.workspace.fork`
event are created together or not at all: when the token or the sandbox is
refused, no fork is created, and the answer is that refusal.

The answer is `201` with `workspace` (the fork's
[workspace object](#the-workspace-object), `access` `owner`), `remote`
(its git URL, `null` when the hub has no external URL), and `token` and
`sandbox` when you asked for them. `token.token` is the secret; no other
answer carries it.

| Error | When | What to do |
|---|---|---|
| `400 invalid_request` | a field is refused; the message names it | fix the body |
| `400 unknown_session` | `session_id` is not a session you may link | |
| `400 not_supported_on_fork` | `{slug}` is itself a fork | fork its parent |
| `400 fork_ref_not_found` | `ref` is not a branch of the parent (a tag or a commit id is not one) | |
| `403 insufficient_scope` | also: `token` without `tokens:manage`, or a token beyond what the calling PAT holds | |
| `404 sandboxes_disabled` | `sandbox` on a hub without sandboxes | |
| `409 conflict` | the parent already has a fork of that name | choose another name |
| `409 workspace_busy`, `workspace_not_ready`, `workspace_archived` | the parent is not `ready` | |
| `429 fork_limit_reached` | the parent has `[forks].max_per_workspace` forks (200 unless configured) | delete forks, or wait for expiry |

### `GET /api/v1/workspaces/{slug}/forks`

Lists the task forks of `{slug}` that you can read, newest first, as a
[list](#using-the-api) of workspace objects. The parent's owner has
`owner` access on every fork; other org members see them as `member`. A
PAT bound to the parent lists the forks it created.

| Parameter | Meaning |
|---|---|
| `owner` | `me`: only the forks you own |
| `session_id` | only the forks created in that session |

### `POST /api/v1/workspaces/{slug}/tokens`

Mints a PAT owned by you and bound to the workspace `{slug}`, repository
or fork. You must own the workspace; the admin token and bound PATs cannot
use the route. It honours `Idempotency-Key`; a replay answers without the
secret.

The body is that of the account's token route, see
[Signing in and your account](#signing-in-and-your-account): `name`,
`permissions`, `paths` and `expires`. A PAT caller cannot grant a scope or
a path it does not hold itself.

```json
{"name":"agent","permissions":["git:read"],"expires":30}
```

The answer is `201` with the token object, `workspace` set to the slug and
the secret in `token`, shown this once. A `parley.token.bind` event records
the token's id, scopes, paths and expiry, never its value. An archived
workspace answers `409 workspace_archived`; a bound PAT `403 token_bound`.

### `GET /api/v1/workspaces/{slug}/tokens`

Lists every token bound to `{slug}`, live, revoked or expired, oldest
first, so that the owner sees everything that can reach the workspace or
once could. It is a [list](#using-the-api) of token objects without their
secrets. It needs owner access (the admin token has it everywhere) and is
closed to bound PATs.

## Git over HTTP

Every workspace and every task fork is an ordinary git remote. You clone,
fetch and push with stock `git` over HTTPS, on the same port as the API,
with the same credentials and the same access rule. This is the path most
work takes into the hub: an agent clones its fork, commits locally and
pushes, and the push itself opens or updates the change.

The hub is not a plain git host, though. It decides every ref update of a
push by a fixed set of rules, so that protected branches move only by
landing, and it turns accepted pushes into changes and change versions.
Only smart HTTP is offered; there is no SSH transport.

### Cloning and pushing

The remote of a workspace is its `hub_url`:

```text
<external URL of the hub>/git/<org slug>/<workspace slug>.git
```

git authenticates with HTTP Basic. The **password** is your credential: an
API key, a PAT or the admin token. The user name is ignored, so any name
works. The simplest set-up is the credential helper of the `parley`
client, which hands git the credential you are logged in with, for the
hub's URL only:

```sh
git config --global credential.https://hub.example.helper '!parley credential-helper'

git clone https://hub.example/git/acme/demo--task1.git
cd demo--task1
git switch -c fix-loader
# edit, commit
git push -o parley.title="Fix the loader" origin fix-loader
```

```text
remote: change demo#12 version 1 (opened)
```

Without the helper, git prompts for a user name and password; enter
anything as the name and the credential as the password.

A PAT needs `git:read` to fetch and `git:write` to push (`git:write`
includes `git:read`); API keys and the admin token carry no scopes.
Fetching needs read access to the workspace, pushing write access. An org
member who only reads a workspace cannot push to it: fork it and push to
the fork.

| Route | What it is |
|---|---|
| `GET /git/{org}/{slug}.git/info/refs?service=…` | ref discovery, for `git-upload-pack` or `git-receive-pack` |
| `POST /git/{org}/{slug}.git/git-upload-pack` | fetch |
| `POST /git/{org}/{slug}.git/git-receive-pack` | push |

These routes are outside `/api/v1` and follow git's protocol, not the
API's conventions: no JSON, no error envelope. Requests count against the
same rate limit as your API requests.

### What a fetch offers

Everything stock git asks for works, under protocol versions 0 and 2:
full and incremental fetches, shallow history (`--depth`, `--deepen`,
`--shallow-since`, `--shallow-exclude`, `--unshallow`), and partial clones
with the filters `blob:none`, `blob:limit=<n>` and `tree:<depth>`. Other
filters are refused.

A default clone takes branches and tags. The hub's own refs under
`refs/parley/` are advertised too and can be fetched by name; the stored
versions of a change are at `refs/parley/changes/<number>/<version>`:

```sh
git fetch origin refs/parley/changes/12/1
```

`HEAD` names the default branch, for a fork its base branch. A fork is
served with its own branches, its tracking branches and tags at the
parent's current values, and the version refs of the changes opened from
it; see
[What a fork shares and what is its own](#what-a-fork-shares-and-what-is-its-own).

With the setting `audit_fetches` on, each fetch that sends a pack writes a
`parley.git.fetch` event that says who fetched and whether the fetch was
shallow or filtered.

### What a push may do

A push moves all of its refs or none. Each ref update is judged by these
rules, and one refused ref refuses the push:

| Rule | Why |
|---|---|
| Nothing under `refs/parley/` can be pushed | the hub maintains these refs: they are the record of changes |
| A protected branch cannot be created, force-pushed or deleted by a push, and cannot be advanced by anyone but the owner | work reaches a protected branch by [landing](#landing), where the policy is applied. Push another branch and open a change |
| The default branch cannot be deleted | |
| A fork's tracking branches are read-only | they mirror the parent. Push a new branch |
| A carry-patch workspace controls its integration branch and patch branches | see [Carry-patch workspaces](#carry-patch-workspaces) |
| A PAT limited to paths can only push commits whose changed files are all inside its paths | this bounds an agent's write access to part of a repository |

Protected branches are those matching the workspace's
`protected_branches` setting, by default the default branch. Unprotected
branches are yours to create, force-push after a rebase, and delete.

**Direct pushes by the owner.** While the setting `protect.owner_push` is
`true` (the default), the workspace's owner and the admin token may
fast-forward a protected branch by pushing to it, with an API key, the
admin token or a PAT that is neither bound to a workspace nor limited to
paths. A bound or path-scoped token belongs to an agent and must go
through a change, even when it is the owner's. Set `protect.owner_push`
to `false` to make landing the only way.

Two pushes that race for one ref never both win: refs move by
compare-and-swap against the value git read, and the loser is told to
fetch and try again. A refused push leaves no ref moved and none of its
objects in the repository.

An accepted push writes one `parley.git.push` event per ref, in the order
pushed and next to each other in the event log. Each says which ref moved,
from what to what, whether it was created, deleted or forced, whether it
was a direct push to a protected branch, and which session pushed. These
events survive a hub that stops right after the refs moved: they are
written when it next starts.

### Opening and updating a change with a push

A push to a branch that is already the source of a draft or open change
records the next version of that change. You need no option for this, and
it is part of the push: the version is stored together with the ref move.
Deleting such a branch abandons its change.

A push of a branch that has no change opens one when you ask for it with
a push option, or without any option in a task fork whose `auto_change`
is `true`. The change is opened in the pushed workspace, or for a fork in
its parent, from the pushed branch.

| Push option | Meaning |
|---|---|
| `parley.change` | open a change for each pushed branch that has none |
| `parley.target=<branch>` | the target branch; implies `parley.change`. Default: the default branch of the pushed workspace, for a fork its base branch |
| `parley.title=<text>` | the title, at most 200 characters. Default: the subject of the head commit |
| `parley.intent=<text>` | the intent. Default: the body of the head commit, then the fork's `purpose` |
| `parley.draft` | open the change as a draft |
| `parley.session=<id>` | the agent session the push, its events and its change version belong to; see [Sessions and telemetry](#sessions-and-telemetry) |

```sh
git push -o parley.change -o parley.target=release -o parley.draft origin fix-loader
```

The hub tells you the outcome per pushed branch, on lines git prints with
the prefix `remote:`:

```text
change demo#12 version 1 (opened)
change demo#12 version 1 (opened, draft)
change demo#12 version 2
warning: no change for fix-loader: <reason>
```

The change is named by the workspace it belongs to, a fork's parent for a
push to a fork. A push is never failed because its change could not be
opened: the refs have moved, and the warning says why there is no change.
The reasons end with the `error_type` of the refusal in parentheses where
there is one, so that a script can match it:

| Reason | What to do |
|---|---|
| the target branch does not exist (`target_not_found`) | name an existing branch with `parley.target` |
| no commits ahead of the target (`nothing_to_land`) | |
| merge commits are not supported (`merge_commits_unsupported`) | rebase the branch |
| intent is required (`intent_required`) | push again with `-o parley.intent=<text>` or `-o parley.draft`, or write a commit body |
| token lacks scope `changes:write` | a PAT needs it to open a change |
| branch is its own target | push another branch |
| branch is protected and was pushed directly | a direct push by the owner opens no change |
| internal error | open the change through the API; see [Changes](#changes) |

After a warning, open the change with the changes route, or fix the cause
and push a new commit.

### Limits

| Limit | Value |
|---|---|
| Push options per push | 16, counting every option, not only the `parley.` ones |
| Length of one push option, name and value | 1024 bytes |
| `parley.title` | 200 characters |
| Pack size of a push | `[git].max_push_size`; no limit unless configured |
| Duration of a fetch or a push | `[git].timeout`; a push that has started to move its refs is finished |
| Request size of a fetch (protocol version 2 body, or the wants of version 0) | 16 MiB |
| Push from a shallow clone | accepted unless the pack itself brings a boundary commit whose history the hub lacks; run `git fetch --unshallow` and push again |

An invalid push option refuses the whole push before its pack is read: an
unknown `parley.` option, an option given twice, a value on `parley.change`
or `parley.draft`, an empty value on the others, a `parley.target` that is
not a valid branch name, or a `parley.session` the hub does not accept for
you. Options without the `parley.` prefix are ignored. The `[git]` keys
are described under
[Server and storage](configuration.md#server-and-storage).

### How refusals reach git

A request refused before the git exchange starts gets an HTTP status and
one line of plain text, which git shows as the failure of the command:

| Status | When | What to do |
|---|---|---|
| `401` | no credential, or one that is unknown, revoked or expired. git asks for a credential again | check the credential |
| `403` | the user is blocked; the PAT lacks `git:read` or `git:write`; or an org member pushes to a workspace they only read | for the last: create a task fork |
| `400` | ref discovery without a valid `service` parameter; stock git never sends this | |
| `404` `repository not found` | no such repository, you cannot read it, the org segment is wrong (it is case-sensitive), the `.git` suffix is missing, or a bound PAT is used outside its binding | take the URL from the workspace's `hub_url` |
| `409` `repository not available` | the workspace is not `ready`, or busy with an archive, reclone or delete; for a fork, the same for its parent | wait, or reclone a failed workspace |
| `409` `repository is read-only` | a push to a fork whose parent is archived | |
| `429` | rate limit, with `Retry-After` | retry later |

Once the exchange has started, the answer is `200` and the refusal comes
in git's own protocol:

- **Fetch.** git fails with the hub's message, for example
  `not our ref <id>` for an object no ref reaches, `filter '<kind>' not
  supported`, `request too large` or `request timed out`.
- **Push.** git prints `! [remote rejected] <ref> (<reason>)` for every
  ref, and `remote: error: <ref>: <reason>` for each ref that broke a
  rule; the other refs of the same push are reported as not updated
  because another ref was refused. The reason names the rule, such as a
  protected branch, a tracking branch, a path outside the token's paths,
  an invalid push option, a pack that is invalid or over the size limit, a
  timeout, or a ref that changed since it was read (fetch and push again).
  `internal error; try again` means the hub could not decide; nothing
  moved, and the push is safe to repeat.

Every request on the three routes is counted in the metric
`parley_git_requests_total`, labelled with the service (`info-refs`,
`upload-pack`, `receive-pack`) and the result (`ok`, `refused`, `error`);
see [Health, version and metrics](#health-version-and-metrics).

## Carry-patch workspaces

A carry-patch workspace is for a team that maintains its own variant of a
project it does not control: a fork with a set of changes that must stay
on top of an upstream that keeps moving. Doing this by hand means rebasing
every change each time upstream moves, resolving the same conflicts again
and again, and hoping the deployed branch still holds every change. The
hub does that work instead: you tell it which branches are your patches
and in which order, and it rebuilds the branch you deploy whenever
upstream or a patch changes.

### How carry-patch workspaces work

A workspace created with `workspace_mode` `carry_patch` (see
[Workspaces](#workspaces)) follows two remotes: **origin** (`git_url`,
your copy of the repository) and **upstream** (`upstream_url`, the project
you follow). On top of that it has three things.

- **Patches.** A patch is a branch of the workspace that you register
  with the hub. Patches have an order, `position` 1 to N. You work on a
  patch branch like on any branch: push to it, or land changes onto it.
- **The integration branch.** The branch you build and deploy from, named
  by the workspace's `integration_branch` (`deploy` when you name none).
  It is always "the upstream base plus every patch that applies, in
  position order". Only the hub writes it. A push to it, by anyone, is
  refused with `integration_branch`, and no change can target it.
- **Rebuilds.** A rebuild recomputes the integration branch. The **upstream
  base** it starts from is the head of the upstream's default branch, or
  of the upstream branch named like the workspace's `default_branch` when
  upstream has no default.

After the first clone (and after every reclone) the hub fetches upstream
and creates the integration branch at the upstream base, before the
workspace becomes `ready`. If upstream cannot be fetched then, the
workspace still becomes `ready`, with the integration branch at origin's
default branch head; the next sync or rebuild fetches upstream again.

The hub reads the upstream credential from the workspace's secrets (see
[Secrets and variables](#secrets-and-variables)) at every fetch, taking
the first that is set: `UPSTREAM_GIT_PAT`; `UPSTREAM_GIT_USERNAME` with
`UPSTREAM_GIT_PASSWORD`; the workspace's origin credential; none. A
rotated secret counts at the next fetch.

#### A rebuild is a durable job

A rebuild runs as a job inside the hub. The job is stored before the
request that asked for it is answered, survives a restart of the hub, and
is retried when it fails for a reason that may pass.

| Status | Meaning | Final |
|---|---|---|
| `queued` | waiting to run | no |
| `running` | an attempt is running | no |
| `failed` | the last attempt failed with an error that may pass; the job runs again at `next_attempt_at` | no |
| `completed` | the integration branch was published | yes |
| `dead_letter` | failed for good: a patch conflict in `fail_fast` mode, a workspace that is archived or failed, or retries spent | yes, until [requeued](#post-apiv1workspacesslugrebuildsidrequeue) |
| `cancelled` | cancelled while it was `queued` | yes |

A workspace has at most one job that is `queued`, `running` or `failed`.
Across the hub at most 4 jobs run at once; queued jobs start oldest first,
within about 5 seconds when a slot is free. A retryable failure (upstream
unreachable, a git error, a git operation longer than 10 minutes, the hub
stopping, the workspace still cloning) is retried after 4 s, 8 s, 16 s and
so on, doubling up to 2 hours; the 21st such failure makes the job
`dead_letter`. A job that was `running` when the hub stopped is queued
again at the next start and runs from the beginning.

An attempt works out of sight: it fetches upstream, reads the tip of every
patch branch once, applies the patches in position order in a scratch
area, and moves the integration branch once, at the end. Until then no
client sees a half-built branch, and a push to a patch branch during the
run is accepted and does not change what the run builds. With the strategy
`rebase` (the default) the commits of a patch that upstream lacks are
replayed oldest first, keeping author, date and message; merge commits are
left out and a commit whose change is already present is dropped. With
`merge` the patch tip is merged with a merge commit. A job captures its
strategy, fail mode and integration branch when it is queued; later
changes of the settings do not affect it.

When the publish succeeds, in the same step the hub removes every patch
that was `merged_upstream` (a soft delete you can
[restore](#post-apiv1workspacesslugpatchesidrestore)) and returns every
`conflict` patch that now applied to `active`. With the setting
`rebuild_push_integration_branch` on, it then pushes the integration
branch to origin; a failed push writes `parley.rebuild.push_failed` and
does not fail the rebuild.

#### What triggers a rebuild

| Trigger | When | `submitted_by` |
|---|---|---|
| You ask | [`POST …/rebuild`](#post-apiv1workspacesslugrebuild) | your user id, or `admin_token` |
| A patch branch moves | an accepted git push, source control API write or land that created or moved a patch branch, with `auto_rebuild_after_push` on | `system:push`, `system:land` |
| A sync | upstream advanced, the branch of an `active` or `conflict` patch was created or moved from origin, or a patch was found merged upstream, with `auto_rebuild_after_sync` on | the caller of the sync |
| A reset to origin | the reset moved the branch of an `active` or `conflict` patch, with `auto_rebuild_after_sync` on | the caller |
| A follow-up | a rebuild finds, when it ends, that a patch branch or upstream moved while it ran | `system:followup` |

Both `auto_rebuild_…` settings default to `true`. An automatic trigger
that finds a rebuild already `queued`, `running` or `failed` queues
nothing; the follow-up check covers what moved in the meantime. Changing a
patch's status or position, deleting, restoring and reordering do not
queue a rebuild: ask for one when you are done. The hub does not poll
upstream; it learns that upstream moved when you
[sync](#workspaces) the workspace or a rebuild fetches it.

#### The states of a patch

| Status | Meaning | In a rebuild |
|---|---|---|
| `active` | applies, or has not been tried yet | applied |
| `conflict` | the last rebuild that tried it could not apply it; `conflict_files` lists the paths | tried again by every rebuild |
| `disabled` | you switched it off | skipped |
| `merged_upstream` | upstream now holds the change | skipped, then removed when the rebuild publishes |
| `deleted` | soft-deleted; has no position; removed for good after 7 days | not listed |

| From | To | What moves it |
|---|---|---|
| any but `deleted` | `active`, `disabled`, `merged_upstream`, `conflict` | [`PATCH …/patches/{id}`](#patch-apiv1workspacesslugpatchesid) with `status` |
| `active`, `conflict` | `conflict` | a rebuild could not apply it |
| `conflict` | `active` | a rebuild applied it and published |
| `active`, `conflict` | `merged_upstream` | a sync detected the patch in upstream (setting `squash_merge_detection`) |
| any but `deleted` | `deleted` | [`DELETE …/patches/{id}`](#delete-apiv1workspacesslugpatchesid), or a rebuild that publishes while the patch is `merged_upstream` |
| `deleted` | `active` | [restore](#post-apiv1workspacesslugpatchesidrestore), at the end of the order |

A patch whose branch does not exist (for example after you deleted the
branch) stays registered and is skipped until the branch returns.

#### When a patch conflicts

When a commit of a patch does not apply, the hub first replays the
[resolutions it remembers](#how-the-hub-remembers-a-resolution). If paths
are still in conflict, the patch is undone, becomes `conflict`, and the
workspace's `rebuild_fail_mode` (or the `fail_mode` of the request)
decides the rest:

- `fail_fast` (the default): the rebuild stops and publishes nothing. The
  integration branch stays as it was. The job is `dead_letter` with a
  conflict report in `conflict`; it is not retried, because a retry would
  meet the same conflict.
- `continue`: the rebuild goes on with the next patch, from the state
  before the conflicting one, and publishes the integration branch without
  it. The job is `completed` with `patches_conflicted` above 0.

The conflict report has the shape a landing conflict has (see
[Landing](#landing)), plus `patch_id`: `target_sha` (the head being built
before the patch), `head_sha` (the patch tip), `base_sha` (their merge
base), `commit_sha` (the first commit that did not apply; with `merge`,
the tip) and `files`, one entry per conflicted path with `path`, `kind`
and the three versions `base`, `ours` (the head being built) and `theirs`
(the commit).

To resolve a conflict, fix the patch branch and let the hub rebuild:

1. Fetch upstream and the patch branch into your clone.
2. Either rebase the patch branch onto the new upstream head and
   force-push it, or merge the upstream head into the patch branch,
   resolve the conflict in the merge commit, and push.
3. The push queues a rebuild (with `auto_rebuild_after_push` on); else
   [queue one](#post-apiv1workspacesslugrebuild) or
   [requeue](#post-apiv1workspacesslugrebuildsidrequeue) the dead-lettered
   job. When the patch applies and the rebuild publishes, the patch is
   `active` again.

#### How the hub remembers a resolution

The hub keeps, per workspace, a record of every conflict a rebuild met and
of how it was resolved (git's rerere). A rebuild that meets a conflict
records it as unresolved. The resolution is learned from your merge
commits: when a patch conflicts, the rebuild looks at the newest 50 merge
commits on the patch branch that upstream lacks, and where one of them
resolved a conflict, records the merge commit's content as the resolution
and tries the patch once more. From then on every rebuild that meets the
same conflict replays the resolution without asking you. So a resolution
made by merging upstream into the patch branch is made once; with a
rebase and force-push there is nothing to remember, since the patch then
applies cleanly.

[`GET …/rerere`](#get-apiv1workspacesslugrerere) lists what is remembered
and [`DELETE …/rerere/{pathspec}`](#delete-apiv1workspacesslugrererepathspec)
forgets a wrong resolution. A fresh clone and a reclone start with none.

#### Rolling back and previewing

Every completed rebuild records the head the integration branch had before
it. [Rollback](#post-apiv1workspacesslugrebuildsidrollback) moves the
branch back to that head and changes nothing else. The next rebuild that
publishes replaces the rolled-back head, so disable the patch at fault, or
turn the automatic triggers off, before the next push or sync.

[Preview](#get-apiv1workspacesslugrebuild-preview) predicts, without
changing anything and without contacting a remote, which patches would
conflict with the upstream base as last fetched.

#### Where patch branches live

Two workspace settings decide who may write a patch branch and how it
reaches origin. They are read at every push.

| `patch_branch_source` | `push_patches_to_origin` | A push to a patch branch on the hub |
|---|---|---|
| `hub` (default) | `false` (default) | accepted, forced updates and deletes included; origin is not told |
| `hub` | `true` | accepted; afterwards the hub mirrors the branch to origin. A mirror that fails writes `parley.patch.mirror_failed` and does not undo the push |
| `origin` | `false` | refused with `patch_branch`: push to origin and sync |
| `origin` | `true` | the hub forwards the update to origin first, without force, and accepts it only when origin does; a delete is refused |

With `origin`, every sync refreshes the patch branches from origin and
stores the outcome on the patch (`origin_sync_state`: `in_sync`,
`diverged` or `missing_on_origin`; `origin_sha`; `origin_synced_at`). The
setting `patch_divergence_policy` says what a sync does with a branch
that diverged: `replace` (the default) takes origin's tip and saves the
hub's as a backup, shown as `replaced_sha` on the patch; `report` leaves
the branch and marks it `diverged`. Over git a refusal fails the whole
push; over the source control API it is `403` with the error type
`integration_branch` or `patch_branch`. A land onto a patch branch is
judged by the same rules before its target moves. The branch of a
`deleted` patch is an ordinary branch.

#### Events

Every action here writes an event (see [Audit and events](#audit-and-events)).
Patch events have the patch as resource: `parley.patch.create`, `.update`
(with `changed`, and for a status change `from`, `to` and a `reason` of
`api`, `rebuild_conflict`, `rebuild_clean` or `merged_upstream`),
`.delete`, `.restore`, `.reorder`, `.reset`, `.replace`, `.sync` and
`.mirror_failed`. Rebuild events have the job as resource:
`parley.rebuild.enqueue`, `.complete` (both heads of the integration
branch and the four counts), `.fail` (`reason`, `will_retry`,
`retry_count` and the conflict report), `.followup`, `.push_failed`,
`.cancel`, `.requeue` and `.rollback`. To react to conflicts, subscribe to
`parley.rebuild.fail` and act when `reason` is `conflict`. A rebuild that
moves the integration branch writes no `parley.git.push`.

### Example: from set-up to a rebuild after upstream moves

Create the workspace. `upstream_url` is required in this mode.

```sh
curl -X POST "$PARLEY_URL/api/v1/workspaces" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"slug":"variant","git_url":"https://git.example.com/acme/variant.git",
       "workspace_mode":"carry_patch",
       "upstream_url":"https://git.example.com/upstream/project.git",
       "integration_branch":"deploy"}'
```

If upstream needs its own credential, store it as a workspace secret:

```sh
curl -X POST "$PARLEY_URL/api/v1/workspaces/variant/secrets" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"entries":[{"key":"UPSTREAM_GIT_PAT","value":"<token>"}]}'
```

Once the workspace is `ready`, push your change as a branch to the hub
(see [Git over HTTP](#git-over-http)) and register the branch as a patch:

```sh
git push hub fix-dial

curl -X POST "$PARLEY_URL/api/v1/workspaces/variant/patches" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"branch_name":"fix-dial","description":"Longer dial timeout"}'
```

```json
{"id":"5c0e0000-0000-4000-8000-000000000001","workspace_slug":"variant",
 "branch_name":"fix-dial","position":1,"status":"active","conflict_files":[],
 "upstream_pr_url":null,"description":"Longer dial timeout",
 "added_at":"2026-11-16T10:30:15Z","updated_at":"2026-11-16T10:30:15Z",
 "deleted_at":null,"origin_sync_state":null,"origin_sha":null,
 "origin_synced_at":null}
```

Registering does not rebuild. Queue the first rebuild and read the job
until it is final:

```sh
curl -X POST "$PARLEY_URL/api/v1/workspaces/variant/rebuild" \
  -H "Authorization: Bearer $TOKEN"

curl "$PARLEY_URL/api/v1/workspaces/variant/rebuilds/7b1c0e2a-4d5f-4a6b-9c8d-0e1f2a3b4c5d" \
  -H "Authorization: Bearer $TOKEN"
```

The `POST` answers `202` with the job, `status` `queued`. When the job is
`completed`, `deploy` is the upstream base plus `fix-dial`. From now on a
push to `fix-dial` queues a rebuild by itself.

Later, upstream moves. Sync the workspace: the sync fetches upstream and,
since upstream advanced, queues a rebuild. Then look at where things
stand:

```sh
curl -X POST "$PARLEY_URL/api/v1/workspaces/variant/sync" \
  -H "Authorization: Bearer $TOKEN"

curl "$PARLEY_URL/api/v1/workspaces/variant/patch-status" \
  -H "Authorization: Bearer $TOKEN"
```

If the rebuild meets a conflict in
`fix-dial`, `patch-status` shows the patch as `conflict` with its
`conflict_files` and `last_rebuild` as `dead_letter`; `deploy` is
unchanged. Merge upstream into the patch, resolve, and push:

```sh
git fetch upstream && git checkout fix-dial
git merge upstream/main        # resolve the conflict, then commit
git push hub fix-dial
```

Here `hub` and `upstream` are the remotes of your clone for the hub and
for upstream, and `main` is upstream's default branch. The push queues a
new rebuild. It learns the resolution from your merge commit, applies the
patch, publishes `deploy` and returns the patch to `active`.

To see what a rebuild would do before it runs (for example with
`auto_rebuild_after_sync` off, after a sync), ask for a preview:

```sh
curl "$PARLEY_URL/api/v1/workspaces/variant/rebuild-preview" \
  -H "Authorization: Bearer $TOKEN"
```

If the new `deploy` turns out to be bad, roll it back:

```sh
curl -X POST "$PARLEY_URL/api/v1/workspaces/variant/rebuilds/<job id>/rollback" \
  -H "Authorization: Bearer $TOKEN"
```

### Carry-patch routes

Authentication, scopes, the order of checks, the error envelope, lists and
`Idempotency-Key` are described in [Using the API](#using-the-api). Rules
that hold for every route below:

- Reads need read access to the workspace, writes need write access.
  `patches:write` also satisfies `patches:read`.
- A workspace that is not in mode `carry_patch`, every task fork included,
  is refused with `400` `workspace_mode_mismatch`.
- Reads answer in every status of the workspace. Writes, and the preview,
  need the workspace `ready` with no archive, reclone or delete running:
  otherwise `409` `workspace_busy`, `workspace_not_ready` or
  `workspace_archived`.
- An `{id}` that names no patch (or job) of this workspace is `404`
  `not_found`.
- A refused write changes nothing and writes no event.

| Route | Scope | What it does |
|---|---|---|
| [`POST /api/v1/workspaces/{slug}/patches`](#post-apiv1workspacesslugpatches) | `patches:write` | Registers one patch or a batch |
| [`GET /api/v1/workspaces/{slug}/patches`](#get-apiv1workspacesslugpatches) | `patches:read` | Lists the patches in position order |
| [`GET /api/v1/workspaces/{slug}/patches/{id}`](#get-apiv1workspacesslugpatchesid) | `patches:read` | Reads one patch |
| [`PATCH /api/v1/workspaces/{slug}/patches/{id}`](#patch-apiv1workspacesslugpatchesid) | `patches:write` | Changes status, position, description, PR URL |
| [`DELETE /api/v1/workspaces/{slug}/patches/{id}`](#delete-apiv1workspacesslugpatchesid) | `patches:write` | Soft-deletes a patch |
| [`POST /api/v1/workspaces/{slug}/patches/{id}/restore`](#post-apiv1workspacesslugpatchesidrestore) | `patches:write` | Brings a deleted patch back |
| [`POST /api/v1/workspaces/{slug}/patches/reorder`](#post-apiv1workspacesslugpatchesreorder) | `patches:write` | Sets the whole order |
| [`POST /api/v1/workspaces/{slug}/patches/{id}/reset-to-origin`](#post-apiv1workspacesslugpatchesidreset-to-origin) | `patches:write` | Forces a patch branch to origin's tip |
| [`POST /api/v1/workspaces/{slug}/rebuild`](#post-apiv1workspacesslugrebuild) | `rebuilds:write` | Queues a rebuild |
| [`GET /api/v1/workspaces/{slug}/rebuilds`](#get-apiv1workspacesslugrebuilds-and-get-rebuildsid) | `rebuilds:read` | Lists the rebuild jobs, newest first |
| [`GET /api/v1/workspaces/{slug}/rebuilds/{id}`](#get-apiv1workspacesslugrebuilds-and-get-rebuildsid) | `rebuilds:read` | Reads one job, with its progress |
| [`DELETE /api/v1/workspaces/{slug}/rebuilds/{id}`](#delete-apiv1workspacesslugrebuildsid) | `rebuilds:write` | Cancels a queued job |
| [`POST /api/v1/workspaces/{slug}/rebuilds/{id}/requeue`](#post-apiv1workspacesslugrebuildsidrequeue) | `rebuilds:write` | Queues a dead-lettered job again |
| [`POST /api/v1/workspaces/{slug}/rebuilds/{id}/rollback`](#post-apiv1workspacesslugrebuildsidrollback) | `rebuilds:write` | Moves the integration branch back |
| [`GET /api/v1/workspaces/{slug}/rebuild-preview`](#get-apiv1workspacesslugrebuild-preview) | `rebuilds:read` | Predicts the outcome per patch |
| [`GET /api/v1/workspaces/{slug}/rerere`](#get-apiv1workspacesslugrerere) | `workspaces:read` | Lists the remembered resolutions |
| [`DELETE /api/v1/workspaces/{slug}/rerere/{pathspec}`](#delete-apiv1workspacesslugrererepathspec) | `workspaces:write` | Forgets remembered resolutions |
| [`GET /api/v1/workspaces/{slug}/patch-status`](#get-apiv1workspacesslugpatch-status) | `workspaces:read` | Summarises the workspace in one document |

### The patch object

Every patch route answers patches in this shape, every member present.

| Member | Value |
|---|---|
| `id` | a UUID |
| `workspace_slug` | the workspace |
| `branch_name` | the patch branch, without `refs/heads/` |
| `position` | 1 to N without gaps among the patches that are not deleted; `null` for a deleted patch |
| `status` | see [the states of a patch](#the-states-of-a-patch) |
| `conflict_files` | paths; `[]` unless `status` is `conflict` |
| `upstream_pr_url`, `description` | strings or `null` |
| `added_at`, `updated_at`, `deleted_at` | timestamps; `deleted_at` is `null` unless deleted |
| `origin_sync_state`, `origin_sha`, `origin_synced_at` | the outcome of the last refresh from origin by a sync or a reset; `null` for a new patch and with `patch_branch_source` `hub` |
| `replaced_sha` | only on the read of one patch, the update and the reset: the tip the hub saved when it last replaced the branch with origin's; absent when there is no backup |

A branch is the branch of at most one patch that is not deleted. Writes
that change positions never fail because of each other; afterwards the
positions are 1 to N without a gap.

### `POST /api/v1/workspaces/{slug}/patches`

Registers a branch as a patch. The body is one object, or a non-empty
array of them that is registered all or nothing. Accepts an
`Idempotency-Key`.

| Member | Required | Meaning |
|---|---|---|
| `branch_name` | yes | a valid branch name, not the integration branch |
| `position` | no | an integer of at least 1; the patches at and after it move down. Absent or beyond the end, the patch is appended |
| `upstream_pr_url`, `description` | no | strings |
| `skip_branch_check` | no | `true` registers the branch without looking for it |
| `if_not_exists` | no | `true` answers an already registered branch with its patch (`200`); not allowed in a batch |

The hub looks for the branch in its own copy, then in what it last fetched
from origin, and, with `patch_branch_source` `origin`, fetches that one
branch from origin. A branch it finds on origin is created on the hub. In
a batch, elements with a `position` are placed first, in array order, then
the rest are appended.

The answer is `201` with the patch, `active`; a batch answers
`{"items":[…]}` in request order. Registering a branch whose earlier patch
is `deleted` removes that old patch for good.

| Error | When |
|---|---|
| `400` `invalid_request` | a bad body; in a batch the message starts with `patch[<i>]:`, the index of the element |
| `404` `branch_not_found` | the branch is found nowhere; push it first, or pass `skip_branch_check` |
| `409` `patch_exists` | the branch is a patch already; use `if_not_exists` to make the call repeatable |
| `409` `conflict` | another branch is in the way of the name, such as `feat` for `feat/x` |
| `502` `origin_fetch_failed` | the fetch of the branch from origin failed; try again |

### `GET /api/v1/workspaces/{slug}/patches`

A [list](#using-the-api) of the patches that are not deleted, in position
order. With `include_deleted=true` the deleted patches follow, most
recently deleted first. `include_deleted` must be exactly `true` or
`false`.

### `GET /api/v1/workspaces/{slug}/patches/{id}`

One patch, deleted or not, with `replaced_sha` when the hub holds a backup
of a replaced tip.

### `PATCH /api/v1/workspaces/{slug}/patches/{id}`

Changes a patch. Each member is optional; an absent or `null` member
leaves its field alone, and a body that changes nothing writes nothing.

| Member | Meaning |
|---|---|
| `status` | `active`, `disabled`, `merged_upstream` or `conflict`; any status but `conflict` clears `conflict_files` |
| `position` | 1 to N; the patch moves there and the others keep their relative order |
| `description`, `upstream_pr_url` | strings; `""` clears the field |

Use `disabled` to take a patch out of the integration branch without
losing it, and `upstream_pr_url` to let a sync recognise the patch once
upstream merges the pull request. The answer is `200` with the patch. No
update queues a rebuild.

| Error | When |
|---|---|
| `400` `invalid_request` | a bad member, `status` `deleted` included, or a `position` beyond N |
| `409` `patch_deleted` | the patch is deleted; restore it first |

### `DELETE /api/v1/workspaces/{slug}/patches/{id}`

Soft-deletes a patch: it becomes `deleted`, loses its position, and the
others close the gap. The answer is `204`; deleting a deleted patch is
`204` too. The patch branch is not touched. The patch stays readable (and
restorable) for 7 days, after which the hub removes it for good, together
with the backup of a replaced tip; the branch stays.

### `POST /api/v1/workspaces/{slug}/patches/{id}/restore`

Brings a deleted patch back as `active`, at the end of the order, whether
you deleted it or a rebuild removed it as merged upstream. The answer is
`200` with the patch.

| Error | When |
|---|---|
| `409` `patch_not_deleted` | the patch is not deleted |
| `409` `patch_exists` | another patch now uses the branch |

### `POST /api/v1/workspaces/{slug}/patches/reorder`

Sets the whole order in one step. The body is `{"patch_ids":["<id>",…]}`
and must name every patch that is not deleted exactly once; anything else
is `400` `invalid_request`. The answer is `200` with
`{"items":[…],"next_cursor":null}`, the patches in the new order. No
reorder queues a rebuild.

### `POST /api/v1/workspaces/{slug}/patches/{id}/reset-to-origin`

Forces the hub's copy of a patch branch to origin's current tip, whatever
`patch_branch_source` and `patch_divergence_policy` say. Use it to settle
a `diverged` patch in favour of origin. Only an `active`, `conflict` or
`disabled` patch can be reset. The hub fetches that one branch from origin
(within 2 minutes) and then:

| Situation | `action` |
|---|---|
| the hub has no such branch: it is created | `created` |
| both tips are equal: nothing is written | `none` |
| the hub's tip is behind origin's: the branch moves forward | `fast_forwarded` |
| otherwise: the hub's tip is saved as a backup, then the branch moves | `replaced` |

The answer is `200` with the patch and `action`, `rebuild_triggered`
(`true` when this call queued a rebuild, see
[what triggers a rebuild](#what-triggers-a-rebuild)), `rebuild_job_id`
(only then) and `replaced_sha` when a backup exists. A backup holds one
tip: the next replace overwrites it.

| Error | When |
|---|---|
| `409` `patch_status_invalid` | the patch is `merged_upstream` or `deleted` |
| `409` `missing_on_origin` | origin has no such branch |
| `409` `ref_moved` | someone moved the branch meanwhile; `details.current_sha` is where it is now. Read it and retry |
| `409` `conflict` | another ref is in the way of the branch's name |
| `502` `origin_fetch_failed` | origin could not be fetched; try again |

### The rebuild job

Every route that returns a job returns this object, every member present.

| Member | Meaning |
|---|---|
| `id`, `workspace_slug` | the job and its workspace |
| `status` | see [a rebuild is a durable job](#a-rebuild-is-a-durable-job) |
| `strategy`, `fail_mode`, `integration_branch` | as captured when the job was queued |
| `submitted_by` | see [what triggers a rebuild](#what-triggers-a-rebuild) |
| `retry_count` | the retryable failures so far |
| `next_attempt_at` | when a `failed` job runs again; else `null` |
| `error` | the text of the last failure, or `null` |
| `conflict` | the conflict report of the last failure, or `null` |
| `created_at`, `updated_at`, `completed_at` | `completed_at` is set in `completed`, `dead_letter` and `cancelled` |
| `upstream_head_sha` | the upstream base the attempt used |
| `integration_head_sha`, `previous_integration_head_sha` | the integration branch after and before the publish; the second is `null` when the branch did not exist |
| `patches_applied`, `patches_skipped`, `patches_conflicted`, `patches_removed` | counts; `0` until the job is `completed` |
| `integration_branch_pushed` | `true` when the branch was pushed to origin |
| `patch_results` | one entry per patch, filled in while the job runs |

Each entry of `patch_results` has `patch_id`, `branch_name`, `position`,
`status` (`success`, `conflict` or `skipped`), `skipped_reason`
(`disabled`, `merged_upstream`, `branch_not_found` or `nothing_to_apply`),
`source_sha` (the patch tip the run used), `new_head_sha` (the head after
the patch, for `success`), `conflict_files` and `conflict` (the report,
for `conflict`). A job that runs again starts with empty results.

### `POST /api/v1/workspaces/{slug}/rebuild`

Queues a rebuild. The body is optional. Accepts an `Idempotency-Key`.

| Member | Meaning |
|---|---|
| `strategy` | `rebase` or `merge`; absent, the workspace setting `rebuild_strategy` (default `rebase`) |
| `fail_mode` | `fail_fast` or `continue`; absent, the setting `rebuild_fail_mode` (default `fail_fast`) |

The answer is `202` with the job, `queued`.

| Error | When |
|---|---|
| `400` `no_active_patches` | no patch is `active` or `conflict`; there is nothing to rebuild |
| `409` `concurrent_rebuild` | a job of the workspace is `queued`, `running` or `failed`; wait for it, or cancel it while it is queued |

### `GET /api/v1/workspaces/{slug}/rebuilds` and `GET …/rebuilds/{id}`

The first is a [list](#using-the-api) of the workspace's jobs, newest
first; `status=<status>` (given once) keeps the jobs in that status. The
second answers one job. A `running` job shows the patch results stored so
far, so you can poll it for progress.

### `DELETE /api/v1/workspaces/{slug}/rebuilds/{id}`

Cancels a `queued` job; a cancelled job never runs. The answer is `200`
with the job, `cancelled`. A job in any other status, also one that
started a moment earlier, is `409` `job_not_cancellable`.

### `POST /api/v1/workspaces/{slug}/rebuilds/{id}/requeue`

Moves a `dead_letter` job back to `queued`, with `retry_count` 0 and its
error and conflict cleared. The job keeps its id, strategy, fail mode and
submitter. Use it after you fixed what stopped the job. The answer is
`200` with the job.

| Error | When |
|---|---|
| `409` `job_not_requeueable` | the job is not `dead_letter` |
| `409` `concurrent_rebuild` | another job of the workspace is `queued`, `running` or `failed` |

### `POST /api/v1/workspaces/{slug}/rebuilds/{id}/rollback`

Moves the integration branch back to the head it had before the completed
rebuild `{id}`, its `previous_integration_head_sha`. Nothing else changes:
no patch, no job, nothing on origin (the branch is not pushed), and no
rebuild is queued. A pending rebuild neither delays nor refuses the
rollback, and replaces the rolled-back head when it publishes. The route
takes no `Idempotency-Key`; repeating it is harmless.

The answer is `200` with
`{"rolled_back_to":"<sha>","previous_head_sha":"<sha>"}`, the head the
branch now has and the one it replaced.

| Error | When |
|---|---|
| `409` `rollback_unavailable` | the job is not `completed`, it was the first rebuild (no previous head), or the previous head no longer exists in the repository, such as after a reclone |
| `409` `ref_moved` | a rebuild or another rollback moved the branch meanwhile; `details.current_sha` is its head now. Read it and decide again |

### `GET /api/v1/workspaces/{slug}/rebuild-preview`

Predicts what a rebuild would do with each patch that is not `disabled`
or `merged_upstream`, in position order. The hub fetches nothing, so the
prediction is against the upstream base as the last sync or rebuild left
it, and works while upstream is unreachable. It changes nothing, writes no
event and can run while a rebuild runs.

The answer is `200` with `upstream_head_sha` (the base used) and
`patch_results`, one entry per tested patch with `patch_id`,
`branch_name`, `position`, `status`, `conflict_files` and `conflict`:

| `status` | Meaning |
|---|---|
| `would_succeed` | the patch merges onto the state so far |
| `would_conflict` | it does not; `conflict_files` and the conflict report say where. Later patches are tested without it |
| `would_skip` | the patch branch does not exist |

A preview always tests a merge of the patch tip and does not use
remembered resolutions, so a patch reported `would_conflict` may still
apply in a real rebuild.

| Error | When |
|---|---|
| `409` `upstream_unknown` | upstream was never fetched; sync the workspace first |

### `GET /api/v1/workspaces/{slug}/rerere`

A [list](#using-the-api) of the workspace's remembered resolutions, newest
first. Each item has `id` (git's id of the conflict), `path` (the file the
conflict was first met at), `resolved` (`false` while only the conflict is
recorded) and `recorded_at`.

### `DELETE /api/v1/workspaces/{slug}/rerere/{pathspec}`

Forgets remembered resolutions. `{pathspec}` is everything after
`/rerere/`, slashes included: the `id` of one entry, or a file path, which
forgets every entry recorded for exactly that path. The answer is `204`.
A forget only changes what later rebuilds replay: the next rebuild that
meets the conflict stops at it again. It changes no patch, no job and no
branch, and queues no rebuild.

| Error | When |
|---|---|
| `400` `invalid_request` | the pathspec is empty or starts with `-` |
| `404` `not_found` | no entry has that id or path |

### `GET /api/v1/workspaces/{slug}/patch-status`

One document that tells you where the workspace stands, from what the hub
already holds: it contacts no remote, so it answers while origin and
upstream are unreachable, and `upstream_head_sha` is the base as last
fetched.

| Member | Meaning |
|---|---|
| `workspace_slug`, `workspace_mode`, `status`, `git_url`, `upstream_url` | from the workspace |
| `integration_branch` | its name (`deploy` when the workspace names none) |
| `integration_head_sha` | its head now, or `null` when it does not exist |
| `upstream_head_sha`, `last_sync_at`, `clone_error`, `sync_error` | from the workspace, or `null` |
| `last_rebuild` | `{id, status, created_at, completed_at}` of the newest job, or `null` |
| `last_completed_rebuild` | the same for the newest `completed` job |
| `patches` | every patch that is not deleted, in position order, with `id`, `branch_name`, `position`, `status`, `conflict_files`, `last_rebuild_result`, `origin_sync_state`, `origin_sha`, `origin_synced_at`; not paged |
| `summary` | counts: `total_patches`, `active`, `conflict`, `disabled`, `merged_upstream`, `rerere_resolutions`, `patches_diverged`, `patches_missing_on_origin` |

`last_rebuild_result` is the patch's result (`success`, `conflict` or
`skipped`) in the newest rebuild that has one for it, or `null`.

## Source control

The source control API reads a workspace's repository and writes to it
without a clone. An agent that wants one file, the history of one path or
the diff between two commits asks for exactly that, and an agent that has
edited three files commits them with one request. Review agents, check
runners and small tasks then need no working copy at all, and orientation
in an unknown repository, the part of a task that reads the most and
changes nothing, costs a few requests.

The API and the git server are two doors to the same repository. What you
write through one is visible through the other, the same push rules decide
both, and both emit the same events (see [Git over HTTP](#git-over-http)).
Anything that needs a working tree, such as running a build, is out of
scope: that is what a clone or a sandbox is for.

All routes live under `/api/v1/workspaces/{slug}/scm`. Reads are `GET`
routes that need the scope `git:read` and read access to the workspace;
writes need `git:write` and write access. `{slug}` may be a task fork: a
fork is read through the same view of refs its git remote shows, its own
branches plus the parent's branches and tags at their current heads.

### Finding your way in an unknown repository

Four reads answer four different questions. Start with the cheapest that
fits.

| Question | Route | Reads | Needs the code index |
|---|---|---|---|
| What files are there? | [`…/scm/tree`](#get-apiv1workspacesslugscmtree) | any revision | no |
| What does this file declare? | [`…/scm/outline`](#get-apiv1workspacesslugscmoutline) | any revision | no |
| What are the lines of the declaration `X` in this file? | [`…/scm/declaration`](#get-apiv1workspacesslugscmdeclaration) | any revision | no |
| Where is the thing called `X` declared? | [`…/scm/symbols`](#get-apiv1workspacesslugscmsymbols) | head of the default branch | yes |
| Which files are about `X`? | [`…/scm/code-search`](#get-apiv1workspacesslugscmcode-search) | head of the default branch | yes |
| Which lines hold exactly this text? | [`…/scm/search`](#get-apiv1workspacesslugscmsearch) | any revision | no |

A usual walk: list the tree of the root, look up a name with symbols or a
topic with code search, outline the files that come back, and only then
read the parts of the files you need with
[`…/scm/file`](#get-apiv1workspacesslugscmfile), by byte range when the file
is large.

Code search and symbols answer from an index the hub builds in the
background for the head of the default branch. They are ranked and fast,
but they cover that one commit only: for a feature branch, a tag, an old
commit or a fork's own branches, use the exact search and the outline,
which read git directly at any revision. See
[The code index](#the-code-index) for when the index is ready and what to
do while it is not.

### Reading history and diffs

- [`…/scm/commits`](#get-apiv1workspacesslugscmcommits) lists the history
  of a revision, optionally of one path, one author or a time span, and
  [`…/scm/commits/{sha}`](#get-apiv1workspacesslugscmcommitssha) answers one
  commit with its statistics and the files it changed. A commit names the
  change that landed it and the agent session that wrote it, when the hub
  knows them.
- [`…/scm/blame`](#get-apiv1workspacesslugscmblame) answers which commit last
  changed each line of a file.
- [`…/scm/diff`](#get-apiv1workspacesslugscmdiff) answers what differs
  between two revisions, as JSON with a patch per file or as one patch
  `git apply` takes.
- [`…/scm/compare`](#get-apiv1workspacesslugscmcompare) answers how two
  revisions relate (ahead, behind, the commits, the files) and whether one
  rebases cleanly onto the other, without moving anything.

### Writing without a clone

A commit is made from file contents:
[`POST …/scm/commits`](#post-apiv1workspacesslugscmcommits) takes a branch,
a message and a list of file operations (`put`, `delete`, `move`,
`chmod`), builds the commit on the hub and moves the branch to it, or
creates the branch.

Every write is a compare-and-swap. You say which commit you saw the branch
at (`expected_head_sha`, `expected_sha`), and the hub moves the ref only
when it is still there. Several agents work on one repository at the same
time; without this, a commit built on a stale head would silently drop
what someone else wrote in between. When the ref has moved, the answer is
`409` `ref_moved` with the commit it is at now in
`error.details.current_sha`, and nothing has changed: read the branch
again, redo your edit on the new head and retry.

Refs are created, moved and deleted with the
[refs routes](#post-apiv1workspacesslugscmrefs), and
[`POST …/scm/rebase`](#post-apiv1workspacesslugscmrebase) replays a branch
onto another commit on the hub, so a branch stays current with its target
without a fetch. A rebase either moves the branch or changes nothing: when
a commit does not apply, the answer is `409` `rebase_conflict` with a
[conflict report](#the-conflict-report) that names the commit and the
files, and the branch stays where it was.

A write through the API is a push in every respect that matters to the
rest of the hub: the same rules refuse it, it emits the same
`parley.git.push` event, and a branch that is the source of a change gets
the change's next version. See
[What every write shares](#what-every-write-shares).

### Example: change a file on a new branch

Read the file as JSON. The answer holds the content and `resolved_sha`,
the commit it was read from.

```sh
curl -s -H "Authorization: Bearer $PARLEY_TOKEN" \
  -H "Accept: application/json" \
  "$HUB/api/v1/workspaces/demo/scm/file?path=README.md"
```

```json
{
  "path": "README.md",
  "sha": "13bcb9b3fd97cada4cd021e2e72c979c8238efa1",
  "mode": "100644",
  "size": 19,
  "encoding": "utf8",
  "content": "# Tree\n\nA fixture.\n",
  "resolved_sha": "b6916afb245765e766995f2bf355f74e2aee2453"
}
```

Commit the new content to a branch that does not exist yet. `base_ref` is
the commit the branch starts at; giving the `resolved_sha` of the read
bases the commit on exactly the tree you read. The `Idempotency-Key` makes
a retry after a lost answer safe.

```sh
curl -s -X POST -H "Authorization: Bearer $PARLEY_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: readme-fixture-1" \
  "$HUB/api/v1/workspaces/demo/scm/commits" \
  -d '{
    "branch": "docs/readme",
    "base_ref": "b6916afb245765e766995f2bf355f74e2aee2453",
    "message": "Describe the fixture",
    "operations": [
      {"op": "put", "path": "README.md",
       "content": "# Tree\n\nA fixture for the tree tests.\n"}
    ]
  }'
```

The answer is `201` with the commit, the branch as a ref object and the
change the commit belongs to, if any. `branch.sha` is the new head. To
commit to the branch again, leave `base_ref` out and send that head as
`expected_head_sha`:

```sh
curl -s -X POST -H "Authorization: Bearer $PARLEY_TOKEN" \
  -H "Content-Type: application/json" \
  "$HUB/api/v1/workspaces/demo/scm/commits" \
  -d '{
    "branch": "docs/readme",
    "expected_head_sha": "<branch.sha of the first answer>",
    "message": "Add a licence note",
    "operations": [
      {"op": "put", "path": "NOTICE", "content": "MIT\n"}
    ]
  }'
```

To open a change for review in the same request, add a `change` object to
the first commit (see
[`POST …/scm/commits`](#post-apiv1workspacesslugscmcommits)).

### Source control routes

The rules that hold for every route (credentials, scopes, the order of
checks, the error envelope, lists, idempotency) are in
[Using the API](#using-the-api).

| Route | Scope | What it does |
|---|---|---|
| [`GET …/scm/refs`](#get-apiv1workspacesslugscmrefs) | `git:read` | lists branches, tags and hub refs |
| [`GET …/scm/refs/{name}`](#get-apiv1workspacesslugscmrefsname) | `git:read` | answers one ref |
| [`GET …/scm/commits`](#get-apiv1workspacesslugscmcommits) | `git:read` | lists the history of a revision |
| [`GET …/scm/commits/{sha}`](#get-apiv1workspacesslugscmcommitssha) | `git:read` | answers one commit, with its files on request |
| [`GET …/scm/tree`](#get-apiv1workspacesslugscmtree) | `git:read` | lists a directory |
| [`GET …/scm/file`](#get-apiv1workspacesslugscmfile) | `git:read` | answers a file, raw or as JSON, whole or by range |
| [`GET …/scm/diff`](#get-apiv1workspacesslugscmdiff) | `git:read` | answers the file changes between two revisions |
| [`GET …/scm/compare`](#get-apiv1workspacesslugscmcompare) | `git:read` | answers how two revisions relate and whether one rebases onto the other |
| [`GET …/scm/blame`](#get-apiv1workspacesslugscmblame) | `git:read` | answers who last changed each line of a file |
| [`GET …/scm/search`](#get-apiv1workspacesslugscmsearch) | `git:read` | exact search of file contents at any revision |
| [`GET …/scm/outline`](#get-apiv1workspacesslugscmoutline) | `git:read` | answers the declarations of one source file |
| [`GET …/scm/declaration`](#get-apiv1workspacesslugscmdeclaration) | `git:read` | answers the declarations of one file that have a name, with their lines |
| [`GET …/scm/code-index`](#get-apiv1workspacesslugscmcode-index) | `git:read` | answers the state of the code index |
| [`GET …/scm/code-search`](#get-apiv1workspacesslugscmcode-search) | `git:read` | ranked search of the default branch |
| [`GET …/scm/symbols`](#get-apiv1workspacesslugscmsymbols) | `git:read` | finds declarations of the default branch by name |
| [`POST …/scm/commits`](#post-apiv1workspacesslugscmcommits) | `git:write` | makes a commit from file operations |
| [`POST …/scm/refs`](#post-apiv1workspacesslugscmrefs) | `git:write` | creates a branch or a tag |
| [`PATCH …/scm/refs/{name}`](#patch-apiv1workspacesslugscmrefsname) | `git:write` | moves a branch or a tag |
| [`DELETE …/scm/refs/{name}`](#delete-apiv1workspacesslugscmrefsname) | `git:write` | deletes a branch or a tag |
| [`POST …/scm/rebase`](#post-apiv1workspacesslugscmrebase) | `git:write` | rebases a branch on the hub |

`…` stands for `/api/v1/workspaces/{slug}`. A PAT with `git:write` also
satisfies `git:read`.

### What every read shares

**Access.** A read takes the admin token, an API key or a PAT and needs
read access to the workspace, so an org member reads as the owner does
(see [Workspaces](#workspaces)). A caller who cannot read the workspace
gets `404` `not_found`, as for a slug that does not exist.

**Revisions.** A revision parameter (`ref`, `base`, `head`, `{sha}`) is a
branch name, a tag name, a full ref name such as `refs/heads/main`, or a
full or abbreviated commit id (4 to 40 hexadecimal characters). A name
that begins with `refs/` is read as that ref only. An annotated tag names
the commit it points at. Expressions such as `main~2`, `HEAD` or `@{1}`
are not taken. A revision that names no commit, or more than one (a name
that is both a branch and a tag, an abbreviation several commits share),
is `404` `ref_not_found`. A `ref` you leave out is the head of the
workspace's default branch.

Each revision is resolved once, and the whole answer is computed from the
commit it resolved to: a read that runs while a branch moves answers for
the old commit or the new one, never a mix. Answers name that commit as
`resolved_sha` (or `base_sha` and `head_sha`), and the cursor of a list
pins it, so every page of one listing reads the same commit.

**Paths.** A `path` that names a file or a directory is relative to the
root of the repository, with no empty, `.`, `..` or `.git` segment. Any
other is `400` `invalid_request`. A path that does not exist at the commit
is `404` `path_not_found`.

**Workspace state.** A read is served when the workspace is `ready`, and
when it is `archived`. Otherwise:

| State | Answer |
|---|---|
| `pending` or `cloning`; a reclone, archive or delete is running | `409` `workspace_busy`: retry later |
| `failed` | `409` `workspace_not_ready` |
| a task fork | as its parent's state decides; a fork of an archived parent is served |

**Time limit.** Every read is bounded by `[scm].read_timeout`, 30 seconds
by default (see
[configuration.md](configuration.md#server-and-storage)). A read that has
not begun its answer by then is `503` `scm_timeout`; one whose answer has
begun is cut off, the connection closed before the body ends. Narrow the
request (a path, a smaller range, a more specific query) and retry.

**Caching.** Every successful read carries an `ETag`. Send it back in
`If-None-Match` and a current copy is answered `304 Not Modified` before
the hub does the expensive part of the read. A read whose revisions are
all written as full 40-character commit ids can never change and is marked
`Cache-Control: private, max-age=31536000, immutable`; every other read,
by name, by abbreviated id or with `ref` left out, and both refs routes,
are `private, no-cache`: revalidate them with the tag.

**Events.** No read emits an event. Reads and writes are counted per
operation in the hub's metrics (see
[Health, version and metrics](#health-version-and-metrics)).

### Objects in answers

A **ref object**:

| Member | Value |
|---|---|
| `name` | the full ref name, such as `refs/heads/main` |
| `kind` | `branch` (`refs/heads/`), `tag` (`refs/tags/`) or `hub` (`refs/parley/`) |
| `sha` | the commit the ref points at; for an annotated tag, the commit it tags |
| `protected` | `true` for a protected branch and for every hub ref |
| `tracking` | `true` for a branch a task fork shows from its parent; you cannot write to it |
| `change` | `{"workspace_slug", "number"}` of the draft or open change whose source the branch is, else `null` |

Refs outside these three namespaces are never listed, and a task fork
shows no `refs/parley/` ref.

A **commit object**:

| Member | Value |
|---|---|
| `sha`, `parents`, `tree_sha` | full ids; `parents` is `[]` for a root commit |
| `author`, `committer` | `{"name", "email", "time"}`, the time in UTC |
| `message` | the whole message as written |
| `change` | `{"workspace_slug", "number", "version"}` of the change whose version head the commit is, or that landed it; `null` for a commit of no change |
| `session_id` | the agent session that introduced the commit, when it is the head of a change's version or was written by a land; else `null` |
| `stats` | `{"files_changed", "insertions", "deletions"}` against the first parent; only where the route says |

A **file change** (commit with `files=true`, diff, compare):

| Member | Value |
|---|---|
| `path` | the file's path on the new side; for a deleted file, the path it had |
| `old_path` | the old path of a renamed or copied file, else `null` |
| `status` | `added`, `modified`, `deleted`, `renamed`, `copied` or `type_changed` |
| `insertions`, `deletions` | lines added and removed; `0` for a binary file |
| `binary` | `true` for a file git treats as binary |
| `old_sha`, `new_sha` | the blob id on each side, `null` where the file is missing |
| `patch` | the file's part of the unified diff; on the diff route only, never for a binary file |

Renames and copies are detected as git detects them, so a file moved with
small edits is one `renamed` change, not a deletion and an addition.

### The code index

The hub keeps a search index of the head of the default branch of a
workspace when its configuration has `[code_index]` (see
[configuration.md](configuration.md#the-configuration-file)) and the
workspace's setting `code_index` is on (see [Workspaces](#workspaces)). It
rebuilds the index in the background whenever that head moves, whatever
moved it: a land, a push, an API write, a sync from the origin or a
rebuild. A periodic pass catches any move the hub was not told of, so the
index never stays behind for long (see
[ADR 11](adr/11-tell-the-code-index-where-a-head-is-written.md)). An index
that is current survives a hub restart without a rebuild.

**You never get a stale answer.** Code search and symbols never answer
from a commit older than the head of the default branch at the moment
your request arrived. If the index already holds that head or a later
one, the answer is immediate. Otherwise the request waits for the build,
for at most the read timeout. So a search right after a land sees the
landed code, at the price of a wait.

When the wait runs out, or the last build failed, the answer is `503`
`code_index_not_ready` with a `Retry-After` header and, in the error
object, `state` (`building` or `failed`), `head_sha` and `indexed_sha`
(`null` when there is no index yet). What to do then:

- Retry after `Retry-After` seconds. A first build of a large repository
  can take longer than one request waits.
- Meanwhile, use the reads that need no index:
  [`…/scm/search`](#get-apiv1workspacesslugscmsearch) for exact text and
  [`…/scm/outline`](#get-apiv1workspacesslugscmoutline) for one file's
  declarations, at any revision.
- Ask [`…/scm/code-index`](#get-apiv1workspacesslugscmcode-index) for the
  state without waiting.

Other refusals of the two indexed routes:

| Answer | When | What to do |
|---|---|---|
| `404` `code_index_disabled` | the hub has no `[code_index]` | use search and outline |
| `409` `code_index_off` | the workspace's setting `code_index` is off (for a fork, its parent's) | turn it on, or use search and outline |
| `404` `ref_not_found` | the workspace has no repository or no commit yet | wait for the clone |
| `409` `code_index_partial` | symbols only: the index holds some of the files | use code search, which answers from a partial index |

**Forks.** A task fork has no index of its own. A request addressed to a
fork is answered from its parent's index, under the same waiting rule,
and says so: `workspace` in the answer is the parent's slug. What the
fork's own branches hold is not in the answer.

**Caching.** A `200` carries an `ETag` that changes with the indexed
commit and `Cache-Control: private, no-cache`. A matching `If-None-Match`
is answered `304` once the waiting rule is satisfied, so after a commit
the same request waits for the new build and is answered anew.

### What every write shares

**Access.** A write takes the admin token, an API key or a PAT with
`git:write`, and needs write access to the workspace, which its owner and
the admin token have (see [Workspaces](#workspaces)). An org member who
can only read gets `403` `forbidden`.

**Workspace state.** A write is refused before its body is read when the
workspace cannot take it:

| State | Answer |
|---|---|
| `archived` | `409` `workspace_archived` |
| `pending` or `cloning`; a reclone, archive or delete is running | `409` `workspace_busy` |
| `failed` | `409` `workspace_not_ready` |
| a task fork whose parent is archived | `409` `parent_archived` |
| a task fork whose parent is busy or failed | the parent's answer |

**Ref names.** A ref name is a full name below `refs/heads/` or
`refs/tags/`, or a bare branch name, read as `refs/heads/{name}`. It must
be a name git accepts. `HEAD` and other namespaces are `400`
`invalid_request`; `refs/parley/` belongs to the hub (`403`
`hub_owned_ref`). A commit id in a body or a query is a full 40-character
id.

**The push rules.** Every ref a write moves is decided by the rules that
decide a `git push` of the same update, described under
[Git over HTTP](#git-over-http). A write is therefore refused exactly
where the push would be, with `403` and one of `hub_owned_ref`,
`protected_branch`, `default_branch`, `tracking_branch`,
`integration_branch`, `patch_branch` or `path_not_allowed`. Two refusals
tell you what to do instead:

- `path_not_allowed` names the first path your token may not change as
  `error.path`.
- `tracking_branch` (a write to a parent's branch in a task fork) names
  the fork's base branch as `error.details.base_ref`: commit to a branch
  of your own.

A protected branch is held as for a push: nobody creates or deletes one
or moves it off its own history, and only the owner or the admin token
moves it forward, while the workspace allows direct pushes. Everyone else
gets `403` `protected_branch`: write to another branch and open a change.

**Compare-and-swap.** A ref is moved only from the commit the request
expects. A ref that is elsewhere, or moved while the request ran, is
`409` `ref_moved` with `error.details.current_sha` (`null` when the ref
no longer exists).

**A refused write changes nothing.** It moves no ref, emits no event,
opens no change and is not recorded for an `Idempotency-Key`, so a retry
is decided again.

**Idempotency.** The three `POST` routes take an `Idempotency-Key` (see
[Using the API](#using-the-api)): a repeat with the same body answers
`200` with the first answer and moves nothing; the same key with another
body is `409` `conflict`. `PATCH` and `DELETE` ignore the header;
`expected_sha` makes their retry safe.

**Sessions.** The commit and the rebase route take a `session_id`, the
agent session the write belongs to. A session the hub does not know for
you and the workspace is `400` `unknown_session`, and nothing moves. An
accepted session is recorded on the write's events, on the change version
it records and, for a commit, on the commit object.

**Consequences.** An accepted write emits one `parley.git.push` event per
ref, with `via` `"api"` where a push has `"git"`; `forced` is `true` when
the old commit is not an ancestor of the new one, and `direct` is `true`
for a move of a protected branch. When the branch is the source of a
draft or open change, the write records the change's next version, of
kind `commit`, `ref` or `rebase`, in the same step as the move (see
[Changes](#changes)); if the version cannot be recorded, the write is
refused and the branch does not move. Writes have no time limit.

### The conflict report

[Compare](#get-apiv1workspacesslugscmcompare) and
[rebase](#post-apiv1workspacesslugscmrebase) replay commits one by one and
stop at the first that does not apply. Both describe it with the same
report, which is also the shape of a land's conflict report (see
[Landing](#landing)):

| Member | Value |
|---|---|
| `target_sha` | the commit the branch was replayed onto |
| `head_sha` | the head of the branch replayed |
| `base_sha` | their merge base, `null` when they share none |
| `commit_sha` | the first commit that did not apply |
| `files` | the conflicted files, by path: `{"path", "kind", "base", "ours", "theirs", "landed_by"}` |
| `landed_changes` | the changes landed on the target since the merge base that changed a conflicted file |
| `change` | always `null` here |

For each file, `base`, `ours` and `theirs` are the file on the three sides
of the merge that failed, each `{"sha", "mode", "path"}` or `null` where
the file does not exist: `base` in the failed commit's parent, `ours` in
the target with the earlier commits replayed on it, `theirs` in the failed
commit. `landed_by` lists the numbers of the changes in `landed_changes` that
touched the file. `kind` says why the file conflicts:

| `kind` | Meaning |
|---|---|
| `content` | both sides changed the same lines |
| `add_add` | both added the path with different content, or a file meets a directory |
| `modify_delete` | the target changed a file the branch deleted |
| `delete_modify` | the target deleted a file the branch changed |
| `rename` | the sides renamed the file in ways that clash |
| `mode` | the sides hold the file with different modes, such as a file and a symlink |
| `binary` | both sides changed a binary file |
| `submodule` | a side holds a submodule |

A replay keeps no merge commits: a merge commit among the commits to
replay is `400` `merge_commits_unsupported`, before anything is replayed.

### `GET /api/v1/workspaces/{slug}/scm/refs`

Lists the refs whose full names begin with `prefix`, as
[ref objects](#objects-in-answers) in order of name. It is a list with the
standard `limit` and `cursor`.

| Parameter | Meaning |
|---|---|
| `prefix` | the start of the full names listed; default `refs/heads/`; `refs/` lists branches, tags and hub refs |

### `GET /api/v1/workspaces/{slug}/scm/refs/{name}`

Answers the ref object of one ref. `{name}` takes the rest of the path: a
full ref name, such as `refs/tags/v1`, or a branch name, so
`…/refs/feature/x` and `…/refs/refs/heads/feature/x` answer the same
object. A tag needs its full name. A ref that does not exist is `404`
`ref_not_found`.

### `GET /api/v1/workspaces/{slug}/scm/commits`

Lists the commits reachable from `ref`, newest first, as
[commit objects](#objects-in-answers). It is a list with the standard
`limit` and `cursor`, and names the commit it started from in
`resolved_sha`.

| Parameter | Meaning |
|---|---|
| `ref` | the revision whose history is listed; default the default branch |
| `first_parent` | `true` (default) follows only the first parent of each merge; `false` also lists the commits a merge brought in |
| `path` | only the commits that changed this file or anything below this directory |
| `follow` | `true` follows `path`, which must then name one file, across renames; default `false` |
| `since`, `until` | RFC 3339 timestamps bounding the committer time, both inclusive |
| `author` | only commits whose author name or e-mail contains this text, ignoring case |
| `stats` | `true` adds `stats` to each commit; default `false` |

The filters combine. A path no commit changed answers an empty list.

### `GET /api/v1/workspaces/{slug}/scm/commits/{sha}`

Answers one commit, always with `stats`. `{sha}` is any revision; write a
branch name that has a `/` with `%2F`, as `…/commits/feature%2Fx`. With
`files=true` the answer also has `files`, the commit's
[file changes](#objects-in-answers) against its first parent, without
`patch`.

### `GET /api/v1/workspaces/{slug}/scm/tree`

Lists the entries of a directory at a commit, in order of path. It is a
list whose `limit` defaults to 1000 and may be 1 to 10000. After
`next_cursor` it names `resolved_sha` and `tree_sha`, the id of the
listed directory.

| Parameter | Meaning |
|---|---|
| `ref` | the revision; default the default branch |
| `path` | the directory listed; default the root |
| `recursive` | `true` lists everything below `path`, directories and their contents; default `false` |

| Entry member | Value |
|---|---|
| `path`, `name` | the path from the root and its last segment |
| `type` | `blob` (file), `tree` (directory), `symlink` or `commit` (submodule) |
| `mode` | git's mode: `100644`, `100755`, `040000`, `120000` or `160000` |
| `sha` | the entry's object id |
| `size` | the size in bytes, for a `blob` only |

A `path` that names a file is `400` `invalid_request`.

### `GET /api/v1/workspaces/{slug}/scm/file`

Answers one file or symbolic link at a commit. `path` is required; `ref`
defaults to the default branch.

**Raw (the default).** The answer is the file's bytes (for a symbolic
link, its target) as `application/octet-stream`, with the headers
`X-Parley-Blob-Sha`, `X-Parley-Resolved-Sha` and `X-Parley-File-Mode`. A
`Range` header with one byte range (`bytes=a-b`, `bytes=a-` or
`bytes=-n`) is answered `206 Partial Content` with `Content-Range`; a
range that starts at or after the end of the file is `416`
`range_not_satisfiable`. A header with several ranges is ignored and the
whole file answered.

**JSON.** With `Accept: application/json` the answer is an object, and
`Range` is ignored:

| Member | Value |
|---|---|
| `path` | the path, as asked |
| `sha`, `mode`, `size` | the blob id, git's mode and the size in bytes |
| `encoding` | `utf8` when the bytes are valid UTF-8, else `base64` |
| `content` | the bytes as text, or base64-encoded |
| `resolved_sha` | the commit `ref` resolved to |

The two forms have separate `ETag`s, and answers carry `Vary: Accept`.

| Error | When |
|---|---|
| `400` `invalid_request` | no `path`, or it names a directory or a submodule |
| `404` `path_not_found` | the path does not exist at the commit |
| `413` `payload_too_large` | the file is over `[scm].max_file_size` (32 MiB by default); `error.size` is its size. Nothing of it is served, by range either |
| `416` `range_not_satisfiable` | the range starts past the end of the file |

### `GET /api/v1/workspaces/{slug}/scm/diff`

Answers the file changes between two revisions.

| Parameter | Meaning |
|---|---|
| `base`, `head` | the two revisions, both required |
| `mode` | `merge_base` (default): what `head` changed since it parted from `base`, as a review wants it; `direct`: `base` itself against `head` |
| `path` | a glob, repeatable: only matching files are answered and counted. `*` matches within a segment, `**` across segments, against the whole path, case-sensitively |
| `patches` | `false` leaves `patch` out of every file; default `true` |
| `context` | lines of context in the patches, 0 to 20; default 3 |
| `format` | `json` (default) or `patch` |

The JSON answer:

| Member | Value |
|---|---|
| `base_sha`, `head_sha` | the commits the revisions resolved to |
| `merge_base_sha` | their merge base, in both modes; `null` when they share no ancestor, and then `merge_base` mode shows every file of `head` as `added` |
| `files` | the [file changes](#objects-in-answers), in order of path |
| `stats` | totals over every file the globs keep, also those a cut leaves out |
| `truncated` | `true` when the answer was cut |

An answer lists at most 5,000 files, and patches are included file by
file up to 8 MiB in total; later files come without `patch`. Either cut
sets `truncated`: narrow the diff with `path`.

With `format=patch` the answer is the unified diff as `text/x-diff`,
binary files included, which `git apply` applies to the base side. It is
cut at a file boundary by the same limits and then carries
`X-Parley-Truncated: true`.

### `GET /api/v1/workspaces/{slug}/scm/compare`

Answers how `head` relates to `base` and whether `head` can be rebased
onto `base`, from a trial rebase that moves no ref. Both parameters are
required. Use it before a [rebase](#post-apiv1workspacesslugscmrebase) or
a land to see a conflict without causing one.

| Member | Value |
|---|---|
| `base_sha`, `head_sha`, `merge_base_sha` | as for the diff |
| `ahead`, `behind` | how many commits `head` has that `base` lacks, and the reverse |
| `commits` | the first 200 commits `head` has and `base` lacks, oldest first |
| `files` | the first 5,000 file changes from the merge base to `head`, without `patch` |
| `mergeable` | `clean` when every commit applies, `conflict` when one does not |
| `conflict` | `null` when `clean`, else the [conflict report](#the-conflict-report) |

A `head` that `base` already contains answers `ahead` `0`, no commits, no
files and `clean`. A merge commit among the commits of `head` is `400`
`merge_commits_unsupported`. The trial needs git 2.40 or later on the
hub.

### `GET /api/v1/workspaces/{slug}/scm/blame`

Answers which commit last changed each line of a text file.

| Parameter | Meaning |
|---|---|
| `ref` | the revision; default the default branch |
| `path` | the file, required |
| `start_line`, `end_line` | the lines answered, 1-based and inclusive; default the whole file |

The answer has `resolved_sha`, `path` and `ranges`, which cover the lines
asked for in order, without gaps. Each range is a run of lines one commit
last changed:

| Member | Value |
|---|---|
| `start_line`, `end_line` | the range in the file as it is at `ref` |
| `commit` | the [commit object](#objects-in-answers), without `stats`; its `change` tells you which change landed the lines |
| `original_path` | the file's path in that commit; renames are followed |
| `original_start_line` | where the range's first line was in that commit |

| Error | When |
|---|---|
| `400` `invalid_request` | no `path`; a directory or a submodule; a binary file; a line range that is not within the file |
| `413` `payload_too_large` | the file is over 2 MiB; `error.size` is its size |

### `GET /api/v1/workspaces/{slug}/scm/search`

Searches the text files of a commit for the lines that match `q` and
answers one item per matching line, by path and then by line. It reads
git directly, so it works at any revision and needs no index; it is
exact and unranked. It is a list whose `limit` defaults to 50 and may be
1 to 200, and it names the commit searched in `resolved_sha`.

| Parameter | Meaning |
|---|---|
| `q` | what to look for, required, 1 to 500 characters; a literal string unless `regex` is `true` |
| `ref` | the revision; default the default branch |
| `regex` | `true` takes `q` as an extended regular expression (`\w`, `\W`, `\s` and `\S` are understood); default `false` |
| `case_sensitive` | `false` ignores case; default `true` |
| `path` | a glob, repeatable, as on the diff route: only matching files are searched |

A match has `path`, `line` (1-based) and `text`, the line without its
line ending, cut to 500 characters. Binary files and submodules are not
searched. An invalid regular expression is `400` `invalid_request`.

### `GET /api/v1/workspaces/{slug}/scm/outline`

Answers what one source file declares: its functions, types and other
declarations, in line order. It reads one file and needs no index, so it
works at any revision, a fork's own branches included. `path` is
required; `ref` defaults to the default branch (for a fork, its base
branch).

| Member | Value |
|---|---|
| `path`, `resolved_sha` | the path as asked and the commit read |
| `language` | from the file's extension: `Go`, `Python`, `JavaScript`, `TypeScript`, `Rust`, `Java`, `Kotlin`, `C#`, `Ruby`, `C`, `C++`, `PHP`, `Scala`, `Lua` or `Shell`; `""` for an unknown language or a symbolic link |
| `supported` | whether this hub outlined the file; `false` for an unknown language, a symbolic link, or a language this hub cannot parse |
| `declarations` | the declarations; `[]` when there are none or `supported` is `false` |

| Declaration member | Value |
|---|---|
| `kind` | `func`, `method`, `type`, `class`, `interface`, `enum`, `trait`, `const`, `var`, `module` or `macro` |
| `name` | the declared name |
| `container` | the type, class or module it belongs to, such as a method's type; `""` for none |
| `signature` | the declaration's header as written, without its body |
| `exported` | whether it is visible outside its file or package, by the language's rule |
| `start_line`, `end_line` | 1-based and inclusive; `end_line` is `0` when unknown |

Check `supported` before you read an empty `declarations` as "declares
nothing". The hub image outlines all fifteen languages; a `parleyd` built
without cgo outlines Go alone and answers the others with `supported`
`false`. A file with syntax errors is outlined as far as it parses.

| Error | When |
|---|---|
| `400` `invalid_request` | no `path`, or it names a directory or a submodule |
| `413` `payload_too_large` | the file is over 1 MiB; `error.size` is its size |
| `422` `binary_file` | the file is binary |
| `503` `scm_timeout` | the parse did not finish in time, or the hub was busy parsing other files: retry |

```json
{"path":"router.go","resolved_sha":"3d0c5a1f0b6e2d5c8a7f4e9b1c2d3e4f5a6b7c8d","language":"Go","supported":true,"declarations":[{"kind":"type","name":"Router","container":"","signature":"type Router struct","exported":true,"start_line":6,"end_line":8},{"kind":"method","name":"Handle","container":"Router","signature":"func (r *Router) Handle(pattern string)","exported":true,"start_line":10,"end_line":12}]}
```

### `GET /api/v1/workspaces/{slug}/scm/declaration`

Answers the declarations of one source file that have a name, each with
its lines, so that an agent reads one function instead of the file. It
reads one file and needs no index, so it works at any revision, a fork's
own branches included.

| Parameter | Meaning |
|---|---|
| `path` | required: the file |
| `name` | required: the declaration's name, 1 to 200 characters, compared exactly and with case |
| `container` | only the declarations of this container, such as a method's type, compared exactly; empty is no filter |
| `kind` | only the declarations of this kind, one of the outline's kinds |
| `ref` | the revision; default the default branch (for a fork, its base branch) |

The answer has `path`, `resolved_sha`, `blob_sha` (the file's blob id),
`language` and `declarations`: every declaration that matches, in line
order, each with the [outline's](#get-apiv1workspacesslugscmoutline)
members and `content`, the file's lines `start_line` to `end_line` with
their line endings as stored. A declaration's lines begin at the line of
its name: a leading comment, and a decorator, annotation or attribute on a
line of its own, are outside them. A file with syntax errors is answered as
far as it parses; a declaration whose last line is not known there has
`end_line` `0` and `content` `null`.

```json
{"path":"svc.go","resolved_sha":"3d0c5a1f0b6e2d5c8a7f4e9b1c2d3e4f5a6b7c8d","blob_sha":"9b1c2d3e4f5a6b7c8d3d0c5a1f0b6e2d5c8a7f4e","language":"Go","declarations":[{"kind":"func","name":"Greet","container":"","signature":"func Greet() string","exported":true,"start_line":4,"end_line":6,"content":"func Greet() string {\n\treturn \"hi\"\n}\n"}]}
```

| Error | When |
|---|---|
| `400` `invalid_request` | no `path` or `name`, a `name` over 200 characters, a `kind` the outline does not have, or a `path` naming a directory or a submodule |
| `404` `path_not_found` | the file does not exist at the revision |
| `404` `declaration_not_found` | no declaration of the file matches |
| `413` `payload_too_large` | the file is over 1 MiB; `error.size` is its size |
| `422` `unsupported_language` | the file is in no language this hub can outline, or is a symbolic link; `error.details.language` is its language, `""` when unknown |
| `422` `binary_file` | the file is binary |
| `503` `scm_timeout` | the parse did not finish in time, or the hub was busy parsing other files: retry |

### `GET /api/v1/workspaces/{slug}/scm/code-index`

Answers which commit the [code index](#the-code-index) of a workspace
holds and what state it is in, without waiting. It answers `200` also
when the feature or the setting is off, takes no parameter and carries no
`ETag`.

| Member | Value |
|---|---|
| `workspace` | the workspace the answer is about: `{slug}`, or the parent's slug for a task fork |
| `enabled` | `true` when the hub has `[code_index]` and the workspace's setting is on |
| `state` | see below |
| `head_sha` | the head of the default branch, `null` while there is none |
| `indexed_sha` | the commit the existing index was built from, `null` when there is none |
| `partial` | `true` when a configured bound (files, size, build time) stopped the build, so the index holds only some of the files |
| `files_indexed` | how many files the index holds |
| `built_at` | when the index was built, `null` when there is none |
| `error` | why the last build failed, `null` otherwise |

| `state` | Meaning |
|---|---|
| `off` | `enabled` is `false` |
| `queued` | the index is behind the head and a build is waiting |
| `building` | a build is running |
| `ready` | the index holds the head |
| `failed` | the last build of the head failed; the hub retries with growing delays, and at once when the head moves |

`indexed_sha`, `partial`, `files_indexed` and `built_at` always describe
the index that exists, so during a build or after a failure they are
those of the previous index.

```json
{"workspace":"demo","enabled":true,"state":"ready","head_sha":"3d0c5a1f0b6e2d5c8a7f4e9b1c2d3e4f5a6b7c8d","indexed_sha":"3d0c5a1f0b6e2d5c8a7f4e9b1c2d3e4f5a6b7c8d","partial":false,"files_indexed":412,"built_at":"2026-11-21T16:05:12Z","error":null}
```

### `GET /api/v1/workspaces/{slug}/scm/code-search`

Answers the files of the default branch that match a query, best first,
from the [code index](#the-code-index).

| Parameter | Meaning |
|---|---|
| `q` | the query, required: 1 to 500 characters and at most 1,024 bytes, in zoekt's query syntax, such as `NewRouter`, `sym:Handler lang:go`, `needle file:internal/`, `"exact phrase"` or a regular expression between slashes |
| `limit` | the most files answered, 1 to 25; default 20 |

| Member | Value |
|---|---|
| `workspace` | the workspace whose index answered: `{slug}`, or its parent for a fork |
| `indexed_sha` | the commit that was searched |
| `partial` | `true` when the index holds only some of the files |
| `truncated` | `true` when more matched than the answer holds |
| `files` | the matching files in rank order, each with `path`, `language` and `matches` |

`matches` are the matching lines of a file in line order, each with
`line` and `text` (cut to 500 characters), at most 20 per file from its
best-ranked places. A file that matches by its name alone has one match,
line 1 with its path as `text`.

| Error | When |
|---|---|
| `400` `invalid_request` | no `q`, a blank or too long one, a bad `limit` |
| `400` `invalid_query` | the query does not parse; the message says why |
| `503` `scm_timeout` | the search took longer than 10 seconds: narrow the query or add a `file:` constraint |
| the index refusals | see [The code index](#the-code-index) |

### `GET /api/v1/workspaces/{slug}/scm/symbols`

Answers the declarations of the default branch whose name starts with a
given name, from the [code index](#the-code-index).

| Parameter | Meaning |
|---|---|
| `name` | the start of the name, required, 1 to 200 characters. Case is ignored unless the name has an upper-case letter; a name with a `.` is matched as container and name (`Router.New`) |
| `exact` | `true` answers only declarations of exactly that name; default `false` |
| `kind` | only this kind, one of the outline's kinds |
| `path` | only declarations in this file or under this directory |
| `limit` | the most declarations answered, 1 to 50; default 20 |

The answer has `workspace`, `indexed_sha`, `partial` (always `false`),
`truncated` (`true` when more matched than `limit`) and `symbols`, by
path and then by line. A symbol has `path` and the members of an
[outline](#get-apiv1workspacesslugscmoutline) declaration.

| Error | When |
|---|---|
| `400` `invalid_request` | a missing or bad parameter |
| `409` `code_index_partial` | the index is partial; `error.indexed_sha` names it. Use code search |
| `422` `too_many_matches` | more than 1,000 declarations match: give a longer name, a `kind` or a `path` |
| the index refusals | see [The code index](#the-code-index) |

### `POST /api/v1/workspaces/{slug}/scm/commits`

Makes one commit from a list of file operations and moves one branch to
it, or creates the branch there. See
[What every write shares](#what-every-write-shares) for the rules.

| Member | Meaning |
|---|---|
| `branch` | required: the branch to commit to |
| `expected_head_sha` | the commit you saw `branch` at; required when the branch exists |
| `base_ref` | a revision naming the commit a new branch starts at; required when the branch does not exist, ignored when it does |
| `message` | required, not empty |
| `author` | `{"name", "email"}`; default the caller (for the admin token, the hub) |
| `operations` | the operations, at most 1,000, of which at most 100 declaration operations, applied in order, each to the tree the one before left |
| `allow_empty` | `true` to make a commit that changes nothing; default `false` |
| `change` | `{"target_branch", "title", "intent", "draft"}`: open a change with `branch` as its source |
| `session_id` | the agent session the commit belongs to |

| `op` | Members | Effect |
|---|---|---|
| `put` | `path`, exactly one of `content` (UTF-8 text) and `content_base64`, optional `mode` (`100644`, the default, `100755`, or `120000` for a symlink whose target is the content) | writes or replaces the file, creating its directories |
| `delete` | `path` | removes the file; a directory left empty goes with it |
| `move` | `from`, `to` | renames the file, keeping content and mode |
| `chmod` | `path`, `mode` (`100644` or `100755`) | changes the mode alone |
| `replace_declaration` | `path`, `name`, optional `container` and `kind`, `content` (UTF-8 text) | replaces the declaration's lines by `content` |
| `delete_declaration` | `path`, `name`, optional `container` and `kind` | removes the declaration's lines |
| `insert_declaration` | `path`, `content`, exactly one of `after` and `before`, each `{"name", "container", "kind"}` | inserts `content` on the line after the declaration's last line, or before its first, adding no blank line |

**Declaration operations** change a file by the address of one of its
declarations instead of sending the whole file, and are then a `put` of
the result in every respect: the push rules, the events and the answer
are those of a `put`, and nothing in them says how the file was changed.
An address is `name`, with `container` and `kind` to tell apart what
shares a name, compared as the
[declaration read](#get-apiv1workspacesslugscmdeclaration) compares them;
it must match exactly one declaration of the file as the operations before
it left it, a `put` or a `move` among them. A declaration's lines are those
the outline reports, so its leading comment is outside them; to change it
too, use `put`. A `content` that does not end in a line ending gets the
file's own, that of its first line (LF for a file with none). The file
keeps its mode. The file must parse before the operation and after it;
only syntax is checked, so a grammar older than the file's language can
refuse a file the language accepts, and `put` is the way past it.

Paths are relative to the root, and no segment may be `.git` or anything
macOS or Windows would read as `.git`. The committer is always the hub
(`parley <parley@localhost>`), and both times are the time of the
request. The commit's parent is the head of `branch`, or the commit
`base_ref` names for a new branch. Of several requests with the same
`expected_head_sha`, one answers `201` and the others `409` `ref_moved`.

The body may be as large as `[scm].max_commit_size`, 32 MiB by default
(see [configuration.md](configuration.md#server-and-storage)), instead of
the general body limit.

**Changes.** The commit relates to changes in three ways (see
[Changes](#changes)):

- The branch is already the source of a draft or open change: the commit
  becomes its next version.
- You send a `change` object: it is checked as a request to open a change
  is, before the branch moves, and a refusal (such as `400`
  `target_not_found` or `409` `change_exists`) is the answer, with nothing
  committed. Otherwise the change is opened once the branch has moved; in
  a task fork it is opened in the parent. A `change` object on a protected
  branch is `400` `invalid_request` for every caller.
- In a task fork whose `auto_change` is on, a commit to a branch with no
  change opens one in the parent, against the fork's base branch. The
  commit is kept whether or not that works: when it fails, `change` is
  `null` and `change_error` is `{"error_type", "message"}`.

The answer has `commit` (the commit object, with `stats`), `branch` (the
ref object at the new commit), `change` (`{"workspace_slug", "number",
"version"}` of the change the commit made a version of or opened, else
`null`) and `change_error`:

```json
{"commit":{"sha":"6d2ec0406c25abdbdf0518833eff0667aecbff02","parents":["8792bc3d8b0e88fe7152443b9175ee4b05c54691"],"tree_sha":"474930f4d466d1c07e08b0986e3b5ca1556d21ac","author":{"name":"owner","email":"owner@example.com","time":"2026-11-15T11:11:44Z"},"committer":{"name":"parley","email":"parley@localhost","time":"2026-11-15T11:11:44Z"},"message":"m\n","change":null,"session_id":null,"stats":{"files_changed":1,"insertions":1,"deletions":0}},"branch":{"name":"refs/heads/work","kind":"branch","sha":"6d2ec0406c25abdbdf0518833eff0667aecbff02","protected":false,"tracking":false,"change":null},"change":null,"change_error":null}
```

After the push event, the commit emits one `parley.scm.commit` event. Its
metadata names the `branch`, the `sha`, the `parent_sha` and `files`, the
first 100 paths the commit changed (`truncated` is `true` when there were
more), so a subscriber sees what was touched without reading the diff.

| Status | When |
|---|---|
| `201 Created` | the commit was made and the branch moved |
| `200 OK` | a replay of an `Idempotency-Key`: the first answer |
| `400` `invalid_request` | a missing or malformed member or operation; a branch that does not exist and no `base_ref`; a `change` object on a protected branch |
| `400` `invalid_operation` | an operation does not apply to the tree, such as a `delete` of a missing path, a `move` onto an existing file, a `put` onto a directory, anything on a submodule, or a declaration operation on a path that is no file; `error.details.index` is the zero-based index of the operation |
| `400` `empty_commit` | the operations change nothing and `allow_empty` is not `true` |
| `400` `unknown_session` | the `session_id` is not accepted |
| `403` | a push rule refuses the update |
| `404` `ref_not_found` | `base_ref` names no commit |
| `409` `ref_moved` | the branch exists and `expected_head_sha` is missing or stale |
| `409` `ref_exists` | a new branch's name clashes with an existing one, one being a directory of the other (`feat` and `feat/x`) |
| `413` `payload_too_large` | the body is over `[scm].max_commit_size`; or a declaration operation's file is over 1 MiB before or after it, `error.size` its size |
| `404` `declaration_not_found` | a declaration operation's address matches no declaration |
| `409` `ambiguous_declaration` | it matches several; `error.details.candidates` lists at most 20, each `{"kind", "name", "container", "signature", "start_line", "end_line"}`, in line order |
| `422` `unsupported_language` | a declaration operation's file is in no language this hub can outline, or is a symbolic link; `error.details.language` is its language |
| `422` `binary_file` | a declaration operation's file is binary |
| `422` `unparseable_file` | a declaration operation's file has a syntax error before it; `error.details.line` is the first error's line |
| `422` `syntax_error` | the file would have a syntax error after it; `error.details.errors` lists at most 20 positions, each `{"line", "column"}`, in the resulting file |
| `503` `scm_timeout` | the declaration operations' parses did not finish within `[scm].read_timeout`, or the hub was busy parsing other files: retry |

A refusal of a declaration operation names it: a malformed one in the
message (`operation 2: …`), one on a path that is no file by
`error.details.index`, and every refusal from `declaration_not_found` to
`syntax_error` by `error.details.index` and `error.details.path`. A file
operation earlier in the request that does not apply is the answer
before any declaration operation after it. Nothing is committed and no
ref moves.

### `POST /api/v1/workspaces/{slug}/scm/refs`

Creates a branch or a tag at a commit and answers `201` with its ref
object.

| Member | Meaning |
|---|---|
| `name` | required: the ref name; a bare name creates a branch |
| `sha` | the full id of the commit to create the ref at |
| `ref` | a revision naming that commit |

Give exactly one of `sha` and `ref`. A tag created this way is
lightweight.

```json
{"name":"refs/tags/v1","ref":"main"}
```

| Error | When |
|---|---|
| `400` `invalid_request` | a missing or invalid name; both or neither of `sha` and `ref` |
| `403` | a push rule refuses the creation |
| `404` `ref_not_found` | the target names no commit |
| `409` `ref_exists` | the ref exists, or its name clashes with another's (`feat` and `feat/x`) |
| `409` `ref_moved` | another write created the ref in between |

### `PATCH /api/v1/workspaces/{slug}/scm/refs/{name}`

Moves a branch or a tag to another commit and answers its ref object.
`{name}` is read as on the `GET` route. Both members are required:

| Member | Meaning |
|---|---|
| `sha` | the full id of the commit to move the ref to |
| `expected_sha` | the full id of the commit you saw the ref at |

A move may go anywhere the push rules allow, backwards included; the push
event then has `forced` `true`. A move to the commit the ref is already
at answers `200` and emits no event. A ref that does not exist, or a
`sha` that names no commit, is `404` `ref_not_found`; a ref that is not
at `expected_sha` is `409` `ref_moved`.

### `DELETE /api/v1/workspaces/{slug}/scm/refs/{name}`

Deletes a branch or a tag and answers `204`. The optional query parameter
`expected_sha` deletes the ref only when it is at that commit; without it
the ref is deleted wherever it is. The commits stay in the repository.

| Error | When |
|---|---|
| `403` `default_branch`, `protected_branch`, `hub_owned_ref` | the ref may not be deleted |
| `404` `ref_not_found` | no ref has the name |
| `409` `ref_moved` | the ref is not at `expected_sha` |
| `409` `ref_in_use` | the branch is the source of a draft or open change |

### `POST /api/v1/workspaces/{slug}/scm/rebase`

Replays the commits of a branch that `onto` does not hold, oldest first,
on top of the commit `onto` names, and moves the branch to the result.

| Member | Meaning |
|---|---|
| `branch` | required: the branch to rebase |
| `onto` | required: a revision naming the commit to rebase onto |
| `expected_head_sha` | the commit you saw `branch` at; without it the rebase applies to the head it finds |
| `session_id` | the agent session the rebase belongs to |

```json
{"branch":"topic","onto":"main","expected_head_sha":"9cdbe993678a61933dbfc1b29496123ac2997ea0"}
```

Each new commit keeps the author, the author's time and the message of
the commit it replays; its committer is the hub. A commit that changes
nothing on top of `onto` is left out. The rebase is all or nothing: the
branch moves once, at the end, by compare-and-swap, so a branch that
received a commit while the rebase ran is `409` `ref_moved` and keeps
that commit.

```json
{"status":"rebased","branch":{"name":"refs/heads/topic","kind":"branch","sha":"6038b95a48a0ef29a63329c161332c1accd8e5e8","protected":false,"tracking":false,"change":null},"old_head_sha":"9cdbe993678a61933dbfc1b29496123ac2997ea0","new_head_sha":"6038b95a48a0ef29a63329c161332c1accd8e5e8","onto_sha":"b68c63f9e524e8f13781aff3a3e31a0c7ab1fe6e","change":null}
```

`status` is `rebased`, or `up_to_date` when the branch already holds
`onto`: then nothing moves and no event is emitted. `change` names the
draft or open change whose source the branch is, with the version the
rebase recorded (or, for `up_to_date`, its current version); `null` when
there is none.

A commit that does not apply ends the rebase with `409`
`rebase_conflict` and the [conflict report](#the-conflict-report) in
`error.details`. The branch is where it was.

| Status | When |
|---|---|
| `200 OK` | `rebased` or `up_to_date`; or a replay of an `Idempotency-Key` |
| `400` `invalid_request` | no `branch` or `onto`, or a malformed member |
| `400` `merge_commits_unsupported` | a commit to replay is a merge commit |
| `400` `unknown_session` | the `session_id` is not accepted |
| `403` | a push rule refuses the move, such as `protected_branch` |
| `404` `ref_not_found` | the branch does not exist, or `onto` names no commit |
| `409` `ref_moved` | the branch is not at `expected_head_sha`, or moved during the rebase |
| `409` `rebase_conflict` | a commit does not apply |

## Changes

A change is Parley's unit of work. It says: add the commits of this source
branch to that target branch, for this reason. A branch is still how
commits travel, but a branch is rewritten, force-pushed and deleted, and it
cannot say why it exists. The change is the durable object on top of it: it
keeps its number while the branch is rewritten, remembers every head the
branch had, records the reason for the work and the session that did it,
and is what policy evaluates, reviewers approve and [landing](#landing)
consumes. You reach for a change as soon as work on a branch should be
seen, reviewed or landed.

### What a change is

A change belongs to the workspace that holds its target branch, the target
workspace. It is numbered there from 1, and a number is never used twice,
also after its change was abandoned. Do not assume the numbers are
contiguous. Its reference is `<workspace_slug>#<number>`, such as `demo#1`.
The source branch is a branch of the target workspace or of a task fork of
it, so someone who can only read a workspace can still propose a change to
it from their own fork. One branch is the source of at most one draft or
open change.

```json
{"workspace_slug":"demo","number":1,"ref":"demo#1","title":"Add x","intent":"Because y","state":"open","target_branch":"main","source":{"workspace_slug":"demo","branch":"feat"},"version":1,"head_sha":"0b86db2bc73da0f067ec404140cfc00060162383","base_sha":"dc54adf9e0607cad9039738e34a4c3061a859f17","depends_on":null,"dependants":[],"author":{"actor_type":"api_key","actor_id":"bce9bb4d-cbcb-47c3-ab7a-d18f4b5ab95a","username":"owner"},"session_id":null,"sessions":[],"mergeable":"unknown","conflict":null,"needs_restack":false,"restack_reason":null,"stats":{"commits":1,"files_changed":1,"insertions":1,"deletions":0},"paths":["feat.txt"],"paths_truncated":false,"landing":{"status":"ready","reason":null,"requirements":[{"rule":"default","paths":["feat.txt"],"approvals":{"required":0,"counted":0,"approvers":["owner"]},"checks":[],"block":false,"satisfied":true}]},"auto_land":false,"labels":["docs"],"landed":null,"reverts":null,"reverted_by":null,"created_at":"2026-11-15T13:12:13Z","updated_at":"2026-11-15T13:12:13Z"}
```

| Member | Meaning |
|---|---|
| `title` | one line of 1 to 200 characters |
| `intent` | why the change exists, at most 16,000 characters, so that a reviewer reads why before what. It may be empty only on a draft, or when the workspace setting `changes.require_intent` is `false` |
| `state` | `draft`, `open`, `landed` or `abandoned` (see [States](#states)) |
| `target_branch`, `source` | where the commits go, and the workspace and branch they come from |
| `version`, `head_sha`, `base_sha` | the current version, its head, and the commit it is built on: the head of the change it depends on, or else the merge base of the head and the target branch. `base_sha` is `null` when the head shares no history with the target |
| `depends_on`, `dependants` | the change directly below this one in a stack, `null` for none, and the draft and open changes directly above it (see [Stacks](#stacks)) |
| `author` | who opened it: `actor_type` (`admin_token`, `api_key`, `pat` or `system`), `actor_id` and `username`, both `null` for the admin token and the hub |
| `session_id`, `sessions` | the session of the current version, `null` for none, and every session that contributed a version |
| `mergeable`, `conflict` | `clean`, `conflict` or `unknown`, and the conflict report while it is `conflict` (see [Mergeability](#mergeability)) |
| `needs_restack`, `restack_reason` | whether the change is not built on what it must sit on, and why: `conflict`, `auto_restack_off` or `stack_too_deep` (see [Restack](#restack)) |
| `stats` | `commits`, `files_changed`, `insertions` and `deletions` of the current version against its base |
| `paths`, `paths_truncated` | the files the current version touches, both sides of a rename, at most 2,000; `paths_truncated` is `true` when it touches more |
| `landing` | whether policy lets it land: `status` (`ready`, `pending` or `blocked`), the structural `reason` of a block (`draft`, `target_not_found`, `nothing_to_land`, `unrelated_history`, `merge_commits`) or `null`, and the policy's `requirements` (see [Landing policy, approvals and checks](#landing-policy-approvals-and-checks)) |
| `auto_land` | `true` has the hub land the change by itself once it is landable (see [Landing](#landing)) |
| `labels` | free-form labels, at most 20, each matching `^[a-z0-9][a-z0-9_.:/-]{0,62}$` |
| `landed` | how it landed, `null` until then: `sha`, `at`, `by`, `land_id` and `commits`, each `{version_sha, landed_sha}` |
| `reverts`, `reverted_by` | the change this one reverts, and the draft, open or landed change that reverts this one, `null` for none (see [Landing](#landing)) |

In a stack, `base_sha`, `stats`, `paths`, the diff and the policy
evaluation cover only the change's own commits, not those of the changes
below it.

### States

| From | To | What moves it |
|---|---|---|
| (new) | `draft` | opened with `draft: true` |
| (new) | `open` | opened otherwise |
| `draft` | `open` | `PATCH` with `{"state":"open"}`; the intent rule must hold |
| `open` | `draft` | `PATCH` with `{"state":"draft"}` |
| `open` | `landed` | a land succeeds (see [Landing](#landing)) |
| `draft`, `open` | `abandoned` | `POST …/abandon`; a git push that deletes the source branch; the deletion of the source task fork; a reclone of the source workspace that does not bring the branch back |
| `abandoned` | `open` | `POST …/restore` |

`landed` is final. A draft is work in progress: it records versions and
is checked for mergeability like an open change, but its `landing` is
`blocked` for `draft` and it never lands. An abandoned change keeps its
versions and its source branch, which is why it can be restored. Deleting
the target workspace deletes its changes.

### Versions

Every head the source branch has while the change is draft or open is a
version, numbered from 1. You do nothing to create one: the hub records it
as part of the write that moves the branch, whatever moves it, so a change
never misses a head. Moving the branch to the commit that is already the
current head records nothing. A version never changes afterwards.

| `kind` | The branch was moved by |
|---|---|
| `push` | a git push |
| `commit`, `ref`, `rebase` | a commit, a ref move or a rebase through the [source control](#source-control) writes. Version 1 of a change opened for a branch that already existed is a `ref` too |
| `restack` | a [restack](#restack), by the hub or on request |
| `revert` | a revert (see [Landing](#landing)) |
| `sync` | a sync from origin |

A version has `number`, `head_sha`, `base_sha`, `kind`, `created_by` (an
author object; `actor_type` `system` for the hub), `session_id`, `stats`,
`created_at` and `patch_equivalent_to`: the latest earlier version whose
diff against its own base is the same as this one's, or `null`. A pure
rebase is patch-equivalent to the version before it, which is how
approvals survive a rebase and how you see that nothing needs re-reading.

Old versions stay readable so that a reviewer can compare what they
approved with what is there now, and so that nothing is lost when a branch
is force-pushed or deleted. The head of each version is kept at
`refs/parley/changes/<number>/<version>` in the target workspace's
repository, also for a fork source, and you can fetch it with git. The
[diff route](#get-apiv1workspacesslugchangesnumberdiff) answers any
version, and the difference between two. The one exception is a reclone of
the workspace, which discards the version refs: the records stay, and
reading the diff of a version whose commit is gone answers `404`
`ref_not_found`.

If the hub stops between a branch move and its bookkeeping, it catches up
when it starts again, before it accepts requests: a branch at another
commit gets that commit as the next version, and a change whose branch is
gone is abandoned.

### Opening a change

There are three ways to open a change, all with the same result:

- **The API**: [`POST …/changes`](#post-apiv1workspacesslugchanges), for a
  branch that already exists on the hub.
- **A git push with push options**: `git push -o parley.change` opens a
  change for each pushed branch. `-o parley.target=<branch>`,
  `-o parley.title=<text>`, `-o parley.intent=<text>`, `-o parley.draft`
  and `-o parley.session=<id>` set its fields. A push into a task fork
  whose `auto_change` is `true` opens one without any option. See
  [Git over HTTP](#git-over-http).
- **An API commit** with a `change` object, or into a task fork whose
  `auto_change` is `true`. See [Source control](#source-control).

A later push or commit to the same branch never opens a second change: it
records the next version of the one that exists.

### Mergeability

`mergeable` tells you, before anyone tries to land, whether the change's
commits would rebase cleanly onto what the change sits on: the head of the
change it depends on, or else the head of the target branch. It is `clean`,
`conflict` (with the conflict report in `conflict`) or `unknown`.

The hub computes it in the background for every draft and open change,
without being asked. It resets to `unknown` whenever the answer may have
changed, and stays so until the new check has stored its result:

- the change gets a new version, is retargeted or is restored;
- the change it depends on gets a new version;
- its target branch moves, is created or is deleted, by a push, the API, a
  landing or a sync.

Checks of one change start at least `[changes].mergeable_debounce` apart
(default `5s`, see
[configuration.md](configuration.md#server-and-storage)), so on a busy
target `unknown` can last a few seconds. It also stays `unknown` while the
target branch does not exist, while a restack of the change is pending,
and after a check that failed or ran past 30 seconds, until the next
trigger. After a restart the hub checks every change that is `unknown`.

Two events let you follow it without polling (see
[Audit and events](#audit-and-events)):

- `parley.change.conflict` when a change goes into conflict; its metadata
  lists the conflicted `files` and says whether the change `needs_restack`.
  A change that stays in conflict is not announced again until it has been
  `clean` in between.
- `parley.change.ready` when an open change becomes landable: its
  `landing.status` is `ready` and `mergeable` is `clean`. It is announced
  once per head, and again after the change stopped being landable.

`mergeable` is advice. Landing never trusts the stored value; it always
rebases for real.

The conflict report names the commit the change was replayed onto
(`target_sha`), the change's `head_sha` and `base_sha`, the first commit
that did not apply (`commit_sha`), the conflicted `files` (each with
`path`, `kind`, the three sides `base`, `ours` and `theirs`, and
`landed_by`) and the `landed_changes` the change collided with. It is the
report a failed land gives; [Landing](#landing) describes its members.

### Stacks

Work often builds on work that has not landed yet. Rather than wait, you
branch from the first change's branch and open a second change. The second
change then depends on the first: its `depends_on` is the first change's
number, its `base_sha` is the first change's head, and its diff, stats and
policy evaluation cover only its own commits. A reviewer reads each change
on its own, and the hub keeps the upper one current while the lower one is
amended or lands.

The hub sets `depends_on` from the commit graph, at open and on every new
version: it is the nearest draft or open change of the same workspace and
target branch whose head is an ancestor of the new head, or `null` when
there is none. So you rarely set it yourself. When you do, at open or by
`PATCH`, and the branch is not built on that change, the hub restacks the
change onto it. A link you name at open holds for version 1; each later
version is linked from the commit graph again.

The stack of a change is its chain of `depends_on` links down to a change
with none, plus the draft and open changes above it. All changes of a
stack have the same target branch; they may come from different source
workspaces. A stack is at most `[changes].max_stack_depth` changes deep
(default 20). A version that puts a change deeper is still recorded, but
the change gets `needs_restack` with `restack_reason` `stack_too_deep`,
and the hub restacks neither it nor the changes above it until a link
below it changes and it is back within the limit.

A change in a stack cannot be retargeted, and abandoning a change that
others depend on asks you what to do with them (see
[abandon](#post-apiv1workspacesslugchangesnumberabandon)).

### Restack

A restack rebases a change's own commits onto the commit it must now sit
on, moves its source branch there and records the result as a version of
kind `restack`. The commit it must sit on is the head of the change it
depends on; once that change landed, the landed commit on the target; and
for a change with no dependency, the head of the target branch. Because
only the change's own commits are replayed, the old commits of a change
below that was amended, landed or abandoned do not come along.

The hub restacks by itself, in the background, when:

- **the change below gets a new version**: each change directly above is
  restacked onto the new head, and so on up the stack;
- **the change below lands**: each change directly above gets `depends_on`
  `null` and is restacked onto the landed commit; the changes further up
  keep their links and follow in turn;
- **the change below is abandoned** with `"dependants":"reparent"`;
- **you set `depends_on`** to a change the branch is not built on.

The push, commit or land that triggers a restack is answered without
waiting for it. Pending restacks run one at a time, in order; several for
one change collapse into one, onto the newest head below; and those
pending when the hub stops run after it starts again. A change that
already sits on the right commit gets no version. A hub restack is
recorded as created by `system` and keeps the session of the version it
replaces. Each restack writes `parley.change.version` with `kind`
`restack`, then `parley.change.restack` with `onto_sha` and the `trigger`
(`version`, `land`, `abandon` or `manual`).

A restack moves the source branch on the hub, so your local branch is then
behind: `git pull --rebase`, or fetch
`refs/parley/changes/<number>/<version>`, before you continue.

When the workspace setting `changes.auto_restack` is `false`, the hub
moves no branch. A change that would have been restacked gets
`needs_restack` with `restack_reason` `auto_restack_off` instead, and you
restack it with [`POST …/restack`](#post-apiv1workspacesslugchangesnumberrestack).

#### When a restack conflicts

Nothing moves: not the change's branch, and not the changes above it,
which keep their versions and links. The change gets `needs_restack`
`true` with `restack_reason` `conflict`, `mergeable` `conflict` with the
conflict report, and `parley.change.conflict` is written with
`needs_restack` `true`. You then have two ways out:

- **Fix it with git.** Fetch, rebase your branch onto the commit it must
  sit on and push. A version whose head contains that commit clears
  `needs_restack`, and the changes above are restacked in turn.
- **Fix it through the API.** Read the report with
  [`GET …/conflict`](#get-apiv1workspacesslugchangesnumberconflict), fetch
  each conflicted file with its markers from
  [`GET …/conflict/file`](#get-apiv1workspacesslugchangesnumberconflictfile),
  and send the merged content as `resolutions` to
  [`POST …/restack`](#post-apiv1workspacesslugchangesnumberrestack).

### Example: open, read, stack

The branch `feat` was pushed to the workspace `demo`. Open a change for
it; `target_branch` defaults to the workspace's default branch and `title`
to the subject of the head commit:

```sh
curl -sS -X POST "$HUB/api/v1/workspaces/demo/changes" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"source":{"branch":"feat"},"intent":"Because y","labels":["docs"]}'
```

The answer is `201` with the change object shown above: `demo#1`, `open`,
at version 1, `mergeable` still `unknown`. Read it a moment later, with
its versions:

```sh
curl -sS "$HUB/api/v1/workspaces/demo/changes/1?include=versions" \
  -H "Authorization: Bearer $TOKEN"
```

`mergeable` is now `clean` or `conflict`, and `landing.status` says
whether policy would let it land.

Now build on it. Branch `feat-2` from `feat`, commit, push it, and open a
second change. You do not name the first one: the hub sees that the head
of `feat-2` has the head of `demo#1` as an ancestor.

```sh
git checkout -b feat-2 feat
git commit -am "Add z"
git push origin feat-2

curl -sS -X POST "$HUB/api/v1/workspaces/demo/changes" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"source":{"branch":"feat-2"},"intent":"Because z needs x"}'
```

The new change `demo#2` has `"depends_on":1`, its `base_sha` is the head
of `demo#1`, and its `stats` count one commit. With git alone, the last
two steps are one: `git push -o parley.change origin feat-2`. Look at the
stack, bottom first:

```sh
curl -sS "$HUB/api/v1/workspaces/demo/changes/2/stack" \
  -H "Authorization: Bearer $TOKEN"
```

```json
{"target_branch":"main","changes":[{"number":1,"title":"Add x","state":"open","version":1,"head_sha":"…","mergeable":"clean","needs_restack":false,"restack_reason":null,"landing":{"status":"ready"}},{"number":2,"title":"Add z","state":"open","version":1,"head_sha":"…","mergeable":"clean","needs_restack":false,"restack_reason":null,"landing":{"status":"ready"}}]}
```

When you amend `feat` and push it, `demo#1` gets version 2 and the hub
restacks `demo#2` onto it: `demo#2` gets a version of kind `restack`, and
`feat-2` on the hub has moved. When `demo#1` lands, `demo#2` is restacked
onto the landed commit and its `depends_on` becomes `null`.

### Access to the change routes

The rules of [Using the API](#using-the-api) apply. On top of them:

- **Reading** needs `changes:read` and read access to `{slug}`.
- **Opening** needs `changes:write`, read access to `{slug}` and write
  access to the source workspace.
- **Modifying** (`PATCH`, abandon, restore, restack) needs `changes:write`
  and read access to `{slug}`, and the caller must be the user who opened
  the change (with any of their credentials), someone with write access to
  `{slug}`, or the admin token. Anyone else who reads `{slug}` gets `403`
  `forbidden`.
- A number that names no change the request may reach answers `404`
  `change_not_found`.
- A task fork holds no changes of its own: they belong to its parent. On a
  fork's slug only the list works, and holds the changes whose source is
  the fork.
- A token bound to a task fork reaches these routes on the fork's parent,
  and there only the changes whose source workspace is that fork. Every
  other change answers `404` `change_not_found`, and opening with another
  source `403` `token_bound`.
- A path-scoped token is held to its paths wherever it would create a
  version or arm a landing: opening, turning `auto_land` on and a manual
  restack answer `403` `path_not_allowed`, with the first offending file
  in `error.path`, when the version touches a file outside them.
- The writes answer `409` `workspace_busy` while the workspace is pending,
  cloning or under a reclone, archive or delete, `409`
  `workspace_not_ready` when its clone failed and `409`
  `workspace_archived` when it is archived.

### Change routes

All routes are under `/api/v1/workspaces/{slug}`, the target workspace.

| Route | Scope | What it does |
|---|---|---|
| [`POST …/changes`](#post-apiv1workspacesslugchanges) | `changes:write` | opens a change |
| [`GET …/changes`](#get-apiv1workspacesslugchanges) | `changes:read` | lists changes, with filters |
| [`GET …/changes/{number}`](#get-apiv1workspacesslugchangesnumber) | `changes:read` | reads one change |
| [`GET …/changes/by-commit/{sha}`](#get-apiv1workspacesslugchangesby-commitsha) | `changes:read` | finds the change a commit belongs to |
| [`GET …/changes/{number}/versions`](#get-apiv1workspacesslugchangesnumberversions) | `changes:read` | lists the versions of a change |
| [`GET …/changes/{number}/versions/{version}`](#get-apiv1workspacesslugchangesnumberversionsversion) | `changes:read` | reads one version |
| [`GET …/changes/{number}/diff`](#get-apiv1workspacesslugchangesnumberdiff) | `changes:read` | the diff of a version, or what moved between two |
| [`PATCH …/changes/{number}`](#patch-apiv1workspacesslugchangesnumber) | `changes:write` | edits title, intent, labels, `auto_land`, state, target or dependency |
| [`POST …/changes/{number}/abandon`](#post-apiv1workspacesslugchangesnumberabandon) | `changes:write` | abandons a change |
| [`POST …/changes/{number}/restore`](#post-apiv1workspacesslugchangesnumberrestore) | `changes:write` | reopens an abandoned change |
| [`GET …/changes/{number}/conflict`](#get-apiv1workspacesslugchangesnumberconflict) | `changes:read` | the change's conflict as it is now |
| [`GET …/changes/{number}/conflict/file`](#get-apiv1workspacesslugchangesnumberconflictfile) | `changes:read` | one conflicted file with conflict markers |
| [`GET …/changes/{number}/stack`](#get-apiv1workspacesslugchangesnumberstack) | `changes:read` | the stack a change is part of |
| [`POST …/changes/{number}/restack`](#post-apiv1workspacesslugchangesnumberrestack) | `changes:write` | restacks a change now, optionally resolving conflicts |

Landing, reverting, approvals and checks of a change are under
[Landing](#landing) and
[Landing policy, approvals and checks](#landing-policy-approvals-and-checks);
its overlaps are under [Awareness](#awareness).

### `POST /api/v1/workspaces/{slug}/changes`

Opens a change into a branch of `{slug}`.

| Member | Meaning |
|---|---|
| `source` | required: `{"workspace_slug"?, "branch"}`; `workspace_slug` defaults to `{slug}`, and is otherwise a task fork of it |
| `target_branch` | defaults to the fork's base branch for a fork source, else to the workspace's default branch |
| `title` | defaults to the subject of the head commit, cut to 200 characters |
| `intent` | defaults to the body of the head commit, then to the purpose of the source fork. Required unless `draft` is `true` or the target's `changes.require_intent` is `false` |
| `draft` | `true` opens the change as a draft |
| `depends_on` | the number of a draft or open change of `{slug}` with the same target branch, in place of the link the commit graph gives (see [Stacks](#stacks)) |
| `auto_land` | defaults to `false` |
| `labels` | defaults to `[]`; a repeated label is kept once |
| `session_id` | defaults to the session recorded on the source fork; it must be a session the caller may link to (see [Sessions and telemetry](#sessions-and-telemetry)) |

The answer is `201` with the change object at version 1, the head of the
source branch. `mergeable` is `unknown` until the first check; `landing`
is already evaluated. The open writes `parley.change.version` and, unless
the change is a draft, `parley.change.open`. A refused open leaves
nothing behind. The route takes an `Idempotency-Key`.

A change may target any branch, protected or not, but the source must
bring something a plain rebase can land:

| Status | When |
|---|---|
| `400` `invalid_request` | a malformed body, title, intent or label; no `source.branch`; the source is the target branch itself |
| `400` `intent_required` | no intent, the change is not a draft and the target requires one. Send an intent or open a draft |
| `400` `target_not_found` | `{slug}` has no branch `target_branch` |
| `400` `source_not_related` | the source workspace does not exist, is not visible to you, or is neither `{slug}` nor a fork of it |
| `400` `source_not_found` | the source workspace has no such branch. Push it first |
| `400` `nothing_to_land` | the source branch has no commit the target lacks |
| `400` `unrelated_history` | the source branch shares no history with the target |
| `400` `merge_commits_unsupported` | a merge commit is among the commits to land. Rebase the branch to a linear history |
| `400` `invalid_dependency` | `depends_on` names no change of `{slug}`, one into another target branch, or one that is neither draft nor open |
| `400` `unknown_session` | the `session_id` is not accepted |
| `403` `forbidden` | you read the source workspace but cannot write it |
| `403` `integration_branch` | the target is the integration branch of a carry-patch workspace, which the hub rebuilds. Target a patch branch |
| `409` `change_exists` | another draft or open change has this source; `details` is `{"workspace_slug", "number", "ref"}` of that change. Push to it instead |

### `GET /api/v1/workspaces/{slug}/changes`

Lists the changes of `{slug}` as a paged list. Each item is the change
object without `conflict`, and without `paths` and `paths_truncated`
unless you ask with `include=paths`.

Without filters you get the draft and open changes, the most recently
updated first. `sort=number` orders by number ascending instead. The
filters combine:

| Parameter | Keeps the changes |
|---|---|
| `state` | in this state; repeatable. Default: `draft` and `open` |
| `target_branch` | with this target branch |
| `source_workspace` | whose source branch is in this workspace |
| `author` | opened by this user id; `me` is the caller (not available to the admin token) |
| `session_id` | to which this session contributed a version |
| `label` | that carry this label |
| `path` | that touch a file this glob matches (`*` within a path segment, `**` across segments) |
| `mergeable` | `clean`, `conflict` or `unknown` |
| `landing_status` | `ready`, `pending` or `blocked` |
| `stack_of` | of the stack of this change number |

`stack_of` lists the whole stack bottom first, in every state and whatever
`state` and `sort` say, every branch of it included; the other filters
still apply. A `stack_of` that names no change you may see answers `404`
`change_not_found`. A value outside a filter's set answers `400`
`invalid_request`.

### `GET /api/v1/workspaces/{slug}/changes/{number}`

Answers `200` with the change object. `include`, a comma list, adds
members of the same name:

| `include` | Adds |
|---|---|
| `versions` | the versions, oldest first |
| `stack` | the stack, as [`GET …/stack`](#get-apiv1workspacesslugchangesnumberstack) answers it |
| `approvals`, `checks` | the verdicts and the check results that apply to the current version (see [Landing policy, approvals and checks](#landing-policy-approvals-and-checks)) |
| `overlaps` | the change's overlap entries (see [Awareness](#awareness)) |

Any other name answers `400` `invalid_request`.

### `GET /api/v1/workspaces/{slug}/changes/by-commit/{sha}`

Finds the change that a commit belongs to: the one that landed `{sha}`, or
that has it as the head of a version. Use it to go from a commit on the
target branch back to its change and session. `{sha}` is a full
40-character commit id.

The answer is `200` with
`{"change": <change object>, "version": <int or null>, "landed": <bool>}`.
`landed` is `true` for a commit the change landed, and `version` is then
the version it was landed from; otherwise `version` is the highest version
with this head. A change that landed the commit wins over one that only
has it as a version head. A commit that is neither, an inner commit of a
version included, answers `404` `change_not_found`.

### `GET /api/v1/workspaces/{slug}/changes/{number}/versions`

Answers the versions of the change, oldest first, as a paged list of
version objects (see [Versions](#versions)).

### `GET /api/v1/workspaces/{slug}/changes/{number}/versions/{version}`

Answers one version object plus `ref`, the name of its version ref to
fetch with git. `{version}` is a number or `latest`.

```json
{"number":2,"head_sha":"…","base_sha":"…","kind":"push","patch_equivalent_to":null,"created_by":{"actor_type":"api_key","actor_id":"…","username":"owner"},"session_id":null,"stats":{"commits":2,"files_changed":1,"insertions":1,"deletions":0},"created_at":"2026-11-15T13:12:13Z","ref":"refs/parley/changes/1/2"}
```

A version number the change does not have answers `404` `not_found`.

### `GET /api/v1/workspaces/{slug}/changes/{number}/diff`

Answers the diff of one version, or, with `against`, the interdiff of two
versions: what moved between them, with what a rebase brought in left out.
The interdiff is how you re-read only what changed since your last review.

| Parameter | Meaning |
|---|---|
| `version` | a version number or `latest` (the default) |
| `against` | a version number: answer the interdiff of `version` and `against` |
| `path` | a glob, repeatable: only matching files are answered and counted |
| `patches` | `true` (the default) or `false`: leave the `patch` texts out |
| `format` | `json` (the default) or `patch` |

Without `against`, the answer is the diff from the version's `base_sha` to
its `head_sha`, in the shape and with the limits of the diff route of
[Source control](#source-control) (5,000 files, 8 MiB of patch text, then
`truncated: true`), plus `version` and `against` (`null`).

With `against`, every file that either version touches is listed with
`changed`: `true` when its patch differs between the two versions or only
one of them touches it. A changed file carries the `patch` from the head
of `against` to the head of `version`; an unchanged file has no `patch`.
`stats` count the changed files only. Two patch-equivalent versions have
no changed file, so a pure rebase reads as empty.

```json
{"base_sha":"dd626b3e…","head_sha":"8adab83c…","merge_base_sha":null,"files":[{"path":"a.txt","old_path":null,"status":"modified","insertions":1,"deletions":1,"binary":false,"old_sha":"…","new_sha":"…","changed":false},{"path":"b.txt","old_path":null,"status":"modified","insertions":1,"deletions":0,"binary":false,"old_sha":"…","new_sha":"…","patch":"diff --git a/b.txt b/b.txt\nindex …\n--- a/b.txt\n+++ b/b.txt\n@@ -2,3 +2,4 @@ b1\n b2\n b3\n b4\n+b5\n","changed":true}],"stats":{"files_changed":1,"insertions":1,"deletions":0},"truncated":false,"version":3,"against":1}
```

With `format=patch` the answer is the patch text alone, as
`Content-Type: text/x-diff`; a cut answer carries
`X-Parley-Truncated: true`.

| Status | When |
|---|---|
| `400` `invalid_request` | a `version`, `against`, `format` or `patches` outside its values |
| `404` `not_found` | a version number the change does not have |
| `404` `ref_not_found` | the commits of the version are no longer in the repository, such as after a reclone |

### `PATCH /api/v1/workspaces/{slug}/changes/{number}`

Edits a change. The body names the members to change; the others keep
their value. Every member is checked before anything is stored, so a body
is applied whole or not at all. The answer is `200` with the change
object.

| Member | Effect |
|---|---|
| `title`, `intent` | replace the text |
| `labels` | replaces the whole list |
| `auto_land` | `true` or `false`. Turning it on records you as the one who set it, and a change that is already landable gets a land attempt (see [Landing](#landing)) |
| `state` | `"open"` marks a draft ready: `landing` is evaluated without the `draft` block and `parley.change.open` is written. `"draft"` turns an open change back into a draft |
| `target_branch` | retargets a draft or open change that is not part of a stack |
| `depends_on` | a change number links a draft or open change to it; `null` unlinks it |

A retarget records no version. The change's `base_sha`, `stats` and
`paths` are measured against the new target, `landing` is evaluated again,
`mergeable` is checked again, and `parley.change.version` is written with
`kind` `retarget`. This is also how you rescue a change whose target
branch was deleted.

Setting `depends_on` restacks the change onto the named change when its
branch is not already built on it. Clearing it measures the change against
the target branch again. Either way `mergeable` is checked again.

An abandoned change takes no edit; a landed one only an edit of `labels`.

| Status | When |
|---|---|
| `400` `invalid_request` | a member of the wrong type or outside its limits; a `target_branch` that is the change's own source branch |
| `400` `intent_required` | the edit would leave the change open with an empty intent while the workspace requires one |
| `400` `target_not_found`, `403` `integration_branch` | as at open, for a new `target_branch` |
| `400` `invalid_dependency` | `depends_on` names no draft or open change of `{slug}` with the same target, the change itself, or a change that already depends on it |
| `409` `conflict` | a retarget of a change that depends on another or has dependants. Unlink it first |
| `409` `change_closed` | the change is abandoned, or landed and the body edits more than `labels` |

### `POST /api/v1/workspaces/{slug}/changes/{number}/abandon`

Abandons a draft or open change. The source branch, the versions and
their refs stay, so the change can be restored. The body is optional:

| Member | Meaning |
|---|---|
| `reason` | free text, recorded on the `parley.change.abandon` event |
| `dependants` | what happens to the changes above it in a stack: `"reparent"` or `"abandon"` |

With `"reparent"`, each change directly above now depends on what the
abandoned change depended on, and is restacked without the abandoned
change's commits. With `"abandon"`, every draft or open change above it
in the stack is abandoned too, with the same reason. The answer is `200`
with the change object.

| Status | When |
|---|---|
| `409` `has_dependants` | draft or open changes depend on this one and the body has no `dependants`; the error's `dependants` member lists their numbers. Nothing changed: repeat the call with a choice |
| `409` `change_closed` | the change is already abandoned or landed |

### `POST /api/v1/workspaces/{slug}/changes/{number}/restore`

Brings an abandoned change back to `open` under its number, at its last
version. It takes no body. If the source branch no longer exists, it is
created again at the head of the last version; if it exists at another
commit, that commit is recorded as a new version. `landing` and
`mergeable` are evaluated again, and `parley.change.restore` is written. A
change that was a draft when it was abandoned comes back `open`.

| Status | When |
|---|---|
| `409` `change_not_abandoned` | the change is draft, open or landed |
| `409` `source_workspace_gone` | the source task fork no longer exists. The versions stay fetchable: open a new change from a branch at the last version's head |
| `409` `change_exists` | another draft or open change now has this source |
| `409` `conflict` | the source branch is gone and so is the head commit of the last version, as after a reclone |
| `400` `intent_required` | the intent is empty while the workspace requires one |

### `GET /api/v1/workspaces/{slug}/changes/{number}/conflict`

Answers `200` with the change's conflict as it is now:
`{"mergeable": …, "conflict": …}`. Unlike the change object's `mergeable`,
this is never `unknown`: when the stored result does not cover the present
head and the commit the change sits on, the route rebases on trial before
it answers. Use it when you need the answer now. It stores nothing on the
change and writes no event. A landed or abandoned change answers what it
holds.

```json
{"mergeable":"conflict","conflict":{"target_sha":"…","head_sha":"…","base_sha":"…","commit_sha":"…","files":[{"path":"a.txt","kind":"content","base":{"sha":"…","mode":"100644","path":"a.txt"},"ours":{"sha":"…","mode":"100644","path":"a.txt"},"theirs":{"sha":"…","mode":"100644","path":"a.txt"},"landed_by":[]}],"landed_changes":[],"change":null}}
```

| Status | When |
|---|---|
| `409` `conflict` | the target branch of a draft or open change does not exist, so there is nothing to rebase onto |
| `400` `merge_commits_unsupported` | a merge commit is among the commits to replay |

### `GET /api/v1/workspaces/{slug}/changes/{number}/conflict/file`

Answers one file of the change's current conflict report, merged as text
with conflict markers, as `text/plain`. `path`, a required query
parameter, names the file. Lines that both sides keep, or that only one
side changed, appear once. Each region both changed is written between
`<<<<<<< target` (the target as replayed so far), `=======` and
`>>>>>>> change` (the change's commit that did not apply). There is no
base section.

```text
one
<<<<<<< target
TARGET
=======
CHANGE
>>>>>>> change
three
four
```

Edit the text into the merged file and send it as a resolution to
[`POST …/restack`](#post-apiv1workspacesslugchangesnumberrestack). The
route refuses as [`GET …/conflict`](#get-apiv1workspacesslugchangesnumberconflict)
does, and:

| Status | When |
|---|---|
| `400` `invalid_request` | no `path`, or an empty one |
| `404` `path_not_found` | the current report has no conflict in `path`; every path of a clean change |
| `409` `not_text_conflict` | the file's `kind` is not `content` (a binary file, a delete against a modify, two different added files and so on), so there is no text to mark. Resolve it with `content_base64` or `delete` |

### `GET /api/v1/workspaces/{slug}/changes/{number}/stack`

Answers the stack the change is part of:
`{"target_branch": …, "changes": […]}`, bottom first. It holds the chain
of `depends_on` links below the change, the change itself, and every
draft or open change above it; landed and abandoned changes above it are
left out. Where a change has several dependants, asking for one of them
lists the chain below it and the changes above it, not the sibling
branches; the `stack_of` filter of
[the list](#get-apiv1workspacesslugchanges) gives all of them. Each entry
is a summary with `number`, `title`, `state`, `version`, `head_sha`,
`mergeable`, `needs_restack`, `restack_reason` and `landing.status`. The
[example](#example-open-read-stack) shows an answer.

### `POST /api/v1/workspaces/{slug}/changes/{number}/restack`

Restacks the change now, as the hub does in the background, and answers
with the outcome in the same call. Use it when `changes.auto_restack` is
off, to bring a change with no dependency up to the head of its target
branch, or to resolve a conflicting restack. The body is optional:

| Member | Meaning |
|---|---|
| `session_id` | the session of the version the restack records; defaults to the session of the version it replaces |
| `resolutions` | the content of conflicted files, a list of `{"path", …}` with exactly one of `content` (text), `content_base64` (bytes) or `delete` (`true`: no file at the path) |

```json
{"resolutions": [
  {"path": "app.txt", "content": "one\nMERGED\nthree\n"},
  {"path": "logo.bin", "content_base64": "AAEC"},
  {"path": "old.txt", "delete": true}
]}
```

When a commit does not apply and every conflicted path of that commit has
a resolution, the commit is replayed with those paths replaced by your
content and the replay goes on. A path that conflicts at several commits
gets its resolution at each; a resolution for a path that does not
conflict changes nothing. One conflicted path without a resolution ends
the restack as a conflict.

| Status | When |
|---|---|
| `200` `{"status":"restacked","change":…}` | the source branch moved. The new version is of kind `restack`, created by you; `needs_restack` is cleared unless its reason is `stack_too_deep`; the changes above are restacked in turn |
| `200` `{"status":"up_to_date","change":…}` | the change already sits on the right commit. No version is recorded; a `needs_restack` for `conflict` or `auto_restack_off` is cleared |
| `409` `rebase_conflict` | a commit does not apply and a conflicted path has no resolution. `details` is the conflict report, and the change is left as a conflicting background restack leaves it. Send resolutions for every path of `details.files` |
| `409` `ref_moved` | the source branch kept moving under the restack; `details.current_sha` is its head. Try again |
| `409` `change_closed` | the change is landed or abandoned |
| `400` `target_not_found` | the change depends on nothing and its target branch does not exist |
| `400` `merge_commits_unsupported` | a merge commit is among the change's own commits |
| `400` `invalid_request`, `unknown_session` | a malformed `resolutions` list (a bad path, two entries for one path, none or several of the three content members), or a `session_id` that is not accepted |

## Landing

Landing adds the commits of a [change](#changes) to its target branch. With
many agents working on one repository, a merge queue makes every change wait
behind every other one. Parley lands optimistically instead: the hub replays
the change onto the current head of the target, on the server, and moves the
branch with a compare-and-swap. There is no queue and no lock per workspace
or branch. Two changes that do not conflict land at the same time, in either
order, and neither caller retries: when the target moved in between, the hub
replays onto the new head and tries again by itself. Safety comes from
[policy](#landing-policy-approvals-and-checks) before the land, and from the
record after it: every land is an event, every landed commit maps back to
its change, and a landed change can be [reverted](#post-apiv1workspacesslugchangesnumberrevert)
with one call.

### How a land works

Every request to land is recorded as a **land attempt**. An attempt lands
the version each change had when the attempt was created, starts `running`
and ends in one of four final states, after which it never changes:

| Status | Meaning | The target branch |
|---|---|---|
| `landed` | the commits are on the target; each change is `landed` | moved to `landed_sha` (unchanged when `noop`) |
| `conflict` | a commit did not apply on the target head | untouched; the change stays `open` |
| `blocked` | policy, or a guard of the target branch, did not let it pass | untouched; the change stays `open` |
| `failed` | the hub could not finish: see `error` | untouched; the change stays `open` |

What the attempt does, as far as you can observe it:

1. **Policy.** Each covered change is judged, bottom first, at the version
   being landed. The first change that is not `ready` (and that an
   `override` does not let pass) ends the attempt `blocked`.
2. **Replay.** The change's own commits are replayed onto the target head.
   With the workspace setting `land.strategy` `rebase` (the default) each
   commit lands as a commit: author, author date and message are kept, the
   committer is the hub's landing identity, and a commit that already sits
   on the target head keeps its id. With `squash` each change lands as one
   commit whose message is the change's title, its intent, and the trailers
   `Parley-Change: <slug>#<number>`, `Parley-Session: <id>` when the change
   has a session, and one `Co-authored-by:` per other author. A commit the
   target already holds is left out. A commit that does not apply ends the
   attempt `conflict`.
3. **Move.** The target moves from `onto_sha` to `landed_sha` by
   compare-and-swap. This is not a push: push rules and branch protection
   do not apply. When another write moved the target first, the hub replays
   onto the new head and tries again, up to `[land].max_attempts` times (10
   by default), then ends the attempt `failed` with `error` `contention`.
   A land never creates its target branch.
4. **Record.** Each change becomes `landed`, and the hub writes one
   `parley.change.land` event per change and one `parley.git.push` (with
   `via` `land`) for the branch. An attempt that ends `conflict`, `blocked`
   or `failed` writes one `parley.change.land_fail` with its `reason`
   instead.

After a land the hub tidies up. It deletes the source branch of each landed
change, when the workspace setting `land.delete_source` is `true` (the
default), the branch still sits at the landed version and no other draft or
open change uses it. It checks the mergeability of every other change on
that target again, and restacks the changes stacked on the landed one. On a
carry-patch workspace a land onto a patch branch also queues a rebuild, and
the workspace's push control may refuse the move (see
[Carry-patch workspaces](#carry-patch-workspaces)). A land touches only the
hub's repositories: publishing a landed branch to origin is a push of its
own. The workspace settings named here are described under
[Workspaces](#workspaces), the `[land]` keys in
[the configuration](configuration.md#the-configuration-file).

**Guarantees.** A land never half-applies. If the hub stops during a land,
it comes back either with the target at its old head and the change `open`,
or with the target moved, the change `landed` and the events written once.
Attempts still `running` at a stop are run again after the start, with the
same `id` and the versions they were created with. Up to `[land].workers`
attempts (8 by default) are computed at once; further ones wait as
`running`. An attempt not finished `[land].timeout` (5 minutes by default)
after it was created ends `failed` with `error` `timeout`. Attempts are kept
for the life of their workspace.

### Worked example: land, and act on a conflict

Land change 7 of `demo` and wait up to 30 seconds for the result:

```sh
curl -sS -X POST "$PARLEY_URL/api/v1/workspaces/demo/changes/7/land?wait=30" \
  -H "Authorization: Bearer $PARLEY_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"expected_version": 2}'
```

`200` with the attempt means it landed:

```json
{"id":"0f9a7f8e-5c1b-4d6e-9a3b-1c2d3e4f5a6b","workspace_slug":"demo",
 "target_branch":"main","changes":[7],"status":"landed","trigger":"manual",
 "strategy":"rebase","attempts":1,
 "onto_sha":"dc54adf9e0607cad9039738e34a4c3061a859f17",
 "landed_sha":"7c1e9a2b3d4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b","noop":false,
 "commits":[{"change":7,
   "version_sha":"0b86db2bc73da0f067ec404140cfc00060162383",
   "landed_sha":"7c1e9a2b3d4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b"}],
 "conflict":null,"blocked":null,"error":null,"override":null,
 "policy_revision":0,
 "requested_by":{"actor_type":"api_key",
   "actor_id":"bce9bb4d-cbcb-47c3-ab7a-d18f4b5ab95a","username":"owner"},
 "session_id":null,"created_at":"2026-11-16T09:00:00Z",
 "finished_at":"2026-11-16T09:00:01Z"}
```

`202` with the attempt still `running` means the wait ran out. The attempt
goes on whatever becomes of your call; read it until its `status` is final:

```sh
curl -sS "$PARLEY_URL/api/v1/workspaces/demo/lands/0f9a7f8e-5c1b-4d6e-9a3b-1c2d3e4f5a6b" \
  -H "Authorization: Bearer $PARLEY_TOKEN"
```

`409` `land_conflict` carries the attempt in `error.details`, and the
attempt's `conflict` is the conflict report:

```json
{"target_sha":"…","head_sha":"…","base_sha":"…","commit_sha":"…",
 "files":[{"path":"api/handler.go","kind":"content",
   "base":{"sha":"…","mode":"100644","path":"api/handler.go"},
   "ours":{"sha":"…","mode":"100644","path":"api/handler.go"},
   "theirs":{"sha":"…","mode":"100644","path":"api/handler.go"},
   "landed_by":[5]}],
 "landed_changes":[{"number":5,"title":"…","landed_sha":"…",
   "session_id":null}],
 "change":null}
```

Nothing changed on the target. To act on it:

1. Read `landed_changes` and each file's `landed_by`: these are the changes
   that landed since your base and touched the conflicted files, so you know
   what you collided with and can read their intent and diff.
2. For a file whose `kind` is `content`, fetch it with conflict markers from
   `GET …/changes/7/conflict/file?path=api/handler.go` (see
   [Changes](#changes)). Other kinds (`add_add`, `modify_delete`,
   `delete_modify`, `rename`, `mode`, `binary`, `submodule`) have no text to
   mark: decide from `base`, `ours` (the target) and `theirs` (your change),
   each the blob id, mode and path on that side, or `null` where the file
   does not exist.
3. Put your change on the new target head with the conflict resolved, by a
   git rebase and push or through the [source control](#source-control)
   routes. That records a new version of the change.
4. Land again.

### The land attempt

Every member is always present, `null` where the attempt has no value.

| Member | Meaning |
|---|---|
| `id` | the attempt's UUID |
| `workspace_slug`, `target_branch` | where the attempt lands |
| `changes` | the numbers of the changes it lands, bottom first |
| `status` | `running`, `landed`, `conflict`, `blocked` or `failed` |
| `trigger` | `manual`, or `auto_land` for [automatic landing](#automatic-landing) |
| `strategy` | `rebase` or `squash`, the workspace's `land.strategy` when the attempt was created |
| `attempts` | how many times the hub tried to move the target |
| `onto_sha`, `landed_sha` | the target head the commits were landed on, and the head the land made |
| `noop` | `true` when every commit was on the target already: the target did not move, and the change is `landed` at that head |
| `commits` | each landed commit: `change`, `version_sha` (the commit of the version) and `landed_sha` (the commit it landed as); one entry per change with `squash` |
| `conflict` | the conflict report of a `conflict` attempt (below) |
| `blocked` | for a `blocked` attempt: the `change` that stopped it, its landing `status` and its `requirements` (see [The landing of a change](#the-landing-of-a-change)) |
| `error` | why a `failed` attempt failed: `contention`, `timeout`, `origin_rejected` or `internal` |
| `override` | `{"reason": …}` when an override let at least one change pass |
| `policy_revision` | the revision of the policy that judged the attempt |
| `requested_by` | who asked: `actor_type` (`admin_token`, `api_key` or `pat`), `actor_id` and `username` |
| `session_id` | the session the land was asked in |
| `created_at`, `finished_at` | when it was created and when it ended |

The **conflict report** has the same shape wherever the hub replays commits
(a land, a restack, a rebase, a comparison, a revert):

| Member | Meaning |
|---|---|
| `target_sha` | the target head the attempt replayed onto |
| `head_sha`, `base_sha` | the head and the base of the version being landed |
| `commit_sha` | the first commit that did not apply |
| `files` | one entry per conflicted file: `path`, `kind`, `base`, `ours`, `theirs` and `landed_by`, the numbers of the landed changes that changed the file |
| `landed_changes` | each change landed between `base_sha` and `target_sha` that changed a conflicted file: `number`, `title`, `landed_sha`, `session_id`. Empty when nothing can be attributed, for example after a direct push |
| `change` | in a stack land, the change whose commit did not apply; else `null` |

A conflicting land also sets the change's `mergeable` to `conflict` with
the same report, and writes `parley.change.conflict` when the change was not
in conflict already.

### Landing a stack

Without `stack`, a change lands only when every change below it has landed
(`409` `depends_on_unlanded` otherwise). With `"stack": true` one attempt
covers the change and every unlanded change below it. The hub replays them
bottom first, each onto the one below, and moves the target once. It is all
or nothing: a conflict or a policy answer other than `ready` in any covered
change ends the attempt with nothing landed, and `conflict.change` or
`blocked.change` names the change that stopped it. You must be allowed to
land every change the stack covers.

### Automatic landing

Set `auto_land` on a change (when you open it or later, see
[Changes](#changes)) and the hub lands it by itself as soon as it is
landable: `open`, its landing `ready` and its `mergeable` `clean`. You do
not poll: the attempt has `trigger` `auto_land`, and its events tell you the
outcome.

- The hub lands one change at a time, never a stack. A change above waits
  until the change below has landed, and is then landed in turn.
- The attempt runs with the rights of the user who set the flag, checked at
  that moment: the user must still exist, not be blocked, and be the owner
  or (while `land.by_author` is on) the author. A flag set with a
  [path-limited token](#path-limits) holds every file of the change to that
  token's paths. When the check fails, the hub clears the flag and writes
  `parley.change.land_fail` with `reason` `not_permitted`.
- After `landed`, `conflict` or `blocked` the flag stays as it is: once the
  change becomes landable again, for example after you push a resolution or
  a reviewer approves, a new attempt starts.
- After `failed` the hub retries with a growing delay (2 s, doubling, capped
  at 300 s), at most 10 times, then clears the flag. Pending retries survive
  a restart.

### Path limits

A PAT may be limited to path globs. Such a token lands, reverts or sets
`auto_land` only on changes all of whose files (both paths of a rename)
match one of its globs. The first file outside them is answered `403`
`path_not_allowed` with the file in `error.path`, whatever the policy says.

### Errors: retry, fix, or ask a person

| Answer | What it means | What to do |
|---|---|---|
| `202`, attempt `running` | the wait ran out | read `GET …/lands/{id}` until it is final |
| `409` `land_conflict` | a commit did not apply | resolve as in the worked example, then land again |
| `409` `version_mismatch` | the change is not at `expected_version`, or got a version while the request was checked | read the change again, then land again |
| `409` `land_in_progress` | another attempt covering the change is running; it is in `error.details` | wait for that attempt instead of starting one |
| `409` `depends_on_unlanded` | a change below has not landed | land it first, or land with `stack` |
| `409` `change_not_open` | a covered change is `draft`, `landed` or `abandoned` | open the draft; otherwise there is nothing to land |
| `409` `policy_blocked`, `blocked.status` `pending` | approvals or checks are missing; `blocked.requirements` says which | wait for the checks, ask the listed approvers, or set `auto_land` and let the hub land it |
| `409` `policy_blocked`, `blocked.status` `blocked` | a rule blocks the paths, the change has a structural cause (`blocked.status` `blocked` with a `reason` on its landing), or push control of a carry-patch workspace refused the move | ask a person: no approval or override lifts it |
| `503` `land_failed`, `error` `contention` or `timeout` | the target was too busy, or the land took too long | land again |
| `503` `land_failed`, `error` `origin_rejected` or `internal` | origin refused the branch, or something is wrong with the change, the target branch or the hub | ask the workspace owner or the operator |
| `409` `workspace_busy`, `workspace_not_ready` | the workspace cannot take a land now | try again later |
| `403` `forbidden`, `token_bound`, `path_not_allowed`, `override_not_allowed`; `409` `workspace_archived` | this credential may not land this change here | ask a person with the rights |

The routes follow the shared rules in [Using the API](#using-the-api).

| Route | Scope | What it does |
|---|---|---|
| [`POST /api/v1/workspaces/{slug}/changes/{number}/land`](#post-apiv1workspacesslugchangesnumberland) | `changes:write` | lands a change or a stack |
| [`GET /api/v1/workspaces/{slug}/changes/{number}/lands`](#reading-land-attempts) | `changes:read` | lists the attempts that cover a change |
| [`GET /api/v1/workspaces/{slug}/lands`](#reading-land-attempts) | `changes:read` | lists the attempts of a workspace |
| [`GET /api/v1/workspaces/{slug}/lands/{id}`](#reading-land-attempts) | `changes:read` | reads one attempt |
| [`POST /api/v1/workspaces/{slug}/changes/{number}/revert`](#post-apiv1workspacesslugchangesnumberrevert) | `changes:write` | opens a change that undoes a landed one |

### `POST /api/v1/workspaces/{slug}/changes/{number}/land`

Lands the change on its target branch. `{slug}` is the workspace that holds
the target branch. The route accepts an `Idempotency-Key`.

**Who may call it:** the admin token, the owner of `{slug}`, and, while the
workspace setting `land.by_author` is `true` (the default), the change's
author with any credential of theirs; with `stack`, the author must have
written every covered change. Any other reader gets `403` `forbidden`. A
token bound to a task fork lands only changes whose source is that fork
(`403` `token_bound` otherwise).

The body is optional:

| Member | Meaning |
|---|---|
| `expected_version` | an integer of at least 1: land only when the change is at this version, so you never land a version you did not see |
| `stack` | `true` lands the change and every unlanded change below it; default `false` |
| `override` | `{"reason": "…"}` with a non-blank reason: lands a change whose landing is `pending`, without its approvals or checks. It never lifts `blocked`. Only the owner of `{slug}` (with an API key or a PAT not bound to a workspace) and the admin token may send it, and only while the setting `land.allow_override` is `true`; otherwise `403` `override_not_allowed`. The reason is kept on the attempt and in its events |
| `session_id` | the session the land is asked in (`400` `unknown_session` when it cannot be linked, see [Sessions and telemetry](#sessions-and-telemetry)) |

The query parameter `wait` is how many seconds the call waits for the
attempt to end: a whole number from 0 to 60, 30 when absent. `wait=0`
answers at once.

A refused request creates no attempt. Of several lands of one change sent
at the same moment, exactly one creates an attempt; the others get
`land_in_progress`. Once the attempt exists, the answer follows its status:

| Answer | Attempt |
|---|---|
| `200`, the attempt | `landed` |
| `202`, the attempt | still `running` when the wait ran out |
| `409` `land_conflict` | `conflict`, the attempt in `error.details` |
| `409` `policy_blocked` | `blocked`, the attempt in `error.details` |
| `503` `land_failed` | `failed`, the attempt in `error.details` |

The refusals are listed under
[Errors](#errors-retry-fix-or-ask-a-person); a `{number}` that names no
change of `{slug}` is `404` `change_not_found`.

### Reading land attempts

- `GET /api/v1/workspaces/{slug}/lands/{id}` answers one attempt; `404`
  `land_not_found` for an id that names no attempt of `{slug}`.
- `GET /api/v1/workspaces/{slug}/changes/{number}/lands` lists the attempts
  that cover a change, those of a stack it was part of included.
- `GET /api/v1/workspaces/{slug}/lands` lists the attempts whose target is
  `{slug}`. Filters: `status` (repeatable, any of the five), `target_branch`
  and `since` (an RFC 3339 timestamp: attempts created at or after it).

Both lists are newest first. All three need read access to `{slug}`. A
token bound to a task fork reads, on the fork's parent, the attempts of
changes whose source is the fork; the workspace-wide list answers it `403`
`token_bound`.

### `POST /api/v1/workspaces/{slug}/changes/{number}/revert`

Undoes a landed change as a change of its own, for when something breaks
after a land. The hub builds, on the head of the original's target branch,
one commit per landed commit of the original, newest first, each undoing
it. It puts them on a new branch `revert/<number>` (or `revert/<number>-2`
and so on when the name is taken) and opens a change from that branch into
the original's target, with `reverts` the original's number. The original
then shows it as `reverted_by`, and gets a `parley.change.revert` event.

The reverting change is an ordinary change: policy judges its files, and it
lands through `POST …/land`, or by itself when you ask for `auto_land`.
Reverting a reverting change is allowed and restores what the original
landed.

**Who may call it:** the admin token, the owner of `{slug}` and, while
`land.by_author` is `true`, the original's author. A token bound to a task
fork may not (`403` `token_bound`). The route accepts an `Idempotency-Key`.

The body is optional:

| Member | Meaning |
|---|---|
| `title` | 1 to 200 characters, one line; default `Revert "<title of the original>"` |
| `intent` | at most 16,000 characters; default `Reverts change <slug>#<number>: <title of the original>.` |
| `draft` | `true` opens the reverting change as a draft |
| `auto_land` | `true` sets `auto_land` on the reverting change, with you as the user who set it |
| `session_id` | the session the reverting change is opened in |

The answer is `201` with the new change, as [Changes](#changes) describes
it. A refusal leaves no branch, no change and no event.

| Error | When | What to do |
|---|---|---|
| `409` `change_not_landed` | the change has not landed | nothing to revert |
| `409` `already_reverted` | a draft, open or landed change reverts it already; its number is in `error.details.reverted_by` | use that change |
| `400` `nothing_to_land` | the original landed no commit, or every undo would be empty | nothing to revert |
| `409` `revert_conflict` | an undo does not apply on the target head; the conflict report is in `error.details.conflict` | later work built on the change: undo it by hand in a normal change |
| `400` `target_not_found` | the original's target branch is gone | ask a person |
| `403` `path_not_allowed` | a path-limited token, and the landed version has a file outside its paths | use a credential that covers the paths |
| `403` `forbidden`, `409` `workspace_archived`, `workspace_busy`, `workspace_not_ready`, `400` `unknown_session` | as for a land | as for a land |

## Landing policy, approvals and checks

Because nothing is serialized at landing, the decision of what may land is
made before it, by policy. A workspace's landing policy says what each part
of the repository needs before a change touching it may land: nothing, named
checks, approvals from named people, or a block. It is written by path
because risk follows paths: documentation can land by itself, `billing/**`
needs its owner, a secrets directory never lands through an agent. You say
this once per workspace, and every change is judged by the files it touches.

Approvals and check results are the evidence policy counts. Both attach to
one **version** of a change, not to the change: a reviewer approves the
diff they read, and a runner reports on the commit it tested. A new version
with different content starts with no approvals. A version that only moved
(a rebase or restack with the same diff, `patch_equivalent_to` an earlier
version) keeps the approvals, which the hub carries over, and its check
results still apply unless the rule asks for `fresh_checks`.

### How policy is written and evaluated

The policy is one JSON document: ordered `rules` and a `default`. For
example:

```json
{
  "default": {"approvals": 0, "checks": ["test"]},
  "rules": [
    {"name": "docs", "paths": ["docs/**", "**/*.md"],
     "exclude": ["docs/internal/**"], "require": {}},
    {"name": "billing", "paths": ["billing/**"],
     "targets": ["main", "release/*"],
     "require": {"approvals": 1, "approvers": ["owner", "user:dana"],
                 "checks": ["test", "billing-e2e"], "fresh_checks": true}},
    {"name": "secrets", "paths": ["deploy/secrets/**"],
     "require": {"block": true}}
  ]
}
```

This says: documentation and Markdown files land with nothing required,
except under `docs/internal/`. A file under `billing/`, in a change into
`main` or a `release/*` branch, needs one approval from the workspace owner
or the user `dana`, and the checks `test` and `billing-e2e` passed on
exactly the version being landed. Nothing under `deploy/secrets/` lands
through a change. Every other file needs the `test` check.

Each file of a change (the files that differ between the base and the head
of its current version, a renamed file under both paths) selects the
**first** rule, in document order, whose `targets` match the change's
target branch, one of whose `paths` matches the file and none of whose
`exclude` does. A file no rule selects takes `default`. Order matters:
above, `billing/README.md` selects `docs`, because `**/*.md` comes first.
Each selected rule gives the change one **requirement**, and the change
must satisfy all of them. A stacked change counts only the files of its own
commits.

| Member | Meaning |
|---|---|
| `rules` | at most 200 rules; absent is none |
| `default` | the requirement of a file no rule selects; absent requires nothing |
| `name` | lower-case letters, digits, `_` and `-`, at most 63 characters, unique, not `default` |
| `paths` | required: path globs, each 1 to 255 characters, not starting with `/`. A glob matches the whole path: `*` any run without `/`, `**` any run including `/`, `?` one character other than `/` |
| `exclude` | path globs that take files back out of the rule |
| `targets` | branch globs; absent matches every target branch |
| `require` | required on a rule: the requirement, with the members below |
| `approvals` | 0 to 10: how many counted approvals are needed; default `0` |
| `approvers` | who may give them: `owner`, `member` (any member of the workspace's organization) or `user:<username>` (an existing user, matched exactly); default `["owner"]` |
| `checks` | names of checks that must be `passed`: lower-case letters, digits, `_`, `.` and `-`, at most 63 characters; default none |
| `fresh_checks` | `true` counts only results reported on the version itself, not on an earlier patch-equivalent one; default `false` |
| `block` | `true`: a change touching a matching file cannot land; default `false` |

The document is at most 65,536 bytes. Every replacement is a new
`revision`, numbered from 1; the hub keeps the 50 newest. A workspace that
never set a policy is at revision 0: no rules, nothing required, everything
may land. A task fork has no policy of its own; a change into a fork is
judged at revision 0, and a change from a fork into its parent by the
parent's policy.

### The landing of a change

Every draft and open change carries its `landing`: the policy's current
answer for its current version. It has a `status`, the `requirements`, and
a `reason` when a structural cause blocks it.

| `status` | Meaning |
|---|---|
| `ready` | every requirement is satisfied, or there is none |
| `pending` | a requirement lacks approvals or a passed check, or has an outstanding rejection. It can become `ready`, and the owner may land over it with an `override` |
| `blocked` | a requirement has `block`, the change is a draft (`reason` `draft`), or the change cannot land as it is (`reason` `target_not_found`, `nothing_to_land`, `unrelated_history` or `merge_commits`). No approval or override lifts it |

Each requirement says what is missing:

| Member | Meaning |
|---|---|
| `rule` | the rule's name, or `default` |
| `paths` | the change's files that selected it, the first 50 |
| `approvals` | `required`, `counted` (the approvals on this version that count) and `approvers` |
| `checks` | each check with its `status`: `passed`, `failed` or `pending` (no result yet) |
| `block` | the rule's `block` |
| `satisfied` | `true` when the rule does not block, enough approvals count, no counted rejection is outstanding and every check passed |

The hub evaluates the landing again whenever its inputs change: a new
version, a retarget, a move between draft and open, a restore, an approval
given or withdrawn, a check reported, a replacement of the policy, and once
more at the moment of a land. When an open change becomes landable (`ready`
and `mergeable` `clean`) the hub writes one `parley.change.ready` event,
which is what [automatic landing](#automatic-landing) and waiting agents
react to. A land of a change that is not `ready` ends `blocked` and is
answered `409` `policy_blocked`; the attempt's `blocked.requirements` is
this same list, so the answer itself says which approval or check is
missing.

### Worked example: from pending to landed

Ask what a change would need before it exists:

```sh
curl -sS -X POST "$PARLEY_URL/api/v1/workspaces/demo/policy/evaluate" \
  -H "Authorization: Bearer $PARLEY_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"paths": ["billing/a.go", "docs/a.md"], "target_branch": "main"}'
```

Under the document above, stored as revision 1:

```json
{"policy_revision":1,"status":"pending","requirements":[
 {"rule":"docs","paths":["docs/a.md"],
  "approvals":{"required":0,"counted":0,"approvers":["owner"]},
  "checks":[],"block":false,"satisfied":true},
 {"rule":"billing","paths":["billing/a.go"],
  "approvals":{"required":1,"counted":0,"approvers":["owner","user:dana"]},
  "checks":[{"name":"test","status":"pending"},
            {"name":"billing-e2e","status":"pending"}],
  "block":false,"satisfied":false}]}
```

Once the change is open as `demo#1`, the CI runner reports its result with
a PAT that holds only `checks:write`:

```sh
curl -sS -X PUT "$PARLEY_URL/api/v1/workspaces/demo/changes/1/checks/test" \
  -H "Authorization: Bearer $RUNNER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"status": "passed", "version": 1, "summary": "42 tests",
       "url": "https://ci.example/run/7"}'
```

and `dana` approves the version she reviewed:

```sh
curl -sS -X POST "$PARLEY_URL/api/v1/workspaces/demo/changes/1/approvals" \
  -H "Authorization: Bearer $DANA_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"verdict": "approve", "version": 1, "comment": "ok"}'
```

The answer's `counts_for` is `["billing"]`: her approval counts toward that
requirement. When `billing-e2e` has passed as well, the landing becomes
`ready`, the hub writes `parley.change.ready`, and the change lands by
`POST …/land` or by itself with `auto_land`.

The routes follow the shared rules in [Using the API](#using-the-api).
`{slug}` is the workspace that holds the change's target branch.

| Route | Scope | What it does |
|---|---|---|
| [`GET /api/v1/workspaces/{slug}/policy`](#get-apiv1workspacesslugpolicy) | `changes:read` | reads the policy, or an earlier revision |
| [`PUT /api/v1/workspaces/{slug}/policy`](#put-apiv1workspacesslugpolicy) | `workspaces:write` | replaces the policy |
| [`POST /api/v1/workspaces/{slug}/policy/evaluate`](#post-apiv1workspacesslugpolicyevaluate) | `changes:read` | says what paths or a change would need, writing nothing |
| [`POST /api/v1/workspaces/{slug}/changes/{number}/approvals`](#post-apiv1workspacesslugchangesnumberapprovals) | `changes:approve` | approves or rejects a version |
| [`GET /api/v1/workspaces/{slug}/changes/{number}/approvals`](#get-apiv1workspacesslugchangesnumberapprovals) | `changes:read` | lists the verdicts |
| [`DELETE /api/v1/workspaces/{slug}/changes/{number}/approvals/{id}`](#delete-apiv1workspacesslugchangesnumberapprovalsid) | `changes:approve` | withdraws a verdict |
| [`PUT /api/v1/workspaces/{slug}/changes/{number}/checks/{name}`](#put-apiv1workspacesslugchangesnumberchecksname) | `checks:write` | reports a check result |
| [`GET /api/v1/workspaces/{slug}/changes/{number}/checks`](#get-apiv1workspacesslugchangesnumberchecks) | `changes:read` | lists the check results that apply to a version |

### `GET /api/v1/workspaces/{slug}/policy`

Answers the policy of the current revision. Anyone with read access may
read it, an org member and a token bound to a task fork of `{slug}`
included.

```json
{"revision":2,
 "default":{"approvals":0,"approvers":["owner"],"checks":[],
            "fresh_checks":false,"block":false},
 "rules":[{"name":"all","paths":["**"],"exclude":[],
   "require":{"approvals":2,"approvers":["owner"],"checks":[],
              "fresh_checks":false,"block":false}}],
 "updated_at":"2026-11-16T10:00:03Z",
 "updated_by":{"actor_type":"admin_token","actor_id":null,"username":null}}
```

The document comes back in normal form: every requirement member present
with its default, `targets` only on a rule that gave it. `updated_at` and
`updated_by` are `null` at revision 0. The header `ETag` carries the
revision in double quotes; send it back as `If-Match` when you replace the
policy. `?revision=N` answers an earlier revision while it is kept, `404`
`not_found` otherwise.

### `PUT /api/v1/workspaces/{slug}/policy`

Replaces the policy with the document in the body. Only the workspace's
owner and the admin token may call it; an org member gets `403`
`forbidden`, a token bound to a task fork `403` `token_bound`.

Send `If-Match` with the revision you read (`If-Match: 2` or `If-Match:
"2"`) to replace only that revision; without it the document replaces
whatever is stored. The answer is `200` with the policy of the new
revision. By then every draft and open change into `{slug}`, those from its
task forks included, has been evaluated against the new revision with the
approvals and checks it already has, and a change that became landable has
its `parley.change.ready`. The replacement is all or nothing, and writes
one `parley.policy.update` event.

| Error | When |
|---|---|
| `400` `invalid_policy` | the document breaks a rule of the format. `error.details` names the first violation: `rule` (the rule's name, `default`, or `null` for the document as a whole), `index` (the rule's zero-based position) and `field` (for example `require.approvals` or `approvers[0]`) |
| `400` `not_supported_on_fork` | `{slug}` is a task fork: set the policy of its parent |
| `412` `revision_mismatch` | the policy was replaced since the revision in `If-Match`: read it again, merge, and retry |
| `409` `workspace_archived` | the workspace is archived |

### `POST /api/v1/workspaces/{slug}/policy/evaluate`

Answers what a list of paths, or an existing change, would need to land.
Use it before you start work, to learn whether a plan needs a person, and
to try a candidate policy before you store it. It writes nothing. Anyone
with read access may call it, also on an archived workspace.

The body has exactly one of `paths` and `change`:

| Member | Meaning |
|---|---|
| `paths` | 1 to 2,000 repository paths, not starting with `/` |
| `target_branch` | with `paths` only: the branch they would land on; default the workspace's default branch |
| `change` | the number of a change of `{slug}`, in any state: its current version's files on its own target branch |
| `policy` | optional: a candidate document, evaluated in place of the stored one |

The answer is `policy_revision` (the stored revision, also when a candidate
was evaluated), and the `status` and `requirements` of
[a landing](#the-landing-of-a-change) as they would be with no approval and
no check result: every `counted` is `0` and every check `pending`. It does
not consider the state of the change, so a draft is not `blocked` for being
a draft.

Errors: `400` `invalid_request` for a malformed body, `400` `invalid_policy`
for a bad candidate (with `details` as for `PUT`), `404` `change_not_found`
for a change that does not exist or that a fork-bound token may not see.

### `POST /api/v1/workspaces/{slug}/changes/{number}/approvals`

Records your verdict on a version: `approve` or `reject`. A person or a
review agent calls it after reading the diff of that version.

**Who may call it:** any user who can read the change, an org member
included. A PAT needs `changes:approve`, which no other scope implies. The
admin token cannot give a verdict (`403` `forbidden`), and neither can a
token bound to a task fork (`403` `token_bound`).

| Member | Meaning |
|---|---|
| `verdict` | required: `approve` or `reject` |
| `comment` | at most 16,000 characters |
| `version` | the version the verdict is on; default the current version. Name the version you reviewed, so that a push in between does not get your approval |

Whether a verdict **counts** toward a requirement is decided by two rules:

- You must be among the requirement's `approvers`.
- The credential you approve with must not have created that version or
  an earlier one of the change. Credentials are compared, not users: a
  person approving their agent's push with their own API key counts; an
  agent approving the version it pushed does not. Versions the hub made (a
  restack, a sync) exclude nobody.

A counted `reject` keeps its requirement unsatisfied until its author
replaces or withdraws it, or a version with different content arrives.

You have one verdict per version: the first is answered `201`, a later one
replaces it and is answered `200`. A verdict on an earlier version that the
current one is patch-equivalent to is carried forward to the current
version. The hub writes `parley.change.approve` or `parley.change.reject`.

```json
{"id":"7192e863-b4a0-4a41-8357-49ca6ca19a64","change":1,"version":2,
 "verdict":"approve","comment":"",
 "by":{"actor_type":"api_key",
       "actor_id":"53874e1f-8c16-48ae-a95a-51563b197fc7","username":"dev"},
 "counts_for":["default"],"carried_from":1,
 "created_at":"2026-11-16T11:13:37Z"}
```

`counts_for` lists the rules of the change's requirements this verdict
counts toward, `[]` when it counts for none or is not on the current
version. `carried_from` is the version the verdict was first given on, for
a copy carried to a patch-equivalent version; else `null`.

Errors: `404` `change_not_found`; `404` `not_found` for a version the
change does not have; `409` `change_closed` for a landed or abandoned
change; `409` `workspace_archived`; `400` `invalid_request` for a bad body,
an unknown member included.

### `GET /api/v1/workspaces/{slug}/changes/{number}/approvals`

Lists the verdicts on every version of the change, or on the version the
query parameter `version` names, newest first.

### `DELETE /api/v1/workspaces/{slug}/changes/{number}/approvals/{id}`

Withdraws one verdict and answers `204`. The verdict's user, the
workspace's owner and the admin token may withdraw it; anyone else gets
`403` `forbidden`, a token bound to a task fork `403` `token_bound`. Copies
carried to other versions are verdicts of their own and stay. Errors as for
the `POST`, with `404` `not_found` for an unknown `{id}`.

### `PUT /api/v1/workspaces/{slug}/changes/{number}/checks/{name}`

Reports the result of the check `{name}` on a version. The hub runs no
checks: an external runner (CI, a test agent) watches for new versions,
runs what it runs, and reports here.

**Who may call it:** a user with write access to the workspace (the owner;
an org member gets `403` `forbidden`) or the admin token. A PAT needs
`checks:write`, which no other scope implies, so a runner's token can hold
that scope alone and do nothing else. A token bound to a task fork gets
`403` `token_bound`.

| Member | Meaning |
|---|---|
| `status` | required: `pending`, `passed` or `failed` |
| `version` | the version tested; default the current version. Always send it: the runner knows which commit it tested |
| `summary` | at most 2,000 characters |
| `url` | an absolute `http` or `https` URL, at most 2,048 characters, for example the run's page |
| `details` | a JSON object of at most 16,384 bytes, kept as sent |

There is one result per version and name. A report replaces the earlier one
whole, so a member you omit becomes empty. The answer is `200` with the
check, and the hub writes `parley.change.check` for every report.

```json
{"name":"test","change":1,"version":1,"status":"passed",
 "summary":"42 tests","url":"https://ci.example/run/7",
 "details":{"duration_ms":812},
 "reported_by":{"actor_type":"pat",
   "actor_id":"53874e1f-8c16-48ae-a95a-51563b197fc7","username":"dev"},
 "updated_at":"2026-11-16T11:13:37Z"}
```

Errors: as for an approval, with `400` `invalid_request` also for a
`{name}` that is not a valid check name.

### `GET /api/v1/workspaces/{slug}/changes/{number}/checks`

Lists the results that apply to the current version, or to the version
`version` names, one per name, ordered by name. The result that applies is
the one reported on that version, else the one on the nearest earlier
version it is patch-equivalent to; such a result keeps the `version` it was
reported on. A rule with `fresh_checks` counts only a result reported on
the version itself. A check no runner reports stays `pending`.

A token bound to a task fork reads the approvals and checks, on the fork's
parent, of changes whose source is that fork.

## Awareness

When many agents work on one repository, the expensive conflicts are the
ones nobody saw coming: two agents rewrite the same file for an hour, and
one of them finds out at landing. Awareness makes work in flight visible
early. Before you start, you ask who else touches the paths you are about
to edit, and announce your own intent. While you work, the hub tells you
when another change starts to overlap yours.

Awareness is advisory. Nothing here refuses a push, a commit, an open or a
land. You decide what to do with what you see: wait, coordinate through the
other change, stack on top of it, or carry on and resolve the conflict
later.

### How awareness works

- **Overlap.** Two changes overlap when both are `draft` or `open`, have
  the same target branch and touch at least one file in common (a renamed
  file counts under both paths). Changes of one stack do not overlap each
  other. Overlap is on paths, not lines. The hub tracks it in the
  background, about a second behind the push that caused it, and never
  delays that push.
- **Overlap events.** When two changes start to overlap, the hub writes
  `parley.change.overlap` on each, naming the other (`with`), its session
  (`with_session_id`) and the common `paths`. A pair is announced once
  while it overlaps, and again if it stops and later overlaps anew.
- **Activity.** One query answers "what touches these paths": the changes
  in flight, the active claims, and what landed recently.
- **Claims.** A claim says that a session intends to work on some paths,
  before any code exists. It is a statement, not a lock: claims never
  conflict with each other, two sessions may claim the same paths, and a
  claim has no effect on pushes, policy or landing. A claim expires by
  itself, so an agent that crashes leaves nothing behind.

Through a task fork's slug every awareness route acts on the fork's
**parent**: you need access to the fork only, and the changes, claims and
default branch are the parent's. A claim made through a fork is stored on
the parent.

### Worked example: look before you start

Announce the work and see what is there, in one call:

```sh
curl -sS -X POST "$PARLEY_URL/api/v1/workspaces/demo/claims" \
  -H "Authorization: Bearer $PARLEY_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"paths": ["api/**"], "intent": "rework handlers", "ttl": "2h"}'
```

The answer is `201` with the claim and the activity on its paths, your own
claim left out:

```json
{"claim":{"id":"<uuid>","workspace_slug":"demo","paths":["api/**"],
   "target_branch":"main","intent":"rework handlers",
   "by":{"actor_type":"api_key","actor_id":"…","username":"bob"},
   "session_id":null,"change":null,"expires_at":"2026-11-16T12:00:00Z",
   "state":"active","created_at":"2026-11-16T10:00:00Z"},
 "activity":{"target_branch":"main","target_sha":"<40 hex>",
   "changes":[{"change":{"number":2,"ref":"demo#2","title":"…",
       "intent":"…","state":"open",
       "author":{"actor_type":"api_key","actor_id":"…","username":"ann"},
       "session_id":null,"version":1,"updated_at":"2026-11-16T09:40:00Z",
       "mergeable":"clean","landing":{"status":"ready"}},
     "paths":["api/handler.go"],"paths_truncated":false,"count":1}],
   "claims":[],"landed":[]}}
```

Here `demo#2` already touches `api/handler.go` and is ready to land. Read
its intent and decide: wait for it, stack on it, or stay out of that file.
If you decide not to do the work, release the claim with `DELETE`. If the
work takes longer than the claim, renew it with `PATCH`.

Later, before you go further, ask what landed in your paths since your base
commit, leaving out your own work:

```sh
curl -sS -G "$PARLEY_URL/api/v1/workspaces/demo/activity" \
  -H "Authorization: Bearer $PARLEY_TOKEN" \
  --data-urlencode "paths=api/**" \
  --data-urlencode "since=dc54adf9e0607cad9039738e34a4c3061a859f17" \
  --data-urlencode "exclude_change=9"
```

A non-empty `landed` means you should rebase before you continue.

The routes follow the shared rules in [Using the API](#using-the-api) and
answer in every workspace status. All need read access; the claim writes
need no more than that, so an org member may claim.

| Route | Scope | What it does |
|---|---|---|
| [`GET /api/v1/workspaces/{slug}/activity`](#get-apiv1workspacesslugactivity) | `changes:read` | what touches a set of paths |
| [`GET /api/v1/workspaces/{slug}/changes/{number}/overlaps`](#get-apiv1workspacesslugchangesnumberoverlaps) | `changes:read` | the changes that overlap one change |
| [`POST /api/v1/workspaces/{slug}/claims`](#post-apiv1workspacesslugclaims) | `changes:write` | announces intent on paths |
| [`GET /api/v1/workspaces/{slug}/claims`](#reading-claims) | `changes:read` | lists claims |
| [`GET /api/v1/workspaces/{slug}/claims/{id}`](#reading-claims) | `changes:read` | reads one claim |
| [`PATCH /api/v1/workspaces/{slug}/claims/{id}`](#renewing-and-releasing-a-claim) | `changes:write` | renews a claim or changes its intent |
| [`DELETE /api/v1/workspaces/{slug}/claims/{id}`](#renewing-and-releasing-a-claim) | `changes:write` | releases a claim |

A token bound to a task fork reaches the four reads on the fork's parent,
and sees all changes there, not only its own. The claim writes answer it
`403` `token_bound` on the parent.

### `GET /api/v1/workspaces/{slug}/activity`

Answers what touches a list of paths on one target branch.

| Parameter | Meaning |
|---|---|
| `paths` | required: 1 to 50 globs (`*` within a path segment, `**` across segments), as a comma list, repeated, or both; each 1 to 255 characters, not starting with `/` |
| `target` | the target branch; default the workspace's default branch. An unknown branch gives `target_sha` `null` and, normally, empty lists |
| `since` | how far back `landed` looks. A 40-character commit id (typically your base or fork point): the changes landed since that commit. Otherwise an RFC 3339 timestamp: the changes landed after it. Default: 24 hours ago |
| `exclude_session` | leaves out the changes, claims and landed changes of that session: your own |
| `exclude_change` | leaves out that change and the claims linked to it |

The answer has `target_branch`, `target_sha` (the head of the target) and
three lists, never `null` and not paged:

- `changes`: the draft and open changes on the target that touch a
  matching file, as overlap entries, most matching files first. Drafts are
  shown, and so are changes from forks you cannot read.
- `claims`: the active claims on the target whose globs can match the same
  files as yours, newest first. The comparison of two globs errs towards
  reporting.
- `landed`: the changes landed on the target since `since` that touched a
  matching file, newest first, at most 100: `number`, `ref`, `title`,
  `landed_sha`, `landed_at`, `session_id` and `paths` (the matching files,
  at most 100).

An **overlap entry** is a summary of the other change and the files in
common:

| Member | Meaning |
|---|---|
| `change` | `number`, `ref`, `title`, `intent` (the first 500 characters), `state`, `author`, `session_id`, `version`, `updated_at`, `mergeable` and `landing.status` |
| `paths` | the files in common (for the activity: the matching files), the first 100 |
| `paths_truncated` | `true` when there are more |
| `count` | how many there are |

`400` `invalid_request` answers missing or invalid `paths`, a `since` that
is neither a timestamp nor a commit the repository holds, and an
`exclude_change` that is not a positive integer.

### `GET /api/v1/workspaces/{slug}/changes/{number}/overlaps`

Lists one overlap entry for every other change that overlaps this one, the
most files in common first. Use it to decide the order in which overlapping
changes should land. A landed or abandoned change has an empty list; an
unknown number is `404` `change_not_found`. Reading the change with
`?include=overlaps` adds the same entries as the member `overlaps` (see
[Changes](#changes)).

### `POST /api/v1/workspaces/{slug}/claims`

Creates a claim and answers `201` with `{"claim": …, "activity": …}`: the
claim and the activity for its paths and target without the claim itself,
so one call both announces and checks. It accepts an `Idempotency-Key` and
writes `parley.claim.create`.

| Member | Meaning |
|---|---|
| `paths` | required: 1 to 50 globs, as for the activity |
| `intent` | required: 1 to 2,000 characters, what you are about to do |
| `target_branch` | default the workspace's default branch |
| `ttl` | how long the claim lives: a whole number followed by `s`, `m`, `h` or `d`, at least `1s` and at most `[awareness].max_claim_ttl` (24 hours by default); default `[awareness].default_claim_ttl` (1 hour by default) |
| `session_id` | the session the claim belongs to |

A claim has a `state`:

| State | Meaning |
|---|---|
| `active` | announced and not past `expires_at` |
| `released` | its holder released it |
| `expired` | it passed `expires_at`; a read shows this at once, and the hub writes `parley.claim.expire` within about a minute |
| `fulfilled` | the change it was linked to landed or was abandoned |

A claim links itself to your change: when a change is opened with a
`session_id` that has active claims on the same workspace and target
branch, those claims get `change` set to its number, and become `fulfilled`
when that change lands or is abandoned. Give the same `session_id` to the
claim and to the change, and you never have to release the claim yourself.
A claim that is no longer active is removed 7 days after it ended.

Errors: `400` `invalid_request` for a missing or invalid member; `400`
`unknown_session`; `429` `claim_limit_reached` when the workspace already
has `[awareness].max_claims_per_workspace` active claims (1,000 by
default) or the session has 50: release claims you no longer need. The
`[awareness]` keys are described in
[the configuration](configuration.md#the-configuration-file).

### Reading claims

`GET /api/v1/workspaces/{slug}/claims` lists the workspace's claims, newest
first. Filters: `state` (one of the four; default `active`), `session_id`
and `path` (a glob, compared as in the activity).
`GET /api/v1/workspaces/{slug}/claims/{id}` answers one claim; `404`
`claim_not_found` for an id that names no claim of the workspace.

### Renewing and releasing a claim

`PATCH /api/v1/workspaces/{slug}/claims/{id}` takes `ttl`, `intent` or
both. `ttl` moves `expires_at` to now plus `ttl`; `intent` replaces the
intent. It answers `200` with the claim. `409` `claim_not_active` answers a
claim that is no longer active: create a new one.

`DELETE /api/v1/workspaces/{slug}/claims/{id}` releases an active claim,
writes `parley.claim.release` and answers `204`. For a claim that is not
active it changes nothing and answers `204` as well.

Only the user who created the claim (with any of their credentials), the
owner of the workspace and the admin token may renew or release it; anyone
else who can read it gets `403` `forbidden`.

## Forge

A workspace's repository usually lives on GitHub or GitLab, and so do its
issues and pull requests. An agent that triages issues, comments on them or
opens a pull request would normally need a forge token of its own. With
these routes it does not: the hub calls the forge with the credential it
already stores for the workspace, so the token stays in the hub and a
caller holds only a hub credential with the scope `forge:read` or
`forge:write`. The routes also answer GitHub and GitLab in one shape, so a
client is written once. On GitLab a "pull request" is a merge request and a
number is the project-level `iid`.

The forge stays the system of record. The hub caches nothing: every read
asks the forge. It never acts on what the forge reports either: a webhook
delivery does not open, change or land a [change](#changes), and a pull
request opened here is an ordinary forge pull request between branches
that exist on the forge.

### Which forge a workspace has

The hub works it out from the workspace's remote URL at every request:

- **Repository.** The host, owner and name are taken from the remote URL
  (`https://`, `http://`, `ssh://`, `git://` or `user@host:owner/name`). A
  GitLab subgroup stays part of the owner (`group/subgroup`).
- **Kind.** `github` or `gitlab`, whichever of the two words the host
  contains. A self-hosted forge whose host names neither (or both) is
  unknown until you set the workspace setting `forge_api_url` (see
  [Workspaces](#workspaces)) to its API URL; the kind is then read from
  that URL.
- **API URL.** The setting when set, else `https://api.github.com` for
  `github.com`, `https://<host>/api/v3` for another GitHub host and
  `https://<host>/api/v4` for GitLab. The setting must be an absolute
  `https://` URL without user information, query or fragment.
- **Credential.** Read from the workspace's own
  [secrets](#secrets-and-variables) at every request, so a rotated secret
  applies to the next call: `GIT_PAT` (kind `pat`), else `GIT_USERNAME`
  with `GIT_PASSWORD`, the password being the token (kind `basic`), else
  none (kind `none`). The hub never takes a forge token from its own
  environment. Reads without a credential are sent anonymously, which
  works on a public repository; writes are refused.

[`GET /api/v1/workspaces/{slug}/forge`](#get-apiv1workspacesslugforge)
shows the outcome without contacting the forge. Start there when a call
fails.

**`target`.** Every read and write takes the query parameter `target`.
Absent or `origin`, the route works on the workspace's `git_url`.
`upstream` works on the upstream repository of a
[carry-patch workspace](#carry-patch-workspaces), with the setting
`upstream_forge_api_url` and the secrets `UPSTREAM_GIT_PAT` or
`UPSTREAM_GIT_USERNAME` with `UPSTREAM_GIT_PASSWORD`, falling back to the
origin credential when neither is stored. On a workspace without an
upstream it is `400` `workspace_mode_mismatch`.

**Task forks.** A [task fork](#task-forks-and-workspace-tokens) has no
forge of its own. Called with a fork's slug, a read or write acts on the
parent's repository with the parent's credential and settings, so an agent
does not need to know the parent. The user behind the credential needs the
route's access on the parent, else `403` `forbidden`. The webhook routes
and the delivery log do not take a fork's slug (`400`
`fork_has_no_origin`).

### Example: triage an issue

```sh
# What does the hub know about this workspace's forge?
curl -s -H "Authorization: Bearer $PARLEY_TOKEN" \
  https://hub.example.com/api/v1/workspaces/widgets/forge
```

```json
{"forge":"github","api_url":"https://api.github.com","repository":{"host":"github.com","owner":"acme","name":"widgets"},"credential":"pat","upstream":null,"webhook":{"configured":false,"url":"https://hub.example.com/hooks/forge/widgets","last_delivery_at":null}}
```

```sh
# Open bugs, newest first
curl -s -H "Authorization: Bearer $PARLEY_TOKEN" \
  "https://hub.example.com/api/v1/workspaces/widgets/issues?labels=bug"
```

```json
{"items":[{"number":7,"title":"Crash","body":"","state":"open","url":"https://github.com/acme/widgets/issues/7","author":{"login":"kim"},"labels":["bug","ui"],"created_at":"2026-10-01T08:00:00Z","updated_at":"2026-10-02T09:30:00Z","closed_at":null,"is_pr":false}],"next_cursor":null}
```

```sh
# Comment, then label (both need forge:write)
curl -s -X POST -H "Authorization: Bearer $PARLEY_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"body":"Reproduced on main; working on a fix."}' \
  https://hub.example.com/api/v1/workspaces/widgets/issues/7/comments

curl -s -X POST -H "Authorization: Bearer $PARLEY_TOKEN" \
  -H "Content-Type: application/json" \
  -d '["in-progress"]' \
  https://hub.example.com/api/v1/workspaces/widgets/issues/7/labels
```

The comment answers `201` with `{"url": "<web URL of the comment>"}`, the
labels `204`.

### Forge routes

| Route | Scope | What it does |
|---|---|---|
| [`GET /api/v1/workspaces/{slug}/forge`](#get-apiv1workspacesslugforge) | `forge:read` | how the hub reaches the forge; webhook status |
| [`GET /api/v1/workspaces/{slug}/forge/repository`](#get-apiv1workspacesslugforgerepository) | `forge:read` | the repository as the forge reports it |
| [`GET /api/v1/workspaces/{slug}/issues`](#get-apiv1workspacesslugissues) | `forge:read` | list issues |
| [`GET /api/v1/workspaces/{slug}/issues/{number}`](#get-apiv1workspacesslugissuesnumber) | `forge:read` | an issue with its first comments |
| [`GET /api/v1/workspaces/{slug}/issues/{number}/comments`](#get-apiv1workspacesslugissuesnumbercomments) | `forge:read` | list an issue's comments |
| [`GET /api/v1/workspaces/{slug}/pulls/{number}`](#get-apiv1workspacesslugpullsnumber) | `forge:read` | a pull request |
| [`GET /api/v1/workspaces/{slug}/pulls/{number}/state`](#get-apiv1workspacesslugpullsnumberstate) | `forge:read` | its state only |
| [`GET /api/v1/workspaces/{slug}/pulls/{number}/files`](#get-apiv1workspacesslugpullsnumberfiles) | `forge:read` | the files it changes |
| [`GET /api/v1/workspaces/{slug}/pulls/{number}/checks`](#get-apiv1workspacesslugpullsnumberchecks) | `forge:read` | the check runs of its head |
| [`GET /api/v1/workspaces/{slug}/pulls/{number}/reviews`](#get-apiv1workspacesslugpullsnumberreviews) | `forge:read` | its reviews |
| [`POST /api/v1/workspaces/{slug}/issues`](#post-apiv1workspacesslugissues) | `forge:write` | create an issue |
| [`PATCH /api/v1/workspaces/{slug}/issues/{number}`](#patch-apiv1workspacesslugissuesnumber) | `forge:write` | update an issue |
| [`POST /api/v1/workspaces/{slug}/issues/{number}/close`](#post-apiv1workspacesslugissuesnumberclose) | `forge:write` | close an issue, with an optional comment |
| [`POST /api/v1/workspaces/{slug}/issues/{number}/comments`](#post-apiv1workspacesslugissuesnumbercomments) | `forge:write` | comment on an issue |
| [`POST /api/v1/workspaces/{slug}/issues/{number}/labels`](#post-apiv1workspacesslugissuesnumberlabels) | `forge:write` | add labels to an issue |
| [`DELETE /api/v1/workspaces/{slug}/issues/{number}/labels/{name}`](#delete-apiv1workspacesslugissuesnumberlabelsname) | `forge:write` | remove a label from an issue |
| [`POST /api/v1/workspaces/{slug}/labels`](#post-apiv1workspacessluglabels) | `forge:write` | make sure a label exists |
| [`POST /api/v1/workspaces/{slug}/pulls`](#post-apiv1workspacesslugpulls) | `forge:write` | open a pull request |
| [`POST /api/v1/workspaces/{slug}/pulls/{number}/comments`](#post-apiv1workspacesslugpullsnumbercomments) | `forge:write` | comment on a pull request |
| [`PUT /api/v1/workspaces/{slug}/forge/webhook`](#put-apiv1workspacesslugforgewebhook) | `forge:write` | set or rotate the webhook secret |
| [`DELETE /api/v1/workspaces/{slug}/forge/webhook`](#delete-apiv1workspacesslugforgewebhook) | `forge:write` | remove the webhook secret |
| [`POST /hooks/forge/{slug}`](#post-hooksforgeslug) | none (signed) | receive a delivery from the forge |
| [`GET /api/v1/workspaces/{slug}/forge/deliveries`](#get-apiv1workspacesslugforgedeliveries) | `forge:read` | list the recorded deliveries |

A PAT with `forge:write` also satisfies `forge:read`. Authentication,
scopes, the error envelope, lists and `Idempotency-Key` work as
[Using the API](#using-the-api) says.

### Reading

Reads need read access to the workspace, so a member of the workspace's
org reads as the owner does. They work in every workspace state, archived
included, do not touch the hub's copy of the repository and record no
event.

A `{number}` is a positive decimal integer. Timestamps are RFC 3339 UTC; a
missing one is `null`, a missing `body` or `summary` is `""`, and `url` is
always the object's web URL.

Lists take `limit` (1 to 200, default 50) and `cursor`. Two things differ
from the hub's own lists, because the forge pages by page number:

- An entry added or removed on the forge between two requests can make the
  next page repeat or skip an entry.
- A `limit` sent together with a `cursor` must equal the page size the
  cursor was made with; leave it out. A page may hold fewer than `limit`
  items and still have a `next_cursor`.

#### `GET /api/v1/workspaces/{slug}/forge`

The diagnostic document. It contacts no forge and ignores `target`.

| Member | Value |
|---|---|
| `forge` | `github`, `gitlab` or `unknown` |
| `api_url` | the API URL; `null` when `forge` is `unknown` |
| `repository` | `{host, owner, name}`; `null` when the remote names no repository |
| `credential` | `pat`, `basic` or `none`; never the token |
| `upstream` | the same four members for a carry-patch workspace's upstream, else `null` |
| `webhook.configured` | whether a webhook secret is stored |
| `webhook.url` | the URL to enter on the forge; `null` when the hub has no `external_url` |
| `webhook.last_delivery_at` | when the newest recorded delivery arrived, or `null` |

An API URL setting that is not a valid URL answers `422`
`forge_api_url_invalid`.

#### `GET /api/v1/workspaces/{slug}/forge/repository`

`full_name`, `default_branch`, `private`, `archived`, `permissions {pull,
push, admin}` (what the stored credential may do) and `allow_merge_commit`,
`allow_squash_merge`, `allow_rebase_merge`, each `null` when the forge does
not report it. Use it to check that the credential can push before you
rely on a write.

#### `GET /api/v1/workspaces/{slug}/issues`

A list of issues, never pull requests.

| Parameter | Values | Default |
|---|---|---|
| `state` | `open`, `closed`, `all` | `open` |
| `sort` | `created`, `updated` | `created` |
| `direction` | `asc`, `desc` | `desc` |
| `labels` | names separated by commas; an issue must carry all of them | none |
| `assignee` | a login | none |

An issue is `{number, title, body, state, url, author {login}, labels,
created_at, updated_at, closed_at, is_pr}`; `state` is `open` or `closed`
and `labels` holds the names.

#### `GET /api/v1/workspaces/{slug}/issues/{number}`

The issue thread: `{issue, comments, comments_next_cursor,
comments_error}`, with the first page of at most 100 comments, oldest
first. Pass `comments_next_cursor`, when it is not `null`, to the comments
route to read on. If the issue could be read and its comments could not,
the answer is still `200`, with `comments` `[]` and `comments_error`
`{error_type, message}`; otherwise `comments_error` is `null`. On GitHub a
pull request's number answers it as an issue with `is_pr: true`.

#### `GET /api/v1/workspaces/{slug}/issues/{number}/comments`

A list of comments, oldest first: `{id, body, author {login}, created_at,
updated_at, url}`. GitLab's system notes are left out.

#### `GET /api/v1/workspaces/{slug}/pulls/{number}`

`{number, title, body, state, url, draft, merged, head_sha, head_branch,
base_branch, author {login}, created_at, updated_at, closed_at,
merged_at}`. `state` is `open`, `closed` or `merged`; `head_sha` is the
full commit id.

#### `GET /api/v1/workspaces/{slug}/pulls/{number}/state`

`{number, state, merged, head_sha}` only: the cheap call for a client that
waits for a merge.

```json
{"number":12,"state":"merged","merged":true,"head_sha":"0123456789abcdef0123456789abcdef01234567"}
```

#### `GET /api/v1/workspaces/{slug}/pulls/{number}/files`

A list of `{filename, status, additions, deletions}`; `status` is `added`,
`modified`, `deleted` or `renamed`.

#### `GET /api/v1/workspaces/{slug}/pulls/{number}/checks`

A list of the check runs of the pull request's head (on GitLab the jobs of
the merge request's latest pipeline): `{name, status, conclusion, summary,
url}`. `status` is `queued`, `in_progress` or `completed`; `conclusion` is
`success`, `failure`, `cancelled`, `skipped`, `neutral`, or `null` until
completed. A head without checks answers an empty list. All pages of one
listing describe the head the first page was read from, even if the head
has moved since.

#### `GET /api/v1/workspaces/{slug}/pulls/{number}/reviews`

A list of `{author {login}, state, body, submitted_at}`, oldest first;
`state` is `approved`, `changes_requested`, `commented` or `dismissed`.
GitHub's pending reviews are left out. On GitLab the approvals come first,
with `body` `""` and `submitted_at` `null` (GitLab reports no time for
them), then the comments.

### Writing

Writes need write access to the workspace: a caller with read access only
gets `403` `forbidden`. The hub never writes anonymously, so the target
needs a stored credential (`422` `forge_credential_missing` otherwise).
An archived workspace refuses writes with `409` `workspace_archived`, and
a fork whose parent is archived with `409` `parent_archived`.

Rules for every body:

- A JSON object (the add-labels route takes a bare array; the close route
  may be empty). A `null` member counts as absent; unknown members are
  ignored. A required string must not be blank. A list of names holds at
  most 100 non-blank entries.
- The `body` of an issue, a comment or a pull request, and the `comment`
  of a close, may be 65,536 characters on GitHub and 1,000,000 on GitLab;
  longer is `413` `payload_too_large`.
- On GitLab `assignees` is never sent: GitLab takes user ids, which the
  hub does not resolve.

**Retries.** The routes that create something (create issue, both comment
routes, ensure label, open pull request) accept `Idempotency-Key`: a
replay answers `200` with the first answer and sends nothing to the forge.
Only a successful answer is kept, so a create the forge failed is carried
out again on retry. A write is finished even when you disconnect.

**Events.** Every write that reached the forge records exactly one event,
whether it succeeded or failed; a request the hub refused beforehand
records none. The event names the caller, the workspace whose forge was
written (the parent, for a call through a fork), the `target`, whether it
succeeded (`ok`, and `error_type` when not), and the `number` and `url` of
the result when known. It never holds a title, a text or a credential.

| Write | Event |
|---|---|
| create, update, close issue | `parley.forge.issue.create`, `.update`, `.close` |
| comment on an issue | `parley.forge.comment.create` |
| add, remove, ensure label | `parley.forge.label.add`, `.remove`, `.create` |
| open, comment on a pull request | `parley.forge.pull_request.create`, `.comment` |

#### `POST /api/v1/workspaces/{slug}/issues`

Body `{"title", "body"?, "labels"?, "assignees"?}`; `title` is required.
Answers `201` with the issue.

#### `PATCH /api/v1/workspaces/{slug}/issues/{number}`

Body with at least one of `title`, `body` (`""` clears it), `state` (`open`
or `closed`), `labels` and `assignees` (each replaces the whole set; `[]`
clears it). Only the fields you set are sent. On GitLab an update that sets
only `assignees` is `400` `invalid_request`. Answers `200` with the issue.

#### `POST /api/v1/workspaces/{slug}/issues/{number}/close`

Body absent, `{}` or `{"comment": "..."}`. The comment is posted first and
the issue is closed only when the forge accepted it. If the close fails
after the comment was posted, the answer is the close's failure and the
event carries the comment's URL as `comment_url`. Closing a closed issue
succeeds. Answers `200` with the issue. `Idempotency-Key` is ignored.

#### `POST /api/v1/workspaces/{slug}/issues/{number}/comments`

Body `{"body": "..."}`, sent exactly as given. Answers `201` with `{"url"}`,
the comment's web URL.

#### `POST /api/v1/workspaces/{slug}/issues/{number}/labels`

Body: a JSON array of 1 to 100 label names. They are added; the issue
keeps its others. Answers `204`. `Idempotency-Key` is ignored.

#### `DELETE /api/v1/workspaces/{slug}/issues/{number}/labels/{name}`

`{name}` is percent-decoded once, so `type%2Fbug` names `type/bug`. Answers
`204` when the issue exists, whether or not it carried the label, and
`404` `forge_not_found` when it does not.

#### `POST /api/v1/workspaces/{slug}/labels`

Body `{"name", "color"?, "description"?}`; `color` is six hex digits, with
or without `#`, default `ededed`. If the repository already has a label of
that name, the answer is `200` with the label as the forge holds it,
unchanged even when your `color` or `description` differ. Otherwise it is
created and the answer is `201`. Two callers racing to create it both get
the label. The label is `{name, color, description}`, `color` without `#`.

#### `POST /api/v1/workspaces/{slug}/pulls`

Body `{"title", "head", "base", "body"?, "draft"?}`; `title`, `head` and
`base` are required and `draft` defaults to `false`. `head` is sent as
given, so `owner:branch` names a branch of a forge-side fork. On GitLab a
draft gets the title prefix `Draft: `. Whether the branches exist and
whether a pull request is already open is the forge's to judge: its
refusal is `422` `forge_rejected`. Answers `201` with the pull request.

#### `POST /api/v1/workspaces/{slug}/pulls/{number}/comments`

Body `{"body": "..."}`: a comment in the pull request's conversation.
Answers `201` with `{"url"}`.

### Forge errors and rate limits

A request the hub refuses itself sends nothing to the forge:

| `error_type` | Status | What to do |
|---|---|---|
| `invalid_request` | `400` | fix the parameter, `target` or body the message names |
| `workspace_mode_mismatch` | `400` | `target=upstream` on a workspace without an upstream |
| `forge_unknown` | `422` | the remote names no repository, or the kind cannot be told: set `forge_api_url` (or `upstream_forge_api_url`) |
| `forge_api_url_invalid` | `422` | correct the setting |
| `forge_credential_missing` | `422` | a write without a stored credential: store `GIT_PAT` in the workspace's secrets |

When the forge was asked, its answer comes back under one of these, with
the forge's own status and message in `message`. The stored token is
removed from every message before it leaves the hub.

| `error_type` | Status | When |
|---|---|---|
| `forge_not_found` | `404` | the forge answered `404`: no such issue or pull request, or the credential cannot see the repository |
| `forge_unauthenticated` | `502` | the forge answered `401`: the stored token is wrong or expired |
| `forge_forbidden` | `502` | the forge answered `403`: the token lacks a permission |
| `forge_rejected` | `422` | the forge refused the request (another `4xx`) |
| `forge_rate_limited` | `429` | the forge's rate limit; see below |
| `forge_timeout` | `504` | no complete answer within 30 seconds |
| `forge_unreachable` | `502` | the connection failed |
| `forge_error` | `502` | the forge answered `5xx`, redirected to another host, or sent an answer the hub could not read |

Each request to the forge has 30 seconds. When the forge says it is rate
limited and its stated wait fits into what is left of those 30 seconds,
the hub waits and asks once more. Otherwise you get `429`
`forge_rate_limited`, with `Retry-After` in seconds when the forge stated
a wait: retry after it.

```json
{"error":{"code":404,"message":"the forge answered 404: 404 Not Found","error_type":"forge_not_found"}}
```

### Webhooks from the forge

Polling the forge for new issues or merged pull requests spends its rate
limit and is late. Instead, let the forge push: it sends its webhook
deliveries to the hub, the hub checks each one against a secret only that
workspace holds, records it once and relays it as `parley.forge.*` events.
Anything that follows the hub's events (see
[Audit and events](#audit-and-events) and
[Event webhooks](#event-webhooks)) then learns of forge activity the same
way it learns of everything else.

The hub never registers a webhook on the forge. Set-up has a step on each
side:

1. **On the hub**, create the secret. With no body the hub generates one
   and shows it this one time:

   ```sh
   curl -s -X PUT -H "Authorization: Bearer $PARLEY_TOKEN" \
     https://hub.example.com/api/v1/workspaces/demo/forge/webhook
   ```

   ```json
   {"configured":true,"url":"https://hub.example.com/hooks/forge/demo","secret":"9f2c…(64 hex characters)"}
   ```

2. **On the forge**, add a webhook in the repository's settings with that
   `url` and secret. On GitHub choose the content type `application/json`
   and put the secret in "Secret"; on GitLab put it in "Secret token".
   Select the events you want from the
   [table below](#post-hooksforgeslug).

3. **Check.** Trigger an event, then read the
   [delivery log](#get-apiv1workspacesslugforgedeliveries), or
   `webhook.last_delivery_at` of the diagnostic document.

The URL is the hub's `[server].external_url` followed by
`/hooks/forge/<slug>`; it is `null` when no `external_url` is configured
(see [configuration.md](configuration.md#server-and-storage)). The secret
is stored as the workspace secret `FORGE_WEBHOOK_SECRET` and read at every
delivery, so a rotation applies to the next one: rotate on the hub, then
update the forge. A delivery signed with the old secret in between is
refused with `401` and not recorded; redeliver it from the forge.

#### `PUT /api/v1/workspaces/{slug}/forge/webhook`

Sets or replaces the secret. Needs write access. The body is empty, `{}`,
or `{"secret": "..."}` with your own value of 16 to 255 printable ASCII
characters without spaces. The answer is `{configured, url}`, plus `secret`
only when the hub generated it; no later answer shows it. Records
`parley.forge.webhook.set`.

| `error_type` | Status | When |
|---|---|---|
| `fork_has_no_origin` | `400` | the slug names a task fork |
| `invalid_request` | `400` | the body or the secret breaks the rule above |
| `workspace_archived` | `409` | the workspace is archived |
| `secrets_unavailable` | `503` | the hub has no secret key (see [Protection at rest](#protection-at-rest)) |
| `secret_limit_reached` | `429` | the workspace already holds 100 secrets |

#### `DELETE /api/v1/workspaces/{slug}/forge/webhook`

Removes the secret, whether or not one existed, and answers `204`. Needs
write access; works on an archived workspace. From then on the ingress
answers `404` for the workspace; the recorded deliveries remain. Records
`parley.forge.webhook.unset`.

#### `POST /hooks/forge/{slug}`

The ingress the forge posts to. It is outside `/api/v1`, takes no bearer
credential and is not counted by the request rate limit; the delivery's
signature is its authentication. A body may be up to 1 MiB.

| Forge | Proof | Delivery id | Event name |
|---|---|---|---|
| GitHub | `X-Hub-Signature-256`: `sha256=` and the HMAC-SHA256 of the raw body under the secret | `X-GitHub-Delivery` | `X-GitHub-Event` |
| GitLab | `X-Gitlab-Token` equal to the secret | `Idempotency-Key`, else `X-Gitlab-Event-UUID` | `X-Gitlab-Event` |

| Answer | When |
|---|---|
| `202` `{"outcome":"relayed"}` | recorded, and its events are in the event log |
| `202` `{"outcome":"ignored"}` | recorded; the hub relays nothing for this event (GitHub's `ping`, for example) |
| `202` `{"outcome":"repository_mismatch"}` | recorded; the payload's repository is neither the workspace's origin nor its upstream |
| `202` `{"outcome":"duplicate"}` | the workspace already has a delivery with this id; nothing happens |
| `400` `invalid_request` | the id or event name header is missing, or the payload is not the JSON its event has (recorded as `malformed`; a form-encoded payload is one) |
| `401` `unauthenticated` | the signature or token is missing or wrong; nothing is recorded |
| `404` `not_found` | no such workspace, a task fork, or no webhook secret: the three are indistinguishable |
| `413` `payload_too_large` | the body is over 1 MiB |

Recording a delivery and writing its events is one step, completed before
the `202`: a delivery is relayed exactly once, however often the forge
sends it, and one that failed with `500` is processed as new when the
forge redelivers. An archived workspace still relays.

What is relayed:

| Hub event | GitHub event and action | GitLab hook and action |
|---|---|---|
| `parley.forge.issue.opened`, `.closed`, `.reopened` | `issues`: `opened`, `closed`, `reopened` | Issue Hook: `open`, `close`, `reopen` |
| `parley.forge.issue.edited` | `issues`: `edited` | Issue Hook: `update` that changes the title or description |
| `parley.forge.issue.labeled`, `.unlabeled` | `issues`: `labeled`, `unlabeled` | Issue Hook: `update` that changes the labels, one event per label |
| `parley.forge.comment.created` | `issue_comment`: `created` | Note Hook on an issue or merge request |
| `parley.forge.pull_request.opened`, `.reopened` | `pull_request`: `opened`, `reopened` | Merge Request Hook: `open`, `reopen` |
| `parley.forge.pull_request.synchronize` | `pull_request`: `synchronize` | Merge Request Hook: `update` with new commits |
| `parley.forge.pull_request.closed`, `.merged` | `pull_request`: `closed` (merged or not) | Merge Request Hook: `close`, `merge` |
| `parley.forge.push` | `push` | Push Hook, Tag Push Hook |

Everything else is `ignored`. Each relayed event has the actor type
`forge` and carries the `forge`, the `target` (`origin` or `upstream`),
the `delivery_id` and the forge login that caused it (`actor`). Issue
events add the `number`, `title`, `url` and `labels` (and `label` for a
label event); comment events the `number`, the `comment_url` and whether
it is on a pull request; pull request events the `number`, `url`, `head`,
`base` and `merged`; push events `ref`, `before` and `after`. No event
carries a body, a comment's text or a commit message: read those through
the routes above. `parley.forge.push` reports a push the forge received;
it is unrelated to a push to the hub.

#### `GET /api/v1/workspaces/{slug}/forge/deliveries`

The delivery log, newest first: what arrived and what the hub made of it.
Needs read access; works in every workspace state. It is a list (`limit`,
`cursor`) and takes `since`, an RFC 3339 instant, to keep the deliveries
received at or after it.

```json
{"items":[{"delivery_id":"d-1","forge":"github","event":"issues","action":"labeled","target":"origin","outcome":"relayed","relayed":["parley.forge.issue.labeled"],"received_at":"2026-10-21T09:12:03Z"}],"next_cursor":null}
```

| Member | Meaning |
|---|---|
| `delivery_id` | the forge's id of the delivery, unique per workspace |
| `forge` | `github` or `gitlab` |
| `event`, `action` | the forge's event name and the payload's action (`""` when it has none) |
| `target` | `origin`, `upstream`, or `null` when no repository matched |
| `outcome` | `relayed`, `ignored`, `repository_mismatch` or `malformed` |
| `relayed` | the event types written, in order |
| `received_at` | when the hub accepted it |

Deliveries that failed authentication are not in the log, since nothing
proves they came from the forge: if the forge reports `401`, the secrets
on the two sides differ. Payloads are never stored. Entries are kept for 7
days, and deleting a workspace deletes its log.

## Secrets and variables

The hub keeps named values for a user, an org and a workspace. They come
in two kinds because they answer two different needs:

- A **secret** is a credential. You can write it, replace it and delete
  it, but no route ever returns its value: not to you, not to an admin.
  Only the hub uses it, for the purposes listed under
  [Where secrets are used](#where-secrets-are-used). It is encrypted at
  rest.
- A **variable** is plain configuration (`LOG_LEVEL`, `REGION`) that an
  agent reads to configure itself. Its value is returned by every list and
  stored in the clear. Variables never change how the hub behaves; that is
  what workspace settings are for (see [Workspaces](#workspaces)).

The two are separate collections with the same shape. The same key may
name a secret and a variable at the same level without conflict.

### Levels and resolution

| Level | Routes below | Holds values for |
|---|---|---|
| user | `/api/v1/user` | the calling user |
| org | `/api/v1/orgs/{slug}` (slug or id) | every workspace of the org |
| workspace | `/api/v1/workspaces/{slug}` | that workspace or task fork alone |

A route at one level lists and changes only that level's own entries. What
a workspace actually runs with is **resolved** along a chain, highest
priority first:

| `origin` | Level | Included |
|---|---|---|
| `workspace` | the workspace's own | always |
| `parent` | the parent workspace's | for a task fork |
| `org` | the workspace's org (a fork's: its parent's) | while the workspace's owner is a member and the org is active |
| `user` | the workspace owner's (a fork's: its creator's) | always |

For each key, compared ignoring case, the first level that holds it wins
entirely. So an org sets a default, a workspace overrides it, and a fork
overrides its parent; delete the override and the lower value shows again.
The chain depends on the workspace's owner, never on the caller: everyone
who asks gets the same answer.

Variables resolve this way through
[`GET /api/v1/workspaces/{slug}/vars/resolved`](#get-apiv1workspacesslugvarsresolved).
Secrets resolve the same way when the hub equips a
[sandbox](#sandboxes), with one restriction: a fork gets its parent's
secrets only when the fork's owner has write access to the parent. A fork
created by a read-only org member runs with the parent's variables and
none of its secrets.

### Who may read and write

A PAT needs `secrets:read`, `secrets:write`, `vars:read` or `vars:write`;
the write scope satisfies the read scope.

| Level | Secrets (every operation) | Variables: read | Variables: write |
|---|---|---|---|
| user | the user (API key or PAT) | the same | the same |
| org | org members, admin token | the same | the same |
| workspace | owner, admin token | owner, org members, admin token | owner, admin token |

- The admin token has no user: on the user level it gets `403`
  `forbidden`.
- An org you are not a member of, a blocked org and an unknown org all
  answer the same `404` `not_found`.
- On a workspace, an org member gets `403` `forbidden` where the table
  says no, so members cannot even list a workspace's secret names. Anyone
  without access gets `404` `not_found`. On a task fork, the parent's
  owner counts as owner. The workspace's state does not matter.
- A workspace-bound token (see
  [Task forks and workspace tokens](#task-forks-and-workspace-tokens))
  reaches only the workspace level of the workspaces it is valid on.

Every create, update and delete records one event per entry:
`parley.secret.create`, `.update`, `.delete` and `parley.variable.create`,
`.update`, `.delete`. An event names the level, its owner and the key,
never a value. Entries go when their owner goes: deleting a user, an org
or a workspace (a fork's expiry included) deletes its secrets and
variables without events, and a slug used again starts empty.

### Protection at rest

Secret values are encrypted with a key the operator supplies
(`PARLEY_SECRETS_KEY` or `[secrets].key_file`; see
[configuration.md](configuration.md#secret-keys)). A copy of the hub's
storage does not disclose them, and losing the key loses them. What this
means for a caller:

- **No key configured.** Storing a secret answers `503`
  `secrets_unavailable`: ask the operator to configure a key. Listing and
  deleting secrets and all variable routes keep working.
- **Key rotation** is invisible: the operator supplies the old key next to
  the new one and the hub re-encrypts in the background.
- **A value the hub can no longer decrypt** (its key is gone) stays
  listed. Replace it with `PATCH` or delete it. Until then, whatever needs
  that value fails with an error instead of going on without a credential.

A malformed key stops the hub at start, before it opens its stores or
listens, so a hub that runs has usable keys or none
([ADR 06](adr/06-load-the-secret-keys-before-the-stores-open.md)).

### Where secrets are used

The hub blocks no key name, but reads some itself, and only from the
workspace level of the workspace concerned:

| Workspace secret | Used as |
|---|---|
| `GIT_PAT` | the credential for the origin repository (clone, sync, push) and the [forge](#forge) token |
| `GIT_USERNAME` + `GIT_PASSWORD` | the same, when `GIT_PAT` is absent |
| `UPSTREAM_GIT_PAT`, or `UPSTREAM_GIT_USERNAME` + `UPSTREAM_GIT_PASSWORD` | the upstream credential of a [carry-patch workspace](#carry-patch-workspaces); without them the origin credential |
| `FORGE_WEBHOOK_SECRET` | verifies [forge webhook](#webhooks-from-the-forge) deliveries; set it with the webhook route |

They are read at every use, so a new value applies to the next operation.
An empty value counts as not set. A task fork never uses these names: it
has no origin of its own.

Any other secret is used only by [sandboxes](#sandboxes), resolved along
the chain when a sandbox is created and at each start. Normally it becomes
a sandbox credential that the runtime attaches to outgoing requests
outside the sandbox, so the workload never sees the value. Only when the
operator names a secret in a sandbox profile's exposed secrets is its real
value put into the sandbox's environment. The reserved names above are
never handed to a fork's sandbox from its parent and can never be exposed.

### Example: an org default, a workspace override

```sh
H="Authorization: Bearer $PARLEY_TOKEN"; J="Content-Type: application/json"
API=https://hub.example.com/api/v1

# A default for every workspace of the org
curl -s -X POST -H "$H" -H "$J" "$API/orgs/acme/vars" \
  -d '{"entries":[{"key":"LOG_LEVEL","value":"info"}]}'

# One workspace overrides it
curl -s -X POST -H "$H" -H "$J" "$API/workspaces/widgets/vars" \
  -d '{"entries":[{"key":"LOG_LEVEL","value":"debug"}]}'

# What the workspace runs with
curl -s -H "$H" "$API/workspaces/widgets/vars/resolved"
```

The last call answers `LOG_LEVEL` `debug` with `"origin":"workspace"`;
every other workspace of the org resolves `info` with `"origin":"org"`.

```sh
# Store the forge token for the workspace. It cannot be read back.
curl -s -X POST -H "$H" -H "$J" "$API/workspaces/widgets/secrets" \
  -d '{"entries":[{"key":"GIT_PAT","value":"ghp_…"}]}'
```

```json
{"items":[{"key":"GIT_PAT","created_at":"2026-11-14T10:30:15Z","updated_at":"2026-11-14T10:30:15Z"}]}
```

```sh
# Rotate it later
curl -s -X PATCH -H "$H" -H "$J" "$API/workspaces/widgets/secrets/GIT_PAT" \
  -d '{"value":"ghp_…"}'
```

### Secret and variable routes

`{level}` stands for `/api/v1/user`, `/api/v1/orgs/{slug}` or
`/api/v1/workspaces/{slug}`; every route below exists at all three, except
the resolved variables.

| Route | Scope | What it does |
|---|---|---|
| [`GET {level}/secrets`](#get-levelsecrets-and-get-levelvars) | `secrets:read` | list the level's secrets, without values |
| [`POST {level}/secrets`](#post-levelsecrets-and-post-levelvars) | `secrets:write` | create secrets |
| [`PATCH {level}/secrets/{key}`](#patch-levelsecretskey-and-patch-levelvarskey) | `secrets:write` | replace a secret's value |
| [`DELETE {level}/secrets/{key}`](#delete-levelsecretskey-and-delete-levelvarskey) | `secrets:write` | delete a secret |
| [`GET {level}/vars`](#get-levelsecrets-and-get-levelvars) | `vars:read` | list the level's variables, with values |
| [`POST {level}/vars`](#post-levelsecrets-and-post-levelvars) | `vars:write` | create variables |
| [`PATCH {level}/vars/{key}`](#patch-levelsecretskey-and-patch-levelvarskey) | `vars:write` | replace a variable's value |
| [`DELETE {level}/vars/{key}`](#delete-levelsecretskey-and-delete-levelvarskey) | `vars:write` | delete a variable |
| [`GET /api/v1/workspaces/{slug}/vars/resolved`](#get-apiv1workspacesslugvarsresolved) | `vars:read` | the variables a workspace runs with |

Limits, the same for both kinds:

| Rule | Value |
|---|---|
| Key | 1 to 255 ASCII letters, digits and `_`, not starting with a digit |
| Key matching | ignores case; the key keeps the casing it was created with |
| Value | a string of at most 262,144 bytes; may be empty |
| Entries | 100 secrets and 100 variables per user, org or workspace |

The access errors are those of [Who may read and write](#who-may-read-and-write),
and the common rules those of [Using the API](#using-the-api). No route
reads a single entry by key.

### `GET {level}/secrets` and `GET {level}/vars`

A list of the level's own entries, ordered by key ignoring case. A secret
is `{key, created_at, updated_at}`; a variable adds `value`. A fork's list
does not include its parent's entries.

```json
{"items":[{"key":"LOG_LEVEL","value":"info","created_at":"2026-11-17T10:30:15Z","updated_at":"2026-11-17T10:30:15Z"}],"next_cursor":null}
```

### `POST {level}/secrets` and `POST {level}/vars`

Creates one or more entries, all or none. Body: `{"entries": [{"key",
"value"}, ...]}` with at least one entry; a missing `value` is the empty
string. Accepts `Idempotency-Key`. Answers `201` with `{"items": [...]}` in
request order, in the shape of the list (secrets without values).

| `error_type` | Status | When |
|---|---|---|
| `invalid_request` | `400` | a bad key or value, or the same key twice in the request; the message names the key |
| `conflict` | `409` | the level already holds one of the keys: use `PATCH` to replace a value |
| `secret_limit_reached` | `429` | the level would hold more than 100 entries (variables use the same `error_type`) |
| `secrets_unavailable` | `503` | secrets only: the hub has no secret key |

### `PATCH {level}/secrets/{key}` and `PATCH {level}/vars/{key}`

Replaces the value. Body: `{"value": "..."}`; `""` is valid, a missing or
`null` value is `400` `invalid_request`. The entry keeps its key casing
and `created_at`. Answers `200` with the entry (a secret without its
value). A key the level does not hold is `404` `not_found`, also when a
parent or another level holds it. A secret update answers `503`
`secrets_unavailable` on a hub without a secret key; it works on a value
the hub can no longer decrypt.

### `DELETE {level}/secrets/{key}` and `DELETE {level}/vars/{key}`

Deletes the entry and answers `204`; `404` `not_found` when the level does
not hold the key. Works without a secret key. Deleting
`FORGE_WEBHOOK_SECRET` turns the workspace's forge webhook off.

Under `/api/v1/workspaces/{slug}/vars/` the name `resolved` belongs to the
route below: `PATCH` and `DELETE` on it answer `405`. A variable you named
`resolved` is reached under another casing, such as `RESOLVED`.

### `GET /api/v1/workspaces/{slug}/vars/resolved`

The variables the workspace or task fork runs with, resolved along the
[chain](#levels-and-resolution): a list ordered by key, each item
`{key, value, origin, created_at, updated_at}` taken whole from the level
that won, with `origin` `workspace`, `parent`, `org` or `user`. It needs
read access to `{slug}` and nothing else: you do not need access to the
org or the owner's user level to see what they contribute.

```json
{"items":[{"key":"LOG_LEVEL","value":"info","origin":"org","created_at":"2026-11-17T10:30:15Z","updated_at":"2026-11-17T10:30:15Z"},{"key":"REGION","value":"eu","origin":"parent","created_at":"2026-11-17T10:32:00Z","updated_at":"2026-11-17T10:32:00Z"}],"next_cursor":null}
```

## Sandboxes

A sandbox is where an agent runs. It is an isolated environment, apart from
the machine that started it: it holds no real credential unless the operator
exposed one by name, and it reaches the hub and nothing else on the network
unless the operator allowed more. You reach for one when you want an agent,
a test run or any untrusted command to work on a task fork without trusting
it with your machine, your keys or the open internet. What the workload did
is saved back to the fork before the sandbox goes away.

**Who does what.** Parley does not isolate anything itself. Sandboxes run on
OpenShell, a sandbox runtime the operator points the hub at. OpenShell
confines the filesystem, the processes and the network, enforces the network
policy and substitutes credentials. The hub is the control plane on top: it
decides who may have a sandbox and of which shape, checks the fork out in
it, gives it a credential for the hub, picks the secrets it may use,
enforces time limits and quotas, saves work as checkpoints, puts requests
for more network access in front of a person, and records what happened. You
need only this API to use a sandbox; no OpenShell client or account.

**Profiles.** A profile is the operator's template for a sandbox: image,
CPU, memory, time limits, base network policy, the credentials attached, the
secrets exposed, whether it gets a hub credential, checkpoints and tools.
Profiles are defined in the hub's
[configuration](configuration.md#sandboxes), and only a profile decides what
a sandbox may hold: nothing in a create request widens it. List them with
[`GET /api/v1/sandbox-profiles`](#get-apiv1sandbox-profiles).

**Birth.** You create a sandbox on a workspace, normally a task fork. The
hub answers at once with the sandbox in state `provisioning` and builds it
in the background: it attaches the credentials, clones the fork into
`/workspace` with the fork's branch checked out and git already able to
push to the hub, starts your `command` if you gave one, and moves the
sandbox to `ready`. Without a `command` the sandbox idles and you drive it
with [exec](#post-apiv1workspacesslugsandboxesidexec).

**When the hub has no sandboxes configured.** Sandboxes are optional. On a
hub without a `[sandbox]` section, every route of this section answers every
authenticated caller `404` with `error_type` `sandboxes_disabled`, whatever
the credential and whether or not the workspace exists. Nothing else about
the hub changes.

**What a real gateway carries today.** The hub never imitates an operation
its runtime lacks: the request fails instead. The OpenShell client the hub
is built on does not yet carry every operation described here; see
[Sandboxes in the architecture](architecture.md#sandboxes) for which ones,
and plan your profiles around it.

### A sandbox from creation to deletion

Create a sandbox on the fork `demo--t1`, run the tests in it, save the work
and delete it. `PARLEY_ENDPOINT_URL` is the hub's URL and `PARLEY_API_KEY`
your API key or a PAT with `sandboxes:write`.

```sh
# Create: answers 201 with the sandbox in state "provisioning".
curl -sS -X POST "$PARLEY_ENDPOINT_URL/api/v1/workspaces/demo--t1/sandboxes" \
  -H "Authorization: Bearer $PARLEY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"profile":"agent","ttl":"1h","purpose":"run tests"}'

# Wait up to 60 seconds for it to leave "provisioning"; check "state".
SBX=sbx_3f2a9c0e7b1d4a5e8f6c2b9a0d1e4f7a
curl -sS -H "Authorization: Bearer $PARLEY_API_KEY" \
  "$PARLEY_ENDPOINT_URL/api/v1/workspaces/demo--t1/sandboxes/$SBX?wait_for=ready&wait=60"

# Run a command in the checked-out fork.
curl -sS -X POST \
  "$PARLEY_ENDPOINT_URL/api/v1/workspaces/demo--t1/sandboxes/$SBX/exec" \
  -H "Authorization: Bearer $PARLEY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"command":["make","test"],"timeout":"10m"}'


# The answer holds exit_code, stdout and stderr; see the exec route below.

# Save unpushed commits and uncommitted changes to the fork on the hub.
curl -sS -X POST -H "Authorization: Bearer $PARLEY_API_KEY" \
  "$PARLEY_ENDPOINT_URL/api/v1/workspaces/demo--t1/sandboxes/$SBX/checkpoint"

# Delete: answers 204. The record stays readable for 24 hours.
curl -sS -X DELETE -H "Authorization: Bearer $PARLEY_API_KEY" \
  "$PARLEY_ENDPOINT_URL/api/v1/workspaces/demo--t1/sandboxes/$SBX?checkpoint=false"
```

### Sandbox routes

The rules that hold for every route are in [Using the API](#using-the-api).
The routes under `/api/v1/workspaces/{slug}/sandboxes` are written `…` below.
A read route needs read access to the workspace and, for a PAT,
`sandboxes:read`; a write route needs write access and `sandboxes:write`.

| Route | Scope | What it does |
|---|---|---|
| [`POST …`](#post-apiv1workspacesslugsandboxes) | `sandboxes:write` | creates a sandbox |
| [`GET …/{id}`](#get-apiv1workspacesslugsandboxesid) | `sandboxes:read` | reads a sandbox, optionally waiting for a state |
| [`GET …`](#get-apiv1workspacesslugsandboxes) | `sandboxes:read` | lists a workspace's sandboxes |
| [`PATCH …/{id}`](#patch-apiv1workspacesslugsandboxesid) | `sandboxes:write` | extends it, or changes its purpose or labels |
| [`POST …/{id}/checkpoint`](#post-apiv1workspacesslugsandboxesidcheckpoint) | `sandboxes:write` | saves its git state to the workspace |
| [`POST …/{id}/stop`](#post-apiv1workspacesslugsandboxesidstop) | `sandboxes:write` | releases its compute, keeps its working directory |
| [`POST …/{id}/start`](#post-apiv1workspacesslugsandboxesidstart) | `sandboxes:write` | starts a stopped or ended sandbox again |
| [`DELETE …/{id}`](#delete-apiv1workspacesslugsandboxesid) | `sandboxes:write` | deletes it |
| [`POST …/{id}/exec`](#post-apiv1workspacesslugsandboxesidexec) | `sandboxes:write` | runs a command |
| [`PUT …/{id}/files`](#put-apiv1workspacesslugsandboxesidfiles) | `sandboxes:write` | uploads a file or a tar archive |
| [`GET …/{id}/files`](#get-apiv1workspacesslugsandboxesidfiles) | `sandboxes:read` | downloads a file or a directory |
| [`GET …/{id}/logs`](#get-apiv1workspacesslugsandboxesidlogs) | `sandboxes:read` | reads or follows its output and policy log |
| [`GET …/{id}/attach`](#get-apiv1workspacesslugsandboxesidattach) | `sandboxes:write` | opens a shell over a WebSocket |
| [`GET …/{id}/tools`](#get-apiv1workspacesslugsandboxesidtools) | `sandboxes:read` | lists the tools the caller may call |
| [`POST …/{id}/tools/{name}`](#post-apiv1workspacesslugsandboxesidtoolsname) | `sandboxes:read` | calls a tool |
| [`POST …/{id}/mcp`](#post-apiv1workspacesslugsandboxesidmcp) | `sandboxes:read` | serves the tools to an MCP client |
| [`GET …/{id}/proposals`](#get-apiv1workspacesslugsandboxesidproposals) | `sandboxes:read` | lists its requests for network access |
| [`POST …/{id}/proposals/{proposal_id}/approve`](#post-apiv1workspacesslugsandboxesidproposalsproposal_idapprove) | `sandboxes:write` | approves a request |
| [`POST …/{id}/proposals/{proposal_id}/reject`](#post-apiv1workspacesslugsandboxesidproposalsproposal_idreject) | `sandboxes:write` | rejects a request |
| [`GET /api/v1/workspaces/{slug}/sandbox-rules`](#get-apiv1workspacesslugsandbox-rules) | `sandboxes:read` | lists the rules kept for a workspace |
| [`DELETE /api/v1/workspaces/{slug}/sandbox-rules/{rule_id}`](#delete-apiv1workspacesslugsandbox-rulesrule_id) | `sandboxes:write` | deletes one |
| [`GET /api/v1/sandbox-profiles`](#get-apiv1sandbox-profiles) | `sandboxes:read` | lists the profiles; needs no workspace |
| [`GET /api/v1/sandboxes`](#get-apiv1sandboxes) | admin token only | lists every sandbox of the hub |

### Rules for every sandbox route

- **Scope before access.** A PAT without the scope gets `403
  insufficient_scope`, also on a slug that names no workspace.
- **The admin token** passes as an owner of every workspace, so it can stop
  or delete any sandbox, but it cannot create one: a sandbox acts for a
  user (`403 forbidden`).
- **An unknown sandbox.** An id that is malformed, unknown or of another
  workspace is `404 sandbox_not_found`.
- **The runtime is down.** A route that needs OpenShell answers `502
  sandbox_runtime_error` when it cannot be reached or fails. Do not branch
  on the message. The hub keeps serving, and the routes that read only its
  own records (reads, lists, logs, profiles, proposals) keep answering.
- **No widening.** Exec, attach and an upload act with the sandbox's
  [hub credential](#credentials-and-secrets-in-a-sandbox), so your
  credential must cover it: a weak token cannot act through a sandbox a
  stronger one created. An API key and the admin token cover every sandbox,
  and every caller covers a sandbox whose `hub_access` is `null`. A PAT
  must hold every scope in `hub_access.scopes` (a `:write` scope counts for
  its `:read`), and if the PAT is limited to `paths`, the sandbox's
  `hub_access.paths` must lie within them. Otherwise the answer is `403
  insufficient_scope` and nothing is run or written.
- **Secrets stay out of texts.** In every error, event and log line about a
  sandbox, the values of its credentials, its hub credential and its
  exposed secrets (of 8 characters or more) read `[redacted]`.

### The sandbox object

Every answer that shows a sandbox shows this object:

```json
{"id":"sbx_3f2a9c0e7b1d4a5e8f6c2b9a0d1e4f7a","workspace":"demo--t1","session_id":null,"profile":"agent","purpose":"run tests","state":"ready","state_reason":null,"command":["make","test"],"env":{"CI":"1"},"exit_code":null,"checkout":{"path":"/workspace","ref":"main","sha":"9fceb02d0ae598e95dc970b74767f19372d61af8","branch":"t1"},"image":"img:1","cpu":"2","memory":"4Gi","gpu":0,"credentials":["model"],"exposed_secrets":["NPM_TOKEN"],"workspace_rules":[],"hub_access":{"scopes":["audit:write","changes:read","changes:write","git:read","git:write","sessions:write"],"paths":null,"expires_at":"2026-11-18T13:00:00Z"},"policy_revision":0,"expires_at":"2026-11-18T13:00:00Z","idle_timeout_seconds":1800,"last_activity_at":"2026-11-18T12:00:00Z","checkpoint":null,"restored_from":null,"runtime_reachable":true,"labels":{"team":"core"},"created_by":{"actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","username":"dana"},"created_at":"2026-11-18T12:00:00Z","updated_at":"2026-11-18T12:00:04Z","ended_at":null,"tools":false}
```

| Member | Meaning |
|---|---|
| `id` | `sbx_` and 32 lowercase hexadecimal characters |
| `workspace` | the slug of the workspace it is attached to |
| `session_id` | the agent session it belongs to, or `null` |
| `profile` | the profile it was created from |
| `purpose`, `labels` | what its creator said about it; `""` and `{}` when nothing |
| `state`, `state_reason` | see [States](#sandbox-states) |
| `command` | the main process's command, or `null` for a sandbox driven by exec |
| `env` | the `env` its creator gave |
| `exit_code` | the main process's exit code once it ended, else `null` |
| `checkout` | `{path, ref, sha, branch}`: the working directory (`/workspace`), the starting ref, the commit checked out and the work branch (`null` on a workspace that is not a fork); `null` when there is no checkout |
| `image`, `cpu`, `memory`, `gpu` | the profile's values when the sandbox was created |
| `credentials` | the names of the credentials attached to it |
| `exposed_secrets` | the names of the secrets whose real values its environment holds; never a value |
| `workspace_rules` | the ids of the [workspace sandbox rules](#the-network-policy-of-a-sandbox) it was created with |
| `hub_access` | `{scopes, paths, expires_at}` of its hub credential, or `null` when it has none; never the credential itself |
| `policy_revision` | `0` at creation, plus one for every approved proposal |
| `expires_at` | when the hub deletes it |
| `idle_timeout_seconds` | how long it may be idle before the hub stops it; `0` when the idle stop is off |
| `last_activity_at` | the time of its last activity (see [Time limits](#sandbox-time-limits)) |
| `checkpoint` | its latest [checkpoint](#sandbox-checkpoints) attempt, or `null` |
| `restored_from` | the sandbox whose checkpoint it was restored from, or `null` |
| `runtime_reachable` | `false` while the hub cannot reach OpenShell; the other members then show what the hub last stored |
| `created_by` | `{actor_type, actor_id, username}` of the creator |
| `created_at`, `updated_at` | timestamps |
| `ended_at` | when it stopped, completed, failed or was deleted; `null` until then, and again after a start |
| `tools` | `true` when its profile named a tool server, so that [the tools](#tools-for-an-agent-outside-the-sandbox) are served for it |

### Sandbox states

| State | Meaning | `state_reason` |
|---|---|---|
| `provisioning` | the hub is building or starting it | `null` |
| `ready` | it runs; exec, files, attach and tools work | `null` |
| `stopped` | its compute is released; the working directory is kept | `api` (a stop request), `idle` (the idle timeout), `archived` (its workspace was archived) |
| `completed` | the main process ended with exit code 0 | `null` |
| `failed` | provisioning failed, the main process ended with another code, or the runtime lost it | `provision_failed`, `checkout_failed`, `credential_missing`, `exit`, `runtime_error` |
| `deleted` | it is gone at the runtime; final | `api`, `expired`, `workspace_deleted` |

What moves a sandbox:

- A new sandbox is `provisioning` and becomes `ready` or `failed` without a
  further request. A provisioning that has not finished after 10 minutes
  fails. A failed create leaves nothing at the runtime.
- A `ready` sandbox becomes `completed` or `failed` (`exit`) when its main
  process ends, `stopped` by a stop, and `failed` (`runtime_error`) when
  the runtime no longer has it.
- A [start](#post-apiv1workspacesslugsandboxesidstart) moves a `stopped`,
  `completed` or `failed` sandbox back to `provisioning`.
- Every state but `deleted` becomes `deleted`, by request, at `expires_at`,
  or with its workspace.

Each move happens once: of two requests, or a request and the hub's own
work, racing for one sandbox, one makes the move and one event is written.

**Restarts.** Stopping or restarting the hub ends no sandbox: a `ready`
sandbox is still `ready` afterwards, and one that was being provisioned is
provisioned again from the start. At start and at every
`[sandbox].reconcile_interval` the hub compares its records with the runtime
and records main processes that ended, sandboxes the runtime lost, and the
idle stops and expiries that came due.

**With the workspace.** Archiving a workspace stops the `ready` sandboxes of
it and of its forks (`archived`), and a start is refused while it stays
archived. Deleting a workspace or a fork, by request or by expiry, deletes
its sandboxes (`workspace_deleted`) without a checkpoint, and their records
and checkpoint refs go with it. A fork with a `provisioning` or `ready`
sandbox is not removed early after its changes landed.

**Events.** A subscriber of the [event stream](#audit-and-events) follows a
sandbox by `parley.sandbox.create`, `.ready`, `.fail` (with `reason` and
`error`), `.exit` (with `exit_code`), `.stop`, `.start`, `.delete` (each
with `reason` where one applies) and `.checkpoint`; the network policy adds
`.deny`, `.proposal`, `.proposal_decided`, `.rule_add` and `.rule_remove`,
and a tool call `.tool_call`. Exec, file transfers, attach and log reads
write no event.

### Sandbox checkpoints

A checkpoint saves the git state of the working directory to the sandbox's
workspace on the hub, so that work survives the sandbox. For every local
branch whose head commit the hub does not have, and for the checked-out
branch when it has uncommitted changes, it writes a ref
`refs/parley/sandboxes/<sandbox id>/<branch>` in the workspace's repository.
Uncommitted changes, new files that git does not ignore included, are saved
as one extra commit on top of the branch head; the branches, the index and
the working tree in the sandbox stay as they were.

- The refs are not a version of a change and trigger nothing. They live as
  long as the workspace; a fork's go with the fork.
- Files that git ignores are not saved. An untracked file over 100 MiB is
  skipped and named in `error` of a checkpoint that is still `ok`.
- A checkpoint stores all its refs or none. It fails when it takes more
  than 5 minutes or when its commits are larger than `[git].max_push_size`.
- A sandbox without a hub credential (profile `hub_access` `none`) gets no
  checkpoint. One without a checkout succeeds with no refs.

When the profile's `checkpoint` is true, the hub takes one of a `ready`
sandbox before a stop, before a deletion (the `checkpoint` query of `DELETE`
overrides this), at expiry and when the main process has ended. A failed
checkpoint never keeps the stop or the deletion from happening. You can
take one at any time with
[`POST …/checkpoint`](#post-apiv1workspacesslugsandboxesidcheckpoint).

The sandbox's `checkpoint` member holds the latest attempt:

```json
{"at":"2026-11-18T12:30:00Z","ok":true,"error":null,"refs":[{"branch":"t1","sha":"4c1f0e9b7a2d3c5e6f8091a2b3c4d5e6f7081920","dirty":true}]}
```

`dirty` is true for a ref that ends in the extra commit of uncommitted
changes; with no branch checked out the branch is `HEAD`. A failed attempt
has `ok` false, its reason in `error`, and the `refs` of the last checkpoint
that succeeded, which still exist. Each attempt writes one
`parley.sandbox.checkpoint` event with `{ok, refs, error}`.

To continue from a checkpoint, create a new sandbox with `restore_from`: its
checkout has each checkpointed branch at its commit and the uncommitted
changes back in the working tree, uncommitted. A start of a stopped sandbox
does the same when the runtime no longer has its working directory.

### Sandbox time limits

- **Expiry.** `expires_at` is the creation time plus `ttl` (the profile's
  when you give none), and never later than a task fork's own `expires_at`.
  At that time the hub deletes the sandbox, in whatever state, with
  `state_reason` `expired`, after a checkpoint of a `ready` one. Extend it
  with [`PATCH`](#patch-apiv1workspacesslugsandboxesid).
- **Idle stop.** Every minute the hub stops each `ready` sandbox whose
  `last_activity_at` is more than `idle_timeout_seconds` ago (`idle`).
  Activity is an exec starting or ending, a file transfer, a tool call, an
  attach opening or closing or receiving input, and a line of output from
  the main process. A refused request, a log read and a tool listing are
  not activity.
- **Records.** A deleted sandbox stays readable, with its logs, for 24
  hours; then its id answers `404 sandbox_not_found`.

### Credentials and secrets in a sandbox

A workload gets what it needs to work without getting anything worth
stealing.

- **Attached credentials** (the profile's `credentials` and
  `optional_credentials`). The hub resolves each credential's secret along
  the workspace's chain (workspace, parent of a fork, org, owner; see
  [Secrets and variables](#secrets-and-variables)) and hands it to the
  runtime, not to the sandbox. The workload's environment holds a
  placeholder. The runtime puts the real value on requests to the endpoints
  the operator named for that credential, outside the sandbox, so code
  inside cannot read it or send it elsewhere.
- **The hub credential.** With a profile whose `hub_access` is `fork`, the
  hub mints a token bound to the sandbox's workspace (see
  [Task forks and workspace tokens](#task-forks-and-workspace-tokens)),
  owned by the creating user and expiring with the sandbox. It holds those
  of `git:read`, `git:write`, `changes:read`, `changes:write`,
  `sessions:write` and `audit:write` that the creating credential holds (an
  API key holds all six), and the creating PAT's `paths`. The workload sees
  it as the placeholder in `PARLEY_API_KEY`. It is revoked whenever the
  sandbox loses its compute (stop, deletion, end of the main process,
  failure), and a start mints a new one. Without `[server].external_url`,
  or when the creator holds none of the six scopes, the sandbox has no hub
  credential (`hub_access` `null`), and then no checkout and no checkpoint.
- **Exposed secrets** (the profile's `expose_secrets` and
  `optional_expose_secrets`). For a tool that cannot work with a
  placeholder, the operator can expose a secret by name: its real value is
  then in the sandbox's environment under that name, and code in the sandbox
  can read it and send it wherever the network policy allows. The sandbox
  object and the profile list name the exposed secrets, never their values.

A required credential or secret that resolves nowhere refuses the create
(`422 credential_missing`); an optional one is skipped. Values are read
again at every start, so a secret rotated while a sandbox was stopped
reaches it.

The environment of a sandbox holds your `env`, then the profile's `env`,
then the exposed secrets, then the hub's own variables, later ones winning:
`PARLEY_WORKSPACE`, `PARLEY_SANDBOX_ID`, `PARLEY_SESSION_ID` (with a
session) and, with a hub credential, `PARLEY_ENDPOINT_URL`, `PARLEY_REMOTE`
(the workspace's git URL) and `PARLEY_API_KEY`.

### The network policy of a sandbox

A sandbox reaches the hub, the endpoints of its credentials and what its
operator allowed, and nothing else. Its policy is made of:

1. the base policy of its profile;
2. protected rules for the hub (its own workspace's API and git paths) and
   for each attached credential, which nothing removes or narrows;
3. added rules: the workspace sandbox rules that existed when it was
   created, then the rules approved for the sandbox itself.

**Denials.** A blocked connection or request shows as a `policy` line with
`action` `deny` in the sandbox's
[logs](#get-apiv1workspacesslugsandboxesidlogs). The first denial of each
program, host and port of a sandbox also writes a `parley.sandbox.deny`
event with `program`, `host`, `port`, `method` and `path` (the last two
`null` for a blocked connection).

**Proposals.** A blocked workload can ask, through OpenShell's own
mechanism, for one more rule. When the sandbox's profile has
`allow_proposals`, the hub stores the request as an `open` proposal within
a few seconds and writes `parley.sandbox.proposal` with `proposal_id`,
`summary` and `max_severity`; otherwise the request is rejected at the
runtime and nothing is stored. A person then
[approves](#post-apiv1workspacesslugsandboxesidproposalsproposal_idapprove)
or
[rejects](#post-apiv1workspacesslugsandboxesidproposalsproposal_idreject)
it with the runtime's risk findings in front of them. A workspace-bound
token cannot decide, so a workload cannot approve its own request with its
hub credential. The open proposals of a deleted sandbox become `withdrawn`.

```json
{"id":"sbp_0c9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f","sandbox_id":"sbx_3f2a9c0e7b1d4a5e8f6c2b9a0d1e4f7a","rule":{"host":"registry.example.org","port":443},"summary":"npm → registry.example.org:443","findings":[{"severity":"high","message":"a package registry can serve arbitrary code"}],"state":"approved","scope":"workspace","workspace_rule_id":"sbr_5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b","decided_by":{"actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","username":"dana"},"decided_at":"2026-11-18T12:03:00Z","comment":"npm registry","created_at":"2026-11-18T12:01:05Z"}
```

| Member | Meaning |
|---|---|
| `id` | `sbp_` and 32 lowercase hexadecimal characters |
| `sandbox_id` | the sandbox that asked |
| `rule` | the proposed rule in OpenShell's policy schema; the hub stores it and passes it on unread |
| `summary` | the runtime's one line about it |
| `findings` | the runtime's risk findings, `{severity, message}` each (`low`, `medium`, `high`) |
| `state` | `open`, `approved`, `rejected` or `withdrawn` |
| `scope` | `sandbox` or `workspace` once approved, else `null` |
| `workspace_rule_id` | with scope `workspace`, the workspace sandbox rule the approval saved or found identical |
| `decided_by`, `decided_at`, `comment` | set by a decision, else `null` |
| `created_at` | when the hub stored it |

**Workspace sandbox rules.** An approval with `scope` `workspace` keeps the
rule on the repository workspace, so that every sandbox created later on it
or on any of its forks has the rule from the start and does not have to ask
again. A task fork holds none of its own: its slug stands for its parent's
rules. A workspace holds at most 50, and the operator can turn the feature
off with `[sandbox].allow_workspace_rules`. A sandbox's set is fixed at its
creation and shown as `workspace_rules`: a rule saved later never reaches a
running sandbox, and a rule deleted later stays in the sandboxes that have
it, across stop and start. A sandbox created with `restore_from` is a new
sandbox: it gets the workspace rules of its own creation and none of the
rules approved for its source alone.

```json
{"id":"sbr_5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b","rule":{"host":"registry.example.org","port":443},"summary":"npm → registry.example.org:443","findings":[{"severity":"high","message":"a package registry can serve arbitrary code"}],"from_proposal":{"sandbox_id":"sbx_3f2a9c0e7b1d4a5e8f6c2b9a0d1e4f7a","proposal_id":"sbp_0c9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f","workspace":"demo--t1"},"created_by":{"actor_type":"api_key","actor_id":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d","username":"dana"},"created_at":"2026-11-18T12:03:00Z"}
```

`from_proposal` names the proposal the rule came from and the workspace of
its sandbox, which may be a fork; `summary` and `findings` are the
proposal's as they were acknowledged.

### Tools for an agent outside the sandbox

An agent does not have to run inside its sandbox. When a profile names a
tool server (`tool_server` in the
[configuration](configuration.md#sandboxes)), the hub serves that program's
tools to callers outside: reading, searching and editing files and running
commands in the sandbox's working directory. A harness that speaks MCP
connects to [one URL](#post-apiv1workspacesslugsandboxesidmcp) with a
Parley credential and nothing else; a script uses
[two REST routes](#get-apiv1workspacesslugsandboxesidtools). The tools run
inside the sandbox, so what they can read, write and reach is what the
sandbox allows. The sandbox object's `tools` says whether it has them; a
sandbox without them answers `404 tools_unavailable` on all three routes,
and only a `ready` sandbox is served (`409 sandbox_not_ready`).

Which tools you get is decided for each request:

| Set | Who | Tools |
|---|---|---|
| `full` | a caller who may exec in the sandbox: `sandboxes:write`, write access to the workspace, and a credential that covers the sandbox's hub credential ([no widening](#rules-for-every-sandbox-route)) | every tool the tool server offers |
| `read_only` | every other caller the routes let through | the full set without `write_file`, `edit_file`, `execute` and `run_command` |

Each call is its own command in the sandbox. It shares the limit of
`[sandbox].max_concurrent_execs` with exec, is bound by
`[sandbox].max_exec_timeout`, and counts as activity. A call whose caller
leaves is killed. Every call that ran writes one `parley.sandbox.tool_call`
event with the tool's name, `ok`, `duration_ms`, `via` (`rest` or `mcp`),
the caller's `set` and, for a failure, `error`; never the arguments or the
result. A result is not redacted: like the output of an exec, it may hold
whatever the sandbox holds.

### `POST /api/v1/workspaces/{slug}/sandboxes`

Creates a sandbox on the workspace. You need write access, a PAT needs
`sandboxes:write`, and the admin token is refused. With read access only,
fork the workspace and create the sandbox on your fork. The route takes an
`Idempotency-Key`.

Every body member is optional:

| Member | Value |
|---|---|
| `profile` | the name of a profile; absent, the one marked `default` |
| `command` | the main process, a list of 1 to 256 non-empty strings; absent, an idle sandbox driven by exec |
| `env` | at most 64 variables. A name must not start with `PARLEY_` or `OPENSHELL_` or be a secret the profile exposes; name and value together have at most 8,192 bytes |
| `checkout` | `false` for no checkout, or `{ref, branch}`: `ref` a branch, tag or commit of the workspace (absent, the fork's base branch, or the default branch of a workspace that is not a fork); `branch` the work branch, on a task fork only (absent, the fork's name) |
| `restore_from` | the id of a sandbox of the same workspace whose latest checkpoint succeeded; not with `checkout: false` or a profile without hub access |
| `ttl` | a duration from `1m` to the profile's `ttl`; absent, the profile's |
| `purpose` | at most 200 characters |
| `labels` | at most 16 labels. A key has 1 to 63 letters, digits, `.`, `_` and `-`, begins and ends with a letter or digit and does not start with `parley.` or `openshell.`; a value has at most 256 bytes |
| `session_id` | an agent session of yours on the workspace; absent, the fork's session when it has one |

```json
{"profile":"agent","command":["make","test"],"env":{"CI":"1"},"ttl":"1h","purpose":"run tests","labels":{"team":"core"}}
```

The answer is `201 Created` with the [sandbox object](#the-sandbox-object)
in state `provisioning`. The route does not wait for the runtime and never
answers `502`: an unreachable runtime shows as a sandbox that ends `failed`
with `provision_failed`. Wait for the outcome with
[`GET …/{id}?wait_for=ready`](#get-apiv1workspacesslugsandboxesid).

On a fork, the checkout is the fork's own branch `checkout.branch` when the
fork has it, else that branch created at `checkout.ref`; on a workspace that
is not a fork it is a detached checkout of `checkout.ref`. Git's user name
and email are the creating user's.

A refused request creates nothing.

| Status | When, and what to do |
|---|---|
| `400` `invalid_request` | a member breaks its rule; the message names it |
| `400` `unknown_session` | `session_id` is not a session you may link |
| `403` `forbidden` | read access only, or the admin token |
| `409` `workspace_archived`, `parent_archived` | the workspace, or a fork's parent, is archived |
| `409` `workspace_busy` | the workspace is still being cloned or under maintenance; try again |
| `409` `workspace_not_ready` | the workspace's clone failed |
| `422` `credential_missing` | a required credential or exposed secret resolves nowhere: `credentials` and `secrets` name the missing ones and `scopes_searched` where the hub looked. Set the secret, then retry |
| `429` `sandbox_capacity` | a quota is reached: `limit` names it (`max_sandboxes`, `max_per_org`, `max_per_workspace`, `max_per_user`), with `maximum` and `current`. Sandboxes in `provisioning` and `ready` count; a fork's count toward its parent's. Stop or delete one |
| `503` `secrets_unavailable` | the hub cannot read its secret store |

### `GET /api/v1/workspaces/{slug}/sandboxes/{id}`

Reads one sandbox, in every state, `deleted` included, while its record
exists. With both query parameters below the request is held until the
sandbox reaches a state, which saves you polling.

| Query parameter | Value |
|---|---|
| `wait_for` | `ready`: until the state is anything but `provisioning`; `ended`: until it is `stopped`, `completed`, `failed` or `deleted` |
| `wait` | the most seconds to hold the request, from 1 to 60 |

The answer is always `200` with the sandbox as it is when the condition
holds or the time has passed, so check `state`. One parameter without the
other, or a value outside these, is `400 invalid_request`.

### `GET /api/v1/workspaces/{slug}/sandboxes`

Lists the workspace's sandboxes, newest first, as a
[list](#using-the-api).

| Query parameter | Value |
|---|---|
| `state` | repeatable; absent, every state but `deleted` |
| `session_id` | only the sandboxes of this session |
| `label` | repeatable, `key=value`; only the sandboxes with every given label |
| `include_forks` | `true` to include, on a workspace that is not a fork, the sandboxes of the forks you can read |
| `limit`, `cursor` | as for every list |

### `PATCH /api/v1/workspaces/{slug}/sandboxes/{id}`

Extends a sandbox or changes what it says about itself. Every member is
optional, and an absent one leaves its value.

| Member | Value |
|---|---|
| `ttl` | a duration from `1m` to the profile's `ttl`, counted from now: `expires_at` becomes now plus `ttl`, cut to a task fork's `expires_at`, and the hub credential's expiry moves with it. It may make the expiry earlier |
| `purpose` | at most 200 characters |
| `labels` | the labels, replacing the old ones |

The answer is `200` with the sandbox object. A deleted sandbox is `409
sandbox_deleted`. No event is written.

### `POST /api/v1/workspaces/{slug}/sandboxes/{id}/checkpoint`

Takes a [checkpoint](#sandbox-checkpoints) of a `ready` sandbox now, whatever the
profile's `checkpoint` says. No body. The answer is `200` with the sandbox
object, whose `checkpoint` is the new one; for a sandbox without a hub
credential, `200` with `checkpoint` `null`. A sandbox in any other state is
`409 sandbox_not_ready`. A checkpoint that fails is recorded on the sandbox
and answered `502 sandbox_runtime_error`.

### `POST /api/v1/workspaces/{slug}/sandboxes/{id}/stop`

Stops a `ready` sandbox: a checkpoint when the profile asks for one, then
its compute is released and its hub credential revoked. The runtime keeps
the working directory. No body. The answer is `200` with the sandbox object
in state `stopped`; a sandbox in any other state is answered `200` as it is.
On `502 sandbox_runtime_error` the sandbox stays `ready`.

### `POST /api/v1/workspaces/{slug}/sandboxes/{id}/start`

Starts a `stopped`, `completed` or `failed` sandbox again. No body. The
answer is `200` with the sandbox in `provisioning`; it then becomes `ready`
or `failed` as a new sandbox does, with the same working directory (rebuilt
from the checkout and the latest checkpoint when the runtime no longer has
it), a new hub credential, the secrets as they resolve now and its
`command` started again. An optional credential or secret that no longer
resolves is dropped. The sandbox starts from what it recorded at creation,
also when its profile has since been removed from the configuration.

A `provisioning` or `ready` sandbox is answered `200` as it is. A start is
checked like a create and refused with the same errors: `409
workspace_archived` or `parent_archived`, `422 credential_missing`, `429
sandbox_capacity`, `503 secrets_unavailable`; and a deleted sandbox is `409
sandbox_deleted`.

### `DELETE /api/v1/workspaces/{slug}/sandboxes/{id}`

Deletes a sandbox in any state: it is removed at the runtime, and its hub
credential is revoked. A `ready` sandbox is
checkpointed first when the query parameter `checkpoint` is `true`, or when
it is absent and the profile's `checkpoint` is true; `checkpoint=false`
skips it. The answer is `204`, also for a sandbox already deleted and when
the runtime cannot be reached: the hub removes what the runtime still holds
later.

### `POST /api/v1/workspaces/{slug}/sandboxes/{id}/exec`

Runs a command in a `ready` sandbox, whether or not its main process runs,
and answers once it has ended. Your credential must
[cover the sandbox's](#rules-for-every-sandbox-route). The JSON body may
have 8 MiB:

| Member | Value |
|---|---|
| `command` | required, a list of 1 to 256 non-empty strings |
| `workdir` | the directory to run in; `/workspace` when absent |
| `env` | variables laid over the sandbox's environment, by the rules of a create's `env` |
| `stdin` | text for the command's standard input |
| `stdin_base64` | the same as base64, at most 4 MiB decoded; not together with `stdin` |
| `timeout` | a duration above `0s` and at most `[sandbox].max_exec_timeout`; `60s` when absent |

```json
{"command":["sh","-c","echo hi; exit 3"],"env":{"CI":"1"},"timeout":"90s"}
```

The answer is `200`, also for a non-zero exit code:

```json
{"exit_code":3,"stdout":"hi\n","stderr":"","truncated":false,"timed_out":false,"duration_ms":12}
```

- At most 1 MiB of each stream is returned; with more, `truncated` is
  `true`.
- A stream that is not valid UTF-8 comes in `stdout_base64` or
  `stderr_base64`, with its text member `""`.
- A command still running at its timeout is killed: `timed_out` is `true`,
  `exit_code` is `null`, and the output until then is returned.
- When you close the connection, the command is killed.

**Streaming.** With `Accept: text/event-stream` the output arrives as it is
produced, with no cap: frames `event: stdout` and `event: stderr`, each with
data `{"data":"…"}` (or `{"data_base64":"…"}` for bytes that are not valid
UTF-8), then one `event: exit` with `{"exit_code","timed_out","duration_ms"}`.
A stream that ends without an `exit` frame means the runtime failed or the
hub stopped. A refusal is the usual JSON error, since it is decided before
the command starts.

```text
event: stdout
data: {"data":"hi\n"}

event: exit
data: {"exit_code":3,"timed_out":false,"duration_ms":12}
```

| Status | When, and what to do |
|---|---|
| `400` `invalid_request` | a member breaks its rule; the message names it |
| `403` `insufficient_scope` | your credential does not cover the sandbox's hub credential |
| `409` `sandbox_not_ready` | the sandbox is not `ready` |
| `413` `payload_too_large` | the body is over 8 MiB |
| `429` `rate_limited` | `[sandbox].max_concurrent_execs` commands already run in the sandbox (the main process and attached shells do not count); there is no `Retry-After`, try again once one has ended |
| `502` `sandbox_runtime_error` | the runtime could not run the command |

### `PUT /api/v1/workspaces/{slug}/sandboxes/{id}/files`

Uploads into a `ready` sandbox's working directory. Your credential must
[cover the sandbox's](#rules-for-every-sandbox-route).

| Query parameter | Value |
|---|---|
| `path` | required: absolute, or relative to `/workspace`; it must be `/workspace` or below |
| `archive` | `tar` to unpack a tar stream under the directory `path` |
| `mode` | the file's mode, octal from `0000` to `0777`; `0644` when absent; not with `archive=tar`, whose entries carry their modes |

Without `archive` the body (`Content-Type: application/octet-stream`)
becomes the file at `path`, replacing an existing one; missing parent
directories are created. With `archive=tar` the body (`application/x-tar`
or `application/octet-stream`) may hold directories and regular files only.
The answer is `200` with the absolute path, the file bytes written and the
number of files and directories written:

```json
{"path":"/workspace/proj","bytes":21,"entries":3}
```

An upload is not atomic: when a tar entry is refused or the size limit is
passed, what was written before it stays.

| Status | When |
|---|---|
| `400` `invalid_request` | a parameter breaks its rule, the body is not a readable tar stream, or a tar entry is a link or a device |
| `400` `path_outside_workdir` | `path`, or a tar entry, leads out of where it must stay |
| `413` `payload_too_large` | the body is over `[sandbox].max_transfer_size` |
| `415` `unsupported_media_type` | another `Content-Type` |
| `502` `sandbox_runtime_error` | the runtime failed |

### `GET /api/v1/workspaces/{slug}/sandboxes/{id}/files`

Downloads from a sandbox in state `ready`, `completed` or `failed`, with the
`path` and `archive` of the upload. A file is answered with its bytes as
`application/octet-stream`. With `archive=tar` the answer is an
`application/x-tar` stream: of a directory's directories and regular files,
named relative to it, or of the one file.

| Status | When |
|---|---|
| `400` `invalid_request`, `path_outside_workdir` | as for the upload |
| `400` `path_is_directory` | a directory without `archive=tar` |
| `404` `path_not_found` | the path does not exist, or the runtime no longer has the working directory |
| `413` `payload_too_large` | a file larger than `[sandbox].max_transfer_size` |
| `502` `sandbox_runtime_error` | the runtime failed before the answer began |

A tar stream that passes the size limit, or that the runtime fails midway,
is cut off: your read ends with a connection error, not a clean end of the
body. Treat that as a failed download.

### `GET /api/v1/workspaces/{slug}/sandboxes/{id}/logs`

Reads what a sandbox printed and what its network policy decided. While a
sandbox is `ready` the hub stores the output of its main process and its
policy log, line by line, so the logs are served from the hub in every
state, `deleted` included, also while the runtime is down. Lines are kept
for `[sandbox].log_retention` and go with the sandbox's record.

```json
{"items":[{"time":"2026-01-02T03:04:05Z","source":"process","stream":"stdout","line":"token=[redacted]"},{"time":"2026-01-02T03:04:06Z","source":"policy","stream":null,"line":"deny example.com:443 /usr/bin/curl","action":"deny","program":"/usr/bin/curl","host":"example.com","port":443}],"next_cursor":null}
```

| Member | Meaning |
|---|---|
| `time` | when the line was produced |
| `source` | `process` (the main process) or `policy` |
| `stream` | `stdout` or `stderr`; `null` for a policy line |
| `line` | the text; a line over 16,384 bytes is split |
| `action`, `program`, `host`, `port` | policy lines only: `allow` or `deny`, and what was decided on |
| `credential` | policy lines of a request that used an attached credential: which one |

| Query parameter | Value |
|---|---|
| `source` | `process` or `policy`; both when absent |
| `since` | an RFC 3339 timestamp; only lines at or after it |
| `tail` | only the last that many matching lines |
| `follow` | `true` to keep the answer open |
| `limit`, `cursor` | as for every [list](#using-the-api), oldest line first; not with `follow=true` |

With `follow=true` the answer is `text/event-stream`: one frame `event: log`
per line with the line's object as data, first the stored lines the other
parameters select, then each new line as it is stored. The stream stays open
while the sandbox is stopped or ended, and ends when the sandbox is deleted.

### `GET /api/v1/workspaces/{slug}/sandboxes/{id}/attach`

Opens an interactive shell (`/bin/sh`) in a `ready` sandbox, in the working
directory with the sandbox's environment, over a WebSocket (version 13).
Authenticate with the `Authorization` header as on every route; your
credential must [cover the sandbox's](#rules-for-every-sandbox-route). The
query parameters `cols` and `rows` (1 to 1,000 each; 80 by 24 when absent)
size the terminal.

- Binary frames carry terminal bytes in both directions. A frame from you
  larger than 1 MiB ends the connection.
- A text frame `{"resize":{"cols":…,"rows":…}}` resizes the terminal.
- Closing the connection ends that shell and nothing else: the main
  process, running execs and other shells go on.
- When the shell exits, the hub sends its remaining output and closes.

Every refusal comes before the upgrade, as a normal HTTP error: `400
invalid_request` for a request that is not a WebSocket upgrade or a bad
`cols` or `rows`, `403 insufficient_scope`, `409 sandbox_not_ready`, `502
sandbox_runtime_error`.

### `GET /api/v1/workspaces/{slug}/sandboxes/{id}/tools`

Lists the [tools](#tools-for-an-agent-outside-the-sandbox) you may call, in
the tool server's order. `set` is `full` or `read_only`; the schemas are the
tool server's, and `output_schema` is `null` for a tool without one.

```json
{"set":"read_only","tools":[{"name":"read_file","description":"…","input_schema":{"type":"object","properties":{"path":{"type":"string"}}},"output_schema":null}]}
```

Errors: `404 tools_unavailable`, `409 sandbox_not_ready`, and `502
tool_server_failed` when the tool server in the sandbox cannot be run or
answers nonsense (check the profile's `tool_server` and its image).

### `POST /api/v1/workspaces/{slug}/sandboxes/{id}/tools/{name}`

Calls the tool `name`. The body is empty or a JSON object of at most 8 MiB:

| Member | Value |
|---|---|
| `input` | the tool's input, a JSON object of at most 4 MiB; `{}` when absent |
| `timeout` | a duration above `0s`; `60s` when absent; cut to `[sandbox].max_exec_timeout` |

The answer is `200` with the tool's result and how long the call took, also
when the tool failed or the call timed out, so check `ok`:

```json
{"ok":false,"data":{},"error":"not_found","detail":"no such file","duration_ms":38}
```

`data` is the tool's result object. `error` and `detail` are present for a
failure (`error` is `timeout` for a call that did not end in time), and
`text`, the text the tool means for a model, when the tool gave one.

| Status | When |
|---|---|
| `400` `invalid_request` | the body or `input` is not an object, or `timeout` is not a duration above `0s` |
| `403` `insufficient_scope` | a tool of the full set, called with the read-only set |
| `404` `tool_not_found` | no set holds the name |
| `404` `tools_unavailable` | the sandbox has no tools |
| `409` `sandbox_not_ready` | the sandbox is not `ready` |
| `413` `payload_too_large` | `input` over 4 MiB, or a body over 8 MiB |
| `429` `rate_limited` | `[sandbox].max_concurrent_execs` commands already run in the sandbox; no `Retry-After` |
| `502` `tool_server_failed`, `sandbox_runtime_error` | the tool server or the runtime failed, as opposed to the tool |

### `POST /api/v1/workspaces/{slug}/sandboxes/{id}/mcp`

An MCP server over Streamable HTTP for the one sandbox. Give an MCP client
this URL and a bearer credential the two routes above accept, and it lists
and calls the sandbox's tools with no other configuration. The route is
stateless: there is no `Mcp-Session-Id`, every request is answered on its
own as `application/json`, with or without an `initialize` before it, and a
hub restart between two requests is not visible. Send `Content-Type:
application/json` and an `Accept` that names both `application/json` and
`text/event-stream`, as MCP clients do. A body may have 8 MiB. `GET` and
`DELETE` are `405`.

| Method | Answer |
|---|---|
| `initialize` | the server name `parley`, the hub's version, the capability `tools` alone, and `instructions` naming the sandbox and the workspace |
| `ping` | an empty result |
| `tools/list` | your tool set on one page: `name`, `description`, `inputSchema` and, where a tool has one, `outputSchema` |
| `tools/call` | the call, with `arguments` as the input, bound to 10 minutes (and to `[sandbox].max_exec_timeout`) |
| a notification | accepted |
| any other method | JSON-RPC error `-32601` |
| a batch | JSON-RPC error `-32600` |

The result of `tools/call` has `isError` true when the tool failed or timed
out, one text content (the tool's `text`, else its data as JSON, or
`<error>: <detail>` for a failure) and, when the tool has an output schema,
`structuredContent`.

| JSON-RPC error | When |
|---|---|
| `-32602` | a tool outside your set or unknown; `arguments` that are not an object or over 4 MiB |
| `-32603` | the tool server or the runtime failed |
| `-32000` | the sandbox is busy: `[sandbox].max_concurrent_execs` commands already run in it |

What is decided before the MCP message is read is a plain HTTP error with
the hub's error object, not JSON-RPC: `401`, `403`, `404`
(`sandbox_not_found`, `tools_unavailable`, `sandboxes_disabled`), `409
sandbox_not_ready`, `413`, `429` for the hub's rate limit, and `502` while
the tool definitions cannot be read. The route offers no resources, prompts,
completions or logging and no MCP authorization flow.

### `GET /api/v1/workspaces/{slug}/sandboxes/{id}/proposals`

Lists a sandbox's [proposals](#the-network-policy-of-a-sandbox), oldest first, as a
[list](#using-the-api). A token bound to the workspace may call it, and a
deleted sandbox is served while its record exists.

| Query parameter | Value |
|---|---|
| `state` | `open` (the default), `approved`, `rejected` or `withdrawn`; given once |
| `limit`, `cursor` | as for every list |

### `POST /api/v1/workspaces/{slug}/sandboxes/{id}/proposals/{proposal_id}/approve`

Approves an open proposal: the rule joins the sandbox's policy at once when
the sandbox is `provisioning` or `ready`, and at its next start otherwise,
and stays for the sandbox's life. You need write access to the sandbox's
workspace and `sandboxes:write`; every workspace-bound token is refused
with `403 token_bound`. All body members are optional:

| Member | Value |
|---|---|
| `comment` | a string, kept with the decision |
| `acknowledge_findings` | `true` is required to approve a proposal with a finding of severity `high` |
| `scope` | `sandbox` (the default), or `workspace` to also keep the rule as a [workspace sandbox rule](#the-network-policy-of-a-sandbox) on the workspace, or on the parent of a task fork |

The answer is `200` with the proposal. The sandbox's `policy_revision` grows
by one, and `parley.sandbox.proposal_decided` is written with `proposal_id`,
`decision`, `acknowledged_findings`, `scope` and `rule_id`; a rule newly
kept for a workspace also writes `parley.sandbox.rule_add` on that
workspace. A rule identical to one the workspace already holds is not saved
twice: the approval names the existing rule.

| Status | When, and what to do |
|---|---|
| `400` `invalid_request` | a member has the wrong type, or `scope` is neither value |
| `403` `token_bound` | a workspace-bound token |
| `403` `forbidden` | read-only access to the sandbox's workspace, or, with scope `workspace`, no write access to the workspace that would hold the rule (a fork owner who can only read the parent). Approve with scope `sandbox`, or ask someone who can write to the parent |
| `403` `workspace_rules_disabled` | scope `workspace` while `[sandbox].allow_workspace_rules` is false |
| `404` `not_found` | no such proposal on this sandbox |
| `409` `parent_archived` | scope `workspace` on a fork whose parent is archived |
| `409` `proposal_decided` | the proposal is not `open`; of two decisions sent together, one wins |
| `409` `findings_not_acknowledged` | a `high` finding without `acknowledge_findings: true`. Read the findings, then repeat with it |
| `409` `conflict` | the runtime rejects the rule; the message holds its reason |
| `429` `sandbox_rule_limit_reached` | scope `workspace` and the workspace already holds 50 rules, none identical. Delete one, or approve with scope `sandbox` |
| `502` `sandbox_runtime_error` | the runtime cannot be reached; nothing changed |

### `POST /api/v1/workspaces/{slug}/sandboxes/{id}/proposals/{proposal_id}/reject`

Rejects an open proposal, in any state of the sandbox. The same callers as
for an approval; the body may hold a `comment`. The answer is `200` with the
proposal, also when the runtime could not be told. A `high` finding needs no
acknowledgement, the policy does not change, and
`parley.sandbox.proposal_decided` is written with `decision` `rejected`. The
refusals are the approval's for a bound token, the scope, read-only access,
the body, an unknown sandbox or proposal and a proposal that is not `open`.

### `GET /api/v1/workspaces/{slug}/sandbox-rules`

Lists the [workspace sandbox rules](#the-network-policy-of-a-sandbox) of a workspace,
oldest first, as a [list](#using-the-api). With a task fork's slug it lists
the parent's rules, and a token bound to the fork may call it.

### `DELETE /api/v1/workspaces/{slug}/sandbox-rules/{rule_id}`

Deletes a workspace sandbox rule, so that sandboxes created from now on no
longer get it. The sandboxes that have the rule keep it. It works in any
state of the workspace, archived included, refuses every workspace-bound
token (`403 token_bound`), answers `204` and writes
`parley.sandbox.rule_remove`. A task fork's slug is `400
not_supported_on_fork` (delete the rule on the parent), and an unknown rule
is `404 sandbox_rule_not_found`.

### `GET /api/v1/sandbox-profiles`

Lists the hub's profiles, so that you know which one to ask for. Any
authenticated caller may call it (a PAT needs `sandboxes:read`); it needs
access to no workspace. The answer holds every profile in name order on one
page, and it answers the same while the runtime is unreachable.

```json
{"items":[{"name":"agent","default":true,"description":"an agent","image":"agent:1","cpu":"2","memory":"4Gi","gpu":1,"ttl":"4h","ttl_seconds":14400,"idle_timeout":"30m","policy_summary":["a.test","b.test"],"credentials":["model"],"optional_credentials":["search"],"exposed_secrets":[{"name":"A_TOKEN","optional":true},{"name":"Z_TOKEN","optional":false}],"hub_access":"none","env_keys":["A","B"],"checkpoint":false,"allow_proposals":false,"tools":true}],"next_cursor":null}
```

| Member | Meaning |
|---|---|
| `name`, `description` | the profile's name and what the operator says about it |
| `default` | `true` for the profile a sandbox gets when its creator names none |
| `image`, `cpu`, `memory`, `gpu` | as configured; `""` (or `0`) where the profile leaves it to the runtime |
| `ttl`, `ttl_seconds` | the default and longest life of its sandboxes, as a duration and in seconds |
| `idle_timeout` | its idle timeout; `0s` when the idle stop is off |
| `policy_summary` | the hosts its base policy allows; `[]` for the built-in policy |
| `credentials`, `optional_credentials` | the credentials it attaches as placeholders |
| `exposed_secrets` | the secrets it exposes with their real values, `{name, optional}` each |
| `hub_access` | `fork` when its sandboxes get a hub credential, else `none` |
| `env_keys` | the names of the variables it sets |
| `checkpoint` | whether the hub checkpoints its sandboxes on its own |
| `allow_proposals` | whether its sandboxes may ask for more network access |
| `tools` | whether its sandboxes serve tools |

File paths, the values of `env` and secret values are never shown.

### `GET /api/v1/sandboxes`

Lists the sandboxes of every workspace on the hub, newest first, as a
[list](#using-the-api), for the operator. Only the admin token may call it;
every other credential, the API key of a user with role `admin` included,
gets `403 forbidden`. It reads the hub's own records, so it answers while
the runtime is unreachable.

| Query parameter | Value |
|---|---|
| `state` | only the sandboxes in this one state |
| `user` | only the sandboxes created by the user with this id |
| `limit`, `cursor` | as for every list |

## Sessions and telemetry

An agent session answers two questions about agent work: what did it cost,
and which run produced which change. An agent opens a session for a
workspace, reports the tokens it spends, names the session on the work it
does (forks, pushes, commits, changes, claims, lands, sandboxes) and closes
it with an outcome. The session record is then the thread that ties one
piece of agent work together. Telemetry goes further: an agent may also
send what happened inside a run (events, tool calls, traces, a postmortem),
so that a person can debug or audit the run later.

"Cost" always means token counts. The hub applies no price.

### How a session works

A session belongs to one workspace, a repository or a task fork, and to the
user who created it. It is `active` from its creation and ends once:

| Status | Set by |
|---|---|
| `active` | the create |
| `completed`, `failed`, `timeout` | the agent's complete; `timeout` also by the hub for a session `active` longer than `[sessions].max_active_age` (default `7d`) |
| `terminated` | the hub, when the workspace is archived or deleted or the task fork is removed or expires (a forced close) |

The four end states are final. Usage is accepted only while the session is
`active`. A forced close writes one `parley.session.force_closed` event per
session; after it a usage report is `409` `session_not_active` and a
complete answers `200` with the `terminated` session.

**Linking work to a session.** Other routes take a `session_id`: a fork
create, an API commit or rebase, a change, a land, a claim, a sandbox. A git
push names it with `git push -o parley.session=<id>`. The hub accepts the
reference when the session exists, you created it (or you are the admin
token), and the session's workspace is the operation's workspace or one is
a task fork of the other. Two forks of one parent do not link to each
other. The session's status is not checked, so a land that happens after
the session ended can still name it. Anything else is `400`
`unknown_session` on the route that asked, and nothing changes. Every hub
event caused by linked work carries the `session_id`, so you can filter the
[event log](#audit-and-events) by session.

**Who may read and change a session.** Its creator, anyone who may read its
workspace (for a session on a task fork: the fork's parent, also after the
fork is gone) and the admin token may read it. A workspace-bound PAT reads
only sessions of the workspaces its binding covers. Anyone else gets `404`
`not_found`, the same answer as for an unknown id. Only the creator and the
admin token may complete it or report usage; another reader gets `403`
`forbidden`. A PAT needs `sessions:read` or `sessions:write` to read and
`sessions:write` to write.

**Ids are yours to choose.** Session ids, usage ids and telemetry record
ids are UUIDs. Send your own and every create is safe to retry: the same
`id` again answers `200` with the stored record and changes nothing. Leave
it out and the hub generates one. A `run_id` has the form
`YYYYMMDD_HHMMSS_` followed by six lower-case hexadecimal digits;
`node_id`, `archetype`, `model` and `tool_name` are labels of at most 255
characters.

Ended sessions are deleted after `[sessions].max_age`, telemetry after the
`[audit]` ages (see [configuration.md](configuration.md#retention)).

### Example: one agent run

```sh
# 1. Open a session on the workspace.
curl -s -X POST "$HUB/api/v1/sessions" -H "Authorization: Bearer $TOKEN" \
  -d '{"workspace_slug":"demo","run_id":"20260601_120000_c0ffee","model":"m1"}'
# → 201 {"id":"0b7e1c52-3f4a-4d8e-9b21-5c6d7e8f9a01","status":"active",…}
SID=0b7e1c52-3f4a-4d8e-9b21-5c6d7e8f9a01

# 2. Report usage as you go.
curl -s -X POST "$HUB/api/v1/sessions/$SID/usage" -H "Authorization: Bearer $TOKEN" \
  -d '{"model":"m1","input_tokens":10,"output_tokens":5}'

# 3. Link the work, here a push; a task fork of the workspace links too.
git push -o parley.session=$SID origin feature

# 4. Close the session.
curl -s -X POST "$HUB/api/v1/sessions/$SID/complete" -H "Authorization: Bearer $TOKEN" \
  -d '{"status":"completed"}'

# 5. Later: what did the workspace cost since June 1, per model?
curl -s "$HUB/api/v1/workspaces/demo/cost?group_by=model&since=2026-06-01T00:00:00Z" \
  -H "Authorization: Bearer $TOKEN"
```

Reading the session back shows the thread:

```json
{"id":"0b7e1c52-3f4a-4d8e-9b21-5c6d7e8f9a01","workspace_slug":"demo","run_id":null,"node_id":null,"archetype":null,"model":"m1","status":"completed","user_id":"…","credential_type":"api_key","started_at":"2026-11-19T10:00:00Z","ended_at":"2026-11-19T10:01:30Z","duration_ms":90000,"error_message":null,"cache_creation_input_tokens":0,"metadata":null,"token_summary":{"total_input_tokens":10,"total_output_tokens":5,"total_cache_read_tokens":0,"models_used":["m1"]},"forks":["demo--t1"],"changes":[{"workspace":"demo","number":1}],"sandboxes":[],"counts":{"forks":1,"changes":1,"sandboxes":0,"versions":1,"pushes":0,"commits":0,"claims":0,"lands":0}}
```

### Session and telemetry routes

The shared rules (credentials, scopes, errors, lists, idempotency) are in
[Using the API](#using-the-api). `…/runs/{run_id}` stands for
`/api/v1/workspaces/{slug}/runs/{run_id}`.

| Route | Scope | What it does |
|---|---|---|
| [`POST /api/v1/sessions`](#post-apiv1sessions) | `sessions:write` | opens a session |
| [`POST /api/v1/sessions/{id}/complete`](#post-apiv1sessionsidcomplete) | `sessions:write` | ends a session with an outcome |
| [`POST /api/v1/sessions/{id}/usage`](#post-apiv1sessionsidusage) | `sessions:write` | reports one token usage increment |
| [`GET /api/v1/sessions/{id}`](#get-apiv1sessionsid) | `sessions:read` | reads a session |
| [`GET /api/v1/sessions/{id}/usage`](#get-apiv1sessionsidusage) | `sessions:read` | lists a session's usage records |
| [`GET /api/v1/sessions`](#get-apiv1sessions) | `sessions:read` | lists the sessions you may read |
| [`GET /api/v1/workspaces/{slug}/cost`](#get-apiv1workspacesslugcost) | `sessions:read` | sums token usage per day, session or model |
| [`POST …/runs/{run_id}/…`](#post-apiv1workspacesslugrunsrun_id) | `audit:write` | stores telemetry records of a run |
| [`POST …/runs/{run_id}/postmortem`](#post-apiv1workspacesslugrunsrun_idpostmortem) | `audit:write` | stores the run's postmortem |
| [`GET …/runs/{run_id}/…`](#get-apiv1workspacesslugrunsrun_id) | `audit:read` | reads a run's records, postmortem and transcript |

### `POST /api/v1/sessions`

Opens a session on a workspace you may write to. Answers `201` with the
[session object](#get-apiv1sessionsid), `status` `active`.

| Member | Value |
|---|---|
| `workspace_slug` | required: a repository or a task fork |
| `id` | a UUID; generated when absent |
| `run_id`, `node_id`, `archetype`, `model` | optional labels |
| `metadata` | any JSON value, kept as sent |

Sending an `id` you already created for the same workspace answers `200`
with the stored session, also when the other members differ.

| Error | When |
|---|---|
| `400` `invalid_request` | `workspace_slug` missing, or a member breaks its form |
| `404` `not_found` | no such workspace, no access to it, or outside a bound PAT's binding |
| `403` `forbidden` | you may only read the workspace |
| `409` `conflict` | the `id` exists for another creator or another workspace |
| `409` `workspace_archived` | the workspace is archived (a replay of your own `id` still answers `200`) |

### `POST /api/v1/sessions/{id}/complete`

Ends an `active` session and answers `200` with it. The body: `status`
(required: `completed`, `failed` or `timeout`), `error_message`,
`duration_ms` (computed from start to end when absent) and
`cache_creation_input_tokens` (default `0`). Completing a session that has
ended already, `terminated` included, answers `200` with the session as it
is; of two completes at once exactly one takes effect. `400`
`invalid_request` answers a `status` outside the three or a negative
number.

### `POST /api/v1/sessions/{id}/usage`

Adds one usage record to an `active` session and answers `201` with it. The
body: `model` (required), `id` (a UUID, generated when absent) and
`input_tokens`, `output_tokens`, `cache_read_tokens` (integers from 0 to
2^40, default `0`). Report increments, not running totals: the hub sums the
records.

```json
{"id":"…","session_id":"…","workspace_slug":"demo","model":"m1","input_tokens":10,"output_tokens":5,"cache_read_tokens":0,"reported_at":"2026-11-19T10:00:30Z"}
```

A replay of the same `id` answers `200` with the stored record, also after
the session ended. `409` `session_not_active` means the session has ended:
stop reporting. `409` `conflict` means the `id` belongs to another session.

### `GET /api/v1/sessions/{id}`

Answers the session object. It is built when you ask, so a usage report or
a new link shows in the next read.

| Member | Value |
|---|---|
| `id`, `workspace_slug`, `run_id`, `node_id`, `archetype`, `model`, `metadata` | as sent at creation, `null` when not sent |
| `status` | `active`, `completed`, `failed`, `timeout` or `terminated` |
| `user_id`, `credential_type` | who created it: the user (`null` for the admin token) and `admin_token`, `api_key` or `pat` |
| `started_at`, `ended_at`, `duration_ms` | `ended_at` is `null` while `active`; a forced close leaves `duration_ms` `null` |
| `error_message` | from the complete, or the reason of a forced close or timeout |
| `cache_creation_input_tokens` | from the complete, else `0` |
| `token_summary` | the sums of the usage records: `total_input_tokens`, `total_output_tokens`, `total_cache_read_tokens`, `models_used` |
| `forks` | slugs of the task forks created in the session, oldest first |
| `changes` | `{workspace, number}` of each change with a version recorded in the session |
| `sandboxes` | `{workspace, id}` of each sandbox created in the session |
| `counts` | links per kind: `forks`, `changes`, `sandboxes`, `versions`, `pushes`, `commits`, `claims`, `lands` |

`forks`, `changes` and `sandboxes` hold at most their first 500 entries;
`counts` holds the true totals. A link stays when the fork, sandbox, change
or workspace it names is removed.

### `GET /api/v1/sessions/{id}/usage`

Lists the session's usage records, newest first (`order=asc` for oldest
first), with `limit` and `cursor`. The answer adds `session_id` and
`totals`, which has the shape of `token_summary` and covers every record,
whatever the page.

### `GET /api/v1/sessions`

Lists the sessions you may read, newest first by `started_at`.

| Parameter | Effect |
|---|---|
| `workspace` | only the sessions of this workspace |
| `include_forks` | `true` adds the sessions on the task forks of `workspace`, removed forks included; needs `workspace` |
| `run_id`, `status`, `credential_type` | only the sessions with this value |
| `since`, `until` | `since` ≤ `started_at` < `until`, RFC 3339 |
| `order` | `desc` (the default) or `asc` |
| `limit`, `cursor` | paging |

A bad value, or a filter given twice, is `400` `invalid_request`; a
`workspace` you may not read is `404` `not_found`.

### `GET /api/v1/workspaces/{slug}/cost`

Sums the token usage reported for the workspace over a window. Any reader
of the workspace may call it, also when it is archived. Only the
workspace's own sessions count: usage of sessions on its task forks belongs
to the forks, so ask each fork for its own cost.

| Parameter | Effect |
|---|---|
| `group_by` | `day` (the default; the key is the UTC date), `session` (the session id) or `model` |
| `since` | start of the window, included; 24 hours ago when absent |
| `until` | end of the window, excluded; now when absent |
| `limit`, `cursor` | paging of the groups |

```json
{"workspace":"demo","period":{"since":"2026-09-01T00:00:00Z","until":"2026-09-02T00:00:00Z"},"group_by":"day","totals":{"input_tokens":10,"output_tokens":0,"cache_read_tokens":0,"sessions":1},"items":[{"key":"2026-09-01","input_tokens":10,"output_tokens":0,"cache_read_tokens":0,"sessions":1}],"next_cursor":null}
```

`period` is the window used, `totals` covers the whole window on every
page, and `sessions` counts distinct sessions. A `since` that is not before
`until` is `400` `invalid_request`.

### `POST /api/v1/workspaces/{slug}/runs/{run_id}/…`

Telemetry is what an agent chooses to tell the hub about a run. It is
stored apart from hub events: sending it writes no hub event and it never
appears on the event stream. Run events do appear in the
[audit query](#get-apiv1audit), with `source` `agent`.

| Route | Stores | Members beyond the common ones |
|---|---|---|
| `POST …/events` | one run event | `event_type` (required, free-form), `severity` (`info`, `warning`, `error`, `critical`), `node_id`, `session_id`, `payload` (an object) |
| `POST …/events/batch` | 1 to 1000 run events | as above, per item |
| `POST …/sessions/outcomes` | one session outcome | `session_id` (required), `status` (required, free-form), `node_id`, `duration_ms`, `token_usage` |
| `POST …/tools/calls` | one tool call | `tool_name` (required), `node_id`, `session_id`, `duration_ms`, `input`, `output` |
| `POST …/tools/errors` | one tool error | `tool_name` (required), `error_msg` (required), `error_code`, `node_id`, `session_id` |
| `POST …/traces` | one trace record | `event_type` (required: `session.init`, `assistant.message`, `tool.use`, `tool.error` or `session.result`), `node_id`, `session_id`, `sequence`, `data` |
| `POST …/traces/batch` | 1 to 1000 trace records | as above, per item |

Every record also has `id` (a UUID, generated when absent), `workspace`,
`run_id`, `timestamp` (yours, RFC 3339 with up to microseconds; the hub's
time when absent) and `ingested_at` (the hub's time). `session_id` must be
a UUID but need not name a stored session. JSON members are stored and
returned as JSON. A run event without `severity` gets `error` for the type
`session.fail`, `warning` for `run.limit_reached`, `git.conflict`,
`harvest.empty` and `review.parse_failure`, and `info` otherwise.

```sh
curl -s -X POST "$HUB/api/v1/workspaces/demo/runs/20260102_030405_a1b2c3/events" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"event_type":"run.start","timestamp":"2026-01-02T03:00:00Z","payload":{"a":{"b":1}}}'
```

```json
{"id":"0a1b2c3d-0000-4000-8000-000000000001","workspace":"demo","run_id":"20260102_030405_a1b2c3","timestamp":"2026-01-02T03:00:00Z","ingested_at":"2026-01-02T03:04:05Z","event_type":"run.start","severity":"info","node_id":null,"session_id":null,"payload":{"a":{"b":1}}}
```

You need write access to the workspace; a task fork accepts telemetry like
a repository.

**Once per id.** A new record answers `201`. The same `id` again, for the
same kind, workspace and run, answers `200` with the stored record and
changes nothing. Each kind has its own ids. The single-record routes also
take an `Idempotency-Key`; the batch routes do not.

**Batches** always answer `200` once the body is a JSON array of 1 to 1000
items. Each item is judged on its own:

```json
{"accepted":2,"duplicates":1,"errors":[{"index":3,"id":null,"message":"event_type is required and must be a non-empty string"}]}
```

`accepted` items are stored together, all or none. `duplicates` counts
items whose `id` is stored already or repeats an earlier item. `errors`
lists the invalid items by zero-based `index`; fix and resend only those.

| Error | When |
|---|---|
| `400` `invalid_request` | a malformed `run_id`, a member that breaks its rule, a body `run_id` that differs from the path's, an empty batch |
| `409` `conflict` | the `id` names a record of another workspace or run |
| `409` `workspace_archived` | an archived workspace accepts no telemetry |
| `413` `payload_too_large` | a batch of more than 1000 items |
| `503` `unavailable` | the write did not finish within `[audit].write_timeout` (default `2s`); nothing was stored, retry after `Retry-After` |

**What the hub adds itself.** When an exec, an attach, an upload or a
download ends in a [sandbox](#sandboxes) whose session has a `run_id`, the
hub stores a run event under that run and session: `sandbox.exec`,
`sandbox.attach`, `sandbox.upload` or `sandbox.download`. Its payload names
the sandbox, the actor and, per kind, the command with its exit code and
duration, or the path and byte count. It never holds input, output, file
content or environment values. A sandbox without such a session leaves no
record.

### `POST /api/v1/workspaces/{slug}/runs/{run_id}/postmortem`

Stores the one postmortem of a run: the summary an agent writes when a run
stops short. The first answers `201`; a later one for the same run answers
`200` with the stored postmortem and changes nothing.

| Member | Value |
|---|---|
| `run_status` | required: `stalled`, `block_limit`, `cost_limit` or `session_limit` |
| `started_at`, `completed_at` | required, RFC 3339 |
| `task_summary` | required, an object with a key `total` |
| `cost_summary` | required, an object |
| `blocked_tasks`, `session_history` | arrays, `[]` when absent |
| `schema_version` | `1`, also when absent; another value is `400` `unknown_schema_version` |

The hub sets `id`, `timestamp` and `ingested_at` itself.

### `GET /api/v1/workspaces/{slug}/runs/{run_id}/…`

Reads back what was stored about one run. You need read access to the
workspace; an archived workspace reads like any other.

| Route | Answers | Filters |
|---|---|---|
| `GET …/events` | run events | `event_type`, `severity`, `node_id`, `session_id` |
| `GET …/sessions/outcomes` | session outcomes | `node_id`, `session_id`, `status` |
| `GET …/tools/calls` | tool calls | `node_id`, `session_id`, `tool_name` |
| `GET …/tools/errors` | tool errors | `node_id`, `session_id`, `tool_name` |
| `GET …/traces` | trace records | `event_type`, `node_id`, `session_id` |
| `GET …/postmortem` | the postmortem | none |
| `GET …/transcript?node_id=…` | the conversation of one node | none |

The five lists are ordered by `timestamp`, oldest first (`order=desc`
reverses), and take `since`, `until`, `limit` and `cursor`. Filters match
exactly and combine. A run without records answers an empty list.

`GET …/postmortem` answers `404` `postmortem_not_found` for a run without
one in this workspace.

`GET …/transcript` rebuilds the conversation of one node from its trace
records, in order. `node_id` is required. Each trace but `session.result`
becomes one item: `role` (`system`, `assistant`, `tool_use` or
`tool_error`), `content` and `tool_name` (from the trace's `data`) and
`timestamp`.

```json
{"items":[{"role":"system","content":"","tool_name":null,"timestamp":"2026-06-01T12:00:00Z"},{"role":"assistant","content":"hello","tool_name":null,"timestamp":"2026-06-01T12:00:01Z"},{"role":"tool_use","content":"","tool_name":"bash","timestamp":"2026-06-01T12:00:02Z"}],"next_cursor":null,"run_id":"20260601_120000_c0ffee","node_id":"coder"}
```

## Audit and events

Every significant state change in the hub writes one hub event: a push, a
change opened, a land, a sandbox that failed. Events let automation react
without rescanning state (a review agent waits for `parley.change.open`, a
CI system for `parley.change.version`) and let an operator reconstruct who
did what. The design is in
[ADR 05](adr/05-deliver-events-by-reading-the-log-by-position.md).

### What an event is

Every event has the same twelve members:

```json
{"seq":11,"id":"…","type":"parley.change.open","time":"2026-11-14T10:30:15Z","workspace":"api","actor_type":"pat","actor_id":"u_1","resource_type":"change","resource_id":"42","action":"open","session_id":null,"metadata":{}}
```

| Member | Meaning |
|---|---|
| `seq` | the hub-wide sequence number: strictly increasing, never reused |
| `id` | a UUID, the event's identity; use it to drop duplicates |
| `type` | see [Event types](#event-types); always starts with `parley.` |
| `time` | when the event was written |
| `workspace` | the slug the event belongs to (a task fork's own slug for a fork), `null` for an event of no workspace |
| `actor_type`, `actor_id` | who caused it: `admin_token`, `api_key`, `pat`, `forge` or `system`, and the user id (`null` for `admin_token`, `forge` and `system`) |
| `resource_type`, `resource_id` | what it is about, for example `change` and `42` |
| `action` | the last segment of `type` |
| `session_id` | the [agent session](#sessions-and-telemetry) the work belongs to, or `null` |
| `metadata` | type-specific details, `{}` when none; never a credential or a secret value |

What the hub guarantees:

- **Durable.** An event commits together with the state change it reports:
  both happen or neither does.
- **Ordered.** Events are delivered in `seq` order, and an event never
  appears after one with a higher `seq` was delivered.
- **At least once.** A consumer that resumes from the last `seq` it
  processed gets every later event it may see. Across a reconnect it can
  get one twice; within one connection or page none repeats.
- **Gaps are normal.** You receive only the events you may see and asked
  for, so the `seq` values you get are not contiguous. A gap is not a loss.

### Who sees which events

One rule holds for the stream, the log and the audit query, and it is
applied when an event is delivered, not when you connected:

| Event | Visible to |
|---|---|
| any | the admin token |
| of a workspace | callers who may read the workspace; for a task fork also the readers of its parent, also after the fork is gone |
| `parley.secret.*`, `parley.token.*`, `parley.webhook.*` of a workspace | only its `owner` or `admin` |
| of no workspace | the user, or the members of the org, the event's `metadata.scope` names |

A workspace-bound PAT sees only the events of the workspaces its binding
covers and no event of no workspace. A token bound to a task fork also sees
the events of the parent's changes that have the fork as their source, so
an agent can follow its own change through review and landing.

A PAT needs `audit:read` (or `audit:write`) for all three routes.

### Three ways to consume, and which to choose

| Way | Choose it when |
|---|---|
| [Stream](#get-apiv1events) (Server-Sent Events) | you can hold a connection open and want events within a second |
| [Log](#get-apiv1eventslog) (JSON pages, optional long-poll) | you run a plain loop, a cron job or a script, or want to replay history |
| [Event webhooks](#event-webhooks) | the consumer cannot connect to the hub or should not poll: CI, a chat bot, a review agent behind a URL |

All three read the same log by position, so they give the same events in
the same order. The [audit query](#get-apiv1audit) is different: it is for
looking back ("who did what"), newest first, across hub events and agent
run events, and reaches further into the past.

**Resuming.** Remember the last position you processed and send it back:
the `id` of the last frame as `Last-Event-ID` on the stream, `next_after`
as `after` on the log. Process an event before you store its position, and
drop duplicates by `id`.

**Filtering.** The stream and the log take the same filters, which combine:
`workspace` (a slug you may read), `types` (up to 20 comma-separated
entries, each an exact type or a prefix ending in `*`, such as
`parley.change.*`) and `session_id`.

**Retention.** The log holds events for `[events].retention` (default
`30d`). A resume position older than that is answered `410`
`cursor_expired` with `oldest_seq`: you have missed events, so rescan the
state you care about and start again from `oldest_seq` minus 1. Events stay
in the audit query until `[audit].max_age` (default `90d`). See
[configuration.md](configuration.md#retention).

### Example: follow the changes of a workspace

With the stream:

```sh
curl -N "$HUB/api/v1/events?workspace=api&types=parley.change.*" \
  -H "Authorization: Bearer $TOKEN" -H "Last-Event-ID: 3"
```

```text
id: 3
event: heartbeat
data: {"time":"2026-11-14T09:30:15Z","seq":3}

id: 4
event: hub_event
data: {"seq":4,"id":"…","type":"parley.change.open","time":"2026-11-14T09:30:20Z","workspace":"api","actor_type":"pat","actor_id":"u_1","resource_type":"change","resource_id":"42","action":"open","session_id":null,"metadata":{}}

```

With a long-poll loop, which needs nothing but `curl` and `jq`:

```sh
after=3
while :; do
  page=$(curl -s "$HUB/api/v1/events/log?after=$after&wait=60&types=parley.change.*" \
    -H "Authorization: Bearer $TOKEN")
  echo "$page" | jq -c '.items[]'          # handle the events
  after=$(echo "$page" | jq '.next_after') # then move on
done
```

And to ask afterwards who landed what in the workspace:

```sh
curl -s "$HUB/api/v1/audit?workspace=api&types=parley.change.land&since=2026-11-01T00:00:00Z" \
  -H "Authorization: Bearer $TOKEN"
```

### Event and audit routes

| Route | Scope | What it does |
|---|---|---|
| [`GET /api/v1/events`](#get-apiv1events) | `audit:read` | streams events live, from a resume position |
| [`GET /api/v1/events/log`](#get-apiv1eventslog) | `audit:read` | answers one page of the event log, optionally waiting for the next event |
| [`GET /api/v1/audit`](#get-apiv1audit) | `audit:read` | lists hub events and agent run events, newest first |

### `GET /api/v1/events`

Streams the events you may see as Server-Sent Events: first those after the
resume position, then every event committed afterwards, on the same
connection, with no gap and no repeat between the two.

| Parameter | Meaning |
|---|---|
| `Last-Event-ID` (header) | the resume position: you get the events with a higher `seq`. It wins over `after` |
| `after` | the same, as a query parameter. Without either, the stream starts now and sends only new events |
| `workspace`, `types`, `session_id` | the filters |

Frames are `hub_event` (its `id` is the event's `seq`, its `data` the
event) and `heartbeat` (its `id` and `data.seq` are the position up to
which the log has been examined for you). A heartbeat comes first and then
every 30 seconds. Its position rises also when no event matched, so
reconnecting with the `id` of the last frame you received, of either kind,
resumes exactly where you stopped. An event reaches an open stream within a
second of its commit.

The hub never drops an event from an open stream; it ends the stream
instead, and you reconnect. A stream ends when the hub stops, when your
credential stops being valid (checked at each heartbeat), when you read too
slowly (256 events waiting, or a write blocked for 60 seconds), or when
your position has fallen out of the log. Reconnect with `Last-Event-ID`.

| Error | When |
|---|---|
| `400` `invalid_request` | a malformed filter, or a position that is not a non-negative integer or is above the last `seq` written |
| `404` `not_found` | a `workspace` you may not read |
| `410` `cursor_expired` | the position has left the log; the body carries `oldest_seq` |
| `503` `unavailable` | `[events].max_sse_connections` streams (default `500`) are open; retry after `Retry-After`, or use the log |

Errors are answered as JSON before the stream begins.

### `GET /api/v1/events/log`

Answers one page of the event log in ascending `seq`.

| Parameter | Meaning |
|---|---|
| `after` | the resume position; without it the page starts at the oldest event in the log. `cursor` is another name for it |
| `limit` | 1 to 200, default 50 |
| `wait` | seconds to hold the request when there is nothing to return: 0 to 60, default 0 |
| `workspace`, `types`, `session_id` | the filters |

```json
{"items":[{"seq":11,"id":"…","type":"parley.change.open","time":"2026-11-14T10:30:15Z","workspace":"api","actor_type":"pat","actor_id":"u_1","resource_type":"change","resource_id":"42","action":"open","session_id":null,"metadata":{}}],"next_cursor":null,"next_after":120,"oldest_seq":11,"last_seq":120}
```

| Member | Value |
|---|---|
| `items` | the visible, matching events after the position, at most `limit` |
| `next_after` | the position to send next. It moves past events you may not see or filtered out, so always use it rather than the last item's `seq` |
| `next_cursor` | `next_after` as a string when the page is full, else `null`: more is waiting |
| `oldest_seq`, `last_seq` | the bounds of the log when the page was read |

Following `next_after` from page to page returns every visible, matching
event exactly once.

**Long-poll.** With `wait` above 0 and nothing to return, the hub holds the
request until a visible, matching event is committed and answers with it
within a second. When the time is up, or the hub stops, it answers `200`
with `items` `[]` and `next_after` moved forward. Held requests do not
count toward the stream limit.

The errors are those of the stream, without the `503`; `400` also answers a
bad `limit` or `wait`, and `after` and `cursor` with different values. A
`410` can also arrive while a request is held, when the position leaves the
log meanwhile.

### `GET /api/v1/audit`

Lists hub events and agents' [run events](#post-apiv1workspacesslugrunsrun_id)
in one list, newest first, for questions such as "who archived this
workspace" or "what did run X do before the land failed". You see the hub
events the [visibility rule](#who-sees-which-events) gives you, and the run
events of the workspaces you can read, including those of a deleted task
fork whose parent you can read. The admin token sees everything stored,
also for workspaces that no longer exist.

Every item has the same members; those of the other source are `null`:

| Member | Hub event | Run event |
|---|---|---|
| `source` | `hub` | `agent` |
| `id`, `workspace`, `session_id`, `time` | the event's | the run event's (`time` is its `timestamp`) |
| `type` | the event's `type` | the run event's `event_type` |
| `seq`, `actor_type`, `actor_id`, `resource_type`, `resource_id`, `action`, `metadata` | the event's | `null` |
| `run_id`, `node_id`, `severity`, `payload` | `null` | the run event's |

| Parameter | Applies to | Meaning |
|---|---|---|
| `source` | both | `hub` or `agent`; both when absent |
| `workspace`, `types`, `session_id` | both | as on the event log |
| `since`, `until` | both | RFC 3339: at or after `since`, before `until` |
| `actor_id`, `actor_type`, `resource_type`, `resource_id`, `action` | hub events | exact match; giving one leaves the run events out |
| `run_id`, `node_id`, `severity` | run events | exact match; giving one leaves the hub events out |
| `limit`, `cursor` | both | paging |

A hub event filter together with `source=agent`, or a run event filter with
`source=hub`, is `400` `invalid_request`, as is any malformed value. A
`workspace` you may not read is `404` `not_found`; a task fork's slug whose
parent you can read is accepted also after the fork is gone.

### Event types

The metadata of each type is described with the route that writes it. The
actor is the caller unless the meaning says the hub acts on its own
(`system`) or relays a forge (`forge`).

| Type | Written when |
|---|---|
| `parley.workspace.create` | a workspace is created |
| `parley.workspace.update` | a workspace's settings are changed |
| `parley.workspace.sync` | a sync from origin completes |
| `parley.workspace.push` | a branch is pushed to origin, whether it succeeds or fails |
| `parley.workspace.archive`, `.reactivate` | a workspace is archived or reactivated |
| `parley.workspace.reclone` | a workspace's copy is discarded to be cloned again |
| `parley.workspace.delete` | a workspace or a task fork is deleted |
| `parley.workspace.fork` | a task fork is created |
| `parley.fork.expire` | the hub removes a task fork past its lifetime |
| `parley.token.bind` | a workspace-bound token is minted |
| `parley.token.bound_refused` | a bound token was refused outside its binding (at most one per token per minute) |
| `parley.git.push` | a git push updates a ref (one event per ref) |
| `parley.git.fetch` | a git fetch that sent a pack is served, when the workspace's setting `audit_fetches` is `true` |
| `parley.scm.commit` | a commit is created through the source control API |
| `parley.change.open` | a change is opened, or leaves draft |
| `parley.change.version` | a change gets a new version |
| `parley.change.ready` | a change becomes landable |
| `parley.change.conflict` | a change no longer merges cleanly into its target |
| `parley.change.restack` | a change is rebased onto its moved base |
| `parley.change.overlap` | two open changes start to touch the same paths |
| `parley.change.approve`, `.reject` | a review approves or rejects a change |
| `parley.change.check` | a check result is reported on a change |
| `parley.change.land` | a change lands |
| `parley.change.land_fail` | a land attempt fails or is blocked |
| `parley.change.abandon`, `.restore` | a change is abandoned or restored |
| `parley.change.revert` | a reverting change is opened for a landed change (written on the original) |
| `parley.policy.update` | a landing policy is changed |
| `parley.claim.create`, `.release`, `.expire` | a claim on paths is made, released, or expired by the hub |
| `parley.patch.create`, `.update`, `.delete`, `.restore` | a carry patch is added, changed, deleted or restored |
| `parley.patch.reorder` | the order of the carry patches is changed |
| `parley.patch.sync` | a patch branch moved, or its state against origin changed, in a sync |
| `parley.patch.replace` | a patch branch's tip was replaced by origin's |
| `parley.patch.reset` | a patch branch is reset on request |
| `parley.patch.mirror_failed` | mirroring a patch branch to origin failed |
| `parley.rebuild.enqueue`, `.cancel`, `.requeue` | a rebuild job is queued, cancelled or queued again |
| `parley.rebuild.complete`, `.fail` | a rebuild ends; `.fail` says whether it will be retried |
| `parley.rebuild.followup` | a rebuild queued a follow-up because its inputs moved meanwhile |
| `parley.rebuild.push_failed` | pushing the integration branch to origin failed |
| `parley.rebuild.rollback` | the integration branch is rolled back |
| `parley.secret.create`, `.update`, `.delete` | a secret is set, replaced or deleted (never its value) |
| `parley.variable.create`, `.update`, `.delete` | a variable is set, replaced or deleted |
| `parley.forge.webhook.set`, `.unset` | the secret for a forge's inbound webhooks is set or removed |
| `parley.forge.issue.create`, `.update`, `.close` | the hub creates, updates or closes a forge issue for a caller |
| `parley.forge.comment.create`, `parley.forge.pull_request.comment` | the hub comments on a forge issue or pull request for a caller |
| `parley.forge.label.add`, `.remove`, `.create` | the hub adds, removes or creates a forge label for a caller |
| `parley.forge.pull_request.create` | the hub opens a pull request for a caller |
| `parley.forge.issue.opened`, `.edited`, `.closed`, `.reopened`, `.labeled`, `.unlabeled` | the forge reports this on an issue (relayed from its webhook) |
| `parley.forge.comment.created` | the forge reports a comment on an issue or pull request |
| `parley.forge.pull_request.opened`, `.closed`, `.merged`, `.reopened`, `.synchronize` | the forge reports this on a pull request |
| `parley.forge.push` | the forge reports a push to the origin repository |
| `parley.sandbox.create`, `.ready`, `.start`, `.stop`, `.delete` | a sandbox moves through its life |
| `parley.sandbox.exit`, `.fail` | a sandbox's workload exits, or the sandbox fails |
| `parley.sandbox.checkpoint` | the hub tried to save a sandbox's work to its fork |
| `parley.sandbox.tool_call` | a tool call in a sandbox ends |
| `parley.sandbox.deny` | a sandbox's network policy blocked a connection (once per program, host and port) |
| `parley.sandbox.proposal`, `.proposal_decided` | a blocked workload asks for a rule; the proposal is approved or rejected |
| `parley.sandbox.rule_add`, `.rule_remove` | a workspace sandbox rule is saved or removed |
| `parley.session.force_closed` | the hub ends an active session because its workspace went away |
| `parley.webhook.create`, `.update`, `.delete` | an event webhook is created, changed (also a secret rotation) or deleted |
| `parley.webhook.failing`, `.disabled` | the hub gave up an event for a subscription, or disabled the subscription |

`parley.webhook.ping` is the type of a test delivery. It is sent only to
the pinged subscription and never appears in the log, the stream or the
audit query.

## Event webhooks

An event webhook makes the hub push the events of a workspace to a URL of
yours, as signed HTTP `POST`s. Reach for it when the consumer cannot hold a
stream open or should not poll: a CI system that builds every new change
version, a review agent that wakes on `parley.change.open`, a chat bot. The
events and their order are those of the [event log](#audit-and-events);
only the direction differs.

### How delivery works

A subscription belongs to a repository workspace and has a URL, a list of
`types` and a signing secret. It receives the events an owner of the
workspace may see that match `types`, the owner-only types included, and
with `include_forks` (the default) the events of the workspace's task
forks. A task fork has no subscriptions of its own. It never receives an
event of another workspace or of no workspace, and it starts with the first
event written after its creation.

**Order.** The hub sends one event at a time per subscription, in `seq`
order, and sends the next only when the current one succeeded or was given
up. So you never get a later event before an earlier one, you may get an
event more than once (dedupe on `Parley-Delivery` or the event's `id`), and
a slow receiver delays only its own subscription.

**Success and failure.** An attempt succeeds when your endpoint answers
`2xx` within `[webhooks].timeout` (default `10s`). Answer fast and do the
work afterwards. Everything else fails, a redirect included: the hub does
not follow redirects.

**Retries.** After a failure the hub waits 10 seconds, then twice as long
each time up to 1 hour, and keeps trying for 24 hours from the event's
first attempt. Each attempt uses the subscription's current URL and secret.

| State | Meaning | How it is left |
|---|---|---|
| `active` | deliveries succeed | an event is given up after 24 hours → `failing`, with a `parley.webhook.failing` event |
| `failing` | at least one event was given up; the hub goes on with the next events | a successful attempt → `active`; 72 hours of failures since the last success → `disabled`, with a `parley.webhook.disabled` event |
| `disabled` | nothing is sent, and the events written meanwhile are not kept for it | `PATCH` with `active: true` |

`failing_since` on the subscription is the first failed attempt since the
last success; watch it, or subscribe something to `parley.webhook.*`.

**Restarts.** The position and the pending delivery are stored. After a
restart the hub resumes each subscription where it was, and repeats an
attempt that was in flight under the same delivery id.

**Archive and delete.** Subscriptions of an archived workspace keep
delivering and can be read, pinged, redelivered and deleted, but not
created or changed (`409` `workspace_archived`). Deleting the workspace
deletes its subscriptions.

### The request you receive

The body is the event, one JSON object, exactly as the event log returns
it.

| Header | Value |
|---|---|
| `Content-Type` | `application/json` |
| `Parley-Event` | the event's `type` |
| `Parley-Delivery` | the delivery id, the same on every retry and on a redelivery |
| `Parley-Seq` | the event's `seq`; `0` for a ping |
| `Parley-Timestamp` | the Unix time in seconds at which this attempt is sent |
| `Parley-Signature` | `sha256=` and the lower-case hexadecimal HMAC-SHA256 of `<Parley-Timestamp>.<body>` |
| `User-Agent` | `parley-webhook/<hub version>` |

**Verify the signature** before you trust a delivery. The key is the secret
exactly as the hub showed it, `whsec_` prefix included. The signed text is
the timestamp header, a dot and the raw body bytes. Every attempt has a new
timestamp and so a new signature; refuse a timestamp that is too old for
you to guard against replays.

```python
import hashlib, hmac

def verify(secret: str, headers, raw_body: bytes) -> bool:
    signed = headers["Parley-Timestamp"].encode() + b"." + raw_body
    want = "sha256=" + hmac.new(secret.encode(), signed, hashlib.sha256).hexdigest()
    return hmac.compare_digest(want, headers["Parley-Signature"])
```

The secret is `whsec_` followed by 64 hexadecimal characters. The hub shows
it twice only: in the answer of the create and of a rotation. Store it
then.

**Destinations.** The URL must be `https://`, and TLS certificates are
always verified. The hub refuses a URL whose host is, or resolves to, a
loopback, link-local (the cloud metadata address included), private,
shared (`100.64.0.0/10`), unspecified or multicast address, so that a
subscription cannot reach the hub's own network. The check runs at the
create, at a change of the URL (`400` `webhook_destination_refused`) and
again before every connection (the attempt fails with
`destination_refused`). An operator lifts it with `[webhooks].allow_private`,
and allows `http://` to a loopback host with `[webhooks].allow_insecure`
(see [configuration.md](configuration.md#the-configuration-file)).

### Example: subscribe a CI system

```sh
# Subscribe to new change versions and lands, forks included.
curl -s -X POST "$HUB/api/v1/workspaces/demo/webhooks" -H "Authorization: Bearer $TOKEN" \
  -d '{"url":"https://ci.example/hooks/parley","types":["parley.change.version","parley.change.land"]}'
```

```json
{"id":"0b6a4bd1-0a3c-4d55-9b1e-2a5e1f0f6c11","workspace":"demo","url":"https://ci.example/hooks/parley","types":["parley.change.version","parley.change.land"],"include_forks":true,"description":"","state":"active","created_by":"u_1","created_at":"2026-11-19T10:00:00Z","updated_at":"2026-11-19T10:00:00Z","last_delivery_at":null,"last_status":null,"failing_since":null,"secret":"whsec_9f2c…"}
```

```sh
WH=0b6a4bd1-0a3c-4d55-9b1e-2a5e1f0f6c11

# Check that the receiver is reachable and verifies the signature.
curl -s -X POST "$HUB/api/v1/workspaces/demo/webhooks/$WH/ping" -H "Authorization: Bearer $TOKEN"

# Later: what failed, and send one again after fixing the receiver.
curl -s "$HUB/api/v1/workspaces/demo/webhooks/$WH/deliveries?status=failed" \
  -H "Authorization: Bearer $TOKEN"
curl -s -X POST "$HUB/api/v1/workspaces/demo/webhooks/$WH/deliveries/$DELIVERY/redeliver" \
  -H "Authorization: Bearer $TOKEN"
```

### Webhook routes

All routes are under `/api/v1/workspaces/{slug}/webhooks`, written `…`
below. Each needs level `owner` on the workspace (or the admin token); an
org member gets `403` `forbidden`. A workspace-bound PAT is refused on all
of them with `403` `token_bound`. Called with a task fork's slug, every
route answers `400` `not_supported_on_fork`: subscribe on the parent with
`include_forks`. An `{id}` that names no subscription of the workspace is
`404` `webhook_not_found`.

| Route | Scope | What it does |
|---|---|---|
| [`POST …`](#post-apiv1workspacesslugwebhooks) | `workspaces:write` | creates a subscription and shows its secret |
| [`GET …`](#get-apiv1workspacesslugwebhooks-and-id) | `workspaces:read` | lists the subscriptions |
| [`GET …/{id}`](#get-apiv1workspacesslugwebhooks-and-id) | `workspaces:read` | reads one, with its backlog |
| [`PATCH …/{id}`](#patch-apiv1workspacesslugwebhooksid) | `workspaces:write` | changes it, disables or re-enables it |
| [`DELETE …/{id}`](#delete-apiv1workspacesslugwebhooksid) | `workspaces:write` | deletes it |
| [`POST …/{id}/rotate-secret`](#post-apiv1workspacesslugwebhooksidrotate-secret) | `workspaces:write` | replaces the signing secret |
| [`POST …/{id}/ping`](#post-apiv1workspacesslugwebhooksidping) | `workspaces:write` | sends a test delivery |
| [`GET …/{id}/deliveries`](#get-apiv1workspacesslugwebhooksiddeliveries) | `workspaces:read` | lists the delivery log |
| [`POST …/{id}/deliveries/{delivery_id}/redeliver`](#post-apiv1workspacesslugwebhooksiddeliveriesdelivery_idredeliver) | `workspaces:write` | sends a finished delivery again |

### `POST /api/v1/workspaces/{slug}/webhooks`

Creates an `active` subscription and answers `201` with it and its
`secret`. A replay with the same `Idempotency-Key` answers `200` without
the secret.

| Member | Value |
|---|---|
| `url` | required: an absolute `https://` URL, without userinfo or fragment, at most 2,048 characters |
| `types` | 1 to 20 entries, each an exact type or a prefix ending in `*`; default `["*"]` |
| `include_forks` | whether the events of task forks are sent too; default `true` |
| `description` | free text of at most 1,024 characters; default `""` |

| Error | When |
|---|---|
| `400` `invalid_request` | a field breaks its rule |
| `400` `webhook_destination_refused` | the host is a refused [destination](#the-request-you-receive) |
| `409` `workspace_archived` | the workspace is archived |
| `429` `webhook_limit_reached` | the workspace holds `[webhooks].max_per_workspace` subscriptions (default `20`), in whatever state |
| `503` `secrets_unavailable` | the hub has no encryption key configured to store the secret with |

### `GET /api/v1/workspaces/{slug}/webhooks` and `…/{id}`

The list is ordered oldest first and takes `state` (`active`, `failing` or
`disabled`), `limit` and `cursor`. Neither route returns the secret.

| Member | Value |
|---|---|
| `id`, `workspace`, `url`, `types`, `include_forks`, `description` | as created or changed |
| `state` | `active`, `failing` or `disabled` |
| `created_by` | the creator's user id, `null` for the admin token |
| `created_at`, `updated_at` | timestamps |
| `last_delivery_at`, `last_status` | time and outcome (`succeeded` or `failed`) of the last attempt, `null` before the first |
| `failing_since` | the first failed attempt since the last success, else `null` |
| `pending` | only on `GET …/{id}`: how many events are waiting to be sent, `0` when `disabled` |

### `PATCH /api/v1/workspaces/{slug}/webhooks/{id}`

Changes the fields given, at least one of `url`, `types`, `include_forks`,
`description` (by the rules of the create) and `active`. A new `url` or
filter applies from the next attempt; a delivery that is being retried is
still sent.

- `active: false` disables the subscription and gives up its pending
  delivery.
- `active: true` makes a `disabled` or `failing` subscription `active`. A
  `disabled` one resumes with the events written from now on: what it
  missed is not sent. Read it from the [event log](#get-apiv1eventslog) if
  you need it.

Errors: `400` `invalid_request` (no field, an unknown field, a bad value),
`400` `webhook_destination_refused`, `409` `workspace_archived`.

### `DELETE /api/v1/workspaces/{slug}/webhooks/{id}`

Deletes the subscription with its delivery log and secret, and answers
`204`. Its deliveries stop.

### `POST /api/v1/workspaces/{slug}/webhooks/{id}/rotate-secret`

Takes no body. Replaces the secret and answers `200` with the subscription
and the new `secret`. The old secret stops being used at once, also for a
delivery being retried, so update the receiver right after. Errors: `409`
`workspace_archived`, `503` `secrets_unavailable`.

### `POST /api/v1/workspaces/{slug}/webhooks/{id}/ping`

Takes no body. Sends one test delivery at once, in any subscription state,
and answers `200` with its [delivery record](#get-apiv1workspacesslugwebhooksiddeliveries)
once the attempt ended, whether it succeeded or failed. The body sent is
shaped like an event of type `parley.webhook.ping` with `seq` `0`, signed
like any delivery. A ping is never retried and changes nothing on the
subscription. `503` `secrets_unavailable` means the hub could not read the
signing secret; nothing was sent.

### `GET /api/v1/workspaces/{slug}/webhooks/{id}/deliveries`

Lists the delivery records, pings included, newest first, with `status`
(`pending`, `succeeded` or `failed`), `limit` and `cursor`. Records are
kept for 7 days after their last attempt; a `pending` one is never deleted.

| Member | Value |
|---|---|
| `id` | the delivery id, sent as `Parley-Delivery` |
| `seq`, `type` | the event's; `0` and `parley.webhook.ping` for a ping |
| `status` | `pending` (the hub keeps trying), `succeeded` or `failed` |
| `attempts` | the attempts made |
| `response_code` | the status of the last attempt's response, `null` when none arrived |
| `error` | why the last attempt failed without a usable status: `timeout`, `connection_failed`, `tls_error`, `destination_refused`, `redirect`; else `null` |
| `duration_ms`, `last_attempt_at` | of the last attempt |
| `created_at`, `next_attempt_at` | when it was recorded, and when the next attempt is due (`null` unless `pending`) |

A record holds neither the payload nor your response body.

### `POST /api/v1/workspaces/{slug}/webhooks/{id}/deliveries/{delivery_id}/redeliver`

Takes no body. Sends the event of a `succeeded` or `failed` delivery once
more, at once and outside the normal order, under the same delivery id, to
the subscription's current URL with its current secret. It answers `200`
with the updated record, whether the attempt succeeded or failed, is not
retried and changes nothing on the subscription.

| Error | When |
|---|---|
| `404` `delivery_not_found` | no such delivery of the subscription |
| `400` `invalid_request` | the delivery is a ping: send a new ping |
| `409` `delivery_pending` | the hub is still trying it |
| `410` `event_expired` | the event has left the event log |
| `503` `secrets_unavailable` | the signing secret cannot be read |
