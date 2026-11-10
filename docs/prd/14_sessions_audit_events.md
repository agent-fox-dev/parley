[Parley PRD](00_overview.md) · §14

## 14. Sessions, audit, events and metrics

### 14.1 Purpose and scope

This section covers how Parley records what happened, why, and by whom.

- **Agent sessions.** An agent opens a session for a workspace, reports token usage while it works, and closes it with an outcome. Forks, pushes, API commits, change versions, claims and lands can name the session, so the session record is the thread that ties one piece of agent work together.
- **Run telemetry.** Agents push records about their runs: run events, session outcomes, tool calls, tool errors, conversation traces and a postmortem. The hub stores them and serves them back per run, including a reconstructed transcript.
- **Hub events.** Every significant state change in the hub produces one event. Events are durable: each gets a hub-wide sequence number, is kept in an event log, and can be consumed live or replayed after a disconnect without loss.
- **Event webhooks.** A workspace owner subscribes a URL. The hub sends each event of the workspace to it as a signed HTTP `POST`, in order, and retries until the receiver accepts it. The stream and the log stay the way to pull events; event webhooks push the same events to consumers that cannot hold a connection or poll.
- **Audit query.** One query spans hub events and agent run events.
- **Retention.** A background task keeps all of the above bounded.
- **Metrics.** A Prometheus endpoint exposes operational metrics.

Out of scope: pricing. "Cost" in this section always means token counts. There is no price table and no model catalogue. The webhooks the hub receives from a forge are §12; this section calls the ones it sends **event webhooks** throughout.

### 14.2 User stories

**AUD-US-01: Agent reports a session.** As an agent, I want to open a session for a workspace, report token usage increments and close it with an outcome, so that the operator can attribute token spend.
- `POST /sessions` creates an `active` session (201). Repeating the same `id` returns the stored session (200).
- `POST /sessions/:id/usage` adds one usage record while the session is `active` (201). A replay answers 200 and changes nothing.
- `POST /sessions/:id/complete` ends the session once. Repeating it answers 200 with the unchanged record.

**AUD-US-02: Session as the thread.** As a developer, I want to open one session record and see the forks it created, the changes it produced and how many commits, claims and lands it made, so that I can follow one piece of agent work from start to finish.

**AUD-US-03: Agent pushes run telemetry.** As an agent, I want to push run events, outcomes, tool calls, tool errors, traces and a postmortem under a run id, so that the run can be audited later.
- Each record is idempotent on its `id`.
- Batches of up to 1000 items are accepted item by item with a per-item error list.

**AUD-US-04: Developer inspects a run.** As a developer with read access, I want to list a run's records with filters and cursors, read its postmortem and a transcript per node, so that I can debug what an agent did.

**AUD-US-05: Operator audits the parley.** As an operator, I want one query over hub and agent events across all workspaces, newest first, so that I can reconstruct who did what.

**AUD-US-06: Durable live feed.** As an integrator, I want to subscribe to hub events, and after a disconnect or a restart resume from the last event I saw, so that my automation never misses an event and never has to rescan state.

**AUD-US-07: Polling consumer.** As an integrator that cannot hold a stream open, I want to page through the same event log as JSON with a long-poll, so that a simple loop gets the same guarantees.

**AUD-US-08: Cost report.** As an org member, I want token usage of a workspace summed per day, session or model over a window.

**AUD-US-09: Retention.** As an operator, I want old data deleted on a schedule, stale active sessions timed out, and data of deleted workspaces removed after a grace period.

**AUD-US-10: Monitoring.** As an operator, I want `GET /metrics` in Prometheus text format, so that I can alert on the hub.

**AUD-US-11: Event webhook.** As a workspace owner, I want the hub to send the events of my workspace and its forks to a URL of mine, so that a CI system or a chat bot reacts without holding a stream open or polling.
- Each delivery is signed with a secret that the hub shows me once, so the receiver can tell it came from the hub.
- Deliveries arrive in `seq` order. A failed delivery is retried, and later events wait behind it.
- I can send a test delivery, see recent deliveries with their outcome, and redeliver one.

**AUD-US-12: Safe destinations.** As an operator, I want event webhooks refused for loopback, private and metadata addresses unless I allow them, so that a subscription cannot be used to reach the hub's own network.

### 14.3 Concepts and data model

#### 14.3.1 Identifiers and time

- Session ids, usage ids, telemetry record ids and hub event ids are UUIDs. A client-supplied id MUST be a UUID in the 8-4-4-4-12 hexadecimal form. Any letter case is accepted; ids are stored and returned in lower case. When the client sends none, the hub generates a version 4 UUID.
- A **run id** identifies one agent run. Its format is `YYYYMMDD_HHMMSS_xxxxxx`, where `xxxxxx` is six lower-case hexadecimal digits.
- `node_id`, `archetype`, `model` and `tool_name` are free-form labels of at most 255 characters.
- Timestamps generated by the hub follow §3 (RFC 3339 UTC, whole seconds). Timestamps supplied by clients in telemetry MUST be RFC 3339 with any offset. They are normalised to UTC and keep fractional seconds up to microseconds, because they order records inside a run.
- JSON-valued fields are stored and returned as JSON, never as strings holding JSON.

#### 14.3.2 Session

| Field | Type | Meaning |
|---|---|---|
| `id` | UUID | Client-chosen or generated |
| `workspace_slug` | string | The workspace the session works in. May be a task fork |
| `run_id`, `node_id`, `archetype`, `model` | string or null | Labels set at creation |
| `status` | enum | `active`, `completed`, `failed`, `timeout`, `terminated` |
| `user_id` | string or null | The user who created the session. Null for an admin token |
| `credential_type` | enum | `admin_token`, `api_key`, `pat` |
| `started_at` | timestamp | Set at creation |
| `ended_at` | timestamp or null | Set on any terminal transition |
| `duration_ms` | integer or null | From `complete`. When the client sends none, the hub computes it from `started_at` and `ended_at` |
| `error_message` | string or null | From `complete`, or the reason of a forced close |
| `cache_creation_input_tokens` | integer | From `complete`. Default 0 |
| `metadata` | any JSON or null | As sent at creation |
| `token_summary` | object | `total_input_tokens`, `total_output_tokens`, `total_cache_read_tokens`, `models_used` (distinct model names, sorted ascending) |
| `forks` | list | Slugs of the task forks created with this session (§4), oldest first |
| `changes` | list | `{workspace, number}` of each change that has at least one version attributed to this session (§7), ordered by first attribution |
| `sandboxes` | list | `{workspace, id}` of each sandbox that belongs to this session (§13), oldest first. Deleted sandboxes stay listed |
| `counts` | object | `forks`, `changes`, `sandboxes`, `versions`, `pushes`, `commits`, `claims`, `lands` |

Every field is present in every session response. `forks`, `changes` and `sandboxes` hold at most 500 entries each; `counts` always holds the true totals. `counts.pushes` counts ref updates by git push, `counts.commits` counts commits created through the source control API (§6), `counts.versions` counts change versions, `counts.lands` counts successful lands.

**State machine.**

```
            POST /sessions            POST /sessions/:id/complete
  (none) ─────────────────▶ active ─────────────────────────────▶ completed | failed | timeout
                              │  workspace archived or deleted, fork expired ─▶ terminated
                              └─ active longer than [sessions].max_active_age ─▶ timeout
```

The four terminal states are absorbing. `terminated` cannot be set by a client. Usage may be reported only while the session is `active`.

**Linking work to a session.** A fork request, a git push (`-o parley.session=<id>`), an API commit, a change version, a claim, a land and a sandbox may name a session. The reference is accepted when:

- the session exists,
- it was created by the caller's user, or the caller is an admin token, and
- the session's workspace and the workspace of the operation are the same workspace, or one is a task fork of the other.

The session's status is not checked: work may be attributed after the session has ended, for example a land that happens later. A reference that fails these rules is refused with `400`, `error_type: unknown_session`, and nothing is changed. On a git push the refusal is reported in git's text form (§5).

Every hub event caused by an operation that names a session carries that `session_id`. An event about a change that is caused by the hub itself (restack, conflict detection, automatic land) carries the session of the change version it concerns. An event about a task fork (`parley.workspace.fork`, `parley.workspace.delete`, `parley.fork.expire`) carries the session the fork was created with (§4). A sandbox event carries the session of its sandbox (§13). A rebuild event carries the session of the push or land that queued the rebuild (§10).

#### 14.3.3 Token usage record

`id` (UUID), `session_id`, `workspace_slug` (copied from the session), `model` (required), `input_tokens`, `output_tokens`, `cache_read_tokens` (integers ≥ 0, default 0), `reported_at` (hub time).

#### 14.3.4 Run telemetry records

All telemetry records carry `id`, `workspace`, `run_id`, `timestamp` (client time, or hub time when absent) and `ingested_at` (hub time). `session_id`, where present, MUST be a UUID; it is not required to name an existing session.

| Record | Fields beyond the common ones |
|---|---|
| Run event | `event_type` (required, free-form), `severity` (`info`, `warning`, `error`, `critical`), `node_id`, `session_id`, `payload` (JSON object) |
| Session outcome | `session_id` (required), `status` (required, free-form), `node_id`, `duration_ms` (integer ≥ 0), `token_usage` (any JSON) |
| Tool call | `tool_name` (required), `node_id`, `session_id`, `duration_ms` (integer ≥ 0), `input`, `output` (any JSON) |
| Tool error | `tool_name` (required), `error_msg` (required), `error_code`, `node_id`, `session_id` |
| Trace | `event_type` (required; one of `session.init`, `assistant.message`, `tool.use`, `tool.error`, `session.result`), `node_id`, `session_id`, `sequence` (integer ≥ 0), `data` (any JSON) |
| Postmortem | One per workspace and run. `schema_version` (only `1`), `run_status` (`stalled`, `block_limit`, `cost_limit`, `session_limit`), `started_at`, `completed_at`, `task_summary` (JSON object with key `total`), `cost_summary` (JSON object), `blocked_tasks`, `session_history` (JSON arrays, default `[]`) |

Default severity of a run event when none is sent: `session.fail` is `error`; `run.limit_reached`, `git.conflict`, `harvest.empty` and `review.parse_failure` are `warning`; every other type is `info`.

#### 14.3.5 Hub event

A hub event reports one state change in the hub. Every event has the same shape:

| Field | Type | Meaning |
|---|---|---|
| `seq` | integer | Hub-wide sequence number. Strictly increasing, never reused, never reset |
| `id` | UUID | Stable identity of the event. Use it to drop duplicates |
| `type` | string | For example `parley.change.land`. Every type starts with `parley.` |
| `time` | timestamp | When the event was written |
| `workspace` | string or null | Slug of the workspace the event belongs to. Null for an event that is not workspace-scoped |
| `actor_type` | enum | `admin_token`, `api_key`, `pat`, `forge`, `system` |
| `actor_id` | string or null | User id of the actor. Null for `admin_token`, `forge` and `system` |
| `resource_type` | string | Kind of the resource the event is about. Never empty |
| `resource_id` | string | Identity of that resource inside its workspace. Never empty |
| `action` | string | The last dot-separated segment of `type` (`parley.change.land` has action `land`). Never empty |
| `session_id` | UUID or null | The session the change is attributed to (§14.3.2) |
| `metadata` | object | Type-specific keys (§14.8). `{}` when none |

Rules that hold for every event:

- Metadata never holds a credential, a secret value, a token, or free text taken from a forge message. URLs carry no userinfo.
- A list of paths in metadata holds at most 100 entries. When it was cut, the metadata also has `truncated: true`.
- Unless §14.8 says otherwise, the actor is the caller of the request that caused the event. Work done by the hub on its own (expiry, restack, automatic land, retention) has actor `system`.

