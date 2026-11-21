# Errata: 59_code_index_builds

Where the implementation of package 59 (`.specs/59_code_index_builds`)
differs from what the package says, or decides what it leaves open.

## Every build has a directory of its own

The package puts the tree at `<data dir>/code-index/<workspace>/tree`,
written beside the old tree and swapped, and the index at
`<workspace>/index`. agentkit-go's `codesearch`, given an `IndexDir`, loads
the index stored for a directory instead of building it, and revalidates
it file by file only when it is searched; a tree swapped in place would
keep the index, the file count and the partial mark of the commit before.
Each build therefore writes `<workspace>/<commit>/tree` and
`<workspace>/<commit>/index`, the record names the commit, and the build
the record no longer names is removed once the new record has committed.
A failed build removes its own directory, so the index there was stays
usable as the package asks.

## A stored index is loaded on its first use

The package opens the stored index of every workspace at hub start. Loading
copies the index's files into a working directory, which a start would do
for every workspace before it serves. The start checks only that the
directory the record names is there, and queues a build where it is not;
the index is loaded when it is first used (`Component.Files`). One that
cannot be loaded then is treated as none: its directory is removed and its
build queued. A partial index is not stored whole by `codesearch`, so its
first use after a restart builds it again within its bounds; this is not a
build job and is not counted.

## Whether an index is partial comes from its search tool

`codesearch` has no accessor for it. After a build the component runs the
index's `code_search` tool once, with a query that matches no file, and
reads `partial` from its result.

## The build_timeout bound is the library's

The package's check holds a build past `build_timeout` through the
component's seam. Only `codesearch` enforces the wall time (as
`MaxBuildTime`), and a build slow enough to meet it cannot be made on
purpose in a test; the test hands the section's value to the index and
makes the index report a bound, as the library does when its time is over.

## A rebuild moves the integration branch

The package lists a rebuild of a carry-patch workspace among what moves the
head of the default branch. A rebuild publishes to the workspace's
integration branch (`deploy` by default). The component is told of that
move and builds when the integration branch is the default branch; the
default branch itself moves by a sync, which is told too.

## A deleted workspace's directory goes in the next maintenance pass

The delete's transaction removes the record and the builds; nothing runs
after a delete outside its transaction, so the directory is removed by the
maintenance pass, as the package's check allows.

## workers above 3 acts as 3

The package bounds the builds by `[code_index].workers`, 1 to 16,
"whatever else the job queue runs". The hub's queue runs four jobs at once
(`jobs.MaxRunning`), and it claims the oldest first, so builds allowed to
take all four places would hold every other job, a rebuild of a carry-patch
workspace among them, for as long as a backlog lasts; the first start with
the section queues a build of every workspace. The limit of the builds is
therefore `workers` or 3, whichever is less. Values up to 16 are still
accepted.

## build_timeout bounds the index, not the tree

`build_timeout` is the wall time of the index build, which `codesearch`
enforces. Writing the commit's files comes before it and has no bound of
its own but the job's context.

## Smaller choices

- `[code_index]` is owned by the configuration loader as a whole section,
  so that the file holding it, even empty, turns the feature on; its
  failures are reported together, and with the refusal of another
  section's key when there is one.
- The setting's row is in the one settings table of `internal/workspace`,
  and the fork's settings test is `TestRead_ShowsAForkTheEffectiveSettingsOfItsParent`
  there, not in `internal/fork`.
- The status route carries no entity tag and no read timeout: it reads a
  record, a ref and the queue, and its answer changes without a ref moving.
- `error` is shown whenever the last build failed, also when the head has
  moved since and the state is `queued`.
- The whole tree of the commit is written before the index is built,
  whatever `max_files` and `max_size` are: the bounds are the index's, and
  the tree of a workspace over them holds every file.
- A file whose name the hub's file system cannot hold (longer than a name
  may be, or bytes it does not take) is left out of the tree, like a name
  that collides with another by case; the build goes on.
- A hub starts even when the head of a workspace cannot be read: the
  failure is logged and the maintenance pass looks again.
- A build that fails 21 times is dead-lettered by the queue like any job;
  the next maintenance pass queues a new one while the index is behind.
