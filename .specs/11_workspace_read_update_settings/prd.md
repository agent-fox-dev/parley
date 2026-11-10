---
spec_id: "11"
spec_name: "workspace_read_update_settings"
title: "Read and change a workspace and its settings"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "4d262848495f7223ae344b445784cef4777f14caee7e76e1d3d0e5ec11f9af42"
schema_version: 3
source: "docs/prd/04_workspaces_and_forks.md"
---

# Read and change a workspace and its settings

## Intent

A user who has registered a repository needs to see the workspaces they may see, read one in full, and change what is changeable about it: its display name, its description, its organization and its settings. This package gives every workspace one response object, one settings document with a fixed set of keys, types, defaults and validation rules, and one update call that changes only what the caller sends, so that every later feature reads a workspace's behaviour from the same place.

## Goals

- `GET /api/v1/workspaces` answers with the workspaces the caller can read, each with the caller's access level, hiding archived workspaces and forks unless asked, ordered by `created_at` descending then `slug` ascending, in pages.
- `GET /api/v1/workspaces/:slug` answers with one workspace object in every status, and with the same `404` for an unknown slug and for a workspace the caller cannot read.
- Every route that returns a workspace returns the same object: every stored field (a nullable one as `null`, never left out), `org_slug`, `hub_url`, `access` and a `settings` object that holds all 21 setting keys with their effective values.
- `PATCH /api/v1/workspaces/:slug` changes `display_name`, `description`, `org_id` and `settings`, refuses every immutable field with `400`, refuses an archived workspace with `409 workspace_archived`, and records `parley.workspace.update` with the list of fields given.
- A `settings` value is validated key by key against a fixed table and applied all-or-nothing; an error names the first invalid key.
- Moving a workspace to another organization moves its forks in the same step.
- `parley workspace list`, `parley workspace get <slug>` and `parley workspace update <slug>` drive those routes, with `--setting key=value` parsed by one fixed rule.

## Non-goals

- Creating a workspace, the git credential, clone work and the status state machine: package 10 `workspace_create_and_clone`. This package only supplies the settings rules that create applies to its `settings` field.
- The access rule itself (levels, the readable set, anti-enumeration) and the scope check: package 07 `workspace_access_rule` and package 06 `api_keys_and_access_tokens`. This package declares what each of its routes needs and calls them.
- Cutting the list down for a workspace-bound token (the second sentence of WS-FR-028, WS-FR-005) and the binding guard: package 08 `workspace_bound_tokens`, which narrows the same readable set this package's list reads.
- What the settings do. `sync_enabled`: package 12 `workspace_sync_from_origin`. `protected_branches` and `protect.owner_push`: package 18 `git_push_rules`, which matches globs with the matcher of this package (Design Decisions, 15). `land.*`: package 25 `optimistic_landing`. `changes.*`: packages 22 and 24. `audit_fetches`: package 17 `git_smart_http`. `forge_api_url`, `upstream_forge_api_url`: package 41 `forge_reads`. The carry-patch keys: packages 31 to 38. This package defines the keys, types, defaults and validation only.
- Sync, push to origin, archive, reactivate, reclone and delete: packages 12, 13 and 14.
- Creating forks, `GET /workspaces/:slug/forks`, a fork's computed `head_sha`, the `409 parent_archived` refusal of a write to a fork whose parent is archived (WS-FR-066), `purpose` and `auto_change` in an update of a fork, and a fork's `session_id` on its events: package 15 `task_fork_creation`. `ttl` in an update of a fork (WS-FR-071): package 16 `task_fork_lifetime`. This package handles only what a fork shares with a repository workspace: it is listed and read, its `display_name` and `description` can be changed, and a change of its `org_id` or `settings` is refused.
- The error envelope, the order of checks, list paging and cursors (PLAT-FR-010 to PLAT-FR-019): package 02 `http_api_conventions`. CLI output, exit codes and the HTTP client (PLAT-FR-090 to PLAT-FR-095): package 09 `cli_foundation`.
- Storing, ordering and streaming events: package 52 `durable_event_log_and_stream`. This package emits one event through it.

## Background