#### 14.3.6 Event log, audit log and delivery guarantees

Hub events are stored once and served through two windows:

- The **event log** is the part addressable by `seq`: events newer than `[events].retention` (default `30d`). `GET /events` and `GET /events/log` read it.
- The **audit log** is every hub event newer than `[audit].max_age` (default `90d`). `GET /audit` reads it.

Event webhooks (§14.3.9) are a third way out of the same event log: the hub reads the log for each subscription and sends what it finds.

Guarantees:

- **Durable.** An event is written in the same transaction as the state change it reports, wherever that state lives in the hub's own store. Either both are committed or neither is. For a state change that lives in a git repository (a ref update), the hub MUST still write the event: when the hub stops between the ref update and the event, the missing event is written at the next start, before the hub serves requests.
- **Ordered.** Events are delivered in `seq` order. An event never becomes visible after an event with a higher `seq` has been delivered to any consumer.
- **At-least-once.** A consumer that resumes from the last `seq` it processed receives every visible event after it. It may receive an event twice across a reconnect; `seq` and `id` identify duplicates. Within one connection or one page no event repeats.
- **Gaps are normal.** A consumer sees only the events it may see and asked for, so the `seq` values it receives are not contiguous. Consumers MUST NOT treat a gap as loss.

#### 14.3.7 Visibility

One rule decides who may see a hub event, on the stream, the log and the audit query. An event webhook is treated as an owner of its workspace under this rule (§14.3.9).

| Event | Visible to |
|---|---|
| `workspace` is set | Callers with read access to that workspace. For a task fork, also callers with read access to its parent; this still holds after the fork is deleted. The next row is the exception |
| `workspace` is set and the type is `parley.secret.*`, `parley.token.*` or `parley.webhook.*` | Only callers with level `owner` or `admin` on that workspace (for a task fork: on the fork or on its parent). Secret names, bound tokens and event webhooks are private to the owner (§3, §11, §14.3.9), so other readers of the workspace do not see these events |
| `workspace` is null, `metadata.scope` is `user` | The user named in `metadata.scope_id` |
| `workspace` is null, `metadata.scope` is `org` | Members of the org named in `metadata.scope_id` |
| Any event | Admin tokens |

A token bound to a workspace (§3) sees only events of that workspace and of the forks the token is valid on (§4). A token bound to a task fork also sees the events of the fork's parent whose resource is a change with that fork as its source (§3, §7), so an agent can follow its own changes. A bound token never sees an event without a workspace.

Access is evaluated when an event is delivered, not when the stream was opened. A workspace or fork created later is included from its first event; a caller who loses access stops receiving events of that workspace.

Run telemetry is never part of the event log. It is visible to callers with read access to its workspace.

#### 14.3.8 Scopes

`sessions:read`, `sessions:write`, `audit:read`, `audit:write`. The rules of §3 apply: a read check accepts either scope of the pair, and only PATs are scope-checked. `GET /events`, `GET /events/log` and `GET /audit` need `audit:read`.

Bound tokens may call the routes of this section that are not under `/workspaces/:slug` (`/sessions…`, `/audit`, `/events`, `/events/log`). Their results are confined to the workspaces the binding covers (§3): events as §14.3.7 describes, and sessions only when the session's workspace is covered. For a bound token, `POST /sessions` with a `workspace_slug` outside the binding and a `workspace` filter outside it answer `404 workspace not found`; every `/sessions/:id` route for a session of an uncovered workspace answers `404 session not found`, also when the token's user created that session.

The event webhook routes (§14.5) use `workspaces:read` and `workspaces:write`, because a subscription is part of a workspace's configuration. They need level `owner` or `admin` on the workspace and refuse every workspace-bound token.

#### 14.3.9 Event webhooks

An **event webhook** is a subscription of a workspace. The hub sends every event the subscription matches to its URL, one HTTP `POST` per event. It is the hub's only outbound delivery of events. The forge's webhooks, which the hub receives, are a different thing (§12).

**Subscription.**

| Field | Type | Default | Meaning and constraints |
|---|---|---|---|
| `id` | UUID | generated | Identity of the subscription |
| `workspace` | string | n/a | Slug of the workspace the subscription belongs to. Never a task fork |
| `url` | string | required | Where deliveries go. An absolute `https://` URL with a host, without userinfo and without a fragment, at most 2,048 characters. `http://` is accepted only when `[webhooks].allow_insecure` is true and the host is a loopback address |
| `types` | list of string | `["*"]` | Type filter with the syntax of AUD-FR-042: at most 20 entries, each an exact type or a prefix ending in `*` |
| `include_forks` | bool | `true` | Whether events of the workspace's task forks are sent too |
| `description` | string | `""` | Free text, at most 1,024 characters |
| `state` | enum | `active` | `active`, `failing` or `disabled` (below) |
| `created_by` | string or null | caller | User id of the creator. Null for the admin token |
| `created_at`, `updated_at` | timestamp | now | |
| `last_delivery_at` | timestamp or null | null | Time of the last delivery attempt, successful or not |
| `last_status` | string or null | null | Outcome of that attempt: `succeeded` or `failed` |
| `failing_since` | timestamp or null | null | Time of the first failed attempt since the last successful one. Null while the last attempt succeeded |

The **signing secret** is not a field of the subscription. The hub generates it: 32 random bytes, shown as `whsec_` followed by 64 lower-case hexadecimal characters. It is returned once, in the response that creates the subscription and in the response that rotates it. It is stored encrypted with the key that encrypts secrets (§11), and no route returns it afterwards.

**What a subscription receives.** Exactly the events that an owner of the workspace could see for it under §14.3.7, and that match `types`:

- every event whose `workspace` is the subscription's workspace, including the owner-only types (`parley.secret.*`, `parley.token.*`, `parley.webhook.*`);
- with `include_forks`, every event whose `workspace` is a task fork of that workspace, also after the fork is gone;
- never an event without a workspace, and never an event of another workspace.

A subscription starts with the first event written after it was created. It never sends older events.

**States.**

| State | Meaning | Deliveries |
|---|---|---|
| `active` | The last attempt succeeded, or nothing was sent yet, or the current event is still inside its 24 hours of retries | sent |
| `failing` | At least one event was given up after 24 hours of failed attempts, and no attempt has succeeded since | sent; the next successful attempt returns the subscription to `active` |
| `disabled` | Switched off by the owner (`active: false`), or by the hub after 72 hours of continuous failure | none; events written meanwhile are not kept for it |

**Delivery.** One record per event sent to one subscription. It is what the deliveries route lists.

| Field | Type | Meaning |
|---|---|---|
| `id` | UUID | Delivery id. The same for every attempt of this delivery, and sent as `Parley-Delivery` |
| `seq` | integer | `seq` of the event. `0` for a ping |
| `type` | string | Type of the event |
| `status` | enum | `pending` (waiting for its first or next attempt), `succeeded`, `failed` (given up) |
| `attempts` | integer | Attempts made so far |
| `response_code` | integer or null | HTTP status of the last attempt. Null when no response arrived |
| `error` | string or null | Why the last attempt failed without a usable response: `timeout`, `connection_failed`, `tls_error`, `destination_refused`, `redirect`. Null otherwise |
| `duration_ms` | integer or null | Duration of the last attempt |
| `created_at`, `last_attempt_at` | timestamp | |
| `next_attempt_at` | timestamp or null | When the next attempt is due. Null unless `pending` |

A delivery record holds no payload and no response body. Records are kept for 7 days after their last attempt.

### 14.4 Functional requirements

**Sessions**

- AUD-FR-001: `POST /sessions` needs `sessions:write` and write access to the workspace named by `workspace_slug`. The workspace may be a task fork. An archived workspace refuses new sessions with `409 workspace_archived`.
- AUD-FR-002: A new session has `status: active`, `started_at` = now, and records the caller's user id and credential type.
- AUD-FR-003: Session creation is idempotent on `id`. A replay by the same user for the same workspace answers 200 with the stored session and changes nothing. An `id` that exists for another user or workspace answers `409 conflict`.
- AUD-FR-004: Only the user who created a session, or an admin token, may complete it or report usage. Any credential of that user qualifies, within the confinement of a bound token (§14.3.8). A caller who can read the session but did not create it gets `403 forbidden`.
- AUD-FR-005: `complete` accepts `completed`, `failed` or `timeout`. Completing an `active` session sets the status, `ended_at`, `error_message`, `duration_ms` and `cache_creation_input_tokens` in one atomic step. Completing a terminal session answers 200 with the unchanged record; when two completes race, exactly one takes effect and both answer 200.
- AUD-FR-006: A usage report needs a non-blank `model` and token counts ≥ 0, and an `active` session (else `409 session_not_active`). A new record answers 201. A replay of an existing usage `id` in the same session answers 200 with the stored record and changes nothing: no record, no total, no metric. A usage `id` that exists in another session answers `409 conflict`.
- AUD-FR-007: A session is readable by its creator, by any caller with read access to its workspace, and by admin tokens. A session on a task fork is also readable by callers with read access to the parent, also after the fork is gone. Workspace status does not affect reads. A caller who may not read a session gets `404 session not found`.
- AUD-FR-008: Every session response carries `token_summary`, `forks`, `changes`, `sandboxes` and `counts`, computed at read time.
- AUD-FR-009: Archiving or deleting a workspace, and the expiry of a task fork, closes all its `active` sessions: `status: terminated`, `ended_at` = the time of the operation, `error_message` = `workspace archived`, `workspace deleted` or `fork expired`. One `parley.session.force_closed` event is written per session, in the same transaction as the workspace change.
- AUD-FR-010: The cost report sums the usage records of one workspace with `since ≤ reported_at < until`. Totals and each group give the three token sums and the number of distinct sessions. `day` groups are UTC calendar dates. No prices are applied.

**Ingestion**

- AUD-FR-020: Every ingestion route needs `audit:write` and write access to the workspace, and follows the check order of §3. Ingestion into a workspace that does not exist is refused with `404 workspace not found` for every caller, admin tokens included. An archived workspace refuses ingestion with `409 workspace_archived`.
- AUD-FR-021: The `run_id` in the path MUST match the run id format, on every per-run route including the transcript. A `run_id` in a body, when present, MUST equal the one in the path.
- AUD-FR-022: Every field is validated as §14.3.4 documents: required fields, enums, UUIDs, RFC 3339 timestamps, non-negative integers, and the JSON type of `payload`, `task_summary`, `cost_summary`, `blocked_tasks` and `session_history`. A JSON `null` counts as absent. A violation answers `400 invalid_request` with a message that names the field.
- AUD-FR-023: Each record is idempotent on its `id` (a postmortem: on workspace and run). A new record answers 201. A replay answers 200 with the **stored** record and changes nothing. An `id` that already exists in another workspace or run answers `409 conflict`.
- AUD-FR-024: Batch routes take a JSON array of 1 to 1000 items. Each item is validated like a single record. Invalid items are reported in `errors` and skipped. Valid items are stored together or not at all. The response is always 200 with `{accepted, duplicates, errors}`.
- AUD-FR-025: An ingestion write that does not finish within `[audit].write_timeout` answers `503 unavailable` with `Retry-After: 5`, and stores nothing.
- AUD-FR-026 (sandbox use). Exec, attach and file transfers in a sandbox (§13) write no hub event. When the sandbox has a session and that session has a `run_id`, the hub itself records each of them as a run event of the sandbox's workspace under that `run_id` and `session_id`, with `event_type` `sandbox.exec`, `sandbox.attach`, `sandbox.upload` or `sandbox.download`. The `payload` holds `sandbox_id`, who did it (`actor_type`, `actor_id`), and: for an exec, the `command` (argument list), `exit_code`, `timed_out` and `duration_ms`; for an attach, `duration_ms`; for a transfer, `path` and `bytes`. The record's `timestamp` is the time the operation started. It never holds stdin, command output, terminal content, file content or environment values. A sandbox without such a session leaves no record of these operations. The record is written when the operation ends, and a failure to write it does not fail the operation.

