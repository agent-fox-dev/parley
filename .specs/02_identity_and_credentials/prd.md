---
spec_id: "02"
spec_name: "identity_and_credentials"
title: "Authenticate callers and manage users, keys, tokens and organizations"
status: "active"
created_at: "2026-10-27T18:08:22Z"
updated_at: "2026-10-27T21:19:22Z"
intent_hash: "5c12b2ab9b6760c4fa001e303703d41b6a7ceab230b206a7d5d5d9ce3a5d2646"
schema_version: 2
source: "docs/prd/01-port-af-hub-to-typescript-on-cloudflare.md"
---

# Authenticate callers and manage users, keys, tokens and organizations

## Intent

Everything else Parley does is gated on knowing who is calling and what they
are allowed to do. The hub answers that with three credential kinds, a flat
set of scopes and an OAuth sign-in that is the only way to get an API key,
and every agent, script and CI job in existence already holds credentials of
exactly those shapes. Reproducing that identity layer faithfully — the string
formats, the scope names, the refusals and the surprising rule that logging
in revokes your other keys — is what lets an existing `afc` installation
point at Parley and keep working.

## Goals

- A caller presenting an API key, a personal access token or an admin token
  is identified, and one presenting nothing or something invalid is refused
  `401`, as `docs/prd/api.md` § Authentication specifies.
- The 24 hub scopes and apikit's 8 are registered, `write` implies `read`,
  and a PAT lacking a route's scope is refused `403`.
- `afc login` against Parley completes: the provider list, the code exchange,
  the new API key, and the revocation of the user's older keys.
- `afc user`, `afc keys`, `afc tokens`, `afc orgs` and `afc admin` each
  produce the stdout documents and exit codes `docs/prd/cli.md` specifies.
- No response, log line or audit event carries a credential secret.

## Non-goals

- **Workspace access.** Deciding whether a caller is a workspace's owner, a
  member of its organization or a stranger, the anti-enumeration rule and
  workspace-bound tokens all belong to `03_workspaces`, which builds them on
  the credential identity this package establishes.
- **Audit events for identity changes.** `api.md` records user and credential
  changes as hub events visible to admin tokens; the event recorder is
  `12_audit_and_events`, which adds them then.
- **The organization secrets and variables routes**, which take an
  organization slug and belong to `04_secrets_and_variables`. This package
  serves apikit's organization routes, which take an id.
- **A web session.** Sign-in from a browser is `15_web_ui`; this package
  serves the API's OAuth flow only.

## Background

Package `01_service_foundation` provides the router, the error envelope, the
request conventions and the platform interfaces. This package is the first to
read and write persistent records, so it is where the relational store stops
being a readiness probe and starts holding data.

The hub's identity layer is the Go library `apikit`. There is no TypeScript
equivalent, so these routes are implemented from `api.md` § Identity and
Access Endpoints rather than wrapped (decision D5 of the source PRD). Two
details of that reference are load-bearing and easy to lose. The credential
strings have fixed shapes — `af_<key_id>_<secret>` for an API key,
`af_pat_<token_id>_<secret>` for a PAT — which `afc` and existing automation
parse. And `POST /api/v1/auth/callback` **revokes every existing API key of
the user** before issuing a new one, so a person who signs in on a second
machine silently stops their own automation; the hub documents this and
Parley must reproduce it rather than quietly improve it.

`api.md` § Permission Scopes also names eight scope names from earlier hub
releases that must be refused when a new PAT asks for them.

## Requirements

### Authenticating a caller

- Every route except the public ones needs `Authorization: Bearer
  <credential>`. The public ones are the four server routes,
  `GET /api/v1/auth/providers`, `POST /api/v1/auth/callback` and the forge
  webhook ingress.
- A request with no credential, or one the service does not accept — unknown,
  malformed, revoked or expired — is `401 authentication required`.
- An accepted credential is identified as an API key, a personal access token
  or an admin token, and the acting user and the credential kind are
  available to every route that runs afterwards.
- A credential whose user is blocked is refused, without the credential being
  revoked; unblocking the user makes it work again.
- No credential secret, and no `Authorization` header, reaches any log record
  or response body.

### Scopes

- API keys and admin tokens hold every hub scope implicitly. Only personal
  access tokens are checked against the scopes they were granted.
- For every hub resource, the `write` scope implies the `read` scope; `read`
  never implies `write`.
- A PAT that lacks the scope a route needs is refused `403` with no
  `error_type`.
- The scope names of earlier releases — `workspaces:create`,
  `workspaces:delete`, `workspaces:sync`, `secrets:list`, `secrets:manage`,
  `secrets:delete`, `vars:manage` and `vars:delete` — are refused when a new
  token asks for them.

### Signing in

