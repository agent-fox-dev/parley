---
spec_id: "45"
spec_name: "sandbox_creation_and_credentials"
title: "Create a sandbox attached to a workspace"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "79720ef72bafdb6e25dbe460b666ae1344df95079ac281d3f5dbc290e551e1ef"
schema_version: 3
source: "docs/prd/13_sandboxes.md"
---

# Create a sandbox attached to a workspace

## Intent

An agent needs a place to run that starts with its task fork checked out, can reach the hub and nothing else, and holds no real credential unless the operator chose to expose one. This package lets a caller create such a sandbox on a workspace in one request, alone or together with a new task fork: the hub checks the request against the operator's profile and quotas, resolves the profile's credentials and exposed secrets, mints the sandbox's own hub credential, and then brings the sandbox up at the runtime with the repository checked out. It matters because every other sandbox feature starts from a sandbox that was created with exactly the access its creator had and no more.

## Goals

- `POST /api/v1/workspaces/:slug/sandboxes` answers `201` with a sandbox object in state `provisioning`, and the sandbox reaches `ready` or `failed` in the background without a further request.
- Every refusal of a creation has one status and one `error_type` and leaves nothing behind: no sandbox record, no token, no event, no call to the runtime.
- The four quotas (`max_sandboxes`, `max_per_org`, `max_per_workspace`, `max_per_user`) are never exceeded, also under concurrent requests, and a refusal names the limit, its value and the current count.
- A sandbox's hub credential never carries a scope or a path the creating credential did not hold, and its value appears in no response, event or log line.
- A secret reaches a sandbox as a real value only when the profile names it in `expose_secrets` or `optional_expose_secrets`; responses and events show names only.
- A sandbox on a task fork becomes `ready` with the fork cloned into the working directory, a work branch checked out, git able to push to the hub and the creating user as commit identity.
- Provisioning survives a restart of the hub: a sandbox that was `provisioning` when the hub stopped ends in `ready` or `failed` after the next start.
- `POST /api/v1/workspaces/:slug/forks` with a `sandbox` object creates both or neither.
- `GET /api/v1/workspaces/:slug/sandboxes` and `GET /api/v1/workspaces/:slug/sandboxes/:id` read sandboxes, and `parley sandbox create|list|get` drive the three routes.

## Non-goals

- The `[sandbox]` configuration and its validation, the start-up runtime check, the runtime client interface and its fake, profiles and `[[sandbox.credentials]]`, the route guard that applies the feature check and the authorization rules of SBX-FR-010 to SBX-FR-015 (including the no-widening rule), `GET /sandbox-profiles`, `GET /sandboxes` and `parley sandbox profiles`: package 44 sandbox_runtime_and_profiles. This package registers its routes through that guard, calls that client by the operation names that package declares, and implements and registers the sandbox source that `GET /sandboxes` reads. The name rules for exposed secrets that are checked when the configuration is loaded and at the start-up runtime check (both halves of SBX-FR-067) are that package's too; this package applies exposed secrets per creation (SBX-FR-065, SBX-FR-066, SBX-FR-068).
- Exec, file upload and download, logs, attach, `last_activity_at` updates and the redaction of stored logs: package 46 sandbox_exec_files_and_logs. This package delivers the scrubbing function and the per-sandbox list of values that package uses.
- Stop, start, idle stop, expiry, delete, `PATCH /workspaces/:slug/sandboxes/:id` (`ttl`, `purpose`, `labels`) and `parley sandbox extend`, what happens when the main process ends, checkpoints and the restore of a working directory from a checkpoint, reconciliation with the runtime, the removal of `deleted` records after 24 hours, the sandbox remover and archive observer that workspace deletion and archive call, and revoking the hub credential at stop and delete: package 47 sandbox_lifecycle_and_checkpoints. This package validates `restore_from`, records `restored_from` and calls the restorer that package supplies; it exports the `ttl` rule and the state machine that package moves records through, and it declares the delete hooks and purge hooks that package runs.
- The effective network policy and its guarantees (SBX-FR-050), denials, proposals, approvals, and the workspace sandbox rules with their routes: package 48 sandbox_network_policy. This package asks a rule provider, which that package registers, for the rules of the workspace at creation and records their ids and bodies, and it asks the policy composer, which that package registers too, for the policy value of every provisioning.
- The fork-create route, its `sandbox` and `token` members and `parley fork create --sandbox-profile`: package 15 task_fork_creation. This package supplies the sandbox component that route calls.
- Secret storage and the resolution chain, including the parent step for forks and the reserved names: packages 39 secrets and 40 variables_and_resolution.
- The minting component, the binding guard and `parley.token.bind`: packages 06 api_keys_and_access_tokens and 08 workspace_bound_tokens.
- The session-linking rule and the `sandboxes` list of a session: package 49 agent_sessions_and_usage. The exposition of metrics at `/metrics`: package 51 audit_query_and_retention. The event log: package 52 durable_event_log_and_stream.
- Building or pulling images, exposing services of a sandbox, and GPU scheduling beyond passing the profile's count to the runtime.

