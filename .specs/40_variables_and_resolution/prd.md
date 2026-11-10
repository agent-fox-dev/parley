---
spec_id: "40"
spec_name: "variables_and_resolution"
title: "Store variables and resolve them along the scope chain"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "62207e1b80780d9b61c787295700cc8fe692989f946be23bc00daf5c23929ffe"
schema_version: 3
source: "docs/prd/11_secrets_and_variables.md"
---

# Store variables and resolve them along the scope chain

## Intent

A workspace runs with configuration that people set at three levels: a user, an organization and the workspace itself. Parley keeps readable named values, variables, at each of those levels and answers one question in one place: which value does a given workspace or task fork run with, and where did it come from. The same chain decides which stored secret a sandbox receives under a name, with one rule that must never bend: a fork owned by someone who cannot write to the parent never receives the parent's secrets. Putting the chain in one component means every caller, every route and every sandbox gets the same answer.

## Goals

- A user, an organization member and a workspace owner create, list, update and delete variables in the `user`, `org` and `workspace` scopes over twelve REST routes, with values readable in every answer.
- Keys are unique per scope ignoring case, at most 255 characters; values are at most 262,144 bytes; a scope holds at most 100 variables; a create is all-or-nothing.
- `GET /workspaces/:slug/vars/resolved` answers the variables a workspace or fork runs with, merged along the chain workspace → parent → org → user, each entry with its `origin`.
- One resolution function gives sandboxes the secret stored under each of a list of names for a workspace or fork, skipping the parent's scope when the fork's owner has no write access to the parent, and skipping reserved names in the parent's scope for everyone.
- Every created entry, update and delete writes one hub event without the value; a failed request writes none.
- A workspace's variables go when the workspace or fork is removed, in the same transaction; a workspace created again under the same slug starts empty.
- `parley vars create|list|update|delete|resolve` drive the routes from the shell, including a create that writes to several scopes in one command.

## Non-goals

