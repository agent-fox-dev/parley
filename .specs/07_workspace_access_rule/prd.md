---
spec_id: "07"
spec_name: "workspace_access_rule"
title: "Decide every caller's access to a workspace"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "0f89688be7e097d7ee9ded84f0054612c48b43bd148cdd61a78f77d15f1b22a2"
schema_version: 3
source: "docs/prd/03_platform.md"
---

# Decide every caller's access to a workspace

## Intent

Every route and every git request that names a workspace must reach the same answer to one question: what may this caller do here. Parley answers it in one place, the workspace access rule, so that an owner, an organization member, the admin token and a stranger are each treated the same way on every surface, a workspace a caller cannot read is indistinguishable from one that does not exist, and a task fork never opens more than its parent does.

## Goals

- One access component in `internal/access` that every workspace route calls with a slug and a declared need (`read`, `write` or `owner`) and that answers granted, `404 not_found` or `403 forbidden`.
- A caller's level on a workspace is one of `admin`, `owner`, `member` or none, resolved from the workspace record, the caller's identity and the caller's memberships in active organizations.
- A caller with level none receives, for an existing workspace, a response whose status and body are byte-for-byte those of a slug that names no workspace, in every workspace status and for every need.
- A task fork never grants more than its parent: a caller with level none on the parent has level none on the fork, even as the fork's creator.
- One readable set for cross-workspace reads, consistent with the per-workspace decision: a workspace is in a caller's set exactly when the caller's level on it is not none.
- A failure while deciding is `500 internal` and never a grant.
- A minimal workspace record (slug, owner, org, kind, parent, status) in the transactional store, so the rule has something to read before the workspace routes exist.

## Non-goals

- The error envelope, the `error_type` catalogue and the order of checks before and after the access step: package 02 `http_api_conventions`. This package produces its answers through that package's envelope and runs where step 7 of the check order places it.
- Authentication, the answers for missing, revoked or expired credentials and for blocked users: package 04 `authentication_and_login`.
- Users, organizations, memberships and their routes, including blocking and deleting an organization: package 05 `users_and_organizations`. This package only reads memberships and organization status.
- The scope registry and the PAT scope check that runs before the access step: package 06 `api_keys_and_access_tokens`.
- The binding guard, what a bound token reaches, and cutting the readable set down to the workspaces a binding covers (PLAT-FR-080 to PLAT-FR-087, the bound-PAT sentences of PLAT-FR-063): package 08 `workspace_bound_tokens`.
- The routes that create, read, update, archive and delete workspaces, slug validation, state preconditions, the full workspace record and the `access` member of the workspace response object: packages 10 `workspace_create_and_clone`, 11 `workspace_read_update_settings` and 14 `workspace_archive_reclone_delete`. This package neither updates nor deletes a workspace record.
- Creating task forks and deleting them: packages 15 `task_fork_creation` and 16 `task_fork_lifetime`.
- The git surface's own wording of a refusal: package 17 `git_smart_http`. That package asks this component for the decision.
- The conditions under which a member may change state (PLAT-FR-062): opening and editing changes from an own fork (package 22 `change_opening_and_versions` and package 23 `change_lifecycle_and_mergeability`), landing and reverting as author (packages 25 `optimistic_landing` and 29 `auto_land_revert_and_path_limits`), verdicts (package 28 `approvals_and_checks`) and claims (package 30 `awareness`). This package gives those routes the member's grant and the facts their conditions read; it does not evaluate the conditions.
- Which events and audit records a caller sees: packages 52 `durable_event_log_and_stream` and 51 `audit_query_and_retention`. They call the readable set.
- What the admin token may not do because it has no user (create workspaces, forks or tokens, record a verdict): the packages that own those routes. Here the admin token has level `admin` on every workspace.
- Any CLI command. This package has no routes and no commands of its own.

## Background

The repository holds no code yet; every file named here is new. The source is `docs/prd/03_platform.md`, §3.3.6 and PLAT-FR-060 to PLAT-FR-063, with WS-FR-003 and WS-FR-004 of `docs/prd/04_workspaces_and_forks.md` saying the same rule from the workspace side.

The rule is step 7 of the global check order (PLAT-FR-013): it runs after the path and method check, authentication, the rate limit, the body checks, the binding guard and the PAT scope check, and before request validation and state preconditions. Two consequences that callers observe follow from that order and are part of this package's behaviour: a caller without access gets `404` even when the body is invalid, and a member's write to an archived workspace is `403`, not `409`.

The code lives in `internal/access/`: `record.go` (the workspace record), `level.go` (level resolution), `decide.go` (the decision for a need), `middleware.go` (the HTTP answer) and `readable.go` (the readable set), each with its test file beside it. The schema migration that creates the workspace table is registered with `internal/store/`.

