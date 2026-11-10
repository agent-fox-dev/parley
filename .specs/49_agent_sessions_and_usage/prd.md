---
spec_id: "49"
spec_name: "agent_sessions_and_usage"
title: "Record agent sessions and their token usage"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "ef0fcbaba6322fa1795dcfb1c6863b81ebda450cccde16ef4264c78975b52205"
schema_version: 3
source: "docs/prd/14_sessions_audit_events.md"
---

# Record agent sessions and their token usage

## Intent

An agent's work on a workspace is spread over forks, pushes, commits, changes, claims, lands and sandboxes. The session is the one record that ties that work together and says what it cost in tokens. This package gives the hub that record: an agent opens a session for a workspace, reports token usage while it works and closes the session with an outcome; every other part of the hub can ask whether a caller may attribute work to a session and can record that a piece of work belongs to it; and anyone who may read the session sees the whole thread and the token totals in one object.

## Goals

- `POST /sessions` opens an `active` session for a workspace the caller may write to and answers `201`; the same `id` sent again by the same user for the same workspace answers `200` with the stored session.
- `POST /sessions/:id/usage` adds one usage record while the session is `active` (`201`); a replay of a usage `id` answers `200` and changes no record, no total and no metric.
- `POST /sessions/:id/complete` ends a session exactly once; every later complete answers `200` with the unchanged record.
- `GET /sessions/:id` answers one object with the token summary, the forks, changes and sandboxes of the session and eight counts, all computed when the request is served.
- `GET /sessions` lists only sessions the caller may read, with filters, and `GET /sessions/:id/usage` lists a session's usage records with totals.
- One session-linking check, exported by `internal/session` as `session.Check`, answers for a session id, a caller and a workspace whether the reference is accepted; a refused reference is reported to callers as `400 unknown_session`.
- One recording operation, exported by `internal/session` as `session.Record`, stores inside the caller's store transaction that a fork, a change version, a sandbox, a push, an API commit, a claim or a land belongs to a session.
- One closing operation, exported by `internal/session` as `session.CloseActive`, sets every `active` session of a workspace to `terminated` inside the caller's store transaction and writes one `parley.session.force_closed` event per session in that transaction.
- `GET /workspaces/:slug/cost` sums a workspace's usage records over a window, grouped by day, session or model, in token counts only.
- `parley session start|usage|complete|get|list` call these routes.

## Non-goals

- Timing out `active` sessions older than `[sessions].max_active_age`, deleting terminal sessions older than `[sessions].max_age`, removing the sessions and usage records of workspaces that no longer exist, the two `[sessions]` configuration keys, and the metrics `parley_agent_sessions_active`, `parley_agent_tokens_total` and `parley_audit_records` (§14.7.2, §14.7.3, §14.9): package 51 audit_query_and_retention. This package gives it the usage observer registration `session.OnUsage` and the count of active sessions per workspace `session.ActiveCounts`; package 51 connects both to its recorder and adds its retention operations beside this package's store in `internal/session/retention.go`.
- Run telemetry, including session outcomes pushed under a run id (AUD-FR-020 to AUD-FR-026): package 50 telemetry_ingestion.
- The event record, `seq`, visibility, the stream and the log (AUD-FR-040 to AUD-FR-049): package 52 durable_event_log_and_stream. This package writes one event type through it.
- Deciding when a workspace is archived or deleted and when a fork expires: packages 14 workspace_archive_reclone_delete and 16 task_fork_lifetime. Package 14 calls the closing operation of this package through the `SessionCloser` of its `Teardown` set, for its own archive and delete and, through its `RemoveOwned`, for every fork removal of package 16.
- Accepting `session_id` on a fork request, a push option, an API commit, a change, a land, a claim and a sandbox request, answering `400 unknown_session` or git's text form of it, and carrying `session_id` on their events: packages 15 task_fork_creation, 19 git_push_to_change, 21 scm_write_api, 22 change_opening_and_versions, 25 optimistic_landing, 30 awareness and 45 sandbox_creation_and_credentials. They call the check, and those whose interface has one the recording operation, of this package through the session linker interface each of them declares (Design Decision 17).
- The binding guard and the set of workspaces a bound token covers (PLAT-FR-081, PLAT-FR-083): package 08 workspace_bound_tokens. This package asks it one question.
- The error envelope, check order, list envelope, cursors, `Idempotency-Key`, timestamps and CLI conventions (PLAT-FR-010 to PLAT-FR-019, PLAT-FR-090 to PLAT-FR-095): packages 02 http_api_conventions and 09 cli_foundation.
- Prices. "Cost" means token counts; there is no price table and no model catalogue.
- A CLI command for the cost report: the source PRD defines none.