- The encrypted secret store, encryption keys and rotation, the secret routes `/user/secrets`, `/orgs/:slug/secrets`, `/workspaces/:slug/secrets`, the events `parley.secret.*`, the `503 secrets_unavailable` answer, removal of secrets and `parley secrets …`: package 39 `secrets`. This package reads secrets through that store and never writes one.
- How a resolved secret becomes a sandbox credential or an exposed secret, the `422 credential_missing` answer, validation of the names a profile exposes, and when a sandbox resolves again (SBX-FR-024, SBX-FR-041, SBX-FR-065): packages 44 `sandbox_runtime_and_profiles`, 45 `sandbox_creation_and_credentials` and 47 `sandbox_lifecycle_and_checkpoints`. This package delivers the function they call.
- Origin, upstream and forge credential resolution from the reserved names of a workspace's own scope: packages 10 `workspace_create_and_clone`, 31 `carry_patch_setup_and_patches` and 41 `forge_reads`.
- The binding guard, and with it the answers a workspace-bound token gets on `/user/…` and `/orgs/…` routes (`403 token_bound`) and on a workspace outside its binding (`404`) (SEC-FR-002): package 08 `workspace_bound_tokens`. This package declares its user and org routes as naming no workspace and its workspace routes as naming `:slug`, and adds nothing to the guard.
- The workspace access rule itself and the question "does this user have write access to this workspace": package 07 `workspace_access_rule`. The scope registry and the scope check: package 06 `api_keys_and_access_tokens`. Users, organizations and memberships: package 05 `users_and_organizations`.
- The error envelope, check order, list paging, `Idempotency-Key` handling and body limits: package 02 `http_api_conventions`. The CLI's client, output and exit codes: package 09 `cli_foundation`.
- The event log, and who may see an event in `/events` and `/audit` (§11.8's visibility rule): packages 52 `durable_event_log_and_stream` and 51 `audit_query_and_retention`. This package writes each event with the fields that rule reads.
- Running a workspace's or a fork's removal: packages 14 `workspace_archive_reclone_delete` and 16 `task_fork_lifetime`. This package supplies the part that removes variables.
- Placing variables in a sandbox's environment: the source PRD defines no such behaviour; not covered by this package.

## Background

The repository holds no code yet; every file named here is new. The source is `docs/prd/11_secrets_and_variables.md`: the variable half of SEC-FR-001 to SEC-FR-017, the resolution chain of §11.3, the variable routes of §11.5, the `parley vars` commands of §11.6, the variable events of §11.8 and the edge cases of §11.10 that concern variables and resolution.

Package 39 `secrets` delivers, in `internal/secret/`, the encrypted secret store and the secret routes. This package adds to the same directory:

| File | Holds |
|---|---|
| `internal/secret/var_validate.go` | key and value rules, the create and update bodies |
| `internal/secret/var_store.go` | the variable table and its operations, the schema migration |
| `internal/secret/var_access.go` | which scope a request addresses and whether the caller may read or write it |
| `internal/secret/var_routes.go` | the twelve variable routes and the resolved route |
| `internal/secret/var_events.go` | the three variable events |
| `internal/secret/chain.go` | the resolution chain of a workspace |
| `internal/secret/resolve_vars.go` | the merge of variables along the chain |
| `internal/secret/resolve_secrets.go` | resolution of secrets by name along the chain |
| `internal/secret/var_remove.go` | removal of a scope's variables |
| `internal/cli/vars.go` | `parley vars …` |

Each has its test file beside it (`internal/secret/var_store_test.go`, …). The end-to-end test is `test/e2e/vars_test.go`. The process is composed in `internal/hub/`, where the removers of this package are registered.

Routes are registered through the router and helpers of `internal/server/` (package 02), the caller comes from the authenticated principal (package 04), the scope check is the one of `internal/identity/` (package 06), and workspace access, the workspace record and the write-access question come from `internal/access/` (package 07).

## Requirements

All paths are relative to `/api/v1`. Every route obeys the platform conventions (PLAT-FR-010 to PLAT-FR-019): the error envelope `{"error":{"code","message","error_type"}}`, the check order (authentication, binding guard, PAT scope, scope access, request validation, state, act), list paging and idempotency keys. They are not restated per route.

### Variable entries and limits

A variable belongs to one owner: a scope (`user`, `org` or `workspace`) and a scope id (the user id, the organization id, or the workspace slug). A task fork is a workspace and has its own `workspace` scope. An entry has a `key`, a `value`, `created_at` and `updated_at`. Responses carry `{key, value, created_at, updated_at}` and never the owner, which the route states. Timestamps are RFC 3339 in UTC with whole seconds.

| Rule | Value |
|---|---|
| Key characters | ASCII letters, digits and `_`; the first character is not a digit |
| Key length | 1 to 255 |
| Key matching | case-insensitive for uniqueness, update, delete and resolution; the key is stored and returned with the casing given at create |
| Value size | at most 262,144 bytes, counted in bytes; the empty string is allowed |
| Entries per scope | at most 100 variables per scope and scope id; secrets are counted apart |

Variable values are not encrypted. Every variable route keeps working when no secret encryption key is configured (SEC-FR-011).

### Who may use which scope

SEC-FR-001: a PAT needs `vars:read` or `vars:write` for a read (the lists and the resolved route) and `vars:write` for a write (create, update, delete); otherwise `403 insufficient_scope`. API keys and the admin token are not scope-checked.

| Scope | Routes under | Who | Refusal |
|---|---|---|---|
| `user` | `/user/vars` | any credential with a user; the scope id is the caller's user id | the admin token gets `403 forbidden` with the message `user-scoped endpoints require a user credential` (SEC-FR-004) |
| `org` | `/orgs/:slug/vars` | members of the organization, and the admin token; `:slug` is the organization's slug or its UUID, as on the secret routes | an unknown organization and a caller who is not a member get `404 not_found` with the same body (SEC-FR-005) |
| `workspace` | `/workspaces/:slug/vars` | SEC-FR-006: the owner and the admin token read and write; an org member reads (list, resolved) | a member's write gets `403 forbidden` with the message `org members have read-only access to this workspace`; anyone else, and an unknown slug, get `404 not_found` with the message `workspace not found` |

The workspace's status (for example `archived`) does not matter, and an exclusive maintenance operation does not block these routes (SEC-FR-006).

### Creating variables

`POST /user/vars`, `POST /orgs/:slug/vars`, `POST /workspaces/:slug/vars` (SEC-FR-007). The body is `{"entries":[{"key": string, "value": string}, …]}`; a missing `value` is the empty string. The route accepts `Idempotency-Key`. The answer is `201` with `{"items":[{key, value, created_at, updated_at}, …]}` in request order; every entry has the same `created_at`, equal to its `updated_at`.

The create is all-or-nothing. Checks run in this order and the first failure decides:

1. `entries` is a non-empty array: else `400 invalid_request`.
2. Each key is valid: else `400 invalid_request`, the message names the key.
3. Each value is a string within the size limit: else `400 invalid_request`, the message names the key.
4. No two keys of the request are equal ignoring case: else `400 invalid_request`, the message names the key.
5. The scope's count plus the new entries is at most 100: else `429` with `error_type` `secret_limit_reached` and the message `maximum of 100 entries per scope exceeded`. The route is specified "as `POST /user/secrets`", so variables use the same `error_type`. A full scope answers this even when a key of the request also conflicts (§11.10).
6. No key is already present ignoring case: else `409 conflict` with the message `key "<k>" already exists`.

Of two concurrent creates of the same key in one scope, exactly one answers `201` and the other `409 conflict`.

### Listing, updating and deleting

- `GET /user/vars`, `GET /orgs/:slug/vars`, `GET /workspaces/:slug/vars` answer `200` with `{"items":[…],"next_cursor":…}`, ordered by key ascending ignoring case, with `limit` and `cursor` as on every list (SEC-FR-010). A workspace's list holds only that workspace's own variables, never a parent's.
- `PATCH …/vars/:key` takes `{"value": string}` (SEC-FR-008). An absent or `null` value, a value that is not a string and a value over the size limit are `400 invalid_request`; `""` is valid. The key is matched ignoring case; no match is `404 not_found` with the message `key "<k>" not found`. The stored key casing and `created_at` are kept; `updated_at` becomes now. The answer is `200` with the entry.
- `DELETE …/vars/:key` matches the key ignoring case (SEC-FR-009). No match is `404 not_found` with the same message. Success is `204` with no body.
- `resolved` is reserved under `/workspaces/:slug/vars/` (SEC-FR-017): `PATCH` and `DELETE` on `/workspaces/:slug/vars/resolved` answer `405 method_not_allowed`. No route reads a single entry by key: `GET` on a `…/vars/:key` path answers `405 method_not_allowed`.

### Events

SEC-FR-016 and §11.8. Each created entry writes one `parley.variable.create`, each update one `parley.variable.update`, each delete one `parley.variable.delete`, in the same transaction as the change. `resource_type` is `variable`, `action` is `create`, `update` or `delete`, `resource_id` is the stored key, and `metadata` is `{"scope": "user"|"org"|"workspace", "scope_id": <user id, org id or workspace slug>, "key": <stored key>}`. The event's `workspace` field is the slug for the workspace scope and null otherwise. No event carries a value. A failed request writes none. If the event cannot be written the change is not made and the answer is `500 internal`.

### The resolution chain

§11.3. The chain of a workspace W, highest priority first:

| Step | Scope | `origin` | Included when |
|---|---|---|---|
| 1 | W's `workspace` scope | `workspace` | always |
| 2 | the parent's `workspace` scope | `parent` | W is a task fork |
| 3 | the `org` scope of W's organization | `org` | W has an organization and W's owner is currently a member of it |
| 4 | the `user` scope of W's owner | `user` | always |

The user is always the workspace's owner, never the caller. For a fork, the owner is whoever created the fork and the organization is the parent's. The chain is the same for every caller, the admin token included, and is worked out again at every resolution, so a change of membership or ownership applies to the next one.

### Resolved variables

`GET /workspaces/:slug/vars/resolved` (SEC-FR-015) needs only variable read access on `:slug`: the owner, the admin token or an org member, and for a PAT `vars:read` or `vars:write`. It needs no access to any other scope of the chain: resolution is use, not read access. It answers `200` with a list `{"items":[{key, value, origin, created_at, updated_at}, …],"next_cursor":…}` ordered by key ascending ignoring case.

Variables resolve by merge: for each key, compared ignoring case, the entry of the highest-priority scope that has it wins entirely, with its key casing, value, timestamps and `origin`. A fork's resolved set includes the parent's variables with `origin` `parent`. A fork's variable of the same name as its parent's wins; deleting the fork's entry shows the parent's again (§11.10). When a fork's owner has left the organization, the fork's chain has no org step, and the parent step stays for the callers who can still reach the fork (§11.10).

### Secrets by name along the chain

Sandboxes resolve secrets by name, for credentials and for exposed secrets (§11.3, SBX-FR-024, SBX-FR-065). This package delivers the one function they call: given a workspace slug and a list of names, it answers, for each name, the value of the first scope in the chain that has the name (compared ignoring case) and that scope's `origin`, or that the name is missing, together with the scopes that were searched in order. Two rules narrow step 2 for secrets:

- Step 2 applies only when the fork's owner has write access to the parent workspace, which only the parent's owner has. The test is on the fork's owner, never on the caller. In a fork owned by a read-only org member the parent's secrets are never read.
- Reserved names (`GIT_PAT`, `GIT_USERNAME`, `GIT_PASSWORD`, `UPSTREAM_GIT_PAT`, `UPSTREAM_GIT_USERNAME`, `UPSTREAM_GIT_PASSWORD`, `FORGE_WEBHOOK_SECRET`, compared ignoring case) are skipped in step 2 for everyone.

Values are read at every call and never cached: a rotated secret is answered by the next call, and a fork owner who loses write access to the parent loses step 2 at the next call (§11.10). A secret that is found and cannot be read or decrypted fails the whole call with an error that names the key and the scope and never a value; it is never treated as missing and never replaced by a lower scope's value (SEC-FR-013).

### Removal with the owner

SEC-FR-014. Deleting a workspace deletes its variables in the same transaction; this includes the deletion and the expiry of a task fork. When that transaction is rolled back the variables are still there. A workspace created again under the same slug starts with no variables. No variable event is written for a removal. Deleting a user or an organization deletes that scope's variables in the transaction of the deletion; when the deletion is refused or rolled back the variables are still there.

### `parley vars`

§11.6. The commands follow the CLI conventions (PLAT-FR-090 to PLAT-FR-095).

- **Scope flags** `--user`, `--org <slug>`, `--workspace <slug>`. Without a flag the scope is the user. `list`, `update` and `delete` accept at most one; more is a usage error (exit 2).
- `parley vars create <KEY=VALUE[,KEY=VALUE…]>… | <KEY> --from-stdin` calls `POST /<scope>/vars` once for each scope flag, in the order user → org → workspace, and prints one document `{"results":[{"scope":"user"|"org"|"workspace","items":[…]}, …]}` holding the scopes that succeeded. A failing scope does not stop the others; each failed scope's error envelope is printed once on stderr and the command exits 1 if any scope failed.
- **Values.** `KEY=VALUE` is split at the first `=`, so a value may contain `=`. A comma separates entries. `--from-stdin` takes one bare `KEY` and reads the value from standard input, dropping one trailing line ending.
- `parley vars list` calls `GET /<scope>/vars` and prints the entries of all pages as one document `{"items":[…]}`, as `parley secrets list` does.
- `parley vars update <KEY=VALUE> | <KEY> --from-stdin` calls `PATCH /<scope>/vars/<KEY>` and prints the entry.
- `parley vars delete <KEY>` calls `DELETE /<scope>/vars/<KEY>` and prints `{"deleted":"<KEY>"}`.
- `parley vars resolve <workspace-slug>` calls `GET /workspaces/<slug>/vars/resolved`, takes no scope flag and prints the resolved entries with `origin`.
- Key naming rules are checked by the hub. Malformed arguments are usage errors (exit 2) and nothing is sent.

### End-to-end flows

1. **A developer sets values at two levels and reads what the workspace runs with.** The developer, a member of an organization and owner of a workspace in it, posts `LOG_LEVEL=info` to `/orgs/:slug/vars` and `log_level=debug` to `/workspaces/:slug/vars`. `parleyd` stores both and writes one `parley.variable.create` for each. The developer reads `/workspaces/:slug/vars/resolved`; `parleyd` answers one entry, key `log_level`, value `debug`, `origin` `workspace`. The developer deletes the workspace's entry and reads again; the entry is now `LOG_LEVEL`, `info`, `origin` `org`.
2. **An org member's fork runs with the parent's variables and without its secrets.** A developer owns a workspace that holds a variable `REGION` and a secret `DEPLOY_KEY`. An org member owns a task fork of it. The org member reads the fork's resolved variables; `parleyd` answers `REGION` with `origin` `parent`. `parleyd` resolves the secret `DEPLOY_KEY` for the fork and for a fork the developer owns: it is missing for the member's fork, with the parent's scope absent from the scopes searched, and found with `origin` `parent` for the developer's fork.
3. **A developer manages variables from the shell.** The developer runs `parley vars create A=1,B=x=y --user --workspace <slug>`; the parley CLI sends one create to `/user/vars` and one to `/workspaces/<slug>/vars`; `parleyd` stores the entries; the CLI prints both scopes' results and exits 0. The developer runs `parley vars resolve <slug>`; the CLI prints `A` and `B` with `origin` `workspace`.

## Design Decisions

1. **Profile: full**, with three execution paths.
2. **Go; SQLite through `modernc.org/sqlite` behind `internal/store`; `net/http` with its method-and-pattern router; `github.com/spf13/cobra` for the CLI; the standard `testing` package with table-driven tests and `net/http/httptest`** (project decisions 1 to 4 and 7). The variable table is created by a schema migration this package registers with `internal/store`.
3. **Variables live in a table of their own**, keyed by scope, scope id and the lower-cased key, beside the stored casing. Why: the two collections have the same shape but different rules for values, and uniqueness ignoring case is then enforced by the store even under concurrent creates.
4. **`secret_limit_reached` for a full variable scope.** The source PRD specifies `POST /user/vars` "as `POST /user/secrets`" and defines no variable-specific type.
5. **Membership means membership of an `active` organization**, on the org routes and in step 3 of the chain. This package asks the same question the access rule asks of identity: the ids of the active organizations a user is a member of. A member of a blocked organization therefore gets `404 not_found` on that organization's variable routes, and a workspace of a blocked organization resolves without the org step. The admin token reaches a blocked organization's variables. The PRD says only "currently a member".
6. **The organization in the path** is resolved with the organization selector of package 05 (`internal/identity`): a value in UUID form is an id, any other value a slug. This is what the secret routes of package 39 do, and the PRD specifies the variable routes "as the secret routes". The scope resolvers of the three scopes (the admin-token refusal on `/user/…`, the one `404` body for an unknown organization, a non-member and a member of a blocked organization) answer exactly as package 39's do; only the workspace need differs (`read` and `write` here, `owner` there).
7. **Body members of the wrong type are `400 invalid_request`**: `entries` that is not an array, an entry that is not an object, a `key` that is absent or not a string, a `value` that is not a string. The PRD names only the missing and the oversized cases.
8. **Update validates before it looks the key up**, as the platform's check order puts request validation before state: an invalid body on an unknown key is `400`.
9. **A key `resolved` can be created like any other valid key.** The PRD blocks no name. In a workspace scope only the exact path segment `resolved` is reserved; such a key is still addressed by another casing (`…/vars/RESOLVED`), because keys match ignoring case and the router matches the literal segment exactly.
10. **The resolved list is paged like every list** (`limit`, `cursor`, default 50), merged from one consistent read of the store on every request.
11. **A missing record ends a step, not the resolution.** A fork whose parent record is missing has no parent step; a workspace whose owner no longer exists has no user step; a workspace whose organization no longer exists has no org step. Why: the PRD is silent, and leaving a scope out can never hand a caller a value it should not get.
12. **A secret with an empty value counts as present** in name resolution. "An empty `GIT_PAT` counts as no credential" is a rule of credential resolution (package 10), not of the chain.
13. **A name asked for twice is answered once**, and names are answered in the order first asked. An empty list of names is answered with no entries and no error.
14. **The secret store is read through package 39's `Get(scope, scope id, name)`**, which answers `(value, found, error)`, matches the name ignoring ASCII case, reads the store on every call and returns an error matching `ErrUnreadable` for an entry that cannot be decrypted. The resolution takes that one method as its reader, so tests can wrap it to record reads; it defines no second read operation.
15. **Removal with a workspace or a fork.** `internal/workspace` imports `internal/secret` (package 10 stores credentials as secrets), so `internal/secret` cannot import the registries of packages 14 and 16; the registration is written in `internal/hub`. This package exports `RemoveWorkspaceVars(tx, slug)`, which deletes every variable of that workspace scope inside the transaction it is given, writes no event and returns the store's error. Package 14's delete runs it as a remover of owned data (a function of the transaction and one slug that returns the ids of the tokens it revoked; this one returns none), and package 16's fork removal runs it as a removal participant (a function of the transaction, the fork's slug and the cause; the cause is not looked at). Packages 14 and 16 are dependencies of this package for that reason, and this package's task writes both registrations. (Package 39 exports `RemoveWorkspace` in the same form; it cannot depend on 14, so package 14 registers that one, and package 16 removes a fork's secrets directly.)
16. **Variables of a deleted user or organization are removed in the deletion's transaction**, through the hook package 05 owns in `internal/identity` (`RegisterUserRemover`, `RegisterOrgRemover` in `internal/identity/removers.go`; this package only registers, as package 39 does): this package exports `RemoveVarScope(tx, scope, scope id)` and `internal/hub` registers it for the user and the org scope, beside package 39's `RemoveScope`. One mechanism serves secrets and variables; this package registers no maintenance task.
17. **`parley vars list` and `parley vars resolve` print one document `{"items":[…]}`**, without `next_cursor`, after following `next_cursor` to the end with `limit=200`: the shape and paging of `parley secrets list` (package 39). The PRD says "all pages" for `list` and names no paging flag for either.
18. **`parley vars create` always prints the `results` document**, for one scope too, and prints `{"results":[]}` when every scope failed. With no scope flag the one scope is `user`.
19. **Malformed arguments** are: an entry without `=` when `--from-stdin` is not given; an empty key before `=`; an empty entry between commas; with `--from-stdin`, any number of arguments other than one, or an argument containing `=` or `,`; for `update`, more than one entry; a scope flag on `resolve`. The line ending dropped from standard input is one `\n` or one `\r\n`.
20. **`parley vars delete` prints the key as typed**, not the stored casing, because the route answers `204` with no body.
21. **Time comes from a replaceable clock**, so tests control `created_at` and `updated_at`.

