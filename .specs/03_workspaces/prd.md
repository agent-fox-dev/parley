---
spec_id: "03"
spec_name: "workspaces"
title: "Register workspaces and decide who may read and change them"
status: "active"
created_at: "2026-10-27T18:14:09Z"
updated_at: "2026-10-27T21:39:19Z"
intent_hash: "055fea170330eecd79d37984fe7f7d0f83290a6436e903cf296e08ccd18574b7"
schema_version: 2
source: "docs/prd/01-port-af-hub-to-typescript-on-cloudflare.md"
---

# Register workspaces and decide who may read and change them

## Intent

A workspace is the unit everything else in Parley hangs from: a repository
the service holds, the settings that govern it, and the answer to who may do
what with it. That last part is the one worth getting exactly right, because
it is repeated on every route of every later package — three access levels, a
deliberate `404` where a `403` would leak that a workspace exists, and a
token that can be bound to one workspace and nothing else. Decided once and
in one place, it is a rule later packages apply; decided twice, it is a hole.

## Goals

- A workspace is created, read, listed, updated, archived, reactivated and
  deleted exactly as `docs/prd/api.md` § Workspace Endpoints specifies, with
  the full response schema and all twelve settings.
- The three access levels — read, write and private — are decided in one
  place for every route that names a workspace, with the member, stranger and
  admin-token answers of the § Workspace Access table.
- The anti-enumeration rule holds: the six workspace CRUD routes answer a PAT
  lacking the scope with `404`, every other route with `403`.
- A workspace-bound token behaves as the § Workspace-Bound Tokens table
  specifies, row by row.
- A lock serialises the operations that work on a workspace's repository, so
  that two never run at once and a conflicting request is `409
  workspace_busy` at once.
- Every sandbox route answers `404 sandboxes_disabled`, which is what a hub
  with no `[sandbox]` section does.

## Non-goals

- **The repository itself.** Creating a workspace writes the record and
  leaves `status` at `pending`; copying the repository in, checking the
  creation credentials against the remote, and moving `status` through
  `cloning` to `ready` or `failed` are `05_repository_store`, which completes
  this package's create route. See Design Decision 2.
- **Ending agent sessions on archive and delete.** `api.md` has both end the
  workspace's active sessions with one `hub.session.force_closed` each.
  Sessions do not exist until `11_sessions_and_cost`, which adds that to
  these routes.
- **Audit events.** `hub.workspace.create`, `.update`, `.archive`,
  `.reactivate`, `.delete` and `hub.token.bind` and `.bound_refused` are
  recorded by `12_audit_and_events`, which adds the recording to these
  routes. This package implements the behaviour the events describe.
- **The secrets and variables routes.** This package defines the secret store
  and uses it for the creation credentials; the three scopes, the key rules
  and the resolved view are `04_secrets_and_variables`. See Design Decision 1.
- **A real sandbox provider**, which is a later PRD entirely. This package
  serves the disabled answer the reference defines.

## Background

`01_service_foundation` provides the router, the envelope and the platform
interfaces; `02_identity_and_credentials` provides the credential identity,
the scope check and the organization membership lookup this package's access
decision reads on every request.

`api.md` § Workspace Access is the densest page of the reference and the one
with the most ways to be subtly wrong. Three of its rules are easy to miss.
The access check runs **after** a PAT's scope check and **before** the
route's status and precondition checks, so a member's write to an archived
workspace is `403` and not `400`. A failed access lookup is `500` and never a
grant. And the `404` a stranger gets is the same `404` a non-existent slug
gets, so no answer discloses whether a workspace exists.

The bound-token table is the other dense part: a token bound to `S` gets
`404` on another workspace's routes **before** any scope, existence or status
check, `403 token_bound` on routes it may not use at all, and two different
messages depending on whether the route is a hub route or an apikit one —
because an apikit route refuses the token before it is authenticated, so it
cannot name the workspace.

## Requirements

### Creating a workspace

- A valid request creates the workspace and answers `201` with the full
  workspace object, `status` `pending` and `access` `owner`.
- The slug is globally unique, 3 to 64 characters, starts with a lowercase
  letter, ends with a lowercase letter or digit, holds only lowercase
  letters, digits and hyphens, and has no consecutive hyphens.
- A missing or invalid `slug` or `git_url`, an over-long `display_name` or
  `description`, or an invalid combination of `workspace_mode`,
  `upstream_url` and `integration_branch` is `400`.