**Reads**

- AUD-FR-030: Per-run reads need `audit:read` and read access to the workspace, whatever its status.
- AUD-FR-031: Per-run lists are ordered by (`timestamp`, `id`), ascending by default; `order=desc` reverses. `since` is inclusive and `until` exclusive. Results are always confined to the workspace and run of the path.
- AUD-FR-032: The audit query merges hub events and run events, newest first (by time, then `id`, both descending). `since` is inclusive, `until` exclusive, and `since` must be before `until`.
- AUD-FR-033: The audit query returns hub events under the visibility rule of §14.3.7 and run events of workspaces the caller can read. A `workspace` filter that names a workspace the caller cannot read answers `404 workspace not found`.
- AUD-FR-034: Every query parameter is validated. An unknown enum value, a malformed timestamp, UUID or cursor answers `400 invalid_request`; nothing is silently ignored or turned into an empty result.

**Durable events**

- AUD-FR-040: Every state change listed in §14.8 writes exactly one event per row of the catalogue, with a new `seq`, with the guarantees of §14.3.6. A request that fails or is refused writes no event, unless the catalogue says the event reports the failure.
- AUD-FR-041: `seq` starts at 1 and survives restarts. The hub MUST NOT hand out a `seq` lower than one it has already delivered.
- AUD-FR-042: The stream and the log accept the same filters. `workspace`: one slug, exact match. `types`: a comma-separated list of at most 20 entries; an entry is an exact type or a prefix ending in `*` (`parley.change.*`; `*` alone matches everything); `*` is allowed only as the last character. `session_id`: one UUID. Filters combine with AND; the entries of `types` combine with OR. A type that no event has is not an error.
- AUD-FR-043: A resume position (`after`, or `Last-Event-ID`) is a `seq`: the consumer receives events with a higher `seq`. A position older than the event log answers `410`, `error_type: cursor_expired`, with `oldest_seq` in the error object. A position ahead of the newest `seq` answers `400 invalid_request`. A position is expired when at least one event after it has left the event log, whether or not the caller could have seen that event.
- AUD-FR-044: `GET /events` first replays every visible, matching event after the resume position, then continues with live events on the same connection. There is no gap and no reordering between the two phases. Without a resume position the stream starts with live events only.
- AUD-FR-045: The stream sends a heartbeat when it opens and then every 30 s. A heartbeat carries the `seq` up to which the stream has examined the log for this client. Because it is sent as the SSE event id, a client whose filters match rarely still advances its resume position and does not run into `cursor_expired`.
- AUD-FR-046: Each stream has a buffer of 256 undelivered events. When a client reads too slowly and the buffer is full, or when the client accepts no data for 60 s, the hub closes the stream. It never drops an event from an open stream. The client reconnects with its last `seq` and receives everything it missed.
- AUD-FR-047: At most `[events].max_sse_connections` streams are open at once. A further request answers `503 unavailable` in the error envelope, with `Retry-After: 5`. Long-polls on `GET /events/log` do not count.
- AUD-FR-048: The hub closes a stream when its credential is revoked or expires, and when the hub shuts down. Shutdown closes streams promptly and does not wait for them.
- AUD-FR-049: `GET /events/log` serves the same events as JSON pages in ascending `seq`. With `wait`, a request that would return no event is held until a matching event is written or the wait ends.

**Retention and metrics**

- AUD-FR-060: Retention runs once at start and then every hour, as §14.7.2 describes.
- AUD-FR-061: `GET /metrics` exports every metric of §14.7.3. Each one has at least one series from start-up, so a documented metric is never absent from the output.

**Event webhooks**

- AUD-FR-062 (access). Every event webhook route needs level `owner` or `admin` on the workspace: the workspace's owner or the admin token. An org member gets `403 forbidden`. A workspace-bound token gets `403 token_bound`, whoever its user is. PAT callers need `workspaces:write` for the writing routes (create, update, delete, rotate, ping, redeliver) and `workspaces:read` for the reading routes.
- AUD-FR-063 (forks). A task fork has no event webhooks. Every event webhook route called with a fork's slug answers `400 not_supported_on_fork`. A subscription of the parent with `include_forks` covers the fork.
- AUD-FR-064 (create). A workspace has at most `[webhooks].max_per_workspace` subscriptions, whatever their state; a create beyond that answers `429`, `error_type: webhook_limit_reached`. The body is validated as §14.3.9 documents; a violation answers `400 invalid_request` naming the field. Creating and updating are refused on an archived workspace (`409 workspace_archived`); reading, deleting and delivering are not affected by the workspace's status. Without an encryption key the create answers `503 secrets_unavailable` (§11).
- AUD-FR-065 (signing secret). The hub generates the signing secret at creation and returns it once, as `secret` in the `201` response. A replay of the create with the same `Idempotency-Key` returns the subscription without `secret`. `POST …/rotate-secret` replaces it and returns the new one once; the old secret stops being used with the next attempt. The secret appears in no other response, no event and no log line.
- AUD-FR-066 (destination safety). The hub refuses a URL whose host is, or resolves to, a loopback, link-local, private, unique-local, unspecified, multicast or cloud-metadata address, over IPv4 and IPv6. `[webhooks].allow_private = true` lifts this for all of them. `[webhooks].allow_insecure = true` accepts `http://` URLs whose host is a loopback address, and lifts the refusal for exactly those. The check runs when a subscription is created, when its `url` is changed, and again at every delivery attempt on the address the hub is about to connect to, so a name that later resolves elsewhere is caught. At create and update the refusal is `400`, `error_type: webhook_destination_refused`. At delivery the attempt fails with `error: destination_refused` and is retried like any failure.
- AUD-FR-067 (request). A delivery is one HTTP `POST` to `url`. The body is the hub event record (§14.3.5) as one JSON object, with `Content-Type: application/json`, exactly as the event log returns it. The headers are:

  | Header | Value |
  |---|---|
  | `Parley-Event` | the event's `type` |
  | `Parley-Delivery` | the delivery id. It is the same on every retry and on a redelivery, so the receiver can drop duplicates |
  | `Parley-Seq` | the event's `seq` |
  | `Parley-Timestamp` | Unix time in seconds at which this attempt was sent |
  | `Parley-Signature` | `sha256=` followed by the lower-case hexadecimal HMAC-SHA256 of `<timestamp>.<body>` with the signing secret, where `<timestamp>` is the `Parley-Timestamp` value and `<body>` the exact bytes of the body |
  | `User-Agent` | `parley-webhook/<hub version>` |

  The hub sends no credential of its own and no other header that identifies a user.
- AUD-FR-068 (outcome of an attempt). An attempt succeeds when a response with a `2xx` status arrives within `[webhooks].timeout`. Every other outcome is a failure: another status, a timeout, a connection or TLS error, a refused destination. Redirects are not followed; a `3xx` answer is a failure with `error: redirect`. The response body is not read beyond what is needed to finish the request and is never stored. TLS certificates are verified; there is no switch to turn that off.
- AUD-FR-069 (order and at-least-once). Each subscription has a position in the event log. The hub sends the subscription's events in `seq` order, one at a time: the next event is sent only when the current one has succeeded or been given up. A receiver may get an event more than once (a retry after a lost response, a hub restart during an attempt, a redelivery) and never gets event B before event A when A has the lower `seq`, except through a redelivery. Subscriptions do not wait for each other.
- AUD-FR-070 (retries). After a failed attempt the hub waits 10 s, then twice as long after each further failure, up to 1 h between attempts. It retries one event for up to 24 hours, counted from its first attempt. Then it gives the event up: the delivery becomes `failed`, the subscription becomes `failing` if it is not already, `parley.webhook.failing` is written on that transition, and the hub goes on with the next event.
- AUD-FR-071 (disabling). When no attempt of a subscription has succeeded for 72 hours since `failing_since`, the hub sets it `disabled`, marks its `pending` delivery `failed` and writes `parley.webhook.disabled`. Any successful attempt before that clears `failing_since` and returns a `failing` subscription to `active`.
- AUD-FR-072 (re-enabling). `PATCH` with `active: true` sets a `disabled` or `failing` subscription to `active` and clears `failing_since`. A subscription that was `disabled` resumes with the first event written after the re-enable. Events written while it was disabled are not sent: there is no replay. A consumer that needs them reads the event log from the last `Parley-Seq` it processed (`GET /events/log`). `PATCH` with `active: false` sets `disabled` and drops the pending delivery.
- AUD-FR-073 (ping). `POST …/ping` sends one test delivery at once, outside the order of the subscription, in any state. Its body has the shape of an event record with `type: parley.webhook.ping`, `seq: 0`, a new `id`, the workspace, the caller as actor, `resource_type: event_webhook`, `resource_id` the subscription id and empty metadata. It is signed like any delivery, is not written to the event log, is not retried, and does not change `state`, `last_status` or `failing_since`. The route answers with the delivery record once the attempt has ended.
- AUD-FR-074 (delivery log and redelivery). The hub keeps a delivery record (§14.3.9) per event and subscription, pings included, for 7 days after the last attempt. `POST …/deliveries/:delivery_id/redeliver` sends the event of a `succeeded` or `failed` delivery once more, at once and outside the order, with the same delivery id, and answers with the updated record. It does not change the subscription's position or state. A `pending` delivery answers `409`, `error_type: delivery_pending`. A delivery whose event has left the event log answers `410`, `error_type: event_expired`. A ping cannot be redelivered (`400 invalid_request`).
- AUD-FR-075 (durability). A subscription's position and its pending delivery survive a restart. An attempt that was in flight when the hub stopped is made again after the start. At shutdown the hub abandons attempts in flight without waiting for them.
- AUD-FR-076 (events and metrics). Creating, updating and deleting a subscription and the two state changes the hub makes on its own write the events of §14.8.10. A rotation is reported as `parley.webhook.update` with `fields: ["secret"]`. Every attempt is counted in `parley_webhook_deliveries_total{result}`, and the number of events waiting is exported as `parley_webhook_queue_depth` (§14.7.3).

### 14.5 REST API

Conventions of §3 apply to every route: authentication, check order, error envelope, list envelope (`items`, `next_cursor`, `limit` default 50 and maximum 200, `cursor`), and `Idempotency-Key` on creating `POST`s. Only errors specific to a route are listed. "Session object" means the shape of §14.3.2.

#### `POST /sessions`
- **Purpose:** open an agent session.
- **Scope / access:** `sessions:write`; write access to the workspace.
- **Body:** `workspace_slug` (required), `id` (UUID), `run_id` (run id format), `node_id`, `archetype`, `model`, `metadata` (any JSON).
- **Response:** 201 session object. 200 with the stored session on a replay of `id`.
- **Errors:** 400 (missing `workspace_slug`, malformed `id` or `run_id`); 404 `workspace not found`; 403 `forbidden` (read-only member); 409 `workspace_archived`; 409 `conflict` (the `id` belongs to another user or workspace).

