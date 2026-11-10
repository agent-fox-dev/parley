---
spec_id: "52"
spec_name: "durable_event_log_and_stream"
title: "Record every hub event durably and stream it"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "f67e8d6719d56c8893f3bcdf59bf7933740600b0dcaa383b83c28c6a1fdb1ea1"
schema_version: 3
source: "docs/prd/14_sessions_audit_events.md"
---

# Record every hub event durably and stream it

## Intent

Automation around Parley must be able to react to what the hub does without rescanning state and without ever missing a step. Parley therefore records every significant state change as one hub event, written together with the change it reports, gives each event a hub-wide sequence number that never repeats, and lets a consumer follow the events live or page through them, resume after a disconnect or a restart from the last number it processed, and see exactly the events its access allows.

## Goals

- One event component in `internal/event/` that every other package emits through: an emit call that takes the caller's store transaction, so the event and the state change are committed together or not at all.
- One record shape for every event (`seq`, `id`, `type`, `time`, `workspace`, `actor_type`, `actor_id`, `resource_type`, `resource_id`, `action`, `session_id`, `metadata`), returned unchanged by every way of reading it.
- `seq` starts at 1, rises by one per committed event, survives restarts and is never reused; no event becomes readable while an event with a lower `seq` is still uncommitted.
- One visibility decision, evaluated when an event is delivered, used by the stream and the log and offered to the audit query (package 51) and event webhooks (package 53).
- `GET /events`: a Server-Sent Events stream that replays from a resume position and continues live on the same connection without a gap, sends heartbeats that advance the position, closes a consumer that falls 256 events behind and never drops an event from an open stream.
- `GET /events/log`: the same events as JSON pages in ascending `seq`, with a long-poll of up to 60 seconds.
- A resume position older than the event log answers `410 cursor_expired` with `oldest_seq`; the hub never skips silently.
- `parley events tail` follows the stream, reconnects by itself and prints each event exactly once; `parley events list` reads one page of the log.

## Non-goals

- The individual event types, when each fires, its `resource_type`, `resource_id`, actor and metadata keys (the rows of §14.8): the package that owns each state change (packages 08, 10 to 16, 18, 21 to 30, 31 to 38, 39, 40, 42, 43, 45 to 48, 49 and 53). This package specifies the record contract every row obeys and checks that one event of each `resource_type` family passes through unchanged.
- The journal that remembers a ref update whose event is still owed and writes it at the next start (§14.3.6, §14.7.1): package 18 `git_push_rules`. This package accepts emits from start hooks before the listener is bound.
- The retention run: the hourly schedule, the `event_log` and `hub_events` steps, deleting hub events older than `[audit].max_age`, orphan records, and the upper bound of `[events].retention` against `[audit].max_age` (§14.7.2): package 51 `audit_query_and_retention`. This package delivers the operation that makes events leave the event log, which that step calls.
- `GET /audit` and `parley audit query`: package 51 `audit_query_and_retention`. It reads the stored events and calls this package's visibility decision.
- `GET /metrics` and the metrics `parley_events_seq`, `parley_sse_connections` and `parley_audit_events_total` (§14.7.3): package 51 `audit_query_and_retention`. This package reports the numbers they are built from through `Counters()` and imports nothing of `internal/metrics`; package 51 connects them to its recorder (Design Decision 27).
- Event webhooks: subscriptions, deliveries, `parley.webhook.ping` (AUD-FR-062 to AUD-FR-076): package 53 `event_webhooks`. It reads the event log through this package and uses the workspace-owner form of the visibility decision.
- Sessions and the session-linking rule that decides an event's `session_id` (§14.3.2): package 49 `agent_sessions_and_usage`. This package stores the `session_id` it is given.
- Run telemetry, which is never part of the event log: package 50 `telemetry_ingestion`.
- The error envelope, the order of checks, the rate limit and response headers (PLAT-FR-010 to PLAT-FR-019, PLAT-FR-025): package 02 `http_api_conventions`. Authentication: package 04 `authentication_and_login`. The scope check: package 06 `api_keys_and_access_tokens`.
- Access levels and the readable set: package 07 `workspace_access_rule`. What a token's binding covers and the allowlist that lets a bound token call `/events` and `/events/log`: package 08 `workspace_bound_tokens`.
- The CLI's settings, HTTP client, output and exit codes (PLAT-FR-090 to PLAT-FR-095): package 09 `cli_foundation`.
- The store, its transactions, schema steps, the configuration key registry, start hooks and the shutdown notification: package 03 `server_lifecycle_and_configuration`. The credential scrubber (PLAT-FR-098): the same package.

