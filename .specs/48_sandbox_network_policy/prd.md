---
spec_id: "48"
spec_name: "sandbox_network_policy"
title: "Control sandbox network access and approve new access"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "becb4c0cc63585bd7d653c01227d0cef381d84ac8dde4875207adc0c06ffd96c"
schema_version: 3
source: "docs/prd/13_sandboxes.md"
---

# Control sandbox network access and approve new access

## Intent

A sandbox reaches the hub, the endpoints of its credentials and what its operator allowed, and nothing else. When an agent in a sandbox is blocked, its request for one more destination reaches a person, who approves or rejects it with the runtime's risk findings in view, and who can keep an approved rule for the workspace so that later sandboxes do not have to ask again. The hub decides who may approve, records every decision, and never lets a profile, a rule or an approval weaken the rules that tie credentials to their endpoints.

## Goals

- Every sandbox is created at the runtime with an effective policy of four parts in a fixed order: the profile's base policy, the hub rule, the rules of the attached credentials, and the workspace sandbox rules that existed when the sandbox was created (SBX-FR-050, SBX-FR-071).
- The first denial of each (program, host, port) of a sandbox is one `parley.sandbox.deny` event; every denial is counted (SBX-FR-051).
- Each new proposal of a sandbox whose profile accepts proposals is stored as `open` and announced by one `parley.sandbox.proposal` event; proposals of other sandboxes are rejected at the runtime without a record (SBX-FR-052).
- `GET /api/v1/workspaces/:slug/sandboxes/:id/proposals` lists proposals; `POST …/proposals/:proposal_id/approve` and `…/reject` decide one, each decision is one `parley.sandbox.proposal_decided` event, and a workspace-bound token can do neither (SBX-FR-014, SBX-FR-053).
- An approval with `scope: workspace` also saves the rule on the workspace, or on the parent of a fork, at most 50 per workspace and never twice (SBX-FR-069, SBX-FR-070, SBX-FR-072, SBX-FR-073).
- `GET /api/v1/workspaces/:slug/sandbox-rules` lists the rules and `DELETE …/sandbox-rules/:rule_id` removes one without changing any sandbox that has it (SBX-FR-072, SBX-FR-074).
- `parley sandbox proposals`, `approve`, `reject`, `rules` and `rule-delete` drive those five routes.

## Non-goals

