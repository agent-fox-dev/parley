---
spec_id: "06"
spec_name: "history_engine"
title: "Compute rebases, merges and recorded resolutions in TypeScript"
status: "active"
created_at: "2026-10-27T18:31:36Z"
updated_at: "2026-10-28T07:22:24Z"
intent_hash: "886fcd0fe7ba4ebccdc302ca1247676b4dc3cffc7fc6c6f09c5e0cf88b8fd71e"
schema_version: 2
source: "docs/prd/01-port-af-hub-to-typescript-on-cloudflare.md"
---

# Compute rebases, merges and recorded resolutions in TypeScript

## Intent

The hub rebases branches, merges them, and replays resolutions it recorded
for conflicts it has seen before — by shelling out to git. Parley has no
subprocess and no git binary, and its store computes nothing, so this is
where that history is produced in TypeScript instead. It is the one package
whose output has to be proved rather than described: a rebase that is
*nearly* git's is a wrong answer that nobody notices until a patch stack is
silently different from what the same commands produce locally.

## Goals

- A three-way merge, a rebase and a merge-commit strategy that produce, for
  any input they accept, **the same history the real `git` binary produces**:
  the same tree, the same parents, the same order.
- A conflict reports exactly the paths `git` reports, and leaves nothing
  half-written.
- A resolution recorded for one conflict is replayed when the same conflict
  recurs, across restarts of whatever recorded it.
- That equivalence is **proved** by a differential suite of at least 200
  generated histories run both ways, which is part of the project's checks.
- The engine works at the sizes the source PRD states, continues across units
  of platform work, and fails cleanly rather than stalling past them.

## Non-goals

- **Any HTTP route.** This package is a library with no surface of its own.
  The rebuild, merge, batch-rebase and rerere routes are `09_carry_patch` and
  `10_merges`, which drive this engine.
- **The patch list, rebuild jobs and their policies.** Position order,
  `fail_fast` versus `continue`, skipping `merged_upstream` patches and
  queuing follow-up rebuilds are `09_carry_patch`. This engine applies one
  thing to another and reports what happened.
- **Transport.** Fetching from and pushing to a remote is
  `05_repository_store` and `08_sync_and_branch_push`; this engine reads and
  writes objects through the repository store and talks to no remote.
- **Matching git's output text.** Only the resulting history and the set of
  conflicting paths must match, not git's messages, its index format or its
  on-disk rerere layout.

## Background

Decision D14 of the source PRD chose TypeScript over a container running real
git: the deployment stays serverless, with no image to build and no second
runtime to operate. The cost was accepted deliberately and lands here.
`isomorphic-git` gives object access, fetch and push, and gives **neither
rebase nor rerere**; the two merge strategies and the conflict machinery are
also ours. So this package is new code standing in for one of the most
heavily exercised programs in the world, and the only honest way to ship it
is to compare it against that program on every change.

Hence the differential suite, which is a requirement here and not a
nice-to-have. It generates histories, runs the same operation through the
engine and through the real `git` binary, and fails on any difference in
tree, parentage, commit order or conflicting paths. `git` is a **test**
dependency only; nothing in the deployment runs it.

Two further constraints come from the platform. Worker CPU and memory bound
the work, so the engine must continue across units of platform work rather
than assume one long-running process, and it must fail with a stated reason
past the sizes in decision D17 rather than stall. And rerere is ours: git's
rerere is a directory of recorded resolutions, which there is no filesystem
for, so resolutions are keyed by a conflict identity the engine computes and
kept in the deployment's own store.

## Requirements

### Three-way merge

- Given a base tree and two sides, the engine produces a merged tree in which
  a path changed on only one side takes that side's content, and identical
  changes on both sides merge cleanly.
- Different changes to the same region of one path are a conflict naming that
  path.
- Renames, mode changes, deletions, additions and binary files are treated as
  `git` treats them.

### Rebase

- Rebasing a branch onto a base copies each of the branch's own commits, in
  order, onto the base.
- Each copied commit keeps its author and its message; its committer and its
  parents are new.
- A commit whose changes are already present becomes empty and is handled as
  `git` handles it.
- A conflict stops the rebase at that commit and reports the conflicting
  paths; no later commit of the branch is applied.

### Merge strategy

- Merging a branch into a base produces one merge commit with the base and
  the branch as its two parents.
- Its tree is the three-way merge of the base, the branch and their merge
  base.
- A conflict produces no commit and reports the conflicting paths.

### Conflicts

- A conflict reports every conflicting path and no path that merged cleanly.
- A conflict has a stable identity computed from its content, so that the
  same conflict arising again is recognised as the same one.
