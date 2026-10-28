---
spec_id: "12"
spec_name: "audit_and_events"
title: "Record what happened and let it be queried and streamed"
status: "active"
created_at: "2026-10-27T19:01:45Z"
updated_at: "2026-10-28T13:06:04Z"
intent_hash: "58ea00925ebb37a555870d27e13cb443dc2045935db0c24e33b23f7ecf12a69b"
schema_version: 2
source: "docs/prd/01-port-af-hub-to-typescript-on-cloudflare.md"
---

# Record what happened and let it be queried and streamed

## Intent

Every package before this one does something worth remembering, and until
now none of them remembered it. This is where the service gets its memory:
the hub's own events for everything it did, the per-run records agents send
about what they did, one query across both, and a live stream for anything
watching. It is the answer to "what did it do" that the service promises,
and it is the last thing anyone thinks about until they need it, which is
why it is specified as a contract rather than as logging.

## Goals

- Every operation the two tables of `docs/prd/api.md` § Hub Audit Events
  names records its event, with the specified type, actor, resource, action
  and metadata keys and no others.
- The per-run ingestion routes accept events, session outcomes, tool calls,
  tool errors, traces and a postmortem, idempotently by id, with the
  owner-only access rule and the batch result the reference specifies.
- `GET /api/v1/audit` interleaves hub and agent events correctly, and
  `GET /api/v1/events` streams them with the documented heartbeat, timeout,
  buffer and limit.
- The transcript rebuilds a node's conversation from its traces.
- Recording an event never changes the answer of the request that caused it.

## Non-goals

- **Emitting the events of other packages.** This package provides the
  recorder and adds the recording calls to the routes that earlier packages
  left ready for it; it does not change what those routes do.
- **Retention beyond what the reference states.** The hourly prune of
  webhook deliveries is `14_forge_webhooks`; this package keeps what the
  reference says it keeps and no policy beyond it.
- **Metrics.** `afhub_audit_events_total` and the audit table gauges are
  `01_service_foundation`'s registry; this package feeds them.

## Background

Earlier packages were written with their events named and deliberately left
unrecorded: `03_workspaces` for the workspace and bound-token events,
`07_git_server` for the push event, `09_carry_patch` for the patch and
rebuild events, `11_sessions_and_cost` for the force-closed session. Each
left a documented place for this package.

Three parts of `api.md` are easy to get subtly wrong. The **ingestion access
rule** checks the workspace, then that it is **not archived** (`409`,
answered *before* ownership is considered), then that the caller **owns** it
(`403 workspace_access_denied`, not `404`) — the opposite order and the
opposite answer from most of the service. The **unified query** mixes two
sources whose timestamps must be compared as instants so they interleave
correctly, and giving a hub-only filter silently excludes agent events,
which is deliberate. And the **stream** fixes its visible workspace set when
it opens, so a membership change takes effect only on the next connection.

Cursor pagination exists: `11_sessions_and_cost` built it, and this package
reuses it rather than writing another.

## Requirements

### Recording the hub's own events

- Every operation the two tables of § Hub Audit Events name records one
  event with that table's type, actor, workspace, resource type and id,
  action and metadata keys, and no other metadata.
- Recording an event never changes the answer of the request that caused it:
  a failure to record is not a failure of the request.
- A bound token's refusal is recorded at most once per token per minute,
  with the refusals held back since the token's previous event counted.
- Events that belong to no workspace are returned to admin tokens only.

### Ingesting what an agent did

- The per-run routes accept one audit event, one session outcome, one tool
  call, one tool error or one trace event, and answer `201` with the stored
  record's identity.
- A record whose id already exists answers `200` and changes nothing.
- A run id is a date, a time and six lowercase hex characters; anything else
  is `400`. Client timestamps are RFC 3339 and a malformed one is `400`.
- The ingestion routes need `audit:write` and the workspace's **owner**:
  they check that the workspace exists, then that it is not archived
  (`409 workspace_archived`, before ownership is considered), then that the
  caller owns it (`403 workspace_access_denied`). A refused request stores
  nothing.
