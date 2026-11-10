[Parley PRD](00_overview.md) · §13

## 13. Sandboxes

### 13.1 Purpose and scope

A sandbox is where an agent runs. In Parley a sandbox is born attached to a workspace, normally a task fork: it starts with the fork checked out, it can reach the hub and nothing else unless an operator allowed more, it holds no real credential unless the operator exposed a named secret in its profile, and what the agent did in it is saved back to the fork before the sandbox goes away.

Parley does not implement isolation itself. Sandboxes run on **OpenShell**, an open-source sandbox runtime. OpenShell provides the isolation (filesystem, process and network confinement, with containers or micro-VMs underneath), the policy format, and the credential mechanism in which a workload sees only a placeholder and the real credential is added outside the sandbox, on requests to approved endpoints. Parley is the control plane on top. It decides who may have a sandbox and of which shape, connects the sandbox to the fork, the session and the secrets of §11, enforces lifetimes and quotas, saves work, and records what happened.

The division of work:

| Concern | Owner |
|---|---|
| Isolation, enforcement of filesystem, process and network policy, credential substitution, the sandbox's own lifecycle | OpenShell |
| Who may create a sandbox, with which profile; the checkout of the fork; the hub credential; which secrets become credentials and which are exposed as raw values; time limits, idle stop and quotas; checkpoints; approval of new network access and the workspace sandbox rules; events and audit | Parley |

An agent needs nothing but the REST API to use a sandbox: create, run commands, move files, read logs. No runtime-specific client is required.

The feature is optional. Without a `[sandbox]` section every sandbox route answers `404`, `error_type: sandboxes_disabled`.

In scope: configuration and start-up checks, profiles, credentials for sandboxes, the sandbox lifecycle, the checkout, exec, files, logs and attach, checkpoints, network policy, its proposals and workspace sandbox rules, exposed secrets, time limits, quotas, reconciliation with the runtime, events, metrics and the `parley sandbox` commands. Out of scope: building images; exposing services that run in a sandbox; GPU scheduling beyond passing a count; running policy checks of §8 in sandboxes (§17); any policy language of Parley's own.

### 13.2 User stories

**SBX-US-01 (Operator): enable sandboxes.** As an operator, I want to point the hub at an OpenShell gateway and declare profiles, so that agents get sandboxes only of shapes I control.
- A configuration error or an unreachable or incompatible gateway stops the start with one message that names the problem.

**SBX-US-02 (Agent): one call to a working environment.** As an agent, I want to create a task fork and a sandbox for it in one call, and find the fork checked out in the sandbox with git already able to push.

**SBX-US-03 (Agent): work without a runtime client.** As an agent, I want to run commands, read their output, and upload and download files through the REST API.

**SBX-US-04 (Developer): run the agent inside.** As a developer, I want to start a sandbox whose main process is my coding agent, detach, and later read its logs and exit code.

**SBX-US-05 (Operator): no real credentials inside by default.** As an operator, I want the model API key and every other secret to stay outside the sandbox unless I say otherwise, usable only for requests to the endpoints I named, so that code in the sandbox cannot read or send them elsewhere.
- A profile that lists nothing in `expose_secrets` puts no real value in its sandboxes.
- When a tool cannot work with a placeholder, I can list a secret in a profile's `expose_secrets`. Its real value is then in the sandbox's environment, and I accept that code in the sandbox can read it and send it wherever the network policy allows.
- The profile list, the sandbox object and the create event name the exposed secrets, never their values.

**SBX-US-06 (Operator): no egress by default.** As an operator, I want a sandbox to reach the hub and nothing else unless a profile allows it.

**SBX-US-07 (Agent, Developer): ask for more network access.** As an agent blocked by the network policy, I want my request for a new destination to reach a person, and as that person I want to approve or reject it with the risk assessment in front of me.
- When I approve, I can keep the rule for the workspace (`scope: workspace`), so that the next sandbox on it or on its forks does not have to ask again.
- I can list the rules kept for a workspace and delete one.

**SBX-US-08 (Agent): nothing is lost.** As an agent, I want uncommitted and unpushed work saved to the fork when my sandbox stops, expires or fails.

**SBX-US-09 (Operator): hub restarts are harmless.** As an operator, I want to restart or upgrade the hub without ending any sandbox.

**SBX-US-10 (Operator): fair use.** As an operator, I want limits per hub, per organization, per workspace and per user, idle sandboxes stopped, and every sandbox ended at its time limit.

**SBX-US-11 (Org member).** As an org member with read-only access to a workspace, I cannot create a sandbox on it. I can fork it and create a sandbox on my fork.

### 13.3 Concepts and data model

#### 13.3.1 Runtime

The hub is a client of one OpenShell gateway, configured in `[sandbox]`. The hub uses one OpenShell workspace of that gateway (`openshell_workspace`, default `parley`) and owns everything in it: no other client creates sandboxes, providers or policies there. People do not need an OpenShell account to use Parley's sandboxes.

Where sandboxes physically run (a container engine, Kubernetes, micro-VMs) is OpenShell's configuration, not Parley's. The hub requires a compatible OpenShell release (§13.9) and checks it at start.

#### 13.3.2 Profile

