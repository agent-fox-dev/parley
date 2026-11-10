---
spec_id: "47"
spec_name: "sandbox_lifecycle_and_checkpoints"
title: "Stop, start, expire and checkpoint sandboxes"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "f1adadfe149a950dcc4f4e61702773f9a4957192fec19db991a97cabe7c15703"
schema_version: 3
source: "docs/prd/13_sandboxes.md"
---

# Stop, start, expire and checkpoint sandboxes

## Intent

A sandbox costs compute while it runs and holds an agent's work while it lives, so the hub must be able to end one without losing that work and without trusting the runtime to do it. This package gives every sandbox a governed life after creation: it can be stopped and started again, it is stopped when idle and deleted at its time limit, it goes when its workspace goes, the git state of its working directory is saved to the workspace before it loses its compute, and the hub's records are brought back in line with the runtime after any interruption, so that a hub restart ends no sandbox and a runtime failure loses no record.

## Goals

- A checkpoint stores, under `refs/parley/sandboxes/<sandbox id>/<branch>` in the sandbox's workspace, every local branch with commits the hub lacks and the uncommitted changes of the checked-out branch, and is recorded on the sandbox as `{at, ok, error, refs}`.
- A checkpoint is taken before a stop, before a deletion for any reason except `workspace_deleted`, when the main process ends and on request; its failure never blocks the stop or the deletion.
- `POST …/stop`, `POST …/start`, `DELETE …`, `POST …/checkpoint` and `PATCH …` on a sandbox answer as §13.5 of the source PRD states, with the listed errors.
- A `ready` sandbox idle for longer than its idle timeout is stopped with reason `idle`; every sandbox is deleted at `expires_at` with reason `expired`, by the hub's own clock.
- Deleting a workspace deletes its sandboxes first, without a checkpoint; archiving one stops its `ready` sandboxes with a checkpoint, and they cannot be started while it stays archived.
- A sandbox started again, or created with `restore_from`, finds the checkpointed branches and the uncommitted changes in its working directory.
- At start and every `reconcile_interval` the hub's records agree with the runtime again: exits are recorded, lost sandboxes are marked `failed`, runtime leftovers are removed, overdue idle stops and expiries are done.
- Stopping or restarting the hub makes no runtime call that stops or deletes a sandbox.
- `deleted` records are removed 24 hours after deletion.
- The commands `parley sandbox stop|start|extend|delete|checkpoint` exist.

## Non-goals

- The `[sandbox]` configuration (including `reconcile_interval`, `idle_timeout`, `max_ttl`), the start-up runtime check, profiles, the runtime client over the OpenShell SDK and its fake, the order of checks on sandbox routes and their authorization (SBX-FR-001..015), `runtime_reachable`: package 44 sandbox_runtime_and_profiles. This package calls the client by the operation names that package declares, adding none, and uses its route guard, its runtime-error answer and its metrics recorder.
- The sandbox record and its state machine, creation, quotas, resolution of credentials and exposed secrets, minting the hub credential, the checkout, the durable provisioning work after a create or a start, validation of `restore_from` (SBX-FR-027), `GET …/sandboxes/:id` and the list, `parley sandbox create|list|get`: package 45 sandbox_creation_and_credentials. This package adds transitions to that record and hands the seeding of a working directory to that provisioning work. It runs the delete hooks and purge hooks that package declares, registers its restore as that package's restorer, and scrubs runtime error text with that package's scrubbing function.
- Exec, files, logs, attach, activity tracking (`last_activity_at`), ending open exec, attach, log and transfer requests at shutdown, and killing commands of a closed exec request (the request half of SBX-FR-060 and SBX-FR-063): package 46 sandbox_exec_files_and_logs.
- Network policy, denials, proposals, and marking a deleted sandbox's open proposals `withdrawn` (SBX-FR-053): package 48 sandbox_network_policy. That package registers with the delete hooks of package 45, which this package runs. The policy of a start is composed by the policy composer that package registers with package 45; this package does not call it.
- When a fork is deleted or expires, and the early-expiry rule (WS-FR-070, WS-FR-078): package 16 task_fork_lifetime. Archive and delete of a workspace: package 14 workspace_archive_reclone_delete. This package supplies the sandbox remover, the archive observer and the remover of owned data, and registers them only in the `Teardown` set of package 14; a fork that expires or is deleted reaches them through package 14's `RemoveOwned`, which package 16 calls.
- The metric catalogue and `/metrics`: package 51 audit_query_and_retention. This package reports `parley_sandbox_checkpoints_total{result}` to the metrics recorder that package 44 declares; `parley_sandbox_seconds` and `parley_sandboxes` are reported by the transition operation of package 45, through which every state change of this package goes.
- Serving checkpoint refs to `git fetch` and refusing pushes to them: packages 17 git_smart_http and 18 git_push_rules.

