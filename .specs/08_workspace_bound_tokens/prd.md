---
spec_id: "08"
spec_name: "workspace_bound_tokens"
title: "Bind tokens to one workspace"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "31eb089e25eaa8a67069185b6f96db2b42e49d3d4474b21862cb3265e3f42ea5"
schema_version: 3
source: "docs/prd/03_platform.md"
---

# Bind tokens to one workspace

## Intent

A developer hands an agent a token that reaches one workspace and nothing else, so that a leaked or misused agent token cannot touch any other repository, any identity route or any other person's work. The hub confines such a token on every surface, reports each attempt to leave its workspace, and revokes it when its workspace goes away.

## Goals

- `POST /workspaces/:slug/tokens` mints a personal access token (PAT) bound to that workspace, atomically, and `GET /workspaces/:slug/tokens` lists the tokens bound to it.
- A binding guard runs for every bound PAT on every REST route and on git, after authentication and before the scope check, and answers `404 not_found` or `403 token_bound` for everything the binding does not cover.
- A token bound to a workspace reaches that workspace and the task forks created with it; a token bound to a task fork reaches the fork and, on the fork's parent, only the 25 contributor routes listed below.
- Each refusal is reported as `parley.token.bound_refused`, at most one event per token per minute, with the refusals in between counted as `suppressed`.
- Every live token bound to a workspace is revoked in the transaction that removes the workspace, and each maintenance pass revokes any live token whose workspace no longer exists.
- `parley tokens create --workspace <slug>` and `parley tokens list --workspace <slug>` call the two routes.
- Other packages get three components: one that mints a bound token inside their own transaction (fork creation, the hub credential of a sandbox), one that revokes the tokens bound to a set of workspaces inside their own transaction (workspace delete, fork expiry), and one that tells a route what the caller's binding covers.

## Non-goals

- PATs in general: `POST /user/tokens`, body validation, the scope registry, the subset rule, the `paths` field and its cover rule, permission changes, revocation, `GET /user/tokens` and the `parley tokens` command group without `--workspace`: package 06 api_keys_and_access_tokens. This package calls its validation and its subset rule and does not restate them.
- Authentication of a PAT (the `401` answers, the blocked user): package 04 authentication_and_login.
- The workspace access rule, access levels, the `404 workspace not found` and the member's `403`, and the readable set of an unbound caller: package 07 workspace_access_rule.
- The error envelope, the order of checks, list paging and the `Idempotency-Key` mechanism: package 02 http_api_conventions.
- Fork creation, the `token` object of the fork-create call, and recording the creating token on a new fork: package 15 task_fork_creation. It calls the minting component of this package.
- Deleting a workspace, expiring a fork, and the `revoked_tokens` member of `parley.workspace.delete` and `parley.fork.expire`: packages 14 workspace_archive_reclone_delete and 16 task_fork_lifetime. They call the revoking component of this package.
- The hub credential of a sandbox (its scopes, its lifetime, minting at create and at each start, revoking at stop and delete): packages 45 sandbox_creation_and_credentials and 47 sandbox_lifecycle_and_checkpoints. They call the minting component of this package.
- The git routes and how a refusal is written for a git client: package 17 git_smart_http. It calls the guard of this package. The push rule that a bound token is never exempt on a protected branch: package 18 git_push_rules.
- The routes a fork-bound token reaches on the parent, and their confinement to changes whose source is the fork (CHG-FR-040, LAND-FR-003, AWR-FR-001), including "never with `override`": packages 22, 23, 24, 25, 26, 27, 28, 29 and 30. This package only lets those routes through and tells them the fork.
- Confinement of the allowlisted routes' results (`GET /workspaces`, `/sessions…`, `/audit`, `/events`, `/events/log`, `GET /sandbox-profiles`), including the `404` for a `workspace_slug` in a body or filter outside the binding: packages 11, 49, 51, 52 and 44. This package gives them the covered set.
- Storing events, their `seq`, their visibility (`parley.token.*` only to `owner` and `admin`) and reading them: package 52 durable_event_log_and_stream. This package emits its two events through that package's event component.
- The maintenance loop, its hourly timer and the order of its tasks: package 03 server_lifecycle_and_configuration. This package registers one task in it.
- CLI settings, output, exit codes and path encoding (PLAT-FR-090 to PLAT-FR-095): package 09 cli_foundation.