The repository holds no code yet; every file named here is new. The workspace record, its store and `POST /workspaces` arrive with package 10 in `internal/workspace/`; the access component with package 07 in `internal/access/`; the event component with package 52 in `internal/event/`; the router, error envelope and list helpers with package 02 in `internal/server/`; the CLI root, HTTP client and output rules with package 09 in `internal/cli/`.

This package adds to `internal/workspace/`: the response object (`response.go`), the settings document (`settings.go`), the read handlers (`read.go`) and the update handler (`update.go`), each with its test file beside it, and to `internal/cli/workspace.go` the commands `list`, `get` and `update`. An end-to-end test in `test/e2e/workspace_read_update_test.go` starts a real hub in-process and drives it over HTTP and through the CLI.

Source: `docs/prd/04_workspaces_and_forks.md`, WS-FR-028 to WS-FR-035, §4.3.2, §4.3.3, the routes `GET /workspaces`, `GET /workspaces/:slug` and `PATCH /workspaces/:slug` of §4.5, the three commands of §4.6 and the `parley.workspace.update` row of §4.8.

## Requirements

### The workspace response object (§4.3.2)

Every route that returns a workspace returns one object. It holds every field of the workspace record (§4.3.1): `slug`, `kind`, `workspace_mode`, `git_url`, `default_branch`, `owner_id`, `org_id`, `status`, `display_name`, `description`, `head_sha`, `origin_head_sha`, `clone_error`, `last_sync_at`, `sync_error`, `upstream_url`, `integration_branch`, `upstream_head_sha`, `parent_slug`, `fork_point`, `expires_at`, `auto_change`, `purpose`, `session_id`, `settings`, `created_at`, `updated_at`. A field without a value is present with the value `null`; no field is ever left out.

It also holds four computed fields:

- `org_slug`: the slug of the workspace's organization.
- `hub_url`: `<server.external_url>/git/<org_slug>/<slug>.git`, with one trailing `/` of `external_url` removed first; `null` when `server.external_url` is not set.
- `access`: the caller's level, `admin` for the admin token, `owner` or `member`. It describes the caller, not the scopes of the token used.
- `settings`: every setting key with its effective value (below).

### Reading one workspace

`GET /workspaces/:slug` needs read access and, for a PAT, the scope `workspaces:read` or `workspaces:write` (WS-FR-002). It answers `200` with the workspace object, for a repository workspace and for a fork, in every status: `pending`, `cloning`, `ready`, `failed` and `archived` (WS-FR-059). A caller with no read access gets HTTP 404 with `error_type` `not_found` and the message `workspace not found`, with the same status and body as for a slug that does not exist (WS-FR-003). A PAT without either scope gets HTTP 403 with `error_type` `insufficient_scope`, whether or not the workspace exists.

### Listing workspaces (WS-FR-028, WS-FR-029)

`GET /workspaces` needs the scope `workspaces:read` or `workspaces:write` for a PAT. It answers `200` with `{"items":[workspace…],"next_cursor":…}`.

- The list holds the workspaces the caller can read: the caller's own workspaces and the forks of those workspaces with `access: "owner"`; the workspaces of the caller's active organizations with `access: "member"`, where ownership wins when both apply; every workspace with `access: "admin"` for the admin token. A caller who can read nothing gets `items: []`.
- Archived workspaces are left out unless `include_archived=true`.
- Forks are left out unless `include_forks=true` or `parent` is given.
- `parent=<slug>` keeps only the forks of that workspace. A parent the caller cannot read, a parent that does not exist and a parent that is itself a fork all give `200` with an empty list.
- The order is `created_at` descending, then `slug` ascending. `limit` and `cursor` page through it; following `next_cursor` until it is `null` returns every item exactly once.
- `include_archived` or `include_forks` with a value other than `true` or `false` is HTTP 400 with `error_type` `invalid_request`.

### The settings document (§4.3.3)

Settings are a flat document of named keys. A key that was never set has its default. A response always shows every key with its effective value.

