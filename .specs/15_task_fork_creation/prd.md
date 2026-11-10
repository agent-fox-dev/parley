---
spec_id: "15"
spec_name: "task_fork_creation"
title: "Create a task fork in one call"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "9b6debf4e4b5e50373cc2c84e712e38ec327d24e04ef03bc7a83f60a872ad673"
schema_version: 3
source: "docs/prd/04_workspaces_and_forks.md"
---

# Create a task fork in one call

## Intent

An agent that starts a task needs its own disposable copy of a repository at once, whatever the size of that repository, and must not be able to disturb anyone else's work. Parley gives it a task fork: one call on a workspace returns a new workspace that is ready immediately, shares the parent's history without copying it, shows the parent's branches read-only and always current, takes the agent's work on branches of its own, and can come with a token that works only on that fork.

## Goals

- `POST /api/v1/workspaces/:slug/forks` answers `201` with a fork in `status: "ready"`, its git remote URL and, when asked, a token bound to the fork and a sandbox; the fork is usable from that moment.
- Creating a fork writes no git object: the fork's repository holds no object and no pack after creation, and every commit reachable from the parent's branches and tags can be read through the fork.
- Every branch the parent has at creation appears in the fork as a tracking branch at the parent's current head, with no sync call; the parent's tags appear the same way; branches the agent creates belong to the fork alone.
- A caller who can only read a workspace can fork it and owns the fork.
- Every refusal of fork creation has one status and one `error_type`, and leaves nothing behind: no record, no repository directory, no token, no event.
- A fork refuses writes with `409 parent_archived` while its parent is archived and stays readable.
- `GET /api/v1/workspaces/:slug/forks` lists a workspace's forks in pages, filtered by owner and by session.
- `parley fork create` and `parley fork list` drive the two routes from the shell.

## Non-goals

- A fork's lifetime after creation: `ttl` in `PATCH /workspaces/:slug` (WS-FR-071), the expiry sweep, early removal after landing (WS-FR-070), deletion of a fork and deletion with the parent (WS-FR-078 to WS-FR-080), `parley fork extend|delete`, the `parley.fork.expire` event, `[forks].expire_on_land` and the `parley_forks_active` metric: package 16 task_fork_lifetime. This package owns what happens to a lifetime at creation: the keys `[forks].default_ttl`, `[forks].max_ttl` and `[forks].max_per_workspace` with their defaults and the refusal of a configuration in which they contradict each other, the one rule that judges a `ttl`, and `expires_at` of a new fork.
- The access rule itself (levels, the `404 workspace not found`, that the parent's owner has `owner` on a fork, that a fork never grants more than its parent; WS-FR-003, WS-FR-004): package 07 workspace_access_rule. This package declares what its routes need and inserts fork records the rule reads.
- The binding guard, what a bound token reaches, the minting component and its `parley.token.bind` event, and revoking a fork's tokens: package 08 workspace_bound_tokens. Body validation of a token, the scope registry, the subset rule and the cover rule for `paths`: package 06 api_keys_and_access_tokens. This package calls the minting component and records the creating token on the fork.
- What the `sandbox` object means, its validation, the `sandboxes:write` scope it needs, the sandbox object and `parley.sandbox.create`: package 45 sandbox_creation_and_credentials. This package hands the object to the sandbox component and creates no fork when that component refuses.
- Sessions and the session-linking rule itself: package 49 agent_sessions_and_usage. This package asks that rule one question.
- Enforcing the tracking-branch refusal on a git push, on an API ref update and on an API commit, and advertising a fork's refs to a git client: packages 18 git_push_rules, 21 scm_write_api and 17 git_smart_http. Showing a fork's refs over the source control API: package 20 scm_read_api. They call the ref view, the write check and the parent-state check of this package.
- Opening a change when an own branch is pushed (`auto_change`, WS-FR-075): packages 19 git_push_to_change and 22 change_opening_and_versions. This package stores the flag and shows it.
- The other operations that do not apply to a fork (WS-FR-061): sync, reclone, push to origin, archive and reactivate in packages 12, 13 and 14; `org_id` and `settings` in an update in package 11; policy, sandbox rules and event webhooks in packages 27, 48 and 53. Of that table this package owns only "creating a fork of a fork".
- The workspace response object, `GET /workspaces`, `GET /workspaces/:slug`, the update of `display_name` and `description`, and a fork's `settings` (its parent's effective values): package 11 workspace_read_update_settings.
- The refusal of reclone while a workspace has forks (`has_forks`): package 14 workspace_archive_reclone_delete.
- How a fork's variables and secrets resolve: packages 39 secrets and 40 variables_and_resolution.
- Measuring that creation time does not grow with repository size (the bound of §15): package 54 operations_and_packaging. This package guarantees the mechanism: no object is copied.
- The error envelope, the order of checks before validation, list paging, the `Idempotency-Key` mechanism (PLAT-FR-010 to PLAT-FR-019): package 02 http_api_conventions. CLI settings, output, exit codes and path encoding (PLAT-FR-090 to PLAT-FR-095): package 09 cli_foundation.