#### `GET /sessions`
- **Purpose:** list sessions.
- **Scope / access:** `sessions:read`. Results are limited to sessions the caller may read (AUD-FR-007).
- **Query:** `workspace` (slug); `include_forks` (`true` also lists sessions on the task forks of `workspace`, including expired forks; default `false`); `run_id`; `status`; `credential_type`; `since`, `until` (on `started_at`; inclusive, exclusive); `order` (`desc` default, `asc`); `limit`; `cursor`.
- **Order:** (`started_at`, `id`) in the chosen direction.
- **Response:** 200 list envelope of session objects.
- **Errors:** 400 (bad parameter; `include_forks` without `workspace`); 404 `workspace not found` (the `workspace` filter names one the caller cannot read).

#### `GET /sessions/:id`
- **Purpose:** read one session with its token summary, forks, changes, sandboxes and counts.
- **Scope / access:** `sessions:read`; AUD-FR-007.
- **Response:** 200 session object.
- **Errors:** 400 (`:id` is not a UUID); 404 `session not found`.

#### `POST /sessions/:id/complete`
- **Purpose:** end an active session.
- **Scope / access:** `sessions:write`; creator or admin token.
- **Body:** `status` (required: `completed`, `failed`, `timeout`), `error_message`, `duration_ms` (integer ≥ 0), `cache_creation_input_tokens` (integer ≥ 0).
- **Response:** 200 session object, updated, or unchanged when it was already terminal.
- **Errors:** 400 (bad `status`, including `terminated`; negative numbers); 404 `session not found`; 403 `forbidden`.

#### `POST /sessions/:id/usage`
- **Purpose:** report one token usage increment.
- **Scope / access:** `sessions:write`; creator or admin token.
- **Body:** `model` (required), `id` (UUID), `input_tokens`, `output_tokens`, `cache_read_tokens` (integers ≥ 0, default 0).
- **Response:** 201 usage record. 200 with the stored record on a replay of `id`; a replay has no effect of any kind.
- **Errors:** 400; 404 `session not found`; 403 `forbidden`; 409 `session_not_active`; 409 `conflict` (the usage `id` belongs to another session).

#### `GET /sessions/:id/usage`
- **Purpose:** list the usage records of a session.
- **Scope / access:** as `GET /sessions/:id`.
- **Query:** `order` (`desc` default, `asc`), `limit`, `cursor`.
- **Order:** (`reported_at`, `id`).
- **Response:** 200 list envelope of usage records, plus `session_id` and `totals` (same shape as `token_summary`, over all records regardless of paging).

#### `GET /workspaces/:slug/cost`
- **Purpose:** token usage of a workspace over a window.
- **Scope / access:** `sessions:read`; read access to the workspace, whatever its status.
- **Query:** `group_by` (`day` default, `session`, `model`); `since` (default: 24 h ago); `until` (default: now); `limit`; `cursor`.
- **Response:** 200

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

  `key` is a UTC date, a session id or a model name, according to `group_by`. Items are ordered by `key` ascending. `period` holds the effective, normalised bounds. `totals` cover the whole window regardless of paging.
- **Errors:** 400 (bad `group_by`, bad timestamp, `since` not before `until`).

#### `POST /workspaces/:slug/runs/:run_id/events`
- **Purpose:** ingest one run event.
- **Scope / access:** `audit:write`; AUD-FR-020.
- **Body:** `event_type` (required), `id`, `severity`, `node_id`, `session_id`, `timestamp`, `payload` (JSON object), `run_id` (must equal the path).
- **Response:** 201 the stored run event; 200 on a replay.
- **Errors:** 400; 404 `workspace not found`; 403 `forbidden`; 409 `workspace_archived`; 409 `conflict` (the `id` exists in another workspace or run); 503 `unavailable` (write timeout, `Retry-After: 5`).

#### `POST /workspaces/:slug/runs/:run_id/events/batch`
- **Purpose:** ingest up to 1000 run events.
- **Body:** JSON array of run event objects.
- **Response:** 200 `{"accepted": <int>, "duplicates": <int>, "errors": [{"index": <int>, "id": "<id or null>", "message": "<text>"}]}`. `errors` is `[]` when there are none. An item whose `id` exists in another workspace or run is reported in `errors`.
- **Errors:** as the single route, plus 400 (not an array; empty array) and 413 `payload_too_large` (more than 1000 items). A 503 means nothing of the batch was stored.

#### `POST /workspaces/:slug/runs/:run_id/sessions/outcomes`
- **Purpose:** ingest one session outcome.
- **Body:** `session_id` (required), `status` (required), `id`, `node_id`, `timestamp`, `duration_ms`, `token_usage`.
- **Response / errors:** as `POST …/events`, with the stored outcome.

#### `POST /workspaces/:slug/runs/:run_id/tools/calls`
- **Purpose:** ingest one tool call.
- **Body:** `tool_name` (required), `id`, `node_id`, `session_id`, `timestamp`, `duration_ms`, `input`, `output`.
- **Response / errors:** as `POST …/events`, with the stored tool call.

#### `POST /workspaces/:slug/runs/:run_id/tools/errors`
- **Purpose:** ingest one tool error.
- **Body:** `tool_name` (required), `error_msg` (required), `id`, `error_code`, `node_id`, `session_id`, `timestamp`.
- **Response / errors:** as `POST …/events`, with the stored tool error.

#### `POST /workspaces/:slug/runs/:run_id/traces`
- **Purpose:** ingest one trace record.
- **Body:** `event_type` (required, closed set of §14.3.4), `id`, `node_id`, `session_id`, `sequence`, `timestamp`, `data`.
- **Response / errors:** as `POST …/events`, with the stored trace.

#### `POST /workspaces/:slug/runs/:run_id/traces/batch`
- **Purpose:** ingest up to 1000 trace records.
- **Body, response, errors:** as `POST …/events/batch`, with trace objects.

#### `POST /workspaces/:slug/runs/:run_id/postmortem`
- **Purpose:** store the postmortem of a run.
- **Body:** `run_status` (required), `started_at`, `completed_at` (required), `task_summary` (required), `cost_summary` (required), `schema_version` (optional; must be 1), `blocked_tasks`, `session_history`.
- **Response:** 201 the stored postmortem. 200 with the stored postmortem when the run already has one in this workspace; the new body is ignored.
- **Errors:** as `POST …/events`; an unsupported `schema_version` answers 400 with `error_type: unknown_schema_version`.

#### `GET /workspaces/:slug/runs/:run_id/events`
- **Purpose:** list the run events of a run.
- **Scope / access:** `audit:read`; read access to the workspace.
- **Query:** `event_type`, `severity`, `node_id`, `session_id` (exact match); `since`, `until`; `order` (`asc` default, `desc`); `limit`; `cursor`.
- **Response:** 200 list envelope of run events (all fields of §14.3.4).
- **Errors:** 400 (bad `run_id` or parameter); 404 `workspace not found`.

#### `GET /workspaces/:slug/runs/:run_id/sessions/outcomes`
- As the run event list. Filters: `node_id`, `session_id`, `status`. Items are session outcomes.

#### `GET /workspaces/:slug/runs/:run_id/tools/calls`
- As the run event list. Filters: `node_id`, `session_id`, `tool_name`. Items are tool calls.

#### `GET /workspaces/:slug/runs/:run_id/tools/errors`
- As the run event list. Filters: `node_id`, `session_id`, `tool_name`. Items are tool errors.

#### `GET /workspaces/:slug/runs/:run_id/traces`
- As the run event list. Filters: `event_type`, `node_id`, `session_id`. Items are trace records.

#### `GET /workspaces/:slug/runs/:run_id/postmortem`
- **Purpose:** read the postmortem of a run.
- **Response:** 200 the postmortem (all fields of §14.3.4).
- **Errors:** 404 with `error_type: postmortem_not_found` when the run has none in this workspace.

#### `GET /workspaces/:slug/runs/:run_id/transcript`
- **Purpose:** rebuild the conversation of one node from the run's trace records.
- **Query:** `node_id` (required).
- **Behaviour:** takes the traces of the run and node, ordered by `timestamp` then `sequence`. `session.init` becomes role `system`, `assistant.message` becomes `assistant`, `tool.use` becomes `tool_use`, `tool.error` becomes `tool_error`; `session.result` is skipped. `content` is `data.content` when that is a string, else `""`. `tool_name` is `data.tool_name` when that is a string, else null.
- **Response:** 200 `{"run_id", "node_id", "items": [{"role", "content", "tool_name", "timestamp"}], "next_cursor"}`. An unknown run or node gives an empty list.
- **Errors:** 400 (bad `run_id`; missing `node_id`); 404 `workspace not found`.

#### `GET /audit`
- **Purpose:** one query over hub events and run events.
- **Scope / access:** `audit:read`; AUD-FR-033.
- **Query:**

| Parameter | Applies to | Meaning |
|---|---|---|
| `source` | both | `hub` or `agent`. Default: both |
| `workspace` | both | Exact slug |
| `types` | both | Same syntax as AUD-FR-042 |
| `session_id` | both | UUID |
| `since`, `until` | both | Inclusive, exclusive |
| `actor_id`, `actor_type`, `resource_type`, `resource_id`, `action` | hub | Exact match. Using one of them excludes run events |
| `run_id`, `node_id`, `severity` | agent | Exact match. Using one of them excludes hub events |
| `limit`, `cursor` | both | List paging |

- **Response:** 200 list envelope. Every item has `source`, `id`, `type`, `time`, `workspace` and `session_id`. A hub item has all other fields of the hub event record (§14.3.5), including `seq`. An agent item has `run_id`, `node_id`, `severity` and `payload`; its `type` is the run event's `event_type` and its `time` the run event's `timestamp`. Fields of the other source are null.
- **Errors:** 400 (bad parameter; `since` not before `until`; a hub-only filter combined with `source=agent`, or an agent-only filter with `source=hub`); 404 `workspace not found`.

#### `GET /events`
- **Purpose:** hub events as a resumable Server-Sent Events stream.
- **Scope / access:** `audit:read`; visibility of §14.3.7.
- **Request:** query `workspace`, `types`, `session_id` (AUD-FR-042) and `after` (a `seq`). The header `Last-Event-ID` also gives a resume position and wins over `after`, so that a client library that reconnects by itself resumes correctly.
- **Response:** 200 with `Content-Type: text/event-stream` and `Cache-Control: no-cache`. The stream holds two kinds of frame:

```
id: 48213
event: hub_event
data: {"seq":48213,"id":"…","type":"parley.change.land","time":"2026-01-02T03:04:05Z","workspace":"api","actor_type":"pat","actor_id":"u_1","resource_type":"change","resource_id":"42","action":"land","session_id":null,"metadata":{…}}

id: 48290
event: heartbeat
data: {"time":"2026-01-02T03:04:35Z","seq":48290}
```

  The `id` of a `hub_event` frame is the event's `seq`. The `id` of a heartbeat is the position up to which the log has been examined for this client (AUD-FR-045). `data` is always one line of JSON. Behaviour: AUD-FR-044 to AUD-FR-048.
- **Errors (all before the stream starts, all in the error envelope):** 400 (bad filter or position); 404 `workspace not found` (the `workspace` filter names one the caller cannot read); 410 `cursor_expired`; 503 `unavailable` (connection limit, `Retry-After: 5`).

  ```json
  {"error": {"code": 410, "message": "event cursor expired", "error_type": "cursor_expired", "oldest_seq": 31007}}
  ```

  To continue after a 410, a consumer resumes with `after = oldest_seq − 1` and accepts that it has missed events.

#### `GET /events/log`
- **Purpose:** the event log as JSON pages, for consumers that poll.
- **Scope / access:** as `GET /events`.
- **Query:** `after` (a `seq`; when absent the page starts at the oldest event in the log); `workspace`, `types`, `session_id`; `limit`; `wait` (whole seconds, 0 to 60, default 0); `cursor` (accepted as another name for `after`).
- **Behaviour:** returns visible, matching events with `seq > after` in ascending `seq`. When there are none and `wait` is above 0, the request is held until one is written or the wait ends, then answers 200 (possibly with no items). A held request is released at once when the hub shuts down.
- **Response:** 200