## Dependencies

- Package 39 `secrets` (draft): the secret store this package reads by scope and name through `Get`, and the directory `internal/secret/` both share.
- Package 05 `users_and_organizations` (draft): `RegisterUserRemover` and `RegisterOrgRemover` of `internal/identity/removers.go`, with which this package registers `RemoveVarScope`.
- Package 14 `workspace_archive_reclone_delete` (draft): the removers of owned data that the workspace delete runs, with which this package registers `RemoveWorkspaceVars`.
- Package 16 `task_fork_lifetime` (draft): the removal participants of a fork's removal, with which this package registers the same operation.
- Package 09 `cli_foundation` (draft): the CLI's HTTP client, output, error and exit-code rules that `parley vars` is built on.
- Package 52 `durable_event_log_and_stream` (draft): the event component that writes `parley.variable.*` in the transaction of the change.
- Through those: package 02 `http_api_conventions` (router, error envelope, list paging, idempotency), package 03 `server_lifecycle_and_configuration` (store, migrations), package 04 `authentication_and_login` (the authenticated principal), package 05 `users_and_organizations` (users, organizations, memberships), package 06 `api_keys_and_access_tokens` (the scopes `vars:read` and `vars:write` and the scope check) and package 07 `workspace_access_rule` (the workspace record, the access decision, the write-access question).
- Callers of the secret resolution function: packages 45 and 47.

