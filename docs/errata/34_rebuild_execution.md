# Errata: 34_rebuild_execution

Where the implementation of package 34 (`.specs/34_rebuild_execution`)
differs from what the package says, or decides what it leaves open.

## The handler is part of the carry-patch component

The package asks for narrow interfaces for each collaborator of the
handler (workspace reader, patch store, job writer, fetcher, settings,
clock, emitter, describer, pusher, enqueuer). The rebuild is a part of
`*carrypatch.Component` instead and uses its bound `carrypatch.Deps`
(store, jobs, git, secrets, settings, events, clock), as the other
carry-patch operations do; what a rebuild adds to `Deps` is the landing
identity, the conflict-file describer, the scrubber, the per-operation
git limit and push limit, the test seams `Push` and `FetchRemote`, and
the test hooks `BeforeRebuildPatch`, `BeforeRebuildPublish` and
`BeforeRebuildSwap`. Tests replace them through `AdjustCarryPatch`. The
handler is `(*Component).RebuildHandler()`, registered by
`composeCarryPatch`.

## The files of a conflict report come from internal/gitrepo

The package takes a report's file entries from "the conflict report
component of `internal/land`". `internal/land` has no operation that makes
file entries: it fills `landed_by` (`Attribute`), which a rebuild leaves
`[]`. The entries are made by `(*gitrepo.Repo).ConflictFiles(ctx, target,
commit, paths)`, built on the same `git merge-tree` reading every conflict
report of the hub uses, with the commit's first parent as the three-way
base (also with `merge`, where the commit is `source_sha`); a path the
merge does not report is still described from the three commits. The
hub wires it as `carrypatch.GitConflictFiles{}`, which is also used when
`Deps.ConflictFiles` is nil. The report is `carrypatch.ConflictReport`,
`patch_id` followed by the members of `gitrepo.ConflictReport`.

## The rerere entries after a fresh clone

The package registers a post-clone step for `carry_patch` that deletes a
workspace's remembered-resolution entries. The post-clone registry holds
one step per mode, and the set-up (`(*Component).PostClone`) is that step,
so `PostClone` deletes the entries before it runs the set-up.

## Remembered-resolution entries

- An entry's `id` is the conflict's directory in `rr-cache`. Git records
  a new variant `<id>.<n>` each time an unresolved conflict is met again;
  the variants fold into one entry, and `resolved` is true when any of
  them has a `postimage`. Entries are keyed by workspace and id, so one
  conflict hunk met at two paths keeps one entry, for the first path.
- The failed commit is tried again only when the learning recorded at
  least one resolution; with none it would meet the same conflict.
- `RemoveRerereEntry` removes the store row only, not `rr-cache/<id>`, and
  the `preimage` variants of conflicts met again stay in `rr-cache`: the
  maintenance of remembered resolutions removes them.

## The job's heads are stored after the publish's transaction

The package has the patch-list changes and the job's
`integration_head_sha` committed in one store transaction.
`jobs.SetResult` opens a transaction of its own and `internal/jobs` is not
changed by this package, so the heads are stored right after the
patch-list transaction commits. A reader at the swap sees neither; after
the publish it sees both. A stop between the commit and `SetResult` leaves
the job without the heads until its next attempt publishes again.

## Order at the end of a run

The stale-input check and the follow-up enqueue run after the counts are
stored, not between the push and the completion: the job is still
`running` either way, and a failure to store the counts cannot turn the
attempt retryable after a follow-up was queued. Every patch the run
listed that is not deleted is checked, whatever its status; a branch that
was missing at the listing and exists now is stale, since its tip differs
from the `null` the run recorded.

## A push cut off by a stop

A push of the integration branch that ends because the attempt's context
was cancelled (the hub stops, or the workspace is deleted) emits no
`parley.rebuild.push_failed`: the attempt ends as a retryable `shutdown`
(or with nothing written), and the next attempt publishes and pushes
again. Every other push failure emits the event and the job completes.

## Scrubbing

The failure text stored as the job's `error` and the `reason` of the
events pass through the hub's scrubber with the attempt's upstream and
origin credential values. The `parley.rebuild.fail` event carries no
message member. The conflict report is not scrubbed: it holds commit and
blob ids, modes and repository paths, and the scrubber's cut to 1024 bytes
would make its JSON invalid.

## Checks

- The time-limit check of a git operation sets the limit to 5 s, not
  50 ms: the limit applies to every git operation of the attempt, and one
  replay runs several. The replay is made to hang by a smudge filter that
  sleeps, and the check confirms that process is killed.
- The credential table of the upstream fetch and "the fetch is not
  started" are observed at the upstream test origin (the credential it
  accepts, the requests it counts) rather than through a recording
  fetcher; the stub fetcher of the scrubbing check is the seam
  `Deps.FetchRemote`.
- "No event after the deletion" is read from the event log after the real
  `DELETE /api/v1/workspaces/:slug`, not from a recording emitter.
- The first execution path's CLI call uses `--poll-interval 1s`: the CLI
  takes whole seconds, minutes, hours or days only.
- In the second execution path the agent's calls use the developer's API
  key; the path asserts no actor.