## Background

The repository holds no code yet; every file named here is new. The source PRD (`docs/prd/13_sandboxes.md`) describes the checkpoint in §13.3.6, the lifecycle in SBX-FR-040 to SBX-FR-046, reconciliation in SBX-FR-060 to SBX-FR-064, the routes in §13.5, the commands in §13.6, the background work in §13.7, the events in §13.8 and the edge cases in §13.10.

Package 44 delivers, in `internal/sandbox/`, the runtime client interface with a fake for tests and the route guard. Package 45 delivers the sandbox record with conditional state transitions, the credential and secret resolution step, the hub credential, the quota check and the provisioning work. This package adds to `internal/sandbox/`: `checkpoint.go`, `restore.go`, `stop.go`, `start.go`, `extend.go`, `delete.go`, `timers.go` (idle stop and expiry), `teardown.go` (workspace collaborators), `cleanup.go`, `reconcile.go`, each with its test file beside it; the five commands to `internal/cli/sandbox.go`; the wiring to `internal/hub/sandbox.go`, the file package 45 starts; and `test/e2e/sandbox_lifecycle_test.go`.

## Requirements

### Checkpoint (§13.3.6)

A checkpoint saves the git state of the working directory to the sandbox's workspace on the hub.

- For every local branch whose head commit the workspace's repository does not have, and for the checked-out branch when the working tree has uncommitted changes, the hub stores a ref `refs/parley/sandboxes/<sandbox id>/<branch>`. Uncommitted changes, including new files that git does not ignore, are saved as one extra commit on top of the branch head; `dirty` is then true. The branch, the index and the working tree inside the sandbox are left as they were.
- Files that git ignores and anything outside the working directory are not saved. An untracked file above 100 MiB is left out of the extra commit and listed in `checkpoint.error` as skipped; the checkpoint is still `ok`.
- The sandbox's `checkpoint` becomes `{at, ok, error, refs: [{branch, sha, dirty}]}`, and one `parley.sandbox.checkpoint` event is written per attempt with `ok`, `refs` and `error`, its actor the caller or `system`.
- Checkpoint refs are not versions of a change and trigger nothing: no change, version or push event results. They live as long as the workspace; a fork's go with the fork.
- A sandbox without a repository (`checkout: false`, or a working directory that is not a git repository) records `ok: true` with no refs. A sandbox with `hub_access: none` has no checkpoint at all.
- Every attempt adds one to `parley_sandbox_checkpoints_total{result}`: `ok` when refs were stored, `empty` when the checkpoint is `ok` without refs, `failed` otherwise. A sandbox with `hub_access: none` has no attempt.
- A checkpoint is best effort. A failure (the runtime cannot be reached, git fails in the sandbox, the attempt passes 5 minutes) is recorded with `ok: false` and the scrubbed reason in `checkpoint.error`, and never blocks the stop or deletion that asked for it.

On request: `POST /workspaces/:slug/sandboxes/:id/checkpoint` answers `200` with the sandbox and its new `checkpoint`. A sandbox that is not `ready` answers `409 sandbox_not_ready`. A failed checkpoint answers `502 sandbox_runtime_error` with the reason, and the failure is recorded.

### Access to the lifecycle routes (SBX-FR-010, SBX-FR-011, SBX-FR-013)

The five routes of this package need `sandboxes:write` and write access to the workspace. Sandboxes disabled: `404 sandboxes_disabled`. A PAT without the scope: `403 insufficient_scope`. A read-only member: `403 forbidden`. A caller without read access: `404 not_found`. An unknown id, an id of another workspace or a record already removed: `404 sandbox_not_found`. The admin token may stop and delete any sandbox.

### Stop (SBX-FR-040, SBX-FR-042, SBX-FR-045)