```json
{"items": [ <hub event>, … ], "next_cursor": "48213", "next_after": 48213, "oldest_seq": 31007, "last_seq": 48290}
```

  `next_after` is always present: pass it as `after` in the next request. It is the `seq` of the last item, or, when the page is not full, the position up to which the log was examined, which may be higher. `next_cursor` is `next_after` as a string when the page was full and more events may follow, else null. `oldest_seq` and `last_seq` bound the event log at the time of the answer.
- **Errors:** 400; 404 `workspace not found`; 410 `cursor_expired` with `oldest_seq`.

#### `POST /workspaces/:slug/webhooks`
- **Purpose:** create an event webhook subscription.
- **Scope / access:** `workspaces:write`; owner or admin; not a workspace-bound token (AUD-FR-062).
- **Body:** `url` (required), `types` (list of strings, default `["*"]`), `include_forks` (bool, default `true`), `description` (string). Accepts `Idempotency-Key`.
- **Response:** 201 the subscription (§14.3.9) plus `secret`, the signing secret. It is shown only here.

```json
{
  "id": "0c1f6c0e-4f0a-4d55-9a53-2c1d8b7e6a10",
  "workspace": "demo",
  "url": "https://ci.example/hooks/parley",
  "types": ["parley.change.*", "parley.git.push"],
  "include_forks": true,
  "description": "CI trigger",
  "state": "active",
  "created_by": "u_1",
  "created_at": "2026-01-02T03:04:05Z",
  "updated_at": "2026-01-02T03:04:05Z",
  "last_delivery_at": null,
  "last_status": null,
  "failing_since": null,
  "secret": "whsec_…"
}
```
- **Errors:** 400 `invalid_request` (bad `url`, `types`, `include_forks` or `description`); 400 `webhook_destination_refused`; 400 `not_supported_on_fork`; 403 `forbidden`; 403 `token_bound`; 409 `workspace_archived`; 429 `webhook_limit_reached`; 503 `secrets_unavailable`.
- **Side effects:** `parley.webhook.create`.

#### `GET /workspaces/:slug/webhooks`
- **Purpose:** list the event webhooks of a workspace.
- **Scope / access:** `workspaces:read`; owner or admin; not a workspace-bound token.
- **Query:** `state` (`active`, `failing`, `disabled`), `limit`, `cursor`.
- **Order:** (`created_at`, `id`) ascending.
- **Response:** 200 list envelope of subscriptions. Never a secret.
- **Errors:** 400 (bad `state`); 400 `not_supported_on_fork`; 403 `forbidden`; 403 `token_bound`.

#### `GET /workspaces/:slug/webhooks/:id`
- **Purpose:** read one event webhook.
- **Scope / access:** as the list.
- **Response:** 200 the subscription, plus `pending`: the number of events waiting to be sent to it.
- **Errors:** 400 (`:id` is not a UUID); 404, `error_type: webhook_not_found` (unknown id, or an id of another workspace); the list's other errors.

#### `PATCH /workspaces/:slug/webhooks/:id`
- **Purpose:** change an event webhook, switch it off, or switch it on again.
- **Scope / access:** `workspaces:write`; owner or admin; not a workspace-bound token.
- **Body:** at least one of `url`, `types`, `include_forks`, `description`, `active` (bool; AUD-FR-072). Fields not given keep their value. A changed `url` or filter applies from the next attempt; the pending delivery is kept.
- **Response:** 200 the subscription.
- **Errors:** 400 `invalid_request` (bad value, unknown field, no field); 400 `webhook_destination_refused`; 400 `not_supported_on_fork`; 403 `forbidden`; 403 `token_bound`; 404 `webhook_not_found`; 409 `workspace_archived`.
- **Side effects:** `parley.webhook.update`.

#### `DELETE /workspaces/:slug/webhooks/:id`
- **Purpose:** delete an event webhook. Its pending delivery, its delivery log and its signing secret go with it.
- **Scope / access:** as `PATCH`. Works in any workspace state.
- **Response:** 204.
- **Errors:** 400 `not_supported_on_fork`; 403 `forbidden`; 403 `token_bound`; 404 `webhook_not_found`.
- **Side effects:** `parley.webhook.delete`.

#### `POST /workspaces/:slug/webhooks/:id/rotate-secret`
- **Purpose:** replace the signing secret (AUD-FR-065). No body.
- **Scope / access:** as `PATCH`.
- **Response:** 200 the subscription plus `secret`, the new signing secret. It is shown only here.
- **Errors:** as `PATCH` without the body errors; 503 `secrets_unavailable`.
- **Side effects:** `parley.webhook.update` with `fields: ["secret"]`.

#### `POST /workspaces/:slug/webhooks/:id/ping`
- **Purpose:** send a `parley.webhook.ping` test delivery now (AUD-FR-073). No body.
- **Scope / access:** as `PATCH`. Works in any subscription state and any workspace state.
- **Response:** 200 the delivery record (§14.3.9), with `status` `succeeded` or `failed`. A failed ping is a 200: the route reports the attempt, it does not fail with it.
- **Errors:** 400 `not_supported_on_fork`; 403 `forbidden`; 403 `token_bound`; 404 `webhook_not_found`.
- **Side effects:** none besides the delivery record. No hub event.

#### `GET /workspaces/:slug/webhooks/:id/deliveries`
- **Purpose:** list the recent deliveries of an event webhook.
- **Scope / access:** as the list of subscriptions.
- **Query:** `status` (`pending`, `succeeded`, `failed`), `limit`, `cursor`.
- **Order:** (`created_at`, `id`) descending: newest first.
- **Response:** 200 list envelope of delivery records: `id`, `seq`, `type`, `status`, `attempts`, `response_code`, `error`, `duration_ms`, `created_at`, `last_attempt_at`, `next_attempt_at`. Records older than 7 days are gone.
- **Errors:** 400 (bad `status`); 400 `not_supported_on_fork`; 403 `forbidden`; 403 `token_bound`; 404 `webhook_not_found`.

#### `POST /workspaces/:slug/webhooks/:id/deliveries/:delivery_id/redeliver`
- **Purpose:** send the event of a past delivery once more (AUD-FR-074). No body.
- **Scope / access:** as `PATCH`. Works in any subscription state.
- **Response:** 200 the delivery record after the attempt.
- **Errors:** 400 `invalid_request` (the delivery is a ping; `:delivery_id` is not a UUID); 400 `not_supported_on_fork`; 403 `forbidden`; 403 `token_bound`; 404 `webhook_not_found`; 404, `error_type: delivery_not_found`; 409 `delivery_pending`; 410 `event_expired`.

#### `GET /metrics`
- **Purpose:** Prometheus text exposition of the metrics of §14.7.3.
- **Auth:** none. The route is outside `/api/v1`. Label values include workspace slugs, so operators should not expose it to untrusted networks.
- **Response:** 200 `text/plain; version=0.0.4`.

### 14.6 CLI

The conventions of §3 apply: JSON on stdout, the error envelope on stderr, exit codes 0, 1 and 2.

#### `parley session start <workspace-slug>`
Opens a session and prints the session object. Flags: `--id <uuid>`, `--run-id <id>`, `--node-id <id>`, `--archetype <text>`, `--model <name>`, `--metadata <json>`. The printed `id` is what `git push -o parley.session=<id>` and the `--session` flags of other commands take.

#### `parley session usage <session-id>`
Reports one usage increment. Flags: `--model <name>` (required), `--input <n>`, `--output <n>`, `--cache-read <n>`, `--id <uuid>`.

#### `parley session complete <session-id>`
Ends a session. Flags: `--status completed|failed|timeout` (default `completed`), `--error <text>`, `--duration-ms <n>`.

#### `parley session get <session-id>`
Prints the session object, with its forks, changes, sandboxes and counts.

#### `parley session list`
Lists sessions. Flags: `--workspace <slug>`, `--include-forks`, `--run-id <id>`, `--status <status>`, `--since <time>`, `--limit <n>`, `--cursor <c>`.

#### `parley audit query`
Calls `GET /audit`. Flags: `--source hub|agent`, `--workspace <slug>`, `--types <list>`, `--session <id>`, `--run-id <id>`, `--actor-id <id>`, `--resource-type <t>`, `--resource-id <id>`, `--action <a>`, `--since <time>`, `--until <time>`, `--limit <n>`, `--cursor <c>`.

#### `parley events tail`
Follows the event stream. Flags: `--workspace <slug>`, `--types <list>`, `--session <id>`, `--after <seq>`.
- Prints one hub event per line as compact JSON (not indented), so the output can be piped line by line. Heartbeats are not printed.
- Without `--after` it starts with live events. With `--after` it replays first.
- Runs until interrupted, without a request timeout. An interrupt exits 0.
- When the stream ends for any reason other than an interrupt (hub restart, slow-consumer close, network error, `503`), the command reconnects by itself, waiting 1 s at first and doubling up to 30 s, and resumes from the last `seq` it received. No event is lost and none is printed twice.
- It stops with exit 1 on an error that a retry cannot fix: `401`, `403`, `404`, `400`, and `410 cursor_expired`. On `cursor_expired` the error on stderr includes `oldest_seq`; the command never skips events silently.

#### `parley events list`
Calls `GET /events/log` once and prints the response. Flags: `--workspace <slug>`, `--types <list>`, `--session <id>`, `--after <seq>`, `--limit <n>`, `--wait <seconds>`. The request timeout is extended by the wait.

#### `parley webhook create <workspace-slug>`
Calls `POST /workspaces/:slug/webhooks` and prints the response, which includes `secret`. Flags: `--url <url>` (required), `--types <list>` (comma-separated), `--no-forks` (sends `include_forks: false`), `--description <text>`. A note on stderr says that the secret is shown only once.

#### `parley webhook list <workspace-slug>`
Calls `GET /workspaces/:slug/webhooks`. Flags: `--state <state>`, `--limit <n>`, `--cursor <c>`.

#### `parley webhook get <workspace-slug> <webhook-id>`
Calls `GET /workspaces/:slug/webhooks/:id` and prints the subscription.

#### `parley webhook update <workspace-slug> <webhook-id>`
Calls `PATCH /workspaces/:slug/webhooks/:id`. Flags: `--url <url>`, `--types <list>`, `--forks` and `--no-forks` (mutually exclusive), `--description <text>`, `--enable` and `--disable` (mutually exclusive; they send `active: true` and `active: false`). At least one flag is required, else exit 2.

#### `parley webhook delete <workspace-slug> <webhook-id>`
Calls `DELETE /workspaces/:slug/webhooks/:id`. Prints `{"status":"deleted","id":"<webhook-id>"}`.

#### `parley webhook rotate-secret <workspace-slug> <webhook-id>`
Calls `POST …/webhooks/:id/rotate-secret` and prints the response, which includes the new `secret`. A note on stderr says that the secret is shown only once.

#### `parley webhook ping <workspace-slug> <webhook-id>`
Calls `POST …/webhooks/:id/ping` and prints the delivery record. Exit 0 when the delivery `succeeded`; exit 1 when it `failed`.

#### `parley webhook deliveries <workspace-slug> <webhook-id>`
Calls `GET …/webhooks/:id/deliveries`. Flags: `--status <status>`, `--limit <n>`, `--cursor <c>`.

#### `parley webhook redeliver <workspace-slug> <webhook-id> <delivery-id>`
Calls `POST …/webhooks/:id/deliveries/:delivery_id/redeliver` and prints the delivery record. Exit codes as `parley webhook ping`.

### 14.7 Background behaviour