## Background

The repository holds no code yet; every file named here is new, except `internal/hub/` (package 03), where this package adds its wiring. The source is `docs/prd/14_sessions_audit_events.md`: §14.3.5 to §14.3.8, AUD-FR-040 to AUD-FR-049, the routes `GET /events` and `GET /events/log` of §14.5, the commands `parley events tail` and `parley events list` of §14.6, the start-up and shutdown rules of §14.7.1, the head of the catalogue §14.8, the `[events]` keys of §14.9 and the event-log edge cases of §14.10 (user stories AUD-US-06 and AUD-US-07).

Almost every other package emits events and names this package as the place they go. The code lives in `internal/event/`:

| File | Holds |
|---|---|
| `internal/event/record.go` | the event record, its JSON form, input validation, the path-list helper |
| `internal/event/schema.go` | the schema step: the event table, the `seq` counter, the log's lower bound |
| `internal/event/emit.go` | the emit calls, `seq` assignment, the counters |
| `internal/event/notify.go` | the commit notification that wakes streams and long-polls |
| `internal/event/log.go` | reading the log by position, the log's bounds, the expire operation |
| `internal/event/visibility.go` | the visibility decision |
| `internal/event/filter.go` | filters and resume positions |
| `internal/event/stream.go` | `GET /events` |
| `internal/event/poll.go` | `GET /events/log` |
| `internal/event/config.go` | the `[events]` configuration keys |
| `internal/cli/events.go` | `parley events tail` and `parley events list` |

Tests live beside the code (`internal/event/emit_test.go`, `record_test.go`, `log_test.go`, `visibility_test.go`, `filter_test.go`, `poll_test.go`, `stream_test.go`, `config_test.go`, `internal/cli/events_test.go`). `internal/hub/events.go` registers the schema step, the configuration keys, the two routes and the shutdown behaviour, with `internal/hub/events_test.go` beside it. The end-to-end flows are proved in `test/e2e/events_test.go`. Until the packages that emit land, no production route writes an event, so those tests add one test route to an in-process hub that changes a row and emits in one transaction.

## Requirements

### The event record (§14.3.5, §14.3.1)

Every hub event has the same twelve fields, serialised as one JSON object with the members in this order:

| Field | Type | Meaning |
|---|---|---|
| `seq` | integer | Hub-wide sequence number. Strictly increasing, never reused, never reset |
| `id` | UUID | Stable identity of the event, a version 4 UUID in lower case, generated by the hub |
| `type` | string | For example `parley.change.land`. Every type starts with `parley.` |
| `time` | timestamp | When the event was written: RFC 3339, UTC, whole seconds |
| `workspace` | string or null | Slug of the workspace the event belongs to. Null for an event that is not workspace-scoped |
| `actor_type` | enum | `admin_token`, `api_key`, `pat`, `forge`, `system` |
| `actor_id` | string or null | User id of the actor. Null for `admin_token`, `forge` and `system` |
| `resource_type` | string | Kind of the resource. Never empty |
| `resource_id` | string | Identity of that resource inside its workspace. Never empty |
| `action` | string | The last dot-separated segment of `type` (`parley.change.land` has action `land`; `parley.forge.issue.create` has `create`) |
| `session_id` | UUID or null | The session the change is attributed to, in lower case |
| `metadata` | object | Type-specific keys. `{}` when none |

Absent values are `null`, never omitted. Metadata is stored and returned as JSON, never as a string holding JSON. The record is the same bytes wherever it is read: in a `hub_event` frame of the stream, in `items` of the log, and (in packages 51 and 53) in the audit query and a webhook body.

