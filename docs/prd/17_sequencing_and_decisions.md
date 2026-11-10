[Parley PRD](00_overview.md) · §17

## 17. Sequencing and decisions

### 17.1 Sequencing

The sections are written as one product, but they are meant to be built in this order. Each stage ends with something usable.

| Stage | Contents | Result |
|---|---|---|
| 0. Foundation | Platform, identity and access (§3); workspaces without forks (§4); git server without change mapping (§5); secrets and variables (§11); the event log and basic audit (§14) | A hub that hosts repositories and serves git with scoped tokens |
| 1. First cut | Changes without stacks (§7); optimistic landing with the default policy, protected branches and conflict reports (§8); task forks (§4); source control API (§6); push-to-change mapping (§5) | An agent can fork, work, open a change and land it, over git or over the API alone |
| 2. Coordination | Stacks and restack (§7); policy rules, approvals, checks, automatic landing, revert, path-scoped tokens (§8, §3); awareness (§9); event replay, filters, long-poll and event webhooks (§14); sessions linked to changes (§14) | Many agents on one repository, with rules about what lands and visibility of each other |
| 3. Carried features | Forge integration (§12); sandboxes on OpenShell (§13); carry-patch workspaces (§10); agent telemetry ingestion and the full audit query (§14) | The complete product |

Stage 3 has no dependency on stage 2 and can run in parallel with it.

### 17.2 Later candidates

Not part of this document's requirements. Listed so that the design leaves room for them.

- **Parallel attempts.** Several forks started from one point for the same task, compared side by side, one chosen to land.
- **An undo log** for server-side operations (land, restack, abandon, ref moves), in the manner of Sapling's `undo`.
- **Checks run by the hub** in its own sandboxes (§13), on the rebased result of a change. With sandboxes that are created on a fork, driven by exec and report an exit code, this is now a small step.
- **Exposing services that run in a sandbox**, for example a preview of a web application, to people outside it.
- **A second sandbox runtime** behind the same contract (the REST routes and the sandbox record of §13), next to OpenShell.
- **Automatic mirror to origin** after each land, as a workspace setting.
- **Exporting a change to the forge** as a pull request, and importing forge review state as approvals.
- **Per-agent quotas** beyond the request rate limit: forks, lands and storage per user or session.
- **Ownership files in the repository** (`OWNERS` style) as an additional source of policy rules.
- **Hunk-level overlap** in awareness.
- **Sparse working copies** managed by the `parley` client.

### 17.3 Decisions

The product owner reviewed every row of this table on 2026-11-10 and confirmed or changed it. The rest of the document states each decision as it stands here.