Until package 10 lands, no production route calls the component. Its end-to-end flows are therefore proved by tests in `internal/access/access_integration_test.go` that mount a probe route on the real router, against a real store.

## Requirements

### The minimal workspace record

The transactional store holds one record per workspace with six fields: `slug` (unique), `owner_id` (a user id), `org_id` (an organization id), `kind` (`repository` or `fork`), `parent_slug` (null for a repository, the parent's slug for a fork) and `status` (`pending`, `cloning`, `ready`, `failed` or `archived`). A record that is inserted is returned unchanged by a later read by slug, also after the store is closed and opened again.

An insert is refused, and nothing is written, when the slug already exists (duplicate-slug error) or when the record is invalid (invalid-record error): an empty slug, owner or org; a kind or status outside the lists above; a repository with a parent; a fork without a parent; a fork whose parent has no record or is itself a fork; a fork whose org differs from its parent's org.

A read of a slug that has no record reports "no such workspace" as a result, not as a failure. A lookup is by exact string match; a string that is not a legal slug has no record.

### Access levels

PLAT-FR-060 step 1 and §3.3.6. The level of a caller on a repository workspace is:

- `admin` for the admin token;
- `owner` for the user whose id is the record's owner, whether or not the user is also a member of the organization and whatever the organization's status;
- `member` for a user who is a member of the record's organization while that organization is `active`;
- none for everyone else.

A blocked organization's memberships grant nothing, and neither do memberships of an organization that no longer exists (PLAT-FR-049: its workspaces are kept and lose their member access). A user with role `admin` is not `admin` here: the role governs identity administration only, so such a user who is neither owner nor member has level none, with an API key as with a PAT. The credential type of a user (API key or PAT) does not change the level. The workspace's status does not change the level.

### Task forks

PLAT-FR-060 step 1 and WS-FR-004. On a task fork the level is `admin` for the admin token. For a user the hub first resolves the user's level on the fork's parent. If that is none, the level on the fork is none, even for the user who created the fork: a member who leaves the organization loses the forks they created in it. Otherwise the level is `owner` for the fork's owner and for the parent's owner, and `member` for every other member of the organization. A fork whose parent has no record gives level none to every user.

### The decision for a need

PLAT-FR-060 steps 2 to 4. Each workspace route declares one need. The component answers:

| Level | `read` | `write` | `owner` |
|---|---|---|---|
| `admin`, `owner` | granted | granted | granted |
| `member` | granted | `403 forbidden` | `403 forbidden` |
| none, or no such workspace | `404 not_found` | `404 not_found` | `404 not_found` |

The `404` carries `error_type` `not_found` and the message `workspace not found`. The `403` carries `error_type` `forbidden` and the message `org members have read-only access to this workspace`. On a refusal the route's handler does not run. On a grant the handler runs and can read the decision: the level as the string `admin`, `owner` or `member` (the value routes put in a workspace object's `access` member) and the workspace record.

A failure while deciding is `500` with `error_type` `internal` and never a grant: a store read that fails, a membership read that fails, a request that reaches the access step without an authenticated caller, or a route that declared a need other than the three.

### Anti-enumeration

PLAT-FR-061. A caller with level none gets exactly the same response for an existing workspace as for a slug that names no workspace: same status, same body bytes. This holds for every need (a stranger on a `write` route gets `404`, never `403`), for every workspace status, for repositories and forks, and for strings that are not legal slugs. So that the git surface (package 17) and other non-REST callers can keep the same promise, the component also gives the decision without an HTTP response; in that form a not-found outcome carries no record and no level, whether or not a record exists. A member's `403` is not a leak, because the member can already read the workspace.

### Member contributions

PLAT-FR-062. Some routes change state and are still open to members. Their owning packages declare them with need `read` plus a condition of their own. The rule does not look at the HTTP method: a member is granted on a `read` route whether the method is `GET`, `POST`, `PATCH` or `DELETE`. The granted decision gives the handler the caller's level and the record's owner, org, kind and parent, which are the facts those conditions read. Conditions that ask about a user other than the caller (for example, whether a fork's owner has write access to the fork's parent) use a write-access question the component answers for any user and workspace: yes exactly when that user's level is `owner`.

### The readable set

PLAT-FR-063. Cross-workspace reads use one set. For the admin token it is every workspace. For a user it is the workspaces the user owns, the forks of those workspaces, and the workspaces (forks included) of the user's active organizations, each with the caller's level; ownership wins over membership when both apply, and each workspace appears once. Workspaces of every status are in the set. A user with no workspaces gets an empty set, not an error. A failure while reading returns an error and no partial set.

The set agrees with the decision: a workspace is in a caller's set exactly when the caller's level on it is not none, and the level in the set is the level the decision resolves.