## Background

The repository holds no code yet; every file named here is new. The source is `docs/prd/14_sessions_audit_events.md`: §14.3.1 (identifiers and time), §14.3.2 (session, state machine, linking rule), §14.3.3 (token usage record), the session sentences of §14.3.8, AUD-FR-001 to AUD-FR-010, the session and cost routes of §14.5, the `parley session` commands of §14.6, the row `parley.session.force_closed` of §14.8.1 and the session entries of §14.10.

The code lives in `internal/session/` (records, routes, the linking check, the recording and closing operations, the cost report), in `internal/cli/session.go` (the five commands) and in `internal/hub/` (the composition of the process, where package 03 starts the hub and where this package's operations are handed to the packages that call them). Tests live beside the code; the end-to-end test is `test/e2e/session_test.go`.

Seven packages (15, 19, 21, 22, 25, 30, 45) accept a `session_id` and were specified without a dependency on this one: each declares a narrow interface for the linking check and refuses every `session_id` until an implementation is installed. Package 14 declares a session closer (`SessionCloser`) in its `Teardown` set in the same way; package 16 keeps no registry of its own and removes a fork's owned data through `RemoveOwned` of package 14, which calls that closer. This package supplies those implementations.

## Requirements

### Identifiers and the session object (§14.3.1, §14.3.2, AUD-FR-002, AUD-FR-008)

Session ids and usage ids are UUIDs in the 8-4-4-4-12 hexadecimal form. Any letter case is accepted in a body, a path or a query; ids are stored and returned in lower case. When the client sends none, the hub generates a version 4 UUID. A run id has the form `YYYYMMDD_HHMMSS_xxxxxx`, where `xxxxxx` is six lower-case hexadecimal digits. `node_id`, `archetype` and `model` are free-form labels of at most 255 characters. Timestamps are RFC 3339 UTC with whole seconds.

The session object has these members, all present in every session response:

| Member | Value |
|---|---|
| `id` | UUID |
| `workspace_slug` | the workspace the session works in; may be a task fork |
| `run_id`, `node_id`, `archetype`, `model` | string or null, as set at creation |
| `status` | `active`, `completed`, `failed`, `timeout` or `terminated` |
| `user_id` | the user who created the session; null for the admin token |
| `credential_type` | `admin_token`, `api_key` or `pat` |
| `started_at` | set at creation |
| `ended_at` | null until a terminal transition |
| `duration_ms` | integer or null |
| `error_message` | string or null |
| `cache_creation_input_tokens` | integer, default 0 |
| `metadata` | any JSON or null, as sent at creation, returned as JSON and never as a string holding JSON |
| `token_summary` | `total_input_tokens`, `total_output_tokens`, `total_cache_read_tokens`, `models_used` (distinct model names, ascending) |
| `forks` | slugs of the task forks created with this session, oldest first |
| `changes` | `{workspace, number}` of each change with at least one version attributed to this session, ordered by first attribution |
| `sandboxes` | `{workspace, id}` of each sandbox of this session, oldest first; deleted sandboxes stay listed |
| `counts` | `forks`, `changes`, `sandboxes`, `versions`, `pushes`, `commits`, `claims`, `lands` |

`token_summary`, `forks`, `changes`, `sandboxes` and `counts` are computed when the response is built: a usage report or a recorded link is visible in the next read. `forks`, `changes` and `sandboxes` hold at most 500 entries each; `counts` always holds the true totals.

States: `active` at creation; `completed`, `failed` or `timeout` by `complete`; `terminated` by a forced close. The four terminal states are absorbing. A client cannot set `terminated`.

### Opening a session (AUD-FR-001 to AUD-FR-003)

`POST /sessions` takes `workspace_slug` (required), `id`, `run_id`, `node_id`, `archetype`, `model` and `metadata`. A PAT needs `sessions:write`. The caller needs write access to the workspace, which may be a task fork. The answer is `201` with the session object: `status` `active`, `started_at` the current time, `user_id` and `credential_type` of the caller.

Failures, in the order they are decided:

| Case | Answer |
|---|---|
| a PAT without `sessions:write` | `403 insufficient_scope` |
| the body has no `workspace_slug`, or it is not a non-empty string | `400 invalid_request` |
| a bound token whose binding does not cover `workspace_slug` | `404 not_found`, `workspace not found` |
| no such workspace, or the caller's level on it is none | `404 not_found`, `workspace not found` |
| an org member (read-only) | `403 forbidden` |
| `id` is not a UUID; `run_id` does not match the run id form; `node_id`, `archetype` or `model` is not a string or is longer than 255 characters | `400 invalid_request`, the message names the member |
| the `id` exists for another user or another workspace | `409 conflict` |
| the workspace is archived | `409 workspace_archived` |

A refused request stores nothing. Creation is idempotent on `id`: when the `id` exists and was created by the same user (or both creations are by the admin token) for the same workspace, the answer is `200` with the stored session and nothing changes, also when the replayed body differs in other members. Of several concurrent creations with one `id`, exactly one stores the session and answers `201`.

### Completing a session (AUD-FR-004, AUD-FR-005)

`POST /sessions/:id/complete` takes `status` (required: `completed`, `failed` or `timeout`), `error_message`, `duration_ms` (integer ≥ 0) and `cache_creation_input_tokens` (integer ≥ 0). A PAT needs `sessions:write`. Completing an `active` session sets `status`, `ended_at` (now), `error_message`, `duration_ms` and `cache_creation_input_tokens` in one atomic step and answers `200` with the updated session. Without `duration_ms` the hub stores the milliseconds between `started_at` and `ended_at`. Completing a terminal session, `terminated` included, answers `200` with the unchanged record. When two completes race, exactly one takes effect and both answer `200`.

Who may complete or report usage: the user who created the session, with any credential of that user, and the admin token. A session opened by the admin token is completed only by the admin token.

| Case | Answer |
|---|---|
| `:id` is not a UUID | `400 invalid_request` |
| the caller may not read the session, or no such session | `404 not_found`, `session not found` |
| the caller may read the session and did not create it | `403 forbidden` |
| `status` missing, not one of the three values (`terminated` included), `duration_ms` or `cache_creation_input_tokens` negative or not an integer, `error_message` not a string | `400 invalid_request` |

### Reporting and listing usage (AUD-FR-006, §14.3.3)

`POST /sessions/:id/usage` takes `model` (required, non-blank, at most 255 characters), `id` (UUID) and `input_tokens`, `output_tokens`, `cache_read_tokens` (integers ≥ 0, default 0). Access and the first three failure rows are those of `complete`. A new record answers `201` with the usage record: `id`, `session_id`, `workspace_slug` (copied from the session), `model`, the three counts and `reported_at` (hub time).

| Case | Answer |
|---|---|
| `model` missing or blank or too long, a count negative or not an integer, `id` not a UUID | `400 invalid_request` |
| the usage `id` exists in the same session | `200` with the stored record; nothing changes |
| the usage `id` exists in another session | `409 conflict` |
| the session is not `active` | `409 session_not_active` |

A replay stores no record, changes no total and notifies no observer, also when its body differs from the stored record. After each new record, and never after a replay or a refusal, the hub notifies the registered usage observers once with the workspace, the model and the three counts; package 51 counts tokens there.

`GET /sessions/:id/usage` is readable as `GET /sessions/:id`. It answers the list envelope of usage records ordered by (`reported_at`, `id`), descending by default and ascending with `order=asc`, with `limit` and `cursor`, plus `session_id` and `totals`. `totals` has the shape of `token_summary` and covers all records of the session regardless of paging.

### Reading a session (AUD-FR-007, §14.3.8)

`GET /sessions/:id` answers `200` with the session object. A PAT needs `sessions:read` or `sessions:write`. A session is readable by:

- its creator, with any credential of that user;
- any caller with read access to the session's workspace;
- for a session on a task fork, any caller with read access to the fork's parent, also after the fork is gone;
- the admin token.

The workspace's status does not affect reads. A session whose workspace no longer exists is readable by its creator, by the admin token and, when the workspace was a task fork, by readers of its parent. For a bound token every `/sessions/:id` route answers `404` for a session whose workspace the binding does not cover, also when the token's user created the session. A caller who may not read a session gets `404 not_found` with the message `session not found`, the same answer as for an id that names no session. A `:id` that is not a UUID is `400 invalid_request`.

### Listing sessions (§14.5)

`GET /sessions` answers the list envelope of session objects the caller may read, ordered by (`started_at`, `id`), descending by default. A PAT needs `sessions:read` or `sessions:write`. Query: `workspace` (a slug); `include_forks` (`true` also lists the sessions on the task forks of `workspace`, forks that no longer exist included; default `false`); `run_id`; `status`; `credential_type`; `since` (inclusive) and `until` (exclusive) on `started_at`; `order` (`desc`, `asc`); `limit`; `cursor`. Filters combine with AND.

| Case | Answer |
|---|---|
| `status` or `credential_type` or `order` not one of its values, `include_forks` not `true` or `false`, `run_id` not in the run id form, `since` or `until` not RFC 3339, `include_forks` without `workspace` | `400 invalid_request` |
| `workspace` names a workspace the caller cannot read, or one a bound token's binding does not cover | `404 not_found`, `workspace not found` |

For a bound token the list holds only sessions whose workspace the binding covers.

### Linking work to a session (§14.3.2)

A fork request, a git push, an API commit, a change version, a claim, a land and a sandbox may name a session. `internal/session` exports the check that decides it, `session.Check`. Given a session id, the caller and the slug of the workspace of the operation, the reference is accepted when:

- the session exists,
- it was created by the caller's user, or the caller is the admin token, and
- the session's workspace and the workspace of the operation are the same workspace, or one is a task fork of the other.

The session's status is not checked: work may be attributed after the session has ended. A session on a fork that no longer exists is still accepted for an operation on that fork's parent. Otherwise the check answers "unknown session", which the calling package reports as `400` with `error_type` `unknown_session` (on a git push, in git's text form), changing nothing. An id that is not a UUID is an unknown session. A failure to read the store is reported as a failure, distinct from "unknown session", and is never an acceptance. The check writes nothing.

`internal/session` also exports the recording operation, `session.Record`. Inside the store transaction of its own operation, a calling package records one link: the session id, the kind (`fork`, `sandbox`, `version`, `push`, `commit`, `claim` or `land`), the workspace and the key of the thing linked (a fork's slug; a sandbox id; a change number with a version number; a ref name with its new commit id for a push; a commit id; a claim id; a land attempt id). The link is stored with the transaction and vanishes with it. Recording the same link twice stores it once. Recording for a session that does not exist fails and stores nothing. The session's status is not checked, so the counts of a terminal session can still grow.

The session object is built from these links: `forks` from `fork` links, `sandboxes` from `sandbox` links, `changes` from the distinct `{workspace, change number}` of `version` links in the order each change was first linked; `counts.forks`, `counts.sandboxes`, `counts.versions`, `counts.pushes`, `counts.commits`, `counts.claims` and `counts.lands` are the numbers of links of each kind, and `counts.changes` is the number of distinct changes. Links are not removed when the fork, sandbox, change or workspace they name is removed.

### Forced close (AUD-FR-009)

Archiving or deleting a workspace, and the expiry of a task fork, close all its `active` sessions. `internal/session` exports the closing operation, `session.CloseActive`, that package 14 calls, through the session closer this package registers in its `Teardown` set, inside the transaction of the workspace change (its own archive and delete, and the fork removals of package 16), with the workspace slug, the reason and the time of the operation. Each `active` session of that workspace gets `status` `terminated`, `ended_at` the given time and `error_message` the reason: `workspace archived`, `workspace deleted` or `fork expired`. One `parley.session.force_closed` event is written per closed session in the same transaction: `workspace` the session's workspace, `resource_type` `session`, `resource_id` the session id, actor `system`, metadata `reason`. Terminal sessions are not touched and get no event. When the caller's transaction (or savepoint) rolls back, no session has changed and no event exists. Any other reason is refused and nothing changes. After a forced close a usage report answers `409 session_not_active` and a complete answers `200` with the `terminated` record.

### Cost report (AUD-FR-010)

`GET /workspaces/:slug/cost` needs read access to the workspace, whatever its status, and for a PAT `sessions:read` or `sessions:write`. Query: `group_by` (`day` default, `session`, `model`), `since` (default: 24 hours before now), `until` (default: now), `limit`, `cursor`. The report sums the usage records whose `workspace_slug` is `:slug` and whose `reported_at` satisfies `since ≤ reported_at < until`. The answer is `200`:

```json
{
  "workspace": "w",
  "period": {"since": "2026-09-01T00:00:00Z", "until": "2026-09-02T00:00:00Z"},
  "group_by": "day",
  "totals": {"input_tokens": 10, "output_tokens": 0, "cache_read_tokens": 0, "sessions": 1},
  "items": [{"key": "2026-09-01", "input_tokens": 10, "output_tokens": 0, "cache_read_tokens": 0, "sessions": 1}],
  "next_cursor": null
}
```

`key` is a UTC calendar date, a session id or a model name. Items are ordered by `key` ascending. `sessions` is the number of distinct sessions in the group or the window. `period` holds the effective bounds in UTC. `totals` cover the whole window regardless of paging. A window without records answers zero totals and an empty `items`. No prices are applied. A `group_by` outside the three values, a `since` or `until` that is not RFC 3339, and a `since` that is not before `until` are `400 invalid_request`. No access is `404 not_found`, `workspace not found`.

### CLI (§14.6)

- `parley session start <workspace-slug>` with `--id`, `--run-id`, `--node-id`, `--archetype`, `--model`, `--metadata <json>`: calls `POST /sessions` and prints the session object.
- `parley session usage <session-id>` with `--model` (required), `--input`, `--output`, `--cache-read`, `--id`: calls `POST /sessions/:id/usage`.
- `parley session complete <session-id>` with `--status completed|failed|timeout` (default `completed`), `--error`, `--duration-ms`: calls `POST /sessions/:id/complete`.
- `parley session get <session-id>`: calls `GET /sessions/:id`.
- `parley session list` with `--workspace`, `--include-forks`, `--run-id`, `--status`, `--since`, `--limit`, `--cursor`: calls `GET /sessions`.

Each prints the hub's answer as one JSON document on stdout and exits 0. A hub error is printed as the error envelope on stderr with exit status 1. A missing argument, a missing `--model`, and a `--metadata` value that is not JSON are usage errors: exit status 2, and no request is sent.

### End-to-end flows

**An agent reports a session.** The agent runs `parley session start demo --model m1`; the parley CLI posts to `parleyd`, which stores an `active` session and answers `201`. The agent runs `parley session usage <id> --model m1 --input 10 --output 5`; `parleyd` stores the record. The agent runs `parley session complete <id>`; `parleyd` sets `completed`. `parley session get <id>` prints the session with `token_summary.total_input_tokens` 10.

**A developer follows the thread and the cost.** `parleyd` accepts a session reference for an operation on a fork of the session's workspace and records a fork link and a version link in the operation's transaction. The developer reads `GET /sessions/<id>` and sees the fork, the change and the counts, then reads `GET /workspaces/demo/cost` and sees the tokens of the day.

**A workspace change closes sessions.** `parleyd`, inside the transaction of a workspace change, closes the workspace's active sessions with reason `workspace archived`. An org member reads the session and sees `terminated`; the event log holds one `parley.session.force_closed` for it.

## Design Decisions

1. **Stack (project-wide).** Go; HTTP on the standard library `net/http` with its method-and-pattern router; the store is embedded SQLite through `modernc.org/sqlite` behind `internal/store`; the CLI uses `github.com/spf13/cobra`; tests use `testing` with tables and `net/http/httptest`; code takes a replaceable clock.
2. **Sessions, usage records and links live in the transactional store**, not the analytical one. Why: the forced close and a recorded link must commit or roll back with another package's transaction.
3. **Links are stored by this package, in one table.** The source says the lists and counts are "computed at read time" and not from what. The packages that own forks, changes and sandboxes do not depend on this one, so the session record cannot query their tables without an import cycle. A link table written through the recording operation keeps one direction of calls, keeps a deleted sandbox and an expired fork listed, and lets the counts of an ended session grow.
4. **A session stores the parent slug of its workspace at creation** when that workspace is a task fork. Why: readers of the parent keep reading the session, and the linking rule keeps holding, after the fork is gone.
5. **Bound-token coverage is asked through a narrow interface declared in `internal/session`** (is the caller's credential bound, and does its binding cover this slug). `parleyd` installs the coverage component of package 08. With none installed no caller counts as bound, because bound tokens do not exist without package 08.
6. **Order of checks on `POST /sessions`.** The workspace is named in the body, so `workspace_slug` is read before access is decided; every other member is validated after access (step 8 of PLAT-FR-013). The `id` lookup (replay, conflict) comes before the archived check. Why: a replay creates nothing, so it does not need a workspace that accepts new sessions.
7. **Order of checks on `usage`.** The usage `id` lookup comes before the `active` check, for the same reason: a replay after the session ended still answers `200`.
8. **A workspace in a status other than `archived`** (`pending`, `cloning`, `failed`) accepts new sessions. The source names only the archived refusal.
9. **A forced close leaves `duration_ms` null.** The source says the value comes from `complete`.
10. **The `parley.session.force_closed` event carries the closed session's id in `session_id`.** The source gives the resource fields only; with the id set, a stream filtered by `session_id` shows the end of the session.
11. **The closing operation closes the sessions of exactly the slug it is given.** The session closer registered with package 14 receives the slugs and the reason and calls it once for each slug, so it runs once for each workspace or fork that packages 14 and 16 remove or archive. A second call for the same slug closes nothing and writes no event.
12. **Which 500.** A list longer than 500 entries holds the first 500 in its stated order.
13. **Sibling forks do not link.** A session on one fork is not accepted for an operation on another fork of the same parent: neither is a fork of the other.
14. **The cost report counts a workspace's own records.** Records of sessions on the workspace's forks belong to the forks' slugs and are not included. `since` and `until` accept any offset and are normalised to UTC, truncated to whole seconds.
15. **`GET /sessions` does not compare `since` with `until`.** A window that holds nothing answers an empty list; the source lists the comparison as an error only for the cost report.
16. **The usage observer and the active-session count are this package's hand-off to package 51**, which owns the metrics registry and its recorder (`Add`, `Set`, `Observe`, `SetFamily` in `internal/metrics`). This package exports `session.OnUsage(observer)`, where an observer takes the workspace slug, the model and the three counts, and `session.ActiveCounts(ctx)`, which returns the number of active sessions per workspace slug. Package 51, which is built after this one, connects them in `internal/hub`: each observer call becomes `Add` on `parley_agent_tokens_total` once per direction, and its `gauges` step passes `ActiveCounts` to `SetFamily` on `parley_agent_sessions_active`. Observers are notified after the usage record is committed. This package imports nothing of `internal/metrics`.
17. **Installing the operations in other packages.** The exported names are fixed: `session.Check(ctx, caller, sessionID, workspaceSlug)` returns the lower-case session id or `session.ErrUnknownSession`; `session.Record(ctx, tx, link)` stores one `session.Link` in the caller's transaction; `session.CloseActive(ctx, tx, workspaceSlug, reason, at)` is the forced close. `internal/hub/session.go` registers `session.Check` and `session.Record`, through one adapter per consumer, with the interface each consumer declares in its own package, when that package exists in the repository at this point; a calling package implemented later installs the same two functions itself. Why: the plan gives this package no dependency on its callers. What each consumer exposes:

    | Package | Go package | Interface the hub fills, as its owner declares it | Operations | Link kind |
    |---|---|---|---|---|
    | 15 task_fork_creation | `internal/fork` | `SessionLinker` | check: caller, session id, the parent's slug; accepted, refused or failed; record: the fork's creating transaction, session id, the parent's slug, the fork's slug | `fork` |
    | 19 git_push_to_change | `internal/gitserver` | `SessionLinker` | check alone: caller, session id, workspace; accepted, refused or internal error | none: a push becomes a link through the version package 22 records |
    | 21 scm_write_api | `internal/scm` (`session.go`) | `SessionLinker`, with its registration call | check alone: caller, session id, workspace; accepted, refused or internal error | none: an API commit becomes a link through the version package 22 records |
    | 22 change_opening_and_versions | `internal/change` (`collaborators.go`) | `SessionLinker`, taken at construction | check: caller, session id, workspace; accepted, refused or internal error; record: the transaction that opens the change or records the version, session id, workspace, change number, version number | `version` |
    | 25 optimistic_landing | `internal/land` | the session interface | check and record | `land` |
    | 30 awareness | `internal/awareness` | the session interface | check and record | `claim` |
    | 45 sandbox_creation_and_credentials | `internal/sandbox` | `SessionLinker`, with its registration function | check: caller, session id, workspace slug; record: the creating transaction, session id, workspace slug, sandbox id | `sandbox` |

    The seven consumers declare different shapes (argument order, a check alone or a check and a record, registration by a call or at construction), so the hub registers one adapter per consumer, each written to that consumer's declaration, and none of them shares a Go type with another. An adapter's check calls `session.Check` and maps `session.ErrUnknownSession` to the consumer's refusal and every other error to its failure (to a refusal where the interface has no failure answer: an unverifiable reference is never accepted). An adapter's record builds the `session.Link` of the consumer's kind from the consumer's open store transaction, the session id, the workspace slug and the key of the thing linked, and calls `session.Record`. Record is called by five consumers: package 15 (`fork`), package 22 (`version`, the link of a change to the session), package 25 (`land`), package 30 (`claim`) and package 45 (`sandbox`). Packages 19 and 21 declare the check alone, so their adapters carry the check only: a push and an API commit become session links only through the change version package 22 records for them, and a push or commit that belongs to no change records no link.

    The forced close has one registration: `internal/hub/session.go` registers a session closer as the `SessionCloser` of the `Teardown` set that package 14 declares in `internal/workspace/teardown.go` (the transaction, the slugs and a reason). The closer calls `session.CloseActive` once per slug with the reason it receives and the hub clock's time. Package 14 calls it inside the transaction of an archive with `workspace archived`, and, through its `RemoveOwned`, inside every removal: its own delete with `workspace deleted`, and the fork removals of package 16 with `workspace deleted` or, for the expiry sweep, `fork expired`. Package 16 keeps no registry of removal participants, so this package registers nothing with `internal/fork` for removal and maps no causes: the reason arrives as text. It registers no archive observer either, because the close belongs inside the archive's transaction, where the closer is called.
18. **Profile: full**, with three execution paths.

## Dependencies

- Package 07 `workspace_access_rule` (draft): the caller's level on a workspace, the readable set, and the minimal workspace record (slug, kind, parent, status) this package reads.
- Package 09 `cli_foundation` (draft): the CLI's HTTP client, output and exit codes, on which the five commands are built.
- Package 52 `durable_event_log_and_stream` (draft): writing `parley.session.force_closed` in the transaction of the forced close.
- Through them: package 02 `http_api_conventions` (route registration, error envelope, list envelope and cursors), package 03 `server_lifecycle_and_configuration` (the store, its migrations and `internal/hub`), package 04 `authentication_and_login` (the authenticated caller and its credential type) and package 06 `api_keys_and_access_tokens` (the scopes `sessions:read` and `sessions:write`).
- Installed at start, not required to build or test this package: the coverage component of package 08; the callers of the linking check and the recording operation (packages 15, 19, 21, 22, 25, 30, 45) and of the closing operation (package 14, whose `Teardown` set also serves the fork removals of package 16); the metrics of package 51.

## Verified External API

- `github.com/spf13/cobra` — **unverified** (not yet in `go.mod`): `cobra.Command` with `Use`, `Short`, `Args`, `RunE func(cmd *cobra.Command, args []string) error`; `(*Command).AddCommand(cmds ...*Command)`; `(*Command).Flags() *pflag.FlagSet`; `cobra.ExactArgs(n int) cobra.PositionalArgs`.

## Glossary

- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires: after 7 days by default, or 24 hours after all its changes have landed or been abandoned.
- **Bound token**: A PAT confined to one workspace (and the forks created with it). A token bound to a fork also reaches its own changes, and the awareness and policy reads, on the fork's parent (§3).
- **Session**: An agent session: who ran, on which workspace, with what token usage, and which changes it produced.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
- **Usage record**: One token usage increment of a session: a model name and three token counts, stamped with hub time.
- **Session link**: The stored fact that one fork, sandbox, change version, push, API commit, claim or land belongs to a session.
- **Session-linking rule**: The three conditions under which a caller may attribute an operation on a workspace to a session.
