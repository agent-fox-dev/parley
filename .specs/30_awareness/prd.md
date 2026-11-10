---
spec_id: "30"
spec_name: "awareness"
title: "Show agents what other work touches their paths"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "06c542e136ad653386b7857e5c947ff19de58bbdc5a7bdc37346bfd7275f1d54"
schema_version: 3
source: "docs/prd/09_awareness.md"
---

# Show agents what other work touches their paths

## Intent

When many agents work on one repository, the expensive conflicts are the ones nobody saw coming. This package makes in-flight work visible early: before an agent starts it can ask who else touches the paths it is about to edit and announce its own plan as an expiring claim, and while it works it is told when another change starts to touch the files of its own change. Awareness is advisory: it informs, and it never refuses a push, a commit or a land.

## Goals

- `GET /api/v1/workspaces/:slug/activity` answers, for a list of path globs and a target branch, with the `draft` and `open` changes that touch a matching file, the active claims whose globs can match the same files, and the changes that landed on the target since a commit or a time.
- `GET /api/v1/workspaces/:slug/changes/:number/overlaps` lists every other `draft` or `open` change with the same target branch and at least one touched file in common, largest overlap first.
- The hub keeps, per workspace and target branch, the full set of files each change touches, and brings it up to date after each new version, retarget, land, abandon and restore, without delaying the request that caused the update.
- When two changes start to overlap, each gets exactly one `parley.change.overlap` event naming the other; the pair is announced again only after it stopped overlapping.
- A claim is created, read, listed, renewed and released through five routes; creating one returns the current activity for its paths in the same answer.
- A claim expires by itself at `expires_at`, is linked to the change its session opens, becomes `fulfilled` when that change lands or is abandoned, and is removed 7 days after it ended.
- `parley activity`, `parley change overlaps` and `parley claim create|list|renew|release` drive those routes, and `--fail-on-overlap` exits 3 when something overlaps.

## Non-goals

- A message board or chat between agents, locking of paths, and line-level or hunk-level merge prediction: out of the product's scope (§9.1, §17).
- The change object, opening changes, versions, the `paths` field of a change and the `parley.change.version` event: package 22 `change_opening_and_versions`. The `overlaps` expansion of `GET …/changes/:number?include=overlaps` is a member of package 22's route; this package registers the `overlaps` expander with the include expander registry of package 22, backed by the function that computes the list.
- Retarget, abandon and restore of a change and `mergeable`: package 23 `change_lifecycle_and_mergeability`. Stacks and `depends_on`: package 24 `change_stacks_and_restack`. Landing and the `parley.change.land` event: package 25 `optimistic_landing`. This package only reads the events and the change records those packages write.
- The binding guard, including `403 token_bound` for a claim write that a fork-bound token sends with the parent's slug, and the list of awareness read routes such a token reaches on the parent (AWR-FR-001, PLAT-FR-081, PLAT-FR-083): package 08 `workspace_bound_tokens`, whose table already holds the four read routes of this package.
- The access rule and its levels: package 07 `workspace_access_rule`. The scope registry and the scope check: package 06 `api_keys_and_access_tokens`. The path-glob matcher: package 06.
- The error envelope, the order of checks, list paging, `Idempotency-Key` (PLAT-FR-010 to PLAT-FR-019): package 02 `http_api_conventions`. CLI output, exit codes and the HTTP client (PLAT-FR-090 to PLAT-FR-095): package 09 `cli_foundation`.
- Storing and streaming events, and filtering the stream by `session_id`: package 52 `durable_event_log_and_stream`.
- Sessions, the session-linking rule itself and the `claims` count of a session record: package 49 `agent_sessions_and_usage`. This package asks its linking check and records one `claim` link per claim that names a session, through the session interface that package 49 installs.
- Removing a workspace's claims when the workspace is deleted (WS-FR-065): package 14 `workspace_archive_reclone_delete`, which runs the remover of owned data that this package supplies for its collaborator set in `internal/workspace`.
- The maintenance loop, the configuration loader and background work: package 03 `server_lifecycle_and_configuration`. This package registers one task, three keys and two pieces of background work with them.

