# Errata: 47_sandbox_lifecycle_and_checkpoints

Where the implementation of package 47
(`.specs/47_sandbox_lifecycle_and_checkpoints`) differs from what the
package says, or decides what it leaves open.

## A start is provisioned by package 45's provisioner, with its own steps

The package hands a start to "the provisioning work of package 45 with the
marker `start`". That work only took the `create` marker. It now takes both,
and a start runs steps of its own (`startSteps` in
`internal/sandbox/provision.go`). It clears nothing at the runtime before
them, since that would delete the stopped sandbox and its working
directory. It registers the providers again with the values resolved now,
calls `StartSandbox` with the composed policy and the environment instead of
`CreateSandbox`, waits until the sandbox runs, rebuilds the working
directory when the runtime reports it gone, and starts the main process
again. A start that fails keeps the runtime's sandbox and its working
directory: the hub asks the runtime to release the compute, best effort,
and ends the record `failed` with the step's reason and
`parley.sandbox.fail`, revoking the hub credential. Only a create's failure
removes what the runtime holds. Test:
`TestStartRoute/fails_a_start_the_runtime_refuses_and_keeps_its_runtime_sandbox`.

## A failed restore is `checkout_failed`

Package 45's provisioning failed every error of the restorer with
`provision_failed`. This package's restore failures (a checkpoint ref that no
longer exists, a git command that fails) end the sandbox with
`checkout_failed`, as the package asks. A missing restorer or an unreadable
source record stays `provision_failed`. Code: `internal/sandbox/checkout.go`;
test: `TestProvision_FailureAtEachStep/the_restorer_fails`.

## The expiry timer moves with every transition

The package re-arms the expiry timer "after a create, a start, a PATCH of
`ttl` and a delete". The loop re-arms it on the service's change notifier,
which every applied transition notifies, and after a change of `ttl`. A
create therefore re-arms it when its sandbox first moves to `ready` or
`failed`, not when the create commits. A sandbox held in `provisioning` past
its `expires_at` is deleted by the next minute's pass, at most a minute
late. Code: `internal/sandbox/timers.go` (`RunLifecycle`); test:
`TestLifecycleTimers`.

## The CLI's `--wait` durations are whole units

The checks of `parley sandbox start --wait` pass `--timeout 50ms` and
`--poll-interval 10ms`. The CLI's duration grammar, package 09's, is a whole
number of `s`, `m`, `h` or `d`, so those values are usage errors, as for
`parley sandbox create` ([erratum 45](45_sandbox_creation_and_credentials.md)).
The tests poll through the CLI's own test seam and use `--timeout 1s`; a
`--timeout 0` or `--poll-interval -1s` is still a usage error with no request.
Test: `TestSandboxStart_Wait`.

## A start is judged by the create's workspace guards

The package lists `409 workspace_archived` and `409 parent_archived` for a
start. The start applies the guards a create applies,
`workspace.RequireReady` and `fork.GuardUpdate`. A workspace under an
archive, a reclone or a delete is therefore answered `409 workspace_busy`,
and one whose clone failed `409 workspace_not_ready`, as a create would be.

## The runtime's pause is its `restarting` state

The package returns a sandbox to `provisioning` while the runtime is
"restarting or reconnecting to" it. The runtime client reports `pending`,
`running`, `restarting` and `stopped`, and no reconnecting state, so the
reconcile pass treats `restarting` as the pause and `running` as its end.

## The OpenShell SDK cannot start a sandbox with a policy

The OpenShell implementation answers `StartSandbox` with a policy value or
an environment `unsupported`
([erratum 44](44_sandbox_runtime_and_profiles.md)); SDK v0.1.9 still has no
start that takes either. Against a real gateway, every start therefore ends
`failed` with `provision_failed`, and no checkpoint is written, because a
checkpoint's exec sends input on stdin, which the SDK cannot carry. The
behaviour is tested against the in-memory runtime.

## A checkpoint is held to the maximum push size

The package bounds only the untracked files of the extra commit (100 MiB
each); it sets no bound on the commits of unpushed branches, which a
checkpoint carries into the workspace's repository as a push would, but
outside the git server and its `[git].max_push_size`. A workload could
fill the hub's disk with one large commit on a branch it never pushed.
A checkpoint's bundle is therefore held to the push's limit: the hub reads
at most `[git].max_push_size` bytes of it from the runtime, and at one
byte more stops reading, fails the checkpoint (`ok` false, `error`
`the checkpoint bundle exceeds the maximum push size (<n> bytes)`, the
refs of the last successful checkpoint kept, none stored) and removes the
temporary file. With the key at `"0"`, its default, there is no limit, as
there is none for a push the sandbox's hub credential could make. Code:
`internal/sandbox/checkpoint.go` (`download`), `Deps.MaxCheckpointSize`
set in `internal/hub/sandbox.go`; test:
`TestCheckpoint_IsHeldToTheMaximumPushSize`.

## A later checkpoint replaces an earlier ref its branch clashes with

The package keeps every ref a checkpoint stored for as long as the
workspace exists, under `refs/parley/sandboxes/<sandbox id>/<branch>`. Git
keeps no ref `a/b` beside a ref `a`, so a later checkpoint with a branch
`feature/a` could not store its refs beside the ref of a branch `feature`
an earlier checkpoint of the same sandbox saved (or the reverse), and every
later checkpoint of that sandbox failed. A later checkpoint therefore
deletes the earlier ref of the same sandbox whose name clashes with one of
its own, in a ref transaction just before it sets its own, since git
refuses both in one; when its refs cannot then be set, the earlier ref is
put back as it was, so a failed checkpoint still keeps the refs of the
last successful one. The ref layout stays as the package fixes it. The
deleted ref is never one the latest checkpoint names: two local branches
cannot clash either, so the earlier branch was deleted or pushed. Refs of
other branches, and of other sandboxes, stay. Code:
`internal/gitrepo/bundle.go` (`FetchBundle`, its `replace` prefix),
`internal/sandbox/checkpoint.go` (`transfer`); tests:
`TestCheckpoint_ReplacesTheEarlierRefItsBranchClashesWith`,
`TestFetchBundle_ReplacesARefBelowThePrefixThatClashes`.

## Smaller choices

- The extra commit of a checkpoint is authored and committed by the hub's
  signature (`parley <parley@localhost>`, `gitrepo.HubSignature`), with no
  hook and no signing, so it is made the same way in a clone without
  `user.name`.
- A checkpoint runs `cp` and `rm` in the sandbox besides git: `cp` seeds the
  temporary index from the real one, and `rm` removes the attempt's files.
- When the hub's own fetch of a checkpoint's bundle fails, `checkpoint.error`
  may name the temporary file on the hub that git read.
- Stops and deletions of one sandbox take turns in the process, besides
  being conditional on the state, so that racing stops make one runtime
  call.
- Every lifecycle route is a write route of package 44, which lets the admin
  token in as an owner: it may checkpoint, start and extend too, not only
  stop and delete.
- The sandbox remover goes on after a sandbox whose deletion cannot be
  recorded, and returns one error naming it. The record cleanup removes
  each record in a transaction of its own.
- The check of a restore from a checkpoint that is not `ok` and has no refs
  starts a sandbox whose working directory the runtime dropped, not a create:
  package 45 refuses `restore_from` naming a sandbox whose checkpoint failed.