- A batch accepts 1 to 1000 items and answers with the number accepted, the
  number of duplicates and an error per failing item, each item succeeding
  or failing on its own. An empty array is `400` and more than 1000 items is
  `413`.
- A run has at most one postmortem: a second submit answers `200` with the
  existing one. A missing field or an invalid status or time is `400`, and a
  schema version other than 1 is `422 unknown_schema_version`.

### Reading what an agent did

- Each per-run list returns its records with the documented filters and
  cursor paging, and needs `audit:read` and read access, whatever the
  workspace's status.
- An invalid run id, cursor or time is `400`.
- The postmortem is read, and a run without one answers `404
  postmortem_not_found`.

### Querying across both sources

- `GET /api/v1/audit` returns hub and agent events together, newest first,
  comparing the two sources' times as instants so they interleave correctly.
- The filters narrow the result; a hub-only filter leaves agent events out
  and a run filter leaves hub events out.
- A caller sees the events of every workspace they own or share an
  organization with, whatever its status; events belonging to no workspace
  go to admin tokens only.
- An invalid cursor or time, or a start that is not before the end, is
  `400`; a workspace the caller may not read is `404`.

### The transcript

- The transcript rebuilds one node's conversation from its trace events,
  mapping the trace types to roles and leaving other types out.
- A request without a node is `400`.

### Streaming events live

- `GET /api/v1/events` streams server-sent events, with a heartbeat at once
  and then every 30 seconds.
- A connection with no reads for 60 seconds is closed. Each client buffers
  256 events and the oldest is dropped when it is full.
- The workspaces a stream may see are fixed when it opens, so joining or
  leaving an organization takes effect on the next connection.
- An event recorded by any component of the deployment reaches a subscriber,
  not only one recorded by the component serving the stream.
- A workspace the caller may not read is `404` before the stream starts, and
  the configured limit of concurrent streams is `503`.

### End-to-end flows

1. **A hub operation is remembered.** An API client archives a workspace;
   the archive succeeds and one `hub.workspace.archive` event is recorded
   with the caller as actor. The client then finds it through the unified
   query, filtered to that workspace.
2. **An agent reports a run and it is read back.** An agent ingests a batch
   of trace events and a postmortem for a run; some ids repeat an earlier
   batch and are counted as duplicates. A client lists the run's traces,
   reads its postmortem, and reads the transcript of one node rebuilt from
   those traces.
3. **A watcher sees an event as it happens.** A client opens the event
   stream for a workspace and receives a heartbeat at once; another
   component of the deployment then records an event for that workspace, and
   the watcher receives it without reconnecting.

## Design Decisions

1. **One recorder, called from the routes that own the operation.** The
   events are not derived from logs or inferred from state changes; each
   route records its own, which is what lets the metadata be exactly the
   keys the reference lists.
2. **A failure to record is not a failure of the request.** The reference
   says recording never changes the answer, so the recorder cannot be on the
   request's critical path in a way that lets it fail the request.
3. **The ingestion access rule is written out in full**, because its order
   and its answers are the reverse of the rest of the service and would
   otherwise be "corrected" to match.
4. **The stream's fan-out is a platform concern.** An event recorded by one
   component must reach a stream served by another, so the event bus is a
   platform interface with two implementations, following `01`'s pattern.

## Dependencies

- `01_service_foundation` for the router, the envelope, the metrics registry
  this package feeds, and the platform interface pattern the event bus
  follows.
- `02_identity_and_credentials` for the actor recorded on an event.
- `03_workspaces` for the access decision and the workspace events it left
  ready to record.
- `11_sessions_and_cost` for the cursor pagination this package reuses.
- The route packages whose events this package records: `03`, `04`, `07`,
  `08`, `09`, `10`, `11`, and later `13` and `14`.
- `docs/prd/api.md` § Hub Audit Events, § Audit Ingestion Endpoints and
  § Audit Query Endpoints are normative.

## Verified External API

Nothing new. Server-sent events are produced directly from the platform's
streaming response, and the event bus is a platform interface rather than a
library; both are verified by this package's own tests against the two
implementations.
