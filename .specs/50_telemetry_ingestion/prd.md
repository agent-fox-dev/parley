---
spec_id: "50"
spec_name: "telemetry_ingestion"
title: "Ingest agent run telemetry"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "7209f3716d2eb4d7a1b30a7524c12214bc35b65bf330ca48c78b6ea94a21d84f"
schema_version: 3
source: "docs/prd/14_sessions_audit_events.md"
---

# Ingest agent run telemetry

## Intent

Agents push records about their runs to the hub — run events, session outcomes, tool calls, tool errors, conversation traces and a postmortem — so that a run can be audited later. The hub validates every record, stores it once under its id however often it is sent, and records on its own what was done inside a sandbox that belongs to a run. Without this an agent's work leaves no trace beyond the commits it produced.

## Goals

- Eight `POST` routes under `/api/v1/workspaces/:slug/runs/:run_id/` store run events, session outcomes, tool calls, tool errors, traces and the postmortem of a run, single and (for run events and traces) in batches of 1 to 1000 items.
- Every route needs the scope `audit:write` and write access to the workspace, answers `404` for a workspace that does not exist to every caller, and `409 workspace_archived` for an archived one.
- Every field is validated; a violation answers `400 invalid_request` with a message that names the field.
- A record is idempotent on its `id` (a postmortem: on workspace and run): new answers `201`, a replay answers `200` with the stored record, an `id` used in another workspace or run answers `409 conflict`.
- A batch answers `200` with `{accepted, duplicates, errors}`; its valid items are stored together or not at all.
- A write that does not finish within `[audit].write_timeout` (default `2s`) answers `503 unavailable` with `Retry-After: 5` and stores nothing.
- Exec, attach and file transfers in a sandbox whose session has a `run_id` are recorded by the hub as run events that never hold input, output or file content.

## Non-goals

- Reading telemetry back: the per-run lists, the postmortem read, the transcript and the unified audit query (AUD-FR-030 to AUD-FR-034) are package 51 audit_query_and_retention. This package stores the records in the shape those reads return.
- Retention of telemetry (`run_records`, `max_runs`, `traces`, `postmortems`, `orphans`), the keys `[audit].max_age`, `[audit].max_runs`, `[audit].trace_max_age`, `[audit].postmortem_max_age` and `[audit].orphan_retention`, and the Prometheus metrics (`parley_audit_events_total`, `parley_audit_records`): package 51. This package only tells the observers registered with `telemetry.OnRunEvent` about each run event it stores; package 51 connects that registration to its recorder.
- Sessions and token usage, and the session-linking rule: package 49 agent_sessions_and_usage. A `session_id` in a telemetry record is not checked against the stored sessions.
- Hub events (`parley.*`), the event log and the stream: package 52 durable_event_log_and_stream. Ingestion writes no hub event, and telemetry is never part of the event log.
- Running exec, attach and file transfers in a sandbox and deciding when one has ended: package 46 sandbox_exec_files_and_logs. It reports each ended operation to the operation observers registered in `internal/sandbox`; the recorder of this package is registered there as one of them (Design Decision 13).
- The error envelope, check order, body size limit, content type, `Idempotency-Key`, rate limit: package 02 http_api_conventions. The scope check: package 06 api_keys_and_access_tokens. The access rule: package 07 workspace_access_rule. Which workspace routes a bound token reaches: package 08 workspace_bound_tokens.
- A CLI command: the source PRD defines none for ingestion.
- Removing telemetry when a workspace is deleted: telemetry of a deleted workspace stays until the `orphans` retention step of package 51 removes it, so this package registers no remover with package 14.

## Background

The repository holds no code yet; every file named here is new. The source is `docs/prd/14_sessions_audit_events.md`: §14.3.1 (identifiers and time), §14.3.4 (run telemetry records), AUD-FR-020 to AUD-FR-026, the ingestion routes of §14.5, the key `[audit].write_timeout` of §14.9 and the ingestion entries of §14.10.