## Background

The repository holds no code yet; every file named here is new, except that `internal/cli/tokens.go` and its test are first created by package 06 and extended here. The source is `docs/prd/03_platform.md`: PLAT-FR-080 to PLAT-FR-087, the two routes of §3.5, the two CLI entries of §3.6, the refusal limiter of §3.7, the two `parley.token.*` events of §3.8 and the bound-token edge cases of §3.10 (user story PLAT-US-04).

A PAT is the only credential that can carry a workspace binding (§3.3.2). Package 06 delivers the PAT with its `workspace` member (a slug or null, fixed at creation); package 07 delivers the workspace record and the access rule; package 02 delivers the position in the check order where the binding guard runs (step 5, after body checks and before the PAT scope check). This package fills that position and adds the two routes that create and list bound tokens.

The code lives in `internal/access/` (guard, coverage, minting, revoking, refusal limiter, sweep, the two route handlers) and in `internal/cli/tokens.go` (the `--workspace` flag).

## Requirements

### Binding (PLAT-FR-080)

A PAT may be bound to exactly one workspace. The binding is set when the token is created and never changes: no route changes a token's `workspace`, and a permission change by the owner leaves `workspace` and `paths` as they were (§3.10). A bound token is an ordinary PAT of the user who minted it. It has that user's access, cut down by the binding. The binding never grants access; it only removes it: if the owner loses access to the workspace, the bound token gets what the owner would get.

A bound token comes into being in three ways: through `POST /workspaces/:slug/tokens` (this package), through fork creation (package 15) and as the hub credential of a sandbox (package 45). All three go through one minting component of this package, which works inside the caller's transaction, applies the body validation and, for a PAT creator, the subset rule of package 06, writes the token with its binding and records `parley.token.bind` in the same transaction. If the transaction is rolled back there is no token and no event.

### Minting over REST (PLAT-FR-082, `POST /workspaces/:slug/tokens`)

The body is that of `POST /user/tokens`: `name` (1–255 characters), `permissions` (at least one registered scope), `paths` (optional), `expires` (0, 30, 60 or 90 days; default 90). The answer is `201` with the `Token` object, its `workspace` set to the slug and the member `token` holding the value `parley_pat_<token_id>_<secret>`. The value is shown in this response only. The route's need is `owner`.

Failures, in the order of checks:

| Caller or request | Answer |
|---|---|
| a bound PAT naming a workspace its binding does not cover | `404 not_found`, `workspace not found` |
| a bound PAT naming a workspace its binding covers | `403 token_bound` |
| an unbound PAT without `tokens:manage`, or asking for a permission or path it does not hold (subset rule) | `403 insufficient_scope` |
| no access to the workspace, or no such workspace | `404 not_found`, `workspace not found` |
| the admin token | `403 forbidden` |
| an org member who is not the owner | `403 forbidden` |
| a body that fails validation | `400 invalid_request` |
| the workspace is archived | `409 workspace_archived` |

Minting is atomic: either a bound token exists and is returned, or nothing was created. A token is never left unbound by a failure. A failure to record the bind event fails the request with `500 internal` and leaves no token.

The route takes `Idempotency-Key`. A replay answers `200` with the original token object without the `token` member (§3.10).

`parley.token.bind` carries: actor the minting user with actor type `api_key` or `pat`; resource type `token`, resource id the token id, action `bind`, workspace the bound slug; metadata `token_id`, `workspace`, `permissions`, `paths` (or null), `expires_at` (or null). It never carries the token value.

### Listing (PLAT-FR-084, `GET /workspaces/:slug/tokens`)

Answers `200` with a list of `Token` objects, oldest first: every token bound to the workspace, live, revoked or expired. No item carries a `token` member. The list follows the paging conventions (default limit 50). It works on archived workspaces. The need is `owner`; the admin token may list the bound tokens of any workspace. Failures: a bound PAT `403 token_bound` (or `404` outside its binding); an unbound PAT without `tokens:read` `403 insufficient_scope`; no access or no such workspace `404 not_found`; an org member `403 forbidden`.

### Coverage (PLAT-FR-081, PLAT-FR-063, §3.10)

A token bound to workspace W covers:

