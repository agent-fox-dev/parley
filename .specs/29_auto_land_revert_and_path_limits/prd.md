---
spec_id: "29"
spec_name: "auto_land_revert_and_path_limits"
title: "Land automatically, revert in one call and honour path limits"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "155bdb6dad8223ddd53a6f63c3b25d3cc5ddcf144cc995ca68bcf2d007c84968"
schema_version: 3
source: "docs/prd/08_landing_and_policy.md"
---

# Land automatically, revert in one call and honour path limits

## Intent

Landing in Parley is not serialized, so safety comes from what surrounds it. An agent must be able to mark a change to land by itself once policy is satisfied and walk away; a developer must be able to undo a landed change with one call when something breaks afterwards; a token limited to some paths must never create, land or revert work outside those paths; and only the workspace's owner, with a credential that is not confined to a workspace, may land past unmet approvals and checks. This package gives the hub those four behaviours on top of optimistic landing and landing policy.

## Goals

- A change with `auto_land: true` gets a land attempt with `trigger: "auto_land"` when it becomes `ready` and `clean`, with permission checked as the user who set the flag and events written with actor `system`.
- An automatic attempt that ends `conflict` or `blocked` keeps the flag; one that ends `failed` is retried after 2 s, doubling to at most 5 min, at most 10 times, and then the flag is cleared.
- In a stack a change is landed automatically only after the change below it has landed.
- `POST /api/v1/workspaces/:slug/changes/:number/revert` on a landed change creates the branch `revert/<number>` and opens a change that undoes the landed commits, or answers `409 revert_conflict` with a conflict report and creates nothing.
- `parley change revert <slug> <number>` does the same from the shell.
- A path-scoped token creates a version, lands, reverts and sets `auto_land` only when every file concerned matches one of its globs; otherwise `403 path_not_allowed` naming the path.
- `override` on a land call is accepted only from the workspace's owner with an API key or an unbound PAT, or from the admin token, while `land.allow_override` is true; it skips unmet approvals and checks, never a `block`, and is recorded.

## Non-goals

- The land attempt object, the land routes, preconditions, rebase or squash, compare-and-swap, bookkeeping, stack landing, after-landing steps, recovery of `running` attempts, who may land (LAND-FR-001, LAND-FR-003) and `parley change land|lands`, `parley land list|show`: package 25 optimistic_landing. This package starts attempts through its start function and registers with `internal/land` what that package declares: the override rule, two request guards (the path limit and the refusal of an override the caller may not use) and a final observer. The shape of `override` (`400 invalid_request`) is validated by package 25.
- The policy document, its evaluation into `landing` and the routes for it: package 27 landing_policy. Approvals and checks: package 28 approvals_and_checks.
- The conflict report's shape and the conflict routes: package 26 conflict_reports_and_resolution. This package returns a report of that shape for a revert that does not apply.
- The change record, opening through `POST …/changes`, versions and the path check at that open: package 22 change_opening_and_versions. Storing `auto_land` and its setter (the user who set it, the credential type and the token globs) at open and through `PATCH`, with the accessor `AutoLandSetter` this package reads, the landable announcement and `parley.change.ready`: package 23 change_lifecycle_and_mergeability. That announcement is the one writer of the event: it writes it once when a change is `open`, `ready` and `clean` for a head it has not been announced for since it last was not landable, whether mergeability (package 23) or policy (package 27) moved last. This package starts automatic lands from that event and from nothing else that says "landable". Restack itself and `depends_on`: package 24 change_stacks_and_restack.
- The push rules, rule 5 on commits a push or an API write adds (GIT-FR-032), the ref writer and `parley.git.push`: package 18 git_push_rules. The API commit, ref and rebase routes: package 21 scm_write_api.
- The `paths` field of a token, its validation and the glob matcher: package 06 api_keys_and_access_tokens. Every path limit of this package is decided by that matcher (`*`, `**`, `?` literal). The matcher of package 27 landing_policy, in which `?` is a wildcard, is for the `paths` of policy rules and is never used on a token's globs. The binding guard that answers a fork-bound token `403 token_bound` on the revert route: package 08 workspace_bound_tokens.
- The event log: package 52 durable_event_log_and_stream. The shared CLI client, output and exit codes: package 09 cli_foundation.
- Not covered by this package: an event when `auto_land` is cleared after the retries are used up (the PRD defines none).

