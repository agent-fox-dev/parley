---
spec_id: "05"
spec_name: "users_and_organizations"
title: "Manage users and organizations"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "018f3e0b40b8fe35d38663555200f221cac900f4643f375f0682553450bbedec"
schema_version: 3
source: "docs/prd/03_platform.md"
---

# Manage users and organizations

## Intent

A hub serves a team, so it needs to know who its people are and how they are grouped. This package gives every user a profile they can read and edit, gives every new user a personal organization, and gives an operator the means to create, change, promote, demote, block and delete users and to create, change, block and delete organizations and their memberships, over the REST API and the `parley` command line. Organizations and memberships are what later decide which workspaces a person can read, and the admin role decides who may administer identity, so the hub must never be left without an active admin by a demotion or a deletion.

## Goals

- A user reads and edits their own profile with `GET /user` and `PATCH /user`, and lists the active organizations they belong to with `GET /user/orgs`.
- An admin creates a user without OAuth (`POST /users`); the user and a personal organization with the user as owner and first member are created in one transaction, or nothing is.
- An admin lists, reads, updates, promotes, demotes, blocks, unblocks and deletes users, naming a user by UUID, e-mail or username.
- Demoting or deleting the only active admin user is refused with `409 conflict`, atomically with the check.
- An admin creates, lists, updates, blocks, unblocks and deletes organizations, naming an organization by UUID or slug, and adds and removes members.
- An organization's members read the organization and its member list; everyone else gets the same `404 not_found` as for a missing organization.
- Every route has a `parley` command: `parley user show|update`, `parley orgs list|show|members`, `parley admin users …` (nine commands) and `parley admin orgs …` (nine commands).

## Non-goals

- Credentials, how each authenticates, the `401` answers, the `403` every credential of a blocked user gets, the `403 forbidden` the admin token gets on routes that need a user (PLAT-FR-021) as a mechanism, OAuth login, first-admin promotion at login, the personal-organization rule with its slug derivation (PLAT-FR-035) as a function, the tables for users, orgs and memberships, and `parley login`: package 04 authentication_and_login. This package only sets the status that package 04 reads, and marks its `/user` routes as needing a user.
- The error envelope, the order of checks, list paging and cursors, `Idempotency-Key` handling, body limits and rate limits: package 02 http_api_conventions. The routes here are registered through it and obey it.
- API keys and personal access tokens as resources: `GET /user/keys`, `/user/tokens…`, `GET /users/:id/keys`, `DELETE /users/:id/keys/:key_id`, `GET /users/:id/tokens`, `DELETE /users/:id/tokens/:token_id`, the scope registry and its implication rules, and `parley keys …`, `parley tokens …`, `parley admin keys …`, `parley admin tokens …`: package 06 api_keys_and_access_tokens. This package only declares which scope each of its routes needs.
- What a membership grants on a workspace, that a blocked organization's memberships grant no workspace access, and that workspaces of a deleted user or a deleted organization are kept: package 07 workspace_access_rule. This package never reads or writes a workspace record.
- The `403 token_bound` a workspace-bound token gets on `/user…`, `/users…` and `/orgs…`: package 08 workspace_bound_tokens.
- Client settings, the HTTP client, output and exit-code rules of the CLI (PLAT-FR-090 to PLAT-FR-095) and `parley help --json`: package 09 cli_foundation.
- Secrets and variables under `/user/…` and `/orgs/:id/…`: packages 39 secrets and 40 variables_and_resolution.
- Events: user, organization and membership operations emit no hub events (§3.8), so this package emits none.
- Paging flags on the list commands: the source PRD gives `parley orgs list`, `parley admin users list` and `parley admin orgs list` no `--limit` or `--cursor`; not covered by this package.

## Background

The repository holds no code yet; every file named here is new. The source is `docs/prd/03_platform.md`: PLAT-FR-035 to PLAT-FR-049, the entities and states of §3.3.3 and §3.3.4 for users, organizations and memberships, the `/user`, `/users…` and `/orgs…` routes of §3.5 and the `parley user`, `parley orgs`, `parley admin users` and `parley admin orgs` commands of §3.6.

