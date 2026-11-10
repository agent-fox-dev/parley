---
spec_id: "44"
spec_name: "sandbox_runtime_and_profiles"
title: "Connect the hub to the sandbox runtime and define profiles"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "f79c4331076a76e21b93fdc1fddca3fc18296e175228e4fa6b5fa390db363e61"
schema_version: 3
source: "docs/prd/13_sandboxes.md"
---

# Connect the hub to the sandbox runtime and define profiles

## Intent

Sandboxes are optional in Parley and run on OpenShell, not in the hub. This package is what turns the feature on safely: an operator points the hub at one OpenShell gateway and declares the sandbox profiles agents may use, the hub refuses to start on a configuration or a gateway it cannot work with, and from then on every sandbox route is decided by one set of rules about who may read, who may write and who may act through a sandbox. It also gives the rest of the sandbox work one client to the runtime, so that no other part of the hub talks to OpenShell on its own.

## Goals

- A hub without a `[sandbox]` section contacts no runtime, and every sandbox route answers HTTP 404 with `error_type` `sandboxes_disabled` to an authenticated caller.
- A `[sandbox]` section with several mistakes stops the start with exit status 1 and one error line that names every offending key; no message contains a credential.
- With a valid section the hub contacts the gateway once at start, within 30 seconds: it authenticates, checks the release, makes sure its OpenShell workspace exists, installs the provider profile of the hub credential, checks every configured provider profile and validates every profile's policy file. Any failure stops the start with a message that names the key or the profile.
- After a successful start, losing the gateway never stops the hub: the runtime client reports the runtime as unreachable, routes that need it answer HTTP 502 with `error_type` `sandbox_runtime_error`, and routes that read stored state keep answering.
- `GET /sandbox-profiles` lists the profiles in name order without policy paths, `env` values or secret values; `parley sandbox profiles` prints that list.
- `GET /sandboxes` lists the sandboxes of every workspace, newest first, to the admin token only.
- Packages 45 to 48 get: the loaded configuration and profiles; one runtime client interface with an OpenShell implementation and an in-memory fake that pass the same contract tests; route declarations that apply the sandbox check order, scopes and workspace needs; and the no-widening decision.

## Non-goals

- The sandbox object, its state machine, creation, quotas (the use of `max_sandboxes`, `max_per_org`, `max_per_workspace`, `max_per_user`), attaching credentials as providers, minting the hub credential, resolving and exposing secrets, redaction of exposed values (SBX-FR-068), the checkout, `GET /workspaces/:slug/sandboxes…` and `parley sandbox create|list|get`: package 45 sandbox_creation_and_credentials. That package also supplies the sandbox source that `GET /sandboxes` of this package reads.
- Exec, attach, file transfer, logs, activity tracking and the use of `max_exec_timeout`, `max_concurrent_execs`, `max_transfer_size` and `log_retention`: package 46 sandbox_exec_files_and_logs. It calls the no-widening decision of this package.
- Stop, start, idle stop, expiry, delete, checkpoints, reconciliation on `reconcile_interval`, record cleanup and the behaviour at hub shutdown: package 47 sandbox_lifecycle_and_checkpoints.
- The effective policy, denials, proposals, approve and reject, workspace sandbox rules, the use of `allow_workspace_rules` and the workspace-scope authority of SBX-FR-070: package 48 sandbox_network_policy. The refusal of workspace-bound tokens on approve and reject (`403 token_bound`, SBX-FR-014) is declared by that package with the declaration of package 08 workspace_bound_tokens, which is not a dependency of this one.
- The scope registry that holds `sandboxes:read` and `sandboxes:write` whether or not sandboxes are enabled (SBX-FR-004), the scope check and the glob cover rule: package 06 api_keys_and_access_tokens. This package calls them.
- The binding guard and its allowlist, on which `GET /sandbox-profiles` stands: package 08 workspace_bound_tokens.
- The metric catalogue, the registry and `/metrics`: package 51 audit_query_and_retention. This package declares the recorder interface the sandbox packages report through and reports `parley_sandbox_runtime_up`; the other sandbox metrics are reported by packages 45 to 48.
- Sandbox events: packages 45, 47 and 48. This package emits no hub event.
- Building or pulling images, and any policy language of Parley's own: out of the product's scope (§13.1).

## Background

The repository holds no code yet; every file named here is new, except the files of other packages that this one extends: `internal/hub/hub.go` (package 03) and the `/version` handler in `internal/server/` (package 01).

The source is `docs/prd/13_sandboxes.md`: SBX-FR-001 to SBX-FR-005 and SBX-FR-010 to SBX-FR-015, §13.3.1 (runtime), §13.3.2 (profile), the configured-credentials table and the name rules of §13.3.4, the static and start-up halves of SBX-FR-067, the routes `GET /sandbox-profiles` and `GET /sandboxes` of §13.5, `parley sandbox profiles` of §13.6, the runtime check of §13.7 and the configuration of §13.9. `docs/prd/03_platform.md` adds the place of the sandbox feature check in the global check order (PLAT-FR-013), the start-up check (PLAT-FR-002) and the `sandbox_runtime` member of `GET /version` (§3.5).