## Background

The repository holds no code yet; every file named here is new. Files under `internal/land/` that package 25 creates, under `internal/change/` that packages 22 to 24 create, under `internal/gitserver/pushrule/` that package 18 creates and `internal/cli/change.go` that package 22 creates are extended, not replaced.

The source is `docs/prd/08_landing_and_policy.md`: LAND-FR-002 (override), LAND-FR-060 to LAND-FR-062 (automatic landing), LAND-FR-070 to LAND-FR-072 (revert), LAND-FR-080 to LAND-FR-082 (path-scoped tokens), the route `POST …/revert` of §8.5, the command `parley change revert` of §8.6, the events `parley.change.revert` and `parley.change.land_fail` of §8.8 and §14.8, and the edge case "revert of a revert" of §8.10.

This package adds to `internal/land/`: `autoland.go` (the automatic lander), `revert.go` (the revert operation and its route), `pathlimit.go` (the path-limit check and the version-files lookup) and `override.go` (the override gate), each with a test file beside it, and the command in `internal/cli/change.go`. It builds on the land component of package 25, the policy evaluation of package 27, the change component of packages 22 to 24, the push-rule engine and ref writer of package 18, the glob matcher of package 06 in `internal/identity`, the access rule of package 07, `internal/gitrepo` and the event component of package 52.

## Requirements

All routes are under `/api/v1`. The conventions of package 02 apply (error envelope, order of checks, `Idempotency-Key`).

### Starting an automatic land (LAND-FR-060)

A change is landable when it is `open`, its `landing.status` is `ready` and its `mergeable` is `clean`. When a change with `auto_land: true` becomes landable, which package 23's announcement reports with exactly one `parley.change.ready` event per transition, `parleyd` starts a land attempt for that one change with `trigger: "auto_land"`, without `stack`, without `override` and with `session_id` null. The attempt's `requested_by` names the user who set the flag; its events (`parley.change.land`, `parley.change.land_fail`) have actor `system`.

Before the attempt starts, permission is checked as the user who set the flag, by LAND-FR-001: that user exists and is not blocked, and has write access to the target workspace or is the change's author while `land.by_author` is true. If the check fails, no attempt is created, the flag is cleared and one `parley.change.land_fail` is emitted on the change with `reason: "not_permitted"`, `status: "blocked"` and `land_id: null`.

Nothing is started when, at the moment the event is handled, the change is no longer `open`, the flag is off, the change is no longer landable, or a land attempt for the change is `running`. The same start is made when the flag is set to `true` on a change that is already landable, and at hub start for every change that is landable with the flag on and no `running` attempt.

### Outcomes and retries (LAND-FR-061)

- `landed`: nothing more to do.
- `conflict` or `blocked`: the flag stays on and no retry is scheduled; the next time the change becomes landable a new attempt is started.
- `failed`: a new attempt is started after a delay. The delays are 2 s, 4 s, 8 s, 16 s, 32 s, 64 s, 128 s, 256 s, 300 s, 300 s: at most 10 retries after the first failed attempt. When the 10th retry also ends `failed`, the flag is cleared and nothing more is started. Each retry is a new land attempt.

A retry that comes due while the change is not landable, not `open` or without the flag is dropped, and the retry count starts again from zero. The count also starts again after any attempt that does not end `failed`. The retry count and the due time are stored, so a retry that was pending when the hub stopped is made after the next start, at its due time or at once when that time has passed.

### Stacks (LAND-FR-062)