- A conflicting operation writes nothing: the repository is exactly as it was
  before it began.

### Recorded resolutions

- Resolving a conflict records the resolution against that conflict's
  identity.
- When the same conflict arises again, the recorded resolution is applied and
  the operation continues as though there had been no conflict.
- A recorded resolution outlives the process and the invocation that recorded
  it.
- Resolutions can be listed, each with its identity, the path where known,
  when it was recorded and whether a resolution is recorded or only the
  conflict has been seen; and one can be forgotten by identity, or by path
  while that path is in conflict.

### Equivalence with git

- For every case in the corpus, the history the engine produces — the
  resulting tree, each commit's parents, and the order of the commits — is
  identical to the history the real `git` binary produces from the same
  inputs with the same strategy.
- For every case that conflicts, the set of conflicting paths is identical to
  the set `git` reports.
- The corpus holds at least 200 generated histories covering linear and
  branching history, renames, binary files, mode changes, empty commits,
  merge commits in the source, conflicting and clean patches, and a stack of
  at least 20 patches.

### Size and progress

- A stack of at least 50 patches applies onto a repository of at most 500 MB
  of packed objects, with at most 1,000 changed files and 10 MB of diff per
  patch.
- Past any of those, the operation fails with a reason naming the limit
  exceeded, rather than stalling or being killed.
- Work that cannot finish in one unit of platform work continues in another.
- A failed or abandoned operation leaves the repository unchanged.

### The engine itself

- The engine is reached through one interface and reads and writes objects
  only through the repository store.
- The engine runs where no `git` binary exists.

### End-to-end flows

1. **A branch is rebased in a real repository.** A calling package asks the
   engine to rebase a branch onto a base in a workspace's repository; the
   engine reads the commits and trees through the repository store, copies
   each commit onto the base, writes the new objects back through the store
   and reports the new head; the ref is then readable with the expected
   history.
2. **A conflict is resolved once and replayed.** The engine hits a conflict
   applying a patch and reports the paths; the resolution is recorded against
   the conflict's identity; the same patch is applied again onto the same
   base after a restart, the engine recognises the conflict, applies the
   recorded resolution and completes without reporting a conflict.
3. **The corpus agrees with git.** The differential suite generates a corpus,
   runs each case through the engine and through the real `git` binary, and
   compares the resulting tree, parents, commit order and conflicting paths;
   every case agrees.

## Design Decisions

1. **Equivalence is a requirement, not a test strategy.** The engine's
   correctness criterion is "the same history git produces", which cannot be
   stated as a list of examples. It is written as a requirement verified by a
   generated corpus, so that a future change that diverges from git fails the
   build rather than passing a thinner set of cases.
2. **Conflict identity is computed, not borrowed.** Git's rerere keys a
   resolution by a hash of the normalised conflict and stores it in a
   directory. There is no filesystem here, so the engine computes its own
   stable identity from the conflict's content and keeps resolutions in the
   deployment's store. The identity need not equal git's; it must only be
   stable across occurrences and restarts.
3. **Only history and conflicting paths are compared with git**, not
   messages, index format or rerere layout. Those are git's internals, and
   binding the port to them would make every git release a risk without
   making any client's behaviour more correct.
4. **The engine is a library with no routes**, so that `09_carry_patch` and
   `10_merges` differ from each other in policy and not in arithmetic.

## Dependencies

- `01_service_foundation` for the platform interface pattern.
- `05_repository_store` for reading and writing objects and refs, and for the
  capability report that says what the store will accept.
- `isomorphic-git` for object access and the git object model. It provides
  neither rebase nor rerere, cannot use IndexedDB or `lightning-fs` in a
  Worker, and needs a storage adapter of its own. **Unverified**, and the
  single largest technical risk in the port.
- **The real `git` binary**, as a test dependency only, for the differential
  suite. Nothing in the deployment runs it.
- `docs/prd/api.md` § Rebuild Endpoints and § Rerere Management Endpoints
  describe what the callers of this engine must be able to report.

## Verified External API

Nothing was verified against installed code. `isomorphic-git`'s Cloudflare
Workers guide was read during the PRD run and says only that IndexedDB and
`lightning-fs` are unavailable and a custom `fs` adapter is needed; no
signature was read, and its absence of rebase and rerere is inferred from its
documented API surface rather than from its source.

The first task of this package verifies the library against its installed
types: which operations exist, what a storage adapter must implement, how
object reads and writes are expressed, and whether its memory behaviour fits
a Worker at the sizes decision D17 states. Where the answer differs from this
document, the finding is recorded and these requirements corrected.