- W itself, in every state. Archiving does not touch bound tokens: the token still reaches an archived W and gets the routes' usual archived answers;
- every task fork of W that was created by a request authenticated with this token. The hub records the creating token on the fork. A fork of W that another user, or another token of the same user, created is not covered.

A token bound to a task fork F covers F. On F's parent P it reaches only the contributor routes of the next section.

For cross-workspace reads, the readable set of a bound PAT is the readable set of its user cut down to the covered workspaces.

### The binding guard (PLAT-FR-083, PLAT-FR-013 step 5)

The guard runs for bound PATs only. An API key, the admin token and an unbound PAT are never touched by it. It runs after authentication: a bound token's id with a wrong secret gets `401`, and an unknown path or a wrong method gets `404` or `405` as for any caller. It runs before the PAT scope check: a bound PAT without the route's scope on a workspace outside its binding gets `404`, not `403 insufficient_scope`.

| Request | Answer |
|---|---|
| names a workspace the binding covers | passes to the route's own checks; a route that declares that it refuses bound tokens answers `403 token_bound` |
| names the parent of the fork the token is bound to | the contributor routes pass to their own checks; every other route is `403 token_bound` |
| names any other workspace, existing or not | `404 not_found`, `workspace not found`, the same body in both cases |
| names no workspace and is on the allowlist | passes; the route confines its result to the covered workspaces |
| anything else, including every identity route | `403 token_bound` |

The allowlist: `GET /workspaces`, `GET /events`, `GET /events/log`, `GET /audit`, `GET /sandbox-profiles`, `POST /sessions`, `GET /sessions`, `GET /sessions/:id`, `POST /sessions/:id/complete`, `POST /sessions/:id/usage`, `GET /sessions/:id/usage`.

The contributor routes on the parent, all under `/workspaces/:slug` (from CHG-FR-040, LAND-FR-003, AWR-FR-001):

- changes: `POST /changes`, `GET /changes`, `GET /changes/:number`, `GET /changes/by-commit/:sha`, `PATCH /changes/:number`, `POST /changes/:number/abandon`, `POST /changes/:number/restore`, `POST /changes/:number/restack`, `GET /changes/:number/versions`, `GET /changes/:number/versions/:version`, `GET /changes/:number/diff`, `GET /changes/:number/stack`;
- landing and policy reads: `POST /changes/:number/land`, `GET /changes/:number/lands`, `GET /lands/:id`, `GET /changes/:number/conflict`, `GET /changes/:number/conflict/file`, `GET /changes/:number/approvals`, `GET /changes/:number/checks`, `GET /policy`, `POST /policy/evaluate`;
- awareness reads: `GET /activity`, `GET /changes/:number/overlaps`, `GET /claims`, `GET /claims/:id`.

A request that passes carries, for its handler, what the binding is: the bound slug and whether the named workspace is the bound one, a fork the token created, or the parent of the bound fork. The owning packages use it to confine a parent route to changes whose source is the fork.

Routes addressed with a fork's own slug that act on the parent's data (awareness and claims, forge, resolved variables) are routes of the fork and pass the guard.

A bound token's `tokens:*`, `keys:*`, `users:*` and `orgs:*` scopes have no use: identity routes answer it `403 token_bound`, including a token's attempt to read or revoke itself.

On git (`/git/:org/:slug.git/…`) the guard gives a decision for the slug: pass when the binding covers it, otherwise refuse with status `404`; package 17 writes the answer a git client reads.

### Refusal events (PLAT-FR-087, §3.7, §3.8)

Each request the guard refuses is reported as `parley.token.bound_refused`: actor the token's user with actor type `pat`; resource type `token`, resource id the token id, action `bound_refused`, workspace the bound slug; metadata `token_id`, `bound_workspace`, `requested_workspace` (only when the request named a workspace; the slug as the caller typed it), `method`, `route`, `status` (403 or 404), `surface` (`api` or `git`) and `suppressed` (an integer).

At most one event is emitted per token per minute. Refusals inside that minute emit nothing and are counted; the count is carried as `suppressed` on the token's next event and then starts again at 0. Tokens are limited independently. A request that passes the guard, a list that was filtered, and a request refused by another check (`401`, `403 insufficient_scope`, the access rule's `404`) are not refusals. The limiter state is in memory and is empty after a restart. A failure to emit never changes the HTTP answer.