- The `[sandbox]` configuration and its validation, the start-up runtime check that validates each profile's `policy` file, the built-in policy, profiles, the runtime client and its fake, the feature check `404 sandboxes_disabled`, `GET /sandbox-profiles`: package 44 sandbox_runtime_and_profiles. This package calls that client by the operation names that package declares, adding none to the interface or its fake, reports its metric to that package's metrics recorder, and reads the key `allow_workspace_rules`, which that package validates with the rest of `[sandbox]`.
- The sandbox record and its state machine, creation, provisioning, quotas, credentials as providers, the hub credential and the content of the hub rule and of the credential rules, exposed secrets and their redaction, `parley.sandbox.create`: package 45 sandbox_creation_and_credentials. This package registers with that package the workspace rule provider (whose rules that package records with the sandbox: the ids in `workspace_rules`, the bodies beside them) and the policy composer (which that package's provisioning calls for a create and for a start), and registers a delete hook and a purge hook with the lists that package declares.
- Serving a sandbox's logs, in which denials are always visible (SBX-FR-034, the last sentence of SBX-FR-051): package 46 sandbox_exec_files_and_logs.
- Stop, start, expiry, deletion, restore from a checkpoint and reconciliation: package 47 sandbox_lifecycle_and_checkpoints. A start is provisioned by package 45's provisioning work, which calls the policy composer this package registers; a deletion and a record's removal run the hooks of package 45, with which this package registers. Neither package depends on the other.
- Running the removal of a workspace: package 14 workspace_archive_reclone_delete, which calls the remover of owned data this package registers for workspace sandbox rules.
- The event record, visibility and the stream: package 52 durable_event_log_and_stream. The metric catalogue and `/metrics`: package 51 audit_query_and_retention. This package reports `parley_sandbox_denials_total` to the metrics recorder that package 44 declares.
- The binding guard and the access rule: packages 08 workspace_bound_tokens and 07 workspace_access_rule. The routes of this package that must refuse workspace-bound tokens (approve, reject, rule delete; SBX-FR-014) declare it at registration with package 08, whose guard then answers `403 token_bound`; package 44 leaves this refusal to this package. The CLI client, output and exit codes: package 09 cli_foundation.
- Any policy language of Parley's own. A rule is an object in OpenShell's schema that the hub stores and passes on without interpreting it.

## Background

The repository holds no code yet; every file named here is new. Sandboxes live in `internal/sandbox/`: package 44 delivers the runtime client interface over the OpenShell SDK with a fake for tests, and package 45 delivers the sandbox record, creation and credentials. This package adds to `internal/sandbox/` the policy composition (`policy.go`), the denial and proposal collector (`collector.go`), the proposal store and routes (`proposal.go`, `proposal_routes.go`), the workspace sandbox rules (`rule.go`, `rule_routes.go`), and to `internal/cli/` the file `sandbox_policy.go`. End-to-end tests go to `test/e2e/sandbox_policy_test.go`. Process composition is in `internal/hub/`.

Source: `docs/prd/13_sandboxes.md` §13.3.7, SBX-FR-014, SBX-FR-050 to SBX-FR-054, SBX-FR-069 to SBX-FR-074, and the parts of §13.5 to §13.10 that belong to them.

## Requirements

### Effective policy (§13.3.7, SBX-FR-050, SBX-FR-054, SBX-FR-071)

A sandbox's effective policy has four parts, in this order: (1) its profile's base policy; (2) the hub rule; (3) the rules contributed by each attached credential's provider profile; (4) the workspace sandbox rules of its workspace as they were when the sandbox was created, and for a sandbox on a task fork those of the fork's parent. Rules approved for the sandbox itself are added on top while it lives. Everything else is denied.

When a sandbox is stored, the hub records, in the same transaction, the workspace sandbox rules that exist at that moment: their ids, oldest first, appear in the sandbox object as `workspace_rules`, and a copy of each rule's body is kept with the sandbox. The set is fixed for the sandbox's life, across stop and start. A rule saved later never reaches a sandbox created before it; deleting a rule changes no sandbox that has it. This holds whatever the sandbox's profile and whatever `allow_workspace_rules` says.

The hub rule and the credential rules are handed to the runtime as protected parts: they are present, unchanged, in every effective policy, and no base policy, workspace rule or approved rule removes or narrows them. A base policy cannot grant the hub credential to another endpoint: when a base policy's rules refer to the provider of the hub credential, provisioning fails and the sandbox ends `failed` with `state_reason: provision_failed` and an error that names the profile.

If the runtime rejects the policy that results, the sandbox ends `failed` with `provision_failed`. When the policy without workspace rules is accepted and the policy with them is not, the error names the id of the first workspace rule, in order, with which the runtime rejects.

At each start the policy is composed again from the same four parts, with the recorded copies of the workspace rules, plus the rules approved for the sandbox with either scope. A sandbox restored from a checkpoint (`restore_from`) is a new sandbox: it gets the workspace rules that exist at its own creation and none of the rules approved for the source sandbox alone.

### Denials (SBX-FR-051)

A denial is a connection or request the runtime blocked. The hub reads denials from the runtime continuously for every sandbox that is `ready`. The first denial of each (program, host, port) per sandbox emits `parley.sandbox.deny` with actor `system`, `resource_type: "sandbox"`, the sandbox's id, workspace and session, and the metadata `program`, `host`, `port`, `method`, `path` (`method` and `path` null for a blocked connection). Later denials of the same triple emit nothing. Every denial, first or not, adds one to `parley_sandbox_denials_total`. Which triples were reported is stored, so a hub restart does not report one twice. Loss of the runtime never stops the hub: reading resumes when the runtime answers again.

### Proposals (SBX-FR-052, SBX-FR-053)

A proposal is a request from the workload for an additional network rule, made with the runtime's own mechanism. The hub collects proposals from the runtime. For a sandbox whose profile had `allow_proposals` at its creation, each new proposal is stored as `open` and `parley.sandbox.proposal` is emitted with actor `system` and the metadata `proposal_id`, `summary`, `max_severity`. A proposal the hub already holds is not stored or announced again. A proposal of a sandbox whose profile did not allow proposals is rejected at the runtime; nothing is stored and no event is emitted.

| Proposal field | Type | Meaning |
|---|---|---|
| `id` | string | `sbp_` + 32 lowercase hex characters |
| `sandbox_id` | string | |
| `rule` | object | The proposed rule, in OpenShell's schema |
| `summary` | string | One line: program, host, port, and methods or paths when restricted |
| `findings` | object[] | The runtime's findings: `{severity, message}` |
| `state` | enum | `open`, `approved`, `rejected`, `withdrawn` |
| `scope` | enum or null | `sandbox` or `workspace` once approved; null otherwise |
| `workspace_rule_id` | string or null | Id of the workspace sandbox rule the approval saved or matched, when `scope` is `workspace` |
| `decided_by` | object or null | `{actor_type, actor_id, username}` |
| `decided_at`, `comment` | timestamp or null, string or null | Set when decided |
| `created_at` | timestamp | |

Open proposals of a sandbox that is deleted become `withdrawn` in the transaction that marks it `deleted`. Proposals are removed with their sandbox's record, in the same transaction.

### Proposal routes (§13.5, SBX-FR-010, SBX-FR-011, SBX-FR-014)

All routes are under `/api/v1` and follow the conventions of §3. Without a `[sandbox]` section each answers `404 sandboxes_disabled`. Common failures: `403 insufficient_scope`; `403 forbidden`; `404 workspace not found`; `404 sandbox_not_found` (unknown id, an id of another workspace, or a removed record).

`GET /workspaces/:slug/sandboxes/:id/proposals` needs `sandboxes:read` and read access. Query: `state` (one of `open`, `approved`, `rejected`, `withdrawn`; default `open`), `limit`, `cursor`. It answers `200` with a list of proposals, oldest first by (`created_at`, `id`). Another `state` value is `400 invalid_request`.

`POST …/proposals/:proposal_id/approve` takes `{"comment"?: string, "acknowledge_findings"?: bool, "scope"?: "sandbox" | "workspace"}`; `scope` defaults to `sandbox`. `POST …/proposals/:proposal_id/reject` takes `{"comment"?: string}`. Both need `sandboxes:write`, write access to the sandbox's workspace and a credential that is not a workspace-bound token: a PAT bound to a workspace, the sandbox's own hub credential included, gets `403 token_bound` from the binding guard of package 08, before its scope is looked at. Both answer `200` with the proposal.

Approving adds the proposal's rule to that sandbox's policy at the runtime, increases the sandbox's `policy_revision` by one, and sets the proposal to `approved` with `scope`, `decided_by`, `decided_at` and `comment`. The rule lasts for the sandbox's life, across stop and start. Rejecting sets the proposal to `rejected` with `decided_by`, `decided_at` and `comment`, and tells the runtime. Both emit `parley.sandbox.proposal_decided` with the caller as actor and the metadata `proposal_id`, `decision` (`approved` or `rejected`), `acknowledged_findings`, `scope` (null for a rejection) and `rule_id` (the workspace sandbox rule saved or matched, else null).

Failures, in the order they are checked:

| Case | Answer |
|---|---|
| a workspace-bound token | `403 token_bound` |
| a PAT without `sandboxes:write` | `403 insufficient_scope` |
| read-only access to the sandbox's workspace | `403 forbidden` |
| the body is not an object, `scope` is not `sandbox` or `workspace`, `comment` is not a string, `acknowledge_findings` is not a boolean | `400 invalid_request` |
| `scope: workspace` while `[sandbox].allow_workspace_rules` is false | `403 workspace_rules_disabled`; nothing is decided |
| `scope: workspace` without write access to the workspace that will hold the rule | `403 forbidden`; the proposal stays `open` |
| `scope: workspace` and the fork's parent is archived | `409 parent_archived` |
| unknown proposal id, or a proposal of another sandbox | `404 not_found` |
| the proposal is not `open` | `409 proposal_decided` |
| a finding of severity `high` and no `acknowledge_findings: true` (approve, both scopes) | `409 findings_not_acknowledged` |
| `scope: workspace`, the workspace has 50 rules and none is identical | `429 sandbox_rule_limit_reached`; the proposal stays `open` |
| the runtime rejects the resulting policy | `409 conflict` with the runtime's reason; the proposal stays `open` |
| the runtime cannot be reached | `502 sandbox_runtime_error`; the proposal stays `open` |

Two decisions of one proposal that race produce one decision and one event; the other answers `409 proposal_decided`.

### Workspace sandbox rules (SBX-FR-069 to SBX-FR-074)

A workspace sandbox rule is an approved rule saved on a repository workspace. A fork has no rules of its own.

| Field | Type | Meaning |
|---|---|---|
| `id` | string | `sbr_` + 32 lowercase hex characters |
| `rule` | object | The rule, in OpenShell's schema, as it was approved |
| `summary` | string | The proposal's one-line summary |
| `findings` | object[] | The runtime's findings on the proposal, as they were acknowledged |
| `from_proposal` | object | `{sandbox_id, proposal_id, workspace}`: the proposal the rule came from and the workspace of its sandbox, which may be a fork |
| `created_by` | object | `{actor_type, actor_id, username}` of the approver |
| `created_at` | timestamp | |

**Promotion.** An approval with `scope: workspace` does what an approval does and, in the same transaction, saves the rule on the sandbox's workspace, or on its parent when that workspace is a task fork. The proposal records `scope: "workspace"` and `workspace_rule_id`. It emits `parley.sandbox.rule_add` with the caller as actor, `resource_type: "sandbox_rule"`, `resource_id` the rule id, `workspace` the workspace that holds the rule, `session_id` the sandbox's, and the metadata `rule_id`, `summary`, `max_severity`, `from_proposal`. A rule identical to one the workspace already has is not saved twice: the approval succeeds, names the existing rule in `workspace_rule_id`, and emits no `parley.sandbox.rule_add`. The authority is checked on the workspace that will hold the rule: a fork owner who can only read the parent gets `403 forbidden` and may still approve with `scope: sandbox`. A workspace has at most 50 rules, fixed and not configurable.

**Operator switch.** `[sandbox].allow_workspace_rules` (bool, default `true`). With `false`, promotion is refused; rules saved earlier stay listed, keep being added to new sandboxes, and can be deleted.

**List.** `GET /workspaces/:slug/sandbox-rules` needs `sandboxes:read` and read access. Query: `limit`, `cursor`. It answers `200` with a list of rules ordered by (`created_at`, `id`), oldest first. With a fork's slug it lists the rules of the fork's parent.

**Delete.** `DELETE /workspaces/:slug/sandbox-rules/:rule_id` needs `sandboxes:write`, write access to the workspace and a credential that is not a workspace-bound token. It works in any workspace state. It answers `204` and emits `parley.sandbox.rule_remove` with the caller as actor, `resource_type: "sandbox_rule"`, `resource_id` the rule id, `session_id` null, and the metadata `rule_id`, `summary`. Failures: `403 token_bound`; `403 forbidden`; `400 not_supported_on_fork` with a fork's slug; `404 sandbox_rule_not_found` for an unknown id or a rule of another workspace.

Deleting a workspace deletes its rules and emits no rule event. The two rule events have normal workspace visibility.

### CLI (§13.6)

| Command | Flags | Calls | Output |
|---|---|---|---|
| `parley sandbox proposals <workspace-slug> <id>` | `--state` | `GET …/proposals` | the response |
| `parley sandbox approve <workspace-slug> <id> <proposal-id>` | `--comment`, `--acknowledge-findings`, `--scope sandbox\|workspace` (default `sandbox`) | `POST …/approve` | the proposal |
| `parley sandbox reject <workspace-slug> <id> <proposal-id>` | `--comment` | `POST …/reject` | the proposal |
| `parley sandbox rules <workspace-slug>` | `--limit`, `--cursor` | `GET /workspaces/:slug/sandbox-rules` | the response |
| `parley sandbox rule-delete <workspace-slug> <rule-id>` | | `DELETE /workspaces/:slug/sandbox-rules/:rule_id` | `{"id":"<rule-id>","status":"deleted"}` |

All follow the CLI conventions of §3: exit 0 on success, 1 with the hub's error envelope on stderr, 2 for a usage error (a wrong argument count, a `--scope` other than `sandbox` or `workspace`) before any request.

### End-to-end flows

1. **Blocked, asked, approved for the workspace.** An agent's program in a sandbox on a fork connects to a host no rule allows. The OpenShell gateway blocks it and reports the denial; the hub emits `parley.sandbox.deny`. The workload proposes a rule; the hub stores the proposal and emits `parley.sandbox.proposal`. A developer runs `parley sandbox proposals` and then `parley sandbox approve … --scope workspace`; the hub adds the rule to the sandbox at the runtime, saves it on the fork's parent and emits `parley.sandbox.proposal_decided` and `parley.sandbox.rule_add`. A sandbox created afterwards on the parent or on any of its forks lists the rule in `workspace_rules` and is created at the runtime with it.
2. **A rule is deleted.** A developer runs `parley sandbox rules` and `parley sandbox rule-delete`; the hub removes the rule and emits `parley.sandbox.rule_remove`. The sandbox that had the rule keeps it across a stop and a start; a sandbox created afterwards does not get it.

## Design Decisions

1. **Go, `net/http`, SQLite behind `internal/store`, cobra for the CLI, `testing` with table-driven tests** (project-wide decisions 1 to 4 and 7).
2. **The runtime is reached only through the runtime client interface of package 44**, backed by `github.com/ai-openshell/sdk-go` (`openshell/v1`). This package adds no operation to it and does not touch its OpenShell implementation or its fake. It calls `Logs` with the source `policy` (denials are the entries whose action is `deny`), `ListProposals`, `RejectProposal`, `AddPolicyRule` and `ValidatePolicy`, and it builds the policy value in three parts that `CreateSandbox` and `StartSandbox` take. A result of kind `invalid` is the runtime's rejection with its reason; `unavailable` and `unsupported` are a runtime that cannot be reached. Why: one interface with one owner; the SDK's symbols are recorded as unverified.
3. **The effective policy is handed to the runtime as parts, not as one merged document**: the base policy, a list of protected rules (hub rule, credential rules), and a list of added rules (workspace rules, then approved rules). Why: Parley defines no policy language, so the hub cannot merge OpenShell documents itself; "cannot be removed or narrowed" is then the runtime client's contract for protected rules.
4. **A base policy that refers to the hub credential's provider fails provisioning** with `provision_failed` naming the profile. Why: SBX-FR-050 says it "cannot grant" and names no outcome; a failure the operator sees is safer than a silent edit of the operator's file. The composer finds it in the result of `ValidatePolicy`, which lists the providers a base document refers to, and returns an error, on which package 45's provisioning fails before any create call.
5. **A sandbox keeps a copy of each workspace rule it was created with**, and of the profile's `allow_proposals`. Both are fields of package 45's record: the copies are what this package's rule provider returned in the creating transaction, and `allow_proposals` is part of the profile snapshot. This package keeps no table of its own for them. Why: a deleted rule must stay in force for sandboxes that have it, across stop and start, and a profile may be removed while its sandboxes live.
6. **The rule that makes the runtime reject a policy is found by adding the workspace rules one at a time**, only after a rejection. Why: SBX-FR-071 wants the error to name the rule and the runtime's error format is unknown.
7. **Proposal ids are `sbp_` + 32 lowercase hex characters**; the runtime's own proposal id is kept beside it to recognise a proposal seen before. Why: the PRD gives no format; this follows `sbx_` and `sbr_`.
8. **`max_severity` is the highest severity among the findings in the order `low` < `medium` < `high`**, null without findings; a severity outside these three ranks below `low`. Why: the PRD names only `high`.
9. **Two rules are identical when their `rule` objects are equal as JSON values** (key order and whitespace ignored). Why: the hub does not interpret rules.
10. **An unknown proposal id answers `404 not_found`**, the generic type. Why: §13 defines no specific type for it.
11. **A runtime that rejects an approved rule answers `409 conflict`** with the runtime's reason in `message`, cut to 1,024 bytes and passed through package 45's redaction. Why: §13.10 gives the status and no type.
12. **An approval reaches the runtime only while the sandbox is `provisioning` or `ready`.** In `stopped`, `completed` and `failed` the decision is stored and the rule is in the policy of the next start. Why: the PRD lists no state refusal for approval, and the runtime holds no running sandbox to change.
13. **Rejection tells the runtime after the decision is stored; a runtime failure is logged and does not change the answer.** Why: SBX-FR-053 says rejecting "records the decision"; the workload should still learn of it when possible.
14. **`withdrawn` and the removal of a sandbox's rows are hooks.** Package 45 declares a list of delete hooks and a list of purge hooks; package 47 runs them in the transaction that marks a sandbox `deleted` and in the one that removes its record. This package registers a delete hook that sets the sandbox's open proposals to `withdrawn` and a purge hook that deletes its proposals, reported denial triples and approved rules. Why: package 47 owns deletion and does not depend on this package, and this package does not depend on package 47.
15. **Workspace state does not block a decision**, except `409 parent_archived` for `scope: workspace` on a fork of an archived parent. Why: that is the only state refusal §13.5 lists for these routes.
16. **Order of an approval's checks**: binding guard, scope, access to the sandbox's workspace, body validation, the operator switch, authority on the holding workspace, parent archived, proposal exists, proposal open, findings, identical rule or limit, runtime, commit. Why: SBX-FR-010 fixes the first four; the rest follows "nothing is decided" for the switch and puts the runtime call last so that a refusal costs nothing.
17. **The denial collector and the proposal collector run as one background worker** per hub, polling pending proposals every 5 seconds and following the policy log of each `ready` sandbox for its deny entries, with a retry delay of 5 seconds after a runtime failure. Why: §13.7 says "continuously" and gives no interval.
18. **`allow_workspace_rules` is package 44's key.** That package validates the whole `[sandbox]` section, this key included (bool, default `true`), and refuses unknown keys; this package reads the loaded value and registers nothing with the configuration.
19. **Decisions run one at a time per sandbox, and promotions one at a time per holding workspace**, by in-process locks held from the first read to the commit; the conditional update on the proposal's state stays as the last guard. Why: the runtime is called before the commit, so without the lock a decision that loses a race could leave its rule at the runtime, and two promotions could pass the limit of 50 together. The hub is one process (project-wide decision 2).
20. **The policy of a sandbox is composed through registration.** Package 45 declares the workspace rule provider and the policy composer (`Compose`, `Explain`) with a default that knows only the base policy and the protected rules; this package registers the full ones. Package 45's provisioning calls the registered composer for a create and for a start, so the start of package 47 gets the workspace rules and the approved rules without a dependency on this package. The protected rules (hub rule, credential rules) are exported by package 45 and passed on unchanged.
21. **The denial count is reported, not kept.** Each denial is one `Add` of `parley_sandbox_denials_total` on the metrics recorder interface of package 44, which the recorder of package 51 fills.

## Dependencies

- Package 45 sandbox_creation_and_credentials (draft): the sandbox record with `policy_revision` and `workspace_rules`, the workspace rule provider and policy composer interfaces this package registers with, the delete and purge hook lists, the protected rules (hub rule and credential rules), the profile snapshot with `allow_proposals`, redaction. Through it: 44 (runtime client with its named operations and fake, profiles, feature check, `[sandbox]` configuration with `allow_workspace_rules`, metrics recorder), 08 (binding guard and the declaration that a route refuses bound tokens), 07 (access rule, fork parent), 06 (scope check), 03 (store, configuration key registry, background work, maintenance tasks), 02 (error envelope, lists).
- Package 52 durable_event_log_and_stream (draft): the event component every event of this package is written through, in the transaction of the state change.
- Package 09 cli_foundation (draft): the HTTP client, output and exit codes of the five commands.
- Wired at start and not needed to build this package: the collaborator set of package 14 (it calls the remover of owned data this package registers for workspace sandbox rules), package 47 (it runs the hooks this package registers with package 45, and its start reaches this package's composer through package 45's provisioning), the recorder of package 51 (behind package 44's metrics interface).
- External: an OpenShell gateway, reached through `github.com/ai-openshell/sdk-go`.

## Verified External API

- `github.com/ai-openshell/sdk-go`, package `openshell/v1`: **unverified**. No signature was read. Assumed to exist, behind the runtime client interface of package 44: a stream of a sandbox's policy log with denial entries (program, host, port, method, path); a listing of pending policy proposals with rule, summary and findings; an operation that adds a rule to a running sandbox's policy and reports a rejection with a reason; an operation that rejects a proposal; and a policy value made of a base document, protected rules and added rules. `af-impl` confirms each before use and adapts only the adapter in `internal/sandbox`.

## Glossary

- **Sandbox**: An isolated environment in which an agent runs, attached to a workspace (normally a task fork) and started with it checked out. It runs on OpenShell; the hub controls it (§13).
- **OpenShell**: The open-source sandbox runtime Parley's sandboxes run on. It provides the isolation, the policy format and the credential mechanism. The hub is a client of one OpenShell gateway.
- **Profile**: The operator's template for a sandbox, defined in configuration: image, resources, time limits, base policy, credentials and the secrets it exposes.
- **Credential (sandbox)**: A secret (§11) attached to a sandbox as an OpenShell provider. The workload sees only a placeholder; the runtime adds the real value outside the sandbox, on requests to the endpoints the provider's profile authorizes.
- **Hub credential**: The fork-bound PAT the hub mints for a sandbox so that it can reach the hub. The workload sees only its placeholder, in `PARLEY_API_KEY`.
- **Proposal (network access)**: A request from a sandbox's workload for an additional network rule. A person approves or rejects it; an approved rule applies to that sandbox only, unless the approver promotes it to a workspace sandbox rule.
- **Workspace sandbox rule**: An approved network rule saved on a workspace. It is added, on top of the profile's policy, to every sandbox created later on that workspace or its forks (§13).
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires: after 7 days by default, or 24 hours after all its changes have landed or been abandoned.
- **Bound token**: A PAT confined to one workspace (and the forks created with it). A token bound to a fork also reaches its own changes, and the awareness and policy reads, on the fork's parent (§3).
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
- **Denial**: A connection or request of a sandbox that the runtime blocked.
- **Effective policy**: The policy a sandbox runs under: the profile's base policy, the hub rule, the credential rules and the workspace sandbox rules recorded at creation, plus the rules approved for the sandbox.