- `git_pat` together with `git_username` or `git_password`, only one half of
  the basic pair, an empty credential value, or any credential with a
  non-HTTPS `git_url` is `400`.
- An `org_id` naming no organization, and an omitted `org_id` for a user with
  no personal organization, are `400`; an owner who is not a member of the
  named organization is `403`.
- An admin token is `403`, because a workspace needs a real user as its
  owner. A PAT without `workspaces:write` is `403`. A slug that exists is
  `409`.
- The creation credentials are stored as the workspace secrets `GIT_PAT`, or
  `GIT_USERNAME` and `GIT_PASSWORD`, encrypted before they reach any store.

### Reading and listing workspaces

- `GET /api/v1/workspaces` returns the caller's own workspaces and those of
  their organizations, newest first and by slug where `created_at` is equal,
  with `include_archived` adding archived ones.
- `GET /api/v1/workspaces/:slug` returns one workspace.
- Every workspace object carries the full response schema, with `hub_url`
  null when the service's external URL is unset or the workspace has no
  organization, and `access` describing the caller.
- An admin token sees every workspace with `access` `admin`; a member sees
  their organizations' with `access` `member` and every other field as the
  owner does.

### Updating a workspace

- `PATCH` changes only the fields in the body: `display_name`,
  `description`, `org_id` and `settings`. A field given as `null` returns to
  its default, and `"settings": null` resets every setting.
- An empty body, an archived workspace, an over-long field, an immutable
  field, `sync_mode`, an unknown setting or a value a setting cannot take is
  `400`; an owner who is not a member of the named organization is `403`.

### Archiving, reactivating and deleting

- Archiving answers `200` with `status` `archived`. A workspace already
  archived is `400`; one that is `cloning`, or whose lock is held, is `409`.
- Reactivating an archived workspace answers `200` with `status` `pending`;
  one that is not archived is `409`.
- Deleting an archived workspace answers `204` and, in the same transaction,
  revokes every live bound token and removes every binding. A workspace that
  is not archived is `409`.

### Settings

- The twelve settings of `api.md` § Workspace Settings Reference exist with
  their documented defaults, and every workspace object carries the effective
  value of all of them, set or not.
- A setting given on create or update takes that value; given as `null` it
  returns to its default; not given, it is kept.
- An unknown setting key, or a value a setting cannot take, is `400` naming
  the key and the values it accepts.

### Deciding access

- Every route that names a workspace decides access in one place: an admin
  token has `admin`, the workspace's owner has `owner`, a member of its
  organization has `member`, and everyone else has none. Membership is read
  on every request, so a user removed from the organization loses access with
  the next request.
- A read-level route gives a member the owner's answer. A write-level route
  gives a member `403` with `error_type` `forbidden` and the message `org
  members have read-only access to this workspace`. A private-level route
  gives a member `404 workspace not found`, as a stranger gets.
- The checks run in this order: the credential, then a PAT's scope, then
  access, then the route's status and precondition checks. A member's write
  to an archived workspace is therefore `403` and not `400`.
- A failed access lookup is `500`, never a grant.

### Anti-enumeration

- `GET /api/v1/workspaces`, `GET`, `PATCH` and `DELETE
  /api/v1/workspaces/:slug`, and `POST .../archive` and `.../reactivate`
  answer a PAT that lacks the scope with `404 workspace not found`.
- Every other route, `POST /api/v1/workspaces` included, answers a missing
  scope with `403`.

### Workspace-bound tokens

- `POST /api/v1/workspaces/:slug/tokens` mints a PAT bound to the workspace,
  handled exactly as an unbound token creation and then bound. Only the
  owner may mint: an admin token is `403`, a member is `404`, a bound caller
  is `403 token_bound`, and an archived workspace is `400`. The response is
  `201` with `Cache-Control: no-store` and is the only one that carries the
  token.
- `GET /api/v1/workspaces/:slug/tokens` lists the workspace's bound tokens,
  revoked and expired ones included, ordered by creation time then token id,
  never carrying a token or a hash, and works on an archived workspace.
- A token bound to `S` reaching any route whose path names another workspace
  is `404 workspace not found`, before any scope, existence or status check.
- A bound token on a route it may not use at all is `403` with `error_type`
  `token_bound`: on a hub route the message names its workspace, on an apikit
  route it does not, because the token is refused before it is authenticated.