#### 14.7.1 Start-up and shutdown

- At start, before serving requests, the hub writes any event that a ref update still owes (§14.3.6).
- Sessions need no recovery at start. Stale active sessions are handled by retention.
- At shutdown the hub closes all event streams and releases all long-polls. Consumers resume from their last `seq` after the restart.
- At start the hub resumes event webhook delivery for every `active` and `failing` subscription from its stored position (AUD-FR-075). At shutdown it abandons the attempts in flight.

#### 14.7.2 Retention

Retention runs once at start and then every hour. Its steps run in the order below. A failing step is logged and counted on `parley_retention_errors_total{step}`; the remaining steps still run. `parley_retention_last_run_timestamp_seconds` is set only when every step succeeded.

| Step (`step` label) | Action |
|---|---|
| `stale_sessions` | `active` sessions started before now − `[sessions].max_active_age` become `timeout`, with `ended_at` = now and `error_message` = `session exceeded maximum active age` |
| `run_records` | Delete run events, session outcomes, tool calls and tool errors with `timestamp` older than `[audit].max_age` |
| `max_runs` | Per workspace, when more than `[audit].max_runs` runs have telemetry, delete the run events, outcomes, tool calls, tool errors and traces of the oldest excess runs. A run's age is its earliest record. Postmortems are kept |
| `traces` | Delete trace records older than `[audit].trace_max_age` |
| `postmortems` | Delete postmortems ingested before now − `[audit].postmortem_max_age` |
| `sessions` | Delete terminal sessions started before now − `[sessions].max_age`, together with their usage records. Active sessions are never deleted |
| `event_log` | Events older than `[events].retention` leave the event log. `oldest_seq` moves forward accordingly |
| `webhook_deliveries` | Delete event webhook delivery records whose last attempt is older than 7 days. `pending` records are never deleted |
| `hub_events` | Delete hub events older than `[audit].max_age` |
| `orphans` | Delete sessions, usage records, telemetry and hub events of workspaces that no longer exist, once they are older than `[audit].orphan_retention`. Records of a deleted task fork count as belonging to its parent while the parent exists, so they are kept under the normal limits |
| `gauges` | Recompute `parley_audit_records{kind}` and `parley_agent_sessions_active{workspace}` from the stored data |

The newest event always stays in the event log, whatever its age, so `last_seq` is always known.

#### 14.7.4 Event webhook delivery

A delivery loop runs for every subscription that is `active` or `failing`. It is driven by new events and by its own retry timer, not by the maintenance loop.

1. Read the next event after the subscription's position that the subscription may receive and that matches its `types` (§14.3.9). Events that do not match move the position forward without a delivery.
2. Record a `pending` delivery and make the first attempt (AUD-FR-067).
3. On success: mark the delivery `succeeded`, move the position past the event, set `last_status`, clear `failing_since`, return a `failing` subscription to `active`.
4. On failure: set `last_status` and, if it is null, `failing_since`. Schedule the next attempt (AUD-FR-070). After 24 hours give the event up and move the position past it.
5. A timer checks at least once a minute for subscriptions that have failed for 72 hours and disables them (AUD-FR-071).

At most one attempt per subscription is in flight. A slow or dead receiver delays only its own subscription. The hub reads a subscription's events from the event log; an event that has left the log before its turn came (possible only when `[events].retention` is shorter than the backlog) is skipped and counted as `failed`.

#### 14.7.3 Prometheus metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `parley_http_requests_total` | counter | `method`, `path` (route template), `status` | Every HTTP request, API, git and webhook routes included |
| `parley_http_request_duration_seconds` | histogram | `method`, `path` | Buckets 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10 |
| `parley_git_requests_total` | counter | `service` (`info-refs`, `upload-pack`, `receive-pack`), `result` (`ok`, `refused`, `error`) | Git protocol requests (§5). `refused` covers authentication, access and policy refusals |
| `parley_scm_requests_total` | counter | `op` (`refs`, `commits`, `tree`, `file`, `diff`, `compare`, `blame`, `search`, `commit`, `rebase`), `result` (`ok`, `ref_moved`, `refused`, `error`) | Source control API requests (§6) |
| `parley_changes_open` | gauge | `workspace` | Changes in state `draft` or `open` (§7) |
| `parley_lands_total` | counter | `result` (`landed`, `conflict`, `blocked`, `failed`) | Land requests by final result (§8) |
| `parley_land_attempts` | histogram | none | Compare-and-swap attempts per land. Buckets 1, 2, 3, 5, 8, 13 |
| `parley_land_duration_seconds` | histogram | none | Time from land request to final result. Buckets 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60 |
| `parley_forks_active` | gauge | none | Task forks that exist (§4) |
| `parley_events_seq` | gauge | none | The last `seq` written |
| `parley_sse_connections` | gauge | none | Open event streams |
| `parley_webhook_deliveries_total` | counter | `result` (`ok`, `retry`, `failed`) | Event webhook attempts: `ok` for a successful attempt, `retry` for a failed attempt that will be retried, `failed` for an attempt after which the event was given up, a failed ping or redelivery, and an event skipped because it left the log |
| `parley_webhook_queue_depth` | gauge | none | Events written and not yet delivered or given up, summed over all `active` and `failing` subscriptions |
| `parley_audit_events_total` | counter | `source` (`hub`, `agent`), `type` | Hub events written and run events ingested. Run events ingested in a batch are counted under their own type |
| `parley_agent_sessions_active` | gauge | `workspace` | Sessions in state `active` |
| `parley_agent_tokens_total` | counter | `workspace`, `model`, `direction` (`input`, `output`, `cache_read`) | Tokens reported in usage records. Replays are not counted |
| `parley_audit_records` | gauge | `kind` (`hub_event`, `run_event`, `outcome`, `tool_call`, `tool_error`, `trace`, `postmortem`, `session`, `usage`) | Stored records, sampled by retention |
| `parley_jobqueue_depth` | gauge | `type` (`rebuild`), `status` (`queued`, `running`, `failed`, `completed`, `dead_letter`, `cancelled`) | Durable jobs by job type and job status (§10) |
| `parley_retention_errors_total` | counter | `step` | Failed retention steps |
| `parley_retention_last_run_timestamp_seconds` | gauge | none | Unix time of the last fully successful retention run |
| `parley_sandboxes` | gauge | `state` (`provisioning`, `ready`, `stopped`, `completed`, `failed`) | Sandboxes by state, without `deleted` (§13) |
| `parley_sandbox_creates_total` | counter | `result` (`ready`, `failed`) | Sandbox creations by outcome (§13) |
| `parley_sandbox_seconds` | histogram | none | Time from `ready` to stop, exit or deletion. Buckets 60, 300, 600, 1200, 1800, 3600, 7200, 14400, 28800, 86400 |
| `parley_sandbox_exec_total` | counter | `result` (`ok`, `nonzero`, `timeout`, `error`) | Commands run through exec (§13) |
| `parley_sandbox_denials_total` | counter | none | Connections and requests the sandbox runtime blocked (§13) |
| `parley_sandbox_checkpoints_total` | counter | `result` (`ok`, `empty`, `failed`) | Checkpoints attempted (§13) |
| `parley_sandbox_runtime_up` | gauge | none | 1 while the sandbox runtime is reachable. The sandbox metrics are exported with value 0 when sandboxes are disabled |

Label values with a closed set are all present from start-up with value 0.

### 14.8 Events

This is the single catalogue of hub event types. Other sections describe when their events fire in more detail; the types, resource fields and metadata keys here are authoritative. Every event has the record shape of §14.3.5. `action` is always the last segment of the type and is not repeated in the tables. "Slug" means the slug of the event's workspace. Unless a row says otherwise, the event's `workspace` is the workspace of the route that caused it, the actor is the caller, and the event is written only on success.

Run event types are free-form strings chosen by agents and are not part of this catalogue.

#### 14.8.1 Workspaces and task forks (§4)

| Type | Emitted when | `resource_type` / `resource_id` | Metadata |
|---|---|---|---|
| `parley.workspace.create` | A repository workspace was created | `workspace` / slug | `kind`, `git_url` (no userinfo), `workspace_mode`, `default_branch` (may be null) |
| `parley.workspace.update` | A workspace was updated | `workspace` / slug | `fields` (names of the fields given) |
| `parley.workspace.archive` | A workspace was archived | `workspace` / slug | `head_sha`, `origin_push` (`ok`, `partial`, `failed`), `forks` (count) |
| `parley.workspace.reactivate` | An archived workspace was reactivated | `workspace` / slug | none |
| `parley.workspace.delete` | A workspace or task fork was deleted by a caller, or a fork was deleted together with its parent (one event per fork and one for the parent). `workspace` is the deleted slug | `workspace` / slug | `kind`, `parent_slug` (forks), `revoked_tokens` (sorted ids of the bound tokens revoked), `deleted_forks` (slugs; parent only) |
| `parley.workspace.sync` | A sync from origin completed, also when branches are `ahead` or `diverged`. A failed sync emits nothing | `workspace` / slug | `created`, `fast_forward`, `ahead`, `diverged`, `reset` (lists of branch names), `updated` (list of `{ref, before, after}`) |
| `parley.workspace.fork` | A task fork was created. `workspace` is the fork's slug | `workspace` / fork slug | `parent_slug`, `fork_point` (`{ref, sha}`), `expires_at`, `auto_change`, `purpose`, `token_id` (when a token was minted) |
| `parley.fork.expire` | The expiry sweep deleted a task fork. `workspace` is the fork's slug. Actor `system`. A fork deleted by request or together with its parent emits `parley.workspace.delete` instead | `workspace` / fork slug | `parent_slug`, `reason` (`ttl`, `landed`), `expires_at`, `revoked_tokens` |
| `parley.workspace.push` | A push of a branch to origin was attempted. Written on success and on failure of the push itself; a request the hub refuses emits nothing | `branch` / branch name | `branch`, `sha`, `force`, `ok`, `remote_url` (no userinfo), then `result` (when `ok`) or `error_type` (when not) |
| `parley.workspace.reclone` | A reclone was accepted | `workspace` / slug | `origin_push` (`ok`, `partial`, `failed`) |
| `parley.session.force_closed` | An active session was terminated because its workspace was archived or deleted, or its fork expired. One per session. Actor `system` | `session` / session id | `reason` (`workspace archived`, `workspace deleted`, `fork expired`) |

Events whose `workspace` is a fork (`parley.workspace.fork`, `parley.workspace.delete` of a fork, `parley.fork.expire`, and everything else that happens on the fork) are visible to readers of the parent (§14.3.7), so the owner of a workspace sees its forks come and go.

#### 14.8.2 Tokens (§3)

| Type | Emitted when | `resource_type` / `resource_id` | Metadata |
|---|---|---|---|
| `parley.token.bind` | A token bound to a workspace was created, including the token issued with a fork and the hub credential the hub mints for a sandbox at create and at each start (§13) | `token` / token id | `token_id`, `workspace`, `permissions`, `paths` (or null), `expires_at` (or null). Never the token |
| `parley.token.bound_refused` | A bound token was refused because the request was outside its workspace. At most one event per token per minute. `workspace` is the bound workspace; the actor is the token's user | `token` / token id | `token_id`, `bound_workspace`, `requested_workspace` (when the request named one), `method`, `route`, `status` (403 or 404), `surface` (`api`, `git`), `suppressed` (refusals not reported since the previous event) |

#### 14.8.3 Git server and source control API (§5, §6)

