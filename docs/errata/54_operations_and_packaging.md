# Errata: 54_operations_and_packaging

Where the implementation of package 54
(`.specs/54_operations_and_packaging`) differs from what the package says.

## git is looked up on the process's PATH when the environment has none

The package says `git` is looked up on the `PATH` of the environment the hub
was started with. `hub.Run` reads its environment from `Options.Env` alone
([ADR 06](../adr/06-load-the-secret-keys-before-the-stores-open.md)), and
the hubs of the test suite, `hubtest`'s among them, start with environments
that hold no `PATH`. The git check (`internal/hub/gitcheck.go`,
`lookPathIn`) therefore looks `git` up on the environment's `PATH` when the
environment has one, a test's empty directory or script directory
included, and on the process's own `PATH`, the one `internal/gitrepo` runs
git with, when it has none. In `parleyd` the environment is the process's,
so both are the same.

## The git version line is a start report

The package says the hub writes the line `git version checked` on success,
and that it precedes the first effect of every other start hook. A failed
start writes one error line and nothing else (ADR 06), so a line written by
the `git-version` hook would precede the error line of a later hook. The
check still runs first and once, before every other start hook; the line is
a start report (`addStartReport`), written once the listener is bound,
right before the `listening` line. The test's later hook asserts that git
has already run, and that the line precedes `listening`.

## Where the checks of a few criteria live

- The load helper of the restore tests is `test/e2e/load_test.go`, not
  `test/e2e/load.go`: `test/e2e` holds only test files.
- The restore test of an idle hub's snapshot hands the restored hub a
  listener it bound on a free loopback port (`Options.Listener`), and its
  start hook checks that the hub has not yet asked that listener for its
  address or a connection, the stage at which the hub binds its configured
  address otherwise; a port written into `config.toml` would be free
  between the test choosing it and the hub binding it, and another test's
  listener could take it then. That the hub binds its configured address
  only after its start hooks is the start order's test in `internal/hub`.
- The restore test under load takes the commits pushed before the backup
  began and the last commit pushed from the load's own record of each
  push rather than from `git rev-parse` on the source, and also checks that
  each restored load table holds at least the rows committed before the
  backup began: without that, a store copied as a raw file passes.
- The performance harness takes its request shapes from the routes the code
  registers (`internal/fork`, `internal/scm`, `internal/change`,
  `internal/land`, `internal/event`, `internal/sandbox`), not from the PRD
  sections the package points at.
- The slow-fork row of the request-duration check delays 3 of the 50 fork
  requests, not 5: the nearest-rank p95 of 50 samples is the 48th, so 3 is
  the fewest that miss the target.
- The stub hub of the landing check holds the 16 requests of
  `land_concurrency` until all have arrived (or 2 seconds pass), so that
  the 16 in flight do not depend on the scheduler.
- Each scenario that needs the target branch `perf/<run id>` asks for it
  and takes `409 ref_exists` as made by an earlier scenario of the run;
  there is still one target branch per run.
- The stub hub of the capacity check limits forks and sandboxes but not
  changes or event streams; the test asserts that exactly 2,000 changes
  were opened and none left open, and that at most 500 streams were open at
  once. A capacity result's `samples` is the count it measured.