The emitting package supplies `type`, `workspace`, `actor_type`, `actor_id`, `resource_type`, `resource_id`, `session_id` and `metadata`. The component assigns `seq`, `id` and `time` and derives `action`. An event of each `resource_type` family of the catalogue (`workspace`, `branch`, `session`, `token`, `ref`, `commit`, `change`, `policy`, `claim`, `patch`, `rebuild`, `secret`, `variable`, `issue`, `comment`, `label`, `pull_request`, `webhook`, `sandbox`, `sandbox_rule`, `event_webhook`) is stored and returned with the supplied fields unchanged.

An emit is refused with an invalid-event error that names the field, and writes nothing, when: `type` does not start with `parley.`, has fewer than three segments or has an empty segment; `resource_type` or `resource_id` is empty; `actor_type` is outside the five values; `actor_id` is set for `admin_token`, `forge` or `system`, or empty for `api_key` or `pat`; `session_id` is not a UUID in the 8-4-4-4-12 form; `workspace` is an empty string (null is the way to say "none"); or the metadata is not a JSON object.

Rules for metadata that the component enforces:

- A list of paths holds at most 100 entries. Emitting packages build such a list with the component's path-list helper: given more than 100 paths it keeps the first 100 in the given order and the metadata also gets `truncated: true`; given 100 or fewer it keeps all and adds no `truncated` member.
- Every string in the metadata, at any depth, passes through the credential scrubber of package 03 before it is stored (NFR-021): user information is removed from URLs and known secret values become `[redacted]`.

What the metadata of a given type holds, and that it never holds a credential or free text from a forge message, is the emitting package's rule.

### Writing an event with its state change (§14.3.6, AUD-FR-040, AUD-FR-041, §14.10)

**In the caller's transaction.** The emit call takes the store transaction in which the caller makes its state change. When that transaction commits, exactly one event exists for the call, with the next `seq`. When the transaction rolls back (its function returns an error or panics), no event exists, and the next committed event gets the `seq` that follows the last committed one. There is no state change without its event and no event without its state change.

**A failed event write fails the request.** When the event cannot be written, the emit call returns an error; the caller returns it from its transaction, nothing is changed, and a REST route answers `500` with `error_type` `internal`.

**Without a state change in the store.** For an event whose state change does not live in the hub's store (a served fetch, a refused bound token, a ref update already made), a second emit call takes no transaction and writes the event in a transaction of its own. Start hooks may call both forms before the listener is bound; the event's `time` is the time of writing.

**`seq`.** The first event of a hub has `seq` 1. Each committed event has the `seq` of the previous one plus 1. Several events emitted in one transaction get consecutive `seq` values in the order of the calls. After a restart the next `seq` is the highest ever assigned plus 1, also when the events that carried the highest values are no longer stored. The hub never hands out a `seq` lower than or equal to one it has already delivered.

**Ordered.** Events are delivered in `seq` order. An event never becomes readable after an event with a higher `seq` has been delivered to any consumer: with any number of transactions emitting at the same time, every reader sees the committed events as a prefix of the `seq` order that only grows.

**Live delivery.** A committed event is offered to open streams and held long-polls within 1 second of its commit (§15.4).

**Counters.** The component reports, on request, the last `seq` written, the number of open streams, and the number of events written since start per type, for the metrics of package 51.

### The event log and its bounds (§14.3.6, AUD-FR-043, §14.7.2)

Events are stored once. The **event log** is the part addressable by `seq`: it is bounded by `oldest_seq` and `last_seq`. `last_seq` is the last `seq` written (0 on a hub that has written none). `oldest_seq` is the lowest `seq` still in the log (0 on a hub that has written none, 1 until something has expired).

The component offers one operation that makes events leave the log: given a cutoff time, every event older than the cutoff leaves the event log, except that the newest event always stays, whatever its age. `oldest_seq` moves forward to the lowest `seq` that stayed. Events that left the log stay stored for the audit query until package 51 deletes them; they are no longer served by `GET /events` or `GET /events/log`. Package 51 calls the operation from its retention run with `now − [events].retention`.

