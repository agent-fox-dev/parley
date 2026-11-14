# Errata: 12_workspace_sync_from_origin

Where the implementation of package 12
(`.specs/12_workspace_sync_from_origin`) differs from what the package
says, or decides what it leaves open.

## The sync operation and its collaborators

- The sync is the method `(*workspace.Service).Sync(ctx, slug, opts)`,
  taking the slug and a `workspace.SyncOptions` value, so that a test runs it
  without an HTTP request; it returns a `workspace.SyncResult`. The package
  writes it as `Sync(ws)`.
- There is no stub credential resolver. The resolver stays package 10's
  `workspace.ResolveOriginCredential` over the workspace's secrets
  (`Deps.Secrets`), called at the start of every sync; tests change the
  stored secrets instead of stubbing it.
- The fetcher is the interface `workspace.Fetcher` (`FetchOrigin`), the
  field `Deps.Fetcher`, which the hub sets to its `gitrepo.Git` in
  `internal/hub/workspace.go` (nil is `gitrepo.Git{}`); tests wrap it
  through `(*hub.Composition).AdjustWorkspace`.
- `FetchOrigin` is a method of `gitrepo.Git`, not of `gitrepo.Repo`,
  because it talks to origin with a credential, which no `Repo` method
  does: `(gitrepo.Git).FetchOrigin(ctx, dir, url, cred, deadline)`, where
  the package writes `FetchOrigin(hub, originPath, credential)`. A deadline
  that passes is an error matching `gitrepo.ErrDeadline`.

## How origin's tags reach the copy

Git hands over the object of a tag only for a ref it writes, over every
transport (the file-based HTTP transport cannot fetch an object by its id).
So the one atomic fetch writes origin's branches and tags into a namespace
of its own, `refs/parley/fetch/<16 random hex digits>/` (`heads/` and
`tags/`), reads them from there and deletes the namespace right after
the fetch, after a failure too, before any origin head is set. The copy's `refs/tags/` is never written by
the fetch; the sync then creates the tags the copy lacks from the values the
fetch returned. The tag objects stay in the copy's object store until a ref
names them or git prunes them as unreachable.

The origin heads are set from that namespace after the fetch has succeeded,
in one `update-ref --stdin` transaction that removes those of branches
origin no longer has and creates or moves the rest. Git refuses that
transaction, and refuses a fetch straight into `refs/remotes/origin/*`,
when origin replaced a branch by one whose name it is a directory of, such
as `dev` by `dev/next`: the old origin head is in the way of the new one.
In that case the removals are made first and the rest after them. A fetch
that fails, or whose namespace cannot be deleted, changes no origin head;
the two-step case can leave the removals made if writing the rest then
fails, which only removes origin heads origin no longer has.

`(*gitrepo.Repo).CompareAndSwapRef` takes `""` as the expected value of a
ref that must not exist, and also answers `gitrepo.ErrRefMoved`, changing
nothing, when another writer holds the ref's lock, since that writer is
moving it at that moment.

## Refs whose name a ref only the hub has is in the way of

The package's six branch results do not cover a branch of origin that the
hub cannot create because a ref only the hub has holds a conflicting name:
git keeps ref names as a tree of files and cannot hold both
`refs/heads/feat` and `refs/heads/feat/x`, or both `refs/heads/topic` and
`refs/heads/topic/a`. Such a hub-only branch must stay untouched, so the
sync neither removes nor renames it.

- `(*gitrepo.Repo).CompareAndSwapRef` answers `gitrepo.ErrRefNameConflict`,
  changing nothing, when the ref it is to create has a ref in the way of its
  name, read from the repository's refs after git refused, not from git's
  message.
- Such a branch is reported with a seventh result, `name_conflict`
  (`workspace.BranchNameConflict`), with `local_sha` `""` and `origin_sha`
  origin's head; it is not created, no branch-move observer is told, and it
  is not in the event's `updated`. The event's `metadata` gains the list
  `name_conflict`, `[]` when empty, like the other results. The other
  branches are synced and the completion is recorded as usual; with
  `reset_to_origin=true` the result is the same. `--fail-on-diverged` does
  not count it: nothing has diverged.