## Background

The repository holds no code yet; every file named here is new. The source is `docs/prd/13_sandboxes.md`: §13.3.3 (the sandbox object and its states), §13.3.4 (credentials, the hub credential, exposed secrets), §13.3.5 (the checkout), SBX-FR-020 to SBX-FR-028 (creation), SBX-FR-065 to SBX-FR-068 (exposed secrets), the entries of §13.5 for the three routes, of §13.6 for the three commands, the provisioning row of §13.7, the events `parley.sandbox.create`, `parley.sandbox.ready` and `parley.sandbox.fail` of §13.8 and the creation cases of §13.10. It rests on `docs/prd/03_platform.md` (PLAT-FR-010 to PLAT-FR-019, PLAT-FR-080, PLAT-FR-090 to PLAT-FR-095), `docs/prd/11_secrets_and_variables.md` §11.3 (the resolution chain) and `docs/prd/04_workspaces_and_forks.md` WS-FR-082.

The code of this package lives in `internal/sandbox/` (the record and its store, the state machine, the create operation, quotas, credential resolution, the hub credential, the provisioner, the checkout, the read routes, the fork component, metrics), in `internal/cli/sandbox.go` (the commands), in `internal/hub/sandbox.go` (wiring, the start hook) and in `test/e2e/` (end-to-end tests). Tests live beside the code.

What it builds on: the runtime client interface, its fake, the profiles, the `[sandbox]` limits and the route guard of package 44 in `internal/sandbox/`; the workspace record, the state guard and the workspace object of packages 07 and 10; the fork record of package 15 and the sandbox component interface it defines in `internal/fork/`; the resolver of package 40 over the secrets of package 39 in `internal/secret/`; the minting component `MintBound` of package 08 in `internal/access/`; the revocation of one token by its id that package 06 owns in `internal/identity/`; the event component of package 52 in `internal/event/`; the scrubbing function in `internal/scrub/`, the start hooks and the store of package 03; the command foundation of package 09 in `internal/cli/`.

## Requirements

### The sandbox object and its states (§13.3.3)

A sandbox has these fields in every response: `id` (`sbx_` and 32 lowercase hexadecimal characters), `workspace` (slug), `session_id` (string or null), `profile`, `purpose`, `state`, `state_reason` (string or null), `command` (list of strings or null), `env` (the caller's `env`), `exit_code` (int or null), `checkout` (`{path, ref, sha, branch}` or null), `image`, `cpu`, `memory`, `gpu` (the profile's values at creation), `credentials` (sorted names), `exposed_secrets` (sorted names), `workspace_rules` (ids), `hub_access` (`{scopes, paths, expires_at}` or null), `policy_revision` (int, 0 at creation), `expires_at`, `idle_timeout_seconds` (0 when disabled), `last_activity_at`, `checkpoint` (object or null), `restored_from` (id or null), `runtime_reachable` (bool), `labels`, `created_by` (`{actor_type, actor_id, username}`), `created_at`, `updated_at`, `ended_at` (null until the sandbox ends).

States are `provisioning`, `ready`, `stopped`, `completed`, `failed` and `deleted`. `deleted` is the only terminal state. The transitions are: new to `provisioning` (create); `provisioning` to `ready` or `failed`; `ready` to `completed`, `failed`, `stopped` or `provisioning`; `stopped`, `completed` and `failed` to `provisioning` (start); any state but `deleted` to `deleted`. Every transition is conditional on the expected current state and is written together with its event, so that two racing paths produce one transition and one event. This package performs create, `provisioning` to `ready` and `provisioning` to `failed`; packages 46 to 48 perform the others through the same transition operation. `state_reason` after a failed provisioning is `provision_failed`, `checkout_failed` or `credential_missing`.

`runtime_reachable` is false while the runtime client reports the runtime unreachable; the other fields then show the stored state (SBX-FR-005).

