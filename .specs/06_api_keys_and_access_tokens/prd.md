---
spec_id: "06"
spec_name: "api_keys_and_access_tokens"
title: "Issue and revoke API keys and scoped tokens"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "1f508003fbb7facd6521ba44242db6e22939983de72924022868f36f4a032758"
schema_version: 3
source: "docs/prd/03_platform.md"
---

# Issue and revoke API keys and scoped tokens

## Intent

Parley gives a person an API key and gives automation personal access tokens (PATs) that carry only the scopes, and optionally only the repository paths, their job needs. This package lets a user see, refresh and revoke their API keys, mint, read, change and revoke PATs, and lets an administrator list and revoke any user's keys and tokens. It also fixes the scope registry and the rules by which a token can never create or widen a token beyond itself, so that a leaked or misused token reaches no more than its owner chose to give it.

## Goals

- A fixed registry of 32 scopes with fixed implication rules, and one scope check that every route of every package uses for PAT callers: a PAT without the needed scope gets HTTP 403 with `error_type` `insufficient_scope`, whether or not the addressed resource exists.
- A user lists their API keys, replaces the secret of a live key (`POST /user/keys/:key_id/refresh`) and revokes a key (`DELETE /user/keys/:key_id`).
- A user mints a PAT (`POST /user/tokens`) with a name, scopes, an optional `paths` limit and a lifetime of 0, 30, 60 or 90 days; the token value appears only in the creation response.
- A PAT can mint or widen tokens only within the scopes it holds exactly and the paths its own globs cover (the subset rule).
- A user reads and revokes their PATs and replaces, adds and removes scopes on them; a change that leaves no scope revokes the token.
- One implementation of path globs (validation, matching a file path, and the cover test between two globs) that the packages enforcing path limits call.
- An administrator lists and revokes the keys and tokens of any user.
- The CLI commands `parley keys list|refresh|revoke`, `parley tokens list|create|show|revoke|replace|add|remove`, `parley admin keys list|revoke` and `parley admin tokens list|revoke`.

## Non-goals

- How a credential authenticates (wire formats, secret hashing, the 401 answers, blocked users), creation of API keys at OAuth login and `parley login`: package 04 authentication_and_login.
- Users, organizations, the admin check, user selectors, and the deletion of a user's keys and tokens when the user is deleted (PLAT-FR-048): package 05 users_and_organizations.
- The workspace access rule: package 07 workspace_access_rule.
- Tokens bound to a workspace: `POST /workspaces/:slug/tokens`, `GET /workspaces/:slug/tokens`, the binding guard (including `403 token_bound` on the identity routes of this package), the `parley.token.*` events, and the `--workspace` flag of `parley tokens create` and `parley tokens list`: package 08 workspace_bound_tokens. This package stores and returns the `workspace` member of a token and always sets it to null on the tokens it mints through `POST /user/tokens`.
- Minting a token in the fork-create call: package 15 task_fork_creation. Minting the hub credential of a sandbox: package 45 sandbox_creation_and_credentials. Both mint only through package 08's minting component `MintBound`, which calls the minting operation of this package; neither calls this package's minting operation directly.
- Enforcing `paths` on writes (`403 path_not_allowed`): packages 18 git_push_rules, 21 scm_write_api, 22 change_opening_and_versions and 29 auto_land_revert_and_path_limits. This package gives them the glob matcher and nothing else.
- The error envelope, the order of checks, list paging, idempotency records and the rate limit: package 02 http_api_conventions. The CLI's settings, output, exit codes and HTTP client: package 09 cli_foundation.
- Events: user, API key and unbound PAT operations emit no events (§3.8).

## Background

The repository holds no code yet; every file named here is new. The module is `github.com/agent-fox-dev/parley`. Identity code lives in `internal/identity/`, the HTTP conventions in `internal/server/` (package 02), the store in `internal/store/` (package 03), and the CLI in `internal/cli/` with one file per command group. Package 04 defines the stored API key and PAT records as far as authentication needs them and reports the authenticated caller (admin token, API key or PAT, and the user) to later checks and to the handler. Package 02 fixes step 6 of the check order as the position of the PAT scope check and leaves it empty; this package fills it.