| Type | Emitted when | `resource_type` / `resource_id` | Metadata |
|---|---|---|---|
| `parley.git.push` | A ref was updated: by a git push (§5), through the source control API (§6: ref create, move, delete, commit, rebase), or by a land moving its target (§8). One event per accepted ref update, in order; a rejected update emits nothing | `ref` / full ref name | `ref`, `before`, `after` (shas; null on the side that does not exist), `created`, `deleted`, `forced`, `direct` (booleans; `forced` means the update was not a fast-forward; `direct` means the owner or the admin token fast-forwarded a protected branch without a change, §8.3.1, and is false in every other case), `via` (`git`, `api` or `land`) |
| `parley.git.fetch` | A fetch or clone was served. Only when the workspace setting `audit_fetches` is on (default off) | `workspace` / slug | `shallow` (boolean), `filter` (partial-clone filter or null) |
| `parley.scm.commit` | A commit was created through the source control API | `commit` / new sha | `branch`, `sha`, `parent_sha`, `files` (paths touched, at most 100) |

All events of one push are written together and have consecutive positions among the workspace's events.

#### 14.8.4 Changes and landing (§7, §8)

For every type in this table `resource_type` is `change`, `resource_id` is the change number as a decimal string, and `workspace` is the workspace the change belongs to (the workspace of its target branch). Every event also has `number` in its metadata. The events owned by §7 (`open`, `version`, `restack`, `conflict`, `ready`, `abandon`, `restore`) also carry `ref` (`<slug>#<number>`), `version` (the current version) and `head_sha`.

| Type | Emitted when | Actor | Metadata beyond `number` |
|---|---|---|---|
| `parley.change.open` | A change entered state `open`: created open, or a draft was published | caller | `title`, `target_branch`, `source_workspace`, `source_branch`, `depends_on` (numbers), `was_draft` |
| `parley.change.version` | A change got a new version, including version 1 of a draft; or the change was retargeted (`kind` is then `retarget` and no version is created) | the pusher, committer or caller; `system` for a restack done by the hub and for a `sync` version | `kind` (`push`, `commit`, `ref`, `rebase`, `restack`, `revert`, `sync`, `retarget`), `base_sha`, `patch_equivalent_to`, `draft` |
| `parley.change.restack` | The hub restacked the change, on its own or on `POST …/restack`. Written after the `parley.change.version` of the new version | `system`; the caller for a manual restack | `onto_sha`, `trigger` (`version`, `land`, `manual`, `abandon`) |
| `parley.change.ready` | The change became landable: its `landing.status` is `ready` and its `mergeable` is `clean`, for its current version | `system` | none |
| `parley.change.conflict` | The change's `mergeable` became `conflict`, or a restack of it failed. Not repeated while it stays so | `system` | `files` (paths), `needs_restack` |
| `parley.change.overlap` | A new version made two changes overlap that did not overlap before. One event on each of the two changes, naming the other. A pair is announced once; if the two stop overlapping and later overlap again, it is announced again | `system` | `with` (number and ref of the other change), `with_session_id`, `paths` (at most 100), `count` |
| `parley.change.approve` | An approving verdict was recorded | caller | `version`, `approval_id`, `counts_for` |
| `parley.change.reject` | A rejecting verdict was recorded | caller | `version`, `approval_id`, `counts_for` |
| `parley.change.check` | A check result was reported | caller | `version`, `name`, `status` |
| `parley.change.land` | A land attempt ended `landed`. One event per landed change, also for a stack | the caller of the land; `system` for an automatic land | `land_id`, `target_branch`, `onto_sha`, `landed_sha`, `strategy`, `attempts`, `noop`, `trigger`, `override`, `policy_revision` |
| `parley.change.land_fail` | A land attempt ended `conflict`, `blocked` or `failed`, or an automatic land could not start because the user who set `auto_land` may no longer land (`reason: not_permitted`) | as above | `land_id`, `status` (`conflict`, `blocked`, `failed`), `reason`, `files` (on `conflict`) |
| `parley.change.abandon` | The change was abandoned: by request, because its source branch was deleted (`reason: source_deleted`), because its source branch was gone after a reclone (`reason: source_lost`), or because its source fork was deleted or expired | caller (for `source_deleted`, the pusher who deleted the branch); `system` for a reclone and for a fork deletion or expiry | `reason` |
| `parley.change.restore` | An abandoned change was restored | caller | none |
| `parley.change.revert` | A reverting change was opened for this landed change. Written on the landed change; the new change gets its own `parley.change.open` | caller | `reverted_by` (number of the reverting change) |
| `parley.policy.update` | The landing policy of a workspace was replaced. `resource_type` `policy`, `resource_id` slug | caller | `revision`, `rules` (number of rules) |

A land that loses a compare-and-swap race and is retried emits nothing for the lost attempt; `attempts` on the final event counts them.

#### 14.8.5 Awareness (§9)

`resource_type` is `claim` and `resource_id` the claim id. `workspace` is the workspace the claim is stored on: the parent, when the claim was made through a fork's slug (§9).

| Type | Emitted when | Actor | Metadata |
|---|---|---|---|
| `parley.claim.create` | A claim was made | caller | `paths`, `intent`, `target_branch`, `expires_at` |
| `parley.claim.release` | A claim was released by request | caller | `paths` |
| `parley.claim.expire` | A claim reached its expiry | `system` | `paths` |

#### 14.8.6 Carry-patch (§10)

`resource_type` is `patch` with `resource_id` the patch id, or `rebuild` with `resource_id` the rebuild job id. Patch events carry the branch as `branch_name` in metadata.

| Type | Emitted when | Actor | `resource_type` | Metadata |
|---|---|---|---|---|
| `parley.patch.create` | A patch was registered. One event per patch, also for a batch | caller | `patch` | `branch_name`, `position`, `branch_resolution` |
| `parley.patch.update` | A patch's status, position, description or PR URL changed | caller, or `system` | `patch` | `branch_name`, `changed` (field names), and for a status change `from`, `to`, `reason` (`api`, `rebuild_conflict`, `rebuild_clean`, `merged_upstream`) |
| `parley.patch.reorder` | The patch list was reordered. One event; `resource_id` is the first patch of the new order | caller | `patch` | `patch_ids` (new order) |
| `parley.patch.delete` | A patch was soft-deleted | caller, or `system` when a rebuild removed a merged patch | `patch` | `branch_name`, `reason` (`api`, `merged_upstream`) |
| `parley.patch.restore` | A deleted patch was restored | caller | `patch` | `branch_name`, `position` |
| `parley.patch.reset` | A reset to origin succeeded, whatever the action | caller | `patch` | `branch_name`, `action` (`none`, `created`, `fast_forwarded`, `replaced`), `local_sha`, `origin_sha`, `replaced_sha` |
| `parley.patch.replace` | A patch branch was replaced by origin's, by a reset or by a sync | caller | `patch` | `branch_name`, `replaced_sha`, `origin_sha`, `trigger` (`reset_to_origin`, `sync`) |
| `parley.patch.sync` | A sync in origin mode moved a patch branch or changed its origin state. One event per such patch | caller | `patch` | `branch_name`, `action`, `state`, `local_sha`, `origin_sha` |
| `parley.patch.mirror_failed` | Mirroring a patch branch to origin failed. One per branch | `system` | `patch` | `branch_name`, `error` |
| `parley.rebuild.enqueue` | A rebuild job was queued, from any source (API, push, sync, landing) | caller, or `system` | `rebuild` | `submitted_by`, `strategy`, `fail_mode`, `patch_count` |
| `parley.rebuild.complete` | A rebuild published | `system` | `rebuild` | `upstream_head_sha`, `integration_head_sha`, `previous_integration_head_sha`, `patches_applied`, `patches_skipped`, `patches_conflicted`, `patches_removed` |
| `parley.rebuild.fail` | A rebuild attempt failed, attempts that will be retried included | `system` | `rebuild` | `reason`, `will_retry`, `retry_count`, `conflict` (the conflict report, when the failure was a conflict) |
| `parley.rebuild.push_failed` | Pushing the integration branch to origin failed after a rebuild | `system` | `rebuild` | `integration_branch`, `reason` |
| `parley.rebuild.followup` | The stale-input check queued a follow-up rebuild. `resource_id` is the job that ended | `system` | `rebuild` | `follow_up_job_id`, `stale_patches` (branch names), `upstream_stale` |
| `parley.rebuild.cancel` | A queued rebuild job was cancelled | caller | `rebuild` | none |
| `parley.rebuild.requeue` | A dead-lettered rebuild job was requeued | caller | `rebuild` | none |
| `parley.rebuild.rollback` | The integration branch was rolled back | caller | `rebuild` | `integration_branch`, `rolled_back_to`, `previous_head_sha` |

#### 14.8.7 Secrets and variables (§11)

Six types: `parley.secret.create`, `parley.secret.update`, `parley.secret.delete`, `parley.variable.create`, `parley.variable.update`, `parley.variable.delete`. One event per key. `resource_type` is `secret` or `variable`; `resource_id` is the key. Metadata: `scope` (`user`, `org`, `workspace`), `scope_id` (the user id, org id or workspace slug) and `key`. Never a value.

At workspace scope the event's `workspace` is the slug. At user and org scope `workspace` is null, and the scope decides who sees the event (§14.3.7). Credentials that the hub stores itself while creating a workspace or setting a webhook secret emit no secret event.

#### 14.8.8 Forge integration (§12)

**Writes to the forge.** One event per write request that reached the forge, on success and on failure. `workspace` is the workspace whose forge was written: for a call through a task fork that is the parent, and `metadata.fork` holds the fork's slug. Common metadata: `target` (`origin`, `upstream`), `ok`, `error_type` (when not `ok`), `number` and `url` (when known), `fork` (for a call through a fork). When the forge assigned no number because the call failed, `resource_id` is `none`.

| Type | `resource_type` / `resource_id` | Extra metadata |
|---|---|---|
| `parley.forge.issue.create` | `issue` / number | none |
| `parley.forge.issue.update` | `issue` / number | `fields` |
| `parley.forge.issue.close` | `issue` / number | `comment_url` (when a comment was posted) |
| `parley.forge.comment.create` | `comment` / issue number | none |
| `parley.forge.label.add` | `label` / issue number | `labels` |
| `parley.forge.label.remove` | `label` / issue number | `label` |
| `parley.forge.label.create` | `label` / label name | `label`, `created` |
| `parley.forge.pull_request.create` | `pull_request` / number | `head`, `base`, `draft` |
| `parley.forge.pull_request.comment` | `pull_request` / number | none |

**Webhook configuration.** `parley.forge.webhook.set` (metadata `generated`) and `parley.forge.webhook.unset` (metadata `existed`). `resource_type` `webhook`, `resource_id` slug.

**Deliveries from the forge.** Written once per verified, newly recorded delivery, in the same transaction that records the delivery. A duplicate delivery emits nothing. One delivery may produce several events, in a fixed order. Actor `forge`. Common metadata: `forge` (`github`, `gitlab`), `target`, `delivery_id`, `actor` (the forge login).

| Type | `resource_type` / `resource_id` | Extra metadata |
|---|---|---|
| `parley.forge.issue.opened`, `.edited`, `.closed`, `.reopened` | `issue` / number | `number`, `title` (first 256 bytes), `url`, `labels` |
| `parley.forge.issue.labeled`, `.unlabeled` | `issue` / number | `number`, `label`, `labels`, `url` |
| `parley.forge.comment.created` | `comment` / issue or pull request number | `number`, `comment_url`, `pull_request` (boolean) |
| `parley.forge.pull_request.opened`, `.closed`, `.merged`, `.reopened`, `.synchronize` | `pull_request` / number | `number`, `url`, `head`, `base`, `merged` |
| `parley.forge.push` | `ref` / pushed ref | `ref`, `before`, `after` |

#### 14.8.9 Sandboxes (§13)

