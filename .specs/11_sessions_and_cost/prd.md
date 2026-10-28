---
spec_id: "11"
spec_name: "sessions_and_cost"
title: "Record agent sessions and report what they cost"
status: "active"
created_at: "2026-10-27T18:58:10Z"
updated_at: "2026-10-28T11:40:10Z"
intent_hash: "64fda8baa77b5e27e4f9b319ba419f534b70773392f15387b4394cb16602fe9f"
schema_version: 2
source: "docs/prd/01-port-af-hub-to-typescript-on-cloudflare.md"
---

# Record agent sessions and report what they cost

## Intent

"What did it do, and what did it cost?" is one of the questions the service
exists to answer, and it cannot be answered after the fact: the tokens an
agent spends are gone unless something counted them while it ran. A session
is that record — one agent run against one workspace, with the credential
that opened it, how it ended, and the tokens it consumed — and the cost
routes are the aggregate a person actually reads.

## Goals

- Sessions open, complete and report usage as `docs/prd/api.md` § Agent
  Session Endpoints specifies, with the documented access rules and the
  idempotent open.
- Sessions, their usage and a workspace's cost are read with the documented
  filters, ordering, limits and totals.
- Cursor pagination exists, with the contract `api.md` § Conventions fixes,
  and is what every later paged route uses.
- Archiving or deleting a workspace ends its active sessions.

## Non-goals

- **Audit ingestion.** The per-run events, tool calls, traces and postmortem
  are `12_audit_and_events`; a session is a cost record, not an audit trail.
- **Audit events.** `hub.session.force_closed` is `12_audit_and_events`,
  which adds the recording to the ending this package performs.
- **Pricing.** The routes report tokens, not money; `api.md` defines no
  currency and this package invents none.

## Background

Cursor pagination was deliberately left out of `01_service_foundation`: no
route there paged, so the facility would have had no caller in production
code. This is the first package with paged routes, so it builds it, to the
contract `api.md` § Conventions fixes, for `12_audit_and_events` and any
later package to reuse.

Two access rules here differ from everywhere else and are easy to flatten.
Opening a session needs **write** access, so a member of the workspace's
organization is refused. And session and cost **reads** need read access
*and an active workspace*: a member of an archived workspace gets `403
workspace access denied`, not the `404` the workspace routes would give.
Completing a session and reporting its usage are for the session's **creator
or an admin token** alone — not the workspace's owner — so a second agent
cannot close the first one's session.

## Requirements

### Cursor pagination

- A page response carries `next_cursor`, null exactly on the last page, and
  `has_more`.
- Passing a response's `next_cursor` back as `cursor` returns the rows that
  follow, and reading pages to the end yields every row exactly once in the
  requested order.
- A cursor that is malformed, truncated or not one this service issued is
  `400`, and no rows are returned.

### Opening a session

- An open records the workspace, the run, the node, the archetype, the model
  and arbitrary metadata, and answers `201` with the session, its credential
  and its start time.
- An open repeating an `id` that exists answers `200` with the existing
  session, when the caller may read its workspace.
- It needs write access to the workspace, whatever the workspace's status; a
  missing `workspace_slug` is `400` and a workspace the caller may not see,
  or a replayed id belonging to one, is `404`.

### Completing a session and reporting its usage

- A complete closes an active session with `completed`, `failed` or
  `timeout`, and answers `200`; completing an already terminal session
  returns it unchanged.
- A usage report records a model and its token counts and answers `201`.
- Both are for the session's creator or an admin token alone.
- A status that is invalid or `terminated` is `400`; a caller who is not the
  creator is `403`; an unknown session is `404`; a second close in flight is
  `409`; a usage report for a session that is not active is `409`; and a
  negative token count is `422`.

### Reading sessions and usage

- Sessions are listed newest first by default, each with its token summary,
  filtered by workspace, run, status, credential type and start time,
  ordered either way, paged with a limit of at most 500.
- One session is read with its token summary; its usage records are listed
  with a limit of at most 1000 and with totals computed over every record of
  the session whatever the page.
- These reads need read access **and** an active workspace: any other
  caller, a member of an archived workspace included, is `403 workspace
  access denied`.

### Reporting cost

- A workspace's cost is aggregated over a period by day, session or model,
  with the period defaulting to the last 24 hours, and answers with the
  totals and the breakdown.
- A `since` that is not before `until`, or an invalid grouping, is `400`.
- It needs read access and an active workspace, as the session reads do.

### Ending sessions with their workspace

- Archiving or deleting a workspace ends every active session of that
  workspace with the status `terminated`.
- No active session of the workspace remains afterwards.

### End-to-end flows

1. **An agent run is recorded end to end.** An agent opens a session against
   a workspace, reports usage twice as it works, and completes the session
   with `completed` and a duration. A client then reads the session and sees
   its token summary, lists its usage records and finds totals over both
   reports.
2. **A cost report is read.** With several sessions recorded across two
   days and two models, a client reads the workspace's cost grouped by day
   and then by model, and finds totals that agree with the sessions'.
3. **A workspace is archived under a running agent.** A session is open when
   its workspace is archived; the session is ended as `terminated`, and the
   agent's next read of the session is refused because the workspace is no
   longer active.

## Design Decisions

1. **Pagination is built here, as the first package that pages.** It was
   deliberately deferred from `01_service_foundation` to avoid shipping a
   facility with no caller; the contract it implements is the one `api.md`
   fixes, so `12_audit_and_events` reuses it rather than writing another.
2. **The creator, not the owner, closes a session.** The reference says so,
   and the reason matters: several agents share one workspace, and an owner
   check would let any of them close another's session.
3. **The archived-workspace answer here is `403`, not `404`.** It is the one
   family of routes where the reference prefers telling the caller the
   workspace is inactive over hiding its existence, and writing it as a
   requirement keeps it from being "corrected" to match the workspace routes.
4. **Totals are computed over the whole session, not the page.** A total
   that changes with the page size is not a total, and the reference is
   explicit.

## Dependencies

- `01_service_foundation` for the router, the error envelope and the request
  conventions whose pagination this package now implements.
- `02_identity_and_credentials` for the credential identity recorded on a
  session.
- `03_workspaces` for the workspace, the access decision and the archive and
  delete this package hooks into.
- `docs/prd/api.md` § Agent Session Endpoints and § Conventions are
  normative.

## Verified External API

Nothing new. The cursor's encoding is the service's own and is verified by
its tests rather than against a library.