The source is `docs/prd/03_platform.md`: §3.3.2 to §3.3.5, PLAT-FR-050 to PLAT-FR-059, the routes of §3.5 under `/user/keys`, `/user/tokens`, `/users/:id/keys` and `/users/:id/tokens`, the commands of §3.6 named above, and the related entries of §3.10.

## Requirements

### Scope registry and the scope check (§3.3.5, PLAT-FR-013 step 6)

A scope is `resource:action`. The registry is fixed and holds exactly these 32 scopes, whether or not a feature is enabled: `users:read`, `users:write`, `orgs:read`, `keys:read`, `keys:manage`, `tokens:read`, `tokens:write`, `tokens:manage`, `workspaces:read`, `workspaces:write`, `git:read`, `git:write`, `changes:read`, `changes:write`, `changes:approve`, `checks:write`, `patches:read`, `patches:write`, `rebuilds:read`, `rebuilds:write`, `secrets:read`, `secrets:write`, `vars:read`, `vars:write`, `forge:read`, `forge:write`, `sandboxes:read`, `sandboxes:write`, `audit:read`, `audit:write`, `sessions:read`, `sessions:write`. Names are case-sensitive.

Each route declares the one scope a PAT needs. The check accepts a PAT when it holds the needed scope or a scope that implies it:

- a check for `R:read` accepts `R:read` or `R:write`; a check for `R:write` accepts only `R:write`;
- `keys:manage` implies `keys:read`; `tokens:manage` implies `tokens:write` and `tokens:read`; `tokens:write` implies `tokens:read`;
- `changes:approve` and `checks:write` are never implied by another scope and imply nothing.

Only PATs are scope-checked: the admin token and API keys pass every scope check. A PAT that fails gets HTTP 403 with `error_type` `insufficient_scope`; the check runs before workspace access, the admin check, validation and the handler, so the answer is the same whether or not the addressed resource exists.

### API keys (PLAT-FR-054, PLAT-FR-055, PLAT-FR-056)

The `Key` object is `{"key_id","user_id","expires_at","revoked_at","created_at"}`; `expires_at` and `revoked_at` are null when unset. A key is live, revoked (final) or expired (by time, once `expires_at` has passed). Records are kept in every state. Only the OAuth callback creates keys (package 04).

- `GET /user/keys` (PAT scope `keys:read`): `200` with the list of all the caller's keys in any state, newest first.
- `POST /user/keys/:key_id/refresh`: replaces the secret of one of the caller's own live keys, keeps `key_id`, and restarts the lifetime from now (`expires_at` becomes now plus the key's `expires_days`, or stays null when that is 0). The answer is `200` `{"key":"parley_<key_id>_<new secret>","key_id":"…","expires_at":…}`. The old secret stops authenticating at once. The caller MUST be an API key: a PAT and the admin token get `403 forbidden`. A revoked or expired key is `409 conflict`.
- `DELETE /user/keys/:key_id` (PAT scope `keys:manage`): revokes one of the caller's own keys and answers `200` with the `Key`, `revoked_at` set. An expired key may be revoked. A key may revoke itself. Revoking an already revoked key answers `200` with the unchanged object (PLAT-FR-047).
- A `key_id` that does not exist or belongs to another user is `404 not_found` on both routes.

### Creating a PAT (PLAT-FR-050, §3.10)

The `Token` object is `{"token_id","user_id","name","permissions","paths","workspace","expires_at","revoked_at","created_at"}`; `paths`, `workspace`, `expires_at` and `revoked_at` are null when unset. A token is live, revoked or expired, as a key is.

`POST /user/tokens` (PAT scope `tokens:manage`) takes `name` (required, 1–255 characters), `permissions` (required, at least one entry, each a registered scope; duplicates are dropped and the order given is kept), `paths` (optional, see below) and `expires` (optional integer, one of 0, 30, 60 or 90 days, default 90; 0 means never). It answers `201` with the `Token` plus `"token":"parley_pat_<token_id>_<secret>"`; `workspace` is null and `expires_at` is the creation instant plus `expires` days, or null. The hub stores only a hash of the secret, and no other response ever carries the value.

