# 05. Deliver events by reading the log by position

## Status

Accepted.

## Context

Every consumer of hub events follows the same log: the event stream, the
long-poll of the event log, event webhooks and the metrics. Each must see
every committed event exactly once, in `seq` order, resume from a position
after a disconnect or a restart, and never see an event while one with a
lower `seq` is still uncommitted.

Events are written by `event.Emit` inside the caller's transaction of
`(*store.Store).Tx`, so the event commits or rolls back with the state change
it reports. The store has no hook that runs after a commit, and the event
component cannot tell when the caller's transaction commits: when `Emit`
returns, the transaction is still open and may still roll back. Pushing
events to consumers from `Emit` would hand out events that never commit.

## Decision

Consumers read the log by position, and the component only tells them when
to read again.

- **The counter row assigns `seq`.** `hub_event_seq` holds `last_seq`. `Emit`
  increments it inside the caller's transaction. The store admits one write
  transaction at a time, so commit order is `seq` order: a rolled-back
  transaction gives its number back to the next one, and deleting event rows
  never lowers the counter.
- **Every read is a prefix.** `ReadAfter(position, limit)` reads the records
  and `last_seq` in one read-only transaction, which begins without the write
  lock and sees one snapshot. In that snapshot every `seq` up to `last_seq` is
  committed. So a read returns a gap-free run of the log, and the position it
  reports as examined (the last record's `seq` when the limit was reached,
  otherwise `last_seq`) never claims an event the read could not see.
  `oldest_seq` is a stored lower bound: `Expire(cutoff)` raises it and deletes
  nothing, and `ReadAfter` never returns a record below it.
- **The component notices commits by polling the counter row.** `Run`, which
  the hub runs as background work, reads `last_seq` every 100 ms while
  anything waits: a waiter, or an `OnCommit` callback. It reads at once when
  `Emit` or `EmitNow` kicks it. While nothing waits, it reads nothing. When
  `last_seq` has risen, it counts the new events by type, hands them to the
  `OnCommit` callbacks in `seq` order from its one goroutine, and wakes the
  waiters.
- **A waiter waits on a channel, then reads by position.** The unexported
  `waitAfter(position)` returns a channel. It is closed once a commit of a
  higher `seq` has been observed (at once if one already has), and in any case
  within a second. A woken waiter calls `ReadAfter` from its own position.
  A spurious wake-up costs one read, and a missed one delays an event by at
  most a second. No event is lost, because the log, not the wake-up, carries
  the events.
- **Counts are taken at commit.** `Counters().WrittenByType` counts the events
  the poll or a call of `Counters` (which first catches up synchronously)
  found committed past `last_seq` as read when the component was bound. A
  rolled-back emit never appears in the log, so it is never counted.
- **The hub ends the poll before it closes the store.** `Bind` reads
  `last_seq` and returns its error, which fails the start. `Run` returns when
  its context ends, and wakes every waiter one last time. The hub registers it
  with `AddBackgroundWork`, whose stop waits for it, with a grace of one
  second, before the stores close. `Bind` and `Run` are the two methods only
  the hub's composition calls. Every other package uses the names of the
  component's fixed API.

## Consequences

- An event reaches a waiting consumer within about 100 ms of its commit,
  however long the emitting transaction stays open after `Emit`. While a
  stream, a long-poll or a callback waits, the hub reads one row ten times a
  second.
- Consumers hold no in-memory copy of the log that could diverge from it. A
  consumer resumes from any position the log still holds, after a reconnect
  or a restart, with the same read it uses live.
- An `OnCommit` callback runs on the component's goroutine and must return
  quickly. It is called only while `Run` runs, so not before the hub serves.
  The events committed earlier, by start hooks, are delivered once it does.
- A test that waits for commits runs `Run` itself and stops it before the
  store closes.
