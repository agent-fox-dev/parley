# Errata: 38_carry_patch_rerere_and_maintenance

Where the implementation of package 38
(`.specs/38_carry_patch_rerere_and_maintenance`) differs from what the
package says, or decides what it leaves open.

## The routes take the workspaces scopes after carrypatch.Route

`carrypatch.Route` gives a carry-patch route the scopes `patches:read` and
`patches:write`. The three routes of this package need `workspaces:read`
and `workspaces:write`, so each is declared with `Route` (for its kind,
its access need and the guard) and its `Scope` is set afterwards, as
`rebuildRoute` does for the rebuild routes.

## The list is built on RerereEntries, which no longer fails

The list is the entries of `(*Component).RerereEntries`, sorted newest
`recorded_at` first, then `id` ascending, and paged in memory; no second
record of an entry is kept. `RerereEntries` used to fail when the copy
could not be opened or git could not say whether an entry is resolved; it
now lists such an entry with `resolved` `false`, so that the list, the
patch status's count and the forget answer in every state of the copy.

## An empty pathspec reaches the forget

The router's `*name` parameter took one or more non-empty segments, so
`DELETE …/rerere/` answered `404` before the handler and could not answer
`400 invalid_request`. A route now sets `server.Route.EmptyCatchAll` to
take the empty remainder too; only the forget sets it, and the other
catch-all routes keep their `404`.

## Git keeps a conflict's variants inside its directory

The package and erratum 34 speak of variants `<id>.<n>` of a conflict. Git
keeps them as `preimage.<n>` and `postimage.<n>` inside `rr-cache/<id>`,
so the forget removes that one directory, renamed first into a fresh
directory of `rr-cache` and then removed, so that of two forgets at once
one removes it and the other finds it gone. The directory is removed
before the stored entry: a forget cut short leaves an entry without a
resolution, never a resolution without an entry.

## The transport rule is the hub's, not only carry-patch work's

The rule sits in `internal/gitrepo`, under every git process with a remote
URL, so origin's clone, sync, push, forward and mirror are held to `https`
and `ssh` as well as carry-patch work. Package 10 accepts only `https://`
and `git@host:path` at creation, so no workspace the hub creates reaches
it; tests that used a local path as a remote were changed: the mirror
tests reach origin's directory as `git@origin.test:<dir>` through a test
`ssh` that runs `git-receive-pack` locally, and the sync test's row of a
missing local origin is a local origin's refusal. Every
`<helper>::<address>` is refused, `https::` included, and a host or user
starting with `-` is refused as an `ssh` address.

## The rebuild's upstream fetch is held to the per-operation limit

The package's time-limit check hangs the upstream fetch of the attempt's
set-up. That fetch was held only to the whole upstream fetch's 10-minute
deadline (erratum 31), not to `Deps.RebuildGitDeadline`. The set-up a
rebuild repeats now holds each git operation of its fetch to
`RebuildGitDeadline` too; both are 10 minutes in production. An attempt
cut off there fails as `upstream_fetch_failed`, retryable, with the
runner's text `git did not finish within its deadline (<limit>)`.
`EnsureSetup`, the clone's set-up and the sync keep the whole-fetch
deadline alone.

## The argument guard covers the operations carry-patch calls

The guard refuses a value beginning with `-` in `IsAncestor`,
`LookupRef`, `CompareAndSwapRef`, `SetRef`, `DeleteRef` and
`FetchOriginBranch`, and passes the values git reads after
`--end-of-options`. `ResolveBranch` answers such a branch as not found,
with no error and no git started, rather than `ErrInvalidArgument`: it
hands git the branch only as `refs/heads/<branch>`, and its callers across
the hub (changes, landing, workspaces) answer every error with `500`, so
opening a change from a branch `-x` would have answered `500` instead of
`400 source_not_found`. `MergeBase`, `CommitsBetween`, `Cherry` and `Subjects`
only receive ids read from git and are unchanged. A full ref name is
refused only when it begins with `-` itself: `refs/heads/-x` is passed
whole, since git cannot read it as an option and landing, changes and the
source control API share these operations; the API refuses such branch
names already. None of these operations takes a caller's path, so the
argument check has no path rows. `SanitizeGitText` now removes the
credential values before it rewrites URL user information, since a
password holding `/` left the user name behind (`https://alice:@host`).

## The start-up discard skips workspaces whose clone is outstanding

A workspace in `pending` or `cloning` is left out: its clone replaces the
copy, and it normally has none. A `failed` or `archived` workspace whose
copy is missing is logged with its slug.

## The test stand-ins of patch_purge were renamed

Tests of `internal/hub` and `test/e2e` registered a maintenance task
named `patch_purge` of their own; with the hub's registered, each would be
a start error. They use `retention` or `sandbox_record_cleanup`, names no
part registers, chosen to keep the order each test checks.

## Checks

- The rebuild-recorded entry, the refusals of the three routes and the
  credential check passed on their first run: the list route, the guard,
  the access check and package 31's cleaning delivered them. Each was
  seen failing with the delivering lines removed for one run (the list
  test on the tree before the route).
- The time-limit check of a request repeats the call, at most three times,
  until one reaches the hanging remote: under the suite's load git did not
  reach it within the 300 ms limit.
- The ref-deletion fault of the purge is a held `refs/parley/replaced/p2.lock`
  in the copy, and the store fault a SQLite trigger the test creates; no
  seam was added for either. The restore race's pause is
  `Deps.BeforePurgePatch`.
- The unremovable work area of the start-up check is made with file
  permissions (0500, restored in cleanup); the leftover of an interrupted
  rebuild is the real scratch working tree, its directory and registration
  moved aside before the attempt is cancelled and moved back after.
- The developer's path runs in the e2e conflict test, whose conflicted file
  is `a.txt`, not `src/net/dial.go`. The purge path runs the loop every
  50 ms on the hub's fake clock, purges before a restart and checks that
  passes after the restart run without a failed purge. The restart path
  plants the scratch working tree a killed stop leaves while the attempt
  waits on upstream, since a stop in process lets the attempt clean up.
- `parley rerere list` takes no `--limit` or `--cursor`, as `parley
  rebuild list`; a later page is not reachable from the CLI.