`POST …/stop` on a `ready` sandbox takes a checkpoint, releases the sandbox's compute at the runtime, revokes the hub credential and records `stopped` with reason `api` and `parley.sandbox.stop`. The runtime keeps the working directory. The answer is `200` with the sandbox. A stop of a sandbox in any other state changes nothing and answers `200` with the sandbox. When the runtime cannot be reached the answer is `502 sandbox_runtime_error` and the sandbox stays `ready` with its credential.

Every minute the hub stops each `ready` sandbox whose `last_activity_at` is older than its idle timeout, with reason `idle`; a timeout of 0 disables this. When a workspace is archived the hub stops the `ready` sandboxes of that workspace and of its forks, with a checkpoint and reason `archived`.

Every transition is conditional on the expected current state, so racing requests produce one transition and one event (§13.3.3).

### Start (SBX-FR-041, SBX-FR-045, SBX-FR-065)

`POST …/start` is allowed from `stopped`, `completed` and `failed`. It resolves the sandbox's credentials and exposed secrets again, applies the quotas of SBX-FR-025, mints a new hub credential, records `provisioning` with `parley.sandbox.start` and answers `200`. The provisioning work then starts the sandbox at the runtime, runs `command` again when there is one and marks it `ready`. If the runtime no longer has the working directory, it is rebuilt from the checkout and the latest checkpoint. A sandbox whose profile no longer exists can be started: it keeps its snapshot.

Failures, each leaving the sandbox as it was: `409 sandbox_deleted`; `409 workspace_archived` or `409 parent_archived` while the workspace or a fork's parent is archived; `422 credential_missing` when a required credential's secret or a required exposed secret no longer resolves; `503 secrets_unavailable` when the secret store cannot be read; `429 sandbox_capacity` with `limit`, `maximum` and `current`.

### Restore (§13.3.6, SBX-FR-027, SBX-FR-041)

A working directory seeded from a checkpoint is the checkout with each checkpointed branch restored and the uncommitted changes of the checked-out branch put back in the working tree, uncommitted. This happens for a new sandbox created with `restore_from` and for a start whose working directory is gone. A checkpoint without refs gives the plain checkout. When a checkpoint ref no longer exists (a reclone of the workspace discards them, §13.10) the sandbox ends `failed` with reason `checkout_failed`.

### Time limit (SBX-FR-022, SBX-FR-043)

`PATCH …/sandboxes/:id` changes `ttl`, `purpose` or `labels` and answers `200` with the sandbox. `ttl` is counted from now, is at least 1 minute and at most the profile's `ttl` as recorded at creation, and `expires_at` is cut to the fork's `expires_at` on a task fork. `purpose` is at most 200 characters; `labels` follow the label rules of §13.5. A violation is `400 invalid_request`; a deleted sandbox is `409 sandbox_deleted`.

At `expires_at` the sandbox is checkpointed and deleted with reason `expired`, whatever its state. The hub enforces this itself. When the sandbox is on a fork whose own `expires_at` has also passed, the fork's deletion applies: reason `workspace_deleted`, no checkpoint.

### Delete (SBX-FR-044, SBX-FR-045, §13.7)

