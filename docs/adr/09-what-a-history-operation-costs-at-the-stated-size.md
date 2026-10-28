# 9. What a history operation costs at the stated size

## Status

Accepted. The figures below were measured on 2026-10-28, not estimated.

## Context

The engine claims it applies a stack of 50 patches onto a repository of
at most 500 MB of packed objects, with at most 1000 changed files and
10 MB of diff per patch. Worker CPU and memory bound that work. Three
questions follow. Can the engine do it at all? What does it cost? And
what happens to an operation that cannot finish inside one unit of
platform work?

## What was measured

A repository of incompressible ballast, a base that moved on and a
branch of 50 one-file patches, built with the real git binary and
pushed into the store as one pack. The engine rebases the branch onto
the base, and the resulting tree is compared with the one the real git
binary produces from the same inputs. One developer machine, Node 24,
the non-Cloudflare store:

| Packed objects | 50 patches rebased | Per megabyte |
| -------------- | ------------------ | ------------ |
| 16 MB          | 3.9 s              | 0.246 s      |
| 128 MB         | 29.4 s             | 0.230 s      |
| 480 MB         | 108.9 s            | 0.227 s      |

Every one of them produced the tree the real git binary produced.

The suite that measures this is `src/history/size.integration.test.ts`.
It builds half a gigabyte and costs two minutes, so it runs when
`PARLEY_BIG_FIXTURE` asks for it and says what it is waiting for
otherwise.

## Decision

**The stated size holds, and the cost is reading the repository in, not
the patches.** The time is linear in packed bytes at about 0.23 s per
megabyte and all but flat in the number of patches: extrapolated to an
empty repository, the 50 replays themselves are under a second. The
engine hydrates a repository by serving one pack and indexing it (ADR
8), and that pack is what the time is spent on.

**An operation at the top of the range does not belong on a request.** A
Worker gives a request 30 seconds of CPU, and 480 MB costs 109. The
engine's own callers run merges and rebases off the request path, where
the budget is minutes rather than seconds, and a caller that has only a
request's budget must either work on a smaller repository or carry the
operation across units of work.

**An operation carries across units of work in a continuation, and
writes nothing until it finishes.** `rebase` takes a `budget` — a count
of patches, a deadline, or neither — and hands back a `continuation`
when the unit is spent. Nothing reaches the store until the last patch
is in: an operation that is abandoned, or that fails part way, leaves
the ref list and every ref's target exactly as they were, and no partial
result is reachable from any ref. That is the whole of the rollback
story, and it needs no undo.

**Past the stated sizes, the engine refuses rather than stalls.** The
four ceilings live in `src/history/limits.ts` and are checked before any
work is done: the patch count and the repository size once, the changed
files and the diff bytes per patch. A refusal is a 413 naming which
ceiling was reached and what is allowed, because a caller told only
"too large" cannot tell how much to split off.

## Consequences

- A continuation holds the hydrated repository in memory, so slicing is
  free within one isolate. It does not survive the isolate: an operation
  taken up in a later Worker invocation reads the repository in again,
  at 0.23 s per megabyte. For a large repository that is the dominant
  cost of continuing, which is a reason to give an operation the largest
  unit of work available rather than many small ones.
- The 500 MB ceiling is a real ceiling, not a round number: at
  0.23 s/MB it is already 109 seconds, and the next doubling would not
  fit in a cron invocation either.
- Measuring a patch costs reading the blobs it changed, which is why the
  diff-size ceiling is checked per patch as the rebase reaches it rather
  than for the whole stack up front.