Package 04 authentication_and_login comes first. It delivers, in `internal/identity/`, the stored user record that authentication reads (id, username, e-mail, role, status, provider identity), the credentials (admin token, API keys, PATs), and the authenticated caller every handler receives: the admin token, or a user with the kind of credential used. Its OAuth callback creates a user at first login together with the personal organization, through the personal-organization rule (PLAT-FR-035) that package 04 owns and exports as one function. Package 04 also creates the tables for users, orgs and memberships. This package calls that function from `POST /users` and owns the rest of the user and organization life cycle, in the same directory:

| Path | Holds |
|---|---|
| `internal/identity/users.go` | user reads, update, role and status changes, delete, the user selector |
| `internal/identity/orgs.go` | the organization and membership records, their operations, the organization selector |
| `internal/identity/admincheck.go` | the admin check |
| `internal/identity/removers.go` | `RegisterUserRemover` and `RegisterOrgRemover`, the removers of owned data that the two deletes call |
| `internal/identity/handlers_user.go`, `handlers_users.go`, `handlers_orgs.go` | the REST handlers and their route registration |
| `internal/cli/user.go`, `internal/cli/orgs.go`, `internal/cli/admin_users.go`, `internal/cli/admin_orgs.go` | the commands |
| `test/e2e/identity_test.go` | the end-to-end flow |

Tests live beside the code (`internal/identity/users_test.go`, `internal/identity/orgs_test.go`, `internal/identity/handlers_users_test.go`, …). Data is kept in the embedded transactional store behind `internal/store` (package 03).

## Requirements

All paths are relative to `/api/v1`. Every route obeys the conventions of §3.4: errors are the envelope `{"error":{"code","message","error_type"}}`, lists answer `{"items":[…],"next_cursor":…}` with `limit` (default 50, 1 to 200) and `cursor`, and checks run in the fixed order (authentication, scope, admin check, validation, state). They are not restated per route.

### Entities and shapes

A **user** has `id` (UUID), `username` (unique), `email` (unique), `full_name` (may be empty), `role` (`user` by default, or `admin`), `status` (`active` by default, or `blocked`), `provider` and `provider_id` (the pair is unique), `created_at` and `updated_at`. The `User` response object carries exactly these ten members.

An **organization** (org) has `id` (UUID), `name` (unique), `slug` (unique, immutable), `url` (free text, default empty), `owner_id` (a user id or null; informational; cleared when that user is deleted), `status` (`active` by default, or `blocked`), `created_at` and `updated_at`. The `Org` response object carries exactly these eight members; `owner_id` is null when unset.

A **membership** is the pair `(org_id, user_id)`, unique, with `created_at`. Deleting the org or the user deletes the membership.

States (§3.3.4): user role `user` ⇄ `admin` through promote and demote; user status `active` ⇄ `blocked`; org status `active` ⇄ `blocked`. Blocking a user revokes nothing. While an org is blocked it is hidden from `GET /user/orgs` and, by default, from `GET /orgs`.

### The caller's own profile

- `GET /user` (any user credential; a PAT needs `users:read`) answers `200` with the caller's `User`.
- `PATCH /user` (any user credential; a PAT needs `users:write`) takes `{"full_name": string}`; the member is required and `""` clears the name. It answers `200` with the updated `User`. A body without `full_name`, or with a value that is not a string, is `400 invalid_request` and changes nothing.
- `GET /user/orgs` (any user credential; a PAT needs `orgs:read`) answers `200` with a list of `Org`: the active orgs the caller is a member of, ordered by `name`. A blocked org is not listed.
- The admin token has no user: all three routes answer it `403 forbidden` (PLAT-FR-021).
- A PAT without the scope a route needs gets `403 insufficient_scope`. The routes that take a PAT are these three and the two org reads below (`orgs:read`).

### The admin check and selectors

"Admin" on the user and org routes means the admin token, or an API key whose user has role `admin`. PATs are never admin, including a PAT of an admin-role user. A failed admin check is `403 forbidden` (PLAT-FR-045). It is decided before the request is validated and before the named user or org is looked up, so a non-admin gets `403` for an existing and for a missing target alike (PLAT-FR-013).