## Background

The repository holds no code yet; every file named here is new. Changes and their store arrive with package 22 in `internal/change/`; the event component with package 52 in `internal/event/`; the access component with package 07 in `internal/access/`; the router, error envelope and list helpers with package 02 in `internal/server/`; the configuration loader, the store, background work and the maintenance loop with package 03; the git wrapper in `internal/gitrepo/`; the CLI root, HTTP client and exit codes with package 09 in `internal/cli/`.

This package adds `internal/awareness/` (the overlap index and its worker, the overlap events, the activity and overlaps routes, claims and their background work), the command files `internal/cli/activity.go`, `internal/cli/claim.go` and `internal/cli/change_overlaps.go`, and the end-to-end test `test/e2e/awareness_test.go`.

Source: `docs/prd/09_awareness.md`, AWR-FR-001 to AWR-FR-011, with the routes of §9.5, the commands of §9.6, the background behaviour of §9.7, the events of §9.8 (and §14.8.4, §14.8.5), the configuration of §9.9 and the edge cases of §9.10.

## Requirements

### Access and addressing (AWR-FR-001)

All routes are under `/api/v1/workspaces/:slug`. The four reads (`GET …/activity`, `GET …/changes/:number/overlaps`, `GET …/claims`, `GET …/claims/:id`) need the scope `changes:read` from a PAT and read access to the workspace. The three claim writes (`POST …/claims`, `PATCH …/claims/:id`, `DELETE …/claims/:id`) need `changes:write` and read access: claims change nothing in the repository, so an org member may make them (PLAT-FR-062). A PAT without the scope gets `403 insufficient_scope`; a caller without read access gets `404 not_found`, the same answer as for an unknown slug.

Through a task fork's slug the routes act on the fork's parent: access is decided on the fork, and the activity, the overlaps, the change numbers and the claims are the parent's. A claim made through a fork's slug is stored on the parent and carries the parent's slug in `workspace_slug`.

A claim can be renewed, edited and released by its creator (the user behind the credential that made it, with any credential of that user), by the owner of the workspace the claim is stored on, and by the admin token. Any other reader gets `403 forbidden` and the claim is unchanged. A claim id that names no claim of the addressed workspace is `404` with `error_type` `claim_not_found`.

The routes have no state precondition: they answer in every workspace status.

### The overlap index (§9.7, AWR-FR-006, §9.10)

The hub keeps, per workspace and target branch, which `draft` and `open` changes touch which files. A change's files are those that differ between the `base_sha` and the `head_sha` of its current version; a renamed file counts under both of its paths. A change touching more than 2,000 files is indexed in full; only lists returned to clients are capped. Landed changes stay in the index, marked as landed with their landed commit and time, so that activity can report them.

The index is updated after each new version, retarget, land, abandon and restore. The update is asynchronous: the push, commit or API call that caused it is answered without waiting for it, and a failure of the update never changes that answer. The update is durable: after a restart the hub continues where it stopped and announces no overlap twice. If the files of a version cannot be listed (its commits are gone after a reclone), the change is recorded with no files and the work continues with the next change.

### Overlaps of a change (AWR-FR-004, §9.3)

Two changes overlap when both are `draft` or `open`, have the same target branch and have at least one touched file in common. Changes in one stack (linked through `depends_on`, directly or through other changes) do not overlap each other. Overlap is on file paths, not on lines: two changes that edit different functions of one file overlap.

`GET …/changes/:number/overlaps` answers `200` with a list (`items`, `next_cursor`) of overlap entries, one for every other change that overlaps this one, ordered by `count` descending, then `number` ascending. For a change that is `landed` or `abandoned` the list is empty. A number that names no change of the workspace is `404` with `error_type` `change_not_found`.