### Visibility (§14.3.7, §14.3.8, §14.10)

One rule decides who may see an event, on the stream and on the log:

| Event | Visible to |
|---|---|
| `workspace` is set | Callers with read access to that workspace (level `admin`, `owner` or `member`). For a task fork, also callers with read access to its parent; this still holds after the fork is deleted. The next row is the exception |
| `workspace` is set and the type is `parley.secret.*`, `parley.token.*` or `parley.webhook.*` | Only callers with level `owner` or `admin` on that workspace (for a task fork: on the fork or on its parent) |
| `workspace` is null, `metadata.scope` is `user` | The user named in `metadata.scope_id` |
| `workspace` is null, `metadata.scope` is `org` | Members of the org named in `metadata.scope_id` |
| Any event | Admin tokens |

An event with a null `workspace` and no `metadata.scope` of `user` or `org` is visible to admin tokens only. Events of a deleted workspace are visible to admin tokens; those of a deleted task fork are also visible to readers of its parent.

**Bound tokens.** A token bound to a workspace sees only the events of the workspaces its binding covers, under the rows above. A token bound to a task fork also sees the events of the fork's parent whose `resource_type` is `change` and whose change has that fork as its source. A bound token never sees an event without a workspace.

**The workspace-owner form.** For event webhooks (package 53) the decision can be asked for "an owner of workspace W": such a caller sees the events of W and of W's task forks, the owner-only types included, and no other event.

**Evaluated at delivery.** Access is evaluated when an event is delivered, not when the stream was opened or the event was written. A workspace or fork created later is included from its first event. A caller who loses access stops receiving events of that workspace, including earlier ones on a replay. A caller who gains access sees earlier events on a replay. Replay uses current access, not past access.

**Failure.** When the access needed for the decision cannot be read, the event is not delivered: `GET /events/log` answers `500` with `error_type` `internal`, and an open stream is closed without sending the event, so the consumer resumes from its last position.

### Access to the two routes (§14.3.8)

`GET /events` and `GET /events/log` need an authenticated caller. A PAT needs `audit:read` or `audit:write`; without either it gets `403` with `error_type` `insufficient_scope`, before any parameter is validated. The admin token and API keys are not scope-checked. A workspace-bound token may call both routes; its results are confined as the visibility rule says.

### Filters (AUD-FR-042)

The stream and the log accept the same filters:

- `workspace`: one slug, exact match.
- `types`: a comma-separated list of at most 20 entries. An entry is an exact type or a prefix ending in `*` (`parley.change.*`). `*` alone matches everything. `*` is allowed only as the last character of an entry.
- `session_id`: one UUID, in any letter case.

Filters combine with AND; the entries of `types` combine with OR. A type that no event has is not an error: it matches nothing.

Refusals, all `400` with `error_type` `invalid_request`: more than 20 entries in `types`; an empty entry (including an empty `types` value); an entry with `*` anywhere but last; a `session_id` that is not a UUID.

A `workspace` filter that names a workspace the caller cannot read, that does not exist, or that a bound token's binding does not cover answers `404` with `error_type` `not_found` and the message `workspace not found`.

### Resume positions (AUD-FR-043, §14.10)

A resume position is a `seq`: the consumer receives events with a higher `seq`. On `GET /events` it is the query parameter `after` or the header `Last-Event-ID`; the header wins when both are present. On `GET /events/log` it is `after`, or `cursor` as another name for it.

- A position that is not a non-negative integer answers `400 invalid_request`.
- A position ahead of `last_seq` answers `400 invalid_request`. A position equal to `last_seq` is valid.
- A position is expired when at least one event after it has left the event log, whether or not the caller could have seen that event: that is, when it is lower than `oldest_seq − 1`. It answers `410` with `error_type` `cursor_expired`, the message `event cursor expired` and `oldest_seq` as a member of the error object:

  ```json
  {"error": {"code": 410, "message": "event cursor expired", "error_type": "cursor_expired", "oldest_seq": 31007}}
  ```

  To continue, a consumer resumes with `after = oldest_seq − 1`, which is valid, and accepts that it has missed events.

