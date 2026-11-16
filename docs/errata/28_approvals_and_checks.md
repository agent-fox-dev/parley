# Errata: 28_approvals_and_checks

Where the implementation of package 28 (`.specs/28_approvals_and_checks`)
differs from what the package says, or decides what it leaves open.

## Composed in `internal/hub`, beside the policy

The package wires its routes, observer, evidence source and expanders
"where parleyd builds the policy and change components" and touches
`cmd/parleyd`. The hub is composed in `internal/hub` (ADR 01), and
`cmd/parleyd` is not touched. `internal/hub/policy.go` `composePolicy`
adds the schema step `policy.ReviewStep` after `policy.SchemaStep` and
registers the evidence source (`RegisterEvidence(comp.Facts())`); the five
routes come with the component's `Routes()`. `composePolicyEvaluation`,
the first part where the change component exists, adds the version
observer after the mergeability observer and registers the expanders
`approvals` and `checks`. The tables are a `store.Step` of
`internal/policy`, not a migration of `internal/store`.

## No table of version creators

The package keeps the creating credential of each version in a table of
its own, written by the version observer. The change component already
stores it with each version (`change_versions.credential_type`,
`credential_id`), and re-attributes it when a raced write takes over a
version (`RecordRacedVersion`), which tells no observer. A second copy
would miss that re-attribution. The counting rule reads the credentials
through `(*change.Component).ReadVersions(ctx, q, workspace, number)`,
added for it and declared on the `policy.Changes` port. The schema step
holds two tables, the verdicts (`change_approvals`) and the check results
(`change_checks`). The checks that look for creator rows read the
versions' credentials instead: the credential of a hub-created version is
empty, and a rolled-back version is absent.

## Names of credentials and events

The admin token's credential type is `admin_token` (its id `admin`), as
the change component names it, not `admin`; a check's `reported_by` and
an event's actor for it are `{admin_token, null, null}`.

`event.Input` has no action member: `approve`, `reject` and `check` follow
from the event type. The metadata of `parley.change.approve`,
`parley.change.reject` and `parley.change.check` carries `number` and
`ref` beside the members the package lists (`version`, `approval_id`,
`counts_for`; `version`, `name`, `status`), as every change event does
(erratum 23).

## An unknown change answers one constant body

The five routes answer a `:number` that is not a positive integer, or
names no change, with the constant `change.ErrChangeNotFound` (`no such
change`), as `POST …/policy/evaluate` does, not with the change routes'
message that names the number. A token bound to a task fork reading the
two lists on the fork's parent gets that same body for a change whose
source is not the fork, so the body says nothing about whether the
change exists.

## Concurrent writes need no retry

The package asks the writes to retry their transaction on a busy or
constraint answer, so that two verdicts of one user on one version, or two
reports of one check, both succeed. `Store.Tx` runs write transactions one
at a time, and the version, the workspace's status and the change's state
are read inside it, so the second write sees the first and replaces it;
there is no retry loop, and the unique keys are a backstop.

## The include expanders read outside the request's transaction

`change.Expander` is given no transaction. `ExpandApprovals` and
`ExpandChecks` read through `Store.Main()`, as the two lists do.

## The observer reads the version as stored

The version observer reads the recorded version through `ReadVersions` in
the version's transaction rather than using the version it is handed,
because the stack linker's `SetBase` may recompute `patch_equivalent_to`
after that value was built (erratum 24). It re-evaluates the change only
when it copied a verdict: the change component stores the landing before
it tells its observers, so the copies would otherwise not be counted.

A `SetBase` outside a recorded version (a retarget, or a dependency's base
moving) can recompute the current version's `patch_equivalent_to` without
recording a version. No observer runs then, so no verdict is carried for
that recomputation. Check results follow it, since they are looked up
through `patch_equivalent_to` when read.

## Evidence is removed with its workspace

Not in the package: the teardown that deletes a workspace also deletes
its verdicts and check results (`policy.WorkspaceDeleted`, ADR 07),
because a workspace registered again under the slug numbers its changes
from 1 again (erratum 22), and its change 1 would otherwise inherit the
evidence of the deleted one.

## Null members are refused

The package lists `comment`, `version`, `summary`, `url` and `details` as
optional. A member given as `null` is refused with `400 invalid_request`,
not read as absent.

## The second carried verdict of the rebase path

The rebase path gives the agent token to the workspace's owner and expects
two verdicts on version 1, the agent's and the owner's, to be carried. A
user has one verdict per version, so the owner's approval with an API key
replaces the one the owner's agent token gave. Only the owner can write to
the workspace, so the agent token cannot belong to another user. The
end-to-end test adds an organization member's verdict, which no approver
form of the policy names, as the second carried verdict.

## Tests

The package's checks describe a hub with a recording event writer and
users alice, bob and carol. The tests extend the policy package's one test
world (`internal/policy/helpers_test.go`, users `dana`, `mel` and `oz`,
workspace `api` in organization `o1`), read events from the hub's event
log, inject faults through `newWorld` adjusters and SQLite triggers, and
the CLI tests pass the stub hub with `--endpoint-url` and `--api-key`. The
end-to-end paths run on the harness's `demo` and its task fork `demo--t1`.