What exists when this package is built: the configuration loader with its key registry, the start hooks and the scrubber (package 03); the router with its check positions, error envelope and list helper (package 02); authentication, which reports the credential type `admin`, `api_key` or `pat` (package 04); the scope check (package 06); the workspace access component (package 07); the CLI client and command declaration (package 09).

This package builds in these places:

| Path | Holds |
|---|---|
| `internal/sandbox/config.go` | the `[sandbox]` keys, their defaults and static validation |
| `internal/sandbox/profile.go` | profiles and configured credentials, their validation and lookup |
| `internal/sandbox/compat.go` | the supported OpenShell release range and the comparison |
| `internal/sandbox/startcheck.go` | the start-up runtime check |
| `internal/sandbox/authz.go` | the feature check, the route kinds, the user-only rule, the no-widening decision, the runtime-error answer |
| `internal/sandbox/routes.go` | `GET /sandbox-profiles` and `GET /sandboxes` |
| `internal/sandbox/source.go` | the sandbox source interface and its registration |
| `internal/sandbox/metrics.go` | the metrics recorder interface and its default |
| `internal/sandbox/runtime/` | the runtime client interface, its types and its error kinds |
| `internal/sandbox/runtime/openshell/` | the implementation over the OpenShell Go SDK |
| `internal/sandbox/runtime/runtimetest/` | the in-memory fake and the contract tests both implementations pass |
| `internal/cli/sandbox.go` | the `parley sandbox` command group with `profiles` |
| `test/e2e/sandbox_runtime_test.go` | the end-to-end flows |

## Requirements

### Feature switch (SBX-FR-001, SBX-FR-004)

Sandboxes are on when the configuration file has a `[sandbox]` section, also an empty one (which is then invalid, because `gateway_url` is required). Without the section the hub builds no runtime client, contacts no gateway, runs no sandbox background work, and leaves stored sandbox records as they are. `GET /version` then has no `sandbox_runtime` member. The scopes `sandboxes:read` and `sandboxes:write` exist either way (package 06).

### Static validation of `[sandbox]` (SBX-FR-002, §13.9)

Validation runs when the configuration is loaded, before any data is opened. It checks everything below, collects every failure and reports them together as one error. Each failure names its key: `sandbox.<key>`, `sandbox.profiles[<name>].<key>` or `sandbox.credentials[<name>].<key>` (the zero-based index in place of the name when the entry has no usable name). The process exits with status 1 and one error line on stderr, as for every configuration error (package 03). An unknown key in `[sandbox]`, in a profile or in a credential entry is a failure like the others. No message contains the content of `token_file` or `oidc_client_secret_file`, and a `gateway_url` or `oidc_issuer` is never echoed with user information.

| Key | Type | Default | Valid |
|---|---|---|---|
| `runtime` | string | `openshell` | `openshell` only |
| `gateway_url` | URL | required | `https://host[:port]`; `http` only when the host is `localhost`, an address in `127.0.0.0/8` or `::1`; no user information, no query, no fragment, and no path other than `/` |
| `tls_cert`, `tls_key` | path | `""` | both or neither; each a readable file |
| `tls_ca` | path | `""` | a readable file |
| `token_file` | path | `""` | a readable file whose content, without surrounding white space, is not empty |
| `oidc_issuer`, `oidc_client_id`, `oidc_client_secret_file` | URL, string, path | `""` | all three or none; the issuer by the URL rule of `gateway_url`; the secret file readable and not empty |
| `openshell_workspace` | string | `parley` | `^[a-z0-9][a-z0-9_-]{0,62}$` |
| `max_ttl` | duration | `24h` | more than 0 |
| `idle_timeout` | duration | `30m` | 0 or more; 0 disables |
| `max_sandboxes` | int | `200` | at least 1 |
| `max_per_org` | int | `100` | at least 1 and at most `max_sandboxes` |
| `max_per_workspace` | int | `50` | at least 1 |
| `max_per_user` | int | `20` | at least 1 |
| `allow_workspace_rules` | bool | `true` | |
| `max_exec_timeout` | duration | `30m` | more than 0 |
| `max_concurrent_execs` | int | `8` | at least 1 |
| `max_transfer_size` | size | `1GB` | the size format of `[server].max_body_size` |
| `log_retention` | duration | `7d` | more than 0 |
| `reconcile_interval` | duration | `1m` | 10 s to 10 m, both included |
| `[[sandbox.profiles]]` | list of tables | | at least one |
| `[[sandbox.credentials]]` | list of tables | none | |

Exactly one way to authenticate to the gateway is configured: mutual TLS (`tls_cert` with `tls_key`), `token_file`, or OIDC client credentials. None, or more than one, is a failure that names `sandbox.gateway_url` and lists the ways found. `tls_ca` may accompany any of the three.

