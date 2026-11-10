---
spec_id: "16"
spec_name: "task_fork_lifetime"
title: "Expire, extend and delete task forks"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "a2b78a1529492e3b733035b8b1da8f9e210b9707ca21317b42e6faec2dbd251c"
schema_version: 3
source: "docs/prd/04_workspaces_and_forks.md"
---

# Expire, extend and delete task forks

## Intent

A task fork is disposable: it exists for one task and must disappear without anyone cleaning up after it. Every fork therefore has a lifetime with fixed limits, the hub removes a fork when that lifetime ends or, earlier, a day after its work has landed, and an owner can lengthen or shorten the lifetime or delete the fork at once. Whatever removes a fork removes it whole, in one step or not at all, together with the credentials bound to it, and leaves a record of why it went.

## Goals

- `PATCH /api/v1/workspaces/:slug` with `ttl` on a fork sets `expires_at` to now plus `ttl`, longer or shorter than before, and answers `200` with the fork.
- `DELETE /api/v1/workspaces/:slug` on a fork answers `204`, needs no archive first, and removes the fork, its own branches, its secrets and the tokens bound to it in one transactional step; a failure answers `500` and leaves everything as it was.
- A sweep that runs at least once a minute deletes every fork whose `expires_at` has passed and writes one `parley.fork.expire` event with reason `ttl` for each.
- With `[forks].expire_on_land` true, the sweep deletes a fork 24 hours after all its changes have landed or been abandoned (reason `landed`), and waits while the fork has a `provisioning` or `ready` sandbox.
- Deleting a parent removes all its forks in the parent's own transaction, with one `parley.workspace.delete` event for each fork.
- A fork whose parent is archived can still be deleted and still expires; its lifetime cannot be changed (`409 parent_archived`).
- `parley fork extend <slug> --ttl <duration>` and `parley fork delete <slug>` do the two owner actions from the shell and refuse a slug that is not a fork slug before sending anything.

## Non-goals