- A tag of origin with such a conflict (`v1/rc` beside the hub's `v1`) is
  skipped, as a tag the hub has already is: it is not created and not in
  `updated`, and the sync goes on.

## The sync route

- There is no `internal/workspace/routes.go`: the workspace routes are the
  list `(*workspace.Service).Routes()` in `internal/workspace/service.go`,
  which `internal/hub/workspace.go` registers. The sync route is defined in
  `internal/workspace/sync_handler.go` and added to that list, so the hub
  needed no change of its own for it; the fetcher was already wired there.
- The router's patterns name parameters with a colon: the route is
  declared as `POST /workspaces/:slug/sync` under `/api/v1`, where the
  package writes `/api/v1/workspaces/{slug}/sync`.
- `reset_to_origin` given more than once is `400 invalid_request` too,
  like the other boolean query parameters of the workspace routes, rather
  than one of its values chosen.
- The stub sync operation recording its reset argument is not used: the
  route's tests send the three accepted queries against a workspace with a
  diverged branch and read the reset from the branch's result (`diverged`,
  `diverged`, `reset`).

## Completion, event and branch-move observers

- The caller reaches the sync as `SyncOptions.Actor`, an
  `identity.Principal`: the route passes `identity.PrincipalOf` of the
  request, a test calling `Sync` directly passes one. The event's actor is
  mapped from it by the same function as the `parley.workspace.update`
  event's (`eventActor`): the admin token is `admin_token` with no actor id.
  A `Sync` call whose actor is neither the admin token nor a user's
  credential is an error before anything is fetched.
- A completed sync does not write `updated_at`: it writes `last_sync_at`,
  `sync_error` (cleared), `head_sha` and `origin_head_sha` only, so that
  `updated_at` stays the time of the record's own last change by a create
  or an update.
- `RegisterBranchMoveObserver` and `NotifyBranchMove` are methods of
  `*workspace.Service`, like `RegisterPostCloneStep`, not package-level
  functions. An observer implements `BranchMoved(ctx, BranchMove) error`;
  `BranchMove.Before` is `""`, not absent, for a created branch, and its
  cause is the constant `workspace.MoveCauseSync` (`"sync"`). Observers are
  added in the order of registration and cannot be removed; a nil observer
  adds none.
- `Sync` returns the record read again after the completion transaction in
  `SyncResult.Workspace`, and the route renders that record rather than
  reading it a second time.

## A sync that cannot fetch

- The resolver's failure is a fetch failure through the real resolver: a
  test makes every read of the workspace's secrets fail rather than
  stubbing the resolver. The recording fetcher of the checks wraps the hub's
  fetcher and can be told to fail with a given error.
- A failed fetch writes `sync_error` alone; `updated_at` stays as it was,
  like `last_sync_at`, `head_sha` and `origin_head_sha`.
- The text is scrubbed with the hub's scrubber given the credential's
  password (or token) only, as the clone does; the user name is not a
  secret value, and a short one would redact unrelated parts of the text.
  When the scrubber cannot remove a secret cleanly its whole result is
  `[redacted]`, and the text then does not begin with
  `origin fetch failed: `.
- A `sync_error` that cannot be written makes the sync answer `500`
  `internal` instead of `502` `upstream_error`.

## Sync beside other writers

- When the fetch still ran as one `git fetch --atomic --prune` straight
  into `refs/remotes/origin/*`, two syncs of one copy at the same time were
  seen to collide: each locked the origin heads and `packed-refs`, and the
  loser failed (`cannot lock ref 'refs/remotes/origin/b0': is at … but
  expected …`), which answered `502`. The fetch now writes into a private
  namespace of its own and sets the origin heads in a separate
  `update-ref --stdin` transaction, and the whole of it is serialised per
  copy by an in-process mutex keyed by the slug on the `Service`, so that
  two such fetch-and-set steps never interleave: a sync waits for the
  other's fetch and never answers `409 workspace_busy`. The mutex is
  defensive: no test forces two fetches to overlap now, and
  `TestSync_TwoConcurrentSyncs` checks the outcome of concurrent syncs (both
  `200`, the branches at origin's heads), not the lock itself. The package's "take
  no lock on the workspace" holds for everything else: tag creation and the
  per-branch pass take no lock, and the workspace's status is never written.
  Tag creation stays outside the mutex because it is create-only: a tag
  another sync created first is skipped without an error and is not
  reported as created by the second sync. The mutex is per hub process,
  which is the only writer of its copies.
- `head_sha` and `origin_head_sha` of a completion are read from the
  copy's refs when the completion is recorded (the hub's head of the
  default branch and its origin head), not taken from the sync's own
  fetch: of two overlapping syncs the one that completes last would
  otherwise record an older origin head than the copy holds.