**Stored with the record and not shown.** Packages 46, 47 and 48 read and write the record through the store of this package, so the record also keeps: the profile snapshot (`ttl`, `idle_timeout`, `checkpoint`, `allow_proposals`, `hub_access`, the profile's `env`, the base policy document, and the names in `credentials`, `optional_credentials`, `expose_secrets` and `optional_expose_secrets` with the provider profile of each credential), so that a sandbox outlives a change or the removal of its profile; a copy of the body of each workspace sandbox rule recorded at creation; the id of the hub credential's token; the creating user's id, the organization id and the family root slug; the provisioning marker (`create` or `start`), a mark that the sandbox was created at the runtime and a mark that the runtime, not a request, caused the state `provisioning` (SBX-FR-062, set and cleared by package 47); and the time the sandbox last became `ready`. Of the shown fields, package 46 writes `last_activity_at`, package 47 writes `checkpoint`, `exit_code`, `expires_at`, `purpose`, `labels` and `hub_access.expires_at`, and package 48 raises `policy_revision`; `workspace_rules`, `exposed_secrets`, `credentials` and `env` are written at creation (a start of package 47 may drop an optional name that no longer resolves).

**Hooks.** `internal/sandbox` keeps two lists that other sandbox packages register functions with: delete hooks, called with the transaction and the sandbox id in the transaction that marks a record `deleted`, and purge hooks, called the same way in the transaction that removes a record. Package 47 performs both steps and runs the lists; packages 46 (stored logs) and 48 (proposals and their rows) register. A hook's error fails the transaction.

### Creating a sandbox (SBX-FR-020 to SBX-FR-023, SBX-FR-026, SBX-FR-027)

`POST /api/v1/workspaces/:slug/sandboxes` needs `sandboxes:write` and write access to the workspace. The body members, all optional: `profile` (string), `command` (list of strings), `env` (map of strings), `checkout` (`false`, or `{ref?, branch?}`), `restore_from` (sandbox id), `ttl` (duration), `purpose` (string), `labels` (map of strings), `session_id` (string). A member that is `null` counts as absent; an absent body is `{}`; unknown members are ignored.

Defaults: `profile` is the profile marked `default`; `command` is null (an idle sandbox driven by exec); `env`, `labels` are empty; `purpose` is `""`; `ttl` is the profile's; `session_id` is the session of the fork when the workspace is a fork that has one, else null. `expires_at` is the creation time plus `ttl`; on a task fork it is cut to the fork's `expires_at` (SBX-FR-022). `idle_timeout_seconds` is the profile's idle timeout.

On success the hub stores the sandbox in state `provisioning` with the snapshot of the profile, writes `parley.sandbox.create` in the same transaction and answers `201` with the sandbox object. The event has resource type `sandbox`, resource id the sandbox id, the workspace, the sandbox's `session_id`, the caller as actor, and metadata `profile`, `purpose`, `command` (the first argument only, or null), `credentials`, `exposed_secrets` (names), `workspace_rules` (ids), `hub_access`, `expires_at`, `restored_from`.

Refusals, in the order in which they are decided (the first that applies wins); each leaves no sandbox record, no token, no event and no call to the runtime:

| Case | Status | `error_type` |
|---|---|---|
| sandboxes are not enabled | `404` | `sandboxes_disabled` |
| a PAT without `sandboxes:write` | `403` | `insufficient_scope` |
| no access to the workspace | `404` | `not_found` |
| read access only; or the admin token (SBX-FR-013) | `403` | `forbidden` |
| the body is not a JSON object; `profile` names no profile, or is absent while no profile is marked `default` (the message lists the known names, SBX-FR-021); `ttl` is not a duration, is shorter than 1 minute or longer than the profile's `ttl`; `command` is not a list of 1 to 256 non-empty strings; `env` is not a map of strings, has more than 64 entries, has an entry whose key and value together exceed 8,192 bytes, has a key that does not match `^[A-Za-z_][A-Za-z0-9_]*$`, starts with `PARLEY_` or `OPENSHELL_`, or is a name the profile exposes as a secret (SBX-FR-066); `labels` breaks the label rules; `purpose` is not a string or is longer than 200 characters; `checkout` is neither `false` nor an object with string members; `checkout.branch` is given on a workspace that is not a fork, or is not a valid branch name; `checkout.ref` does not resolve to a commit in the workspace's repository; `restore_from` does not name a sandbox of the same workspace whose latest checkpoint has `ok` true, or is given together with `checkout: false` or a profile with `hub_access: none` | `400` | `invalid_request` |
| `session_id` fails the session-linking rule of §14 | `400` | `unknown_session` |
| the workspace is archived | `409` | `workspace_archived` |
| the workspace is a fork whose parent is archived | `409` | `parent_archived` |
| the workspace is not `ready` | `409` | `workspace_not_ready` |
| the secret store cannot be read | `503` | `secrets_unavailable` |
| a required credential's secret or a required exposed secret resolves nowhere | `422` | `credential_missing` |
| a quota would be exceeded | `429` | `sandbox_capacity` |

**Labels.** At most 16 entries. A key is 1 to 63 characters of letters, digits, `.`, `_` and `-`, starts and ends with a letter or digit, and does not start with `parley.` or `openshell.`. A value is at most 256 bytes without control characters.

### Quotas (SBX-FR-025)

The hub counts sandboxes in state `provisioning` or `ready`. Four limits apply, checked in this order: `max_sandboxes` (the whole hub), `max_per_org` (all workspaces of the organization of the sandbox's workspace, and their forks), `max_per_workspace` (the workspace family: a workspace and its forks; a fork counts toward its parent's family), `max_per_user` (the sandboxes the creating user created, on any workspace). A request that would exceed one is `429 sandbox_capacity`; the error object carries `limit` (the name of the limit), `maximum` (its configured value) and `current` (the count), and the message is `sandbox limit reached: <limit>`. When several are exceeded, the first in the order above is reported. Sandboxes in other states do not count. The count and the insert are atomic: of concurrent requests, no more than the limit allows succeed.

### Credentials and exposed secrets (§13.3.4, SBX-FR-024, SBX-FR-065 to SBX-FR-068)

Before anything is created the hub resolves, for the sandbox's workspace along the chain of §11.3, the secret of every credential in the profile's `credentials` and `optional_credentials` and every name in `expose_secrets` and `optional_expose_secrets`. The chain is package 40's: for a fork whose owner lacks write access to the parent, the parent step is skipped.

- A required credential or a required exposed secret that resolves nowhere: `422 credential_missing`. The error object carries `credentials` (the names of the missing credentials, sorted), `secrets` (the names of the missing exposed secrets, sorted) and `scopes_searched` (the scopes of the chain that were searched, in order, from `workspace`, `parent`, `org`, `user`).
- A missing optional credential or optional exposed secret is skipped.
- The secret store cannot be read (no encryption key, or a value that cannot be decrypted): `503 secrets_unavailable`.

The sandbox object's `credentials` holds the names of the credentials that are attached and `exposed_secrets` the names that are placed, each sorted. A profile that lists nothing in `expose_secrets` and `optional_expose_secrets` places no real value. No request field, label or caller `env` can expose a secret.

Values are read again when the sandbox is provisioned, so the value used is the one stored at that moment. If a required one has gone missing by then, the sandbox ends `failed` with reason `credential_missing`.

No response, event or log line of the hub contains a secret value or the hub credential. Text the hub writes about a sandbox into an event, an error or its log has every exact occurrence of a resolved value of that sandbox (credential secrets, exposed secrets of at least 8 characters, the hub credential) replaced by `[redacted]` and is then cut to 1,024 bytes (SBX-FR-064, SBX-FR-068). An exposed value shorter than 8 characters is not matched.

### The hub credential (§13.3.4, PLAT-FR-080)

With a profile whose `hub_access` is `fork`, the hub mints a PAT for the sandbox in the transaction that stores the sandbox. The PAT is bound to the sandbox's workspace, owned by the creating user, carries those of `git:read`, `git:write`, `changes:read`, `changes:write`, `sessions:write` and `audit:write` that the creating credential holds (an API key holds all), inherits the `paths` limit of the creating credential, and expires at the sandbox's `expires_at`. The sandbox object shows `hub_access: {scopes, paths, expires_at}` with `scopes` sorted and `paths` null when there is no limit. Minting emits `parley.token.bind` (package 08). When minting fails the creation fails with `500 internal` and nothing is stored.

`hub_access` is null and no token is minted when the profile has `hub_access: none`, when the creating credential holds none of the six scopes, or when `[server].external_url` is empty.

### Provisioning (SBX-FR-026, §13.7)

After the creating transaction has committed, the hub provisions the sandbox in the background:

1. it resolves the secrets again;
2. it registers one provider per attached credential, and one for the hub credential, with the runtime;
3. it creates the sandbox at the runtime with the profile snapshot (`image`, `cpu`, `memory`, `gpu`), the working directory, the providers, the environment below and the policy value the registered policy composer returns for the sandbox. The default composer returns the base policy of the snapshot with the protected rules and nothing else; package 48 registers the composer that adds the recorded workspace sandbox rules and the approved rules. The protected rules are this package's: the hub rule (the hub credential's provider on the scheme, host and port of `[server].external_url`, for the API and git routes of the sandbox's workspace only) when the sandbox has a hub credential, then one rule per attached credential that names its provider, in the order of `credentials`. A composer's error fails the provisioning; when the runtime refuses the policy value, the composer is asked to explain the refusal and its text is the error of the failure;
4. it prepares the checkout (next section) and, with `restore_from`, has the working directory restored from the named sandbox's checkpoint;
5. it starts `command` when there is one;
6. it moves the sandbox from `provisioning` to `ready` and writes `parley.sandbox.ready` (actor `system`, metadata `checkout`) with that change.

The environment of the main process and of every exec, later entries winning over earlier ones on the same name: the caller's `env`; the profile's `env`; each exposed secret that resolved, under its own name, with its real value; the variable each attached provider profile declares, holding that credential's placeholder; `PARLEY_ENDPOINT_URL` (the hub's base URL, `[server].external_url`), `PARLEY_WORKSPACE` (the slug), `PARLEY_REMOTE` (the workspace's git URL), `PARLEY_SANDBOX_ID`, `PARLEY_SESSION_ID` (only when the sandbox has a session) and `PARLEY_API_KEY` (the placeholder of the hub credential). `PARLEY_ENDPOINT_URL`, `PARLEY_REMOTE` and `PARLEY_API_KEY` are absent when `hub_access` is null.

A failure at any step moves the sandbox from `provisioning` to `failed` with `state_reason` `credential_missing` (step 1), `checkout_failed` (step 4) or `provision_failed` (every other step, including an unreachable runtime and a provisioning that takes longer than 10 minutes), sets `ended_at`, removes the sandbox and the providers that were created at the runtime, revokes the hub credential, and writes `parley.sandbox.fail` (actor `system`, metadata `reason` and the scrubbed `error`) with the change. When the record is no longer `provisioning` at the end (it was deleted meanwhile), the provisioner changes nothing in the record, writes no event and removes what it created at the runtime.

Provisioning is durable. A sandbox that was being provisioned when the hub stopped is provisioned again from step 1 at the next start: what the earlier run left at the runtime is removed, the hub credential minted earlier is revoked and replaced by one with the same scopes, paths and expiry, and the sandbox ends in `ready` or `failed` with exactly one event.

Metrics, reported to the metrics recorder that package 44 declares: `parley_sandbox_creates_total{result}` counts provisionings that ended `ready` or `failed`; `parley_sandboxes{state}` is the number of stored sandboxes in each state except `deleted`; `parley_sandbox_seconds` observes, when a sandbox leaves `ready` for `stopped`, `completed`, `failed` or `deleted`, the seconds since it became `ready`.

### The checkout (§13.3.5)

The hub prepares the working directory before `ready` when the sandbox has a hub credential that carries `git:read` and the request did not say `checkout: false`. Otherwise `checkout` is null and no git command is run.

- The repository is cloned from `PARLEY_REMOTE` into the working directory as a partial clone without file contents (filter `blob:none`).
- `checkout.ref` selects the starting point; the default is the fork's base branch for a fork and the default branch otherwise.
- For a fork, a branch named `checkout.branch` (default: the fork's `name`) is created at the starting point and checked out. If the fork already has a branch of that name, it is checked out as it is.
- For a workspace that is not a fork, the starting point is checked out and no branch is created.
- Git is configured to authenticate to the hub's host with the hub credential, and `user.name` and `user.email` are the creating user's name and e-mail.

The sandbox object's `checkout` is `{path, ref, sha, branch}`: the working directory, the starting ref, the commit that is checked out, and the work branch (null when none was created). It is filled at creation with the commit the hub resolves then and updated at `ready` with the commit actually checked out. A failing git command ends the provisioning with `checkout_failed`.

### Reading sandboxes (§13.5)

`GET /api/v1/workspaces/:slug/sandboxes/:id` needs `sandboxes:read` and read access and answers `200` with the sandbox object. An unknown id, an id of a sandbox of another workspace and a removed record are `404 sandbox_not_found`. A `deleted` record is still answered. With `wait_for` (`ready` or `ended`) and `wait` (whole seconds, 1 to 60) the request is held until the condition holds or the time has passed, and then answers `200` with the sandbox as it is. `ready` holds when the state is anything but `provisioning`; `ended` holds when the state is `stopped`, `completed`, `failed` or `deleted`. One of the two parameters without the other, an unknown `wait_for`, or a `wait` outside 1 to 60 is `400 invalid_request`.

`GET /api/v1/workspaces/:slug/sandboxes` needs the same and lists the workspace's sandboxes, newest first (`created_at` descending, then `id` descending), in the list shape of PLAT-FR-014. Query: `state` (repeatable; default every state but `deleted`), `session_id`, `label` (`key=value`, repeatable; every given label must match), `include_forks` (bool; on a parent, also the sandboxes of those of its forks the caller can read), `limit`, `cursor`. An unknown `state`, a `label` without `=` and an `include_forks` that is not `true` or `false` are `400 invalid_request`.

### Fork and sandbox in one call (SBX-FR-028, WS-FR-082)

`POST /api/v1/workspaces/:slug/forks` with a `sandbox` object hands that object to the sandbox component of this package inside the fork's transaction. The component treats it as the body of a create request on the new fork, with the fork's `session_id` and `purpose` as defaults (a `purpose` longer than 200 characters is cut to 200), and applies every check of the create route that the fork route has not made: the `sandboxes:write` scope, validation, credentials and quotas. The fork's creator has write access to the fork. The component returns the sandbox object, which the fork route answers in `sandbox`, or the refusal with its status, `error_type` and extra members, on which the fork is not created either. Provisioning starts only after the fork's transaction has committed; when it is rolled back there is no sandbox, no hub credential and no call to the runtime. When sandboxes are not enabled no component is wired, and package 15 answers `404 sandboxes_disabled`.

### CLI (§13.6)

All commands follow the conventions of PLAT-FR-090 to PLAT-FR-095.

- `parley sandbox create <workspace-slug> [-- command...]`. Flags: `--profile`, `--env key=value` (repeatable), `--no-checkout`, `--ref`, `--branch`, `--restore-from <id>`, `--ttl`, `--purpose`, `--label key=value` (repeatable), `--session <id>`, `--wait`, `--wait-exit`, `--timeout`, `--poll-interval`. It sends `POST /workspaces/<slug>/sandboxes` with a member for each flag that was given and the trailing arguments as `command`, and prints the sandbox object. With `--wait` it then polls `GET /workspaces/<slug>/sandboxes/<id>` until the state is no longer `provisioning`: `ready` and `completed` exit 0; `failed`, `stopped` and `deleted` print the record, report `wait_failed` and exit 1. With `--wait-exit` (which implies `--wait` and needs a command) it polls until the state is `completed` (exit 0) or `failed`, `stopped` or `deleted` (`wait_failed`, exit 1). Usage errors (exit 2, nothing sent): no slug; `--wait-exit` without a command; `--no-checkout` with `--ref` or `--branch`; an `--env` or `--label` value without `=`.
- `parley sandbox list <workspace-slug>`. Flags: `--state` (repeatable), `--session`, `--label` (repeatable), `--include-forks`, `--limit`, `--cursor`. Calls `GET /workspaces/<slug>/sandboxes` and prints the list document.
- `parley sandbox get <workspace-slug> <id>`. Calls `GET /workspaces/<slug>/sandboxes/<id>` and prints the sandbox object.

### End-to-end flows

1. **Create and wait.** A developer runs `parley sandbox create demo--task1 --wait -- make test`. The parley CLI posts to `parleyd`, which checks the caller, resolves the profile's credential, checks the quotas, stores the sandbox with its hub credential and the create event and answers `201`. `parleyd` then registers the providers at the OpenShell gateway, creates the sandbox there, clones the fork into the working directory, starts the command and marks the sandbox `ready`. The CLI, polling, prints the sandbox in state `ready` and exits 0.
2. **Fork and sandbox in one call.** An agent posts to `/workspaces/demo/forks` with `{"name": "task2", "sandbox": {}}`. `parleyd` creates the fork and, in the same transaction, the sandbox on it and its hub credential, and answers `201` with both. After the commit `parleyd` provisions the sandbox at the OpenShell gateway; the agent reads it with `wait_for=ready` and receives it in state `ready` with the branch `task2` checked out. The same request with a profile whose required secret is missing answers `422 credential_missing` and leaves no fork.

## Design Decisions

1. **Profile: full.** Two end-to-end flows cross the CLI, the router, the sandbox component, the store, the secret resolver, the minting component and the runtime client.
2. **Project-wide choices relied on.** Go; the store is embedded SQLite behind `internal/store`; HTTP is `net/http`; the CLI uses `github.com/spf13/cobra`; the runtime is reached only through the client interface of package 44, never through the OpenShell SDK directly; tests are table-driven with `testing` and `net/http/httptest`; code takes a replaceable clock.
3. **Additional dependencies.** Beyond the plan's list (44, 15, 39, 08) this package depends on 40 (it calls the resolver that package delivers), 52 (it emits three events), 09 (it has commands) and 06 (it revokes one token by id, decision 34).
4. **The hub credential is minted in the creating transaction and its value is never stored.** PLAT-FR-080 says one is minted when the sandbox is created. The value is handed to the provisioner in memory. A provisioning that is resumed after a restart no longer has it, so it revokes that token and mints a replacement with the same scopes, paths and expiry on behalf of the owning user. Why: storing a token value would add a secret at rest that nothing else needs.
5. **Secret values are not stored with the sandbox either.** They are resolved at creation to decide the refusals and again at provisioning to be used; a required one that vanished in between ends the sandbox `failed` with `credential_missing`, the reason the state table of §13.3.3 gives for it.
6. **How the provisioner finds its work.** The record carries a provisioning marker that create sets (and that package 47 sets at start) and that the provisioner clears when it ends. The provisioner looks for marked records at start (a start hook of package 03), when the create operation wakes it, and every second, because a sandbox created inside the fork route's transaction becomes visible only when that route commits. The state `provisioning` alone is not the marker, because a sandbox also shows `provisioning` while the runtime restarts it (SBX-FR-062).
7. **Create never answers `502`.** The request does not wait for the runtime; an unreachable runtime shows as `failed` with `provision_failed`. Why: SBX-FR-026 answers `201` before the runtime is called.
8. **A provisioning is bounded at 10 minutes** and then fails with `provision_failed`. The PRD gives no bound; work that calls another system needs one.
9. **A failed provisioning revokes the hub credential.** The PRD names stop and delete; a `failed` sandbox has nothing at the runtime that could use it, and a start mints a new one.
10. **Order of refusals after authorization**: validation, session link, workspace state, secret store unreadable, missing credentials, quotas. The PRD fixes validation before state (SBX-FR-010) and resolution "before anything is created"; quotas come last because their count is atomic with the insert.
11. **Workspace state.** The state guard of package 10 is applied: a workspace that is not `ready` is `409 workspace_not_ready`. SBX-FR-020 names only the archived cases.
12. **Shape of `credential_missing`.** The error object carries `credentials`, `secrets` and `scopes_searched`. The PRD says the error names the credentials and the scopes searched without giving member names.
13. **`env` rules beyond the PRD.** A key must be usable as an environment variable name; "8 KiB each" counts the bytes of key and value together; a `command` must have at least one argument and no empty argument.
14. **Precedence in the environment**: caller `env`, profile `env`, exposed secrets, provider placeholders, `PARLEY_*`, later ones winning. The PRD lists the variables without an order; the operator's and the hub's values must not be overridden by a caller.
15. **No hub credential without a scope or without a public URL.** When the creating credential holds none of the six scopes, or `[server].external_url` is empty, the sandbox is created as with `hub_access: none`. A token without permissions cannot be minted, and without a URL the sandbox has no address for the hub.
16. **The checkout needs `git:read`.** A hub credential without it cannot clone, so the checkout is skipped and `checkout` is null instead of failing every such sandbox.
17. **`checkout.branch` on a workspace that is not a fork is `400 invalid_request`.** §13.3.5 creates a branch only for a fork.
18. **`checkout.ref` is resolved at creation** against the workspace's repository, so a wrong ref is a `400` and not a failed sandbox.
19. **`restore_from` needs a checkout.** With `checkout: false` or `hub_access: none` it is `400`, because the restore is defined on the checkout (§13.3.6). "Has a checkpoint" means that the latest checkpoint has `ok` true.
20. **The working directory is the fixed path `/workspace`**, exported as one constant of `internal/sandbox` and passed to the runtime at creation, so that packages 46 and 47 use the same path. The PRD names no path.
21. **Git authentication inside the sandbox** is a git credential helper configured for the hub's host that answers with the username `x-token-auth` and the content of `PARLEY_API_KEY`; the image needs `git` only. The runtime replaces the placeholder as the hub provider profile of package 44 defines.
22. **A purpose taken from a fork is cut to 200 characters.** A fork's `purpose` may hold 1,024 characters (package 15) and is the default of the sandbox's, which holds 200.
23. **`wait_for` and `wait` are given together**, `wait` in whole seconds from 1 to 60. `wait_for=ready` also ends on an ended state, because such a sandbox cannot become `ready` without a start.
24. **`include_forks`** returns only sandboxes of forks the caller can read and is ignored on a fork.
25. **`--wait-exit` implies `--wait`.** `--wait` alone treats `completed` as success: the command ran and exited 0 before the first poll.
26. **Collaborators outside this package's dependencies are interfaces defined here**, each with a registration function, filled when their package exists: the session linker `SessionLinker` (package 49, which installs it; it has a check, given the caller, the session id and the workspace slug, and a record, given the creating transaction, the session id, the workspace slug and the sandbox id, through which the session's `sandboxes` list is kept; without one every `session_id` is `400 unknown_session`), the workspace rule provider (package 48; called in the creating transaction with the family root slug, returns the rules with id and body, oldest first; without one `workspace_rules` is empty), the policy composer (package 48; `Compose` returns the policy value of a sandbox or an error, `Explain` turns a refusal of the runtime into the error text; the default composes the base policy with the protected rules and returns the refusal unchanged) and the restorer (package 47; without one a provisioning with `restored_from` fails with `provision_failed`). Both the provisioning of a create and the provisioning of a start (package 47) call the registered composer, so package 47 does not depend on package 48.
27. **Metrics.** This package reports `parley_sandbox_creates_total`, `parley_sandboxes` (set from the store's counts after every insert, applied transition and removal) and `parley_sandbox_seconds` (observed by the transition operation, the one place every package leaves `ready` through) to the metrics recorder interface package 44 declares in `internal/sandbox`; it does not import `internal/metrics`. The recorder it is given is package 51's, the one recorder of `internal/metrics`, which satisfies package 44's interface and is passed in from `internal/hub/sandbox.go`; as long as package 51 is not built the composition passes package 44's default, which records nothing, so this package does not depend on package 51. The other sandbox metrics are reported by the packages whose behaviour they count (46, 47, 48).
28. **Runtime operations.** The operation names of the runtime client are package 44's, and this package adds none to the interface or its fake. It calls `EnsureProvider`, `DeleteProvider`, `ListProviders`, `GetProviderProfile` (for the variable a provider profile declares), `CreateSandbox`, `WaitReady`, `Exec` (the checkout), `RunMain`, `DeleteSandbox` and `Reachable`.
29. **The sandbox object shows `env`**, the caller's map. §13.3.4 says the `env` maps are shown in responses, while the field table of §13.3.3 has no such field.
30. **The sandbox source of `GET /sandboxes` (package 44) is implemented here** over the record store and registered with `RegisterSource`; it sets each object's `runtime_reachable` from the client's flag.
31. **Delete hooks and purge hooks are declared here**, in `internal/sandbox/hooks.go`, although package 47 runs them: the record is this package's, and packages 46 and 48, which register clean-up, depend on this package and not on package 47.
32. **One scrubbing function for sandbox text.** The function of this package (SBX-FR-068) is the one packages 46, 47 and 48 pass runtime error text and log lines through (SBX-FR-064); none of them writes a second one.
33. **The record keeps the whole profile snapshot** (decision recorded under "Stored with the record"), because a start (package 47) resolves the names of the snapshot, a checkpoint follows its `checkpoint` flag, an extension is bounded by its `ttl`, and proposals follow its `allow_proposals` (package 48), also after the profile changed or was removed.
34. **Revoking one token.** The hub credential is revoked by its token id. Package 08 revokes only all tokens bound to a set of workspaces, which would also end a fork's own token; this package therefore uses the revocation by id of `internal/identity`, `RevokeTokenByID(tx, token_id)`, inside its own transaction and lists package 06 as a dependency. Package 06 owns and exports that operation; this package adds nothing to `internal/identity`.
35. **One way to mint (with package 08).** The hub credential is minted only through `MintBound` of package 08, at creation and for every replacement, so that every hub credential is bound and has its `parley.token.bind` event. This package never calls the minting operation of package 06 directly.
36. **Wiring is in `internal/hub/sandbox.go`**, the file of the sandbox area in the process composition that package 03 owns; nothing is wired anywhere else.

## Dependencies

- Package 44 sandbox_runtime_and_profiles (draft): the runtime client interface and its fake, the profiles and configured credentials, the `[sandbox]` limits, the route guard with the feature check and the authorization rules.
- Package 15 task_fork_creation (draft): the fork record (`name`, base branch, `expires_at`, `session_id`, `purpose`) and the sandbox component interface its create route calls.
- Package 39 secrets (draft): the secret store whose values become credentials and exposed secrets, and `503 secrets_unavailable`.
- Package 40 variables_and_resolution (draft): the function that resolves a secret name along the chain of a workspace or fork.
- Package 08 workspace_bound_tokens (draft): the minting component `MintBound` for the hub credential (with the expiry given as an instant) and the binding guard on this package's routes.
- Package 06 api_keys_and_access_tokens (draft): the scope check, and the revocation of one token by its id inside the caller's transaction, `RevokeTokenByID(tx, token_id)`, which that package exports from `internal/identity`.
- Package 52 durable_event_log_and_stream (draft): writes `parley.sandbox.create`, `parley.sandbox.ready` and `parley.sandbox.fail` with their state changes.
- Package 09 cli_foundation (draft): the HTTP client, output, exit codes and the `--wait` helper of the commands.
- Used through them, not plan dependencies: the store, start hooks and `internal/scrub` of package 03, the access rule of package 07, the workspace record and state guard of package 10.
- Registered at start with interfaces of this package, not required to build or test it: 47 (restorer; it also runs the delete and purge hooks), 48 (workspace rule provider, policy composer, hooks), 46 (purge hook), 49 (session linker), 51 (its recorder in `internal/metrics`, which satisfies package 44's metrics interface and is passed in from `internal/hub/sandbox.go`).
- External system: one OpenShell gateway, reached only through package 44's client.

## Verified External API

- `github.com/spf13/cobra` v1.10.2, read in the module cache: `func (c *Command) Flags() *pflag.FlagSet`; `func (c *Command) ArgsLenAtDash() int`; `func MinimumNArgs(n int) PositionalArgs`.
- `github.com/spf13/pflag` v1.0.10, read in the module cache: `func (f *FlagSet) StringArray(name string, value []string, usage string) *[]string`; `func (f *FlagSet) Changed(name string) bool`.
- `github.com/ai-openshell/sdk-go` is not called by this package.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires.
- **Sandbox**: An isolated environment in which an agent runs, attached to a workspace (normally a task fork) and started with it checked out. It runs on OpenShell; the hub controls it (§13).
- **OpenShell**: The open-source sandbox runtime Parley's sandboxes run on. It provides the isolation, the policy format and the credential mechanism. The hub is a client of one OpenShell gateway.
- **Profile**: The operator's template for a sandbox, defined in configuration: image, resources, time limits, base policy, credentials and the secrets it exposes.
- **Exposed secret**: A secret (§11) that a profile names in `expose_secrets`. Its real value is placed in the sandbox's environment, where code in the sandbox can read it. Off unless the operator lists the secret (§13).
- **Credential (sandbox)**: A secret (§11) attached to a sandbox as an OpenShell provider. The workload sees only a placeholder; the runtime adds the real value outside the sandbox, on requests to the endpoints the provider's profile authorizes.
- **Hub credential**: The fork-bound PAT the hub mints for a sandbox so that it can reach the hub. The workload sees only its placeholder, in `PARLEY_API_KEY`.
- **Checkpoint**: The git state of a sandbox's working directory, saved to the workspace under `refs/parley/sandboxes/<id>/<branch>`.
- **Workspace sandbox rule**: An approved network rule saved on a workspace. It is added, on top of the profile's policy, to every sandbox created later on that workspace or its forks (§13).
- **Bound token**: A PAT confined to one workspace (and the forks created with it).
- **Session**: An agent session: who ran, on which workspace, with what token usage, and which changes it produced.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number.