A change with `depends_on` naming a change that is not `landed` is never landed automatically: no attempt is started and the flag stays on. When the change below lands, the hub restacks the dependant (package 24); when the dependant then becomes landable it is landed automatically like any other change. An automatic attempt never lands a stack.

### Revert (LAND-FR-070, LAND-FR-071)

`POST /workspaces/:slug/changes/:number/revert` with body `{"title"?: string, "intent"?: string, "draft"?: bool, "auto_land"?: bool, "session_id"?: string}` on a landed change:

1. reads the head *T* of the original change's target branch;
2. builds, on *T*, one commit per landed commit of the original change, newest landed commit first, each undoing that commit. A reverting commit has the subject `Revert "<subject of the undone commit>"` and the body `This reverts commit <full id>.`; its author and committer are the hub's landing identity (`[land].committer_name`, `[land].committer_email`);
3. creates the branch `revert/<number>` in the target workspace at the last of those commits. When that branch exists, `revert/<number>-2` is used, then `-3`, and so on, the first name that is free;
4. opens a change with that branch as source, the original's target branch as target, `reverts: <number>`, a first version of kind `revert`, the caller as author, the title `Revert "<title of the original>"` (cut to 200 characters) unless `title` is given, and the intent `Reverts change <slug>#<number>: <title of the original>.` unless `intent` is given; `draft: true` opens it as a draft;
5. answers `201` with the new change.

The original change then shows `reverted_by: <new number>`. `parley.change.revert` is emitted on the original with `reverted_by`, with the caller as actor; the new change gets `parley.change.version` and, unless it is a draft, `parley.change.open`, as any opened change does. The branch creation is recorded as `parley.git.push` with `via: "api"`.

The reverting change is an ordinary change: policy is evaluated for it, it needs whatever its files need, and it lands through the ordinary land call. `auto_land: true` in the body sets the flag with the caller as the user who set it. Reverting a reverting change is allowed and restores the original content.

Failures, in this order after authentication, scope, access and the lookup of the change:

| Case | Answer |
|---|---|
| body member of the wrong type; `title` not 1 to 200 characters or with a line break; `intent` over 16,000 characters | `400 invalid_request` |
| `session_id` fails the session-linking rule | `400 unknown_session` |
| target workspace `archived` | `409 workspace_archived` |
| the change is not `landed` | `409 change_not_landed` |
| a `draft`, `open` or `landed` change already reverts it | `409 already_reverted`, with `details.reverted_by` |
| the original's target branch no longer exists | `400 target_not_found` |
| a file of the original change is outside a path-scoped caller's globs | `403 path_not_allowed`, with `path` |
| the original landed with `noop: true`, or every reverting commit would be empty | `400 nothing_to_land` |
| a landed commit can no longer be undone on *T* | `409 revert_conflict`, with a conflict report in `details.conflict` |

On every failure nothing is created: no branch, no change, no event. `reverted_by` is the highest-numbered change that reverts this one and is `draft`, `open` or `landed`; it is null when there is none, so an abandoned revert allows a new one.

### Who may revert (LAND-FR-072, LAND-FR-003)

Reverting needs `changes:write` on a PAT (`403 insufficient_scope` without it) and either write access to the target workspace, or being the author of the original change while `land.by_author` is true (`403 forbidden` otherwise). A caller without read access gets `404 not_found`; an unknown change number gets `404 change_not_found`. A token bound to a fork of the workspace gets `403 token_bound`: a fork token cannot revert on the parent.

### `parley change revert <slug> <number>`

Flags `--title`, `--intent`, `--draft`, `--auto-land`, `--session <id>`. It sends `POST /workspaces/:slug/changes/:number/revert` with the members the flags give, prints the new change as JSON on stdout and exits 0; a hub error prints the envelope on stderr and exits 1; a missing argument or a number that is not a positive integer is a usage error, exit 2, with no request sent.

### Path limits when a version is created (LAND-FR-080)