### Removal of the workspace (PLAT-FR-085, PLAT-FR-086)

When a workspace is deleted or a task fork expires, every live token bound to it is revoked in the same transaction as the removal. The revoking component takes the slugs being removed, revokes each live token bound to one of them, and returns the revoked token ids sorted, for the removal's event. Tokens that are already revoked or expired are left as they are and are not returned. If the transaction is rolled back, no token was revoked. A revoked bound token answers `401 unauthenticated` from then on.

As a backstop, each maintenance pass runs the token binding sweep: it revokes every live token whose bound workspace no longer exists. Tokens bound to an existing workspace (archived ones included) and unbound tokens are not touched. A failing sweep returns its error to the maintenance loop and revokes nothing.

### CLI (§3.6)

- `parley tokens create --name <string> --permissions <list> [--paths <list>] [--expires <days>] --workspace <slug>` calls `POST /workspaces/<slug>/tokens` instead of `POST /user/tokens`, prints the `Token` with its value on stdout and warns on stderr that the value will not be shown again.
- `parley tokens list --workspace <slug>` calls `GET /workspaces/<slug>/tokens` instead of `GET /user/tokens` and prints the response body.

A hub error is printed on stderr as received and the command exits 1.

### End-to-end flows

1. **Mint and use.** A developer runs `parley tokens create --workspace W`; the CLI posts to the hub; the hub checks the caller, writes the token and the bind event in one transaction and answers `201`; the CLI prints the token. An agent then uses the value: a route of W passes the guard, a route of another workspace answers `404 workspace not found`, and `GET /user` answers `403 token_bound`.
2. **Refusal reporting.** An agent with a bound token calls routes outside its binding several times within a minute; the hub answers each one and emits one `parley.token.bound_refused`; after the minute the next refusal emits a second event whose `suppressed` is the number of refusals that were not reported.
3. **Sweep.** A bound token outlives its workspace record; the next maintenance pass revokes it; the agent's next request answers `401`.

## Design Decisions

