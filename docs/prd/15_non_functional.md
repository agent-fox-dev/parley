[Parley PRD](00_overview.md) · §15

## 15. Non-functional requirements

### 15.1 Deployment model

- **NFR-001** The hub runs as **one process**. Several replicas are not supported. Nothing in the product may require an external database, cache or message broker. The one optional external component is the OpenShell gateway, needed only when sandboxes are enabled (§13); a hub without `[sandbox]` needs nothing external.
- **NFR-002** State is kept in embedded stores on local disk under one data directory: the hub's transactional state (identity, workspaces, changes, policy, land attempts, claims, jobs, secrets, sandbox records, the event log), the analytical store for audit queries and agent telemetry, and the repositories. The choice of storage engines is an implementation decision. The repositories MUST remain ordinary git repositories that stock `git` can read.
- **NFR-003** Schema evolution is automatic. Every start brings the stores to the current schema without a separate migration step, and a newer binary can always open the data of the previous release.
- **NFR-004** The server needs the `git` command at run time (version 2.38 or later, checked at start). The `parley` client is a single static binary with no dependency on the server's stacks.
- **NFR-005** Version, commit and build time are set at build time and reported by `GET /version` and `parley version`.
- **NFR-006** Deliverables: the two binaries, a hub container image, a reference sandbox image that satisfies the image contract of §13.9, and Kubernetes manifests (configuration, deployment with one replica, persistent volume claim, service, an example for secrets).
- **NFR-007** Backup is a consistent copy of the data directory. The hub provides `parleyd backup <directory>` (§3), which writes a consistent snapshot of the data directory of a running hub into an empty directory. A hub started on a restored snapshot starts cleanly.

### 15.2 Concurrency and consistency

- **NFR-010** There is no global or per-workspace lock that a request waits on or fails on. Ref updates are compare-and-swap. The only exclusive periods are the maintenance operations of §4 (initial clone, reclone, archive, delete).
- **NFR-011** Readers never see a partial update: a ref has its old or its new value, a change is landed or not, a rebuild's integration branch is the old or the new one.
- **NFR-012** State that must agree is changed atomically: a ref move with its version record (§7), a land with its bookkeeping and event (§8), a state change with the event that reports it (§14).
- **NFR-013** Every piece of background work that a client was told about (clone, rebuild, land attempt, restack, automatic landing, sandbox provisioning) survives a restart and is resumed or cleanly failed. After a crash the hub reconciles refs with records at start (§7.7).
- **NFR-014** Only these things live in memory and are lost at restart without harm: open event-stream connections (clients resume by `seq`), rate-limit counters, caches, and open exec, attach, log and transfer requests to sandboxes. The sandboxes themselves belong to the runtime and are not affected by a stop, crash or upgrade of the hub (SBX-FR-060).

### 15.3 Security

- **NFR-020** Secret values are write-only through every interface and encrypted at rest (§11). Tokens and keys are stored only as hashes.
- **NFR-021** Text that can carry credentials is scrubbed before it reaches a response, an event or a log: git errors, remote URLs, forge errors.
- **NFR-022** Anti-enumeration: a caller without read access to a workspace gets the same answer as for a workspace that does not exist, on every surface.
- **NFR-023** Git subprocesses run with validated ref names, a scrubbed environment, timeouts and process-group termination. Repository content is never executed by the hub: no hooks from repositories run, and server-side rebases do not run filters or drivers defined in repository configuration or attributes.
- **NFR-024** Least privilege is expressible end to end: a token can be limited to scopes, to one workspace or fork, to paths, and in time. A task fork's token reaches nothing outside the fork except its own changes and the read-only awareness and policy routes of its parent (§3).
- **NFR-025** Forge webhooks are authenticated by signature or token and de-duplicated (§12). The ingress is outside bearer authentication and has its own body limit.
- **NFR-026** Policy cannot be bypassed by choice of surface: the push rules apply identically to git pushes and to the source control API, and a protected branch moves only by landing, by sync from origin, or by a direct fast-forward push from the workspace's owner or the admin token, which is recorded and which the workspace setting `protect.owner_push` turns off (§8.3.1). A workspace-bound or path-scoped token never moves a protected branch directly, and nobody force-pushes or deletes one.
- **NFR-027** No real credential is ever placed in a sandbox unless the operator exposes a named secret in a profile (`expose_secrets`, §13.3.4). Without that the workload sees placeholders only; the hub credential and every secret are added outside the sandbox, on requests to authorized endpoints. The hub credential is never exposed. An exposed secret is readable by code in the sandbox and can be sent wherever the network policy allows; the hub names exposed secrets in the profile list, the sandbox object and the create event, and redacts their values only in the logs it stores and in event text. A sandbox has no network egress except to the hub unless a profile, an approved proposal or a workspace sandbox rule allows it.
- **NFR-028** Sandbox isolation (filesystem, process and network confinement) is provided and enforced by OpenShell, not by the hub. The hub's part is access control, the policy it hands to the runtime, time limits, quotas and the record of what happened (§13.1).
- **NFR-044** Event webhooks (§14) are signed with a per-subscription secret (HMAC-SHA256 over the timestamp and the body), sent over verified TLS, and never follow redirects. The hub refuses destinations on loopback, link-local, private and metadata addresses unless the operator allows them, and checks the address again at every delivery. The signing secret is encrypted at rest and shown once. Only a workspace's owner or the admin token manages its event webhooks, never a workspace-bound token.

