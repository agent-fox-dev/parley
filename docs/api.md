# Parley REST API Reference

This is the reference of Parley's HTTP API: every route a client can call,
what it takes, what it answers and how it fails. It is for people writing
clients of Parley and for operators checking what a call does.

Parley is a port of `af-hub` and keeps its contract. Where this document is
silent, [the hub's API reference](prd/api.md) is the specification being
implemented; routes appear here as they are built.

## Contents

- [Conventions](#conventions)
- [Authentication](#authentication)
- [Permission Scopes](#permission-scopes)
- [Error Response Schema](#error-response-schema)
- [Signing in](#signing-in)
- [The caller's profile](#the-callers-profile)
- [API keys](#api-keys)
- [Personal access tokens](#personal-access-tokens)
- [Organizations](#organizations)
- [Administration](#administration)
- [Workspace access](#workspace-access)
- [Workspaces](#workspaces)
- [Workspace-bound tokens](#workspace-bound-tokens)
- [Sandboxes](#sandboxes)
- [Server Endpoints](#server-endpoints)

## Conventions

- **Base URL.** Versioned routes live under the mount point, `/api/v1` by
  default (`PARLEY_MOUNT_POINT`; see [configuration](configuration.md)). The
  four [server endpoints](#server-endpoints) are served outside it, at the
  server root, whatever the mount point is configured to.
- **Unmatched paths.** A request for a path that matches no route is
  answered `404` in the error envelope, whether or not it is under the mount
  point.
- **Methods.** A server endpoint requested with a method other than `GET` is
  answered `405` in the error envelope, and its handler does not run.
- **Bodies** are JSON. A request body larger than the configured maximum,
  1,048,576 bytes by default, is answered `413` before any route handler
  runs.
- **Timestamps** are RFC 3339, rendered in UTC and always ending in `Z`. A
  timestamp a client sends may carry any offset and is converted to UTC,
  preserving the instant; one that is not valid RFC 3339 is answered `400`
  with a message naming the field.

## Authentication

Every route except the public ones needs a credential in the
`Authorization: Bearer <credential>` header. The public routes are the four
[server endpoints](#server-endpoints), `GET /api/v1/auth/providers`,
`POST /api/v1/auth/callback` and the forge webhook ingress under
`/hooks/forge/`.

Three kinds of credential are accepted:

| Kind                  | Shape                             | Identity               | Scopes                    |
| --------------------- | --------------------------------- | ---------------------- | ------------------------- |
| API key               | `af_<key_id>_<secret>`            | the user it belongs to | every scope, implicitly   |
| Personal access token | `af_pat_<token_id>_<secret>`      | the user it belongs to | only those it was granted |
| Admin token           | the deployment's configured value | none — it owns nothing | every scope, implicitly   |

A request that carries no credential, or one the service does not accept, is
answered:

```json
{ "error": { "code": 401, "message": "authentication required" } }
```

The same answer is given whether the credential was absent, malformed,
unknown, revoked, expired, or belongs to a blocked user. A caller learns
nothing from it about which applied.

Blocking a user stops every credential of theirs working **without revoking
any of them**: the block is read on each request, so unblocking makes the
same credentials work again.

No credential, and no `Authorization` header, is written to a log record or
a response body. Secrets are stored only as a hash
([ADR 3](adr/03-hash-credential-secrets-with-one-pass-of-sha-256.md)).

## Permission Scopes

Every hub resource has one `read` and one `write` scope, and the identity
routes use their own. API keys and admin tokens hold them all; only a
personal access token is checked.

| Resource scopes                      |                                    |
| ------------------------------------ | ---------------------------------- |
| `workspaces:read` `workspaces:write` | `patches:read` `patches:write`     |
| `rebuilds:read` `rebuilds:write`     | `merges:read` `merges:write`       |
| `git:read` `git:write`               | `secrets:read` `secrets:write`     |
| `vars:read` `vars:write`             | `sessions:read` `sessions:write`   |
| `audit:read` `audit:write`           | `sandboxes:read` `sandboxes:write` |
| `forge:read` `forge:write`           |                                    |

The identity scopes are `users:read`, `keys:read`, `keys:manage`,
`tokens:read`, `tokens:write`, `tokens:manage`, `orgs:read` and
`orgs:write`.

`<resource>:write` grants `<resource>:read`. Nothing else is implied: a read
scope never reaches a write route, and `tokens:manage` does not grant
`tokens:read`.

A token that does not hold the scope a route needs is answered `403` in the
error envelope, with **no** `error_type`, and the route's handler does not
run.

These names of earlier hub releases are refused when a token asks for them:
`workspaces:create`, `workspaces:delete`, `workspaces:sync`, `secrets:list`,
`secrets:manage`, `secrets:delete`, `vars:manage` and `vars:delete`. So is
any name outside the registered set; either is answered `400` naming the
scope.

## Error Response Schema

Every error is answered as JSON in one envelope:

```json
{
  "error": {
    "code": 409,
    "message": "a rebuild job is already queued or running for this workspace",
    "error_type": "concurrent_rebuild"
  }
}
```

`code` repeats the HTTP status and `message` describes the error for a
person. `error_type` is a machine-readable class, present only where the
contract lists one for that condition and **absent** — not null — otherwise.

An unhandled failure is answered `500` with the message exactly
`internal server error`. No part of the underlying failure reaches the
response body; it goes to the log.

## Signing in

OAuth is the only source of an API key; there is no password login. Both
routes are public.

### GET /api/v1/auth/providers

The OAuth providers this deployment is configured with, and where to send a
person to authorise. Cached publicly (`Cache-Control: public, max-age=300`).

**Response:** `200 OK`

```json
[{ "name": "github", "authorize_url": "https://github.com/login/oauth/authorize?client_id=..." }]
```

A deployment that configured no provider answers `200` with an empty array:
sign-in is unavailable, every other route still works.

### POST /api/v1/auth/callback

Exchange a provider's authorization code for the user and a new API key.

**Request:**

| Field          | Required | Type    | Description                                                         |
| -------------- | -------- | ------- | ------------------------------------------------------------------- |
| `provider`     | yes      | string  | A provider from the list above                                      |
| `code`         | yes      | string  | The provider's authorization code                                   |
| `redirect_uri` | no       | string  | Must be on the provider's allowlist, where it has one               |
| `expires`      | no       | integer | Key lifetime in days: `0` (never), `30`, `60` or `90`. Default `90` |

**Behaviour.** The user is created or found, **every** API key that user
already holds is revoked, and one new key is issued — the revocation and the
issue in a single transaction, so a failure leaves neither half done.
Automation holding an older key of the same user stops working after a
sign-in; this is the hub's behaviour and Parley reproduces it. A user
signing in for the first time also gets a personal organization, owned by
them.

**Response:** `200 OK`. **The key is returned here and by no other route.**

```json
{
  "user": {
    "id": "uuid-string",
    "username": "alice",
    "email": "alice@example.com",
    "full_name": "Alice Example",
    "status": "active",
    "role": "user",
    "provider": "github",
    "provider_id": "12345",
    "created_at": "2026-03-01T12:00:00Z",
    "updated_at": "2026-03-01T12:00:00Z"
  },
  "api_key": {
    "key": "af_<key_id>_<secret>",
    "key_id": "<key_id>",
    "expires_at": "2026-05-30T12:00:00Z"
  }
}
```

`expires_at` is `null` for a key issued with `expires` of `0`.

**Errors.** None of them creates a user, issues a key or revokes one.

| Status | Condition                                                                                                                                                                 |
| ------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 400    | A body that is not a JSON object; `provider` or `code` missing; an unconfigured provider; a `redirect_uri` outside the allowlist; an `expires` other than 0, 30, 60 or 90 |
| 401    | The authorization code is invalid or expired                                                                                                                              |
| 403    | The user is blocked                                                                                                                                                       |
| 409    | The username belongs to another provider identity                                                                                                                         |

## The caller's profile

### GET /api/v1/user

The profile of the user behind the credential.

**Access:** `users:read`.

**Response:** `200 OK` with `id`, `username`, `email`, `full_name`,
`status`, `role`, `provider`, `provider_id`, `created_at` and `updated_at`.

**Errors:** `404` when the credential has no user of its own, which is the
case for an admin token.

### PATCH /api/v1/user

Change `full_name`, the only mutable field.

**Access:** `users:read`. **Request:** `{"full_name": "Alice Example"}`.

**Response:** `200 OK` with the updated profile.

**Errors:** `400` for a body that is not a JSON object, or a `full_name`
that is absent, null, empty or not a string; the stored profile is
unchanged.

## API keys

A key is created only by [signing in](#post-apiv1authcallback); these
routes list, rotate and revoke them.

### GET /api/v1/user/keys

**Access:** `keys:read`.

**Response:** `200 OK` with one entry per key — `key_id`, `created_at`,
`expires_at`, `revoked_at` — and an `ETag`. Revoked and expired keys are
listed too. **No entry carries a secret or its hash.** A repeat request
whose `If-None-Match` still matches is answered `304` with no body.

### POST /api/v1/user/keys/:key_id/refresh

Issue a new secret under the same `key_id`. The expiry is computed again
from the span the key was originally issued for, counted from now.

**Access:** an API key only.

**Response:** `200 OK` with `key`, `key_id` and `expires_at`. The previous
secret is refused from the next request on.

| Status | Condition                              |
| ------ | -------------------------------------- |
| 400    | The key is revoked or has expired      |
| 401    | The caller is not an API key           |
| 404    | No such key, or it is not the caller's |

### DELETE /api/v1/user/keys/:key_id

Revoke a key. A key may revoke itself; the request still completes, and the
next request made with it is refused `401`.

**Access:** `keys:manage`. **Response:** `204 No Content`.

**Errors:** `404` when no such key exists or it belongs to another user.
The two are answered identically, so a caller cannot learn which
identifiers exist.

## Personal access tokens

A token belongs to a user and is limited to the scopes it was granted.

### GET /api/v1/user/tokens

**Access:** `tokens:read`.

**Response:** `200 OK` with one entry per token — `token_id`, `name`,
`permissions`, `expires_at`, `created_at`, `revoked_at`. No entry carries
the token string or its hash.

### POST /api/v1/user/tokens

**Access:** `tokens:manage`. A token may grant only scopes it holds itself;
an API key may grant any registered scope.

**Request:**

| Field         | Required | Type     | Description                                                     |
| ------------- | -------- | -------- | --------------------------------------------------------------- |
| `name`        | yes      | string   | Label, at most 255 characters                                   |
| `permissions` | yes      | string[] | [Scopes](#permission-scopes) to grant, at least one             |
| `expires`     | no       | integer  | Lifetime in days: `0` (never), `30`, `60` or `90`. Default `90` |

**Response:** `201 Created` with the metadata and `token`, in the form
`af_pat_<token_id>_<secret>`. **The token is returned here and nowhere
else.**

| Status | Condition                                                                                                                                                             |
| ------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 400    | A body that is not an object; `name` absent, empty or too long; `permissions` absent, empty, not an array, or naming a retired or unregistered scope; a bad `expires` |
| 403    | A token granting a scope it does not hold                                                                                                                             |

### GET /api/v1/user/tokens/:token_id

**Access:** `tokens:read`. **Response:** `200 OK` with the metadata.
`404` when no such token exists or it is not the caller's.

### DELETE /api/v1/user/tokens/:token_id

**Access:** `tokens:manage`.

**Response:** `200 OK` with the token's metadata and `revoked_at` set —
**not** `204`. The token is refused `401` from the next request on.

| Status | Condition                                |
| ------ | ---------------------------------------- |
| 400    | The token is already revoked             |
| 404    | No such token, or it is not the caller's |

### PUT / PATCH / DELETE /api/v1/user/tokens/:token_id/permissions

Change a token's scopes: `PUT` replaces the set, `PATCH` adds to it,
`DELETE` removes from it.

**Access:** `tokens:write`. A token may add only scopes it holds itself;
removing needs no such check.

**Request:** `{"permissions": ["workspaces:read"]}`. `permissions` must be
present and not null.

**Response:** `200 OK` with the token's metadata after the change.
Removing a scope the token does not hold succeeds and changes nothing.

**`PUT` with an empty array** removes every permission **and revokes the
token**: the answer carries `"permissions": []` with `revoked_at` set, and
the token is refused `401` from the next request on. `PATCH` and `DELETE`
refuse an empty array.

| Status | Condition                                                                                                                          |
| ------ | ---------------------------------------------------------------------------------------------------------------------------------- |
| 400    | `permissions` absent or null; an empty array on `PATCH` or `DELETE`; a retired or unregistered scope; the token is already revoked |
| 403    | A token adding a scope it does not hold                                                                                            |
| 404    | No such token, or it is not the caller's                                                                                           |

## Organizations

An organization is read by its **id**. A user gets a personal one at first
sign-in, and it is listed with any other they belong to.

### GET /api/v1/user/orgs

**Access:** `orgs:read`.

**Response:** `200 OK` with one entry per organization the caller belongs
to — `id`, `name`, `slug`, `url`, `owner_id`, `status`, `created_at`,
`updated_at`. An admin token belongs to no user, so it is answered with an
empty array.

### GET /api/v1/orgs/:id

**Access:** `orgs:read`, and the caller must be a member or an
administrator.

**Response:** `200 OK` with the organization and an `ETag`; `304` with no
body when the client's `If-None-Match` still matches.

| Status | Condition                                              |
| ------ | ------------------------------------------------------ |
| 403    | The organization exists and the caller is not a member |
| 404    | No organization has that id                            |

## Administration

These routes need the deployment's admin token or an API key of a user
holding the admin role. There is no `/admin` prefix. Every one answers
`401` without a credential and `403` for any other accepted credential —
including a personal access token holding every registered scope, because
a scope is not a role.

### Users

| Endpoint                                    | Description                                                                                                                                      |
| ------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| `POST /api/v1/users`                        | Create a user. Body: `{username, email, provider, provider_id}`, each required. `201`; `409` when the username or the provider identity is taken |
| `GET /api/v1/users`                         | List every user                                                                                                                                  |
| `GET /api/v1/users/:id`                     | Read a user. Supports `ETag`, answering `304` when it matches                                                                                    |
| `PATCH /api/v1/users/:id`                   | Change a user. Body: `{full_name}`, required                                                                                                     |
| `DELETE /api/v1/users/:id`                  | Delete a user with their credentials and memberships; `204`, or `409` for the last active admin                                                  |
| `POST /api/v1/users/:id/promote`            | Give the admin role. Repeating it succeeds and changes nothing                                                                                   |
| `POST /api/v1/users/:id/demote`             | Take the admin role away; `409` for the last active admin                                                                                        |
| `POST /api/v1/users/:id/block`              | Block a user: every credential of theirs stops working, none is revoked                                                                          |
| `POST /api/v1/users/:id/unblock`            | Unblock: every credential that is not revoked or expired works again                                                                             |
| `GET /api/v1/users/:id/keys`                | List a user's API keys; each entry carries `user_id` and no secret                                                                               |
| `DELETE /api/v1/users/:id/keys/:key_id`     | Revoke one of their keys; `204`                                                                                                                  |
| `GET /api/v1/users/:id/tokens`              | List a user's tokens; each entry carries `user_id` and no secret                                                                                 |
| `DELETE /api/v1/users/:id/tokens/:token_id` | Revoke one of their tokens; `204`                                                                                                                |

A **blocked** admin does not count as active, so a deployment cannot be
left with one admin who cannot sign in.

### Organizations

| Endpoint                                   | Description                                                                                                                                                                                                                                                           |
| ------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `POST /api/v1/orgs`                        | Create an organization. Body: `{name, slug, owner_id}` required, `url` optional. `201`; `409` for a taken name or slug; `400` for a slug that is not lower-case words joined by single hyphens, or an `owner_id` naming no user. The owner is a member from the start |
| `GET /api/v1/orgs`                         | List every organization                                                                                                                                                                                                                                               |
| `PATCH /api/v1/orgs/:id`                   | Change `name`, `url`, or both; at least one is required. `409` on a name collision                                                                                                                                                                                    |
| `DELETE /api/v1/orgs/:id`                  | Delete an organization and its memberships; `204`                                                                                                                                                                                                                     |
| `POST /api/v1/orgs/:id/block`              | Block an organization                                                                                                                                                                                                                                                 |
| `POST /api/v1/orgs/:id/unblock`            | Unblock an organization                                                                                                                                                                                                                                               |
| `GET /api/v1/orgs/:id/members`             | List the memberships                                                                                                                                                                                                                                                  |
| `PUT /api/v1/orgs/:id/members/:user_id`    | Add a member; `200` with the membership                                                                                                                                                                                                                               |
| `DELETE /api/v1/orgs/:id/members/:user_id` | Remove a member; `204`                                                                                                                                                                                                                                                |

A membership is `{org_id, user_id, username, email, role, created_at}`.

`GET /api/v1/orgs/:id` is **not** an administration route: it is the
member-facing read described under [Organizations](#organizations).

## Workspace access

Every route that names a workspace decides the caller's access the same
way, in one place. An admin token has `admin`; the user behind an API key
or a token is `owner` of a workspace they own and `member` of one belonging
to an organization they are in; everybody else has none.

**Membership is read on every request.** A user removed from the
organization loses access with their next request, with no re-authentication
and nothing to expire.

Each route needs one of three levels:

| Need    | Admitted                   | A member gets                                                                                            | Anybody else gets         |
| ------- | -------------------------- | -------------------------------------------------------------------------------------------------------- | ------------------------- |
| read    | owner, member, admin token | the owner's answer, differing only in `access`                                                           | `404 workspace not found` |
| write   | owner, admin token         | `403` with `error_type: forbidden` and the message `org members have read-only access to this workspace` | `404 workspace not found` |
| private | owner, admin token         | `404 workspace not found`                                                                                | `404 workspace not found` |

The `404` is the same answer a slug naming nothing gets, byte for byte, so
no refusal says whether a workspace exists.

**The checks run in this order:** the credential, then a token's scope, then
access, then the route's own status and precondition checks. A member
writing to an archived workspace is therefore `403`, not the `400` the
archived status would give. An access lookup that fails is `500`, never a
grant, and the route's handler does not run.

### Anti-enumeration

Six routes — `GET /api/v1/workspaces`, `GET`, `PATCH` and `DELETE
/api/v1/workspaces/:slug`, and `POST .../archive` and `.../reactivate` —
answer a token that lacks the scope with `404 workspace not found` instead
of `403`, identical to the answer for a workspace that does not exist.
Every other route, `POST /api/v1/workspaces` included, answers a missing
scope with the ordinary `403`.

## Workspaces

A workspace is the service's record of one repository: the URL it came
from, the settings that govern it, and who may read and change it.

### The workspace object

Every route that returns a workspace returns the same object: `slug`,
`git_url`, `hub_url`, `branch`, `owner_id`, `org_id`, `org_slug`, `access`,
`status`, `display_name`, `description`, `head_sha`, `clone_error`,
`upstream_head_sha`, `last_sync_at`, `sync_error`, `workspace_mode`,
`upstream_url`, `integration_branch`, `settings`, `created_at` and
`updated_at`.

`display_name` is never null or empty — it falls back to the slug — and
`description` is never null. `hub_url` is the clone URL on this service's
git server, and is `null` when `PARLEY_EXTERNAL_URL` is unset or the
workspace has no organization. `access` describes the caller, not their
scopes. `settings` carries the effective value of every setting, set or
not.

`status` is `pending` (the record exists, the repository has not been
copied in), `cloning`, `ready`, `failed` or `archived`.

### POST /api/v1/workspaces

**Access:** an API key, or a token with `workspaces:write`. An admin token
is refused `403`, because a workspace needs a real user as its owner.

| Field                          | Required          | Constraints                                                                                                                                                                  |
| ------------------------------ | ----------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `slug`                         | yes               | Globally unique; 3–64 characters; starts with a lowercase letter; ends with a lowercase letter or digit; lowercase letters, digits and hyphens only; no two hyphens in a row |
| `git_url`                      | yes               | An https or ssh git URL                                                                                                                                                      |
| `branch`                       | no                | Defaults to null, the repository's default branch                                                                                                                            |
| `org_id`                       | no                | An organization id **or slug**; the owner must be a member. Omitted, the owner's own organization is used                                                                    |
| `display_name`                 | no                | At most 128 characters; defaults to the slug                                                                                                                                 |
| `description`                  | no                | At most 1024 characters; defaults to `""`                                                                                                                                    |
| `workspace_mode`               | no                | `standard` (default) or `carry_patch`                                                                                                                                        |
| `upstream_url`                 | for `carry_patch` | Required for `carry_patch`, refused for `standard`                                                                                                                           |
| `integration_branch`           | no                | `carry_patch` only; defaults to `deploy`                                                                                                                                     |
| `settings`                     | no                | The settings to set; the rest take their defaults                                                                                                                            |
| `git_pat`                      | no                | Excludes the basic pair; needs an https `git_url`                                                                                                                            |
| `git_username`, `git_password` | no                | Both or neither; exclude `git_pat`; need an https `git_url`                                                                                                                  |

Credentials are stored as the workspace secrets `GIT_PAT`, or
`GIT_USERNAME` and `GIT_PASSWORD`, sealed before they reach any store
([ADR 4](adr/04-seal-secrets-with-aes-gcm-under-a-deployment-key.md)). No
response carries them.

**Response:** `201 Created` with the workspace, `status` `pending` and
`access` `owner`.

| Status | Condition                                                                                                                                                                                                                                  |
| ------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| 400    | A malformed `slug` or `git_url`; a field too long; an invalid mode combination; a bad credential combination; an unknown setting or a value it cannot take; `org_id` names no organization; no `org_id` and the caller has no organization |
| 403    | An admin token; a token without `workspaces:write`; the caller is not a member of the organization named                                                                                                                                   |
| 409    | A workspace with that slug exists                                                                                                                                                                                                          |

### GET /api/v1/workspaces

**Access:** `workspaces:read`; a token without it gets the
[anti-enumeration](#anti-enumeration) `404`.

**Query:** `include_archived=true` adds archived workspaces.

**Response:** `200 OK` with the caller's own workspaces and those of their
organizations, **newest first, and by slug where two share an instant**. An
admin token sees every workspace, each with `access: "admin"`.

### GET /api/v1/workspaces/:slug

**Access:** `workspaces:read` (a token without it gets `404`); read access.

**Response:** `200 OK` with the workspace. A member sees field-for-field
what the owner sees, differing only in `access`.

### PATCH /api/v1/workspaces/:slug

Change a workspace's mutable fields. Only the fields the body names change.

**Access:** `workspaces:write` (a token without it gets `404`); write
access.

| Field          | `null` means                                                                          |
| -------------- | ------------------------------------------------------------------------------------- |
| `display_name` | back to the slug                                                                      |
| `description`  | back to `""`                                                                          |
| `org_id`       | remove the organization                                                               |
| `settings`     | a key given as `null` returns to its default; `"settings": null` resets every setting |

`slug`, `git_url`, `branch`, `owner_id`, `workspace_mode`, `upstream_url`
and `integration_branch` are immutable and refused, as is the retired
`sync_mode` — use the `sync_enabled` setting.

**Response:** `200 OK` with the updated workspace.

| Status | Condition                                                                                                                                                                   |
| ------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 400    | An empty body; the workspace is archived; a field too long; an immutable field or `sync_mode`; an unknown setting or a value it cannot take; `org_id` names no organization |
| 403    | The workspace's **owner** is not a member of the organization named                                                                                                         |

The access check runs first, so a member's update of an archived workspace
is `403`, not `400`.

### Workspace settings

Twelve switches, each with a default. Every workspace object carries the
effective value of all of them; a write that names an unknown key, or a
value a key cannot take, is `400` naming the key and, for a list, the
values it accepts, and changes nothing — not even the valid keys in the
same body.

| Setting                           | Values                                   | Default     |
| --------------------------------- | ---------------------------------------- | ----------- |
| `rebuild_strategy`                | `rebase`, `merge`                        | `rebase`    |
| `rebuild_fail_mode`               | `fail_fast`, `continue`                  | `fail_fast` |
| `auto_rebuild_after_sync`         | boolean                                  | `true`      |
| `auto_rebuild_after_push`         | boolean                                  | `true`      |
| `patch_branch_source`             | `hub`, `origin`                          | `hub`       |
| `patch_divergence_policy`         | `replace`, `report`                      | `replace`   |
| `push_patches_to_origin`          | boolean                                  | `false`     |
| `rebuild_push_integration_branch` | boolean                                  | `false`     |
| `squash_merge_detection`          | `ancestry_only`, `content_based`, `both` | `both`      |
| `forge_api_url`                   | string                                   | `""`        |
| `upstream_forge_api_url`          | string                                   | `""`        |
| `sync_enabled`                    | boolean                                  | `true`      |

### POST /api/v1/workspaces/:slug/archive

Archive a workspace. An archived workspace keeps its records and can be
reactivated; writes to it are refused.

**Access:** `workspaces:write` (a token without it gets `404`); write
access. Archiving takes the [workspace lock](#the-workspace-lock).

**Response:** `200 OK` with the workspace, `status` `archived`. Bound
tokens are left alone and keep working.

| Status               | Condition                                    |
| -------------------- | -------------------------------------------- |
| 400                  | The workspace is already archived            |
| 409                  | The workspace is `cloning`                   |
| 409 `workspace_busy` | Another operation holds the workspace's lock |

### POST /api/v1/workspaces/:slug/reactivate

**Access:** `workspaces:write` (a token without it gets `404`); write
access.

**Response:** `200 OK` with the workspace, `status` `pending`. `409` when
the workspace is not archived.

### DELETE /api/v1/workspaces/:slug

Delete an archived workspace for good.

**Access:** `workspaces:write` (a token without it gets `404`); write
access.

**Behaviour:** in one transaction, every live token bound to the workspace
is revoked, every binding removed, the workspace's secrets deleted and the
record itself removed. A failure part way leaves all of them as they were,
so tokens are never revoked for a workspace that is still there.

**Response:** `204 No Content`. `409` when the workspace is not archived.

### The workspace lock

The operations that work on a workspace's repository take turns. A request
that finds another running answers `409` with `error_type:
workspace_busy` **at once**, without waiting, and can be retried. The lock
is shared by every instance of the service, and an instance that stops
without releasing one holds it no longer than its lease
([ADR 5](adr/05-hold-the-workspace-lock-as-a-lease-in-the-store.md)).

## Workspace-bound tokens

A personal access token can be tied to exactly one workspace when it is
minted. It then reaches that workspace under its scopes, with the answers
an unbound token of the same user and scopes would get, and nothing else.

### POST /api/v1/workspaces/:slug/tokens

Mint a token bound to the workspace. The body, the validation, the expiry
and the privilege rules are those of
[`POST /api/v1/user/tokens`](#post-apiv1usertokens); the token is bound
afterwards.

**Access:** the workspace's **owner** only, with `tokens:manage`.

**Response:** `201 Created`, `Cache-Control: no-store`, with the token's
metadata, `workspace`, and `token` — the only response that carries it.

**Errors**, in the order they are checked:

| Status            | Condition                                                                                                    |
| ----------------- | ------------------------------------------------------------------------------------------------------------ |
| 401               | No credential                                                                                                |
| 403 `token_bound` | The caller is itself a bound token                                                                           |
| 403               | An admin token: a bound token needs a real user as its owner                                                 |
| 404               | The workspace does not exist, or the caller is not its owner — a member of its organization is told this too |
| 400               | The workspace is archived                                                                                    |
| any               | The token creation's own answer, unchanged; nothing is bound                                                 |
| 500               | The binding could not be written; the token is revoked                                                       |

### GET /api/v1/workspaces/:slug/tokens

List the tokens bound to the workspace, revoked and expired ones included,
ordered by creation time then token id. Works on an archived workspace. A
binding whose token no longer exists is left out. No entry carries a token
or a hash.

**Access:** the owner or an admin token, with `tokens:read`; a member gets
`404`. A bound token is refused `403 token_bound`.

### What a bound token may reach

| Request by a token bound to `S`                                              | Answer                                                                                                      |
| ---------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| Any route naming a workspace other than `S`                                  | `404 workspace not found`, **before** any scope, existence or status check                                  |
| The same routes for `S`                                                      | The route's usual answer                                                                                    |
| `GET /api/v1/workspaces`                                                     | At most `S`                                                                                                 |
| `GET /api/v1/sandbox-profiles`                                               | As for an unbound token                                                                                     |
| Any other route under the mount point, including one that matches nothing    | `403`, `error_type: token_bound`, message `this token is bound to workspace <S>`                            |
| An identity route (`/user`, `/user/keys`, `/user/tokens`, `/users`, `/orgs`) | `403`, `error_type: token_bound`, message `this token is bound to a workspace` — the workspace is not named |

The `404` for another workspace is the same answer an unknown slug gets, so
a bound token learns nothing about what else exists.

**A binding the service cannot read is `500`.** The token is never let
through as though it were unbound.

Deleting a workspace revokes its live bound tokens and removes their
bindings in one transaction. At start and every hour after, a sweep
revokes the live tokens of bindings whose workspace is gone and removes
bindings whose token is gone; a binding of a revoked or expired token of a
workspace that still exists is kept, so that workspace's token list still
shows it.

## Sandboxes

This deployment lends no sandboxes. Every sandbox route is registered and
answers `404` with `error_type: sandboxes_disabled` and the message
`sandboxes are not enabled on this hub`, to every authenticated caller,
**before** any scope is checked and before the workspace is looked up — so
the answer is the same whether the workspace exists, is archived, or the
caller's token lacks the scope.

The paths are `GET /api/v1/sandbox-profiles`, `POST`, `GET`
`/api/v1/workspaces/:slug/sandboxes`, `GET`, `DELETE`
`/api/v1/workspaces/:slug/sandboxes/:id` and `GET /api/v1/sandboxes`.

A caller with no credential is still refused `401` first: the missing
credential is the earlier answer, and it carries no `error_type`.

The routes are registered rather than left unrouted, because an unrouted
path answers `404` with no `error_type`, which clients tell apart from a
deployment that has sandboxes switched off.

## Server Endpoints

These four routes are served at the server root, outside the mount point,
and need no credential.

### GET /healthz

Liveness: the process is up. Answers without reading any store.

**Response:** `200 OK` with `{"status": "ok"}`.

### GET /readyz

Readiness: the service can serve.

**Response:** `200 OK` with `{"status": "ready"}` when the relational store
answers; `503` with the error envelope when it does not. The reason goes to
the log, not to the probe's answer.

### GET /version

Build information and the mount point in use. Cached publicly
(`Cache-Control: public, max-age=300`).

**Response:** `200 OK` with exactly these four keys:

```json
{
  "build_time": "2026-10-27T00:00:00Z",
  "commit": "abc1234",
  "mount_point": "/api/v1",
  "runtime_version": "Node.js/26"
}
```

`runtime_version` is the JavaScript runtime's version, reported under a
truthful key in place of the hub's `go_version`. `build_time` and `commit`
come from the deployment's environment and are the empty string where it
supplied neither. `build_time` is rendered as RFC 3339 in UTC whatever
offset the deployment supplied, and is the empty string when it supplied
something unreadable.

### GET /metrics

Operational metrics in the Prometheus text format, for a scraper.

**Response:** `200 OK`, `Content-Type: text/plain; version=0.0.4`.

Every metric the hub's reference names is exposed with the type and the
label names that reference gives it. A metric this service has no source
for yet is exposed with the value `0` rather than omitted, so a scraper's
queries do not start failing the day it gains one.

`afhub_http_requests_total` counts every answered request by `method`,
`path` and `status`, and `afhub_http_request_duration_seconds` observes its
latency in the documented buckets. The `path` label is the **route
template** the request matched, never the requested URI; every request that
matched no route is counted under one fixed value, so no caller can grow the
label set.