| # | Decision | Where | Status |
|---|---|---|---|
| 1 | The name is Parley everywhere: credential prefix `parley_`, environment prefix `PARLEY_`, client configuration in `~/.parley`, metrics `parley_*`, event types `parley.*` and hub-owned refs under `refs/parley/` | §3, §5, §14 | Changed 2026-11-10 |
| 2 | The default branch is protected by default. A protected branch moves by landing, by sync from origin, and by a direct fast-forward push from the workspace's owner or the admin token, over git or the source control API. The exemption holds only for an API key, the admin token or an unbound PAT without `paths`; never for a workspace-bound token or an org member. Forced updates and deletes stay refused for everyone. The workspace setting `protect.owner_push` (default `true`) turns the exemption off. A direct push is recorded as `parley.git.push` with `direct: true`, and its commits belong to no change | §4, §5, §6, §8 | Changed 2026-11-10 |
| 3 | The default policy requires nothing: every change may land | §8 | Confirmed 2026-11-10 |
| 4 | Checks are reported on a version, not on the rebased result, and carry over a pure rebase | §8 | Confirmed 2026-11-10 |
| 5 | An approval is not counted when given with a credential that created any version of the change; the rule compares credentials, not users | §8 | Confirmed 2026-11-10 |
| 6 | Owners may override unmet approvals and checks (never blocks), with a recorded reason | §8 | Confirmed 2026-11-10 |
| 7 | Landing strategy defaults to `rebase` (commits kept); `squash` is a setting | §8 | Confirmed 2026-11-10 |
| 8 | In a fork, the first push of a new branch opens a change automatically | §4, §5 | Confirmed 2026-11-10 |
| 9 | Task forks live 7 days by default, 30 days at most, and are removed 24 hours after all their changes have landed or been abandoned | §4 | Changed 2026-11-10 |
| 10 | A fork owned by a read-only org member does not receive the parent workspace's secrets | §11 | Confirmed 2026-11-10 |
| 11 | Org members can fork and open changes against workspaces they can only read, and can land and revert their own change when policy is satisfied (`land.by_author`). They may also record verdicts and make claims | §3, §7, §8, §9 | Confirmed 2026-11-10 |
| 12 | Overlap is computed per file, and awareness never blocks | §9 | Confirmed 2026-11-10 |
| 13 | Events are delivered by resumable stream, by long-poll and, in the first release, by event webhooks: signed HTTP `POST`s per workspace subscription, in `seq` order, at least once, retried with backoff for 24 hours per event, with destination checks, a delivery log, ping and redelivery | §14 | Changed 2026-11-10 |
| 14 | No compatibility with the earlier hub: API, CLI, configuration names and data files all change | §1 | Confirmed 2026-11-10 |
| 15 | This document is split into one file per section | — | Changed 2026-11-10 |
| 16 | A workspace-bound token can never override policy, whoever its user is | §8 | Confirmed 2026-11-10 |
| 17 | A token bound to a fork cannot record verdicts, report checks or revert on the parent | §3, §8 | Confirmed 2026-11-10 |
| 18 | Reporting a check needs write access to the target workspace | §8 | Confirmed 2026-11-10 |
| 19 | A fork never grants more than its parent: a member who leaves the organization loses the forks they created in it | §3, §4 | Confirmed 2026-11-10 |
| 20 | An approval is discounted for any credential that created any version of the change, not only the approved one | §8 | Confirmed 2026-11-10 |
| 21 | Secret, bound-token and event-webhook events of a workspace are visible only at `owner` or `admin` level | §3, §14 | Confirmed 2026-11-10 |
| 22 | The admin token cannot create sandboxes, because the hub credential needs a user as its owner. It can read, stop and delete any sandbox | §13 | Confirmed 2026-11-10 |
| 23 | OpenShell is a hard dependency for sandboxes, and it is pre-1.0. The hub supports a stated range of releases and refuses to start against another | §13 | Confirmed 2026-11-10 |
| 24 | Raw secrets in a sandbox are opt-in. By default a secret reaches a sandbox only as a credential behind a placeholder. A profile may list secret names in `expose_secrets`; their real values are then placed in the sandbox's environment, where code in the sandbox can read them and send them wherever the network policy allows. Exposed names are shown in the profile list, the sandbox object and the create event; the hub redacts the values in stored logs and event text only | §11, §13 | Changed 2026-11-10 |
| 25 | Exec, files, logs and attach go through the hub's REST API, not directly to the runtime | §13 | Confirmed 2026-11-10 |
| 26 | An approved network rule applies to one sandbox by default (`scope: sandbox`) and is then not copied to a sandbox restored from it. The approver may promote it (`scope: workspace`): the rule is saved as a workspace sandbox rule and applied to every sandbox created later on that workspace or its forks, never to sandboxes that already run. At most 50 rules per workspace; the operator can switch promotion off with `[sandbox].allow_workspace_rules` | §13 | Changed 2026-11-10 |
| 27 | A sandbox never outlives its fork, and the deletion of a fork takes no checkpoint of its sandboxes | §4, §13 | Confirmed 2026-11-10 |
| 28 | Default quotas of 200 running sandboxes per hub, 100 per organization, 50 per workspace and its forks, and 20 per user; a refusal names the limit that was hit; a sandbox lives at most 24 h | §13 | Changed 2026-11-10 |