### 15.4 Performance targets

Targets are for one hub on a host with 8 cores, 32 GB of memory and local SSD storage, with repositories up to 5 GB of history. They are the initial bar and are to be revisited with measurements.

| Operation | Target |
|---|---|
| Task fork creation, any repository size | p95 under 1 s; no history is copied |
| Read of one file or tree through the API | p95 under 100 ms |
| Commit creation through the API (up to 100 files) | p95 under 500 ms |
| Land of a change of up to 20 commits, no contention | p95 under 2 s |
| Lands per minute on one target branch, non-conflicting changes | at least 60 sustained |
| Concurrent land attempts in flight | 8 computing, the rest waiting without failing |
| Mergeability known after a target move, 100 open changes | within 60 s |
| Event visible to a live subscriber after its state change | within 1 s |
| Active task forks per hub | 5,000 |
| Open changes per workspace | 2,000 |
| Concurrent event-stream connections | 500 (configurable) |
| Running sandboxes per hub (`provisioning` or `ready`) | 200, the default of `[sandbox].max_sandboxes` |
| Sandbox `ready` after create, image already cached by the runtime | within 60 s || Overhead the hub adds to a sandbox exec | under 200 ms |

### 15.5 Limits

Each limit is defined in the section that owns it. The main ones:

| Limit | Value | Section |
|---|---|---|
| Request body under `/api/v1` | 1 MiB (commit creation: 32 MiB) | §3, §6 |
| List page size | default 50, max 200 | §3 |
| Idempotency-key window | 24 h | §3 |
| Request rate per credential | 600 per minute (configurable, 0 = off) | §3 |
| Fork TTL | default 7 d, max 30 d; early removal 24 h after all its changes landed or were abandoned | §4 |
| Active forks per workspace | 200 | §4 |
| Operations per API commit | 1,000 | §6 |
| File read through the API | 32 MiB | §6 |
| Stack depth | 20 | §7 |
| Compare-and-swap tries per land | 10 | §8 |
| Land attempt duration | 5 min | §8 |
| Policy document | 200 rules, 64 KiB | §8 |
| Claim TTL | default 1 h, max 24 h | §9 |
| Durable job retries | 20, backoff 4 s doubling to 2 h | §10 |
| Sandbox TTL | set per profile, max 24 h | §13 |
| Sandbox idle timeout | default 30 min | §13 |
| Running sandboxes (`provisioning` or `ready`) | 200 per hub, 100 per organization, 50 per workspace and its forks, 20 per user | §13 |
| Workspace sandbox rules | 50 per workspace | §13 |
| Sandbox exec timeout | default 60 s, max 30 min | §13 |
| Sandbox file transfer | 1 GiB per upload or download | §13 |
| Event log retention | 30 d | §14 |
| Event webhooks per workspace | 20 | §14 |
| Event webhook delivery | timeout 10 s per attempt; retries from 10 s doubling to 1 h, for 24 h per event; subscription disabled after 72 h of continuous failure; delivery log kept 7 d | §14 |
| `parley` request timeout | 30 s (3 min for sync, push to origin, archive and reclone) | §3, §4 |
| `--wait` defaults | `--timeout 5m`, `--poll-interval 5s` | §3 |

### 15.6 Observability

- **NFR-040** `GET /metrics` exposes the Prometheus metrics catalogued in §14. Every documented metric is exported.
- **NFR-041** `/healthz` reports liveness, `/readyz` readiness, `/version` build information (§3).
- **NFR-042** Logs are structured (one JSON object per line), carry the request id, and never contain credentials or secret values.
- **NFR-043** The hourly maintenance loop (§3) runs retention and cleanup tasks. One failing task never stops the others.