| Key | Type | Values | Default |
|---|---|---|---|
| `sync_enabled` | bool | | `true` |
| `protected_branches` | list of string | 0 to 50 branch globs | `[<default branch>]` |
| `protect.owner_push` | bool | | `true` |
| `land.strategy` | enum | `rebase`, `squash` | `rebase` |
| `land.by_author` | bool | | `true` |
| `land.delete_source` | bool | | `true` |
| `land.allow_override` | bool | | `true` |
| `changes.require_intent` | bool | | `true` |
| `changes.auto_restack` | bool | | `true` |
| `audit_fetches` | bool | | `false` |
| `forge_api_url` | URL string | `""` or an absolute `https://` URL | `""` |
| `upstream_forge_api_url` | URL string | same | `""` |
| `patch_branch_source` | enum | `hub`, `origin` | `hub` |
| `patch_divergence_policy` | enum | `replace`, `report` | `replace` |
| `push_patches_to_origin` | bool | | `false` |
| `auto_rebuild_after_push` | bool | | `true` |
| `auto_rebuild_after_sync` | bool | | `true` |
| `rebuild_strategy` | enum | `rebase`, `merge` | `rebase` |
| `rebuild_fail_mode` | enum | `fail_fast`, `continue` | `fail_fast` |
| `rebuild_push_integration_branch` | bool | | `false` |
| `squash_merge_detection` | enum | `ancestry_only`, `content_based`, `both` | `both` |

The default of `protected_branches` follows the workspace's `default_branch`. While `default_branch` is still `null` the effective value is `[]`. Once the key is set explicitly, including to `[]`, the stored list is used as it is.

A fork has no settings of its own. Its `settings` shows the parent's effective values, and follows them when the parent's change.

### Applying a settings value

The same rules apply to the `settings` field of `POST /workspaces` and of `PATCH /workspaces/:slug`:

- `settings: null` returns every key to its default.
- Otherwise the value must be a JSON object, else `400`.
- An unknown key is `400` with a message that contains `unknown setting "<key>"`.
- A key with value `null` returns to its default.
- A bool key needs a JSON boolean. An enum key needs one of the listed strings.
- A URL key needs a JSON string that, after trimming surrounding whitespace, is empty or an absolute `https://` URL with a host and without userinfo, query or fragment. The trimmed value is stored.
- `protected_branches` needs a JSON array of at most 50 distinct strings. Each is a branch glob of 1 to 255 characters: a valid branch name (WS-FR-011) in which `*` and `**` may also appear. A branch glob is matched against a whole branch name, case-sensitive: `*` matches any run of characters except `/`, `**` any run including `/`, and every other character itself (§8.3.1). This package provides that matcher for every package that reads `protected_branches` or another list of branch globs.
- Keys not given keep their current value (on create: the default).
- The request is all-or-nothing: one invalid key and no key changes. When several keys are invalid, the error names the first invalid key in the order of the table above.

Every one of these failures is HTTP 400 with `error_type` `invalid_request`; on create nothing is created, on update nothing changes.

### Updating a workspace (WS-FR-030 to WS-FR-034)

`PATCH /workspaces/:slug` needs write access (the owner or the admin token) and, for a PAT, the scope `workspaces:write`. It answers `200` with the workspace object after the update.

- The mutable fields of a repository workspace are `display_name`, `description`, `org_id` and `settings`. The body must be a JSON object with at least one of them. Unknown fields are ignored. Only the fields given change.
- `display_name`: a string of at most 128 characters; `null` or `""` resets it to the slug. `description`: a string of at most 1024 characters; `null` resets it to `""`.
- An immutable field in the body is refused with `400`, whatever its value: `slug`, `kind`, `git_url`, `default_branch`, `owner_id`, `workspace_mode`, `upstream_url`, `integration_branch`, `parent_slug`, `fork_point`.
- The fork-only fields `ttl`, `purpose` and `auto_change` on a repository workspace are refused with `400`.
- On a fork, `display_name` and `description` are updated the same way; `org_id` or `settings` is refused with HTTP 400 and `error_type` `not_supported_on_fork` (WS-FR-061).
- An archived workspace cannot be updated: HTTP 409 with `error_type` `workspace_archived`. A workspace in any other status can.
- A member of the workspace's organization gets HTTP 403 `forbidden`, also when the workspace is archived; a PAT without `workspaces:write` gets HTTP 403 `insufficient_scope`; a caller with no read access gets HTTP 404 `not_found`.