Failures: body validation is `400 invalid_request` and creates nothing; a PAT without `tokens:manage` is `403 insufficient_scope`; the admin token is `403 forbidden` because it has no user. A replay with the same `Idempotency-Key` answers `200` with the token object without the `token` member and creates nothing.

### The subset rule (PLAT-FR-051, PLAT-FR-059, §3.10)

A PAT can never create or widen a token beyond itself. When the caller is a PAT:

- every requested permission MUST be held by the caller exactly; implication does not count (a caller with `git:write` cannot grant `git:read` unless it holds it);
- a caller without `paths` may request any `paths`; a caller with `paths` MUST request `paths` (omitting them, or sending null, is refused, not defaulted), and each requested glob MUST be covered by one of the caller's globs.

A violation is `403 insufficient_scope`; the message names the first offending permission or glob; nothing is created. API key callers are not restricted.

### Path-scoped tokens: the `paths` field, matching and covering (PLAT-FR-057, PLAT-FR-058, PLAT-FR-059)

`paths` is a list of globs over repository paths, set at creation, stored on the token, returned in every token object and never changed afterwards. Absent or null means unrestricted. Validation (each failure is `400 invalid_request`): an empty array; more than 50 globs; an entry that is not a string; a glob shorter than 1 or longer than 255 characters; a glob that starts with `/`. `paths` on a token without any write scope is accepted and has no effect.

Matching a file path: `*` matches any run of characters within one path segment (no `/`); `**` matches any run of characters including `/`; every other character matches itself. Matching is case-sensitive, uses `/` as the separator and covers the whole path. `docs/**` matches `docs/a.md` and `docs/api/b.md`; `src/*.go` matches `src/main.go` but not `src/x/y.go`.

Covering: glob `C` covers glob `G` when `C`, used as a pattern, matches the text of `G` taken literally, with one restriction: a `*` in `C` never matches a run that contains `**`. Equal globs always cover each other. `src/**` covers `src/api/*.go` and `src/**`; `src/*` does not cover `src/**`; `src/**` does not cover `docs/**`. The test is conservative: it may refuse a glob that is narrower in fact and never accepts a wider one.

`paths` restricts writes only; enforcement is in other packages (see Non-goals), which call the matcher of this package.

### Reading and revoking PATs (PLAT-FR-053, PLAT-FR-056)

- `GET /user/tokens` (PAT scope `tokens:read`): `200` with the list of all the caller's PATs in any state, bound ones included, newest first.
- `GET /user/tokens/:token_id` (PAT scope `tokens:read`): `200` `Token`.
- `DELETE /user/tokens/:token_id` (PAT scope `tokens:manage`): revokes the token and answers `200` with the `Token`, `revoked_at` set. A PAT may revoke itself. An expired token may be revoked. Revoking an already revoked token answers `200` with the unchanged object.
- A `token_id` that does not exist or belongs to another user is `404 not_found` on every `/user/tokens/:token_id…` route.
- The admin token gets `403 forbidden` on every route under `/user/keys` and `/user/tokens` (PLAT-FR-021).
- For callers inside the hub this package also exports two operations that have no route and run inside the caller's transaction: the **revocation by id** of one token, and the **expiry update of one token by id**, which sets the token's `expires_at` to a given instant. Neither checks ownership, neither writes an event, and an unknown `token_id` is an error to the caller. The sandbox packages 45 and 47 use them for the hub credential of a sandbox.

### Permission changes (PLAT-FR-052)

`PUT`, `PATCH` and `DELETE` on `/user/tokens/:token_id/permissions` take `{"permissions": string[]}` and replace, add and remove scopes on one of the caller's own tokens. All answer `200` with the `Token` after the change. PAT callers need `tokens:write`.