Admin routes: `POST /users`, `GET /users`, `GET|PATCH|DELETE /users/:id`, `POST /users/:id/promote|demote|block|unblock`, `POST /orgs`, `GET /orgs`, `PATCH|DELETE /orgs/:id`, `POST /orgs/:id/block|unblock`, `PUT|DELETE /orgs/:id/members/:user_id`.

On user routes `:id` and `:user_id` accept the user's UUID, e-mail (a value containing `@`) or username. On org routes `:id` accepts the org's UUID or slug (PLAT-FR-040). A selector that matches nothing is `404 not_found`.

### Creating a user with a personal organization

`POST /users` (admin) creates a user without OAuth. Body: `username`, `email`, `provider`, `provider_id`, strings, all required and non-empty; `full_name`, string, optional, default empty. The user is created with role `user` and status `active`; there is no first-admin promotion, even when the e-mail equals the bootstrap admin e-mail. The answer is `201` with the `User`.

Every new user, from OAuth or `POST /users`, gets an org in the same transaction: `name` is the username, `owner_id` is the user, and the user is its first member (PLAT-FR-035). The rule is owned and built by package 04 authentication_and_login; `POST /users` calls it. It is repeated here for the reader. The slug is derived from the username:

1. lowercase; replace every character outside `[a-z0-9_-]` with `-`; collapse runs of `-`; trim leading and trailing `-` and `_`;
2. if fewer than 2 characters remain, use `u-<first 8 characters of the user id>`;
3. if it starts with a digit, prefix `u-`;
4. if the slug is taken, try `<slug>-1` … `<slug>-10`.

Examples: `Alice.Smith` → `alice-smith`; `42bob` → `u-42bob`.

Failures: a missing or empty required field, or a member of the wrong type, is `400 invalid_request`. A username, e-mail or provider identity that is taken, an org that already has the user's name, or no free slug after `<slug>-10`, is `409 conflict`. On every failure nothing is created: no user, no org, no membership.

### Reading and updating users

- `GET /users` (admin) lists `User` objects, newest first. By default only active users are listed; `include_blocked=true` adds blocked users.
- `GET /users/:id` (admin) answers `200` with the `User`, blocked or not; `404 not_found` when the selector matches no user.
- `PATCH /users/:id` (admin) takes `{"full_name": string}`, required, and answers `200` with the `User`. Errors: `400 invalid_request`, `404 not_found`.

### Role and status of a user

- `POST /users/:id/promote` answers `200` with the `User` with role `admin`; `POST /users/:id/demote` with role `user`; `POST /users/:id/block` with status `blocked`; `POST /users/:id/unblock` with status `active`. All four are admin routes and take no body.
- Each answers `200` with the unchanged object when the target is already in the requested state (PLAT-FR-047).
- Demoting a user who is the only `active` user with role `admin` is `409 conflict`. The check and the change are atomic: of two concurrent demotions that would together remove the last two active admins, exactly one succeeds (PLAT-FR-046). A blocked admin does not count as an active admin.
- Blocking has no such safeguard, because the admin token can always unblock: blocking the only active admin answers `200`.

### Deleting a user

`DELETE /users/:id` (admin) deletes the user, the user's API keys, PATs (bound ones included) and memberships, and clears `owner_id` on orgs the user owned, in one transaction (PLAT-FR-048). The orgs themselves, the personal org among them, are kept. The answer is `204`. Errors: `404 not_found`; `409 conflict` when the user is the only `active` user with role `admin` (PLAT-FR-046), in which case nothing is deleted.

Other packages keep data that belongs to a user (secrets, variables). They register a **user remover** with this package (`RegisterUserRemover`), a function of a transaction and a user id. The delete calls every registered remover inside its transaction, in registration order, before the user record is removed. A remover that fails rolls the whole delete back and the answer is `500 internal`.

### Creating, reading and updating organizations