A refused update changes nothing, `updated_at` included.

### Moving a workspace to another organization (WS-FR-032, WS-FR-033)

`org_id` in an update names the target organization. `null` moves the workspace to its owner's personal organization. For any other value the workspace's owner must be a member of the target organization, else HTTP 403 `forbidden`; this is checked against the owner, not the caller, so it also holds for the admin token. A value that names no organization is HTTP 400 `invalid_request`. The move changes `org_id`, `org_slug` and `hub_url` of the workspace and, in the same step, of every one of its forks. Afterwards the members of the new organization can read the workspace and the members of the old one cannot.

### The update event (WS-FR-035, §4.8)

A successful update emits one hub event `parley.workspace.update` with `workspace` and `resource_id` set to the slug, `resource_type` `workspace`, the caller as actor, and `metadata.fields`: the names of the mutable fields given in the body. For a fork, `metadata.parent_slug` names the parent. The event is written with the update: both are stored or neither. A refused update emits no event. The event never carries a setting value or a credential.

### CLI (§4.6)

The commands follow the conventions of PLAT-FR-090 to PLAT-FR-095: JSON on stdout, the error envelope on stderr, exit codes 0, 1 and 2. A slug is sent as one percent-encoded path segment.

- `parley workspace list` with `--include-archived`, `--include-forks`, `--parent <slug>`, `--limit`, `--cursor` calls `GET /workspaces` and prints the list page.
- `parley workspace get <slug>` calls `GET /workspaces/:slug` and prints the workspace.
- `parley workspace update <slug>` with `--display-name`, `--description`, `--org`, `--clear-display-name`, `--clear-description`, `--clear-org` and `--setting key=value` (repeatable) calls `PATCH /workspaces/:slug` and prints the updated workspace. A clear flag sends the field as `null` and wins over the matching value flag. Without any of these flags the command is a usage error: exit 2, nothing sent.
- `--setting key=value` is split at the first `=`. A missing `=` or an empty key is a usage error (exit 2, nothing sent). The value `true` or `false` is sent as a boolean. An empty value is sent as `null`. For `protected_branches` the value is split at commas into a list. Anything else is sent as a string. A repeated key: the last one wins.

### End-to-end flows

1. A developer runs `parley workspace update demo --description "payments service" --setting land.strategy=squash`. The CLI sends `PATCH /workspaces/demo`; `parleyd` checks access, validates the body, stores the two fields and the event in one step and answers `200`; the CLI prints the workspace. The developer runs `parley workspace get demo` and sees the new description, `settings["land.strategy"]` equal to `squash` and every other key at its default.
2. A developer moves a workspace into an organization with `PATCH /workspaces/demo {"org_id":"acme"}`. `parleyd` checks that the owner is a member of `acme`, moves the workspace and its forks and records the event. A member of `acme` calls `GET /workspaces` and finds `demo` with `access: "member"` and a `hub_url` under `acme`; the same member's `PATCH` is answered `403 forbidden`.

## Design Decisions