- The hook between a branch's evaluation and its swap is the optional
  field `Deps.BeforeRefSwap func(ctx, slug, branch string)`, called after a
  branch was classified as `created`, `fast_forward` or `reset` and before
  its compare-and-swap, nil in production and set by tests through
  `(*hub.Composition).AdjustWorkspace`, like `Deps.InsertWorkspace`. It is
  not called for a branch that does not move.
- The re-evaluation test covers its four cases in one sync, on four
  branches of one workspace, rather than one sync per case on `main`.
- The test giving up after five evaluations syncs with
  `reset_to_origin=true`: its hook moves `zz` to a new child of the common
  ancestor, which makes `zz` diverged, and only a reset tries to swap a
  diverged branch again.

## Branches a carry-patch workspace keeps out of sync

- `RegisterExcludedBranchProvider` is a method of `*workspace.Service`,
  like `RegisterPostCloneStep`. The provider implements
  `ExcludedBranches(ctx, slug) ([]string, error)`, taking a context as well
  as the slug. One provider is registered at a time; a later registration
  replaces it and nil removes it.
- The provider is asked after the fetch and the tag step, right before the
  per-branch pass. A provider failure therefore leaves the tags that sync
  created in the copy, as the fetch's origin heads stay. Only the branches,
  the sync fields and the event are untouched, which is what the package
  asks for.
- The `500` message names the failure and includes the provider's error,
  scrubbed by the hub's scrubber: `the sync of workspace "<slug>" stopped
  before any branch was compared: the excluded-branch provider failed:
  <error>`.
- A name the provider returns that origin has no branch for changes
  nothing.
- The checks of the integration branch and of the provider are one test.
  That test makes the fixture's record `carry_patch` with an `UPDATE` of
  `workspace_mode` and `integration_branch`, not through a create. It also
  removes the fixture's `dev` from origin so that `branches` is exactly the
  list the check names. It syncs with `reset_to_origin=true` for both parts,
  and then syncs the same refs as a `standard` workspace.

## The parley workspace sync command

- The 3-minute timeout is the command's request timeout
  (`command.Timeout`), so it bounds the reads of the rebuild job too, not
  the sync's `POST` alone; every read after the first is also bounded by
  the wait's `--timeout`.
- `--wait` is the CLI's shared wait, started only once the sync's answer
  names a job: it reads the job at once and then polls it one interval after
  each answer. `--timeout` therefore counts from the first answer about the
  job rather than from the sync's answer, and the job's first read comes
  right after the sync instead of one poll interval later. Without a
  `rebuild_job_id` nothing of the wait runs, so stderr stays empty, with no
  progress line.
- The message of `--fail-on-diverged` is a `condition_not_met` error naming
  the branches and then the patches: `diverged from origin: exp, patch/a`.
- The checks run the wait with `--timeout 1s` and a 10 ms poll interval set
  through the test options, because `--timeout` and `--poll-interval` take
  whole seconds or more (`50ms` is a usage error). The argument-count and
  `--timeout 0s` refusals and the `502` refusal are rows of the workspace
  commands' shared tests of a bad command line and of the hub's refusal.

## The sync extension point

- `RegisterSyncExtension` is a method of `*workspace.Service`, like
  `RegisterExcludedBranchProvider`. One extension is registered at a time;
  a later registration replaces it and nil removes it.
- The run value is `SyncRun{Workspace, Actor, ResetToOrigin, OriginHeads}`:
  the record as the sync read it before the fetch, the caller as an
  `identity.Principal`, the flag, and a copy of origin's branch heads as
  fetched. The second stage gets a copy of the per-branch results.
- `SyncAddition.Members` is a `map[string]json.RawMessage`. A member whose
  value is not valid JSON stops the sync with `500 internal` before the
  completion is recorded, as an error of the second stage does, since the
  answer could not carry it.
- The extra members follow the workspace object's members and `branches`
  in the answer, in byte order of their names. A member whose name the
  object or `branches` has already is left out without an error.
- The text of a failed first stage is the extension's error, scrubbed, with
  no prefix; only a failed origin fetch begins with `origin fetch failed: `.
  The log line of both is `workspace sync could not fetch`. The `500`
  message of a failed second stage is the scrubbed error text alone.
- The check that a disconnect stops neither stage is a second sync in the
  stage-order test, not a part of the first one: a request the caller
  cancels has no status to assert. The tests' workspace is `demo`, not
  `my-project`, and the members test also returns a member named
  `branches`, to show that it is not replaced either.