The code lives in `internal/telemetry/`. It builds on what earlier packages deliver: the router, error envelope and creating-`POST` helper of `internal/server` (package 02), the configuration key registry and the analytical store with its schema steps (package 03), the scope check (package 06), the access component (package 07) and the session records of `internal/session` (package 49). `parleyd`'s composition in `internal/hub/` wires them together.

## Requirements

### Access and preconditions (AUD-FR-020, AUD-FR-021)

The eight ingestion routes are:

| Route (under `/api/v1/workspaces/:slug/runs/:run_id`) | Stores |
|---|---|
| `POST /events` | one run event |
| `POST /events/batch` | up to 1000 run events |
| `POST /sessions/outcomes` | one session outcome |
| `POST /tools/calls` | one tool call |
| `POST /tools/errors` | one tool error |
| `POST /traces` | one trace record |
| `POST /traces/batch` | up to 1000 trace records |
| `POST /postmortem` | the postmortem of the run |

Every route needs the scope `audit:write` from a PAT caller and write access to the workspace, and follows the hub's order of checks:

- A PAT without `audit:write` gets `403 insufficient_scope`, whether or not the workspace exists.
- A workspace that does not exist, or one the caller has no access to, answers `404 not_found` with the message `workspace not found`, for every caller, the admin token included. The two answers are byte-identical.
- A member of the workspace's organization who is not its owner gets `403 forbidden`, also when the workspace is archived.
- The `run_id` in the path must match `YYYYMMDD_HHMMSS_xxxxxx`: eight digits, an underscore, six digits, an underscore and six lower-case hexadecimal digits. Any other value answers `400 invalid_request` with a message that names `run_id`. A `run_id` member in a body (or in a batch item), when present, must equal the one in the path; otherwise the answer is the same `400` (for a batch item: an entry in `errors`).
- An archived workspace answers `409 workspace_archived` to a caller with write access. A workspace in any other status (`pending`, `cloning`, `ready`, `failed`) accepts ingestion, and so does a task fork.

A refused request stores nothing.

### Record fields and validation (§14.3.1, §14.3.4, AUD-FR-022)

Every stored record carries `id`, `workspace` (the slug of the path), `run_id`, `timestamp` and `ingested_at`, and every member of its kind is present in every response; an optional member that was not sent is `null`.