All these answers are given before a stream starts and are JSON in the error envelope.

### `GET /events` (AUD-FR-044 to AUD-FR-048, §14.5, §14.7.1)

The answer is `200` with `Content-Type: text/event-stream` and `Cache-Control: no-cache`. The stream holds two kinds of frame, each ended by an empty line; `data` is always one line of JSON:

```
id: 48213
event: hub_event
data: {"seq":48213,"id":"…","type":"parley.change.land","time":"2026-01-02T03:04:05Z","workspace":"api","actor_type":"pat","actor_id":"u_1","resource_type":"change","resource_id":"42","action":"land","session_id":null,"metadata":{}}

id: 48290
event: heartbeat
data: {"time":"2026-01-02T03:04:35Z","seq":48290}
```

- **Replay, then live.** With a resume position the stream first sends every visible, matching event after the position, in ascending `seq`, then continues with live events on the same connection. There is no gap, no reordering and no repeated event between the two phases. Without a resume position the stream starts with live events only: events written after it opened.
- **Heartbeats.** The stream sends a heartbeat when it opens and then every 30 seconds. A heartbeat's `seq`, which is also its SSE `id`, is the position up to which the log has been examined for this client: every visible, matching event up to it has already been sent. A client whose filters match rarely therefore still advances its resume position and does not run into `cursor_expired`.
- **Slow consumers.** Each stream has a buffer of 256 undelivered events. When the buffer is full, or when the client accepts no data for 60 seconds, the hub closes the stream. It never drops an event from an open stream. The client reconnects with its last `seq` and receives everything it missed.
- **Connection limit.** At most `[events].max_sse_connections` streams are open at once. A further request answers `503` with `error_type` `unavailable` in the error envelope and the header `Retry-After: 5`. A stream that ends frees its place. Long-polls on `GET /events/log` do not count.
- **Credential.** The hub closes a stream when its credential is revoked or expires, at most 30 seconds later.
- **Shutdown.** At shutdown the hub closes all streams at once and does not wait for them. Consumers resume from their last `seq` after the restart.

### `GET /events/log` (AUD-FR-049, §14.5, §14.7.1)

Query: `after` (when absent, the page starts at the oldest event in the log), `cursor` (another name for `after`), `workspace`, `types`, `session_id`, `limit` (1 to 200, default 50) and `wait` (whole seconds, 0 to 60, default 0).

The answer is `200` with the visible, matching events with `seq > after` in ascending `seq`:

```json
{"items": [ <hub event>, … ], "next_cursor": "48213", "next_after": 48213, "oldest_seq": 31007, "last_seq": 48290}
```

- `items` is always an array.
- `next_after` is always present. When the page is full it is the `seq` of the last item. When the page is not full it is the position up to which the log was examined, which may be higher than the last item's `seq`. Passing it as `after` in the next request never skips and never repeats an event, and keeps a quiet filter from expiring.
- `next_cursor` is `next_after` as a string when the page was full, else null.
- `oldest_seq` and `last_seq` bound the event log at the time of the answer.

**Long-poll.** When there are no events to return and `wait` is above 0, the request is held until a visible, matching event is written or the wait ends, then answers `200`: with that event, or with no items and `next_after` moved to the position examined. A held request is released at once when the hub shuts down and answers `200` with no items.

Refusals, `400 invalid_request`: a `limit` outside 1 to 200 or not an integer; a `wait` outside 0 to 60 or not a whole number; `after` and `cursor` both present with different values; the filter and position refusals above. `404` and `410` as above.

### Configuration (§14.9)

| Key | Default | Valid | Effect |
|---|---|---|---|
| `[events].retention` | `30d` | a duration (`s`, `m`, `h`, `d`) of at least `1h` | How long an event stays in the event log and can be resumed from |
| `[events].max_sse_connections` | `500` | an integer of at least 1 | Open event streams allowed at once |

A malformed or out-of-range value stops the hub at start with exit status 1 and a message that names the key. Fixed, not configurable: heartbeat interval 30 s; per-stream buffer 256 events; idle-write limit 60 s; maximum long-poll 60 s; `types` filter maximum 20 entries.