- Deleting the workspace revokes its live bound tokens and removes their
  bindings in one transaction. At start and every hour after, a sweep revokes
  the live tokens of bindings whose workspace no longer exists and removes
  bindings whose token no longer exists, while keeping bindings of revoked or
  expired tokens of a workspace that still exists.
- A binding that cannot be read is `500`; the token is never let through as
  unbound.

### The workspace lock

- The operations `api.md` § Workspace Lock names take the workspace's lock,
  and two of them never run at once for one workspace.
- A request that finds the lock held answers `409` with `error_type`
  `workspace_busy` at once, without waiting, and may be retried.
- The lock holds across every component of the deployment, not only within
  one of them, and is released if the component holding it stops.

### Sandbox routes with no provider

- Every route under `/api/v1/sandbox-profiles`,
  `/api/v1/workspaces/:slug/sandboxes` and `/api/v1/sandboxes` answers `404`
  with `error_type` `sandboxes_disabled` and the message `sandboxes are not
  enabled on this hub`, to every authenticated caller, before any scope check
  and before the workspace is looked up.
- An unauthenticated caller still gets `401`.

### End-to-end flows

1. **An owner registers a workspace and reads it back.** An API client posts
   a slug, a git URL and a `GIT_PAT` to `POST /api/v1/workspaces`; Parley
   validates the body, resolves the owner's organization, encrypts and stores
   the credential as a workspace secret, writes the record with `status`
   `pending` and answers `201`. The client then reads the workspace and the
   workspace list and finds it with `access` `owner` and every setting at its
   effective value.
2. **A member reads but cannot write.** A second user, a member of the
   workspace's organization, reads the workspace and sees the owner's answer
   with `access` `member`; their `PATCH` is refused `403 forbidden`; their
   read of the workspace's bound tokens is refused `404`.
3. **A bound token is confined.** The owner mints a token bound to the
   workspace; an API client reaches that workspace with it, is refused `404`
   on another workspace's route before any other check, and is refused `403
   token_bound` on `POST /api/v1/workspaces`.
4. **A workspace is retired.** The owner archives the workspace, a write is
   then refused, and the owner deletes it; the bound token minted earlier is
   revoked in the same transaction and is refused `401` from then on.

## Design Decisions

1. **This package defines the secret store and uses it; `04` builds its
   routes.** Creating a workspace stores `GIT_PAT` or the basic pair, so the
   storage facility and its encryption cannot wait for the package that
   serves the secrets routes. This package defines the interface — put, get
   and delete by scope and key, with values encrypted before storage — and
   `04_secrets_and_variables` adds the three scopes' routes, the key, size
   and count rules and the resolved view on top of it.
2. **Create writes the record and stops.** `api.md` has the create route
   check the given credentials against the repository before the workspace
   exists, and then start the clone. Both need the repository store, which is
   `05`. Splitting the route across two packages is the lesser evil: this
   package delivers a create that is correct in everything but the remote
   check, and `05` adds the check and the copy to the same route. The
   alternative — deferring the whole route to `05` — would leave this package
   with an access model and nothing to apply it to.
3. **The lock is a property of the workspace, not of a route.** Later
   packages add operations that take it; this package defines it, proves the
   mutual exclusion and the `409 workspace_busy` answer, and applies it to
   archive, the one operation here that touches the repository.
4. **Sandbox routes are registered, not omitted.** Leaving the paths
   unrouted would make them `404` with no `error_type`, which is a different
   answer from the one the reference defines for a hub without sandboxes, and
   `afc sandbox` distinguishes the two.

## Dependencies

- `01_service_foundation` for the router, the error envelope, the request
  conventions and the platform interfaces.
- `02_identity_and_credentials` for the credential identity, the scope
  registry and check, the token creation this package's bound minting reuses,
  and the organization membership lookup the access decision reads.
- `docs/prd/api.md` § Workspace Access, § Anti-Enumeration Policy,
  § Workspace-Bound Tokens, § Workspace Lock, § Workspace Response Schema,
  § Workspace Endpoints and § Workspace Settings Reference are normative.

## Verified External API

Nothing was verified against installed code. This package needs an
authenticated-encryption primitive for secret values at rest and a
distributed lock; neither has been chosen. The lock is expected to rest on
the per-workspace actor the platform provides, which is a platform interface
this package defines and `01`'s pattern governs, not an external library.
