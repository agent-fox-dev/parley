---
spec_id: "10"
spec_name: "workspace_create_and_clone"
title: "Register a repository as a workspace"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "728fa65d7688a09c21a503bc6810679f90e4fd646c611ab89a43c182b460b86a"
schema_version: 3
source: "docs/prd/04_workspaces_and_forks.md"
---

# Register a repository as a workspace

## Intent

A developer registers a git repository under a slug with one call, and the hub makes its own bare copy of that repository in the background, so that agents and people can work on it through the hub. Registration either records everything it needs (the workspace, its origin credential and the clone work) or records nothing, the credential is never readable again, and clone work survives a restart of the hub. The rules this needs — how workspace names, URLs, branch names and durations are validated, and how a route answers for a workspace that is not ready — are built once here, for every later workspace feature.

## Goals

- `POST /workspaces` with a `slug` and a `git_url` answers `201` with a workspace in `status: "pending"` and `access: "owner"`, and emits `parley.workspace.create`.
- A given origin credential is checked against origin within 10 seconds before anything is stored; a refused credential creates nothing and answers `400 credential_check_failed`.
- A stored credential is a workspace secret and appears in no response, event, log line or `clone_error`.
- Create is all-or-nothing: workspace, credential secrets, clone work and event are recorded together or not at all.
- The clone runs in the background: `pending` → `cloning` → `ready` or `failed`. A `ready` workspace has every branch and tag of origin with full history, a `default_branch`, a `head_sha` and the origin head of every branch as the ref `refs/remotes/origin/<name>` of the copy.
- A failed clone leaves `status: "failed"`, a `clone_error` without credentials and no partial copy on disk.
- Clone work that was `pending` or `cloning` when the hub stopped is taken up again at the next start, in `created_at` order.
- At most `[workspace].workers` clones run at the same time.
- One guard answers `409 workspace_busy`, `409 workspace_not_ready` or `409 workspace_archived` for a workspace that cannot serve a route that needs its repository.
- `parley workspace create` registers a repository from the shell and, with `--wait`, returns when the workspace is `ready` (exit 0) or `failed` (exit 1).

## Non-goals

- The workspace access rule itself (levels `admin`, `owner`, `member`, none; the `404 workspace not found` answer; fork access; WS-FR-003 and WS-FR-004 as logic): package 07 workspace_access_rule. This package calls it and adds nothing to it.
- The binding guard and the `403 token_bound` answer a bound token gets on `POST /workspaces` and on workspaces outside its binding (WS-FR-005): package 08 workspace_bound_tokens. `POST /workspaces` is registered as a route that is not workspace-scoped, which that guard refuses for a bound token. Not covered by this package.
- `GET /workspaces`, `GET /workspaces/:slug`, `PATCH /workspaces/:slug`, the list of setting keys with their types, defaults and validation, and `parley workspace list|get|update`: package 11 workspace_read_update_settings. This package ships the settings document with an empty key registry (see Design Decisions, 6).
- Sync from origin: package 12 workspace_sync_from_origin. Push to origin: package 13 workspace_push_to_origin.
- Archive, reactivate, reclone and delete, and setting the maintenance marker the state guard reads: package 14 workspace_archive_reclone_delete. That package records clone work through the entry point this package provides.
- Task forks (creation, slug, tracking branches, lifetime): packages 15 task_fork_creation and 16 task_fork_lifetime. This package provides only the fork-name and duration validators they call (WS-FR-012, WS-FR-013).
- The default `deploy` of `integration_branch`, upstream credentials and the upstream set-up after a fresh clone of a carry-patch workspace (CP-FR-002 to CP-FR-005): package 31 carry_patch_setup_and_patches. This package validates and stores the three carry-patch fields and runs the post-clone step that package registers.
- Secret storage, encryption and the secrets routes: package 39 secrets. The event log and its guarantees: package 52 durable_event_log_and_stream.
- The error envelope, the check order, idempotency keys, body limits and the CLI's output, exit codes and waiting rules (PLAT-FR-010 to PLAT-FR-019, PLAT-FR-090 to PLAT-FR-095): packages 02 http_api_conventions and 09 cli_foundation. This package obeys them.
- Loading `[workspace].path`, `[workspace].workers` and `[server].external_url` from the configuration file: package 03 server_lifecycle_and_configuration. This package takes them as values.
- Compare-and-swap of refs (the second half of WS-FR-008): packages 18 git_push_rules and 21 scm_write_api.
- Serving the copy over git or the source control API: packages 17 git_smart_http and 20 scm_read_api.