### `parley events tail` (§14.6)

Follows the event stream. Flags: `--workspace <slug>`, `--types <list>`, `--session <id>`, `--after <seq>`.

- Prints one hub event per line on stdout as compact JSON (not indented), exactly the `data` of the frame, so the output can be piped line by line. Heartbeats are not printed.
- Without `--after` it starts with live events. With `--after` it replays first.
- Runs until interrupted, without a request timeout. An interrupt exits 0.
- When the stream ends for any reason other than an interrupt (hub restart, slow-consumer close, network error, `503`, any other answer not listed below), the command reconnects by itself, waiting 1 s at first and doubling up to 30 s, and resumes from the last position it received. No event is lost and none is printed twice.
- It stops with exit status 1 on an error that a retry cannot fix: `401`, `403`, `404`, `400` and `410 cursor_expired`. The hub's error envelope is printed on stderr as received; on `cursor_expired` it includes `oldest_seq`. The command never skips events silently.
- A `--after` value that is not a non-negative integer is a usage error (exit status 2) before any request.

### `parley events list` (§14.6)

Calls `GET /events/log` once and prints the response on stdout as one JSON document. Flags: `--workspace <slug>`, `--types <list>`, `--session <id>`, `--after <seq>`, `--limit <n>`, `--wait <seconds>`. The request timeout is 30 seconds extended by the wait. A hub error is printed on stderr as received (with `oldest_seq` on `410`), nothing is printed on stdout and the exit status is 1.

### End-to-end flows

1. **Durable live feed (AUD-US-06).** A review agent opens `GET /events?types=parley.change.*` with a PAT that holds `audit:read`. `parleyd` answers with a heartbeat. A developer's request changes state; `parleyd` commits the change and its event in one transaction and sends the event on the stream. The review agent's connection drops; two more events are written. The review agent reconnects with `Last-Event-ID` set to the last `seq` it saw; `parleyd` replays the two events in order and continues live. No event is missing and none is repeated.
2. **Polling consumer and an expired position (AUD-US-07).** An agent calls `GET /events/log?wait=30` with `after` set to `last_seq`. `parleyd` holds the request; an event is committed; `parleyd` answers at once with it and a `next_after`. The agent stays away; an operator's retention run makes old events leave the log. The agent comes back with its old position; `parleyd` answers `410 cursor_expired` with `oldest_seq`. The agent resumes with `after = oldest_seq − 1` and gets the events still in the log.
3. **Tailing from the shell.** A developer runs `parley events tail --workspace api`. The parley CLI opens the stream; `parleyd` sends events; the CLI prints one line each. `parleyd` is restarted by the operator; the stream ends; the CLI waits, reconnects with the last position it received and prints the events written meanwhile once each. The developer interrupts; the CLI exits 0.

## Design Decisions