- `POST /orgs` (admin). Body: `name`, string, required, non-empty after trimming; `slug`, string, required, 1 to 128 characters of `[a-z0-9_-]`, starting and ending with a letter or digit; `url`, string, optional; `owner_id`, string, optional, an existing user's id. It creates the org and no membership, and answers `201` with the `Org`. Errors: `400 invalid_request`; `409 conflict` when the name or the slug is taken.
- `GET /orgs` (admin) lists `Org` objects ordered by `name`. `include_blocked=true` adds blocked orgs.
- `GET /orgs/:id` (an admin, or a member of the org; a PAT needs `orgs:read`) answers `200` with the `Org`. A blocked org is returned to admins only. A missing org and a caller who is neither admin nor member get the same `404 not_found`.
- `PATCH /orgs/:id` (admin). Body: `name` (string, non-empty if present) and `url` (string); at least one is required. `slug` cannot be changed. It answers `200` with the `Org`. Errors: `400 invalid_request`; `404 not_found`; `409 conflict` when the name is taken.

### Blocking and deleting organizations

- `POST /orgs/:id/block` and `POST /orgs/:id/unblock` (admin, no body) answer `200` with the `Org` with status `blocked` or `active`, and with the unchanged object when the org is already in that state.
- `DELETE /orgs/:id` (admin) deletes the org and its memberships and answers `204` (PLAT-FR-049). Personal orgs can be deleted. The users who were members are kept. Error: `404 not_found`.
- Other packages register an **org remover** (`RegisterOrgRemover`), a function of a transaction and an org id. The delete of an org calls every registered remover inside its transaction, in registration order, before the org record is removed; a remover that fails rolls the whole delete back and the answer is `500 internal`.

### Membership

- `GET /orgs/:id/members` (an admin, or a member of the org; a PAT needs `orgs:read`) answers `200` with a list of `{"org_id","user_id","username","email","created_at"}` ordered by `username`; `created_at` is the membership time. Non-members get `404 not_found`.
- `PUT /orgs/:id/members/:user_id` (admin; `:user_id` is a user selector) answers `204`. It is idempotent: an existing membership is left as it is. Error: `404 not_found` for a missing org or user.
- `DELETE /orgs/:id/members/:user_id` (admin) answers `204`. Error: `404 not_found` for a missing org, user or membership.

### The commands

Each command calls one route with the caller's credential, prints the route's response body on stdout as one JSON document and exits 0; a command whose route answers `204` prints `{}`. A hub error is printed on stderr as received and the command exits 1; a missing required flag or a wrong number of arguments is a usage error, exit 2, and no request is sent (PLAT-FR-092, PLAT-FR-093). `<user>` is an id, username or e-mail; `<org>` is an id or slug; each is sent as one percent-encoded path segment (PLAT-FR-091).

| Command | Flags | Calls |
|---|---|---|
| `parley user show` | | `GET /user` |
| `parley user update` | `--full-name <string>`, required; an empty string clears it | `PATCH /user` |
| `parley orgs list` | | `GET /user/orgs` |
| `parley orgs show <org>` | | `GET /orgs/<org>` |
| `parley orgs members <org>` | | `GET /orgs/<org>/members` |
| `parley admin users list` | `--include-blocked` | `GET /users` |
| `parley admin users show <user>` | | `GET /users/<user>` |
| `parley admin users create` | `--username`, `--email`, `--provider`, `--provider-id` (all required); `--full-name` | `POST /users` |
| `parley admin users update <user>` | `--full-name <string>`, required | `PATCH /users/<user>` |
| `parley admin users delete <user>` | | `DELETE /users/<user>`; prints `{}` |
| `parley admin users promote\|demote\|block\|unblock <user>` | | `POST /users/<user>/promote\|demote\|block\|unblock` |
| `parley admin orgs list` | `--include-blocked` | `GET /orgs` |
| `parley admin orgs create` | `--name`, `--slug` (required); `--url`; `--owner <user id>` | `POST /orgs` |
| `parley admin orgs update <org>` | `--name`, `--url`; at least one is required (usage error otherwise) | `PATCH /orgs/<org>` |
| `parley admin orgs delete <org>` | | `DELETE /orgs/<org>`; prints `{}` |
| `parley admin orgs block\|unblock <org>` | | `POST /orgs/<org>/block\|unblock` |
| `parley admin orgs members list <org>` | | `GET /orgs/<org>/members` |
| `parley admin orgs members add <org> <user>` | | `PUT /orgs/<org>/members/<user>`; prints `{}` |
| `parley admin orgs members remove <org> <user>` | | `DELETE /orgs/<org>/members/<user>`; prints `{}` |

