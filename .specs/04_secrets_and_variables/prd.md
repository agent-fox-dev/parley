---
spec_id: "04"
spec_name: "secrets_and_variables"
title: "Store secrets and variables for users, organizations and workspaces"
status: "active"
created_at: "2026-10-27T18:22:14Z"
updated_at: "2026-10-27T22:18:53Z"
intent_hash: "474a2086118dae652aad7c56b20b1e1db22eb057d346237a642424e7ef98008c"
schema_version: 2
source: "docs/prd/01-port-af-hub-to-typescript-on-cloudflare.md"
---

# Store secrets and variables for users, organizations and workspaces

## Intent

Agents need configuration and credentials they did not bring with them: a
token for the fork, a model key, an endpoint that differs per environment.
Parley holds those in two kinds — secrets, whose values never come back out,
and variables, whose values do — each at three scopes that resolve from the
narrowest outwards. Keeping the two kinds genuinely different is the point:
a secret that can be read back is not a secret, and a variable that cannot
be read is useless.

## Goals

- The twelve secret routes and the thirteen variable routes behave as
  `docs/prd/api.md` § Secrets Endpoints and § Variables Endpoints specify,
  across the user, organization and workspace scopes.
- No route, log record or audit event ever returns a secret's value, and a
  dump of the underlying store yields no plaintext secret.
- `GET /api/v1/workspaces/:slug/vars/resolved` merges the three tiers with
  the `origin` field and the owner-centred rule the reference states.
- `afc secrets` and `afc vars`, including their multi-scope `create` and
  `--from-stdin`, behave as `docs/prd/cli.md` specifies.

## Non-goals

- **Interpreting the reserved names.** `GIT_PAT`, `UPSTREAM_GIT_PAT`,
  `FORGE_WEBHOOK_SECRET` and the rest are ordinary secrets to these routes;
  `api.md` is explicit that the hub interprets them only where it uses them.
  Resolving a workspace's git credential from them belongs to
  `05_repository_store`, and the webhook secret to `14_forge_webhooks`. See
  Design Decision 2.
- **Audit events.** `hub.secret.*` and `hub.variable.*` are recorded by
  `12_audit_and_events`, which adds the recording to these routes.
- **The secret store itself.** `03_workspaces` defined the platform
  interface and its encryption to hold the creation credentials; this
  package builds the routes, the scopes and the rules on top of it.

## Background

`03_workspaces` already stores secrets: creating a workspace writes
`GIT_PAT`, or `GIT_USERNAME` and `GIT_PASSWORD`, through the secret store
interface, encrypted. What it did not build is the three scopes, the key and
size rules, the per-scope entry cap, or any route. This package adds those,
and adds variables, which share every rule except that their values are
returned.

Two asymmetries in `api.md` are easy to flatten by accident and must not be.
**Workspace secrets need private access** — the owner and an admin token,
with a member of the organization getting `404` as a stranger does — while
**workspace variables need read access to read and write access to change**,
so a member reads them and gets `403 forbidden` on a change. And the
**resolved view is owner-centred**: the user tier is the workspace *owner's*
variables, not the caller's, and the organization tier applies only when the
owner is a member of the workspace's organization. A member of that
organization therefore gets exactly the same answer as the owner, which is
the opposite of what a caller-centred implementation would produce.

## Requirements

### Rules shared by both kinds and all scopes

- A key holds letters, digits and underscores, does not start with a digit,
  and is at most 255 characters.
- Keys match case-insensitively, both when looking one up and when checking
  that it is unique within its scope.
- A value is at most 262,144 bytes.
- A scope — one user, one organization or one workspace — holds at most 100
  entries, and a create that would exceed that is refused.
- Every list is sorted by key, ignoring case.

### Secrets

- The four routes at each of the three scopes create one or more entries,
  list them, replace one value and delete one. A response carries the key
  and the timestamps and never the value.
- User secrets belong to the caller; an admin token has no user and is
  refused. Organization secrets are reached by slug, by members of the
  organization and admin tokens. Workspace secrets need private access.
- A create whose key already exists in the scope, ignoring case, is `409`. A
  replace or delete naming no key is `404`. A replace with no `value` is
  `400`. An organization that does not exist, or one the caller does not
  belong to, is `404`; a workspace the caller may not reach privately is
  `404`.

### Variables

- The same four routes at the same three scopes, plus the resolved view,
  with the same key, size, count and ordering rules — but every response
  carries the value.
- Workspace variables need read access to read and write access to change,
  so a member of the organization lists them and is refused `403 forbidden`
  on a create, replace or delete.
- A variable whose key happens to match a workspace setting is only a
  variable: no behaviour of the service reads it.

### The resolved view

- `GET /api/v1/workspaces/:slug/vars/resolved` merges three tiers, a
  workspace value overriding an organization value, which overrides a user
  value, and returns each entry with an `origin` of `user`, `org` or
  `workspace`.
- The user tier is the workspace **owner's** variables, never the caller's,
  and the organization tier is used only when the owner is a member of the
  workspace's organization. A member of that organization therefore gets the
  same answer as the owner.
- The result is sorted by key, ignoring case.

### Keeping secrets secret

- No route returns a secret's value, including the route that just set it.
- A value is encrypted before it reaches any store, so a dump of the store
  yields no plaintext.
- No log record carries a secret's value.

### End-to-end flows

1. **An owner sets a workspace secret.** An API client creates
   `ANTHROPIC_API_KEY` in a workspace's scope; Parley validates the key,
   checks the scope's entry count, encrypts the value and stores it, and
   answers with the key and its timestamps. A later list shows the key and
   no value, and the stored bytes contain no plaintext.
2. **Variables resolve across three tiers.** The workspace's owner sets
   `REGION` at user scope, the owner's organization sets it at organization
   scope, and the workspace sets it too; a client reads the resolved view and
   finds one entry with the workspace's value and `origin` `workspace`, while
   a key set only at user scope comes back with `origin` `user`.
3. **A member reads variables but cannot change them.** A member of the
   workspace's organization lists its variables and is served; their create
   is refused `403 forbidden`; their attempt to list the workspace's secrets
   is refused `404`.

## Design Decisions

1. **One storage path, two kinds.** Secrets and variables share the key,
   size, count and ordering rules and differ only in whether the value is
   returned and in the workspace scope's access level. They are built as one
   store with two projections rather than two parallel implementations, so a
   rule cannot drift between them.
2. **The reserved names mean nothing here.** `api.md` says the hub reads a
   fixed set of workspace secret names for its own git and forge operations,
   and that every other name is an ordinary secret. These routes therefore
   treat all names alike; the packages that use a reserved name resolve it,
   which keeps the resolution next to its only caller.
3. **The resolved view is computed from the owner, and that is tested
   explicitly.** A caller-centred implementation passes every test written
   from the owner's point of view and fails only for a member — so the member
   case is a requirement of its own rather than an edge case.

## Dependencies

- `01_service_foundation` for the router, the error envelope and the
  platform interfaces.
- `02_identity_and_credentials` for the credential identity, the scope check
  and the organization membership lookup.
- `03_workspaces` for the workspace record, the access decision this package
  applies at two different levels, and the secret store interface with its
  encryption.
- `docs/prd/api.md` § Secrets Endpoints and § Variables Endpoints are
  normative, and `docs/prd/cli.md` § Secrets Commands and § Variables
  Commands for what `afc` expects.

## Verified External API

Nothing was verified against installed code. The authenticated-encryption
primitive this package relies on is chosen by `03_workspaces`, which defines
the secret store; no new external dependency is expected here.