`DELETE …/sandboxes/:id` takes a checkpoint (query `checkpoint`, a bool, default the profile's `checkpoint`), removes the sandbox and its credentials at the runtime, revokes the hub credential, records `deleted` with reason `api` and `parley.sandbox.delete`, and answers `204`, also when the sandbox was already deleted. If the runtime cannot be reached, the record becomes `deleted` at once, the hub credential is revoked on the hub, and reconciliation removes the runtime's remains later. The record stays readable for 24 hours; the hourly maintenance then removes it and its logs, and the id answers `404 sandbox_not_found`.

Deleting a workspace, including fork expiry, deletes its sandboxes first with reason `workspace_deleted` and no checkpoint, and the workspace's sandbox records and checkpoint refs go with it. A fork with a `provisioning` or `ready` sandbox is not deleted early after landing; this package answers that question for package 16.

### Main process end and reconciliation (SBX-FR-046, SBX-FR-061, SBX-FR-062)

At start and every `reconcile_interval` (default 1 minute) the hub compares its records with the runtime:

- a `ready` sandbox whose main process has exited is checkpointed and recorded `completed` (exit code 0) or `failed` (another code, reason `exit`) with `exit_code` and `parley.sandbox.exit`; its compute is released, and the record and the working directory stay until `expires_at`;
- a `ready` sandbox the runtime is restarting or reconnecting to shows `provisioning`, and returns to `ready` when the runtime reports it running again;
- a `provisioning` or `ready` record the runtime does not know becomes `failed` with reason `runtime_error` and `parley.sandbox.fail`;
- runtime sandboxes and providers that carry this hub's label and belong to no live record are deleted at the runtime;
- idle stops and expiries that became overdue while the hub was down are done.

While the runtime cannot be reached, a pass changes no state except by expiry, and the hub keeps running.

### Hub stop and restart, and runtime error text (SBX-FR-060, SBX-FR-063, SBX-FR-064)

Sandboxes are owned by the runtime. Stopping the hub ends its own background work and nothing at the runtime; after a restart a `ready` sandbox is still `ready` and its main process was not touched.

Runtime error text that this package stores, logs, emits or returns has resolved secret values and the hub credential replaced by `[redacted]` and is cut to 1,024 bytes, by the scrubbing function of package 45.

### Commands (§13.6)

| Command | Calls | Notes |
|---|---|---|
| `parley sandbox checkpoint <workspace-slug> <id>` | `POST …/checkpoint` | prints the sandbox |
| `parley sandbox stop <workspace-slug> <id>` | `POST …/stop` | prints the sandbox |
| `parley sandbox start <workspace-slug> <id>` | `POST …/start` | `--wait`, `--timeout`, `--poll-interval` |
| `parley sandbox extend <workspace-slug> <id>` | `PATCH …/sandboxes/:id` | `--ttl <duration>`, required |
| `parley sandbox delete <workspace-slug> <id>` | `DELETE …/sandboxes/:id` | `--no-checkpoint`; prints `{"id":"<id>","status":"deleted"}` |

They follow PLAT-FR-090 to PLAT-FR-095: a hub error is printed as received on stderr with exit status 1, a usage error exits 2.

### End-to-end flows

**Stop and start.** An agent with uncommitted work runs `parley sandbox stop demo--t1 <id>`. `parleyd` asks the OpenShell gateway to run git in the sandbox, stores the checkpoint ref in the fork, releases the compute, revokes the hub credential and records `stopped`. The agent runs `parley sandbox start demo--t1 <id> --wait`; `parleyd` mints a new credential, records `provisioning`, the sandbox becomes `ready`, and the CLI exits 0.

**Expiry and restore.** A sandbox reaches `expires_at`. `parleyd` checkpoints it, removes it at the OpenShell gateway and records `deleted` with reason `expired`. A developer creates a new sandbox on the same fork with `restore_from` and finds the branch and the uncommitted file in its working directory.

**Hub restart.** An operator stops `parleyd` while a sandbox runs; no runtime call is made. The main process exits while the hub is down. The operator starts `parleyd`; its first reconcile pass records `completed` with the exit code and a checkpoint, and removes a labelled runtime sandbox that has no record.

## Design Decisions

1. **`PATCH …/sandboxes/:id` belongs to this package.** The plan names no owner for the route; `parley sandbox extend` is its only command and is in this package's scope.
2. **The runtime is reached only through the client of package 44.** No OpenShell SDK symbol is called directly, and this package adds nothing to the interface or its fake. It calls `Exec`, `Download` and `Upload` (the git bundle of a checkpoint and of a restore), `StopSandbox`, `StartSandbox`, `RunMain`, `DeleteSandbox`, `DeleteProvider`, `GetSandbox` (whose status says whether the main process ended, with which code, and whether the working directory still exists), `ListSandboxes` and `ListProviders` (with labels, for reconciliation). An `unsupported` result of `StopSandbox` or `StartSandbox` is a runtime error: `502 sandbox_runtime_error` on the route, `provision_failed` in the provisioning work.
3. **A checkpoint travels as a git bundle through the runtime**, created by git inside the sandbox, read by the hub through the runtime's file transfer and fetched into the workspace's repository with `internal/gitrepo`. Why: pushes to hub-owned refs are refused and a checkpoint must not depend on the hub credential.
4. **"Commits the hub does not have"** means the branch's head commit is absent from the workspace's repository.
5. **A checkpoint attempt is limited to 5 minutes** and is all or nothing: refs are stored only when the whole transfer succeeded. Why: the PRD says a checkpoint never blocks a stop or deletion and gives no bound.
6. **A failed checkpoint keeps the `refs` of the last successful one** in `checkpoint.refs`, with `ok: false` and the new `at` and `error`. Why: those refs still exist, and a later restore must not lose them.
7. **Skipped large files leave `ok: true`** with `error` set to `skipped: ` and the paths.
8. **Uncommitted changes with no branch checked out** are saved under the branch name `HEAD`.
9. **Automatic checkpoints follow the profile's `checkpoint` flag** (stop, idle stop, archive, main process end, expiry, deletion); a request to `POST …/checkpoint` always takes one. The `checkpoint` query of `DELETE` overrides the flag for that deletion.
10. **A checkpoint before a deletion or an expiry is attempted only for a `ready` sandbox.** Other states have no running compute and took theirs at stop or at the main process's end.
11. **`hub_access: none`**: automatic checkpoints are skipped without an event; `POST …/checkpoint` answers `200` with `checkpoint` null.
12. **`POST …/checkpoint` on a `deleted` sandbox** answers `409 sandbox_not_ready`, like every other state but `ready`.
13. **Start on a `ready` or `provisioning` sandbox is a no-op** answered `200` with the sandbox, mirroring stop.
14. **Start clears `exit_code`, `state_reason` and `ended_at`.** The new hub credential has the scopes and paths recorded in `hub_access`, the sandbox's creator as owner and the sandbox's `expires_at` as expiry.
15. **Start resolves the names of the sandbox's stored snapshot**, required and optional as the profile had them at creation; a missing optional name is skipped and leaves the sandbox's `credentials` or `exposed_secrets`.
16. **Only start is refused on an archived workspace.** Stop, checkpoint, extend and delete stay available.
17. **The hub credential is also revoked when the main process ends and when reconciliation marks a sandbox `failed`.** Why: the compute is gone and a start mints a new one.
18. **The end of a main process is detected by the reconcile pass**, so within one `reconcile_interval`. Why: one mechanism covers exits while the hub runs and exits while it was down.
19. **Expiry** is checked at start, in every reconcile pass and by a timer set to the earliest `expires_at` of a live sandbox. A change of `ttl` also moves the hub credential's expiry to the new `expires_at`, through `SetTokenExpiryByID(tx, token_id, instant)`, the operation of `internal/identity` that sets the expiry of one token by its id, inside the caller's transaction. Package 06 owns and exports that operation, as it does the revocation of one token by id; this package adds nothing to `internal/identity` and edits no file of it.
20. **The pause of SBX-FR-062 writes no event in either direction.** The record remembers that the runtime caused the `provisioning` state, so the pass returns only such records to `ready` and leaves provisioning work of package 45 alone. The transition table lists `parley.sandbox.ready` for `provisioning` to `ready`; that event stays reserved for a finished create or start.
21. **A `provisioning` record counts as unknown to the runtime** only after the provisioning work recorded that it created the sandbox there.
22. **Workspace collaborators.** This package registers only with package 14, in its `Teardown` set of `internal/workspace/teardown.go` and from `internal/hub/sandbox.go`: the `SandboxRemover` (takes the slugs; deletes with reason `workspace_deleted` and no checkpoint, called before the removal step), an `ArchiveObserver` in its list of archive observers (takes the slug and the slugs of its forks, called after an archive committed) and a `Remover` of owned data (takes the transaction and one slug, deletes the slug's sandbox records and, for a fork, its checkpoint refs, and returns an error only, as package 14 declares it; it revokes no token). It registers no remover, observer or removal participant with `internal/fork`: package 16 keeps no registry of its own, and a fork removed by request, by expiry or with its parent reaches the remover of owned data through package 14's `RemoveOwned`, which package 16 calls. The two operations of the sandbox collaborator that package 16 declares (delete the sandboxes of a slug with a reason and a checkpoint switch; report whether a slug has a `provisioning` or `ready` sandbox) are exported from `internal/sandbox` with that method set, and the composition passes them to package 16 as that package describes. Events of these paths have actor `system`.
23. **Hooks for packages 46 and 48 are declared by package 45 and run here.** The delete hooks run in the transaction that marks a sandbox `deleted`, the purge hooks in the transaction that removes a record (the 24-hour cleanup and the removal with a workspace). Why: logs and proposals belong to packages that depend on package 45 and not on this one; the record is package 45's.
24. **"Older than 24 hours"** is measured from `ended_at` of the `deleted` record; a record deleted exactly 24 hours ago stays one more pass.
25. **The dirty branch on restore.** Uncommitted changes are put back when the branch checked out in the seeded working directory is the checkpoint's dirty branch; otherwise that branch is restored to the commit under the extra one and its changes stay in the checkpoint ref.
26. **`parley sandbox start --wait`** ends on `ready` and `completed` (successful) and on `failed`, `stopped` and `deleted` (unsuccessful), polling `GET …/sandboxes/:id`.
27. **Smoke checks run against the fake runtime of package 44**, in its host-directory mode, where `Exec` runs real git in a temporary directory. No OpenShell gateway is available to tests.
28. **A start re-uses package 45's functions**: the resolution step fed from the record's profile snapshot, the quota check (the same function creation calls), the minting helper for the hub credential and the provisioning work with the marker `start`, which calls `StartSandbox` with the policy value of the policy composer registered with package 45. Why: package 48, which registers the full composer, is not a dependency of this package.
29. **No second scrubbing function.** SBX-FR-064 is met with the scrubbing function of package 45, which already knows each sandbox's resolved values and its hub credential.
30. **Wiring is in `internal/hub/sandbox.go`**, the composition file of the sandbox area that package 45 starts, in the process composition that package 03 owns; nothing is wired anywhere else.

## Dependencies

- Package 45 sandbox_creation_and_credentials (draft): the sandbox record and its conditional transitions, resolution of credentials and exposed secrets, the hub credential (minting helper and revocation by id), the quota check, the provisioning work with its policy composer and restorer interfaces, the delete and purge hooks, the scrubbing function, `GET …/sandboxes/:id` and `internal/cli/sandbox.go`. Through it: package 44 (runtime client, fake, route guard, `[sandbox]` keys), package 15 (fork record and storage), package 39 (secrets), package 08 and 06 (minting, revoking one token by id, and the expiry update of one token), package 10 (`internal/gitrepo`, workspace state), package 03 (scrubber, maintenance task registry, start hooks, background work, shutdown) and package 02 (router, error envelope).
- Package 09 cli_foundation (draft): the command declaration, HTTP client, output, exit codes and `--wait`.
- Package 52 durable_event_log_and_stream (draft): writing an event in the transaction of its state change.
- Package 14 workspace_archive_reclone_delete (draft): the `Teardown` set with its `SandboxRemover`, `Remover` and `ArchiveObserver` entries, in which this package registers, and `RemoveOwned`, through which package 16 reaches the remover of owned data.
- Package 06 api_keys_and_access_tokens (draft): the operations that revoke one token by its id, `RevokeTokenByID(tx, token_id)`, and set the expiry of one token by its id, `SetTokenExpiryByID(tx, token_id, instant)`, which that package owns and exports from `internal/identity`.
- Package 16 task_fork_lifetime (draft): the sandbox collaborator interface whose two operations this package exports; nothing is registered with `internal/fork`.
- The `git` command, in the hub (2.38 or later) and in every sandbox image (§13.9, image contract).

## Glossary

- **Sandbox**: An isolated environment in which an agent runs, attached to a workspace (normally a task fork) and started with it checked out. It runs on OpenShell; the hub controls it (§13).
- **OpenShell**: The open-source sandbox runtime Parley's sandboxes run on. It provides the isolation, the policy format and the credential mechanism. The hub is a client of one OpenShell gateway.
- **Profile**: The operator's template for a sandbox, defined in configuration: image, resources, time limits, base policy, credentials and the secrets it exposes.
- **Checkpoint**: The git state of a sandbox's working directory, saved to the workspace under `refs/parley/sandboxes/<id>/<branch>`.
- **Hub credential**: The fork-bound PAT the hub mints for a sandbox so that it can reach the hub. The workload sees only its placeholder, in `PARLEY_API_KEY`.
- **Exposed secret**: A secret (§11) that a profile names in `expose_secrets`. Its real value is placed in the sandbox's environment, where code in the sandbox can read it. Off unless the operator lists the secret (§13).
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires: after 7 days by default, or 24 hours after all its changes have landed or been abandoned.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
- **Live record**: A sandbox record in any state but `deleted`.