- Creating a fork, its slug, fork point, tracking branches, `auto_change`, the token and sandbox objects of the create call, the fork limit `[forks].max_per_workspace`, `GET /workspaces/:slug/forks`, `parley fork create|list`, how a fork's own branches are stored, `purpose` and `auto_change` in an update of a fork, and the `409 parent_archived` refusal of every other write to a fork (git push, source control API): package 15 `task_fork_creation`. The lifetime at creation is also that package's: the keys `[forks].default_ttl` and `[forks].max_ttl` with their defaults and the refusal of a contradictory configuration, the rule `ResolveTTL` that judges a `ttl`, and `expires_at` of a new fork (WS-FR-069). This package adds the `ttl` field to the update, which calls that rule, and the key `[forks].expire_on_land`.
- `PATCH /workspaces/:slug` itself (body rules, `display_name`, `description`, the refusal of `ttl` on a repository workspace, the `parley.workspace.update` event): package 11 `workspace_read_update_settings`.
- Archiving a parent, and the transactional delete of a repository workspace with its own data (WS-FR-064, WS-FR-065): package 14 `workspace_archive_reclone_delete`. That package declares the `ForkRemover` interface and the one registry of removers of owned data; this package implements the interface (`DeleteFork` for `DELETE` on a fork, `RemoveForksOf` for the forks inside a parent's delete) and removes a fork's owned data through that registry.
- The access rule and the scope check: packages 07 `workspace_access_rule` and 06 `api_keys_and_access_tokens`. Revoking bound tokens and the binding sweep: package 08 `workspace_bound_tokens`; its `RevokeBoundTo` is the token revoker package 14 wires, and this package reaches it through package 14's `RemoveOwned`.
- Deleting a fork's sandboxes and what a sandbox does when its workspace goes (SBX-FR-044, SBX-FR-045): package 47 `sandbox_lifecycle_and_checkpoints`. Force-closing a fork's sessions (AUD-FR-009): package 49 `agent_sessions_and_usage`. Removing a fork's variables: package 40 `variables_and_resolution`. What a `draft` or `open` change does when its source fork is gone, and keeping its versions fetchable (WS-FR-079, CHG-FR-023): packages 22 `change_opening_and_versions` and 23 `change_lifecycle_and_mergeability`. Each of them takes part in a fork's removal by registering one remover of owned data (or, for sessions, the session closer) in the `Teardown` set of package 14, which is called for a fork removed alone, by expiry or with its parent; sandboxes also through the sandbox collaborator this package defines.
- Calling the activity hook when a fork is pushed to or a change of it is opened, landed, abandoned or restored: packages 18 `git_push_rules`, 21 `scm_write_api`, 22, 23 and 25 `optimistic_landing`.
- The gauge `parley_forks_active`: package 51 `audit_query_and_retention`.
- The error envelope, the order of checks (PLAT-FR-010 to PLAT-FR-019): package 02 `http_api_conventions`. CLI output, exit codes and the HTTP client (PLAT-FR-090 to PLAT-FR-095): package 09 `cli_foundation`. The maintenance loop and the configuration loader: package 03 `server_lifecycle_and_configuration`. Storing events: package 52 `durable_event_log_and_stream`.

## Background

The repository holds no code yet; every file named here is new. Source: `docs/prd/04_workspaces_and_forks.md`, the lifetime part of WS-FR-066 to WS-FR-082 (WS-FR-066, WS-FR-069, WS-FR-070, WS-FR-071, WS-FR-078, WS-FR-079, WS-FR-080), §4.3.6 "Lifetime", the `ttl` row of `PATCH /workspaces/:slug` and `DELETE /workspaces/:slug` for a fork in §4.5, `parley fork extend` and `parley fork delete` in §4.6, §4.7.2, the `parley.fork.expire` and `parley.workspace.delete` rows of §4.8, the `[forks]` keys `default_ttl`, `max_ttl` and `expire_on_land` of §4.9 and the fork lifetime cases of §4.10.

Package 15 creates forks in `internal/fork/` and adds `parley fork create|list` in `internal/cli/fork.go`. Package 11 owns the update handler in `internal/workspace/` and exports `RegisterForkField`, with which this package registers `ttl`. Package 14 owns `DELETE /workspaces/:slug`, the `ForkRemover` interface and `RemoveOwned` in `internal/workspace/teardown.go`. Package 10 provides the duration validator of WS-FR-013 and the state guard with its maintenance marker. Package 08 provides the component that revokes the tokens bound to a set of slugs inside a given transaction and returns their ids.

Code of this package lives in `internal/fork/` (the `ttl` update, removal, sweep, early expiry), `internal/config/` (the key `[forks].expire_on_land`), `internal/cli/fork.go` (the two commands), `internal/hub/` (registering the fork remover, starting and stopping the sweep) and `test/e2e/`.

## Requirements

### Lifetime limits (WS-FR-069, WS-FR-013, §4.9)

The lifetime limits are package 15's: `[forks].default_ttl` (default `7d`), `[forks].max_ttl` (default `30d`), the refusal to start on a contradictory configuration, and the one rule for a requested `ttl` (`ResolveTTL`: a duration of WS-FR-013, at least 5 minutes and at most `max_ttl`, both bounds inclusive, `expires_at` the given instant plus `ttl` in whole seconds). Package 15 applies the rule at creation with the creation time. This package adds one configuration key, `[forks].expire_on_land` (bool, default `true`), and applies the same rule to an update with the time of the request.

`PATCH /workspaces/:slug` on a fork accepts `ttl`. A caller with level `owner` on the fork (its creator or the parent's owner) or the admin token, holding `workspaces:write` when the credential is a PAT, sets `expires_at` to now plus `ttl`. This may shorten the lifetime. The answer is `200` with the workspace object; `updated_at` is set. A `parley.workspace.update` event is written with the update, its `metadata.fields` naming `ttl`, its `metadata.parent_slug` naming the parent, and the fork's `session_id` when it has one.

Failures, each leaving `expires_at`, `updated_at` and every other field of the same body unchanged and writing no event:

- `ttl` that is not a string (`null`, a number, a boolean), is not a duration, is shorter than 5 minutes or longer than `max_ttl`: HTTP 400, `invalid_request`.
- An organization member who is not an owner: HTTP 403, `forbidden`. A PAT without `workspaces:write`: HTTP 403, `insufficient_scope`. A caller with no read access: HTTP 404, `not_found`.
- The parent is `archived`: HTTP 409, `parent_archived`. The parent is `pending` or `cloning`, or an archive or delete is running on it: HTTP 409, `workspace_busy`.

The checks run in the order of PLAT-FR-013, so a member gets `403` also when the parent is archived, and an invalid `ttl` gets `400` also when the parent is archived.

A fork stays usable until the sweep removes it. Between `expires_at` and the sweep's next pass it is read and written as before, and a `ttl` update in that window is accepted: the fork lives on (§4.10).

### Removing a fork (WS-FR-078, WS-FR-079, WS-FR-081)

A fork is removed for one of three causes: a caller deletes it, the sweep expires it, or its parent is deleted. Every cause runs the same removal:

1. The fork's sandboxes are deleted first, with reason `workspace_deleted` and without a checkpoint (SBX-FR-045). If that fails, the removal does not start and the fork stays whole. When the fork goes with its parent, the parent's delete has already deleted the sandboxes of the workspace and of its forks (WS-FR-065), so this step is the parent's.
2. In one store transaction: the fork's record is removed and, through `RemoveOwned` of package 14, every live token bound to it is revoked and every owner of fork data does its part by its registered remover (workspace secrets and variables are removed, active sessions are force-closed with reason `workspace deleted` or, for the sweep, `fork expired`, changes are told their source is gone), and the event of the removal is written. If anything in the step fails, nothing has changed.
3. After the transaction commits, the fork's own branches are discarded from the repository data. A failure here does not undo the removal; the discard is repeated by each later sweep pass until it succeeds.

Afterwards the fork answers `404 not_found`, it is absent from the parent's fork list, tokens that were bound to it answer `401 unauthenticated`, its own branches no longer resolve, and its slug is free: a fork created later under the same name has none of the earlier fork's branches. The parent's branches are never changed by a removal. Change records are not deleted.

### Deleting a fork on request (`DELETE /workspaces/:slug`, §4.5)

A caller with level `owner` on the fork or the admin token, holding `workspaces:write` when the credential is a PAT, deletes a fork without archiving anything first. The answer is `204` with an empty body. It works while the parent is `ready` and while it is `archived`.

One `parley.workspace.delete` event is written in the removal's transaction: `workspace` and `resource_id` are the fork's slug, the actor is the caller, `metadata.kind` is `fork`, `metadata.parent_slug` names the parent, `metadata.revoked_tokens` holds the revoked token ids in ascending order, there is no `deleted_forks` member, and the event carries the fork's `session_id` when it has one.

Failures, each leaving the fork whole and writing no event:

- An organization member who is not an owner: HTTP 403, `forbidden`. A PAT without `workspaces:write`: HTTP 403, `insufficient_scope`. No read access, or a fork that is already gone: HTTP 404, `not_found`.
- The parent is `pending` or `cloning`, or an archive or delete is running on it: HTTP 409, `workspace_busy`.
- The sandbox deletion or the transactional step fails: HTTP 500, `internal`.

Of two concurrent deletes of one fork, one answers `204` and the other `404`, and one event is written.

### Removing forks with their parent (WS-FR-080)

The delete of a repository workspace (package 14) removes every fork of that workspace inside its own transaction, by calling `RemoveForksOf` of the fork remover this package registers. The step removes each fork as above with cause "parent deleted", writes one `parley.workspace.delete` event for each fork (actor: the actor of the parent's delete; `metadata.kind` `fork`, `parent_slug`, that fork's `revoked_tokens`), writes no `parley.fork.expire`, and returns the fork slugs in ascending order for the parent event's `deleted_forks`. A parent without forks yields an empty list and no event. If the parent's transaction is rolled back, every fork, token and secret is as before and no event exists. The forks' own branches are discarded after the commit.

### Expiry sweep (§4.7.2)

The sweep runs one pass right after start, then one every 60 seconds, and again as the task `fork expiry` of the hourly maintenance loop (PLAT-FR-005). Two passes never run at once: a pass asked for while another is running does nothing.

A pass removes every fork whose `expires_at` is at or before the current time, with reason `ttl`. Forks of an archived parent expire like any other. A fork whose parent is `pending` or `cloning`, or has an archive or delete running, is left for the next pass.

Every fork the sweep removes gets one `parley.fork.expire` event in the removal's transaction and no `parley.workspace.delete`: actor `system`, `workspace` and `resource_id` the fork's slug, `metadata.parent_slug`, `metadata.reason` (`ttl` or `landed`), `metadata.expires_at` (the fork's value at removal) and `metadata.revoked_tokens` in ascending order, with the fork's `session_id` when it has one.

A removal that fails is logged, leaves that fork whole and without an event, does not stop the pass, and is tried again by the next pass. When the hub shuts down during a pass, the removal in progress completes or is rolled back and no further removal starts.

### Early expiry after landing (WS-FR-070)

When `[forks].expire_on_land` is true, a pass also removes, with reason `landed`, every fork not yet past `expires_at` for which all of these hold:

- the fork is the source of at least one change;
- every change with the fork as source is `landed` or `abandoned`;
- every own branch that still exists in the fork is the source of such a change;
- the last activity on the fork is at least 24 hours old;
- the fork has no sandbox in state `provisioning` or `ready`.

Activity is a push to the fork, a change opened from it, and a change of it landing, being abandoned or being restored. Each restarts the 24 hours. A fork that is past `expires_at` and also meets these conditions is removed once, with reason `ttl`. With `expire_on_land` false no fork is removed before `expires_at`. A fork with no change is never removed early (§4.10, "Fork with work that has no change"). If the changes or the sandboxes of a fork cannot be read, the fork is not removed early in that pass; its `expires_at` still applies.

### CLI (§4.6)

- `parley fork extend <slug> --ttl <duration>` sends `PATCH /workspaces/:slug` with `{"ttl": "<duration>"}` and prints the fork. `--ttl` is required.
- `parley fork delete <slug>` sends `DELETE /workspaces/:slug` and prints `{"status": "deleted", "slug": "<slug>"}`. It has no confirmation flag: forks are disposable.
- Both refuse a slug without `--` before sending anything: a usage error, exit status 2, nothing on stdout. `parley fork delete` can therefore never delete a repository workspace.
- A hub error is printed on stderr as received and the exit status is 1.

### End-to-end flows

1. An agent runs `parley fork extend demo--task1 --ttl 2d`; the `parley CLI` sends the update; `parleyd` sets `expires_at` and records `parley.workspace.update`; the agent later runs `parley fork delete demo--task1`; `parleyd` removes the fork, revokes its bound token and records `parley.workspace.delete`; reading the fork answers `404` and the token answers `401`.
2. A developer creates a fork with `ttl` `5m`; six minutes later the sweep of `parleyd` finds it past `expires_at`, removes it and records `parley.fork.expire` with reason `ttl`; the developer reads the fork and gets `404`, and finds the event in the event log.

## Design Decisions

1. **Stack (project-wide).** Go; HTTP on `net/http`; the store is SQLite behind `internal/store`; the CLI uses `github.com/spf13/cobra`; git runs as a subprocess through `internal/gitrepo`; tests are table-driven with `testing` and `net/http/httptest`.
2. **The lifetime at creation is package 15's; extension, expiry and early removal are this package's.** Creation needs the default and the bounds, and package 15 is built first, so it owns `[forks].default_ttl`, `[forks].max_ttl`, the refusal of a contradictory configuration, the rule `ResolveTTL` and `expires_at` of a new fork. This package owns `ttl` in `PATCH` (WS-FR-071), which calls `ResolveTTL` with the request time, the sweep, early removal and `[forks].expire_on_land`.
3. **Configuration limits.** The refusal of a `default_ttl` shorter than 5 minutes or a `max_ttl` shorter than `default_ttl` is package 15's (its requirement on fork limits in configuration).
4. **Both bounds of `ttl` are inclusive**, and `expires_at` is a whole second (PLAT-FR-016).
5. **A fork is expired when `expires_at` is at or before the sweep's current time.**
6. **The sweep interval is 60 seconds, with a pass right after start.** The source says "at least once a minute"; the maintenance loop's hourly task is the backstop PLAT-FR-005 lists.
7. **"Held for 24 hours without interruption" is computed from one timestamp.** The fork keeps the time of its last activity, shown in no response and set to the creation time at first. Everything that can make the conditions of WS-FR-070 start or stop holding is an activity, so "the conditions hold now and the last activity is at least 24 hours old" equals the source's rule and needs no history. This package exports the hook that records an activity; the packages that own pushes and change transitions call it.
8. **Collaborators.** Two interfaces are defined here because no dependency has them: a sandbox collaborator (delete the sandboxes of a workspace with a reason and a checkpoint switch; report whether one is `provisioning` or `ready`), which package 47 implements, and a change reader (the source branch and state of every change with a given fork as source). The hub's composition in `internal/hub/fork.go` passes the real ones as their packages arrive. Without a change reader no fork is removed early; without a sandbox collaborator a fork has no sandboxes. Everything else that owns data of a fork is reached through package 14: this package keeps no registry of removal participants. `RemoveOwned` of package 14 revokes the bound tokens (package 08), runs every registered remover of owned data (`secret.RemoveWorkspace` of package 39, sandboxes' records of package 47, and the others) and asks the session closer (package 49) with the reason `fork expired` for an expiry and `workspace deleted` for a delete, alone or with the parent. Why: a package that stores data per workspace registers once and is called for every way a workspace or a fork goes away.
9. **A failed sandbox deletion stops the removal.** The source orders "sandboxes first"; removing the fork under a sandbox that still exists would break "a sandbox never outlives its fork". A request gets `500 internal`; the sweep tries again at its next pass.
10. **Repository data is discarded after the commit**, never before, so a rolled-back removal has lost nothing. A discard that fails is recorded and repeated by later sweep passes.
11. **A fork whose parent is busy is skipped by the sweep.** WS-FR-066 says a fork answers `409 workspace_busy` in that state and that expiry keeps working while the parent is archived; the busy states are short, so the next pass does the work.
12. **Deleting a fork that is already gone is `404 not_found`**, as for any unknown slug; the source defines no idempotent delete for workspaces.
13. **TTL wins over landing** when a fork meets both in one pass: one removal, reason `ttl`.
14. **`ttl` in an update is all-or-nothing with the other fields of the body**, as package 11 defines for every update. In `metadata.fields` of the update event `ttl` has its place in package 11's fixed order: after `display_name`, `description`, `org_id` and `settings`, before `purpose` and `auto_change`.
15. **The CLI does not validate the form of `--ttl`** beyond requiring a non-empty value; the hub's `400` is printed as received. The source lists one client check, the fork slug.
16. **Event actor on removal with the parent** is the actor of the parent's delete, because WS-FR-080 makes it one operation.
17. **Profile: full.** Seven requirements and two end-to-end flows.

18. **One interface with package 14, in one place.** `DELETE /workspaces/:slug` is package 14's route. `internal/workspace/teardown.go` of package 14 declares `ForkRemover` with `DeleteFork(ctx, slug, caller)` and `RemoveForksOf(ctx, transaction, parent slug, actor)`; this package implements both on its removal component and registers it in package 14's `Teardown` set from `internal/hub/fork.go`. It registers no route and edits no handler of package 14. Package 14 depends on nothing of this package; this package depends on package 14.
19. **The activity hook is `RecordActivity(ctx, slug)` of `internal/fork`, registered from `internal/hub/fork.go`.** Three packages declare a fork-activity hook and none of them is a dependency of this package: `pushrule.ForkActivity` (package 18; pushes and the API writes of package 21 go through its ref writer), `change.ForkActivity` (package 22; a change opened or versioned, and the transitions of package 23) and the after-landing hook list of `internal/land` (package 25). `internal/hub/fork.go` registers `fork.RecordActivity` with all three, through one adapter each; the adapter for package 25 calls it for the source workspace of every landed change. `internal/fork` imports none of them. A registration is written for each of the three that exists in the repository when this package is built and is added to the same file when a later one arrives; until then a fork's last activity is its creation time.
20. **A fork's secrets are removed by package 14's `RemoveOwned`, not by a call of this package (package 39).** Package 39 exports `RemoveScope(tx, scope, scope id)` and `RemoveWorkspace(tx, slug)`, the same for one workspace scope. Package 14 registers `secret.RemoveWorkspace` as a remover of owned data, and `RemoveOwned` runs it with the fork's slug inside the removal transaction, which has the effect of `RemoveScope(tx, workspace, <fork slug>)`. This package therefore does not call `RemoveScope` itself; a second call would only repeat the removal.
21. **No removal participants in `internal/fork` (packages 47 and 49).** Packages 47 and 49 register their removers of owned data, the sandbox remover and the session closer only in package 14's `Teardown`; nothing registers with `internal/fork` for removal. This package passes the session closer its reason through `RemoveOwned`: `fork expired` for the cause `expired` (the sweep, by `ttl` or after landing) and `workspace deleted` for the causes `deleted` and `parent_deleted`. All wiring of this package is in `internal/hub/fork.go`; none is in `cmd/parleyd`.
22. **`CountTaskForks` for the metrics (with package 51).** `internal/fork` exports `CountTaskForks(ctx)`, the number of task forks that exist. Package 51 registers it in `internal/hub` as the source of `parley_forks_active`; this package reports nothing to a recorder.

## Dependencies

- Package 15 `task_fork_creation` (draft): the fork record with `expires_at`, the `[forks]` limits and `ResolveTTL`, the parent-state guard of fork updates, the storage of a fork's own branches, and `internal/cli/fork.go`.
- Package 14 `workspace_archive_reclone_delete` (draft): the `DELETE` route, the `ForkRemover` interface this package implements, `RemoveOwned` and the `Teardown` set with its token revoker (package 08), removers of owned data (package 39 and others) and session closer.
- Package 11 `workspace_read_update_settings` (draft): the update handler with `RegisterForkField`, and `GET /workspaces/:slug`.
- Through them: package 10 (duration validator, state guard, maintenance marker), package 08 (revoking bound tokens), package 39 (workspace secrets), package 07 (access rule), package 52 (event writing), package 03 (configuration key registry, maintenance task registry, the composition in `internal/hub`), package 02 (router, error envelope) and package 09 (CLI root and HTTP client).

## Verified External API

- `github.com/spf13/cobra`: `cobra.Command`, `(*cobra.Command).Flags`, `(*cobra.Command).MarkFlagRequired`, `cobra.ExactArgs`. **Unverified**: the module is not yet in `go.mod`; the signatures are the ones the library documents.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires: after 7 days by default, or 24 hours after all its changes have landed or been abandoned.
- **Tracking branch**: In a fork, a branch that existed in the parent at fork time. It is read-only in the fork and always equals the parent's branch.
- **Own branch**: A branch of a fork that is not a tracking branch; it belongs to the fork alone and is not visible in the parent (§4.3.6).
- **Change**: The unit of work: a proposal to add the commits of a source branch to a target branch, with a number, a title, an intent, versions and a state.
- **Bound token**: A PAT confined to one workspace (and the forks created with it). A token bound to a fork also reaches its own changes, and the awareness and policy reads, on the fork's parent (§3).
- **Sandbox**: An isolated environment in which an agent runs, attached to a workspace (normally a task fork) and started with it checked out. It runs on OpenShell; the hub controls it (§13).
- **Session**: An agent session: who ran, on which workspace, with what token usage, and which changes it produced.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
- **Maintenance marker**: The record that a reclone, archive or delete is running on a workspace (package 10).
- **Sweep**: The background pass that removes forks whose lifetime has ended.
- **Remover of owned data**: A component that owns data of a workspace or a fork and removes it inside the removal transaction; registered once, in the `Teardown` set of package 14.
