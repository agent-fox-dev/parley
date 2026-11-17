# 09. Run durable jobs on a queue the hub composes

## Status

Accepted.

## Context

A rebuild of a carry-patch workspace must be recorded before the request
that asks for it is answered, survive a restart, be retried with growing
delays, and never be pending twice for one workspace. Later packages add
the rebuild's handler (what an attempt does) and further sources of a
rebuild, and the model is meant to carry other job types later.

Several things had to be settled that the code, not the spec, decides:

- where the queue is created and how a handler for a job type reaches it,
  without `cmd/parleyd` changing when a package plugs in (ADR 01);
- how "one pending job per type and workspace" holds under concurrent
  enqueues, given that a follow-up (a queued job beside a running one) is
  allowed, so that no unique index can state the rule;
- how the jobs of a deleted workspace stop running, given that a remover
  of owned data (ADR 07) runs inside the delete's transaction and cannot
  see it commit;
- how a dispatcher woken by an enqueue sees a job whose transaction has
  not committed yet;
- what the shared rebuild enqueue returns, given that package 31 declared
  its interface before the queue existed.

## Decision

- `internal/jobs` is a general queue: a job has a type, a workspace, a
  payload and a result held as opaque JSON. `internal/carrypatch` gives
  rebuild jobs their fields, routes and events.
- The hub part `composeJobs` (`internal/hub/jobs.go`) creates the queue
  at composition, so `(*Composition).Jobs()` returns it to every later
  part and to `Options.Compose`. A handler is registered with
  `Jobs().Register(type, handler)` from a part or from `Options.Compose`;
  the hub's run function gets no option for it. `AdjustJobs` changes the
  queue's dependencies (clock, interval, wake) before its start hook binds
  them and runs `Recover`. A type with no handler keeps its jobs `queued`.
- Dedup is a check and an insert in the caller's transaction. `Store.Tx`
  admits one write transaction at a time in the process, so the two are
  atomic and there is neither a unique index nor a retry loop.
- Claim and cancel exclude each other through the same condition,
  `status = 'queued'`, in one serialized write transaction each.
- `RemoveWorkspace` deletes in the caller's transaction and, when one of
  the deleted jobs has a running attempt, cancels it from a store
  transaction of its own, which starts only after the delete's has ended:
  a rolled-back delete cancels nothing, and a settlement that finds no
  `running` row writes nothing and calls no hook.
- A dispatcher pass that was woken goes straight to the write
  transaction, which waits for the waking transaction to end; an
  unprompted pass first looks for work on a read connection, so that an
  idle hub takes no write turn every second.
- The shutdown grace of 30 seconds runs on the queue's own clock; the hub
  registers the queue as background work with the same grace. An attempt
  still running at its end is cancelled and not settled, and its job is
  recovered at the next start.
- `(*carrypatch.Component).EnqueueRebuild` keeps package 31's
  `RebuildResult{Queued, JobID, ExistingJobID}`; the route uses the
  unexported operation behind it, which returns the job, so a test that
  registers its own enqueuer does not intercept the route. A caller with
  no user id (the admin token) is recorded as `submitted_by`
  `admin_token`, its actor type.

## Consequences

- A package with a job type registers its handler from its own part; tests
  register scripted handlers through `hubtest.Config.Compose` and control
  the queue's clock through `AdjustJobs`.
- The `rebuild` handler is the carry-patch component's
  (`(*carrypatch.Component).RebuildHandler()`), registered by
  `composeCarryPatch`; a test that registers its own replaces it.
- Dedup relies on the single writer of `Store.Tx`: a second process on
  the same data directory, which the hub does not support, would break it.
- The queue's grace and the hub's stop limit are both 30 seconds; in
  production they expire together.
- A settlement whose own store transaction fails is logged with the job
  id and not retried: the job stays `running`, and holds its type and
  workspace, until the next start's `Recover` queues it again.