### End-to-end flows

**An operator sets up a team.** The operator, holding the admin token, runs `parley admin users create` twice; `parleyd` creates each user with a personal org. The operator runs `parley admin orgs create` and `parley admin orgs members add` for one of the users; `parleyd` stores the org and the membership. That user, an org member, lists their orgs with `GET /user/orgs` and reads the member list; the other user asks for the same org and gets `404 not_found`. The operator blocks the org; it disappears from the member's list. The operator deletes the member; the member's user is gone and the org remains without that membership.

**The last admin is protected.** The operator promotes a user. An attempt to demote or delete that user, the only active admin, is answered `409 conflict` and changes nothing. After a second user is promoted, the first can be demoted.

## Design Decisions

1. **Project decisions relied on.** Go; the store is embedded SQLite behind `internal/store` (project decision 2); HTTP uses `net/http` and the route registration and check order of package 02 (decision 3); the CLI uses `github.com/spf13/cobra` (decision 4); tests use `testing`, table-driven, with `net/http/httptest` (decision 7).
2. **The personal-organization rule is package 04's; this package calls it.** PLAT-FR-035 covers users from OAuth and from `POST /users`. Package 04 `authentication_and_login` owns the rule, because login needs it first: one exported function of `internal/identity` that takes an open transaction, the user's fields and the role, and creates the user, the personal org with its derived slug and the membership, or returns a conflict error. `POST /users` calls that function with role `user` and maps its conflict error to `409`. This package writes no slug function and no second form of the rule; its criteria state what a caller of `POST /users` observes. Why: two copies of a slug rule drift.
3. **Selector resolution is by shape, with no fallback.** A value in the hyphenated UUID form is looked up as an id only; otherwise a value containing `@` as an e-mail only; otherwise as a username. An org value in UUID form is an id, otherwise a slug. Usernames, e-mails and slugs are compared exactly, byte for byte. Why: the PRD gives the three forms and no precedence; shape decides without a second query.
4. **Uniqueness is exact.** `username`, `email`, org `name` and `slug` are unique as stored strings, case-sensitively. Why: the PRD says "unique" and nothing more.
5. **Org `name` is stored trimmed** of leading and trailing white space, on create and on update, since it is validated "non-empty after trimming". `url` and `full_name` are stored as given.
6. **`include_blocked`.** Only the exact value `true` adds blocked entries; any other value, like an absent parameter, lists active ones only. Why: the PRD names one value.
7. **Total orders for lists.** "Newest first" is `created_at` descending, then `id` descending. "Ordered by `name`" and "ordered by `username`" compare the stored strings byte-wise, ascending; both are unique within their list.
8. **`PATCH /orgs/:id` with `slug`.** A `slug` member is ignored like any unknown member (PLAT-FR-017); a body with `slug` and neither `name` nor `url` is `400 invalid_request`. A PATCH that sets the org's own current name is not a conflict.
9. **`owner_id` on `POST /orgs`** is a user's UUID, not a selector; a value that is not the id of an existing user is `400 invalid_request`, as the route's validation column says.
10. **A blocked org and its members.** `GET /orgs/:id/members` follows `GET /orgs/:id`: a blocked org is answered to admins only; a member gets `404 not_found`. Why: the PRD states the rule for the org read and is silent for the member list; one rule for both reads is simplest.
11. **An admin-role user reading an org with a PAT** is treated as a non-admin (PATs are never admin) and so needs a membership.
12. **Idempotent state changes leave `updated_at` alone.** "The unchanged object" (PLAT-FR-047) includes `updated_at`. A change of role, status, `full_name`, org `name` or `url` sets `updated_at` to the time of the change; clearing `owner_id` by a user deletion does too.
13. **Last-admin atomicity.** The count of active admins and the demotion or deletion run in one write transaction of the store, which admits one writer at a time.
14. **Membership removal of an org's owner** is allowed and leaves `owner_id` as it is; `owner_id` is informational.
15. **Scopes.** Each route that accepts a PAT declares its scope through package 02's route registration; whether a PAT holds it (with the implication rules) is decided by the scope check package 06 installs. This package's tests install a scope check that compares the declared scope with the PAT's permissions.
16. **CLI plumbing.** The commands are declared through the command helper of package 09 `cli_foundation` and use its shared client, result and error printers, exit-code mapping and path-segment encoding in `internal/cli`. This package depends on package 09 for that and writes no client, no printer and no exit-code code of its own.
17. **The list commands print one page.** They send no `limit` or `cursor`, because the PRD gives them no such flags.
18. **Profile: full**, with two execution paths.
19. **Identity tables are package 04's.** The tables for users, orgs and memberships, with every column and uniqueness rule this package needs, are created by package 04's schema step. This package adds no table and no column.
20. **File names.** The admin check lives in `internal/identity/admincheck.go` with `admincheck_test.go`, because package 03 `server_lifecycle_and_configuration` already uses `internal/identity/admin.go` and `admin_test.go` for the admin token. The test hub helper in `internal/identity/testhub_test.go` is package 04's; this package extends it. Routes that need a user carry package 04's needs-a-user mark, so the admin token is answered `403 forbidden` with package 04's message `this route needs a user`. The admin check is installed in the access position of package 02's pipeline, which holds a list of access checks; it applies to routes that carry this package's admin mark.
21. **Removers of owned data** (decided with packages 39 secrets and 40 variables_and_resolution). This package owns `internal/identity/removers.go` and its two registration functions, `RegisterUserRemover` and `RegisterOrgRemover`; each takes a function of a store transaction and an id that returns an error. The delete of a user calls the user removers and the delete of an org calls the org removers, inside the delete's transaction, in registration order, before the record is removed; an error rolls the delete back and the route answers `500 internal`. A user delete calls no org remover, because the user's orgs are kept. Why here: PLAT-FR-048 and PLAT-FR-049 make the delete one transaction, the packages that own scoped data are built later, and `internal/identity` must not import them. Packages 39 and 40 register their removal through `internal/hub` and add nothing to this file.