1. **Where the code lives.** Guard, coverage, minting, revoking, limiter, sweep and the two route handlers are in `internal/access/`; the `--workspace` flag is added to `internal/cli/tokens.go` of package 06. Why: the brief's layout puts bound tokens in `internal/access`, and the same Go package as the access rule lets the readable set be cut down without an import cycle.
2. **The binding is stored as the slug**, as the PAT entity defines (`workspace`, a slug or null). The creating token of a fork is stored on the workspace record of package 07 as a nullable token id, added by this package and set by package 15. Why: PLAT-FR-081 says the hub records it on the fork.
3. **A token bound to a workspace that no longer exists** is not special-cased by the guard: a request naming that slug passes the guard and gets the access rule's `404`; every other workspace is `404` too. The sweep revokes it.
4. **"Names a workspace"** means that the matched route is under `/workspaces/:slug` (REST) or `/git/:org/:slug.git` (git). A slug in a query parameter or body is not seen by the guard; the allowlisted route confines it. Why: PLAT-FR-083 puts those routes on the allowlist and §14 gives them their own `404`.
5. **The contributor routes are one fixed table in this package**, matched by method and route pattern. Why: the PRD enumerates them (CHG-FR-040, LAND-FR-003, AWR-FR-001), and a fixed table can be tested before the routes exist.
6. **Routes that refuse every bound token declare it at registration**, and the guard answers `403 token_bound` at step 5, before the scope check. The two routes of this package use it; later packages use it for the routes the PRD marks "not a workspace-bound token". Outside the binding the `404` still comes first. Why: the PRD gives `403 token_bound` for these routes without placing it; step 5 is where every other `token_bound` is decided.
7. **On git every refusal is `404`**, also for the parent of the bound fork. Why: GIT-FR-003 names only `404` for the guard and §5 says the token "is answered like a stranger"; PLAT-FR-083's `403 token_bound` for the parent's other routes is read as applying to the REST API.
8. **Events are emitted through the event component of package 52** `durable_event_log_and_stream` (`internal/event`): `parley.token.bind` with its emit call that takes the caller's store transaction, so the token and its event commit together, and `parley.token.bound_refused` with its emit call that takes no transaction (`EmitNow`), as that package provides for a refused bound token. This package depends on package 52 and has no discarding fallback: the hub always passes the component. One Go constraint shapes the code: `internal/event` reads the workspace record of package 07 from `internal/access`, so `internal/access` cannot import `internal/event`. The guard and the minting component therefore call the component through a two-method interface declared in `internal/access` (`EventEmitter`: emit in a transaction, emit now), and `internal/hub/access.go` passes package 52's component behind it. Unit tests pass a recording implementation; the end-to-end checks run the real component and read the event log.
9. **The limiter's minute** starts when an event is emitted for the token; the clock is injectable. An entry whose minute has passed with a count of 0 is dropped at the next refusal of any token.
10. **`route` in the refusal event** is `/api/v1` followed by the pattern as registered with package 02's route registration, without the method (for example `/api/v1/workspaces/:slug/tokens`); for a request that matched no workspace it is still the matched pattern. Package 02 gives every check the matched route with its pattern and declarations.
11. **The hub credential of a sandbox appears in the list** like any bound token; "returned by no route" is read as its value. The minting component therefore takes an expiry instant, not only 0, 30, 60 or 90 days, for hub-internal callers.
12. **List order** is `created_at` ascending, then `token_id` ascending, because timestamps have whole seconds.
13. **The sweep and the revoking component emit no event of their own.** The removal's event carries `revoked_tokens`; the PRD defines no event for the sweep.
14. **`--workspace` with an empty value is a usage error** (exit 2, no request). Why: falling back to an unbound token would silently widen what the caller asked for.
15. **Tests** use the standard `testing` package with table-driven tests and `net/http/httptest`; the store is the embedded SQLite store of `internal/store` in a temporary directory (project decisions 2, 3, 7).
16. **Seams with earlier packages.** Minting calls the one minting operation of package 06 `api_keys_and_access_tokens` with the workspace binding and, for hub-internal callers, an expiry instant; this package does not validate or write a PAT by itself. The `--workspace` flag of both `parley tokens create` and `parley tokens list` is this package's, added to the commands package 06 created. The mark by which a route refuses every bound token is a named mark of package 02's route registration. The admin token on `POST /workspaces/:slug/tokens` gets `403 forbidden` with the message `this route needs a user`, the one message package 04 `authentication_and_login` gives the admin token wherever a user is needed; here it is answered by the handler, because the access check must run first. The sweep is registered under the maintenance task name `token_binding_sweep` that package 03 fixes. Wiring is in `internal/hub/access.go`.

## Dependencies

- Package 06 api_keys_and_access_tokens (draft): the PAT record with its `workspace` member, body validation, the subset rule, the scope check, revocation, the `parley tokens` commands.
- Package 07 workspace_access_rule (draft): the workspace record (slug, owner, org, kind, parent), the access rule with need `owner`, the readable set.
- Package 09 cli_foundation (draft): the command helper, the client with its segment encoder, the printers and the exit codes the `--workspace` flag is built on.
- Package 52 durable_event_log_and_stream (draft): the event component in `internal/event` with its two emit calls, through which `parley.token.bind` and `parley.token.bound_refused` are written.
- Through them: package 02 (check order position 5, the matched route and its named marks, error envelope, lists, idempotency), package 03 (store, the maintenance task registry with the fixed task name `token_binding_sweep`, the composition in `internal/hub/`), package 04 (authentication of the PAT).

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires: after 7 days by default, or 24 hours after all its changes have landed or been abandoned.
- **Change**: The unit of work: a proposal to add the commits of a source branch to a target branch, with a number, a title, an intent, versions and a state.
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
- **Bound token**: A PAT confined to one workspace (and the forks created with it). A token bound to a fork also reaches its own changes, and the awareness and policy reads, on the fork's parent (§3).
- **Path-scoped token**: A PAT whose writes are limited to files matching a list of globs.
- **Hub credential**: The fork-bound PAT the hub mints for a sandbox so that it can reach the hub. The workload sees only its placeholder, in `PARLEY_API_KEY`.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
- **Binding guard**: The check that confines a bound PAT, run after authentication on every REST route and on git.
- **Covered workspace**: A workspace a binding reaches in full: the bound workspace and the task forks created with the token.