## Verified External API

- `github.com/spf13/cobra` — **unverified** (not yet in `go.mod`): `cobra.Command` with `Use`, `Short`, `Args`, `RunE func(cmd *cobra.Command, args []string) error`; `(*Command).AddCommand(cmds ...*Command)`; `(*Command).Flags() *pflag.FlagSet`; `(*Command).InOrStdin() io.Reader`.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires.
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area. Not to be confused with the scope of an entry (`user`, `org`, `workspace`), which names its owner.
- **Bound token**: A PAT confined to one workspace (and the forks created with it).
- **Sandbox**: An isolated environment in which an agent runs, attached to a workspace (normally a task fork) and started with it checked out.
- **Exposed secret**: A secret that a profile names in `expose_secrets`. Its real value is placed in the sandbox's environment, where code in the sandbox can read it.
- **Credential (sandbox)**: A secret attached to a sandbox as an OpenShell provider. The workload sees only a placeholder.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
- **Variable**: A named, readable value owned by a user, an organization or a workspace.
- **Resolution chain**: The ordered scopes a workspace's variables and secrets are looked up in: the workspace, its parent when it is a fork, its organization, its owner.
- **Reserved name**: One of the secret names the hub itself reads from a workspace's own scope for origin, upstream and forge webhook credentials.
