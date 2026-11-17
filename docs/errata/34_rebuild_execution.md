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
`[]`. The entries are made by `(*gitrepo.Repo).ConflictFiles(ctx, base,
target, commit, paths)`, built on the same `git merge-tree` reading every
conflict report of the hub uses. The caller names the three-way base:
with `rebase` the first parent of the commit that did not apply, with
`merge` the merge base of `source_sha` and the target, the report's
`base_sha` (`""` is the empty tree). A path the merge does not report is
still described from the three commits. The describer interface
`carrypatch.ConflictFileDescriber` takes the base too, and
`carrypatch.PatchConflict.Merge` tells `BuildConflictReport` which base
to give it. The
hub wires it as `carrypatch.GitConflictFiles{}`, which is also used when
`Deps.ConflictFiles` is nil. The report is `carrypatch.ConflictReport`,
`patch_id` followed by the members of `gitrepo.ConflictReport`.

## The set-up at the start of an attempt

An attempt repeats the workspace's set-up (`Setup` of package 31: the
record's default `integration_branch`, the upstream fetch, `rerere.enabled`)
with `Setup.LeaveBranch` set, so it never writes the integration branch:
`EnsureSetup` would create a missing one at the upstream base or
`head_sha`, and the publish is the one write of the branch. An attempt
that fails or stops at a conflict before the publish leaves a missing
branch missing, and the publish that creates it reports
`previous_integration_head_sha: null`. `EnsureSetup` itself, for its other
callers, still creates a missing branch. A set-up that fails while the
attempt's context is cancelled (the hub stops, the workspace is deleted)
ends the attempt as `shutdown` (or with nothing written), not as
`upstream_fetch_failed`.

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

The publish reads the integration branch's head before it opens that
transaction, which holds the store's one writer slot; the swap, inside
the transaction, is made against the head read, so a move in between
still fails it with `ref_moved`.

## Order at the end of a run

The stale-input check and the follow-up enqueue run after the counts are
stored, not between the push and the completion: the job is still
`running` either way, and a failure to store the counts cannot turn the
attempt retryable after a follow-up was queued. Every patch the run
listed that is not deleted is checked, whatever its status; a branch that
was missing at the listing and exists now is stale, since its tip differs
from the `null` the run recorded. A branch tip or upstream base the check
cannot read is logged and counts as not stale.

## A push cut off by a stop

A push of the integration branch that ends because the attempt's context
was cancelled (the hub stops, or the workspace is deleted) emits no
`parley.rebuild.push_failed`: the attempt ends as a retryable `shutdown`
(or with nothing written), and the next attempt publishes and pushes
again. Every other push failure emits the event and the job completes.

## Scrubbing

The failure text stored as the job's `error` and the `reason` of the
events pass through the hub's scrubber with the attempt's upstream and
origin credential values. The outcome the handler keeps from its `Run` to
the job's settlement holds the scrubbed text and reason and not the
values, since a job deleted before its settlement leaves it kept. The
`parley.rebuild.fail` event carries no message member. The conflict report is not scrubbed: it holds commit and
blob ids, modes and repository paths, and the scrubber's cut to 1024 bytes
would make its JSON invalid.

## The 10-minute limit is per step of the scratch tree

The package bounds each single git operation of a run to 10 minutes. The
limit (`Deps.RebuildGitDeadline`, `DefaultGitDeadline` in production) is
set by `RunGit` around each step of the build instead: one replay of a
commit (`Scratch.Pick`), one merge, one learning, one conflict report,
the discard of leftovers with the creation of the scratch tree. Each step
runs a few git processes, and a step whose processes together run longer
than 10 minutes ends the attempt as a retryable `timeout`, its processes
killed with their process groups.

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