A profile is the operator's template for a sandbox. It is defined in configuration and fixed for the life of the hub process.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `name` | string | required | `^[a-z0-9][a-z0-9_-]{0,62}$`, unique |
| `default` | bool | false | At most one profile sets it |
| `description` | string | `""` | Shown in the profile list |
| `image` | string | the runtime's default image | OCI image reference. The hub does not build or pull images |
| `cpu` | string | `""` | CPU limit as the runtime understands it (for example `2`); empty means the runtime's default |
| `memory` | string | `""` | Memory limit (for example `4Gi`) |
| `gpu` | int | 0 | Number of GPUs requested |
| `ttl` | duration | required | Default and longest lifetime of a sandbox of this profile; at most `[sandbox].max_ttl` |
| `idle_timeout` | duration | `[sandbox].idle_timeout` | Stop the sandbox after this long without activity; `0` disables |
| `policy` | path | the built-in policy | An OpenShell policy file (YAML, OpenShell's schema). It is the sandbox's base policy |
| `credentials` | list of strings | `[]` | Names of entries in `[[sandbox.credentials]]` attached to every sandbox of this profile |
| `optional_credentials` | list of strings | `[]` | Attached when their secret exists; skipped otherwise |
| `expose_secrets` | list of strings | `[]` | Names of secrets (§11) whose real values are placed in the environment of every sandbox of this profile, each under its own name (§13.3.4). Empty means no raw secret reaches the sandbox |
| `optional_expose_secrets` | list of strings | `[]` | As `expose_secrets`, but a secret that does not exist is skipped instead of failing the creation |
| `hub_access` | enum | `fork` | `fork`: the sandbox gets a hub credential for its workspace. `none`: it gets none and cannot reach the hub |
| `env` | map of strings | `{}` | Fixed, non-secret environment for the main process and for every exec |
| `checkpoint` | bool | true | Save work at stop, expiry and deletion (§13.3.6) |
| `allow_proposals` | bool | true | Whether network access proposals are accepted from sandboxes of this profile |

**The built-in policy** allows no egress at all and lets the workload read the image and write only its working directory. The hub adds the hub rule and the rules of attached credentials (§13.3.4) to whatever base policy a profile names.

Parley defines no policy language. A `policy` file is read by OpenShell as it is; the hub validates it against the runtime at start and refuses to start if the runtime rejects it.

#### 13.3.3 Sandbox

| Field | Type | Meaning |
|---|---|---|
| `id` | string | `sbx_` + 32 lowercase hex characters |
| `workspace` | string | Workspace slug; normally a task fork |
| `session_id` | string or null | The agent session the sandbox belongs to (§14) |
| `profile` | string | Profile name at creation |
| `purpose` | string | Caller text, at most 200 characters |
| `state` | enum | `provisioning`, `ready`, `stopped`, `completed`, `failed`, `deleted` |
| `state_reason` | string or null | Why, for `stopped`, `failed` and `deleted` (table below) |
| `command` | string[] or null | The main process. Null means an idle sandbox that is driven by exec |
| `exit_code` | int or null | Exit code of the main process once it ended |
| `checkout` | object or null | `{path, ref, sha, branch}`: where the repository was checked out, from which ref, and the branch created for the work |
| `image`, `cpu`, `memory`, `gpu` | | Snapshot of the profile at creation |
| `credentials` | string[] | Names of the credentials attached, sorted. Never values |
| `exposed_secrets` | string[] | Names of the secrets whose real values are in the sandbox's environment (§13.3.4), sorted. Names only, never values. `[]` for a profile that exposes none |
| `workspace_rules` | string[] | Ids of the workspace sandbox rules that were added to this sandbox's policy when it was created (§13.3.7) |
| `hub_access` | object or null | `{scopes, paths, expires_at}` of the hub credential in the sandbox; null when there is none |
| `policy_revision` | int | Increases each time the sandbox's network policy changes |
| `expires_at` | timestamp | When the sandbox is deleted |
| `idle_timeout_seconds` | int | 0 when disabled |
| `last_activity_at` | timestamp | Last exec, attach, file transfer, or output of the main process |
| `checkpoint` | object or null | The latest checkpoint: `{at, ok, error, refs: [{branch, sha, dirty}]}` |
| `restored_from` | string or null | Id of the sandbox whose checkpoint seeded this one |
| `runtime_reachable` | bool | False while the hub cannot reach the runtime; the other fields then show the last known state |
| `labels` | map of strings | The caller's labels |
| `created_by` | object | `{actor_type, actor_id, username}` |
| `created_at`, `updated_at`, `ended_at` | timestamp | |

**States.**

| State | Meaning | Exec, files, attach |
|---|---|---|
| `provisioning` | The runtime is creating or restarting the sandbox, or the hub is preparing the checkout | refused, `409 sandbox_not_ready` |
| `ready` | Running | available |
| `stopped` | Compute released; the working directory is kept | refused |
| `completed` | The main process exited with code 0 | refused; files can still be downloaded |
| `failed` | The main process exited with another code, or the runtime failed | refused; files can still be downloaded when the runtime kept them |
| `deleted` | Gone. The record stays readable for 24 hours | refused |

**Transitions.**

| From | To | Trigger | `state_reason` | Event |
|---|---|---|---|---|
| (new) | `provisioning` | create | | `parley.sandbox.create` |
| `provisioning` | `ready` | runtime ready and checkout done | | `parley.sandbox.ready` |
| `provisioning` | `failed` | image, runtime or checkout failure; a required credential or exposed secret is missing | `provision_failed`, `checkout_failed`, `credential_missing` | `parley.sandbox.fail` |
| `ready` | `completed` | main process exited 0 | | `parley.sandbox.exit` |
| `ready` | `failed` | main process exited non-zero, or the runtime lost the sandbox | `exit`, `runtime_error` | `parley.sandbox.exit` or `parley.sandbox.fail` |
| `ready` | `stopped` | stop request; idle timeout; the workspace or its parent is archived | `api`, `idle`, `archived` | `parley.sandbox.stop` |
| `ready` | `provisioning` | the runtime is restarting the sandbox or reconnecting to it | | none |
| `stopped`, `completed`, `failed` | `provisioning` | start request | | `parley.sandbox.start` |
| any but `deleted` | `deleted` | delete request; `expires_at`; the workspace is deleted or the fork expires | `api`, `expired`, `workspace_deleted` | `parley.sandbox.delete` |

`deleted` is the only terminal state. Every transition is conditional on the expected current state, so racing paths produce one transition and one event.

#### 13.3.4 Credentials

By default a sandbox holds no real credential. The one exception is a secret the operator exposes on purpose (**Raw secrets are opt-in**, below). Each credential is an OpenShell **provider**: the workload sees a placeholder in an environment variable, and the runtime replaces it with the real value outside the sandbox, only on requests that go to an endpoint the provider's profile authorizes, from a program that profile lists.

**Configured credentials** connect a secret of §11 to a provider profile:

| Field | Type | Meaning |
|---|---|---|
| `name` | string | Unique; what profiles refer to |
| `provider_profile` | string | Id of an OpenShell provider profile known to the gateway (for example a model vendor's, or one the operator imported for an internal API) |
| `secret` | string | Name of a secret (§11), resolved for the sandbox's workspace along the chain of §11.3 |

The provider profile, not Parley, defines the environment variable name, the authorized hosts, ports and paths, and the programs that may use the credential.

**The hub credential.** With `hub_access: fork` the hub mints a PAT for the sandbox and attaches it as a provider whose profile authorizes exactly the hub's own API and git endpoints for the workspace. The PAT:

- is bound to the sandbox's workspace and owned by the user who created the sandbox;
- carries `git:read`, `git:write`, `changes:read`, `changes:write`, `sessions:write` and `audit:write`, each only if the creating credential holds it (the subset rule of §3; an API key holds all);
- inherits the `paths` limit of the creating credential;
- expires with the sandbox and is revoked when the sandbox is stopped or deleted. A start mints a new one.

The value is treated as a secret: no response, event or log contains it, and the workload sees only its placeholder.

**What the workload finds in its environment:**

| Variable | Value |
|---|---|
| `PARLEY_ENDPOINT_URL` | The hub's base URL |
| `PARLEY_WORKSPACE` | The sandbox's workspace slug |
| `PARLEY_REMOTE` | The workspace's git URL (§5) |
| `PARLEY_SANDBOX_ID` | The sandbox id |
| `PARLEY_SESSION_ID` | The session id, when the sandbox has one |
| `PARLEY_API_KEY` | The placeholder of the hub credential (absent with `hub_access: none`) |
| the variable each attached provider profile declares | That credential's placeholder |
| each name in the profile's `expose_secrets` and `optional_expose_secrets` that resolved | The real value of that secret |
| the profile's `env` and the caller's `env` | As given |

Names starting with `PARLEY_` or `OPENSHELL_` are reserved and refused in a profile's or a caller's `env`.

**Raw secrets are opt-in.** A profile that lists nothing in `expose_secrets` and `optional_expose_secrets` puts no real value in its sandboxes: a secret then reaches a sandbox only as a credential of the kind above. This is the default, and it is the recommended way.

A profile may list secret names in `expose_secrets`. For each one the hub resolves the secret and places its **real value** in the sandbox's environment, in a variable with the secret's name, for the main process and for every exec. This exists for tools that cannot work with a placeholder: a client that signs requests itself, a tool that checks the format of its key, a credential for a protocol the runtime cannot rewrite.

What this means MUST be plain to the operator who turns it on:

- **Code in the sandbox can read an exposed value.** That includes the agent, every program it starts, and anything it downloads and runs.
- **Code in the sandbox can send an exposed value wherever the sandbox's network policy allows**: to the hub, to every host of the profile's policy, of an attached credential and of an approved rule. Nothing ties an exposed value to an endpoint, as a provider ties a credential.
- **The value can end up in the work.** A program may write it to a file, and a checkpoint or a commit may then carry it into the fork.
- **The hub redacts only what it stores as text.** It replaces exact matches of exposed values with `[redacted]` in the sandbox logs it keeps and in the text of events and errors (SBX-FR-068). It does **not** redact the output of exec, the attach terminal, downloaded files or checkpoints.

Rules for exposed secrets:

- **Operator only.** Only a profile can expose a secret. No request field, label or caller `env` can.
- **Resolution.** A name resolves along the same chain as the secret of a credential (§11.3), with the same rule for forks: the parent step is skipped for a fork whose owner lacks write access to the parent. Values are read at creation and again at each start.
- **Missing values.** A name in `expose_secrets` that resolves nowhere fails the creation like a missing credential (`422 credential_missing`). A name in `optional_expose_secrets` is skipped.
- **Names.** A name MUST be usable as an environment variable (`^[A-Za-z_][A-Za-z0-9_]*$`). It MUST NOT start with `PARLEY_` or `OPENSHELL_`, be a reserved secret name of §11 (the origin, upstream and forge webhook credentials), equal a variable that a provider profile of the same sandbox profile declares, or be a key of the profile's `env`. A name may not be in both lists. A caller's `env` may not set a name the profile exposes (`400`).
- **Visibility.** The profile list shows `exposed_secrets`. The sandbox object shows `exposed_secrets`, names only. `parley.sandbox.create` lists them in its metadata. No response, event or log line of the hub contains a value.

The `env` maps are for non-secret configuration and are shown in responses.

#### 13.3.5 Checkout

When the sandbox's workspace has a repository and the profile has `hub_access: fork`, the hub prepares the working directory before the sandbox becomes `ready`:

- The repository is cloned from `PARLEY_REMOTE` into the working directory as a partial clone (file contents are fetched when needed, §5).
- `checkout.ref` selects the starting point (default: for a fork, its base branch; otherwise the default branch).
- For a fork, a new branch is created for the work and checked out. Its name is `checkout.branch`; the default is the fork's `name`. If the branch already exists in the fork it is checked out as it is.
- Git is configured to authenticate to the hub with the hub credential, and the commit identity is set to the creating user's name and e-mail.

A push of that branch from the sandbox then opens or updates a change by the rules of §5 and §7, with the sandbox's session attached.

`checkout: false` at creation skips all of this.

#### 13.3.6 Checkpoint

A checkpoint saves the git state of the working directory to the sandbox's workspace on the hub:

- For every local branch that has commits the hub does not have, and for the checked-out branch when the working tree has uncommitted changes, the hub stores a ref `refs/parley/sandboxes/<sandbox id>/<branch>`. Uncommitted changes, including new files that are not git-ignored, are saved as one extra commit on top; `dirty` is then true.
- Checkpoint refs are hub-owned (§5) and fetchable. They are not versions of a change and trigger nothing.
- A checkpoint is taken before a stop (requested or idle), before deletion for any reason except `workspace_deleted`, when the main process ends, and on request. It is best effort: a failure is recorded in `checkpoint.error` and never blocks the stop or deletion.
- Checkpoint refs live as long as the workspace. A fork's checkpoints go with the fork.
- A new sandbox can start from a checkpoint (`restore_from`): its working directory is the checkout with each checkpointed branch restored and the uncommitted changes of the checked-out branch put back in the working tree.

Files that git ignores, and anything outside the working directory, are not saved.

#### 13.3.7 Network policy and proposals

- A sandbox's **effective policy** has four parts:
  1. its profile's base policy;
  2. the hub rule (the hub's API and git endpoints, for the workspace's routes only);
  3. the rules contributed by each attached credential's provider profile;
  4. the **workspace sandbox rules** of its workspace as they were when the sandbox was created; for a sandbox on a task fork, those of the fork's parent.

  Rules approved for the sandbox itself are added on top while it lives. Everything else is denied.
- The filesystem and process parts are fixed at creation. The network part can change while the sandbox runs.
- A **denial** is a connection or request the runtime blocked. The hub reads denials from the runtime's log and reports the first denial of each destination per sandbox as an event.
- A **proposal** is a request from the workload for an additional network rule, made with the runtime's own mechanism. The hub lists open proposals with the runtime's risk findings (for example: the rule would let a credentialed program reach a new host). A proposal is approved or rejected by a person. The approval has a `scope`. With `sandbox`, the default, the rule is added to that sandbox's policy only. With `workspace`, it is added to that sandbox and also saved as a workspace sandbox rule. Neither changes the profile.
- A **workspace sandbox rule** is an approved rule saved on a repository workspace. It is added to the policy of every sandbox created later on that workspace or on one of its forks, whatever the sandbox's profile. It never reaches a sandbox that was created before it, and deleting it changes no sandbox that already has it. A fork has no rules of its own: a rule promoted from a fork's sandbox is saved on the fork's parent. A workspace has at most 50 rules.

| Proposal field | Type | Meaning |
|---|---|---|
| `id` | string | |
| `sandbox_id` | string | |
| `rule` | object | The proposed rule, in OpenShell's schema |
| `summary` | string | One line: program, host, port, and methods or paths when restricted |
| `findings` | object[] | The runtime's findings: `{severity, message}` |
| `state` | enum | `open`, `approved`, `rejected`, `withdrawn` |
| `scope` | enum or null | `sandbox` or `workspace` once approved; null otherwise |
| `workspace_rule_id` | string or null | Id of the workspace sandbox rule the approval saved, when `scope` is `workspace` |
| `decided_by`, `decided_at`, `comment` | | Set when decided |
| `created_at` | timestamp | |

| Workspace sandbox rule field | Type | Meaning |
|---|---|---|
| `id` | string | `sbr_` + 32 lowercase hex characters |
| `rule` | object | The rule, in OpenShell's schema, as it was approved |
| `summary` | string | The proposal's one-line summary |
| `findings` | object[] | The runtime's findings on the proposal, as they were acknowledged |
| `from_proposal` | object | `{sandbox_id, proposal_id, workspace}`: the proposal the rule came from and the workspace of its sandbox, which may be a fork |
| `created_by` | object | `{actor_type, actor_id, username}` of the approver |
| `created_at` | timestamp | |

### 13.4 Functional requirements

**Configuration and start-up**

- SBX-FR-001: The feature is on when the configuration has a `[sandbox]` section. When off, the hub runs no sandbox work, and stored sandbox records are left as they are.
- SBX-FR-002: Static validation of `[sandbox]` (§13.9) collects every failure and reports them together. Unknown keys are errors. No message contains a credential.
- SBX-FR-003: After static validation the hub contacts the runtime once, within 30 s: it authenticates, checks that the runtime's release is compatible, makes sure its OpenShell workspace exists, installs the provider profile for the hub credential, checks that every `provider_profile` named in `[[sandbox.credentials]]` exists, and validates every profile's `policy` file. Any failure stops the start with a message naming the key or profile.
- SBX-FR-004: The scopes `sandboxes:read` and `sandboxes:write` exist whether or not sandboxes are enabled.
- SBX-FR-005: After start, loss of the runtime never stops the hub. Sandbox reads answer from stored state with `runtime_reachable: false`; operations that need the runtime answer `502`, `error_type: sandbox_runtime_error`.

**Authorization**

- SBX-FR-010: Check order: steps 1–4 of the global order (§3) → feature enabled (`404 sandboxes_disabled`) → binding guard → PAT scope → workspace access → validation → state.
- SBX-FR-011: Reads (get, list, logs, file download, proposals list) need `sandboxes:read` and read access to the workspace. Everything else needs `sandboxes:write` and write access. A read-only member gets `403 forbidden` on a write route.
- SBX-FR-012 (no widening). A caller can run commands in a sandbox, attach to it, or write files into it only with a credential that covers the sandbox's hub credential: it holds every scope in `hub_access.scopes`, and its `paths` limit, if any, is no narrower than the sandbox's. Otherwise `403 insufficient_scope`. This keeps a weak token from acting through a sandbox that a stronger credential created.
- SBX-FR-013: The admin token cannot create a sandbox, because the hub credential needs a user as its owner (`403 forbidden`). It can read, stop and delete any sandbox.
- SBX-FR-014: Approving or rejecting a proposal needs write access to the workspace and a credential that is not a workspace-bound token (`403 token_bound`), so that an agent cannot approve its own request. Approving with `scope: workspace` and deleting a workspace sandbox rule need the same, on the workspace that holds the rule (SBX-FR-070).
- SBX-FR-015: `GET /sandbox-profiles` needs `sandboxes:read` and no workspace access; bound tokens may call it. `GET /sandboxes` needs the admin token.

**Creating a sandbox**

- SBX-FR-020: A sandbox is refused on an archived workspace (`409 workspace_archived`) and on a fork whose parent is archived (`409 parent_archived`).
- SBX-FR-021: `profile` defaults to the profile marked `default`; without one, an absent `profile` is `400` and lists the known names.
- SBX-FR-022: `ttl` defaults to the profile's and is at least 1 minute and at most the profile's. On a task fork, `expires_at` is cut to the fork's `expires_at`. Extending the fork (§4) does not extend the sandbox; `PATCH` does, within the same limits.
- SBX-FR-023: `command` is an argument list of at most 256 entries. `env` holds at most 64 entries of at most 8 KiB each, with reserved names refused. `labels` follows the label rules of §13.5. `session_id` is validated like every session reference (§14); when absent on a fork that has a session, the sandbox takes the fork's.
- SBX-FR-024 (credentials). Before anything is created, the hub resolves the secret of every credential in the profile's `credentials` along the chain of §11.3 for the workspace, as that section defines it for forks. A missing one is `422`, `error_type: credential_missing`, naming the credentials and the scopes searched. Missing `optional_credentials` are skipped. Values are read again at each start, so a rotated secret is used from the next start. The secrets a profile exposes are resolved in the same step (SBX-FR-065).
- SBX-FR-025 (quotas). The hub counts sandboxes in state `provisioning` or `ready`. Four limits apply, checked in this order:

  | Limit | Default | Counted over |
  |---|---|---|
  | `max_sandboxes` | 200 | the whole hub |
  | `max_per_org` | 100 | all workspaces of the organization of the sandbox's workspace, and their forks |
  | `max_per_workspace` | 50 | the workspace family: a workspace and its forks |
  | `max_per_user` | 20 | the sandboxes the creating user created, on any workspace |

  A request that would exceed one of them is `429`, `error_type: sandbox_capacity`. The error object names the limit that was hit in `limit` (`max_sandboxes`, `max_per_org`, `max_per_workspace` or `max_per_user`), with `maximum` (its configured value) and `current` (the count). When several are exceeded, the first in the order above is reported. Stopped sandboxes do not count. The count and the insert are atomic.
- SBX-FR-026: On success the hub stores the sandbox in state `provisioning`, answers `201`, and continues in the background: it registers the credentials with the runtime, creates the sandbox there with the effective policy, prepares the checkout, starts `command` if there is one, and marks the sandbox `ready`. A failure at any step ends in `failed` with the reason, and what was created at the runtime is removed.
- SBX-FR-027: `restore_from` names a sandbox of the same workspace that has a checkpoint; otherwise `400`. The new sandbox's working directory is seeded from it (§13.3.6).
- SBX-FR-028 (fork and sandbox in one call). `POST /workspaces/:slug/forks` (§4) accepts a `sandbox` object with the fields of the sandbox create request. The fork is created first, then the sandbox on it. The response carries both. If the sandbox request is refused, the fork is not created either. `session_id` and `purpose` default to the fork's.

**Using a sandbox**

- SBX-FR-030 (exec). A command runs in the sandbox as the workload user, in the working directory unless `workdir` says otherwise, with the sandbox's environment plus the request's `env`. The request waits for the command up to `timeout` (default 60 s, at most `[sandbox].max_exec_timeout`, default 30 min), then kills it and answers with `timed_out: true`. Output is returned up to 1 MiB each for stdout and stderr; more is dropped and `truncated` says so. `stdin` is at most 4 MiB.
- SBX-FR-031 (exec stream). With `Accept: text/event-stream` the answer is a stream of `stdout`, `stderr` and a final `exit` frame, without the 1 MiB cap. Closing the stream kills the command.
- SBX-FR-032: Exec is independent of the main process. Several execs may run at once (at most `[sandbox].max_concurrent_execs` per sandbox, default 8; beyond that `429`).
- SBX-FR-033 (files). Upload writes one file, or with `archive=tar` unpacks a tar stream, under a path inside the working directory. Download returns one file, or a tar stream of a directory. Paths that resolve outside the working directory are `400`, `error_type: path_outside_workdir`. Transfers are streamed and limited to `[sandbox].max_transfer_size` (default 1 GiB).
- SBX-FR-034 (logs). Logs are the output of the main process and the runtime's policy log for the sandbox (denials, credential use by provider name). They can be read as pages or followed as a stream. The hub keeps the last `[sandbox].log_retention` (default 7 d) of each sandbox's logs for the life of its record.
- SBX-FR-035 (attach). A WebSocket gives an interactive terminal on a new shell in the sandbox, with resize messages. It is meant for people. Closing it ends that shell only.
- SBX-FR-036 (activity). Exec, attach, file transfer and output of the main process each set `last_activity_at`. A running main process alone does not count as activity unless it produces output.

**Stop, start, time limits**

- SBX-FR-040 (stop). Stop takes a checkpoint, revokes the hub credential, and releases the sandbox's compute. The working directory is kept by the runtime. A stop of a sandbox that is not `ready` is a no-op answered with the sandbox.
- SBX-FR-041 (start). Start is allowed from `stopped`, `completed` and `failed`, subject to the quotas of SBX-FR-025. It mints a new hub credential, resolves secrets again, and runs `command` again if there is one. If the runtime no longer has the working directory, the hub rebuilds it from the latest checkpoint.
- SBX-FR-042 (idle). A `ready` sandbox whose `last_activity_at` is older than its idle timeout is stopped with reason `idle`.
- SBX-FR-043 (expiry). At `expires_at` the sandbox is checkpointed and deleted with reason `expired`, whatever its state. The hub enforces this itself and does not rely on the runtime. When a sandbox's expiry falls together with its fork's, the fork's deletion applies: reason `workspace_deleted`, no checkpoint (SBX-FR-045).
- SBX-FR-044 (delete). Delete takes a checkpoint unless the reason is `workspace_deleted`, removes the sandbox and its credentials at the runtime, revokes the hub credential, and marks the record `deleted`. It is idempotent. If the runtime cannot be reached, the record becomes `deleted` at once, the credentials are revoked on the hub side, and reconciliation removes the runtime's remains later.
- SBX-FR-045 (workspace deletion). Deleting a workspace, including fork expiry, deletes its sandboxes first. A fork with a `provisioning` or `ready` sandbox is not deleted early after landing (§4): early expiry waits until its sandboxes are stopped, ended or deleted. The fork's TTL still applies. Archiving a workspace (§4) stops the `ready` sandboxes of that workspace and of its forks, with a checkpoint and reason `archived`; they cannot be started while the workspace stays archived (`409 workspace_archived` or `parent_archived`).
- SBX-FR-046 (main process end). When the main process exits, the hub takes a checkpoint and records `completed` or `failed` with the exit code. The sandbox's compute is released; the record and the working directory stay until `expires_at`, so logs and files remain available.

**Network policy**

- SBX-FR-050: The hub rule and the credential rules cannot be removed or narrowed by a profile's base policy, and a base policy cannot grant the hub credential to any other endpoint.
- SBX-FR-051 (denials). The first denial of each (program, host, port) per sandbox emits `parley.sandbox.deny`; later ones are counted in the metric only. Denials are always visible in the sandbox's logs.
- SBX-FR-052 (proposals). The hub collects proposals from the runtime for sandboxes whose profile has `allow_proposals`, and emits `parley.sandbox.proposal` for each new one. Proposals from other sandboxes are rejected automatically.
- SBX-FR-053 (decision). Approving a proposal adds its rule to that sandbox's policy at the runtime and increases `policy_revision`. A proposal with a finding of severity `high` needs `acknowledge_findings: true` in the approval, else `409`, `error_type: findings_not_acknowledged`. This holds for both scopes. Rejecting records the decision. Both emit `parley.sandbox.proposal_decided`. Open proposals of a sandbox that is deleted become `withdrawn`.
- SBX-FR-054: Rules approved with `scope: sandbox` last for the sandbox's life, across stop and start. They are not copied to a sandbox restored from its checkpoint. A rule approved with `scope: workspace` is a workspace sandbox rule as well, so a restored sandbox, like every sandbox created later, gets it (SBX-FR-071).

**Runtime reconciliation**

- SBX-FR-060 (hub restart). Sandboxes are owned by the runtime and are not affected by a stop, crash or upgrade of the hub. Running commands started through a closed exec request are killed; the main process is not.
- SBX-FR-061 (reconcile). At start and every `reconcile_interval` the hub compares its records with the runtime: it updates the state of each sandbox, records the exit of main processes that ended while it was away, deletes runtime sandboxes and providers that carry this hub's label and belong to no live record, and marks as `failed` (`runtime_error`) any `provisioning` or `ready` record the runtime does not know. Work that became overdue while the hub was down (idle stops, expiry) is done then.
- SBX-FR-062 (runtime restart). While the runtime restarts or reconnects to a sandbox, the sandbox shows `provisioning` and the workload is paused. It returns to `ready` without action by the hub.
- SBX-FR-063 (shutdown). Stopping the hub ends open exec, attach, log and transfer requests and nothing else.
- SBX-FR-064 (scrubbing). Runtime error text that is stored, logged, emitted or returned has resolved secret values and the hub credential replaced by `[redacted]` and is cut to 1,024 bytes.

**Exposed secrets**

- SBX-FR-065 (resolution). Before anything is created, the hub resolves every name in the profile's `expose_secrets` and `optional_expose_secrets` exactly as SBX-FR-024 resolves the secret of a credential: along the chain of §11.3 for the workspace, without the parent step for a fork whose owner lacks write access to the parent, and never a reserved name. A name of `expose_secrets` that resolves nowhere is `422`, `error_type: credential_missing`, naming the secrets and the scopes searched, and nothing is created. A missing name of `optional_expose_secrets` is skipped. Values are read again at each start; a start with a missing required one answers `422 credential_missing` and the sandbox stays as it was.
- SBX-FR-066 (placement). Each resolved value is placed in the sandbox's environment under the secret's name, for the main process and for every exec and attach. A profile without `expose_secrets` and `optional_expose_secrets` places no real value. No request can add to the list: a caller's `env` that names an exposed secret is `400 invalid_request`.
- SBX-FR-067 (validation at start). Static validation (SBX-FR-002) refuses a profile whose exposed names break the name rules of §13.3.4: not usable as an environment variable, a reserved prefix, a reserved secret name of §11, a key of the profile's `env`, or a name in both lists. The runtime check (SBX-FR-003) also refuses a name that equals a variable declared by a provider profile attached to the same sandbox profile. Whether the secrets exist is not checked at start, because that depends on the workspace.
- SBX-FR-068 (visibility and redaction). `GET /sandbox-profiles` shows each profile's `exposed_secrets`. The sandbox object shows the names that were placed, in `exposed_secrets`. `parley.sandbox.create` carries them in its metadata. Values appear nowhere. The hub replaces exact matches of exposed values with `[redacted]` in the sandbox logs it stores and serves (SBX-FR-034) and in every text it writes to events, errors and its own log (SBX-FR-064). A value shorter than 8 characters is not matched, because it would mangle unrelated text. The hub does **not** redact exec output, the exec stream, the attach terminal, downloaded files, checkpoints or pushed commits. Code in the sandbox can read these values and send them wherever the sandbox's network policy allows.

**Workspace sandbox rules**

- SBX-FR-069 (promotion). The approval of a proposal accepts `scope`: `sandbox` (default) or `workspace`. With `workspace` the hub does what SBX-FR-053 says and, in the same step, saves the rule as a workspace sandbox rule of the sandbox's workspace, or of its parent when that workspace is a task fork. The proposal records `scope` and `workspace_rule_id`. Findings must be acknowledged as for any approval. A rule identical to one the workspace already has is not saved twice: the approval succeeds and names the existing rule.
- SBX-FR-070 (authority). `scope: workspace` needs what approval needs (SBX-FR-014), on the workspace that will hold the rule: write access to it and a credential that is not a workspace-bound token. For a proposal of a sandbox on a fork that means write access to the fork's parent; a fork owner who can only read the parent gets `403 forbidden` and may still approve with `scope: sandbox`. When the parent is archived the answer is `409 parent_archived`.
- SBX-FR-071 (application). When a sandbox is created, the hub adds the workspace sandbox rules that exist at that moment to its effective policy (§13.3.7) and records their ids in `workspace_rules`. The set is fixed for the sandbox's life, across stop and start. A rule saved later never reaches a sandbox created before it, and deleting a rule does not change a sandbox that has it. The hub rule and the credential rules still cannot be narrowed (SBX-FR-050). If the runtime rejects the policy that results for a profile, the sandbox ends `failed` with `provision_failed` and an error that names the rule.
- SBX-FR-072 (list, delete, limit). `GET /workspaces/:slug/sandbox-rules` lists the rules of a workspace; called with a fork's slug it lists the parent's, which are the ones that apply to the fork. It needs `sandboxes:read` and read access. `DELETE /workspaces/:slug/sandbox-rules/:rule_id` removes one, needs `sandboxes:write` and the authority of SBX-FR-070, and is `400 not_supported_on_fork` on a fork. A workspace has at most 50 rules; a promotion beyond that is `429`, `error_type: sandbox_rule_limit_reached`, and the proposal stays `open`. Deleting a workspace deletes its rules (§4).
- SBX-FR-073 (operator switch). With `[sandbox].allow_workspace_rules = false`, an approval with `scope: workspace` is `403`, `error_type: workspace_rules_disabled`, and nothing is decided. The switch stops promotion only: rules saved earlier stay listed, keep being added to new sandboxes, and can be deleted.
- SBX-FR-074 (events). Saving a rule emits `parley.sandbox.rule_add` and deleting one emits `parley.sandbox.rule_remove`, on the workspace that holds the rule, with normal workspace visibility (§14).

### 13.5 REST API

All routes follow the conventions of §3. The feature check of SBX-FR-010 applies to every route and is not repeated. Common errors: `401`; `403 insufficient_scope`; `403 forbidden`; `404 workspace not found`; `404`, `error_type: sandbox_not_found` (unknown id, an id of another workspace, or a record removed after 24 hours); `502 sandbox_runtime_error`.

**Labels.** At most 16 entries. A key is 1 to 63 characters of letters, digits, `.`, `_` and `-`, starting and ending with a letter or digit, and does not start with `parley.` or `openshell.`. A value is at most 256 bytes without control characters.

#### `GET /sandbox-profiles`

- **Purpose:** list the hub's profiles.
- **Response:** `200`, a list ordered by `name`. Each item has the fields of §13.3.2 except `policy` (replaced by `policy_summary`: the hosts the base policy allows), `env` (replaced by `env_keys`), and `expose_secrets` and `optional_expose_secrets` (replaced by `exposed_secrets`: `[{name, optional}]`, sorted by name), plus `ttl_seconds`. A profile that exposes nothing shows `exposed_secrets: []`.

#### `POST /workspaces/:slug/sandboxes`

- **Purpose:** create a sandbox.
- **Body:**

  | Field | Type | Default |
  |---|---|---|
  | `profile` | string | the default profile |
  | `command` | string[] | none (an idle sandbox driven by exec) |
  | `env` | map of strings | `{}` |
  | `checkout` | `false`, or `{ref?, branch?}` | checkout with defaults (§13.3.5) |
  | `restore_from` | sandbox id | none |
  | `ttl` | duration | the profile's |
  | `purpose` | string | `""` |
  | `labels` | map of strings | `{}` |
  | `session_id` | string | the fork's session, else null |

- **Response:** `201` sandbox object, state `provisioning`.
- **Errors:** `400` validation (including an `env` key that the profile exposes as a secret); `400 unknown_session`; `409 workspace_archived`, `parent_archived`; `422 credential_missing` (a credential's secret or an exposed secret is missing); `429 sandbox_capacity` with `limit`, `maximum` and `current` in the error object (SBX-FR-025):

  ```json
  {"error": {"code": 429, "message": "sandbox limit reached: max_per_org", "error_type": "sandbox_capacity", "limit": "max_per_org", "maximum": 100, "current": 100}}
  ```
- **Side effects:** `parley.sandbox.create`; later `parley.sandbox.ready` or `parley.sandbox.fail`.

#### `GET /workspaces/:slug/sandboxes`

- **Purpose:** list the workspace's sandboxes, newest first.
- **Query:** `state` (repeatable; default: every state but `deleted`), `session_id`, `label` (`key=value`, repeatable), `include_forks` (bool; on a parent, include its forks' sandboxes), `limit`, `cursor`.

#### `GET /workspaces/:slug/sandboxes/:id`

- **Purpose:** read one sandbox. **Query:** `wait_for` (`ready` or `ended`) with `wait` (seconds, at most 60) holds the request until the sandbox reaches that condition or the time passes.

#### `PATCH /workspaces/:slug/sandboxes/:id`

- **Purpose:** change `ttl` (counted from now, within SBX-FR-022), `purpose` or `labels`.
- **Response:** `200` sandbox object. **Errors:** `409`, `error_type: sandbox_deleted`.

#### `POST /workspaces/:slug/sandboxes/:id/exec`

- **Purpose:** run a command (SBX-FR-030 to SBX-FR-032).
- **Body:** `{"command": string[], "workdir"?: string, "env"?: map, "stdin"?: string, "stdin_base64"?: string, "timeout"?: duration}`.
- **Response:** `200 {"exit_code": int | null, "stdout": string, "stderr": string, "stdout_base64"?: string, "stderr_base64"?: string, "truncated": bool, "timed_out": bool, "duration_ms": int}`. Output that is not valid UTF-8 is returned in the `_base64` members. With `Accept: text/event-stream`: frames `stdout` and `stderr` (`{"data": string}` or `{"data_base64": string}`) and one `exit` (`{"exit_code", "timed_out", "duration_ms"}`).
- **Errors:** `409 sandbox_not_ready`; `403 insufficient_scope` (SBX-FR-012); `429`.
- A non-zero exit code is not an error: the answer is `200`.

#### `PUT /workspaces/:slug/sandboxes/:id/files`

- **Purpose:** upload (SBX-FR-033).
- **Query:** `path` (required), `archive` (`tar`), `mode` (octal, for a single file).
- **Body:** raw bytes (`application/octet-stream`) or a tar stream.
- **Response:** `200 {"path", "bytes", "entries"}`. **Errors:** `400 path_outside_workdir`; `409 sandbox_not_ready`; `413`.

#### `GET /workspaces/:slug/sandboxes/:id/files`

- **Purpose:** download a file, or a directory as a tar stream.
- **Query:** `path` (required), `archive` (`tar`, required for a directory).
- **Response:** `200` bytes. Allowed in `ready`, `completed` and `failed`. **Errors:** `404 path_not_found`; `400 path_outside_workdir`.

#### `GET /workspaces/:slug/sandboxes/:id/logs`

- **Purpose:** read or follow logs (SBX-FR-034).
- **Query:** `source` (`process`, `policy`; default both), `since` (timestamp), `tail` (lines), `follow` (bool; then the answer is an event stream), `limit`, `cursor`.
- **Response:** `200` list of `{time, source, stream, line}`; policy lines also carry `action` (`allow`, `deny`), `program`, `host`, `port` and, for credentialed requests, `credential`.

#### `GET /workspaces/:slug/sandboxes/:id/attach`

- **Purpose:** interactive terminal over WebSocket (SBX-FR-035).
- **Query:** `cols`, `rows`. Binary frames carry terminal bytes; a text frame `{"resize": {"cols", "rows"}}` resizes.
- **Errors:** `409 sandbox_not_ready`; `403 insufficient_scope` (SBX-FR-012).

#### `POST /workspaces/:slug/sandboxes/:id/checkpoint`

- **Purpose:** take a checkpoint now. **Response:** `200` sandbox object with the new `checkpoint`. **Errors:** `409 sandbox_not_ready`; `502` with the reason when the checkpoint failed.

#### `POST /workspaces/:slug/sandboxes/:id/stop`

- **Purpose:** stop (SBX-FR-040). **Response:** `200` sandbox object.

#### `POST /workspaces/:slug/sandboxes/:id/start`

- **Purpose:** start again (SBX-FR-041). **Response:** `200` sandbox object, state `provisioning`. **Errors:** `409 sandbox_deleted`; `422 credential_missing`; `429 sandbox_capacity`.

#### `DELETE /workspaces/:slug/sandboxes/:id`

- **Purpose:** delete (SBX-FR-044). **Query:** `checkpoint` (bool, default the profile's). **Response:** `204`, also when it was already deleted.

#### `GET /workspaces/:slug/sandboxes/:id/proposals`

- **Purpose:** list network access proposals. **Query:** `state` (default `open`).

#### `POST /workspaces/:slug/sandboxes/:id/proposals/:proposal_id/approve`

- **Purpose:** approve (SBX-FR-053), for this sandbox or also for the workspace (SBX-FR-069). **Body:** `{"comment"?: string, "acknowledge_findings"?: bool, "scope"?: "sandbox" | "workspace"}`. `scope` defaults to `sandbox`.
- **Response:** `200` proposal, with `scope` and, for `workspace`, `workspace_rule_id`.
- **Errors:** `400 invalid_request` (unknown `scope`); `403 token_bound`; `403 forbidden` (`scope: workspace` on a fork without write access to its parent); `403 workspace_rules_disabled`; `409 findings_not_acknowledged`; `409`, `error_type: proposal_decided`; `409 parent_archived`; `429 sandbox_rule_limit_reached`.
- **Side effects:** `parley.sandbox.proposal_decided`; with `scope: workspace` and a new rule, `parley.sandbox.rule_add`.

#### `POST /workspaces/:slug/sandboxes/:id/proposals/:proposal_id/reject`

- **Purpose:** reject. **Body:** `{"comment"?: string}`. **Response:** `200` proposal.

#### `GET /workspaces/:slug/sandbox-rules`

- **Purpose:** list the workspace sandbox rules (SBX-FR-072). With a fork's slug: the rules of the fork's parent.
- **Auth / scope:** `sandboxes:read`; read access to the workspace.
- **Query:** `limit`, `cursor`. Ordered by (`created_at`, `id`), oldest first.
- **Response:** `200` list of `{id, rule, summary, findings, from_proposal, created_by, created_at}` (§13.3.7).

#### `DELETE /workspaces/:slug/sandbox-rules/:rule_id`

- **Purpose:** delete a workspace sandbox rule. Sandboxes that already have the rule keep it (SBX-FR-071).
- **Auth / scope:** `sandboxes:write`; write access to the workspace; not a workspace-bound token. Works in any workspace state.
- **Response:** `204`.
- **Errors:** `400 not_supported_on_fork`; `403 token_bound`; `403 forbidden`; `404`, `error_type: sandbox_rule_not_found`.
- **Side effects:** `parley.sandbox.rule_remove`.

#### `GET /sandboxes`

- **Purpose:** list the sandboxes of every workspace. Admin token only.
- **Query:** `state`, `user`, `limit`, `cursor`. Newest first.

### 13.6 CLI

All commands follow the CLI conventions of §3.

#### `parley sandbox profiles`
- **Calls:** `GET /sandbox-profiles`.

#### `parley sandbox create <workspace-slug> [-- command...]`
- **Flags:** `--profile`, `--env key=value` (repeatable), `--no-checkout`, `--ref`, `--branch`, `--restore-from <id>`, `--ttl`, `--purpose`, `--label key=value` (repeatable), `--session <id>`, `--wait` (until `ready`, or until the main process ended when a command is given and `--wait-exit` is set), `--wait-exit`, `--timeout`, `--poll-interval`.
- **Calls:** `POST …/sandboxes`, then `GET …/sandboxes/:id` with `--wait`.
- **Exit codes:** 0; 1 hub error, `failed`, or wait timeout; with `--wait-exit`, 0 only for `completed`.

#### `parley sandbox list <workspace-slug>`
- **Flags:** `--state` (repeatable), `--session`, `--label`, `--include-forks`, `--limit`, `--cursor`.

#### `parley sandbox get <workspace-slug> <id>`
- **Calls:** `GET …/sandboxes/:id`.

#### `parley sandbox exec <workspace-slug> <id> -- <command...>`
- **Flags:** `--workdir`, `--env key=value` (repeatable), `--timeout`, `--stdin` (read stdin and send it), `--json` (print the response document instead of streaming).
- **Calls:** `POST …/exec` as a stream by default.
- **Output:** the command's stdout on stdout and stderr on stderr. This is one of the commands whose stdout is not JSON unless `--json` is given.
- **Exit codes:** the command's exit code when it ran (values above 125 are mapped to 125); 1 for a hub error or a timeout; 2 usage.

#### `parley sandbox shell <workspace-slug> <id>`
- **Purpose:** interactive terminal. **Calls:** `GET …/attach`.

#### `parley sandbox logs <workspace-slug> <id>`
- **Flags:** `--source process|policy`, `--since`, `--tail <n>`, `--follow`.
- **Output:** one JSON object per line.

#### `parley sandbox upload <workspace-slug> <id> <local-path> <sandbox-path>`
- **Purpose:** upload a file, or a directory as a tar stream. **Calls:** `PUT …/files`.

#### `parley sandbox download <workspace-slug> <id> <sandbox-path> <local-path>`
- **Purpose:** download a file, or a directory (unpacked locally). **Calls:** `GET …/files`.

#### `parley sandbox checkpoint <workspace-slug> <id>`
- **Calls:** `POST …/checkpoint`.

#### `parley sandbox stop <workspace-slug> <id>`
- **Calls:** `POST …/stop`.

#### `parley sandbox start <workspace-slug> <id>`
- **Flags:** `--wait`, `--timeout`, `--poll-interval`. **Calls:** `POST …/start`.

#### `parley sandbox extend <workspace-slug> <id>`
- **Flags:** `--ttl <duration>` (required). **Calls:** `PATCH …/sandboxes/:id`.

#### `parley sandbox delete <workspace-slug> <id>`
- **Flags:** `--no-checkpoint`. **Calls:** `DELETE …/sandboxes/:id`. **Output:** `{"id":"<id>","status":"deleted"}`.

#### `parley sandbox proposals <workspace-slug> <id>`
- **Flags:** `--state`. **Calls:** `GET …/proposals`.

#### `parley sandbox approve <workspace-slug> <id> <proposal-id>`
- **Flags:** `--comment`, `--acknowledge-findings`, `--scope sandbox|workspace` (default `sandbox`; `workspace` also saves the rule for the workspace, SBX-FR-069). **Calls:** `POST …/proposals/:proposal_id/approve`.

#### `parley sandbox reject <workspace-slug> <id> <proposal-id>`
- **Flags:** `--comment`. **Calls:** `POST …/proposals/:proposal_id/reject`.

#### `parley sandbox rules <workspace-slug>`
- **Purpose:** list the workspace sandbox rules. **Flags:** `--limit`, `--cursor`. **Calls:** `GET /workspaces/:slug/sandbox-rules`.

#### `parley sandbox rule-delete <workspace-slug> <rule-id>`
- **Calls:** `DELETE /workspaces/:slug/sandbox-rules/:rule_id`. **Output:** `{"id":"<rule-id>","status":"deleted"}`.

`parley fork create` (§4) takes `--sandbox-profile <name>` and an optional trailing `-- command...` to create the fork and its sandbox in one call (SBX-FR-028).

### 13.7 Background behaviour

| Activity | When | Effect |
|---|---|---|
| Runtime check | start | SBX-FR-003; fatal on failure |
| Reconcile | start, then every `reconcile_interval` (default 1 m) | SBX-FR-061 |
| Provisioning | after each create and start | SBX-FR-026; durable: resumed after a hub restart |
| Idle stop | checked every minute | SBX-FR-042 |
| Expiry | at each sandbox's `expires_at` | SBX-FR-043 |
| Denials and proposals | read from the runtime continuously | SBX-FR-051, SBX-FR-052 |
| Record cleanup | hourly maintenance (§3) | `deleted` records older than 24 hours and their logs are removed |

**Metrics** (§14; exported with value 0 when sandboxes are disabled):

| Metric | Type | Meaning |
|---|---|---|
| `parley_sandboxes{state}` | gauge | Sandboxes by state, without `deleted` |
| `parley_sandbox_creates_total{result}` | counter | `ready` or `failed` |
| `parley_sandbox_seconds` | histogram | Time from `ready` to stop, exit or deletion |
| `parley_sandbox_exec_total{result}` | counter | `ok`, `nonzero`, `timeout`, `error` |
| `parley_sandbox_denials_total` | counter | Blocked connections and requests |
| `parley_sandbox_checkpoints_total{result}` | counter | `ok`, `empty`, `failed` |
| `parley_sandbox_runtime_up` | gauge | 1 while the runtime is reachable |

### 13.8 Events

All sandbox events except the two rule events have `resource_type: "sandbox"`, `resource_id` = the sandbox id, `workspace` = the sandbox's workspace and `session_id` = the sandbox's. Metadata never contains a credential, a secret value or command output; exposed secrets appear by name only. Each event is written with the state change it reports (§14).

| Type | Actor | When | Metadata |
|---|---|---|---|
| `parley.sandbox.create` | caller | sandbox stored | `profile`, `purpose`, `command` (first argument only), `credentials`, `exposed_secrets` (names), `workspace_rules` (ids), `hub_access`, `expires_at`, `restored_from` |
| `parley.sandbox.ready` | `system` | became `ready` | `checkout` |
| `parley.sandbox.fail` | `system` | became `failed` other than by exit | `reason`, `error` (scrubbed) |
| `parley.sandbox.exit` | `system` | the main process ended | `exit_code` |
| `parley.sandbox.stop` | caller, or `system` | became `stopped` | `reason` (`api`, `idle`, `archived`) |
| `parley.sandbox.start` | caller | start accepted | |
| `parley.sandbox.checkpoint` | caller, or `system` | a checkpoint was attempted | `ok`, `refs` (`[{branch, sha, dirty}]`), `error` |
| `parley.sandbox.deny` | `system` | first denial of a destination (SBX-FR-051) | `program`, `host`, `port`, `method`, `path` |
| `parley.sandbox.proposal` | `system` | a proposal arrived | `proposal_id`, `summary`, `max_severity` |
| `parley.sandbox.proposal_decided` | caller | approved or rejected | `proposal_id`, `decision`, `acknowledged_findings`, `scope` (`sandbox`, `workspace`; null for a rejection), `rule_id` (when a workspace sandbox rule was saved or matched) |
| `parley.sandbox.delete` | caller, or `system` | became `deleted` | `reason` (`api`, `expired`, `workspace_deleted`) |
| `parley.sandbox.rule_add` | caller | a workspace sandbox rule was saved (SBX-FR-069). `resource_type: "sandbox_rule"`, `resource_id` = the rule id, `workspace` = the workspace that holds the rule (the parent, for a proposal from a fork), `session_id` = the sandbox's | `rule_id`, `summary`, `max_severity`, `from_proposal` (`{sandbox_id, proposal_id, workspace}`) |
| `parley.sandbox.rule_remove` | caller | a workspace sandbox rule was deleted by request. `resource_type: "sandbox_rule"`, `resource_id` = the rule id, `session_id` null | `rule_id`, `summary` |

The two rule events have normal workspace visibility (§14.3.7): every reader of the workspace sees them. Rules removed with their workspace emit no event of their own.

Exec, attach and file transfers emit no hub events. They are recorded in the audit log (§14) as agent telemetry of the sandbox's session when it has one: who ran which command, when, with which exit code, never the output.

### 13.9 Configuration

The `[sandbox]` table:

| Key | Type | Default | Rules |
|---|---|---|---|
| `runtime` | string | `openshell` | The only value |
| `gateway_url` | URL | required | The OpenShell gateway, `https://host[:port]`; `http` to a loopback host only |
| `tls_cert`, `tls_key`, `tls_ca` | path | `""` | Client certificate, key and the gateway's CA for mutual TLS. `tls_cert` and `tls_key`: both or neither |
| `token_file` | path | `""` | A file holding a bearer token for the gateway |
| `oidc_issuer`, `oidc_client_id`, `oidc_client_secret_file` | URL, string, path | `""` | OIDC client credentials with which the hub obtains and renews its token for the gateway; all three or none |

Exactly one way to authenticate to the gateway is configured: mutual TLS, `token_file`, or OIDC client credentials.

| Key | Type | Default | Rules |
|---|---|---|---|
| `openshell_workspace` | string | `parley` | The OpenShell workspace the hub owns |
| `max_ttl` | duration | `24h` | Ceiling for every profile's `ttl` |
| `idle_timeout` | duration | `30m` | Default idle timeout; `0` disables |
| `max_sandboxes` | int | `200` | Hub-wide limit on `provisioning` and `ready` sandboxes |
| `max_per_org` | int | `100` | Per organization: all its workspaces and their forks. At most `max_sandboxes` |
| `max_per_workspace` | int | `50` | Per workspace family: a workspace and its forks |
| `max_per_user` | int | `20` | Per creating user |
| `allow_workspace_rules` | bool | `true` | Whether an approved network rule may be promoted to a workspace sandbox rule. `false`: an approval with `scope: workspace` is `403` (SBX-FR-073) |
| `max_exec_timeout` | duration | `30m` | |
| `max_concurrent_execs` | int | `8` | Per sandbox |
| `max_transfer_size` | size | `1GB` | Per upload or download |
| `log_retention` | duration | `7d` | |
| `reconcile_interval` | duration | `1m` | 10 s to 10 m |
| `[[sandbox.profiles]]` | list of tables | at least one | §13.3.2 |
| `[[sandbox.credentials]]` | list of tables | none | §13.3.4 |

Compatibility: the hub states which OpenShell releases it supports in its release notes and `GET /version` (`sandbox_runtime: {supported, connected}`), and refuses to start against a gateway outside that range.

**Image contract.** A profile's image must contain `git` for the checkout and for checkpoints. Nothing else is required: no agent, no Parley component. An image that also contains the `parley` CLI can use it as it is, because `PARLEY_ENDPOINT_URL` and `PARLEY_API_KEY` are set.

Example:

```toml
[sandbox]
gateway_url = "https://openshell.internal:8443"
tls_cert = "/etc/parley/openshell-client.crt"
tls_key  = "/etc/parley/openshell-client.key"
tls_ca   = "/etc/parley/openshell-ca.crt"

[[sandbox.credentials]]
name = "model"
provider_profile = "anthropic"
secret = "ANTHROPIC_API_KEY"

[[sandbox.profiles]]
name = "agent"
default = true
image = "registry.example.com/agent:1.4"
cpu = "2"
memory = "4Gi"
ttl = "4h"
credentials = ["model"]
policy = "/etc/parley/policies/agent.yaml"   # an OpenShell policy file
# expose_secrets = ["NPM_TOKEN"]   # off unless listed: puts the REAL value of the secret NPM_TOKEN
#                                  # into the sandbox's environment. Code in the sandbox can read it
#                                  # and send it wherever the network policy allows (§13.3.4).
```

Fixed, not configurable: at most 50 workspace sandbox rules per workspace.

### 13.10 Edge cases and error handling

- **Secrets and the agent.** Code in a sandbox can use a credential only as its provider profile allows: for requests to the named endpoints, from the named programs. It cannot read the value, and a request that carries the placeholder anywhere else is sent without the credential or blocked. This is a property of the runtime; Parley's part is never to hand a real value to the sandbox by another path, except for the secrets a profile names in `expose_secrets`.
- **An exposed secret is readable inside.** Code in the sandbox reads it from the environment and can print it, write it to a file, commit it, or send it to any host the network policy allows. The hub redacts exact matches in stored logs and event text only; `exec` output that prints the value returns the value. An operator who needs the value kept from the workload uses a credential instead.
- **An exposed secret and a credential use the same secret.** Allowed. The workload then has both the placeholder variable of the provider and the real value under the secret's name; the protection of the credential is gone for that profile.
- **An exposed secret is rotated or deleted while a sandbox runs.** The running sandbox keeps the old value in its environment. The next start reads again; a deleted required one fails the start with `422 credential_missing`.
- **A fork owned by a read-only org member, with a profile that exposes a parent secret.** The parent step is skipped (§11.3), so the name resolves only from the fork, the organization or the user. If it resolves nowhere, creation is `422 credential_missing` for `expose_secrets` and proceeds without it for `optional_expose_secrets`.
- **A workspace sandbox rule and a sandbox that is already running.** The rule does not reach it. The sandbox whose proposal was promoted has the rule anyway, as a rule of its own. Other running sandboxes ask with a proposal of their own, or are replaced by new ones.
- **A workspace sandbox rule is deleted.** Sandboxes created while it existed keep it until they are deleted, across stop and start. Sandboxes created afterwards do not get it.
- **A workspace sandbox rule and another profile.** Rules apply to every later sandbox of the workspace family, whatever its profile. A rule approved for a sandbox of one profile may therefore let a credentialed program of another profile reach the host. The approver sees the runtime's findings for the proposing sandbox only; the rule list shows them.
- **`scope: workspace` by a fork owner without write access to the parent.** `403 forbidden`; the proposal stays `open`. The same approval with `scope: sandbox` succeeds.
- **Quota per organization and forks.** A fork counts toward its parent's organization and its parent's workspace family. One busy workspace can use at most `max_per_workspace` of its organization's `max_per_org`.
- **Fork expires while a sandbox runs.** The sandbox is deleted with the fork, without a checkpoint, because the fork that would hold it is going too. Pushed work that became a change is unaffected. A sandbox never outlives its fork (SBX-FR-022), so an agent that needs more time extends the fork first and then the sandbox.
- **A stopped sandbox whose secret was deleted.** Start fails with `422 credential_missing`; the sandbox stays `stopped`.
- **The secret store cannot be read** (no encryption key, or a value that cannot be decrypted, §11). Create and start answer `503 secrets_unavailable` and nothing is created or started.
- **Reclone of the workspace (§4).** A reclone discards the workspace's checkpoint refs. Sandboxes are not touched; their working directories still hold the work, and the next checkpoint writes the refs again.
- **Replay of a fork-create that carried a `sandbox`.** A replay with the same `Idempotency-Key` returns the fork and the sandbox as they are now and creates nothing.
- **Checkpoint of a sandbox without a repository** (`checkout: false`, or a working directory that is not a git repository) records `ok: true` with no refs.
- **Checkpoint of very large untracked files.** Files above 100 MiB each are left out of the uncommitted-changes commit and listed in `checkpoint.error` as skipped.
- **Exec while the main process is the only thing running.** Allowed; this is how a person inspects a running agent.
- **The caller disconnects during a non-stream exec.** The command is killed.
- **The runtime is unreachable at expiry.** The record becomes `deleted`, the hub credential is revoked on the hub, and reconciliation removes the sandbox when the runtime returns. The sandbox cannot reach the hub in the meantime because its credential is gone.
- **A proposal for a destination the base policy explicitly denies** can be approved only if the runtime accepts the resulting policy; otherwise the approval answers `409` with the runtime's reason.
- **Profile changed or removed after creation.** A sandbox keeps its snapshot, including across stop and start. A start of a sandbox whose profile no longer exists is allowed.
- **Two sandboxes on one fork.** Allowed. Each has its own working directory and its own checkout branch unless told otherwise; they meet only through the fork's branches.
- **`hub_access: none`.** There is no checkout, no hub credential and no checkpoint. Such a sandbox is a plain isolated environment tied to a workspace for access control and accounting.