## Dependencies

- Package 04 authentication_and_login (draft): the stored user record and credentials in `internal/identity/`, authentication of the admin token, API keys and PATs, the authenticated caller handed to handlers, the refusal of blocked users and of the admin token on routes that need a user, the tables for users, orgs and memberships, and the exported personal-organization function that `POST /users` calls. Through it, packages 02 (route registration with its scope and mark declarations, error envelope, lists, check order and the access position) and 03 (the store) are available.
- Package 09 cli_foundation (draft): the command helper, the shared client, the result and error printers, the exit codes and the path-segment encoding that every command of this package is built on.

## Verified External API

`github.com/spf13/cobra` v1.10.2, read in the module cache (`command.go`, `args.go`, `shell_completions.go`):

- `func (c *Command) AddCommand(cmds ...*Command)`
- `func (c *Command) Flags() *flag.FlagSet` (`flag` is `github.com/spf13/pflag`)
- `func (c *Command) MarkFlagRequired(name string) error`
- `func ExactArgs(n int) PositionalArgs`
- `func NoArgs(cmd *Command, args []string) error`

`github.com/spf13/pflag` v1.0.10 (`flag.go`, `string.go`, `bool.go`):

- `func (f *FlagSet) Changed(name string) bool`
- `func (f *FlagSet) StringVar(p *string, name string, value string, usage string)`
- `func (f *FlagSet) BoolVar(p *bool, name string, value bool, usage string)`

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
- **Bound token**: A PAT confined to one workspace (and the forks created with it). A token bound to a fork also reaches its own changes, and the awareness and policy reads, on the fork's parent (§3).
- **Org**: An organization: a named group of users. Its memberships give its members read access to the org's workspaces.
- **Personal org**: The org created with a user, named after the user, with the user as owner and first member.
- **Admin**: On user and org routes, the admin token or an API key whose user has role `admin`.
- **User selector**: A user's UUID, e-mail or username in a path.