An overlap entry has `change` (a summary: `number`, `ref`, `title`, `intent` cut to its first 500 characters, `state`, `author`, `session_id`, `version`, `updated_at`, `mergeable`, and `landing` with `status`), `paths` (the files in common in ascending order, at most 100), `paths_truncated` (true when there are more than 100) and `count` (the number of files in common).

### Overlap events (AWR-FR-005, §9.8)

When an index update makes two changes overlap that did not overlap before, the hub emits one `parley.change.overlap` for each of the two, naming the other. The event has `resource_type` `change`, `resource_id` the change number, the target workspace as `workspace`, the actor `system`, the change's own `session_id`, and metadata `number`, `with` (`number` and `ref` of the other change), `with_session_id`, `paths` (at most 100) and `count`. The pair is announced once: further versions that keep the two overlapping emit nothing. When they stop overlapping (no common file any more, different targets, or one of them landed or abandoned) the pair is forgotten, and a later overlap is announced again.

### Activity (AWR-FR-002, AWR-FR-003)

`GET …/activity` takes `paths` (required: one or more globs, as a comma list, repeated, or both; at most 50), `target` (default: the default branch of the workspace acted on), `since`, `exclude_session` and `exclude_change`. It answers `200` with:

- `target_branch` and `target_sha` (the head of that branch, or `null` when the workspace has no such branch);
- `changes`: the `draft` and `open` changes on that target with a touched file matching any glob, as overlap entries whose `paths` and `count` are the matching files, ordered by `count` descending, then `number` ascending;
- `claims`: the active claims on that target with a glob that can match the same files as a requested glob, newest first. Two globs are treated as matching when either one, used as a pattern, matches the other's literal prefix (its text before the first `*`); this errs on the side of reporting;
- `landed`: the changes landed on that target after `since` that touched a matching file, newest first, at most 100, each `{number, ref, title, landed_sha, landed_at, session_id, paths}`.