1. **Profile: full**, with three execution paths.
2. **Project decisions relied on.** Go; the transactional store is embedded SQLite through `modernc.org/sqlite` behind `internal/store`; HTTP is the standard library `net/http`; the CLI uses `github.com/spf13/cobra` through the command helper of package 09; tests use the standard `testing` package, table-driven, with `net/http/httptest`; code takes a replaceable clock so tests control time, here also for the 30-second heartbeat, the 60-second idle limit and the long-poll wait.
3. **Events live in the transactional store.** They must commit with the state change they report (NFR-012), so the event table is in the same database file as the state. The audit query of package 51 reads that table.
4. **`seq` comes from a counter row, assigned inside the write transaction.** The counter is separate from the event rows, so deleting events never lowers it, and a rolled-back transaction gives its number back. Because the store admits one write transaction at a time, commit order is `seq` order, which is the "ordered" guarantee. Why: the PRD demands "never reused, never reset" and "never visible after a higher `seq` was delivered", and names no mechanism.
5. **Two emit calls.** One takes the caller's transaction; one takes none and opens its own. Why: several catalogue rows report something that is not a store change, and package 08 calls exactly these two through the interface `EventEmitter` it declares in `internal/access` (emit in a transaction, emit now). `internal/event` imports `internal/access` for the workspace record and the access rule of package 07, so `internal/access` cannot import `internal/event` back: the component that `internal/hub/events.go` builds is installed behind `access.EventEmitter` by the hub's composition, which passes it to the guard and the minting component of package 08 (in that package's `internal/hub/access.go`, because package 08 is built after this one). The two emit calls keep the method set that interface names, so no adapter is needed.
6. **The owed-event journal is not here.** Package 18 keeps it and emits through this component at the next start. This package only guarantees that an emit works from a start hook.
7. **The fork's parent is stored with the event.** At emit time the component reads the workspace record in the same transaction and stores the parent's slug beside the event when the workspace is a task fork; the caller may also pass the parent explicitly (needed when it deletes the fork's record before emitting). The parent is not part of the record consumers see. Why: visibility for readers of the parent must hold after the fork is deleted.
8. **Visibility is by slug at delivery time.** A slug that names no workspace gives level none, so events of a deleted repository workspace are seen by admin tokens only. If a new workspace is later created under the slug of a deleted one, its readers see the old workspace's events that are still stored; the PRD evaluates access at delivery and says nothing else.
9. **Org-scoped events use the same membership question as the access rule** (package 07): the user is a member of the named org while that org is `active`.
10. **An event without a workspace and without `metadata.scope`** is visible to admin tokens only. The PRD's table has no row for it.
11. **The visibility rule gets the fork-parent lookup, the covered set and the change lookup by injection, through narrow interfaces declared in `internal/event`.** The plan gives this package the dependencies 03 and 07 only, and the packages that own the other two (08, 22) emit events and so import `internal/event`, or are imported by it: importing them here would be a Go import cycle. `internal/hub/events.go` passes the fork-parent lookup (the parent slug of a task fork, also inside a given transaction for the emit of decision 7), filled with the workspace record of package 07; `internal/hub` passes the covered set of package 08 and the change lookup of package 22 when those packages exist. So `internal/event` imports no package that imports it, and `internal/access` reaches this component only behind `access.EventEmitter` (decision 5). Without a covered set a bound PAT sees no event and every `workspace` filter answers `404`; without a change lookup a fork-bound token sees no event of its parent.
12. **The workspace-owner form of the decision** exists for package 53, which the PRD says is "treated as an owner of its workspace".
13. **The path-list helper.** The component cannot tell which metadata member is a list of paths, so the 100-entry rule is delivered as a helper the emitting packages call. Other lists in metadata (branch names, token ids) are not cut.
14. **Scrubbing metadata strings** uses the one scrubber of package 03, so a metadata string is also cut to 1024 bytes, as that package specifies for text that leaves the hub in events.
15. **The emit call does not check that `session_id` names a session.** The session-linking rule is package 49's and runs in the emitting package.
16. **Type shape.** A type has at least three dot-separated, non-empty segments beginning with `parley`. The catalogue has three-segment and four-segment types (`parley.forge.issue.create`).
17. **Bounds of an empty log.** `oldest_seq` and `last_seq` are 0 when no event was ever written; `after=0` is then valid and any higher position is `400`.
18. **The expire operation is here, its schedule in package 51.** Cursor expiry cannot be specified or tested without a way for events to leave the log. The operation works by position: `oldest_seq` becomes the lowest `seq` whose `time` is not older than the cutoff, or `last_seq` when every event is older. It never moves backwards.
19. **`[events].retention` is registered here with its lower bound only.** Its upper bound is `[audit].max_age`, a key package 51 owns; package 51 adds the cross-key check that refuses the start when `[events].retention` exceeds `[audit].max_age`, and reads the loaded value through `Retention()`.
20. **The `workspace` filter for the admin token is not checked against the registry.** The admin token may read every workspace, and this lets an operator read the events of a deleted workspace by slug. Every other caller gets `404` for a slug the access rule answers "not found" for, a deleted fork included.
21. **`after` and `cursor` together.** Equal values are accepted; different values are `400 invalid_request`. The PRD says only that `cursor` is another name.
22. **`/events/log` does not use the opaque cursor of PLAT-FR-014.** Its `next_cursor` is a `seq` as a string, as §14.5 states; the list helper of package 02 supplies only the `limit` rule.
23. **A stream's credential is checked again at every heartbeat**, so a revoked or expired credential closes its stream at most 30 seconds later. A credential that no longer authenticates for another reason (a blocked user) closes it the same way. The PRD gives no bound.
24. **Unknown query parameters are ignored**, as unknown body members are (PLAT-FR-017).
25. **`parley events tail` details.** The resume position is the `id` of the last frame received, heartbeats included, so a quiet filter does not expire across a reconnect; an event is printed only when its `seq` is higher than the last one printed. The wait returns to 1 s after a connection that delivered at least one frame. `429` and every `5xx` are retried; the five answers the PRD lists stop the command. Progress about reconnects goes to stderr.
26. **A long-poll released by shutdown answers `200` with no items.** The PRD says "released at once" and names no status.
27. **Exported names, stated once.** Other packages use the component of `internal/event` by these names and no others: `Emit` (the emit call in the caller's store transaction), `EmitNow` (the emit call that opens its own transaction), `ReadAfter(position, limit)` (the in-process read of the event log after a sequence number, used by package 53 for delivery and by package 30), `Bounds` (`oldest_seq` and `last_seq`), `Expire(cutoff)` (the expire operation that package 51's `event_log` step calls), `Visible` and `VisibleToOwnerOf` (the visibility decision and its workspace-owner form), `OnCommit` (the registration for the in-process notification of a committed event, which package 53's delivery loops use), `Retention()` (the loaded `[events].retention`) and `Counters()` (`last_seq`, `open_streams`, `written_by_type`). Package 51, which is built after this one, registers sources with its metrics registry that read `Counters()`: `last_seq` for `parley_events_seq`, `open_streams` for `parley_sse_connections` and `written_by_type` for the `source` `hub` series of `parley_audit_events_total`. Deleting stored hub events (the `hub_events` and `orphans` steps) is added by package 51 beside this store in `internal/event/retention.go`.

## Dependencies

- Package 03 `server_lifecycle_and_configuration` (draft): the transactional store and its transactions, schema steps, the configuration key registry, start hooks, the shutdown notification and the credential scrubber.
- Package 07 `workspace_access_rule` (draft): the workspace record (kind, parent), access levels, the readable set and the membership question the visibility rule uses. Through it: package 02 (router, error envelope, check order), package 04 (the authenticated caller), package 05 (organizations and memberships) and package 06 (the scope check and the scopes `audit:read`, `audit:write`).
- Package 09 `cli_foundation` (draft): the command helper, HTTP client, output and exit codes that `parley events tail` and `parley events list` are built on. Added to the plan's list by the brief's rule for packages with CLI commands.
- Not dependencies, wired by `internal/hub` when present: the covered set of package 08 and the change lookup of package 22 (decision 11). Package 08 depends on this package and declares `EventEmitter` in `internal/access`; the hub installs this package's component behind it (decision 5).

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires.
- **Change**: The unit of work: a proposal to add the commits of a source branch to a target branch, with a number, a title, an intent, versions and a state.
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
- **Bound token**: A PAT confined to one workspace (and the forks created with it). A token bound to a fork also reaches its own changes, and the awareness and policy reads, on the fork's parent (§3).
- **Session**: An agent session: who ran, on which workspace, with what token usage, and which changes it produced.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
- **Event webhook**: A workspace's subscription that makes the hub send each matching hub event to a URL as a signed HTTP `POST`, in order and with retries (§14).
- **Event log**: The part of the stored hub events addressable by `seq`: events newer than `[events].retention`, bounded by `oldest_seq` and `last_seq`.
- **Resume position**: A `seq`; a consumer that gives it receives the events with a higher `seq`.
- **Heartbeat**: A stream frame that carries no event and tells the client up to which `seq` the log has been examined for it.
- **Level**: What a caller is on one workspace: `admin`, `owner`, `member` or none (package 07).