The files of a version are the paths that differ between its base and its head, a rename counting with both its old and its new path. They are computed from the repository, not from the change's stored `paths` list, which stops at 2,000. A credential with `paths` may create a version only when every one of those files matches at least one of its globs (the matcher of package 06 in `internal/identity`, not the policy matcher of package 27). The first file in byte order that matches none is named in the refusal. A credential without `paths` is never checked, and versions the hub creates on its own (a restack with actor `system`, a `sync` version) are not checked.

Package 18 checks the commits a push or an API write adds, and package 22 checks `POST …/changes`. This package covers what is left:

- **Updates that add no new commit.** A push, an API ref move or a rebase answered from existing commits that moves a branch which is the source of a `draft` or `open` change, or that opens a change, is checked against the files of the resulting version. This package supplies the push-rule engine's version-files lookup: given what the engine of package 18 passes (the slug, the branch, the new head and the flag that the update opens a change), it answers whether a version results and which files it has; for an update that opens a change the files are taken against the target branch that package 22's open defaults to. The engine refuses with rule 5 (`403 path_not_allowed` with `path` over the API; `path <path> is outside this token's allowed paths` over git), so a path-scoped token cannot adopt out-of-path commits made with another credential.
- **Restack.** `POST …/changes/:number/restack`, with or without `resolutions`, by a path-scoped caller is refused with `403 path_not_allowed` and `path` when a file of the version it would record is outside the globs; the source branch does not move and no version is recorded.

### Path limits at landing, revert and auto_land (LAND-FR-081, LAND-FR-082)

A path-scoped token may land a change, revert a change and set `auto_land: true` on a change only when every file of the change's current version matches its globs; otherwise `403 path_not_allowed` with `path`, and nothing changes: no land attempt is recorded, no branch or change is created, the flag keeps its value. For a land with `stack: true` every change the attempt would land is checked. For a revert the files are those of the landed version. Setting `auto_land` is checked wherever it can be set: `POST …/changes`, `PATCH …/changes/:number` and `POST …/revert`. Setting it to `false` is never refused.

When the flag is set with a path-scoped token, the token's globs are stored with the flag. When an automatic land is about to start, the files of the change's current version are checked against those stored globs; on a miss no attempt is created, the flag is cleared and `parley.change.land_fail` with `reason: "not_permitted"` is emitted, as for a user who may no longer land.

Path limits and policy are independent. A path-scoped token whose globs cover the change still gets `409 policy_blocked` when policy is not `ready`; a change whose policy is `ready` is still refused to a token whose globs do not cover it.

### Override (LAND-FR-002)

`POST …/changes/:number/land` may carry `override: {"reason": "<text>"}`. The shape of the member is checked by the land route of package 25 (first row). This package's refusals run as a request guard of that route, before a land attempt is created, whatever the policy status:

| Case | Answer |
|---|---|
| `override` is not an object, or `reason` is missing, not a string or empty after trimming white space | `400 invalid_request` (package 25) |
| the credential is a workspace-bound PAT (bound to the workspace or to a fork), whoever its user is | `403 override_not_allowed` |
| the caller is not the workspace's owner and not the admin token (a member with write access, an author landing by `land.by_author`) | `403 override_not_allowed` |
| the workspace setting `land.allow_override` is false, for every caller including the admin token | `403 override_not_allowed` |

An accepted override takes effect through the override rule this package registers beside the policy gate of package 25, for every change of the attempt: a change whose `landing.status` is `pending` passes. A change whose status is `blocked` (a `block` requirement or a structural reason) still ends the attempt `blocked` with `409 policy_blocked`. When at least one change passed only because of the override, the land attempt's `override` is `{"reason": …}` and each `parley.change.land` event carries it in `override`; when every change was `ready` anyway, `override` is null.

### End-to-end flows