## Background

The repository holds no code yet; every file named here is new. Source: `docs/prd/04_workspaces_and_forks.md`, WS-FR-001 to WS-FR-027 with §4.3.1, §4.3.4, §4.3.5, §4.5 (`POST /workspaces`), §4.6 (`parley workspace create`), §4.7.1, §4.8 (`parley.workspace.create`) and §4.9 (`[workspace].workers`).

Package 07 workspace_access_rule delivers the minimal workspace record the access rule reads (slug, owner, organization, kind, parent). This package extends that record to every field of §4.3.1 and is the first to create a workspace. It is also the first package that runs git: `internal/gitrepo/` starts here with the operations this package needs (list a remote's refs, make a bare clone, open and inspect a copy, remove a copy). Later packages add to it.

Code lives in `internal/workspace/` (record, validators, state guard, create handler, clone work), `internal/gitrepo/` (the `git` subprocess wrapper and a test helper that serves an origin), `internal/cli/workspace.go` (the command) and `test/e2e/` (end-to-end tests). The composition in `internal/hub` (package 03) wires the clone workers into the hub's start and stop.

## Requirements

All paths are relative to `/api/v1`. Errors are the error envelope of the platform conventions; "`409 conflict`" means HTTP 409 with `error_type` `conflict`.

### Validation rules (WS-FR-009 to WS-FR-013)

These rules are used by this package and by every later workspace package.

- **Repository slug (WS-FR-009).** 3 to 64 characters; lower-case letters `a`–`z`, digits and hyphens only; starts with a letter; ends with a letter or digit; does not contain `--`.
- **`git_url` (WS-FR-010).** Either `https://<host>/<path>` or `git@<host>:<path>`, with a non-empty host and a non-empty path. `http://`, `ssh://`, `file://` and every other form are refused. An `https` URL with userinfo (`https://user@host/…`, `https://user:pass@host/…`) is refused. The check is on the form only: no name is resolved and no connection is made.
- **Branch name (WS-FR-011).** Not empty; no control character, space, `~`, `^`, `:`, `?`, `*`, `[` or `\`; no `..`, `//` or `@{`; not exactly `@`; does not start with `-` or `/`; does not end with `/`, `.` or `.lock`; no path component that starts with `.` or ends with `.lock`.
- **Fork name (WS-FR-012).** 1 to 32 characters; lower-case letters, digits and hyphens; starts and ends with a letter or digit; no `--`.
- **Duration (WS-FR-013).** A positive whole number followed by one unit `m`, `h` or `d` (`90m`, `24h`, `7d`). `0m`, `-1h`, `1.5h`, `10`, `10s`, `1h30m` and the empty string are refused.

### State preconditions (WS-FR-007, WS-FR-008)

A route that needs the repository of a workspace asks one guard, after access and validation. The guard answers:

| Workspace | Answer | `error_type` |
|---|---|---|
| a reclone, archive or delete is running on it (the maintenance marker is set), or `status` is `pending` or `cloning` | `409` | `workspace_busy` |
| `status` is `failed` | `409` | `workspace_not_ready` |
| `status` is `archived` | `409` | `workspace_archived` |
| `status` is `ready` and no marker | passes | |

The marker is checked first. A `ready` workspace always passes, whatever other requests are running on it: there is no per-workspace lock a caller can observe, and `409 workspace_busy` is answered in the first row's cases only.

### Authentication and access on `POST /workspaces` (WS-FR-001, WS-FR-002, WS-FR-006)

The route follows the platform's check order: authentication, binding guard, scope, access, validation, state, act. The first failing check decides the answer.

- Scope: `workspaces:write`. Only personal access tokens are scope-checked. A token without it — also one that holds only `workspaces:read` — gets `403 insufficient_scope`. An API key is not scope-checked.
- An admin token cannot create a workspace, because a workspace needs a user as owner: `403 forbidden`.
- Because scope and access come before validation, a token without the scope and an admin token get their `403` also when the body is invalid.

### `POST /workspaces`: the request (WS-FR-014, WS-FR-015, WS-FR-017, WS-FR-018)

Body members, checked in this order; the first invalid one decides the message. Every failure is `400 invalid_request` unless stated, and creates nothing. Unknown members are ignored.

| Member | Required | Rule |
|---|---|---|
| `slug` | yes | string; the slug rule |
| `git_url` | yes | string; the `git_url` rule |
| `default_branch` | no | string or null; the branch-name rule. Absent or null: taken from origin when the clone completes |
| `org_id` | no | string or null; an organization id or slug. Absent or null: the caller's personal organization. An unknown organization is `400 invalid_request`. An organization the caller is not a member of is `403 forbidden` |
| `display_name` | no | string or null; at most 128 characters. Absent, null or empty: stored as the slug |
| `description` | no | string or null; at most 1024 characters. Absent or null: `""` |
| `workspace_mode` | no | string or null; `standard` or `carry_patch`. Absent, null or empty: `standard` |
| `upstream_url`, `integration_branch` | carry-patch | With `standard`: the presence of either with a non-null value is refused. With `carry_patch`: `upstream_url` is required and follows the `git_url` rule; `integration_branch`, when given, follows the branch-name rule |
| `settings` | no | object or null; applied as a settings value: any other JSON type is refused, and a key that is not a registered setting is refused with the message `unknown setting "<key>"` |
| `git_pat` | no | string, not empty; excludes `git_username` and `git_password` |
| `git_username`, `git_password` | no | strings, not empty; given together or not at all |

Any credential member needs an `https` `git_url`; with a `git@…` URL it is refused. A member of the wrong JSON type is refused. Lengths are counted in characters (Unicode code points).

### `POST /workspaces`: the credential check (WS-FR-016)

After validation, and only when a credential was given:

1. If the hub has no secret encryption key configured, the answer is `503 secrets_unavailable`. Origin is not contacted and nothing is created.
2. The hub lists origin's refs with the credential, with a deadline of 10 seconds. Any failure — origin refuses the credential, origin cannot be reached, the deadline passes — answers `400 credential_check_failed` and creates nothing. The message contains neither the token nor the password.

Without a credential origin is not contacted during the request; a repository that cannot be reached anonymously shows up as a failed clone.

### `POST /workspaces`: the result (WS-FR-019 to WS-FR-021, §4.3.1, §4.3.2, §4.8)

On success the hub records, in one transaction: the workspace (`kind: "repository"`, `status: "pending"`, `owner_id` the caller, the organization, the given or default fields), which is also its clone work; the credential as workspace secrets (`GIT_PAT`, or `GIT_USERNAME` and `GIT_PASSWORD`); and the event `parley.workspace.create`. If any of them cannot be stored, nothing is created and the answer is `500 internal`.

The answer is `201` with the workspace object: every field of §4.3.1, nullable ones as `null` and never omitted (`default_branch` is the given value or `null`; `head_sha`, `origin_head_sha`, `clone_error`, `last_sync_at`, `sync_error`, `upstream_head_sha`, `parent_slug`, `fork_point`, `expires_at`, `auto_change`, `purpose` and `session_id` are `null`), `created_at` and `updated_at`, and the computed fields:

- `org_slug`: the slug of the workspace's organization;
- `hub_url`: `<external_url>/git/<org_slug>/<slug>.git` with one trailing `/` of `[server].external_url` removed, or `null` when `external_url` is empty;
- `access`: `"owner"`;
- `settings`: every registered setting key with its effective value.

No member of the response carries the credential.

The event `parley.workspace.create` has `workspace` and resource the slug, the caller as actor, and metadata `kind` (`repository`), `git_url`, `workspace_mode` and `default_branch` (the given value or `null`). A refused or failed request emits no event.

A slug that is in use — by any workspace, in any status — answers `409 conflict` and changes nothing. Of two concurrent creates with the same slug, exactly one answers `201` and the other `409 conflict`.

The route creates a resource, so it accepts `Idempotency-Key` under the platform's rules.

### Git credentials for origin (§4.3.5)

Whenever the hub talks to origin for a workspace it resolves the credential anew from the workspace's secrets:

1. `GIT_PAT` is set and not empty: HTTP Basic authentication, user name `x-token-auth`, the token as password.
2. Otherwise `GIT_USERNAME` and `GIT_PASSWORD` are both set: HTTP Basic authentication with them.
3. Otherwise no credential (anonymous). A user name without a password is no credential.

A failure to read the secrets is an error; the operation that needed the credential fails and never falls back to anonymous. A credential is handed to `git` outside the URL, the command-line arguments and the copy's configuration.

### Clone (WS-FR-022 to WS-FR-024, §4.3.4)

Clone work for a `pending` workspace starts by setting `status: "cloning"` and clearing `clone_error`. It then makes a bare copy of origin with all branches and tags and full history, under `[workspace].path`.

- **Success.** The hub records `default_branch` (the one given at creation, else origin's default branch), `head_sha` (the head of that branch), the origin head of every branch as the ref `refs/remotes/origin/<name>` of the copy (so `origin_head_sha` equals `head_sha`), then sets `status: "ready"`. Commit ids are full 40-character ids.
- **Post-clone step.** A later package may register a step for a workspace mode. After a fresh clone of a workspace in that mode and before `ready`, the hub runs it. A failure of the step is logged and does not prevent `ready`.
- **Failure.** The clone fails when origin cannot be reached or refuses, when the credential cannot be read, when a `default_branch` was given and origin has no such branch (the `clone_error` names the branch), and when origin has no branch at all. The hub sets `status: "failed"` and `clone_error`, and leaves no directory at the copy's place. `clone_error` has known secret values replaced by `[redacted]`, userinfo removed from URLs, and is at most 1024 bytes. A failed clone is not retried automatically and emits no event.

### Durable clone work (WS-FR-025 to WS-FR-027, §4.7.1, §4.9)

- **Workers.** At most `[workspace].workers` clones (default 4) run at the same time; further clone work waits and is started in the order it was recorded. A value below 1 stops the start of `parleyd` with an error that names `[workspace].workers`.
- **Start.** When the hub starts, every workspace in `pending` or `cloning` is queued again in `created_at` order (then slug) before the hub listens. Leftovers of an interrupted clone are removed.
- **Existing copy.** When clone work finds a copy at the workspace's place, it checks that the copy is valid: it opens as a repository, the workspace's default branch (the copy's own default branch while the record has none) resolves to a commit, its origin is the workspace's `git_url`, and it was made for this very workspace record. A valid copy is kept: the workspace becomes `ready` with `head_sha` read from the copy, and nothing is cloned. Anything else is discarded and cloned fresh. A copy left by an earlier workspace with the same slug is therefore never adopted.
- **Shutdown.** When the hub stops, a running clone is interrupted. The workspace stays `pending` or `cloning`, nothing is left at the copy's place, and the work is resumed at the next start.

### `parley workspace create` (§4.6)

Flags: `--slug` and `--git-url` (required), `--default-branch`, `--org`, `--display-name`, `--description`, `--workspace-mode`, `--upstream-url`, `--integration-branch`, `--git-pat`, `--git-username`, `--git-password`, `--setting key=value` (repeatable), `--wait`, `--timeout`, `--poll-interval`.

- **Client checks.** Exit 2 with a `usage_error` on stderr, and no request is sent, when: `--slug` or `--git-url` is missing; `--git-pat` is given with `--git-username` or `--git-password`; one of `--git-username` and `--git-password` is given without the other; a credential flag is given with an empty value; a `--setting` has no `=` or an empty key.
- **`--setting`.** The text is split at the first `=`. An empty value is sent as `null`. Otherwise, for `protected_branches` the value is split at commas into a list of strings. Otherwise `true` and `false` are sent as booleans. Anything else is sent as a string. Of a repeated key the last one wins. The parser (`ParseSettingFlags`) is this package's and `parley workspace update` of package 11 calls it.
- **Request.** `POST /workspaces` with a body that holds a member for each flag given (`--org` as `org_id`, the settings as `settings`) and no member for a flag not given.
- **Output.** The workspace on stdout, exit 0. A hub error is printed and exits 1 by the CLI's common rules.
- **`--wait`.** After a `201` the command polls `GET /workspaces/:slug` every `--poll-interval` under `--timeout`. `ready` and `failed` are the terminal states. `ready`: the final workspace on stdout, exit 0. `failed`: the final workspace on stdout, `wait_failed` on stderr, exit 1.

### End-to-end flows

1. **Public repository.** A developer runs `parley workspace create --slug … --git-url …`; the CLI posts to the hub; `parleyd` validates, records the workspace and the event and answers `201` `pending`; a clone worker clones origin; the workspace is `ready` with origin's branches, tags and head.
2. **Private repository.** A developer posts with `git_pat`; `parleyd` lists origin's refs with the token, stores it as the secret `GIT_PAT` with the workspace and answers `201`; the clone resolves the credential from the secret and succeeds; the token is in no response, event or log line.
3. **Restart.** A workspace is `pending` when the hub stops; an operator starts the hub again; `parleyd` queues the workspace before it listens and clones it; the workspace becomes `ready`.

## Design Decisions

1. **Language, git and tests (project-wide).** Go; the hub runs the `git` command (2.38 or later) as a subprocess through `internal/gitrepo`, with no git library; repositories are bare repositories on disk; HTTP uses `net/http`; the CLI uses `github.com/spf13/cobra`; the store is SQLite behind `internal/store`; tests are table-driven with `testing` and `net/http/httptest`.
2. **Clone work is the workspace row.** A workspace in `pending` is its own clone work record; there is no separate queue table. One row write makes "workspace and clone work recorded together" (WS-FR-020) true by construction, and the start-up scan of WS-FR-025 reads the same rows. Workers are woken in memory after the transaction commits; packages that put a workspace back to `pending` (package 14) call the same entry point.
3. **Where a copy lives.** `<[workspace].path>/<slug>.git`. A clone is made in a temporary directory beside it and renamed into place only when complete, so a failed or interrupted clone leaves nothing a later check could take for a valid copy. Other packages get the path from `internal/workspace`, never by building it.
4. **Recognising a copy of an earlier workspace.** Each workspace record gets a random instance id at creation, shown in no response. A finished clone writes it into the copy's git configuration (`parley.workspace`); the validity check of WS-FR-026 compares it. The PRD says such a copy is never adopted and not how it is told apart.
5. **Origin heads are refs, not rows.** The last head seen on origin for each branch is the ref `refs/remotes/origin/<name>` of the workspace's copy, as §10.3 of the source describes them and as package 12 keeps them on every sync. A finished clone writes one for every branch; `internal/gitrepo` reads and writes them (`OriginHeads`, `OriginHead`, `SetOriginHead`, `DeleteOriginHead`), and packages 12, 13 and 14 use those functions. The store keeps only `origin_head_sha`, the origin head of the default branch.
6. **Settings at creation: this package declares the registry, package 11 fills it.** Package 11 depends on this package, so this package cannot call it. `internal/workspace/settings.go` here declares the mechanism as a `SettingsRegistry` value the workspace service is built with: `RegisterSetting` (a key with its name, its default and a `Validate` function for its value), `ApplySettings(stored, raw)` (`null`, object, unknown key, a key with value `null`, and each other value judged by its key's `Validate`) and `EffectiveSettings(stored, default_branch)`. Create applies `settings` with `ApplySettings` and answers `settings` from `EffectiveSettings`, and knows no key. Package 11 registers the 21 keys of §4.3.3 through `RegisterSetting` (its `RegisterSettingKeys`, which the hub's composition calls on the registry) and owns their types, defaults, validation rules and the choice of the key an error names; once it is built, create applies those rules without a change here. With this package alone, `settings: {}` and `null` are accepted, any key is `unknown setting`, and the response shows `{}`.
7. **Carry-patch members.** WS-FR-017 is implemented as written: the mode value, `upstream_url` required for `carry_patch` and checked by the `git_url` rule, `integration_branch` checked by the branch-name rule, both refused for `standard`. `integration_branch` is stored as given (`null` when absent); its default and everything after the clone belong to package 31, which registers a post-clone step.
8. **Order of the checks inside create.** Body members in the table's order; then the organization (unknown `400`, not a member `403`); then slug in use (`409`); then the missing encryption key (`503`); then the credential check against origin (`400`); then the transaction, where a racing duplicate slug is still `409`. Cheap local refusals come before the outbound call.
9. **No credential, no check.** Without a credential the request does not contact origin; the PRD checks only "a given credential".
10. **`git_url` in the event.** `https` URLs with userinfo are refused at creation, so the stored `git_url` is emitted unchanged. The `git@` of the other form is that form's fixed login, not a credential.
11. **An origin without branches fails the clone.** The PRD is silent; a workspace needs a default branch that resolves, so the clone ends `failed` with `clone_error` `origin has no branches`.
12. **No deadline on a clone.** The PRD gives the credential check a deadline and the clone none. A clone ends by success, failure or shutdown.
13. **Empty `workspace_mode`.** An empty string means `standard`, as CP-FR-001 states for the same member.
14. **How the credential reaches git.** Through the environment of the `git` process (an askpass program fed from the environment), never in the URL, the arguments or the copy's configuration, so that no process listing, `clone_error` or stored remote URL can carry it.
15. **`--wait` works once package 11 is built.** `--wait` polls `GET /workspaces/:slug`, which package 11 delivers. Until then the poll gets `404` and the command ends with that error, as the CLI's waiting rules say for a hub error. This package tests the wait against a scripted `httptest` server; package 11 runs `parley workspace create --wait` against a real hub in its end-to-end test.
16. **Test origin.** Because `git_url` must be `https`, tests serve a bare repository over TLS from `net/http/httptest` (git's file-based HTTP transport after `git update-server-info`, with optional Basic authentication) and make the hub's `git` trust the test certificate through an environment option of `internal/gitrepo`. The helper lives in `internal/gitrepo/gitrepotest/` and is the only test origin of the repository: package 13 extends it with a mode that accepts pushes, and packages 12, 14 and 31 use it.

17. **One response encoder.** `RenderWorkspace` in `internal/workspace/response.go` is the only place a workspace object is built. Package 11 owns the object's full definition (WS-FR-029) and completes the encoder; package 15 registers a fork view with `RegisterForkView` so that a fork's computed members are shown without `internal/workspace` importing `internal/fork`.
18. **Composition.** The clone workers are built and started on the composition value of `internal/hub` (package 03), not in `cmd/parleyd`.
19. **The secret component's operations (package 39).** This package uses `internal/secret` by the names package 39 exports and adds none: `Available()` decides the `503 secrets_unavailable` refusal before origin is contacted; `Put`, given the create transaction, stores `GIT_PAT`, or `GIT_USERNAME` and `GIT_PASSWORD`, in the workspace scope of the new slug and writes no event of its own; `Get(workspace scope, slug, name)` reads each of them for credential resolution on every call and registers the value with the text scrubber. An error from `Put` rolls the create transaction back (`500 internal`); an error from `Get` is returned as an error and is never read as "no credential".

## Dependencies

- **07 workspace_access_rule** (draft): the minimal workspace record this package extends; the caller identity and the access levels.
- **39 secrets** (draft): stores `GIT_PAT`, `GIT_USERNAME` and `GIT_PASSWORD` as workspace secrets inside the create transaction (`Put`), reads them for credential resolution (`Get`), and reports whether an encryption key is configured (`Available`).
- **52 durable_event_log_and_stream** (draft): writes `parley.workspace.create` in the same transaction as the workspace.
- Through those: 02 http_api_conventions (route registration, error envelope, idempotency), 03 server_lifecycle_and_configuration (configuration values, start and stop, the text scrubber), 04 to 06 (authentication, organizations, scopes), 09 cli_foundation (the CLI's client, output and waiting).
- External: the `git` command, 2.38 or later, on the hub's `PATH`.

## Verified External API

- `github.com/spf13/cobra` — **unverified** (not yet in `go.mod`): `cobra.Command` with `Use`, `Short`, `RunE func(cmd *cobra.Command, args []string) error`; `(*Command).AddCommand(cmds ...*Command)`; `(*Command).Flags() *pflag.FlagSet`; `(*Command).MarkFlagRequired(name string) error`.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Origin**: A registered workspace's remote (`git_url`).
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires: after 7 days by default, or 24 hours after all its changes have landed or been abandoned.
- **Carry-patch workspace**: A workspace that tracks an upstream project and keeps an ordered list of patches on top of it.
- **Upstream**: For a carry-patch workspace, the project it tracks (`upstream_url`).
- **Integration branch**: For a carry-patch workspace, the branch the hub rebuilds as upstream plus the ordered patches.
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
- **Bound token**: A PAT confined to one workspace (and the forks created with it). A token bound to a fork also reaches its own changes, and the awareness and policy reads, on the fork's parent (§3).
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
- **Durable job**: Background work that is recorded before its request returns, survives restarts and is retried with backoff. Rebuilds are the only durable jobs (§10). Clone work, restacks and land attempts are also durable but follow their own rules and are not retried with backoff (§4, §7, §8).
- **Origin head**: For a branch of a repository workspace, the last head the hub saw on origin.
- **Clone work**: The durable instruction to make or re-check the hub's copy of a workspace; a workspace in `pending` or `cloning` has clone work outstanding.
- **Maintenance marker**: The record that a reclone, archive or delete is running on a workspace.
