# 7. Account for repository bytes, and what the stated sizes cost

## Status

Accepted. The figures below were measured on 2026-10-28, not estimated.

## Context

Parley claims a workspace's repository may hold 500 MB of packed objects
and 10,000 refs. Two questions follow. Can the implementation do that at
all? And what stops a repository growing without limit, given that the
store reports no storage error of its own?

## What was measured

The pack reader and writer, over incompressible data so that compression
does not flatter the numbers, on one developer machine:

| Fixture        | Pack       | Write            | Read                   |
| -------------- | ---------- | ---------------- | ---------------------- |
| 500 × 64 KiB   | 31 MB      | 388 ms (81 MB/s) | 20 ms (1563 MB/s)      |
| 2,000 × 64 KiB | 125 MB     | 1.56 s (80 MB/s) | 80 ms (1563 MB/s)      |
| 500 × 1 MiB    | **500 MB** | 7.2 s (70 MB/s)  | **254 ms** (1969 MB/s) |

Ten thousand refs are listed, written and added to in under half a second.

Reading a pack is cheap because it is one inflate pass with the deltas
resolved; writing one costs a deflate per object, which is where the time
goes. Neither is near a limit that would make the stated sizes impossible,
and a copy relays a pack the remote already built rather than building one.

The 10,000-ref fixture runs in the suite behind `PARLEY_LARGE_FIXTURES=1`;
the 500 MB one is a measurement rather than a test, because seven seconds
of fixture building does not belong in a gate anybody runs often.

## Decision

**The stated sizes stand.** Nothing measured suggests 500 MB of packed
objects or 10,000 refs is out of reach, so this package's requirements and
decision D17 of the source PRD are left as they are.

**The service counts the bytes it writes.** A `repository_usage` row per
repository is incremented before each pack is written, and a write that
would take a repository past 1 GB is refused with `507` and a message
naming the limit. The count is taken before anything is written, so a
refused write leaves the repository exactly as it was and everything it
already holds still readable. Deleting a workspace forgets the count with
the rest of it.

`507` is the service's own choice: the reference names no error for this,
because the hub it is modelled on had a filesystem to run out of.

## Consequences

- A repository cannot grow without limit, and the caller is told which
  limit it met rather than seeing an opaque failure.
- The count is of bytes **received**, not of bytes the store settled on
  after its own packing, so it is an upper bound on what is really held.
  An upper bound is the safe direction for a cap.
- The numbers above are one machine's. A Worker is slower, and a copy
  there is bounded by the per-request budget rather than by this code,
  which is why a copy is checkpointed by ref batch in the first place.