1. An agent opens a change with `auto_land: true` in a workspace whose policy requires the check `test`. `parleyd` evaluates it `pending`. A developer's runner reports `test` as `passed`; `parleyd` re-evaluates, announces the change landable, starts a land attempt with `trigger: "auto_land"` and lands it. The agent reads the change: `landed`.
2. A developer runs `parley change revert demo 7`. The parley CLI posts to the revert route; `parleyd` builds the reverting commits, creates `revert/7`, opens change 8 with `reverts: 7` and answers `201`. The developer lands change 8; the target branch has the content it had before change 7 landed.
3. An agent holding a token with `paths: ["docs/**"]` asks to land a change that touches `src/main.go`; `parleyd` answers `403 path_not_allowed` naming `src/main.go` and records no attempt. The agent lands a change that touches only `docs/a.md` with the same token; it lands.

## Design Decisions

1. **Profile: full.** Three flows cross the CLI, the routes, the land component, the repository and the event log.
2. **Language Go; git through `internal/gitrepo` as a subprocess; SQLite behind `internal/store`; `net/http`; cobra for the CLI; a replaceable clock.** Project-wide decisions. The backoff tests rely on the clock.
3. **The override rules are this package's; the land call is package 25's.** The plan names LAND-FR-002's override rules here. Package 25 declares two registration points for them in `internal/land` and this package fills both from `internal/land/override.go`: a request guard, asked before the preconditions, that answers `403 override_not_allowed`; and the override rule, asked by the engine for a gate answer that is not `ready`, that lets `pending` pass and nothing else. Package 25 validates the member's shape, records `override` when the rule let a change pass, and package 27's gate knows nothing of overrides.
4. **Any `override` from a caller who may not override is refused, even when policy is `ready`.** LAND-FR-002 says a bound PAT "can never override"; one rule is simpler than one that depends on policy.
5. **`land.allow_override: false` answers `403 override_not_allowed` to everyone, the admin token included.** The PRD says only that overrides "can be disabled per workspace".
6. **"Owner" is the access level `owner` of package 07**, as in package 18.
7. **`reason` must be a non-empty string; no other limit.** The PRD gives none; the request body limit of package 02 bounds it.
8. **`override` is recorded only when it let a change pass**, following the land attempt field "`{reason}` when policy was overridden".
9. **`not_permitted` without an attempt is reported as `parley.change.land_fail` with `status: "blocked"` and `land_id: null`.** The catalogue allows three statuses and no attempt exists.
10. **A failed path check at the start of an automatic land is handled as `not_permitted`.** LAND-FR-081 says it is "checked the same way" and names no other outcome.
11. **The globs and the credential type are stored when the flag is set, by package 23 (with packages 22 and 23).** The flag has one store: `SetAutoLand` of `internal/change`, which the `Open` of package 22 and the `PATCH` of package 23 both call, stores the setter (user, credential type, token globs) beside the flag, and package 23 exposes it through the accessor `AutoLandSetter`. This package keeps no setter row; its own table holds only the retry count and the due time of a pending retry, and it clears the flag through the same `SetAutoLand`. `paths` is fixed at creation, so the copy is exact, and the check still works after the token is revoked or expires. `requested_by` of an automatic attempt is `{actor_type: <that credential type>, actor_id, username}` of the user who set the flag.
12. **The user-level check at start does not look at scopes.** The credential had `changes:write` when it set the flag; LAND-FR-060 says "checked as the user". A flag set with the admin token is always permitted.
13. **Setting the flag on an already landable change, and hub start, start an attempt.** Otherwise such a change waits for a transition that never comes (LAND-US-07); the PRD names only the transition.
14. **"Up to 10 times" means 10 retries after the first failed attempt**, 11 attempts in all, and the count restarts after any other outcome.
15. **A due retry is dropped when the change is not landable.** The next landable announcement starts a fresh attempt.
16. **Automatic landing never uses `stack: true`**, so LAND-FR-062 holds by one rule: no attempt while `depends_on` names an unlanded change.
17. **Reverting commits are authored and committed by the hub's landing identity**, with git's usual revert message. The PRD does not say; the caller is recorded as the change's author and the version's creator.
18. **The default intent is `Reverts change <slug>#<number>: <title>.`** The PRD asks only that it name the original.
19. **A draft reverting change counts for `already_reverted` and `reverted_by`; an abandoned one does not.** `draft: true` is a body option, and two live reverts of one change would conflict with each other.
20. **Nothing to undo is `400 nothing_to_land`**, the answer an open with no commits gets (CHG-FR-003).
21. **The revert branch is created as an API ref creation of package 18**: `Evaluate` of the push rule engine in `internal/gitserver/pushrule` with the caller's facts, then `Apply`, its ref writer, with `via: "api"` (the writer accepts only `git`, `api` and `land`), so every branch movement is one `parley.git.push`. A refusal by a push rule (for example a protected-branch glob that covers `revert/*`) is answered as package 21 answers it, `403` with the rule's `error_type`, and nothing is created.
22. **The path check at landing runs before the preconditions of LAND-FR-010 and creates no attempt.** The PRD gives it no position; a refusal that does not depend on state should not leave a record.
23. **Wiring.** Everything this package adds to another package goes through a registration point the owner declares. With `internal/land` (package 25): the override rule, request guards, a final observer and the start function for trigger `auto_land`. With the push-rule engine (package 18): its version-files lookup. With `internal/change`: an `auto_land` guard (the `AutoLandGuard` package 22 declares, registered with `RegisterAutoLandGuard`), called by the open of package 22 and the `PATCH` of package 23 before the flag is stored as `true`, with the caller's credential and the change; and a version guard (the `VersionGuard` package 24 declares, registered with `RegisterVersionGuard`), called by the manual restack of package 24 after the replay is computed and before the source branch moves, with the caller's credential and the base and head of the version to record. The landable trigger needs no registration: the automatic lander reads `parley.change.ready` events after a stored position through `ReadAfter` of the event component (package 52), so each event starts at most one attempt and none is lost over a restart.
24. **The revert's conflict report** is `ConflictReport`, the one conflict report type (declared by package 20 in `internal/gitrepo/conflictreport.go`, used by landing); `landed_changes` and every `landed_by` are empty and `change` is null.