When sandboxes are on, `[server].external_url` must be set; an empty one is a failure that names `server.external_url`.

A valid section is available to the rest of the hub as one read-only value with every default filled in. It does not change while the process runs.

### Profiles and configured credentials (§13.3.2, §13.3.4, SBX-FR-067)

A profile is the operator's template for a sandbox, fixed for the life of the process.

| Field | Type | Default | Valid |
|---|---|---|---|
| `name` | string | required | `^[a-z0-9][a-z0-9_-]{0,62}$`, unique among the profiles |
| `default` | bool | false | at most one profile sets it |
| `description` | string | `""` | |
| `image` | string | `""` (the runtime's default image) | |
| `cpu`, `memory` | string | `""` (the runtime's default) | |
| `gpu` | int | 0 | 0 or more |
| `ttl` | duration | required | more than 0 and at most `[sandbox].max_ttl` |
| `idle_timeout` | duration | `[sandbox].idle_timeout` | 0 or more |
| `policy` | path | the built-in policy | a readable file |
| `credentials` | list of strings | `[]` | each the `name` of a `[[sandbox.credentials]]` entry |
| `optional_credentials` | list of strings | `[]` | as `credentials`; a name may not be in both lists |
| `expose_secrets` | list of strings | `[]` | the name rules below |
| `optional_expose_secrets` | list of strings | `[]` | the name rules below; a name may not be in both lists |
| `hub_access` | enum | `fork` | `fork` or `none` |
| `env` | map of strings | `{}` | each key matches `^[A-Za-z_][A-Za-z0-9_]*$` and does not start with `PARLEY_` or `OPENSHELL_` |
| `checkpoint` | bool | true | |
| `allow_proposals` | bool | true | |

A configured credential has `name` (required, `^[a-z0-9][a-z0-9_-]{0,62}$`, unique), `provider_profile` (required, not empty) and `secret` (required, not empty).

Name rules for `expose_secrets` and `optional_expose_secrets` (static half of SBX-FR-067): a name matches `^[A-Za-z_][A-Za-z0-9_]*$`; does not start with `PARLEY_` or `OPENSHELL_`; is not a reserved secret name of §11 (`GIT_PAT`, `GIT_USERNAME`, `GIT_PASSWORD`, `UPSTREAM_GIT_PAT`, `UPSTREAM_GIT_USERNAME`, `UPSTREAM_GIT_PASSWORD`, `FORGE_WEBHOOK_SECRET`, compared without regard to case); is not a key of the profile's `env`; and is not in both lists. Whether the secrets exist is not checked at start.

The loaded profiles can be looked up by name, the default profile can be asked for (there may be none), and all can be listed in name order.

### Start-up runtime check (SBX-FR-003, PLAT-FR-002, §13.7, §13.9)

After static validation, as a start hook that runs before the listener is bound, the hub contacts the runtime once. The whole check has 30 seconds. In this order it:

1. connects and authenticates, and reads the gateway's release;
2. checks that the release is inside the range this hub supports;
3. makes sure the OpenShell workspace `openshell_workspace` exists, creating it when it does not;
4. installs the provider profile of the hub credential, id `parley-hub`: it declares the variable `PARLEY_API_KEY` and authorizes the scheme, host and port of `[server].external_url` for paths under `/api/v1/` and `/git/`, for any program. A profile with that id that already exists is replaced;
5. reads every `provider_profile` named in `[[sandbox.credentials]]`; one the gateway does not know is a failure naming `sandbox.credentials[<name>].provider_profile`. For each sandbox profile, an exposed-secret name that equals a variable declared by a provider profile of one of the profile's `credentials` or `optional_credentials` is a failure naming the profile and the name (start-up half of SBX-FR-067);
6. sends every profile's `policy` file to the runtime for validation; one the runtime rejects, or cannot validate, is a failure naming `sandbox.profiles[<name>].policy` with the runtime's reason. The hosts the runtime reports as allowed are kept for the profile list. The built-in policy is not sent.

The first failure stops the check and the start: exit status 1, one error line on stderr that names the key or the profile, no `listening` line. A gateway that cannot be reached, refuses the credential, or does not answer in time is a failure naming `sandbox.gateway_url`. A release outside the range is a failure whose message holds the connected release and the supported range. The message is scrubbed and never contains the gateway token or the OIDC client secret.

On success the runtime is marked reachable, and `GET /version` carries `sandbox_runtime: {"supported": "<range>", "connected": "<release>"}`: the range as `>=<lowest> <<first unsupported>` and the release read in step 1.

### Runtime client (§13.3.1, SBX-FR-005)

The hub is a client of one OpenShell gateway and owns everything in its OpenShell workspace. All of the hub's traffic to the runtime goes through one client interface, defined in `internal/sandbox/runtime`, with two implementations: one over the OpenShell Go SDK and one in memory for tests. The interface offers these operations, each taking a context:

| Group | Operations |
|---|---|
| Gateway | `Release` (the gateway's health and its release); `EnsureWorkspace`; `Reachable` (the flag below, no call) |
| Provider profiles | `GetProviderProfile` (id, declared variables, authorized hosts); `InstallProviderProfile` (create or replace) |
| Providers | `EnsureProvider` (name, provider profile id, secret value, labels; create or replace); `DeleteProvider`; `AttachProvider` and `DetachProvider` (a provider and a sandbox); `SandboxProviders` (those of one sandbox); `ListProviders` (all of the workspace, with their labels) |
| Policy | `ValidatePolicy` (a policy value; returns the hosts it allows and the provider names its base document refers to); `GetPolicy` (a sandbox's policy and its revision); `AddPolicyRule` (one rule added to a sandbox's policy); `ListProposals` (the pending proposals of the workspace: the runtime's proposal id, the sandbox, the rule, the summary, the findings); `ApproveProposal`; `RejectProposal` |
| Sandboxes | `CreateSandbox` (name, image, cpu, memory, gpu, working directory, environment, policy value, providers, labels); `GetSandbox`; `ListSandboxes` (all of the workspace, with their labels); `DeleteSandbox`; `StopSandbox` (releases the compute, keeps the working directory); `StartSandbox` (with a policy value and an environment); `WaitReady`; `WatchSandboxes` (state changes); `RunMain` (starts the main process of a sandbox from a command) |
| Exec | `Exec` (command, working directory, environment, stdin; collects exit code, stdout and stderr); `ExecStream` (the same, with stdout and stderr delivered as they are produced; the command is killed when the context ends); `ExecInteractive` (a command on a terminal of a given size, with input, output, resize and close) |
| Files | `Upload` (a reader to a path with a mode, creating parent directories); `MakeDir`; `Stat` (file or directory, size, mode); `Download` (a file as a stream); `Walk` (the files and directories below a directory) |
| Logs | `Logs` (the main process's output or the policy log of one sandbox, with since, tail and follow) |

This is the whole interface: packages 45 to 48 call these operations by these names and add none. The types they exchange are plain data:

- **Policy value**: three parts, which the runtime merges: the base document (the content of a profile's `policy` file, or the built-in policy), the protected rules and the added rules. A rule is an object in OpenShell's schema that the hub does not interpret. No base document and no added rule removes or narrows a protected rule; a policy value the runtime refuses is an `invalid` result with the runtime's reason.
- **Sandbox status** (`GetSandbox`, `ListSandboxes`, `WatchSandboxes`): the name, the labels, the state (`pending`, `running`, `restarting`, `stopped`), whether the main process has ended and with which exit code, and whether the runtime still holds the working directory.
- **Log entry** (`Logs`): the time, the source (`process` or `policy`), the stream (`stdout` or `stderr`) and the text of a process line; for a policy entry the action (`allow` or `deny`), program, host, port, method and path (both absent for a connection) and the provider name of a credentialed request.
- **Proposal** (`ListProposals`): the runtime's proposal id, the sandbox name, the rule, the one-line summary and the findings (`severity`, `message`).

What every operation guarantees:

- **Scope.** It acts in the configured OpenShell workspace and in no other.
- **Error kinds.** A failure is one of four kinds, which callers tell apart: `not found` (the runtime has no such sandbox, provider, profile, proposal or path), `invalid` (the runtime understood and refused the request; the runtime's reason is kept), `unsupported` (the SDK in use has no such operation; every operation of the table may answer so, and none is imitated with other calls), and `unavailable` (everything else: the gateway cannot be reached, refuses the hub's credential, answers with a server error, or the call ran out of time).
- **Time.** An operation that does not stream is given at most 30 seconds, or less when the caller's context ends sooner; past it the result is `unavailable`. Streaming operations (`WatchSandboxes`, followed `Logs`, `ExecStream`, `ExecInteractive`, `Upload`, `Download`, `Walk`) end with the caller's context.
- **Reachability.** The client keeps one flag. A call that ends `unavailable` clears it; a call that ends in any other way sets it. It is set by a successful start-up check. Other packages read it for `runtime_reachable`. Every change of the flag is reported to the metrics recorder as `parley_sandbox_runtime_up` 1 or 0.
- **No process exit.** No failure of the runtime ends the hub process once it is listening.
- **No credential in text.** The gateway token and the OIDC client secret are registered with the scrubber when the client is built, and an error's text is scrubbed before it is returned.

The in-memory implementation gives the same results as the SDK implementation for the same sequence of calls, which one set of contract tests proves by running against both. It is the only test double of the runtime in the repository: packages 45 to 48 test against it and do not extend it. It keeps per workspace: provider profiles, providers, sandboxes with their status, the policy values it received, files, log entries and proposals. A sandbox's files are held in memory or, when a test asks for it, in a directory of the host in which `Exec` runs real commands (package 47 runs `git` this way); otherwise exec results are scripted. Its terminal echoes its input. Tests can tell it: to be unreachable; to fail one named operation with a chosen error kind and text; to report a chosen release; to hold provider profiles; to reject every policy value a predicate matches and to report provider names for a base document; to change a sandbox's state, end its main process with a code or drop its working directory; to emit process output and policy log entries; to add a pending proposal; and to break an open stream. It reports the calls it received in order and the streams that are open.

**Metrics recorder.** `internal/sandbox` declares the recorder the sandbox packages report their metrics to: `Add`, `Set` and `Observe`, each with a metric name and a label map and each returning an error, which is the method set of the recorder of package 51 in `internal/metrics`. `parleyd` passes that recorder when it exists; the default records nothing. Packages 44 to 48 report the metrics of §13.7 under exactly these names: `parley_sandbox_runtime_up` (this package), `parley_sandboxes{state}`, `parley_sandbox_creates_total{result}` and `parley_sandbox_seconds` (package 45), `parley_sandbox_exec_total{result}` (package 46), `parley_sandbox_checkpoints_total{result}` (package 47) and `parley_sandbox_denials_total` (package 48).

### Feature check and the runtime-error answer (SBX-FR-001, SBX-FR-005, SBX-FR-010)

Every sandbox route, of this package and of packages 45 to 48, declares the sandbox feature check. The check runs after steps 1 to 4 of the global order (path and method, authentication, rate limit, body size and content type) and before the binding guard, the scope check, workspace access, validation and state. While sandboxes are off it answers HTTP 404 with `error_type` `sandboxes_disabled`. So, with sandboxes off: a request without a credential gets `401 unauthenticated`; a path that does not exist gets `404 not_found`; a body over the limit gets `413`; and every authenticated request to a sandbox route gets `404 sandboxes_disabled`, whatever its scopes and whether or not the workspace exists.

A sandbox route whose handler ends with a runtime error of kind `unavailable` or `unsupported` that it did not handle answers HTTP 502 with `error_type` `sandbox_runtime_error` and a scrubbed message. Packages 46 and 47 give the same answer through a function of this package, with the runtime's text scrubbed of the sandbox's own secret values (package 45) as the message; no other package defines a second answer of this type. Routes that answer from stored state or from configuration (`GET /sandbox-profiles`, `GET /sandboxes`) keep answering 200 while the runtime is unreachable.

### Authorization on workspace sandbox routes (SBX-FR-010, SBX-FR-011, SBX-FR-013, SBX-FR-014)

A route under `/workspaces/:slug/sandboxes…` or `/workspaces/:slug/sandbox-rules…` declares one of two kinds, and the kind fixes its checks in the order feature check, binding guard, PAT scope, workspace access:

| Kind | PAT scope | Workspace need | Routes |
|---|---|---|---|
| read | `sandboxes:read` | read | get, list, logs, file download, proposals list |
| write | `sandboxes:write` | write | everything else, including attach, approve and reject |

The answers are those of the components behind the kind: a PAT without the scope gets `403 insufficient_scope` whether or not the workspace exists; a caller with no access gets `404 not_found` (`workspace not found`); an org member with read-only access gets `403 forbidden` on a write route and passes a read route; an owner and the admin token pass both.

A route may also declare that it needs a user (the create route does, SBX-FR-013). The admin token then gets `403 forbidden` with the message `the admin token cannot create a sandbox`, after workspace access and before the handler validates anything. On every other route the admin token is treated like an owner: it can read, stop and delete any sandbox.

### No widening (SBX-FR-012)

Running a command in a sandbox, attaching to it, or writing a file into it needs a credential that covers the sandbox's hub credential. The handler asks for the decision after it has found the sandbox and before it validates the body. The decision:

- a sandbox without a hub credential (`hub_access` null) is covered by every caller;
- the admin token and an API key cover every hub credential;
- a PAT covers it when it holds every scope in `hub_access.scopes`, by the scope check of package 06, and, when the PAT has a `paths` limit, the sandbox's `hub_access.paths` is not null and each of its globs is covered by one of the PAT's globs, by the cover rule of package 06 (PLAT-FR-059).

A caller that does not cover it gets `403 insufficient_scope`, and nothing is run, attached or written.

### `GET /sandbox-profiles` and `parley sandbox profiles` (SBX-FR-015, §13.5, §13.6)

Lists the hub's profiles. It needs `sandboxes:read` from a PAT and no workspace access: every user with an API key, the admin token and a PAT with the scope may call it, and workspace-bound tokens may too (the allowlist of package 08). The answer is `200` with `{"items": […], "next_cursor": null}`: every profile, ordered by `name`, on one page. The route takes no query parameters.

Each item has `name`, `default`, `description`, `image`, `cpu`, `memory`, `gpu`, `ttl` (the duration as text), `ttl_seconds`, `idle_timeout` (the duration as text; the hub's default when the profile sets none), `policy_summary` (the hosts the base policy allows, sorted; `[]` for the built-in policy), `credentials`, `optional_credentials`, `exposed_secrets` (`[{name, optional}]` sorted by name, `[]` when the profile exposes nothing), `hub_access`, `env_keys` (the keys of `env`, sorted), `checkpoint` and `allow_proposals`. It has no `policy`, `env`, `expose_secrets` or `optional_expose_secrets` member, and no value of an `env` entry or of a secret appears anywhere in the answer.

Errors: `401`; `403 insufficient_scope`; `404 sandboxes_disabled`.

`parley sandbox profiles` calls the route, prints the body on stdout and exits 0; a hub error is printed and exits by the CLI conventions (package 09). `parley sandbox` without a subcommand prints help.

### `GET /sandboxes` (SBX-FR-015, SBX-FR-005, §13.5)

Lists the sandboxes of every workspace, newest first (by `created_at`, then by `id`, both descending), paged by the list convention (`limit` 1 to 200, default 50; `cursor`). Query: `state` (one of `provisioning`, `ready`, `stopped`, `completed`, `failed`, `deleted`) and `user` (the id of the creating user). Filters combine with "and". A `user` that created nothing gives an empty list.

It needs the admin token. Any other credential, including the API key of a user with role `admin` and every PAT, gets `403 forbidden`. An unknown `state`, a `limit` outside 1 to 200 and a cursor that is malformed or from another query are `400 invalid_request`.

The items are sandbox objects, which package 45 defines and stores. This package reads them through a sandbox source, an interface of `internal/sandbox` with one operation `List`: given the filters `state` and `user`, a position (`created_at`, `id`) or none, and a limit, it returns the objects after that position in the order above, each with its `created_at` and `id`. Package 45 implements it over its store and registers it with `RegisterSource`. Until a source is registered the list is empty. The route answers from stored state and never calls the runtime, so it answers 200 while the runtime is unreachable; the source sets each object's `runtime_reachable` from the reachability flag of the runtime client.

Errors: `401`; `403 forbidden`; `400 invalid_request`; `404 sandboxes_disabled`.

### End-to-end flows

**An operator enables sandboxes and a developer lists the profiles.** The operator writes a `[sandbox]` section with a gateway URL, a token file, one credential and one profile, and starts `parleyd`. `parleyd` validates the section, contacts the OpenShell gateway, checks the release, makes sure the workspace exists, installs the `parley-hub` provider profile, checks the credential's provider profile and listens. The developer runs `parley sandbox profiles`; the parley CLI calls `GET /sandbox-profiles`; `parleyd` answers with the profile; the CLI prints it and exits 0. `GET /version` shows the connected release.

**An operator starts the hub against a gateway that is not there.** The operator starts `parleyd` with a valid `[sandbox]` section whose gateway does not answer. `parleyd` validates the section, fails to connect, writes one error line naming `sandbox.gateway_url` and exits 1 without listening.

**An agent asks a hub without sandboxes for the profiles.** The hub runs without a `[sandbox]` section. An agent with a PAT calls `GET /sandbox-profiles`. `parleyd` authenticates it and answers `404 sandboxes_disabled`.

**The gateway goes away while the hub runs.** The hub runs with sandboxes on. The OpenShell gateway stops answering. A call through the runtime client fails as `unavailable` and the client marks the runtime unreachable. The operator calls `GET /sandboxes` with the admin token and `parleyd` answers 200 from stored state; a route that needs the runtime answers `502 sandbox_runtime_error`; the process keeps running.

## Design Decisions

1. **Project decisions relied on.** Go; HTTP on `net/http` with the router of package 02; the CLI on `github.com/spf13/cobra` through package 09; the runtime client is `github.com/ai-openshell/sdk-go` (package `openshell/v1`); tests use the standard `testing` package, table-driven, with `net/http/httptest`; code takes a replaceable clock.
2. **The section is registered as a whole.** Package 03's loader checks one registered key at a time and stops at the first bad one; SBX-FR-002 wants every failure together. This package registers the section `sandbox` with the loader as one entry whose validator receives the parsed section, after environment references are replaced, and returns one error listing every failure, separated by `; `. The registration is package 03's `RegisterSection(name, validator)` in `internal/config/registry.go`, and the loaded section is read through `cfg.Section(name)`; package 03 owns both and this package edits no file of `internal/config` (with package 03). Why: the unknown-key rule and the collect-everything rule then have one owner.
3. **The one error line is cut at 1024 bytes** by the scrubber of package 03 like every start error. Failures are listed in the order of the tables above, so the first ones always survive.
4. **Files are checked for readability at static validation** (`tls_*`, `token_file`, `oidc_client_secret_file`, `policy`). The PRD types them as paths and says nothing; a missing file is a mistake the operator wants reported with the others.
5. **`token_file` is read once, at start,** and its content without surrounding white space is the bearer token for the life of the process. Why: the configuration is read once at start (package 03), and renewal is what the OIDC option is for.
6. **`gateway_url` shape.** No user information, query, fragment or path. "Loopback" means `localhost`, `127.0.0.0/8` and `::1`. The same rule applies to `oidc_issuer`, except that an issuer may have a path.
7. **Pattern for `openshell_workspace` and credential names** is the profile name pattern. The PRD gives none.
8. **Lower bounds.** Each `max_…` count is at least 1; `max_ttl`, `max_exec_timeout` and `log_retention` are more than 0. The PRD gives only defaults.
9. **`max_per_org` above `max_sandboxes`.** A `max_per_org` the file sets above `max_sandboxes` is a failure. When the file does not set it, its default is the smaller of 100 and `max_sandboxes`, so lowering `max_sandboxes` alone is not an error.
10. **`[server].external_url` is required with sandboxes on.** A sandbox reaches the hub at that URL (`PARLEY_ENDPOINT_URL`) and the hub credential's provider profile authorizes exactly its host. The PRD does not say what happens without it.
11. **A credential name in both `credentials` and `optional_credentials` is a failure,** like an exposed name in both of its lists.
12. **Static name rules of SBX-FR-067 are validated here,** although package 45 owns exposed secrets, because profile validation is one pass that reports everything together. Package 45 owns resolution, missing values and redaction.
13. **The start-up check stops at its first failure.** Later steps need the earlier ones, and SBX-US-01 asks for one message that names the problem.
14. **The supported release range** is one pair in `internal/sandbox/compat.go`: the lowest supported release, included, and the first unsupported release, excluded, compared as semantic versions. They are set from the SDK version in `go.mod`: lowest is that version's `major.minor.0`; first unsupported is the next major, or the next minor while the major is 0. A release that is not a semantic version is outside the range. Why: the PRD says the hub states its range and refuses anything outside it, and gives no numbers.
15. **The hub credential's provider profile** has the fixed id `parley-hub`, is installed at every start whatever the profiles' `hub_access`, and names no program restriction. The per-workspace narrowing of the hub rule belongs to the sandbox's policy (packages 45 and 48).
16. **A runtime that cannot validate a policy fails the start** for a profile that names a `policy` file (the client's `unsupported` kind is treated like a rejection). Why: starting with a policy nobody checked would break "refuses to start if the runtime rejects it". The built-in policy is the hub's own and is not sent.
17. **Layout inside `internal/sandbox`.** The interface is in `internal/sandbox/runtime`, the SDK implementation in `…/runtime/openshell`, the fake and the contract tests in `…/runtime/runtimetest` (as `internal/gitrepo/gitrepotest` does for origins). Only `…/runtime/openshell` imports the SDK.
18. **Error kinds and the 30-second bound** on non-streaming calls are this package's. The PRD names only `502 sandbox_runtime_error`; callers need to tell "no such sandbox" from "no gateway".
19. **An SDK without an operation.** Sandbox stop and start are described by OpenShell's documentation but were not confirmed in the SDK, and a call that validates a policy value, the policy value in three parts, `AddPolicyRule`, `RunMain`, `Stat`, `MakeDir` and `Walk` are not in the SDK's documented surface. The interface declares them all. Where the SDK in `go.mod` has no call that does what an operation says, the implementation returns the `unsupported` kind and does not imitate the operation with others; a caller that gets `unsupported` answers `502 sandbox_runtime_error` or fails its sandbox, as its own package says.
20. **Reachability has no probe of its own.** The flag follows the calls other packages make (reconciliation runs every minute, package 47).
21. **The hub composition takes a runtime factory** (configuration in, client out) whose default builds the SDK implementation, so that tests and `test/e2e` start a hub with the SDK implementation over the SDK's in-memory fake client, or with the fake of this package.
22. **`GET /sandbox-profiles` is one page** with `next_cursor` null and no query parameters. Profiles are configuration and few; the CLI entry has no paging flags.
23. **Profile list details.** `policy_summary` is an array of host strings. `image`, `cpu` and `memory` are `""` when the profile leaves them to the runtime. `ttl` and `idle_timeout` are written in the largest unit that divides them (`4h`, `30m`, `0s`).
24. **`GET /sandboxes` reads through a source that package 45 registers,** because the sandbox object and its storage are that package's while the route is this one's. `user` is a user id. A PAT gets `403 forbidden` from the admin check; the route declares no scope.
25. **Non-PAT credentials cover every hub credential** in the no-widening decision: an API key holds every scope and has no `paths`, and the admin token is not scope-checked. A PAT with `paths` does not cover a sandbox whose hub credential has none.
26. **The no-widening decision runs after the sandbox lookup and before body validation;** the user-only rule runs after workspace access. Both are authorization and so precede validation in the order of SBX-FR-010.
27. **Dependency on package 09** is added to the plan's list because this package specifies a `parley` command (brief, note on CLI commands).
28. **Profile: full**, with four execution paths.
29. **The interface is complete for packages 45 to 48.** Those packages were written to "add what is missing" to the client; instead this package declares every operation they call (the table of "Runtime client"), with the fake's test controls, and they use the names of this package and touch neither the interface, the OpenShell implementation nor the fake. Why: four packages extending one interface in parallel would each define it differently. The operations come from their needs: providers, creation in a working directory, `RunMain` and the environment (45); `ExecStream`, `ExecInteractive`, the file operations and `Logs` (46); `StopSandbox`, `StartSandbox`, the status with the main process's end and the working directory, `ListSandboxes` and `ListProviders` with labels (47); the policy value in three parts, `ValidatePolicy` with the providers a base document refers to, `AddPolicyRule`, `ListProposals`, `RejectProposal` and the policy source of `Logs` for denials (48).
30. **Sandbox metrics go through one recorder interface declared here.** Package 51 owns the catalogue and its recorder and expects each feature package to report to it; no sandbox package depends on package 51. The interface has the recorder's method set, each method returning an error as the registry methods of package 51 do, so `parleyd` passes the registry of `internal/metrics` without an adapter. This package reports `parley_sandbox_runtime_up` on every change of the reachability flag.

## Dependencies

- Package 03 server_lifecycle_and_configuration (draft): the configuration loader and its registry, the start hooks, the scrubber, the hub composition in `internal/hub`.
- Package 07 workspace_access_rule (draft): the access decision for a declared need; through it, packages 02 (router, check positions, error envelope, lists), 04 (the caller's credential type), 05 and 06 (scope check, cover rule).
- Package 09 cli_foundation (draft): the HTTP client, output, exit codes and command declaration for `parley sandbox profiles`.
- OpenShell: one gateway of a supported release, reached through `github.com/ai-openshell/sdk-go`.

## Verified External API

`github.com/ai-openshell/sdk-go`, package `openshell/v1`. Nothing below was read in source; every symbol is **unverified** and recorded with `"verified": false`. The documented surface:

- `v1.NewClient(v1.Config{Address, Auth, TLS})`; auth helpers `v1.StaticToken`, `v1.RefreshableToken` and the `oidc` sub-package.
- `Sandboxes()`: create, get, list, delete, watch, `WaitReady`, logs, provider attach, detach and list. Not confirmed: starting a main process after creation (`RunMain`), a working directory at creation, and the status members for the main process's end and the working directory.
- `Exec()`: `Run`, stream, interactive.
- `Files()`: upload, download. Not confirmed: `Stat`, `MakeDir` and `Walk`.
- `Providers()`: create, read, update, delete, ensure; `.Profiles()`: list, get, import, update, lint, delete.
- `Policy()`: draft review, approve, reject, merge, status. Not confirmed: a policy value in three parts and `AddPolicyRule`.
- `Workspaces()`.
- `Health()`: health, gateway info.
- package `fake`: an in-memory client.

Not confirmed:

- **Sandbox stop and start**: described in OpenShell's documentation, not confirmed in the SDK. Assumed to be operations of `Sandboxes()`. If absent, design decision 19 applies.
- **Validation of a policy document without a sandbox**: **NOT FOUND** in the documented surface. Assumed to be an operation of `Policy()` that returns the runtime's verdict and the hosts the policy allows. If absent, design decisions 16 and 19 apply.
- The exact method names, parameter types and error types of every operation above, and the module version, which is set when the module is added to `go.mod`.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
- **Bound token**: A PAT confined to one workspace (and the forks created with it). A token bound to a fork also reaches its own changes, and the awareness and policy reads, on the fork's parent (§3).
- **Path-scoped token**: A PAT whose writes are limited to files matching a list of globs.
- **Sandbox**: An isolated environment in which an agent runs, attached to a workspace (normally a task fork) and started with it checked out. It runs on OpenShell; the hub controls it (§13).
- **OpenShell**: The open-source sandbox runtime Parley's sandboxes run on. It provides the isolation, the policy format and the credential mechanism. The hub is a client of one OpenShell gateway.
- **Profile**: The operator's template for a sandbox, defined in configuration: image, resources, time limits, base policy, credentials and the secrets it exposes.
- **Exposed secret**: A secret (§11) that a profile names in `expose_secrets`. Its real value is placed in the sandbox's environment, where code in the sandbox can read it. Off unless the operator lists the secret (§13).
- **Credential (sandbox)**: A secret (§11) attached to a sandbox as an OpenShell provider. The workload sees only a placeholder; the runtime adds the real value outside the sandbox, on requests to the endpoints the provider's profile authorizes.
- **Hub credential**: The fork-bound PAT the hub mints for a sandbox so that it can reach the hub. The workload sees only its placeholder, in `PARLEY_API_KEY`.
- **Provider profile**: OpenShell's definition of how a credential may be used: the environment variable that holds its placeholder, and the hosts, ports, paths and programs it is added for.
- **Runtime**: The OpenShell gateway the hub is configured with, and the one OpenShell workspace in it that the hub owns.