1. **Stack.** Go; HTTP on the standard library `net/http` router; the store is embedded SQLite through `modernc.org/sqlite` behind `internal/store`; the CLI uses `github.com/spf13/cobra`; tests use `testing` with tables and `net/http/httptest`. These are project-wide decisions.
2. **Only explicitly set keys are stored.** The store keeps the keys a caller set; the effective document is computed at read time from that and the defaults. This is what makes the default of `protected_branches` follow `default_branch` and lets a fork show its parent's values.
3. **`org_id` in an update takes an organization id or slug**, as in create (WS-FR-018); the source is silent for update. A value that is not a string or `null`, or names no organization, is `400 invalid_request`, as in create.
4. **Order of refusals inside one update.** Body shape and field values first (`400`), then the owner's membership of the target organization (`403`), then the archived state (`409`). The source fixes validation before state (PLAT-FR-013) and is silent on where the membership check sits; it reads other records, so it comes after the checks that read only the body.
5. **Order of the `400` checks.** Not a JSON object; an immutable field; a field that does not apply to the workspace's kind (`not_supported_on_fork` for `org_id` or `settings` on a fork, `invalid_request` for a fork-only field on a repository workspace); no mutable field; then the values in the order `display_name`, `description`, `org_id`, `settings`. The first failure decides the answer.
6. **Unknown setting keys are reported before invalid values**, and of several unknown keys the first in byte order of the key. The source orders only the keys of the table.
7. **Lengths count Unicode code points**, for `display_name`, `description` and a branch glob.
8. **`include_archived` and `include_forks` accept exactly `true` and `false`**; any other value is `400 invalid_request`. The source names only `true`; refusing a misspelling is simpler to rely on than ignoring it.
9. **An update that changes no value still succeeds**, sets `updated_at` and emits the event: the source ties the event to a successful update, not to a difference.
10. **`metadata.fields` lists the mutable field names given, in the fixed order `display_name`, `description`, `org_id`, `settings`**, so that the event does not depend on the order of the members of the body.
11. **`--setting protected_branches=<value>` with a non-empty value is always split into a list**, also when the value is `true` or `false` (those are valid branch names); an empty value is sent as `null` for every key, so the CLI cannot set the empty list.
12. **Package 10 declares the settings registry, this package fills it.** This package depends on package 10, so package 10 cannot call it. Package 10 declares the mechanism in `internal/workspace/settings.go` as a `SettingsRegistry` (`RegisterSetting`, `ApplySettings`, `EffectiveSettings`) and its create handler applies `settings` with `ApplySettings`. This package registers the 21 keys with `RegisterSetting` (through `RegisterSettingKeys`, which the hub's composition calls), owns their types, defaults and validation rules and the choice of the key an error names, and changes nothing in the create handler.
13. **Fork-only fields in an update of a fork are left to packages 15 and 16.** The update handler keeps its table of fields per kind in one place and exports `RegisterForkField` (package 15 registers `purpose` and `auto_change`, package 16 `ttl`) and `RegisterForkUpdateGuard` (package 15 registers the parent-state check of a fork), so that those packages change no rule written here. `metadata.fields` of the event keeps one fixed order for all seven names: `display_name`, `description`, `org_id`, `settings`, `ttl`, `purpose`, `auto_change`.
14. **One response encoder.** `RenderWorkspace` of package 10 is the only encoder of the workspace object; this package owns the object's definition and completes that function.
15. **One branch-glob matcher.** `internal/workspace/branchglob.go` (`MatchBranchGlob`, `MatchAnyBranchGlob`, `ValidateBranchGlob`) is the only matcher of branch globs in the repository. It is here because this package validates `protected_branches`, and it is the first package built that needs the rule of §8.3.1. Package 13 (forced push of a protected branch), package 18 (push rules) and package 27 (policy `targets`) call it.
16. **`parley workspace create --wait`.** Package 10's wait polls `GET /workspaces/:slug` of this package; the end-to-end test here runs it against a real hub.
17. **One `--setting` parser.** `ParseSettingFlags` of package 10; `parley workspace update` calls it.

## Dependencies

- Package 10 `workspace_create_and_clone` (draft): the workspace record, its store, `POST /workspaces` and the branch-name rule of WS-FR-011 that the glob validation reuses. Through it this package uses package 07 (access levels and the readable set), package 52 (event writing), package 02 (router, error envelope, list paging) and package 09 (CLI root and HTTP client).

## Verified External API

- `github.com/spf13/cobra`: `cobra.Command`, `(*cobra.Command).Flags`, `(*pflag.FlagSet).StringArray`, `(*pflag.FlagSet).Changed`. **Unverified**: the module is not yet in `go.mod`; the signatures are the ones the library documents.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Origin**: A registered workspace's remote (`git_url`).
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires: after 7 days by default, or 24 hours after all its changes have landed or been abandoned.
- **Protected branch**: A branch that moves only by landing a change, by sync from origin, or by a direct fast-forward push from the workspace's owner or the admin token (§8). Nobody can force-push or delete it. By default the default branch.
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
- **Bound token**: A PAT confined to one workspace (and the forks created with it). A token bound to a fork also reaches its own changes, and the awareness and policy reads, on the fork's parent (§3).
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
- **Effective value**: The value of a setting key that a response shows: the stored value when the key was set, otherwise its default.