## Dependencies

- Package 25 optimistic_landing (draft): land attempts, the land call and its policy gate, the landed commits of a change.
- Package 27 landing_policy (draft): `landing.status` per change, which the automatic lander reads, and the policy gate whose answers the override rule judges.
- Package 24 change_stacks_and_restack (draft): the manual restack, which calls the version guard this package registers.
- Package 09 cli_foundation (draft): the command helper and client `parley change revert` is built on.
- Package 52 durable_event_log_and_stream (draft): the event writer for `parley.change.revert` and `parley.change.land_fail`.
- Through them: packages 02, 06, 07, 08, 18, 20, 22 and 23.
- The `git` command, 2.38 or later, at run time.

## Glossary

- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Change**: The unit of work: a proposal to add the commits of a source branch to a target branch, with a number, a title, an intent, versions and a state.
- **Version**: One head of a change's source branch. Every push, API commit or restack that moves the source creates a new version. Old versions stay fetchable.
- **Stack**: Changes linked by `depends_on`, lowest first.
- **Restack**: The hub rebasing a dependent change onto the new head of the change below it, or onto the target once that change has landed.
- **Landing**: Adding a change's commits to its target branch: a server-side rebase followed by a compare-and-swap of the target ref.
- **Land attempt**: One request to land a change (or a stack), with its outcome.
- **Conflict report**: The structured description of why a rebase could not be done: files, kinds, and the landed changes involved.
- **Policy**: A workspace's ordered rules that map touched paths to landing requirements.
- **Bound token**: A PAT confined to one workspace (and the forks created with it).
- **Path-scoped token**: A PAT whose writes are limited to files matching a list of globs.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number.
- **Landable**: A change that is `open`, with `landing.status` `ready` and `mergeable` `clean`.
- **Setter**: The user whose credential last set `auto_land` to `true` on a change.