## Background

The repository holds no code yet; every file named here is new. Source: `docs/prd/04_workspaces_and_forks.md`, §4.3.6, WS-FR-066 to WS-FR-069, WS-FR-072 to WS-FR-077 and WS-FR-082, with WS-FR-006, WS-FR-007, WS-FR-012 and WS-FR-061 where they speak of fork creation, the routes `POST` and `GET /workspaces/:slug/forks` of §4.5, `parley fork create|list` of §4.6, the `parley.workspace.fork` row of §4.8, the `[forks]` keys of §4.9 and the fork cases of §4.10.

What this package builds on: the workspace record and the access component of package 07 in `internal/access/`; the workspace store, the fork-name and duration validators, the state guard, the place of a repository copy and the workspace object of package 10 in `internal/workspace/`; the `git` wrapper in `internal/gitrepo/`; the minting component of package 08 in `internal/access/`; the router and helpers of package 02 in `internal/server/`; the CLI root and HTTP client of package 09 in `internal/cli/`; the event component of package 52 in `internal/event/`, used as package 10 uses it.

This package adds `internal/fork/` (the fork record's fields, the fork repository, the ref view, the parent-state check, the create, list and update handlers, each with its test file beside it), fork repository creation in `internal/gitrepo/`, the commands in `internal/cli/fork.go`, the wiring in `internal/hub/fork.go` and an end-to-end test in `test/e2e/fork_test.go`.

## Requirements

### Creating a fork (WS-FR-067, WS-FR-069, §4.3.6)

`POST /api/v1/workspaces/:slug/forks` creates a task fork of the workspace `:slug`, the parent. A PAT needs the scope `workspaces:write`. Read access to the parent is enough: its owner and the members of its organization may fork (PLAT-FR-062). A token bound to the parent may fork it. The body is a JSON object in which every field is optional; an absent body and `{}` are valid, a member whose value is `null` counts as absent, and unknown fields are ignored.

| Field | Type | Default | Rule |
|---|---|---|---|
| `name` | string | a generated id of 8 lower-case letters and digits | WS-FR-012: 1 to 32 characters; lower-case letters, digits and hyphens; starts and ends with a letter or digit; no `--` |
| `ref` | string | the parent's default branch | names a branch of the parent |
| `ttl` | duration | `[forks].default_ttl` (default `7d`) | WS-FR-013 form; at least 5 minutes and at most `[forks].max_ttl` (default `30d`) |
| `auto_change` | bool | `true` | |
| `token` | object | none | `{name?, permissions, paths?, expires?}` |
| `purpose` | string | `null` | at most 1024 characters |
| `session_id` | string | `null` | a UUID that satisfies the session-linking rule |
| `sandbox` | object | none | handed to the sandbox component |

The creation is synchronous. The answer is `201` with

```json
{ "workspace": { "…": "…" }, "remote": "<hub_url>", "token": { "…": "…" }, "sandbox": { "…": "…" } }
```

`workspace` is the fork's workspace object with `access: "owner"`. `remote` equals its `hub_url` (and is `null` when `[server].external_url` is not set). `token` and `sandbox` are present only when they were requested.

The fork is a workspace with: `slug` `<parent slug>--<name>`; `kind: "fork"`; `workspace_mode: "standard"`, also when the parent is a carry-patch workspace; `git_url`, `origin_head_sha`, `upstream_url`, `integration_branch` and `upstream_head_sha` `null`; `status: "ready"`; `owner_id` the caller's user id; `org_id` the parent's; `display_name` the slug; `description` `""`; `parent_slug`; `default_branch` equal to `ref`, the fork's base branch; `fork_point` `{ref, sha}`, the base branch and the parent's head of it at creation as a 40-character id, which never changes afterwards; `expires_at` the creation time plus `ttl`; `auto_change`, `purpose` and `session_id` as given or defaulted.

A generated name that is already in use is replaced by another generated name; the caller never sees a conflict for a name it did not choose (§4.10).

The call emits one hub event `parley.workspace.fork`, written with the fork (both are stored or neither): `workspace` and `resource_id` are the fork's slug, the actor is the caller, the event carries the fork's `session_id` when it has one, and its metadata is `parent_slug`, `fork_point`, `expires_at`, `auto_change`, `purpose` and, when a token was minted, `token_id`. A refused request emits no event.

The route accepts `Idempotency-Key` (PLAT-FR-015): a replay with the same key, caller and body within 24 hours answers `200` with the original body without the member `token.token`, and creates no second fork.

### Refusals of fork creation (WS-FR-006, WS-FR-007, WS-FR-061, WS-FR-072, §4.10)

Every refusal leaves the hub unchanged: no fork record, no repository directory, no token, no event.

| Case | Answer |
|---|---|
| the caller is the admin token (a fork needs a user as owner) | `403 forbidden` |
| the body is not a JSON object; `name` breaks WS-FR-012; `ref` is not a non-empty string; `ttl` is not a duration, is shorter than 5 minutes or longer than `[forks].max_ttl`; `auto_change` is not a boolean; `purpose` is not a string or is longer than 1024 characters; `token` or `sandbox` is not an object; `session_id` is not a UUID | `400 invalid_request` |
| `session_id` fails the session-linking rule | `400 unknown_session` |
| the parent is itself a fork | `400 not_supported_on_fork` |
| the parent is `pending` or `cloning`, or a reclone, archive or delete is running on it | `409 workspace_busy` |
| the parent is `failed` | `409 workspace_not_ready` |
| the parent is `archived` | `409 workspace_archived` |
| `ref` is not a branch of the parent: an unknown name, a tag or a commit id | `400`, `error_type` `fork_ref_not_found` |
| the parent already has `[forks].max_per_workspace` forks (default 200); deleted and expired forks do not count | `429 fork_limit_reached` |
| the given `name` is in use for this parent | `409 conflict` |
| the token is refused (below) | the token rules' answer |
| the sandbox component refuses | that component's answer |

When several cases hold, the answer is the first in the order of the table. Of two concurrent requests with the same `name`, exactly one answers `201` and the other `409 conflict`. Concurrent requests never bring a parent above its fork limit.

If the fork's repository or record cannot be written, the answer is `500 internal` and nothing remains.

### A token minted with the fork (WS-FR-076, PLAT-FR-081)

When the body has `token`, the hub creates, in the same step as the fork, a PAT owned by the caller and bound to the fork, and returns the `Token` object of §3 in `token`, with its secret in the member `token.token`. The secret is shown in this response only.

- `permissions` is a non-empty list of registered scopes.
- `paths` is an optional list of path globs that limits what the token may write.
- `expires` is an optional duration (WS-FR-013). With it, the token's `expires_at` is the creation time plus the duration. Without it, `expires_at` is `null` and the token lives as long as the fork.
- `name` defaults to `fork:<fork slug>`.

A `token` object that breaks these rules, or the token rules of §3 (an unregistered scope, an invalid glob), is `400 invalid_request`. If the caller authenticates with a PAT, it needs `tokens:manage` and must itself hold every requested scope, else `403 insufficient_scope`; if the caller's own PAT is path-limited, `paths` is required and each glob must be covered by one of the caller's, else `403 insufficient_scope`. In each of these cases no fork is created.

The hub records on the fork the id of the PAT that authenticated the request (nothing for an API key). A token bound to the parent therefore reaches the forks it created and no others.

### Session link and sandbox (WS-FR-077, WS-FR-082)

`session_id` is accepted when the session-linking rule of §14 holds for the caller and the parent: the session exists, was created by the caller's user, and its workspace is the parent or a fork of the parent. The session's status is not checked. The id is stored on the fork, shown in the fork's workspace object and carried on the fork's events. Otherwise the answer is `400 unknown_session`.

When the body has `sandbox`, the hub creates the fork and then hands the object, unread, to the sandbox component together with the caller, the fork, and the fork's `session_id` and `purpose` as defaults. The component's sandbox object is returned in `sandbox`. If the component refuses, the fork is not created either, and the answer is the component's error with its status, `error_type` and extra members unchanged. A hub without a sandbox component answers a request with `sandbox` `404`, `error_type` `sandboxes_disabled`.

### No copy (WS-FR-068, §4.3.6)

Creating a fork copies no history. After creation the fork's own repository holds no object and no pack, and the parent's repository is unchanged. Every commit reachable from the parent's branches and tags can be read through the fork at once, including commits the parent receives after the fork was created. What an agent writes to the fork is stored in the fork alone: an own branch and its new commits are not visible in the parent, and the storage a fork uses grows only with what is written to it.

### The fork's refs (§4.3.6, WS-FR-073, WS-FR-074, §4.10)

The hub gives every component that shows or serves a fork one view of its refs:

- **Tracking branches.** Every branch that exists in the parent when the fork is created is a tracking branch of the fork. Its head in the view is the parent's current head of the branch of the same name; when the parent's branch moves, the view moves with it. While the parent has no branch of that name, the tracking branch is absent from the view. Branches created in the parent later do not appear.
- **Tags.** The parent's tags that exist at creation are visible under the same rules.
- **Own branches.** Branches created in the fork, each marked as not tracking. A branch the parent creates later under a name the fork already uses changes nothing in the fork.

A tracking branch cannot be written in the fork: a component that asks whether a branch of a fork may be created, moved or deleted gets, for the name of a tracking branch, the refusal `403` with `error_type` `tracking_branch`, also while the parent has no branch of that name.

A fork's `head_sha` is the parent's current head of the fork's base branch, computed when the fork is read, and `null` when the parent no longer has that branch. `fork_point.sha` does not change.

### A fork follows its parent's status (WS-FR-066)

A fork is always `ready`. Components that act on a fork ask one check, with the kind of use:

| The parent | Read (fetch, API read) | Write (push, API write, update of the fork) |
|---|---|---|
| `ready` | allowed | allowed |
| `archived` | allowed | `409 parent_archived` |
| `pending` or `cloning`, or an archive or delete is running on it | `409 workspace_busy` | `409 workspace_busy` |
| `failed` | `409 workspace_not_ready` | `409 workspace_not_ready` |

Deleting a fork and its expiry are not subject to the check (package 16).

### Updating a fork's own fields (§4.5 `PATCH /workspaces/:slug`)

In an update of a fork, `purpose` takes a string of at most 1024 characters or `null`, which clears it, and `auto_change` takes a boolean. `auto_change: null`, a value of another type and a longer `purpose` are `400 invalid_request` and change nothing. The answer is `200` with the fork's workspace object; `parley.workspace.update` lists the given names in `metadata.fields` and carries the fork's `session_id`. Every update of a fork, of whichever field, is a write under the table above: `409 parent_archived` while the parent is archived.

### Listing forks (§4.5 `GET /workspaces/:slug/forks`)

`GET /api/v1/workspaces/:slug/forks` needs read access to `:slug` and, for a PAT, `workspaces:read` or `workspaces:write`. It answers `200` with `{"items": [workspace…], "next_cursor": …}`: the forks of the workspace as workspace objects with the caller's `access`, ordered by `created_at` descending, then `slug` ascending, with `limit` and `cursor` as on every list. `owner=me` keeps only the forks the caller owns. `session_id=<id>` keeps only the forks created with that session. Called with a fork's slug the list is empty. The list works in every status of the parent. For a workspace-bound token it holds only the forks the binding covers. An `owner` other than `me` and a `session_id` that is not a UUID are `400 invalid_request`.

### Fork limits in configuration (§4.9 `[forks]`, WS-FR-069, WS-FR-072)

| Key | Default | Rule |
|---|---|---|
| `[forks].default_ttl` | `7d` | a duration of at least 5 minutes |
| `[forks].max_ttl` | `30d` | a duration not shorter than `default_ttl` |
| `[forks].max_per_workspace` | `200` | the number of existing forks a parent may have |

A key that is omitted takes its default. A value that is not a duration, a `default_ttl` shorter than 5 minutes or a `max_ttl` shorter than `default_ttl` (equal is accepted) makes the configuration load fail with an error that names the key (`forks.default_ttl` or `forks.max_ttl`); `parleyd` then writes one error line and exits with status 1 without listening.

One rule judges every `ttl`: a positive whole number followed by one unit `m`, `h` or `d` (WS-FR-013), at least 5 minutes and at most `[forks].max_ttl`; `expires_at` is the given instant plus the `ttl`, in whole seconds UTC. Creation applies it with the creation time; package 16 applies the same rule with the request time when a `ttl` is updated.

### Commands (§4.6)

`parley fork create <parent>` calls `POST /workspaces/:slug/forks` and prints the response.

| Flag | Sent as |
|---|---|
| `--name <name>` | `name` |
| `--ref <branch>` | `ref` |
| `--ttl <duration>` | `ttl` |
| `--no-auto-change` | `auto_change: false` |
| `--token-permissions <list>` | `token.permissions`, split at commas; giving the flag is what requests a token |
| `--token-paths <globs>` | `token.paths`, split at commas |
| `--purpose <text>` | `purpose` |
| `--session <id>` | `session_id` |
| `--sandbox-profile <name>` | `sandbox.profile`; giving the flag is what requests a sandbox |
| `-- command...` | `sandbox.command`, the trailing arguments as a list |

A flag that is not given sends no member. `--token-paths` without `--token-permissions`, and a trailing command without `--sandbox-profile`, are usage errors: exit 2, nothing sent. When the response carries a token secret, the command says on stderr that it is printed this once only. Exit codes are 0, 1 and 2.

`parley fork list <parent>` calls `GET /workspaces/:slug/forks` with `--mine` (sends `owner=me`), `--session <id>`, `--limit` and `--cursor`, and prints the list page.

### End-to-end flows

1. **Fork with a token.** An agent runs `parley fork create demo --name task1 --token-permissions git:read,git:write`. The CLI posts to `parleyd`, which checks the caller, creates the fork's repository without copying an object, writes the fork, the bound token and the `parley.workspace.fork` event in one step and answers `201`. The CLI prints the fork, its remote and the token and warns on stderr. The agent reads the fork with the token; the same token gets `404` on another workspace. The developer who owns `demo` finds the fork with `parley fork list demo`.
2. **A member's fork follows the parent.** An org member who can only read `demo` posts to `/workspaces/demo/forks` and receives a fork they own. The parent's default branch then moves. `parleyd` shows the fork with the new `head_sha` and the old `fork_point.sha`, and the ref view shows the tracking branch at the parent's new head.

## Design Decisions

1. **Profile: full.** Two end-to-end flows cross the CLI, the router, the access component, the fork component, the store and git.
2. **Project-wide choices relied on.** Go; the hub runs the `git` command through `internal/gitrepo`, with no git library; repositories are bare repositories on disk; the store is embedded SQLite behind `internal/store`; HTTP is `net/http`; the CLI uses `github.com/spf13/cobra`; tests are table-driven with `testing` and `net/http/httptest`, on repositories made with `git init` in `t.TempDir()`.
3. **How a fork shares history.** A fork's repository is an empty bare repository whose alternates file names the parent's object directory. Nothing is copied, later commits of the parent are readable at once, and what is pushed to the fork is stored in the fork. Its place comes from `internal/workspace`, like every copy.
4. **Tracking branches are names, not refs.** The fork's record keeps the names of the parent's branches and tags at creation; the ref view resolves them in the parent on every read. The fork's repository holds its own refs and, once it has been served over git, a copy of the tracking refs and of the version refs `refs/parley/changes/*` of its changes, which package 17 fetches from the parent before stock git runs; that copy is a cache and the ref view stays the one answer to which refs a fork has. Every own-branch listing of this package therefore leaves out the recorded tracking-branch names, so that a copied tracking ref is never shown as an own branch, also while the parent has no branch of that name. Recording the names costs time in the number of the parent's refs, not in its history; WS-FR-068 is read as speaking of history.
5. **A tracking name stays reserved.** While the parent has no branch of that name the tracking branch is absent, the name still cannot be used for an own branch, and the tracking branch is back when the parent has the branch again. The PRD says only that it disappears.
6. **Order of refusals** as in the table of "Refusals of fork creation". The PRD fixes validation before state (PLAT-FR-013) and is silent beyond that. `ref` is checked after the parent's state because the check reads the parent's repository. The token and the sandbox are created inside the fork's transaction, so their refusals come last and roll everything back.
7. **The admin token is refused at the access step**, before validation: `403 forbidden` whatever the body.
8. **Unknown body fields are ignored**, as in workspace creation (WS-FR-014), **and a `null` member counts as absent**, because `purpose` and `session_id` are shown as `null` and a client may send back what it read. The members of `token` are passed to the minting component of package 08, which validates them; `expires` is converted here to an instant.
9. **A token's `expires` may exceed the fork's `ttl`.** The token is revoked with the fork in any case.
10. **Session linking goes through the interface `SessionLinker`, declared in `internal/fork`** with two operations: a check (caller, session id, parent slug; accepted with the lower-case id, refused or failed) and a record (the fork's open transaction, the session id, the parent's slug and the fork's slug). Package 49 registers its `session.Check` and `session.Record` with it from `internal/hub/session.go` (link kind `fork`); this package does not depend on package 49. The check is called at fork creation, at its place in the order of refusals; the record is called inside the creation transaction, so the link is stored with the fork or not at all. Without a registered linker every `session_id` is `400 unknown_session`, because no session exists.
11. **The sandbox is asked through a narrow interface**, called once inside the fork's transaction after the fork and its token are written and the fork's repository exists. Package 45 registers its component from `internal/hub/sandbox.go`; without one the answer is `404 sandboxes_disabled`. The `sandboxes:write` scope of WS-FR-082 is checked by that component with the rest of the sandbox request, so that the answer is "the sandbox route's error".
12. **A generated name is tried at most 8 times**; after that the answer is `500 internal`. With 36^8 names this is not reached in practice, and a loop needs a bound.
13. **Leftovers.** The repository directory is made before the transaction and removed when the transaction fails. A directory found at a new fork's place with no fork record is discarded and replaced.
14. **`remote` is `null` when `[server].external_url` is not set**, like `hub_url`.
15. **A `failed` parent** makes the fork answer `409 workspace_not_ready`, as WS-FR-007 does for the workspace itself; WS-FR-066 names only `archived` and busy.
16. **The list of forks holds, for a bound token, only the forks its binding covers**, so that it never shows a workspace that then answers `404`. An admin token with `owner=me` gets an empty list.
17. **Lengths count Unicode code points**, as in package 11.
18. **`purpose` and `auto_change` in an update** go through package 11's update handler, which is a dependency: this package registers their handlers with `RegisterForkField` and the parent-state check with `RegisterForkUpdateGuard`, and edits no file of package 11. In `metadata.fields` they come last in package 11's fixed order (`display_name`, `description`, `org_id`, `settings`, `ttl`, `purpose`, `auto_change`).
19. **The lifetime at creation is this package's; extension, expiry and early removal are package 16's.** This package registers `[forks].default_ttl`, `[forks].max_ttl` and `[forks].max_per_workspace`, refuses a contradictory configuration, and owns `ResolveTTL` in `internal/fork/lifetime.go`, the one rule for a `ttl`, which sets `expires_at` at creation. Package 16 calls `ResolveTTL` for `ttl` in `PATCH` (WS-FR-071) and owns `[forks].expire_on_land`. Why here: creation needs the default and the bounds, and this package is built first.
20. **The fork view.** A fork's computed `head_sha` reaches the workspace object through `RegisterForkView` of package 10's `RenderWorkspace`.
21. **Composition.** The fork component and its optional collaborators are wired on the composition value of `internal/hub` (package 03), in `internal/hub/fork.go`; nothing is wired in `cmd/parleyd`.
22. **The fork token is minted only through `MintBound` of package 08.** The create handler calls `MintBound(ctx, tx, input)` of `internal/access` with the fork's transaction, the caller as owner, the authenticating credential as creator and the fork's slug as the binding. It never calls the minting operation of package 06 directly and writes no token row and no `parley.token.bind` event itself; validation of permissions, paths and expiry, the subset rule and that event are `MintBound`'s, and its API error is answered unchanged.

## Dependencies

- Package 10 workspace_create_and_clone (draft): the workspace store and record, the fork-name and duration validators, the state guard and maintenance marker, the place of a repository copy, the workspace object, the `git` wrapper and the event component as it uses it.
- Package 08 workspace_bound_tokens (draft): `MintBound`, the minting component that creates the fork-bound token inside the fork's transaction, the creating-token field on the workspace record, and the guard that lets a token bound to the parent reach the forks it created.
- Package 11 workspace_read_update_settings (draft): the update handler with `RegisterForkField` and `RegisterForkUpdateGuard`, the workspace object and `GET /workspaces/:slug`.
- Not dependencies: packages 49 and 45 implement the two narrow interfaces of this package (`SessionLinker` and the sandbox component) and register with them; package 17 copies tracking and version refs into the fork's repository; package 16 extends, expires and removes the forks created here.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires: after 7 days by default, or 24 hours after all its changes have landed or been abandoned.
- **Parent**: The repository workspace a fork was created from.
- **Base branch**: The parent branch a fork was taken from; the fork's `default_branch`.
- **Fork point**: The base branch and the parent's head of it when the fork was created.
- **Tracking branch**: In a fork, a branch that existed in the parent at fork time. It is read-only in the fork and always equals the parent's branch.
- **Own branch**: A branch created in a fork. It belongs to the fork alone and is not visible in the parent.
- **Change**: The unit of work: a proposal to add the commits of a source branch to a target branch, with a number, a title, an intent, versions and a state.
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
- **Bound token**: A PAT confined to one workspace (and the forks created with it). A token bound to a fork also reaches its own changes, and the awareness and policy reads, on the fork's parent (§3).
- **Path-scoped token**: A PAT whose writes are limited to files matching a list of globs.
- **Sandbox**: An isolated environment in which an agent runs, attached to a workspace (normally a task fork) and started with it checked out.
- **Session**: An agent session: who ran, on which workspace, with what token usage, and which changes it produced.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number.