`since` is a full 40-character commit id (typically the caller's fork point or base: the changes whose landed commit is not reachable from it are reported) or an RFC 3339 timestamp (the changes landed after it). Its default is 24 hours before the request.

With `exclude_session=<id>` the changes, claims and landed changes of that session are left out; with `exclude_change=<number>` that change and the claims linked to it are left out.

Errors, all `400 invalid_request`: `paths` missing or empty; more than 50 globs; a glob that is empty, longer than 255 characters or starts with `/`; a `since` that is neither a 40-character commit id nor a timestamp, or a commit id the repository does not hold; an `exclude_change` that is not a positive integer.

Activity shows changes whose source is a fork the caller cannot read; the change lives in the target workspace, which the caller can read. Drafts are included.

### Claims (AWR-FR-007, AWR-FR-008, AWR-FR-010, AWR-FR-011, §9.3)

A claim has `id` (UUID), `workspace_slug`, `paths` (1–50 globs), `target_branch`, `intent` (1–2,000 characters), `by` (`{actor_type, actor_id, username}`), `session_id` (string or null), `change` (int or null), `expires_at`, `state` (`active`, `released`, `expired`, `fulfilled`) and `created_at`.

`POST …/claims` takes `{"paths", "intent", "target_branch"?, "ttl"?, "session_id"?}`. `target_branch` defaults to the default branch of the workspace acted on; `ttl` defaults to `[awareness].default_claim_ttl` and may not exceed `[awareness].max_claim_ttl`. The answer is `201 {"claim": <claim>, "activity": <the activity answer for the claim's paths and target, without the claim itself>}`, and `parley.claim.create` is emitted (`resource_type` `claim`, `resource_id` the id, `workspace` the workspace the claim is stored on, the caller as actor, metadata `paths`, `intent`, `target_branch`, `expires_at`). Claims never conflict with each other: two sessions may claim the same paths, and each sees the other in the activity. A claim on paths that do not exist yet is valid.

Errors: `400 invalid_request` for an invalid body (`paths` not an array of 1–50 strings, a glob that is empty, longer than 255 characters or starts with `/`, `intent` missing or outside 1–2,000 characters, `ttl` not a duration, not positive or above the maximum, `target_branch` empty or longer than 255 characters); `400 unknown_session` when `session_id` fails the session-linking rule of §14; `429` with `error_type` `claim_limit_reached` when the workspace already has `[awareness].max_claims_per_workspace` active claims or the session already has 50. A refused request stores nothing and emits nothing.

`GET …/claims` lists claims newest first with the filters `state` (default `active`), `session_id` and `path` (a glob, matched by the rule above), paged with `limit` and `cursor`. `GET …/claims/:id` reads one.

`PATCH …/claims/:id` takes `{"ttl"?, "intent"?}`: `ttl` moves `expires_at` to the time of the request plus `ttl` (at most the maximum), `intent` replaces the intent. It answers `200` with the claim. A claim that is not active is `409` with `error_type` `claim_not_active`. A body with neither member, or an invalid one, is `400 invalid_request`.

`DELETE …/claims/:id` releases an active claim: its state becomes `released`, `parley.claim.release` is emitted (metadata `paths`, the caller as actor) and the answer is `204`. Releasing a claim that is not active is a no-op that also answers `204` and emits nothing.

Claims have no effect on pushes, commits, policy or landing.

### Claim lifetime (AWR-FR-008, AWR-FR-009, §9.7)

A claim past its `expires_at` counts as `expired` at once in every read and in the limits. Every minute the hub marks such claims `expired` in the store and emits `parley.claim.expire` for each (actor `system`, metadata `paths`).

When a change is opened with a `session_id` that has active claims on the same workspace and target branch, those claims get `change` set to its number. When that change lands or is abandoned, its claims that are still active become `fulfilled`.

A claim that is `released`, `expired` or `fulfilled` is kept for 7 days and then removed by the `claim_cleanup` task of the hourly maintenance loop.

### Configuration (§9.9)

| Key | Type | Default | Effect |
|---|---|---|---|
| `[awareness].default_claim_ttl` | duration | `1h` | TTL of a claim without `ttl` |
| `[awareness].max_claim_ttl` | duration | `24h` | Longest TTL |
| `[awareness].max_claims_per_workspace` | int | `1000` | Active claims per workspace |

A duration that is not positive, a default above the maximum, or a claim limit below 1 stops the start with an error naming the key.

### CLI (§9.6)

- `parley activity <slug>` with `--paths <list>` (required), `--target <branch>`, `--since <sha|time>`, `--exclude-session <id>`, `--exclude-change <number>`, `--fail-on-overlap`: calls `GET …/activity`.
- `parley change overlaps <slug> <number>` with `--fail-on-overlap`: calls `GET …/changes/:number/overlaps`.
- `parley claim create <slug>` with `--paths <list>` (required), `--intent <text>` (required), `--target <branch>`, `--ttl <duration>`, `--session <id>`: calls `POST …/claims`.
- `parley claim list <slug>` with `--state`, `--session`, `--path`, `--limit`, `--cursor`: calls `GET …/claims`.
- `parley claim renew <slug> <id>` with `--ttl <duration>` (default `1h`): calls `PATCH …/claims/:id`.
- `parley claim release <slug> <id>`: calls `DELETE …/claims/:id` and prints `{}`.

Each command prints the route's response body and exits by the global rules. With `--fail-on-overlap` the result is printed first; then, when `changes` or `claims` of the activity answer (or `items` of the overlaps answer) is not empty, `condition_not_met` is reported on stderr and the exit status is 3. A missing required flag, a wrong number of arguments or a number that is not a positive integer is a usage error (exit status 2) and no request is sent.

### End-to-end flows

**Two agents meet on one file.** An agent opens change 1 from a branch that edits `api/handler.go`. A second agent runs `parley claim create demo --paths 'api/**' --intent "rework handlers" --session <id>`; the parley CLI posts the claim; `parleyd` stores it, emits `parley.claim.create` and answers with the claim and an activity that lists change 1. The second agent goes on anyway and opens change 2, which edits the same file; `parleyd` accepts it, the claim gets `change: 2`, the index worker records both changes and emits one `parley.change.overlap` on each. The first agent runs `parley change overlaps demo 1 --fail-on-overlap`, reads the entry for change 2 and gets exit status 3.

**A crashed agent's claim disappears.** An agent creates a claim with a `ttl` of 5 minutes and stops. Six minutes later `parleyd`'s expiry run marks the claim `expired` and emits `parley.claim.expire`; activity for the same paths no longer returns it. Seven days later the maintenance loop removes the record.

## Design Decisions

1. **Profile: full.** Two end-to-end flows cross the CLI, the router, the store, the event log and the git wrapper.
2. **Project decisions relied on.** Go; embedded SQLite through `modernc.org/sqlite` behind `internal/store`; `net/http` with its method-and-pattern router; `github.com/spf13/cobra` for the CLI; git through the `git` binary behind `internal/gitrepo`; the standard `testing` package with table-driven tests, `net/http/httptest` and repositories made with `git init` in `t.TempDir()`; a replaceable clock.
3. **The index follows the event log.** A background worker reads the event log in sequence order with `ReadAfter(position, limit)` of the event component of package 52 (`internal/event`), which returns the events after a position and the position up to which the log was examined, starting after a position it stores in the transactional store. It acts on the types `parley.change.version` (which also carries retargets), `parley.change.land`, `parley.change.abandon` and `parley.change.restore`, skips every other type, and moves its position to the examined one. The index update, the overlap events it causes, the claim links and the new position are written in one transaction. Why: it is asynchronous by construction (AWR-FR-006), survives restarts without announcing twice, and needs no call from packages 22, 23 and 25 into this one. The worker looks for new events every second; the PRD gives no latency figure.
4. **Narrow interfaces instead of imports.** `internal/awareness` defines the three things it needs from others as interfaces that `parleyd` wires: an event source, whose one method has the shape of `ReadAfter(position, limit)` of package 52 so that the event component satisfies it as it is; a change reader (one change by workspace and number, with state, target branch, `depends_on`, session, version, `base_sha`, `head_sha`, landed commit and time, and the summary fields), which the exported read of the change component of package 22 implements; and a session interface with two operations, check and record, which package 49 installs from the linking check and the recording operation that `internal/session` exports. Why: the three owners export these under their own names, and `internal/awareness` stays testable with fixtures. With no session interface installed, every `session_id` is refused with `unknown_session`.
5. **A change's files are computed here, not taken from the change's `paths`.** `paths` is capped at 2,000; the index is not (§9.10). The files come from one name-and-status diff with rename detection between `base_sha` and `head_sha`.
6. **Stack membership is read at evaluation time**, by following `depends_on` to the bottom change; two changes with the same bottom are one stack. It is not stored in the index, so a `depends_on` edit needs no index update.
7. **Any index update can announce.** A retarget or a restore that makes two changes overlap is announced like a new version. Why: §9.8 words the event as "two changes started to overlap", and the pair record must agree with the index whatever caused the update.
8. **Order and caps the PRD leaves open.** Equal `count` is ordered by `number` ascending. `paths` in an entry, in an event and in a `landed` item are in ascending byte order and cut to 100. The overlaps list is paged by the list conventions; `changes` and `claims` of the activity answer are not paged and not capped.
9. **`target` is not validated.** An unknown branch gives `target_sha: null` and, normally, empty lists; a claim may name a branch that does not exist yet. Why: awareness is advisory and a claim on paths that do not exist yet is valid (§9.10).
10. **Activity and the claim lists are limited to the target branch**, because the index is kept per target branch (§9.7).
11. **Through a fork's slug the default target is the parent's default branch**, not the fork's base branch. Why: §9.3 says "default: the default branch".
12. **`since` forms.** Exactly 40 hexadecimal characters is a commit id; anything else must parse as an RFC 3339 timestamp. A commit id selects the landed changes whose `landed_sha` is not reachable from it.
13. **A claim is expired from `expires_at` on**, also before the minute's run has written the state: reads report `expired`, activity leaves it out, the limits do not count it, `PATCH` answers `claim_not_active`. The event follows with the run. Why: the acceptance criterion (§16, 11) expects activity to stop returning the claim when it expires.
14. **Limits.** The workspace limit counts active claims stored on the workspace (for a fork: its parent). The session limit of 50 counts active claims that carry that `session_id`; a claim without a session counts for no session. The refusal is `429` with `error_type` `claim_limit_reached`, a type of its own like the other quantity limits of the product (`fork_limit_reached`, `secret_limit_reached`, `sandbox_capacity`); `rate_limited` stays the answer of the request rate limits of package 02 alone. AWR-FR-011 fixes the status and names no type.
15. **`ttl` bounds.** At least 1 second, at most `[awareness].max_claim_ttl`; a renewal without `ttl` leaves `expires_at` alone.
16. **Renewal emits no event**; §9.8 lists none.
17. **Linking a claim to a change** happens when the worker processes the first version of a change (`version` 1) that carries a session: every active claim of that session on the same workspace and target branch whose `change` is null gets the number. A restored change does not turn its `fulfilled` claims back.
18. **`fulfilled` claims are removed like the others**, 7 days after they became `fulfilled`. The store keeps the time a claim ended for this; it is not part of the claim object.
19. **A fork-bound token may read the overlaps of any change of the parent**, not only of its own changes. Why: AWR-FR-001 lists the route without the restriction CHG-FR-040 puts on the change routes, and activity shows the same changes.
20. **No state precondition.** The routes answer in every workspace status, also on an archived workspace: they neither read nor write the repository's branches, except for `target_sha` (null when unreadable) and a commit `since` (`400` when the commit cannot be resolved).
21. **A malformed claim id or change number is "not found"** (`404 claim_not_found`, `404 change_not_found`), not `400`.

## Dependencies

- Package 22 `change_opening_and_versions` (draft): the change records and versions this package reads, the `parley.change.version` event, and the opening of changes used by the end-to-end test. Through it this package uses package 07 (access levels), package 06 (scope check and the path-glob matcher), package 03 (store, configuration loader, background work, maintenance loop), package 02 (router, error envelope, list paging) and `internal/gitrepo`.
- Package 52 `durable_event_log_and_stream` (draft): the event component the four events are emitted through and the worker reads from.
- Package 09 `cli_foundation` (draft): the CLI root, the HTTP client, output, exit codes and the shared exit with status 3.

## Verified External API

- `github.com/spf13/cobra`: `cobra.Command`, `(*cobra.Command).AddCommand`, `(*cobra.Command).Flags`. **Unverified**: the module is not yet in `go.mod`; the signatures are the ones the library documents.

## Glossary

- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires.
- **Change**: The unit of work: a proposal to add the commits of a source branch to a target branch, with a number, a title, an intent, versions and a state.
- **Version**: One head of a change's source branch. Every push, API commit or restack that moves the source creates a new version.
- **Intent**: The author's statement of why the change exists.
- **Stack**: Changes linked by `depends_on`, lowest first.
- **Claim**: An advisory, expiring statement that a session intends to work on some paths.
- **Session**: An agent session: who ran, on which workspace, with what token usage, and which changes it produced.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
- **Bound token**: A PAT confined to one workspace (and the forks created with it). A token bound to a fork also reaches its own changes, and the awareness and policy reads, on the fork's parent (§3).
- **Overlap**: Two `draft` or `open` changes with the same target branch and at least one touched file in common, not in one stack.
- **Overlap entry**: The summary of another change together with the files in common and their count.
- **Activity**: The answer to "what touches these paths": in-flight changes, live claims and recently landed changes, each matched against a list of path globs.
- **Literal prefix**: The text of a glob before its first `*`.
