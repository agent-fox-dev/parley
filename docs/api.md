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
- [Secrets](#secrets)
- [Variables](#variables)
- [Sandboxes](#sandboxes)
- [Keeping a workspace and its remote in step](#keeping-a-workspace-and-its-remote-in-step)
- [Merging branches](#merging-branches)
- [Carrying a patch stack](#carrying-a-patch-stack)
- [Agent sessions and what they cost](#agent-sessions-and-what-they-cost)
- [Audit and events](#audit-and-events)
- [The forge](#the-forge)
- [Git Server](#git-server)
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

**Behaviour.** Branches pushed to this service exist only here, so before
the repository is let go every branch is pushed to `origin` with the
workspace's stored credential. The push does **not** force: pushing a
workspace out is no licence to rewrite history on somebody else's forge.
If origin refuses a branch, the push fails, or the workspace has no
usable credential, the workspace is archived anyway and **the repository
is kept**, so no commit is lost and reactivating reuses it.

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
so tokens are never revoked for a workspace that is still there. The
workspace's repository is removed from the store afterwards, so a store
that cannot answer never leaves the record behind.

**Response:** `204 No Content`. `409` when the workspace is not archived.

### Repository size

A workspace's repository may hold at most **1 GB**. The service counts
the bytes it writes, because the store reports no storage error of its
own, and a write that would take a repository past the cap is answered
`507` with a message naming the limit. Nothing is written, so everything
the repository already holds is still readable and its refs are
unchanged. See [ADR 7](adr/07-account-for-repository-bytes-and-what-the-sizes-cost.md).

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

## Secrets

Named values kept for a user, an organization or a workspace — the git
credentials of a workspace, a forge webhook secret, whatever a deployment
needs. **Their values are never returned.**

| Routes                             | Whose                          | Who may reach them                                                                                      |
| ---------------------------------- | ------------------------------ | ------------------------------------------------------------------------------------------------------- |
| `/api/v1/user/secrets`             | the caller's own               | any user; an admin token gets `403`, having no user                                                     |
| `/api/v1/orgs/:slug/secrets`       | an organization's, **by slug** | members and admin tokens; anybody else gets `404`, as for an organization that does not exist           |
| `/api/v1/workspaces/:slug/secrets` | a workspace's                  | private access: the owner and admin tokens; a member of its organization gets `404`, as a stranger does |

Each scope serves four routes:

| Route                     | Answer                                                                                      |
| ------------------------- | ------------------------------------------------------------------------------------------- |
| `POST .../secrets`        | `201` with the created entries, **no values**                                               |
| `GET .../secrets`         | `200` with the entries, sorted by key ignoring case, **no values**                          |
| `PATCH .../secrets/:key`  | `200` with the entry, **no value**; `400` when `value` is missing; `404` for an unknown key |
| `DELETE .../secrets/:key` | `204`; `404` for an unknown key                                                             |

A secret in a response is `{key, created_at, updated_at}` — there is no
`value` field at all, so no route can answer with one.

Values are sealed before they reach the store
([ADR 4](adr/04-seal-secrets-with-aes-gcm-under-a-deployment-key.md)), and a
route that reads a value registers it with the request, so a failure the
service did not expect cannot carry it into a log.

The access needed is `secrets:read` to read and `secrets:write` to change.

### Reserved secret names

Four names at a **workspace's** scope are read by the service itself, for
the git remotes a workspace follows. They are ordinary secrets in every
other respect; nothing outside the workspace's own scope is consulted for
them, so neither the owner's nor the organization's secret of the same
name has any effect.

| Remote                                                  | Looked for, in order                                                     |
| ------------------------------------------------------- | ------------------------------------------------------------------------ |
| `origin` (the workspace's `git_url`)                    | `GIT_PAT`, else `GIT_USERNAME` with `GIT_PASSWORD`, else none            |
| `upstream` (a `carry_patch` workspace's `upstream_url`) | `UPSTREAM_GIT_PAT`, else the upstream pair, else **origin's** credential |

A pair with only one half stored is ignored and the service goes on as
though neither half were there, saying so in its log by name and never by
value.

### Rules both kinds obey

| Rule         | Constraint                                                                                                         |
| ------------ | ------------------------------------------------------------------------------------------------------------------ |
| Key format   | Letters, digits and underscores; never starting with a digit                                                       |
| Key length   | At most 255 characters                                                                                             |
| Key matching | Case-insensitive, both in lookups and in the uniqueness check; the stored key keeps the casing it was created with |
| Value size   | At most 262,144 bytes, measured in bytes and not characters                                                        |
| Entries      | At most 100 per scope, counted per kind                                                                            |
| Ordering     | Every list is sorted by key, ignoring case                                                                         |

A create is all or nothing: one duplicate key (`409`) or one entry over the
cap (`400`) and none of the create's entries is written.

## Variables

The same thing, at the same three scopes, under the same rules — except
that **their values are returned** and a workspace's are read at read
access and changed at write access, so a member of its organization lists
a workspace's variables and gets `403 forbidden` on a change.

A variable in a response is `{key, value, created_at, updated_at}`. The
access needed is `vars:read` to read and `vars:write` to change.

A variable named after a [workspace setting](#workspace-settings) is an
ordinary variable and changes no behaviour.

### GET /api/v1/workspaces/:slug/vars/resolved

The variables a workspace sees, merged from three tiers: the workspace's
override the organization's, which override the **owner's**.

**Access:** `vars:read`; read access.

**Response:** `200 OK` with an array sorted by key ignoring case, each
entry carrying `key`, `value`, `origin` (`user`, `org` or `workspace`),
`created_at` and `updated_at`.

Two rules make this a property of the workspace rather than of who asked:
the user tier is the **workspace owner's**, never the caller's, so a member
of the organization is answered exactly what the owner is answered; and the
organization tier is included only when the owner belongs to that
organization.

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

## Keeping a workspace and its remote in step

Three routes close the loop in both directions. All three take
`workspaces:write` and write access, all three take the workspace lock
without waiting — a held lock is `409` with `error_type`
`workspace_busy` — and no answer of any of them carries the stored
credential, the userinfo of `git_url` or the git library's own words.

### POST /api/v1/workspaces/:slug/sync

Brings origin's work in. The workspace must be `ready` with its
`sync_enabled` setting true, or the answer is `400` and origin is not
contacted.

| Outcome                           | Status | What the workspace records                               |
| --------------------------------- | ------ | -------------------------------------------------------- |
| Up to date                        | 200    | `last_sync_at` moves; `head_sha` does not                |
| Fast-forward                      | 200    | `head_sha` and `upstream_head_sha` move to origin's head |
| Reset (`?reset_to_upstream=true`) | 200    | the same, whatever the ancestry                          |
| Diverged                          | 409    | `sync_error` names `reset_to_upstream` as the recovery   |
| Fetch failure                     | 502    | `sync_error` says what origin did                        |
| Cancelled                         | 504    | `sync_error` says it was cancelled                       |

`sync_error` is the workspace's present condition, not a log: the next
successful sync clears it. A sync in progress is not a stored state —
the status reads `ready` throughout, and a second sync is refused by
the lock.

A `carry_patch` workspace fetches upstream and works on its patch list
instead; that is the carry-patch package's, and until it lands such a
sync answers `501`.

### POST /api/v1/workspaces/:slug/reclone

Pushes out what the repository holds that origin does not, discards the
repository and copies it again. The push is best effort: a repository
too broken to read is what the route is for, and its failure is not the
route's answer. The workspace returns to `pending` with `sync_error`
and `upstream_head_sha` cleared, and follows the new copy.

`400` for an archived workspace, `409` where a copy is already under
way, `500` where the repository cannot be discarded or the workspace
cannot be updated — and in that case nothing changes.

### POST /api/v1/workspaces/:slug/branches/:name/push

Sends exactly `refs/heads/<name>` to `refs/heads/<name>` on origin,
fast-forward only unless `{"force": true}` is given. `:name` is one
path segment with each `/` escaped, decoded exactly once: `fix%2F42`
names `fix/42` and `a%2525b` names `a%25b`.

The checks run in this order, and the order is the behaviour: the
credential, then a PAT's scope before the workspace is looked up; the
workspace and write access; the body, then the name; readiness; the
branches this route will not send; the lock; then the protections that
need the repository; then the push.

A success is `200` with `branch`, `sha`, `remote`, `remote_url` without
userinfo, and `result` — `up_to_date`, `forced` or `pushed`. The push is
handed to the platform's execution context before it is awaited, so it
finishes even where the caller goes away.

Every refusal of this route repeats its own type at the start of the
message, because the CLI shows the message and nothing else:

| Status | `error_type`                                                                   |
| ------ | ------------------------------------------------------------------------------ |
| 400    | `integration_branch`, `patch_branch`, `protected_branch`                       |
| 404    | `branch_not_found`, `forge_not_found`                                          |
| 409    | `workspace_busy`, `non_fast_forward`                                           |
| 422    | `forge_rejected`                                                               |
| 502    | `forge_unauthenticated`, `forge_forbidden`, `forge_unreachable`, `forge_error` |
| 504    | `forge_timeout`                                                                |

## Merging branches

A merge job rebases a source branch onto a target and fast-forwards
the target to the result, so the history it leaves is the source's
commits on top of the target's with no merge commit.

**`POST .../merges`** answers `202` with a queued job. Both branch
names are required, both follow git's rules and both must exist, and
**the body is checked before the workspace is looked up** — the
opposite of the anti-enumeration rule elsewhere, and deliberate: a
name that breaks git's rules is the caller's mistake whatever
workspace it names. The same source into the same target twice is
`409 duplicate_merge`.

A merge **waits its turn** for the workspace rather than being
refused. Every other operation answers `409 workspace_busy` when the
workspace is taken; a queued job has somewhere to wait.

`GET .../merges` lists them (at most 200), `GET .../merges/:id` reads
one, `DELETE .../merges/:id` cancels a queued one, and
`POST .../merges/:id/requeue` queues a dead-lettered one again with
its retry counted.

A job that stops making progress for fifteen minutes is dead-lettered
rather than left reading "running" with nothing running it, and a job
whose runner went away is simply run again — a merge is a rebase of
whatever the branches hold now, so running it twice is safe. The
target branch holds either the head it had or the merged result, never
anything between the two.

**`POST .../rebase`** is the same arithmetic offered synchronously, for
the question "which of these still apply": up to 20 branches rebased
onto a target in the order given, one result each — `ok` with the new
head, `conflict` with the paths, or `error`. A branch that conflicts
does not stop the others and is left where it was. Unlike a merge job
this is refused at once for a busy workspace, because a synchronous
request has nowhere to wait.

## Carrying a patch stack

A `carry_patch` workspace keeps an ordered list of branches and an
integration branch rebuilt from the upstream head plus those branches.

**The patch list.** `POST .../patches` adds one or, with a JSON array,
several in one transaction: every element's syntax is checked first in
array order, then every branch, and the first branch failure refuses
the whole batch while leaving branches already created in place. A
branch is looked for in the workspace's own repository, in the fork's
already-fetched copy, and — only with `patch_branch_source` `origin` —
on the fork itself, and which one answered is reported as
`branch_resolution`. `GET`, `PATCH`, `DELETE`, `.../restore` and
`.../reorder` do what they say; positions are 1 to n with no gap after
every one of them, and a patch's branch never changes.

**`POST .../patches/:id/reset-to-origin`** sets a patch branch to the
fork's tip by the first rule that fits: `created`, `none`,
`fast_forwarded`, or `replaced` with the old tip kept under
`refs/hub/replaced/<branch>`.

**The sync of a carry-patch workspace** is a different operation from
the standard one: it fetches upstream rather than the fork, moves no
branch of its own, and answers with `patches_merged`,
`rebuild_triggered`, `force_push_detected`, `origin_fetched` and —
with `patch_branch_source` `origin` — `patches_synced` and
`patches_diverged`. It uses no `sync_error`. A failure refreshing a
patch branch ends it with `500`, keeping what the earlier steps
reached and writing neither `upstream_head_sha` nor `last_sync_at`.

**Rebuilds.** `POST .../rebuild` answers `202` with a queued job;
`GET .../rebuilds` and `.../rebuilds/:id` follow it, and its per-patch
results grow as it runs. `DELETE` cancels a queued one,
`.../requeue` queues a dead-lettered one again, and `.../rollback`
sets the integration branch back to where it was.
`POST .../rebuild/preview` predicts what a rebuild would do and writes
nothing at all.

A rebuild holds the workspace lock only at its start and its end, so a
sync or a push may run while patches are applied; a tip that moves
meanwhile yields exactly one follow-up rebuild, submitted by the hub.
A workspace owes at most one rebuild at a time, whatever asked for it
— a push to a patch branch, a sync, or a reset.

**`GET .../rerere`** lists the conflict resolutions the hub will
replay, and `DELETE .../rerere/<pathspec>` forgets one by identity or
by path, so the next occurrence is reported rather than settled.

**`GET .../patch-status`** is the dashboard: the workspace, the latest
rebuild, every patch with what that rebuild made of it, and counts
derived from the same list it carries.

## Agent sessions and what they cost

A session records one agent run's use of a workspace: who opened it,
when, against which model, what it spent and how it ended. Everything
is reported in tokens. The service invents no currency, because the
reference defines none and a price would be a guess at somebody's
contract.

**Opening.** `POST /api/v1/sessions` needs `sessions:write` **and
write access** to `workspace_slug` — a member of the owning
organization is refused, unlike every read here. The workspace's
status does not matter: an agent that worked on an archived workspace
still spent what it spent. An open that repeats an `id` the service
already holds answers `200` with that session instead of opening a
second one, so an agent that retried a request it never saw answered
ends with one session, not two.

**Reporting and closing.** `POST .../sessions/:id/usage` adds a usage
record (`201`) and `POST .../sessions/:id/complete` ends the session
(`200`). Both are for the session's **creator**, not the workspace's
owner: several agents share one workspace, and an owner check would
let any of them close another's session. Where the session recorded
a credential, that credential is the whole test — two keys of one
person are two agents. A session that is no longer active refuses
further usage with `409`, and a negative token count is `422`.

**Reading.** `GET /api/v1/sessions`, `GET .../sessions/:id` and
`GET .../sessions/:id/usage` need `sessions:read`, read access **and
an active workspace**: a session of an archived workspace answers
`403 workspace access denied` rather than `404`. This is the one
family of routes that prefers saying the workspace is inactive to
hiding that it exists. The list takes `workspace_slug`, `run_id`,
`status`, `credential_type` and `since`, and with no `workspace_slug`
it lists the sessions of every workspace the caller can read. A
session carries its `token_summary`; the usage list carries `totals`
over **every** record of the session, whatever page was asked for,
because a total of one page is not a total.

**`GET /api/v1/workspaces/:slug/cost`** answers what the workspace's
agents spent over a period: `since` and `until` (the last day by
default, and `since` must be before `until`) and `group_by` of `day`,
`session` or `model`. The totals and the breakdown are summed from
one list of records, so they cannot disagree. A session that spans
two days is counted once in `totals.sessions` and appears in both
days' rows.

**Retirement.** Archiving or deleting a workspace ends its active
sessions with status `terminated`, inside the same lock as the rest
of the operation, so no session is left reading "active" against a
workspace that is gone.

### Pagination

The session and usage lists page with an opaque cursor. `limit`
(sessions: 50, at most 500; usage: 100, at most 1000) and `order`
(`desc` by default) say how much and which way; `cursor` says from
where. The answer carries `next_cursor` and `has_more`, and
`next_cursor` is `null` on the last page.

A cursor holds the position of the last row served — its timestamp
and its id — not an offset, so rows inserted while a caller pages are
neither skipped nor served twice. It is the service's own string:
anything else is `400 the cursor is not one this service issued`.

## Audit and events

Every operation worth remembering records an event, agents report what
they did for each run, one query reads across both, and a stream carries
them live.

### The hub's own events

Each significant operation records one event with an `event_type`, an
actor (by default the calling credential's user and type), the
`workspace`, a `resource_type`, a `resource_id`, an `action` and
metadata. The types and the metadata keys of each are the two tables of
[the hub's reference](prd/api.md#hub-audit-events); the service records
from one declaration of those tables, so an event carries exactly the
keys the reference lists and no others.

**Recording never changes the answer of the request that caused it.** A
recorder that fails leaves the operation's status and body exactly as
they would have been, which is why every call site goes through a
recorder that swallows and logs.

**A bound token's refusals** are recorded at most once per token per
minute. The event is written at the first refusal and the refusals held
back since are counted into its `suppressed`, so twenty refusals inside
a minute leave one event reading nineteen suppressed.

Events of operations this service does not perform yet — the forge
writes and deliveries, the sandbox leases — are declared and recorded by
nobody until the packages that perform them land.

### What an agent reports

The per-run routes live under `/api/v1/workspaces/:slug/runs/:run_id/`:
`events`, `sessions/outcomes`, `tools/calls`, `tools/errors`, `traces`,
`events/batch`, `traces/batch` and `postmortem`.

A `run_id` is `YYYYMMDD_HHMMSS_` and six lowercase hex characters;
anything else is `400`. Every record is **idempotent by `id`**: a repeat
answers `200` with what is stored and writes nothing, which is what
keeps a retried request from counting a run's work twice. Client
timestamps are RFC 3339.

**The access rule runs the other way round from the rest of the
service**, and deliberately: the workspace must exist (`404`), then it
must not be archived (`409 workspace_archived`, answered **before**
anybody's access is considered, so a member of an archived workspace is
told `409` and not `403`), and only then must the caller own it
(`403 workspace_access_denied`, where every other route would answer
`404`). A refused request stores nothing. The reads are open to anyone
who may read the workspace, whatever its status.

**A batch** carries 1 to 1000 items and answers `200` with `accepted`,
`duplicates` and an `errors` entry per failing item naming its index;
an item's failure does not stop the others. An empty array is `400` and
more than 1000 items is `413`.

**A run has at most one postmortem**, and it is the first submitted: a
second submit answers `200` with the one already stored. A schema
version other than 1 is `422 unknown_schema_version` — the request is
well formed and this service simply does not read that version. Reading
a run that has none is `404 postmortem_not_found`.

**The lists** page with `order`, `limit` (50, at most 500), `cursor`,
`since` (inclusive) and `until` (exclusive), and each answers with its
own collection, `next_cursor` and `has_more`. The JSON an agent sent
comes back as JSON.

### GET /api/v1/audit

Hub events and agent events as one timeline, newest first. Both halves
are selected with the same columns and joined, so the database orders
one list rather than the service merging two — which is what makes
paging to the end yield every event exactly once.

Times are compared as instants: every timestamp is stored through
`toISOString`, so one precision everywhere and no event sorts by its
spelling.

The filters are `source`, `workspace`, `event_type`,
`event_type_prefix`, `severity`, `actor_id`, `actor_type`,
`resource_type`, `action`, `run_id`, `since`, `until`, `limit` (100, at
most 1000) and `cursor`. **A filter only hub events carry leaves the
agent events out** rather than matching none of them, and `run_id`
leaves the hub events out.

A caller sees every workspace they own or share an organization with,
archived ones included; an event belonging to no workspace is the
deployment's own business and reaches an admin token alone. A workspace
the caller may not read is answered exactly as one that does not exist.

### The transcript

`GET .../runs/:run_id/transcript?node_id=` rebuilds one node's
conversation from its trace events. `session.init` becomes a `system`
message, `assistant.message` an `assistant` message, `tool.use` a
`tool_use` message with its tool, and `tool.error` a `tool_error`
message; every other trace type produces no message. A request without
a node is `400`, because a run's traces are several conversations
interleaved.

### The live event stream

`GET /api/v1/events` is a `text/event-stream` with two kinds of frame,
`heartbeat` and `audit_event`. A heartbeat is sent at once and then
every 30 seconds. `workspace` narrows it to one workspace and
`category` to `hub` or `agent`.

- **The workspaces a stream may see are fixed when it opens**, so
  joining or leaving an organization takes effect on the next
  connection.
- A connection with no reads for 60 seconds is closed, and a client
  that falls behind keeps the most recent 256 events and loses the
  oldest rather than stalling the stream.
- An event recorded by **any** component of the deployment reaches the
  subscriber: events are published to a table every component shares,
  the same substrate as the workspace lock and the job queue.
- A workspace the caller may not read is `404` before the stream
  starts, and the configured limit of concurrent streams
  (`AF_SSE_MAX_CONNECTIONS`, 100) is `503`. Neither leaves a half-open
  stream.

## The forge

A workspace's repository lives on GitHub or GitLab. These routes read
and change it **with the credential the workspace holds**, never the
caller's: an agent that can open a pull request through Parley needs no
forge token of its own, which is the whole point. No answer shows the
credential, only its kind.

### Which forge, and whose credential

The repository is read from the remote URL — `https://`, `ssh://` or
`git@host:owner/name` — with `.git` and any userinfo removed and a
GitLab subgroup path kept whole, so `…/group/subgroup/widgets` has the
owner `group/subgroup`.

Which forge it is comes from the host's name or from an API URL setting
containing one of the two words. Without a setting the API URL is
`https://api.github.com` for `github.com`, `https://<host>/api/v3` for
another GitHub host and `https://<host>/api/v4` for a GitLab host; a
GitLab setting gains `/api/v4` when it lacks it. **When neither the host
nor a setting says which forge it is, the answer is `422
forge_unknown`** and nothing is sent.

`?target=upstream` works on a `carry_patch` workspace's `upstream_url`,
with the upstream credential and `upstream_forge_api_url` — which does
**not** fall back to `forge_api_url`, the two being different
repositories. Any other value is `400`.

Nothing is cached: a rotated secret or a changed setting applies to the
next call. Every request carries the hub's user agent, refuses a
redirect to another host, port or scheme, and has 30 seconds including
its retries.

### The order of checks

A read checks the credential, then a token's `forge:read` scope
**before the workspace is looked up**, then the workspace, then the
target, then the path and query parameters, then the forge. A write
checks the credential, then `forge:write`, then the workspace with a
member refused `403 forbidden`, then that it is not archived, then the
request, then the forge, and **last** the body limit — so an oversized
body on a workspace whose forge cannot be identified is `422
forge_unknown` rather than `413`. A request refused at any step sends
nothing to the forge and records no event.

### GET /api/v1/workspaces/:slug/forge

How the hub reaches the forge, **without contacting it** — the route to
run when another answers `forge_unknown`. It reports the forge (or
`unknown` for a host it cannot classify, rather than refusing), the API
URL, the repository, the credential's kind, the upstream's equivalent,
and a webhook block saying whether the secret is stored, where the
forge should post and when the last delivery arrived. It never carries
the secret, and it answers for an archived workspace.

An API URL setting that is not an absolute `https` URL with a host and
without userinfo, query or fragment is `422 forge_api_url_invalid`,
naming the setting and the rule but never the value. A store that
cannot be read is `500` with no partial answer.

### The reads

`forge/repository`, `issues`, `issues/:n`, `issues/:n/comments`,
`pulls/:n`, `pulls/:n/state`, `pulls/:n/files`, `pulls/:n/checks` and
`pulls/:n/reviews`, each `forge:read` with read access and any workspace
status. Every list is a bare array, empty rather than null.

`GET .../issues` takes `state`, `labels`, `assignee`, `sort`,
`direction` and `limit` (1 to 100, 30 by default). **A parameter the
caller did not give is not sent**, so the forge's own default applies;
empty items are dropped from `labels`. A bad, empty or repeated value is
`400` naming the parameter, and `:n` must be a positive decimal integer.

An issue thread carries `truncated` where more comments exist than were
read, and where the issue was read but its comments were not it answers
`200` with the issue and the comments' error in `comments_err`.

GitLab has neither reviews nor check runs; its approvals and its
pipelines are what those two routes answer with. See
[ADR 11](adr/11-what-the-two-forges-really-answer.md) for every such
divergence.

### The writes

`POST .../issues`, `PATCH .../issues/:n`, `POST .../issues/:n/close`,
`POST .../issues/:n/comments`, `POST .../issues/:n/labels`,
`DELETE .../issues/:n/labels/:name`, `POST .../labels`,
`POST .../pulls` and `POST .../pulls/:n/comments`, each `forge:write`
with write access on a workspace that is not archived.

Three of them make more than one request, and their ordering is the
contract:

- **A close with a comment** posts the comment first and closes only if
  that worked. A refused comment leaves the issue open; a close that
  fails afterwards leaves the comment in place and records its URL in
  the event.
- **A label removal** answers `204` whether or not the issue had the
  label. The forge says "not found" for both, so the issue is read once
  to tell them apart, and only an issue that is really gone is
  `404 forge_not_found`. The name is the last path segment decoded
  exactly once: `type%2Fbug` removes `type/bug` and `af:fix` is itself.
- **A label create** never changes a label that exists (`200`), creates
  one that does not (`201`), and reads once more where a concurrent
  call won the race.

An issue update changes only the fields it names; `labels` and
`assignees` replace the whole set, `body: ""` clears the body, and a
body that sets nothing is `400 nothing to update` — as is one that sets
only `assignees` on GitLab, which takes user ids rather than names. A
pull request's `head` is sent exactly as given, so `owner:branch` opens
from a fork, and nothing is deduplicated: a retried create can create
twice.

**Body limits.** An issue's body on create and update, a close's
comment, a comment's body and a pull request's body are measured in
Unicode code points — 65,536 on GitHub, 1,000,000 on GitLab — and one
over it is `413 body_too_large` with nothing sent. Titles, heads, bases,
label names and descriptions and assignee names are not measured.

**Audit.** A write that reached the forge records exactly one
`hub.forge.*` event, successful or not; one refused before it reached
the forge records none.

### When the forge says no

| What happened                                              | Answer                      |
| ---------------------------------------------------------- | --------------------------- |
| the forge answered 404                                     | `404 forge_not_found`       |
| it answered 401                                            | `502 forge_unauthenticated` |
| it answered a 403 that is not a rate limit                 | `502 forge_forbidden`       |
| a rate limit the hub could not wait out                    | `429 forge_rate_limited`    |
| it answered another 4xx                                    | `422 forge_rejected`        |
| it answered 5xx, unreadably, or redirected to another host | `502 forge_error`           |
| the connection failed                                      | `502 forge_unreachable`     |
| the 30-second deadline passed                              | `504 forge_timeout`         |

The statuses are deliberately not the forge's own: its `401` means the
**workspace's** credential was refused, not the caller's. The forge's
message is carried through as `the forge answered <status>: <message>`
with the token replaced, URL userinfo removed and the text cut to 1024
bytes, and a `401` for a stored username and password adds that the
forge API needs a token.

One rate-limit backoff of up to two minutes is waited out and the
request made again. A wait the 30-second deadline cannot contain is
`504 forge_timeout`, and a refusal carries `Retry-After` in whole
seconds where the forge said how long. A write with no credential
configured is `502 forge_error` with nothing sent.

## Git Server

Each workspace's repository is served over git's smart HTTP protocol at the
server root, outside the mount point, because a clone URL is what a person
types:

```
<external_url>/git/<org-slug>/<workspace-slug>.git
```

That address is exactly what the workspace reports as its `hub_url`, and it
is null for a workspace with no organization or a deployment with no
external URL. The `.git` suffix is required.

Three paths are served:

| Route                                                    | What it is                                                 |
| -------------------------------------------------------- | ---------------------------------------------------------- |
| `GET /git/:org/:repo/info/refs?service=git-upload-pack`  | the ref advertisement for a fetch                          |
| `GET /git/:org/:repo/info/refs?service=git-receive-pack` | the ref advertisement for a push, which needs write access |
| `POST /git/:org/:repo/git-upload-pack`                   | a fetch                                                    |
| `POST /git/:org/:repo/git-receive-pack`                  | a push                                                     |

**Authentication is HTTP Basic.** The password is a hub credential — an API
key, a personal access token or the admin token — and the username is
ignored. A request with no credential is answered `401` with
`WWW-Authenticate: Basic realm="Parley"`, which is what makes a git
credential helper offer its key.

**Every refusal is pkt-line**, not the JSON envelope, with `text/plain` as
its content type: that is the shape in which a git client renders the
message to the person. The refusals are:

| Status | Message                                               |
| ------ | ----------------------------------------------------- |
| 401    | `authentication required`                             |
| 403    | `org members have read-only access to this workspace` |
| 403    | `invalid or missing service parameter`                |
| 403    | `this token does not hold git:read` (or `git:write`)  |
| 404    | `repository not found`                                |
| 409    | `repository not available`                            |
| 503    | `repository not available`                            |

**Who may do what.** The owner and the admin token fetch and push; a member
of the workspace's organization fetches and is refused a push; everyone
else, and a token bound to another workspace, is told the repository was
not found. The checks run in that order: the workspace and its
organization, then whether the repository is ready, then access, then the
token's git scopes — so a member's push is refused for being a member
whatever scopes the token holds.

**The protocol version is 0**, and a client that asks for version 2 falls
back to it. Version 2's negotiation expects the server to remember what it
acknowledged between one request and the next; see ADR 10 for the
measurements. A gzip-encoded request body is accepted.

**A push is decided one ref at a time**, before any of it reaches the
store, and the client receives git's per-ref report covering every ref it
sent. A ref may be refused while the rest of the same push lands:

- Nothing under `refs/hub/replaced/` may be created, updated or deleted, on
  any workspace, with any credential; the ref is reported
  `ng <ref> refs/hub/replaced/ is maintained by the hub and cannot be
pushed to`.
- On a `carry_patch` workspace, an update of a registered patch branch
  follows the workspace's `patch_branch_source` and
  `push_patches_to_origin` settings: refused with the address to push to
  instead, forwarded to the fork and taken only if the fork takes it, taken
  and then mirrored to the fork, or simply taken.

**After every push**, including one whose every ref was refused, the
workspace's `head_sha` is refreshed from the repository.

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