- `PUT` replaces the list (duplicates dropped, order given). An empty array is allowed and revokes the token.
- `PATCH` adds scopes after the existing ones; scopes already present are kept as they are. At least one entry is required.
- `DELETE` removes scopes; scopes the token does not have, registered or not, are ignored. At least one entry is required. There is no subset check.
- A change that leaves no scope revokes the token: the answer has `permissions` `[]` and `revoked_at` set.
- `PUT` and `PATCH` validate every scope against the registry (`400 invalid_request`); a missing or non-array `permissions` is `400`.
- When the caller is a PAT, the subset rule applies to every scope the change would add (`403 insufficient_scope`, token unchanged).
- A revoked or expired token is `409 conflict`.
- `paths` and `workspace` cannot be changed; members of those names in the body are ignored.

### Admin key and token routes (§3.5, PLAT-FR-045, PLAT-FR-047)

`GET /users/:id/keys` and `GET /users/:id/tokens` list every key or token of the selected user, newest first. `DELETE /users/:id/keys/:key_id` and `DELETE /users/:id/tokens/:token_id` revoke one and answer `200` with the object, `revoked_at` set; revoking one that is already revoked answers `200` with the unchanged object. All four need an admin (the admin token, or an API key whose user has role `admin`; a PAT is never admin): anyone else gets `403 forbidden`. `:id` is a user selector (UUID, e-mail or username, package 05). An unknown user, or a key or token that is not that user's, is `404 not_found`.

### CLI commands (§3.6)

Each command prints the route's response body on stdout and follows the global behaviour of package 09.

- `parley keys list` calls `GET /user/keys`.
- `parley keys refresh` calls `POST /user/keys/<key_id>/refresh` with the `key_id` of the configured API key, prints the response, warns on stderr that the key will not be shown again, then saves the new key as `api_key` in the config file. If the save fails the response has still been printed; the command warns on stderr and exits 2. A configured credential that is not an API key is a client error (exit 2, no request sent).
- `parley keys revoke` calls `DELETE /user/keys/<key_id>` for the configured API key, prints the response, clears `api_key` and `user_id` in the config file, and tells the user on stderr to run `parley login`. A configured credential that is not an API key is a client error.
- `parley tokens list` calls `GET /user/tokens`; `parley tokens show <token_id>` calls `GET /user/tokens/<token_id>`; `parley tokens revoke <token_id>` calls `DELETE /user/tokens/<token_id>`.
- `parley tokens create --name <string> --permissions <list> [--paths <list>]… [--expires <days>]` calls `POST /user/tokens`. `--name` and `--permissions` are required; `--permissions` is comma-separated; `--paths` is comma-separated and repeatable; `--expires` defaults to 90. It prints the `Token` with its value and warns on stderr that the value will not be shown again.
- `parley tokens replace|add|remove <token_id> --permissions <list>` call `PUT`, `PATCH` and `DELETE` on `/user/tokens/<token_id>/permissions`; `--permissions` is required. `replace` and `remove` note on stderr when the change revoked the token.
- `parley admin keys list <user>`, `parley admin keys revoke <user> <key_id>`, `parley admin tokens list <user>` and `parley admin tokens revoke <user> <token_id>` call the four admin routes.

### End-to-end flows

- A developer runs `parley tokens create --name ci --permissions git:read,tokens:manage --paths 'docs/**'`. The parley CLI posts to `parleyd`, which validates, stores the hash and answers `201`; the CLI prints the token with its value. An agent holding that token asks `parleyd` for a token with `git:write`: refused, `403 insufficient_scope`. It asks for `git:read` with `paths` `docs/api/*.md`: created. The developer runs `parley tokens revoke <token_id>`; the agent's next request with the revoked token is answered `401`.
- A developer runs `parley keys refresh`. The parley CLI posts to `parleyd`, which replaces the secret; the CLI prints the new key and saves it. The old key now gets `401`; `parley keys list` with the saved key succeeds and shows the same `key_id`.
- An operator runs `parley admin tokens list alice` with the admin token, sees a live token, and runs `parley admin tokens revoke alice <token_id>`. `parleyd` revokes it; a request with that token is answered `401`.