Except for the two rule events, `resource_type` is `sandbox` and `resource_id` the sandbox id. `workspace` is the sandbox's workspace and `session_id` the sandbox's. Metadata never contains a credential, a secret value or command output; exposed secrets appear by name only.

| Type | Emitted when | Actor | Metadata |
|---|---|---|---|
| `parley.sandbox.create` | The sandbox was stored | caller | `profile`, `purpose`, `command` (first argument only), `credentials`, `exposed_secrets` (names of the secrets placed in the environment; `[]` when none), `workspace_rules` (ids), `hub_access`, `expires_at`, `restored_from` |
| `parley.sandbox.ready` | The sandbox became `ready` | `system` | `checkout` |
| `parley.sandbox.fail` | The sandbox became `failed` other than by exit | `system` | `reason`, `error` (scrubbed) |
| `parley.sandbox.exit` | The main process ended | `system` | `exit_code` |
| `parley.sandbox.stop` | The sandbox became `stopped` | caller, or `system` | `reason` (`api`, `idle`, `archived`) |
| `parley.sandbox.start` | A start was accepted | caller | none |
| `parley.sandbox.checkpoint` | A checkpoint was attempted | caller, or `system` | `ok`, `refs` (`[{branch, sha, dirty}]`), `error` |
| `parley.sandbox.deny` | First denial of a destination per sandbox (SBX-FR-051) | `system` | `program`, `host`, `port`, `method`, `path` |
| `parley.sandbox.proposal` | A network access proposal arrived | `system` | `proposal_id`, `summary`, `max_severity` |
| `parley.sandbox.proposal_decided` | A proposal was approved or rejected | caller | `proposal_id`, `decision`, `acknowledged_findings`, `scope` (`sandbox`, `workspace`; null for a rejection), `rule_id` (when a workspace sandbox rule was saved or matched) |
| `parley.sandbox.delete` | The sandbox became `deleted` | caller, or `system` | `reason` (`api`, `expired`, `workspace_deleted`) |
| `parley.sandbox.rule_add` | An approval with `scope: workspace` saved a workspace sandbox rule. `resource_type` `sandbox_rule`, `resource_id` the rule id. `workspace` is the workspace that holds the rule: the parent, when the proposal came from a fork's sandbox. `session_id` is the sandbox's | caller | `rule_id`, `summary`, `max_severity`, `from_proposal` (`{sandbox_id, proposal_id, workspace}`) |
| `parley.sandbox.rule_remove` | A workspace sandbox rule was deleted by request. `resource_type` `sandbox_rule`, `resource_id` the rule id. `session_id` null | caller | `rule_id`, `summary` |

The two rule events have normal workspace visibility (§14.3.7).

Exec, attach and file transfers emit no hub event; they are recorded as run telemetry (AUD-FR-026).

#### 14.8.10 Event webhooks (§14.3.9)

`resource_type` is `event_webhook` and `resource_id` the subscription id. `workspace` is the subscription's workspace. These events are visible only at `owner` or `admin` level, like token events (§14.3.7). Metadata never holds the signing secret, and holds the host of the URL, not the URL, because a path or query may carry a token of the receiver.

| Type | Emitted when | Actor | Metadata |
|---|---|---|---|
| `parley.webhook.create` | A subscription was created | caller | `webhook_id`, `host`, `types`, `include_forks` |
| `parley.webhook.update` | A subscription was changed, switched on or off by request, or its secret was rotated | caller | `webhook_id`, `fields` (names of the fields given; `secret` for a rotation), `state` |
| `parley.webhook.delete` | A subscription was deleted by request. Subscriptions removed with their workspace emit nothing of their own | caller | `webhook_id`, `host` |
| `parley.webhook.failing` | The hub gave an event up after 24 hours and the subscription became `failing`. Once per transition | `system` | `webhook_id`, `host`, `seq` (the event given up), `failing_since`, `response_code`, `error` |
| `parley.webhook.disabled` | The hub disabled the subscription after 72 hours of continuous failure | `system` | `webhook_id`, `host`, `failing_since` |

`parley.webhook.ping` is not a hub event. It is the type of the test delivery (AUD-FR-073) and never appears in the event log, the stream or the audit query.

### 14.9 Configuration

Keys of the hub configuration file (§3). Durations take a number and a unit (`s`, `m`, `h`, `d`). A value that is malformed or out of range stops the hub at start with a message that names the key.

| Key | Default | Range | Effect |
|---|---|---|---|
| `[events].retention` | `30d` | `1h` to `[audit].max_age` | How long an event stays in the event log and can be resumed from |
| `[events].max_sse_connections` | `500` | ≥ 1 | Open event streams allowed at once |
| `[audit].max_age` | `90d` | ≥ `1d` | Hub events, run events, outcomes, tool calls, tool errors |
| `[audit].max_runs` | `50` | ≥ 1 | Runs with telemetry kept per workspace |
| `[audit].trace_max_age` | `30d` | ≥ `1d` | Trace records |
| `[audit].postmortem_max_age` | `180d` | ≥ `1d` | Postmortems |
| `[audit].orphan_retention` | `30d` | ≥ `1d` | Grace period for records of deleted workspaces |
| `[audit].write_timeout` | `2s` | ≥ `100ms` | Limit for one ingestion write |
| `[sessions].max_age` | `90d` | ≥ `1d` | Terminal sessions and their usage records |
| `[sessions].max_active_age` | `7d` | ≥ `1h` | Active sessions older than this are timed out |
| `[webhooks].max_per_workspace` | `20` | ≥ 0 | Event webhook subscriptions a workspace may have. `0` turns event webhooks off: every create answers `429 webhook_limit_reached` |
| `[webhooks].timeout` | `10s` | `1s` to `60s` | Limit for one delivery attempt, from connect to the response status |
| `[webhooks].allow_private` | `false` | bool | Allow destinations on loopback, link-local, private and metadata addresses (AUD-FR-066) |
| `[webhooks].allow_insecure` | `false` | bool | Allow `http://` URLs to loopback addresses. Meant for development |

Fixed, not configurable: heartbeat interval 30 s; per-stream buffer 256 events; idle-write limit 60 s; maximum long-poll 60 s; retention interval 1 h; batch maximum 1000 items; `types` filter maximum 20 entries; event webhook retry schedule (10 s doubling to 1 h, 24 h per event), disabling after 72 h of continuous failure, delivery log retention 7 d.

The workspace setting `audit_fetches` (boolean, default `false`, §4) turns on `parley.git.fetch`.

### 14.10 Edge cases and error handling

- **Event write fails.** The state change and its event share a transaction, so a failed event write fails the request with 500 and nothing is changed. There is no state change without its event.
- **Crash after a ref update.** The owed event is written at the next start (§14.7.1) with the original ref values. Its `time` is the time of writing, so it may be later than the ref update.
- **Consumer falls behind.** The stream is closed (AUD-FR-046). The consumer reconnects with its last `seq` and gets the backlog by replay. Nothing is lost unless the consumer stays away longer than `[events].retention`.
- **Consumer stays away too long.** It gets `410 cursor_expired` with `oldest_seq`. It must decide for itself: resume at `oldest_seq − 1` and rebuild its state from the REST API. The hub never skips silently.
- **Filters that match rarely.** Heartbeats advance the resume position (AUD-FR-045), and `next_after` does the same for polling, so a quiet filter does not expire.
- **Duplicates.** A consumer that stores its position after processing may see the last events again after a crash of its own. Handlers should be idempotent on `seq` or `id`.
- **Access changes while streaming.** Events are checked against the caller's access when delivered. Events written while the caller had no access are not delivered later, also not by replay, if access is still missing at replay time. Replay uses current access, not past access.
- **Deleted workspaces.** Events and sessions of a deleted workspace remain until retention removes them. They are visible to admin tokens; those of a task fork are also visible to readers of its parent.
- **Empty log.** With no events newer than the resume position, the stream sends heartbeats only, and the log answers an empty page. `after` equal to `last_seq` is valid.
- **Both resume inputs.** `Last-Event-ID` wins over `after`. A `Last-Event-ID` that is not a non-negative integer answers 400.
- **Session ids reused.** A session, usage or telemetry `id` identifies one record hub-wide. Reuse in another place is `409 conflict`, never a silent no-op.
- **Replays.** A replay answers 200 with the stored record, even when the replayed body differs. The stored record is never changed by a replay.
- **Different credential, same user.** Session ownership is by user: a session opened with a PAT may be completed with an API key of the same user. Sessions opened by an admin token can be completed only by an admin token.
- **Session on an archived workspace.** New sessions and ingestion are refused (`409 workspace_archived`). Reads keep working.
- **Session named after it ended.** Allowed (§14.3.2). The counts of a terminal session can therefore still grow.
- **Fork expiry and sessions.** Active sessions on the fork are terminated. The session records stay readable by their creator and by readers of the parent, and still list the fork's slug and the changes produced, which live in the parent.
- **Batch with only invalid items.** 200 with `accepted: 0`, `duplicates: 0` and one error per item. Nothing is stored.
- **Write timeout.** 503 with `Retry-After: 5`; a batch is stored completely or not at all, so a retry with the same ids is safe.
- **Audit query filters that exclude a source.** A hub-only filter returns hub events only, an agent-only filter run events only. Combining one of each returns an empty list; combining a filter with the opposite explicit `source` is a 400.
- **`max_runs`.** Removing the telemetry of an old run keeps its postmortem and its sessions, which follow their own limits.
- **Metrics after retention.** A workspace without active sessions disappears from `parley_agent_sessions_active`.
- **Event webhook receiver is slow but answers.** An answer after `[webhooks].timeout` is a failed attempt. The receiver may have processed the event; it sees the same `Parley-Delivery` again and should drop it.
- **Event webhook receiver verifies the timestamp.** Each attempt carries a new `Parley-Timestamp` and a signature over it, so a retry hours later still passes a receiver that refuses old timestamps.
- **Secret rotated while a delivery is retrying.** The next attempt is signed with the new secret. The hub never signs with two secrets; the receiver switches when it gets the new one.
- **Receiver is down for a day.** The first event is retried for 24 hours and then given up; the subscription is `failing`; the hub goes on with the next event, which again gets up to 24 hours. After 72 hours without a success the subscription is `disabled`. The events given up are not sent later. The consumer reads them from the event log by `seq`.
- **`include_forks` and a fork that expires.** `parley.fork.expire` and the events of the fork's last moments have the fork's slug as `workspace` and are delivered to the parent's subscription.
- **The workspace is deleted.** Its subscriptions go in the same step (§4), with whatever they had not delivered. `parley.workspace.delete` of that workspace is therefore not delivered by its own event webhooks. A consumer that must see it follows the stream.
- **The workspace is archived.** Subscriptions stay and keep delivering, for example `parley.workspace.reactivate`. They cannot be created or changed until the workspace is reactivated, and can be deleted.
- **A subscription's own events.** `parley.webhook.*` events of the workspace are sent to its subscriptions like any other event, unless `types` leaves them out. A subscription can therefore receive its own `parley.webhook.failing` once it works again.
- **`types` changed while events wait.** The new filter applies to every event not yet examined. The pending delivery is still sent.
- **The URL resolves to several addresses.** The hub checks the address it connects to. If any attempt would connect to a refused address, that attempt fails.
- **The creator of a subscription leaves.** A subscription belongs to the workspace, not to its creator. It keeps delivering.
- **Hub restarts during an attempt.** The attempt is made again with the same delivery id. The receiver may see the event twice.
- **No encryption key.** Creating a subscription and rotating a secret answer `503 secrets_unavailable`. Existing subscriptions cannot sign, so their attempts fail and are retried until the key is back.
