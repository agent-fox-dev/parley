# 8. Stand the history engine on isomorphic-git

## Status

Accepted. The findings below were established against isomorphic-git
1.44.7 as installed, not from its documentation.

## Context

Parley has to merge, rebase and resolve conflicts with no git binary and
no filesystem, and produce the history git itself would. Writing that
arithmetic from nothing is a year of work and a decade of corner cases, so
the question is which library to stand on and what it really does.

## What isomorphic-git actually provides

| Operation       | Present | Notes                                                                                                           |
| --------------- | ------- | --------------------------------------------------------------------------------------------------------------- |
| `merge`         | yes     | Works on a **bare** gitdir with no worktree                                                                     |
| `findMergeBase` | yes     |                                                                                                                 |
| `cherryPick`    | yes     |                                                                                                                 |
| `rebase`        | **no**  | Must be built from `cherryPick` or the plumbing                                                                 |
| plumbing        | yes     | `readTree`, `writeTree`, `readBlob`, `writeBlob`, `readCommit`, `writeCommit`, `walk`, `writeRef`, `resolveRef` |

Three details decide the design:

- **Everything takes an `FsClient`.** There is no object-store entry point;
  the library is filesystem-shaped throughout. Backing it with Parley's
  repository store therefore means supplying a filesystem, not an object
  adapter.
- **`dryRun` computes a merge without writing it.** A dry-run merge returns
  the merged tree's oid but does not put the tree in the object store, so
  reading it back fails. That is exactly what a three-way merge wants:
  the arithmetic without a commit.
- **Conflicts arrive as a thrown `MergeConflictError`** carrying
  `data.filepaths`, `bothModified`, `deleteByUs` and `deleteByTheirs` —
  which is the list of conflicting paths this package must report.

A `mergeDriver` callback can supply the content merge for a path, which is
where recorded resolutions are replayed.

## Decision

**The engine stands on isomorphic-git**, reached through an in-memory
filesystem that is lazily backed by the repository store of package 05.
A read of an object the fs does not hold resolves through
`RepositoryStore.readObject`; writes go to an overlay and are collected at
the end of an operation into one pack, which is handed to the store with
the ref changes. Nothing hydrates a whole repository, so a 500 MB
repository costs only the objects an operation touches.

**Rebase is Parley's own**, built on `cherryPick` over the plumbing,
because the library has none. That is the one piece of history arithmetic
written here, and it is the reason the differential corpus against the
real git binary exists.

**Objects are deflated with `CompressionStream`**, which both Node and
Workers have, so the adapter needs neither `node:zlib` nor pako of its own.

## Consequences

- No git binary, no filesystem, no child process; the engine runs where a
  Worker runs.
- The engine is bound to a library's behaviour, which is why equivalence
  with the real git binary is a **requirement** verified by a generated
  corpus rather than a test strategy: a library upgrade that diverges from
  git fails the build.
- The merge arithmetic is isomorphic-git's, so a bug there is a bug here.
  The corpus is what would catch it.
- A tag object is not reconstructable from the Cloudflare binding's
  decoded reads, so the adapter reads commits, trees and blobs and refuses
  a tag object with a clear error. Nothing in the hub's contract merges a
  tag.