## Design Decisions

1. **Storage and code.** Keys and tokens are rows of the transactional store (SQLite through `internal/store`, project decision 2); `permissions` and `paths` are kept as ordered lists. The code is in `internal/identity/`, routes are registered with `internal/server` using the standard library router (decision 3), the CLI uses cobra (decision 4), tests use `testing`, table-driven, with `net/http/httptest` (decision 7).
2. **A route declares one scope.** The scope check reads the scope the route declared at registration and the caller that authentication reported; a route that declares none passes. Why: §3.5 names one scope per route.
3. **Lifetimes.** `expires_at` is the creation (or refresh) instant plus `expires` × 24 hours, in whole seconds. Whether a credential is expired at a given instant is decided by the one predicate package 04's authentication uses, so that the `409` answers here and the `401` answers there never disagree. An `expires` that is not an integer or not one of 0, 30, 60, 90 is `400 invalid_request`. The identity component takes its clock as a parameter so tests can move time.
4. **Order inside token creation.** After the scope check: body validation (`400`), then the subset rule (`403`), then the write. An unregistered scope requested by a PAT is therefore `400`, not `403`. Why: the subset rule needs a valid body, and step 8 of the check order is validation. Permission changes follow: ownership (`404`), validation (`400`), subset rule (`403`), state (`409`).
5. **`paths` is stored as given.** Globs keep their order and duplicates are not removed; the limit of 50 counts the entries as sent. Why: the PRD drops duplicates for `permissions` only.
6. **Glob details.** A run matched by `*` or `**` may be empty (`src/*.go` matches `src/.go`). `?`, `[` and `\` have no special meaning and match themselves. `**` is a wildcard wherever it appears, not only as a whole segment; a longer run of asterisks is read as `**` followed by the rest.
7. **What a `*` may match when covering.** In the cover test a `*` of the caller's glob matches a run of characters that contains no `/` and does not contain the two-character sequence `**`; `**` matches any run. A caller with `paths` that omits `paths` gets a message saying that `paths` is required.
8. **A change that leaves no scope** stores the empty list and sets `revoked_at` in one transaction; the answer shows `permissions` `[]`.
9. **Revoking is idempotent for tokens too.** `DELETE /user/tokens/:token_id` on a revoked token answers `200` unchanged (PLAT-FR-047), and an expired token may be revoked, as an expired key may.
10. **Total list order.** "Newest first" is `created_at` descending, then the id descending, so paging is stable.
11. **Token ids.** `token_id` is 8 and the secret 32 random characters from `[a-z0-9]`, from `crypto/rand`; a `token_id` that already exists is generated again. The secret is hashed as package 04 hashes credentials. Generating and storing are done by the store-level PAT function of package 04, which the minting operation calls for its write.
12. **One minting operation.** Validation, the subset rule and the write are one exported operation of `internal/identity` that takes the caller, the request and an optional workspace binding and can run inside a caller's transaction, so that package 08's minting component `MintBound` calls it inside its caller's transaction; packages 15 and 45 mint only through `MintBound` and never call this operation directly. `POST /user/tokens` passes no binding. Agreed with package 04 `authentication_and_login`: this operation is the one way a PAT is created in production; it wraps package 04's store-level PAT function (id, secret, hash, insert), which no other package calls. For hub-internal callers (the sandbox credential of package 45, through package 08) the operation also accepts an expiry instant in place of `expires`.
13. **`parley keys refresh` without a config file.** Only `parley login` creates the config file (§3.3.7). When the file does not exist (the credential came from a flag or the environment), nothing is written and the command behaves as for a failed save: the response is printed, stderr warns that the new key was not saved, exit 2. `parley keys revoke` with no config file clears nothing and exits 0.
14. **CLI flag checks.** A missing required flag, a wrong argument count or a non-integer `--expires` is a usage error (exit 2) before any request; whether an integer is an allowed lifetime, and whether a scope is registered, is left to the hub.
15. **The refresh response is not an idempotent create.** `POST /user/keys/:key_id/refresh` creates no resource, so an `Idempotency-Key` on it is ignored (package 02).
16. **The scope-check message** names the scope the route needs. Clients read `error_type`, not the message.
17. **Tables, declarations and the CLI come from earlier packages.** The API key and PAT tables are created by package 04; this package adds no table and no column. A route's scope is the scope declaration of package 02's route registration. The `/user/keys` and `/user/tokens` routes carry package 04's needs-a-user mark, so the admin token gets `403 forbidden` with the message `this route needs a user`. The admin routes use package 05's admin check and user selector. The commands are built on package 09's command helper, client and configuration-file save. The `--workspace` flag of `parley tokens create` and `parley tokens list` is package 08's on both commands.
18. **Two operations for hub-internal callers** (decided with packages 45 sandbox_creation_and_credentials and 47 sandbox_lifecycle_and_checkpoints). `internal/identity` exports the revocation by id, `RevokeTokenByID(tx, token_id)`, and the expiry update of one token by id, `SetTokenExpiryByID(tx, token_id, instant)`. Both write through the transaction they are given and do not commit, so the caller's change of a sandbox and of its hub credential succeed or fail together. They have no route, check no ownership and write no event; a `token_id` that does not exist is a not-found error. Revoking a revoked token changes nothing; the expiry update changes `expires_at` only. Why here: package 08 revokes only all tokens bound to a set of workspaces, and the PAT records are this package's to change.

## Dependencies

- Package 04 authentication_and_login (draft): authenticates the admin token, API keys and PATs, defines their wire formats, stored records and secret hashing, reports the caller to this package's checks and handlers, and refuses revoked and expired credentials.
- Package 05 users_and_organizations (draft): the user record, the user selector on `/users/:id…` and the admin check that the admin key and token routes use.
- Package 09 cli_foundation (draft): the command helper, the shared client, the printers, the exit codes, the show-once warning and the one read and save of the client configuration file (`internal/cli/config.go`), which the `parley keys` commands write `api_key` through.
- Through them, package 02 http_api_conventions (route registration with its scope declaration, error envelope, check order, lists, idempotency), which this package uses and does not restate.

## Verified External API

Read in the Go module cache (`~/go/pkg/mod`), `github.com/spf13/cobra@v1.10.2` and `github.com/spf13/pflag@v1.0.10`; the version the project pins is set by package 01 and must be confirmed against `go.mod`.

- `cobra.Command` fields `Use string`, `Short string`, `Args PositionalArgs`, `RunE func(cmd *Command, args []string) error` (`command.go`).
- `func (c *Command) AddCommand(cmds ...*Command)`, `func (c *Command) Flags() *pflag.FlagSet`, `func (c *Command) SetArgs(a []string)` (`command.go`).
- `func (c *Command) MarkFlagRequired(name string) error` (`shell_completions.go`); `func ExactArgs(n int) PositionalArgs` (`args.go`).
- `func (f *FlagSet) StringVar(p *string, name string, value string, usage string)`, `func (f *FlagSet) StringSliceVar(p *[]string, name string, value []string, usage string)` (splits each value at commas and appends across repeats), `func (f *FlagSet) IntVar(p *int, name string, value int, usage string)` (pflag).

## Glossary

- **Hub**: The `parleyd` server.
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
- **Bound token**: A PAT confined to one workspace (and the forks created with it). A token bound to a fork also reaches its own changes, and the awareness and policy reads, on the fork's parent (§3).
- **Path-scoped token**: A PAT whose writes are limited to files matching a list of globs.
- **PAT**: Personal access token. The credential for automation, and the only one that carries scopes, a workspace binding or path limits.
- **API key**: The credential of a person at a terminal, created by OAuth login. It is not scope-checked.
- **Admin token**: The operator's credential, written at first boot. It has no user.
- **Subset rule**: A PAT can never create or widen a token beyond the scopes it holds exactly and the paths its globs cover.