- `id`: a UUID in the 8-4-4-4-12 hexadecimal form, any letter case, stored and returned in lower case. When the client sends none the hub generates a version 4 UUID.
- `timestamp`: RFC 3339 with any offset. It is normalised to UTC and keeps fractional seconds up to microseconds; further digits are cut off. When absent it is the hub's time at ingestion.
- `ingested_at`: the hub's time, RFC 3339 UTC with whole seconds.
- `session_id`, where a kind has it: a UUID, lower-cased; it need not name an existing session.
- `node_id` and `tool_name`: strings of at most 255 characters.
- `duration_ms` and `sequence`: integers of at least 0.
- JSON-valued members (`payload`, `token_usage`, `input`, `output`, `data`, and the postmortem's four) are stored and returned as JSON, never as strings holding JSON.

| Kind | Members beyond the common ones |
|---|---|
| Run event | `event_type` (required, free-form), `severity` (`info`, `warning`, `error`, `critical`), `node_id`, `session_id`, `payload` (JSON object) |
| Session outcome | `session_id` (required), `status` (required, free-form), `node_id`, `duration_ms`, `token_usage` (any JSON) |
| Tool call | `tool_name` (required), `node_id`, `session_id`, `duration_ms`, `input`, `output` (any JSON) |
| Tool error | `tool_name` (required), `error_msg` (required), `error_code`, `node_id`, `session_id` |
| Trace | `event_type` (required; one of `session.init`, `assistant.message`, `tool.use`, `tool.error`, `session.result`), `node_id`, `session_id`, `sequence`, `data` (any JSON) |

A run event sent without `severity` gets `error` for the type `session.fail`; `warning` for `run.limit_reached`, `git.conflict`, `harvest.empty` and `review.parse_failure`; `info` for every other type.

A JSON `null` counts as absent. Members the kind does not define are ignored. A violation — a missing or empty required member, a value outside an enum, a malformed UUID or timestamp, a negative or fractional integer, a label over 255 characters, a member of the wrong JSON type (a `payload` that is not an object, a number where a string is expected) — answers `400 invalid_request` with a message that names the member, and stores nothing.

### Idempotent records (AUD-FR-023, §14.10)

A new record answers `201` with the stored record. A request whose `id` already names a record of the same kind in the same workspace and run is a replay: it answers `200` with the **stored** record, also when the replayed body differs, and changes nothing. An `id` that names a record of the same kind in another workspace or another run answers `409 conflict`. Ids are compared without regard to letter case. When several requests with the same new `id` arrive at once, exactly one stores the record and answers `201`; the others answer `200` with that record.

### Postmortem (§14.3.4, `POST …/postmortem`)

A run has at most one postmortem per workspace. The body holds `run_status` (required; `stalled`, `block_limit`, `cost_limit` or `session_limit`), `started_at` and `completed_at` (required, RFC 3339), `task_summary` (required, a JSON object with the key `total`), `cost_summary` (required, a JSON object), `schema_version` (optional; only `1`), and `blocked_tasks` and `session_history` (JSON arrays, default `[]`).

The first postmortem of a run answers `201` with the stored record: the members above, with `schema_version` `1`, plus `id` (generated by the hub), `workspace`, `run_id`, `timestamp` and `ingested_at` (both the hub's time). When the run already has a postmortem in this workspace, the answer is `200` with the stored one and the new body changes nothing. The same run id in another workspace has its own postmortem. A `schema_version` other than `1` answers `400` with `error_type` `unknown_schema_version`. Every other violation answers `400 invalid_request` naming the member.

### Batches (AUD-FR-024, §14.10)

`POST …/events/batch` and `POST …/traces/batch` take a JSON array of 1 to 1000 items. A body that is not an array, or an empty array, answers `400 invalid_request`. More than 1000 items answer `413 payload_too_large`. In both cases nothing is stored.

Otherwise the answer is always `200`:

```json
{"accepted": 2, "duplicates": 1, "errors": [{"index": 3, "id": null, "message": "event_type is required"}]}
```

- Each item is validated like a single record. An invalid item is skipped and reported in `errors` with its zero-based `index`, its `id` (lower-cased when the item carries a valid UUID, else `null`) and a `message` that names the member. An item that is not a JSON object is invalid.
- An item whose `id` exists in another workspace or run is reported in `errors`.
- An item whose `id` already exists in this workspace and run, and a later item that repeats the `id` of an earlier item of the same batch, count in `duplicates` and change nothing.
- Every other item is stored and counted in `accepted`. The accepted items are stored together or not at all.
- `errors` is `[]` when there are none. A batch of only invalid items answers `200` with `accepted: 0`, `duplicates: 0` and one error per item.

### Write timeout (AUD-FR-025, §14.9)

One ingestion write — a single record or the whole of a batch — that does not finish within `[audit].write_timeout` answers `503 unavailable` with the header `Retry-After: 5`, and stores nothing. A retry with the same ids is safe: it stores the records as new.

`[audit].write_timeout` is a duration (`ms`, `s`, `m`, `h`, `d`), default `2s`, at least `100ms`. A malformed value or one below `100ms` stops the hub at start with exit status 1 and a message that names the key.

### Sandbox use (AUD-FR-026)

Exec, attach and file transfers in a sandbox write no hub event. When such an operation ends, the sandbox component tells the recorder of this package: the sandbox's workspace and id, the sandbox's session id (or none), who did it (`actor_type`, `actor_id`), the time the operation started, and the facts of the operation. When the sandbox has a session and that session has a `run_id`, the hub stores one run event of the sandbox's workspace under that `run_id` and `session_id`:

| Operation | `event_type` | `payload` members |
|---|---|---|
| exec | `sandbox.exec` | `sandbox_id`, `actor_type`, `actor_id`, `command` (argument list), `exit_code`, `timed_out`, `duration_ms` |
| attach | `sandbox.attach` | `sandbox_id`, `actor_type`, `actor_id`, `duration_ms` |
| upload | `sandbox.upload` | `sandbox_id`, `actor_type`, `actor_id`, `path`, `bytes` |
| download | `sandbox.download` | `sandbox_id`, `actor_type`, `actor_id`, `path`, `bytes` |

The record's `timestamp` is the time the operation started, its `severity` is `info`, its `id` is generated. The payload holds exactly the members of the table: never stdin, command output, terminal content, file content or environment values. A sandbox without a session, with a session that has no `run_id`, or with a session id that names no session leaves no record. A failure to write the record — a store error or the write timeout — is logged and does not fail the operation: the recorder reports nothing back.

### Run event notification

Each run event that is newly stored — by the single route, by a batch, or by the sandbox recorder — is reported once to the registered observers with its `event_type`, after it is stored. A replay, a duplicate, an invalid item and a write that failed are not reported. The registration is `telemetry.OnRunEvent(observer)`; package 51 registers an observer there that reports `parley_audit_events_total{source="agent"}` to its recorder.

### End-to-end flows

1. An agent posts a run event with an `id` under a run of its workspace; `parleyd` checks scope and access, validates, stores the record in the analytical store and answers `201`. The agent posts a batch of trace records that also contains one invalid item; `parleyd` answers `200` with the counts and the error. The agent sends both again; `parleyd` answers `200` with the stored run event and reports every valid trace as a duplicate.
2. An agent opens a session with a `run_id` on a task fork. The sandbox component of `parleyd` reports an exec that ended in a sandbox of that session; `parleyd` looks the session up and stores one `sandbox.exec` run event under the session's run, with the command and exit code and without any output.

## Design Decisions

1. **Profile: full.** Two end-to-end flows cross the router, the access component, the session component and the analytical store.
2. **Project decisions relied on.** Go; telemetry is stored in the analytical store (SQLite through `modernc.org/sqlite`, the second database file) behind `internal/store`, the workspace check reads the transactional store; HTTP uses `net/http`; tests use `testing`, table-driven, with `net/http/httptest`; code takes a replaceable clock.
3. **An `id` is unique per record kind.** AUD-FR-023 speaks of an `id` in "another workspace or run" and says nothing of an `id` reused for another kind of record. Each kind has its own table keyed by `id`; the same UUID used for a run event and a tool call names two records. Why: the simplest reading, and no route can then answer for a record of another kind.
4. **A replay is validated before it is recognised.** The check order puts validation before the act, so a replay with an invalid body answers `400`. This holds for the postmortem too, although its route says the new body "is ignored": ignored means it changes nothing.
5. **Required strings must be non-empty.** `event_type`, `status`, `tool_name`, `error_msg` and `run_status` sent as `""` are violations. The PRD says "required" only.
6. **No length limit on `event_type`, `status`, `error_msg` and `error_code`.** The PRD limits only `node_id` and `tool_name` (among the members this package stores); the request body limit bounds the rest.
7. **`task_summary.total`.** The key must be present with a value other than `null`; its type is not checked. The PRD says only "JSON object with key `total`".
8. **The postmortem's `id` and `timestamp` come from the hub.** The route's body lists neither; an `id` or `timestamp` member in the body is ignored like any unknown member. `started_at` and `completed_at` are both required and are normalised like `timestamp`; their order is not checked.
9. **Timestamps in responses.** Client timestamps are returned in UTC with `Z`, with the fractional part cut to microseconds and without trailing zeros (no fractional part when it is zero).
10. **Statuses other than `archived` accept ingestion**, and so do task forks. Ingestion needs no repository, and the PRD refuses only the archived workspace.
11. **Batch details the PRD leaves open.** A repeated `id` inside one batch: the first item is accepted, later ones are duplicates. An item that is not an object is an invalid item with `id` `null`. `errors[].id` is the lower-cased `id` only when the item carries a valid UUID. A store failure other than the timeout answers `500 internal` with nothing stored.
12. **`Idempotency-Key`.** The six single-record routes are registered as creating `POST`s with package 02's helper. The two batch routes do not take the header: they always answer `200` and their items are idempotent on `id`.
13. **The sandbox recorder checks neither access nor workspace status.** It is the hub's own write. It is bounded by `[audit].write_timeout` like any ingestion write. It reads the session's `run_id` through a narrow lookup that package 49's session component implements. **How package 46 reaches it:** package 46 owns an operation observer registry in `internal/sandbox` (a register function and a record with kind, sandbox id, workspace, session id, actor, start time, `duration_ms` and, per kind, `command`, `exit_code`, `timed_out`, `result`, `path` and `bytes`). `internal/hub` registers an adapter there that turns such a record into one of the recorder's four operation values and calls the recorder's `Record`; `result` is not stored. This package does not depend on package 46: the registration is made when `internal/sandbox` has that registry, and by package 46's composition when it is implemented later.
14. **Run event observers.** The metrics catalogue is package 51's, which depends on this package. This package therefore offers the observer registration `telemetry.OnRunEvent`, where an observer is a function taking the `event_type` of one stored run event; package 51 registers an observer that reports to its recorder (`Add` on `parley_audit_events_total` with `source` `agent` and that `type`). This package imports nothing of `internal/metrics`. Observers are told about newly stored run events only, so replays are not counted (as for usage records, AUD-FR-006). Run events written by the sandbox recorder are reported too.
15. **`[audit].write_timeout` is registered by this package** with the configuration key registry of package 03; the other `[audit]` keys belong to package 51.
16. **No remover on workspace deletion** (see Non-goals): §14.7.2 keeps records of deleted workspaces for `[audit].orphan_retention`.

## Dependencies

- Package 49 agent_sessions_and_usage (draft): the session records; the sandbox recorder reads a session's `run_id` from them, and the end-to-end test opens a session through its route.
- Through it, and not listed as dependencies of their own: package 07 workspace_access_rule (the write-access decision and the workspace record with its status), package 06 api_keys_and_access_tokens (the scope `audit:write`), package 02 http_api_conventions (router, error envelope, creating-`POST` helper) and package 03 server_lifecycle_and_configuration (analytical store, schema steps, configuration key registry).
- Used by, not required by: package 46 sandbox_exec_files_and_logs reports to the sandbox recorder through its operation observer registry; package 51 audit_query_and_retention reads the stored records, registers the run event observer with `telemetry.OnRunEvent`, and adds its retention operations beside this package's store in `internal/telemetry/retention.go`.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires: after 7 days by default, or 24 hours after all its changes have landed or been abandoned.
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
- **Sandbox**: An isolated environment in which an agent runs, attached to a workspace (normally a task fork) and started with it checked out. It runs on OpenShell; the hub controls it.
- **Session**: An agent session: who ran, on which workspace, with what token usage, and which changes it produced.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
- **Agent telemetry**: Run events, tool calls, traces, outcomes and postmortems that agents send to the hub.
- **Run id**: The identifier of one agent run, in the form `YYYYMMDD_HHMMSS_xxxxxx` with six lower-case hexadecimal digits at the end.
- **Run event**: A telemetry record with a free-form `event_type`, a severity and a JSON object `payload`.
- **Postmortem**: The one record per workspace and run that says how the run ended and why.