### End-to-end flows

1. A developer owns a workspace in an organization; an org member and an outsider exist. Each calls a route on that workspace that declares need `write`. `parleyd` authenticates each, resolves the level and answers the developer `200`, the org member `403 forbidden` and the outsider `404 not_found`, the outsider's response being identical to the one for a slug that names no workspace.
2. An org member owns a task fork of that workspace and reaches it with level `owner`. An operator removes the member from the organization. `parleyd` now answers the former member `404 not_found` on the fork and on the parent, and neither is in that user's readable set; the developer who owns the parent still has `owner` on the fork.

## Design Decisions

1. **Profile: full.** The rule is one cohesive feature with two end-to-end flows across the router, the access component, identity and the store.
2. **The workspace record's code lives in `internal/access`, its table in the store.** `internal/workspace` (package 10) calls the access component, so the component cannot import `internal/workspace` without an import cycle. Package 10 adds its columns to the same table with its own migration.
3. **No foreign keys from the record to users or organizations.** PLAT-FR-049 keeps the workspaces of a deleted organization, and PLAT-FR-048 does not touch the workspaces of a deleted user. A dangling `org_id` grants no membership; a dangling `owner_id` matches no caller.
4. **Memberships are read through a narrow interface.** The component asks one question of identity (package 05): the ids of the `active` organizations a user is a member of. The hub wires the real implementation in `internal/hub/access.go`; unit tests stub it.
5. **The caller is taken from the authenticated principal** that package 04 attaches to the request: the admin token, or a user id. A request with no principal at the access step is a wiring fault and is answered `500 internal`.
6. **A fork with a missing parent record gives level none to users.** The PRD is silent; it follows from "a fork never grants more than its parent", and a parent's deletion removes its forks in the same step (WS-FR-080), so the case is an inconsistency, answered by the safe side. The admin token keeps `admin`.
7. **A string that is not a legal slug is "no such workspace" (`404`), not `400`.** Slug validation at the access step would let a stranger tell legal from illegal names; PLAT-FR-061 asks for one answer.
8. **The readable set contains exactly the workspaces on which the level is not none.** PLAT-FR-063 says "the workspaces the user owns"; read literally that would include a fork whose creator has lost access to the parent, which PLAT-FR-060 hides from the same user. The decision wins, so lists never show a workspace that then answers `404`.
9. **The write-access question for a user answers yes only for level `owner`.** `admin` is a level of the admin token, which is not a user.
10. **The rule ignores the HTTP method and the workspace status.** Member contributions are declared as need `read` by their routes (PLAT-FR-062), and state preconditions come after access in the check order (PLAT-FR-013).
11. **Store: embedded SQLite through `modernc.org/sqlite`, behind `internal/store`** (project decision 2). **HTTP: the standard library `net/http` with its method-and-pattern router** (project decision 3); the slug is the route's path parameter `slug` (`/workspaces/:slug/…` in package 02's registration). **Tests: the standard `testing` package, table-driven, with `net/http/httptest`** (project decision 7).
12. **How the rule meets the pipeline.** The HTTP side is an access check installed in the access position of package 02 `http_api_conventions` (step 7), not a wrapper around a handler, so that it runs before the idempotency stage: a replayed create is still authorised. A workspace route declares its need (`read`, `write` or `owner`) with this package's need mark in package 02's route registration; a route without the mark is passed. The position holds a list of checks, so this check and the admin check of package 05 are both installed. Registration of the schema step, the membership source and the check happens on the hub's composition, in `internal/hub/access.go` (package 03). Package 52 `durable_event_log_and_stream` reads this package's workspace record from `internal/event`; `internal/access` therefore never imports `internal/event`.

## Dependencies

- Package 05 `users_and_organizations` (draft): users, organizations, memberships and organization status, which level resolution reads.
- Package 06 `api_keys_and_access_tokens` (draft): PATs and the scope check that runs immediately before the access step; a PAT caller's level is its user's level.
- Through them, package 04 `authentication_and_login` (the authenticated principal), package 02 `http_api_conventions` (the error envelope and check order) and package 03 `server_lifecycle_and_configuration` (the store and its migrations).

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires.
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
- **Bound token**: A PAT confined to one workspace (and the forks created with it).
- **Level**: What a caller is on one workspace: `admin`, `owner`, `member` or none (§3.3.6).
- **Need**: What a workspace route declares it requires: `read`, `write` or `owner`. `owner` marks routes that are not writes but are still private to the owner.
- **Readable set**: The workspaces a caller may see in cross-workspace reads (PLAT-FR-063).
- **Anti-enumeration**: The promise that a caller without read access cannot tell an existing workspace from a nonexistent one (PLAT-FR-061).