- `GET /api/v1/auth/providers` is public, cached publicly for five minutes,
  and lists each configured provider with its authorize URL.
- `POST /api/v1/auth/callback` exchanges a provider's authorization code: it
  creates or updates the user, revokes **every** existing API key of that
  user, issues a new one and returns the user and the key together. The key
  is returned here and nowhere else. A new user also gets a personal
  organization.
- An invalid or expired code is `401`, a `redirect_uri` outside the
  provider's allowlist is `400`, a blocked user is `403`, and a conflicting
  identity is `409`. None of them creates a user or issues a key.
- `expires` is 0, 30, 60 or 90 days, defaulting to 90; any other value is
  `400`.

### The caller's own profile, keys and tokens

- `GET /api/v1/user` returns the caller's profile; `PATCH /api/v1/user`
  changes `full_name`, the only mutable field.
- `GET /api/v1/user/keys` lists the caller's API keys as metadata only — never
  a secret or its hash — and supports conditional `GET` with `ETag`.
- `POST /api/v1/user/keys/:key_id/refresh` rotates a key's secret, keeping
  `key_id` and recomputing the expiry. Only an API key may call it. A revoked
  or expired key is `400`.
- `DELETE /api/v1/user/keys/:key_id` revokes a key; a key may revoke itself
  and the request still completes.
- Personal access tokens are listed, created, read and revoked. Creation
  returns the token once; revocation answers `200` with the token's metadata,
  not `204`, and a token already revoked is `400`.
- A PAT caller may grant only scopes it holds; an API key may grant any
  registered scope.
- `PUT`, `PATCH` and `DELETE` on a token's permissions replace, add to and
  remove from its set. `PUT` with an empty array removes every permission and
  revokes the token; `PATCH` and `DELETE` with an empty array are `400`.

### Organizations

- `GET /api/v1/user/orgs` lists the organizations the caller belongs to.
- `GET /api/v1/orgs/:id` reads one by **id**, for a member or an admin, with
  `ETag` support. A non-member is `403` and an unknown id is `404`.

### Administration

- Every administration route needs an admin token or an API key of a user
  with the admin role: `401` without a credential, `403` for anyone else.
- Users can be created, listed, read, updated, promoted, demoted, blocked and
  unblocked, and any user's keys and tokens can be listed and revoked.
- Organizations can be created, listed, updated, deleted, blocked and
  unblocked, and their members listed, added and removed.
- Removing the last active admin, by deletion or demotion, is refused `409`.

### End-to-end flows

1. **A developer signs in and gets a key.** The developer runs `afc login`;
   Parley lists its providers, the developer authorises with the provider,
   `afc` posts the code to the callback, Parley creates the user and a
   personal organization, revokes the user's older API keys, issues a new one
   and returns it; `afc` stores it and prints the user.
2. **A scoped token is minted and refused.** The developer creates a PAT with
   `workspaces:read` alone; an API client then calls a route needing
   `tokens:manage` with it and is refused `403`, while a route needing
   `workspaces:read` admits it.
3. **An administrator blocks a user.** An administrator calls the block
   route; the blocked user's API client, holding a key that was never
   revoked, is refused on its next request; after the unblock the same key
   works again.

## Design Decisions

1. **Credential secrets are stored only as hashes**, with the prefix and
   identifier kept in clear so a credential can be found without a table
   scan. `api.md` requires that no response carries a secret; storing only a
   hash is what makes that true even of the database.
2. **A blocked user is refused `401`, not `403`.** `api.md` says a blocked
   user's keys "stop working without being revoked" and that blocked users
   are refused by the API and the git server, without giving a status for an
   ordinary request; `403` at login is the one case it does state. `401`
   keeps the two consistent: an unusable credential is an authentication
   failure, and it tells an attacker nothing about why.
3. **The scope registry is data, not code paths.** The 24 hub scopes and
   apikit's 8 are declared in one place with their implications, so that
   later packages name a scope rather than re-deriving the rule, and the
   retired names are refused from the same table.

## Dependencies

- `01_service_foundation` for the router, the error envelope, the request
  conventions and the platform interfaces — in particular the relational
  store, which this package is the first to use for real data.
- `docs/prd/api.md` § Authentication, § Permission Scopes, § Implied
  Permissions and § Identity and Access Endpoints are normative, and
  `docs/prd/cli.md` § Account and Admin Commands for what `afc` expects.
- An OAuth provider, GitHub by default, at deployment time.

## Verified External API

Nothing was verified against installed code: the repository has no
dependencies yet. This package will need a password-grade hash for
credential secrets and an OAuth client; neither has been chosen and no
signature has been read. Each must be verified against its installed types
when it is adopted, and the hash must be one whose verification runs inside
the platform's per-request CPU budget — a constraint worth measuring before
the choice is fixed.
